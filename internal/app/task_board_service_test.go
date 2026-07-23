package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
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
		Application:    "backend",
		Objective:      "Add a bounded task board authoring feature",
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
		Application:    "backend",
		Objective:      "Recover a failed source capture after the TUI restarts",
		MetadataJSON:   `{}`,
		Reason:         "exercise durable pre-Task source capture recovery",
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

func TestTaskBoardServiceRecoversMissingOutputSubmissionProcessFailureThroughDedicatedPath(t *testing.T) {
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
		BaseImage:                    taskBoardAuthoringTestBaseImage,
		Slug:                         "task-board-authoring-recovery",
		Title:                        "Task board authoring recovery",
		TaskType:                     "feature",
		Application:                  "backend",
		Objective:                    "Recover a bounded task board authoring run",
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
		ErrorText: "standard_authoring_codex_agent_turn.output_submission_missing", FailureClass: string(workflowkit.FailureProcess), Actor: "worker", Reason: "record missing-output process failure",
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
	preview, err := services.TaskBoard.PreviewRunRecovery(ctx, TaskBoardPreviewRunRecoveryRequest{
		TaskID: receipt.TaskID, RunID: run.ID, Reason: "inspect missing output recovery boundary",
	})
	if err != nil {
		t.Fatalf("preview missing-output authoring recovery: %v", err)
	}
	recoveryRequest := TaskBoardRetryRunRequest{
		IdempotencyKey: recoveryKey, TaskID: receipt.TaskID, RunID: run.ID, Reason: "recover missing structured output submission",
		ExpectedRecoveryCheckpoint: &preview.Checkpoint, ExpectedRecoveryPlanFingerprint: preview.SemanticPlanFingerprint,
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

func TestTaskBoardServicePreviewsAuthoringRecoveryFromVerifiedCheckpointWithoutMutation(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
	seedAuthoringRecoveryRepoPrepare(t, ctx, fixture)
	fixture.services.TaskBoard.actor = func() (string, error) { return "task-board-preview", nil }

	before := snapshotAuthoringRecoveryManagedFiles(t, fixture.root)
	preview, err := fixture.services.TaskBoard.PreviewRunRecovery(ctx, TaskBoardPreviewRunRecoveryRequest{
		TaskID: fixture.task.ID,
		RunID:  fixture.run.ID,
		Reason: "verify the authoring checkpoint before recovering it",
	})
	if err != nil {
		t.Fatalf("preview authoring recovery through task board: %v", err)
	}
	if preview.TaskID != fixture.task.ID || preview.RunID != fixture.run.ID ||
		preview.Strategy != TaskBoardRetryStrategyAuthoringRecovery ||
		preview.CurrentExecutionEpoch != fixture.run.ExecutionEpoch || preview.NextExecutionEpoch != fixture.run.ExecutionEpoch+1 ||
		preview.SubjectDigest == "" || preview.WorkflowFingerprint == "" || preview.Checkpoint.Sequence == 0 || preview.SemanticPlanFingerprint == "" {
		t.Fatalf("authoring recovery preview = %+v", preview)
	}
	if !containsTaskBoardPreviewStage(preview.TargetStages, workflowadapter.RepoAnalyze) ||
		!containsTaskBoardPreviewStage(preview.ScheduledStages, workflowadapter.RepoAnalyze) ||
		!containsTaskBoardPreviewStage(preview.ReusedStages, workflowadapter.RepoPrepare) {
		t.Fatalf("authoring recovery preview did not retain checkpoint boundary: %+v", preview)
	}
	if after := snapshotAuthoringRecoveryManagedFiles(t, fixture.root); !reflect.DeepEqual(after, before) {
		t.Fatal("task board recovery preview changed managed state")
	}

	other, err := fixture.store.CreateTaskV2(ctx, store.CreateTaskV2Request{
		Slug: "unrelated-recovery-preview", Title: "Unrelated recovery preview", Actor: "fixture", Reason: "exercise task ownership check",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.services.TaskBoard.PreviewRunRecovery(ctx, TaskBoardPreviewRunRecoveryRequest{
		TaskID: other.ID, RunID: fixture.run.ID, Reason: "must not preview another task's Run",
	}); err == nil {
		t.Fatal("cross-task recovery preview unexpectedly succeeded")
	}
}

func TestTaskBoardServiceRejectsStaleAuthoringRecoveryPreviewBeforePlanning(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
	seedAuthoringRecoveryRepoPrepare(t, ctx, fixture)
	preview, err := fixture.services.TaskBoard.PreviewRunRecovery(ctx, TaskBoardPreviewRunRecoveryRequest{
		TaskID: fixture.task.ID, RunID: fixture.run.ID, Reason: "inspect authoring recovery before confirmation",
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := fixture.services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.services.TaskBoard.RetryRun(ctx, TaskBoardRetryRunRequest{
		IdempotencyKey: key, TaskID: fixture.task.ID, RunID: fixture.run.ID, Reason: "must require preview binding",
	}); !errors.Is(err, ErrTaskBoardRecoveryPreviewRequired) {
		t.Fatalf("missing authoring recovery preview error = %v", err)
	}

	advanced := transitionAuthoringRecoveryRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunRunning)
	_ = transitionAuthoringRecoveryRun(t, ctx, fixture.store, advanced, store.WorkflowRunFailedRecoverable)
	_, err = fixture.services.TaskBoard.RetryRun(ctx, TaskBoardRetryRunRequest{
		IdempotencyKey: key, TaskID: fixture.task.ID, RunID: fixture.run.ID, Reason: "reject stale preview checkpoint",
		ExpectedRecoveryCheckpoint: &preview.Checkpoint, ExpectedRecoveryPlanFingerprint: preview.SemanticPlanFingerprint,
	})
	if !errors.Is(err, ErrTaskBoardRecoveryPreviewStale) {
		t.Fatalf("stale authoring recovery preview error = %v", err)
	}
	if command, lookupErr := fixture.store.GetContinuationCommandByKey(ctx, key); lookupErr != nil || command != nil {
		t.Fatalf("stale preview wrote recovery command = %+v, %v", command, lookupErr)
	}
}

func TestTaskBoardServiceRejectsAuthoringRecoveryWhenPreviewSemanticsChange(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
	seedAuthoringRecoveryRepoPrepare(t, ctx, fixture)
	preview, err := fixture.services.TaskBoard.PreviewRunRecovery(ctx, TaskBoardPreviewRunRecoveryRequest{
		TaskID: fixture.task.ID, RunID: fixture.run.ID, Reason: "inspect reusable source before confirmation",
	})
	if err != nil {
		t.Fatal(err)
	}
	reference := authoringRecoveryStageArtifactRef(t, ctx, fixture, workflowadapter.RepoPrepare)
	removeAuthoringRecoveryArtifactObject(t, ctx, fixture, reference)
	key, err := fixture.services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.services.TaskBoard.RetryRun(ctx, TaskBoardRetryRunRequest{
		IdempotencyKey: key, TaskID: fixture.task.ID, RunID: fixture.run.ID, Reason: "reject changed recovery semantics",
		ExpectedRecoveryCheckpoint: &preview.Checkpoint, ExpectedRecoveryPlanFingerprint: preview.SemanticPlanFingerprint,
	})
	if !errors.Is(err, ErrTaskBoardRecoveryPreviewStale) {
		t.Fatalf("changed authoring recovery semantics error = %v", err)
	}
	command, err := fixture.store.GetContinuationCommandByKey(ctx, key)
	if err != nil || command == nil {
		t.Fatalf("semantic-drift command = %+v, %v", command, err)
	}
	frozen, err := fixture.store.GetFrozenPlanByCommand(ctx, command.ID)
	if err != nil || frozen == nil {
		t.Fatalf("semantic-drift plan = %+v, %v", frozen, err)
	}
	plan, err := decodeFrozenContinuationPlan(ctx, fixture.services.core, *frozen)
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range plan.Snapshot().Nodes {
		if transition.NodeID != workflowkit.NodeID(workflowadapter.RepoPrepare) {
			continue
		}
		if transition.Disposition != workflowkit.DispositionSchedule {
			t.Fatalf("semantic-drift repo_prepare transition = %+v", transition)
		}
		return
	}
	t.Fatal("semantic-drift plan omitted repo_prepare")
}

func containsTaskBoardPreviewStage(stages []string, want string) bool {
	for _, stage := range stages {
		if stage == want {
			return true
		}
	}
	return false
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
		BaseImage:      taskBoardAuthoringTestBaseImage,
		Slug:           "task-board-activation-retry",
		Title:          "Task board activation retry",
		TaskType:       "feature",
		Application:    "backend",
		Objective:      "Retry a bounded task board authoring launch",
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

func TestTaskBoardAuthoringRequestChangesRunsRepairAndReturnsToReview(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryLaunchFixture(t)
	fixture.services.TaskBoard.actor = func() (string, error) { return "task-board-reviewer", nil }
	runtime := newFrozenRuntime(t, fixture.services, taskBoardAuthoringRuntimeRegistry(t, fixture.workflow))
	worker := newFrozenRuntimeWorker(t, fixture.store, runtime, "task-board-authoring-repair-worker")

	firstReview := driveTaskBoardAuthoringToReview(t, ctx, fixture.services, worker, fixture.task.ID, fixture.run.ID, "")
	decisionKey, err := fixture.services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.services.TaskBoard.DecideReview(ctx, TaskBoardDecideReviewRequest{
		IdempotencyKey: decisionKey, TaskID: fixture.task.ID, Review: firstReview,
		Decision: TaskBoardRequestChanges, Reason: "Correct the misspelled tower-http path.",
	}); err != nil {
		t.Fatal(err)
	}
	driveTaskBoardRunToStatus(t, ctx, worker, fixture.store, fixture.run.ID, store.WorkflowRunWaitingContinuation)

	waiting, err := fixture.services.TaskBoard.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waitingTask := taskBoardTaskByID(t, waiting, fixture.task.ID)
	if len(waitingTask.Runs) != 1 || !waitingTask.Runs[0].CanRetry || waitingTask.Runs[0].RetryStrategy != TaskBoardRetryStrategyAuthoringAdmissionRepair {
		t.Fatalf("waiting authoring repair projection = %+v", waitingTask.Runs)
	}
	retryKey, err := fixture.services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	preview, err := fixture.services.TaskBoard.PreviewRunRecovery(ctx, TaskBoardPreviewRunRecoveryRequest{
		TaskID: fixture.task.ID, RunID: fixture.run.ID, Reason: "inspect requested authoring changes",
	})
	if err != nil {
		t.Fatalf("preview authoring repair: %v", err)
	}
	if _, err := fixture.services.TaskBoard.RetryRun(ctx, TaskBoardRetryRunRequest{
		IdempotencyKey: retryKey, TaskID: fixture.task.ID, RunID: fixture.run.ID, Reason: "apply requested authoring changes",
		ExpectedRecoveryCheckpoint: &preview.Checkpoint, ExpectedRecoveryPlanFingerprint: preview.SemanticPlanFingerprint,
	}); err != nil {
		t.Fatal(err)
	}
	secondReview := driveTaskBoardAuthoringToReview(t, ctx, fixture.services, worker, fixture.task.ID, fixture.run.ID, firstReview.RequestID)
	if secondReview.RequestID == firstReview.RequestID {
		t.Fatalf("repair reused task review request %s", firstReview.RequestID)
	}

	attempts, err := fixture.store.ListStageAttemptsForRun(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var latestRepoAnalysis store.StageAttempt
	repoAnalysisAttempts := 0
	for _, attempt := range attempts {
		if attempt.StageKey != workflowadapter.RepoAnalyze {
			continue
		}
		repoAnalysisAttempts++
		latestRepoAnalysis = attempt
	}
	if repoAnalysisAttempts != 2 || latestRepoAnalysis.ExecutionStatus != store.StageExecutionCompleted {
		t.Fatalf("repo_analyze repair attempts = %d latest=%+v", repoAnalysisAttempts, latestRepoAnalysis)
	}
	references, err := fixture.store.ListArtifactRefs(ctx, latestRepoAnalysis.ArtifactManifestID)
	if err != nil || len(references) != 1 {
		t.Fatalf("repaired repo_analysis refs = %+v, %v", references, err)
	}
	var bindings []workflowkit.ArtifactBinding
	if err := decodeStrictJSON(references[0].InputBindingsJSON, &bindings); err != nil {
		t.Fatal(err)
	}
	foundFeedback := false
	for _, binding := range bindings {
		if binding.Name == "task_review_decision" && binding.SchemaVersion == "harbor.review-decision.v1" {
			foundFeedback = true
		}
	}
	if !foundFeedback {
		t.Fatalf("repaired repo_analysis lineage omitted task review feedback: %+v", bindings)
	}
}

func taskBoardAuthoringRuntimeRegistry(t *testing.T, workflow workflowkit.WorkflowDescriptor) *workflowkit.ControlledPluginRegistry[workflowkit.StageExecutor] {
	t.Helper()
	executor := workflowkit.StageExecutorFunc(func(ctx context.Context, request workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
		switch request.Stage.Key {
		case workflowkit.StageKey(workflowadapter.TaskReview), workflowkit.StageKey(workflowadapter.ContentReview), workflowkit.StageKey(workflowadapter.SolutionReview):
			binding, err := workflowkit.NewOpaqueExecutionBinding("task-board.authoring-review.wait", "1", []byte(`{"kind":"authoring-review"}`))
			if err != nil {
				return workflowkit.StageExecutionResult{}, err
			}
			return workflowkit.StageExecutionResult{Wait: &workflowkit.StageWait{
				Kind: workflowkit.StageWaitExternalDecision, OperationKey: "task-board-review:" + request.Execution.ID + ":" + string(request.Stage.Key), DecisionBinding: binding,
			}}, nil
		default:
			return completedFixtureStage(ctx, request)
		}
	})
	seen := make(map[workflowkit.PluginBinding]struct{})
	registrations := make([]workflowkit.PluginRegistration[workflowkit.StageExecutor], 0, len(workflow.Stages))
	for _, stage := range workflow.Stages {
		if _, present := seen[stage.Plugin]; present {
			continue
		}
		seen[stage.Plugin] = struct{}{}
		registrations = append(registrations, workflowkit.PluginRegistration[workflowkit.StageExecutor]{Binding: stage.Plugin, Implementation: executor})
	}
	registry, err := workflowkit.NewControlledPluginRegistry(registrations)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func driveTaskBoardAuthoringToReview(t *testing.T, ctx context.Context, services *LifecycleServices, worker *DurableWorker, taskID, runID, previousRequestID string) TaskBoardReview {
	t.Helper()
	for cycle := 0; cycle < 24; cycle++ {
		snapshot, err := services.TaskBoard.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		task := taskBoardTaskByID(t, snapshot, taskID)
		if task.Review != nil && task.Review.RequestID != "" && task.Review.RequestID != previousRequestID {
			return *task.Review
		}
		result, err := worker.RunOnce(ctx)
		if err != nil {
			t.Fatalf("drive authoring review cycle %d: %+v, %v", cycle, result, err)
		}
		if result.Empty {
			t.Fatalf("authoring worker became empty before Run %s reached review", runID)
		}
	}
	t.Fatalf("Run %s did not reach authoring review", runID)
	return TaskBoardReview{}
}

func driveTaskBoardRunToStatus(t *testing.T, ctx context.Context, worker *DurableWorker, dataStore *store.Store, runID string, status store.WorkflowRunStatus) {
	t.Helper()
	for cycle := 0; cycle < 12; cycle++ {
		run, err := dataStore.GetWorkflowRun(ctx, runID)
		if err != nil || run == nil {
			t.Fatalf("load authoring Run %s: %+v, %v", runID, run, err)
		}
		if run.Status == status {
			return
		}
		result, err := worker.RunOnce(ctx)
		if err != nil {
			t.Fatalf("drive authoring Run %s cycle %d: %+v, %v", runID, cycle, result, err)
		}
		if result.Empty {
			t.Fatalf("authoring worker became empty before Run %s reached %s", runID, status)
		}
	}
	t.Fatalf("Run %s did not reach %s", runID, status)
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

func TestTaskBoardProjectsDurableHandoffFailureRecord(t *testing.T) {
	finishedAt := time.Date(2026, time.July, 17, 10, 20, 0, 0, time.UTC)
	projected := (&TaskBoardService{}).projectTaskBoardRun(context.Background(), RunInspection{
		Run: store.WorkflowRun{
			ID:          "run-handoff",
			SubjectKind: store.WorkflowRunSubjectTaskRevision,
			Status:      store.WorkflowRunInDoubt,
		},
		Stages: []store.StageAttempt{{ID: "attempt-materialize", StageKey: workflowadapter.MaterializeTask}},
		Jobs: []DurableJobInspection{{Job: store.DurableJob{
			ID:             "job-handoff",
			CommandType:    standardAuthoringHandoffCommandType,
			EntityType:     "artifact_ref",
			EntityID:       "artifact-handoff",
			StageAttemptID: "attempt-materialize",
			State:          store.JobInDoubt,
			UpdatedAt:      finishedAt.Add(-time.Minute),
			FinishedAt:     &finishedAt,
			Failure: &store.DurableJobFailure{
				Code:        "handoff.definition_unavailable",
				Message:     "The approved child definition is unavailable.",
				DetailsJSON: `{"artifact_id":"artifact-handoff","stage":"materialize_task"}`,
			},
		}}},
	}, nil)

	if projected.FailureStage != string(workflowadapter.MaterializeTask) ||
		projected.FailureCode != "handoff.definition_unavailable" ||
		projected.FailureSummary != "The approved child definition is unavailable." ||
		projected.FailureJobID != "job-handoff" || projected.FailureArtifactID != "artifact-handoff" ||
		projected.FailureRecordedAt == nil || !projected.FailureRecordedAt.Equal(finishedAt) {
		t.Fatalf("durable handoff failure projection = %+v", projected)
	}
	if !projected.CanRedrive || projected.FailureRecoveryAction != TaskBoardFailureRecoveryRedriveAuthoringHandoff {
		t.Fatalf("recoverable handoff action = can_redrive:%t action:%q", projected.CanRedrive, projected.FailureRecoveryAction)
	}
}

func TestTaskBoardHidesResolvedOriginalHandoffFailure(t *testing.T) {
	runID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	stageAttemptID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	artifactID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	childRunID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	originalID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	recoveryID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(standardAuthoringHandoffPayload{
		Format: standardAuthoringHandoffPayloadFormat, AuthoringRunID: runID, StageAttemptID: stageAttemptID,
		HandoffArtifactID: artifactID, ChildRunID: childRunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	finishedAt := time.Date(2026, time.July, 17, 10, 20, 0, 0, time.UTC)
	original := store.DurableJob{
		ID: originalID, CommandType: standardAuthoringHandoffCommandType, EntityType: "artifact_ref", EntityID: artifactID,
		RunID: runID, StageAttemptID: stageAttemptID, PayloadJSON: string(payload), State: store.JobInDoubt, FinishedAt: &finishedAt,
		Failure: &store.DurableJobFailure{Code: handoffDefinitionUnavailableCode, Message: "The approved child definition is unavailable.", DetailsJSON: `{}`},
	}
	recovery := store.DurableJob{
		ID: recoveryID, CommandType: standardAuthoringHandoffRedriveCommandType, EntityType: "artifact_ref", EntityID: artifactID,
		RunID: runID, StageAttemptID: stageAttemptID, PayloadJSON: string(payload), State: store.JobSucceeded,
	}
	projected := (&TaskBoardService{}).projectTaskBoardRun(context.Background(), RunInspection{
		Run:  store.WorkflowRun{ID: runID, Status: store.WorkflowRunRunning},
		Jobs: []DurableJobInspection{{Job: original}, {Job: recovery}},
	}, nil)
	if projected.FailureCode != "" || projected.FailureRecoveryAction != TaskBoardFailureRecoveryNone || projected.CanRedrive {
		t.Fatalf("resolved handoff continued to advertise recovery: %+v", projected)
	}
}

func TestTaskBoardDoesNotOfferRedriveForFailedDurableJob(t *testing.T) {
	projected := (&TaskBoardService{}).projectTaskBoardRun(context.Background(), RunInspection{
		Run: store.WorkflowRun{ID: "run-failed", SubjectKind: store.WorkflowRunSubjectTaskRevision, Status: store.WorkflowRunFailedTerminal},
		Jobs: []DurableJobInspection{{Job: store.DurableJob{
			ID: "job-failed", CommandType: standardAuthoringHandoffCommandType, EntityType: "artifact_ref", EntityID: "artifact-failed",
			State: store.JobFailed, Failure: &store.DurableJobFailure{
				Code: "handoff.artifact_lineage_invalid", Message: "The handoff artifact lineage is invalid.", DetailsJSON: `{}`,
			},
		}}},
	}, nil)

	if projected.CanRedrive || projected.FailureRecoveryAction != TaskBoardFailureRecoveryRepairOrNewRun {
		t.Fatalf("terminal failed job exposed redrive = can_redrive:%t action:%q", projected.CanRedrive, projected.FailureRecoveryAction)
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

func TestTaskBoardLaunchesEvaluatorThroughControlledWorkerHandoff(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
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
