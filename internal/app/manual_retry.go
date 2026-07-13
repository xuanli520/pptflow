package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/runlock"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

// PlanNodeRetry is read-only and suitable for confirmation UIs. Run performs
// the same planning again while holding the workspace lock.
func PlanNodeRetry(opts RunnerOptions, nodeID string) (ManualRetryPlan, error) {
	return planNodeRetry(opts, nodeID, true)
}

func (r *Runner) PlanNodeRetry(nodeID string) (ManualRetryPlan, error) {
	if r == nil {
		return ManualRetryPlan{}, fmt.Errorf("runner is nil")
	}
	return PlanNodeRetry(r.opts, nodeID)
}

func planNodeRetry(opts RunnerOptions, nodeID string, checkActive bool) (ManualRetryPlan, error) {
	opts = HydrateRuntimeOptions(opts)
	workspace := nodes.DefaultWorkspace(opts.Workspace)
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return ManualRetryPlan{}, fmt.Errorf("manual retry node ID is required")
	}
	if checkActive {
		active, err := runlock.IsActive(workspace)
		if err != nil {
			return ManualRetryPlan{}, fmt.Errorf("check workspace run state: %w", err)
		}
		if active {
			return ManualRetryPlan{}, fmt.Errorf("manual retry rejected: workspace run is active")
		}
	}
	definition, err := buildWorkflowDefinition(opts)
	if err != nil {
		return ManualRetryPlan{}, err
	}
	raw, err := os.ReadFile(filepath.Join(workspace, "run_result.json"))
	if err != nil {
		return ManualRetryPlan{}, fmt.Errorf("read workflow result for manual retry: %w", err)
	}
	var result workflow.RunResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return ManualRetryPlan{}, fmt.Errorf("parse workflow result for manual retry: %w", err)
	}
	if result.Status == workflow.RunRunning || result.ActiveNodeID != "" {
		return ManualRetryPlan{}, fmt.Errorf("manual retry rejected: workflow is still running")
	}
	if result.WorkflowID != "" && result.WorkflowID != definition.ID {
		return ManualRetryPlan{}, fmt.Errorf("manual retry workflow mismatch: result=%s definition=%s", result.WorkflowID, definition.ID)
	}
	prior := latestNodeRuns(result.Nodes)
	plan, err := workflow.PlanManualRetry(definition, prior, result.Revision, nodeID)
	if err != nil {
		return ManualRetryPlan{}, err
	}
	return ManualRetryPlan{Workspace: workspace, RunID: result.RunID, ManualRetryPlan: plan}, nil
}

func latestNodeRuns(runs []workflow.NodeRun) map[string]workflow.NodeRun {
	prior := make(map[string]workflow.NodeRun, len(runs))
	for _, run := range runs {
		existing, ok := prior[run.NodeID]
		if !ok || run.Revision >= existing.Revision {
			prior[run.NodeID] = run
		}
	}
	return prior
}
