package tui

import (
	"context"
	"encoding/json"
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
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func taskHubExecutionSpec(taskID, revisionID, revisionDigest string) workflowadapter.RunExecutionSpec {
	return testsupport.CompleteRunExecutionSpec(taskID, revisionID, revisionDigest)
}

func newTaskHubAdapterLifecycleServices(root string, dataStore *store.Store) (*app.LifecycleServices, error) {
	return app.NewLifecycleServicesWithOptions(root, dataStore, app.LifecycleServicesOptions{
		OperationResolver: testsupport.AcceptAllStageOperationResolver(),
	})
}

type taskHubEvaluatorDefinitionProvider struct {
	profile workflowadapter.ExecutionProfile
	spec    workflowadapter.RunExecutionSpec
	err     error
}

func (provider *taskHubEvaluatorDefinitionProvider) DefinitionForEvaluatorRun(_ context.Context, _ app.EvaluatorRunDefinitionRequest) (app.EvaluatorRunDefinition, error) {
	if provider == nil {
		return app.EvaluatorRunDefinition{}, app.ErrCodeEdgeEvaluatorDefinitionUnavailable
	}
	if provider.err != nil {
		return app.EvaluatorRunDefinition{}, provider.err
	}
	return app.EvaluatorRunDefinition{Profile: provider.profile.Clone(), ExecutionSpec: provider.spec.Clone()}, nil
}

func newTaskHubEvaluatorLifecycleServices(root string, dataStore *store.Store, provider app.EvaluatorRunDefinitionProvider) (*app.LifecycleServices, error) {
	return app.NewLifecycleServicesWithOptions(root, dataStore, app.LifecycleServicesOptions{
		OperationResolver:              testsupport.AcceptAllStageOperationResolver(),
		EvaluatorRunDefinitionProvider: provider,
	})
}

func TestAppTaskHubLifecycleAdapterQueriesRealServicesAndPlansWithoutMutation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := newTaskHubAdapterLifecycleServices(root, dataStore)
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
	if !found || !softDelete.Enabled || softDelete.DisabledReason != "" {
		t.Fatalf("soft-delete capability = %+v, want V12 confirmation-capable action", softDelete)
	}
	startRun, found := taskHubActionStateFor(projected.Actions, TaskHubActionStartRun)
	if !found || startRun.Enabled || !strings.Contains(startRun.DisabledReason, "当前 TaskRevision") {
		t.Fatalf("start-run capability = %+v, want current-revision requirement", startRun)
	}
	newTask, found := taskHubActionStateFor(snapshot.GlobalActions, TaskHubActionNewTask)
	if !found || !newTask.Enabled {
		t.Fatalf("global create capability = %+v, want V12 confirmation-capable action", newTask)
	}
	startKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	startRequest := TaskHubMutationRequest{
		Action: TaskHubActionStartRun, Target: TaskHubTarget{TaskID: task.ID}, IdempotencyKey: startKey, Actor: "tester", Reason: "attempt unavailable TUI start",
		Values: map[string]string{taskHubExecutionProfilePathField: filepath.Join(root, "profile.json"), taskHubRunTriggerField: "task_hub"},
	}
	if _, err := adapter.ExecuteTaskHubMutation(ctx, startRequest); err == nil || !strings.Contains(err.Error(), "Execution spec JSON") {
		t.Fatalf("profile-only StartRun submission error = %v", err)
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
	if !plan.ConfirmationNeeded || plan.Title != "软删除 Task" || plan.Expected.TaskID != task.ID || plan.Expected.TaskVersion != before.Version {
		t.Fatalf("soft-delete plan = %+v, want a V12 checkpointed confirmation preview", plan)
	}
	after, err := services.Tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.LifecycleState != before.LifecycleState || after.Version != before.Version || after.DeletedAt != before.DeletedAt {
		t.Fatalf("plan mutated Task: before=%+v after=%+v", before, after)
	}
}

func TestTaskHubRunStartTwoPhaseFlowFreezesInputsAndRetriesLostReply(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := newTaskHubAdapterLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{Slug: "tui-start-run", Title: "TUI Start Run", Actor: "tester", Reason: "Task Hub Run-start fixture"},
		SourceDirectory:        taskHubAdapterSnapshot(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	revision, err = services.Revisions.MarkValidated(ctx, revision.ID, revision.StateVersion, "sha256:tui-start-run-evidence", "tester", "validate StartRun fixture")
	if err != nil {
		t.Fatal(err)
	}
	review, err := services.Reviews.Request(ctx, revision.ID, revision.ValidationEvidenceManifest, "tester", "request StartRun fixture review")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Reviews.Decide(ctx, app.DecideReviewRequest{
		ReviewRequestID: review.ID, RevisionID: revision.ID, Action: store.ReviewDecisionApprove,
		ExpectedRevisionDigest: revision.TaskDigest, Actor: "tester", Reason: "approve StartRun fixture review",
	}); err != nil {
		t.Fatal(err)
	}
	task, err = services.Reviews.PromoteCurrent(ctx, task.ID, revision.ID, task.Version, "tester", "promote StartRun fixture revision")
	if err != nil {
		t.Fatal(err)
	}
	profilePath, specificationPath := writeTaskHubStartRunInputs(t, root, task.ID, revision.ID, revision.TaskDigest)
	adapter := &lateReplyTaskHubAdapter{
		AppTaskHubLifecycleAdapter: NewAppTaskHubLifecycleAdapter(services),
		failFirstMutationReply:     true,
	}
	m, cleanup := newTestTaskHubV2Model(t, adapter)
	defer cleanup()
	if m.taskHub.SelectedTaskID != task.ID {
		t.Fatalf("Task Hub selected task = %q, want %q", m.taskHub.SelectedTaskID, task.ID)
	}

	updated, _ := m.Update(runeKey("x"))
	m = updated.(model)
	updated, planCommand := m.Update(runeKey("n"))
	m = updated.(model)
	if planCommand == nil {
		t.Fatal("x n did not request a StartRun plan")
	}
	updated, _ = m.Update(planCommand())
	m = updated.(model)
	if m.taskHubPlan == nil || !m.taskHubPlan.ConfirmationNeeded || m.taskHubPlan.Expected.TaskID != task.ID || m.taskHubPlan.Expected.RevisionID != revision.ID {
		t.Fatalf("x n StartRun plan = %+v", m.taskHubPlan)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if m.taskHubMutation == nil || m.taskHubMutation.Action != TaskHubActionStartRun {
		t.Fatalf("x n StartRun confirmation form = %+v", m.taskHubMutation)
	}
	form := m.taskHubMutation
	form.ReasonInput.SetValue("freeze explicit profile and spec")
	setTaskHubMutationFormValue(t, form, taskHubExecutionProfilePathField, profilePath)
	setTaskHubMutationFormValue(t, form, taskHubExecutionSpecPathField, specificationPath)
	setTaskHubMutationFormValue(t, form, taskHubRunTriggerField, "task_hub")
	key := form.IdempotencyKey
	if err := store.ValidateUUIDv7(key); err != nil {
		t.Fatalf("TUI StartRun idempotency key %q: %v", key, err)
	}

	updated, prepareCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if prepareCommand == nil {
		t.Fatal("first StartRun confirmation did not prepare frozen inputs")
	}
	updated, _ = m.Update(prepareCommand())
	m = updated.(model)
	if m.taskHubMutation == nil || !m.taskHubMutation.isFrozen() || m.taskHubMutation.Preview.PlanID != "run-start:"+key ||
		m.taskHubMutation.FrozenReason != "freeze explicit profile and spec" {
		t.Fatalf("first StartRun confirmation did not retain frozen form: %+v", m.taskHubMutation)
	}
	for _, name := range []string{"execution-profile.json", "run-execution-spec.json", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(root, "run-inputs", key, name)); err != nil {
			t.Fatalf("prepared TUI StartRun input %s: %v", name, err)
		}
	}
	runs, err := services.Runs.ListForTask(ctx, task.ID)
	if err != nil || len(runs) != 0 {
		t.Fatalf("first confirmation created a Run before final confirmation: %+v, %v", runs, err)
	}
	if err := os.Remove(profilePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(specificationPath); err != nil {
		t.Fatal(err)
	}

	updated, firstExecute := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if firstExecute == nil {
		t.Fatal("final StartRun confirmation did not submit execution")
	}
	updated, _ = m.Update(firstExecute())
	m = updated.(model)
	if m.taskHubMutation == nil || m.taskHubMutation.IdempotencyKey != key || m.taskHubMutation.Error == "" || !m.taskHubMutation.isFrozen() {
		t.Fatalf("lost StartRun reply did not retain frozen retry form: %+v", m.taskHubMutation)
	}
	runs, err = services.Runs.ListForTask(ctx, task.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("lost StartRun reply did not durably create exactly one Run: %+v, %v", runs, err)
	}
	jobs, err := services.Store().ListDurableJobsForRun(ctx, runs[0].ID)
	if err != nil || len(jobs) != 1 || jobs[0].CommandType != "workflow_run.execute" {
		t.Fatalf("lost StartRun reply durable jobs = %+v, %v", jobs, err)
	}

	updated, retryExecute := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if retryExecute == nil {
		t.Fatal("lost StartRun reply retry did not submit execution")
	}
	updated, _ = m.Update(retryExecute())
	m = updated.(model)
	if m.taskHubMutation != nil || !m.taskHub.Loading {
		t.Fatalf("idempotent StartRun retry did not close form and request refresh: form=%+v loading=%t", m.taskHubMutation, m.taskHub.Loading)
	}
	runs, err = services.Runs.ListForTask(ctx, task.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("StartRun retry changed durable Run count: %+v, %v", runs, err)
	}
	jobs, err = services.Store().ListDurableJobsForRun(ctx, runs[0].ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("StartRun retry changed durable job count: %+v, %v", jobs, err)
	}
	updated, _ = m.Update(m.loadTaskHubV2()())
	m = updated.(model)
	if m.taskHub.Loading || len(m.taskHub.Snapshot.Runs) != 1 || m.taskHub.Snapshot.Runs[0].RunID != runs[0].ID {
		t.Fatalf("Task Hub refresh after StartRun = %+v", m.taskHub.Snapshot)
	}
}

func TestAppTaskHubAttachUsesLocalRuntimeValidLeaseProjection(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := newTaskHubAdapterLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{Slug: "attach-runtime", Actor: "tester", Reason: "Task Hub attach fixture"},
		SourceDirectory:        taskHubAdapterSnapshot(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: taskHubAdapterCompleteProfile(t), ExecutionSpec: taskHubExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "attach-runtime", Actor: "tester", Reason: "start attach fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "tester", Reason: "worker started attach fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := services.Store().ListDurableJobsForRun(ctx, run.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("initial attach fixture jobs = %+v, %v", jobs, err)
	}
	claim, err := services.Store().ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
		IdempotencyKey: "tui-attach-runtime-claim", Owner: "tui-attach-worker", LeaseTTL: time.Minute, Actor: "tester", Reason: "claim attach fixture",
	})
	if err != nil || claim.Job == nil || claim.DispatchLease == nil || claim.Job.ID != jobs[0].ID {
		t.Fatalf("claim attach fixture job = %+v, %v", claim, err)
	}
	handoffID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	handoffKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Store().ReserveRunWorkerHandoff(ctx, store.ReserveRunWorkerHandoffRequest{
		ID: handoffID, IdempotencyKey: handoffKey, RequestFingerprint: "sha256:tui-attach-handoff",
		RunID: run.ID, ExpectedRunVersion: run.Version, ExpectedRunExecutionEpoch: run.ExecutionEpoch,
		ExpectedRunDefinitionHash: run.DefinitionHash, Owner: "tui-attach-worker", Actor: "tester",
		Reason: "create Task Hub handoff projection fixture", LaunchTTL: time.Minute,
	}); err != nil {
		t.Fatalf("reserve Task Hub handoff fixture: %v", err)
	}

	adapter := NewAppTaskHubLifecycleAdapter(services)
	target := TaskHubTarget{TaskID: task.ID, RunID: run.ID, RevisionID: revision.ID}
	snapshot, err := adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubRunsTab})
	if err != nil || len(snapshot.Runs) != 1 {
		t.Fatalf("query attach fixture Task Hub = %+v, %v", snapshot, err)
	}
	attachState, found := taskHubActionStateFor(snapshot.Runs[0].Actions, TaskHubActionAttachRun)
	if !found || !attachState.Enabled {
		t.Fatalf("valid dispatch lease did not enable attach: %+v", snapshot.Runs[0].Actions)
	}
	if snapshot.Runs[0].WorkerHandoff == nil || snapshot.Runs[0].WorkerHandoff.OperationID != handoffID || snapshot.Runs[0].WorkerHandoff.State != string(store.RunWorkerHandoffLaunching) {
		t.Fatalf("Task Hub worker handoff projection = %+v", snapshot.Runs[0].WorkerHandoff)
	}
	preview, err := adapter.PlanTaskHubCommand(ctx, TaskHubCommand{Action: TaskHubActionAttachRun, Target: target})
	if err != nil || preview.ConfirmationNeeded || !strings.Contains(preview.Summary, "有效 lease") {
		t.Fatalf("valid attach preview = %+v, %v", preview, err)
	}

	if _, err := services.Store().AcquireLease(ctx, store.AcquireLeaseRequest{
		ResourceType: "fixture_auxiliary", ResourceID: "tui-attach-auxiliary-" + claim.Job.ID, Owner: "tui-attach-worker",
		JobID: claim.Job.ID, TTL: time.Minute, Actor: "tester", Reason: "keep unrelated fixture lease active",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := services.Store().HeartbeatLease(ctx, store.HeartbeatLeaseRequest{
		LeaseID: claim.DispatchLease.ID, Owner: claim.DispatchLease.Owner, FencingToken: claim.DispatchLease.FencingToken,
		ExpectedVersion: claim.DispatchLease.Version, TTL: 10 * time.Millisecond, Actor: "tester", Reason: "shorten dispatch lease for stale attach fixture",
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)

	snapshot, err = adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubRunsTab})
	if err != nil || len(snapshot.Runs) != 1 {
		t.Fatalf("query stale attach fixture Task Hub = %+v, %v", snapshot, err)
	}
	attachState, found = taskHubActionStateFor(snapshot.Runs[0].Actions, TaskHubActionAttachRun)
	if !found || attachState.Enabled || !strings.Contains(attachState.DisabledReason, "有效 durable lease") {
		t.Fatalf("stale dispatch lease remained attachable: %+v", snapshot.Runs[0].Actions)
	}
	preview, err = adapter.PlanTaskHubCommand(ctx, TaskHubCommand{Action: TaskHubActionAttachRun, Target: target})
	if err != nil || preview.ConfirmationNeeded || !strings.Contains(preview.Reason, "有效 durable lease") {
		t.Fatalf("stale attach preview accepted an unrelated lease: %+v, %v", preview, err)
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
	services, err := newTaskHubAdapterLifecycleServices(root, dataStore)
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

func TestTaskHubCodeEdgePackageRequiresApprovedRecordAndBindsSelectedRun(t *testing.T) {
	ctx, services, task, revision := newTaskHubLocalPackageMutationFixture(t)
	run := taskHubCreateCodeEdgePackageRun(t, ctx, services, task, revision)
	record := taskHubApprovedCodeEdgeComplianceRecord(t, run, task, revision)
	adapter := NewAppTaskHubLifecycleAdapter(services)

	alternate := run
	alternate.ID = mustTaskHubCodeEdgeUUID(t)
	alternate.CreatedAt = run.CreatedAt.Add(time.Second)
	alternateRecord := taskHubApprovedCodeEdgeComplianceRecord(t, alternate, task, revision)
	selected, unavailable := selectTaskHubCodeEdgePackageAuthorization(task, revision, []store.WorkflowRun{run, alternate}, map[string]*store.CodeEdgeComplianceRecord{run.ID: &record, alternate.ID: &alternateRecord}, "")
	if unavailable != "" || selected == nil || selected.Run.ID != alternate.ID || selected.Record.ID != alternateRecord.ID {
		t.Fatalf("approved CodeEdge package default selection = %+v / %q", selected, unavailable)
	}
	selected, unavailable = selectTaskHubCodeEdgePackageAuthorization(task, revision, []store.WorkflowRun{run, alternate}, map[string]*store.CodeEdgeComplianceRecord{run.ID: &record, alternate.ID: &alternateRecord}, run.ID)
	if unavailable != "" || selected == nil || selected.Run.ID != run.ID || selected.Record.ID != record.ID {
		t.Fatalf("preferred approved CodeEdge package selection = %+v / %q", selected, unavailable)
	}
	expected := TaskHubLifecycleCheckpoint{
		TaskID: task.ID, RevisionID: revision.ID, RevisionDigest: revision.TaskDigest,
		RunID: run.ID, RunVersion: run.Version, RunExecutionEpoch: run.ExecutionEpoch, RunDefinitionHash: run.DefinitionHash,
		CodeEdgeComplianceRecordID: record.ID, CodeEdgeAuthorizationFingerprint: record.AuthorizationFingerprint,
	}
	converted := appLifecycleMutationCheckpoint(expected)
	if converted.RunID != run.ID || converted.CodeEdgeComplianceRecordID != record.ID ||
		converted.CodeEdgeAuthorizationFingerprint != record.AuthorizationFingerprint {
		t.Fatalf("CodeEdge Task Hub checkpoint did not survive app conversion: %+v", converted)
	}

	// The TUI must carry the application-selected Run into its confirmation
	// target, rather than retaining a stale task-row selection after p p.
	plan := TaskHubPlanPreview{Title: "生成本地 package", Summary: "已选择 approved CodeEdge Run", ConfirmationNeeded: true, Expected: expected}
	fake := &fakeTaskHubLifecycle{snapshot: TaskHubSnapshot{
		Tasks: []TaskHubTask{{TaskID: task.ID, RevisionID: revision.ID, Actions: []TaskHubActionState{{Action: TaskHubActionPackageRevision, Enabled: true}}}},
		Runs:  []TaskHubRun{{RunID: run.ID, TaskID: task.ID, RevisionID: revision.ID}},
	}, plan: plan}
	m, cleanup := newTestTaskHubV2Model(t, fake)
	defer cleanup()
	updated, _ := m.Update(runeKey("p"))
	m = updated.(model)
	updated, planCommand := m.Update(runeKey("p"))
	m = updated.(model)
	if planCommand == nil {
		t.Fatal("p p did not request a CodeEdge package plan")
	}
	updated, _ = m.Update(planCommand())
	m = updated.(model)
	if m.taskHubPlan == nil || m.taskHubPlanCommand == nil || m.taskHubPlan.Expected.RunID != run.ID ||
		m.taskHubPlanCommand.Target.RunID != run.ID || m.taskHub.SelectedRunID != run.ID {
		t.Fatalf("p p did not bind its selected CodeEdge Run: plan=%+v command=%+v selected=%s", m.taskHubPlan, m.taskHubPlanCommand, m.taskHub.SelectedRunID)
	}

	// A caller cannot bypass the selected authorization by constructing a TUI
	// request with a task/revision checkpoint but no CodeEdge Run. The release
	// service must reject it before creating a local package.
	checkpoint, err := services.Mutations.CaptureCheckpoint(ctx, task.ID, revision.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	bypassKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.ExecuteTaskHubMutation(ctx, TaskHubMutationRequest{
		Action:         TaskHubActionPackageRevision,
		Target:         TaskHubTarget{TaskID: task.ID, RevisionID: revision.ID},
		Expected:       taskHubLifecycleCheckpoint(checkpoint),
		IdempotencyKey: bypassKey,
		Actor:          "tester",
		Reason:         "prove Task Hub cannot omit CodeEdge authorization",
		Values:         map[string]string{taskHubPackageVersionField: "codeedge-bypass-must-fail"},
	})
	if err == nil || !strings.Contains(err.Error(), "CodeEdge package requires an approved Run ID") {
		t.Fatalf("unbound CodeEdge TUI package request error = %v", err)
	}
	releases, err := services.Releases.List(ctx, task.ID)
	if err != nil || len(releases) != 0 {
		t.Fatalf("unbound CodeEdge TUI package request created releases = %+v, %v", releases, err)
	}

	detail, err := adapter.QueryTaskHubDetail(ctx, TaskHubDetailQuery{TaskID: task.ID, RunID: run.ID})
	if err != nil || len(detail.CodeEdgeCompliance) != 1 || detail.CodeEdgeCompliance[0].State != TaskHubCodeEdgeComplianceNotRecorded {
		t.Fatalf("CodeEdge unrecorded detail projection = %+v, %v", detail.CodeEdgeCompliance, err)
	}
	compliance := taskHubCodeEdgeComplianceFact(run, revision, &record)
	if compliance.State != TaskHubCodeEdgeComplianceApproved || compliance.ComplianceRecordID != record.ID || compliance.AuthorizationFingerprint != record.AuthorizationFingerprint {
		t.Fatalf("CodeEdge safe compliance fact = %+v", compliance)
	}
	overlay := newTaskHubDetailOverlay(TaskHubDetailQuery{TaskID: task.ID, RunID: run.ID})
	overlay.Loading = false
	overlay.Detail = TaskHubDetail{FrozenExecutions: []TaskHubFrozenExecutionFact{{RunID: run.ID, State: TaskHubFrozenExecutionUnavailable}}, CodeEdgeCompliance: []TaskHubCodeEdgeComplianceFact{compliance}}
	overlay.Tab = TaskHubDetailFrozenTab
	rendered := overlay.View(120, 60)
	for _, hidden := range []string{"qwen-receipt-secret", "submission-receipt-secret", "authorization-secret"} {
		if strings.Contains(rendered, hidden) {
			t.Fatalf("CodeEdge detail leaked raw immutable receipt content %q:\n%s", hidden, rendered)
		}
	}
	if !strings.Contains(rendered, "CodeEdge 最终合规") || !strings.Contains(rendered, record.AuthorizationFingerprint) {
		t.Fatalf("CodeEdge detail omitted safe compliance projection:\n%s", rendered)
	}
}

func TestSelectTaskHubCodeEdgePackageAuthorizationDoesNotFallbackFromCurrentPreferredRun(t *testing.T) {
	_, _, task, revision := newTaskHubLocalPackageMutationFixture(t)
	preferred := store.WorkflowRun{
		ID:                      mustTaskHubCodeEdgeUUID(t),
		TaskID:                  task.ID,
		RevisionID:              revision.ID,
		WorkflowTemplateID:      workflowadapter.CodeEdgePhase1WorkflowTemplateID,
		WorkflowTemplateVersion: workflowadapter.CodeEdgePhase1WorkflowTemplateVersion,
		CreatedAt:               time.Now().UTC(),
	}
	approved := preferred
	approved.ID = mustTaskHubCodeEdgeUUID(t)
	approved.CreatedAt = preferred.CreatedAt.Add(time.Second)
	approvedRecord := taskHubApprovedCodeEdgeComplianceRecord(t, approved, task, revision)
	runs := []store.WorkflowRun{preferred, approved}

	selected, unavailable := selectTaskHubCodeEdgePackageAuthorization(task, revision, runs, map[string]*store.CodeEdgeComplianceRecord{
		approved.ID: &approvedRecord,
	}, preferred.ID)
	if selected != nil || !strings.Contains(unavailable, "指定的 CodeEdge Phase-1 Run") || !strings.Contains(unavailable, "没有已批准") {
		t.Fatalf("missing preferred CodeEdge approval selected = %+v / %q, want explicit unavailability without fallback", selected, unavailable)
	}

	nonCodeEdge := preferred
	nonCodeEdge.ID = mustTaskHubCodeEdgeUUID(t)
	nonCodeEdge.WorkflowTemplateID = workflowadapter.StandardWorkflowTemplateID
	nonCodeEdge.WorkflowTemplateVersion = workflowadapter.StandardWorkflowTemplateVersion
	selected, unavailable = selectTaskHubCodeEdgePackageAuthorization(task, revision, append(runs, nonCodeEdge), map[string]*store.CodeEdgeComplianceRecord{
		approved.ID: &approvedRecord,
	}, nonCodeEdge.ID)
	if unavailable != "" || selected == nil || selected.Run.ID != approved.ID || selected.Record.ID != approvedRecord.ID {
		t.Fatalf("invalid preferred CodeEdge Run did not fall back to ordered approved candidate: %+v / %q", selected, unavailable)
	}
}

func TestTaskHubCodeEdgePackageIsUnavailableWithoutApprovedRecord(t *testing.T) {
	ctx, services, task, revision := newTaskHubLocalPackageMutationFixture(t)
	_ = taskHubCreateCodeEdgePackageRun(t, ctx, services, task, revision)
	adapter := NewAppTaskHubLifecycleAdapter(services)

	snapshot, err := adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubTasksTab})
	if err != nil || len(snapshot.Tasks) != 1 {
		t.Fatalf("query unapproved CodeEdge package fixture = %+v, %v", snapshot, err)
	}
	packageAction, found := taskHubActionStateFor(snapshot.Tasks[0].Actions, TaskHubActionPackageRevision)
	if !found || packageAction.Enabled || !strings.Contains(packageAction.DisabledReason, "没有已批准") {
		t.Fatalf("unapproved CodeEdge package action = %+v, want a clear disabled reason", packageAction)
	}
	plan, err := adapter.PlanTaskHubCommand(ctx, TaskHubCommand{
		Action: TaskHubActionPackageRevision,
		Target: TaskHubTarget{TaskID: task.ID, RevisionID: revision.ID},
	})
	if err != nil || plan.ConfirmationNeeded || !strings.Contains(plan.Reason, "没有已批准") {
		t.Fatalf("unapproved CodeEdge package plan = %+v, %v", plan, err)
	}
}

func taskHubCreateCodeEdgePackageRun(t *testing.T, ctx context.Context, services *app.LifecycleServices, task store.TaskV2, revision store.TaskRevision) store.WorkflowRun {
	t.Helper()
	profile := taskHubAdapterCompleteProfileForTemplate(t, workflowadapter.CodeEdgePhase1WorkflowTemplate())
	run, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID,
		Profile:       profile,
		ExecutionSpec: testsupport.CompleteCodeEdgePhase1RunExecutionSpec(task.ID, revision.ID, revision.TaskDigest),
		Trigger:       "task-hub-codeedge-package-fixture",
		Actor:         "tester",
		Reason:        "create CodeEdge package fixture",
	})
	if err != nil {
		t.Fatalf("start CodeEdge package fixture Run: %v", err)
	}
	return run
}

func TestAppTaskHubProjectsCodeEdgeEvaluatorEligibilityWithoutLeakingDefinitionErrors(t *testing.T) {
	ctx, services, provider, task, revision, parent := newTaskHubCodeEdgeEvaluatorFixture(t)
	launcher := &recordingTaskHubRunWorkerLauncher{}
	adapter := NewAppTaskHubLifecycleAdapterWithRunWorkerHandoffLauncher(services, launcher)

	snapshot, err := adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubRunsTab})
	if err != nil {
		t.Fatal(err)
	}
	parentProjection := taskHubRunProjectionByID(t, snapshot, parent.ID)
	action, found := taskHubActionStateFor(parentProjection.Actions, TaskHubActionEvaluateCodeEdge)
	if !found || action.Enabled || !strings.Contains(action.DisabledReason, "FinalReview") {
		t.Fatalf("unapproved evaluator action = %+v, want FinalReview-gated state", action)
	}

	parent = taskHubApproveCodeEdgeFinalReviewGate(t, ctx, services, parent, revision)
	snapshot, err = adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubRunsTab})
	if err != nil {
		t.Fatal(err)
	}
	parentProjection = taskHubRunProjectionByID(t, snapshot, parent.ID)
	action, found = taskHubActionStateFor(parentProjection.Actions, TaskHubActionEvaluateCodeEdge)
	if !found || !action.Enabled || action.DisabledReason != "" {
		t.Fatalf("approved evaluator action = %+v, want enabled", action)
	}

	provider.err = errors.New("definition provider rejected credential=unexposed-test-secret")
	snapshot, err = adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubRunsTab})
	if err != nil {
		t.Fatal(err)
	}
	parentProjection = taskHubRunProjectionByID(t, snapshot, parent.ID)
	action, found = taskHubActionStateFor(parentProjection.Actions, TaskHubActionEvaluateCodeEdge)
	if !found || action.Enabled || strings.Contains(action.DisabledReason, "unexposed-test-secret") || !strings.Contains(action.DisabledReason, "定义") {
		t.Fatalf("unsafe evaluator definition failure projection = %+v", action)
	}
	plan, err := adapter.PlanTaskHubCommand(ctx, TaskHubCommand{Action: TaskHubActionEvaluateCodeEdge, Target: TaskHubTarget{TaskID: task.ID, RevisionID: revision.ID, RunID: parent.ID}})
	if err != nil || plan.ConfirmationNeeded || strings.Contains(plan.Reason, "unexposed-test-secret") || !strings.Contains(plan.Reason, "定义") {
		t.Fatalf("unsafe evaluator plan projection = %+v, %v", plan, err)
	}
	expected, err := adapter.captureTaskHubMutationCheckpoint(ctx, services, task.ID, revision.ID, parent.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	_, prepareErr := adapter.PrepareTaskHubMutation(ctx, TaskHubMutationRequest{
		Action: TaskHubActionEvaluateCodeEdge, Target: TaskHubTarget{TaskID: task.ID, RevisionID: revision.ID, RunID: parent.ID},
		Expected: expected, Actor: "tester", Reason: "assert provider errors remain private", IdempotencyKey: mustTaskHubCodeEdgeUUID(t),
	})
	if prepareErr == nil || strings.Contains(prepareErr.Error(), "unexposed-test-secret") || !strings.Contains(prepareErr.Error(), "定义") {
		t.Fatalf("unsafe evaluator prepare error = %v", prepareErr)
	}
}

func newTaskHubCodeEdgeEvaluatorFixture(t *testing.T) (context.Context, *app.LifecycleServices, *taskHubEvaluatorDefinitionProvider, store.TaskV2, store.TaskRevision, store.WorkflowRun) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	provider := &taskHubEvaluatorDefinitionProvider{
		profile: taskHubAdapterCompleteProfileForTemplate(t, workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplate()),
	}
	services, err := newTaskHubEvaluatorLifecycleServices(root, database, provider)
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{Slug: "tui-codeedge-evaluator", Title: "TUI CodeEdge Evaluator", Actor: "tester", Reason: "create CodeEdge evaluator fixture"},
		SourceDirectory:        taskHubAdapterSnapshot(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	provider.spec = testsupport.CompleteCodeEdgeEvaluatorChildRunExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	parent := taskHubCreateCodeEdgePackageRun(t, ctx, services, task, revision)
	return ctx, services, provider, task, revision, parent
}

func taskHubApproveCodeEdgeFinalReviewGate(t *testing.T, ctx context.Context, services *app.LifecycleServices, parent store.WorkflowRun, revision store.TaskRevision) store.WorkflowRun {
	t.Helper()
	running, err := services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: parent.ID, ExpectedVersion: parent.Version, Status: store.WorkflowRunRunning,
		Actor: "tester", Reason: "open approved CodeEdge FinalReview fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, found := workflowadapter.CodeEdgePhase1StageCatalog().Stage(workflowkit.StageKey(workflowadapter.FinalReview))
	if !found {
		t.Fatal("CodeEdge Phase-1 catalog lacks FinalReview")
	}
	inputFingerprint := taskHubTestFingerprint('c')
	stage, err := services.Store().CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: running.ID, StageKey: workflowadapter.FinalReview, StageGroup: string(definition.Group), Ordinal: 1,
		InputFingerprint: inputFingerprint, BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "tester", Reason: "create approved CodeEdge FinalReview fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := services.Store().OpenReviewGate(ctx, store.OpenReviewGateRequest{
		RunID: running.ID, ExpectedRunVersion: running.Version, RevisionID: revision.ID, RevisionDigest: revision.TaskDigest, DefinitionHash: running.DefinitionHash,
		StageAttemptID: stage.ID, ExpectedStageAttemptVersion: stage.Version, StageKey: workflowadapter.FinalReview, ReviewKind: string(workflowadapter.ReviewFinalQuality),
		NodeGeneration: 0, NodeAttempt: 1, InputBindingsJSON: `[]`, InputFingerprint: inputFingerprint, EvidenceManifestDigest: "sha256:codeedge-final-review-fixture",
		Actor: "tester", Reason: "open approved CodeEdge FinalReview fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Store().RecordReviewGateDecision(ctx, store.RecordReviewGateDecisionRequest{
		ReviewRequestID: opened.Review.ID, RunID: opened.Run.ID, RevisionID: revision.ID, StageAttemptID: opened.StageAttempt.ID,
		ExpectedRevisionDigest: revision.TaskDigest, ExpectedRunVersion: opened.Run.Version, ExpectedStageAttemptVersion: opened.StageAttempt.Version,
		Action: store.ReviewDecisionApprove, ResolutionPayloadJSON: `{}`, Actor: "tester", Reason: "approve CodeEdge FinalReview fixture",
	}); err != nil {
		t.Fatal(err)
	}
	currentStage, err := services.Store().GetStageAttempt(ctx, opened.StageAttempt.ID)
	if err != nil || currentStage == nil {
		t.Fatalf("read approved CodeEdge FinalReview stage = %+v, %v", currentStage, err)
	}
	if _, err := services.Store().TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: currentStage.ID, ExpectedVersion: currentStage.Version, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictPass,
		Actor: "tester", Reason: "complete approved CodeEdge FinalReview fixture",
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := services.Runs.Get(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func taskHubRunProjectionByID(t *testing.T, snapshot TaskHubSnapshot, runID string) TaskHubRun {
	t.Helper()
	for _, run := range snapshot.Runs {
		if run.RunID == runID {
			return run
		}
	}
	t.Fatalf("Task Hub snapshot does not contain Run %s: %+v", runID, snapshot.Runs)
	return TaskHubRun{}
}

func taskHubApprovedCodeEdgeComplianceRecord(t *testing.T, run store.WorkflowRun, task store.TaskV2, revision store.TaskRevision) store.CodeEdgeComplianceRecord {
	t.Helper()
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	return store.CodeEdgeComplianceRecord{
		ID: key, RunID: run.ID, TaskID: task.ID, RevisionID: revision.ID, TaskDigest: revision.TaskDigest,
		Status:                   store.CodeEdgeComplianceApproved,
		QwenReceiptJSON:          `{"receipt":"qwen-receipt-secret"}`,
		OpusReceiptJSON:          `{"receipt":"opus-receipt-secret"}`,
		SubmissionReceiptJSON:    `{"receipt":"submission-receipt-secret"}`,
		DecisionJSON:             `{"decision":"approved"}`,
		DecisionFingerprint:      taskHubTestFingerprint('a'),
		AuthorizationJSON:        `{"authorization":"authorization-secret"}`,
		AuthorizationFingerprint: taskHubTestFingerprint('b'),
	}
}

func taskHubTestFingerprint(character rune) string {
	return "sha256:" + strings.Repeat(string(character), 64)
}

func mustTaskHubCodeEdgeUUID(t *testing.T) string {
	t.Helper()
	id, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newTaskHubAuthoringReviewFixture(t *testing.T) (context.Context, *app.LifecycleServices, store.TaskV2, store.AuthoringReviewGateOpenResult) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	services, err := newTaskHubAdapterLifecycleServices(root, database)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	source, err := database.CreateAuthoringSource(ctx, store.CreateAuthoringSourceRequest{
		RepositoryURL: "https://github.com/tower-rs/tower-http.git", CommitSHA: "f066e10ebc07ea9050a2ce4576315abfa568edf4",
		SnapshotArtifactRef: digest, SnapshotContentDigest: digest, SnapshotSchemaVersion: "harbor.source-snapshot.v1",
		IdempotencyKey: "tui-authoring-source-" + t.Name(), Actor: "author", Reason: "freeze TUI authoring source",
	})
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	task, err := database.CreateTaskV2(ctx, store.CreateTaskV2Request{
		Slug: "tui-authoring-review-" + strings.ToLower(strings.ReplaceAll(t.Name(), "_", "-")), Title: "TUI authoring review fixture",
		SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA, Actor: "author", Reason: "reserve TUI draft ownership",
	})
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	session, err := database.CreateAuthoringSession(ctx, store.CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID, WorkflowTemplateID: "harbor.standard-authoring", WorkflowTemplateVersion: "1.0.0",
		SessionManifestJSON: `{"mode":"standard"}`, IdempotencyKey: "tui-authoring-session-" + t.Name(), Actor: "author", Reason: "freeze TUI authoring session",
	})
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	run, err := database.CreateAuthoringWorkflowRun(ctx, store.CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID, WorkflowTemplateID: session.WorkflowTemplateID, WorkflowTemplateVersion: session.WorkflowTemplateVersion,
		ResolvedProfileHash: "sha256:tui-authoring-profile", DefinitionHash: "sha256:tui-authoring-definition",
		RunManifestJSON: `{}`, Trigger: "task.generate", Actor: "author", Reason: "start TUI authoring Run",
	})
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	run, err = database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "author", Reason: "run TUI authoring fixture",
	})
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	stage, err := database.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: "task_review", StageGroup: "authoring", Ordinal: 1,
		InputFingerprint: "sha256:tui-authoring-review-input", BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "author", Reason: "prepare TUI authoring review gate",
	})
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	opened, err := database.OpenAuthoringReviewGate(ctx, store.OpenAuthoringReviewGateRequest{
		IdempotencyKey: "open-tui-authoring-review-" + stage.ID, RunID: run.ID, AuthoringSessionID: session.ID, AuthoringSourceID: source.ID,
		SourceSnapshotDigest: source.SnapshotContentDigest, ExpectedRunVersion: run.Version, DefinitionHash: run.DefinitionHash,
		StageAttemptID: stage.ID, ExpectedStageAttemptVersion: stage.Version, StageKey: stage.StageKey, ReviewKind: "task_direction",
		NodeGeneration: 0, NodeAttemptOrdinal: 1, InputBindingsJSON: `{"ports":["repo_analysis"]}`,
		InputFingerprint: stage.InputFingerprint, EvidenceManifestDigest: "sha256:tui-authoring-review-evidence", Actor: "worker", Reason: "open TUI source/session review gate",
	})
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	return ctx, services, task, opened
}

func TestAppTaskHubLifecycleAdapterReviewDecisionUsesV12CheckpointAndReceipt(t *testing.T) {
	ctx, services, task, revision, review := newTaskHubReviewMutationFixture(t)
	adapter := NewAppTaskHubLifecycleAdapter(services)
	target := TaskHubTarget{
		TaskID:           task.ID,
		RevisionID:       revision.ID,
		ReviewRequestID:  review.ID,
		ReviewRevisionID: revision.ID,
	}
	plan, err := adapter.PlanTaskHubCommand(ctx, TaskHubCommand{Action: TaskHubActionApproveReview, Target: target})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.ConfirmationNeeded || plan.Expected.TaskID != task.ID || plan.Expected.RevisionID != revision.ID ||
		plan.Expected.ReviewRequestID != review.ID || plan.Expected.ReviewRevisionID != revision.ID ||
		plan.Expected.ReviewState != "open" || plan.Expected.ReviewEvidenceDigest != review.EvidenceManifestDigest {
		t.Fatalf("review plan did not capture complete V12 review checkpoint: %+v", plan)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	request := TaskHubMutationRequest{
		Action: TaskHubActionApproveReview, Target: target, Expected: plan.Expected,
		IdempotencyKey: key, Actor: "tester", Reason: "approve through Task Hub V12 command",
	}
	result, err := adapter.ExecuteTaskHubMutation(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.ReceiptID == "" || result.ReceiptID == review.ID {
		t.Fatalf("review result did not expose V12 receipt identity: %+v", result)
	}
	operation, err := services.Store().GetLifecycleOperation(ctx, result.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Action != string(app.LifecycleMutationReview) || operation.ReviewRequestID != review.ID ||
		operation.ExpectedReviewRevisionID != revision.ID || operation.ExpectedReviewState != "open" ||
		operation.ExpectedReviewEvidenceDigest != review.EvidenceManifestDigest || operation.IdempotencyKey != key {
		t.Fatalf("review was not persisted through the V12 lifecycle operation: %+v", operation)
	}
	replay, err := adapter.ExecuteTaskHubMutation(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replay.ReceiptID != result.ReceiptID {
		t.Fatalf("review V12 replay receipt = %+v, want %s", replay, result.ReceiptID)
	}
	decisions, err := services.Store().ListReviewDecisionsForRequest(ctx, review.ID)
	if err != nil || len(decisions) != 1 || decisions[0].ID != key {
		t.Fatalf("review decision replay did not retain one TUI idempotency key: %+v, %v", decisions, err)
	}
}

func TestAppTaskHubLifecycleAdapterAuthoringReviewUsesSourceSessionCheckpoint(t *testing.T) {
	ctx, services, task, opened := newTaskHubAuthoringReviewFixture(t)
	defer services.Store().Close()
	adapter := NewAppTaskHubLifecycleAdapter(services)

	snapshot, err := adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubTasksTab})
	if err != nil {
		t.Fatal(err)
	}
	var taskProjection *TaskHubTask
	for index := range snapshot.Tasks {
		if snapshot.Tasks[index].TaskID == task.ID {
			value := snapshot.Tasks[index]
			taskProjection = &value
			break
		}
	}
	if taskProjection == nil || taskProjection.ActiveAuthoringReviewID != opened.Request.ID ||
		taskProjection.ActiveAuthoringReviewRunID != opened.Run.ID {
		t.Fatalf("Task Hub did not expose the single open authoring review target: %+v", taskProjection)
	}
	if state, found := taskHubActionStateFor(taskProjection.Actions, TaskHubActionApproveReview); !found || !state.Enabled {
		t.Fatalf("source/session review action was not available on its draft Task: %+v", state)
	}
	var runProjection *TaskHubRun
	for index := range snapshot.Runs {
		if snapshot.Runs[index].RunID == opened.Run.ID {
			value := snapshot.Runs[index]
			runProjection = &value
			break
		}
	}
	if runProjection == nil || runProjection.TaskID != task.ID || runProjection.RevisionID != "" {
		t.Fatalf("Task Hub did not project AuthoringSession Run through its draft Task ownership: %+v", runProjection)
	}

	detail, err := adapter.QueryTaskHubDetail(ctx, TaskHubDetailQuery{RunID: opened.Run.ID})
	if err != nil {
		t.Fatalf("read authoring Run detail without a TaskRevision: %v", err)
	}
	if detail.Task.TaskID != task.ID || detail.SelectedRunID != opened.Run.ID || len(detail.AuthoringReviews) != 1 {
		t.Fatalf("authoring detail omitted its draft ownership or gate: %+v", detail)
	}
	projectedReview := detail.AuthoringReviews[0]
	if projectedReview.ReviewRequestID != opened.Request.ID || projectedReview.BindingID != opened.Binding.ID ||
		projectedReview.RunID != opened.Run.ID || projectedReview.AuthoringSessionID != opened.Request.AuthoringSessionID ||
		projectedReview.AuthoringSourceID != opened.Request.AuthoringSourceID || projectedReview.StageAttemptID != opened.StageAttempt.ID ||
		projectedReview.State != string(store.AuthoringReviewGateOpen) {
		t.Fatalf("authoring review detail projection lost immutable source/session facts: %+v", projectedReview)
	}

	target := TaskHubTarget{TaskID: task.ID, RunID: opened.Run.ID, AuthoringReviewRequestID: opened.Request.ID}
	plan, err := adapter.PlanTaskHubCommand(ctx, TaskHubCommand{Action: TaskHubActionApproveReview, Target: target})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.ConfirmationNeeded || plan.AuthoringReviewExpected.ReviewRequestID != opened.Request.ID ||
		plan.AuthoringReviewExpected.BindingID != opened.Binding.ID || plan.AuthoringReviewExpected.RunID != opened.Run.ID ||
		plan.AuthoringReviewExpected.StageAttemptID != opened.StageAttempt.ID || plan.AuthoringReviewExpected.AuthoringSessionID != opened.Request.AuthoringSessionID ||
		plan.Expected.RevisionID != "" || plan.Expected.ReviewRequestID != "" {
		t.Fatalf("authoring review plan did not preserve a source/session-only checkpoint: %+v", plan)
	}
	key := mustTaskHubCodeEdgeUUID(t)
	request := TaskHubMutationRequest{
		Action: TaskHubActionApproveReview, Target: target, AuthoringReviewExpected: plan.AuthoringReviewExpected,
		IdempotencyKey: key, Actor: "tester", Reason: "approve source/session gate through Task Hub",
	}
	result, err := adapter.ExecuteTaskHubMutation(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.ReceiptID == "" || result.ExecutionID == "" {
		t.Fatalf("authoring Task Hub decision did not return immutable decision/job identities: %+v", result)
	}
	replay, err := adapter.ExecuteTaskHubMutation(ctx, request)
	if err != nil {
		t.Fatalf("replay source/session review through Task Hub: %v", err)
	}
	if replay.ReceiptID != result.ReceiptID || replay.ExecutionID != result.ExecutionID {
		t.Fatalf("authoring Task Hub replay created different durable identities: first=%+v replay=%+v", result, replay)
	}
	if decisions, err := services.Store().ListAuthoringReviewDecisionsForRequest(ctx, opened.Request.ID); err != nil || len(decisions) != 1 || decisions[0].ID != result.ReceiptID || decisions[0].IdempotencyKey != key {
		t.Fatalf("authoring Task Hub decision was not persisted as one source/session fact: %+v err=%v", decisions, err)
	}
	if generic, err := services.Store().GetReviewRequest(ctx, opened.Request.ID); err != nil || generic != nil {
		t.Fatalf("authoring Task Hub decision wrote TaskRevision review state: request=%+v err=%v", generic, err)
	}
}

func TestAppTaskHubLifecycleAdapterSoftDeleteAndRestoreUseCapturedV12Checkpoint(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := newTaskHubAdapterLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, err := services.Tasks.CreateDraft(ctx, app.CreateDraftTaskRequest{
		Slug: "tui-v12-transition", Title: "TUI V12 transition", Actor: "fixture", Reason: "create transition fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAppTaskHubLifecycleAdapter(services)
	deletePlan, err := adapter.PlanTaskHubCommand(ctx, TaskHubCommand{Action: TaskHubActionSoftDeleteTask, Target: TaskHubTarget{TaskID: task.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if !deletePlan.ConfirmationNeeded || deletePlan.Expected.TaskVersion != task.Version {
		t.Fatalf("soft delete plan = %+v", deletePlan)
	}
	deleteKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := adapter.ExecuteTaskHubMutation(ctx, TaskHubMutationRequest{
		Action: TaskHubActionSoftDeleteTask, Target: TaskHubTarget{TaskID: task.ID}, Expected: deletePlan.Expected,
		IdempotencyKey: deleteKey, Actor: "fixture", Reason: "soft delete through Task Hub",
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, err := services.Store().GetLifecycleOperation(ctx, deleted.ReceiptID)
	if err != nil || operation == nil || operation.Action != string(app.LifecycleMutationSoftDelete) || operation.DeletionRecordID == "" {
		t.Fatalf("soft delete V12 receipt = %+v, err=%v", operation, err)
	}
	current, err := services.Tasks.Get(ctx, task.ID)
	if err != nil || current.LifecycleState != store.TaskLifecycleDeleted {
		t.Fatalf("soft delete state = %+v, err=%v", current, err)
	}

	restorePlan, err := adapter.PlanTaskHubCommand(ctx, TaskHubCommand{Action: TaskHubActionRestoreTask, Target: TaskHubTarget{TaskID: task.ID}})
	if err != nil {
		t.Fatal(err)
	}
	restoreKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := adapter.ExecuteTaskHubMutation(ctx, TaskHubMutationRequest{
		Action: TaskHubActionRestoreTask, Target: TaskHubTarget{TaskID: task.ID}, Expected: restorePlan.Expected,
		IdempotencyKey: restoreKey, Actor: "fixture", Reason: "restore through Task Hub",
		Values: map[string]string{taskHubRestoreStateField: string(store.TaskLifecycleDraft)},
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, err = services.Store().GetLifecycleOperation(ctx, restored.ReceiptID)
	if err != nil || operation == nil || operation.Action != string(app.LifecycleMutationRestore) || operation.TargetLifecycleState != store.TaskLifecycleDraft {
		t.Fatalf("restore V12 receipt = %+v, err=%v", operation, err)
	}
	current, err = services.Tasks.Get(ctx, task.ID)
	if err != nil || current.LifecycleState != store.TaskLifecycleDraft {
		t.Fatalf("restore state = %+v, err=%v", current, err)
	}
}

func TestAppTaskHubLifecycleAdapterCreateImportAndForkUseV12Commands(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := newTaskHubAdapterLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAppTaskHubLifecycleAdapter(services)

	createKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	created, err := adapter.ExecuteTaskHubMutation(ctx, TaskHubMutationRequest{
		Action: TaskHubActionNewTask, IdempotencyKey: createKey, Actor: "fixture", Reason: "create through Task Hub",
		Values: map[string]string{taskHubTaskSlugField: "tui-v12-create", taskHubTaskTitleField: "TUI V12 Create", taskHubTaskMetadataJSONField: `{"source":"tui"}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	createOperation, err := services.Store().GetLifecycleOperation(ctx, created.ReceiptID)
	if err != nil || createOperation == nil || createOperation.Action != string(app.LifecycleMutationCreateDraft) {
		t.Fatalf("create V12 operation = %+v, err=%v", createOperation, err)
	}
	if created.Target.TaskID != "" {
		t.Fatalf("global create result retained an unrelated selected target: %+v", created.Target)
	}
	createdTask, err := services.Tasks.Get(ctx, createOperation.TaskID)
	if err != nil || createdTask.Slug != "tui-v12-create" {
		t.Fatalf("created Task = %+v, err=%v", createdTask, err)
	}

	importKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	imported, err := adapter.ExecuteTaskHubMutation(ctx, TaskHubMutationRequest{
		Action: TaskHubActionImportTask, IdempotencyKey: importKey, Actor: "fixture", Reason: "import through Task Hub",
		Values: map[string]string{
			taskHubTaskSlugField: "tui-v12-import", taskHubTaskTitleField: "TUI V12 Import", taskHubImportSourcePathField: taskHubAdapterSnapshot(t),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	importOperation, err := services.Store().GetLifecycleOperation(ctx, imported.ReceiptID)
	if err != nil || importOperation == nil || importOperation.Action != string(app.LifecycleMutationImport) {
		t.Fatalf("import V12 operation = %+v, err=%v", importOperation, err)
	}
	importedTask, err := services.Tasks.Get(ctx, importOperation.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	forkPlan, err := adapter.PlanTaskHubCommand(ctx, TaskHubCommand{
		Action: TaskHubActionForkTask, Target: TaskHubTarget{TaskID: importedTask.ID, RevisionID: importOperation.RevisionID},
	})
	if err != nil || !forkPlan.ConfirmationNeeded || forkPlan.Expected.RevisionID != importOperation.RevisionID {
		t.Fatalf("fork plan = %+v, err=%v", forkPlan, err)
	}
	forkKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	forked, err := adapter.ExecuteTaskHubMutation(ctx, TaskHubMutationRequest{
		Action: TaskHubActionForkTask, Target: TaskHubTarget{TaskID: importedTask.ID, RevisionID: importOperation.RevisionID}, Expected: forkPlan.Expected,
		IdempotencyKey: forkKey, Actor: "fixture", Reason: "fork through Task Hub",
		Values: map[string]string{taskHubTaskSlugField: "tui-v12-fork", taskHubTaskTitleField: "TUI V12 Fork"},
	})
	if err != nil {
		t.Fatal(err)
	}
	forkOperation, err := services.Store().GetLifecycleOperation(ctx, forked.ReceiptID)
	if err != nil || forkOperation == nil || forkOperation.Action != string(app.LifecycleMutationFork) || forkOperation.TaskID == importedTask.ID {
		t.Fatalf("fork V12 operation = %+v, err=%v", forkOperation, err)
	}
}

func TestAppTaskHubLifecycleAdapterPackageAndWithdrawUseV12Commands(t *testing.T) {
	ctx, services, task, revision := newTaskHubLocalPackageMutationFixture(t)
	adapter := NewAppTaskHubLifecycleAdapter(services)
	packagePlan, err := adapter.PlanTaskHubCommand(ctx, TaskHubCommand{
		Action: TaskHubActionPackageRevision, Target: TaskHubTarget{TaskID: task.ID, RevisionID: revision.ID},
	})
	if err != nil || !packagePlan.ConfirmationNeeded {
		t.Fatalf("package plan = %+v, err=%v", packagePlan, err)
	}
	packageKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	packaged, err := adapter.ExecuteTaskHubMutation(ctx, TaskHubMutationRequest{
		Action: TaskHubActionPackageRevision, Target: TaskHubTarget{TaskID: task.ID, RevisionID: revision.ID}, Expected: packagePlan.Expected,
		IdempotencyKey: packageKey, Actor: "fixture", Reason: "package through Task Hub", Values: map[string]string{taskHubPackageVersionField: "tui-v12-withdraw-v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	packageOperation, err := services.Store().GetLifecycleOperation(ctx, packaged.ReceiptID)
	if err != nil || packageOperation == nil || packageOperation.Action != string(app.LifecycleMutationPackage) {
		t.Fatalf("package V12 operation = %+v, err=%v", packageOperation, err)
	}
	release, err := services.Store().GetLocalPackageRelease(ctx, packageOperation.ReleaseID)
	if err != nil || release == nil || release.WithdrawnAt != nil {
		t.Fatalf("packaged release = %+v, err=%v", release, err)
	}
	published, err := services.Tasks.Get(ctx, task.ID)
	if err != nil || published.LifecycleState != store.TaskLifecyclePublished {
		t.Fatalf("local package did not publish reviewed Task: %+v, err=%v", published, err)
	}
	archivePlan, err := adapter.PlanTaskHubCommand(ctx, TaskHubCommand{Action: TaskHubActionArchiveTask, Target: TaskHubTarget{TaskID: task.ID}})
	if err != nil || !archivePlan.ConfirmationNeeded || archivePlan.Expected.TaskVersion != published.Version {
		t.Fatalf("archive plan = %+v, err=%v", archivePlan, err)
	}
	archiveKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	archived, err := adapter.ExecuteTaskHubMutation(ctx, TaskHubMutationRequest{
		Action: TaskHubActionArchiveTask, Target: TaskHubTarget{TaskID: task.ID}, Expected: archivePlan.Expected,
		IdempotencyKey: archiveKey, Actor: "fixture", Reason: "archive through Task Hub",
	})
	if err != nil {
		t.Fatal(err)
	}
	archiveOperation, err := services.Store().GetLifecycleOperation(ctx, archived.ReceiptID)
	if err != nil || archiveOperation == nil || archiveOperation.Action != string(app.LifecycleMutationArchive) {
		t.Fatalf("archive V12 operation = %+v, err=%v", archiveOperation, err)
	}
	withdrawPlan, err := adapter.PlanTaskHubCommand(ctx, TaskHubCommand{
		Action: TaskHubActionWithdrawRelease, Target: TaskHubTarget{TaskID: task.ID, RevisionID: revision.ID, ReleaseID: release.ID},
	})
	if err != nil || !withdrawPlan.ConfirmationNeeded || withdrawPlan.Expected.ReleaseID != release.ID || withdrawPlan.Expected.ReleaseRecordVersion != release.RecordVersion {
		t.Fatalf("withdraw plan = %+v, err=%v", withdrawPlan, err)
	}
	withdrawKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	withdrawn, err := adapter.ExecuteTaskHubMutation(ctx, TaskHubMutationRequest{
		Action: TaskHubActionWithdrawRelease, Target: TaskHubTarget{TaskID: task.ID, RevisionID: revision.ID, ReleaseID: release.ID}, Expected: withdrawPlan.Expected,
		IdempotencyKey: withdrawKey, Actor: "fixture", Reason: "withdraw through Task Hub",
	})
	if err != nil {
		t.Fatal(err)
	}
	withdrawOperation, err := services.Store().GetLifecycleOperation(ctx, withdrawn.ReceiptID)
	if err != nil || withdrawOperation == nil || withdrawOperation.Action != string(app.LifecycleMutationWithdraw) || withdrawOperation.ReleaseID != release.ID {
		t.Fatalf("withdraw V12 operation = %+v, err=%v", withdrawOperation, err)
	}
	updatedRelease, err := services.Store().GetLocalPackageRelease(ctx, release.ID)
	if err != nil || updatedRelease == nil || updatedRelease.WithdrawnAt == nil {
		t.Fatalf("withdrawn release = %+v, err=%v", updatedRelease, err)
	}
}

func TestAppTaskHubLifecycleAdapterManualPatchUsesV12PrepareAndFrozenPlan(t *testing.T) {
	ctx, services, task, revision := newTaskHubLocalPackageMutationFixture(t)
	run, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: taskHubAdapterCandidateLeaseProfile(t), ExecutionSpec: taskHubExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "task_hub_edit",
		Actor: "fixture", Reason: "start manual patch fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAppTaskHubLifecycleAdapter(services)
	target := TaskHubTarget{TaskID: task.ID, RevisionID: revision.ID, RunID: run.ID}
	plan, err := adapter.PlanTaskHubCommand(ctx, TaskHubCommand{Action: TaskHubActionEditTask, Target: target})
	if err != nil || !plan.ConfirmationNeeded || plan.Expected.RunID != run.ID || plan.Expected.RevisionID != revision.ID {
		t.Fatalf("manual patch preview = %+v, err=%v", plan, err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	request := TaskHubMutationRequest{
		Action: TaskHubActionEditTask, Target: target, Expected: plan.Expected,
		IdempotencyKey: key, Actor: "fixture", Reason: "patch through Task Hub",
		Values: map[string]string{taskHubUnifiedDiffField: "--- a/instruction.md\n+++ b/instruction.md\n@@ -1 +1 @@\n-Solve the task.\n+Solve the patched task."},
	}
	prepared, err := adapter.PrepareTaskHubMutation(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Preview.PlanID == "" || prepared.Preview.Expected != plan.Expected || prepared.Actor != request.Actor || prepared.Reason != request.Reason {
		t.Fatalf("manual patch preparation did not freeze a V12-bound plan: %+v", prepared)
	}
	operation, err := services.Store().GetLifecycleOperationByIdempotencyKey(ctx, key)
	if err != nil || operation == nil || operation.Action != string(app.LifecycleMutationEdit) || operation.RunID != run.ID || operation.RevisionID != revision.ID {
		t.Fatalf("manual patch V12 operation = %+v, err=%v", operation, err)
	}
	request.PlanID = prepared.Preview.PlanID
	completed, err := adapter.ExecuteTaskHubMutation(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if completed.ReceiptID != operation.ID || completed.PlanID != prepared.Preview.PlanID || completed.ExecutionID == "" {
		t.Fatalf("manual patch execution result = %+v, operation=%+v", completed, operation)
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
	services, err := newTaskHubAdapterLifecycleServices(root, dataStore)
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
		TaskID: task.ID, RevisionID: revision.ID, Profile: taskHubAdapterCompleteProfile(t), ExecutionSpec: taskHubExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "integration",
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
	services, err := newTaskHubAdapterLifecycleServices(root, dataStore)
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
		TaskID: task.ID, RevisionID: revision.ID, Profile: taskHubAdapterCompleteProfile(t), ExecutionSpec: taskHubExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "integration",
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

func TestAppTaskHubLocalReconcileUsesScopedCLIRecoveryThroughConfirmedTUI(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := newTaskHubAdapterLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{
			Slug: "tui-local-reconcile", Title: "TUI local reconcile", Actor: "tester", Reason: "create Task Hub reconcile fixture",
		},
		SourceDirectory: taskHubAdapterSnapshot(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	start := func(trigger string) store.WorkflowRun {
		t.Helper()
		run, startErr := services.Runs.StartRun(ctx, app.StartRunRequest{
			TaskID: task.ID, RevisionID: revision.ID, Profile: taskHubAdapterCompleteProfile(t),
			ExecutionSpec: taskHubExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: trigger,
			Actor: "tester", Reason: "start Task Hub local reconcile fixture",
		})
		if startErr != nil {
			t.Fatal(startErr)
		}
		return run
	}
	claim := func(runID string) store.DurableJobDispatchClaim {
		t.Helper()
		jobs, listErr := services.Store().ListDurableJobsForRun(ctx, runID)
		if listErr != nil || len(jobs) != 1 {
			t.Fatalf("list initial durable jobs for Run %s = %+v, %v", runID, jobs, listErr)
		}
		key, keyErr := store.NewUUIDv7()
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		claimed, claimErr := services.Store().ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
			IdempotencyKey: key, Owner: "tui-local-reconcile-worker", LeaseTTL: 10 * time.Millisecond,
			Actor: "tester", Reason: "claim local reconcile fixture",
		})
		if claimErr != nil || claimed.Job == nil || claimed.DispatchLease == nil || claimed.Job.ID != jobs[0].ID || claimed.Job.RunID != runID {
			t.Fatalf("claim durable job for Run %s = %+v, %v", runID, claimed, claimErr)
		}
		return claimed
	}

	selectedRun := start("selected-local-reconcile")
	selectedClaim := claim(selectedRun.ID)
	otherRun := start("other-local-reconcile")
	otherClaim := claim(otherRun.ID)
	selectedRun, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: selectedRun.ID, ExpectedVersion: selectedRun.Version, Status: store.WorkflowRunInDoubt,
		Actor: "tester", Reason: "make selected Run reconcile-eligible",
	})
	if err != nil {
		t.Fatal(err)
	}

	effectKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	effect, err := services.Store().CreateSideEffectOperation(ctx, store.CreateSideEffectOperationRequest{
		OperationKey: "tui-local-reconcile-external-effect-" + selectedRun.ID, IdempotencyKey: effectKey, RunID: selectedRun.ID,
		EffectKind: "external_provider_fixture", SourceDigest: "sha256:tui-local-reconcile", PayloadJSON: `{}`,
		Actor: "tester", Reason: "record unknown external effect that local reconcile must not infer",
	})
	if err != nil {
		t.Fatal(err)
	}
	effect, err = services.Store().TransitionSideEffectOperation(ctx, store.TransitionSideEffectOperationRequest{
		OperationID: effect.ID, ExpectedVersion: effect.Version, State: store.SideEffectStarted,
		Actor: "tester", Reason: "begin external fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	effect, err = services.Store().TransitionSideEffectOperation(ctx, store.TransitionSideEffectOperationRequest{
		OperationID: effect.ID, ExpectedVersion: effect.Version, State: store.SideEffectUnknown,
		Actor: "tester", Reason: "leave external outcome unknown",
	})
	if err != nil {
		t.Fatal(err)
	}
	effectVersion := effect.Version

	// Store time is intentionally opaque to the adapter. Let both claimed
	// leases expire before the selected, confirmed local recovery occurs.
	time.Sleep(40 * time.Millisecond)
	adapter := NewAppTaskHubLifecycleAdapter(services)
	snapshot, err := adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubRunsTab})
	if err != nil {
		t.Fatal(err)
	}
	var selectedProjection, otherProjection *TaskHubRun
	for index := range snapshot.Runs {
		switch snapshot.Runs[index].RunID {
		case selectedRun.ID:
			selectedProjection = &snapshot.Runs[index]
		case otherRun.ID:
			otherProjection = &snapshot.Runs[index]
		}
	}
	if selectedProjection == nil || otherProjection == nil {
		t.Fatalf("Task Hub did not project both reconcile fixture Runs: %+v", snapshot.Runs)
	}
	selectedCapability, selectedFound := taskHubRunControlActionStateFor(selectedProjection.Control.Actions, TaskHubRunControlReconcile)
	if !selectedFound || !selectedCapability.Enabled || selectedCapability.DisabledReason != "" {
		t.Fatalf("in_doubt Run reconcile capability = %+v", selectedCapability)
	}
	if capability, found := taskHubRunControlActionStateFor(otherProjection.Control.Actions, TaskHubRunControlReconcile); found {
		t.Fatalf("normal Run exposed reconcile capability = %+v", capability)
	}
	preview, err := adapter.PlanTaskHubRunControl(ctx, TaskHubRunControlCommand{
		Action: TaskHubRunControlReconcile,
		Target: TaskHubTarget{TaskID: task.ID, RunID: selectedRun.ID, RevisionID: revision.ID},
	})
	if err != nil || !preview.ConfirmationNeeded || !strings.Contains(preview.Summary, "harbor run reconcile") || !strings.Contains(preview.Summary, "不调用 provider") {
		t.Fatalf("selected local reconcile preview = %+v, err=%v", preview, err)
	}
	otherPreview, err := adapter.PlanTaskHubRunControl(ctx, TaskHubRunControlCommand{
		Action: TaskHubRunControlReconcile,
		Target: TaskHubTarget{TaskID: task.ID, RunID: otherRun.ID, RevisionID: revision.ID},
	})
	if err != nil || otherPreview.ConfirmationNeeded || !strings.Contains(otherPreview.Reason, "只有 interrupted 或 in_doubt") {
		t.Fatalf("normal Run reconcile preview = %+v, err=%v", otherPreview, err)
	}
	missingReasonKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.ExecuteTaskHubRunControlMutation(ctx, TaskHubRunControlMutationRequest{
		Action: TaskHubRunControlReconcile,
		Target: TaskHubTarget{TaskID: task.ID, RunID: selectedRun.ID, RevisionID: revision.ID},
		Actor:  "tester", IdempotencyKey: missingReasonKey,
	}); err == nil {
		t.Fatal("Task Hub local reconcile accepted a missing audit reason")
	}

	m, cleanup := newTestTaskHubV2Model(t, adapter)
	defer cleanup()
	m.taskHub.SelectedTaskID = task.ID
	m.taskHub.SelectedRunID = selectedRun.ID
	m.openRunControl()
	updated, selectCommand := m.Update(runeKey("r"))
	m = updated.(model)
	if selectCommand != nil || m.runControl == nil || m.runControl.SelectedAction != TaskHubRunControlReconcile {
		t.Fatalf("TUI did not select local reconcile preview: overlay=%+v command=%v", m.runControl, selectCommand)
	}
	updated, previewCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if previewCommand == nil {
		t.Fatal("TUI local reconcile did not defer an impact preview")
	}
	updated, _ = m.Update(previewCommand())
	m = updated.(model)
	if m.runControl == nil || m.runControl.Preview == nil || !m.runControl.Preview.ConfirmationNeeded {
		t.Fatalf("TUI local reconcile preview = %+v", m.runControl)
	}
	beforeConfirm, err := services.Store().GetDurableJob(ctx, selectedClaim.Job.ID)
	if err != nil || beforeConfirm == nil || beforeConfirm.State != store.JobRunning {
		t.Fatalf("preview changed selected durable job before confirmation: %+v, %v", beforeConfirm, err)
	}
	updated, confirmationCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if confirmationCommand != nil || m.taskHubMutation == nil || m.taskHubMutation.ControlAction != TaskHubRunControlReconcile {
		t.Fatalf("TUI local reconcile did not open native confirmation: command=%v form=%+v", confirmationCommand, m.taskHubMutation)
	}
	m.taskHubMutation.ReasonInput.SetValue("recover only selected local durable Run")
	actor := m.taskHubMutation.Actor
	key := m.taskHubMutation.IdempotencyKey
	expected := m.taskHubMutation.Expected
	if err := store.ValidateUUIDv7(key); err != nil {
		t.Fatalf("TUI local reconcile key %q is not UUIDv7: %v", key, err)
	}
	updated, executeCommand := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if executeCommand == nil {
		t.Fatal("TUI local reconcile confirmation did not defer execution")
	}
	updated, _ = m.Update(executeCommand())
	m = updated.(model)
	if m.taskHubMutation != nil || !strings.Contains(m.notice, "本地 reconcile 已完成") || !strings.Contains(m.notice, "未调用外部 provider") {
		t.Fatalf("TUI local reconcile completion = form:%+v notice:%q", m.taskHubMutation, m.notice)
	}

	selectedAfter, err := services.Store().GetDurableJob(ctx, selectedClaim.Job.ID)
	if err != nil || selectedAfter == nil || selectedAfter.State != store.JobInterrupted {
		t.Fatalf("selected Run local recovery = %+v, %v", selectedAfter, err)
	}
	selectedLease, err := services.Store().GetLease(ctx, selectedClaim.DispatchLease.ID)
	if err != nil || selectedLease == nil || selectedLease.State != store.LeaseExpired {
		t.Fatalf("selected Run lease recovery = %+v, %v", selectedLease, err)
	}
	otherAfter, err := services.Store().GetDurableJob(ctx, otherClaim.Job.ID)
	if err != nil || otherAfter == nil || otherAfter.State != store.JobRunning {
		t.Fatalf("selected reconcile changed unselected Run job = %+v, %v", otherAfter, err)
	}
	otherLease, err := services.Store().GetLease(ctx, otherClaim.DispatchLease.ID)
	if err != nil || otherLease == nil || otherLease.State != store.LeaseActive {
		t.Fatalf("selected reconcile changed unselected Run lease = %+v, %v", otherLease, err)
	}
	unchangedEffect, err := services.Store().GetSideEffectOperation(ctx, effect.ID)
	if err != nil || unchangedEffect == nil || unchangedEffect.State != store.SideEffectUnknown || unchangedEffect.Version != effectVersion {
		t.Fatalf("local reconcile inferred or changed external provider fact: %+v, %v", unchangedEffect, err)
	}
	controls, err := services.Control.ListForRun(ctx, selectedRun.ID)
	if err != nil || len(controls) != 0 {
		t.Fatalf("local reconcile created a durable control operation: %+v, %v", controls, err)
	}
	selectedRunAfter, err := services.Runs.Get(ctx, selectedRun.ID)
	if err != nil || selectedRunAfter.Status != store.WorkflowRunInDoubt {
		t.Fatalf("local reconcile rewrote external in_doubt Run state: %+v, %v", selectedRunAfter, err)
	}

	// The UI keeps the UUIDv7 key across a lost reply, while the CLI-equivalent
	// local recovery itself is repeat-safe: a direct replay must not re-touch
	// either the selected terminal recovery or the unrelated Run.
	replay, err := adapter.ExecuteTaskHubRunControlMutation(ctx, TaskHubRunControlMutationRequest{
		Action:   TaskHubRunControlReconcile,
		Target:   TaskHubTarget{TaskID: task.ID, RunID: selectedRun.ID, RevisionID: revision.ID},
		Expected: expected, Actor: actor, Reason: "recover only selected local durable Run", IdempotencyKey: key,
	})
	if err != nil || replay.Action != TaskHubRunControlReconcile || replay.OperationID != "" {
		t.Fatalf("repeat local reconcile result = %+v, err=%v", replay, err)
	}
	selectedReplay, err := services.Store().GetDurableJob(ctx, selectedClaim.Job.ID)
	if err != nil || selectedReplay == nil || selectedReplay.Version != selectedAfter.Version || selectedReplay.State != selectedAfter.State {
		t.Fatalf("repeat local reconcile changed selected terminal job: before=%+v after=%+v err=%v", selectedAfter, selectedReplay, err)
	}
	otherReplay, err := services.Store().GetDurableJob(ctx, otherClaim.Job.ID)
	if err != nil || otherReplay == nil || otherReplay.Version != otherAfter.Version || otherReplay.State != otherAfter.State {
		t.Fatalf("repeat local reconcile changed unselected Run job: before=%+v after=%+v err=%v", otherAfter, otherReplay, err)
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
	return taskHubAdapterCompleteProfileForTemplate(t, workflowadapter.StandardWorkflowTemplate())
}

// taskHubAdapterCandidateLeaseProfile gives candidate-provider integration
// tests a realistic frozen provider window. The generic Task Hub profile keeps
// its short stage budget for ordinary UI tests; a manual patch also
// materializes, validates, and binds a candidate before the provider starts.
func taskHubAdapterCandidateLeaseProfile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	profile := taskHubAdapterCompleteProfile(t)
	profile.CandidateProviderBudget = workflowadapter.CandidateProviderBudget{AttemptTimeout: 15 * time.Second}
	return profile
}

func taskHubAdapterCompleteProfileForTemplate(t *testing.T, template workflowadapter.WorkflowTemplate) workflowadapter.ExecutionProfile {
	t.Helper()
	catalog := template.Catalog
	profile := workflowadapter.ExecutionProfile{
		Template:            template.Reference(),
		ID:                  "task-hub-integration",
		Version:             "1",
		ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL,
		ControlGracePeriod:  30 * time.Second,
		CandidateProviderBudget: workflowadapter.CandidateProviderBudget{
			AttemptTimeout: time.Second,
		},
	}
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

func TestTaskHubAdapterCompleteProfileFreezesStandardTemplateInProfileJSON(t *testing.T) {
	profile := taskHubAdapterCompleteProfile(t)
	if !profile.Template.Equal(workflowadapter.StandardTemplateReference()) {
		t.Fatalf("TUI profile template = %#v, want %#v", profile.Template, workflowadapter.StandardTemplateReference())
	}
	raw, err := profile.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical TUI profile: %v", err)
	}
	var document struct {
		Template workflowadapter.TemplateReference `json:"template"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode canonical TUI profile: %v", err)
	}
	if !document.Template.Equal(workflowadapter.StandardTemplateReference()) {
		t.Fatalf("canonical TUI profile template = %#v, want %#v", document.Template, workflowadapter.StandardTemplateReference())
	}
}

func writeTaskHubStartRunInputs(t *testing.T, root, taskID, revisionID, revisionDigest string) (string, string) {
	t.Helper()
	profileRaw, err := taskHubAdapterCompleteProfile(t).CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var profileDocument struct {
		Template workflowadapter.TemplateReference `json:"template"`
	}
	if err := json.Unmarshal(profileRaw, &profileDocument); err != nil {
		t.Fatalf("decode canonical TUI execution profile: %v", err)
	}
	if !profileDocument.Template.Equal(workflowadapter.StandardTemplateReference()) {
		t.Fatalf("canonical TUI execution profile template = %#v, want %#v", profileDocument.Template, workflowadapter.StandardTemplateReference())
	}
	specificationRaw, err := taskHubExecutionSpec(taskID, revisionID, revisionDigest).CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(root, "tui-execution-profile.json")
	specificationPath := filepath.Join(root, "tui-execution-spec.json")
	if err := os.WriteFile(profilePath, profileRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specificationPath, specificationRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	return profilePath, specificationPath
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
