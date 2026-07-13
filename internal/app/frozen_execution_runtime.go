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
)

// FrozenExecutionRuntimeConfig supplies the controlled implementations that
// may execute V2 stage descriptors. Registrations are resolved by the exact
// plugin ID/version frozen into a Run; the runtime never asks the legacy
// Runner, TaskScheduler, or current template for a fallback implementation.
type FrozenExecutionRuntimeConfig struct {
	Services            *LifecycleServices
	Registry            *workflowkit.ControlledPluginRegistry[StageExecutor]
	QuotaLeaseTTL       time.Duration
	ControlPollInterval time.Duration
}

// FrozenExecutionRuntime is the durable coordinator plus StageAttempt worker
// handler. Coordinator jobs expand only their immutable schedule. Each actual
// execution belongs to a separately claimed stage_attempt.execute job.
type FrozenExecutionRuntime struct {
	core                *lifecycleServiceCore
	services            *LifecycleServices
	registry            *workflowkit.ControlledPluginRegistry[StageExecutor]
	quotaLeaseTTL       time.Duration
	controlPollInterval time.Duration
}

// NewFrozenExecutionRuntime constructs the V2-only durable job handler.
func NewFrozenExecutionRuntime(config FrozenExecutionRuntimeConfig) (*FrozenExecutionRuntime, error) {
	if config.Services == nil || config.Services.core == nil || config.Services.core.store == nil {
		return nil, fmt.Errorf("%w: lifecycle services are required", ErrFrozenExecutionRuntimeConfiguration)
	}
	if config.Registry == nil {
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
		core:                config.Services.core,
		services:            config.Services,
		registry:            config.Registry,
		quotaLeaseTTL:       config.QuotaLeaseTTL,
		controlPollInterval: config.ControlPollInterval,
	}, nil
}

var _ DurableJobHandler = (*FrozenExecutionRuntime)(nil)

// HandleDurableJob dispatches only the three immutable V2 execution payloads.
// Domain failures are projected into the run/stage state machine and return a
// successful worker delivery so unrelated queued jobs continue. Payload or
// storage integrity failures return JobFailed after conservatively marking the
// affected run in_doubt where possible.
func (runtime *FrozenExecutionRuntime) HandleDurableJob(ctx context.Context, execution DurableJobExecution) (store.JobState, error) {
	if err := runtime.validate(); err != nil {
		return store.JobFailed, err
	}
	if execution.Claim.Job == nil {
		return store.JobFailed, fmt.Errorf("%w: durable claim has no job", ErrFrozenExecutionPayload)
	}
	job := *execution.Claim.Job
	switch job.CommandType {
	case "workflow_run.execute":
		return runtime.handleWorkflowRun(ctx, execution, job)
	case "task_continuation.execute":
		return runtime.handleContinuation(ctx, execution, job)
	case "stage_attempt.execute":
		return runtime.handleStageAttempt(ctx, execution, job)
	default:
		return store.JobFailed, fmt.Errorf("%w: unsupported command type %q", ErrFrozenExecutionPayload, job.CommandType)
	}
}

func (runtime *FrozenExecutionRuntime) validate() error {
	if runtime == nil || runtime.core == nil || runtime.core.store == nil || runtime.services == nil || runtime.registry == nil {
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
	QuotaPolicy             workflowadapter.ResolvedQuotaPolicy
}

func (plan runtimeExecutionPlan) stageTransition(key workflowkit.StageKey) (workflowkit.NodeTransition, bool) {
	transition, found := plan.Transitions[key]
	return transition.Clone(), found
}

func (runtime *FrozenExecutionRuntime) handleWorkflowRun(ctx context.Context, execution DurableJobExecution, job store.DurableJob) (store.JobState, error) {
	var payload workflowRunExecutionPayload
	if err := decodeStrictJSON(job.PayloadJSON, &payload); err != nil {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("decode workflow run payload: %w", err))
	}
	if payload.Format != workflowRunExecutionPayloadFormat || payload.RunID != job.RunID || payload.RunID != job.EntityID {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("%w: workflow run job does not bind its payload", ErrFrozenExecutionPayload))
	}
	run, frozen, err := runtime.loadFrozenRun(ctx, payload.RunID, payload.DefinitionHash, payload.QuotaPolicy)
	if err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	if state, handled, err := runtime.handlePreExecutionControl(ctx, job, run); handled || err != nil {
		return state, err
	}
	if err := runtime.transitionRunToRunning(ctx, run, job.CreatedBy, "begin frozen workflow run"); err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	plan, err := initialRuntimeExecutionPlan(frozen)
	if err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	if err := runtime.ensureRunAttempt(ctx, run.ID, plan.ExecutionKey, job.CreatedBy, "begin workflow run attempt"); err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	if err := runtime.scheduleNextBatch(ctx, job, run, frozen, plan); err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	return store.JobSucceeded, nil
}

func (runtime *FrozenExecutionRuntime) handleContinuation(ctx context.Context, execution DurableJobExecution, job store.DurableJob) (store.JobState, error) {
	var payload continuationExecutionPayload
	if err := decodeStrictJSON(job.PayloadJSON, &payload); err != nil {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("decode continuation execution payload: %w", err))
	}
	if payload.Format != continuationExecutionFormat || payload.RunID != job.RunID || job.EntityType != "continuation_execution" || payload.PlanID == "" {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("%w: continuation job does not bind its payload", ErrFrozenExecutionPayload))
	}
	continuation, err := runtime.core.store.GetContinuationExecution(ctx, job.EntityID)
	if err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	if continuation == nil || continuation.RunID != job.RunID || continuation.PlanID != payload.PlanID || continuation.PayloadJSON != job.PayloadJSON {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("%w: continuation execution does not match durable job", ErrFrozenExecutionPayload))
	}
	run, frozen, err := runtime.loadFrozenRun(ctx, payload.RunID, "", payload.QuotaPolicy)
	if err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	plan, err := runtime.services.Continuations.GetTaskContinuationPlan(ctx, payload.PlanID)
	if err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	if payload.PlanFingerprint != plan.Fingerprint() {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("%w: continuation plan fingerprint mismatch", ErrFrozenExecutionPayload))
	}
	snapshot := plan.Snapshot()
	if snapshot.TargetRunRelation == workflowkit.RelationSameRunAttempt && snapshot.SourceRunID != run.ID {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("%w: same-run continuation source does not match job run", ErrFrozenExecutionPayload))
	}
	if err := runtime.transitionContinuationToRunning(ctx, *continuation, job.CreatedBy); err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	if state, handled, err := runtime.handlePreExecutionControl(ctx, job, run); handled || err != nil {
		return state, err
	}
	if err := runtime.transitionRunToRunning(ctx, run, job.CreatedBy, "begin frozen continuation"); err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	if payload.SourceRunID != snapshot.SourceRunID {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("%w: continuation source run mismatch", ErrFrozenExecutionPayload))
	}
	runtimePlan, err := continuationRuntimeExecutionPlan(plan, frozen.Workflow, frozen.QuotaPolicy, continuation.ID)
	if err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	if err := runtime.ensureRunAttempt(ctx, run.ID, runtimePlan.ExecutionKey, job.CreatedBy, "begin continuation attempt"); err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	if err := runtime.scheduleNextBatch(ctx, job, run, frozen, runtimePlan); err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	return store.JobSucceeded, nil
}

func (runtime *FrozenExecutionRuntime) loadFrozenRun(ctx context.Context, runID, definitionHash string, payloadPolicy workflowadapter.ResolvedQuotaPolicy) (store.WorkflowRun, frozenRunDefinition, error) {
	run, err := runtime.core.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return store.WorkflowRun{}, frozenRunDefinition{}, err
	}
	if run == nil {
		return store.WorkflowRun{}, frozenRunDefinition{}, fmt.Errorf("%w: workflow run %s", ErrLifecycleNotFound, runID)
	}
	frozen, err := decodeFrozenRunDefinition(*run)
	if err != nil {
		return store.WorkflowRun{}, frozenRunDefinition{}, err
	}
	if definitionHash != "" && definitionHash != run.DefinitionHash {
		return store.WorkflowRun{}, frozenRunDefinition{}, fmt.Errorf("%w: workflow definition hash mismatch", ErrFrozenExecutionPayload)
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
	order, err := frozen.Workflow.TopologicalStages()
	if err != nil {
		return runtimeExecutionPlan{}, err
	}
	plan := runtimeExecutionPlan{
		ExecutionKey: "initial",
		Workflow:     frozen.Workflow.Clone(),
		Transitions:  make(map[workflowkit.StageKey]workflowkit.NodeTransition, len(order)),
		QuotaPolicy:  frozen.QuotaPolicy.Clone(),
	}
	for index, key := range order {
		plan.Transitions[key] = workflowkit.NodeTransition{NodeID: key, FromGeneration: 0, ToGeneration: 0, Disposition: workflowkit.DispositionSchedule}
		plan.Schedule = append(plan.Schedule, workflowkit.ScheduleBatch{ID: fmt.Sprintf("initial-%03d-%s", index+1, key), NodeIDs: []workflowkit.NodeID{key}})
	}
	return plan, nil
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

func (runtime *FrozenExecutionRuntime) failMalformedJob(ctx context.Context, job store.DurableJob, cause error) (store.JobState, error) {
	if job.RunID != "" {
		if run, err := runtime.core.store.GetWorkflowRun(context.Background(), job.RunID); err == nil && run != nil {
			_, _ = runtime.core.store.TransitionWorkflowRun(context.Background(), store.TransitionWorkflowRunRequest{
				RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunInDoubt,
				Actor: job.CreatedBy, Reason: "durable execution payload requires reconciliation",
			})
		}
	}
	return store.JobFailed, cause
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
	current := operation
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

// scheduleNextBatch expands at most one immutable schedule batch. This makes
// the durable job table the handoff boundary between batches: a process crash
// cannot cause a later batch to run before its persisted predecessor has a
// trustworthy terminal StageAttempt.
func (runtime *FrozenExecutionRuntime) scheduleNextBatch(ctx context.Context, parentJob store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, plan runtimeExecutionPlan) error {
	if err := plan.Workflow.Validate(); err != nil {
		return fmt.Errorf("validate runtime workflow: %w", err)
	}
	if frozen.QuotaPolicy.Fingerprint != plan.QuotaPolicy.Fingerprint || frozen.QuotaPolicy.ID != plan.QuotaPolicy.ID || frozen.QuotaPolicy.Version != plan.QuotaPolicy.Version {
		return fmt.Errorf("%w: execution plan quota policy differs from frozen run", ErrFrozenExecutionPayload)
	}
	stageJobs, err := runtime.stageJobsForPlan(ctx, run.ID, plan.ExecutionKey)
	if err != nil {
		return err
	}
	for _, batch := range plan.Schedule {
		ready, completed, err := runtime.batchState(ctx, batch, plan, stageJobs)
		if err != nil {
			return err
		}
		if completed {
			continue
		}
		if !ready {
			return nil
		}
		for _, nodeID := range batch.NodeIDs {
			stageKey := workflowkit.StageKey(nodeID)
			if _, exists := stageJobs[stageKey]; exists {
				continue
			}
			stage, found := plan.Workflow.Stage(stageKey)
			if !found {
				return fmt.Errorf("%w: batch %s refers to unknown stage %q", ErrFrozenExecutionPayload, batch.ID, stageKey)
			}
			transition, found := plan.stageTransition(stageKey)
			if !found || transition.Disposition != workflowkit.DispositionSchedule {
				return fmt.Errorf("%w: batch %s stage %q is not frozen for scheduling", ErrFrozenExecutionPayload, batch.ID, stageKey)
			}
			if err := runtime.enqueueStageAttempt(ctx, parentJob, run, frozen, plan, stage, transition); err != nil {
				return err
			}
		}
		return nil
	}
	return runtime.completeExecutionIfSatisfied(ctx, parentJob, run, frozen, plan)
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

func (runtime *FrozenExecutionRuntime) batchState(ctx context.Context, batch workflowkit.ScheduleBatch, plan runtimeExecutionPlan, jobs map[workflowkit.StageKey]runtimePlannedStageJob) (ready, completed bool, err error) {
	if len(batch.NodeIDs) == 0 {
		return false, false, fmt.Errorf("%w: empty schedule batch %q", ErrFrozenExecutionPayload, batch.ID)
	}
	allComplete := true
	for _, nodeID := range batch.NodeIDs {
		stageKey := workflowkit.StageKey(nodeID)
		transition, found := plan.stageTransition(stageKey)
		if !found || transition.Disposition != workflowkit.DispositionSchedule {
			return false, false, fmt.Errorf("%w: schedule batch %s has invalid stage %q", ErrFrozenExecutionPayload, batch.ID, stageKey)
		}
		planned, exists := jobs[stageKey]
		if !exists {
			allComplete = false
			continue
		}
		stageAttempt, getErr := runtime.core.store.GetStageAttempt(ctx, planned.Payload.StageAttemptID)
		if getErr != nil {
			return false, false, getErr
		}
		if stageAttempt == nil {
			return false, false, fmt.Errorf("%w: stage job %s refers to missing attempt", ErrFrozenExecutionPayload, planned.Job.ID)
		}
		if stageAttempt.ExecutionStatus == store.StageExecutionCompleted && verdictAllowsProgress(stageAttempt.Verdict) && planned.Job.State == store.JobSucceeded {
			continue
		}
		allComplete = false
		if planned.Job.State == store.JobQueued || planned.Job.State == store.JobRunning || planned.Job.State == store.JobPauseRequested || planned.Job.State == store.JobCancelRequested || planned.Job.State == store.JobStopRequested {
			return false, false, nil
		}
		// A terminal non-success stage was already projected to a durable Run
		// outcome by its handler. Do not manufacture a later batch from it.
		return false, false, nil
	}
	return true, allComplete, nil
}

func verdictAllowsProgress(verdict store.Verdict) bool {
	return verdict == store.VerdictPass || verdict == store.VerdictAdvisory
}

func stageExecutionKey(payload frozenStageExecutionPayload) string {
	if payload.ContinuationPlanID != "" {
		return payload.ContinuationPlanID
	}
	return "initial"
}

func (runtime *FrozenExecutionRuntime) enqueueStageAttempt(ctx context.Context, parentJob store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, plan runtimeExecutionPlan, stage workflowkit.StageDescriptor, transition workflowkit.NodeTransition) error {
	revision, err := runtime.core.store.GetTaskRevision(ctx, run.RevisionID)
	if err != nil {
		return err
	}
	if revision == nil || revision.TaskID != run.TaskID {
		return fmt.Errorf("%w: run %s has no matching immutable revision", ErrFrozenExecutionPayload, run.ID)
	}
	inputs, err := resolveStageInputs(ctx, runtime.core.store, runtime.core.objects, run, *revision, stage)
	if err != nil {
		return fmt.Errorf("resolve immutable inputs for stage %q: %w", stage.Key, err)
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
	return runtime.completeRunIfSatisfied(ctx, run, frozen, parentJob.CreatedBy)
}

func (runtime *FrozenExecutionRuntime) handleStageAttempt(ctx context.Context, execution DurableJobExecution, job store.DurableJob) (store.JobState, error) {
	var payload frozenStageExecutionPayload
	if err := decodeStrictJSON(job.PayloadJSON, &payload); err != nil {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("decode stage execution payload: %w", err))
	}
	if payload.Format != frozenStageExecutionPayloadFormat || payload.RunID != job.RunID || payload.StageAttemptID != job.StageAttemptID || payload.StageAttemptID != job.EntityID || payload.Generation < 0 {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("%w: stage job does not bind its payload", ErrFrozenExecutionPayload))
	}
	run, frozen, err := runtime.loadFrozenRun(ctx, payload.RunID, payload.DefinitionHash, payload.QuotaPolicy)
	if err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	stage, found := frozen.Workflow.Stage(payload.StageKey)
	if !found {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("%w: frozen workflow omits stage %q", ErrFrozenExecutionPayload, payload.StageKey))
	}
	loadedStageAttempt, err := runtime.core.store.GetStageAttempt(ctx, payload.StageAttemptID)
	if err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	if loadedStageAttempt == nil || loadedStageAttempt.RunID != run.ID || loadedStageAttempt.StageKey != string(stage.Key) {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("%w: stage attempt does not match durable job", ErrFrozenExecutionPayload))
	}
	if err := runtime.validateStageAttemptPlanBinding(*loadedStageAttempt, payload); err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	if loadedStageAttempt.ExecutionStatus == store.StageExecutionCompleted || loadedStageAttempt.ExecutionStatus == store.StageExecutionInfraFailed || loadedStageAttempt.ExecutionStatus == store.StageExecutionInterrupted || loadedStageAttempt.ExecutionStatus == store.StageExecutionCanceled {
		return store.JobSucceeded, nil
	}
	stageAttempt := *loadedStageAttempt
	revision, err := runtime.core.store.GetTaskRevision(ctx, run.RevisionID)
	if err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	if revision == nil || revision.TaskID != run.TaskID {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("%w: stage run revision is unavailable", ErrFrozenExecutionPayload))
	}
	if state, handled, controlErr := runtime.handlePreStageControl(ctx, job, run, *revision, *stageAttempt); handled || controlErr != nil {
		return state, controlErr
	}
	if stageAttempt.ExecutionStatus == store.StageExecutionQueued {
		stageAttempt, err = runtime.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
			StageAttemptID: stageAttempt.ID, ExpectedVersion: stageAttempt.Version, ExecutionStatus: store.StageExecutionRunning,
			Actor: job.CreatedBy, Reason: "durable stage worker started",
		})
		if err != nil {
			return runtime.failMalformedJob(ctx, job, err)
		}
	}
	if stageAttempt.ExecutionStatus != store.StageExecutionRunning {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("%w: stage attempt %s is %s", ErrFrozenExecutionPayload, stageAttempt.ID, stageAttempt.ExecutionStatus))
	}

	reservation, admissionErr := runtime.admitStageQuota(ctx, execution, job, run, frozen.QuotaPolicy, stage)
	if admissionErr != nil {
		return runtime.projectAdmissionFailure(ctx, job, run, frozen, payload, *revision, stage, &stageAttempt, admissionErr)
	}
	defer reservation.stop()

	stageContext, cancel := context.WithTimeout(ctx, stage.Budget.MaxElapsed)
	defer cancel()
	monitor := runtime.startStageControlMonitor(stageContext, cancel, run.ID, stageAttempt.ID, job.CreatedBy)
	defer monitor.stop()

	inputs, err := resolveStageInputs(stageContext, runtime.core.store, runtime.core.objects, run, *revision, stage)
	if err != nil {
		return runtime.projectStageTerminal(ctx, job, run, frozen, payload, *revision, stage, stageAttempt, reservation, StageExecutionResult{
			Outcome: workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailurePermanent}, ErrorText: err.Error(), FailureClass: string(workflowkit.FailurePermanent),
		}, nil, monitor)
	}
	inputFingerprint, err := workflowkit.FingerprintArtifactBindings(inputs)
	if err != nil {
		return runtime.failMalformedJob(ctx, job, err)
	}
	if string(inputFingerprint) != stageAttempt.InputFingerprint {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("%w: stage attempt input fingerprint drift", ErrFrozenExecutionPayload))
	}

	var result StageExecutionResult
	for ordinal := 1; ordinal <= stage.Budget.MaxAttempts; ordinal++ {
		nodeAttempt, nodeErr := runtime.createRunningNodeAttempt(ctx, stageAttempt, stage, payload.Generation, ordinal, job.CreatedBy)
		if nodeErr != nil {
			return runtime.failMalformedJob(ctx, job, nodeErr)
		}
		attemptContext, attemptCancel := context.WithTimeout(stageContext, stage.Budget.AttemptTimeout)
		result, nodeErr = runtime.executeFrozenStage(attemptContext, run, *revision, stageAttempt, nodeAttempt, stage, inputs, monitor)
		attemptCancel()
		result = normalizeStageExecutionResult(result, nodeErr, attemptContext.Err())
		if err := result.Outcome.Validate(); err != nil {
			result = StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailurePolicy}, ErrorText: err.Error(), FailureClass: string(workflowkit.FailurePolicy)}
		}
		if result.Outcome.Status == workflowkit.StatusCompleted && !stage.Verdicts.Allows(result.Outcome.Verdict) {
			result = StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailurePolicy}, ErrorText: fmt.Sprintf("stage emitted frozen-disallowed verdict %q", result.Outcome.Verdict), FailureClass: string(workflowkit.FailurePolicy)}
		}
		if result.Outcome.Status != workflowkit.StatusCompleted && len(result.Artifacts) != 0 {
			result = StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailurePolicy}, ErrorText: "non-completed stage result cannot publish artifacts", FailureClass: string(workflowkit.FailurePolicy)}
		}
		if err := runtime.transitionNodeAttempt(ctx, nodeAttempt, result.Outcome, result.ErrorText, job.CreatedBy); err != nil {
			return runtime.failMalformedJob(ctx, job, err)
		}
		if result.Outcome.Status == workflowkit.StatusInfraFailed && stageRetryable(stage, result.Outcome.Failure) && ordinal < stage.Budget.MaxAttempts && stageContext.Err() == nil {
			if err := waitStageRetry(stageContext, RetryDelay(stage, ordinal+1)); err != nil {
				result = StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInterrupted}, ErrorText: err.Error()}
				break
			}
			continue
		}
		break
	}
	return runtime.projectStageTerminal(ctx, job, run, frozen, payload, *revision, stage, stageAttempt, reservation, result, execution.LeaseLost, monitor)
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

func (runtime *FrozenExecutionRuntime) executeFrozenStage(ctx context.Context, run store.WorkflowRun, revision store.TaskRevision, stageAttempt store.StageAttempt, nodeAttempt store.NodeAttempt, stage workflowkit.StageDescriptor, inputs []workflowkit.ArtifactBinding, monitor *stageControlMonitor) (StageExecutionResult, error) {
	executor, err := runtime.registry.ResolveStagePlugin(stage)
	if err != nil {
		return StageExecutionResult{}, fmt.Errorf("resolve frozen stage plugin %s@%s: %w", stage.Plugin.ID, stage.Plugin.Version, err)
	}
	return executor.ExecuteStage(ctx, StageExecutionRequest{
		Run: run, Revision: revision, StageAttempt: stageAttempt, NodeAttempt: nodeAttempt, Stage: stage, Inputs: inputs,
		Checkpoint: runtime.stageCheckpointWriter(stageAttempt.ID, nodeAttempt.ID, stageAttempt.InputFingerprint, monitor),
		Control: monitor.signals,
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

func stageRetryable(stage workflowkit.StageDescriptor, failure workflowkit.FailureClass) bool {
	for _, candidate := range stage.Retry.Retryable {
		if candidate == failure {
			return true
		}
	}
	return false
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
}

func (reservation stageQuotaReservation) stop() {
	if reservation.heartbeats != nil {
		reservation.heartbeats.stop()
	}
}

func (reservation stageQuotaReservation) lost() bool {
	return reservation.heartbeats != nil && reservation.heartbeats.lostLease()
}

func (runtime *FrozenExecutionRuntime) admitStageQuota(ctx context.Context, execution DurableJobExecution, job store.DurableJob, run store.WorkflowRun, policy workflowadapter.ResolvedQuotaPolicy, stage workflowkit.StageDescriptor) (stageQuotaReservation, error) {
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
	decision, err := runtime.core.store.AdmitTaskActorQuota(ctx, store.AdmitTaskActorQuotaRequest{
		IdempotencyKey:    "stage-admission:" + job.ID,
		TaskID:            run.TaskID,
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
	reservation := stageQuotaReservation{leases: append([]store.DurableQuotaLease(nil), decision.Leases...)}
	reservation.heartbeats = newQuotaLeaseHeartbeats(runtime, execution.Claim.Owner, job.CreatedBy, reservation.leases)
	reservation.heartbeats.start()
	for _, lease := range reservation.leases {
		if _, err := runtime.core.store.RecordQuotaUsage(ctx, store.RecordQuotaUsageRequest{
			OperationKey: "stage-usage:" + job.ID + ":" + lease.ID,
			LeaseID:      lease.ID,
			FencingToken: lease.FencingToken,
			Units:        lease.ReservedUnits,
			OccurredAt:   lease.CreatedAt,
			Actor:        job.CreatedBy,
			Reason:       "frozen stage execution began",
		}); err != nil {
			reservation.stop()
			return stageQuotaReservation{}, err
		}
	}
	return reservation, nil
}

func (runtime *FrozenExecutionRuntime) projectAdmissionFailure(ctx context.Context, job store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, payload frozenStageExecutionPayload, revision store.TaskRevision, stage workflowkit.StageDescriptor, attempt *store.StageAttempt, admissionErr error) (store.JobState, error) {
	if attempt == nil {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("%w: stage attempt is required for admission projection", ErrFrozenExecutionPayload))
	}
	result := StageExecutionResult{
		Outcome:      workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailurePolicy},
		ErrorText:    admissionErr.Error(),
		FailureClass: string(workflowkit.FailurePolicy),
	}
	return runtime.projectStageTerminal(ctx, job, run, frozen, payload, revision, stage, *attempt, stageQuotaReservation{}, result, nil, nil)
}

// quotaLeaseHeartbeats keeps admission fences alive independently from the
// worker dispatch lease. A lost quota fence is treated as uncertain rather
// than allowing a stale executor to settle or overwrite accounting facts.
type quotaLeaseHeartbeats struct {
	runtime *FrozenExecutionRuntime
	owner   string
	actor   string
	leases  []store.DurableQuotaLease

	mu     sync.Mutex
	lost   bool
	once   sync.Once
	stopCh chan struct{}
	doneCh chan struct{}
	count  uint64
}

func newQuotaLeaseHeartbeats(runtime *FrozenExecutionRuntime, owner, actor string, leases []store.DurableQuotaLease) *quotaLeaseHeartbeats {
	return &quotaLeaseHeartbeats{
		runtime: runtime, owner: owner, actor: actor, leases: append([]store.DurableQuotaLease(nil), leases...),
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
				heartbeats.mu.Unlock()
				return
			}
		}
	}
}

func (heartbeats *quotaLeaseHeartbeats) heartbeat() error {
	for _, lease := range heartbeats.leases {
		heartbeats.mu.Lock()
		heartbeats.count++
		ordinal := heartbeats.count
		heartbeats.mu.Unlock()
		if _, err := heartbeats.runtime.core.store.HeartbeatQuotaLease(context.Background(), store.HeartbeatQuotaLeaseRequest{
			IdempotencyKey: fmt.Sprintf("stage-quota-heartbeat:%s:%d", lease.ID, ordinal), LeaseID: lease.ID,
			Owner: heartbeats.owner, FencingToken: lease.FencingToken, TTL: heartbeats.runtime.quotaLeaseTTL,
			Actor: heartbeats.actor, Reason: "durable stage quota lease heartbeat",
		}); err != nil {
			return err
		}
	}
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

	mu           sync.Mutex
	operation    *store.DurableControlOperation
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
		signals: make(chan StageControlSignal, 1), stopCh: make(chan struct{}), doneCh: make(chan struct{}),
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
	operations, err := monitor.runtime.services.Control.ListForRun(context.Background(), monitor.runID)
	if err != nil {
		return
	}
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
		monitor.accept(operation)
		return
	}
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

func (monitor *stageControlMonitor) accept(operation store.DurableControlOperation) {
	if operation.Status == store.ControlOperationRequested {
		transitioned, err := monitor.runtime.services.Control.Transition(context.Background(), TransitionExecutionControlRequest{
			OperationID: operation.ID, ExpectedVersion: operation.Version, Status: store.ControlOperationPropagating,
			Actor: monitor.actor, Reason: "durable stage runtime received control request",
		})
		if err != nil {
			return
		}
		operation = transitioned
	}
	if operation.Status != store.ControlOperationPropagating {
		return
	}
	monitor.mu.Lock()
	if monitor.operation != nil && controlPriority(monitor.operation.Action) >= controlPriority(operation.Action) {
		monitor.mu.Unlock()
		return
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
