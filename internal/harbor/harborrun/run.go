package harborrun

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/executor"
	"github.com/purplevoid/harbor-factory/internal/harbor/commandlog"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/sanitize"
)

const retryingClaudeImportPath = "harbor_factory_retrying_claude:RetryingClaudeCode"

//go:embed pyshim/harbor_factory_retrying_claude.py
var retryingClaudeAgentSource []byte

type Options struct {
	TaskPath            string
	Model               string
	Agent               string
	AgentEnv            []string
	OutputDir           string
	TimeoutSeconds      int
	SetupTimeoutSeconds int
	AgentCacheDir       string
	Preflight           bool
	Concurrency         int
	Attempts            int
	InfraRetries        int
	Progress            func(line, source string)
	Env                 []string
	Exec                executor.CommandRunner
	CacheExec           executor.CommandRunner
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
	requestedAgent := strings.TrimSpace(opts.Agent)
	if requestedAgent == "" {
		requestedAgent = "claude-code"
	}
	agent := requestedAgent
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
	infraRetries := opts.InfraRetries
	if infraRetries < 0 {
		infraRetries = 0
	}
	exec := opts.Exec
	if exec == nil {
		exec = executor.New()
	}
	agentEnvArgs, processEnv, err := secureAgentEnvironment(opts.AgentEnv, opts.Env)
	if err != nil {
		return domain.TrialResult{}, domain.CommandRun{}, err
	}
	cacheManifestPath := ""
	cacheMount := ""
	if strings.EqualFold(requestedAgent, "claude-code") {
		cacheManifestPath, cacheMount, err = prepareClaudeAgentCache(ctx, opts.AgentCacheDir, opts.CacheExec)
		if err != nil {
			return domain.TrialResult{}, domain.CommandRun{}, err
		}
		if cacheManifestPath != "" {
			cacheManifestPath, err = copyAgentCacheManifest(cacheManifestPath, outputDir)
			if err != nil {
				return domain.TrialResult{}, domain.CommandRun{}, err
			}
		}
		agent, processEnv, err = prepareRetryingClaudeAgent(outputDir, processEnv, infraRetries)
		if err != nil {
			return domain.TrialResult{}, domain.CommandRun{}, err
		}
	}
	timeout := time.Duration(opts.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 2 * time.Hour
	}
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	attempts := opts.Attempts
	if attempts <= 0 {
		attempts = 4
	}
	preflightPath := ""
	schemaPreflightPath := ""
	preflightResultPath := ""
	if opts.Preflight {
		setupTimeout := time.Duration(opts.SetupTimeoutSeconds) * time.Second
		if setupTimeout <= 0 {
			setupTimeout = 20 * time.Minute
		}
		preflightRoot := filepath.Join(outputDir, "preflight")
		schemaDir := filepath.Join(preflightRoot, "schema")
		schemaArgs := appendCacheMount(harborArgs(taskPath, agent, model, filepath.Join(schemaDir, "jobs"), agentEnvArgs), cacheMount)
		schemaArgs = append(schemaArgs, "--print-config", "--yes", "-n", "1", "-k", "1")
		schemaRun, path, err := executeHarborCommand(ctx, exec, setupTimeout, schemaDir, "harbor_schema_preflight_"+safePathName(model), processEnv, agentEnvArgs, opts.Progress, schemaArgs)
		if err != nil {
			return domain.TrialResult{SchemaPreflightPath: path}, schemaRun, fmt.Errorf("harbor task schema preflight failed: %w", err)
		}
		if !schemaRun.Passed {
			return domain.TrialResult{SchemaPreflightPath: path}, schemaRun, fmt.Errorf("harbor task schema preflight did not pass")
		}
		schemaPreflightPath = path

		var preflightRun domain.CommandRun
		var preflightErr error
		for attempt := 1; attempt <= infraRetries+1; attempt++ {
			preflightDir := filepath.Join(preflightRoot, "install", fmt.Sprintf("attempt-%02d", attempt))
			preflightJobsDir := filepath.Join(preflightDir, "jobs")
			if err := os.MkdirAll(preflightJobsDir, 0o700); err != nil {
				return domain.TrialResult{}, domain.CommandRun{}, err
			}
			preflightArgs := appendCacheMount(harborArgs(taskPath, agent, model, preflightJobsDir, agentEnvArgs), cacheMount)
			preflightArgs = append(preflightArgs, "--install-only", "--yes", "-n", "1", "-k", "1")
			preflightRun, preflightPath, preflightErr = executeHarborCommand(ctx, exec, setupTimeout, preflightDir, fmt.Sprintf("harbor_preflight_%s_attempt_%02d", safePathName(model), attempt), processEnv, agentEnvArgs, opts.Progress, preflightArgs)
			if preflightErr == nil && preflightRun.Passed {
				preflightResultPath, preflightErr = validateInstallPreflight(preflightJobsDir)
			}
			if preflightErr == nil && preflightRun.Passed {
				break
			}
			if attempt <= infraRetries && opts.Progress != nil {
				opts.Progress(fmt.Sprintf("install preflight attempt %d failed; retrying", attempt), "factory")
			}
		}
		if preflightErr != nil {
			return domain.TrialResult{SchemaPreflightPath: schemaPreflightPath, PreflightRunPath: preflightPath, PreflightResultPath: preflightResultPath}, preflightRun, fmt.Errorf("harbor install preflight failed: %w", preflightErr)
		}
		if !preflightRun.Passed {
			return domain.TrialResult{SchemaPreflightPath: schemaPreflightPath, PreflightRunPath: preflightPath, PreflightResultPath: preflightResultPath}, preflightRun, fmt.Errorf("harbor install preflight command did not pass")
		}
	}
	start := time.Now().UTC()
	args := appendCacheMount(harborArgs(taskPath, agent, model, jobsDir, agentEnvArgs), cacheMount)
	args = append(args, "--yes", "-n", fmt.Sprintf("%d", concurrency), "-k", fmt.Sprintf("%d", attempts))
	if infraRetries > 0 {
		args = append(args,
			"--max-retries", fmt.Sprintf("%d", infraRetries),
			"--retry-include", "RuntimeError",
			"--retry-include", "NetworkConnectionError",
			"--retry-include", "NonZeroAgentExitCodeError",
			"--retry-include", "ApiRateLimitError",
			"--retry-include", "ApiInternalServerError",
			"--retry-include", "ApiOverloadedError",
			"--retry-include", "ApiConnectionClosedError",
			"--retry-include", "UnknownApiError",
		)
	}
	stopJobMonitor := startJobProgressMonitor(ctx, jobsDir, outputDir, opts.Progress)
	result := runHarborCommand(ctx, exec, timeout, outputDir, processEnv, opts.Progress, args)
	retryEvidencePath := stopJobMonitor()
	finished := time.Now().UTC()
	stdout := commandlog.RedactText(result.Stdout)
	stderr := commandlog.RedactText(result.Stderr)
	envSummary := append(commandlog.EffectiveEnv(processEnv), agentEnvArgs...)
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
	trialResult, parseErr := parseCommandResult(outputDir, jobsDir, result.Stdout, result.Stderr)
	if parseErr != nil {
		if result.Err != nil {
			return domain.TrialResult{}, commandRun, fmt.Errorf("harbor run failed: %w; additionally failed to parse result: %v", result.Err, parseErr)
		}
		return domain.TrialResult{}, commandRun, parseErr
	}
	trialResult.Model = defaultString(trialResult.Model, model)
	trialResult.Agent = defaultString(trialResult.Agent, requestedAgent)
	trialResult.TaskDigest = taskDigest
	trialResult.CommandRunPath = commandRunPath
	trialResult.SchemaPreflightPath = schemaPreflightPath
	trialResult.PreflightRunPath = preflightPath
	trialResult.PreflightResultPath = preflightResultPath
	trialResult.AgentCacheManifest = cacheManifestPath
	trialResult.RetryEvidence = retryEvidencePath
	if trialResult.CreatedAt.IsZero() {
		trialResult.CreatedAt = finished
	}
	trialResult = sanitize.TrialResult(trialResult)
	normalizedPath := filepath.Join(outputDir, "trial_result.json")
	if err := writeTrialResult(normalizedPath, trialResult); err != nil {
		return trialResult, commandRun, err
	}
	trialResult.ResultPath = normalizedPath
	completionFailures := validateRunCompletion(trialResult)
	if result.Err != nil && len(completionFailures) > 0 {
		return trialResult, commandRun, fmt.Errorf("harbor run failed: %w; result also failed completion audit: %s", result.Err, strings.Join(completionFailures, "; "))
	}
	if result.Err != nil {
		return trialResult, commandRun, fmt.Errorf("harbor run failed: %w", result.Err)
	}
	if len(completionFailures) > 0 {
		return trialResult, commandRun, fmt.Errorf("harbor result failed completion audit: %s", strings.Join(completionFailures, "; "))
	}
	return trialResult, commandRun, nil
}

func copyAgentCacheManifest(source, outputDir string) (string, error) {
	raw, err := os.ReadFile(source)
	if err != nil {
		return "", fmt.Errorf("read Harbor agent cache manifest: %w", err)
	}
	path := filepath.Join(outputDir, "agent_cache_manifest.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("copy Harbor agent cache manifest: %w", err)
	}
	return path, nil
}

func appendCacheMount(args []string, mount string) []string {
	if mount = strings.TrimSpace(mount); mount != "" {
		args = append(args, "--mounts", mount)
	}
	return args
}

func prepareRetryingClaudeAgent(outputDir string, processEnv []string, infraRetries int) (string, []string, error) {
	shimDir := filepath.Join(outputDir, ".factory-agent")
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create Harbor agent shim directory: %w", err)
	}
	shimPath := filepath.Join(shimDir, "harbor_factory_retrying_claude.py")
	if err := os.WriteFile(shimPath, retryingClaudeAgentSource, 0o600); err != nil {
		return "", nil, fmt.Errorf("write Harbor agent shim: %w", err)
	}
	pythonPath := shimDir
	if current := processEnvValue(processEnv, "PYTHONPATH"); current != "" {
		pythonPath += string(os.PathListSeparator) + current
	}
	processEnv = upsertProcessEnv(processEnv, "PYTHONPATH", pythonPath)
	processEnv = upsertProcessEnv(processEnv, "HARBOR_FACTORY_INSTALL_ATTEMPTS", fmt.Sprintf("%d", infraRetries+1))
	processEnv = upsertProcessEnv(processEnv, "HARBOR_FACTORY_NPM_FETCH_RETRIES", fmt.Sprintf("%d", infraRetries))
	return retryingClaudeImportPath, processEnv, nil
}

func processEnvValue(values []string, key string) string {
	for i := len(values) - 1; i >= 0; i-- {
		itemKey, value, ok := strings.Cut(values[i], "=")
		if ok && itemKey == key {
			return value
		}
	}
	return ""
}

func harborArgs(taskPath, agent, model, jobsDir string, agentEnv []string) []string {
	args := []string{"run", "-p", taskPath, "-a", agent, "-m", model, "-o", jobsDir}
	for _, item := range agentEnv {
		item = strings.TrimSpace(item)
		if item != "" {
			args = append(args, "--ae", item)
		}
	}
	return args
}

func secureAgentEnvironment(agentEnv, baseEnv []string) ([]string, []string, error) {
	processEnv := append([]string(nil), baseEnv...)
	if len(processEnv) == 0 {
		processEnv = os.Environ()
	}
	args := make([]string, 0, len(agentEnv))
	for _, item := range agentEnv {
		key, value, ok := strings.Cut(item, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || key == "" {
			return nil, nil, fmt.Errorf("invalid Harbor agent environment assignment: %s", commandlog.RedactText(item))
		}
		if isSensitiveEnvName(key) {
			if isEnvTemplate(value) {
				args = append(args, key+"="+value)
				continue
			}
			processEnv = upsertProcessEnv(processEnv, key, value)
			args = append(args, key+"=${"+key+"}")
			continue
		}
		args = append(args, key+"="+value)
	}
	return args, processEnv, nil
}

func isSensitiveEnvName(key string) bool {
	key = strings.ToUpper(strings.TrimSpace(key))
	if key == "" || strings.HasSuffix(key, "_TOKENS") || strings.HasSuffix(key, "_KEYS") {
		return false
	}
	for _, marker := range []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "CREDENTIAL", "AUTH"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

func isEnvTemplate(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") && len(value) > 3
}

func upsertProcessEnv(values []string, key, value string) []string {
	out := make([]string, 0, len(values)+1)
	for _, item := range values {
		itemKey, _, ok := strings.Cut(item, "=")
		if ok && strings.EqualFold(strings.TrimSpace(itemKey), key) {
			continue
		}
		out = append(out, item)
	}
	return append(out, key+"="+value)
}

func runHarborCommand(ctx context.Context, exec executor.CommandRunner, timeout time.Duration, outputDir string, env []string, progress func(line, source string), args []string) executor.Result {
	if progress == nil {
		return exec.Run(ctx, timeout, "", env, "harbor", args...)
	}
	livePath := filepath.Join(outputDir, "live.log")
	live, err := os.OpenFile(livePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return executor.Result{Command: strings.Join(append([]string{"harbor"}, args...), " "), ExitCode: -1, Err: err}
	}
	defer live.Close()
	return exec.RunStreamingWithOutput(ctx, timeout, "", env, io.Discard, func(line, source string) {
		redacted := commandlog.RedactText(line)
		_, _ = live.WriteString(redacted)
		if !strings.HasSuffix(redacted, "\n") {
			_, _ = live.WriteString("\n")
		}
		progress(redacted, source)
	}, "harbor", args...)
}

func executeHarborCommand(ctx context.Context, exec executor.CommandRunner, timeout time.Duration, outputDir, name string, env, agentEnv []string, progress func(line, source string), args []string) (domain.CommandRun, string, error) {
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return domain.CommandRun{}, "", err
	}
	start := time.Now().UTC()
	result := runHarborCommand(ctx, exec, timeout, outputDir, env, progress, args)
	finished := time.Now().UTC()
	stdout := commandlog.RedactText(result.Stdout)
	stderr := commandlog.RedactText(result.Stderr)
	envSummary := append(commandlog.EffectiveEnv(env), agentEnv...)
	run := domain.CommandRun{
		Name:         name,
		Command:      commandlog.RedactText(result.Command),
		Argv:         commandlog.RedactArgv(append([]string{"harbor"}, args...)),
		Dir:          commandlog.ResolveCWD(""),
		Env:          commandlog.RedactEnv(envSummary),
		Attempt:      1,
		ExitCode:     result.ExitCode,
		Stdout:       stdout,
		Stderr:       stderr,
		Timeout:      result.Timeout,
		FailureClass: commandlog.ClassifyFailure(result.ExitCode, result.Timeout, stdout, stderr),
		StartedAt:    start,
		FinishedAt:   finished,
		DurationMS:   finished.Sub(start).Milliseconds(),
		Passed:       result.Err == nil && !result.Timeout,
	}
	if result.Err != nil && run.ExitCode == 0 {
		run.ExitCode = -1
	}
	path, err := writeCommandArtifacts(outputDir, &run)
	if err != nil {
		return run, path, err
	}
	if result.Err != nil {
		return run, path, result.Err
	}
	return run, path, nil
}

func validateRunCompletion(result domain.TrialResult) []string {
	var failures []string
	if result.Trials != 4 {
		failures = append(failures, fmt.Sprintf("harbor result must contain 4 trials, got %d", result.Trials))
	}
	if len(result.Runs) != 4 {
		failures = append(failures, fmt.Sprintf("harbor result runs must contain 4 records, got %d", len(result.Runs)))
	}
	for _, run := range result.Runs {
		if strings.TrimSpace(run.FailureReason) != "" {
			failures = append(failures, fmt.Sprintf("harbor trial %d errored: %s", run.Trial, compactDiagnostic(run.FailureReason, 600)))
		}
	}
	return failures
}

func validateInstallPreflight(jobsDir string) (string, error) {
	resultPath := findJobResult(jobsDir)
	if resultPath == "" {
		return "", fmt.Errorf("install-only result.json not found under jobs directory")
	}
	result, err := ParseFile(resultPath)
	if err != nil {
		return resultPath, fmt.Errorf("parse install-only result: %w", err)
	}
	if len(result.Runs) == 0 {
		return resultPath, fmt.Errorf("install-only result contains no trial records")
	}
	for _, run := range result.Runs {
		if reason := strings.TrimSpace(run.FailureReason); reason != "" {
			return resultPath, fmt.Errorf("install-only trial %d errored: %s", run.Trial, compactDiagnostic(commandlog.RedactText(reason), 600))
		}
	}
	return resultPath, nil
}

func compactDiagnostic(value string, limit int) string {
	value = strings.TrimSpace(value)
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		value = strings.TrimSpace(value[:index])
	}
	if limit > 0 && len(value) > limit {
		value = value[:limit] + "..."
	}
	return value
}

type jobProgress struct {
	Total     int
	Completed int
	Errored   int
	Running   int
	Pending   int
	Cancelled int
	Retries   int
}

func startJobProgressMonitor(ctx context.Context, jobsDir, outputDir string, progress func(line, source string)) func() string {
	if progress == nil {
		progress = func(string, string) {}
	}
	monitor := newJobMonitor(jobsDir, outputDir, progress)
	monitorCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-monitorCtx.Done():
				return
			case <-ticker.C:
				monitor.poll(false)
			}
		}
	}()
	return func() string {
		cancel()
		<-done
		monitor.poll(true)
		return monitor.retryEvidenceManifestPath()
	}
}

func readLatestJobLogLine(jobsDir string) (string, bool) {
	var latestPath string
	var latestMod time.Time
	_ = filepath.WalkDir(jobsDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() != "job.log" {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr == nil && (latestPath == "" || info.ModTime().After(latestMod)) {
			latestPath = path
			latestMod = info.ModTime()
		}
		return nil
	})
	if latestPath == "" {
		return "", false
	}
	file, err := os.Open(latestPath)
	if err != nil {
		return "", false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return "", false
	}
	const tailBytes int64 = 16 << 10
	start := info.Size() - tailBytes
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", false
	}
	raw, err := io.ReadAll(io.LimitReader(file, tailBytes))
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(raw), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if line != "" {
			return line, true
		}
	}
	return "", false
}

func readJobProgress(jobsDir string) (jobProgress, bool) {
	path := findJobResult(jobsDir)
	if path == "" {
		return jobProgress{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return jobProgress{}, false
	}
	var payload struct {
		Total int `json:"n_total_trials"`
		Stats struct {
			Completed int `json:"n_completed_trials"`
			Errored   int `json:"n_errored_trials"`
			Running   int `json:"n_running_trials"`
			Pending   int `json:"n_pending_trials"`
			Cancelled int `json:"n_cancelled_trials"`
			Retries   int `json:"n_retries"`
		} `json:"stats"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return jobProgress{}, false
	}
	return jobProgress{
		Total:     payload.Total,
		Completed: payload.Stats.Completed,
		Errored:   payload.Stats.Errored,
		Running:   payload.Stats.Running,
		Pending:   payload.Stats.Pending,
		Cancelled: payload.Stats.Cancelled,
		Retries:   payload.Stats.Retries,
	}, true
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

func parseCommandResult(outputDir, jobsDir, stdout, stderr string) (domain.TrialResult, error) {
	if result, err := ParseResult([]byte(stdout)); err == nil {
		result.ResultPath = filepath.Join(outputDir, "stdout.txt")
		return result, nil
	}
	resultPath := findResultPath(stdout + "\n" + stderr)
	if resultPath == "" {
		resultPath = findJobResult(jobsDir)
	}
	if resultPath == "" {
		return domain.TrialResult{}, fmt.Errorf("harbor result JSON path not found in stdout/stderr or jobs directory")
	}
	return ParseFile(resultPath)
}

func findJobResult(jobsDir string) string {
	type candidate struct {
		path  string
		depth int
		mod   time.Time
	}
	var candidates []candidate
	_ = filepath.WalkDir(jobsDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() != "result.json" {
			return nil
		}
		rel, relErr := filepath.Rel(jobsDir, path)
		if relErr != nil {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil
		}
		candidates = append(candidates, candidate{path: path, depth: len(strings.Split(filepath.ToSlash(rel), "/")), mod: info.ModTime()})
		return nil
	})
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].depth != candidates[j].depth {
			return candidates[i].depth < candidates[j].depth
		}
		return candidates[i].mod.After(candidates[j].mod)
	})
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0].path
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
