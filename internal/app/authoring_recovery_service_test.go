package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

type authoringRecoveryFixture struct {
	root     string
	services *LifecycleServices
	store    *store.Store
	catalog  *stageprovider.DeploymentOperationCatalogResolver
	source   store.AuthoringSource
	session  store.AuthoringSession
	task     store.TaskV2
	run      store.WorkflowRun
	workflow workflowkit.WorkflowDescriptor
	failed   store.StageAttempt
}

func TestAuthoringRecoveryFreezesRetryPlanAndQueuesOneExecution(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)

	available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, fixture.run.ID)
	if err != nil || !available || reason != "" {
		t.Fatalf("authoring recovery availability = %t, %q, %v", available, reason, err)
	}
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	previewKey := authoringRecoveryUUID(t)
	beforePreview := snapshotAuthoringRecoveryManagedFiles(t, fixture.root)
	preview, err := fixture.services.AuthoringRecovery.PreviewAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: previewKey, RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "inspect frozen authoring recovery",
	})
	if err != nil {
		t.Fatalf("preview authoring recovery: %v", err)
	}
	if persisted, err := fixture.store.GetContinuationCommandByKey(ctx, previewKey); err != nil || persisted != nil {
		t.Fatalf("preview persisted command = %+v, %v", persisted, err)
	}
	if afterPreview := snapshotAuthoringRecoveryManagedFiles(t, fixture.root); !reflect.DeepEqual(afterPreview, beforePreview) {
		t.Fatal("preview authoring recovery changed managed state")
	}
	assertAuthoringRecoveryPlan(t, preview, fixture)

	key := authoringRecoveryUUID(t)
	plan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: key, RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "recover network-failed authoring stage",
	})
	if err != nil {
		t.Fatalf("plan authoring recovery: %v", err)
	}
	assertAuthoringRecoveryPlan(t, plan, fixture)
	command, err := fixture.store.GetContinuationCommandByKey(ctx, key)
	if err != nil || command == nil {
		t.Fatalf("stored authoring recovery command = %+v, %v", command, err)
	}
	persisted, err := decodeAuthoringRecoveryCommand(*command)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Format != authoringRecoveryCommandFormat || len(persisted.FailureStageAttemptIDs) != 1 || persisted.FailureStageAttemptIDs[0] != fixture.failed.ID ||
		persisted.Expected.SubjectID != fixture.source.ID || persisted.Expected.SubjectRevisionID != fixture.session.ID ||
		persisted.Expected.SubjectDigest != workflowkit.SubjectDigest(fixture.source.SnapshotContentDigest) ||
		persisted.Expected.WorkflowFingerprint != workflowkit.Fingerprint(fixture.run.DefinitionHash) ||
		persisted.TargetTaskID != fixture.task.ID || persisted.TargetTaskVersion != fixture.task.Version {
		t.Fatalf("persisted authoring recovery command = %+v", persisted)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(command.PayloadJSON), &payload); err != nil {
		t.Fatalf("decode v2 authoring recovery command payload: %v", err)
	}
	for _, redundant := range []string{"authoring_source_id", "authoring_session_id", "source_digest", "definition_fingerprint"} {
		if _, found := payload[redundant]; found {
			t.Fatalf("v2 authoring recovery command retained redundant %q: %s", redundant, command.PayloadJSON)
		}
	}

	execution, err := fixture.services.AuthoringRecovery.ExecuteAuthoringRecovery(ctx, plan.ID())
	if err != nil {
		t.Fatalf("execute authoring recovery: %v", err)
	}
	replayed, err := fixture.services.AuthoringRecovery.ExecuteAuthoringRecovery(ctx, plan.ID())
	if err != nil || replayed.ID != execution.ID || replayed.State != store.ContinuationExecutionQueued {
		t.Fatalf("replayed recovery execution = %+v, %v; first=%+v", replayed, err, execution)
	}
	updated, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || updated == nil || updated.ExecutionEpoch != fixture.run.ExecutionEpoch+1 || updated.Version != fixture.run.Version+1 {
		t.Fatalf("recovery commit run = %+v, %v", updated, err)
	}
	refreshed, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	replayedPlan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: key, RunID: fixture.run.ID, Expected: refreshed, Actor: "operator", Reason: "recover network-failed authoring stage",
	})
	if err != nil || replayedPlan.ID() != plan.ID() {
		t.Fatalf("replayed plan after epoch advance = %s, %v; first=%s", replayedPlan.ID(), err, plan.ID())
	}
	if _, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "stale new recovery request",
	}); !errors.Is(err, store.ErrOptimisticLock) {
		t.Fatalf("new key with stale checkpoint error = %v, want ErrOptimisticLock", err)
	}
	if _, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: refreshed, Actor: "operator", Reason: "overlapping recovery request",
	}); !errors.Is(err, ErrAuthoringRecoveryUnavailable) {
		t.Fatalf("new key while recovery is active error = %v, want unavailable", err)
	}
	active, err := fixture.store.HasActiveContinuationExecutionForRun(ctx, fixture.run.ID)
	if err != nil || !active {
		t.Fatalf("active authoring recovery = %t, %v", active, err)
	}
	if available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, fixture.run.ID); err != nil || available || !strings.Contains(reason, "active recovery") {
		t.Fatalf("availability while execution is queued = %t, %q, %v", available, reason, err)
	}
}

func TestAuthoringRecoveryAllowsMissingOutputSubmissionProcessFailure(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureProcess)

	available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, fixture.run.ID)
	if err != nil || !available || reason != "" {
		t.Fatalf("missing-output recovery availability = %t, %q, %v", available, reason, err)
	}
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.services.AuthoringRecovery.PreviewAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: checkpoint,
		Actor: "operator", Reason: "retry missing structured output submission",
	})
	if err != nil {
		t.Fatalf("preview missing-output authoring recovery: %v", err)
	}
	assertAuthoringRecoveryPlan(t, plan, fixture)
}

func TestAuthoringRecoveryAllowsQuotaAdmissionRetryOnlyAfterOwnerGrant(t *testing.T) {
	ctx := context.Background()
	fixture, outputAccount := newQuotaExhaustedAuthoringRecoveryFixture(t, ctx)

	if available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, fixture.run.ID); err != nil || available || !strings.Contains(reason, "frozen retry policy") {
		t.Fatalf("quota exhaustion without grant availability = %t, %q, %v", available, reason, err)
	}
	wrongOwnerGrant, err := fixture.store.GrantQuota(ctx, store.GrantBudgetRequest{
		IdempotencyKey: authoringRecoveryUUID(t), ScopeKind: store.QuotaScopeTask, ScopeID: fixture.task.ID,
		Dimension: "output_submission", DeltaUnits: 64, ExpectedVersion: outputAccount.Version,
		Actor: "not-the-task-owner", Reason: "attempt non-owner quota recovery grant",
	})
	if err != nil {
		t.Fatalf("record non-owner quota grant fixture: %v", err)
	}
	if available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, fixture.run.ID); err != nil || available || !strings.Contains(reason, "frozen retry policy") {
		t.Fatalf("quota exhaustion after non-owner grant availability = %t, %q, %v", available, reason, err)
	}
	if _, err := fixture.services.Budgets.GrantRunBudget(ctx, GrantRunBudgetRequest{
		RunID: fixture.run.ID, IdempotencyKey: authoringRecoveryUUID(t), Dimension: "output_submission", DeltaUnits: 64,
		ExpectedVersion: wrongOwnerGrant.Version, Actor: "author", Reason: "owner approves quota recovery for rejected Docker validation",
	}); err != nil {
		t.Fatalf("grant owner quota for recoverable admission: %v", err)
	}
	if available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, fixture.run.ID); err != nil || !available || reason != "" {
		t.Fatalf("quota exhaustion after owner grant availability = %t, %q, %v", available, reason, err)
	}
}

func TestAuthoringRecoveryKeepsUnrelatedPolicyFailureNonRecoverableAfterOwnerGrant(t *testing.T) {
	ctx := context.Background()
	fixture := newUnrelatedPolicyAuthoringRecoveryFixture(t, ctx)
	account, err := fixture.store.CreateQuotaAccount(ctx, store.CreateQuotaAccountRequest{
		ScopeKind: store.QuotaScopeTask, ScopeID: fixture.task.ID, Dimension: "output_submission", LimitUnits: 64,
		Actor: "author", Reason: "configure unrelated policy failure quota fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.services.Budgets.GrantRunBudget(ctx, GrantRunBudgetRequest{
		RunID: fixture.run.ID, IdempotencyKey: authoringRecoveryUUID(t), Dimension: "output_submission", DeltaUnits: 64,
		ExpectedVersion: account.Version, Actor: "author", Reason: "owner grants capacity unrelated to policy failure",
	}); err != nil {
		t.Fatal(err)
	}
	if available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, fixture.run.ID); err != nil || available || !strings.Contains(reason, "frozen retry policy") {
		t.Fatalf("unrelated policy failure availability = %t, %q, %v", available, reason, err)
	}
}

func TestAuthoringRecoveryRejectsWaitingContinuationWithoutWritingRecoveryState(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
	running, err := fixture.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: fixture.run.ID, ExpectedVersion: fixture.run.Version, Status: store.WorkflowRunRunning,
		Actor: "worker", Reason: "reach a legal historical continuation transition",
	})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := fixture.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: running.ID, ExpectedVersion: running.Version, Status: store.WorkflowRunWaitingContinuation,
		Actor: "worker", Reason: "preserve historical authoring continuation state",
	})
	if err != nil {
		t.Fatal(err)
	}
	available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, waiting.ID)
	if err != nil || available || !strings.Contains(reason, string(store.WorkflowRunWaitingContinuation)) {
		t.Fatalf("waiting continuation recovery availability = %t, %q, %v", available, reason, err)
	}
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, waiting.ID)
	if err != nil {
		t.Fatal(err)
	}
	key := authoringRecoveryUUID(t)
	_, err = fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: key, RunID: waiting.ID, Expected: checkpoint, Actor: "operator", Reason: "do not mutate historical waiting continuation",
	})
	if !errors.Is(err, ErrAuthoringRecoveryUnavailable) {
		t.Fatalf("waiting continuation recovery plan = %v, want unavailable", err)
	}
	if command, lookupErr := fixture.store.GetContinuationCommandByKey(ctx, key); lookupErr != nil || command != nil {
		t.Fatalf("waiting continuation created recovery command=%+v err=%v", command, lookupErr)
	}
	updated, err := fixture.store.GetWorkflowRun(ctx, waiting.ID)
	if err != nil || updated == nil || updated.Version != waiting.Version || updated.ExecutionEpoch != waiting.ExecutionEpoch || updated.Status != store.WorkflowRunWaitingContinuation {
		t.Fatalf("waiting continuation Run mutated by rejected recovery: %+v, %v", updated, err)
	}
}

func TestAuthoringRecoveryResumesLegacyV1Command(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	key := authoringRecoveryUUID(t)
	legacy := authoringRecoveryCommandV1{
		Format:                 authoringRecoveryCommandFormatV1,
		CommandKey:             key,
		RunID:                  fixture.run.ID,
		AuthoringSourceID:      fixture.source.ID,
		AuthoringSessionID:     fixture.session.ID,
		TargetTaskID:           fixture.task.ID,
		TargetTaskVersion:      fixture.task.Version,
		SourceDigest:           workflowkit.SubjectDigest(fixture.source.SnapshotContentDigest),
		DefinitionFingerprint:  workflowkit.Fingerprint(fixture.run.DefinitionHash),
		Expected:               checkpoint,
		FailureStageAttemptIDs: []string{fixture.failed.ID},
		TargetNodeIDs:          []workflowkit.NodeID{workflowkit.NodeID(workflowadapter.RepoAnalyze)},
	}
	payload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	record, err := fixture.store.CreateContinuationCommand(ctx, store.CreateContinuationCommandRequest{
		CommandKey: key, SubjectID: fixture.source.ID, RunID: fixture.run.ID, PayloadJSON: string(payload),
		Actor: "operator", Reason: "resume persisted v1 recovery command",
	})
	if err != nil {
		t.Fatal(err)
	}

	plan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: key, RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "resume persisted v1 recovery command",
	})
	if err != nil {
		t.Fatalf("resume legacy v1 authoring recovery command: %v", err)
	}
	assertAuthoringRecoveryPlan(t, plan, fixture)
	decoded, err := decodeAuthoringRecoveryCommand(record)
	if err != nil || decoded.Format != authoringRecoveryCommandFormat || decoded.Expected != checkpoint {
		t.Fatalf("normalize legacy v1 command = %+v, %v", decoded, err)
	}
	if _, err := fixture.services.AuthoringRecovery.ExecuteAuthoringRecovery(ctx, plan.ID()); err != nil {
		t.Fatalf("execute resumed legacy v1 authoring recovery: %v", err)
	}
}

func TestAuthoringRecoveryRevalidatesTargetsAfterCommandPersistence(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	key := authoringRecoveryUUID(t)
	baseObserver := fixture.services.AuthoringRecovery.observer
	observations := 0
	fixture.services.AuthoringRecovery.observer = authoringRecoverySubjectObserverFunc(func(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, workflow workflowkit.WorkflowDescriptor) (continuationRunState, error) {
		state, err := baseObserver.ObserveSubject(ctx, run, subject, workflow)
		if err != nil {
			return continuationRunState{}, err
		}
		observations++
		if observations != 2 {
			return state, nil
		}
		persisted, lookupErr := fixture.store.GetContinuationCommandByKey(ctx, key)
		if lookupErr != nil || persisted == nil {
			t.Fatalf("command was not durable before revalidation: %+v, %v", persisted, lookupErr)
		}
		stageID := workflowkit.NodeID(workflowadapter.RepoAnalyze)
		latest := state.Latest[stageID]
		latest.ID = authoringRecoveryUUID(t)
		state.Latest[stageID] = latest
		return state, nil
	})

	_, err = fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: key, RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "reject target drift after command persistence",
	})
	if !errors.Is(err, store.ErrOptimisticLock) {
		t.Fatalf("target drift recovery plan error = %v, want ErrOptimisticLock", err)
	}
	if observations != 2 {
		t.Fatalf("authoring recovery observations = %d, want post-persistence revalidation", observations)
	}
	record, err := fixture.store.GetContinuationCommandByKey(ctx, key)
	if err != nil || record == nil {
		t.Fatalf("persisted command after target drift = %+v, %v", record, err)
	}
	if frozen, err := fixture.store.GetFrozenPlanByCommand(ctx, record.ID); err != nil || frozen != nil {
		t.Fatalf("target-drifted command froze plan = %+v, %v", frozen, err)
	}
}

func TestAuthoringAdmissionRecoveryRegeneratesEveryPackageProducer(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	workflow, err := template.Compile(lifecycleCompleteProfileForTemplate(t, template))
	if err != nil {
		t.Fatal(err)
	}
	state := continuationRunState{Latest: map[workflowkit.NodeID]store.StageAttempt{
		workflowkit.NodeID(workflowadapter.CodeEdgePackageAdmission): {
			ID: "admission-attempt", StageKey: workflowadapter.CodeEdgePackageAdmission,
			ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictNeedsRepair,
		},
	}}
	selection, err := authoringRecoveryTargets(store.WorkflowRun{Status: store.WorkflowRunWaitingContinuation}, workflow.Descriptor, state)
	if err != nil {
		t.Fatal(err)
	}
	want := []workflowkit.NodeID{
		workflowkit.NodeID(workflowadapter.InstructionGen), workflowkit.NodeID(workflowadapter.TaskTOMLGen), workflowkit.NodeID(workflowadapter.DockerfileGen),
		workflowkit.NodeID(workflowadapter.SolveGen), workflowkit.NodeID(workflowadapter.TestGen), workflowkit.NodeID(workflowadapter.TestsAnalysis),
	}
	if !reflect.DeepEqual(selection.targetNodeIDs, want) || !reflect.DeepEqual(selection.failureStageAttemptIDs, []string{"admission-attempt"}) ||
		len(selection.feedback) != 1 || selection.feedback[0].artifactName != "codeedge_package_admission_report" {
		t.Fatalf("admission recovery selection=%+v", selection)
	}
}

func TestAuthoringGeneratedFilesRecoveryRegeneratesProducer(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	workflow, err := template.Compile(lifecycleCompleteProfileForTemplate(t, template))
	if err != nil {
		t.Fatal(err)
	}
	state := continuationRunState{Latest: map[workflowkit.NodeID]store.StageAttempt{
		workflowkit.NodeID(workflowadapter.GenerateTaskFiles): {
			ID: "generated-files-attempt", StageKey: workflowadapter.GenerateTaskFiles,
			ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictNeedsRepair,
		},
	}}
	selection, err := authoringRecoveryTargets(store.WorkflowRun{Status: store.WorkflowRunWaitingContinuation}, workflow.Descriptor, state)
	if err != nil {
		t.Fatal(err)
	}
	wantTargets := []workflowkit.NodeID{workflowkit.NodeID(workflowadapter.GenerateTaskFiles)}
	if !reflect.DeepEqual(selection.targetNodeIDs, wantTargets) || !reflect.DeepEqual(selection.failureStageAttemptIDs, []string{"generated-files-attempt"}) || len(selection.feedback) != 0 {
		t.Fatalf("generated-files recovery selection=%+v", selection)
	}
}

func TestCurrentAuthoringContentProducerRecoveryAcceptsDirectNeedsRepair(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	workflow, err := template.Compile(lifecycleCompleteProfileForTemplate(t, template))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []workflowkit.NodeID{
		workflowkit.NodeID(workflowadapter.TaskDesign), workflowkit.NodeID(workflowadapter.GenerateTaskFiles),
		workflowkit.NodeID(workflowadapter.InstructionGen), workflowkit.NodeID(workflowadapter.TaskTOMLGen), workflowkit.NodeID(workflowadapter.DockerfileGen),
	} {
		t.Run(string(key), func(t *testing.T) {
			attemptID := "direct-needs-repair-" + string(key)
			state := continuationRunState{Latest: map[workflowkit.NodeID]store.StageAttempt{
				key: {ID: attemptID, StageKey: string(key), ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictNeedsRepair},
			}}
			selection, err := authoringRecoveryTargets(store.WorkflowRun{Status: store.WorkflowRunWaitingContinuation}, workflow.Descriptor, state)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(selection.targetNodeIDs, []workflowkit.NodeID{key}) || !reflect.DeepEqual(selection.failureStageAttemptIDs, []string{attemptID}) || len(selection.feedback) != 0 {
				t.Fatalf("direct content-producer recovery selection=%+v", selection)
			}
		})
	}
}

func TestCurrentAuthoringFixedFileScriptsRejectDirectNeedsRepair(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	workflow, err := template.Compile(lifecycleCompleteProfileForTemplate(t, template))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []workflowkit.NodeID{workflowkit.NodeID(workflowadapter.SolveGen), workflowkit.NodeID(workflowadapter.TestGen)} {
		t.Run(string(key), func(t *testing.T) {
			_, err := authoringRecoveryTargets(store.WorkflowRun{
				Status: store.WorkflowRunWaitingContinuation, WorkflowTemplateID: template.ID, WorkflowTemplateVersion: template.Version,
			}, workflow.Descriptor, continuationRunState{Latest: map[workflowkit.NodeID]store.StageAttempt{
				key: {ID: "fixed-file-needs-repair-" + string(key), StageKey: string(key), ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictNeedsRepair},
			}})
			if !errors.Is(err, ErrAuthoringRecoveryUnavailable) || !strings.Contains(err.Error(), "outside its frozen verdict policy") {
				t.Fatalf("fixed-file %q direct needs_repair error = %v", key, err)
			}
		})
	}
}

func TestHistoricalAuthoringHarnessScriptsRetainDirectNeedsRepair(t *testing.T) {
	template := workflowadapter.StandardAuthoringHarnessWorkflowTemplate()
	workflow, err := template.Compile(lifecycleCompleteProfileForTemplate(t, template))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []workflowkit.NodeID{workflowkit.NodeID(workflowadapter.SolveGen), workflowkit.NodeID(workflowadapter.TestGen)} {
		t.Run(string(key), func(t *testing.T) {
			attemptID := "historical-script-needs-repair-" + string(key)
			selection, err := authoringRecoveryTargets(store.WorkflowRun{
				Status: store.WorkflowRunWaitingContinuation, WorkflowTemplateID: template.ID, WorkflowTemplateVersion: template.Version,
			}, workflow.Descriptor, continuationRunState{Latest: map[workflowkit.NodeID]store.StageAttempt{
				key: {ID: attemptID, StageKey: string(key), ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictNeedsRepair},
			}})
			if err != nil || !reflect.DeepEqual(selection.targetNodeIDs, []workflowkit.NodeID{key}) || !reflect.DeepEqual(selection.failureStageAttemptIDs, []string{attemptID}) {
				t.Fatalf("historical 1.7 script recovery selection=%+v err=%v", selection, err)
			}
		})
	}
}

func TestAuthoringContentProducerRecoveryRejectsVerdictOutsideFrozenPolicy(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	workflow, err := template.Compile(lifecycleCompleteProfileForTemplate(t, template))
	if err != nil {
		t.Fatal(err)
	}
	key := workflowkit.NodeID(workflowadapter.TestsAnalysis)
	state := continuationRunState{Latest: map[workflowkit.NodeID]store.StageAttempt{
		key: {
			ID: "invalid-analysis-needs-repair", StageKey: string(key),
			ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictNeedsRepair,
		},
	}}
	_, err = authoringRecoveryTargets(store.WorkflowRun{Status: store.WorkflowRunWaitingContinuation}, workflow.Descriptor, state)
	if !errors.Is(err, ErrAuthoringRecoveryUnavailable) || !strings.Contains(err.Error(), "outside its frozen verdict policy") {
		t.Fatalf("pass-only analysis recovery error = %v, want frozen-policy rejection", err)
	}
}

func TestHistoricalAuthoringAnalysisRecoveryRetainsNeedsRepairPolicy(t *testing.T) {
	template := workflowadapter.StandardAuthoringRepairFeedbackWorkflowTemplate()
	workflow, err := template.Compile(lifecycleCompleteProfileForTemplate(t, template))
	if err != nil {
		t.Fatal(err)
	}
	key := workflowkit.NodeID(workflowadapter.TestsAnalysis)
	state := continuationRunState{Latest: map[workflowkit.NodeID]store.StageAttempt{
		key: {
			ID: "historical-analysis-needs-repair", StageKey: string(key),
			ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictNeedsRepair,
		},
	}}
	selection, err := authoringRecoveryTargets(store.WorkflowRun{Status: store.WorkflowRunWaitingContinuation}, workflow.Descriptor, state)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(selection.targetNodeIDs, []workflowkit.NodeID{key}) || !reflect.DeepEqual(selection.failureStageAttemptIDs, []string{"historical-analysis-needs-repair"}) {
		t.Fatalf("historical tests_analysis recovery selection = %+v", selection)
	}
}

func TestAuthoringReviewRecoveryRegeneratesReviewedProducers(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	workflow, err := template.Compile(lifecycleCompleteProfileForTemplate(t, template))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		review  workflowkit.NodeID
		targets []workflowkit.NodeID
	}{
		{
			name: "task direction", review: workflowkit.NodeID(workflowadapter.TaskReview),
			targets: []workflowkit.NodeID{workflowkit.NodeID(workflowadapter.RepoAnalyze), workflowkit.NodeID(workflowadapter.TaskDesign)},
		},
		{
			name: "generated content", review: workflowkit.NodeID(workflowadapter.ContentReview),
			targets: []workflowkit.NodeID{
				workflowkit.NodeID(workflowadapter.InstructionGen), workflowkit.NodeID(workflowadapter.TaskTOMLGen), workflowkit.NodeID(workflowadapter.DockerfileGen),
			},
		},
		{
			name: "solution and tests", review: workflowkit.NodeID(workflowadapter.SolutionReview),
			targets: []workflowkit.NodeID{
				workflowkit.NodeID(workflowadapter.InstructionGen), workflowkit.NodeID(workflowadapter.TaskTOMLGen), workflowkit.NodeID(workflowadapter.DockerfileGen),
				workflowkit.NodeID(workflowadapter.SolveGen), workflowkit.NodeID(workflowadapter.TestGen), workflowkit.NodeID(workflowadapter.TestsAnalysis),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attemptID := "needs-repair-" + string(test.review)
			state := continuationRunState{Latest: map[workflowkit.NodeID]store.StageAttempt{
				test.review: {
					ID: attemptID, StageKey: string(test.review), ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictNeedsRepair,
				},
			}}
			selection, err := authoringRecoveryTargets(store.WorkflowRun{Status: store.WorkflowRunWaitingContinuation}, workflow.Descriptor, state)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(selection.targetNodeIDs, test.targets) || !reflect.DeepEqual(selection.failureStageAttemptIDs, []string{attemptID}) || len(selection.feedback) != 1 {
				t.Fatalf("review recovery selection=%+v", selection)
			}
		})
	}
}

func TestAuthoringContentReviewRecoverySchedulesPackageClosure(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	workflow, err := template.Compile(lifecycleCompleteProfileForTemplate(t, template))
	if err != nil {
		t.Fatal(err)
	}
	workflowFingerprint, err := workflow.Descriptor.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	state := continuationRunState{
		Latest: map[workflowkit.NodeID]store.StageAttempt{
			workflowkit.NodeID(workflowadapter.ContentReview): {
				ID:              "content-repair",
				StageKey:        workflowadapter.ContentReview,
				ExecutionStatus: store.StageExecutionCompleted,
				Verdict:         store.VerdictNeedsRepair,
			},
		},
		ReuseStates: make([]workflowkit.StageReuseState, 0, len(workflow.Descriptor.Stages)),
	}
	for _, stage := range workflow.Descriptor.Stages {
		state.ReuseStates = append(state.ReuseStates, workflowkit.StageReuseState{
			NodeID:                   stage.Key,
			Present:                  true,
			ArtifactsIntact:          true,
			ExpectedInputFingerprint: emptyInputs,
		})
	}
	selection, err := authoringRecoveryTargets(store.WorkflowRun{Status: store.WorkflowRunWaitingContinuation}, workflow.Descriptor, state)
	if err != nil {
		t.Fatal(err)
	}
	invalidation, err := workflowkit.PlanInvalidation(workflow.Descriptor, workflowkit.InvalidationRequest{
		RecomputeNodes: selection.targetNodeIDs,
		ReuseStates:    state.ReuseStates,
		Matcher:        workflowadapter.HarborResourceMatch,
	})
	if err != nil {
		t.Fatal(err)
	}
	subjectDigest := workflowkit.SubjectDigest(workflowkit.SHA256Fingerprint([]byte("authoring-source")))
	checkpoint := workflowkit.CheckpointRef{
		Sequence: 1, ExecutionEpoch: 1, SubjectID: "authoring-source", SubjectRevisionID: "authoring-session",
		SubjectDigest: subjectDigest, WorkflowFingerprint: workflowFingerprint,
	}
	snapshot, err := buildAuthoringRecoveryPlan("plan", "command", normalizedAuthoringRecoveryCommand{
		Expected: checkpoint, TargetNodeIDs: selection.targetNodeIDs,
	}, store.WorkflowRun{ID: "run", ExecutionEpoch: 1, DefinitionHash: string(workflowFingerprint)}, workflowRunSubject{
		Binding: workflowkit.SubjectBinding{
			SubjectID: checkpoint.SubjectID, RevisionID: checkpoint.SubjectRevisionID, Digest: subjectDigest,
		},
		Kind:             store.WorkflowRunSubjectAuthoringSession,
		AuthoringSource:  &store.AuthoringSource{ID: checkpoint.SubjectID, SnapshotContentDigest: string(subjectDigest)},
		AuthoringSession: &store.AuthoringSession{ID: checkpoint.SubjectRevisionID},
	}, workflow.Descriptor, state, invalidation, nil, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]workflowkit.NodeID, 0, len(snapshot.Schedule))
	for _, batch := range snapshot.Schedule {
		got = append(got, batch.NodeIDs...)
	}
	want := []workflowkit.NodeID{
		workflowkit.NodeID(workflowadapter.InstructionGen),
		workflowkit.NodeID(workflowadapter.TaskTOMLGen),
		workflowkit.NodeID(workflowadapter.DockerfileGen),
		workflowkit.NodeID(workflowadapter.DockerfileBuildValidate),
		workflowkit.NodeID(workflowadapter.ContentReview),
		workflowkit.NodeID(workflowadapter.SolveGen),
		workflowkit.NodeID(workflowadapter.TestGen),
		workflowkit.NodeID(workflowadapter.AuthoringHarness),
		workflowkit.NodeID(workflowadapter.TestsAnalysis),
		workflowkit.NodeID(workflowadapter.CodeEdgePackageAdmission),
		workflowkit.NodeID(workflowadapter.SolutionReview),
		workflowkit.NodeID(workflowadapter.MaterializeTask),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("content-review recovery schedule = %v, want %v", got, want)
	}
}

func TestAuthoringReviewRecoveryUnionsNestedNeedsRepairFeedback(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	workflow, err := template.Compile(lifecycleCompleteProfileForTemplate(t, template))
	if err != nil {
		t.Fatal(err)
	}
	state := continuationRunState{Latest: map[workflowkit.NodeID]store.StageAttempt{
		workflowkit.NodeID(workflowadapter.ContentReview): {
			ID: "content-repair", StageKey: workflowadapter.ContentReview, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictNeedsRepair,
		},
		workflowkit.NodeID(workflowadapter.SolutionReview): {
			ID: "solution-repair", StageKey: workflowadapter.SolutionReview, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictNeedsRepair,
		},
	}}
	selection, err := authoringRecoveryTargets(store.WorkflowRun{Status: store.WorkflowRunWaitingContinuation}, workflow.Descriptor, state)
	if err != nil {
		t.Fatal(err)
	}
	wantTargets := []workflowkit.NodeID{
		workflowkit.NodeID(workflowadapter.InstructionGen), workflowkit.NodeID(workflowadapter.TaskTOMLGen), workflowkit.NodeID(workflowadapter.DockerfileGen),
		workflowkit.NodeID(workflowadapter.SolveGen), workflowkit.NodeID(workflowadapter.TestGen), workflowkit.NodeID(workflowadapter.TestsAnalysis),
	}
	if !reflect.DeepEqual(selection.targetNodeIDs, wantTargets) || !reflect.DeepEqual(selection.failureStageAttemptIDs, []string{"content-repair", "solution-repair"}) || len(selection.feedback) != 2 {
		t.Fatalf("nested review repair selection=%+v", selection)
	}
}

func TestAuthoringInfrastructureRetryRetainsActiveReviewFeedback(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	workflow, err := template.Compile(lifecycleCompleteProfileForTemplate(t, template))
	if err != nil {
		t.Fatal(err)
	}
	state := continuationRunState{Latest: map[workflowkit.NodeID]store.StageAttempt{
		workflowkit.NodeID(workflowadapter.ContentReview): {
			ID: "content-repair", StageKey: workflowadapter.ContentReview, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictNeedsRepair,
		},
		workflowkit.NodeID(workflowadapter.SolveGen): {
			ID: "solve-failure", StageKey: workflowadapter.SolveGen, ExecutionStatus: store.StageExecutionInfraFailed, FailureClass: string(workflowkit.FailureNetwork),
		},
	}}
	selection, err := authoringRecoveryTargets(store.WorkflowRun{Status: store.WorkflowRunFailedRecoverable}, workflow.Descriptor, state)
	if err != nil {
		t.Fatal(err)
	}
	wantTargets := []workflowkit.NodeID{
		workflowkit.NodeID(workflowadapter.InstructionGen), workflowkit.NodeID(workflowadapter.TaskTOMLGen), workflowkit.NodeID(workflowadapter.DockerfileGen),
		workflowkit.NodeID(workflowadapter.SolveGen),
	}
	if !reflect.DeepEqual(selection.targetNodeIDs, wantTargets) || !reflect.DeepEqual(selection.failureStageAttemptIDs, []string{"content-repair", "solve-failure"}) ||
		len(selection.feedback) != 1 || selection.feedback[0].artifactName != "content_review_decision" {
		t.Fatalf("infrastructure retry repair selection=%+v", selection)
	}
}

func TestAuthoringReviewRecoveryFreezesDurableDecisionFeedback(t *testing.T) {
	ctx := context.Background()
	fixture, decisionRef, reason := newAuthoringTaskReviewRepairFixture(t, ctx)

	available, unavailableReason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, fixture.run.ID)
	if err != nil || !available || unavailableReason != "" {
		t.Fatalf("task-review repair availability = %t, %q, %v", available, unavailableReason, err)
	}
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: checkpoint,
		Actor: "operator", Reason: "apply frozen task review feedback",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantBinding := workflowkit.ArtifactBinding{
		Name: "task_review_decision", ArtifactID: workflowkit.ArtifactID(decisionRef.ID),
		ContentDigest: workflowkit.Fingerprint(decisionRef.ContentDigest), SchemaVersion: decisionRef.SchemaVersion,
	}
	seen := 0
	for _, transition := range plan.Snapshot().Nodes {
		if transition.NodeID != workflowkit.NodeID(workflowadapter.RepoAnalyze) && transition.NodeID != workflowkit.NodeID(workflowadapter.TaskDesign) {
			continue
		}
		seen++
		if transition.Disposition != workflowkit.DispositionSchedule || len(transition.InputBindings) != 1 || transition.InputBindings[0] != wantBinding {
			t.Fatalf("repair transition %q = %+v", transition.NodeID, transition)
		}
		fingerprint, err := workflowkit.FingerprintArtifactBindings(transition.InputBindings)
		if err != nil || transition.ExpectedInputFingerprint != fingerprint {
			t.Fatalf("repair transition %q fingerprint = %q, %v; want %q", transition.NodeID, transition.ExpectedInputFingerprint, err, fingerprint)
		}
	}
	if seen != 2 {
		t.Fatalf("task-review repair bound feedback to %d producers, want 2", seen)
	}
	subject, err := fixture.services.core.resolveWorkflowRunSubject(ctx, fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	reader := newStageInputReaderForSubject(fixture.store, fixture.services.core.objects, fixture.run, subject, []workflowkit.ArtifactBinding{wantBinding})
	decisionRaw, err := reader(ctx, wantBinding)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(decisionRaw), reason) {
		t.Fatalf("frozen repair feedback omitted decision reason: %s", decisionRaw)
	}
}

func TestAuthoringReviewRecoveryFailsClosedWhenDecisionObjectIsMissing(t *testing.T) {
	ctx := context.Background()
	fixture, decisionRef, _ := newAuthoringTaskReviewRepairFixture(t, ctx)
	manifest, err := loadStageArtifactManifestIndex(ctx, fixture.store, decisionRef.ManifestID)
	if err != nil {
		t.Fatal(err)
	}
	object, err := manifest.objectFor(decisionRef)
	if err != nil {
		t.Fatal(err)
	}
	path, err := fixture.services.core.objects.ObjectPath(object)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, fixture.run.ID)
	if err != nil || available || !strings.Contains(reason, "lacks immutable repair feedback") {
		t.Fatalf("missing repair object availability = %t, %q, %v", available, reason, err)
	}
}

func TestAuthoringReviewRecoveryRejectsExecutionWhenFrozenFeedbackDisappearsBeforeCommit(t *testing.T) {
	ctx := context.Background()
	fixture, decisionRef, _ := newAuthoringTaskReviewRepairFixture(t, ctx)
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: checkpoint,
		Actor: "operator", Reason: "reject missing task review feedback before execution commit",
	})
	if err != nil {
		t.Fatal(err)
	}
	removeAuthoringRecoveryArtifactObject(t, ctx, fixture, decisionRef)

	_, err = fixture.services.AuthoringRecovery.ExecuteAuthoringRecovery(ctx, plan.ID())
	if !errors.Is(err, ErrAuthoringRecoveryUnavailable) || !strings.Contains(err.Error(), "required continuation input drift") {
		t.Fatalf("pre-commit missing frozen feedback error = %v, want unavailable input drift", err)
	}
	executionKey := "authoring-recovery-execution:" + plan.ID()
	if execution, lookupErr := fixture.store.GetContinuationExecutionByIdempotency(ctx, executionKey); lookupErr != nil || execution != nil {
		t.Fatalf("pre-commit missing feedback execution = %+v, %v; want none", execution, lookupErr)
	}
	updatedRun, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || updatedRun == nil || updatedRun.Status != store.WorkflowRunWaitingContinuation || updatedRun.ExecutionEpoch != fixture.run.ExecutionEpoch || updatedRun.Version != fixture.run.Version {
		t.Fatalf("pre-commit missing feedback Run = %+v, %v; want unchanged %+v", updatedRun, err, fixture.run)
	}
	jobs, err := fixture.store.ListDurableJobsForRun(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.CommandType == "task_continuation.execute" {
			t.Fatalf("pre-commit missing feedback published continuation job %+v", job)
		}
	}
}

func TestAuthoringReviewRecoveryMarksCommittedExecutionReconcileRequiredWhenFrozenFeedbackDisappears(t *testing.T) {
	ctx := context.Background()
	fixture, decisionRef, _ := newAuthoringTaskReviewRepairFixture(t, ctx)
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: checkpoint,
		Actor: "operator", Reason: "freeze task review feedback before execution",
	})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := fixture.services.AuthoringRecovery.ExecuteAuthoringRecovery(ctx, plan.ID())
	if err != nil {
		t.Fatal(err)
	}
	removeAuthoringRecoveryArtifactObject(t, ctx, fixture, decisionRef)
	attemptsBefore, err := fixture.store.ListStageAttemptsForRun(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := fixture.store.ListDurableJobsForRun(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var executionJob store.DurableJob
	for _, job := range jobs {
		if job.CommandType == "task_continuation.execute" && job.EntityID == execution.ID {
			executionJob = job
			break
		}
	}
	if executionJob.ID == "" {
		t.Fatal("queued authoring recovery has no continuation job")
	}
	runtime := &FrozenExecutionRuntime{core: fixture.services.core, services: fixture.services}
	state, err := runtime.handleContinuation(ctx, DurableJobExecution{}, executionJob)
	if err == nil || state != store.JobFailed || !errors.Is(err, ErrFrozenExecutionPayload) {
		t.Fatalf("missing frozen feedback execution = %s, %v", state, err)
	}
	updatedExecution, err := fixture.store.GetContinuationExecution(ctx, execution.ID)
	if err != nil || updatedExecution == nil || updatedExecution.State != store.ContinuationExecutionReconcileRequired || updatedExecution.FinishedAt != nil {
		t.Fatalf("missing-feedback continuation = %+v, %v", updatedExecution, err)
	}
	updatedRun, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || updatedRun == nil || updatedRun.Status != store.WorkflowRunInDoubt {
		t.Fatalf("missing-feedback Run = %+v, %v", updatedRun, err)
	}
	active, err := fixture.store.HasActiveContinuationExecutionForRun(ctx, fixture.run.ID)
	if err != nil || active {
		t.Fatalf("missing-feedback active continuation = %t, %v", active, err)
	}
	attemptsAfter, err := fixture.store.ListStageAttemptsForRun(ctx, fixture.run.ID)
	if err != nil || len(attemptsAfter) != len(attemptsBefore) {
		t.Fatalf("missing-feedback StageAttempts = %d, %v; want %d", len(attemptsAfter), err, len(attemptsBefore))
	}
}

func removeAuthoringRecoveryArtifactObject(t *testing.T, ctx context.Context, fixture authoringRecoveryFixture, reference store.ArtifactRef) {
	t.Helper()
	manifest, err := loadStageArtifactManifestIndex(ctx, fixture.store, reference.ManifestID)
	if err != nil {
		t.Fatal(err)
	}
	object, err := manifest.objectFor(reference)
	if err != nil {
		t.Fatal(err)
	}
	path, err := fixture.services.core.objects.ObjectPath(object)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func TestAuthoringRecoveryRejectsExecutionWhenPreservedArtifactDriftsBeforeCommit(t *testing.T) {
	ctx := context.Background()
	for _, scenario := range []struct {
		name   string
		mutate func(*testing.T, authoringRecoveryFixture, store.ArtifactRef)
	}{
		{
			name: "missing object",
			mutate: func(t *testing.T, fixture authoringRecoveryFixture, reference store.ArtifactRef) {
				removeAuthoringRecoveryArtifactObject(t, ctx, fixture, reference)
			},
		},
		{
			name: "corrupt object",
			mutate: func(t *testing.T, fixture authoringRecoveryFixture, reference store.ArtifactRef) {
				path := authoringRecoveryArtifactObjectPath(t, ctx, fixture, reference)
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("corrupted preserved artifact"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
			seedAuthoringRecoveryRepoPrepare(t, ctx, fixture)
			checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
				CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: checkpoint,
				Actor: "operator", Reason: "reject drift in a preserved source artifact",
			})
			if err != nil {
				t.Fatal(err)
			}
			reference := authoringRecoveryStageArtifactRef(t, ctx, fixture, workflowadapter.RepoPrepare)
			scenario.mutate(t, fixture, reference)

			_, err = fixture.services.AuthoringRecovery.ExecuteAuthoringRecovery(ctx, plan.ID())
			if !errors.Is(err, ErrAuthoringRecoveryUnavailable) || !strings.Contains(err.Error(), "preserved stage") {
				t.Fatalf("preserved artifact drift execution error = %v", err)
			}
			executionKey := "authoring-recovery-execution:" + plan.ID()
			if execution, lookupErr := fixture.store.GetContinuationExecutionByIdempotency(ctx, executionKey); lookupErr != nil || execution != nil {
				t.Fatalf("preserved artifact drift execution = %+v, %v; want none", execution, lookupErr)
			}
			updated, lookupErr := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
			if lookupErr != nil || updated == nil || updated.Status != fixture.run.Status || updated.ExecutionEpoch != fixture.run.ExecutionEpoch || updated.Version != fixture.run.Version {
				t.Fatalf("preserved artifact drift Run = %+v, %v; want unchanged %+v", updated, lookupErr, fixture.run)
			}
		})
	}
}

func TestAuthoringRecoveryReconcilesWhenPreservedArtifactDriftsAfterCommit(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
	seedAuthoringRecoveryRepoPrepare(t, ctx, fixture)
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: checkpoint,
		Actor: "operator", Reason: "reconcile drift after recovery commit",
	})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := fixture.services.AuthoringRecovery.ExecuteAuthoringRecovery(ctx, plan.ID())
	if err != nil {
		t.Fatal(err)
	}
	reference := authoringRecoveryStageArtifactRef(t, ctx, fixture, workflowadapter.RepoPrepare)
	removeAuthoringRecoveryArtifactObject(t, ctx, fixture, reference)
	attemptsBefore, err := fixture.store.ListStageAttemptsForRun(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	job := authoringRecoveryExecutionJob(t, ctx, fixture, execution.ID)
	runtime := &FrozenExecutionRuntime{core: fixture.services.core, services: fixture.services}
	state, err := runtime.handleContinuation(ctx, DurableJobExecution{}, job)
	if err == nil || state != store.JobFailed || !errors.Is(err, ErrFrozenExecutionPayload) {
		t.Fatalf("preserved artifact runtime drift = %s, %v", state, err)
	}
	updatedExecution, err := fixture.store.GetContinuationExecution(ctx, execution.ID)
	if err != nil || updatedExecution == nil || updatedExecution.State != store.ContinuationExecutionReconcileRequired {
		t.Fatalf("preserved artifact reconciliation = %+v, %v", updatedExecution, err)
	}
	attemptsAfter, err := fixture.store.ListStageAttemptsForRun(ctx, fixture.run.ID)
	if err != nil || len(attemptsAfter) != len(attemptsBefore) {
		t.Fatalf("preserved artifact drift StageAttempts = %d, %v; want %d", len(attemptsAfter), err, len(attemptsBefore))
	}
}

func TestAuthoringRecoverySchedulesUpstreamStageWhenInputFingerprintDrifts(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
	seedAuthoringRecoveryRepoPrepare(t, ctx, fixture)
	baseObserver := fixture.services.AuthoringRecovery.observer
	fixture.services.AuthoringRecovery.observer = authoringRecoverySubjectObserverFunc(func(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, workflow workflowkit.WorkflowDescriptor) (continuationRunState, error) {
		state, err := baseObserver.ObserveSubject(ctx, run, subject, workflow)
		if err != nil {
			return continuationRunState{}, err
		}
		for index := range state.ReuseStates {
			if state.ReuseStates[index].NodeID == workflowkit.NodeID(workflowadapter.RepoPrepare) {
				state.ReuseStates[index].ExpectedInputFingerprint = workflowkit.SHA256Fingerprint([]byte("drifted repo_prepare inputs"))
			}
		}
		return state, nil
	})
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: checkpoint,
		Actor: "operator", Reason: "rebuild source preparation after input fingerprint drift",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range plan.Snapshot().Nodes {
		if transition.NodeID != workflowkit.NodeID(workflowadapter.RepoPrepare) {
			continue
		}
		if transition.Disposition != workflowkit.DispositionSchedule || !containsAuthoringRecoveryPlanReason(transition.ReasonCodes, workflowkit.PlanReason(workflowkit.InvalidationInputFingerprintDrift)) {
			t.Fatalf("input-drifted repo_prepare transition = %+v", transition)
		}
		return
	}
	t.Fatal("input-drifted recovery plan omitted repo_prepare")
}

func containsAuthoringRecoveryPlanReason(reasons []workflowkit.PlanReason, want workflowkit.PlanReason) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

func authoringRecoveryExecutionJob(t *testing.T, ctx context.Context, fixture authoringRecoveryFixture, executionID string) store.DurableJob {
	t.Helper()
	jobs, err := fixture.store.ListDurableJobsForRun(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.CommandType == "task_continuation.execute" && job.EntityID == executionID {
			return job
		}
	}
	t.Fatalf("authoring recovery execution %s has no durable job", executionID)
	return store.DurableJob{}
}

func authoringRecoveryStageArtifactRef(t *testing.T, ctx context.Context, fixture authoringRecoveryFixture, stageKey string) store.ArtifactRef {
	t.Helper()
	attempts, err := fixture.store.ListStageAttemptsForRun(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range attempts {
		if attempt.StageKey != stageKey || attempt.ExecutionStatus != store.StageExecutionCompleted || attempt.ArtifactManifestID == "" {
			continue
		}
		references, listErr := fixture.store.ListArtifactRefs(ctx, attempt.ArtifactManifestID)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(references) > 0 {
			return references[0]
		}
	}
	t.Fatalf("stage %q has no completed artifact reference", stageKey)
	return store.ArtifactRef{}
}

func authoringRecoveryArtifactObjectPath(t *testing.T, ctx context.Context, fixture authoringRecoveryFixture, reference store.ArtifactRef) string {
	t.Helper()
	manifest, err := loadStageArtifactManifestIndex(ctx, fixture.store, reference.ManifestID)
	if err != nil {
		t.Fatal(err)
	}
	object, err := manifest.objectFor(reference)
	if err != nil {
		t.Fatal(err)
	}
	path, err := fixture.services.core.objects.ObjectPath(object)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAuthoringRecoveryReusesVerifiedSourceSessionArtifacts(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
	seedAuthoringRecoveryRepoPrepare(t, ctx, fixture)
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "reuse verified source preparation",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range plan.Snapshot().Nodes {
		if transition.NodeID != workflowkit.NodeID(workflowadapter.RepoPrepare) {
			continue
		}
		if transition.Disposition != workflowkit.DispositionPreserve || transition.ExpectedInputFingerprint == "" {
			t.Fatalf("repo_prepare transition = %+v, want verified preservation", transition)
		}
		return
	}
	t.Fatal("authoring recovery plan omitted repo_prepare")
}

func TestAuthoringRecoveryResumesPausedRunBeforeFirstStage(t *testing.T) {
	ctx := context.Background()
	fixture := newPausedAuthoringRecoveryFixture(t)

	available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, fixture.run.ID)
	if err != nil || !available || reason != "" {
		t.Fatalf("paused authoring recovery availability = %t, %q, %v", available, reason, err)
	}
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "resume before first stage",
	})
	if err != nil {
		t.Fatalf("plan paused authoring recovery: %v", err)
	}
	for _, transition := range plan.Snapshot().Nodes {
		if transition.NodeID == workflowkit.NodeID(workflowadapter.RepoPrepare) {
			if transition.Disposition != workflowkit.DispositionSchedule {
				t.Fatalf("paused source-root transition = %+v", transition)
			}
			if _, err := fixture.services.AuthoringRecovery.ExecuteAuthoringRecovery(ctx, plan.ID()); err != nil {
				t.Fatalf("execute paused authoring recovery: %v", err)
			}
			return
		}
	}
	t.Fatal("paused authoring recovery omitted repo_prepare")
}

func TestAuthoringRecoveryFailsClosedOnDeploymentCatalogDrift(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)

	driftedDocument := fixture.catalog.Catalog()
	driftedDocument.CatalogVersion = "authoring-recovery-drift"
	driftedCatalog, err := stageprovider.NewDeploymentOperationCatalogResolver(driftedDocument)
	if err != nil {
		t.Fatalf("build drifted deployment catalog: %v", err)
	}
	driftedServices, err := NewLifecycleServicesWithOptions(fixture.root, fixture.store, standardAuthoringLaunchTestOptions(nil, nil, driftedCatalog))
	if err != nil {
		t.Fatalf("reopen lifecycle services with drifted catalog: %v", err)
	}

	available, reason, err := driftedServices.AuthoringRecovery.CanRecover(ctx, fixture.run.ID)
	if err != nil || available || !strings.Contains(reason, stageprovider.ErrDeploymentOperationCatalogDrift.Error()) {
		t.Fatalf("catalog-drifted recovery availability = %t, %q, %v", available, reason, err)
	}
	checkpoint, err := driftedServices.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	commandKey := authoringRecoveryUUID(t)
	if _, err := driftedServices.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: commandKey, RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "reject catalog-drifted recovery",
	}); !errors.Is(err, ErrAuthoringRecoveryUnavailable) {
		t.Fatalf("catalog-drifted recovery plan error = %v, want unavailable", err)
	}
	if command, err := fixture.store.GetContinuationCommandByKey(ctx, commandKey); err != nil || command != nil {
		t.Fatalf("catalog-drifted recovery persisted command = %+v, %v", command, err)
	}
	updated, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || updated == nil || updated.Version != fixture.run.Version || updated.ExecutionEpoch != fixture.run.ExecutionEpoch {
		t.Fatalf("catalog-drifted recovery changed run = %+v, %v", updated, err)
	}
}

func TestAuthoringRecoveryFailsClosedOnDeploymentCatalogLockDrift(t *testing.T) {
	ctx := context.Background()
	fixture := newLockedAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
	driftedResolver := &authoringRecoveryCatalogLockResolver{
		DeploymentOperationCatalogResolver: fixture.catalog,
		identity:                           authoringRecoveryCatalogLockIdentity("v2"),
	}
	driftedServices, err := NewLifecycleServicesWithOptions(fixture.root, fixture.store, LifecycleServicesOptions{
		OperationResolver: driftedResolver,
		DeploymentCatalogResolvers: []TemplateDeploymentCatalogResolver{{
			Template: workflowadapter.StandardAuthoringCurrentTemplateReference(), Resolver: driftedResolver,
		}},
		RequireDeploymentCatalog: true,
		RequireDeploymentLock:    true,
	})
	if err != nil {
		t.Fatalf("reopen lifecycle services with drifted catalog lock: %v", err)
	}

	available, reason, err := driftedServices.AuthoringRecovery.CanRecover(ctx, fixture.run.ID)
	if err != nil || available || !strings.Contains(reason, stageprovider.ErrDeploymentOperationCatalogLockDrift.Error()) {
		t.Fatalf("catalog-lock-drifted recovery availability = %t, %q, %v", available, reason, err)
	}
	checkpoint, err := driftedServices.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	commandKey := authoringRecoveryUUID(t)
	if _, err := driftedServices.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: commandKey, RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "reject catalog-lock-drifted recovery",
	}); !errors.Is(err, ErrAuthoringRecoveryUnavailable) {
		t.Fatalf("catalog-lock-drifted recovery plan error = %v, want unavailable", err)
	}
	if command, err := fixture.store.GetContinuationCommandByKey(ctx, commandKey); err != nil || command != nil {
		t.Fatalf("catalog-lock-drifted recovery persisted command = %+v, %v", command, err)
	}
	updated, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || updated == nil || updated.Version != fixture.run.Version || updated.ExecutionEpoch != fixture.run.ExecutionEpoch {
		t.Fatalf("catalog-lock-drifted recovery changed run = %+v, %v", updated, err)
	}
}

func TestAuthoringRecoveryRejectsInDoubtAndFrozenNonRetryableFailure(t *testing.T) {
	ctx := context.Background()
	t.Run("in doubt requires reconciliation", func(t *testing.T) {
		fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
		run := transitionAuthoringRecoveryRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunInDoubt)
		fixture.run = run
		available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, run.ID)
		if err != nil || available || !strings.Contains(reason, store.ErrContinuationReconciliationRequired.Error()) {
			t.Fatalf("in_doubt availability = %t, %q, %v", available, reason, err)
		}
		checkpoint, checkpointErr := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, run.ID)
		if checkpointErr != nil {
			t.Fatal(checkpointErr)
		}
		_, err = fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
			CommandKey: authoringRecoveryUUID(t), RunID: run.ID, Expected: checkpoint, Actor: "operator", Reason: "attempt unreconciled recovery",
		})
		if !errors.Is(err, store.ErrContinuationReconciliationRequired) {
			t.Fatalf("in_doubt plan error = %v, want reconciliation requirement", err)
		}
	})
	t.Run("retry policy rejects permanent failure", func(t *testing.T) {
		fixture := newAuthoringRecoveryFixture(t, workflowkit.FailurePermanent)
		available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, fixture.run.ID)
		if err != nil || available || !strings.Contains(reason, "retry policy") {
			t.Fatalf("permanent failure availability = %t, %q, %v", available, reason, err)
		}
		checkpoint, checkpointErr := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
		if checkpointErr != nil {
			t.Fatal(checkpointErr)
		}
		_, err = fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
			CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "attempt permanent failure recovery",
		})
		if !errors.Is(err, ErrAuthoringRecoveryUnavailable) {
			t.Fatalf("permanent failure plan error = %v, want authoring recovery unavailable", err)
		}
	})
}

func TestAuthoringRecoveryRejectsMaterializedRunWithoutSecondRevision(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "freeze plan before materialization race",
	})
	if err != nil {
		t.Fatal(err)
	}
	run := transitionAuthoringRecoveryRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunRunning)
	task, err := fixture.store.GetTaskV2(ctx, fixture.task.ID)
	if err != nil || task == nil {
		t.Fatalf("load draft task = %+v, %v", task, err)
	}
	materialized, err := fixture.store.MaterializeAuthoringTask(ctx, store.MaterializeAuthoringTaskRequest{
		IdempotencyKey: authoringRecoveryUUID(t), AuthoringSessionID: fixture.session.ID, AuthoringRunID: run.ID,
		ExpectedTaskVersion: task.Version, ExpectedRunVersion: run.Version,
		TaskDigest: "harbor.task.v2:sha256:" + strings.Repeat("d", 64), ProposalDigest: "proposal",
		ManifestID: "authoring-recovery-materialized-manifest", ChangeSummary: "race fixture", MetadataJSON: `{}`,
		Actor: "worker", Reason: "materialize before recovery commit",
	})
	if err != nil {
		t.Fatalf("materialize authoring fixture: %v", err)
	}
	loadedRun, err := fixture.store.GetWorkflowRun(ctx, run.ID)
	if err != nil || loadedRun == nil {
		t.Fatalf("load materialized Run = %+v, %v", loadedRun, err)
	}
	runValue := transitionAuthoringRecoveryRun(t, ctx, fixture.store, *loadedRun, store.WorkflowRunFailedRecoverable)
	fixture.run = runValue
	if _, err := fixture.services.AuthoringRecovery.ExecuteAuthoringRecovery(ctx, plan.ID()); !errors.Is(err, ErrAuthoringRecoveryUnavailable) {
		t.Fatalf("materialized recovery execution error = %v, want unavailable", err)
	}
	finalTask, err := fixture.store.GetTaskV2(ctx, fixture.task.ID)
	if err != nil || finalTask == nil || finalTask.CurrentRevisionID != materialized.Revision.ID {
		t.Fatalf("materialized task = %+v, %v", finalTask, err)
	}
	if duplicate, err := fixture.store.GetAuthoringTaskMaterializationForRun(ctx, fixture.run.ID); err != nil || duplicate == nil || duplicate.RevisionID != materialized.Revision.ID {
		t.Fatalf("materialization receipt after rejected recovery = %+v, %v", duplicate, err)
	}
}

func newAuthoringTaskReviewRepairFixture(t *testing.T, ctx context.Context) (authoringRecoveryFixture, store.ArtifactRef, string) {
	t.Helper()
	fixture := newAuthoringRecoveryLaunchFixture(t)
	fixture.run = transitionAuthoringRecoveryRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunRunning)
	seedCompletedAuthoringRecoveryStage(t, ctx, fixture, workflowkit.NodeID(workflowadapter.RepoPrepare), map[string][]byte{
		"repo_prepared": []byte("prepared frozen source"),
	})
	seedCompletedAuthoringRecoveryStage(t, ctx, fixture, workflowkit.NodeID(workflowadapter.RepoAnalyze), map[string][]byte{
		"repo_analysis": []byte(`{"paths":["tower-http/src/lib.rs"]}`),
	})
	seedCompletedAuthoringRecoveryStage(t, ctx, fixture, workflowkit.NodeID(workflowadapter.TaskDesign), map[string][]byte{
		"task_proposal": []byte(`{"feature":"add a bounded backend feature"}`),
	})

	subject, err := fixture.services.core.resolveWorkflowRunSubject(ctx, fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	stage, found := fixture.workflow.Stage(workflowkit.NodeID(workflowadapter.TaskReview))
	if !found {
		t.Fatal("frozen authoring workflow omits task_review")
	}
	inputs, err := resolveStageInputsForSubject(ctx, fixture.store, fixture.services.core.objects, fixture.run, subject, stage)
	if err != nil {
		t.Fatal(err)
	}
	inputFingerprint, err := workflowkit.FingerprintArtifactBindings(inputs)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := fixture.store.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: fixture.run.ID, StageKey: string(stage.Key), StageGroup: stage.Group, Ordinal: 1,
		InputFingerprint: string(inputFingerprint), BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "worker", Reason: "open task review repair fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	encodedInputs, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := fixture.store.OpenAuthoringReviewGate(ctx, store.OpenAuthoringReviewGateRequest{
		IdempotencyKey: "open-task-review-repair:" + attempt.ID,
		RunID:          fixture.run.ID, AuthoringSessionID: fixture.session.ID, AuthoringSourceID: fixture.source.ID,
		SourceSnapshotDigest: fixture.source.SnapshotContentDigest, ExpectedRunVersion: fixture.run.Version,
		DefinitionHash: fixture.run.DefinitionHash, StageAttemptID: attempt.ID, ExpectedStageAttemptVersion: attempt.Version,
		StageKey: string(stage.Key), ReviewKind: string(workflowadapter.ReviewTaskDirection), NodeGeneration: 0, NodeAttemptOrdinal: 1,
		InputBindingsJSON: string(encodedInputs), InputFingerprint: string(inputFingerprint),
		EvidenceManifestDigest: string(workflowkit.SHA256Fingerprint(encodedInputs)), Actor: "worker", Reason: "open frozen task direction review",
	})
	if err != nil {
		t.Fatal(err)
	}
	sourcePayload, err := json.Marshal(frozenStageExecutionPayload{
		Format: frozenStageExecutionPayloadFormat, RunID: fixture.run.ID, StageAttemptID: attempt.ID,
		StageKey: stage.Key, DefinitionHash: fixture.run.DefinitionHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "stage_attempt.execute", EntityType: "stage_attempt", EntityID: attempt.ID,
		RunID: fixture.run.ID, StageAttemptID: attempt.ID, PayloadJSON: string(sourcePayload),
		IdempotencyKey: "task-review-repair-source-job:" + attempt.ID, Actor: "worker", Reason: "bind task review source stage job",
	}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := fixture.services.AuthoringReviews.CaptureCheckpoint(ctx, opened.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	reason := "Correct the misspelled tower-http path and re-check every cited command."
	decision, err := fixture.services.AuthoringReviews.Decide(ctx, DecideAuthoringReviewRequest{
		IdempotencyKey: authoringRecoveryUUID(t), Action: store.ReviewDecisionRequestChanges,
		Actor: "operator", Reason: reason, Expected: checkpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &FrozenExecutionRuntime{core: fixture.services.core, services: fixture.services}
	state, err := runtime.handleAuthoringReviewGateResolution(ctx, DurableJobExecution{}, decision.ResolutionJob)
	if err != nil || state != store.JobSucceeded {
		t.Fatalf("resolve task review request_changes = %s, %v", state, err)
	}
	updated, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || updated == nil || updated.Status != store.WorkflowRunWaitingContinuation {
		t.Fatalf("task review repair Run = %+v, %v", updated, err)
	}
	fixture.run = *updated
	completed, err := fixture.store.GetStageAttempt(ctx, attempt.ID)
	if err != nil || completed == nil || completed.ExecutionStatus != store.StageExecutionCompleted || completed.Verdict != store.VerdictNeedsRepair {
		t.Fatalf("task review repair attempt = %+v, %v", completed, err)
	}
	references, err := fixture.store.ListArtifactRefs(ctx, completed.ArtifactManifestID)
	if err != nil || len(references) != 1 || references[0].ArtifactKey != "task_review_decision" {
		t.Fatalf("task review repair artifacts = %+v, %v", references, err)
	}
	return fixture, references[0], reason
}

func seedCompletedAuthoringRecoveryStage(t *testing.T, ctx context.Context, fixture authoringRecoveryFixture, stageID workflowkit.NodeID, contents map[string][]byte) {
	t.Helper()
	stage, found := fixture.workflow.Stage(stageID)
	if !found {
		t.Fatalf("frozen authoring workflow omits %q", stageID)
	}
	subject, err := fixture.services.core.resolveWorkflowRunSubject(ctx, fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := resolveStageInputsForSubject(ctx, fixture.store, fixture.services.core.objects, fixture.run, subject, stage)
	if err != nil {
		t.Fatal(err)
	}
	inputFingerprint, err := workflowkit.FingerprintArtifactBindings(inputs)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := fixture.store.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: fixture.run.ID, StageKey: string(stage.Key), StageGroup: stage.Group, Ordinal: 1,
		InputFingerprint: string(inputFingerprint), BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "worker", Reason: "seed completed authoring stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionRunning,
		Actor: "worker", Reason: "run completed authoring stage fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := fixture.store.CreateNodeAttempt(ctx, store.CreateNodeAttemptRequest{
		StageAttemptID: attempt.ID, NodeID: string(stage.Key), Generation: 0, Attempt: 1,
		IdempotencyKey: authoringRecoveryUUID(t), Actor: "worker", Reason: "seed completed authoring node",
	})
	if err != nil {
		t.Fatal(err)
	}
	artifacts := make([]StageArtifact, 0, len(stage.Outputs))
	for _, output := range stage.Outputs {
		content, present := contents[output.Name]
		if !present {
			t.Fatalf("stage %q fixture omits output %q", stage.Key, output.Name)
		}
		artifacts = append(artifacts, StageArtifact{
			ID: authoringRecoveryUUID(t), Key: output.Name, SchemaVersion: output.SchemaVersion,
			Content: append([]byte(nil), content...), TurnOrdinal: 1,
		})
	}
	manifest, _, err := persistStageArtifactsForSubject(ctx, fixture.services.core, fixture.run, subject, attempt, node, stage, inputs, artifacts, "worker", "persist completed authoring stage fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionCompleted,
		Verdict: store.VerdictPass, ArtifactManifestID: manifest.ID, Actor: "worker", Reason: "complete authoring stage fixture",
	}); err != nil {
		t.Fatal(err)
	}
}

func newAuthoringRecoveryFixture(t *testing.T, failure workflowkit.FailureClass) authoringRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	fixture := newAuthoringRecoveryLaunchFixture(t)
	return failAuthoringRecoveryLaunchFixture(t, ctx, fixture, failure)
}

func newQuotaExhaustedAuthoringRecoveryFixture(t *testing.T, ctx context.Context) (authoringRecoveryFixture, store.QuotaAccount) {
	t.Helper()
	fixture := newAuthoringRecoveryLaunchFixture(t)
	fixture.run = transitionAuthoringRecoveryRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunRunning)
	frozen, err := decodeFrozenRunDefinition(fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	stage, found := frozen.Workflow.Stage(workflowkit.NodeID(workflowadapter.DockerfileBuildValidate))
	if !found {
		t.Fatal("frozen authoring workflow has no dockerfile_build_validate")
	}
	admission, err := BuildFrozenStageQuotaAdmission(frozen.QuotaPolicy, stage)
	if err != nil {
		t.Fatal(err)
	}
	var outputClaim int64
	for _, claim := range admission.Claims {
		if claim.Dimension == "output_submission" {
			outputClaim = claim.Units
		}
	}
	if outputClaim <= 0 {
		t.Fatalf("dockerfile_build_validate claims = %+v, want output_submission", admission.Claims)
	}
	for _, bootstrap := range admission.BootstrapAccounts {
		for _, scope := range []struct {
			kind  store.QuotaScopeKind
			id    string
			limit int64
		}{
			{kind: store.QuotaScopeTask, id: fixture.task.ID, limit: bootstrap.TaskLimitUnits},
			{kind: store.QuotaScopeActor, id: "author", limit: bootstrap.ActorLimitUnits},
		} {
			if _, err := fixture.store.CreateQuotaAccount(ctx, store.CreateQuotaAccountRequest{
				ScopeKind: scope.kind, ScopeID: scope.id, Dimension: bootstrap.Dimension, LimitUnits: scope.limit,
				Actor: "author", Reason: "initialize frozen quota admission fixture",
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	outputAccount, err := fixture.store.GetQuotaAccountForScope(ctx, store.QuotaScopeTask, fixture.task.ID, "output_submission")
	if err != nil || outputAccount == nil {
		t.Fatalf("load output_submission account = %+v, %v", outputAccount, err)
	}
	reserveUnits := outputAccount.LimitUnits - outputClaim + 1
	if reserveUnits <= 0 {
		t.Fatalf("output_submission account limit %d cannot force claim %d exhaustion", outputAccount.LimitUnits, outputClaim)
	}
	if _, err := fixture.store.ReserveQuota(ctx, store.QuotaLeaseRequest{
		IdempotencyKey: authoringRecoveryUUID(t), Owner: "quota-fixture", ScopeKind: store.QuotaScopeTask, ScopeID: fixture.task.ID,
		Dimension: "output_submission", Units: reserveUnits, ReclaimPolicy: store.QuotaReclaimNever, TTL: time.Hour,
		Actor: "author", Reason: "exhaust output submission capacity before Docker validation",
	}); err != nil {
		t.Fatal(err)
	}
	inputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := fixture.store.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: fixture.run.ID, StageKey: string(stage.Key), StageGroup: stage.Group, Ordinal: 1, InputFingerprint: string(inputs),
		BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "author", Reason: "create quota-rejected Docker validation fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err = fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: failed.ID, ExpectedVersion: failed.Version, ExecutionStatus: store.StageExecutionRunning,
		Actor: "author", Reason: "start quota-rejected Docker validation fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := fixture.store.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "workflow_stage.execute", EntityType: "stage_attempt", EntityID: failed.ID, RunID: fixture.run.ID, StageAttemptID: failed.ID,
		PayloadJSON: `{}`, IdempotencyKey: authoringRecoveryUUID(t), Actor: "author", Reason: "create quota-rejected Docker validation job",
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := fixture.store.AdmitTaskActorQuota(ctx, store.AdmitTaskActorQuotaRequest{
		IdempotencyKey: "stage-admission:" + job.ID, TaskID: fixture.task.ID, Actor: job.CreatedBy, LeaseOwner: "quota-fixture", LeaseTTL: time.Hour,
		Policy: admission.Policy, BootstrapAccounts: admission.BootstrapAccounts, Claims: admission.Claims,
		Reason: "reject Docker validation after frozen quota admission",
	})
	if err != nil || decision.Accepted || decision.Reason != store.AdmissionReasonQuotaExhausted {
		t.Fatalf("quota admission decision = %+v, %v", decision, err)
	}
	failed, err = fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: failed.ID, ExpectedVersion: failed.Version, ExecutionStatus: store.StageExecutionInfraFailed,
		FailureClass: string(workflowkit.FailurePolicy), ErrorText: "frozen stage quota admission rejected: quota_exhausted",
		Actor: "author", Reason: "project quota-rejected Docker validation",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.run = transitionAuthoringRecoveryRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunFailedRecoverable)
	outputAccount, err = fixture.store.GetQuotaAccountForScope(ctx, store.QuotaScopeTask, fixture.task.ID, "output_submission")
	if err != nil || outputAccount == nil {
		t.Fatalf("reload exhausted output_submission account = %+v, %v", outputAccount, err)
	}
	fixture.failed = failed
	return fixture, *outputAccount
}

func newUnrelatedPolicyAuthoringRecoveryFixture(t *testing.T, ctx context.Context) authoringRecoveryFixture {
	t.Helper()
	fixture := newAuthoringRecoveryLaunchFixture(t)
	fixture.run = transitionAuthoringRecoveryRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunRunning)
	stage, found := fixture.workflow.Stage(workflowkit.NodeID(workflowadapter.DockerfileBuildValidate))
	if !found {
		t.Fatal("frozen authoring workflow has no dockerfile_build_validate")
	}
	inputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := fixture.store.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: fixture.run.ID, StageKey: string(stage.Key), StageGroup: stage.Group, Ordinal: 1, InputFingerprint: string(inputs),
		BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "author", Reason: "create unrelated policy failure fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err = fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: failed.ID, ExpectedVersion: failed.Version, ExecutionStatus: store.StageExecutionRunning,
		Actor: "author", Reason: "start unrelated policy failure fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err = fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: failed.ID, ExpectedVersion: failed.Version, ExecutionStatus: store.StageExecutionInfraFailed,
		FailureClass: string(workflowkit.FailurePolicy), ErrorText: "unrelated immutable policy failure",
		Actor: "author", Reason: "project unrelated policy failure fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.run = transitionAuthoringRecoveryRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunFailedRecoverable)
	fixture.failed = failed
	return fixture
}

func newLockedAuthoringRecoveryFixture(t *testing.T, failure workflowkit.FailureClass) authoringRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	fixture := newLockedAuthoringRecoveryLaunchFixture(t)
	return failAuthoringRecoveryLaunchFixture(t, ctx, fixture, failure)
}

func failAuthoringRecoveryLaunchFixture(t *testing.T, ctx context.Context, fixture authoringRecoveryFixture, failure workflowkit.FailureClass) authoringRecoveryFixture {
	t.Helper()
	runValue := transitionAuthoringRecoveryRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunRunning)
	stage, found := fixture.workflow.Stage(workflowkit.NodeID(workflowadapter.RepoAnalyze))
	if !found {
		t.Fatal("frozen authoring workflow has no repo_analyze")
	}
	if failure != workflowkit.FailurePermanent && !stage.Retry.Allows(failure) {
		t.Fatalf("fixture repo_analyze retry policy does not allow %s", failure)
	}
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := fixture.store.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: runValue.ID, StageKey: string(stage.Key), StageGroup: stage.Group, Ordinal: 1, InputFingerprint: string(emptyInputs),
		BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "worker", Reason: "create failed authoring stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err = fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: failed.ID, ExpectedVersion: failed.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "worker", Reason: "run failed authoring stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err = fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: failed.ID, ExpectedVersion: failed.Version, ExecutionStatus: store.StageExecutionInfraFailed,
		FailureClass: string(failure), ErrorText: "simulated authoring provider failure", Actor: "worker", Reason: "fail authoring stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.run = transitionAuthoringRecoveryRun(t, ctx, fixture.store, runValue, store.WorkflowRunFailedRecoverable)
	fixture.failed = failed
	return fixture
}

func newPausedAuthoringRecoveryFixture(t *testing.T) authoringRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	fixture := newAuthoringRecoveryLaunchFixture(t)
	run := transitionAuthoringRecoveryRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunRunning)
	run = transitionAuthoringRecoveryRun(t, ctx, fixture.store, run, store.WorkflowRunPauseRequested)
	run = transitionAuthoringRecoveryRun(t, ctx, fixture.store, run, store.WorkflowRunPausing)
	fixture.run = transitionAuthoringRecoveryRun(t, ctx, fixture.store, run, store.WorkflowRunPaused)
	return fixture
}

func newAuthoringRecoveryLaunchFixture(t *testing.T) authoringRecoveryFixture {
	t.Helper()
	ctx := context.Background()
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
	return startAuthoringRecoveryLaunchFixture(t, ctx, root, database, services, definitions.catalog)
}

func newLockedAuthoringRecoveryLaunchFixture(t *testing.T) authoringRecoveryFixture {
	t.Helper()
	ctx := context.Background()
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
	resolver := &authoringRecoveryCatalogLockResolver{
		DeploymentOperationCatalogResolver: definitions.catalog,
		identity:                           authoringRecoveryCatalogLockIdentity("v1"),
	}
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver: resolver,
		DeploymentCatalogResolvers: []TemplateDeploymentCatalogResolver{{
			Template: workflowadapter.StandardAuthoringCurrentTemplateReference(), Resolver: resolver,
		}},
		RequireDeploymentCatalog:               true,
		RequireDeploymentLock:                  true,
		StandardAuthoringSourceCapturer:        capturer,
		StandardAuthoringRunDefinitionProvider: definitions,
	})
	if err != nil {
		t.Fatal(err)
	}
	return startAuthoringRecoveryLaunchFixture(t, ctx, root, database, services, definitions.catalog)
}

func startAuthoringRecoveryLaunchFixture(t *testing.T, ctx context.Context, root string, database *store.Store, services *LifecycleServices, catalog *stageprovider.DeploymentOperationCatalogResolver) authoringRecoveryFixture {
	t.Helper()
	receipt, err := services.AuthoringLaunches.Start(ctx, StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: authoringRecoveryUUID(t), Actor: "author", Reason: "create authoring recovery fixture"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL, CommitSHA: standardAuthoringLaunchTestCoordinate.CommitSHA,
		BaseImage: standardAuthoringLaunchTestBaseImage,
		TaskType:  standardAuthoringLaunchTestTaskType, Application: standardAuthoringLaunchTestApplication, Objective: standardAuthoringLaunchTestObjective,
		Slug: "authoring-recovery-fixture", Title: "Authoring recovery fixture", MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.GetWorkflowRun(ctx, receipt.RunID)
	if err != nil || run == nil {
		t.Fatalf("load authoring fixture Run = %+v, %v", run, err)
	}
	workflow, err := decodeFrozenWorkflow(*run)
	if err != nil {
		t.Fatal(err)
	}
	source, err := database.GetAuthoringSource(ctx, receipt.AuthoringSourceID)
	if err != nil || source == nil {
		t.Fatalf("load authoring fixture source = %+v, %v", source, err)
	}
	session, err := database.GetAuthoringSession(ctx, receipt.AuthoringSessionID)
	if err != nil || session == nil {
		t.Fatalf("load authoring fixture session = %+v, %v", session, err)
	}
	task, err := database.GetTaskV2(ctx, receipt.TaskID)
	if err != nil || task == nil {
		t.Fatalf("load authoring fixture task = %+v, %v", task, err)
	}
	return authoringRecoveryFixture{root: root, services: services, store: database, catalog: catalog, source: *source, session: *session, task: *task, run: *run, workflow: workflow}
}

type authoringRecoveryCatalogLockResolver struct {
	*stageprovider.DeploymentOperationCatalogResolver
	identity stageprovider.DeploymentOperationCatalogLockIdentity
}

func (resolver *authoringRecoveryCatalogLockResolver) LockIdentity() stageprovider.DeploymentOperationCatalogLockIdentity {
	return resolver.identity
}

func (resolver *authoringRecoveryCatalogLockResolver) VerifyLockIdentity(identity stageprovider.DeploymentOperationCatalogLockIdentity) error {
	if identity != resolver.identity {
		return stageprovider.ErrDeploymentOperationCatalogLockDrift
	}
	return nil
}

func authoringRecoveryCatalogLockIdentity(version string) stageprovider.DeploymentOperationCatalogLockIdentity {
	return stageprovider.DeploymentOperationCatalogLockIdentity{
		LockID: "authoring-recovery-test-lock", LockVersion: version,
		Fingerprint: workflowkit.SHA256Fingerprint([]byte("authoring-recovery-test-lock:" + version)),
	}
}

func seedAuthoringRecoveryRepoPrepare(t *testing.T, ctx context.Context, fixture authoringRecoveryFixture) {
	t.Helper()
	stage, found := fixture.workflow.Stage(workflowkit.NodeID(workflowadapter.RepoPrepare))
	if !found || len(stage.Outputs) != 1 {
		t.Fatalf("repo_prepare descriptor = %+v", stage)
	}
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := fixture.store.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: fixture.run.ID, StageKey: string(stage.Key), StageGroup: stage.Group, Ordinal: 1, InputFingerprint: string(emptyInputs),
		BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "worker", Reason: "seed reusable authoring source stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "worker", Reason: "run reusable authoring source stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := fixture.store.CreateNodeAttempt(ctx, store.CreateNodeAttemptRequest{
		StageAttemptID: attempt.ID, NodeID: string(stage.Key), Generation: 0, Attempt: 1, IdempotencyKey: authoringRecoveryUUID(t), Actor: "worker", Reason: "seed authoring source node",
	})
	if err != nil {
		t.Fatal(err)
	}
	subject, err := fixture.services.core.resolveWorkflowRunSubject(ctx, fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := persistStageArtifactsForSubject(ctx, fixture.services.core, fixture.run, subject, attempt, node, stage, nil, []StageArtifact{{
		ID: authoringRecoveryUUID(t), Key: stage.Outputs[0].Name, SchemaVersion: stage.Outputs[0].SchemaVersion, Content: []byte("verified authoring repo preparation"), TurnOrdinal: 1,
	}}, "worker", "persist reusable authoring source artifact")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictPass,
		ArtifactManifestID: manifest.ID, Actor: "worker", Reason: "complete reusable authoring source stage",
	}); err != nil {
		t.Fatal(err)
	}
}

func assertAuthoringRecoveryPlan(t *testing.T, plan workflowkit.ContinuationPlan, fixture authoringRecoveryFixture) {
	t.Helper()
	snapshot := plan.Snapshot()
	if snapshot.Strategy != workflowkit.StrategyRetryAttempt || snapshot.SourceRunID != fixture.run.ID ||
		snapshot.SubjectRevisionID != fixture.session.ID || snapshot.SubjectDigest != workflowkit.SubjectDigest(fixture.source.SnapshotContentDigest) ||
		snapshot.BaseCheckpoint.SubjectID != fixture.source.ID || snapshot.BaseCheckpoint.SubjectVersion != store.AuthoringSessionControlSubjectVersion {
		t.Fatalf("authoring recovery plan binding = %+v", snapshot)
	}
	transitions := make(map[workflowkit.NodeID]workflowkit.NodeTransition, len(snapshot.Nodes))
	for _, transition := range snapshot.Nodes {
		transitions[transition.NodeID] = transition
	}
	for _, nodeID := range []workflowkit.NodeID{workflowkit.NodeID(workflowadapter.RepoAnalyze), workflowkit.NodeID(workflowadapter.TaskDesign)} {
		transition, found := transitions[nodeID]
		if !found || transition.Disposition != workflowkit.DispositionSchedule {
			t.Fatalf("authoring recovery transition %s = %+v, found=%t", nodeID, transition, found)
		}
	}
}

func transitionAuthoringRecoveryRun(t *testing.T, ctx context.Context, dataStore *store.Store, run store.WorkflowRun, target store.WorkflowRunStatus) store.WorkflowRun {
	t.Helper()
	if run.Status == target {
		return run
	}
	updated, err := dataStore.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: target, Actor: "worker", Reason: "authoring recovery fixture transition",
	})
	if err != nil {
		t.Fatalf("transition authoring fixture Run %s from %s to %s: %v", run.ID, run.Status, target, err)
	}
	return updated
}

type authoringRecoverySubjectObserverFunc func(context.Context, store.WorkflowRun, workflowRunSubject, workflowkit.WorkflowDescriptor) (continuationRunState, error)

func (observer authoringRecoverySubjectObserverFunc) ObserveSubject(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, workflow workflowkit.WorkflowDescriptor) (continuationRunState, error) {
	return observer(ctx, run, subject, workflow)
}

func snapshotAuthoringRecoveryManagedFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || path == filepath.Join(root, "harbor.db-wal") || path == filepath.Join(root, "harbor.db-shm") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = content
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func authoringRecoveryUUID(t *testing.T) string {
	t.Helper()
	id, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
