package harborrun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/executor"
	"github.com/purplevoid/harbor-factory/internal/harbor/commandlog"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/sanitize"
)

type Options struct {
	TaskPath       string
	Model          string
	Agent          string
	AgentEnv       []string
	OutputDir      string
	TimeoutSeconds int
	Env            []string
	Exec           executor.CommandRunner
}

func Run(ctx context.Context, opts Options) (domain.TrialResult, domain.CommandRun, error) {
	taskPath := strings.TrimSpace(opts.TaskPath)
	if taskPath == "" {
		return domain.TrialResult{}, domain.CommandRun{}, fmt.Errorf("task path is required")
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		return domain.TrialResult{}, domain.CommandRun{}, fmt.Errorf("model is required")
	}
	agent := strings.TrimSpace(opts.Agent)
	if agent == "" {
		agent = "claude-code"
	}
	outputDir := strings.TrimSpace(opts.OutputDir)
	if outputDir == "" {
		outputDir = filepath.Join(".harbor-factory", "harbor_run", safePathName(model))
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return domain.TrialResult{}, domain.CommandRun{}, err
	}
	jobsDir := filepath.Join(outputDir, "jobs")
	if err := os.MkdirAll(jobsDir, 0o700); err != nil {
		return domain.TrialResult{}, domain.CommandRun{}, err
	}
	taskDigest, err := ComputeTaskDigest(taskPath)
	if err != nil {
		return domain.TrialResult{}, domain.CommandRun{}, fmt.Errorf("compute task digest: %w", err)
	}
	exec := opts.Exec
	if exec == nil {
		exec = executor.New()
	}
	timeout := time.Duration(opts.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 2 * time.Hour
	}
	start := time.Now().UTC()
	args := []string{"run", "-p", taskPath, "-a", agent, "-m", model, "-o", jobsDir}
	for _, item := range opts.AgentEnv {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		args = append(args, "--ae", item)
	}
	args = append(args, "-n", "4", "-k", "4")
	result := exec.Run(ctx, timeout, "", opts.Env, "harbor", args...)
	finished := time.Now().UTC()
	stdout := commandlog.RedactText(result.Stdout)
	stderr := commandlog.RedactText(result.Stderr)
	envSummary := append(commandlog.EffectiveEnv(opts.Env), opts.AgentEnv...)
	commandRun := domain.CommandRun{
		Name:       "harbor_run_" + safePathName(model),
		Command:    commandlog.RedactText(result.Command),
		Argv:       commandlog.RedactArgv(append([]string{"harbor"}, args...)),
		Dir:        commandlog.ResolveCWD(""),
		Env:        commandlog.RedactEnv(envSummary),
		Attempt:    1,
		ExitCode:   result.ExitCode,
		Stdout:     stdout,
		Stderr:     stderr,
		Timeout:    result.Timeout,
		StartedAt:  start,
		FinishedAt: finished,
		DurationMS: finished.Sub(start).Milliseconds(),
		Passed:     result.Err == nil && !result.Timeout,
	}
	if result.Err != nil && commandRun.ExitCode == 0 {
		commandRun.ExitCode = -1
	}
	commandRun.FailureClass = commandlog.ClassifyFailure(commandRun.ExitCode, commandRun.Timeout, stdout, stderr)
	commandRunPath, err := writeCommandArtifacts(outputDir, &commandRun)
	if err != nil {
		return domain.TrialResult{}, commandRun, err
	}
	trialResult, parseErr := parseCommandResult(outputDir, result.Stdout, result.Stderr)
	if parseErr != nil {
		if result.Err != nil {
			return domain.TrialResult{}, commandRun, fmt.Errorf("harbor run failed: %w; additionally failed to parse result: %v", result.Err, parseErr)
		}
		return domain.TrialResult{}, commandRun, parseErr
	}
	trialResult.Model = defaultString(trialResult.Model, model)
	trialResult.Agent = defaultString(trialResult.Agent, agent)
	trialResult.TaskDigest = taskDigest
	trialResult.CommandRunPath = commandRunPath
	trialResult = sanitize.TrialResult(trialResult)
	if failures := validateRunCompletion(trialResult); len(failures) > 0 {
		return trialResult, commandRun, fmt.Errorf("harbor result failed completion audit: %s", strings.Join(failures, "; "))
	}
	normalizedPath := filepath.Join(outputDir, "trial_result.json")
	if err := writeTrialResult(normalizedPath, trialResult); err != nil {
		return trialResult, commandRun, err
	}
	trialResult.ResultPath = normalizedPath
	if result.Err != nil {
		return trialResult, commandRun, fmt.Errorf("harbor run failed: %w", result.Err)
	}
	return trialResult, commandRun, nil
}

func validateRunCompletion(result domain.TrialResult) []string {
	var failures []string
	if result.Trials != 4 {
		failures = append(failures, fmt.Sprintf("harbor result must contain 4 trials, got %d", result.Trials))
	}
	if len(result.Runs) > 0 && len(result.Runs) != 4 {
		failures = append(failures, fmt.Sprintf("harbor result runs must contain 4 records, got %d", len(result.Runs)))
	}
	if result.HarborTaskChecksum != "" {
		for _, run := range result.Runs {
			if strings.TrimSpace(run.FailureReason) != "" {
				failures = append(failures, fmt.Sprintf("harbor trial %d errored: %s", run.Trial, run.FailureReason))
			}
		}
	}
	return failures
}

func writeTrialResult(path string, result domain.TrialResult) error {
	result = sanitize.TrialResult(result)
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func writeCommandArtifacts(outputDir string, commandRun *domain.CommandRun) (string, error) {
	clean := sanitize.CommandRun(*commandRun)
	*commandRun = clean
	stdoutPath, stderrPath, err := commandlog.WriteOutputFiles(outputDir, commandRun.Stdout, commandRun.Stderr)
	if err != nil {
		return "", err
	}
	commandRun.StdoutPath = stdoutPath
	commandRun.StderrPath = stderrPath
	data, err := json.MarshalIndent(*commandRun, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(outputDir, "command_run.json")
	return path, os.WriteFile(path, append(data, '\n'), 0o600)
}

func parseCommandResult(outputDir, stdout, stderr string) (domain.TrialResult, error) {
	if result, err := ParseResult([]byte(stdout)); err == nil {
		result.ResultPath = filepath.Join(outputDir, "stdout.txt")
		return result, nil
	}
	resultPath := findResultPath(stdout + "\n" + stderr)
	if resultPath == "" {
		return domain.TrialResult{}, fmt.Errorf("harbor result JSON path not found in stdout/stderr")
	}
	return ParseFile(resultPath)
}

var resultPathPattern = regexp.MustCompile(`(?m)(/[^[:space:]"']*result\.json|[A-Za-z0-9._/\-]+result\.json)`)

func findResultPath(text string) string {
	matches := resultPathPattern.FindAllString(text, -1)
	for _, match := range matches {
		candidate := strings.Trim(match, " \t\r\n\"'`,.;:")
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func safePathName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return "model"
	}
	return out
}

func defaultString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return strings.TrimSpace(fallback)
	}
	return value
}
