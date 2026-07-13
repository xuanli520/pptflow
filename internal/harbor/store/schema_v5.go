package store

// migrationV5 adds the durable ledger, control, reconciliation, and worker
// dispatch records needed by the V2 workflow control plane. The similarly
// named V2 tables remain historical compatibility data; V5 never mutates or
// reinterprets them.
const migrationV5 = `
CREATE TABLE IF NOT EXISTS quota_accounts_v5 (
    id             TEXT PRIMARY KEY,
    scope_kind     TEXT NOT NULL CHECK (scope_kind IN ('task', 'actor')),
    scope_id       TEXT NOT NULL,
    dimension      TEXT NOT NULL,
    limit_units    INTEGER NOT NULL CHECK (limit_units >= 0),
    consumed_units INTEGER NOT NULL DEFAULT 0 CHECK (consumed_units >= 0),
    reserved_units INTEGER NOT NULL DEFAULT 0 CHECK (reserved_units >= 0),
    version        INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at     DATETIME NOT NULL,
    updated_at     DATETIME NOT NULL,
    UNIQUE(scope_kind, scope_id, dimension)
);
CREATE INDEX IF NOT EXISTS idx_quota_accounts_v5_scope
    ON quota_accounts_v5(scope_kind, scope_id, dimension);

CREATE TABLE IF NOT EXISTS quota_leases_v5 (
    id              TEXT PRIMARY KEY,
    account_id      TEXT NOT NULL REFERENCES quota_accounts_v5(id) ON DELETE RESTRICT,
    admission_id    TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL UNIQUE,
    owner           TEXT NOT NULL,
    scope_kind      TEXT NOT NULL CHECK (scope_kind IN ('task', 'actor')),
    scope_id        TEXT NOT NULL,
    dimension       TEXT NOT NULL,
    reserved_units  INTEGER NOT NULL CHECK (reserved_units > 0),
    consumed_units  INTEGER NOT NULL DEFAULT 0 CHECK (consumed_units >= 0),
    released_units  INTEGER NOT NULL DEFAULT 0 CHECK (released_units >= 0),
    reclaim_policy  TEXT NOT NULL CHECK (reclaim_policy IN ('reclaim_unused', 'reclaim_never')),
    fencing_token   INTEGER NOT NULL CHECK (fencing_token > 0),
    ttl_ms          INTEGER NOT NULL CHECK (ttl_ms > 0),
    expires_at      DATETIME NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('active', 'settled', 'uncertain', 'expired')),
    created_at      DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL,
    settled_at      DATETIME,
    version         INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    CHECK (consumed_units + released_units <= reserved_units)
);
CREATE INDEX IF NOT EXISTS idx_quota_leases_v5_account_state
    ON quota_leases_v5(account_id, state, expires_at);
CREATE INDEX IF NOT EXISTS idx_quota_leases_v5_admission
    ON quota_leases_v5(admission_id, created_at);

CREATE TABLE IF NOT EXISTS quota_usage_events_v5 (
    id            TEXT PRIMARY KEY,
    operation_key TEXT NOT NULL,
    lease_id      TEXT NOT NULL REFERENCES quota_leases_v5(id) ON DELETE RESTRICT,
    fencing_token INTEGER NOT NULL CHECK (fencing_token > 0),
    units         INTEGER NOT NULL CHECK (units > 0),
    occurred_at   DATETIME NOT NULL,
    actor         TEXT NOT NULL,
    reason        TEXT NOT NULL DEFAULT '',
    recorded_at   DATETIME NOT NULL,
    UNIQUE(lease_id, operation_key)
);
CREATE INDEX IF NOT EXISTS idx_quota_usage_events_v5_lease
    ON quota_usage_events_v5(lease_id, occurred_at);

CREATE TABLE IF NOT EXISTS quota_heartbeats_v5 (
    id              TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    lease_id        TEXT NOT NULL REFERENCES quota_leases_v5(id) ON DELETE RESTRICT,
    owner           TEXT NOT NULL,
    fencing_token   INTEGER NOT NULL CHECK (fencing_token > 0),
    ttl_ms          INTEGER NOT NULL CHECK (ttl_ms > 0),
    expires_at      DATETIME NOT NULL,
    actor           TEXT NOT NULL,
    reason          TEXT NOT NULL DEFAULT '',
    created_at      DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS quota_settlements_v5 (
    id              TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    lease_id        TEXT NOT NULL REFERENCES quota_leases_v5(id) ON DELETE RESTRICT,
    kind            TEXT NOT NULL CHECK (kind IN ('settlement', 'reconcile')),
    outcome         TEXT NOT NULL CHECK (outcome IN ('completed', 'canceled', 'uncertain')),
    consumed_units  INTEGER NOT NULL CHECK (consumed_units >= 0),
    released_units  INTEGER NOT NULL CHECK (released_units >= 0),
    owner           TEXT NOT NULL,
    fencing_token   INTEGER NOT NULL CHECK (fencing_token > 0),
    actor           TEXT NOT NULL,
    reason          TEXT NOT NULL DEFAULT '',
    settled_at      DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_quota_settlements_v5_lease
    ON quota_settlements_v5(lease_id, settled_at);

CREATE TABLE IF NOT EXISTS quota_budget_grants_v5 (
    id               TEXT PRIMARY KEY,
    idempotency_key  TEXT NOT NULL UNIQUE,
    account_id       TEXT NOT NULL REFERENCES quota_accounts_v5(id) ON DELETE RESTRICT,
    scope_kind       TEXT NOT NULL CHECK (scope_kind IN ('task', 'actor')),
    scope_id         TEXT NOT NULL,
    dimension        TEXT NOT NULL,
    delta_units      INTEGER NOT NULL CHECK (delta_units > 0),
    previous_version INTEGER NOT NULL CHECK (previous_version > 0),
    version          INTEGER NOT NULL CHECK (version > previous_version),
    limit_units      INTEGER NOT NULL CHECK (limit_units >= 0),
    actor            TEXT NOT NULL,
    reason           TEXT NOT NULL,
    granted_at       DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS admission_decisions_v5 (
    id                  TEXT PRIMARY KEY,
    idempotency_key     TEXT NOT NULL UNIQUE,
    request_fingerprint TEXT NOT NULL,
    task_id             TEXT NOT NULL,
    actor               TEXT NOT NULL,
    accepted            BOOLEAN NOT NULL,
    reason              TEXT NOT NULL CHECK (reason IN ('accepted', 'quota_exhausted')),
    decided_at          DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_admission_decisions_v5_task
    ON admission_decisions_v5(task_id, decided_at DESC);

CREATE TABLE IF NOT EXISTS control_operations_v5 (
    id                   TEXT PRIMARY KEY,
    operation_key        TEXT NOT NULL UNIQUE,
    action               TEXT NOT NULL CHECK (action IN ('pause', 'cancel_stage', 'terminate')),
    run_id               TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    stage_attempt_id     TEXT REFERENCES stage_attempts(id) ON DELETE RESTRICT,
    checkpoint_sequence  INTEGER NOT NULL CHECK (checkpoint_sequence >= 0),
    execution_epoch      INTEGER NOT NULL CHECK (execution_epoch >= 0),
    subject_version      INTEGER NOT NULL CHECK (subject_version >= 0),
    subject_id           TEXT NOT NULL,
    subject_revision_id  TEXT NOT NULL,
    subject_digest       TEXT NOT NULL,
    workflow_fingerprint TEXT NOT NULL,
    actor                TEXT NOT NULL,
    reason               TEXT NOT NULL,
    grace_period_ms      INTEGER NOT NULL CHECK (grace_period_ms >= 0),
    status               TEXT NOT NULL CHECK (status IN ('requested', 'propagating', 'acknowledged', 'reconcile_required', 'failed')),
    checkpoint_id        TEXT NOT NULL DEFAULT '',
    quota_settlement_id  TEXT NOT NULL DEFAULT '',
    failure_reason       TEXT NOT NULL DEFAULT '',
    created_at           DATETIME NOT NULL,
    updated_at           DATETIME NOT NULL,
    version              INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    CHECK ((action = 'cancel_stage' AND stage_attempt_id IS NOT NULL)
        OR (action <> 'cancel_stage' AND stage_attempt_id IS NULL))
);
CREATE INDEX IF NOT EXISTS idx_control_operations_v5_run_status
    ON control_operations_v5(run_id, status, updated_at);

CREATE TABLE IF NOT EXISTS control_operation_transitions_v5 (
    id                  TEXT PRIMARY KEY,
    operation_id        TEXT NOT NULL REFERENCES control_operations_v5(id) ON DELETE RESTRICT,
    expected_version    INTEGER NOT NULL CHECK (expected_version > 0),
    status              TEXT NOT NULL CHECK (status IN ('propagating', 'acknowledged', 'reconcile_required', 'failed')),
    checkpoint_id       TEXT NOT NULL DEFAULT '',
    quota_settlement_id TEXT NOT NULL DEFAULT '',
    failure_reason      TEXT NOT NULL DEFAULT '',
    actor               TEXT NOT NULL,
    reason              TEXT NOT NULL DEFAULT '',
    created_at          DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_control_transitions_v5_operation
    ON control_operation_transitions_v5(operation_id, created_at);

CREATE TABLE IF NOT EXISTS runtime_termination_receipts_v5 (
    id                      TEXT PRIMARY KEY,
    control_operation_id    TEXT NOT NULL REFERENCES control_operations_v5(id) ON DELETE RESTRICT,
    runtime_scope_id        TEXT NOT NULL,
    observed_at             DATETIME NOT NULL,
    graceful                BOOLEAN NOT NULL,
    external_outcome_unknown BOOLEAN NOT NULL,
    payload_json            TEXT NOT NULL DEFAULT '{}',
    created_at              DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_runtime_receipts_v5_operation
    ON runtime_termination_receipts_v5(control_operation_id, observed_at, id);

CREATE TABLE IF NOT EXISTS side_effect_operations_v5 (
    id                 TEXT PRIMARY KEY,
    operation_key      TEXT NOT NULL UNIQUE,
    run_id             TEXT NOT NULL DEFAULT '',
    stage_attempt_id   TEXT NOT NULL DEFAULT '',
    effect_kind        TEXT NOT NULL,
    idempotency_key    TEXT NOT NULL UNIQUE,
    source_digest      TEXT NOT NULL,
    destination_digest TEXT NOT NULL DEFAULT '',
    receipt_ref        TEXT NOT NULL DEFAULT '',
    payload_json       TEXT NOT NULL,
    state              TEXT NOT NULL CHECK (state IN ('prepared', 'started', 'succeeded', 'failed', 'unknown')),
    created_at         DATETIME NOT NULL,
    updated_at         DATETIME NOT NULL,
    version            INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE INDEX IF NOT EXISTS idx_side_effect_operations_v5_state
    ON side_effect_operations_v5(state, updated_at);

CREATE TABLE IF NOT EXISTS reconciliation_attempts_v5 (
    id                       TEXT PRIMARY KEY,
    operation_key            TEXT NOT NULL UNIQUE,
    subject_type             TEXT NOT NULL,
    subject_id               TEXT NOT NULL,
    side_effect_operation_id TEXT REFERENCES side_effect_operations_v5(id) ON DELETE RESTRICT,
    control_operation_id     TEXT REFERENCES control_operations_v5(id) ON DELETE RESTRICT,
    ordinal                  INTEGER NOT NULL CHECK (ordinal > 0),
    state                    TEXT NOT NULL CHECK (state IN ('running', 'completed', 'failed')),
    observed_json            TEXT NOT NULL,
    resolution               TEXT NOT NULL DEFAULT '',
    created_at               DATETIME NOT NULL,
    finished_at              DATETIME,
    version                  INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    CHECK ((side_effect_operation_id IS NOT NULL AND control_operation_id IS NULL)
        OR (side_effect_operation_id IS NULL AND control_operation_id IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_reconciliation_attempts_v5_subject
    ON reconciliation_attempts_v5(subject_type, subject_id, created_at DESC);

CREATE TABLE IF NOT EXISTS capacity_pools_v5 (
    id          TEXT PRIMARY KEY,
    pool_key    TEXT NOT NULL UNIQUE,
    capacity    INTEGER NOT NULL CHECK (capacity > 0),
    created_at  DATETIME NOT NULL,
    updated_at  DATETIME NOT NULL,
    version     INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);

CREATE TABLE IF NOT EXISTS job_dispatch_claims_v5 (
    id                TEXT PRIMARY KEY,
    idempotency_key   TEXT NOT NULL UNIQUE,
    job_id            TEXT REFERENCES jobs(id) ON DELETE RESTRICT,
    owner             TEXT NOT NULL,
    lease_ttl_ms      INTEGER NOT NULL CHECK (lease_ttl_ms > 0),
    dispatch_lease_id TEXT REFERENCES leases(id) ON DELETE RESTRICT,
    capacity_pool_key TEXT NOT NULL DEFAULT '',
    capacity_lease_id TEXT REFERENCES leases(id) ON DELETE RESTRICT,
    state             TEXT NOT NULL CHECK (state IN ('active', 'released', 'expired', 'empty')),
    claimed_at        DATETIME NOT NULL,
    updated_at        DATETIME NOT NULL,
    CHECK ((state = 'empty' AND job_id IS NULL AND dispatch_lease_id IS NULL AND capacity_lease_id IS NULL)
        OR (state <> 'empty' AND job_id IS NOT NULL AND dispatch_lease_id IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_job_dispatch_claims_v5_active_job
    ON job_dispatch_claims_v5(job_id) WHERE state = 'active';
CREATE INDEX IF NOT EXISTS idx_job_dispatch_claims_v5_state
    ON job_dispatch_claims_v5(state, updated_at);
`
