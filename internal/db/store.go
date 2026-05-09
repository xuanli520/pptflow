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

	"github.com/xuanli520/p2r_tui/internal/pathutil"
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

	LatestArtifactRoot string
	LatestStaticOnly   bool
}

type ProjectSort string

const (
	ProjectSortTaskID   ProjectSort = "task_id"
	ProjectSortStatus   ProjectSort = "status"
	ProjectSortSeverity ProjectSort = "severity"
	ProjectSortLastRun  ProjectSort = "last_run"
	ProjectSortVerdict  ProjectSort = "verdict"
)

type ProjectSearch struct {
	Terms []ProjectSearchTerm
}

type ProjectSearchTerm struct {
	Text         string
	Statuses     []string
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
       latest_static_only
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
		); err != nil {
			_ = rows.Close()
			return nil, 0, err
		}
		project.LatestStaticOnly = staticOnly == 1
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
           COALESCE(lr.static_only, 0) AS latest_static_only
    FROM projects p
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
	case ProjectSortTaskID, ProjectSortStatus, ProjectSortSeverity, ProjectSortLastRun, ProjectSortVerdict:
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
			Verdicts:     filterUnique(term.Verdicts, validManualVerdicts()),
			FailedStages: filterUnique(term.FailedStages, validFailedStages()),
		}
		if normalized.Text == "" && len(normalized.Statuses) == 0 && len(normalized.Verdicts) == 0 && len(normalized.FailedStages) == 0 {
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
