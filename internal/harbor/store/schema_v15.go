package store

import (
	"database/sql"
	"fmt"
)

// migrationV15 adds the immutable binding between a durable review gate and
// its generic ReviewRequest. It also extends the original V2 stage status
// constraint with waiting, because a human review is durable work that is
// neither an infrastructure failure nor a terminal execution result.
//
// stage_attempts is rebuilt rather than patched in place because SQLite does
// not support altering an existing CHECK constraint. applyMigrationV15 runs
// this script on a dedicated connection with FK enforcement temporarily off,
// because SQLite applies ON DELETE RESTRICT immediately while dropping a
// referenced parent table. It verifies every child before committing and
// restores FK enforcement before releasing the connection.
const migrationV15 = `
CREATE TABLE stage_attempts_v15 (
    id                        TEXT PRIMARY KEY,
    run_id                    TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    retry_of_stage_attempt_id TEXT REFERENCES stage_attempts_v15(id) ON DELETE RESTRICT,
    stage_key                 TEXT NOT NULL,
    stage_group               TEXT NOT NULL,
    ordinal                   INTEGER NOT NULL CHECK (ordinal > 0),
    input_fingerprint         TEXT NOT NULL,
    execution_status          TEXT NOT NULL DEFAULT 'queued'
                              CHECK (execution_status IN ('queued', 'running', 'waiting', 'completed', 'infra_failed', 'interrupted', 'in_doubt', 'reconciling', 'canceled')),
    verdict                   TEXT NOT NULL DEFAULT ''
                              CHECK (verdict IN ('', 'pass', 'needs_repair', 'reject', 'advisory')),
    budget_snapshot_json      TEXT NOT NULL DEFAULT '{}',
    retry_snapshot_json       TEXT NOT NULL DEFAULT '{}',
    artifact_manifest_id      TEXT NOT NULL DEFAULT '',
    error_text                TEXT NOT NULL DEFAULT '',
    failure_class             TEXT NOT NULL DEFAULT '',
    created_at                DATETIME NOT NULL,
    started_at                DATETIME,
    finished_at               DATETIME,
    version                   INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    UNIQUE(run_id, stage_key, ordinal)
);

INSERT INTO stage_attempts_v15 (
    id, run_id, retry_of_stage_attempt_id, stage_key, stage_group, ordinal,
    input_fingerprint, execution_status, verdict, budget_snapshot_json,
    retry_snapshot_json, artifact_manifest_id, error_text, failure_class,
    created_at, started_at, finished_at, version
)
SELECT
    id, run_id, retry_of_stage_attempt_id, stage_key, stage_group, ordinal,
    input_fingerprint, execution_status, verdict, budget_snapshot_json,
    retry_snapshot_json, artifact_manifest_id, error_text, failure_class,
    created_at, started_at, finished_at, version
FROM stage_attempts;

DROP TABLE stage_attempts;
ALTER TABLE stage_attempts_v15 RENAME TO stage_attempts;
CREATE INDEX idx_stage_attempts_run ON stage_attempts(run_id, ordinal);
CREATE INDEX idx_stage_attempts_status ON stage_attempts(execution_status);

CREATE TABLE review_gate_bindings_v15 (
    stage_attempt_id        TEXT PRIMARY KEY REFERENCES stage_attempts(id) ON DELETE RESTRICT,
    review_request_id       TEXT NOT NULL UNIQUE REFERENCES review_requests(id) ON DELETE RESTRICT,
    run_id                  TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    revision_id             TEXT NOT NULL REFERENCES task_revisions(id) ON DELETE RESTRICT,
    revision_digest         TEXT NOT NULL,
    definition_hash         TEXT NOT NULL,
    stage_key               TEXT NOT NULL,
    review_kind             TEXT NOT NULL,
    node_attempt_id         TEXT NOT NULL UNIQUE REFERENCES node_attempts(id) ON DELETE RESTRICT,
    input_bindings_json     TEXT NOT NULL,
    input_fingerprint       TEXT NOT NULL,
    evidence_manifest_digest TEXT NOT NULL,
    created_at              DATETIME NOT NULL
);
CREATE INDEX idx_review_gate_bindings_v15_run
    ON review_gate_bindings_v15(run_id, stage_attempt_id);
CREATE INDEX idx_review_gate_bindings_v15_revision
    ON review_gate_bindings_v15(revision_id, created_at DESC);
CREATE TRIGGER review_gate_bindings_v15_no_update
BEFORE UPDATE ON review_gate_bindings_v15
BEGIN
    SELECT RAISE(ABORT, 'review gate binding is immutable');
END;
CREATE TRIGGER review_gate_bindings_v15_no_delete
BEFORE DELETE ON review_gate_bindings_v15
BEGIN
    SELECT RAISE(ABORT, 'review gate binding is immutable');
END;
`

func applyMigrationV15(tx *sql.Tx) error {
	if _, err := tx.Exec(migrationV15); err != nil {
		return fmt.Errorf("create immutable review gate bindings: %w", err)
	}
	// Rebuilding stage_attempts drops its V6 global-ID triggers. Existing IDs
	// stay registered; these triggers protect every later insert.
	source := globalIdentitySource{table: "stage_attempts", entityType: "stage_attempt"}
	if _, err := tx.Exec(globalIdentityInsertTrigger(source)); err != nil {
		return fmt.Errorf("restore stage attempt global identity insert trigger: %w", err)
	}
	if _, err := tx.Exec(globalIdentityImmutableTrigger(source)); err != nil {
		return fmt.Errorf("restore stage attempt immutable ID trigger: %w", err)
	}
	rows, err := tx.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check v15 foreign keys: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowID any
		var parent string
		var foreignKey int
		if err := rows.Scan(&table, &rowID, &parent, &foreignKey); err != nil {
			return fmt.Errorf("scan v15 foreign key check: %w", err)
		}
		return fmt.Errorf("v15 foreign key violation in %s row %v referencing %s key %d", table, rowID, parent, foreignKey)
	}
	return rows.Err()
}
