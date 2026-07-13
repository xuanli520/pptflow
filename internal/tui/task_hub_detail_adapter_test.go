package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

func TestAppTaskHubDetailAdapterProjectsRealSQLiteFactsWithoutMutation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := app.NewLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{
			Slug: "detail-sqlite", Title: "Detail SQLite", SourceRepo: "https://example.invalid/detail", SourceCommit: "abc123",
			Actor: "tester", Reason: "Task Hub detail fixture",
		},
		SourceDirectory: taskHubAdapterSnapshot(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	revision, err = services.Revisions.MarkValidated(ctx, revision.ID, revision.StateVersion, "sha256:detail-evidence", "tester", "validate detail fixture")
	if err != nil {
		t.Fatal(err)
	}
	review, err := services.Reviews.Request(ctx, revision.ID, revision.ValidationEvidenceManifest, "tester", "request detail review")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Reviews.Decide(ctx, app.DecideReviewRequest{
		ReviewRequestID: review.ID, RevisionID: revision.ID, Action: store.ReviewDecisionApprove,
		ExpectedRevisionDigest: revision.TaskDigest, Actor: "tester", Reason: "approve detail fixture",
	}); err != nil {
		t.Fatal(err)
	}
	task, err = services.Reviews.PromoteCurrent(ctx, task.ID, revision.ID, task.Version, "tester", "promote detail fixture")
	if err != nil {
		t.Fatal(err)
	}
	packageResult, err := services.Releases.PackageRevision(ctx, app.PackageRevisionRequest{
		RevisionID: revision.ID, ExpectedStateVersion: revision.StateVersion, ReleaseVersion: "detail-v1", Channel: "detail-stable", ExpectedChannelVersion: 0,
		Actor: "tester", Reason: "create local detail package",
	})
	if err != nil {
		t.Fatal(err)
	}
	revision, err = services.Revisions.Get(ctx, revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: taskHubAdapterCompleteProfile(t), Trigger: "detail-integration", Actor: "tester", Reason: "start detail fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "tester", Reason: "worker started detail fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := services.Store().CreateArtifactManifest(ctx, store.CreateArtifactManifestRequest{
		SubjectRevisionID: revision.ID, SubjectDigest: revision.TaskDigest, WorkflowFingerprint: "sha256:detail-workflow",
		ManifestJSON: `{"artifacts":["quality-report"]}`, ManifestFingerprint: "sha256:detail-manifest", IdempotencyKey: "detail-artifact-manifest", Actor: "tester", Reason: "create detail evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err := services.Store().CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: "quality", StageGroup: "verify", Ordinal: 1, InputFingerprint: "sha256:detail-stage", BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "tester", Reason: "create detail stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err = services.Store().TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "tester", Reason: "start detail stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Store().CreateArtifactRef(ctx, store.CreateArtifactRefRequest{
		ManifestID: manifest.ID, ArtifactKey: "quality-report", ContentDigest: "sha256:detail-artifact", SchemaVersion: "report.v1", RunID: run.ID,
		StageKey: "quality", AttemptID: stage.ID, TurnOrdinal: 0, SubjectRevisionID: revision.ID, SubjectDigest: revision.TaskDigest,
		WorkflowFingerprint: manifest.WorkflowFingerprint, InputBindingsJSON: `[]`, InputFingerprint: "sha256:detail-input", ProducerVersion: "fixture.v1", IdempotencyKey: "detail-artifact-ref", Actor: "tester", Reason: "record detail report",
	}); err != nil {
		t.Fatal(err)
	}
	stage, err = services.Store().TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictPass, ArtifactManifestID: manifest.ID,
		Actor: "tester", Reason: "complete detail stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	command, err := services.Store().CreateContinuationCommand(ctx, store.CreateContinuationCommandRequest{
		CommandKey: "detail-repair-command", SubjectID: task.ID, RunID: run.ID, PayloadJSON: `{"kind":"repair"}`, Actor: "tester", Reason: "record detail repair command",
	})
	if err != nil {
		t.Fatal(err)
	}
	repair, err := services.Store().CreateRepairSession(ctx, store.CreateRepairSessionRequest{
		CommandID: command.ID, SubjectID: task.ID, BaseRevisionID: revision.ID, MaxRounds: 2, FindingsJSON: `[]`, PolicyJSON: `{"max_rounds":2}`,
		IdempotencyKey: "detail-repair-session", Actor: "tester", Reason: "record detail repair session",
	})
	if err != nil {
		t.Fatal(err)
	}
	change, err := services.Store().CreatePreparedChange(ctx, store.CreatePreparedChangeRequest{
		CommandID: command.ID, RepairSessionID: repair.ID, RoundOrdinal: 1, ProviderID: "local-patch", OperationKey: "detail-repair-operation",
		PayloadJSON: `{"directive":"repair"}`, ObservedChangesJSON: `["solution/solve.sh"]`, BeforeDigest: "sha256:before", AfterDigest: "sha256:after", Actor: "tester", Reason: "record detail prepared change",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Store().CreateMutationReceipt(ctx, store.CreateMutationReceiptRequest{
		PreparedChangeID: change.ID, OperationKey: change.OperationKey, Outcome: store.MutationReceiptUncertain, ReceiptJSON: `{"external":"unknown"}`,
		IdempotencyKey: "detail-repair-receipt", Actor: "tester", Reason: "record detail receipt",
	}); err != nil {
		t.Fatal(err)
	}

	beforeTask, err := services.Tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeRun, err := services.Runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeStage, err := services.Runs.GetStageAttempt(ctx, stage.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeReleases, err := services.Releases.List(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}

	adapter := NewAppTaskHubLifecycleAdapter(services)
	detail, err := adapter.QueryTaskHubDetail(ctx, TaskHubDetailQuery{TaskID: task.ID, RunID: run.ID})
	if err != nil {
		t.Fatalf("query real Task Hub detail: %v", err)
	}
	if detail.Task.TaskID != task.ID || detail.SelectedRunID != run.ID || len(detail.Revisions) != 1 || len(detail.Runs) != 1 || len(detail.Releases) != 1 {
		t.Fatalf("core detail projection = %+v", detail)
	}
	if detail.Releases[0].ReleaseID != packageResult.Release.ID || len(detail.Runs[0].Stages) != 1 || detail.Runs[0].Stages[0].ArtifactManifestID != manifest.ID {
		t.Fatalf("release/stage facts were not projected: %+v", detail)
	}
	if len(detail.Artifacts) != 1 || len(detail.Artifacts[0].Refs) != 1 || detail.Artifacts[0].Refs[0].ArtifactKey != "quality-report" {
		t.Fatalf("artifact facts were not projected: %+v", detail.Artifacts)
	}
	if len(detail.Reviews) != 1 || detail.Reviews[0].State != "closed" || len(detail.Reviews[0].Decisions) != 1 || detail.Reviews[0].Decisions[0].Action != string(store.ReviewDecisionApprove) {
		t.Fatalf("review facts were not projected: %+v", detail.Reviews)
	}
	if len(detail.Repairs) != 1 || len(detail.Repairs[0].Changes) != 1 || detail.Repairs[0].Changes[0].ProviderID != "local-patch" || len(detail.Repairs[0].Changes[0].Receipts) != 1 || detail.Repairs[0].Changes[0].Receipts[0].Outcome != string(store.MutationReceiptUncertain) {
		t.Fatalf("repair facts were not projected: %+v", detail.Repairs)
	}

	afterTask, _ := services.Tasks.Get(ctx, task.ID)
	afterRun, _ := services.Runs.Get(ctx, run.ID)
	afterStage, _ := services.Runs.GetStageAttempt(ctx, stage.ID)
	afterReleases, _ := services.Releases.List(ctx, task.ID)
	if afterTask.Version != beforeTask.Version || afterTask.LifecycleState != beforeTask.LifecycleState || afterRun.Version != beforeRun.Version || afterRun.Status != beforeRun.Status || afterStage.Version != beforeStage.Version || afterStage.ExecutionStatus != beforeStage.ExecutionStatus || len(afterReleases) != len(beforeReleases) {
		t.Fatalf("detail query mutated durable state: task before=%+v after=%+v run before=%+v after=%+v stage before=%+v after=%+v releases before=%+v after=%+v", beforeTask, afterTask, beforeRun, afterRun, beforeStage, afterStage, beforeReleases, afterReleases)
	}

	m, cleanup := newTestTaskHubV2Model(t, adapter)
	defer cleanup()
	m.width, m.height = 100, 30
	m.selectTaskHubTab(TaskHubRunsTab)
	m.taskHub.SelectedRunID = run.ID
	m.taskHub.SelectedTaskID = task.ID
	updated, detailCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if detailCommand == nil || m.taskHubDetail == nil {
		t.Fatalf("real lifecycle Task Hub did not open deferred detail: overlay=%+v command=%v", m.taskHubDetail, detailCommand)
	}
	updated, _ = m.Update(detailCommand())
	m = updated.(model)
	if m.taskHubDetail == nil || m.taskHubDetail.Detail.SelectedRunID != run.ID {
		t.Fatalf("real detail response did not bind selected Run: %+v", m.taskHubDetail)
	}
	for range []int{0, 1, 2, 3} {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = updated.(model)
	}
	if rendered := m.taskHubDetail.View(100, 30); !strings.Contains(rendered, "quality-report") || !strings.Contains(rendered, "local-patch") || !strings.Contains(rendered, "uncertain") {
		t.Fatalf("real SQLite facts did not survive TUI detail rendering:\n%s", rendered)
	}
}
