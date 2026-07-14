package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
	var version, versionRows, v1History int
	if err := s.db.QueryRow(`SELECT MAX(version), COUNT(*) FROM schema_version`).Scan(&version, &versionRows); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version = 1`).Scan(&v1History); err != nil {
		t.Fatal(err)
	}
	var marker string
	if err := s.db.QueryRow(`SELECT value FROM store_metadata WHERE key = ?`, baselineV2MetadataKey).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || versionRows != 1 || v1History != 0 || marker != baselineV2MetadataValue {
		t.Fatalf("V2 bootstrap state = current:%d rows:%d v1_rows:%d marker:%q", version, versionRows, v1History, marker)
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

func TestOpenAndOpenReadOnlyRejectPreConsolidationStoresWithoutMutation(t *testing.T) {
	tests := []struct {
		name   string
		create func(*sql.DB) error
		verify func(*sql.DB) error
	}{
		{
			name: "historical_version_twenty",
			create: func(db *sql.DB) error {
				if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
					return err
				}
				_, err := db.Exec(`INSERT INTO schema_version (version) VALUES (20)`)
				return err
			},
			verify: func(db *sql.DB) error {
				var version, metadataTables int
				if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
					return err
				}
				if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'store_metadata'`).Scan(&metadataTables); err != nil {
					return err
				}
				if version != 20 || metadataTables != 0 {
					return errors.New("historical V20 fixture was rewritten")
				}
				return nil
			},
		},
		{
			name: "unmarked_historical_version_two",
			create: func(db *sql.DB) error {
				if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
					return err
				}
				_, err := db.Exec(`INSERT INTO schema_version (version) VALUES (2)`)
				return err
			},
			verify: func(db *sql.DB) error {
				var version, metadataTables int
				if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
					return err
				}
				if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'store_metadata'`).Scan(&metadataTables); err != nil {
					return err
				}
				if version != 2 || metadataTables != 0 {
					return errors.New("historical V2 fixture was rewritten")
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

			if _, err := Open(root); !errors.Is(err, ErrPreConsolidationStore) {
				t.Fatalf("Open pre-consolidation fixture error = %v, want ErrPreConsolidationStore", err)
			}
			if _, err := OpenReadOnly(root); !errors.Is(err, ErrPreConsolidationStore) {
				t.Fatalf("OpenReadOnly pre-consolidation fixture error = %v, want ErrPreConsolidationStore", err)
			}

			db, err = sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := test.verify(db); err != nil {
				t.Fatal(err)
			}
		})
	}
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
