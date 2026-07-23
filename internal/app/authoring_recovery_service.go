package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

var (
	// ErrAuthoringRecoveryUnavailable marks a Run that cannot safely resume its
	// frozen pre-materialization authoring workflow.
	ErrAuthoringRecoveryUnavailable = errors.New("authoring recovery: run is not recoverable")
	// ErrAuthoringRecoveryNotFound distinguishes a missing recovery plan or
	// command from a stale or ineligible authoring Run.
	ErrAuthoringRecoveryNotFound = errors.New("authoring recovery: record not found")
)

const (
	authoringRecoveryCommandFormatV1 = "harbor.authoring-recovery-command.v1"
	authoringRecoveryCommandFormat   = "harbor.authoring-recovery-command.v2"
)

// AuthoringRecoveryService resumes a failed Standard authoring Run with its
// original frozen workflow definition. It is deliberately separate from
// TaskContinuationService because an authoring-session has no TaskRevision
// until materialize_task succeeds.
type AuthoringRecoveryService struct {
	core     *lifecycleServiceCore
	observer continuationSubjectStateObserver
}

func newAuthoringRecoveryService(core *lifecycleServiceCore) *AuthoringRecoveryService {
	return &AuthoringRecoveryService{
		core:     core,
		observer: storeContinuationStateObserver{dataStore: core.store, objects: core.objects},
	}
}

// AuthoringRecoveryCommand is the caller's idempotent request to resume one
// immutable source/session authoring Run. Expected must come from
// CurrentCheckpoint; the service derives the failed StageAttempt from durable
// state instead of trusting a caller-selected restart point.
type AuthoringRecoveryCommand struct {
	CommandKey string                    `json:"command_key"`
	RunID      string                    `json:"run_id"`
	Expected   workflowkit.CheckpointRef `json:"expected"`
	Actor      string                    `json:"actor"`
	Reason     string                    `json:"reason"`
}

type normalizedAuthoringRecoveryCommand struct {
	Format                 string                    `json:"format"`
	CommandKey             string                    `json:"command_key"`
	RunID                  string                    `json:"run_id"`
	TargetTaskID           string                    `json:"target_task_id"`
	TargetTaskVersion      int64                     `json:"target_task_version"`
	Expected               workflowkit.CheckpointRef `json:"expected"`
	FailureStageAttemptIDs []string                  `json:"failure_stage_attempt_ids"`
	TargetNodeIDs          []workflowkit.NodeID      `json:"target_node_ids"`
}

// authoringRecoveryCommandV1 is read-only compatibility for commands written
// before the checkpoint became the single source of source/session identity.
// New commands always use normalizedAuthoringRecoveryCommand (v2).
type authoringRecoveryCommandV1 struct {
	Format                 string                    `json:"format"`
	CommandKey             string                    `json:"command_key"`
	RunID                  string                    `json:"run_id"`
	AuthoringSourceID      string                    `json:"authoring_source_id"`
	AuthoringSessionID     string                    `json:"authoring_session_id"`
	TargetTaskID           string                    `json:"target_task_id"`
	TargetTaskVersion      int64                     `json:"target_task_version"`
	SourceDigest           workflowkit.SubjectDigest `json:"source_digest"`
	DefinitionFingerprint  workflowkit.Fingerprint   `json:"definition_fingerprint"`
	Expected               workflowkit.CheckpointRef `json:"expected"`
	FailureStageAttemptIDs []string                  `json:"failure_stage_attempt_ids"`
	TargetNodeIDs          []workflowkit.NodeID      `json:"target_node_ids"`
}

type authoringRecoveryBinding struct {
	run     store.WorkflowRun
	subject workflowRunSubject
	frozen  frozenRunDefinition
}

// authoringRecoveryAssessment keeps the only durable observation used to
// derive retry targets. A command is first prepared from one assessment, then
// assessed again after persistence before its plan can be frozen.
type authoringRecoveryAssessment struct {
	binding                 authoringRecoveryBinding
	state                   continuationRunState
	targetNodeIDs           []workflowkit.NodeID
	failureStageAttemptIDs  []string
	requiredScheduledInputs map[workflowkit.NodeID][]workflowkit.ArtifactBinding
}

type authoringRecoverySelection struct {
	targetNodeIDs          []workflowkit.NodeID
	failureStageAttemptIDs []string
	feedback               []authoringRecoveryFeedback
}

type authoringRecoveryFeedback struct {
	artifactName    string
	reviewKind      string
	attemptID       string
	consumerNodeIDs []workflowkit.NodeID
}

// authoringRecoveryInfraFailureOverride is deliberately narrower than a
// generic retry-policy override. It exists only for recovery facts that are
// independently durable and can be revalidated at preview and execution-plan
// time.
type authoringRecoveryInfraFailureOverride func(workflowkit.StageDescriptor, store.StageAttempt) (bool, error)

// CurrentCheckpoint returns the source/session checkpoint required to resume
// this exact frozen authoring Run. The AuthoringSession is immutable, so its
// stable subject version is zero rather than the mutable draft Task version.
func (service *AuthoringRecoveryService) CurrentCheckpoint(ctx context.Context, runID string) (workflowkit.CheckpointRef, error) {
	binding, err := service.loadBinding(ctx, runID)
	if err != nil {
		return workflowkit.CheckpointRef{}, err
	}
	return authoringRecoveryCheckpoint(binding.run, binding.subject), nil
}

// CanRecover exposes the durable eligibility decision used by UI and CLI
// adapters. It performs no mutation and intentionally reports a reason rather
// than making adapters reimplement source/session or materialization rules.
func (service *AuthoringRecoveryService) CanRecover(ctx context.Context, runID string) (bool, string, error) {
	binding, err := service.loadBinding(ctx, runID)
	if err != nil {
		return false, "", err
	}
	if _, err := service.assessRecoverableBinding(ctx, binding); err != nil {
		if errors.Is(err, ErrAuthoringRecoveryUnavailable) || errors.Is(err, store.ErrContinuationReconciliationRequired) {
			return false, err.Error(), nil
		}
		return false, "", err
	}
	return true, "", nil
}

// PreviewAuthoringRecovery computes a fresh, non-durable recovery plan. The
// preview has ephemeral IDs and must be planned again before execution.
func (service *AuthoringRecoveryService) PreviewAuthoringRecovery(ctx context.Context, command AuthoringRecoveryCommand) (workflowkit.ContinuationPlan, error) {
	normalized, err := service.normalizeCommand(command)
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	commandID, err := store.NewUUIDv7()
	if err != nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("allocate authoring recovery preview command ID: %w", err)
	}
	prepared, assessment, err := service.prepareCommand(ctx, normalized)
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	return service.buildPlan(prepared, commandID, assessment)
}

// PlanAuthoringRecovery stores one immutable recovery command and freezes a
// same-run continuation plan. A replayed command key returns the original
// plan rather than interpreting a later failure state.
func (service *AuthoringRecoveryService) PlanAuthoringRecovery(ctx context.Context, command AuthoringRecoveryCommand) (workflowkit.ContinuationPlan, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("authoring recovery service is not configured")
	}
	normalized, err := service.normalizeCommand(command)
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}

	// Resume an incomplete durable command before observing mutable state. The
	// persisted payload contains the original failure-attempt checkpoint.
	if existing, err := service.core.store.GetContinuationCommandByKey(ctx, normalized.CommandKey); err != nil {
		return workflowkit.ContinuationPlan{}, err
	} else if existing != nil {
		persisted, err := decodeAuthoringRecoveryCommand(*existing)
		if err != nil {
			return workflowkit.ContinuationPlan{}, err
		}
		if err := matchAuthoringRecoveryIntent(normalized, persisted, *existing, command); err != nil {
			return workflowkit.ContinuationPlan{}, err
		}
		if plan, err := service.core.store.GetFrozenPlanByCommand(ctx, existing.ID); err != nil {
			return workflowkit.ContinuationPlan{}, err
		} else if plan != nil {
			return decodeFrozenContinuationPlan(ctx, service.core, *plan)
		}
		return service.compilePersistedPlan(ctx, *existing)
	}

	prepared, _, err := service.prepareCommand(ctx, normalized)
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	encoded, err := json.Marshal(prepared)
	if err != nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("encode authoring recovery command: %w", err)
	}
	record, err := service.core.store.CreateContinuationCommand(ctx, store.CreateContinuationCommandRequest{
		CommandKey: prepared.CommandKey, SubjectID: prepared.Expected.SubjectID, RunID: prepared.RunID,
		PayloadJSON: string(encoded), Actor: command.Actor, Reason: command.Reason,
	})
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	if plan, err := service.core.store.GetFrozenPlanByCommand(ctx, record.ID); err != nil {
		return workflowkit.ContinuationPlan{}, err
	} else if plan != nil {
		return decodeFrozenContinuationPlan(ctx, service.core, *plan)
	}
	return service.compilePersistedPlan(ctx, record)
}

// ExecuteAuthoringRecovery queues the previously frozen recovery plan. The
// Store rechecks the source/session checkpoint and pre-materialization barrier
// in the same transaction that advances execution_epoch and creates the job.
func (service *AuthoringRecoveryService) ExecuteAuthoringRecovery(ctx context.Context, planID string) (store.ContinuationExecution, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return store.ContinuationExecution{}, fmt.Errorf("authoring recovery service is not configured")
	}
	plan, err := getFrozenContinuationPlan(ctx, service.core, planID)
	if err != nil {
		return store.ContinuationExecution{}, err
	}
	snapshot := plan.Snapshot()
	command, err := service.core.store.GetContinuationCommand(ctx, snapshot.CommandID)
	if err != nil {
		return store.ContinuationExecution{}, err
	}
	if command == nil {
		return store.ContinuationExecution{}, fmt.Errorf("%w: command %s", ErrAuthoringRecoveryNotFound, snapshot.CommandID)
	}
	persisted, err := decodeAuthoringRecoveryCommand(*command)
	if err != nil {
		return store.ContinuationExecution{}, err
	}
	if snapshot.SourceRunID != persisted.RunID || snapshot.BaseCheckpoint != persisted.Expected ||
		snapshot.SubjectRevisionID != persisted.Expected.SubjectRevisionID || snapshot.SubjectDigest != persisted.Expected.SubjectDigest {
		return store.ContinuationExecution{}, fmt.Errorf("%w: frozen plan does not match authoring recovery command", store.ErrImmutable)
	}

	executionKey := "authoring-recovery-execution:" + snapshot.PlanID
	if existing, err := service.core.store.GetContinuationExecutionByIdempotency(ctx, executionKey); err != nil {
		return store.ContinuationExecution{}, err
	} else if existing != nil {
		if existing.PlanID != snapshot.PlanID || existing.RunID != snapshot.SourceRunID {
			return store.ContinuationExecution{}, fmt.Errorf("%w: authoring recovery execution key %s", store.ErrIdempotencyConflict, executionKey)
		}
		return *existing, nil
	}
	if plan.IsExpired(service.core.now().UTC()) {
		return store.ContinuationExecution{}, fmt.Errorf("%w: %s", store.ErrContinuationPlanExpired, planID)
	}
	binding, err := service.loadBinding(ctx, snapshot.SourceRunID)
	if err != nil {
		return store.ContinuationExecution{}, err
	}
	if err := service.ensureRecoverableBinding(ctx, binding); err != nil {
		return store.ContinuationExecution{}, err
	}
	payload, err := json.Marshal(continuationExecutionPayload{
		Format: continuationExecutionFormat, PlanID: snapshot.PlanID, CommandID: snapshot.CommandID,
		PlanFingerprint: plan.Fingerprint(), RunID: snapshot.SourceRunID, SourceRunID: snapshot.SourceRunID,
		QuotaPolicy: binding.frozen.QuotaPolicy.Clone(),
	})
	if err != nil {
		return store.ContinuationExecution{}, fmt.Errorf("encode authoring recovery execution: %w", err)
	}
	if err := matchAuthoringRecoveryCommandBinding(persisted, binding); err != nil {
		return store.ContinuationExecution{}, err
	}
	runtimePlan, err := continuationRuntimeExecutionPlan(plan, binding.frozen.Workflow, binding.frozen.QuotaPolicy, "")
	if err != nil {
		return store.ContinuationExecution{}, err
	}
	// The plan may outlive an object-store entry. Re-prove every binding frozen
	// specifically for this continuation immediately before advancing the Run
	// epoch and publishing its durable execution job.
	if err := validateRequiredContinuationInputs(ctx, service.core, binding.run, runtimePlan); err != nil {
		return store.ContinuationExecution{}, fmt.Errorf("%w: frozen recovery inputs changed after planning: %v", ErrAuthoringRecoveryUnavailable, err)
	}
	commit, err := service.core.store.CommitAuthoringRecoveryExecution(ctx, store.CommitAuthoringRecoveryExecutionRequest{
		CommitContinuationExecutionRequest: store.CommitContinuationExecutionRequest{
			PlanID: snapshot.PlanID, RunID: snapshot.SourceRunID, IdempotencyKey: executionKey,
			PayloadJSON: string(payload), Expected: storeCheckpoint(snapshot.BaseCheckpoint), Actor: command.Actor, Reason: command.Reason,
		},
		AuthoringSourceID: persisted.Expected.SubjectID, AuthoringSessionID: persisted.Expected.SubjectRevisionID,
		TargetTaskID: persisted.TargetTaskID, ExpectedTargetTaskVersion: persisted.TargetTaskVersion,
	})
	if err != nil {
		return store.ContinuationExecution{}, err
	}
	return commit.Execution, nil
}

func (service *AuthoringRecoveryService) normalizeCommand(command AuthoringRecoveryCommand) (normalizedAuthoringRecoveryCommand, error) {
	if service == nil || service.core == nil {
		return normalizedAuthoringRecoveryCommand{}, fmt.Errorf("authoring recovery service is not configured")
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(command.CommandKey)); err != nil {
		return normalizedAuthoringRecoveryCommand{}, fmt.Errorf("authoring recovery command key: %w", err)
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(command.RunID)); err != nil {
		return normalizedAuthoringRecoveryCommand{}, err
	}
	if strings.TrimSpace(command.Actor) == "" || strings.TrimSpace(command.Reason) == "" {
		return normalizedAuthoringRecoveryCommand{}, fmt.Errorf("authoring recovery actor and reason are required")
	}
	if err := validateAuthoringRecoveryCheckpoint(command.Expected); err != nil {
		return normalizedAuthoringRecoveryCommand{}, err
	}
	return normalizedAuthoringRecoveryCommand{
		Format: authoringRecoveryCommandFormat, CommandKey: strings.TrimSpace(command.CommandKey), RunID: strings.TrimSpace(command.RunID), Expected: command.Expected,
	}, nil
}

func (service *AuthoringRecoveryService) prepareCommand(ctx context.Context, command normalizedAuthoringRecoveryCommand) (normalizedAuthoringRecoveryCommand, authoringRecoveryAssessment, error) {
	binding, err := service.loadBinding(ctx, command.RunID)
	if err != nil {
		return normalizedAuthoringRecoveryCommand{}, authoringRecoveryAssessment{}, err
	}
	if err := matchAuthoringRecoveryCheckpoint(command.Expected, binding.run, binding.subject); err != nil {
		return normalizedAuthoringRecoveryCommand{}, authoringRecoveryAssessment{}, err
	}
	assessment, err := service.assessRecoverableBinding(ctx, binding)
	if err != nil {
		return normalizedAuthoringRecoveryCommand{}, authoringRecoveryAssessment{}, err
	}
	command.TargetTaskID = assessment.binding.subject.TargetTask.ID
	command.TargetTaskVersion = assessment.binding.subject.TargetTask.Version
	command.TargetNodeIDs = append([]workflowkit.NodeID(nil), assessment.targetNodeIDs...)
	command.FailureStageAttemptIDs = append([]string(nil), assessment.failureStageAttemptIDs...)
	return command, assessment, nil
}

func (service *AuthoringRecoveryService) assessRecoverableBinding(ctx context.Context, binding authoringRecoveryBinding) (authoringRecoveryAssessment, error) {
	if err := service.ensureRecoverableBinding(ctx, binding); err != nil {
		return authoringRecoveryAssessment{}, err
	}
	state, err := service.observer.ObserveSubject(ctx, binding.run, binding.subject, binding.frozen.Workflow)
	if err != nil {
		return authoringRecoveryAssessment{}, err
	}
	if state.InDoubt {
		return authoringRecoveryAssessment{}, fmt.Errorf("%w: workflow run %s has unresolved stage or node evidence", store.ErrContinuationReconciliationRequired, binding.run.ID)
	}
	selection, err := authoringRecoveryTargetsWithInfraFailureOverride(binding.run, binding.frozen.Workflow, state,
		func(stage workflowkit.StageDescriptor, failed store.StageAttempt) (bool, error) {
			return service.ownerGrantedQuotaAdmissionRecoveryAllowed(ctx, binding, stage, failed)
		})
	if err != nil {
		return authoringRecoveryAssessment{}, err
	}
	requiredScheduledInputs, err := service.authoringRecoveryRequiredScheduledInputs(ctx, binding, selection)
	if err != nil {
		return authoringRecoveryAssessment{}, err
	}
	return authoringRecoveryAssessment{
		binding:                 binding,
		state:                   state,
		targetNodeIDs:           selection.targetNodeIDs,
		failureStageAttemptIDs:  selection.failureStageAttemptIDs,
		requiredScheduledInputs: requiredScheduledInputs,
	}, nil
}

func (service *AuthoringRecoveryService) buildPlan(command normalizedAuthoringRecoveryCommand, commandID string, assessment authoringRecoveryAssessment) (workflowkit.ContinuationPlan, error) {
	invalidation, err := workflowkit.PlanInvalidation(assessment.binding.frozen.Workflow, workflowkit.InvalidationRequest{
		RecomputeNodes: command.TargetNodeIDs, ReuseStates: assessment.state.ReuseStates, Matcher: workflowadapter.HarborResourceMatch,
	})
	if err != nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("plan authoring recovery invalidation: %w", err)
	}
	planID, err := store.NewUUIDv7()
	if err != nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("allocate authoring recovery plan ID: %w", err)
	}
	snapshot, err := buildAuthoringRecoveryPlan(planID, commandID, command, assessment.binding.run, assessment.binding.subject, assessment.binding.frozen.Workflow, assessment.state, invalidation, assessment.requiredScheduledInputs, service.core.now().UTC().Add(assessment.binding.frozen.ContinuationPlanTTL))
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	plan, err := workflowkit.FreezeContinuationPlan(snapshot, assessment.binding.frozen.Workflow)
	if err != nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("freeze authoring recovery plan: %w", err)
	}
	return plan, nil
}

func (service *AuthoringRecoveryService) compilePersistedPlan(ctx context.Context, record store.ContinuationCommand) (workflowkit.ContinuationPlan, error) {
	command, err := decodeAuthoringRecoveryCommand(record)
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	binding, err := service.loadBinding(ctx, command.RunID)
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	if err := matchAuthoringRecoveryCommandBinding(command, binding); err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	// A command may outlive the coordinator that created it. Re-observe after
	// the durable write so target attempts cannot drift before its plan freezes.
	assessment, err := service.assessRecoverableBinding(ctx, binding)
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	if err := validatePersistedAuthoringRecoveryTargets(command, assessment); err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	plan, err := service.buildPlan(command, record.ID, assessment)
	if err != nil {
		return workflowkit.ContinuationPlan{}, err
	}
	encoded, err := json.Marshal(plan.Snapshot())
	if err != nil {
		return workflowkit.ContinuationPlan{}, fmt.Errorf("encode frozen authoring recovery plan: %w", err)
	}
	stored, err := service.core.store.CreateFrozenPlan(ctx, store.CreateFrozenPlanRequest{
		ID: plan.ID(), CommandID: record.ID, SubjectID: assessment.binding.subject.AuthoringSource.ID,
		SubjectRevisionID: assessment.binding.subject.AuthoringSession.ID, SubjectDigest: assessment.binding.subject.subjectDigest(),
		WorkflowFingerprint: assessment.binding.run.DefinitionHash, PlanFingerprint: string(plan.Fingerprint()), PayloadJSON: string(encoded),
		ExpiresAt: plan.Snapshot().ExpiresAt, Actor: record.Actor, Reason: record.Reason,
	})
	if err != nil {
		if errors.Is(err, store.ErrIdempotencyConflict) {
			if existing, lookupErr := service.core.store.GetFrozenPlanByCommand(ctx, record.ID); lookupErr == nil && existing != nil {
				return decodeFrozenContinuationPlan(ctx, service.core, *existing)
			}
		}
		return workflowkit.ContinuationPlan{}, err
	}
	return decodeFrozenContinuationPlan(ctx, service.core, stored)
}

func (service *AuthoringRecoveryService) loadBinding(ctx context.Context, runID string) (authoringRecoveryBinding, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return authoringRecoveryBinding{}, fmt.Errorf("authoring recovery service is not configured")
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(runID)); err != nil {
		return authoringRecoveryBinding{}, err
	}
	run, err := service.core.store.GetWorkflowRun(ctx, strings.TrimSpace(runID))
	if err != nil {
		return authoringRecoveryBinding{}, err
	}
	if run == nil {
		return authoringRecoveryBinding{}, fmt.Errorf("%w: run %s", ErrLifecycleNotFound, runID)
	}
	if run.SubjectKind != store.WorkflowRunSubjectAuthoringSession || !isCurrentStandardAuthoringRun(*run) {
		return authoringRecoveryBinding{}, fmt.Errorf("%w: workflow run %s is not a Standard authoring Run", ErrAuthoringRecoveryUnavailable, run.ID)
	}
	subject, err := service.core.resolveWorkflowRunSubject(ctx, *run)
	if err != nil {
		return authoringRecoveryBinding{}, err
	}
	if !subject.isAuthoringSession() {
		return authoringRecoveryBinding{}, fmt.Errorf("%w: workflow run %s has no authoring source/session subject", ErrAuthoringRecoveryUnavailable, run.ID)
	}
	frozen, err := decodeFrozenRunDefinition(*run)
	if err != nil {
		return authoringRecoveryBinding{}, err
	}
	return authoringRecoveryBinding{run: *run, subject: subject, frozen: frozen}, nil
}

func (service *AuthoringRecoveryService) ensureRecoverableBinding(ctx context.Context, binding authoringRecoveryBinding) error {
	if binding.run.Status == store.WorkflowRunInDoubt {
		return fmt.Errorf("%w: workflow run %s", store.ErrContinuationReconciliationRequired, binding.run.ID)
	}
	switch binding.run.Status {
	case store.WorkflowRunFailedRecoverable, store.WorkflowRunPaused:
	case store.WorkflowRunWaitingContinuation:
		if !isCurrentStandardAuthoringRun(binding.run) {
			return fmt.Errorf("%w: workflow run %s is %s; legacy authoring admission failures require an explicit new task revision", ErrAuthoringRecoveryUnavailable, binding.run.ID, binding.run.Status)
		}
	default:
		return fmt.Errorf("%w: workflow run %s is %s", ErrAuthoringRecoveryUnavailable, binding.run.ID, binding.run.Status)
	}
	if _, _, err := service.core.verifyRunManagedExecutionInputs(ctx, binding.run); err != nil {
		return fmt.Errorf("%w: verify frozen managed execution inputs: %v", ErrAuthoringRecoveryUnavailable, err)
	}
	if err := service.core.verifyRunDeploymentCatalogReceipt(binding.run); err != nil {
		return fmt.Errorf("%w: verify frozen deployment catalog/lock: %v", ErrAuthoringRecoveryUnavailable, err)
	}
	active, err := service.core.store.HasActiveContinuationExecutionForRun(ctx, binding.run.ID)
	if err != nil {
		return err
	}
	if active {
		return fmt.Errorf("%w: workflow run %s already has an active recovery", ErrAuthoringRecoveryUnavailable, binding.run.ID)
	}
	materialization, err := service.core.store.GetAuthoringTaskMaterializationForRun(ctx, binding.run.ID)
	if err != nil {
		return err
	}
	if materialization != nil {
		return fmt.Errorf("%w: workflow run %s already materialized task revision %s", ErrAuthoringRecoveryUnavailable, binding.run.ID, materialization.RevisionID)
	}
	handoff, err := service.core.store.GetAuthoringPhase1HandoffForAuthoringRun(ctx, binding.run.ID)
	if err != nil {
		return err
	}
	if handoff != nil {
		return fmt.Errorf("%w: workflow run %s already has Phase-1 handoff %s", ErrAuthoringRecoveryUnavailable, binding.run.ID, handoff.ID)
	}
	if binding.subject.TargetTask == nil || binding.subject.TargetTask.CurrentRevisionID != "" || binding.subject.TargetTask.LifecycleState != store.TaskLifecycleDraft {
		return fmt.Errorf("%w: authoring target task is no longer an unmaterialized draft", ErrAuthoringRecoveryUnavailable)
	}
	return nil
}

func authoringRecoveryCheckpoint(run store.WorkflowRun, subject workflowRunSubject) workflowkit.CheckpointRef {
	return workflowkit.CheckpointRef{
		Sequence: uint64(run.Version), ExecutionEpoch: run.ExecutionEpoch, SubjectVersion: store.AuthoringSessionControlSubjectVersion,
		SubjectID: subject.AuthoringSource.ID, SubjectRevisionID: subject.AuthoringSession.ID, SubjectDigest: subject.Binding.Digest,
		WorkflowFingerprint: workflowkit.Fingerprint(run.DefinitionHash),
	}
}

func validateAuthoringRecoveryCheckpoint(checkpoint workflowkit.CheckpointRef) error {
	if checkpoint.Sequence == 0 || checkpoint.ExecutionEpoch < 0 || checkpoint.SubjectVersion != store.AuthoringSessionControlSubjectVersion {
		return fmt.Errorf("invalid authoring recovery checkpoint versions")
	}
	if err := store.ValidateUUIDv7(checkpoint.SubjectID); err != nil {
		return err
	}
	if err := store.ValidateUUIDv7(checkpoint.SubjectRevisionID); err != nil {
		return err
	}
	if err := checkpoint.SubjectDigest.Validate(); err != nil {
		return err
	}
	return checkpoint.WorkflowFingerprint.Validate()
}

func matchAuthoringRecoveryCheckpoint(expected workflowkit.CheckpointRef, run store.WorkflowRun, subject workflowRunSubject) error {
	current := authoringRecoveryCheckpoint(run, subject)
	if expected != current {
		return fmt.Errorf("%w: authoring recovery checkpoint is stale", store.ErrOptimisticLock)
	}
	return nil
}

func matchAuthoringRecoveryCommandBinding(command normalizedAuthoringRecoveryCommand, binding authoringRecoveryBinding) error {
	if command.Format != authoringRecoveryCommandFormat || command.RunID != binding.run.ID || binding.subject.TargetTask == nil ||
		command.TargetTaskID != binding.subject.TargetTask.ID || command.TargetTaskVersion != binding.subject.TargetTask.Version {
		return fmt.Errorf("%w: authoring recovery command does not match frozen source/session binding", store.ErrImmutable)
	}
	return matchAuthoringRecoveryCheckpoint(command.Expected, binding.run, binding.subject)
}

func decodeAuthoringRecoveryCommand(record store.ContinuationCommand) (normalizedAuthoringRecoveryCommand, error) {
	var envelope struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal([]byte(record.PayloadJSON), &envelope); err != nil {
		return normalizedAuthoringRecoveryCommand{}, fmt.Errorf("decode authoring recovery command %s format: %w", record.ID, err)
	}
	switch envelope.Format {
	case authoringRecoveryCommandFormat:
		var command normalizedAuthoringRecoveryCommand
		if err := decodeStrictJSON(record.PayloadJSON, &command); err != nil {
			return normalizedAuthoringRecoveryCommand{}, fmt.Errorf("decode authoring recovery command %s: %w", record.ID, err)
		}
		return validateNormalizedAuthoringRecoveryCommand(command, record)
	case authoringRecoveryCommandFormatV1:
		var legacy authoringRecoveryCommandV1
		if err := decodeStrictJSON(record.PayloadJSON, &legacy); err != nil {
			return normalizedAuthoringRecoveryCommand{}, fmt.Errorf("decode legacy authoring recovery command %s: %w", record.ID, err)
		}
		return normalizeAuthoringRecoveryCommandV1(legacy, record)
	default:
		return normalizedAuthoringRecoveryCommand{}, fmt.Errorf("%w: unsupported authoring recovery command format %q", store.ErrImmutable, envelope.Format)
	}
}

func validateNormalizedAuthoringRecoveryCommand(command normalizedAuthoringRecoveryCommand, record store.ContinuationCommand) (normalizedAuthoringRecoveryCommand, error) {
	if command.Format != authoringRecoveryCommandFormat || command.CommandKey != record.CommandKey || command.RunID != record.RunID ||
		command.Expected.SubjectID != record.SubjectID {
		return normalizedAuthoringRecoveryCommand{}, fmt.Errorf("%w: authoring recovery command %s has inconsistent immutable fields", store.ErrImmutable, record.ID)
	}
	if err := validateAuthoringRecoveryCheckpoint(command.Expected); err != nil {
		return normalizedAuthoringRecoveryCommand{}, err
	}
	if err := store.ValidateUUIDv7(command.TargetTaskID); err != nil {
		return normalizedAuthoringRecoveryCommand{}, err
	}
	if command.TargetTaskVersion <= 0 {
		return normalizedAuthoringRecoveryCommand{}, fmt.Errorf("authoring recovery command target task version must be positive")
	}
	return command, nil
}

func normalizeAuthoringRecoveryCommandV1(legacy authoringRecoveryCommandV1, record store.ContinuationCommand) (normalizedAuthoringRecoveryCommand, error) {
	if legacy.Format != authoringRecoveryCommandFormatV1 || legacy.AuthoringSourceID != legacy.Expected.SubjectID ||
		legacy.AuthoringSessionID != legacy.Expected.SubjectRevisionID || legacy.SourceDigest != legacy.Expected.SubjectDigest ||
		legacy.DefinitionFingerprint != legacy.Expected.WorkflowFingerprint || legacy.AuthoringSourceID != record.SubjectID {
		return normalizedAuthoringRecoveryCommand{}, fmt.Errorf("%w: legacy authoring recovery command %s has inconsistent source/session fields", store.ErrImmutable, record.ID)
	}
	return validateNormalizedAuthoringRecoveryCommand(normalizedAuthoringRecoveryCommand{
		Format:                 authoringRecoveryCommandFormat,
		CommandKey:             legacy.CommandKey,
		RunID:                  legacy.RunID,
		TargetTaskID:           legacy.TargetTaskID,
		TargetTaskVersion:      legacy.TargetTaskVersion,
		Expected:               legacy.Expected,
		FailureStageAttemptIDs: append([]string(nil), legacy.FailureStageAttemptIDs...),
		TargetNodeIDs:          append([]workflowkit.NodeID(nil), legacy.TargetNodeIDs...),
	}, record)
}

func matchAuthoringRecoveryIntent(request normalizedAuthoringRecoveryCommand, persisted normalizedAuthoringRecoveryCommand, record store.ContinuationCommand, original AuthoringRecoveryCommand) error {
	// A client may have lost the response after Execute advanced the Run epoch.
	// Replaying its command key must return the existing frozen intent; only a
	// new command key is evaluated against a supplied checkpoint.
	if request.CommandKey != persisted.CommandKey || request.RunID != persisted.RunID ||
		record.Actor != strings.TrimSpace(original.Actor) || record.Reason != strings.TrimSpace(original.Reason) {
		return fmt.Errorf("%w: authoring recovery command key %s", store.ErrIdempotencyConflict, request.CommandKey)
	}
	return nil
}

// ownerGrantedQuotaAdmissionRecoveryAllowed permits one otherwise
// non-retryable policy failure only when durable state proves all of the
// following: the exact StageAttempt was rejected for quota exhaustion, the
// frozen claims are now affordable, and the Task owner explicitly granted
// capacity after that failure. Other policy failures remain unavailable.
func (service *AuthoringRecoveryService) ownerGrantedQuotaAdmissionRecoveryAllowed(ctx context.Context, binding authoringRecoveryBinding, stage workflowkit.StageDescriptor, failed store.StageAttempt) (bool, error) {
	if failed.RunID != binding.run.ID || failed.StageKey != string(stage.Key) ||
		failed.FailureClass != string(workflowkit.FailurePolicy) || failed.FinishedAt == nil {
		return false, nil
	}
	taskID, err := binding.subject.quotaTaskID()
	if err != nil {
		return false, fmt.Errorf("%w: resolve quota-owning Task for policy recovery: %v", ErrAuthoringRecoveryUnavailable, err)
	}
	jobs, err := service.core.store.ListDurableJobsForRun(ctx, binding.run.ID)
	if err != nil {
		return false, err
	}
	var rejected *store.DurableAdmissionDecision
	for _, job := range jobs {
		if job.StageAttemptID != failed.ID {
			continue
		}
		decision, lookupErr := service.core.store.GetDurableAdmissionDecisionByIdempotencyKey(ctx, "stage-admission:"+job.ID)
		if lookupErr != nil {
			return false, lookupErr
		}
		if decision == nil || decision.Accepted || decision.Reason != store.AdmissionReasonQuotaExhausted ||
			decision.TaskID != taskID || decision.Actor != job.CreatedBy {
			continue
		}
		if rejected != nil {
			return false, fmt.Errorf("%w: StageAttempt %s has multiple quota-exhausted admissions", ErrAuthoringRecoveryUnavailable, failed.ID)
		}
		copyDecision := *decision
		rejected = &copyDecision
	}
	if rejected == nil {
		return false, nil
	}

	admission, err := BuildFrozenStageQuotaAdmission(binding.frozen.QuotaPolicy, stage)
	if err != nil {
		return false, fmt.Errorf("%w: validate frozen quota admission for stage %q: %v", ErrAuthoringRecoveryUnavailable, stage.Key, err)
	}
	if len(admission.Claims) == 0 {
		return false, nil
	}
	owner, err := taskOwnerFromAudit(ctx, service.core.store, taskID)
	if err != nil {
		return false, fmt.Errorf("%w: resolve quota-owning Task owner: %v", ErrAuthoringRecoveryUnavailable, err)
	}
	grantNotBefore := rejected.DecidedAt
	if failed.FinishedAt.After(grantNotBefore) {
		grantNotBefore = *failed.FinishedAt
	}
	ownerGrantedRequiredCapacity := false
	for _, claim := range admission.Claims {
		taskAccount, lookupErr := service.core.store.GetQuotaAccountForScope(ctx, store.QuotaScopeTask, taskID, claim.Dimension)
		if lookupErr != nil {
			return false, fmt.Errorf("%w: inspect task quota account %q: %v", ErrAuthoringRecoveryUnavailable, claim.Dimension, lookupErr)
		}
		actorAccount, lookupErr := service.core.store.GetQuotaAccountForScope(ctx, store.QuotaScopeActor, rejected.Actor, claim.Dimension)
		if lookupErr != nil {
			return false, fmt.Errorf("%w: inspect actor quota account %q: %v", ErrAuthoringRecoveryUnavailable, claim.Dimension, lookupErr)
		}
		if taskAccount == nil || actorAccount == nil || taskAccount.AvailableUnits() < claim.Units || actorAccount.AvailableUnits() < claim.Units {
			return false, nil
		}
		events, lookupErr := service.core.store.ListAuditEvents(ctx, store.ListAuditEventsRequest{
			EntityType: "quota_account",
			EntityID:   taskAccount.ID,
		})
		if lookupErr != nil {
			return false, fmt.Errorf("%w: inspect task quota grant audit: %v", ErrAuthoringRecoveryUnavailable, lookupErr)
		}
		for _, event := range events {
			if event.Action == "quota_account.granted" && event.Actor == owner && !event.CreatedAt.Before(grantNotBefore) {
				ownerGrantedRequiredCapacity = true
				break
			}
		}
	}
	return ownerGrantedRequiredCapacity, nil
}

func authoringRecoveryTargets(run store.WorkflowRun, workflow workflowkit.WorkflowDescriptor, state continuationRunState) (authoringRecoverySelection, error) {
	return authoringRecoveryTargetsWithInfraFailureOverride(run, workflow, state, nil)
}

func authoringRecoveryTargetsWithInfraFailureOverride(run store.WorkflowRun, workflow workflowkit.WorkflowDescriptor, state continuationRunState, failureOverride authoringRecoveryInfraFailureOverride) (authoringRecoverySelection, error) {
	repairSelection, err := activeAuthoringRepairSelection(workflow, state)
	if err != nil {
		return authoringRecoverySelection{}, err
	}
	if run.Status == store.WorkflowRunWaitingContinuation {
		if len(repairSelection.feedback) == 0 && len(repairSelection.targetNodeIDs) == 0 {
			return authoringRecoverySelection{}, fmt.Errorf("%w: waiting authoring Run is %s and has no supported needs_repair result", ErrAuthoringRecoveryUnavailable, run.Status)
		}
		return repairSelection, nil
	}
	order, err := workflow.TopologicalStages()
	if err != nil {
		return authoringRecoverySelection{}, err
	}
	targetSet := make(map[workflowkit.NodeID]struct{}, len(repairSelection.targetNodeIDs))
	for _, target := range repairSelection.targetNodeIDs {
		targetSet[target] = struct{}{}
	}
	failures := append([]string(nil), repairSelection.failureStageAttemptIDs...)
	for _, nodeID := range order {
		latest, exists := state.Latest[nodeID]
		if !exists {
			continue
		}
		switch latest.ExecutionStatus {
		case store.StageExecutionInfraFailed:
			stage, found := workflow.Stage(nodeID)
			if !found {
				return authoringRecoverySelection{}, fmt.Errorf("%w: frozen retry policy does not allow %s failure for stage %q", ErrAuthoringRecoveryUnavailable, latest.FailureClass, nodeID)
			}
			retryAllowed := stage.Retry.Allows(workflowkit.FailureClass(latest.FailureClass))
			if !retryAllowed && failureOverride != nil {
				var overrideErr error
				retryAllowed, overrideErr = failureOverride(stage, latest)
				if overrideErr != nil {
					return authoringRecoverySelection{}, overrideErr
				}
			}
			if !retryAllowed {
				return authoringRecoverySelection{}, fmt.Errorf("%w: frozen retry policy does not allow %s failure for stage %q", ErrAuthoringRecoveryUnavailable, latest.FailureClass, nodeID)
			}
			targetSet[nodeID] = struct{}{}
			failures = append(failures, latest.ID)
		case store.StageExecutionInterrupted, store.StageExecutionCanceled:
			targetSet[nodeID] = struct{}{}
			failures = append(failures, latest.ID)
		case store.StageExecutionQueued, store.StageExecutionRunning:
			if run.Status == store.WorkflowRunPaused {
				targetSet[nodeID] = struct{}{}
				failures = append(failures, latest.ID)
			}
		}
	}
	if len(targetSet) == 0 && (run.Status == store.WorkflowRunFailedRecoverable || run.Status == store.WorkflowRunPaused) {
		// A failure or pause can occur before a StageAttempt is persisted. The
		// source root is the only deterministic safe restart point.
		if len(order) == 0 {
			return authoringRecoverySelection{}, fmt.Errorf("%w: frozen authoring workflow has no stages", ErrAuthoringRecoveryUnavailable)
		}
		targetSet[order[0]] = struct{}{}
	}
	if len(targetSet) == 0 {
		return authoringRecoverySelection{}, fmt.Errorf("%w: no failed or paused authoring stage", ErrAuthoringRecoveryUnavailable)
	}
	targets := make([]workflowkit.NodeID, 0, len(targetSet))
	for _, nodeID := range order {
		if _, selected := targetSet[nodeID]; selected {
			targets = append(targets, nodeID)
		}
	}
	for _, nodeID := range targets {
		stage, found := workflow.Stage(nodeID)
		if !found || stage.OperatorOnly() {
			return authoringRecoverySelection{}, fmt.Errorf("%w: authoring recovery target %q is not automatically dispatchable", ErrAuthoringRecoveryUnavailable, nodeID)
		}
	}
	repairSelection.targetNodeIDs = targets
	repairSelection.failureStageAttemptIDs = failures
	return repairSelection, nil
}

func activeAuthoringRepairSelection(workflow workflowkit.WorkflowDescriptor, state continuationRunState) (authoringRecoverySelection, error) {
	// A direct content-producer needs_repair result has no downstream review
	// decision to bind. Re-run that producer with the same frozen inputs; the
	// failed attempt itself is the durable checkpoint. Invalidation will also
	// invalidate its dependent stages while preserving operator-only gates.
	producers := []workflowkit.NodeID{
		workflowkit.NodeID(workflowadapter.InstructionGen),
		workflowkit.NodeID(workflowadapter.TaskTOMLGen),
		workflowkit.NodeID(workflowadapter.DockerfileGen),
		workflowkit.NodeID(workflowadapter.SolveGen),
		workflowkit.NodeID(workflowadapter.TestGen),
		workflowkit.NodeID(workflowadapter.TestsAnalysis),
	}
	repairs := []struct {
		blocker      workflowkit.NodeID
		reviewKind   workflowadapter.ReviewKind
		artifactName string
		targets      []workflowkit.NodeID
	}{
		{
			blocker: workflowkit.NodeID(workflowadapter.TaskReview), reviewKind: workflowadapter.ReviewTaskDirection, artifactName: "task_review_decision",
			targets: []workflowkit.NodeID{workflowkit.NodeID(workflowadapter.RepoAnalyze), workflowkit.NodeID(workflowadapter.TaskDesign)},
		},
		{
			blocker: workflowkit.NodeID(workflowadapter.ContentReview), reviewKind: workflowadapter.ReviewContent, artifactName: "content_review_decision",
			targets: []workflowkit.NodeID{
				workflowkit.NodeID(workflowadapter.InstructionGen),
				workflowkit.NodeID(workflowadapter.TaskTOMLGen),
				workflowkit.NodeID(workflowadapter.DockerfileGen),
			},
		},
		{
			blocker: workflowkit.NodeID(workflowadapter.CodeEdgePackageAdmission), artifactName: "codeedge_package_admission_report",
			targets: append([]workflowkit.NodeID(nil), producers...),
		},
		{
			blocker: workflowkit.NodeID(workflowadapter.SolutionReview), reviewKind: workflowadapter.ReviewSolutionVerifier, artifactName: "solution_review_decision",
			targets: append([]workflowkit.NodeID(nil), producers...),
		},
	}
	targetSet := make(map[workflowkit.NodeID]struct{})
	selection := authoringRecoverySelection{}
	for _, repair := range repairs {
		latest, present := state.Latest[repair.blocker]
		if present && latest.ExecutionStatus == store.StageExecutionCompleted && latest.Verdict == store.VerdictNeedsRepair {
			selection.failureStageAttemptIDs = append(selection.failureStageAttemptIDs, latest.ID)
			selection.feedback = append(selection.feedback, authoringRecoveryFeedback{
				artifactName: repair.artifactName, reviewKind: string(repair.reviewKind), attemptID: latest.ID,
				consumerNodeIDs: append([]workflowkit.NodeID(nil), repair.targets...),
			})
			for _, target := range repair.targets {
				targetSet[target] = struct{}{}
			}
		}
	}
	for _, stage := range workflow.Stages {
		if stage.OperatorOnly() || stage.Effect != workflowkit.EffectContentProducer {
			continue
		}
		latest, present := state.Latest[stage.Key]
		if present && latest.ExecutionStatus == store.StageExecutionCompleted && latest.Verdict == store.VerdictNeedsRepair {
			if !stage.Verdicts.Allows(workflowkit.VerdictNeedsRepair) {
				return authoringRecoverySelection{}, fmt.Errorf("%w: stage %q persisted needs_repair outside its frozen verdict policy", ErrAuthoringRecoveryUnavailable, stage.Key)
			}
			targetSet[stage.Key] = struct{}{}
			selection.failureStageAttemptIDs = append(selection.failureStageAttemptIDs, latest.ID)
		}
	}
	order, err := workflow.TopologicalStages()
	if err != nil {
		return authoringRecoverySelection{}, err
	}
	for _, nodeID := range order {
		if _, selected := targetSet[nodeID]; selected {
			selection.targetNodeIDs = append(selection.targetNodeIDs, nodeID)
		}
	}
	return selection, nil
}

func (service *AuthoringRecoveryService) authoringRecoveryRequiredScheduledInputs(ctx context.Context, binding authoringRecoveryBinding, selection authoringRecoverySelection) (map[workflowkit.NodeID][]workflowkit.ArtifactBinding, error) {
	if len(selection.feedback) == 0 {
		return nil, nil
	}
	required := make(map[workflowkit.NodeID][]workflowkit.ArtifactBinding)
	for _, repair := range selection.feedback {
		if repair.artifactName == "" || repair.attemptID == "" || len(repair.consumerNodeIDs) == 0 {
			return nil, fmt.Errorf("%w: repair feedback selection is incomplete", ErrAuthoringRecoveryUnavailable)
		}
		var frozenFeedback workflowkit.ArtifactBinding
		for _, nodeID := range repair.consumerNodeIDs {
			stage, found := binding.frozen.Workflow.Stage(nodeID)
			if !found || stage.OperatorOnly() {
				return nil, fmt.Errorf("%w: repair feedback consumer %q is unavailable", ErrAuthoringRecoveryUnavailable, nodeID)
			}
			declared := false
			for _, input := range stage.Inputs {
				if input.Name == repair.artifactName {
					if input.Required {
						return nil, fmt.Errorf("%w: repair feedback input %q on stage %q is not optional", ErrAuthoringRecoveryUnavailable, input.Name, nodeID)
					}
					declared = true
					break
				}
			}
			if !declared {
				return nil, fmt.Errorf("%w: stage %q does not declare repair feedback input %q", ErrAuthoringRecoveryUnavailable, nodeID, repair.artifactName)
			}
			inputs, err := resolveStageInputsForSubject(ctx, service.core.store, service.core.objects, binding.run, binding.subject, stage)
			if err != nil {
				return nil, fmt.Errorf("%w: resolve repair feedback for stage %q: %v", ErrAuthoringRecoveryUnavailable, nodeID, err)
			}
			var feedback workflowkit.ArtifactBinding
			for _, input := range inputs {
				if input.Name == repair.artifactName {
					feedback = input
					break
				}
			}
			if feedback.Name == "" {
				return nil, fmt.Errorf("%w: stage %q lacks immutable repair feedback %q", ErrAuthoringRecoveryUnavailable, nodeID, repair.artifactName)
			}
			reference, err := service.core.store.GetArtifactRef(ctx, string(feedback.ArtifactID))
			if err != nil {
				return nil, err
			}
			if reference == nil || reference.AttemptID != repair.attemptID || reference.ArtifactKey != repair.artifactName {
				return nil, fmt.Errorf("%w: repair feedback %q does not come from the selected needs_repair attempt", ErrAuthoringRecoveryUnavailable, repair.artifactName)
			}
			if frozenFeedback.Name == "" {
				reader := newStageInputReaderForSubject(service.core.store, service.core.objects, binding.run, binding.subject, []workflowkit.ArtifactBinding{feedback})
				raw, err := reader(ctx, feedback)
				if err != nil {
					return nil, fmt.Errorf("%w: read repair feedback %q: %v", ErrAuthoringRecoveryUnavailable, repair.artifactName, err)
				}
				if err := service.validateAuthoringRecoveryFeedback(ctx, binding, repair, raw); err != nil {
					return nil, err
				}
				frozenFeedback = feedback
			} else if feedback != frozenFeedback {
				return nil, fmt.Errorf("%w: repair feedback binding differs across scheduled consumers", ErrAuthoringRecoveryUnavailable)
			}
			required[nodeID] = append(required[nodeID], feedback)
		}
	}
	for nodeID := range required {
		sort.Slice(required[nodeID], func(left, right int) bool { return required[nodeID][left].Name < required[nodeID][right].Name })
	}
	return required, nil
}

func (service *AuthoringRecoveryService) validateAuthoringRecoveryFeedback(ctx context.Context, binding authoringRecoveryBinding, repair authoringRecoveryFeedback, raw []byte) error {
	if repair.artifactName == "codeedge_package_admission_report" {
		var receipt standardAuthoringAdmissionReceipt
		if err := decodeStrictJSON(string(raw), &receipt); err != nil {
			return fmt.Errorf("%w: decode package-admission repair feedback: %v", ErrAuthoringRecoveryUnavailable, err)
		}
		attempt, err := service.core.store.GetStageAttempt(ctx, repair.attemptID)
		if err != nil {
			return err
		}
		if receipt.Format != standardAuthoringAdmissionReceiptFormat || receipt.Version != standardAuthoringAdmissionReceiptVersion ||
			receipt.RunID != binding.run.ID || binding.subject.AuthoringSource == nil || binding.subject.AuthoringSession == nil ||
			receipt.AuthoringSourceID != binding.subject.AuthoringSource.ID || receipt.AuthoringSessionID != binding.subject.AuthoringSession.ID ||
			receipt.Report.Passed || len(receipt.Report.Violations) == 0 || attempt == nil || attempt.RunID != binding.run.ID ||
			attempt.StageKey != workflowadapter.CodeEdgePackageAdmission || attempt.ExecutionStatus != store.StageExecutionCompleted ||
			attempt.Verdict != store.VerdictNeedsRepair || attempt.InputFingerprint != string(receipt.InputFingerprint) {
			return fmt.Errorf("%w: package-admission repair feedback is not a bound failed report", ErrAuthoringRecoveryUnavailable)
		}
		return nil
	}
	var decision authoringReviewGateDecisionArtifact
	if err := decodeStrictJSON(string(raw), &decision); err != nil {
		return fmt.Errorf("%w: decode review repair feedback: %v", ErrAuthoringRecoveryUnavailable, err)
	}
	if decision.Format != authoringReviewGateDecisionArtifactFormat || decision.Action != store.ReviewDecisionRequestChanges ||
		binding.subject.AuthoringSource == nil || binding.subject.AuthoringSession == nil ||
		decision.AuthoringSourceID != binding.subject.AuthoringSource.ID || decision.AuthoringSessionID != binding.subject.AuthoringSession.ID ||
		decision.SourceSnapshotDigest != binding.subject.subjectDigest() || decision.ReviewKind != repair.reviewKind ||
		strings.TrimSpace(decision.DecisionReason) == "" {
		return fmt.Errorf("%w: review repair feedback is not a bound request_changes decision", ErrAuthoringRecoveryUnavailable)
	}
	gate, err := service.core.store.GetAuthoringReviewGateBindingByStageAttempt(ctx, repair.attemptID)
	if err != nil {
		return err
	}
	if gate == nil || gate.RunID != binding.run.ID || gate.StageAttemptID != repair.attemptID || gate.ReviewKind != repair.reviewKind ||
		gate.ReviewRequestID != decision.ReviewRequestID || gate.AuthoringSourceID != decision.AuthoringSourceID ||
		gate.AuthoringSessionID != decision.AuthoringSessionID || gate.SourceSnapshotDigest != decision.SourceSnapshotDigest ||
		gate.EvidenceManifestDigest != decision.EvidenceManifestDigest || gate.InputFingerprint != decision.InputFingerprint {
		return fmt.Errorf("%w: review repair feedback differs from its durable gate binding", ErrAuthoringRecoveryUnavailable)
	}
	decisions, err := service.core.store.ListAuthoringReviewDecisionsForRequest(ctx, gate.ReviewRequestID)
	if err != nil {
		return err
	}
	if len(decisions) != 1 || decisions[0].ID != decision.ReviewDecisionID || decisions[0].BindingID != gate.ID ||
		decisions[0].Action != decision.Action || decisions[0].Actor != decision.DecisionActor || decisions[0].Reason != decision.DecisionReason {
		return fmt.Errorf("%w: review repair feedback differs from its durable operator decision", ErrAuthoringRecoveryUnavailable)
	}
	return nil
}

func validatePersistedAuthoringRecoveryTargets(command normalizedAuthoringRecoveryCommand, assessment authoringRecoveryAssessment) error {
	if len(assessment.targetNodeIDs) != len(command.TargetNodeIDs) || len(assessment.failureStageAttemptIDs) != len(command.FailureStageAttemptIDs) {
		return fmt.Errorf("%w: authoring recovery target state changed", store.ErrOptimisticLock)
	}
	for index := range assessment.targetNodeIDs {
		if assessment.targetNodeIDs[index] != command.TargetNodeIDs[index] {
			return fmt.Errorf("%w: authoring recovery failure checkpoint changed", store.ErrOptimisticLock)
		}
	}
	for index := range assessment.failureStageAttemptIDs {
		if assessment.failureStageAttemptIDs[index] != command.FailureStageAttemptIDs[index] {
			return fmt.Errorf("%w: authoring recovery failure checkpoint changed", store.ErrOptimisticLock)
		}
	}
	return nil
}

func buildAuthoringRecoveryPlan(planID, commandID string, command normalizedAuthoringRecoveryCommand, run store.WorkflowRun, subject workflowRunSubject, workflow workflowkit.WorkflowDescriptor, state continuationRunState, invalidation workflowkit.InvalidationPlan, requiredScheduledInputs map[workflowkit.NodeID][]workflowkit.ArtifactBinding, expiresAt time.Time) (workflowkit.ContinuationPlanSnapshot, error) {
	return buildSameRunContinuationPlan(planID, commandID, continuationPlanInput{Expected: command.Expected, RequiredScheduledInputs: requiredScheduledInputs}, run, subject.subjectRevisionID(), workflowkit.SubjectDigest(subject.subjectDigest()), workflow, state, invalidation, command.TargetNodeIDs, workflowkit.StrategyRetryAttempt, expiresAt, true)
}
