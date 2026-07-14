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
	revision    store.TaskRevision
	frozen      frozenRunDefinition
	payload     frozenStageExecutionPayload
	stage       workflowkit.StageDescriptor
	attempt     store.StageAttempt
	node        store.NodeAttempt
	inputs      []workflowkit.ArtifactBinding
	reservation stageQuotaReservation
	monitor     *stageControlMonitor
	review      *workflowadapter.ReviewStage

	result       *workflowkit.StageExecutionResult
	rejected     error
	reviewResult *store.JobState
}

func (runtime *FrozenExecutionRuntime) executeWorkflowkitStage(ctx context.Context, execution DurableJobExecution, job store.DurableJob, run store.WorkflowRun, revision store.TaskRevision, frozen frozenRunDefinition, payload frozenStageExecutionPayload, attempt store.StageAttempt, node store.NodeAttempt, stage workflowkit.StageDescriptor, inputs []workflowkit.ArtifactBinding, reservation stageQuotaReservation, monitor *stageControlMonitor) (StageExecutionResult, error) {
	backend := &workflowkitStageBackend{
		callContext: ctx,
		runtime:     runtime, execution: execution, job: job, run: run, revision: revision, frozen: frozen, payload: payload,
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

func (runtime *FrozenExecutionRuntime) executeWorkflowkitReviewGate(ctx context.Context, execution DurableJobExecution, job store.DurableJob, run store.WorkflowRun, revision store.TaskRevision, frozen frozenRunDefinition, payload frozenStageExecutionPayload, attempt store.StageAttempt, stage workflowkit.StageDescriptor, inputs []workflowkit.ArtifactBinding, review workflowadapter.ReviewStage) (store.JobState, error) {
	backend := &workflowkitStageBackend{
		callContext: ctx,
		runtime:     runtime, execution: execution, job: job, run: run, revision: revision, frozen: frozen, payload: payload,
		stage: stage.Clone(), attempt: attempt, inputs: append([]workflowkit.ArtifactBinding(nil), inputs...), review: &review,
	}
	if _, err := runtime.handleWorkflowkitStageClaim(ctx, backend); err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	if backend.rejected != nil {
		return runtime.projectStageTerminal(ctx, job, run, frozen, payload, revision, stage, attempt, inputs, stageQuotaReservation{}, StageExecutionResult{
			Outcome:      workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailurePolicy},
			ErrorText:    backend.rejected.Error(),
			FailureClass: string(workflowkit.FailurePolicy),
		}, execution.LeaseLost, nil)
	}
	if backend.reviewResult == nil {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("%w: public Engine review gate did not commit an external-decision wait", ErrFrozenExecutionPayload))
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

func (backend *workflowkitStageBackend) claim() (workflowkit.JobClaim, error) {
	if backend == nil || backend.runtime == nil {
		return workflowkit.JobClaim{}, ErrFrozenExecutionRuntimeConfiguration
	}
	if backend.execution.Claim.Job == nil || backend.execution.Claim.Job.ID != backend.job.ID || backend.execution.Claim.DispatchLease == nil {
		return workflowkit.JobClaim{}, fmt.Errorf("%w: public Engine stage has no matching durable dispatch claim", ErrFrozenExecutionPayload)
	}
	lease := backend.execution.Claim.DispatchLease
	if lease.FencingToken == 0 || lease.ExpiresAt.IsZero() || strings.TrimSpace(lease.Owner) == "" {
		return workflowkit.JobClaim{}, fmt.Errorf("%w: public Engine stage has an invalid durable dispatch lease", ErrFrozenExecutionPayload)
	}
	execution, err := backend.frozenExecution()
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
	claim := workflowkit.JobClaim{
		JobID: backend.job.ID, ClaimID: backend.execution.Claim.ID, Kind: workflowkit.JobStage, Owner: lease.Owner,
		FencingToken: lease.FencingToken, LeaseExpiresAt: lease.ExpiresAt, Execution: execution, Stage: &stageClaim,
	}
	if err := claim.Validate(); err != nil {
		return workflowkit.JobClaim{}, fmt.Errorf("%w: construct public Engine stage claim: %v", ErrFrozenExecutionPayload, err)
	}
	return claim, nil
}

func (backend *workflowkitStageBackend) frozenExecution() (workflowkit.FrozenExecution, error) {
	if backend == nil || backend.runtime == nil {
		return workflowkit.FrozenExecution{}, ErrFrozenExecutionRuntimeConfiguration
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
	if manifest.Format != "harbor.workflow-run-manifest.v2" || manifest.RunID != backend.run.ID || manifest.TaskID != backend.run.TaskID || manifest.Revision != backend.run.RevisionID || manifest.Inputs == nil || manifest.Inputs.Format != runManifestInputsFormat || len(manifest.ExecutionSpec) == 0 {
		return workflowkit.FrozenExecution{}, fmt.Errorf("%w: public Engine run manifest has no canonical execution specification", ErrFrozenExecutionPayload)
	}
	specification, err := workflowadapter.ParseRunExecutionSpecJSON(manifest.ExecutionSpec)
	if err != nil {
		return workflowkit.FrozenExecution{}, fmt.Errorf("%w: parse public Engine execution specification: %v", ErrFrozenExecutionPayload, err)
	}
	canonical, err := specification.CanonicalJSON()
	if err != nil || string(canonical) != string(manifest.ExecutionSpec) {
		return workflowkit.FrozenExecution{}, fmt.Errorf("%w: public Engine execution specification is not canonical", ErrFrozenExecutionPayload)
	}
	binding, err := workflowkit.NewOpaqueExecutionBinding(workflowadapter.RunExecutionSpecFormat, workflowadapter.RunExecutionSpecVersion, canonical)
	if err != nil {
		return workflowkit.FrozenExecution{}, fmt.Errorf("%w: freeze public Engine execution binding: %v", ErrFrozenExecutionPayload, err)
	}
	createdAt := manifest.Created
	if createdAt.IsZero() {
		createdAt = backend.run.CreatedAt
	}
	execution := workflowkit.FrozenExecution{
		ID:             backend.run.ID,
		IdempotencyKey: "workflow-run:" + backend.run.ID,
		Subject: workflowkit.SubjectBinding{
			SubjectID: backend.run.TaskID, RevisionID: backend.run.RevisionID, Digest: workflowkit.SubjectDigest(backend.revision.TaskDigest),
		},
		Workflow:              backend.frozen.Workflow.Clone(),
		DefinitionFingerprint: workflowkit.Fingerprint(backend.run.DefinitionHash),
		ProfileFingerprint:    manifest.Resolved.ExecutionProfileFingerprint,
		Binding:               binding,
		Plan:                  backend.frozen.InitialExecutionPlan.Clone(),
		Actor:                 backend.job.CreatedBy,
		Reason:                "execute frozen stage " + string(backend.stage.Key),
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

func (backend *workflowkitStageBackend) AdvanceCoordinator(context.Context, workflowkit.JobClaim) (workflowkit.JobTerminalState, error) {
	return "", fmt.Errorf("%w: a Harbor stage backend cannot advance a coordinator", ErrFrozenExecutionPayload)
}

func (backend *workflowkitStageBackend) ReadStageInput(ctx context.Context, claim workflowkit.JobClaim, binding workflowkit.ArtifactBinding) ([]byte, error) {
	if err := backend.matchesClaim(claim); err != nil {
		return nil, err
	}
	reader := newStageInputReader(backend.runtime.core.store, backend.runtime.core.objects, backend.run, backend.revision, backend.inputs)
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
	state, err := backend.runtime.openReviewGate(ctx, backend.job, backend.run, backend.revision, backend.frozen, backend.payload, backend.stage, backend.attempt, backend.inputs, *backend.review)
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
	return nil
}

func stageResultFromWorkflowkit(result workflowkit.StageExecutionResult) StageExecutionResult {
	converted := StageExecutionResult{Outcome: result.Outcome, ErrorText: result.ErrorText, FailureClass: string(result.Outcome.Failure)}
	converted.Artifacts = make([]StageArtifact, len(result.Artifacts))
	for index, artifact := range result.Artifacts {
		converted.Artifacts[index] = StageArtifact{Key: artifact.Name, SchemaVersion: artifact.SchemaVersion, Content: append([]byte(nil), artifact.Content...), TurnOrdinal: artifact.TurnOrdinal}
	}
	return converted
}

func workflowkitJobTerminalState(state store.JobState) workflowkit.JobTerminalState {
	switch state {
	case store.JobSucceeded:
		return workflowkit.JobCompleted
	case store.JobInterrupted:
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
