package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAuthoringReviewGateOpenDecisionCompletionAndReplay(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	source, session, run, stage := createAuthoringReviewGateFixture(t, ctx, s)
	if _, err := s.db.Exec(`
		INSERT INTO authoring_review_requests_v22 (
			id, run_id, authoring_session_id, authoring_source_id,
			source_snapshot_digest, definition_hash, evidence_manifest_digest,
			request_fingerprint, idempotency_key, created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, 'sha256:forged-evidence', 'sha256:forged-request', 'forged-authoring-review', 'forged', CURRENT_TIMESTAMP)
	`, mustUUIDv7(t), run.ID, session.ID, source.ID, authoringDigest("b"), run.DefinitionHash); err == nil || !strings.Contains(err.Error(), "authoring review request does not match source/session run lineage") {
		t.Fatalf("raw mismatched source/session review request err=%v, want lineage rejection", err)
	}
	openRequest := authoringReviewGateOpenRequest(run, session, source, stage)
	opened, err := s.OpenAuthoringReviewGate(ctx, openRequest)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Request.RunID != run.ID || opened.Request.AuthoringSessionID != session.ID || opened.Request.AuthoringSourceID != source.ID ||
		opened.Request.SourceSnapshotDigest != source.SnapshotContentDigest || opened.Binding.StageAttemptID != stage.ID ||
		opened.Binding.NodeAttemptID != opened.NodeAttempt.ID || opened.Binding.InputFingerprint != stage.InputFingerprint ||
		opened.Run.Status != WorkflowRunWaitingReview || opened.StageAttempt.ExecutionStatus != StageExecutionWaiting || opened.NodeAttempt.Status != NodeAttemptWaiting {
		t.Fatalf("authoring review gate did not persist an atomic source/session wait: %+v", opened)
	}
	if generic, err := s.GetReviewRequest(ctx, opened.Request.ID); err != nil || generic != nil {
		t.Fatalf("authoring review request leaked into task-review API: generic=%+v err=%v", generic, err)
	}
	byRequest, err := s.GetAuthoringReviewGateBindingByRequest(ctx, opened.Request.ID)
	if err != nil || byRequest == nil || byRequest.ID != opened.Binding.ID {
		t.Fatalf("authoring gate binding by request = %+v err=%v", byRequest, err)
	}
	byStage, err := s.GetAuthoringReviewGateBindingByStageAttempt(ctx, stage.ID)
	if err != nil || byStage == nil || byStage.ID != opened.Binding.ID {
		t.Fatalf("authoring gate binding by stage = %+v err=%v", byStage, err)
	}
	byNode, err := s.GetAuthoringReviewGateBindingByNodeAttempt(ctx, opened.NodeAttempt.ID)
	if err != nil || byNode == nil || byNode.ID != opened.Binding.ID {
		t.Fatalf("authoring gate binding by node = %+v err=%v", byNode, err)
	}
	if decisions, err := s.ListAuthoringReviewDecisionsForRequest(ctx, opened.Request.ID); err != nil || len(decisions) != 0 {
		t.Fatalf("new gate decisions=%+v err=%v, want empty", decisions, err)
	}
	if state, err := s.GetAuthoringReviewGateState(ctx, opened.Request.ID); err != nil || state != AuthoringReviewGateOpen {
		t.Fatalf("opened authoring review state=%q err=%v", state, err)
	}
	replayedOpen, err := s.OpenAuthoringReviewGate(ctx, openRequest)
	if err != nil || replayedOpen.Request.ID != opened.Request.ID || replayedOpen.Binding.ID != opened.Binding.ID || replayedOpen.NodeAttempt.ID != opened.NodeAttempt.ID || replayedOpen.Run.Version != opened.Run.Version {
		t.Fatalf("authoring gate open replay=%+v err=%v", replayedOpen, err)
	}

	decisionRequest := authoringReviewGateDecisionRequest(opened)
	decided, err := s.DecideAuthoringReviewGate(ctx, decisionRequest)
	if err != nil {
		t.Fatal(err)
	}
	if decided.Decision.Action != ReviewDecisionApprove || decided.Decision.ReviewRequestID != opened.Request.ID || decided.Decision.BindingID != opened.Binding.ID ||
		decided.ResolutionJob.CommandType != AuthoringReviewGateResolutionCommandType || decided.ResolutionJob.EntityID != opened.Binding.ID ||
		decided.ResolutionJob.RunID != opened.Run.ID || decided.ResolutionJob.StageAttemptID != opened.StageAttempt.ID ||
		decided.ResolutionJob.IdempotencyKey != AuthoringReviewGateResolutionJobKey(opened.Binding.ID, decided.Decision.ID) {
		t.Fatalf("authoring gate decision/job lineage is invalid: %+v", decided)
	}
	decisions, err := s.ListAuthoringReviewDecisionsForRequest(ctx, opened.Request.ID)
	if err != nil || len(decisions) != 1 || decisions[0].ID != decided.Decision.ID {
		t.Fatalf("authoring gate decision history=%+v err=%v", decisions, err)
	}
	if state, err := s.GetAuthoringReviewGateState(ctx, opened.Request.ID); err != nil || state != AuthoringReviewGateDecided {
		t.Fatalf("decided authoring review state=%q err=%v", state, err)
	}
	replayedDecision, err := s.DecideAuthoringReviewGate(ctx, decisionRequest)
	if err != nil || replayedDecision.Decision.ID != decided.Decision.ID || replayedDecision.ResolutionJob.ID != decided.ResolutionJob.ID {
		t.Fatalf("authoring gate decision replay=%+v err=%v", replayedDecision, err)
	}

	completeRequest := authoringReviewGateCompletionRequest(opened, decided)
	manifest := createAuthoringReviewGateResolutionManifest(t, ctx, s, opened.Binding)
	completeRequest.ArtifactManifestID = manifest.ID
	completed, err := s.CompleteAuthoringReviewGateResolution(ctx, completeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Resolution.DecisionID != decided.Decision.ID || completed.Resolution.Verdict != VerdictPass ||
		completed.Resolution.ArtifactManifestID != manifest.ID || completed.StageAttempt.ArtifactManifestID != manifest.ID ||
		completed.StageAttempt.ExecutionStatus != StageExecutionCompleted || completed.StageAttempt.Verdict != VerdictPass ||
		completed.NodeAttempt.Status != NodeAttemptCompleted || completed.Run.Status != WorkflowRunWaitingReview {
		t.Fatalf("authoring gate completion did not preserve terminal projection: %+v", completed)
	}
	replayedCompletion, err := s.CompleteAuthoringReviewGateResolution(ctx, completeRequest)
	if err != nil || replayedCompletion.Resolution.ID != completed.Resolution.ID || replayedCompletion.StageAttempt.Version != completed.StageAttempt.Version {
		t.Fatalf("authoring gate completion replay=%+v err=%v", replayedCompletion, err)
	}
	resolution, err := s.GetAuthoringReviewGateResolution(ctx, opened.Request.ID)
	if err != nil || resolution == nil || resolution.ID != completed.Resolution.ID {
		t.Fatalf("authoring gate resolution lookup=%+v err=%v", resolution, err)
	}
	if state, err := s.GetAuthoringReviewGateState(ctx, opened.Request.ID); err != nil || state != AuthoringReviewGateCompleted {
		t.Fatalf("completed authoring review state=%q err=%v", state, err)
	}

	for _, statement := range []struct {
		query string
		id    string
	}{
		{`UPDATE authoring_review_requests_v22 SET definition_hash = 'forged' WHERE id = ?`, opened.Request.ID},
		{`DELETE FROM authoring_review_gate_bindings_v22 WHERE id = ?`, opened.Binding.ID},
		{`UPDATE authoring_review_decisions_v22 SET action = 'reject_terminal' WHERE id = ?`, decided.Decision.ID},
		{`DELETE FROM authoring_review_gate_resolutions_v22 WHERE id = ?`, completed.Resolution.ID},
	} {
		if _, err := s.db.Exec(statement.query, statement.id); err == nil {
			t.Fatalf("raw immutable authoring review statement succeeded: %s", statement.query)
		}
	}
}

func TestAuthoringReviewGateRejectsStaleAndMismatchedSourceSessionRequests(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	source, session, run, stage := createAuthoringReviewGateFixture(t, ctx, s)
	openRequest := authoringReviewGateOpenRequest(run, session, source, stage)
	opened, err := s.OpenAuthoringReviewGate(ctx, openRequest)
	if err != nil {
		t.Fatal(err)
	}
	wrongOpen := openRequest
	wrongOpen.ReviewKind = "different_gate"
	if _, err := s.OpenAuthoringReviewGate(ctx, wrongOpen); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed authoring open replay err=%v, want idempotency conflict", err)
	}
	decisionRequest := authoringReviewGateDecisionRequest(opened)
	staleDecision := decisionRequest
	staleDecision.ExpectedRunVersion--
	if _, err := s.DecideAuthoringReviewGate(ctx, staleDecision); !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("stale authoring decision err=%v, want optimistic lock", err)
	}
	wrongDecision := decisionRequest
	wrongDecision.SourceSnapshotDigest = authoringDigest("b")
	if _, err := s.DecideAuthoringReviewGate(ctx, wrongDecision); !errors.Is(err, ErrImmutable) {
		t.Fatalf("mismatched authoring source decision err=%v, want immutable binding error", err)
	}
	decided, err := s.DecideAuthoringReviewGate(ctx, decisionRequest)
	if err != nil {
		t.Fatal(err)
	}
	completeRequest := authoringReviewGateCompletionRequest(opened, decided)
	staleComplete := completeRequest
	staleComplete.ExpectedNodeAttemptVersion++
	if _, err := s.CompleteAuthoringReviewGateResolution(ctx, staleComplete); !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("stale authoring completion err=%v, want optimistic lock", err)
	}
	wrongManifest, err := s.CreateArtifactManifest(ctx, CreateArtifactManifestRequest{
		SubjectRevisionID: mustUUIDv7(t), SubjectDigest: authoringDigest("f"), WorkflowFingerprint: opened.Binding.DefinitionHash,
		ManifestJSON: `{}`, ManifestFingerprint: authoringDigest("f"), IdempotencyKey: "wrong-authoring-review-manifest", Actor: "worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	wrongManifestComplete := completeRequest
	wrongManifestComplete.ArtifactManifestID = wrongManifest.ID
	if _, err := s.CompleteAuthoringReviewGateResolution(ctx, wrongManifestComplete); !errors.Is(err, ErrImmutable) {
		t.Fatalf("mismatched authoring review manifest err=%v, want immutable lineage failure", err)
	}
	completeRequest.ArtifactManifestID = createAuthoringReviewGateResolutionManifest(t, ctx, s, opened.Binding).ID
	completed, err := s.CompleteAuthoringReviewGateResolution(ctx, completeRequest)
	if err != nil {
		t.Fatal(err)
	}
	changedComplete := completeRequest
	changedComplete.ResolutionEvidenceDigest = "sha256:" + strings.Repeat("c", 64)
	if _, err := s.CompleteAuthoringReviewGateResolution(ctx, changedComplete); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed authoring completion replay err=%v, want idempotency conflict", err)
	}
	if completed.Request.AuthoringSessionID != session.ID || completed.Binding.AuthoringSourceID != source.ID || completed.Run.TaskID != "" || completed.Run.RevisionID != "" {
		t.Fatalf("authoring completion drifted into a task revision subject: %+v", completed)
	}
}

func createAuthoringReviewGateFixture(t *testing.T, ctx context.Context, s *Store) (AuthoringSource, AuthoringSession, WorkflowRun, StageAttempt) {
	t.Helper()
	source := createAuthoringSourceFixture(t, ctx, s, "source-authoring-review-"+t.Name())
	task, err := s.CreateTaskV2(ctx, CreateTaskV2Request{
		Slug: "authoring-review-" + strings.ToLower(t.Name()), Title: "Authoring review draft",
		SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA, Actor: "author",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.CreateAuthoringSession(ctx, CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID,
		WorkflowTemplateID: "harbor.standard-authoring", WorkflowTemplateVersion: "1.2.0",
		SessionManifestJSON: `{"mode":"standard"}`, IdempotencyKey: "session-authoring-review-" + t.Name(), Actor: "author",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateAuthoringWorkflowRun(ctx, CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID, WorkflowTemplateID: session.WorkflowTemplateID, WorkflowTemplateVersion: session.WorkflowTemplateVersion,
		ResolvedProfileHash: "sha256:authoring-review-profile", DefinitionHash: "sha256:authoring-review-definition",
		RunManifestJSON: `{}`, Trigger: "task.generate", Actor: "author",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = s.TransitionWorkflowRun(ctx, TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: WorkflowRunRunning, Actor: "author",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err := s.CreateStageAttempt(ctx, CreateStageAttemptRequest{
		RunID: run.ID, StageKey: "task_review", StageGroup: "authoring", Ordinal: 1,
		InputFingerprint: "sha256:authoring-review-input", BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "author",
	})
	if err != nil {
		t.Fatal(err)
	}
	return source, session, run, stage
}

func authoringReviewGateOpenRequest(run WorkflowRun, session AuthoringSession, source AuthoringSource, stage StageAttempt) OpenAuthoringReviewGateRequest {
	return OpenAuthoringReviewGateRequest{
		IdempotencyKey: "open-authoring-review-" + stage.ID,
		RunID:          run.ID, AuthoringSessionID: session.ID, AuthoringSourceID: source.ID, SourceSnapshotDigest: source.SnapshotContentDigest,
		ExpectedRunVersion: run.Version, DefinitionHash: run.DefinitionHash,
		StageAttemptID: stage.ID, ExpectedStageAttemptVersion: stage.Version, StageKey: stage.StageKey, ReviewKind: "task_direction",
		NodeGeneration: 0, NodeAttemptOrdinal: 1, InputBindingsJSON: `{"ports":["repo_analysis"]}`,
		InputFingerprint: stage.InputFingerprint, EvidenceManifestDigest: "sha256:authoring-review-evidence", Actor: "reviewer", Reason: "request operator review",
	}
}

func authoringReviewGateDecisionRequest(opened AuthoringReviewGateOpenResult) DecideAuthoringReviewGateRequest {
	return DecideAuthoringReviewGateRequest{
		IdempotencyKey:  "decide-authoring-review-" + opened.Request.ID,
		ReviewRequestID: opened.Request.ID, BindingID: opened.Binding.ID, RunID: opened.Run.ID,
		AuthoringSessionID: opened.Request.AuthoringSessionID, AuthoringSourceID: opened.Request.AuthoringSourceID,
		SourceSnapshotDigest: opened.Request.SourceSnapshotDigest, DefinitionHash: opened.Request.DefinitionHash,
		StageAttemptID: opened.StageAttempt.ID, InputFingerprint: opened.Binding.InputFingerprint,
		EvidenceManifestDigest: opened.Binding.EvidenceManifestDigest, ExpectedRunVersion: opened.Run.Version,
		ExpectedStageAttemptVersion: opened.StageAttempt.Version, Action: ReviewDecisionApprove,
		ResolutionPayloadJSON: `{"format":"authoring-review-resolution.v1"}`, ResolutionPriority: 4, Actor: "reviewer", Reason: "approved",
	}
}

func authoringReviewGateCompletionRequest(opened AuthoringReviewGateOpenResult, decided AuthoringReviewGateDecisionResult) CompleteAuthoringReviewGateResolutionRequest {
	return CompleteAuthoringReviewGateResolutionRequest{
		IdempotencyKey:  "complete-authoring-review-" + opened.Request.ID,
		ReviewRequestID: opened.Request.ID, BindingID: opened.Binding.ID, DecisionID: decided.Decision.ID, RunID: opened.Run.ID,
		AuthoringSessionID: opened.Request.AuthoringSessionID, AuthoringSourceID: opened.Request.AuthoringSourceID,
		SourceSnapshotDigest: opened.Request.SourceSnapshotDigest, DefinitionHash: opened.Request.DefinitionHash,
		StageAttemptID: opened.StageAttempt.ID, NodeAttemptID: opened.NodeAttempt.ID, InputFingerprint: opened.Binding.InputFingerprint,
		EvidenceManifestDigest: opened.Binding.EvidenceManifestDigest, ExpectedRunVersion: opened.Run.Version,
		ExpectedStageAttemptVersion: opened.StageAttempt.Version, ExpectedNodeAttemptVersion: opened.NodeAttempt.Version,
		ResolutionEvidenceDigest: "sha256:" + strings.Repeat("a", 64), ResolutionPayloadJSON: `{"format":"authoring-review-resolution.v1"}`,
		Actor: "worker", Reason: "project operator decision",
	}
}

func createAuthoringReviewGateResolutionManifest(t *testing.T, ctx context.Context, s *Store, binding AuthoringReviewGateBinding) ArtifactManifest {
	t.Helper()
	manifest, err := s.CreateArtifactManifest(ctx, CreateArtifactManifestRequest{
		SubjectRevisionID: binding.AuthoringSessionID, SubjectDigest: binding.SourceSnapshotDigest, WorkflowFingerprint: binding.DefinitionHash,
		ManifestJSON: `{"format":"authoring-review-gate-manifest.v1"}`, ManifestFingerprint: authoringDigest("d"),
		IdempotencyKey: "authoring-review-gate-manifest-" + binding.ID, Actor: "worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateArtifactRef(ctx, CreateArtifactRefRequest{
		ManifestID: manifest.ID, ArtifactKey: "review_decision", ContentDigest: authoringDigest("e"), SchemaVersion: "harbor.authoring-review-decision.v1",
		RunID: binding.RunID, StageKey: binding.StageKey, AttemptID: binding.StageAttemptID, TurnOrdinal: 0,
		SubjectRevisionID: binding.AuthoringSessionID, SubjectDigest: binding.SourceSnapshotDigest, WorkflowFingerprint: binding.DefinitionHash,
		InputBindingsJSON: binding.InputBindingsJSON, InputFingerprint: binding.InputFingerprint, ProducerVersion: "test",
		IdempotencyKey: "authoring-review-gate-artifact-" + binding.ID, Actor: "worker",
	}); err != nil {
		t.Fatal(err)
	}
	return manifest
}
