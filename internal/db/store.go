package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
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

type ArtifactPruneItem struct {
	TaskID string
	Batch  string
	Path   string
	Runs   int
}

type ArtifactPruneResult struct {
	Removed []ArtifactPruneItem
	Skipped []ArtifactPruneItem
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	handle, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	handle.SetMaxOpenConns(8)
	handle.SetMaxIdleConns(3)
	if err := configureSQLite(handle); err != nil {
		_ = handle.Close()
		return nil, err
	}
	store := &Store{db: handle}
	if err := store.Migrate(context.Background()); err != nil {
		_ = handle.Close()
		return nil, err
	}
	return store, nil
}

func sqliteDSN(path string) string {
	if path == ":memory:" {
		return path
	}
	if strings.HasPrefix(path, "file:") {
		u, err := url.Parse(path)
		if err != nil {
			return path
		}
		return withSQLitePragmas(*u)
	}
	u := url.URL{Scheme: "file", Path: filepath.Clean(path)}
	return withSQLitePragmas(u)
}

func withSQLitePragmas(u url.URL) string {
	q := u.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	u.RawQuery = q.Encode()
	return u.String()
}

func configureSQLite(handle *sql.DB) error {
	for _, statement := range []string{
		`PRAGMA busy_timeout = 5000;`,
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA synchronous = NORMAL;`,
	} {
		if _, err := handle.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Migrate(ctx context.Context) error {
	return migrate(ctx, s.db)
}

func (s *Store) withWriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *Store) UpsertProjects(ctx context.Context, projects []scanner.Project) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		for _, project := range projects {
			_, err := tx.ExecContext(ctx, `INSERT INTO projects(task_id, batch, path)
			VALUES(?, ?, ?)
			ON CONFLICT(task_id) DO UPDATE SET batch=excluded.batch, path=excluded.path`,
				project.TaskID, project.Batch, project.Path)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) PruneArtifactProjects(ctx context.Context, scanRoot string) (ArtifactPruneResult, error) {
	var result ArtifactPruneResult
	scanRoot = filepath.Clean(scanRoot)
	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT task_id, batch, path FROM projects ORDER BY task_id`)
		if err != nil {
			return err
		}
		var candidates []ArtifactPruneItem
		for rows.Next() {
			var item ArtifactPruneItem
			if err := rows.Scan(&item.TaskID, &item.Batch, &item.Path); err != nil {
				_ = rows.Close()
				return err
			}
			if artifactProjectPath(scanRoot, item.Path) {
				candidates = append(candidates, item)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, item := range candidates {
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE task_id = ?`, item.TaskID).Scan(&item.Runs); err != nil {
				return err
			}
			if item.Runs > 0 {
				result.Skipped = append(result.Skipped, item)
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE task_id = ?`, item.TaskID); err != nil {
				return err
			}
			result.Removed = append(result.Removed, item)
		}
		return nil
	})
	return result, err
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
	var projects []ProjectSummary
	for rows.Next() {
		var project ProjectSummary
		if err := rows.Scan(&project.TaskID, &project.Batch, &project.Path, &project.RunCount, &project.LastRunID, &project.LastRunAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range projects {
		project := &projects[i]
		run, err := s.LatestRunForTask(ctx, project.TaskID)
		if err == nil {
			project.LastRunID = run.RunID
			project.LastRunAt = firstNonEmptyString(run.FinishedAt, run.StartedAt, project.LastRunAt)
			project.RunStatus = run.Status
			project.ManualVerdict = run.ManualVerdict
			project.FailedStage = firstFailedStage(ctx, s, run.RunID)
			project.Blocking, project.High = findingCounts(ctx, s, run.RunID)
		}
	}
	return projects, nil
}

func (s *Store) CreateRun(ctx context.Context, run model.RunRecord) error {
	staticOnly := 0
	if run.StaticOnly {
		staticOnly = 1
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO runs(run_id, task_id, started_at, status, manual_verdict, static_only, artifact_root, tool_versions, prompt_versions)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			run.RunID, run.TaskID, run.StartedAt, run.Status, run.ManualVerdict, staticOnly, run.ArtifactRoot, run.ToolVersions, run.PromptVersions)
		if err != nil {
			return err
		}
		startedAt := firstNonEmptyString(run.StartedAt, time.Now().UTC().Format(time.RFC3339))
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET last_run_id = ?, last_run_at = ? WHERE task_id = ?`, run.RunID, startedAt, run.TaskID); err != nil {
			return err
		}
		return nil
	})
}

func (s *Store) FinishRun(ctx context.Context, runID, taskID, status string, duration time.Duration) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE runs SET finished_at = ?, status = ?, duration_ms = ? WHERE run_id = ?`,
			now, status, duration.Milliseconds(), runID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE projects SET run_count = run_count + 1, last_run_id = ?, last_run_at = ? WHERE task_id = ?`,
			runID, now, taskID)
		if err != nil {
			return err
		}
		return nil
	})
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
	row := s.db.QueryRowContext(ctx, `SELECT run_id FROM runs WHERE task_id = ? ORDER BY COALESCE(started_at, '') DESC, run_id DESC LIMIT 1`, taskID)
	if err := row.Scan(&runID); err != nil {
		return model.RunRecord{}, err
	}
	return s.GetRun(ctx, runID)
}

func (s *Store) ListRunsForTask(ctx context.Context, taskID string) ([]model.RunRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, task_id, COALESCE(started_at,''), COALESCE(finished_at,''), status, manual_verdict, static_only, duration_ms, artifact_root, COALESCE(tool_versions,''), COALESCE(prompt_versions,'') FROM runs WHERE task_id = ? ORDER BY started_at DESC, run_id DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []model.RunRecord
	for rows.Next() {
		var run model.RunRecord
		var staticOnly int
		if err := rows.Scan(&run.RunID, &run.TaskID, &run.StartedAt, &run.FinishedAt, &run.Status, &run.ManualVerdict, &staticOnly, &run.DurationMS, &run.ArtifactRoot, &run.ToolVersions, &run.PromptVersions); err != nil {
			return nil, err
		}
		run.StaticOnly = staticOnly == 1
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) RunningRuns(ctx context.Context) ([]model.RunRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, task_id, COALESCE(started_at,''), COALESCE(finished_at,''), status, manual_verdict, static_only, duration_ms, artifact_root, COALESCE(tool_versions,''), COALESCE(prompt_versions,'') FROM runs WHERE status = ? ORDER BY started_at DESC, run_id DESC`, model.RunRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []model.RunRecord
	for rows.Next() {
		var run model.RunRecord
		var staticOnly int
		if err := rows.Scan(&run.RunID, &run.TaskID, &run.StartedAt, &run.FinishedAt, &run.Status, &run.ManualVerdict, &staticOnly, &run.DurationMS, &run.ArtifactRoot, &run.ToolVersions, &run.PromptVersions); err != nil {
			return nil, err
		}
		run.StaticOnly = staticOnly == 1
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) PutStage(ctx context.Context, runID string, stage model.StageRecord) error {
	blockedBy, _ := json.Marshal(stage.BlockedBy)
	artifacts, _ := json.Marshal(stage.ArtifactPaths)
	name := stage.Name
	if name == "" {
		name = model.StageDisplayName(stage.Stage)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO run_stages(run_id, stage, name, status, started_at, finished_at, duration_ms, blocked_by, log_path, artifact_json, error_summary)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, stage) DO UPDATE SET name=excluded.name, status=excluded.status, started_at=excluded.started_at, finished_at=excluded.finished_at, duration_ms=excluded.duration_ms, blocked_by=excluded.blocked_by, log_path=excluded.log_path, artifact_json=excluded.artifact_json, error_summary=excluded.error_summary`,
		runID, stage.Stage, name, stage.Status, stage.StartedAt, stage.FinishedAt, stage.DurationMS, string(blockedBy), stage.LogPath, string(artifacts), stage.ErrorSummary)
	return err
}

func (s *Store) InsertFindings(ctx context.Context, runID string, findings []model.Finding) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		for _, finding := range findings {
			_, err := tx.ExecContext(ctx, `INSERT INTO findings(id, run_id, stage, severity, title, rule, evidence, impact, done_criteria, minimum_fix, source_path)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(run_id, id) DO UPDATE SET stage=excluded.stage, severity=excluded.severity, title=excluded.title, rule=excluded.rule, evidence=excluded.evidence, impact=excluded.impact, done_criteria=excluded.done_criteria, minimum_fix=excluded.minimum_fix, source_path=excluded.source_path`,
				finding.ID, runID, finding.Stage, finding.Severity, finding.Title, finding.Rule, finding.Evidence, finding.Impact, finding.DoneCriteria, finding.MinimumFix, finding.SourcePath)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) Stages(ctx context.Context, runID string) ([]model.StageRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT stage, COALESCE(name,''), status, COALESCE(started_at,''), COALESCE(finished_at,''), duration_ms, COALESCE(blocked_by,'[]'), COALESCE(log_path,''), COALESCE(artifact_json,'[]'), COALESCE(error_summary,'') FROM run_stages WHERE run_id = ? ORDER BY CASE stage WHEN 'A' THEN 1 WHEN 'B' THEN 2 WHEN 'C' THEN 3 WHEN 'D' THEN 4 WHEN 'E' THEN 5 WHEN 'F' THEN 6 ELSE 99 END, stage`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stages []model.StageRecord
	for rows.Next() {
		var stage model.StageRecord
		var blockedBy string
		var artifacts string
		if err := rows.Scan(&stage.Stage, &stage.Name, &stage.Status, &stage.StartedAt, &stage.FinishedAt, &stage.DurationMS, &blockedBy, &stage.LogPath, &artifacts, &stage.ErrorSummary); err != nil {
			return nil, err
		}
		if stage.Name == "" {
			stage.Name = model.StageDisplayName(stage.Stage)
		}
		_ = json.Unmarshal([]byte(blockedBy), &stage.BlockedBy)
		_ = json.Unmarshal([]byte(artifacts), &stage.ArtifactPaths)
		stages = append(stages, stage)
	}
	return stages, rows.Err()
}

func (s *Store) Findings(ctx context.Context, runID string) ([]model.Finding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, stage, severity, title, COALESCE(rule,''), COALESCE(evidence,''), COALESCE(impact,''), COALESCE(done_criteria,''), COALESCE(minimum_fix,''), COALESCE(source_path,'') FROM findings WHERE run_id = ? ORDER BY severity, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var findings []model.Finding
	for rows.Next() {
		var finding model.Finding
		if err := rows.Scan(&finding.ID, &finding.Stage, &finding.Severity, &finding.Title, &finding.Rule, &finding.Evidence, &finding.Impact, &finding.DoneCriteria, &finding.MinimumFix, &finding.SourcePath); err != nil {
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

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func FormatNotFound(kind, id string) error {
	return fmt.Errorf("%s not found: %s", kind, id)
}

func artifactProjectPath(scanRoot, projectPath string) bool {
	rel, ok := relUnderRoot(scanRoot, projectPath)
	if !ok || rel == "." {
		return false
	}
	parts := splitPathParts(rel)
	if len(parts) == 0 {
		return false
	}
	switch parts[0] {
	case "result", ".qa-control":
		return true
	}
	for index, part := range parts {
		if part == "script_input_snapshot" {
			return true
		}
		if part == "qa" && index+1 < len(parts) && parts[index+1] == "runs" {
			return true
		}
	}
	return false
}

func relUnderRoot(root, path string) (string, bool) {
	root = absClean(root)
	path = absClean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.Clean(rel), true
}

func splitPathParts(path string) []string {
	path = filepath.Clean(path)
	if path == "." {
		return nil
	}
	return strings.Split(path, string(filepath.Separator))
}

func absClean(path string) string {
	cleaned := filepath.Clean(path)
	if abs, err := filepath.Abs(cleaned); err == nil {
		return abs
	}
	return cleaned
}
