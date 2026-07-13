package workflow

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func manualRetryDefinition() WorkflowDefinition {
	return WorkflowDefinition{ID: "manual-retry", Policy: Policy{MaxRevisions: 2}, Nodes: []NodeSpec{
		{ID: "a", Kind: "production"},
		{ID: "b", Kind: "production", DependsOn: []string{"a"}},
		{ID: "d", Kind: "production", DependsOn: []string{"b"}},
		{ID: "c", Kind: "production"},
	}}
}

func TestPlanManualRetryResolvesSkippedNodeToBlockingAncestor(t *testing.T) {
	prior := map[string]NodeRun{
		"a": {NodeID: "a", Status: NodeSucceeded},
		"b": {NodeID: "b", Status: NodeFailed},
		"d": {NodeID: "d", Status: NodeSkipped},
		"c": {NodeID: "c", Status: NodeSucceeded},
	}
	plan, err := PlanManualRetry(manualRetryDefinition(), prior, 7, "d")
	if err != nil {
		t.Fatal(err)
	}
	if plan.RestartNodeID != "b" || strings.Join(plan.RetryRoots, ",") != "b" || strings.Join(plan.AffectedNodes, ",") != "b,d" {
		t.Fatalf("unexpected skipped retry plan: %+v", plan)
	}
	if plan.CurrentRevision != 7 || plan.NextRevision != 8 || strings.Join(plan.ReusedUpstream, ",") != "a,c" {
		t.Fatalf("unexpected revision/reuse plan: %+v", plan)
	}
}

func TestPlanManualRetryRejectsIneligibleStates(t *testing.T) {
	definition := manualRetryDefinition()
	for _, status := range []NodeStatus{NodeSucceeded, NodeRunning, NodePending, NodeRequeued} {
		_, err := PlanManualRetry(definition, map[string]NodeRun{"a": {NodeID: "a", Status: status}}, 0, "a")
		if err == nil || !strings.Contains(err.Error(), "requires failed, canceled, or skipped") {
			t.Fatalf("status %s accepted: %v", status, err)
		}
	}
	if _, err := PlanManualRetry(definition, nil, 0, "missing"); err == nil || !strings.Contains(err.Error(), "unknown node") {
		t.Fatalf("unknown node accepted: %v", err)
	}
}

func TestEngineManualRetryInvalidatesDownstreamAndPreservesOtherBranches(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileArtifactStore(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	put := func(nodeID string) ArtifactRef {
		ref, putErr := store.PutText(context.Background(), nodeID+"-old.txt", "test", nodeID, "old")
		if putErr != nil {
			t.Fatal(putErr)
		}
		return ref
	}
	aRef, bRef, dRef, cRef := put("a"), put("b"), put("d"), put("c")
	prior := map[string]NodeRun{
		"a": {NodeID: "a", Kind: "production", Status: NodeSucceeded, Revision: 6, Artifacts: []ArtifactRef{aRef}},
		"b": {NodeID: "b", Kind: "production", Status: NodeFailed, Revision: 6, Artifacts: []ArtifactRef{bRef}},
		"d": {NodeID: "d", Kind: "production", Status: NodeSkipped, Revision: 6, Artifacts: []ArtifactRef{dRef}},
		"c": {NodeID: "c", Kind: "production", Status: NodeFailed, Revision: 6, Artifacts: []ArtifactRef{cRef}},
	}
	plugin := &productionPlugin{}
	registry := NewRegistry()
	if err := registry.Register(plugin); err != nil {
		t.Fatal(err)
	}
	result, runErr := NewEngine(registry, Runtimes{}).Run(context.Background(), RunRequest{
		RunID: "same-run", Revision: 6, Workflow: manualRetryDefinition(), WorkspaceRoot: filepath.Join(root, "workspace"),
		Store: store, Prior: prior, Retry: &ManualRetryRequest{NodeID: "b"},
	})
	if runErr == nil || result.Status != RunFailed {
		t.Fatalf("independent failed branch should remain failed: result=%+v err=%v", result, runErr)
	}
	if result.RunID != "same-run" || result.Revision != 7 || plugin.calls["a"] != 0 || plugin.calls["b"] != 1 || plugin.calls["d"] != 1 || plugin.calls["c"] != 0 {
		t.Fatalf("manual retry scope/revision mismatch: calls=%+v result=%+v", plugin.calls, result)
	}
	if result.ManualRetry == nil || strings.Join(result.ManualRetry.AffectedNodes, ",") != "b,d" {
		t.Fatalf("manual retry audit metadata missing: %+v", result.ManualRetry)
	}
	refs, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if (ref.Producer == "b" || ref.Producer == "d") && strings.Contains(ref.Name, "-old") {
			t.Fatalf("stale affected artifact survived: %+v", ref)
		}
	}
	if reader, _, err := store.Get(context.Background(), aRef); err != nil {
		t.Fatalf("successful upstream artifact was not preserved: %v", err)
	} else {
		_ = reader.Close()
	}
	if reader, _, err := store.Get(context.Background(), cRef); err != nil {
		t.Fatalf("out-of-scope branch artifact was not preserved: %v", err)
	} else {
		_ = reader.Close()
	}
}

func TestManualRetryCheckpointRetainsPriorWhenFirstNodeStartCheckpointFails(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileArtifactStore(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	prior := map[string]NodeRun{
		"a": {NodeID: "a", Kind: "production", Status: NodeSucceeded, Revision: 2},
		"b": {NodeID: "b", Kind: "production", Status: NodeFailed, Revision: 2},
		"d": {NodeID: "d", Kind: "production", Status: NodeSkipped, Revision: 2},
		"c": {NodeID: "c", Kind: "production", Status: NodeSucceeded, Revision: 2},
	}
	plugin := &productionPlugin{}
	registry := NewRegistry()
	if err := registry.Register(plugin); err != nil {
		t.Fatal(err)
	}
	_, runErr := NewEngine(registry, Runtimes{}).Run(context.Background(), RunRequest{
		RunID: "crash-safe", Revision: 2, Workflow: manualRetryDefinition(), WorkspaceRoot: filepath.Join(root, "workspace"),
		Store: store, Prior: prior, Retry: &ManualRetryRequest{NodeID: "b"},
		Checkpoint: func(_ context.Context, checkpoint RunResult) error {
			if checkpoint.ActiveNodeID == "b" && lastEventType(checkpoint.Events) == "node_started" {
				return errors.New("simulated checkpoint interruption")
			}
			return nil
		},
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "simulated checkpoint interruption") {
		t.Fatalf("expected simulated interruption, got %v", runErr)
	}
	var checkpoint RunResult
	if _, err := store.ReadJSON(context.Background(), "run_result.json", &checkpoint); err != nil {
		t.Fatal(err)
	}
	latest := make(map[string]NodeRun, len(checkpoint.Nodes))
	for _, run := range checkpoint.Nodes {
		latest[run.NodeID] = run
	}
	if latest["a"].Status != NodeSucceeded || latest["c"].Status != NodeSucceeded || latest["b"].Status != NodeFailed || latest["d"].Status != NodeSkipped {
		t.Fatalf("checkpoint lost retryable prior snapshot: %+v", checkpoint.Nodes)
	}
	if _, err := PlanManualRetry(manualRetryDefinition(), latest, checkpoint.Revision, "b"); err != nil {
		t.Fatalf("interrupted manual retry cannot be planned again: %v", err)
	}
}

func lastEventType(events []Event) string {
	if len(events) == 0 {
		return ""
	}
	return events[len(events)-1].Type
}
