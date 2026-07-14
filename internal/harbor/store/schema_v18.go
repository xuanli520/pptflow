package store

import (
	"database/sql"
	"fmt"
)

// migrationV18 makes a RepairSession a bounded aggregate over an ordered
// chain of distinct continuation commands. The initial command remains the
// session provenance root; every later round is linked by its candidate and
// must use the next ordinal exactly once.
const migrationV18 = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_revision_candidates_v8_repair_round
    ON revision_candidates_v8(repair_session_id, round_ordinal)
    WHERE repair_session_id IS NOT NULL;
`

func applyMigrationV18(tx *sql.Tx) error {
	if _, err := tx.Exec(migrationV18); err != nil {
		return fmt.Errorf("install repair-session round uniqueness: %w", err)
	}
	return nil
}
