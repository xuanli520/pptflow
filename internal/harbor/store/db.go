package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const (
	schemaVersion = 1
	dbFileName    = "harbor.db"
)

type Store struct {
	db *sql.DB
}

func Open(rootDir string) (*Store, error) {
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" {
		rootDir = ".harbor-factory"
	}
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		return nil, fmt.Errorf("create store directory: %w", err)
	}
	dbPath := filepath.Join(rootDir, dbFileName)
	dsn := "file:" + filepath.ToSlash(dbPath) + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate store: %w", err)
	}
	return s, nil
}

// ResetDatabase removes the disposable SQLite index and its sidecar files.
// Workspace files remain untouched and can rebuild the index on the next sync.
func ResetDatabase(rootDir string) error {
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" {
		rootDir = ".harbor-factory"
	}
	dbPath := filepath.Join(rootDir, dbFileName)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove store database: %w", err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`)
	if err != nil {
		return err
	}
	var current int
	if err := s.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&current); err != nil {
		current = 0
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
	default:
		return fmt.Errorf("unknown migration version %d", version)
	}

	if _, err := tx.Exec("INSERT OR REPLACE INTO schema_version (version) VALUES (?)", version); err != nil {
		return err
	}
	return tx.Commit()
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
