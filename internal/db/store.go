package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xuanli520/p2r_tui/internal/pathutil"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/projectlayout"
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

	LatestArtifactRoot string
	LatestStaticOnly   bool
	HasTask            bool
	TaskState          string
	CompletionCount    int
	FrontendURL        string
	DockerRunning      bool
	EnteredWaitingAt   string
	LastCompletedAt    string
	SyncError          string
}

type ProjectSort string

const (
	ProjectSortTaskID          ProjectSort = "task_id"
	ProjectSortStatus          ProjectSort = "status"
	ProjectSortSeverity        ProjectSort = "severity"
	ProjectSortLastRun         ProjectSort = "last_run"
	ProjectSortVerdict         ProjectSort = "verdict"
	ProjectSortCompletionCount ProjectSort = "completion_count"
)

type ProjectSearch struct {
	Terms []ProjectSearchTerm
}

type ProjectSearchTerm struct {
	Text         string
	Statuses     []string
	TaskStates   []string
	Verdicts     []string
	FailedStages []string
}

type ProjectQuery struct {
	Sort   ProjectSort
	Asc    bool
	Search ProjectSearch
	Limit  int
	Offset int
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

const (
	defaultBatchMaxTasks    = 20
	ActiveTaskStateLimit    = 10
	CompletedTaskStateLimit = 10
)

var ErrInspectingTaskLimit = errors.New("开始质检已达到 10 道上限")

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
	dsn := filepath.Clean(path)
	if strings.HasPrefix(path, "file:") {
		dsn = path
	} else if runtime.GOOS != "windows" {
		dsn = (&url.URL{Scheme: "file", Path: dsn}).String()
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	query := url.Values{}
	query.Add("_pragma", "busy_timeout=5000")
	query.Add("_pragma", "foreign_keys=ON")
	query.Add("_pragma", "journal_mode=WAL")
	query.Add("_pragma", "synchronous=NORMAL")
	return dsn + sep + query.Encode()
}

func configureSQLite(handle *sql.DB) error {
	for _, statement := range []string{
		`PRAGMA busy_timeout = 5000;`,
		`PRAGMA foreign_keys = ON;`,
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

func (s *Store) CreateTaskWithBatch(ctx context.Context, taskID, gitURL, scanPath string) (model.Task, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	var task model.Task
	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		exists, err := taskExistsTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("task already exists: %s", taskID)
		}
		if err := requireInspectingCapacityTx(ctx, tx); err != nil {
			return err
		}
		batch, err := selectWritableBatch(ctx, tx, now)
		if err != nil {
			return err
		}
		repoPath := projectlayout.ExpectedProjectPath(scanPath, batch.ID, taskID)
		_, err = tx.ExecContext(ctx, `INSERT INTO tasks(id, batch_id, git_url, repo_path, state, completion_count, frontend_url, docker_running, compose_meta, entered_waiting_at, last_completed_at, sync_error, created_at, updated_at)
			VALUES(?, ?, ?, ?, ?, 0, '', 0, '', '', '', '', ?, ?)
			`,
			taskID, batch.ID, gitURL, repoPath, model.TaskInspecting, now, now)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO projects(task_id, batch, path)
			VALUES(?, ?, ?)
			ON CONFLICT(task_id) DO UPDATE SET batch=excluded.batch, path=excluded.path`,
			taskID, batch.ID, repoPath); err != nil {
			return err
		}
		task, err = getTaskTx(ctx, tx, taskID)
		return err
	})
	return task, err
}

func (s *Store) GetTask(ctx context.Context, taskID string) (model.Task, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.Task{}, err
	}
	defer tx.Rollback()
	task, err := getTaskTx(ctx, tx, taskID)
	if err != nil {
		return model.Task{}, err
	}
	return task, tx.Commit()
}

func (s *Store) ListTasksByState(ctx context.Context, state string) ([]model.Task, error) {
	rows, err := s.db.QueryContext(ctx, taskSelectSQL()+` WHERE state = ? AND COALESCE(archived_at, '') = '' ORDER BY updated_at DESC, id ASC`, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

func (s *Store) CountTasksByState(ctx context.Context, state string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE state = ? AND COALESCE(archived_at, '') = ''`, state).Scan(&count)
	return count, err
}

func (s *Store) ListTasksWithDockerRunning(ctx context.Context) ([]model.Task, error) {
	rows, err := s.db.QueryContext(ctx, taskSelectSQL()+` WHERE docker_running = 1 ORDER BY updated_at DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

func (s *Store) RecordTaskGitError(ctx context.Context, taskID string, syncErr error) error {
	message := ""
	if syncErr != nil {
		message = syncErr.Error()
	}
	now := time.Now().UTC().Format(time.RFC3339)
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE tasks SET sync_error = ?, updated_at = ? WHERE id = ?`, message, now, taskID)
		if err != nil {
			return err
		}
		return requireAffected(result, "task", taskID)
	})
}

func (s *Store) UpdateTaskGitURL(ctx context.Context, taskID string, gitURL string) (model.Task, error) {
	gitURL = strings.TrimSpace(gitURL)
	if gitURL == "" {
		return model.Task{}, errors.New("git url is required")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var task model.Task
	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE tasks SET git_url = ?, updated_at = ? WHERE id = ?`, gitURL, now, taskID)
		if err != nil {
			return err
		}
		if err := requireAffected(result, "task", taskID); err != nil {
			return err
		}
		task, err = getTaskTx(ctx, tx, taskID)
		return err
	})
	return task, err
}

func (s *Store) ReopenTaskForInspection(ctx context.Context, taskID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		if err := requireInspectingCapacityTx(ctx, tx); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE tasks
			SET state = ?, current_run_id = NULL, frontend_url = '', docker_running = 0, compose_meta = '', entered_waiting_at = '', archived_at = '', sync_error = '', updated_at = ?
			WHERE id = ? AND state = ? AND current_run_id IS NULL`,
			model.TaskInspecting, now, taskID, model.TaskCompleted)
		if err != nil {
			return err
		}
		return requireAffected(result, "completed task", taskID)
	})
}

func (s *Store) RecordTaskRuntime(ctx context.Context, taskID string, frontendURL string, dockerRunning bool, meta model.ComposeMeta) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		return recordTaskRuntimeTx(ctx, tx, taskID, frontendURL, dockerRunning, meta, now)
	})
}

func (s *Store) MarkTaskDockerStopped(ctx context.Context, taskID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE tasks SET docker_running = 0, updated_at = ? WHERE id = ?`, now, taskID)
		if err != nil {
			return err
		}
		return requireAffected(result, "task", taskID)
	})
}

func (s *Store) CompleteTask(ctx context.Context, taskID string) (model.Task, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	var task model.Task
	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE tasks
			SET state = ?, current_run_id = NULL, docker_running = 0, completion_count = completion_count + 1,
			    last_completed_at = ?, updated_at = ?
			WHERE id = ? AND state = ?`,
			model.TaskCompleted, now, now, taskID, model.TaskWaitingManual)
		if err != nil {
			return err
		}
		if err := requireAffected(result, "waiting task", taskID); err != nil {
			return err
		}
		if err := archiveCompletedOverflowTx(ctx, tx, now); err != nil {
			return err
		}
		task, err = getTaskTx(ctx, tx, taskID)
		return err
	})
	return task, err
}

func (s *Store) RepairTaskStates(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE tasks
			SET state = ?, current_run_id = NULL, entered_waiting_at = CASE WHEN entered_waiting_at = '' THEN ? ELSE entered_waiting_at END, updated_at = ?
			WHERE state = ? AND current_run_id IN (SELECT run_id FROM runs WHERE status IN (?, ?))`,
			model.TaskWaitingManual, now, now, model.TaskInspecting, model.RunCompletedClean, model.RunCompletedWithFindings)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE tasks
			SET state = ?, current_run_id = NULL, docker_running = 0, frontend_url = '', compose_meta = '', updated_at = ?
			WHERE state = ? AND current_run_id IN (SELECT run_id FROM runs WHERE status IN (?, ?))`,
			model.TaskCompleted, now, model.TaskInspecting, model.RunAborted, model.RunCrashed)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE tasks
			SET state = ?, docker_running = 0, frontend_url = '', compose_meta = '', updated_at = ?
			WHERE state = ? AND current_run_id IS NULL AND id IN (
				SELECT task_id
				FROM (
					SELECT r.task_id,
					       r.status,
					       ROW_NUMBER() OVER (
					           PARTITION BY r.task_id
					           ORDER BY COALESCE(r.started_at, '') DESC, r.run_id DESC
					       ) AS rn
					FROM runs r
				) latest
				WHERE rn = 1 AND status IN (?, ?)
			)`,
			model.TaskCompleted, now, model.TaskInspecting, model.RunAborted, model.RunCrashed)
		if err != nil {
			return err
		}
		if err := archiveCompletedOverflowTx(ctx, tx, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE tasks
			SET current_run_id = NULL, updated_at = ?
			WHERE current_run_id IS NOT NULL AND current_run_id NOT IN (SELECT run_id FROM runs)`, now)
		return err
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
	projects, _, err := s.listProjectSummaries(ctx, ProjectQuery{
		Sort: ProjectSortTaskID,
		Asc:  true,
	}, false)
	return projects, err
}

func (s *Store) ListProjectsPaginated(ctx context.Context, q ProjectQuery) ([]ProjectSummary, int, error) {
	return s.listProjectSummaries(ctx, q, true)
}

func (s *Store) listProjectSummaries(ctx context.Context, q ProjectQuery, paginated bool) ([]ProjectSummary, int, error) {
	q = normalizeProjectQuery(q, paginated)
	where := projectSearchPredicate(q.Search)

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	total := 0
	if paginated {
		countSQL := baseProjectRowsSQL(where) + `SELECT COUNT(*) FROM project_rows`
		if err := tx.QueryRowContext(ctx, countSQL, where.Args...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}

	selectSQL := baseProjectRowsSQL(where) + `SELECT task_id,
       batch,
       path,
       run_count,
       last_run_id,
       last_run_at,
       run_status,
       manual_verdict,
       failed_stage,
       blocking,
       high,
       latest_artifact_root,
       latest_static_only,
       has_task,
       task_state,
       completion_count,
       frontend_url,
       docker_running,
       entered_waiting_at,
       last_completed_at,
       sync_error
FROM project_rows
` + projectOrderClause(q)
	args := append([]any{}, where.Args...)
	if paginated {
		selectSQL += `
LIMIT ? OFFSET ?`
		args = append(args, q.Limit, q.Offset)
	}

	rows, err := tx.QueryContext(ctx, selectSQL, args...)
	if err != nil {
		return nil, 0, err
	}
	var projects []ProjectSummary
	for rows.Next() {
		var project ProjectSummary
		var staticOnly int
		var hasTask int
		var dockerRunning int
		if err := rows.Scan(
			&project.TaskID,
			&project.Batch,
			&project.Path,
			&project.RunCount,
			&project.LastRunID,
			&project.LastRunAt,
			&project.RunStatus,
			&project.ManualVerdict,
			&project.FailedStage,
			&project.Blocking,
			&project.High,
			&project.LatestArtifactRoot,
			&staticOnly,
			&hasTask,
			&project.TaskState,
			&project.CompletionCount,
			&project.FrontendURL,
			&dockerRunning,
			&project.EnteredWaitingAt,
			&project.LastCompletedAt,
			&project.SyncError,
		); err != nil {
			_ = rows.Close()
			return nil, 0, err
		}
		project.LatestStaticOnly = staticOnly == 1
		project.HasTask = hasTask == 1
		project.DockerRunning = dockerRunning == 1
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, 0, err
	}
	if err := rows.Close(); err != nil {
		return nil, 0, err
	}
	if !paginated {
		total = len(projects)
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	committed = true
	return projects, total, nil
}

type projectWhere struct {
	SQL  string
	Args []any
}

func allProjectRowsWhere() projectWhere {
	return projectWhere{SQL: "1=1"}
}

func (w projectWhere) sqlOrDefault() string {
	if strings.TrimSpace(w.SQL) == "" {
		return "1=1"
	}
	return w.SQL
}

func baseProjectRowsSQL(where projectWhere) string {
	return fmt.Sprintf(`WITH latest_run AS (
    SELECT *
    FROM (
        SELECT r.*,
               ROW_NUMBER() OVER (
                   PARTITION BY r.task_id
                   ORDER BY COALESCE(r.started_at, '') DESC, r.run_id DESC
               ) AS rn
        FROM runs r
    ) ranked_runs
    WHERE rn = 1
),
failed_stage AS (
    SELECT run_id, stage
    FROM (
        SELECT s.run_id,
               s.stage,
               ROW_NUMBER() OVER (
                   PARTITION BY s.run_id
                   ORDER BY %s, s.stage
               ) AS rn
        FROM run_stages s
        WHERE s.status IN ('failed', 'blocked')
    ) ranked_stages
    WHERE rn = 1
),
finding_counts AS (
    SELECT run_id,
           SUM(CASE WHEN severity = 'Blocker' THEN 1 ELSE 0 END) AS blocking,
           SUM(CASE WHEN severity = 'High' THEN 1 ELSE 0 END) AS high
    FROM findings
    GROUP BY run_id
),
project_rows AS (
    SELECT p.task_id,
           p.batch,
           p.path,
           p.run_count,
           COALESCE(lr.run_id, '') AS last_run_id,
           COALESCE(NULLIF(lr.finished_at, ''), NULLIF(lr.started_at, ''), NULLIF(p.last_run_at, ''), '') AS last_run_at,
           COALESCE(lr.status, '') AS run_status,
           COALESCE(NULLIF(lr.manual_verdict, ''), 'unset') AS manual_verdict,
           CASE COALESCE(lr.status, '')
               WHEN 'running' THEN 50
               WHEN 'crashed' THEN 40
               WHEN 'completed_with_findings' THEN 30
               WHEN 'aborted' THEN 20
               WHEN 'completed_clean' THEN 10
               ELSE 0
           END AS status_rank,
           CASE COALESCE(NULLIF(lr.manual_verdict, ''), 'unset')
               WHEN 'fail' THEN 40
               WHEN 'rework' THEN 30
               WHEN 'unset' THEN 20
               WHEN 'pass' THEN 10
               ELSE 0
           END AS verdict_rank,
           COALESCE(fs.stage, '') AS failed_stage,
           COALESCE(fc.blocking, 0) AS blocking,
           COALESCE(fc.high, 0) AS high,
           COALESCE(lr.artifact_root, '') AS latest_artifact_root,
           COALESCE(lr.static_only, 0) AS latest_static_only,
           CASE WHEN t.id IS NULL THEN 0 ELSE 1 END AS has_task,
           COALESCE(t.state, '') AS task_state,
           COALESCE(t.completion_count, 0) AS completion_count,
           COALESCE(t.frontend_url, '') AS frontend_url,
           COALESCE(t.docker_running, 0) AS docker_running,
           COALESCE(t.entered_waiting_at, '') AS entered_waiting_at,
           COALESCE(t.last_completed_at, '') AS last_completed_at,
           COALESCE(t.sync_error, '') AS sync_error
    FROM projects p
    LEFT JOIN tasks t ON t.id = p.task_id
    LEFT JOIN latest_run lr ON lr.task_id = p.task_id
    LEFT JOIN failed_stage fs ON fs.run_id = lr.run_id
    LEFT JOIN finding_counts fc ON fc.run_id = lr.run_id
    WHERE %s
)
`, stageOrderCaseSQL("s.stage"), where.sqlOrDefault())
}

func normalizeProjectQuery(q ProjectQuery, paginated bool) ProjectQuery {
	if !validProjectSort(q.Sort) {
		q.Sort = ProjectSortTaskID
		q.Asc = true
	}
	if paginated {
		q.Limit = normalizeProjectLimit(q.Limit)
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	q.Search = normalizeProjectSearch(q.Search)
	return q
}

func validProjectSort(sort ProjectSort) bool {
	switch sort {
	case ProjectSortTaskID, ProjectSortStatus, ProjectSortSeverity, ProjectSortLastRun, ProjectSortVerdict, ProjectSortCompletionCount:
		return true
	default:
		return false
	}
}

func normalizeProjectLimit(limit int) int {
	switch limit {
	case 10, 20, 40, 50:
		return limit
	default:
		return 20
	}
}

func normalizeProjectSearch(search ProjectSearch) ProjectSearch {
	capacity := len(search.Terms)
	if capacity > 8 {
		capacity = 8
	}
	terms := make([]ProjectSearchTerm, 0, capacity)
	for _, term := range search.Terms {
		if len(terms) >= 8 {
			break
		}
		normalized := ProjectSearchTerm{
			Text:         limitSearchText(strings.TrimSpace(term.Text), 64),
			Statuses:     filterUnique(term.Statuses, validRunStatuses()),
			TaskStates:   filterUnique(term.TaskStates, validTaskStates()),
			Verdicts:     filterUnique(term.Verdicts, validManualVerdicts()),
			FailedStages: filterUnique(term.FailedStages, validFailedStages()),
		}
		if normalized.Text == "" && len(normalized.Statuses) == 0 && len(normalized.TaskStates) == 0 && len(normalized.Verdicts) == 0 && len(normalized.FailedStages) == 0 {
			continue
		}
		terms = append(terms, normalized)
	}
	return ProjectSearch{Terms: terms}
}

func limitSearchText(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes])
}

func filterUnique(values []string, allowed map[string]bool) []string {
	var result []string
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || !allowed[value] || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func validRunStatuses() map[string]bool {
	return map[string]bool{
		model.RunRunning:               true,
		model.RunCrashed:               true,
		model.RunCompletedWithFindings: true,
		model.RunAborted:               true,
		model.RunCompletedClean:        true,
	}
}

func validTaskStates() map[string]bool {
	return map[string]bool{
		model.TaskInspecting:    true,
		model.TaskWaitingManual: true,
		model.TaskCompleted:     true,
	}
}

func validManualVerdicts() map[string]bool {
	return map[string]bool{
		model.ManualUnset:  true,
		model.ManualPass:   true,
		model.ManualRework: true,
		model.ManualFail:   true,
	}
}

func validFailedStages() map[string]bool {
	result := map[string]bool{}
	for _, stage := range model.AllStages() {
		result[stage] = true
	}
	return result
}

func stageOrderCaseSQL(column string) string {
	var builder strings.Builder
	builder.WriteString("CASE ")
	builder.WriteString(column)
	for _, spec := range model.AllStageSpecs() {
		builder.WriteString(fmt.Sprintf(" WHEN '%s' THEN %d", spec.ID, spec.Order))
	}
	builder.WriteString(" ELSE 99 END")
	return builder.String()
}

func projectSearchPredicate(search ProjectSearch) projectWhere {
	if len(search.Terms) == 0 {
		return allProjectRowsWhere()
	}
	var termPredicates []string
	var args []any
	for _, term := range search.Terms {
		predicate := projectSearchTermPredicate(term)
		if predicate.SQL == "" {
			continue
		}
		termPredicates = append(termPredicates, predicate.SQL)
		args = append(args, predicate.Args...)
	}
	if len(termPredicates) == 0 {
		return allProjectRowsWhere()
	}
	return projectWhere{SQL: strings.Join(termPredicates, " AND "), Args: args}
}

func projectSearchTermPredicate(term ProjectSearchTerm) projectWhere {
	var clauses []string
	var args []any
	if term.Text != "" {
		pattern := likePattern(term.Text)
		for _, expression := range []string{
			"p.task_id",
			"p.batch",
			"p.path",
			"COALESCE(lr.status, '')",
			"COALESCE(t.state, '')",
			"COALESCE(NULLIF(lr.manual_verdict, ''), 'unset')",
			"COALESCE(fs.stage, '')",
		} {
			clauses = append(clauses, expression+` LIKE ? ESCAPE '\'`)
			args = append(args, pattern)
		}
	}
	if len(term.Statuses) > 0 {
		clauses = append(clauses, "COALESCE(lr.status, '') IN ("+placeholders(len(term.Statuses))+")")
		for _, status := range term.Statuses {
			args = append(args, status)
		}
	}
	if len(term.TaskStates) > 0 {
		clauses = append(clauses, "COALESCE(t.state, '') IN ("+placeholders(len(term.TaskStates))+")")
		for _, state := range term.TaskStates {
			args = append(args, state)
		}
	}
	if len(term.Verdicts) > 0 {
		clauses = append(clauses, "COALESCE(NULLIF(lr.manual_verdict, ''), 'unset') IN ("+placeholders(len(term.Verdicts))+")")
		for _, verdict := range term.Verdicts {
			args = append(args, verdict)
		}
	}
	if len(term.FailedStages) > 0 {
		clauses = append(clauses, "COALESCE(fs.stage, '') IN ("+placeholders(len(term.FailedStages))+")")
		for _, stage := range term.FailedStages {
			args = append(args, stage)
		}
	}
	if len(clauses) == 0 {
		return projectWhere{}
	}
	return projectWhere{SQL: "(" + strings.Join(clauses, " OR ") + ")", Args: args}
}

func likePattern(value string) string {
	var builder strings.Builder
	builder.WriteByte('%')
	for _, r := range value {
		switch r {
		case '%', '_', '\\':
			builder.WriteByte('\\')
		}
		builder.WriteRune(r)
	}
	builder.WriteByte('%')
	return builder.String()
}

func placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", count), ",")
}

func projectOrderClause(q ProjectQuery) string {
	dir := "ASC"
	if !q.Asc {
		dir = "DESC"
	}

	switch q.Sort {
	case ProjectSortStatus:
		return fmt.Sprintf("ORDER BY status_rank %s, task_id ASC", dir)
	case ProjectSortSeverity:
		return fmt.Sprintf("ORDER BY blocking %s, high %s, task_id ASC", dir, dir)
	case ProjectSortLastRun:
		return fmt.Sprintf("ORDER BY last_run_at %s, task_id ASC", dir)
	case ProjectSortVerdict:
		return fmt.Sprintf("ORDER BY verdict_rank %s, task_id ASC", dir)
	case ProjectSortCompletionCount:
		return fmt.Sprintf("ORDER BY completion_count %s, task_id ASC", dir)
	default:
		return fmt.Sprintf("ORDER BY task_id %s", dir)
	}
}

func (s *Store) CreateRun(ctx context.Context, run model.RunRecord) error {
	staticOnly := 0
	if run.StaticOnly {
		staticOnly = 1
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		round := 1
		taskExists, err := taskExistsTx(ctx, tx, run.TaskID)
		if err != nil {
			return err
		}
		if taskExists {
			var state string
			if err := tx.QueryRowContext(ctx, `SELECT state, completion_count + 1 FROM tasks WHERE id = ?`, run.TaskID).Scan(&state, &round); err != nil {
				return err
			}
			if state != model.TaskInspecting {
				if err := requireInspectingCapacityTx(ctx, tx); err != nil {
					return err
				}
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO runs(run_id, task_id, started_at, status, manual_verdict, static_only, duration_ms, artifact_root, tool_versions, prompt_versions, completion_round)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			run.RunID, run.TaskID, run.StartedAt, run.Status, run.ManualVerdict, staticOnly, run.DurationMS, run.ArtifactRoot, run.ToolVersions, run.PromptVersions, round)
		if err != nil {
			return err
		}
		startedAt := firstNonEmptyString(run.StartedAt, time.Now().UTC().Format(time.RFC3339))
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET last_run_id = ?, last_run_at = ? WHERE task_id = ?`, run.RunID, startedAt, run.TaskID); err != nil {
			return err
		}
		if taskExists {
			now := time.Now().UTC().Format(time.RFC3339)
			result, err := tx.ExecContext(ctx, `UPDATE tasks
				SET current_run_id = ?, state = ?, archived_at = '', sync_error = '', updated_at = ?
				WHERE id = ? AND current_run_id IS NULL`,
				run.RunID, model.TaskInspecting, now, run.TaskID)
			if err != nil {
				return err
			}
			if err := requireAffected(result, "idle task", run.TaskID); err != nil {
				return err
			}
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
		taskExists, err := taskExistsTx(ctx, tx, taskID)
		if err != nil || !taskExists {
			return err
		}
		var result sql.Result
		switch status {
		case model.RunCompletedClean, model.RunCompletedWithFindings:
			result, err = tx.ExecContext(ctx, `UPDATE tasks
				SET state = ?, current_run_id = NULL, entered_waiting_at = CASE WHEN entered_waiting_at = '' THEN ? ELSE entered_waiting_at END, updated_at = ?
				WHERE id = ? AND (current_run_id = ? OR current_run_id IS NULL)`,
				model.TaskWaitingManual, now, now, taskID, runID)
			if err != nil {
				return err
			}
			err = requireAffected(result, "active task", taskID)
		case model.RunAborted, model.RunCrashed:
			result, err = tx.ExecContext(ctx, `UPDATE tasks
				SET state = ?, current_run_id = NULL, docker_running = 0, frontend_url = '', compose_meta = '', updated_at = ?
				WHERE id = ? AND (current_run_id = ? OR current_run_id IS NULL)`,
				model.TaskCompleted, now, taskID, runID)
			if err != nil {
				return err
			}
			err = requireAffected(result, "active task", taskID)
		}
		if err != nil {
			return err
		}
		if status == model.RunAborted || status == model.RunCrashed {
			return archiveCompletedOverflowTx(ctx, tx, now)
		}
		return err
	})
}

func (s *Store) FinishRunAndTransitionTask(ctx context.Context, runID, taskID, status string, duration time.Duration) error {
	return s.FinishRun(ctx, runID, taskID, status, duration)
}

func (s *Store) SetLatestRunManualVerdict(ctx context.Context, taskID, verdict string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE runs
			SET manual_verdict = ?
			WHERE run_id = (
				SELECT run_id FROM runs
				WHERE task_id = ?
				ORDER BY COALESCE(started_at, '') DESC, run_id DESC
				LIMIT 1
			)`, verdict, taskID)
		if err != nil {
			return err
		}
		if err := requireAffected(result, "latest run", taskID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE id = ?`, now, taskID)
		return err
	})
}

func (s *Store) GetRun(ctx context.Context, runID string) (model.RunRecord, error) {
	var run model.RunRecord
	var staticOnly int
	row := s.db.QueryRowContext(ctx, `SELECT run_id, task_id, COALESCE(started_at,''), COALESCE(finished_at,''), status, manual_verdict, static_only, duration_ms, artifact_root, COALESCE(tool_versions,''), COALESCE(prompt_versions,''), completion_round FROM runs WHERE run_id = ?`, runID)
	err := row.Scan(&run.RunID, &run.TaskID, &run.StartedAt, &run.FinishedAt, &run.Status, &run.ManualVerdict, &staticOnly, &run.DurationMS, &run.ArtifactRoot, &run.ToolVersions, &run.PromptVersions, &run.CompletionRound)
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
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, task_id, COALESCE(started_at,''), COALESCE(finished_at,''), status, manual_verdict, static_only, duration_ms, artifact_root, COALESCE(tool_versions,''), COALESCE(prompt_versions,''), completion_round FROM runs WHERE task_id = ? ORDER BY started_at DESC, run_id DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []model.RunRecord
	for rows.Next() {
		var run model.RunRecord
		var staticOnly int
		if err := rows.Scan(&run.RunID, &run.TaskID, &run.StartedAt, &run.FinishedAt, &run.Status, &run.ManualVerdict, &staticOnly, &run.DurationMS, &run.ArtifactRoot, &run.ToolVersions, &run.PromptVersions, &run.CompletionRound); err != nil {
			return nil, err
		}
		run.StaticOnly = staticOnly == 1
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) RunningRuns(ctx context.Context) ([]model.RunRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, task_id, COALESCE(started_at,''), COALESCE(finished_at,''), status, manual_verdict, static_only, duration_ms, artifact_root, COALESCE(tool_versions,''), COALESCE(prompt_versions,''), completion_round FROM runs WHERE status = ? ORDER BY started_at DESC, run_id DESC`, model.RunRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []model.RunRecord
	for rows.Next() {
		var run model.RunRecord
		var staticOnly int
		if err := rows.Scan(&run.RunID, &run.TaskID, &run.StartedAt, &run.FinishedAt, &run.Status, &run.ManualVerdict, &staticOnly, &run.DurationMS, &run.ArtifactRoot, &run.ToolVersions, &run.PromptVersions, &run.CompletionRound); err != nil {
			return nil, err
		}
		run.StaticOnly = staticOnly == 1
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) PutStage(ctx context.Context, runID string, stage model.StageRecord) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		return putStageTx(ctx, tx, runID, stage)
	})
}

func (s *Store) PutStageAndRecordTaskRuntime(ctx context.Context, runID string, stage model.StageRecord, taskID string, frontendURL string, dockerRunning bool, meta model.ComposeMeta) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		if err := putStageTx(ctx, tx, runID, stage); err != nil {
			return err
		}
		return recordTaskRuntimeTx(ctx, tx, taskID, frontendURL, dockerRunning, meta, now)
	})
}

func putStageTx(ctx context.Context, tx *sql.Tx, runID string, stage model.StageRecord) error {
	blockedBy, _ := json.Marshal(stage.BlockedBy)
	artifacts, _ := json.Marshal(stage.ArtifactPaths)
	name := stage.Name
	if name == "" {
		name = model.StageDisplayName(stage.Stage)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO run_stages(run_id, stage, name, status, started_at, finished_at, duration_ms, blocked_by, log_path, artifact_json, error_summary)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, stage) DO UPDATE SET name=excluded.name, status=excluded.status, started_at=excluded.started_at, finished_at=excluded.finished_at, duration_ms=excluded.duration_ms, blocked_by=excluded.blocked_by, log_path=excluded.log_path, artifact_json=excluded.artifact_json, error_summary=excluded.error_summary`,
		runID, stage.Stage, name, stage.Status, stage.StartedAt, stage.FinishedAt, stage.DurationMS, string(blockedBy), stage.LogPath, string(artifacts), stage.ErrorSummary)
	return err
}

func recordTaskRuntimeTx(ctx context.Context, tx *sql.Tx, taskID string, frontendURL string, dockerRunning bool, meta model.ComposeMeta, now string) error {
	composeMeta, err := marshalComposeMeta(meta)
	if err != nil {
		return err
	}
	dockerFlag := 0
	if dockerRunning {
		dockerFlag = 1
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks
		SET frontend_url = ?, docker_running = ?, compose_meta = ?, updated_at = ?
		WHERE id = ?`,
		frontendURL, dockerFlag, composeMeta, now, taskID)
	if err != nil {
		return err
	}
	return requireAffected(result, "task", taskID)
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
	query := fmt.Sprintf(`SELECT stage, COALESCE(name,''), status, COALESCE(started_at,''), COALESCE(finished_at,''), duration_ms, COALESCE(blocked_by,'[]'), COALESCE(log_path,''), COALESCE(artifact_json,'[]'), COALESCE(error_summary,'') FROM run_stages WHERE run_id = ? ORDER BY %s, stage`, stageOrderCaseSQL("stage"))
	rows, err := s.db.QueryContext(ctx, query, runID)
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
	query := fmt.Sprintf(`SELECT COALESCE(stage, '') FROM run_stages WHERE run_id = ? AND status IN ('failed', 'blocked') ORDER BY %s, stage LIMIT 1`, stageOrderCaseSQL("stage"))
	row := s.db.QueryRowContext(ctx, query, runID)
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

func selectWritableBatch(ctx context.Context, tx *sql.Tx, now string) (model.Batch, error) {
	rows, err := tx.QueryContext(ctx, `SELECT b.id, b.display_name, COALESCE(NULLIF(b.max_tasks, 0), ?), b.created_at,
	       COUNT(t.id) AS task_count
	FROM batches b
	LEFT JOIN tasks t ON t.batch_id = b.id
	GROUP BY b.id, b.display_name, b.max_tasks, b.created_at
	ORDER BY CAST(SUBSTR(b.id, 7) AS INTEGER), b.id`, defaultBatchMaxTasks)
	if err != nil {
		return model.Batch{}, err
	}
	var batches []model.Batch
	for rows.Next() {
		var batch model.Batch
		if err := rows.Scan(&batch.ID, &batch.DisplayName, &batch.MaxTasks, &batch.CreatedAt, &batch.TaskCount); err != nil {
			_ = rows.Close()
			return model.Batch{}, err
		}
		batch.IsFull = batch.TaskCount >= batch.MaxTasks
		batches = append(batches, batch)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return model.Batch{}, err
	}
	if err := rows.Close(); err != nil {
		return model.Batch{}, err
	}
	for _, batch := range batches {
		if batch.TaskCount < batch.MaxTasks {
			return batch, nil
		}
	}
	next := nextBatchID(batches)
	batch := model.Batch{
		ID:          next,
		DisplayName: next,
		MaxTasks:    defaultBatchMaxTasks,
		CreatedAt:   now,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO batches(id, display_name, task_count, max_tasks, created_at, is_full)
		VALUES(?, ?, 0, ?, ?, 0)`, batch.ID, batch.DisplayName, batch.MaxTasks, batch.CreatedAt); err != nil {
		return model.Batch{}, err
	}
	return batch, nil
}

func nextBatchID(batches []model.Batch) string {
	maxID := 0
	for _, batch := range batches {
		suffix := strings.TrimPrefix(strings.TrimSpace(batch.ID), "batch-")
		number, err := strconv.Atoi(suffix)
		if err == nil && number > maxID {
			maxID = number
		}
	}
	return fmt.Sprintf("batch-%03d", maxID+1)
}

func taskSelectSQL() string {
	return `SELECT id, batch_id, git_url, repo_path, state, COALESCE(current_run_id, ''), completion_count,
	       COALESCE(frontend_url, ''), docker_running, COALESCE(compose_meta, ''), COALESCE(entered_waiting_at, ''),
	       COALESCE(last_completed_at, ''), COALESCE(archived_at, ''), COALESCE(sync_error, ''), created_at, updated_at
	FROM tasks`
}

func getTaskTx(ctx context.Context, tx *sql.Tx, taskID string) (model.Task, error) {
	row := tx.QueryRowContext(ctx, taskSelectSQL()+` WHERE id = ?`, taskID)
	return scanTask(row)
}

type taskScanner interface {
	Scan(dest ...any) error
}

func scanTask(row taskScanner) (model.Task, error) {
	var task model.Task
	var dockerRunning int
	var composeMeta string
	err := row.Scan(
		&task.ID,
		&task.BatchID,
		&task.GitURL,
		&task.RepoPath,
		&task.State,
		&task.CurrentRunID,
		&task.CompletionCount,
		&task.FrontendURL,
		&dockerRunning,
		&composeMeta,
		&task.EnteredWaitingAt,
		&task.LastCompletedAt,
		&task.ArchivedAt,
		&task.SyncError,
		&task.CreatedAt,
		&task.UpdatedAt,
	)
	if err != nil {
		return model.Task{}, err
	}
	task.DockerRunning = dockerRunning == 1
	if strings.TrimSpace(composeMeta) != "" {
		_ = json.Unmarshal([]byte(composeMeta), &task.ComposeMeta)
	}
	return task, nil
}

func scanTasks(rows *sql.Rows) ([]model.Task, error) {
	var tasks []model.Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func taskExistsTx(ctx context.Context, tx *sql.Tx, taskID string) (bool, error) {
	var found string
	err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id = ?`, taskID).Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func countTasksByStateTx(ctx context.Context, tx *sql.Tx, state string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE state = ? AND COALESCE(archived_at, '') = ''`, state).Scan(&count)
	return count, err
}

func requireInspectingCapacityTx(ctx context.Context, tx *sql.Tx) error {
	count, err := countTasksByStateTx(ctx, tx, model.TaskInspecting)
	if err != nil {
		return err
	}
	if count >= ActiveTaskStateLimit {
		return ErrInspectingTaskLimit
	}
	return nil
}

func archiveCompletedOverflowTx(ctx context.Context, tx *sql.Tx, now string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM tasks
		WHERE state = ? AND COALESCE(archived_at, '') = ''
		ORDER BY COALESCE(NULLIF(last_completed_at, ''), updated_at, created_at) ASC, id ASC
		LIMIT (
			SELECT MAX(COUNT(*) - ?, 0)
			FROM tasks
			WHERE state = ? AND COALESCE(archived_at, '') = ''
		)`, model.TaskCompleted, CompletedTaskStateLimit, model.TaskCompleted)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET archived_at = ?, updated_at = ? WHERE id = ?`, now, now, id); err != nil {
			return err
		}
	}
	return nil
}

func marshalComposeMeta(meta model.ComposeMeta) (string, error) {
	if strings.TrimSpace(meta.Project) == "" && len(meta.ComposeFiles) == 0 && len(meta.EnvFiles) == 0 && strings.TrimSpace(meta.WorkDir) == "" && len(meta.Ports) == 0 {
		return "", nil
	}
	content, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func requireAffected(result sql.Result, kind, id string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return FormatNotFound(kind, id)
	}
	return nil
}

func terminalRunStatusArgs() []any {
	statuses := terminalRunStatuses()
	args := make([]any, 0, len(statuses))
	for _, status := range statuses {
		args = append(args, status)
	}
	return args
}

func terminalRunStatuses() []string {
	return []string{
		model.RunCompletedClean,
		model.RunCompletedWithFindings,
		model.RunAborted,
		model.RunCrashed,
	}
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
	rel, ok := pathutil.RelUnderRoot(scanRoot, projectPath)
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

func splitPathParts(path string) []string {
	path = filepath.Clean(path)
	if path == "." {
		return nil
	}
	return strings.Split(path, string(filepath.Separator))
}
