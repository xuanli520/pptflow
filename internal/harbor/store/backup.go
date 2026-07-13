package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const verifiedBackupInterval = 15 * time.Minute

// BackupRecord describes a SQLite snapshot that passed both checksum and
// SQLite integrity verification. Path is local to this installation.
type BackupRecord struct {
	Path      string
	CreatedAt time.Time
	SHA256    string
	SizeBytes int64
	Reason    string
}

type backupManifest struct {
	FileName  string    `json:"file_name"`
	CreatedAt time.Time `json:"created_at"`
	SHA256    string    `json:"sha256"`
	SizeBytes int64     `json:"size_bytes"`
	Reason    string    `json:"reason"`
}

// BackupIfDue creates a verified snapshot when the latest verified snapshot
// is at least fifteen minutes old. It returns nil when no new backup is due.
func (s *Store) BackupIfDue(ctx context.Context) (*BackupRecord, error) {
	if err := s.requireWritable(); err != nil {
		return nil, err
	}
	backups, err := listVerifiedBackups(s.backupDir)
	if err != nil {
		return nil, err
	}
	if len(backups) > 0 && s.now().UTC().Sub(backups[0].CreatedAt) < verifiedBackupInterval {
		return nil, nil
	}
	record, err := s.BackupNow(ctx, "interval")
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// BackupNow always writes a new verified SQLite snapshot. It is safe to call
// concurrently with repository operations; SQLite provides the snapshot.
func (s *Store) BackupNow(ctx context.Context, reason string) (BackupRecord, error) {
	if err := s.requireWritable(); err != nil {
		return BackupRecord{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "manual"
	}

	s.backupMu.Lock()
	defer s.backupMu.Unlock()

	if err := os.MkdirAll(s.backupDir, 0o700); err != nil {
		return BackupRecord{}, fmt.Errorf("create backup directory: %w", err)
	}
	now := s.now().UTC()
	id, err := newUUIDv7(now)
	if err != nil {
		return BackupRecord{}, err
	}
	name := "harbor-" + now.Format("20060102T150405.000000000Z") + "-" + id + ".sqlite"
	path := filepath.Join(s.backupDir, name)
	tmpPath := path + ".tmp"
	_ = os.Remove(tmpPath)

	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return BackupRecord{}, fmt.Errorf("snapshot SQLite database: %w", err)
	}
	if err := verifySQLiteFile(tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return BackupRecord{}, fmt.Errorf("verify snapshot: %w", err)
	}
	digest, size, err := fileSHA256(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return BackupRecord{}, fmt.Errorf("hash snapshot: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return BackupRecord{}, fmt.Errorf("protect snapshot: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return BackupRecord{}, fmt.Errorf("commit snapshot: %w", err)
	}
	if err := syncDirectory(s.backupDir); err != nil {
		return BackupRecord{}, fmt.Errorf("sync snapshot directory: %w", err)
	}
	record := BackupRecord{
		Path:      path,
		CreatedAt: now,
		SHA256:    digest,
		SizeBytes: size,
		Reason:    reason,
	}
	manifest := backupManifest{
		FileName:  filepath.Base(path),
		CreatedAt: record.CreatedAt,
		SHA256:    record.SHA256,
		SizeBytes: record.SizeBytes,
		Reason:    record.Reason,
	}
	if err := writeBackupManifest(path+".json", manifest); err != nil {
		_ = os.Remove(path)
		return BackupRecord{}, fmt.Errorf("write snapshot manifest: %w", err)
	}
	return record, nil
}

// BackupBeforeCriticalOperation is the mandatory preflight for operations
// that can rewrite control-plane state or alter durable user-visible history.
func (s *Store) BackupBeforeCriticalOperation(ctx context.Context, operation string) (BackupRecord, error) {
	operation = strings.TrimSpace(operation)
	if operation == "" {
		return BackupRecord{}, fmt.Errorf("critical operation name is required")
	}
	return s.BackupNow(ctx, "critical:"+operation)
}

// WithCriticalOperation keeps the backup protocol close to the state-changing
// operation. The callback is intentionally supplied by the caller so it can
// own its transaction and expected-version semantics.
func (s *Store) WithCriticalOperation(ctx context.Context, operation string, fn func(context.Context) error) error {
	if fn == nil {
		return fmt.Errorf("critical operation callback is required")
	}
	if _, err := s.BackupBeforeCriticalOperation(ctx, operation); err != nil {
		return err
	}
	return fn(ctx)
}

// ListVerifiedBackups exposes only snapshots whose manifest checksum and
// SQLite integrity check still pass. Corrupt backups are ignored, never used.
func (s *Store) ListVerifiedBackups() ([]BackupRecord, error) {
	return listVerifiedBackups(s.backupDir)
}

// LastBackupError reports a background interval-backup failure. Synchronous
// callers still receive their own error from BackupIfDue or BackupNow.
func (s *Store) LastBackupError() error {
	s.backupErrMu.RLock()
	defer s.backupErrMu.RUnlock()
	return s.lastBackupErr
}

func (s *Store) startBackupLoop() {
	s.backupLoopStarted = true
	go func() {
		ticker := time.NewTicker(verifiedBackupInterval)
		defer ticker.Stop()
		defer close(s.backupDone)
		for {
			select {
			case <-s.backupStop:
				return
			case <-ticker.C:
				_, err := s.BackupIfDue(context.Background())
				s.backupErrMu.Lock()
				s.lastBackupErr = err
				s.backupErrMu.Unlock()
			}
		}
	}()
}

func restoreLatestVerifiedBackup(rootDir, dbPath string) error {
	backups, err := listVerifiedBackups(filepath.Join(rootDir, "backups"))
	if err != nil {
		return err
	}
	if len(backups) == 0 {
		return fmt.Errorf("no verified backup is available")
	}

	tmpPath := dbPath + ".restore.tmp"
	if err := copyFileWithSync(backups[0].Path, tmpPath); err != nil {
		return fmt.Errorf("copy verified backup: %w", err)
	}
	defer os.Remove(tmpPath)
	if err := verifySQLiteFile(tmpPath); err != nil {
		return fmt.Errorf("verify restored database: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale SQLite sidecar: %w", err)
		}
	}
	if err := os.Rename(tmpPath, dbPath); err != nil {
		return fmt.Errorf("activate restored database: %w", err)
	}
	if err := syncDirectory(filepath.Dir(dbPath)); err != nil {
		return fmt.Errorf("sync restored database directory: %w", err)
	}
	return nil
}

func listVerifiedBackups(backupDir string) ([]BackupRecord, error) {
	entries, err := os.ReadDir(backupDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read backup directory: %w", err)
	}
	backups := make([]BackupRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sqlite.json") {
			continue
		}
		manifestPath := filepath.Join(backupDir, entry.Name())
		manifest, err := readBackupManifest(manifestPath)
		if err != nil {
			continue
		}
		if filepath.Base(manifest.FileName) != manifest.FileName || !strings.HasSuffix(manifest.FileName, ".sqlite") {
			continue
		}
		path := filepath.Join(backupDir, manifest.FileName)
		digest, size, err := fileSHA256(path)
		if err != nil || digest != manifest.SHA256 || size != manifest.SizeBytes {
			continue
		}
		if err := verifySQLiteFile(path); err != nil {
			continue
		}
		backups = append(backups, BackupRecord{
			Path:      path,
			CreatedAt: manifest.CreatedAt.UTC(),
			SHA256:    manifest.SHA256,
			SizeBytes: manifest.SizeBytes,
			Reason:    manifest.Reason,
		})
	}
	sort.Slice(backups, func(i, j int) bool {
		if backups[i].CreatedAt.Equal(backups[j].CreatedAt) {
			return backups[i].Path > backups[j].Path
		}
		return backups[i].CreatedAt.After(backups[j].CreatedAt)
	})
	return backups, nil
}

func writeBackupManifest(path string, manifest backupManifest) error {
	payload, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, payload, 0o600); err != nil {
		return err
	}
	if err := syncFile(tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func readBackupManifest(path string) (backupManifest, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return backupManifest{}, err
	}
	var manifest backupManifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		return backupManifest{}, err
	}
	if manifest.FileName == "" || manifest.CreatedAt.IsZero() || manifest.SHA256 == "" || manifest.SizeBytes <= 0 {
		return backupManifest{}, fmt.Errorf("incomplete backup manifest")
	}
	return manifest, nil
}

func verifySQLiteFile(path string) error {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	db, err := sql.Open("sqlite", u.String()+"?mode=ro&immutable=1&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	return verifySQLiteDatabase(db)
}

func verifySQLiteDatabase(db *sql.DB) error {
	rows, err := db.Query("PRAGMA quick_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return err
		}
		if strings.TrimSpace(strings.ToLower(result)) != "ok" {
			return fmt.Errorf("integrity check failed: %s", result)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	fkRows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer fkRows.Close()
	if fkRows.Next() {
		return fmt.Errorf("foreign key check failed")
	}
	return fkRows.Err()
}

func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

func copyFileWithSync(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func isSQLiteCorruption(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"database disk image is malformed",
		"database malformed",
		"file is not a database",
		"database corruption",
		"database corrupt",
		"sqlite_corrupt",
		"integrity check failed",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
