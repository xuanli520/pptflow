package store

// migrationV4 is intentionally isolated from the migration registry. The
// caller that owns Store schema registration applies it after V3. Its records
// persist generic immutable evidence and continuation facts; workflow policy
// remains outside the store.
const migrationV4 = `
CREATE TABLE IF NOT EXISTS artifact_manifests_v4 (
    id                   TEXT PRIMARY KEY,
    subject_revision_id  TEXT NOT NULL,
    subject_digest       TEXT NOT NULL,
    workflow_fingerprint TEXT NOT NULL,
    manifest_json        TEXT NOT NULL,
    manifest_fingerprint TEXT NOT NULL,
    idempotency_key      TEXT NOT NULL UNIQUE,
    created_by           TEXT NOT NULL,
    created_at           DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_artifact_manifests_v4_subject
    ON artifact_manifests_v4(subject_revision_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_artifact_manifests_v4_fingerprint
    ON artifact_manifests_v4(manifest_fingerprint);
CREATE TRIGGER IF NOT EXISTS artifact_manifests_v4_no_update
BEFORE UPDATE ON artifact_manifests_v4
BEGIN
    SELECT RAISE(ABORT, 'artifact manifests are immutable');
END;
CREATE TRIGGER IF NOT EXISTS artifact_manifests_v4_no_delete
BEFORE DELETE ON artifact_manifests_v4
BEGIN
    SELECT RAISE(ABORT, 'artifact manifests are immutable');
END;

CREATE TABLE IF NOT EXISTS artifact_refs_v4 (
    id                   TEXT PRIMARY KEY,
    manifest_id          TEXT NOT NULL REFERENCES artifact_manifests_v4(id) ON DELETE RESTRICT,
    artifact_key         TEXT NOT NULL,
    content_digest       TEXT NOT NULL,
    schema_version       TEXT NOT NULL,
    run_id               TEXT NOT NULL,
    stage_key            TEXT NOT NULL,
    attempt_id           TEXT NOT NULL,
    turn_ordinal         INTEGER NOT NULL CHECK (turn_ordinal >= 0),
    subject_revision_id  TEXT NOT NULL,
    subject_digest       TEXT NOT NULL,
    workflow_fingerprint TEXT NOT NULL,
    input_bindings_json  TEXT NOT NULL,
    input_fingerprint    TEXT NOT NULL,
    producer_version     TEXT NOT NULL,
    idempotency_key      TEXT NOT NULL UNIQUE,
    created_at           DATETIME NOT NULL,
    UNIQUE(manifest_id, artifact_key)
);
CREATE INDEX IF NOT EXISTS idx_artifact_refs_v4_manifest ON artifact_refs_v4(manifest_id, artifact_key);
CREATE INDEX IF NOT EXISTS idx_artifact_refs_v4_lineage
    ON artifact_refs_v4(subject_revision_id, workflow_fingerprint, stage_key, created_at DESC);
CREATE TRIGGER IF NOT EXISTS artifact_refs_v4_no_update
BEFORE UPDATE ON artifact_refs_v4
BEGIN
    SELECT RAISE(ABORT, 'artifact refs are immutable');
END;
CREATE TRIGGER IF NOT EXISTS artifact_refs_v4_no_delete
BEFORE DELETE ON artifact_refs_v4
BEGIN
    SELECT RAISE(ABORT, 'artifact refs are immutable');
END;

CREATE TABLE IF NOT EXISTS continuation_commands_v4 (
    id             TEXT PRIMARY KEY,
    command_key    TEXT NOT NULL UNIQUE,
    subject_id     TEXT NOT NULL,
    run_id         TEXT NOT NULL DEFAULT '',
    payload_json   TEXT NOT NULL,
    payload_digest TEXT NOT NULL,
    actor          TEXT NOT NULL,
    reason         TEXT NOT NULL DEFAULT '',
    created_at     DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_continuation_commands_v4_subject
    ON continuation_commands_v4(subject_id, created_at DESC);
CREATE TRIGGER IF NOT EXISTS continuation_commands_v4_no_update
BEFORE UPDATE ON continuation_commands_v4
BEGIN
    SELECT RAISE(ABORT, 'continuation commands are immutable');
END;
CREATE TRIGGER IF NOT EXISTS continuation_commands_v4_no_delete
BEFORE DELETE ON continuation_commands_v4
BEGIN
    SELECT RAISE(ABORT, 'continuation commands are immutable');
END;

CREATE TABLE IF NOT EXISTS repair_sessions_v4 (
    id                TEXT PRIMARY KEY,
    command_id        TEXT NOT NULL REFERENCES continuation_commands_v4(id) ON DELETE RESTRICT,
    subject_id        TEXT NOT NULL,
    base_revision_id  TEXT NOT NULL,
    max_rounds        INTEGER NOT NULL CHECK (max_rounds > 0),
    status            TEXT NOT NULL CHECK (status IN ('open', 'needs_human', 'completed', 'canceled')),
    findings_json     TEXT NOT NULL,
    policy_json       TEXT NOT NULL,
    idempotency_key   TEXT NOT NULL UNIQUE,
    created_by        TEXT NOT NULL,
    created_at        DATETIME NOT NULL,
    updated_at        DATETIME NOT NULL,
    version           INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE INDEX IF NOT EXISTS idx_repair_sessions_v4_subject ON repair_sessions_v4(subject_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_repair_sessions_v4_command ON repair_sessions_v4(command_id);

CREATE TABLE IF NOT EXISTS prepared_changes_v4 (
    id                    TEXT PRIMARY KEY,
    command_id            TEXT NOT NULL REFERENCES continuation_commands_v4(id) ON DELETE RESTRICT,
    repair_session_id     TEXT REFERENCES repair_sessions_v4(id) ON DELETE RESTRICT,
    round_ordinal         INTEGER NOT NULL DEFAULT 0 CHECK (round_ordinal >= 0),
    provider_id           TEXT NOT NULL,
    operation_key         TEXT NOT NULL UNIQUE,
    payload_json          TEXT NOT NULL,
    observed_changes_json TEXT NOT NULL,
    before_digest         TEXT NOT NULL,
    after_digest          TEXT NOT NULL,
    created_by            TEXT NOT NULL,
    created_at            DATETIME NOT NULL,
    UNIQUE(repair_session_id, round_ordinal)
);
CREATE INDEX IF NOT EXISTS idx_prepared_changes_v4_command ON prepared_changes_v4(command_id, created_at);
CREATE TRIGGER IF NOT EXISTS prepared_changes_v4_no_update
BEFORE UPDATE ON prepared_changes_v4
BEGIN
    SELECT RAISE(ABORT, 'prepared changes are immutable');
END;
CREATE TRIGGER IF NOT EXISTS prepared_changes_v4_no_delete
BEFORE DELETE ON prepared_changes_v4
BEGIN
    SELECT RAISE(ABORT, 'prepared changes are immutable');
END;

CREATE TABLE IF NOT EXISTS mutation_receipts_v4 (
    id                    TEXT PRIMARY KEY,
    prepared_change_id    TEXT NOT NULL REFERENCES prepared_changes_v4(id) ON DELETE RESTRICT,
    operation_key         TEXT NOT NULL,
    outcome               TEXT NOT NULL CHECK (outcome IN ('applied', 'no_op', 'uncertain', 'failed')),
    receipt_json          TEXT NOT NULL,
    receipt_digest        TEXT NOT NULL,
    supersedes_receipt_id TEXT REFERENCES mutation_receipts_v4(id) ON DELETE RESTRICT,
    idempotency_key       TEXT NOT NULL UNIQUE,
    created_by            TEXT NOT NULL,
    created_at            DATETIME NOT NULL,
    UNIQUE(prepared_change_id, operation_key, receipt_digest)
);
CREATE INDEX IF NOT EXISTS idx_mutation_receipts_v4_change ON mutation_receipts_v4(prepared_change_id, created_at);
CREATE TRIGGER IF NOT EXISTS mutation_receipts_v4_no_update
BEFORE UPDATE ON mutation_receipts_v4
BEGIN
    SELECT RAISE(ABORT, 'mutation receipts are immutable');
END;
CREATE TRIGGER IF NOT EXISTS mutation_receipts_v4_no_delete
BEFORE DELETE ON mutation_receipts_v4
BEGIN
    SELECT RAISE(ABORT, 'mutation receipts are immutable');
END;

CREATE TABLE IF NOT EXISTS frozen_plans_v4 (
    id                    TEXT PRIMARY KEY,
    command_id            TEXT NOT NULL UNIQUE REFERENCES continuation_commands_v4(id) ON DELETE RESTRICT,
    prepared_change_id    TEXT REFERENCES prepared_changes_v4(id) ON DELETE RESTRICT,
    subject_id            TEXT NOT NULL,
    subject_revision_id   TEXT NOT NULL,
    subject_digest        TEXT NOT NULL,
    workflow_fingerprint  TEXT NOT NULL,
    plan_fingerprint      TEXT NOT NULL,
    payload_json          TEXT NOT NULL,
    payload_digest        TEXT NOT NULL,
    expires_at            DATETIME NOT NULL,
    created_by            TEXT NOT NULL,
    created_at            DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_frozen_plans_v4_subject ON frozen_plans_v4(subject_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_frozen_plans_v4_prepared_change ON frozen_plans_v4(prepared_change_id);
CREATE TRIGGER IF NOT EXISTS frozen_plans_v4_no_update
BEFORE UPDATE ON frozen_plans_v4
BEGIN
    SELECT RAISE(ABORT, 'frozen plans are immutable');
END;
CREATE TRIGGER IF NOT EXISTS frozen_plans_v4_no_delete
BEFORE DELETE ON frozen_plans_v4
BEGIN
    SELECT RAISE(ABORT, 'frozen plans are immutable');
END;

CREATE TABLE IF NOT EXISTS continuation_executions_v4 (
    id                    TEXT PRIMARY KEY,
    plan_id               TEXT NOT NULL REFERENCES frozen_plans_v4(id) ON DELETE RESTRICT,
    parent_execution_id   TEXT REFERENCES continuation_executions_v4(id) ON DELETE RESTRICT,
    run_id                TEXT NOT NULL DEFAULT '',
    idempotency_key       TEXT NOT NULL UNIQUE,
    state                 TEXT NOT NULL CHECK (state IN ('queued', 'running', 'completed', 'failed', 'canceled', 'reconcile_required')),
    payload_json          TEXT NOT NULL,
    created_by            TEXT NOT NULL,
    created_at            DATETIME NOT NULL,
    updated_at            DATETIME NOT NULL,
    finished_at           DATETIME,
    version               INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE INDEX IF NOT EXISTS idx_continuation_executions_v4_plan ON continuation_executions_v4(plan_id, created_at);
CREATE INDEX IF NOT EXISTS idx_continuation_executions_v4_state ON continuation_executions_v4(state, updated_at);
CREATE TRIGGER IF NOT EXISTS continuation_executions_v4_content_immutable
BEFORE UPDATE ON continuation_executions_v4
WHEN NEW.id <> OLD.id
  OR NEW.plan_id <> OLD.plan_id
  OR NEW.parent_execution_id IS NOT OLD.parent_execution_id
  OR NEW.run_id <> OLD.run_id
  OR NEW.idempotency_key <> OLD.idempotency_key
  OR NEW.payload_json <> OLD.payload_json
  OR NEW.created_by <> OLD.created_by
  OR NEW.created_at <> OLD.created_at
BEGIN
    SELECT RAISE(ABORT, 'continuation execution content is immutable');
END;
`
