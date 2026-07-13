package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type testPlugin struct {
	order *[]string
	fail  map[string]bool
}

func (p testPlugin) Manifest() PluginManifest {
	return PluginManifest{ID: "test.plugin", Version: "0", Kinds: []string{"test"}}
}

func (p testPlugin) Validate(NodeSpec) error {
	return nil
}

func (p testPlugin) Execute(ctx context.Context, req NodeRequest) (NodeResult, error) {
	*p.order = append(*p.order, req.Spec.ID)
	if p.fail[req.Spec.ID] {
		return NodeResult{}, context.Canceled
	}
	return NodeResult{}, nil
}

func TestEngineRunsDependsOnInTopologicalOrder(t *testing.T) {
	order := []string{}
	registry := NewRegistry()
	if err := registry.Register(testPlugin{order: &order}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(registry, Runtimes{})
	result, err := engine.Run(context.Background(), RunRequest{
		Workflow: WorkflowDefinition{
			ID: "topo",
			Nodes: []NodeSpec{
				{ID: "b", Kind: "test", DependsOn: []string{"a"}},
				{ID: "a", Kind: "test"},
			},
		},
		ArtifactRoot:  filepath.Join(t.TempDir(), "artifacts"),
		WorkspaceRoot: filepath.Join(t.TempDir(), "workspace"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunSucceeded {
		t.Fatalf("status = %s", result.Status)
	}
	if got := strings.Join(order, ","); got != "a,b" {
		t.Fatalf("order = %s", got)
	}
}

func TestEngineUsesEdgesAsDependencies(t *testing.T) {
	order := []string{}
	registry := NewRegistry()
	if err := registry.Register(testPlugin{order: &order}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(registry, Runtimes{})
	_, err := engine.Run(context.Background(), RunRequest{
		Workflow: WorkflowDefinition{
			ID: "edges",
			Nodes: []NodeSpec{
				{ID: "b", Kind: "test"},
				{ID: "a", Kind: "test"},
			},
			Edges: []EdgeSpec{{From: "a", To: "b"}},
		},
		ArtifactRoot:  filepath.Join(t.TempDir(), "artifacts"),
		WorkspaceRoot: filepath.Join(t.TempDir(), "workspace"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "a,b" {
		t.Fatalf("order = %s", got)
	}
}

func TestEngineRejectsDependencyCycle(t *testing.T) {
	registry := NewRegistry()
	order := []string{}
	if err := registry.Register(testPlugin{order: &order}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(registry, Runtimes{})
	_, err := engine.Run(context.Background(), RunRequest{
		Workflow: WorkflowDefinition{
			ID:    "cycle",
			Nodes: []NodeSpec{{ID: "a", Kind: "test"}, {ID: "b", Kind: "test"}},
			Edges: []EdgeSpec{{From: "a", To: "b"}, {From: "b", To: "a"}},
		},
		ArtifactRoot:  filepath.Join(t.TempDir(), "artifacts"),
		WorkspaceRoot: filepath.Join(t.TempDir(), "workspace"),
	})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err = %v", err)
	}
}

func TestEngineSkipsDownstreamAfterDependencyFailure(t *testing.T) {
	order := []string{}
	registry := NewRegistry()
	if err := registry.Register(testPlugin{order: &order, fail: map[string]bool{"a": true}}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(registry, Runtimes{})
	result, err := engine.Run(context.Background(), RunRequest{
		Workflow: WorkflowDefinition{
			ID: "failed-dep",
			Nodes: []NodeSpec{
				{ID: "a", Kind: "test"},
				{ID: "b", Kind: "test", DependsOn: []string{"a"}},
				{ID: "c", Kind: "test"},
			},
		},
		ArtifactRoot:  filepath.Join(t.TempDir(), "artifacts"),
		WorkspaceRoot: filepath.Join(t.TempDir(), "workspace"),
	})
	if err == nil {
		t.Fatal("expected workflow error")
	}
	if got := strings.Join(order, ","); got != "a,c" {
		t.Fatalf("order = %s", got)
	}
	if len(result.Nodes) != 3 {
		t.Fatalf("nodes = %d", len(result.Nodes))
	}
	if result.Nodes[1].NodeID != "b" || result.Nodes[1].Status != NodeSkipped {
		t.Fatalf("node b = %+v", result.Nodes[1])
	}
	if result.Nodes[2].NodeID != "c" || result.Nodes[2].Status != NodeSucceeded {
		t.Fatalf("node c = %+v", result.Nodes[2])
	}
}

func TestFileArtifactStoreRegisterRecordsExistingFile(t *testing.T) {
	store, err := NewFileArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.Path("evidence/log.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("log"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Register(context.Background(), RegisterArtifactRequest{Name: "evidence/log.txt", Type: "text", Producer: "collect_evidence", Path: path}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Register(context.Background(), RegisterArtifactRequest{Name: "evidence/log.txt", Type: "text", Producer: "collect_evidence", Path: path}); err != nil {
		t.Fatal(err)
	}
	refs, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %d", len(refs))
	}
	if refs[0].Name != "evidence/log.txt" || refs[0].Type != "text" {
		t.Fatalf("ref = %+v", refs[0])
	}
}

type productionPlugin struct {
	mu                   sync.Mutex
	calls                map[string]int
	attempts             []int
	inputs               []ArtifactRef
	failures             int
	unclassifiedFailures int
}

func (p *productionPlugin) Manifest() PluginManifest {
	return PluginManifest{ID: "test.production", Version: "1", Kinds: []string{"production"}}
}
func (p *productionPlugin) Validate(NodeSpec) error { return nil }
func (p *productionPlugin) Execute(ctx context.Context, req NodeRequest) (NodeResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	p.calls[req.Spec.ID]++
	p.attempts = append(p.attempts, req.Attempt)
	p.inputs = append([]ArtifactRef(nil), req.Inputs...)
	if p.failures > 0 {
		p.failures--
		return NodeResult{}, NewNodeError(FailureTransient, true, "temporary", errors.New("unavailable"))
	}
	if p.unclassifiedFailures > 0 {
		p.unclassifiedFailures--
		return NodeResult{}, errors.New("temporary unclassified failure")
	}
	ref, err := req.Store.PutText(ctx, req.Spec.ID+".txt", "test", req.Spec.ID, req.Spec.ID)
	return NodeResult{Artifacts: []ArtifactRef{ref}}, err
}

func TestEngineExternalRunUsesExactWorkspaceAndInjectedInfrastructure(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	artifacts := filepath.Join(workspace, "artifacts")
	store, err := NewFileArtifactStore(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	events, err := NewPersistentEventRecorder(filepath.Join(workspace, "custom-events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	plugin := &productionPlugin{}
	registry := NewRegistry()
	if err := registry.Register(plugin); err != nil {
		t.Fatal(err)
	}
	result, err := NewEngine(registry, Runtimes{}).Run(context.Background(), RunRequest{
		RunID: "fixed-run", Workflow: WorkflowDefinition{ID: "production", Nodes: []NodeSpec{{ID: "a", Kind: "production"}}},
		WorkspaceRoot: workspace, Store: store, Events: events,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantWorkspace, _ := filepath.Abs(workspace)
	if result.RunID != "fixed-run" || result.WorkspaceRoot != wantWorkspace || result.ArtifactRoot != store.Root() {
		t.Fatalf("unexpected external layout: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(workspace, "fixed-run")); !os.IsNotExist(err) {
		t.Fatalf("external workspace was unexpectedly nested: %v", err)
	}
	if len(events.Events()) < 3 {
		t.Fatalf("durable events = %+v", events.Events())
	}
}

func TestEngineRetryUsesTypedFailureAndAttempt(t *testing.T) {
	plugin := &productionPlugin{failures: 2}
	registry := NewRegistry()
	if err := registry.Register(plugin); err != nil {
		t.Fatal(err)
	}
	result, err := NewEngine(registry, Runtimes{}).Run(context.Background(), RunRequest{
		Workflow:     WorkflowDefinition{ID: "retry", Nodes: []NodeSpec{{ID: "a", Kind: "production", Policy: NodePolicy{MaxAttempts: 3, RetryBackoffMS: 1, Retryable: []FailureKind{FailureTransient}}}}},
		ArtifactRoot: filepath.Join(t.TempDir(), "artifacts"), WorkspaceRoot: filepath.Join(t.TempDir(), "workspace"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(plugin.attempts); got != "[1 2 3]" {
		t.Fatalf("attempts = %s", got)
	}
	if result.Nodes[0].Metrics.RetryCount != 2 {
		t.Fatalf("metrics = %+v", result.Nodes[0].Metrics)
	}
	if countWorkflowEvent(result.Events, "a", "node_retry_scheduled") != 2 {
		t.Fatalf("retry schedule events missing: %+v", result.Events)
	}
}

func TestEngineRetryAllowsUnclassifiedOperationalFailure(t *testing.T) {
	plugin := &productionPlugin{unclassifiedFailures: 2}
	registry := NewRegistry()
	if err := registry.Register(plugin); err != nil {
		t.Fatal(err)
	}
	var checkpoints []RunResult
	result, err := NewEngine(registry, Runtimes{}).Run(context.Background(), RunRequest{
		Workflow: WorkflowDefinition{ID: "retry-unknown", Nodes: []NodeSpec{{
			ID: "a", Kind: "production", Policy: NodePolicy{
				MaxAttempts: 3, RetryBackoffMS: 1, Retryable: []FailureKind{FailureUnknown},
			},
		}}},
		ArtifactRoot: filepath.Join(t.TempDir(), "artifacts"), WorkspaceRoot: filepath.Join(t.TempDir(), "workspace"),
		Checkpoint: func(_ context.Context, checkpoint RunResult) error {
			checkpoints = append(checkpoints, checkpoint)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(plugin.attempts); got != "[1 2 3]" {
		t.Fatalf("attempts = %s", got)
	}
	if result.Nodes[0].Metrics.RetryCount != 2 {
		t.Fatalf("metrics = %+v", result.Nodes[0].Metrics)
	}
	retryCheckpoint := false
	for _, checkpoint := range checkpoints {
		if checkpoint.ActiveNodeID != "a" || checkpoint.ActiveAttempt != 2 || len(checkpoint.Events) == 0 {
			continue
		}
		if checkpoint.Events[len(checkpoint.Events)-1].Type == "node_retry_scheduled" {
			retryCheckpoint = true
			break
		}
	}
	if !retryCheckpoint {
		t.Fatalf("scheduled retry was not durably checkpointed: %+v", checkpoints)
	}
}

func countWorkflowEvent(events []Event, nodeID, eventType string) int {
	count := 0
	for _, event := range events {
		if event.NodeID == nodeID && event.Type == eventType {
			count++
		}
	}
	return count
}

func TestEngineResolvesInputsAndReusesPriorNode(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileArtifactStore(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	priorRef, err := store.PutText(context.Background(), "a.txt", "test", "a", "a")
	if err != nil {
		t.Fatal(err)
	}
	plugin := &productionPlugin{}
	registry := NewRegistry()
	if err := registry.Register(plugin); err != nil {
		t.Fatal(err)
	}
	result, err := NewEngine(registry, Runtimes{}).Run(context.Background(), RunRequest{
		RunID: "resume-run",
		Workflow: WorkflowDefinition{ID: "resume", Nodes: []NodeSpec{
			{ID: "a", Kind: "production"},
			{ID: "b", Kind: "production", DependsOn: []string{"a"}, Inputs: []ArtifactRef{{Name: "a.txt"}}},
		}},
		WorkspaceRoot: filepath.Join(root, "workspace"), Store: store,
		Prior: map[string]NodeRun{"a": {NodeID: "a", Kind: "production", Status: NodeSucceeded, Artifacts: []ArtifactRef{priorRef}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plugin.calls["a"] != 0 || plugin.calls["b"] != 1 {
		t.Fatalf("calls = %+v", plugin.calls)
	}
	if len(plugin.inputs) != 1 || plugin.inputs[0].SHA256 != priorRef.SHA256 {
		t.Fatalf("resolved inputs = %+v", plugin.inputs)
	}
	foundReuse := false
	for _, event := range result.Events {
		foundReuse = foundReuse || event.Type == "node_reused" && event.NodeID == "a"
	}
	if !foundReuse {
		t.Fatalf("reuse event missing: %+v", result.Events)
	}
}

func TestEngineEmitsNodeSkipped(t *testing.T) {
	order := []string{}
	registry := NewRegistry()
	if err := registry.Register(testPlugin{order: &order, fail: map[string]bool{"a": true}}); err != nil {
		t.Fatal(err)
	}
	result, _ := NewEngine(registry, Runtimes{}).Run(context.Background(), RunRequest{
		Workflow:     WorkflowDefinition{ID: "skip-event", Nodes: []NodeSpec{{ID: "a", Kind: "test"}, {ID: "b", Kind: "test", DependsOn: []string{"a"}}}},
		ArtifactRoot: filepath.Join(t.TempDir(), "artifacts"), WorkspaceRoot: filepath.Join(t.TempDir(), "workspace"),
	})
	for _, event := range result.Events {
		if event.Type == "node_skipped" && event.NodeID == "b" && event.Status == NodeSkipped {
			return
		}
	}
	t.Fatalf("node_skipped event missing: %+v", result.Events)
}

func TestFileArtifactStorePersistsDigestIndexAndRejectsTampering(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.PutText(context.Background(), "result.txt", "text", "node", "content")
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := NewFileArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := reopened.List(context.Background(), "")
	if err != nil || len(refs) != 1 || refs[0].SHA256 == "" || refs[0].SizeBytes != 7 {
		t.Fatalf("persistent refs=%+v err=%v", refs, err)
	}
	if err := os.WriteFile(ref.Path, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if reader, _, err := reopened.Get(context.Background(), refs[0]); err == nil {
		reader.Close()
		t.Fatal("expected digest mismatch")
	}
}

func TestFileArtifactStoreConcurrentPuts(t *testing.T) {
	store, err := NewFileArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := store.PutText(context.Background(), fmt.Sprintf("items/%02d.txt", i), "text", "race", fmt.Sprint(i)); err != nil {
				t.Errorf("put %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	refs, err := store.List(context.Background(), "items/")
	if err != nil || len(refs) != 32 {
		t.Fatalf("refs=%d err=%v", len(refs), err)
	}
	reopened, err := NewFileArtifactStore(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	refs, _ = reopened.List(context.Background(), "items/")
	if len(refs) != 32 {
		t.Fatalf("reopened refs=%d", len(refs))
	}
}

func TestPersistentEventRecorderRestoresSequencesAndPublishes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	recorder, err := NewPersistentEventRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	updates, unsubscribe := recorder.Subscribe(64)
	defer unsubscribe()
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := recorder.Emit(context.Background(), Event{RunID: "run", Type: "progress", Message: fmt.Sprint(i)}); err != nil {
				t.Errorf("emit: %v", err)
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < 24; i++ {
		select {
		case <-updates:
		case <-time.After(time.Second):
			t.Fatal("subscriber did not receive all events")
		}
	}
	reopened, err := NewPersistentEventRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	events := reopened.Events()
	if len(events) != 24 {
		t.Fatalf("events=%d", len(events))
	}
	seen := map[uint64]bool{}
	for _, event := range events {
		if event.Sequence == 0 || seen[event.Sequence] {
			t.Fatalf("invalid sequence: %+v", events)
		}
		seen[event.Sequence] = true
	}
}

type revisionPlugin struct {
	mu          sync.Mutex
	calls       map[string]int
	requeueNode string
	requeues    int
	failNode    string
	failures    int
}

func (p *revisionPlugin) Manifest() PluginManifest {
	return PluginManifest{ID: "test.revision", Version: "1", Kinds: []string{"revision"}}
}
func (p *revisionPlugin) Validate(NodeSpec) error { return nil }
func (p *revisionPlugin) Execute(ctx context.Context, req NodeRequest) (NodeResult, error) {
	p.mu.Lock()
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	p.calls[req.Spec.ID]++
	call := p.calls[req.Spec.ID]
	shouldFail := req.Spec.ID == p.failNode && p.failures > 0
	if shouldFail {
		p.failures--
	}
	shouldRequeue := req.Spec.ID == p.requeueNode && call <= p.requeues
	p.mu.Unlock()
	if shouldFail {
		return NodeResult{}, NewNodeError(FailurePermanent, false, "test failure", errors.New("failed"))
	}
	ref, err := req.Store.PutText(ctx, fmt.Sprintf("%s-r%d-call%d.txt", req.Spec.ID, req.Revision, call), "revision", req.Spec.ID, fmt.Sprintf("revision=%d", req.Revision))
	if err != nil {
		return NodeResult{}, err
	}
	if shouldRequeue {
		if req.Events != nil {
			if err := req.Events.Emit(ctx, Event{RunID: req.RunID, NodeID: req.Spec.ID, Type: "gate_requested", Status: NodeRunning, Attempt: req.Attempt}); err != nil {
				return NodeResult{}, err
			}
		}
		return NodeResult{Artifacts: []ArtifactRef{ref}, Directive: &NodeDirective{Action: DirectiveRequeue, RestartFrom: "b", Reason: "revise"}}, nil
	}
	return NodeResult{Artifacts: []ArtifactRef{ref}}, nil
}

func TestEngineRevisionRequeuesWithoutDAGCycleAndInvalidatesArtifacts(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileArtifactStore(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	plugin := &revisionPlugin{requeueNode: "gate", requeues: 2}
	registry := NewRegistry()
	if err := registry.Register(plugin); err != nil {
		t.Fatal(err)
	}
	var checkpoints []RunResult
	result, err := NewEngine(registry, Runtimes{}).Run(context.Background(), RunRequest{
		RunID: "revision-run",
		Workflow: WorkflowDefinition{ID: "revision", Policy: Policy{MaxRevisions: 5}, Nodes: []NodeSpec{
			{ID: "a", Kind: "revision"},
			{ID: "b", Kind: "revision", DependsOn: []string{"a"}},
			{ID: "gate", Kind: "revision", DependsOn: []string{"b"}},
		}},
		WorkspaceRoot: filepath.Join(root, "workspace"), Store: store,
		Checkpoint: func(_ context.Context, checkpoint RunResult) error {
			checkpoints = append(checkpoints, checkpoint)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != 2 || plugin.calls["a"] != 1 || plugin.calls["b"] != 3 || plugin.calls["gate"] != 3 {
		t.Fatalf("revision=%d calls=%+v", result.Revision, plugin.calls)
	}
	refs, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	producerCounts := map[string]int{}
	for _, ref := range refs {
		producerCounts[ref.Producer]++
		if (ref.Producer == "b" || ref.Producer == "gate") && !strings.Contains(ref.Name, "-r2-") {
			t.Fatalf("stale revision artifact survived: %+v", ref)
		}
	}
	if producerCounts["a"] != 1 || producerCounts["b"] != 1 || producerCounts["gate"] != 1 {
		t.Fatalf("producer counts = %+v refs=%+v", producerCounts, refs)
	}
	foundActiveGate := false
	for _, checkpoint := range checkpoints {
		if checkpoint.Status == RunRunning && checkpoint.ActiveNodeID == "gate" && checkpoint.ActiveAttempt == 1 {
			foundActiveGate = true
			break
		}
	}
	if !foundActiveGate {
		t.Fatalf("gate wait was not observable in checkpoints: %+v", checkpoints)
	}
	for _, run := range result.Nodes {
		if run.NodeID == "b" && run.Revision > 0 {
			return
		}
	}
	t.Fatalf("node revision metadata missing: %+v", result.Nodes)
}

func TestEngineRevisionLimitIsFive(t *testing.T) {
	root := t.TempDir()
	plugin := &revisionPlugin{requeueNode: "gate", requeues: 100}
	registry := NewRegistry()
	if err := registry.Register(plugin); err != nil {
		t.Fatal(err)
	}
	result, err := NewEngine(registry, Runtimes{}).Run(context.Background(), RunRequest{
		RunID: "bounded-revision",
		Workflow: WorkflowDefinition{ID: "bounded", Policy: Policy{MaxRevisions: 99}, Nodes: []NodeSpec{
			{ID: "b", Kind: "revision"}, {ID: "gate", Kind: "revision", DependsOn: []string{"b"}},
		}},
		ArtifactRoot: filepath.Join(root, "artifacts"), WorkspaceRoot: filepath.Join(root, "workspace"),
	})
	if err == nil || !strings.Contains(err.Error(), "maximum 5") {
		t.Fatalf("expected five revision boundary, result=%+v err=%v", result, err)
	}
	if result.Revision != 5 || plugin.calls["gate"] != 6 {
		t.Fatalf("revision=%d calls=%+v", result.Revision, plugin.calls)
	}
}

func TestEngineCheckpointSupportsIntermediateRecovery(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileArtifactStore(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	firstPlugin := &revisionPlugin{failNode: "b", failures: 1}
	registry := NewRegistry()
	if err := registry.Register(firstPlugin); err != nil {
		t.Fatal(err)
	}
	definition := WorkflowDefinition{ID: "recover", Nodes: []NodeSpec{{ID: "a", Kind: "revision"}, {ID: "b", Kind: "revision", DependsOn: []string{"a"}}}}
	_, firstErr := NewEngine(registry, Runtimes{}).Run(context.Background(), RunRequest{RunID: "recover-run", Workflow: definition, WorkspaceRoot: filepath.Join(root, "workspace"), Store: store})
	if firstErr == nil {
		t.Fatal("expected interrupted round failure")
	}
	var checkpoint RunResult
	if _, err := store.ReadJSON(context.Background(), "run_result.json", &checkpoint); err != nil {
		t.Fatal(err)
	}
	prior := map[string]NodeRun{}
	for _, run := range checkpoint.Nodes {
		prior[run.NodeID] = run
	}
	secondPlugin := &revisionPlugin{}
	secondRegistry := NewRegistry()
	if err := secondRegistry.Register(secondPlugin); err != nil {
		t.Fatal(err)
	}
	result, err := NewEngine(secondRegistry, Runtimes{}).Run(context.Background(), RunRequest{RunID: "recover-run", Revision: checkpoint.Revision, Workflow: definition, WorkspaceRoot: filepath.Join(root, "workspace"), Store: store, Prior: prior})
	if err != nil {
		t.Fatal(err)
	}
	if secondPlugin.calls["a"] != 0 || secondPlugin.calls["b"] != 1 || result.Status != RunSucceeded {
		t.Fatalf("recovery calls=%+v result=%+v", secondPlugin.calls, result)
	}
}

func TestEngineStopsWhenCheckpointCallbackFails(t *testing.T) {
	plugin := &productionPlugin{}
	registry := NewRegistry()
	if err := registry.Register(plugin); err != nil {
		t.Fatal(err)
	}
	checkpoints := 0
	result, err := NewEngine(registry, Runtimes{}).Run(context.Background(), RunRequest{
		RunID:        "checkpoint-failure",
		Workflow:     WorkflowDefinition{ID: "checkpoint", Nodes: []NodeSpec{{ID: "a", Kind: "production"}}},
		ArtifactRoot: filepath.Join(t.TempDir(), "artifacts"), WorkspaceRoot: filepath.Join(t.TempDir(), "workspace"),
		Checkpoint: func(context.Context, RunResult) error {
			checkpoints++
			if checkpoints >= 2 {
				return errors.New("checkpoint unavailable")
			}
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "checkpoint unavailable") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if plugin.calls["a"] != 0 {
		t.Fatalf("plugin executed after node-start checkpoint failed: %+v", plugin.calls)
	}
}
