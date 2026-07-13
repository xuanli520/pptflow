package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBackupIfDueAndCriticalOperation(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	initial, err := s.ListVerifiedBackups()
	if err != nil {
		t.Fatal(err)
	}
	if len(initial) != 1 || initial[0].Reason != "interval" {
		t.Fatalf("Open should create one initial verified backup, got %+v", initial)
	}
	s.now = func() time.Time { return initial[0].CreatedAt.Add(verifiedBackupInterval + time.Second) }
	due, err := s.BackupIfDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if due == nil || due.Reason != "interval" {
		t.Fatalf("due backup was not created: %+v", due)
	}
	before, err := s.ListVerifiedBackups()
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if err := s.WithCriticalOperation(ctx, "release_withdraw", func(context.Context) error {
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("critical operation callback was not invoked")
	}
	after, err := s.ListVerifiedBackups()
	if err != nil {
		t.Fatal(err)
	}
	foundCritical := false
	for _, record := range after {
		if record.Reason == "critical:release_withdraw" {
			foundCritical = true
			break
		}
	}
	if len(after) != len(before)+1 || !foundCritical {
		t.Fatalf("critical operation backup missing: before=%+v after=%+v", before, after)
	}
}

func TestOpenRestoresLatestVerifiedBackupAfterCorruption(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTaskV2(ctx, CreateTaskV2Request{Slug: "recoverable", Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	backupTime := time.Now().UTC().Add(time.Minute)
	s.now = func() time.Time { return backupTime }
	backup, err := s.BackupNow(ctx, "recovery_fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, dbFileName)
	if err := os.WriteFile(dbPath, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatalf("Open should restore from %s: %v", backup.Path, err)
	}
	defer reopened.Close()
	restored, err := reopened.GetTaskV2(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored == nil || restored.Slug != task.Slug {
		t.Fatalf("latest verified backup did not restore task: %+v", restored)
	}
}

func TestRestoreSkipsBackupWithInvalidChecksum(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.CreateTaskV2(ctx, CreateTaskV2Request{Slug: "first", Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	firstTime := time.Now().UTC().Add(time.Minute)
	s.now = func() time.Time { return firstTime }
	if _, err := s.BackupNow(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTaskV2(ctx, CreateTaskV2Request{Slug: "second", Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	secondTime := firstTime.Add(time.Minute)
	s.now = func() time.Time { return secondTime }
	newest, err := s.BackupNow(ctx, "newest")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newest.Path, []byte("corrupted backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, dbFileName)
	if err := os.WriteFile(dbPath, []byte("corrupted primary"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(dbPath + suffix)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, err := reopened.GetTaskV2(ctx, first.ID); err != nil || got == nil {
		t.Fatalf("valid older backup was not selected: got=%+v err=%v", got, err)
	}
}
