package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// workflowkitStageBackend adapts one already-admitted Harbor StageAttempt to
// the public Engine's semantic durable port. It is intentionally scoped to one
// dispatch claim: SQLite fencing, quota settlement, artifacts, and review
// records remain Harbor concerns, while the Engine owns the immutable claim,
// exact plugin lookup, executor invocation, failure preservation, and generic
// wait validation.
type workflowkitStageBackend struct {
	callContext context.Context
	runtime     *FrozenExecutionRuntime
	execution   DurableJobExecution
	job         store.DurableJob
	run         store.WorkflowRun
	subject     workflowRunSubject
	frozen      frozenRunDefinition
	payload     frozenStageExecutionPayload
	stage       workflowkit.StageDescriptor
	attempt     store.StageAttempt
	node        store.NodeAttempt
	inputs      []workflowkit.ArtifactBinding
	reservation stageQuotaReservation
	monitor     *stageControlMonitor
	review      *workflowadapter.ReviewStage
	// executionReason lets a coordinator reuse the same frozen-execution
	// reconstruction proof without pretending it is a stage invocation.
	executionReason string

	result       *workflowkit.StageExecutionResult
	rejected     error
	reviewResult *store.JobState
}

func (runtime *FrozenExecutionRuntime) executeWorkflowkitStage(ctx context.Context, execution DurableJobExecution, job store.DurableJob, run store.WorkflowRun, subject workflowRunSubject, frozen frozenRunDefinition, payload frozenStageExecutionPayload, attempt store.StageAttempt, node store.NodeAttempt, stage workflowkit.StageDescriptor, inputs []workflowkit.ArtifactBinding, reservation stageQuotaReservation, monitor *stageControlMonitor) (StageExecutionResult, error) {
	backend := &workflowkitStageBackend{
		callContext: ctx,
		runtime:     runtime, execution: execution, job: job, run: run, subject: subject, frozen: frozen, payload: payload,
		stage: stage.Clone(), attempt: attempt, node: node, inputs: append([]workflowkit.ArtifactBinding(nil), inputs...), reservation: reservation, monitor: monitor,
	}
	if _, err := runtime.handleWorkflowkitStageClaim(ctx, backend); err != nil {
		return StageExecutionResult{}, err
	}
	if backend.rejected != nil {
		return StageExecutionResult{
			Outcome:      workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailurePolicy},
			ErrorText:    backend.rejected.Error(),
			FailureClass: string(workflowkit.FailurePolicy),
		}, nil
	}
	if backend.result == nil {
		return StageExecutionResult{}, fmt.Errorf("%w: public Engine did not commit a terminal stage result", ErrFrozenExecutionPayload)
	}
	return stageResultFromWorkflowkit(*backend.result), nil
}

func (runtime *FrozenExecutionRuntime) executeWorkflowkitReviewGate(ctx context.Context, execution DurableJobExecution, job store.DurableJob, run store.WorkflowRun, subject workflowRunSubject, frozen frozenRunDefinition, payload frozenStageExecutionPayload, attempt store.StageAttempt, stage workflowkit.StageDescriptor, inputs []workflowkit.ArtifactBinding, review workflowadapter.ReviewStage) (store.JobState, error) {
	backend := &workflowkitStageBackend{
		callContext: ctx,
		runtime:     runtime, execution: execution, job: job, run: run, subject: subject, frozen: frozen, payload: payload,
		stage: stage.Clone(), attempt: attempt, inputs: append([]workflowkit.ArtifactBinding(nil), inputs...), review: &review,
	}
	if _, err := runtime.handleWorkflowkitStageClaim(ctx, backend); err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if backend.rejected != nil {
		return runtime.projectStageTerminal(ctx, job, run, frozen, payload, subject, stage, attempt, inputs, stageQuotaReservation{}, StageExecutionResult{
			Outcome:      workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailurePolicy},
			ErrorText:    backend.rejected.Error(),
			FailureClass: string(workflowkit.FailurePolicy),
		}, execution.LeaseLost, nil)
	}
	if backend.reviewResult == nil {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: public Engine review gate did not commit an external-decision wait", ErrFrozenExecutionPayload))
	}
	return *backend.reviewResult, nil
}

func (runtime *FrozenExecutionRuntime) handleWorkflowkitStageClaim(ctx context.Context, backend *workflowkitStageBackend) (workflowkit.JobTerminalState, error) {
	if runtime == nil || runtime.workflowkitRegistry == nil {
		return "", ErrFrozenExecutionRuntimeConfiguration
	}
	claim, err := backend.claim()
	if err != nil {
		return "", err
	}
	engine, err := workflowkit.NewEngine(workflowkit.EngineConfig{Backend: backend, Executors: runtime.workflowkitRegistry})
	if err != nil {
		return "", fmt.Errorf("construct public workflow engine: %w", err)
	}
	state, err := engine.HandleClaim(ctx, claim)
	if err != nil {
		return "", fmt.Errorf("handle frozen public workflow claim: %w", err)
	}
	return state, nil
}

// workflowkitCoordinatorBackend adapts one already-leased Harbor coordinator
// job to the same public Engine boundary used by stage work. The generic
// Engine validates the frozen claim, loads durable node facts, and decides the
// next action; this adapter persists that already-decided action through
// FrozenExecutionRuntime.
type workflowkitCoordinatorBackend struct {
	callContext context.Context
	runtime     *FrozenExecutionRuntime
	execution   DurableJobExecution
	job         store.DurableJob
	run         store.WorkflowRun
	frozen      frozenRunDefinition
	plan        runtimeExecutionPlan
}

func (runtime *FrozenExecutionRuntime) executeWorkflowkitCoordinator(ctx context.Context, execution DurableJobExecution, job store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, plan runtimeExecutionPlan) (store.JobState, error) {
	backend := &workflowkitCoordinatorBackend{callContext: ctx, runtime: runtime, execution: execution, job: job, run: run, frozen: frozen, plan: plan}
	claim, err := backend.claim()
	if err != nil {
		return store.JobFailed, err
	}
	engine, err := workflowkit.NewEngine(workflowkit.EngineConfig{Backend: backend, Executors: runtime.workflowkitRegistry})
	if err != nil {
		return store.JobFailed, fmt.Errorf("construct public workflow coordinator engine: %w", err)
	}
	state, err := engine.HandleClaim(ctx, claim)
	if err != nil {
		return store.JobFailed, fmt.Errorf("handle frozen public workflow coordinator claim: %w", err)
	}
	if state != workflowkit.JobCompleted {
		return store.JobFailed, fmt.Errorf("%w: public workflow coordinator returned %q", ErrFrozenExecutionPayload, state)
	}
	return store.JobSucceeded, nil
}

func (backend *workflowkitCoordinatorBackend) claim() (workflowkit.JobClaim, error) {
	if backend == nil || backend.runtime == nil || backend.execution.Claim.Job == nil || backend.execution.Claim.Job.ID != backend.job.ID {
		return workflowkit.JobClaim{}, fmt.Errorf("%w: public Engine coordinator has no matching durable dispatch claim", ErrFrozenExecutionPayload)
	}
	subject, err := backend.runtime.core.resolveWorkflowRunSubject(backend.callContext, backend.run)
	if err != nil {
		return workflowkit.JobClaim{}, err
	}
	// frozenExecution owns the canonical managed-input, catalog receipt and
	// execution-spec proof used by both coordinator and stage claims.
	proof := &workflowkitStageBackend{
		callContext: backend.callContext, runtime: backend.runtime, job: backend.job,
		run: backend.run, subject: subject, frozen: backend.frozen,
		executionReason: "advance frozen workflow coordinator",
	}
	frozenExecution, err := proof.frozenExecution()
	if err != nil {
		return workflowkit.JobClaim{}, err
	}
	lease, err := currentWorkflowkitDispatchLease(backend.callContext, backend.runtime, backend.execution, backend.job)
	if err != nil {
		return workflowkit.JobClaim{}, err
	}
	schedule, err := backend.plan.frozenCoordinatorSchedule()
	if err != nil {
		return workflowkit.JobClaim{}, err
	}
	claim := workflowkit.JobClaim{
		JobID: backend.job.ID, ClaimID: backend.execution.Claim.ID, Kind: workflowkit.JobCoordinator,
		Owner: lease.Owner, FencingToken: lease.FencingToken, LeaseExpiresAt: lease.ExpiresAt,
		Execution: frozenExecution, Coordinator: &workflowkit.CoordinatorClaim{Schedule: schedule},
	}
	if err := claim.Validate(); err != nil {
		return workflowkit.JobClaim{}, fmt.Errorf("%w: construct public Engine coordinator claim: %v", ErrFrozenExecutionPayload, err)
	}
	return claim, nil
}

func (backend *workflowkitCoordinatorBackend) PrepareExecution(context.Context, workflowkit.PrepareRequest, workflowkit.FrozenExecution) (workflowkit.PreparedExecution, error) {
	return workflowkit.PreparedExecution{}, fmt.Errorf("%w: a claimed Harbor coordinator cannot prepare another execution", ErrFrozenExecutionPayload)
}

func (backend *workflowkitCoordinatorBackend) validateCoordinatorClaim(claim workflowkit.JobClaim) error {
	if backend == nil || backend.runtime == nil || claim.JobID != backend.job.ID || claim.ClaimID != backend.execution.Claim.ID || claim.Kind != workflowkit.JobCoordinator || claim.Execution.ID != backend.run.ID || claim.Coordinator == nil {
		return fmt.Errorf("%w: public Engine coordinator callback claim does not match the active Harbor job", ErrFrozenExecutionPayload)
	}
	return nil
}

// LoadCoordinatorInput is the Harbor-to-kernel half of the coordinator port.
// It loads only frozen plan and durable node facts; it makes no scheduling
// decision and cannot invoke a provider.
func (backend *workflowkitCoordinatorBackend) LoadCoordinatorInput(ctx context.Context, claim workflowkit.JobClaim) (workflowkit.CoordinatorInput, error) {
	if err := backend.validateCoordinatorClaim(claim); err != nil {
		return workflowkit.CoordinatorInput{}, err
	}
	if err := backend.plan.Workflow.Validate(); err != nil {
		return workflowkit.CoordinatorInput{}, fmt.Errorf("validate runtime workflow: %w", err)
	}
	if backend.frozen.QuotaPolicy.Fingerprint != backend.plan.QuotaPolicy.Fingerprint || backend.frozen.QuotaPolicy.ID != backend.plan.QuotaPolicy.ID || backend.frozen.QuotaPolicy.Version != backend.plan.QuotaPolicy.Version {
		return workflowkit.CoordinatorInput{}, fmt.Errorf("%w: execution plan quota policy differs from frozen run", ErrFrozenExecutionPayload)
	}
	stageJobs, err := backend.runtime.stageJobsForPlan(ctx, backend.run.ID, backend.plan.ExecutionKey)
	if err != nil {
		return workflowkit.CoordinatorInput{}, err
	}
	input, err := backend.runtime.workflowkitCoordinatorInput(ctx, backend.plan, stageJobs)
	if err != nil {
		return workflowkit.CoordinatorInput{}, err
	}
	if claim.Coordinator.Schedule.ExecutionKey != backend.plan.ExecutionKey {
		return workflowkit.CoordinatorInput{}, fmt.Errorf("%w: coordinator claim execution key differs from its durable plan", ErrFrozenExecutionPayload)
	}
	if err := claim.Coordinator.Schedule.ValidateInput(input); err != nil {
		return workflowkit.CoordinatorInput{}, fmt.Errorf("%w: verify frozen coordinator schedule: %v", ErrFrozenExecutionPayload, err)
	}
	return input.Clone(), nil
}

// CommitCoordinatorDecision is the kernel-to-Harbor half of the coordinator
// port.  It accepts no mutable stage selector or domain scheduling mode: the
// only admitted work is the exact batch chosen from the frozen input by the
// public workflowkit engine.
func (backend *workflowkitCoordinatorBackend) CommitCoordinatorDecision(ctx context.Context, claim workflowkit.JobClaim, decision workflowkit.CoordinatorDecision) (workflowkit.JobTerminalState, error) {
	if err := backend.validateCoordinatorClaim(claim); err != nil {
		return "", err
	}
	if err := backend.runtime.commitCoordinatorDecision(ctx, backend.job, backend.run, backend.frozen, backend.plan, decision.Clone()); err != nil {
		return "", err
	}
	return workflowkit.JobCompleted, nil
}

func (backend *workflowkitCoordinatorBackend) ReadStageInput(context.Context, workflowkit.JobClaim, workflowkit.ArtifactBinding) ([]byte, error) {
	return nil, fmt.Errorf("%w: a coordinator has no stage input", ErrFrozenExecutionPayload)
}

func (backend *workflowkitCoordinatorBackend) RecordStageCheckpoint(context.Context, workflowkit.JobClaim, workflowkit.StageCheckpoint) (workflowkit.CheckpointReceipt, error) {
	return workflowkit.CheckpointReceipt{}, fmt.Errorf("%w: a coordinator cannot checkpoint a stage", ErrFrozenExecutionPayload)
}

func (backend *workflowkitCoordinatorBackend) RecordStageUsage(context.Context, workflowkit.JobClaim, workflowkit.StageUsage) error {
	return fmt.Errorf("%w: a coordinator cannot charge stage usage", ErrFrozenExecutionPayload)
}

func (backend *workflowkitCoordinatorBackend) CommitStage(context.Context, workflowkit.StageCompletion) (workflowkit.JobTerminalState, error) {
	return "", fmt.Errorf("%w: a coordinator cannot commit a stage", ErrFrozenExecutionPayload)
}

func (backend *workflowkitCoordinatorBackend) CommitStageWait(context.Context, workflowkit.StageWaitCommit) (workflowkit.JobTerminalState, error) {
	return "", fmt.Errorf("%w: a coordinator cannot enter a stage wait", ErrFrozenExecutionPayload)
}

func (backend *workflowkitCoordinatorBackend) RejectStageClaim(context.Context, workflowkit.JobClaim, error) (workflowkit.JobTerminalState, error) {
	return "", fmt.Errorf("%w: a coordinator has no stage claim", ErrFrozenExecutionPayload)
}

func (backend *workflowkitCoordinatorBackend) ListRecoverySubjects(context.Context, workflowkit.RecoveryScope) ([]workflowkit.RecoverySubject, error) {
	return nil, fmt.Errorf("%w: a claimed Harbor coordinator cannot list recovery subjects", ErrFrozenExecutionPayload)
}

func (backend *workflowkitCoordinatorBackend) ApplyRecovery(context.Context, workflowkit.RecoveryScope, []workflowkit.RecoveryDecision) error {
	return fmt.Errorf("%w: a claimed Harbor coordinator cannot apply recovery", ErrFrozenExecutionPayload)
}

func (backend *workflowkitStageBackend) claim() (workflowkit.JobClaim, error) {
	if backend == nil || backend.runtime == nil {
		return workflowkit.JobClaim{}, ErrFrozenExecutionRuntimeConfiguration
	}
	if backend.execution.Claim.Job == nil || backend.execution.Claim.Job.ID != backend.job.ID {
		return workflowkit.JobClaim{}, fmt.Errorf("%w: public Engine stage has no matching durable dispatch claim", ErrFrozenExecutionPayload)
	}
	execution, err := backend.frozenExecution()
	if err != nil {
		return workflowkit.JobClaim{}, err
	}
	lease, err := currentWorkflowkitDispatchLease(backend.callContext, backend.runtime, backend.execution, backend.job)
	if err != nil {
		return workflowkit.JobClaim{}, err
	}
	control := workflowkitControlSignals(backend.callContext, nil)
	if backend.monitor != nil {
		control = workflowkitControlSignals(backend.callContext, backend.monitor.signals)
	}
	stageClaim := workflowkit.StageClaim{
		StageAttempt: workflowkit.AttemptIdentity{
			ID: workflowkit.AttemptID(backend.attempt.ID), Kind: workflowkit.AttemptStage,
			ScopeID: backend.run.ID + ":" + string(backend.stage.Key), Ordinal: 1,
		},
		Stage:      backend.stage.Clone(),
		Generation: backend.payload.Generation,
		Inputs:     append([]workflowkit.ArtifactBinding(nil), backend.inputs...),
		Control:    control,
	}
	if backend.node.ID != "" {
		node := workflowkit.AttemptIdentity{
			ID: workflowkit.AttemptID(backend.node.ID), Kind: workflowkit.AttemptNode,
			ScopeID: backend.run.ID + ":" + string(backend.stage.Key), ParentAttemptID: workflowkit.AttemptID(backend.attempt.ID),
			Ordinal: backend.node.Attempt,
		}
		stageClaim.NodeAttempt = &node
	}
	claim := workflowkit.JobClaim{
		JobID: backend.job.ID, ClaimID: backend.execution.Claim.ID, Kind: workflowkit.JobStage, Owner: lease.Owner,
		FencingToken: lease.FencingToken, LeaseExpiresAt: lease.ExpiresAt, Execution: execution, Stage: &stageClaim,
	}
	if err := claim.Validate(); err != nil {
		return workflowkit.JobClaim{}, fmt.Errorf("%w: construct public Engine stage claim: %v", ErrFrozenExecutionPayload, err)
	}
	return claim, nil
}

// currentWorkflowkitDispatchLease refreshes the durable lease snapshot after
// any Harbor-side admission work but immediately before the generic Engine
// checks it. Dispatch heartbeats update the Store record while a handler is
// running; carrying only the original claim snapshot would make a healthy,
// long-running worker appear expired to workflowkit.
func currentWorkflowkitDispatchLease(ctx context.Context, runtime *FrozenExecutionRuntime, execution DurableJobExecution, job store.DurableJob) (store.Lease, error) {
	if runtime == nil || runtime.core == nil || runtime.core.store == nil || execution.Claim.Job == nil || execution.Claim.Job.ID != job.ID || execution.Claim.DispatchLease == nil {
		return store.Lease{}, fmt.Errorf("%w: public Engine has no matching durable dispatch claim", ErrFrozenExecutionPayload)
	}
	claimed := execution.Claim.DispatchLease
	if claimed.FencingToken == 0 || claimed.ID == "" || strings.TrimSpace(claimed.Owner) == "" {
		return store.Lease{}, fmt.Errorf("%w: public Engine has an invalid durable dispatch lease", ErrFrozenExecutionPayload)
	}
	current, err := runtime.core.store.GetLease(ctx, claimed.ID)
	if err != nil {
		return store.Lease{}, fmt.Errorf("read current public Engine dispatch lease: %w", err)
	}
	if current == nil || current.ResourceType != "job_dispatch" || current.ResourceID != job.ID || current.JobID != job.ID ||
		current.Owner != claimed.Owner || current.FencingToken != claimed.FencingToken || current.State != store.LeaseActive || !current.ExpiresAt.After(time.Now().UTC()) {
		return store.Lease{}, store.ErrDispatchFenceLost
	}
	return *current, nil
}

func (backend *workflowkitStageBackend) frozenExecution() (workflowkit.FrozenExecution, error) {
	if backend == nil || backend.runtime == nil {
		return workflowkit.FrozenExecution{}, ErrFrozenExecutionRuntimeConfiguration
	}
	// The coordinator checks these files before it admits a stage job. Repeat
	// that proof at the public Engine boundary so callers that construct a
	// backend directly cannot turn a stale or substituted companion file into
	// an Engine claim.
	if _, _, err := backend.runtime.core.verifyRunManagedExecutionInputs(backend.callContext, backend.run); err != nil {
		return workflowkit.FrozenExecution{}, fmt.Errorf("%w: verify public Engine managed execution inputs: %w", ErrFrozenExecutionPayload, err)
	}
	// loadFrozenRun performs this check before a stage job is admitted. Keep it
	// at the public Engine bridge as well so a future caller cannot construct a
	// StageClaim from a catalog-drifted manifest by bypassing the normal
	// coordinator path.
	if err := backend.runtime.core.verifyRunDeploymentCatalogReceipt(backend.run); err != nil {
		return workflowkit.FrozenExecution{}, fmt.Errorf("%w: verify public Engine deployment catalog receipt: %w", ErrFrozenExecutionPayload, err)
	}
	var manifest runManifest
	if err := decodeStrictJSON(backend.run.RunManifestJSON, &manifest); err != nil {
		return workflowkit.FrozenExecution{}, fmt.Errorf("%w: decode public Engine run manifest: %v", ErrFrozenExecutionPayload, err)
	}
	if manifest.Format != "harbor.workflow-run-manifest.v2" || manifest.RunID != backend.run.ID || manifest.Inputs == nil || manifest.Inputs.Format != runManifestInputsFormat || len(manifest.ExecutionSpec) == 0 {
		return workflowkit.FrozenExecution{}, fmt.Errorf("%w: public Engine run manifest has no canonical execution specification", ErrFrozenExecutionPayload)
	}
	if err := validateRunManifestSubject(manifest, backend.run); err != nil {
		return workflowkit.FrozenExecution{}, fmt.Errorf("%w: public Engine run manifest subject: %v", ErrFrozenExecutionPayload, err)
	}
	specification, err := workflowadapter.ParseRunExecutionSpecJSON(manifest.ExecutionSpec)
	if err != nil {
		return workflowkit.FrozenExecution{}, fmt.Errorf("%w: parse public Engine execution specification: %v", ErrFrozenExecutionPayload, err)
	}
	canonical, err := specification.CanonicalJSON()
	if err != nil || string(canonical) != string(manifest.ExecutionSpec) {
		return workflowkit.FrozenExecution{}, fmt.Errorf("%w: public Engine execution specification is not canonical", ErrFrozenExecutionPayload)
	}
	specificationFingerprint, err := specification.Fingerprint()
	if err != nil || specificationFingerprint != manifest.Inputs.ExecutionSpecFingerprint {
		return workflowkit.FrozenExecution{}, fmt.Errorf("%w: public Engine execution specification fingerprint does not match manifest inputs", ErrFrozenExecutionPayload)
	}
	specificationSubject, err := specification.Selection.SubjectBinding()
	if err != nil || specificationSubject != backend.subject.Binding || !backend.subject.matchesRun(backend.run) {
		return workflowkit.FrozenExecution{}, fmt.Errorf("%w: public Engine execution specification selection does not match Run", ErrFrozenExecutionPayload)
	}
	binding, err := workflowkit.NewOpaqueExecutionBinding(workflowadapter.RunExecutionSpecFormat, workflowadapter.RunExecutionSpecVersion, canonical)
	if err != nil {
		return workflowkit.FrozenExecution{}, fmt.Errorf("%w: freeze public Engine execution binding: %v", ErrFrozenExecutionPayload, err)
	}
	createdAt := manifest.Created
	if createdAt.IsZero() {
		createdAt = backend.run.CreatedAt
	}
	reason := backend.executionReason
	if strings.TrimSpace(reason) == "" {
		reason = "execute frozen stage " + string(backend.stage.Key)
	}
	execution := workflowkit.FrozenExecution{
		ID:                    backend.run.ID,
		IdempotencyKey:        "workflow-run:" + backend.run.ID,
		Subject:               backend.subject.Binding,
		Workflow:              backend.frozen.Workflow.Clone(),
		DefinitionFingerprint: workflowkit.Fingerprint(backend.run.DefinitionHash),
		ProfileFingerprint:    manifest.Resolved.ExecutionProfileFingerprint,
		Binding:               binding,
		Plan:                  backend.frozen.InitialExecutionPlan.Clone(),
		Actor:                 backend.job.CreatedBy,
		Reason:                reason,
		CreatedAt:             createdAt.UTC(),
	}
	if err := execution.Validate(); err != nil {
		return workflowkit.FrozenExecution{}, fmt.Errorf("%w: validate public Engine frozen execution: %v", ErrFrozenExecutionPayload, err)
	}
	return execution, nil
}

func (backend *workflowkitStageBackend) PrepareExecution(context.Context, workflowkit.PrepareRequest, workflowkit.FrozenExecution) (workflowkit.PreparedExecution, error) {
	return workflowkit.PreparedExecution{}, fmt.Errorf("%w: a claimed Harbor stage backend cannot prepare another execution", ErrFrozenExecutionPayload)
}

func (backend *workflowkitStageBackend) LoadCoordinatorInput(context.Context, workflowkit.JobClaim) (workflowkit.CoordinatorInput, error) {
	return workflowkit.CoordinatorInput{}, fmt.Errorf("%w: a Harbor stage backend cannot load coordinator input", ErrFrozenExecutionPayload)
}

func (backend *workflowkitStageBackend) CommitCoordinatorDecision(context.Context, workflowkit.JobClaim, workflowkit.CoordinatorDecision) (workflowkit.JobTerminalState, error) {
	return "", fmt.Errorf("%w: a Harbor stage backend cannot commit a coordinator decision", ErrFrozenExecutionPayload)
}

func (backend *workflowkitStageBackend) ReadStageInput(ctx context.Context, claim workflowkit.JobClaim, binding workflowkit.ArtifactBinding) ([]byte, error) {
	if err := backend.matchesClaim(claim); err != nil {
		return nil, err
	}
	reader := newStageInputReaderForSubject(backend.runtime.core.store, backend.runtime.core.objects, backend.run, backend.subject, backend.inputs)
	return reader(ctx, binding)
}

func (backend *workflowkitStageBackend) RecordStageCheckpoint(ctx context.Context, claim workflowkit.JobClaim, checkpoint workflowkit.StageCheckpoint) (workflowkit.CheckpointReceipt, error) {
	if err := backend.matchesClaim(claim); err != nil {
		return workflowkit.CheckpointReceipt{}, err
	}
	if checkpoint.TurnOrdinal <= 0 {
		return workflowkit.CheckpointReceipt{}, fmt.Errorf("%w: public Engine checkpoint turn ordinal must be positive", ErrInvalidStageExecution)
	}
	payload, err := json.Marshal(struct {
		Format         string    `json:"format"`
		CheckpointID   string    `json:"checkpoint_id"`
		IdempotencyKey string    `json:"idempotency_key"`
		OccurredAt     time.Time `json:"occurred_at"`
		Payload        []byte    `json:"payload"`
	}{
		Format: "workflowkit.stage-checkpoint.v1", CheckpointID: checkpoint.CheckpointID, IdempotencyKey: checkpoint.IdempotencyKey,
		OccurredAt: checkpoint.OccurredAt.UTC(), Payload: checkpoint.Payload,
	})
	if err != nil {
		return workflowkit.CheckpointReceipt{}, err
	}
	writer := backend.runtime.stageCheckpointWriter(backend.attempt.ID, backend.node.ID, backend.attempt.InputFingerprint, backend.monitor)
	persisted, err := writer(ctx, StageCheckpoint{
		Turn: checkpoint.TurnOrdinal, Substep: checkpoint.Substep, PayloadJSON: string(payload), ArtifactID: string(checkpoint.ArtifactID), Resumable: checkpoint.Resumable,
	})
	if err != nil {
		return workflowkit.CheckpointReceipt{}, err
	}
	return workflowkit.CheckpointReceipt{CheckpointID: persisted.ID}, nil
}

func (backend *workflowkitStageBackend) RecordStageUsage(ctx context.Context, claim workflowkit.JobClaim, usage workflowkit.StageUsage) error {
	if err := backend.matchesClaim(claim); err != nil {
		return err
	}
	return backend.reservation.chargeWriter(backend.runtime, backend.attempt.ID, backend.node.ID)(ctx, StageUsage{
		OperationKey: usage.OperationKey, Dimension: usage.Dimension, Units: usage.Units, OccurredAt: usage.OccurredAt,
	})
}

func (backend *workflowkitStageBackend) CommitStage(_ context.Context, completion workflowkit.StageCompletion) (workflowkit.JobTerminalState, error) {
	if err := backend.matchesClaim(completion.Claim); err != nil {
		return "", err
	}
	if backend.review != nil {
		return "", fmt.Errorf("%w: review gate %q completed instead of waiting for an external decision", ErrFrozenExecutionPayload, backend.stage.Key)
	}
	result := completion.Result.Clone()
	backend.result = &result
	return workflowkit.JobCompleted, nil
}

func (backend *workflowkitStageBackend) CommitStageWait(ctx context.Context, commit workflowkit.StageWaitCommit) (workflowkit.JobTerminalState, error) {
	if err := backend.matchesClaim(commit.Claim); err != nil {
		return "", err
	}
	if backend.review == nil || commit.Wait.Kind != workflowkit.StageWaitExternalDecision {
		return "", fmt.Errorf("%w: only a frozen Harbor review gate may enter a public Engine external-decision wait", ErrFrozenExecutionPayload)
	}
	state, err := backend.runtime.openReviewGate(ctx, backend.job, backend.run, backend.subject, backend.frozen, backend.payload, backend.stage, backend.attempt, backend.inputs, *backend.review)
	if err != nil {
		return "", err
	}
	backend.reviewResult = &state
	return workflowkitJobTerminalState(state), nil
}

func (backend *workflowkitStageBackend) RejectStageClaim(_ context.Context, claim workflowkit.JobClaim, cause error) (workflowkit.JobTerminalState, error) {
	if err := backend.matchesClaim(claim); err != nil {
		return "", err
	}
	backend.rejected = cause
	return workflowkit.JobCompleted, nil
}

func (backend *workflowkitStageBackend) ListRecoverySubjects(context.Context, workflowkit.RecoveryScope) ([]workflowkit.RecoverySubject, error) {
	return nil, fmt.Errorf("%w: a claimed Harbor stage backend cannot list recovery subjects", ErrFrozenExecutionPayload)
}

func (backend *workflowkitStageBackend) ApplyRecovery(context.Context, workflowkit.RecoveryScope, []workflowkit.RecoveryDecision) error {
	return fmt.Errorf("%w: a claimed Harbor stage backend cannot apply recovery", ErrFrozenExecutionPayload)
}

func (backend *workflowkitStageBackend) matchesClaim(claim workflowkit.JobClaim) error {
	if backend == nil || claim.JobID != backend.job.ID || claim.ClaimID != backend.execution.Claim.ID || claim.Execution.ID != backend.run.ID || claim.Stage == nil || claim.Stage.StageAttempt.ID != workflowkit.AttemptID(backend.attempt.ID) || claim.Stage.Stage.Key != backend.stage.Key {
		return fmt.Errorf("%w: public Engine callback claim does not match the active Harbor stage", ErrFrozenExecutionPayload)
	}
	if backend.node.ID == "" {
		if claim.Stage.NodeAttempt != nil {
			return fmt.Errorf("%w: public Engine stage claim unexpectedly carries a node attempt", ErrFrozenExecutionPayload)
		}
		return nil
	}
	if claim.Stage.NodeAttempt == nil || claim.Stage.NodeAttempt.ID != workflowkit.AttemptID(backend.node.ID) || claim.Stage.NodeAttempt.Ordinal != backend.node.Attempt {
		return fmt.Errorf("%w: public Engine stage node attempt does not match the active Harbor node", ErrFrozenExecutionPayload)
	}
	return nil
}

func stageResultFromWorkflowkit(result workflowkit.StageExecutionResult) StageExecutionResult {
	converted := StageExecutionResult{Outcome: result.Outcome, ErrorText: result.ErrorText, FailureClass: string(result.Outcome.Failure)}
	converted.Artifacts = make([]StageArtifact, len(result.Artifacts))
	for index, artifact := range result.Artifacts {
		converted.Artifacts[index] = StageArtifact{ID: string(artifact.ID), Key: artifact.Name, SchemaVersion: artifact.SchemaVersion, Content: append([]byte(nil), artifact.Content...), TurnOrdinal: artifact.TurnOrdinal}
	}
	return converted
}

func workflowkitJobTerminalState(state store.JobState) workflowkit.JobTerminalState {
	switch state {
	case store.JobSucceeded:
		return workflowkit.JobCompleted
	case store.JobInterrupted, store.JobInDoubt:
		return workflowkit.JobReconcileRequired
	default:
		return workflowkit.JobCompleted
	}
}

func workflowkitControlSignals(ctx context.Context, source <-chan StageControlSignal) <-chan workflowkit.StageControlSignal {
	if source == nil {
		return nil
	}
	destination := make(chan workflowkit.StageControlSignal, 1)
	go func() {
		defer close(destination)
		for {
			select {
			case <-ctx.Done():
				return
			case signal, open := <-source:
				if !open {
					return
				}
				var action workflowkit.ControlAction
				switch signal.Action {
				case store.ControlActionPause:
					action = workflowkit.ControlPause
				case store.ControlActionCancelStage:
					action = workflowkit.ControlCancelStage
				case store.ControlActionTerminate:
					action = workflowkit.ControlTerminate
				default:
					return
				}
				select {
				case destination <- workflowkit.StageControlSignal{Action: action, GracePeriod: signal.GracePeriod}:
				case <-ctx.Done():
				}
				return
			}
		}
	}()
	return destination
}

var _ workflowkit.DurableBackend = (*workflowkitStageBackend)(nil)
var _ workflowkit.DurableBackend = (*workflowkitCoordinatorBackend)(nil)
