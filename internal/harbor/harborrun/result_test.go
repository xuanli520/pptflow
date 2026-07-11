package harborrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

func TestParseResultNormalizesRuns(t *testing.T) {
	result, err := ParseResult([]byte(`{
		"model": "qwen3.7-max",
		"runs": [
			{"trial": 1, "passed": false, "turns": 21, "reward": 0},
			{"trial": 2, "passed": true, "turns": 24, "reward": 1},
			{"trial": 3, "passed": false, "turns": 22, "reward": 0},
			{"trial": 4, "passed": false, "turns": 25, "reward": 0}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Trials != 4 || result.PassCount != 1 {
		t.Fatalf("unexpected normalized counts: %+v", result)
	}
	if result.AverageTurns != 23 {
		t.Fatalf("average_turns = %v, want 23", result.AverageTurns)
	}
	if failures := ValidateForCodeEdge(result, true); len(failures) != 0 {
		t.Fatalf("unexpected CodeEdge failures: %+v", failures)
	}
}

func TestParseResultPreservesExplicitFailedRunWhenRewardExists(t *testing.T) {
	result, err := ParseResult([]byte(`{
		"model": "qwen3.7-max",
		"runs": [
			{"trial": 1, "passed": false, "turns": 46, "reward": 1, "failure_reason": "AgentTimeoutError"},
			{"trial": 2, "turns": 20, "reward": 1}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Runs[0].Passed || !result.Runs[1].Passed || result.PassCount != 1 {
		t.Fatalf("explicit pass state was not preserved: %+v", result)
	}
}

func TestValidateForCodeEdgeFailsQwenThresholds(t *testing.T) {
	result, err := ParseResult([]byte(`{
		"model": "qwen3.7-max",
		"trials": 4,
		"pass_count": 2,
		"pass_at_4": 0.5,
		"average_turns": 12
	}`))
	if err != nil {
		t.Fatal(err)
	}
	failures := ValidateForCodeEdge(result, true)
	if len(failures) != 3 {
		t.Fatalf("failures = %+v, want 3 failures", failures)
	}
}

func TestValidateForCodeEdgeDoesNotApplyQwenDifficultyThresholdToOpus(t *testing.T) {
	result, err := ParseResult([]byte(`{
		"model": "claude-opus-4-8",
		"trials": 4,
		"pass_count": 4,
		"pass_at_4": 1,
		"average_turns": 6
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if failures := ValidateForCodeEdge(result, false); len(failures) != 0 {
		t.Fatalf("Opus should not inherit Qwen difficulty thresholds: %+v", failures)
	}
}

func TestValidateForCodeEdgeWithOptionsRequiresModelRunsAndDigest(t *testing.T) {
	taskDir := writeHarborRunTask(t)
	digest, err := ComputeTaskDigest(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ParseResult([]byte(`{
		"model": "qwen3.7-max",
		"task_digest": "` + digest + `",
		"runs": [
			{"trial": 1, "passed": false, "turns": 21, "reward": 0},
			{"trial": 2, "passed": true, "turns": 24, "reward": 1},
			{"trial": 3, "passed": false, "turns": 22, "reward": 0},
			{"trial": 4, "passed": false, "turns": 25, "reward": 0}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	failures := ValidateForCodeEdgeWithOptions(result, ValidationOptions{
		Qwen:              true,
		ExpectedModel:     "qwen",
		TaskDir:           taskDir,
		RequireRuns:       true,
		RequireTaskDigest: true,
	})
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	result.TaskDigest = "sha256:bad"
	failures = ValidateForCodeEdgeWithOptions(result, ValidationOptions{Qwen: true, ExpectedModel: "opus", TaskDir: taskDir, RequireRuns: true, RequireTaskDigest: true})
	if len(failures) < 2 {
		t.Fatalf("expected model and digest failures, got %+v", failures)
	}
}

func TestComputeTaskDigestIncludesTestsAnalysis(t *testing.T) {
	taskDir := writeHarborRunTask(t)
	before, err := ComputeTaskDigest(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "tests_analysis.md"), []byte("changed tests analysis\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := ComputeTaskDigest(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatalf("task digest did not change after tests_analysis.md mutation: %s", before)
	}
}

func TestParseHarborJobResultCollectsTrialResults(t *testing.T) {
	taskDir := writeHarborRunTask(t)
	jobPath := writeHarborJobFixture(t, t.TempDir(), taskDir, "qwen3.7-max", []harborTrialFixture{
		{Reward: 0, Turns: 21},
		{Reward: 1, Turns: 24},
		{Reward: 0, Turns: 22},
		{Reward: 0, Turns: 25},
	})
	result, err := ParseFile(jobPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Trials != 4 || result.PassCount != 1 || result.PassAt4 != 0.25 || result.AverageTurns != 23 {
		t.Fatalf("unexpected normalized Harbor job result: %+v", result)
	}
	if result.Model != "qwen3.7-max" || result.Agent != "claude-code" {
		t.Fatalf("unexpected agent/model: %+v", result)
	}
	if result.RawResultPath != jobPath || result.RawResultSHA256 == "" || len(result.RawTrialResults) != 4 {
		t.Fatalf("missing raw result evidence: %+v", result)
	}
	if result.TaskDigest == "" || result.HarborTaskChecksum == "" || result.TaskPath != taskDir {
		t.Fatalf("missing task evidence: %+v", result)
	}
	failures := ValidateForCodeEdgeWithOptions(result, ValidationOptions{
		Qwen:              true,
		ExpectedModel:     "qwen3.7-max",
		TaskDir:           taskDir,
		RequireRuns:       true,
		RequireTaskDigest: true,
	})
	if len(failures) != 0 {
		t.Fatalf("unexpected strict validation failures: %+v", failures)
	}
}

func TestParsePartialHarborJobPreservesPlannedTrialDenominator(t *testing.T) {
	taskDir := writeHarborRunTask(t)
	jobPath := writeHarborJobFixture(t, t.TempDir(), taskDir, "qwen3.7-max", []harborTrialFixture{
		{Reward: 1, Turns: 24},
		{Turns: 22, ExceptionType: "AgentTimeoutError"},
	})
	raw, err := os.ReadFile(jobPath)
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatal(err)
	}
	job["n_total_trials"] = 4
	raw, err = json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := ParseFile(jobPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Trials != 4 || len(result.Runs) != 2 || result.PassCount != 1 || result.PassAt4 != 0.25 {
		t.Fatalf("partial result used completed runs as the pass@4 denominator: %+v", result)
	}
}

func TestValidateForCodeEdgeFailsSummaryOnlyResultWhenStrict(t *testing.T) {
	result, err := ParseResult([]byte(`{
		"model": "qwen3.7-max",
		"trials": 4,
		"pass_count": 1,
		"pass_at_4": 0.25,
		"average_turns": 23
	}`))
	if err != nil {
		t.Fatal(err)
	}
	failures := ValidateForCodeEdgeWithOptions(result, ValidationOptions{Qwen: true, ExpectedModel: "qwen", RequireRuns: true, RequireTaskDigest: true})
	if len(failures) == 0 {
		t.Fatal("expected strict result validation failure")
	}
}

func TestValidateForCodeEdgeRequiresCommandRunWhenStrict(t *testing.T) {
	taskDir := writeHarborRunTask(t)
	digest, err := ComputeTaskDigest(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ParseResult([]byte(`{
		"model": "qwen3.7-max",
		"agent": "claude-code",
		"task_digest": "` + digest + `",
		"runs": [
			{"trial": 1, "passed": false, "turns": 21, "reward": 0},
			{"trial": 2, "passed": true, "turns": 24, "reward": 1},
			{"trial": 3, "passed": false, "turns": 22, "reward": 0},
			{"trial": 4, "passed": false, "turns": 25, "reward": 0}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	failures := ValidateForCodeEdgeWithOptions(result, ValidationOptions{
		Qwen:              true,
		ExpectedModel:     "qwen3.7-max",
		TaskDir:           taskDir,
		RequireRuns:       true,
		RequireTaskDigest: true,
		RequireCommandRun: true,
	})
	if !failureContains(failures, "command_run_path is required") {
		t.Fatalf("expected missing command_run_path failure, got %+v", failures)
	}

	commandDir := t.TempDir()
	commandPath := filepath.Join(commandDir, "command_run.json")
	stdoutPath := filepath.Join(commandDir, "stdout.txt")
	stderrPath := filepath.Join(commandDir, "stderr.txt")
	rawResultPath := filepath.Join(commandDir, "raw_result.json")
	rawResult, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rawResultPath, rawResult, 0o644); err != nil {
		t.Fatal(err)
	}
	result.RawResultPath = rawResultPath
	result.RawResultSHA256 = sha256Evidence(rawResult)
	command := domain.CommandRun{
		Argv:       []string{"harbor", "run", "-p", taskDir, "-a", "claude-code", "-m", "qwen3.7-max", "-n", "4", "-k", "4"},
		ExitCode:   0,
		Passed:     true,
		Stdout:     "Results written to " + rawResultPath + "\n",
		Stderr:     "",
		StdoutPath: stdoutPath,
		StderrPath: stderrPath,
	}
	if err := os.WriteFile(stdoutPath, []byte(command.Stdout), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderrPath, []byte(command.Stderr), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(commandPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	result.CommandRunPath = commandPath
	failures = ValidateForCodeEdgeWithOptions(result, ValidationOptions{
		Qwen:              true,
		ExpectedModel:     "qwen3.7-max",
		TaskDir:           taskDir,
		RequireRuns:       true,
		RequireTaskDigest: true,
		RequireCommandRun: true,
	})
	if len(failures) != 0 {
		t.Fatalf("unexpected command audit failures: %+v", failures)
	}

	mismatchedRawResult := result
	mismatchedRawResult.Runs = append([]domain.TrialRun(nil), result.Runs...)
	mismatchedRawResult.Runs[2].Passed = true
	raw, err = json.Marshal(mismatchedRawResult)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rawResultPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	result.RawResultSHA256 = sha256Evidence(raw)
	failures = ValidateForCodeEdgeWithOptions(result, ValidationOptions{
		Qwen:              true,
		ExpectedModel:     "qwen3.7-max",
		TaskDir:           taskDir,
		RequireRuns:       true,
		RequireTaskDigest: true,
		RequireCommandRun: true,
	})
	if !failureContains(failures, "trial 3 pass state") {
		t.Fatalf("expected raw result mismatch failure, got %+v", failures)
	}
	if err := os.WriteFile(rawResultPath, rawResult, 0o644); err != nil {
		t.Fatal(err)
	}
	result.RawResultSHA256 = sha256Evidence(rawResult)

	command.StdoutPath = ""
	raw, err = json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(commandPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	failures = ValidateForCodeEdgeWithOptions(result, ValidationOptions{
		Qwen:              true,
		ExpectedModel:     "qwen3.7-max",
		TaskDir:           taskDir,
		RequireRuns:       true,
		RequireTaskDigest: true,
		RequireCommandRun: true,
	})
	if !failureContains(failures, "stdout_path is required") {
		t.Fatalf("expected missing stdout_path failure, got %+v", failures)
	}

	command.StdoutPath = stdoutPath
	if err := os.WriteFile(stdoutPath, []byte("different stdout"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(commandPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	failures = ValidateForCodeEdgeWithOptions(result, ValidationOptions{
		Qwen:              true,
		ExpectedModel:     "qwen3.7-max",
		TaskDir:           taskDir,
		RequireRuns:       true,
		RequireTaskDigest: true,
		RequireCommandRun: true,
	})
	if !failureContains(failures, "stdout_path content does not match") {
		t.Fatalf("expected stdout content mismatch failure, got %+v", failures)
	}
	if err := os.WriteFile(stdoutPath, []byte(command.Stdout), 0o644); err != nil {
		t.Fatal(err)
	}

	command.Argv = []string{"harbor", "run", "-p", filepath.Join(t.TempDir(), "other-task"), "-a", "claude-code", "-m", "qwen3.7-max", "-n", "4", "-k", "4"}
	raw, err = json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(commandPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	failures = ValidateForCodeEdgeWithOptions(result, ValidationOptions{
		Qwen:              true,
		ExpectedModel:     "qwen3.7-max",
		TaskDir:           taskDir,
		RequireRuns:       true,
		RequireTaskDigest: true,
		RequireCommandRun: true,
	})
	if !failureContains(failures, "current task path") {
		t.Fatalf("expected task path command audit failure, got %+v", failures)
	}

	command.Argv = []string{"harbor", "run", "-p", taskDir, "-a", "claude-code", "-m", "qwen3.7-max", "-n", "4", "-k", "4"}
	command.Stdout = "OPENAI_API_KEY=raw-api-value"
	if err := os.WriteFile(stdoutPath, []byte(command.Stdout), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(commandPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	failures = ValidateForCodeEdgeWithOptions(result, ValidationOptions{
		Qwen:              true,
		ExpectedModel:     "qwen3.7-max",
		TaskDir:           taskDir,
		RequireRuns:       true,
		RequireTaskDigest: true,
		RequireCommandRun: true,
	})
	if !failureContains(failures, "secret-like values") {
		t.Fatalf("expected command secret scan failure, got %+v", failures)
	}
}

func TestCommandOutputReferencesSoftWrappedHarborResultPath(t *testing.T) {
	path := "/tmp/harbor-factory/jobs/2026-07-11/result.json"
	output := "Results written to \n/tmp/harbor-factory/jobs\r\n/2026-07-11/result.json\n"
	if !commandOutputReferencesPath(output, path, path) {
		t.Fatal("expected CR/LF-soft-wrapped Harbor result path to pass command audit")
	}
	if commandOutputReferencesPath("Results written to /tmp/harbor-factory/jobs /2026-07-11/result.json", path, path) {
		t.Fatal("ordinary whitespace must not be removed while matching result paths")
	}
}

func TestValidateForCodeEdgeRejectsFakeModelWhenExpectedModelIsExact(t *testing.T) {
	result, err := ParseResult([]byte(`{
		"model": "fake-qwen-proxy",
		"runs": [
			{"trial": 1, "passed": false, "turns": 21, "reward": 0},
			{"trial": 2, "passed": true, "turns": 24, "reward": 1},
			{"trial": 3, "passed": false, "turns": 22, "reward": 0},
			{"trial": 4, "passed": false, "turns": 25, "reward": 0}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	failures := ValidateForCodeEdgeWithOptions(result, ValidationOptions{Qwen: true, ExpectedModel: "qwen3.7-max", RequireRuns: true})
	if len(failures) == 0 {
		t.Fatal("expected fake model failure")
	}
}

func TestValidateForCodeEdgeRejectsMissingOrNonSequentialTrials(t *testing.T) {
	result, err := ParseResult([]byte(`{
		"model": "qwen3.7-max",
		"pass_at_4": 0.25,
		"runs": [
			{"trial": 1, "passed": false, "turns": 21, "reward": 0},
			{"trial": 2, "passed": true, "turns": 24, "reward": 1},
			{"trial": 3, "passed": false, "turns": 22, "reward": 0},
			{"passed": false, "turns": 25, "reward": 0}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	failures := ValidateForCodeEdgeWithOptions(result, ValidationOptions{Qwen: true, ExpectedModel: "qwen3.7-max", RequireRuns: true})
	if len(failures) == 0 {
		t.Fatal("expected missing explicit trial failure")
	}

	result, err = ParseResult([]byte(`{
		"model": "qwen3.7-max",
		"pass_at_4": 0.25,
		"runs": [
			{"trial": 1, "passed": false, "turns": 21, "reward": 0},
			{"trial": 2, "passed": true, "turns": 24, "reward": 1},
			{"trial": 3, "passed": false, "turns": 22, "reward": 0},
			{"trial": 5, "passed": false, "turns": 25, "reward": 0}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	failures = ValidateForCodeEdgeWithOptions(result, ValidationOptions{Qwen: true, ExpectedModel: "qwen3.7-max", RequireRuns: true})
	if len(failures) == 0 {
		t.Fatal("expected missing trial 4 failure")
	}
}

func TestValidateForCodeEdgeRejectsPassAt4Mismatch(t *testing.T) {
	result, err := ParseResult([]byte(`{
		"model": "qwen3.7-max",
		"pass_at_4": 0.50,
		"runs": [
			{"trial": 1, "passed": false, "turns": 21, "reward": 0},
			{"trial": 2, "passed": true, "turns": 24, "reward": 1},
			{"trial": 3, "passed": false, "turns": 22, "reward": 0},
			{"trial": 4, "passed": false, "turns": 25, "reward": 0}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	failures := ValidateForCodeEdgeWithOptions(result, ValidationOptions{Qwen: true, ExpectedModel: "qwen3.7-max", RequireRuns: true})
	if len(failures) == 0 {
		t.Fatal("expected pass_at_4 mismatch failure")
	}
}

func failureContains(failures []string, want string) bool {
	for _, failure := range failures {
		if strings.Contains(failure, want) {
			return true
		}
	}
	return false
}

type harborTrialFixture struct {
	Reward        float64
	Turns         int
	ExceptionType string
}

func writeHarborJobFixture(t *testing.T, jobDir, taskDir, model string, trials []harborTrialFixture) string {
	t.Helper()
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	evalKey := "claude-code__" + model + "__local"
	errored := 0
	for idx, trial := range trials {
		if trial.ExceptionType != "" {
			errored++
		}
		trialDir := filepath.Join(jobDir, "trial-"+strconv.Itoa(idx+1))
		if err := os.MkdirAll(trialDir, 0o755); err != nil {
			t.Fatal(err)
		}
		raw := harborTrialResultJSON(taskDir, model, "trial-"+strconv.Itoa(idx+1), trial)
		if err := os.WriteFile(filepath.Join(trialDir, "result.json"), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	job := map[string]any{
		"id":             "7f82cfb5-ec91-417a-897e-5c96360a5159",
		"started_at":     "2026-07-10T11:27:45.311583",
		"updated_at":     "2026-07-10T11:28:25.850268",
		"finished_at":    "2026-07-10T11:28:25.850268",
		"n_total_trials": len(trials),
		"stats": map[string]any{
			"n_completed_trials": len(trials),
			"n_errored_trials":   errored,
			"n_running_trials":   0,
			"n_pending_trials":   0,
			"evals": map[string]any{
				evalKey: map[string]any{
					"n_trials":        len(trials) - errored,
					"n_errors":        errored,
					"metrics":         []any{map[string]any{"mean": 0.25}},
					"pass_at_k":       map[string]any{},
					"reward_stats":    map[string]any{},
					"exception_stats": map[string]any{},
				},
			},
		},
	}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(jobDir, "result.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func harborTrialResultJSON(taskDir, model, trialName string, fixture harborTrialFixture) string {
	exception := any(nil)
	verifier := any(map[string]any{"rewards": map[string]any{"reward": fixture.Reward}})
	if fixture.ExceptionType != "" {
		exception = map[string]any{
			"exception_type":      fixture.ExceptionType,
			"exception_message":   "trial failed",
			"exception_traceback": "traceback",
			"occurred_at":         "2026-07-10T11:28:24.382875",
		}
		verifier = nil
	}
	raw, err := json.Marshal(map[string]any{
		"id":         "b346b3c3-7b50-4353-884b-9d7720318e81",
		"task_name":  "local/task",
		"trial_name": trialName,
		"trial_uri":  "file://" + filepath.Join(filepath.Dir(taskDir), trialName),
		"task_id": map[string]any{
			"path": taskDir,
		},
		"source":        "local",
		"task_checksum": "6ad7a883e183f3636fa1656c8b926d11f26507bf9b69be91bcf45c83582095b3",
		"config": map[string]any{
			"agent": map[string]any{
				"name":       "claude-code",
				"model_name": model,
			},
		},
		"agent_info": map[string]any{
			"name":    "claude-code",
			"version": "1.0.0",
			"model_info": map[string]any{
				"name": model,
			},
		},
		"agent_result": map[string]any{
			"metadata": map[string]any{
				"turns": fixture.Turns,
			},
		},
		"verifier_result": verifier,
		"exception_info":  exception,
		"started_at":      "2026-07-10T03:27:45.387750Z",
		"finished_at":     "2026-07-10T03:28:25.847813Z",
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}
