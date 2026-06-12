package pipeline

import (
	"path/filepath"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

func runtimeInputProjectPath(run model.RunRecord, projectPath string) string {
	snapshot := filepath.Join(run.ArtifactRoot, "script_input_snapshot")
	if dirExists(filepath.Join(snapshot, "repo")) {
		return snapshot
	}
	return projectPath
}

func runtimeInputRepoPath(run model.RunRecord, projectPath string) string {
	return filepath.Join(runtimeInputProjectPath(run, projectPath), "repo")
}
