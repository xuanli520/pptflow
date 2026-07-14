package store

import (
	"database/sql"
	"fmt"
)

// migrationV12 introduces a durable receipt ledger for Task lifecycle
// mutations. The application layer owns action-specific behavior, while this
// ledger provides one replay boundary for its UUIDv7 idempotency key and the
// exact checkpoint the operator inspected.
const migrationV12 = `
CREATE TABLE IF NOT EXISTS lifecycle_operations_v12 (
    id                              TEXT PRIMARY KEY,
    idempotency_key                 TEXT NOT NULL UNIQUE,
    action                          TEXT NOT NULL,
    request_fingerprint             TEXT NOT NULL,
    task_id                         TEXT NOT NULL DEFAULT '',
    revision_id                     TEXT NOT NULL DEFAULT '',
    run_id                          TEXT NOT NULL DEFAULT '',
    review_request_id               TEXT NOT NULL DEFAULT '',
    release_id                      TEXT NOT NULL DEFAULT '',
    deletion_record_id              TEXT NOT NULL DEFAULT '',
    target_lifecycle_state          TEXT NOT NULL DEFAULT '',
    expected_task_version           INTEGER NOT NULL DEFAULT 0 CHECK (expected_task_version >= 0),
    expected_revision_state_version INTEGER NOT NULL DEFAULT 0 CHECK (expected_revision_state_version >= 0),
    expected_revision_digest        TEXT NOT NULL DEFAULT '',
    expected_run_version            INTEGER NOT NULL DEFAULT 0 CHECK (expected_run_version >= 0),
    expected_run_execution_epoch    INTEGER NOT NULL DEFAULT 0 CHECK (expected_run_execution_epoch >= 0),
    expected_run_definition_hash    TEXT NOT NULL DEFAULT '',
    expected_release_record_version INTEGER NOT NULL DEFAULT 0 CHECK (expected_release_record_version >= 0),
    expected_review_revision_id     TEXT NOT NULL DEFAULT '',
    expected_review_state           TEXT NOT NULL DEFAULT '',
    expected_review_evidence_digest TEXT NOT NULL DEFAULT '',
    actor                           TEXT NOT NULL,
    reason                          TEXT NOT NULL,
    state                           TEXT NOT NULL CHECK (state IN ('prepared', 'completed')),
    result_json                     TEXT NOT NULL DEFAULT '',
    created_at                      DATETIME NOT NULL,
    updated_at                      DATETIME NOT NULL,
    completed_at                    DATETIME,
    version                         INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE INDEX IF NOT EXISTS idx_lifecycle_operations_v12_target_task
    ON lifecycle_operations_v12(task_id, created_at DESC, id);
CREATE INDEX IF NOT EXISTS idx_lifecycle_operations_v12_action
    ON lifecycle_operations_v12(action, created_at DESC, id);
`

var lifecycleOperationIdentitySource = globalIdentitySource{
	table:      "lifecycle_operations_v12",
	entityType: "lifecycle_operation",
}

func applyMigrationV12(tx *sql.Tx) error {
	if _, err := tx.Exec(migrationV12); err != nil {
		return err
	}
	source := lifecycleOperationIdentitySource
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
