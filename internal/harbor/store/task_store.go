package store

import (
	"database/sql"
	"strings"
	"time"
)

// Task is a record from the tasks table.
type Task struct {
	ID          int64
	TaskDir     string
	TaskName    string
	CodeLang    string
	TaskType    string
	Application string
	RepoURL     string
	CommitSHA   string
	IsGenerated bool
	FirstSeen   time.Time
	UpdatedAt   time.Time
}

func (s *Store) UpsertTask(t Task) (int64, error) {
	t.TaskDir = normalizePath(t.TaskDir)
	if t.TaskDir == "" {
		return 0, nil
	}
	now := time.Now().UTC()
	var existingID int64
	err := s.db.QueryRow("SELECT id FROM tasks WHERE task_dir = ?", t.TaskDir).Scan(&existingID)
	if err != nil {
		existingID = 0
	}
	res, err := s.db.Exec(`
		INSERT INTO tasks (task_dir, task_name, code_lang, task_type, application, repo_url, commit_sha, is_generated, first_seen, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_dir) DO UPDATE SET
			task_name   = COALESCE(NULLIF(excluded.task_name, ''), tasks.task_name),
			code_lang   = COALESCE(NULLIF(excluded.code_lang, ''), tasks.code_lang),
			task_type   = COALESCE(NULLIF(excluded.task_type, ''), tasks.task_type),
			application = COALESCE(NULLIF(excluded.application, ''), tasks.application),
			repo_url    = COALESCE(NULLIF(excluded.repo_url, ''), tasks.repo_url),
			commit_sha  = COALESCE(NULLIF(excluded.commit_sha, ''), tasks.commit_sha),
			is_generated = excluded.is_generated,
			updated_at  = excluded.updated_at
	`, t.TaskDir, t.TaskName, t.CodeLang, t.TaskType, t.Application, t.RepoURL, t.CommitSHA, t.IsGenerated, now, now)
	if err != nil {
		return existingID, err
	}
	if existingID != 0 {
		return existingID, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) GetTaskByDir(taskDir string) (*Task, error) {
	taskDir = normalizePath(taskDir)
	if taskDir == "" {
		return nil, nil
	}
	row := s.db.QueryRow("SELECT id, task_dir, task_name, code_lang, task_type, application, repo_url, commit_sha, is_generated, first_seen, updated_at FROM tasks WHERE task_dir = ?", taskDir)
	var t Task
	err := row.Scan(&t.ID, &t.TaskDir, &t.TaskName, &t.CodeLang, &t.TaskType, &t.Application, &t.RepoURL, &t.CommitSHA, &t.IsGenerated, &t.FirstSeen, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) DeleteTask(id int64) error {
	_, err := s.db.Exec("DELETE FROM tasks WHERE id = ?", id)
	return err
}

func (s *Store) ListTasks(filter string) ([]Task, error) {
	query := `SELECT id, task_dir, task_name, code_lang, task_type, application, repo_url, commit_sha, is_generated, first_seen, updated_at FROM tasks`
	var args []any
	if filter = sanitizeText(filter); filter != "" {
		query += ` WHERE task_name LIKE ? OR code_lang LIKE ? OR task_type LIKE ? OR application LIKE ? OR repo_url LIKE ?`
		pattern := "%" + filter + "%"
		args = append(args, pattern, pattern, pattern, pattern, pattern)
	}
	query += ` ORDER BY updated_at DESC, task_name ASC`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		var task Task
		if err := rows.Scan(&task.ID, &task.TaskDir, &task.TaskName, &task.CodeLang, &task.TaskType, &task.Application, &task.RepoURL, &task.CommitSHA, &task.IsGenerated, &task.FirstSeen, &task.UpdatedAt); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func (s *Store) SearchTasks(query string) ([]Task, error) {
	return s.ListTasks(strings.TrimSpace(query))
}
