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
	schemaVersion = 18
	dbFileName    = "harbor.db"
)

// ErrReadOnly is returned when a caller tries to mutate a Store opened for a
// no-side-effect preview.
var (
	ErrReadOnly = errors.New("store: read-only")

	// ErrLegacyV1Store rejects databases from the retired workspace-index
	// implementation. Hard cutover never reads, migrates, or rewrites them.
	ErrLegacyV1Store = errors.New("store: V1 database is incompatible with the hard cutover")
)

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
	if err := s.rejectLegacyV1Store(); err != nil {
		_ = db.Close()
		return nil, err
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
	if err := s.rejectLegacyV1Store(); err != nil {
		return err
	}

	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`)
	if err != nil {
		return err
	}
	var current int
	if err := s.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current == 0 {
		return s.bootstrapV2()
	}
	if current < 2 {
		return fmt.Errorf("%w: schema version %d", ErrLegacyV1Store, current)
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

// rejectLegacyV1Store recognizes only schema markers. It never reads V1 rows,
// imports them, or makes a schema change before returning the hard-cutover
// error. A verified pure-V2 backup is the only supported recovery source.
func (s *Store) rejectLegacyV1Store() error {
	legacyTables, err := s.hasLegacyV1Tables()
	if err != nil {
		return err
	}
	if legacyTables {
		return legacyV1StoreError("retired V1 tasks/runs table")
	}
	v1History, err := s.hasV1SchemaHistory()
	if err != nil {
		return err
	}
	if v1History {
		return legacyV1StoreError("schema version 1 history")
	}
	return nil
}

func legacyV1StoreError(marker string) error {
	return fmt.Errorf("%w (%s): restore a verified pure V2 backup or initialize a new control-plane root", ErrLegacyV1Store, marker)
}

// hasLegacyV1Tables identifies the retired workspace-index schema without
// opening or interpreting its rows.
func (s *Store) hasLegacyV1Tables() (bool, error) {
	var count int
	if err := s.db.QueryRow(`
		SELECT COUNT(*)
		FROM sqlite_master
		WHERE type = 'table' AND lower(name) IN ('tasks', 'runs')
	`).Scan(&count); err != nil {
		return false, fmt.Errorf("inspect legacy schema tables: %w", err)
	}
	return count != 0, nil
}

func (s *Store) hasV1SchemaHistory() (bool, error) {
	var schemaVersionTable int
	if err := s.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM sqlite_master
			WHERE type = 'table' AND lower(name) = 'schema_version'
		)
	`).Scan(&schemaVersionTable); err != nil {
		return false, fmt.Errorf("inspect schema version table: %w", err)
	}
	if schemaVersionTable == 0 {
		return false, nil
	}
	var v1History int
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM schema_version WHERE version = 1)`).Scan(&v1History); err != nil {
		return false, fmt.Errorf("read V1 schema history: %w", err)
	}
	return v1History != 0, nil
}

// bootstrapV2 initializes an empty database from the V2 control-plane schema.
// Version 1 is intentionally absent: it belonged to the retired workspace
// index and is never created for a new installation.
func (s *Store) bootstrapV2() error {
	for version := 2; version <= schemaVersion; version++ {
		if err := s.applyMigration(version); err != nil {
			return fmt.Errorf("bootstrap V2 schema version %d: %w", version, err)
		}
	}
	return nil
}

func (s *Store) applyMigration(version int) error {
	if version == 15 {
		return s.applyMigrationV15WithForeignKeysDisabled()
	}
	if version == 17 {
		return s.applyMigrationV17WithForeignKeysDisabled()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	switch version {
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
	case 12:
		if err := applyMigrationV12(tx); err != nil {
			return fmt.Errorf("migration v12: %w", err)
		}
	case 13:
		if err := applyMigrationV13(tx); err != nil {
			return fmt.Errorf("migration v13: %w", err)
		}
	case 14:
		if err := applyMigrationV14(tx); err != nil {
			return fmt.Errorf("migration v14: %w", err)
		}
	case 16:
		if err := applyMigrationV16(tx); err != nil {
			return fmt.Errorf("migration v16: %w", err)
		}
	case 18:
		if err := applyMigrationV18(tx); err != nil {
			return fmt.Errorf("migration v18: %w", err)
		}
	default:
		return fmt.Errorf("unknown migration version %d", version)
	}

	if _, err := tx.Exec("INSERT INTO schema_version (version) VALUES (?)", version); err != nil {
		return err
	}
	return tx.Commit()
}

// applyMigrationV17WithForeignKeysDisabled rebuilds tasks_v2 without the
// retired legacy-identity columns. It follows the same dedicated-connection
// discipline as V15 because current V2 rows can be referenced by durable
// lifecycle records.
func (s *Store) applyMigrationV17WithForeignKeysDisabled() (returnErr error) {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve connection for migration v17: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for migration v17: %w", err)
	}
	restored := false
	restoreForeignKeys := func() error {
		if restored {
			return nil
		}
		restored = true
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
			return fmt.Errorf("restore foreign keys after migration v17: %w", err)
		}
		return nil
	}
	defer func() {
		if err := restoreForeignKeys(); err != nil && returnErr == nil {
			returnErr = err
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration v17: %w", err)
	}
	defer tx.Rollback()
	if err := applyMigrationV17(tx); err != nil {
		return fmt.Errorf("migration v17: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, 17); err != nil {
		return fmt.Errorf("record migration v17: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration v17: %w", err)
	}
	if err := restoreForeignKeys(); err != nil {
		return err
	}
	return nil
}

// applyMigrationV15WithForeignKeysDisabled rebuilds a SQLite table that is a
// parent of pre-existing node, job, and control-operation rows. SQLite's
// ON DELETE RESTRICT action is immediate even when defer_foreign_keys is set,
// so a normal transactional DROP TABLE cannot safely replace stage_attempts.
// This uses one reserved connection, checks every reference before commit,
// and restores FK enforcement before releasing that connection.
func (s *Store) applyMigrationV15WithForeignKeysDisabled() (returnErr error) {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve connection for migration v15: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for migration v15: %w", err)
	}
	restored := false
	restoreForeignKeys := func() error {
		if restored {
			return nil
		}
		restored = true
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
			return fmt.Errorf("restore foreign keys after migration v15: %w", err)
		}
		return nil
	}
	defer func() {
		if err := restoreForeignKeys(); err != nil && returnErr == nil {
			returnErr = err
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration v15: %w", err)
	}
	defer tx.Rollback()
	if err := applyMigrationV15(tx); err != nil {
		return fmt.Errorf("migration v15: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, 15); err != nil {
		return fmt.Errorf("record migration v15: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration v15: %w", err)
	}
	if err := restoreForeignKeys(); err != nil {
		return err
	}
	return nil
}

func normalizeStoreRoot(rootDir string) string {
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" {
		return ".harbor-factory"
	}
	return rootDir
}

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
