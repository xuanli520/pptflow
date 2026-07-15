package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
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
	var marker, contract string
	if err := s.db.QueryRow(`SELECT value FROM store_metadata WHERE key = ?`, baselineV2MetadataKey).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT value FROM store_metadata WHERE key = ?`, baselineV2SchemaContractMetadataKey).Scan(&contract); err != nil {
		t.Fatal(err)
	}
	expectedContract, err := consolidatedV2SchemaContract()
	if err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || versionRows != 1 || v1History != 0 || marker != baselineV2MetadataValue || contract != expectedContract {
		t.Fatalf("V2 bootstrap state = current:%d rows:%d v1_rows:%d marker:%q contract:%q", version, versionRows, v1History, marker, contract)
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

func TestOpenForTestPreservesStoreIntegrityWithoutBackupFixtureIO(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}

	backups, err := s.ListVerifiedBackups()
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	if len(backups) != 0 {
		_ = s.Close()
		t.Fatalf("OpenForTest created automatic backups: %+v", backups)
	}
	if _, err := s.CreateTaskV2(ctx, CreateTaskV2Request{Slug: "fixture-without-backup-io", Actor: "tester", Reason: "unit fixture"}); err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	backups, err = s.ListVerifiedBackups()
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	if len(backups) != 0 {
		_ = s.Close()
		t.Fatalf("OpenForTest mutation unexpectedly created backups: %+v", backups)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// OpenForTest is a fixture-only optimization. Returning to the production
	// entry point on the same database must still create the initial verified
	// backup and start the ordinary recovery protection behavior.
	production, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer production.Close()
	backups, err = production.ListVerifiedBackups()
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 || backups[0].Reason != "interval" {
		t.Fatalf("Open after OpenForTest did not create the production backup: %+v", backups)
	}
}

func TestOpenForTestBaselineBytesAreDefensivelyIsolated(t *testing.T) {
	baseline, err := consolidatedV2TestBaselineBytes()
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline) == 0 {
		t.Fatal("test baseline is empty")
	}
	expected := append([]byte(nil), baseline...)
	baseline[0] ^= 0xff

	// The public-to-tests accessor must never expose the process-global cache
	// for mutation. A new fixture thereafter must receive the original bytes
	// and pass the normal V2 admission checks.
	fresh, err := consolidatedV2TestBaselineBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fresh, expected) {
		t.Fatal("mutating a returned V2 baseline changed the cached template")
	}
	store, err := OpenForTest(t.TempDir())
	if err != nil {
		t.Fatalf("OpenForTest after baseline-byte mutation: %v", err)
	}
	defer store.Close()
	if err := store.validateConsolidatedV2Baseline(); err != nil {
		t.Fatalf("seeded test fixture did not pass V2 baseline admission: %v", err)
	}
}

func TestOpenForTestSeedsPrivateRegularDatabase(t *testing.T) {
	root := t.TempDir()
	store, err := OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(root, dbFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("seeded test database is not a regular file: %s", info.Mode())
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("seeded test database mode = %o, want 0600", got)
	}
}

func TestOpenForTestRejectsDatabaseSymlinkWithoutTouchingTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside-harbor.db")
	original := []byte("outside database must remain untouched")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, dbFileName)); err != nil {
		t.Skipf("create database symlink: %v", err)
	}
	if _, err := OpenForTest(root); err == nil {
		t.Fatal("OpenForTest accepted a database symlink")
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("OpenForTest changed the target of a rejected database symlink")
	}
}

func TestOpenForTestConcurrentFreshRoots(t *testing.T) {
	const fixtures = 8
	parent := t.TempDir()
	errs := make(chan error, fixtures)
	var group sync.WaitGroup
	for i := 0; i < fixtures; i++ {
		i := i
		group.Add(1)
		go func() {
			defer group.Done()
			root := filepath.Join(parent, fmt.Sprintf("fixture-%d", i))
			store, err := OpenForTest(root)
			if err != nil {
				errs <- fmt.Errorf("open fixture %d: %w", i, err)
				return
			}
			defer store.Close()
			if _, err := store.CreateTaskV2(context.Background(), CreateTaskV2Request{
				Slug:   fmt.Sprintf("concurrent-fixture-%d", i),
				Actor:  "tester",
				Reason: "concurrent test baseline fixture",
			}); err != nil {
				errs <- fmt.Errorf("create task in fixture %d: %w", i, err)
			}
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestArchitectureProductionSourcesDoNotCallOpenForTest(t *testing.T) {
	moduleRoot := findModuleRoot(t)
	const storeImportPath = "github.com/purplevoid/harbor-factory/internal/harbor/store"
	var violations []string
	err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse production Go source %s: %w", path, err)
		}
		aliases, dotImport := storeImportAliases(file, storeImportPath)
		if len(aliases) == 0 && !dotImport {
			return nil
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if isStoreOpenForTestCall(call, aliases, dotImport) {
				position := fileSet.Position(call.Pos())
				relative, relErr := filepath.Rel(moduleRoot, path)
				if relErr != nil {
					relative = path
				}
				violations = append(violations, fmt.Sprintf("%s:%d", filepath.ToSlash(relative), position.Line))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("OpenForTest is test-only and must not be called by production sources: %s", strings.Join(violations, ", "))
	}
}

func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && info.Mode().IsRegular() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find module root from %s", dir)
		}
		dir = parent
	}
}

func storeImportAliases(file *ast.File, storeImportPath string) (map[string]struct{}, bool) {
	aliases := make(map[string]struct{})
	dotImport := false
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != storeImportPath {
			continue
		}
		if spec.Name == nil {
			aliases["store"] = struct{}{}
			continue
		}
		switch spec.Name.Name {
		case ".":
			dotImport = true
		case "_":
			// A blank import cannot introduce a callable identifier.
		default:
			aliases[spec.Name.Name] = struct{}{}
		}
	}
	return aliases, dotImport
}

func isStoreOpenForTestCall(call *ast.CallExpr, aliases map[string]struct{}, dotImport bool) bool {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		qualifier, ok := fun.X.(*ast.Ident)
		if !ok || fun.Sel.Name != "OpenForTest" {
			return false
		}
		_, ok = aliases[qualifier.Name]
		return ok
	case *ast.Ident:
		return dotImport && fun.Name == "OpenForTest"
	default:
		return false
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
			if _, err := OpenForTest(root); !errors.Is(err, ErrLegacyV1Store) {
				t.Fatalf("OpenForTest legacy fixture error = %v, want ErrLegacyV1Store", err)
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

func TestWritableOpenRejectsIncompatibleSchemasWithoutDurableSideEffects(t *testing.T) {
	tests := []struct {
		name    string
		create  func(*sql.DB) error
		wantErr error
	}{
		{
			name: "retired_v1_tables",
			create: func(db *sql.DB) error {
				if _, err := db.Exec(`CREATE TABLE tasks (id INTEGER PRIMARY KEY)`); err != nil {
					return err
				}
				_, err := db.Exec(`CREATE TABLE runs (id INTEGER PRIMARY KEY, task_id INTEGER)`)
				return err
			},
			wantErr: ErrLegacyV1Store,
		},
		{
			name: "retired_v1_history",
			create: func(db *sql.DB) error {
				if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
					return err
				}
				_, err := db.Exec(`INSERT INTO schema_version (version) VALUES (1)`)
				return err
			},
			wantErr: ErrLegacyV1Store,
		},
		{
			name: "pre_consolidation_history",
			create: func(db *sql.DB) error {
				if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
					return err
				}
				_, err := db.Exec(`INSERT INTO schema_version (version) VALUES (20)`)
				return err
			},
			wantErr: ErrPreConsolidationStore,
		},
		{
			name: "pre_consolidation_history_with_foreign_key_violation",
			create: func(db *sql.DB) error {
				if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
					return err
				}
				if _, err := db.Exec(`CREATE TABLE legacy_parent (id INTEGER PRIMARY KEY)`); err != nil {
					return err
				}
				if _, err := db.Exec(`CREATE TABLE legacy_child (parent_id INTEGER REFERENCES legacy_parent(id))`); err != nil {
					return err
				}
				if _, err := db.Exec(`INSERT INTO legacy_child (parent_id) VALUES (404)`); err != nil {
					return err
				}
				if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
					return err
				}
				_, err := db.Exec(`INSERT INTO schema_version (version) VALUES (20)`)
				return err
			},
			wantErr: ErrPreConsolidationStore,
		},
		{
			name: "pre_consolidation_unmarked_user_table",
			create: func(db *sql.DB) error {
				_, err := db.Exec(`CREATE TABLE retired_v2_control_plane (id TEXT PRIMARY KEY)`)
				return err
			},
			wantErr: ErrPreConsolidationStore,
		},
	}
	openers := []struct {
		name string
		open func(string) (*Store, error)
	}{
		{name: "Open", open: Open},
		{name: "OpenForTest", open: OpenForTest},
	}

	for _, test := range tests {
		for _, opener := range openers {
			t.Run(test.name+"/"+opener.name, func(t *testing.T) {
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

				before := snapshotStoreTree(t, root)
				if _, err := opener.open(root); !errors.Is(err, test.wantErr) {
					t.Fatalf("%s error = %v, want %v", opener.name, err, test.wantErr)
				}
				after := snapshotStoreTree(t, root)
				if !reflect.DeepEqual(after, before) {
					t.Fatalf("%s changed rejected %s database; before=%#v after=%#v", opener.name, test.name, before, after)
				}
			})
		}
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
			if _, err := OpenForTest(root); !errors.Is(err, ErrPreConsolidationStore) {
				t.Fatalf("OpenForTest pre-consolidation fixture error = %v, want ErrPreConsolidationStore", err)
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

func TestOpenForTestRejectsCorruptPrimaryWithoutInitializingBackupFallback(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, dbFileName)
	corrupt := []byte("corrupted primary database")
	if err := os.WriteFile(dbPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenForTest(root); err == nil {
		t.Fatal("OpenForTest accepted a corrupt primary database without a verified backup")
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, corrupt) {
		t.Fatal("OpenForTest rewrote a corrupt primary database without a verified backup")
	}
}

func TestOpenAndOpenReadOnlyRejectConsolidatedV2StructuralDriftWithoutRepair(t *testing.T) {
	tests := []struct {
		name       string
		dropSQL    string
		objectType string
		objectName string
	}{
		{
			name:       "handoff_immutability_trigger",
			dropSQL:    `DROP TRIGGER codeedge_evaluator_evidence_handoffs_v2_immutable`,
			objectType: "trigger",
			objectName: "codeedge_evaluator_evidence_handoffs_v2_immutable",
		},
		{
			name:       "handoff_identity_registry_trigger",
			dropSQL:    `DROP TRIGGER entity_id_registry_codeedge_evaluator_evidence_handoffs_v2_insert`,
			objectType: "trigger",
			objectName: "entity_id_registry_codeedge_evaluator_evidence_handoffs_v2_insert",
		},
		{
			name:       "handoff_lookup_index",
			dropSQL:    `DROP INDEX idx_codeedge_evaluator_handoff_v2_task`,
			objectType: "index",
			objectName: "idx_codeedge_evaluator_handoff_v2_task",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			s, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			dbPath := filepath.Join(root, dbFileName)
			db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(test.dropSQL); err != nil {
				_ = db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			if _, err := Open(root); !errors.Is(err, ErrPreConsolidationStore) {
				t.Fatalf("Open structural drift error = %v, want ErrPreConsolidationStore", err)
			}
			if _, err := OpenForTest(root); !errors.Is(err, ErrPreConsolidationStore) {
				t.Fatalf("OpenForTest structural drift error = %v, want ErrPreConsolidationStore", err)
			}
			if _, err := OpenReadOnly(root); !errors.Is(err, ErrPreConsolidationStore) {
				t.Fatalf("OpenReadOnly structural drift error = %v, want ErrPreConsolidationStore", err)
			}

			db, err = sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`, test.objectType, test.objectName).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("Open repaired or accepted missing schema object %s %q", test.objectType, test.objectName)
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

type storeTreeEntry struct {
	Mode    fs.FileMode
	Content []byte
}

// snapshotStoreTree deliberately includes SQLite WAL/SHM sidecars and empty
// directories. It guards the hard-cutover admission promise that rejecting an
// incompatible existing store does not rewrite it or create recovery material.
func snapshotStoreTree(t *testing.T, root string) map[string]storeTreeEntry {
	t.Helper()
	entries := make(map[string]storeTreeEntry)
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entry := storeTreeEntry{Mode: info.Mode()}
		if info.Mode().IsRegular() {
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry.Content = content
		} else if !info.IsDir() {
			return fmt.Errorf("store tree snapshot encountered non-regular non-directory file")
		}
		entries[filepath.ToSlash(relative)] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
