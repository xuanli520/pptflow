package harborrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/executor"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/secretscan"
)

type fakeCommandRunner struct {
	result          executor.Result
	results         []executor.Result
	commands        []string
	streamingLines  []string
	streamingSource string
	onRun           func(call int)
	envs            [][]string
}

type cancelAwareCommandRunner struct {
	started chan struct{}
}

func (r *cancelAwareCommandRunner) LookPath(name string) (string, error) { return name, nil }

func (r *cancelAwareCommandRunner) Run(ctx context.Context, _ time.Duration, _ string, _ []string, name string, args ...string) executor.Result {
	close(r.started)
	<-ctx.Done()
	return executor.Result{Command: strings.Join(append([]string{name}, args...), " "), ExitCode: -1, Err: ctx.Err()}
}

func (r *cancelAwareCommandRunner) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, _ io.Writer, _ executor.OutputCallback, name string, args ...string) executor.Result {
	return r.Run(ctx, timeout, dir, env, name, args...)
}

func TestNormalizePassPlanDefaultsAndRejectsInvalidBounds(t *testing.T) {
	plan, err := NormalizePassPlan(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Concurrency != domain.DefaultHarborConcurrency || plan.Attempts != domain.RequiredTrialCount {
		t.Fatalf("unexpected default pass plan: %+v", plan)
	}
	for _, test := range []struct {
		name        string
		concurrency int
		attempts    int
	}{
		{name: "negative concurrency", concurrency: -1, attempts: 4},
		{name: "concurrency exceeds pass count", concurrency: 5, attempts: 4},
		{name: "attempt count is not pass at four", concurrency: 2, attempts: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NormalizePassPlan(test.concurrency, test.attempts); err == nil {
				t.Fatalf("NormalizePassPlan(%d, %d) unexpectedly succeeded", test.concurrency, test.attempts)
			}
		})
	}
	if plan, err := NormalizePassPlan(4, 4); err != nil || plan.Concurrency != 4 {
		t.Fatalf("maximum useful pass@4 concurrency rejected: plan=%+v err=%v", plan, err)
	}
}

func TestRunCancellationStopsParallelPassAndPreservesCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exec := &cancelAwareCommandRunner{started: make(chan struct{})}
	taskDir := writeHarborRunTask(t)
	outputDir := t.TempDir()
	type outcome struct {
		command domain.CommandRun
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		_, command, err := Run(ctx, Options{
			TaskPath: taskDir, Model: "qwen", Agent: "test-agent",
			OutputDir: outputDir, Concurrency: 2, Attempts: 4, Exec: exec,
		})
		done <- outcome{command: command, err: err}
	}()
	select {
	case <-exec.started:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("Harbor pass did not start")
	}
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("cancellation cause was not preserved: %v", got.err)
		}
		if got.command.Passed || got.command.ExitCode != -1 {
			t.Fatalf("canceled command evidence is incorrect: %+v", got.command)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Harbor pass did not stop after cancellation")
	}
}

func TestRunRejectsDuplicateParallelTrialResults(t *testing.T) {
	exec := &fakeCommandRunner{result: executor.Result{Stdout: `{
		"model":"qwen",
		"runs":[{"trial":1},{"trial":2},{"trial":2},{"trial":4}]
	}`}}
	_, _, err := Run(context.Background(), Options{
		TaskPath: writeHarborRunTask(t), Model: "qwen", Agent: "test-agent",
		OutputDir: t.TempDir(), Concurrency: 2, Attempts: 4, Exec: exec,
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate trial 2") {
		t.Fatalf("duplicate parallel result was not rejected: %v", err)
	}
}

func (f *fakeCommandRunner) LookPath(name string) (string, error) {
	return name, nil
}

func (f *fakeCommandRunner) Run(_ context.Context, _ time.Duration, _ string, env []string, name string, args ...string) executor.Result {
	f.envs = append(f.envs, append([]string(nil), env...))
	return f.next(strings.Join(append([]string{name}, args...), " "))
}

func (f *fakeCommandRunner) next(command string) executor.Result {
	call := len(f.commands)
	f.commands = append(f.commands, command)
	if f.onRun != nil {
		f.onRun(call)
	}
	result := f.result
	if call < len(f.results) {
		result = f.results[call]
	}
	if result.Command == "" {
		result.Command = command
	}
	return result
}

func (f *fakeCommandRunner) RunStreamingWithOutput(_ context.Context, _ time.Duration, _ string, env []string, output io.Writer, callback executor.OutputCallback, name string, args ...string) executor.Result {
	f.envs = append(f.envs, append([]string(nil), env...))
	for _, line := range f.streamingLines {
		_, _ = io.WriteString(output, line+"\n")
		callback(line, f.streamingSource)
	}
	return f.next(strings.Join(append([]string{name}, args...), " "))
}

func TestRunParsesJSONStdoutAndWritesArtifacts(t *testing.T) {
	outputDir := t.TempDir()
	taskDir := writeHarborRunTask(t)
	exec := &fakeCommandRunner{result: executor.Result{
		Stdout: `{"model":"qwen3.7-max","runs":[{"trial":1,"passed":false,"turns":22,"reward":0},{"trial":2,"passed":true,"turns":24,"reward":1},{"trial":3,"passed":false,"turns":23,"reward":0},{"trial":4,"passed":false,"turns":23,"reward":0}]}`,
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
	if len(exec.commands) != 1 || !strings.Contains(exec.commands[0], "harbor run -p "+taskDir+" -a "+retryingClaudeImportPath+" -m qwen3.7-max -o "+filepath.Join(outputDir, "jobs")+" --ae ANTHROPIC_MODEL=qwen3.7-max --ae ANTHROPIC_AUTH_TOKEN=${ANTHROPIC_AUTH_TOKEN} --yes -n 2 -k 4") || strings.Contains(exec.commands[0], "secret-token") {
		t.Fatalf("unexpected commands: %v", exec.commands)
	}
	if len(exec.envs) != 1 || !envContains(exec.envs[0], "ANTHROPIC_AUTH_TOKEN=secret-token") || !envKeyHasPath(exec.envs[0], "PYTHONPATH", filepath.Join(outputDir, ".factory-agent")) || !envContains(exec.envs[0], "HARBOR_FACTORY_INSTALL_ATTEMPTS=1") || !envContains(exec.envs[0], "HARBOR_FACTORY_NPM_FETCH_RETRIES=0") {
		t.Fatalf("secret was not delivered through the Harbor process environment: %+v", exec.envs)
	}
	if result.Agent != "claude-code" {
		t.Fatalf("retry shim must preserve the public agent identity: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(outputDir, ".factory-agent", "harbor_factory_retrying_claude.py")); err != nil {
		t.Fatalf("retrying Claude agent shim missing: %v", err)
	}
	shim, err := os.ReadFile(filepath.Join(outputDir, ".factory-agent", "harbor_factory_retrying_claude.py"))
	if err != nil {
		t.Fatal(err)
	}
	shimText := string(shim)
	for _, required := range []string{"_ProtectedEnvironment", "stdin_data=payload", "self._extra_env.pop", "set -a; ."} {
		if !strings.Contains(shimText, required) {
			t.Fatalf("retrying Claude shim missing secret-safe environment behavior %q", required)
		}
	}
	if findings := secretscan.ScanBytes("harbor_factory_retrying_claude.py", shim); len(findings) != 0 {
		t.Fatalf("retrying Claude shim triggers workspace secret scan: %+v", findings)
	}
	if !commandRun.Passed {
		t.Fatalf("command should pass: %+v", commandRun)
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

func envContains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func envKeyHasPath(values []string, key, expectedPath string) bool {
	for _, value := range values {
		itemKey, itemValue, ok := strings.Cut(value, "=")
		if !ok || itemKey != key {
			continue
		}
		for _, path := range filepath.SplitList(itemValue) {
			if path == expectedPath {
				return true
			}
		}
	}
	return false
}

func TestRunPreflightsAndUsesConfiguredPassAt4Settings(t *testing.T) {
	outputDir := t.TempDir()
	taskDir := writeHarborRunTask(t)
	exec := &fakeCommandRunner{results: []executor.Result{
		{},
		{},
		{Stdout: `{"model":"qwen","runs":[{"trial":1,"turns":20},{"trial":2,"turns":21},{"trial":3,"turns":22},{"trial":4,"turns":23}]}`},
	}}
	exec.onRun = func(call int) {
		if call == 1 {
			writeHarborJobFixture(t, filepath.Join(outputDir, "preflight", "install", "attempt-01", "jobs", "install-ok"), taskDir, "qwen", []harborTrialFixture{{}})
		}
	}
	result, _, err := Run(context.Background(), Options{
		TaskPath:            taskDir,
		Model:               "qwen",
		OutputDir:           outputDir,
		Preflight:           true,
		SetupTimeoutSeconds: 321,
		Concurrency:         2,
		Attempts:            4,
		InfraRetries:        3,
		Exec:                exec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(exec.commands) != 3 {
		t.Fatalf("expected schema preflight, install preflight, and main run, got %v", exec.commands)
	}
	if !strings.Contains(exec.commands[0], "--print-config --yes -n 1 -k 1") {
		t.Fatalf("unexpected schema preflight: %s", exec.commands[0])
	}
	if !strings.Contains(exec.commands[1], "--install-only --yes -n 1 -k 1") {
		t.Fatalf("unexpected install preflight: %s", exec.commands[1])
	}
	if !strings.Contains(exec.commands[2], "--yes -n 2 -k 4 --max-retries 3 --retry-include RuntimeError --retry-include NetworkConnectionError --retry-include NonZeroAgentExitCodeError --retry-include ApiRateLimitError --retry-include ApiInternalServerError --retry-include ApiOverloadedError --retry-include ApiConnectionClosedError --retry-include UnknownApiError") {
		t.Fatalf("unexpected main run: %s", exec.commands[2])
	}
	for index, env := range exec.envs {
		if !envContains(env, "HARBOR_FACTORY_INSTALL_ATTEMPTS=4") || !envContains(env, "HARBOR_FACTORY_NPM_FETCH_RETRIES=3") {
			t.Fatalf("command %d did not receive the unified retry budget: %+v", index, env)
		}
	}
	if result.SchemaPreflightPath == "" || result.PreflightRunPath == "" || result.PreflightResultPath == "" {
		t.Fatalf("preflight audit path missing: %+v", result)
	}
}

func TestRunRejectsErroredInstallPreflightWithZeroExit(t *testing.T) {
	outputDir := t.TempDir()
	taskDir := writeHarborRunTask(t)
	exec := &fakeCommandRunner{results: []executor.Result{{}, {}}}
	exec.onRun = func(call int) {
		if call == 1 {
			writeHarborJobFixture(t, filepath.Join(outputDir, "preflight", "install", "attempt-01", "jobs", "install-error"), taskDir, "qwen", []harborTrialFixture{{ExceptionType: "RuntimeError"}})
		}
	}
	result, _, err := Run(context.Background(), Options{TaskPath: taskDir, Model: "qwen", OutputDir: outputDir, Preflight: true, Exec: exec})
	if err == nil || !strings.Contains(err.Error(), "install preflight failed") || !strings.Contains(err.Error(), "RuntimeError") {
		t.Fatalf("expected zero-exit install error to block main run, result=%+v err=%v", result, err)
	}
	if len(exec.commands) != 2 || result.PreflightResultPath == "" {
		t.Fatalf("main pass@4 should not start and preflight evidence must remain: commands=%v result=%+v", exec.commands, result)
	}
}

func TestRunRetriesTransientInstallPreflightBeforeMainRun(t *testing.T) {
	outputDir := t.TempDir()
	taskDir := writeHarborRunTask(t)
	exec := &fakeCommandRunner{results: []executor.Result{
		{}, {}, {},
		{Stdout: `{"model":"qwen","runs":[{"trial":1},{"trial":2},{"trial":3},{"trial":4}]}`},
	}}
	exec.onRun = func(call int) {
		switch call {
		case 1:
			writeHarborJobFixture(t, filepath.Join(outputDir, "preflight", "install", "attempt-01", "jobs", "install-error"), taskDir, "qwen", []harborTrialFixture{{ExceptionType: "NetworkConnectionError"}})
		case 2:
			writeHarborJobFixture(t, filepath.Join(outputDir, "preflight", "install", "attempt-02", "jobs", "install-ok"), taskDir, "qwen", []harborTrialFixture{{}})
		}
	}
	result, _, err := Run(context.Background(), Options{TaskPath: taskDir, Model: "qwen", OutputDir: outputDir, Preflight: true, InfraRetries: 1, Exec: exec})
	if err != nil {
		t.Fatal(err)
	}
	if len(exec.commands) != 4 || !strings.Contains(result.PreflightRunPath, "attempt-02") || !strings.Contains(result.PreflightResultPath, "attempt-02") {
		t.Fatalf("preflight retry evidence missing: commands=%v result=%+v", exec.commands, result)
	}
}

func TestRunRetainsPartialJobResultAndCombinesCommandFailure(t *testing.T) {
	outputDir := t.TempDir()
	taskDir := writeHarborRunTask(t)
	exec := &fakeCommandRunner{result: executor.Result{Err: errors.New("deadline exceeded"), Timeout: true, ExitCode: -1}}
	exec.onRun = func(_ int) {
		writeHarborJobFixture(t, filepath.Join(outputDir, "jobs", "partial-job"), taskDir, "qwen", []harborTrialFixture{
			{ExceptionType: "RuntimeError"},
		})
	}
	result, commandRun, err := Run(context.Background(), Options{
		TaskPath:  taskDir,
		Model:     "qwen",
		OutputDir: outputDir,
		Exec:      exec,
	})
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") || !strings.Contains(err.Error(), "completion audit") {
		t.Fatalf("expected combined failure, got %v", err)
	}
	if result.ResultPath == "" || !commandRun.Timeout {
		t.Fatalf("partial evidence was not retained: result=%+v command=%+v", result, commandRun)
	}
	if result.CreatedAt.IsZero() {
		t.Fatalf("partial evidence has a zero creation timestamp: %+v", result)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "trial_result.json")); statErr != nil {
		t.Fatalf("missing normalized partial result: %v", statErr)
	}
}

func TestRunStreamsRedactedProgress(t *testing.T) {
	outputDir := t.TempDir()
	taskDir := writeHarborRunTask(t)
	exec := &fakeCommandRunner{
		result:          executor.Result{Stdout: `{"model":"qwen","runs":[{"trial":1},{"trial":2},{"trial":3},{"trial":4}]}`},
		streamingLines:  []string{"building environment", "ANTHROPIC_AUTH_TOKEN=secret-token"},
		streamingSource: "stderr",
	}
	var progress []string
	_, _, err := Run(context.Background(), Options{
		TaskPath:  taskDir,
		Model:     "qwen",
		OutputDir: outputDir,
		Progress: func(line, source string) {
			progress = append(progress, source+":"+line)
		},
		Exec: exec,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(progress, "\n")
	if len(progress) != 2 || strings.Contains(joined, "secret-token") || !strings.Contains(joined, "<redacted>") {
		t.Fatalf("unexpected progress: %q", joined)
	}
	live, readErr := os.ReadFile(filepath.Join(outputDir, "live.log"))
	if readErr != nil || !strings.Contains(string(live), "building environment") || strings.Contains(string(live), "secret-token") {
		t.Fatalf("live log missing: %v %q", readErr, live)
	}
}

func TestReadJobProgressReportsTrialLifecycle(t *testing.T) {
	jobsDir := t.TempDir()
	jobDir := filepath.Join(jobsDir, "2026-07-11__12-00-00")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"n_total_trials":4,"stats":{"n_completed_trials":2,"n_errored_trials":1,"n_running_trials":1,"n_pending_trials":0,"n_cancelled_trials":0,"n_retries":2}}`)
	if err := os.WriteFile(filepath.Join(jobDir, "result.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	progress, ok := readJobProgress(jobsDir)
	if !ok || progress.Total != 4 || progress.Completed != 2 || progress.Errored != 1 || progress.Running != 1 || progress.Retries != 2 {
		t.Fatalf("unexpected job progress: ok=%v progress=%+v", ok, progress)
	}
}

func TestReadLatestJobLogLineReturnsNewestNonEmptyLine(t *testing.T) {
	jobsDir := t.TempDir()
	jobDir := filepath.Join(jobsDir, "2026-07-11__12-00-00")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "job.log"), []byte("building environment\ntrial 2 running\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	line, ok := readLatestJobLogLine(jobsDir)
	if !ok || line != "trial 2 running" {
		t.Fatalf("unexpected job log tail: ok=%v line=%q", ok, line)
	}
}

func TestJobMonitorSnapshotsRetryEvidenceAndPromotesAPIRetry(t *testing.T) {
	outputDir := t.TempDir()
	jobsDir := filepath.Join(outputDir, "jobs")
	trial := "task__retry123"
	trialDir := filepath.Join(jobsDir, "2026-07-11__12-00-00", trial)
	if err := os.MkdirAll(filepath.Join(trialDir, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trialDir, "exception.txt"), []byte("UnknownApiError\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trialDir, "trial.log"), []byte("ANTHROPIC_AUTH_TOKEN=unit-test-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agentLog := strings.Join([]string{
		`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":517.6,"error_status":524,"error":"response sk-unit-test-secret","uuid":"retry-1"}`,
		`{"type":"system","subtype":"api_retry","attempt":2,"max_retries":10,"retry_delay_ms":1100.2,"error_status":"overloaded","error":"response body must stay private","uuid":"retry-2"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(trialDir, "agent", "claude-code.txt"), []byte(agentLog), 0o600); err != nil {
		t.Fatal(err)
	}
	jobLog := fmt.Sprintf("Trial %s failed with exception UnknownApiError. Retrying in 1.00 seconds...\n", trial)
	if err := os.WriteFile(filepath.Join(filepath.Dir(trialDir), "job.log"), []byte(jobLog), 0o600); err != nil {
		t.Fatal(err)
	}
	var progress []string
	monitor := newJobMonitor(jobsDir, outputDir, func(line, source string) {
		progress = append(progress, source+":"+line)
	})
	monitor.poll(true)

	manifestPath := monitor.retryEvidenceManifestPath()
	if manifestPath == "" {
		t.Fatal("retry evidence manifest was not produced")
	}
	manifestRaw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest retryEvidenceManifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 1 || manifest.Entries[0].Trial != trial || manifest.Entries[0].Retry != 1 || len(manifest.Entries[0].Files) != 3 {
		t.Fatalf("unexpected retry evidence manifest: %+v", manifest)
	}
	snapshot, err := os.ReadFile(filepath.Join(outputDir, "retry_evidence", trial, "retry-01", "trial.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(snapshot), "unit-test-secret") || !strings.Contains(string(snapshot), "<redacted>") {
		t.Fatalf("retry snapshot was not redacted: %s", snapshot)
	}
	if findings, err := secretscan.ScanDir(filepath.Join(outputDir, "retry_evidence")); err != nil || len(findings) != 0 {
		t.Fatalf("retry evidence secret scan failed: err=%v findings=%+v", err, findings)
	}
	joined := strings.Join(progress, "\n")
	if strings.Count(joined, "harbor-api-retry:") != 1 || !strings.Contains(joined, "attempt=2/10") || !strings.Contains(joined, "delay_ms=1100") || !strings.Contains(joined, "status=overloaded") {
		t.Fatalf("API retries were not coalesced into structured progress: %s", joined)
	}
	if strings.Contains(joined, "response body") || strings.Contains(joined, "unit-test-secret") || strings.Contains(joined, "sk-") {
		t.Fatalf("API retry progress leaked raw error details: %s", joined)
	}
}

func TestRunCarriesRetryEvidenceManifestWithoutProgressCallback(t *testing.T) {
	outputDir := t.TempDir()
	taskDir := writeHarborRunTask(t)
	trial := "task__retry456"
	exec := &fakeCommandRunner{result: executor.Result{Stdout: `{"model":"qwen","runs":[{"trial":1},{"trial":2},{"trial":3},{"trial":4}]}`}}
	exec.onRun = func(_ int) {
		trialDir := filepath.Join(outputDir, "jobs", "2026-07-11__12-00-00", trial)
		if err := os.MkdirAll(filepath.Join(trialDir, "agent"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(trialDir, "exception.txt"), []byte("UnknownApiError\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(trialDir, "agent", "claude-code.txt"), []byte("retry log\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		line := fmt.Sprintf("Trial %s failed with exception UnknownApiError. Retrying in 1.00 seconds...\n", trial)
		if err := os.WriteFile(filepath.Join(filepath.Dir(trialDir), "job.log"), []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result, _, err := Run(context.Background(), Options{TaskPath: taskDir, Model: "qwen", OutputDir: outputDir, Exec: exec})
	if err != nil {
		t.Fatal(err)
	}
	if result.RetryEvidence == "" {
		t.Fatalf("normalized result missing retry evidence path: %+v", result)
	}
	if _, err := os.Stat(result.RetryEvidence); err != nil {
		t.Fatalf("retry evidence manifest missing: %v", err)
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
