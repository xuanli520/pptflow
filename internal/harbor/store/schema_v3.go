package store

// migrationV3 upgrades the initial V2 control plane with the repository-level
// CAS metadata and indexes required by managed execution, local package
// releases, outbox delivery, and deletion planning. It contains no runtime,
// continuation, quota, or external-provider behavior.
const migrationV3 = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_v2_canonical_import_identity
    ON tasks_v2(legacy_identity, source_repo, source_commit)
    WHERE identity_state = 'canonical' AND legacy_identity <> '';

CREATE INDEX IF NOT EXISTS idx_workspaces_v2_state ON workspaces_v2(state, updated_at);

ALTER TABLE run_attempts ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0);

ALTER TABLE node_attempts ADD COLUMN created_at DATETIME;
ALTER TABLE node_attempts ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0);

ALTER TABLE turn_checkpoints ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0);

ALTER TABLE outbox_events ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0);
CREATE UNIQUE INDEX IF NOT EXISTS idx_outbox_events_idempotency
    ON outbox_events(topic, entity_type, entity_id, idempotency_key)
    WHERE idempotency_key <> '';

ALTER TABLE releases ADD COLUMN withdrawn_by TEXT NOT NULL DEFAULT '';
ALTER TABLE releases ADD COLUMN record_version INTEGER NOT NULL DEFAULT 1 CHECK (record_version > 0);
CREATE TRIGGER IF NOT EXISTS releases_content_immutable
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
CREATE TRIGGER IF NOT EXISTS releases_withdraw_transition
BEFORE UPDATE ON releases
WHEN NEW.record_version <> OLD.record_version + 1
  OR OLD.withdrawn_at IS NOT NULL
  OR NEW.withdrawn_at IS NULL
  OR NEW.withdrawn_by = ''
BEGIN
    SELECT RAISE(ABORT, 'invalid release withdrawal transition');
END;
CREATE TRIGGER IF NOT EXISTS releases_no_delete
BEFORE DELETE ON releases
BEGIN
    SELECT RAISE(ABORT, 'releases permanently pin their evidence');
END;

ALTER TABLE deletion_records ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0);
`
