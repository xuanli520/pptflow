package store

// migrationV2 is the initial hard-cutover control-plane schema. It has no
// workspace-index tables and no identity bridge to the retired implementation.
const migrationV2 = `
CREATE TABLE IF NOT EXISTS store_metadata (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks_v2 (
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
CREATE INDEX IF NOT EXISTS idx_tasks_v2_slug ON tasks_v2(slug);
CREATE INDEX IF NOT EXISTS idx_tasks_v2_current_revision ON tasks_v2(current_revision_id);

CREATE TABLE IF NOT EXISTS task_revisions (
    id                 TEXT PRIMARY KEY,
    task_id            TEXT NOT NULL REFERENCES tasks_v2(id) ON DELETE RESTRICT,
    version_number     INTEGER NOT NULL CHECK (version_number > 0),
    parent_revision_id TEXT REFERENCES task_revisions(id) ON DELETE RESTRICT,
    origin             TEXT NOT NULL
                       CHECK (origin IN ('generated', 'imported', 'manual', 'repair', 'fork', 'rollback')),
    task_digest        TEXT NOT NULL,
    proposal_digest    TEXT NOT NULL DEFAULT '',
    manifest_id        TEXT NOT NULL DEFAULT '',
    state              TEXT NOT NULL
                       CHECK (state IN ('sealed', 'validated', 'released', 'superseded')),
    validation_evidence_manifest TEXT NOT NULL DEFAULT '',
    state_version      INTEGER NOT NULL DEFAULT 1 CHECK (state_version > 0),
    state_updated_by   TEXT NOT NULL,
    state_updated_at   DATETIME NOT NULL,
    change_summary     TEXT NOT NULL DEFAULT '',
    metadata_json      TEXT NOT NULL DEFAULT '{}',
    created_by         TEXT NOT NULL,
    created_at         DATETIME NOT NULL,
    UNIQUE(task_id, version_number)
);
CREATE INDEX IF NOT EXISTS idx_task_revisions_task ON task_revisions(task_id, version_number DESC);
CREATE INDEX IF NOT EXISTS idx_task_revisions_digest ON task_revisions(task_digest);
CREATE TRIGGER IF NOT EXISTS task_revisions_content_immutable
BEFORE UPDATE ON task_revisions
WHEN NEW.id <> OLD.id
  OR NEW.task_id <> OLD.task_id
  OR NEW.version_number <> OLD.version_number
  OR NEW.parent_revision_id IS NOT OLD.parent_revision_id
  OR NEW.origin <> OLD.origin
  OR NEW.task_digest <> OLD.task_digest
  OR NEW.proposal_digest <> OLD.proposal_digest
  OR NEW.manifest_id <> OLD.manifest_id
  OR NEW.change_summary <> OLD.change_summary
  OR NEW.metadata_json <> OLD.metadata_json
  OR NEW.created_by <> OLD.created_by
  OR NEW.created_at <> OLD.created_at
BEGIN
    SELECT RAISE(ABORT, 'task revision content is immutable');
END;
CREATE TRIGGER IF NOT EXISTS task_revisions_state_transition
BEFORE UPDATE ON task_revisions
WHEN NEW.state_version <> OLD.state_version + 1
  OR NEW.state_updated_by = ''
  OR NEW.state_updated_at IS NULL
  OR NOT (
      (OLD.state = 'sealed' AND NEW.state = 'validated' AND NEW.validation_evidence_manifest <> '')
      OR (OLD.state = 'sealed' AND NEW.state = 'superseded' AND NEW.validation_evidence_manifest = '')
      OR (OLD.state = 'validated' AND NEW.state = 'released' AND NEW.validation_evidence_manifest = OLD.validation_evidence_manifest)
      OR (OLD.state = 'validated' AND NEW.state = 'superseded' AND NEW.validation_evidence_manifest = OLD.validation_evidence_manifest)
      OR (OLD.state = 'released' AND NEW.state = 'superseded' AND NEW.validation_evidence_manifest = OLD.validation_evidence_manifest)
  )
BEGIN
    SELECT RAISE(ABORT, 'invalid task revision state transition');
END;
CREATE TRIGGER IF NOT EXISTS task_revisions_no_delete
BEFORE DELETE ON task_revisions
BEGIN
    SELECT RAISE(ABORT, 'task revisions are immutable');
END;

CREATE TABLE IF NOT EXISTS workspaces_v2 (
    id          TEXT PRIMARY KEY,
    root_uri    TEXT NOT NULL,
    purpose     TEXT NOT NULL,
    task_id     TEXT NOT NULL REFERENCES tasks_v2(id) ON DELETE RESTRICT,
    revision_id TEXT REFERENCES task_revisions(id) ON DELETE RESTRICT,
    run_id      TEXT,
    state       TEXT NOT NULL DEFAULT 'active'
                CHECK (state IN ('active', 'released', 'trash', 'purged')),
    created_at  DATETIME NOT NULL,
    updated_at  DATETIME NOT NULL,
    version     INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    UNIQUE(root_uri)
);
CREATE INDEX IF NOT EXISTS idx_workspaces_v2_task ON workspaces_v2(task_id);

CREATE TABLE IF NOT EXISTS workflow_runs (
    id                        TEXT PRIMARY KEY,
    task_id                   TEXT NOT NULL REFERENCES tasks_v2(id) ON DELETE RESTRICT,
    revision_id               TEXT NOT NULL REFERENCES task_revisions(id) ON DELETE RESTRICT,
    workflow_template_id      TEXT NOT NULL,
    workflow_template_version TEXT NOT NULL,
    resolved_profile_hash     TEXT NOT NULL,
    definition_hash           TEXT NOT NULL,
    run_manifest_json         TEXT NOT NULL DEFAULT '{}',
    parent_run_id             TEXT REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    trigger                   TEXT NOT NULL,
    execution_epoch           INTEGER NOT NULL DEFAULT 0 CHECK (execution_epoch >= 0),
    status                    TEXT NOT NULL DEFAULT 'queued'
                              CHECK (status IN (
                                  'queued', 'running', 'pause_requested', 'pausing', 'paused',
                                  'resume_requested', 'waiting_review', 'waiting_continuation',
                                  'succeeded', 'failed_recoverable', 'failed_terminal',
                                  'cancel_requested', 'stop_requested', 'canceling', 'canceled',
                                  'interrupted', 'in_doubt'
                              )),
    created_by                TEXT NOT NULL,
    created_at                DATETIME NOT NULL,
    started_at                DATETIME,
    finished_at               DATETIME,
    version                   INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE INDEX IF NOT EXISTS idx_workflow_runs_task ON workflow_runs(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_workflow_runs_revision ON workflow_runs(revision_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_workflow_runs_status ON workflow_runs(status);

CREATE TABLE IF NOT EXISTS run_attempts (
    id          TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    ordinal     INTEGER NOT NULL CHECK (ordinal > 0),
    trigger     TEXT NOT NULL,
    resume_from TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL,
    created_at  DATETIME NOT NULL,
    started_at  DATETIME,
    finished_at DATETIME,
    UNIQUE(run_id, ordinal)
);
CREATE INDEX IF NOT EXISTS idx_run_attempts_run ON run_attempts(run_id, ordinal);

CREATE TABLE IF NOT EXISTS stage_attempts (
    id                        TEXT PRIMARY KEY,
    run_id                    TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    retry_of_stage_attempt_id TEXT REFERENCES stage_attempts(id) ON DELETE RESTRICT,
    stage_key                 TEXT NOT NULL,
    stage_group               TEXT NOT NULL,
    ordinal                   INTEGER NOT NULL CHECK (ordinal > 0),
    input_fingerprint         TEXT NOT NULL,
    execution_status          TEXT NOT NULL DEFAULT 'queued'
                              CHECK (execution_status IN ('queued', 'running', 'completed', 'infra_failed', 'interrupted', 'in_doubt', 'reconciling', 'canceled')),
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
CREATE INDEX IF NOT EXISTS idx_stage_attempts_run ON stage_attempts(run_id, ordinal);
CREATE INDEX IF NOT EXISTS idx_stage_attempts_status ON stage_attempts(execution_status);

CREATE TABLE IF NOT EXISTS node_attempts (
    id               TEXT PRIMARY KEY,
    stage_attempt_id TEXT NOT NULL REFERENCES stage_attempts(id) ON DELETE RESTRICT,
    node_id          TEXT NOT NULL,
    generation       INTEGER NOT NULL CHECK (generation >= 0),
    attempt          INTEGER NOT NULL CHECK (attempt > 0),
    status           TEXT NOT NULL,
    idempotency_key  TEXT NOT NULL DEFAULT '',
    started_at       DATETIME,
    finished_at      DATETIME,
    error_text       TEXT NOT NULL DEFAULT '',
    UNIQUE(stage_attempt_id, node_id, generation, attempt)
);
CREATE INDEX IF NOT EXISTS idx_node_attempts_stage ON node_attempts(stage_attempt_id);

CREATE TABLE IF NOT EXISTS turn_checkpoints (
    id               TEXT PRIMARY KEY,
    node_attempt_id  TEXT NOT NULL REFERENCES node_attempts(id) ON DELETE RESTRICT,
    turn             INTEGER NOT NULL CHECK (turn > 0),
    substep          TEXT NOT NULL,
    status           TEXT NOT NULL,
    input_digest     TEXT NOT NULL DEFAULT '',
    artifact_id      TEXT NOT NULL DEFAULT '',
    payload_json     TEXT NOT NULL DEFAULT '{}',
    created_at       DATETIME NOT NULL,
    finished_at      DATETIME,
    UNIQUE(node_attempt_id, turn, substep)
);
CREATE INDEX IF NOT EXISTS idx_turn_checkpoints_node ON turn_checkpoints(node_attempt_id, turn);

CREATE TABLE IF NOT EXISTS review_requests (
    id                       TEXT PRIMARY KEY,
    revision_id              TEXT NOT NULL REFERENCES task_revisions(id) ON DELETE RESTRICT,
    evidence_manifest_digest TEXT NOT NULL,
    state                    TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'closed')),
    created_by               TEXT NOT NULL,
    created_at               DATETIME NOT NULL,
    closed_at                DATETIME
);
CREATE INDEX IF NOT EXISTS idx_review_requests_revision ON review_requests(revision_id, created_at DESC);

CREATE TABLE IF NOT EXISTS review_decisions (
    id                       TEXT PRIMARY KEY,
    review_request_id        TEXT NOT NULL REFERENCES review_requests(id) ON DELETE RESTRICT,
    revision_id              TEXT NOT NULL REFERENCES task_revisions(id) ON DELETE RESTRICT,
    action                   TEXT NOT NULL CHECK (action IN ('approve', 'request_changes', 'reject_terminal')),
    expected_revision_digest TEXT NOT NULL,
    actor                    TEXT NOT NULL,
    reason                   TEXT NOT NULL DEFAULT '',
    created_at               DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_review_decisions_revision ON review_decisions(revision_id, created_at DESC);

CREATE TABLE IF NOT EXISTS quota_policies (
    id           TEXT PRIMARY KEY,
    policy_key   TEXT NOT NULL UNIQUE,
    definition_json TEXT NOT NULL,
    created_at   DATETIME NOT NULL,
    updated_at   DATETIME NOT NULL,
    version      INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE TABLE IF NOT EXISTS quota_accounts (
    id           TEXT PRIMARY KEY,
    scope_type   TEXT NOT NULL,
    scope_id     TEXT NOT NULL,
    dimension    TEXT NOT NULL,
    limit_units  INTEGER NOT NULL,
    version      INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    updated_at   DATETIME NOT NULL,
    UNIQUE(scope_type, scope_id, dimension)
);
CREATE TABLE IF NOT EXISTS quota_ledger_entries (
    id             TEXT PRIMARY KEY,
    account_id     TEXT NOT NULL REFERENCES quota_accounts(id) ON DELETE RESTRICT,
    operation_key  TEXT NOT NULL,
    reserved_units INTEGER NOT NULL DEFAULT 0,
    consumed_units INTEGER NOT NULL DEFAULT 0,
    released_units INTEGER NOT NULL DEFAULT 0,
    created_at     DATETIME NOT NULL,
    UNIQUE(account_id, operation_key)
);
CREATE TABLE IF NOT EXISTS quota_leases (
    id             TEXT PRIMARY KEY,
    account_id     TEXT NOT NULL REFERENCES quota_accounts(id) ON DELETE RESTRICT,
    job_id         TEXT NOT NULL DEFAULT '',
    dimension      TEXT NOT NULL,
    reserved_units INTEGER NOT NULL,
    consumed_units INTEGER NOT NULL DEFAULT 0,
    released_units INTEGER NOT NULL DEFAULT 0,
    fencing_token  INTEGER NOT NULL,
    expires_at     DATETIME NOT NULL,
    state          TEXT NOT NULL,
    created_at     DATETIME NOT NULL,
    updated_at     DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS usage_events (
    id            TEXT PRIMARY KEY,
    lease_id      TEXT NOT NULL REFERENCES quota_leases(id) ON DELETE RESTRICT,
    operation_key TEXT NOT NULL,
    units         INTEGER NOT NULL,
    created_at    DATETIME NOT NULL,
    UNIQUE(lease_id, operation_key)
);
CREATE TABLE IF NOT EXISTS budget_settlements (
    id          TEXT PRIMARY KEY,
    lease_id    TEXT NOT NULL REFERENCES quota_leases(id) ON DELETE RESTRICT,
    state       TEXT NOT NULL,
    detail_json TEXT NOT NULL DEFAULT '{}',
    created_at  DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS budget_grants (
    id               TEXT PRIMARY KEY,
    scope_type       TEXT NOT NULL,
    scope_id         TEXT NOT NULL,
    dimension        TEXT NOT NULL,
    delta            INTEGER NOT NULL,
    expected_version INTEGER NOT NULL,
    actor            TEXT NOT NULL,
    reason           TEXT NOT NULL,
    idempotency_key  TEXT NOT NULL UNIQUE,
    created_at       DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS jobs (
    id              TEXT PRIMARY KEY,
    command_type    TEXT NOT NULL,
    entity_type     TEXT NOT NULL,
    entity_id       TEXT NOT NULL,
    run_id          TEXT REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    stage_attempt_id TEXT REFERENCES stage_attempts(id) ON DELETE RESTRICT,
    state           TEXT NOT NULL DEFAULT 'queued'
                    CHECK (state IN ('queued', 'running', 'pause_requested', 'cancel_requested', 'stop_requested', 'paused', 'canceled', 'succeeded', 'failed', 'interrupted', 'in_doubt')),
    priority        INTEGER NOT NULL DEFAULT 0,
    payload_json    TEXT NOT NULL DEFAULT '{}',
    idempotency_key TEXT NOT NULL UNIQUE,
    created_by      TEXT NOT NULL,
    created_at      DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL,
    started_at      DATETIME,
    finished_at     DATETIME,
    version         INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE INDEX IF NOT EXISTS idx_jobs_state_priority ON jobs(state, priority DESC, created_at);
CREATE INDEX IF NOT EXISTS idx_jobs_entity ON jobs(entity_type, entity_id);

CREATE TABLE IF NOT EXISTS leases (
    id            TEXT PRIMARY KEY,
    resource_type TEXT NOT NULL,
    resource_id   TEXT NOT NULL,
    owner         TEXT NOT NULL,
    job_id        TEXT REFERENCES jobs(id) ON DELETE RESTRICT,
    expires_at    DATETIME NOT NULL,
    fencing_token INTEGER NOT NULL CHECK (fencing_token > 0),
    state         TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'released', 'expired')),
    created_at    DATETIME NOT NULL,
    updated_at    DATETIME NOT NULL,
    version       INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    UNIQUE(resource_type, resource_id, fencing_token)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_leases_active_resource
    ON leases(resource_type, resource_id) WHERE state = 'active';
CREATE INDEX IF NOT EXISTS idx_leases_expiry ON leases(state, expires_at);

CREATE TABLE IF NOT EXISTS audit_events (
    id            TEXT PRIMARY KEY,
    actor         TEXT NOT NULL,
    entity_type   TEXT NOT NULL,
    entity_id     TEXT NOT NULL,
    action        TEXT NOT NULL,
    reason        TEXT NOT NULL DEFAULT '',
    payload_json  TEXT NOT NULL DEFAULT '{}',
    operation_key TEXT NOT NULL DEFAULT '',
    created_at    DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_events_entity ON audit_events(entity_type, entity_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_actor ON audit_events(actor, created_at DESC);
CREATE TRIGGER IF NOT EXISTS audit_events_no_update
BEFORE UPDATE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit events are append-only');
END;
CREATE TRIGGER IF NOT EXISTS audit_events_no_delete
BEFORE DELETE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit events are append-only');
END;

CREATE TABLE IF NOT EXISTS outbox_events (
    id             TEXT PRIMARY KEY,
    topic          TEXT NOT NULL,
    entity_type    TEXT NOT NULL,
    entity_id      TEXT NOT NULL,
    payload_json   TEXT NOT NULL,
    idempotency_key TEXT NOT NULL DEFAULT '',
    state          TEXT NOT NULL DEFAULT 'pending',
    created_at     DATETIME NOT NULL,
    published_at   DATETIME
);
CREATE INDEX IF NOT EXISTS idx_outbox_events_state ON outbox_events(state, created_at);

CREATE TABLE IF NOT EXISTS releases (
    id             TEXT PRIMARY KEY,
    version        TEXT NOT NULL UNIQUE,
    revision_id    TEXT NOT NULL REFERENCES task_revisions(id) ON DELETE RESTRICT,
    task_id        TEXT NOT NULL REFERENCES tasks_v2(id) ON DELETE RESTRICT,
    task_digest    TEXT NOT NULL,
    package_ref    TEXT NOT NULL,
    evidence_ref   TEXT NOT NULL,
    published_at   DATETIME NOT NULL,
    withdrawn_at   DATETIME,
    created_by     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS release_channels (
    channel     TEXT PRIMARY KEY,
    release_id  TEXT NOT NULL REFERENCES releases(id) ON DELETE RESTRICT,
    updated_at  DATETIME NOT NULL,
    updated_by  TEXT NOT NULL,
    version     INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);

CREATE TABLE IF NOT EXISTS deletion_records (
    id            TEXT PRIMARY KEY,
    entity_type   TEXT NOT NULL,
    entity_id     TEXT NOT NULL,
    action        TEXT NOT NULL,
    state         TEXT NOT NULL,
    actor         TEXT NOT NULL,
    reason        TEXT NOT NULL DEFAULT '',
    created_at    DATETIME NOT NULL,
    completed_at  DATETIME
);
CREATE INDEX IF NOT EXISTS idx_deletion_records_entity ON deletion_records(entity_type, entity_id, created_at DESC);
`
