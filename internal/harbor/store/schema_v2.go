package store

// migrationV2 is the sole, immutable V2 baseline relative to the retired V1 store.
// It creates the complete current control-plane schema in one transaction; no V3+
// migration history is accepted or recorded.
const migrationV2 = `
CREATE TABLE schema_version (version INTEGER NOT NULL);

-- table admission_decisions_v5
CREATE TABLE admission_decisions_v5 (
    id                  TEXT PRIMARY KEY,
    idempotency_key     TEXT NOT NULL UNIQUE,
    request_fingerprint TEXT NOT NULL,
    task_id             TEXT NOT NULL,
    actor               TEXT NOT NULL,
    accepted            BOOLEAN NOT NULL,
    reason              TEXT NOT NULL CHECK (reason IN ('accepted', 'quota_exhausted')),
    decided_at          DATETIME NOT NULL
);

-- table artifact_manifests_v4
CREATE TABLE artifact_manifests_v4 (
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

-- table artifact_refs_v4
CREATE TABLE artifact_refs_v4 (
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

-- table authoring_sources_v2
-- A source is the immutable, content-addressed repository snapshot from
-- which a Standard authoring Run may derive its first task revision. The
-- artifact reference is deliberately the same canonical SHA-256 identity as
-- the snapshot bytes; mutable paths and moving repository refs never enter
-- the durable subject boundary.
CREATE TABLE authoring_sources_v2 (
    id                      TEXT PRIMARY KEY,
    repository_url          TEXT NOT NULL,
    commit_sha              TEXT NOT NULL,
    snapshot_artifact_ref   TEXT NOT NULL,
    snapshot_content_digest TEXT NOT NULL,
    snapshot_schema_version TEXT NOT NULL,
    source_fingerprint      TEXT NOT NULL UNIQUE,
    idempotency_key         TEXT NOT NULL UNIQUE,
    created_by              TEXT NOT NULL,
    created_at              DATETIME NOT NULL,
    UNIQUE(repository_url, commit_sha, snapshot_content_digest, snapshot_schema_version),
    CHECK (length(commit_sha) IN (40, 64)),
    CHECK (commit_sha = lower(commit_sha)),
    CHECK (commit_sha NOT GLOB '*[^0-9a-f]*'),
    CHECK (snapshot_artifact_ref = snapshot_content_digest),
    CHECK (length(snapshot_content_digest) = 71),
    CHECK (substr(snapshot_content_digest, 1, 7) = 'sha256:'),
    CHECK (substr(snapshot_content_digest, 8) NOT GLOB '*[^0-9a-f]*'),
    CHECK (length(source_fingerprint) = 71),
    CHECK (substr(source_fingerprint, 1, 7) = 'sha256:'),
    CHECK (substr(source_fingerprint, 8) NOT GLOB '*[^0-9a-f]*')
);

-- table authoring_sessions_v2
-- A session freezes the Standard authoring contract over one source. It is
-- intentionally not a TaskRevision: it exists before a task has been
-- materialized and remains immutable after that materialization.
CREATE TABLE authoring_sessions_v2 (
    id                        TEXT PRIMARY KEY,
    source_id                 TEXT NOT NULL REFERENCES authoring_sources_v2(id) ON DELETE RESTRICT,
    target_task_id            TEXT NOT NULL UNIQUE REFERENCES tasks_v2(id) ON DELETE RESTRICT,
    workflow_template_id      TEXT NOT NULL,
    workflow_template_version TEXT NOT NULL,
    session_manifest_json     TEXT NOT NULL,
    session_fingerprint       TEXT NOT NULL UNIQUE,
    idempotency_key           TEXT NOT NULL UNIQUE,
    created_by                TEXT NOT NULL,
    created_at                DATETIME NOT NULL,
    UNIQUE(source_id, session_fingerprint),
    CHECK (length(session_fingerprint) = 71),
    CHECK (substr(session_fingerprint, 1, 7) = 'sha256:'),
    CHECK (substr(session_fingerprint, 8) NOT GLOB '*[^0-9a-f]*')
);

-- table authoring_run_input_artifacts_v2
-- A pre-materialization Run consumes the frozen source snapshot directly.
-- This is intentionally separate from run_input_artifacts, whose foreign key
-- contract is a real TaskRevision and would otherwise force a synthetic
-- revision into the authoring path.
CREATE TABLE authoring_run_input_artifacts_v2 (
    id                    TEXT PRIMARY KEY,
    run_id                TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    session_id            TEXT NOT NULL REFERENCES authoring_sessions_v2(id) ON DELETE RESTRICT,
    source_id             TEXT NOT NULL REFERENCES authoring_sources_v2(id) ON DELETE RESTRICT,
    source_fingerprint    TEXT NOT NULL,
    port                  TEXT NOT NULL,
    snapshot_artifact_ref TEXT NOT NULL,
    content_digest        TEXT NOT NULL,
    schema_version        TEXT NOT NULL,
    idempotency_key       TEXT NOT NULL UNIQUE,
    created_by            TEXT NOT NULL,
    created_at            DATETIME NOT NULL,
    UNIQUE(run_id, port),
    CHECK (snapshot_artifact_ref = content_digest),
    CHECK (length(content_digest) = 71),
    CHECK (substr(content_digest, 1, 7) = 'sha256:'),
    CHECK (substr(content_digest, 8) NOT GLOB '*[^0-9a-f]*'),
    CHECK (length(source_fingerprint) = 71),
    CHECK (substr(source_fingerprint, 1, 7) = 'sha256:'),
    CHECK (substr(source_fingerprint, 8) NOT GLOB '*[^0-9a-f]*')
);

-- table authoring_review_requests_v22
-- These request envelopes are purpose-built for the pre-materialization
-- source/session subject. They deliberately do not reuse review_requests,
-- which is a TaskRevision-only lifecycle contract.
CREATE TABLE authoring_review_requests_v22 (
    id                       TEXT PRIMARY KEY,
    run_id                   TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    authoring_session_id     TEXT NOT NULL REFERENCES authoring_sessions_v2(id) ON DELETE RESTRICT,
    authoring_source_id      TEXT NOT NULL REFERENCES authoring_sources_v2(id) ON DELETE RESTRICT,
    source_snapshot_digest   TEXT NOT NULL,
    definition_hash          TEXT NOT NULL,
    evidence_manifest_digest TEXT NOT NULL,
    request_fingerprint      TEXT NOT NULL UNIQUE,
    idempotency_key          TEXT NOT NULL UNIQUE,
    created_by               TEXT NOT NULL,
    created_at               DATETIME NOT NULL,
    CHECK (length(source_snapshot_digest) = 71),
    CHECK (substr(source_snapshot_digest, 1, 7) = 'sha256:'),
    CHECK (substr(source_snapshot_digest, 8) NOT GLOB '*[^0-9a-f]*')
);

-- table authoring_review_gate_bindings_v22
-- The binding is the immutable source/session analogue of a task-revision
-- review gate. Every operator decision and resolution references this fact.
CREATE TABLE authoring_review_gate_bindings_v22 (
    id                       TEXT PRIMARY KEY,
    review_request_id        TEXT NOT NULL UNIQUE REFERENCES authoring_review_requests_v22(id) ON DELETE RESTRICT,
    run_id                   TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    authoring_session_id     TEXT NOT NULL REFERENCES authoring_sessions_v2(id) ON DELETE RESTRICT,
    authoring_source_id      TEXT NOT NULL REFERENCES authoring_sources_v2(id) ON DELETE RESTRICT,
    source_snapshot_digest   TEXT NOT NULL,
    definition_hash          TEXT NOT NULL,
    stage_attempt_id         TEXT NOT NULL UNIQUE REFERENCES stage_attempts(id) ON DELETE RESTRICT,
    stage_key                TEXT NOT NULL,
    node_attempt_id          TEXT NOT NULL UNIQUE REFERENCES node_attempts(id) ON DELETE RESTRICT,
    node_generation          INTEGER NOT NULL CHECK (node_generation >= 0),
    node_attempt_ordinal     INTEGER NOT NULL CHECK (node_attempt_ordinal > 0),
    review_kind              TEXT NOT NULL,
    input_bindings_json      TEXT NOT NULL,
    input_fingerprint        TEXT NOT NULL,
    evidence_manifest_digest TEXT NOT NULL,
    binding_fingerprint      TEXT NOT NULL UNIQUE,
    created_at               DATETIME NOT NULL,
    CHECK (length(source_snapshot_digest) = 71),
    CHECK (substr(source_snapshot_digest, 1, 7) = 'sha256:'),
    CHECK (substr(source_snapshot_digest, 8) NOT GLOB '*[^0-9a-f]*')
);

-- table authoring_review_decisions_v22
-- At most one immutable operator decision may exist for an authoring review
-- request. Its presence transitions the derived gate state from open to
-- decided without mutating the request envelope.
CREATE TABLE authoring_review_decisions_v22 (
    id                   TEXT PRIMARY KEY,
    review_request_id    TEXT NOT NULL UNIQUE REFERENCES authoring_review_requests_v22(id) ON DELETE RESTRICT,
    binding_id           TEXT NOT NULL UNIQUE REFERENCES authoring_review_gate_bindings_v22(id) ON DELETE RESTRICT,
    action               TEXT NOT NULL CHECK (action IN ('approve', 'request_changes', 'reject_terminal')),
    decision_fingerprint TEXT NOT NULL UNIQUE,
    idempotency_key      TEXT NOT NULL UNIQUE,
    actor                TEXT NOT NULL,
    reason               TEXT NOT NULL DEFAULT '',
    created_at           DATETIME NOT NULL
);

-- table authoring_review_gate_resolutions_v22
-- A completion receipt is immutable and one-to-one with the operator decision.
-- Result evidence is deliberately opaque here: it is not misrepresented as a
-- TaskRevision artifact before materialize_task has created one.
CREATE TABLE authoring_review_gate_resolutions_v22 (
    id                          TEXT PRIMARY KEY,
    review_request_id           TEXT NOT NULL UNIQUE REFERENCES authoring_review_requests_v22(id) ON DELETE RESTRICT,
    binding_id                  TEXT NOT NULL UNIQUE REFERENCES authoring_review_gate_bindings_v22(id) ON DELETE RESTRICT,
    decision_id                 TEXT NOT NULL UNIQUE REFERENCES authoring_review_decisions_v22(id) ON DELETE RESTRICT,
    verdict                     TEXT NOT NULL CHECK (verdict IN ('pass', 'needs_repair', 'reject')),
    artifact_manifest_id        TEXT NOT NULL DEFAULT '',
    resolution_evidence_digest  TEXT NOT NULL,
    resolution_payload_json     TEXT NOT NULL,
    resolution_fingerprint      TEXT NOT NULL UNIQUE,
    idempotency_key             TEXT NOT NULL UNIQUE,
    created_by                  TEXT NOT NULL,
    created_at                  DATETIME NOT NULL
);

-- table run_input_artifacts
-- Immutable, run-scoped subject inputs. These are intentionally distinct
-- from stage-produced artifact refs: no synthetic producer StageAttempt is
-- needed to make a sealed revision available to a root workflow stage.
CREATE TABLE run_input_artifacts (
    id              TEXT PRIMARY KEY,
    run_id          TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    task_id         TEXT NOT NULL REFERENCES tasks_v2(id) ON DELETE RESTRICT,
    revision_id     TEXT NOT NULL REFERENCES task_revisions(id) ON DELETE RESTRICT,
    revision_digest TEXT NOT NULL,
    port            TEXT NOT NULL,
    content_digest  TEXT NOT NULL,
    schema_version  TEXT NOT NULL,
    size_bytes      INTEGER NOT NULL CHECK (size_bytes >= 0),
    idempotency_key TEXT NOT NULL UNIQUE,
    created_by      TEXT NOT NULL,
    created_at      DATETIME NOT NULL,
    UNIQUE (run_id, port)
);

-- table audit_events
CREATE TABLE audit_events (
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

-- table budget_grants
CREATE TABLE budget_grants (
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

-- table budget_settlements
CREATE TABLE budget_settlements (
    id          TEXT PRIMARY KEY,
    lease_id    TEXT NOT NULL REFERENCES quota_leases(id) ON DELETE RESTRICT,
    state       TEXT NOT NULL,
    detail_json TEXT NOT NULL DEFAULT '{}',
    created_at  DATETIME NOT NULL
);

-- table candidate_gc_operations_v10
CREATE TABLE candidate_gc_operations_v10 (
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

-- table capacity_pools_v5
CREATE TABLE capacity_pools_v5 (
    id          TEXT PRIMARY KEY,
    pool_key    TEXT NOT NULL UNIQUE,
    capacity    INTEGER NOT NULL CHECK (capacity > 0),
    created_at  DATETIME NOT NULL,
    updated_at  DATETIME NOT NULL,
    version     INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);

-- table change_operations_v8
CREATE TABLE change_operations_v8 (
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

-- table codeedge_compliance_records_v20
CREATE TABLE codeedge_compliance_records_v20 (
    id                        TEXT PRIMARY KEY,
    run_id                    TEXT NOT NULL UNIQUE REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    task_id                   TEXT NOT NULL REFERENCES tasks_v2(id) ON DELETE RESTRICT,
    revision_id               TEXT NOT NULL REFERENCES task_revisions(id) ON DELETE RESTRICT,
    task_digest               TEXT NOT NULL,
    status                    TEXT NOT NULL CHECK (status IN ('approved', 'rejected')),
    evaluator_evidence_handoff_id TEXT NOT NULL UNIQUE REFERENCES codeedge_evaluator_evidence_handoffs_v2(id) ON DELETE RESTRICT,
    evaluator_evidence_handoff_fingerprint TEXT NOT NULL,
    qwen_receipt_json         TEXT NOT NULL,
    opus_receipt_json         TEXT NOT NULL,
    submission_receipt_json   TEXT NOT NULL,
    decision_json             TEXT NOT NULL,
    decision_fingerprint      TEXT NOT NULL,
    authorization_json        TEXT NOT NULL DEFAULT '',
    authorization_fingerprint TEXT NOT NULL DEFAULT '',
    idempotency_key           TEXT NOT NULL UNIQUE,
    created_by                TEXT NOT NULL,
    created_at                DATETIME NOT NULL,
    CHECK (
        (status = 'approved' AND length(authorization_json) > 0 AND length(authorization_fingerprint) > 0)
        OR
        (status = 'rejected' AND authorization_json = '' AND authorization_fingerprint = '')
    )
);

-- table codeedge_evaluator_evidence_handoffs_v2
-- The parent CodeEdge Phase-1 Run remains the only compliance/package owner.
-- This immutable record links it to the closed evaluator child Run without
-- copying or re-labelling the child-owned Harbor evidence.
CREATE TABLE codeedge_evaluator_evidence_handoffs_v2 (
    id                                TEXT PRIMARY KEY,
    parent_run_id                     TEXT NOT NULL UNIQUE REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    child_run_id                      TEXT NOT NULL UNIQUE REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    task_id                           TEXT NOT NULL REFERENCES tasks_v2(id) ON DELETE RESTRICT,
    revision_id                       TEXT NOT NULL REFERENCES task_revisions(id) ON DELETE RESTRICT,
    task_digest                       TEXT NOT NULL,
    parent_catalog_fingerprint        TEXT NOT NULL,
    parent_lock_fingerprint           TEXT NOT NULL,
    parent_manifest_fingerprint       TEXT NOT NULL,
    parent_definition_fingerprint     TEXT NOT NULL,
    child_catalog_fingerprint         TEXT NOT NULL,
    child_lock_fingerprint            TEXT NOT NULL,
    child_manifest_fingerprint        TEXT NOT NULL,
    child_definition_fingerprint      TEXT NOT NULL,
    qwen_stage_attempt_id             TEXT NOT NULL UNIQUE REFERENCES stage_attempts(id) ON DELETE RESTRICT,
    qwen_bundle_artifact_id           TEXT NOT NULL UNIQUE REFERENCES artifact_refs_v4(id) ON DELETE RESTRICT,
    qwen_bundle_content_digest        TEXT NOT NULL,
    qwen_bundle_schema_version        TEXT NOT NULL,
    qwen_screenshot_artifact_id       TEXT NOT NULL UNIQUE REFERENCES artifact_refs_v4(id) ON DELETE RESTRICT,
    qwen_screenshot_content_digest    TEXT NOT NULL,
    qwen_screenshot_schema_version    TEXT NOT NULL,
    qwen_trial_set_fingerprint        TEXT NOT NULL,
    opus_stage_attempt_id             TEXT NOT NULL UNIQUE REFERENCES stage_attempts(id) ON DELETE RESTRICT,
    opus_bundle_artifact_id           TEXT NOT NULL UNIQUE REFERENCES artifact_refs_v4(id) ON DELETE RESTRICT,
    opus_bundle_content_digest        TEXT NOT NULL,
    opus_bundle_schema_version        TEXT NOT NULL,
    opus_screenshot_artifact_id       TEXT NOT NULL UNIQUE REFERENCES artifact_refs_v4(id) ON DELETE RESTRICT,
    opus_screenshot_content_digest    TEXT NOT NULL,
    opus_screenshot_schema_version    TEXT NOT NULL,
    opus_trial_set_fingerprint        TEXT NOT NULL,
    handoff_json                      TEXT NOT NULL,
    handoff_fingerprint               TEXT NOT NULL,
    idempotency_key                   TEXT NOT NULL UNIQUE,
    created_by                        TEXT NOT NULL,
    created_at                        DATETIME NOT NULL,
    CHECK (parent_run_id <> child_run_id),
    CHECK (qwen_stage_attempt_id <> opus_stage_attempt_id),
    CHECK (qwen_bundle_artifact_id <> qwen_screenshot_artifact_id),
    CHECK (opus_bundle_artifact_id <> opus_screenshot_artifact_id),
    CHECK (qwen_bundle_artifact_id <> opus_bundle_artifact_id),
    CHECK (qwen_screenshot_artifact_id <> opus_screenshot_artifact_id)
);

-- table continuation_commands_v4
CREATE TABLE continuation_commands_v4 (
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

-- table continuation_executions_v4
CREATE TABLE continuation_executions_v4 (
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

-- table control_operation_transitions_v5
CREATE TABLE control_operation_transitions_v5 (
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

-- table control_operations_v5
CREATE TABLE control_operations_v5 (
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

-- table deletion_records
CREATE TABLE deletion_records (
    id            TEXT PRIMARY KEY,
    entity_type   TEXT NOT NULL,
    entity_id     TEXT NOT NULL,
    action        TEXT NOT NULL,
    state         TEXT NOT NULL,
    actor         TEXT NOT NULL,
    reason        TEXT NOT NULL DEFAULT '',
    created_at    DATETIME NOT NULL,
    completed_at  DATETIME,
    version       INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);

-- table entity_id_registry
CREATE TABLE entity_id_registry (
    id            TEXT NOT NULL PRIMARY KEY,
    entity_type   TEXT NOT NULL,
    registered_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- table frozen_plans_v4
CREATE TABLE frozen_plans_v4 (
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

-- table job_dispatch_claims_v5
CREATE TABLE job_dispatch_claims_v5 (
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
    run_id            TEXT NOT NULL DEFAULT '',
    CHECK ((state = 'empty' AND job_id IS NULL AND dispatch_lease_id IS NULL AND capacity_lease_id IS NULL)
        OR (state <> 'empty' AND job_id IS NOT NULL AND dispatch_lease_id IS NOT NULL))
);

-- table jobs
CREATE TABLE jobs (
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
    failure_code         TEXT NOT NULL DEFAULT '',
    failure_message      TEXT NOT NULL DEFAULT '',
    failure_details_json TEXT NOT NULL DEFAULT '{}',
    idempotency_key TEXT NOT NULL UNIQUE,
    created_by      TEXT NOT NULL,
    created_at      DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL,
    started_at      DATETIME,
    finished_at     DATETIME,
    version         INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    CHECK (
        (state IN ('failed', 'in_doubt') AND failure_code <> '' AND failure_message <> '' AND failure_details_json <> '')
        OR
        (state NOT IN ('failed', 'in_doubt') AND failure_code = '' AND failure_message = '' AND failure_details_json = '{}')
    )
);

-- table leases
CREATE TABLE leases (
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

-- table lifecycle_operations_v12
CREATE TABLE lifecycle_operations_v12 (
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
    expected_task_id                TEXT NOT NULL DEFAULT '',
    expected_revision_id            TEXT NOT NULL DEFAULT '',
    expected_run_id                 TEXT NOT NULL DEFAULT '',
    expected_release_id             TEXT NOT NULL DEFAULT '',
    expected_review_request_id      TEXT NOT NULL DEFAULT '',
    expected_codeedge_compliance_record_id TEXT NOT NULL DEFAULT '',
    expected_codeedge_authorization_fingerprint TEXT NOT NULL DEFAULT '',
    actor                           TEXT NOT NULL,
    reason                          TEXT NOT NULL,
    state                           TEXT NOT NULL CHECK (state IN ('prepared', 'completed')),
    result_json                     TEXT NOT NULL DEFAULT '',
    created_at                      DATETIME NOT NULL,
    updated_at                      DATETIME NOT NULL,
    completed_at                    DATETIME,
    version                         INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);

-- table mutation_receipts_v4
CREATE TABLE mutation_receipts_v4 (
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

-- table node_attempts
CREATE TABLE node_attempts (
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
    created_at       DATETIME,
    version          INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    UNIQUE(stage_attempt_id, node_id, generation, attempt)
);

-- table outbox_delivery_operations_v9
CREATE TABLE outbox_delivery_operations_v9 (
    id                  TEXT PRIMARY KEY,
    idempotency_key     TEXT NOT NULL UNIQUE,
    kind                TEXT NOT NULL CHECK (kind IN ('claim', 'heartbeat', 'ack', 'nack')),
    request_fingerprint TEXT NOT NULL,
    response_json       TEXT NOT NULL,
    created_at          DATETIME NOT NULL
);

-- table outbox_events
CREATE TABLE outbox_events (
    id             TEXT PRIMARY KEY,
    topic          TEXT NOT NULL,
    entity_type    TEXT NOT NULL,
    entity_id      TEXT NOT NULL,
    payload_json   TEXT NOT NULL,
    idempotency_key TEXT NOT NULL DEFAULT '',
    state          TEXT NOT NULL DEFAULT 'pending',
    created_at     DATETIME NOT NULL,
    published_at   DATETIME,
    version        INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    available_at   DATETIME,
    lease_owner    TEXT NOT NULL DEFAULT '',
    lease_expires_at DATETIME,
    lease_fencing_token INTEGER NOT NULL DEFAULT 0 CHECK (lease_fencing_token >= 0),
    delivery_count INTEGER NOT NULL DEFAULT 0 CHECK (delivery_count >= 0),
    last_error     TEXT NOT NULL DEFAULT '',
    updated_at     DATETIME
);

-- table prepared_changes_v4
CREATE TABLE prepared_changes_v4 (
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

-- table quota_account_policy_bindings_v11
CREATE TABLE quota_account_policy_bindings_v11 (
    account_id          TEXT PRIMARY KEY REFERENCES quota_accounts_v5(id) ON DELETE RESTRICT,
    policy_id           TEXT NOT NULL,
    policy_version      TEXT NOT NULL,
    policy_fingerprint  TEXT NOT NULL,
    initial_limit_units INTEGER NOT NULL CHECK (initial_limit_units > 0),
    bound_at            DATETIME NOT NULL
);

-- table quota_accounts
CREATE TABLE quota_accounts (
    id           TEXT PRIMARY KEY,
    scope_type   TEXT NOT NULL,
    scope_id     TEXT NOT NULL,
    dimension    TEXT NOT NULL,
    limit_units  INTEGER NOT NULL,
    version      INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    updated_at   DATETIME NOT NULL,
    UNIQUE(scope_type, scope_id, dimension)
);

-- table quota_accounts_v5
CREATE TABLE quota_accounts_v5 (
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

-- table quota_budget_grants_v5
CREATE TABLE quota_budget_grants_v5 (
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

-- table quota_heartbeats_v5
CREATE TABLE quota_heartbeats_v5 (
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

-- table quota_leases
CREATE TABLE quota_leases (
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

-- table quota_leases_v5
CREATE TABLE quota_leases_v5 (
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

-- table quota_ledger_entries
CREATE TABLE quota_ledger_entries (
    id             TEXT PRIMARY KEY,
    account_id     TEXT NOT NULL REFERENCES quota_accounts(id) ON DELETE RESTRICT,
    operation_key  TEXT NOT NULL,
    reserved_units INTEGER NOT NULL DEFAULT 0,
    consumed_units INTEGER NOT NULL DEFAULT 0,
    released_units INTEGER NOT NULL DEFAULT 0,
    created_at     DATETIME NOT NULL,
    UNIQUE(account_id, operation_key)
);

-- table quota_policies
CREATE TABLE quota_policies (
    id           TEXT PRIMARY KEY,
    policy_key   TEXT NOT NULL UNIQUE,
    definition_json TEXT NOT NULL,
    created_at   DATETIME NOT NULL,
    updated_at   DATETIME NOT NULL,
    version      INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);

-- table quota_settlements_v5
CREATE TABLE quota_settlements_v5 (
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

-- table quota_usage_events_v5
CREATE TABLE quota_usage_events_v5 (
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

-- table reconciliation_attempts_v5
CREATE TABLE reconciliation_attempts_v5 (
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

-- table release_channels
CREATE TABLE release_channels (
    channel     TEXT PRIMARY KEY,
    release_id  TEXT NOT NULL REFERENCES releases(id) ON DELETE RESTRICT,
    updated_at  DATETIME NOT NULL,
    updated_by  TEXT NOT NULL,
    version     INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
);

-- table release_withdraw_operations_v10
CREATE TABLE release_withdraw_operations_v10 (
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

-- table release_withdraw_receipts_v10
CREATE TABLE release_withdraw_receipts_v10 (
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

-- table releases
CREATE TABLE releases (
    id             TEXT PRIMARY KEY,
    version        TEXT NOT NULL UNIQUE,
    revision_id    TEXT NOT NULL REFERENCES task_revisions(id) ON DELETE RESTRICT,
    task_id        TEXT NOT NULL REFERENCES tasks_v2(id) ON DELETE RESTRICT,
    task_digest    TEXT NOT NULL,
    package_ref    TEXT NOT NULL,
    evidence_ref   TEXT NOT NULL,
    published_at   DATETIME NOT NULL,
    withdrawn_at   DATETIME,
    created_by     TEXT NOT NULL,
    withdrawn_by   TEXT NOT NULL DEFAULT '',
    record_version INTEGER NOT NULL DEFAULT 1 CHECK (record_version > 0)
);

-- table repair_sessions_v4
CREATE TABLE repair_sessions_v4 (
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

-- table review_decisions
CREATE TABLE review_decisions (
    id                       TEXT PRIMARY KEY,
    review_request_id        TEXT NOT NULL REFERENCES review_requests(id) ON DELETE RESTRICT,
    revision_id              TEXT NOT NULL REFERENCES task_revisions(id) ON DELETE RESTRICT,
    action                   TEXT NOT NULL CHECK (action IN ('approve', 'request_changes', 'reject_terminal')),
    expected_revision_digest TEXT NOT NULL,
    actor                    TEXT NOT NULL,
    reason                   TEXT NOT NULL DEFAULT '',
    created_at               DATETIME NOT NULL
);

-- table review_gate_bindings_v15
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

-- table review_requests
CREATE TABLE review_requests (
    id                       TEXT PRIMARY KEY,
    revision_id              TEXT NOT NULL REFERENCES task_revisions(id) ON DELETE RESTRICT,
    evidence_manifest_digest TEXT NOT NULL,
    state                    TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'closed')),
    created_by               TEXT NOT NULL,
    created_at               DATETIME NOT NULL,
    closed_at                DATETIME
);

-- table revision_candidate_identity_reservations_v8
CREATE TABLE revision_candidate_identity_reservations_v8 (
    reserved_id       TEXT PRIMARY KEY,
    candidate_id      TEXT NOT NULL REFERENCES revision_candidates_v8(id) ON DELETE RESTRICT,
    intended_type     TEXT NOT NULL CHECK (intended_type IN ('task_revision', 'workflow_run')),
    created_at        DATETIME NOT NULL,
    UNIQUE(candidate_id, intended_type)
);

-- table revision_candidates_v8
CREATE TABLE revision_candidates_v8 (
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
    checkout_tombstoned_at   DATETIME,
    checkout_tombstoned_by   TEXT NOT NULL DEFAULT '',
    version                  INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    UNIQUE(prepared_change_id),
    UNIQUE(mutation_receipt_id),
    UNIQUE(frozen_plan_id)
);

-- table run_attempts
CREATE TABLE run_attempts (
    id          TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    ordinal     INTEGER NOT NULL CHECK (ordinal > 0),
    trigger     TEXT NOT NULL,
    resume_from TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL,
    created_at  DATETIME NOT NULL,
    started_at  DATETIME,
    finished_at DATETIME,
    version     INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    UNIQUE(run_id, ordinal)
);

-- table run_worker_handoffs_v16
CREATE TABLE run_worker_handoffs_v16 (
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

-- table runtime_termination_receipts_v5
CREATE TABLE runtime_termination_receipts_v5 (
    id                      TEXT PRIMARY KEY,
    control_operation_id    TEXT NOT NULL REFERENCES control_operations_v5(id) ON DELETE RESTRICT,
    runtime_scope_id        TEXT NOT NULL,
    observed_at             DATETIME NOT NULL,
    graceful                BOOLEAN NOT NULL,
    external_outcome_unknown BOOLEAN NOT NULL,
    payload_json            TEXT NOT NULL DEFAULT '{}',
    created_at              DATETIME NOT NULL
);

-- table side_effect_operations_v5
CREATE TABLE side_effect_operations_v5 (
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

-- table stage_attempts
CREATE TABLE "stage_attempts" (
    id                        TEXT PRIMARY KEY,
    run_id                    TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    retry_of_stage_attempt_id TEXT REFERENCES "stage_attempts"(id) ON DELETE RESTRICT,
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

-- table store_metadata
CREATE TABLE store_metadata (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at DATETIME NOT NULL
);

-- table task_purge_operations_v7
CREATE TABLE task_purge_operations_v7 (
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

-- table task_revisions
CREATE TABLE task_revisions (
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

-- table authoring_task_materializations_v2
-- This is the one-way bridge from a pre-task AuthoringSession to the first
-- real immutable TaskRevision. It is written in the same transaction as the
-- Task and TaskRevision, so a retry can recover one committed identity but
-- can never manufacture a seed or placeholder revision.
CREATE TABLE authoring_task_materializations_v2 (
    id                 TEXT PRIMARY KEY,
    session_id         TEXT NOT NULL UNIQUE REFERENCES authoring_sessions_v2(id) ON DELETE RESTRICT,
    source_id          TEXT NOT NULL REFERENCES authoring_sources_v2(id) ON DELETE RESTRICT,
    authoring_run_id   TEXT NOT NULL UNIQUE REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    task_id            TEXT NOT NULL UNIQUE REFERENCES tasks_v2(id) ON DELETE RESTRICT,
    revision_id        TEXT NOT NULL UNIQUE REFERENCES task_revisions(id) ON DELETE RESTRICT,
    source_fingerprint TEXT NOT NULL,
    task_digest        TEXT NOT NULL,
    request_fingerprint TEXT NOT NULL,
    idempotency_key    TEXT NOT NULL UNIQUE,
    created_by         TEXT NOT NULL,
    created_at         DATETIME NOT NULL,
    CHECK (length(source_fingerprint) = 71),
    CHECK (substr(source_fingerprint, 1, 7) = 'sha256:'),
    CHECK (substr(source_fingerprint, 8) NOT GLOB '*[^0-9a-f]*'),
    CHECK (length(request_fingerprint) = 71),
    CHECK (substr(request_fingerprint, 1, 7) = 'sha256:'),
    CHECK (substr(request_fingerprint, 8) NOT GLOB '*[^0-9a-f]*')
);

-- table authoring_phase1_handoffs_v2
-- This is the second, separately durable bridge: a persisted
-- authoring_task_handoff artifact may prepare exactly one task-bound
-- CodeEdge Phase-1 child Run. It reserves the child identity before Run
-- creation, while the application layer verifies the artifact's strict JSON
-- content and object bytes before inserting this row.
CREATE TABLE authoring_phase1_handoffs_v2 (
    id                    TEXT PRIMARY KEY,
    authoring_run_id      TEXT NOT NULL UNIQUE REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    authoring_session_id  TEXT NOT NULL REFERENCES authoring_sessions_v2(id) ON DELETE RESTRICT,
    authoring_source_id   TEXT NOT NULL REFERENCES authoring_sources_v2(id) ON DELETE RESTRICT,
    handoff_artifact_id   TEXT NOT NULL UNIQUE REFERENCES artifact_refs_v4(id) ON DELETE RESTRICT,
    handoff_fingerprint   TEXT NOT NULL UNIQUE,
    task_id               TEXT NOT NULL REFERENCES tasks_v2(id) ON DELETE RESTRICT,
    revision_id           TEXT NOT NULL REFERENCES task_revisions(id) ON DELETE RESTRICT,
    task_digest           TEXT NOT NULL,
    child_run_id          TEXT NOT NULL UNIQUE,
    idempotency_key       TEXT NOT NULL UNIQUE,
    created_by            TEXT NOT NULL,
    created_at            DATETIME NOT NULL,
    CHECK (length(handoff_fingerprint) = 71),
    CHECK (substr(handoff_fingerprint, 1, 7) = 'sha256:'),
    CHECK (substr(handoff_fingerprint, 8) NOT GLOB '*[^0-9a-f]*'),
    CHECK (length(task_digest) = 86),
    CHECK (substr(task_digest, 1, 22) = 'harbor.task.v2:sha256:'),
    CHECK (substr(task_digest, 23) NOT GLOB '*[^0-9a-f]*')
);

-- table tasks_v2
CREATE TABLE "tasks_v2" (
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

-- table trial_attempts_v19
CREATE TABLE trial_attempts_v19 (
    id                        TEXT PRIMARY KEY,
    trial_execution_id        TEXT NOT NULL REFERENCES trial_executions_v19(id) ON DELETE RESTRICT,
    retry_of_trial_attempt_id TEXT REFERENCES trial_attempts_v19(id) ON DELETE RESTRICT,
    ordinal                   INTEGER NOT NULL CHECK (ordinal > 0),
    status                    TEXT NOT NULL DEFAULT 'queued'
                              CHECK (status IN ('queued', 'running', 'waiting', 'completed', 'infra_failed', 'interrupted', 'in_doubt', 'reconciling', 'canceled')),
    error_text                TEXT NOT NULL DEFAULT '',
    failure_class             TEXT NOT NULL DEFAULT '',
    created_at                DATETIME NOT NULL,
    updated_at                DATETIME NOT NULL,
    started_at                DATETIME,
    finished_at               DATETIME,
    version                   INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    CHECK (retry_of_trial_attempt_id IS NULL OR retry_of_trial_attempt_id <> ''),
    UNIQUE(trial_execution_id, ordinal)
);

-- table trial_executions_v19
CREATE TABLE trial_executions_v19 (
    id               TEXT PRIMARY KEY,
    run_id           TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    stage_attempt_id TEXT NOT NULL REFERENCES stage_attempts(id) ON DELETE RESTRICT,
    stage_key        TEXT NOT NULL CHECK (length(trim(stage_key)) > 0),
    ordinal          INTEGER NOT NULL CHECK (ordinal > 0),
    status           TEXT NOT NULL DEFAULT 'queued'
                     CHECK (status IN ('queued', 'running', 'waiting', 'completed', 'infra_failed', 'interrupted', 'in_doubt', 'reconciling', 'canceled')),
    created_at       DATETIME NOT NULL,
    updated_at       DATETIME NOT NULL,
    started_at       DATETIME,
    finished_at      DATETIME,
    version          INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    UNIQUE(run_id, stage_attempt_id, stage_key, ordinal)
);

-- table turn_checkpoints
CREATE TABLE turn_checkpoints (
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
    version          INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    UNIQUE(node_attempt_id, turn, substep)
);

-- table usage_events
CREATE TABLE usage_events (
    id            TEXT PRIMARY KEY,
    lease_id      TEXT NOT NULL REFERENCES quota_leases(id) ON DELETE RESTRICT,
    operation_key TEXT NOT NULL,
    units         INTEGER NOT NULL,
    created_at    DATETIME NOT NULL,
    UNIQUE(lease_id, operation_key)
);

-- table workflow_runs
CREATE TABLE workflow_runs (
    id                        TEXT PRIMARY KEY,
    subject_kind              TEXT NOT NULL DEFAULT 'task_revision'
                              CHECK (subject_kind IN ('task_revision', 'authoring_session')),
    subject_id                TEXT NOT NULL,
    subject_revision_id       TEXT NOT NULL,
    subject_digest            TEXT NOT NULL,
    task_id                   TEXT REFERENCES tasks_v2(id) ON DELETE RESTRICT,
    revision_id               TEXT REFERENCES task_revisions(id) ON DELETE RESTRICT,
    authoring_session_id      TEXT REFERENCES authoring_sessions_v2(id) ON DELETE RESTRICT,
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
    version                   INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    CHECK (
        (subject_kind = 'task_revision'
         AND task_id IS NOT NULL
         AND revision_id IS NOT NULL
         AND authoring_session_id IS NULL)
        OR
        (subject_kind = 'authoring_session'
         AND task_id IS NULL
         AND revision_id IS NULL
         AND authoring_session_id IS NOT NULL)
    )
);

-- table workspaces_v2
CREATE TABLE workspaces_v2 (
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

-- index idx_admission_decisions_v5_task
CREATE INDEX idx_admission_decisions_v5_task
    ON admission_decisions_v5(task_id, decided_at DESC);

-- index idx_artifact_manifests_v4_fingerprint
CREATE INDEX idx_artifact_manifests_v4_fingerprint
    ON artifact_manifests_v4(manifest_fingerprint);

-- index idx_artifact_manifests_v4_subject
CREATE INDEX idx_artifact_manifests_v4_subject
    ON artifact_manifests_v4(subject_revision_id, created_at DESC);

-- index idx_artifact_refs_v4_lineage
CREATE INDEX idx_artifact_refs_v4_lineage
    ON artifact_refs_v4(subject_revision_id, workflow_fingerprint, stage_key, created_at DESC);

-- index idx_artifact_refs_v4_manifest
CREATE INDEX idx_artifact_refs_v4_manifest ON artifact_refs_v4(manifest_id, artifact_key);

-- index idx_authoring_sessions_v2_source
CREATE INDEX idx_authoring_sessions_v2_source
    ON authoring_sessions_v2(source_id, created_at DESC);

-- index idx_authoring_run_input_artifacts_v2_run
CREATE INDEX idx_authoring_run_input_artifacts_v2_run
    ON authoring_run_input_artifacts_v2(run_id, port);

-- index idx_authoring_review_requests_v22_run
CREATE INDEX idx_authoring_review_requests_v22_run
    ON authoring_review_requests_v22(run_id, created_at DESC);

-- index idx_authoring_review_gate_bindings_v22_stage
CREATE INDEX idx_authoring_review_gate_bindings_v22_stage
    ON authoring_review_gate_bindings_v22(run_id, stage_attempt_id);

-- index idx_authoring_review_gate_bindings_v22_session
CREATE INDEX idx_authoring_review_gate_bindings_v22_session
    ON authoring_review_gate_bindings_v22(authoring_session_id, created_at DESC);

-- index idx_authoring_review_decisions_v22_request
CREATE INDEX idx_authoring_review_decisions_v22_request
    ON authoring_review_decisions_v22(review_request_id, created_at ASC, id ASC);

-- index idx_authoring_review_gate_resolutions_v22_request
CREATE INDEX idx_authoring_review_gate_resolutions_v22_request
    ON authoring_review_gate_resolutions_v22(review_request_id, created_at ASC, id ASC);

-- index idx_authoring_task_materializations_v2_revision
CREATE INDEX idx_authoring_task_materializations_v2_revision
    ON authoring_task_materializations_v2(revision_id);

-- index idx_authoring_task_materializations_v2_run
CREATE INDEX idx_authoring_task_materializations_v2_run
    ON authoring_task_materializations_v2(authoring_run_id);

-- index idx_authoring_phase1_handoffs_v2_task
CREATE INDEX idx_authoring_phase1_handoffs_v2_task
    ON authoring_phase1_handoffs_v2(task_id, revision_id, created_at DESC, id);

-- index idx_run_input_artifacts_run_port
CREATE INDEX idx_run_input_artifacts_run_port
    ON run_input_artifacts(run_id, port);

-- index idx_audit_events_actor
CREATE INDEX idx_audit_events_actor ON audit_events(actor, created_at DESC);

-- index idx_audit_events_entity
CREATE INDEX idx_audit_events_entity ON audit_events(entity_type, entity_id, created_at DESC);

-- index idx_candidate_gc_operations_v10_active_candidate
CREATE UNIQUE INDEX idx_candidate_gc_operations_v10_active_candidate
    ON candidate_gc_operations_v10(candidate_id) WHERE state = 'in_progress';

-- index idx_candidate_gc_operations_v10_candidate
CREATE INDEX idx_candidate_gc_operations_v10_candidate
    ON candidate_gc_operations_v10(candidate_id, created_at DESC);

-- index idx_change_operations_v8_candidate
CREATE INDEX idx_change_operations_v8_candidate
    ON change_operations_v8(candidate_id, created_at DESC);

-- index idx_change_operations_v8_state
CREATE INDEX idx_change_operations_v8_state
    ON change_operations_v8(state, updated_at);

-- index idx_codeedge_compliance_v20_revision
CREATE INDEX idx_codeedge_compliance_v20_revision
    ON codeedge_compliance_records_v20(revision_id, created_at, id);

-- index idx_codeedge_compliance_v20_task
CREATE INDEX idx_codeedge_compliance_v20_task
    ON codeedge_compliance_records_v20(task_id, created_at, id);

-- index idx_codeedge_evaluator_handoff_v2_task
CREATE INDEX idx_codeedge_evaluator_handoff_v2_task
    ON codeedge_evaluator_evidence_handoffs_v2(task_id, revision_id, created_at, id);

-- index idx_continuation_commands_v4_subject
CREATE INDEX idx_continuation_commands_v4_subject
    ON continuation_commands_v4(subject_id, created_at DESC);

-- index idx_continuation_executions_v4_plan
CREATE INDEX idx_continuation_executions_v4_plan ON continuation_executions_v4(plan_id, created_at);

-- index idx_continuation_executions_v4_state
CREATE INDEX idx_continuation_executions_v4_state ON continuation_executions_v4(state, updated_at);

-- index idx_control_operations_v5_run_status
CREATE INDEX idx_control_operations_v5_run_status
    ON control_operations_v5(run_id, status, updated_at);

-- index idx_control_transitions_v5_operation
CREATE INDEX idx_control_transitions_v5_operation
    ON control_operation_transitions_v5(operation_id, created_at);

-- index idx_deletion_records_entity
CREATE INDEX idx_deletion_records_entity ON deletion_records(entity_type, entity_id, created_at DESC);

-- index idx_frozen_plans_v4_prepared_change
CREATE INDEX idx_frozen_plans_v4_prepared_change ON frozen_plans_v4(prepared_change_id);

-- index idx_frozen_plans_v4_subject
CREATE INDEX idx_frozen_plans_v4_subject ON frozen_plans_v4(subject_id, created_at DESC);

-- index idx_job_dispatch_claims_v5_active_job
CREATE UNIQUE INDEX idx_job_dispatch_claims_v5_active_job
    ON job_dispatch_claims_v5(job_id) WHERE state = 'active';

-- index idx_job_dispatch_claims_v5_run_state
CREATE INDEX idx_job_dispatch_claims_v5_run_state
    ON job_dispatch_claims_v5(run_id, state, claimed_at);

-- index idx_job_dispatch_claims_v5_state
CREATE INDEX idx_job_dispatch_claims_v5_state
    ON job_dispatch_claims_v5(state, updated_at);

-- index idx_jobs_entity
CREATE INDEX idx_jobs_entity ON jobs(entity_type, entity_id);

-- index idx_jobs_state_priority
CREATE INDEX idx_jobs_state_priority ON jobs(state, priority DESC, created_at);

-- index idx_leases_active_resource
CREATE UNIQUE INDEX idx_leases_active_resource
    ON leases(resource_type, resource_id) WHERE state = 'active';

-- index idx_leases_expiry
CREATE INDEX idx_leases_expiry ON leases(state, expires_at);

-- index idx_lifecycle_operations_v12_action
CREATE INDEX idx_lifecycle_operations_v12_action
    ON lifecycle_operations_v12(action, created_at DESC, id);

-- index idx_lifecycle_operations_v12_target_task
CREATE INDEX idx_lifecycle_operations_v12_target_task
    ON lifecycle_operations_v12(task_id, created_at DESC, id);

-- index idx_mutation_receipts_v4_change
CREATE INDEX idx_mutation_receipts_v4_change ON mutation_receipts_v4(prepared_change_id, created_at);

-- index idx_node_attempts_stage
CREATE INDEX idx_node_attempts_stage ON node_attempts(stage_attempt_id);

-- index idx_outbox_delivery_operations_v9_created
CREATE INDEX idx_outbox_delivery_operations_v9_created
    ON outbox_delivery_operations_v9(created_at, id);

-- index idx_outbox_events_dispatchable_v9
CREATE INDEX idx_outbox_events_dispatchable_v9
    ON outbox_events(state, available_at, created_at, id);

-- index idx_outbox_events_idempotency
CREATE UNIQUE INDEX idx_outbox_events_idempotency
    ON outbox_events(topic, entity_type, entity_id, idempotency_key)
    WHERE idempotency_key <> '';

-- index idx_outbox_events_lease_expiry_v9
CREATE INDEX idx_outbox_events_lease_expiry_v9
    ON outbox_events(state, lease_expires_at, id);

-- index idx_outbox_events_state
CREATE INDEX idx_outbox_events_state ON outbox_events(state, created_at);

-- index idx_prepared_changes_v4_command
CREATE INDEX idx_prepared_changes_v4_command ON prepared_changes_v4(command_id, created_at);

-- index idx_quota_account_policy_bindings_v11_policy
CREATE INDEX idx_quota_account_policy_bindings_v11_policy
    ON quota_account_policy_bindings_v11(policy_id, policy_version, account_id);

-- index idx_quota_accounts_v5_scope
CREATE INDEX idx_quota_accounts_v5_scope
    ON quota_accounts_v5(scope_kind, scope_id, dimension);

-- index idx_quota_leases_v5_account_state
CREATE INDEX idx_quota_leases_v5_account_state
    ON quota_leases_v5(account_id, state, expires_at);

-- index idx_quota_leases_v5_admission
CREATE INDEX idx_quota_leases_v5_admission
    ON quota_leases_v5(admission_id, created_at);

-- index idx_quota_settlements_v5_lease
CREATE INDEX idx_quota_settlements_v5_lease
    ON quota_settlements_v5(lease_id, settled_at);

-- index idx_quota_usage_events_v5_lease
CREATE INDEX idx_quota_usage_events_v5_lease
    ON quota_usage_events_v5(lease_id, occurred_at);

-- index idx_reconciliation_attempts_v5_subject
CREATE INDEX idx_reconciliation_attempts_v5_subject
    ON reconciliation_attempts_v5(subject_type, subject_id, created_at DESC);

-- index idx_release_withdraw_operations_v10_release
CREATE INDEX idx_release_withdraw_operations_v10_release
    ON release_withdraw_operations_v10(release_id, created_at DESC);

-- index idx_release_withdraw_receipts_v10_release
CREATE INDEX idx_release_withdraw_receipts_v10_release
    ON release_withdraw_receipts_v10(release_id, created_at DESC);

-- index idx_repair_sessions_v4_command
CREATE INDEX idx_repair_sessions_v4_command ON repair_sessions_v4(command_id);

-- index idx_repair_sessions_v4_subject
CREATE INDEX idx_repair_sessions_v4_subject ON repair_sessions_v4(subject_id, created_at DESC);

-- index idx_review_decisions_revision
CREATE INDEX idx_review_decisions_revision ON review_decisions(revision_id, created_at DESC);

-- index idx_review_gate_bindings_v15_revision
CREATE INDEX idx_review_gate_bindings_v15_revision
    ON review_gate_bindings_v15(revision_id, created_at DESC);

-- index idx_review_gate_bindings_v15_run
CREATE INDEX idx_review_gate_bindings_v15_run
    ON review_gate_bindings_v15(run_id, stage_attempt_id);

-- index idx_review_requests_revision
CREATE INDEX idx_review_requests_revision ON review_requests(revision_id, created_at DESC);

-- index idx_revision_candidates_v10_gc
CREATE INDEX idx_revision_candidates_v10_gc
    ON revision_candidates_v8(state, retain_until, checkout_tombstoned_at, id);

-- index idx_revision_candidates_v8_active_task
CREATE UNIQUE INDEX idx_revision_candidates_v8_active_task
    ON revision_candidates_v8(task_id)
    WHERE state IN ('ready', 'applying', 'prepared', 'reconcile_required', 'committing');

-- index idx_revision_candidates_v8_command
CREATE INDEX idx_revision_candidates_v8_command
    ON revision_candidates_v8(command_id);

-- index idx_revision_candidates_v8_repair
CREATE INDEX idx_revision_candidates_v8_repair
    ON revision_candidates_v8(repair_session_id, round_ordinal);

-- index idx_revision_candidates_v8_repair_round
CREATE UNIQUE INDEX idx_revision_candidates_v8_repair_round
    ON revision_candidates_v8(repair_session_id, round_ordinal)
    WHERE repair_session_id IS NOT NULL;

-- index idx_revision_candidates_v8_source_run
CREATE INDEX idx_revision_candidates_v8_source_run
    ON revision_candidates_v8(source_run_id, created_at DESC);

-- index idx_revision_candidates_v8_state
CREATE INDEX idx_revision_candidates_v8_state
    ON revision_candidates_v8(state, updated_at);

-- index idx_revision_candidates_v8_task
CREATE INDEX idx_revision_candidates_v8_task
    ON revision_candidates_v8(task_id, created_at DESC);

-- index idx_run_attempts_run
CREATE INDEX idx_run_attempts_run ON run_attempts(run_id, ordinal);

-- index idx_run_worker_handoffs_v16_active_run
CREATE UNIQUE INDEX idx_run_worker_handoffs_v16_active_run
    ON run_worker_handoffs_v16(run_id)
    WHERE state IN ('launching', 'handed_off');

-- index idx_run_worker_handoffs_v16_run
CREATE INDEX idx_run_worker_handoffs_v16_run
    ON run_worker_handoffs_v16(run_id, created_at DESC, id);

-- index idx_run_worker_handoffs_v16_worker_lease
CREATE INDEX idx_run_worker_handoffs_v16_worker_lease
    ON run_worker_handoffs_v16(worker_lease_id);

-- index idx_runtime_receipts_v5_operation
CREATE INDEX idx_runtime_receipts_v5_operation
    ON runtime_termination_receipts_v5(control_operation_id, observed_at, id);

-- index idx_side_effect_operations_v5_state
CREATE INDEX idx_side_effect_operations_v5_state
    ON side_effect_operations_v5(state, updated_at);

-- index idx_stage_attempts_run
CREATE INDEX idx_stage_attempts_run ON stage_attempts(run_id, ordinal);

-- index idx_stage_attempts_status
CREATE INDEX idx_stage_attempts_status ON stage_attempts(execution_status);

-- index idx_task_purge_operations_v7_in_progress_task
CREATE UNIQUE INDEX idx_task_purge_operations_v7_in_progress_task
    ON task_purge_operations_v7(task_id) WHERE state = 'in_progress';

-- index idx_task_purge_operations_v7_task
CREATE INDEX idx_task_purge_operations_v7_task
    ON task_purge_operations_v7(task_id, created_at DESC);

-- index idx_task_revisions_digest
CREATE INDEX idx_task_revisions_digest ON task_revisions(task_digest);

-- index idx_task_revisions_task
CREATE INDEX idx_task_revisions_task ON task_revisions(task_id, version_number DESC);

-- index idx_tasks_v2_current_revision
CREATE INDEX idx_tasks_v2_current_revision ON tasks_v2(current_revision_id);

-- index idx_tasks_v2_slug
CREATE INDEX idx_tasks_v2_slug ON tasks_v2(slug);

-- index idx_trial_attempts_v19_active_execution
CREATE UNIQUE INDEX idx_trial_attempts_v19_active_execution
    ON trial_attempts_v19(trial_execution_id)
    WHERE status IN ('queued', 'running', 'waiting', 'in_doubt', 'reconciling');

-- index idx_trial_attempts_v19_execution
CREATE INDEX idx_trial_attempts_v19_execution
    ON trial_attempts_v19(trial_execution_id, ordinal, id);

-- index idx_trial_attempts_v19_retry_successor
CREATE UNIQUE INDEX idx_trial_attempts_v19_retry_successor
    ON trial_attempts_v19(retry_of_trial_attempt_id)
    WHERE retry_of_trial_attempt_id IS NOT NULL;

-- index idx_trial_attempts_v19_status
CREATE INDEX idx_trial_attempts_v19_status
    ON trial_attempts_v19(status, updated_at, id);

-- index idx_trial_executions_v19_run
CREATE INDEX idx_trial_executions_v19_run
    ON trial_executions_v19(run_id, stage_attempt_id, ordinal, id);

-- index idx_trial_executions_v19_stage
CREATE INDEX idx_trial_executions_v19_stage
    ON trial_executions_v19(stage_attempt_id, ordinal, id);

-- index idx_trial_executions_v19_status
CREATE INDEX idx_trial_executions_v19_status
    ON trial_executions_v19(status, updated_at, id);

-- index idx_turn_checkpoints_node
CREATE INDEX idx_turn_checkpoints_node ON turn_checkpoints(node_attempt_id, turn);

-- index idx_workflow_runs_revision
CREATE INDEX idx_workflow_runs_revision ON workflow_runs(revision_id, created_at DESC);

-- index idx_workflow_runs_authoring_session
CREATE UNIQUE INDEX idx_workflow_runs_authoring_session
    ON workflow_runs(authoring_session_id)
    WHERE authoring_session_id IS NOT NULL;

-- index idx_workflow_runs_status
CREATE INDEX idx_workflow_runs_status ON workflow_runs(status);

-- index idx_workflow_runs_task
CREATE INDEX idx_workflow_runs_task ON workflow_runs(task_id, created_at DESC);

-- index idx_workspaces_v2_state
CREATE INDEX idx_workspaces_v2_state ON workspaces_v2(state, updated_at);

-- index idx_workspaces_v2_task
CREATE INDEX idx_workspaces_v2_task ON workspaces_v2(task_id);

-- trigger artifact_manifests_v4_no_delete
CREATE TRIGGER artifact_manifests_v4_no_delete
BEFORE DELETE ON artifact_manifests_v4
BEGIN
    SELECT RAISE(ABORT, 'artifact manifests are immutable');
END;

-- trigger entity_id_registry_uuidv7_insert
-- Every lifecycle table enters the global identity namespace through an
-- entity_id_registry insert trigger. Keep the UUIDv7 validation at that
-- single SQLite boundary so raw SQL cannot introduce an empty, NULL, mixed
-- case, non-hex, wrong-version, or wrong-variant lifecycle identity.
CREATE TRIGGER entity_id_registry_uuidv7_insert
BEFORE INSERT ON entity_id_registry
WHEN NEW.id IS NULL
  OR length(NEW.id) <> 36
  OR NEW.id <> trim(NEW.id)
  OR NEW.id <> lower(NEW.id)
  OR substr(NEW.id, 9, 1) <> '-'
  OR substr(NEW.id, 14, 1) <> '-'
  OR substr(NEW.id, 15, 1) <> '7'
  OR substr(NEW.id, 19, 1) <> '-'
  OR substr(NEW.id, 20, 1) NOT IN ('8', '9', 'a', 'b')
  OR substr(NEW.id, 24, 1) <> '-'
  OR length(replace(NEW.id, '-', '')) <> 32
  OR replace(NEW.id, '-', '') GLOB '*[^0-9a-f]*'
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity must be canonical UUIDv7');
END;

-- trigger entity_id_registry_no_update
-- Registry entries are durable tombstones for real lifecycle identities. An
-- entity must never be relabelled after it has occupied the global namespace.
CREATE TRIGGER entity_id_registry_no_update
BEFORE UPDATE ON entity_id_registry
BEGIN
    SELECT RAISE(ABORT, 'lifecycle identity registry entries are immutable');
END;

-- trigger entity_id_registry_no_delete
-- A source entity can be deleted by a maintenance operation, but its UUIDv7
-- must remain permanently occupied. Candidate target identities are the lone
-- exception: they are provisional reservations that are atomically consumed
-- immediately before the target task_revision/workflow_run insert.
CREATE TRIGGER entity_id_registry_no_delete
BEFORE DELETE ON entity_id_registry
WHEN OLD.entity_type NOT IN ('reserved_task_revision', 'reserved_workflow_run')
BEGIN
    SELECT RAISE(ABORT, 'lifecycle identity registry entries are permanent');
END;

-- trigger entity_id_registry_reserved_delete_requires_reservation
-- Preserve the candidate promotion path while rejecting a forged or orphaned
-- reserved registry deletion. consumeCandidateIdentityReservationTx deletes
-- this row first, then deletes the matching reservation, in one transaction.
CREATE TRIGGER entity_id_registry_reserved_delete_requires_reservation
BEFORE DELETE ON entity_id_registry
WHEN (OLD.entity_type = 'reserved_task_revision' AND NOT EXISTS (
        SELECT 1 FROM revision_candidate_identity_reservations_v8
        WHERE reserved_id = OLD.id AND intended_type = 'task_revision'
    ))
  OR (OLD.entity_type = 'reserved_workflow_run' AND NOT EXISTS (
        SELECT 1 FROM revision_candidate_identity_reservations_v8
        WHERE reserved_id = OLD.id AND intended_type = 'workflow_run'
    ))
BEGIN
    SELECT RAISE(ABORT, 'candidate identity reservation is required for registry promotion');
END;

-- trigger artifact_manifests_v4_no_update
CREATE TRIGGER artifact_manifests_v4_no_update
BEFORE UPDATE ON artifact_manifests_v4
BEGIN
    SELECT RAISE(ABORT, 'artifact manifests are immutable');
END;

-- trigger artifact_refs_v4_no_delete
CREATE TRIGGER artifact_refs_v4_no_delete
BEFORE DELETE ON artifact_refs_v4
BEGIN
    SELECT RAISE(ABORT, 'artifact refs are immutable');
END;

-- trigger artifact_refs_v4_no_update
CREATE TRIGGER artifact_refs_v4_no_update
BEFORE UPDATE ON artifact_refs_v4
BEGIN
    SELECT RAISE(ABORT, 'artifact refs are immutable');
END;

-- trigger run_input_artifacts_no_delete
CREATE TRIGGER run_input_artifacts_no_delete
BEFORE DELETE ON run_input_artifacts
BEGIN
    SELECT RAISE(ABORT, 'run input artifacts are immutable');
END;

-- trigger run_input_artifacts_no_update
CREATE TRIGGER run_input_artifacts_no_update
BEFORE UPDATE ON run_input_artifacts
BEGIN
    SELECT RAISE(ABORT, 'run input artifacts are immutable');
END;

-- trigger audit_events_no_delete
CREATE TRIGGER audit_events_no_delete
BEFORE DELETE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit events are append-only');
END;

-- trigger audit_events_no_update
CREATE TRIGGER audit_events_no_update
BEFORE UPDATE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit events are append-only');
END;

-- trigger codeedge_compliance_records_v20_immutable
CREATE TRIGGER codeedge_compliance_records_v20_immutable
BEFORE UPDATE ON codeedge_compliance_records_v20
BEGIN
    SELECT RAISE(ABORT, 'CodeEdge compliance record is immutable');
END;

-- trigger codeedge_compliance_records_v20_no_delete
CREATE TRIGGER codeedge_compliance_records_v20_no_delete
BEFORE DELETE ON codeedge_compliance_records_v20
BEGIN
    SELECT RAISE(ABORT, 'CodeEdge compliance records are append-only');
END;

-- trigger codeedge_evaluator_evidence_handoffs_v2_immutable
CREATE TRIGGER codeedge_evaluator_evidence_handoffs_v2_immutable
BEFORE UPDATE ON codeedge_evaluator_evidence_handoffs_v2
BEGIN
    SELECT RAISE(ABORT, 'CodeEdge evaluator evidence handoff is immutable');
END;

-- trigger codeedge_evaluator_evidence_handoffs_v2_no_delete
CREATE TRIGGER codeedge_evaluator_evidence_handoffs_v2_no_delete
BEFORE DELETE ON codeedge_evaluator_evidence_handoffs_v2
BEGIN
    SELECT RAISE(ABORT, 'CodeEdge evaluator evidence handoffs are append-only');
END;

-- trigger continuation_commands_v4_no_delete
CREATE TRIGGER continuation_commands_v4_no_delete
BEFORE DELETE ON continuation_commands_v4
BEGIN
    SELECT RAISE(ABORT, 'continuation commands are immutable');
END;

-- trigger continuation_commands_v4_no_update
CREATE TRIGGER continuation_commands_v4_no_update
BEFORE UPDATE ON continuation_commands_v4
BEGIN
    SELECT RAISE(ABORT, 'continuation commands are immutable');
END;

-- trigger continuation_executions_v4_content_immutable
CREATE TRIGGER continuation_executions_v4_content_immutable
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

-- trigger entity_id_registry_admission_decisions_v5_id_immutable
CREATE TRIGGER entity_id_registry_admission_decisions_v5_id_immutable
BEFORE UPDATE OF id ON admission_decisions_v5
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_admission_decisions_v5_insert
CREATE TRIGGER entity_id_registry_admission_decisions_v5_insert
BEFORE INSERT ON admission_decisions_v5
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'admission_decision_v5');
END;

-- trigger entity_id_registry_artifact_manifests_v4_id_immutable
CREATE TRIGGER entity_id_registry_artifact_manifests_v4_id_immutable
BEFORE UPDATE OF id ON artifact_manifests_v4
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_artifact_manifests_v4_insert
CREATE TRIGGER entity_id_registry_artifact_manifests_v4_insert
BEFORE INSERT ON artifact_manifests_v4
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'artifact_manifest');
END;

-- trigger entity_id_registry_artifact_refs_v4_id_immutable
CREATE TRIGGER entity_id_registry_artifact_refs_v4_id_immutable
BEFORE UPDATE OF id ON artifact_refs_v4
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_artifact_refs_v4_insert
CREATE TRIGGER entity_id_registry_artifact_refs_v4_insert
BEFORE INSERT ON artifact_refs_v4
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'artifact_ref');
END;

-- trigger entity_id_registry_authoring_sources_v2_id_immutable
CREATE TRIGGER entity_id_registry_authoring_sources_v2_id_immutable
BEFORE UPDATE OF id ON authoring_sources_v2
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_authoring_sources_v2_insert
CREATE TRIGGER entity_id_registry_authoring_sources_v2_insert
BEFORE INSERT ON authoring_sources_v2
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'authoring_source');
END;

-- trigger entity_id_registry_authoring_sessions_v2_id_immutable
CREATE TRIGGER entity_id_registry_authoring_sessions_v2_id_immutable
BEFORE UPDATE OF id ON authoring_sessions_v2
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_authoring_sessions_v2_insert
CREATE TRIGGER entity_id_registry_authoring_sessions_v2_insert
BEFORE INSERT ON authoring_sessions_v2
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'authoring_session');
END;

-- trigger entity_id_registry_authoring_task_materializations_v2_id_immutable
CREATE TRIGGER entity_id_registry_authoring_task_materializations_v2_id_immutable
BEFORE UPDATE OF id ON authoring_task_materializations_v2
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_authoring_task_materializations_v2_insert
CREATE TRIGGER entity_id_registry_authoring_task_materializations_v2_insert
BEFORE INSERT ON authoring_task_materializations_v2
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'authoring_task_materialization');
END;

-- trigger entity_id_registry_authoring_phase1_handoffs_v2_id_immutable
CREATE TRIGGER entity_id_registry_authoring_phase1_handoffs_v2_id_immutable
BEFORE UPDATE OF id ON authoring_phase1_handoffs_v2
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_authoring_phase1_handoffs_v2_insert
CREATE TRIGGER entity_id_registry_authoring_phase1_handoffs_v2_insert
BEFORE INSERT ON authoring_phase1_handoffs_v2
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'authoring_phase1_handoff');
END;

-- trigger entity_id_registry_authoring_run_input_artifacts_v2_id_immutable
CREATE TRIGGER entity_id_registry_authoring_run_input_artifacts_v2_id_immutable
BEFORE UPDATE OF id ON authoring_run_input_artifacts_v2
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_authoring_run_input_artifacts_v2_insert
CREATE TRIGGER entity_id_registry_authoring_run_input_artifacts_v2_insert
BEFORE INSERT ON authoring_run_input_artifacts_v2
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'authoring_run_input_artifact');
END;

-- trigger entity_id_registry_authoring_review_requests_v22_id_immutable
CREATE TRIGGER entity_id_registry_authoring_review_requests_v22_id_immutable
BEFORE UPDATE OF id ON authoring_review_requests_v22
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_authoring_review_requests_v22_insert
CREATE TRIGGER entity_id_registry_authoring_review_requests_v22_insert
BEFORE INSERT ON authoring_review_requests_v22
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'authoring_review_request');
END;

-- trigger entity_id_registry_authoring_review_gate_bindings_v22_id_immutable
CREATE TRIGGER entity_id_registry_authoring_review_gate_bindings_v22_id_immutable
BEFORE UPDATE OF id ON authoring_review_gate_bindings_v22
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_authoring_review_gate_bindings_v22_insert
CREATE TRIGGER entity_id_registry_authoring_review_gate_bindings_v22_insert
BEFORE INSERT ON authoring_review_gate_bindings_v22
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'authoring_review_gate_binding');
END;

-- trigger entity_id_registry_authoring_review_decisions_v22_id_immutable
CREATE TRIGGER entity_id_registry_authoring_review_decisions_v22_id_immutable
BEFORE UPDATE OF id ON authoring_review_decisions_v22
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_authoring_review_decisions_v22_insert
CREATE TRIGGER entity_id_registry_authoring_review_decisions_v22_insert
BEFORE INSERT ON authoring_review_decisions_v22
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'authoring_review_decision');
END;

-- trigger entity_id_registry_authoring_review_gate_resolutions_v22_id_immutable
CREATE TRIGGER entity_id_registry_authoring_review_gate_resolutions_v22_id_immutable
BEFORE UPDATE OF id ON authoring_review_gate_resolutions_v22
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_authoring_review_gate_resolutions_v22_insert
CREATE TRIGGER entity_id_registry_authoring_review_gate_resolutions_v22_insert
BEFORE INSERT ON authoring_review_gate_resolutions_v22
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'authoring_review_gate_resolution');
END;

-- trigger entity_id_registry_run_input_artifacts_id_immutable
CREATE TRIGGER entity_id_registry_run_input_artifacts_id_immutable
BEFORE UPDATE OF id ON run_input_artifacts
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_run_input_artifacts_insert
CREATE TRIGGER entity_id_registry_run_input_artifacts_insert
BEFORE INSERT ON run_input_artifacts
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'run_input_artifact');
END;

-- trigger entity_id_registry_audit_events_id_immutable
CREATE TRIGGER entity_id_registry_audit_events_id_immutable
BEFORE UPDATE OF id ON audit_events
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_audit_events_insert
CREATE TRIGGER entity_id_registry_audit_events_insert
BEFORE INSERT ON audit_events
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'audit_event');
END;

-- trigger entity_id_registry_budget_grants_id_immutable
CREATE TRIGGER entity_id_registry_budget_grants_id_immutable
BEFORE UPDATE OF id ON budget_grants
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_budget_grants_insert
CREATE TRIGGER entity_id_registry_budget_grants_insert
BEFORE INSERT ON budget_grants
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'budget_grant');
END;

-- trigger entity_id_registry_budget_settlements_id_immutable
CREATE TRIGGER entity_id_registry_budget_settlements_id_immutable
BEFORE UPDATE OF id ON budget_settlements
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_budget_settlements_insert
CREATE TRIGGER entity_id_registry_budget_settlements_insert
BEFORE INSERT ON budget_settlements
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'budget_settlement');
END;

-- trigger entity_id_registry_candidate_gc_operations_v10_id_immutable
CREATE TRIGGER entity_id_registry_candidate_gc_operations_v10_id_immutable
BEFORE UPDATE OF id ON candidate_gc_operations_v10
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_candidate_gc_operations_v10_insert
CREATE TRIGGER entity_id_registry_candidate_gc_operations_v10_insert
BEFORE INSERT ON candidate_gc_operations_v10
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'candidate_gc_operation');
END;

-- trigger entity_id_registry_capacity_pools_v5_id_immutable
CREATE TRIGGER entity_id_registry_capacity_pools_v5_id_immutable
BEFORE UPDATE OF id ON capacity_pools_v5
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_capacity_pools_v5_insert
CREATE TRIGGER entity_id_registry_capacity_pools_v5_insert
BEFORE INSERT ON capacity_pools_v5
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'capacity_pool');
END;

-- trigger entity_id_registry_change_operations_v8_id_immutable
CREATE TRIGGER entity_id_registry_change_operations_v8_id_immutable
BEFORE UPDATE OF id ON change_operations_v8
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_change_operations_v8_insert
CREATE TRIGGER entity_id_registry_change_operations_v8_insert
BEFORE INSERT ON change_operations_v8
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'change_operation');
END;

-- trigger entity_id_registry_codeedge_compliance_records_v20_id_immutable
CREATE TRIGGER entity_id_registry_codeedge_compliance_records_v20_id_immutable
BEFORE UPDATE OF id ON codeedge_compliance_records_v20
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_codeedge_compliance_records_v20_insert
CREATE TRIGGER entity_id_registry_codeedge_compliance_records_v20_insert
BEFORE INSERT ON codeedge_compliance_records_v20
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'codeedge_compliance_record');
END;

-- trigger entity_id_registry_codeedge_evaluator_evidence_handoffs_v2_id_immutable
CREATE TRIGGER entity_id_registry_codeedge_evaluator_evidence_handoffs_v2_id_immutable
BEFORE UPDATE OF id ON codeedge_evaluator_evidence_handoffs_v2
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_codeedge_evaluator_evidence_handoffs_v2_insert
CREATE TRIGGER entity_id_registry_codeedge_evaluator_evidence_handoffs_v2_insert
BEFORE INSERT ON codeedge_evaluator_evidence_handoffs_v2
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'codeedge_evaluator_evidence_handoff');
END;

-- trigger entity_id_registry_continuation_commands_v4_id_immutable
CREATE TRIGGER entity_id_registry_continuation_commands_v4_id_immutable
BEFORE UPDATE OF id ON continuation_commands_v4
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_continuation_commands_v4_insert
CREATE TRIGGER entity_id_registry_continuation_commands_v4_insert
BEFORE INSERT ON continuation_commands_v4
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'continuation_command');
END;

-- trigger entity_id_registry_continuation_executions_v4_id_immutable
CREATE TRIGGER entity_id_registry_continuation_executions_v4_id_immutable
BEFORE UPDATE OF id ON continuation_executions_v4
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_continuation_executions_v4_insert
CREATE TRIGGER entity_id_registry_continuation_executions_v4_insert
BEFORE INSERT ON continuation_executions_v4
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'continuation_execution');
END;

-- trigger entity_id_registry_control_operation_transitions_v5_id_immutable
CREATE TRIGGER entity_id_registry_control_operation_transitions_v5_id_immutable
BEFORE UPDATE OF id ON control_operation_transitions_v5
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_control_operation_transitions_v5_insert
CREATE TRIGGER entity_id_registry_control_operation_transitions_v5_insert
BEFORE INSERT ON control_operation_transitions_v5
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'control_operation_transition');
END;

-- trigger entity_id_registry_control_operations_v5_id_immutable
CREATE TRIGGER entity_id_registry_control_operations_v5_id_immutable
BEFORE UPDATE OF id ON control_operations_v5
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_control_operations_v5_insert
CREATE TRIGGER entity_id_registry_control_operations_v5_insert
BEFORE INSERT ON control_operations_v5
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'control_operation');
END;

-- trigger entity_id_registry_deletion_records_id_immutable
CREATE TRIGGER entity_id_registry_deletion_records_id_immutable
BEFORE UPDATE OF id ON deletion_records
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_deletion_records_insert
CREATE TRIGGER entity_id_registry_deletion_records_insert
BEFORE INSERT ON deletion_records
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'deletion_record');
END;

-- trigger entity_id_registry_frozen_plans_v4_id_immutable
CREATE TRIGGER entity_id_registry_frozen_plans_v4_id_immutable
BEFORE UPDATE OF id ON frozen_plans_v4
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_frozen_plans_v4_insert
CREATE TRIGGER entity_id_registry_frozen_plans_v4_insert
BEFORE INSERT ON frozen_plans_v4
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'frozen_plan');
END;

-- trigger entity_id_registry_job_dispatch_claims_v5_id_immutable
CREATE TRIGGER entity_id_registry_job_dispatch_claims_v5_id_immutable
BEFORE UPDATE OF id ON job_dispatch_claims_v5
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_job_dispatch_claims_v5_insert
CREATE TRIGGER entity_id_registry_job_dispatch_claims_v5_insert
BEFORE INSERT ON job_dispatch_claims_v5
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'job_dispatch_claim');
END;

-- trigger entity_id_registry_jobs_id_immutable
CREATE TRIGGER entity_id_registry_jobs_id_immutable
BEFORE UPDATE OF id ON jobs
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger jobs_failure_record_immutable
-- A transition writes state and its first failure record together. Once a
-- failure exists, neither a retrying worker nor an administrative SQL path can
-- replace the diagnostic that explains the original terminal delivery fact.
CREATE TRIGGER jobs_failure_record_immutable
BEFORE UPDATE OF failure_code, failure_message, failure_details_json ON jobs
WHEN OLD.failure_code <> '' OR OLD.failure_message <> '' OR OLD.failure_details_json <> '{}'
BEGIN
    SELECT RAISE(ABORT, 'durable job failure record is immutable');
END;

-- trigger entity_id_registry_jobs_insert
CREATE TRIGGER entity_id_registry_jobs_insert
BEFORE INSERT ON jobs
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'job');
END;

-- trigger entity_id_registry_leases_id_immutable
CREATE TRIGGER entity_id_registry_leases_id_immutable
BEFORE UPDATE OF id ON leases
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_leases_insert
CREATE TRIGGER entity_id_registry_leases_insert
BEFORE INSERT ON leases
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'lease');
END;

-- trigger entity_id_registry_lifecycle_operations_v12_id_immutable
CREATE TRIGGER entity_id_registry_lifecycle_operations_v12_id_immutable
BEFORE UPDATE OF id ON lifecycle_operations_v12
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_lifecycle_operations_v12_insert
CREATE TRIGGER entity_id_registry_lifecycle_operations_v12_insert
BEFORE INSERT ON lifecycle_operations_v12
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'lifecycle_operation');
END;

-- trigger entity_id_registry_mutation_receipts_v4_id_immutable
CREATE TRIGGER entity_id_registry_mutation_receipts_v4_id_immutable
BEFORE UPDATE OF id ON mutation_receipts_v4
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_mutation_receipts_v4_insert
CREATE TRIGGER entity_id_registry_mutation_receipts_v4_insert
BEFORE INSERT ON mutation_receipts_v4
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'mutation_receipt');
END;

-- trigger entity_id_registry_node_attempts_id_immutable
CREATE TRIGGER entity_id_registry_node_attempts_id_immutable
BEFORE UPDATE OF id ON node_attempts
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_node_attempts_insert
CREATE TRIGGER entity_id_registry_node_attempts_insert
BEFORE INSERT ON node_attempts
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'node_attempt');
END;

-- trigger entity_id_registry_outbox_delivery_operations_v9_id_immutable
CREATE TRIGGER entity_id_registry_outbox_delivery_operations_v9_id_immutable
BEFORE UPDATE OF id ON outbox_delivery_operations_v9
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_outbox_delivery_operations_v9_insert
CREATE TRIGGER entity_id_registry_outbox_delivery_operations_v9_insert
BEFORE INSERT ON outbox_delivery_operations_v9
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'outbox_delivery_operation');
END;

-- trigger entity_id_registry_outbox_events_id_immutable
CREATE TRIGGER entity_id_registry_outbox_events_id_immutable
BEFORE UPDATE OF id ON outbox_events
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_outbox_events_insert
CREATE TRIGGER entity_id_registry_outbox_events_insert
BEFORE INSERT ON outbox_events
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'outbox_event');
END;

-- trigger entity_id_registry_prepared_changes_v4_id_immutable
CREATE TRIGGER entity_id_registry_prepared_changes_v4_id_immutable
BEFORE UPDATE OF id ON prepared_changes_v4
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_prepared_changes_v4_insert
CREATE TRIGGER entity_id_registry_prepared_changes_v4_insert
BEFORE INSERT ON prepared_changes_v4
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'prepared_change');
END;

-- trigger entity_id_registry_quota_accounts_id_immutable
CREATE TRIGGER entity_id_registry_quota_accounts_id_immutable
BEFORE UPDATE OF id ON quota_accounts
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_quota_accounts_insert
CREATE TRIGGER entity_id_registry_quota_accounts_insert
BEFORE INSERT ON quota_accounts
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'quota_account');
END;

-- trigger entity_id_registry_quota_accounts_v5_id_immutable
CREATE TRIGGER entity_id_registry_quota_accounts_v5_id_immutable
BEFORE UPDATE OF id ON quota_accounts_v5
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_quota_accounts_v5_insert
CREATE TRIGGER entity_id_registry_quota_accounts_v5_insert
BEFORE INSERT ON quota_accounts_v5
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'quota_account_v5');
END;

-- trigger entity_id_registry_quota_budget_grants_v5_id_immutable
CREATE TRIGGER entity_id_registry_quota_budget_grants_v5_id_immutable
BEFORE UPDATE OF id ON quota_budget_grants_v5
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_quota_budget_grants_v5_insert
CREATE TRIGGER entity_id_registry_quota_budget_grants_v5_insert
BEFORE INSERT ON quota_budget_grants_v5
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'quota_budget_grant_v5');
END;

-- trigger entity_id_registry_quota_heartbeats_v5_id_immutable
CREATE TRIGGER entity_id_registry_quota_heartbeats_v5_id_immutable
BEFORE UPDATE OF id ON quota_heartbeats_v5
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_quota_heartbeats_v5_insert
CREATE TRIGGER entity_id_registry_quota_heartbeats_v5_insert
BEFORE INSERT ON quota_heartbeats_v5
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'quota_heartbeat_v5');
END;

-- trigger entity_id_registry_quota_leases_id_immutable
CREATE TRIGGER entity_id_registry_quota_leases_id_immutable
BEFORE UPDATE OF id ON quota_leases
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_quota_leases_insert
CREATE TRIGGER entity_id_registry_quota_leases_insert
BEFORE INSERT ON quota_leases
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'quota_lease');
END;

-- trigger entity_id_registry_quota_leases_v5_id_immutable
CREATE TRIGGER entity_id_registry_quota_leases_v5_id_immutable
BEFORE UPDATE OF id ON quota_leases_v5
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_quota_leases_v5_insert
CREATE TRIGGER entity_id_registry_quota_leases_v5_insert
BEFORE INSERT ON quota_leases_v5
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'quota_lease_v5');
END;

-- trigger entity_id_registry_quota_ledger_entries_id_immutable
CREATE TRIGGER entity_id_registry_quota_ledger_entries_id_immutable
BEFORE UPDATE OF id ON quota_ledger_entries
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_quota_ledger_entries_insert
CREATE TRIGGER entity_id_registry_quota_ledger_entries_insert
BEFORE INSERT ON quota_ledger_entries
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'quota_ledger_entry');
END;

-- trigger entity_id_registry_quota_policies_id_immutable
CREATE TRIGGER entity_id_registry_quota_policies_id_immutable
BEFORE UPDATE OF id ON quota_policies
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_quota_policies_insert
CREATE TRIGGER entity_id_registry_quota_policies_insert
BEFORE INSERT ON quota_policies
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'quota_policy');
END;

-- trigger entity_id_registry_quota_settlements_v5_id_immutable
CREATE TRIGGER entity_id_registry_quota_settlements_v5_id_immutable
BEFORE UPDATE OF id ON quota_settlements_v5
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_quota_settlements_v5_insert
CREATE TRIGGER entity_id_registry_quota_settlements_v5_insert
BEFORE INSERT ON quota_settlements_v5
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'quota_settlement_v5');
END;

-- trigger entity_id_registry_quota_usage_events_v5_id_immutable
CREATE TRIGGER entity_id_registry_quota_usage_events_v5_id_immutable
BEFORE UPDATE OF id ON quota_usage_events_v5
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_quota_usage_events_v5_insert
CREATE TRIGGER entity_id_registry_quota_usage_events_v5_insert
BEFORE INSERT ON quota_usage_events_v5
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'quota_usage_event_v5');
END;

-- trigger entity_id_registry_reconciliation_attempts_v5_id_immutable
CREATE TRIGGER entity_id_registry_reconciliation_attempts_v5_id_immutable
BEFORE UPDATE OF id ON reconciliation_attempts_v5
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_reconciliation_attempts_v5_insert
CREATE TRIGGER entity_id_registry_reconciliation_attempts_v5_insert
BEFORE INSERT ON reconciliation_attempts_v5
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'reconciliation_attempt');
END;

-- trigger entity_id_registry_release_withdraw_operations_v10_id_immutable
CREATE TRIGGER entity_id_registry_release_withdraw_operations_v10_id_immutable
BEFORE UPDATE OF id ON release_withdraw_operations_v10
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_release_withdraw_operations_v10_insert
CREATE TRIGGER entity_id_registry_release_withdraw_operations_v10_insert
BEFORE INSERT ON release_withdraw_operations_v10
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'release_withdraw_operation');
END;

-- trigger entity_id_registry_release_withdraw_receipts_v10_id_immutable
CREATE TRIGGER entity_id_registry_release_withdraw_receipts_v10_id_immutable
BEFORE UPDATE OF id ON release_withdraw_receipts_v10
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_release_withdraw_receipts_v10_insert
CREATE TRIGGER entity_id_registry_release_withdraw_receipts_v10_insert
BEFORE INSERT ON release_withdraw_receipts_v10
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'release_withdraw_receipt');
END;

-- trigger entity_id_registry_releases_id_immutable
CREATE TRIGGER entity_id_registry_releases_id_immutable
BEFORE UPDATE OF id ON releases
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_releases_insert
CREATE TRIGGER entity_id_registry_releases_insert
BEFORE INSERT ON releases
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'release');
END;

-- trigger entity_id_registry_repair_sessions_v4_id_immutable
CREATE TRIGGER entity_id_registry_repair_sessions_v4_id_immutable
BEFORE UPDATE OF id ON repair_sessions_v4
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_repair_sessions_v4_insert
CREATE TRIGGER entity_id_registry_repair_sessions_v4_insert
BEFORE INSERT ON repair_sessions_v4
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'repair_session');
END;

-- trigger entity_id_registry_review_decisions_id_immutable
CREATE TRIGGER entity_id_registry_review_decisions_id_immutable
BEFORE UPDATE OF id ON review_decisions
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_review_decisions_insert
CREATE TRIGGER entity_id_registry_review_decisions_insert
BEFORE INSERT ON review_decisions
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'review_decision');
END;

-- trigger entity_id_registry_review_requests_id_immutable
CREATE TRIGGER entity_id_registry_review_requests_id_immutable
BEFORE UPDATE OF id ON review_requests
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_review_requests_insert
CREATE TRIGGER entity_id_registry_review_requests_insert
BEFORE INSERT ON review_requests
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'review_request');
END;

-- trigger entity_id_registry_revision_candidates_v8_id_immutable
CREATE TRIGGER entity_id_registry_revision_candidates_v8_id_immutable
BEFORE UPDATE OF id ON revision_candidates_v8
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_revision_candidates_v8_insert
CREATE TRIGGER entity_id_registry_revision_candidates_v8_insert
BEFORE INSERT ON revision_candidates_v8
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'revision_candidate');
END;

-- trigger entity_id_registry_run_attempts_id_immutable
CREATE TRIGGER entity_id_registry_run_attempts_id_immutable
BEFORE UPDATE OF id ON run_attempts
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_run_attempts_insert
CREATE TRIGGER entity_id_registry_run_attempts_insert
BEFORE INSERT ON run_attempts
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'run_attempt');
END;

-- trigger entity_id_registry_run_worker_handoffs_v16_id_immutable
CREATE TRIGGER entity_id_registry_run_worker_handoffs_v16_id_immutable
BEFORE UPDATE OF id ON run_worker_handoffs_v16
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_run_worker_handoffs_v16_insert
CREATE TRIGGER entity_id_registry_run_worker_handoffs_v16_insert
BEFORE INSERT ON run_worker_handoffs_v16
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'run_worker_handoff');
END;

-- trigger entity_id_registry_runtime_termination_receipts_v5_id_immutable
CREATE TRIGGER entity_id_registry_runtime_termination_receipts_v5_id_immutable
BEFORE UPDATE OF id ON runtime_termination_receipts_v5
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_runtime_termination_receipts_v5_insert
CREATE TRIGGER entity_id_registry_runtime_termination_receipts_v5_insert
BEFORE INSERT ON runtime_termination_receipts_v5
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'runtime_termination_receipt');
END;

-- trigger entity_id_registry_side_effect_operations_v5_id_immutable
CREATE TRIGGER entity_id_registry_side_effect_operations_v5_id_immutable
BEFORE UPDATE OF id ON side_effect_operations_v5
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_side_effect_operations_v5_insert
CREATE TRIGGER entity_id_registry_side_effect_operations_v5_insert
BEFORE INSERT ON side_effect_operations_v5
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'side_effect_operation');
END;

-- trigger entity_id_registry_stage_attempts_id_immutable
CREATE TRIGGER entity_id_registry_stage_attempts_id_immutable
BEFORE UPDATE OF id ON stage_attempts
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_stage_attempts_insert
CREATE TRIGGER entity_id_registry_stage_attempts_insert
BEFORE INSERT ON stage_attempts
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'stage_attempt');
END;

-- trigger entity_id_registry_task_purge_operations_v7_id_immutable
CREATE TRIGGER entity_id_registry_task_purge_operations_v7_id_immutable
BEFORE UPDATE OF id ON task_purge_operations_v7
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_task_purge_operations_v7_insert
CREATE TRIGGER entity_id_registry_task_purge_operations_v7_insert
BEFORE INSERT ON task_purge_operations_v7
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'task_purge_operation');
END;

-- trigger entity_id_registry_task_revisions_id_immutable
CREATE TRIGGER entity_id_registry_task_revisions_id_immutable
BEFORE UPDATE OF id ON task_revisions
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_task_revisions_insert
CREATE TRIGGER entity_id_registry_task_revisions_insert
BEFORE INSERT ON task_revisions
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'task_revision');
END;

-- trigger entity_id_registry_tasks_v2_id_immutable
CREATE TRIGGER entity_id_registry_tasks_v2_id_immutable
BEFORE UPDATE OF id ON tasks_v2
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_tasks_v2_insert
CREATE TRIGGER entity_id_registry_tasks_v2_insert
BEFORE INSERT ON tasks_v2
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'task');
END;

-- trigger entity_id_registry_trial_attempts_v19_id_immutable
CREATE TRIGGER entity_id_registry_trial_attempts_v19_id_immutable
BEFORE UPDATE OF id ON trial_attempts_v19
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_trial_attempts_v19_insert
CREATE TRIGGER entity_id_registry_trial_attempts_v19_insert
BEFORE INSERT ON trial_attempts_v19
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'trial_attempt');
END;

-- trigger entity_id_registry_trial_executions_v19_id_immutable
CREATE TRIGGER entity_id_registry_trial_executions_v19_id_immutable
BEFORE UPDATE OF id ON trial_executions_v19
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_trial_executions_v19_insert
CREATE TRIGGER entity_id_registry_trial_executions_v19_insert
BEFORE INSERT ON trial_executions_v19
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'trial_execution');
END;

-- trigger entity_id_registry_turn_checkpoints_id_immutable
CREATE TRIGGER entity_id_registry_turn_checkpoints_id_immutable
BEFORE UPDATE OF id ON turn_checkpoints
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_turn_checkpoints_insert
CREATE TRIGGER entity_id_registry_turn_checkpoints_insert
BEFORE INSERT ON turn_checkpoints
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'turn_checkpoint');
END;

-- trigger entity_id_registry_usage_events_id_immutable
CREATE TRIGGER entity_id_registry_usage_events_id_immutable
BEFORE UPDATE OF id ON usage_events
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_usage_events_insert
CREATE TRIGGER entity_id_registry_usage_events_insert
BEFORE INSERT ON usage_events
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'usage_event');
END;

-- trigger entity_id_registry_workflow_runs_id_immutable
CREATE TRIGGER entity_id_registry_workflow_runs_id_immutable
BEFORE UPDATE OF id ON workflow_runs
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_workflow_runs_insert
CREATE TRIGGER entity_id_registry_workflow_runs_insert
BEFORE INSERT ON workflow_runs
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'workflow_run');
END;

-- trigger entity_id_registry_workspaces_v2_id_immutable
CREATE TRIGGER entity_id_registry_workspaces_v2_id_immutable
BEFORE UPDATE OF id ON workspaces_v2
WHEN NEW.id <> OLD.id
BEGIN
    SELECT RAISE(ABORT, 'lifecycle entity identity is immutable');
END;

-- trigger entity_id_registry_workspaces_v2_insert
CREATE TRIGGER entity_id_registry_workspaces_v2_insert
BEFORE INSERT ON workspaces_v2
BEGIN
    SELECT CASE WHEN EXISTS (SELECT 1 FROM entity_id_registry WHERE id = NEW.id)
        THEN RAISE(ABORT, 'global entity identity collision')
    END;
    INSERT INTO entity_id_registry (id, entity_type) VALUES (NEW.id, 'workspace');
END;

-- trigger frozen_plans_v4_no_delete
CREATE TRIGGER frozen_plans_v4_no_delete
BEFORE DELETE ON frozen_plans_v4
BEGIN
    SELECT RAISE(ABORT, 'frozen plans are immutable');
END;

-- trigger frozen_plans_v4_no_update
CREATE TRIGGER frozen_plans_v4_no_update
BEFORE UPDATE ON frozen_plans_v4
BEGIN
    SELECT RAISE(ABORT, 'frozen plans are immutable');
END;

-- trigger mutation_receipts_v4_no_delete
CREATE TRIGGER mutation_receipts_v4_no_delete
BEFORE DELETE ON mutation_receipts_v4
BEGIN
    SELECT RAISE(ABORT, 'mutation receipts are immutable');
END;

-- trigger mutation_receipts_v4_no_update
CREATE TRIGGER mutation_receipts_v4_no_update
BEFORE UPDATE ON mutation_receipts_v4
BEGIN
    SELECT RAISE(ABORT, 'mutation receipts are immutable');
END;

-- trigger prepared_changes_v4_no_delete
CREATE TRIGGER prepared_changes_v4_no_delete
BEFORE DELETE ON prepared_changes_v4
BEGIN
    SELECT RAISE(ABORT, 'prepared changes are immutable');
END;

-- trigger prepared_changes_v4_no_update
CREATE TRIGGER prepared_changes_v4_no_update
BEFORE UPDATE ON prepared_changes_v4
BEGIN
    SELECT RAISE(ABORT, 'prepared changes are immutable');
END;

-- trigger releases_content_immutable
CREATE TRIGGER releases_content_immutable
BEFORE UPDATE ON releases
WHEN NEW.id <> OLD.id
  OR NEW.version <> OLD.version
  OR NEW.revision_id <> OLD.revision_id
  OR NEW.task_id <> OLD.task_id
  OR NEW.task_digest <> OLD.task_digest
  OR NEW.package_ref <> OLD.package_ref
  OR NEW.evidence_ref <> OLD.evidence_ref
  OR NEW.published_at <> OLD.published_at
  OR NEW.created_by <> OLD.created_by
BEGIN
    SELECT RAISE(ABORT, 'release content is immutable');
END;

-- trigger releases_no_delete
CREATE TRIGGER releases_no_delete
BEFORE DELETE ON releases
BEGIN
    SELECT RAISE(ABORT, 'releases permanently pin their evidence');
END;

-- trigger releases_withdraw_transition
CREATE TRIGGER releases_withdraw_transition
BEFORE UPDATE ON releases
WHEN NEW.record_version <> OLD.record_version + 1
  OR OLD.withdrawn_at IS NOT NULL
  OR NEW.withdrawn_at IS NULL
  OR NEW.withdrawn_by = ''
BEGIN
    SELECT RAISE(ABORT, 'invalid release withdrawal transition');
END;

-- trigger review_gate_bindings_v15_no_delete
CREATE TRIGGER review_gate_bindings_v15_no_delete
BEFORE DELETE ON review_gate_bindings_v15
BEGIN
    SELECT RAISE(ABORT, 'review gate binding is immutable');
END;

-- trigger review_gate_bindings_v15_no_update
CREATE TRIGGER review_gate_bindings_v15_no_update
BEFORE UPDATE ON review_gate_bindings_v15
BEGIN
    SELECT RAISE(ABORT, 'review gate binding is immutable');
END;

-- trigger task_purge_v7_blocks_task_mutation
CREATE TRIGGER task_purge_v7_blocks_task_mutation
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

-- trigger authoring_sources_v2_immutable
CREATE TRIGGER authoring_sources_v2_immutable
BEFORE UPDATE ON authoring_sources_v2
BEGIN
    SELECT RAISE(ABORT, 'authoring sources are immutable');
END;

-- trigger authoring_sources_v2_no_delete
CREATE TRIGGER authoring_sources_v2_no_delete
BEFORE DELETE ON authoring_sources_v2
BEGIN
    SELECT RAISE(ABORT, 'authoring sources are immutable');
END;

-- trigger authoring_sessions_v2_immutable
CREATE TRIGGER authoring_sessions_v2_immutable
BEFORE UPDATE ON authoring_sessions_v2
BEGIN
    SELECT RAISE(ABORT, 'authoring sessions are immutable');
END;

-- trigger authoring_sessions_v2_no_delete
CREATE TRIGGER authoring_sessions_v2_no_delete
BEFORE DELETE ON authoring_sessions_v2
BEGIN
    SELECT RAISE(ABORT, 'authoring sessions are immutable');
END;

-- trigger authoring_task_materializations_v2_immutable
CREATE TRIGGER authoring_task_materializations_v2_immutable
BEFORE UPDATE ON authoring_task_materializations_v2
BEGIN
    SELECT RAISE(ABORT, 'authoring task materializations are immutable');
END;

-- trigger authoring_task_materializations_v2_no_delete
CREATE TRIGGER authoring_task_materializations_v2_no_delete
BEFORE DELETE ON authoring_task_materializations_v2
BEGIN
    SELECT RAISE(ABORT, 'authoring task materializations are immutable');
END;

-- trigger authoring_phase1_handoffs_v2_immutable
CREATE TRIGGER authoring_phase1_handoffs_v2_immutable
BEFORE UPDATE ON authoring_phase1_handoffs_v2
BEGIN
    SELECT RAISE(ABORT, 'authoring Phase-1 handoffs are immutable');
END;

-- trigger authoring_phase1_handoffs_v2_no_delete
CREATE TRIGGER authoring_phase1_handoffs_v2_no_delete
BEFORE DELETE ON authoring_phase1_handoffs_v2
BEGIN
    SELECT RAISE(ABORT, 'authoring Phase-1 handoffs are immutable');
END;

-- trigger authoring_run_input_artifacts_v2_immutable
CREATE TRIGGER authoring_run_input_artifacts_v2_immutable
BEFORE UPDATE ON authoring_run_input_artifacts_v2
BEGIN
    SELECT RAISE(ABORT, 'authoring run input artifacts are immutable');
END;

-- trigger authoring_run_input_artifacts_v2_no_delete
CREATE TRIGGER authoring_run_input_artifacts_v2_no_delete
BEFORE DELETE ON authoring_run_input_artifacts_v2
BEGIN
    SELECT RAISE(ABORT, 'authoring run input artifacts are immutable');
END;

-- trigger authoring_review_requests_v22_immutable
CREATE TRIGGER authoring_review_requests_v22_immutable
BEFORE UPDATE ON authoring_review_requests_v22
BEGIN
    SELECT RAISE(ABORT, 'authoring review requests are immutable');
END;

-- trigger authoring_review_requests_v22_no_delete
CREATE TRIGGER authoring_review_requests_v22_no_delete
BEFORE DELETE ON authoring_review_requests_v22
BEGIN
    SELECT RAISE(ABORT, 'authoring review requests are append-only');
END;

-- trigger authoring_review_gate_bindings_v22_immutable
CREATE TRIGGER authoring_review_gate_bindings_v22_immutable
BEFORE UPDATE ON authoring_review_gate_bindings_v22
BEGIN
    SELECT RAISE(ABORT, 'authoring review gate bindings are immutable');
END;

-- trigger authoring_review_gate_bindings_v22_no_delete
CREATE TRIGGER authoring_review_gate_bindings_v22_no_delete
BEFORE DELETE ON authoring_review_gate_bindings_v22
BEGIN
    SELECT RAISE(ABORT, 'authoring review gate bindings are append-only');
END;

-- trigger authoring_review_decisions_v22_immutable
CREATE TRIGGER authoring_review_decisions_v22_immutable
BEFORE UPDATE ON authoring_review_decisions_v22
BEGIN
    SELECT RAISE(ABORT, 'authoring review decisions are immutable');
END;

-- trigger authoring_review_decisions_v22_no_delete
CREATE TRIGGER authoring_review_decisions_v22_no_delete
BEFORE DELETE ON authoring_review_decisions_v22
BEGIN
    SELECT RAISE(ABORT, 'authoring review decisions are append-only');
END;

-- trigger authoring_review_gate_resolutions_v22_immutable
CREATE TRIGGER authoring_review_gate_resolutions_v22_immutable
BEFORE UPDATE ON authoring_review_gate_resolutions_v22
BEGIN
    SELECT RAISE(ABORT, 'authoring review gate resolutions are immutable');
END;

-- trigger authoring_review_gate_resolutions_v22_no_delete
CREATE TRIGGER authoring_review_gate_resolutions_v22_no_delete
BEFORE DELETE ON authoring_review_gate_resolutions_v22
BEGIN
    SELECT RAISE(ABORT, 'authoring review gate resolutions are append-only');
END;

-- trigger authoring_review_requests_v22_lineage_insert
-- A source/session request must bind exactly the immutable generic subject of
-- its authoring Run. No TaskRevision is valid on this side of materialization.
CREATE TRIGGER authoring_review_requests_v22_lineage_insert
BEFORE INSERT ON authoring_review_requests_v22
WHEN NOT EXISTS (
    SELECT 1
    FROM workflow_runs AS run
    JOIN authoring_sessions_v2 AS session ON session.id = run.authoring_session_id
    JOIN authoring_sources_v2 AS source ON source.id = session.source_id
    WHERE run.id = NEW.run_id
      AND run.subject_kind = 'authoring_session'
      AND run.task_id IS NULL
      AND run.revision_id IS NULL
      AND run.authoring_session_id = NEW.authoring_session_id
      AND run.subject_id = NEW.authoring_source_id
      AND run.subject_revision_id = NEW.authoring_session_id
      AND run.subject_digest = NEW.source_snapshot_digest
      AND session.id = NEW.authoring_session_id
      AND session.source_id = NEW.authoring_source_id
      AND source.id = NEW.authoring_source_id
      AND source.snapshot_content_digest = NEW.source_snapshot_digest
      AND run.definition_hash = NEW.definition_hash
)
BEGIN
    SELECT RAISE(ABORT, 'authoring review request does not match source/session run lineage');
END;

-- trigger authoring_review_gate_bindings_v22_lineage_insert
CREATE TRIGGER authoring_review_gate_bindings_v22_lineage_insert
BEFORE INSERT ON authoring_review_gate_bindings_v22
WHEN NOT EXISTS (
    SELECT 1
    FROM authoring_review_requests_v22 AS request
    JOIN stage_attempts AS stage ON stage.id = NEW.stage_attempt_id
    JOIN node_attempts AS node ON node.id = NEW.node_attempt_id
    WHERE request.id = NEW.review_request_id
      AND request.run_id = NEW.run_id
      AND request.authoring_session_id = NEW.authoring_session_id
      AND request.authoring_source_id = NEW.authoring_source_id
      AND request.source_snapshot_digest = NEW.source_snapshot_digest
      AND request.definition_hash = NEW.definition_hash
      AND request.evidence_manifest_digest = NEW.evidence_manifest_digest
      AND stage.run_id = NEW.run_id
      AND stage.stage_key = NEW.stage_key
      AND stage.input_fingerprint = NEW.input_fingerprint
      AND stage.execution_status IN ('queued', 'running')
      AND node.stage_attempt_id = NEW.stage_attempt_id
      AND node.node_id = NEW.stage_key
      AND node.generation = NEW.node_generation
      AND node.attempt = NEW.node_attempt_ordinal
      AND node.status = 'waiting'
)
BEGIN
    SELECT RAISE(ABORT, 'authoring review gate binding does not match frozen request or stage lineage');
END;

-- trigger authoring_review_decisions_v22_lineage_insert
CREATE TRIGGER authoring_review_decisions_v22_lineage_insert
BEFORE INSERT ON authoring_review_decisions_v22
WHEN NOT EXISTS (
    SELECT 1
    FROM authoring_review_requests_v22 AS request
    JOIN authoring_review_gate_bindings_v22 AS binding ON binding.review_request_id = request.id
    WHERE request.id = NEW.review_request_id
      AND binding.id = NEW.binding_id
)
BEGIN
    SELECT RAISE(ABORT, 'authoring review decision does not match immutable gate binding');
END;

-- trigger authoring_review_gate_resolutions_v22_lineage_insert
CREATE TRIGGER authoring_review_gate_resolutions_v22_lineage_insert
BEFORE INSERT ON authoring_review_gate_resolutions_v22
WHEN NOT EXISTS (
    SELECT 1
    FROM authoring_review_requests_v22 AS request
    JOIN authoring_review_gate_bindings_v22 AS binding ON binding.review_request_id = request.id
    JOIN authoring_review_decisions_v22 AS decision ON decision.binding_id = binding.id
    WHERE request.id = NEW.review_request_id
      AND binding.id = NEW.binding_id
      AND decision.id = NEW.decision_id
      AND decision.review_request_id = NEW.review_request_id
      AND (
          (decision.action = 'approve' AND NEW.verdict = 'pass')
          OR (decision.action = 'request_changes' AND NEW.verdict = 'needs_repair')
          OR (decision.action = 'reject_terminal' AND NEW.verdict = 'reject')
      )
)
BEGIN
    SELECT RAISE(ABORT, 'authoring review gate resolution does not match immutable decision lineage');
END;

-- trigger workflow_runs_subject_binding_insert
-- FK checks establish that referenced rows exist. This trigger additionally
-- proves a task revision belongs to its task and keeps authoring Runs rooted
-- only in their immutable AuthoringSession.
CREATE TRIGGER workflow_runs_subject_binding_insert
BEFORE INSERT ON workflow_runs
WHEN (
    NEW.subject_kind = 'task_revision'
    AND NOT EXISTS (
        SELECT 1 FROM task_revisions
        WHERE id = NEW.revision_id
          AND task_id = NEW.task_id
          AND NEW.subject_id = NEW.task_id
          AND NEW.subject_revision_id = NEW.revision_id
          AND NEW.subject_digest = task_digest
    )
) OR (
    NEW.subject_kind = 'authoring_session'
    AND NOT EXISTS (
        SELECT 1
        FROM authoring_sessions_v2 AS session
        JOIN authoring_sources_v2 AS source ON source.id = session.source_id
        WHERE session.id = NEW.authoring_session_id
          AND NEW.subject_id = source.id
          AND NEW.subject_revision_id = session.id
          AND NEW.subject_digest = source.snapshot_content_digest
    )
)
BEGIN
    SELECT RAISE(ABORT, 'workflow run subject binding is invalid');
END;

-- trigger authoring_run_input_artifacts_v2_binding_insert
CREATE TRIGGER authoring_run_input_artifacts_v2_binding_insert
BEFORE INSERT ON authoring_run_input_artifacts_v2
WHEN NOT EXISTS (
    SELECT 1
    FROM workflow_runs AS run
    JOIN authoring_sessions_v2 AS session ON session.id = run.authoring_session_id
    JOIN authoring_sources_v2 AS source ON source.id = session.source_id
    WHERE run.id = NEW.run_id
      AND run.subject_kind = 'authoring_session'
      AND run.authoring_session_id = NEW.session_id
      AND NEW.source_id = source.id
      AND NEW.source_fingerprint = source.source_fingerprint
      AND NEW.snapshot_artifact_ref = source.snapshot_artifact_ref
      AND NEW.content_digest = source.snapshot_content_digest
      AND NEW.schema_version = source.snapshot_schema_version
)
BEGIN
    SELECT RAISE(ABORT, 'authoring run input does not match its frozen source subject');
END;

-- trigger authoring_task_materializations_v2_binding_insert
CREATE TRIGGER authoring_task_materializations_v2_binding_insert
BEFORE INSERT ON authoring_task_materializations_v2
WHEN NOT EXISTS (
    SELECT 1
    FROM authoring_sessions_v2 AS session
    JOIN authoring_sources_v2 AS source ON source.id = session.source_id
    JOIN workflow_runs AS run ON run.id = NEW.authoring_run_id
    JOIN tasks_v2 AS task ON task.id = NEW.task_id
    JOIN task_revisions AS revision ON revision.id = NEW.revision_id
    WHERE session.id = NEW.session_id
      AND source.id = NEW.source_id
      AND source.source_fingerprint = NEW.source_fingerprint
      AND session.target_task_id = task.id
      AND task.source_repo = source.repository_url
      AND task.source_commit = source.commit_sha
      AND run.subject_kind = 'authoring_session'
      AND run.authoring_session_id = session.id
      AND run.subject_id = source.id
      AND run.subject_revision_id = session.id
      AND run.subject_digest = source.snapshot_content_digest
      AND revision.task_id = task.id
      AND revision.version_number = 1
      AND revision.parent_revision_id IS NULL
      AND revision.origin = 'generated'
      AND revision.state = 'sealed'
      AND revision.task_digest = NEW.task_digest
)
BEGIN
    SELECT RAISE(ABORT, 'authoring task materialization does not match frozen session/run/source lineage');
END;

-- trigger authoring_phase1_handoffs_v2_binding_insert
-- The Store cannot parse the handoff object bytes, but it still proves every
-- relational fact around that artifact: it must be the unique completed
-- materialize_task output of the same source/session Run and the one sealed
-- generated revision recorded by authoring_task_materializations_v2.
CREATE TRIGGER authoring_phase1_handoffs_v2_binding_insert
BEFORE INSERT ON authoring_phase1_handoffs_v2
WHEN NOT EXISTS (
    SELECT 1
    FROM workflow_runs AS run
    JOIN authoring_sessions_v2 AS session ON session.id = run.authoring_session_id
    JOIN authoring_sources_v2 AS source ON source.id = session.source_id
    JOIN authoring_task_materializations_v2 AS materialization ON materialization.authoring_run_id = run.id
    JOIN task_revisions AS revision ON revision.id = materialization.revision_id
    JOIN artifact_refs_v4 AS artifact ON artifact.id = NEW.handoff_artifact_id
    JOIN stage_attempts AS attempt ON attempt.id = artifact.attempt_id
    WHERE run.id = NEW.authoring_run_id
      AND run.subject_kind = 'authoring_session'
      AND run.workflow_template_id = 'harbor.standard-authoring'
      AND (
          (run.workflow_template_version = '1.2.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v1')
          OR
          (run.workflow_template_version = '1.3.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')
          OR
          (run.workflow_template_version = '1.4.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')
          OR
          (run.workflow_template_version = '1.5.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')
          OR
          (run.workflow_template_version = '1.6.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')
      )
      AND run.authoring_session_id = NEW.authoring_session_id
      AND run.subject_id = NEW.authoring_source_id
      AND run.subject_revision_id = NEW.authoring_session_id
      AND session.source_id = NEW.authoring_source_id
      AND session.target_task_id = NEW.task_id
      AND materialization.session_id = NEW.authoring_session_id
      AND materialization.source_id = NEW.authoring_source_id
      AND materialization.task_id = NEW.task_id
      AND materialization.revision_id = NEW.revision_id
      AND materialization.task_digest = NEW.task_digest
      AND revision.task_id = NEW.task_id
      AND revision.task_digest = NEW.task_digest
      AND revision.origin = 'generated'
      AND revision.state = 'sealed'
      AND artifact.run_id = run.id
      AND artifact.stage_key = 'materialize_task'
      AND artifact.artifact_key = 'authoring_task_handoff'
      AND artifact.subject_revision_id = NEW.authoring_session_id
      AND artifact.subject_digest = source.snapshot_content_digest
      AND attempt.run_id = run.id
      AND attempt.id = artifact.attempt_id
      AND attempt.stage_key = 'materialize_task'
      AND attempt.execution_status = 'completed'
      AND attempt.verdict IN ('pass', 'advisory')
)
BEGIN
    SELECT RAISE(ABORT, 'authoring Phase-1 handoff does not match persisted materialization lineage');
END;

-- trigger workflow_runs_content_immutable
-- Execution state/epoch may advance, but a frozen workflow Run can never be
-- rebound from a session to a revision or vice versa.
CREATE TRIGGER workflow_runs_content_immutable
BEFORE UPDATE ON workflow_runs
WHEN NEW.subject_kind <> OLD.subject_kind
  OR NEW.subject_id <> OLD.subject_id
  OR NEW.subject_revision_id <> OLD.subject_revision_id
  OR NEW.subject_digest <> OLD.subject_digest
  OR NEW.task_id IS NOT OLD.task_id
  OR NEW.revision_id IS NOT OLD.revision_id
  OR NEW.authoring_session_id IS NOT OLD.authoring_session_id
  OR NEW.workflow_template_id <> OLD.workflow_template_id
  OR NEW.workflow_template_version <> OLD.workflow_template_version
  OR NEW.resolved_profile_hash <> OLD.resolved_profile_hash
  OR NEW.definition_hash <> OLD.definition_hash
  OR NEW.run_manifest_json <> OLD.run_manifest_json
  OR NEW.parent_run_id IS NOT OLD.parent_run_id
  OR NEW.trigger <> OLD.trigger
  OR NEW.created_by <> OLD.created_by
  OR NEW.created_at <> OLD.created_at
BEGIN
    SELECT RAISE(ABORT, 'workflow run content is immutable');
END;

-- trigger task_revisions_content_immutable
CREATE TRIGGER task_revisions_content_immutable
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

-- trigger task_revisions_no_delete
CREATE TRIGGER task_revisions_no_delete
BEFORE DELETE ON task_revisions
BEGIN
    SELECT RAISE(ABORT, 'task revisions are immutable');
END;

-- trigger task_revisions_state_transition
CREATE TRIGGER task_revisions_state_transition
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

-- trigger trial_attempts_v19_attempt_identity_immutable
CREATE TRIGGER trial_attempts_v19_attempt_identity_immutable
BEFORE UPDATE ON trial_attempts_v19
WHEN NEW.trial_execution_id <> OLD.trial_execution_id
  OR NEW.retry_of_trial_attempt_id IS NOT OLD.retry_of_trial_attempt_id
  OR NEW.ordinal <> OLD.ordinal
  OR NEW.created_at <> OLD.created_at
BEGIN
    SELECT RAISE(ABORT, 'trial attempt identity is immutable');
END;

-- trigger trial_attempts_v19_no_delete
CREATE TRIGGER trial_attempts_v19_no_delete
BEFORE DELETE ON trial_attempts_v19
BEGIN
    SELECT RAISE(ABORT, 'trial attempts are append-only');
END;

-- trigger trial_attempts_v19_parent_status
CREATE TRIGGER trial_attempts_v19_parent_status
BEFORE UPDATE OF status ON trial_attempts_v19
WHEN NEW.status <> OLD.status
 AND (
    (NEW.status = 'running' AND COALESCE((SELECT status FROM trial_executions_v19 WHERE id = NEW.trial_execution_id), '') <> 'running')
 OR (NEW.status = 'waiting' AND COALESCE((SELECT status FROM trial_executions_v19 WHERE id = NEW.trial_execution_id), '') <> 'waiting')
 OR (NEW.status = 'in_doubt' AND COALESCE((SELECT status FROM trial_executions_v19 WHERE id = NEW.trial_execution_id), '') <> 'in_doubt')
 OR (NEW.status = 'reconciling' AND COALESCE((SELECT status FROM trial_executions_v19 WHERE id = NEW.trial_execution_id), '') <> 'reconciling')
 OR (NEW.status IN ('infra_failed', 'interrupted') AND COALESCE((SELECT status FROM trial_executions_v19 WHERE id = NEW.trial_execution_id), '') <> 'running')
 )
BEGIN
    SELECT RAISE(ABORT, 'trial attempt does not match logical trial state');
END;

-- trigger trial_attempts_v19_retry_lineage
CREATE TRIGGER trial_attempts_v19_retry_lineage
BEFORE INSERT ON trial_attempts_v19
WHEN (NEW.ordinal = 1 AND NEW.retry_of_trial_attempt_id IS NOT NULL)
  OR (NEW.ordinal > 1 AND (
      NEW.retry_of_trial_attempt_id IS NULL
      OR NOT EXISTS (
          SELECT 1
          FROM trial_attempts_v19 AS predecessor
          WHERE predecessor.id = NEW.retry_of_trial_attempt_id
            AND predecessor.trial_execution_id = NEW.trial_execution_id
            AND predecessor.ordinal = NEW.ordinal - 1
            AND predecessor.status IN ('infra_failed', 'interrupted')
      )
  ))
BEGIN
    SELECT RAISE(ABORT, 'invalid trial retry lineage');
END;

-- trigger trial_attempts_v19_retry_requires_running_execution
CREATE TRIGGER trial_attempts_v19_retry_requires_running_execution
BEFORE INSERT ON trial_attempts_v19
WHEN NEW.ordinal > 1
 AND EXISTS (
    SELECT 1
    FROM trial_attempts_v19 AS predecessor
    WHERE predecessor.id = NEW.retry_of_trial_attempt_id
      AND predecessor.trial_execution_id = NEW.trial_execution_id
      AND predecessor.ordinal = NEW.ordinal - 1
      AND predecessor.status IN ('infra_failed', 'interrupted')
 )
 AND COALESCE((SELECT status FROM trial_executions_v19 WHERE id = NEW.trial_execution_id), '') <> 'running'
BEGIN
    SELECT RAISE(ABORT, 'trial retry requires running logical execution');
END;

-- trigger trial_attempts_v19_status_transition
CREATE TRIGGER trial_attempts_v19_status_transition
BEFORE UPDATE OF status ON trial_attempts_v19
WHEN NEW.status <> OLD.status
 AND NOT (
    (OLD.status = 'queued' AND NEW.status IN ('running', 'waiting', 'canceled'))
 OR (OLD.status = 'running' AND NEW.status IN ('waiting', 'completed', 'infra_failed', 'interrupted', 'in_doubt', 'canceled'))
 OR (OLD.status = 'waiting' AND NEW.status IN ('running', 'completed', 'infra_failed', 'interrupted', 'in_doubt', 'canceled'))
 OR (OLD.status = 'in_doubt' AND NEW.status = 'reconciling')
 OR (OLD.status = 'reconciling' AND NEW.status IN ('completed', 'infra_failed', 'interrupted', 'canceled'))
 )
BEGIN
    SELECT RAISE(ABORT, 'invalid trial attempt status transition');
END;

-- trigger trial_executions_v19_logical_identity_immutable
CREATE TRIGGER trial_executions_v19_logical_identity_immutable
BEFORE UPDATE ON trial_executions_v19
WHEN NEW.run_id <> OLD.run_id
  OR NEW.stage_attempt_id <> OLD.stage_attempt_id
  OR NEW.stage_key <> OLD.stage_key
  OR NEW.ordinal <> OLD.ordinal
  OR NEW.created_at <> OLD.created_at
BEGIN
    SELECT RAISE(ABORT, 'trial execution logical identity is immutable');
END;

-- trigger trial_executions_v19_no_delete
CREATE TRIGGER trial_executions_v19_no_delete
BEFORE DELETE ON trial_executions_v19
BEGIN
    SELECT RAISE(ABORT, 'trial executions are append-only');
END;

-- trigger trial_executions_v19_stage_binding
CREATE TRIGGER trial_executions_v19_stage_binding
BEFORE INSERT ON trial_executions_v19
WHEN NOT EXISTS (
    SELECT 1
    FROM stage_attempts
    WHERE id = NEW.stage_attempt_id
      AND run_id = NEW.run_id
      AND stage_key = NEW.stage_key
)
BEGIN
    SELECT RAISE(ABORT, 'trial execution does not match stage attempt');
END;

-- trigger trial_executions_v19_status_transition
CREATE TRIGGER trial_executions_v19_status_transition
BEFORE UPDATE OF status ON trial_executions_v19
WHEN NEW.status <> OLD.status
 AND NOT (
    (OLD.status = 'queued' AND NEW.status IN ('running', 'waiting', 'canceled'))
 OR (OLD.status = 'running' AND NEW.status IN ('waiting', 'completed', 'infra_failed', 'interrupted', 'in_doubt', 'canceled'))
 OR (OLD.status = 'waiting' AND NEW.status IN ('running', 'completed', 'infra_failed', 'interrupted', 'in_doubt', 'canceled'))
 OR (OLD.status = 'in_doubt' AND NEW.status = 'reconciling')
 OR (OLD.status = 'reconciling' AND NEW.status IN ('running', 'completed', 'infra_failed', 'interrupted', 'canceled'))
 )
BEGIN
    SELECT RAISE(ABORT, 'invalid trial execution status transition');
END;

-- trigger trial_executions_v19_terminal_requires_no_active_attempt
CREATE TRIGGER trial_executions_v19_terminal_requires_no_active_attempt
BEFORE UPDATE OF status ON trial_executions_v19
WHEN NEW.status <> OLD.status
 AND NEW.status IN ('completed', 'infra_failed', 'interrupted', 'canceled')
 AND EXISTS (
    SELECT 1
    FROM trial_attempts_v19
    WHERE trial_execution_id = NEW.id
      AND status IN ('queued', 'running', 'waiting', 'in_doubt', 'reconciling')
 )
BEGIN
    SELECT RAISE(ABORT, 'terminal trial execution has active technical attempt');
END;

`
