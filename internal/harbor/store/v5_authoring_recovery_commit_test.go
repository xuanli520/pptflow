package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

type authoringRecoveryCommitFixture struct {
	store   *Store
	source  AuthoringSource
	session AuthoringSession
	task    TaskV2
	run     WorkflowRun
}

func TestCommitAuthoringRecoveryExecutionUsesDedicatedAtomicBarrier(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryCommitFixture(t, ctx)
	plan, checkpoint := createAuthoringRecoveryCommitPlan(t, ctx, fixture)
	request := authoringRecoveryCommitRequest(fixture, plan, checkpoint, "authoring-recovery-commit")

	if _, err := fixture.store.CommitContinuationExecution(ctx, request.CommitContinuationExecutionRequest); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("generic authoring continuation commit error = %v, want dedicated authoring rejection", err)
	}
	committed, err := fixture.store.CommitAuthoringRecoveryExecution(ctx, request)
	if err != nil {
		t.Fatalf("commit authoring recovery: %v", err)
	}
	replayed, err := fixture.store.CommitAuthoringRecoveryExecution(ctx, request)
	if err != nil || replayed.Execution.ID != committed.Execution.ID || replayed.Job.ID != committed.Job.ID {
		t.Fatalf("replayed authoring recovery commit = %+v, %v; first=%+v", replayed, err, committed)
	}

	updated, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || updated == nil || updated.ExecutionEpoch != fixture.run.ExecutionEpoch+1 || updated.Version != fixture.run.Version+1 {
		t.Fatalf("committed authoring recovery run = %+v, %v", updated, err)
	}
}

func TestCommitAuthoringRecoveryExecutionRejectsMaterializationAndInDoubt(t *testing.T) {
	ctx := context.Background()
	t.Run("materialized run", func(t *testing.T) {
		fixture := materializedAuthoringRecoveryCommitFixture(t, ctx)
		plan, checkpoint := createAuthoringRecoveryCommitPlan(t, ctx, fixture)
		request := authoringRecoveryCommitRequest(fixture, plan, checkpoint, "authoring-recovery-materialized")
		if _, err := fixture.store.CommitAuthoringRecoveryExecution(ctx, request); !errors.Is(err, ErrAuthoringRecoveryBarrier) {
			t.Fatalf("materialized authoring recovery commit error = %v, want barrier", err)
		}
	})
	t.Run("Phase-1 handoff appears after plan freeze", func(t *testing.T) {
		fixture := materializedAuthoringRecoveryCommitFixture(t, ctx)
		plan, checkpoint := createAuthoringRecoveryCommitPlan(t, ctx, fixture)
		handoff := prepareAuthoringRecoveryPhase1Handoff(t, ctx, fixture)
		if handoff.AuthoringRunID != fixture.run.ID {
			t.Fatalf("prepared Phase-1 handoff = %+v", handoff)
		}

		request := authoringRecoveryCommitRequest(fixture, plan, checkpoint, "authoring-recovery-handoff")
		if _, err := fixture.store.CommitAuthoringRecoveryExecution(ctx, request); !errors.Is(err, ErrAuthoringRecoveryBarrier) {
			t.Fatalf("handoff-blocked authoring recovery commit error = %v, want barrier", err)
		}
		if execution, err := fixture.store.GetContinuationExecutionByIdempotency(ctx, request.IdempotencyKey); err != nil || execution != nil {
			t.Fatalf("handoff-blocked recovery execution = %+v, %v", execution, err)
		}
		if jobs, err := fixture.store.ListDurableJobsForRun(ctx, fixture.run.ID); err != nil || len(jobs) != 0 {
			t.Fatalf("handoff-blocked recovery jobs = %+v, %v", jobs, err)
		}
	})
	t.Run("in doubt run", func(t *testing.T) {
		fixture := newAuthoringRecoveryCommitFixture(t, ctx)
		fixture.run = transitionAuthoringRecoveryCommitRun(t, ctx, fixture.store, fixture.run, WorkflowRunInDoubt)
		plan, checkpoint := createAuthoringRecoveryCommitPlan(t, ctx, fixture)
		request := authoringRecoveryCommitRequest(fixture, plan, checkpoint, "authoring-recovery-in-doubt")
		if _, err := fixture.store.CommitAuthoringRecoveryExecution(ctx, request); !errors.Is(err, ErrContinuationReconciliationRequired) {
			t.Fatalf("in_doubt authoring recovery commit error = %v, want reconciliation requirement", err)
		}
	})
}

func materializedAuthoringRecoveryCommitFixture(t *testing.T, ctx context.Context) authoringRecoveryCommitFixture {
	t.Helper()
	fixture := newAuthoringRecoveryCommitFixture(t, ctx)
	run := transitionAuthoringRecoveryCommitRun(t, ctx, fixture.store, fixture.run, WorkflowRunRunning)
	materialized, err := fixture.store.MaterializeAuthoringTask(ctx, MaterializeAuthoringTaskRequest{
		IdempotencyKey: "authoring-recovery-materialization", AuthoringSessionID: fixture.session.ID, AuthoringRunID: run.ID,
		ExpectedTaskVersion: fixture.task.Version, ExpectedRunVersion: run.Version,
		TaskDigest: "harbor.task.v2:sha256:" + strings.Repeat("e", 64), ProposalDigest: "proposal",
		ManifestID: "authoring-recovery-fixture-manifest", ChangeSummary: "materialize before retry", MetadataJSON: `{}`,
		Actor: "worker", Reason: "create materialization barrier",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.run = transitionAuthoringRecoveryCommitRun(t, ctx, fixture.store, run, WorkflowRunFailedRecoverable)
	fixture.task = materialized.Task
	return fixture
}

// prepareAuthoringRecoveryPhase1Handoff persists the minimum completed
// materialize_task lineage accepted by the Store's Phase-1 handoff trigger.
// The test needs the durable bridge to be written after plan freeze without
// mutating the source Run checkpoint.
func prepareAuthoringRecoveryPhase1Handoff(t *testing.T, ctx context.Context, fixture authoringRecoveryCommitFixture) AuthoringPhase1Handoff {
	t.Helper()
	inputFingerprint := string(workflowkit.SHA256Fingerprint([]byte("authoring recovery handoff inputs")))
	attempt, err := fixture.store.CreateStageAttempt(ctx, CreateStageAttemptRequest{
		RunID: fixture.run.ID, StageKey: "materialize_task", StageGroup: "task_generation", Ordinal: 1,
		InputFingerprint: inputFingerprint, BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "worker", Reason: "prepare Phase-1 handoff barrier",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = fixture.store.TransitionStageAttempt(ctx, TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: StageExecutionRunning,
		Actor: "worker", Reason: "complete materialize_task handoff fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := fixture.store.CreateArtifactManifest(ctx, CreateArtifactManifestRequest{
		SubjectRevisionID: fixture.session.ID, SubjectDigest: fixture.source.SnapshotContentDigest, WorkflowFingerprint: fixture.run.DefinitionHash,
		ManifestJSON:        `{"format":"authoring-recovery-phase1-handoff.v1"}`,
		ManifestFingerprint: string(workflowkit.SHA256Fingerprint([]byte("authoring recovery handoff manifest"))),
		IdempotencyKey:      "authoring-recovery-handoff-manifest-" + fixture.run.ID,
		Actor:               "worker", Reason: "persist Phase-1 handoff artifact",
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := fixture.store.CreateArtifactRef(ctx, CreateArtifactRefRequest{
		ManifestID: manifest.ID, ArtifactKey: "authoring_task_handoff", ContentDigest: string(workflowkit.SHA256Fingerprint([]byte("authoring recovery handoff artifact"))),
		SchemaVersion: "harbor.authoring-task-handoff.v1", RunID: fixture.run.ID, StageKey: "materialize_task", AttemptID: attempt.ID, TurnOrdinal: 0,
		SubjectRevisionID: fixture.session.ID, SubjectDigest: fixture.source.SnapshotContentDigest, WorkflowFingerprint: fixture.run.DefinitionHash,
		InputBindingsJSON: `[]`, InputFingerprint: inputFingerprint, ProducerVersion: "test",
		IdempotencyKey: "authoring-recovery-handoff-artifact-" + fixture.run.ID,
		Actor:          "worker", Reason: "persist Phase-1 handoff artifact",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = fixture.store.TransitionStageAttempt(ctx, TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: StageExecutionCompleted, Verdict: VerdictPass,
		ArtifactManifestID: manifest.ID, Actor: "worker", Reason: "complete materialize_task handoff fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := fixture.store.PrepareAuthoringPhase1Handoff(ctx, PrepareAuthoringPhase1HandoffRequest{
		AuthoringRunID: fixture.run.ID, AuthoringSessionID: fixture.session.ID, AuthoringSourceID: fixture.source.ID,
		HandoffArtifactID: artifact.ID, HandoffFingerprint: string(workflowkit.SHA256Fingerprint([]byte("authoring recovery Phase-1 handoff"))),
		TaskID: fixture.task.ID, RevisionID: fixture.task.CurrentRevisionID, TaskDigest: "harbor.task.v2:sha256:" + strings.Repeat("e", 64),
		ChildRunID: authoringRecoveryCommitID(t), IdempotencyKey: "authoring-recovery-phase1-handoff-" + fixture.run.ID,
		Actor: "worker", Reason: "write Phase-1 handoff after recovery plan freeze",
	})
	if err != nil {
		t.Fatal(err)
	}
	return handoff
}

func newAuthoringRecoveryCommitFixture(t *testing.T, ctx context.Context) authoringRecoveryCommitFixture {
	t.Helper()
	dataStore := tempDB(t)
	source := createAuthoringSourceFixture(t, ctx, dataStore, "authoring-recovery-commit-source")
	task, err := dataStore.CreateTaskV2(ctx, CreateTaskV2Request{
		Slug: "authoring-recovery-commit", Title: "Authoring recovery commit fixture", MetadataJSON: `{}`,
		SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA, Actor: "author", Reason: "reserve authoring draft",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := dataStore.CreateAuthoringSession(ctx, CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID, WorkflowTemplateID: "harbor.standard-authoring", WorkflowTemplateVersion: "1.0.0",
		SessionManifestJSON: `{}`, IdempotencyKey: "authoring-recovery-commit-session", Actor: "author", Reason: "freeze source session",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := dataStore.CreateAuthoringWorkflowRun(ctx, CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID, WorkflowTemplateID: session.WorkflowTemplateID, WorkflowTemplateVersion: session.WorkflowTemplateVersion,
		ResolvedProfileHash: "sha256:authoring-recovery-profile", DefinitionHash: "sha256:authoring-recovery-definition", RunManifestJSON: `{}`,
		Trigger: "authoring.recovery.fixture", Actor: "author", Reason: "start authoring recovery fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run = transitionAuthoringRecoveryCommitRun(t, ctx, dataStore, run, WorkflowRunRunning)
	run = transitionAuthoringRecoveryCommitRun(t, ctx, dataStore, run, WorkflowRunFailedRecoverable)
	return authoringRecoveryCommitFixture{store: dataStore, source: source, session: session, task: task, run: run}
}

func createAuthoringRecoveryCommitPlan(t *testing.T, ctx context.Context, fixture authoringRecoveryCommitFixture) (FrozenPlan, ControlCheckpointRef) {
	t.Helper()
	checkpoint := ControlCheckpointRef{
		Sequence: uint64(fixture.run.Version), ExecutionEpoch: fixture.run.ExecutionEpoch, SubjectVersion: AuthoringSessionControlSubjectVersion,
		SubjectID: fixture.source.ID, SubjectRevisionID: fixture.session.ID, SubjectDigest: fixture.source.SnapshotContentDigest,
		WorkflowFingerprint: fixture.run.DefinitionHash,
	}
	command, err := fixture.store.CreateContinuationCommand(ctx, CreateContinuationCommandRequest{
		CommandKey: "authoring-recovery-command-" + fixture.run.ID + "-" + authoringRecoveryCommitID(t), SubjectID: fixture.source.ID, RunID: fixture.run.ID,
		PayloadJSON: `{}`, Actor: "operator", Reason: "freeze authoring recovery command",
	})
	if err != nil {
		t.Fatal(err)
	}
	planID := authoringRecoveryCommitID(t)
	snapshot := workflowkit.ContinuationPlanSnapshot{
		PlanID: planID, CommandID: command.ID, Strategy: workflowkit.StrategyRetryAttempt,
		BaseCheckpoint: workflowkit.CheckpointRef{
			Sequence: checkpoint.Sequence, ExecutionEpoch: checkpoint.ExecutionEpoch, SubjectVersion: checkpoint.SubjectVersion,
			SubjectID: checkpoint.SubjectID, SubjectRevisionID: checkpoint.SubjectRevisionID, SubjectDigest: workflowkit.SubjectDigest(checkpoint.SubjectDigest), WorkflowFingerprint: workflowkit.Fingerprint(checkpoint.WorkflowFingerprint),
		},
		NextExecutionEpoch: fixture.run.ExecutionEpoch + 1, SourceRunID: fixture.run.ID, TargetRunRelation: workflowkit.RelationSameRunAttempt,
		SubjectRevisionID: fixture.session.ID, SubjectDigest: workflowkit.SubjectDigest(fixture.source.SnapshotContentDigest), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.store.CreateFrozenPlan(ctx, CreateFrozenPlanRequest{
		ID: planID, CommandID: command.ID, SubjectID: fixture.source.ID, SubjectRevisionID: fixture.session.ID,
		SubjectDigest: fixture.source.SnapshotContentDigest, WorkflowFingerprint: fixture.run.DefinitionHash,
		PlanFingerprint: string(workflowkit.SHA256Fingerprint([]byte(planID))), PayloadJSON: string(payload), ExpiresAt: snapshot.ExpiresAt,
		Actor: "operator", Reason: "freeze authoring recovery plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan, checkpoint
}

func authoringRecoveryCommitRequest(fixture authoringRecoveryCommitFixture, plan FrozenPlan, checkpoint ControlCheckpointRef, key string) CommitAuthoringRecoveryExecutionRequest {
	return CommitAuthoringRecoveryExecutionRequest{
		CommitContinuationExecutionRequest: CommitContinuationExecutionRequest{
			PlanID: plan.ID, RunID: fixture.run.ID, IdempotencyKey: key, PayloadJSON: `{}`, Expected: checkpoint,
			Actor: "operator", Reason: "commit authoring recovery",
		},
		AuthoringSourceID: fixture.source.ID, AuthoringSessionID: fixture.session.ID,
		TargetTaskID: fixture.task.ID, ExpectedTargetTaskVersion: fixture.task.Version,
	}
}

func transitionAuthoringRecoveryCommitRun(t *testing.T, ctx context.Context, dataStore *Store, run WorkflowRun, status WorkflowRunStatus) WorkflowRun {
	t.Helper()
	if run.Status == status {
		return run
	}
	updated, err := dataStore.TransitionWorkflowRun(ctx, TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: status, Actor: "worker", Reason: "authoring recovery commit fixture transition",
	})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func authoringRecoveryCommitID(t *testing.T) string {
	t.Helper()
	id, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
