package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAgentTurnTranscriptAtomicallyCompletesCheckpointAndRejectsCoordinateConflict(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return now }
	fixture := createAgentTurnTranscriptFixture(t, s)

	request := agentTurnTranscriptCheckpointRequest(fixture.node.ID, 1, now)
	persisted, err := s.RecordAgentTurnTranscriptWithCheckpoint(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Replayed || persisted.Checkpoint.Status != TurnCheckpointCompleted || persisted.Checkpoint.Version != 2 || persisted.Checkpoint.FinishedAt == nil {
		t.Fatalf("atomic record = %+v", persisted)
	}
	if persisted.Transcript.ResponseSHA256 == "" || persisted.Transcript.ResponseBytes != int64(len("model prose")) || persisted.Transcript.ExpiresAt.Sub(now) != AgentTurnTranscriptRetention {
		t.Fatalf("transcript audit facts = %+v", persisted.Transcript)
	}
	submissions, err := s.ListAgentTurnTranscriptSubmissions(ctx, persisted.Transcript.ID)
	if err != nil || len(submissions) != 2 {
		t.Fatalf("submissions = %+v, %v", submissions, err)
	}
	if submissions[0].RawRequestJSON != `{"artifacts":` || submissions[0].ValidationJSON != `{"accepted":false}` || submissions[0].RejectionCode != "structured_output_invalid" {
		t.Fatalf("raw rejected submission was not preserved: %+v", submissions[0])
	}
	checkpoints, err := s.ListTurnCheckpoints(ctx, fixture.node.ID)
	if err != nil || len(checkpoints) != 1 || checkpoints[0].ID != persisted.Checkpoint.ID || checkpoints[0].Status != TurnCheckpointCompleted {
		t.Fatalf("checkpoints = %+v, %v", checkpoints, err)
	}
	if _, err := s.CreateTurnCheckpoint(ctx, CreateTurnCheckpointRequest{
		NodeAttemptID: fixture.node.ID, Turn: 2, Substep: agentTurnTranscriptCompletedSubstep, InputDigest: "transcript-input", PayloadJSON: `{}`, Actor: "tester",
	}); err == nil || !strings.Contains(err.Error(), "must be recorded with their transcript") {
		t.Fatalf("bare completed Agent turn checkpoint error = %v", err)
	}

	replayed, err := s.RecordAgentTurnTranscriptWithCheckpoint(ctx, request)
	if err != nil || !replayed.Replayed || replayed.Transcript.ID != persisted.Transcript.ID || replayed.Checkpoint.ID != persisted.Checkpoint.ID {
		t.Fatalf("atomic record replay = %+v, %v", replayed, err)
	}
	conflict := request
	conflict.Transcript.ResponseText = "different prose"
	if _, err := s.RecordAgentTurnTranscriptWithCheckpoint(ctx, conflict); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("different transcript at same coordinate error = %v, want %v", err, ErrIdentityCollision)
	}
	if _, err := s.db.Exec(`UPDATE agent_turn_transcripts SET response_text = 'forged' WHERE id = ?`, persisted.Transcript.ID); err == nil {
		t.Fatal("direct raw transcript mutation was accepted")
	}
	events, err := s.ListAuditEvents(ctx, ListAuditEventsRequest{EntityType: "agent_turn_transcript", EntityID: persisted.Transcript.ID})
	if err != nil || len(events) != 1 || events[0].Action != "agent_turn_transcript.recorded" || strings.Contains(events[0].PayloadJSON, "model prose") {
		t.Fatalf("transcript audit = %+v, %v", events, err)
	}
}

func TestAgentTurnTranscriptRetentionUsesCASAndRejectsActiveWorkerAndLegalHold(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return now }
	fixture := createAgentTurnTranscriptFixture(t, s)
	recorded, err := s.RecordAgentTurnTranscriptWithCheckpoint(ctx, agentTurnTranscriptCheckpointRequest(fixture.node.ID, 1, now))
	if err != nil {
		t.Fatal(err)
	}
	transcript := recorded.Transcript
	now = now.Add(AgentTurnTranscriptRetention)
	if _, err := s.ExpireAgentTurnTranscript(ctx, ExpireAgentTurnTranscriptRequest{TranscriptID: transcript.ID, ExpectedVersion: transcript.Version + 1, Actor: "retention", Reason: "expired"}); !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("stale expiration error = %v, want %v", err, ErrOptimisticLock)
	}
	blocked, err := s.ExpireAgentTurnTranscript(ctx, ExpireAgentTurnTranscriptRequest{TranscriptID: transcript.ID, ExpectedVersion: transcript.Version, Actor: "retention", Reason: "expired"})
	if !errors.Is(err, ErrTranscriptRetentionBlocked) || blocked.Block != AgentTurnTranscriptExpiryBlockedActiveAttempt {
		t.Fatalf("active attempt expiration = %+v, %v", blocked, err)
	}
	completeAgentTurnTranscriptFixture(t, s, fixture)

	lease, err := s.AcquireLease(ctx, AcquireLeaseRequest{ResourceType: RunWorkerSupervisorLeaseResourceType, ResourceID: fixture.run.ID, Owner: "worker", TTL: time.Hour, Actor: "worker", Reason: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	blocked, err = s.ExpireAgentTurnTranscript(ctx, ExpireAgentTurnTranscriptRequest{TranscriptID: transcript.ID, ExpectedVersion: transcript.Version, Actor: "retention", Reason: "expired"})
	if !errors.Is(err, ErrTranscriptRetentionBlocked) || blocked.Block != AgentTurnTranscriptExpiryBlockedActiveWorker {
		t.Fatalf("active worker expiration = %+v, %v", blocked, err)
	}
	if _, err := s.ReleaseLease(ctx, ReleaseLeaseRequest{LeaseID: lease.ID, Owner: lease.Owner, FencingToken: lease.FencingToken, ExpectedVersion: lease.Version, Actor: "worker", Reason: "fixture complete"}); err != nil {
		t.Fatal(err)
	}

	hold, err := s.CreateAgentTurnTranscriptLegalHold(ctx, CreateAgentTurnTranscriptLegalHoldRequest{TranscriptID: transcript.ID, HoldKey: "court-order-1", Actor: "legal", Reason: "legal retention"})
	if err != nil {
		t.Fatal(err)
	}
	blocked, err = s.ExpireAgentTurnTranscript(ctx, ExpireAgentTurnTranscriptRequest{TranscriptID: transcript.ID, ExpectedVersion: transcript.Version, Actor: "retention", Reason: "expired"})
	if !errors.Is(err, ErrTranscriptRetentionBlocked) || blocked.Block != AgentTurnTranscriptExpiryBlockedLegalHold {
		t.Fatalf("legal hold expiration = %+v, %v", blocked, err)
	}
	if _, err := s.ReleaseAgentTurnTranscriptLegalHold(ctx, ReleaseAgentTurnTranscriptLegalHoldRequest{HoldID: hold.ID, ExpectedVersion: hold.Version, Actor: "legal", Reason: "hold released"}); err != nil {
		t.Fatal(err)
	}

	expired, err := s.ExpireAgentTurnTranscript(ctx, ExpireAgentTurnTranscriptRequest{TranscriptID: transcript.ID, ExpectedVersion: transcript.Version, Actor: "retention", Reason: "expired"})
	if err != nil || !expired.Expired || expired.Transcript.ExpiredAt == nil || expired.Transcript.ResponseText != "" || expired.Transcript.ResponseSHA256 != transcript.ResponseSHA256 || expired.Transcript.ResponseBytes != transcript.ResponseBytes || expired.Transcript.Version != transcript.Version+1 {
		t.Fatalf("expiration = %+v, %v", expired, err)
	}
	submissions, err := s.ListAgentTurnTranscriptSubmissions(ctx, transcript.ID)
	if err != nil || len(submissions) != 2 || submissions[0].RawRequestJSON != "" || submissions[0].ValidationJSON != "" || submissions[0].ReceiptJSON != "" || submissions[0].RejectionCode != "structured_output_invalid" || submissions[0].ExpiredAt == nil {
		t.Fatalf("expired submissions = %+v, %v", submissions, err)
	}
	replay, err := s.ExpireAgentTurnTranscript(ctx, ExpireAgentTurnTranscriptRequest{TranscriptID: transcript.ID, ExpectedVersion: transcript.Version, Actor: "retention", Reason: "expired"})
	if err != nil || !replay.Replayed || replay.Transcript.ID != transcript.ID {
		t.Fatalf("expiration replay = %+v, %v", replay, err)
	}

	second := createAgentTurnTranscriptFixture(t, s)
	if _, err := s.RecordAgentTurnTranscriptWithCheckpoint(ctx, agentTurnTranscriptCheckpointRequest(second.node.ID, 1, now)); err != nil {
		t.Fatal(err)
	}
	completeAgentTurnTranscriptFixture(t, s, second)
	now = now.Add(AgentTurnTranscriptRetention)
	swept, err := s.SweepExpiredAgentTurnTranscripts(ctx, SweepExpiredAgentTurnTranscriptsRequest{Limit: 10, Actor: "retention", Reason: "sweep"})
	if err != nil || len(swept.Expired) != 1 || len(swept.Blocked) != 0 || !swept.Expired[0].Expired {
		t.Fatalf("retention sweep = %+v, %v", swept, err)
	}
	events, err := s.ListAuditEvents(ctx, ListAuditEventsRequest{EntityType: "agent_turn_transcript", EntityID: transcript.ID})
	if err != nil {
		t.Fatal(err)
	}
	actions := make(map[string]bool, len(events))
	for _, event := range events {
		actions[event.Action] = true
	}
	if !actions["agent_turn_transcript.expiry_blocked"] || !actions["agent_turn_transcript.expired"] {
		t.Fatalf("retention audit actions = %+v", actions)
	}
}

func TestAgentTurnTranscriptSweepOrdersLimitsAndRetainsBlockedRecords(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	now := time.Date(2026, time.July, 28, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	firstFixture := createAgentTurnTranscriptFixture(t, s)
	first := recordAgentTurnTranscriptFixture(t, s, firstFixture, now.Add(-AgentTurnTranscriptRetention-3*time.Hour))
	completeAgentTurnTranscriptFixture(t, s, firstFixture)
	secondFixture := createAgentTurnTranscriptFixture(t, s)
	second := recordAgentTurnTranscriptFixture(t, s, secondFixture, now.Add(-AgentTurnTranscriptRetention-2*time.Hour))
	completeAgentTurnTranscriptFixture(t, s, secondFixture)
	thirdFixture := createAgentTurnTranscriptFixture(t, s)
	third := recordAgentTurnTranscriptFixture(t, s, thirdFixture, now.Add(-AgentTurnTranscriptRetention-time.Hour))
	completeAgentTurnTranscriptFixture(t, s, thirdFixture)

	for _, want := range []AgentTurnTranscript{first, second, third} {
		swept, err := s.SweepExpiredAgentTurnTranscripts(ctx, SweepExpiredAgentTurnTranscriptsRequest{Limit: 1, Actor: "retention", Reason: "ordered sweep"})
		if err != nil || len(swept.Expired) != 1 || len(swept.Blocked) != 0 || swept.Expired[0].Transcript.ID != want.ID {
			t.Fatalf("limited ordered sweep want=%s got=%+v err=%v", want.ID, swept, err)
		}
	}
	replay, err := s.SweepExpiredAgentTurnTranscripts(ctx, SweepExpiredAgentTurnTranscriptsRequest{Limit: 10, Actor: "retention", Reason: "ordered sweep replay"})
	if err != nil || len(replay.Expired) != 0 || len(replay.Blocked) != 0 {
		t.Fatalf("completed sweep replay = %+v, %v", replay, err)
	}

	activeFixture := createAgentTurnTranscriptFixture(t, s)
	active := recordAgentTurnTranscriptFixture(t, s, activeFixture, now.Add(-AgentTurnTranscriptRetention-2*time.Hour))
	heldFixture := createAgentTurnTranscriptFixture(t, s)
	held := recordAgentTurnTranscriptFixture(t, s, heldFixture, now.Add(-AgentTurnTranscriptRetention-time.Hour))
	completeAgentTurnTranscriptFixture(t, s, heldFixture)
	if _, err := s.CreateAgentTurnTranscriptLegalHold(ctx, CreateAgentTurnTranscriptLegalHoldRequest{TranscriptID: held.ID, HoldKey: "sweep-legal-hold", Actor: "legal", Reason: "retention suspended"}); err != nil {
		t.Fatal(err)
	}

	blocked, err := s.SweepExpiredAgentTurnTranscripts(ctx, SweepExpiredAgentTurnTranscriptsRequest{Limit: 10, Actor: "retention", Reason: "blocked sweep"})
	if err != nil || len(blocked.Expired) != 0 || len(blocked.Blocked) != 2 {
		t.Fatalf("blocked sweep = %+v, %v", blocked, err)
	}
	blocks := map[string]AgentTurnTranscriptExpiryBlock{}
	for _, item := range blocked.Blocked {
		blocks[item.Transcript.ID] = item.Block
	}
	if blocks[active.ID] != AgentTurnTranscriptExpiryBlockedActiveAttempt || blocks[held.ID] != AgentTurnTranscriptExpiryBlockedLegalHold {
		t.Fatalf("sweep blocks = %+v", blocks)
	}
	for _, transcript := range []AgentTurnTranscript{active, held} {
		persisted, err := s.GetAgentTurnTranscript(ctx, transcript.ID)
		if err != nil || persisted == nil || persisted.ExpiredAt != nil || persisted.ResponseText != "model prose" {
			t.Fatalf("blocked transcript %s was changed: %+v err=%v", transcript.ID, persisted, err)
		}
		events, err := s.ListAuditEvents(ctx, ListAuditEventsRequest{EntityType: "agent_turn_transcript", EntityID: transcript.ID})
		if err != nil || !hasAgentTranscriptAuditAction(events, "agent_turn_transcript.expiry_blocked") {
			t.Fatalf("blocked transcript audit %s = %+v err=%v", transcript.ID, events, err)
		}
	}
}

type agentTurnTranscriptFixture struct {
	run   WorkflowRun
	stage StageAttempt
	node  NodeAttempt
}

func createAgentTurnTranscriptFixture(t *testing.T, s *Store) agentTurnTranscriptFixture {
	t.Helper()
	ctx := context.Background()
	task, revision := createValidatedTaskAndRevision(t, s)
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "harbor.standard", WorkflowTemplateVersion: "v1",
		ResolvedProfileHash: "transcript-profile", DefinitionHash: "transcript-definition", RunManifestJSON: `{}`,
		Trigger: "transcript-test", Actor: "tester", Reason: "create transcript fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err := s.CreateStageAttempt(ctx, CreateStageAttemptRequest{RunID: run.ID, StageKey: "research", StageGroup: "standard", Ordinal: 1, InputFingerprint: "transcript-input", Actor: "tester", Reason: "create transcript fixture"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := s.CreateNodeAttempt(ctx, CreateNodeAttemptRequest{StageAttemptID: stage.ID, NodeID: "agent", Generation: 0, Attempt: 1, IdempotencyKey: "transcript-" + stage.ID, Actor: "tester", Reason: "create transcript fixture"})
	if err != nil {
		t.Fatal(err)
	}
	return agentTurnTranscriptFixture{run: run, stage: stage, node: node}
}

func completeAgentTurnTranscriptFixture(t *testing.T, s *Store, fixture agentTurnTranscriptFixture) {
	t.Helper()
	ctx := context.Background()
	stage, err := s.TransitionStageAttempt(ctx, TransitionStageAttemptRequest{StageAttemptID: fixture.stage.ID, ExpectedVersion: fixture.stage.Version, ExecutionStatus: StageExecutionRunning, Actor: "tester", Reason: "start fixture"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := s.TransitionNodeAttempt(ctx, TransitionNodeAttemptRequest{NodeAttemptID: fixture.node.ID, ExpectedVersion: fixture.node.Version, Status: NodeAttemptRunning, Actor: "tester", Reason: "start fixture"})
	if err != nil {
		t.Fatal(err)
	}
	node, err = s.TransitionNodeAttempt(ctx, TransitionNodeAttemptRequest{NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: NodeAttemptCompleted, Actor: "tester", Reason: "complete fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionStageAttempt(ctx, TransitionStageAttemptRequest{StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: StageExecutionCompleted, Verdict: VerdictAdvisory, Actor: "tester", Reason: "complete fixture"}); err != nil {
		t.Fatal(err)
	}
}

func agentTurnTranscriptRequest(nodeAttemptID string, turn int, occurredAt time.Time) CreateAgentTurnTranscriptRequest {
	return CreateAgentTurnTranscriptRequest{
		NodeAttemptID: nodeAttemptID, Turn: turn, ResponseText: "model prose", ModelID: "gpt-test",
		SubmissionStatus: AgentTurnSubmissionRejected, ProtocolRejectionCode: "structured_output_invalid", FailureCode: "missing_submission",
		OccurredAt: occurredAt, Actor: "agent", Reason: "record actual model turn",
		Submissions: []CreateAgentTurnTranscriptSubmissionRequest{
			{Ordinal: 1, Status: AgentTurnSubmissionRejected, RawRequestJSON: `{"artifacts":`, ValidationJSON: `{"accepted":false}`, ReceiptJSON: `{"error":"invalid"}`, RejectionCode: "structured_output_invalid"},
			{Ordinal: 2, Status: AgentTurnSubmissionRejected, RawRequestJSON: `{"artifacts":[]}`, ValidationJSON: `{"accepted":false}`, ReceiptJSON: `{"error":"empty"}`, RejectionCode: "structured_output_required"},
		},
	}
}

func recordAgentTurnTranscriptFixture(t *testing.T, s *Store, fixture agentTurnTranscriptFixture, occurredAt time.Time) AgentTurnTranscript {
	t.Helper()
	persisted, err := s.RecordAgentTurnTranscriptWithCheckpoint(context.Background(), agentTurnTranscriptCheckpointRequest(fixture.node.ID, 1, occurredAt))
	if err != nil {
		t.Fatal(err)
	}
	return persisted.Transcript
}

func hasAgentTranscriptAuditAction(events []AuditEvent, action string) bool {
	for _, event := range events {
		if event.Action == action {
			return true
		}
	}
	return false
}

func agentTurnTranscriptCheckpointRequest(nodeAttemptID string, turn int, occurredAt time.Time) RecordAgentTurnTranscriptWithCheckpointRequest {
	return RecordAgentTurnTranscriptWithCheckpointRequest{
		Transcript: agentTurnTranscriptRequest(nodeAttemptID, turn, occurredAt),
		Checkpoint: AgentTurnTranscriptCheckpoint{NodeAttemptID: nodeAttemptID, Turn: turn, Substep: "turn_completed", InputDigest: "transcript-input", PayloadJSON: `{"response_digest":"sha256:placeholder"}`},
		Actor:      "runtime",
		Reason:     "record completed agent turn",
	}
}
