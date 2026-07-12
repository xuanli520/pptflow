package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
)

type CloneWorkspaceOptions struct {
	SourceWorkspace  string
	TargetWorkspace  string
	ReuseDocker      bool
	ReuseQuality     bool
	ReuseSimilarity  bool
	ReuseHarbor      bool
	AutoApproveGates bool
	RuntimeOptions   RunnerOptions
}

type CloneWorkspaceManifest struct {
	SchemaVersion   string    `json:"schema_version"`
	SourceWorkspace string    `json:"source_workspace"`
	TargetWorkspace string    `json:"target_workspace"`
	ReusedEvidence  []string  `json:"reused_evidence,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

const cloneManifestName = "clone_manifest.json"

// CloneRunnerOptions creates a new workspace from a sanitized run-options
// snapshot. Selected reusable evidence is copied or referenced explicitly;
// state.json and event_log.jsonl are never cloned, so the new run has its own
// lifecycle and run ID.
func CloneRunnerOptions(config CloneWorkspaceOptions) (RunnerOptions, CloneWorkspaceManifest, error) {
	source, err := absoluteCleanPath(config.SourceWorkspace)
	if err != nil {
		return RunnerOptions{}, CloneWorkspaceManifest{}, fmt.Errorf("source workspace: %w", err)
	}
	target, err := absoluteCleanPath(config.TargetWorkspace)
	if err != nil {
		return RunnerOptions{}, CloneWorkspaceManifest{}, fmt.Errorf("target workspace: %w", err)
	}
	if source == target {
		return RunnerOptions{}, CloneWorkspaceManifest{}, fmt.Errorf("target workspace must differ from source workspace")
	}
	createdTarget, err := ensureEmptyWorkspace(target)
	if err != nil {
		return RunnerOptions{}, CloneWorkspaceManifest{}, err
	}
	completed := false
	defer func() {
		if createdTarget && !completed {
			_ = os.RemoveAll(target)
		}
	}()

	opts, _, err := LoadRunnerOptions(source)
	if err != nil {
		return RunnerOptions{}, CloneWorkspaceManifest{}, fmt.Errorf("load source run options: %w", err)
	}
	opts = MergeRuntimeOptions(opts, config.RuntimeOptions)
	opts.Workspace = target
	opts.AutoApprove = config.AutoApproveGates
	manifest := CloneWorkspaceManifest{
		SchemaVersion:   "harbor.clone_manifest.v1",
		SourceWorkspace: source,
		TargetWorkspace: target,
		CreatedAt:       time.Now().UTC(),
	}

	copyEvidence := func(label, sourcePath, targetPath string) error {
		copied, copyErr := copyRegularFile(sourcePath, targetPath)
		if copyErr != nil {
			return copyErr
		}
		if copied {
			manifest.ReusedEvidence = append(manifest.ReusedEvidence, label)
		}
		return nil
	}
	if config.ReuseDocker {
		if err := copyEvidence("docker_verify", nodes.VerifyReportPath(source), nodes.VerifyReportPath(target)); err != nil {
			return RunnerOptions{}, CloneWorkspaceManifest{}, err
		}
	}
	if config.ReuseQuality {
		if err := copyEvidence("quality", nodes.QualityReportPath(source), nodes.QualityReportPath(target)); err != nil {
			return RunnerOptions{}, CloneWorkspaceManifest{}, err
		}
	}
	if config.ReuseSimilarity {
		if err := copyEvidence("similarity", nodes.SimilarityReportPath(source), nodes.SimilarityReportPath(target)); err != nil {
			return RunnerOptions{}, CloneWorkspaceManifest{}, err
		}
	}
	if config.ReuseHarbor {
		if regularFile(nodes.QwenResultPath(source)) {
			opts.QwenResult = nodes.QwenResultPath(source)
			manifest.ReusedEvidence = append(manifest.ReusedEvidence, "harbor_qwen")
			setTrialScreenshot(nodes.QwenResultPath(source), &opts.QwenScreenshot)
		}
		if regularFile(nodes.OpusResultPath(source)) {
			opts.OpusResult = nodes.OpusResultPath(source)
			manifest.ReusedEvidence = append(manifest.ReusedEvidence, "harbor_opus")
			setTrialScreenshot(nodes.OpusResultPath(source), &opts.OpusScreenshot)
		}
	}

	if _, err := SaveRunnerOptions(opts); err != nil {
		return RunnerOptions{}, CloneWorkspaceManifest{}, fmt.Errorf("write cloned run options: %w", err)
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return RunnerOptions{}, CloneWorkspaceManifest{}, err
	}
	if err := os.WriteFile(filepath.Join(target, cloneManifestName), append(raw, '\n'), 0o600); err != nil {
		return RunnerOptions{}, CloneWorkspaceManifest{}, fmt.Errorf("write clone manifest: %w", err)
	}
	completed = true
	return opts, manifest, nil
}

func ensureEmptyWorkspace(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err == nil {
		if len(entries) != 0 {
			return false, fmt.Errorf("target workspace is not empty: %s", path)
		}
		return false, nil
	}
	if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect target workspace: %w", err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return false, fmt.Errorf("create target workspace: %w", err)
	}
	return true, nil
}

func absoluteCleanPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func copyRegularFile(source, target string) (bool, error) {
	info, err := os.Stat(source)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect reusable evidence: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("reusable evidence is not a regular file: %s", source)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		return false, fmt.Errorf("read reusable evidence: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return false, err
	}
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		return false, fmt.Errorf("copy reusable evidence: %w", err)
	}
	return true, nil
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func setTrialScreenshot(path string, target *string) {
	if target == nil || strings.TrimSpace(*target) != "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var result domain.TrialResult
	if json.Unmarshal(raw, &result) != nil || strings.TrimSpace(result.Screenshot) == "" {
		return
	}
	screenshot := result.Screenshot
	if !filepath.IsAbs(screenshot) {
		screenshot = filepath.Join(filepath.Dir(path), screenshot)
	}
	if regularFile(screenshot) {
		*target = screenshot
	}
}
