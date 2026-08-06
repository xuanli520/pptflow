package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const taskBoardAuthoringTestBaseImage = "docker.io/library/rust:1.65.0-bullseye@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

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
		BaseImage:      taskBoardAuthoringTestBaseImage,
		Slug:           "task-board-authoring",
		Title:          "Task board authoring",
		TaskType:       "feature",
		Application:    "backend", CodeLang: "rust",
		Objective:    "Add a bounded task board authoring feature",
		MetadataJSON: `{}`,
		Reason:       "exercise task board authoring launch",
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
	evidence := task.Runs[0].AuthoringEvidence
	if evidence == nil || evidence.Contract.Digest == "" || evidence.Contract.TaskID != created.TaskID ||
		evidence.Contract.RepositoryURL != standardAuthoringLaunchTestCoordinate.RepositoryURL ||
		evidence.Contract.CommitSHA != standardAuthoringLaunchTestCoordinate.CommitSHA ||
		evidence.Contract.BaseImage != taskBoardAuthoringTestBaseImage || evidence.Contract.Objective != "Add a bounded task board authoring feature" ||
		len(evidence.Claims) != 0 || len(evidence.Lineage) != 0 {
		t.Fatalf("authoring evidence projection = %+v", evidence)
	}
}

func TestTaskBoardProjectsAndRetriesFailedPreTaskSourceCaptureAfterRestart(t *testing.T) {
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
		failures:   1,
	}
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	services.TaskBoard.actor = func() (string, error) { return "task-board-capture-recovery", nil }
	key, err := services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	request := TaskBoardStartAuthoringRequest{
		IdempotencyKey: key,
		RepositoryURL:  standardAuthoringLaunchTestCoordinate.RepositoryURL,
		CommitSHA:      standardAuthoringLaunchTestCoordinate.CommitSHA,
		BaseImage:      taskBoardAuthoringTestBaseImage,
		Slug:           "pre-task-source-capture-recovery",
		Title:          "Pre-Task source capture recovery",
		TaskType:       "feature",
		Application:    "backend", CodeLang: "rust",
		Objective:    "Recover a failed source capture after the TUI restarts",
		MetadataJSON: `{}`,
		Reason:       "exercise durable pre-Task source capture recovery",
	}
	if _, err := services.TaskBoard.StartAuthoring(ctx, request); err == nil {
		t.Fatal("initial source capture unexpectedly succeeded")
	}
	operation, err := database.GetLifecycleOperationByIdempotencyKey(ctx, key)
	if err != nil || operation == nil || operation.State != store.LifecycleOperationPrepared {
		t.Fatalf("prepared source-capture operation = %+v, %v", operation, err)
	}
	if task, err := database.GetTaskV2(ctx, operation.TaskID); err != nil || task != nil {
		t.Fatalf("failed pre-Task capture created Task = %+v, %v", task, err)
	}

	firstSnapshot, err := services.TaskBoard.List(ctx)
	if err != nil {
		t.Fatalf("list failed pre-Task capture: %v", err)
	}
	if len(firstSnapshot.PendingAuthoringLaunches) != 1 {
		t.Fatalf("pending authoring launches = %+v", firstSnapshot.PendingAuthoringLaunches)
	}
	launch := firstSnapshot.PendingAuthoringLaunches[0]
	if launch.OperationID != operation.ID || launch.RepositoryURL != request.RepositoryURL || launch.CommitSHA != request.CommitSHA ||
		launch.Slug != request.Slug || launch.Title != request.Title || !launch.CanRetry || launch.Status != "source_capture_failed" ||
		launch.FailureCode != standardAuthoringLaunchCaptureFailureCode || launch.FailureSummary != standardAuthoringLaunchCaptureFailureSummary {
		t.Fatalf("failed pre-Task launch projection = %+v", launch)
	}

	// Construct a fresh service graph over the same managed root and store. The
	// retry must recover from durable records rather than the first TUI's form.
	restarted, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	restarted.TaskBoard.actor = func() (string, error) { return "different-local-actor-must-not-change-launch", nil }
	restartedSnapshot, err := restarted.TaskBoard.List(ctx)
	if err != nil || len(restartedSnapshot.PendingAuthoringLaunches) != 1 || restartedSnapshot.PendingAuthoringLaunches[0].OperationID != operation.ID {
		t.Fatalf("restarted failed launch projection = %+v, %v", restartedSnapshot.PendingAuthoringLaunches, err)
	}
	recovered, err := restarted.TaskBoard.RetryAuthoringLaunch(ctx, TaskBoardRetryAuthoringLaunchRequest{OperationID: operation.ID})
	if err != nil {
		t.Fatalf("retry failed pre-Task source capture: %v", err)
	}
	if recovered.OperationID != operation.ID || recovered.TaskID != operation.TaskID || recovered.RunID != operation.RunID || recovered.Summary == "" || capturer.calls != 2 {
		t.Fatalf("recovered source capture = %+v, capture calls=%d", recovered, capturer.calls)
	}
	finalSnapshot, err := restarted.TaskBoard.List(ctx)
	if err != nil || len(finalSnapshot.PendingAuthoringLaunches) != 0 {
		t.Fatalf("resolved pre-Task launch remains visible = %+v, %v", finalSnapshot.PendingAuthoringLaunches, err)
	}
	if task, err := database.GetTaskV2(ctx, operation.TaskID); err != nil || task == nil {
		t.Fatalf("recovered capture Task = %+v, %v", task, err)
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
	jobs, err := fixture.dataStore.ListDurableJobsForRun(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("running Run cancellation unexpectedly queued a second coordinator: %+v", jobs)
	}
}

func TestTaskBoardServiceCancellationWakesWaitingContinuation(t *testing.T) {
	ctx := context.Background()
	fixture := newContinuationFixture(t, store.WorkflowRunRunning)
	fixture.services.TaskBoard.actor = func() (string, error) { return "task-board-cancel", nil }

	jobs, err := fixture.dataStore.ListDurableJobsForRun(ctx, fixture.run.ID)
	if err != nil || len(jobs) != 1 || jobs[0].CommandType != "workflow_run.execute" {
		t.Fatalf("initial workflow coordinator = %+v, %v", jobs, err)
	}
	initial := jobs[0]
	initial, err = fixture.dataStore.TransitionDurableJob(ctx, store.TransitionDurableJobRequest{
		JobID: initial.ID, ExpectedVersion: initial.Version, State: store.JobRunning, Actor: "tester", Reason: "consume initial coordinator",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.dataStore.TransitionDurableJob(ctx, store.TransitionDurableJobRequest{
		JobID: initial.ID, ExpectedVersion: initial.Version, State: store.JobSucceeded, Actor: "tester", Reason: "complete initial coordinator",
	}); err != nil {
		t.Fatal(err)
	}
	waiting, err := fixture.dataStore.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: fixture.run.ID, ExpectedVersion: fixture.run.Version, Status: store.WorkflowRunWaitingContinuation,
		Actor: "tester", Reason: "fixture waits for operator continuation",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.run = waiting
	launcher := &recordingRunActivationLauncher{}
	fixture.services.TaskBoard.activations = &RunActivationService{core: fixture.services.core, launcher: launcher}

	key, err := fixture.services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.services.TaskBoard.CancelRun(ctx, TaskBoardCancelRunRequest{
		IdempotencyKey: key, TaskID: fixture.task.ID, RunID: fixture.run.ID, Reason: "cancel waiting continuation",
	}); err != nil {
		t.Fatalf("cancel waiting continuation: %v", err)
	}
	controls, err := fixture.services.Control.ListForRun(ctx, fixture.run.ID)
	if err != nil || len(controls) != 1 {
		t.Fatalf("termination control = %+v, %v", controls, err)
	}
	jobs, err = fixture.dataStore.ListDurableJobsForRun(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs after waiting-continuation cancellation = %+v", jobs)
	}
	wakeKey := "workflow-run-terminate:" + fixture.run.ID + ":" + controls[0].ID
	var wake *store.DurableJob
	for index := range jobs {
		if jobs[index].IdempotencyKey == wakeKey {
			wake = &jobs[index]
			break
		}
	}
	if wake == nil || wake.State != store.JobQueued || wake.CommandType != "workflow_run.execute" || wake.EntityType != "workflow_run" || wake.EntityID != fixture.run.ID || wake.PayloadJSON != initial.PayloadJSON {
		t.Fatalf("waiting-continuation termination coordinator = %+v", wake)
	}
	launches := launcher.snapshot()
	if len(launches) != 1 || launches[0].RunID != fixture.run.ID {
		t.Fatalf("waiting-continuation cancellation did not launch controlled worker: %+v", launches)
	}
}

func TestTaskBoardRetryAvailabilitySharesRecoveryContractAcrossSubjects(t *testing.T) {
	for _, status := range []store.WorkflowRunStatus{
		store.WorkflowRunFailedRecoverable, store.WorkflowRunInterrupted, store.WorkflowRunCanceled,
		store.WorkflowRunPaused, store.WorkflowRunWaitingContinuation,
	} {
		for _, subject := range []store.WorkflowRunSubjectKind{store.WorkflowRunSubjectAuthoringSession, store.WorkflowRunSubjectTaskRevision} {
			available, reason := taskBoardRetryAvailability(store.WorkflowRun{SubjectKind: subject, Status: status})
			if !available || reason != "" {
				t.Fatalf("subject %s status %s retry availability = available:%t reason:%q", subject, status, available, reason)
			}
		}
	}
	available, reason := taskBoardRetryAvailability(store.WorkflowRun{
		SubjectKind: store.WorkflowRunSubjectAuthoringSession, Status: store.WorkflowRunFailedTerminal,
	})
	if available || reason == "" {
		t.Fatalf("terminal authoring retry availability = available:%t reason:%q", available, reason)
	}
	if strategy := taskBoardRetryStrategy(store.WorkflowRun{SubjectKind: store.WorkflowRunSubjectAuthoringSession}); strategy != TaskBoardRetryStrategyTaskContinuation {
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
		BaseImage:      taskBoardAuthoringTestBaseImage,
		Slug:           "task-board-activation-retry",
		Title:          "Task board activation retry",
		TaskType:       "feature",
		Application:    "backend", CodeLang: "rust",
		Objective:    "Retry a bounded task board authoring launch",
		MetadataJSON: `{}`,
		Reason:       "exercise task board retry",
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

func TestTaskBoardShowsInDoubtStageAndLeaseLossReconcileAction(t *testing.T) {
	projected := (&TaskBoardService{}).projectTaskBoardRun(context.Background(), RunInspection{
		Run:    store.WorkflowRun{ID: "run-lease-lost", SubjectKind: store.WorkflowRunSubjectTaskRevision, Status: store.WorkflowRunInDoubt},
		Stages: []store.StageAttempt{{ID: "attempt-lease-lost", StageKey: "generate_task_files", ExecutionStatus: store.StageExecutionInDoubt}},
		Jobs: []DurableJobInspection{{Job: store.DurableJob{
			ID: "job-lease-lost", CommandType: "stage_attempt.execute", StageAttemptID: "attempt-lease-lost", State: store.JobInDoubt,
			Failure: &store.DurableJobFailure{Code: "job.lease_lost", Message: "The worker lease expired before the job outcome was recorded.", DetailsJSON: `{}`},
		}}},
	}, nil)
	if projected.CurrentStage != "generate_task_files" || projected.FailureCode != "job.lease_lost" || projected.FailureRecoveryAction != TaskBoardFailureRecoveryReconcile {
		t.Fatalf("lease-loss task board projection = %+v", projected)
	}
}

func TestTaskBoardDoesNotExposeRawStageOrWorkerFailureTextWithoutDurableRecord(t *testing.T) {
	projected := (&TaskBoardService{}).projectTaskBoardRun(context.Background(), RunInspection{
		Run: store.WorkflowRun{ID: "run-no-durable-failure", Status: store.WorkflowRunInDoubt},
		Stages: []store.StageAttempt{{
			ID: "attempt-no-durable-failure", StageKey: "materialize_task",
			ErrorText: "provider output from /private/handoff with sk-sensitive-token", FailureClass: "provider",
		}},
	}, []store.RunWorkerHandoff{{FailureReason: "worker output from /private/log"}})
	if projected.FailureSummary != "" || projected.FailureReason != "" || projected.FailureCode != "" || projected.FailureClass != "" {
		t.Fatalf("raw failure text leaked without durable record: %+v", projected)
	}
}

func TestTaskBoardOperatorSummarySeparatesValidationVerdictFromStagePass(t *testing.T) {
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
	services.TaskBoard.actor = func() (string, error) { return "task-board-summary", nil }
	key, err := services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	created, err := services.TaskBoard.StartAuthoring(ctx, TaskBoardStartAuthoringRequest{
		IdempotencyKey: key,
		RepositoryURL:  standardAuthoringLaunchTestCoordinate.RepositoryURL,
		CommitSHA:      standardAuthoringLaunchTestCoordinate.CommitSHA,
		BaseImage:      taskBoardAuthoringTestBaseImage,
		Slug:           "task-board-validation-summary",
		Title:          "Task board validation summary",
		TaskType:       "feature",
		Application:    "backend",
		CodeLang:       "rust",
		Objective:      "Exercise validation summary projection",
		MetadataJSON:   `{}`,
		Reason:         "start validation summary fixture",
	})
	if err != nil {
		t.Fatalf("start authoring fixture: %v", err)
	}
	run, err := database.GetWorkflowRun(ctx, created.RunID)
	if err != nil || run == nil {
		t.Fatalf("load authoring run fixture = %+v, %v", run, err)
	}
	runValue, err := database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning,
		Actor: "task-board-summary", Reason: "run validation summary fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run = &runValue
	resolvedWorkflow, err := workflowadapter.StandardAuthoringCurrentWorkflowTemplate().Compile(standardAuthoringLaunchTestProfile())
	if err != nil {
		t.Fatal(err)
	}
	stage, found := resolvedWorkflow.Descriptor.Stage(workflowkit.StageKey(workflowadapter.HostCandidateVerify))
	if !found {
		t.Fatal("Standard authoring catalog lacks host_candidate_verify")
	}
	inputFingerprint, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := database.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: workflowadapter.HostCandidateVerify, StageGroup: stage.Group, Ordinal: 1,
		InputFingerprint: string(inputFingerprint), BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "task-board-summary", Reason: "create validation summary stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionRunning,
		Actor: "task-board-summary", Reason: "run validation summary stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNodeAttempt(ctx, store.CreateNodeAttemptRequest{
		StageAttemptID: attempt.ID, NodeID: string(stage.Key), Generation: 0, Attempt: 1,
		IdempotencyKey: "task-board-validation-summary-node", Actor: "task-board-summary", Reason: "create validation summary node",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err = database.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
		NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: store.NodeAttemptRunning,
		Actor: "task-board-summary", Reason: "run validation summary node",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err = database.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
		NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: store.NodeAttemptCompleted,
		Actor: "task-board-summary", Reason: "finish validation summary node",
	})
	if err != nil {
		t.Fatal(err)
	}
	subject, err := services.core.resolveWorkflowRunSubject(ctx, *run)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	receipt, err := workflowkit.NewValidationReceipt(workflowkit.ValidationReceipt{
		SnapshotDigest: workflowkit.SHA256Fingerprint([]byte("candidate rejected by tests")),
		ContractDigest: workflowkit.SHA256Fingerprint([]byte("verification contract")),
		Verdict:        workflowkit.ValidationReject,
		FailureCode:    workflowkit.AgentFailureValidatorReject,
		Diagnostics: []workflowkit.AgentCommandReport{{
			CommandID: "oracle_verify", ExitCode: 2, TestStarted: true,
			StdoutTail: "redacted stdout", StderrTail: "redacted stderr",
		}},
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	receiptRaw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := persistStageEvidenceForSubject(ctx, services.core, *run, subject, attempt, node, stage, nil, []StageArtifact{{
		Key: "validation_receipt", SchemaVersion: workflowkit.ValidationReceiptFormat,
		Content: receiptRaw, TurnOrdinal: 1,
	}}, "task-board-summary", "persist rejected validation receipt")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionCompleted,
		Verdict: store.VerdictPass, ArtifactManifestID: manifest.ID,
		Actor: "task-board-summary", Reason: "complete validation summary stage",
	})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := services.TaskBoard.Summary(ctx, TaskBoardRunSummaryRequest{RunID: run.ID})
	if err != nil {
		t.Fatalf("read TaskBoard summary: %v", err)
	}
	if summary.OperatorSummary == nil || summary.OperatorSummary.Status != "validation_rejected" ||
		summary.OperatorSummary.Cause != "rejected at oracle_verify" || summary.OperatorSummary.NextAction != "repair candidate" {
		t.Fatalf("operator summary = %+v", summary.OperatorSummary)
	}
	latest := summary.OperatorSummary.LatestValidation
	if latest == nil || latest.Verdict != string(workflowkit.ValidationReject) || latest.Stage != workflowadapter.HostCandidateVerify ||
		latest.StageAttemptID != attempt.ID || latest.StageExecutionStatus != string(store.StageExecutionCompleted) ||
		latest.StageVerdict != string(store.VerdictPass) || latest.FailureCode != string(workflowkit.AgentFailureValidatorReject) ||
		latest.FailedStep != "oracle_verify" || latest.ExitCode != 2 || !latest.TestStarted {
		t.Fatalf("latest validation summary = %+v", latest)
	}
	snapshot, err := services.TaskBoard.List(ctx)
	if err != nil {
		t.Fatalf("list TaskBoard with validation summary: %v", err)
	}
	task := taskBoardTaskByID(t, snapshot, created.TaskID)
	if task.OperatorSummary == nil || task.OperatorSummary.Status != "validation_rejected" || task.OperatorSummary.LatestValidation == nil ||
		task.OperatorSummary.LatestValidation.StageExecutionStatus != string(store.StageExecutionCompleted) ||
		task.OperatorSummary.LatestValidation.StageVerdict != string(store.VerdictPass) {
		t.Fatalf("task operator summary = %+v", task.OperatorSummary)
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
