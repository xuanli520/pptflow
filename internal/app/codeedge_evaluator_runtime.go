package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	codeEdgeEvaluatorTrialCount                    = 4
	codeEdgeEvaluatorEffectKind                    = "codeedge.phase1.evaluator.v1"
	codeEdgeEvaluatorEffectPayloadFormat           = "harbor.codeedge-evaluator-effect.v1"
	codeEdgeEvaluatorReconciliationObservationForm = "harbor.codeedge-evaluator-reconciliation.v1"
)

// codeEdgeEvaluatorEffectPayload is deliberately limited to durable identity
// facts. Evaluator receipts and result schemas remain owned by the existing
// CodeEdge compliance projection.
type codeEdgeEvaluatorEffectPayload struct {
	Format           string                            `json:"format"`
	RunID            string                            `json:"run_id"`
	StageAttemptID   string                            `json:"stage_attempt_id"`
	StageKey         workflowkit.StageKey              `json:"stage_key"`
	InputFingerprint workflowkit.Fingerprint           `json:"input_fingerprint"`
	Trials           []codeEdgeEvaluatorTrialReference `json:"trials"`
}

type codeEdgeEvaluatorTrialReference struct {
	Ordinal          int    `json:"ordinal"`
	TrialExecutionID string `json:"trial_execution_id"`
	TrialAttemptID   string `json:"trial_attempt_id"`
}

type codeEdgeEvaluatorEffectFence struct {
	Operation store.SideEffectOperation
	Invoke    bool
}

type codeEdgeEvaluatorReconciliationObservation struct {
	Format                string `json:"format"`
	RunID                 string `json:"run_id"`
	StageAttemptID        string `json:"stage_attempt_id"`
	StageKey              string `json:"stage_key"`
	SideEffectOperationID string `json:"side_effect_operation_id"`
}

func isCodeEdgeEvaluatorStage(run store.WorkflowRun, stage workflowkit.StageDescriptor) bool {
	return workflowadapter.IsCodeEdgeEvaluatorWorkflowTemplate(workflowadapter.TemplateReference{
		ID: run.WorkflowTemplateID, Version: run.WorkflowTemplateVersion,
	}) && isCodeEdgeEvaluatorStageKey(stage.Key)
}

func isCodeEdgeEvaluatorStageKey(key workflowkit.StageKey) bool {
	return key == workflowkit.StageKey(workflowadapter.HarborRunQwen) || key == workflowkit.StageKey(workflowadapter.HarborRunOpus)
}

func isCodeEdgeEvaluatorNode(workflow workflowkit.WorkflowDescriptor, key workflowkit.NodeID) bool {
	return workflowadapter.IsCodeEdgeEvaluatorWorkflowTemplate(workflowadapter.TemplateReference{
		ID: workflow.ID, Version: workflow.Version,
	}) && isCodeEdgeEvaluatorStageKey(workflowkit.StageKey(key))
}

func validateCodeEdgeEvaluatorBudget(stage workflowkit.StageDescriptor) error {
	if stage.Budget.MaxAttempts != 1 {
		return fmt.Errorf("%w: CodeEdge evaluator stage %q requires budget.max_attempts=1", ErrFrozenExecutionPayload, stage.Key)
	}
	return nil
}

func codeEdgeEvaluatorOperationKey(runID, stageAttemptID string) string {
	return "codeedge-evaluator:v1:" + runID + ":" + stageAttemptID
}

func codeEdgeEvaluatorReconciliationKey(operationKey string) string {
	return "codeedge-evaluator-reconcile:v1:" + operationKey
}

// prepareCodeEdgeEvaluatorEffect establishes the durable fence immediately
// before the one evaluator invocation. Once the operation is started, a
// later delivery cannot prove the invocation did not happen and must never
// invoke the provider again.
func (runtime *FrozenExecutionRuntime) prepareCodeEdgeEvaluatorEffect(ctx context.Context, job store.DurableJob, run store.WorkflowRun, stageAttempt store.StageAttempt, stage workflowkit.StageDescriptor) (codeEdgeEvaluatorEffectFence, error) {
	if err := validateCodeEdgeEvaluatorBudget(stage); err != nil {
		return codeEdgeEvaluatorEffectFence{}, err
	}
	if !isCodeEdgeEvaluatorStage(run, stage) || stageAttempt.RunID != run.ID || stageAttempt.StageKey != string(stage.Key) {
		return codeEdgeEvaluatorEffectFence{}, fmt.Errorf("%w: CodeEdge evaluator effect has mismatched stage binding", ErrFrozenExecutionPayload)
	}
	operationKey := codeEdgeEvaluatorOperationKey(run.ID, stageAttempt.ID)
	existing, err := runtime.core.store.GetSideEffectOperationByOperationKey(ctx, operationKey)
	if err != nil {
		return codeEdgeEvaluatorEffectFence{}, err
	}
	if existing != nil {
		if _, err := runtime.validateCodeEdgeEvaluatorEffect(ctx, *existing, run, stageAttempt, stage); err != nil {
			return codeEdgeEvaluatorEffectFence{}, err
		}
		return runtime.codeEdgeEvaluatorEffectFence(ctx, *existing, job.CreatedBy)
	}

	trials, err := runtime.preallocateCodeEdgeEvaluatorTrials(ctx, run, stageAttempt, job.CreatedBy)
	if err != nil {
		return codeEdgeEvaluatorEffectFence{}, err
	}
	payload, err := codeEdgeEvaluatorEffectPayloadFor(run, stageAttempt, stage, trials)
	if err != nil {
		return codeEdgeEvaluatorEffectFence{}, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return codeEdgeEvaluatorEffectFence{}, err
	}
	operation, err := runtime.core.store.CreateSideEffectOperation(ctx, store.CreateSideEffectOperationRequest{
		OperationKey:   operationKey,
		RunID:          run.ID,
		StageAttemptID: stageAttempt.ID,
		EffectKind:     codeEdgeEvaluatorEffectKind,
		IdempotencyKey: operationKey,
		SourceDigest:   stageAttempt.InputFingerprint,
		PayloadJSON:    string(encoded),
		Actor:          job.CreatedBy,
		Reason:         "prepare CodeEdge evaluator invocation fence",
	})
	if err != nil {
		return codeEdgeEvaluatorEffectFence{}, err
	}
	if _, err := runtime.validateCodeEdgeEvaluatorEffect(ctx, operation, run, stageAttempt, stage); err != nil {
		return codeEdgeEvaluatorEffectFence{}, err
	}
	return runtime.codeEdgeEvaluatorEffectFence(ctx, operation, job.CreatedBy)
}

func (runtime *FrozenExecutionRuntime) codeEdgeEvaluatorEffectFence(ctx context.Context, operation store.SideEffectOperation, actor string) (codeEdgeEvaluatorEffectFence, error) {
	switch operation.State {
	case store.SideEffectPrepared:
		started, err := runtime.core.store.TransitionSideEffectOperation(ctx, store.TransitionSideEffectOperationRequest{
			OperationID: operation.ID, ExpectedVersion: operation.Version, State: store.SideEffectStarted,
			Actor: actor, Reason: "begin CodeEdge evaluator invocation after durable trial allocation",
		})
		if err == nil {
			return codeEdgeEvaluatorEffectFence{Operation: started, Invoke: true}, nil
		}
		if !errors.Is(err, store.ErrOptimisticLock) {
			return codeEdgeEvaluatorEffectFence{}, err
		}
		current, lookupErr := runtime.core.store.GetSideEffectOperation(ctx, operation.ID)
		if lookupErr != nil {
			return codeEdgeEvaluatorEffectFence{}, lookupErr
		}
		if current == nil {
			return codeEdgeEvaluatorEffectFence{}, fmt.Errorf("%w: CodeEdge evaluator side effect %s disappeared", ErrFrozenExecutionPayload, operation.ID)
		}
		operation = *current
		if operation.State == store.SideEffectStarted || operation.State == store.SideEffectUnknown || operation.State == store.SideEffectSucceeded {
			return codeEdgeEvaluatorEffectFence{Operation: operation}, nil
		}
		return codeEdgeEvaluatorEffectFence{}, fmt.Errorf("%w: CodeEdge evaluator side effect %s raced to %s", ErrFrozenExecutionPayload, operation.ID, operation.State)
	case store.SideEffectStarted, store.SideEffectUnknown, store.SideEffectSucceeded:
		return codeEdgeEvaluatorEffectFence{Operation: operation}, nil
	case store.SideEffectFailed:
		return codeEdgeEvaluatorEffectFence{}, fmt.Errorf("%w: CodeEdge evaluator side effect %s failed before a safe invocation", ErrFrozenExecutionPayload, operation.ID)
	default:
		return codeEdgeEvaluatorEffectFence{}, fmt.Errorf("%w: CodeEdge evaluator side effect %s has unsupported state %s", ErrFrozenExecutionPayload, operation.ID, operation.State)
	}
}

func (runtime *FrozenExecutionRuntime) preallocateCodeEdgeEvaluatorTrials(ctx context.Context, run store.WorkflowRun, stageAttempt store.StageAttempt, actor string) ([]codeEdgeEvaluatorTrialReference, error) {
	executions, err := runtime.core.store.ListTrialExecutionsForStageAttempt(ctx, stageAttempt.ID)
	if err != nil {
		return nil, err
	}
	if len(executions) > codeEdgeEvaluatorTrialCount {
		return nil, fmt.Errorf("%w: CodeEdge evaluator stage %s has more than four TrialExecutions", ErrFrozenExecutionPayload, stageAttempt.ID)
	}
	byOrdinal := make(map[int]store.TrialExecution, codeEdgeEvaluatorTrialCount)
	for _, execution := range executions {
		if err := validateCodeEdgeTrialExecution(execution, run, stageAttempt); err != nil {
			return nil, err
		}
		if _, duplicate := byOrdinal[execution.Ordinal]; duplicate {
			return nil, fmt.Errorf("%w: CodeEdge evaluator stage %s has duplicate TrialExecution ordinal %d", ErrFrozenExecutionPayload, stageAttempt.ID, execution.Ordinal)
		}
		byOrdinal[execution.Ordinal] = execution
	}

	references := make([]codeEdgeEvaluatorTrialReference, 0, codeEdgeEvaluatorTrialCount)
	for ordinal := 1; ordinal <= codeEdgeEvaluatorTrialCount; ordinal++ {
		execution, found := byOrdinal[ordinal]
		if !found {
			id, idErr := store.NewUUIDv7()
			if idErr != nil {
				return nil, idErr
			}
			created, createErr := runtime.core.store.CreateTrialExecution(ctx, store.CreateTrialExecutionRequest{
				ID: id, RunID: run.ID, StageAttemptID: stageAttempt.ID, StageKey: stageAttempt.StageKey, Ordinal: ordinal,
				Actor: actor, Reason: "preallocate CodeEdge evaluator logical trial",
			})
			if createErr != nil {
				return nil, createErr
			}
			execution = created
		}
		attempts, listErr := runtime.core.store.ListTrialAttemptsForTrialExecution(ctx, execution.ID)
		if listErr != nil {
			return nil, listErr
		}
		if len(attempts) > 1 {
			return nil, fmt.Errorf("%w: CodeEdge evaluator TrialExecution %s has technical retries before its first invocation", ErrFrozenExecutionPayload, execution.ID)
		}
		var attempt store.TrialAttempt
		if len(attempts) == 0 {
			id, idErr := store.NewUUIDv7()
			if idErr != nil {
				return nil, idErr
			}
			created, createErr := runtime.core.store.CreateTrialAttempt(ctx, store.CreateTrialAttemptRequest{
				ID: id, TrialExecutionID: execution.ID, Ordinal: 1, Actor: actor,
				Reason: "preallocate CodeEdge evaluator initial technical trial attempt",
			})
			if createErr != nil {
				return nil, createErr
			}
			attempt = created
		} else {
			attempt = attempts[0]
		}
		if attempt.Ordinal != 1 || attempt.RetryOfTrialAttemptID != "" {
			return nil, fmt.Errorf("%w: CodeEdge evaluator TrialExecution %s has an invalid initial TrialAttempt", ErrFrozenExecutionPayload, execution.ID)
		}
		switch attempt.Status {
		case store.TrialAttemptQueued, store.TrialAttemptWaiting:
			started, transitionErr := runtime.core.store.TransitionTrialAttempt(ctx, store.TransitionTrialAttemptRequest{
				TrialAttemptID: attempt.ID, ExpectedVersion: attempt.Version, Status: store.TrialAttemptRunning,
				Actor: actor, Reason: "begin preallocated CodeEdge evaluator technical trial attempt",
			})
			if transitionErr != nil {
				return nil, transitionErr
			}
			attempt = started
		case store.TrialAttemptRunning:
		default:
			return nil, fmt.Errorf("%w: CodeEdge evaluator initial TrialAttempt %s is %s before invocation", ErrFrozenExecutionPayload, attempt.ID, attempt.Status)
		}
		current, getErr := runtime.core.store.GetTrialExecution(ctx, execution.ID)
		if getErr != nil {
			return nil, getErr
		}
		if current == nil || current.Status != store.TrialExecutionRunning {
			return nil, fmt.Errorf("%w: CodeEdge evaluator TrialExecution %s did not reach running before invocation", ErrFrozenExecutionPayload, execution.ID)
		}
		references = append(references, codeEdgeEvaluatorTrialReference{Ordinal: ordinal, TrialExecutionID: execution.ID, TrialAttemptID: attempt.ID})
	}
	return references, nil
}

func validateCodeEdgeTrialExecution(execution store.TrialExecution, run store.WorkflowRun, stageAttempt store.StageAttempt) error {
	if execution.RunID != run.ID || execution.StageAttemptID != stageAttempt.ID || execution.StageKey != stageAttempt.StageKey || execution.Ordinal < 1 || execution.Ordinal > codeEdgeEvaluatorTrialCount {
		return fmt.Errorf("%w: CodeEdge evaluator TrialExecution %s does not match its frozen stage", ErrFrozenExecutionPayload, execution.ID)
	}
	return nil
}

func codeEdgeEvaluatorEffectPayloadFor(run store.WorkflowRun, stageAttempt store.StageAttempt, stage workflowkit.StageDescriptor, trials []codeEdgeEvaluatorTrialReference) (codeEdgeEvaluatorEffectPayload, error) {
	if len(trials) != codeEdgeEvaluatorTrialCount {
		return codeEdgeEvaluatorEffectPayload{}, fmt.Errorf("%w: CodeEdge evaluator requires exactly four preallocated trials", ErrFrozenExecutionPayload)
	}
	trials = append([]codeEdgeEvaluatorTrialReference(nil), trials...)
	sort.Slice(trials, func(left, right int) bool { return trials[left].Ordinal < trials[right].Ordinal })
	for index, trial := range trials {
		if trial.Ordinal != index+1 || trial.TrialExecutionID == "" || trial.TrialAttemptID == "" {
			return codeEdgeEvaluatorEffectPayload{}, fmt.Errorf("%w: invalid CodeEdge evaluator trial fence payload", ErrFrozenExecutionPayload)
		}
	}
	return codeEdgeEvaluatorEffectPayload{
		Format: codeEdgeEvaluatorEffectPayloadFormat, RunID: run.ID, StageAttemptID: stageAttempt.ID,
		StageKey: stage.Key, InputFingerprint: workflowkit.Fingerprint(stageAttempt.InputFingerprint), Trials: trials,
	}, nil
}

func (runtime *FrozenExecutionRuntime) validateCodeEdgeEvaluatorEffect(ctx context.Context, operation store.SideEffectOperation, run store.WorkflowRun, stageAttempt store.StageAttempt, stage workflowkit.StageDescriptor) ([]codeEdgeEvaluatorTrialReference, error) {
	if operation.OperationKey != codeEdgeEvaluatorOperationKey(run.ID, stageAttempt.ID) || operation.RunID != run.ID || operation.StageAttemptID != stageAttempt.ID ||
		operation.EffectKind != codeEdgeEvaluatorEffectKind || operation.IdempotencyKey != operation.OperationKey || operation.SourceDigest != stageAttempt.InputFingerprint {
		return nil, fmt.Errorf("%w: CodeEdge evaluator side effect does not match its frozen stage", ErrFrozenExecutionPayload)
	}
	var payload codeEdgeEvaluatorEffectPayload
	if err := decodeStrictJSON(operation.PayloadJSON, &payload); err != nil {
		return nil, fmt.Errorf("%w: decode CodeEdge evaluator side effect payload: %v", ErrFrozenExecutionPayload, err)
	}
	if payload.Format != codeEdgeEvaluatorEffectPayloadFormat || payload.RunID != run.ID || payload.StageAttemptID != stageAttempt.ID || payload.StageKey != stage.Key || string(payload.InputFingerprint) != stageAttempt.InputFingerprint {
		return nil, fmt.Errorf("%w: CodeEdge evaluator side effect payload does not match its frozen stage", ErrFrozenExecutionPayload)
	}
	if _, err := codeEdgeEvaluatorEffectPayloadFor(run, stageAttempt, stage, payload.Trials); err != nil {
		return nil, err
	}
	executions, err := runtime.core.store.ListTrialExecutionsForStageAttempt(ctx, stageAttempt.ID)
	if err != nil {
		return nil, err
	}
	if len(executions) != codeEdgeEvaluatorTrialCount {
		return nil, fmt.Errorf("%w: CodeEdge evaluator effect has %d persisted TrialExecutions, want four", ErrFrozenExecutionPayload, len(executions))
	}
	byOrdinal := make(map[int]store.TrialExecution, len(executions))
	for _, execution := range executions {
		if err := validateCodeEdgeTrialExecution(execution, run, stageAttempt); err != nil {
			return nil, err
		}
		if _, duplicate := byOrdinal[execution.Ordinal]; duplicate {
			return nil, fmt.Errorf("%w: CodeEdge evaluator effect has duplicate TrialExecution ordinal %d", ErrFrozenExecutionPayload, execution.Ordinal)
		}
		byOrdinal[execution.Ordinal] = execution
	}
	for _, reference := range payload.Trials {
		execution, found := byOrdinal[reference.Ordinal]
		if !found || execution.ID != reference.TrialExecutionID {
			return nil, fmt.Errorf("%w: CodeEdge evaluator effect trial %d does not match its persisted TrialExecution", ErrFrozenExecutionPayload, reference.Ordinal)
		}
		attempts, listErr := runtime.core.store.ListTrialAttemptsForTrialExecution(ctx, execution.ID)
		if listErr != nil {
			return nil, listErr
		}
		foundAttempt := false
		for _, attempt := range attempts {
			if attempt.ID == reference.TrialAttemptID && attempt.Ordinal == 1 && attempt.RetryOfTrialAttemptID == "" {
				foundAttempt = true
				break
			}
		}
		if !foundAttempt {
			return nil, fmt.Errorf("%w: CodeEdge evaluator effect trial %d does not match its initial TrialAttempt", ErrFrozenExecutionPayload, reference.Ordinal)
		}
	}
	return append([]codeEdgeEvaluatorTrialReference(nil), payload.Trials...), nil
}

// codeEdgeEvaluatorEffectAlreadyStarted returns the persisted external fence
// without creating trial records. A started effect is never safe to replay.
func (runtime *FrozenExecutionRuntime) codeEdgeEvaluatorEffectAlreadyStarted(ctx context.Context, run store.WorkflowRun, stageAttempt store.StageAttempt, stage workflowkit.StageDescriptor) (*store.SideEffectOperation, error) {
	operation, err := runtime.core.store.GetSideEffectOperationByOperationKey(ctx, codeEdgeEvaluatorOperationKey(run.ID, stageAttempt.ID))
	if err != nil || operation == nil {
		return operation, err
	}
	if _, err := runtime.validateCodeEdgeEvaluatorEffect(ctx, *operation, run, stageAttempt, stage); err != nil {
		return nil, err
	}
	switch operation.State {
	case store.SideEffectStarted, store.SideEffectUnknown, store.SideEffectSucceeded:
		return operation, nil
	default:
		return nil, nil
	}
}

func codeEdgeEvaluatorOutcomeIsUncertain(result StageExecutionResult, reservation stageQuotaReservation, workerLeaseLost <-chan struct{}, monitor *stageControlMonitor) bool {
	if reservation.lost() || channelClosed(workerLeaseLost) || result.Outcome.Status != workflowkit.StatusCompleted {
		return true
	}
	return monitor != nil && monitor.current() != nil
}

// projectCodeEdgeEvaluatorInDoubt centralizes the order of durable evidence
// after a started evaluator invocation becomes unknowable: trials and effect
// facts first, then an idempotent reconciliation record, then stage/run
// projections. Nothing in this path creates a fifth logical sample.
func (runtime *FrozenExecutionRuntime) projectCodeEdgeEvaluatorInDoubt(ctx context.Context, job store.DurableJob, run store.WorkflowRun, stageAttempt store.StageAttempt, stage workflowkit.StageDescriptor, reservation stageQuotaReservation, effect store.SideEffectOperation, control *store.DurableControlOperation, detail string) (store.JobState, error) {
	reservation.stop()
	settlementID, settlementErr := runtime.settleStageQuota(ctx, reservation, store.QuotaSettlementUncertain, job.CreatedBy, "CodeEdge evaluator outcome requires reconciliation")

	var failures []error
	if settlementErr != nil {
		failures = append(failures, settlementErr)
	}
	if err := runtime.markCodeEdgeEvaluatorTrialsInDoubt(ctx, run, stageAttempt, job.CreatedBy, detail); err != nil {
		failures = append(failures, err)
	}
	currentEffect, err := runtime.markCodeEdgeEvaluatorEffectUnknown(ctx, effect, job.CreatedBy, detail)
	if err != nil {
		failures = append(failures, err)
	} else {
		effect = currentEffect
	}
	if err := runtime.startCodeEdgeEvaluatorReconciliation(ctx, run, stageAttempt, stage, effect, job.CreatedBy); err != nil {
		failures = append(failures, err)
	}
	if err := runtime.markCodeEdgeStageInDoubt(ctx, stageAttempt, job.CreatedBy, detail); err != nil {
		failures = append(failures, err)
	}
	if err := runtime.markCodeEdgeRunInDoubt(ctx, run.ID, job.CreatedBy, detail); err != nil {
		failures = append(failures, err)
	}
	if err := runtime.enqueueCodeEdgeEvaluatorReconciliation(ctx, job, run, stageAttempt, stage); err != nil {
		failures = append(failures, err)
	}
	if runtime.core.repairs != nil {
		if err := runtime.enqueueAutomaticRepairOutcome(ctx, run.ID, job.CreatedBy, "CodeEdge evaluator external effect requires reconciliation"); err != nil {
			failures = append(failures, err)
		}
	}
	if control != nil {
		if _, err := runtime.requireControlReconcile(ctx, *control, settlementID, "CodeEdge evaluator outcome may include an unknown external side effect"); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) != 0 {
		return store.JobFailed, fmt.Errorf("CodeEdge evaluator reconciliation projection: %w", errors.Join(failures...))
	}
	return store.JobInterrupted, nil
}

func (runtime *FrozenExecutionRuntime) markCodeEdgeEvaluatorTrialsInDoubt(ctx context.Context, run store.WorkflowRun, stageAttempt store.StageAttempt, actor, detail string) error {
	executions, err := runtime.core.store.ListTrialExecutionsForStageAttempt(ctx, stageAttempt.ID)
	if err != nil {
		return err
	}
	if len(executions) != codeEdgeEvaluatorTrialCount {
		return fmt.Errorf("%w: CodeEdge evaluator stage %s has %d TrialExecutions, want four", ErrFrozenExecutionPayload, stageAttempt.ID, len(executions))
	}
	for _, execution := range executions {
		if err := validateCodeEdgeTrialExecution(execution, run, stageAttempt); err != nil {
			return err
		}
		attempts, listErr := runtime.core.store.ListTrialAttemptsForTrialExecution(ctx, execution.ID)
		if listErr != nil {
			return listErr
		}
		for _, attempt := range attempts {
			switch attempt.Status {
			case store.TrialAttemptQueued:
				started, transitionErr := runtime.core.store.TransitionTrialAttempt(ctx, store.TransitionTrialAttemptRequest{TrialAttemptID: attempt.ID, ExpectedVersion: attempt.Version, Status: store.TrialAttemptRunning, Actor: actor, Reason: "prepare unknown CodeEdge evaluator trial outcome"})
				if transitionErr != nil {
					return transitionErr
				}
				attempt = started
				fallthrough
			case store.TrialAttemptRunning, store.TrialAttemptWaiting:
				if _, transitionErr := runtime.core.store.TransitionTrialAttempt(ctx, store.TransitionTrialAttemptRequest{TrialAttemptID: attempt.ID, ExpectedVersion: attempt.Version, Status: store.TrialAttemptInDoubt, ErrorText: detail, FailureClass: string(workflowkit.FailureUnknown), Actor: actor, Reason: "CodeEdge evaluator external outcome is unknown"}); transitionErr != nil {
					return transitionErr
				}
			}
		}
		current, getErr := runtime.core.store.GetTrialExecution(ctx, execution.ID)
		if getErr != nil {
			return getErr
		}
		if current == nil {
			return fmt.Errorf("%w: CodeEdge TrialExecution %s", ErrLifecycleNotFound, execution.ID)
		}
		switch current.Status {
		case store.TrialExecutionQueued:
			running, transitionErr := runtime.core.store.TransitionTrialExecution(ctx, store.TransitionTrialExecutionRequest{TrialExecutionID: current.ID, ExpectedVersion: current.Version, Status: store.TrialExecutionRunning, Actor: actor, Reason: "prepare unknown CodeEdge evaluator trial outcome"})
			if transitionErr != nil {
				return transitionErr
			}
			current = &running
			fallthrough
		case store.TrialExecutionRunning, store.TrialExecutionWaiting:
			if _, transitionErr := runtime.core.store.TransitionTrialExecution(ctx, store.TransitionTrialExecutionRequest{TrialExecutionID: current.ID, ExpectedVersion: current.Version, Status: store.TrialExecutionInDoubt, Actor: actor, Reason: "CodeEdge evaluator external outcome is unknown"}); transitionErr != nil {
				return transitionErr
			}
		}
	}
	return nil
}

func (runtime *FrozenExecutionRuntime) markCodeEdgeEvaluatorEffectUnknown(ctx context.Context, effect store.SideEffectOperation, actor, detail string) (store.SideEffectOperation, error) {
	switch effect.State {
	case store.SideEffectStarted:
		return runtime.core.store.TransitionSideEffectOperation(ctx, store.TransitionSideEffectOperationRequest{
			OperationID: effect.ID, ExpectedVersion: effect.Version, State: store.SideEffectUnknown,
			Actor: actor, Reason: "CodeEdge evaluator external outcome is unknown: " + detail,
		})
	case store.SideEffectUnknown, store.SideEffectSucceeded:
		return effect, nil
	default:
		return store.SideEffectOperation{}, fmt.Errorf("%w: CodeEdge evaluator side effect %s is %s after invocation fence", ErrFrozenExecutionPayload, effect.ID, effect.State)
	}
}

func (runtime *FrozenExecutionRuntime) startCodeEdgeEvaluatorReconciliation(ctx context.Context, run store.WorkflowRun, stageAttempt store.StageAttempt, stage workflowkit.StageDescriptor, effect store.SideEffectOperation, actor string) error {
	observed, err := json.Marshal(codeEdgeEvaluatorReconciliationObservation{
		Format: codeEdgeEvaluatorReconciliationObservationForm, RunID: run.ID, StageAttemptID: stageAttempt.ID,
		StageKey: stageAttempt.StageKey, SideEffectOperationID: effect.ID,
	})
	if err != nil {
		return err
	}
	_, err = runtime.core.store.StartReconciliationAttempt(ctx, store.StartReconciliationAttemptRequest{
		OperationKey: codeEdgeEvaluatorReconciliationKey(effect.OperationKey), SubjectType: "stage_attempt", SubjectID: stageAttempt.ID,
		SideEffectOperationID: effect.ID, Ordinal: 1, ObservedJSON: string(observed), Actor: actor,
		Reason: "reconcile started CodeEdge evaluator external effect",
	})
	return err
}

func (runtime *FrozenExecutionRuntime) markCodeEdgeStageInDoubt(ctx context.Context, stageAttempt store.StageAttempt, actor, detail string) error {
	current, err := runtime.core.store.GetStageAttempt(ctx, stageAttempt.ID)
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("%w: stage attempt %s", ErrLifecycleNotFound, stageAttempt.ID)
	}
	switch current.ExecutionStatus {
	case store.StageExecutionQueued:
		running, transitionErr := runtime.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{StageAttemptID: current.ID, ExpectedVersion: current.Version, ExecutionStatus: store.StageExecutionRunning, Actor: actor, Reason: "prepare unknown CodeEdge evaluator outcome"})
		if transitionErr != nil {
			return transitionErr
		}
		current = &running
		fallthrough
	case store.StageExecutionRunning, store.StageExecutionWaiting:
		_, err = runtime.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
			StageAttemptID: current.ID, ExpectedVersion: current.Version, ExecutionStatus: store.StageExecutionInDoubt,
			ErrorText: detail, FailureClass: string(workflowkit.FailureUnknown), Actor: actor, Reason: "CodeEdge evaluator external outcome is unknown",
		})
		return err
	case store.StageExecutionInDoubt, store.StageExecutionReconciling:
		return nil
	default:
		return nil
	}
}

func (runtime *FrozenExecutionRuntime) markCodeEdgeRunInDoubt(ctx context.Context, runID, actor, detail string) error {
	run, err := runtime.core.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("%w: workflow run %s", ErrLifecycleNotFound, runID)
	}
	if run.Status == store.WorkflowRunInDoubt || terminalWorkflowRunStatus(run.Status) {
		return nil
	}
	_, err = runtime.core.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunInDoubt,
		Actor: actor, Reason: "CodeEdge evaluator external outcome is unknown: " + detail,
	})
	return err
}

func (runtime *FrozenExecutionRuntime) completeCodeEdgeEvaluatorEffect(ctx context.Context, run store.WorkflowRun, stageAttempt store.StageAttempt, stage workflowkit.StageDescriptor, manifest store.ArtifactManifest, actor string) error {
	effect, err := runtime.core.store.GetSideEffectOperationByOperationKey(ctx, codeEdgeEvaluatorOperationKey(run.ID, stageAttempt.ID))
	if err != nil {
		return err
	}
	if effect == nil {
		return fmt.Errorf("%w: completed CodeEdge evaluator stage has no side effect fence", ErrFrozenExecutionPayload)
	}
	if _, err := runtime.validateCodeEdgeEvaluatorEffect(ctx, *effect, run, stageAttempt, stage); err != nil {
		return err
	}
	switch effect.State {
	case store.SideEffectStarted:
		_, err = runtime.core.store.TransitionSideEffectOperation(ctx, store.TransitionSideEffectOperationRequest{
			OperationID: effect.ID, ExpectedVersion: effect.Version, State: store.SideEffectSucceeded,
			DestinationDigest: manifest.ManifestFingerprint, ReceiptRef: manifest.ID,
			Actor: actor, Reason: "persisted CodeEdge evaluator result artifacts",
		})
		return err
	case store.SideEffectSucceeded:
		if effect.DestinationDigest != manifest.ManifestFingerprint || effect.ReceiptRef != manifest.ID {
			return fmt.Errorf("%w: CodeEdge evaluator side effect success receipt differs from persisted artifacts", ErrFrozenExecutionPayload)
		}
		return nil
	default:
		return fmt.Errorf("%w: completed CodeEdge evaluator stage has side effect state %s", ErrFrozenExecutionPayload, effect.State)
	}
}

// completeTrustedCodeEdgeEvaluatorTrials projects the four logical samples
// only after a controlled evaluator result has crossed the durable evidence
// fence. Both the direct completion and the read-only recovery path use this
// helper, so replay can finish an interrupted projection without allocating a
// fifth sample or a second external invocation.
func (runtime *FrozenExecutionRuntime) completeTrustedCodeEdgeEvaluatorTrials(ctx context.Context, run store.WorkflowRun, stage store.StageAttempt, actor, reason string) error {
	if runtime == nil || runtime.core == nil || runtime.core.store == nil || runtime.services == nil || runtime.services.CodeEdgeCompliance == nil {
		return errors.New("CodeEdge evaluator trusted trial projector is not configured")
	}
	stageDescriptor := workflowkit.StageDescriptor{Key: workflowkit.StageKey(stage.StageKey)}
	if !isCodeEdgeEvaluatorStage(run, stageDescriptor) {
		return fmt.Errorf("%w: trusted trial projection targets non-evaluator stage %q", ErrFrozenExecutionPayload, stage.StageKey)
	}
	effect, err := runtime.core.store.GetSideEffectOperationByOperationKey(ctx, codeEdgeEvaluatorOperationKey(run.ID, stage.ID))
	if err != nil {
		return err
	}
	if effect == nil {
		return fmt.Errorf("%w: trusted trial projection has no evaluator side effect", ErrFrozenExecutionPayload)
	}
	if _, err := runtime.validateCodeEdgeEvaluatorEffect(ctx, *effect, run, stage, stageDescriptor); err != nil {
		return err
	}
	return runtime.services.CodeEdgeCompliance.completeTrustedTrialSet(ctx, run, stage, codeEdgeEvaluatorTrialCount, actor, reason)
}

func (runtime *FrozenExecutionRuntime) reconcileRecoveredCodeEdgeEvaluatorStage(ctx context.Context, job store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, payload frozenStageExecutionPayload, stageAttempt store.StageAttempt, stage workflowkit.StageDescriptor) (bool, error) {
	effect, err := runtime.codeEdgeEvaluatorEffectAlreadyStarted(ctx, run, stageAttempt, stage)
	if err != nil {
		return false, err
	}
	if effect == nil {
		return false, nil
	}
	if effect.State == store.SideEffectSucceeded && stageAttempt.ExecutionStatus == store.StageExecutionCompleted {
		// The side effect and its immutable receipt are already final, but a
		// worker can have died before restoring the Run projection. Resume only
		// that deterministic projection; do not ask the observer to inspect or
		// invoke anything again. A direct completion never owns a reconciliation
		// attempt, so it must not fabricate one while replaying this path.
		_, resumeErr := runtime.resumeCommittedCodeEdgeEvaluatorCompletion(ctx, job, run, frozen, payload, stage, stageAttempt, *effect)
		return true, resumeErr
	}
	if _, err := runtime.projectCodeEdgeEvaluatorInDoubt(ctx, job, run, stageAttempt, stage, stageQuotaReservation{}, *effect, nil, "expired durable stage job observed after CodeEdge evaluator invocation fence"); err != nil {
		return true, err
	}

	// Recovery may only create the separate idempotent observation delivery. It
	// must not inspect provider evidence itself: the recovered source job has no
	// live dispatch lease and must never turn recovery into an implicit rerun.
	return true, nil
}
