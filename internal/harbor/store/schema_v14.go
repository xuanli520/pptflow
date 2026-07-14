package store

import (
	"database/sql"
	"fmt"
)

// migrationV14 separates an operation's original optimistic checkpoint from
// server-allocated target identities. A Fork, StartRun, or Package operation
// cannot safely reconstruct its source checkpoint from the target rows after a
// process interruption.
const migrationV14 = `
ALTER TABLE lifecycle_operations_v12 ADD COLUMN expected_task_id TEXT NOT NULL DEFAULT '';
ALTER TABLE lifecycle_operations_v12 ADD COLUMN expected_revision_id TEXT NOT NULL DEFAULT '';
ALTER TABLE lifecycle_operations_v12 ADD COLUMN expected_run_id TEXT NOT NULL DEFAULT '';
ALTER TABLE lifecycle_operations_v12 ADD COLUMN expected_release_id TEXT NOT NULL DEFAULT '';
ALTER TABLE lifecycle_operations_v12 ADD COLUMN expected_review_request_id TEXT NOT NULL DEFAULT '';
`

func applyMigrationV14(tx *sql.Tx) error {
	if _, err := tx.Exec(migrationV14); err != nil {
		return fmt.Errorf("add lifecycle operation expected identities: %w", err)
	}
	return nil
}
