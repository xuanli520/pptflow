package store

import (
	"database/sql"
	"fmt"
)

// migrationV16 persists the full local child-worker handoff state machine.
// The active partial unique index prevents concurrent idempotency keys from
// launching competing children for one Run.
const migrationV16 = `
CREATE TABLE IF NOT EXISTS run_worker_handoffs_v16 (
    id                           TEXT PRIMARY KEY,
    idempotency_key              TEXT NOT NULL UNIQUE,
    request_fingerprint          TEXT NOT NULL,
    run_id                       TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    expected_run_version         INTEGER NOT NULL CHECK (expected_run_version > 0),
    expected_run_execution_epoch INTEGER NOT NULL CHECK (expected_run_execution_epoch >= 0),
    expected_run_definition_hash TEXT NOT NULL,
    owner                        TEXT NOT NULL,
    actor                        TEXT NOT NULL,
    reason                       TEXT NOT NULL,
    state                        TEXT NOT NULL CHECK (state IN ('launching', 'handed_off', 'released', 'failed', 'expired')),
    launch_deadline_at           DATETIME NOT NULL,
    worker_lease_id              TEXT NOT NULL DEFAULT '',
    worker_lease_owner           TEXT NOT NULL DEFAULT '',
    worker_lease_fencing_token   INTEGER NOT NULL DEFAULT 0 CHECK (worker_lease_fencing_token >= 0),
    worker_lease_version         INTEGER NOT NULL DEFAULT 0 CHECK (worker_lease_version >= 0),
    process_id                   INTEGER NOT NULL DEFAULT 0 CHECK (process_id >= 0),
    log_path                     TEXT NOT NULL DEFAULT '',
    receipt_json                 TEXT NOT NULL DEFAULT '',
    failure_reason               TEXT NOT NULL DEFAULT '',
    created_at                   DATETIME NOT NULL,
    updated_at                   DATETIME NOT NULL,
    spawned_at                   DATETIME,
    handed_off_at                DATETIME,
    released_at                  DATETIME,
    version                      INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE INDEX IF NOT EXISTS idx_run_worker_handoffs_v16_run
    ON run_worker_handoffs_v16(run_id, created_at DESC, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_run_worker_handoffs_v16_active_run
    ON run_worker_handoffs_v16(run_id)
    WHERE state IN ('launching', 'handed_off');
CREATE INDEX IF NOT EXISTS idx_run_worker_handoffs_v16_worker_lease
    ON run_worker_handoffs_v16(worker_lease_id);
`

var runWorkerHandoffIdentitySource = globalIdentitySource{
	table:      "run_worker_handoffs_v16",
	entityType: "run_worker_handoff",
}

func applyMigrationV16(tx *sql.Tx) error {
	if _, err := tx.Exec(migrationV16); err != nil {
		return err
	}
	source := runWorkerHandoffIdentitySource
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
