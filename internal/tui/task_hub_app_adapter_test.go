package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestAppTaskHubLifecycleAdapterQueriesRealServicesAndPlansWithoutMutation(t *testing.T) {
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
	task, err := services.Tasks.CreateDraft(ctx, app.CreateDraftTaskRequest{
		Slug:   "adapter-draft",
		Title:  "Adapter Draft",
		Actor:  "tester",
		Reason: "Task Hub adapter integration fixture",
	})
	if err != nil {
		t.Fatal(err)
	}

	adapter := NewAppTaskHubLifecycleAdapter(services)
	snapshot, err := adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubTasksTab, Filter: "adapter"})
	if err != nil {
		t.Fatalf("query real Task Hub services: %v", err)
	}
	if len(snapshot.Tasks) != 1 {
		t.Fatalf("Task Hub tasks = %#v, want one real draft Task", snapshot.Tasks)
	}
	projected := snapshot.Tasks[0]
	if projected.TaskID != task.ID || projected.Name != task.Title || projected.Lifecycle != string(store.TaskLifecycleDraft) {
		t.Fatalf("Task Hub projection = %+v, want real Task %+v", projected, task)
	}
	softDelete, found := taskHubActionStateFor(projected.Actions, TaskHubActionSoftDeleteTask)
	if !found || softDelete.Enabled || !strings.Contains(softDelete.DisabledReason, "UUIDv7") {
		t.Fatalf("soft-delete capability = %+v, want disabled without an idempotent command contract", softDelete)
	}

	before, err := services.Tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := adapter.PlanTaskHubCommand(ctx, TaskHubCommand{
		Action: TaskHubActionSoftDeleteTask,
		Target: TaskHubTarget{TaskID: task.ID},
	})
	if err != nil {
		t.Fatalf("plan real Task Hub action: %v", err)
	}
	if plan.ConfirmationNeeded || plan.Title != "软删除 Task" || !strings.Contains(plan.Reason, "UUIDv7") {
		t.Fatalf("soft-delete plan = %+v, want an explicitly unavailable preview", plan)
	}
	after, err := services.Tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.LifecycleState != before.LifecycleState || after.Version != before.Version || after.DeletedAt != before.DeletedAt {
		t.Fatalf("plan mutated Task: before=%+v after=%+v", before, after)
	}
}

func TestTaskHubContinuationTransitionLabelRetainsSortedPlannerReasons(t *testing.T) {
	label := taskHubContinuationTransitionLabel(workflowkit.NodeTransition{
		NodeID:      "quality_check",
		ReasonCodes: []workflowkit.PlanReason{"artifact_reused", "force_recompute", "artifact_reused"},
	})
	if label != "quality_check（artifact_reused, force_recompute）" {
		t.Fatalf("continuation transition label = %q", label)
	}
	if label := taskHubContinuationTransitionLabel(workflowkit.NodeTransition{NodeID: "similarity_check"}); label != "similarity_check" {
		t.Fatalf("reason-free transition label = %q", label)
	}
}

func TestAppTaskHubLifecycleAdapterPlansOnlyLocalPackageForReviewedRevision(t *testing.T) {
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
			Slug:   "local-package-adapter",
			Title:  "Local package adapter",
			Actor:  "tester",
			Reason: "Task Hub local-package fixture",
		},
		SourceDirectory: taskHubAdapterSnapshot(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	revision, err = services.Revisions.MarkValidated(ctx, revision.ID, revision.StateVersion, "sha256:adapter-evidence", "tester", "validate fixture")
	if err != nil {
		t.Fatal(err)
	}
	review, err := services.Reviews.Request(ctx, revision.ID, revision.ValidationEvidenceManifest, "tester", "request fixture review")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Reviews.Decide(ctx, app.DecideReviewRequest{
		ReviewRequestID:        review.ID,
		RevisionID:             revision.ID,
		Action:                 store.ReviewDecisionApprove,
		ExpectedRevisionDigest: revision.TaskDigest,
		Actor:                  "tester",
		Reason:                 "approve fixture",
	}); err != nil {
		t.Fatal(err)
	}
	task, err = services.Reviews.PromoteCurrent(ctx, task.ID, revision.ID, task.Version, "tester", "promote fixture")
	if err != nil {
		t.Fatal(err)
	}

	adapter := NewAppTaskHubLifecycleAdapter(services)
	snapshot, err := adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubTasksTab})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 1 {
		t.Fatalf("Task Hub tasks = %#v, want imported Task", snapshot.Tasks)
	}
	packageAction, found := taskHubActionStateFor(snapshot.Tasks[0].Actions, TaskHubActionPackageRevision)
	if !found || !packageAction.Enabled {
		t.Fatalf("local-package capability = %+v, want enabled", packageAction)
	}

	beforeTask, err := services.Tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeRevision, err := services.Revisions.Get(ctx, revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeReleases, err := services.Releases.List(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := adapter.PlanTaskHubCommand(ctx, TaskHubCommand{
		Action: TaskHubActionPackageRevision,
		Target: TaskHubTarget{TaskID: task.ID, RevisionID: revision.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.ConfirmationNeeded || plan.Title != "生成本地 package" ||
		!strings.Contains(plan.Summary, "受管目录") || !strings.Contains(plan.Summary, "不会上传") ||
		!strings.Contains(plan.Summary, "provider") || len(plan.ExternalEffects) != 0 {
		t.Fatalf("local package plan = %+v, want local-only no-provider preview", plan)
	}
	afterTask, err := services.Tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterRevision, err := services.Revisions.Get(ctx, revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterReleases, err := services.Releases.List(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTask.LifecycleState != beforeTask.LifecycleState || afterTask.Version != beforeTask.Version ||
		afterRevision.State != beforeRevision.State || afterRevision.StateVersion != beforeRevision.StateVersion ||
		len(afterReleases) != len(beforeReleases) {
		t.Fatalf("local package plan mutated lifecycle state: task before=%+v after=%+v revision before=%+v after=%+v releases before=%+v after=%+v", beforeTask, afterTask, beforeRevision, afterRevision, beforeReleases, afterReleases)
	}
}

func TestAppTaskHubLifecycleAdapterProjectsRealControlAndTUIOnlyPreviews(t *testing.T) {
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
			Slug:   "control-adapter",
			Title:  "Control adapter",
			Actor:  "tester",
			Reason: "Task Hub control fixture",
		},
		SourceDirectory: taskHubAdapterSnapshot(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: taskHubAdapterCompleteProfile(t), Trigger: "integration",
		Actor: "tester", Reason: "start Task Hub control fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning,
		Actor: "tester", Reason: "worker started fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err := services.Store().CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: "harbor_qwen", StageGroup: "evaluation", Ordinal: 1,
		InputFingerprint: "sha256:task-hub-control-stage", BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "tester", Reason: "current control stage fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAppTaskHubLifecycleAdapter(services)
	beforeControlSnapshot, err := adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubRunsTab})
	if err != nil {
		t.Fatalf("query running Task Hub control capability: %v", err)
	}
	if len(beforeControlSnapshot.Runs) != 1 {
		t.Fatalf("running Task Hub runs = %#v, want one Run", beforeControlSnapshot.Runs)
	}
	runningPause, found := taskHubRunControlActionStateFor(beforeControlSnapshot.Runs[0].Control.Actions, TaskHubRunControlPause)
	if !found || !runningPause.Enabled || runningPause.DisabledReason != "" {
		t.Fatalf("running pause capability = %+v, want a confirmation-capable action", runningPause)
	}
	checkpoint, err := services.Control.CurrentCheckpoint(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	control, err := services.Control.Request(ctx, app.RequestExecutionControlRequest{
		OperationKey: "task-hub-control-read-fixture", Action: store.ControlActionPause, RunID: run.ID,
		Expected: checkpoint, Actor: "tester", Reason: "persist control facts for Task Hub",
	})
	if err != nil {
		t.Fatal(err)
	}
	control, err = services.Control.Transition(ctx, app.TransitionExecutionControlRequest{
		OperationID: control.ID, ExpectedVersion: control.Version, Status: store.ControlOperationAcknowledged,
		CheckpointID: "checkpoint-task-hub", QuotaSettlementID: "quota-task-hub", Actor: "tester", Reason: "worker acknowledged fixture",
		RuntimeReceipts: []store.RuntimeTerminationReceipt{{
			RuntimeScopeID: "worker:task-hub", ObservedAt: time.Now().UTC(), Graceful: true, PayloadJSON: `{}`,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubRunsTab})
	if err != nil {
		t.Fatalf("query real Task Hub control state: %v", err)
	}
	if len(snapshot.Runs) != 1 {
		t.Fatalf("Task Hub runs = %#v, want one real Run", snapshot.Runs)
	}
	projected := snapshot.Runs[0]
	if projected.RunID != run.ID || projected.Stage != "evaluation/harbor_qwen" || projected.Control.StageAttemptID != stage.ID {
		t.Fatalf("Run control stage projection = %+v, want run=%s stage=%s", projected, run.ID, stage.ID)
	}
	if projected.ControlStatus != string(store.ControlOperationAcknowledged) || projected.Control.OperationID != control.ID ||
		projected.Control.CheckpointID != "checkpoint-task-hub" || projected.Control.QuotaSettlementID != "quota-task-hub" ||
		projected.Control.RuntimeReceiptCount != 1 || projected.Control.CheckpointSequence == 0 {
		t.Fatalf("Run control facts were not projected from real services: %+v", projected.Control)
	}
	pause, found := taskHubRunControlActionStateFor(projected.Control.Actions, TaskHubRunControlPause)
	if !found || pause.Enabled || !strings.Contains(pause.DisabledReason, "pause_requested") {
		t.Fatalf("pause-requested capability = %+v, want explicit current-state reason", pause)
	}
	cancelStage, found := taskHubRunControlActionStateFor(projected.Control.Actions, TaskHubRunControlCancelStage)
	if !found || cancelStage.Enabled || !strings.Contains(cancelStage.DisabledReason, "cancel capability") {
		t.Fatalf("stage cancel capability = %+v, want explicit provider-capability reason", cancelStage)
	}
	controlView := newLifecycleRunControlOverlay(projected).View(100, 40)
	for _, expected := range []string{
		"当前阶段：evaluation/harbor_qwen / queued",
		"控制 checkpoint：序列 ",
		"控制状态：acknowledged",
		"最近 checkpoint：checkpoint-task-hub",
		"runtime ack：1 条 receipt",
		"quota settlement：quota-task-hub",
		"cancel capability",
	} {
		if !strings.Contains(controlView, expected) {
			t.Fatalf("real Run Control projection omitted %q:\n%s", expected, controlView)
		}
	}

	beforeRun, err := services.Runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeStage, err := services.Runs.GetStageAttempt(ctx, stage.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeControls, err := services.Control.ListForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	m, cleanup := newTestTaskHubV2Model(t, adapter)
	defer cleanup()
	if m.taskHub.SelectedRunID != run.ID {
		t.Fatalf("real lifecycle Task Hub did not select projected Run: %q", m.taskHub.SelectedRunID)
	}
	m.openRunControl()
	updated, selectCmd := m.Update(runeKey("s"))
	m = updated.(model)
	if selectCmd != nil || m.runControl == nil || m.runControl.SelectedAction != TaskHubRunControlTerminate {
		t.Fatalf("S did not only select a run-control preview: overlay=%+v command=%v", m.runControl, selectCmd)
	}
	updated, previewCmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if previewCmd == nil {
		t.Fatal("selected real run control did not return a deferred preview command")
	}
	updated, _ = m.Update(previewCmd())
	m = updated.(model)
	if m.runControl == nil || m.runControl.Preview == nil || !strings.Contains(m.runControl.Preview.Summary, "确认表单") || !m.runControl.Preview.ConfirmationNeeded {
		t.Fatalf("real service control preview was not retained for confirmation: %+v", m.runControl)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(model)
	if m.runControl != nil {
		t.Fatal("Esc did not dismiss real control overlay")
	}

	cancelPreview, err := adapter.PlanTaskHubRunControl(ctx, TaskHubRunControlCommand{
		Action: TaskHubRunControlCancelStage,
		Target: TaskHubTarget{TaskID: task.ID, RunID: run.ID, RevisionID: revision.ID, StageAttemptID: stage.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cancelPreview.Reason, "cancel capability") || !strings.Contains(cancelPreview.Summary, "只读取事实") {
		t.Fatalf("cancel-stage preview did not make missing capability explicit: %+v", cancelPreview)
	}
	missingStagePreview, err := adapter.PlanTaskHubRunControl(ctx, TaskHubRunControlCommand{
		Action: TaskHubRunControlCancelStage,
		Target: TaskHubTarget{TaskID: task.ID, RunID: run.ID, RevisionID: revision.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(missingStagePreview.Reason, "没有明确") {
		t.Fatalf("cancel-stage preview did not make a missing target stage explicit: %+v", missingStagePreview)
	}

	afterRun, err := services.Runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterStage, err := services.Runs.GetStageAttempt(ctx, stage.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterControls, err := services.Control.ListForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterRun.Version != beforeRun.Version || afterRun.Status != beforeRun.Status ||
		afterStage.Version != beforeStage.Version || afterStage.ExecutionStatus != beforeStage.ExecutionStatus ||
		len(afterControls) != len(beforeControls) || afterControls[0].Version != beforeControls[0].Version {
		t.Fatalf("Task Hub control preview or Esc mutated durable state: run before=%+v after=%+v stage before=%+v after=%+v controls before=%+v after=%+v", beforeRun, afterRun, beforeStage, afterStage, beforeControls, afterControls)
	}
}

func TestAppTaskHubRunControlConfirmationUsesCapturedCheckpointAndFrozenGrace(t *testing.T) {
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
			Slug: "control-submit", Title: "Control submit", Actor: "tester", Reason: "control submission fixture",
		},
		SourceDirectory: taskHubAdapterSnapshot(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: taskHubAdapterCompleteProfile(t), Trigger: "integration",
		Actor: "tester", Reason: "start control submission fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning,
		Actor: "tester", Reason: "worker started fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAppTaskHubLifecycleAdapter(services)
	snapshot, err := adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubRunsTab})
	if err != nil || len(snapshot.Runs) != 1 {
		t.Fatalf("query control submission fixture = %#v, err=%v", snapshot, err)
	}
	stale := snapshot.Runs[0].Control.Expected
	if snapshot.Runs[0].Control.GracePeriod != 30*time.Second {
		t.Fatalf("projected frozen grace = %s, want 30s", snapshot.Runs[0].Control.GracePeriod)
	}
	preview, err := adapter.PlanTaskHubRunControl(ctx, TaskHubRunControlCommand{
		Action: TaskHubRunControlPause,
		Target: TaskHubTarget{TaskID: task.ID, RunID: run.ID, RevisionID: revision.ID},
	})
	if err != nil || preview.BudgetImpact != "冻结 grace period：30s" {
		t.Fatalf("control preview = %+v, err=%v", preview, err)
	}
	updatedTask, err := services.Tasks.Update(ctx, app.UpdateTaskRequest{
		TaskID: task.ID, ExpectedVersion: task.Version, Slug: task.Slug, Title: task.Title,
		Actor: "tester", Reason: "invalidate captured control checkpoint",
	})
	if err != nil {
		t.Fatal(err)
	}
	staleKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.ExecuteTaskHubRunControlMutation(ctx, TaskHubRunControlMutationRequest{
		Action:   TaskHubRunControlPause,
		Target:   TaskHubTarget{TaskID: task.ID, RunID: run.ID, RevisionID: revision.ID},
		Expected: stale, Actor: "tester", Reason: "submit stale control confirmation", IdempotencyKey: staleKey,
	})
	if !errors.Is(err, store.ErrOptimisticLock) {
		t.Fatalf("stale control confirmation error = %v, want optimistic-lock rejection", err)
	}
	operations, err := services.Control.ListForRun(ctx, run.ID)
	if err != nil || len(operations) != 0 {
		t.Fatalf("stale confirmation created control operation: %#v, err=%v", operations, err)
	}

	snapshot, err = adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubRunsTab})
	if err != nil || len(snapshot.Runs) != 1 {
		t.Fatalf("refresh control submission fixture = %#v, err=%v", snapshot, err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	request := TaskHubRunControlMutationRequest{
		Action:   TaskHubRunControlPause,
		Target:   TaskHubTarget{TaskID: updatedTask.ID, RunID: run.ID, RevisionID: revision.ID},
		Expected: snapshot.Runs[0].Control.Expected, Actor: "tester", Reason: "pause through Task Hub", IdempotencyKey: key,
	}
	result, err := adapter.ExecuteTaskHubRunControlMutation(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.OperationID == "" || result.Action != TaskHubRunControlPause {
		t.Fatalf("Task Hub control result = %+v", result)
	}
	operation, err := services.Control.Get(ctx, result.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if operation.Action != store.ControlActionPause || operation.GracePeriod != 30*time.Second || operation.Expected.SubjectVersion != updatedTask.Version {
		t.Fatalf("durable control did not preserve frozen policy/checkpoint: %+v", operation)
	}
	replay, err := adapter.ExecuteTaskHubRunControlMutation(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replay.OperationID != result.OperationID {
		t.Fatalf("control replay operation = %s, want %s", replay.OperationID, result.OperationID)
	}
	persistedRun, err := services.Runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedRun.Status != store.WorkflowRunPauseRequested {
		t.Fatalf("pause request did not durably transition run: %+v", persistedRun)
	}
}

func taskHubActionStateFor(states []TaskHubActionState, action TaskHubAction) (TaskHubActionState, bool) {
	for _, state := range states {
		if state.Action == action {
			return state, true
		}
	}
	return TaskHubActionState{}, false
}

func taskHubRunControlActionStateFor(states []TaskHubRunControlActionState, action TaskHubRunControlAction) (TaskHubRunControlActionState, bool) {
	for _, state := range states {
		if state.Action == action {
			return state, true
		}
	}
	return TaskHubRunControlActionState{}, false
}

func taskHubAdapterCompleteProfile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	catalog := workflowadapter.StandardStageCatalog()
	profile := workflowadapter.ExecutionProfile{ID: "task-hub-integration", Version: "1", ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL, ControlGracePeriod: 30 * time.Second}
	for _, stage := range catalog.Stages {
		turns := stage.RequiredTurns
		profile.Stages = append(profile.Stages, workflowadapter.StageBudget{
			StageKey: stage.Key,
			Budget: workflowkit.ExecutionBudget{
				TurnTimeout:    time.Second,
				MaxTurns:       turns,
				AttemptTimeout: time.Duration(turns) * time.Second,
				MaxAttempts:    1,
				MaxElapsed:     time.Duration(turns) * time.Second,
				Backoff:        workflowkit.BackoffPolicy{},
			},
		})
	}
	return profile
}

func taskHubAdapterSnapshot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for path, contents := range map[string]string{
		"instruction.md":         "Solve the task.\n",
		"task.toml":              "[task]\nname = \"adapter\"\n",
		"tests_analysis.md":      "analysis\n",
		"environment/Dockerfile": "FROM alpine:3.21\n",
		"solution/solve.sh":      "#!/bin/sh\nexit 0\n",
		"tests/test.sh":          "#!/bin/sh\nexit 0\n",
	} {
		path = filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(path, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(contents), mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
