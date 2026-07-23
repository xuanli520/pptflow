package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const codeEdgeEvaluatorRunTrigger = "codeedge-evaluator"

var (
	ErrCodeEdgeEvaluatorDefinitionUnavailable = errors.New("CodeEdge evaluator launch definition is unavailable")
	// ErrCodeEdgeEvaluatorDefinitionInvalid intentionally does not wrap a
	// deployment provider's original error. Providers may inspect
	// environment-backed endpoint or credential material; their raw failure
	// text must not cross into a CLI/TUI response, lifecycle receipt, or audit
	// payload.
	ErrCodeEdgeEvaluatorDefinitionInvalid = errors.New("CodeEdge evaluator launch definition did not pass controlled validation")
	// ErrCodeEdgeEvaluatorChildAlreadyExists preserves the one-canonical-child
	// boundary for a Phase-1 parent. Store-level uniqueness is the race
	// authority; this error gives API callers a stable explanation.
	ErrCodeEdgeEvaluatorChildAlreadyExists = errors.New("CodeEdge evaluator child already exists for the Phase-1 parent")
)

// EvaluatorRunDefinitionRequest identifies the immutable parent/subject facts
// from which deployment composition may construct its closed evaluator child
// definition. It contains no caller-selected provider, model, endpoint,
// secret, executable, profile, or arbitrary stage payload.
type EvaluatorRunDefinitionRequest struct {
	TaskID         string
	RevisionID     string
	RevisionDigest workflowkit.SubjectDigest
	ParentRunID    string
	// ParentProfile is read from the parent's managed immutable inputs only
	// after its canonical bytes and manifest fingerprints have been verified.
	// It is contextual evidence for the parent approval check only: a controlled
	// deployment provider must obtain the evaluator child's complete profile
	// from its immutable deployment lock, never project it from this parent.
	ParentProfile workflowadapter.ExecutionProfile
}

// EvaluatorRunDefinition is the narrow app-facing result of production
// composition. The application service validates it again against the closed
// evaluator template and the installed operation resolver before freezing it.
type EvaluatorRunDefinition struct {
	Profile       workflowadapter.ExecutionProfile
	ExecutionSpec workflowadapter.RunExecutionSpec
}

// EvaluatorRunDefinitionProvider is implemented only by controlled deployment
// composition. It is intentionally not a CLI/TUI interface: user interfaces
// can launch the evaluator but cannot choose or construct its execution
// definition.
type EvaluatorRunDefinitionProvider interface {
	DefinitionForEvaluatorRun(context.Context, EvaluatorRunDefinitionRequest) (EvaluatorRunDefinition, error)
}

// CodeEdgeEvaluatorLaunchPlan describes the fixed child Run that an operator
// is about to freeze. It is safe for CLI/TUI projection because it contains
// only immutable identities and fingerprints, never deployment secrets or
// endpoint text.
type CodeEdgeEvaluatorLaunchPlan struct {
	ParentRunID                 string
	TaskID                      string
	RevisionID                  string
	RevisionDigest              string
	Template                    workflowadapter.TemplateReference
	ExecutionProfileFingerprint workflowkit.Fingerprint
	ExecutionSpecFingerprint    workflowkit.Fingerprint
}

// PreparedCodeEdgeEvaluatorLaunch is the durable result of the first explicit
// confirmation. Confirming it later creates exactly one evaluator child Run.
type PreparedCodeEdgeEvaluatorLaunch struct {
	InputBundleID            string
	ParentRunID              string
	ProfileFingerprint       string
	ExecutionSpecFingerprint string
}

// CodeEdgeEvaluatorLaunchResult joins the immutable child-Run receipt with
// the controlled worker-handoff receipt created by the final confirmation.
// The handoff remains durable authority for child-process ownership.
type CodeEdgeEvaluatorLaunchResult struct {
	Receipt LifecycleMutationReceipt
	Handoff store.RunWorkerHandoff
}

// CodeEdgeEvaluatorLaunchService owns the application boundary for the strict
// CodeEdge evaluator child. It uses the generic RunService and V12 lifecycle
// ledger; it is not another execution engine or scheduler.
type CodeEdgeEvaluatorLaunchService struct {
	core        *lifecycleServiceCore
	mutations   *LifecycleMutationService
	definitions EvaluatorRunDefinitionProvider
}

// Available reports whether this process was composed with an evaluator
// definition provider. A true value does not bypass Plan/Prepare validation of
// the current catalog/lock, parent review gate, or lifecycle checkpoint.
func (service *CodeEdgeEvaluatorLaunchService) Available() bool {
	return service != nil && service.core != nil && service.mutations != nil && service.definitions != nil
}

// Plan validates the selected parent Run and the installed closed evaluator
// definition without writing a bundle, lifecycle operation, Run, job, or
// provider effect. UI adapters use it for their first read-only preview.
func (service *CodeEdgeEvaluatorLaunchService) Plan(ctx context.Context, parentRunID string) (CodeEdgeEvaluatorLaunchPlan, error) {
	parent, revision, err := service.loadApprovedParent(ctx, parentRunID)
	if err != nil {
		return CodeEdgeEvaluatorLaunchPlan{}, err
	}
	if existing, existingErr := service.codeEdgeEvaluatorChildForParent(ctx, parent); existingErr != nil {
		return CodeEdgeEvaluatorLaunchPlan{}, existingErr
	} else if existing != nil {
		return CodeEdgeEvaluatorLaunchPlan{}, fmt.Errorf("%w: parent Run %s", ErrCodeEdgeEvaluatorChildAlreadyExists, parent.ID)
	}
	definition, err := service.definitionFor(ctx, parent, revision)
	if err != nil {
		return CodeEdgeEvaluatorLaunchPlan{}, err
	}
	profile, specification, err := service.bindAndValidateDefinition(parent, revision, definition, parent.ID)
	if err != nil {
		return CodeEdgeEvaluatorLaunchPlan{}, err
	}
	profileFingerprint, err := profile.Fingerprint()
	if err != nil {
		return CodeEdgeEvaluatorLaunchPlan{}, err
	}
	specificationFingerprint, err := specification.Fingerprint()
	if err != nil {
		return CodeEdgeEvaluatorLaunchPlan{}, err
	}
	return CodeEdgeEvaluatorLaunchPlan{
		ParentRunID:                 parent.ID,
		TaskID:                      parent.TaskID,
		RevisionID:                  parent.RevisionID,
		RevisionDigest:              revision.TaskDigest,
		Template:                    workflowadapter.CodeEdgeEvaluatorChildTemplateReference(),
		ExecutionProfileFingerprint: profileFingerprint,
		ExecutionSpecFingerprint:    specificationFingerprint,
	}, nil
}

// Prepare is the first explicit confirmation. It persists a canonical input
// bundle but deliberately does not create a Run or invoke a provider.
func (service *CodeEdgeEvaluatorLaunchService) Prepare(ctx context.Context, command CodeEdgeEvaluatorLaunchCommand) (PreparedCodeEdgeEvaluatorLaunch, error) {
	start, parent, revision, err := service.validateLaunchCommand(ctx, command)
	if err != nil {
		return PreparedCodeEdgeEvaluatorLaunch{}, err
	}
	// Preserve replay of the original frozen input bundle even after its child
	// Run exists. A new idempotency key, however, must not freeze a competing
	// evaluator launch for the same parent.
	if directoryExists(service.core.layout.runStartInputDirectory(start.IdempotencyKey)) {
		inputs, inputErr := service.mutations.readFrozenRunStartInputs(LifecycleMutationCodeEdgeEvaluator, start)
		if inputErr != nil {
			return PreparedCodeEdgeEvaluatorLaunch{}, inputErr
		}
		if inputErr := service.validateFrozenInputs(ctx, parent, revision, start, inputs); inputErr != nil {
			return PreparedCodeEdgeEvaluatorLaunch{}, inputErr
		}
		prepared := preparedStartRunResult(inputs)
		return PreparedCodeEdgeEvaluatorLaunch{
			InputBundleID:            prepared.InputBundleID,
			ParentRunID:              parent.ID,
			ProfileFingerprint:       prepared.ProfileFingerprint,
			ExecutionSpecFingerprint: prepared.ExecutionSpecFingerprint,
		}, nil
	}
	if existing, existingErr := service.codeEdgeEvaluatorChildForParent(ctx, parent); existingErr != nil {
		return PreparedCodeEdgeEvaluatorLaunch{}, existingErr
	} else if existing != nil {
		return PreparedCodeEdgeEvaluatorLaunch{}, fmt.Errorf("%w: parent Run %s", ErrCodeEdgeEvaluatorChildAlreadyExists, parent.ID)
	}
	inputs, err := service.mutations.prepareRunStartInputs(ctx, LifecycleMutationCodeEdgeEvaluator, start, func() (workflowadapter.ExecutionProfile, workflowadapter.RunExecutionSpec, error) {
		definition, definitionErr := service.definitionFor(ctx, parent, revision)
		if definitionErr != nil {
			return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, definitionErr
		}
		return service.bindAndValidateDefinition(parent, revision, definition, start.IdempotencyKey)
	})
	if err != nil {
		return PreparedCodeEdgeEvaluatorLaunch{}, err
	}
	if err := service.validateFrozenInputs(ctx, parent, revision, start, inputs); err != nil {
		return PreparedCodeEdgeEvaluatorLaunch{}, err
	}
	prepared := preparedStartRunResult(inputs)
	return PreparedCodeEdgeEvaluatorLaunch{
		InputBundleID:            prepared.InputBundleID,
		ParentRunID:              parent.ID,
		ProfileFingerprint:       prepared.ProfileFingerprint,
		ExecutionSpecFingerprint: prepared.ExecutionSpecFingerprint,
	}, nil
}

// ConfirmAndLaunch consumes only an already frozen evaluator input bundle,
// creates its one child Run, and launches the existing controlled worker
// handoff protocol. A direct confirm without Prepare is rejected, preserving
// the CLI/TUI two-stage confirmation contract for a real external evaluation.
func (service *CodeEdgeEvaluatorLaunchService) ConfirmAndLaunch(ctx context.Context, command CodeEdgeEvaluatorLaunchCommand, launcher RunWorkerHandoffLauncher) (CodeEdgeEvaluatorLaunchResult, error) {
	if launcher == nil {
		return CodeEdgeEvaluatorLaunchResult{}, fmt.Errorf("CodeEdge evaluator controlled worker launcher is required")
	}
	receipt, run, err := service.confirmRun(ctx, command)
	if err != nil {
		return CodeEdgeEvaluatorLaunchResult{}, err
	}
	handoff, err := service.launchWorker(ctx, run, receipt.OperationID, launcher)
	if err != nil {
		return CodeEdgeEvaluatorLaunchResult{}, err
	}
	return CodeEdgeEvaluatorLaunchResult{Receipt: receipt, Handoff: handoff}, nil
}

// confirmRun creates or replays the child Run. Worker launch is intentionally
// kept outside this mutation so the app service can reuse the established
// durable reserve/spawn/claim protocol supplied by CLI/TUI composition.
func (service *CodeEdgeEvaluatorLaunchService) confirmRun(ctx context.Context, command CodeEdgeEvaluatorLaunchCommand) (LifecycleMutationReceipt, store.WorkflowRun, error) {
	if service == nil || service.core == nil || service.core.store == nil || service.mutations == nil {
		return LifecycleMutationReceipt{}, store.WorkflowRun{}, fmt.Errorf("CodeEdge evaluator launch service is not configured")
	}
	if receipt, replayed, err := service.mutations.completedOperationReplay(ctx, LifecycleMutationCodeEdgeEvaluator, command.LifecycleMutationCommandBase); err != nil {
		return LifecycleMutationReceipt{}, store.WorkflowRun{}, err
	} else if replayed {
		run, runErr := service.core.store.GetWorkflowRun(ctx, receipt.RunID)
		if runErr != nil {
			return LifecycleMutationReceipt{}, store.WorkflowRun{}, runErr
		}
		if run == nil {
			return LifecycleMutationReceipt{}, store.WorkflowRun{}, fmt.Errorf("%w: CodeEdge evaluator child Run %s", ErrLifecycleNotFound, receipt.RunID)
		}
		return receipt, *run, nil
	}
	start, parent, revision, err := service.validateLaunchCommand(ctx, command)
	if err != nil {
		return LifecycleMutationReceipt{}, store.WorkflowRun{}, err
	}
	existingChild, err := service.codeEdgeEvaluatorChildForParent(ctx, parent)
	if err != nil {
		return LifecycleMutationReceipt{}, store.WorkflowRun{}, err
	}
	if existingChild != nil {
		existingOperation, operationErr := service.core.store.GetLifecycleOperationByIdempotencyKey(ctx, command.IdempotencyKey)
		if operationErr != nil {
			return LifecycleMutationReceipt{}, store.WorkflowRun{}, operationErr
		}
		// A crash after creating the child but before completing the lifecycle
		// receipt must be resumable by the same operation. Every other child is
		// a competing evaluator invocation and is rejected before any worker
		// launch can occur.
		if existingOperation == nil || existingOperation.Action != string(LifecycleMutationCodeEdgeEvaluator) || existingOperation.RunID != existingChild.ID {
			return LifecycleMutationReceipt{}, store.WorkflowRun{}, fmt.Errorf("%w: parent Run %s", ErrCodeEdgeEvaluatorChildAlreadyExists, parent.ID)
		}
	}
	inputs, err := service.mutations.readFrozenRunStartInputs(LifecycleMutationCodeEdgeEvaluator, start)
	if err != nil {
		return LifecycleMutationReceipt{}, store.WorkflowRun{}, err
	}
	if err := service.validateFrozenInputs(ctx, parent, revision, start, inputs); err != nil {
		return LifecycleMutationReceipt{}, store.WorkflowRun{}, err
	}

	runID, err := store.NewUUIDv7()
	if err != nil {
		return LifecycleMutationReceipt{}, store.WorkflowRun{}, fmt.Errorf("allocate CodeEdge evaluator child Run ID: %w", err)
	}
	payload := struct {
		Format                   string `json:"format"`
		InputBundleID            string `json:"input_bundle_id"`
		ParentRunID              string `json:"parent_run_id"`
		ProfileFingerprint       string `json:"profile_fingerprint"`
		ExecutionSpecFingerprint string `json:"execution_spec_fingerprint"`
	}{
		Format:                   "harbor.codeedge-evaluator-launch.v1",
		InputBundleID:            inputs.Bundle.IdempotencyKey,
		ParentRunID:              parent.ID,
		ProfileFingerprint:       string(inputs.Bundle.ProfileFingerprint),
		ExecutionSpecFingerprint: string(inputs.Bundle.ExecutionSpecFingerprint),
	}
	op, replay, err := service.mutations.begin(ctx, LifecycleMutationCodeEdgeEvaluator, command.LifecycleMutationCommandBase, payload, lifecycleOperationTargets{
		TaskID: command.Expected.TaskID, RevisionID: command.Expected.RevisionID, RunID: runID,
	})
	if err != nil || replay != nil {
		receipt, replayErr := lifecycleReplayResult(replay, err)
		if replayErr != nil {
			return LifecycleMutationReceipt{}, store.WorkflowRun{}, replayErr
		}
		run, runErr := service.core.store.GetWorkflowRun(ctx, receipt.RunID)
		if runErr != nil {
			return LifecycleMutationReceipt{}, store.WorkflowRun{}, runErr
		}
		if run == nil {
			return LifecycleMutationReceipt{}, store.WorkflowRun{}, fmt.Errorf("%w: CodeEdge evaluator child Run %s", ErrLifecycleNotFound, receipt.RunID)
		}
		return receipt, *run, nil
	}
	if existing, lookupErr := service.core.store.GetWorkflowRun(ctx, op.RunID); lookupErr != nil {
		return LifecycleMutationReceipt{}, store.WorkflowRun{}, lookupErr
	} else if existing != nil {
		if existing.TaskID != op.TaskID || existing.RevisionID != op.RevisionID || existing.ParentRunID != parent.ID ||
			existing.WorkflowTemplateID != workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID ||
			existing.WorkflowTemplateVersion != workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion {
			return LifecycleMutationReceipt{}, store.WorkflowRun{}, fmt.Errorf("%w: CodeEdge evaluator child Run %s does not match lifecycle operation", store.ErrIdempotencyConflict, existing.ID)
		}
		receipt, completeErr := service.mutations.complete(ctx, op, codeEdgeEvaluatorRunReceipt(*existing))
		return receipt, *existing, completeErr
	}
	if err := service.mutations.validateCheckpoint(ctx, command.Expected); err != nil {
		return LifecycleMutationReceipt{}, store.WorkflowRun{}, err
	}
	// Repeat the approved-parent proof immediately before scheduling the child
	// so a confirmation cannot race a parent/review state change.
	parent, revision, err = service.loadApprovedParent(ctx, parent.ID)
	if err != nil {
		return LifecycleMutationReceipt{}, store.WorkflowRun{}, err
	}
	if parent.TaskID != op.TaskID || parent.RevisionID != op.RevisionID || revision.ID != op.RevisionID {
		return LifecycleMutationReceipt{}, store.WorkflowRun{}, fmt.Errorf("%w: CodeEdge evaluator parent no longer matches frozen lifecycle target", store.ErrOptimisticLock)
	}
	run, err := (&RunService{core: service.core}).StartRun(ctx, StartRunRequest{
		ID:                            op.RunID,
		TaskID:                        op.TaskID,
		RevisionID:                    op.RevisionID,
		Profile:                       inputs.Profile,
		ExecutionSpec:                 inputs.ExecutionSpec,
		InputBundleID:                 inputs.Bundle.IdempotencyKey,
		ProfileFingerprint:            inputs.Bundle.ProfileFingerprint,
		ExecutionSpecFingerprint:      inputs.Bundle.ExecutionSpecFingerprint,
		DeploymentCatalogReceipt:      append([]byte(nil), inputs.DeploymentCatalogReceipt...),
		DeploymentCatalogLockIdentity: cloneDeploymentCatalogLockIdentity(inputs.DeploymentCatalogLockIdentity),
		ParentRunID:                   parent.ID,
		Trigger:                       inputs.Bundle.Trigger,
		Actor:                         op.Actor,
		Reason:                        op.Reason,
	})
	if err != nil {
		if errors.Is(err, store.ErrIdentityCollision) {
			if child, childErr := service.codeEdgeEvaluatorChildForParent(ctx, parent); childErr != nil {
				return LifecycleMutationReceipt{}, store.WorkflowRun{}, childErr
			} else if child != nil {
				return LifecycleMutationReceipt{}, store.WorkflowRun{}, fmt.Errorf("%w: parent Run %s", ErrCodeEdgeEvaluatorChildAlreadyExists, parent.ID)
			}
		}
		return LifecycleMutationReceipt{}, store.WorkflowRun{}, err
	}
	receipt, completeErr := service.mutations.complete(ctx, op, codeEdgeEvaluatorRunReceipt(run))
	return receipt, run, completeErr
}

func (service *CodeEdgeEvaluatorLaunchService) codeEdgeEvaluatorChildForParent(ctx context.Context, parent store.WorkflowRun) (*store.WorkflowRun, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return nil, fmt.Errorf("CodeEdge evaluator launch service is not configured")
	}
	runs, err := service.core.store.ListWorkflowRunsForTask(ctx, parent.TaskID)
	if err != nil {
		return nil, err
	}
	var child *store.WorkflowRun
	for index := range runs {
		run := runs[index]
		if run.ParentRunID != parent.ID || run.WorkflowTemplateID != workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID {
			continue
		}
		if child != nil {
			return nil, fmt.Errorf("%w: parent Run %s has multiple persisted children", ErrCodeEdgeEvaluatorChildAlreadyExists, parent.ID)
		}
		child = &run
	}
	return child, nil
}

func (service *CodeEdgeEvaluatorLaunchService) launchWorker(ctx context.Context, run store.WorkflowRun, lifecycleOperationID string, launcher RunWorkerHandoffLauncher) (store.RunWorkerHandoff, error) {
	if service == nil || service.core == nil || service.core.store == nil || launcher == nil {
		return store.RunWorkerHandoff{}, fmt.Errorf("CodeEdge evaluator controlled worker handoff is not configured")
	}
	if err := store.ValidateUUIDv7(lifecycleOperationID); err != nil {
		return store.RunWorkerHandoff{}, fmt.Errorf("CodeEdge evaluator lifecycle operation ID: %w", err)
	}
	// The lifecycle operation ID is a persistent, globally unique UUIDv7. It
	// is used only as the handoff idempotency key; the handoff entity itself
	// receives a distinct Store-allocated UUIDv7. Thus a crash after Run
	// creation can retry the same one launch authority without reusing entity
	// identities or creating a second child process.
	handoff, err := (&RunWorkerHandoffService{core: service.core}).LaunchRunWorkerHandoff(ctx, ReserveRunWorkerHandoffCommand{
		IdempotencyKey: lifecycleOperationID,
		RunID:          run.ID,
		Expected: RunWorkerHandoffCheckpoint{
			RunVersion: run.Version, ExecutionEpoch: run.ExecutionEpoch, DefinitionHash: run.DefinitionHash,
		},
		Owner:  codeEdgeEvaluatorWorkerOwner(run.ID),
		Actor:  run.CreatedBy,
		Reason: codeEdgeEvaluatorWorkerReason(run.ID),
	}, launcher)
	if err != nil {
		return store.RunWorkerHandoff{}, err
	}
	switch handoff.State {
	case store.RunWorkerHandoffLaunching, store.RunWorkerHandoffHandedOff, store.RunWorkerHandoffReleased:
		return handoff, nil
	case store.RunWorkerHandoffFailed:
		return store.RunWorkerHandoff{}, fmt.Errorf("CodeEdge evaluator controlled worker handoff %s failed: %s", handoff.ID, strings.TrimSpace(handoff.FailureReason))
	case store.RunWorkerHandoffExpired:
		return store.RunWorkerHandoff{}, fmt.Errorf("CodeEdge evaluator controlled worker handoff %s expired before child-worker claim", handoff.ID)
	default:
		return store.RunWorkerHandoff{}, fmt.Errorf("CodeEdge evaluator controlled worker handoff %s has unsupported state %s", handoff.ID, handoff.State)
	}
}

func codeEdgeEvaluatorWorkerOwner(runID string) string {
	return "codeedge-evaluator:" + runID
}

func codeEdgeEvaluatorWorkerReason(runID string) string {
	return "launch controlled CodeEdge evaluator child worker for Run " + runID
}

func (service *CodeEdgeEvaluatorLaunchService) validateLaunchCommand(ctx context.Context, command CodeEdgeEvaluatorLaunchCommand) (StartRunLifecycleCommand, store.WorkflowRun, store.TaskRevision, error) {
	if service == nil || service.core == nil || service.mutations == nil {
		return StartRunLifecycleCommand{}, store.WorkflowRun{}, store.TaskRevision{}, fmt.Errorf("CodeEdge evaluator launch service is not configured")
	}
	parentRunID := strings.TrimSpace(command.ParentRunID)
	if err := store.ValidateUUIDv7(parentRunID); err != nil {
		return StartRunLifecycleCommand{}, store.WorkflowRun{}, store.TaskRevision{}, fmt.Errorf("CodeEdge evaluator parent Run ID: %w", err)
	}
	if command.Expected.RunID != parentRunID || command.Expected.RunVersion <= 0 || command.Expected.RunDefinitionHash == "" {
		return StartRunLifecycleCommand{}, store.WorkflowRun{}, store.TaskRevision{}, fmt.Errorf("CodeEdge evaluator requires the captured parent Run checkpoint")
	}
	start := StartRunLifecycleCommand{
		LifecycleMutationCommandBase: command.LifecycleMutationCommandBase,
		ParentRunID:                  parentRunID,
		Trigger:                      codeEdgeEvaluatorRunTrigger,
	}
	if err := validateStartRunInputCommand(start); err != nil {
		return StartRunLifecycleCommand{}, store.WorkflowRun{}, store.TaskRevision{}, err
	}
	parent, revision, err := service.loadApprovedParent(ctx, parentRunID)
	if err != nil {
		return StartRunLifecycleCommand{}, store.WorkflowRun{}, store.TaskRevision{}, err
	}
	if parent.TaskID != command.Expected.TaskID || parent.RevisionID != command.Expected.RevisionID || revision.TaskDigest != command.Expected.RevisionDigest {
		return StartRunLifecycleCommand{}, store.WorkflowRun{}, store.TaskRevision{}, fmt.Errorf("%w: CodeEdge evaluator parent does not match the confirmed TaskRevision", store.ErrOptimisticLock)
	}
	return start, parent, revision, nil
}

func (service *CodeEdgeEvaluatorLaunchService) loadApprovedParent(ctx context.Context, parentRunID string) (store.WorkflowRun, store.TaskRevision, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return store.WorkflowRun{}, store.TaskRevision{}, fmt.Errorf("CodeEdge evaluator launch service is not configured")
	}
	if !service.Available() {
		return store.WorkflowRun{}, store.TaskRevision{}, ErrCodeEdgeEvaluatorDefinitionUnavailable
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(parentRunID)); err != nil {
		return store.WorkflowRun{}, store.TaskRevision{}, fmt.Errorf("CodeEdge evaluator parent Run ID: %w", err)
	}
	parent, err := service.core.store.GetWorkflowRun(ctx, parentRunID)
	if err != nil {
		return store.WorkflowRun{}, store.TaskRevision{}, err
	}
	if parent == nil {
		return store.WorkflowRun{}, store.TaskRevision{}, fmt.Errorf("%w: parent workflow Run %s", ErrLifecycleNotFound, parentRunID)
	}
	if !isCodeEdgePhase1Run(*parent) {
		return store.WorkflowRun{}, store.TaskRevision{}, fmt.Errorf("CodeEdge evaluator parent Run %s must use %s@%s", parent.ID, workflowadapter.CodeEdgePhase1WorkflowTemplateID, workflowadapter.CodeEdgePhase1WorkflowTemplateVersion)
	}
	revision, err := service.core.store.GetTaskRevision(ctx, parent.RevisionID)
	if err != nil {
		return store.WorkflowRun{}, store.TaskRevision{}, err
	}
	if revision == nil || revision.TaskID != parent.TaskID {
		return store.WorkflowRun{}, store.TaskRevision{}, fmt.Errorf("%w: CodeEdge evaluator parent revision %s", ErrLifecycleNotFound, parent.RevisionID)
	}
	if err := requireApprovedCodeEdgeReviewGate(ctx, service.core.store, *parent, *revision, workflowadapter.FinalReview, workflowadapter.ReviewFinalQuality); err != nil {
		return store.WorkflowRun{}, store.TaskRevision{}, fmt.Errorf("CodeEdge evaluator requires an approved parent final review: %w", err)
	}
	return *parent, *revision, nil
}

func (service *CodeEdgeEvaluatorLaunchService) definitionFor(ctx context.Context, parent store.WorkflowRun, revision store.TaskRevision) (EvaluatorRunDefinition, error) {
	if !service.Available() {
		return EvaluatorRunDefinition{}, ErrCodeEdgeEvaluatorDefinitionUnavailable
	}
	parentProfile, _, err := service.core.verifyRunManagedExecutionInputs(ctx, parent)
	if err != nil {
		return EvaluatorRunDefinition{}, fmt.Errorf("verify frozen CodeEdge evaluator parent inputs: %w", err)
	}
	if !parentProfile.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		return EvaluatorRunDefinition{}, fmt.Errorf("CodeEdge evaluator parent profile must use %s@%s", workflowadapter.CodeEdgePhase1WorkflowTemplateID, workflowadapter.CodeEdgePhase1WorkflowTemplateVersion)
	}
	definition, err := service.definitions.DefinitionForEvaluatorRun(ctx, EvaluatorRunDefinitionRequest{
		TaskID: parent.TaskID, RevisionID: revision.ID, RevisionDigest: workflowkit.SubjectDigest(revision.TaskDigest), ParentRunID: parent.ID,
		ParentProfile: parentProfile.Clone(),
	})
	if err != nil {
		return EvaluatorRunDefinition{}, ErrCodeEdgeEvaluatorDefinitionInvalid
	}
	return definition, nil
}

func (service *CodeEdgeEvaluatorLaunchService) bindAndValidateDefinition(parent store.WorkflowRun, revision store.TaskRevision, definition EvaluatorRunDefinition, provisionalArtifactID string) (workflowadapter.ExecutionProfile, workflowadapter.RunExecutionSpec, error) {
	if err := store.ValidateUUIDv7(strings.TrimSpace(provisionalArtifactID)); err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("CodeEdge evaluator provisional task artifact ID: %w", err)
	}
	if !definition.Profile.Template.Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) ||
		!definition.ExecutionSpec.Template.Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("CodeEdge evaluator definition must use %s@%s", workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID, workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion)
	}
	placeholder := workflowadapter.ArtifactReference{
		ID:            workflowkit.ArtifactID(provisionalArtifactID),
		ContentDigest: workflowkit.SHA256Fingerprint([]byte("codeedge-evaluator-provisional-task-snapshot:" + revision.TaskDigest)),
		SchemaVersion: workflowadapter.CodeEdgeEvaluatorTaskSnapshotSchemaVersion,
	}
	specification, err := definition.ExecutionSpec.BindManagedArtifactInput(workflowadapter.CodeEdgeEvaluatorTaskSnapshotArtifact, placeholder)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("bind CodeEdge evaluator managed task snapshot: %w", err)
	}
	if err := validateRunExecutionSpecSelection(specification, LifecycleMutationCheckpoint{
		TaskID: parent.TaskID, RevisionID: revision.ID, RevisionDigest: revision.TaskDigest,
	}); err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, err
	}
	template, err := resolveFrozenRunTemplate(definition.Profile, specification)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, err
	}
	if !template.Reference().Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("CodeEdge evaluator definition resolved an unexpected workflow template")
	}
	if _, err := template.Compile(definition.Profile); err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("compile CodeEdge evaluator execution profile: %w", err)
	}
	if err := validateRunExecutionSpecOperationResolver(specification, service.core.operationResolver); err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, err
	}
	if err := service.core.validateDeploymentCatalogExecutionSpec(specification); err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, err
	}
	return definition.Profile.Clone(), specification, nil
}

func (service *CodeEdgeEvaluatorLaunchService) validateFrozenInputs(ctx context.Context, parent store.WorkflowRun, revision store.TaskRevision, command StartRunLifecycleCommand, inputs frozenRunStartInputs) error {
	if inputs.Bundle.Action != LifecycleMutationCodeEdgeEvaluator || inputs.Bundle.ParentRunID != parent.ID || inputs.Bundle.Trigger != codeEdgeEvaluatorRunTrigger {
		return fmt.Errorf("%w: frozen CodeEdge evaluator input bundle does not match the selected parent Run", store.ErrIdempotencyConflict)
	}
	if _, _, err := service.loadApprovedParent(ctx, parent.ID); err != nil {
		return err
	}
	profile, specification, err := service.bindAndValidateDefinition(parent, revision, EvaluatorRunDefinition{Profile: inputs.Profile, ExecutionSpec: inputs.ExecutionSpec}, command.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("validate frozen CodeEdge evaluator definition: %w", err)
	}
	profileFingerprint, err := profile.Fingerprint()
	if err != nil || profileFingerprint != inputs.Bundle.ProfileFingerprint {
		return fmt.Errorf("frozen CodeEdge evaluator execution profile fingerprint does not match input bundle")
	}
	specificationFingerprint, err := specification.Fingerprint()
	if err != nil || specificationFingerprint != inputs.Bundle.ExecutionSpecFingerprint {
		return fmt.Errorf("frozen CodeEdge evaluator execution specification fingerprint does not match input bundle")
	}
	return nil
}

func codeEdgeEvaluatorRunReceipt(run store.WorkflowRun) LifecycleMutationReceipt {
	receipt := receiptForRun(LifecycleMutationCodeEdgeEvaluator, run)
	receipt.ParentRunID = run.ParentRunID
	receipt.Summary = "CodeEdge 评测 child Run 已冻结并入队"
	return receipt
}
