package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// AppTaskHubLifecycleAdapter projects the V2 application services into the
// Task Hub boundary. Every lifecycle confirmation is delegated to the typed
// V12 mutation service; this adapter never writes through Store() or takes a
// shortcut through a legacy domain service.
type AppTaskHubLifecycleAdapter struct {
	services                 *app.LifecycleServices
	runWorkerHandoffLauncher app.RunWorkerHandoffLauncher
}

var _ TaskHubLifecycleService = (*AppTaskHubLifecycleAdapter)(nil)
var _ TaskHubRunControlPlanner = (*AppTaskHubLifecycleAdapter)(nil)
var _ TaskHubMutationPlanner = (*AppTaskHubLifecycleAdapter)(nil)
var _ TaskHubMutationExecutor = (*AppTaskHubLifecycleAdapter)(nil)
var _ TaskHubRunControlMutationExecutor = (*AppTaskHubLifecycleAdapter)(nil)
var _ TaskHubRunHandoffExecutor = (*AppTaskHubLifecycleAdapter)(nil)

// NewAppTaskHubLifecycleAdapter creates the real Task Hub adapter for a V2
// application service bundle. A nil bundle is retained so callers receive a
// useful error from QueryTaskHub or PlanTaskHubCommand rather than a panic.
func NewAppTaskHubLifecycleAdapter(services *app.LifecycleServices) *AppTaskHubLifecycleAdapter {
	return &AppTaskHubLifecycleAdapter{services: services}
}

// NewAppTaskHubLifecycleAdapterWithRunWorkerHandoffLauncher creates the
// composition-aware Task Hub adapter. The launcher is deliberately supplied
// here, outside the TUI state machine: it is the only platform-specific step
// after the application service has durably reserved one Run handoff.
func NewAppTaskHubLifecycleAdapterWithRunWorkerHandoffLauncher(services *app.LifecycleServices, launcher app.RunWorkerHandoffLauncher) *AppTaskHubLifecycleAdapter {
	return &AppTaskHubLifecycleAdapter{services: services, runWorkerHandoffLauncher: launcher}
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

	standardAuthoringAvailable := services.AuthoringLaunches != nil && services.AuthoringLaunches.Available()
	snapshot := TaskHubSnapshot{
		GlobalActions: appTaskHubGlobalActions(services.Mutations != nil, standardAuthoringAvailable),
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
		if services.Mutations == nil {
			return unavailableTaskHubPlan(command.Action, "LifecycleMutationService 不可用"), nil
		}
		return TaskHubPlanPreview{
			Title:              "新建 Task",
			Summary:            "确认表单将收集新 Task 的标识、标题、可选来源与审计原因；提交后由 V12 幂等命令分配全局 UUIDv7 身份。",
			Reason:             "尚未创建任何 Task、revision、Run 或本地 package。",
			RevisionImpact:     "新 Task 以 draft 生命周期创建，不会修改已存在的 TaskRevision。",
			ConfirmationNeeded: true,
		}, nil
	case TaskHubActionStartStandardAuthoring:
		if command.Target != (TaskHubTarget{}) {
			return TaskHubPlanPreview{}, fmt.Errorf("Standard authoring launch is global and cannot target an existing Task, Run, or revision")
		}
		if services.AuthoringLaunches == nil || !services.AuthoringLaunches.Available() {
			return unavailableTaskHubPlan(command.Action, "当前部署未配置受控 Standard 创题定义"), nil
		}
		return TaskHubPlanPreview{
			Title:              "启动 Standard 创题",
			Summary:            "确认表单将收集新题目的标识、标题与可选元数据；提交后捕获已锁定的 Tower HTTP 源码，创建 revision-free draft Task 与 AuthoringSession，并排队 Standard 创题 Run。",
			Reason:             "来源仓库、固定提交、Codex/profile、模型与 catalog/lock 全部由当前部署冻结，不接受 TUI 覆盖。",
			RevisionImpact:     "不会立即创建 TaskRevision；只有 Standard authoring materialize 阶段完成后才会生成首个不可变 revision。",
			ExternalEffects:    []string{"受控捕获已批准的 Tower HTTP 源码", "创建本地 AuthoringSource、AuthoringSession 与 Standard Run"},
			ConfirmationNeeded: true,
		}, nil
	case TaskHubActionImportTask:
		if services.Mutations == nil {
			return unavailableTaskHubPlan(command.Action, "LifecycleMutationService 不可用"), nil
		}
		return TaskHubPlanPreview{
			Title:              "导入 Task",
			Summary:            "确认表单将收集受管快照来源与 Task 元数据；提交后由 V12 幂等命令创建初始不可变 revision。",
			Reason:             "导入源会在提交时按受管快照规则验证。",
			RevisionImpact:     "创建独立 Task 和初始 imported revision；不会修改现有 TaskRevision。",
			ConfirmationNeeded: true,
		}, nil
	}

	target, err := adapter.resolveTaskHubTarget(ctx, services, command.Target)
	if err != nil {
		return TaskHubPlanPreview{}, err
	}

	switch command.Action {
	case TaskHubActionEditTask:
		if services.Mutations == nil {
			return unavailableTaskHubPlan(command.Action, "LifecycleMutationService 不可用"), nil
		}
		if target.run == nil {
			return unavailableTaskHubPlan(command.Action, "创建修改需要从 Run 视图选择明确的 Run"), nil
		}
		revision, unavailable, err := target.currentRevision(ctx, services)
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		if unavailable != "" {
			return unavailableTaskHubPlan(command.Action, unavailable), nil
		}
		if target.run.RevisionID != revision.ID {
			return unavailableTaskHubPlan(command.Action, "只能基于当前 TaskRevision 对应的 Run 创建候选修改"), nil
		}
		expected, err := adapter.captureTaskHubMutationCheckpoint(ctx, services, target.task.ID, revision.ID, target.run.ID, "")
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		return TaskHubPlanPreview{
			Title:              "基于当前 revision 创建 draft 修改",
			Summary:            "确认表单将收集 unified diff；提交前先在隔离候选快照中冻结变更计划，第二次确认才会提交新 revision。",
			Reason:             "修改绑定当前 Task、revision 与 Run checkpoint。",
			RevisionImpact:     "不会原地修改当前 TaskRevision；有实际 digest 变化时才提交候选 revision。",
			ExecutionScope:     []string{target.task.ID, revision.ID, target.run.ID},
			ConfirmationNeeded: true,
			Expected:           expected,
		}, nil
	case TaskHubActionForkTask:
		if services.Mutations == nil {
			return unavailableTaskHubPlan(command.Action, "LifecycleMutationService 不可用"), nil
		}
		var revision store.TaskRevision
		if target.revision != nil {
			revision = *target.revision
		} else if strings.TrimSpace(target.task.CurrentRevisionID) != "" {
			current, err := services.Revisions.Get(ctx, target.task.CurrentRevisionID)
			if err != nil {
				return TaskHubPlanPreview{}, err
			}
			revision = current
		} else {
			return unavailableTaskHubPlan(command.Action, "Fork 需要从详情选择明确的源 revision"), nil
		}
		expected, err := adapter.captureTaskHubMutationCheckpoint(ctx, services, target.task.ID, revision.ID, "", "")
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		return TaskHubPlanPreview{
			Title:              "Fork Task",
			Summary:            "确认表单将收集新 Task 的标识和标题；提交后由 V12 幂等命令复制当前不可变 revision 到新的 Task 身份。",
			Reason:             "Fork 绑定当前 Task 与当前 revision checkpoint。",
			RevisionImpact:     "创建独立 Task 和 fork revision，不会修改源 TaskRevision。",
			ExecutionScope:     []string{target.task.ID, revision.ID},
			ConfirmationNeeded: true,
			Expected:           expected,
		}, nil
	case TaskHubActionArchiveTask:
		if services.Mutations == nil {
			return unavailableTaskHubPlan(command.Action, "LifecycleMutationService 不可用"), nil
		}
		if target.task.LifecycleState != store.TaskLifecyclePublished {
			return unavailableTaskHubPlan(command.Action, fmt.Sprintf("Task 当前处于 %s；只有 published Task 可以归档", target.task.LifecycleState)), nil
		}
		expected, err := adapter.captureTaskHubMutationCheckpoint(ctx, services, target.task.ID, "", "", "")
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		return taskHubTaskTransitionPreview(command.Action, target.task, expected), nil
	case TaskHubActionSoftDeleteTask:
		if services.Mutations == nil {
			return unavailableTaskHubPlan(command.Action, "LifecycleMutationService 不可用"), nil
		}
		if target.task.LifecycleState == store.TaskLifecycleDeleted {
			return unavailableTaskHubPlan(command.Action, "Task 已处于 deleted；请使用恢复操作"), nil
		}
		expected, err := adapter.captureTaskHubMutationCheckpoint(ctx, services, target.task.ID, "", "", "")
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		return taskHubTaskTransitionPreview(command.Action, target.task, expected), nil
	case TaskHubActionRestoreTask:
		if services.Mutations == nil {
			return unavailableTaskHubPlan(command.Action, "LifecycleMutationService 不可用"), nil
		}
		if target.task.LifecycleState != store.TaskLifecycleDeleted {
			return unavailableTaskHubPlan(command.Action, fmt.Sprintf("Task 当前处于 %s；只有 deleted Task 可以恢复", target.task.LifecycleState)), nil
		}
		expected, err := adapter.captureTaskHubMutationCheckpoint(ctx, services, target.task.ID, "", "", "")
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		return taskHubTaskTransitionPreview(command.Action, target.task, expected), nil
	case TaskHubActionStartRun:
		if services.Mutations == nil {
			return unavailableTaskHubPlan(command.Action, "LifecycleMutationService 不可用"), nil
		}
		revision, unavailable, err := target.currentRevision(ctx, services)
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		if unavailable != "" {
			return unavailableTaskHubPlan(command.Action, unavailable), nil
		}
		expected, err := adapter.captureTaskHubMutationCheckpoint(ctx, services, target.task.ID, revision.ID, "", "")
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		return TaskHubPlanPreview{
			Title:              "启动新 Run",
			Summary:            "确认表单会收集独立的 execution profile 与 execution specification；首次确认将把二者 canonicalize 后迁入受管目录，第二次确认才创建冻结 Run。",
			Reason:             "Run 绑定当前 TaskRevision 与 digest，不会从 workspace 或 UI 默认值推导执行输入。",
			RevisionImpact:     "不会修改 TaskRevision 内容。",
			ExecutionScope:     []string{target.task.ID, revision.ID},
			ExternalEffects:    []string{"首次确认写入受管的 profile/spec 冻结副本"},
			ConfirmationNeeded: true,
			Expected:           expected,
		}, nil
	case TaskHubActionEvaluateCodeEdge:
		if services.EvaluatorLaunches == nil || !services.EvaluatorLaunches.Available() {
			return unavailableTaskHubPlan(command.Action, "当前部署未配置受控 CodeEdge 评测定义"), nil
		}
		if adapter == nil || adapter.runWorkerHandoffLauncher == nil {
			return unavailableTaskHubPlan(command.Action, "当前 Task Hub 未配置受控 CodeEdge 评测 worker launcher"), nil
		}
		if target.run == nil {
			return unavailableTaskHubPlan(command.Action, "执行 CodeEdge 评测需要明确的已批准 Phase-1 父 Run"), nil
		}
		launchPlan, err := services.EvaluatorLaunches.Plan(ctx, target.run.ID)
		if err != nil {
			return unavailableTaskHubPlan(command.Action, taskHubCodeEdgeEvaluatorPlanUnavailableReason(err)), nil
		}
		expected, err := adapter.captureTaskHubMutationCheckpoint(ctx, services, launchPlan.TaskID, launchPlan.RevisionID, launchPlan.ParentRunID, "")
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		return TaskHubPlanPreview{
			Title:              "执行 CodeEdge 评测",
			Summary:            "首次确认将冻结 Qwen 后 Opus 的严格 pass@4 child Run 说明书；第二次确认才会创建 child Run 并启动受控 worker。",
			Reason:             "父 Run 已通过 FinalReview，并绑定当前 TaskRevision、受控 catalog/lock 与评测定义。",
			RevisionImpact:     "不会修改 TaskRevision；评测证据仅写入新的 parent-linked child Run。",
			ExecutionScope:     []string{launchPlan.ParentRunID, launchPlan.TaskID, launchPlan.RevisionID, string(launchPlan.Template.ID) + "@" + launchPlan.Template.Version},
			BudgetImpact:       "Qwen 4 个逻辑 Trial，随后 Opus 4 个逻辑 Trial；每个阶段禁止通用 retry。",
			ExternalEffects:    []string{"第二次确认将创建 child Run、durable job 和一个受控 child-worker handoff", "受控 worker 将按冻结白名单调用 Harbor"},
			ConfirmationNeeded: true,
			Expected:           expected,
		}, nil
	case TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff:
		if services.Mutations == nil {
			return unavailableTaskHubPlan(command.Action, "LifecycleMutationService 不可用"), nil
		}
		if services.EvaluatorEvidenceHandoffs == nil {
			return unavailableTaskHubPlan(command.Action, "CodeEdge evaluator 证据交接服务不可用"), nil
		}
		if target.run == nil {
			return unavailableTaskHubPlan(command.Action, "采用 CodeEdge 评测证据需要选择完成的 evaluator child Run"), nil
		}
		parent, child, err := adapter.resolveTaskHubCodeEdgeEvaluatorEvidenceHandoffRuns(ctx, services, target.run.ID)
		if err != nil {
			return unavailableTaskHubPlan(command.Action, taskHubCodeEdgeEvaluatorEvidenceHandoffUnavailableReason(err)), nil
		}
		existing, err := services.Store().GetCodeEdgeEvaluatorEvidenceHandoffForParentRun(ctx, parent.ID)
		if err != nil {
			return TaskHubPlanPreview{}, fmt.Errorf("read CodeEdge evaluator evidence handoff for parent Run %s: %w", parent.ID, err)
		}
		if existing != nil {
			return unavailableTaskHubPlan(command.Action, "该 Phase-1 父 Run 已采用一份不可变的 CodeEdge evaluator 证据交接"), nil
		}
		handoffPlan, err := services.EvaluatorEvidenceHandoffs.Plan(ctx, parent.ID, child.ID)
		if err != nil {
			return unavailableTaskHubPlan(command.Action, taskHubCodeEdgeEvaluatorEvidenceHandoffUnavailableReason(err)), nil
		}
		if handoffPlan.ParentRunID != parent.ID || handoffPlan.ChildRunID != child.ID ||
			handoffPlan.TaskID != parent.TaskID || handoffPlan.RevisionID != parent.RevisionID {
			return unavailableTaskHubPlan(command.Action, "CodeEdge evaluator 证据交接计划未绑定所选 child 与其冻结父 Run"), nil
		}
		expected, err := adapter.captureTaskHubMutationCheckpoint(ctx, services, parent.TaskID, parent.RevisionID, parent.ID, "")
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		return TaskHubPlanPreview{
			Title:              "采用 CodeEdge 评测证据",
			Summary:            "首次确认将冻结已验证的 Qwen/Opus pass@4 child 证据交接；再次确认才会将其采用到 Phase-1 父 Run。",
			Reason:             "已从完成的 evaluator child Run 重新解析其不可变 parent lineage、Qwen/Opus trial 集合与证据指纹。",
			RevisionImpact:     "不会修改 TaskRevision；仅为已冻结 Phase-1 父 Run 记录一份不可变证据交接。",
			ExecutionScope:     []string{parent.ID, child.ID, parent.TaskID, parent.RevisionID},
			ReusedEvidence:     []string{string(handoffPlan.HandoffFingerprint), string(handoffPlan.QwenTrialFingerprint), string(handoffPlan.OpusTrialFingerprint)},
			ConfirmationNeeded: true,
			Expected:           expected,
		}, nil
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
		if strings.TrimSpace(target.authoringReviewRequestID) != "" {
			return adapter.planTaskHubAuthoringReview(ctx, services, command.Action, target)
		}
		if services.Mutations == nil {
			return unavailableTaskHubPlan(command.Action, "LifecycleMutationService 不可用"), nil
		}
		review, unavailable, err := adapter.openTaskHubReview(ctx, services, target)
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		if unavailable != "" {
			return unavailableTaskHubPlan(command.Action, unavailable), nil
		}
		expected, err := adapter.captureTaskHubReviewCheckpoint(ctx, services, target.task.ID, review.RevisionID, review.ID)
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		return TaskHubPlanPreview{
			Title:              taskHubActionLabel(command.Action),
			Summary:            "确认后将通过 V12 幂等审核命令记录不可变决定并关闭该 ReviewRequest。",
			Reason:             "审核请求 " + review.ID + " 当前处于 open 状态。",
			RevisionImpact:     "审核决定绑定当前 revision digest；不会原地修改 TaskRevision 内容。",
			ExecutionScope:     []string{target.task.ID, review.RevisionID, review.ID},
			ConfirmationNeeded: true,
			Expected:           expected,
		}, nil
	case TaskHubActionPackageRevision:
		if services.Mutations == nil {
			return unavailableTaskHubPlan(command.Action, "LifecycleMutationService 不可用"), nil
		}
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
		runs, err := services.Runs.ListForTask(ctx, target.task.ID)
		if err != nil {
			return TaskHubPlanPreview{}, fmt.Errorf("list Runs for CodeEdge package authorization: %w", err)
		}
		preferredRunID := ""
		if target.run != nil {
			preferredRunID = target.run.ID
		}
		authorization, unavailable, err := taskHubCodeEdgePackageAuthorization(ctx, services, target.task, revision, runs, preferredRunID)
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		if unavailable != "" {
			return unavailableTaskHubPlan(command.Action, unavailable), nil
		}
		selectedRunID := ""
		executionScope := []string{target.task.ID, revision.ID}
		reason := "当前 revision 已验证，并绑定了验证证据。"
		summary := "确认后只会在 Harbor Flow 受管目录生成不可变本地 package；不会上传、复制到外部目的地或调用 provider。"
		if authorization != nil {
			selectedRunID = authorization.Run.ID
			executionScope = append(executionScope, authorization.Run.ID, authorization.Record.ID)
			reason = "当前 revision 存在 CodeEdge Phase-1 Run；已选择与当前 TaskRevision/digest 绑定的 approved 最终合规记录。"
			summary = "确认后只会在 Harbor Flow 受管目录生成不可变本地 package；已冻结选中 CodeEdge Run、最终合规记录和授权指纹；不会上传、复制到外部目的地或调用 provider。"
		}
		expected, err := adapter.captureTaskHubMutationCheckpoint(ctx, services, target.task.ID, revision.ID, selectedRunID, "")
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		return TaskHubPlanPreview{
			Title:              "生成本地 package",
			Summary:            summary,
			Reason:             reason,
			RevisionImpact:     "revision 内容和 digest 不变；仅记录本地 package receipt。",
			ExecutionScope:     executionScope,
			ConfirmationNeeded: true,
			Expected:           expected,
		}, nil
	case TaskHubActionWithdrawRelease:
		if services.Mutations == nil {
			return unavailableTaskHubPlan(command.Action, "LifecycleMutationService 不可用"), nil
		}
		releaseID := strings.TrimSpace(target.releaseID)
		if releaseID == "" {
			return unavailableTaskHubPlan(command.Action, "请从详情“本地包”分类选择明确的未撤回 release"), nil
		}
		release, err := services.Store().GetLocalPackageRelease(ctx, releaseID)
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		if release == nil || release.TaskID != target.task.ID {
			return unavailableTaskHubPlan(command.Action, "指定 release 不属于当前 Task 或已不存在"), nil
		}
		if release.WithdrawnAt != nil {
			return unavailableTaskHubPlan(command.Action, "指定 release 已撤回"), nil
		}
		expected, err := adapter.captureTaskHubMutationCheckpoint(ctx, services, target.task.ID, release.RevisionID, "", release.ID)
		if err != nil {
			return TaskHubPlanPreview{}, err
		}
		return TaskHubPlanPreview{
			Title:              "撤回本地 package",
			Summary:            "确认后由 V12 幂等命令记录 release 撤回；package 文件和证据不会被删除或上传。",
			Reason:             "release " + release.ReleaseVersion + " 当前未撤回。",
			RevisionImpact:     "TaskRevision 内容与 digest 不变；仅写入撤回 receipt。",
			ExecutionScope:     []string{target.task.ID, release.RevisionID, release.ID},
			ConfirmationNeeded: true,
			Expected:           expected,
		}, nil
	default:
		return TaskHubPlanPreview{}, fmt.Errorf("unsupported Task Hub action %q", command.Action)
	}
}

// PlanTaskHubRunControl reads the currently authoritative Run and returns a
// non-mutating impact preview. The Task Hub deliberately does not collect the
// actor, reason, idempotency key, confirmation, or provider capability needed
// to submit a Run action, so this method can never submit one.
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
	if command.Action == TaskHubRunControlReconcile {
		return adapter.planTaskHubLocalRunReconcile(ctx, services, target)
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

// planTaskHubLocalRunReconcile previews exactly the local boundary exposed by
// `harbor run reconcile`. It intentionally does not inspect a provider,
// interpret external receipts, or promise that an unknown external effect has
// been resolved.
func (adapter *AppTaskHubLifecycleAdapter) planTaskHubLocalRunReconcile(ctx context.Context, services *app.LifecycleServices, target taskHubResolvedTarget) (TaskHubPlanPreview, error) {
	if target.run == nil {
		return unavailableRunControlPlan(TaskHubRunControlReconcile, "本地 reconcile 需要明确的 Run"), nil
	}
	if services == nil || services.LocalRuntime == nil {
		return unavailableRunControlPlan(TaskHubRunControlReconcile, "本地 durable runtime 服务不可用"), nil
	}
	if reason := taskHubRunControlTargetReason(TaskHubRunControlReconcile, *target.run, nil); reason != "" {
		return unavailableRunControlPlan(TaskHubRunControlReconcile, reason), nil
	}
	attachment, err := services.LocalRuntime.AttachRun(ctx, app.AttachRunRequest{RunID: target.run.ID})
	if err != nil {
		return TaskHubPlanPreview{}, fmt.Errorf("read local runtime attachment for Run %s reconciliation preview: %w", target.run.ID, err)
	}
	return TaskHubPlanPreview{
		Title:   "本地 reconcile 影响预览",
		Summary: "确认后将调用与 harbor run reconcile 相同的本地 durable-state recovery；仅处理所选 Run，不调用 provider、模型、Docker 或重跑 workflow。",
		Reason: fmt.Sprintf(
			"Run 当前处于 %s；当前本地投影包含 %d 个 job、%d 个 worker lease、%d 个 worker handoff、%d 个 task quota lease 和 %d 个 actor quota lease。过期事实会在提交时重新判定。",
			target.run.Status, len(attachment.Jobs), len(attachment.WorkerLeases), len(attachment.WorkerHandoffs), len(attachment.TaskQuota.Leases), len(attachment.ActorQuota.Leases),
		),
		RevisionImpact:     "本次操作不会修改 TaskRevision，也不会猜测或覆盖外部 provider 结果。",
		ExecutionScope:     []string{target.task.ID, target.run.ID},
		ExternalEffects:    taskHubRunControlPotentialEffects(TaskHubRunControlReconcile),
		ConfirmationNeeded: true,
	}, nil
}

// PrepareTaskHubMutation freezes actions that require a durable plan before
// execution. Continuation and manual patch both retain the exact operator
// provenance and checkpoint that were reviewed in the confirmation form.
func (adapter *AppTaskHubLifecycleAdapter) PrepareTaskHubMutation(ctx context.Context, request TaskHubMutationRequest) (TaskHubPreparedMutation, error) {
	services, err := adapter.lifecycleServices()
	if err != nil {
		return TaskHubPreparedMutation{}, err
	}
	if err := validateTaskHubMutationRequest(request); err != nil {
		return TaskHubPreparedMutation{}, err
	}
	switch request.Action {
	case TaskHubActionStartRun:
		if services.Mutations == nil {
			return TaskHubPreparedMutation{}, fmt.Errorf("LifecycleMutationService is not configured")
		}
		prepared, err := services.Mutations.PrepareStartRun(ctx, app.StartRunLifecycleCommand{
			LifecycleMutationCommandBase: taskHubLifecycleMutationBase(request),
			ProfilePath:                  taskHubMutationValue(request, taskHubExecutionProfilePathField),
			ExecutionSpecPath:            taskHubMutationValue(request, taskHubExecutionSpecPathField),
			Trigger:                      taskHubMutationValue(request, taskHubRunTriggerField),
		})
		if err != nil {
			return TaskHubPreparedMutation{}, err
		}
		return TaskHubPreparedMutation{
			Preview: TaskHubPlanPreview{
				PlanID:             "run-start:" + request.IdempotencyKey,
				Title:              "启动新 Run（输入已冻结）",
				Summary:            "execution profile 与 execution specification 已 canonicalize 并迁入受管目录；再次确认将创建唯一的 durable Run。",
				Reason:             "profile " + prepared.ProfileFingerprint + "；execution spec " + prepared.ExecutionSpecFingerprint + "。",
				RevisionImpact:     "不会修改 TaskRevision 内容。",
				ExecutionScope:     []string{request.Target.TaskID, request.Target.RevisionID},
				ExternalEffects:    []string{"第二次确认将创建 Run、durable job 与 outbox 记录"},
				ConfirmationNeeded: true,
				Expected:           request.Expected,
			},
			Actor:  request.Actor,
			Reason: request.Reason,
		}, nil
	case TaskHubActionEvaluateCodeEdge:
		if services.EvaluatorLaunches == nil {
			return TaskHubPreparedMutation{}, fmt.Errorf("CodeEdge evaluator launch service is not configured")
		}
		prepared, err := services.EvaluatorLaunches.Prepare(ctx, app.CodeEdgeEvaluatorLaunchCommand{
			LifecycleMutationCommandBase: taskHubLifecycleMutationBase(request),
			ParentRunID:                  strings.TrimSpace(request.Target.RunID),
		})
		if err != nil {
			return TaskHubPreparedMutation{}, taskHubCodeEdgeEvaluatorMutationError(err)
		}
		return TaskHubPreparedMutation{
			Preview: TaskHubPlanPreview{
				PlanID:             "codeedge-evaluator:" + request.IdempotencyKey,
				Title:              "执行 CodeEdge 评测（说明书已冻结）",
				Summary:            "Qwen 与 Opus 的严格 child Run profile/spec 已迁入受管目录；再次确认将创建 Run 并启动受控 worker。",
				Reason:             "profile " + prepared.ProfileFingerprint + "；execution spec " + prepared.ExecutionSpecFingerprint + "。",
				RevisionImpact:     "不会修改 TaskRevision；新的 child Run 绑定父 Run " + prepared.ParentRunID + "。",
				ExecutionScope:     []string{prepared.ParentRunID, request.Target.TaskID, request.Target.RevisionID},
				BudgetImpact:       "Qwen 4 个逻辑 Trial，随后 Opus 4 个逻辑 Trial。",
				ExternalEffects:    []string{"第二次确认将创建 child Run、durable job 和一个受控 child-worker handoff"},
				ConfirmationNeeded: true,
				Expected:           request.Expected,
			},
			Actor:  request.Actor,
			Reason: request.Reason,
		}, nil
	case TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff:
		if services.Mutations == nil {
			return TaskHubPreparedMutation{}, fmt.Errorf("LifecycleMutationService is not configured")
		}
		if services.EvaluatorEvidenceHandoffs == nil {
			return TaskHubPreparedMutation{}, fmt.Errorf("CodeEdge evaluator evidence handoff service is not configured")
		}
		parent, child, err := adapter.resolveTaskHubCodeEdgeEvaluatorEvidenceHandoffRuns(ctx, services, request.Target.RunID)
		if err != nil {
			return TaskHubPreparedMutation{}, errors.New(taskHubCodeEdgeEvaluatorEvidenceHandoffUnavailableReason(err))
		}
		prepared, err := services.Mutations.PrepareCodeEdgeEvaluatorEvidenceHandoff(ctx, app.CodeEdgeEvaluatorEvidenceHandoffCommand{
			LifecycleMutationCommandBase: taskHubLifecycleMutationBase(request),
			ParentRunID:                  parent.ID,
			ChildRunID:                   child.ID,
		})
		if err != nil {
			return TaskHubPreparedMutation{}, errors.New(taskHubCodeEdgeEvaluatorEvidenceHandoffUnavailableReason(err))
		}
		if prepared.ParentRunID != parent.ID || prepared.ChildRunID != child.ID || strings.TrimSpace(string(prepared.HandoffFingerprint)) == "" {
			return TaskHubPreparedMutation{}, errors.New("CodeEdge evaluator 证据交接冻结结果未绑定所选 parent/child 证据")
		}
		return TaskHubPreparedMutation{
			Preview: TaskHubPlanPreview{
				PlanID:             "codeedge-evaluator-evidence-handoff:" + request.IdempotencyKey,
				Title:              "采用 CodeEdge 评测证据（交接已冻结）",
				Summary:            "已冻结 parent/child 身份和已验证的 Qwen/Opus pass@4 证据指纹；再次确认才会写入不可变证据交接。",
				Reason:             "交接指纹 " + string(prepared.HandoffFingerprint) + "。",
				RevisionImpact:     "不会修改 TaskRevision；仅在已冻结 Phase-1 父 Run 上采用这份 child 证据。",
				ExecutionScope:     []string{prepared.ParentRunID, prepared.ChildRunID},
				ReusedEvidence:     []string{string(prepared.QwenTrialFingerprint), string(prepared.OpusTrialFingerprint)},
				ConfirmationNeeded: true,
				Expected:           request.Expected,
			},
			Actor:  request.Actor,
			Reason: request.Reason,
		}, nil
	case TaskHubActionContinue:
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
	case TaskHubActionEditTask:
		if services.Mutations == nil {
			return TaskHubPreparedMutation{}, fmt.Errorf("LifecycleMutationService is not configured")
		}
		if strings.TrimSpace(request.PlanID) != "" {
			return TaskHubPreparedMutation{}, fmt.Errorf("手工修改计划已经冻结")
		}
		receipt, err := services.Mutations.PrepareManualPatch(ctx, app.EditLifecycleCommand{
			LifecycleMutationCommandBase: taskHubLifecycleMutationBase(request),
			UnifiedDiff:                  taskHubMutationValue(request, taskHubUnifiedDiffField),
		})
		if err != nil {
			return TaskHubPreparedMutation{}, err
		}
		if strings.TrimSpace(receipt.PlanID) == "" || services.Continuations == nil {
			return TaskHubPreparedMutation{}, fmt.Errorf("手工修改未返回可执行的冻结计划")
		}
		plan, err := services.Continuations.GetTaskContinuationPlan(ctx, receipt.PlanID)
		if err != nil {
			return TaskHubPreparedMutation{}, err
		}
		preview := taskHubContinuationPlanPreview(plan)
		preview.Title = "手工修改计划（已冻结）"
		preview.Summary = "已在隔离候选快照中准备 unified diff；再次确认后才会提交候选 revision。"
		preview.Expected = request.Expected
		return TaskHubPreparedMutation{Preview: preview, Actor: request.Actor, Reason: request.Reason}, nil
	default:
		return TaskHubPreparedMutation{}, fmt.Errorf("Task Hub action %q does not have a separate planning phase", request.Action)
	}
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
	case TaskHubActionNewTask:
		if services.Mutations == nil {
			return TaskHubMutationResult{}, fmt.Errorf("LifecycleMutationService is not configured")
		}
		receipt, err := services.Mutations.CreateDraft(ctx, app.CreateDraftLifecycleCommand{
			LifecycleMutationCommandBase: taskHubLifecycleMutationBase(request),
			Slug:                         taskHubMutationValue(request, taskHubTaskSlugField),
			Title:                        taskHubMutationValue(request, taskHubTaskTitleField),
			MetadataJSON:                 taskHubMutationValue(request, taskHubTaskMetadataJSONField),
			SourceRepo:                   taskHubMutationValue(request, taskHubTaskSourceRepoField),
			SourceCommit:                 taskHubMutationValue(request, taskHubTaskSourceCommitField),
		})
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		return taskHubMutationResult(request, receipt), nil
	case TaskHubActionStartStandardAuthoring:
		if request.Target != (TaskHubTarget{}) {
			return TaskHubMutationResult{}, fmt.Errorf("Standard authoring launch is global and cannot target an existing Task, Run, or revision")
		}
		if services.AuthoringLaunches == nil || !services.AuthoringLaunches.Available() {
			return TaskHubMutationResult{}, fmt.Errorf("controlled Standard authoring launch service is not configured")
		}
		receipt, err := services.AuthoringLaunches.Start(ctx, app.StandardAuthoringLaunchCommand{
			LifecycleMutationCommandBase: taskHubLifecycleMutationBase(request),
			Slug:                         taskHubMutationValue(request, taskHubTaskSlugField),
			Title:                        taskHubMutationValue(request, taskHubTaskTitleField),
			MetadataJSON:                 taskHubMutationValue(request, taskHubTaskMetadataJSONField),
		})
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		return taskHubMutationResult(request, receipt), nil
	case TaskHubActionImportTask:
		if services.Mutations == nil {
			return TaskHubMutationResult{}, fmt.Errorf("LifecycleMutationService is not configured")
		}
		receipt, err := services.Mutations.Import(ctx, app.ImportLifecycleCommand{
			LifecycleMutationCommandBase: taskHubLifecycleMutationBase(request),
			Slug:                         taskHubMutationValue(request, taskHubTaskSlugField),
			Title:                        taskHubMutationValue(request, taskHubTaskTitleField),
			MetadataJSON:                 taskHubMutationValue(request, taskHubTaskMetadataJSONField),
			SourceRepo:                   taskHubMutationValue(request, taskHubTaskSourceRepoField),
			SourceCommit:                 taskHubMutationValue(request, taskHubTaskSourceCommitField),
			SourcePath:                   taskHubMutationValue(request, taskHubImportSourcePathField),
			ProposalDigest:               taskHubMutationValue(request, taskHubImportProposalDigestField),
			ChangeSummary:                taskHubMutationValue(request, taskHubImportChangeSummaryField),
		})
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		return taskHubMutationResult(request, receipt), nil
	case TaskHubActionForkTask:
		if services.Mutations == nil {
			return TaskHubMutationResult{}, fmt.Errorf("LifecycleMutationService is not configured")
		}
		receipt, err := services.Mutations.Fork(ctx, app.ForkLifecycleCommand{
			LifecycleMutationCommandBase: taskHubLifecycleMutationBase(request),
			Slug:                         taskHubMutationValue(request, taskHubTaskSlugField),
			Title:                        taskHubMutationValue(request, taskHubTaskTitleField),
			MetadataJSON:                 taskHubMutationValue(request, taskHubTaskMetadataJSONField),
		})
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		return taskHubMutationResult(request, receipt), nil
	case TaskHubActionArchiveTask:
		if services.Mutations == nil {
			return TaskHubMutationResult{}, fmt.Errorf("LifecycleMutationService is not configured")
		}
		receipt, err := services.Mutations.Archive(ctx, taskHubLifecycleMutationBase(request))
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		return taskHubMutationResult(request, receipt), nil
	case TaskHubActionSoftDeleteTask:
		if services.Mutations == nil {
			return TaskHubMutationResult{}, fmt.Errorf("LifecycleMutationService is not configured")
		}
		receipt, err := services.Mutations.SoftDelete(ctx, taskHubLifecycleMutationBase(request))
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		return taskHubMutationResult(request, receipt), nil
	case TaskHubActionRestoreTask:
		if services.Mutations == nil {
			return TaskHubMutationResult{}, fmt.Errorf("LifecycleMutationService is not configured")
		}
		receipt, err := services.Mutations.Restore(ctx, app.RestoreLifecycleCommand{
			LifecycleMutationCommandBase: taskHubLifecycleMutationBase(request),
			RestoreState:                 store.TaskLifecycleState(taskHubMutationValue(request, taskHubRestoreStateField)),
		})
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		return taskHubMutationResult(request, receipt), nil
	case TaskHubActionStartRun:
		if services.Mutations == nil {
			return TaskHubMutationResult{}, fmt.Errorf("LifecycleMutationService is not configured")
		}
		receipt, err := services.Mutations.StartRun(ctx, app.StartRunLifecycleCommand{
			LifecycleMutationCommandBase: taskHubLifecycleMutationBase(request),
			ProfilePath:                  taskHubMutationValue(request, taskHubExecutionProfilePathField),
			ExecutionSpecPath:            taskHubMutationValue(request, taskHubExecutionSpecPathField),
			Trigger:                      taskHubMutationValue(request, taskHubRunTriggerField),
		})
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		return taskHubMutationResult(request, receipt), nil
	case TaskHubActionEvaluateCodeEdge:
		if services.EvaluatorLaunches == nil {
			return TaskHubMutationResult{}, fmt.Errorf("CodeEdge evaluator launch service is not configured")
		}
		if adapter == nil || adapter.runWorkerHandoffLauncher == nil {
			return TaskHubMutationResult{}, fmt.Errorf("Task Hub CodeEdge evaluator worker launcher is not configured")
		}
		result, err := services.EvaluatorLaunches.ConfirmAndLaunch(ctx, app.CodeEdgeEvaluatorLaunchCommand{
			LifecycleMutationCommandBase: taskHubLifecycleMutationBase(request),
			ParentRunID:                  strings.TrimSpace(request.Target.RunID),
		}, adapter.runWorkerHandoffLauncher)
		if err != nil {
			return TaskHubMutationResult{}, taskHubCodeEdgeEvaluatorMutationError(err)
		}
		return TaskHubMutationResult{
			Action:      request.Action,
			Target:      request.Target,
			PlanID:      request.PlanID,
			ExecutionID: result.Receipt.RunID,
			ReceiptID:   result.Handoff.ID,
			Summary:     "CodeEdge 评测 child Run 已入队并启动受控 worker",
		}, nil
	case TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff:
		if services.Mutations == nil {
			return TaskHubMutationResult{}, fmt.Errorf("LifecycleMutationService is not configured")
		}
		parent, child, err := adapter.resolveTaskHubCodeEdgeEvaluatorEvidenceHandoffRuns(ctx, services, request.Target.RunID)
		if err != nil {
			return TaskHubMutationResult{}, errors.New(taskHubCodeEdgeEvaluatorEvidenceHandoffUnavailableReason(err))
		}
		receipt, err := services.Mutations.AdoptCodeEdgeEvaluatorEvidenceHandoff(ctx, app.CodeEdgeEvaluatorEvidenceHandoffCommand{
			LifecycleMutationCommandBase: taskHubLifecycleMutationBase(request),
			ParentRunID:                  parent.ID,
			ChildRunID:                   child.ID,
		})
		if err != nil {
			return TaskHubMutationResult{}, errors.New(taskHubCodeEdgeEvaluatorEvidenceHandoffUnavailableReason(err))
		}
		return taskHubMutationResult(request, receipt), nil
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
	case TaskHubActionEditTask:
		if services.Mutations == nil {
			return TaskHubMutationResult{}, fmt.Errorf("LifecycleMutationService is not configured")
		}
		if strings.TrimSpace(request.PlanID) == "" {
			return TaskHubMutationResult{}, fmt.Errorf("手工修改必须先冻结候选变更计划")
		}
		receipt, err := services.Mutations.ExecuteManualPatch(ctx, taskHubLifecycleMutationBase(request), request.PlanID)
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		return taskHubMutationResult(request, receipt), nil
	case TaskHubActionApproveReview, TaskHubActionRequestChanges, TaskHubActionRejectReview:
		if strings.TrimSpace(request.Target.AuthoringReviewRequestID) != "" {
			if services.AuthoringReviews == nil {
				return TaskHubMutationResult{}, fmt.Errorf("authoring review service is not configured")
			}
			if strings.TrimSpace(request.Target.AuthoringReviewRequestID) != strings.TrimSpace(request.AuthoringReviewExpected.ReviewRequestID) ||
				strings.TrimSpace(request.Target.RunID) != strings.TrimSpace(request.AuthoringReviewExpected.RunID) {
				return TaskHubMutationResult{}, fmt.Errorf("authoring review confirmation target differs from its frozen source/session checkpoint")
			}
			result, err := services.AuthoringReviews.Decide(ctx, app.DecideAuthoringReviewRequest{
				IdempotencyKey: request.IdempotencyKey, Action: taskHubReviewDecisionAction(request.Action), Actor: request.Actor, Reason: request.Reason,
				Expected: appAuthoringReviewCheckpoint(request.AuthoringReviewExpected),
			})
			if err != nil {
				return TaskHubMutationResult{}, err
			}
			return TaskHubMutationResult{
				Action: request.Action, Target: request.Target, ReceiptID: result.Decision.ID, ExecutionID: result.ResolutionJob.ID,
				Summary: "已记录 source/session 审核决定，并排队本地受控 resolution job",
			}, nil
		}
		if services.Mutations == nil {
			return TaskHubMutationResult{}, fmt.Errorf("LifecycleMutationService is not configured")
		}
		receipt, err := services.Mutations.DecideReview(ctx, app.DecideReviewLifecycleCommand{
			LifecycleMutationCommandBase: taskHubLifecycleMutationBase(request),
			Decision:                     taskHubReviewDecisionAction(request.Action),
		})
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		return taskHubMutationResult(request, receipt), nil
	case TaskHubActionPackageRevision:
		if services.Mutations == nil {
			return TaskHubMutationResult{}, fmt.Errorf("LifecycleMutationService is not configured")
		}
		receipt, err := services.Mutations.Package(ctx, app.PackageLifecycleCommand{
			LifecycleMutationCommandBase: taskHubLifecycleMutationBase(request),
			ReleaseVersion:               taskHubMutationValue(request, taskHubPackageVersionField),
		})
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		return taskHubMutationResult(request, receipt), nil
	case TaskHubActionWithdrawRelease:
		if services.Mutations == nil {
			return TaskHubMutationResult{}, fmt.Errorf("LifecycleMutationService is not configured")
		}
		receipt, err := services.Mutations.Withdraw(ctx, taskHubLifecycleMutationBase(request))
		if err != nil {
			return TaskHubMutationResult{}, err
		}
		return taskHubMutationResult(request, receipt), nil
	default:
		return TaskHubMutationResult{}, fmt.Errorf("Task Hub action %q has no confirmed idempotent execution contract", request.Action)
	}
}

// ExecuteTaskHubRunControlMutation executes one confirmed, target-scoped Run
// action. Durable control operations use exactly the checkpoint the operator
// inspected. Local reconcile deliberately mirrors the CLI contract instead:
// it invokes LocalRuntime.ReconcileRun with the explicit Run, actor, and audit
// reason, without creating a ControlOperation or contacting a provider.
func (adapter *AppTaskHubLifecycleAdapter) ExecuteTaskHubRunControlMutation(ctx context.Context, request TaskHubRunControlMutationRequest) (TaskHubRunControlMutationResult, error) {
	if err := validateTaskHubRunControlMutationRequest(request); err != nil {
		return TaskHubRunControlMutationResult{}, err
	}
	services, err := adapter.lifecycleServices()
	if err != nil {
		return TaskHubRunControlMutationResult{}, err
	}
	target, err := adapter.resolveTaskHubTarget(ctx, services, request.Target)
	if err != nil {
		return TaskHubRunControlMutationResult{}, err
	}
	if target.run == nil {
		return TaskHubRunControlMutationResult{}, fmt.Errorf("运行控制需要明确的 Run")
	}
	if request.Action == TaskHubRunControlReconcile {
		if services.LocalRuntime == nil {
			return TaskHubRunControlMutationResult{}, fmt.Errorf("local durable runtime service is not configured")
		}
		if reason := taskHubRunControlTargetReason(TaskHubRunControlReconcile, *target.run, nil); reason != "" {
			return TaskHubRunControlMutationResult{}, fmt.Errorf("本地 reconcile 不可提交：%s", reason)
		}
		result, err := services.LocalRuntime.ReconcileRun(ctx, app.ReconcileRunRequest{
			RunID:  target.run.ID,
			Actor:  request.Actor,
			Reason: request.Reason,
		})
		if err != nil {
			return TaskHubRunControlMutationResult{}, err
		}
		return TaskHubRunControlMutationResult{
			Action:  request.Action,
			Summary: taskHubLocalRunReconcileSummary(result),
		}, nil
	}
	if services.Control == nil {
		return TaskHubRunControlMutationResult{}, fmt.Errorf("execution control service is not configured")
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

func taskHubLocalRunReconcileSummary(result app.RunReconciliationResult) string {
	return fmt.Sprintf(
		"本地 reconcile 已完成：恢复 %d 个过期 job，过期 job lease %d、worker lease %d、worker handoff %d、task quota %d、actor quota %d；未调用外部 provider 或重跑 workflow。",
		len(result.RecoveredJobs), result.ExpiredJobLeases, result.ExpiredWorkerLeases, len(result.ExpiredWorkerHandoffs), result.ExpiredTaskQuotas, result.ExpiredActorQuotas,
	)
}

// ExecuteTaskHubRunHandoff transfers exactly one selected Run to the
// application handoff service. It does not inspect SQLite or spawn processes
// itself; the injected launcher remains behind RunWorkerHandoffService's
// reserve-launch-receipt protocol.
func (adapter *AppTaskHubLifecycleAdapter) ExecuteTaskHubRunHandoff(ctx context.Context, request TaskHubRunHandoffRequest) (TaskHubRunHandoffResult, error) {
	if err := validateTaskHubRunHandoffRequest(request); err != nil {
		return TaskHubRunHandoffResult{}, err
	}
	services, err := adapter.lifecycleServices()
	if err != nil {
		return TaskHubRunHandoffResult{}, err
	}
	if services.WorkerHandoffs == nil {
		return TaskHubRunHandoffResult{}, fmt.Errorf("run-worker handoff service is not configured")
	}
	if adapter == nil || adapter.runWorkerHandoffLauncher == nil {
		return TaskHubRunHandoffResult{}, fmt.Errorf("Task Hub controlled child-worker launcher is not configured")
	}
	handoff, err := services.WorkerHandoffs.LaunchRunWorkerHandoff(ctx, app.ReserveRunWorkerHandoffCommand{
		ID:             strings.TrimSpace(request.HandoffOperationID),
		IdempotencyKey: strings.TrimSpace(request.IdempotencyKey),
		RunID:          strings.TrimSpace(request.RunID),
		Expected: app.RunWorkerHandoffCheckpoint{
			RunVersion:     request.Expected.RunVersion,
			ExecutionEpoch: request.Expected.ExecutionEpoch,
			DefinitionHash: strings.TrimSpace(request.Expected.DefinitionHash),
		},
		Owner:  strings.TrimSpace(request.Owner),
		Actor:  strings.TrimSpace(request.Actor),
		Reason: strings.TrimSpace(request.Reason),
	}, adapter.runWorkerHandoffLauncher)
	if err != nil {
		return TaskHubRunHandoffResult{}, err
	}
	result := TaskHubRunHandoffResult{
		RunID:       handoff.RunID,
		OperationID: handoff.ID,
		State:       string(handoff.State),
		Summary:     "已提交受控 child-worker handoff",
	}
	switch handoff.State {
	case store.RunWorkerHandoffLaunching, store.RunWorkerHandoffHandedOff, store.RunWorkerHandoffReleased:
		return result, nil
	case store.RunWorkerHandoffFailed:
		failure := strings.TrimSpace(handoff.FailureReason)
		if failure == "" {
			failure = "controlled child worker launch failed"
		}
		return result, fmt.Errorf("run-worker handoff %s previously failed: %s", handoff.ID, failure)
	case store.RunWorkerHandoffExpired:
		return result, fmt.Errorf("run-worker handoff %s expired before a controlled worker was confirmed", handoff.ID)
	default:
		return result, fmt.Errorf("run-worker handoff %s returned unsupported state %s", handoff.ID, handoff.State)
	}
}

func validateTaskHubRunHandoffRequest(request TaskHubRunHandoffRequest) error {
	if err := store.ValidateUUIDv7(strings.TrimSpace(request.RunID)); err != nil {
		return fmt.Errorf("Task Hub run-worker handoff run ID: %w", err)
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(request.HandoffOperationID)); err != nil {
		return fmt.Errorf("Task Hub run-worker handoff operation ID: %w", err)
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(request.IdempotencyKey)); err != nil {
		return fmt.Errorf("Task Hub run-worker handoff idempotency key: %w", err)
	}
	if request.Expected.RunVersion <= 0 || request.Expected.ExecutionEpoch < 0 || strings.TrimSpace(request.Expected.DefinitionHash) == "" {
		return fmt.Errorf("Task Hub run-worker handoff Run checkpoint is required")
	}
	for _, field := range []struct {
		label string
		value string
	}{
		{label: "owner", value: request.Owner},
		{label: "actor", value: request.Actor},
		{label: "reason", value: request.Reason},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("Task Hub run-worker handoff %s is required", field.label)
		}
	}
	return nil
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
	if services == nil || services.LocalRuntime == nil {
		return nil, nil, fmt.Errorf("local durable runtime service is not configured")
	}
	attachment, err := services.LocalRuntime.AttachRun(ctx, app.AttachRunRequest{RunID: runID})
	if err != nil {
		return nil, nil, err
	}
	for _, attachedJob := range attachment.Jobs {
		if !attachedJob.Attachable {
			continue
		}
		for _, attachedLease := range attachedJob.Leases {
			lease := attachedLease.Lease
			if !attachedLease.Valid || lease.ResourceType != "job_dispatch" || lease.ResourceID != attachedJob.Job.ID {
				continue
			}
			job := attachedJob.Job
			return &job, &lease, nil
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
	return validateTaskHubMutationValues(request)
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
	if snapshot.Strategy == workflowkit.StrategyReviseSubject && snapshot.TargetRunRelation == workflowkit.RelationChildRun {
		return "计划包含候选主体；仅在最终执行 CAS 成功时提交新版本。"
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

func (adapter *AppTaskHubLifecycleAdapter) captureTaskHubMutationCheckpoint(ctx context.Context, services *app.LifecycleServices, taskID, revisionID, runID, releaseID string) (TaskHubLifecycleCheckpoint, error) {
	if services == nil || services.Mutations == nil {
		return TaskHubLifecycleCheckpoint{}, fmt.Errorf("LifecycleMutationService is not configured")
	}
	checkpoint, err := services.Mutations.CaptureCheckpoint(ctx, taskID, revisionID, runID, releaseID)
	if err != nil {
		return TaskHubLifecycleCheckpoint{}, err
	}
	return taskHubLifecycleCheckpoint(checkpoint), nil
}

func (adapter *AppTaskHubLifecycleAdapter) captureTaskHubReviewCheckpoint(ctx context.Context, services *app.LifecycleServices, taskID, revisionID, reviewRequestID string) (TaskHubLifecycleCheckpoint, error) {
	if services == nil || services.Mutations == nil {
		return TaskHubLifecycleCheckpoint{}, fmt.Errorf("LifecycleMutationService is not configured")
	}
	checkpoint, err := services.Mutations.CaptureReviewCheckpoint(ctx, taskID, revisionID, reviewRequestID)
	if err != nil {
		return TaskHubLifecycleCheckpoint{}, err
	}
	return taskHubLifecycleCheckpoint(checkpoint), nil
}

func taskHubLifecycleCheckpoint(checkpoint app.LifecycleMutationCheckpoint) TaskHubLifecycleCheckpoint {
	return TaskHubLifecycleCheckpoint{
		TaskID:                           checkpoint.TaskID,
		TaskVersion:                      checkpoint.TaskVersion,
		RevisionID:                       checkpoint.RevisionID,
		RevisionStateVersion:             checkpoint.RevisionStateVersion,
		RevisionDigest:                   checkpoint.RevisionDigest,
		RunID:                            checkpoint.RunID,
		RunVersion:                       checkpoint.RunVersion,
		RunExecutionEpoch:                checkpoint.RunExecutionEpoch,
		RunDefinitionHash:                checkpoint.RunDefinitionHash,
		CodeEdgeComplianceRecordID:       checkpoint.CodeEdgeComplianceRecordID,
		CodeEdgeAuthorizationFingerprint: checkpoint.CodeEdgeAuthorizationFingerprint,
		ReleaseID:                        checkpoint.ReleaseID,
		ReleaseRecordVersion:             checkpoint.ReleaseRecordVersion,
		ReviewRequestID:                  checkpoint.ReviewRequestID,
		ReviewRevisionID:                 checkpoint.ReviewRevisionID,
		ReviewState:                      checkpoint.ReviewState,
		ReviewEvidenceDigest:             checkpoint.ReviewEvidenceDigest,
	}
}

func taskHubAuthoringReviewCheckpoint(checkpoint app.AuthoringReviewCheckpoint) TaskHubAuthoringReviewCheckpoint {
	return TaskHubAuthoringReviewCheckpoint{
		ReviewRequestID: checkpoint.ReviewRequestID, BindingID: checkpoint.BindingID, RunID: checkpoint.RunID,
		AuthoringSessionID: checkpoint.AuthoringSessionID, AuthoringSourceID: checkpoint.AuthoringSourceID,
		SourceSnapshotDigest: checkpoint.SourceSnapshotDigest, DefinitionHash: checkpoint.DefinitionHash,
		StageAttemptID: checkpoint.StageAttemptID, InputFingerprint: checkpoint.InputFingerprint,
		EvidenceManifestDigest: checkpoint.EvidenceManifestDigest, RunVersion: checkpoint.RunVersion,
		StageAttemptVersion: checkpoint.StageAttemptVersion,
	}
}

func appAuthoringReviewCheckpoint(checkpoint TaskHubAuthoringReviewCheckpoint) app.AuthoringReviewCheckpoint {
	return app.AuthoringReviewCheckpoint{
		ReviewRequestID: strings.TrimSpace(checkpoint.ReviewRequestID), BindingID: strings.TrimSpace(checkpoint.BindingID),
		RunID: strings.TrimSpace(checkpoint.RunID), AuthoringSessionID: strings.TrimSpace(checkpoint.AuthoringSessionID),
		AuthoringSourceID: strings.TrimSpace(checkpoint.AuthoringSourceID), SourceSnapshotDigest: strings.TrimSpace(checkpoint.SourceSnapshotDigest),
		DefinitionHash: strings.TrimSpace(checkpoint.DefinitionHash), StageAttemptID: strings.TrimSpace(checkpoint.StageAttemptID),
		InputFingerprint: strings.TrimSpace(checkpoint.InputFingerprint), EvidenceManifestDigest: strings.TrimSpace(checkpoint.EvidenceManifestDigest),
		RunVersion: checkpoint.RunVersion, StageAttemptVersion: checkpoint.StageAttemptVersion,
	}
}

func appLifecycleMutationCheckpoint(checkpoint TaskHubLifecycleCheckpoint) app.LifecycleMutationCheckpoint {
	return app.LifecycleMutationCheckpoint{
		TaskID:                           strings.TrimSpace(checkpoint.TaskID),
		TaskVersion:                      checkpoint.TaskVersion,
		RevisionID:                       strings.TrimSpace(checkpoint.RevisionID),
		RevisionStateVersion:             checkpoint.RevisionStateVersion,
		RevisionDigest:                   strings.TrimSpace(checkpoint.RevisionDigest),
		RunID:                            strings.TrimSpace(checkpoint.RunID),
		RunVersion:                       checkpoint.RunVersion,
		RunExecutionEpoch:                checkpoint.RunExecutionEpoch,
		RunDefinitionHash:                strings.TrimSpace(checkpoint.RunDefinitionHash),
		CodeEdgeComplianceRecordID:       strings.TrimSpace(checkpoint.CodeEdgeComplianceRecordID),
		CodeEdgeAuthorizationFingerprint: strings.TrimSpace(checkpoint.CodeEdgeAuthorizationFingerprint),
		ReleaseID:                        strings.TrimSpace(checkpoint.ReleaseID),
		ReleaseRecordVersion:             checkpoint.ReleaseRecordVersion,
		ReviewRequestID:                  strings.TrimSpace(checkpoint.ReviewRequestID),
		ReviewRevisionID:                 strings.TrimSpace(checkpoint.ReviewRevisionID),
		ReviewState:                      strings.TrimSpace(checkpoint.ReviewState),
		ReviewEvidenceDigest:             strings.TrimSpace(checkpoint.ReviewEvidenceDigest),
	}
}

func taskHubLifecycleMutationBase(request TaskHubMutationRequest) app.LifecycleMutationCommandBase {
	return app.LifecycleMutationCommandBase{
		IdempotencyKey: strings.TrimSpace(request.IdempotencyKey),
		Actor:          strings.TrimSpace(request.Actor),
		Reason:         strings.TrimSpace(request.Reason),
		Expected:       appLifecycleMutationCheckpoint(request.Expected),
	}
}

func taskHubMutationValue(request TaskHubMutationRequest, field string) string {
	return strings.TrimSpace(request.Values[field])
}

func taskHubMutationResult(request TaskHubMutationRequest, receipt app.LifecycleMutationReceipt) TaskHubMutationResult {
	summary := strings.TrimSpace(receipt.Summary)
	if summary == "" {
		summary = taskHubActionLabel(request.Action) + "已提交"
	}
	executionID := strings.TrimSpace(receipt.ExecutionID)
	if executionID == "" {
		executionID = strings.TrimSpace(receipt.RunID)
	}
	return TaskHubMutationResult{
		Action:      request.Action,
		Target:      request.Target,
		PlanID:      receipt.PlanID,
		ExecutionID: executionID,
		ReceiptID:   receipt.OperationID,
		Summary:     summary,
	}
}

func taskHubTaskTransitionPreview(action TaskHubAction, task store.TaskV2, expected TaskHubLifecycleCheckpoint) TaskHubPlanPreview {
	preview := TaskHubPlanPreview{
		Title:              taskHubActionLabel(action),
		ExecutionScope:     []string{task.ID},
		ConfirmationNeeded: true,
		Expected:           expected,
	}
	switch action {
	case TaskHubActionArchiveTask:
		preview.Summary = "确认后由 V12 幂等命令将 published Task 归档；revision、package 与证据不会删除。"
		preview.Reason = "Task 当前处于 published，已捕获完整 Task CAS checkpoint。"
		preview.RevisionImpact = "TaskRevision 内容与 digest 不变；Task lifecycle 将变为 archived。"
	case TaskHubActionSoftDeleteTask:
		preview.Summary = "确认后由 V12 幂等命令写入可恢复 deletion record 并将 Task 置为 deleted。"
		preview.Reason = "已捕获完整 Task CAS checkpoint；提交会拒绝已变化的 Task。"
		preview.RevisionImpact = "TaskRevision、package 与证据保留；Task lifecycle 将变为 deleted。"
	case TaskHubActionRestoreTask:
		preview.Summary = "确认表单将要求明确选择恢复后的 lifecycle 状态；提交由 V12 幂等命令完成。"
		preview.Reason = "Task 当前处于 deleted，已捕获完整 Task CAS checkpoint。"
		preview.RevisionImpact = "TaskRevision、package 与证据保持不变；Task lifecycle 将恢复到所选状态。"
	}
	return preview
}

// openTaskHubReview resolves a specific open review. The adapter deliberately
// refuses to choose between multiple open reviews: the projection must carry a
// stable ReviewRequestID captured when the operator started the action.
func (adapter *AppTaskHubLifecycleAdapter) openTaskHubReview(ctx context.Context, services *app.LifecycleServices, target taskHubResolvedTarget) (store.ReviewRequest, string, error) {
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
			if review.State != "open" {
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

func (adapter *AppTaskHubLifecycleAdapter) planTaskHubAuthoringReview(ctx context.Context, services *app.LifecycleServices, action TaskHubAction, target taskHubResolvedTarget) (TaskHubPlanPreview, error) {
	if services == nil || services.AuthoringReviews == nil {
		return unavailableTaskHubPlan(action, "AuthoringReviewService 不可用"), nil
	}
	review, unavailable, err := adapter.openTaskHubAuthoringReview(ctx, services, target)
	if err != nil {
		return TaskHubPlanPreview{}, err
	}
	if unavailable != "" {
		return unavailableTaskHubPlan(action, unavailable), nil
	}
	checkpoint, err := services.AuthoringReviews.CaptureCheckpoint(ctx, review.Request.ID)
	if err != nil {
		return TaskHubPlanPreview{}, err
	}
	if checkpoint.RunID != review.Binding.RunID || checkpoint.BindingID != review.Binding.ID || checkpoint.StageAttemptID != review.Binding.StageAttemptID {
		return TaskHubPlanPreview{}, fmt.Errorf("authoring review checkpoint differs from inspected source/session gate")
	}
	return TaskHubPlanPreview{
		Title:                   taskHubActionLabel(action),
		Summary:                 "确认后将对冻结的 source/session 审核 gate 记录不可变决定，并排队本地受控 resolution job。",
		Reason:                  "审核请求 " + review.Request.ID + " 当前处于 open 状态，绑定 AuthoringSession " + review.Binding.AuthoringSessionID + "。",
		RevisionImpact:          "不会创建、伪造或修改 TaskRevision；决定只绑定冻结的 source/session、Run 与 StageAttempt。",
		ExecutionScope:          []string{target.task.ID, review.Binding.AuthoringSourceID, review.Binding.AuthoringSessionID, review.Binding.RunID, review.Binding.StageAttemptID, review.Request.ID},
		ConfirmationNeeded:      true,
		AuthoringReviewExpected: taskHubAuthoringReviewCheckpoint(checkpoint),
	}, nil
}

// openTaskHubAuthoringReview resolves only the explicit authoring request
// selected in the detail surface. It never falls back to TaskRevision review
// tables or guesses which source/session gate the operator meant.
func (adapter *AppTaskHubLifecycleAdapter) openTaskHubAuthoringReview(ctx context.Context, services *app.LifecycleServices, target taskHubResolvedTarget) (app.AuthoringReviewGateSnapshot, string, error) {
	if services == nil || services.AuthoringReviews == nil {
		return app.AuthoringReviewGateSnapshot{}, "AuthoringReviewService 不可用", nil
	}
	requestID := strings.TrimSpace(target.authoringReviewRequestID)
	if requestID == "" {
		return app.AuthoringReviewGateSnapshot{}, "请从详情“证据/审核/返修”分类选择明确的 source/session 审核请求", nil
	}
	if target.run == nil || target.run.SubjectKind != store.WorkflowRunSubjectAuthoringSession {
		return app.AuthoringReviewGateSnapshot{}, "source/session 审核需要明确的 AuthoringSession Run", nil
	}
	review, err := services.AuthoringReviews.Inspect(ctx, requestID)
	if err != nil {
		return app.AuthoringReviewGateSnapshot{}, "", err
	}
	if review.Binding.RunID != target.run.ID || review.Run.ID != target.run.ID || review.Request.ID != requestID ||
		review.Run.SubjectKind != store.WorkflowRunSubjectAuthoringSession {
		return app.AuthoringReviewGateSnapshot{}, "指定 source/session 审核请求不属于当前 AuthoringSession Run", nil
	}
	if review.State != store.AuthoringReviewGateOpen {
		return app.AuthoringReviewGateSnapshot{}, "指定 source/session 审核请求已不再处于 open 状态", nil
	}
	return review, "", nil
}

func targetReviewRequestID(target taskHubResolvedTarget) string {
	return target.reviewRequestID
}

func unavailableRunControlPlan(action TaskHubRunControlAction, reason string) TaskHubPlanPreview {
	return TaskHubPlanPreview{
		Title:   taskHubRunControlActionLabel(action) + "（不可提交）",
		Summary: "当前状态下不能生成可提交的 Run 操作；本次调用只读取事实，不会创建 ControlOperation、调用 provider 或重跑 workflow。",
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
	case TaskHubRunControlReconcile:
		if !taskHubRunReconcileEligible(run.Status) {
			return fmt.Sprintf("Run 当前处于 %s；只有 interrupted 或 in_doubt Run 可以执行本地 reconcile", run.Status)
		}
	}
	return ""
}

func taskHubRunReconcileEligible(status store.WorkflowRunStatus) bool {
	switch status {
	case store.WorkflowRunInterrupted, store.WorkflowRunInDoubt:
		return true
	default:
		return false
	}
}

func taskHubRunControlPotentialEffects(action TaskHubRunControlAction) []string {
	switch action {
	case TaskHubRunControlPause:
		return []string{"提交后 worker 将请求 checkpoint；本次预览不会发出该请求"}
	case TaskHubRunControlCancelStage:
		return []string{"提交后仅会影响选中的 StageAttempt；本次预览不会取消阶段"}
	case TaskHubRunControlTerminate:
		return []string{"提交后 worker 会执行目标 scoped 的 graceful termination；本次预览不会终止运行"}
	case TaskHubRunControlReconcile:
		return []string{
			"提交后仅回收所选 Run 的过期本地 durable 状态；本次预览不会修改任何事实。",
			"不会调用 provider、模型或 Docker，也不会重跑 workflow；未知外部副作用仍保持 in_doubt，须由受控 provider-specific reconciler 处理。",
		}
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
	authoringRuns, err := services.Store().ListAuthoringWorkflowRunsForTargetTask(ctx, task.ID)
	if err != nil {
		return TaskHubTask{}, nil, nil, fmt.Errorf("list AuthoringSession Runs for Task %s: %w", task.ID, err)
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
	activeAuthoringReview, err := appTaskHubSingleOpenAuthoringReview(ctx, services, task.ID)
	if err != nil {
		return TaskHubTask{}, nil, nil, err
	}
	if activeReview != nil {
		projection.ActiveReview = "open"
		projection.ActiveReviewID = activeReview.ID
		projection.ActiveReviewRevisionID = activeReview.RevisionID
	}
	if activeReview == nil && activeAuthoringReview != nil {
		projection.ActiveReview = "open"
		projection.ActiveAuthoringReviewID = activeAuthoringReview.Request.ID
		projection.ActiveAuthoringReviewRunID = activeAuthoringReview.Binding.RunID
	}
	codeEdgePackageUnavailable := ""
	if current != nil {
		_, codeEdgePackageUnavailable, err = taskHubCodeEdgePackageAuthorization(ctx, services, task, *current, runs, "")
		if err != nil {
			return TaskHubTask{}, nil, nil, fmt.Errorf("read CodeEdge package authorization for Task %s: %w", task.ID, err)
		}
	}
	projection.Actions = appTaskHubTaskActions(task, current, activeReview != nil || activeAuthoringReview != nil, services.Mutations != nil || services.AuthoringReviews != nil, codeEdgePackageUnavailable)

	displayRuns := make([]store.WorkflowRun, 0, len(runs)+len(authoringRuns))
	displayRuns = append(displayRuns, runs...)
	displayRuns = append(displayRuns, authoringRuns...)
	resultRuns := make([]TaskHubRun, 0, len(displayRuns))
	queued := make([]taskHubQueuedRun, 0)
	for _, run := range displayRuns {
		runProjection, projectErr := adapter.projectTaskHubRun(ctx, services, run, task.ID)
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

// appTaskHubSingleOpenAuthoringReview finds one source/session gate owned by
// a draft Task. Multiple open gates deliberately remain unselected: the
// operator must choose one in the detail surface before a decision is planned.
func appTaskHubSingleOpenAuthoringReview(ctx context.Context, services *app.LifecycleServices, taskID string) (*app.AuthoringReviewGateSnapshot, error) {
	if services == nil || services.Store() == nil || services.AuthoringReviews == nil {
		return nil, nil
	}
	runs, err := services.Store().ListAuthoringWorkflowRunsForTargetTask(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("list authoring Runs for Task Hub task %s: %w", taskID, err)
	}
	var active *app.AuthoringReviewGateSnapshot
	for _, run := range runs {
		stages, err := services.Store().ListStageAttemptsForRun(ctx, run.ID)
		if err != nil {
			return nil, fmt.Errorf("list authoring stages for Run %s: %w", run.ID, err)
		}
		for _, stage := range stages {
			binding, err := services.Store().GetAuthoringReviewGateBindingByStageAttempt(ctx, stage.ID)
			if err != nil {
				return nil, fmt.Errorf("read authoring review binding for stage %s: %w", stage.ID, err)
			}
			if binding == nil {
				continue
			}
			review, err := services.AuthoringReviews.Inspect(ctx, binding.ReviewRequestID)
			if err != nil {
				return nil, fmt.Errorf("inspect authoring review %s: %w", binding.ReviewRequestID, err)
			}
			if review.State != store.AuthoringReviewGateOpen {
				continue
			}
			if review.Binding.RunID != run.ID || review.Binding.StageAttemptID != stage.ID {
				return nil, fmt.Errorf("authoring review %s differs from its task-owned Run/stage", review.Request.ID)
			}
			if active != nil {
				return nil, nil
			}
			candidate := review
			active = &candidate
		}
	}
	return active, nil
}

func (adapter *AppTaskHubLifecycleAdapter) projectTaskHubRun(ctx context.Context, services *app.LifecycleServices, run store.WorkflowRun, ownershipTaskID string) (TaskHubRun, error) {
	if strings.TrimSpace(ownershipTaskID) == "" {
		ownershipTaskID = run.TaskID
	}
	attachable := false
	attachReason := "只有由有效 durable lease 持有的 running job 可以附着"
	var attachment *app.RunAttachment
	if taskHubRunIsActive(run.Status) && services.LocalRuntime != nil {
		loaded, err := services.LocalRuntime.AttachRun(ctx, app.AttachRunRequest{RunID: run.ID})
		if err != nil {
			if run.SubjectKind == store.WorkflowRunSubjectAuthoringSession {
				attachReason = "当前部署尚未公开 source/session Run 的本地 Attach capability"
			} else {
				return TaskHubRun{}, fmt.Errorf("read local durable state for Run %s: %w", run.ID, err)
			}
		} else {
			attachment = &loaded
		}
	}
	if taskHubRunCanAttach(run.Status) {
		if attachment == nil {
			if run.SubjectKind != store.WorkflowRunSubjectAuthoringSession {
				attachReason = "本地 durable runtime 附着服务不可用"
			}
		} else {
			attachable = attachment.AttachableJobs > 0
			if !attachable {
				attachReason = "当前 Run 没有由有效 durable lease 持有的运行中 job"
			}
		}
	}
	actions := appTaskHubRunActions(run, attachable, attachReason)
	if evaluatorAction, applies := adapter.taskHubCodeEdgeEvaluatorRunAction(ctx, services, run); applies {
		actions = append(actions, evaluatorAction)
	}
	if handoffAction, applies := adapter.taskHubCodeEdgeEvaluatorEvidenceHandoffRunAction(ctx, services, run); applies {
		actions = append(actions, handoffAction)
	}
	projection := TaskHubRun{
		RunID:          run.ID,
		TaskID:         ownershipTaskID,
		RevisionID:     run.RevisionID,
		ExecutionState: string(run.Status),
		Active:         taskHubRunIsActive(run.Status),
		Actions:        actions,
	}
	if attachment != nil {
		projection.WorkerHandoff = taskHubLatestWorkerHandoff(attachment.WorkerHandoffs)
	}
	projection.Handoff = taskHubRunHandoffCapability(run, projection.WorkerHandoff, services.WorkerHandoffs != nil && adapter.runWorkerHandoffLauncher != nil)
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
		projection.Control.Actions = appTaskHubRunControlActions(run, stage, false, services.LocalRuntime != nil)
		return projection, nil
	}
	checkpoint, err := services.Control.CurrentCheckpoint(ctx, run.ID)
	if err != nil {
		if run.SubjectKind == store.WorkflowRunSubjectAuthoringSession {
			// The normal control service may not yet advertise source/session
			// capability. Keep the Run visible with a capability-derived disabled
			// projection; a later compatible backend will flow through unchanged.
			projection.Control.Actions = appTaskHubRunControlActions(run, stage, false, services.LocalRuntime != nil)
			return projection, nil
		}
		return TaskHubRun{}, fmt.Errorf("read control checkpoint for Run %s: %w", run.ID, err)
	}
	projection.Control.CheckpointSequence = checkpoint.Sequence
	projection.Control.ExecutionEpoch = checkpoint.ExecutionEpoch
	projection.Control.Expected = taskHubControlCheckpoint(checkpoint)
	gracePeriod, err := services.Control.FrozenGracePeriod(ctx, run.ID)
	if err != nil {
		if run.SubjectKind == store.WorkflowRunSubjectAuthoringSession {
			projection.Control.Actions = appTaskHubRunControlActions(run, stage, false, services.LocalRuntime != nil)
			return projection, nil
		}
		return TaskHubRun{}, fmt.Errorf("read frozen control grace period for Run %s: %w", run.ID, err)
	}
	projection.Control.GracePeriod = gracePeriod
	operations, err := services.Control.ListForRun(ctx, run.ID)
	if err != nil {
		if run.SubjectKind == store.WorkflowRunSubjectAuthoringSession {
			projection.Control.Actions = appTaskHubRunControlActions(run, stage, false, services.LocalRuntime != nil)
			return projection, nil
		}
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
	projection.Control.Actions = appTaskHubRunControlActions(run, stage, true, services.LocalRuntime != nil)
	return projection, nil
}

func taskHubLatestWorkerHandoff(handoffs []store.RunWorkerHandoff) *TaskHubWorkerHandoff {
	if len(handoffs) == 0 {
		return nil
	}
	handoff := handoffs[len(handoffs)-1]
	return &TaskHubWorkerHandoff{
		OperationID:      handoff.ID,
		State:            string(handoff.State),
		WorkerLeaseID:    handoff.WorkerLeaseID,
		LaunchDeadlineAt: handoff.LaunchDeadlineAt,
		UpdatedAt:        handoff.UpdatedAt,
		FailureRecorded:  strings.TrimSpace(handoff.FailureReason) != "",
	}
}

func taskHubRunHandoffCapability(run store.WorkflowRun, current *TaskHubWorkerHandoff, launcherConfigured bool) TaskHubRunHandoffCapability {
	capability := TaskHubRunHandoffCapability{
		Expected: TaskHubRunHandoffCheckpoint{
			RunVersion:     run.Version,
			ExecutionEpoch: run.ExecutionEpoch,
			DefinitionHash: run.DefinitionHash,
		},
	}
	if !taskHubRunCanHandoff(run.Status) {
		capability.DisabledReason = fmt.Sprintf("Run 当前处于 %s；此状态不需要或不能交接受控 worker", run.Status)
		return capability
	}
	if current != nil && (current.State == string(store.RunWorkerHandoffLaunching) || current.State == string(store.RunWorkerHandoffHandedOff)) {
		capability.DisabledReason = "当前 Run 已有受控 child worker handoff"
		return capability
	}
	if !launcherConfigured {
		capability.DisabledReason = "当前 Task Hub 未配置受控 child-worker launcher"
		return capability
	}
	capability.Enabled = true
	return capability
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

func appTaskHubGlobalActions(mutationsAvailable, standardAuthoringAvailable bool) []TaskHubActionState {
	actions := []TaskHubActionState{
		{Action: TaskHubActionNewTask, Enabled: mutationsAvailable, DisabledReason: "LifecycleMutationService 不可用"},
		{Action: TaskHubActionImportTask, Enabled: mutationsAvailable, DisabledReason: "LifecycleMutationService 不可用"},
		{Action: TaskHubActionStartStandardAuthoring, Enabled: standardAuthoringAvailable, DisabledReason: "当前部署未配置受控 Standard 创题定义"},
	}
	for index := range actions {
		if actions[index].Enabled {
			actions[index].DisabledReason = ""
		}
	}
	return actions
}

// taskHubCodeEdgePackageAuthorization is a read-only preflight for the TUI.
// It does not treat a stored record as sufficient proof to package: the
// application service repeats the full manifest, catalog, lock, artifact,
// trial, and review verification when the confirmation is executed. Its only
// purpose here is to select the exact immutable Run/record checkpoint or to
// make the missing approval visible before the operator opens a form.
type taskHubCodeEdgePackageAuthorizationBinding struct {
	Run    store.WorkflowRun
	Record store.CodeEdgeComplianceRecord
}

func taskHubCodeEdgePackageAuthorization(ctx context.Context, services *app.LifecycleServices, task store.TaskV2, revision store.TaskRevision, runs []store.WorkflowRun, preferredRunID string) (*taskHubCodeEdgePackageAuthorizationBinding, string, error) {
	if services == nil || services.Store() == nil {
		return nil, "", fmt.Errorf("CodeEdge package authorization store is unavailable")
	}
	codeEdgeRuns := taskHubCodeEdgeRuns(task, revision, runs)
	recordsByRun := make(map[string]*store.CodeEdgeComplianceRecord)
	preferredRunID = strings.TrimSpace(preferredRunID)
	if preferred := taskHubCodeEdgeRunByID(codeEdgeRuns, preferredRunID); preferred != nil {
		record, err := services.Store().GetCodeEdgeComplianceRecordForRun(ctx, preferred.ID)
		if err != nil {
			return nil, "", fmt.Errorf("read CodeEdge final compliance for Run %s: %w", preferred.ID, err)
		}
		recordsByRun[preferred.ID] = record
		selected, unavailable := selectTaskHubCodeEdgePackageAuthorization(task, revision, runs, recordsByRun, preferred.ID)
		return selected, unavailable, nil
	}
	for _, run := range codeEdgeRuns {
		record, err := services.Store().GetCodeEdgeComplianceRecordForRun(ctx, run.ID)
		if err != nil {
			return nil, "", fmt.Errorf("read CodeEdge final compliance for Run %s: %w", run.ID, err)
		}
		recordsByRun[run.ID] = record
	}
	selected, unavailable := selectTaskHubCodeEdgePackageAuthorization(task, revision, runs, recordsByRun, preferredRunID)
	return selected, unavailable, nil
}

func selectTaskHubCodeEdgePackageAuthorization(task store.TaskV2, revision store.TaskRevision, runs []store.WorkflowRun, recordsByRun map[string]*store.CodeEdgeComplianceRecord, preferredRunID string) (*taskHubCodeEdgePackageAuthorizationBinding, string) {
	codeEdgeRuns := taskHubCodeEdgeRuns(task, revision, runs)
	if len(codeEdgeRuns) == 0 {
		return nil, ""
	}
	preferredRunID = strings.TrimSpace(preferredRunID)
	if preferred := taskHubCodeEdgeRunByID(codeEdgeRuns, preferredRunID); preferred != nil {
		return taskHubPreferredCodeEdgePackageAuthorization(task, revision, *preferred, recordsByRun[preferred.ID])
	}

	candidates := make([]taskHubCodeEdgePackageAuthorizationBinding, 0, len(codeEdgeRuns))
	hasRecord := false
	hasApprovedRecord := false
	hasMismatchedApprovedRecord := false
	for _, run := range codeEdgeRuns {
		record := recordsByRun[run.ID]
		if record == nil {
			continue
		}
		hasRecord = true
		if record.Status != store.CodeEdgeComplianceApproved {
			continue
		}
		hasApprovedRecord = true
		if record.RunID != run.ID || record.TaskID != task.ID || record.RevisionID != revision.ID || record.TaskDigest != revision.TaskDigest ||
			workflowkit.Fingerprint(strings.TrimSpace(record.AuthorizationFingerprint)).Validate() != nil {
			hasMismatchedApprovedRecord = true
			continue
		}
		candidates = append(candidates, taskHubCodeEdgePackageAuthorizationBinding{Run: run, Record: *record})
	}
	if len(candidates) == 0 {
		switch {
		case !hasRecord:
			return nil, "当前 revision 存在 CodeEdge Phase-1 Run，但没有已批准的最终合规记录，不能创建本地 package"
		case !hasApprovedRecord:
			return nil, "当前 revision 的 CodeEdge Phase-1 Run 没有已批准的最终合规记录，不能创建本地 package"
		case hasMismatchedApprovedRecord:
			return nil, "当前 revision 的 CodeEdge 已批准合规记录未与当前 TaskRevision/digest 和授权指纹一致绑定，不能创建本地 package"
		default:
			return nil, "当前 revision 的 CodeEdge Phase-1 Run 没有可用的已批准最终合规记录，不能创建本地 package"
		}
	}

	sort.SliceStable(candidates, func(left, right int) bool {
		if !candidates[left].Run.CreatedAt.Equal(candidates[right].Run.CreatedAt) {
			return candidates[left].Run.CreatedAt.After(candidates[right].Run.CreatedAt)
		}
		return candidates[left].Run.ID < candidates[right].Run.ID
	})
	selected := candidates[0]
	return &selected, ""
}

func taskHubCodeEdgeRuns(task store.TaskV2, revision store.TaskRevision, runs []store.WorkflowRun) []store.WorkflowRun {
	codeEdgeRuns := make([]store.WorkflowRun, 0)
	for _, run := range runs {
		if run.TaskID == task.ID && run.RevisionID == revision.ID && taskHubIsCodeEdgePhase1Run(run) {
			codeEdgeRuns = append(codeEdgeRuns, run)
		}
	}
	return codeEdgeRuns
}

func taskHubCodeEdgeRunByID(runs []store.WorkflowRun, runID string) *store.WorkflowRun {
	for index := range runs {
		if runs[index].ID == runID {
			return &runs[index]
		}
	}
	return nil
}

func taskHubPreferredCodeEdgePackageAuthorization(task store.TaskV2, revision store.TaskRevision, run store.WorkflowRun, record *store.CodeEdgeComplianceRecord) (*taskHubCodeEdgePackageAuthorizationBinding, string) {
	if record == nil {
		return nil, "指定的 CodeEdge Phase-1 Run 没有已批准的最终合规记录，不能创建本地 package"
	}
	if record.Status != store.CodeEdgeComplianceApproved {
		return nil, "指定的 CodeEdge Phase-1 Run 的最终合规记录尚未批准，不能创建本地 package"
	}
	if record.RunID != run.ID || record.TaskID != task.ID || record.RevisionID != revision.ID || record.TaskDigest != revision.TaskDigest ||
		workflowkit.Fingerprint(strings.TrimSpace(record.AuthorizationFingerprint)).Validate() != nil {
		return nil, "指定的 CodeEdge Phase-1 Run 的已批准合规记录未与当前 TaskRevision/digest 和授权指纹一致绑定，不能创建本地 package"
	}
	return &taskHubCodeEdgePackageAuthorizationBinding{Run: run, Record: *record}, ""
}

func taskHubIsCodeEdgePhase1Run(run store.WorkflowRun) bool {
	return run.WorkflowTemplateID == workflowadapter.CodeEdgePhase1WorkflowTemplateID &&
		run.WorkflowTemplateVersion == workflowadapter.CodeEdgePhase1WorkflowTemplateVersion
}

func appTaskHubTaskActions(task store.TaskV2, current *store.TaskRevision, hasSingleOpenReview, mutationsAvailable bool, codeEdgePackageUnavailable string) []TaskHubActionState {
	if !mutationsAvailable {
		return []TaskHubActionState{
			{Action: TaskHubActionEditTask, DisabledReason: "LifecycleMutationService 不可用"},
			{Action: TaskHubActionForkTask, DisabledReason: "LifecycleMutationService 不可用"},
			{Action: TaskHubActionArchiveTask, DisabledReason: "LifecycleMutationService 不可用"},
			{Action: TaskHubActionSoftDeleteTask, DisabledReason: "LifecycleMutationService 不可用"},
			{Action: TaskHubActionRestoreTask, DisabledReason: "LifecycleMutationService 不可用"},
			{Action: TaskHubActionStartRun, DisabledReason: "LifecycleMutationService 不可用"},
			{Action: TaskHubActionApproveReview, DisabledReason: "LifecycleMutationService 不可用"},
			{Action: TaskHubActionRequestChanges, DisabledReason: "LifecycleMutationService 不可用"},
			{Action: TaskHubActionRejectReview, DisabledReason: "LifecycleMutationService 不可用"},
			{Action: TaskHubActionPackageRevision, DisabledReason: "LifecycleMutationService 不可用"},
			{Action: TaskHubActionWithdrawRelease, DisabledReason: "LifecycleMutationService 不可用"},
		}
	}
	hasCurrentRevision := current != nil
	packageable := hasCurrentRevision &&
		(current.State == store.RevisionStateValidated || current.State == store.RevisionStateReleased) &&
		strings.TrimSpace(current.ValidationEvidenceManifest) != "" &&
		strings.TrimSpace(codeEdgePackageUnavailable) == ""
	packageReason := "需要当前且已验证的 TaskRevision"
	if hasCurrentRevision && (current.State == store.RevisionStateValidated || current.State == store.RevisionStateReleased) &&
		strings.TrimSpace(current.ValidationEvidenceManifest) != "" && strings.TrimSpace(codeEdgePackageUnavailable) != "" {
		packageReason = strings.TrimSpace(codeEdgePackageUnavailable)
	}
	actions := []TaskHubActionState{
		{Action: TaskHubActionEditTask, Enabled: false, DisabledReason: "需要选择当前 TaskRevision 对应的 Run"},
		{Action: TaskHubActionForkTask, Enabled: hasCurrentRevision, DisabledReason: "当前 Task 没有可 Fork 的 revision"},
		{Action: TaskHubActionArchiveTask, Enabled: task.LifecycleState == store.TaskLifecyclePublished, DisabledReason: "只有 published Task 可以归档"},
		{Action: TaskHubActionSoftDeleteTask, Enabled: task.LifecycleState != store.TaskLifecycleDeleted, DisabledReason: "Task 已处于 deleted；请使用恢复操作"},
		{Action: TaskHubActionRestoreTask, Enabled: task.LifecycleState == store.TaskLifecycleDeleted, DisabledReason: "只有 deleted Task 可以恢复"},
		{Action: TaskHubActionStartRun, Enabled: hasCurrentRevision && task.LifecycleState != store.TaskLifecycleDeleted, DisabledReason: "需要未删除 Task 的当前 TaskRevision"},
		{Action: TaskHubActionApproveReview, Enabled: hasSingleOpenReview, DisabledReason: "需要唯一且打开的 ReviewRequest"},
		{Action: TaskHubActionRequestChanges, Enabled: hasSingleOpenReview, DisabledReason: "需要唯一且打开的 ReviewRequest"},
		{Action: TaskHubActionRejectReview, Enabled: hasSingleOpenReview, DisabledReason: "需要唯一且打开的 ReviewRequest"},
		{Action: TaskHubActionPackageRevision, Enabled: packageable, DisabledReason: packageReason},
		{Action: TaskHubActionWithdrawRelease, Enabled: false, DisabledReason: "请从详情“本地包”分类选择明确的未撤回 release"},
	}
	for index := range actions {
		if actions[index].Enabled {
			actions[index].DisabledReason = ""
		}
	}
	return actions
}

func appTaskHubRunActions(run store.WorkflowRun, attachable bool, attachReason string) []TaskHubActionState {
	continueEnabled := taskHubRunCanContinue(run.Status)
	continueReason := "当前 Run 状态不能进入 continuation planner"
	if continueEnabled {
		continueReason = ""
	}
	attachEnabled := taskHubRunCanAttach(run.Status) && attachable
	if attachEnabled {
		attachReason = ""
	} else if attachReason == "" {
		attachReason = "只有由有效 durable lease 持有的 running job 可以附着"
	}
	return []TaskHubActionState{
		{Action: TaskHubActionContinue, Enabled: continueEnabled, DisabledReason: continueReason},
		{Action: TaskHubActionAttachRun, Enabled: attachEnabled, DisabledReason: attachReason},
		{Action: TaskHubActionOpenRunControl, Enabled: true},
	}
}

// taskHubCodeEdgeEvaluatorRunAction exposes the evaluator only on a concrete
// Phase-1 parent Run. Plan is read-only, so this projection can surface the
// same authoritative gate that the first confirmation will enforce without
// creating a bundle, child Run, durable job, or provider effect.
func (adapter *AppTaskHubLifecycleAdapter) taskHubCodeEdgeEvaluatorRunAction(ctx context.Context, services *app.LifecycleServices, run store.WorkflowRun) (TaskHubActionState, bool) {
	if run.WorkflowTemplateID != workflowadapter.CodeEdgePhase1WorkflowTemplateID ||
		run.WorkflowTemplateVersion != workflowadapter.CodeEdgePhase1WorkflowTemplateVersion {
		return TaskHubActionState{}, false
	}
	state := TaskHubActionState{Action: TaskHubActionEvaluateCodeEdge}
	if services == nil || services.EvaluatorLaunches == nil || !services.EvaluatorLaunches.Available() {
		state.DisabledReason = "当前部署未配置受控 CodeEdge 评测定义"
		return state, true
	}
	if adapter == nil || adapter.runWorkerHandoffLauncher == nil {
		state.DisabledReason = "当前 Task Hub 未配置受控 CodeEdge 评测 worker launcher"
		return state, true
	}
	if _, err := services.EvaluatorLaunches.Plan(ctx, run.ID); err != nil {
		state.DisabledReason = taskHubCodeEdgeEvaluatorPlanUnavailableReason(err)
		return state, true
	}
	state.Enabled = true
	return state, true
}

// taskHubCodeEdgeEvaluatorEvidenceHandoffRunAction exposes adoption only on a
// closed evaluator child Run. It never accepts a parent identity from the TUI:
// the authoritative parent is reloaded from the child's immutable lineage and
// then validated by the application handoff planner.
func (adapter *AppTaskHubLifecycleAdapter) taskHubCodeEdgeEvaluatorEvidenceHandoffRunAction(ctx context.Context, services *app.LifecycleServices, run store.WorkflowRun) (TaskHubActionState, bool) {
	if run.WorkflowTemplateID != workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID ||
		run.WorkflowTemplateVersion != workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion {
		return TaskHubActionState{}, false
	}
	state := TaskHubActionState{Action: TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff}
	if services == nil || services.Mutations == nil {
		state.DisabledReason = "LifecycleMutationService 不可用"
		return state, true
	}
	if services.EvaluatorEvidenceHandoffs == nil {
		state.DisabledReason = "CodeEdge evaluator 证据交接服务不可用"
		return state, true
	}
	if run.Status != store.WorkflowRunSucceeded {
		state.DisabledReason = "只有已成功完成的 CodeEdge evaluator child Run 可以采用证据"
		return state, true
	}
	parent, child, err := adapter.resolveTaskHubCodeEdgeEvaluatorEvidenceHandoffRuns(ctx, services, run.ID)
	if err != nil {
		state.DisabledReason = taskHubCodeEdgeEvaluatorEvidenceHandoffUnavailableReason(err)
		return state, true
	}
	if services.Store() == nil {
		state.DisabledReason = "CodeEdge evaluator 证据交接存储不可用"
		return state, true
	}
	existing, err := services.Store().GetCodeEdgeEvaluatorEvidenceHandoffForParentRun(ctx, parent.ID)
	if err != nil {
		state.DisabledReason = "无法读取 Phase-1 父 Run 的既有证据交接"
		return state, true
	}
	if existing != nil {
		state.DisabledReason = "该 Phase-1 父 Run 已采用一份不可变的 CodeEdge evaluator 证据交接"
		return state, true
	}
	plan, err := services.EvaluatorEvidenceHandoffs.Plan(ctx, parent.ID, child.ID)
	if err != nil {
		state.DisabledReason = taskHubCodeEdgeEvaluatorEvidenceHandoffUnavailableReason(err)
		return state, true
	}
	if plan.ParentRunID != parent.ID || plan.ChildRunID != child.ID || plan.TaskID != parent.TaskID || plan.RevisionID != parent.RevisionID {
		state.DisabledReason = "CodeEdge evaluator 证据交接计划未绑定所选 child 与其冻结父 Run"
		return state, true
	}
	state.Enabled = true
	return state, true
}

// resolveTaskHubCodeEdgeEvaluatorEvidenceHandoffRuns reads the child again
// from lifecycle services and proves its durable parent relation. Callers only
// pass the selected child Run ID; this prevents a stale or manually assembled
// UI target from rebinding valid child evidence to another Phase-1 parent.
func (adapter *AppTaskHubLifecycleAdapter) resolveTaskHubCodeEdgeEvaluatorEvidenceHandoffRuns(ctx context.Context, services *app.LifecycleServices, childRunID string) (store.WorkflowRun, store.WorkflowRun, error) {
	if services == nil || services.Runs == nil {
		return store.WorkflowRun{}, store.WorkflowRun{}, fmt.Errorf("CodeEdge evaluator run service is not configured")
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(childRunID)); err != nil {
		return store.WorkflowRun{}, store.WorkflowRun{}, fmt.Errorf("CodeEdge evaluator child Run: %w", err)
	}
	child, err := services.Runs.Get(ctx, childRunID)
	if err != nil {
		return store.WorkflowRun{}, store.WorkflowRun{}, fmt.Errorf("read CodeEdge evaluator child Run: %w", err)
	}
	if child.WorkflowTemplateID != workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID ||
		child.WorkflowTemplateVersion != workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion {
		return store.WorkflowRun{}, store.WorkflowRun{}, fmt.Errorf("selected Run is not a CodeEdge evaluator child")
	}
	if child.Status != store.WorkflowRunSucceeded {
		return store.WorkflowRun{}, store.WorkflowRun{}, fmt.Errorf("CodeEdge evaluator child Run is not successfully completed")
	}
	parentRunID := strings.TrimSpace(child.ParentRunID)
	if err := store.ValidateUUIDv7(parentRunID); err != nil {
		return store.WorkflowRun{}, store.WorkflowRun{}, fmt.Errorf("CodeEdge evaluator child Run has no valid parent Run identity: %w", err)
	}
	parent, err := services.Runs.Get(ctx, parentRunID)
	if err != nil {
		return store.WorkflowRun{}, store.WorkflowRun{}, fmt.Errorf("read CodeEdge evaluator parent Run: %w", err)
	}
	if parent.WorkflowTemplateID != workflowadapter.CodeEdgePhase1WorkflowTemplateID ||
		parent.WorkflowTemplateVersion != workflowadapter.CodeEdgePhase1WorkflowTemplateVersion {
		return store.WorkflowRun{}, store.WorkflowRun{}, fmt.Errorf("CodeEdge evaluator child Run parent is not a CodeEdge Phase-1 Run")
	}
	if parent.TaskID != child.TaskID || parent.RevisionID != child.RevisionID {
		return store.WorkflowRun{}, store.WorkflowRun{}, fmt.Errorf("CodeEdge evaluator child Run does not share its parent TaskRevision")
	}
	return parent, child, nil
}

// taskHubCodeEdgeEvaluatorEvidenceHandoffUnavailableReason is deliberately
// coarse. Evidence verification errors can name internal artifacts, managed
// paths, or deployment facts, none of which should become an operator-facing
// TUI error string.
func taskHubCodeEdgeEvaluatorEvidenceHandoffUnavailableReason(err error) string {
	if err == nil {
		return "CodeEdge evaluator 证据当前不可采用"
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "must be prepared"):
		return "CodeEdge evaluator 证据采用必须先完成第一步冻结确认"
	case strings.Contains(text, "idempotency"), strings.Contains(text, "completed codeedge evaluator evidence handoff"):
		return "该 CodeEdge evaluator 证据交接已完成或幂等键与既有操作冲突"
	case strings.Contains(text, "final_review"), strings.Contains(text, "final review"):
		return "父 Run 的 FinalReview 尚未完成已批准审批"
	case strings.Contains(text, "parent"), strings.Contains(text, "child run"):
		return "所选 Run 未形成有效的完成态 CodeEdge evaluator child 到 Phase-1 父 Run 谱系"
	case strings.Contains(text, "trial"), strings.Contains(text, "artifact"), strings.Contains(text, "evidence"):
		return "child Run 的 Qwen/Opus 证据或四个逻辑 Trial 尚未完整验证"
	case strings.Contains(text, "catalog"), strings.Contains(text, "lock"), strings.Contains(text, "manifest"), strings.Contains(text, "frozen"), strings.Contains(text, "binding"):
		return "父 Run 或 child Run 的冻结 catalog、lock 或执行说明书未通过校验"
	default:
		return "CodeEdge evaluator 证据当前不可采用；请检查 child 完成状态、父 Run 审批和受控证据"
	}
}

// taskHubCodeEdgeEvaluatorPlanUnavailableReason intentionally maps app-layer
// failures to operator-safe categories. A definition provider may consult
// environment-backed credentials at execution time; its raw error must never
// become a TUI string because it could include deployment-only data.
func taskHubCodeEdgeEvaluatorPlanUnavailableReason(err error) string {
	if err == nil {
		return "受控 CodeEdge 评测当前不可用"
	}
	if errors.Is(err, app.ErrCodeEdgeEvaluatorDefinitionUnavailable) {
		return "当前部署未配置受控 CodeEdge 评测定义"
	}
	if errors.Is(err, app.ErrCodeEdgeEvaluatorDefinitionInvalid) {
		return "受控 CodeEdge 评测定义未通过校验"
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "final_review"), strings.Contains(text, "final review"):
		return "父 Run 的 FinalReview 尚未完成已批准审批"
	case strings.Contains(text, "must use"), strings.Contains(text, "parent run"):
		return "所选 Run 不是可评测的 CodeEdge Phase-1 父 Run"
	case strings.Contains(text, "catalog"), strings.Contains(text, "operation resolver"), strings.Contains(text, "operation provider"):
		return "部署 catalog/lock 或受控操作白名单未通过校验"
	case strings.Contains(text, "managed task snapshot"):
		return "任务快照不能作为受控评测输入冻结"
	case strings.Contains(text, "definition"):
		return "受控 CodeEdge 评测定义未通过校验"
	default:
		return "受控 CodeEdge 评测当前不可用；请检查父 Run 审批与部署说明书"
	}
}

func taskHubCodeEdgeEvaluatorMutationError(err error) error {
	if err == nil {
		return nil
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "handoff"), strings.Contains(text, "worker launcher"), strings.Contains(text, "child worker"):
		return errors.New("受控 CodeEdge child worker 未完成启动；可使用相同幂等键重试")
	case strings.Contains(text, "frozen"), strings.Contains(text, "input bundle"), strings.Contains(text, "no such file"):
		return errors.New("CodeEdge 评测说明书未冻结或不可用；请重新查看计划后准备")
	default:
		return errors.New(taskHubCodeEdgeEvaluatorPlanUnavailableReason(err))
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

func taskHubRunCanHandoff(status store.WorkflowRunStatus) bool {
	switch status {
	case store.WorkflowRunQueued, store.WorkflowRunRunning, store.WorkflowRunPauseRequested, store.WorkflowRunPausing,
		store.WorkflowRunResumeRequested, store.WorkflowRunCancelRequested, store.WorkflowRunStopRequested, store.WorkflowRunCanceling:
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

func appTaskHubRunControlActions(run store.WorkflowRun, stage *store.StageAttempt, controlAvailable, localRuntimeAvailable bool) []TaskHubRunControlActionState {
	actions := []TaskHubRunControlActionState{
		{Action: TaskHubRunControlPause},
		{Action: TaskHubRunControlCancelStage},
		{Action: TaskHubRunControlTerminate},
	}
	// Reconcile is deliberately not a generic command. The projection exposes
	// it only for the two durable Run states that already mean reconciliation is
	// required; normal/active Runs do not render or accept its mnemonic.
	if taskHubRunReconcileEligible(run.Status) {
		actions = append(actions, TaskHubRunControlActionState{Action: TaskHubRunControlReconcile})
	}
	for index := range actions {
		if actions[index].Action == TaskHubRunControlReconcile {
			if !localRuntimeAvailable {
				actions[index].DisabledReason = "本地 durable runtime 服务不可用"
				continue
			}
			actions[index].Enabled = true
			continue
		}
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
	task                     store.TaskV2
	run                      *store.WorkflowRun
	revision                 *store.TaskRevision
	reviewRequestID          string
	reviewRevisionID         string
	authoringReviewRequestID string
	releaseID                string
}

func (adapter *AppTaskHubLifecycleAdapter) resolveTaskHubTarget(ctx context.Context, services *app.LifecycleServices, target TaskHubTarget) (taskHubResolvedTarget, error) {
	resolved := taskHubResolvedTarget{
		reviewRequestID:          strings.TrimSpace(target.ReviewRequestID),
		reviewRevisionID:         strings.TrimSpace(target.ReviewRevisionID),
		authoringReviewRequestID: strings.TrimSpace(target.AuthoringReviewRequestID),
		releaseID:                strings.TrimSpace(target.ReleaseID),
	}
	if strings.TrimSpace(target.RunID) != "" {
		run, err := services.Runs.Get(ctx, target.RunID)
		if err != nil {
			return resolved, fmt.Errorf("get Run %s for Task Hub plan: %w", target.RunID, err)
		}
		resolved.run = &run
		if run.SubjectKind == store.WorkflowRunSubjectAuthoringSession {
			session, err := services.Store().GetAuthoringSessionForRun(ctx, run.ID)
			if err != nil {
				return resolved, fmt.Errorf("get AuthoringSession for Task Hub Run %s: %w", run.ID, err)
			}
			if session == nil || strings.TrimSpace(session.TargetTaskID) == "" {
				return resolved, fmt.Errorf("authoring Run %s has no draft Task ownership", run.ID)
			}
			if target.TaskID != "" && target.TaskID != session.TargetTaskID {
				return resolved, fmt.Errorf("Task Hub target AuthoringSession Run %s does not belong to Task %s", run.ID, target.TaskID)
			}
			if target.RevisionID != "" {
				return resolved, fmt.Errorf("Task Hub target AuthoringSession Run %s cannot use a TaskRevision", run.ID)
			}
			target.TaskID = session.TargetTaskID
		} else {
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
