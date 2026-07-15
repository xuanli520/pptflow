package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

func TestAuthoringReviewServiceDecidesFrozenSourceSessionGateAndReplays(t *testing.T) {
	ctx := context.Background()
	services, database := newAuthoringReviewServiceFixture(t)
	defer database.Close()
	opened := openAuthoringReviewServiceGate(t, ctx, database)

	checkpoint, err := services.AuthoringReviews.CaptureCheckpoint(ctx, opened.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.ReviewRequestID != opened.Request.ID || checkpoint.BindingID != opened.Binding.ID ||
		checkpoint.RunID != opened.Run.ID || checkpoint.StageAttemptID != opened.StageAttempt.ID ||
		checkpoint.AuthoringSessionID != opened.Request.AuthoringSessionID || checkpoint.AuthoringSourceID != opened.Request.AuthoringSourceID ||
		checkpoint.RunVersion != opened.Run.Version || checkpoint.StageAttemptVersion != opened.StageAttempt.Version {
		t.Fatalf("captured authoring checkpoint drifted from gate: %+v", checkpoint)
	}
	idempotencyKey := newAuthoringReviewServiceUUID(t)
	decision, err := services.AuthoringReviews.Decide(ctx, DecideAuthoringReviewRequest{
		IdempotencyKey: idempotencyKey, Action: store.ReviewDecisionApprove, Actor: "operator", Reason: "approved frozen authoring proposal",
		Expected: checkpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Request.ID != opened.Request.ID || decision.Binding.ID != opened.Binding.ID ||
		decision.Decision.Action != store.ReviewDecisionApprove || decision.Decision.Actor != "operator" ||
		decision.ResolutionJob.CommandType != store.AuthoringReviewGateResolutionCommandType ||
		decision.ResolutionJob.RunID != opened.Run.ID || decision.ResolutionJob.StageAttemptID != opened.StageAttempt.ID {
		t.Fatalf("authoring review decision or durable resolution job lacks frozen lineage: %+v", decision)
	}
	var payload authoringReviewGateResolutionPayload
	if err := json.Unmarshal([]byte(decision.ResolutionJob.PayloadJSON), &payload); err != nil {
		t.Fatalf("decode authoring resolution payload: %v", err)
	}
	if payload.Format != authoringReviewGateResolutionPayloadFormat || payload.ReviewRequestID != opened.Request.ID ||
		payload.BindingID != opened.Binding.ID || payload.AuthoringSessionID != opened.Request.AuthoringSessionID ||
		payload.AuthoringSourceID != opened.Request.AuthoringSourceID || payload.SourceSnapshotDigest != opened.Request.SourceSnapshotDigest {
		t.Fatalf("authoring resolution payload does not bind the inspected source/session gate: %+v", payload)
	}
	if generic, err := database.GetReviewRequest(ctx, opened.Request.ID); err != nil || generic != nil {
		t.Fatalf("authoring review leaked into task revision review API: request=%+v err=%v", generic, err)
	}

	// A retry after the response is lost reads the now-decided gate, recreates
	// the stable payload, and returns the original immutable decision/job.
	replayCheckpoint, err := services.AuthoringReviews.CaptureCheckpoint(ctx, opened.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := services.AuthoringReviews.Decide(ctx, DecideAuthoringReviewRequest{
		IdempotencyKey: idempotencyKey, Action: store.ReviewDecisionApprove, Actor: "operator", Reason: "approved frozen authoring proposal",
		Expected: replayCheckpoint,
	})
	if err != nil {
		t.Fatalf("replay authoring review decision: %v", err)
	}
	if replayed.Decision.ID != decision.Decision.ID || replayed.ResolutionJob.ID != decision.ResolutionJob.ID {
		t.Fatalf("authoring decision replay returned a different durable outcome: first=%+v replay=%+v", decision, replayed)
	}
	if _, err := services.AuthoringReviews.Decide(ctx, DecideAuthoringReviewRequest{
		IdempotencyKey: idempotencyKey, Action: store.ReviewDecisionRejectTerminal, Actor: "operator", Reason: "approved frozen authoring proposal",
		Expected: replayCheckpoint,
	}); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("changed authoring review replay err=%v, want idempotency conflict", err)
	}
}

func TestAuthoringReviewServiceRejectsStaleOrIncompleteOperatorCommands(t *testing.T) {
	ctx := context.Background()
	services, database := newAuthoringReviewServiceFixture(t)
	defer database.Close()
	opened := openAuthoringReviewServiceGate(t, ctx, database)
	checkpoint, err := services.AuthoringReviews.CaptureCheckpoint(ctx, opened.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale := checkpoint
	stale.RunVersion--
	if _, err := services.AuthoringReviews.Decide(ctx, DecideAuthoringReviewRequest{
		IdempotencyKey: newAuthoringReviewServiceUUID(t), Action: store.ReviewDecisionApprove, Actor: "operator", Reason: "approve", Expected: stale,
	}); !errors.Is(err, store.ErrOptimisticLock) {
		t.Fatalf("stale authoring checkpoint err=%v, want optimistic lock", err)
	}
	for _, request := range []DecideAuthoringReviewRequest{
		{IdempotencyKey: "not-a-uuid", Action: store.ReviewDecisionApprove, Actor: "operator", Reason: "approve", Expected: checkpoint},
		{IdempotencyKey: newAuthoringReviewServiceUUID(t), Action: store.ReviewDecisionApprove, Actor: "", Reason: "approve", Expected: checkpoint},
		{IdempotencyKey: newAuthoringReviewServiceUUID(t), Action: store.ReviewDecisionAction("invalid"), Actor: "operator", Reason: "approve", Expected: checkpoint},
	} {
		if _, err := services.AuthoringReviews.Decide(ctx, request); err == nil {
			t.Fatalf("invalid authoring review command was accepted: %+v", request)
		}
	}
}

func newAuthoringReviewServiceFixture(t *testing.T) (*LifecycleServices, *store.Store) {
	t.Helper()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	services, err := newLifecycleServicesForTest(root, database)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	return services, database
}

func openAuthoringReviewServiceGate(t *testing.T, ctx context.Context, database *store.Store) store.AuthoringReviewGateOpenResult {
	t.Helper()
	digest := "sha256:" + strings.Repeat("a", 64)
	source, err := database.CreateAuthoringSource(ctx, store.CreateAuthoringSourceRequest{
		RepositoryURL: "https://github.com/tower-rs/tower-http.git", CommitSHA: "f066e10ebc07ea9050a2ce4576315abfa568edf4",
		SnapshotArtifactRef: digest, SnapshotContentDigest: digest, SnapshotSchemaVersion: "harbor.source-snapshot.v1",
		IdempotencyKey: "authoring-review-source-" + t.Name(), Actor: "author", Reason: "freeze source fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := database.CreateTaskV2(ctx, store.CreateTaskV2Request{
		Slug:  "authoring-review-service-" + strings.ToLower(strings.ReplaceAll(t.Name(), "_", "-")),
		Title: "Authoring review service fixture", SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA, Actor: "author", Reason: "reserve task ownership",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := database.CreateAuthoringSession(ctx, store.CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID, WorkflowTemplateID: "harbor.standard-authoring", WorkflowTemplateVersion: "1.0.0",
		SessionManifestJSON: `{"mode":"standard"}`, IdempotencyKey: "authoring-review-session-" + t.Name(), Actor: "author", Reason: "freeze authoring session",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.CreateAuthoringWorkflowRun(ctx, store.CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID, WorkflowTemplateID: session.WorkflowTemplateID, WorkflowTemplateVersion: session.WorkflowTemplateVersion,
		ResolvedProfileHash: "sha256:authoring-review-profile", DefinitionHash: "sha256:authoring-review-definition",
		RunManifestJSON: `{}`, Trigger: "task.generate", Actor: "author", Reason: "start authoring fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "author", Reason: "start authoring fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err := database.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: "task_review", StageGroup: "authoring", Ordinal: 1,
		InputFingerprint: "sha256:authoring-review-input", BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "author", Reason: "prepare review gate",
	})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := database.OpenAuthoringReviewGate(ctx, store.OpenAuthoringReviewGateRequest{
		IdempotencyKey: "open-authoring-review-" + stage.ID, RunID: run.ID, AuthoringSessionID: session.ID, AuthoringSourceID: source.ID,
		SourceSnapshotDigest: source.SnapshotContentDigest, ExpectedRunVersion: run.Version, DefinitionHash: run.DefinitionHash,
		StageAttemptID: stage.ID, ExpectedStageAttemptVersion: stage.Version, StageKey: stage.StageKey, ReviewKind: "task_direction",
		NodeGeneration: 0, NodeAttemptOrdinal: 1, InputBindingsJSON: `{"ports":["repo_analysis"]}`,
		InputFingerprint: stage.InputFingerprint, EvidenceManifestDigest: "sha256:authoring-review-evidence", Actor: "worker", Reason: "open source session review gate",
	})
	if err != nil {
		t.Fatal(err)
	}
	return opened
}

func newAuthoringReviewServiceUUID(t *testing.T) string {
	t.Helper()
	id, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
