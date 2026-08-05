package app

import (
	"context"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

// newAuthoringRunFixture launches one Standard authoring Run through the test
// composition and returns the composed services plus the durable Run.
type authoringRunFixture struct {
	services *LifecycleServices
	store    *store.Store
	run      store.WorkflowRun
	taskID   string
	sourceID string
}

func newAuthoringRunFixture(t *testing.T, ctx context.Context) authoringRunFixture {
	t.Helper()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	capturer := &standardAuthoringSourceCapturerFixture{
		coordinate: standardAuthoringLaunchTestCoordinate,
		snapshot:   standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate),
	}
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	services.TaskBoard.actor = func() (string, error) { return "authoring-recovery-test", nil }
	services.Continuations.observer = storeContinuationStateObserver{dataStore: database, objects: services.core.objects}
	key, err := services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	created, err := services.TaskBoard.StartAuthoring(ctx, TaskBoardStartAuthoringRequest{
		IdempotencyKey: key,
		RepositoryURL:  standardAuthoringLaunchTestCoordinate.RepositoryURL,
		CommitSHA:      standardAuthoringLaunchTestCoordinate.CommitSHA,
		BaseImage:      taskBoardAuthoringTestBaseImage,
		Slug:           "authoring-run-recovery",
		Title:          "Authoring run recovery",
		TaskType:       "feature",
		Application:    "backend", CodeLang: "rust",
		Objective:    "Recover and restart authoring Runs from frozen inputs",
		MetadataJSON: `{}`,
		Reason:       "exercise authoring run recovery",
	})
	if err != nil {
		t.Fatalf("start authoring run: %v", err)
	}
	run, err := database.GetWorkflowRun(ctx, created.RunID)
	if err != nil || run == nil {
		t.Fatalf("load authoring Run: %+v, %v", run, err)
	}
	if run.SubjectKind != store.WorkflowRunSubjectAuthoringSession || run.SubjectID == "" {
		t.Fatalf("launched Run is not an authoring-session Run: %+v", run)
	}
	return authoringRunFixture{services: services, store: database, run: *run, taskID: created.TaskID, sourceID: run.SubjectID}
}

func (fixture authoringRunFixture) recoverableRun(t *testing.T, ctx context.Context) store.WorkflowRun {
	t.Helper()
	return transitionContinuationRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunFailedRecoverable)
}

func TestTaskBoardRecoversStandardAuthoringRunFromFrozenCheckpoint(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRunFixture(t, ctx)
	run := fixture.recoverableRun(t, ctx)

	preview, err := fixture.services.TaskBoard.PreviewRunRecovery(ctx, TaskBoardPreviewRunRecoveryRequest{TaskID: fixture.taskID, RunID: run.ID, Reason: "preview authoring recovery"})
	if err != nil {
		t.Fatalf("preview authoring recovery: %v", err)
	}
	if preview.RunID != run.ID || preview.Strategy != TaskBoardRetryStrategyTaskContinuation || preview.CheckpointSequence != uint64(run.Version) ||
		preview.CurrentExecutionEpoch != run.ExecutionEpoch || preview.NextExecutionEpoch != run.ExecutionEpoch+1 ||
		preview.SubjectDigest != run.SubjectDigest || preview.SemanticPlanFingerprint == "" {
		t.Fatalf("authoring recovery preview = %+v", preview)
	}
	if len(preview.TargetStages) == 0 || len(preview.ScheduledStages) == 0 {
		t.Fatalf("authoring recovery preview did not schedule any stage: %+v", preview)
	}

	key, err := fixture.services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := preview.Checkpoint
	mutation, err := fixture.services.TaskBoard.RetryRun(ctx, TaskBoardRetryRunRequest{
		IdempotencyKey:                  key,
		TaskID:                          fixture.taskID,
		RunID:                           run.ID,
		Reason:                          "confirm authoring recovery",
		ExpectedRecoveryCheckpoint:      &checkpoint,
		ExpectedRecoveryPlanFingerprint: preview.SemanticPlanFingerprint,
	})
	if err != nil {
		t.Fatalf("confirm authoring recovery: %v", err)
	}
	if mutation.RunID != run.ID || mutation.TaskID != fixture.taskID {
		t.Fatalf("authoring recovery mutation = %+v", mutation)
	}
	recovered, err := fixture.store.GetWorkflowRun(ctx, run.ID)
	if err != nil || recovered == nil {
		t.Fatalf("load recovered Run: %v", err)
	}
	if recovered.Status != store.WorkflowRunRunning || recovered.ExecutionEpoch != run.ExecutionEpoch+1 || recovered.Version != run.Version+1 {
		t.Fatalf("recovered Run = status:%s epoch:%d version:%d, want running epoch %d", recovered.Status, recovered.ExecutionEpoch, recovered.Version, run.ExecutionEpoch+1)
	}
	active, err := fixture.store.HasActiveContinuationExecutionForRun(ctx, run.ID)
	if err != nil || !active {
		t.Fatalf("authoring continuation execution active = %t, %v", active, err)
	}
	jobs, err := fixture.store.ListDurableJobsForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundExecutionJob := false
	for _, job := range jobs {
		if job.State == store.JobQueued && job.CommandType == "workflow_run.execute" && strings.Contains(job.PayloadJSON, run.DefinitionHash) {
			foundExecutionJob = true
		}
	}
	if !foundExecutionJob {
		t.Fatalf("authoring recovery did not re-queue the frozen execution: %+v", jobs)
	}

	stale := preview
	stale.CheckpointSequence++
	if _, err := fixture.services.TaskBoard.RetryRun(ctx, TaskBoardRetryRunRequest{
		IdempotencyKey:                  key,
		RunID:                           run.ID,
		Reason:                          "stale authoring recovery",
		ExpectedRecoveryCheckpoint:      &stale.Checkpoint,
		ExpectedRecoveryPlanFingerprint: preview.SemanticPlanFingerprint,
	}); err == nil {
		t.Fatal("stale authoring recovery checkpoint was accepted")
	}
}

func TestTaskBoardRecoversInterruptedAuthoringRun(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRunFixture(t, ctx)
	run := transitionContinuationRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunRunning)
	interrupted, err := fixture.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunInterrupted, Actor: "tester", Reason: "interrupt fixture"})
	if err != nil {
		t.Fatalf("interrupt authoring Run: %v", err)
	}
	preview, err := fixture.services.TaskBoard.PreviewRunRecovery(ctx, TaskBoardPreviewRunRecoveryRequest{TaskID: fixture.taskID, RunID: interrupted.ID, Reason: "preview interrupted recovery"})
	if err != nil {
		t.Fatalf("preview interrupted authoring recovery: %v", err)
	}
	key, err := fixture.services.TaskBoard.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := preview.Checkpoint
	if _, err := fixture.services.TaskBoard.RetryRun(ctx, TaskBoardRetryRunRequest{
		IdempotencyKey:                  key,
		TaskID:                          fixture.taskID,
		RunID:                           interrupted.ID,
		Reason:                          "confirm interrupted recovery",
		ExpectedRecoveryCheckpoint:      &checkpoint,
		ExpectedRecoveryPlanFingerprint: preview.SemanticPlanFingerprint,
	}); err != nil {
		t.Fatalf("confirm interrupted authoring recovery: %v", err)
	}
	recovered, err := fixture.store.GetWorkflowRun(ctx, interrupted.ID)
	if err != nil || recovered == nil {
		t.Fatalf("load recovered Run: %v", err)
	}
	if recovered.Status != store.WorkflowRunRunning || recovered.ExecutionEpoch != interrupted.ExecutionEpoch+1 {
		t.Fatalf("interrupted Run recovered to status:%s epoch:%d", recovered.Status, recovered.ExecutionEpoch)
	}
}

func TestRestartAuthoringRunReusesFrozenSourceAndRecordsLineage(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRunFixture(t, ctx)
	oldRun := transitionContinuationRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunRunning)
	oldRun, err := fixture.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: oldRun.ID, ExpectedVersion: oldRun.Version, Status: store.WorkflowRunFailedTerminal, Actor: "tester", Reason: "terminal fixture"})
	if err != nil {
		t.Fatalf("terminate authoring Run: %v", err)
	}

	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := fixture.services.AuthoringLaunches.RestartAuthoringRun(ctx, RestartAuthoringRunCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "restart-test", Reason: "restart terminal authoring run"},
		SourceRunID:                  oldRun.ID,
	})
	if err != nil {
		t.Fatalf("restart authoring Run: %v", err)
	}
	if receipt.RunID == "" || receipt.RunID == oldRun.ID || receipt.AuthoringSessionID == oldRun.AuthoringSessionID || receipt.AuthoringSourceID != fixture.sourceID {
		t.Fatalf("restart receipt = %+v", receipt)
	}
	newRun, err := fixture.store.GetWorkflowRun(ctx, receipt.RunID)
	if err != nil || newRun == nil {
		t.Fatalf("load restarted Run: %v", err)
	}
	if newRun.SubjectKind != store.WorkflowRunSubjectAuthoringSession || newRun.SubjectID != oldRun.SubjectID ||
		newRun.SubjectDigest != oldRun.SubjectDigest || newRun.SubjectRevisionID == oldRun.SubjectRevisionID ||
		newRun.DefinitionHash != oldRun.DefinitionHash || newRun.Status != store.WorkflowRunQueued {
		t.Fatalf("restarted Run = %+v", newRun)
	}
	if newRun.AuthoringSessionID == oldRun.AuthoringSessionID {
		t.Fatalf("restarted Run must create a new authoring session")
	}
	manifest, err := decodeRunManifest(*newRun)
	if err != nil {
		t.Fatalf("decode restarted Run manifest: %v", err)
	}
	if manifest.RestartOfRunID != oldRun.ID || manifest.AuthoringSessionID != newRun.AuthoringSessionID || manifest.SubjectRevisionID != newRun.SubjectRevisionID {
		t.Fatalf("restarted Run manifest lineage = %+v", manifest)
	}
	session, err := fixture.store.GetAuthoringSession(ctx, newRun.AuthoringSessionID)
	if err != nil || session == nil {
		t.Fatalf("load restarted session: %v", err)
	}
	if session.ID == oldRun.AuthoringSessionID || session.SourceID != fixture.sourceID || session.TargetTaskID == fixture.taskID || session.WorkflowTemplateVersion != oldRun.WorkflowTemplateVersion {
		t.Fatalf("restarted session = %+v", session)
	}
	oldRunAfter, err := fixture.store.GetWorkflowRun(ctx, oldRun.ID)
	if err != nil || oldRunAfter == nil || oldRunAfter.Status != store.WorkflowRunFailedTerminal || oldRunAfter.Version != oldRun.Version {
		t.Fatalf("old Run mutated by restart: %+v, %v", oldRunAfter, err)
	}

	// Idempotent replay with the same key returns the same receipt and Run.
	replayed, err := fixture.services.AuthoringLaunches.RestartAuthoringRun(ctx, RestartAuthoringRunCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "restart-test", Reason: "restart terminal authoring run"},
		SourceRunID:                  oldRun.ID,
	})
	if err != nil {
		t.Fatalf("replay restart authoring Run: %v", err)
	}
	if replayed.RunID != receipt.RunID || replayed.AuthoringSessionID != receipt.AuthoringSessionID {
		t.Fatalf("restart replay = %+v, want %+v", replayed, receipt)
	}
	sessions, err := fixture.store.GetAuthoringSession(ctx, receipt.AuthoringSessionID)
	if err != nil || sessions == nil {
		t.Fatalf("restart replay lost the session: %v", err)
	}
}

func TestRestartAuthoringRunRejectsNonTerminalOrMaterializedState(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRunFixture(t, ctx)

	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	command := RestartAuthoringRunCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "restart-test", Reason: "restart running authoring run"},
		SourceRunID:                  fixture.run.ID,
	}
	if _, err := fixture.services.AuthoringLaunches.RestartAuthoringRun(ctx, command); err == nil || !strings.Contains(err.Error(), "only terminal Runs") {
		t.Fatalf("restart of a queued authoring Run = %v, want terminal rejection", err)
	}
	replayKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.services.AuthoringLaunches.RestartAuthoringRun(ctx, RestartAuthoringRunCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: replayKey, Actor: "restart-test", Reason: "restart a task-revision run"},
		SourceRunID:                  fixture.run.ID,
	}); err == nil || !strings.Contains(err.Error(), "only terminal Runs") {
		t.Fatalf("restart of a non-terminal authoring Run = %v, want terminal rejection", err)
	}
}
