package store

import (
	"database/sql"
	"fmt"
)

// migrationV13 persists the Run fence used to create every durable dispatch
// claim. In particular, an empty scoped claim must replay only for that same
// Run; deriving scope from a missing job would make the idempotency boundary
// ambiguous.
const migrationV13 = `
ALTER TABLE job_dispatch_claims_v5 ADD COLUMN run_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_job_dispatch_claims_v5_run_state
    ON job_dispatch_claims_v5(run_id, state, claimed_at);
`

func applyMigrationV13(tx *sql.Tx) error {
	if _, err := tx.Exec(migrationV13); err != nil {
		return fmt.Errorf("add durable dispatch claim run fence: %w", err)
	}
	return nil
}
