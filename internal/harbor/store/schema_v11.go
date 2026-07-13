package store

import "database/sql"

// migrationV11 records the frozen code policy that initialized each durable
// V5 task/actor quota account. The table owns no new UUID identity: account_id
// is a foreign key to the globally registered quota-account identity.
const migrationV11 = `
CREATE TABLE IF NOT EXISTS quota_account_policy_bindings_v11 (
    account_id          TEXT PRIMARY KEY REFERENCES quota_accounts_v5(id) ON DELETE RESTRICT,
    policy_id           TEXT NOT NULL,
    policy_version      TEXT NOT NULL,
    policy_fingerprint  TEXT NOT NULL,
    initial_limit_units INTEGER NOT NULL CHECK (initial_limit_units > 0),
    bound_at            DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_quota_account_policy_bindings_v11_policy
    ON quota_account_policy_bindings_v11(policy_id, policy_version, account_id);
`

func applyMigrationV11(tx *sql.Tx) error {
	_, err := tx.Exec(migrationV11)
	return err
}
