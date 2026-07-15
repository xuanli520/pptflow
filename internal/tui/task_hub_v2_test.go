package tui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

func TestAggregateTaskHubGroupsRunsByStableTaskID(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	rows := AggregateTaskHub(TaskHubSnapshot{
		Tasks: []TaskHubTask{
			{TaskID: "task-a", Name: "alpha", UpdatedAt: now.Add(-time.Minute)},
			{TaskID: "task-b", Name: "beta", UpdatedAt: now},
		},
		Runs: []TaskHubRun{
			{RunID: "run-a-old", TaskID: "task-a", ExecutionState: "failed", StartedAt: now.Add(-2 * time.Hour)},
			{RunID: "run-a-new", TaskID: "task-a", ExecutionState: "running", Active: true, StartedAt: now.Add(-time.Hour)},
			{RunID: "run-b", TaskID: "task-b", ExecutionState: "completed", FinishedAt: now.Add(-time.Minute)},
			{RunID: "orphan", TaskID: "missing-task", ExecutionState: "running", Active: true, StartedAt: now},
		},
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 declared tasks", len(rows))
	}
	if rows[0].Task.TaskID != "task-b" || rows[1].Task.TaskID != "task-a" {
		t.Fatalf("rows are not ordered by task update time: %#v", rows)
	}
	alpha := rows[1]
	if alpha.RunCount != 2 || alpha.ActiveRunCount != 1 || !alpha.HasLatestRun || alpha.LatestRun.RunID != "run-a-new" {
		t.Fatalf("alpha aggregation = %#v, want two runs and latest active run", alpha)
	}
}

func TestTaskHubRunDisplayStateUsesDistinctControlOutcomeLabels(t *testing.T) {
	cases := []struct {
		name string
		run  TaskHubRun
		want string
	}{
		{"paused", TaskHubRun{ExecutionState: "paused"}, "已暂停·可继续"},
		{"stage canceled", TaskHubRun{ExecutionState: "running", Active: true, Control: TaskHubRunControl{OperationAction: TaskHubRunControlCancelStage, OperationStatus: "acknowledged"}}, "阶段已取消·Run 仍进行"},
		{"terminated", TaskHubRun{ExecutionState: "canceled"}, "已终止"},
		{"reconcile", TaskHubRun{ExecutionState: "in_doubt"}, "异常中断·待 reconcile"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := taskHubRunDisplayState(testCase.run); got != testCase.want {
				t.Fatalf("display state = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestTaskHubV2PrefixIsSafeUntilSecondKeyAndTimeoutOrEscapeCancel(t *testing.T) {
	service := &fakeTaskHubLifecycle{snapshot: enabledTaskHubSnapshot()}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()

	updated, firstCmd := m.Update(runeKey("t"))
	m = updated.(model)
	if firstCmd == nil || m.taskHubPrefix.Prefix != 't' || service.planCallCount() != 0 {
		t.Fatalf("plain prefix must only enter local state: prefix=%q plans=%d cmd=%v", m.taskHubPrefix.Prefix, service.planCallCount(), firstCmd)
	}
	sequence := m.taskHubPrefix.Sequence
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(model)
	if m.taskHubPrefix.Prefix != 0 || service.planCallCount() != 0 {
		t.Fatalf("Esc did not safely clear prefix: prefix=%q plans=%d", m.taskHubPrefix.Prefix, service.planCallCount())
	}

	updated, _ = m.Update(runeKey("t"))
	m = updated.(model)
	updated, _ = m.Update(taskHubPrefixTimeoutMsg{sequence: m.taskHubPrefix.Sequence})
	m = updated.(model)
	if m.taskHubPrefix.Prefix != 0 {
		t.Fatal("prefix timeout did not clear state")
	}
	updated, lateCmd := m.Update(runeKey("a"))
	m = updated.(model)
	if lateCmd != nil || service.planCallCount() != 0 {
		t.Fatalf("second key after timeout planned a mutation: plans=%d cmd=%v", service.planCallCount(), lateCmd)
	}
	if sequence == m.taskHubPrefix.Sequence {
		t.Fatal("new prefix sequence did not advance")
	}

	m.hubSearching = true
	m.hubSearch.Focus()
	updated, _ = m.Update(runeKey("t"))
	m = updated.(model)
	if m.taskHubPrefix.Prefix != 0 || service.planCallCount() != 0 {
		t.Fatalf("input focus allowed lifecycle prefix parsing: prefix=%q plans=%d", m.taskHubPrefix.Prefix, service.planCallCount())
	}
}

func TestTaskHubV2DisabledReasonIsShownAndDoesNotPlan(t *testing.T) {
	snapshot := enabledTaskHubSnapshot()
	snapshot.Tasks[0].Actions = append(snapshot.Tasks[0].Actions,
		TaskHubActionState{Action: TaskHubActionArchiveTask, Enabled: false, DisabledReason: "存在活跃 Run"},
	)
	service := &fakeTaskHubLifecycle{snapshot: snapshot}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()

	updated, _ := m.Update(runeKey("t"))
	m = updated.(model)
	if hint := m.taskHubPrefixHint(); !strings.Contains(hint, "存在活跃 Run") {
		t.Fatalf("prefix hint omitted disabled reason: %q", hint)
	}
	updated, command := m.Update(runeKey("a"))
	m = updated.(model)
	if service.planCallCount() != 0 {
		t.Fatalf("disabled action invoked plan service %d times", service.planCallCount())
	}
	if command == nil || !strings.Contains(m.notice, "存在活跃 Run") {
		t.Fatalf("disabled action did not explain reason: notice=%q cmd=%v", m.notice, command)
	}
}

func TestTaskHubEditActionRequiresSelectedCurrentRevisionRun(t *testing.T) {
	snapshot := enabledTaskHubSnapshot()
	snapshot.Tasks[0].Actions = append(snapshot.Tasks[0].Actions, TaskHubActionState{
		Action: TaskHubActionEditTask, DisabledReason: "需要选择当前 TaskRevision 对应的 Run",
	})
	service := &fakeTaskHubLifecycle{snapshot: snapshot}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()
	if state := m.taskHubActionState(TaskHubActionEditTask); !state.Enabled {
		t.Fatalf("selected current-revision Run did not enable manual patch: %+v", state)
	}
	m.taskHub.Snapshot.Runs[0].RevisionID = "revision-old"
	if state := m.taskHubActionState(TaskHubActionEditTask); state.Enabled || !strings.Contains(state.DisabledReason, "选择") {
		t.Fatalf("stale Run revision enabled manual patch: %+v", state)
	}
}

func TestTaskHubV2SecondKeyRequestsPlanAndControlShowsServiceStatus(t *testing.T) {
	service := &fakeTaskHubLifecycle{
		snapshot: enabledTaskHubSnapshot(),
		plan: TaskHubPlanPreview{
			PlanID:             "plan-1",
			Title:              "继续处理 Run",
			Summary:            "从最近 checkpoint 恢复",
			Reason:             "stage interrupted",
			BudgetImpact:       "stage_attempt +0",
			ConfirmationNeeded: true,
		},
	}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()

	updated, _ := m.Update(runeKey("x"))
	m = updated.(model)
	updated, command := m.Update(runeKey("c"))
	m = updated.(model)
	if command == nil || service.planCallCount() != 0 {
		t.Fatalf("second key should return a deferred plan command only: cmd=%v plans=%d", command, service.planCallCount())
	}
	message := command()
	updated, _ = m.Update(message)
	m = updated.(model)
	if service.planCallCount() != 1 || m.taskHubPlan == nil || m.taskHubPlan.PlanID != "plan-1" {
		t.Fatalf("plan preview was not applied: calls=%d preview=%#v", service.planCallCount(), m.taskHubPlan)
	}
	if rendered := m.taskHubV2View(); !strings.Contains(rendered, "计划预览") || !strings.Contains(rendered, "最近 checkpoint") {
		t.Fatalf("plan preview did not render:\n%s", rendered)
	}

	updated, _ = m.Update(runeKey("x"))
	m = updated.(model)
	updated, controlCmd := m.Update(runeKey("k"))
	m = updated.(model)
	if controlCmd != nil || m.runControl == nil || m.runControl.RunID != "run-1" || m.runControl.ControlStatus != "acknowledged" {
		t.Fatalf("x k did not open V2 control status: command=%v control=%+v", controlCmd, m.runControl)
	}
	if rendered := m.runControl.View(72, 24); !strings.Contains(rendered, "控制状态：acknowledged") {
		t.Fatalf("control overlay omitted service status:\n%s", rendered)
	}
}

func TestTaskHubPlanPreviewRendersEveryFrozenConsequenceInHubAndConfirmation(t *testing.T) {
	preview := TaskHubPlanPreview{
		PlanID:              "frozen-consequences",
		Title:               "继续处理计划（已冻结）",
		Summary:             "已根据冻结 checkpoint 计算范围。",
		Reason:              "quality stage interrupted",
		RevisionImpact:      "当前 TaskRevision 保持不变。",
		ExecutionScope:      []string{"quality_check", "similarity_check"},
		InvalidatedEvidence: []string{"package"},
		ReusedEvidence:      []string{"repo_prepare"},
		BudgetImpact:        "stage_attempt +1",
		ExternalEffects:     []string{"package requires explicit confirmation"},
		ConfirmationNeeded:  true,
	}
	service := &fakeTaskHubLifecycle{snapshot: enabledTaskHubSnapshot()}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()
	m.taskHubPlan = &preview

	assertPlanFacts := func(t *testing.T, rendered string) {
		t.Helper()
		for _, want := range []string{
			"原因：", "quality stage interrupted", "Task 版本影响：", "当前 TaskRevision 保持不变。",
			"将执行：", "quality_check", "将失效：", "package", "将复用：", "repo_prepare",
			"预算变化：", "stage_attempt +1", "外部副作用：", "package requires explicit confirmation",
		} {
			if !strings.Contains(rendered, want) {
				t.Fatalf("plan preview omitted %q:\n%s", want, rendered)
			}
		}
	}
	assertPlanFacts(t, ansi.Strip(m.taskHubV2View()))

	overlay, err := newTaskHubMutationOverlay(TaskHubActionContinue, TaskHubTarget{TaskID: "task-1", RunID: "run-1"}, preview)
	if err != nil {
		t.Fatal(err)
	}
	assertPlanFacts(t, ansi.Strip(overlay.View(100, 60)))
}

func TestTaskHubV2NarrowLayoutFitsTerminal(t *testing.T) {
	service := &fakeTaskHubLifecycle{snapshot: enabledTaskHubSnapshot()}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()
	m.width, m.height = 40, 16
	m.taskHub.Snapshot.Tasks[0].Name = strings.Repeat("very-long-task-name-", 6)
	m.taskHub.Rows = AggregateTaskHub(m.taskHub.Snapshot)
	rendered := m.taskHubV2View()
	for _, line := range strings.Split(ansi.Strip(rendered), "\n") {
		if got := ansi.StringWidth(line); got > m.width {
			t.Fatalf("narrow Task Hub line width = %d, terminal width = %d: %q\n%s", got, m.width, line, rendered)
		}
	}
}

func TestTaskHubQueueShowsUnconfiguredCapacityWithoutInventingZero(t *testing.T) {
	service := &fakeTaskHubLifecycle{snapshot: enabledTaskHubSnapshot()}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()

	m.taskHub.Snapshot.Queue = TaskHubQueue{Running: 2, Queued: 1}
	lines := strings.Join(m.taskHubQueueLines(80), "\n")
	if !strings.Contains(lines, "运行中 2 / 未配置") || strings.Contains(lines, "运行中 2 / 0") {
		t.Fatalf("unconfigured capacity rendered misleadingly: %q", lines)
	}

	m.taskHub.Snapshot.Queue.Concurrency = 4
	lines = strings.Join(m.taskHubQueueLines(80), "\n")
	if !strings.Contains(lines, "运行中 2 / 4") {
		t.Fatalf("configured capacity did not render: %q", lines)
	}
}

func TestTaskHubV2PlainPrefixesNeverExecuteLifecycleMutation(t *testing.T) {
	for _, prefix := range []string{"t", "x", "v", "p"} {
		t.Run(prefix, func(t *testing.T) {
			service := &fakeTaskHubLifecycle{snapshot: enabledTaskHubSnapshot()}
			m, cleanup := newTestTaskHubV2Model(t, service)
			defer cleanup()
			updated, command := m.Update(runeKey(prefix))
			got := updated.(model)
			if service.planCallCount() != 0 || got.taskHubPrefix.Prefix != rune(prefix[0]) {
				t.Fatalf("plain %q called lifecycle service or missed prefix: calls=%d prefix=%q", prefix, service.planCallCount(), got.taskHubPrefix.Prefix)
			}
			if command == nil {
				t.Fatalf("plain %q did not arm timeout state", prefix)
			}
		})
	}
}

func TestTaskHubV2PackageSequencePlansOnlyLocalPackage(t *testing.T) {
	snapshot := enabledTaskHubSnapshot()
	snapshot.Tasks[0].Actions = append(snapshot.Tasks[0].Actions,
		TaskHubActionState{Action: TaskHubActionPackageRevision, Enabled: true},
	)
	service := &fakeTaskHubLifecycle{snapshot: snapshot, plan: TaskHubPlanPreview{Title: "本地 package", Summary: "写入受管包目录"}}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()

	updated, _ := m.Update(runeKey("p"))
	m = updated.(model)
	updated, command := m.Update(runeKey("p"))
	m = updated.(model)
	if command == nil || service.planCallCount() != 0 {
		t.Fatalf("p p must produce a deferred local-package plan: command=%v plans=%d", command, service.planCallCount())
	}
	updated, _ = m.Update(command())
	m = updated.(model)
	if action := service.lastPlanAction(); action != TaskHubActionPackageRevision {
		t.Fatalf("p p action = %q, want local package action %q", action, TaskHubActionPackageRevision)
	}
	if m.taskHubPlan == nil || !strings.Contains(m.taskHubPlan.Title, "package") {
		t.Fatalf("local package plan was not rendered: %#v", m.taskHubPlan)
	}
}

func TestTaskHubV2PlanPreviewRequiresExplicitActionAndCanBeDismissed(t *testing.T) {
	service := &fakeTaskHubLifecycle{
		snapshot: enabledTaskHubSnapshot(),
		plan:     TaskHubPlanPreview{PlanID: "plan-preview", Title: "继续处理", Summary: "从已保存 checkpoint 恢复", ConfirmationNeeded: true},
	}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()

	updated, _ := m.Update(runeKey("x"))
	m = updated.(model)
	updated, planCmd := m.Update(runeKey("c"))
	m = updated.(model)
	if planCmd == nil {
		t.Fatal("continue sequence did not request a plan")
	}
	updated, _ = m.Update(planCmd())
	m = updated.(model)
	if m.taskHubPlan == nil {
		t.Fatal("plan preview was not retained")
	}

	updated, enterCmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if enterCmd != nil || service.planCallCount() != 1 || m.taskHubPlan == nil || m.taskHubMutation == nil {
		t.Fatalf("Enter must only open the native confirmation form: command=%v calls=%d plan=%#v form=%#v", enterCmd, service.planCallCount(), m.taskHubPlan, m.taskHubMutation)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(model)
	if m.taskHubMutation != nil || service.mutationCallCount() != 0 {
		t.Fatalf("Esc must close confirmation without executing: form=%#v mutations=%d", m.taskHubMutation, service.mutationCallCount())
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(model)
	if m.taskHubPlan != nil {
		t.Fatal("Esc did not dismiss the plan preview")
	}
}

func TestTaskHubV2RunControlOnlySelectsAndPreviewsWithoutSubmittingControl(t *testing.T) {
	service := &fakeTaskHubLifecycle{
		snapshot: enabledTaskHubSnapshot(),
		controlPlan: TaskHubPlanPreview{
			Title:              "暂停运行影响预览",
			Summary:            "确认后会创建 ControlOperation",
			Reason:             "Run 正在运行",
			ExternalEffects:    []string{"预览不发出 checkpoint 请求"},
			ConfirmationNeeded: true,
		},
	}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()

	updated, _ := m.Update(runeKey("x"))
	m = updated.(model)
	updated, _ = m.Update(runeKey("k"))
	m = updated.(model)
	if m.runControl == nil {
		t.Fatal("x k did not open the lifecycle Run Control overlay")
	}

	updated, selectCmd := m.Update(runeKey("p"))
	m = updated.(model)
	if selectCmd != nil || m.runControl.SelectedAction != TaskHubRunControlPause || service.controlPlanCallCount() != 0 || service.planCallCount() != 0 {
		t.Fatalf("P must only select a control preview: selected=%q controlPlans=%d taskPlans=%d cmd=%v", m.runControl.SelectedAction, service.controlPlanCallCount(), service.planCallCount(), selectCmd)
	}
	if rendered := m.runControl.View(88, 30); !strings.Contains(rendered, "[P] 暂停运行") {
		t.Fatalf("Run Control omitted selected pause action:\n%s", rendered)
	}
	// A hub refresh can change the active row while an overlay is open. The
	// preview must retain the immutable target captured by that overlay.
	m.taskHub.SelectedTaskID = "other-task"
	m.taskHub.SelectedRunID = "other-run"

	updated, previewCmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if previewCmd == nil || service.controlPlanCallCount() != 0 || service.planCallCount() != 0 {
		t.Fatalf("Enter must create only a deferred control-preview read: controlPlans=%d taskPlans=%d cmd=%v", service.controlPlanCallCount(), service.planCallCount(), previewCmd)
	}
	updated, _ = m.Update(previewCmd())
	m = updated.(model)
	if service.controlPlanCallCount() != 1 || m.runControl == nil || m.runControl.Preview == nil || !strings.Contains(m.runControl.Preview.Summary, "确认后") {
		t.Fatalf("control preview was not retained before confirmation: calls=%d overlay=%+v", service.controlPlanCallCount(), m.runControl)
	}
	command := service.lastControlPlanCommand()
	if command.Target.TaskID != "task-1" || command.Target.RunID != "run-1" || command.Target.RevisionID != "revision-1" || command.Target.StageAttemptID != "stage-1" {
		t.Fatalf("control preview target was rebound after selection changed: %+v", command.Target)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(model)
	if m.runControl != nil || service.controlPlanCallCount() != 1 || service.planCallCount() != 0 {
		t.Fatalf("Esc must close the control overlay without submitting anything: overlay=%+v controlPlans=%d taskPlans=%d", m.runControl, service.controlPlanCallCount(), service.planCallCount())
	}
}

func TestTaskHubV2RunControlEscDropsLatePreviewWithoutSideEffects(t *testing.T) {
	service := &fakeTaskHubLifecycle{
		snapshot:    enabledTaskHubSnapshot(),
		controlPlan: TaskHubPlanPreview{Title: "终止运行（只读预览）", Summary: "不会创建 ControlOperation"},
	}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()
	m.openRunControl()

	updated, _ := m.Update(runeKey("s"))
	m = updated.(model)
	updated, previewCmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if previewCmd == nil {
		t.Fatal("selected control action did not create a deferred preview")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(model)
	if m.runControl != nil {
		t.Fatal("Esc did not close control overlay before preview returned")
	}
	updated, _ = m.Update(previewCmd())
	m = updated.(model)
	if m.runControl != nil || service.controlPlanCallCount() != 1 || service.planCallCount() != 0 {
		t.Fatalf("late preview reopened or mutated a dismissed overlay: overlay=%+v controlPlans=%d taskPlans=%d", m.runControl, service.controlPlanCallCount(), service.planCallCount())
	}
}

func TestTaskHubV2RunControlReconcileKeyAndActionRequireEligibleRun(t *testing.T) {
	normal := enabledTaskHubSnapshot()
	normal.Runs[0].ExecutionState = string(store.WorkflowRunRunning)
	normalService := &fakeTaskHubLifecycle{snapshot: normal}
	normalModel, normalCleanup := newTestTaskHubV2Model(t, normalService)
	defer normalCleanup()
	normalModel.openRunControl()
	if rendered := normalModel.runControl.View(88, 30); strings.Contains(rendered, "[R] 本地 reconcile") {
		t.Fatalf("normal Run rendered a reconcile action:\n%s", rendered)
	}
	updated, command := normalModel.Update(runeKey("r"))
	normalModel = updated.(model)
	if command != nil || normalModel.runControl.SelectedAction != "" {
		t.Fatalf("normal Run accepted reconcile mnemonic: action=%q command=%v", normalModel.runControl.SelectedAction, command)
	}

	eligible := enabledTaskHubSnapshot()
	eligible.Runs[0].ExecutionState = string(store.WorkflowRunInDoubt)
	eligible.Runs[0].Control.Actions = append(eligible.Runs[0].Control.Actions, TaskHubRunControlActionState{
		Action: TaskHubRunControlReconcile, Enabled: true,
	})
	eligibleService := &fakeTaskHubLifecycle{snapshot: eligible}
	eligibleModel, eligibleCleanup := newTestTaskHubV2Model(t, eligibleService)
	defer eligibleCleanup()
	eligibleModel.openRunControl()
	if rendered := eligibleModel.runControl.View(88, 30); !strings.Contains(rendered, "[R] 本地 reconcile") || !strings.Contains(rendered, "[P/K/S/R]") {
		t.Fatalf("reconcile-eligible Run did not render its explicit action/key hint:\n%s", rendered)
	}
	if footer := ansi.Strip(eligibleModel.footer()); !strings.Contains(footer, "[P/K/S/R]") {
		t.Fatalf("reconcile-eligible Run footer omitted its available key: %q", footer)
	}
	updated, command = eligibleModel.Update(runeKey("r"))
	eligibleModel = updated.(model)
	if command != nil || eligibleModel.runControl.SelectedAction != TaskHubRunControlReconcile {
		t.Fatalf("eligible Run did not select reconcile preview: action=%q command=%v", eligibleModel.runControl.SelectedAction, command)
	}
	eligibleModel.runControl.selectReturn()
	if command = clickRenderedMarker(t, &eligibleModel, "[R] 本地 reconcile"); command != nil || eligibleModel.runControl.SelectedAction != TaskHubRunControlReconcile {
		t.Fatalf("eligible Run mouse target did not select reconcile preview: action=%q command=%v", eligibleModel.runControl.SelectedAction, command)
	}
}

func TestLifecycleRunControlNarrowLayoutFitsTerminal(t *testing.T) {
	overlay := newLifecycleRunControlOverlay(TaskHubRun{
		RunID:          "run-with-a-very-long-stable-identity",
		TaskID:         "task-with-a-very-long-stable-identity",
		ExecutionState: "in_doubt",
		Stage:          "evaluation/harbor_qwen",
		ControlStatus:  "reconcile_required",
		Control: TaskHubRunControl{
			StageAttemptID:         "stage-attempt-with-a-very-long-stable-identity",
			StageExecutionState:    "reconciling",
			CheckpointSequence:     12345,
			ExecutionEpoch:         7,
			OperationID:            "operation-with-a-very-long-stable-identity",
			OperationAction:        TaskHubRunControlTerminate,
			CheckpointID:           "checkpoint-with-a-very-long-stable-identity",
			QuotaSettlementID:      "quota-settlement-with-a-very-long-stable-identity",
			RuntimeReceiptCount:    2,
			ExternalOutcomeUnknown: true,
			Actions: []TaskHubRunControlActionState{
				{Action: TaskHubRunControlPause, DisabledReason: "需要操作者、操作原因和幂等键；当前 TUI 仅提供只读预览"},
				{Action: TaskHubRunControlCancelStage, DisabledReason: "当前 StageAttempt 未声明 cancel capability；不能由 TUI 推断 provider 是否支持取消"},
				{Action: TaskHubRunControlTerminate, DisabledReason: "需要操作者、操作原因和幂等键；当前 TUI 仅提供只读预览"},
				{Action: TaskHubRunControlReconcile, Enabled: true},
			},
		},
	})
	overlay.selectAction(TaskHubRunControlCancelStage)
	overlay.Preview = &TaskHubPlanPreview{
		Title:           "取消选中阶段（不可提交）",
		Summary:         "当前状态下不能生成可提交的运行控制命令；本次调用只读取事实，不会创建 ControlOperation。",
		Reason:          "当前 StageAttempt 未声明 cancel capability。",
		ExternalEffects: []string{"本次预览不会取消阶段"},
	}
	rendered := overlay.View(34, 14)
	for _, line := range strings.Split(ansi.Strip(rendered), "\n") {
		if got := ansi.StringWidth(line); got > 34 {
			t.Fatalf("narrow Run Control line width = %d, terminal width = 34: %q\n%s", got, line, rendered)
		}
	}
}

func TestRunWithLifecycleStartsServiceBackedHubWithoutLegacyStore(t *testing.T) {
	previous := newTeaProgram
	defer func() { newTeaProgram = previous }()
	service := &fakeTaskHubLifecycle{snapshot: enabledTaskHubSnapshot()}
	var captured tea.Model
	newTeaProgram = func(model tea.Model, _ ...tea.ProgramOption) teaProgram {
		captured = model
		return fakeTeaProgram{model: model}
	}
	if err := RunWithLifecycle(context.Background(), service); err != nil {
		t.Fatalf("run V2 lifecycle hub: %v", err)
	}
	got, ok := captured.(model)
	if !ok {
		t.Fatalf("captured model type = %T, want tui model", captured)
	}
	if got.lifecycle == nil || got.view != viewHub {
		t.Fatalf("V2 lifecycle hub did not initialize the lifecycle page: lifecycle=%v view=%v", got.lifecycle != nil, got.view)
	}
}

type fakeTaskHubLifecycle struct {
	mu               sync.Mutex
	snapshot         TaskHubSnapshot
	plan             TaskHubPlanPreview
	controlPlan      TaskHubPlanPreview
	prepared         TaskHubPlanPreview
	queries          []TaskHubQuery
	commands         []TaskHubCommand
	controlCommands  []TaskHubRunControlCommand
	prepareCommands  []TaskHubMutationRequest
	mutationCommands []TaskHubMutationRequest
	controlMutations []TaskHubRunControlMutationRequest
	err              error
}

func (service *fakeTaskHubLifecycle) QueryTaskHub(_ context.Context, query TaskHubQuery) (TaskHubSnapshot, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.queries = append(service.queries, query)
	return service.snapshot.Clone(), service.err
}

func (service *fakeTaskHubLifecycle) PlanTaskHubCommand(_ context.Context, command TaskHubCommand) (TaskHubPlanPreview, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.commands = append(service.commands, command)
	return service.plan.Clone(), service.err
}

func (service *fakeTaskHubLifecycle) PlanTaskHubRunControl(_ context.Context, command TaskHubRunControlCommand) (TaskHubPlanPreview, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.controlCommands = append(service.controlCommands, command)
	return service.controlPlan.Clone(), service.err
}

func (service *fakeTaskHubLifecycle) PrepareTaskHubMutation(_ context.Context, command TaskHubMutationRequest) (TaskHubPreparedMutation, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.prepareCommands = append(service.prepareCommands, command.Clone())
	preview := service.prepared.Clone()
	if preview.PlanID == "" {
		preview.PlanID = "frozen-plan-for-test"
	}
	preview.ConfirmationNeeded = true
	return TaskHubPreparedMutation{Preview: preview, Actor: command.Actor, Reason: command.Reason}, service.err
}

func (service *fakeTaskHubLifecycle) ExecuteTaskHubMutation(_ context.Context, command TaskHubMutationRequest) (TaskHubMutationResult, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.mutationCommands = append(service.mutationCommands, command.Clone())
	return TaskHubMutationResult{Action: command.Action, Target: command.Target, PlanID: command.PlanID, ExecutionID: "execution-for-test", Summary: "测试生命周期操作已提交"}, service.err
}

func (service *fakeTaskHubLifecycle) ExecuteTaskHubRunControlMutation(_ context.Context, command TaskHubRunControlMutationRequest) (TaskHubRunControlMutationResult, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.controlMutations = append(service.controlMutations, command)
	return TaskHubRunControlMutationResult{Action: command.Action, OperationID: "control-for-test", Summary: "测试运行控制已提交"}, service.err
}

func (service *fakeTaskHubLifecycle) planCallCount() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	return len(service.commands)
}

func (service *fakeTaskHubLifecycle) lastPlanAction() TaskHubAction {
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.commands) == 0 {
		return ""
	}
	return service.commands[len(service.commands)-1].Action
}

func (service *fakeTaskHubLifecycle) controlPlanCallCount() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	return len(service.controlCommands)
}

func (service *fakeTaskHubLifecycle) mutationCallCount() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	return len(service.mutationCommands)
}

func (service *fakeTaskHubLifecycle) prepareCallCount() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	return len(service.prepareCommands)
}

func (service *fakeTaskHubLifecycle) controlMutationCallCount() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	return len(service.controlMutations)
}

func (service *fakeTaskHubLifecycle) lastMutationCommand() TaskHubMutationRequest {
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.mutationCommands) == 0 {
		return TaskHubMutationRequest{}
	}
	return service.mutationCommands[len(service.mutationCommands)-1].Clone()
}

func (service *fakeTaskHubLifecycle) lastControlPlanCommand() TaskHubRunControlCommand {
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.controlCommands) == 0 {
		return TaskHubRunControlCommand{}
	}
	return service.controlCommands[len(service.controlCommands)-1]
}

func newTestTaskHubV2Model(t *testing.T, service TaskHubLifecycleService) (model, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m := initialLifecycleHubModel(ctx, cancel, service)
	m.width, m.height = 100, 30
	loaded := m.loadTaskHubV2()().(taskHubLoadedMsg)
	if loaded.err != nil {
		cancel()
		t.Fatalf("load V2 Task Hub: %v", loaded.err)
	}
	m.applyTaskHubSnapshot(loaded.snapshot)
	return m, cancel
}

func TestTaskHubIgnoresOutOfOrderAsyncQueryResponses(t *testing.T) {
	service := &fakeTaskHubLifecycle{snapshot: enabledTaskHubSnapshot()}
	m, cancel := newTestTaskHubV2Model(t, service)
	defer cancel()

	// Start an older search, then replace it before its result arrives. The
	// response payloads are deliberately distinct so this catches an old result
	// being applied over the active query rather than merely testing a guard.
	m.taskHub.Query.Filter = "older"
	m.taskHub.Loading = true
	olderCmd := m.loadTaskHubV2()
	m.taskHub.Query.Filter = "newer"
	newerCmd := m.loadTaskHubV2()
	older := olderCmd().(taskHubLoadedMsg)
	older.snapshot.Tasks[0].Name = "stale search result"
	newer := newerCmd().(taskHubLoadedMsg)
	newer.snapshot.Tasks[0].Name = "current search result"

	updated, _ := m.Update(older)
	m = updated.(model)
	if m.taskHub.Query.Filter != "newer" || m.taskHub.Snapshot.Tasks[0].Name == "stale search result" || !m.taskHub.Loading {
		t.Fatalf("stale query response changed Task Hub state: query=%q snapshot=%+v loading=%t", m.taskHub.Query.Filter, m.taskHub.Snapshot, m.taskHub.Loading)
	}
	updated, _ = m.Update(newer)
	m = updated.(model)
	if m.taskHub.Snapshot.Tasks[0].Name != "current search result" || m.taskHub.Loading {
		t.Fatalf("current query response was not applied: snapshot=%+v loading=%t", m.taskHub.Snapshot, m.taskHub.Loading)
	}

	// Polls can overlap even when their query is identical. The later request
	// remains authoritative, preventing an older poll from regressing visible
	// lifecycle facts after a newer response has arrived.
	m.taskHub.Loading = true
	olderPollCmd := m.loadTaskHubV2()
	newerPollCmd := m.loadTaskHubV2()
	olderPoll := olderPollCmd().(taskHubLoadedMsg)
	olderPoll.snapshot.Tasks[0].Name = "stale poll result"
	newerPoll := newerPollCmd().(taskHubLoadedMsg)
	newerPoll.snapshot.Tasks[0].Name = "current poll result"
	updated, _ = m.Update(olderPoll)
	m = updated.(model)
	if m.taskHub.Snapshot.Tasks[0].Name == "stale poll result" || !m.taskHub.Loading {
		t.Fatalf("stale poll response changed Task Hub state: snapshot=%+v loading=%t", m.taskHub.Snapshot, m.taskHub.Loading)
	}
	updated, _ = m.Update(newerPoll)
	m = updated.(model)
	if m.taskHub.Snapshot.Tasks[0].Name != "current poll result" || m.taskHub.Loading {
		t.Fatalf("latest poll response was not applied: snapshot=%+v loading=%t", m.taskHub.Snapshot, m.taskHub.Loading)
	}
}

func enabledTaskHubSnapshot() TaskHubSnapshot {
	return TaskHubSnapshot{
		Tasks: []TaskHubTask{{
			TaskID:     "task-1",
			Name:       "durable-task",
			Lifecycle:  "validated",
			RevisionID: "revision-1",
			Revision:   "v1",
			Actions: []TaskHubActionState{
				{Action: TaskHubActionContinue, Enabled: true},
				{Action: TaskHubActionOpenRunControl, Enabled: true},
			},
		}},
		Runs: []TaskHubRun{{
			RunID:          "run-1",
			TaskID:         "task-1",
			RevisionID:     "revision-1",
			ExecutionState: "paused",
			Stage:          "quality",
			ControlStatus:  "acknowledged",
			Actions: []TaskHubActionState{
				{Action: TaskHubActionContinue, Enabled: true},
				{Action: TaskHubActionOpenRunControl, Enabled: true},
			},
			Control: TaskHubRunControl{
				StageAttemptID:      "stage-1",
				StageExecutionState: "running",
				CheckpointSequence:  4,
				ExecutionEpoch:      1,
				Actions: []TaskHubRunControlActionState{
					{Action: TaskHubRunControlPause, Enabled: true},
					{Action: TaskHubRunControlCancelStage, DisabledReason: "当前 StageAttempt 未声明 cancel capability；不能由 TUI 推断 provider 是否支持取消"},
					{Action: TaskHubRunControlTerminate, Enabled: true},
				},
			},
		}},
		GlobalActions: []TaskHubActionState{
			{Action: TaskHubActionNewTask, Enabled: true},
		},
	}
}
