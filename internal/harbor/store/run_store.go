package store

import (
	"database/sql"
	"strings"
	"time"
)

type SortColumn int

const (
	SortByStartedAt SortColumn = iota
	SortByTaskName
	SortByStatus
	SortBySizeBytes
)

// Run is a record from the runs table.
type Run struct {
	ID            int64
	TaskID        int64
	WorkspacePath string
	RunID         string
	Status        string
	Passed        bool
	StartedAt     time.Time
	FinishedAt    time.Time
	SizeBytes     int64
	IsActive      bool
	IsResumable   bool
	CreatedAt     time.Time
}

// RunWithTask is a JOIN result between runs and tasks.
type RunWithTask struct {
	Run  Run
	Task Task
}

func (s *Store) UpsertRun(r Run) error {
	r.WorkspacePath = normalizePath(r.WorkspacePath)
	if r.WorkspacePath == "" {
		return nil
	}
	_, err := s.db.Exec(`
		INSERT INTO runs (task_id, workspace_path, run_id, status, passed, started_at, finished_at, size_bytes, is_active, is_resumable, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(workspace_path) DO UPDATE SET
			task_id       = excluded.task_id,
			run_id        = excluded.run_id,
			status        = excluded.status,
			passed        = excluded.passed,
			started_at    = excluded.started_at,
			finished_at   = excluded.finished_at,
			size_bytes    = CASE WHEN excluded.size_bytes > 0 THEN excluded.size_bytes ELSE runs.size_bytes END,
			is_active     = excluded.is_active,
			is_resumable  = excluded.is_resumable
	`, r.TaskID, r.WorkspacePath, r.RunID, r.Status, r.Passed, nullTime(r.StartedAt), nullTime(r.FinishedAt), r.SizeBytes, r.IsActive, r.IsResumable, time.Now().UTC())
	return err
}

func (s *Store) GetRunByWorkspace(workspacePath string) (*Run, error) {
	workspacePath = normalizePath(workspacePath)
	if workspacePath == "" {
		return nil, nil
	}
	row := s.db.QueryRow("SELECT id, task_id, workspace_path, run_id, status, passed, started_at, finished_at, size_bytes, is_active, is_resumable, created_at FROM runs WHERE workspace_path = ?", workspacePath)
	var r Run
	var startedAt, finishedAt sql.NullTime
	err := row.Scan(&r.ID, &r.TaskID, &r.WorkspacePath, &r.RunID, &r.Status, &r.Passed, &startedAt, &finishedAt, &r.SizeBytes, &r.IsActive, &r.IsResumable, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.StartedAt = nullableTime(startedAt)
	r.FinishedAt = nullableTime(finishedAt)
	return &r, nil
}

func (s *Store) ListRuns(sortCol SortColumn, asc bool, filter string) ([]RunWithTask, error) {
	filter = sanitizeText(filter)
	order := "DESC"
	if asc {
		order = "ASC"
	}
	col := "r.started_at"
	switch sortCol {
	case SortByTaskName:
		col = "t.task_name"
	case SortByStatus:
		col = "r.status"
	case SortBySizeBytes:
		col = "r.size_bytes"
	}

	query := `
		SELECT r.id, r.task_id, r.workspace_path, r.run_id, r.status, r.passed,
		       r.started_at, r.finished_at, r.size_bytes, r.is_active, r.is_resumable, r.created_at,
		       t.id, t.task_dir, t.task_name, t.code_lang, t.task_type, t.application,
		       t.repo_url, t.commit_sha, t.is_generated, t.first_seen, t.updated_at
		FROM runs r
		JOIN tasks t ON r.task_id = t.id
	`
	var args []any
	if filter != "" {
		terms, resumable := searchTerms(filter)
		var clauses []string
		for _, term := range terms {
			clauses = append(clauses, `(t.task_name LIKE ? OR t.code_lang LIKE ? OR t.task_type LIKE ? OR t.application LIKE ? OR r.status LIKE ?)`)
			pattern := "%" + term + "%"
			args = append(args, pattern, pattern, pattern, pattern, pattern)
		}
		if resumable {
			clauses = append(clauses, "r.is_resumable = 1")
		}
		query += ` WHERE ` + strings.Join(clauses, " OR ")
	}
	query += ` ORDER BY ` + col + ` ` + order

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []RunWithTask
	for rows.Next() {
		var rwt RunWithTask
		var startedAt, finishedAt sql.NullTime
		if err := rows.Scan(
			&rwt.Run.ID, &rwt.Run.TaskID, &rwt.Run.WorkspacePath, &rwt.Run.RunID,
			&rwt.Run.Status, &rwt.Run.Passed,
			&startedAt, &finishedAt, &rwt.Run.SizeBytes,
			&rwt.Run.IsActive, &rwt.Run.IsResumable, &rwt.Run.CreatedAt,
			&rwt.Task.ID, &rwt.Task.TaskDir, &rwt.Task.TaskName,
			&rwt.Task.CodeLang, &rwt.Task.TaskType, &rwt.Task.Application,
			&rwt.Task.RepoURL, &rwt.Task.CommitSHA, &rwt.Task.IsGenerated,
			&rwt.Task.FirstSeen, &rwt.Task.UpdatedAt,
		); err != nil {
			return nil, err
		}
		rwt.Run.StartedAt = nullableTime(startedAt)
		rwt.Run.FinishedAt = nullableTime(finishedAt)
		results = append(results, rwt)
	}
	return results, rows.Err()
}

func (s *Store) SearchRuns(query string) ([]RunWithTask, error) {
	return s.ListRuns(SortByStartedAt, false, query)
}

func searchTerms(filter string) ([]string, bool) {
	terms := []string{filter}
	lower := strings.ToLower(filter)
	aliases := map[string]string{
		"成功":  "succeeded",
		"失败":  "failed",
		"运行中": "running",
	}
	for alias, status := range aliases {
		if strings.Contains(alias, filter) || strings.Contains(filter, alias) || strings.Contains(lower, status) {
			terms = append(terms, status)
		}
	}
	resumable := strings.Contains("可恢复", filter) || strings.Contains(filter, "可恢复") || strings.Contains(lower, "resumable")
	return terms, resumable
}

func (s *Store) DeleteRunByWorkspace(workspacePath string) error {
	workspacePath = normalizePath(workspacePath)
	if workspacePath == "" {
		return nil
	}
	_, err := s.db.Exec("DELETE FROM runs WHERE workspace_path = ?", workspacePath)
	return err
}

func (s *Store) ListRunsByTask(taskID int64) ([]Run, error) {
	rows, err := s.db.Query(`SELECT id, task_id, workspace_path, run_id, status, passed, started_at, finished_at, size_bytes, is_active, is_resumable, created_at FROM runs WHERE task_id = ? ORDER BY started_at DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		var run Run
		var startedAt, finishedAt sql.NullTime
		if err := rows.Scan(&run.ID, &run.TaskID, &run.WorkspacePath, &run.RunID, &run.Status, &run.Passed, &startedAt, &finishedAt, &run.SizeBytes, &run.IsActive, &run.IsResumable, &run.CreatedAt); err != nil {
			return nil, err
		}
		run.StartedAt = nullableTime(startedAt)
		run.FinishedAt = nullableTime(finishedAt)
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) runningWorkspacePaths() ([]string, error) {
	rows, err := s.db.Query(`SELECT workspace_path FROM runs WHERE status = 'running' OR is_active = 1 OR is_resumable = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

func (s *Store) CleanOrphanTasks() error {
	_, err := s.db.Exec("DELETE FROM tasks WHERE id NOT IN (SELECT DISTINCT task_id FROM runs)")
	return err
}

func (s *Store) AllWorkspacePaths() ([]string, error) {
	rows, err := s.db.Query("SELECT workspace_path FROM runs")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func nullableTime(value sql.NullTime) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time
}
