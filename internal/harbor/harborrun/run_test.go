package harborrun

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/executor"
)

type fakeCommandRunner struct {
	result  executor.Result
	command string
}

func (f *fakeCommandRunner) LookPath(name string) (string, error) {
	return name, nil
}

func (f *fakeCommandRunner) Run(_ context.Context, _ time.Duration, _ string, _ []string, name string, args ...string) executor.Result {
	f.command = strings.Join(append([]string{name}, args...), " ")
	if f.result.Command == "" {
		f.result.Command = f.command
	}
	return f.result
}

func (f *fakeCommandRunner) RunStreamingWithOutput(context.Context, time.Duration, string, []string, io.Writer, executor.OutputCallback, string, ...string) executor.Result {
	return f.result
}

func TestRunParsesJSONStdoutAndWritesArtifacts(t *testing.T) {
	outputDir := t.TempDir()
	taskDir := writeHarborRunTask(t)
	exec := &fakeCommandRunner{result: executor.Result{
		Stdout: `{"model":"qwen3.7-max","runs":[{"trial":1,"passed":false,"turns":22,"reward":0,"failure_reason":"API_KEY=secret-token"},{"trial":2,"passed":true,"turns":24,"reward":1},{"trial":3,"passed":false,"turns":23,"reward":0},{"trial":4,"passed":false,"turns":23,"reward":0}]}`,
		Stderr: `ANTHROPIC_AUTH_TOKEN=secret-token`,
	}}
	result, commandRun, err := Run(context.Background(), Options{
		TaskPath: taskDir,
		Model:    "qwen3.7-max",
		AgentEnv: []string{
			"ANTHROPIC_MODEL=qwen3.7-max",
			"ANTHROPIC_AUTH_TOKEN=secret-token",
		},
		OutputDir: outputDir,
		Exec:      exec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Trials != 4 || result.PassCount != 1 || result.AverageTurns != 23 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.TaskDigest == "" || result.CommandRunPath == "" {
		t.Fatalf("result missing task digest or command path: %+v", result)
	}
	if !strings.Contains(exec.command, "harbor run -p "+taskDir+" -a claude-code -m qwen3.7-max -o "+filepath.Join(outputDir, "jobs")+" --ae ANTHROPIC_MODEL=qwen3.7-max --ae ANTHROPIC_AUTH_TOKEN=secret-token -n 4 -k 4") {
		t.Fatalf("unexpected command: %s", exec.command)
	}
	if !commandRun.Passed {
		t.Fatalf("command should pass: %+v", commandRun)
	}
	if strings.Contains(result.Runs[0].FailureReason, "secret-token") {
		t.Fatalf("trial result failure reason was not redacted: %+v", result.Runs[0])
	}
	if commandRun.StdoutPath == "" || commandRun.StderrPath == "" || len(commandRun.Argv) == 0 || commandRun.Attempt != 1 {
		t.Fatalf("command audit fields missing: %+v", commandRun)
	}
	if commandRun.Dir == "" || len(commandRun.Env) == 0 {
		t.Fatalf("command cwd/env missing: %+v", commandRun)
	}
	argv := strings.Join(commandRun.Argv, " ")
	if !strings.Contains(argv, "ANTHROPIC_MODEL=qwen3.7-max") || strings.Contains(argv, "secret-token") {
		t.Fatalf("unexpected argv redaction: %s", argv)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "command_run.json")); err != nil {
		t.Fatalf("missing command artifact: %v", err)
	}
	commandArtifact, err := os.ReadFile(filepath.Join(outputDir, "command_run.json"))
	if err != nil {
		t.Fatal(err)
	}
	trialArtifact, err := os.ReadFile(filepath.Join(outputDir, "trial_result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(commandArtifact), "secret-token") || strings.Contains(string(trialArtifact), "secret-token") || strings.Contains(commandRun.Command, "secret-token") || strings.Contains(commandRun.Stdout, "secret-token") || strings.Contains(commandRun.Stderr, "secret-token") {
		t.Fatalf("secret leaked into command artifacts: command=%s artifact=%s", commandRun.Command, commandArtifact)
	}
}

func TestRunParsesResultPathFromOutput(t *testing.T) {
	outputDir := t.TempDir()
	taskDir := writeHarborRunTask(t)
	resultPath := filepath.Join(outputDir, "result.json")
	if err := os.WriteFile(resultPath, []byte(`{"model":"opus","runs":[{"trial":1,"passed":true,"turns":28,"reward":1},{"trial":2,"passed":true,"turns":29,"reward":1},{"trial":3,"passed":true,"turns":27,"reward":1},{"trial":4,"passed":false,"turns":28,"reward":0}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := &fakeCommandRunner{result: executor.Result{
		Stdout: "job finished, result.json: " + resultPath + "\n",
	}}
	result, _, err := Run(context.Background(), Options{
		TaskPath:  taskDir,
		Model:     "opus",
		OutputDir: outputDir,
		Exec:      exec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ResultPath != filepath.Join(outputDir, "trial_result.json") || result.Trials != 4 || result.AverageTurns != 28 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, err := os.Stat(resultPath); err != nil {
		t.Fatalf("raw result path should still exist: %v", err)
	}
}

func TestRunRejectsErroredHarborJobResultWithZeroExit(t *testing.T) {
	outputDir := t.TempDir()
	taskDir := writeHarborRunTask(t)
	jobPath := writeHarborJobFixture(t, filepath.Join(outputDir, "jobs", "2026-07-10__11-27-45"), taskDir, "qwen3.7-max", []harborTrialFixture{
		{ExceptionType: "RuntimeError"},
	})
	exec := &fakeCommandRunner{result: executor.Result{
		Stdout: "Results written to " + jobPath + "\n",
	}}
	result, _, err := Run(context.Background(), Options{
		TaskPath:  taskDir,
		Model:     "qwen3.7-max",
		OutputDir: outputDir,
		Exec:      exec,
	})
	if err == nil {
		t.Fatalf("expected errored Harbor job to fail, got result %+v", result)
	}
	if !strings.Contains(err.Error(), "completion audit") || !strings.Contains(err.Error(), "errored") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func writeHarborRunTask(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"environment", "solution", "tests"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"instruction.md":         "Fix the task.\n",
		"task.toml":              "schema_version = \"1.3\"\n",
		"tests_analysis.md":      "## 1. instruction 和 environment 已提供的信息\n- instruction 和 environment describe the visible task.\n\n## 2. 模型的理论通过路径\n- The model can make the fix and run tests/test.sh.\n\n## 3. 模型具备通过条件的依据\n- The verifier checks visible behavior.\n",
		"environment/Dockerfile": "FROM alpine\n",
		"solution/solve.sh":      "#!/bin/sh\nexit 0\n",
		"tests/test.sh":          "#!/bin/sh\nexit 1\n",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
