package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

func TestTaskHubReviewConfirmationEscLeavesRealSQLiteUntouched(t *testing.T) {
	ctx, services, task, revision, review := newTaskHubReviewMutationFixture(t)
	adapter := NewAppTaskHubLifecycleAdapter(services)
	m, cleanup := newTestTaskHubV2Model(t, adapter)
	defer cleanup()

	m = openTaskHubReviewConfirmation(t, m)
	if m.taskHubMutation == nil || m.taskHubMutation.Action != TaskHubActionApproveReview {
		t.Fatalf("review confirmation form was not opened: %+v", m.taskHubMutation)
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(model)
	if m.taskHubMutation != nil {
		t.Fatal("Esc did not close review confirmation form")
	}
	requests, err := services.Store().ListReviewRequestsForRevision(ctx, revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].ID != review.ID || requests[0].State != "open" {
		t.Fatalf("Esc mutated review request: %+v", requests)
	}
	decisions, err := services.Store().ListReviewDecisionsForRequest(ctx, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 0 {
		t.Fatalf("Esc created review decisions: %+v", decisions)
	}
	current, err := services.Tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Version != task.Version || current.CurrentRevisionID != task.CurrentRevisionID {
		t.Fatalf("Esc mutated Task projection: before=%+v after=%+v", task, current)
	}
}

func TestTaskHubReviewConfirmationRetriesWithSameKeyAgainstRealSQLite(t *testing.T) {
	ctx, services, _, _, review := newTaskHubReviewMutationFixture(t)
	adapter := &lateReplyTaskHubAdapter{AppTaskHubLifecycleAdapter: NewAppTaskHubLifecycleAdapter(services), failFirstMutationReply: true}
	m, cleanup := newTestTaskHubV2Model(t, adapter)
	defer cleanup()

	m = openTaskHubReviewConfirmation(t, m)
	form := m.taskHubMutation
	if form == nil {
		t.Fatal("review confirmation form was not opened")
	}
	form.ReasonInput.SetValue("approve reviewed fixture")
	key := form.IdempotencyKey

	updated, firstCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if firstCommand == nil {
		t.Fatal("first review confirmation did not create deferred mutation command")
	}
	updated, _ = m.Update(firstCommand())
	m = updated.(model)
	if m.taskHubMutation == nil || m.taskHubMutation.IdempotencyKey != key || m.taskHubMutation.ReasonInput.Value() != "approve reviewed fixture" {
		t.Fatalf("failed mutation did not retain form/key for retry: %+v", m.taskHubMutation)
	}
	if m.taskHubMutation.Error == "" {
		t.Fatal("simulated lost response did not leave a retryable form error")
	}
	decisions, err := services.Store().ListReviewDecisionsForRequest(ctx, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].ID != key {
		t.Fatalf("first durable decision was not recorded under TUI idempotency key: %+v", decisions)
	}

	updated, retryCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if retryCommand == nil {
		t.Fatal("retry did not create deferred mutation command")
	}
	updated, _ = m.Update(retryCommand())
	m = updated.(model)
	if m.taskHubMutation != nil {
		t.Fatalf("idempotent retry did not complete confirmation: %+v", m.taskHubMutation)
	}
	decisions, err = services.Store().ListReviewDecisionsForRequest(ctx, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].ID != key {
		t.Fatalf("retry created duplicate or changed decision: %+v", decisions)
	}
}

func TestTaskHubCodeEdgeEvaluatorTwoPhaseConfirmationRetriesLostReplyWithoutSecondWorker(t *testing.T) {
	ctx, services, _, task, revision, parent := newTaskHubCodeEdgeEvaluatorFixture(t)
	parent = taskHubApproveCodeEdgeFinalReviewGate(t, ctx, services, parent, revision)
	launcher := &recordingTaskHubRunWorkerLauncher{}
	adapter := &lateReplyTaskHubAdapter{
		AppTaskHubLifecycleAdapter: NewAppTaskHubLifecycleAdapterWithRunWorkerHandoffLauncher(services, launcher),
		failFirstMutationReply:     true,
	}
	m, cleanup := newTestTaskHubV2Model(t, adapter)
	defer cleanup()
	m.taskHub.SelectedTaskID = task.ID
	m.taskHub.SelectedRunID = parent.ID

	updated, _ := m.Update(runeKey("x"))
	m = updated.(model)
	updated, planCommand := m.Update(runeKey("e"))
	m = updated.(model)
	if planCommand == nil {
		t.Fatal("x e did not request a CodeEdge evaluator plan")
	}
	updated, _ = m.Update(planCommand())
	m = updated.(model)
	if m.taskHubPlan == nil || m.taskHubPlanCommand == nil || m.taskHubPlanCommand.Action != TaskHubActionEvaluateCodeEdge ||
		m.taskHubPlanCommand.Target.RunID != parent.ID || !m.taskHubPlan.ConfirmationNeeded {
		t.Fatalf("CodeEdge evaluator plan did not bind the approved parent Run: plan=%+v command=%+v", m.taskHubPlan, m.taskHubPlanCommand)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if m.taskHubMutation == nil || m.taskHubMutation.Action != TaskHubActionEvaluateCodeEdge {
		t.Fatalf("CodeEdge evaluator confirmation form was not opened: %+v", m.taskHubMutation)
	}
	m.taskHubMutation.ReasonInput.SetValue("run the approved CodeEdge evaluator")
	key := m.taskHubMutation.IdempotencyKey
	if err := store.ValidateUUIDv7(key); err != nil {
		t.Fatalf("TUI evaluator idempotency key %q is not UUIDv7: %v", key, err)
	}

	updated, prepareCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if prepareCommand == nil {
		t.Fatal("first evaluator confirmation did not create a prepare command")
	}
	updated, _ = m.Update(prepareCommand())
	m = updated.(model)
	if m.taskHubMutation == nil || !m.taskHubMutation.isFrozen() || m.taskHubMutation.IdempotencyKey != key ||
		m.taskHubMutation.FrozenReason != "run the approved CodeEdge evaluator" || m.taskHubMutation.Preview.PlanID != "codeedge-evaluator:"+key {
		t.Fatalf("evaluator prepare did not freeze the original confirmation: %+v", m.taskHubMutation)
	}
	runs, err := services.Store().ListWorkflowRunsForTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != parent.ID {
		t.Fatalf("evaluator prepare created a child Run: %+v", runs)
	}
	if launches := launcher.launchRequests(); len(launches) != 0 {
		t.Fatalf("evaluator prepare launched a worker: %+v", launches)
	}

	updated, firstConfirmCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if firstConfirmCommand == nil {
		t.Fatal("second evaluator confirmation did not create a launch command")
	}
	updated, _ = m.Update(firstConfirmCommand())
	m = updated.(model)
	if m.taskHubMutation == nil || m.taskHubMutation.IdempotencyKey != key || m.taskHubMutation.Error == "" || !m.taskHubMutation.isFrozen() {
		t.Fatalf("lost evaluator response did not retain the frozen retry form: %+v", m.taskHubMutation)
	}
	runs, err = services.Store().ListWorkflowRunsForTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	child := taskHubEvaluatorChildRunForParent(t, runs, parent.ID)
	if child.WorkflowTemplateID != workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID ||
		child.WorkflowTemplateVersion != workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion || child.ParentRunID != parent.ID {
		t.Fatalf("evaluator child Run did not preserve closed parent/template identity: %+v", child)
	}
	jobs, err := services.Store().ListDurableJobsForRun(ctx, child.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("evaluator child durable jobs = %+v, %v; want one", jobs, err)
	}
	handoffs, err := services.Store().ListRunWorkerHandoffsForRun(ctx, child.ID)
	if err != nil || len(handoffs) != 1 || handoffs[0].State != store.RunWorkerHandoffLaunching {
		t.Fatalf("evaluator child durable handoffs = %+v, %v; want one launched handoff", handoffs, err)
	}
	if launches := launcher.launchRequests(); len(launches) != 1 || launches[0].RunID != child.ID {
		t.Fatalf("first evaluator confirmation launches = %+v, want exactly one child worker", launches)
	}

	updated, retryCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if retryCommand == nil {
		t.Fatal("lost evaluator response did not retain retry command")
	}
	updated, _ = m.Update(retryCommand())
	m = updated.(model)
	if m.taskHubMutation != nil {
		t.Fatalf("idempotent evaluator retry did not close confirmation: %+v", m.taskHubMutation)
	}
	if launches := launcher.launchRequests(); len(launches) != 1 {
		t.Fatalf("evaluator retry launched another child worker: %+v", launches)
	}
	runs, err = services.Store().ListWorkflowRunsForTask(ctx, task.ID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("evaluator retry created duplicate child Run: %+v, %v", runs, err)
	}
	handoffs, err = services.Store().ListRunWorkerHandoffsForRun(ctx, child.ID)
	if err != nil || len(handoffs) != 1 {
		t.Fatalf("evaluator retry created duplicate handoff: %+v, %v", handoffs, err)
	}
}

func taskHubEvaluatorChildRunForParent(t *testing.T, runs []store.WorkflowRun, parentRunID string) store.WorkflowRun {
	t.Helper()
	var child *store.WorkflowRun
	for index := range runs {
		if runs[index].ParentRunID != parentRunID {
			continue
		}
		if child != nil {
			t.Fatalf("multiple evaluator child Runs for parent %s: %+v", parentRunID, runs)
		}
		candidate := runs[index]
		child = &candidate
	}
	if child == nil {
		t.Fatalf("no evaluator child Run for parent %s: %+v", parentRunID, runs)
	}
	return *child
}

func TestTaskHubDetailSelectedReviewExecutesOnlyCapturedReviewAgainstRealSQLite(t *testing.T) {
	ctx, services, task, revision, reviews := newTaskHubMultipleReviewMutationFixture(t)
	adapter := NewAppTaskHubLifecycleAdapter(services)
	m, cleanup := newTestTaskHubV2Model(t, adapter)
	defer cleanup()

	updated, detailCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if detailCommand == nil || m.taskHubDetail == nil {
		t.Fatalf("multiple-review Task Hub did not open detail: overlay=%+v command=%v", m.taskHubDetail, detailCommand)
	}
	updated, _ = m.Update(detailCommand())
	m = updated.(model)
	m.taskHubDetail.Tab = TaskHubDetailFactsTab
	updated, _ = m.Update(runeKey("]"))
	m = updated.(model)
	selectedID := m.taskHub.SelectedReviewRequestID
	if selectedID == "" || m.taskHub.SelectedReviewRevisionID != revision.ID {
		t.Fatalf("detail did not retain a concrete open review: state=%+v", m.taskHub)
	}
	if selectedID != reviews[0].ID && selectedID != reviews[1].ID {
		t.Fatalf("detail selected an unknown review %q from %+v", selectedID, reviews)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(model)
	updated, _ = m.Update(runeKey("v"))
	m = updated.(model)
	updated, planCommand := m.Update(runeKey("a"))
	m = updated.(model)
	if planCommand == nil {
		t.Fatal("selected review did not enter the two-key plan path")
	}
	updated, _ = m.Update(planCommand())
	m = updated.(model)
	if m.taskHubPlanCommand == nil || m.taskHubPlan == nil || m.taskHubPlanCommand.Target.ReviewRequestID != selectedID || m.taskHubPlanCommand.Target.ReviewRevisionID != revision.ID || !m.taskHubPlan.ConfirmationNeeded {
		t.Fatalf("real adapter plan did not bind selected review: command=%+v preview=%+v", m.taskHubPlanCommand, m.taskHubPlan)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if m.taskHubMutation == nil || m.taskHubMutation.Target.ReviewRequestID != selectedID {
		t.Fatalf("native confirmation lost selected review identity: %+v", m.taskHubMutation)
	}
	m.taskHubMutation.ReasonInput.SetValue("approve the specifically selected review")
	updated, executeCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if executeCommand == nil {
		t.Fatal("review confirmation did not create a deferred execute command")
	}
	updated, _ = m.Update(executeCommand())
	m = updated.(model)
	if m.taskHubMutation != nil {
		t.Fatalf("selected review execution did not close native confirmation: %+v", m.taskHubMutation)
	}

	current, err := services.Store().ListReviewRequestsForRevision(ctx, revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[string]string, len(current))
	for _, review := range current {
		states[review.ID] = review.State
	}
	if states[selectedID] != "closed" {
		t.Fatalf("selected review %s was not closed: %+v", selectedID, states)
	}
	for _, review := range reviews {
		if review.ID != selectedID && states[review.ID] != "open" {
			t.Fatalf("unselected review %s was mutated while deciding %s: %+v", review.ID, selectedID, states)
		}
	}
	currentTask, err := services.Tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if currentTask.ID != task.ID {
		t.Fatalf("unexpected Task identity after selected review decision: %+v", currentTask)
	}
}

func TestTaskHubLocalPackageConfirmationRetriesWithSameKeyAgainstRealSQLite(t *testing.T) {
	ctx, services, task, revision := newTaskHubLocalPackageMutationFixture(t)
	adapter := &lateReplyTaskHubAdapter{AppTaskHubLifecycleAdapter: NewAppTaskHubLifecycleAdapter(services), failFirstMutationReply: true}
	m, cleanup := newTestTaskHubV2Model(t, adapter)
	defer cleanup()

	updated, _ := m.Update(runeKey("p"))
	m = updated.(model)
	updated, planCommand := m.Update(runeKey("p"))
	m = updated.(model)
	if planCommand == nil {
		t.Fatal("p p did not request a local package plan")
	}
	updated, _ = m.Update(planCommand())
	m = updated.(model)
	if m.taskHubPlan == nil || !m.taskHubPlan.ConfirmationNeeded {
		t.Fatalf("local package plan was not confirmation-capable: %+v", m.taskHubPlan)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	form := m.taskHubMutation
	if form == nil || form.Action != TaskHubActionPackageRevision {
		t.Fatalf("local package confirmation form was not opened: %+v", form)
	}
	form.ReasonInput.SetValue("create local package after reviewed validation")
	versionInput := form.ValueInputs[taskHubPackageVersionField]
	versionInput.SetValue("tui-package-v1")
	form.ValueInputs[taskHubPackageVersionField] = versionInput
	key := form.IdempotencyKey
	if err := store.ValidateUUIDv7(key); err != nil {
		t.Fatalf("local package confirmation key %q is not UUIDv7: %v", key, err)
	}

	updated, firstCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if firstCommand == nil {
		t.Fatal("first local package confirmation did not create a deferred mutation command")
	}
	updated, _ = m.Update(firstCommand())
	m = updated.(model)
	if m.taskHubMutation == nil || m.taskHubMutation.IdempotencyKey != key ||
		m.taskHubMutation.ReasonInput.Value() != "create local package after reviewed validation" ||
		m.taskHubMutation.ValueInputs[taskHubPackageVersionField].Value() != "tui-package-v1" {
		t.Fatalf("failed local package mutation did not retain retry form: %+v", m.taskHubMutation)
	}
	if m.taskHubMutation.Error == "" {
		t.Fatal("simulated lost local package response did not leave a retryable form error")
	}
	releases, err := services.Releases.List(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 1 || releases[0].ID != key || releases[0].RevisionID != revision.ID || releases[0].ReleaseVersion != "tui-package-v1" {
		t.Fatalf("first local package submit was not durably keyed by the TUI UUIDv7: %+v", releases)
	}

	updated, retryCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if retryCommand == nil {
		t.Fatal("local package retry did not create a deferred mutation command")
	}
	updated, _ = m.Update(retryCommand())
	m = updated.(model)
	if m.taskHubMutation != nil {
		t.Fatalf("idempotent local package retry did not complete confirmation: %+v", m.taskHubMutation)
	}
	releases, err = services.Releases.List(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 1 || releases[0].ID != key || releases[0].ReleaseVersion != "tui-package-v1" {
		t.Fatalf("local package retry created a duplicate or changed release: %+v", releases)
	}
}

func TestTaskHubContinuationFormFreezesPlanThenExecutesWithOneKey(t *testing.T) {
	service := &fakeTaskHubLifecycle{
		snapshot: enabledTaskHubSnapshot(),
		plan: TaskHubPlanPreview{
			Title: "继续处理", Summary: "先生成计划", ConfirmationNeeded: true,
		},
		prepared: TaskHubPlanPreview{PlanID: "frozen-plan", Title: "继续处理计划（已冻结）", Summary: "精确范围", ConfirmationNeeded: true},
	}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()

	updated, _ := m.Update(runeKey("x"))
	m = updated.(model)
	updated, planCommand := m.Update(runeKey("c"))
	m = updated.(model)
	updated, _ = m.Update(planCommand())
	m = updated.(model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if m.taskHubMutation == nil {
		t.Fatal("continuation confirmation form was not opened")
	}
	key := m.taskHubMutation.IdempotencyKey
	actor := m.taskHubMutation.Actor
	if err := store.ValidateUUIDv7(key); err != nil {
		t.Fatalf("confirmation did not allocate UUIDv7 idempotency key %q: %v", key, err)
	}
	m.taskHubMutation.ReasonInput.SetValue("resume after checkpoint")

	updated, prepareCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if prepareCommand == nil || service.prepareCallCount() != 0 || service.mutationCallCount() != 0 {
		t.Fatalf("first confirmation should only freeze plan: prepare=%v planned=%d executed=%d", prepareCommand, service.prepareCallCount(), service.mutationCallCount())
	}
	updated, _ = m.Update(prepareCommand())
	m = updated.(model)
	if m.taskHubMutation == nil || m.taskHubMutation.Preview.PlanID != "frozen-plan" || m.taskHubMutation.IdempotencyKey != key {
		t.Fatalf("frozen plan was not retained in same confirmation form: %+v", m.taskHubMutation)
	}
	if !m.taskHubMutation.isFrozen() || m.taskHubMutation.FrozenActor != actor || m.taskHubMutation.FrozenReason != "resume after checkpoint" {
		t.Fatalf("frozen plan provenance was not locked: %+v", m.taskHubMutation)
	}
	if service.prepareCallCount() != 1 || service.mutationCallCount() != 0 {
		t.Fatalf("freeze phase counts = prepare %d execute %d", service.prepareCallCount(), service.mutationCallCount())
	}
	// Even a direct model mutation cannot change the frozen command provenance;
	// normal keyboard input is rejected by the frozen-form key path as well.
	m.taskHubMutation.ReasonInput.SetValue("tampered after freeze")
	updated, editCommand := m.Update(runeKey("x"))
	m = updated.(model)
	if editCommand != nil || m.taskHubMutation.ReasonInput.Value() != "tampered after freeze" || m.taskHubMutation.request().Reason != "resume after checkpoint" {
		t.Fatalf("frozen form accepted mutable provenance: command=%v form=%+v", editCommand, m.taskHubMutation)
	}
	rendered := m.taskHubMutation.View(100, 60)
	if !strings.Contains(rendered, "冻结操作原因：resume after checkpoint") || !strings.Contains(rendered, "冻结操作员："+actor) {
		t.Fatalf("frozen form did not render immutable provenance:\n%s", rendered)
	}

	updated, executeCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if executeCommand == nil {
		t.Fatal("second confirmation did not execute frozen plan")
	}
	updated, _ = m.Update(executeCommand())
	m = updated.(model)
	if service.mutationCallCount() != 1 || m.taskHubMutation != nil {
		t.Fatalf("frozen continuation was not executed once: calls=%d form=%+v", service.mutationCallCount(), m.taskHubMutation)
	}
	command := service.lastMutationCommand()
	if command.IdempotencyKey != key || command.PlanID != "frozen-plan" || command.Reason != "resume after checkpoint" {
		t.Fatalf("continuation execution did not retain form identity: %+v", command)
	}
}

func TestTaskHubConfirmationRequiresReasonBeforeAnyPlannerOrMutation(t *testing.T) {
	service := &fakeTaskHubLifecycle{
		snapshot: enabledTaskHubSnapshot(),
		plan:     TaskHubPlanPreview{Title: "继续处理", Summary: "确认前不冻结", ConfirmationNeeded: true},
	}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()
	updated, _ := m.Update(runeKey("x"))
	m = updated.(model)
	updated, planCommand := m.Update(runeKey("c"))
	m = updated.(model)
	updated, _ = m.Update(planCommand())
	m = updated.(model)
	updated, command := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if command != nil || m.taskHubMutation == nil {
		t.Fatalf("plan confirmation did not open form: command=%v form=%+v", command, m.taskHubMutation)
	}
	updated, command = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if command != nil || service.prepareCallCount() != 0 || service.mutationCallCount() != 0 || m.taskHubMutation.Error == "" {
		t.Fatalf("blank reason reached lifecycle services: command=%v prepare=%d execute=%d error=%q", command, service.prepareCallCount(), service.mutationCallCount(), m.taskHubMutation.Error)
	}
}

func TestTaskHubRunControlConfirmationCarriesCapturedCheckpoint(t *testing.T) {
	snapshot := enabledTaskHubSnapshot()
	snapshot.Runs[0].ExecutionState = "running"
	snapshot.Runs[0].Control.Expected = TaskHubControlCheckpoint{
		Sequence: 4, ExecutionEpoch: 1, SubjectVersion: 2, SubjectID: "task-1", SubjectRevisionID: "revision-1",
		SubjectDigest: "harbor.task.v2:sha256:fixture", WorkflowFingerprint: "sha256:workflow",
	}
	service := &fakeTaskHubLifecycle{
		snapshot: snapshot,
		controlPlan: TaskHubPlanPreview{
			Title: "暂停运行影响预览", Summary: "确认后提交", ConfirmationNeeded: true,
		},
	}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()
	m.openRunControl()

	updated, _ := m.Update(runeKey("p"))
	m = updated.(model)
	updated, previewCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	updated, _ = m.Update(previewCommand())
	m = updated.(model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if m.taskHubMutation == nil || !m.taskHubMutation.isRunControl() {
		t.Fatalf("run-control confirmation form was not opened: %+v", m.taskHubMutation)
	}
	m.taskHubMutation.ReasonInput.SetValue("pause before inspection")
	updated, executeCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	updated, _ = m.Update(executeCommand())
	m = updated.(model)
	if service.controlMutationCallCount() != 1 {
		t.Fatalf("control mutation calls = %d, want one", service.controlMutationCallCount())
	}
	service.mu.Lock()
	command := service.controlMutations[0]
	service.mu.Unlock()
	if command.Action != TaskHubRunControlPause || command.Target.RunID != "run-1" || command.Expected != snapshot.Runs[0].Control.Expected {
		t.Fatalf("run-control command lost captured target/checkpoint: %+v", command)
	}
	if command.Reason != "pause before inspection" || command.Actor == "" || command.IdempotencyKey == "" {
		t.Fatalf("run-control confirmation omitted required input: %+v", command)
	}
}

func TestTaskHubRunControlReconcileRequiresConfirmationAndRetainsUUIDv7KeyForRetry(t *testing.T) {
	snapshot := enabledTaskHubSnapshot()
	snapshot.Runs[0].ExecutionState = string(store.WorkflowRunInDoubt)
	snapshot.Runs[0].Control.Actions = append(snapshot.Runs[0].Control.Actions, TaskHubRunControlActionState{
		Action: TaskHubRunControlReconcile, Enabled: true,
	})
	service := &fakeTaskHubLifecycle{
		snapshot: snapshot,
		controlPlan: TaskHubPlanPreview{
			Title:              "本地 reconcile 影响预览",
			Summary:            "确认后只恢复本地 durable 状态",
			ConfirmationNeeded: true,
		},
	}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()
	m.openRunControl()

	updated, selectCommand := m.Update(runeKey("r"))
	m = updated.(model)
	if selectCommand != nil || m.runControl == nil || m.runControl.SelectedAction != TaskHubRunControlReconcile || service.controlMutationCallCount() != 0 {
		t.Fatalf("reconcile selection performed work before preview: overlay=%+v command=%v mutations=%d", m.runControl, selectCommand, service.controlMutationCallCount())
	}
	updated, previewCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if previewCommand == nil || service.controlPlanCallCount() != 0 || service.controlMutationCallCount() != 0 {
		t.Fatalf("reconcile Enter did not defer a read-only preview: plan=%d mutations=%d command=%v", service.controlPlanCallCount(), service.controlMutationCallCount(), previewCommand)
	}
	updated, _ = m.Update(previewCommand())
	m = updated.(model)
	if m.runControl == nil || m.runControl.Preview == nil || !m.runControl.Preview.ConfirmationNeeded || service.controlMutationCallCount() != 0 {
		t.Fatalf("reconcile preview was not retained before confirmation: overlay=%+v mutations=%d", m.runControl, service.controlMutationCallCount())
	}
	if planned := service.lastControlPlanCommand(); planned.Action != TaskHubRunControlReconcile || planned.Target.RunID != snapshot.Runs[0].RunID {
		t.Fatalf("reconcile preview target/action = %+v", planned)
	}
	updated, confirmationCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if confirmationCommand != nil || m.taskHubMutation == nil || !m.taskHubMutation.isRunControl() || m.taskHubMutation.ControlAction != TaskHubRunControlReconcile {
		t.Fatalf("reconcile preview did not open native confirmation: command=%v form=%+v", confirmationCommand, m.taskHubMutation)
	}
	m.taskHubMutation.ReasonInput.SetValue("recover selected local durable facts")
	key := m.taskHubMutation.IdempotencyKey
	if err := store.ValidateUUIDv7(key); err != nil {
		t.Fatalf("reconcile confirmation key %q is not UUIDv7: %v", key, err)
	}

	service.mu.Lock()
	service.err = errors.New("simulated response loss after local reconcile")
	service.mu.Unlock()
	updated, firstExecute := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if firstExecute == nil {
		t.Fatal("reconcile confirmation did not defer execution")
	}
	updated, _ = m.Update(firstExecute())
	m = updated.(model)
	if m.taskHubMutation == nil || m.taskHubMutation.IdempotencyKey != key || m.taskHubMutation.ReasonInput.Value() != "recover selected local durable facts" || m.taskHubMutation.Error == "" {
		t.Fatalf("failed reconcile response did not retain retryable form/key: %+v", m.taskHubMutation)
	}

	service.mu.Lock()
	service.err = nil
	service.mu.Unlock()
	updated, retryExecute := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if retryExecute == nil {
		t.Fatal("reconcile retry did not defer execution")
	}
	updated, _ = m.Update(retryExecute())
	m = updated.(model)
	if m.taskHubMutation != nil {
		t.Fatalf("successful reconcile retry did not close confirmation: %+v", m.taskHubMutation)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.controlMutations) != 2 {
		t.Fatalf("reconcile execution calls = %d, want two retries", len(service.controlMutations))
	}
	for _, command := range service.controlMutations {
		if command.Action != TaskHubRunControlReconcile || command.Target.RunID != snapshot.Runs[0].RunID || command.IdempotencyKey != key ||
			command.Reason != "recover selected local durable facts" || command.Actor == "" {
			t.Fatalf("reconcile retry lost its confirmed input: %+v", command)
		}
	}
}

func TestTaskHubMutationOverlayCollectsActionSpecificTypedInputs(t *testing.T) {
	cases := []struct {
		name   string
		action TaskHubAction
		values map[string]string
	}{
		{name: "create", action: TaskHubActionNewTask, values: map[string]string{taskHubTaskSlugField: "typed-create", taskHubTaskTitleField: "Typed Create", taskHubTaskMetadataJSONField: `{"kind":"fixture"}`}},
		{name: "import", action: TaskHubActionImportTask, values: map[string]string{taskHubTaskSlugField: "typed-import", taskHubTaskTitleField: "Typed Import", taskHubImportSourcePathField: "/managed/snapshot"}},
		{name: "fork", action: TaskHubActionForkTask, values: map[string]string{taskHubTaskSlugField: "typed-fork", taskHubTaskTitleField: "Typed Fork"}},
		{name: "restore", action: TaskHubActionRestoreTask, values: map[string]string{taskHubRestoreStateField: "ready"}},
		{name: "start", action: TaskHubActionStartRun, values: map[string]string{taskHubExecutionProfilePathField: "/profiles/explicit.json", taskHubExecutionSpecPathField: "/specs/frozen.json", taskHubRunTriggerField: "task_hub"}},
		{name: "edit", action: TaskHubActionEditTask, values: map[string]string{taskHubUnifiedDiffField: "--- a/instruction.md\n+++ b/instruction.md\n@@ -1 +1 @@\n-old\n+new"}},
		{name: "package", action: TaskHubActionPackageRevision, values: map[string]string{taskHubPackageVersionField: "v1.2.3"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			overlay, err := newTaskHubMutationOverlay(testCase.action, TaskHubTarget{}, TaskHubPlanPreview{ConfirmationNeeded: true})
			if err != nil {
				t.Fatal(err)
			}
			overlay.ReasonInput.SetValue("typed form fixture")
			for field, value := range testCase.values {
				setTaskHubMutationFormValue(t, overlay, field, value)
			}
			if err := overlay.validate(); err != nil {
				t.Fatalf("validate %s form: %v", testCase.action, err)
			}
			request := overlay.request()
			for field, want := range testCase.values {
				if got := request.Values[field]; got != want {
					t.Fatalf("request value %s = %q, want %q", field, got, want)
				}
			}
			if err := store.ValidateUUIDv7(request.IdempotencyKey); err != nil {
				t.Fatalf("form idempotency key %q is not UUIDv7: %v", request.IdempotencyKey, err)
			}
		})
	}
}

func TestTaskHubRunStartFormRequiresIndependentProfileAndExecutionSpecPaths(t *testing.T) {
	overlay, err := newTaskHubMutationOverlay(TaskHubActionStartRun, TaskHubTarget{TaskID: "task-1", RevisionID: "revision-1"}, TaskHubPlanPreview{ConfirmationNeeded: true})
	if err != nil {
		t.Fatal(err)
	}
	overlay.ReasonInput.SetValue("start frozen run")
	setTaskHubMutationFormValue(t, overlay, taskHubExecutionProfilePathField, "/profiles/explicit.json")
	setTaskHubMutationFormValue(t, overlay, taskHubRunTriggerField, "task_hub")
	if err := overlay.validate(); err == nil || !strings.Contains(err.Error(), "Execution spec JSON") {
		t.Fatalf("profile-only start form validation = %v, want execution-spec path error", err)
	}

	setTaskHubMutationFormValue(t, overlay, taskHubExecutionProfilePathField, "")
	setTaskHubMutationFormValue(t, overlay, taskHubExecutionSpecPathField, "/specs/frozen.json")
	if err := overlay.validate(); err == nil || !strings.Contains(err.Error(), "Execution profile JSON") {
		t.Fatalf("execution-spec-only start form validation = %v, want profile path error", err)
	}

	setTaskHubMutationFormValue(t, overlay, taskHubExecutionProfilePathField, "/profiles/explicit.json")
	if err := overlay.validate(); err != nil {
		t.Fatalf("complete start form validation: %v", err)
	}
	wantOrder := []string{"reason", taskHubExecutionProfilePathField, taskHubExecutionSpecPathField, taskHubRunTriggerField}
	if len(overlay.FieldOrder) != len(wantOrder) {
		t.Fatalf("start form field order = %#v, want %#v", overlay.FieldOrder, wantOrder)
	}
	for index, want := range wantOrder {
		if got := overlay.FieldOrder[index]; got != want {
			t.Fatalf("start form field %d = %q, want %q", index, got, want)
		}
	}
	request := overlay.request()
	if request.Values[taskHubExecutionProfilePathField] != "/profiles/explicit.json" || request.Values[taskHubExecutionSpecPathField] != "/specs/frozen.json" || request.Values[taskHubRunTriggerField] != "task_hub" {
		t.Fatalf("independent start paths were not preserved in request: %+v", request.Values)
	}
	if rendered := overlay.View(100, 30); !strings.Contains(rendered, "Execution profile JSON") || !strings.Contains(rendered, "Execution spec JSON") {
		t.Fatalf("start form did not render both explicit path inputs:\n%s", rendered)
	}
}

func TestTaskHubRunStartFormErrorRetainsPathsAndIdempotencyKeyForRetry(t *testing.T) {
	service := &fakeTaskHubLifecycle{snapshot: enabledTaskHubSnapshot()}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()

	overlay, err := newTaskHubMutationOverlay(TaskHubActionStartRun, TaskHubTarget{TaskID: "task-1", RevisionID: "revision-1"}, TaskHubPlanPreview{ConfirmationNeeded: true})
	if err != nil {
		t.Fatal(err)
	}
	overlay.ReasonInput.SetValue("start after input validation")
	setTaskHubMutationFormValue(t, overlay, taskHubExecutionProfilePathField, "/profiles/explicit.json")
	setTaskHubMutationFormValue(t, overlay, taskHubExecutionSpecPathField, "/specs/frozen.json")
	setTaskHubMutationFormValue(t, overlay, taskHubRunTriggerField, "task_hub")
	m.taskHubMutation = overlay
	key := overlay.IdempotencyKey
	service.mu.Lock()
	service.err = errors.New("freeze execution inputs failed")
	service.mu.Unlock()

	updated, first := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if first == nil {
		t.Fatal("start form submission did not create a deferred mutation command")
	}
	updated, _ = m.Update(first())
	m = updated.(model)
	if m.taskHubMutation == nil || m.taskHubMutation.IdempotencyKey != key || m.taskHubMutation.Phase != taskHubMutationReady || m.taskHubMutation.Error == "" || m.taskHubMutation.isFrozen() {
		t.Fatalf("failed StartRun input freeze did not retain retryable form: %+v", m.taskHubMutation)
	}
	if m.taskHubMutation.ValueInputs[taskHubExecutionProfilePathField].Value() != "/profiles/explicit.json" || m.taskHubMutation.ValueInputs[taskHubExecutionSpecPathField].Value() != "/specs/frozen.json" || m.taskHubMutation.ValueInputs[taskHubRunTriggerField].Value() != "task_hub" {
		t.Fatalf("failed start mutation lost independent input paths: %+v", m.taskHubMutation.ValueInputs)
	}

	service.mu.Lock()
	service.err = nil
	service.mu.Unlock()
	updated, retry := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if retry == nil {
		t.Fatal("start form retry did not create a deferred input-freeze command")
	}
	updated, _ = m.Update(retry())
	m = updated.(model)
	if m.taskHubMutation == nil || !m.taskHubMutation.isFrozen() || m.taskHubMutation.IdempotencyKey != key {
		t.Fatalf("successful StartRun input freeze did not retain final confirmation: %+v", m.taskHubMutation)
	}
	updated, execute := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if execute == nil {
		t.Fatal("frozen StartRun did not create a deferred execution command")
	}
	updated, _ = m.Update(execute())
	m = updated.(model)
	if m.taskHubMutation != nil {
		t.Fatalf("successful frozen StartRun did not close confirmation: %+v", m.taskHubMutation)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.prepareCommands) != 2 {
		t.Fatalf("StartRun input-freeze attempts = %d, want 2", len(service.prepareCommands))
	}
	for index, request := range service.prepareCommands {
		if request.IdempotencyKey != key || request.Values[taskHubExecutionProfilePathField] != "/profiles/explicit.json" || request.Values[taskHubExecutionSpecPathField] != "/specs/frozen.json" || request.Values[taskHubRunTriggerField] != "task_hub" {
			t.Fatalf("StartRun freeze attempt %d lost retry inputs or idempotency key: %+v", index, request)
		}
	}
	if len(service.mutationCommands) != 1 || service.mutationCommands[0].IdempotencyKey != key || service.mutationCommands[0].PlanID == "" {
		t.Fatalf("frozen StartRun execution = %+v, want one final command with retained plan/key", service.mutationCommands)
	}
}

func TestTaskHubMutationOverlayRejectsInvalidRestoreStateAndKeepsDiffEditable(t *testing.T) {
	restore, err := newTaskHubMutationOverlay(TaskHubActionRestoreTask, TaskHubTarget{}, TaskHubPlanPreview{ConfirmationNeeded: true})
	if err != nil {
		t.Fatal(err)
	}
	restore.ReasonInput.SetValue("restore fixture")
	setTaskHubMutationFormValue(t, restore, taskHubRestoreStateField, "deleted")
	if err := restore.validate(); err == nil || !strings.Contains(err.Error(), "draft") {
		t.Fatalf("invalid restore state error = %v", err)
	}

	edit, err := newTaskHubMutationOverlay(TaskHubActionEditTask, TaskHubTarget{}, TaskHubPlanPreview{ConfirmationNeeded: true})
	if err != nil {
		t.Fatal(err)
	}
	edit.ReasonInput.SetValue("patch fixture")
	edit.focusInput(1)
	if !edit.focusedInputIsMultiline() {
		t.Fatal("unified diff field was not a native multiline input")
	}
	setTaskHubMutationFormValue(t, edit, taskHubUnifiedDiffField, "--- a/task.toml\n+++ b/task.toml\n@@ -1 +1 @@\n-old\n+new\n")
	if err := edit.validate(); err != nil {
		t.Fatalf("valid unified diff form rejected: %v", err)
	}
	if rendered := edit.View(100, 40); !strings.Contains(rendered, "Unified diff") || !strings.Contains(rendered, "--- a/task.toml") || !strings.Contains(rendered, "Ctrl+S") {
		t.Fatalf("manual patch form did not render native multiline input controls:\n%s", rendered)
	}
}

func setTaskHubMutationFormValue(t *testing.T, overlay *TaskHubMutationOverlay, field, value string) {
	t.Helper()
	if input, found := overlay.TextAreaInputs[field]; found {
		input.SetValue(value)
		overlay.TextAreaInputs[field] = input
		return
	}
	input, found := overlay.ValueInputs[field]
	if !found {
		t.Fatalf("form has no input for field %q", field)
	}
	input.SetValue(value)
	overlay.ValueInputs[field] = input
}

type lateReplyTaskHubAdapter struct {
	*AppTaskHubLifecycleAdapter
	failFirstMutationReply bool
}

func (adapter *lateReplyTaskHubAdapter) ExecuteTaskHubMutation(ctx context.Context, request TaskHubMutationRequest) (TaskHubMutationResult, error) {
	result, err := adapter.AppTaskHubLifecycleAdapter.ExecuteTaskHubMutation(ctx, request)
	if err != nil {
		return result, err
	}
	if adapter.failFirstMutationReply {
		adapter.failFirstMutationReply = false
		return result, errors.New("simulated response loss after durable commit")
	}
	return result, nil
}

func openTaskHubReviewConfirmation(t *testing.T, m model) model {
	t.Helper()
	updated, _ := m.Update(runeKey("v"))
	m = updated.(model)
	updated, planCommand := m.Update(runeKey("a"))
	m = updated.(model)
	if planCommand == nil {
		t.Fatal("v a did not request review plan")
	}
	updated, _ = m.Update(planCommand())
	m = updated.(model)
	if m.taskHubPlan == nil || !m.taskHubPlan.ConfirmationNeeded {
		t.Fatalf("review plan was not confirmation-capable: %+v", m.taskHubPlan)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	return m
}

func newTaskHubReviewMutationFixture(t *testing.T) (context.Context, *app.LifecycleServices, store.TaskV2, store.TaskRevision, store.ReviewRequest) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	services, err := newTaskHubAdapterLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{Slug: "tui-review", Title: "TUI Review", Actor: "fixture", Reason: "create review fixture"},
		SourceDirectory:        taskHubAdapterSnapshot(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	revision, err = services.Revisions.MarkValidated(ctx, revision.ID, revision.StateVersion, "sha256:tui-review-evidence", "fixture", "validate review fixture")
	if err != nil {
		t.Fatal(err)
	}
	review, err := services.Reviews.Request(ctx, revision.ID, revision.ValidationEvidenceManifest, "fixture", "request review fixture")
	if err != nil {
		t.Fatal(err)
	}
	return ctx, services, task, revision, review
}

func newTaskHubMultipleReviewMutationFixture(t *testing.T) (context.Context, *app.LifecycleServices, store.TaskV2, store.TaskRevision, []store.ReviewRequest) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	services, err := newTaskHubAdapterLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{Slug: "tui-multiple-reviews", Title: "TUI Multiple Reviews", Actor: "fixture", Reason: "create multiple-review fixture"},
		SourceDirectory:        taskHubAdapterSnapshot(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	revision, err = services.Revisions.MarkValidated(ctx, revision.ID, revision.StateVersion, "sha256:tui-multiple-review-evidence", "fixture", "validate multiple-review fixture")
	if err != nil {
		t.Fatal(err)
	}
	first, err := services.Reviews.Request(ctx, revision.ID, revision.ValidationEvidenceManifest, "fixture", "request first review fixture")
	if err != nil {
		t.Fatal(err)
	}
	second, err := services.Reviews.Request(ctx, revision.ID, revision.ValidationEvidenceManifest, "fixture", "request second review fixture")
	if err != nil {
		t.Fatal(err)
	}
	return ctx, services, task, revision, []store.ReviewRequest{first, second}
}

func newTaskHubLocalPackageMutationFixture(t *testing.T) (context.Context, *app.LifecycleServices, store.TaskV2, store.TaskRevision) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	services, err := newTaskHubAdapterLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{Slug: "tui-package", Title: "TUI Local Package", Actor: "fixture", Reason: "create local package fixture"},
		SourceDirectory:        taskHubAdapterSnapshot(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	revision, err = services.Revisions.MarkValidated(ctx, revision.ID, revision.StateVersion, "sha256:tui-package-evidence", "fixture", "validate local package fixture")
	if err != nil {
		t.Fatal(err)
	}
	review, err := services.Reviews.Request(ctx, revision.ID, revision.ValidationEvidenceManifest, "fixture", "request local package review")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Reviews.Decide(ctx, app.DecideReviewRequest{
		ReviewRequestID:        review.ID,
		RevisionID:             revision.ID,
		Action:                 store.ReviewDecisionApprove,
		ExpectedRevisionDigest: revision.TaskDigest,
		Actor:                  "fixture",
		Reason:                 "approve local package fixture",
	}); err != nil {
		t.Fatal(err)
	}
	task, err = services.Reviews.PromoteCurrent(ctx, task.ID, revision.ID, task.Version, "fixture", "promote local package fixture")
	if err != nil {
		t.Fatal(err)
	}
	return ctx, services, task, revision
}
