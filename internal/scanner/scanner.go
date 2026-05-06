package scanner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Project struct {
	TaskID                string
	Batch                 string
	Path                  string
	MetadataPromptMissing bool
}

type Result struct {
	Root        string
	VisitedDirs int
	Projects    []Project
	Skipped     int
}

func Scan(root string) (Result, error) {
	result := Result{Root: filepath.Clean(root)}
	info, err := os.Stat(result.Root)
	if err != nil {
		return result, fmt.Errorf("scan root unavailable: %w", err)
	}
	if !info.IsDir() {
		return result, fmt.Errorf("scan root is not a directory: %s", result.Root)
	}
	err = filepath.WalkDir(result.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("scan traversal failed at %s: %w", path, err)
		}
		if !d.IsDir() {
			return nil
		}
		result.VisitedDirs++
		if isProject(path) {
			project := Project{
				TaskID:                taskID(path),
				Batch:                 batch(path, result.Root),
				Path:                  filepath.Clean(path),
				MetadataPromptMissing: promptMissing(filepath.Join(path, "metadata.json")),
			}
			result.Projects = append(result.Projects, project)
			return filepath.SkipDir
		}
		return nil
	})
	result.Skipped = result.VisitedDirs - len(result.Projects)
	return result, err
}

func isProject(path string) bool {
	for _, item := range []string{"docs", "repo", "original_sessions"} {
		info, err := os.Stat(filepath.Join(path, item))
		if err != nil || !info.IsDir() {
			return false
		}
	}
	info, err := os.Stat(filepath.Join(path, "metadata.json"))
	return err == nil && !info.IsDir()
}

func taskID(path string) string {
	metadata := filepath.Join(path, "metadata.json")
	content, err := os.ReadFile(metadata)
	if err == nil {
		var data map[string]any
		if json.Unmarshal(content, &data) == nil {
			for _, key := range []string{"task_id", "taskId", "id"} {
				if value, ok := data[key].(string); ok && strings.TrimSpace(value) != "" {
					return sanitizeID(value)
				}
			}
		}
	}
	return sanitizeID(filepath.Base(path))
}

func batch(path, root string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return filepath.Base(path)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) > 1 {
		return parts[0]
	}
	return filepath.Base(filepath.Dir(path))
}

func promptMissing(path string) bool {
	content, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	var data map[string]any
	if json.Unmarshal(content, &data) != nil {
		return true
	}
	value, ok := data["prompt"].(string)
	return !ok || strings.TrimSpace(value) == ""
}

var invalidID = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func sanitizeID(value string) string {
	value = strings.TrimSpace(value)
	value = invalidID.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if value == "" {
		return "TASK-UNKNOWN"
	}
	return value
}
