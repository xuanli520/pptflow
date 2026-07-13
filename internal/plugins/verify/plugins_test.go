package verify

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

type fakeCommandRuntime struct {
	requests []workflow.CommandRequest
	results  []workflow.CommandResult
	errors   []error
}

func (r *fakeCommandRuntime) Run(_ context.Context, request workflow.CommandRequest) (workflow.CommandResult, error) {
	r.requests = append(r.requests, request)
	result := r.results[0]
	r.results = r.results[1:]
	var err error
	if len(r.errors) > 0 {
		err = r.errors[0]
		r.errors = r.errors[1:]
	}
	return result, err
}

type fakeEvaluationRuntime struct {
	request workflow.EvaluationRequest
	result  workflow.EvaluationResult
	err     error
}

func (r *fakeEvaluationRuntime) Evaluate(_ context.Context, request workflow.EvaluationRequest) (workflow.EvaluationResult, error) {
	r.request = request
	return r.result, r.err
}

func TestCommandPluginsExecuteOneStepAndStoreEvidence(t *testing.T) {
	taskDir := writeTask(t)
	store := newStore(t)
	command := &fakeCommandRuntime{
		results: []workflow.CommandResult{
			{Command: "docker build", ExitCode: 0, Stdout: "built"},
			{Command: "docker run", ExitCode: 1, Stderr: "behavioral assertion failed"},
			{Command: "docker run", ExitCode: 0, Stdout: "oracle passed"},
		},
		errors: []error{nil, errors.New("exit status 1"), nil},
	}
	plugins := []workflow.Plugin{DockerBuildPlugin{}, InitialVerifyPlugin{}, OracleVerifyPlugin{}}
	ids := []string{"docker_build", "initial_verify", "oracle_verify"}
	prior := map[string]workflow.NodeRun{}
	for idx, plugin := range plugins {
		spec := workflow.NodeSpec{ID: ids[idx], Kind: plugin.Manifest().Kinds[0], Config: map[string]any{"task_dir": taskDir, "image_tag": "test-image", "timeout_seconds": 30}}
		result, err := plugin.Execute(context.Background(), workflow.NodeRequest{RunID: "run-1", Spec: spec, Attempt: 1, Store: store, Prior: prior, Runtimes: workflow.Runtimes{Command: command}})
		if err != nil {
			t.Fatalf("%s failed: %v", ids[idx], err)
		}
		wantArtifacts := 3
		if ids[idx] == "oracle_verify" {
			wantArtifacts = 10
		}
		if len(result.Artifacts) != wantArtifacts || result.Artifacts[0].Type != "command_run" {
			t.Fatalf("%s evidence mismatch: %+v", ids[idx], result.Artifacts)
		}
		prior[ids[idx]] = workflow.NodeRun{NodeID: ids[idx], Status: workflow.NodeSucceeded, Artifacts: result.Artifacts}
	}
	if len(command.requests) != 3 {
		t.Fatalf("plugins performed unexpected sub-orchestration: %d commands", len(command.requests))
	}
	if !containsArg(command.requests[1].Args, "/tests/test.sh") || !containsArg(command.requests[2].Args, "/solution/solve.sh && /tests/test.sh") {
		t.Fatalf("verification commands lost their contract: %+v", command.requests)
	}
}

func TestHarborPluginUsesEvaluationRuntimeAndRequiresExactFourRuns(t *testing.T) {
	store := newStore(t)
	evaluation := &fakeEvaluationRuntime{result: validEvaluation("qwen-model")}
	spec := workflow.NodeSpec{ID: "harbor_run_qwen", Kind: HarborRunQwenKind, Config: map[string]any{
		"task_dir": "/task", "model": "qwen-model", "agent": "claude-code", "agent_env": []string{"API_KEY=${API_KEY}"}, "concurrency": 2,
	}}
	result, err := (HarborRunQwenPlugin{}).Execute(context.Background(), workflow.NodeRequest{RunID: "run-1", Spec: spec, Store: store, Runtimes: workflow.Runtimes{Evaluation: evaluation}})
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.request.Attempts != 4 || evaluation.request.Model != "qwen-model" || evaluation.request.Concurrency != 2 {
		t.Fatalf("Harbor routing/request mismatch: %+v", evaluation.request)
	}
	if len(result.Artifacts) != 6 || result.Artifacts[0].Type != "trial_result" || result.Artifacts[1].Type != "trial_result" || result.Artifacts[2].Type != "pass4_screenshot" || result.Artifacts[3].Type != "command_run" {
		t.Fatalf("Harbor evidence mismatch: %+v", result.Artifacts)
	}
	var stored domain.TrialResult
	storedRaw, readErr := os.ReadFile(result.Artifacts[0].Path)
	if readErr != nil || json.Unmarshal(storedRaw, &stored) != nil {
		t.Fatalf("read stored Harbor result: %v", readErr)
	}
	if stored.Screenshot != filepath.Base(result.Artifacts[2].Path) {
		t.Fatalf("stored result does not point at generated screenshot: %+v", stored)
	}

	evaluation.result.TrialResult.Runs = evaluation.result.TrialResult.Runs[:3]
	failed, err := (HarborRunQwenPlugin{}).Execute(context.Background(), workflow.NodeRequest{RunID: "run-2", Spec: spec, Store: store, Runtimes: workflow.Runtimes{Evaluation: evaluation}})
	if err == nil || len(failed.Artifacts) != 6 {
		t.Fatalf("invalid four-run result should fail after storing evidence: %+v, %v", failed, err)
	}
	var classified workflow.ClassifiedError
	if !errors.As(err, &classified) || classified.FailureKind() != workflow.FailureTransient || !classified.Retryable() {
		t.Fatalf("generated Harbor result validation must be retryable: %T %v", err, err)
	}
}

func TestHarborPluginRetryInvalidatesAndReplacesOldEvidence(t *testing.T) {
	store := newStore(t)
	evaluation := &fakeEvaluationRuntime{result: validEvaluation("qwen-model")}
	spec := workflow.NodeSpec{ID: "harbor_run_qwen", Kind: HarborRunQwenKind, Config: map[string]any{
		"task_dir": "/task", "model": "qwen-model", "agent": "claude-code",
	}}
	first, err := (HarborRunQwenPlugin{}).Execute(context.Background(), workflow.NodeRequest{
		RunID: "retry-run", Spec: spec, Store: store, Runtimes: workflow.Runtimes{Evaluation: evaluation},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstScreenshotDigest := first.Artifacts[2].SHA256
	if _, err := store.PutText(context.Background(), "phase3/artifacts/harbor_run_qwen/obsolete-attempt.txt", "debug", spec.ID, "stale"); err != nil {
		t.Fatal(err)
	}
	evaluation.result.TrialResult.Runs[0].Passed = false
	evaluation.result.TrialResult.PassCount = 0
	evaluation.result.TrialResult.PassAt4 = 0
	second, err := (HarborRunQwenPlugin{}).Execute(context.Background(), workflow.NodeRequest{
		RunID: "retry-run", Spec: spec, Store: store, Runtimes: workflow.Runtimes{Evaluation: evaluation},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Artifacts[2].SHA256 == firstScreenshotDigest {
		t.Fatal("retry preserved the old screenshot content")
	}
	refs, err := store.List(context.Background(), "phase3/artifacts/harbor_run_qwen/")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 6 {
		t.Fatalf("retry left stale or duplicate artifacts: %+v", refs)
	}
	for _, ref := range refs {
		if strings.Contains(ref.Name, "obsolete-attempt") {
			t.Fatalf("retry did not invalidate old attempt evidence: %+v", refs)
		}
	}
}

func TestHarborPluginRejectsInvalidPassConcurrencyBeforeEvaluation(t *testing.T) {
	store := newStore(t)
	evaluation := &fakeEvaluationRuntime{result: validEvaluation("qwen-model")}
	spec := workflow.NodeSpec{ID: "harbor_run_qwen", Kind: HarborRunQwenKind, Config: map[string]any{
		"task_dir": "/task", "model": "qwen-model", "concurrency": 5, "attempts": 4,
	}}
	if err := (HarborRunQwenPlugin{}).Validate(spec); err == nil {
		t.Fatal("plugin validation accepted concurrency greater than the pass@4 trial count")
	}
	_, err := (HarborRunQwenPlugin{}).Execute(context.Background(), workflow.NodeRequest{
		RunID: "run-1", Spec: spec, Store: store, Runtimes: workflow.Runtimes{Evaluation: evaluation},
	})
	if err == nil || evaluation.request.Model != "" {
		t.Fatalf("invalid plan reached evaluation runtime: request=%+v err=%v", evaluation.request, err)
	}
}

func TestHarborPluginImportsStrictExistingEvidenceWithoutEvaluation(t *testing.T) {
	taskDir := writeTask(t)
	digest, err := harborrun.ComputeTaskDigest(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	evidenceDir := t.TempDir()
	stdoutPath := filepath.Join(evidenceDir, "stdout.txt")
	stderrPath := filepath.Join(evidenceDir, "stderr.txt")
	rawResultPath := filepath.Join(evidenceDir, "raw_result.json")
	rawTrial := domain.TrialResult{
		SchemaVersion: "harbor.trial_result.v1", Model: "qwen3.7-max", Agent: "claude-code",
		Trials: 4, PassCount: 1, PassAt4: .25, AverageTurns: 23, TaskDigest: digest, TaskPath: taskDir,
		Runs: []domain.TrialRun{{Trial: 1, Passed: true, Turns: 22}, {Trial: 2, Turns: 20}, {Trial: 3, Turns: 24}, {Trial: 4, Turns: 26}},
	}
	rawTrialJSON, err := json.Marshal(rawTrial)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rawResultPath, rawTrialJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout := "Raw result evidence: " + rawResultPath + "\n"
	if err := os.WriteFile(stdoutPath, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderrPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	commandPath := filepath.Join(evidenceDir, "command_run.json")
	command := domain.CommandRun{
		Name: "harbor_run_qwen", Argv: []string{"harbor", "run", "-p", taskDir, "-a", "claude-code", "-m", "qwen3.7-max", "-n", "4", "-k", "4"},
		ExitCode: 0, Passed: true, Stdout: stdout, StdoutPath: stdoutPath, StderrPath: stderrPath,
	}
	commandRaw, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(commandPath, commandRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(evidenceDir, "qwen_result.json")
	trial := domain.TrialResult{
		SchemaVersion: "harbor.trial_result.v1", Model: "qwen3.7-max", Agent: "claude-code",
		Trials: 4, PassCount: 1, PassAt4: .25, AverageTurns: 23, TaskDigest: digest, TaskPath: taskDir, CommandRunPath: commandPath,
		RawResultPath: rawResultPath, RawResultSHA256: fmt.Sprintf("%x", sha256.Sum256(rawTrialJSON)),
		Runs: []domain.TrialRun{{Trial: 1, Passed: true, Turns: 22}, {Trial: 2, Turns: 20}, {Trial: 3, Turns: 24}, {Trial: 4, Turns: 26}},
	}
	trialRaw, err := json.Marshal(trial)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, trialRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	store := newStore(t)
	evaluation := &fakeEvaluationRuntime{err: errors.New("evaluation must not run")}
	spec := workflow.NodeSpec{ID: "harbor_run_qwen", Kind: HarborRunQwenKind, Config: map[string]any{
		"task_dir": taskDir, "model": "qwen3.7-max", "agent": "claude-code", "result_path": resultPath,
	}}
	result, err := (HarborRunQwenPlugin{}).Execute(context.Background(), workflow.NodeRequest{
		RunID: "run-import", Spec: spec, Store: store, Runtimes: workflow.Runtimes{Evaluation: evaluation},
	})
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.request.Model != "" {
		t.Fatalf("existing evidence unexpectedly invoked evaluation: %+v", evaluation.request)
	}
	if len(result.Artifacts) != 6 || result.Artifacts[0].Name != "phase3/artifacts/harbor_run_qwen/qwen_result.json" || result.Artifacts[2].Type != "pass4_screenshot" {
		t.Fatalf("imported evidence was not canonicalized in ArtifactStore: %+v", result.Artifacts)
	}
	resumeSpec := spec
	resumeSpec.Config = map[string]any{
		"task_dir": taskDir, "model": "qwen3.7-max", "agent": "claude-code", "result_path": result.Artifacts[0].Path,
	}
	resumed, err := (HarborRunQwenPlugin{}).Execute(context.Background(), workflow.NodeRequest{
		RunID: "run-import-resume", Spec: resumeSpec, Store: store, Runtimes: workflow.Runtimes{Evaluation: evaluation},
	})
	if err != nil {
		t.Fatalf("resume could not re-import canonical result before invalidation: %v", err)
	}
	if len(resumed.Artifacts) != 6 || resumed.Artifacts[2].Type != "pass4_screenshot" {
		t.Fatalf("resume did not regenerate canonical screenshot evidence: %+v", resumed.Artifacts)
	}
}

func TestHarborPluginStoresRedactedPartialEvidenceOnCancellation(t *testing.T) {
	store := newStore(t)
	evaluation := &fakeEvaluationRuntime{
		result: workflow.EvaluationResult{
			TrialResult: workflow.EvaluationTrialResult{Model: "opus-model", Trials: 1, Runs: []workflow.EvaluationTrialRun{{Trial: 1}}},
			CommandRun:  workflow.CommandEvidence{Command: "harbor --api-key secret-value", Env: []string{"API_KEY=secret-value"}, Stderr: "API_KEY=secret-value", ExitCode: -1},
		},
		err: context.Canceled,
	}
	spec := workflow.NodeSpec{ID: "harbor_run_opus", Kind: HarborRunOpusKind, Config: map[string]any{"task_dir": "/task", "model": "opus-model"}}
	result, err := (HarborRunOpusPlugin{}).Execute(context.Background(), workflow.NodeRequest{RunID: "run-1", Spec: spec, Store: store, Runtimes: workflow.Runtimes{Evaluation: evaluation}})
	if err == nil || len(result.Artifacts) != 6 {
		t.Fatalf("partial cancellation evidence was not retained: %+v, %v", result, err)
	}
	raw, readErr := os.ReadFile(result.Artifacts[3].Path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(raw), "secret-value") {
		t.Fatalf("credential leaked into command artifact:\n%s", raw)
	}
}

func TestQualityPluginRunsDeterministicChecksAndStoresReport(t *testing.T) {
	taskDir := writeTask(t)
	store := newStore(t)
	spec := workflow.NodeSpec{ID: "quality_check", Kind: QualityCheckKind, Config: map[string]any{
		"task_dir": taskDir, "tests_analysis": filepath.Join(taskDir, "tests_analysis.md"),
	}}
	result, err := (QualityCheckPlugin{}).Execute(context.Background(), workflow.NodeRequest{Spec: spec, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].Type != "quality_report" {
		t.Fatalf("quality report artifact mismatch: %+v", result.Artifacts)
	}
	var report domain.QualityReport
	raw, err := os.ReadFile(result.Artifacts[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &report); err != nil || !report.OverallPass {
		t.Fatalf("quality report did not pass: %+v, %v", report, err)
	}
}

func validEvaluation(model string) workflow.EvaluationResult {
	runs := []workflow.EvaluationTrialRun{
		{Trial: 1, Passed: true, Turns: 22}, {Trial: 2, Turns: 20}, {Trial: 3, Turns: 24}, {Trial: 4, Turns: 26},
	}
	return workflow.EvaluationResult{
		TrialResult: workflow.EvaluationTrialResult{SchemaVersion: "harbor.trial_result.v1", Model: model, Trials: 4, PassCount: 1, PassAt4: .25, AverageTurns: 23, Runs: runs, CreatedAt: time.Now().UTC()},
		CommandRun:  workflow.CommandEvidence{Name: "harbor_run", Command: "harbor run", ExitCode: 0, Passed: true, StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC()},
	}
}

func writeTask(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"instruction.md":         "Fix the public configuration behavior and preserve compatibility. Run the repository tests.\n",
		"task.toml":              "version = \"1\"\n",
		"environment/Dockerfile": "FROM ubuntu:24.04\n",
		"solution/solve.sh":      "printf '%s\\n' fixed > /tmp/fixed\n",
		"tests/test.sh":          "set -eu\nprintf '%s\\n' start\ntest -f /tmp/fixed\ngrep -q fixed /tmp/fixed\nprintf '%s\\n' checked\nprintf '%s\\n' done\n",
		"tests_analysis.md":      "## 1. instruction 和 environment 已提供的信息\n- public contract\n\n## 2. 模型的理论通过路径\n- inspect and fix\n\n## 3. 模型具备通过条件的依据\n- public tests\n",
	}
	for rel, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newStore(t *testing.T) *workflow.FileArtifactStore {
	t.Helper()
	store, err := workflow.NewFileArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func containsArg(args []string, value string) bool {
	for _, arg := range args {
		if arg == value {
			return true
		}
	}
	return false
}
