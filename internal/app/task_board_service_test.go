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
	"github.com/purplevoid/harbor-factory/internal/testsupport"
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
	if strategy := taskBoardRetryStrategy(store.WorkflowRun{SubjectKind: store.WorkflowRunSubjectAuthoringSession}); strategy != TaskBoardRetryStrategyNone {
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

func TestTaskBoardProjectsAndAdoptsVerifiedEvaluatorEvidence(t *testing.T) {
	ctx := context.Background()
	fixture := newCodeEdgeComplianceFixture(t, codeEdgeComplianceFixtureOptions{stopBeforeHandoff: true})
	fixture.services.TaskBoard.actor = func() (string, error) { return "task-board-evaluator", nil }

	snapshot, err := fixture.services.TaskBoard.List(ctx)
	if err != nil {
		t.Fatalf("list TaskBoard before evaluator evidence adoption: %v", err)
	}
	task := taskBoardTaskByID(t, snapshot, fixture.task.ID)
	if task.Evaluator == nil || task.Evaluator.State != TaskBoardEvaluatorReadyToAdopt || !task.Evaluator.CanAdopt || task.Evaluator.CanLaunch ||
		task.Evaluator.ParentRunID != fixture.run.ID || task.Evaluator.ChildRunID != fixture.childRun.ID {
		t.Fatalf("evaluator projection before adoption = %+v", task.Evaluator)
	}

	preview, err := fixture.services.TaskBoard.PreviewEvaluatorEvidenceHandoff(ctx, TaskBoardEvaluatorEvidenceHandoffPreviewRequest{
		TaskID: fixture.task.ID, ParentRunID: fixture.run.ID, ChildRunID: fixture.childRun.ID,
	})
	if err != nil {
		t.Fatalf("preview evaluator evidence adoption: %v", err)
	}
	if preview.TaskID != fixture.task.ID || preview.ParentRunID != fixture.run.ID || preview.ChildRunID != fixture.childRun.ID ||
		preview.RevisionID != fixture.revision.ID || preview.HandoffFingerprint == "" || preview.QwenTrialFingerprint == "" || preview.OpusTrialFingerprint == "" {
		t.Fatalf("evaluator evidence preview = %+v", preview)
	}

	key, err := fixture.services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	request := TaskBoardEvaluatorEvidenceHandoffRequest{
		IdempotencyKey: key, TaskID: fixture.task.ID, ParentRunID: fixture.run.ID, ChildRunID: fixture.childRun.ID,
		Reason: "adopt verified canonical Qwen and Opus trial evidence",
	}
	prepared, err := fixture.services.TaskBoard.PrepareEvaluatorEvidenceHandoff(ctx, request)
	if err != nil {
		t.Fatalf("prepare evaluator evidence adoption: %v", err)
	}
	if prepared.TaskID != fixture.task.ID || prepared.ParentRunID != fixture.run.ID || prepared.ChildRunID != fixture.childRun.ID ||
		prepared.OperationID == "" || prepared.HandoffFingerprint != preview.HandoffFingerprint ||
		prepared.QwenTrialFingerprint != preview.QwenTrialFingerprint || prepared.OpusTrialFingerprint != preview.OpusTrialFingerprint {
		t.Fatalf("prepared evaluator evidence adoption = %+v; preview=%+v", prepared, preview)
	}
	if handoff, lookupErr := fixture.database.GetCodeEdgeEvaluatorEvidenceHandoffForParentRun(ctx, fixture.run.ID); lookupErr != nil || handoff != nil {
		t.Fatalf("prepare created evaluator evidence handoff = %+v, %v", handoff, lookupErr)
	}

	adopted, err := fixture.services.TaskBoard.AdoptEvaluatorEvidenceHandoff(ctx, request)
	if err != nil {
		t.Fatalf("adopt evaluator evidence through TaskBoard: %v", err)
	}
	if adopted.TaskID != fixture.task.ID || adopted.RunID != fixture.run.ID || adopted.OperationID != prepared.OperationID || adopted.Summary == "" {
		t.Fatalf("adopted evaluator evidence mutation = %+v", adopted)
	}
	handoff, err := fixture.database.GetCodeEdgeEvaluatorEvidenceHandoffForParentRun(ctx, fixture.run.ID)
	if err != nil || handoff == nil || handoff.ID != key || handoff.ChildRunID != fixture.childRun.ID {
		t.Fatalf("persisted evaluator evidence handoff = %+v, %v", handoff, err)
	}
	replayed, err := fixture.services.TaskBoard.AdoptEvaluatorEvidenceHandoff(ctx, request)
	if err != nil || replayed.OperationID != adopted.OperationID || replayed.RunID != adopted.RunID {
		t.Fatalf("replayed TaskBoard evidence adoption = %+v, %v; first=%+v", replayed, err, adopted)
	}

	snapshot, err = fixture.services.TaskBoard.List(ctx)
	if err != nil {
		t.Fatalf("list TaskBoard after evaluator evidence adoption: %v", err)
	}
	task = taskBoardTaskByID(t, snapshot, fixture.task.ID)
	if task.Evaluator == nil || task.Evaluator.State != TaskBoardEvaluatorAdopted || task.Evaluator.CanLaunch || task.Evaluator.CanAdopt ||
		task.Evaluator.ParentRunID != fixture.run.ID || task.Evaluator.ChildRunID != fixture.childRun.ID {
		t.Fatalf("evaluator projection after adoption = %+v", task.Evaluator)
	}
}

func TestTaskBoardEvaluatorEvidenceAdoptionRejectsParentCheckpointDrift(t *testing.T) {
	ctx := context.Background()
	fixture := newCodeEdgeComplianceFixture(t, codeEdgeComplianceFixtureOptions{stopBeforeHandoff: true})
	fixture.services.TaskBoard.actor = func() (string, error) { return "task-board-evaluator-drift", nil }
	key, err := fixture.services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	request := TaskBoardEvaluatorEvidenceHandoffRequest{
		IdempotencyKey: key, TaskID: fixture.task.ID, ParentRunID: fixture.run.ID, ChildRunID: fixture.childRun.ID,
		Reason: "reject stale evaluator evidence adoption after parent checkpoint drift",
	}
	if _, err := fixture.services.TaskBoard.PrepareEvaluatorEvidenceHandoff(ctx, request); err != nil {
		t.Fatalf("prepare evaluator evidence adoption before parent drift: %v", err)
	}
	parent, err := fixture.database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: fixture.run.ID, ExpectedVersion: fixture.run.Version, Status: store.WorkflowRunWaitingContinuation,
		Actor: "task-board-evaluator-drift", Reason: "advance parent checkpoint after evidence prepare",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: parent.ID, ExpectedVersion: parent.Version, Status: store.WorkflowRunRunning,
		Actor: "task-board-evaluator-drift", Reason: "restore parent fixture after checkpoint drift",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.services.TaskBoard.AdoptEvaluatorEvidenceHandoff(ctx, request); !errors.Is(err, store.ErrOptimisticLock) {
		t.Fatalf("stale evaluator evidence adoption error = %v, want %v", err, store.ErrOptimisticLock)
	}
	if handoff, lookupErr := fixture.database.GetCodeEdgeEvaluatorEvidenceHandoffForParentRun(ctx, fixture.run.ID); lookupErr != nil || handoff != nil {
		t.Fatalf("stale evaluator evidence adoption persisted handoff = %+v, %v", handoff, lookupErr)
	}
}

func TestTaskBoardLaunchesEvaluatorThroughControlledWorkerHandoff(t *testing.T) {
	ctx := context.Background()
	fixture := newTaskBoardEvaluatorLaunchFixture(t, ctx)
	services, database := fixture.services, fixture.database
	task, revision, parent, launcher := fixture.task, fixture.revision, fixture.parent, fixture.launcher

	snapshot, err := services.TaskBoard.List(ctx)
	if err != nil {
		t.Fatalf("list TaskBoard before evaluator launch: %v", err)
	}
	projected := taskBoardTaskByID(t, snapshot, task.ID)
	if projected.Evaluator == nil || projected.Evaluator.State != TaskBoardEvaluatorReadyToLaunch || !projected.Evaluator.CanLaunch || projected.Evaluator.CanAdopt ||
		projected.Evaluator.ParentRunID != parent.ID || projected.Evaluator.ChildRunID != "" {
		t.Fatalf("evaluator launch projection = %+v", projected.Evaluator)
	}
	preview, err := services.TaskBoard.PreviewEvaluatorLaunch(ctx, TaskBoardEvaluatorLaunchPreviewRequest{TaskID: task.ID, ParentRunID: parent.ID})
	if err != nil {
		t.Fatalf("preview TaskBoard evaluator launch: %v", err)
	}
	if preview.TaskID != task.ID || preview.ParentRunID != parent.ID || preview.RevisionID != revision.ID || preview.TemplateID != workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID ||
		preview.TemplateVersion != workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion || preview.ExecutionProfileFingerprint == "" || preview.ExecutionSpecFingerprint == "" {
		t.Fatalf("TaskBoard evaluator launch preview = %+v", preview)
	}
	missingPrepareKey, err := services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, confirmErr := services.TaskBoard.ConfirmEvaluatorLaunch(ctx, TaskBoardEvaluatorLaunchRequest{
		IdempotencyKey: missingPrepareKey, TaskID: task.ID, ParentRunID: parent.ID,
		Reason: "reject an evaluator launch without frozen inputs",
	}); confirmErr == nil {
		t.Fatal("TaskBoard evaluator confirm without prepare unexpectedly succeeded")
	}
	if len(launcher.requests) != 0 {
		t.Fatalf("TaskBoard evaluator confirm without prepare launched workers: %+v", launcher.requests)
	}

	key, err := services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	request := TaskBoardEvaluatorLaunchRequest{
		IdempotencyKey: key, TaskID: task.ID, ParentRunID: parent.ID,
		Reason: "launch the approved Qwen and Opus evaluator child",
	}
	prepared, err := services.TaskBoard.PrepareEvaluatorLaunch(ctx, request)
	if err != nil {
		t.Fatalf("prepare TaskBoard evaluator launch: %v", err)
	}
	if prepared.TaskID != task.ID || prepared.ParentRunID != parent.ID || prepared.InputBundleID != key ||
		prepared.ExecutionProfileFingerprint == "" || prepared.ExecutionSpecFingerprint == "" {
		t.Fatalf("prepared TaskBoard evaluator launch = %+v", prepared)
	}
	runs, err := database.ListWorkflowRunsForTask(ctx, task.ID)
	if err != nil || len(runs) != 1 || runs[0].ID != parent.ID {
		t.Fatalf("prepare created evaluator child Run: %+v, %v", runs, err)
	}
	secondKey, err := services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	secondRequest := TaskBoardEvaluatorLaunchRequest{
		IdempotencyKey: secondKey, TaskID: task.ID, ParentRunID: parent.ID,
		Reason: "prepare a competing evaluator child before the first confirmation",
	}
	if _, prepareErr := services.TaskBoard.PrepareEvaluatorLaunch(ctx, secondRequest); prepareErr != nil {
		t.Fatalf("prepare competing evaluator launch before first confirmation: %v", prepareErr)
	}

	launched, err := services.TaskBoard.ConfirmEvaluatorLaunch(ctx, request)
	if err != nil {
		t.Fatalf("confirm TaskBoard evaluator launch: %v", err)
	}
	if launched.TaskID != task.ID || launched.RunID == "" || launched.RunID == parent.ID || launched.OperationID == "" {
		t.Fatalf("TaskBoard evaluator launch mutation = %+v", launched)
	}
	if len(launcher.requests) != 1 || launcher.requests[0].RunID != launched.RunID {
		t.Fatalf("TaskBoard evaluator worker launch = %+v", launcher.requests)
	}
	replayed, err := services.TaskBoard.ConfirmEvaluatorLaunch(ctx, request)
	if err != nil || replayed.RunID != launched.RunID || replayed.OperationID != launched.OperationID || len(launcher.requests) != 1 {
		t.Fatalf("replayed TaskBoard evaluator launch = %+v, %v; first=%+v workers=%+v", replayed, err, launched, launcher.requests)
	}
	if _, secondErr := services.TaskBoard.ConfirmEvaluatorLaunch(ctx, secondRequest); !errors.Is(secondErr, ErrCodeEdgeEvaluatorChildAlreadyExists) {
		t.Fatalf("competing evaluator confirmation error = %v, want %v", secondErr, ErrCodeEdgeEvaluatorChildAlreadyExists)
	}
	if len(launcher.requests) != 1 {
		t.Fatalf("competing evaluator confirmation launched another worker: %+v", launcher.requests)
	}
	runs, err = database.ListWorkflowRunsForTask(ctx, task.ID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("competing evaluator confirmation changed child Run count: %+v, %v", runs, err)
	}
	if _, previewErr := services.TaskBoard.PreviewEvaluatorLaunch(ctx, TaskBoardEvaluatorLaunchPreviewRequest{TaskID: task.ID, ParentRunID: parent.ID}); !errors.Is(previewErr, ErrCodeEdgeEvaluatorChildAlreadyExists) {
		t.Fatalf("second evaluator launch preview error = %v, want %v", previewErr, ErrCodeEdgeEvaluatorChildAlreadyExists)
	}

	snapshot, err = services.TaskBoard.List(ctx)
	if err != nil {
		t.Fatalf("list TaskBoard after evaluator launch: %v", err)
	}
	projected = taskBoardTaskByID(t, snapshot, task.ID)
	if projected.Evaluator == nil || projected.Evaluator.State != TaskBoardEvaluatorChildActive || projected.Evaluator.CanLaunch || projected.Evaluator.CanAdopt ||
		projected.Evaluator.ParentRunID != parent.ID || projected.Evaluator.ChildRunID != launched.RunID {
		t.Fatalf("evaluator child-active projection = %+v", projected.Evaluator)
	}
}

func TestTaskBoardEvaluatorLaunchRejectsParentCheckpointDriftAfterPrepare(t *testing.T) {
	ctx := context.Background()
	fixture := newTaskBoardEvaluatorLaunchFixture(t, ctx)
	key, err := fixture.services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	request := TaskBoardEvaluatorLaunchRequest{
		IdempotencyKey: key, TaskID: fixture.task.ID, ParentRunID: fixture.parent.ID,
		Reason: "reject stale evaluator confirmation after parent checkpoint drift",
	}
	if _, err := fixture.services.TaskBoard.PrepareEvaluatorLaunch(ctx, request); err != nil {
		t.Fatalf("prepare evaluator launch before parent drift: %v", err)
	}
	parent, err := fixture.database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: fixture.parent.ID, ExpectedVersion: fixture.parent.Version, Status: store.WorkflowRunWaitingContinuation,
		Actor: "task-board-evaluator", Reason: "advance parent checkpoint after evaluator prepare",
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, err = fixture.database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: parent.ID, ExpectedVersion: parent.Version, Status: store.WorkflowRunRunning,
		Actor: "task-board-evaluator", Reason: "restore parent fixture after checkpoint drift",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.services.TaskBoard.ConfirmEvaluatorLaunch(ctx, request); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("stale evaluator confirmation error = %v, want %v", err, store.ErrIdempotencyConflict)
	}
	if len(fixture.launcher.requests) != 0 {
		t.Fatalf("stale evaluator confirmation launched a worker: %+v", fixture.launcher.requests)
	}
	runs, err := fixture.database.ListWorkflowRunsForTask(ctx, fixture.task.ID)
	if err != nil || len(runs) != 1 || runs[0].ID != fixture.parent.ID {
		t.Fatalf("stale evaluator confirmation created a child Run: %+v, %v", runs, err)
	}
}

type taskBoardEvaluatorLaunchFixture struct {
	database *store.Store
	services *LifecycleServices
	task     store.TaskV2
	revision store.TaskRevision
	parent   store.WorkflowRun
	launcher *recordingTaskBoardEvaluatorWorkerLauncher
}

func newTaskBoardEvaluatorLaunchFixture(t *testing.T, ctx context.Context) taskBoardEvaluatorLaunchFixture {
	t.Helper()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	provider := &taskBoardEvaluatorDefinitionProvider{profile: codeEdgeEvaluatorRuntimeProfile(t)}
	launcher := &recordingTaskBoardEvaluatorWorkerLauncher{}
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver:              testsupport.AcceptAllStageOperationResolver(),
		EvaluatorRunDefinitionProvider: provider,
		RunWorkerHandoffLauncher:       launcher,
	})
	if err != nil {
		t.Fatal(err)
	}
	services.TaskBoard.actor = func() (string, error) { return "task-board-evaluator", nil }
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{
			Slug: "task-board-evaluator-launch", Title: "Task Board Evaluator Launch", Actor: "task-board-evaluator", Reason: "create evaluator launch fixture",
		},
		SourceDirectory: writeLifecycleSnapshot(t, "TaskBoard evaluator launch fixture\n"),
		ChangeSummary:   "create immutable evaluator launch fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	provider.spec = testsupport.CompleteCodeEdgeEvaluatorChildRunExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	parent, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID,
		Profile:       codeEdgePhase1RuntimeProfile(t),
		ExecutionSpec: testsupport.CompleteCodeEdgePhase1RunExecutionSpec(task.ID, revision.ID, revision.TaskDigest),
		Trigger:       "task-board-evaluator-launch", Actor: "task-board-evaluator", Reason: "freeze evaluator parent fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, err = database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: parent.ID, ExpectedVersion: parent.Version, Status: store.WorkflowRunRunning,
		Actor: "task-board-evaluator", Reason: "open approved final review fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	parent = seedApprovedCodeEdgeReviewGate(t, ctx, services, parent, revision, workflowadapter.FinalReview, workflowadapter.ReviewFinalQuality)
	parent, err = database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: parent.ID, ExpectedVersion: parent.Version, Status: store.WorkflowRunRunning,
		Actor: "task-board-evaluator", Reason: "continue after approved final review fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return taskBoardEvaluatorLaunchFixture{
		database: database, services: services, task: task, revision: revision, parent: parent, launcher: launcher,
	}
}

type taskBoardEvaluatorDefinitionProvider struct {
	profile workflowadapter.ExecutionProfile
	spec    workflowadapter.RunExecutionSpec
}

func (provider *taskBoardEvaluatorDefinitionProvider) DefinitionForEvaluatorRun(context.Context, EvaluatorRunDefinitionRequest) (EvaluatorRunDefinition, error) {
	return EvaluatorRunDefinition{Profile: provider.profile.Clone(), ExecutionSpec: provider.spec.Clone()}, nil
}

type recordingTaskBoardEvaluatorWorkerLauncher struct {
	requests []RunWorkerHandoffLaunchRequest
}

func (launcher *recordingTaskBoardEvaluatorWorkerLauncher) LaunchRunWorker(_ context.Context, request RunWorkerHandoffLaunchRequest) (RunWorkerHandoffLaunchReceipt, error) {
	launcher.requests = append(launcher.requests, request)
	return RunWorkerHandoffLaunchReceipt{
		RunID: request.RunID, Owner: request.Owner, ProcessID: 9900 + len(launcher.requests),
		LogPath: filepath.Join("/tmp", "task-board-evaluator-"+request.HandoffOperationID+".log"),
	}, nil
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
