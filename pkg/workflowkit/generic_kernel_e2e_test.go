package workflowkit

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestWorkflowkitNonHarborKernelE2E exercises the public durable-kernel
// boundary with a deliberately non-Harbor workflow.  This file imports only
// the standard library: its opaque binding, plugin IDs, resource keys, and
// recovery subjects are generic test values rather than domain vocabulary.
func TestWorkflowkitNonHarborKernelE2E(t *testing.T) {
	assertWorkflowkitProductionHasNoHarborImports(t)

	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	workflow := WorkflowDescriptor{
		ID:      "generic-kernel-e2e",
		Version: "1",
		Stages: []StageDescriptor{
			genericKernelE2EStage("collect", nil, nil, []ArtifactSpec{{Name: "collected", SchemaVersion: "generic/v1", Required: true}}),
			genericKernelE2EStage("transform", []StageKey{"collect"}, []ArtifactSpec{{Name: "collected", SchemaVersion: "generic/v1", Required: true}}, []ArtifactSpec{{Name: "transformed", SchemaVersion: "generic/v1", Required: true}}),
		},
	}
	if err := workflow.Validate(); err != nil {
		t.Fatalf("validate generic workflow: %v", err)
	}
	binding, err := NewOpaqueExecutionBinding("example.generic-execution", "1", []byte(`{"input":"immutable"}`))
	if err != nil {
		t.Fatalf("freeze generic opaque binding: %v", err)
	}
	backend := newGenericKernelE2EBackend(now)
	registry, err := NewControlledPluginRegistry([]PluginRegistration[StageExecutor]{
		{
			Binding: PluginBinding{ID: "example.generic-stage", Version: "1"},
			Implementation: StageExecutorFunc(func(ctx context.Context, request StageExecutionRequest) (StageExecutionResult, error) {
				if _, err := request.Checkpoint(ctx, StageCheckpoint{
					CheckpointID:   "checkpoint-" + string(request.Stage.Key),
					IdempotencyKey: "checkpoint-key-" + string(request.Stage.Key),
					TurnOrdinal:    1,
					Substep:        "complete",
					Resumable:      true,
					OccurredAt:     now,
				}); err != nil {
					return StageExecutionResult{}, fmt.Errorf("checkpoint generic stage: %w", err)
				}
				if err := request.Charge(ctx, StageUsage{
					OperationKey: "usage-" + string(request.Stage.Key),
					Dimension:    "generic_work",
					Units:        1,
					OccurredAt:   now,
				}); err != nil {
					return StageExecutionResult{}, fmt.Errorf("charge generic stage: %w", err)
				}

				switch request.Stage.Key {
				case "collect":
					return StageExecutionResult{
						Outcome:   Outcome{Status: StatusCompleted, Verdict: VerdictPass},
						Artifacts: []StageArtifact{{Name: "collected", SchemaVersion: "generic/v1", Content: []byte("immutable collected fact")}},
					}, nil
				case "transform":
					if len(request.Inputs) != 1 || request.Inputs[0].Name != "collected" {
						return StageExecutionResult{}, fmt.Errorf("transform did not receive its frozen collected input")
					}
					input, err := request.ReadInput(ctx, request.Inputs[0])
					if err != nil {
						return StageExecutionResult{}, fmt.Errorf("read frozen collected input: %w", err)
					}
					if string(input) != "immutable collected fact" {
						return StageExecutionResult{}, fmt.Errorf("transform input drifted: %q", input)
					}
					return StageExecutionResult{
						Outcome:   Outcome{Status: StatusCompleted, Verdict: VerdictPass},
						Artifacts: []StageArtifact{{Name: "transformed", SchemaVersion: "generic/v1", Content: []byte("derived generic fact")}},
					}, nil
				default:
					return StageExecutionResult{}, fmt.Errorf("unexpected generic stage %q", request.Stage.Key)
				}
			}),
		},
	})
	if err != nil {
		t.Fatalf("install generic plugin: %v", err)
	}
	engine, err := NewEngine(EngineConfig{
		Backend: backend, Executors: registry, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("construct generic engine: %v", err)
	}

	prepared, err := engine.Prepare(context.Background(), PrepareRequest{
		ExecutionID:    "generic-execution-1",
		IdempotencyKey: "generic-execution-key-1",
		Subject: SubjectBinding{
			SubjectID:  "generic-subject-1",
			RevisionID: "generic-revision-1",
			Digest:     SubjectDigest(SHA256Fingerprint([]byte("generic subject"))),
		},
		Workflow:           workflow,
		ProfileFingerprint: SHA256Fingerprint([]byte("generic-profile")),
		Binding:            binding,
		Actor:              "generic-operator",
		Reason:             "exercise public workflowkit kernel",
	})
	if err != nil {
		t.Fatalf("freeze generic DAG: %v", err)
	}
	if err := prepared.Execution.Validate(); err != nil {
		t.Fatalf("prepared execution is not frozen: %v", err)
	}
	if got := prepared.Execution.Plan.Batches; len(got) != 2 || !reflect.DeepEqual(got[0].NodeIDs, []NodeID{"collect"}) || !reflect.DeepEqual(got[1].NodeIDs, []NodeID{"transform"}) {
		t.Fatalf("frozen generic dependency plan = %#v", got)
	}

	coordinator, err := genericKernelE2ECoordinatorClaim(prepared.Execution, now)
	if err != nil {
		t.Fatalf("freeze generic coordinator claim: %v", err)
	}
	assertGenericKernelE2EHandle(t, engine, coordinator)
	if backend.coordinatorLoads != 1 {
		t.Fatalf("coordinator did not load one frozen generic input: loads=%d", backend.coordinatorLoads)
	}
	backend.requireScheduled(t, "dependency-level-001", "collect")
	backend.requireScheduleCount(t, "collect", 1)
	backend.requireScheduleCount(t, "transform", 0)

	// Replaying the coordinator after its first atomic admission observes a
	// queued node and waits.  It must not manufacture a second stage job.
	assertGenericKernelE2EHandle(t, engine, coordinator)
	backend.requireScheduleCount(t, "collect", 1)
	backend.requireScheduleCount(t, "transform", 0)

	collectClaim := backend.claimFor(t, "collect")
	assertGenericKernelE2EHandle(t, engine, collectClaim)
	backend.requireCheckpointAndUsageCount(t, "collect", 1, 1)

	assertGenericKernelE2EHandle(t, engine, coordinator)
	backend.requireScheduled(t, "dependency-level-002", "transform")
	backend.requireScheduleCount(t, "transform", 1)

	// The same duplicate-delivery invariant also holds for later batches.
	assertGenericKernelE2EHandle(t, engine, coordinator)
	backend.requireScheduleCount(t, "transform", 1)

	transformClaim := backend.claimFor(t, "transform")
	assertGenericKernelE2EHandle(t, engine, transformClaim)
	backend.requireCheckpointAndUsageCount(t, "transform", 1, 1)

	assertGenericKernelE2EHandle(t, engine, coordinator)
	if backend.lastCoordinatorDecision.Kind != CoordinatorComplete {
		t.Fatalf("generic coordinator terminal decision = %#v, want complete", backend.lastCoordinatorDecision)
	}
	if backend.totalScheduleCount() != 2 {
		t.Fatalf("generic coordinator scheduled %d stages, want exactly collect and transform once", backend.totalScheduleCount())
	}

	backend.recoverySubjects = []RecoverySubject{
		{
			SubjectID: "recoverable-generic-stage", Status: StatusPaused,
			CheckpointRecoverable: true, InputsUnchanged: true, DefinitionUnchanged: true,
		},
		{
			SubjectID: "unknown-generic-effect", Status: StatusInDoubt,
			UnknownExternalOutcome: true,
		},
	}
	decisions, err := engine.Reconcile(context.Background(), RecoveryScope{
		ExecutionID: prepared.Execution.ID, ObservedAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("apply generic recovery API: %v", err)
	}
	if !reflect.DeepEqual(decisions, []RecoveryDecision{
		{SubjectID: "recoverable-generic-stage", Action: RecoveryResumeCheckpoint, Reasons: []RecoveryReason{RecoveryReasonCheckpointReusable}},
		{SubjectID: "unknown-generic-effect", Action: RecoveryReconcile, Reasons: []RecoveryReason{RecoveryReasonUnknownSideEffect}},
	}) {
		t.Fatalf("generic recovery decisions = %#v", decisions)
	}
	if !reflect.DeepEqual(backend.appliedRecovery, decisions) {
		t.Fatalf("backend did not receive generic recovery decisions: %#v", backend.appliedRecovery)
	}
	if backend.totalScheduleCount() != 2 {
		t.Fatalf("generic recovery duplicated durable scheduling: %d schedules", backend.totalScheduleCount())
	}
}

// assertWorkflowkitProductionHasNoHarborImports makes the boundary exercised
// above explicit: the reusable production package may not depend directly on
// a Harbor-domain implementation.  The E2E fake remains wholly local to this
// test, so it cannot accidentally satisfy that property through a product
// adapter.
func assertWorkflowkitProductionHasNoHarborImports(t *testing.T) {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate workflowkit E2E test source")
	}
	entries, err := os.ReadDir(filepath.Dir(source))
	if err != nil {
		t.Fatalf("read workflowkit source directory: %v", err)
	}
	fileSet := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(filepath.Dir(source), name)
		parsed, parseErr := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			t.Fatalf("parse workflowkit source %s: %v", name, parseErr)
		}
		for _, imported := range parsed.Imports {
			value, unquoteErr := strconv.Unquote(imported.Path.Value)
			if unquoteErr != nil {
				t.Fatalf("unquote import in %s: %v", name, unquoteErr)
			}
			if strings.Contains(value, "/internal/harbor") {
				t.Fatalf("workflowkit production source %s imports Harbor-domain package %q", name, value)
			}
		}
	}
}

func genericKernelE2EStage(key StageKey, dependencies []StageKey, inputs, outputs []ArtifactSpec) StageDescriptor {
	return StageDescriptor{
		Key: key, Version: "1", Plugin: PluginBinding{ID: "example.generic-stage", Version: "1"},
		Group: "generic", Dependencies: append([]StageKey(nil), dependencies...),
		Inputs: append([]ArtifactSpec(nil), inputs...), Outputs: append([]ArtifactSpec(nil), outputs...),
		ReadSet: []ResourceKey{"generic/input/" + ResourceKey(key)}, WriteSet: []ResourceKey{"generic/output/" + ResourceKey(key)},
		Effect: EffectEvidenceOnly, Dispatch: StageDispatchAutomatic,
		Budget: ExecutionBudget{
			TurnTimeout: time.Second, MaxTurns: 1, AttemptTimeout: time.Second,
			MaxAttempts: 1, MaxElapsed: time.Second, Backoff: BackoffPolicy{RetryDelays: []time.Duration{}},
		},
		Retry: RetryPolicy{}, Verdicts: VerdictPolicy{Allowed: []Verdict{VerdictPass}}, Reuse: ReuseWhenInputsMatch,
	}
}

func assertGenericKernelE2EHandle(t *testing.T, engine *Engine, claim JobClaim) {
	t.Helper()
	state, err := engine.HandleClaim(context.Background(), claim)
	if err != nil {
		t.Fatalf("handle %s claim %q: %v", claim.Kind, claim.JobID, err)
	}
	if state != JobCompleted {
		t.Fatalf("handle %s claim %q state = %q, want completed", claim.Kind, claim.JobID, state)
	}
}

func genericKernelE2ECoordinatorClaim(execution FrozenExecution, now time.Time) (JobClaim, error) {
	schedule, err := FreezeCoordinatorSchedule(
		execution.Workflow, execution.ID, CoordinatorScheduleExecutionPlan, execution.Plan, nil, nil,
	)
	if err != nil {
		return JobClaim{}, err
	}
	return JobClaim{
		JobID: "generic-coordinator-job", ClaimID: "generic-coordinator-claim", Kind: JobCoordinator,
		Owner: "generic-worker", FencingToken: 1, LeaseExpiresAt: now.Add(time.Hour), Execution: execution,
		Coordinator: &CoordinatorClaim{Schedule: schedule},
	}, nil
}

type genericKernelE2EBackend struct {
	now                     time.Time
	execution               FrozenExecution
	states                  map[NodeID]CoordinatorNodeStatus
	claims                  map[NodeID]JobClaim
	scheduled               map[NodeID]int
	scheduledBatches        map[string][]NodeID
	artifacts               map[ArtifactID][]byte
	outputBindings          map[NodeID]map[string]ArtifactBinding
	checkpoints             map[string]int
	usages                  map[string]int
	coordinatorLoads        int
	lastCoordinatorDecision CoordinatorDecision
	recoverySubjects        []RecoverySubject
	appliedRecovery         []RecoveryDecision
}

func newGenericKernelE2EBackend(now time.Time) *genericKernelE2EBackend {
	return &genericKernelE2EBackend{
		now: now, states: make(map[NodeID]CoordinatorNodeStatus), claims: make(map[NodeID]JobClaim),
		scheduled: make(map[NodeID]int), scheduledBatches: make(map[string][]NodeID), artifacts: make(map[ArtifactID][]byte),
		outputBindings: make(map[NodeID]map[string]ArtifactBinding), checkpoints: make(map[string]int), usages: make(map[string]int),
	}
}

func (backend *genericKernelE2EBackend) PrepareExecution(_ context.Context, _ PrepareRequest, execution FrozenExecution) (PreparedExecution, error) {
	if backend.execution.ID != "" {
		if backend.execution.ID != execution.ID || backend.execution.DefinitionFingerprint != execution.DefinitionFingerprint {
			return PreparedExecution{}, fmt.Errorf("different execution replay")
		}
		return PreparedExecution{Execution: backend.execution.Clone(), CoordinatorJobID: "generic-coordinator-job"}, nil
	}
	backend.execution = execution.Clone()
	for _, stage := range execution.Workflow.Stages {
		backend.states[stage.Key] = CoordinatorNodePending
	}
	return PreparedExecution{Execution: backend.execution.Clone(), CoordinatorJobID: "generic-coordinator-job"}, nil
}

func (backend *genericKernelE2EBackend) LoadCoordinatorInput(_ context.Context, claim JobClaim) (CoordinatorInput, error) {
	if claim.Execution.ID != backend.execution.ID {
		return CoordinatorInput{}, fmt.Errorf("coordinator execution drift")
	}
	if claim.Coordinator == nil || claim.Coordinator.Schedule.ExecutionKey != backend.execution.ID {
		return CoordinatorInput{}, fmt.Errorf("coordinator claim omits its frozen schedule binding")
	}
	backend.coordinatorLoads++
	nodes := make([]CoordinatorNodeState, 0, len(backend.execution.Workflow.Stages))
	for _, stage := range backend.execution.Workflow.Stages {
		nodes = append(nodes, CoordinatorNodeState{NodeID: stage.Key, Generation: 0, Status: backend.states[stage.Key]})
	}
	input := CoordinatorInput{Workflow: backend.execution.Workflow.Clone(), ScheduleMode: CoordinatorScheduleExecutionPlan, Plan: backend.execution.Plan.Clone(), Nodes: nodes}
	return input, nil
}

func (backend *genericKernelE2EBackend) CommitCoordinatorDecision(_ context.Context, claim JobClaim, decision CoordinatorDecision) (JobTerminalState, error) {
	if claim.Execution.ID != backend.execution.ID {
		return "", fmt.Errorf("coordinator commit execution drift")
	}
	backend.lastCoordinatorDecision = decision.Clone()
	if decision.Kind != CoordinatorScheduleNextBatch {
		return JobCompleted, nil
	}
	for _, nodeID := range decision.NextBatch.NodeIDs {
		if backend.states[nodeID] != CoordinatorNodePending {
			return "", fmt.Errorf("coordinator attempted duplicate admission for %q in state %q", nodeID, backend.states[nodeID])
		}
		stage, found := backend.execution.Workflow.Stage(StageKey(nodeID))
		if !found {
			return "", fmt.Errorf("coordinator selected unknown stage %q", nodeID)
		}
		inputs, err := backend.stageInputs(stage)
		if err != nil {
			return "", err
		}
		stageClaim := JobClaim{
			JobID: "generic-stage-job-" + string(nodeID), ClaimID: "generic-stage-claim-" + string(nodeID), Kind: JobStage,
			Owner: "generic-worker", FencingToken: 1, LeaseExpiresAt: backend.now.Add(time.Hour), Execution: backend.execution.Clone(),
			Stage: &StageClaim{
				StageAttempt: AttemptIdentity{ID: AttemptID("generic-stage-attempt-" + string(nodeID)), Kind: AttemptStage, ScopeID: backend.execution.ID + ":" + string(nodeID), Ordinal: 1},
				Stage:        stage, Generation: 0, Inputs: inputs,
			},
		}
		backend.states[nodeID] = CoordinatorNodeQueued
		backend.claims[nodeID] = stageClaim.Clone()
		backend.scheduled[nodeID]++
		backend.scheduledBatches[decision.NextBatch.ID] = append(backend.scheduledBatches[decision.NextBatch.ID], nodeID)
	}
	return JobCompleted, nil
}

func (backend *genericKernelE2EBackend) ReadStageInput(_ context.Context, _ JobClaim, binding ArtifactBinding) ([]byte, error) {
	content, found := backend.artifacts[binding.ArtifactID]
	if !found || SHA256Fingerprint(content) != binding.ContentDigest {
		return nil, fmt.Errorf("immutable input %q is unavailable or drifted", binding.Name)
	}
	return append([]byte(nil), content...), nil
}

func (backend *genericKernelE2EBackend) RecordStageCheckpoint(_ context.Context, _ JobClaim, checkpoint StageCheckpoint) (CheckpointReceipt, error) {
	backend.checkpoints[checkpoint.IdempotencyKey]++
	return CheckpointReceipt{CheckpointID: checkpoint.CheckpointID}, nil
}

func (backend *genericKernelE2EBackend) RecordStageUsage(_ context.Context, _ JobClaim, usage StageUsage) error {
	backend.usages[usage.OperationKey]++
	return nil
}

func (backend *genericKernelE2EBackend) CommitStage(_ context.Context, completion StageCompletion) (JobTerminalState, error) {
	if completion.Claim.Stage == nil {
		return "", fmt.Errorf("stage completion has no stage")
	}
	key := completion.Claim.Stage.Stage.Key
	if backend.states[key] != CoordinatorNodeQueued {
		return "", fmt.Errorf("stage %q completion is not for an admitted queued claim", key)
	}
	if completion.Result.Outcome.Status != StatusCompleted || completion.Result.Outcome.Verdict != VerdictPass {
		return "", fmt.Errorf("unexpected generic stage outcome %#v", completion.Result.Outcome)
	}
	bindings := make(map[string]ArtifactBinding, len(completion.Result.Artifacts))
	for _, artifact := range completion.Result.Artifacts {
		id := ArtifactID("generic-artifact-" + string(key) + "-" + artifact.Name)
		content := append([]byte(nil), artifact.Content...)
		backend.artifacts[id] = content
		bindings[artifact.Name] = ArtifactBinding{Name: artifact.Name, ArtifactID: id, ContentDigest: SHA256Fingerprint(content), SchemaVersion: artifact.SchemaVersion}
	}
	backend.outputBindings[key] = bindings
	backend.states[key] = CoordinatorNodeSucceeded
	return JobCompleted, nil
}

func (backend *genericKernelE2EBackend) CommitStageWait(context.Context, StageWaitCommit) (JobTerminalState, error) {
	return "", fmt.Errorf("generic kernel E2E stages never enter a wait")
}

func (backend *genericKernelE2EBackend) RejectStageClaim(_ context.Context, _ JobClaim, cause error) (JobTerminalState, error) {
	return "", fmt.Errorf("generic kernel E2E rejected a valid stage claim: %w", cause)
}

func (backend *genericKernelE2EBackend) ListRecoverySubjects(_ context.Context, _ RecoveryScope) ([]RecoverySubject, error) {
	return append([]RecoverySubject(nil), backend.recoverySubjects...), nil
}

func (backend *genericKernelE2EBackend) ApplyRecovery(_ context.Context, _ RecoveryScope, decisions []RecoveryDecision) error {
	backend.appliedRecovery = append([]RecoveryDecision(nil), decisions...)
	return nil
}

func (backend *genericKernelE2EBackend) stageInputs(stage StageDescriptor) ([]ArtifactBinding, error) {
	if len(stage.Inputs) == 0 {
		return nil, nil
	}
	if stage.Key != "transform" || len(stage.Inputs) != 1 || stage.Inputs[0].Name != "collected" {
		return nil, fmt.Errorf("generic backend has no declared input binding for %q", stage.Key)
	}
	binding, found := backend.outputBindings["collect"]["collected"]
	if !found {
		return nil, fmt.Errorf("transform scheduled before collected output was durable")
	}
	return []ArtifactBinding{binding}, nil
}

func (backend *genericKernelE2EBackend) claimFor(t *testing.T, nodeID NodeID) JobClaim {
	t.Helper()
	claim, found := backend.claims[nodeID]
	if !found {
		t.Fatalf("no durable generic stage claim for %q", nodeID)
	}
	return claim.Clone()
}

func (backend *genericKernelE2EBackend) requireScheduled(t *testing.T, batchID string, want ...NodeID) {
	t.Helper()
	if got := backend.scheduledBatches[batchID]; !reflect.DeepEqual(got, want) {
		t.Fatalf("scheduled batch %q = %#v, want %#v", batchID, got, want)
	}
}

func (backend *genericKernelE2EBackend) requireScheduleCount(t *testing.T, nodeID NodeID, want int) {
	t.Helper()
	if got := backend.scheduled[nodeID]; got != want {
		t.Fatalf("stage %q durable schedules = %d, want %d", nodeID, got, want)
	}
}

func (backend *genericKernelE2EBackend) requireCheckpointAndUsageCount(t *testing.T, nodeID NodeID, checkpointWant, usageWant int) {
	t.Helper()
	if got := backend.checkpoints["checkpoint-key-"+string(nodeID)]; got != checkpointWant {
		t.Fatalf("stage %q checkpoint writes = %d, want %d", nodeID, got, checkpointWant)
	}
	if got := backend.usages["usage-"+string(nodeID)]; got != usageWant {
		t.Fatalf("stage %q usage writes = %d, want %d", nodeID, got, usageWant)
	}
}

func (backend *genericKernelE2EBackend) totalScheduleCount() int {
	total := 0
	for _, count := range backend.scheduled {
		total += count
	}
	return total
}
