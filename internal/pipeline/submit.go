package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/projectlayout"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

type submitArtifactCopy struct {
	Name        string `json:"name"`
	Stage       string `json:"stage,omitempty"`
	Source      string `json:"source,omitempty"`
	Target      string `json:"target,omitempty"`
	OK          bool   `json:"ok"`
	NotSelected bool   `json:"not_selected,omitempty"`
	Error       string `json:"error,omitempty"`
}

type submitArtifactSpec struct {
	Name  string
	Stage string
}

func (s *runState) aggregateSubmitArtifacts(r Runner) {
	submitDir := submitRoot(r.cfg.ScanPath, s.prepare.project)
	copies, resetErr := aggregateSubmitArtifacts(
		s.identity.artifactRoot,
		submitDir,
		submitArtifactSpecs(s.prepare.opts.Mode),
		selectedSubmitStages(s.execution.stages),
	)
	if resetErr != nil {
		s.addArtifactWarning(ArtifactWarning{
			Path:       submitDir,
			Op:         "submit_reset",
			Error:      resetErr.Error(),
			RecordedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}
	for _, item := range copies {
		if item.OK || item.NotSelected {
			continue
		}
		s.addArtifactWarning(ArtifactWarning{
			Path:       item.Name,
			Op:         "submit_copy",
			Error:      item.Error,
			RecordedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}
	s.persistArtifactWarnings()
	reset := map[string]any{"ok": resetErr == nil}
	if resetErr != nil {
		reset["error"] = resetErr.Error()
	}
	_ = s.identity.writer.BestEffortJSON("submit_manifest.json", map[string]any{
		"submit_dir": submitDir,
		"reset":      reset,
		"files":      copies,
	})
}

func aggregateSubmitArtifacts(artifactRoot, submitDir string, specs []submitArtifactSpec, selected map[string]bool) ([]submitArtifactCopy, error) {
	resetErr := resetSubmitDir(submitDir)
	copies := make([]submitArtifactCopy, 0, len(specs))
	for _, spec := range specs {
		source := filepath.Join(artifactRoot, spec.Name)
		target := filepath.Join(submitDir, spec.Name)
		item := submitArtifactCopy{Name: spec.Name, Stage: spec.Stage, Source: source, Target: target}
		if !selected[spec.Stage] {
			item.NotSelected = true
			copies = append(copies, item)
			continue
		}
		if resetErr != nil {
			item.Error = "reset submit directory: " + resetErr.Error()
			copies = append(copies, item)
			continue
		}
		info, err := os.Stat(source)
		if err != nil {
			item.Error = err.Error()
			copies = append(copies, item)
			continue
		}
		if info.IsDir() {
			item.Error = "source is a directory"
			copies = append(copies, item)
			continue
		}
		if err := copyFile(source, target, info.Mode()); err != nil {
			item.Error = err.Error()
			copies = append(copies, item)
			continue
		}
		item.OK = true
		copies = append(copies, item)
	}
	return copies, resetErr
}

func resetSubmitDir(submitDir string) error {
	submitDir = filepath.Clean(submitDir)
	if filepath.Base(submitDir) != "submit" {
		return fmt.Errorf("refusing to reset non-submit directory %q", submitDir)
	}
	if err := os.RemoveAll(submitDir); err != nil {
		return err
	}
	return os.MkdirAll(submitDir, 0o755)
}

func submitRoot(scanPath string, project scanner.Project) string {
	batchDir := projectlayout.SafePathSegment(project.Batch, "unbatched")
	taskDir := projectlayout.SafePathSegment(project.TaskID, "TASK-UNKNOWN")
	return filepath.Join(filepath.Clean(scanPath), "result", batchDir, taskDir, "submit")
}

func submitArtifactNames(mode string) []string {
	specs := submitArtifactSpecs(mode)
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

func submitArtifactSpecs(mode string) []submitArtifactSpec {
	if mode == "recheck" {
		return []submitArtifactSpec{
			{Name: qaArtifactName("codex_report_verification.md"), Stage: "E"},
			{Name: qaArtifactName("validation_report.md"), Stage: "A"},
			{Name: qaArtifactName("prompt_requirements_verification.md"), Stage: "F"},
			{Name: qaArtifactName("codex_report_issues_verification.md"), Stage: "F"},
			{Name: qaArtifactName("test_effectiveness_verification.md"), Stage: "D"},
			{Name: qaArtifactName("docker_startup.png"), Stage: "B"},
			{Name: qaArtifactName("run_tests_screenshot.png"), Stage: "C"},
			{Name: qaArtifactName("trajectory_archive.png"), Stage: "A"},
		}
	}
	return []submitArtifactSpec{
		{Name: qaArtifactName("codex_report.md"), Stage: "E"},
		{Name: qaArtifactName("validation_report.md"), Stage: "A"},
		{Name: qaArtifactName("operator_prompt_requirements_verification.md"), Stage: "F"},
		{Name: qaArtifactName("operator_codex_report_issues_verification.md"), Stage: "F"},
		{Name: qaArtifactName("test_effectiveness_report.md"), Stage: "D"},
		{Name: qaArtifactName("docker_startup.png"), Stage: "B"},
		{Name: qaArtifactName("run_tests_screenshot.png"), Stage: "C"},
		{Name: qaArtifactName("trajectory_archive.png"), Stage: "A"},
	}
}

func selectedSubmitStages(stages []model.StageRecord) map[string]bool {
	selected := map[string]bool{}
	for _, stage := range stages {
		if stage.Status == model.StageSkipped {
			continue
		}
		selected[stage.Stage] = true
	}
	return selected
}
