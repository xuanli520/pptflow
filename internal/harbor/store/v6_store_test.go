package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestV6RegistersEveryTextPrimaryKeyEntity(t *testing.T) {
	s := tempDB(t)
	expected := make(map[string]struct{}, len(globalIdentitySourcesCurrent))
	for _, source := range globalIdentitySourcesCurrent {
		expected[source.table] = struct{}{}
	}

	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tableNames []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		tableNames = append(tableNames, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}

	actual := make(map[string]struct{})
	for _, table := range tableNames {
		if table == "entity_id_registry" {
			continue
		}
		columns, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%q)", table))
		if err != nil {
			t.Fatal(err)
		}
		for columns.Next() {
			var cid, notNull, primaryKey int
			var name, columnType string
			var defaultValue any
			if err := columns.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
				columns.Close()
				t.Fatal(err)
			}
			if name == "id" && strings.EqualFold(columnType, "TEXT") && primaryKey > 0 {
				actual[table] = struct{}{}
			}
		}
		if err := columns.Err(); err != nil {
			columns.Close()
			t.Fatal(err)
		}
		if err := columns.Close(); err != nil {
			t.Fatal(err)
		}
	}

	if got, want := sortedSet(actual), sortedSet(expected); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("global identity source tables = %v, want %v", got, want)
	}
	for table := range actual {
		for _, trigger := range []string{
			"entity_id_registry_" + table + "_insert",
			"entity_id_registry_" + table + "_id_immutable",
		} {
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?`, trigger).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("missing global identity trigger %s", trigger)
			}
		}
	}
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func TestV6RejectsGlobalEntityIdentityCollisions(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	id, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTaskV2(ctx, CreateTaskV2Request{ID: id, Slug: "global-id-task", Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateTaskRevision(ctx, CreateTaskRevisionRequest{
		ID: task.ID, TaskID: task.ID, Origin: RevisionOriginManual, TaskDigest: validTaskDigest("a"), Actor: "tester",
	}); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("V2 cross-entity collision err=%v, want ErrIdentityCollision", err)
	}
	if _, err := s.CreateArtifactManifest(ctx, CreateArtifactManifestRequest{
		ID: task.ID, SubjectRevisionID: "revision", SubjectDigest: "sha256:subject",
		WorkflowFingerprint: "sha256:workflow", ManifestJSON: `{}`,
		ManifestFingerprint: "sha256:manifest", IdempotencyKey: "global-id-v4", Actor: "tester",
	}); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("V4 cross-entity collision err=%v, want ErrIdentityCollision", err)
	}
	if _, err := s.ConfigureCapacityPool(ctx, ConfigureCapacityPoolRequest{
		ID: task.ID, PoolKey: "global-id-v5", Capacity: 1, ExpectedVersion: 0, Actor: "tester",
	}); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("V5 cross-entity collision err=%v, want ErrIdentityCollision", err)
	}
	if _, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		ID: task.ID, CommandType: "test", EntityType: "task", EntityID: task.ID,
		PayloadJSON: `{}`, IdempotencyKey: "global-id-job", Actor: "tester",
	}); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("durable job collision err=%v, want ErrIdentityCollision", err)
	}
	if _, err := s.CreateOutboxEvent(ctx, CreateOutboxEventRequest{
		ID: task.ID, Topic: "test", EntityType: "task", EntityID: task.ID,
		PayloadJSON: `{}`, IdempotencyKey: "global-id-outbox", Actor: "tester",
	}); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("outbox collision err=%v, want ErrIdentityCollision", err)
	}
	if _, err := s.AcquireLease(ctx, AcquireLeaseRequest{
		ID: task.ID, ResourceType: "test", ResourceID: "global-id", Owner: "tester", TTL: time.Minute, Actor: "tester",
	}); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("lease collision err=%v, want ErrIdentityCollision", err)
	}

	var registryCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM entity_id_registry WHERE id = ?`, task.ID).Scan(&registryCount); err != nil {
		t.Fatal(err)
	}
	if registryCount != 1 {
		t.Fatalf("registry count for %s = %d, want 1", task.ID, registryCount)
	}

	if _, err := s.db.Exec(`
		INSERT INTO audit_events (id, actor, entity_type, entity_id, action, reason, payload_json, operation_key, created_at)
		VALUES (?, 'tester', 'task', ?, 'raw.insert', '', '{}', '', ?)
	`, task.ID, task.ID, time.Now().UTC()); err == nil {
		t.Fatal("raw cross-table insert accepted a globally registered ID")
	}
	if _, err := s.db.Exec(`
		INSERT OR IGNORE INTO audit_events (id, actor, entity_type, entity_id, action, reason, payload_json, operation_key, created_at)
		VALUES (?, 'tester', 'task', ?, 'raw.ignore', '', '{}', '', ?)
	`, task.ID, task.ID, time.Now().UTC()); err == nil {
		t.Fatal("INSERT OR IGNORE bypassed the global identity collision guard")
	}
	var rawRows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE id = ?`, task.ID).Scan(&rawRows); err != nil {
		t.Fatal(err)
	}
	if rawRows != 0 {
		t.Fatalf("INSERT OR IGNORE bypassed global identity registry: %d rows", rawRows)
	}

	pool, err := s.ConfigureCapacityPool(ctx, ConfigureCapacityPoolRequest{
		PoolKey: "global-id-update", Capacity: 1, ExpectedVersion: 0, Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE capacity_pools_v5 SET id = ? WHERE id = ?`, task.ID, pool.ID); err == nil || !strings.Contains(err.Error(), "lifecycle entity identity is immutable") {
		t.Fatalf("raw identity update err=%v, want immutable identity rejection", err)
	}
}

func TestV6MigrationRejectsExistingGlobalIdentityCollisionWithoutMutation(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, dbFileName)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range []string{migrationV1, migrationV2, migrationV3, migrationV4, migrationV5} {
		if _, err := db.Exec(migration); err != nil {
			t.Fatalf("create V5 fixture: %v", err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (5)`); err != nil {
		t.Fatal(err)
	}
	id, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.Exec(`
		INSERT INTO audit_events (id, actor, entity_type, entity_id, action, reason, payload_json, operation_key, created_at)
		VALUES (?, 'legacy', 'task', 'legacy-task', 'legacy.created', '', '{}', '', ?)
	`, id, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO outbox_events (id, topic, entity_type, entity_id, payload_json, idempotency_key, state, created_at, published_at, version)
		VALUES (?, 'legacy', 'task', 'legacy-task', '{}', '', 'pending', ?, NULL, 1)
	`, id, now); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(root); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("migrate colliding V5 store err=%v, want ErrIdentityCollision", err)
	}

	db, err = sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, auditRows, outboxRows, registryTables int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE id = ?`, id).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE id = ?`, id).Scan(&outboxRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'entity_id_registry'`).Scan(&registryTables); err != nil {
		t.Fatal(err)
	}
	if version != 5 || auditRows != 1 || outboxRows != 1 || registryTables != 0 {
		t.Fatalf("failed V6 migration mutated legacy store: version=%d audit=%d outbox=%d registry=%d", version, auditRows, outboxRows, registryTables)
	}
}

func TestV6MigrationBackfillsExistingEntityIDs(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, dbFileName)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range []string{migrationV1, migrationV2, migrationV3, migrationV4, migrationV5} {
		if _, err := db.Exec(migration); err != nil {
			t.Fatalf("create V5 fixture: %v", err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (5)`); err != nil {
		t.Fatal(err)
	}
	id, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO audit_events (id, actor, entity_type, entity_id, action, reason, payload_json, operation_key, created_at)
		VALUES (?, 'legacy', 'task', 'legacy-task', 'legacy.created', '', '{}', '', ?)
	`, id, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(root)
	if err != nil {
		t.Fatalf("migrate non-colliding V5 store: %v", err)
	}
	defer s.Close()
	var version, auditRows int
	var entityType string
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE id = ?`, id).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT entity_type FROM entity_id_registry WHERE id = ?`, id).Scan(&entityType); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || auditRows != 1 || entityType != "audit_event" {
		t.Fatalf("V6 backfill result: version=%d audit=%d entity_type=%q", version, auditRows, entityType)
	}
}
