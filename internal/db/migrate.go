package db

import (
	"context"
	"database/sql"
	"fmt"
)

const currentSchemaVersion = 5

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
		case 3:
			if err := migrateV3ToV4(ctx, tx); err != nil {
				return err
			}
			version = 4
		case 4:
			if err := migrateV4ToV5(ctx, tx); err != nil {
				return err
			}
			version = 5
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
	if err := ensureReadIndexes(ctx, tx); err != nil {
		return err
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
	if err := ensureTaskTables(ctx, tx); err != nil {
		return err
	}
	if err := ensureSchemaVersion(ctx, tx, currentSchemaVersion); err != nil {
		return err
	}
	return ensureReadIndexes(ctx, tx)
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
			prompt_versions TEXT,
			completion_round INTEGER NOT NULL DEFAULT 1
		);`,
		`CREATE TABLE IF NOT EXISTS run_stages (
			run_id TEXT NOT NULL REFERENCES runs(run_id),
			stage TEXT NOT NULL,
			name TEXT,
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
		stageColumns, stageErr := tableColumns(ctx, tx, "run_stages")
		if stageErr != nil {
			return 0, stageErr
		}
		if _, hasName := stageColumns["name"]; !hasName {
			return 3, nil
		}
		if migrated, err := hasV5Tables(ctx, tx); err != nil || !migrated {
			return 4, err
		}
		return currentSchemaVersion, nil
	}
	columns, err := findingColumns(ctx, tx)
	if err != nil {
		return 0, err
	}
	if columns["run_id"] > 0 && columns["id"] > 0 {
		stageColumns, stageErr := tableColumns(ctx, tx, "run_stages")
		if stageErr != nil {
			return 0, stageErr
		}
		if _, ok := columns["done_criteria"]; ok {
			if _, hasName := stageColumns["name"]; hasName {
				if migrated, err := hasV5Tables(ctx, tx); err != nil || !migrated {
					return 4, err
				}
				return currentSchemaVersion, nil
			}
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

func migrateV3ToV4(ctx context.Context, tx *sql.Tx) error {
	columns, err := tableColumns(ctx, tx, "run_stages")
	if err != nil {
		return err
	}
	if _, ok := columns["name"]; ok {
		return nil
	}
	_, err = tx.ExecContext(ctx, `ALTER TABLE run_stages ADD COLUMN name TEXT;`)
	return err
}

func migrateV4ToV5(ctx context.Context, tx *sql.Tx) error {
	runColumns, err := tableColumns(ctx, tx, "runs")
	if err != nil {
		return err
	}
	if _, ok := runColumns["completion_round"]; !ok {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE runs ADD COLUMN completion_round INTEGER NOT NULL DEFAULT 1;`); err != nil {
			return err
		}
	}
	if err := ensureTaskTables(ctx, tx); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO batches(id, display_name, task_count, max_tasks, created_at, is_full)
		SELECT batch, batch, 0, 20, datetime('now'), 0
		FROM projects
		WHERE TRIM(batch) <> ''
		GROUP BY batch;`)
	return err
}

func ensureTaskTables(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS batches (
			id TEXT PRIMARY KEY,
			display_name TEXT NOT NULL,
			task_count INTEGER NOT NULL DEFAULT 0,
			max_tasks INTEGER NOT NULL DEFAULT 20,
			created_at TEXT NOT NULL,
			is_full INTEGER NOT NULL DEFAULT 0
		);`,
		`CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY,
			batch_id TEXT NOT NULL,
			git_url TEXT NOT NULL,
			repo_path TEXT NOT NULL,
			state TEXT NOT NULL DEFAULT 'inspecting' CHECK (state IN ('inspecting', 'waiting_manual', 'completed')),
			current_run_id TEXT REFERENCES runs(run_id) ON DELETE SET NULL,
			completion_count INTEGER NOT NULL DEFAULT 0 CHECK (completion_count >= 0),
			frontend_url TEXT DEFAULT '',
			docker_running INTEGER NOT NULL DEFAULT 0 CHECK (docker_running IN (0, 1)),
			compose_meta TEXT DEFAULT '',
			entered_waiting_at TEXT DEFAULT '',
			last_completed_at TEXT DEFAULT '',
			sync_error TEXT DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			FOREIGN KEY (batch_id) REFERENCES batches(id)
		);`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func ensureReadIndexes(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`CREATE INDEX IF NOT EXISTS idx_runs_task_started ON runs(task_id, started_at DESC, run_id DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_runs_task_status ON runs(task_id, status);`,
		`CREATE INDEX IF NOT EXISTS idx_runs_task_completion_round ON runs(task_id, completion_round DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_run_stages_run_status_stage ON run_stages(run_id, status, stage);`,
		`CREATE INDEX IF NOT EXISTS idx_findings_run_severity ON findings(run_id, severity);`,
		`CREATE INDEX IF NOT EXISTS idx_projects_batch ON projects(batch);`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_state ON tasks(state);`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_batch ON tasks(batch_id);`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_batch_state ON tasks(batch_id, state);`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_state_docker ON tasks(state, docker_running);`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
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

func hasV5Tables(ctx context.Context, tx *sql.Tx) (bool, error) {
	runColumns, err := tableColumns(ctx, tx, "runs")
	if err != nil {
		return false, err
	}
	if _, ok := runColumns["completion_round"]; !ok {
		return false, nil
	}
	tasks, err := tableExists(ctx, tx, "tasks")
	if err != nil || !tasks {
		return false, err
	}
	batches, err := tableExists(ctx, tx, "batches")
	if err != nil || !batches {
		return false, err
	}
	return true, nil
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
	return tableColumns(ctx, tx, "findings")
}

func tableColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]int, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
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
