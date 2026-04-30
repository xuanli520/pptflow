package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

type taskRunLock struct {
	path string
	file *os.File
}

func (r Runner) acquireTaskRunLock(taskID string) (taskRunLock, error) {
	lockDir := filepath.Join(r.cfg.ScanPath, ".qa-control", "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return taskRunLock{}, err
	}
	name := safeLockName(taskID) + ".lock"
	path := filepath.Join(lockDir, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil && os.IsExist(err) && staleTaskRunLock(path) {
		_ = os.Remove(path)
		file, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	}
	if err != nil {
		return taskRunLock{}, fmt.Errorf("task %s is already locked for a p2r run or lock cannot be created: %w", taskID, err)
	}
	_, _ = fmt.Fprintf(file, "task_id=%s\npid=%d\ncreated_at=%s\n", taskID, os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	return taskRunLock{path: path, file: file}, nil
}

func (l taskRunLock) Release() {
	if l.file != nil {
		_ = l.file.Close()
	}
	if l.path != "" {
		_ = os.Remove(l.path)
	}
}

func (r Runner) cleanupStaleRuns(ctx context.Context, runs []model.RunRecord, currentRunID, artifactRoot string) []dockermgr.CleanupSummary {
	var summaries []dockermgr.CleanupSummary
	for _, run := range runs {
		if run.RunID == currentRunID || run.ArtifactRoot == "" {
			continue
		}
		evidence, err := readRuntimeEvidence(filepath.Join(run.ArtifactRoot, "port_map.json"))
		if err != nil || evidence.ComposeProject == "" {
			continue
		}
		summary := dockermgr.CleanupComposeProject(ctx, r.exec, r.cfg.Docker, evidence.ComposeFile, evidence.ComposeProject, evidence.WorkDir)
		summary.Status = "stale_" + summary.Status
		summaries = append(summaries, summary)
	}
	if len(summaries) > 0 {
		_ = writeJSON(filepath.Join(artifactRoot, "pre_run_cleanup_summary.json"), map[string]any{"stale_runs": summaries})
	}
	return summaries
}

func (r Runner) cleanupCurrentRuntime(ctx context.Context, run model.RunRecord, keepRuntime bool) dockermgr.CleanupSummary {
	path := filepath.Join(run.ArtifactRoot, "port_map.json")
	evidence, err := readRuntimeEvidence(path)
	if err != nil || evidence.ComposeProject == "" {
		summary := dockermgr.CleanupSummary{Status: "not_applicable"}
		if err != nil {
			summary.Warnings = append(summary.Warnings, "runtime evidence unavailable: "+err.Error())
		}
		_ = writeJSON(filepath.Join(run.ArtifactRoot, "cleanup_summary.json"), summary)
		return summary
	}
	if keepRuntime {
		args := cleanupCommand(evidence.ComposeFile, evidence.ComposeProject)
		summary := dockermgr.CleanupSummary{
			Status:         "kept_by_operator_request",
			ComposeFile:    evidence.ComposeFile,
			ComposeProject: evidence.ComposeProject,
			WorkDir:        evidence.WorkDir,
			ManualCommand:  strings.Join(append([]string{"docker"}, args...), " "),
		}
		_ = writeJSON(filepath.Join(run.ArtifactRoot, "cleanup_summary.json"), summary)
		return summary
	}
	summary := dockermgr.CleanupComposeProject(ctx, r.exec, r.cfg.Docker, evidence.ComposeFile, evidence.ComposeProject, evidence.WorkDir)
	_ = writeJSON(filepath.Join(run.ArtifactRoot, "cleanup_summary.json"), summary)
	return summary
}

func runtimeCleanupPoint(stage string, stages []model.StageRecord) bool {
	if stage == "C" {
		return true
	}
	if stage != "B" {
		return false
	}
	for _, item := range stages {
		if item.Stage == "C" && item.Status != model.StageSkipped {
			return false
		}
	}
	return true
}

func runtimeStageWasSelected(stages []model.StageRecord) bool {
	for _, item := range stages {
		if (item.Stage == "B" || item.Stage == "C") && item.Status != model.StageSkipped {
			return true
		}
	}
	return false
}

func cleanupCommand(composeFile, projectName string) []string {
	args := []string{"compose"}
	if strings.TrimSpace(composeFile) != "" {
		args = append(args, "-f", composeFile)
	}
	args = append(args, "-p", projectName, "down", "-v", "--remove-orphans", "--rmi", "local")
	return args
}

func safeLockName(taskID string) string {
	value := strings.ToLower(taskID)
	var builder strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		builder.WriteString("task")
	}
	sum := sha256Text(taskID)
	name := builder.String()
	if len(name) > 48 {
		name = name[:48]
	}
	return strings.Trim(name, "._-") + "-" + sum[:8]
}

func staleTaskRunLock(path string) bool {
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(content), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	pid, err := strconv.Atoi(values["pid"])
	if err != nil || pid <= 0 {
		return false
	}
	return !processAlive(pid)
}

func mergeCleanupIntoManifest(path string, key string, value any) {
	content, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var data map[string]any
	if json.Unmarshal(content, &data) != nil {
		return
	}
	data[key] = value
	_ = writeJSON(path, data)
}

func cleanupFinding(summary dockermgr.CleanupSummary) model.Finding {
	evidence := strings.TrimSpace(summary.Error)
	if evidence == "" {
		evidence = strings.TrimSpace(summary.Stderr)
	}
	if evidence == "" {
		evidence = "cleanup command failed"
	}
	return model.Finding{
		ID:         "P2R-INFRA-HIGH-001",
		Stage:      "INFRA",
		Severity:   "High",
		Title:      "Docker cleanup failed",
		Rule:       "p2r-managed Docker resources must be cleaned after runtime stages.",
		Evidence:   evidence,
		Impact:     "Repeated runs may hit stale containers, networks, volumes, or port conflicts.",
		MinimumFix: "Run the cleanup command recorded in cleanup_summary.json and rerun the affected task.",
	}
}
