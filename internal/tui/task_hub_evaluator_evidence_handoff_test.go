package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

func TestTaskHubCodeEdgeEvaluatorEvidenceHandoffShortcutFreezesAndRetriesSameKey(t *testing.T) {
	const childRunID = "child-evaluator-run"
	service := &taskHubEvidenceHandoffLostReplyLifecycle{
		fakeTaskHubLifecycle: &fakeTaskHubLifecycle{
			snapshot: TaskHubSnapshot{
				Tasks: []TaskHubTask{{TaskID: "task-evaluator", RevisionID: "revision-evaluator"}},
				Runs: []TaskHubRun{{
					RunID: childRunID, TaskID: "task-evaluator", RevisionID: "revision-evaluator", ExecutionState: "succeeded",
					Actions: []TaskHubActionState{{Action: TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff, Enabled: true}},
				}},
			},
			plan: TaskHubPlanPreview{
				Title: "采用 CodeEdge 评测证据", Summary: "确认 child 到 parent 的不可变证据交接。", ConfirmationNeeded: true,
				Expected: TaskHubLifecycleCheckpoint{TaskID: "task-evaluator", RevisionID: "revision-evaluator", RunID: "parent-phase1-run", RunVersion: 3, RunDefinitionHash: "sha256:parent-definition"},
			},
			prepared: TaskHubPlanPreview{
				PlanID: "codeedge-evaluator-evidence-handoff:frozen", Title: "采用 CodeEdge 评测证据（交接已冻结）",
				Summary: "parent/child 和 pass@4 证据已冻结。", ConfirmationNeeded: true,
			},
		},
		failFirstReply: true,
	}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()
	m.taskHub.SelectedTaskID = "task-evaluator"
	m.taskHub.SelectedRunID = childRunID

	updated, prefixCmd := m.Update(runeKey("x"))
	m = updated.(model)
	if prefixCmd == nil || !strings.Contains(m.taskHubPrefixHint(), "x h 采用 CodeEdge 评测证据") {
		t.Fatalf("x prefix did not expose evidence handoff hint: hint=%q command=%v", m.taskHubPrefixHint(), prefixCmd)
	}
	updated, planCmd := m.Update(runeKey("h"))
	m = updated.(model)
	if planCmd == nil || service.planCallCount() != 0 {
		t.Fatalf("x h did not defer evidence-handoff planning: calls=%d command=%v", service.planCallCount(), planCmd)
	}
	updated, _ = m.Update(planCmd())
	m = updated.(model)
	if m.taskHubPlan == nil || m.taskHubPlanCommand == nil || m.taskHubPlanCommand.Action != TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff ||
		m.taskHubPlanCommand.Target.RunID != childRunID || !m.taskHubPlan.ConfirmationNeeded {
		t.Fatalf("evidence-handoff plan did not bind selected child Run: plan=%+v command=%+v", m.taskHubPlan, m.taskHubPlanCommand)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if m.taskHubMutation == nil || m.taskHubMutation.Action != TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff {
		t.Fatalf("evidence-handoff confirmation form was not opened: %+v", m.taskHubMutation)
	}
	m.taskHubMutation.ReasonInput.SetValue("adopt verified evaluator child evidence")
	key := m.taskHubMutation.IdempotencyKey
	if err := store.ValidateUUIDv7(key); err != nil {
		t.Fatalf("TUI evidence-handoff idempotency key %q is not UUIDv7: %v", key, err)
	}

	updated, prepareCmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if prepareCmd == nil || service.prepareCallCount() != 0 {
		t.Fatalf("first evidence-handoff confirmation did not defer prepare: calls=%d command=%v", service.prepareCallCount(), prepareCmd)
	}
	updated, _ = m.Update(prepareCmd())
	m = updated.(model)
	if m.taskHubMutation == nil || !m.taskHubMutation.isFrozen() || m.taskHubMutation.IdempotencyKey != key ||
		m.taskHubMutation.FrozenReason != "adopt verified evaluator child evidence" {
		t.Fatalf("evidence-handoff prepare did not freeze original provenance: %+v", m.taskHubMutation)
	}
	if prepared := service.lastPrepareCommand(); prepared.Action != TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff || prepared.Target.RunID != childRunID ||
		prepared.IdempotencyKey != key || prepared.Reason != "adopt verified evaluator child evidence" {
		t.Fatalf("evidence-handoff prepare command = %+v", prepared)
	}

	updated, firstExecute := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if firstExecute == nil {
		t.Fatal("frozen evidence-handoff form did not defer final confirmation")
	}
	updated, _ = m.Update(firstExecute())
	m = updated.(model)
	if m.taskHubMutation == nil || m.taskHubMutation.IdempotencyKey != key || !m.taskHubMutation.isFrozen() || m.taskHubMutation.Error == "" {
		t.Fatalf("lost evidence-handoff reply did not keep frozen retry form: %+v", m.taskHubMutation)
	}
	if service.durableCommitCount() != 1 {
		t.Fatalf("first evidence-handoff final confirmation durable commits = %d, want 1", service.durableCommitCount())
	}

	updated, retryExecute := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if retryExecute == nil {
		t.Fatal("evidence-handoff retry did not defer final confirmation")
	}
	updated, _ = m.Update(retryExecute())
	m = updated.(model)
	if m.taskHubMutation != nil {
		t.Fatalf("idempotent evidence-handoff retry did not close form: %+v", m.taskHubMutation)
	}
	if service.durableCommitCount() != 1 || service.mutationCallCount() != 2 {
		t.Fatalf("evidence-handoff retry commits=%d calls=%d, want one durable commit and two same-key submissions", service.durableCommitCount(), service.mutationCallCount())
	}
	first, second := service.mutationCommandAt(0), service.mutationCommandAt(1)
	if first.Action != TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff || second.Action != first.Action || first.IdempotencyKey != key || second.IdempotencyKey != key ||
		first.PlanID == "" || second.PlanID != first.PlanID || first.Target.RunID != childRunID || second.Target.RunID != childRunID {
		t.Fatalf("evidence-handoff retry changed frozen command identity: first=%+v second=%+v", first, second)
	}
}

func TestTaskHubCodeEdgeEvaluatorEvidenceHandoffDisabledStateStopsAtSecondKey(t *testing.T) {
	service := &fakeTaskHubLifecycle{snapshot: TaskHubSnapshot{
		Tasks: []TaskHubTask{{TaskID: "task-evaluator", RevisionID: "revision-evaluator"}},
		Runs: []TaskHubRun{{
			RunID: "child-evaluator-run", TaskID: "task-evaluator", RevisionID: "revision-evaluator", ExecutionState: "running",
			Actions: []TaskHubActionState{{
				Action: TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff, DisabledReason: "只有已成功完成的 CodeEdge evaluator child Run 可以采用证据",
			}},
		}},
	}}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()
	m.taskHub.SelectedTaskID = "task-evaluator"
	m.taskHub.SelectedRunID = "child-evaluator-run"

	updated, _ := m.Update(runeKey("x"))
	m = updated.(model)
	hint := m.taskHubPrefixHint()
	if !strings.Contains(hint, "x h 采用 CodeEdge 评测证据") || !strings.Contains(hint, "只有已成功完成") {
		t.Fatalf("disabled evidence-handoff hint = %q", hint)
	}
	updated, disabledCmd := m.Update(runeKey("h"))
	m = updated.(model)
	if disabledCmd == nil || service.planCallCount() != 0 || !strings.Contains(m.notice, "只有已成功完成") {
		t.Fatalf("disabled x h planned or lost reason: plans=%d notice=%q command=%v", service.planCallCount(), m.notice, disabledCmd)
	}
}

func TestAppTaskHubProjectsEvidenceHandoffActionOnlyForEvaluatorChild(t *testing.T) {
	ctx, services, provider, task, revision, parent := newTaskHubCodeEdgeEvaluatorFixture(t)
	child, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, ParentRunID: parent.ID,
		Profile: provider.profile.Clone(), ExecutionSpec: provider.spec.Clone(),
		Trigger: "task-hub-evidence-handoff-action", Actor: "tester", Reason: "create child action projection fixture",
	})
	if err != nil {
		t.Fatalf("start CodeEdge evaluator child fixture: %v", err)
	}
	adapter := NewAppTaskHubLifecycleAdapter(services)
	snapshot, err := adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubRunsTab})
	if err != nil {
		t.Fatal(err)
	}
	parentProjection := taskHubRunProjectionByID(t, snapshot, parent.ID)
	if _, found := taskHubActionStateFor(parentProjection.Actions, TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff); found {
		t.Fatalf("Phase-1 parent exposed child-only evidence-handoff action: %+v", parentProjection.Actions)
	}
	childProjection := taskHubRunProjectionByID(t, snapshot, child.ID)
	action, found := taskHubActionStateFor(childProjection.Actions, TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff)
	if !found || action.Enabled || !strings.Contains(action.DisabledReason, "已成功完成") {
		t.Fatalf("queued evaluator child handoff action = %+v, want visible disabled completed-child requirement", action)
	}

	child, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: child.ID, ExpectedVersion: child.Version, Status: store.WorkflowRunRunning,
		Actor: "tester", Reason: "start child action projection fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: child.ID, ExpectedVersion: child.Version, Status: store.WorkflowRunSucceeded,
		Actor: "tester", Reason: "complete child action projection fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubRunsTab})
	if err != nil {
		t.Fatal(err)
	}
	childProjection = taskHubRunProjectionByID(t, snapshot, child.ID)
	action, found = taskHubActionStateFor(childProjection.Actions, TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff)
	if !found || action.Enabled || strings.Contains(action.DisabledReason, "fixture") || strings.Contains(action.DisabledReason, "create child") {
		t.Fatalf("completed evaluator child handoff action = %+v, want visible safe disabled preflight", action)
	}

	plan, err := adapter.PlanTaskHubCommand(ctx, TaskHubCommand{
		Action: TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff,
		Target: TaskHubTarget{TaskID: task.ID, RevisionID: revision.ID, RunID: child.ID},
	})
	if err != nil || plan.ConfirmationNeeded || strings.Contains(plan.Reason, "fixture") || strings.Contains(plan.Reason, "create child") {
		t.Fatalf("completed child evidence-handoff preflight = %+v, %v; want a safe unavailable plan", plan, err)
	}
	expected, err := adapter.captureTaskHubMutationCheckpoint(ctx, services, task.ID, revision.ID, parent.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	request := TaskHubMutationRequest{
		Action:   TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff,
		Target:   TaskHubTarget{TaskID: task.ID, RevisionID: revision.ID, RunID: child.ID},
		Expected: expected, IdempotencyKey: mustTaskHubCodeEdgeUUID(t), Actor: "tester", Reason: "assert durable handoff preparation is mandatory",
	}
	if _, err := adapter.PrepareTaskHubMutation(ctx, request); err == nil || strings.Contains(err.Error(), "fixture") || strings.Contains(err.Error(), "create child") {
		t.Fatalf("invalid child evidence prepare = %v, want safe application preflight rejection", err)
	}
	if _, err := adapter.ExecuteTaskHubMutation(ctx, request); err == nil || !strings.Contains(err.Error(), "第一步冻结确认") {
		t.Fatalf("direct evidence adoption execution = %v, want mandatory durable prepare rejection", err)
	}
}

type taskHubEvidenceHandoffLostReplyLifecycle struct {
	*fakeTaskHubLifecycle
	failFirstReply bool
	durableKeys    map[string]struct{}
}

func (service *taskHubEvidenceHandoffLostReplyLifecycle) ExecuteTaskHubMutation(_ context.Context, command TaskHubMutationRequest) (TaskHubMutationResult, error) {
	service.mu.Lock()
	service.mutationCommands = append(service.mutationCommands, command.Clone())
	if service.durableKeys == nil {
		service.durableKeys = make(map[string]struct{})
	}
	_, completed := service.durableKeys[command.IdempotencyKey]
	if !completed {
		service.durableKeys[command.IdempotencyKey] = struct{}{}
	}
	fail := service.failFirstReply
	service.failFirstReply = false
	service.mu.Unlock()

	result := TaskHubMutationResult{
		Action: command.Action, Target: command.Target, PlanID: command.PlanID,
		ReceiptID: "evaluator-evidence-handoff-receipt", Summary: "已采用并验证 CodeEdge evaluator child 证据",
	}
	if fail {
		return result, errors.New("simulated response loss after durable evidence handoff")
	}
	return result, nil
}

func (service *taskHubEvidenceHandoffLostReplyLifecycle) durableCommitCount() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	return len(service.durableKeys)
}

func (service *taskHubEvidenceHandoffLostReplyLifecycle) mutationCommandAt(index int) TaskHubMutationRequest {
	service.mu.Lock()
	defer service.mu.Unlock()
	if index < 0 || index >= len(service.mutationCommands) {
		return TaskHubMutationRequest{}
	}
	return service.mutationCommands[index].Clone()
}

func (service *taskHubEvidenceHandoffLostReplyLifecycle) lastPrepareCommand() TaskHubMutationRequest {
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.prepareCommands) == 0 {
		return TaskHubMutationRequest{}
	}
	return service.prepareCommands[len(service.prepareCommands)-1].Clone()
}

var _ TaskHubLifecycleService = (*taskHubEvidenceHandoffLostReplyLifecycle)(nil)
var _ TaskHubMutationPlanner = (*taskHubEvidenceHandoffLostReplyLifecycle)(nil)
var _ TaskHubMutationExecutor = (*taskHubEvidenceHandoffLostReplyLifecycle)(nil)
