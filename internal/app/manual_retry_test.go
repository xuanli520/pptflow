package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/runlock"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

func TestPlanNodeRetryAndUnifiedRunnerReuseSameRun(t *testing.T) {
	workspace := t.TempDir()
	opts := RunnerOptions{TaskDir: writeRunnerTask(t), Workspace: workspace, AutoApprove: true}
	first := NewRunner(opts)
	firstSummary, err := first.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := readAppWorkflowResult(t, workspace)
	for index := range result.Nodes {
		if result.Nodes[index].NodeID == nodes.FinalReview {
			result.Nodes[index].Status = workflow.NodeFailed
			result.Nodes[index].Error = "manual retry fixture"
		}
	}
	result.Status = workflow.RunFailed
	writeAppWorkflowResult(t, workspace, result)

	plan, err := PlanNodeRetry(opts, nodes.FinalReview)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RunID != firstSummary.RunID || plan.RequestedNodeID != nodes.FinalReview || plan.NextRevision != result.Revision+1 {
		t.Fatalf("unexpected app retry plan: %+v", plan)
	}
	if !containsString(plan.AffectedNodes, nodes.FinalReview) || containsString(plan.AffectedNodes, nodes.CodeEdgeLint) {
		t.Fatalf("retry closure does not preserve upstream: %+v", plan)
	}

	retry, err := NewRetryRunner(opts, nodes.FinalReview)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := retry.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.RunID != firstSummary.RunID || !summary.Recovered || summary.Status != "succeeded" {
		t.Fatalf("manual retry did not continue the same durable run: %+v", summary)
	}
	retried := readAppWorkflowResult(t, workspace)
	if retried.Revision != plan.NextRevision || retried.ManualRetry == nil || retried.ManualRetry.RequestedNodeID != nodes.FinalReview {
		t.Fatalf("manual retry revision/audit metadata missing: %+v", retried)
	}
	if countEventType(summary.Events, nodes.FinalReview, "node_started") == 0 || countEventType(summary.Events, nodes.CodeEdgeLint, "node_reused") == 0 {
		t.Fatalf("retry did not rerun selected node and reuse upstream: %+v", summary.Events)
	}
}

func TestPlanNodeRetryRejectsSucceededRunningUnknownAndActiveWorkspace(t *testing.T) {
	workspace := t.TempDir()
	opts := RunnerOptions{TaskDir: writeRunnerTask(t), Workspace: workspace, AutoApprove: true}
	if _, err := NewRunner(opts).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanNodeRetry(opts, nodes.CodeEdgeLint); err == nil || !strings.Contains(err.Error(), "requires failed, canceled, or skipped") {
		t.Fatalf("succeeded node retry was not rejected: %v", err)
	}
	if _, err := PlanNodeRetry(opts, "unknown-node"); err == nil || !strings.Contains(err.Error(), "unknown node") {
		t.Fatalf("unknown node retry was not rejected: %v", err)
	}

	result := readAppWorkflowResult(t, workspace)
	result.Status = workflow.RunRunning
	result.ActiveNodeID = nodes.CodeEdgeLint
	writeAppWorkflowResult(t, workspace, result)
	if _, err := PlanNodeRetry(opts, nodes.CodeEdgeLint); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("running workflow retry was not rejected: %v", err)
	}
	result.Status, result.ActiveNodeID = workflow.RunFailed, ""
	for index := range result.Nodes {
		if result.Nodes[index].NodeID == nodes.CodeEdgeLint {
			result.Nodes[index].Status = workflow.NodeFailed
		}
	}
	writeAppWorkflowResult(t, workspace, result)
	lock, err := runlock.Acquire(workspace, runlock.Metadata{RunID: result.RunID, StartedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := PlanNodeRetry(opts, nodes.CodeEdgeLint); err == nil || !strings.Contains(err.Error(), "workspace run is active") {
		t.Fatalf("active workspace retry was not rejected: %v", err)
	}
}

func readAppWorkflowResult(t *testing.T, workspace string) workflow.RunResult {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(workspace, "run_result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result workflow.RunResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func writeAppWorkflowResult(t *testing.T, workspace string, result workflow.RunResult) {
	t.Helper()
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "run_result.json"), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
