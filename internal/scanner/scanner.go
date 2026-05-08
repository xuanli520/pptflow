package scanner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xuanli520/p2r_tui/internal/projectlayout"
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
	batches, err := os.ReadDir(result.Root)
	if err != nil {
		return result, fmt.Errorf("scan root unavailable: %w", err)
	}
	for _, batchEntry := range batches {
		if !batchEntry.IsDir() || excludedTopLevel(batchEntry.Name()) || !projectlayout.IsBatchDir(batchEntry.Name()) {
			continue
		}
		result.VisitedDirs++
		batchName := batchEntry.Name()
		batchPath := filepath.Join(result.Root, batchName)
		tasks, err := os.ReadDir(batchPath)
		if err != nil {
			return result, fmt.Errorf("scan batch failed at %s: %w", batchPath, err)
		}
		for _, taskEntry := range tasks {
			if !taskEntry.IsDir() || excludedComponent(taskEntry.Name()) {
				continue
			}
			result.VisitedDirs++
			if !projectlayout.IsTaskID(taskEntry.Name()) {
				continue
			}
			taskID := taskEntry.Name()
			projectPath := projectlayout.ExpectedProjectPath(result.Root, batchName, taskID)
			if !isValidProject(projectPath) {
				continue
			}
			result.Projects = append(result.Projects, Project{
				TaskID:                taskID,
				Batch:                 batchName,
				Path:                  filepath.Clean(projectPath),
				MetadataPromptMissing: promptMissing(filepath.Join(projectPath, "metadata.json")),
			})
		}
	}
	result.Skipped = result.VisitedDirs - len(result.Projects)
	return result, nil
}

func isValidProject(path string) bool {
	return projectlayout.ValidatePackageRoot(path).Valid
}

func excludedTopLevel(name string) bool {
	switch name {
	case "result", ".qa-control", "task-docs":
		return true
	default:
		return excludedComponent(name)
	}
}

func excludedComponent(name string) bool {
	switch name {
	case "qa", "runs", "script_input_snapshot":
		return true
	default:
		return false
	}
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
