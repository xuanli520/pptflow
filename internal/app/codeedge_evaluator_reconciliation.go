package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	codeEdgeEvaluatorObservedCompletionFormat    = "harbor.codeedge-evaluator-observed-completion.v1"
	codeEdgeEvaluatorReconciliationCommandType   = "codeedge_evaluator.reconcile"
	codeEdgeEvaluatorReconciliationPayloadFormat = "harbor.codeedge-evaluator-reconcile.v1"
	codeEdgeEvaluatorReconciliationEntityType    = "stage_attempt"
)

var errCodeEdgeEvaluatorObservationUnavailable = errors.New("CodeEdge evaluator completed evidence is unavailable")

// codeEdgeEvaluatorReconciliationPayload binds a read-only observation job to
// the immutable stage delivery that crossed the evaluator invocation fence.
// It deliberately carries no command, endpoint, workspace, or credential
// values: the provider-specific observer resolves those only through the
// frozen deployment catalog and its lock.
type codeEdgeEvaluatorReconciliationPayload struct {
	Format           string                              `json:"format"`
	RunID            string                              `json:"run_id"`
	StageAttemptID   string                              `json:"stage_attempt_id"`
	StageKey         workflowkit.StageKey                `json:"stage_key"`
	DefinitionHash   string                              `json:"definition_hash"`
	Generation       int                                 `json:"generation"`
	QuotaPolicy      workflowadapter.ResolvedQuotaPolicy `json:"quota_policy"`
	SourceStageJobID string                              `json:"source_stage_job_id"`
}

// codeEdgeEvaluatorObservedCompletion records only immutable artifact facts
// proven by a local read-only Harbor job observation. It intentionally omits
// the raw Harbor transcript, workspace path, endpoint, and credential values.
type codeEdgeEvaluatorObservedCompletion struct {
	Format              string                  `json:"format"`
	RunID               string                  `json:"run_id"`
	StageAttemptID      string                  `json:"stage_attempt_id"`
	StageKey            workflowkit.StageKey    `json:"stage_key"`
	SideEffectOperation string                  `json:"side_effect_operation_id"`
	ArtifactManifestID  string                  `json:"artifact_manifest_id"`
	ArtifactFingerprint workflowkit.Fingerprint `json:"artifact_manifest_fingerprint"`
}

// reconcileObservedCodeEdgeEvaluator attempts the approved provider-specific
// observation after the generic durable worker has lost the original stage
// lease. It does not provide an execution fallback: an absent observer or an
// incomplete/malformed observation simply leaves the existing effect in_doubt.
func (runtime *FrozenExecutionRuntime) reconcileObservedCodeEdgeEvaluator(ctx context.Context, job store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, payload frozenStageExecutionPayload, stageAttempt store.StageAttempt, stage workflowkit.StageDescriptor, effect store.SideEffectOperation) (bool, error) {
	if runtime == nil || runtime.core == nil || runtime.core.evaluatorObserver == nil {
		return false, nil
	}
	revision, err := runtime.core.store.GetTaskRevision(ctx, run.RevisionID)
	if err != nil {
		return false, err
	}
	if revision == nil || revision.TaskID != run.TaskID {
		return false, fmt.Errorf("%w: CodeEdge evaluator reconciliation revision", ErrLifecycleNotFound)
	}
	request, err := runtime.codeEdgeEvaluatorObservationRequest(ctx, job, run, *revision, frozen, payload, stageAttempt, stage)
	if err != nil {
		return false, err
	}
	result, observed, err := runtime.core.evaluatorObserver.ObserveCompletedCodeEdgeEvaluator(ctx, request)
	if err != nil {
		return false, fmt.Errorf("%w: observer read failed: %v", errCodeEdgeEvaluatorObservationUnavailable, err)
	}
	if !observed {
		return false, nil
	}
	if result.Outcome.Status != workflowkit.StatusCompleted || result.Outcome.Verdict != workflowkit.VerdictPass {
		return false, fmt.Errorf("%w: observer returned a non-completed result", errCodeEdgeEvaluatorObservationUnavailable)
	}
	converted := stageResultFromWorkflowkit(result)
	if err := validateCodeEdgeObservedEvaluatorArtifacts(stage, converted.Artifacts); err != nil {
		return false, fmt.Errorf("%w: %v", errCodeEdgeEvaluatorObservationUnavailable, err)
	}
	state, err := runtime.projectObservedCodeEdgeEvaluatorCompletion(ctx, job, run, *revision, frozen, payload, stageAttempt, stage, effect, request.Execution.Inputs, converted)
	if err != nil {
		return false, err
	}
	return state == store.JobSucceeded, nil
}

func codeEdgeEvaluatorReconciliationJobKey(sourceStageJobID string) string {
	return "codeedge-evaluator-reconcile-job:v1:" + sourceStageJobID
}

// enqueueCodeEdgeEvaluatorReconciliation creates one idempotent, read-only
// observer delivery after all in_doubt facts are durable. It never replaces or
// retries the source stage job; its source identity is preserved in the
// payload and validated again before every observation.
func (runtime *FrozenExecutionRuntime) enqueueCodeEdgeEvaluatorReconciliation(ctx context.Context, sourceJob store.DurableJob, run store.WorkflowRun, stageAttempt store.StageAttempt, stage workflowkit.StageDescriptor) error {
	if sourceJob.CommandType != "stage_attempt.execute" || sourceJob.EntityType != codeEdgeEvaluatorReconciliationEntityType || sourceJob.EntityID != stageAttempt.ID || sourceJob.RunID != run.ID || sourceJob.StageAttemptID != stageAttempt.ID {
		return fmt.Errorf("%w: CodeEdge evaluator reconciliation source job does not bind its stage", ErrFrozenExecutionPayload)
	}
	var sourcePayload frozenStageExecutionPayload
	if err := decodeStrictJSON(sourceJob.PayloadJSON, &sourcePayload); err != nil {
		return fmt.Errorf("decode CodeEdge evaluator reconciliation source payload: %w", err)
	}
	if sourcePayload.Format != frozenStageExecutionPayloadFormat || sourcePayload.RunID != run.ID || sourcePayload.StageAttemptID != stageAttempt.ID || sourcePayload.StageKey != stage.Key || sourcePayload.DefinitionHash != run.DefinitionHash || sourcePayload.Generation < 0 {
		return fmt.Errorf("%w: CodeEdge evaluator reconciliation source payload does not bind its stage", ErrFrozenExecutionPayload)
	}
	if err := sourcePayload.QuotaPolicy.Validate(); err != nil {
		return fmt.Errorf("%w: CodeEdge evaluator reconciliation source quota policy: %v", ErrFrozenExecutionPayload, err)
	}
	payload := codeEdgeEvaluatorReconciliationPayload{
		Format:           codeEdgeEvaluatorReconciliationPayloadFormat,
		RunID:            run.ID,
		StageAttemptID:   stageAttempt.ID,
		StageKey:         stage.Key,
		DefinitionHash:   sourcePayload.DefinitionHash,
		Generation:       sourcePayload.Generation,
		QuotaPolicy:      sourcePayload.QuotaPolicy.Clone(),
		SourceStageJobID: sourceJob.ID,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = runtime.core.store.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: codeEdgeEvaluatorReconciliationCommandType, EntityType: codeEdgeEvaluatorReconciliationEntityType,
		EntityID: stageAttempt.ID, RunID: run.ID, StageAttemptID: stageAttempt.ID, Priority: sourceJob.Priority,
		PayloadJSON: string(encoded), IdempotencyKey: codeEdgeEvaluatorReconciliationJobKey(sourceJob.ID),
		Actor: sourceJob.CreatedBy, Reason: "observe completed CodeEdge evaluator local evidence",
	})
	return err
}

func (runtime *FrozenExecutionRuntime) handleCodeEdgeEvaluatorReconciliation(ctx context.Context, _ DurableJobExecution, job store.DurableJob) (store.JobState, error) {
	binding, err := runtime.loadCodeEdgeEvaluatorReconciliationBinding(ctx, job)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	currentRun, err := runtime.core.store.GetWorkflowRun(ctx, binding.run.ID)
	if err != nil {
		return store.JobFailed, err
	}
	if currentRun == nil {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: CodeEdge evaluator reconciliation Run", ErrLifecycleNotFound))
	}
	if binding.effect.State == store.SideEffectSucceeded && binding.stageAttempt.ExecutionStatus == store.StageExecutionCompleted {
		// A crash can occur after the stage/effect receipt commits but before
		// the reconciliation record or successor coordinator. Reconstruct only
		// that already-proven local receipt; do not re-read provider evidence.
		return runtime.resumeObservedCodeEdgeEvaluatorCompletion(ctx, binding.sourceJob, binding.run, binding.frozen, binding.sourcePayload, binding.stage, binding.stageAttempt, binding.effect)
	}
	if currentRun.Status != store.WorkflowRunInDoubt || (binding.stageAttempt.ExecutionStatus != store.StageExecutionInDoubt && binding.stageAttempt.ExecutionStatus != store.StageExecutionReconciling) || binding.effect.State != store.SideEffectUnknown {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: CodeEdge evaluator reconciliation state is not in_doubt", ErrFrozenExecutionPayload))
	}
	if _, err := runtime.reconcileObservedCodeEdgeEvaluator(ctx, binding.sourceJob, *currentRun, binding.frozen, binding.sourcePayload, binding.stageAttempt, binding.stage, binding.effect); err != nil {
		if errors.Is(err, errCodeEdgeEvaluatorObservationUnavailable) {
			// Observation failures are not execution failures. The durable
			// in_doubt projection remains intact and this one read-only delivery
			// is complete, so the worker must never turn it into a provider retry.
			return store.JobSucceeded, nil
		}
		return store.JobFailed, err
	}
	return store.JobSucceeded, nil
}

func validateCodeEdgeEvaluatorReconciliationPayload(payload codeEdgeEvaluatorReconciliationPayload, job store.DurableJob) error {
	if job.CommandType != codeEdgeEvaluatorReconciliationCommandType || payload.Format != codeEdgeEvaluatorReconciliationPayloadFormat || payload.RunID != job.RunID || payload.StageAttemptID != job.StageAttemptID || payload.StageAttemptID != job.EntityID || job.EntityType != codeEdgeEvaluatorReconciliationEntityType || payload.StageKey == "" || payload.DefinitionHash == "" || payload.SourceStageJobID == "" || payload.Generation < 0 {
		return fmt.Errorf("%w: CodeEdge evaluator reconciliation job does not bind its immutable source", ErrFrozenExecutionPayload)
	}
	if err := store.ValidateUUIDv7(payload.RunID); err != nil {
		return fmt.Errorf("%w: CodeEdge evaluator reconciliation run identity: %v", ErrFrozenExecutionPayload, err)
	}
	if err := store.ValidateUUIDv7(payload.StageAttemptID); err != nil {
		return fmt.Errorf("%w: CodeEdge evaluator reconciliation stage identity: %v", ErrFrozenExecutionPayload, err)
	}
	if err := store.ValidateUUIDv7(payload.SourceStageJobID); err != nil {
		return fmt.Errorf("%w: CodeEdge evaluator reconciliation source job identity: %v", ErrFrozenExecutionPayload, err)
	}
	if err := workflowkit.Fingerprint(payload.DefinitionHash).Validate(); err != nil {
		return fmt.Errorf("%w: CodeEdge evaluator reconciliation definition fingerprint: %v", ErrFrozenExecutionPayload, err)
	}
	if err := payload.QuotaPolicy.Validate(); err != nil {
		return fmt.Errorf("%w: CodeEdge evaluator reconciliation quota policy: %v", ErrFrozenExecutionPayload, err)
	}
	return nil
}

func (runtime *FrozenExecutionRuntime) codeEdgeEvaluatorReconciliationSourceJob(ctx context.Context, reconciliationJob store.DurableJob, payload codeEdgeEvaluatorReconciliationPayload) (store.DurableJob, frozenStageExecutionPayload, error) {
	sourceJob, err := runtime.core.store.GetDurableJob(ctx, payload.SourceStageJobID)
	if err != nil {
		return store.DurableJob{}, frozenStageExecutionPayload{}, err
	}
	if sourceJob == nil {
		return store.DurableJob{}, frozenStageExecutionPayload{}, fmt.Errorf("%w: CodeEdge evaluator reconciliation source job", ErrLifecycleNotFound)
	}
	if sourceJob.CommandType != "stage_attempt.execute" || sourceJob.EntityType != codeEdgeEvaluatorReconciliationEntityType || sourceJob.EntityID != payload.StageAttemptID || sourceJob.RunID != payload.RunID || sourceJob.StageAttemptID != payload.StageAttemptID || sourceJob.CreatedBy != reconciliationJob.CreatedBy {
		return store.DurableJob{}, frozenStageExecutionPayload{}, fmt.Errorf("%w: CodeEdge evaluator reconciliation source job does not match payload", ErrFrozenExecutionPayload)
	}
	var sourcePayload frozenStageExecutionPayload
	if err := decodeStrictJSON(sourceJob.PayloadJSON, &sourcePayload); err != nil {
		return store.DurableJob{}, frozenStageExecutionPayload{}, fmt.Errorf("decode CodeEdge evaluator reconciliation source stage payload: %w", err)
	}
	if sourcePayload.Format != frozenStageExecutionPayloadFormat || sourcePayload.RunID != payload.RunID || sourcePayload.StageAttemptID != payload.StageAttemptID || sourcePayload.StageKey != payload.StageKey || sourcePayload.DefinitionHash != payload.DefinitionHash || sourcePayload.Generation != payload.Generation || !reflect.DeepEqual(sourcePayload.QuotaPolicy, payload.QuotaPolicy) {
		return store.DurableJob{}, frozenStageExecutionPayload{}, fmt.Errorf("%w: CodeEdge evaluator reconciliation source payload drift", ErrFrozenExecutionPayload)
	}
	return *sourceJob, sourcePayload, nil
}

type codeEdgeEvaluatorReconciliationBinding struct {
	sourceJob     store.DurableJob
	sourcePayload frozenStageExecutionPayload
	run           store.WorkflowRun
	frozen        frozenRunDefinition
	stage         workflowkit.StageDescriptor
	stageAttempt  store.StageAttempt
	effect        store.SideEffectOperation
}

func (runtime *FrozenExecutionRuntime) loadCodeEdgeEvaluatorReconciliationBinding(ctx context.Context, job store.DurableJob) (codeEdgeEvaluatorReconciliationBinding, error) {
	var payload codeEdgeEvaluatorReconciliationPayload
	if err := decodeStrictJSON(job.PayloadJSON, &payload); err != nil {
		return codeEdgeEvaluatorReconciliationBinding{}, fmt.Errorf("decode CodeEdge evaluator reconciliation payload: %w", err)
	}
	if err := validateCodeEdgeEvaluatorReconciliationPayload(payload, job); err != nil {
		return codeEdgeEvaluatorReconciliationBinding{}, err
	}
	sourceJob, sourcePayload, err := runtime.codeEdgeEvaluatorReconciliationSourceJob(ctx, job, payload)
	if err != nil {
		return codeEdgeEvaluatorReconciliationBinding{}, err
	}
	run, frozen, err := runtime.loadFrozenRun(ctx, payload.RunID, payload.DefinitionHash, "", payload.QuotaPolicy)
	if err != nil {
		return codeEdgeEvaluatorReconciliationBinding{}, err
	}
	stage, found := frozen.Workflow.Stage(payload.StageKey)
	if !found || !isCodeEdgeEvaluatorStage(run, stage) {
		return codeEdgeEvaluatorReconciliationBinding{}, fmt.Errorf("%w: CodeEdge evaluator reconciliation stage %q is not frozen", ErrFrozenExecutionPayload, payload.StageKey)
	}
	stageAttempt, err := runtime.core.store.GetStageAttempt(ctx, payload.StageAttemptID)
	if err != nil {
		return codeEdgeEvaluatorReconciliationBinding{}, err
	}
	if stageAttempt == nil || stageAttempt.RunID != run.ID || stageAttempt.StageKey != string(stage.Key) {
		return codeEdgeEvaluatorReconciliationBinding{}, fmt.Errorf("%w: CodeEdge evaluator reconciliation stage attempt does not match its source", ErrFrozenExecutionPayload)
	}
	if err := runtime.validateStageAttemptPlanBinding(*stageAttempt, sourcePayload); err != nil {
		return codeEdgeEvaluatorReconciliationBinding{}, err
	}
	effect, err := runtime.codeEdgeEvaluatorEffectAlreadyStarted(ctx, run, *stageAttempt, stage)
	if err != nil {
		return codeEdgeEvaluatorReconciliationBinding{}, err
	}
	if effect == nil {
		return codeEdgeEvaluatorReconciliationBinding{}, fmt.Errorf("%w: CodeEdge evaluator reconciliation source has no started effect", ErrFrozenExecutionPayload)
	}
	return codeEdgeEvaluatorReconciliationBinding{
		sourceJob: sourceJob, sourcePayload: sourcePayload, run: run, frozen: frozen,
		stage: stage, stageAttempt: *stageAttempt, effect: *effect,
	}, nil
}

// reconcileRecoveredCodeEdgeEvaluatorReconciliation repairs only an already
// committed local receipt after the observer delivery itself lost its lease.
// It intentionally does not call the observer: an unknown read may not be
// replayed after its dispatch fence expires, and a completed receipt needs no
// external provider access to finish its deterministic projection.
func (runtime *FrozenExecutionRuntime) reconcileRecoveredCodeEdgeEvaluatorReconciliation(ctx context.Context, job store.DurableJob) error {
	binding, err := runtime.loadCodeEdgeEvaluatorReconciliationBinding(ctx, job)
	if err != nil {
		_, projected := runtime.failRuntimeJob(ctx, job, err)
		return projected
	}
	if binding.effect.State != store.SideEffectSucceeded || binding.stageAttempt.ExecutionStatus != store.StageExecutionCompleted {
		return nil
	}
	_, err = runtime.resumeObservedCodeEdgeEvaluatorCompletion(ctx, binding.sourceJob, binding.run, binding.frozen, binding.sourcePayload, binding.stage, binding.stageAttempt, binding.effect)
	return err
}

func (runtime *FrozenExecutionRuntime) codeEdgeEvaluatorObservationRequest(ctx context.Context, job store.DurableJob, run store.WorkflowRun, revision store.TaskRevision, frozen frozenRunDefinition, payload frozenStageExecutionPayload, stageAttempt store.StageAttempt, stage workflowkit.StageDescriptor) (CodeEdgeEvaluatorObservationRequest, error) {
	_, specification, err := runtime.core.verifyRunManagedExecutionInputs(ctx, run)
	if err != nil {
		return CodeEdgeEvaluatorObservationRequest{}, fmt.Errorf("%w: verify CodeEdge evaluator reconciliation inputs: %w", ErrFrozenExecutionPayload, err)
	}
	resolution, err := specification.ResolveStageOperation(stage.Key)
	if err != nil {
		return CodeEdgeEvaluatorObservationRequest{}, fmt.Errorf("%w: resolve CodeEdge evaluator reconciliation operation: %w", ErrFrozenExecutionPayload, err)
	}
	inputs, err := resolveStageInputs(ctx, runtime.core.store, runtime.core.objects, run, revision, stage)
	if err != nil {
		return CodeEdgeEvaluatorObservationRequest{}, err
	}
	inputFingerprint, err := workflowkit.FingerprintArtifactBindings(inputs)
	if err != nil {
		return CodeEdgeEvaluatorObservationRequest{}, err
	}
	if string(inputFingerprint) != stageAttempt.InputFingerprint {
		return CodeEdgeEvaluatorObservationRequest{}, fmt.Errorf("%w: CodeEdge evaluator reconciliation input fingerprint drift", ErrFrozenExecutionPayload)
	}
	backend := &workflowkitStageBackend{
		callContext: ctx,
		runtime:     runtime,
		job:         job,
		run:         run,
		subject:     taskRevisionSubjectForLineage(run, revision),
		frozen:      frozen,
		payload:     payload,
		stage:       stage.Clone(),
		attempt:     stageAttempt,
		inputs:      append([]workflowkit.ArtifactBinding(nil), inputs...),
	}
	execution, err := backend.frozenExecution()
	if err != nil {
		return CodeEdgeEvaluatorObservationRequest{}, err
	}
	stageClaim := workflowkit.StageClaim{
		StageAttempt: workflowkit.AttemptIdentity{
			ID: workflowkit.AttemptID(stageAttempt.ID), Kind: workflowkit.AttemptStage,
			ScopeID: run.ID + ":" + string(stage.Key), Ordinal: 1,
		},
		Stage:      stage.Clone(),
		Generation: payload.Generation,
		Inputs:     append([]workflowkit.ArtifactBinding(nil), inputs...),
	}
	// The original dispatch lease is intentionally unavailable after recovery.
	// This synthetic claim is read-only transport for the observer; no Engine
	// callback, checkpoint, usage charge, or external invocation can use it.
	claim := workflowkit.JobClaim{
		JobID: job.ID, ClaimID: "reconciliation:" + stageAttempt.ID, Kind: workflowkit.JobStage,
		Owner: job.CreatedBy, FencingToken: 1, LeaseExpiresAt: time.Now().UTC().Add(time.Minute),
		Execution: execution.Clone(), Stage: &stageClaim,
	}
	return CodeEdgeEvaluatorObservationRequest{
		Execution: workflowkit.StageExecutionRequest{
			Execution: execution,
			Claim:     claim,
			Stage:     stage.Clone(),
			Inputs:    append([]workflowkit.ArtifactBinding(nil), inputs...),
			ReadInput: newStageInputReader(runtime.core.store, runtime.core.objects, run, revision, inputs),
		},
		Resolution: resolution,
	}, nil
}

func validateCodeEdgeObservedEvaluatorArtifacts(stage workflowkit.StageDescriptor, artifacts []StageArtifact) error {
	if err := validateStageArtifactsForPersistence(stage, artifacts, true); err != nil {
		return err
	}
	var bundle []byte
	for _, artifact := range artifacts {
		if artifact.SchemaVersion != codeedge.HarborRunBundleV018Format {
			continue
		}
		if bundle != nil {
			return fmt.Errorf("CodeEdge evaluator observed result has multiple Harbor bundles")
		}
		bundle = artifact.Content
	}
	if len(bundle) == 0 {
		return fmt.Errorf("CodeEdge evaluator observed result has no Harbor bundle")
	}
	inspection, err := codeedge.ParseAndInspectHarborRunBundleV018(bundle)
	if err != nil {
		return fmt.Errorf("inspect observed CodeEdge Harbor bundle: %w", err)
	}
	job := inspection.Job()
	if job.TotalTrials != codeEdgeEvaluatorTrialCount || job.RunningTrials != 0 || job.PendingTrials != 0 || len(inspection.Trials()) != codeEdgeEvaluatorTrialCount {
		return fmt.Errorf("CodeEdge evaluator observed Harbor bundle is not a completed four-trial result")
	}
	return nil
}

func (runtime *FrozenExecutionRuntime) projectObservedCodeEdgeEvaluatorCompletion(ctx context.Context, job store.DurableJob, run store.WorkflowRun, revision store.TaskRevision, frozen frozenRunDefinition, payload frozenStageExecutionPayload, stageAttempt store.StageAttempt, stage workflowkit.StageDescriptor, effect store.SideEffectOperation, inputs []workflowkit.ArtifactBinding, result StageExecutionResult) (store.JobState, error) {
	if err := runtime.startCodeEdgeEvaluatorReconciliation(ctx, run, stageAttempt, stage, effect, job.CreatedBy); err != nil {
		return store.JobFailed, err
	}
	currentStage, err := runtime.core.store.GetStageAttempt(ctx, stageAttempt.ID)
	if err != nil {
		return store.JobFailed, err
	}
	if currentStage == nil || currentStage.RunID != run.ID || currentStage.StageKey != string(stage.Key) {
		return store.JobFailed, fmt.Errorf("%w: CodeEdge evaluator reconciliation stage", ErrFrozenExecutionPayload)
	}
	switch currentStage.ExecutionStatus {
	case store.StageExecutionInDoubt:
		updated, transitionErr := runtime.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
			StageAttemptID: currentStage.ID, ExpectedVersion: currentStage.Version, ExecutionStatus: store.StageExecutionReconciling,
			Actor: job.CreatedBy, Reason: "inspect completed CodeEdge evaluator local evidence",
		})
		if transitionErr != nil {
			return store.JobFailed, transitionErr
		}
		currentStage = &updated
	case store.StageExecutionReconciling:
	case store.StageExecutionCompleted:
		return runtime.resumeObservedCodeEdgeEvaluatorCompletion(ctx, job, run, frozen, payload, stage, *currentStage, effect)
	default:
		return store.JobFailed, fmt.Errorf("%w: CodeEdge evaluator stage %s is %s during observation", ErrFrozenExecutionPayload, currentStage.ID, currentStage.ExecutionStatus)
	}

	node := latestNodeAttempt(ctx, runtime.core.store, currentStage.ID)
	if node.ID == "" {
		return store.JobFailed, fmt.Errorf("%w: CodeEdge evaluator reconciliation has no original node attempt", ErrFrozenExecutionPayload)
	}
	manifest, _, err := persistStageArtifacts(ctx, runtime.core, run, revision, *currentStage, node, stage, append([]workflowkit.ArtifactBinding(nil), inputs...), result.Artifacts, job.CreatedBy, "persist observed completed CodeEdge evaluator artifacts")
	if err != nil {
		return store.JobFailed, err
	}
	if err := runtime.completeTrustedCodeEdgeEvaluatorTrials(ctx, run, *currentStage, job.CreatedBy, "project observed completed CodeEdge evaluator trials"); err != nil {
		return store.JobFailed, err
	}
	if err := runtime.reconcileObservedCodeEdgeEvaluatorQuota(ctx, run, job); err != nil {
		return store.JobFailed, err
	}
	if _, err := runtime.completeReconciledCodeEdgeEvaluatorEffect(ctx, effect, manifest, job.CreatedBy); err != nil {
		return store.JobFailed, err
	}
	if node.Status == store.NodeAttemptInDoubt || node.Status == store.NodeAttemptRunning || node.Status == store.NodeAttemptWaiting {
		if _, err := runtime.core.store.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
			NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: store.NodeAttemptCompleted,
			Actor: job.CreatedBy, Reason: "project observed completed CodeEdge evaluator node",
		}); err != nil {
			return store.JobFailed, err
		}
	} else if node.Status != store.NodeAttemptCompleted {
		return store.JobFailed, fmt.Errorf("%w: CodeEdge evaluator node %s is %s during observation", ErrFrozenExecutionPayload, node.ID, node.Status)
	}
	completed, err := runtime.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: currentStage.ID, ExpectedVersion: currentStage.Version, ExecutionStatus: store.StageExecutionCompleted,
		Verdict: store.VerdictPass, ArtifactManifestID: manifest.ID,
		Actor: job.CreatedBy, Reason: "project observed completed CodeEdge evaluator stage",
	})
	if err != nil {
		return store.JobFailed, err
	}
	currentRun, err := runtime.core.store.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		return store.JobFailed, err
	}
	if currentRun == nil {
		return store.JobFailed, fmt.Errorf("%w: CodeEdge evaluator reconciliation Run", ErrLifecycleNotFound)
	}
	if currentRun.Status == store.WorkflowRunInDoubt {
		updatedRun, transitionErr := runtime.core.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
			RunID: currentRun.ID, ExpectedVersion: currentRun.Version, Status: store.WorkflowRunRunning,
			Actor: job.CreatedBy, Reason: "completed CodeEdge evaluator local evidence reconciled",
		})
		if transitionErr != nil {
			return store.JobFailed, transitionErr
		}
		currentRun = &updatedRun
	}
	if currentRun.Status != store.WorkflowRunRunning {
		return store.JobFailed, fmt.Errorf("%w: CodeEdge evaluator Run %s is %s during observation", ErrFrozenExecutionPayload, currentRun.ID, currentRun.Status)
	}
	if err := runtime.completeCodeEdgeEvaluatorReconciliation(ctx, effect, stage, manifest, job.CreatedBy); err != nil {
		return store.JobFailed, err
	}
	return runtime.afterStageTerminal(ctx, job, *currentRun, frozen, payload, stage, completed, nil, "", nil)
}

func (runtime *FrozenExecutionRuntime) reconcileObservedCodeEdgeEvaluatorQuota(ctx context.Context, run store.WorkflowRun, job store.DurableJob) error {
	actor := job.CreatedBy
	if actor == "" {
		return fmt.Errorf("%w: CodeEdge evaluator reconciliation actor is required", ErrFrozenExecutionPayload)
	}
	// Expire only overdue leases for the two affected authoritative scopes. A
	// lease that is still active is settled directly below; a known completion
	// may reconcile an already uncertain/expired lease without reserving new
	// quota or creating a new admission.
	if _, err := runtime.core.store.ExpireQuotaLeasesForScope(ctx, store.QuotaScopeTask, run.TaskID, actor, "expire stale CodeEdge evaluator quota before observation"); err != nil {
		return err
	}
	if _, err := runtime.core.store.ExpireQuotaLeasesForScope(ctx, store.QuotaScopeActor, actor, actor, "expire stale CodeEdge evaluator quota before observation"); err != nil {
		return err
	}
	decision, err := runtime.core.store.GetDurableAdmissionDecisionByIdempotencyKey(ctx, "stage-admission:"+job.ID)
	if err != nil {
		return err
	}
	if decision == nil || !decision.Accepted || len(decision.Leases) == 0 || decision.TaskID != run.TaskID || decision.Actor != actor {
		return fmt.Errorf("%w: CodeEdge evaluator quota admission is unavailable", ErrFrozenExecutionPayload)
	}
	leases := append([]store.DurableQuotaLease(nil), decision.Leases...)
	sort.Slice(leases, func(left, right int) bool { return leases[left].ID < leases[right].ID })
	for pass := 0; pass <= len(leases); pass++ {
		active := make([]store.SettleQuotaLeaseRequest, 0, len(leases))
		for _, lease := range leases {
			current, err := runtime.core.store.GetDurableQuotaLease(ctx, lease.ID)
			if err != nil || current == nil {
				if err != nil {
					return err
				}
				return fmt.Errorf("%w: quota lease %s", ErrLifecycleNotFound, lease.ID)
			}
			if current.State == store.DurableQuotaLeaseActive {
				active = append(active, store.SettleQuotaLeaseRequest{
					IdempotencyKey: "codeedge-evaluator-observe-settle:" + current.ID,
					LeaseID:        current.ID, Owner: current.Owner, FencingToken: current.FencingToken,
					Outcome: store.QuotaSettlementCompleted, Actor: actor, Reason: "completed CodeEdge evaluator local evidence observed",
				})
			}
		}
		if len(active) == 0 {
			break
		}
		if _, err := runtime.core.store.SettleQuotaLeases(ctx, active); err != nil && !errors.Is(err, store.ErrQuotaLeaseExpired) {
			return err
		}
	}
	reconciliations := make([]store.ReconcileQuotaLeaseRequest, 0, len(leases))
	for _, lease := range leases {
		current, err := runtime.core.store.GetDurableQuotaLease(ctx, lease.ID)
		if err != nil || current == nil {
			if err != nil {
				return err
			}
			return fmt.Errorf("%w: quota lease %s", ErrLifecycleNotFound, lease.ID)
		}
		switch current.State {
		case store.DurableQuotaLeaseUncertain, store.DurableQuotaLeaseExpired:
			reconciliations = append(reconciliations, store.ReconcileQuotaLeaseRequest{
				IdempotencyKey: "codeedge-evaluator-observe-reconcile:" + current.ID,
				LeaseID:        current.ID, Owner: current.Owner, FencingToken: current.FencingToken,
				Outcome: store.QuotaSettlementCompleted, Actor: actor, Reason: "completed CodeEdge evaluator local evidence reconciled",
			})
		case store.DurableQuotaLeaseSettled:
		default:
			return fmt.Errorf("unsupported quota lease state %s", current.State)
		}
	}
	if len(reconciliations) != 0 {
		if _, err := runtime.core.store.ReconcileQuotaLeases(ctx, reconciliations); err != nil {
			return err
		}
	}
	return nil
}

func (runtime *FrozenExecutionRuntime) completeReconciledCodeEdgeEvaluatorEffect(ctx context.Context, effect store.SideEffectOperation, manifest store.ArtifactManifest, actor string) (store.SideEffectOperation, error) {
	if effect.State == store.SideEffectSucceeded {
		if effect.DestinationDigest != manifest.ManifestFingerprint || effect.ReceiptRef != manifest.ID {
			return store.SideEffectOperation{}, fmt.Errorf("%w: reconciled CodeEdge evaluator effect receipt differs from immutable artifacts", ErrFrozenExecutionPayload)
		}
		return effect, nil
	}
	if effect.State != store.SideEffectUnknown {
		return store.SideEffectOperation{}, fmt.Errorf("%w: CodeEdge evaluator effect %s is %s during observation", ErrFrozenExecutionPayload, effect.ID, effect.State)
	}
	return runtime.core.store.TransitionSideEffectOperation(ctx, store.TransitionSideEffectOperationRequest{
		OperationID: effect.ID, ExpectedVersion: effect.Version, State: store.SideEffectSucceeded,
		DestinationDigest: manifest.ManifestFingerprint, ReceiptRef: manifest.ID,
		Actor: actor, Reason: "completed CodeEdge evaluator local evidence observed",
	})
}

func (runtime *FrozenExecutionRuntime) completeCodeEdgeEvaluatorReconciliation(ctx context.Context, effect store.SideEffectOperation, stage workflowkit.StageDescriptor, manifest store.ArtifactManifest, actor string) error {
	attempt, err := runtime.core.store.GetReconciliationAttemptByOperationKey(ctx, codeEdgeEvaluatorReconciliationKey(effect.OperationKey))
	if err != nil {
		return err
	}
	if attempt == nil || attempt.SideEffectOperationID != effect.ID {
		return fmt.Errorf("%w: CodeEdge evaluator reconciliation attempt is unavailable", ErrFrozenExecutionPayload)
	}
	observed, err := json.Marshal(codeEdgeEvaluatorObservedCompletion{
		Format: codeEdgeEvaluatorObservedCompletionFormat, RunID: effect.RunID, StageAttemptID: effect.StageAttemptID, StageKey: stage.Key,
		SideEffectOperation: effect.ID, ArtifactManifestID: manifest.ID,
		ArtifactFingerprint: workflowkit.Fingerprint(manifest.ManifestFingerprint),
	})
	if err != nil {
		return err
	}
	if attempt.State == store.ReconciliationCompleted {
		return nil
	}
	if attempt.State != store.ReconciliationRunning {
		return fmt.Errorf("%w: CodeEdge evaluator reconciliation attempt %s is %s", ErrFrozenExecutionPayload, attempt.ID, attempt.State)
	}
	_, err = runtime.core.store.CompleteReconciliationAttempt(ctx, store.CompleteReconciliationAttemptRequest{
		AttemptID: attempt.ID, ExpectedVersion: attempt.Version, State: store.ReconciliationCompleted,
		ObservedJSON: string(observed), Resolution: "completed from controlled local Harbor evidence",
		Actor: actor, Reason: "complete CodeEdge evaluator local observation reconciliation",
	})
	return err
}

// completedCodeEdgeEvaluatorManifest reconstructs the exact immutable receipt
// already attached to a successful effect. It is used only after both the
// effect and StageAttempt are terminal, so it cannot turn a recovery into a
// new provider execution or an alternate evidence projection.
func (runtime *FrozenExecutionRuntime) completedCodeEdgeEvaluatorManifest(ctx context.Context, run store.WorkflowRun, stageAttempt store.StageAttempt, stage workflowkit.StageDescriptor, effect store.SideEffectOperation) (store.ArtifactManifest, error) {
	if effect.State != store.SideEffectSucceeded || stageAttempt.ExecutionStatus != store.StageExecutionCompleted || stageAttempt.ArtifactManifestID == "" || effect.ReceiptRef != stageAttempt.ArtifactManifestID || effect.DestinationDigest == "" {
		return store.ArtifactManifest{}, fmt.Errorf("%w: CodeEdge evaluator completed receipt is inconsistent", ErrFrozenExecutionPayload)
	}
	index, err := loadStageArtifactManifestIndex(ctx, runtime.core.store, stageAttempt.ArtifactManifestID)
	if err != nil {
		return store.ArtifactManifest{}, err
	}
	if index.manifest.ID != effect.ReceiptRef || index.manifest.ManifestFingerprint != effect.DestinationDigest || index.manifest.SubjectRevisionID != run.RevisionID || index.manifest.WorkflowFingerprint != run.DefinitionHash || index.payload.RunID != run.ID || index.payload.StageAttemptID != stageAttempt.ID || index.payload.StageKey != stage.Key {
		return store.ArtifactManifest{}, fmt.Errorf("%w: CodeEdge evaluator completed receipt does not match immutable stage lineage", ErrFrozenExecutionPayload)
	}
	revision, err := runtime.core.store.GetTaskRevision(ctx, run.RevisionID)
	if err != nil {
		return store.ArtifactManifest{}, err
	}
	if revision == nil || revision.TaskID != run.TaskID || index.manifest.SubjectDigest != revision.TaskDigest {
		return store.ArtifactManifest{}, fmt.Errorf("%w: CodeEdge evaluator completed receipt revision lineage", ErrFrozenExecutionPayload)
	}
	if len(index.artifacts) != len(stage.Outputs) {
		return store.ArtifactManifest{}, fmt.Errorf("%w: CodeEdge evaluator completed receipt artifact count", ErrFrozenExecutionPayload)
	}
	for _, output := range stage.Outputs {
		artifact, found := index.artifacts[output.Name]
		if !found || artifact.SchemaVersion != output.SchemaVersion {
			return store.ArtifactManifest{}, fmt.Errorf("%w: CodeEdge evaluator completed receipt output %q", ErrFrozenExecutionPayload, output.Name)
		}
	}
	return index.manifest, nil
}

func (runtime *FrozenExecutionRuntime) resumeObservedCodeEdgeEvaluatorCompletion(ctx context.Context, job store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, payload frozenStageExecutionPayload, stage workflowkit.StageDescriptor, stageAttempt store.StageAttempt, effect store.SideEffectOperation) (store.JobState, error) {
	return runtime.resumeCommittedCodeEdgeEvaluatorCompletionWithReconciliation(ctx, job, run, frozen, payload, stage, stageAttempt, effect, true)
}

// resumeCommittedCodeEdgeEvaluatorCompletion restores a direct evaluator
// success after the source worker loses its lease. Unlike an observer delivery,
// the direct path has no reconciliation attempt to complete.
func (runtime *FrozenExecutionRuntime) resumeCommittedCodeEdgeEvaluatorCompletion(ctx context.Context, job store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, payload frozenStageExecutionPayload, stage workflowkit.StageDescriptor, stageAttempt store.StageAttempt, effect store.SideEffectOperation) (store.JobState, error) {
	return runtime.resumeCommittedCodeEdgeEvaluatorCompletionWithReconciliation(ctx, job, run, frozen, payload, stage, stageAttempt, effect, false)
}

func (runtime *FrozenExecutionRuntime) resumeCommittedCodeEdgeEvaluatorCompletionWithReconciliation(ctx context.Context, job store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, payload frozenStageExecutionPayload, stage workflowkit.StageDescriptor, stageAttempt store.StageAttempt, effect store.SideEffectOperation, completeReconciliation bool) (store.JobState, error) {
	if effect.State != store.SideEffectSucceeded || stageAttempt.ExecutionStatus != store.StageExecutionCompleted {
		return store.JobFailed, fmt.Errorf("%w: CodeEdge evaluator completion resumption has inconsistent durable state", ErrFrozenExecutionPayload)
	}
	manifest, err := runtime.completedCodeEdgeEvaluatorManifest(ctx, run, stageAttempt, stage, effect)
	if err != nil {
		return store.JobFailed, err
	}
	if err := runtime.completeTrustedCodeEdgeEvaluatorTrials(ctx, run, stageAttempt, job.CreatedBy, "restore completed CodeEdge evaluator logical trials"); err != nil {
		return store.JobFailed, err
	}
	if completeReconciliation {
		if err := runtime.completeCodeEdgeEvaluatorReconciliation(ctx, effect, stage, manifest, job.CreatedBy); err != nil {
			return store.JobFailed, err
		}
	}
	currentRun, err := runtime.core.store.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		return store.JobFailed, err
	}
	if currentRun == nil {
		return store.JobFailed, fmt.Errorf("%w: CodeEdge evaluator completion Run", ErrLifecycleNotFound)
	}
	if currentRun.Status == store.WorkflowRunInDoubt {
		updated, transitionErr := runtime.core.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
			RunID: currentRun.ID, ExpectedVersion: currentRun.Version, Status: store.WorkflowRunRunning,
			Actor: job.CreatedBy, Reason: "restore completed CodeEdge evaluator observation",
		})
		if transitionErr != nil {
			return store.JobFailed, transitionErr
		}
		currentRun = &updated
	}
	if currentRun.Status != store.WorkflowRunRunning {
		return store.JobFailed, fmt.Errorf("%w: CodeEdge evaluator completion Run %s is %s", ErrFrozenExecutionPayload, currentRun.ID, currentRun.Status)
	}
	return runtime.afterStageTerminal(ctx, job, *currentRun, frozen, payload, stage, stageAttempt, nil, "", nil)
}
