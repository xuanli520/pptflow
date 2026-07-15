package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// ExecutionControlService is the sole application boundary for target-scoped
// pause, stage cancellation, and run termination. It never cancels a caller's
// process context; workers consume the durable control operation through the
// V5 outbox and acknowledge it with runtime receipts.
type ExecutionControlService struct{ core *lifecycleServiceCore }

// ControlCheckpoint is the optimistic identity a client reads before asking
// to control a run. It prevents a delayed UI or CLI request from affecting a
// newer execution epoch, revision, task version, or workflow definition.
type ControlCheckpoint = store.ControlCheckpointRef

type RequestExecutionControlRequest struct {
	ID             string
	OperationKey   string
	Action         store.ControlAction
	RunID          string
	StageAttemptID string
	Expected       ControlCheckpoint
	Actor          string
	Reason         string
}

type TransitionExecutionControlRequest struct {
	ID                string
	OperationID       string
	ExpectedVersion   int64
	Status            store.ControlOperationStatus
	RuntimeReceipts   []store.RuntimeTerminationReceipt
	CheckpointID      string
	QuotaSettlementID string
	FailureReason     string
	Actor             string
	Reason            string
}

// CurrentCheckpoint reads the exact, currently authoritative checkpoint for
// a Run. It is intentionally separate from Request so UI/CLI clients can show
// what they are targeting and so a stale request is rejected rather than
// silently rebound to later work.
func (service *ExecutionControlService) CurrentCheckpoint(ctx context.Context, runID string) (ControlCheckpoint, error) {
	if service == nil || service.core == nil {
		return ControlCheckpoint{}, fmt.Errorf("execution control service is not configured")
	}
	run, err := service.core.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return ControlCheckpoint{}, err
	}
	if run == nil {
		return ControlCheckpoint{}, fmt.Errorf("%w: run %s", ErrLifecycleNotFound, runID)
	}
	subject, err := service.core.resolveWorkflowRunSubject(ctx, *run)
	if err != nil {
		return ControlCheckpoint{}, err
	}
	checkpoint := ControlCheckpoint{
		Sequence:            uint64(run.Version),
		ExecutionEpoch:      run.ExecutionEpoch,
		SubjectID:           subject.Binding.SubjectID,
		SubjectRevisionID:   subject.Binding.RevisionID,
		SubjectDigest:       string(subject.Binding.Digest),
		WorkflowFingerprint: run.DefinitionHash,
	}
	switch {
	case subject.isTaskRevision() && subject.Task != nil:
		checkpoint.SubjectVersion = subject.Task.Version
	case subject.isAuthoringSession():
		checkpoint.SubjectVersion = store.AuthoringSessionControlSubjectVersion
	default:
		return ControlCheckpoint{}, fmt.Errorf("workflow Run %s has no supported control subject", run.ID)
	}
	return checkpoint, nil
}

// Request persists an idempotent, target-scoped control command. The V5 store
// atomically validates Expected and transitions the run into its requested
// state where applicable, so no in-memory scheduler context is authoritative.
func (service *ExecutionControlService) Request(ctx context.Context, request RequestExecutionControlRequest) (store.DurableControlOperation, error) {
	if service == nil || service.core == nil {
		return store.DurableControlOperation{}, fmt.Errorf("execution control service is not configured")
	}
	if strings.TrimSpace(request.OperationKey) == "" {
		return store.DurableControlOperation{}, fmt.Errorf("execution control operation key is required")
	}
	if strings.TrimSpace(request.Actor) == "" || strings.TrimSpace(request.Reason) == "" {
		return store.DurableControlOperation{}, fmt.Errorf("execution control actor and reason are required")
	}
	if request.Expected.Sequence == 0 {
		return store.DurableControlOperation{}, fmt.Errorf("execution control checkpoint sequence is required")
	}
	gracePeriod, err := service.FrozenGracePeriod(ctx, request.RunID)
	if err != nil {
		return store.DurableControlOperation{}, err
	}
	if request.Action == store.ControlActionCancelStage {
		if err := service.validateStageCancellation(ctx, request.RunID, request.StageAttemptID); err != nil {
			return store.DurableControlOperation{}, err
		}
	}
	return service.core.store.CreateExecutionControlOperation(ctx, store.ExecutionControlCommand{
		ID:             request.ID,
		OperationKey:   request.OperationKey,
		Action:         request.Action,
		RunID:          request.RunID,
		StageAttemptID: request.StageAttemptID,
		Expected:       request.Expected,
		Actor:          request.Actor,
		Reason:         request.Reason,
		GracePeriod:    gracePeriod,
	})
}

// FrozenGracePeriod reads the grace policy frozen into the immutable run
// manifest. Callers can display it, but cannot override it for one control
// command, so a retry cannot change termination semantics mid-run.
func (service *ExecutionControlService) FrozenGracePeriod(ctx context.Context, runID string) (time.Duration, error) {
	if service == nil || service.core == nil {
		return 0, fmt.Errorf("execution control service is not configured")
	}
	run, err := service.core.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return 0, err
	}
	if run == nil {
		return 0, fmt.Errorf("%w: run %s", ErrLifecycleNotFound, runID)
	}
	frozen, err := decodeFrozenRunDefinition(*run)
	if err != nil {
		return 0, err
	}
	return frozen.ControlGracePeriod, nil
}

// validateStageCancellation binds a cancellation request to the exact
// descriptor frozen into the target Run. A UI can describe a missing
// capability, but the application boundary must enforce it so CLI/API callers
// cannot cancel a stage whose provider never declared that operation.
func (service *ExecutionControlService) validateStageCancellation(ctx context.Context, runID, stageAttemptID string) error {
	run, err := service.core.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("%w: run %s", ErrLifecycleNotFound, runID)
	}
	stage, err := service.core.store.GetStageAttempt(ctx, stageAttemptID)
	if err != nil {
		return err
	}
	if stage == nil {
		return fmt.Errorf("%w: stage attempt %s", ErrLifecycleNotFound, stageAttemptID)
	}
	if stage.RunID != run.ID {
		return fmt.Errorf("stage attempt %s does not belong to run %s", stage.ID, run.ID)
	}
	workflow, err := decodeFrozenWorkflow(*run)
	if err != nil {
		return err
	}
	descriptor, found := workflow.Stage(workflowkit.StageKey(stage.StageKey))
	if !found {
		return fmt.Errorf("frozen workflow does not define stage %q", stage.StageKey)
	}
	if !descriptor.Capabilities.Has(workflowkit.CapabilityCancel) {
		return fmt.Errorf("stage %q does not declare cancel capability", stage.StageKey)
	}
	return nil
}

func (service *ExecutionControlService) Get(ctx context.Context, operationID string) (store.DurableControlOperation, error) {
	if service == nil || service.core == nil {
		return store.DurableControlOperation{}, fmt.Errorf("execution control service is not configured")
	}
	operation, err := service.core.store.GetExecutionControlOperation(ctx, operationID)
	if err != nil {
		return store.DurableControlOperation{}, err
	}
	if operation == nil {
		return store.DurableControlOperation{}, fmt.Errorf("%w: control operation %s", ErrLifecycleNotFound, operationID)
	}
	return *operation, nil
}

// ListForRun exposes durable control facts for a Run in newest-first order.
// It is intentionally read-only so UI adapters can render request/acknowledge
// state without gaining a path to mutate a worker or process context.
func (service *ExecutionControlService) ListForRun(ctx context.Context, runID string) ([]store.DurableControlOperation, error) {
	if service == nil || service.core == nil {
		return nil, fmt.Errorf("execution control service is not configured")
	}
	if err := store.ValidateUUIDv7(runID); err != nil {
		return nil, err
	}
	return service.core.store.ListExecutionControlOperationsForRun(ctx, runID)
}

// Transition records a worker acknowledgement, failure, or reconcile state.
// Runtime receipts and quota settlement are immutable facts supplied by the
// controlled worker or reconciler, never inferred from a TUI keypress.
func (service *ExecutionControlService) Transition(ctx context.Context, request TransitionExecutionControlRequest) (store.DurableControlOperation, error) {
	if service == nil || service.core == nil {
		return store.DurableControlOperation{}, fmt.Errorf("execution control service is not configured")
	}
	return service.core.store.TransitionExecutionControlOperation(ctx, store.TransitionControlOperationRequest{
		ID:                request.ID,
		OperationID:       request.OperationID,
		ExpectedVersion:   request.ExpectedVersion,
		Status:            request.Status,
		RuntimeReceipts:   append([]store.RuntimeTerminationReceipt(nil), request.RuntimeReceipts...),
		CheckpointID:      request.CheckpointID,
		QuotaSettlementID: request.QuotaSettlementID,
		FailureReason:     request.FailureReason,
		Actor:             request.Actor,
		Reason:            request.Reason,
	})
}
