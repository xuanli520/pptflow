package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
)

func TestTaskHubCodeEdgeEvidenceRowsUseExactImmutableRefsAndWarnFailClosed(t *testing.T) {
	detail := taskHubCodeEdgeEvidenceDetailFixture()
	overlay := newTaskHubDetailOverlay(TaskHubDetailQuery{TaskID: detail.Task.TaskID, RunID: detail.SelectedRunID})
	overlay.Loading = false
	overlay.Detail = detail

	rendered := strings.Join(overlay.evaluationEvidenceRows(detail.SelectedRunID), "\n")
	for _, required := range []string{
		"CodeEdge Harbor 评测证据（immutable lineage）",
		"Run 状态：in_doubt；需 reconcile；TUI 不会继续或自动重跑。",
		"Qwen StageAttempt 状态：interrupted；需 reconcile；TUI 不会继续或自动重跑。",
		"Qwen Harbor 运行证据包状态：已登记（qwen_trial_result  sha256:qwen-bundle  " + codeedge.HarborRunBundleV018Format + "）",
		"Qwen 截图状态：已登记（qwen_pass4_evidence  sha256:qwen-screenshot  image/png）",
		"Opus Harbor 运行证据包状态：已登记（opus_trial_result  sha256:opus-bundle  " + codeedge.HarborRunBundleV018Format + "）",
		"Opus 截图状态：已登记（opus_pass4_evidence  sha256:opus-screenshot  image/png）",
		"不会读取 Harbor result、截图或 artifact 原文；上述状态仅表示 immutable ref 是否已登记。",
	} {
		if !strings.Contains(rendered, required) {
			t.Fatalf("CodeEdge evidence rows omitted %q:\n%s", required, rendered)
		}
	}
	for _, unexpected := range []string{
		"sha256:qwen-stale-attempt",
		"sha256:qwen-wrong-stage",
		"sha256:opus-wrong-run",
		"登记了 2 条 qwen_trial_result",
		"登记了 2 条 qwen_pass4_evidence",
		"登记了 2 条 opus_trial_result",
	} {
		if strings.Contains(rendered, unexpected) {
			t.Fatalf("CodeEdge evidence rows accepted stale or mismatched ref %q:\n%s", unexpected, rendered)
		}
	}
}

func TestTaskHubCodeEdgeParentShowsHandoffInsteadOfChildOwnedQwenOpusStages(t *testing.T) {
	detail := taskHubDetailFixture()
	parent := &detail.Runs[0]
	parent.WorkflowTemplateID = workflowadapter.CodeEdgePhase1WorkflowTemplateID
	parent.WorkflowTemplateVer = workflowadapter.CodeEdgePhase1WorkflowTemplateVersion
	// Deliberately retain a stale-looking Qwen stage: parent rendering must not
	// reinterpret it as current evaluator evidence after the child split.
	parent.Stages = []TaskHubStageFact{{
		StageAttemptID: "stale-parent-qwen", StageKey: workflowadapter.HarborRunQwen, StageGroup: "evaluation", ExecutionState: string(store.StageExecutionCompleted),
	}}
	detail.CodeEdgeEvaluatorEvidenceHandoffs = []TaskHubCodeEdgeEvaluatorEvidenceHandoffFact{{
		ParentRunID: parent.RunID, State: TaskHubCodeEdgeEvaluatorEvidenceHandoffRecorded,
		HandoffID: "019f6207-2345-7000-8000-000000000001", ChildRunID: "019f6207-2345-7000-8000-000000000002",
		HandoffFingerprint: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}}
	overlay := newTaskHubDetailOverlay(TaskHubDetailQuery{TaskID: detail.Task.TaskID, RunID: parent.RunID})
	overlay.Loading = false
	overlay.Detail = detail
	rendered := strings.Join(overlay.evaluationEvidenceRows(parent.RunID), "\n")
	for _, required := range []string{"CodeEdge evaluator evidence handoff", "evaluator child Run：019f6207-2345-7000-8000-000000000002", "handoff fingerprint：sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"} {
		if !strings.Contains(rendered, required) {
			t.Fatalf("parent handoff rows omitted %q:\n%s", required, rendered)
		}
	}
	if strings.Contains(rendered, "stale-parent-qwen") || strings.Contains(rendered, "Qwen Harbor 运行") {
		t.Fatalf("parent evidence rows rendered child-owned evaluator stages:\n%s", rendered)
	}
}

func TestTaskHubCodeEdgeEvidenceDetailOnlyReadsAndNeverPlansLifecycleAction(t *testing.T) {
	service := &fakeTaskHubDetailLifecycle{
		fakeTaskHubLifecycle: &fakeTaskHubLifecycle{snapshot: enabledTaskHubSnapshot()},
		detail:               taskHubCodeEdgeEvidenceDetailFixture(),
	}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()
	m.width, m.height = 160, 60

	updated, load := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if load == nil || m.taskHubDetail == nil || service.planCallCount() != 0 {
		t.Fatalf("opening CodeEdge evidence detail = overlay=%+v plans=%d command=%v, want deferred read only", m.taskHubDetail, service.planCallCount(), load)
	}
	updated, _ = m.Update(load())
	m = updated.(model)
	if m.taskHubDetail == nil || m.taskHubDetail.Loading || service.detailQueryCount() != 1 || service.planCallCount() != 0 {
		t.Fatalf("CodeEdge evidence detail load = overlay=%+v reads=%d plans=%d", m.taskHubDetail, service.detailQueryCount(), service.planCallCount())
	}
	m.taskHubDetail.Tab = TaskHubDetailFrozenTab
	rendered := ansi.Strip(m.taskHubDetail.View(m.width, m.height))
	if !strings.Contains(rendered, "CodeEdge Harbor 评测证据") || !strings.Contains(rendered, "TUI 不会继续或自动重跑") {
		t.Fatalf("read-only CodeEdge evidence detail did not render fail-closed state:\n%s", rendered)
	}

	updated, refresh := m.Update(runeKey("r"))
	m = updated.(model)
	if refresh == nil || !m.taskHubDetail.Loading || service.planCallCount() != 0 {
		t.Fatalf("CodeEdge evidence refresh = loading=%v plans=%d command=%v, want another deferred read only", m.taskHubDetail.Loading, service.planCallCount(), refresh)
	}
	updated, _ = m.Update(refresh())
	m = updated.(model)
	if m.taskHubDetail.Loading || service.detailQueryCount() != 2 || service.planCallCount() != 0 {
		t.Fatalf("CodeEdge evidence refresh = loading=%v reads=%d plans=%d, want only read requests", m.taskHubDetail.Loading, service.detailQueryCount(), service.planCallCount())
	}
}

func TestAppTaskHubDetailAdapterProjectsCodeEdgeEvidenceFromSQLite(t *testing.T) {
	ctx, services, task, revision := newTaskHubLocalPackageMutationFixture(t)
	parent := taskHubCreateCodeEdgePackageRun(t, ctx, services, task, revision)
	run, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, ParentRunID: parent.ID,
		Profile:       taskHubAdapterCompleteProfileForTemplate(t, workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplate()),
		ExecutionSpec: testsupport.CompleteCodeEdgeEvaluatorChildRunExecutionSpec(task.ID, revision.ID, revision.TaskDigest),
		Trigger:       "task-hub-codeedge-evidence-child", Actor: "tester", Reason: "create CodeEdge evaluator child evidence fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning,
		Actor: "tester", Reason: "start CodeEdge evidence fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunInDoubt,
		Actor: "tester", Reason: "preserve uncertain external evaluator outcome",
	})
	if err != nil {
		t.Fatal(err)
	}

	manifest, err := services.Store().CreateArtifactManifest(ctx, store.CreateArtifactManifestRequest{
		SubjectRevisionID: revision.ID, SubjectDigest: revision.TaskDigest, WorkflowFingerprint: "sha256:codeedge-evidence-workflow",
		ManifestJSON:        `{"artifacts":["qwen_trial_result","qwen_pass4_evidence","opus_trial_result","opus_pass4_evidence"]}`,
		ManifestFingerprint: "sha256:codeedge-evidence-manifest", IdempotencyKey: "tui-codeedge-evidence-manifest",
		Actor: "tester", Reason: "record CodeEdge evidence fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	staleManifest, err := services.Store().CreateArtifactManifest(ctx, store.CreateArtifactManifestRequest{
		SubjectRevisionID: revision.ID, SubjectDigest: revision.TaskDigest, WorkflowFingerprint: "sha256:codeedge-evidence-workflow",
		ManifestJSON:        `{"artifacts":["qwen_trial_result","qwen_pass4_evidence"]}`,
		ManifestFingerprint: "sha256:codeedge-evidence-stale-manifest", IdempotencyKey: "tui-codeedge-evidence-stale-manifest",
		Actor: "tester", Reason: "record stale CodeEdge evidence fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	qwen := taskHubCreateCodeEdgeEvidenceStage(t, ctx, services, run.ID, workflowadapter.HarborRunQwen, 12, store.StageExecutionInDoubt)
	opus := taskHubCreateCodeEdgeEvidenceStage(t, ctx, services, run.ID, workflowadapter.HarborRunOpus, 13, store.StageExecutionInterrupted)

	taskHubCreateCodeEdgeEvidenceRef(t, ctx, services, manifest, revision, run.ID, workflowadapter.HarborRunQwen, qwen.ID, "qwen_trial_result", "sha256:sqlite-qwen-bundle", codeedge.HarborRunBundleV018Format, "qwen-bundle")
	taskHubCreateCodeEdgeEvidenceRef(t, ctx, services, manifest, revision, run.ID, workflowadapter.HarborRunQwen, qwen.ID, "qwen_pass4_evidence", "sha256:sqlite-qwen-screenshot", "image/png", "qwen-screenshot")
	taskHubCreateCodeEdgeEvidenceRef(t, ctx, services, manifest, revision, run.ID, workflowadapter.HarborRunOpus, opus.ID, "opus_trial_result", "sha256:sqlite-opus-bundle", codeedge.HarborRunBundleV018Format, "opus-bundle")
	taskHubCreateCodeEdgeEvidenceRef(t, ctx, services, manifest, revision, run.ID, workflowadapter.HarborRunOpus, opus.ID, "opus_pass4_evidence", "sha256:sqlite-opus-screenshot", "image/png", "opus-screenshot")
	// These use expected artifact keys but do not bind to either current
	// StageAttempt. The read-only view must not attach them by key alone.
	taskHubCreateCodeEdgeEvidenceRef(t, ctx, services, staleManifest, revision, run.ID, workflowadapter.HarborRunQwen, "stale-qwen-attempt", "qwen_trial_result", "sha256:sqlite-stale-attempt", codeedge.HarborRunBundleV018Format, "stale-attempt")
	taskHubCreateCodeEdgeEvidenceRef(t, ctx, services, staleManifest, revision, run.ID, workflowadapter.HarborRunOpus, qwen.ID, "qwen_pass4_evidence", "sha256:sqlite-wrong-stage", "image/png", "wrong-stage")

	adapter := NewAppTaskHubLifecycleAdapter(services)
	detail, err := adapter.QueryTaskHubDetail(ctx, TaskHubDetailQuery{TaskID: task.ID, RunID: run.ID})
	if err != nil {
		t.Fatalf("query CodeEdge evidence detail from SQLite: %v", err)
	}
	var childProjection *TaskHubRunFact
	for index := range detail.Runs {
		if detail.Runs[index].RunID == run.ID {
			childProjection = &detail.Runs[index]
			break
		}
	}
	if len(detail.Runs) != 2 || childProjection == nil || childProjection.ParentRunID != parent.ID || childProjection.Status != string(store.WorkflowRunInDoubt) || len(childProjection.Stages) != 2 {
		t.Fatalf("SQLite CodeEdge Run/stage projection = %+v", detail.Runs)
	}
	artifactRefCount := 0
	for _, artifact := range detail.Artifacts {
		artifactRefCount += len(artifact.Refs)
	}
	if len(detail.Artifacts) != 2 || artifactRefCount != 6 {
		t.Fatalf("SQLite CodeEdge artifact projection = %+v", detail.Artifacts)
	}
	childFrozen := false
	for _, frozen := range detail.FrozenExecutions {
		if frozen.RunID == run.ID && frozen.State == TaskHubFrozenExecutionBound && frozen.TemplateID == workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID {
			childFrozen = true
		}
	}
	if len(detail.FrozenExecutions) != 2 || !childFrozen {
		t.Fatalf("SQLite CodeEdge frozen execution projection = %+v", detail.FrozenExecutions)
	}

	overlay := newTaskHubDetailOverlay(TaskHubDetailQuery{TaskID: task.ID, RunID: run.ID})
	overlay.Loading = false
	overlay.Detail = detail
	rendered := strings.Join(overlay.frozenExecutionRows(), "\n")
	for _, required := range []string{
		"sha256:sqlite-qwen-bundle",
		"sha256:sqlite-qwen-screenshot",
		"sha256:sqlite-opus-bundle",
		"sha256:sqlite-opus-screenshot",
		"Run 状态：in_doubt；需 reconcile；TUI 不会继续或自动重跑。",
		"Qwen StageAttempt 状态：in_doubt；需 reconcile；TUI 不会继续或自动重跑。",
		"Opus StageAttempt 状态：interrupted；需 reconcile；TUI 不会继续或自动重跑。",
	} {
		if !strings.Contains(rendered, required) {
			t.Fatalf("SQLite CodeEdge TUI projection omitted %q:\n%s", required, rendered)
		}
	}
	for _, unexpected := range []string{"sha256:sqlite-stale-attempt", "sha256:sqlite-wrong-stage"} {
		if strings.Contains(rendered, unexpected) {
			t.Fatalf("SQLite CodeEdge TUI projection attached mismatched ref %q:\n%s", unexpected, rendered)
		}
	}

	afterRun, err := services.Runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterQwen, err := services.Runs.GetStageAttempt(ctx, qwen.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterOpus, err := services.Runs.GetStageAttempt(ctx, opus.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterRun.Version != run.Version || afterRun.Status != run.Status || afterQwen.ExecutionStatus != store.StageExecutionInDoubt || afterOpus.ExecutionStatus != store.StageExecutionInterrupted {
		t.Fatalf("read-only SQLite detail query mutated CodeEdge lifecycle state: run=%+v qwen=%+v opus=%+v", afterRun, afterQwen, afterOpus)
	}
}

func taskHubCodeEdgeEvidenceDetailFixture() TaskHubDetail {
	detail := taskHubDetailFixture()
	detail.Runs[0].Status = string(store.WorkflowRunInDoubt)
	// Qwen and Opus are owned by the closed evaluator child, never by the
	// Phase-1 parent. Keep this fixture on the child topology so the TUI cannot
	// accidentally regress to showing fabricated parent-owned evaluator stages.
	detail.Runs[0].ParentRunID = "parent-codeedge-run"
	detail.Runs[0].WorkflowTemplateID = workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID
	detail.Runs[0].WorkflowTemplateVer = workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion
	detail.Runs[0].Stages = []TaskHubStageFact{
		{
			StageAttemptID: "qwen-current-attempt", StageKey: workflowadapter.HarborRunQwen, StageGroup: "evaluation", Ordinal: 12,
			ExecutionState: string(store.StageExecutionInterrupted),
		},
		{
			StageAttemptID: "opus-current-attempt", StageKey: workflowadapter.HarborRunOpus, StageGroup: "evaluation", Ordinal: 13,
			ExecutionState: string(store.StageExecutionCompleted), Verdict: string(store.VerdictPass),
		},
	}
	detail.FrozenExecutions = []TaskHubFrozenExecutionFact{{
		RunID: detail.Runs[0].RunID, State: TaskHubFrozenExecutionBound,
		TemplateID: workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID, TemplateVersion: workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion,
		ExecutionProfileID: "codeedge-test", ExecutionProfileVersion: "1",
		DeploymentCatalog: TaskHubDeploymentCatalogFact{State: TaskHubDeploymentCatalogNotRecorded},
	}}
	detail.Artifacts = []TaskHubArtifactFact{{
		ManifestID: "codeedge-evidence-manifest", RevisionID: detail.Runs[0].RevisionID, SubjectDigest: "harbor.task.v2:sha256:fixture", WorkflowFingerprint: "sha256:workflow",
		Refs: []TaskHubArtifactRefFact{
			{ArtifactKey: "qwen_trial_result", ContentDigest: "sha256:qwen-bundle", SchemaVersion: codeedge.HarborRunBundleV018Format, RunID: detail.Runs[0].RunID, StageKey: workflowadapter.HarborRunQwen, AttemptID: "qwen-current-attempt"},
			{ArtifactKey: "qwen_pass4_evidence", ContentDigest: "sha256:qwen-screenshot", SchemaVersion: "image/png", RunID: detail.Runs[0].RunID, StageKey: workflowadapter.HarborRunQwen, AttemptID: "qwen-current-attempt"},
			{ArtifactKey: "opus_trial_result", ContentDigest: "sha256:opus-bundle", SchemaVersion: codeedge.HarborRunBundleV018Format, RunID: detail.Runs[0].RunID, StageKey: workflowadapter.HarborRunOpus, AttemptID: "opus-current-attempt"},
			{ArtifactKey: "opus_pass4_evidence", ContentDigest: "sha256:opus-screenshot", SchemaVersion: "image/png", RunID: detail.Runs[0].RunID, StageKey: workflowadapter.HarborRunOpus, AttemptID: "opus-current-attempt"},
			{ArtifactKey: "qwen_trial_result", ContentDigest: "sha256:qwen-stale-attempt", SchemaVersion: codeedge.HarborRunBundleV018Format, RunID: detail.Runs[0].RunID, StageKey: workflowadapter.HarborRunQwen, AttemptID: "qwen-older-attempt"},
			{ArtifactKey: "qwen_pass4_evidence", ContentDigest: "sha256:qwen-wrong-stage", SchemaVersion: "image/png", RunID: detail.Runs[0].RunID, StageKey: workflowadapter.HarborRunOpus, AttemptID: "qwen-current-attempt"},
			{ArtifactKey: "opus_trial_result", ContentDigest: "sha256:opus-wrong-run", SchemaVersion: codeedge.HarborRunBundleV018Format, RunID: "another-run", StageKey: workflowadapter.HarborRunOpus, AttemptID: "opus-current-attempt"},
		},
	}}
	return detail
}

func taskHubCreateCodeEdgeEvidenceStage(t *testing.T, ctx context.Context, services *app.LifecycleServices, runID, stageKey string, ordinal int, terminal store.StageExecutionStatus) store.StageAttempt {
	t.Helper()
	stage, err := services.Store().CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: runID, StageKey: stageKey, StageGroup: "evaluation", Ordinal: ordinal,
		InputFingerprint:   "sha256:tui-codeedge-evidence-stage-" + stageKey,
		BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "tester", Reason: "create CodeEdge TUI evidence stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err = services.Store().TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: store.StageExecutionRunning,
		Actor: "tester", Reason: "start CodeEdge TUI evidence stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err = services.Store().TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: terminal,
		Actor: "tester", Reason: "preserve CodeEdge evaluator outcome for TUI",
	})
	if err != nil {
		t.Fatal(err)
	}
	return stage
}

func taskHubCreateCodeEdgeEvidenceRef(t *testing.T, ctx context.Context, services *app.LifecycleServices, manifest store.ArtifactManifest, revision store.TaskRevision, runID, stageKey, attemptID, artifactKey, contentDigest, schemaVersion, suffix string) {
	t.Helper()
	if _, err := services.Store().CreateArtifactRef(ctx, store.CreateArtifactRefRequest{
		ManifestID: manifest.ID, ArtifactKey: artifactKey, ContentDigest: contentDigest, SchemaVersion: schemaVersion,
		RunID: runID, StageKey: stageKey, AttemptID: attemptID, TurnOrdinal: 0,
		SubjectRevisionID: revision.ID, SubjectDigest: revision.TaskDigest, WorkflowFingerprint: manifest.WorkflowFingerprint,
		InputBindingsJSON: `[]`, InputFingerprint: "sha256:tui-codeedge-evidence-input-" + suffix, ProducerVersion: "tui-evidence-test.v1",
		IdempotencyKey: "tui-codeedge-evidence-ref-" + suffix, Actor: "tester", Reason: "record CodeEdge TUI evidence ref",
	}); err != nil {
		t.Fatal(err)
	}
}
