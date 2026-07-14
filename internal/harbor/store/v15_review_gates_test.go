package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestReviewGateOpenDecisionResolutionIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	_, revision, run, stage := createReviewGateFixture(t, s)
	openRequest := reviewGateOpenRequest(run, revision, stage)

	opened, err := s.OpenReviewGate(ctx, openRequest)
	if err != nil {
		t.Fatalf("open review gate: %v", err)
	}
	if opened.Run.Status != WorkflowRunWaitingReview || opened.StageAttempt.ExecutionStatus != StageExecutionWaiting || opened.NodeAttempt.Status != NodeAttemptWaiting {
		t.Fatalf("gate open did not atomically project waiting state: %+v", opened)
	}
	if opened.Review.State != "open" || opened.Binding.ReviewRequestID != opened.Review.ID || opened.Binding.NodeAttemptID != opened.NodeAttempt.ID || opened.Binding.RevisionDigest != revision.TaskDigest {
		t.Fatalf("gate open durable binding = %+v", opened)
	}
	if opened.NodeAttempt.NodeID != stage.StageKey || opened.NodeAttempt.StartedAt == nil || opened.StageAttempt.StartedAt == nil {
		t.Fatalf("gate waiting attempts did not retain stage identity/start time: %+v", opened)
	}
	jobs, err := s.ListDurableJobsForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("opening a human gate created execution jobs: %+v", jobs)
	}
	replayedOpen, err := s.OpenReviewGate(ctx, openRequest)
	if err != nil {
		t.Fatalf("replay review gate open: %v", err)
	}
	if replayedOpen.Review.ID != opened.Review.ID || replayedOpen.NodeAttempt.ID != opened.NodeAttempt.ID || replayedOpen.StageAttempt.Version != opened.StageAttempt.Version {
		t.Fatalf("gate open replay created or changed durable facts: first=%+v replay=%+v", opened, replayedOpen)
	}
	if _, err := s.RecordReviewDecision(ctx, RecordReviewDecisionRequest{
		ReviewRequestID: opened.Review.ID, RevisionID: revision.ID, Action: ReviewDecisionApprove,
		ExpectedRevisionDigest: revision.TaskDigest, Actor: "reviewer", Reason: "must use gate protocol",
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("generic decision accepted a bound gate review: %v", err)
	}

	decisionRequest := RecordReviewGateDecisionRequest{
		ReviewRequestID: opened.Review.ID, RunID: opened.Run.ID, RevisionID: revision.ID, StageAttemptID: opened.StageAttempt.ID,
		ExpectedRevisionDigest: revision.TaskDigest, ExpectedRunVersion: opened.Run.Version, ExpectedStageAttemptVersion: opened.StageAttempt.Version,
		Action: ReviewDecisionApprove, ResolutionPayloadJSON: `{"format":"test.review-gate-resolution.v1"}`, ResolutionPriority: 7,
		Actor: "reviewer", Reason: "approve gate",
	}
	decided, err := s.RecordReviewGateDecision(ctx, decisionRequest)
	if err != nil {
		t.Fatalf("record gate decision: %v", err)
	}
	if decided.Decision.Action != ReviewDecisionApprove || decided.ResolutionJob.CommandType != ReviewGateResolutionCommandType || decided.ResolutionJob.EntityID != opened.StageAttempt.ID || decided.ResolutionJob.State != JobQueued {
		t.Fatalf("gate decision/job = %+v", decided)
	}
	if got, want := decided.ResolutionJob.IdempotencyKey, ReviewGateResolutionJobKey(opened.StageAttempt.ID, decided.Decision.ID); got != want {
		t.Fatalf("resolution job key = %q, want %q", got, want)
	}
	replayedDecision, err := s.RecordReviewGateDecision(ctx, decisionRequest)
	if err != nil {
		t.Fatalf("replay gate decision: %v", err)
	}
	if replayedDecision.Decision.ID != decided.Decision.ID || replayedDecision.ResolutionJob.ID != decided.ResolutionJob.ID {
		t.Fatalf("gate decision replay changed immutable facts: first=%+v replay=%+v", decided, replayedDecision)
	}
	jobs, err = s.ListDurableJobsForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != decided.ResolutionJob.ID {
		t.Fatalf("gate decision did not create exactly one resolution job: %+v", jobs)
	}

	manifest := createReviewGateResolutionManifest(t, s, opened.Binding, "task_review_decision")
	resolved, err := s.CompleteReviewGateResolution(ctx, CompleteReviewGateResolutionRequest{
		ReviewRequestID: opened.Review.ID, ReviewDecisionID: decided.Decision.ID, RunID: opened.Run.ID, StageAttemptID: opened.StageAttempt.ID,
		ExpectedRunVersion: opened.Run.Version, ExpectedStageAttemptVersion: opened.StageAttempt.Version, ExpectedNodeAttemptVersion: opened.NodeAttempt.Version,
		ArtifactManifestID: manifest.ID, Actor: "resolver", Reason: "materialized typed decision",
	})
	if err != nil {
		t.Fatalf("complete gate resolution: %v", err)
	}
	if resolved.Run.Status != WorkflowRunWaitingReview || resolved.StageAttempt.ExecutionStatus != StageExecutionCompleted || resolved.StageAttempt.Verdict != VerdictPass || resolved.StageAttempt.ArtifactManifestID != manifest.ID || resolved.NodeAttempt.Status != NodeAttemptCompleted {
		t.Fatalf("gate resolution projection = %+v", resolved)
	}
	replayedResolution, err := s.CompleteReviewGateResolution(ctx, CompleteReviewGateResolutionRequest{
		ReviewRequestID: opened.Review.ID, ReviewDecisionID: decided.Decision.ID, RunID: opened.Run.ID, StageAttemptID: opened.StageAttempt.ID,
		ExpectedRunVersion: opened.Run.Version, ExpectedStageAttemptVersion: opened.StageAttempt.Version, ExpectedNodeAttemptVersion: opened.NodeAttempt.Version,
		ArtifactManifestID: manifest.ID, Actor: "resolver", Reason: "materialized typed decision",
	})
	if err != nil {
		t.Fatalf("replay gate resolution: %v", err)
	}
	if replayedResolution.StageAttempt.Version != resolved.StageAttempt.Version || replayedResolution.NodeAttempt.Version != resolved.NodeAttempt.Version {
		t.Fatalf("gate resolution replay changed terminal facts: first=%+v replay=%+v", resolved, replayedResolution)
	}

	bindings, err := s.ListReviewGateBindingsForRevision(ctx, revision.ID)
	if err != nil || len(bindings) != 1 || bindings[0].StageAttemptID != stage.ID {
		t.Fatalf("list gate bindings = %+v, %v", bindings, err)
	}
	byReview, err := s.GetReviewGateBindingByReviewRequest(ctx, opened.Review.ID)
	if err != nil || byReview == nil || byReview.StageAttemptID != stage.ID {
		t.Fatalf("get binding by review = %+v, %v", byReview, err)
	}
}

func TestReviewGateDecisionRejectsStaleAndMismatchedRequests(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	_, revision, run, stage := createReviewGateFixture(t, s)
	opened, err := s.OpenReviewGate(ctx, reviewGateOpenRequest(run, revision, stage))
	if err != nil {
		t.Fatal(err)
	}
	request := RecordReviewGateDecisionRequest{
		ReviewRequestID: opened.Review.ID, RunID: opened.Run.ID, RevisionID: revision.ID, StageAttemptID: opened.StageAttempt.ID,
		ExpectedRevisionDigest: revision.TaskDigest, ExpectedRunVersion: opened.Run.Version - 1, ExpectedStageAttemptVersion: opened.StageAttempt.Version,
		Action: ReviewDecisionApprove, ResolutionPayloadJSON: `{}`, Actor: "reviewer",
	}
	if _, err := s.RecordReviewGateDecision(ctx, request); !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("stale gate decision = %v, want optimistic lock", err)
	}
	request.ExpectedRunVersion = opened.Run.Version
	request.ExpectedRevisionDigest = validTaskDigest("f")
	if _, err := s.RecordReviewGateDecision(ctx, request); !errors.Is(err, ErrImmutable) {
		t.Fatalf("mismatched frozen digest = %v, want immutable mismatch", err)
	}
	wrongOpen := reviewGateOpenRequest(run, revision, stage)
	wrongOpen.InputBindingsJSON = `["changed"]`
	if _, err := s.OpenReviewGate(ctx, wrongOpen); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("mismatched open replay = %v, want idempotency conflict", err)
	}
}

func TestReviewGateResolutionMapsEveryDecisionAndRejectsManifestMismatch(t *testing.T) {
	for _, test := range []struct {
		name    string
		action  ReviewDecisionAction
		verdict Verdict
	}{
		{name: "approve", action: ReviewDecisionApprove, verdict: VerdictPass},
		{name: "changes", action: ReviewDecisionRequestChanges, verdict: VerdictNeedsRepair},
		{name: "reject", action: ReviewDecisionRejectTerminal, verdict: VerdictReject},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			s := tempDB(t)
			_, revision, run, stage := createReviewGateFixture(t, s)
			opened, err := s.OpenReviewGate(ctx, reviewGateOpenRequest(run, revision, stage))
			if err != nil {
				t.Fatal(err)
			}
			decided, err := s.RecordReviewGateDecision(ctx, RecordReviewGateDecisionRequest{
				ReviewRequestID: opened.Review.ID, RunID: opened.Run.ID, RevisionID: revision.ID, StageAttemptID: opened.StageAttempt.ID,
				ExpectedRevisionDigest: revision.TaskDigest, ExpectedRunVersion: opened.Run.Version, ExpectedStageAttemptVersion: opened.StageAttempt.Version,
				Action: test.action, ResolutionPayloadJSON: `{}`, Actor: "reviewer",
			})
			if err != nil {
				t.Fatal(err)
			}
			wrongManifest := createMismatchedReviewGateResolutionManifest(t, s, opened.Binding, "wrong_"+test.name)
			if _, err := s.CompleteReviewGateResolution(ctx, CompleteReviewGateResolutionRequest{
				ReviewRequestID: opened.Review.ID, ReviewDecisionID: decided.Decision.ID, RunID: opened.Run.ID, StageAttemptID: opened.StageAttempt.ID,
				ExpectedRunVersion: opened.Run.Version, ExpectedStageAttemptVersion: opened.StageAttempt.Version, ExpectedNodeAttemptVersion: opened.NodeAttempt.Version,
				ArtifactManifestID: wrongManifest.ID, Actor: "resolver",
			}); !errors.Is(err, ErrImmutable) {
				t.Fatalf("mismatched manifest = %v, want immutable mismatch", err)
			}
			manifest := createReviewGateResolutionManifest(t, s, opened.Binding, "decision")
			resolved, err := s.CompleteReviewGateResolution(ctx, CompleteReviewGateResolutionRequest{
				ReviewRequestID: opened.Review.ID, ReviewDecisionID: decided.Decision.ID, RunID: opened.Run.ID, StageAttemptID: opened.StageAttempt.ID,
				ExpectedRunVersion: opened.Run.Version, ExpectedStageAttemptVersion: opened.StageAttempt.Version, ExpectedNodeAttemptVersion: opened.NodeAttempt.Version,
				ArtifactManifestID: manifest.ID, Actor: "resolver",
			})
			if err != nil {
				t.Fatal(err)
			}
			if resolved.StageAttempt.Verdict != test.verdict {
				t.Fatalf("action %q verdict = %q, want %q", test.action, resolved.StageAttempt.Verdict, test.verdict)
			}
		})
	}
}

func TestGateApprovalDoesNotPromoteRevisionButGenericReviewStillDoes(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, revision, run, stage := createReviewGateFixture(t, s)
	opened, err := s.OpenReviewGate(ctx, reviewGateOpenRequest(run, revision, stage))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordReviewGateDecision(ctx, RecordReviewGateDecisionRequest{
		ReviewRequestID: opened.Review.ID, RunID: opened.Run.ID, RevisionID: revision.ID, StageAttemptID: opened.StageAttempt.ID,
		ExpectedRevisionDigest: revision.TaskDigest, ExpectedRunVersion: opened.Run.Version, ExpectedStageAttemptVersion: opened.StageAttempt.Version,
		Action: ReviewDecisionApprove, ResolutionPayloadJSON: `{}`, Actor: "reviewer",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PromoteTaskCurrentRevision(ctx, PromoteCurrentRevisionRequest{TaskID: task.ID, RevisionID: revision.ID, ExpectedVersion: task.Version, Actor: "tester"}); !errors.Is(err, ErrReviewApprovalNeeded) {
		t.Fatalf("gate approval promoted current revision: %v", err)
	}
	generic, err := s.CreateReviewRequest(ctx, CreateReviewRequest{RevisionID: revision.ID, EvidenceManifestDigest: "generic-evidence", Actor: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordReviewDecision(ctx, RecordReviewDecisionRequest{
		ReviewRequestID: generic.ID, RevisionID: revision.ID, Action: ReviewDecisionApprove, ExpectedRevisionDigest: revision.TaskDigest, Actor: "reviewer",
	}); err != nil {
		t.Fatalf("ordinary review decision changed by gate support: %v", err)
	}
	promoted, err := s.PromoteTaskCurrentRevision(ctx, PromoteCurrentRevisionRequest{TaskID: task.ID, RevisionID: revision.ID, ExpectedVersion: task.Version, Actor: "tester"})
	if err != nil || promoted.CurrentRevisionID != revision.ID {
		t.Fatalf("ordinary approved review did not promote revision: %+v, %v", promoted, err)
	}
}

func createReviewGateFixture(t *testing.T, s *Store) (TaskV2, TaskRevision, WorkflowRun, StageAttempt) {
	t.Helper()
	ctx := context.Background()
	task, revision := createValidatedTaskAndRevision(t, s)
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "test.review-gate", WorkflowTemplateVersion: "v1",
		ResolvedProfileHash: "profile-review-gate", DefinitionHash: "definition-review-gate", RunManifestJSON: `{}`,
		Trigger: "test", Actor: "tester", Reason: "gate fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = s.TransitionWorkflowRun(ctx, TransitionWorkflowRunRequest{RunID: run.ID, ExpectedVersion: run.Version, Status: WorkflowRunRunning, Actor: "tester", Reason: "gate fixture"})
	if err != nil {
		t.Fatal(err)
	}
	stage, err := s.CreateStageAttempt(ctx, CreateStageAttemptRequest{
		RunID: run.ID, StageKey: "task_review", StageGroup: "review", Ordinal: 1, InputFingerprint: "input-review-gate",
		BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "tester", Reason: "gate fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return task, revision, run, stage
}

func reviewGateOpenRequest(run WorkflowRun, revision TaskRevision, stage StageAttempt) OpenReviewGateRequest {
	return OpenReviewGateRequest{
		RunID: run.ID, ExpectedRunVersion: run.Version, RevisionID: revision.ID, RevisionDigest: revision.TaskDigest,
		DefinitionHash: run.DefinitionHash, StageAttemptID: stage.ID, ExpectedStageAttemptVersion: stage.Version,
		StageKey: stage.StageKey, ReviewKind: "task_review", NodeGeneration: 0, NodeAttempt: 1,
		InputBindingsJSON: `[]`, InputFingerprint: stage.InputFingerprint, EvidenceManifestDigest: "review-evidence-task-review",
		Actor: "reviewer", Reason: "open durable review",
	}
}

func createReviewGateResolutionManifest(t *testing.T, s *Store, binding ReviewGateBinding, artifactKey string) ArtifactManifest {
	t.Helper()
	ctx := context.Background()
	manifest, err := s.CreateArtifactManifest(ctx, CreateArtifactManifestRequest{
		SubjectRevisionID: binding.RevisionID, SubjectDigest: binding.RevisionDigest, WorkflowFingerprint: binding.DefinitionHash,
		ManifestJSON: fmt.Sprintf(`{"artifact":%q}`, artifactKey), ManifestFingerprint: "manifest-" + artifactKey,
		IdempotencyKey: "review-gate-manifest-" + binding.StageAttemptID + "-" + artifactKey, Actor: "resolver",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateArtifactRef(ctx, CreateArtifactRefRequest{
		ManifestID: manifest.ID, ArtifactKey: artifactKey, ContentDigest: "sha256:" + artifactKey, SchemaVersion: "harbor.review-decision.v1",
		RunID: binding.RunID, StageKey: binding.StageKey, AttemptID: binding.StageAttemptID, TurnOrdinal: 0,
		SubjectRevisionID: binding.RevisionID, SubjectDigest: binding.RevisionDigest, WorkflowFingerprint: binding.DefinitionHash,
		InputBindingsJSON: binding.InputBindingsJSON, InputFingerprint: binding.InputFingerprint, ProducerVersion: "v1",
		IdempotencyKey: "review-gate-ref-" + binding.StageAttemptID + "-" + artifactKey, Actor: "resolver",
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func createMismatchedReviewGateResolutionManifest(t *testing.T, s *Store, binding ReviewGateBinding, artifactKey string) ArtifactManifest {
	t.Helper()
	ctx := context.Background()
	manifest, err := s.CreateArtifactManifest(ctx, CreateArtifactManifestRequest{
		SubjectRevisionID: binding.RevisionID, SubjectDigest: binding.RevisionDigest, WorkflowFingerprint: binding.DefinitionHash,
		ManifestJSON: fmt.Sprintf(`{"artifact":%q}`, artifactKey), ManifestFingerprint: "manifest-" + artifactKey,
		IdempotencyKey: "review-gate-mismatched-manifest-" + binding.StageAttemptID + "-" + artifactKey, Actor: "resolver",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateArtifactRef(ctx, CreateArtifactRefRequest{
		ManifestID: manifest.ID, ArtifactKey: artifactKey, ContentDigest: "sha256:" + artifactKey, SchemaVersion: "harbor.review-decision.v1",
		RunID: binding.RunID, StageKey: binding.StageKey, AttemptID: binding.StageAttemptID, TurnOrdinal: 0,
		SubjectRevisionID: binding.RevisionID, SubjectDigest: binding.RevisionDigest, WorkflowFingerprint: binding.DefinitionHash,
		InputBindingsJSON: binding.InputBindingsJSON, InputFingerprint: "mismatched-input-fingerprint", ProducerVersion: "v1",
		IdempotencyKey: "review-gate-mismatched-ref-" + binding.StageAttemptID + "-" + artifactKey, Actor: "resolver",
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}
