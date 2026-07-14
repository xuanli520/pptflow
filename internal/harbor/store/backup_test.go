package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
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

func TestBackupIfDueDoesNotTreatIneligibleSchemaBackupAsProtection(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	if err := os.RemoveAll(s.backupDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.backupDir, 0o700); err != nil {
		t.Fatal(err)
	}

	observedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	legacyPath := writeConsolidatedV2RecoveryBackup(t, s.rootDir, "legacy-only.sqlite", observedAt)
	mutateRecoveryBackup(t, legacyPath, func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE schema_version SET version = 1`)
		return err
	})
	writeRecoveryBackupManifest(t, legacyPath, observedAt)
	s.now = func() time.Time { return observedAt.Add(time.Minute) }

	due, err := s.BackupIfDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if due == nil || due.Reason != "interval" {
		t.Fatalf("ineligible schema backup suppressed V2 interval backup: %+v", due)
	}
	if err := verifyConsolidatedV2SQLiteFile(due.Path); err != nil {
		t.Fatalf("due backup is not recoverable consolidated V2: %v", err)
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

func TestOpenDoesNotRestoreNonConsolidatedV2Backup(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*sql.DB) error
		want   error
	}{
		{
			name: "legacy_v1_schema_history",
			mutate: func(db *sql.DB) error {
				_, err := db.Exec(`UPDATE schema_version SET version = 1`)
				return err
			},
			want: ErrLegacyV1Store,
		},
		{
			name: "retired_incremental_v20_history",
			mutate: func(db *sql.DB) error {
				_, err := db.Exec(`UPDATE schema_version SET version = 20`)
				return err
			},
			want: ErrPreConsolidationStore,
		},
		{
			name: "absent_baseline_marker",
			mutate: func(db *sql.DB) error {
				_, err := db.Exec(`DELETE FROM store_metadata WHERE key = ?`, baselineV2MetadataKey)
				return err
			},
			want: ErrPreConsolidationStore,
		},
		{
			name: "tampered_baseline_marker",
			mutate: func(db *sql.DB) error {
				_, err := db.Exec(`UPDATE store_metadata SET value = 'tampered' WHERE key = ?`, baselineV2MetadataKey)
				return err
			},
			want: ErrPreConsolidationStore,
		},
		{
			name: "missing_required_baseline_table",
			mutate: func(db *sql.DB) error {
				_, err := db.Exec(`DROP TABLE codeedge_compliance_records_v20`)
				return err
			},
			want: ErrPreConsolidationStore,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			backupPath := writeConsolidatedV2RecoveryBackup(t, root, "ineligible.sqlite", time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC))
			mutateRecoveryBackup(t, backupPath, test.mutate)
			writeRecoveryBackupManifest(t, backupPath, time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC))
			backupBefore, err := os.ReadFile(backupPath)
			if err != nil {
				t.Fatal(err)
			}

			dbPath := filepath.Join(root, dbFileName)
			corruptPrimary := []byte("corrupt primary must not be replaced")
			if err := os.WriteFile(dbPath, corruptPrimary, 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := Open(root); !errors.Is(err, test.want) {
				t.Fatalf("Open error = %v, want %v", err, test.want)
			}
			after, err := os.ReadFile(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, corruptPrimary) {
				t.Fatal("ineligible recovery backup replaced the primary database")
			}
			backupAfter, err := os.ReadFile(backupPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(backupAfter, backupBefore) {
				t.Fatal("read-only recovery validation modified the backup artifact")
			}
		})
	}
}

func TestOpenRestoresOlderConsolidatedV2BackupWhenNewestIsIneligible(t *testing.T) {
	root := t.TempDir()
	olderTime := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	olderPath := writeConsolidatedV2RecoveryBackup(t, root, "older.sqlite", olderTime)

	newerPath := filepath.Join(filepath.Dir(olderPath), "newer.sqlite")
	if err := copyFileWithSync(olderPath, newerPath); err != nil {
		t.Fatal(err)
	}
	mutateRecoveryBackup(t, newerPath, func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE store_metadata SET value = 'tampered' WHERE key = ?`, baselineV2MetadataKey)
		return err
	})
	writeRecoveryBackupManifest(t, newerPath, olderTime.Add(time.Minute))

	dbPath := filepath.Join(root, dbFileName)
	if err := os.WriteFile(dbPath, []byte("corrupt primary"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatalf("Open should skip the newer ineligible backup: %v", err)
	}
	defer reopened.Close()
	if err := reopened.validateConsolidatedV2Baseline(); err != nil {
		t.Fatalf("restored database did not come from the eligible V2 backup: %v", err)
	}
}

func writeConsolidatedV2RecoveryBackup(t *testing.T, root, name string, createdAt time.Time) string {
	t.Helper()
	sourceRoot := t.TempDir()
	source, err := Open(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	backups, err := source.ListVerifiedBackups()
	if err != nil {
		_ = source.Close()
		t.Fatal(err)
	}
	if len(backups) != 1 {
		_ = source.Close()
		t.Fatalf("source backup count = %d, want 1", len(backups))
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	backupDir := filepath.Join(root, "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(backupDir, name)
	if err := copyFileWithSync(backups[0].Path, path); err != nil {
		t.Fatal(err)
	}
	writeRecoveryBackupManifest(t, path, createdAt)
	return path
}

func mutateRecoveryBackup(t *testing.T, path string, mutate func(*sql.DB) error) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := mutate(db); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeRecoveryBackupManifest(t *testing.T, path string, createdAt time.Time) {
	t.Helper()
	digest, size, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBackupManifest(path+".json", backupManifest{
		FileName:  filepath.Base(path),
		CreatedAt: createdAt,
		SHA256:    digest,
		SizeBytes: size,
		Reason:    "recovery_fixture",
	}); err != nil {
		t.Fatal(err)
	}
}
