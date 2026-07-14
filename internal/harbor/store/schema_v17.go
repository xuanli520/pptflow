package store

import (
	"database/sql"
	"fmt"
)

// migrationV17 removes the retired V1 identity bridge from a pure V2 control
// plane. Databases that still contain V1 schema history are rejected before
// this migration is considered.
const migrationV17 = `
DROP INDEX IF EXISTS idx_tasks_v2_canonical_import_identity;
DROP INDEX IF EXISTS idx_tasks_v2_identity_state;

CREATE TABLE tasks_v2_v17 (
    id                  TEXT PRIMARY KEY,
    slug                TEXT NOT NULL,
    title               TEXT NOT NULL DEFAULT '',
    metadata_json       TEXT NOT NULL DEFAULT '{}',
    source_repo         TEXT NOT NULL DEFAULT '',
    source_commit       TEXT NOT NULL DEFAULT '',
    lifecycle_state     TEXT NOT NULL DEFAULT 'draft'
                        CHECK (lifecycle_state IN ('draft', 'ready', 'published', 'archived', 'deleted')),
    current_revision_id TEXT NOT NULL DEFAULT '',
    created_at          DATETIME NOT NULL,
    updated_at          DATETIME NOT NULL,
    deleted_at          DATETIME,
    version             INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);

INSERT INTO tasks_v2_v17 (
    id, slug, title, metadata_json, source_repo, source_commit,
    lifecycle_state, current_revision_id, created_at, updated_at, deleted_at, version
)
SELECT
    id, slug, title, metadata_json, source_repo, source_commit,
    lifecycle_state, current_revision_id, created_at, updated_at, deleted_at, version
FROM tasks_v2;

DROP TABLE tasks_v2;
ALTER TABLE tasks_v2_v17 RENAME TO tasks_v2;
CREATE INDEX idx_tasks_v2_slug ON tasks_v2(slug);
CREATE INDEX idx_tasks_v2_current_revision ON tasks_v2(current_revision_id);

-- Rebuilding tasks_v2 drops triggers attached to the retired table. Restore
-- the V7 purge fence before allowing any task mutation on the replacement.
CREATE TRIGGER IF NOT EXISTS task_purge_v7_blocks_task_mutation
BEFORE UPDATE ON tasks_v2
WHEN EXISTS (
    SELECT 1 FROM leases
    WHERE resource_type = 'task_purge'
      AND resource_id = OLD.id
      AND state = 'active'
)
BEGIN
    SELECT RAISE(ABORT, 'task purge is in progress');
END;
`

func applyMigrationV17(tx *sql.Tx) error {
	if _, err := tx.Exec(migrationV17); err != nil {
		return fmt.Errorf("rebuild tasks without legacy identity fields: %w", err)
	}
	source := globalIdentitySource{table: "tasks_v2", entityType: "task"}
	if _, err := tx.Exec(globalIdentityInsertTrigger(source)); err != nil {
		return fmt.Errorf("restore task identity insert trigger: %w", err)
	}
	if _, err := tx.Exec(globalIdentityImmutableTrigger(source)); err != nil {
		return fmt.Errorf("restore task identity immutable trigger: %w", err)
	}
	rows, err := tx.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check v17 foreign keys: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowID any
		var parent string
		var foreignKey int
		if err := rows.Scan(&table, &rowID, &parent, &foreignKey); err != nil {
			return fmt.Errorf("scan v17 foreign key check: %w", err)
		}
		return fmt.Errorf("v17 foreign key violation in %s row %v referencing %s key %d", table, rowID, parent, foreignKey)
	}
	return rows.Err()
}
