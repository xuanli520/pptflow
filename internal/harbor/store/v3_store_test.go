package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func tempV3DB(t *testing.T) *Store {
	t.Helper()
	return tempDB(t)
}

func TestAtomicTaskRevisionRejectsInvalidDigestWithoutPartialTask(t *testing.T) {
	ctx := context.Background()
	s := tempV3DB(t)
	allocated, err := NewUUIDv7()
	if err != nil || ValidateUUIDv7(allocated) != nil {
		t.Fatalf("public UUIDv7 helper failed: id=%q err=%v", allocated, err)
	}
	result, err := s.CreateTaskWithRevision(ctx, CreateTaskWithRevisionRequest{
		Task: CreateTaskV2Request{
			Slug: "imported-task", SourceRepo: "https://example.invalid/repo", SourceCommit: "abc123",
			Actor: "importer", Reason: "create managed task",
		},
		Revision: CreateTaskRevisionRequest{
			Origin: RevisionOriginImported, TaskDigest: validTaskDigest("d"), ManifestID: "managed-manifest", Actor: "importer",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision.TaskID != result.Task.ID || result.Revision.State != RevisionStateSealed || result.Revision.VersionNumber != 1 {
		t.Fatalf("atomic task/revision binding is invalid: %+v", result)
	}
	if _, err := s.CreateTaskWithRevision(ctx, CreateTaskWithRevisionRequest{
		Task:     CreateTaskV2Request{Slug: "must-roll-back", Actor: "importer"},
		Revision: CreateTaskRevisionRequest{Origin: RevisionOriginImported, TaskDigest: "sha256:" + strings.Repeat("0", 64), Actor: "importer"},
	}); err == nil {
		t.Fatal("atomic import accepted a V1 digest")
	}
	tasks, err := s.ListTasksV2(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.Slug == "must-roll-back" {
			t.Fatal("atomic import left a partial task after invalid revision")
		}
	}
}

func TestManagedWorkspaceAndExecutionAttemptRepositories(t *testing.T) {
	ctx := context.Background()
	s := tempV3DB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "harbor.standard", WorkflowTemplateVersion: "v1",
		ResolvedProfileHash: "profile", DefinitionHash: "definition", RunManifestJSON: `{}`, Trigger: "create", Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := s.CreateManagedWorkspace(ctx, CreateManagedWorkspaceRequest{
		RootURI: "file:///managed/task-workspace", Purpose: "validation", TaskID: task.ID, RevisionID: revision.ID, RunID: run.ID, Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = s.TransitionManagedWorkspace(ctx, TransitionManagedWorkspaceRequest{WorkspaceID: workspace.ID, ExpectedVersion: workspace.Version, State: WorkspaceReleased, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = s.TransitionManagedWorkspace(ctx, TransitionManagedWorkspaceRequest{WorkspaceID: workspace.ID, ExpectedVersion: workspace.Version, State: WorkspaceTrash, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = s.TransitionManagedWorkspace(ctx, TransitionManagedWorkspaceRequest{WorkspaceID: workspace.ID, ExpectedVersion: workspace.Version, State: WorkspacePurged, Actor: "tester"})
	if err != nil || workspace.State != WorkspacePurged {
		t.Fatalf("workspace lifecycle failed: %+v err=%v", workspace, err)
	}

	runAttempt, err := s.CreateRunAttempt(ctx, CreateRunAttemptRequest{RunID: run.ID, Ordinal: 1, Trigger: "create", Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	runAttempt, err = s.TransitionRunAttempt(ctx, TransitionRunAttemptRequest{RunAttemptID: runAttempt.ID, ExpectedVersion: runAttempt.Version, Status: RunAttemptRunning, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	runAttempt, err = s.TransitionRunAttempt(ctx, TransitionRunAttemptRequest{RunAttemptID: runAttempt.ID, ExpectedVersion: runAttempt.Version, Status: RunAttemptSucceeded, Actor: "tester"})
	if err != nil || runAttempt.FinishedAt == nil {
		t.Fatalf("run attempt lifecycle failed: %+v err=%v", runAttempt, err)
	}
	stage, err := s.CreateStageAttempt(ctx, CreateStageAttemptRequest{RunID: run.ID, StageKey: "generate", StageGroup: "generation", Ordinal: 1, InputFingerprint: "input", Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := s.CreateNodeAttempt(ctx, CreateNodeAttemptRequest{StageAttemptID: stage.ID, NodeID: "draft", Generation: 0, Attempt: 1, IdempotencyKey: "node-key", Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	node, err = s.TransitionNodeAttempt(ctx, TransitionNodeAttemptRequest{NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: NodeAttemptRunning, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := s.CreateTurnCheckpoint(ctx, CreateTurnCheckpointRequest{NodeAttemptID: node.ID, Turn: 1, Substep: "draft", InputDigest: "input-sha256", PayloadJSON: `{"draft":true}`, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err = s.TransitionTurnCheckpoint(ctx, TransitionTurnCheckpointRequest{CheckpointID: checkpoint.ID, ExpectedVersion: checkpoint.Version, Status: TurnCheckpointCompleted, ArtifactID: "candidate-artifact", Actor: "tester"})
	if err != nil || checkpoint.FinishedAt == nil {
		t.Fatalf("checkpoint lifecycle failed: %+v err=%v", checkpoint, err)
	}
	node, err = s.TransitionNodeAttempt(ctx, TransitionNodeAttemptRequest{NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: NodeAttemptCompleted, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTurnCheckpoint(ctx, TransitionTurnCheckpointRequest{CheckpointID: checkpoint.ID, ExpectedVersion: checkpoint.Version, Status: TurnCheckpointFailed, Actor: "tester"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal checkpoint transition err=%v", err)
	}
	checkpoints, err := s.ListTurnCheckpoints(ctx, node.ID)
	if err != nil || len(checkpoints) != 1 || checkpoints[0].ArtifactID != "candidate-artifact" {
		t.Fatalf("checkpoint list is incomplete: %+v err=%v", checkpoints, err)
	}
}

func TestOutboxLocalReleaseDeletionAndPurgeDependencies(t *testing.T) {
	ctx := context.Background()
	s := tempV3DB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	revision, err := s.TransitionTaskRevisionState(ctx, TransitionTaskRevisionStateRequest{RevisionID: revision.ID, ExpectedStateVersion: revision.StateVersion, State: RevisionStateReleased, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.CreateOutboxEvent(ctx, CreateOutboxEventRequest{Topic: "local.package.ready", EntityType: "task", EntityID: task.ID, PayloadJSON: `{"package":"local"}`, IdempotencyKey: "outbox-1", Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := s.CreateOutboxEvent(ctx, CreateOutboxEventRequest{Topic: "local.package.ready", EntityType: "task", EntityID: task.ID, PayloadJSON: `{"package":"local"}`, IdempotencyKey: "outbox-1", Actor: "tester"})
	if err != nil || replayed.ID != pending.ID {
		t.Fatalf("outbox idempotency failed: %+v err=%v", replayed, err)
	}
	release, err := s.CreateLocalPackageRelease(ctx, CreateLocalPackageReleaseRequest{
		ReleaseVersion: "1.0.0", TaskID: task.ID, RevisionID: revision.ID, TaskDigest: revision.TaskDigest,
		PackageRef: "objects/sha256/package", EvidenceRef: "objects/sha256/evidence", Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := s.SetReleaseChannel(ctx, SetReleaseChannelRequest{Channel: "stable", ReleaseID: release.ID, ExpectedVersion: 0, Actor: "tester"})
	if err != nil || channel.ReleaseID != release.ID {
		t.Fatalf("release channel did not bind: %+v err=%v", channel, err)
	}
	record, err := s.CreateDeletionRecord(ctx, CreateDeletionRecordRequest{EntityType: "task", EntityID: task.ID, Action: "purge", Actor: "tester", Reason: "retention"})
	if err != nil {
		t.Fatal(err)
	}
	record, err = s.TransitionDeletionRecord(ctx, TransitionDeletionRecordRequest{DeletionRecordID: record.ID, ExpectedVersion: record.Version, State: DeletionBlocked, Actor: "tester", Reason: "release pin"})
	if err != nil || record.State != DeletionBlocked {
		t.Fatalf("deletion record transition failed: %+v err=%v", record, err)
	}
	report, err := s.QueryPurgeDependencies(ctx, PurgeDependencyQuery{EntityType: "task", EntityID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !report.HasBlockers() || len(report.ReleaseIDs) != 1 || report.ReleaseIDs[0] != release.ID || len(report.PendingOutboxIDs) != 1 || report.PendingOutboxIDs[0] != pending.ID || len(report.PackageRefs) != 1 {
		t.Fatalf("purge dependencies are incomplete: %+v", report)
	}
	claim, err := s.ClaimOutboxEvents(ctx, ClaimOutboxEventsRequest{
		IdempotencyKey: "outbox-test-claim", Owner: "test-dispatcher", Limit: 1, LeaseTTL: time.Minute,
		Actor: "tester", Reason: "deliver package event",
	})
	if err != nil || len(claim.Events) != 1 || claim.Events[0].ID != pending.ID {
		t.Fatalf("outbox claim failed: %+v err=%v", claim, err)
	}
	pending, err = s.AckOutboxEvent(ctx, AckOutboxEventRequest{
		IdempotencyKey: "outbox-test-ack", OutboxEventID: claim.Events[0].ID, Owner: "test-dispatcher",
		ExpectedVersion: claim.Events[0].Version, LeaseFencingToken: claim.Events[0].LeaseFencingToken,
		Actor: "tester", Reason: "package event delivered",
	})
	if err != nil || pending.State != OutboxPublished {
		t.Fatalf("outbox acknowledgement failed: %+v err=%v", pending, err)
	}
	withdrawal, err := s.ExecuteReleaseWithdraw(ctx, ExecuteReleaseWithdrawRequest{
		ReleaseID: release.ID, ExpectedReleaseVersion: release.RecordVersion, IdempotencyKey: mustUUIDv7(t),
		Actor: "tester", Reason: "withdraw local package fixture",
	})
	if err != nil || withdrawal.Release.WithdrawnAt == nil || withdrawal.Receipt.ID == "" {
		t.Fatalf("local release withdrawal failed: %+v err=%v", withdrawal, err)
	}
	release = withdrawal.Release
	if _, err := s.SetReleaseChannel(ctx, SetReleaseChannelRequest{Channel: "withdrawn", ReleaseID: release.ID, ExpectedVersion: 0, Actor: "tester"}); err == nil {
		t.Fatal("withdrawn release was accepted as a channel target")
	}
	report, err = s.QueryPurgeDependencies(ctx, PurgeDependencyQuery{EntityType: "task_revision", EntityID: revision.ID})
	if err != nil || len(report.ReleaseIDs) != 1 || len(report.PendingOutboxIDs) != 0 {
		t.Fatalf("withdrawn release must still pin evidence: %+v err=%v", report, err)
	}
}

func TestCreateLocalPackageReleaseIdempotencyUsesUUIDv7Key(t *testing.T) {
	ctx := context.Background()
	s := tempV3DB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	revision, err := s.TransitionTaskRevisionState(ctx, TransitionTaskRevisionStateRequest{
		RevisionID: revision.ID, ExpectedStateVersion: revision.StateVersion, State: RevisionStateReleased, Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	localPackageKey, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	request := CreateLocalPackageReleaseRequest{
		IdempotencyKey: localPackageKey,
		ReleaseVersion: "1.0.1",
		TaskID:         task.ID,
		RevisionID:     revision.ID,
		TaskDigest:     revision.TaskDigest,
		PackageRef:     "objects/sha256/package-v2",
		EvidenceRef:    "objects/sha256/evidence",
		Actor:          "tester",
	}
	created, err := s.CreateLocalPackageRelease(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != localPackageKey {
		t.Fatalf("local package idempotency key was not retained as release identity: %+v", created)
	}
	replayed, err := s.CreateLocalPackageRelease(ctx, request)
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("local package idempotency replay failed: release=%+v err=%v", replayed, err)
	}
	conflicting := request
	conflicting.PackageRef = "objects/sha256/other-package"
	if _, err := s.CreateLocalPackageRelease(ctx, conflicting); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("local package idempotency conflict = %v, want ErrIdempotencyConflict", err)
	}
}
