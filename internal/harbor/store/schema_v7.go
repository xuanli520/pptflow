package store

import (
	"database/sql"
	"fmt"
)

// migrationV7 adds a task-specific irreversible-purge protocol. The operation
// row is durable across the filesystem boundary, while the referenced lease
// supplies fencing and prevents a restore from racing a live purge.
const migrationV7 = `
CREATE TABLE IF NOT EXISTS task_purge_operations_v7 (
    id                    TEXT PRIMARY KEY,
    task_id               TEXT NOT NULL REFERENCES tasks_v2(id) ON DELETE RESTRICT,
    idempotency_key       TEXT NOT NULL UNIQUE,
    expected_task_version INTEGER NOT NULL CHECK (expected_task_version > 0),
    actor                 TEXT NOT NULL,
    reason                TEXT NOT NULL,
    state                 TEXT NOT NULL CHECK (state IN ('in_progress', 'blocked', 'completed')),
    lease_id              TEXT REFERENCES leases(id) ON DELETE RESTRICT,
    lease_owner           TEXT NOT NULL DEFAULT '',
    lease_fencing_token   INTEGER NOT NULL DEFAULT 0 CHECK (lease_fencing_token >= 0),
    lease_version         INTEGER NOT NULL DEFAULT 0 CHECK (lease_version >= 0),
    dependencies_json     TEXT NOT NULL DEFAULT '{}',
    last_error            TEXT NOT NULL DEFAULT '',
    created_at            DATETIME NOT NULL,
    updated_at            DATETIME NOT NULL,
    completed_at          DATETIME,
    version               INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE INDEX IF NOT EXISTS idx_task_purge_operations_v7_task
    ON task_purge_operations_v7(task_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_task_purge_operations_v7_in_progress_task
    ON task_purge_operations_v7(task_id) WHERE state = 'in_progress';

-- A live purge lease protects every task mutation, including restore. Normal
-- store mutations expire a stale lease before touching the task; raw SQL is
-- conservatively rejected until recovery resolves the stale lease.
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

func applyMigrationV7(tx *sql.Tx) error {
	if _, err := tx.Exec(migrationV7); err != nil {
		return err
	}
	source := taskPurgeOperationIdentitySource
	statement := fmt.Sprintf(
		"INSERT INTO entity_id_registry (id, entity_type) SELECT id, '%s' FROM %s",
		source.entityType,
		source.table,
	)
	if _, err := tx.Exec(statement); err != nil {
		return fmt.Errorf("register %s identities: %w", source.table, err)
	}
	if _, err := tx.Exec(globalIdentityInsertTrigger(source)); err != nil {
		return fmt.Errorf("create global identity trigger for %s: %w", source.table, err)
	}
	if _, err := tx.Exec(globalIdentityImmutableTrigger(source)); err != nil {
		return fmt.Errorf("create immutable identity trigger for %s: %w", source.table, err)
	}
	return nil
}
