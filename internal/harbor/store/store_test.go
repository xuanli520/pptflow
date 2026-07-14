package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func tempDB(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenBootstrapsV2OnlySchema(t *testing.T) {
	s := tempDB(t)
	for _, table := range []string{"tasks", "runs"} {
		var found int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&found); err != nil {
			t.Fatal(err)
		}
		if found != 0 {
			t.Fatalf("V2 bootstrap created retired table %q", table)
		}
	}
	for _, column := range []string{"identity_state", "legacy_v1_task_id", "legacy_identity"} {
		var found int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('tasks_v2') WHERE name = ?`, column).Scan(&found); err != nil {
			t.Fatal(err)
		}
		if found != 0 {
			t.Fatalf("V2 bootstrap retained legacy column %q", column)
		}
	}
	var version, v1History int
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version = 1`).Scan(&v1History); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || v1History != 0 {
		t.Fatalf("V2 bootstrap versions = current:%d v1_rows:%d", version, v1History)
	}
}

func TestOpenReadOnlyPreservesControlPlaneAndRejectsMutations(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writable, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	task, err := writable.CreateTaskV2(ctx, CreateTaskV2Request{Slug: "read-only-fixture", Actor: "tester", Reason: "fixture"})
	if err != nil {
		_ = writable.Close()
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotStoreFiles(t, root)

	readOnly, err := OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := readOnly.GetTaskV2(ctx, task.ID)
	if err != nil || loaded == nil || loaded.ID != task.ID {
		_ = readOnly.Close()
		t.Fatalf("read-only task lookup = %+v, %v", loaded, err)
	}
	if _, err := readOnly.CreateTaskV2(ctx, CreateTaskV2Request{Slug: "must-not-write", Actor: "tester", Reason: "mutation rejection"}); !errors.Is(err, ErrReadOnly) {
		_ = readOnly.Close()
		t.Fatalf("read-only mutation error = %v, want ErrReadOnly", err)
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	after := snapshotStoreFiles(t, root)
	if !reflect.DeepEqual(after, before) {
		t.Fatal("read-only store changed control-plane or backup files")
	}
}

func TestOpenReadOnlyDoesNotInitializeMissingStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent-control-plane")
	if _, err := OpenReadOnly(root); err == nil {
		t.Fatal("read-only open initialized a missing control plane")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only open created root %q: %v", root, err)
	}
}

func TestOpenAndOpenReadOnlyRejectV1StoreMarkers(t *testing.T) {
	tests := []struct {
		name        string
		create      func(*sql.DB) error
		verifyUncut func(*sql.DB) error
	}{
		{
			name: "retired_tables_without_schema_history",
			create: func(db *sql.DB) error {
				if _, err := db.Exec(`CREATE TABLE tasks (id INTEGER PRIMARY KEY)`); err != nil {
					return err
				}
				_, err := db.Exec(`CREATE TABLE runs (id INTEGER PRIMARY KEY, task_id INTEGER)`)
				return err
			},
			verifyUncut: func(db *sql.DB) error {
				var retiredTables, schemaVersionTable int
				if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('tasks', 'runs')`).Scan(&retiredTables); err != nil {
					return err
				}
				if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_version'`).Scan(&schemaVersionTable); err != nil {
					return err
				}
				if retiredTables != 2 || schemaVersionTable != 0 {
					return errors.New("V1 table fixture was changed before rejection")
				}
				return nil
			},
		},
		{
			name: "version_one_history_without_retired_tables",
			create: func(db *sql.DB) error {
				if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
					return err
				}
				_, err := db.Exec(`INSERT INTO schema_version (version) VALUES (1)`)
				return err
			},
			verifyUncut: func(db *sql.DB) error {
				var v1Rows, retiredTables int
				if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version = 1`).Scan(&v1Rows); err != nil {
					return err
				}
				if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('tasks', 'runs')`).Scan(&retiredTables); err != nil {
					return err
				}
				if v1Rows != 1 || retiredTables != 0 {
					return errors.New("V1 history fixture was changed before rejection")
				}
				return nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dbPath := filepath.Join(root, dbFileName)
			db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
			if err != nil {
				t.Fatal(err)
			}
			if err := test.create(db); err != nil {
				_ = db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			if _, err := Open(root); !errors.Is(err, ErrLegacyV1Store) {
				t.Fatalf("Open legacy fixture error = %v, want ErrLegacyV1Store", err)
			}
			if _, err := OpenReadOnly(root); !errors.Is(err, ErrLegacyV1Store) {
				t.Fatalf("OpenReadOnly legacy fixture error = %v, want ErrLegacyV1Store", err)
			}

			db, err = sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := test.verifyUncut(db); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMigratePureV16StoreRemovesRetiredIdentityColumns(t *testing.T) {
	root := t.TempDir()
	taskID, revisionID, runID := createPureV16StoreWithRetiredIdentityColumns(t, root)

	s, err := Open(root)
	if err != nil {
		t.Fatalf("migrate pure V16 store: %v", err)
	}
	defer s.Close()

	for _, column := range []string{"identity_state", "legacy_v1_task_id", "legacy_identity"} {
		var found int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('tasks_v2') WHERE name = ?`, column).Scan(&found); err != nil {
			t.Fatal(err)
		}
		if found != 0 {
			t.Fatalf("V17 retained retired column %q", column)
		}
	}
	if task, err := s.GetTaskV2(context.Background(), taskID); err != nil || task == nil || task.ID != taskID || task.CurrentRevisionID != revisionID {
		t.Fatalf("migrated task = %+v, %v", task, err)
	}
	if revision, err := s.GetTaskRevision(context.Background(), revisionID); err != nil || revision == nil || revision.TaskID != taskID {
		t.Fatalf("migrated revision = %+v, %v", revision, err)
	}
	if run, err := s.GetWorkflowRun(context.Background(), runID); err != nil || run == nil || run.TaskID != taskID || run.RevisionID != revisionID {
		t.Fatalf("migrated run = %+v, %v", run, err)
	}
	rows, err := s.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("V17 migration left a foreign-key violation")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var version, triggerCount int
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = 'task_purge_v7_blocks_task_mutation'`).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || triggerCount != 1 {
		t.Fatalf("V17 migration state = version:%d purge_trigger:%d", version, triggerCount)
	}
}

func createPureV16StoreWithRetiredIdentityColumns(t *testing.T, root string) (string, string, string) {
	t.Helper()
	dbPath := filepath.Join(root, dbFileName)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrationV2); err != nil {
		t.Fatalf("create V2 fixture: %v", err)
	}
	for _, statement := range []string{
		`ALTER TABLE tasks_v2 ADD COLUMN identity_state TEXT NOT NULL DEFAULT 'canonical' CHECK (identity_state IN ('canonical', 'legacy_orphan'))`,
		`ALTER TABLE tasks_v2 ADD COLUMN legacy_v1_task_id INTEGER`,
		`ALTER TABLE tasks_v2 ADD COLUMN legacy_identity TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX idx_tasks_v2_identity_state ON tasks_v2(identity_state)`,
		`CREATE UNIQUE INDEX idx_tasks_v2_canonical_import_identity ON tasks_v2(legacy_identity, source_repo, source_commit) WHERE identity_state = 'canonical' AND legacy_identity <> ''`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("extend V2 fixture with retired identity fields: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (2)`); err != nil {
		t.Fatal(err)
	}
	fixture := &Store{db: db}
	for version := 3; version <= 16; version++ {
		if err := fixture.applyMigration(version); err != nil {
			t.Fatalf("apply V%d fixture migration: %v", version, err)
		}
	}

	taskID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	revisionID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	runID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.Exec(`
		INSERT INTO tasks_v2 (
			id, slug, title, metadata_json, source_repo, source_commit,
			lifecycle_state, current_revision_id, identity_state, legacy_v1_task_id,
			legacy_identity, created_at, updated_at, deleted_at, version
		) VALUES (?, 'v16-task', '', '{}', 'https://example.invalid/repo', 'commit', 'ready', ?, 'canonical', 42, 'retired-identity', ?, ?, NULL, 1)
	`, taskID, revisionID, now, now); err != nil {
		t.Fatalf("insert V16 task fixture: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO task_revisions (
			id, task_id, version_number, parent_revision_id, origin, task_digest,
			proposal_digest, manifest_id, state, validation_evidence_manifest, state_version,
			state_updated_by, state_updated_at, change_summary, metadata_json, created_by, created_at
		) VALUES (?, ?, 1, NULL, 'manual', ?, '', '', 'sealed', '', 1, 'fixture', ?, '', '{}', 'fixture', ?)
	`, revisionID, taskID, validTaskDigest("a"), now, now); err != nil {
		t.Fatalf("insert V16 task revision fixture: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_runs (
			id, task_id, revision_id, workflow_template_id, workflow_template_version,
			resolved_profile_hash, definition_hash, run_manifest_json, trigger, created_by, created_at
		) VALUES (?, ?, ?, 'fixture.workflow', 'v1', 'profile', 'definition', '{}', 'fixture', 'fixture', ?)
	`, runID, taskID, revisionID, now); err != nil {
		t.Fatalf("insert V16 workflow run fixture: %v", err)
	}
	return taskID, revisionID, runID
}

func snapshotStoreFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if path == filepath.Join(root, dbFileName+"-wal") || path == filepath.Join(root, dbFileName+"-shm") {
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("store snapshot encountered non-regular file")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = content
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
