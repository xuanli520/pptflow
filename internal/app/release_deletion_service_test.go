package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

func TestPreviewPurgeTaskIsReadOnlyAndReportsLifecycleAndDependencyBlockers(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := NewLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, err := services.Tasks.CreateDraft(ctx, CreateDraftTaskRequest{
		Slug: "purge-preview", Actor: "tester", Reason: "create purge preview fixture",
	})
	if err != nil {
		t.Fatal(err)
	}

	beforeEvents, err := dataStore.ListAuditEvents(ctx, store.ListAuditEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := services.Deletion.PreviewPurgeTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Eligible || preview.WillMutate || !hasPurgeTaskBlocker(preview.Blockers, "task_not_soft_deleted", "") {
		t.Fatalf("draft purge preview = %+v", preview)
	}
	unchanged, err := services.Tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Version != task.Version || unchanged.LifecycleState != task.LifecycleState {
		t.Fatalf("preview changed task: before=%+v after=%+v", task, unchanged)
	}
	afterEvents, err := dataStore.ListAuditEvents(ctx, store.ListAuditEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(afterEvents) != len(beforeEvents) {
		t.Fatalf("preview wrote audit/deletion state: before=%d after=%d", len(beforeEvents), len(afterEvents))
	}

	deleted, _, err := services.Deletion.SoftDeleteTask(ctx, task.ID, task.Version, "tester", "soft delete preview fixture")
	if err != nil {
		t.Fatal(err)
	}
	preview, err = services.Deletion.PreviewPurgeTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Eligible || preview.WillMutate || len(preview.Blockers) != 0 || preview.Task.Version != deleted.Version {
		t.Fatalf("eligible purge preview = %+v", preview)
	}
	pending, err := dataStore.CreateOutboxEvent(ctx, store.CreateOutboxEventRequest{
		Topic: "task.purge.fixture", EntityType: "task", EntityID: task.ID,
		PayloadJSON: `{"fixture":true}`, IdempotencyKey: "purge-preview-pending", Actor: "tester", Reason: "hold purge fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err = dataStore.ListAuditEvents(ctx, store.ListAuditEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	preview, err = services.Deletion.PreviewPurgeTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Task.ID != task.ID || preview.Task.Version != deleted.Version || preview.Eligible || preview.WillMutate {
		t.Fatalf("soft-deleted purge preview = %+v", preview)
	}
	if !hasPurgeTaskBlocker(preview.Blockers, "pending_outbox", pending.ID) || len(preview.Dependencies.PendingOutboxIDs) != 1 {
		t.Fatalf("preview omitted pending outbox blocker: %+v", preview)
	}
	afterEvents, err = dataStore.ListAuditEvents(ctx, store.ListAuditEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(afterEvents) != len(beforeEvents) {
		t.Fatalf("blocked preview wrote audit/deletion state: before=%d after=%d", len(beforeEvents), len(afterEvents))
	}
}

func hasPurgeTaskBlocker(blockers []PurgeTaskBlocker, code, id string) bool {
	for _, blocker := range blockers {
		if blocker.Code != code {
			continue
		}
		if id == "" {
			return true
		}
		for _, candidate := range blocker.IDs {
			if candidate == id {
				return true
			}
		}
	}
	return false
}

func TestPurgeTaskBlockersIncludeImmutableArtifactReferences(t *testing.T) {
	blockers := purgeTaskBlockers(store.TaskV2{LifecycleState: store.TaskLifecycleDeleted}, store.PurgeDependencyReport{
		ArtifactManifestIDs: []string{"manifest-id"},
		ArtifactRefIDs:      []string{"ref-id"},
	})
	if !hasPurgeTaskBlocker(blockers, "artifact_manifest", "manifest-id") || !hasPurgeTaskBlocker(blockers, "artifact_ref", "ref-id") {
		t.Fatalf("artifact purge blockers = %+v", blockers)
	}
}

func TestPurgeTaskRemovesOnlyManagedDirectoryAndReplaysIdempotently(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := NewLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, err := services.Tasks.CreateDraft(ctx, CreateDraftTaskRequest{
		Slug: "purge-execute", Actor: "tester", Reason: "create purge fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	deleted, _, err := services.Deletion.SoftDeleteTask(ctx, task.ID, task.Version, "tester", "retire purge fixture")
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, managedTasksDirectory, task.ID)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "ephemeral.txt"), []byte("remove me"), 0o640); err != nil {
		t.Fatal(err)
	}
	request := PurgeTaskRequest{
		TaskID: task.ID, ExpectedTaskVersion: deleted.Version, IdempotencyKey: "app-purge-execute",
		Actor: "tester", Reason: "retention elapsed",
	}

	result, err := services.Deletion.PurgeTask(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Purged || result.InProgress || result.Operation.State != store.TaskPurgeCompleted || result.Operation.CompletedAt == nil {
		t.Fatalf("purge result = %+v", result)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed task directory remains after purge: %v", err)
	}
	lease, err := dataStore.GetLease(ctx, result.Operation.LeaseID)
	if err != nil || lease == nil || lease.State != store.LeaseReleased {
		t.Fatalf("completed purge lease = %+v err=%v", lease, err)
	}

	replayed, err := services.Deletion.PurgeTask(ctx, request)
	if err != nil {
		t.Fatalf("idempotent purge replay: %v", err)
	}
	if !replayed.Purged || replayed.Operation.ID != result.Operation.ID || replayed.Operation.Version != result.Operation.Version {
		t.Fatalf("replayed purge = %+v initial=%+v", replayed, result)
	}
	if _, err := services.Deletion.RestoreTask(ctx, task.ID, deleted.Version, store.TaskLifecycleDraft, "tester", "unsafe restore"); !errors.Is(err, store.ErrTaskPurged) {
		t.Fatalf("restore after irreversible purge err=%v, want ErrTaskPurged", err)
	}
	preview, err := services.Deletion.PreviewPurgeTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Eligible || !hasPurgeTaskBlocker(preview.Blockers, "task_already_purged", result.Operation.ID) {
		t.Fatalf("completed purge preview = %+v", preview)
	}
}

func TestPurgeTaskFinalizesWhenManagedDirectoryWasRemovedBeforeRecovery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := NewLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, err := services.Tasks.CreateDraft(ctx, CreateDraftTaskRequest{
		Slug: "purge-missing-directory", Actor: "tester", Reason: "create purge fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	deleted, _, err := services.Deletion.SoftDeleteTask(ctx, task.ID, task.Version, "tester", "retire purge fixture")
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, managedTasksDirectory, task.ID)
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture unexpectedly has a managed task directory: %v", err)
	}
	result, err := services.Deletion.PurgeTask(ctx, PurgeTaskRequest{
		TaskID: task.ID, ExpectedTaskVersion: deleted.Version, IdempotencyKey: "app-purge-missing-directory",
		Actor: "tester", Reason: "recover interrupted purge",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Purged || result.Operation.State != store.TaskPurgeCompleted {
		t.Fatalf("missing-directory purge result = %+v", result)
	}
}

func TestPurgeTaskRejectsSymlinkAndRecoversWithSameIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := NewLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, err := services.Tasks.CreateDraft(ctx, CreateDraftTaskRequest{
		Slug: "purge-symlink", Actor: "tester", Reason: "create purge fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	deleted, _, err := services.Deletion.SoftDeleteTask(ctx, task.ID, task.Version, "tester", "retire purge fixture")
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "must-survive.txt")
	if err := os.WriteFile(sentinel, []byte("outside managed root"), 0o640); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, managedTasksDirectory)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(parent, task.ID)
	if err := os.Symlink(outside, directory); err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("creating symlinks is not permitted in this test environment: %v", err)
		}
		t.Fatalf("create unsafe task-directory symlink: %v", err)
	}
	request := PurgeTaskRequest{
		TaskID: task.ID, ExpectedTaskVersion: deleted.Version, IdempotencyKey: "app-purge-symlink",
		Actor: "tester", Reason: "retention elapsed",
	}

	failed, err := services.Deletion.PurgeTask(ctx, request)
	if !errors.Is(err, store.ErrTaskPurgeFilesystem) {
		t.Fatalf("unsafe purge err=%v, want ErrTaskPurgeFilesystem", err)
	}
	if !failed.InProgress || failed.Operation.State != store.TaskPurgeInProgress || failed.Operation.LastError == "" {
		t.Fatalf("unsafe purge result = %+v", failed)
	}
	preview, err := services.Deletion.PreviewPurgeTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Eligible || !hasPurgeTaskBlocker(preview.Blockers, "task_purge_in_progress", failed.Operation.ID) {
		t.Fatalf("failed-purge preview = %+v", preview)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("purge followed unsafe symlink: sentinel stat=%v", err)
	}
	lease, err := dataStore.GetLease(ctx, failed.Operation.LeaseID)
	if err != nil || lease == nil || lease.State != store.LeaseReleased {
		t.Fatalf("failed purge did not release its lease: %+v err=%v", lease, err)
	}

	if err := os.Remove(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	recovered, err := services.Deletion.PurgeTask(ctx, request)
	if err != nil {
		t.Fatalf("recover purge with same key: %v", err)
	}
	if !recovered.Purged || recovered.Operation.ID != failed.Operation.ID || recovered.Operation.State != store.TaskPurgeCompleted {
		t.Fatalf("recovered purge = %+v failed=%+v", recovered, failed)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered purge left task directory: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("recovered purge damaged outside sentinel: %v", err)
	}
}

func TestPurgeTaskPersistsBlockedOperationWithoutFilesystemMutation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := NewLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, err := services.Tasks.CreateDraft(ctx, CreateDraftTaskRequest{
		Slug: "purge-blocked", Actor: "tester", Reason: "create purge fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	deleted, _, err := services.Deletion.SoftDeleteTask(ctx, task.ID, task.Version, "tester", "retire purge fixture")
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, managedTasksDirectory, task.ID)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(directory, "remain.txt")
	if err := os.WriteFile(sentinel, []byte("still pinned"), 0o640); err != nil {
		t.Fatal(err)
	}
	pending, err := dataStore.CreateOutboxEvent(ctx, store.CreateOutboxEventRequest{
		Topic: "task.purge.blocked", EntityType: "task", EntityID: task.ID, PayloadJSON: `{}`,
		IdempotencyKey: "app-purge-blocked-outbox", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := services.Deletion.PurgeTask(ctx, PurgeTaskRequest{
		TaskID: task.ID, ExpectedTaskVersion: deleted.Version, IdempotencyKey: "app-purge-blocked-operation",
		Actor: "tester", Reason: "retention elapsed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Purged || result.InProgress || result.Operation.State != store.TaskPurgeBlocked || len(result.Dependencies.PendingOutboxIDs) != 1 || result.Dependencies.PendingOutboxIDs[0] != pending.ID {
		t.Fatalf("blocked purge result = %+v", result)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("blocked purge changed managed directory: %v", err)
	}
}
