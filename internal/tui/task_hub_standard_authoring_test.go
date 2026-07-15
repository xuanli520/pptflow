package tui

import (
	"archive/tar"
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

type taskHubStandardAuthoringCapturer struct {
	snapshot    app.StandardAuthoringSourceSnapshot
	calls       int
	coordinates []app.StandardAuthoringSourceCoordinate
}

func (capturer *taskHubStandardAuthoringCapturer) CaptureStandardAuthoringSource(_ context.Context, coordinate app.StandardAuthoringSourceCoordinate) (app.StandardAuthoringSourceSnapshot, error) {
	capturer.calls++
	capturer.coordinates = append(capturer.coordinates, coordinate)
	content, err := taskHubStandardAuthoringArchive(coordinate.CommitSHA)
	if err != nil {
		return app.StandardAuthoringSourceSnapshot{}, err
	}
	result := capturer.snapshot
	result.RepositoryURL = coordinate.RepositoryURL
	result.CommitSHA = coordinate.CommitSHA
	result.Content = content
	return result, nil
}

func newTaskHubStandardAuthoringTestServices(t *testing.T) (*app.LifecycleServices, *taskHubStandardAuthoringCapturer) {
	t.Helper()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close Standard authoring test store: %v", err)
		}
	})
	capturer := &taskHubStandardAuthoringCapturer{snapshot: taskHubStandardAuthoringSnapshot(t)}
	definitions, catalog := newTaskHubStandardAuthoringDefinitionProvider(t)
	services, err := app.NewLifecycleServicesWithOptions(root, database, app.LifecycleServicesOptions{
		OperationResolver: testsupport.AcceptAllStageOperationResolver(),
		DeploymentCatalogResolvers: []app.TemplateDeploymentCatalogResolver{{
			Template: workflowadapter.StandardAuthoringTemplateReference(), Resolver: catalog,
		}},
		StandardAuthoringSourceCapturer:        capturer,
		StandardAuthoringRunDefinitionProvider: definitions,
	})
	if err != nil {
		t.Fatal(err)
	}
	return services, capturer
}

func TestAppTaskHubStandardAuthoringPlansLaunchesAndReplays(t *testing.T) {
	ctx := context.Background()
	services, capturer := newTaskHubStandardAuthoringTestServices(t)
	adapter := NewAppTaskHubLifecycleAdapter(services)

	snapshot, err := adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubTasksTab})
	if err != nil {
		t.Fatal(err)
	}
	standardAction, found := taskHubActionStateFor(snapshot.GlobalActions, TaskHubActionStartStandardAuthoring)
	if !found || !standardAction.Enabled || standardAction.DisabledReason != "" {
		t.Fatalf("Standard authoring global capability = %+v", standardAction)
	}
	plan, err := adapter.PlanTaskHubCommand(ctx, TaskHubCommand{Action: TaskHubActionStartStandardAuthoring})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.ConfirmationNeeded || plan.Expected != (TaskHubLifecycleCheckpoint{}) || !bytes.Contains([]byte(plan.Summary), []byte("来源仓库 URL")) || !bytes.Contains([]byte(plan.Summary), []byte("AuthoringSession")) || !bytes.Contains([]byte(plan.RevisionImpact), []byte("不会立即创建 TaskRevision")) {
		t.Fatalf("Standard authoring plan = %+v", plan)
	}
	if tasks, err := services.Tasks.List(ctx, true); err != nil || len(tasks) != 0 {
		t.Fatalf("Standard authoring plan created Task records: tasks=%+v err=%v", tasks, err)
	}
	if _, err := adapter.PlanTaskHubCommand(ctx, TaskHubCommand{Action: TaskHubActionStartStandardAuthoring, Target: TaskHubTarget{TaskID: "not-a-target"}}); err == nil {
		t.Fatal("Standard authoring plan accepted an existing Task target")
	}

	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	request := TaskHubMutationRequest{
		Action:         TaskHubActionStartStandardAuthoring,
		IdempotencyKey: key,
		Actor:          "tester",
		Reason:         "create a source-selected task through Task Hub",
		Values: map[string]string{
			taskHubStandardAuthoringRepositoryURLField: "https://github.com/acme/cors-origin-policy.git",
			taskHubStandardAuthoringCommitSHAField:     "0123456789abcdef0123456789abcdef01234567",
			taskHubTaskSlugField:                       "cors-origin-policy",
			taskHubTaskTitleField:                      "CORS origin policy",
			taskHubTaskMetadataJSONField:               `{"difficulty":"hard"}`,
		},
	}
	invalidTarget := request
	invalidTarget.Target = TaskHubTarget{TaskID: "not-a-target"}
	if _, err := adapter.ExecuteTaskHubMutation(ctx, invalidTarget); err == nil {
		t.Fatal("Standard authoring mutation accepted an existing Task target")
	}
	first, err := adapter.ExecuteTaskHubMutation(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.ReceiptID == "" || first.ExecutionID == "" || first.Target.TaskID == "" || first.Target.RunID != first.ExecutionID {
		t.Fatalf("Standard authoring Task Hub result = %+v", first)
	}
	if capturer.calls != 1 {
		t.Fatalf("source captures = %d, want one", capturer.calls)
	}
	if got, want := capturer.coordinates, []app.StandardAuthoringSourceCoordinate{{
		RepositoryURL: request.Values[taskHubStandardAuthoringRepositoryURLField],
		CommitSHA:     request.Values[taskHubStandardAuthoringCommitSHAField],
	}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("source capture coordinate = %+v, want %+v", got, want)
	}

	tasks, err := services.Tasks.List(ctx, true)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("launched Standard Task list = %+v, %v", tasks, err)
	}
	task := tasks[0]
	if task.Slug != request.Values[taskHubTaskSlugField] || task.CurrentRevisionID != "" || task.LifecycleState != store.TaskLifecycleDraft || task.SourceRepo != request.Values[taskHubStandardAuthoringRepositoryURLField] || task.SourceCommit != request.Values[taskHubStandardAuthoringCommitSHAField] {
		t.Fatalf("Standard authoring Task = %+v", task)
	}
	authoringRuns, err := services.Store().ListAuthoringWorkflowRunsForTargetTask(ctx, task.ID)
	if err != nil || len(authoringRuns) != 1 {
		t.Fatalf("Standard authoring Runs = %+v, %v", authoringRuns, err)
	}
	run := authoringRuns[0]
	if run.ID != first.ExecutionID || run.SubjectKind != store.WorkflowRunSubjectAuthoringSession || run.TaskID != "" || run.RevisionID != "" || run.AuthoringSessionID == "" {
		t.Fatalf("Standard authoring Run = %+v", run)
	}
	if genericRuns, err := services.Store().ListWorkflowRunsForTask(ctx, task.ID); err != nil || len(genericRuns) != 0 {
		t.Fatalf("Standard authoring launch created generic TaskRevision Runs: runs=%+v err=%v", genericRuns, err)
	}
	session, err := services.Store().GetAuthoringSession(ctx, run.AuthoringSessionID)
	if err != nil || session == nil || session.TargetTaskID != task.ID || session.SourceID == "" {
		t.Fatalf("Standard authoring session = %+v, %v", session, err)
	}
	source, err := services.Store().GetAuthoringSource(ctx, session.SourceID)
	if err != nil || source == nil || source.RepositoryURL != request.Values[taskHubStandardAuthoringRepositoryURLField] || source.CommitSHA != request.Values[taskHubStandardAuthoringCommitSHAField] {
		t.Fatalf("Standard authoring source = %+v, %v", source, err)
	}
	if jobs, err := services.Store().ListDurableJobsForRun(ctx, run.ID); err != nil || len(jobs) != 1 {
		t.Fatalf("Standard authoring durable jobs = %+v, %v", jobs, err)
	}
	operation, err := services.Store().GetLifecycleOperation(ctx, first.ReceiptID)
	if err != nil || operation == nil || operation.Action != "authoring.start" || operation.TaskID != task.ID || operation.RunID != run.ID || operation.IdempotencyKey != key {
		t.Fatalf("Standard authoring lifecycle operation = %+v, %v", operation, err)
	}

	replayed, err := adapter.ExecuteTaskHubMutation(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed != first || capturer.calls != 1 {
		t.Fatalf("Standard authoring replay = %+v; first=%+v captures=%d", replayed, first, capturer.calls)
	}
	if authoringRuns, err := services.Store().ListAuthoringWorkflowRunsForTargetTask(ctx, task.ID); err != nil || len(authoringRuns) != 1 {
		t.Fatalf("Standard authoring replay created duplicate Run: runs=%+v err=%v", authoringRuns, err)
	}
}

func TestTaskHubStandardAuthoringRetryRetainsOneFrozenLaunch(t *testing.T) {
	services, capturer := newTaskHubStandardAuthoringTestServices(t)
	ctx := context.Background()
	oldKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	old, err := services.AuthoringLaunches.Start(ctx, app.StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: app.LifecycleMutationCommandBase{IdempotencyKey: oldKey, Actor: "fixture", Reason: "seed an existing Standard authoring selection"},
		RepositoryURL:                "https://github.com/acme/existing-source.git",
		CommitSHA:                    "0123456789abcdef0123456789abcdef01234567",
		Slug:                         "existing-source",
		Title:                        "Existing source authoring",
		MetadataJSON:                 `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if old.TaskID == "" || old.RunID == "" || capturer.calls != 1 {
		t.Fatalf("seed Standard authoring launch = %+v, captures=%d", old, capturer.calls)
	}
	adapter := &lateReplyTaskHubAdapter{
		AppTaskHubLifecycleAdapter: NewAppTaskHubLifecycleAdapter(services),
		failFirstMutationReply:     true,
	}
	m, cleanup := newTestTaskHubV2Model(t, adapter)
	defer cleanup()
	if m.taskHub.SelectedTaskID != old.TaskID || m.taskHub.SelectedRunID != old.RunID {
		t.Fatalf("seed Standard authoring selection = task=%q run=%q, want task=%q run=%q", m.taskHub.SelectedTaskID, m.taskHub.SelectedRunID, old.TaskID, old.RunID)
	}
	m.taskHub.Query.Filter = "existing-source"
	m.hubSearch.SetValue("existing-source")

	updated, _ := m.Update(runeKey("t"))
	m = updated.(model)
	updated, planCommand := m.Update(runeKey("s"))
	m = updated.(model)
	if planCommand == nil {
		t.Fatal("t s did not request a Standard authoring plan")
	}
	updated, _ = m.Update(planCommand())
	m = updated.(model)
	if m.taskHubPlan == nil || m.taskHubPlanCommand == nil || m.taskHubPlanCommand.Target != (TaskHubTarget{}) {
		t.Fatalf("t s plan did not retain a global empty target: plan=%+v command=%+v", m.taskHubPlan, m.taskHubPlanCommand)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if m.taskHubMutation == nil || m.taskHubMutation.Action != TaskHubActionStartStandardAuthoring || m.taskHubMutation.isFrozen() {
		t.Fatalf("t s did not open an unfrozen one-step Standard authoring form: %+v", m.taskHubMutation)
	}
	form := m.taskHubMutation
	form.ReasonInput.SetValue("launch source-selected authoring through Task Hub")
	setTaskHubMutationFormValue(t, form, taskHubStandardAuthoringRepositoryURLField, "ssh://git@github.com/acme/retry-safe.git")
	setTaskHubMutationFormValue(t, form, taskHubStandardAuthoringCommitSHAField, "89abcdef0123456789abcdef0123456789abcdef")
	setTaskHubMutationFormValue(t, form, taskHubTaskSlugField, "source-retry-safe")
	setTaskHubMutationFormValue(t, form, taskHubTaskTitleField, "Source retry-safe authoring")
	setTaskHubMutationFormValue(t, form, taskHubTaskMetadataJSONField, `{"topic":"headers"}`)
	key := form.IdempotencyKey

	updated, firstCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if firstCommand == nil {
		t.Fatal("Standard authoring form did not issue a deferred mutation command")
	}
	updated, _ = m.Update(firstCommand())
	m = updated.(model)
	if m.taskHubMutation == nil || m.taskHubMutation.IdempotencyKey != key || m.taskHubMutation.Error == "" || capturer.calls != 2 {
		t.Fatalf("lost Standard authoring reply did not retain retryable form: form=%+v captures=%d", m.taskHubMutation, capturer.calls)
	}
	retained := m.taskHubMutation.request()
	if retained.Values[taskHubStandardAuthoringRepositoryURLField] != "ssh://git@github.com/acme/retry-safe.git" || retained.Values[taskHubStandardAuthoringCommitSHAField] != "89abcdef0123456789abcdef0123456789abcdef" {
		t.Fatalf("lost Standard authoring reply changed retained source coordinate: %+v", retained.Values)
	}

	updated, retryCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if retryCommand == nil {
		t.Fatal("Standard authoring retry did not issue a deferred mutation command")
	}
	updated, _ = m.Update(retryCommand())
	m = updated.(model)
	if m.taskHubMutation != nil {
		t.Fatalf("replayed Standard authoring mutation did not close the form: %+v", m.taskHubMutation)
	}
	if capturer.calls != 2 {
		t.Fatalf("Standard authoring retry recaptured source %d times", capturer.calls)
	}

	tasks, err := services.Tasks.List(context.Background(), true)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("Standard authoring retry Task list = %+v, %v", tasks, err)
	}
	var created store.TaskV2
	for _, task := range tasks {
		if task.Slug == "source-retry-safe" {
			created = task
			break
		}
	}
	if created.ID == "" {
		t.Fatalf("new Standard authoring Task was not found in %+v", tasks)
	}
	runs, err := services.Store().ListAuthoringWorkflowRunsForTargetTask(context.Background(), created.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("Standard authoring retry runs = %+v, %v", runs, err)
	}
	if m.taskHub.Query.Filter != "" || m.hubSearch.Value() != "" {
		t.Fatalf("successful Standard authoring launch retained a filter that hides its new Task: filter=%q search=%q", m.taskHub.Query.Filter, m.hubSearch.Value())
	}
	if m.taskHub.SelectedTaskID != created.ID || m.taskHub.SelectedRunID != runs[0].ID {
		t.Fatalf("successful Standard authoring launch did not replace the old selection: task=%q run=%q want task=%q run=%q", m.taskHub.SelectedTaskID, m.taskHub.SelectedRunID, created.ID, runs[0].ID)
	}
	updated, _ = m.Update(m.loadTaskHubV2()())
	m = updated.(model)
	if m.taskHub.SelectedTaskID != created.ID || m.taskHub.SelectedRunID != runs[0].ID {
		t.Fatalf("Standard authoring refresh did not preserve its new Task/Run selection: task=%q run=%q want task=%q run=%q", m.taskHub.SelectedTaskID, m.taskHub.SelectedRunID, created.ID, runs[0].ID)
	}
	if generic, err := services.Store().ListWorkflowRunsForTask(context.Background(), created.ID); err != nil || len(generic) != 0 {
		t.Fatalf("Standard authoring retry created generic Runs: %+v, %v", generic, err)
	}
}

func taskHubStandardAuthoringSnapshot(t *testing.T) app.StandardAuthoringSourceSnapshot {
	t.Helper()
	commit := "0123456789abcdef0123456789abcdef01234567"
	archive, err := taskHubStandardAuthoringArchive(commit)
	if err != nil {
		t.Fatal(err)
	}
	return app.StandardAuthoringSourceSnapshot{
		RepositoryURL: "https://github.com/acme/fixture-repository.git",
		CommitSHA:     commit,
		SchemaVersion: app.StandardAuthoringSourceSnapshotSchemaVersion,
		Content:       archive,
	}
}

func taskHubStandardAuthoringArchive(commit string) ([]byte, error) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{
		Name:       "pax_global_header",
		Typeflag:   tar.TypeXGlobalHeader,
		PAXRecords: map[string]string{"comment": commit},
	}); err != nil {
		return nil, err
	}
	if err := writer.WriteHeader(&tar.Header{Name: "source/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		return nil, err
	}
	for _, entry := range []struct {
		name    string
		content string
	}{
		{name: "source/README.md", content: "# source fixture\n"},
		{name: "source/src/lib.rs", content: "pub fn fixture() {}\n"},
	} {
		if err := writer.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o644, Size: int64(len(entry.content)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := writer.Write([]byte(entry.content)); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return archive.Bytes(), nil
}

func newTaskHubStandardAuthoringDefinitionProvider(t *testing.T) (app.StandardAuthoringRunDefinitionProvider, *stageprovider.DeploymentOperationCatalogResolver) {
	t.Helper()
	document := stageprovider.DeploymentOperationCatalog{
		Format: stageprovider.DeploymentOperationCatalogFormat, Version: stageprovider.DeploymentOperationCatalogVersion,
		CatalogID: "task-hub-standard-authoring-test", CatalogVersion: "1", Template: workflowadapter.StandardAuthoringTemplateReference(),
	}
	for _, stage := range workflowadapter.StandardAuthoringStageCatalog().Stages {
		operation := workflowadapter.StageOperationBinding{ProviderID: "task-hub-standard-authoring-test", OperationID: "test." + string(stage.Key), Version: "1"}
		switch stage.Key {
		case workflowkit.StageKey(workflowadapter.RepoPrepare):
			operation.Payload = workflowadapter.LocalCommandOperationPayload{CommandID: "test-source-capture", Arguments: []string{}}
		case workflowkit.StageKey(workflowadapter.TaskReview), workflowkit.StageKey(workflowadapter.ContentReview), workflowkit.StageKey(workflowadapter.SolutionReview):
			operation.Payload = workflowadapter.DurableReviewOperationPayload{PolicyID: "test-review"}
		case workflowkit.StageKey(workflowadapter.MaterializeTask):
			operation.Payload = workflowadapter.HarborBuiltinOperationPayload{HandlerID: "test-materialize"}
		default:
			operation.Payload = workflowadapter.AgentTurnOperationPayload{AgentID: "test-agent", ModelID: "test-model", MaxTurns: stage.RequiredTurns}
		}
		document.Operations = append(document.Operations, stageprovider.DeploymentOperationRegistration{
			Stage:     stageprovider.DeploymentStageContract{Key: stage.Key, Type: taskHubStandardAuthoringStageType(t, stage.Key), Group: stage.Group, Plugin: workflowkit.PluginBinding{ID: stage.Plugin.ID, Version: stage.Plugin.Version}},
			Provider:  workflowadapter.ProviderReference{ID: "task-hub-standard-authoring-test", Kind: "test", Version: "1"},
			Operation: operation,
			Runtime:   workflowadapter.RuntimeReference{ID: "task-hub-standard-authoring-runtime", Kind: "test", Version: "1"},
			Checkout:  stageprovider.DeploymentCheckoutContract{ID: "task-hub-standard-authoring-checkout", Purpose: "source"},
			Secrets:   []workflowadapter.SecretReference{},
		})
	}
	catalog, err := stageprovider.NewDeploymentOperationCatalogResolver(document)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := app.NewCatalogStandardAuthoringRunDefinitionProvider(catalog, taskHubStandardAuthoringProfile(t))
	if err != nil {
		t.Fatal(err)
	}
	return provider, catalog
}

func taskHubStandardAuthoringStageType(t *testing.T, key workflowkit.StageKey) workflowadapter.StageBindingType {
	t.Helper()
	switch key {
	case workflowkit.StageKey(workflowadapter.RepoPrepare):
		return workflowadapter.StageBindingRepoPrepare
	case workflowkit.StageKey(workflowadapter.RepoAnalyze):
		return workflowadapter.StageBindingRepoAnalyze
	case workflowkit.StageKey(workflowadapter.TaskDesign):
		return workflowadapter.StageBindingTaskDesign
	case workflowkit.StageKey(workflowadapter.TaskReview):
		return workflowadapter.StageBindingTaskReview
	case workflowkit.StageKey(workflowadapter.GenerateTaskFiles):
		return workflowadapter.StageBindingGenerateTaskFiles
	case workflowkit.StageKey(workflowadapter.InstructionGen):
		return workflowadapter.StageBindingInstructionGen
	case workflowkit.StageKey(workflowadapter.TaskTOMLGen):
		return workflowadapter.StageBindingTaskTOMLGen
	case workflowkit.StageKey(workflowadapter.DockerfileGen):
		return workflowadapter.StageBindingDockerfileGen
	case workflowkit.StageKey(workflowadapter.ContentReview):
		return workflowadapter.StageBindingContentReview
	case workflowkit.StageKey(workflowadapter.SolveGen):
		return workflowadapter.StageBindingSolveGen
	case workflowkit.StageKey(workflowadapter.TestGen):
		return workflowadapter.StageBindingTestGen
	case workflowkit.StageKey(workflowadapter.TestsAnalysis):
		return workflowadapter.StageBindingTestsAnalysis
	case workflowkit.StageKey(workflowadapter.SolutionReview):
		return workflowadapter.StageBindingSolutionReview
	case workflowkit.StageKey(workflowadapter.MaterializeTask):
		return workflowadapter.StageBindingMaterializeTask
	default:
		t.Fatalf("unsupported Standard authoring stage %q", key)
		return ""
	}
}

func taskHubStandardAuthoringProfile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	template := workflowadapter.StandardAuthoringWorkflowTemplate()
	profile := workflowadapter.ExecutionProfile{
		Template: template.Reference(), ID: "task-hub-standard-authoring-test", Version: "1",
		ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL,
		ControlGracePeriod:  30 * time.Second,
		CandidateProviderBudget: workflowadapter.CandidateProviderBudget{
			AttemptTimeout: time.Minute,
		},
	}
	for _, stage := range template.Catalog.Stages {
		turns := stage.RequiredTurns
		if turns < 1 {
			turns = 1
		}
		attempt := time.Duration(turns) * time.Second
		profile.Stages = append(profile.Stages, workflowadapter.StageBudget{
			StageKey: stage.Key,
			Budget: workflowkit.ExecutionBudget{
				TurnTimeout: time.Second, MaxTurns: turns, AttemptTimeout: attempt, MaxAttempts: 1, MaxElapsed: attempt,
			},
		})
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("build Standard authoring test profile: %v", err)
	}
	return profile
}
