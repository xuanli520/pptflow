package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

type Store struct {
	db *sql.DB
}

type ProjectSummary struct {
	TaskID        string
	Batch         string
	Path          string
	RunCount      int
	LastRunID     string
	LastRunAt     string
	RunStatus     string
	ManualVerdict string
	FailedStage   string
	Blocking      int
	High          int
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	handle, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	store := &Store{db: handle}
	if err := store.Migrate(context.Background()); err != nil {
		_ = handle.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Migrate(ctx context.Context) error {
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
		`CREATE TABLE IF NOT EXISTS findings (
			id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL REFERENCES runs(run_id),
			stage TEXT,
			severity TEXT NOT NULL,
			title TEXT NOT NULL,
			rule TEXT,
			evidence TEXT,
			impact TEXT,
			minimum_fix TEXT,
			source_path TEXT
		);`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) UpsertProjects(ctx context.Context, projects []scanner.Project) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, project := range projects {
		_, err := tx.ExecContext(ctx, `INSERT INTO projects(task_id, batch, path)
			VALUES(?, ?, ?)
			ON CONFLICT(task_id) DO UPDATE SET batch=excluded.batch, path=excluded.path`,
			project.TaskID, project.Batch, project.Path)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetProject(ctx context.Context, taskID string) (scanner.Project, error) {
	var project scanner.Project
	row := s.db.QueryRowContext(ctx, `SELECT task_id, batch, path FROM projects WHERE task_id = ?`, taskID)
	if err := row.Scan(&project.TaskID, &project.Batch, &project.Path); err != nil {
		return project, err
	}
	project.MetadataPromptMissing = metadataPromptMissing(project.Path)
	return project, nil
}

func (s *Store) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id, batch, path, run_count, COALESCE(last_run_id, ''), COALESCE(last_run_at, '') FROM projects ORDER BY task_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var projects []ProjectSummary
	for rows.Next() {
		var project ProjectSummary
		if err := rows.Scan(&project.TaskID, &project.Batch, &project.Path, &project.RunCount, &project.LastRunID, &project.LastRunAt); err != nil {
			return nil, err
		}
		if project.LastRunID != "" {
			run, err := s.GetRun(ctx, project.LastRunID)
			if err == nil {
				project.RunStatus = run.Status
				project.ManualVerdict = run.ManualVerdict
				project.FailedStage = firstFailedStage(ctx, s, project.LastRunID)
				project.Blocking, project.High = findingCounts(ctx, s, project.LastRunID)
			}
		}
		projects = append(projects, project)
	}
	return projects, rows.Err()
}

func (s *Store) CreateRun(ctx context.Context, run model.RunRecord) error {
	staticOnly := 0
	if run.StaticOnly {
		staticOnly = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs(run_id, task_id, started_at, status, manual_verdict, static_only, artifact_root, tool_versions, prompt_versions)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.RunID, run.TaskID, run.StartedAt, run.Status, run.ManualVerdict, staticOnly, run.ArtifactRoot, run.ToolVersions, run.PromptVersions)
	return err
}

func (s *Store) FinishRun(ctx context.Context, runID, taskID, status string, duration time.Duration) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE runs SET finished_at = ?, status = ?, duration_ms = ? WHERE run_id = ?`,
		now, status, duration.Milliseconds(), runID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE projects SET run_count = run_count + 1, last_run_id = ?, last_run_at = ? WHERE task_id = ?`,
		runID, now, taskID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetRun(ctx context.Context, runID string) (model.RunRecord, error) {
	var run model.RunRecord
	var staticOnly int
	row := s.db.QueryRowContext(ctx, `SELECT run_id, task_id, COALESCE(started_at,''), COALESCE(finished_at,''), status, manual_verdict, static_only, duration_ms, artifact_root, COALESCE(tool_versions,''), COALESCE(prompt_versions,'') FROM runs WHERE run_id = ?`, runID)
	err := row.Scan(&run.RunID, &run.TaskID, &run.StartedAt, &run.FinishedAt, &run.Status, &run.ManualVerdict, &staticOnly, &run.DurationMS, &run.ArtifactRoot, &run.ToolVersions, &run.PromptVersions)
	run.StaticOnly = staticOnly == 1
	return run, err
}

func (s *Store) LatestRunForTask(ctx context.Context, taskID string) (model.RunRecord, error) {
	var runID string
	row := s.db.QueryRowContext(ctx, `SELECT COALESCE(last_run_id, '') FROM projects WHERE task_id = ?`, taskID)
	if err := row.Scan(&runID); err != nil {
		return model.RunRecord{}, err
	}
	if runID == "" {
		return model.RunRecord{}, sql.ErrNoRows
	}
	return s.GetRun(ctx, runID)
}

func (s *Store) PutStage(ctx context.Context, runID string, stage model.StageRecord) error {
	blockedBy, _ := json.Marshal(stage.BlockedBy)
	artifacts, _ := json.Marshal(stage.ArtifactPaths)
	_, err := s.db.ExecContext(ctx, `INSERT INTO run_stages(run_id, stage, status, started_at, finished_at, duration_ms, blocked_by, log_path, artifact_json, error_summary)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, stage) DO UPDATE SET status=excluded.status, started_at=excluded.started_at, finished_at=excluded.finished_at, duration_ms=excluded.duration_ms, blocked_by=excluded.blocked_by, log_path=excluded.log_path, artifact_json=excluded.artifact_json, error_summary=excluded.error_summary`,
		runID, stage.Stage, stage.Status, stage.StartedAt, stage.FinishedAt, stage.DurationMS, string(blockedBy), stage.LogPath, string(artifacts), stage.ErrorSummary)
	return err
}

func (s *Store) InsertFindings(ctx context.Context, runID string, findings []model.Finding) error {
	for _, finding := range findings {
		_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO findings(id, run_id, stage, severity, title, rule, evidence, impact, minimum_fix, source_path)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			finding.ID, runID, finding.Stage, finding.Severity, finding.Title, finding.Rule, finding.Evidence, finding.Impact, finding.MinimumFix, finding.SourcePath)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Stages(ctx context.Context, runID string) ([]model.StageRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT stage, status, COALESCE(started_at,''), COALESCE(finished_at,''), duration_ms, COALESCE(blocked_by,'[]'), COALESCE(log_path,''), COALESCE(artifact_json,'[]'), COALESCE(error_summary,'') FROM run_stages WHERE run_id = ? ORDER BY stage`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stages []model.StageRecord
	for rows.Next() {
		var stage model.StageRecord
		var blockedBy string
		var artifacts string
		if err := rows.Scan(&stage.Stage, &stage.Status, &stage.StartedAt, &stage.FinishedAt, &stage.DurationMS, &blockedBy, &stage.LogPath, &artifacts, &stage.ErrorSummary); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(blockedBy), &stage.BlockedBy)
		_ = json.Unmarshal([]byte(artifacts), &stage.ArtifactPaths)
		stages = append(stages, stage)
	}
	return stages, rows.Err()
}

func (s *Store) Findings(ctx context.Context, runID string) ([]model.Finding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, stage, severity, title, COALESCE(rule,''), COALESCE(evidence,''), COALESCE(impact,''), COALESCE(minimum_fix,''), COALESCE(source_path,'') FROM findings WHERE run_id = ? ORDER BY severity, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var findings []model.Finding
	for rows.Next() {
		var finding model.Finding
		if err := rows.Scan(&finding.ID, &finding.Stage, &finding.Severity, &finding.Title, &finding.Rule, &finding.Evidence, &finding.Impact, &finding.MinimumFix, &finding.SourcePath); err != nil {
			return nil, err
		}
		findings = append(findings, finding)
	}
	return findings, rows.Err()
}

func firstFailedStage(ctx context.Context, s *Store, runID string) string {
	row := s.db.QueryRowContext(ctx, `SELECT COALESCE(stage, '') FROM run_stages WHERE run_id = ? AND status IN ('failed', 'blocked') ORDER BY stage LIMIT 1`, runID)
	var stage string
	if err := row.Scan(&stage); err != nil {
		return ""
	}
	return stage
}

func findingCounts(ctx context.Context, s *Store, runID string) (int, int) {
	var blocker int
	var high int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM findings WHERE run_id = ? AND severity = 'Blocker'`, runID).Scan(&blocker)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM findings WHERE run_id = ? AND severity = 'High'`, runID).Scan(&high)
	return blocker, high
}

func metadataPromptMissing(projectPath string) bool {
	content, err := os.ReadFile(filepath.Join(projectPath, "metadata.json"))
	if err != nil {
		return true
	}
	var data map[string]any
	if json.Unmarshal(content, &data) != nil {
		return true
	}
	prompt, ok := data["prompt"].(string)
	return !ok || prompt == ""
}

func FormatNotFound(kind, id string) error {
	return fmt.Errorf("%s not found: %s", kind, id)
}
