package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const frozenStageExecutionPayloadFormat = "harbor.stage-attempt-execution.v1"

var (
	// ErrFrozenExecutionRuntimeConfiguration marks an incomplete controlled
	// runtime. In particular, accepting an unversioned plugin resolver would
	// undermine the immutable binding carried by every Run manifest.
	ErrFrozenExecutionRuntimeConfiguration = errors.New("v2 executor: invalid frozen execution runtime configuration")
	// ErrFrozenExecutionPayload marks a durable payload that does not match the
	// run, plan, or policy it claims to execute. It is intentionally distinct
	// from a provider failure: callers must reconcile a tampered payload rather
	// than retry it as useful work.
	ErrFrozenExecutionPayload = errors.New("v2 executor: invalid frozen execution payload")
	// errFrozenRunIntegrity distinguishes persisted Run integrity failures from
	// a durable Job payload that merely fails to bind the Run it names.
	errFrozenRunIntegrity = errors.New("v2 executor: frozen run integrity failure")
	// errRequiredContinuationInputDrift identifies an input that was frozen into
	// a committed continuation plan but can no longer be proved at execution.
	errRequiredContinuationInputDrift = errors.New("v2 executor: required continuation input drift")
)

// FrozenExecutionRuntimeConfig supplies the controlled implementations that
// may execute V2 stage descriptors. Registrations are resolved by the exact
// plugin ID/version frozen into a Run; the runtime never asks the legacy
// Runner, TaskScheduler, or current template for a fallback implementation.
type FrozenExecutionRuntimeConfig struct {
	Services *LifecycleServices
	// WorkflowkitRegistry is the production execution registry. Its executors
	// receive only workflowkit's generic frozen claim contract; Harbor's store,
	// artifacts, quota, and review projection remain behind the durable backend
	// bridge below.
	WorkflowkitRegistry *workflowkit.ControlledPluginRegistry[workflowkit.StageExecutor]
	QuotaLeaseTTL       time.Duration
	ControlPollInterval time.Duration
}

// FrozenExecutionRuntime is the durable coordinator plus StageAttempt worker
// handler. Coordinator jobs expand only their immutable schedule. Each actual
// execution belongs to a separately claimed stage_attempt.execute job.
type FrozenExecutionRuntime struct {
	core                 *lifecycleServiceCore
	services             *LifecycleServices
	workflowkitRegistry  *workflowkit.ControlledPluginRegistry[workflowkit.StageExecutor]
	quotaLeaseTTL        time.Duration
	controlPollInterval  time.Duration
	heartbeatQuotaLeases func(context.Context, []store.HeartbeatQuotaLeaseRequest) ([]store.DurableQuotaLease, error)
}

// NewFrozenExecutionRuntime constructs the V2-only durable job handler.
func NewFrozenExecutionRuntime(config FrozenExecutionRuntimeConfig) (*FrozenExecutionRuntime, error) {
	if config.Services == nil || config.Services.core == nil || config.Services.core.store == nil {
		return nil, fmt.Errorf("%w: lifecycle services are required", ErrFrozenExecutionRuntimeConfiguration)
	}
	if config.WorkflowkitRegistry == nil {
		return nil, fmt.Errorf("%w: controlled stage registry is required", ErrFrozenExecutionRuntimeConfiguration)
	}
	if config.QuotaLeaseTTL == 0 {
		config.QuotaLeaseTTL = store.DefaultLeaseTTL
	}
	if config.ControlPollInterval == 0 {
		config.ControlPollInterval = 100 * time.Millisecond
	}
	if config.QuotaLeaseTTL <= 0 || config.ControlPollInterval <= 0 {
		return nil, fmt.Errorf("%w: quota lease TTL and control poll interval must be positive", ErrFrozenExecutionRuntimeConfiguration)
	}
	return &FrozenExecutionRuntime{
		core:                 config.Services.core,
		services:             config.Services,
		workflowkitRegistry:  config.WorkflowkitRegistry,
		quotaLeaseTTL:        config.QuotaLeaseTTL,
		controlPollInterval:  config.ControlPollInterval,
		heartbeatQuotaLeases: config.Services.core.store.HeartbeatQuotaLeases,
	}, nil
}

var _ DurableJobHandler = (*FrozenExecutionRuntime)(nil)
var _ DurableJobRecoveryHandler = (*FrozenExecutionRuntime)(nil)

// HandleDurableJob dispatches immutable V2 execution payloads and returns the
// terminal durable delivery fact. The handler, rather than DurableWorker,
// owns the diagnostic classification for every failed or in-doubt result.
func (runtime *FrozenExecutionRuntime) HandleDurableJob(ctx context.Context, execution DurableJobExecution) (DurableJobResult, error) {
	state, err := runtime.handleDurableJobState(ctx, execution)
	job := store.DurableJob{}
	if execution.Claim.Job != nil {
		job = *execution.Claim.Job
	}
	result := durableJobResultForOutcome(job, state, err)
	return result, durableJobHandlerError(result, err)
}

// handleDurableJobState retains the execution dispatch boundary for the
// individual runtime handlers. Its result is converted into an explicit
// DurableJobResult by HandleDurableJob above.
func (runtime *FrozenExecutionRuntime) handleDurableJobState(ctx context.Context, execution DurableJobExecution) (store.JobState, error) {
	if err := runtime.validate(); err != nil {
		return store.JobFailed, err
	}
	if execution.Claim.Job == nil {
		return store.JobFailed, fmt.Errorf("%w: durable claim has no job", ErrFrozenExecutionPayload)
	}
	if channelClosed(execution.LeaseLost) {
		return store.JobInDoubt, ErrDurableJobLeaseLost
	}
	job := *execution.Claim.Job
	switch job.CommandType {
	case "workflow_run.execute":
		return runtime.handleWorkflowRun(ctx, execution, job)
	case "task_continuation.execute":
		return runtime.handleContinuation(ctx, execution, job)
	case "stage_attempt.execute":
		return runtime.handleStageAttempt(ctx, execution, job)
	case codeEdgeEvaluatorReconciliationCommandType:
		return runtime.handleCodeEdgeEvaluatorReconciliation(ctx, execution, job)
	case repairSessionAdvanceCommandType:
		return runtime.handleRepairSessionAdvance(ctx, job)
	case store.ReviewGateResolutionCommandType:
		return runtime.handleReviewGateResolution(ctx, execution, job)
	case store.AuthoringReviewGateResolutionCommandType:
		return runtime.handleAuthoringReviewGateResolution(ctx, execution, job)
	default:
		return store.JobFailed, fmt.Errorf("%w: unsupported command type %q", ErrFrozenExecutionPayload, job.CommandType)
	}
}

// ReconcileDurableJobRecoveries repairs only projections whose predecessor
// facts are already final. It never retries a recovered execution job: an
// interrupted external effect remains explicitly reconcile-required.
//
// A worker can crash after a StageAttempt has committed its terminal result but
// before the handler has projected the Run outcome or created the next
// coordinator. The generic dispatcher correctly fences that job as interrupted
// on lease expiry; this method uses the final StageAttempt, not the interrupted
// delivery job, to restore the missing deterministic terminal projection.
func (runtime *FrozenExecutionRuntime) ReconcileDurableJobRecoveries(ctx context.Context, request DurableJobRecoveryRequest) error {
	if err := runtime.validateRecovery(); err != nil {
		return err
	}
	recoveries, err := runtime.rebuildDurableJobRecoveries(ctx, request)
	if err != nil {
		return err
	}
	for _, recovery := range recoveries {
		job := recovery.Job
		if len(recovery.Operations) != 0 {
			// The store has marked a control operation for explicit
			// reconciliation. Do not schedule past an unresolved pause,
			// cancellation, or termination request.
			continue
		}
		switch job.CommandType {
		case "stage_attempt.execute":
			if err := runtime.reconcileRecoveredStageJob(ctx, job); err != nil {
				return err
			}
		case codeEdgeEvaluatorReconciliationCommandType:
			if err := runtime.reconcileRecoveredCodeEdgeEvaluatorReconciliation(ctx, job); err != nil {
				return err
			}
		case "workflow_run.execute":
			if err := runtime.reconcileRecoveredWorkflowCoordinator(ctx, job); err != nil {
				return err
			}
		case "task_continuation.execute":
			if err := runtime.reconcileRecoveredContinuationCoordinator(ctx, job); err != nil {
				return err
			}
		case repairSessionAdvanceCommandType:
			if err := runtime.reconcileRecoveredRepairSessionAdvance(ctx, job); err != nil {
				return err
			}
		case store.ReviewGateResolutionCommandType:
			if err := runtime.reconcileRecoveredReviewGateResolution(ctx, job); err != nil {
				return err
			}
		case store.AuthoringReviewGateResolutionCommandType:
			if err := runtime.reconcileRecoveredAuthoringReviewGateResolution(ctx, job); err != nil {
				return err
			}
		}
	}
	return nil
}

func (runtime *FrozenExecutionRuntime) validateRecovery() error {
	if runtime == nil || runtime.core == nil || runtime.core.store == nil || runtime.services == nil {
		return ErrFrozenExecutionRuntimeConfiguration
	}
	return nil
}

func (runtime *FrozenExecutionRuntime) rebuildDurableJobRecoveries(ctx context.Context, request DurableJobRecoveryRequest) ([]store.ExpiredDurableJobRecovery, error) {
	jobs, err := runtime.core.store.ListLeaseLostDurableJobsForRecovery(ctx, request.RunID)
	if err != nil {
		return nil, fmt.Errorf("list durable lease-loss recovery facts: %w", err)
	}
	byJob := make(map[string]store.ExpiredDurableJobRecovery, len(request.Recoveries)+len(jobs))
	for _, recovery := range request.Recoveries {
		byJob[recovery.Job.ID] = recovery
	}
	for _, job := range jobs {
		if _, exists := byJob[job.ID]; exists {
			continue
		}
		byJob[job.ID] = store.ExpiredDurableJobRecovery{Job: job}
	}
	recoveries := make([]store.ExpiredDurableJobRecovery, 0, len(byJob))
	for _, recovery := range byJob {
		recoveries = append(recoveries, recovery)
	}
	sort.Slice(recoveries, func(left, right int) bool {
		if !recoveries[left].Job.UpdatedAt.Equal(recoveries[right].Job.UpdatedAt) {
			return recoveries[left].Job.UpdatedAt.Before(recoveries[right].Job.UpdatedAt)
		}
		return recoveries[left].Job.ID < recoveries[right].Job.ID
	})
	return recoveries, nil
}

func (runtime *FrozenExecutionRuntime) reconcileRecoveredStageJob(ctx context.Context, job store.DurableJob) error {
	var payload frozenStageExecutionPayload
	if err := decodeStrictJSON(job.PayloadJSON, &payload); err != nil {
		_, projected := runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: decode recovered stage execution payload: %w", ErrFrozenExecutionPayload, err))
		return projected
	}
	if payload.Format != frozenStageExecutionPayloadFormat || payload.RunID != job.RunID || payload.StageAttemptID != job.StageAttemptID || payload.StageAttemptID != job.EntityID || payload.Generation < 0 {
		_, projected := runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: recovered stage job does not bind its payload", ErrFrozenExecutionPayload))
		return projected
	}
	run, frozen, err := runtime.loadFrozenRun(ctx, payload.RunID, payload.DefinitionHash, "", payload.QuotaPolicy)
	if err != nil {
		_, projected := runtime.failRuntimeJob(ctx, job, err)
		return projected
	}
	stage, found := frozen.Workflow.Stage(payload.StageKey)
	if !found {
		_, projected := runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: frozen workflow omits recovered stage %q", ErrFrozenExecutionPayload, payload.StageKey))
		return projected
	}
	if !stage.AutomaticallyDispatchable() {
		_, projected := runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: recovered job targets operator-only stage %q", ErrFrozenExecutionPayload, stage.Key))
		return projected
	}
	attempt, err := runtime.core.store.GetStageAttempt(ctx, payload.StageAttemptID)
	if err != nil {
		return err
	}
	if attempt == nil || attempt.RunID != run.ID || attempt.StageKey != string(stage.Key) {
		_, projected := runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: recovered stage attempt does not match durable job", ErrFrozenExecutionPayload))
		return projected
	}
	if err := runtime.validateStageAttemptPlanBinding(*attempt, payload); err != nil {
		_, projected := runtime.failRuntimeJob(ctx, job, err)
		return projected
	}
	if isCodeEdgeEvaluatorStage(run, stage) {
		handled, reconcileErr := runtime.reconcileRecoveredCodeEdgeEvaluatorStage(ctx, job, run, frozen, payload, *attempt, stage)
		if reconcileErr != nil {
			return reconcileErr
		}
		currentRun, currentRunErr := runtime.core.store.GetWorkflowRun(ctx, run.ID)
		if currentRunErr != nil {
			return currentRunErr
		}
		if currentRun == nil {
			_, projected := runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: recovered CodeEdge evaluator Run disappeared", ErrLifecycleNotFound))
			return projected
		}
		run = *currentRun
		if handled {
			return nil
		}
	}
	if attempt.ExecutionStatus == store.StageExecutionRunning || attempt.ExecutionStatus == store.StageExecutionInDoubt || attempt.ExecutionStatus == store.StageExecutionReconciling {
		return runtime.reconcileLeaseLostStageAttempt(ctx, job, run, payload, *attempt)
	}
	if attempt.ExecutionStatus == store.StageExecutionInterrupted && run.Status == store.WorkflowRunFailedRecoverable && job.Failure != nil && job.Failure.Code == "job.lease_lost" {
		return nil
	}
	if run.Status != store.WorkflowRunRunning {
		return runtime.enqueueAutomaticRepairOutcome(ctx, run.ID, job.CreatedBy, "recover repair-loop handoff after terminal stage projection")
	}
	if !recoverableStageTerminalStatus(attempt.ExecutionStatus) {
		return nil
	}
	if _, err := runtime.afterStageTerminal(ctx, job, run, frozen, payload, stage, *attempt, nil, "", nil); err != nil {
		_, projected := runtime.failRuntimeJob(ctx, job, fmt.Errorf("restore terminal projection after recovered stage %s: %w", attempt.ID, err))
		return projected
	}
	return nil
}

func (runtime *FrozenExecutionRuntime) reconcileLeaseLostStageAttempt(ctx context.Context, job store.DurableJob, run store.WorkflowRun, payload frozenStageExecutionPayload, attempt store.StageAttempt) error {
	if job.State != store.JobInDoubt || job.Failure == nil || job.Failure.Code != "job.lease_lost" {
		return nil
	}
	controls, err := runtime.core.store.ListExecutionControlOperationsForRun(ctx, run.ID)
	if err != nil {
		return err
	}
	for _, operation := range controls {
		if operation.Status == store.ControlOperationReconcileRequired {
			return nil
		}
	}
	references, err := runtime.core.store.ListArtifactRefsForAttempt(ctx, attempt.ID)
	if err != nil {
		return err
	}
	effects, err := runtime.core.store.ListSideEffectOperationsForStageAttempt(ctx, attempt.ID)
	if err != nil {
		return err
	}
	unknownOutcome := attempt.ArtifactManifestID != "" || len(references) != 0 || leaseLostStageHasUnknownEffect(effects)
	if err := runtime.reconcileLeaseLostStageQuota(ctx, job, !unknownOutcome); err != nil {
		return err
	}
	if unknownOutcome {
		return runtime.projectLeaseLostStageInDoubt(ctx, job, attempt)
	}
	leaseExpiredAt := job.UpdatedAt
	if job.FinishedAt != nil {
		leaseExpiredAt = job.FinishedAt.UTC()
	}
	decision, err := workflowkit.DecideRecovery(workflowkit.RecoverySubject{
		SubjectID: attempt.ID, Status: workflowkit.StatusRunning, LeaseExpiresAt: leaseExpiredAt,
		ObservedAt: runtime.core.now().UTC(), InputsUnchanged: true, DefinitionUnchanged: true,
	})
	if err != nil {
		return err
	}
	if decision.Action != workflowkit.RecoveryMarkInterrupted {
		return nil
	}
	if err := runtime.interruptLeaseLostNodes(ctx, job, attempt.ID); err != nil {
		return err
	}
	current, err := runtime.core.store.GetStageAttempt(ctx, attempt.ID)
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("%w: recovered stage attempt %s", ErrLifecycleNotFound, attempt.ID)
	}
	if current.ExecutionStatus == store.StageExecutionRunning {
		transitioned, err := runtime.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
			StageAttemptID: current.ID, ExpectedVersion: current.Version, ExecutionStatus: store.StageExecutionInDoubt,
			ErrorText: "dispatch lease expired before the stage outcome was recorded", FailureClass: string(workflowkit.FailureUnknown),
			Actor: job.CreatedBy, Reason: "reconcile expired durable stage dispatch",
		})
		if err != nil {
			return err
		}
		current = &transitioned
	}
	if current.ExecutionStatus == store.StageExecutionInDoubt {
		transitioned, err := runtime.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
			StageAttemptID: current.ID, ExpectedVersion: current.Version, ExecutionStatus: store.StageExecutionReconciling,
			Actor: job.CreatedBy, Reason: "apply durable stage lease-loss recovery",
		})
		if err != nil {
			return err
		}
		current = &transitioned
	}
	if current.ExecutionStatus == store.StageExecutionReconciling {
		if _, err := runtime.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
			StageAttemptID: current.ID, ExpectedVersion: current.Version, ExecutionStatus: store.StageExecutionInterrupted,
			ErrorText: "dispatch lease expired before the stage outcome was recorded",
			Actor:     job.CreatedBy, Reason: "complete durable stage lease-loss recovery",
		}); err != nil {
			return err
		}
	}
	if err := runtime.finishRunWithStatus(ctx, run.ID, store.WorkflowRunFailedRecoverable, job.CreatedBy, "stage dispatch lease loss is recoverable"); err != nil {
		return err
	}
	_, err = runtime.finishContinuationForRunOutcome(ctx, payload.ContinuationExecutionID, store.ContinuationExecutionFailed, job.CreatedBy, "continuation stage dispatch lease was lost")
	return err
}

func leaseLostStageHasUnknownEffect(operations []store.SideEffectOperation) bool {
	for _, operation := range operations {
		switch operation.State {
		case store.SideEffectStarted, store.SideEffectUnknown, store.SideEffectSucceeded:
			return true
		}
	}
	return false
}

func (runtime *FrozenExecutionRuntime) reconcileLeaseLostStageQuota(ctx context.Context, job store.DurableJob, canceled bool) error {
	decision, err := runtime.core.store.GetDurableAdmissionDecisionByIdempotencyKey(ctx, "stage-admission:"+job.ID)
	if err != nil || decision == nil {
		return err
	}
	leases := append([]store.DurableQuotaLease(nil), decision.Leases...)
	sort.Slice(leases, func(left, right int) bool { return leases[left].ID < leases[right].ID })
	for pass := 0; pass <= len(leases); pass++ {
		active := make([]store.SettleQuotaLeaseRequest, 0, len(leases))
		for _, lease := range leases {
			current, err := runtime.core.store.GetDurableQuotaLease(ctx, lease.ID)
			if err != nil {
				return err
			}
			if current == nil {
				return fmt.Errorf("%w: quota lease %s", ErrLifecycleNotFound, lease.ID)
			}
			if current.State == store.DurableQuotaLeaseActive {
				active = append(active, store.SettleQuotaLeaseRequest{
					IdempotencyKey: "stage-lease-loss-uncertain:" + job.ID + ":" + current.ID, LeaseID: current.ID,
					Owner: current.Owner, FencingToken: current.FencingToken, Outcome: store.QuotaSettlementUncertain,
					Actor: job.CreatedBy, Reason: "dispatch lease loss requires stage quota reconciliation",
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
	if !canceled {
		return nil
	}
	reconciliations := make([]store.ReconcileQuotaLeaseRequest, 0, len(leases))
	for _, lease := range leases {
		current, err := runtime.core.store.GetDurableQuotaLease(ctx, lease.ID)
		if err != nil {
			return err
		}
		if current == nil {
			return fmt.Errorf("%w: quota lease %s", ErrLifecycleNotFound, lease.ID)
		}
		if current.State == store.DurableQuotaLeaseUncertain || current.State == store.DurableQuotaLeaseExpired {
			reconciliations = append(reconciliations, store.ReconcileQuotaLeaseRequest{
				IdempotencyKey: "stage-lease-loss-canceled:" + job.ID + ":" + current.ID, LeaseID: current.ID,
				Owner: current.Owner, FencingToken: current.FencingToken, Outcome: store.QuotaSettlementCanceled,
				Actor: job.CreatedBy, Reason: "cancel incomplete stage quota after dispatch lease loss",
			})
		}
	}
	if len(reconciliations) != 0 {
		_, err = runtime.core.store.ReconcileQuotaLeases(ctx, reconciliations)
	}
	return err
}

func (runtime *FrozenExecutionRuntime) projectLeaseLostStageInDoubt(ctx context.Context, job store.DurableJob, attempt store.StageAttempt) error {
	if err := runtime.markLeaseLostNodesInDoubt(ctx, job, attempt.ID); err != nil {
		return err
	}
	current, err := runtime.core.store.GetStageAttempt(ctx, attempt.ID)
	if err != nil || current == nil {
		return err
	}
	if current.ExecutionStatus == store.StageExecutionRunning {
		if _, err = runtime.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
			StageAttemptID: current.ID, ExpectedVersion: current.Version, ExecutionStatus: store.StageExecutionInDoubt,
			ErrorText: "dispatch lease expired with an unconfirmed durable outcome", FailureClass: string(workflowkit.FailureUnknown),
			Actor: job.CreatedBy, Reason: "retain unknown stage outcome for domain reconciliation",
		}); err != nil {
			return err
		}
	}
	return runtime.finishRunWithStatus(ctx, attempt.RunID, store.WorkflowRunInDoubt, job.CreatedBy, "stage dispatch lease loss has an unconfirmed durable outcome")
}

func (runtime *FrozenExecutionRuntime) interruptLeaseLostNodes(ctx context.Context, job store.DurableJob, stageAttemptID string) error {
	return runtime.transitionLeaseLostNodes(ctx, job, stageAttemptID, store.NodeAttemptInterrupted)
}

func (runtime *FrozenExecutionRuntime) markLeaseLostNodesInDoubt(ctx context.Context, job store.DurableJob, stageAttemptID string) error {
	return runtime.transitionLeaseLostNodes(ctx, job, stageAttemptID, store.NodeAttemptInDoubt)
}

func (runtime *FrozenExecutionRuntime) transitionLeaseLostNodes(ctx context.Context, job store.DurableJob, stageAttemptID string, target store.NodeAttemptStatus) error {
	nodes, err := runtime.core.store.ListNodeAttempts(ctx, stageAttemptID)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		switch node.Status {
		case store.NodeAttemptQueued, store.NodeAttemptRunning, store.NodeAttemptWaiting, store.NodeAttemptInDoubt:
		default:
			continue
		}
		if node.Status == target {
			continue
		}
		if _, err := runtime.core.store.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
			NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: target,
			ErrorText: "dispatch lease expired before the node outcome was recorded",
			Actor:     job.CreatedBy, Reason: "reconcile durable node dispatch lease loss",
		}); err != nil {
			return err
		}
	}
	return nil
}

func recoverableStageTerminalStatus(status store.StageExecutionStatus) bool {
	switch status {
	case store.StageExecutionCompleted, store.StageExecutionInfraFailed, store.StageExecutionInterrupted, store.StageExecutionCanceled:
		return true
	default:
		return false
	}
}

func (runtime *FrozenExecutionRuntime) reconcileRecoveredWorkflowCoordinator(ctx context.Context, job store.DurableJob) error {
	var payload workflowRunExecutionPayload
	if err := decodeStrictJSON(job.PayloadJSON, &payload); err != nil {
		_, projected := runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: decode recovered workflow coordinator payload: %w", ErrFrozenExecutionPayload, err))
		return projected
	}
	if err := validateWorkflowRunExecutionPayload(payload, job); err != nil {
		_, projected := runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: recovered workflow coordinator does not bind its payload", ErrFrozenExecutionPayload))
		return projected
	}
	run, _, err := runtime.loadFrozenRun(ctx, payload.RunID, payload.DefinitionHash, payload.ExecutionSpecFingerprint, payload.QuotaPolicy)
	if err != nil {
		_, projected := runtime.failRuntimeJob(ctx, job, err)
		return projected
	}
	if !requeueableCoordinatorRunStatus(run.Status) {
		return runtime.enqueueAutomaticRepairOutcome(ctx, run.ID, job.CreatedBy, "recover repair-loop handoff after terminal workflow coordinator")
	}
	_, err = runtime.core.store.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "workflow_run.execute", EntityType: "workflow_run", EntityID: run.ID, RunID: run.ID,
		PayloadJSON: job.PayloadJSON, IdempotencyKey: "workflow-run-recover:" + run.ID + ":" + job.ID,
		Actor: job.CreatedBy, Reason: "recover frozen workflow coordinator after expired worker lease",
	})
	return err
}

func (runtime *FrozenExecutionRuntime) reconcileRecoveredContinuationCoordinator(ctx context.Context, job store.DurableJob) error {
	var payload continuationExecutionPayload
	if err := decodeStrictJSON(job.PayloadJSON, &payload); err != nil {
		_, projected := runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: decode recovered continuation coordinator payload: %w", ErrFrozenExecutionPayload, err))
		return projected
	}
	if payload.Format != continuationExecutionFormat || payload.RunID != job.RunID || job.EntityType != "continuation_execution" || payload.PlanID == "" {
		_, projected := runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: recovered continuation coordinator does not bind its payload", ErrFrozenExecutionPayload))
		return projected
	}
	execution, err := runtime.core.store.GetContinuationExecution(ctx, job.EntityID)
	if err != nil {
		return err
	}
	if execution == nil || execution.RunID != job.RunID || execution.PlanID != payload.PlanID || execution.PayloadJSON != job.PayloadJSON {
		_, projected := runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: recovered continuation execution does not match durable job", ErrFrozenExecutionPayload))
		return projected
	}
	run, _, err := runtime.loadFrozenRun(ctx, payload.RunID, "", "", payload.QuotaPolicy)
	if err != nil {
		_, projected := runtime.failRuntimeJob(ctx, job, err)
		return projected
	}
	if !requeueableCoordinatorRunStatus(run.Status) || (execution.State != store.ContinuationExecutionQueued && execution.State != store.ContinuationExecutionRunning) {
		return runtime.enqueueAutomaticRepairOutcome(ctx, run.ID, job.CreatedBy, "recover repair-loop handoff after terminal continuation coordinator")
	}
	_, err = runtime.core.store.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "task_continuation.execute", EntityType: "continuation_execution", EntityID: execution.ID, RunID: run.ID,
		PayloadJSON: job.PayloadJSON, IdempotencyKey: "continuation-recover:" + execution.ID + ":" + job.ID,
		Actor: job.CreatedBy, Reason: "recover frozen continuation coordinator after expired worker lease",
	})
	return err
}

func requeueableCoordinatorRunStatus(status store.WorkflowRunStatus) bool {
	switch status {
	case store.WorkflowRunQueued, store.WorkflowRunRunning, store.WorkflowRunResumeRequested:
		return true
	default:
		return false
	}
}

func (runtime *FrozenExecutionRuntime) validate() error {
	if runtime == nil || runtime.core == nil || runtime.core.store == nil || runtime.services == nil || runtime.workflowkitRegistry == nil {
		return ErrFrozenExecutionRuntimeConfiguration
	}
	return nil
}

// frozenStageExecutionPayload duplicates the immutable identity facts that a
// worker needs after detaching. It carries no caller-configurable budget,
// plugin, or quota values: those are verified against the run manifest before
// the stage begins.
type frozenStageExecutionPayload struct {
	Format                  string                              `json:"format"`
	RunID                   string                              `json:"run_id"`
	StageAttemptID          string                              `json:"stage_attempt_id"`
	StageKey                workflowkit.StageKey                `json:"stage_key"`
	DefinitionHash          string                              `json:"definition_hash"`
	Generation              int                                 `json:"generation"`
	ContinuationExecutionID string                              `json:"continuation_execution_id,omitempty"`
	ContinuationPlanID      string                              `json:"continuation_plan_id,omitempty"`
	QuotaPolicy             workflowadapter.ResolvedQuotaPolicy `json:"quota_policy"`
}

type runtimeStageAttemptSnapshot struct {
	Format                  string                  `json:"format"`
	ExecutionKey            string                  `json:"execution_key"`
	ContinuationExecutionID string                  `json:"continuation_execution_id,omitempty"`
	ContinuationPlanID      string                  `json:"continuation_plan_id,omitempty"`
	Generation              int                     `json:"generation"`
	Retry                   workflowkit.RetryPolicy `json:"retry"`
}

const runtimeStageAttemptSnapshotFormat = "harbor.stage-attempt-snapshot.v1"

type runtimeExecutionPlan struct {
	ExecutionKey            string
	ContinuationExecutionID string
	ContinuationPlanID      string
	Workflow                workflowkit.WorkflowDescriptor
	Transitions             map[workflowkit.StageKey]workflowkit.NodeTransition
	Schedule                []workflowkit.ScheduleBatch
	// InitialExecutionPlan is present only for a newly-started Run. A
	// continuation freezes a subset schedule in Schedule instead, because its
	// preserved/invalidated nodes must not be admitted as worker jobs.
	InitialExecutionPlan *workflowkit.ExecutionPlan
	QuotaPolicy          workflowadapter.ResolvedQuotaPolicy
}

func (plan runtimeExecutionPlan) stageTransition(key workflowkit.StageKey) (workflowkit.NodeTransition, bool) {
	transition, found := plan.Transitions[key]
	return transition.Clone(), found
}

// frozenCoordinatorSchedule exposes the exact initial or continuation
// schedule to the public kernel claim.  The app keeps only Harbor-specific
// identifiers and quota projection in runtimeExecutionPlan; generic topology
// is no longer hidden from workflowkit during continuation execution.
func (plan runtimeExecutionPlan) frozenCoordinatorSchedule() (workflowkit.FrozenCoordinatorSchedule, error) {
	if err := plan.Workflow.Validate(); err != nil {
		return workflowkit.FrozenCoordinatorSchedule{}, fmt.Errorf("validate frozen coordinator workflow: %w", err)
	}
	if plan.InitialExecutionPlan != nil {
		if err := plan.InitialExecutionPlan.Validate(plan.Workflow); err != nil {
			return workflowkit.FrozenCoordinatorSchedule{}, fmt.Errorf("validate frozen initial coordinator plan: %w", err)
		}
		return workflowkit.FreezeCoordinatorSchedule(plan.Workflow, plan.ExecutionKey, workflowkit.CoordinatorScheduleExecutionPlan, plan.InitialExecutionPlan.Clone(), nil, nil)
	}
	transitions := make([]workflowkit.NodeTransition, 0, len(plan.Workflow.Stages))
	for _, key := range mustTopologicalStageKeys(plan.Workflow) {
		transition, found := plan.stageTransition(key)
		if !found {
			return workflowkit.FrozenCoordinatorSchedule{}, fmt.Errorf("%w: continuation coordinator schedule omits transition for stage %q", ErrFrozenExecutionPayload, key)
		}
		transitions = append(transitions, transition)
	}
	return workflowkit.FreezeCoordinatorSchedule(plan.Workflow, plan.ExecutionKey, workflowkit.CoordinatorScheduleTransitionSubset, workflowkit.ExecutionPlan{}, append([]workflowkit.ScheduleBatch(nil), plan.Schedule...), transitions)
}

func validateWorkflowRunExecutionPayload(payload workflowRunExecutionPayload, job store.DurableJob) error {
	if payload.Format != workflowRunExecutionPayloadFormat || payload.RunID != job.RunID || payload.RunID != job.EntityID {
		return fmt.Errorf("workflow run job does not bind its payload")
	}
	if err := payload.ExecutionSpecFingerprint.Validate(); err != nil {
		return fmt.Errorf("workflow run execution specification fingerprint: %w", err)
	}
	return nil
}

func (runtime *FrozenExecutionRuntime) handleWorkflowRun(ctx context.Context, execution DurableJobExecution, job store.DurableJob) (store.JobState, error) {
	var payload workflowRunExecutionPayload
	if err := decodeStrictJSON(job.PayloadJSON, &payload); err != nil {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: decode workflow run payload: %w", ErrFrozenExecutionPayload, err))
	}
	if err := validateWorkflowRunExecutionPayload(payload, job); err != nil {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: workflow run job does not bind its payload", ErrFrozenExecutionPayload))
	}
	run, frozen, err := runtime.loadFrozenRun(ctx, payload.RunID, payload.DefinitionHash, payload.ExecutionSpecFingerprint, payload.QuotaPolicy)
	if err != nil {
		if errors.Is(err, errFrozenRunIntegrity) {
			if projectionErr := runtime.finishRunWithStatus(ctx, payload.RunID, store.WorkflowRunInDoubt, job.CreatedBy, "frozen workflow Run integrity requires reconciliation"); projectionErr != nil {
				err = fmt.Errorf("%w: project frozen workflow Run integrity failure: %v", err, projectionErr)
			}
		}
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if state, handled, err := runtime.handlePreExecutionControl(ctx, job, run); handled || err != nil {
		return state, err
	}
	if err := runtime.transitionRunToRunning(ctx, run, job.CreatedBy, "begin frozen workflow run"); err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	plan, err := initialRuntimeExecutionPlan(frozen)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if err := runtime.ensureRunAttempt(ctx, run.ID, plan.ExecutionKey, job.CreatedBy, "begin workflow run attempt"); err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	state, coordinatorErr := runtime.executeWorkflowkitCoordinator(ctx, execution, job, run, frozen, plan)
	if coordinatorErr != nil {
		return runtime.failRuntimeJob(ctx, job, coordinatorErr)
	}
	return state, nil
}

func (runtime *FrozenExecutionRuntime) handleContinuation(ctx context.Context, execution DurableJobExecution, job store.DurableJob) (store.JobState, error) {
	var payload continuationExecutionPayload
	if err := decodeStrictJSON(job.PayloadJSON, &payload); err != nil {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: decode continuation execution payload: %w", ErrFrozenExecutionPayload, err))
	}
	if payload.Format != continuationExecutionFormat || payload.RunID != job.RunID || job.EntityType != "continuation_execution" || payload.PlanID == "" {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: continuation job does not bind its payload", ErrFrozenExecutionPayload))
	}
	continuation, err := runtime.core.store.GetContinuationExecution(ctx, job.EntityID)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if continuation == nil || continuation.RunID != job.RunID || continuation.PlanID != payload.PlanID || continuation.PayloadJSON != job.PayloadJSON {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: continuation execution does not match durable job", ErrFrozenExecutionPayload))
	}
	run, frozen, err := runtime.loadFrozenRun(ctx, payload.RunID, "", "", payload.QuotaPolicy)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	plan, err := runtime.services.Continuations.GetTaskContinuationPlan(ctx, payload.PlanID)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if payload.PlanFingerprint != plan.Fingerprint() {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: continuation plan fingerprint mismatch", ErrFrozenExecutionPayload))
	}
	snapshot := plan.Snapshot()
	if snapshot.TargetRunRelation == workflowkit.RelationSameRunAttempt && snapshot.SourceRunID != run.ID {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: same-run continuation source does not match job run", ErrFrozenExecutionPayload))
	}
	if payload.SourceRunID != snapshot.SourceRunID {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: continuation source run mismatch", ErrFrozenExecutionPayload))
	}
	runtimePlan, err := continuationRuntimeExecutionPlan(plan, frozen.Workflow, frozen.QuotaPolicy, continuation.ID)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if err := runtime.validateRemainingRequiredContinuationInputs(ctx, run, runtimePlan); err != nil {
		return runtime.reconcileContinuationInputDrift(ctx, job, *continuation, err)
	}
	if err := runtime.transitionContinuationToRunning(ctx, *continuation, job.CreatedBy); err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if state, handled, err := runtime.handlePreExecutionControl(ctx, job, run); handled || err != nil {
		return state, err
	}
	if err := runtime.transitionRunToRunning(ctx, run, job.CreatedBy, "begin frozen continuation"); err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if err := runtime.ensureRunAttempt(ctx, run.ID, runtimePlan.ExecutionKey, job.CreatedBy, "begin continuation attempt"); err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	state, coordinatorErr := runtime.executeWorkflowkitCoordinator(ctx, execution, job, run, frozen, runtimePlan)
	if coordinatorErr != nil {
		if errors.Is(coordinatorErr, errRequiredContinuationInputDrift) {
			return runtime.reconcileContinuationInputDrift(ctx, job, *continuation, coordinatorErr)
		}
		return runtime.failRuntimeJob(ctx, job, coordinatorErr)
	}
	return state, nil
}

func (runtime *FrozenExecutionRuntime) handleRepairSessionAdvance(ctx context.Context, job store.DurableJob) (store.JobState, error) {
	if runtime.core.repairs == nil {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("repair loop service is not configured"))
	}
	if _, err := runtime.core.repairs.HandleDurableJob(ctx, job); err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	return store.JobSucceeded, nil
}

func (runtime *FrozenExecutionRuntime) reconcileRecoveredRepairSessionAdvance(ctx context.Context, job store.DurableJob) error {
	if runtime.core.repairs == nil {
		return fmt.Errorf("repair loop service is not configured")
	}
	_, err := runtime.core.repairs.HandleDurableJob(ctx, job)
	return err
}

func (runtime *FrozenExecutionRuntime) loadFrozenRun(ctx context.Context, runID, definitionHash string, expectedExecutionSpecFingerprint workflowkit.Fingerprint, payloadPolicy workflowadapter.ResolvedQuotaPolicy) (store.WorkflowRun, frozenRunDefinition, error) {
	run, err := runtime.core.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return store.WorkflowRun{}, frozenRunDefinition{}, err
	}
	if run == nil {
		return store.WorkflowRun{}, frozenRunDefinition{}, fmt.Errorf("%w: workflow run %s", ErrLifecycleNotFound, runID)
	}
	frozen, err := decodeFrozenRunDefinition(*run)
	if err != nil {
		return store.WorkflowRun{}, frozenRunDefinition{}, fmt.Errorf("%w: %w: decode frozen Run definition: %v", ErrFrozenExecutionPayload, errFrozenRunIntegrity, err)
	}
	if _, _, err := runtime.core.verifyRunManagedExecutionInputs(ctx, *run); err != nil {
		return store.WorkflowRun{}, frozenRunDefinition{}, fmt.Errorf("%w: %w: verify frozen managed execution inputs: %w", ErrFrozenExecutionPayload, errFrozenRunIntegrity, err)
	}
	if err := runtime.core.verifyRunDeploymentCatalogReceipt(*run); err != nil {
		return store.WorkflowRun{}, frozenRunDefinition{}, fmt.Errorf("%w: %w: verify frozen deployment catalog receipt: %w", ErrFrozenExecutionPayload, errFrozenRunIntegrity, err)
	}
	if definitionHash != "" && definitionHash != run.DefinitionHash {
		return store.WorkflowRun{}, frozenRunDefinition{}, fmt.Errorf("%w: workflow definition hash mismatch", ErrFrozenExecutionPayload)
	}
	if expectedExecutionSpecFingerprint != "" && expectedExecutionSpecFingerprint != frozen.ExecutionSpecFingerprint {
		return store.WorkflowRun{}, frozenRunDefinition{}, fmt.Errorf("%w: workflow execution specification fingerprint differs from run manifest", ErrFrozenExecutionPayload)
	}
	if err := payloadPolicy.Validate(); err != nil {
		return store.WorkflowRun{}, frozenRunDefinition{}, fmt.Errorf("%w: payload quota policy: %v", ErrFrozenExecutionPayload, err)
	}
	if payloadPolicy.ID != frozen.QuotaPolicy.ID || payloadPolicy.Version != frozen.QuotaPolicy.Version || payloadPolicy.Fingerprint != frozen.QuotaPolicy.Fingerprint {
		return store.WorkflowRun{}, frozenRunDefinition{}, fmt.Errorf("%w: payload quota policy differs from run manifest", ErrFrozenExecutionPayload)
	}
	return *run, frozen, nil
}

func initialRuntimeExecutionPlan(frozen frozenRunDefinition) (runtimeExecutionPlan, error) {
	if err := frozen.Workflow.Validate(); err != nil {
		return runtimeExecutionPlan{}, err
	}
	executionPlan := frozen.InitialExecutionPlan.Clone()
	if err := executionPlan.Validate(frozen.Workflow); err != nil {
		return runtimeExecutionPlan{}, fmt.Errorf("validate frozen initial execution plan: %w", err)
	}
	plan := runtimeExecutionPlan{
		ExecutionKey: "initial",
		Workflow:     frozen.Workflow.Clone(),
		Transitions:  make(map[workflowkit.StageKey]workflowkit.NodeTransition, len(frozen.Workflow.Stages)),
		Schedule:     append([]workflowkit.ScheduleBatch(nil), executionPlan.Batches...),
		QuotaPolicy:  frozen.QuotaPolicy.Clone(),
	}
	initial := executionPlan.Clone()
	plan.InitialExecutionPlan = &initial
	for _, key := range mustTopologicalStageKeys(frozen.Workflow) {
		stage, found := frozen.Workflow.Stage(key)
		if !found {
			return runtimeExecutionPlan{}, fmt.Errorf("%w: frozen workflow omits initial stage %q", ErrFrozenExecutionPayload, key)
		}
		disposition := workflowkit.DispositionSchedule
		if stage.OperatorOnly() {
			disposition = workflowkit.DispositionOperatorOnly
		}
		plan.Transitions[key] = workflowkit.NodeTransition{NodeID: key, FromGeneration: 0, ToGeneration: 0, Disposition: disposition}
	}
	return plan, nil
}

func mustTopologicalStageKeys(workflow workflowkit.WorkflowDescriptor) []workflowkit.StageKey {
	keys, err := workflow.TopologicalStages()
	if err != nil {
		panic(fmt.Sprintf("validated frozen workflow has no topological ordering: %v", err))
	}
	return keys
}

func continuationRuntimeExecutionPlan(plan workflowkit.ContinuationPlan, workflow workflowkit.WorkflowDescriptor, policy workflowadapter.ResolvedQuotaPolicy, executionID string) (runtimeExecutionPlan, error) {
	snapshot := plan.Snapshot()
	if err := plan.Validate(workflow); err != nil {
		return runtimeExecutionPlan{}, fmt.Errorf("validate continuation plan against target frozen workflow: %w", err)
	}
	result := runtimeExecutionPlan{
		ExecutionKey:            snapshot.PlanID,
		ContinuationExecutionID: executionID,
		ContinuationPlanID:      snapshot.PlanID,
		Workflow:                workflow.Clone(),
		Transitions:             make(map[workflowkit.StageKey]workflowkit.NodeTransition, len(snapshot.Nodes)),
		Schedule:                append([]workflowkit.ScheduleBatch(nil), snapshot.Schedule...),
		QuotaPolicy:             policy.Clone(),
	}
	for _, transition := range snapshot.Nodes {
		result.Transitions[workflowkit.StageKey(transition.NodeID)] = transition.Clone()
	}
	return result, nil
}

// failRuntimeJob is the common controlled-runtime failure boundary. Domain
// handlers may preserve their own typed errors, while an unclassified error
// receives one bounded execution diagnosis instead of leaking its raw text to
// a durable job, CLI, or TUI surface.
func (runtime *FrozenExecutionRuntime) failRuntimeJob(ctx context.Context, job store.DurableJob, cause error) (store.JobState, error) {
	if errors.Is(cause, ErrFrozenExecutionPayload) {
		return runtime.failMalformedJob(ctx, job, cause)
	}
	failure := newDurableJobFailure("job.execution_failed", "The durable job could not complete its controlled execution.", durableJobFailureDetails(job, "execution"))
	return store.JobFailed, newDurableJobFailureError(*failure, cause)
}

// failMalformedJob handles only invalid frozen payloads and persisted
// execution-integrity facts. Ordinary handler failures go through
// failRuntimeJob so they retain a distinct, operator-safe diagnosis.
func (runtime *FrozenExecutionRuntime) failMalformedJob(ctx context.Context, job store.DurableJob, cause error) (store.JobState, error) {
	code, message := "job.storage_integrity_invalid", "The durable job could not verify its persisted execution data."
	if errors.Is(cause, ErrFrozenExecutionPayload) {
		code, message = "job.payload_invalid", "The durable job payload is malformed or does not match its frozen execution record."
	}
	failure := newDurableJobFailure(code, message, durableJobFailureDetails(job, "payload_or_storage"))
	return store.JobFailed, newDurableJobFailureError(*failure, cause)
}

func (runtime *FrozenExecutionRuntime) transitionRunToRunning(ctx context.Context, run store.WorkflowRun, actor, reason string) error {
	current, err := runtime.core.store.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("%w: workflow run %s", ErrLifecycleNotFound, run.ID)
	}
	switch current.Status {
	case store.WorkflowRunRunning:
		return nil
	case store.WorkflowRunQueued, store.WorkflowRunResumeRequested, store.WorkflowRunWaitingReview, store.WorkflowRunWaitingContinuation, store.WorkflowRunFailedRecoverable:
		_, err = runtime.core.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: current.ID, ExpectedVersion: current.Version, Status: store.WorkflowRunRunning, Actor: actor, Reason: reason})
		return err
	case store.WorkflowRunPaused:
		resumed, transitionErr := runtime.core.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: current.ID, ExpectedVersion: current.Version, Status: store.WorkflowRunResumeRequested, Actor: actor, Reason: reason})
		if transitionErr != nil {
			return transitionErr
		}
		_, err = runtime.core.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: resumed.ID, ExpectedVersion: resumed.Version, Status: store.WorkflowRunRunning, Actor: actor, Reason: reason})
		return err
	default:
		return fmt.Errorf("%w: workflow run %s cannot begin from %s", ErrFrozenExecutionPayload, current.ID, current.Status)
	}
}

func (runtime *FrozenExecutionRuntime) transitionContinuationToRunning(ctx context.Context, execution store.ContinuationExecution, actor string) error {
	if execution.State == store.ContinuationExecutionRunning {
		return nil
	}
	if execution.State != store.ContinuationExecutionQueued {
		return fmt.Errorf("%w: continuation execution %s is %s", ErrFrozenExecutionPayload, execution.ID, execution.State)
	}
	_, err := runtime.core.store.TransitionContinuationExecution(ctx, store.TransitionContinuationExecutionRequest{
		ContinuationExecutionID: execution.ID, ExpectedVersion: execution.Version, State: store.ContinuationExecutionRunning,
		Actor: actor, Reason: "durable continuation worker started",
	})
	return err
}

func validateRequiredContinuationInputs(ctx context.Context, core *lifecycleServiceCore, run store.WorkflowRun, plan runtimeExecutionPlan) error {
	return validateRequiredContinuationInputsExcept(ctx, core, run, plan, nil)
}

// validateRemainingRequiredContinuationInputs proves only inputs for stages
// that have not been admitted by this continuation yet. Once a StageAttempt
// exists, its immutable input fingerprint and retry snapshot are the source of
// truth; resolving the subject again would incorrectly treat a later review
// decision as drift in an already-completed stage.
func (runtime *FrozenExecutionRuntime) validateRemainingRequiredContinuationInputs(ctx context.Context, run store.WorkflowRun, plan runtimeExecutionPlan) error {
	if runtime == nil || runtime.core == nil || runtime.core.store == nil {
		return fmt.Errorf("%w: continuation input validator is not configured", ErrFrozenExecutionPayload)
	}
	scheduled, err := runtime.scheduledContinuationStages(ctx, run.ID, plan)
	if err != nil {
		return err
	}
	return validateRequiredContinuationInputsExcept(ctx, runtime.core, run, plan, scheduled)
}

func validateRequiredContinuationInputsExcept(ctx context.Context, core *lifecycleServiceCore, run store.WorkflowRun, plan runtimeExecutionPlan, admitted map[workflowkit.StageKey]struct{}) error {
	if core == nil || core.store == nil || core.objects == nil {
		return fmt.Errorf("%w: %w: continuation input validator is not configured", ErrFrozenExecutionPayload, errRequiredContinuationInputDrift)
	}
	subject, err := core.resolveWorkflowRunSubject(ctx, run)
	if err != nil {
		return fmt.Errorf("%w: %w: resolve continuation subject: %v", ErrFrozenExecutionPayload, errRequiredContinuationInputDrift, err)
	}
	if err := validatePreservedContinuationArtifacts(ctx, core, run, subject, plan); err != nil {
		return err
	}
	for _, stageKey := range mustTopologicalStageKeys(plan.Workflow) {
		transition, found := plan.stageTransition(stageKey)
		if !found || transition.Disposition != workflowkit.DispositionSchedule || len(transition.InputBindings) == 0 {
			continue
		}
		if _, present := admitted[stageKey]; present {
			continue
		}
		stage, found := plan.Workflow.Stage(stageKey)
		if !found || !stage.AutomaticallyDispatchable() {
			return fmt.Errorf("%w: required continuation inputs refer to unavailable stage %q", ErrFrozenExecutionPayload, stageKey)
		}
		inputs, err := resolveStageInputsForSubjectWithExplicitInputs(ctx, core.store, core.objects, run, subject, stage, transition.InputBindings)
		if err != nil {
			return fmt.Errorf("%w: %w: resolve required continuation inputs for stage %q: %v", ErrFrozenExecutionPayload, errRequiredContinuationInputDrift, stageKey, err)
		}
		if err := requirePlannedStageInputs(transition, inputs); err != nil {
			return fmt.Errorf("%w: %w: stage %q required continuation inputs: %v", ErrFrozenExecutionPayload, errRequiredContinuationInputDrift, stageKey, err)
		}
	}
	return nil
}

// validatePreservedContinuationArtifacts re-observes every stage a frozen plan
// intends to reuse. The plan can outlive an object-store entry, so its original
// preserve assertion is not enough once execution is about to commit or resume.
func validatePreservedContinuationArtifacts(ctx context.Context, core *lifecycleServiceCore, run store.WorkflowRun, subject workflowRunSubject, plan runtimeExecutionPlan) error {
	observer := storeContinuationStateObserver{dataStore: core.store, objects: core.objects}
	state, err := observer.ObserveSubject(ctx, run, subject, plan.Workflow)
	if err != nil {
		return fmt.Errorf("%w: %w: observe preserved continuation artifacts: %v", ErrFrozenExecutionPayload, errRequiredContinuationInputDrift, err)
	}
	if state.InDoubt {
		return fmt.Errorf("%w: %w: preserved continuation evidence is in doubt", ErrFrozenExecutionPayload, errRequiredContinuationInputDrift)
	}
	reuseByStage := make(map[workflowkit.StageKey]workflowkit.StageReuseState, len(state.ReuseStates))
	for _, reuse := range state.ReuseStates {
		reuseByStage[workflowkit.StageKey(reuse.NodeID)] = reuse
	}
	for _, stageKey := range mustTopologicalStageKeys(plan.Workflow) {
		transition, found := plan.stageTransition(stageKey)
		if !found || transition.Disposition != workflowkit.DispositionPreserve {
			continue
		}
		reuse, found := reuseByStage[stageKey]
		if !found || !reuse.Present || !reuse.ArtifactsIntact ||
			reuse.ExpectedInputFingerprint != transition.ExpectedInputFingerprint ||
			!sameArtifactBindings(reuse.CurrentInputs, transition.InputBindings) {
			return fmt.Errorf("%w: %w: preserved stage %q artifact is missing, damaged, or has input fingerprint drift", ErrFrozenExecutionPayload, errRequiredContinuationInputDrift, stageKey)
		}
	}
	return nil
}

// scheduledContinuationStages returns only StageAttempts created by this
// exact continuation execution. A prior retry or another continuation must
// never suppress validation for the current frozen plan.
func (runtime *FrozenExecutionRuntime) scheduledContinuationStages(ctx context.Context, runID string, plan runtimeExecutionPlan) (map[workflowkit.StageKey]struct{}, error) {
	admitted := make(map[workflowkit.StageKey]struct{})
	if plan.ContinuationPlanID == "" || plan.ContinuationExecutionID == "" {
		return admitted, nil
	}
	attempts, err := runtime.core.store.ListStageAttemptsForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	for _, attempt := range attempts {
		stageKey := workflowkit.StageKey(attempt.StageKey)
		transition, found := plan.stageTransition(stageKey)
		if !found || transition.Disposition != workflowkit.DispositionSchedule || transition.ToGeneration < 0 {
			continue
		}
		var snapshot runtimeStageAttemptSnapshot
		if err := decodeStrictJSON(attempt.RetrySnapshotJSON, &snapshot); err != nil {
			continue
		}
		if snapshot.Format != runtimeStageAttemptSnapshotFormat ||
			snapshot.ExecutionKey != plan.ExecutionKey ||
			snapshot.ContinuationPlanID != plan.ContinuationPlanID ||
			snapshot.ContinuationExecutionID != plan.ContinuationExecutionID ||
			snapshot.Generation != transition.ToGeneration {
			continue
		}
		if _, duplicate := admitted[stageKey]; duplicate {
			return nil, fmt.Errorf("%w: continuation plan has multiple matching StageAttempts for stage %q", ErrFrozenExecutionPayload, stageKey)
		}
		admitted[stageKey] = struct{}{}
	}
	return admitted, nil
}

func (runtime *FrozenExecutionRuntime) reconcileContinuationInputDrift(ctx context.Context, job store.DurableJob, execution store.ContinuationExecution, cause error) (store.JobState, error) {
	if _, err := runtime.core.store.MarkContinuationExecutionReconcileRequired(ctx, store.MarkContinuationExecutionReconcileRequiredRequest{
		ContinuationExecutionID: execution.ID, RunID: execution.RunID, Actor: job.CreatedBy,
		Reason: "required frozen continuation inputs changed after execution commit",
	}); err != nil {
		cause = fmt.Errorf("%w: project continuation input drift for reconciliation: %v", cause, err)
	}
	return runtime.failRuntimeJob(ctx, job, cause)
}

func (runtime *FrozenExecutionRuntime) ensureRunAttempt(ctx context.Context, runID, executionKey, actor, reason string) error {
	attempts, err := runtime.core.store.ListRunAttempts(ctx, runID)
	if err != nil {
		return err
	}
	for _, attempt := range attempts {
		if attempt.ResumeFrom != executionKey {
			continue
		}
		if attempt.Status == store.RunAttemptQueued {
			_, err := runtime.core.store.TransitionRunAttempt(ctx, store.TransitionRunAttemptRequest{RunAttemptID: attempt.ID, ExpectedVersion: attempt.Version, Status: store.RunAttemptRunning, Actor: actor, Reason: reason})
			return err
		}
		return nil
	}
	attempt, err := runtime.core.store.CreateRunAttempt(ctx, store.CreateRunAttemptRequest{
		RunID: runID, Ordinal: len(attempts) + 1, Trigger: "durable", ResumeFrom: executionKey, Actor: actor, Reason: reason,
	})
	if err != nil {
		return err
	}
	_, err = runtime.core.store.TransitionRunAttempt(ctx, store.TransitionRunAttemptRequest{RunAttemptID: attempt.ID, ExpectedVersion: attempt.Version, Status: store.RunAttemptRunning, Actor: actor, Reason: reason})
	return err
}

// handlePreExecutionControl acknowledges controls that arrive before a
// coordinator has scheduled any stage. A queued termination consumes no
// stage quota and creates no StageAttempt.
func (runtime *FrozenExecutionRuntime) handlePreExecutionControl(ctx context.Context, job store.DurableJob, run store.WorkflowRun) (store.JobState, bool, error) {
	operations, err := runtime.services.Control.ListForRun(ctx, run.ID)
	if err != nil {
		return "", false, err
	}
	for _, operation := range operations {
		if operation.Status != store.ControlOperationRequested && operation.Status != store.ControlOperationPropagating {
			continue
		}
		if operation.Action == store.ControlActionCancelStage {
			continue
		}
		if operation.Action == store.ControlActionPause && run.Status != store.WorkflowRunPauseRequested {
			continue
		}
		if operation.Action == store.ControlActionTerminate && run.Status != store.WorkflowRunCancelRequested {
			continue
		}
		if operation.Action == store.ControlActionPause {
			return runtime.ackQueuedPause(ctx, run, operation)
		}
		return runtime.ackQueuedTermination(ctx, run, operation)
	}
	return "", false, nil
}

func (runtime *FrozenExecutionRuntime) ackQueuedPause(ctx context.Context, run store.WorkflowRun, operation store.DurableControlOperation) (store.JobState, bool, error) {
	pausing, err := runtime.core.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunPausing, Actor: operation.Actor, Reason: operation.Reason})
	if err != nil {
		return store.JobFailed, true, err
	}
	if _, err := runtime.core.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: pausing.ID, ExpectedVersion: pausing.Version, Status: store.WorkflowRunPaused, Actor: operation.Actor, Reason: operation.Reason}); err != nil {
		return store.JobFailed, true, err
	}
	if _, err := runtime.ackControl(ctx, operation, "", "", true, false, "queued run paused before stage execution"); err != nil {
		return store.JobFailed, true, err
	}
	return store.JobInterrupted, true, nil
}

func (runtime *FrozenExecutionRuntime) ackQueuedTermination(ctx context.Context, run store.WorkflowRun, operation store.DurableControlOperation) (store.JobState, bool, error) {
	if _, err := runtime.core.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunCanceled, Actor: operation.Actor, Reason: operation.Reason}); err != nil {
		return store.JobFailed, true, err
	}
	if _, err := runtime.ackControl(ctx, operation, "", "", true, false, "queued run terminated before stage execution"); err != nil {
		return store.JobFailed, true, err
	}
	return store.JobCanceled, true, nil
}

func (runtime *FrozenExecutionRuntime) ackControl(ctx context.Context, operation store.DurableControlOperation, checkpointID, settlementID string, graceful, unknown bool, detail string) (store.DurableControlOperation, error) {
	current, err := runtime.services.Control.Get(ctx, operation.ID)
	if err != nil {
		return store.DurableControlOperation{}, err
	}
	switch current.Status {
	case store.ControlOperationAcknowledged:
		return current, nil
	case store.ControlOperationReconcileRequired, store.ControlOperationFailed:
		return store.DurableControlOperation{}, fmt.Errorf("%w: control operation %s is already %s", ErrFrozenExecutionPayload, current.ID, current.Status)
	}
	if current.Status == store.ControlOperationRequested {
		transitioned, err := runtime.services.Control.Transition(ctx, TransitionExecutionControlRequest{
			OperationID: current.ID, ExpectedVersion: current.Version, Status: store.ControlOperationPropagating,
			Actor: current.Actor, Reason: "durable runtime began control propagation",
		})
		if err != nil {
			return store.DurableControlOperation{}, err
		}
		current = transitioned
	}
	if current.Status != store.ControlOperationPropagating {
		return current, nil
	}
	return runtime.services.Control.Transition(ctx, TransitionExecutionControlRequest{
		OperationID: current.ID, ExpectedVersion: current.Version, Status: store.ControlOperationAcknowledged,
		CheckpointID: checkpointID, QuotaSettlementID: settlementID, Actor: current.Actor, Reason: detail,
		RuntimeReceipts: []store.RuntimeTerminationReceipt{{RuntimeScopeID: "durable-job:" + current.RunID, ObservedAt: runtime.core.now().UTC(), Graceful: graceful, ExternalOutcomeUnknown: unknown, PayloadJSON: mustJSON(map[string]any{"detail": detail})}},
	})
}

// commitCoordinatorDecision applies exactly one generic workflowkit decision
// to Harbor's durable control plane.  It deliberately receives an already
// validated decision from workflowkit.Engine: this adapter may persist jobs
// and projections, but must not make or replace scheduling policy.
//
// The durable job table remains the handoff boundary between batches, so a
// process crash cannot make a later batch run before its predecessor has a
// trustworthy terminal StageAttempt.
func (runtime *FrozenExecutionRuntime) commitCoordinatorDecision(ctx context.Context, parentJob store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, plan runtimeExecutionPlan, decision workflowkit.CoordinatorDecision) error {
	switch decision.Kind {
	case workflowkit.CoordinatorScheduleNextBatch:
		for _, nodeID := range decision.NextBatch.NodeIDs {
			stageKey := workflowkit.StageKey(nodeID)
			stage, found := plan.Workflow.Stage(stageKey)
			if !found || !stage.AutomaticallyDispatchable() {
				return fmt.Errorf("%w: coordinator selected non-automatic stage %q", ErrFrozenExecutionPayload, stageKey)
			}
			transition, found := plan.stageTransition(stageKey)
			if !found || transition.Disposition != workflowkit.DispositionSchedule {
				return fmt.Errorf("%w: coordinator selected stage %q without a frozen schedule transition", ErrFrozenExecutionPayload, stageKey)
			}
			if err := runtime.enqueueStageAttempt(ctx, parentJob, run, frozen, plan, stage, transition); err != nil {
				return err
			}
		}
		return nil
	case workflowkit.CoordinatorWait, workflowkit.CoordinatorBlocked:
		// A previous worker/review/reconciliation already owns the next
		// durable transition. The coordinator deliberately creates neither a
		// duplicate stage attempt nor a synthetic terminal Run outcome.
		return nil
	case workflowkit.CoordinatorComplete:
		return runtime.completeExecutionIfSatisfied(ctx, parentJob, run, frozen, plan)
	default:
		return fmt.Errorf("%w: public workflow coordinator returned unsupported decision %q", ErrFrozenExecutionPayload, decision.Kind)
	}
}

type runtimePlannedStageJob struct {
	Job     store.DurableJob
	Payload frozenStageExecutionPayload
}

func (runtime *FrozenExecutionRuntime) stageJobsForPlan(ctx context.Context, runID, executionKey string) (map[workflowkit.StageKey]runtimePlannedStageJob, error) {
	jobs, err := runtime.core.store.ListDurableJobsForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	result := make(map[workflowkit.StageKey]runtimePlannedStageJob)
	for _, job := range jobs {
		if job.CommandType != "stage_attempt.execute" {
			continue
		}
		var payload frozenStageExecutionPayload
		if err := decodeStrictJSON(job.PayloadJSON, &payload); err != nil {
			return nil, fmt.Errorf("decode stage job %s: %w", job.ID, err)
		}
		if payload.Format != frozenStageExecutionPayloadFormat || payload.RunID != runID || payload.StageAttemptID != job.StageAttemptID || payload.StageAttemptID != job.EntityID {
			return nil, fmt.Errorf("%w: stage job %s has inconsistent binding", ErrFrozenExecutionPayload, job.ID)
		}
		if stageExecutionKey(payload) != executionKey {
			continue
		}
		if previous, exists := result[payload.StageKey]; exists && previous.Job.ID != job.ID {
			return nil, fmt.Errorf("%w: execution %s has multiple jobs for stage %q", ErrFrozenExecutionPayload, executionKey, payload.StageKey)
		}
		result[payload.StageKey] = runtimePlannedStageJob{Job: job, Payload: payload}
	}
	return result, nil
}

// workflowkitCoordinatorInput translates only durable Harbor facts into the
// public kernel's domain-neutral coordinator snapshot. It deliberately makes
// no scheduling decision itself: a status that cannot be proven from a
// StageAttempt becomes a fail-closed error rather than a guessed successor.
func (runtime *FrozenExecutionRuntime) workflowkitCoordinatorInput(ctx context.Context, plan runtimeExecutionPlan, jobs map[workflowkit.StageKey]runtimePlannedStageJob) (workflowkit.CoordinatorInput, error) {
	input := workflowkit.CoordinatorInput{Workflow: plan.Workflow.Clone(), Nodes: make([]workflowkit.CoordinatorNodeState, 0, len(plan.Workflow.Stages))}
	if plan.InitialExecutionPlan != nil {
		input.ScheduleMode = workflowkit.CoordinatorScheduleExecutionPlan
		input.Plan = plan.InitialExecutionPlan.Clone()
	} else {
		input.ScheduleMode = workflowkit.CoordinatorScheduleTransitionSubset
		input.Schedule = append([]workflowkit.ScheduleBatch(nil), plan.Schedule...)
		input.Transitions = make([]workflowkit.NodeTransition, 0, len(plan.Workflow.Stages))
	}
	for _, stageKey := range mustTopologicalStageKeys(plan.Workflow) {
		stage, found := plan.Workflow.Stage(stageKey)
		if !found {
			return workflowkit.CoordinatorInput{}, fmt.Errorf("%w: coordinator workflow omits stage %q", ErrFrozenExecutionPayload, stageKey)
		}
		transition, found := plan.stageTransition(stageKey)
		if !found {
			return workflowkit.CoordinatorInput{}, fmt.Errorf("%w: coordinator plan omits transition for stage %q", ErrFrozenExecutionPayload, stageKey)
		}
		if plan.InitialExecutionPlan == nil {
			input.Transitions = append(input.Transitions, transition.Clone())
		}
		status, err := runtime.workflowkitCoordinatorNodeStatus(ctx, stage, transition, jobs)
		if err != nil {
			return workflowkit.CoordinatorInput{}, err
		}
		input.Nodes = append(input.Nodes, workflowkit.CoordinatorNodeState{NodeID: workflowkit.NodeID(stageKey), Generation: transition.ToGeneration, Status: status})
	}
	return input, nil
}

func (runtime *FrozenExecutionRuntime) workflowkitCoordinatorNodeStatus(ctx context.Context, stage workflowkit.StageDescriptor, transition workflowkit.NodeTransition, jobs map[workflowkit.StageKey]runtimePlannedStageJob) (workflowkit.CoordinatorNodeStatus, error) {
	switch transition.Disposition {
	case workflowkit.DispositionPreserve:
		// Continuation-plan assertion and artifact lineage validation already
		// prove preservation before the durable continuation job is created.
		return workflowkit.CoordinatorNodePreserved, nil
	case workflowkit.DispositionInvalidate:
		return workflowkit.CoordinatorNodeInvalidated, nil
	case workflowkit.DispositionOperatorOnly:
		if !stage.OperatorOnly() {
			return "", fmt.Errorf("%w: automatic stage %q has operator-only coordinator transition", ErrFrozenExecutionPayload, stage.Key)
		}
		return workflowkit.CoordinatorNodePending, nil
	case workflowkit.DispositionSchedule:
		if !stage.AutomaticallyDispatchable() {
			return "", fmt.Errorf("%w: operator-only stage %q has schedule coordinator transition", ErrFrozenExecutionPayload, stage.Key)
		}
	default:
		return "", fmt.Errorf("%w: stage %q has unsupported coordinator transition %q", ErrFrozenExecutionPayload, stage.Key, transition.Disposition)
	}
	planned, found := jobs[stage.Key]
	if !found {
		return workflowkit.CoordinatorNodePending, nil
	}
	attempt, err := runtime.core.store.GetStageAttempt(ctx, planned.Payload.StageAttemptID)
	if err != nil {
		return "", err
	}
	if attempt == nil || attempt.RunID != planned.Job.RunID || attempt.StageKey != string(stage.Key) {
		return "", fmt.Errorf("%w: coordinator stage job %s has no matching StageAttempt", ErrFrozenExecutionPayload, planned.Job.ID)
	}
	switch attempt.ExecutionStatus {
	case store.StageExecutionQueued:
		return workflowkit.CoordinatorNodeQueued, nil
	case store.StageExecutionRunning:
		return workflowkit.CoordinatorNodeRunning, nil
	case store.StageExecutionWaiting:
		return workflowkit.CoordinatorNodeWaiting, nil
	case store.StageExecutionCompleted:
		if attempt.Verdict == store.VerdictPass || attempt.Verdict == store.VerdictAdvisory {
			return workflowkit.CoordinatorNodeSucceeded, nil
		}
		return workflowkit.CoordinatorNodeBlocked, nil
	case store.StageExecutionInfraFailed, store.StageExecutionInterrupted:
		return workflowkit.CoordinatorNodeFailed, nil
	case store.StageExecutionInDoubt, store.StageExecutionReconciling:
		return workflowkit.CoordinatorNodeInDoubt, nil
	case store.StageExecutionCanceled:
		return workflowkit.CoordinatorNodeCanceled, nil
	default:
		return "", fmt.Errorf("%w: stage %q has unsupported execution status %q", ErrFrozenExecutionPayload, stage.Key, attempt.ExecutionStatus)
	}
}

func stageExecutionKey(payload frozenStageExecutionPayload) string {
	if payload.ContinuationPlanID != "" {
		return payload.ContinuationPlanID
	}
	return "initial"
}

func (runtime *FrozenExecutionRuntime) enqueueStageAttempt(ctx context.Context, parentJob store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, plan runtimeExecutionPlan, stage workflowkit.StageDescriptor, transition workflowkit.NodeTransition) error {
	if !stage.AutomaticallyDispatchable() {
		return fmt.Errorf("%w: operator-only stage %q cannot receive a StageAttempt", ErrFrozenExecutionPayload, stage.Key)
	}
	subject, err := runtime.core.resolveWorkflowRunSubject(ctx, run)
	if err != nil {
		return err
	}
	inputs, err := resolveStageInputsForSubjectWithExplicitInputs(ctx, runtime.core.store, runtime.core.objects, run, subject, stage, transition.InputBindings)
	if err != nil {
		if len(transition.InputBindings) != 0 {
			return fmt.Errorf("%w: %w: resolve required continuation inputs for stage %q: %v", ErrFrozenExecutionPayload, errRequiredContinuationInputDrift, stage.Key, err)
		}
		return fmt.Errorf("resolve immutable inputs for stage %q: %w", stage.Key, err)
	}
	if err := requirePlannedStageInputs(transition, inputs); err != nil {
		return fmt.Errorf("%w: %w: stage %q required continuation inputs: %v", ErrFrozenExecutionPayload, errRequiredContinuationInputDrift, stage.Key, err)
	}
	inputFingerprint, err := workflowkit.FingerprintArtifactBindings(inputs)
	if err != nil {
		return err
	}
	attempt, err := runtime.findOrCreatePlannedStageAttempt(ctx, run, plan, stage, transition, inputFingerprint, parentJob.CreatedBy)
	if err != nil {
		return err
	}
	payload := frozenStageExecutionPayload{
		Format:                  frozenStageExecutionPayloadFormat,
		RunID:                   run.ID,
		StageAttemptID:          attempt.ID,
		StageKey:                stage.Key,
		DefinitionHash:          run.DefinitionHash,
		Generation:              transition.ToGeneration,
		ContinuationExecutionID: plan.ContinuationExecutionID,
		ContinuationPlanID:      plan.ContinuationPlanID,
		QuotaPolicy:             frozen.QuotaPolicy.Clone(),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode stage execution payload: %w", err)
	}
	jobKey := "stage-attempt-execution:" + run.ID + ":" + plan.ExecutionKey + ":" + string(stage.Key)
	_, err = runtime.core.store.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "stage_attempt.execute", EntityType: "stage_attempt", EntityID: attempt.ID,
		RunID: run.ID, StageAttemptID: attempt.ID, PayloadJSON: string(encoded), IdempotencyKey: jobKey,
		Actor: parentJob.CreatedBy, Reason: "enqueue frozen stage attempt " + string(stage.Key),
	})
	return err
}

func requirePlannedStageInputs(transition workflowkit.NodeTransition, actual []workflowkit.ArtifactBinding) error {
	if transition.Disposition != workflowkit.DispositionSchedule || len(transition.InputBindings) == 0 {
		return nil
	}
	byName := make(map[string]workflowkit.ArtifactBinding, len(actual))
	for _, binding := range actual {
		if _, duplicate := byName[binding.Name]; duplicate {
			return fmt.Errorf("resolved duplicate input %q", binding.Name)
		}
		byName[binding.Name] = binding
	}
	for _, required := range transition.InputBindings {
		binding, present := byName[required.Name]
		if !present {
			return fmt.Errorf("missing frozen input %q", required.Name)
		}
		if binding != required {
			return fmt.Errorf("frozen input %q changed after continuation planning", required.Name)
		}
	}
	return nil
}

func (runtime *FrozenExecutionRuntime) findOrCreatePlannedStageAttempt(ctx context.Context, run store.WorkflowRun, plan runtimeExecutionPlan, stage workflowkit.StageDescriptor, transition workflowkit.NodeTransition, inputFingerprint workflowkit.Fingerprint, actor string) (store.StageAttempt, error) {
	attempts, err := runtime.core.store.ListStageAttemptsForRun(ctx, run.ID)
	if err != nil {
		return store.StageAttempt{}, err
	}
	for _, attempt := range attempts {
		if attempt.StageKey != string(stage.Key) {
			continue
		}
		var snapshot runtimeStageAttemptSnapshot
		if err := decodeStrictJSON(attempt.RetrySnapshotJSON, &snapshot); err != nil || snapshot.Format != runtimeStageAttemptSnapshotFormat {
			continue
		}
		if snapshot.ExecutionKey == plan.ExecutionKey && snapshot.Generation == transition.ToGeneration && snapshot.ContinuationPlanID == plan.ContinuationPlanID && snapshot.ContinuationExecutionID == plan.ContinuationExecutionID {
			if attempt.InputFingerprint != string(inputFingerprint) {
				return store.StageAttempt{}, fmt.Errorf("%w: planned stage %q input fingerprint changed before execution", ErrFrozenExecutionPayload, stage.Key)
			}
			return attempt, nil
		}
	}
	ordinal := 1
	var retryOf string
	for _, attempt := range attempts {
		if attempt.StageKey != string(stage.Key) {
			continue
		}
		if attempt.Ordinal >= ordinal {
			ordinal = attempt.Ordinal + 1
			retryOf = attempt.ID
		}
	}
	budgetJSON, err := json.Marshal(stage.Budget)
	if err != nil {
		return store.StageAttempt{}, err
	}
	retryJSON, err := json.Marshal(runtimeStageAttemptSnapshot{
		Format: runtimeStageAttemptSnapshotFormat, ExecutionKey: plan.ExecutionKey,
		ContinuationExecutionID: plan.ContinuationExecutionID, ContinuationPlanID: plan.ContinuationPlanID,
		Generation: transition.ToGeneration, Retry: stage.Retry.Clone(),
	})
	if err != nil {
		return store.StageAttempt{}, err
	}
	created, err := runtime.core.store.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, RetryOfStageAttemptID: retryOf, StageKey: string(stage.Key), StageGroup: stage.Group,
		Ordinal: ordinal, InputFingerprint: string(inputFingerprint), BudgetSnapshotJSON: string(budgetJSON), RetrySnapshotJSON: string(retryJSON),
		Actor: actor, Reason: "schedule frozen stage " + string(stage.Key),
	})
	if err == nil {
		return created, nil
	}
	if !errors.Is(err, store.ErrIdentityCollision) {
		return store.StageAttempt{}, err
	}
	// A concurrent worker may have won the unique (run, stage, ordinal) race.
	// Re-read only the immutable execution snapshot instead of creating a
	// second stage attempt under a different ordinal.
	attempts, listErr := runtime.core.store.ListStageAttemptsForRun(ctx, run.ID)
	if listErr != nil {
		return store.StageAttempt{}, listErr
	}
	for _, attempt := range attempts {
		if attempt.StageKey != string(stage.Key) {
			continue
		}
		var snapshot runtimeStageAttemptSnapshot
		if decodeStrictJSON(attempt.RetrySnapshotJSON, &snapshot) == nil && snapshot.Format == runtimeStageAttemptSnapshotFormat && snapshot.ExecutionKey == plan.ExecutionKey && snapshot.Generation == transition.ToGeneration {
			return attempt, nil
		}
	}
	return store.StageAttempt{}, err
}

func (runtime *FrozenExecutionRuntime) completeExecutionIfSatisfied(ctx context.Context, parentJob store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, plan runtimeExecutionPlan) error {
	completed, err := runtime.completeRunIfSatisfied(ctx, run, frozen, plan, parentJob.CreatedBy)
	if err != nil {
		return err
	}
	if !completed {
		// A Standard authoring materialization may have a durable Phase-1
		// handoff still in flight. A stale coordinator must finish harmlessly
		// rather than terminally completing the parent before that bridge is
		// consumed. The handoff handler publishes the replacement coordinator.
		return nil
	}
	if plan.ContinuationExecutionID != "" {
		execution, err := runtime.core.store.GetContinuationExecution(ctx, plan.ContinuationExecutionID)
		if err != nil {
			return err
		}
		if execution != nil && execution.State == store.ContinuationExecutionRunning {
			if _, err := runtime.core.store.TransitionContinuationExecution(ctx, store.TransitionContinuationExecutionRequest{
				ContinuationExecutionID: execution.ID, ExpectedVersion: execution.Version, State: store.ContinuationExecutionCompleted,
				Actor: parentJob.CreatedBy, Reason: "all frozen continuation batches completed",
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (runtime *FrozenExecutionRuntime) handleStageAttempt(ctx context.Context, execution DurableJobExecution, job store.DurableJob) (store.JobState, error) {
	var payload frozenStageExecutionPayload
	if err := decodeStrictJSON(job.PayloadJSON, &payload); err != nil {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: decode stage execution payload: %w", ErrFrozenExecutionPayload, err))
	}
	if payload.Format != frozenStageExecutionPayloadFormat || payload.RunID != job.RunID || payload.StageAttemptID != job.StageAttemptID || payload.StageAttemptID != job.EntityID || payload.Generation < 0 {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: stage job does not bind its payload", ErrFrozenExecutionPayload))
	}
	run, frozen, err := runtime.loadFrozenRun(ctx, payload.RunID, payload.DefinitionHash, "", payload.QuotaPolicy)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	stage, found := frozen.Workflow.Stage(payload.StageKey)
	if !found {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: frozen workflow omits stage %q", ErrFrozenExecutionPayload, payload.StageKey))
	}
	if !stage.AutomaticallyDispatchable() {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: operator-only stage %q cannot execute from a durable worker job", ErrFrozenExecutionPayload, stage.Key))
	}
	loadedStageAttempt, err := runtime.core.store.GetStageAttempt(ctx, payload.StageAttemptID)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if loadedStageAttempt == nil || loadedStageAttempt.RunID != run.ID || loadedStageAttempt.StageKey != string(stage.Key) {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: stage attempt does not match durable job", ErrFrozenExecutionPayload))
	}
	if err := runtime.validateStageAttemptPlanBinding(*loadedStageAttempt, payload); err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	plan, err := runtime.runtimePlanForStagePayload(ctx, payload, frozen)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	transition, found := plan.stageTransition(stage.Key)
	if !found || transition.Disposition != workflowkit.DispositionSchedule {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: stage attempt is not scheduled by its frozen execution plan", ErrFrozenExecutionPayload))
	}
	isCodeEdgeEvaluator := isCodeEdgeEvaluatorStage(run, stage)
	if isCodeEdgeEvaluator {
		effect, effectErr := runtime.codeEdgeEvaluatorEffectAlreadyStarted(ctx, run, *loadedStageAttempt, stage)
		if effectErr != nil {
			return runtime.failRuntimeJob(ctx, job, effectErr)
		}
		if effect != nil && !(effect.State == store.SideEffectSucceeded && loadedStageAttempt.ExecutionStatus == store.StageExecutionCompleted) {
			return runtime.projectCodeEdgeEvaluatorInDoubt(ctx, job, run, *loadedStageAttempt, stage, stageQuotaReservation{}, *effect, nil, "CodeEdge evaluator invocation fence was already started")
		}
	}
	if loadedStageAttempt.ExecutionStatus == store.StageExecutionCompleted || loadedStageAttempt.ExecutionStatus == store.StageExecutionInfraFailed || loadedStageAttempt.ExecutionStatus == store.StageExecutionInterrupted || loadedStageAttempt.ExecutionStatus == store.StageExecutionCanceled {
		return store.JobSucceeded, nil
	}
	if loadedStageAttempt.ExecutionStatus == store.StageExecutionWaiting {
		// A gate activation job has already atomically handed the stage to its
		// durable review record. Its later decision is resolved by a separate
		// scoped job, so replaying this delivery must not reopen the gate.
		return store.JobSucceeded, nil
	}
	stageAttempt := *loadedStageAttempt
	subject, err := runtime.core.resolveWorkflowRunSubject(ctx, run)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if state, handled, controlErr := runtime.handlePreStageControl(ctx, job, run, stageAttempt); handled || controlErr != nil {
		return state, controlErr
	}
	if isCodeEdgeEvaluator {
		if err := validateCodeEdgeEvaluatorBudget(stage); err != nil {
			return runtime.failRuntimeJob(ctx, job, err)
		}
	}
	if stageAttempt.ExecutionStatus == store.StageExecutionQueued {
		stageAttempt, err = runtime.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
			StageAttemptID: stageAttempt.ID, ExpectedVersion: stageAttempt.Version, ExecutionStatus: store.StageExecutionRunning,
			Actor: job.CreatedBy, Reason: "durable stage worker started",
		})
		if err != nil {
			return runtime.failRuntimeJob(ctx, job, err)
		}
	}
	if stageAttempt.ExecutionStatus != store.StageExecutionRunning {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: stage attempt %s is %s", ErrFrozenExecutionPayload, stageAttempt.ID, stageAttempt.ExecutionStatus))
	}

	if review, isReviewGate := frozen.ReviewStage(stage.Key); isReviewGate {
		inputs, inputErr := resolveStageInputsForSubjectWithExplicitInputs(ctx, runtime.core.store, runtime.core.objects, run, subject, stage, transition.InputBindings)
		if inputErr != nil {
			return runtime.projectStageTerminal(ctx, job, run, frozen, payload, subject, stage, stageAttempt, nil, stageQuotaReservation{}, StageExecutionResult{
				Outcome: workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailurePermanent}, ErrorText: inputErr.Error(), FailureClass: string(workflowkit.FailurePermanent),
			}, execution.LeaseLost, nil)
		}
		inputFingerprint, inputErr := workflowkit.FingerprintArtifactBindings(inputs)
		if inputErr != nil {
			return runtime.failRuntimeJob(ctx, job, inputErr)
		}
		if string(inputFingerprint) != stageAttempt.InputFingerprint {
			return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: review gate input fingerprint drift", ErrFrozenExecutionPayload))
		}
		return runtime.executeWorkflowkitReviewGate(ctx, execution, job, run, subject, frozen, payload, stageAttempt, stage, inputs, review)
	}

	stageContext, cancel := context.WithTimeout(ctx, stage.Budget.MaxElapsed)
	defer cancel()
	if execution.ExecutionAbort != nil {
		go func() {
			select {
			case <-execution.ExecutionAbort:
				cancel()
			case <-stageContext.Done():
			}
		}()
	}
	reservation, admissionErr := runtime.admitStageQuota(ctx, execution, job, run, subject, frozen.QuotaPolicy, stage, cancel)
	if admissionErr != nil {
		cancel()
		return runtime.projectAdmissionFailure(ctx, job, run, frozen, payload, subject, stage, &stageAttempt, admissionErr)
	}
	defer reservation.stop()

	monitor := runtime.startStageControlMonitor(stageContext, cancel, run.ID, stageAttempt.ID, job.CreatedBy)
	defer monitor.stop()

	inputs, err := resolveStageInputsForSubjectWithExplicitInputs(stageContext, runtime.core.store, runtime.core.objects, run, subject, stage, transition.InputBindings)
	if err != nil {
		return runtime.projectStageTerminal(ctx, job, run, frozen, payload, subject, stage, stageAttempt, nil, reservation, StageExecutionResult{
			Outcome: workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailurePermanent}, ErrorText: err.Error(), FailureClass: string(workflowkit.FailurePermanent),
		}, nil, monitor)
	}
	inputFingerprint, err := workflowkit.FingerprintArtifactBindings(inputs)
	if err != nil {
		return runtime.failAdmittedStageIntegrity(ctx, job, stageAttempt, reservation, execution.LeaseLost, err)
	}
	if string(inputFingerprint) != stageAttempt.InputFingerprint {
		return runtime.failAdmittedStageIntegrity(ctx, job, stageAttempt, reservation, execution.LeaseLost, fmt.Errorf("%w: stage attempt input fingerprint drift", ErrFrozenExecutionPayload))
	}
	var codeEdgeEffect *store.SideEffectOperation
	if isCodeEdgeEvaluator {
		fence, fenceErr := runtime.prepareCodeEdgeEvaluatorEffect(ctx, job, run, stageAttempt, stage)
		if fenceErr != nil {
			return runtime.failAdmittedStageIntegrity(ctx, job, stageAttempt, reservation, execution.LeaseLost, fenceErr)
		}
		codeEdgeEffect = &fence.Operation
		if !fence.Invoke {
			return runtime.projectCodeEdgeEvaluatorInDoubt(ctx, job, run, stageAttempt, stage, reservation, fence.Operation, nil, "CodeEdge evaluator invocation fence prevents replay")
		}
	}
	var result StageExecutionResult
	for ordinal := 1; ordinal <= stage.Budget.MaxAttempts; ordinal++ {
		nodeAttempt, nodeErr := runtime.createRunningNodeAttempt(ctx, stageAttempt, stage, payload.Generation, ordinal, job.CreatedBy)
		if nodeErr != nil {
			if codeEdgeEffect != nil {
				return runtime.projectCodeEdgeEvaluatorInDoubt(ctx, job, run, stageAttempt, stage, reservation, *codeEdgeEffect, nil, nodeErr.Error())
			}
			return runtime.failAdmittedStageIntegrity(ctx, job, stageAttempt, reservation, execution.LeaseLost, nodeErr)
		}
		attemptContext, attemptCancel := context.WithTimeout(stageContext, stage.Budget.AttemptTimeout)
		result, nodeErr = runtime.executeWorkflowkitStage(attemptContext, execution, job, run, subject, frozen, payload, stageAttempt, nodeAttempt, stage, inputs, reservation, monitor)
		attemptContextErr := attemptContext.Err()
		attemptCancel()
		result = normalizeStageExecutionResult(result, nodeErr, attemptContextErr)
		if channelClosed(execution.LeaseLost) {
			return store.JobInDoubt, ErrDurableJobLeaseLost
		}
		if err := result.Outcome.Validate(); err != nil {
			result = StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailurePolicy}, ErrorText: err.Error(), FailureClass: string(workflowkit.FailurePolicy)}
		}
		if result.Outcome.Status == workflowkit.StatusCompleted && !stage.Verdicts.Allows(result.Outcome.Verdict) {
			result = StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailurePolicy}, ErrorText: fmt.Sprintf("stage emitted frozen-disallowed verdict %q", result.Outcome.Verdict), FailureClass: string(workflowkit.FailurePolicy)}
		}
		nodeOutcome := result.Outcome
		if codeEdgeEffect != nil && codeEdgeEvaluatorOutcomeIsUncertain(result, reservation, execution.LeaseLost, monitor) {
			nodeOutcome = workflowkit.Outcome{Status: workflowkit.StatusInDoubt}
		}
		if err := runtime.transitionNodeAttempt(ctx, nodeAttempt, nodeOutcome, result.ErrorText, job.CreatedBy); err != nil {
			if codeEdgeEffect != nil {
				return runtime.projectCodeEdgeEvaluatorInDoubt(ctx, job, run, stageAttempt, stage, reservation, *codeEdgeEffect, nil, err.Error())
			}
			return runtime.failAdmittedStageIntegrity(ctx, job, stageAttempt, reservation, execution.LeaseLost, err)
		}
		retry, retryErr := workflowkit.DecideStageRetry(stage, ordinal, result.Outcome)
		if retryErr != nil {
			if codeEdgeEffect != nil {
				return runtime.projectCodeEdgeEvaluatorInDoubt(ctx, job, run, stageAttempt, stage, reservation, *codeEdgeEffect, nil, retryErr.Error())
			}
			return runtime.failAdmittedStageIntegrity(ctx, job, stageAttempt, reservation, execution.LeaseLost, retryErr)
		}
		if retry.Retry && stageContext.Err() == nil {
			if err := waitStageRetry(stageContext, retry.Delay); err != nil {
				result = StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInterrupted}, ErrorText: err.Error()}
				break
			}
			continue
		}
		break
	}
	return runtime.projectStageTerminal(ctx, job, run, frozen, payload, subject, stage, stageAttempt, inputs, reservation, result, execution.LeaseLost, monitor)
}

// failAdmittedStageIntegrity closes the accounting boundary for a corruption
// or optimistic-concurrency failure discovered after stage admission but
// before a normal terminal projection can run. The actual amount consumed is
// no longer knowable, so each reservation remains fenced as uncertain rather
// than silently releasing quota or leaving an active lease to expire.
func (runtime *FrozenExecutionRuntime) failAdmittedStageIntegrity(ctx context.Context, job store.DurableJob, attempt store.StageAttempt, reservation stageQuotaReservation, leaseLost <-chan struct{}, cause error) (store.JobState, error) {
	reservation.stop()
	if channelClosed(leaseLost) || errors.Is(cause, store.ErrDispatchFenceLost) {
		return store.JobInDoubt, ErrDurableJobLeaseLost
	}
	cleanupCtx := context.WithoutCancel(ctx)
	var settlementErr error
	if _, err := runtime.settleStageQuota(cleanupCtx, reservation, store.QuotaSettlementUncertain, job.CreatedBy, "frozen stage integrity failure requires quota reconciliation"); err != nil {
		settlementErr = err
	}
	current, lookupErr := runtime.core.store.GetStageAttempt(cleanupCtx, attempt.ID)
	if lookupErr != nil {
		if settlementErr == nil {
			settlementErr = lookupErr
		}
	} else if current != nil && current.ExecutionStatus == store.StageExecutionRunning {
		if err := runtime.transitionStageInDoubt(cleanupCtx, *current, job.CreatedBy, cause.Error()); err != nil && settlementErr == nil {
			settlementErr = err
		}
	}
	if runErr := runtime.finishRunWithStatus(cleanupCtx, attempt.RunID, store.WorkflowRunInDoubt, job.CreatedBy, "admitted stage integrity requires reconciliation"); runErr != nil && settlementErr == nil {
		settlementErr = runErr
	}
	if settlementErr != nil {
		cause = fmt.Errorf("%w: settle admitted stage integrity failure: %v", cause, settlementErr)
	}
	return runtime.failRuntimeJob(ctx, job, cause)
}

func (runtime *FrozenExecutionRuntime) validateStageAttemptPlanBinding(attempt store.StageAttempt, payload frozenStageExecutionPayload) error {
	var snapshot runtimeStageAttemptSnapshot
	if err := decodeStrictJSON(attempt.RetrySnapshotJSON, &snapshot); err != nil {
		return fmt.Errorf("%w: decode stage attempt schedule snapshot: %v", ErrFrozenExecutionPayload, err)
	}
	if snapshot.Format != runtimeStageAttemptSnapshotFormat || snapshot.Generation != payload.Generation || snapshot.ContinuationPlanID != payload.ContinuationPlanID || snapshot.ContinuationExecutionID != payload.ContinuationExecutionID || snapshot.ExecutionKey != stageExecutionKey(payload) {
		return fmt.Errorf("%w: stage attempt schedule snapshot differs from job", ErrFrozenExecutionPayload)
	}
	return nil
}

func (runtime *FrozenExecutionRuntime) createRunningNodeAttempt(ctx context.Context, stageAttempt store.StageAttempt, stage workflowkit.StageDescriptor, generation, ordinal int, actor string) (store.NodeAttempt, error) {
	key := fmt.Sprintf("stage-node:%s:%s:%d:%d", stageAttempt.ID, stage.Key, generation, ordinal)
	node, err := runtime.core.store.CreateNodeAttempt(ctx, store.CreateNodeAttemptRequest{
		StageAttemptID: stageAttempt.ID, NodeID: string(stage.Key), Generation: generation, Attempt: ordinal,
		IdempotencyKey: key, Actor: actor, Reason: "execute frozen stage node attempt",
	})
	if err != nil {
		return store.NodeAttempt{}, err
	}
	return runtime.core.store.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
		NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: store.NodeAttemptRunning,
		Actor: actor, Reason: "frozen stage node attempt started",
	})
}

func normalizeStageExecutionResult(result StageExecutionResult, executionErr, contextErr error) StageExecutionResult {
	if result.Outcome.Status != "" {
		if result.ErrorText == "" && executionErr != nil {
			result.ErrorText = executionErr.Error()
		}
		return result
	}
	if executionErr == nil {
		return StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailureUnknown}, ErrorText: "stage executor returned no terminal outcome", FailureClass: string(workflowkit.FailureUnknown)}
	}
	if errors.Is(executionErr, context.Canceled) || errors.Is(contextErr, context.Canceled) {
		return StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInterrupted}, ErrorText: executionErr.Error()}
	}
	if errors.Is(executionErr, context.DeadlineExceeded) || errors.Is(contextErr, context.DeadlineExceeded) {
		return StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailureTimeout}, ErrorText: executionErr.Error(), FailureClass: string(workflowkit.FailureTimeout)}
	}
	return StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailureUnknown}, ErrorText: executionErr.Error(), FailureClass: string(workflowkit.FailureUnknown)}
}

func waitStageRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (runtime *FrozenExecutionRuntime) transitionNodeAttempt(ctx context.Context, attempt store.NodeAttempt, outcome workflowkit.Outcome, errorText, actor string) error {
	status := store.NodeAttemptInfraFailed
	switch outcome.Status {
	case workflowkit.StatusCompleted:
		status = store.NodeAttemptCompleted
	case workflowkit.StatusInterrupted:
		status = store.NodeAttemptInterrupted
	case workflowkit.StatusCanceled:
		status = store.NodeAttemptCanceled
	case workflowkit.StatusInDoubt:
		status = store.NodeAttemptInDoubt
	case workflowkit.StatusInfraFailed:
		status = store.NodeAttemptInfraFailed
	default:
		return fmt.Errorf("%w: unsupported node outcome %q", ErrFrozenExecutionPayload, outcome.Status)
	}
	_, err := runtime.core.store.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
		NodeAttemptID: attempt.ID, ExpectedVersion: attempt.Version, Status: status, ErrorText: errorText,
		Actor: actor, Reason: "frozen stage node attempt completed",
	})
	return err
}

type quotaAdmissionRejectedError struct {
	Reason store.AdmissionReason
}

func (err quotaAdmissionRejectedError) Error() string {
	return "frozen stage quota admission rejected: " + string(err.Reason)
}

type stageQuotaReservation struct {
	leases     []store.DurableQuotaLease
	heartbeats *quotaLeaseHeartbeats
	actor      string
}

func (reservation stageQuotaReservation) stop() {
	if reservation.heartbeats != nil {
		reservation.heartbeats.stop()
	}
}

func (reservation stageQuotaReservation) lost() bool {
	return reservation.heartbeats != nil && reservation.heartbeats.lostLease()
}

func (reservation stageQuotaReservation) currentLeases() []store.DurableQuotaLease {
	if reservation.heartbeats != nil {
		if leases := reservation.heartbeats.currentLeases(); len(leases) != 0 {
			return leases
		}
	}
	return append([]store.DurableQuotaLease(nil), reservation.leases...)
}

func quotaLeaseDeadline(leases []store.DurableQuotaLease) time.Time {
	if len(leases) == 0 {
		return time.Time{}
	}
	deadline := leases[0].ExpiresAt
	for _, lease := range leases[1:] {
		if lease.ExpiresAt.Before(deadline) {
			deadline = lease.ExpiresAt
		}
	}
	return deadline
}

func (runtime *FrozenExecutionRuntime) admitStageQuota(ctx context.Context, execution DurableJobExecution, job store.DurableJob, run store.WorkflowRun, subject workflowRunSubject, policy workflowadapter.ResolvedQuotaPolicy, stage workflowkit.StageDescriptor, cancel context.CancelFunc) (stageQuotaReservation, error) {
	if len(stage.QuotaClaims) == 0 {
		return stageQuotaReservation{}, nil
	}
	frozenAdmission, err := BuildFrozenStageQuotaAdmission(policy, stage)
	if err != nil {
		return stageQuotaReservation{}, err
	}
	if execution.Claim.Owner == "" {
		return stageQuotaReservation{}, fmt.Errorf("%w: durable worker owner is required for stage quota", ErrFrozenExecutionPayload)
	}
	taskID, err := subject.quotaTaskID()
	if err != nil {
		return stageQuotaReservation{}, fmt.Errorf("%w: resolve stage quota subject: %v", ErrFrozenExecutionPayload, err)
	}
	decision, err := runtime.core.store.AdmitTaskActorQuota(ctx, store.AdmitTaskActorQuotaRequest{
		IdempotencyKey:    "stage-admission:" + job.ID,
		TaskID:            taskID,
		Actor:             job.CreatedBy,
		LeaseOwner:        execution.Claim.Owner,
		LeaseTTL:          runtime.quotaLeaseTTL,
		Policy:            frozenAdmission.Policy,
		BootstrapAccounts: frozenAdmission.BootstrapAccounts,
		Claims:            frozenAdmission.Claims,
		Reason:            "admit frozen stage " + string(stage.Key),
	})
	if err != nil {
		return stageQuotaReservation{}, err
	}
	if !decision.Accepted {
		return stageQuotaReservation{}, quotaAdmissionRejectedError{Reason: decision.Reason}
	}
	reservation := stageQuotaReservation{
		leases: append([]store.DurableQuotaLease(nil), decision.Leases...),
		actor:  job.CreatedBy,
	}
	reservation.heartbeats = newQuotaLeaseHeartbeats(runtime, execution.Claim.Owner, job.CreatedBy, reservation.leases, cancel)
	reservation.heartbeats.start()
	for _, claim := range frozenAdmission.Claims {
		if claim.Dimension != "stage_attempt" {
			continue
		}
		if err := reservation.recordDimension(ctx, runtime, claim.Dimension, claim.Units, "stage-attempt:"+job.ID, time.Time{}, job.CreatedBy, "stage attempt began"); err != nil {
			reservation.stop()
			return stageQuotaReservation{}, err
		}
	}
	return reservation, nil
}

func (reservation stageQuotaReservation) chargeWriter(runtime *FrozenExecutionRuntime, stageAttemptID, nodeAttemptID string) func(context.Context, StageUsage) error {
	return func(ctx context.Context, usage StageUsage) error {
		if strings.TrimSpace(usage.Dimension) == "" || usage.Units <= 0 || strings.TrimSpace(usage.OperationKey) == "" || usage.OccurredAt.IsZero() {
			return fmt.Errorf("%w: stage usage needs dimension, positive units, operation key, and occurrence time", ErrInvalidStageExecution)
		}
		key := "stage-usage:" + stageAttemptID + ":" + nodeAttemptID + ":" + usage.OperationKey
		return reservation.recordDimension(ctx, runtime, usage.Dimension, usage.Units, key, usage.OccurredAt, reservation.actor, "frozen stage usage")
	}
}

func (reservation stageQuotaReservation) recordDimension(ctx context.Context, runtime *FrozenExecutionRuntime, dimension string, units int64, operationKey string, occurredAt time.Time, actor, reason string) error {
	requests := make([]store.RecordQuotaUsageRequest, 0, 2)
	for _, lease := range reservation.leases {
		if lease.Dimension != dimension {
			continue
		}
		when := occurredAt
		if when.IsZero() {
			when = lease.CreatedAt
		}
		requests = append(requests, store.RecordQuotaUsageRequest{
			OperationKey: operationKey + ":" + lease.ID, LeaseID: lease.ID, FencingToken: lease.FencingToken,
			Units: units, OccurredAt: when.UTC(), Actor: actor, Reason: reason,
		})
	}
	if len(requests) == 0 {
		return fmt.Errorf("%w: stage emitted usage for unreserved dimension %q", ErrFrozenQuotaAdmission, dimension)
	}
	_, err := retryIdempotentLeaseHeartbeat(ctx, nil, runtime.quotaLeaseTTL/3, quotaLeaseDeadline(reservation.currentLeases()),
		func(callCtx context.Context) ([]store.QuotaAccount, error) {
			return runtime.core.store.RecordQuotaUsages(callCtx, requests)
		})
	return err
}

func (runtime *FrozenExecutionRuntime) projectAdmissionFailure(ctx context.Context, job store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, payload frozenStageExecutionPayload, subject workflowRunSubject, stage workflowkit.StageDescriptor, attempt *store.StageAttempt, admissionErr error) (store.JobState, error) {
	if attempt == nil {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: stage attempt is required for admission projection", ErrFrozenExecutionPayload))
	}
	result := StageExecutionResult{
		Outcome:      workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailurePolicy},
		ErrorText:    admissionErr.Error(),
		FailureClass: string(workflowkit.FailurePolicy),
	}
	return runtime.projectStageTerminal(ctx, job, run, frozen, payload, subject, stage, *attempt, nil, stageQuotaReservation{}, result, nil, nil)
}

// quotaLeaseHeartbeats keeps admission fences alive independently from the
// worker dispatch lease. A lost quota fence is treated as uncertain rather
// than allowing a stale executor to settle or overwrite accounting facts.
type quotaLeaseHeartbeats struct {
	runtime *FrozenExecutionRuntime
	owner   string
	actor   string
	leases  []store.DurableQuotaLease
	cancel  context.CancelFunc

	mu      sync.Mutex
	lost    bool
	failure error
	once    sync.Once
	stopCh  chan struct{}
	doneCh  chan struct{}
	count   uint64
}

func newQuotaLeaseHeartbeats(runtime *FrozenExecutionRuntime, owner, actor string, leases []store.DurableQuotaLease, cancel context.CancelFunc) *quotaLeaseHeartbeats {
	return &quotaLeaseHeartbeats{
		runtime: runtime, owner: owner, actor: actor, leases: append([]store.DurableQuotaLease(nil), leases...), cancel: cancel,
		stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
}

func (heartbeats *quotaLeaseHeartbeats) start() {
	if heartbeats == nil || len(heartbeats.leases) == 0 {
		return
	}
	go heartbeats.run()
}

func (heartbeats *quotaLeaseHeartbeats) stop() {
	if heartbeats == nil || len(heartbeats.leases) == 0 {
		return
	}
	heartbeats.once.Do(func() { close(heartbeats.stopCh) })
	<-heartbeats.doneCh
}

func (heartbeats *quotaLeaseHeartbeats) lostLease() bool {
	if heartbeats == nil {
		return false
	}
	heartbeats.mu.Lock()
	defer heartbeats.mu.Unlock()
	return heartbeats.lost
}

func (heartbeats *quotaLeaseHeartbeats) failureError() error {
	if heartbeats == nil {
		return nil
	}
	heartbeats.mu.Lock()
	defer heartbeats.mu.Unlock()
	return heartbeats.failure
}

func (heartbeats *quotaLeaseHeartbeats) currentLeases() []store.DurableQuotaLease {
	if heartbeats == nil {
		return nil
	}
	heartbeats.mu.Lock()
	defer heartbeats.mu.Unlock()
	return append([]store.DurableQuotaLease(nil), heartbeats.leases...)
}

func (heartbeats *quotaLeaseHeartbeats) run() {
	defer close(heartbeats.doneCh)
	interval := heartbeats.runtime.quotaLeaseTTL / 3
	if interval <= 0 || interval >= heartbeats.runtime.quotaLeaseTTL {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-heartbeats.stopCh:
			return
		case <-ticker.C:
			if err := heartbeats.heartbeat(); err != nil {
				heartbeats.mu.Lock()
				heartbeats.lost = true
				heartbeats.failure = err
				heartbeats.mu.Unlock()
				if heartbeats.cancel != nil {
					heartbeats.cancel()
				}
				return
			}
		}
	}
}

func (heartbeats *quotaLeaseHeartbeats) heartbeat() error {
	heartbeats.mu.Lock()
	heartbeats.count++
	ordinal := heartbeats.count
	leases := append([]store.DurableQuotaLease(nil), heartbeats.leases...)
	heartbeats.mu.Unlock()
	requests := make([]store.HeartbeatQuotaLeaseRequest, len(leases))
	deadline := leases[0].ExpiresAt
	for index, lease := range leases {
		if lease.ExpiresAt.Before(deadline) {
			deadline = lease.ExpiresAt
		}
		requests[index] = store.HeartbeatQuotaLeaseRequest{
			IdempotencyKey: fmt.Sprintf("stage-quota-heartbeat:%s:%d", lease.ID, ordinal), LeaseID: lease.ID,
			Owner: heartbeats.owner, FencingToken: lease.FencingToken, TTL: heartbeats.runtime.quotaLeaseTTL,
			Actor: heartbeats.actor, Reason: "durable stage quota lease heartbeat",
		}
	}
	updated, err := retryIdempotentLeaseHeartbeat(context.Background(), heartbeats.stopCh, heartbeats.runtime.quotaLeaseTTL/3, deadline,
		func(ctx context.Context) ([]store.DurableQuotaLease, error) {
			return heartbeats.runtime.heartbeatQuotaLeases(ctx, requests)
		})
	if err != nil {
		return err
	}
	heartbeats.mu.Lock()
	heartbeats.leases = append([]store.DurableQuotaLease(nil), updated...)
	heartbeats.mu.Unlock()
	return nil
}

// stageControlMonitor bridges durable ControlOperation facts into one scoped
// executor. It deliberately has no connection to a TUI or scheduler root
// context. A cooperative executor receives the signal first; only the frozen
// grace deadline escalates to context cancellation.
type stageControlMonitor struct {
	runtime        *FrozenExecutionRuntime
	runID          string
	stageAttemptID string
	actor          string
	cancel         context.CancelFunc
	signals        chan StageControlSignal
	listForRun     func(context.Context, string) ([]store.DurableControlOperation, error)

	mu           sync.Mutex
	operation    *store.DurableControlOperation
	pollErr      error
	checkpointID string
	checkpointOK bool
	forced       bool
	stopOnce     sync.Once
	stopCh       chan struct{}
	doneCh       chan struct{}
}

func (runtime *FrozenExecutionRuntime) startStageControlMonitor(ctx context.Context, cancel context.CancelFunc, runID, stageAttemptID, actor string) *stageControlMonitor {
	monitor := &stageControlMonitor{
		runtime: runtime, runID: runID, stageAttemptID: stageAttemptID, actor: actor, cancel: cancel,
		signals: make(chan StageControlSignal, 1), listForRun: runtime.services.Control.ListForRun,
		stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
	go monitor.run(ctx)
	return monitor
}

func (monitor *stageControlMonitor) run(ctx context.Context) {
	defer close(monitor.doneCh)
	monitor.poll()
	ticker := time.NewTicker(monitor.runtime.controlPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-monitor.stopCh:
			return
		case <-ticker.C:
			monitor.poll()
		}
	}
}

func (monitor *stageControlMonitor) stop() {
	if monitor == nil {
		return
	}
	monitor.stopOnce.Do(func() { close(monitor.stopCh) })
	<-monitor.doneCh
}

func (monitor *stageControlMonitor) poll() {
	operations, err := monitor.listForRun(context.Background(), monitor.runID)
	if err != nil {
		monitor.recordPollError(err)
		return
	}
	monitor.recordPollError(nil)
	for _, operation := range operations {
		if operation.Status != store.ControlOperationRequested && operation.Status != store.ControlOperationPropagating {
			continue
		}
		if operation.Action == store.ControlActionCancelStage && operation.StageAttemptID != monitor.stageAttemptID {
			continue
		}
		if operation.Action != store.ControlActionCancelStage && operation.StageAttemptID != "" {
			continue
		}
		if err := monitor.accept(operation); err != nil {
			monitor.recordPollError(err)
		}
		return
	}
}

func (monitor *stageControlMonitor) recordPollError(err error) {
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	monitor.pollErr = err
}

func (monitor *stageControlMonitor) failureError() error {
	if monitor == nil {
		return nil
	}
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	return monitor.pollErr
}

func controlPriority(action store.ControlAction) int {
	switch action {
	case store.ControlActionTerminate:
		return 3
	case store.ControlActionCancelStage:
		return 2
	case store.ControlActionPause:
		return 1
	default:
		return 0
	}
}

func (monitor *stageControlMonitor) accept(operation store.DurableControlOperation) error {
	if operation.Status == store.ControlOperationRequested {
		transitioned, err := monitor.runtime.services.Control.Transition(context.Background(), TransitionExecutionControlRequest{
			OperationID: operation.ID, ExpectedVersion: operation.Version, Status: store.ControlOperationPropagating,
			Actor: monitor.actor, Reason: "durable stage runtime received control request",
		})
		if err != nil {
			return err
		}
		operation = transitioned
	}
	if operation.Status != store.ControlOperationPropagating {
		return nil
	}
	monitor.mu.Lock()
	if monitor.operation != nil && controlPriority(monitor.operation.Action) >= controlPriority(operation.Action) {
		monitor.mu.Unlock()
		return nil
	}
	copyOperation := operation
	monitor.operation = &copyOperation
	monitor.mu.Unlock()
	select {
	case monitor.signals <- StageControlSignal{Action: operation.Action, GracePeriod: operation.GracePeriod}:
	default:
	}
	go func(grace time.Duration) {
		if grace > 0 {
			timer := time.NewTimer(grace)
			defer timer.Stop()
			select {
			case <-monitor.stopCh:
				return
			case <-timer.C:
			}
		}
		monitor.mu.Lock()
		monitor.forced = true
		monitor.mu.Unlock()
		monitor.cancel()
	}(operation.GracePeriod)
	return nil
}

func (monitor *stageControlMonitor) current() *store.DurableControlOperation {
	if monitor == nil {
		return nil
	}
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	if monitor.operation == nil {
		return nil
	}
	copyOperation := *monitor.operation
	copyOperation.RuntimeReceipts = append([]store.RuntimeTerminationReceipt(nil), monitor.operation.RuntimeReceipts...)
	return &copyOperation
}

func (monitor *stageControlMonitor) checkpoint() (string, bool) {
	if monitor == nil {
		return "", false
	}
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	return monitor.checkpointID, monitor.checkpointOK
}

func (monitor *stageControlMonitor) markCheckpoint(id string, resumable bool) {
	if monitor == nil {
		return
	}
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	monitor.checkpointID = id
	monitor.checkpointOK = resumable
}

func (monitor *stageControlMonitor) wasForced() bool {
	if monitor == nil {
		return false
	}
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	return monitor.forced
}

type persistedStageCheckpointPayload struct {
	Format    string          `json:"format"`
	Resumable bool            `json:"resumable"`
	Payload   json.RawMessage `json:"payload"`
}

const persistedStageCheckpointFormat = "harbor.stage-checkpoint.v1"

func (runtime *FrozenExecutionRuntime) stageCheckpointWriter(stageAttemptID, nodeAttemptID, defaultInputDigest string, monitor *stageControlMonitor) func(context.Context, StageCheckpoint) (store.TurnCheckpoint, error) {
	return func(ctx context.Context, checkpoint StageCheckpoint) (store.TurnCheckpoint, error) {
		if checkpoint.Turn <= 0 || strings.TrimSpace(checkpoint.Substep) == "" {
			return store.TurnCheckpoint{}, fmt.Errorf("%w: checkpoint turn and substep are required", ErrInvalidStageExecution)
		}
		inputDigest := strings.TrimSpace(checkpoint.InputDigest)
		if inputDigest == "" {
			inputDigest = defaultInputDigest
		}
		if inputDigest == "" {
			return store.TurnCheckpoint{}, fmt.Errorf("%w: checkpoint input digest is required", ErrInvalidStageExecution)
		}
		payload := json.RawMessage(checkpoint.PayloadJSON)
		if len(payload) == 0 {
			payload = json.RawMessage(`{}`)
		}
		if !json.Valid(payload) {
			return store.TurnCheckpoint{}, fmt.Errorf("%w: checkpoint payload must be JSON", ErrInvalidStageExecution)
		}
		encoded, err := json.Marshal(persistedStageCheckpointPayload{Format: persistedStageCheckpointFormat, Resumable: checkpoint.Resumable, Payload: payload})
		if err != nil {
			return store.TurnCheckpoint{}, err
		}
		created, err := runtime.core.store.CreateTurnCheckpoint(ctx, store.CreateTurnCheckpointRequest{
			NodeAttemptID: nodeAttemptID, Turn: checkpoint.Turn, Substep: checkpoint.Substep, InputDigest: inputDigest,
			ArtifactID: checkpoint.ArtifactID, PayloadJSON: string(encoded), Actor: "runtime", Reason: "durable executor checkpoint",
		})
		if err != nil {
			return store.TurnCheckpoint{}, err
		}
		completed, err := runtime.core.store.TransitionTurnCheckpoint(ctx, store.TransitionTurnCheckpointRequest{
			CheckpointID: created.ID, ExpectedVersion: created.Version, Status: store.TurnCheckpointCompleted,
			ArtifactID: checkpoint.ArtifactID, PayloadJSON: string(encoded), Actor: "runtime", Reason: "durable checkpoint completed",
		})
		if err != nil {
			return store.TurnCheckpoint{}, err
		}
		monitor.markCheckpoint(completed.ID, checkpoint.Resumable)
		return completed, nil
	}
}

func (runtime *FrozenExecutionRuntime) pendingStageControl(ctx context.Context, runID, stageAttemptID, actor string) (*store.DurableControlOperation, error) {
	operations, err := runtime.services.Control.ListForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	var selected *store.DurableControlOperation
	for index := range operations {
		candidate := operations[index]
		if candidate.Status != store.ControlOperationRequested && candidate.Status != store.ControlOperationPropagating {
			continue
		}
		if candidate.Action == store.ControlActionCancelStage && candidate.StageAttemptID != stageAttemptID {
			continue
		}
		if candidate.Action != store.ControlActionCancelStage && candidate.StageAttemptID != "" {
			continue
		}
		if selected == nil || controlPriority(candidate.Action) > controlPriority(selected.Action) {
			copyCandidate := candidate
			selected = &copyCandidate
		}
	}
	if selected == nil || selected.Status != store.ControlOperationRequested {
		return selected, nil
	}
	transitioned, err := runtime.services.Control.Transition(ctx, TransitionExecutionControlRequest{
		OperationID: selected.ID, ExpectedVersion: selected.Version, Status: store.ControlOperationPropagating,
		Actor: actor, Reason: "durable stage runtime received control request",
	})
	if err != nil {
		return nil, err
	}
	return &transitioned, nil
}

func (runtime *FrozenExecutionRuntime) handlePreStageControl(ctx context.Context, job store.DurableJob, run store.WorkflowRun, attempt store.StageAttempt) (store.JobState, bool, error) {
	operation, err := runtime.pendingStageControl(ctx, run.ID, attempt.ID, job.CreatedBy)
	if err != nil || operation == nil {
		return "", operation != nil && err != nil, err
	}
	current, err := runtime.core.store.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		return store.JobFailed, true, err
	}
	if current == nil {
		return store.JobFailed, true, fmt.Errorf("%w: workflow run %s", ErrLifecycleNotFound, run.ID)
	}
	switch operation.Action {
	case store.ControlActionPause:
		if current.Status != store.WorkflowRunPauseRequested {
			return "", false, nil
		}
		started, err := runtime.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionRunning, Actor: job.CreatedBy, Reason: "pause reached queued stage boundary"})
		if err != nil {
			return store.JobFailed, true, err
		}
		if _, err := runtime.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{StageAttemptID: started.ID, ExpectedVersion: started.Version, ExecutionStatus: store.StageExecutionInterrupted, ErrorText: "pause requested before stage execution", Actor: job.CreatedBy, Reason: operation.Reason}); err != nil {
			return store.JobFailed, true, err
		}
		pausing, err := runtime.core.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: current.ID, ExpectedVersion: current.Version, Status: store.WorkflowRunPausing, Actor: operation.Actor, Reason: operation.Reason})
		if err != nil {
			return store.JobFailed, true, err
		}
		if _, err := runtime.core.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: pausing.ID, ExpectedVersion: pausing.Version, Status: store.WorkflowRunPaused, Actor: operation.Actor, Reason: operation.Reason}); err != nil {
			return store.JobFailed, true, err
		}
		if _, err := runtime.ackControl(ctx, *operation, "", "", true, false, "pause acknowledged at queued stage boundary"); err != nil {
			return store.JobFailed, true, err
		}
		return store.JobInterrupted, true, nil
	case store.ControlActionCancelStage:
		if _, err := runtime.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionCanceled, ErrorText: "stage cancellation requested before execution", Actor: operation.Actor, Reason: operation.Reason}); err != nil {
			return store.JobFailed, true, err
		}
		if _, err := runtime.ackControl(ctx, *operation, "", "", true, false, "stage cancellation acknowledged before execution"); err != nil {
			return store.JobFailed, true, err
		}
		if err := runtime.finishRunWithStatus(ctx, run.ID, store.WorkflowRunFailedRecoverable, job.CreatedBy, "stage was canceled before execution"); err != nil {
			return store.JobFailed, true, err
		}
		return store.JobCanceled, true, nil
	case store.ControlActionTerminate:
		if _, err := runtime.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionCanceled, ErrorText: "run termination requested before stage execution", Actor: operation.Actor, Reason: operation.Reason}); err != nil {
			return store.JobFailed, true, err
		}
		if err := runtime.finishRunWithStatus(ctx, run.ID, store.WorkflowRunCanceled, operation.Actor, operation.Reason); err != nil {
			return store.JobFailed, true, err
		}
		if _, err := runtime.ackControl(ctx, *operation, "", "", true, false, "run termination acknowledged before stage execution"); err != nil {
			return store.JobFailed, true, err
		}
		return store.JobCanceled, true, nil
	default:
		return store.JobFailed, true, fmt.Errorf("%w: unsupported control action %q", ErrFrozenExecutionPayload, operation.Action)
	}
}

func (runtime *FrozenExecutionRuntime) projectStageTerminal(ctx context.Context, job store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, payload frozenStageExecutionPayload, subject workflowRunSubject, stage workflowkit.StageDescriptor, attempt store.StageAttempt, inputs []workflowkit.ArtifactBinding, reservation stageQuotaReservation, result StageExecutionResult, workerLeaseLost <-chan struct{}, monitor *stageControlMonitor) (store.JobState, error) {
	if channelClosed(workerLeaseLost) {
		return store.JobInDoubt, ErrDurableJobLeaseLost
	}
	reservation.stop()
	monitor.stop()
	var operation *store.DurableControlOperation
	if monitor != nil {
		operation = monitor.current()
	}
	var codeEdgeEffect *store.SideEffectOperation
	if isCodeEdgeEvaluatorStage(run, stage) {
		effect, effectErr := runtime.codeEdgeEvaluatorEffectAlreadyStarted(ctx, run, attempt, stage)
		if effectErr != nil {
			return store.JobFailed, effectErr
		}
		if effect != nil {
			codeEdgeEffect = effect
			if effect.State != store.SideEffectStarted || codeEdgeEvaluatorOutcomeIsUncertain(result, reservation, workerLeaseLost, monitor) {
				return runtime.projectCodeEdgeEvaluatorInDoubt(ctx, job, run, attempt, stage, reservation, *effect, operation, "CodeEdge evaluator terminal projection cannot prove the external outcome")
			}
		}
	}
	unknownOutcome := reservation.lost() || channelClosed(workerLeaseLost) || result.Outcome.Status == workflowkit.StatusInDoubt || monitor.failureError() != nil
	if operation != nil && operation.Action == store.ControlActionTerminate && stage.Effect == workflowkit.EffectExternalSideEffect && result.Outcome.Status != workflowkit.StatusCompleted {
		unknownOutcome = true
	}
	if unknownOutcome {
		settlementID, settlementErr := runtime.settleStageQuota(ctx, reservation, store.QuotaSettlementUncertain, job.CreatedBy, "stage outcome requires reconciliation")
		detail := result.ErrorText
		if heartbeatErr := reservation.heartbeats.failureError(); heartbeatErr != nil {
			detail = strings.TrimSpace(detail + "; quota heartbeat: " + heartbeatErr.Error())
		}
		if controlErr := monitor.failureError(); controlErr != nil {
			detail = strings.TrimSpace(detail + "; control monitor: " + controlErr.Error())
		}
		if settlementErr != nil {
			detail = strings.TrimSpace(detail + "; quota settlement requires reconciliation: " + settlementErr.Error())
		}
		if err := runtime.transitionStageInDoubt(ctx, attempt, job.CreatedBy, detail); err != nil {
			return store.JobFailed, err
		}
		if err := runtime.finishRunWithStatus(ctx, run.ID, store.WorkflowRunInDoubt, job.CreatedBy, "durable stage outcome is unknown"); err != nil {
			return store.JobFailed, err
		}
		if err := runtime.enqueueAutomaticRepairOutcome(ctx, run.ID, job.CreatedBy, "repair child run requires reconciliation"); err != nil {
			return store.JobFailed, err
		}
		if operation != nil {
			if _, err := runtime.requireControlReconcile(ctx, *operation, settlementID, "stage outcome may include an unknown side effect"); err != nil {
				return store.JobFailed, err
			}
		}
		return store.JobInDoubt, nil
	}

	if operation != nil && operation.Action == store.ControlActionCancelStage && result.Outcome.Status != workflowkit.StatusCompleted {
		result.Outcome = workflowkit.Outcome{Status: workflowkit.StatusCanceled}
		if result.ErrorText == "" {
			result.ErrorText = "stage cancellation acknowledged"
		}
	}
	if operation != nil && operation.Action == store.ControlActionTerminate && result.Outcome.Status != workflowkit.StatusCompleted {
		result.Outcome = workflowkit.Outcome{Status: workflowkit.StatusCanceled}
		if result.ErrorText == "" {
			result.ErrorText = "run termination acknowledged"
		}
	}

	manifestID := ""
	var manifest store.ArtifactManifest
	if len(result.Artifacts) != 0 {
		persist := persistStageEvidenceForSubject
		reason := "persist frozen stage diagnostic evidence"
		if result.Outcome.Status == workflowkit.StatusCompleted {
			persist = persistStageArtifactsForSubject
			reason = "persist frozen stage outputs"
		}
		persistedManifest, _, err := persist(ctx, runtime.core, run, subject, attempt, latestNodeAttempt(ctx, runtime.core.store, attempt.ID), stage, inputs, result.Artifacts, job.CreatedBy, reason)
		if err != nil {
			result = StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailurePermanent}, ErrorText: err.Error(), FailureClass: string(workflowkit.FailurePermanent)}
		} else {
			manifest = persistedManifest
			manifestID = manifest.ID
		}
	}
	if codeEdgeEffect != nil && codeEdgeEvaluatorOutcomeIsUncertain(result, reservation, workerLeaseLost, monitor) {
		return runtime.projectCodeEdgeEvaluatorInDoubt(ctx, job, run, attempt, stage, reservation, *codeEdgeEffect, operation, "CodeEdge evaluator evidence persistence cannot prove the external outcome")
	}

	stageStatus, verdict, failureClass, err := stageProjection(result.Outcome)
	if err != nil {
		return store.JobFailed, err
	}
	if codeEdgeEffect != nil && stageStatus == store.StageExecutionCompleted {
		if err := runtime.completeCodeEdgeEvaluatorEffect(ctx, run, attempt, stage, manifest, job.CreatedBy); err != nil {
			return runtime.projectCodeEdgeEvaluatorInDoubt(ctx, job, run, attempt, stage, reservation, *codeEdgeEffect, operation, "CodeEdge evaluator success fence could not be finalized: "+err.Error())
		}
		completedEffect, effectErr := runtime.codeEdgeEvaluatorEffectAlreadyStarted(ctx, run, attempt, stage)
		if effectErr != nil {
			return runtime.failRuntimeJob(ctx, job, effectErr)
		}
		if completedEffect == nil || completedEffect.State != store.SideEffectSucceeded {
			return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: completed CodeEdge evaluator has no succeeded side effect", ErrFrozenExecutionPayload))
		}
		codeEdgeEffect = completedEffect
		if err := runtime.completeTrustedCodeEdgeEvaluatorTrials(ctx, run, attempt, job.CreatedBy, "project direct completed CodeEdge evaluator trials"); err != nil {
			return runtime.projectCodeEdgeEvaluatorInDoubt(ctx, job, run, attempt, stage, reservation, *codeEdgeEffect, operation, "CodeEdge evaluator trusted trial projection could not be finalized: "+err.Error())
		}
	}
	settlementOutcome := store.QuotaSettlementCanceled
	if stageStatus == store.StageExecutionCompleted {
		settlementOutcome = store.QuotaSettlementCompleted
	}
	settlementID, settlementErr := runtime.settleStageQuota(ctx, reservation, settlementOutcome, job.CreatedBy, "settle frozen stage quota")
	if settlementErr != nil {
		return runtime.failRuntimeJob(ctx, job, settlementErr)
	}
	updated, err := runtime.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: stageStatus, Verdict: verdict,
		ArtifactManifestID: manifestID, ErrorText: result.ErrorText, FailureClass: failureClass,
		Actor: job.CreatedBy, Reason: "frozen stage terminal outcome",
	})
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	return runtime.afterStageTerminal(ctx, job, run, frozen, payload, stage, updated, operation, settlementID, monitor)
}

func latestNodeAttempt(ctx context.Context, dataStore *store.Store, stageAttemptID string) store.NodeAttempt {
	attempts, err := dataStore.ListNodeAttempts(ctx, stageAttemptID)
	if err != nil || len(attempts) == 0 {
		return store.NodeAttempt{}
	}
	return attempts[len(attempts)-1]
}

func stageProjection(outcome workflowkit.Outcome) (store.StageExecutionStatus, store.Verdict, string, error) {
	switch outcome.Status {
	case workflowkit.StatusCompleted:
		return store.StageExecutionCompleted, store.Verdict(outcome.Verdict), "", nil
	case workflowkit.StatusInfraFailed:
		return store.StageExecutionInfraFailed, "", string(outcome.Failure), nil
	case workflowkit.StatusInterrupted:
		return store.StageExecutionInterrupted, "", "", nil
	case workflowkit.StatusCanceled:
		return store.StageExecutionCanceled, "", "", nil
	default:
		return "", "", "", fmt.Errorf("%w: cannot project outcome %q", ErrFrozenExecutionPayload, outcome.Status)
	}
}

func (runtime *FrozenExecutionRuntime) transitionStageInDoubt(ctx context.Context, attempt store.StageAttempt, actor, detail string) error {
	_, err := runtime.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionInDoubt,
		ErrorText: detail, FailureClass: string(workflowkit.FailureUnknown), Actor: actor, Reason: "durable stage outcome requires reconciliation",
	})
	return err
}

func channelClosed(signal <-chan struct{}) bool {
	if signal == nil {
		return false
	}
	select {
	case <-signal:
		return true
	default:
		return false
	}
}

func (runtime *FrozenExecutionRuntime) settleStageQuota(ctx context.Context, reservation stageQuotaReservation, outcome store.QuotaSettlementOutcome, actor, reason string) (string, error) {
	leases := reservation.currentLeases()
	if len(leases) == 0 {
		return "", nil
	}
	sort.Slice(leases, func(left, right int) bool { return leases[left].ID < leases[right].ID })
	requests := make([]store.SettleQuotaLeaseRequest, len(leases))
	for index, lease := range leases {
		requests[index] = store.SettleQuotaLeaseRequest{
			IdempotencyKey: "stage-settlement:" + lease.ID + ":" + string(outcome), LeaseID: lease.ID,
			Owner: lease.Owner, FencingToken: lease.FencingToken, Outcome: outcome, Actor: actor, Reason: reason,
		}
	}
	settlements, err := retryIdempotentLeaseHeartbeat(ctx, nil, runtime.quotaLeaseTTL/3, quotaLeaseDeadline(leases),
		func(callCtx context.Context) ([]store.DurableQuotaSettlement, error) {
			return runtime.core.store.SettleQuotaLeases(callCtx, requests)
		})
	if err != nil || len(settlements) == 0 {
		return "", err
	}
	return settlements[0].ID, nil
}

func (runtime *FrozenExecutionRuntime) afterStageTerminal(ctx context.Context, job store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, payload frozenStageExecutionPayload, stage workflowkit.StageDescriptor, attempt store.StageAttempt, operation *store.DurableControlOperation, settlementID string, monitor *stageControlMonitor) (store.JobState, error) {
	if operation != nil {
		switch operation.Action {
		case store.ControlActionPause:
			checkpointID, resumable := monitor.checkpoint()
			if attempt.ExecutionStatus == store.StageExecutionInterrupted && !resumable {
				if _, err := runtime.failControl(ctx, *operation, "pause reached no resumable checkpoint"); err != nil {
					return store.JobFailed, err
				}
				if err := runtime.finishRunWithStatus(ctx, run.ID, store.WorkflowRunInterrupted, job.CreatedBy, "pause has no resumable checkpoint"); err != nil {
					return store.JobFailed, err
				}
				return store.JobInterrupted, nil
			}
			if err := runtime.pauseRun(ctx, run.ID, operation.Actor, operation.Reason); err != nil {
				return store.JobFailed, err
			}
			if _, err := runtime.ackControl(ctx, *operation, checkpointID, settlementID, !monitor.wasForced(), false, "pause acknowledged by durable stage runtime"); err != nil {
				return store.JobFailed, err
			}
			return store.JobInterrupted, nil
		case store.ControlActionCancelStage:
			if _, err := runtime.ackControl(ctx, *operation, "", settlementID, !monitor.wasForced(), false, "stage cancellation acknowledged by durable runtime"); err != nil {
				return store.JobFailed, err
			}
			if attempt.ExecutionStatus != store.StageExecutionCompleted {
				if err := runtime.finishRunWithStatus(ctx, run.ID, store.WorkflowRunFailedRecoverable, job.CreatedBy, "stage cancellation requires continuation"); err != nil {
					return store.JobFailed, err
				}
				return store.JobCanceled, nil
			}
		case store.ControlActionTerminate:
			if err := runtime.finishRunWithStatus(ctx, run.ID, store.WorkflowRunCanceled, operation.Actor, operation.Reason); err != nil {
				return store.JobFailed, err
			}
			if _, err := runtime.ackControl(ctx, *operation, "", settlementID, !monitor.wasForced(), false, "run termination acknowledged by durable runtime"); err != nil {
				return store.JobFailed, err
			}
			return store.JobCanceled, nil
		}
	}

	switch attempt.ExecutionStatus {
	case store.StageExecutionCompleted:
		switch attempt.Verdict {
		case store.VerdictPass, store.VerdictAdvisory:
			// The StageAttempt has already committed its artifact, quota, and
			// terminal-result projection. The following coordinator therefore
			// gates on that state, rather than waiting for this worker's later
			// JobSucceeded delivery projection.
			if err := runtime.enqueueNextCoordinator(ctx, job, run, frozen, payload); err != nil {
				return runtime.failRuntimeJob(ctx, job, err)
			}
			return store.JobSucceeded, nil
		case store.VerdictNeedsRepair:
			if err := runtime.finishRunWithStatus(ctx, run.ID, store.WorkflowRunWaitingContinuation, job.CreatedBy, "stage verdict requires repair continuation"); err != nil {
				return store.JobFailed, err
			}
			if err := runtime.enqueueAutomaticRepairOutcome(ctx, run.ID, job.CreatedBy, "stage verdict queued automatic repair progression"); err != nil {
				return store.JobFailed, err
			}
			return runtime.finishContinuationForRunOutcome(ctx, payload.ContinuationExecutionID, store.ContinuationExecutionCompleted, job.CreatedBy, "continuation reached repairable verdict")
		case store.VerdictReject:
			if err := runtime.finishRunWithStatus(ctx, run.ID, store.WorkflowRunFailedTerminal, job.CreatedBy, "stage verdict rejected task"); err != nil {
				return store.JobFailed, err
			}
			if err := runtime.enqueueAutomaticRepairOutcome(ctx, run.ID, job.CreatedBy, "stage verdict requires human repair decision"); err != nil {
				return store.JobFailed, err
			}
			return runtime.finishContinuationForRunOutcome(ctx, payload.ContinuationExecutionID, store.ContinuationExecutionFailed, job.CreatedBy, "continuation reached terminal rejection")
		}
	case store.StageExecutionInfraFailed:
		if err := runtime.finishRunWithStatus(ctx, run.ID, store.WorkflowRunFailedRecoverable, job.CreatedBy, "stage infrastructure failure is recoverable"); err != nil {
			return store.JobFailed, err
		}
		return runtime.finishContinuationForRunOutcome(ctx, payload.ContinuationExecutionID, store.ContinuationExecutionFailed, job.CreatedBy, "continuation stage infrastructure failure")
	case store.StageExecutionInterrupted:
		if err := runtime.finishRunWithStatus(ctx, run.ID, store.WorkflowRunInterrupted, job.CreatedBy, "worker stage was interrupted"); err != nil {
			return store.JobFailed, err
		}
		return runtime.finishContinuationForRunOutcome(ctx, payload.ContinuationExecutionID, store.ContinuationExecutionFailed, job.CreatedBy, "continuation stage interrupted")
	case store.StageExecutionCanceled:
		if err := runtime.finishRunWithStatus(ctx, run.ID, store.WorkflowRunFailedRecoverable, job.CreatedBy, "stage canceled and requires continuation"); err != nil {
			return store.JobFailed, err
		}
		return runtime.finishContinuationForRunOutcome(ctx, payload.ContinuationExecutionID, store.ContinuationExecutionCanceled, job.CreatedBy, "continuation stage canceled")
	}
	return store.JobFailed, fmt.Errorf("%w: unexpected terminal stage status %s", ErrFrozenExecutionPayload, attempt.ExecutionStatus)
}

// runtimePlanForStagePayload rehydrates only the exact frozen plan that
// created a StageAttempt. A stage worker must never infer a later plan from
// current user input or the current template.
func (runtime *FrozenExecutionRuntime) runtimePlanForStagePayload(ctx context.Context, payload frozenStageExecutionPayload, frozen frozenRunDefinition) (runtimeExecutionPlan, error) {
	if payload.ContinuationPlanID == "" {
		if payload.ContinuationExecutionID != "" {
			return runtimeExecutionPlan{}, fmt.Errorf("%w: initial stage has a continuation execution", ErrFrozenExecutionPayload)
		}
		return initialRuntimeExecutionPlan(frozen)
	}
	if payload.ContinuationExecutionID == "" {
		return runtimeExecutionPlan{}, fmt.Errorf("%w: continuation stage has no execution", ErrFrozenExecutionPayload)
	}
	execution, err := runtime.core.store.GetContinuationExecution(ctx, payload.ContinuationExecutionID)
	if err != nil {
		return runtimeExecutionPlan{}, err
	}
	if execution == nil || execution.PlanID != payload.ContinuationPlanID || execution.RunID != payload.RunID {
		return runtimeExecutionPlan{}, fmt.Errorf("%w: continuation stage payload does not match execution", ErrFrozenExecutionPayload)
	}
	plan, err := runtime.services.Continuations.GetTaskContinuationPlan(ctx, payload.ContinuationPlanID)
	if err != nil {
		return runtimeExecutionPlan{}, err
	}
	return continuationRuntimeExecutionPlan(plan, frozen.Workflow, frozen.QuotaPolicy, execution.ID)
}

// enqueueNextCoordinator records the durable handoff after a progressing
// StageAttempt. Its idempotency key includes the predecessor job, so recovery
// can repair a crash between the StageAttempt terminal projection and this
// enqueue without ever duplicating downstream work.
func (runtime *FrozenExecutionRuntime) enqueueNextCoordinator(ctx context.Context, stageJob store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, payload frozenStageExecutionPayload) error {
	if payload.ContinuationPlanID == "" {
		encoded, err := json.Marshal(workflowRunExecutionPayload{
			Format: workflowRunExecutionPayloadFormat, RunID: run.ID, DefinitionHash: run.DefinitionHash,
			ExecutionSpecFingerprint: frozen.ExecutionSpecFingerprint, QuotaPolicy: frozen.QuotaPolicy.Clone(),
		})
		if err != nil {
			return err
		}
		_, err = runtime.core.store.CreateDurableJob(ctx, store.CreateDurableJobRequest{
			CommandType: "workflow_run.execute", EntityType: "workflow_run", EntityID: run.ID, RunID: run.ID,
			PayloadJSON: string(encoded), IdempotencyKey: "workflow-run-next:" + run.ID + ":" + stageJob.ID,
			Actor: stageJob.CreatedBy, Reason: "advance frozen workflow after stage " + payload.StageAttemptID,
		})
		return err
	}
	execution, err := runtime.core.store.GetContinuationExecution(ctx, payload.ContinuationExecutionID)
	if err != nil {
		return err
	}
	if execution == nil || execution.RunID != run.ID || execution.PlanID != payload.ContinuationPlanID {
		return fmt.Errorf("%w: continuation coordinator binding", ErrFrozenExecutionPayload)
	}
	_, err = runtime.core.store.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "task_continuation.execute", EntityType: "continuation_execution", EntityID: execution.ID, RunID: run.ID,
		PayloadJSON: execution.PayloadJSON, IdempotencyKey: "continuation-next:" + execution.ID + ":" + stageJob.ID,
		Actor: stageJob.CreatedBy, Reason: "advance frozen continuation after stage " + payload.StageAttemptID,
	})
	return err
}

func (runtime *FrozenExecutionRuntime) completeRunIfSatisfied(ctx context.Context, run store.WorkflowRun, frozen frozenRunDefinition, plan runtimeExecutionPlan, actor string) (bool, error) {
	current, err := runtime.core.store.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		return false, err
	}
	if current == nil {
		return false, fmt.Errorf("%w: workflow run %s", ErrLifecycleNotFound, run.ID)
	}
	if current.Status == store.WorkflowRunSucceeded {
		return true, nil
	}
	if current.Status != store.WorkflowRunRunning {
		return false, fmt.Errorf("%w: workflow run %s cannot complete from %s", ErrFrozenExecutionPayload, current.ID, current.Status)
	}

	// Defend the final projection against a corrupted coordinator payload even
	// though scheduleNextBatch already verified every schedule batch.
	latest := make(map[workflowkit.StageKey]store.StageAttempt)
	attempts, err := runtime.core.store.ListStageAttemptsForRun(ctx, run.ID)
	if err != nil {
		return false, err
	}
	for _, attempt := range attempts {
		key := workflowkit.StageKey(attempt.StageKey)
		previous, found := latest[key]
		if !found || attempt.Ordinal > previous.Ordinal || (attempt.Ordinal == previous.Ordinal && attempt.ID > previous.ID) {
			latest[key] = attempt
		}
	}
	for _, transition := range plan.Transitions {
		if transition.Disposition != workflowkit.DispositionSchedule {
			continue
		}
		attempt, found := latest[transition.NodeID]
		if !found || attempt.ExecutionStatus != store.StageExecutionCompleted || (attempt.Verdict != store.VerdictPass && attempt.Verdict != store.VerdictAdvisory) {
			return false, fmt.Errorf("%w: scheduled stage %s is not completed with a progressing verdict", ErrFrozenExecutionPayload, transition.NodeID)
		}
	}
	if err := frozen.Workflow.Validate(); err != nil {
		return false, err
	}
	if err := runtime.finishRunWithStatus(ctx, run.ID, store.WorkflowRunSucceeded, actor, "all frozen schedule batches completed"); err != nil {
		return false, err
	}
	// A Standard authoring parent is a source/session Run. It has no
	// TaskRevision lifecycle to repair after materialization.
	if current.SubjectKind == store.WorkflowRunSubjectAuthoringSession {
		return true, nil
	}
	return true, runtime.enqueueAutomaticRepairOutcome(ctx, run.ID, actor, "repair child run passed all frozen checks")
}

func (runtime *FrozenExecutionRuntime) enqueueAutomaticRepairOutcome(ctx context.Context, runID, actor, reason string) error {
	if runtime.core == nil || runtime.core.repairs == nil {
		return fmt.Errorf("repair loop service is not configured")
	}
	_, _, err := runtime.core.repairs.EnqueueRunOutcome(ctx, runID, actor, reason)
	return err
}

func (runtime *FrozenExecutionRuntime) finishRunWithStatus(ctx context.Context, runID string, target store.WorkflowRunStatus, actor, reason string) error {
	run, err := runtime.core.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("%w: workflow run %s", ErrLifecycleNotFound, runID)
	}
	if run.Status == target {
		return nil
	}
	if terminalWorkflowRunStatus(run.Status) {
		return fmt.Errorf("%w: workflow run %s is terminal as %s", ErrFrozenExecutionPayload, run.ID, run.Status)
	}
	if _, err := runtime.core.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: target, Actor: actor, Reason: reason,
	}); err != nil {
		return err
	}
	if !terminalWorkflowRunStatus(target) {
		return nil
	}
	return runtime.finishActiveRunAttempts(ctx, run.ID, target, actor, reason)
}

func terminalWorkflowRunStatus(status store.WorkflowRunStatus) bool {
	switch status {
	case store.WorkflowRunSucceeded, store.WorkflowRunFailedRecoverable, store.WorkflowRunFailedTerminal,
		store.WorkflowRunCanceled, store.WorkflowRunInterrupted:
		return true
	default:
		return false
	}
}

func (runtime *FrozenExecutionRuntime) finishActiveRunAttempts(ctx context.Context, runID string, runStatus store.WorkflowRunStatus, actor, reason string) error {
	attempts, err := runtime.core.store.ListRunAttempts(ctx, runID)
	if err != nil {
		return err
	}
	var target store.RunAttemptStatus
	switch runStatus {
	case store.WorkflowRunSucceeded:
		target = store.RunAttemptSucceeded
	case store.WorkflowRunCanceled:
		target = store.RunAttemptCanceled
	case store.WorkflowRunInterrupted:
		target = store.RunAttemptInterrupted
	default:
		target = store.RunAttemptFailed
	}
	for _, attempt := range attempts {
		if attempt.Status != store.RunAttemptRunning {
			continue
		}
		if _, err := runtime.core.store.TransitionRunAttempt(ctx, store.TransitionRunAttemptRequest{
			RunAttemptID: attempt.ID, ExpectedVersion: attempt.Version, Status: target, Actor: actor, Reason: reason,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (runtime *FrozenExecutionRuntime) pauseRun(ctx context.Context, runID, actor, reason string) error {
	run, err := runtime.core.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("%w: workflow run %s", ErrLifecycleNotFound, runID)
	}
	if run.Status == store.WorkflowRunPaused {
		return nil
	}
	if run.Status != store.WorkflowRunPauseRequested {
		return fmt.Errorf("%w: workflow run %s cannot pause from %s", ErrFrozenExecutionPayload, run.ID, run.Status)
	}
	pausing, err := runtime.core.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunPausing, Actor: actor, Reason: reason,
	})
	if err != nil {
		return err
	}
	_, err = runtime.core.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: pausing.ID, ExpectedVersion: pausing.Version, Status: store.WorkflowRunPaused, Actor: actor, Reason: reason,
	})
	return err
}

func (runtime *FrozenExecutionRuntime) failControl(ctx context.Context, operation store.DurableControlOperation, detail string) (store.DurableControlOperation, error) {
	current, err := runtime.services.Control.Get(ctx, operation.ID)
	if err != nil {
		return store.DurableControlOperation{}, err
	}
	if current.Status == store.ControlOperationFailed {
		return current, nil
	}
	if current.Status != store.ControlOperationRequested && current.Status != store.ControlOperationPropagating {
		return store.DurableControlOperation{}, fmt.Errorf("%w: control operation %s cannot fail from %s", ErrFrozenExecutionPayload, current.ID, current.Status)
	}
	return runtime.services.Control.Transition(ctx, TransitionExecutionControlRequest{
		OperationID: current.ID, ExpectedVersion: current.Version, Status: store.ControlOperationFailed,
		FailureReason: detail, Actor: current.Actor, Reason: detail,
	})
}

func (runtime *FrozenExecutionRuntime) requireControlReconcile(ctx context.Context, operation store.DurableControlOperation, settlementID, detail string) (store.DurableControlOperation, error) {
	current, err := runtime.services.Control.Get(ctx, operation.ID)
	if err != nil {
		return store.DurableControlOperation{}, err
	}
	if current.Status == store.ControlOperationReconcileRequired {
		return current, nil
	}
	if current.Status != store.ControlOperationRequested && current.Status != store.ControlOperationPropagating {
		return store.DurableControlOperation{}, fmt.Errorf("%w: control operation %s cannot require reconciliation from %s", ErrFrozenExecutionPayload, current.ID, current.Status)
	}
	if current.Status == store.ControlOperationRequested {
		transitioned, transitionErr := runtime.services.Control.Transition(ctx, TransitionExecutionControlRequest{
			OperationID: current.ID, ExpectedVersion: current.Version, Status: store.ControlOperationPropagating,
			Actor: current.Actor, Reason: "durable runtime began control reconciliation",
		})
		if transitionErr != nil {
			return store.DurableControlOperation{}, transitionErr
		}
		current = transitioned
	}
	return runtime.services.Control.Transition(ctx, TransitionExecutionControlRequest{
		OperationID: current.ID, ExpectedVersion: current.Version, Status: store.ControlOperationReconcileRequired,
		QuotaSettlementID: settlementID, Actor: current.Actor, Reason: detail,
		RuntimeReceipts: []store.RuntimeTerminationReceipt{{
			RuntimeScopeID: "durable-job:" + current.RunID, ObservedAt: runtime.core.now().UTC(),
			Graceful: false, ExternalOutcomeUnknown: true, PayloadJSON: mustJSON(map[string]any{"detail": detail}),
		}},
	})
}

func (runtime *FrozenExecutionRuntime) finishContinuationForRunOutcome(ctx context.Context, executionID string, target store.ContinuationExecutionState, actor, reason string) (store.JobState, error) {
	if executionID == "" {
		return store.JobSucceeded, nil
	}
	execution, err := runtime.core.store.GetContinuationExecution(ctx, executionID)
	if err != nil {
		return store.JobFailed, err
	}
	if execution == nil {
		return store.JobFailed, fmt.Errorf("%w: continuation execution %s", ErrLifecycleNotFound, executionID)
	}
	if execution.State == target {
		return store.JobSucceeded, nil
	}
	if execution.State != store.ContinuationExecutionRunning {
		return store.JobFailed, fmt.Errorf("%w: continuation execution %s is %s", ErrFrozenExecutionPayload, execution.ID, execution.State)
	}
	if _, err := runtime.core.store.TransitionContinuationExecution(ctx, store.TransitionContinuationExecutionRequest{
		ContinuationExecutionID: execution.ID, ExpectedVersion: execution.Version, State: target, Actor: actor, Reason: reason,
	}); err != nil {
		return store.JobFailed, err
	}
	return store.JobSucceeded, nil
}
