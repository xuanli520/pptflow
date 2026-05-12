package pipeline

import (
	"path/filepath"
	"strings"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/pathutil"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/projectlayout"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func SelfTestReportPath(projectPath string, cfg config.Config) string {
	candidates := SelfTestReportCandidates(projectPath, cfg)
	if len(candidates) == 0 {
		return filepath.Clean(filepath.Join(projectPath, "repo", "self_test_report.md"))
	}
	return candidates[0]
}

func SelfTestReportCandidates(projectPath string, cfg config.Config) []string {
	var candidates []string
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(projectPath, path)
		}
		path = filepath.Clean(path)
		for _, existing := range candidates {
			if existing == path {
				return
			}
		}
		candidates = append(candidates, path)
	}
	add(cfg.Pipeline.SelfTestReportPath)
	add("repo/self_test_report.md")
	add("docs/self-test-report.md")
	return candidates
}

func runArtifactRoot(scanPath string, project scanner.Project, runID string) string {
	batchDir := projectlayout.SafePathSegment(project.Batch, "unbatched")
	taskDir := projectlayout.SafePathSegment(project.TaskID, "TASK-UNKNOWN")
	runDir := projectlayout.SafePathSegment(runID, "run-unknown")
	primary := filepath.Join(filepath.Clean(scanPath), "result", batchDir, taskDir, runDir)
	if pathutil.PathWithin(primary, project.Path) {
		return filepath.Join(filepath.Clean(scanPath), ".qa-control", "runs", batchDir, taskDir, runDir)
	}
	return primary
}

func stageLogPath(artifactRoot, stage string) string {
	return filepath.Join(artifactRoot, "logs", model.StageLogName(stage))
}
