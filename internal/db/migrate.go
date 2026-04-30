package db

import (
	"context"
	"database/sql"
	"fmt"
)

const currentSchemaVersion = 3

func migrate(ctx context.Context, handle *sql.DB) error {
	tx, err := handle.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	version, err := currentVersion(ctx, tx)
	if err != nil {
		return err
	}
	if version == 0 {
		empty, err := isEmptyDatabase(ctx, tx)
		if err != nil {
			return err
		}
		if empty {
			if err := createCurrentSchema(ctx, tx); err != nil {
				return err
			}
			return tx.Commit()
		}
		version, err = inferLegacyVersion(ctx, tx)
		if err != nil {
			return err
		}
		if err := ensureCoreTables(ctx, tx); err != nil {
			return err
		}
		if err := ensureSchemaVersion(ctx, tx, version); err != nil {
			return err
		}
	}
	for version < currentSchemaVersion {
		switch version {
		case 1:
			if err := migrateV1ToV2(ctx, tx); err != nil {
				return err
			}
			version = 2
		case 2:
			if err := migrateV2ToV3(ctx, tx); err != nil {
				return err
			}
			version = 3
		default:
			return fmt.Errorf("unsupported schema version %d", version)
		}
		if err := setSchemaVersion(ctx, tx, version); err != nil {
			return err
		}
	}
	if version != currentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, currentSchemaVersion)
	}
	return tx.Commit()
}

func currentVersion(ctx context.Context, tx *sql.Tx) (int, error) {
	exists, err := tableExists(ctx, tx, "schema_version")
	if err != nil || !exists {
		return 0, err
	}
	var version int
	err = tx.QueryRowContext(ctx, `SELECT version FROM schema_version WHERE id = 1`).Scan(&version)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return version, err
}

func isEmptyDatabase(ctx context.Context, tx *sql.Tx) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type IN ('table', 'index', 'trigger') AND name NOT LIKE 'sqlite_%'`).Scan(&count)
	return count == 0, err
}

func createCurrentSchema(ctx context.Context, tx *sql.Tx) error {
	if err := ensureCoreTables(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, currentFindingsDDL); err != nil {
		return err
	}
	return ensureSchemaVersion(ctx, tx, currentSchemaVersion)
}

func ensureCoreTables(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS projects (
			task_id TEXT PRIMARY KEY,
			batch TEXT NOT NULL,
			path TEXT NOT NULL,
			run_count INTEGER DEFAULT 0,
			last_run_id TEXT,
			last_run_at TEXT,
			created_at TEXT DEFAULT (datetime('now'))
		);`,
		`CREATE TABLE IF NOT EXISTS runs (
			run_id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES projects(task_id),
			started_at TEXT,
			finished_at TEXT,
			status TEXT DEFAULT 'running',
			manual_verdict TEXT DEFAULT 'unset',
			static_only INTEGER DEFAULT 0,
			duration_ms INTEGER DEFAULT 0,
			artifact_root TEXT NOT NULL,
			tool_versions TEXT,
			prompt_versions TEXT
		);`,
		`CREATE TABLE IF NOT EXISTS run_stages (
			run_id TEXT NOT NULL REFERENCES runs(run_id),
			stage TEXT NOT NULL,
			status TEXT NOT NULL,
			started_at TEXT,
			finished_at TEXT,
			duration_ms INTEGER DEFAULT 0,
			blocked_by TEXT,
			log_path TEXT,
			artifact_json TEXT,
			error_summary TEXT,
			PRIMARY KEY (run_id, stage)
		);`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

const currentFindingsDDL = `CREATE TABLE IF NOT EXISTS findings (
	id TEXT NOT NULL,
	run_id TEXT NOT NULL REFERENCES runs(run_id),
	stage TEXT,
	severity TEXT NOT NULL,
	title TEXT NOT NULL,
	rule TEXT,
	evidence TEXT,
	impact TEXT,
	done_criteria TEXT,
	minimum_fix TEXT,
	source_path TEXT,
	PRIMARY KEY (run_id, id)
);`

func inferLegacyVersion(ctx context.Context, tx *sql.Tx) (int, error) {
	exists, err := tableExists(ctx, tx, "findings")
	if err != nil || !exists {
		if _, err := tx.ExecContext(ctx, currentFindingsDDL); err != nil {
			return 0, err
		}
		return currentSchemaVersion, nil
	}
	columns, err := findingColumns(ctx, tx)
	if err != nil {
		return 0, err
	}
	if columns["run_id"] > 0 && columns["id"] > 0 {
		if _, ok := columns["done_criteria"]; ok {
			return 3, nil
		}
		return 2, nil
	}
	return 1, nil
}

func migrateV1ToV2(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE findings_v2 (
		id TEXT NOT NULL,
		run_id TEXT NOT NULL REFERENCES runs(run_id),
		stage TEXT,
		severity TEXT NOT NULL,
		title TEXT NOT NULL,
		rule TEXT,
		evidence TEXT,
		impact TEXT,
		minimum_fix TEXT,
		source_path TEXT,
		PRIMARY KEY (run_id, id)
	);`)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO findings_v2(id, run_id, stage, severity, title, rule, evidence, impact, minimum_fix, source_path)
		SELECT f.id, f.run_id, f.stage, f.severity, f.title, f.rule, f.evidence, f.impact, f.minimum_fix, f.source_path
		FROM findings f
		JOIN (
			SELECT run_id, id, MAX(rowid) AS rowid
			FROM findings
			GROUP BY run_id, id
		) latest ON latest.rowid = f.rowid;`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE findings;`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `ALTER TABLE findings_v2 RENAME TO findings;`)
	return err
}

func migrateV2ToV3(ctx context.Context, tx *sql.Tx) error {
	columns, err := findingColumns(ctx, tx)
	if err != nil {
		return err
	}
	if _, ok := columns["done_criteria"]; ok {
		return nil
	}
	_, err = tx.ExecContext(ctx, `ALTER TABLE findings ADD COLUMN done_criteria TEXT;`)
	return err
}

func ensureSchemaVersion(ctx context.Context, tx *sql.Tx, version int) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (
		id INTEGER PRIMARY KEY CHECK(id = 1),
		version INTEGER NOT NULL
	);`); err != nil {
		return err
	}
	return setSchemaVersion(ctx, tx, version)
}

func setSchemaVersion(ctx context.Context, tx *sql.Tx, version int) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO schema_version(id, version) VALUES(1, ?)
		ON CONFLICT(id) DO UPDATE SET version = excluded.version`, version)
	return err
}

func tableExists(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var found string
	err := tx.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func findingColumns(ctx context.Context, tx *sql.Tx) (map[string]int, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(findings)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]int{}
	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		columns[name] = pk
	}
	return columns, rows.Err()
}
