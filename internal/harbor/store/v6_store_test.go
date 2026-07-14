package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestConsolidatedV2RegistersEveryTextPrimaryKeyEntity(t *testing.T) {
	s := tempDB(t)
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	tables := make([]string, 0)
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}

	identityTables := make(map[string]struct{})
	for _, table := range tables {
		// entity_id_registry is the global identity ledger itself. Its TEXT
		// primary key is intentionally not guarded by a recursive registry
		// trigger; every lifecycle entity is guarded before it reaches this table.
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
				_ = columns.Close()
				t.Fatal(err)
			}
			if name == "id" && strings.EqualFold(columnType, "TEXT") && primaryKey > 0 {
				identityTables[table] = struct{}{}
			}
		}
		if err := columns.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{
		"tasks_v2", "workflow_runs", "lifecycle_operations_v12",
		"trial_executions_v19", "trial_attempts_v19", "codeedge_compliance_records_v20",
	} {
		if _, ok := identityTables[table]; !ok {
			t.Fatalf("missing V2 global identity source table %q", table)
		}
	}
	for table := range identityTables {
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

func TestConsolidatedV2RejectsGlobalEntityIdentityCollisions(t *testing.T) {
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
	if _, err := s.CreateArtifactManifest(ctx, CreateArtifactManifestRequest{
		ID: task.ID, SubjectRevisionID: "revision", SubjectDigest: "sha256:subject",
		WorkflowFingerprint: "sha256:workflow", ManifestJSON: `{}`,
		ManifestFingerprint: "sha256:manifest", IdempotencyKey: "global-id-artifact", Actor: "tester",
	}); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("cross-entity collision err=%v, want ErrIdentityCollision", err)
	}
	if _, err := s.db.Exec(`
		INSERT OR IGNORE INTO audit_events (id, actor, entity_type, entity_id, action, reason, payload_json, operation_key, created_at)
		VALUES (?, 'tester', 'task', ?, 'raw.ignore', '', '{}', '', ?)
	`, task.ID, task.ID, time.Now().UTC()); err == nil {
		t.Fatal("INSERT OR IGNORE bypassed the global identity collision guard")
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
	var entityType string
	if err := s.db.QueryRow(`SELECT entity_type FROM entity_id_registry WHERE id = ?`, task.ID).Scan(&entityType); err != nil {
		t.Fatal(err)
	}
	if entityType != "task" {
		t.Fatalf("registry entity type=%q, want task", entityType)
	}
}
