package store

import (
	"database/sql"
	"fmt"
)

// migrationV9 turns the historical outbox audit list into a fenced local
// delivery queue. The event row remains the source of truth for immutable
// routing and payload facts; delivery state is updated only by the dispatcher
// APIs below. Operation records retain exact idempotent responses after a
// worker crashes between a SQLite commit and its caller receiving the result.
const migrationV9 = `
ALTER TABLE outbox_events ADD COLUMN available_at DATETIME;
ALTER TABLE outbox_events ADD COLUMN lease_owner TEXT NOT NULL DEFAULT '';
ALTER TABLE outbox_events ADD COLUMN lease_expires_at DATETIME;
ALTER TABLE outbox_events ADD COLUMN lease_fencing_token INTEGER NOT NULL DEFAULT 0 CHECK (lease_fencing_token >= 0);
ALTER TABLE outbox_events ADD COLUMN delivery_count INTEGER NOT NULL DEFAULT 0 CHECK (delivery_count >= 0);
ALTER TABLE outbox_events ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE outbox_events ADD COLUMN updated_at DATETIME;

UPDATE outbox_events
SET available_at = created_at,
    updated_at = created_at
WHERE available_at IS NULL OR updated_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_outbox_events_dispatchable_v9
    ON outbox_events(state, available_at, created_at, id);
CREATE INDEX IF NOT EXISTS idx_outbox_events_lease_expiry_v9
    ON outbox_events(state, lease_expires_at, id);

CREATE TABLE IF NOT EXISTS outbox_delivery_operations_v9 (
    id                  TEXT PRIMARY KEY,
    idempotency_key     TEXT NOT NULL UNIQUE,
    kind                TEXT NOT NULL CHECK (kind IN ('claim', 'heartbeat', 'ack', 'nack')),
    request_fingerprint TEXT NOT NULL,
    response_json       TEXT NOT NULL,
    created_at          DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_outbox_delivery_operations_v9_created
    ON outbox_delivery_operations_v9(created_at, id);
`

var outboxDeliveryOperationIdentitySource = globalIdentitySource{
	table:      "outbox_delivery_operations_v9",
	entityType: "outbox_delivery_operation",
}

func applyMigrationV9(tx *sql.Tx) error {
	if _, err := tx.Exec(migrationV9); err != nil {
		return err
	}
	source := outboxDeliveryOperationIdentitySource
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
