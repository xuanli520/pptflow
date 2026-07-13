package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func tempV4DB(t *testing.T) *Store {
	t.Helper()
	s := tempDB(t)
	if _, err := s.db.Exec(migrationV4); err != nil {
		t.Fatalf("apply v4 schema: %v", err)
	}
	if _, err := s.db.Exec(migrationV4); err != nil {
		t.Fatalf("reapply v4 schema: %v", err)
	}
	return s
}

func TestV4ArtifactManifestAndRefsAreImmutableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s := tempV4DB(t)
	manifestRequest := CreateArtifactManifestRequest{
		SubjectRevisionID:   "revision-1",
		SubjectDigest:       "sha256:subject",
		WorkflowFingerprint: "sha256:workflow",
		ManifestJSON:        ` { "artifacts" : ["report"] } `,
		ManifestFingerprint: "sha256:manifest",
		IdempotencyKey:      "manifest-key-1",
		Actor:               "tester",
		Reason:              "persist fixture evidence",
	}
	manifest, err := s.CreateArtifactManifest(ctx, manifestRequest)
	if err != nil {
		t.Fatalf("create manifest: %v", err)
	}
	if !isUUIDv7(manifest.ID) || manifest.ManifestJSON != `{"artifacts":["report"]}` {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	replayed, err := s.CreateArtifactManifest(ctx, manifestRequest)
	if err != nil || replayed.ID != manifest.ID {
		t.Fatalf("manifest replay = %+v, err = %v", replayed, err)
	}
	conflict := manifestRequest
	conflict.ManifestJSON = `{"artifacts":["other"]}`
	if _, err := s.CreateArtifactManifest(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("manifest conflict err = %v, want ErrIdempotencyConflict", err)
	}
	if _, err := s.CreateArtifactManifest(ctx, CreateArtifactManifestRequest{
		SubjectRevisionID: "revision-2", SubjectDigest: "sha256:subject", WorkflowFingerprint: "sha256:workflow",
		ManifestJSON: `{not-json}`, ManifestFingerprint: "sha256:bad", IdempotencyKey: "manifest-bad", Actor: "tester",
	}); err == nil {
		t.Fatal("invalid manifest JSON was accepted")
	}

	referenceRequest := CreateArtifactRefRequest{
		ManifestID:          manifest.ID,
		ArtifactKey:         "quality-report",
		ContentDigest:       "sha256:artifact",
		SchemaVersion:       "report.v1",
		RunID:               "run-1",
		StageKey:            "quality",
		AttemptID:           "attempt-1",
		TurnOrdinal:         0,
		SubjectRevisionID:   manifest.SubjectRevisionID,
		SubjectDigest:       manifest.SubjectDigest,
		WorkflowFingerprint: manifest.WorkflowFingerprint,
		InputBindingsJSON:   `[]`,
		InputFingerprint:    "sha256:inputs",
		ProducerVersion:     "plugin.v1",
		IdempotencyKey:      "artifact-ref-key-1",
		Actor:               "tester",
		Reason:              "persist report ref",
	}
	reference, err := s.CreateArtifactRef(ctx, referenceRequest)
	if err != nil {
		t.Fatalf("create artifact ref: %v", err)
	}
	if !isUUIDv7(reference.ID) || reference.ManifestID != manifest.ID {
		t.Fatalf("unexpected artifact ref: %+v", reference)
	}
	replayedRef, err := s.CreateArtifactRef(ctx, referenceRequest)
	if err != nil || replayedRef.ID != reference.ID {
		t.Fatalf("artifact ref replay = %+v, err = %v", replayedRef, err)
	}
	if _, err := s.db.Exec(`UPDATE artifact_refs_v4 SET artifact_key = 'mutated' WHERE id = ?`, reference.ID); err == nil {
		t.Fatal("immutable artifact ref accepted direct update")
	}
	references, err := s.ListArtifactRefs(ctx, manifest.ID)
	if err != nil || len(references) != 1 || references[0].ID != reference.ID {
		t.Fatalf("artifact refs = %+v, err = %v", references, err)
	}
	events, err := s.ListAuditEvents(ctx, ListAuditEventsRequest{EntityID: manifest.ID})
	if err != nil || len(events) != 1 || events[0].Action != "artifact_manifest.created" {
		t.Fatalf("manifest audit = %+v, err = %v", events, err)
	}
}

func TestV4ContinuationPersistenceFreezesPlanAndUsesCAS(t *testing.T) {
	ctx := context.Background()
	s := tempV4DB(t)
	commandRequest := CreateContinuationCommandRequest{
		CommandKey:  "command-key-1",
		SubjectID:   "subject-1",
		RunID:       "run-1",
		PayloadJSON: ` { "target" : "quality" } `,
		Actor:       "tester",
		Reason:      "continue work",
	}
	command, err := s.CreateContinuationCommand(ctx, commandRequest)
	if err != nil {
		t.Fatalf("create continuation command: %v", err)
	}
	if !isUUIDv7(command.ID) || command.PayloadJSON != `{"target":"quality"}` || command.PayloadDigest != v4PayloadDigest(command.PayloadJSON) {
		t.Fatalf("unexpected continuation command: %+v", command)
	}
	replayedCommand, err := s.CreateContinuationCommand(ctx, commandRequest)
	if err != nil || replayedCommand.ID != command.ID {
		t.Fatalf("command replay = %+v, err = %v", replayedCommand, err)
	}
	actorConflict := commandRequest
	actorConflict.Actor = "another-tester"
	if _, err := s.CreateContinuationCommand(ctx, actorConflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("command actor conflict err = %v, want ErrIdempotencyConflict", err)
	}
	reasonConflict := commandRequest
	reasonConflict.Reason = "a different continuation reason"
	if _, err := s.CreateContinuationCommand(ctx, reasonConflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("command reason conflict err = %v, want ErrIdempotencyConflict", err)
	}
	commandConflict := commandRequest
	commandConflict.PayloadJSON = `{"target":"similarity"}`
	if _, err := s.CreateContinuationCommand(ctx, commandConflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("command payload conflict err = %v, want ErrIdempotencyConflict", err)
	}
	if _, err := s.CreateContinuationCommand(ctx, CreateContinuationCommandRequest{CommandKey: "bad-json", SubjectID: "subject-1", PayloadJSON: `{not-json}`, Actor: "tester"}); err == nil {
		t.Fatal("invalid command JSON was accepted")
	}

	session, err := s.CreateRepairSession(ctx, CreateRepairSessionRequest{
		CommandID: command.ID, SubjectID: command.SubjectID, BaseRevisionID: "revision-1", MaxRounds: 2,
		FindingsJSON: `[]`, PolicyJSON: `{ "rounds" : 2 }`, IdempotencyKey: "repair-session-key-1", Actor: "tester",
	})
	if err != nil {
		t.Fatalf("create repair session: %v", err)
	}
	session, err = s.TransitionRepairSession(ctx, TransitionRepairSessionRequest{RepairSessionID: session.ID, ExpectedVersion: session.Version, Status: RepairSessionNeedsHuman, Actor: "tester", Reason: "waiting for direction"})
	if err != nil || session.Status != RepairSessionNeedsHuman || session.Version != 2 {
		t.Fatalf("transition repair session = %+v, err = %v", session, err)
	}
	if _, err := s.TransitionRepairSession(ctx, TransitionRepairSessionRequest{RepairSessionID: session.ID, ExpectedVersion: 1, Status: RepairSessionOpen, Actor: "tester"}); !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("stale repair transition err = %v, want ErrOptimisticLock", err)
	}

	changeRequest := CreatePreparedChangeRequest{
		CommandID: command.ID, RepairSessionID: session.ID, RoundOrdinal: 1,
		ProviderID: "change-provider", OperationKey: "mutation-operation-1",
		PayloadJSON: `{ "directive" : "repair" }`, ObservedChangesJSON: `["subject/content"]`,
		BeforeDigest: "sha256:before", AfterDigest: "sha256:after", Actor: "tester", Reason: "prepared repair",
	}
	change, err := s.CreatePreparedChange(ctx, changeRequest)
	if err != nil {
		t.Fatalf("create prepared change: %v", err)
	}
	replayedChange, err := s.CreatePreparedChange(ctx, changeRequest)
	if err != nil || replayedChange.ID != change.ID {
		t.Fatalf("prepared change replay = %+v, err = %v", replayedChange, err)
	}
	changeConflict := changeRequest
	changeConflict.AfterDigest = "sha256:changed"
	if _, err := s.CreatePreparedChange(ctx, changeConflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("prepared change conflict err = %v, want ErrIdempotencyConflict", err)
	}

	receipt, err := s.CreateMutationReceipt(ctx, CreateMutationReceiptRequest{
		PreparedChangeID: change.ID, OperationKey: change.OperationKey, Outcome: MutationReceiptUncertain,
		ReceiptJSON: `{ "external" : "unknown" }`, IdempotencyKey: "receipt-key-1", Actor: "tester",
	})
	if err != nil || !isUUIDv7(receipt.ID) || receipt.ReceiptDigest != v4PayloadDigest(receipt.ReceiptJSON) {
		t.Fatalf("create mutation receipt = %+v, err = %v", receipt, err)
	}

	planRequest := CreateFrozenPlanRequest{
		CommandID: command.ID, PreparedChangeID: change.ID, SubjectID: command.SubjectID, SubjectRevisionID: "revision-2",
		SubjectDigest: "sha256:after", WorkflowFingerprint: "sha256:workflow", PlanFingerprint: "sha256:plan",
		PayloadJSON: `{ "transitions" : ["quality"] }`, ExpiresAt: time.Now().Add(time.Hour), Actor: "tester", Reason: "freeze plan",
	}
	plan, err := s.CreateFrozenPlan(ctx, planRequest)
	if err != nil {
		t.Fatalf("create frozen plan: %v", err)
	}
	replayedPlan, err := s.CreateFrozenPlan(ctx, planRequest)
	if err != nil || replayedPlan.ID != plan.ID {
		t.Fatalf("frozen plan replay = %+v, err = %v", replayedPlan, err)
	}
	planConflict := planRequest
	planConflict.PayloadJSON = `{"transitions":["similarity"]}`
	if _, err := s.CreateFrozenPlan(ctx, planConflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("frozen plan conflict err = %v, want ErrIdempotencyConflict", err)
	}
	if _, err := s.db.Exec(`UPDATE frozen_plans_v4 SET payload_json = '{}' WHERE id = ?`, plan.ID); err == nil {
		t.Fatal("frozen plan accepted direct payload update")
	}

	executionRequest := CreateContinuationExecutionRequest{
		PlanID: plan.ID, RunID: command.RunID, IdempotencyKey: "execution-key-1", PayloadJSON: `{ "schedule" : ["quality"] }`, Actor: "tester",
	}
	execution, err := s.CreateContinuationExecution(ctx, executionRequest)
	if err != nil {
		t.Fatalf("create continuation execution: %v", err)
	}
	replayedExecution, err := s.CreateContinuationExecution(ctx, executionRequest)
	if err != nil || replayedExecution.ID != execution.ID {
		t.Fatalf("continuation execution replay = %+v, err = %v", replayedExecution, err)
	}
	execution, err = s.TransitionContinuationExecution(ctx, TransitionContinuationExecutionRequest{ContinuationExecutionID: execution.ID, ExpectedVersion: execution.Version, State: ContinuationExecutionRunning, Actor: "tester"})
	if err != nil || execution.State != ContinuationExecutionRunning || execution.Version != 2 {
		t.Fatalf("start continuation execution = %+v, err = %v", execution, err)
	}
	execution, err = s.TransitionContinuationExecution(ctx, TransitionContinuationExecutionRequest{ContinuationExecutionID: execution.ID, ExpectedVersion: execution.Version, State: ContinuationExecutionReconcileRequired, Actor: "tester"})
	if err != nil || execution.State != ContinuationExecutionReconcileRequired {
		t.Fatalf("reconcile continuation execution = %+v, err = %v", execution, err)
	}
	execution, err = s.TransitionContinuationExecution(ctx, TransitionContinuationExecutionRequest{ContinuationExecutionID: execution.ID, ExpectedVersion: execution.Version, State: ContinuationExecutionCompleted, Actor: "tester"})
	if err != nil || execution.FinishedAt == nil || execution.State != ContinuationExecutionCompleted {
		t.Fatalf("complete continuation execution = %+v, err = %v", execution, err)
	}
	if _, err := s.TransitionContinuationExecution(ctx, TransitionContinuationExecutionRequest{ContinuationExecutionID: execution.ID, ExpectedVersion: execution.Version, State: ContinuationExecutionRunning, Actor: "tester"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal continuation transition err = %v, want ErrInvalidTransition", err)
	}
	if _, err := s.db.Exec(`UPDATE continuation_executions_v4 SET payload_json = '{}' WHERE id = ?`, execution.ID); err == nil {
		t.Fatal("continuation execution accepted direct payload update")
	}
	events, err := s.ListAuditEvents(ctx, ListAuditEventsRequest{EntityID: execution.ID})
	if err != nil || len(events) < 3 {
		t.Fatalf("continuation execution audit = %+v, err = %v", events, err)
	}
	if strings.Contains(strings.Join([]string{command.PayloadJSON, plan.PayloadJSON, execution.PayloadJSON}, ""), "not-json") {
		t.Fatal("unexpected noncanonical payload persisted")
	}
}
