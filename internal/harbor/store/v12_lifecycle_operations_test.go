package store

import (
	"context"
	"errors"
	"testing"
)

func TestLifecycleOperationLedgerReplaysCanonicalRequestAndImmutableResult(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	key := mustUUIDv7(t)
	taskID := mustUUIDv7(t)
	revisionID := mustUUIDv7(t)
	runID := mustUUIDv7(t)
	request := BeginLifecycleOperationRequest{
		IdempotencyKey:     key,
		Action:             "task.import",
		RequestFingerprint: "sha256:task-import-fixture",
		TaskID:             taskID,
		RevisionID:         revisionID,
		RunID:              runID,
		Actor:              "tester",
		Reason:             "exercise lifecycle receipt replay",
	}
	first, err := s.BeginLifecycleOperation(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.Operation.State != LifecycleOperationPrepared || first.Operation.ID == "" || first.Operation.TaskID != taskID {
		t.Fatalf("first lifecycle operation = %+v", first)
	}

	// A retry has no authority to replace server-reserved entity identities.
	retry := request
	retry.TaskID = mustUUIDv7(t)
	retry.RevisionID = mustUUIDv7(t)
	replayed, err := s.BeginLifecycleOperation(ctx, retry)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Operation.ID != first.Operation.ID || replayed.Operation.TaskID != taskID || replayed.Operation.RevisionID != revisionID {
		t.Fatalf("replayed lifecycle operation = %+v, want original %+v", replayed, first.Operation)
	}

	conflict := request
	conflict.RequestFingerprint = "sha256:changed-import-payload"
	if _, err := s.BeginLifecycleOperation(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting lifecycle operation = %v, want ErrIdempotencyConflict", err)
	}

	completed, err := s.CompleteLifecycleOperation(ctx, CompleteLifecycleOperationRequest{
		OperationID:     first.Operation.ID,
		ExpectedVersion: first.Operation.Version,
		ResultJSON:      ` { "task_id": "` + taskID + `", "revision_id": "` + revisionID + `" } `,
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != LifecycleOperationCompleted || completed.CompletedAt == nil || completed.ResultJSON != `{"revision_id":"`+revisionID+`","task_id":"`+taskID+`"}` {
		t.Fatalf("completed lifecycle operation = %+v", completed)
	}

	completionReplay, err := s.CompleteLifecycleOperation(ctx, CompleteLifecycleOperationRequest{
		OperationID:     first.Operation.ID,
		ExpectedVersion: first.Operation.Version,
		ResultJSON:      `{"revision_id":"` + revisionID + `","task_id":"` + taskID + `"}`,
	})
	if err != nil {
		t.Fatalf("canonical completion replay: %v", err)
	}
	if completionReplay.ID != completed.ID || completionReplay.ResultJSON != completed.ResultJSON {
		t.Fatalf("completion replay = %+v, want %+v", completionReplay, completed)
	}
	if _, err := s.CompleteLifecycleOperation(ctx, CompleteLifecycleOperationRequest{
		OperationID:     first.Operation.ID,
		ExpectedVersion: completed.Version,
		ResultJSON:      `{"task_id":"different"}`,
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different lifecycle receipt result = %v, want ErrIdempotencyConflict", err)
	}
}

func TestLifecycleOperationRetainsExpectedIdentitySeparatelyFromTargetIdentity(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	targetTaskID := mustUUIDv7(t)
	targetRevisionID := mustUUIDv7(t)
	expectedTaskID := mustUUIDv7(t)
	expectedRevisionID := mustUUIDv7(t)
	expectedRunID := mustUUIDv7(t)
	expectedReleaseID := mustUUIDv7(t)
	expectedReviewID := mustUUIDv7(t)
	expectedCodeEdgeComplianceRecordID := mustUUIDv7(t)
	const expectedCodeEdgeAuthorizationFingerprint = "sha256:codeedge-authorization"

	started, err := s.BeginLifecycleOperation(ctx, BeginLifecycleOperationRequest{
		IdempotencyKey:                           mustUUIDv7(t),
		Action:                                   "task.fork",
		RequestFingerprint:                       "sha256:fork-source-checkpoint",
		TaskID:                                   targetTaskID,
		RevisionID:                               targetRevisionID,
		ExpectedTaskID:                           expectedTaskID,
		ExpectedRevisionID:                       expectedRevisionID,
		ExpectedRunID:                            expectedRunID,
		ExpectedReleaseID:                        expectedReleaseID,
		ExpectedReviewRequestID:                  expectedReviewID,
		ExpectedCodeEdgeComplianceRecordID:       expectedCodeEdgeComplianceRecordID,
		ExpectedCodeEdgeAuthorizationFingerprint: expectedCodeEdgeAuthorizationFingerprint,
		Actor:                                    "tester",
		Reason:                                   "retain original source checkpoint",
	})
	if err != nil {
		t.Fatal(err)
	}
	op := started.Operation
	if op.TaskID != targetTaskID || op.RevisionID != targetRevisionID ||
		op.ExpectedTaskID != expectedTaskID || op.ExpectedRevisionID != expectedRevisionID ||
		op.ExpectedRunID != expectedRunID || op.ExpectedReleaseID != expectedReleaseID || op.ExpectedReviewRequestID != expectedReviewID ||
		op.ExpectedCodeEdgeComplianceRecordID != expectedCodeEdgeComplianceRecordID ||
		op.ExpectedCodeEdgeAuthorizationFingerprint != expectedCodeEdgeAuthorizationFingerprint {
		t.Fatalf("lifecycle operation lost target/source identity distinction: %+v", op)
	}
	loaded, err := s.GetLifecycleOperation(ctx, op.ID)
	if err != nil || loaded == nil || loaded.ExpectedCodeEdgeComplianceRecordID != expectedCodeEdgeComplianceRecordID ||
		loaded.ExpectedCodeEdgeAuthorizationFingerprint != expectedCodeEdgeAuthorizationFingerprint {
		t.Fatalf("persisted CodeEdge lifecycle CAS facts = %+v, %v", loaded, err)
	}

	invalid := BeginLifecycleOperationRequest{
		IdempotencyKey: mustUUIDv7(t), Action: "package.local", RequestFingerprint: "sha256:invalid-codeedge-record",
		ExpectedCodeEdgeComplianceRecordID: "not-a-uuid", Actor: "tester", Reason: "reject invalid record identity",
	}
	if _, err := s.BeginLifecycleOperation(ctx, invalid); !errors.Is(err, ErrInvalidUUIDv7Identity) {
		t.Fatalf("invalid CodeEdge compliance record identity = %v, want ErrInvalidUUIDv7Identity", err)
	}
}

func TestListPreparedLifecycleOperationsByActionExcludesCompletedAndOtherActions(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	first, err := s.BeginLifecycleOperation(ctx, BeginLifecycleOperationRequest{
		IdempotencyKey: mustUUIDv7(t), Action: "authoring.start", RequestFingerprint: "sha256:prepared-authoring",
		TaskID: mustUUIDv7(t), RunID: mustUUIDv7(t), Actor: "tester", Reason: "project recoverable launch",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.BeginLifecycleOperation(ctx, BeginLifecycleOperationRequest{
		IdempotencyKey: mustUUIDv7(t), Action: "authoring.start", RequestFingerprint: "sha256:completed-authoring",
		TaskID: mustUUIDv7(t), RunID: mustUUIDv7(t), Actor: "tester", Reason: "complete another launch",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteLifecycleOperation(ctx, CompleteLifecycleOperationRequest{
		OperationID: second.Operation.ID, ExpectedVersion: second.Operation.Version, ResultJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginLifecycleOperation(ctx, BeginLifecycleOperationRequest{
		IdempotencyKey: mustUUIDv7(t), Action: "task.create", RequestFingerprint: "sha256:prepared-task",
		TaskID: mustUUIDv7(t), Actor: "tester", Reason: "unrelated prepared operation",
	}); err != nil {
		t.Fatal(err)
	}

	operations, err := s.ListPreparedLifecycleOperationsByAction(ctx, "authoring.start")
	if err != nil || len(operations) != 1 || operations[0].ID != first.Operation.ID || operations[0].State != LifecycleOperationPrepared {
		t.Fatalf("prepared authoring operations = %+v, %v", operations, err)
	}
	if _, err := s.ListPreparedLifecycleOperationsByAction(ctx, " "); err == nil {
		t.Fatal("blank lifecycle action was accepted")
	}
}

func TestLifecycleOperationIdentityParticipatesInGlobalUUIDv7Registry(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	operationID := mustUUIDv7(t)
	key := mustUUIDv7(t)
	if _, err := s.BeginLifecycleOperation(ctx, BeginLifecycleOperationRequest{
		ID:                 operationID,
		IdempotencyKey:     key,
		Action:             "task.create",
		RequestFingerprint: "sha256:identity-fixture",
		Actor:              "tester",
		Reason:             "verify global UUIDv7 registry",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTaskV2(ctx, CreateTaskV2Request{
		ID: operationID, Slug: "collision", MetadataJSON: `{}`, Actor: "tester", Reason: "must not reuse operation identity",
	}); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("reuse lifecycle operation identity = %v, want ErrIdentityCollision", err)
	}
}

func TestExecuteLifecycleTaskTransitionAtomicallyCompletesSoftDeleteReceipt(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, err := s.CreateTaskV2(ctx, CreateTaskV2Request{
		Slug: "atomic-soft-delete", MetadataJSON: `{}`, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	deletionRecordID := mustUUIDv7(t)
	begin, err := s.BeginLifecycleOperation(ctx, BeginLifecycleOperationRequest{
		IdempotencyKey:       mustUUIDv7(t),
		Action:               "task.soft_delete",
		RequestFingerprint:   "sha256:atomic-soft-delete",
		TaskID:               task.ID,
		DeletionRecordID:     deletionRecordID,
		TargetLifecycleState: TaskLifecycleDeleted,
		ExpectedTaskVersion:  task.Version,
		Actor:                "tester",
		Reason:               "soft-delete through lifecycle operation",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.ExecuteLifecycleTaskTransition(ctx, ExecuteLifecycleTaskTransitionRequest{
		OperationID: begin.Operation.ID, ExpectedVersion: begin.Operation.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Operation.State != LifecycleOperationCompleted || result.Task.LifecycleState != TaskLifecycleDeleted || result.Task.Version != task.Version+1 ||
		result.DeletionRecord == nil || result.DeletionRecord.ID != deletionRecordID || result.DeletionRecord.State != DeletionCompleted {
		t.Fatalf("atomic soft delete result = %+v", result)
	}
	persistedTask, err := s.GetTaskV2(ctx, task.ID)
	if err != nil || persistedTask == nil || persistedTask.LifecycleState != TaskLifecycleDeleted || persistedTask.Version != task.Version+1 {
		t.Fatalf("persisted soft-deleted task = %+v, %v", persistedTask, err)
	}
	persistedRecord, err := s.GetDeletionRecord(ctx, deletionRecordID)
	if err != nil || persistedRecord == nil || persistedRecord.State != DeletionCompleted || persistedRecord.EntityID != task.ID {
		t.Fatalf("persisted deletion record = %+v, %v", persistedRecord, err)
	}

	// A response can be lost after commit. Replaying the original operation
	// reads its immutable receipt without applying another transition.
	replayed, err := s.ExecuteLifecycleTaskTransition(ctx, ExecuteLifecycleTaskTransitionRequest{
		OperationID: begin.Operation.ID, ExpectedVersion: begin.Operation.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Operation.ID != result.Operation.ID || replayed.Task.Version != result.Task.Version || replayed.DeletionRecord == nil || replayed.DeletionRecord.ID != deletionRecordID {
		t.Fatalf("soft-delete replay = %+v, want %+v", replayed, result)
	}
}

func TestExecuteLifecycleTaskTransitionRejectsStaleCheckpointWithoutCompletion(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, err := s.CreateTaskV2(ctx, CreateTaskV2Request{
		Slug: "stale-archive", MetadataJSON: `{}`, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	begin, err := s.BeginLifecycleOperation(ctx, BeginLifecycleOperationRequest{
		IdempotencyKey:       mustUUIDv7(t),
		Action:               "task.archive",
		RequestFingerprint:   "sha256:stale-archive",
		TaskID:               task.ID,
		TargetLifecycleState: TaskLifecycleArchived,
		ExpectedTaskVersion:  task.Version,
		Actor:                "tester",
		Reason:               "archive through lifecycle operation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTaskV2(ctx, UpdateTaskV2Request{
		TaskID: task.ID, ExpectedVersion: task.Version, Title: "newer task state", Actor: "tester", Reason: "make checkpoint stale",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExecuteLifecycleTaskTransition(ctx, ExecuteLifecycleTaskTransitionRequest{
		OperationID: begin.Operation.ID, ExpectedVersion: begin.Operation.Version,
	}); !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("stale lifecycle transition = %v, want ErrOptimisticLock", err)
	}
	operation, err := s.GetLifecycleOperation(ctx, begin.Operation.ID)
	if err != nil || operation == nil || operation.State != LifecycleOperationPrepared || operation.ResultJSON != "" {
		t.Fatalf("stale lifecycle receipt = %+v, %v", operation, err)
	}
	persistedTask, err := s.GetTaskV2(ctx, task.ID)
	if err != nil || persistedTask == nil || persistedTask.LifecycleState != TaskLifecycleDraft || persistedTask.Version != task.Version+1 {
		t.Fatalf("stale task transition mutated task = %+v, %v", persistedTask, err)
	}
}
