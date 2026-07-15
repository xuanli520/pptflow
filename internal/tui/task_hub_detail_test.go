package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type fakeTaskHubDetailLifecycle struct {
	*fakeTaskHubLifecycle
	detail        TaskHubDetail
	detailErr     error
	detailQueries []TaskHubDetailQuery
}

func (service *fakeTaskHubDetailLifecycle) QueryTaskHubDetail(_ context.Context, query TaskHubDetailQuery) (TaskHubDetail, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.detailQueries = append(service.detailQueries, query)
	return service.detail.Clone(), service.detailErr
}

func (service *fakeTaskHubDetailLifecycle) detailQueryCount() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	return len(service.detailQueries)
}

func TestTaskHubDetailKeyboardFlowIsReadOnlyAndRefreshes(t *testing.T) {
	service := &fakeTaskHubDetailLifecycle{
		fakeTaskHubLifecycle: &fakeTaskHubLifecycle{snapshot: enabledTaskHubSnapshot()},
		detail:               taskHubDetailFixture(),
	}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()

	updated, command := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if command == nil || m.taskHubDetail == nil || !m.taskHubDetail.Loading || service.detailQueryCount() != 0 || service.planCallCount() != 0 {
		t.Fatalf("Enter must only open a deferred read-only detail request: overlay=%+v detailQueries=%d plans=%d command=%v", m.taskHubDetail, service.detailQueryCount(), service.planCallCount(), command)
	}
	updated, _ = m.Update(command())
	m = updated.(model)
	if m.taskHubDetail == nil || m.taskHubDetail.Loading || m.taskHubDetail.Detail.Task.TaskID != "task-1" {
		t.Fatalf("detail response was not applied: %+v", m.taskHubDetail)
	}
	if rendered := ansi.Strip(m.View()); !strings.Contains(rendered, "Task 详情") || !strings.Contains(rendered, "可用事实") {
		t.Fatalf("overview detail did not render task facts:\n%s", rendered)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(model)
	if m.taskHubDetail.Tab != TaskHubDetailRevisionsTab || !strings.Contains(ansi.Strip(m.View()), "验证证据") {
		t.Fatalf("Tab did not move to revision facts: tab=%q\n%s", m.taskHubDetail.Tab, ansi.Strip(m.View()))
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(model)
	if m.taskHubDetail.Tab != TaskHubDetailRunsTab || !strings.Contains(ansi.Strip(m.View()), "阶段 1") {
		t.Fatalf("second Tab did not render Run/stage facts: tab=%q\n%s", m.taskHubDetail.Tab, ansi.Strip(m.View()))
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(model)
	factsView := ansi.Strip(m.View())
	if m.taskHubDetail.Tab != TaskHubDetailFactsTab || !strings.Contains(factsView, "工件") || !strings.Contains(factsView, "审核") || !strings.Contains(factsView, "返修") {
		t.Fatalf("facts tab omitted durable evidence/review/repair facts: tab=%q\n%s", m.taskHubDetail.Tab, ansi.Strip(m.View()))
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m = updated.(model)
	if m.taskHubDetail.Scroll == 0 || !strings.Contains(ansi.Strip(m.View()), "uncertain") {
		t.Fatalf("facts tab did not make lower repair receipt facts keyboard-reachable: scroll=%d\n%s", m.taskHubDetail.Scroll, ansi.Strip(m.View()))
	}

	updated, refresh := m.Update(runeKey("r"))
	m = updated.(model)
	if refresh == nil || !m.taskHubDetail.Loading || service.detailQueryCount() != 1 || service.planCallCount() != 0 {
		t.Fatalf("r must only issue another deferred detail read: loading=%v queries=%d plans=%d command=%v", m.taskHubDetail.Loading, service.detailQueryCount(), service.planCallCount(), refresh)
	}
	updated, _ = m.Update(refresh())
	m = updated.(model)
	if m.taskHubDetail.Loading || service.detailQueryCount() != 2 || service.planCallCount() != 0 {
		t.Fatalf("refresh changed a lifecycle action or did not resolve: loading=%v queries=%d plans=%d", m.taskHubDetail.Loading, service.detailQueryCount(), service.planCallCount())
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(model)
	if m.taskHubDetail != nil || service.planCallCount() != 0 {
		t.Fatalf("Esc did not dismiss read-only detail safely: overlay=%+v plans=%d", m.taskHubDetail, service.planCallCount())
	}
}

func TestTaskHubFrozenExecutionTabShowsBoundCatalogAndExactEvaluationLineageReadOnly(t *testing.T) {
	detail := taskHubDetailFixture()
	detail.FrozenExecutions = []TaskHubFrozenExecutionFact{{
		RunID:                       "run-1",
		State:                       TaskHubFrozenExecutionBound,
		TemplateID:                  "harbor.codeedge-phase1",
		TemplateVersion:             "1.0.0",
		ExecutionProfileID:          "codeedge-local",
		ExecutionProfileVersion:     "2026.07",
		ContinuationPlanTTL:         24 * time.Hour,
		ControlGracePeriod:          30 * time.Second,
		TemplateFingerprint:         "sha256:template",
		ProfileFingerprint:          "sha256:profile",
		DefinitionFingerprint:       "sha256:definition",
		ResolvedManifestFingerprint: "sha256:manifest",
		InitialPlanFingerprint:      "sha256:plan",
		InputBundleID:               "bundle-1",
		ExecutionSpecFingerprint:    "sha256:spec",
		DeploymentCatalog: TaskHubDeploymentCatalogFact{
			State: TaskHubDeploymentCatalogBound, CatalogID: "codeedge-phase1-production", CatalogVersion: "2026.07",
			TemplateID: "harbor.codeedge-phase1", TemplateVersion: "1.0.0", CatalogFingerprint: "sha256:catalog",
			LockState: TaskHubDeploymentCatalogLockBound, LockID: "codeedge-phase1-lock", LockVersion: "2026.07", LockFingerprint: "sha256:lock",
		},
	}}
	detail.Runs[0].Stages = append(detail.Runs[0].Stages,
		TaskHubStageFact{StageAttemptID: "evaluation-attempt", StageKey: "harbor_run_qwen", StageGroup: "evaluation", Ordinal: 12, ExecutionState: "completed", Verdict: "pass"},
	)
	detail.Artifacts = append(detail.Artifacts, TaskHubArtifactFact{
		ManifestID: "evaluation-manifest", RevisionID: "revision-1", SubjectDigest: "harbor.task.v2:sha256:fixture", WorkflowFingerprint: "sha256:workflow",
		Refs: []TaskHubArtifactRefFact{
			{ArtifactKey: "qwen-trial-result", ContentDigest: "sha256:qwen-result", SchemaVersion: "codeedge.phase1.evaluation-receipt.v1", RunID: "run-1", StageKey: "harbor_run_qwen", AttemptID: "evaluation-attempt"},
			{ArtifactKey: "qwen-screenshot", ContentDigest: "sha256:qwen-screenshot", SchemaVersion: "image/png", RunID: "run-1", StageKey: "harbor_run_qwen", AttemptID: "evaluation-attempt"},
			// This is deliberately for a different attempt and must not be
			// attached to the selected evaluation summary.
			{ArtifactKey: "stale-evidence", ContentDigest: "sha256:stale", SchemaVersion: "report.v1", RunID: "run-1", StageKey: "harbor_run_qwen", AttemptID: "older-attempt"},
		},
	})
	service := &fakeTaskHubDetailLifecycle{
		fakeTaskHubLifecycle: &fakeTaskHubLifecycle{snapshot: enabledTaskHubSnapshot()},
		detail:               detail,
	}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()
	m.width, m.height = 120, 52

	updated, command := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if command == nil || m.taskHubDetail == nil || service.planCallCount() != 0 {
		t.Fatalf("opening frozen execution detail must remain a deferred read: overlay=%+v plans=%d command=%v", m.taskHubDetail, service.planCallCount(), command)
	}
	updated, _ = m.Update(command())
	m = updated.(model)
	for range []int{0, 1, 2} {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = updated.(model)
	}
	if m.taskHubDetail.Tab != TaskHubDetailFrozenTab {
		t.Fatalf("Tab sequence did not reach discoverable frozen-execution surface: %q", m.taskHubDetail.Tab)
	}
	rendered := ansi.Strip(m.View())
	for _, required := range []string{
		"冻结执行", "冻结 manifest：已解析并与 durable Run 身份一致", "codeedge-phase1-production@2026.07", "codeedge-phase1-lock@2026.07",
		"harbor_run_qwen", "qwen-trial-result", "qwen-screenshot", "受验证评测摘要：尚未提供",
	} {
		if !strings.Contains(rendered, required) {
			t.Fatalf("frozen execution detail omitted %q:\n%s", required, rendered)
		}
	}
	if strings.Contains(rendered, "stale-evidence") {
		t.Fatalf("frozen execution detail attached evidence from a different StageAttempt:\n%s", rendered)
	}
	if service.planCallCount() != 0 {
		t.Fatalf("read-only frozen execution view requested a lifecycle plan: %d", service.planCallCount())
	}
}

func TestTaskHubDetailMouseTabsRowsAndHelpUseV2Surfaces(t *testing.T) {
	service := &fakeTaskHubDetailLifecycle{
		fakeTaskHubLifecycle: &fakeTaskHubLifecycle{snapshot: enabledTaskHubSnapshot()},
		detail:               taskHubDetailFixture(),
	}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()
	m.width, m.height = 110, 32

	clickRenderedMarker(t, &m, "Runs")
	if m.taskHub.Query.Tab != TaskHubRunsTab {
		t.Fatalf("V2 Runs mouse tab was not selected: %q", m.taskHub.Query.Tab)
	}
	clickRenderedMarker(t, &m, shortTaskHubID("run-1"))
	if m.taskHub.SelectedRunID != "run-1" || m.taskHub.SelectedTaskID != "task-1" {
		t.Fatalf("V2 Run row was not selected by mouse: task=%q run=%q", m.taskHub.SelectedTaskID, m.taskHub.SelectedRunID)
	}
	command := clickRenderedMarker(t, &m, "[Enter/d 详情]")
	if command == nil || m.taskHubDetail == nil || service.detailQueryCount() != 0 {
		t.Fatalf("detail footer mouse target was not deferred: overlay=%+v queries=%d command=%v", m.taskHubDetail, service.detailQueryCount(), command)
	}
	updated, _ := m.Update(command())
	m = updated.(model)
	if m.taskHubDetail == nil || m.taskHubDetail.Detail.SelectedRunID != "run-1" {
		t.Fatalf("Run detail did not retain selected Run context: %+v", m.taskHubDetail)
	}
	clickRenderedMarker(t, &m, "修订")
	if m.taskHubDetail.Tab != TaskHubDetailRevisionsTab {
		t.Fatalf("detail mouse tab did not select revisions: %q", m.taskHubDetail.Tab)
	}
	clickRenderedMarker(t, &m, "冻结执行")
	if m.taskHubDetail.Tab != TaskHubDetailFrozenTab {
		t.Fatalf("detail mouse tab did not expose frozen execution facts: %q", m.taskHubDetail.Tab)
	}
	clickRenderedMarker(t, &m, "[Esc] 返回")
	if m.taskHubDetail != nil {
		t.Fatal("detail return mouse target did not close overlay")
	}

	updated, _ = m.Update(runeKey("?"))
	m = updated.(model)
	help := ansi.Strip(m.View())
	if !m.taskHubHelpVisible || !strings.Contains(help, "两键计划与确认") || !strings.Contains(help, "UUIDv7 幂等键") || !strings.Contains(help, "[R] 本地 reconcile") {
		t.Fatalf("V2 Task Hub help was not rendered:\n%s", help)
	}
	if strings.Contains(help, "Ctrl+O") || strings.Contains(help, "工作区") {
		t.Fatalf("V2 Task Hub help leaked legacy workspace instructions:\n%s", help)
	}
	clickRenderedMarker(t, &m, "[? / Esc / q] 关闭帮助")
	if m.taskHubHelpVisible {
		t.Fatal("V2 Task Hub help close target did not dismiss help")
	}
}

func TestTaskHubDetailAndHelpFitNarrowTerminal(t *testing.T) {
	overlay := newTaskHubDetailOverlay(TaskHubDetailQuery{TaskID: "task-1"})
	overlay.Loading = false
	overlay.Detail = taskHubDetailFixture()
	for _, size := range []struct{ width, height int }{{20, 10}, {40, 16}} {
		for _, tab := range taskHubDetailTabs() {
			overlay.Tab = tab
			overlay.Scroll = 0
			rendered := overlay.View(size.width, size.height)
			assertRenderedWidth(t, "TaskHubDetail/"+string(tab), rendered, size.width)
			if got := len(strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")); got > size.height {
				t.Fatalf("TaskHubDetail/%s exceeded %dx%d terminal height with %d lines:\n%s", tab, size.width, size.height, got, ansi.Strip(rendered))
			}
		}
		help := (taskHubHelpOverlay{}).View(size.width, size.height)
		assertRenderedWidth(t, "TaskHubHelp", help, size.width)
		if got := len(strings.Split(strings.TrimSuffix(help, "\n"), "\n")); got > size.height {
			t.Fatalf("TaskHubHelp exceeded %dx%d terminal height with %d lines:\n%s", size.width, size.height, got, ansi.Strip(help))
		}
	}
}

func TestTaskHubDetailSelectionCapturesSpecificOpenReviewForTwoKeyPlan(t *testing.T) {
	snapshot := enabledTaskHubSnapshot()
	snapshot.Tasks[0].Actions = append(snapshot.Tasks[0].Actions,
		TaskHubActionState{Action: TaskHubActionApproveReview, DisabledReason: "需要唯一且打开的 ReviewRequest"},
		TaskHubActionState{Action: TaskHubActionRequestChanges, DisabledReason: "需要唯一且打开的 ReviewRequest"},
		TaskHubActionState{Action: TaskHubActionRejectReview, DisabledReason: "需要唯一且打开的 ReviewRequest"},
	)
	detail := taskHubDetailFixture()
	detail.Reviews = []TaskHubReviewFact{
		{ReviewRequestID: "review-open-a", RevisionID: "revision-1", State: "open", EvidenceManifest: "sha256:evidence-a"},
		{ReviewRequestID: "review-open-b", RevisionID: "revision-1", State: "open", EvidenceManifest: "sha256:evidence-b"},
	}
	service := &fakeTaskHubDetailLifecycle{
		fakeTaskHubLifecycle: &fakeTaskHubLifecycle{
			snapshot: snapshot,
			plan:     TaskHubPlanPreview{Title: "审核通过", Summary: "记录已选 ReviewRequest", ConfirmationNeeded: true},
		},
		detail: detail,
	}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()

	updated, detailCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if detailCommand == nil || m.taskHubDetail == nil {
		t.Fatalf("did not open deferred detail: overlay=%+v command=%v", m.taskHubDetail, detailCommand)
	}
	updated, _ = m.Update(detailCommand())
	m = updated.(model)
	m.taskHubDetail.Tab = TaskHubDetailFactsTab
	updated, _ = m.Update(runeKey("]"))
	m = updated.(model)
	selectedID := m.taskHub.SelectedReviewRequestID
	if selectedID != "review-open-a" || m.taskHub.SelectedReviewRevisionID != "revision-1" {
		t.Fatalf("detail did not capture the first stable open review target: %+v", m.taskHub)
	}
	if rendered := strings.Join(m.taskHubDetail.factRows(), "\n"); !strings.Contains(rendered, "> review："+selectedID) {
		t.Fatalf("selected review was not marked in detail:\n%s", rendered)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(model)
	if m.taskHubDetail != nil {
		t.Fatal("Esc did not close detail after a local-only selection")
	}
	updated, _ = m.Update(runeKey("v"))
	m = updated.(model)
	updated, planCommand := m.Update(runeKey("a"))
	m = updated.(model)
	if planCommand == nil || service.planCallCount() != 0 {
		t.Fatalf("selected review did not enable a deferred two-key plan: command=%v plans=%d", planCommand, service.planCallCount())
	}
	updated, _ = m.Update(planCommand())
	m = updated.(model)
	if service.planCallCount() != 1 || m.taskHubPlanCommand == nil || m.taskHubPlanCommand.Target.ReviewRequestID != selectedID || m.taskHubPlanCommand.Target.ReviewRevisionID != "revision-1" {
		t.Fatalf("plan did not capture selected review identity: calls=%d command=%+v", service.planCallCount(), m.taskHubPlanCommand)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if m.taskHubMutation == nil || m.taskHubMutation.Target.ReviewRequestID != selectedID || m.taskHubMutation.Target.ReviewRevisionID != "revision-1" {
		t.Fatalf("native confirmation was rebound away from selected review: %+v", m.taskHubMutation)
	}
}

func TestTaskHubDetailSelectionCapturesSourceSessionReviewAndAuthoringRunTarget(t *testing.T) {
	snapshot := enabledTaskHubSnapshot()
	detail := taskHubDetailFixture()
	detail.AuthoringReviews = []TaskHubAuthoringReviewFact{{
		ReviewRequestID: "authoring-review-open", BindingID: "authoring-binding", RunID: "authoring-run", AuthoringSessionID: "authoring-session",
		AuthoringSourceID: "authoring-source", SourceSnapshotDigest: "sha256:source", DefinitionHash: "sha256:definition",
		StageAttemptID: "authoring-stage", StageKey: "task_review", ReviewKind: "task_direction", InputFingerprint: "sha256:input",
		EvidenceManifest: "sha256:evidence", State: "open",
	}}
	service := &fakeTaskHubDetailLifecycle{fakeTaskHubLifecycle: &fakeTaskHubLifecycle{snapshot: snapshot}, detail: detail}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()

	updated, detailCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if detailCommand == nil {
		t.Fatal("did not open deferred detail for source/session review")
	}
	updated, _ = m.Update(detailCommand())
	m = updated.(model)
	m.taskHubDetail.Tab = TaskHubDetailFactsTab
	updated, _ = m.Update(runeKey("]"))
	m = updated.(model)
	if m.taskHub.SelectedAuthoringReviewRequestID != "authoring-review-open" || m.taskHub.SelectedAuthoringReviewRunID != "authoring-run" ||
		m.taskHub.SelectedReviewRequestID != "" {
		t.Fatalf("detail did not preserve an explicit source/session review selection: %+v", m.taskHub)
	}
	target := m.taskHubTarget()
	if target.TaskID != "task-1" || target.RunID != "authoring-run" || target.AuthoringReviewRequestID != "authoring-review-open" ||
		target.RevisionID != "" || target.ReviewRequestID != "" {
		t.Fatalf("Task Hub target manufactured a TaskRevision for authoring review: %+v", target)
	}
	if rendered := strings.Join(m.taskHubDetail.factRows(), "\n"); !strings.Contains(rendered, "> authoring review：authoring-review-open") ||
		!strings.Contains(rendered, "session：authoring-session") {
		t.Fatalf("source/session gate was not visibly selected in detail:\n%s", rendered)
	}
}

func TestTaskHubDetailSelectionEnablesWithdrawWithCapturedRelease(t *testing.T) {
	snapshot := enabledTaskHubSnapshot()
	snapshot.Tasks[0].Actions = append(snapshot.Tasks[0].Actions,
		TaskHubActionState{Action: TaskHubActionWithdrawRelease, DisabledReason: "需要明确的 release ID 和幂等撤回契约"},
	)
	detail := taskHubDetailFixture()
	detail.Releases = []TaskHubReleaseFact{
		{ReleaseID: "release-active-a", ReleaseVersion: "v1", RevisionID: "revision-1"},
		{ReleaseID: "release-withdrawn", ReleaseVersion: "v0", RevisionID: "revision-1", WithdrawnAt: time.Now().UTC()},
	}
	service := &fakeTaskHubDetailLifecycle{fakeTaskHubLifecycle: &fakeTaskHubLifecycle{snapshot: snapshot}, detail: detail}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()

	updated, detailCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	updated, _ = m.Update(detailCommand())
	m = updated.(model)
	m.taskHubDetail.Tab = TaskHubDetailReleasesTab
	updated, _ = m.Update(runeKey("]"))
	m = updated.(model)
	if m.taskHub.SelectedReleaseID != "release-active-a" || m.taskHubTarget().ReleaseID != "release-active-a" {
		t.Fatalf("detail did not capture active release identity: state=%+v target=%+v", m.taskHub, m.taskHubTarget())
	}
	if rendered := strings.Join(m.taskHubDetail.releaseRows(), "\n"); !strings.Contains(rendered, "> v1") {
		t.Fatalf("selected release was not marked in detail:\n%s", rendered)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(model)
	updated, _ = m.Update(runeKey("p"))
	m = updated.(model)
	updated, command := m.Update(runeKey("w"))
	m = updated.(model)
	if command == nil {
		t.Fatal("selected active release did not enter the withdraw plan path")
	}
	updated, _ = m.Update(command())
	m = updated.(model)
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.commands) != 1 || service.commands[0].Action != TaskHubActionWithdrawRelease || service.commands[0].Target.ReleaseID != "release-active-a" {
		t.Fatalf("withdraw plan did not retain selected release identity: commands=%+v", service.commands)
	}
}

func TestTaskHubDetailRefreshDropsClosedSelectedReview(t *testing.T) {
	detail := taskHubDetailFixture()
	detail.Reviews = []TaskHubReviewFact{{ReviewRequestID: "review-open", RevisionID: "revision-1", State: "open"}}
	overlay := newTaskHubDetailOverlay(TaskHubDetailQuery{TaskID: "task-1"})
	overlay.Loading = false
	overlay.Detail = detail
	overlay.SelectedReviewRequestID = "review-open"
	service := &fakeTaskHubDetailLifecycle{fakeTaskHubLifecycle: &fakeTaskHubLifecycle{snapshot: enabledTaskHubSnapshot()}, detail: detail}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()
	m.taskHubDetail = overlay
	m.taskHub.SelectedReviewTaskID = "task-1"
	m.taskHub.SelectedReviewRequestID = "review-open"
	m.taskHub.SelectedReviewRevisionID = "revision-1"

	m.taskHubDetail.Detail.Reviews[0].State = "closed"
	m.syncTaskHubDetailSelections()
	if m.taskHub.SelectedReviewRequestID != "" || m.taskHub.SelectedReviewRevisionID != "" || m.taskHubDetail.SelectedReviewRequestID != "" {
		t.Fatalf("refresh retained a no-longer-open review selection: state=%+v overlay=%+v", m.taskHub, m.taskHubDetail)
	}
}

func taskHubDetailFixture() TaskHubDetail {
	now := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	return TaskHubDetail{
		Task: TaskHubDetailTask{
			TaskID: "task-1", Slug: "durable-task", Name: "durable-task", Lifecycle: "ready", CurrentRevisionID: "revision-1",
			SourceRepo: "https://example.invalid/harbor", SourceCommit: "abc123", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now,
		},
		SelectedRunID: "run-1",
		Revisions: []TaskHubRevisionFact{{
			RevisionID: "revision-1", VersionNumber: 1, Origin: "imported", State: "validated", TaskDigest: "harbor.task.v2:sha256:fixture",
			ValidationEvidenceManifest: "sha256:evidence", ChangeSummary: "initial immutable snapshot", Current: true, CreatedAt: now.Add(-time.Hour), StateUpdatedAt: now,
		}},
		Runs: []TaskHubRunFact{{
			RunID: "run-1", RevisionID: "revision-1", Status: "paused", Trigger: "fixture", ExecutionEpoch: 1, WorkflowTemplateID: "standard", WorkflowTemplateVer: "1", CreatedAt: now.Add(-30 * time.Minute),
			Stages: []TaskHubStageFact{{StageAttemptID: "stage-1", StageKey: "quality", StageGroup: "verify", Ordinal: 1, ExecutionState: "completed", Verdict: "pass", ArtifactManifestID: "manifest-1", CreatedAt: now.Add(-20 * time.Minute)}},
		}},
		Releases:   []TaskHubReleaseFact{{ReleaseID: "release-1", ReleaseVersion: "v1", RevisionID: "revision-1", TaskDigest: "harbor.task.v2:sha256:fixture", EvidenceRef: "sha256:evidence", PublishedAt: now.Add(-10 * time.Minute)}},
		Artifacts:  []TaskHubArtifactFact{{ManifestID: "manifest-1", RevisionID: "revision-1", SubjectDigest: "harbor.task.v2:sha256:fixture", WorkflowFingerprint: "sha256:workflow", CreatedAt: now.Add(-20 * time.Minute), Refs: []TaskHubArtifactRefFact{{ArtifactKey: "quality-report", ContentDigest: "sha256:artifact", SchemaVersion: "report.v1", RunID: "run-1", StageKey: "quality", AttemptID: "stage-1"}}}},
		Reviews:    []TaskHubReviewFact{{ReviewRequestID: "review-1", RevisionID: "revision-1", State: "closed", EvidenceManifest: "sha256:evidence", CreatedAt: now.Add(-45 * time.Minute), Decisions: []TaskHubReviewDecisionFact{{DecisionID: "decision-1", Action: "approve", ExpectedRevisionDigest: "harbor.task.v2:sha256:fixture", CreatedAt: now.Add(-40 * time.Minute)}}}},
		Repairs:    []TaskHubRepairFact{{RepairSessionID: "repair-1", SubjectID: "task-1", BaseRevisionID: "revision-1", Status: "needs_human", MaxRounds: 2, CreatedAt: now.Add(-15 * time.Minute), UpdatedAt: now.Add(-5 * time.Minute), Changes: []TaskHubRepairChangeFact{{PreparedChangeID: "change-1", RoundOrdinal: 1, ProviderID: "local-patch", BeforeDigest: "sha256:before", AfterDigest: "sha256:after", CreatedAt: now.Add(-10 * time.Minute), Receipts: []TaskHubMutationReceiptFact{{ReceiptID: "receipt-1", Outcome: "uncertain", CreatedAt: now.Add(-5 * time.Minute)}}}}}},
		ObservedAt: now,
	}
}
