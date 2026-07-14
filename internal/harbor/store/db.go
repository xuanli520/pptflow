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
	schemaVersion           = 2
	dbFileName              = "harbor.db"
	baselineV2MetadataKey   = "schema_baseline"
	baselineV2MetadataValue = "harbor-workflow-v2-consolidated"
)

// ErrReadOnly is returned when a caller tries to mutate a Store opened for a
// no-side-effect preview.
var (
	ErrReadOnly = errors.New("store: read-only")

	// ErrLegacyV1Store rejects databases from the retired workspace-index
	// implementation. Hard cutover never reads, migrates, or rewrites them.
	ErrLegacyV1Store = errors.New("store: V1 database is incompatible with the hard cutover")

	// ErrPreConsolidationStore rejects a database created by the retired
	// incremental V2-V20 migration chain. The consolidated V2 baseline is
	// deliberately destructive relative to that development-only history, so
	// operators must create a new control-plane root instead of asking this
	// binary to reinterpret or rewrite prior data.
	ErrPreConsolidationStore = errors.New("store: pre-consolidation database requires rebuild")
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
	return s.validateConsolidatedV2Baseline()
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
	hasVersionTable, err := s.hasSchemaVersionTable()
	if err != nil {
		return err
	}
	if !hasVersionTable {
		hasTables, err := s.hasUserTables()
		if err != nil {
			return err
		}
		if hasTables {
			return preConsolidationStoreError("database has tables but no V2 baseline marker")
		}
		return s.bootstrapV2()
	}
	return s.validateConsolidatedV2Baseline()
}

// rejectLegacyV1Store recognizes only schema markers. It never reads V1 rows,
// imports them, or makes a schema change before returning the hard-cutover
// error. A verified pure-V2 backup is the only supported recovery source.
func (s *Store) rejectLegacyV1Store() error {
	return rejectLegacyV1Database(s.db)
}

// rejectLegacyV1Database recognizes only schema markers. It never reads V1
// rows, imports them, or makes a schema change before returning the
// hard-cutover error. A verified pure-V2 backup is the only supported recovery
// source.
func rejectLegacyV1Database(db *sql.DB) error {
	legacyTables, err := hasLegacyV1TablesDatabase(db)
	if err != nil {
		return err
	}
	if legacyTables {
		return legacyV1StoreError("retired V1 tasks/runs table")
	}
	v1History, err := hasV1SchemaHistoryDatabase(db)
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
func hasLegacyV1TablesDatabase(db *sql.DB) (bool, error) {
	var count int
	if err := db.QueryRow(`
		SELECT COUNT(*)
		FROM sqlite_master
		WHERE type = 'table' AND lower(name) IN ('tasks', 'runs')
	`).Scan(&count); err != nil {
		return false, fmt.Errorf("inspect legacy schema tables: %w", err)
	}
	return count != 0, nil
}

func hasV1SchemaHistoryDatabase(db *sql.DB) (bool, error) {
	hasTable, err := hasSchemaVersionTableDatabase(db)
	if err != nil {
		return false, err
	}
	if !hasTable {
		return false, nil
	}
	var v1History int
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM schema_version WHERE version = 1)`).Scan(&v1History); err != nil {
		return false, fmt.Errorf("read V1 schema history: %w", err)
	}
	return v1History != 0, nil
}

func (s *Store) hasSchemaVersionTable() (bool, error) {
	return hasSchemaVersionTableDatabase(s.db)
}

func hasSchemaVersionTableDatabase(db *sql.DB) (bool, error) {
	var exists int
	if err := db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM sqlite_master
			WHERE type = 'table' AND lower(name) = 'schema_version'
		)
	`).Scan(&exists); err != nil {
		return false, fmt.Errorf("inspect schema version table: %w", err)
	}
	return exists != 0, nil
}

func (s *Store) hasUserTables() (bool, error) {
	var count int
	if err := s.db.QueryRow(`
		SELECT COUNT(*)
		FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
	`).Scan(&count); err != nil {
		return false, fmt.Errorf("inspect database tables: %w", err)
	}
	return count != 0, nil
}

// validateConsolidatedV2Baseline accepts only a database created by the sole
// V2 bootstrap. A bare historical "version = 2" row is not sufficient: that
// was the first step of the retired incremental chain and has a different
// physical schema. This check is deliberately read-only.
func (s *Store) validateConsolidatedV2Baseline() error {
	return validateConsolidatedV2BaselineDatabase(s.db)
}

// validateConsolidatedV2BaselineDatabase is the shared read-only admission
// check for a control-plane SQLite database. It is used for normal store opens
// and recovery candidates so a backup cannot restore a database that this
// binary would immediately reject.
func validateConsolidatedV2BaselineDatabase(db *sql.DB) error {
	hasTable, err := hasSchemaVersionTableDatabase(db)
	if err != nil {
		return err
	}
	if !hasTable {
		return preConsolidationStoreError("schema_version table is absent")
	}
	rows, err := db.Query(`SELECT version FROM schema_version ORDER BY version`)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	defer rows.Close()
	versions := make([]int, 0, 1)
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return fmt.Errorf("scan schema version: %w", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate schema version: %w", err)
	}
	if len(versions) != 1 || versions[0] != schemaVersion {
		return preConsolidationStoreError(fmt.Sprintf("schema history %v is not the sole V2 baseline", versions))
	}

	var marker string
	if err := db.QueryRow(`SELECT value FROM store_metadata WHERE key = ?`, baselineV2MetadataKey).Scan(&marker); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return preConsolidationStoreError("V2 baseline marker is absent")
		}
		return preConsolidationStoreError(fmt.Sprintf("read V2 baseline marker: %v", err))
	}
	if marker != baselineV2MetadataValue {
		return preConsolidationStoreError("V2 baseline marker does not match this control plane")
	}
	for _, table := range []string{
		"entity_id_registry",
		"lifecycle_operations_v12",
		"run_input_artifacts",
		"trial_executions_v19",
		"trial_attempts_v19",
		"codeedge_compliance_records_v20",
	} {
		var exists int
		if err := db.QueryRow(`
			SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)
		`, table).Scan(&exists); err != nil {
			return fmt.Errorf("inspect V2 baseline table %s: %w", table, err)
		}
		if exists == 0 {
			return preConsolidationStoreError(fmt.Sprintf("V2 baseline table %s is absent", table))
		}
	}
	return nil
}

func preConsolidationStoreError(marker string) error {
	return fmt.Errorf("%w (%s): initialize a new control-plane root; this binary will not migrate or rewrite an older database", ErrPreConsolidationStore, marker)
}

// bootstrapV2 initializes an empty database from the one consolidated V2
// control-plane schema. Version 1 and V3-V20 history are intentionally never
// created: the transaction records only the final V2 baseline marker.
func (s *Store) bootstrapV2() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(migrationV2); err != nil {
		return fmt.Errorf("bootstrap consolidated V2 schema: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, schemaVersion); err != nil {
		return fmt.Errorf("record V2 baseline schema version: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO store_metadata (key, value, updated_at)
		VALUES (?, ?, ?)
	`, baselineV2MetadataKey, baselineV2MetadataValue, s.now().UTC()); err != nil {
		return fmt.Errorf("record V2 baseline marker: %w", err)
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
