package pipeline_test

import (
	"context"
	"encoding/json"
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

func stageCProxyPlan(runtime testRuntimeEvidence, repoPath, artifactRoot, runnerImage, proxyImage string) (pipelinepkg.TestStageCProxyPlan, error) {
	return pipelinepkg.StageCProxyPlanForTest(runtime, repoPath, artifactRoot, runnerImage, proxyImage)
}

func stageCProxyPlanFromComposeContent(runtime testRuntimeEvidence, repoPath, artifactRoot, runnerImage, proxyImage, composeContent string) (pipelinepkg.TestStageCProxyPlan, error) {
	return pipelinepkg.StageCProxyPlanFromComposeContentForTest(runtime, repoPath, artifactRoot, runnerImage, proxyImage, composeContent)
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
	writeFile(filepath.Join(repoPath, ".venv-tests", "pyvenv.cfg"))
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
		".venv-tests",
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

func TestStageCProxyPlanMapsOriginalPublishedPortsToServices(t *testing.T) {
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(repoPath, "compose.yml")
	if err := os.WriteFile(composePath, []byte(`services:
  web:
    image: nginx
    ports:
      - "8080:80"
  api:
    image: api
    ports:
      - target: 9000
        published: "19000"
        protocol: tcp
  worker:
    image: worker
    ports:
      - "7000"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := stageCProxyPlan(testRuntimeEvidence{
		ComposeProject: "p2rqa_task_run",
		ComposeFile:    composePath,
		ComposeFiles:   []string{composePath},
		WorkDir:        repoPath,
	}, repoPath, filepath.Join(root, "artifacts"), "golang:1.25", "alpine/socat:latest")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Mappings) != 2 {
		t.Fatalf("proxy mappings = %#v, want web/api published ports only", plan.Mappings)
	}
	if plan.Mappings[0].Listen != 8080 || plan.Mappings[0].Service != "web" || plan.Mappings[0].Target != 80 {
		t.Fatalf("first mapping = %#v", plan.Mappings[0])
	}
	if plan.Mappings[1].Listen != 19000 || plan.Mappings[1].Service != "api" || plan.Mappings[1].Target != 9000 {
		t.Fatalf("second mapping = %#v", plan.Mappings[1])
	}
	if !strings.Contains(plan.EnvContent, "P2R_WEB_LOCALHOST_URL=http://localhost:8080") {
		t.Fatalf("env content missing localhost URL:\n%s", plan.EnvContent)
	}
	if !strings.Contains(plan.OverrideContent, "network_mode: service:p2r_stage_c_proxy") ||
		strings.Contains(plan.OverrideContent, "ports:") ||
		!strings.Contains(plan.OverrideContent, "entrypoint: []") ||
		!strings.Contains(plan.OverrideContent, repoPath+":/workspace") ||
		!strings.Contains(plan.OverrideContent, "golang:1.25") {
		t.Fatalf("runner override should share proxy namespace without publishing ports:\n%s", plan.OverrideContent)
	}
}

func TestStageCProxyPlanUsesComposeConfigContent(t *testing.T) {
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(repoPath, "compose.yml")
	if err := os.WriteFile(composePath, []byte("services:\n  web:\n    image: nginx\n    ports:\n      - \"${APP_PORT:-8080}:80\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := stageCProxyPlanFromComposeContent(testRuntimeEvidence{
		ComposeProject: "p2rqa_task_run",
		ComposeFile:    composePath,
		ComposeFiles:   []string{composePath},
		WorkDir:        repoPath,
	}, repoPath, filepath.Join(root, "artifacts"), "alpine:3.20", "alpine/socat:latest", "services:\n  web:\n    image: nginx\n    ports:\n      - target: 80\n        published: \"8080\"\n        protocol: tcp\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Mappings) != 1 || plan.Mappings[0].Listen != 8080 || plan.Mappings[0].Target != 80 {
		t.Fatalf("proxy mappings should come from resolved compose config: %#v", plan.Mappings)
	}
}

func TestStageCProxyPlanExpandsPortRanges(t *testing.T) {
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(repoPath, "compose.yml")
	if err := os.WriteFile(composePath, []byte("services:\n  web:\n    image: nginx\n    ports:\n      - \"8080-8081:80-81\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := stageCProxyPlan(testRuntimeEvidence{
		ComposeProject: "p2rqa_task_run",
		ComposeFile:    composePath,
		ComposeFiles:   []string{composePath},
		WorkDir:        repoPath,
	}, repoPath, filepath.Join(root, "artifacts"), "alpine:3.20", "alpine/socat:latest")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Mappings) != 2 ||
		plan.Mappings[0].Listen != 8080 || plan.Mappings[0].Target != 80 ||
		plan.Mappings[1].Listen != 8081 || plan.Mappings[1].Target != 81 {
		t.Fatalf("proxy mappings should expand matching ranges: %#v", plan.Mappings)
	}
}

func TestStageCProxyPlanUsesPortableRunTestsEntrypoint(t *testing.T) {
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(repoPath, "compose.yml")
	if err := os.WriteFile(composePath, []byte("services:\n  web:\n    image: nginx\n    ports:\n      - \"8080:80\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, "run_tests.sh"), []byte("#!/usr/bin/env sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := stageCProxyPlan(testRuntimeEvidence{
		ComposeProject: "p2rqa_task_run",
		ComposeFile:    composePath,
		ComposeFiles:   []string{composePath},
		WorkDir:        repoPath,
	}, repoPath, filepath.Join(root, "artifacts"), "alpine:3.20", "alpine/socat:latest")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/bin/sh", "-lc", "head -n 1", "$$(head -n 1 \"$$script\"", "exec /bin/sh \"$$script\""} {
		if !strings.Contains(plan.OverrideContent, want) {
			t.Fatalf("runner command missing %q:\n%s", want, plan.OverrideContent)
		}
	}
	for _, bad := range []string{"first=$(head", "\"$script\"", "\"$first\"", " set -- $interpreter", "interpreter=${first"} {
		if strings.Contains(plan.OverrideContent, bad) {
			t.Fatalf("runner command contains unescaped compose interpolation %q:\n%s", bad, plan.OverrideContent)
		}
	}
	if strings.Contains(plan.OverrideContent, "\n    - bash\n") || strings.Contains(plan.OverrideContent, "\n    - run_tests.sh\n") {
		t.Fatalf("runner command should not hardcode bash:\n%s", plan.OverrideContent)
	}
}

func TestStageCIsolatedRejectsUnmappedHardcodedLocalhost(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "batch", "TASK-1")
	repoPath := filepath.Join(projectPath, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, "run_tests.sh"), []byte("#!/usr/bin/env bash\ncurl http://localhost:9999\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(repoPath, "compose.yml")
	if err := os.WriteFile(composePath, []byte("services:\n  web:\n    image: nginx\n    ports:\n      - \"8080:80\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(root, "artifacts")
	cfg := config.Default()
	cfg.Pipeline.StageC.Execution = "isolated"
	cfg.Pipeline.StageC.RunnerImage = "golang:1.25"
	runner := pipelinepkg.NewRunner(nil, cfg, pipelinepkg.WithCommandRunner(&stageCIsolatedRunner{}))

	record := runner.StageCForTest(context.Background(), model.RunRecord{
		RunID:        "run-1",
		TaskID:       "TASK-1",
		ArtifactRoot: artifactRoot,
	}, scanner.Project{TaskID: "TASK-1", Path: projectPath}, testRuntimeEvidence{
		ComposeProject: "p2rqa_task_run",
		ComposeFile:    composePath,
		ComposeFiles:   []string{composePath},
		WorkDir:        repoPath,
	}, nil)

	if record.Status != model.StageFailed || !strings.Contains(record.ErrorSummary, "localhost:9999") {
		t.Fatalf("unmapped localhost should fail Stage C, got %#v", record)
	}
	if len(record.Findings) == 0 || !strings.Contains(record.Findings[0].Evidence, "localhost:9999") {
		t.Fatalf("expected finding with unmapped port evidence: %#v", record.Findings)
	}
}

func TestStageCIsolatedRunsDockerComposeAndCleansProxy(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "batch", "TASK-1")
	repoPath := filepath.Join(projectPath, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, "run_tests.sh"), []byte("#!/usr/bin/env bash\ncurl http://localhost:8080\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(repoPath, "compose.yml")
	if err := os.WriteFile(composePath, []byte("services:\n  web:\n    image: nginx\n    ports:\n      - \"8080:80\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(root, "artifacts")
	cfg := config.Default()
	cfg.Pipeline.StageC.Execution = "isolated"
	cfg.Pipeline.StageC.RunnerImage = "golang:1.25"
	commandRunner := &stageCIsolatedRunner{}
	runner := pipelinepkg.NewRunner(nil, cfg, pipelinepkg.WithCommandRunner(commandRunner))

	record := runner.StageCForTest(context.Background(), model.RunRecord{
		RunID:        "run-1",
		TaskID:       "TASK-1",
		ArtifactRoot: artifactRoot,
	}, scanner.Project{TaskID: "TASK-1", Path: projectPath}, testRuntimeEvidence{
		ComposeProject: "p2rqa_task_run",
		ComposeFile:    composePath,
		ComposeFiles:   []string{composePath},
		WorkDir:        repoPath,
	}, nil)

	if record.Status != model.StageDone {
		t.Fatalf("isolated Stage C should pass, got %#v", record)
	}
	overridePath := filepath.Join(artifactRoot, "stage_c.runner.override.yml")
	if _, err := os.Stat(overridePath); err != nil {
		t.Fatalf("runner override should be written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(artifactRoot, "p2r_stage_c_proxy.json")); err != nil {
		t.Fatalf("proxy metadata should be written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(artifactRoot, "p2r_ports.env")); err != nil {
		t.Fatalf("ports env should be written: %v", err)
	}
	for _, want := range []string{
		"--profile p2r-stage-c up -d p2r_stage_c_proxy",
		"--profile p2r-stage-c run --rm -T --no-deps --name p2r_stage_c_p2rqa_task_run p2r_stage_c_runner",
		"rm -f p2r_stage_c_p2rqa_task_run",
		"--profile p2r-stage-c rm -sf p2r_stage_c_proxy",
	} {
		if !containsCommand(commandRunner.commands, want) {
			t.Fatalf("missing command %q in %#v", want, commandRunner.commands)
		}
	}
}

func TestStageCIsolatedUsesPersistedProxyPlan(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "batch", "TASK-1")
	repoPath := filepath.Join(projectPath, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, "run_tests.sh"), []byte("#!/usr/bin/env bash\ncurl http://localhost:8080\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(repoPath, "compose.yml")
	if err := os.WriteFile(composePath, []byte("services:\n  web:\n    image: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(root, "artifacts")
	overridePath := filepath.Join(artifactRoot, "stage_c.runner.override.yml")
	envPath := filepath.Join(artifactRoot, "p2r_ports.env")
	if err := os.MkdirAll(artifactRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overridePath, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("P2R_WEB_LOCALHOST_URL=http://localhost:8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := map[string]any{
		"compose_project":   "p2rqa_task_run",
		"compose_files":     []string{composePath},
		"work_dir":          repoPath,
		"runner_name":       "p2r_stage_c_p2rqa_task_run",
		"runner_image":      "golang:1.25",
		"proxy_image":       "alpine/socat:latest",
		"override_file":     overridePath,
		"env_file":          envPath,
		"proxy_config_file": filepath.Join(artifactRoot, "p2r_stage_c_proxy.json"),
		"mappings": []map[string]any{{
			"listen":  8080,
			"service": "web",
			"target":  80,
		}},
		"env": []string{"P2R_WEB_LOCALHOST_URL=http://localhost:8080"},
	}
	content, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "p2r_stage_c_proxy.json"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Pipeline.StageC.Execution = "isolated"
	cfg.Pipeline.StageC.RunnerImage = "golang:1.25"
	commandRunner := &stageCIsolatedRunner{}
	runner := pipelinepkg.NewRunner(nil, cfg, pipelinepkg.WithCommandRunner(commandRunner))
	record := runner.StageCForTest(context.Background(), model.RunRecord{
		RunID:        "run-1",
		TaskID:       "TASK-1",
		ArtifactRoot: artifactRoot,
	}, scanner.Project{TaskID: "TASK-1", Path: projectPath}, testRuntimeEvidence{
		ComposeProject: "p2rqa_task_run",
		ComposeFile:    composePath,
		ComposeFiles:   []string{composePath},
		WorkDir:        repoPath,
	}, nil)

	if record.Status != model.StageDone {
		t.Fatalf("isolated Stage C should use persisted proxy plan, got %#v", record)
	}
	overrideContent, err := os.ReadFile(overridePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(overrideContent), "entrypoint: []") {
		t.Fatalf("persisted override should be regenerated with proxy entrypoint override:\n%s", overrideContent)
	}
}

func TestStageCIsolatedCleansRunnerAfterRunCancel(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "batch", "TASK-1")
	repoPath := filepath.Join(projectPath, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, "run_tests.sh"), []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(repoPath, "compose.yml")
	if err := os.WriteFile(composePath, []byte("services:\n  web:\n    image: nginx\n    ports:\n      - \"8080:80\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(root, "artifacts")
	cfg := config.Default()
	cfg.Pipeline.StageC.Execution = "isolated"
	cfg.Pipeline.StageC.RunnerImage = "alpine:3.20"
	commandRunner := &cancelledStageCIsolatedRunner{}
	runner := pipelinepkg.NewRunner(nil, cfg, pipelinepkg.WithCommandRunner(commandRunner))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	record := runner.StageCForTest(ctx, model.RunRecord{
		RunID:        "run-1",
		TaskID:       "TASK-1",
		ArtifactRoot: artifactRoot,
	}, scanner.Project{TaskID: "TASK-1", Path: projectPath}, testRuntimeEvidence{
		ComposeProject: "p2rqa_task_run",
		ComposeFile:    composePath,
		ComposeFiles:   []string{composePath},
		WorkDir:        repoPath,
	}, nil)

	if record.Status != model.StageFailed {
		t.Fatalf("cancelled run should fail Stage C, got %#v", record)
	}
	for _, want := range []string{
		"rm -f p2r_stage_c_p2rqa_task_run",
		"--profile p2r-stage-c rm -sf p2r_stage_c_proxy",
	} {
		if !containsCommand(commandRunner.commands, want) {
			t.Fatalf("missing cleanup command %q in %#v", want, commandRunner.commands)
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
		"HTTP_PROXY=http://user:pass@proxy.example:8080",
		"HTTPS_PROXY=https://proxy.example:8443",
		"NO_PROXY=localhost,127.0.0.1",
		"http_proxy=http://lower.example:8080",
		"DOCKER_TOKEN=secret",
		"HTTPS_PROXY_PASSWORD=secret",
		"HOME=/home/test",
	}, nil, true)
	values := envMap(env)
	if values["DOCKER_HOST"] == "" || values["HOME"] == "" {
		t.Fatalf("docker env should keep Docker/HOME settings: %#v", env)
	}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy"} {
		if values[key] == "" {
			t.Fatalf("docker env should keep proxy %s: %#v", key, env)
		}
	}
	for _, key := range []string{"DOCKER_TOKEN", "HTTPS_PROXY_PASSWORD"} {
		if _, ok := values[key]; ok {
			t.Fatalf("docker env leaked %s: %#v", key, env)
		}
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
	writeSupplementalDoc(t, root, task.ID)
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

type stageCIsolatedRunner struct {
	commands []string
}

func (r *stageCIsolatedRunner) LookPath(name string) (string, error) {
	return name, nil
}

func (r *stageCIsolatedRunner) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
	return r.result(name, args...)
}

func (r *stageCIsolatedRunner) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput executor.OutputCallback, name string, args ...string) executor.Result {
	result := r.result(name, args...)
	if writer != nil {
		_, _ = writer.Write([]byte(result.Stdout + result.Stderr))
	}
	if onOutput != nil && strings.TrimSpace(result.Stdout) != "" {
		onOutput(strings.TrimSpace(result.Stdout), "stdout")
	}
	return result
}

func (r *stageCIsolatedRunner) result(name string, args ...string) executor.Result {
	command := strings.Join(append([]string{name}, args...), " ")
	r.commands = append(r.commands, command)
	return executor.Result{Command: command, Stdout: command + " ok\n"}
}

type cancelledStageCIsolatedRunner struct {
	commands []string
}

func (r *cancelledStageCIsolatedRunner) LookPath(name string) (string, error) {
	return name, nil
}

func (r *cancelledStageCIsolatedRunner) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
	command := strings.Join(append([]string{name}, args...), " ")
	r.commands = append(r.commands, command)
	if ctx.Err() != nil {
		return executor.Result{Command: command, Err: ctx.Err()}
	}
	return executor.Result{Command: command, Stdout: command + " ok\n"}
}

func (r *cancelledStageCIsolatedRunner) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput executor.OutputCallback, name string, args ...string) executor.Result {
	command := strings.Join(append([]string{name}, args...), " ")
	r.commands = append(r.commands, command)
	if strings.Contains(command, " run ") {
		return executor.Result{Command: command, Err: context.Canceled}
	}
	return executor.Result{Command: command, Stdout: command + " ok\n"}
}

func containsCommand(commands []string, needle string) bool {
	for _, command := range commands {
		if strings.Contains(command, needle) {
			return true
		}
	}
	return false
}
