package verify

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/executor"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

func TestRunBuildInitialFailOraclePass(t *testing.T) {
	taskDir := writeVerifyTask(t)
	fake := &fakeExec{results: []executor.Result{
		{Command: "docker build", ExitCode: 0},
		{Command: "docker run initial", ExitCode: 1, Err: errors.New("test failed")},
		{Command: "docker run oracle", ExitCode: 0},
		{Command: "docker image rm", ExitCode: 0},
	}}
	workspace := t.TempDir()
	reportPath := filepath.Join(workspace, "verify_report.json")
	report, err := Run(context.Background(), Options{TaskDir: taskDir, Workspace: workspace, Exec: fake, WriteReport: reportPath})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || !report.InitialExposesIssue || report.OracleVerify == nil || !report.OracleVerify.Passed {
		t.Fatalf("report = %+v", report)
	}
	if report.TaskDigest == "" {
		t.Fatalf("report missing task digest: %+v", report)
	}
	if report.Cleanup == nil || !report.Cleanup.Passed {
		t.Fatalf("cleanup missing: %+v", report)
	}
	if len(report.CommandLogs) != 4 {
		t.Fatalf("command logs = %+v", report.CommandLogs)
	}
	for _, run := range report.CommandLogs {
		if run.Dir == "" || len(run.Env) == 0 || len(run.Argv) == 0 || run.Attempt != 1 {
			t.Fatalf("missing command audit fields: %+v", run)
		}
		if run.StdoutPath == "" || run.StderrPath == "" {
			t.Fatalf("missing output paths: %+v", run)
		}
		if _, err := os.Stat(run.StdoutPath); err != nil {
			t.Fatalf("missing stdout artifact %s: %v", run.StdoutPath, err)
		}
	}
	if _, err := os.Stat(reportPath); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"phase2/artifacts/docker_build/build_result.json",
		"phase2/artifacts/initial_verify/initial_result.json",
		"phase2/artifacts/oracle_verify/oracle_result.json",
	} {
		if _, err := os.Stat(filepath.Join(workspace, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("missing split verify artifact %s: %v", rel, err)
		}
	}
	if len(fake.commands) != 4 {
		t.Fatalf("commands = %v", fake.commands)
	}
	if !strings.Contains(fake.commands[0], "docker build") {
		t.Fatalf("build command = %s", fake.commands[0])
	}
	if !strings.Contains(fake.commands[3], "docker image rm -f") {
		t.Fatalf("cleanup command = %s", fake.commands[3])
	}
}

func TestWriteReportRedactsSecretLikeFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verify_report.json")
	report := domain.VerifyReport{
		SchemaVersion: "harbor.verify_report.v1",
		TaskDir:       "/tmp/OPENAI_API_KEY=raw-report-secret/task",
		ImageTag:      "Bearer raw-report-secret",
		CommandLogs: []domain.CommandRun{{
			Name:   "docker_build",
			Stdout: "OPENAI_API_KEY=raw-report-secret",
		}},
	}
	if err := writeReport(report, path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "raw-report-secret") {
		t.Fatalf("secret leaked in verify report: %s", raw)
	}
}

func TestRunFailsWhenInitialVerifyPasses(t *testing.T) {
	taskDir := writeVerifyTask(t)
	fake := &fakeExec{results: []executor.Result{
		{Command: "docker build", ExitCode: 0},
		{Command: "docker run initial", ExitCode: 0},
	}}
	report, err := Run(context.Background(), Options{TaskDir: taskDir, Exec: fake})
	if err == nil {
		t.Fatal("expected error")
	}
	if report.Passed || report.InitialExposesIssue {
		t.Fatalf("report = %+v", report)
	}
}

func TestRunDoesNotCountInfraInitialFailuresAsIssueExposure(t *testing.T) {
	cases := map[string]executor.Result{
		"timeout": {
			Command:  "docker run initial",
			ExitCode: -1,
			Timeout:  true,
			Err:      errors.New("timeout"),
		},
		"missing_tool": {
			Command:  "docker run initial",
			ExitCode: 127,
			Stderr:   "executable file not found",
			Err:      errors.New("missing"),
		},
	}
	for name, initial := range cases {
		t.Run(name, func(t *testing.T) {
			taskDir := writeVerifyTask(t)
			fake := &fakeExec{results: []executor.Result{
				{Command: "docker build", ExitCode: 0},
				initial,
				{Command: "docker image rm", ExitCode: 0},
			}}
			report, err := Run(context.Background(), Options{TaskDir: taskDir, Exec: fake})
			if err == nil {
				t.Fatal("expected error")
			}
			if report.Passed || report.InitialExposesIssue || report.InitialVerify == nil || report.InitialVerify.Passed {
				t.Fatalf("infra failure should not expose task issue: %+v", report)
			}
		})
	}
}

func TestRunComposeBuildInitialFailOraclePass(t *testing.T) {
	taskDir := writeVerifyComposeTask(t)
	fake := &fakeExec{results: []executor.Result{
		{Command: "docker compose build", ExitCode: 0},
		{Command: "docker compose run initial", ExitCode: 1, Err: errors.New("test failed")},
		{Command: "docker compose run oracle", ExitCode: 0},
		{Command: "docker compose down", ExitCode: 0},
	}}
	report, err := Run(context.Background(), Options{TaskDir: taskDir, Exec: fake})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || !report.InitialExposesIssue || report.OracleVerify == nil || !report.OracleVerify.Passed {
		t.Fatalf("report = %+v", report)
	}
	if report.Cleanup == nil || !report.Cleanup.Passed || len(report.CommandLogs) != 4 {
		t.Fatalf("cleanup/command logs missing: %+v", report)
	}
	if len(fake.commands) != 4 {
		t.Fatalf("commands = %v", fake.commands)
	}
	if !strings.Contains(fake.commands[0], "docker compose") || !strings.Contains(fake.commands[0], "build main") {
		t.Fatalf("compose build command = %s", fake.commands[0])
	}
	if !strings.Contains(fake.commands[2], "/solution/solve.sh && /tests/test.sh") {
		t.Fatalf("oracle command = %s", fake.commands[2])
	}
}

type fakeExec struct {
	results  []executor.Result
	commands []string
}

func (f *fakeExec) LookPath(name string) (string, error) {
	return name, nil
}

func (f *fakeExec) Run(_ context.Context, _ time.Duration, dir string, _ []string, name string, args ...string) executor.Result {
	f.commands = append(f.commands, strings.Join(append([]string{name}, args...), " "))
	if len(f.results) == 0 {
		return executor.Result{Command: name, ExitCode: 0}
	}
	result := f.results[0]
	f.results = f.results[1:]
	if result.Command == "" {
		result.Command = strings.Join(append([]string{name}, args...), " ")
	}
	return result
}

func (f *fakeExec) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput executor.OutputCallback, name string, args ...string) executor.Result {
	return f.Run(ctx, timeout, dir, env, name, args...)
}

func writeVerifyTask(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"environment", "solution", "tests"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("instruction.md", "Fix the task.\n")
	write("task.toml", "schema_version = \"1.3\"\n")
	write("tests_analysis.md", "## 1. instruction 和 environment 已提供的信息\n- instruction 和 environment describe the visible task.\n\n## 2. 模型的理论通过路径\n- The model can make the fix and run tests/test.sh.\n\n## 3. 模型具备通过条件的依据\n- The verifier checks visible behavior.\n")
	write(filepath.Join("environment", "Dockerfile"), "FROM alpine\n")
	write(filepath.Join("solution", "solve.sh"), "#!/bin/sh\nexit 0\n")
	write(filepath.Join("tests", "test.sh"), "#!/bin/sh\nexit 1\n")
	return root
}

func writeVerifyComposeTask(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"environment", "solution", "tests"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("instruction.md", "Fix the task.\n")
	write("task.toml", "schema_version = \"1.3\"\n")
	write("tests_analysis.md", "## 1. instruction 和 environment 已提供的信息\n- instruction 和 environment describe the visible task.\n\n## 2. 模型的理论通过路径\n- The model can make the fix and run tests/test.sh.\n\n## 3. 模型具备通过条件的依据\n- The verifier checks visible behavior.\n")
	write(filepath.Join("environment", "docker-compose.yaml"), "services:\n  main:\n    build:\n      context: ./environment\n")
	write(filepath.Join("solution", "solve.sh"), "#!/bin/sh\nexit 0\n")
	write(filepath.Join("tests", "test.sh"), "#!/bin/sh\nexit 1\n")
	return root
}
