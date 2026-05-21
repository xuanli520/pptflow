package pipeline_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/executor"
	pipelinepkg "github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

type testRuntimeEvidence = pipelinepkg.TestRuntimeEvidence
type testProbeResult = pipelinepkg.TestProbeResult
type testStageCCommandEnv = pipelinepkg.TestStageCCommandEnv
type testServiceURLEnv = pipelinepkg.TestServiceURLEnv
type testServiceURL = pipelinepkg.TestServiceURL

func stageCEnvironment(evidence testRuntimeEvidence) testStageCCommandEnv {
	return pipelinepkg.StageCEnvironmentForTest(evidence)
}

func cleanupStageCTestArtifacts(repoPath string) pipelinepkg.TestStageCTestArtifactCleanup {
	return pipelinepkg.CleanupStageCTestArtifactsForTest(repoPath)
}

func TestStageCEnvironmentPassesComposeProjectAndFile(t *testing.T) {
	evidence := testRuntimeEvidence{
		ComposeProject: "p2rqa_task_run_hash",
		ComposeFile:    "/tmp/project/repo/compose.yaml",
		Mappings: map[string][]portMapping{
			"api": {{
				Service:   "api",
				URL:       "0.0.0.0",
				Host:      4300,
				Container: 4300,
				Protocol:  "tcp",
			}},
		},
	}

	env := stageCEnvironment(evidence)
	if got := env.Values["API_URL"]; got != "http://localhost:4300" {
		t.Fatalf("API_URL = %q, want host runtime URL", got)
	}
	if got := env.Values["COMPOSE_PROJECT_NAME"]; got != evidence.ComposeProject {
		t.Fatalf("COMPOSE_PROJECT_NAME = %q, want %q", got, evidence.ComposeProject)
	}
	if got := env.Values["COMPOSE_FILE"]; got != evidence.ComposeFile {
		t.Fatalf("COMPOSE_FILE = %q, want %q", got, evidence.ComposeFile)
	}
	assertKeyOrder(t, env.Keys, []string{"API_URL", "COMPOSE_PROJECT_NAME", "COMPOSE_FILE"})
}

func TestCleanupStageCTestArtifactsRemovesGeneratedTestOutputs(t *testing.T) {
	repoPath := t.TempDir()
	writeFile := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("test"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join(repoPath, ".coverage"))
	writeFile(filepath.Join(repoPath, ".nyc_output", "coverage.json"))
	writeFile(filepath.Join(repoPath, ".pytest_cache", "README.md"))
	writeFile(filepath.Join(repoPath, "nested", "__pycache__", "module.pyc"))
	writeFile(filepath.Join(repoPath, "playwright-report", "index.html"))
	writeFile(filepath.Join(repoPath, "test-results", "result.json"))
	writeFile(filepath.Join(repoPath, ".git", ".coverage"))
	writeFile(filepath.Join(repoPath, "node_modules", ".pytest_cache", "README.md"))
	writeFile(filepath.Join(repoPath, "src", "main.go"))

	cleanup := cleanupStageCTestArtifacts(repoPath)
	if len(cleanup.Warnings) != 0 {
		t.Fatalf("cleanup warnings = %#v, want none", cleanup.Warnings)
	}
	slices.Sort(cleanup.Removed)
	wantRemoved := []string{
		".coverage",
		".nyc_output",
		".pytest_cache",
		"nested/__pycache__",
		"playwright-report",
		"test-results",
	}
	if !slices.Equal(cleanup.Removed, wantRemoved) {
		t.Fatalf("removed = %#v, want %#v", cleanup.Removed, wantRemoved)
	}
	for _, removed := range wantRemoved {
		if _, err := os.Stat(filepath.Join(repoPath, filepath.FromSlash(removed))); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be removed, stat err: %v", removed, err)
		}
	}
	for _, preserved := range []string{
		filepath.Join(".git", ".coverage"),
		filepath.Join("node_modules", ".pytest_cache", "README.md"),
		filepath.Join("src", "main.go"),
	} {
		if _, err := os.Stat(filepath.Join(repoPath, preserved)); err != nil {
			t.Fatalf("expected %s to be preserved: %v", preserved, err)
		}
	}
}

func TestRuntimeCleanupPointDoesNotCleanBetweenRuntimeStages(t *testing.T) {
	stages := []model.StageRecord{
		{Stage: "B", Status: model.StageDone},
		{Stage: "C", Status: model.StagePending},
	}
	for _, stage := range []string{"B", "C"} {
		if pipelinepkg.RuntimeCleanupPointForTest(stage, stages) {
			t.Fatalf("runtime cleanup point for %s should be disabled", stage)
		}
	}
}

func TestFilteredRuntimeEnvDropsHostSecretsAndKeepsInjectedValues(t *testing.T) {
	env := pipelinepkg.FilteredRuntimeEnvForTest([]string{
		"PATH=/usr/bin",
		"OPENAI_API_KEY=secret",
		"AWS_SESSION_TOKEN=secret",
		"HOME=/home/test",
	}, []string{
		"API_URL=http://localhost:38080",
		"COMPOSE_PROJECT_NAME=p2r_test",
	}, false)
	values := envMap(env)
	if values["PATH"] != "/usr/bin" {
		t.Fatalf("PATH should be preserved in runtime env: %#v", env)
	}
	for _, key := range []string{"OPENAI_API_KEY", "AWS_SESSION_TOKEN", "HOME"} {
		if _, ok := values[key]; ok {
			t.Fatalf("runtime env leaked %s: %#v", key, env)
		}
	}
	if values["API_URL"] != "http://localhost:38080" || values["COMPOSE_PROJECT_NAME"] != "p2r_test" {
		t.Fatalf("injected runtime values missing: %#v", env)
	}
}

func TestFilteredDockerEnvKeepsDockerSettingsWithoutSecrets(t *testing.T) {
	env := pipelinepkg.FilteredRuntimeEnvForTest([]string{
		"PATH=/usr/bin",
		"DOCKER_HOST=unix:///var/run/docker.sock",
		"DOCKER_TOKEN=secret",
		"HOME=/home/test",
	}, nil, true)
	values := envMap(env)
	if values["DOCKER_HOST"] == "" || values["HOME"] == "" {
		t.Fatalf("docker env should keep Docker/HOME settings: %#v", env)
	}
	if _, ok := values["DOCKER_TOKEN"]; ok {
		t.Fatalf("docker env leaked token: %#v", env)
	}
}

func TestStageBReturnsRuntimeStateWhenPortMapWriteFails(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "batch", "TASK-1")
	repoPath := filepath.Join(projectPath, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(repoPath, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte("services:\n  web:\n    image: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(root, "run")
	if err := os.MkdirAll(filepath.Join(artifactRoot, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(artifactRoot, "port_map.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Docker.HealthCheckTimeoutSeconds = 1
	runner := pipelinepkg.NewRunner(nil, cfg, pipelinepkg.WithCommandRunner(stageBDockerRunner{}))
	outcome := runner.StageBForTest(context.Background(), model.RunRecord{
		RunID:        "run-1",
		TaskID:       "TASK-1",
		ArtifactRoot: artifactRoot,
	}, scanner.Project{TaskID: "TASK-1", Path: projectPath})

	if outcome.Record.Status != model.StageFailed || !strings.Contains(outcome.Record.ErrorSummary, "port_map.json") {
		t.Fatalf("Stage B should fail on required port_map write, got %#v", outcome.Record)
	}
	if outcome.Runtime == nil || !outcome.Runtime.HasCleanupTarget() {
		t.Fatalf("runtime cleanup target should survive artifact write failure: %#v", outcome.Runtime)
	}
	if !outcome.Runtime.HasServiceMappings() {
		t.Fatalf("runtime service mappings should be returned from memory: %#v", outcome.Runtime)
	}
}

func TestStageBEmptyPortMappingKeepsTaskDockerRunning(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.ScanPath = root
	cfg.Docker.HealthCheckTimeoutSeconds = 1
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.CreateTaskWithBatch(context.Background(), "TASK-20260521-AAAAAA", "https://gitlab.example/TASK-20260521-AAAAAA", root)
	if err != nil {
		t.Fatal(err)
	}
	repoPath := filepath.Join(task.RepoPath, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(task.RepoPath, "docs", "original-session"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task.RepoPath, "metadata.json"), []byte(`{"task_id":"`+task.ID+`","prompt":"build it"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, "docker-compose.yml"), []byte("services:\n  web:\n    image: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := pipelinepkg.NewRunner(store, cfg, pipelinepkg.WithCommandRunner(stageBNoPortRunner{})).Run(
		context.Background(),
		task.ID,
		pipelinepkg.RunOptions{Stages: []string{"B"}, DeferRuntimeCleanup: true},
	)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	stageB := stageRecordForTest(result.Stages, "B")
	if stageB.Status != model.StageFailed || !strings.Contains(stageB.ErrorSummary, "no published ports") {
		t.Fatalf("Stage B should fail with no published ports: %#v", stageB)
	}
	stored, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.DockerRunning || stored.FrontendURL != "" || stored.ComposeMeta.Project == "" {
		t.Fatalf("partial Docker startup should keep task docker_running with compose meta: %#v", stored)
	}
}

func stageRecordForTest(stages []model.StageRecord, stage string) model.StageRecord {
	for _, record := range stages {
		if record.Stage == stage {
			return record
		}
	}
	return model.StageRecord{}
}

func assertKeyOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("keys = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keys = %#v, want %#v", got, want)
		}
	}
}

func envMap(env []string) map[string]string {
	values := map[string]string{}
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

type stageBDockerRunner struct{}

func (stageBDockerRunner) LookPath(name string) (string, error) {
	if name == "docker" {
		return "docker", nil
	}
	return name, nil
}

func (r stageBDockerRunner) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
	return r.result(name, args...)
}

func (r stageBDockerRunner) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput executor.OutputCallback, name string, args ...string) executor.Result {
	result := r.result(name, args...)
	if writer != nil {
		_, _ = writer.Write([]byte(result.Stdout + result.Stderr))
	}
	return result
}

func (stageBDockerRunner) result(name string, args ...string) executor.Result {
	command := strings.Join(append([]string{name}, args...), " ")
	if strings.Contains(command, " ps --format json") {
		return executor.Result{
			Command: command,
			Stdout:  `{"Service":"web","Publishers":[{"URL":"0.0.0.0","TargetPort":8080,"PublishedPort":38080,"Protocol":"tcp"}]}` + "\n",
		}
	}
	if strings.Contains(command, " config --services") {
		return executor.Result{Command: command, Stdout: "web\n"}
	}
	if strings.Contains(command, " ps -q") {
		return executor.Result{Command: command, Stdout: "container-web\n"}
	}
	return executor.Result{Command: command, Stdout: fmt.Sprintf("%s ok\n", command)}
}

type stageBNoPortRunner struct{}

func (stageBNoPortRunner) LookPath(name string) (string, error) {
	if name == "docker" {
		return "docker", nil
	}
	return name, nil
}

func (r stageBNoPortRunner) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
	return r.result(name, args...)
}

func (r stageBNoPortRunner) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput executor.OutputCallback, name string, args ...string) executor.Result {
	result := r.result(name, args...)
	if writer != nil {
		_, _ = writer.Write([]byte(result.Stdout + result.Stderr))
	}
	return result
}

func (stageBNoPortRunner) result(name string, args ...string) executor.Result {
	command := strings.Join(append([]string{name}, args...), " ")
	switch {
	case strings.Contains(command, " ps --format json"):
		return executor.Result{Command: command, Stdout: ""}
	case strings.Contains(command, " config --services"):
		return executor.Result{Command: command, Stdout: "web\n"}
	case strings.Contains(command, " ps -q"):
		return executor.Result{Command: command, Stdout: "container-web\n"}
	case strings.Contains(command, " port "):
		return executor.Result{Command: command, Stdout: ""}
	default:
		return executor.Result{Command: command, Stdout: fmt.Sprintf("%s ok\n", command)}
	}
}
