package workflowkit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCompileDependencyExecutionPlanGroupsIndependentStages(t *testing.T) {
	workflow := WorkflowDescriptor{
		ID: "parallel-workflow", Version: "1",
		Stages: []StageDescriptor{
			testStage("left", nil, EffectEvidenceOnly, nil, []ResourceKey{"evidence/left"}),
			testStage("right", nil, EffectEvidenceOnly, nil, []ResourceKey{"evidence/right"}),
			testStage("join", []StageKey{"left", "right"}, EffectEvidenceOnly, []ResourceKey{"evidence/left", "evidence/right"}, []ResourceKey{"evidence/join"}),
			testStage("finish", []StageKey{"join"}, EffectEvidenceOnly, []ResourceKey{"evidence/join"}, []ResourceKey{"evidence/finish"}),
		},
	}
	plan, err := CompileDependencyExecutionPlan(workflow)
	if err != nil {
		t.Fatalf("compile dependency plan: %v", err)
	}
	if err := plan.Validate(workflow); err != nil {
		t.Fatalf("validate dependency plan: %v", err)
	}
	if len(plan.Batches) != 3 {
		t.Fatalf("batches = %#v, want 3 dependency levels", plan.Batches)
	}
	if got := plan.Batches[0].NodeIDs; len(got) != 2 || got[0] != "left" || got[1] != "right" {
		t.Fatalf("first batch = %#v, want independent left/right stages", got)
	}
	if got := plan.Batches[1].NodeIDs; len(got) != 1 || got[0] != "join" {
		t.Fatalf("second batch = %#v, want join", got)
	}
	if got := plan.Batches[2].NodeIDs; len(got) != 1 || got[0] != "finish" {
		t.Fatalf("third batch = %#v, want finish", got)
	}
}

func TestCompileDependencyExecutionPlanExcludesOperatorOnlyStages(t *testing.T) {
	operatorOnly := testStage("deliver", []StageKey{"verify"}, EffectExternalSideEffect, []ResourceKey{"evidence/verify"}, []ResourceKey{"delivery/receipt"})
	operatorOnly.Dispatch = StageDispatchOperatorOnly
	operatorOnly.Retry = RetryPolicy{}
	operatorOnly.Reuse = ReuseNever
	workflow := WorkflowDescriptor{
		ID: "operator-only-workflow", Version: "1",
		Stages: []StageDescriptor{
			testStage("source", nil, EffectEvidenceOnly, nil, []ResourceKey{"evidence/source"}),
			testStage("verify", []StageKey{"source"}, EffectEvidenceOnly, []ResourceKey{"evidence/source"}, []ResourceKey{"evidence/verify"}),
			operatorOnly,
		},
	}

	plan, err := CompileDependencyExecutionPlan(workflow)
	if err != nil {
		t.Fatalf("compile operator-only execution plan: %v", err)
	}
	if err := plan.Validate(workflow); err != nil {
		t.Fatalf("validate operator-only execution plan: %v", err)
	}
	if len(plan.Batches) != 2 || plan.Batches[0].NodeIDs[0] != "source" || plan.Batches[1].NodeIDs[0] != "verify" {
		t.Fatalf("automatic batches = %#v, want only source then verify", plan.Batches)
	}
	for _, batch := range plan.Batches {
		for _, nodeID := range batch.NodeIDs {
			if nodeID == "deliver" {
				t.Fatalf("operator-only stage appeared in initial execution plan: %#v", plan.Batches)
			}
		}
	}

	invalid, err := FreezeExecutionPlan(workflow, []ScheduleBatch{
		{ID: "source", NodeIDs: []NodeID{"source"}},
		{ID: "verify", NodeIDs: []NodeID{"verify"}},
		{ID: "deliver", NodeIDs: []NodeID{"deliver"}},
	})
	if err == nil || invalid.Fingerprint != "" {
		t.Fatalf("operator-only stage was accepted in a worker plan: %+v, %v", invalid, err)
	}
}

func TestEngineFreezesExecutionAndCommitsFailureEvidence(t *testing.T) {
	now := time.Date(2026, time.July, 14, 9, 0, 0, 0, time.UTC)
	workflow := singleStageWorkflow(t)
	binding, err := NewOpaqueExecutionBinding("example.execution", "1", []byte(`{"selection":"immutable"}`))
	if err != nil {
		t.Fatalf("freeze opaque binding: %v", err)
	}
	backend := &engineTestBackend{}
	registry, err := NewControlledPluginRegistry([]PluginRegistration[StageExecutor]{
		{Binding: workflow.Stages[0].Plugin, Implementation: StageExecutorFunc(func(ctx context.Context, request StageExecutionRequest) (StageExecutionResult, error) {
			if request.Execution.ID != "execution-1" || request.Stage.Key != "source" {
				t.Fatalf("executor received unexpected frozen request: %#v", request)
			}
			if _, err := request.Checkpoint(ctx, StageCheckpoint{CheckpointID: "checkpoint-1", IdempotencyKey: "checkpoint-key-1", TurnOrdinal: 1, Substep: "draft", Resumable: true, OccurredAt: now}); err != nil {
				t.Fatalf("persist checkpoint: %v", err)
			}
			if err := request.Charge(ctx, StageUsage{OperationKey: "turn-1", Dimension: "agent_turn", Units: 1, OccurredAt: now}); err != nil {
				t.Fatalf("charge usage: %v", err)
			}
			return StageExecutionResult{Artifacts: []StageArtifact{{Name: "report", SchemaVersion: "v1", Content: []byte("partial report")}}}, errors.New("network lost after report")
		})},
	})
	if err != nil {
		t.Fatalf("create executor registry: %v", err)
	}
	engine, err := NewEngine(EngineConfig{
		Backend: backend, Executors: registry, Now: func() time.Time { return now },
		Classifier: FailureClassifierFunc(func(error) FailureClass { return FailureNetwork }),
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	prepared, err := engine.Prepare(context.Background(), PrepareRequest{
		ExecutionID: "execution-1", IdempotencyKey: "execution-key-1",
		Subject:  SubjectBinding{SubjectID: "subject-1", RevisionID: "revision-1", Digest: SubjectDigest(SHA256Fingerprint([]byte("subject")))},
		Workflow: workflow, ProfileFingerprint: SHA256Fingerprint([]byte("profile")), Binding: binding,
		Actor: "operator", Reason: "test frozen execution",
	})
	if err != nil {
		t.Fatalf("prepare execution: %v", err)
	}
	if prepared.Execution.Plan.Fingerprint == "" || len(prepared.Execution.Plan.Batches) != 1 {
		t.Fatalf("prepared execution did not freeze plan: %#v", prepared.Execution.Plan)
	}
	claim := JobClaim{
		JobID: "job-1", ClaimID: "claim-1", Kind: JobStage, Owner: "worker-1", FencingToken: 1,
		LeaseExpiresAt: now.Add(time.Minute), Execution: prepared.Execution,
		Stage: &StageClaim{StageAttempt: AttemptIdentity{ID: "stage-attempt-1", Kind: AttemptStage, ScopeID: "source", Ordinal: 1}, Stage: workflow.Stages[0], Generation: 0},
	}
	state, err := engine.HandleClaim(context.Background(), claim)
	if err != nil {
		t.Fatalf("handle stage claim: %v", err)
	}
	if state != JobCompleted {
		t.Fatalf("claim terminal state = %q, want completed", state)
	}
	if backend.checkpoints != 1 || backend.usages != 1 {
		t.Fatalf("backend callbacks checkpoints=%d usages=%d, want 1/1", backend.checkpoints, backend.usages)
	}
	if backend.completion == nil {
		t.Fatal("stage completion was not committed")
	}
	if got := backend.completion.Result.Outcome; got.Status != StatusInfraFailed || got.Failure != FailureNetwork {
		t.Fatalf("failure outcome = %#v, want infrastructure network failure", got)
	}
	if got := backend.completion.Result.Artifacts; len(got) != 1 || string(got[0].Content) != "partial report" {
		t.Fatalf("failed evidence was lost: %#v", got)
	}
	if backend.prepared == nil || backend.prepared.Execution.Binding.Fingerprint != binding.Fingerprint {
		t.Fatalf("backend did not receive frozen binding: %#v", backend.prepared)
	}
}

func TestEngineCoordinatorClaimUsesMandatoryGenericDecisionPort(t *testing.T) {
	now := time.Date(2026, time.July, 15, 9, 0, 0, 0, time.UTC)
	workflow := singleStageWorkflow(t)
	binding, err := NewOpaqueExecutionBinding("example.execution", "1", []byte(`{"selection":"immutable"}`))
	if err != nil {
		t.Fatalf("freeze opaque binding: %v", err)
	}
	plan, err := CompileDependencyExecutionPlan(workflow)
	if err != nil {
		t.Fatalf("compile coordinator plan: %v", err)
	}
	backend := &coordinatorDecisionTestBackend{input: CoordinatorInput{
		Workflow: workflow.Clone(), ScheduleMode: CoordinatorScheduleExecutionPlan, Plan: plan.Clone(),
		Nodes: []CoordinatorNodeState{{NodeID: "source", Generation: 0, Status: CoordinatorNodePending}},
	}}
	registry, err := NewControlledPluginRegistry([]PluginRegistration[StageExecutor]{{
		Binding: workflow.Stages[0].Plugin,
		Implementation: StageExecutorFunc(func(context.Context, StageExecutionRequest) (StageExecutionResult, error) {
			t.Fatal("coordinator claim must not invoke a stage executor")
			return StageExecutionResult{}, nil
		}),
	}})
	if err != nil {
		t.Fatalf("create executor registry: %v", err)
	}
	engine, err := NewEngine(EngineConfig{Backend: backend, Executors: registry, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	prepared := prepareEngineTestExecution(t, engine, workflow, binding)
	schedule, err := FreezeCoordinatorSchedule(prepared.Execution.Workflow, prepared.Execution.ID, CoordinatorScheduleExecutionPlan, plan, nil, nil)
	if err != nil {
		t.Fatalf("freeze coordinator schedule: %v", err)
	}
	state, err := engine.HandleClaim(context.Background(), JobClaim{
		JobID: "coordinator-job", ClaimID: "coordinator-claim", Kind: JobCoordinator,
		Owner: "worker-1", FencingToken: 1, LeaseExpiresAt: now.Add(time.Minute), Execution: prepared.Execution,
		Coordinator: &CoordinatorClaim{Schedule: schedule},
	})
	if err != nil {
		t.Fatalf("handle coordinator claim: %v", err)
	}
	if state != JobCompleted {
		t.Fatalf("coordinator terminal state = %q, want %q", state, JobCompleted)
	}
	if backend.committed == nil || backend.committed.Kind != CoordinatorScheduleNextBatch || len(backend.committed.NextBatch.NodeIDs) != 1 || backend.committed.NextBatch.NodeIDs[0] != "source" {
		t.Fatalf("committed coordinator decision = %#v, want first frozen batch", backend.committed)
	}
}

func TestCoordinatorClaimRequiresFrozenSchedule(t *testing.T) {
	now := time.Date(2026, time.July, 15, 9, 0, 0, 0, time.UTC)
	workflow := singleStageWorkflow(t)
	binding, err := NewOpaqueExecutionBinding("example.execution", "1", []byte(`{"selection":"immutable"}`))
	if err != nil {
		t.Fatalf("freeze opaque binding: %v", err)
	}
	execution, err := freezeExecution(PrepareRequest{
		ExecutionID: "missing-coordinator-claim", IdempotencyKey: "missing-coordinator-claim-key",
		Subject:  SubjectBinding{SubjectID: "subject", RevisionID: "revision", Digest: SubjectDigest(SHA256Fingerprint([]byte("subject")))},
		Workflow: workflow, ProfileFingerprint: SHA256Fingerprint([]byte("profile")), Binding: binding, Actor: "operator", Reason: "test missing coordinator claim",
	}, now)
	if err != nil {
		t.Fatalf("freeze execution: %v", err)
	}
	claim := JobClaim{
		JobID: "coordinator-job", ClaimID: "coordinator-claim", Kind: JobCoordinator,
		Owner: "worker", FencingToken: 1, LeaseExpiresAt: now.Add(time.Minute), Execution: execution,
	}
	if err := claim.Validate(); !errors.Is(err, ErrInvalidJobClaim) {
		t.Fatalf("coordinator claim without frozen schedule error = %v, want invalid job claim", err)
	}
}

func TestEngineRejectsCoordinatorInputOutsideFrozenClaim(t *testing.T) {
	now := time.Date(2026, time.July, 15, 9, 0, 0, 0, time.UTC)
	workflow := singleStageWorkflow(t)
	binding, err := NewOpaqueExecutionBinding("example.execution", "1", []byte(`{"selection":"immutable"}`))
	if err != nil {
		t.Fatalf("freeze opaque binding: %v", err)
	}
	plan, err := CompileDependencyExecutionPlan(workflow)
	if err != nil {
		t.Fatalf("compile coordinator plan: %v", err)
	}
	backend := &coordinatorDecisionTestBackend{input: CoordinatorInput{
		Workflow: workflow.Clone(), ScheduleMode: CoordinatorScheduleTransitionSubset,
		Schedule:    []ScheduleBatch{{ID: "drifted-schedule", NodeIDs: []NodeID{"source"}}},
		Transitions: []NodeTransition{coordinatorTransition(t, "source", DispositionSchedule, 0, 0)},
		Nodes:       []CoordinatorNodeState{{NodeID: "source", Generation: 0, Status: CoordinatorNodePending}},
	}}
	registry, err := NewControlledPluginRegistry([]PluginRegistration[StageExecutor]{{
		Binding: workflow.Stages[0].Plugin,
		Implementation: StageExecutorFunc(func(context.Context, StageExecutionRequest) (StageExecutionResult, error) {
			t.Fatal("coordinator claim must not invoke a stage executor")
			return StageExecutionResult{}, nil
		}),
	}})
	if err != nil {
		t.Fatalf("create executor registry: %v", err)
	}
	engine, err := NewEngine(EngineConfig{Backend: backend, Executors: registry, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	prepared := prepareEngineTestExecution(t, engine, workflow, binding)
	schedule, err := FreezeCoordinatorSchedule(prepared.Execution.Workflow, prepared.Execution.ID, CoordinatorScheduleExecutionPlan, plan, nil, nil)
	if err != nil {
		t.Fatalf("freeze coordinator schedule: %v", err)
	}
	_, err = engine.HandleClaim(context.Background(), JobClaim{
		JobID: "coordinator-job", ClaimID: "coordinator-claim", Kind: JobCoordinator,
		Owner: "worker", FencingToken: 1, LeaseExpiresAt: now.Add(time.Minute), Execution: prepared.Execution,
		Coordinator: &CoordinatorClaim{Schedule: schedule},
	})
	if !errors.Is(err, ErrInvalidJobClaim) {
		t.Fatalf("schedule-drifted coordinator input error = %v, want invalid job claim", err)
	}
	if backend.committed != nil {
		t.Fatalf("schedule-drifted coordinator input reached commit: %#v", backend.committed)
	}
}

func TestStageClaimOptionalNodeAttemptMustBindItsParentStageAttempt(t *testing.T) {
	workflow := singleStageWorkflow(t)
	binding, err := NewOpaqueExecutionBinding("example.execution", "1", []byte(`{"selection":"immutable"}`))
	if err != nil {
		t.Fatalf("freeze opaque binding: %v", err)
	}
	execution, err := freezeExecution(PrepareRequest{
		ExecutionID: "node-claim-execution", IdempotencyKey: "node-claim-key",
		Subject:  SubjectBinding{SubjectID: "subject", RevisionID: "revision", Digest: SubjectDigest(SHA256Fingerprint([]byte("subject")))},
		Workflow: workflow, ProfileFingerprint: SHA256Fingerprint([]byte("profile")), Binding: binding, Actor: "operator", Reason: "test node attempt binding",
	}, time.Date(2026, time.July, 15, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("freeze execution: %v", err)
	}
	stageAttempt := AttemptIdentity{ID: "stage-attempt", Kind: AttemptStage, ScopeID: "scope", Ordinal: 1}
	nodeAttempt := AttemptIdentity{ID: "node-attempt", Kind: AttemptNode, ScopeID: "scope", ParentAttemptID: stageAttempt.ID, Ordinal: 1}
	claim := JobClaim{
		JobID: "node-job", ClaimID: "node-claim", Kind: JobStage, Owner: "worker", FencingToken: 1,
		LeaseExpiresAt: time.Date(2026, time.July, 15, 10, 0, 0, 0, time.UTC), Execution: execution,
		Stage: &StageClaim{StageAttempt: stageAttempt, NodeAttempt: &nodeAttempt, Stage: workflow.Stages[0]},
	}
	if err := claim.Validate(); err != nil {
		t.Fatalf("valid child node attempt was rejected: %v", err)
	}
	withoutNode := claim.Clone()
	withoutNode.Stage.NodeAttempt = nil
	if err := withoutNode.Validate(); err != nil {
		t.Fatalf("stage claim without backend-owned nested node attempt was rejected: %v", err)
	}
	forged := claim.Clone()
	forged.Stage.NodeAttempt.ParentAttemptID = "another-stage-attempt"
	if err := forged.Validate(); !errors.Is(err, ErrInvalidJobClaim) {
		t.Fatalf("forged node parent error = %v, want invalid job claim", err)
	}
}

func TestEngineRejectsPluginVersionDriftWithoutExecutingStage(t *testing.T) {
	now := time.Date(2026, time.July, 14, 9, 0, 0, 0, time.UTC)
	workflow := singleStageWorkflow(t)
	binding, err := NewOpaqueExecutionBinding("example.execution", "1", []byte(`{"selection":"immutable"}`))
	if err != nil {
		t.Fatalf("freeze opaque binding: %v", err)
	}
	backend := &engineTestBackend{}
	registry, err := NewControlledPluginRegistry([]PluginRegistration[StageExecutor]{
		{Binding: PluginBinding{ID: workflow.Stages[0].Plugin.ID, Version: "2.0.0"}, Implementation: StageExecutorFunc(func(context.Context, StageExecutionRequest) (StageExecutionResult, error) {
			t.Fatal("drifted plugin must not execute")
			return StageExecutionResult{}, nil
		})},
	})
	if err != nil {
		t.Fatalf("create drifted registry: %v", err)
	}
	engine, err := NewEngine(EngineConfig{Backend: backend, Executors: registry, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	prepared := prepareEngineTestExecution(t, engine, workflow, binding)
	_, err = engine.HandleClaim(context.Background(), JobClaim{
		JobID: "job-drift", ClaimID: "claim-drift", Kind: JobStage, Owner: "worker-1", FencingToken: 1,
		LeaseExpiresAt: now.Add(time.Minute), Execution: prepared.Execution,
		Stage: &StageClaim{StageAttempt: AttemptIdentity{ID: "stage-drift", Kind: AttemptStage, ScopeID: "source", Ordinal: 1}, Stage: workflow.Stages[0]},
	})
	if err != nil {
		t.Fatalf("plugin drift should be projected by backend, got %v", err)
	}
	if backend.rejected == nil || !errors.Is(backend.rejected, ErrPluginVersionMismatch) {
		t.Fatalf("plugin drift cause = %v, want ErrPluginVersionMismatch", backend.rejected)
	}
}

func TestEngineRejectsOperatorOnlyStageClaimWithoutExecutingPlugin(t *testing.T) {
	now := time.Date(2026, time.July, 14, 9, 0, 0, 0, time.UTC)
	workflow := singleStageWorkflow(t)
	workflow.Stages[0].Dispatch = StageDispatchOperatorOnly
	workflow.Stages[0].Retry = RetryPolicy{}
	workflow.Stages[0].Reuse = ReuseNever
	if err := workflow.Validate(); err != nil {
		t.Fatalf("validate operator-only workflow: %v", err)
	}
	binding, err := NewOpaqueExecutionBinding("example.execution", "1", []byte(`{"selection":"immutable"}`))
	if err != nil {
		t.Fatalf("freeze opaque binding: %v", err)
	}
	backend := &engineTestBackend{}
	registry, err := NewControlledPluginRegistry([]PluginRegistration[StageExecutor]{
		{Binding: workflow.Stages[0].Plugin, Implementation: StageExecutorFunc(func(context.Context, StageExecutionRequest) (StageExecutionResult, error) {
			t.Fatal("operator-only stage plugin must never execute")
			return StageExecutionResult{}, nil
		})},
	})
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	engine, err := NewEngine(EngineConfig{Backend: backend, Executors: registry, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	prepared := prepareEngineTestExecution(t, engine, workflow, binding)
	state, err := engine.HandleClaim(context.Background(), JobClaim{
		JobID: "job-operator-only", ClaimID: "claim-operator-only", Kind: JobStage, Owner: "worker-1", FencingToken: 1,
		LeaseExpiresAt: now.Add(time.Minute), Execution: prepared.Execution,
		Stage: &StageClaim{StageAttempt: AttemptIdentity{ID: "stage-operator-only", Kind: AttemptStage, ScopeID: "source", Ordinal: 1}, Stage: workflow.Stages[0]},
	})
	if err != nil || state != JobReconcileRequired {
		t.Fatalf("operator-only claim result = %q, %v", state, err)
	}
	if !errors.Is(backend.rejected, ErrInvalidJobClaim) || backend.completion != nil {
		t.Fatalf("operator-only claim rejection = %v completion=%+v", backend.rejected, backend.completion)
	}
}

func TestEngineProjectsNonterminalExternalDecisionWait(t *testing.T) {
	now := time.Date(2026, time.July, 14, 9, 0, 0, 0, time.UTC)
	workflow := singleStageWorkflow(t)
	workflow.Stages[0].Capabilities = append(workflow.Stages[0].Capabilities, CapabilityApprove)
	binding, err := NewOpaqueExecutionBinding("example.execution", "1", []byte(`{"selection":"immutable"}`))
	if err != nil {
		t.Fatalf("freeze opaque binding: %v", err)
	}
	waitBinding, err := NewOpaqueExecutionBinding("example.decision", "1", []byte(`{"request":"operator"}`))
	if err != nil {
		t.Fatalf("freeze wait binding: %v", err)
	}
	backend := &engineTestBackend{}
	registry, err := NewControlledPluginRegistry([]PluginRegistration[StageExecutor]{
		{Binding: workflow.Stages[0].Plugin, Implementation: StageExecutorFunc(func(context.Context, StageExecutionRequest) (StageExecutionResult, error) {
			return StageExecutionResult{Wait: &StageWait{Kind: StageWaitExternalDecision, OperationKey: "decision-1", DecisionBinding: waitBinding}}, nil
		})},
	})
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	engine, err := NewEngine(EngineConfig{Backend: backend, Executors: registry, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	prepared := prepareEngineTestExecution(t, engine, workflow, binding)
	state, err := engine.HandleClaim(context.Background(), JobClaim{
		JobID: "job-wait", ClaimID: "claim-wait", Kind: JobStage, Owner: "worker-1", FencingToken: 1,
		LeaseExpiresAt: now.Add(time.Minute), Execution: prepared.Execution,
		Stage: &StageClaim{StageAttempt: AttemptIdentity{ID: "stage-wait", Kind: AttemptStage, ScopeID: "source", Ordinal: 1}, Stage: workflow.Stages[0]},
	})
	if err != nil || state != JobCompleted {
		t.Fatalf("handle wait claim = %q, %v", state, err)
	}
	if backend.waitCommit == nil || backend.waitCommit.Wait.DecisionBinding.Fingerprint != waitBinding.Fingerprint || backend.completion != nil {
		t.Fatalf("wait projection = %+v, terminal completion = %+v", backend.waitCommit, backend.completion)
	}
}

func TestEngineRejectsExternalDecisionWaitFromStageWithoutApprovalCapability(t *testing.T) {
	now := time.Date(2026, time.July, 14, 9, 0, 0, 0, time.UTC)
	workflow := singleStageWorkflow(t)
	binding, err := NewOpaqueExecutionBinding("example.execution", "1", []byte(`{"selection":"immutable"}`))
	if err != nil {
		t.Fatalf("freeze opaque binding: %v", err)
	}
	waitBinding, err := NewOpaqueExecutionBinding("example.decision", "1", []byte(`{"request":"operator"}`))
	if err != nil {
		t.Fatalf("freeze wait binding: %v", err)
	}
	backend := &engineTestBackend{}
	registry, err := NewControlledPluginRegistry([]PluginRegistration[StageExecutor]{
		{Binding: workflow.Stages[0].Plugin, Implementation: StageExecutorFunc(func(context.Context, StageExecutionRequest) (StageExecutionResult, error) {
			return StageExecutionResult{Wait: &StageWait{Kind: StageWaitExternalDecision, OperationKey: "decision-1", DecisionBinding: waitBinding}}, nil
		})},
	})
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	engine, err := NewEngine(EngineConfig{Backend: backend, Executors: registry, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	prepared := prepareEngineTestExecution(t, engine, workflow, binding)
	state, err := engine.HandleClaim(context.Background(), JobClaim{
		JobID: "job-wait-without-approval", ClaimID: "claim-wait-without-approval", Kind: JobStage, Owner: "worker-1", FencingToken: 1,
		LeaseExpiresAt: now.Add(time.Minute), Execution: prepared.Execution,
		Stage: &StageClaim{StageAttempt: AttemptIdentity{ID: "stage-wait-without-approval", Kind: AttemptStage, ScopeID: "source", Ordinal: 1}, Stage: workflow.Stages[0]},
	})
	if err != nil || state != JobReconcileRequired {
		t.Fatalf("handle invalid wait claim = %q, %v", state, err)
	}
	if !errors.Is(backend.rejected, ErrInvalidStageResult) || backend.waitCommit != nil || backend.completion != nil {
		t.Fatalf("invalid wait projection rejected=%v wait=%+v completion=%+v", backend.rejected, backend.waitCommit, backend.completion)
	}
}

func TestEngineReconcileAppliesOnlyDerivedDecisions(t *testing.T) {
	now := time.Date(2026, time.July, 14, 9, 0, 0, 0, time.UTC)
	backend := &engineTestBackend{recoverySubjects: []RecoverySubject{
		{SubjectID: "paused", Status: StatusPaused, CheckpointRecoverable: true, InputsUnchanged: true, DefinitionUnchanged: true},
		{SubjectID: "unknown-effect", Status: StatusRunning, LeaseExpiresAt: now.Add(-time.Second), UnknownExternalOutcome: true},
	}}
	registry, err := NewControlledPluginRegistry([]PluginRegistration[StageExecutor]{{Binding: PluginBinding{ID: "example.source", Version: "1.0.0"}, Implementation: StageExecutorFunc(func(context.Context, StageExecutionRequest) (StageExecutionResult, error) {
		return StageExecutionResult{}, nil
	})}})
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	engine, err := NewEngine(EngineConfig{Backend: backend, Executors: registry, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	decisions, err := engine.Reconcile(context.Background(), RecoveryScope{ExecutionID: "execution-1", ObservedAt: now})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(decisions) != 2 || decisions[0].Action != RecoveryResumeCheckpoint || decisions[1].Action != RecoveryReconcile {
		t.Fatalf("recovery decisions = %#v", decisions)
	}
	if len(backend.appliedRecovery) != 2 || backend.appliedRecovery[1].Action != RecoveryReconcile {
		t.Fatalf("backend recovery apply = %#v", backend.appliedRecovery)
	}
}

func singleStageWorkflow(t *testing.T) WorkflowDescriptor {
	t.Helper()
	stage := testStage("source", nil, EffectEvidenceOnly, nil, []ResourceKey{"evidence/report"})
	stage.Outputs = []ArtifactSpec{{Name: "report", SchemaVersion: "v1", Required: true}}
	workflow := WorkflowDescriptor{ID: "engine-test", Version: "1", Stages: []StageDescriptor{stage}}
	if err := workflow.Validate(); err != nil {
		t.Fatalf("single-stage workflow invalid: %v", err)
	}
	return workflow
}

func prepareEngineTestExecution(t *testing.T, engine *Engine, workflow WorkflowDescriptor, binding OpaqueExecutionBinding) PreparedExecution {
	t.Helper()
	prepared, err := engine.Prepare(context.Background(), PrepareRequest{
		ExecutionID: "execution-1", IdempotencyKey: "execution-key-1",
		Subject:  SubjectBinding{SubjectID: "subject-1", RevisionID: "revision-1", Digest: SubjectDigest(SHA256Fingerprint([]byte("subject")))},
		Workflow: workflow, ProfileFingerprint: SHA256Fingerprint([]byte("profile")), Binding: binding,
		Actor: "operator", Reason: "test frozen execution",
	})
	if err != nil {
		t.Fatalf("prepare test execution: %v", err)
	}
	return prepared
}

type engineTestBackend struct {
	prepared         *PreparedExecution
	completion       *StageCompletion
	waitCommit       *StageWaitCommit
	rejected         error
	checkpoints      int
	usages           int
	recoverySubjects []RecoverySubject
	appliedRecovery  []RecoveryDecision
}

type coordinatorDecisionTestBackend struct {
	engineTestBackend
	input     CoordinatorInput
	committed *CoordinatorDecision
}

func (backend *coordinatorDecisionTestBackend) LoadCoordinatorInput(_ context.Context, _ JobClaim) (CoordinatorInput, error) {
	return backend.input.Clone(), nil
}

func (backend *coordinatorDecisionTestBackend) CommitCoordinatorDecision(_ context.Context, _ JobClaim, decision CoordinatorDecision) (JobTerminalState, error) {
	copyDecision := decision.Clone()
	backend.committed = &copyDecision
	return JobCompleted, nil
}

func (backend *engineTestBackend) PrepareExecution(_ context.Context, _ PrepareRequest, execution FrozenExecution) (PreparedExecution, error) {
	prepared := PreparedExecution{Execution: execution.Clone(), CoordinatorJobID: "coordinator-" + execution.ID}
	backend.prepared = &prepared
	return prepared, nil
}

func (backend *engineTestBackend) LoadCoordinatorInput(context.Context, JobClaim) (CoordinatorInput, error) {
	return CoordinatorInput{}, errors.New("stage-only test backend cannot load coordinator input")
}

func (backend *engineTestBackend) CommitCoordinatorDecision(context.Context, JobClaim, CoordinatorDecision) (JobTerminalState, error) {
	return "", errors.New("stage-only test backend cannot commit coordinator decision")
}

func (backend *engineTestBackend) ReadStageInput(context.Context, JobClaim, ArtifactBinding) ([]byte, error) {
	return []byte("input"), nil
}

func (backend *engineTestBackend) RecordStageCheckpoint(_ context.Context, _ JobClaim, checkpoint StageCheckpoint) (CheckpointReceipt, error) {
	backend.checkpoints++
	return CheckpointReceipt{CheckpointID: checkpoint.CheckpointID}, nil
}

func (backend *engineTestBackend) RecordStageUsage(context.Context, JobClaim, StageUsage) error {
	backend.usages++
	return nil
}

func (backend *engineTestBackend) CommitStage(_ context.Context, completion StageCompletion) (JobTerminalState, error) {
	copyCompletion := completion.Clone()
	backend.completion = &copyCompletion
	return JobCompleted, nil
}

func (backend *engineTestBackend) CommitStageWait(_ context.Context, commit StageWaitCommit) (JobTerminalState, error) {
	copyCommit := commit.Clone()
	backend.waitCommit = &copyCommit
	return JobCompleted, nil
}

func (backend *engineTestBackend) RejectStageClaim(_ context.Context, _ JobClaim, cause error) (JobTerminalState, error) {
	backend.rejected = cause
	return JobReconcileRequired, nil
}

func (backend *engineTestBackend) ListRecoverySubjects(context.Context, RecoveryScope) ([]RecoverySubject, error) {
	return append([]RecoverySubject(nil), backend.recoverySubjects...), nil
}

func (backend *engineTestBackend) ApplyRecovery(_ context.Context, _ RecoveryScope, decisions []RecoveryDecision) error {
	backend.appliedRecovery = append([]RecoveryDecision(nil), decisions...)
	return nil
}
