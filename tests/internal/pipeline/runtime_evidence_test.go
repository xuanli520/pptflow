package pipeline_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
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
