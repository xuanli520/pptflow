package store

import (
	"database/sql"
	"fmt"
)

// migrationV10 records two local lifecycle operations that cross a durable
// boundary: terminal candidate checkout cleanup and release withdrawal. The
// former retains the candidate row as a tombstone; the latter retains both an
// idempotent operation and an immutable receipt.
const migrationV10 = `
ALTER TABLE revision_candidates_v8 ADD COLUMN checkout_tombstoned_at DATETIME;
ALTER TABLE revision_candidates_v8 ADD COLUMN checkout_tombstoned_by TEXT NOT NULL DEFAULT '';

UPDATE revision_candidates_v8
SET retain_until = datetime(updated_at, '+7 days')
WHERE retain_until IS NULL AND state IN ('no_op', 'discarded');

CREATE INDEX IF NOT EXISTS idx_revision_candidates_v10_gc
    ON revision_candidates_v8(state, retain_until, checkout_tombstoned_at, id);

CREATE TABLE IF NOT EXISTS candidate_gc_operations_v10 (
    id                         TEXT PRIMARY KEY,
    candidate_id               TEXT NOT NULL REFERENCES revision_candidates_v8(id) ON DELETE RESTRICT,
    idempotency_key            TEXT NOT NULL UNIQUE,
    expected_candidate_version INTEGER NOT NULL CHECK (expected_candidate_version > 0),
    actor                      TEXT NOT NULL,
    reason                     TEXT NOT NULL,
    state                      TEXT NOT NULL CHECK (state IN ('in_progress', 'completed')),
    last_error                 TEXT NOT NULL DEFAULT '',
    created_at                 DATETIME NOT NULL,
    updated_at                 DATETIME NOT NULL,
    completed_at               DATETIME,
    version                    INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE INDEX IF NOT EXISTS idx_candidate_gc_operations_v10_candidate
    ON candidate_gc_operations_v10(candidate_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_candidate_gc_operations_v10_active_candidate
    ON candidate_gc_operations_v10(candidate_id) WHERE state = 'in_progress';

CREATE TABLE IF NOT EXISTS release_withdraw_operations_v10 (
    id                       TEXT PRIMARY KEY,
    release_id               TEXT NOT NULL REFERENCES releases(id) ON DELETE RESTRICT,
    idempotency_key          TEXT NOT NULL UNIQUE,
    expected_release_version INTEGER NOT NULL CHECK (expected_release_version > 0),
    actor                    TEXT NOT NULL,
    reason                   TEXT NOT NULL,
    state                    TEXT NOT NULL CHECK (state = 'completed'),
    receipt_id               TEXT NOT NULL UNIQUE,
    result_release_version   INTEGER NOT NULL CHECK (result_release_version > 0),
    created_at               DATETIME NOT NULL,
    completed_at             DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_release_withdraw_operations_v10_release
    ON release_withdraw_operations_v10(release_id, created_at DESC);

CREATE TABLE IF NOT EXISTS release_withdraw_receipts_v10 (
    id                      TEXT PRIMARY KEY,
    operation_id            TEXT NOT NULL UNIQUE REFERENCES release_withdraw_operations_v10(id) ON DELETE RESTRICT,
    release_id              TEXT NOT NULL REFERENCES releases(id) ON DELETE RESTRICT,
    release_version         TEXT NOT NULL,
    expected_record_version INTEGER NOT NULL CHECK (expected_record_version > 0),
    result_record_version   INTEGER NOT NULL CHECK (result_record_version > 0),
    receipt_json            TEXT NOT NULL,
    receipt_digest          TEXT NOT NULL,
    created_by              TEXT NOT NULL,
    created_at              DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_release_withdraw_receipts_v10_release
    ON release_withdraw_receipts_v10(release_id, created_at DESC);
`

var candidateGarbageCollectionOperationIdentitySource = globalIdentitySource{
	table:      "candidate_gc_operations_v10",
	entityType: "candidate_gc_operation",
}

var releaseWithdrawOperationIdentitySource = globalIdentitySource{
	table:      "release_withdraw_operations_v10",
	entityType: "release_withdraw_operation",
}

var releaseWithdrawReceiptIdentitySource = globalIdentitySource{
	table:      "release_withdraw_receipts_v10",
	entityType: "release_withdraw_receipt",
}

func applyMigrationV10(tx *sql.Tx) error {
	if _, err := tx.Exec(migrationV10); err != nil {
		return err
	}
	for _, source := range []globalIdentitySource{
		candidateGarbageCollectionOperationIdentitySource,
		releaseWithdrawOperationIdentitySource,
		releaseWithdrawReceiptIdentitySource,
	} {
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
	}
	return nil
}
