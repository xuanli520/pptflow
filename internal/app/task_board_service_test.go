package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestTaskBoardServiceStartsStandardAuthoringAndProjectsDraft(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	capturer := &standardAuthoringSourceCapturerFixture{
		coordinate: standardAuthoringLaunchTestCoordinate,
		snapshot:   standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate),
	}
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	services.TaskBoard.actor = func() (string, error) { return "task-board-test", nil }
	key, err := services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}

	created, err := services.TaskBoard.StartAuthoring(ctx, TaskBoardStartAuthoringRequest{
		IdempotencyKey: key,
		RepositoryURL:  standardAuthoringLaunchTestCoordinate.RepositoryURL,
		CommitSHA:      standardAuthoringLaunchTestCoordinate.CommitSHA,
		Slug:           "task-board-authoring",
		Title:          "Task board authoring",
		MetadataJSON:   `{}`,
		Reason:         "exercise task board authoring launch",
	})
	if err != nil {
		t.Fatalf("start task board authoring: %v", err)
	}
	if created.TaskID == "" || created.RunID == "" || capturer.calls != 1 {
		t.Fatalf("task board authoring result = %+v, captures=%d", created, capturer.calls)
	}

	snapshot, err := services.TaskBoard.List(ctx)
	if err != nil {
		t.Fatalf("list task board: %v", err)
	}
	task := taskBoardTaskByID(t, snapshot, created.TaskID)
	if task.Column != TaskBoardPending || task.RunID != created.RunID || task.RunStatus != string(store.WorkflowRunQueued) || task.RepositoryURL != standardAuthoringLaunchTestCoordinate.RepositoryURL || len(task.Runs) != 1 || task.Runs[0].ID != created.RunID {
		t.Fatalf("task board projection = %+v", task)
	}
}

func TestTaskBoardServiceRetriesTheSelectedTaskRevisionRun(t *testing.T) {
	ctx := context.Background()
	fixture := newContinuationFixture(t, store.WorkflowRunFailedRecoverable)
	fixture.services.TaskBoard.actor = func() (string, error) { return "task-board-retry", nil }
	key, err := fixture.services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.services.TaskBoard.RetryRun(ctx, TaskBoardRetryRunRequest{
		IdempotencyKey: key, TaskID: fixture.task.ID, RunID: fixture.run.ID, Reason: "retry selected recoverable Run",
	})
	if err != nil {
		t.Fatalf("retry task board Run: %v", err)
	}
	if result.TaskID != fixture.task.ID || result.RunID != fixture.run.ID || result.Summary == "" {
		t.Fatalf("retry result = %+v", result)
	}
}

func TestTaskBoardServiceRecoversAuthoringRunThroughDedicatedPath(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	capturer := &standardAuthoringSourceCapturerFixture{
		coordinate: standardAuthoringLaunchTestCoordinate,
		snapshot:   standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate),
	}
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	services.TaskBoard.actor = func() (string, error) { return "task-board-authoring-recovery", nil }
	launchKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := services.AuthoringLaunches.Start(ctx, StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: launchKey, Actor: "author", Reason: "create recoverable authoring board fixture"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL,
		CommitSHA:                    standardAuthoringLaunchTestCoordinate.CommitSHA,
		Slug:                         "task-board-authoring-recovery",
		Title:                        "Task board authoring recovery",
		MetadataJSON:                 `{}`,
	})
	if err != nil {
		t.Fatalf("launch authoring fixture: %v", err)
	}
	storedRun, err := database.GetWorkflowRun(ctx, receipt.RunID)
	if err != nil || storedRun == nil {
		t.Fatalf("load authoring Run: %+v, %v", storedRun, err)
	}
	run := *storedRun
	run, err = database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "worker", Reason: "start authoring recovery fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := database.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: workflowadapter.RepoPrepare, StageGroup: string(workflowadapter.StageSourcePrepare), Ordinal: 1,
		InputFingerprint: "sha256:authoring-board-recovery-input", BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "worker", Reason: "create recoverable authoring stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionRunning,
		Actor: "worker", Reason: "start recoverable authoring stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionInfraFailed,
		ErrorText: "transient provider network failure", FailureClass: string(workflowkit.FailureNetwork), Actor: "worker", Reason: "record recoverable authoring failure",
	}); err != nil {
		t.Fatal(err)
	}
	run, err = database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunFailedRecoverable, Actor: "worker", Reason: "project recoverable authoring failure",
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := services.TaskBoard.List(ctx)
	if err != nil {
		t.Fatalf("project recoverable authoring Run: %v", err)
	}
	task := taskBoardTaskByID(t, snapshot, receipt.TaskID)
	if len(task.Runs) != 1 || !task.Runs[0].CanRetry || task.Runs[0].RetryStrategy != TaskBoardRetryStrategyAuthoringRecovery {
		t.Fatalf("authoring recovery board projection = %+v", task.Runs)
	}
	recoveryKey, err := services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	recoveryRequest := TaskBoardRetryRunRequest{
		IdempotencyKey: recoveryKey, TaskID: receipt.TaskID, RunID: run.ID, Reason: "recover transient provider failure",
	}
	services.TaskBoard.activations = &RunActivationService{core: services.core, launcher: failingTaskBoardActivationLauncher{}}
	if _, err := services.TaskBoard.RetryRun(ctx, recoveryRequest); err == nil {
		t.Fatal("authoring recovery unexpectedly succeeded while activation failed")
	}
	services.TaskBoard.activations = nil
	result, err := services.TaskBoard.RetryRun(ctx, recoveryRequest)
	if err != nil {
		t.Fatalf("replay authoring recovery through board: %v", err)
	}
	if result.TaskID != receipt.TaskID || result.RunID != run.ID || result.Summary == "" {
		t.Fatalf("authoring recovery board result = %+v", result)
	}
	updated, err := database.GetWorkflowRun(ctx, run.ID)
	if err != nil || updated == nil || updated.ExecutionEpoch != run.ExecutionEpoch+1 {
		t.Fatalf("recovered authoring Run = %+v, %v", updated, err)
	}
	jobs, err := database.ListDurableJobsForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var recoveries int
	for _, job := range jobs {
		if job.CommandType == "task_continuation.execute" {
			recoveries++
		}
	}
	if recoveries != 1 {
		t.Fatalf("authoring recovery jobs = %+v", jobs)
	}
}

func TestTaskBoardServiceCancelsTheSelectedRun(t *testing.T) {
	ctx := context.Background()
	fixture := newContinuationFixture(t, store.WorkflowRunRunning)
	fixture.services.TaskBoard.actor = func() (string, error) { return "task-board-cancel", nil }
	key, err := fixture.services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.services.TaskBoard.CancelRun(ctx, TaskBoardCancelRunRequest{
		IdempotencyKey: key, TaskID: fixture.task.ID, RunID: fixture.run.ID, Reason: "cancel selected Run",
	})
	if err != nil {
		t.Fatalf("cancel task board Run: %v", err)
	}
	if result.TaskID != fixture.task.ID || result.RunID != fixture.run.ID || result.Summary == "" {
		t.Fatalf("cancel result = %+v", result)
	}
	controls, err := fixture.services.Control.ListForRun(ctx, fixture.run.ID)
	if err != nil || len(controls) != 1 || controls[0].Action != store.ControlActionTerminate {
		t.Fatalf("cancel controls = %+v, %v", controls, err)
	}
}

func TestTaskBoardRetryAvailabilityDoesNotOfferUnsupportedAuthoringRetry(t *testing.T) {
	available, reason := taskBoardRetryAvailability(store.WorkflowRun{
		SubjectKind: store.WorkflowRunSubjectAuthoringSession, Status: store.WorkflowRunFailedRecoverable,
	})
	if available || reason == "" {
		t.Fatalf("authoring retry availability = available:%t reason:%q", available, reason)
	}
	available, reason = taskBoardRetryAvailability(store.WorkflowRun{
		SubjectKind: store.WorkflowRunSubjectTaskRevision, Status: store.WorkflowRunFailedRecoverable,
	})
	if !available || reason != "" {
		t.Fatalf("task revision retry availability = available:%t reason:%q", available, reason)
	}
	if strategy := taskBoardRetryStrategy(store.WorkflowRun{SubjectKind: store.WorkflowRunSubjectAuthoringSession}); strategy != TaskBoardRetryStrategyAuthoringRecovery {
		t.Fatalf("authoring retry strategy = %q", strategy)
	}
	if strategy := taskBoardRetryStrategy(store.WorkflowRun{SubjectKind: store.WorkflowRunSubjectTaskRevision}); strategy != TaskBoardRetryStrategyTaskContinuation {
		t.Fatalf("task retry strategy = %q", strategy)
	}
}

func TestTaskBoardServiceReadsTheSelectedRunWorkerLog(t *testing.T) {
	ctx := context.Background()
	fixture := newContinuationFixture(t, store.WorkflowRunRunning)
	logPath := filepath.Join(t.TempDir(), "worker.log")
	if err := os.WriteFile(logPath, []byte("first line\nsecond line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := fixture.services.WorkerHandoffs.ReserveRunWorkerHandoff(ctx, ReserveRunWorkerHandoffCommand{
		IdempotencyKey: key,
		RunID:          fixture.run.ID,
		Expected: RunWorkerHandoffCheckpoint{
			RunVersion: fixture.run.Version, ExecutionEpoch: fixture.run.ExecutionEpoch, DefinitionHash: fixture.run.DefinitionHash,
		},
		Owner: "task-board-log-reader", Actor: "tester", Reason: "attach test log", LaunchTTL: time.Minute,
	})
	if err != nil || !reserved.Launch {
		t.Fatalf("reserve log handoff = %+v, %v", reserved, err)
	}
	if _, err := fixture.services.WorkerHandoffs.RecordRunWorkerHandoffSpawned(ctx, reserved.Handoff.ID, 4242, logPath, "tester", "record test log"); err != nil {
		t.Fatalf("record log handoff: %v", err)
	}
	log, err := fixture.services.TaskBoard.ReadRunLog(ctx, TaskBoardReadRunLogRequest{TaskID: fixture.task.ID, RunID: fixture.run.ID})
	if err != nil || log.Path != logPath || log.Content != "first line\nsecond line\n" {
		t.Fatalf("read task board log = %+v, %v", log, err)
	}
}

type failingTaskBoardActivationLauncher struct{}

func (failingTaskBoardActivationLauncher) LaunchRunWorker(context.Context, RunWorkerHandoffLaunchRequest) (RunWorkerHandoffLaunchReceipt, error) {
	return RunWorkerHandoffLaunchReceipt{}, errors.New("simulate task board activation failure")
}

func TestTaskBoardServiceReplaysAuthoringLaunchAfterActivationFailure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	capturer := &standardAuthoringSourceCapturerFixture{
		coordinate: standardAuthoringLaunchTestCoordinate,
		snapshot:   standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate),
	}
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	services.TaskBoard.actor = func() (string, error) { return "task-board-test", nil }
	services.TaskBoard.activations = &RunActivationService{core: services.core, launcher: failingTaskBoardActivationLauncher{}}
	key, err := services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	request := TaskBoardStartAuthoringRequest{
		IdempotencyKey: key,
		RepositoryURL:  standardAuthoringLaunchTestCoordinate.RepositoryURL,
		CommitSHA:      standardAuthoringLaunchTestCoordinate.CommitSHA,
		Slug:           "task-board-activation-retry",
		Title:          "Task board activation retry",
		MetadataJSON:   `{}`,
		Reason:         "exercise task board retry",
	}
	if _, err := services.TaskBoard.StartAuthoring(ctx, request); err == nil {
		t.Fatal("authoring launch unexpectedly succeeded while activation failed")
	}
	services.TaskBoard.activations = nil
	replayed, err := services.TaskBoard.StartAuthoring(ctx, request)
	if err != nil {
		t.Fatalf("replay authoring launch after activation failure: %v", err)
	}
	snapshot, err := services.TaskBoard.List(ctx)
	if err != nil || len(snapshot.Tasks) != 1 || snapshot.Tasks[0].ID != replayed.TaskID || capturer.calls != 1 {
		t.Fatalf("replayed authoring launch = %+v tasks=%+v captures=%d err=%v", replayed, snapshot.Tasks, capturer.calls, err)
	}
}

func TestTaskBoardServiceDecidesAuthoringAndRevisionReviewsThroughTheirOwnContracts(t *testing.T) {
	ctx := context.Background()
	services, database := newAuthoringReviewServiceFixture(t)
	defer database.Close()
	services.TaskBoard.actor = func() (string, error) { return "task-board-reviewer", nil }
	opened := openAuthoringReviewServiceGate(t, ctx, database)
	session, err := database.GetAuthoringSession(ctx, opened.Request.AuthoringSessionID)
	if err != nil || session == nil {
		t.Fatalf("load authoring session: %+v, %v", session, err)
	}

	snapshot, err := services.TaskBoard.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	authoringTask := taskBoardTaskByID(t, snapshot, session.TargetTaskID)
	if authoringTask.Review == nil || authoringTask.Review.Kind != TaskBoardAuthoringReview || authoringTask.Review.RequestID != opened.Request.ID {
		t.Fatalf("authoring review projection = %+v", authoringTask)
	}
	authoringKey, err := services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.TaskBoard.DecideReview(ctx, TaskBoardDecideReviewRequest{
		IdempotencyKey: authoringKey, TaskID: session.TargetTaskID, Review: *authoringTask.Review, Decision: TaskBoardApprove, Reason: "approve authoring fixture",
	}); err != nil {
		t.Fatalf("approve authoring board review: %v", err)
	}
	authoringDecisions, err := database.ListAuthoringReviewDecisionsForRequest(ctx, opened.Request.ID)
	if err != nil || len(authoringDecisions) != 1 || authoringDecisions[0].Action != store.ReviewDecisionApprove || authoringDecisions[0].Actor != "task-board-reviewer" {
		t.Fatalf("authoring board decisions = %+v, %v", authoringDecisions, err)
	}

	_, revisionServices := newLifecycleMutationTestServices(t)
	revisionServices.TaskBoard.actor = func() (string, error) { return "task-board-reviewer", nil }
	task, revision, err := revisionServices.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "task-board-review", Title: "Task board review", MetadataJSON: `{}`, Actor: "fixture", Reason: "create review fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "task board review fixture\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	revision, err = revisionServices.Revisions.MarkValidated(ctx, revision.ID, revision.StateVersion, "sha256:task-board-review-evidence", "fixture", "validate review fixture")
	if err != nil {
		t.Fatal(err)
	}
	review, err := revisionServices.Reviews.Request(ctx, revision.ID, revision.ValidationEvidenceManifest, "fixture", "request review fixture")
	if err != nil {
		t.Fatal(err)
	}
	revisionSnapshot, err := revisionServices.TaskBoard.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	revisionTask := taskBoardTaskByID(t, revisionSnapshot, task.ID)
	if revisionTask.Review == nil || revisionTask.Review.Kind != TaskBoardRevisionReview || revisionTask.Review.RevisionID != revision.ID {
		t.Fatalf("revision review projection = %+v", revisionTask)
	}
	revisionKey, err := revisionServices.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := revisionServices.TaskBoard.DecideReview(ctx, TaskBoardDecideReviewRequest{
		IdempotencyKey: revisionKey, TaskID: task.ID, Review: *revisionTask.Review, Decision: TaskBoardRequestChanges, Reason: "request revision changes",
	}); err != nil {
		t.Fatalf("request changes through task board: %v", err)
	}
	decisions, err := revisionServices.Store().ListReviewDecisionsForRequest(ctx, review.ID)
	if err != nil || len(decisions) != 1 || decisions[0].Action != store.ReviewDecisionRequestChanges || decisions[0].Actor != "task-board-reviewer" {
		t.Fatalf("revision board decisions = %+v, %v", decisions, err)
	}
}

func TestTaskBoardServiceReplaysAuthoringReviewAfterActivationFailure(t *testing.T) {
	ctx := context.Background()
	services, database := newAuthoringReviewServiceFixture(t)
	defer database.Close()
	services.TaskBoard.actor = func() (string, error) { return "task-board-reviewer", nil }
	opened := openAuthoringReviewServiceGate(t, ctx, database)
	session, err := database.GetAuthoringSession(ctx, opened.Request.AuthoringSessionID)
	if err != nil || session == nil {
		t.Fatalf("load authoring session: %+v, %v", session, err)
	}
	key, err := services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	request := TaskBoardDecideReviewRequest{
		IdempotencyKey: key,
		TaskID:         session.TargetTaskID,
		Review:         TaskBoardReview{Kind: TaskBoardAuthoringReview, RequestID: opened.Request.ID},
		Decision:       TaskBoardApprove,
		Reason:         "approve authoring retry fixture",
	}
	services.TaskBoard.activations = &RunActivationService{core: services.core, launcher: failingTaskBoardActivationLauncher{}}
	if _, err := services.TaskBoard.DecideReview(ctx, request); err == nil {
		t.Fatal("authoring review unexpectedly succeeded while activation failed")
	}
	services.TaskBoard.activations = nil
	replayed, err := services.TaskBoard.DecideReview(ctx, request)
	if err != nil || replayed.RunID != opened.Run.ID {
		t.Fatalf("replay authoring review = %+v err=%v", replayed, err)
	}
	decisions, err := database.ListAuthoringReviewDecisionsForRequest(ctx, opened.Request.ID)
	if err != nil || len(decisions) != 1 || decisions[0].IdempotencyKey != key {
		t.Fatalf("replayed authoring review decisions = %+v err=%v", decisions, err)
	}
}

func TestTaskBoardServiceRejectsAuthoringReviewForAnotherTask(t *testing.T) {
	ctx := context.Background()
	services, database := newAuthoringReviewServiceFixture(t)
	defer database.Close()
	services.TaskBoard.actor = func() (string, error) { return "task-board-reviewer", nil }
	opened := openAuthoringReviewServiceGate(t, ctx, database)
	other, err := database.CreateTaskV2(ctx, store.CreateTaskV2Request{
		Slug: "task-board-unrelated", Title: "Unrelated task", Actor: "fixture", Reason: "create unrelated task",
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	_, err = services.TaskBoard.DecideReview(ctx, TaskBoardDecideReviewRequest{
		IdempotencyKey: key,
		TaskID:         other.ID,
		Review:         TaskBoardReview{Kind: TaskBoardAuthoringReview, RequestID: opened.Request.ID},
		Decision:       TaskBoardApprove,
		Reason:         "must not decide another task review",
	})
	if !errors.Is(err, store.ErrImmutable) {
		t.Fatalf("cross-task authoring review error = %v, want ErrImmutable", err)
	}
	decisions, err := database.ListAuthoringReviewDecisionsForRequest(ctx, opened.Request.ID)
	if err != nil || len(decisions) != 0 {
		t.Fatalf("cross-task authoring review changed gate: decisions=%+v err=%v", decisions, err)
	}
}

func TestTaskBoardColumnKeepsRecoverableRunsOutOfCompleted(t *testing.T) {
	for _, status := range []store.WorkflowRunStatus{
		store.WorkflowRunFailedRecoverable,
		store.WorkflowRunInterrupted,
		store.WorkflowRunInDoubt,
	} {
		if column := taskBoardColumnForRun(status); column != TaskBoardPending {
			t.Fatalf("status %s column = %s, want %s", status, column, TaskBoardPending)
		}
	}
}

func taskBoardTaskByID(t *testing.T, snapshot TaskBoardSnapshot, id string) TaskBoardTask {
	t.Helper()
	for _, task := range snapshot.Tasks {
		if task.ID == id {
			return task
		}
	}
	t.Fatalf("task board snapshot does not contain %s: %+v", id, snapshot.Tasks)
	return TaskBoardTask{}
}
