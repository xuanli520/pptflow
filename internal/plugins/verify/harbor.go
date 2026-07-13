package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/commandlog"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/evidence"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/harbor/sanitize"
	"github.com/purplevoid/harbor-factory/internal/plugins/pluginutil"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

const (
	HarborRunQwenPluginID = "harborfactory.harbor_run_qwen"
	HarborRunQwenKind     = "harborfactory.harbor_run_qwen"
	HarborRunOpusPluginID = "harborfactory.harbor_run_opus"
	HarborRunOpusKind     = "harborfactory.harbor_run_opus"
)

type HarborRunQwenPlugin struct{}
type HarborRunOpusPlugin struct{}

func (HarborRunQwenPlugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: HarborRunQwenPluginID, Version: "1.0.0", Kinds: []string{HarborRunQwenKind}}
}

func (HarborRunOpusPlugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: HarborRunOpusPluginID, Version: "1.0.0", Kinds: []string{HarborRunOpusKind}}
}

func (HarborRunQwenPlugin) Validate(spec workflow.NodeSpec) error { return validateHarborSpec(spec) }
func (HarborRunOpusPlugin) Validate(spec workflow.NodeSpec) error { return validateHarborSpec(spec) }

func validateHarborSpec(spec workflow.NodeSpec) error {
	if err := pluginutil.RequiredString(spec, "task_dir"); err != nil {
		return err
	}
	if err := pluginutil.RequiredString(spec, "model"); err != nil {
		return err
	}
	_, err := harborrun.NormalizePassPlan(pluginutil.IntValue(spec.Config["concurrency"]), pluginutil.IntValue(spec.Config["attempts"]))
	return err
}

func (HarborRunQwenPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	return executeHarbor(ctx, req, "qwen")
}

func (HarborRunOpusPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	return executeHarbor(ctx, req, "opus")
}

func executeHarbor(ctx context.Context, req workflow.NodeRequest, slot string) (workflow.NodeResult, error) {
	if req.Store == nil {
		return workflow.NodeResult{}, fmt.Errorf("harbor run artifact store is required")
	}
	if resultPath := strings.TrimSpace(pluginutil.String(req, "result_path")); resultPath != "" {
		return importHarborResult(ctx, req, slot, resultPath)
	}
	if req.Runtimes.Evaluation == nil {
		return workflow.NodeResult{}, fmt.Errorf("evaluation runtime is required")
	}
	model := pluginutil.String(req, "model")
	outputDir := pluginutil.String(req, "output_dir")
	if outputDir == "" {
		var err error
		outputDir, err = req.Store.Path(filepath.ToSlash(filepath.Join("phase3", "artifacts", req.Spec.ID, "runtime")))
		if err != nil {
			return workflow.NodeResult{}, err
		}
	}
	plan, err := harborrun.NormalizePassPlan(pluginutil.Int(req, "concurrency"), pluginutil.Int(req, "attempts"))
	if err != nil {
		return workflow.NodeResult{}, workflow.NewNodeError(workflow.FailurePermanent, false, "validate Harbor "+slot+" pass settings", err)
	}
	evaluation, runErr := req.Runtimes.Evaluation.Evaluate(ctx, workflow.EvaluationRequest{
		NodeID:              req.Spec.ID,
		TaskPath:            pluginutil.String(req, "task_dir"),
		Model:               model,
		Agent:               pluginutil.String(req, "agent"),
		AgentEnv:            pluginutil.Strings(req, "agent_env"),
		OutputDir:           outputDir,
		TimeoutSeconds:      pluginutil.Int(req, "timeout_seconds"),
		SetupTimeoutSeconds: pluginutil.Int(req, "setup_timeout_seconds"),
		AgentCacheDir:       pluginutil.String(req, "agent_cache_dir"),
		Preflight:           pluginutil.Bool(req, "preflight"),
		Concurrency:         plan.Concurrency,
		Attempts:            plan.Attempts,
		InfraRetries:        pluginutil.Int(req, "infra_retries"),
		Env:                 pluginutil.Strings(req, "env"),
		Progress: func(line, source string) {
			if req.Events == nil {
				return
			}
			_ = req.Events.Emit(ctx, workflow.Event{RunID: req.RunID, NodeID: req.Spec.ID, Type: "node_progress", Message: sanitize.Text(line), Fields: map[string]any{"source": sanitize.Text(source)}})
		},
	})
	trial := sanitize.TrialResult(trialToDomain(evaluation.TrialResult))
	command := sanitize.CommandRun(commandToDomain(evaluation.CommandRun))
	if len(command.Argv) > 0 {
		command.Command = strings.Join(command.Argv, " ")
	} else if strings.TrimSpace(command.Command) != "" {
		command.Command = strings.Join(commandlog.RedactArgv(strings.Fields(command.Command)), " ")
	}
	refs, storeErr := storeEvaluation(ctx, req, trial, command)
	result := workflow.NodeResult{Artifacts: refs}
	if storeErr != nil {
		return result, storeErr
	}
	if runErr != nil {
		return result, evaluationFailure(ctx, command, slot, runErr)
	}
	if err := validateEvaluationResult(trial, model); err != nil {
		return result, workflow.NewNodeError(workflow.FailurePermanent, false, "validate Harbor "+slot+" result", err)
	}
	return result, nil
}

func importHarborResult(ctx context.Context, req workflow.NodeRequest, slot, resultPath string) (workflow.NodeResult, error) {
	trial, err := harborrun.ParseFile(resultPath)
	if err != nil {
		return workflow.NodeResult{}, workflow.NewNodeError(workflow.FailurePermanent, false, "parse provided Harbor "+slot+" result", err)
	}
	model := pluginutil.String(req, "model")
	failures := harborrun.ValidateForCodeEdgeWithOptions(trial, harborrun.ValidationOptions{
		Qwen: slot == "qwen", ExpectedModel: model, ExpectedAgent: pluginutil.String(req, "agent"),
		TaskDir: pluginutil.String(req, "task_dir"), RequireRuns: true, RequireTaskDigest: true, RequireCommandRun: true,
	})
	if len(failures) > 0 {
		return workflow.NodeResult{}, workflow.NewNodeError(workflow.FailurePermanent, false, "validate provided Harbor "+slot+" result", fmt.Errorf("%s", strings.Join(failures, "; ")))
	}
	commandPath := strings.TrimSpace(trial.CommandRunPath)
	if !filepath.IsAbs(commandPath) {
		commandPath = filepath.Join(filepath.Dir(resultPath), commandPath)
	}
	raw, err := os.ReadFile(commandPath)
	if err != nil {
		return workflow.NodeResult{}, workflow.NewNodeError(workflow.FailurePermanent, false, "read provided Harbor command evidence", err)
	}
	var command domain.CommandRun
	if err := json.Unmarshal(raw, &command); err != nil {
		return workflow.NodeResult{}, workflow.NewNodeError(workflow.FailurePermanent, false, "parse provided Harbor command evidence", err)
	}
	refs, err := storeEvaluation(ctx, req, sanitize.TrialResult(trial), sanitize.CommandRun(command))
	if err != nil {
		return workflow.NodeResult{Artifacts: refs}, err
	}
	return workflow.NodeResult{Artifacts: refs}, nil
}

func storeEvaluation(ctx context.Context, req workflow.NodeRequest, trial domain.TrialResult, command domain.CommandRun) ([]workflow.ArtifactRef, error) {
	base := filepath.ToSlash(filepath.Join("phase3", "artifacts", req.Spec.ID))
	primaryFilename := "trial_result.json"
	if req.Spec.ID == "harbor_run_qwen" {
		primaryFilename = "qwen_result.json"
	} else if req.Spec.ID == "harbor_run_opus" {
		primaryFilename = "opus_result.json"
	}
	primaryName := pluginutil.ArtifactName(req, base+"/"+primaryFilename)
	trialName := base + "/trial_result.json"
	commandName := base + "/command_run.json"
	stdoutName := base + "/stdout.txt"
	stderrName := base + "/stderr.txt"
	screenshotName := base + "/pass4_evidence.png"
	primaryPath, err := req.Store.Path(primaryName)
	if err != nil {
		return nil, err
	}
	commandPath, err := req.Store.Path(commandName)
	if err != nil {
		return nil, err
	}
	stdoutRef, err := req.Store.PutText(ctx, stdoutName, "command_stdout", req.Spec.ID, command.Stdout)
	if err != nil {
		return nil, fmt.Errorf("store Harbor stdout: %w", err)
	}
	stderrRef, err := req.Store.PutText(ctx, stderrName, "command_stderr", req.Spec.ID, command.Stderr)
	if err != nil {
		return []workflow.ArtifactRef{stdoutRef}, fmt.Errorf("store Harbor stderr: %w", err)
	}
	command.StdoutPath = stdoutRef.Path
	command.StderrPath = stderrRef.Path
	commandRef, err := req.Store.PutJSON(ctx, commandName, "command_run", req.Spec.ID, command)
	if err != nil {
		return []workflow.ArtifactRef{stdoutRef, stderrRef}, fmt.Errorf("store Harbor command evidence: %w", err)
	}
	trial.ResultPath = primaryPath
	trial.CommandRunPath = commandPath
	screenshotPath, err := req.Store.Path(screenshotName)
	if err != nil {
		return []workflow.ArtifactRef{commandRef, stdoutRef, stderrRef}, err
	}
	trial.Screenshot = filepath.Base(screenshotPath)
	expectedModel := pluginutil.String(req, "model")
	expectedAgent := pluginutil.String(req, "agent")
	strictFailures := harborrun.ValidateForCodeEdgeWithOptions(trial, harborrun.ValidationOptions{
		Qwen: strings.Contains(strings.ToLower(req.Spec.ID), "qwen"), ExpectedModel: expectedModel, ExpectedAgent: expectedAgent,
		TaskDir: pluginutil.String(req, "task_dir"), RequireRuns: true, RequireTaskDigest: true, RequireCommandRun: true,
	})
	verified := command.Passed && !command.Timeout && validateEvaluationResult(trial, expectedModel) == nil && len(strictFailures) == 0
	screenshotPNG, err := evidence.RenderPassAt4PNGWithStatus(req.Spec.ID, trial, evidence.RenderStatus{
		Verified: verified, RawResultSHA256: trial.RawResultSHA256, CommandEvidenceSHA: commandRef.SHA256,
	})
	if err != nil {
		return []workflow.ArtifactRef{commandRef, stdoutRef, stderrRef}, fmt.Errorf("render Harbor pass@4 screenshot: %w", err)
	}
	screenshotRef, err := req.Store.Put(ctx, workflow.PutArtifactRequest{
		Name: screenshotName, Type: "pass4_screenshot", Producer: req.Spec.ID,
		Metadata: map[string]string{"model": trial.Model, "trials": fmt.Sprintf("%d", trial.Trials)},
		Content:  bytes.NewReader(screenshotPNG),
	})
	if err != nil {
		return []workflow.ArtifactRef{commandRef, stdoutRef, stderrRef}, fmt.Errorf("store Harbor pass@4 screenshot: %w", err)
	}
	primaryRef, err := req.Store.PutJSON(ctx, primaryName, "trial_result", req.Spec.ID, trial)
	if err != nil {
		return []workflow.ArtifactRef{screenshotRef, commandRef, stdoutRef, stderrRef}, fmt.Errorf("store Harbor primary result: %w", err)
	}
	trialRef, err := req.Store.PutJSON(ctx, trialName, "trial_result", req.Spec.ID, trial)
	if err != nil {
		return []workflow.ArtifactRef{primaryRef, screenshotRef, commandRef, stdoutRef, stderrRef}, fmt.Errorf("store Harbor normalized result: %w", err)
	}
	return []workflow.ArtifactRef{primaryRef, trialRef, screenshotRef, commandRef, stdoutRef, stderrRef}, nil
}

func validateEvaluationResult(result domain.TrialResult, expectedModel string) error {
	if strings.TrimSpace(result.Model) != strings.TrimSpace(expectedModel) {
		return fmt.Errorf("model mismatch: got %q want %q", result.Model, expectedModel)
	}
	if result.Trials != 4 || len(result.Runs) != 4 {
		return fmt.Errorf("Harbor result must contain exactly 4 trials and 4 run records, got trials=%d runs=%d", result.Trials, len(result.Runs))
	}
	trials := make([]int, 0, len(result.Runs))
	passCount := 0
	for _, run := range result.Runs {
		trials = append(trials, run.Trial)
		if run.Passed {
			passCount++
		}
	}
	sort.Ints(trials)
	for idx, trial := range trials {
		if trial != idx+1 {
			return fmt.Errorf("Harbor runs must contain unique trial numbers 1..4, got %v", trials)
		}
	}
	if result.PassCount != passCount {
		return fmt.Errorf("pass_count mismatch: got %d computed %d", result.PassCount, passCount)
	}
	if math.Abs(result.PassAt4-float64(passCount)/4.0) > 1e-9 {
		return fmt.Errorf("pass_at_4 mismatch: got %v computed %v", result.PassAt4, float64(passCount)/4.0)
	}
	return nil
}

func evaluationFailure(ctx context.Context, command domain.CommandRun, slot string, err error) error {
	clean := fmt.Errorf("%s", sanitize.Text(err.Error()))
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return workflow.NewNodeError(workflow.FailureCanceled, false, "Harbor "+slot+" evaluation", clean)
	}
	if errors.Is(err, context.DeadlineExceeded) || command.Timeout {
		return workflow.NewNodeError(workflow.FailureTimeout, true, "Harbor "+slot+" evaluation", clean)
	}
	return workflow.NewNodeError(workflow.FailureTransient, true, "Harbor "+slot+" evaluation", clean)
}

func trialToDomain(input workflow.EvaluationTrialResult) domain.TrialResult {
	runs := make([]domain.TrialRun, 0, len(input.Runs))
	for _, run := range input.Runs {
		runs = append(runs, domain.TrialRun{Trial: run.Trial, Passed: run.Passed, Turns: run.Turns, DurationSeconds: run.DurationSeconds, Reward: run.Reward, FailureReason: run.FailureReason})
	}
	raw := make([]domain.ResultFileEvidence, 0, len(input.RawTrialResults))
	for _, evidence := range input.RawTrialResults {
		raw = append(raw, domain.ResultFileEvidence{Path: evidence.Path, SHA256: evidence.SHA256})
	}
	return domain.TrialResult{
		SchemaVersion: input.SchemaVersion, Model: input.Model, Agent: input.Agent,
		Trials: input.Trials, PassCount: input.PassCount, PassAt4: input.PassAt4, AverageTurns: input.AverageTurns,
		Runs: runs, ResultPath: input.ResultPath, RawResultPath: input.RawResultPath, RawResultSHA256: input.RawResultSHA256,
		RawTrialResults: raw, TaskDigest: input.TaskDigest, HarborTaskChecksum: input.HarborTaskChecksum,
		TaskPath: input.TaskPath, CommandRunPath: input.CommandRunPath, SchemaPreflightPath: input.SchemaPreflightPath,
		PreflightRunPath: input.PreflightRunPath, PreflightResultPath: input.PreflightResultPath,
		AgentCacheManifest: input.AgentCacheManifest, RetryEvidence: input.RetryEvidence,
		Screenshot: input.Screenshot, CreatedAt: input.CreatedAt,
	}
}

func commandToDomain(input workflow.CommandEvidence) domain.CommandRun {
	return domain.CommandRun{
		Name: input.Name, Command: input.Command, Argv: append([]string(nil), input.Argv...), Dir: input.Dir,
		Env: append([]string(nil), input.Env...), Attempt: input.Attempt, ExitCode: input.ExitCode,
		Stdout: input.Stdout, Stderr: input.Stderr, StdoutPath: input.StdoutPath, StderrPath: input.StderrPath,
		Timeout: input.Timeout, FailureClass: input.FailureClass, StartedAt: input.StartedAt,
		FinishedAt: input.FinishedAt, DurationMS: input.DurationMS, Passed: input.Passed,
	}
}
