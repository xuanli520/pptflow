package harborrun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/secretscan"
)

func ParseFile(path string) (domain.TrialResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.TrialResult{}, err
	}
	result, err := parseResult(raw, path)
	if err != nil {
		return domain.TrialResult{}, err
	}
	if result.ResultPath == "" {
		result.ResultPath = path
	}
	return result, nil
}

func ParseResult(raw []byte) (domain.TrialResult, error) {
	return parseResult(raw, "")
}

func parseResult(raw []byte, path string) (domain.TrialResult, error) {
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return domain.TrialResult{}, fmt.Errorf("decode result JSON: %w", err)
	}
	switch {
	case isHarborJobResult(decoded):
		return parseHarborJobResult(decoded, path, raw)
	case isHarborTrialResult(decoded):
		return parseHarborTrialResult(decoded, path, raw)
	default:
		return parseNormalizedResult(decoded), nil
	}
}

func parseNormalizedResult(decoded map[string]any) domain.TrialResult {
	result := domain.TrialResult{
		SchemaVersion:      stringValue(decoded, "schema_version"),
		Model:              firstString(decoded, "model", "model_name"),
		Agent:              firstString(decoded, "agent", "agent_name"),
		Trials:             firstInt(decoded, "trials", "trial_count", "num_trials", "n"),
		PassCount:          firstInt(decoded, "pass_count", "passed", "success_count"),
		PassAt4:            firstFloat(decoded, "pass_at_4", "pass@4", "pass_at_k", "pass_rate"),
		AverageTurns:       firstFloat(decoded, "average_turns", "avg_turns", "mean_turns"),
		ResultPath:         firstString(decoded, "result_path"),
		RawResultPath:      firstString(decoded, "raw_result_path"),
		RawResultSHA256:    firstString(decoded, "raw_result_sha256"),
		RawTrialResults:    parseResultFileEvidence(decoded["raw_trial_results"]),
		TaskDigest:         firstString(decoded, "task_digest", "task_sha256", "task_hash"),
		HarborTaskChecksum: firstString(decoded, "harbor_task_checksum", "task_checksum"),
		TaskPath:           firstString(decoded, "task_path"),
		CommandRunPath:     firstString(decoded, "command_run_path", "command_path"),
		Screenshot:         firstString(decoded, "screenshot", "screenshot_path", "pass4_screenshot"),
	}
	result.Runs = parseRuns(decoded["runs"])
	if result.SchemaVersion == "" {
		result.SchemaVersion = "harbor.trial_result.v1"
	}
	if result.Trials == 0 && len(result.Runs) > 0 {
		result.Trials = len(result.Runs)
	}
	if result.PassCount == 0 && len(result.Runs) > 0 {
		for _, run := range result.Runs {
			if run.Passed {
				result.PassCount++
			}
		}
	}
	if result.PassAt4 == 0 && result.Trials > 0 {
		result.PassAt4 = float64(result.PassCount) / float64(result.Trials)
	}
	if result.AverageTurns == 0 && len(result.Runs) > 0 {
		total := 0
		count := 0
		for _, run := range result.Runs {
			if run.Turns > 0 {
				total += run.Turns
				count++
			}
		}
		if count > 0 {
			result.AverageTurns = float64(total) / float64(count)
		}
	}
	return result
}

func isHarborJobResult(decoded map[string]any) bool {
	if _, ok := decoded["stats"].(map[string]any); !ok {
		return false
	}
	_, hasTotal := decoded["n_total_trials"]
	_, hasEvals := decoded["evals"]
	return hasTotal || hasEvals
}

func isHarborTrialResult(decoded map[string]any) bool {
	if _, ok := decoded["agent_info"].(map[string]any); !ok {
		return false
	}
	return stringValue(decoded, "trial_name") != "" && stringValue(decoded, "task_checksum") != ""
}

func parseHarborJobResult(decoded map[string]any, path string, raw []byte) (domain.TrialResult, error) {
	result := domain.TrialResult{
		SchemaVersion:   "harbor.trial_result.v1",
		Trials:          firstInt(decoded, "n_total_trials"),
		ResultPath:      path,
		RawResultPath:   path,
		RawResultSHA256: sha256Evidence(raw),
	}
	trials, err := parseHarborJobTrials(decoded, path)
	if err != nil {
		return domain.TrialResult{}, err
	}
	if len(trials) == 0 {
		result.Agent, result.Model = agentModelFromJobStats(decoded)
		result.PassCount = passCountFromJobStats(decoded)
		if result.Trials > 0 {
			result.PassAt4 = float64(result.PassCount) / float64(result.Trials)
		}
		return result, nil
	}
	sort.SliceStable(trials, func(i, j int) bool {
		return trialSortKey(trials[i]) < trialSortKey(trials[j])
	})
	result.Runs = make([]domain.TrialRun, 0, len(trials))
	totalTurns := 0
	turnCount := 0
	for idx, trial := range trials {
		if result.Agent == "" {
			result.Agent = trial.Agent
		}
		if result.Model == "" {
			result.Model = trial.Model
		}
		if result.TaskPath == "" {
			result.TaskPath = trial.TaskPath
		}
		if result.HarborTaskChecksum == "" {
			result.HarborTaskChecksum = trial.HarborTaskChecksum
		}
		if len(trial.RawTrialResults) > 0 {
			result.RawTrialResults = append(result.RawTrialResults, trial.RawTrialResults...)
		}
		for _, run := range trial.Runs {
			if run.Trial <= 0 {
				run.Trial = idx + 1
			}
			result.Runs = append(result.Runs, run)
			if run.Passed {
				result.PassCount++
			}
			if run.Turns > 0 {
				totalTurns += run.Turns
				turnCount++
			}
		}
	}
	if len(result.Runs) > 0 {
		result.Trials = len(result.Runs)
		result.PassAt4 = float64(result.PassCount) / float64(len(result.Runs))
	}
	if turnCount == len(result.Runs) && len(result.Runs) > 0 {
		result.AverageTurns = float64(totalTurns) / float64(len(result.Runs))
	}
	if result.TaskPath != "" {
		if digest, err := ComputeTaskDigest(result.TaskPath); err == nil {
			result.TaskDigest = digest
		}
	}
	return result, nil
}

func parseHarborJobTrials(decoded map[string]any, path string) ([]domain.TrialResult, error) {
	var trials []domain.TrialResult
	for _, item := range arrayValue(decoded["trial_results"]) {
		object, ok := item.(map[string]any)
		if !ok || !isHarborTrialResult(object) {
			continue
		}
		trial, err := parseHarborTrialResult(object, "", nil)
		if err != nil {
			return nil, err
		}
		trials = append(trials, trial)
	}
	if len(trials) > 0 || strings.TrimSpace(path) == "" {
		return trials, nil
	}
	jobDir := filepath.Dir(path)
	var resultPaths []string
	err := filepath.WalkDir(jobDir, func(candidate string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if candidate != jobDir && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "result.json" || sameFilesystemPath(candidate, "", path) {
			return nil
		}
		resultPaths = append(resultPaths, candidate)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(resultPaths)
	for _, resultPath := range resultPaths {
		raw, err := os.ReadFile(resultPath)
		if err != nil {
			return nil, err
		}
		var object map[string]any
		if err := json.Unmarshal(raw, &object); err != nil || !isHarborTrialResult(object) {
			continue
		}
		trial, err := parseHarborTrialResult(object, resultPath, raw)
		if err != nil {
			return nil, err
		}
		trials = append(trials, trial)
	}
	return trials, nil
}

func parseHarborTrialResult(decoded map[string]any, path string, raw []byte) (domain.TrialResult, error) {
	reward, hasReward := verifierReward(decoded)
	failureReason := harborFailureReason(decoded)
	turns := harborTurns(decoded, path)
	passed := hasReward && reward >= 1 && failureReason == ""
	run := domain.TrialRun{
		Trial:           firstInt(decoded, "trial", "trial_number", "trial_index", "attempt"),
		Passed:          passed,
		Turns:           turns,
		DurationSeconds: trialDurationSeconds(decoded),
		Reward:          reward,
		FailureReason:   failureReason,
	}
	result := domain.TrialResult{
		SchemaVersion:      "harbor.trial_result.v1",
		Model:              harborModel(decoded),
		Agent:              harborAgent(decoded),
		Trials:             1,
		PassCount:          boolInt(passed),
		PassAt4:            float64(boolInt(passed)),
		AverageTurns:       float64(turns),
		Runs:               []domain.TrialRun{run},
		ResultPath:         path,
		RawResultPath:      path,
		RawResultSHA256:    sha256Evidence(raw),
		TaskPath:           harborTaskPath(decoded),
		HarborTaskChecksum: firstString(decoded, "task_checksum"),
	}
	if path != "" && len(raw) > 0 {
		result.RawTrialResults = []domain.ResultFileEvidence{{Path: path, SHA256: result.RawResultSHA256}}
	}
	if result.TaskPath != "" {
		if digest, err := ComputeTaskDigest(result.TaskPath); err == nil {
			result.TaskDigest = digest
		}
	}
	return result, nil
}

func agentModelFromJobStats(decoded map[string]any) (string, string) {
	stats, ok := decoded["stats"].(map[string]any)
	if !ok {
		return "", ""
	}
	evals, ok := stats["evals"].(map[string]any)
	if !ok {
		return "", ""
	}
	var keys []string
	for key := range evals {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "", ""
	}
	parts := strings.Split(keys[0], "__")
	if len(parts) >= 3 {
		return parts[0], parts[1]
	}
	if len(parts) >= 1 {
		return parts[0], ""
	}
	return "", ""
}

func passCountFromJobStats(decoded map[string]any) int {
	stats, ok := decoded["stats"].(map[string]any)
	if !ok {
		return 0
	}
	evals, ok := stats["evals"].(map[string]any)
	if !ok {
		return 0
	}
	passCount := 0
	for _, eval := range evals {
		evalObject, ok := eval.(map[string]any)
		if !ok {
			continue
		}
		rewardStats, ok := evalObject["reward_stats"].(map[string]any)
		if !ok {
			continue
		}
		for _, byValue := range rewardStats {
			valueObject, ok := byValue.(map[string]any)
			if !ok {
				continue
			}
			for rewardText, trials := range valueObject {
				reward, err := strconv.ParseFloat(strings.TrimSpace(rewardText), 64)
				if err != nil || reward < 1 {
					continue
				}
				passCount += len(arrayValue(trials))
			}
		}
	}
	return passCount
}

func harborAgent(decoded map[string]any) string {
	if agentInfo, ok := decoded["agent_info"].(map[string]any); ok {
		if value := stringValue(agentInfo, "name"); value != "" {
			return value
		}
	}
	if config, ok := decoded["config"].(map[string]any); ok {
		if agent, ok := config["agent"].(map[string]any); ok {
			return stringValue(agent, "name")
		}
	}
	return ""
}

func harborModel(decoded map[string]any) string {
	if agentInfo, ok := decoded["agent_info"].(map[string]any); ok {
		if modelInfo, ok := agentInfo["model_info"].(map[string]any); ok {
			if value := stringValue(modelInfo, "name"); value != "" {
				return value
			}
		}
	}
	if config, ok := decoded["config"].(map[string]any); ok {
		if agent, ok := config["agent"].(map[string]any); ok {
			return stringValue(agent, "model_name")
		}
	}
	return ""
}

func harborTaskPath(decoded map[string]any) string {
	if taskID, ok := decoded["task_id"].(map[string]any); ok {
		if value := stringValue(taskID, "path"); value != "" {
			return value
		}
	}
	if config, ok := decoded["config"].(map[string]any); ok {
		if task, ok := config["task"].(map[string]any); ok {
			return stringValue(task, "path")
		}
	}
	return ""
}

func verifierReward(decoded map[string]any) (float64, bool) {
	if verifier, ok := decoded["verifier_result"].(map[string]any); ok {
		if reward, ok := rewardFromVerifier(verifier); ok {
			return reward, true
		}
	}
	for _, item := range arrayValue(decoded["step_results"]) {
		step, ok := item.(map[string]any)
		if !ok {
			continue
		}
		verifier, ok := step["verifier_result"].(map[string]any)
		if !ok {
			continue
		}
		if reward, ok := rewardFromVerifier(verifier); ok {
			return reward, true
		}
	}
	return 0, false
}

func rewardFromVerifier(verifier map[string]any) (float64, bool) {
	rewards, ok := verifier["rewards"].(map[string]any)
	if !ok || len(rewards) == 0 {
		return 0, false
	}
	var reward float64
	first := true
	for _, value := range rewards {
		current := floatValue(value)
		if first || current < reward {
			reward = current
			first = false
		}
	}
	return reward, !first
}

func harborFailureReason(decoded map[string]any) string {
	if reason := exceptionReason(decoded["exception_info"]); reason != "" {
		return reason
	}
	for _, item := range arrayValue(decoded["step_results"]) {
		step, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if reason := exceptionReason(step["exception_info"]); reason != "" {
			stepName := firstString(step, "step_name", "name")
			if stepName != "" {
				return stepName + ": " + reason
			}
			return reason
		}
	}
	return ""
}

func exceptionReason(value any) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	exceptionType := firstString(object, "exception_type", "type")
	message := firstString(object, "exception_message", "message")
	switch {
	case exceptionType != "" && message != "":
		return exceptionType + ": " + message
	case exceptionType != "":
		return exceptionType
	default:
		return message
	}
}

func harborTurns(decoded map[string]any, path string) int {
	turns := turnsFromAgentContext(decoded["agent_result"])
	for _, item := range arrayValue(decoded["step_results"]) {
		step, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if stepTurns := turnsFromAgentContext(step["agent_result"]); stepTurns > turns {
			turns = stepTurns
		}
	}
	if turns == 0 && path != "" {
		turns = turnsFromTrajectoryFiles(filepath.Dir(path))
	}
	return turns
}

func turnsFromAgentContext(value any) int {
	context, ok := value.(map[string]any)
	if !ok {
		return 0
	}
	if turns := firstInt(context, "turns", "turn_count", "n_turns", "num_turns", "total_turns"); turns > 0 {
		return turns
	}
	if metadata, ok := context["metadata"].(map[string]any); ok {
		if turns := firstInt(metadata, "turns", "turn_count", "n_turns", "num_turns", "total_turns"); turns > 0 {
			return turns
		}
	}
	turns := 0
	for _, rollout := range arrayValue(context["rollout_details"]) {
		rolloutObject, ok := rollout.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"completion_token_ids", "prompt_token_ids"} {
			if count := len(arrayValue(rolloutObject[key])); count > turns {
				turns = count
			}
		}
	}
	return turns
}

func turnsFromTrajectoryFiles(trialDir string) int {
	trialDir = strings.TrimSpace(trialDir)
	if trialDir == "" {
		return 0
	}
	turns := 0
	seen := 0
	_ = filepath.WalkDir(trialDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "trajectory") || !strings.HasSuffix(name, ".json") {
			return nil
		}
		seen++
		if seen > 20 {
			return filepath.SkipAll
		}
		if info, err := entry.Info(); err == nil && info.Size() > 20*1024*1024 {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var object map[string]any
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil
		}
		if fileTurns := turnsFromTrajectory(object); fileTurns > turns {
			turns = fileTurns
		}
		return nil
	})
	return turns
}

func turnsFromTrajectory(object map[string]any) int {
	if metrics, ok := object["final_metrics"].(map[string]any); ok {
		if turns := firstInt(metrics, "total_steps"); turns > 0 {
			return turns
		}
	}
	return len(arrayValue(object["steps"]))
}

func trialDurationSeconds(decoded map[string]any) int {
	started, okStart := parseHarborTime(firstString(decoded, "started_at"))
	finished, okFinished := parseHarborTime(firstString(decoded, "finished_at"))
	if !okStart || !okFinished || finished.Before(started) {
		return 0
	}
	return int(finished.Sub(started).Seconds())
}

func parseHarborTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, true
	}
	if parsed, err := time.Parse("2006-01-02T15:04:05.999999", value); err == nil {
		return parsed, true
	}
	if parsed, err := time.Parse("2006-01-02T15:04:05", value); err == nil {
		return parsed, true
	}
	return time.Time{}, false
}

func trialSortKey(result domain.TrialResult) string {
	if len(result.Runs) > 0 && result.Runs[0].Trial > 0 {
		return fmt.Sprintf("%08d", result.Runs[0].Trial)
	}
	if result.RawResultPath != "" {
		return result.RawResultPath
	}
	if result.ResultPath != "" {
		return result.ResultPath
	}
	if result.TaskPath != "" {
		return result.TaskPath
	}
	return result.HarborTaskChecksum
}

func sha256Evidence(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func parseResultFileEvidence(value any) []domain.ResultFileEvidence {
	items := arrayValue(value)
	if len(items) == 0 {
		return nil
	}
	out := make([]domain.ResultFileEvidence, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		evidence := domain.ResultFileEvidence{
			Path:   firstString(object, "path"),
			SHA256: firstString(object, "sha256", "sha256sum", "digest"),
		}
		if evidence.Path != "" || evidence.SHA256 != "" {
			out = append(out, evidence)
		}
	}
	return out
}

func ValidateForCodeEdge(result domain.TrialResult, qwen bool) []string {
	return ValidateForCodeEdgeWithOptions(result, ValidationOptions{Qwen: qwen})
}

type ValidationOptions struct {
	Qwen              bool
	ExpectedModel     string
	TaskDir           string
	RequireRuns       bool
	RequireTaskDigest bool
	RequireCommandRun bool
	ExpectedAgent     string
}

func ValidateForCodeEdgeWithOptions(result domain.TrialResult, opts ValidationOptions) []string {
	var failures []string
	if result.Trials != 4 {
		failures = append(failures, fmt.Sprintf("trials must be 4, got %d", result.Trials))
	}
	expectedModel := strings.TrimSpace(opts.ExpectedModel)
	if expectedModel == "" {
		if opts.Qwen {
			expectedModel = "qwen"
		} else {
			expectedModel = "opus"
		}
	}
	if !modelMatches(result.Model, expectedModel) {
		failures = append(failures, fmt.Sprintf("model %q does not match expected %q", result.Model, expectedModel))
	}
	if opts.Qwen && result.PassCount > 1 {
		failures = append(failures, fmt.Sprintf("Qwen pass_count must be <= 1, got %d", result.PassCount))
	}
	if opts.Qwen && result.PassAt4 > 0.25 {
		failures = append(failures, fmt.Sprintf("Qwen pass@4 must be <= 0.25, got %.2f", result.PassAt4))
	}
	if result.AverageTurns < 20 {
		failures = append(failures, fmt.Sprintf("average_turns must be >= 20, got %.2f", result.AverageTurns))
	}
	failures = append(failures, validateRuns(result, opts.RequireRuns)...)
	if opts.RequireTaskDigest || strings.TrimSpace(opts.TaskDir) != "" {
		failures = append(failures, validateTaskDigest(result, opts.TaskDir)...)
	}
	if opts.RequireCommandRun {
		failures = append(failures, validateCommandRun(result, expectedModel, opts.ExpectedAgent, opts.TaskDir)...)
	}
	return failures
}

func validateCommandRun(result domain.TrialResult, expectedModel, expectedAgent, taskDir string) []string {
	path := strings.TrimSpace(result.CommandRunPath)
	if path == "" {
		return []string{"command_run_path is required"}
	}
	if !filepath.IsAbs(path) && strings.TrimSpace(result.ResultPath) != "" {
		candidate := filepath.Join(filepath.Dir(result.ResultPath), path)
		if _, err := os.Stat(candidate); err == nil {
			path = candidate
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return []string{"command_run_path cannot be read: " + err.Error()}
	}
	if findings := secretscan.ScanBytes(filepath.Base(path), raw); len(findings) > 0 {
		return []string{"command_run_path contains secret-like values: " + secretscan.Summary(findings, 3)}
	}
	var commandRun domain.CommandRun
	if err := json.Unmarshal(raw, &commandRun); err != nil {
		return []string{"command_run_path cannot be parsed: " + err.Error()}
	}
	argv := commandRun.Argv
	var failures []string
	if !argvHasPrefix(argv, "harbor", "run") {
		failures = append(failures, "command_run argv must start with harbor run")
	}
	if strings.TrimSpace(taskDir) != "" {
		taskPath, ok := argvFlagText(argv, "-p")
		if !ok || !sameFilesystemPath(taskPath, commandRun.Dir, taskDir) {
			failures = append(failures, "command_run argv must include current task path with -p")
		}
	}
	if !argvFlagValue(argv, "-m", expectedModel) {
		failures = append(failures, "command_run argv must include expected model "+expectedModel)
	}
	if expectedAgent = strings.TrimSpace(expectedAgent); expectedAgent == "" {
		expectedAgent = strings.TrimSpace(result.Agent)
	}
	if expectedAgent == "" {
		expectedAgent = "claude-code"
	}
	if !argvFlagValue(argv, "-a", expectedAgent) {
		failures = append(failures, "command_run argv must include expected agent "+expectedAgent)
	}
	if !argvFlagValue(argv, "-n", "4") {
		failures = append(failures, "command_run argv must include -n 4")
	}
	if !argvFlagValue(argv, "-k", "4") {
		failures = append(failures, "command_run argv must include -k 4")
	}
	if commandRun.ExitCode != 0 || !commandRun.Passed {
		failures = append(failures, "command_run must show successful harbor run")
	}
	failures = append(failures, validateCommandOutputFile(path, "stdout_path", commandRun.StdoutPath, commandRun.Stdout)...)
	failures = append(failures, validateCommandOutputFile(path, "stderr_path", commandRun.StderrPath, commandRun.Stderr)...)
	failures = append(failures, validateRawResultEvidence(result, path, commandRun)...)
	return failures
}

func validateRawResultEvidence(result domain.TrialResult, commandRunPath string, commandRun domain.CommandRun) []string {
	if strings.TrimSpace(result.RawResultPath) == "" && len(result.RawTrialResults) == 0 {
		return []string{"raw_result_path is required"}
	}
	var failures []string
	var rawResults []domain.TrialResult
	bases := []string{filepath.Dir(commandRunPath)}
	if strings.TrimSpace(result.ResultPath) != "" {
		bases = append([]string{filepath.Dir(result.ResultPath)}, bases...)
	}
	commandOutput := commandRun.Stdout + "\n" + commandRun.Stderr
	if strings.TrimSpace(result.RawResultPath) != "" {
		resolved, rawFailures := validateEvidenceFile("raw_result_path", result.RawResultPath, result.RawResultSHA256, bases)
		failures = append(failures, rawFailures...)
		if resolved != "" && !commandOutputReferencesPath(commandOutput, result.RawResultPath, resolved) {
			failures = append(failures, "command_run stdout/stderr must reference raw_result_path")
		}
		if resolved != "" && len(rawFailures) == 0 {
			rawResult, err := ParseFile(resolved)
			if err != nil {
				failures = append(failures, "raw_result_path cannot be parsed: "+err.Error())
			} else {
				rawResults = append(rawResults, rawResult)
			}
		}
	}
	for idx, evidence := range result.RawTrialResults {
		label := fmt.Sprintf("raw_trial_results[%d]", idx)
		resolved, itemFailures := validateEvidenceFile(label, evidence.Path, evidence.SHA256, bases)
		failures = append(failures, itemFailures...)
		if resolved != "" && len(itemFailures) == 0 {
			rawResult, err := ParseFile(resolved)
			if err != nil {
				failures = append(failures, label+" cannot be parsed: "+err.Error())
			} else {
				rawResults = append(rawResults, rawResult)
			}
		}
	}
	failures = append(failures, validateRawResultsMatchNormalized(result, rawResults)...)
	return failures
}

func validateRawResultsMatchNormalized(result domain.TrialResult, rawResults []domain.TrialResult) []string {
	raw, ok := selectRawResultForComparison(result, rawResults)
	if !ok {
		return nil
	}
	var failures []string
	hasRawCounts := raw.Trials > 0 || len(raw.Runs) > 0 || raw.PassAt4 > 0
	if raw.Trials != 0 && result.Trials != raw.Trials {
		failures = append(failures, fmt.Sprintf("raw_result_path trials %d does not match normalized trials %d", raw.Trials, result.Trials))
	}
	if hasRawCounts && raw.PassCount != result.PassCount {
		failures = append(failures, fmt.Sprintf("raw_result_path pass_count %d does not match normalized pass_count %d", raw.PassCount, result.PassCount))
	}
	if raw.PassAt4 > 0 && absFloat(raw.PassAt4-result.PassAt4) > 0.01 {
		failures = append(failures, fmt.Sprintf("raw_result_path pass_at_4 %.2f does not match normalized pass_at_4 %.2f", raw.PassAt4, result.PassAt4))
	}
	if raw.AverageTurns > 0 && result.AverageTurns > 0 && absFloat(raw.AverageTurns-result.AverageTurns) > 0.01 {
		failures = append(failures, fmt.Sprintf("raw_result_path average_turns %.2f does not match normalized average_turns %.2f", raw.AverageTurns, result.AverageTurns))
	}
	if strings.TrimSpace(raw.Model) != "" && strings.TrimSpace(result.Model) != "" && !modelMatches(result.Model, raw.Model) && !modelMatches(raw.Model, result.Model) {
		failures = append(failures, fmt.Sprintf("raw_result_path model %q does not match normalized model %q", raw.Model, result.Model))
	}
	if strings.TrimSpace(raw.TaskDigest) != "" && strings.TrimSpace(result.TaskDigest) != "" && !strings.EqualFold(raw.TaskDigest, result.TaskDigest) {
		failures = append(failures, "raw_result_path task_digest does not match normalized task_digest")
	}
	if strings.TrimSpace(raw.HarborTaskChecksum) != "" && strings.TrimSpace(result.HarborTaskChecksum) != "" && raw.HarborTaskChecksum != result.HarborTaskChecksum {
		failures = append(failures, "raw_result_path harbor_task_checksum does not match normalized harbor_task_checksum")
	}
	failures = append(failures, compareRawRuns(result.Runs, raw.Runs)...)
	return failures
}

func selectRawResultForComparison(result domain.TrialResult, rawResults []domain.TrialResult) (domain.TrialResult, bool) {
	if len(rawResults) == 0 {
		return domain.TrialResult{}, false
	}
	for _, candidate := range rawResults {
		if result.Trials > 0 && len(candidate.Runs) == result.Trials {
			return candidate, true
		}
		if result.Trials > 0 && candidate.Trials == result.Trials && len(candidate.Runs) != 1 {
			return candidate, true
		}
	}
	if len(rawResults) == 1 {
		return rawResults[0], true
	}
	return aggregateRawTrialResults(rawResults), true
}

func aggregateRawTrialResults(rawResults []domain.TrialResult) domain.TrialResult {
	var out domain.TrialResult
	totalTurns := 0
	turnCount := 0
	for _, raw := range rawResults {
		if out.Model == "" {
			out.Model = raw.Model
		}
		if out.Agent == "" {
			out.Agent = raw.Agent
		}
		if out.TaskDigest == "" {
			out.TaskDigest = raw.TaskDigest
		}
		if out.HarborTaskChecksum == "" {
			out.HarborTaskChecksum = raw.HarborTaskChecksum
		}
		runs := raw.Runs
		if len(runs) == 0 && raw.Trials == 1 {
			runs = []domain.TrialRun{{Trial: 1, Passed: raw.PassCount > 0, Turns: int(raw.AverageTurns)}}
		}
		for _, run := range runs {
			if run.Trial <= 0 {
				run.Trial = len(out.Runs) + 1
			}
			out.Runs = append(out.Runs, run)
			if run.Passed {
				out.PassCount++
			}
			if run.Turns > 0 {
				totalTurns += run.Turns
				turnCount++
			}
		}
	}
	out.Trials = len(out.Runs)
	if out.Trials > 0 {
		out.PassAt4 = float64(out.PassCount) / float64(out.Trials)
	}
	if turnCount == len(out.Runs) && len(out.Runs) > 0 {
		out.AverageTurns = float64(totalTurns) / float64(len(out.Runs))
	}
	return out
}

func compareRawRuns(normalized, raw []domain.TrialRun) []string {
	if len(normalized) == 0 || len(raw) == 0 {
		return nil
	}
	if len(normalized) != len(raw) {
		return []string{fmt.Sprintf("raw_result_path runs count %d does not match normalized runs count %d", len(raw), len(normalized))}
	}
	rawByTrial := map[int]domain.TrialRun{}
	for _, run := range raw {
		rawByTrial[run.Trial] = run
	}
	var failures []string
	for _, run := range normalized {
		rawRun, ok := rawByTrial[run.Trial]
		if !ok {
			failures = append(failures, fmt.Sprintf("raw_result_path missing trial %d", run.Trial))
			continue
		}
		if rawRun.Passed != run.Passed {
			failures = append(failures, fmt.Sprintf("raw_result_path trial %d pass state does not match normalized result", run.Trial))
		}
		if rawRun.Turns > 0 && run.Turns > 0 && rawRun.Turns != run.Turns {
			failures = append(failures, fmt.Sprintf("raw_result_path trial %d turns %d does not match normalized turns %d", run.Trial, rawRun.Turns, run.Turns))
		}
	}
	return failures
}

func validateEvidenceFile(label, path, expectedHash string, bases []string) (string, []string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", []string{label + " path is required"}
	}
	resolved := resolveEvidencePath(path, bases)
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return resolved, []string{label + " cannot be read: " + err.Error()}
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() {
		return resolved, []string{label + " is not a regular readable file"}
	}
	if findings := secretscan.ScanBytes(filepath.Base(resolved), raw); len(findings) > 0 {
		return resolved, []string{label + " contains secret-like values: " + secretscan.Summary(findings, 3)}
	}
	if strings.TrimSpace(expectedHash) == "" {
		return resolved, []string{label + " sha256 is required"}
	}
	actual := sha256Evidence(raw)
	if !hashMatches(actual, expectedHash) {
		return resolved, []string{label + " sha256 does not match file content"}
	}
	return resolved, nil
}

func resolveEvidencePath(path string, bases []string) string {
	if filepath.IsAbs(path) {
		return path
	}
	for _, base := range bases {
		base = strings.TrimSpace(base)
		if base == "" {
			continue
		}
		candidate := filepath.Join(base, path)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return path
}

func commandOutputReferencesPath(output, original, resolved string) bool {
	output = strings.TrimSpace(output)
	if output == "" {
		return false
	}
	candidates := []string{strings.TrimSpace(original), strings.TrimSpace(resolved)}
	if abs, err := filepath.Abs(resolved); err == nil {
		candidates = append(candidates, abs)
	}
	for _, candidate := range candidates {
		if candidate != "" && strings.Contains(output, candidate) {
			return true
		}
	}
	return false
}

func hashMatches(actual, expected string) bool {
	actual = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(actual)), "sha256:")
	expected = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(expected)), "sha256:")
	return actual != "" && actual == expected
}

func validateCommandOutputFile(commandRunPath, label, outputPath, expected string) []string {
	outputPath = strings.TrimSpace(outputPath)
	if outputPath == "" {
		return []string{label + " is required"}
	}
	resolved := outputPath
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(commandRunPath), resolved)
	}
	if !pathWithinDir(resolved, filepath.Dir(commandRunPath)) {
		return []string{label + " must stay under command_run directory"}
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return []string{label + " cannot be read: " + err.Error()}
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() {
		return []string{label + " is not a regular readable file"}
	}
	if findings := secretscan.ScanBytes(filepath.Base(resolved), raw); len(findings) > 0 {
		return []string{label + " contains secret-like values: " + secretscan.Summary(findings, 3)}
	}
	if string(raw) != expected {
		return []string{label + " content does not match command_run " + strings.TrimSuffix(label, "_path")}
	}
	return nil
}

func pathWithinDir(path, dir string) bool {
	path = strings.TrimSpace(path)
	dir = strings.TrimSpace(dir)
	if path == "" || dir == "" {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	if evaluated, err := filepath.EvalSymlinks(absPath); err == nil {
		absPath = evaluated
	}
	if evaluated, err := filepath.EvalSymlinks(absDir); err == nil {
		absDir = evaluated
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func argvFlagText(argv []string, flag string) (string, bool) {
	for i := 0; i < len(argv); i++ {
		item := strings.TrimSpace(argv[i])
		if item == flag && i+1 < len(argv) {
			return strings.TrimSpace(argv[i+1]), true
		}
		if strings.HasPrefix(item, flag+"=") {
			return strings.TrimSpace(strings.TrimPrefix(item, flag+"=")), true
		}
	}
	return "", false
}

func sameFilesystemPath(actual, cwd, expected string) bool {
	actual = strings.TrimSpace(actual)
	expected = strings.TrimSpace(expected)
	if actual == "" || expected == "" {
		return false
	}
	if !filepath.IsAbs(actual) && strings.TrimSpace(cwd) != "" {
		actual = filepath.Join(cwd, actual)
	}
	actualAbs, err := filepath.Abs(actual)
	if err != nil {
		return false
	}
	expectedAbs, err := filepath.Abs(expected)
	if err != nil {
		return false
	}
	if evaluated, err := filepath.EvalSymlinks(actualAbs); err == nil {
		actualAbs = evaluated
	}
	if evaluated, err := filepath.EvalSymlinks(expectedAbs); err == nil {
		expectedAbs = evaluated
	}
	return filepath.Clean(actualAbs) == filepath.Clean(expectedAbs)
}

func argvHasPrefix(argv []string, values ...string) bool {
	if len(argv) < len(values) {
		return false
	}
	for i, value := range values {
		if strings.TrimSpace(argv[i]) != value {
			return false
		}
	}
	return true
}

func argvFlagValue(argv []string, flag, value string) bool {
	value = strings.TrimSpace(value)
	for i := 0; i < len(argv); i++ {
		item := strings.TrimSpace(argv[i])
		if item == flag && i+1 < len(argv) && strings.TrimSpace(argv[i+1]) == value {
			return true
		}
		if strings.HasPrefix(item, flag+"=") && strings.TrimSpace(strings.TrimPrefix(item, flag+"=")) == value {
			return true
		}
	}
	return false
}

func validateRuns(result domain.TrialResult, require bool) []string {
	if len(result.Runs) == 0 {
		if require {
			return []string{"runs must contain 4 per-trial records"}
		}
		return nil
	}
	var failures []string
	if len(result.Runs) != 4 {
		failures = append(failures, fmt.Sprintf("runs must contain 4 records, got %d", len(result.Runs)))
	}
	seen := map[int]bool{}
	passCount := 0
	totalTurns := 0
	turnCount := 0
	for idx, run := range result.Runs {
		if run.Trial <= 0 {
			failures = append(failures, fmt.Sprintf("run %d missing trial number", idx+1))
		} else if seen[run.Trial] {
			failures = append(failures, fmt.Sprintf("duplicate trial number %d", run.Trial))
		}
		seen[run.Trial] = true
		if run.Turns <= 0 {
			failures = append(failures, fmt.Sprintf("trial %d missing positive turns", run.Trial))
		} else {
			totalTurns += run.Turns
			turnCount++
		}
		if run.Passed {
			passCount++
		}
	}
	if len(result.Runs) == 4 && result.PassCount != passCount {
		failures = append(failures, fmt.Sprintf("pass_count %d does not match per-trial passes %d", result.PassCount, passCount))
	}
	if len(result.Runs) == 4 {
		for trial := 1; trial <= 4; trial++ {
			if !seen[trial] {
				failures = append(failures, fmt.Sprintf("runs must include trial %d", trial))
			}
		}
		expectedPassAt4 := float64(passCount) / 4
		if absFloat(result.PassAt4-expectedPassAt4) > 0.01 {
			failures = append(failures, fmt.Sprintf("pass_at_4 %.2f does not match pass_count/4 %.2f", result.PassAt4, expectedPassAt4))
		}
	}
	if turnCount == len(result.Runs) && len(result.Runs) > 0 {
		average := float64(totalTurns) / float64(len(result.Runs))
		if result.AverageTurns > 0 && absFloat(result.AverageTurns-average) > 0.01 {
			failures = append(failures, fmt.Sprintf("average_turns %.2f does not match per-trial average %.2f", result.AverageTurns, average))
		}
	}
	return failures
}

func validateTaskDigest(result domain.TrialResult, taskDir string) []string {
	if strings.TrimSpace(result.TaskDigest) == "" {
		return []string{"task_digest is required"}
	}
	if strings.TrimSpace(taskDir) == "" {
		return nil
	}
	digest, err := ComputeTaskDigest(taskDir)
	if err != nil {
		return []string{"task digest cannot be computed: " + err.Error()}
	}
	if !strings.EqualFold(result.TaskDigest, digest) {
		return []string{"task_digest does not match current task"}
	}
	return nil
}

func modelMatches(actual, expected string) bool {
	actual = normalizeModel(actual)
	expected = normalizeModel(expected)
	if actual == "" || expected == "" {
		return false
	}
	if actual == expected {
		return true
	}
	switch expected {
	case "qwen":
		return strings.HasPrefix(actual, "qwen")
	case "opus":
		return actual == "opus" || strings.HasPrefix(actual, "claudeopus")
	default:
		return false
	}
}

func normalizeModel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer("-", "", "_", "", ".", "", " ", "")
	return replacer.Replace(value)
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func parseRuns(value any) []domain.TrialRun {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	runs := make([]domain.TrialRun, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		trial := intValue(object["trial"])
		runs = append(runs, domain.TrialRun{
			Trial:           trial,
			Passed:          boolValue(object["passed"]) || floatValue(object["reward"]) >= 1,
			Turns:           firstInt(object, "turns", "turn_count"),
			DurationSeconds: firstInt(object, "duration_seconds", "duration_sec"),
			Reward:          floatValue(object["reward"]),
			FailureReason:   firstString(object, "failure_reason", "error"),
		})
	}
	return runs
}

func firstString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(object, key); value != "" {
			return value
		}
	}
	return ""
}

func stringValue(object map[string]any, key string) string {
	value, ok := object[key]
	if !ok {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func firstInt(object map[string]any, keys ...string) int {
	for _, key := range keys {
		value := intValue(object[key])
		if value != 0 {
			return value
		}
	}
	return 0
}

func intValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		n, _ := typed.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(typed))
		return n
	default:
		return 0
	}
}

func firstFloat(object map[string]any, keys ...string) float64 {
	for _, key := range keys {
		value := floatValue(object[key])
		if value != 0 {
			return value
		}
	}
	return 0
}

func floatValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int:
		return float64(typed)
	case json.Number:
		n, _ := typed.Float64()
		return n
	case string:
		n, _ := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return n
	default:
		return 0
	}
}

func boolValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true") || strings.TrimSpace(typed) == "1"
	default:
		return false
	}
}

func arrayValue(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	default:
		return nil
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
