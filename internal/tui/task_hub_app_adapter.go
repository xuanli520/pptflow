package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// AppTaskHubLifecycleAdapter projects the V2 application services into the
// read/plan-only boundary consumed by the Task Hub. It deliberately has no
// mutation path: confirmations are owned by the application service that
// eventually executes a planned command.
type AppTaskHubLifecycleAdapter struct {
	services *app.LifecycleServices
}

var _ TaskHubLifecycleService = (*AppTaskHubLifecycleAdapter)(nil)
var _ TaskHubRunControlPlanner = (*AppTaskHubLifecycleAdapter)(nil)
var _ TaskHubMutationPlanner = (*AppTaskHubLifecycleAdapter)(nil)
var _ TaskHubMutationExecutor = (*AppTaskHubLifecycleAdapter)(nil)
var _ TaskHubRunControlMutationExecutor = (*AppTaskHubLifecycleAdapter)(nil)

// NewAppTaskHubLifecycleAdapter creates the real Task Hub adapter for a V2
// application service bundle. A nil bundle is retained so callers receive a
// useful error from QueryTaskHub or PlanTaskHubCommand rather than a panic.
func NewAppTaskHubLifecycleAdapter(services *app.LifecycleServices) *AppTaskHubLifecycleAdapter {
	return &AppTaskHubLifecycleAdapter{services: services}
}

// QueryTaskHub reads V2 lifecycle records through public application services.
// Deleted tasks remain visible because restoring them is a Task Hub workflow.
func (adapter *AppTaskHubLifecycleAdapter) QueryTaskHub(ctx context.Context, query TaskHubQuery) (TaskHubSnapshot, error) {
	services, err := adapter.lifecycleServices()
	if err != nil {
		return TaskHubSnapshot{}, err
	}
	if query.Tab == "" {
		query.Tab = TaskHubTasksTab
	}
	if !query.Tab.valid() {
		return TaskHubSnapshot{}, fmt.Errorf("invalid Task Hub tab %q", query.Tab)
	}

	observedAt := time.Now().UTC()
	tasks, err := services.Tasks.List(ctx, true)
	if err != nil {
		return TaskHubSnapshot{}, fmt.Errorf("list Task Hub tasks: %w", err)
	}

	snapshot := TaskHubSnapshot{
		GlobalActions: appTaskHubGlobalActions(),
		ObservedAt:    observedAt,
	}
	queued := make([]taskHubQueuedRun, 0)
	for _, task := range tasks {
		projection, runs, taskQueued, err := adapter.projectTask(ctx, services, task)
		if err != nil {
			return TaskHubSnapshot{}, err
		}
		snapshot.Tasks = append(snapshot.Tasks, projection)
		snapshot.Runs = append(snapshot.Runs, runs...)
		queued = append(queued, taskQueued...)
	}
	assignTaskHubQueuePositions(snapshot.Runs, queued)
	snapshot.Queue = buildTaskHubQueue(snapshot.Runs, observedAt)
	return filterTaskHubSnapshot(snapshot, query.Filter), nil
}

// PlanTaskHubCommand validates the current authoritative entities and returns
// a preview only. It never calls a mutation method, creates a package, or
// reaches a provider or external destination.
func (adapter *AppTaskHubLifecycleAdapter) PlanTaskHubCommand(ctx context.Context, command TaskHubCommand) (TaskHubPlanPreview, error) {
	services, err := adapter.lifecycleServices()
	if err != nil {
		return TaskHubPlanPreview{}, err
	}
	if !taskHubActionKnown(command.Action) {
		return TaskHubPlanPreview{}, fmt.Errorf("unknown Task Hub action %q", command.Action)
	}

	switch command.Action {
	case TaskHubActionNewTask:
		return unavailableTaskHubPlan(command.Action, "新建 Task 需要标题、来源和审计原因；当前命令未携带这些输入"), nil
	case TaskHubActionImportTask:
		return unavailableTaskHubPlan(command.Action, "导入需要一个受管快照来源；当前命令未携带该输入"), nil
	case TaskHubActionGenerateTask:
		return unavailableTaskHubPlan(command.Action, "从仓库出题需要仓库和提交信息；当前命令未携带这些输入"), nil
	}

	target, err := adapter.resolveTaskHubTarget(ctx, services, command.Target)
	if err != nil {
		return TaskHubPlanPreview{}, err
	}

	switch command.Action {
	case TaskHubActionEditTask:
		return unavailableTaskHubPlan(command.Action, "创建修改需要候选快照和变更说明；当前命令未携带这些输入"), nil
	case TaskHubActionForkTask:
		return unavailableTaskHubPlan(command.Action, "Fork 需要新 Task 的名称和标识；当前命令未携带这些输入"), nil
	case TaskHubActionArchiveTask:
		return unavailableTaskHubPlan(command.Action, "归档尚未提供带 UUIDv7 幂等键的 application command 契约"), nil
	case TaskHubActionSoftDeleteTask:
		return unavailableTaskHubPlan(command.Action, "软删除尚未提供带 UUIDv7 幂等键的 application command 契约"), nil
	case TaskHubActionRestoreTask:
		return unavailableTaskHubPlan(command.Action, "恢复需要明确的目标 lifecycle 状态；当前命令未携带该输入"), nil
	case TaskHubActionStartRun:
		return unavailableTaskHubPlan(command.Action, "启动 Run 需要完整且显式的执行 profile；当前命令未携带该输入"), nil
	case TaskHubActionContinue:
		if target.run == nil || services.Continuations == nil {
			return unavailableTaskHubPlan(command.Action, "继续处理需要明确的 Run 和 TaskContinuationService"), nil
		}
		return TaskHubPlanPreview{
			Title:              "继续处理",
			Summary:            "填写操作原因后将冻结不可变 continuation plan；冻结后会再次显示精确执行、失效和复用范围供确认。",
			Reason:             "Run 当前处于 " + string(target.run.Status) + "。",
			RevisionImpact:     "无内容变更时保持当前 TaskRevision；内容变更必须由候选变更事务创建新 revision。",
			ExecutionScope:     []string{target.task.ID, target.run.ID, target.run.RevisionID},
			ConfirmationNeeded: true,
		}, nil
	case TaskHubActionAttachRun:
		if target.run == nil {
			return unavailableTaskHubPlan(command.Action, "附着需要明确的 Run"), nil
		}
		job, lease, err := adapter.taskHubAttachLease(ctx, services, target.run.ID)
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		if job == nil || lease == nil {
			return unavailableTaskHubPlan(command.Action, "当前 Run 没有由有效 durable lease 持有的运行中 job"), nil
		}
		return TaskHubPlanPreview{
			Title:              "Attach durable Run（只读）",
			Summary:            "已验证运行中 durable job 与有效 lease；当前交互只附着只读状态，不创建 attempt、控制操作或 worker。",
			Reason:             fmt.Sprintf("job %s 由 %s 持有，lease 在 %s 到期。", job.ID, lease.Owner, lease.ExpiresAt.UTC().Format(time.RFC3339)),
			ExecutionScope:     []string{target.task.ID, target.run.ID, job.ID, lease.ID},
			ConfirmationNeeded: false,
		}, nil
	case TaskHubActionOpenRunControl:
		if target.run == nil {
			return unavailableTaskHubPlan(command.Action, "运行控制需要明确的 Run"), nil
		}
		return TaskHubPlanPreview{
			Title:              "打开运行控制",
			Summary:            "将打开只读运行控制视图，不会暂停、终止或取消 Run。",
			Reason:             "Run 当前处于 " + string(target.run.Status) + "。",
			ExecutionScope:     []string{target.run.ID},
			ConfirmationNeeded: false,
		}, nil
	case TaskHubActionApproveReview, TaskHubActionRequestChanges, TaskHubActionRejectReview:
		review, unavailable, err := adapter.openTaskHubReview(ctx, services, target, false)
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		if unavailable != "" {
			return unavailableTaskHubPlan(command.Action, unavailable), nil
		}
		return TaskHubPlanPreview{
			Title:              taskHubActionLabel(command.Action),
			Summary:            "确认后将记录不可变审核决定并关闭该 ReviewRequest。",
			Reason:             "审核请求 " + review.ID + " 当前处于 open 状态。",
			RevisionImpact:     "审核决定绑定当前 revision digest；不会原地修改 TaskRevision 内容。",
			ExecutionScope:     []string{target.task.ID, review.RevisionID, review.ID},
			ConfirmationNeeded: true,
		}, nil
	case TaskHubActionPackageRevision:
		revision, unavailable, err := target.currentRevision(ctx, services)
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		if unavailable != "" {
			return unavailableTaskHubPlan(command.Action, unavailable), nil
		}
		if revision.State != store.RevisionStateValidated && revision.State != store.RevisionStateReleased {
			return unavailableTaskHubPlan(command.Action, fmt.Sprintf("revision 当前处于 %s；必须先完成验证", revision.State)), nil
		}
		if strings.TrimSpace(revision.ValidationEvidenceManifest) == "" {
			return unavailableTaskHubPlan(command.Action, "revision 缺少验证证据清单，不能创建本地 package"), nil
		}
		return TaskHubPlanPreview{
			Title:              "生成本地 package",
			Summary:            "确认后只会在 Harbor Flow 受管目录生成不可变本地 package；不会上传、复制到外部目的地或调用 provider。",
			Reason:             "当前 revision 已验证，并绑定了验证证据。",
			RevisionImpact:     "revision 内容和 digest 不变；仅记录本地 package receipt。",
			ExecutionScope:     []string{target.task.ID, revision.ID},
			ConfirmationNeeded: true,
		}, nil
	case TaskHubActionWithdrawRelease:
		return unavailableTaskHubPlan(command.Action, "撤回需要明确的 release ID；当前 Task Hub 命令未携带该目标"), nil
	default:
		return TaskHubPlanPreview{}, fmt.Errorf("unsupported Task Hub action %q", command.Action)
	}
}

// PlanTaskHubRunControl reads the currently authoritative Run and returns a
// non-mutating impact preview. The Task Hub deliberately does not collect the
// actor, reason, idempotency key, confirmation, or provider capability needed
// to create a durable control command, so this method can never submit one.
func (adapter *AppTaskHubLifecycleAdapter) PlanTaskHubRunControl(ctx context.Context, command TaskHubRunControlCommand) (TaskHubPlanPreview, error) {
	services, err := adapter.lifecycleServices()
	if err != nil {
		return TaskHubPlanPreview{}, err
	}
	if !taskHubRunControlActionKnown(command.Action) {
		return TaskHubPlanPreview{}, fmt.Errorf("unknown Task Hub run control action %q", command.Action)
	}
	target, err := adapter.resolveTaskHubTarget(ctx, services, command.Target)
	if err != nil {
		return TaskHubPlanPreview{}, err
	}
	if target.run == nil {
		return unavailableTaskHubPlan(TaskHubActionOpenRunControl, "运行控制需要明确的 Run"), nil
	}
	if services.Control == nil {
		return unavailableRunControlPlan(command.Action, "运行控制服务不可用"), nil
	}

	var stage *store.StageAttempt
	stageAttemptID := strings.TrimSpace(command.Target.StageAttemptID)
	if stageAttemptID != "" {
		attempt, err := services.Runs.GetStageAttempt(ctx, stageAttemptID)
		if err != nil {
			return TaskHubPlanPreview{}, fmt.Errorf("get stage attempt %s for Run control preview: %w", stageAttemptID, err)
		}
		if attempt.RunID != target.run.ID {
			return TaskHubPlanPreview{}, fmt.Errorf("Task Hub target stage attempt %s does not belong to Run %s", attempt.ID, target.run.ID)
		}
		stage = &attempt
	}
	if reason := taskHubRunControlTargetReason(command.Action, *target.run, stage); reason != "" {
		return unavailableRunControlPlan(command.Action, reason), nil
	}
	checkpoint, err := services.Control.CurrentCheckpoint(ctx, target.run.ID)
	if err != nil {
		return TaskHubPlanPreview{}, fmt.Errorf("read current control checkpoint for Run %s: %w", target.run.ID, err)
	}
	gracePeriod, err := services.Control.FrozenGracePeriod(ctx, target.run.ID)
	if err != nil {
		return TaskHubPlanPreview{}, fmt.Errorf("read frozen control grace period for Run %s: %w", target.run.ID, err)
	}
	scope := []string{target.task.ID, target.run.ID}
	if stage != nil {
		scope = append(scope, stage.ID)
	}
	return TaskHubPlanPreview{
		Title:              taskHubRunControlActionLabel(command.Action) + "影响预览",
		Summary:            "确认表单会收集本机操作员、操作原因和幂等键；确认后才创建 durable ControlOperation。grace period 已冻结，不能由本次操作覆盖。",
		Reason:             fmt.Sprintf("Run 当前处于 %s；控制 checkpoint 为序列 %d / epoch %d。", target.run.Status, checkpoint.Sequence, checkpoint.ExecutionEpoch),
		RevisionImpact:     "本次预览不会修改 TaskRevision、Run 或 StageAttempt。",
		ExecutionScope:     scope,
		BudgetImpact:       "冻结 grace period：" + gracePeriod.String(),
		ExternalEffects:    taskHubRunControlPotentialEffects(command.Action),
		ConfirmationNeeded: true,
	}, nil
}

// PrepareTaskHubMutation freezes actions that require a durable plan before
// execution. At present only continuation uses this extra phase; ordinary
// lifecycle actions remain read-only until the final confirmation submit.
func (adapter *AppTaskHubLifecycleAdapter) PrepareTaskHubMutation(ctx context.Context, request TaskHubMutationRequest) (TaskHubPreparedMutation, error) {
	services, err := adapter.lifecycleServices()
	if err != nil {
		return TaskHubPreparedMutation{}, err
	}
	if err := validateTaskHubMutationRequest(request); err != nil {
		return TaskHubPreparedMutation{}, err
	}
	if request.Action != TaskHubActionContinue {
		return TaskHubPreparedMutation{}, fmt.Errorf("Task Hub action %q does not have a separate planning phase", request.Action)
	}
	if services.Continuations == nil {
		return TaskHubPreparedMutation{}, fmt.Errorf("TaskContinuationService is not configured")
	}
	if strings.TrimSpace(request.PlanID) != "" {
		plan, err := services.Continuations.GetTaskContinuationPlan(ctx, request.PlanID)
		if err != nil {
			return TaskHubPreparedMutation{}, err
		}
		return adapter.preparedTaskHubContinuation(ctx, services, plan)
	}
	target, err := adapter.resolveTaskHubTarget(ctx, services, request.Target)
	if err != nil {
		return TaskHubPreparedMutation{}, err
	}
	if target.run == nil {
		return TaskHubPreparedMutation{}, fmt.Errorf("继续处理需要明确的 Run")
	}
	checkpoint, err := services.Continuations.CurrentCheckpoint(ctx, target.run.ID)
	if err != nil {
		return TaskHubPreparedMutation{}, err
	}
	plan, err := services.Continuations.PlanTaskContinuation(ctx, app.ContinueTaskCommand{
		CommandKey: request.IdempotencyKey,
		TaskID:     target.task.ID,
		RunID:      target.run.ID,
		Expected:   checkpoint,
		Actor:      request.Actor,
		Reason:     request.Reason,
	})
	if err != nil {
		return TaskHubPreparedMutation{}, err
	}
	return adapter.preparedTaskHubContinuation(ctx, services, plan)
}

func (adapter *AppTaskHubLifecycleAdapter) preparedTaskHubContinuation(ctx context.Context, services *app.LifecycleServices, plan workflowkit.ContinuationPlan) (TaskHubPreparedMutation, error) {
	if services == nil || services.Store() == nil {
		return TaskHubPreparedMutation{}, fmt.Errorf("continuation command store is unavailable")
	}
	snapshot := plan.Snapshot()
	command, err := services.Store().GetContinuationCommand(ctx, snapshot.CommandID)
	if err != nil {
		return TaskHubPreparedMutation{}, err
	}
	if command == nil || command.SubjectID != snapshot.BaseCheckpoint.SubjectID || command.RunID != snapshot.SourceRunID ||
		strings.TrimSpace(command.Actor) == "" || strings.TrimSpace(command.Reason) == "" {
		return TaskHubPreparedMutation{}, fmt.Errorf("frozen continuation plan has no valid immutable command provenance")
	}
	return TaskHubPreparedMutation{
		Preview: taskHubContinuationPlanPreview(plan),
		Actor:   command.Actor,
		Reason:  command.Reason,
	}, nil
}

// ExecuteTaskHubMutation delegates confirmed lifecycle actions to public
// application services. It never writes through Store() and never opens an
// execution worker, provider, or workspace from the TUI package.
func (adapter *AppTaskHubLifecycleAdapter) ExecuteTaskHubMutation(ctx context.Context, request TaskHubMutationRequest) (TaskHubMutationResult, error) {
	services, err := adapter.lifecycleServices()
	if err != nil {
		return TaskHubMutationResult{}, err
	}
	if err := validateTaskHubMutationRequest(request); err != nil {
		return TaskHubMutationResult{}, err
	}
	switch request.Action {
	case TaskHubActionContinue:
		if services.Continuations == nil {
			return TaskHubMutationResult{}, fmt.Errorf("TaskContinuationService is not configured")
		}
		if strings.TrimSpace(request.PlanID) == "" {
			return TaskHubMutationResult{}, fmt.Errorf("继续处理必须先冻结计划")
		}
		plan, err := services.Continuations.GetTaskContinuationPlan(ctx, request.PlanID)
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		snapshot := plan.Snapshot()
		if request.Target.TaskID != "" && request.Target.TaskID != snapshot.BaseCheckpoint.SubjectID {
			return TaskHubMutationResult{}, fmt.Errorf("冻结计划不属于当前 Task")
		}
		if request.Target.RunID != "" && request.Target.RunID != snapshot.SourceRunID {
			return TaskHubMutationResult{}, fmt.Errorf("冻结计划不属于当前 Run")
		}
		execution, err := services.Continuations.ExecuteTaskContinuation(ctx, request.PlanID)
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		return TaskHubMutationResult{
			Action:      request.Action,
			Target:      request.Target,
			PlanID:      request.PlanID,
			ExecutionID: execution.ID,
			Summary:     "续跑计划已提交到 durable worker 队列",
		}, nil
	case TaskHubActionApproveReview, TaskHubActionRequestChanges, TaskHubActionRejectReview:
		if services.Reviews == nil {
			return TaskHubMutationResult{}, fmt.Errorf("review service is not configured")
		}
		target, err := adapter.resolveTaskHubTarget(ctx, services, request.Target)
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		review, unavailable, err := adapter.openTaskHubReview(ctx, services, target, true)
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		if unavailable != "" {
			return TaskHubMutationResult{}, fmt.Errorf("%s", unavailable)
		}
		revision, err := services.Revisions.Get(ctx, review.RevisionID)
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		decision, err := services.Reviews.Decide(ctx, app.DecideReviewRequest{
			ID:                     request.IdempotencyKey,
			ReviewRequestID:        review.ID,
			RevisionID:             review.RevisionID,
			Action:                 taskHubReviewDecisionAction(request.Action),
			ExpectedRevisionDigest: revision.TaskDigest,
			Actor:                  request.Actor,
			Reason:                 request.Reason,
		})
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		return TaskHubMutationResult{
			Action:    request.Action,
			Target:    request.Target,
			ReceiptID: decision.ID,
			Summary:   taskHubActionLabel(request.Action) + "已记录",
		}, nil
	case TaskHubActionPackageRevision:
		if services.Releases == nil {
			return TaskHubMutationResult{}, fmt.Errorf("release service is not configured")
		}
		target, err := adapter.resolveTaskHubTarget(ctx, services, request.Target)
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		revision, unavailable, err := target.currentRevision(ctx, services)
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		if unavailable != "" {
			return TaskHubMutationResult{}, fmt.Errorf("%s", unavailable)
		}
		version := strings.TrimSpace(request.Values[taskHubPackageVersionField])
		if version == "" {
			return TaskHubMutationResult{}, fmt.Errorf("本地 package 版本是必填项")
		}
		result, err := services.Releases.PackageRevision(ctx, app.PackageRevisionRequest{
			RevisionID:           revision.ID,
			ExpectedStateVersion: revision.StateVersion,
			ReleaseVersion:       version,
			IdempotencyKey:       request.IdempotencyKey,
			Actor:                request.Actor,
			Reason:               request.Reason,
		})
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		return TaskHubMutationResult{
			Action:    request.Action,
			Target:    request.Target,
			ReceiptID: result.Release.ID,
			Summary:   "本地 package 已生成并记录；未上传或调用远端 provider",
		}, nil
	default:
		return TaskHubMutationResult{}, fmt.Errorf("Task Hub action %q has no confirmed idempotent execution contract", request.Action)
	}
}

// ExecuteTaskHubRunControlMutation creates a target-scoped durable control
// operation using exactly the checkpoint the operator inspected. The service
// reads grace policy only from the frozen run manifest; this adapter never
// accepts a per-request override or recomputes a newer checkpoint.
func (adapter *AppTaskHubLifecycleAdapter) ExecuteTaskHubRunControlMutation(ctx context.Context, request TaskHubRunControlMutationRequest) (TaskHubRunControlMutationResult, error) {
	if err := validateTaskHubRunControlMutationRequest(request); err != nil {
		return TaskHubRunControlMutationResult{}, err
	}
	services, err := adapter.lifecycleServices()
	if err != nil {
		return TaskHubRunControlMutationResult{}, err
	}
	if services.Control == nil {
		return TaskHubRunControlMutationResult{}, fmt.Errorf("execution control service is not configured")
	}
	target, err := adapter.resolveTaskHubTarget(ctx, services, request.Target)
	if err != nil {
		return TaskHubRunControlMutationResult{}, err
	}
	if target.run == nil {
		return TaskHubRunControlMutationResult{}, fmt.Errorf("运行控制需要明确的 Run")
	}
	if request.Expected.SubjectID != target.task.ID || request.Expected.SubjectRevisionID != target.run.RevisionID {
		return TaskHubRunControlMutationResult{}, fmt.Errorf("运行控制 checkpoint 不属于当前 Task 或 Run revision")
	}
	action, err := taskHubStoreControlAction(request.Action)
	if err != nil {
		return TaskHubRunControlMutationResult{}, err
	}
	operation, err := services.Control.Request(ctx, app.RequestExecutionControlRequest{
		OperationKey:   request.IdempotencyKey,
		Action:         action,
		RunID:          target.run.ID,
		StageAttemptID: strings.TrimSpace(request.Target.StageAttemptID),
		Expected: app.ControlCheckpoint{
			Sequence:            request.Expected.Sequence,
			ExecutionEpoch:      request.Expected.ExecutionEpoch,
			SubjectVersion:      request.Expected.SubjectVersion,
			SubjectID:           request.Expected.SubjectID,
			SubjectRevisionID:   request.Expected.SubjectRevisionID,
			SubjectDigest:       request.Expected.SubjectDigest,
			WorkflowFingerprint: request.Expected.WorkflowFingerprint,
		},
		Actor:  request.Actor,
		Reason: request.Reason,
	})
	if err != nil {
		return TaskHubRunControlMutationResult{}, err
	}
	return TaskHubRunControlMutationResult{
		Action:      request.Action,
		OperationID: operation.ID,
		Summary:     taskHubRunControlActionLabel(request.Action) + "请求已提交到 durable 控制队列",
	}, nil
}

func taskHubStoreControlAction(action TaskHubRunControlAction) (store.ControlAction, error) {
	switch action {
	case TaskHubRunControlPause:
		return store.ControlActionPause, nil
	case TaskHubRunControlCancelStage:
		return store.ControlActionCancelStage, nil
	case TaskHubRunControlTerminate:
		return store.ControlActionTerminate, nil
	default:
		return "", fmt.Errorf("unknown Task Hub run control action %q", action)
	}
}

func (adapter *AppTaskHubLifecycleAdapter) taskHubAttachLease(ctx context.Context, services *app.LifecycleServices, runID string) (*store.DurableJob, *store.Lease, error) {
	if services == nil || services.Inspection == nil {
		return nil, nil, fmt.Errorf("lifecycle inspection service is not configured")
	}
	detail, err := services.Inspection.ReadTaskDetail(ctx, app.TaskInspectionQuery{RunID: runID})
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	for _, inspectedRun := range detail.Runs {
		if inspectedRun.Run.ID != runID {
			continue
		}
		for jobIndex := range inspectedRun.Jobs {
			job := inspectedRun.Jobs[jobIndex].Job
			if job.State != store.JobRunning {
				continue
			}
			for leaseIndex := range inspectedRun.Jobs[jobIndex].Leases {
				lease := inspectedRun.Jobs[jobIndex].Leases[leaseIndex]
				if lease.State == store.LeaseActive && lease.ExpiresAt.After(now) {
					return &job, &lease, nil
				}
			}
		}
	}
	return nil, nil, nil
}

func validateTaskHubMutationRequest(request TaskHubMutationRequest) error {
	if !taskHubActionKnown(request.Action) {
		return fmt.Errorf("unknown Task Hub action %q", request.Action)
	}
	if strings.TrimSpace(request.Actor) == "" || strings.TrimSpace(request.Reason) == "" {
		return fmt.Errorf("Task Hub confirmation actor and reason are required")
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(request.IdempotencyKey)); err != nil {
		return err
	}
	return nil
}

func validateTaskHubRunControlMutationRequest(request TaskHubRunControlMutationRequest) error {
	if !taskHubRunControlActionKnown(request.Action) {
		return fmt.Errorf("unknown Task Hub run control action %q", request.Action)
	}
	if strings.TrimSpace(request.Actor) == "" || strings.TrimSpace(request.Reason) == "" {
		return fmt.Errorf("Task Hub run control actor and reason are required")
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(request.IdempotencyKey)); err != nil {
		return err
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(request.Target.RunID)); err != nil {
		return err
	}
	return nil
}

func taskHubContinuationPlanPreview(plan workflowkit.ContinuationPlan) TaskHubPlanPreview {
	snapshot := plan.Snapshot()
	executionScope := make([]string, 0)
	invalidated := make([]string, 0)
	reused := make([]string, 0)
	for _, transition := range snapshot.Nodes {
		label := taskHubContinuationTransitionLabel(transition)
		switch transition.Disposition {
		case workflowkit.DispositionSchedule:
			executionScope = append(executionScope, label)
		case workflowkit.DispositionInvalidate:
			invalidated = append(invalidated, label)
		case workflowkit.DispositionPreserve:
			reused = append(reused, label)
		}
	}
	return TaskHubPlanPreview{
		PlanID:              snapshot.PlanID,
		Title:               "继续处理计划（已冻结）",
		Summary:             fmt.Sprintf("策略：%s；冻结计划将在 %s 失效。", snapshot.Strategy, snapshot.ExpiresAt.UTC().Format(time.RFC3339)),
		Reason:              "基于当前 checkpoint 和已冻结 workflow 生成。",
		RevisionImpact:      taskHubContinuationRevisionImpact(snapshot),
		ExecutionScope:      executionScope,
		InvalidatedEvidence: invalidated,
		ReusedEvidence:      reused,
		ExternalEffects:     taskHubContinuationExternalEffects(snapshot),
		ConfirmationNeeded:  true,
	}
}

func taskHubContinuationTransitionLabel(transition workflowkit.NodeTransition) string {
	reasons := make([]string, 0, len(transition.ReasonCodes))
	for _, reason := range transition.ReasonCodes {
		if value := strings.TrimSpace(string(reason)); value != "" {
			reasons = append(reasons, value)
		}
	}
	sort.Strings(reasons)
	unique := reasons[:0]
	for _, reason := range reasons {
		if len(unique) == 0 || unique[len(unique)-1] != reason {
			unique = append(unique, reason)
		}
	}
	if len(unique) == 0 {
		return string(transition.NodeID)
	}
	return string(transition.NodeID) + "（" + strings.Join(unique, ", ") + "）"
}

func taskHubContinuationRevisionImpact(snapshot workflowkit.ContinuationPlanSnapshot) string {
	if snapshot.CandidateRevisionID != "" {
		return "计划包含候选 revision；仅在最终执行 CAS 成功时提交新版本。"
	}
	return "当前 TaskRevision 保持不变。"
}

func taskHubContinuationExternalEffects(snapshot workflowkit.ContinuationPlanSnapshot) []string {
	if len(snapshot.ExternalEffectConfirmations) == 0 {
		return nil
	}
	effects := make([]string, 0, len(snapshot.ExternalEffectConfirmations))
	for _, confirmation := range snapshot.ExternalEffectConfirmations {
		effects = append(effects, string(confirmation.NodeID))
	}
	return effects
}

func taskHubReviewDecisionAction(action TaskHubAction) store.ReviewDecisionAction {
	switch action {
	case TaskHubActionApproveReview:
		return store.ReviewDecisionApprove
	case TaskHubActionRequestChanges:
		return store.ReviewDecisionRequestChanges
	default:
		return store.ReviewDecisionRejectTerminal
	}
}

// openTaskHubReview resolves a specific open review. The adapter deliberately
// refuses to choose between multiple open reviews: the projection must carry a
// stable ReviewRequestID captured when the operator started the action.
func (adapter *AppTaskHubLifecycleAdapter) openTaskHubReview(ctx context.Context, services *app.LifecycleServices, target taskHubResolvedTarget, allowRecordedReplay bool) (store.ReviewRequest, string, error) {
	if services == nil || services.Store() == nil {
		return store.ReviewRequest{}, "", fmt.Errorf("review inspection store is unavailable")
	}
	revisionID := strings.TrimSpace(target.reviewRevisionID)
	if revisionID == "" {
		revision, unavailable, err := target.currentRevision(ctx, services)
		if err != nil {
			return store.ReviewRequest{}, "", err
		}
		if unavailable != "" {
			return store.ReviewRequest{}, unavailable, nil
		}
		revisionID = revision.ID
	}
	revision, err := services.Revisions.Get(ctx, revisionID)
	if err != nil {
		return store.ReviewRequest{}, "", err
	}
	if revision.TaskID != target.task.ID {
		return store.ReviewRequest{}, "", fmt.Errorf("review revision does not belong to target Task")
	}
	reviews, err := services.Store().ListReviewRequestsForRevision(ctx, revision.ID)
	if err != nil {
		return store.ReviewRequest{}, "", err
	}
	requestedID := strings.TrimSpace(targetReviewRequestID(target))
	open := make([]store.ReviewRequest, 0, len(reviews))
	for _, review := range reviews {
		if review.State == "open" {
			open = append(open, review)
		}
	}
	if requestedID != "" {
		for _, review := range reviews {
			if review.ID != requestedID {
				continue
			}
			if review.State != "open" && !allowRecordedReplay {
				return store.ReviewRequest{}, "指定的 ReviewRequest 已关闭或不属于当前 revision", nil
			}
			return review, "", nil
		}
		return store.ReviewRequest{}, "指定的 ReviewRequest 已关闭或不属于当前 revision", nil
	}
	if len(open) == 0 {
		return store.ReviewRequest{}, "当前 revision 没有打开的 ReviewRequest", nil
	}
	if len(open) > 1 {
		return store.ReviewRequest{}, "当前 revision 有多个打开的 ReviewRequest；请从详情选择明确审核目标", nil
	}
	return open[0], "", nil
}

func targetReviewRequestID(target taskHubResolvedTarget) string {
	return target.reviewRequestID
}

func unavailableRunControlPlan(action TaskHubRunControlAction, reason string) TaskHubPlanPreview {
	return TaskHubPlanPreview{
		Title:   taskHubRunControlActionLabel(action) + "（不可提交）",
		Summary: "当前状态下不能生成可提交的运行控制命令；本次调用只读取事实，不会创建 ControlOperation。",
		Reason:  reason,
	}
}

func taskHubRunControlTargetReason(action TaskHubRunControlAction, run store.WorkflowRun, stage *store.StageAttempt) string {
	switch action {
	case TaskHubRunControlPause:
		if run.Status != store.WorkflowRunRunning {
			return fmt.Sprintf("Run 当前处于 %s；只有 running Run 可以请求暂停", run.Status)
		}
	case TaskHubRunControlCancelStage:
		if stage == nil {
			return "当前没有明确的非终态 StageAttempt，无法确定取消目标"
		}
		if taskHubStageAttemptTerminal(stage.ExecutionStatus) {
			return fmt.Sprintf("StageAttempt 当前处于 %s，已是终态", stage.ExecutionStatus)
		}
		return "当前 StageAttempt 未声明 cancel capability；不能由 TUI 推断 provider 是否支持取消"
	case TaskHubRunControlTerminate:
		switch run.Status {
		case store.WorkflowRunQueued, store.WorkflowRunRunning, store.WorkflowRunPauseRequested, store.WorkflowRunPausing,
			store.WorkflowRunPaused, store.WorkflowRunResumeRequested, store.WorkflowRunWaitingReview, store.WorkflowRunWaitingContinuation:
			return ""
		default:
			return fmt.Sprintf("Run 当前处于 %s，不能请求终止", run.Status)
		}
	}
	return ""
}

func taskHubRunControlPotentialEffects(action TaskHubRunControlAction) []string {
	switch action {
	case TaskHubRunControlPause:
		return []string{"提交后 worker 将请求 checkpoint；本次预览不会发出该请求"}
	case TaskHubRunControlCancelStage:
		return []string{"提交后仅会影响选中的 StageAttempt；本次预览不会取消阶段"}
	case TaskHubRunControlTerminate:
		return []string{"提交后 worker 会执行目标 scoped 的 graceful termination；本次预览不会终止运行"}
	default:
		return nil
	}
}

func (adapter *AppTaskHubLifecycleAdapter) lifecycleServices() (*app.LifecycleServices, error) {
	if adapter == nil || adapter.services == nil {
		return nil, fmt.Errorf("Task Hub lifecycle services are unavailable")
	}
	services := adapter.services
	if services.Tasks == nil || services.Revisions == nil || services.Runs == nil || services.Releases == nil {
		return nil, fmt.Errorf("Task Hub lifecycle services are incomplete")
	}
	return services, nil
}

type taskHubQueuedRun struct {
	id        string
	createdAt time.Time
}

func (adapter *AppTaskHubLifecycleAdapter) projectTask(ctx context.Context, services *app.LifecycleServices, task store.TaskV2) (TaskHubTask, []TaskHubRun, []taskHubQueuedRun, error) {
	revisions, err := services.Revisions.List(ctx, task.ID)
	if err != nil {
		return TaskHubTask{}, nil, nil, fmt.Errorf("list revisions for Task %s: %w", task.ID, err)
	}
	runs, err := services.Runs.ListForTask(ctx, task.ID)
	if err != nil {
		return TaskHubTask{}, nil, nil, fmt.Errorf("list runs for Task %s: %w", task.ID, err)
	}
	releases, err := services.Releases.List(ctx, task.ID)
	if err != nil {
		return TaskHubTask{}, nil, nil, fmt.Errorf("list local packages for Task %s: %w", task.ID, err)
	}

	current := currentTaskHubRevision(task, revisions)
	projection := TaskHubTask{
		TaskID:        task.ID,
		Name:          taskHubTaskName(task),
		Lifecycle:     string(task.LifecycleState),
		LatestRelease: latestTaskHubRelease(releases),
		UpdatedAt:     task.UpdatedAt,
	}
	if current != nil {
		projection.RevisionID = current.ID
		projection.Revision = fmt.Sprintf("v%d", current.VersionNumber)
		projection.TaskDigest = current.TaskDigest
	}
	activeReview, err := appTaskHubSingleOpenReview(ctx, services, revisions)
	if err != nil {
		return TaskHubTask{}, nil, nil, err
	}
	if activeReview != nil {
		projection.ActiveReview = "open"
		projection.ActiveReviewID = activeReview.ID
		projection.ActiveReviewRevisionID = activeReview.RevisionID
	}
	projection.Actions = appTaskHubTaskActions(current, activeReview != nil)

	resultRuns := make([]TaskHubRun, 0, len(runs))
	queued := make([]taskHubQueuedRun, 0)
	for _, run := range runs {
		runProjection, projectErr := adapter.projectTaskHubRun(ctx, services, run)
		if projectErr != nil {
			return TaskHubTask{}, nil, nil, projectErr
		}
		resultRuns = append(resultRuns, runProjection)
		if run.Status == store.WorkflowRunQueued {
			queued = append(queued, taskHubQueuedRun{id: run.ID, createdAt: run.CreatedAt})
		}
	}
	return projection, resultRuns, queued, nil
}

func appTaskHubSingleOpenReview(ctx context.Context, services *app.LifecycleServices, revisions []store.TaskRevision) (*store.ReviewRequest, error) {
	if len(revisions) == 0 || services == nil || services.Store() == nil {
		return nil, nil
	}
	var active *store.ReviewRequest
	for _, revision := range revisions {
		reviews, err := services.Store().ListReviewRequestsForRevision(ctx, revision.ID)
		if err != nil {
			return nil, fmt.Errorf("list review requests for Task Hub revision %s: %w", revision.ID, err)
		}
		for index := range reviews {
			if reviews[index].State != "open" {
				continue
			}
			if active != nil {
				// An ambiguous review target must remain non-actionable from the task
				// list. The detail surface can expose the individual durable IDs.
				return nil, nil
			}
			candidate := reviews[index]
			active = &candidate
		}
	}
	return active, nil
}

func (adapter *AppTaskHubLifecycleAdapter) projectTaskHubRun(ctx context.Context, services *app.LifecycleServices, run store.WorkflowRun) (TaskHubRun, error) {
	projection := TaskHubRun{
		RunID:          run.ID,
		TaskID:         run.TaskID,
		RevisionID:     run.RevisionID,
		ExecutionState: string(run.Status),
		Active:         taskHubRunIsActive(run.Status),
		Actions:        appTaskHubRunActions(run),
	}
	if run.StartedAt != nil {
		projection.StartedAt = *run.StartedAt
	}
	if run.FinishedAt != nil {
		projection.FinishedAt = *run.FinishedAt
	}
	attempts, err := services.Runs.ListStageAttempts(ctx, run.ID)
	if err != nil {
		return TaskHubRun{}, fmt.Errorf("list stage attempts for Run %s: %w", run.ID, err)
	}
	stage := currentTaskHubStageAttempt(attempts)
	if stage != nil {
		projection.Stage = taskHubStageAttemptLabel(*stage)
		projection.Control.StageAttemptID = stage.ID
		projection.Control.StageExecutionState = string(stage.ExecutionStatus)
	}
	if services.Control == nil {
		projection.Control.Actions = appTaskHubRunControlActions(run, stage, false)
		return projection, nil
	}
	checkpoint, err := services.Control.CurrentCheckpoint(ctx, run.ID)
	if err != nil {
		return TaskHubRun{}, fmt.Errorf("read control checkpoint for Run %s: %w", run.ID, err)
	}
	projection.Control.CheckpointSequence = checkpoint.Sequence
	projection.Control.ExecutionEpoch = checkpoint.ExecutionEpoch
	projection.Control.Expected = taskHubControlCheckpoint(checkpoint)
	gracePeriod, err := services.Control.FrozenGracePeriod(ctx, run.ID)
	if err != nil {
		return TaskHubRun{}, fmt.Errorf("read frozen control grace period for Run %s: %w", run.ID, err)
	}
	projection.Control.GracePeriod = gracePeriod
	operations, err := services.Control.ListForRun(ctx, run.ID)
	if err != nil {
		return TaskHubRun{}, fmt.Errorf("list control operations for Run %s: %w", run.ID, err)
	}
	if len(operations) > 0 {
		latest := operations[0]
		projection.ControlStatus = string(latest.Status)
		projection.Control.OperationID = latest.ID
		projection.Control.OperationAction = TaskHubRunControlAction(latest.Action)
		projection.Control.OperationStatus = string(latest.Status)
		projection.Control.CheckpointID = latest.CheckpointID
		projection.Control.QuotaSettlementID = latest.QuotaSettlementID
		projection.Control.RuntimeReceiptCount = len(latest.RuntimeReceipts)
		projection.Control.FailureReason = latest.FailureReason
		for _, receipt := range latest.RuntimeReceipts {
			if receipt.ExternalOutcomeUnknown {
				projection.Control.ExternalOutcomeUnknown = true
				break
			}
		}
	}
	projection.Control.Actions = appTaskHubRunControlActions(run, stage, true)
	return projection, nil
}

func taskHubControlCheckpoint(checkpoint app.ControlCheckpoint) TaskHubControlCheckpoint {
	return TaskHubControlCheckpoint{
		Sequence:            checkpoint.Sequence,
		ExecutionEpoch:      checkpoint.ExecutionEpoch,
		SubjectVersion:      checkpoint.SubjectVersion,
		SubjectID:           checkpoint.SubjectID,
		SubjectRevisionID:   checkpoint.SubjectRevisionID,
		SubjectDigest:       checkpoint.SubjectDigest,
		WorkflowFingerprint: checkpoint.WorkflowFingerprint,
	}
}

func currentTaskHubStageAttempt(attempts []store.StageAttempt) *store.StageAttempt {
	var current *store.StageAttempt
	for index := range attempts {
		if taskHubStageAttemptTerminal(attempts[index].ExecutionStatus) {
			continue
		}
		candidate := attempts[index]
		current = &candidate
	}
	return current
}

func taskHubStageAttemptTerminal(status store.StageExecutionStatus) bool {
	switch status {
	case store.StageExecutionCompleted, store.StageExecutionInfraFailed, store.StageExecutionInterrupted, store.StageExecutionCanceled:
		return true
	default:
		return false
	}
}

func taskHubStageAttemptLabel(attempt store.StageAttempt) string {
	group := strings.TrimSpace(attempt.StageGroup)
	key := strings.TrimSpace(attempt.StageKey)
	switch {
	case group != "" && key != "" && group != key:
		return group + "/" + key
	case group != "":
		return group
	default:
		return key
	}
}

func currentTaskHubRevision(task store.TaskV2, revisions []store.TaskRevision) *store.TaskRevision {
	if strings.TrimSpace(task.CurrentRevisionID) == "" {
		return nil
	}
	for index := range revisions {
		if revisions[index].ID == task.CurrentRevisionID {
			return &revisions[index]
		}
	}
	return nil
}

func taskHubTaskName(task store.TaskV2) string {
	if value := strings.TrimSpace(task.Title); value != "" {
		return value
	}
	if value := strings.TrimSpace(task.Slug); value != "" {
		return value
	}
	return task.ID
}

func latestTaskHubRelease(releases []store.LocalPackageRelease) string {
	for _, release := range releases {
		if release.WithdrawnAt == nil {
			return release.ReleaseVersion
		}
	}
	if len(releases) == 0 {
		return ""
	}
	return releases[0].ReleaseVersion + " (withdrawn)"
}

func assignTaskHubQueuePositions(runs []TaskHubRun, queued []taskHubQueuedRun) {
	sort.SliceStable(queued, func(left, right int) bool {
		if !queued[left].createdAt.Equal(queued[right].createdAt) {
			return queued[left].createdAt.Before(queued[right].createdAt)
		}
		return queued[left].id < queued[right].id
	})
	positionByID := make(map[string]int, len(queued))
	for index, item := range queued {
		positionByID[item.id] = index + 1
	}
	for index := range runs {
		runs[index].QueuePosition = positionByID[runs[index].RunID]
	}
}

func buildTaskHubQueue(runs []TaskHubRun, observedAt time.Time) TaskHubQueue {
	queue := TaskHubQueue{UpdatedAt: observedAt}
	for _, run := range runs {
		if run.ExecutionState == string(store.WorkflowRunQueued) {
			queue.Queued++
			continue
		}
		if taskHubRunConsumesExecutionSlot(store.WorkflowRunStatus(run.ExecutionState)) {
			queue.Running++
		}
	}
	return queue
}

func taskHubRunIsActive(status store.WorkflowRunStatus) bool {
	switch status {
	case store.WorkflowRunSucceeded, store.WorkflowRunFailedRecoverable, store.WorkflowRunFailedTerminal, store.WorkflowRunCanceled, store.WorkflowRunInterrupted:
		return false
	default:
		return true
	}
}

func taskHubRunConsumesExecutionSlot(status store.WorkflowRunStatus) bool {
	switch status {
	case store.WorkflowRunRunning, store.WorkflowRunPauseRequested, store.WorkflowRunPausing, store.WorkflowRunResumeRequested,
		store.WorkflowRunCancelRequested, store.WorkflowRunStopRequested, store.WorkflowRunCanceling:
		return true
	default:
		return false
	}
}

func appTaskHubGlobalActions() []TaskHubActionState {
	return []TaskHubActionState{
		{Action: TaskHubActionNewTask, DisabledReason: "需要标题、来源和审计原因"},
		{Action: TaskHubActionImportTask, DisabledReason: "需要受管快照来源"},
		{Action: TaskHubActionGenerateTask, DisabledReason: "需要仓库和提交信息"},
	}
}

func appTaskHubTaskActions(current *store.TaskRevision, hasSingleOpenReview bool) []TaskHubActionState {
	actions := []TaskHubActionState{
		{Action: TaskHubActionEditTask, DisabledReason: "需要候选快照和变更说明"},
		{Action: TaskHubActionForkTask, DisabledReason: "需要新 Task 的名称和标识"},
		{Action: TaskHubActionArchiveTask, DisabledReason: "需要带 UUIDv7 幂等键的归档命令契约"},
		{Action: TaskHubActionSoftDeleteTask, DisabledReason: "需要带 UUIDv7 幂等键的软删除命令契约"},
		{Action: TaskHubActionRestoreTask, DisabledReason: "需要明确的目标 lifecycle 状态"},
		{Action: TaskHubActionStartRun, DisabledReason: "需要完整且显式的执行 profile"},
		{Action: TaskHubActionApproveReview, Enabled: hasSingleOpenReview, DisabledReason: "需要唯一且打开的 ReviewRequest"},
		{Action: TaskHubActionRequestChanges, Enabled: hasSingleOpenReview, DisabledReason: "需要唯一且打开的 ReviewRequest"},
		{Action: TaskHubActionRejectReview, Enabled: hasSingleOpenReview, DisabledReason: "需要唯一且打开的 ReviewRequest"},
		{Action: TaskHubActionPackageRevision, DisabledReason: "需要当前且已验证的 TaskRevision"},
		{Action: TaskHubActionWithdrawRelease, DisabledReason: "需要明确的 release ID"},
	}
	for index := range actions {
		switch actions[index].Action {
		case TaskHubActionPackageRevision:
			actions[index].Enabled = current != nil &&
				(current.State == store.RevisionStateValidated || current.State == store.RevisionStateReleased) &&
				strings.TrimSpace(current.ValidationEvidenceManifest) != ""
		}
		if actions[index].Enabled {
			actions[index].DisabledReason = ""
		}
	}
	return actions
}

func appTaskHubRunActions(run store.WorkflowRun) []TaskHubActionState {
	continueEnabled := taskHubRunCanContinue(run.Status)
	continueReason := "当前 Run 状态不能进入 continuation planner"
	if continueEnabled {
		continueReason = ""
	}
	attachEnabled := taskHubRunCanAttach(run.Status)
	attachReason := "只有 running durable Run 可以附着"
	if attachEnabled {
		attachReason = ""
	}
	return []TaskHubActionState{
		{Action: TaskHubActionContinue, Enabled: continueEnabled, DisabledReason: continueReason},
		{Action: TaskHubActionAttachRun, Enabled: attachEnabled, DisabledReason: attachReason},
		{Action: TaskHubActionOpenRunControl, Enabled: true},
	}
}

func taskHubRunCanAttach(status store.WorkflowRunStatus) bool {
	switch status {
	case store.WorkflowRunRunning, store.WorkflowRunPauseRequested, store.WorkflowRunPausing,
		store.WorkflowRunResumeRequested, store.WorkflowRunCancelRequested, store.WorkflowRunStopRequested,
		store.WorkflowRunCanceling:
		return true
	default:
		return false
	}
}

func taskHubRunCanContinue(status store.WorkflowRunStatus) bool {
	switch status {
	case store.WorkflowRunPaused, store.WorkflowRunCanceled, store.WorkflowRunFailedRecoverable,
		store.WorkflowRunWaitingContinuation, store.WorkflowRunInterrupted:
		return true
	default:
		return false
	}
}

func appTaskHubRunControlActions(run store.WorkflowRun, stage *store.StageAttempt, controlAvailable bool) []TaskHubRunControlActionState {
	actions := []TaskHubRunControlActionState{
		{Action: TaskHubRunControlPause},
		{Action: TaskHubRunControlCancelStage},
		{Action: TaskHubRunControlTerminate},
	}
	for index := range actions {
		if !controlAvailable {
			actions[index].DisabledReason = "运行控制服务不可用"
			continue
		}
		if reason := taskHubRunControlTargetReason(actions[index].Action, run, stage); reason != "" {
			actions[index].DisabledReason = reason
			continue
		}
		actions[index].Enabled = true
		actions[index].DisabledReason = ""
	}
	return actions
}

func taskHubActionKnown(action TaskHubAction) bool {
	for _, prefix := range []rune{'t', 'x', 'v', 'p'} {
		for _, candidate := range taskHubPrefixActions(prefix) {
			if candidate == action {
				return true
			}
		}
	}
	return false
}

func unavailableTaskHubPlan(action TaskHubAction, reason string) TaskHubPlanPreview {
	return TaskHubPlanPreview{
		Title:   taskHubActionLabel(action),
		Summary: "当前状态下不能生成可确认的计划。",
		Reason:  reason,
	}
}

type taskHubResolvedTarget struct {
	task             store.TaskV2
	run              *store.WorkflowRun
	revision         *store.TaskRevision
	reviewRequestID  string
	reviewRevisionID string
	releaseID        string
}

func (adapter *AppTaskHubLifecycleAdapter) resolveTaskHubTarget(ctx context.Context, services *app.LifecycleServices, target TaskHubTarget) (taskHubResolvedTarget, error) {
	resolved := taskHubResolvedTarget{
		reviewRequestID:  strings.TrimSpace(target.ReviewRequestID),
		reviewRevisionID: strings.TrimSpace(target.ReviewRevisionID),
		releaseID:        strings.TrimSpace(target.ReleaseID),
	}
	if strings.TrimSpace(target.RunID) != "" {
		run, err := services.Runs.Get(ctx, target.RunID)
		if err != nil {
			return resolved, fmt.Errorf("get Run %s for Task Hub plan: %w", target.RunID, err)
		}
		resolved.run = &run
		if target.TaskID != "" && target.TaskID != run.TaskID {
			return resolved, fmt.Errorf("Task Hub target Run %s does not belong to Task %s", run.ID, target.TaskID)
		}
		if target.RevisionID != "" && target.RevisionID != run.RevisionID {
			return resolved, fmt.Errorf("Task Hub target Run %s does not use revision %s", run.ID, target.RevisionID)
		}
		target.TaskID = run.TaskID
		if target.RevisionID == "" {
			target.RevisionID = run.RevisionID
		}
	}
	if strings.TrimSpace(target.RevisionID) != "" {
		revision, err := services.Revisions.Get(ctx, target.RevisionID)
		if err != nil {
			return resolved, fmt.Errorf("get revision %s for Task Hub plan: %w", target.RevisionID, err)
		}
		resolved.revision = &revision
		if target.TaskID != "" && target.TaskID != revision.TaskID {
			return resolved, fmt.Errorf("Task Hub target revision %s does not belong to Task %s", revision.ID, target.TaskID)
		}
		target.TaskID = revision.TaskID
	}
	if strings.TrimSpace(target.TaskID) == "" {
		return resolved, fmt.Errorf("Task Hub action target requires a Task, Run, or revision")
	}
	task, err := services.Tasks.Get(ctx, target.TaskID)
	if err != nil {
		return resolved, fmt.Errorf("get Task %s for Task Hub plan: %w", target.TaskID, err)
	}
	resolved.task = task
	return resolved, nil
}

func (target taskHubResolvedTarget) currentRevision(ctx context.Context, services *app.LifecycleServices) (store.TaskRevision, string, error) {
	if target.revision != nil {
		if target.revision.TaskID != target.task.ID {
			return store.TaskRevision{}, "", fmt.Errorf("Task Hub revision target does not belong to selected Task")
		}
		if target.task.CurrentRevisionID != target.revision.ID {
			return store.TaskRevision{}, "only the current reviewed TaskRevision may be packaged locally", nil
		}
		return *target.revision, "", nil
	}
	if strings.TrimSpace(target.task.CurrentRevisionID) == "" {
		return store.TaskRevision{}, "Task 没有当前 TaskRevision", nil
	}
	revision, err := services.Revisions.Get(ctx, target.task.CurrentRevisionID)
	if err != nil {
		return store.TaskRevision{}, "", fmt.Errorf("get current revision %s: %w", target.task.CurrentRevisionID, err)
	}
	if revision.TaskID != target.task.ID {
		return store.TaskRevision{}, "", fmt.Errorf("current revision %s does not belong to Task %s", revision.ID, target.task.ID)
	}
	return revision, "", nil
}

func filterTaskHubSnapshot(snapshot TaskHubSnapshot, filter string) TaskHubSnapshot {
	needle := strings.ToLower(strings.TrimSpace(filter))
	if needle == "" {
		return snapshot
	}
	matchingTasks := make(map[string]bool, len(snapshot.Tasks))
	for _, task := range snapshot.Tasks {
		matchingTasks[task.TaskID] = taskHubTaskMatches(task, needle)
	}
	matchingRunTaskIDs := make(map[string]bool)
	for _, run := range snapshot.Runs {
		if taskHubRunMatches(run, needle) {
			matchingRunTaskIDs[run.TaskID] = true
		}
	}
	filtered := TaskHubSnapshot{
		Queue:         snapshot.Queue,
		GlobalActions: append([]TaskHubActionState(nil), snapshot.GlobalActions...),
		ObservedAt:    snapshot.ObservedAt,
	}
	for _, task := range snapshot.Tasks {
		if matchingTasks[task.TaskID] || matchingRunTaskIDs[task.TaskID] {
			filtered.Tasks = append(filtered.Tasks, task.Clone())
		}
	}
	for _, run := range snapshot.Runs {
		if matchingTasks[run.TaskID] || taskHubRunMatches(run, needle) {
			filtered.Runs = append(filtered.Runs, run.Clone())
		}
	}
	return filtered
}

func taskHubTaskMatches(task TaskHubTask, needle string) bool {
	return taskHubTextMatches(needle, task.TaskID, task.Name, task.Lifecycle, task.RevisionID, task.Revision, task.TaskDigest, task.LatestRelease)
}

func taskHubRunMatches(run TaskHubRun, needle string) bool {
	return taskHubTextMatches(needle, run.RunID, run.TaskID, run.RevisionID, run.ExecutionState, run.Stage, run.Failure, run.ControlStatus)
}

func taskHubTextMatches(needle string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), needle) {
			return true
		}
	}
	return false
}
