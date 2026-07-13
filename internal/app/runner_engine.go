package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/runlock"
	"github.com/purplevoid/harbor-factory/internal/harbor/sanitize"
	"github.com/purplevoid/harbor-factory/internal/runtime/codexruntime"
	commandruntime "github.com/purplevoid/harbor-factory/internal/runtime/command"
	"github.com/purplevoid/harbor-factory/internal/runtime/harborruntime"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

type cancelingEvaluationRuntime struct {
	runner  *Runner
	runtime workflow.EvaluationRuntime
}

func (r cancelingEvaluationRuntime) Evaluate(ctx context.Context, request workflow.EvaluationRequest) (workflow.EvaluationResult, error) {
	stageCtx, cancel := context.WithCancel(ctx)
	unregister := func() {}
	if r.runner != nil && (request.NodeID == nodes.HarborRunQwen || request.NodeID == nodes.HarborRunOpus) {
		unregister = r.runner.registerStageCancel(request.NodeID, cancel)
	}
	defer func() {
		unregister()
		cancel()
	}()
	return r.runtime.Evaluate(stageCtx, request)
}

func (r *Runner) runWithEngine(ctx context.Context) (summary domain.RunSummary, runErr error) {
	workspace := nodes.DefaultWorkspace(r.opts.Workspace)
	r.opts.Workspace = workspace
	started := time.Now().UTC()
	manualRetry := r.retryIntent != nil
	previousRunID := ""
	var preliminaryRetry ManualRetryPlan
	if manualRetry {
		var err error
		preliminaryRetry, err = planNodeRetry(r.opts, r.retryIntent.NodeID, true)
		if err != nil {
			return summary, err
		}
		r.stateMu.Lock()
		r.runID = preliminaryRetry.RunID
		r.stateMu.Unlock()
	} else {
		previousRunID = recoverablePreviousRunID(workspace)
		if previousRunID != "" {
			r.stateMu.Lock()
			r.runID = previousRunID
			r.stateMu.Unlock()
		}
	}
	runID := r.ensureRunID()
	lock, err := runlock.Acquire(workspace, runlock.Metadata{RunID: runID, StartedAt: started})
	if err != nil {
		return summary, err
	}
	defer lock.Close()
	defer close(r.events)

	summary = domain.RunSummary{RunID: runID, Workspace: workspace, StartedAt: started, Status: "running", Passed: true}
	if previousRunID != "" || manualRetry {
		summary.Recovered = true
		summary.PreviousRunID = runID
	}
	if err := r.validateOptions(); err != nil {
		summary.Status, summary.Passed, summary.FinishedAt = "failed", false, time.Now().UTC()
		_ = r.writeState(sanitize.RunSummary(summary))
		return summary, err
	}
	applyWorkspaceEvidenceDefaults(workspace, &r.opts.TestsAnalysis, &r.opts.QwenResult, &r.opts.OpusResult, &r.opts.QwenScreenshot, &r.opts.OpusScreenshot)
	if _, err := SaveRunnerOptions(r.opts); err != nil {
		return summary, err
	}
	definition, err := buildWorkflowDefinition(r.opts)
	if err != nil {
		return summary, err
	}
	registry, err := buildProductionRegistry(r)
	if err != nil {
		return summary, err
	}
	store, err := workflow.NewFileArtifactStore(workspace)
	if err != nil {
		return summary, err
	}
	agent := r.opts.Agent
	if agent == nil {
		runtime := codexruntime.New(nil, r.opts.CodexPath, nil)
		agent = runtime
	}
	engine := workflow.NewEngine(registry, workflow.Runtimes{
		Command:    commandruntime.New(r.opts.VerifyExec),
		Agent:      agent,
		Evaluation: cancelingEvaluationRuntime{runner: r, runtime: harborruntime.New(r.opts.HarborExec, nil)},
	})
	prior, revision := map[string]workflow.NodeRun(nil), 0
	var retryRequest *workflow.ManualRetryRequest
	if manualRetry {
		lockedPlan, planErr := planNodeRetry(r.opts, r.retryIntent.NodeID, false)
		if planErr != nil {
			return summary, planErr
		}
		if lockedPlan.RunID != preliminaryRetry.RunID || lockedPlan.CurrentRevision != preliminaryRetry.CurrentRevision {
			return summary, fmt.Errorf("manual retry state changed before workspace lock was acquired")
		}
		prior, revision = loadWorkflowResume(workspace)
		retryRequest = &workflow.ManualRetryRequest{NodeID: lockedPlan.RequestedNodeID}
	} else if previousRunID != "" {
		prior, revision = loadWorkflowResume(workspace)
	}
	requestInput := map[string]any{"github_token": r.opts.GitHubToken}
	result, engineErr := engine.Run(ctx, workflow.RunRequest{
		RunID: runID, Revision: revision, Workflow: definition, ArtifactRoot: workspace, WorkspaceRoot: workspace,
		Input: requestInput, Store: store, Events: runnerWorkflowEventSink{runner: r}, Prior: prior, Retry: retryRequest,
		Checkpoint: func(_ context.Context, checkpoint workflow.RunResult) error {
			projected := projectRunSummary(store, checkpoint, r.opts, r.snapshot())
			if checkpoint.ActiveNodeID != "" {
				projected.Status = "running"
				projected.Passed = true
				projected.FinishedAt = time.Time{}
			}
			projected.Recovered = previousRunID != "" || manualRetry
			if projected.Recovered {
				projected.PreviousRunID = runID
			}
			return r.writeState(projected)
		},
	})
	if engineErr != nil && onlyCanceledHarborStage(result) && ctx.Err() == nil {
		engineErr = nil
	} else if engineErr != nil {
		for _, node := range result.Nodes {
			if node.NodeID == nodes.HarborRunQwen && node.Status == workflow.NodeFailed {
				engineErr = fmt.Errorf("Qwen Harbor stage: %w", engineErr)
				break
			}
			if node.NodeID == nodes.HarborRunOpus && node.Status == workflow.NodeFailed {
				engineErr = fmt.Errorf("Opus Harbor stage: %w", engineErr)
				break
			}
		}
	}
	summary = projectRunSummary(store, result, r.opts, r.snapshot())
	summary.Recovered = previousRunID != "" || manualRetry
	if summary.Recovered {
		summary.PreviousRunID = runID
	}
	if r.opts.RunHarbor {
		runQwen, runOpus, _ := harborModelSelection(r.opts.HarborModels)
		if runQwen && summary.QwenResult == nil || runOpus && summary.OpusResult == nil {
			summary.Passed = false
			summary.Status = "failed"
		}
	}
	if summary.Recovered {
		summary.ReusedNodes, summary.RerunNodes = recoveryNodeSets(summary.Events)
	}
	if engineErr != nil {
		summary.Status = "failed"
		summary.Passed = false
		if summary.FinishedAt.IsZero() {
			summary.FinishedAt = time.Now().UTC()
		}
	}
	if err := r.writeState(summary); err != nil && engineErr == nil {
		engineErr = err
	}
	return summary, engineErr
}

func onlyCanceledHarborStage(result workflow.RunResult) bool {
	canceled := false
	for _, run := range result.Nodes {
		switch run.Status {
		case workflow.NodeCanceled:
			if run.NodeID != nodes.HarborRunQwen && run.NodeID != nodes.HarborRunOpus {
				return false
			}
			canceled = true
		case workflow.NodeFailed:
			return false
		}
	}
	return canceled
}

type runnerWorkflowEventSink struct{ runner *Runner }

func (s runnerWorkflowEventSink) Emit(_ context.Context, event workflow.Event) error {
	if s.runner == nil {
		return fmt.Errorf("runner event sink is not configured")
	}
	for index := range event.Artifacts {
		event.Artifacts[index].Name = filepath.Base(event.Artifacts[index].Name)
		if event.Path == "" {
			event.Path = event.Artifacts[index].Path
		}
	}
	event = sanitize.RunnerEvent(event)
	s.runner.recordStageEvent(event.NodeID, event.Type)
	s.runner.mu.Lock()
	s.runner.log = append(s.runner.log, event)
	s.runner.mu.Unlock()
	select {
	case s.runner.events <- event:
	default:
	}
	return nil
}

// Decide implements the HumanGate plugin broker. Engine owns the gate node
// and durable decision artifact; Runner only bridges interactive decisions
// from CLI/TUI or an externally written decision file.
func (r *Runner) Decide(ctx context.Context, request domain.GateRequest) (domain.GateDecision, error) {
	if r.opts.AutoApprove {
		for _, item := range request.Checklist {
			if item.Critical && !item.Passed {
				return domain.GateDecision{}, fmt.Errorf("gate %s has a failing critical check: %s", request.GateID, item.Label)
			}
		}
		return domain.GateDecision{RequestID: request.RequestID, GateID: request.GateID, Action: "approve", Approved: true, DecidedAt: time.Now().UTC()}, nil
	}
	phase, ok := reviewGatePhaseForRunner(request.GateID)
	if !ok {
		return domain.GateDecision{}, fmt.Errorf("unknown gate %s", request.GateID)
	}
	decisionPath := nodes.ReviewDecisionPath(r.opts.Workspace, phase, request.GateID)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return domain.GateDecision{}, ctx.Err()
		case decision := <-r.decisions:
			if decision.RequestID != "" && decision.RequestID != request.RequestID {
				continue
			}
			if decision.GateID != "" && decision.GateID != request.GateID {
				continue
			}
			return normalizeGateDecision(request, decision), nil
		case <-ticker.C:
			decision, ok := readMatchingGateDecision(decisionPath, request)
			if ok {
				return decision, nil
			}
		}
	}
}

func normalizeGateDecision(request domain.GateRequest, decision domain.GateDecision) domain.GateDecision {
	decision.RequestID = request.RequestID
	decision.GateID = request.GateID
	if decision.DecidedAt.IsZero() {
		decision.DecidedAt = time.Now().UTC()
	}
	if strings.TrimSpace(decision.Action) == "" {
		if decision.Approved {
			decision.Action = "approve"
		} else {
			decision.Action = "reject"
		}
	}
	return sanitize.GateDecision(decision)
}

func readMatchingGateDecision(path string, request domain.GateRequest) (domain.GateDecision, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.GateDecision{}, false
	}
	var decision domain.GateDecision
	if json.Unmarshal(raw, &decision) != nil {
		return domain.GateDecision{}, false
	}
	if decision.RequestID != request.RequestID || (decision.GateID != "" && decision.GateID != request.GateID) {
		return domain.GateDecision{}, false
	}
	return normalizeGateDecision(request, decision), true
}

func reviewGatePhaseForRunner(gateID string) (string, bool) {
	switch gateID {
	case nodes.TaskReview, nodes.ContentReview:
		return "phase1", true
	case nodes.SolutionReview, nodes.FinalReview:
		return "phase2", true
	case nodes.ResultReview:
		return "phase3", true
	default:
		return "", false
	}
}

func loadWorkflowResume(workspace string) (map[string]workflow.NodeRun, int) {
	raw, err := os.ReadFile(filepath.Join(nodes.DefaultWorkspace(workspace), "run_result.json"))
	if err != nil {
		return nil, 0
	}
	var result workflow.RunResult
	if json.Unmarshal(raw, &result) != nil {
		return nil, 0
	}
	prior := map[string]workflow.NodeRun{}
	for _, run := range result.Nodes {
		if existing, ok := prior[run.NodeID]; !ok || run.Revision >= existing.Revision {
			prior[run.NodeID] = run
		}
	}
	return prior, result.Revision
}

func projectRunSummary(store workflow.ArtifactStore, result workflow.RunResult, opts RunnerOptions, events []domain.RunnerEvent) domain.RunSummary {
	summary := domain.RunSummary{
		RunID: result.RunID, Workspace: result.WorkspaceRoot, StartedAt: result.StartedAt, FinishedAt: result.FinishedAt,
		Status: string(result.Status), Passed: result.Status == workflow.RunSucceeded, Events: append([]domain.RunnerEvent(nil), events...),
	}
	read := func(name string, target any) bool {
		if store != nil {
			_, err := store.ReadJSON(context.Background(), filepath.ToSlash(name), target)
			return err == nil
		}
		return false
	}
	var prepared domain.RepoPrepared
	read("phase0/repo_prepared.json", &prepared)
	if prepared.SchemaVersion != "" || prepared.SourcePath != "" {
		summary.RepoPrepared = &prepared
	}
	var analysis domain.RepoAnalysis
	var proposal domain.TaskProposal
	var files domain.GeneratedTaskFiles
	read("phase1/artifacts/repo_analyze/repo_analysis.json", &analysis)
	read("phase1/artifacts/task_design/task_proposal.json", &proposal)
	read("phase1/artifacts/generate_task_files/task_files.json", &files)
	var publish domain.TaskPublishReceipt
	if read("phase3/artifacts/task_publish/publish_receipt.json", &publish) && publish.SchemaVersion != "" {
		summary.TaskPublish = &publish
	}
	if proposal.SchemaVersion != "" {
		taskDir := effectiveEngineTaskDir(opts)
		if publish.Passed && strings.TrimSpace(publish.DestinationDir) != "" {
			taskDir = publish.DestinationDir
		}
		summary.GenReport = &domain.GenReport{
			SchemaVersion: "harbor.gen_report.v1", TaskDir: taskDir, TestsAnalysisPath: filepath.Join(taskDir, "tests_analysis.md"),
			RepoAnalysisPath: nodes.RepoAnalysisPath(result.WorkspaceRoot), TaskProposalPath: nodes.TaskProposalPath(result.WorkspaceRoot),
			TaskFilesPath: nodes.TaskFilesPath(result.WorkspaceRoot), RepoAnalysis: analysis, TaskProposal: proposal,
			CreatedAt: result.FinishedAt, Passed: true,
		}
	}
	var lintReport domain.LintReport
	read("phase2/artifacts/submission_lint/lint_report.json", &lintReport)
	if lintReport.SchemaVersion == "" {
		read("phase2/artifacts/codeedge_lint/lint_report.json", &lintReport)
	}
	if lintReport.SchemaVersion != "" {
		summary.LintReport = &lintReport
	}
	var verifyReport domain.VerifyReport
	read("phase2/artifacts/verify/verify_report.json", &verifyReport)
	if verifyReport.SchemaVersion != "" {
		summary.VerifyReport = &verifyReport
	}
	var qualityReport domain.QualityReport
	read("phase2/artifacts/quality_check/quality_report.json", &qualityReport)
	if qualityReport.SchemaVersion != "" {
		summary.QualityReport = &qualityReport
	}
	var similarityReport domain.SimilarityReport
	read("phase2/artifacts/similarity_check/similarity_report.json", &similarityReport)
	if similarityReport.SchemaVersion != "" {
		summary.SimilarityReport = &similarityReport
	}
	var qwen domain.TrialResult
	qwenPresent := read("phase3/artifacts/harbor_run_qwen/qwen_result.json", &qwen)
	if !qwenPresent {
		qwenPresent = read("phase3/artifacts/harbor_run_qwen/trial_result.json", &qwen)
	}
	if qwenPresent {
		summary.QwenResult = &qwen
	}
	var opus domain.TrialResult
	opusPresent := read("phase3/artifacts/harbor_run_opus/opus_result.json", &opus)
	if !opusPresent {
		opusPresent = read("phase3/artifacts/harbor_run_opus/trial_result.json", &opus)
	}
	if opusPresent {
		summary.OpusResult = &opus
	}
	var packageReport domain.PackageReport
	read("phase3/artifacts/package/package_report.json", &packageReport)
	if packageReport.SchemaVersion != "" {
		summary.PackageReport = &packageReport
	}
	for _, gateID := range []string{nodes.TaskReview, nodes.ContentReview, nodes.SolutionReview, nodes.FinalReview, nodes.ResultReview} {
		phase, _ := reviewGatePhaseForRunner(gateID)
		var decision domain.GateDecision
		read(filepath.ToSlash(filepath.Join(phase, "artifacts", "reviews", gateID, "decision.json")), &decision)
		if decision.GateID != "" {
			summary.GateDecisions = append(summary.GateDecisions, decision)
		}
	}
	return sanitize.RunSummary(summary)
}

func effectiveEngineTaskDir(opts RunnerOptions) string {
	if opts.Generate {
		return filepath.Join(nodes.DefaultWorkspace(opts.Workspace), "phase2", "task", "generated-task")
	}
	return strings.TrimSpace(opts.TaskDir)
}
