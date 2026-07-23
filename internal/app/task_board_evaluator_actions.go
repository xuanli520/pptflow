package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

// PreviewEvaluatorLaunch revalidates the selected parent without persisting a
// lifecycle operation. The subsequent prepare repeats this check before it
// freezes the evaluator inputs.
func (service *TaskBoardService) PreviewEvaluatorLaunch(ctx context.Context, request TaskBoardEvaluatorLaunchPreviewRequest) (TaskBoardEvaluatorLaunchPreview, error) {
	parent, err := service.taskBoardCodeEdgeParent(ctx, request.TaskID, request.ParentRunID)
	if err != nil {
		return TaskBoardEvaluatorLaunchPreview{}, err
	}
	if service.evaluatorLaunches == nil {
		return TaskBoardEvaluatorLaunchPreview{}, fmt.Errorf("CodeEdge evaluator launch service is not configured")
	}
	plan, err := service.evaluatorLaunches.Plan(ctx, parent.ID)
	if err != nil {
		return TaskBoardEvaluatorLaunchPreview{}, err
	}
	return TaskBoardEvaluatorLaunchPreview{
		TaskID:                      plan.TaskID,
		ParentRunID:                 plan.ParentRunID,
		RevisionID:                  plan.RevisionID,
		TemplateID:                  plan.Template.ID,
		TemplateVersion:             plan.Template.Version,
		ExecutionProfileFingerprint: string(plan.ExecutionProfileFingerprint),
		ExecutionSpecFingerprint:    string(plan.ExecutionSpecFingerprint),
	}, nil
}

// PrepareEvaluatorLaunch performs the first explicit evaluator confirmation.
// It persists only the closed input bundle, never a child Run or provider
// effect.
func (service *TaskBoardService) PrepareEvaluatorLaunch(ctx context.Context, request TaskBoardEvaluatorLaunchRequest) (TaskBoardPreparedEvaluatorLaunch, error) {
	parent, actor, checkpoint, _, err := service.taskBoardEvaluatorCheckpoint(ctx, LifecycleMutationCodeEdgeEvaluator, request.IdempotencyKey, request.TaskID, request.ParentRunID, request.Reason)
	if err != nil {
		return TaskBoardPreparedEvaluatorLaunch{}, err
	}
	if service.evaluatorLaunches == nil {
		return TaskBoardPreparedEvaluatorLaunch{}, fmt.Errorf("CodeEdge evaluator launch service is not configured")
	}
	prepared, err := service.evaluatorLaunches.Prepare(ctx, CodeEdgeEvaluatorLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: strings.TrimSpace(request.IdempotencyKey), Actor: actor, Reason: strings.TrimSpace(request.Reason), Expected: checkpoint,
		},
		ParentRunID: parent.ID,
	})
	if err != nil {
		return TaskBoardPreparedEvaluatorLaunch{}, err
	}
	return TaskBoardPreparedEvaluatorLaunch{
		TaskID:                      parent.TaskID,
		ParentRunID:                 prepared.ParentRunID,
		InputBundleID:               prepared.InputBundleID,
		ExecutionProfileFingerprint: prepared.ProfileFingerprint,
		ExecutionSpecFingerprint:    prepared.ExecutionSpecFingerprint,
	}, nil
}

// ConfirmEvaluatorLaunch consumes the durable first confirmation and starts
// exactly one evaluator child through the composition-owned worker launcher.
func (service *TaskBoardService) ConfirmEvaluatorLaunch(ctx context.Context, request TaskBoardEvaluatorLaunchRequest) (TaskBoardMutation, error) {
	parent, actor, checkpoint, _, err := service.taskBoardEvaluatorCheckpoint(ctx, LifecycleMutationCodeEdgeEvaluator, request.IdempotencyKey, request.TaskID, request.ParentRunID, request.Reason)
	if err != nil {
		return TaskBoardMutation{}, err
	}
	if service.evaluatorLaunches == nil || service.workerLauncher == nil {
		return TaskBoardMutation{}, fmt.Errorf("CodeEdge evaluator controlled worker launcher is not configured")
	}
	result, err := service.evaluatorLaunches.ConfirmAndLaunch(ctx, CodeEdgeEvaluatorLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: strings.TrimSpace(request.IdempotencyKey), Actor: actor, Reason: strings.TrimSpace(request.Reason), Expected: checkpoint,
		},
		ParentRunID: parent.ID,
	}, service.workerLauncher)
	if err != nil {
		return TaskBoardMutation{}, err
	}
	return TaskBoardMutation{TaskID: parent.TaskID, RunID: result.Receipt.RunID, OperationID: result.Receipt.OperationID, Summary: "已确认 CodeEdge evaluator child 并启动受控 worker"}, nil
}

// PreviewEvaluatorEvidenceHandoff verifies the completed evaluator child and
// its canonical Qwen/Opus evidence before any adoption state is persisted.
func (service *TaskBoardService) PreviewEvaluatorEvidenceHandoff(ctx context.Context, request TaskBoardEvaluatorEvidenceHandoffPreviewRequest) (TaskBoardEvaluatorEvidenceHandoffPreview, error) {
	parent, err := service.taskBoardCodeEdgeParent(ctx, request.TaskID, request.ParentRunID)
	if err != nil {
		return TaskBoardEvaluatorEvidenceHandoffPreview{}, err
	}
	if _, err := service.taskBoardCodeEdgeEvaluatorChild(ctx, parent, request.ChildRunID); err != nil {
		return TaskBoardEvaluatorEvidenceHandoffPreview{}, err
	}
	if service.evaluatorEvidence == nil {
		return TaskBoardEvaluatorEvidenceHandoffPreview{}, fmt.Errorf("CodeEdge evaluator evidence handoff service is not configured")
	}
	plan, err := service.evaluatorEvidence.Plan(ctx, parent.ID, strings.TrimSpace(request.ChildRunID))
	if err != nil {
		return TaskBoardEvaluatorEvidenceHandoffPreview{}, err
	}
	return TaskBoardEvaluatorEvidenceHandoffPreview{
		TaskID:               plan.TaskID,
		ParentRunID:          plan.ParentRunID,
		ChildRunID:           plan.ChildRunID,
		RevisionID:           plan.RevisionID,
		HandoffFingerprint:   string(plan.HandoffFingerprint),
		QwenTrialFingerprint: string(plan.QwenTrialFingerprint),
		OpusTrialFingerprint: string(plan.OpusTrialFingerprint),
	}, nil
}

// PrepareEvaluatorEvidenceHandoff persists the first adoption confirmation
// after rebuilding the closed parent/child evidence graph.
func (service *TaskBoardService) PrepareEvaluatorEvidenceHandoff(ctx context.Context, request TaskBoardEvaluatorEvidenceHandoffRequest) (TaskBoardPreparedEvaluatorEvidenceHandoff, error) {
	parent, actor, checkpoint, _, err := service.taskBoardEvaluatorCheckpoint(ctx, LifecycleMutationCodeEdgeEvaluatorEvidenceHandoff, request.IdempotencyKey, request.TaskID, request.ParentRunID, request.Reason)
	if err != nil {
		return TaskBoardPreparedEvaluatorEvidenceHandoff{}, err
	}
	if _, err := service.taskBoardCodeEdgeEvaluatorChild(ctx, parent, request.ChildRunID); err != nil {
		return TaskBoardPreparedEvaluatorEvidenceHandoff{}, err
	}
	if service.mutations == nil {
		return TaskBoardPreparedEvaluatorEvidenceHandoff{}, fmt.Errorf("CodeEdge evaluator evidence handoff lifecycle service is not configured")
	}
	prepared, err := service.mutations.PrepareCodeEdgeEvaluatorEvidenceHandoff(ctx, CodeEdgeEvaluatorEvidenceHandoffCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: strings.TrimSpace(request.IdempotencyKey), Actor: actor, Reason: strings.TrimSpace(request.Reason), Expected: checkpoint,
		},
		ParentRunID: parent.ID,
		ChildRunID:  strings.TrimSpace(request.ChildRunID),
	})
	if err != nil {
		return TaskBoardPreparedEvaluatorEvidenceHandoff{}, err
	}
	return TaskBoardPreparedEvaluatorEvidenceHandoff{
		TaskID:               parent.TaskID,
		OperationID:          prepared.OperationID,
		ParentRunID:          prepared.ParentRunID,
		ChildRunID:           prepared.ChildRunID,
		HandoffFingerprint:   string(prepared.HandoffFingerprint),
		QwenTrialFingerprint: string(prepared.QwenTrialFingerprint),
		OpusTrialFingerprint: string(prepared.OpusTrialFingerprint),
	}, nil
}

// AdoptEvaluatorEvidenceHandoff consumes the prepared adoption and publishes
// only the verified, immutable parent/child evidence bridge.
func (service *TaskBoardService) AdoptEvaluatorEvidenceHandoff(ctx context.Context, request TaskBoardEvaluatorEvidenceHandoffRequest) (TaskBoardMutation, error) {
	parent, actor, checkpoint, prepared, err := service.taskBoardEvaluatorCheckpoint(ctx, LifecycleMutationCodeEdgeEvaluatorEvidenceHandoff, request.IdempotencyKey, request.TaskID, request.ParentRunID, request.Reason)
	if err != nil {
		return TaskBoardMutation{}, err
	}
	if !prepared {
		return TaskBoardMutation{}, fmt.Errorf("CodeEdge evaluator evidence handoff must be prepared with the same idempotency key before adoption")
	}
	if _, err := service.taskBoardCodeEdgeEvaluatorChild(ctx, parent, request.ChildRunID); err != nil {
		return TaskBoardMutation{}, err
	}
	if service.mutations == nil {
		return TaskBoardMutation{}, fmt.Errorf("CodeEdge evaluator evidence handoff lifecycle service is not configured")
	}
	receipt, err := service.mutations.AdoptCodeEdgeEvaluatorEvidenceHandoff(ctx, CodeEdgeEvaluatorEvidenceHandoffCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: strings.TrimSpace(request.IdempotencyKey), Actor: actor, Reason: strings.TrimSpace(request.Reason), Expected: checkpoint,
		},
		ParentRunID: parent.ID,
		ChildRunID:  strings.TrimSpace(request.ChildRunID),
	})
	if err != nil {
		return TaskBoardMutation{}, err
	}
	if err := service.FlushQueuedRuns(ctx); err != nil {
		return TaskBoardMutation{}, err
	}
	return TaskBoardMutation{TaskID: parent.TaskID, RunID: parent.ID, OperationID: receipt.OperationID, Summary: "已采用经验证的 CodeEdge evaluator 证据"}, nil
}

func (service *TaskBoardService) taskBoardCodeEdgeParent(ctx context.Context, taskID, parentRunID string) (store.WorkflowRun, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return store.WorkflowRun{}, fmt.Errorf("task board service is not configured")
	}
	taskID = strings.TrimSpace(taskID)
	parentRunID = strings.TrimSpace(parentRunID)
	if err := store.ValidateUUIDv7(taskID); err != nil {
		return store.WorkflowRun{}, fmt.Errorf("task board task ID: %w", err)
	}
	if err := store.ValidateUUIDv7(parentRunID); err != nil {
		return store.WorkflowRun{}, fmt.Errorf("CodeEdge evaluator parent Run ID: %w", err)
	}
	parent, err := service.core.store.GetWorkflowRun(ctx, parentRunID)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	if parent == nil || parent.TaskID != taskID || !isCodeEdgePhase1Run(*parent) {
		return store.WorkflowRun{}, fmt.Errorf("CodeEdge evaluator parent Run does not belong to the selected task")
	}
	return *parent, nil
}

func (service *TaskBoardService) taskBoardCodeEdgeEvaluatorChild(ctx context.Context, parent store.WorkflowRun, childRunID string) (store.WorkflowRun, error) {
	childRunID = strings.TrimSpace(childRunID)
	if err := store.ValidateUUIDv7(childRunID); err != nil {
		return store.WorkflowRun{}, fmt.Errorf("CodeEdge evaluator child Run ID: %w", err)
	}
	child, err := service.core.store.GetWorkflowRun(ctx, childRunID)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	if child == nil || child.TaskID != parent.TaskID || child.ParentRunID != parent.ID ||
		child.WorkflowTemplateID != workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID ||
		child.WorkflowTemplateVersion != workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion {
		return store.WorkflowRun{}, fmt.Errorf("CodeEdge evaluator child Run does not belong to the selected parent")
	}
	return *child, nil
}

// taskBoardEvaluatorCheckpoint restores the original checkpoint after a
// lifecycle operation exists. An evaluator launch's first confirmation reads
// its separately frozen input bundle and deliberately has no lifecycle
// operation yet; its service enforces the prepare-before-confirm contract.
// Evidence adoption, in contrast, creates its prepared lifecycle operation
// at the first confirmation and uses the returned bool as that prerequisite.
func (service *TaskBoardService) taskBoardEvaluatorCheckpoint(ctx context.Context, action LifecycleMutationAction, idempotencyKey, taskID, parentRunID, reason string) (store.WorkflowRun, string, LifecycleMutationCheckpoint, bool, error) {
	parent, err := service.taskBoardCodeEdgeParent(ctx, taskID, parentRunID)
	if err != nil {
		return store.WorkflowRun{}, "", LifecycleMutationCheckpoint{}, false, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if err := store.ValidateUUIDv7(idempotencyKey); err != nil {
		return store.WorkflowRun{}, "", LifecycleMutationCheckpoint{}, false, fmt.Errorf("task board evaluator idempotency key: %w", err)
	}
	if strings.TrimSpace(reason) == "" {
		return store.WorkflowRun{}, "", LifecycleMutationCheckpoint{}, false, fmt.Errorf("task board evaluator reason is required")
	}
	actor, err := service.currentActor()
	if err != nil {
		return store.WorkflowRun{}, "", LifecycleMutationCheckpoint{}, false, err
	}
	if service.mutations == nil {
		return store.WorkflowRun{}, "", LifecycleMutationCheckpoint{}, false, fmt.Errorf("task board lifecycle mutation service is not configured")
	}
	operation, err := service.core.store.GetLifecycleOperationByIdempotencyKey(ctx, idempotencyKey)
	if err != nil {
		return store.WorkflowRun{}, "", LifecycleMutationCheckpoint{}, false, err
	}
	if operation == nil {
		checkpoint, captureErr := service.mutations.CaptureCheckpoint(ctx, parent.TaskID, parent.RevisionID, parent.ID, "")
		if captureErr != nil {
			return store.WorkflowRun{}, "", LifecycleMutationCheckpoint{}, false, captureErr
		}
		return parent, actor, checkpoint, false, nil
	}
	if operation.Action != string(action) {
		return store.WorkflowRun{}, "", LifecycleMutationCheckpoint{}, false, fmt.Errorf("%w: lifecycle operation key %s", store.ErrIdempotencyConflict, idempotencyKey)
	}
	if operation.State == store.LifecycleOperationPrepared && strings.TrimSpace(operation.ExpectedTaskID) == "" {
		return store.WorkflowRun{}, "", LifecycleMutationCheckpoint{}, false, fmt.Errorf("%w: prepared lifecycle operation %s has no persisted expected checkpoint identities", store.ErrIdempotencyConflict, operation.ID)
	}
	checkpoint := LifecycleMutationCheckpoint{
		TaskID:                           operation.ExpectedTaskID,
		TaskVersion:                      operation.ExpectedTaskVersion,
		RevisionID:                       operation.ExpectedRevisionID,
		RevisionStateVersion:             operation.ExpectedRevisionStateVersion,
		RevisionDigest:                   operation.ExpectedRevisionDigest,
		RunID:                            operation.ExpectedRunID,
		RunVersion:                       operation.ExpectedRunVersion,
		RunExecutionEpoch:                operation.ExpectedRunExecutionEpoch,
		RunDefinitionHash:                operation.ExpectedRunDefinitionHash,
		CodeEdgeComplianceRecordID:       operation.ExpectedCodeEdgeComplianceRecordID,
		CodeEdgeAuthorizationFingerprint: operation.ExpectedCodeEdgeAuthorizationFingerprint,
		ReleaseID:                        operation.ExpectedReleaseID,
		ReleaseRecordVersion:             operation.ExpectedReleaseRecordVersion,
		ReviewRequestID:                  operation.ExpectedReviewRequestID,
		ReviewRevisionID:                 operation.ExpectedReviewRevisionID,
		ReviewState:                      operation.ExpectedReviewState,
		ReviewEvidenceDigest:             operation.ExpectedReviewEvidenceDigest,
	}
	if checkpoint.RunID != parent.ID || checkpoint.TaskID != parent.TaskID || checkpoint.RevisionID != parent.RevisionID {
		return store.WorkflowRun{}, "", LifecycleMutationCheckpoint{}, false, fmt.Errorf("%w: persisted evaluator checkpoint does not match the selected parent", store.ErrIdempotencyConflict)
	}
	return parent, actor, checkpoint, true, nil
}

func (service *TaskBoardService) projectTaskBoardEvaluator(ctx context.Context, detail TaskInspectionSnapshot) *TaskBoardEvaluatorStatus {
	parents := make([]store.WorkflowRun, 0, 1)
	children := make([]store.WorkflowRun, 0, 1)
	for _, inspected := range detail.Runs {
		if isCodeEdgePhase1Run(inspected.Run) {
			parents = append(parents, inspected.Run)
		}
	}
	if len(parents) != 1 {
		return nil
	}
	parent := parents[0]
	status := &TaskBoardEvaluatorStatus{ParentRunID: parent.ID, State: TaskBoardEvaluatorUnavailable}
	for _, inspected := range detail.Runs {
		run := inspected.Run
		if run.ParentRunID == parent.ID && run.WorkflowTemplateID == workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID && run.WorkflowTemplateVersion == workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion {
			children = append(children, run)
		}
	}
	if len(children) > 1 {
		status.Reason = "存在多个 evaluator child Run，需使用明确的运行控制界面"
		return status
	}
	if len(children) == 1 {
		child := children[0]
		status.ChildRunID = child.ID
		if handoff, err := service.core.store.GetCodeEdgeEvaluatorEvidenceHandoffForParentRun(ctx, parent.ID); err != nil {
			// The board is an operator-facing projection. Store, verifier, or
			// provider diagnostics can contain internal material, so they belong
			// in durable records rather than this short status string.
			status.Reason = "评测证据状态暂时不可读取"
			return status
		} else if handoff != nil {
			status.State = TaskBoardEvaluatorAdopted
			return status
		}
		if child.Status != store.WorkflowRunSucceeded {
			status.State = TaskBoardEvaluatorChildActive
			status.Reason = string(child.Status)
			return status
		}
		if service.evaluatorEvidence == nil {
			status.Reason = "evaluator evidence handoff 服务未配置"
			return status
		}
		if _, err := service.evaluatorEvidence.Plan(ctx, parent.ID, child.ID); err != nil {
			status.Reason = "完整且经过验证的 Qwen/Opus 评测证据尚不可采用"
			return status
		}
		status.State = TaskBoardEvaluatorReadyToAdopt
		status.CanAdopt = true
		return status
	}
	if service.evaluatorLaunches == nil || !service.evaluatorLaunches.Available() {
		status.Reason = "CodeEdge evaluator launch 服务未配置"
		return status
	}
	if _, err := service.evaluatorLaunches.Plan(ctx, parent.ID); err != nil {
		status.State = TaskBoardEvaluatorAwaitingFinalReview
		status.Reason = "需要已批准的 Phase-1 最终审核和有效的评测部署定义"
		return status
	}
	status.State = TaskBoardEvaluatorReadyToLaunch
	status.CanLaunch = true
	return status
}
