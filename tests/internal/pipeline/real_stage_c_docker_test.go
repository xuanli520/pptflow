//go:build realdocker

package pipeline_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/executor"
	pipelinepkg "github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func TestRealBatch1StageBCRunTests(t *testing.T) {
	scanPath := realDockerEnv("P2R_REAL_SCAN_PATH", "../projects-qa")
	result, err := scanner.Scan(scanPath)
	if err != nil {
		t.Fatal(err)
	}
	projects := map[string]scanner.Project{}
	for _, project := range result.Projects {
		projects[project.TaskID] = project
	}
	taskIDs := realDockerTaskIDs([]string{
		"TASK-20260421-73955A",
		"TASK-20260508-5388C5",
		"TASK-20260508-6CCDE1",
	})
	for _, taskID := range taskIDs {
		project, ok := projects[taskID]
		if !ok {
			t.Fatalf("project %s not found under %s", taskID, scanPath)
		}
		t.Run(taskID, func(t *testing.T) {
			runRealStageBC(t, project)
		})
	}
}

func runRealStageBC(t *testing.T, project scanner.Project) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cfg := config.Default()
	cfg.ScanPath = filepath.Clean(realDockerEnv("P2R_REAL_SCAN_PATH", "../projects-qa"))
	cfg.DBPath = filepath.Join(cfg.ScanPath, ".qa-control", "real_stage_c.db")
	cfg.Pipeline.StageC.Execution = realDockerEnv("P2R_REAL_STAGE_C_EXECUTION", "host")
	cfg.Pipeline.StageC.RunnerImage = strings.TrimSpace(os.Getenv("P2R_REAL_STAGE_C_RUNNER_IMAGE"))
	cfg.Docker.CleanupImages = false
	cfg.Docker.CleanupVolumes = true
	cfg.Docker.BuildCachePruneUntil = ""
	if timeout := strings.TrimSpace(os.Getenv("P2R_REAL_STAGE_TIMEOUT_SECONDS")); timeout != "" {
		for _, key := range []string{"B", "B_PULL", "B_BUILD", "B_UP", "B_HEALTH", "B_PORT", "C"} {
			cfg.Pipeline.StageTimeouts[key] = 1800
		}
	}

	artifactRoot := filepath.Join(cfg.ScanPath, ".qa-control", "real-stage-c", project.TaskID)
	if err := os.RemoveAll(artifactRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(artifactRoot, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := model.RunRecord{
		RunID:        "real-stage-c-" + strings.ToLower(project.TaskID),
		TaskID:       project.TaskID,
		ArtifactRoot: artifactRoot,
	}
	runner := pipelinepkg.NewRunner(nil, cfg)
	stageB := runner.StageBForTest(ctx, run, project)
	if stageB.Runtime != nil {
		defer func() {
			_ = dockermgr.CleanupComposeProjectFilesWithEnvFiles(context.Background(), executor.New(), cfg.Docker, stageB.Runtime.ComposeFiles, stageB.Runtime.EnvFiles, stageB.Runtime.ComposeProject, stageB.Runtime.WorkDir)
		}()
	}
	if stageB.Record.Status != model.StageDone {
		t.Fatalf("stage B = %s (%s); log: %s", stageB.Record.Status, stageB.Record.ErrorSummary, stageB.Record.LogPath)
	}
	runtime := testRuntimeEvidenceFromState(*stageB.Runtime)
	stageC := runner.StageCForTest(ctx, run, project, runtime, map[string]model.StageRecord{
		string(model.StageB): stageB.Record,
	})
	if stageC.Status != model.StageDone {
		t.Fatalf("stage C = %s (%s); log: %s", stageC.Status, stageC.ErrorSummary, stageC.LogPath)
	}
}

func testRuntimeEvidenceFromState(state pipelinepkg.RuntimeState) testRuntimeEvidence {
	state.Normalize()
	mappings := map[string][]pipelinepkg.TestPortMapping{}
	for service, values := range state.Mappings {
		for _, value := range values {
			mappings[service] = append(mappings[service], pipelinepkg.TestPortMapping{
				Service:   value.Service,
				URL:       value.URL,
				Host:      value.Host,
				Container: value.Container,
				Protocol:  value.Protocol,
			})
		}
	}
	var probes []testProbeResult
	for _, probe := range state.Probes {
		probes = append(probes, testProbeResult{
			Service: probe.Service,
			URL:     probe.URL,
			OK:      probe.OK,
			Status:  probe.Status,
			Error:   probe.Error,
		})
	}
	return testRuntimeEvidence{
		ComposeProject: state.ComposeProject,
		ComposeFile:    state.ComposeFile,
		ComposeFiles:   append([]string{}, state.ComposeFiles...),
		WorkDir:        state.WorkDir,
		Services:       append([]string{}, state.Services...),
		Mappings:       mappings,
		Probes:         probes,
		Mirror: pipelinepkg.TestRuntimeMirrorState{
			BuildMirrorEnabled:      state.Mirror.BuildMirrorEnabled,
			BuildMirrorMode:         state.Mirror.BuildMirrorMode,
			BuildMirrorFallbackUsed: state.Mirror.BuildMirrorFallbackUsed,
			BuildMirrorSummary:      state.Mirror.BuildMirrorSummary,
		},
	}
}

func realDockerEnv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func realDockerTaskIDs(fallback []string) []string {
	raw := strings.TrimSpace(os.Getenv("P2R_REAL_TASK_IDS"))
	if raw == "" {
		return fallback
	}
	var taskIDs []string
	for _, item := range strings.Split(raw, ",") {
		if taskID := strings.TrimSpace(item); taskID != "" {
			taskIDs = append(taskIDs, taskID)
		}
	}
	if len(taskIDs) == 0 {
		return fallback
	}
	return taskIDs
}
