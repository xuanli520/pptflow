package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRevisionCandidateReservesOperationBeforeProviderAndFinalizesFacts(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "template", WorkflowTemplateVersion: "v1",
		ResolvedProfileHash: "profile", DefinitionHash: "definition", RunManifestJSON: `{}`,
		Trigger: "test", Actor: "tester", Reason: "create source run",
	})
	if err != nil {
		t.Fatal(err)
	}
	command, err := s.CreateContinuationCommand(ctx, CreateContinuationCommandRequest{
		CommandKey: "candidate-command", SubjectID: task.ID, RunID: run.ID, PayloadJSON: `{}`,
		Actor: "tester", Reason: "repair task",
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.AcquireLease(ctx, AcquireLeaseRequest{
		ResourceType: "task_revision_candidate", ResourceID: task.ID, Owner: "candidate-owner", TTL: time.Hour,
		Actor: "tester", Reason: "exclusive candidate write",
	})
	if err != nil {
		t.Fatal(err)
	}
	targetRevisionID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	targetRunID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.CreateRevisionCandidate(ctx, CreateRevisionCandidateRequest{
		TaskID: task.ID, SourceRunID: run.ID, CommandID: command.ID, BaseRevisionID: revision.ID,
		BaseDigest: revision.TaskDigest, TargetRevisionID: targetRevisionID, TargetRunID: targetRunID,
		ExpectedTaskVersion: task.Version, ProviderID: "local_patch", CheckoutRelpath: "candidates/test/checkout",
		FindingsJSON: `[]`, LeaseID: lease.ID, LeaseOwner: lease.Owner, LeaseFencingToken: lease.FencingToken,
		LeaseVersion: lease.Version, Actor: "tester", Reason: "prepare candidate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.State != RevisionCandidateReady || candidate.TargetRevisionID != targetRevisionID {
		t.Fatalf("candidate = %+v", candidate)
	}
	if _, err := s.CreateTaskRevision(ctx, CreateTaskRevisionRequest{
		ID: targetRevisionID, TaskID: task.ID, Origin: RevisionOriginManual, TaskDigest: validTaskDigest("d"), Actor: "tester",
	}); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("reserved target revision id collision = %v, want ErrIdentityCollision", err)
	}
	operation, err := s.CreateChangeOperation(ctx, CreateChangeOperationRequest{
		CandidateID: candidate.ID, ProviderID: "local_patch", OperationKey: "candidate-operation", PayloadJSON: `{"diff":"--- task.toml\\n+++ task.toml\\n"}`,
		Actor: "tester", Reason: "reserve patch operation",
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, candidate, err = s.StartChangeOperation(ctx, StartChangeOperationRequest{
		OperationID: operation.ID, ExpectedVersion: operation.Version, LeaseID: lease.ID, LeaseOwner: lease.Owner,
		LeaseFencingToken: lease.FencingToken, LeaseVersion: lease.Version, Actor: "tester", Reason: "start patch operation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if operation.State != ChangeOperationRunning || candidate.State != RevisionCandidateApplying {
		t.Fatalf("started operation/candidate = %+v %+v", operation, candidate)
	}
	operation, candidate, change, receipt, err := s.FinalizeChangeOperation(ctx, FinalizeChangeOperationRequest{
		OperationID: operation.ID, ExpectedVersion: operation.Version, LeaseID: lease.ID, LeaseOwner: lease.Owner,
		LeaseFencingToken: lease.FencingToken, LeaseVersion: lease.Version, Outcome: MutationReceiptApplied,
		AfterDigest: validTaskDigest("d"), ObservedChangesJSON: `["task/task.toml"]`, PreparedChangeID: mustUUIDv7(t),
		PreparedChangePayloadJSON: `{"diff_digest":"sha256:test"}`, MutationReceiptID: mustUUIDv7(t),
		MutationReceiptJSON: `{"outcome":"applied"}`, MutationReceiptKey: "candidate-receipt", Actor: "tester", Reason: "verified patch",
	})
	if err != nil {
		t.Fatal(err)
	}
	if operation.State != ChangeOperationSucceeded || candidate.State != RevisionCandidatePrepared || change.BeforeDigest != revision.TaskDigest || receipt.Outcome != MutationReceiptApplied {
		t.Fatalf("finalized facts = operation=%+v candidate=%+v change=%+v receipt=%+v", operation, candidate, change, receipt)
	}
	if _, _, _, _, err := s.FinalizeChangeOperation(ctx, FinalizeChangeOperationRequest{
		OperationID: operation.ID, ExpectedVersion: operation.Version, LeaseID: lease.ID, LeaseOwner: lease.Owner,
		LeaseFencingToken: lease.FencingToken, LeaseVersion: lease.Version, Outcome: MutationReceiptApplied,
		AfterDigest: validTaskDigest("d"), ObservedChangesJSON: `["task/task.toml"]`, PreparedChangeID: change.ID,
		PreparedChangePayloadJSON: `{"diff_digest":"sha256:test"}`, MutationReceiptID: receipt.ID,
		MutationReceiptJSON: `{"outcome":"applied"}`, MutationReceiptKey: "candidate-receipt", Actor: "tester", Reason: "retry",
	}); err != nil {
		t.Fatalf("finalization replay: %v", err)
	}
}

func TestRevisionCandidateUnknownOperationCannotBeBlindlyRestarted(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "template", WorkflowTemplateVersion: "v1",
		ResolvedProfileHash: "profile", DefinitionHash: "definition", RunManifestJSON: `{}`, Trigger: "test", Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	command, err := s.CreateContinuationCommand(ctx, CreateContinuationCommandRequest{CommandKey: "unknown-command", SubjectID: task.ID, RunID: run.ID, PayloadJSON: `{}`, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.AcquireLease(ctx, AcquireLeaseRequest{ResourceType: "task_revision_candidate", ResourceID: task.ID, Owner: "owner", TTL: time.Hour, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.CreateRevisionCandidate(ctx, CreateRevisionCandidateRequest{
		TaskID: task.ID, SourceRunID: run.ID, CommandID: command.ID, BaseRevisionID: revision.ID, BaseDigest: revision.TaskDigest,
		TargetRevisionID: mustUUIDv7(t), TargetRunID: mustUUIDv7(t), ExpectedTaskVersion: task.Version, ProviderID: "agent_repair",
		CheckoutRelpath: "candidates/unknown/checkout", FindingsJSON: `[]`, LeaseID: lease.ID, LeaseOwner: lease.Owner,
		LeaseFencingToken: lease.FencingToken, LeaseVersion: lease.Version, Actor: "tester", Reason: "candidate",
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, err := s.CreateChangeOperation(ctx, CreateChangeOperationRequest{CandidateID: candidate.ID, ProviderID: candidate.ProviderID, OperationKey: "unknown-operation", PayloadJSON: `{}`, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	operation, _, err = s.StartChangeOperation(ctx, StartChangeOperationRequest{OperationID: operation.ID, ExpectedVersion: operation.Version, LeaseID: lease.ID, LeaseOwner: lease.Owner, LeaseFencingToken: lease.FencingToken, LeaseVersion: lease.Version, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	operation, candidate, err = s.MarkChangeOperationUnknown(ctx, operation.ID, operation.Version, "tester", "process lost")
	if err != nil {
		t.Fatal(err)
	}
	if operation.State != ChangeOperationUnknown || candidate.State != RevisionCandidateReconcileRequired {
		t.Fatalf("unknown projection = %+v %+v", operation, candidate)
	}
	if _, _, err := s.StartChangeOperation(ctx, StartChangeOperationRequest{OperationID: operation.ID, ExpectedVersion: operation.Version, LeaseID: lease.ID, LeaseOwner: lease.Owner, LeaseFencingToken: lease.FencingToken, LeaseVersion: lease.Version, Actor: "tester"}); !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("unknown operation restarted err=%v, want optimistic conflict", err)
	}
	currentLease, err := s.HeartbeatLease(ctx, HeartbeatLeaseRequest{LeaseID: lease.ID, Owner: lease.Owner, FencingToken: lease.FencingToken, ExpectedVersion: lease.Version, TTL: time.Hour, Actor: "tester", Reason: "reconcile fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.FinalizeChangeOperation(ctx, FinalizeChangeOperationRequest{
		OperationID: operation.ID, ExpectedVersion: operation.Version, LeaseID: lease.ID, LeaseOwner: lease.Owner,
		LeaseFencingToken: lease.FencingToken, LeaseVersion: lease.Version, Outcome: MutationReceiptApplied,
		AfterDigest: validTaskDigest("e"), ObservedChangesJSON: `["instruction.md"]`, PreparedChangeID: mustUUIDv7(t),
		PreparedChangePayloadJSON: `{}`, MutationReceiptID: mustUUIDv7(t), MutationReceiptJSON: `{}`,
		MutationReceiptKey: "unknown-stale-fence", Actor: "tester", Reason: "stale reconcile",
	}); !errors.Is(err, ErrFencingToken) {
		t.Fatalf("unknown finalization with stale lease = %v, want ErrFencingToken", err)
	}
	if _, _, _, _, err := s.FinalizeChangeOperation(ctx, FinalizeChangeOperationRequest{
		OperationID: operation.ID, ExpectedVersion: operation.Version, LeaseID: currentLease.ID, LeaseOwner: currentLease.Owner,
		LeaseFencingToken: currentLease.FencingToken, LeaseVersion: currentLease.Version, Outcome: MutationReceiptApplied,
		AfterDigest: validTaskDigest("e"), ObservedChangesJSON: `["instruction.md"]`, PreparedChangeID: mustUUIDv7(t),
		PreparedChangePayloadJSON: `{}`, MutationReceiptID: mustUUIDv7(t), MutationReceiptJSON: `{}`,
		MutationReceiptKey: "unknown-current-fence", Actor: "tester", Reason: "verified reconcile",
	}); err != nil {
		t.Fatalf("unknown finalization with current lease: %v", err)
	}
}

func TestRevisionCandidateUnknownOperationRejectsReplacementLease(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "template", WorkflowTemplateVersion: "v1",
		ResolvedProfileHash: "profile", DefinitionHash: "definition", RunManifestJSON: `{}`, Trigger: "test", Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	command, err := s.CreateContinuationCommand(ctx, CreateContinuationCommandRequest{CommandKey: "replacement-lease-command", SubjectID: task.ID, RunID: run.ID, PayloadJSON: `{}`, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	originalLease, err := s.AcquireLease(ctx, AcquireLeaseRequest{ResourceType: "task_revision_candidate", ResourceID: task.ID, Owner: "original-owner", TTL: time.Hour, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.CreateRevisionCandidate(ctx, CreateRevisionCandidateRequest{
		TaskID: task.ID, SourceRunID: run.ID, CommandID: command.ID, BaseRevisionID: revision.ID, BaseDigest: revision.TaskDigest,
		TargetRevisionID: mustUUIDv7(t), TargetRunID: mustUUIDv7(t), ExpectedTaskVersion: task.Version, ProviderID: "agent_repair",
		CheckoutRelpath: "candidates/replacement/checkout", FindingsJSON: `[]`, LeaseID: originalLease.ID, LeaseOwner: originalLease.Owner,
		LeaseFencingToken: originalLease.FencingToken, LeaseVersion: originalLease.Version, Actor: "tester", Reason: "candidate",
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, err := s.CreateChangeOperation(ctx, CreateChangeOperationRequest{CandidateID: candidate.ID, ProviderID: candidate.ProviderID, OperationKey: "replacement-lease-operation", PayloadJSON: `{}`, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	operation, _, err = s.StartChangeOperation(ctx, StartChangeOperationRequest{OperationID: operation.ID, ExpectedVersion: operation.Version, LeaseID: originalLease.ID, LeaseOwner: originalLease.Owner, LeaseFencingToken: originalLease.FencingToken, LeaseVersion: originalLease.Version, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	operation, _, err = s.MarkChangeOperationUnknown(ctx, operation.ID, operation.Version, "tester", "provider process lost")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseLease(ctx, ReleaseLeaseRequest{LeaseID: originalLease.ID, Owner: originalLease.Owner, FencingToken: originalLease.FencingToken, ExpectedVersion: originalLease.Version, Actor: "tester", Reason: "simulate expired owner"}); err != nil {
		t.Fatal(err)
	}
	replacementLease, err := s.AcquireLease(ctx, AcquireLeaseRequest{ResourceType: "task_revision_candidate", ResourceID: task.ID, Owner: "replacement-owner", TTL: time.Hour, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.FinalizeChangeOperation(ctx, FinalizeChangeOperationRequest{
		OperationID: operation.ID, ExpectedVersion: operation.Version, LeaseID: replacementLease.ID, LeaseOwner: replacementLease.Owner,
		LeaseFencingToken: replacementLease.FencingToken, LeaseVersion: replacementLease.Version, Outcome: MutationReceiptApplied,
		AfterDigest: validTaskDigest("e"), ObservedChangesJSON: `["instruction.md"]`, PreparedChangeID: mustUUIDv7(t),
		PreparedChangePayloadJSON: `{}`, MutationReceiptID: mustUUIDv7(t), MutationReceiptJSON: `{}`,
		MutationReceiptKey: "replacement-lease-receipt", Actor: "tester", Reason: "unsafe takeover",
	}); !errors.Is(err, ErrFencingToken) {
		t.Fatalf("replacement lease finalized old candidate err=%v, want ErrFencingToken", err)
	}
}

func TestCreateAndBindRevisionCandidatePlanIsAtomicAndRecoversUnboundPlan(t *testing.T) {
	t.Run("validation failure leaves no executable plan", func(t *testing.T) {
		fixture := prepareRevisionCandidatePlanFixture(t)
		request := fixture.planRequest(t, fixture.store.now().UTC().Add(time.Hour))
		_, _, err := fixture.store.CreateAndBindRevisionCandidatePlan(context.Background(), CreateAndBindRevisionCandidatePlanRequest{
			Plan: request, CandidateID: fixture.candidate.ID, ExpectedCandidateVersion: fixture.candidate.Version,
			FinalManifestID: "manifest-object", ChildRunManifestJSON: "not-json", Actor: "tester", Reason: "bind candidate",
		})
		if err == nil {
			t.Fatal("invalid child manifest created a plan")
		}
		storedPlan, lookupErr := fixture.store.GetFrozenPlanByCommand(context.Background(), fixture.command.ID)
		if lookupErr != nil || storedPlan != nil {
			t.Fatalf("failed atomic plan write = plan=%+v err=%v", storedPlan, lookupErr)
		}
		candidate, lookupErr := fixture.store.GetRevisionCandidate(context.Background(), fixture.candidate.ID)
		if lookupErr != nil || candidate == nil || candidate.FrozenPlanID != "" || candidate.State != RevisionCandidatePrepared {
			t.Fatalf("failed atomic candidate write = candidate=%+v err=%v", candidate, lookupErr)
		}
	})

	t.Run("candidate binding failure rolls back a newly inserted plan", func(t *testing.T) {
		fixture := prepareRevisionCandidatePlanFixture(t)
		request := fixture.planRequest(t, fixture.store.now().UTC().Add(time.Hour))
		if _, err := fixture.store.db.Exec(`
			CREATE TRIGGER force_revision_candidate_plan_bind_failure
			BEFORE UPDATE OF frozen_plan_id ON revision_candidates_v8
			BEGIN
				SELECT RAISE(ABORT, 'forced candidate binding failure');
			END
		`); err != nil {
			t.Fatal(err)
		}
		_, _, err := fixture.store.CreateAndBindRevisionCandidatePlan(context.Background(), CreateAndBindRevisionCandidatePlanRequest{
			Plan: request, CandidateID: fixture.candidate.ID, ExpectedCandidateVersion: fixture.candidate.Version,
			FinalManifestID: "manifest-object", ChildRunManifestJSON: `{}`, Actor: "tester", Reason: "force rollback",
		})
		if err == nil {
			t.Fatal("candidate binding failure committed an executable plan")
		}
		storedPlan, lookupErr := fixture.store.GetFrozenPlanByCommand(context.Background(), fixture.command.ID)
		if lookupErr != nil || storedPlan != nil {
			t.Fatalf("rolled-back plan remained visible = plan=%+v err=%v", storedPlan, lookupErr)
		}
		candidate, lookupErr := fixture.store.GetRevisionCandidate(context.Background(), fixture.candidate.ID)
		if lookupErr != nil || candidate == nil || candidate.FrozenPlanID != "" || candidate.State != RevisionCandidatePrepared {
			t.Fatalf("rolled-back candidate binding = candidate=%+v err=%v", candidate, lookupErr)
		}
	})

	t.Run("legacy unbound plan is bound without renewal", func(t *testing.T) {
		fixture := prepareRevisionCandidatePlanFixture(t)
		expiresAt := fixture.store.now().UTC().Add(time.Hour)
		request := fixture.planRequest(t, expiresAt)
		legacy, err := fixture.store.CreateFrozenPlan(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		stored, candidate, err := fixture.store.CreateAndBindRevisionCandidatePlan(context.Background(), CreateAndBindRevisionCandidatePlanRequest{
			Plan: request, CandidateID: fixture.candidate.ID, ExpectedCandidateVersion: fixture.candidate.Version,
			FinalManifestID: "manifest-object", ChildRunManifestJSON: `{}`, Actor: "tester", Reason: "recover legacy binding",
		})
		if err != nil {
			t.Fatal(err)
		}
		if stored.ID != legacy.ID || !stored.ExpiresAt.Equal(expiresAt) || candidate.FrozenPlanID != legacy.ID || candidate.FinalManifestID != "manifest-object" {
			t.Fatalf("recovered atomic binding = plan=%+v candidate=%+v", stored, candidate)
		}
		replayedPlan, replayedCandidate, err := fixture.store.CreateAndBindRevisionCandidatePlan(context.Background(), CreateAndBindRevisionCandidatePlanRequest{
			Plan: request, CandidateID: fixture.candidate.ID, ExpectedCandidateVersion: fixture.candidate.Version,
			FinalManifestID: "manifest-object", ChildRunManifestJSON: `{}`, Actor: "tester", Reason: "replay binding",
		})
		if err != nil || replayedPlan.ID != legacy.ID || replayedCandidate.Version != candidate.Version {
			t.Fatalf("idempotent bound plan replay = plan=%+v candidate=%+v err=%v", replayedPlan, replayedCandidate, err)
		}
	})
}

func TestExpireRevisionCandidateReleasesOnlyAnExpiredFrozenPlan(t *testing.T) {
	fixture := prepareRevisionCandidatePlanFixture(t)
	clock := fixture.store.now().UTC()
	fixture.store.now = func() time.Time { return clock }
	request := fixture.planRequest(t, clock.Add(time.Hour))
	_, candidate, err := fixture.store.CreateAndBindRevisionCandidatePlan(context.Background(), CreateAndBindRevisionCandidatePlanRequest{
		Plan: request, CandidateID: fixture.candidate.ID, ExpectedCandidateVersion: fixture.candidate.Version,
		FinalManifestID: "manifest-object", ChildRunManifestJSON: `{}`, Actor: "tester", Reason: "freeze candidate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ExpireRevisionCandidate(context.Background(), ExpireRevisionCandidateRequest{CandidateID: candidate.ID, ExpectedVersion: candidate.Version, Actor: "tester", Reason: "too early"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("premature candidate expiry = %v, want ErrInvalidTransition", err)
	}
	clock = clock.Add(2 * time.Hour)
	expired, err := fixture.store.ExpireRevisionCandidate(context.Background(), ExpireRevisionCandidateRequest{CandidateID: candidate.ID, ExpectedVersion: candidate.Version, Actor: "tester", Reason: "ttl elapsed"})
	if err != nil {
		t.Fatal(err)
	}
	if expired.State != RevisionCandidateDiscarded {
		t.Fatalf("expired candidate state = %s, want discarded", expired.State)
	}
	lease, err := fixture.store.GetLease(context.Background(), fixture.lease.ID)
	if err != nil || lease == nil || lease.State != LeaseReleased {
		t.Fatalf("expired candidate lease = %+v, err=%v", lease, err)
	}
	if replayed, err := fixture.store.ExpireRevisionCandidate(context.Background(), ExpireRevisionCandidateRequest{CandidateID: expired.ID, ExpectedVersion: expired.Version, Actor: "tester", Reason: "ttl replay"}); err != nil || replayed.State != RevisionCandidateDiscarded {
		t.Fatalf("expired candidate replay = %+v, err=%v", replayed, err)
	}
}

type revisionCandidatePlanFixture struct {
	store     *Store
	task      TaskV2
	revision  TaskRevision
	run       WorkflowRun
	command   ContinuationCommand
	lease     Lease
	candidate RevisionCandidate
	change    PreparedChange
}

func prepareRevisionCandidatePlanFixture(t *testing.T) revisionCandidatePlanFixture {
	t.Helper()
	ctx := context.Background()
	s := tempDB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "template", WorkflowTemplateVersion: "v1",
		ResolvedProfileHash: "profile", DefinitionHash: "definition", RunManifestJSON: `{}`, Trigger: "test", Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	command, err := s.CreateContinuationCommand(ctx, CreateContinuationCommandRequest{CommandKey: "candidate-plan-" + t.Name(), SubjectID: task.ID, RunID: run.ID, PayloadJSON: `{}`, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.AcquireLease(ctx, AcquireLeaseRequest{ResourceType: "task_revision_candidate", ResourceID: task.ID, Owner: "candidate-plan-owner", TTL: time.Hour, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.CreateRevisionCandidate(ctx, CreateRevisionCandidateRequest{
		TaskID: task.ID, SourceRunID: run.ID, CommandID: command.ID, BaseRevisionID: revision.ID, BaseDigest: revision.TaskDigest,
		TargetRevisionID: mustUUIDv7(t), TargetRunID: mustUUIDv7(t), ExpectedTaskVersion: task.Version, ProviderID: "local_patch",
		CheckoutRelpath: "candidates/fixture/checkout", FindingsJSON: `[]`, LeaseID: lease.ID, LeaseOwner: lease.Owner,
		LeaseFencingToken: lease.FencingToken, LeaseVersion: lease.Version, Actor: "tester", Reason: "candidate fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, err := s.CreateChangeOperation(ctx, CreateChangeOperationRequest{CandidateID: candidate.ID, ProviderID: candidate.ProviderID, OperationKey: "candidate-plan-operation-" + t.Name(), PayloadJSON: `{}`, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	operation, candidate, err = s.StartChangeOperation(ctx, StartChangeOperationRequest{OperationID: operation.ID, ExpectedVersion: operation.Version, LeaseID: lease.ID, LeaseOwner: lease.Owner, LeaseFencingToken: lease.FencingToken, LeaseVersion: lease.Version, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	_, candidate, change, _, err := s.FinalizeChangeOperation(ctx, FinalizeChangeOperationRequest{
		OperationID: operation.ID, ExpectedVersion: operation.Version, LeaseID: lease.ID, LeaseOwner: lease.Owner, LeaseFencingToken: lease.FencingToken, LeaseVersion: lease.Version,
		Outcome: MutationReceiptApplied, AfterDigest: validTaskDigest("d"), ObservedChangesJSON: `["instruction.md"]`, PreparedChangeID: mustUUIDv7(t),
		PreparedChangePayloadJSON: `{}`, MutationReceiptID: mustUUIDv7(t), MutationReceiptJSON: `{}`, MutationReceiptKey: "candidate-plan-receipt-" + t.Name(), Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	return revisionCandidatePlanFixture{store: s, task: task, revision: revision, run: run, command: command, lease: lease, candidate: candidate, change: change}
}

func (fixture revisionCandidatePlanFixture) planRequest(t *testing.T, expiresAt time.Time) CreateFrozenPlanRequest {
	t.Helper()
	return CreateFrozenPlanRequest{
		ID: mustUUIDv7(t), CommandID: fixture.command.ID, PreparedChangeID: fixture.change.ID, SubjectID: fixture.task.ID,
		SubjectRevisionID: fixture.candidate.TargetRevisionID, SubjectDigest: fixture.candidate.AfterDigest, WorkflowFingerprint: fixture.run.DefinitionHash,
		PlanFingerprint: "candidate-plan-fingerprint", PayloadJSON: `{}`, ExpiresAt: expiresAt, Actor: "tester", Reason: "freeze candidate",
	}
}

func mustUUIDv7(t *testing.T) string {
	t.Helper()
	id, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
