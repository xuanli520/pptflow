package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"

	_ "modernc.org/sqlite"
)

const (
	schemaVersion           = 2
	dbFileName              = "harbor.db"
	baselineV2MetadataKey   = "schema_baseline"
	baselineV2MetadataValue = "harbor-workflow-v2-consolidated"

	// baselineV2SchemaContractMetadataKey records the exact SQLite DDL contract
	// produced by migrationV2. Version 2 intentionally has one destructive
	// baseline, so marker/version checks alone are not sufficient to admit a
	// database whose tables, constraints, indexes, or triggers have drifted.
	baselineV2SchemaContractMetadataKey = "schema_contract_fingerprint"
	baselineV2SchemaContractDomain      = "harbor.store.consolidated-v2-schema-contract.v1"

	// This is the one previously published consolidated V2 schema whose
	// Standard authoring handoff trigger still bound 1.2/v1. It is admitted only
	// for the atomic upgrade below; unknown fingerprints remain rejected.
	legacyConsolidatedV2SchemaContractFingerprint = "sha256:db935bd40f92f0d7a9ae4d432b568f5987aebe4d41a5d66f90c55d3fafc53f0b"
	authoringPhase1HandoffTriggerName             = "authoring_phase1_handoffs_v2_binding_insert"
)

type sqliteSchemaContractObject struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Table string `json:"table"`
	SQL   string `json:"sql"`
}

var (
	consolidatedV2SchemaContractOnce        sync.Once
	consolidatedV2SchemaContractFingerprint string
	consolidatedV2SchemaContractErr         error
	testBaselineV2Once                      sync.Once
	testBaselineV2Bytes                     []byte
	testBaselineV2Err                       error
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
	// backupTestMode is set only by OpenForTest. It preserves migration and
	// SQLite integrity admission while omitting backup I/O from application
	// unit fixtures that do not exercise disaster-recovery behavior.
	backupTestMode bool

	backupMu          sync.Mutex
	backupVerifyMu    sync.Mutex
	verifiedBackup    *BackupRecord
	backupStop        chan struct{}
	backupDone        chan struct{}
	backupLoopStarted bool
	backupErrMu       sync.RWMutex
	lastBackupErr     error

	closeOnce sync.Once
	closeErr  error
}

func Open(rootDir string) (*Store, error) {
	return openWritable(rootDir, false)
}

// OpenForTest opens the same writable, migrated, integrity-checked V2 store
// as Open, but deliberately omits automatic and critical-operation backup
// work. It exists for internal application unit fixtures; backup/restore
// tests must use Open so production recovery semantics remain covered.
//
// This is not a production fallback: normal CLI, TUI, worker, and service
// composition continue to call Open and therefore retain verified backups.
func OpenForTest(rootDir string) (*Store, error) {
	var err error
	rootDir, err = normalizeStoreRoot(rootDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		return nil, fmt.Errorf("create test store directory: %w", err)
	}
	dbPath := filepath.Join(rootDir, dbFileName)
	if info, err := os.Lstat(dbPath); errors.Is(err, os.ErrNotExist) {
		if err := seedConsolidatedV2TestBaseline(dbPath); err != nil {
			return nil, fmt.Errorf("seed test V2 baseline: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("inspect test store database: %w", err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("test store database must be a regular non-symlink file: %s", dbPath)
	}
	return openWritable(rootDir, true)
}

// seedConsolidatedV2TestBaseline copies a process-local pristine V2 database
// into a fresh test root. Each copied database still passes through
// openAndMigrate's normal SQLite quick-check and baseline-contract admission;
// the cache only avoids reparsing the large immutable migration SQL for every
// independent application test fixture.
func seedConsolidatedV2TestBaseline(dbPath string) error {
	contents, err := consolidatedV2TestBaselineBytes()
	if err != nil {
		return err
	}
	file, err := os.OpenFile(dbPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	written, writeErr := file.Write(contents)
	if writeErr == nil && written != len(contents) {
		writeErr = io.ErrShortWrite
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(dbPath)
		if writeErr != nil {
			return writeErr
		}
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	}
	return nil
}

func consolidatedV2TestBaselineBytes() ([]byte, error) {
	testBaselineV2Once.Do(func() {
		root, err := os.MkdirTemp("", "harbor-store-v2-test-baseline-")
		if err != nil {
			testBaselineV2Err = err
			return
		}
		defer os.RemoveAll(root)
		dbPath := filepath.Join(root, dbFileName)
		store, err := openAndMigrate(root, dbPath)
		if err != nil {
			testBaselineV2Err = err
			return
		}
		if _, err := store.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			_ = store.db.Close()
			testBaselineV2Err = err
			return
		}
		if err := store.db.Close(); err != nil {
			testBaselineV2Err = err
			return
		}
		contents, err := os.ReadFile(dbPath)
		if err != nil {
			testBaselineV2Err = err
			return
		}
		if len(contents) == 0 {
			testBaselineV2Err = fmt.Errorf("empty consolidated V2 baseline")
			return
		}
		testBaselineV2Bytes = append([]byte(nil), contents...)
	})
	if testBaselineV2Err != nil {
		return nil, testBaselineV2Err
	}
	return append([]byte(nil), testBaselineV2Bytes...), nil
}

func openWritable(rootDir string, backupTestMode bool) (*Store, error) {
	var err error
	rootDir, err = normalizeStoreRoot(rootDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		return nil, fmt.Errorf("create store directory: %w", err)
	}
	dbPath := filepath.Join(rootDir, dbFileName)
	// Do not let the writable WAL connection touch an existing retired or
	// pre-consolidation database merely to discover that it is inadmissible.
	// In particular, SQLite's journal_mode=WAL pragma is persistent and can
	// create sidecars. The immutable preflight below reads only schema markers
	// and integrity metadata; a database it cannot inspect is deliberately
	// left to the established writable-open/corruption-recovery path.
	if err := preflightWritableStoreAdmission(dbPath); err != nil {
		return nil, err
	}
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
	s.backupTestMode = backupTestMode
	if backupTestMode {
		return s, nil
	}
	if _, err := s.BackupIfDue(context.Background()); err != nil {
		_ = s.db.Close()
		return nil, fmt.Errorf("create initial verified backup: %w", err)
	}
	s.startBackupLoop()
	return s, nil
}

// preflightWritableStoreAdmission rejects a readable existing database only
// when its schema markers prove it is a retired V1 or pre-consolidation
// control plane. It intentionally opens immutable/read-only so these
// rejection paths cannot persist journal_mode=WAL, create WAL/SHM files, or
// otherwise rewrite the rejected database. A malformed, locked, or otherwise
// uninspectable file returns nil: openAndMigrate remains the single place that
// classifies corruption and performs verified V2 backup recovery.
func preflightWritableStoreAdmission(dbPath string) error {
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect existing store database: %w", err)
	}

	db, err := openImmutableSQLiteFile(dbPath)
	if err != nil {
		return nil
	}
	defer db.Close()

	// V1 detection is intentionally first and only reads sqlite_master plus
	// schema_version. It must not be replaced by generic V2 admission because
	// callers rely on the precise hard-cutover error for retired databases.
	if err := rejectLegacyV1Database(db); err != nil {
		if errors.Is(err, ErrLegacyV1Store) {
			return err
		}
		return nil
	}

	hasVersionTable, err := hasSchemaVersionTableDatabase(db)
	if err != nil {
		return nil
	}
	if !hasVersionTable {
		var userTableCount int
		if err := db.QueryRow(`
			SELECT COUNT(*)
			FROM sqlite_master
			WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		`).Scan(&userTableCount); err != nil {
			return nil
		}
		// A deliberately empty pre-created SQLite file is still a valid target
		// for the sole V2 bootstrap, exactly as it was before this preflight.
		if userTableCount == 0 {
			return nil
		}
		return preConsolidationStoreError("database has tables but no V2 baseline marker")
	}

	if err := validateConsolidatedV2BaselineDatabase(db, true); err != nil {
		if errors.Is(err, ErrPreConsolidationStore) {
			return err
		}
		return nil
	}

	// A current baseline that cannot pass a physical integrity scan is not
	// rejected as an old schema. Leave that case to openAndMigrate so the
	// existing verified-backup corruption-recovery path remains authoritative.
	if err := verifySQLiteDatabase(db); err != nil {
		return nil
	}
	return nil
}

// OpenReadOnly opens an already-migrated control plane without creating a
// backup, starting maintenance work, or permitting control-plane mutations.
// SQLite may create transient WAL coordination files while serving current
// reads; those files carry no lifecycle record and are not a durable effect.
// It is for preview surfaces whose contract explicitly promises no durable
// side effect.
// A database that needs migration must be opened through Open first.
func OpenReadOnly(rootDir string) (*Store, error) {
	var err error
	rootDir, err = normalizeStoreRoot(rootDir)
	if err != nil {
		return nil, err
	}
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

	dsn, err := sqliteFileURI(dbPath)
	if err != nil {
		return nil, fmt.Errorf("construct read-only SQLite URI: %w", err)
	}
	dsn += "?mode=ro&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
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
	dsn, err := sqliteFileURI(dbPath)
	if err != nil {
		return nil, fmt.Errorf("construct store SQLite URI: %w", err)
	}
	// Every writable Store transaction is a mutation boundary. BEGIN IMMEDIATE
	// acquires SQLite's writer reservation before the first read, preventing a
	// deferred read snapshot from later failing its write upgrade with
	// SQLITE_BUSY_SNAPSHOT under multi-process heartbeat/outbox concurrency.
	dsn += "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate"
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
	if err := validateConsolidatedV2BaselineDatabase(s.db, true); err != nil {
		return err
	}
	if err := s.upgradeLegacyConsolidatedV2Schema(); err != nil {
		return err
	}
	return validateConsolidatedV2BaselineDatabase(s.db, false)
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
	return validateConsolidatedV2BaselineDatabase(s.db, false)
}

// validateConsolidatedV2BaselineDatabase is the shared read-only admission
// check for a control-plane SQLite database. It is used for normal store opens
// and recovery candidates so a backup cannot restore a database that this
// binary would immediately reject.
func validateConsolidatedV2BaselineDatabase(db *sql.DB, allowKnownLegacy bool) error {
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
	expectedContract, err := consolidatedV2SchemaContract()
	if err != nil {
		return fmt.Errorf("derive consolidated V2 schema contract: %w", err)
	}
	var recordedContract string
	if err := db.QueryRow(`SELECT value FROM store_metadata WHERE key = ?`, baselineV2SchemaContractMetadataKey).Scan(&recordedContract); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return preConsolidationStoreError("V2 schema contract marker is absent")
		}
		return preConsolidationStoreError(fmt.Sprintf("read V2 schema contract marker: %v", err))
	}
	actualContract, err := sqliteSchemaContract(db)
	if err != nil {
		return fmt.Errorf("derive persisted V2 schema contract: %w", err)
	}
	if recordedContract == expectedContract && actualContract == expectedContract {
		return nil
	}
	if allowKnownLegacy && recordedContract == legacyConsolidatedV2SchemaContractFingerprint && actualContract == legacyConsolidatedV2SchemaContractFingerprint {
		return nil
	}
	if recordedContract != expectedContract {
		return preConsolidationStoreError("V2 schema contract marker does not match this control plane")
	}
	if actualContract != expectedContract {
		return preConsolidationStoreError("V2 physical schema does not match the consolidated baseline")
	}
	return nil
}

// upgradeLegacyConsolidatedV2Schema repairs the one published V2 contract that
// predates the 1.3/v2 Standard authoring handoff. The trigger replacement and
// contract marker update are one transaction, so an interrupted startup leaves
// the old, internally consistent schema for the next attempt. No unknown
// schema is admitted here.
func (s *Store) upgradeLegacyConsolidatedV2Schema() error {
	var recordedContract string
	if err := s.db.QueryRow(`SELECT value FROM store_metadata WHERE key = ?`, baselineV2SchemaContractMetadataKey).Scan(&recordedContract); err != nil {
		return err
	}
	actualContract, err := sqliteSchemaContract(s.db)
	if err != nil {
		return fmt.Errorf("derive persisted legacy V2 schema contract: %w", err)
	}
	if recordedContract != legacyConsolidatedV2SchemaContractFingerprint || actualContract != legacyConsolidatedV2SchemaContractFingerprint {
		return nil
	}
	triggerSQL, err := currentAuthoringPhase1HandoffTriggerSQL()
	if err != nil {
		return err
	}
	expectedContract, err := consolidatedV2SchemaContract()
	if err != nil {
		return fmt.Errorf("derive upgraded V2 schema contract: %w", err)
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("begin legacy V2 schema upgrade: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DROP TRIGGER IF EXISTS ` + authoringPhase1HandoffTriggerName); err != nil {
		return fmt.Errorf("drop legacy Standard authoring handoff trigger: %w", err)
	}
	if _, err := tx.Exec(triggerSQL); err != nil {
		return fmt.Errorf("install upgraded Standard authoring handoff trigger: %w", err)
	}
	if _, err := tx.Exec(`UPDATE store_metadata SET value = ?, updated_at = ? WHERE key = ?`, expectedContract, s.now().UTC(), baselineV2SchemaContractMetadataKey); err != nil {
		return fmt.Errorf("record upgraded V2 schema contract: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy V2 schema upgrade: %w", err)
	}
	return nil
}

func currentAuthoringPhase1HandoffTriggerSQL() (string, error) {
	startMarker := "CREATE TRIGGER " + authoringPhase1HandoffTriggerName
	start := strings.Index(migrationV2, startMarker)
	if start < 0 {
		return "", fmt.Errorf("current Standard authoring handoff trigger is missing from V2 migration")
	}
	endMarker := "\n\n-- trigger workflow_runs_content_immutable"
	relativeEnd := strings.Index(migrationV2[start:], endMarker)
	if relativeEnd < 0 {
		return "", fmt.Errorf("current Standard authoring handoff trigger boundary is missing from V2 migration")
	}
	return strings.TrimSpace(migrationV2[start : start+relativeEnd]), nil
}

// consolidatedV2SchemaContract derives the canonical DDL fingerprint from a
// fresh in-memory application of migrationV2. Comparing sqlite_master rather
// than source text verifies the schema SQLite actually enforces, including
// table columns/constraints and all named indexes and triggers.
func consolidatedV2SchemaContract() (string, error) {
	consolidatedV2SchemaContractOnce.Do(func() {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			consolidatedV2SchemaContractErr = err
			return
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
			consolidatedV2SchemaContractErr = err
			return
		}
		if _, err := db.Exec(migrationV2); err != nil {
			consolidatedV2SchemaContractErr = err
			return
		}
		consolidatedV2SchemaContractFingerprint, consolidatedV2SchemaContractErr = sqliteSchemaContract(db)
	})
	return consolidatedV2SchemaContractFingerprint, consolidatedV2SchemaContractErr
}

// sqliteSchemaContract returns one domain-separated fingerprint for every
// application-owned schema object. sqlite_master stores each table's complete
// CREATE statement, so its digest covers column shape, foreign keys, CHECK and
// UNIQUE constraints; named indexes and triggers are included separately.
// SQLite-owned autoindexes/statistics tables are intentionally excluded.
func sqliteSchemaContract(db *sql.DB) (string, error) {
	rows, err := db.Query(`
		SELECT type, name, tbl_name, sql
		FROM sqlite_master
		WHERE type IN ('table', 'index', 'trigger', 'view')
		  AND name NOT LIKE 'sqlite_%'
		  AND sql IS NOT NULL
		ORDER BY type, name, tbl_name
	`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	objects := make([]sqliteSchemaContractObject, 0, 128)
	for rows.Next() {
		var object sqliteSchemaContractObject
		if err := rows.Scan(&object.Type, &object.Name, &object.Table, &object.SQL); err != nil {
			return "", err
		}
		object.SQL = strings.TrimSpace(object.SQL)
		if object.Type == "" || object.Name == "" || object.Table == "" || object.SQL == "" {
			return "", fmt.Errorf("sqlite_master returned an incomplete schema object")
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(objects)
	if err != nil {
		return "", err
	}
	fingerprint, err := workflowkit.FingerprintBytes(baselineV2SchemaContractDomain, payload)
	if err != nil {
		return "", err
	}
	return string(fingerprint), nil
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
	contract, err := consolidatedV2SchemaContract()
	if err != nil {
		return fmt.Errorf("derive consolidated V2 schema contract: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO store_metadata (key, value, updated_at)
		VALUES (?, ?, ?)
	`, baselineV2MetadataKey, baselineV2MetadataValue, s.now().UTC()); err != nil {
		return fmt.Errorf("record V2 baseline marker: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO store_metadata (key, value, updated_at)
		VALUES (?, ?, ?)
	`, baselineV2SchemaContractMetadataKey, contract, s.now().UTC()); err != nil {
		return fmt.Errorf("record V2 schema contract marker: %w", err)
	}
	return tx.Commit()
}

func normalizeStoreRoot(rootDir string) (string, error) {
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" {
		rootDir = ".harbor-factory"
	}
	return absoluteCleanPath(rootDir)
}

// sqliteFileURI produces an absolute file URI. modernc SQLite treats a
// relative path in a file: URI as a URI authority on some code paths, so all
// connection modes must normalize it before SQLite sees it.
func sqliteFileURI(path string) (string, error) {
	absolutePath, err := absoluteCleanPath(path)
	if err != nil {
		return "", err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absolutePath)}
	return u.String(), nil
}

func absoluteCleanPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path is required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path %q: %w", path, err)
	}
	return filepath.Clean(absolutePath), nil
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
