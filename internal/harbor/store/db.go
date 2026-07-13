package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	schemaVersion = 11
	dbFileName    = "harbor.db"
)

// ErrReadOnly is returned when a caller tries to mutate a Store opened for a
// no-side-effect preview.
var ErrReadOnly = errors.New("store: read-only")

type Store struct {
	db        *sql.DB
	rootDir   string
	dbPath    string
	backupDir string
	now       func() time.Time
	readOnly  bool

	backupMu          sync.Mutex
	backupStop        chan struct{}
	backupDone        chan struct{}
	backupLoopStarted bool
	backupErrMu       sync.RWMutex
	lastBackupErr     error

	closeOnce sync.Once
	closeErr  error
}

func Open(rootDir string) (*Store, error) {
	rootDir = normalizeStoreRoot(rootDir)
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		return nil, fmt.Errorf("create store directory: %w", err)
	}
	dbPath := filepath.Join(rootDir, dbFileName)
	s, err := openAndMigrate(rootDir, dbPath)
	if err != nil && isSQLiteCorruption(err) {
		if restoreErr := restoreLatestVerifiedBackup(rootDir, dbPath); restoreErr != nil {
			return nil, fmt.Errorf("open corrupt store and restore latest verified backup: %w", restoreErr)
		}
		s, err = openAndMigrate(rootDir, dbPath)
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.BackupIfDue(context.Background()); err != nil {
		_ = s.db.Close()
		return nil, fmt.Errorf("create initial verified backup: %w", err)
	}
	s.startBackupLoop()
	return s, nil
}

// OpenReadOnly opens an already-migrated control plane without creating a
// backup, starting maintenance work, or permitting control-plane mutations.
// SQLite may create transient WAL coordination files while serving current
// reads; those files carry no lifecycle record and are not a durable effect.
// It is for preview surfaces whose contract explicitly promises no durable
// side effect.
// A database that needs migration must be opened through Open first.
func OpenReadOnly(rootDir string) (*Store, error) {
	rootDir = normalizeStoreRoot(rootDir)
	dbPath := filepath.Join(rootDir, dbFileName)
	info, err := os.Stat(dbPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("open read-only store: database does not exist: %s", dbPath)
		}
		return nil, fmt.Errorf("inspect read-only store: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("open read-only store: database is not a regular file: %s", dbPath)
	}

	u := url.URL{Scheme: "file", Path: filepath.ToSlash(dbPath)}
	dsn := u.String() + "?mode=ro&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read-only store: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{
		db:         db,
		rootDir:    rootDir,
		dbPath:     dbPath,
		backupDir:  filepath.Join(rootDir, "backups"),
		now:        time.Now,
		readOnly:   true,
		backupStop: make(chan struct{}),
		backupDone: make(chan struct{}),
	}
	if err := s.validateReadOnlySchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := verifySQLiteDatabase(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("verify read-only store: %w", err)
	}
	return s, nil
}

func (s *Store) validateReadOnlySchema() error {
	var current int
	if err := s.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&current); err != nil {
		return fmt.Errorf("read read-only store schema: %w", err)
	}
	if current != schemaVersion {
		return fmt.Errorf("read-only store schema %d is not current schema %d", current, schemaVersion)
	}
	return nil
}

func (s *Store) requireWritable() error {
	if s != nil && s.readOnly {
		return ErrReadOnly
	}
	return nil
}

func openAndMigrate(rootDir, dbPath string) (*Store, error) {
	dsn := "file:" + filepath.ToSlash(dbPath) + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{
		db:         db,
		rootDir:    rootDir,
		dbPath:     dbPath,
		backupDir:  filepath.Join(rootDir, "backups"),
		now:        time.Now,
		backupStop: make(chan struct{}),
		backupDone: make(chan struct{}),
	}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate store: %w", err)
	}
	if err := verifySQLiteDatabase(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("verify store: %w", err)
	}
	return s, nil
}

// ResetDatabase is retained only for legacy V1 index callers. V2 SQLite state
// is authoritative and corruption is recovered through verified backups in
// Open; new lifecycle code must not call this destructive compatibility API.
//
// Deprecated: use automatic verified-backup recovery instead.
func ResetDatabase(rootDir string) error {
	rootDir = normalizeStoreRoot(rootDir)
	dbPath := filepath.Join(rootDir, dbFileName)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove store database: %w", err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		if s.backupLoopStarted {
			close(s.backupStop)
			<-s.backupDone
		}
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`)
	if err != nil {
		return err
	}
	var current int
	if err := s.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > schemaVersion {
		return fmt.Errorf("store schema %d is newer than supported schema %d", current, schemaVersion)
	}
	if current > 0 && current < schemaVersion {
		if _, err := s.BackupBeforeCriticalOperation(context.Background(), "schema_migration"); err != nil {
			return fmt.Errorf("backup before schema migration: %w", err)
		}
	}
	for v := current + 1; v <= schemaVersion; v++ {
		if err := s.applyMigration(v); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyMigration(version int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	switch version {
	case 1:
		if _, err := tx.Exec(migrationV1); err != nil {
			return fmt.Errorf("migration v1: %w", err)
		}
	case 2:
		if _, err := tx.Exec(migrationV2); err != nil {
			return fmt.Errorf("migration v2: %w", err)
		}
	case 3:
		if _, err := tx.Exec(migrationV3); err != nil {
			return fmt.Errorf("migration v3: %w", err)
		}
	case 4:
		if _, err := tx.Exec(migrationV4); err != nil {
			return fmt.Errorf("migration v4: %w", err)
		}
	case 5:
		if _, err := tx.Exec(migrationV5); err != nil {
			return fmt.Errorf("migration v5: %w", err)
		}
	case 6:
		if err := applyMigrationV6(tx); err != nil {
			return fmt.Errorf("migration v6: %w", err)
		}
	case 7:
		if err := applyMigrationV7(tx); err != nil {
			return fmt.Errorf("migration v7: %w", err)
		}
	case 8:
		if err := applyMigrationV8(tx); err != nil {
			return fmt.Errorf("migration v8: %w", err)
		}
	case 9:
		if err := applyMigrationV9(tx); err != nil {
			return fmt.Errorf("migration v9: %w", err)
		}
	case 10:
		if err := applyMigrationV10(tx); err != nil {
			return fmt.Errorf("migration v10: %w", err)
		}
	case 11:
		if err := applyMigrationV11(tx); err != nil {
			return fmt.Errorf("migration v11: %w", err)
		}
	default:
		return fmt.Errorf("unknown migration version %d", version)
	}

	if _, err := tx.Exec("INSERT INTO schema_version (version) VALUES (?)", version); err != nil {
		return err
	}
	return tx.Commit()
}

func normalizeStoreRoot(rootDir string) string {
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" {
		return ".harbor-factory"
	}
	return rootDir
}

const migrationV1 = `
CREATE TABLE IF NOT EXISTS tasks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    task_dir     TEXT NOT NULL UNIQUE,
    task_name    TEXT NOT NULL DEFAULT '',
    code_lang    TEXT NOT NULL DEFAULT '',
    task_type    TEXT NOT NULL DEFAULT '',
    application  TEXT NOT NULL DEFAULT '',
    repo_url     TEXT NOT NULL DEFAULT '',
    commit_sha   TEXT NOT NULL DEFAULT '',
    is_generated BOOLEAN NOT NULL DEFAULT 0,
    first_seen   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_tasks_name ON tasks(task_name);
CREATE INDEX IF NOT EXISTS idx_tasks_lang ON tasks(code_lang);

CREATE TABLE IF NOT EXISTS runs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id         INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    workspace_path  TEXT NOT NULL UNIQUE,
    run_id          TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'unknown',
    passed          BOOLEAN NOT NULL DEFAULT 0,
    started_at      DATETIME,
    finished_at     DATETIME,
    size_bytes      INTEGER NOT NULL DEFAULT 0,
    is_active       BOOLEAN NOT NULL DEFAULT 0,
    is_resumable    BOOLEAN NOT NULL DEFAULT 0,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_runs_task    ON runs(task_id);
CREATE INDEX IF NOT EXISTS idx_runs_status  ON runs(status);
CREATE INDEX IF NOT EXISTS idx_runs_started ON runs(started_at DESC);
`

func sanitizeText(s string) string {
	return strings.TrimSpace(s)
}

func normalizePath(path string) string {
	path = sanitizeText(path)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(abs)
}
