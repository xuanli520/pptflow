package store

import (
	"database/sql"
	"fmt"
)

// migrationV8 introduces the crash-safe, isolated candidate boundary for
// content-changing continuation. A candidate is not a TaskRevision: its
// checkout is mutable only while the candidate write lease is fenced. The
// final revision and child-run identities are reserved in the global UUIDv7
// namespace before a plan can refer to them.
const migrationV8 = `
CREATE TABLE IF NOT EXISTS revision_candidates_v8 (
    id                       TEXT PRIMARY KEY,
    task_id                  TEXT NOT NULL REFERENCES tasks_v2(id) ON DELETE RESTRICT,
    source_run_id            TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    command_id               TEXT NOT NULL UNIQUE REFERENCES continuation_commands_v4(id) ON DELETE RESTRICT,
    repair_session_id        TEXT REFERENCES repair_sessions_v4(id) ON DELETE RESTRICT,
    round_ordinal            INTEGER NOT NULL DEFAULT 0 CHECK (round_ordinal >= 0),
    base_revision_id         TEXT NOT NULL REFERENCES task_revisions(id) ON DELETE RESTRICT,
    base_digest              TEXT NOT NULL,
    target_revision_id       TEXT NOT NULL UNIQUE,
    target_run_id            TEXT NOT NULL UNIQUE,
    expected_task_version    INTEGER NOT NULL CHECK (expected_task_version > 0),
    provider_id              TEXT NOT NULL,
    checkout_relpath         TEXT NOT NULL,
    findings_json            TEXT NOT NULL,
    state                    TEXT NOT NULL CHECK (state IN ('ready', 'applying', 'prepared', 'no_op', 'reconcile_required', 'discarded', 'committing', 'committed')),
    after_digest             TEXT NOT NULL DEFAULT '',
    observed_changes_json    TEXT NOT NULL DEFAULT '[]',
    prepared_change_id       TEXT REFERENCES prepared_changes_v4(id) ON DELETE RESTRICT,
    mutation_receipt_id      TEXT REFERENCES mutation_receipts_v4(id) ON DELETE RESTRICT,
    frozen_plan_id           TEXT REFERENCES frozen_plans_v4(id) ON DELETE RESTRICT,
    final_manifest_id        TEXT NOT NULL DEFAULT '',
    child_run_manifest_json  TEXT NOT NULL DEFAULT '',
    lease_id                 TEXT REFERENCES leases(id) ON DELETE RESTRICT,
    lease_owner              TEXT NOT NULL DEFAULT '',
    lease_fencing_token      INTEGER NOT NULL DEFAULT 0 CHECK (lease_fencing_token >= 0),
    lease_version            INTEGER NOT NULL DEFAULT 0 CHECK (lease_version >= 0),
    created_by               TEXT NOT NULL,
    reason                   TEXT NOT NULL,
    created_at               DATETIME NOT NULL,
    updated_at               DATETIME NOT NULL,
    retain_until             DATETIME,
    version                  INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    UNIQUE(prepared_change_id),
    UNIQUE(mutation_receipt_id),
    UNIQUE(frozen_plan_id)
);
CREATE INDEX IF NOT EXISTS idx_revision_candidates_v8_task
    ON revision_candidates_v8(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_revision_candidates_v8_source_run
    ON revision_candidates_v8(source_run_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_revision_candidates_v8_command
    ON revision_candidates_v8(command_id);
CREATE INDEX IF NOT EXISTS idx_revision_candidates_v8_repair
    ON revision_candidates_v8(repair_session_id, round_ordinal);
CREATE INDEX IF NOT EXISTS idx_revision_candidates_v8_state
    ON revision_candidates_v8(state, updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_revision_candidates_v8_active_task
    ON revision_candidates_v8(task_id)
    WHERE state IN ('ready', 'applying', 'prepared', 'reconcile_required', 'committing');

-- Target identities are registered before the actual TaskRevision/WorkflowRun
-- rows exist. Commit consumes a reservation atomically; no other entity can
-- observe or reuse either identity in between.
CREATE TABLE IF NOT EXISTS revision_candidate_identity_reservations_v8 (
    reserved_id       TEXT PRIMARY KEY,
    candidate_id      TEXT NOT NULL REFERENCES revision_candidates_v8(id) ON DELETE RESTRICT,
    intended_type     TEXT NOT NULL CHECK (intended_type IN ('task_revision', 'workflow_run')),
    created_at        DATETIME NOT NULL,
    UNIQUE(candidate_id, intended_type)
);

CREATE TABLE IF NOT EXISTS change_operations_v8 (
    id                 TEXT PRIMARY KEY,
    candidate_id       TEXT NOT NULL REFERENCES revision_candidates_v8(id) ON DELETE RESTRICT,
    provider_id        TEXT NOT NULL,
    operation_key      TEXT NOT NULL UNIQUE,
    payload_json       TEXT NOT NULL,
    payload_digest     TEXT NOT NULL,
    state              TEXT NOT NULL CHECK (state IN ('prepared', 'running', 'succeeded', 'failed', 'unknown')),
    receipt_id         TEXT REFERENCES mutation_receipts_v4(id) ON DELETE RESTRICT,
    created_by         TEXT NOT NULL,
    created_at         DATETIME NOT NULL,
    updated_at         DATETIME NOT NULL,
    version            INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    UNIQUE(candidate_id, operation_key)
);
CREATE INDEX IF NOT EXISTS idx_change_operations_v8_candidate
    ON change_operations_v8(candidate_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_change_operations_v8_state
    ON change_operations_v8(state, updated_at);
`

var revisionCandidateIdentitySource = globalIdentitySource{
	table:      "revision_candidates_v8",
	entityType: "revision_candidate",
}

var changeOperationIdentitySource = globalIdentitySource{
	table:      "change_operations_v8",
	entityType: "change_operation",
}

func applyMigrationV8(tx *sql.Tx) error {
	if _, err := tx.Exec(migrationV8); err != nil {
		return err
	}
	for _, source := range []globalIdentitySource{revisionCandidateIdentitySource, changeOperationIdentitySource} {
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
