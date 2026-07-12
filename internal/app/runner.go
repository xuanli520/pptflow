package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/purplevoid/harbor-factory/internal/executor"
	"github.com/purplevoid/harbor-factory/internal/harbor/commandlog"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/gen"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/harbor/lint"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/packager"
	"github.com/purplevoid/harbor-factory/internal/harbor/quality"
	"github.com/purplevoid/harbor-factory/internal/harbor/repair"
	"github.com/purplevoid/harbor-factory/internal/harbor/repoprep"
	"github.com/purplevoid/harbor-factory/internal/harbor/runlock"
	"github.com/purplevoid/harbor-factory/internal/harbor/sanitize"
	"github.com/purplevoid/harbor-factory/internal/harbor/similarity"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/harbor/verify"
	"github.com/purplevoid/harbor-factory/internal/runtime/codexruntime"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

type RunnerOptions struct {
	RepoURL               string
	Commit                string
	AllowLocalRepo        bool
	TaskDir               string
	Generate              bool
	TaskOutputDir         string
	Workspace             string
	TestsAnalysis         string
	QwenResult            string
	OpusResult            string
	AutoApprove           bool
	VerifyDocker          bool
	QualityCheck          bool
	QualityAgent          bool
	SimilarityCheck       bool
	SimilarityGitHub      bool
	SimilarityHistoryDirs []string
	SimilarityTB3Dirs     []string
	SimilarityThreshold   float64
	GitHubToken           string
	RunHarbor             bool
	HarborModels          string
	HarborAgent           string
	HarborAgentEnv        []string
	QwenModel             string
	OpusModel             string
	QwenHarborBaseURL     string
	OpusHarborBaseURL     string
	HarborTimeout         int
	HarborSetupTimeout    int
	HarborAgentCacheDir   string
	HarborPreflight       bool
	HarborConcurrency     int
	HarborAttempts        int
	HarborInfraRetries    int
	HarborExec            executor.CommandRunner
	VerifyExec            executor.CommandRunner
	Package               bool
	OutputDir             string
	StrictSubmission      bool
	TaskName              string
	CodeLang              string
	TaskType              string
	Application           string
	AHT                   string
	Description           string
	IsZeroToOne           bool
	QwenScreenshot        string
	OpusScreenshot        string
	Model                 string
	Reasoning             string
	CodexPath             string
	AgentTimeout          int
	RepairGuidance        string
	RepairSource          string
	Agent                 workflow.AgentRuntime
}

type Runner struct {
	opts      RunnerOptions
	events    chan domain.RunnerEvent
	decisions chan domain.GateDecision
	mu        sync.Mutex
	log       []domain.RunnerEvent
	gates     []domain.GateDecision

	stateMu           sync.Mutex
	runID             string
	currentSummary    *domain.RunSummary
	persistenceErrors []string
	stageMu           sync.Mutex
	stageCancels      map[string]context.CancelFunc
}

const runnerOptionsSchemaVersion = "harbor.runner_options.v1"

func DefaultHarborAgentCacheDir() string {
	root, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(root) == "" {
		root = filepath.Join(".", ".cache")
	}
	return filepath.Join(root, "harbor-factory", "agents", "claude-code")
}

var ErrHarborModelStageCanceled = errors.New("Harbor model stage canceled")

func NewRunner(opts RunnerOptions) *Runner {
	opts = HydrateRuntimeOptions(opts)
	return &Runner{opts: opts, events: make(chan domain.RunnerEvent, 64), decisions: make(chan domain.GateDecision, 8), stageCancels: map[string]context.CancelFunc{}}
}

func SaveRunnerOptions(opts RunnerOptions) (domain.RunnerOptionsSnapshot, error) {
	opts = HydrateRuntimeOptions(opts)
	snapshot := sanitize.RunnerOptionsSnapshot(runnerOptionsSnapshot(opts))
	if err := writeRunnerOptionsSnapshot(snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func LoadRunnerOptions(workspace string) (RunnerOptions, domain.RunnerOptionsSnapshot, error) {
	workspace = defaultString(strings.TrimSpace(workspace), filepath.Join(".harbor-factory", "workspace"))
	path := nodes.RunOptionsPath(workspace)
	raw, err := os.ReadFile(path)
	if err != nil {
		return RunnerOptions{}, domain.RunnerOptionsSnapshot{}, err
	}
	var snapshot domain.RunnerOptionsSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return RunnerOptions{}, domain.RunnerOptionsSnapshot{}, fmt.Errorf("parse run options: %w", err)
	}
	if snapshot.SchemaVersion != "" && snapshot.SchemaVersion != runnerOptionsSchemaVersion {
		return RunnerOptions{}, snapshot, fmt.Errorf("unsupported run options schema %q", snapshot.SchemaVersion)
	}
	if strings.TrimSpace(snapshot.Workspace) == "" {
		snapshot.Workspace = workspace
	}
	opts := runnerOptionsFromSnapshot(snapshot)
	opts = MergeRuntimeOptions(opts, RuntimeOptionsFromEnvironment())
	return opts, sanitize.RunnerOptionsSnapshot(snapshot), nil
}

func (r *Runner) Events() <-chan domain.RunnerEvent {
	return r.events
}

func (r *Runner) SubmitGateDecision(decision domain.GateDecision) {
	if decision.DecidedAt.IsZero() {
		decision.DecidedAt = time.Now().UTC()
	}
	r.decisions <- decision
}

func (r *Runner) CancelNode(nodeID string) bool {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID != nodes.HarborRunQwen && nodeID != nodes.HarborRunOpus {
		return false
	}
	r.stageMu.Lock()
	cancel := r.stageCancels[nodeID]
	r.stageMu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

func (r *Runner) registerStageCancel(nodeID string, cancel context.CancelFunc) func() {
	r.stageMu.Lock()
	r.stageCancels[nodeID] = cancel
	r.stageMu.Unlock()
	return func() {
		r.stageMu.Lock()
		delete(r.stageCancels, nodeID)
		r.stageMu.Unlock()
	}
}

func (r *Runner) Run(ctx context.Context) (summary domain.RunSummary, runErr error) {
	start := time.Now().UTC()
	runID := r.ensureRunID()
	workspaceLock, err := runlock.Acquire(defaultString(strings.TrimSpace(r.opts.Workspace), filepath.Join(".harbor-factory", "workspace")), runlock.Metadata{RunID: runID, StartedAt: start})
	if err != nil {
		return summary, err
	}
	defer workspaceLock.Close()
	summary.RunID = runID
	summary.Workspace = r.opts.Workspace
	summary.StartedAt = start
	summary.Passed = true
	summary.Status = "running"
	if previousRunID := recoverablePreviousRunID(r.opts.Workspace); previousRunID != "" && previousRunID != runID {
		summary.PreviousRunID = previousRunID
		summary.Recovered = true
	}
	r.setCurrentSummary(&summary)
	if _, err := SaveRunnerOptions(r.opts); err != nil {
		r.recordPersistenceError("run_options.json: " + err.Error())
	}
	r.emit("run_started", "", "running", "run started", "")
	if summary.Recovered {
		r.emit("run_recovered", "", "running", fmt.Sprintf("recovered run: previous=%s new=%s; valid evidence will be reused and stale nodes rerun", summary.PreviousRunID, summary.RunID), "")
	}
	defer func() {
		if summary.FinishedAt.IsZero() {
			summary.FinishedAt = time.Now().UTC()
		}
		summary.Events = r.snapshot()
		if summary.Recovered {
			summary.ReusedNodes, summary.RerunNodes = recoveryNodeSets(summary.Events)
		}
		summary.GateDecisions = r.gateSnapshot()
		if summary.Status == "" || summary.Status == "running" {
			if summary.Passed && runErr == nil {
				summary.Status = "succeeded"
			} else {
				summary.Status = "failed"
			}
		}
		summary.PersistenceErrors = r.persistenceErrorSnapshot()
		summary = sanitize.RunSummary(summary)
		if err := r.writeState(summary); err != nil {
			r.recordPersistenceError("state.json: " + err.Error())
			summary.PersistenceErrors = r.persistenceErrorSnapshot()
			summary = sanitize.RunSummary(summary)
		}
	}()
	if err := r.validateOptions(); err != nil {
		r.emit("run_failed", "", "failed", err.Error(), "")
		summary.Passed = false
		summary.Status = "failed"
		summary.FinishedAt = time.Now().UTC()
		summary.Events = r.snapshot()
		close(r.events)
		return summary, err
	}

	needPrepare := r.opts.Generate
	var prepared *domain.RepoPrepared
	if needPrepare {
		if r.opts.RepoURL == "" || r.opts.Commit == "" {
			err := fmt.Errorf("repo and commit are required for repository preparation")
			r.emit("node_failed", nodes.RepoPrepare, "failed", err.Error(), "")
			summary.Passed = false
			summary.Status = "failed"
			summary.FinishedAt = time.Now().UTC()
			summary.Events = r.snapshot()
			close(r.events)
			return summary, err
		}
		r.emit("node_started", nodes.RepoPrepare, "running", "preparing repository", "")
		repoPrepared, err := repoprep.Prepare(ctx, repoprep.Options{
			RepoURL:    r.opts.RepoURL,
			Commit:     r.opts.Commit,
			Workspace:  r.opts.Workspace,
			AllowLocal: r.opts.AllowLocalRepo,
		})
		if err != nil {
			r.emit("node_failed", nodes.RepoPrepare, "failed", err.Error(), "")
			summary.Passed = false
			summary.Status = "failed"
			summary.FinishedAt = time.Now().UTC()
			summary.Events = r.snapshot()
			close(r.events)
			return summary, err
		}
		summary.RepoPrepared = &repoPrepared
		prepared = &repoPrepared
		r.emit("node_succeeded", nodes.RepoPrepare, "succeeded", "repository prepared", repoPrepared.SourcePath)
	}

	effectiveTaskDir := r.opts.TaskDir
	effectiveTestsAnalysis := r.opts.TestsAnalysis
	effectiveQwenResult := r.opts.QwenResult
	effectiveOpusResult := r.opts.OpusResult
	effectiveQwenScreenshot := r.opts.QwenScreenshot
	effectiveOpusScreenshot := r.opts.OpusScreenshot
	effectiveRepoURL := r.opts.RepoURL
	effectiveCommit := r.opts.Commit
	effectiveTaskName := r.opts.TaskName
	effectiveCodeLang := r.opts.CodeLang
	effectiveTaskType := r.opts.TaskType
	effectiveApplication := r.opts.Application
	effectiveAHT := r.opts.AHT
	effectiveDescription := r.opts.Description
	effectiveIsZeroToOne := r.opts.IsZeroToOne
	if r.opts.Generate {
		if prepared == nil {
			err := fmt.Errorf("prepared repository is required for generation")
			r.emit("node_failed", nodes.RepoAnalyze, "failed", err.Error(), "")
			summary.Passed = false
			summary.Status = "failed"
			summary.FinishedAt = time.Now().UTC()
			summary.Events = r.snapshot()
			close(r.events)
			return summary, err
		}
		agent := r.opts.Agent
		if agent == nil {
			agent = codexruntime.New(nil, r.opts.CodexPath, nil)
		}
		genReport, err := gen.Run(ctx, gen.Options{
			RepoPrepared:        *prepared,
			Workspace:           r.opts.Workspace,
			TaskOutputDir:       r.opts.TaskOutputDir,
			TaskName:            r.opts.TaskName,
			Model:               r.opts.Model,
			ReasoningEffort:     r.opts.Reasoning,
			AgentTimeoutSeconds: r.opts.AgentTimeout,
			Agent:               agent,
			RuntimeSelfCheck:    true,
			TaskReview: func(analysis domain.RepoAnalysis, proposal domain.TaskProposal, proposalPath string) error {
				artifacts := gateArtifacts(proposalPath)
				if analysisPath := nodes.RepoAnalysisPath(r.opts.Workspace); isReadableFile(analysisPath) {
					artifacts = append(gateArtifacts(analysisPath), artifacts...)
				}
				decision, err := r.reviewGate(ctx, nodes.TaskReview, "Task Direction Gate", nodes.TaskReview, "Confirm the task direction before generating files and spending runtime validation resources.", taskProposalChecklist(proposal), artifacts, "phase1")
				if err != nil {
					return err
				}
				summary.GateDecisions = append(summary.GateDecisions, decision)
				if !decision.Approved {
					return fmt.Errorf("task proposal gate rejected")
				}
				return nil
			},
			Progress: func(nodeID, status, message, path string) {
				eventType := "node_started"
				switch status {
				case "succeeded":
					eventType = "node_succeeded"
				case "failed":
					eventType = "node_failed"
				}
				r.emit(eventType, nodeID, status, message, path)
			},
		})
		summary.GenReport = &genReport
		if err != nil {
			summary.Passed = false
			summary.Status = "failed"
			summary.FinishedAt = time.Now().UTC()
			summary.Events = r.snapshot()
			close(r.events)
			return summary, err
		}
		effectiveTaskDir = genReport.TaskDir
		effectiveTestsAnalysis = genReport.TestsAnalysisPath
		effectiveTaskName = defaultString(effectiveTaskName, genReport.TaskProposal.TaskName)
		effectiveCodeLang = defaultString(effectiveCodeLang, genReport.TaskProposal.CodeLang)
		effectiveTaskType = defaultString(effectiveTaskType, genReport.TaskProposal.TaskType)
		effectiveApplication = defaultString(effectiveApplication, genReport.TaskProposal.Application)
		effectiveRepoURL = defaultString(effectiveRepoURL, genReport.TaskProposal.GitHubLink)
		effectiveCommit = defaultString(effectiveCommit, genReport.TaskProposal.CommitSHA)
		effectiveAHT = defaultString(effectiveAHT, formatAHT(genReport.TaskProposal.EstimatedAHTMinutes))
		effectiveDescription = defaultString(effectiveDescription, genReport.TaskProposal.OneLineDescription)
		if genReport.TaskProposal.IsZeroToOne {
			effectiveIsZeroToOne = true
		}
	}
	if effectiveTaskDir != "" {
		defaults := readTaskDefaults(effectiveTaskDir)
		effectiveTaskName = defaultString(effectiveTaskName, packageTaskName(defaults.TaskName))
		effectiveCodeLang = defaultString(effectiveCodeLang, defaults.CodeLang)
		effectiveTaskType = defaultString(effectiveTaskType, defaults.TaskType)
		effectiveApplication = defaultString(effectiveApplication, defaults.Application)
		effectiveRepoURL = defaultString(effectiveRepoURL, defaults.GitHubURL)
		effectiveCommit = defaultString(effectiveCommit, defaults.CommitID)
		effectiveAHT = defaultString(effectiveAHT, formatAHT(defaults.EstimatedAHTMinutes))
		effectiveDescription = defaultString(effectiveDescription, defaults.Description)
		if defaults.IsZeroToOne {
			effectiveIsZeroToOne = true
		}
	}
	if guidance := strings.TrimSpace(r.opts.RepairGuidance); effectiveTaskDir != "" && guidance != "" {
		source := normalizedRepairSource(r.opts.RepairSource)
		reportPath := nodes.TaskRepairReportPath(r.opts.Workspace, source, 1)
		if _, ok := repair.Reusable(reportPath, effectiveTaskDir, guidance, source); ok {
			r.emit("node_succeeded", nodes.FinalReview, "succeeded", "reused completed external review repair", reportPath)
		} else {
			r.emit("node_started", nodes.FinalReview, "running", "Codex is repairing the task from operator/external review guidance", "")
			if _, err := r.runTaskRepair(ctx, effectiveTaskDir, source, guidance, nil, 1); err != nil {
				r.emit("node_failed", nodes.FinalReview, "failed", err.Error(), reportPath)
				summary.Passed = false
				summary.Status = "failed"
				close(r.events)
				return summary, err
			}
			r.emit("node_succeeded", nodes.FinalReview, "succeeded", "Codex external review repair changed the task; running all checks", reportPath)
		}
	}
	applyWorkspaceEvidenceDefaults(r.opts.Workspace, &effectiveTestsAnalysis, &effectiveQwenResult, &effectiveOpusResult, &effectiveQwenScreenshot, &effectiveOpusScreenshot)

	reviewRevisionCount := 0
	autoRepairLoop := false
	var harborModelErrors []error
reviewChecks:
	if effectiveTaskDir != "" && summary.Passed {
		strictSubmission := r.opts.StrictSubmission || r.opts.Package
		runQwen, runOpus, _ := harborModelSelection(r.opts.HarborModels)
		needQwenHarborRun := (r.opts.Package || r.opts.RunHarbor && runQwen) && strings.TrimSpace(effectiveQwenResult) == ""
		needOpusHarborRun := (r.opts.Package || r.opts.RunHarbor && runOpus) && strings.TrimSpace(effectiveOpusResult) == ""
		needHarborRun := needQwenHarborRun || needOpusHarborRun
		forceVerifyDocker := r.opts.VerifyDocker || r.opts.Package
		preLintStrict := strictSubmission && !needHarborRun
		r.emit("node_started", nodes.CodeEdgeLint, "running", "running CodeEdge lint", "")
		reportPath := nodes.CodeEdgeLintReportPath(r.opts.Workspace)
		report, err := lint.Run(ctx, lint.Options{
			TaskDir:          effectiveTaskDir,
			RepoURL:          effectiveRepoURL,
			Commit:           effectiveCommit,
			QwenResult:       effectiveQwenResult,
			OpusResult:       effectiveOpusResult,
			QwenScreenshot:   effectiveQwenScreenshot,
			OpusScreenshot:   effectiveOpusScreenshot,
			TestsAnalysis:    effectiveTestsAnalysis,
			StrictSubmission: preLintStrict,
			WriteReport:      reportPath,
		})
		summary.LintReport = &report
		if err != nil {
			r.emit("node_failed", nodes.CodeEdgeLint, "failed", err.Error(), reportPath)
			summary.Passed = false
			summary.Status = "failed"
			summary.FinishedAt = time.Now().UTC()
			summary.Events = r.snapshot()
			close(r.events)
			return summary, err
		}
		if !report.Passed {
			r.emit("node_failed", nodes.CodeEdgeLint, "failed", "lint failed", reportPath)
			summary.Passed = false
		} else {
			r.emit("node_succeeded", nodes.CodeEdgeLint, "succeeded", "lint passed", reportPath)
		}
		verifyPath := ""
		if forceVerifyDocker && report.Passed {
			verifyPath = nodes.VerifyReportPath(r.opts.Workspace)
			if verifyReport, ok := loadReusableVerifyReport(effectiveTaskDir, verifyPath); ok {
				summary.VerifyReport = &verifyReport
				r.emitVerifyStepEvents(verifyReport)
				r.emit("node_succeeded", nodes.HarborVerify, "succeeded", "reused existing Docker/oracle verification report", verifyPath)
			} else {
				r.emit("node_started", nodes.HarborVerify, "running", "running Docker and oracle verification", "")
				verifyReport, err := verify.Run(ctx, verify.Options{
					TaskDir:        effectiveTaskDir,
					Workspace:      r.opts.Workspace,
					ImageTag:       runnerVerifyImageTag(runID, nodes.HarborVerify),
					WriteReport:    verifyPath,
					TimeoutSeconds: 600,
					Exec:           r.opts.VerifyExec,
				})
				summary.VerifyReport = &verifyReport
				r.emitVerifyStepEvents(verifyReport)
				if err != nil {
					r.emit("node_failed", nodes.HarborVerify, "failed", err.Error(), verifyPath)
					summary.Passed = false
				} else {
					r.emit("node_succeeded", nodes.HarborVerify, "succeeded", "Docker/oracle verification passed", verifyPath)
				}
			}
		}
		qualityPath := ""
		if r.opts.QualityCheck {
			qualityPath = nodes.QualityReportPath(r.opts.Workspace)
			if qualityReport, ok := loadReusableQualityReport(effectiveTaskDir, qualityPath); ok {
				summary.QualityReport = &qualityReport
				r.emit("node_succeeded", nodes.QualityCheck, "succeeded", "reused existing quality report", qualityPath)
			} else {
				r.emit("node_started", nodes.QualityCheck, "running", "running CodeEdge semantic quality check", "")
				var qualityAgent workflow.AgentRuntime
				if r.opts.QualityAgent {
					qualityAgent = r.opts.Agent
					if qualityAgent == nil {
						qualityAgent = codexruntime.New(nil, r.opts.CodexPath, nil)
					}
				}
				var proposal *domain.TaskProposal
				if summary.GenReport != nil {
					proposal = &summary.GenReport.TaskProposal
				}
				qualityReport, err := quality.Run(ctx, quality.Options{
					TaskDir:             effectiveTaskDir,
					Workspace:           r.opts.Workspace,
					RepoURL:             effectiveRepoURL,
					Commit:              effectiveCommit,
					TestsAnalysisPath:   effectiveTestsAnalysis,
					Proposal:            proposal,
					Agent:               qualityAgent,
					Model:               r.opts.Model,
					ReasoningEffort:     r.opts.Reasoning,
					AgentTimeoutSeconds: r.opts.AgentTimeout,
					WriteReport:         qualityPath,
				})
				summary.QualityReport = &qualityReport
				if err != nil {
					r.emit("node_failed", nodes.QualityCheck, "failed", err.Error(), qualityPath)
					summary.Passed = false
				} else if !qualityReport.OverallPass {
					r.emit("node_failed", nodes.QualityCheck, "failed", "quality check found blocking issues", qualityPath)
					summary.Passed = false
				} else {
					r.emit("node_succeeded", nodes.QualityCheck, "succeeded", "quality check passed", qualityPath)
				}
			}
		}
		similarityPath := ""
		if r.opts.SimilarityCheck || r.opts.Package {
			similarityPath = nodes.SimilarityReportPath(r.opts.Workspace)
			if similarityReport, ok := loadReusableSimilarityReport(effectiveTaskDir, similarityPath); ok {
				summary.SimilarityReport = &similarityReport
				r.emit("node_succeeded", nodes.SimilarityCheck, "succeeded", "reused existing similarity report", similarityPath)
			} else {
				r.emit("node_started", nodes.SimilarityCheck, "running", "running issue/TB3/history similarity check", "")
				similarityReport, err := similarity.Run(ctx, similarity.Options{
					TaskDir:           effectiveTaskDir,
					RepoURL:           effectiveRepoURL,
					TestsAnalysisPath: effectiveTestsAnalysis,
					HistoryDirs:       r.opts.SimilarityHistoryDirs,
					TB3Dirs:           r.opts.SimilarityTB3Dirs,
					EnableGitHub:      r.opts.SimilarityGitHub,
					GitHubToken:       r.opts.GitHubToken,
					Threshold:         r.opts.SimilarityThreshold,
					StrictSources:     r.opts.Package,
					WriteReport:       similarityPath,
				})
				summary.SimilarityReport = &similarityReport
				if err != nil {
					r.emit("node_failed", nodes.SimilarityCheck, "failed", err.Error(), similarityPath)
					summary.Passed = false
				} else if !similarityReport.OverallPass {
					r.emit("node_failed", nodes.SimilarityCheck, "failed", "similarity check found duplicate-risk candidates", similarityPath)
					summary.Passed = false
				} else {
					r.emit("node_succeeded", nodes.SimilarityCheck, "succeeded", "similarity check passed", similarityPath)
				}
			}
		}
		if needHarborRun && summary.Passed {
			if needQwenHarborRun {
				qwenModel := defaultString(r.opts.QwenModel, "qwen3.7-max")
				qwenResult, err := r.runHarborModel(ctx, effectiveTaskDir, nodes.HarborRunQwen, qwenModel)
				if err != nil {
					summary.QwenResult = &qwenResult
					summary.Passed = false
					if !errors.Is(err, ErrHarborModelStageCanceled) {
						harborModelErrors = append(harborModelErrors, fmt.Errorf("Qwen Harbor stage: %w", err))
					}
				} else {
					summary.QwenResult = &qwenResult
					effectiveQwenResult = qwenResult.ResultPath
					effectiveQwenScreenshot = defaultString(effectiveQwenScreenshot, qwenResult.Screenshot)
				}
			}
			if needOpusHarborRun {
				opusModel := defaultString(r.opts.OpusModel, "claude-opus-4-8")
				opusResult, err := r.runHarborModel(ctx, effectiveTaskDir, nodes.HarborRunOpus, opusModel)
				if err != nil {
					summary.OpusResult = &opusResult
					summary.Passed = false
					if !errors.Is(err, ErrHarborModelStageCanceled) {
						harborModelErrors = append(harborModelErrors, fmt.Errorf("Opus Harbor stage: %w", err))
					}
				} else {
					summary.OpusResult = &opusResult
					effectiveOpusResult = opusResult.ResultPath
					effectiveOpusScreenshot = defaultString(effectiveOpusScreenshot, opusResult.Screenshot)
				}
			}
		}
		if (needHarborRun || (strictSubmission && !preLintStrict)) && summary.Passed {
			r.emit("node_started", nodes.SubmissionLint, "running", "running submission evidence lint", "")
			submissionReportPath := nodes.SubmissionLintReportPath(r.opts.Workspace)
			submissionReport, err := lint.Run(ctx, lint.Options{
				TaskDir:          effectiveTaskDir,
				RepoURL:          effectiveRepoURL,
				Commit:           effectiveCommit,
				QwenResult:       effectiveQwenResult,
				OpusResult:       effectiveOpusResult,
				QwenScreenshot:   effectiveQwenScreenshot,
				OpusScreenshot:   effectiveOpusScreenshot,
				TestsAnalysis:    effectiveTestsAnalysis,
				StrictSubmission: strictSubmission,
				WriteReport:      submissionReportPath,
			})
			summary.LintReport = &submissionReport
			report = submissionReport
			reportPath = submissionReportPath
			if err != nil {
				r.emit("node_failed", nodes.SubmissionLint, "failed", err.Error(), submissionReportPath)
				summary.Passed = false
				summary.Status = "failed"
				summary.FinishedAt = time.Now().UTC()
				summary.Events = r.snapshot()
				close(r.events)
				return summary, err
			}
			if !submissionReport.Passed {
				r.emit("node_failed", nodes.SubmissionLint, "failed", "submission lint failed", submissionReportPath)
				summary.Passed = false
			} else {
				r.emit("node_succeeded", nodes.SubmissionLint, "succeeded", "submission lint passed", submissionReportPath)
			}
		}
		if summary.Passed {
			if summary.QwenResult == nil && strings.TrimSpace(effectiveQwenResult) != "" {
				qwenModel := defaultString(r.opts.QwenModel, "qwen3.7-max")
				qwenResult, err := r.loadProvidedHarborResult(nodes.HarborRunQwen, effectiveQwenResult, effectiveTaskDir, qwenModel, true)
				if err != nil {
					r.emit("node_failed", nodes.HarborRunQwen, "failed", err.Error(), effectiveQwenResult)
					summary.Passed = false
				} else {
					summary.QwenResult = &qwenResult
					effectiveQwenScreenshot = defaultString(effectiveQwenScreenshot, qwenResult.Screenshot)
				}
			}
			if summary.OpusResult == nil && strings.TrimSpace(effectiveOpusResult) != "" {
				opusModel := defaultString(r.opts.OpusModel, "claude-opus-4-8")
				opusResult, err := r.loadProvidedHarborResult(nodes.HarborRunOpus, effectiveOpusResult, effectiveTaskDir, opusModel, false)
				if err != nil {
					r.emit("node_failed", nodes.HarborRunOpus, "failed", err.Error(), effectiveOpusResult)
					summary.Passed = false
				} else {
					summary.OpusResult = &opusResult
					effectiveOpusScreenshot = defaultString(effectiveOpusScreenshot, opusResult.Screenshot)
				}
			}
		}
		finalArtifacts := gateArtifacts(
			reportPath,
			filepath.Join(effectiveTaskDir, "instruction.md"),
			filepath.Join(effectiveTaskDir, "task.toml"),
			filepath.Join(effectiveTaskDir, "environment", "Dockerfile"),
			filepath.Join(effectiveTaskDir, "solution", "solve.sh"),
			filepath.Join(effectiveTaskDir, "tests", "test.sh"),
			filepath.Join(effectiveTaskDir, "tests_analysis.md"),
		)
		if verifyPath != "" {
			finalArtifacts = append(finalArtifacts, gateArtifacts(verifyPath)...)
		}
		if summary.QwenResult != nil && summary.QwenResult.ResultPath != "" {
			finalArtifacts = append(finalArtifacts, gateArtifacts(summary.QwenResult.ResultPath)...)
		}
		if summary.OpusResult != nil && summary.OpusResult.ResultPath != "" {
			finalArtifacts = append(finalArtifacts, gateArtifacts(summary.OpusResult.ResultPath)...)
		}
		if strings.TrimSpace(effectiveQwenScreenshot) != "" {
			finalArtifacts = append(finalArtifacts, domain.ArtifactPreview{Name: filepath.Base(effectiveQwenScreenshot), Path: effectiveQwenScreenshot})
		}
		if strings.TrimSpace(effectiveOpusScreenshot) != "" {
			finalArtifacts = append(finalArtifacts, domain.ArtifactPreview{Name: filepath.Base(effectiveOpusScreenshot), Path: effectiveOpusScreenshot})
		}
		if qualityPath != "" {
			finalArtifacts = append(finalArtifacts, gateArtifacts(qualityPath)...)
		}
		if similarityPath != "" {
			finalArtifacts = append(finalArtifacts, gateArtifacts(similarityPath)...)
		}
		finalChecklist := lintChecklist(report)
		if summary.QualityReport != nil {
			finalChecklist = append(finalChecklist, qualityChecklist(*summary.QualityReport)...)
		}
		if summary.SimilarityReport != nil {
			finalChecklist = append(finalChecklist, similarityChecklist(*summary.SimilarityReport)...)
		}
		if summary.QwenResult != nil || summary.OpusResult != nil {
			finalChecklist = append(finalChecklist, resultChecklist(summary.QwenResult, summary.OpusResult, effectiveTaskDir, defaultString(r.opts.QwenModel, "qwen3.7-max"), defaultString(r.opts.OpusModel, "claude-opus-4-8"), defaultString(r.opts.HarborAgent, "claude-code"), effectiveQwenScreenshot, effectiveOpusScreenshot)...)
		}
		if autoRepairLoop && len(blockingGateChecklist(finalChecklist)) > 0 {
			reviewRevisionCount++
			if reviewRevisionCount > 5 {
				err := fmt.Errorf("automatic Final Review repair reached the maximum of 5 rounds")
				r.emit("node_failed", nodes.FinalReview, "failed", err.Error(), "")
				summary.Passed = false
				return summary, err
			}
			decision := domain.GateDecision{RequestID: stableGateRequestID("phase2", nodes.FinalReview), GateID: nodes.FinalReview, Action: "repair_loop", Notes: "automatic Codex repair loop", DecidedAt: time.Now().UTC()}
			if err := r.writeGateDecision("phase2", nodes.FinalReview, decision); err != nil {
				return summary, err
			}
			if err := r.archiveGateRevision("phase2", nodes.FinalReview, decision, reviewRevisionCount); err != nil {
				return summary, err
			}
			r.mu.Lock()
			r.gates = append(r.gates, sanitizeGateDecision(decision))
			r.mu.Unlock()
			if _, err := r.runTaskRepair(ctx, effectiveTaskDir, "final_review", decision.Notes, blockingGateChecklist(finalChecklist), reviewRevisionCount); err != nil {
				r.emit("node_failed", nodes.FinalReview, "failed", err.Error(), nodes.TaskRepairReportPath(r.opts.Workspace, "final_review", reviewRevisionCount))
				summary.Passed = false
				return summary, err
			}
			r.emit("node_progress", nodes.FinalReview, "running", fmt.Sprintf("automatic Codex repair round %d changed the task; rerunning all checks", reviewRevisionCount), "")
			clearReviewEvidence(&summary, &effectiveQwenResult, &effectiveOpusResult)
			goto reviewChecks
		}
		decision, err := r.reviewGate(ctx, nodes.FinalReview, "Final Release Gate", nodes.FinalReview, "Review all mandatory machine checks and model evidence, then approve or request repair.", finalChecklist, finalArtifacts, "phase2")
		if err != nil {
			r.emit("node_failed", nodes.FinalReview, "failed", err.Error(), "")
			summary.Passed = false
			summary.Status = "failed"
			summary.FinishedAt = time.Now().UTC()
			summary.Events = r.snapshot()
			summary.GateDecisions = r.gateSnapshot()
			close(r.events)
			return summary, err
		}
		summary.GateDecisions = append(summary.GateDecisions, decision)
		if decision.Action == "revise" || decision.Action == "repair" || decision.Action == "repair_loop" {
			reviewRevisionCount++
			if reviewRevisionCount > 5 {
				err := fmt.Errorf("final review exceeded the maximum of 5 revise-and-rerun rounds")
				r.emit("node_failed", nodes.FinalReview, "failed", err.Error(), "")
				summary.Passed = false
				return summary, err
			}
			if err := r.archiveGateRevision("phase2", nodes.FinalReview, decision, reviewRevisionCount); err != nil {
				return summary, err
			}
			if decision.Action == "repair" || decision.Action == "repair_loop" {
				if _, err := r.runTaskRepair(ctx, effectiveTaskDir, "final_review", decision.Notes, blockingGateChecklist(finalChecklist), reviewRevisionCount); err != nil {
					r.emit("node_failed", nodes.FinalReview, "failed", err.Error(), nodes.TaskRepairReportPath(r.opts.Workspace, "final_review", reviewRevisionCount))
					summary.Passed = false
					return summary, err
				}
				autoRepairLoop = decision.Action == "repair_loop"
				r.emit("node_progress", nodes.FinalReview, "running", fmt.Sprintf("Codex repair round %d changed the task; rerunning lint, verification, quality, similarity, and model evidence", reviewRevisionCount), "")
			} else {
				autoRepairLoop = false
				r.emit("node_progress", nodes.FinalReview, "running", fmt.Sprintf("manual revision %d accepted; rerunning lint, verification, quality, similarity, and model evidence", reviewRevisionCount), "")
			}
			clearReviewEvidence(&summary, &effectiveQwenResult, &effectiveOpusResult)
			goto reviewChecks
		}
		if !decision.Approved {
			r.emit("node_failed", nodes.FinalReview, "failed", "gate rejected", "")
			summary.Passed = false
		} else {
			r.emit("node_succeeded", nodes.FinalReview, "succeeded", "gate approved", "")
		}
		if r.opts.Package && summary.Passed {
			r.emit("node_started", nodes.Package, "running", "packaging Harbor task", "")
			packageReport, err := packager.Package(packager.Options{
				TaskDir:          effectiveTaskDir,
				OutputDir:        r.opts.OutputDir,
				TaskName:         effectiveTaskName,
				CodeLang:         effectiveCodeLang,
				TaskType:         effectiveTaskType,
				Application:      effectiveApplication,
				AHT:              effectiveAHT,
				Description:      effectiveDescription,
				IsZeroToOne:      effectiveIsZeroToOne,
				GitHubURL:        effectiveRepoURL,
				CommitID:         effectiveCommit,
				TestsAnalysis:    effectiveTestsAnalysis,
				VerifyReport:     verifyPath,
				QualityReport:    qualityPath,
				SimilarityReport: similarityPath,
				QwenResult:       effectiveQwenResult,
				OpusResult:       effectiveOpusResult,
				QwenScreenshot:   effectiveQwenScreenshot,
				OpusScreenshot:   effectiveOpusScreenshot,
			})
			summary.PackageReport = &packageReport
			if err != nil {
				r.emit("node_failed", nodes.Package, "failed", err.Error(), "")
				summary.Passed = false
			} else {
				r.emit("node_succeeded", nodes.Package, "succeeded", "package created", packageReport.OutputZip)
			}
		}
	}

	summary.FinishedAt = time.Now().UTC()
	summary.GateDecisions = r.gateSnapshot()
	if summary.Passed {
		summary.Status = "succeeded"
		r.emit("run_succeeded", "", "succeeded", "run succeeded", "")
	} else {
		summary.Status = "failed"
		r.emit("run_failed", "", "failed", "run failed", "")
	}
	summary.Events = r.snapshot()
	close(r.events)
	runErr = errors.Join(harborModelErrors...)
	return summary, runErr
}

func recoverablePreviousRunID(workspace string) string {
	workspace = defaultString(strings.TrimSpace(workspace), filepath.Join(".harbor-factory", "workspace"))
	raw, err := os.ReadFile(filepath.Join(workspace, "state.json"))
	if err != nil {
		return ""
	}
	var previous domain.RunSummary
	if json.Unmarshal(raw, &previous) != nil || strings.TrimSpace(previous.RunID) == "" {
		return ""
	}
	if previous.Status == "succeeded" || previous.Status == "failed" || !previous.FinishedAt.IsZero() {
		return ""
	}
	return strings.TrimSpace(previous.RunID)
}

func recoveryNodeSets(events []domain.RunnerEvent) ([]string, []string) {
	reused := map[string]bool{}
	rerun := map[string]bool{}
	for _, event := range events {
		if event.NodeID == "" {
			continue
		}
		if event.Type == "node_started" {
			rerun[event.NodeID] = true
		}
		if event.Type == "node_succeeded" && strings.Contains(strings.ToLower(event.Message), "reused existing") {
			reused[event.NodeID] = true
			delete(rerun, event.NodeID)
		}
	}
	return sortedSet(reused), sortedSet(rerun)
}

func sortedSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (r *Runner) loadProvidedHarborResult(nodeID, path, taskDir, expectedModel string, qwen bool) (domain.TrialResult, error) {
	result, err := harborrun.ParseFile(path)
	if err != nil {
		return domain.TrialResult{}, fmt.Errorf("parse provided Harbor result: %w", err)
	}
	if result.ResultPath == "" {
		result.ResultPath = path
	}
	failures := harborrun.ValidateForCodeEdgeWithOptions(result, harborResultValidationOptions(taskDir, expectedModel, defaultString(r.opts.HarborAgent, "claude-code"), qwen))
	if len(failures) > 0 {
		return domain.TrialResult{}, fmt.Errorf("provided Harbor result failed strict validation: %s", strings.Join(failures, "; "))
	}
	r.emit("node_succeeded", nodeID, "succeeded", fmt.Sprintf("loaded Harbor result evidence: trials=%d pass_count=%d average_turns=%.2f", result.Trials, result.PassCount, result.AverageTurns), path)
	return result, nil
}

func (r *Runner) runHarborModel(ctx context.Context, taskDir, nodeID, model string) (domain.TrialResult, error) {
	modelCtx, cancel := context.WithCancel(ctx)
	unregister := r.registerStageCancel(nodeID, cancel)
	defer func() {
		unregister()
		cancel()
	}()
	r.emit("node_started", nodeID, "running", "running Harbor pass@4 for "+model, "")
	outputDir := nodes.HarborRunDir(r.opts.Workspace, nodeID)
	livePath := filepath.Join(outputDir, "live.log")
	agentEnv := append([]string(nil), r.opts.HarborAgentEnv...)
	// Claude Code may choose tier defaults for built-in subagents even when the
	// parent was launched with an explicit model. Keep every nested request on
	// the current stage route instead of leaking Opus requests into Qwen-only
	// gateways (or vice versa).
	for _, key := range []string{
		"CLAUDE_CODE_SUBAGENT_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_FABLE_MODEL",
	} {
		agentEnv = upsertEnv(agentEnv, key, model)
	}
	switch nodeID {
	case nodes.HarborRunQwen:
		agentEnv = upsertEnv(agentEnv, "ANTHROPIC_BASE_URL", r.opts.QwenHarborBaseURL)
	case nodes.HarborRunOpus:
		agentEnv = upsertEnv(agentEnv, "ANTHROPIC_BASE_URL", r.opts.OpusHarborBaseURL)
	}
	lastProgress := map[string]time.Time{}
	var progressMu sync.Mutex
	trialResult, commandRun, err := harborrun.Run(modelCtx, harborrun.Options{
		TaskPath:            taskDir,
		Model:               model,
		Agent:               r.opts.HarborAgent,
		AgentEnv:            agentEnv,
		OutputDir:           outputDir,
		TimeoutSeconds:      r.opts.HarborTimeout,
		SetupTimeoutSeconds: r.opts.HarborSetupTimeout,
		AgentCacheDir:       r.opts.HarborAgentCacheDir,
		Preflight:           r.opts.HarborPreflight,
		Concurrency:         r.opts.HarborConcurrency,
		Attempts:            r.opts.HarborAttempts,
		InfraRetries:        r.opts.HarborInfraRetries,
		Progress: func(line, source string) {
			progressMu.Lock()
			defer progressMu.Unlock()
			now := time.Now()
			if previous := lastProgress[source]; !previous.IsZero() && now.Sub(previous) < time.Second {
				return
			}
			lastProgress[source] = now
			message := strings.TrimSpace(line)
			if len(message) > 320 {
				message = message[:320] + "..."
			}
			if message != "" {
				r.emit("node_progress", nodeID, "running", source+": "+message, livePath)
			}
		},
		Exec: r.opts.HarborExec,
	})
	if trialResult.ResultPath != "" {
		aliasPath, aliasErr := writeHarborResultAlias(outputDir, nodeID, &trialResult)
		if aliasErr != nil {
			return trialResult, aliasErr
		}
		if aliasPath != "" {
			trialResult.ResultPath = aliasPath
		}
	}
	if err != nil {
		path := firstNonEmpty(trialResult.ResultPath, trialResult.PreflightResultPath, trialResult.CommandRunPath, trialResult.PreflightRunPath, trialResult.SchemaPreflightPath)
		r.emit("node_failed", nodeID, "failed", fmt.Sprintf("Harbor failed with partial result: trials=%d pass_count=%d: %v", trialResult.Trials, trialResult.PassCount, err), path)
		if errors.Is(modelCtx.Err(), context.Canceled) && ctx.Err() == nil {
			r.emit("node_canceled", nodeID, "canceled", "Harbor model stage canceled; continuing remaining workflow stages", path)
			return trialResult, fmt.Errorf("%w: %s", ErrHarborModelStageCanceled, model)
		}
		return trialResult, err
	}
	path := trialResult.ResultPath
	if path == "" {
		path = nodes.TrialResultPath(r.opts.Workspace, nodeID)
	}
	r.emit("node_succeeded", nodeID, "succeeded", fmt.Sprintf("Harbor result: trials=%d pass_count=%d average_turns=%.2f", trialResult.Trials, trialResult.PassCount, trialResult.AverageTurns), path)
	if !commandRun.Passed {
		return trialResult, fmt.Errorf("harbor command did not pass")
	}
	return trialResult, nil
}

func writeHarborResultAlias(outputDir, nodeID string, result *domain.TrialResult) (string, error) {
	var filename string
	switch nodeID {
	case nodes.HarborRunQwen:
		filename = filepath.Base(nodes.QwenResultPath(""))
	case nodes.HarborRunOpus:
		filename = filepath.Base(nodes.OpusResultPath(""))
	default:
		return "", nil
	}
	path := filepath.Join(outputDir, filename)
	copy := *result
	copy.ResultPath = path
	data, err := json.MarshalIndent(copy, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func (r *Runner) ensureRunID() string {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.runID == "" {
		r.runID = fmt.Sprintf("run-%d", time.Now().UTC().UnixNano())
	}
	return r.runID
}

func runnerVerifyImageTag(runID, nodeID string) string {
	value := strings.ToLower(strings.TrimSpace("harbor-task-" + runID + "-" + nodeID))
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
		return "harbor-task"
	}
	if len(out) > 128 {
		out = out[:128]
		out = strings.TrimRight(out, ".-")
	}
	if out == "" {
		return "harbor-task"
	}
	return out
}

func (r *Runner) setCurrentSummary(summary *domain.RunSummary) {
	r.stateMu.Lock()
	r.currentSummary = summary
	r.stateMu.Unlock()
}

func (r *Runner) checkpointCurrent(status string) {
	r.stateMu.Lock()
	current := r.currentSummary
	r.stateMu.Unlock()
	if current == nil {
		return
	}
	summary := *current
	if status != "" && summary.Status == "running" {
		summary.Status = status
	}
	summary.RunID = r.ensureRunID()
	summary.Events = r.snapshot()
	summary.GateDecisions = r.gateSnapshot()
	summary.PersistenceErrors = r.persistenceErrorSnapshot()
	if err := r.writeState(summary); err != nil {
		r.recordPersistenceError("state.json: " + err.Error())
	}
}

func (r *Runner) recordPersistenceError(message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	for _, existing := range r.persistenceErrors {
		if existing == message {
			return
		}
	}
	r.persistenceErrors = append(r.persistenceErrors, message)
}

func (r *Runner) persistenceErrorSnapshot() []string {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return append([]string(nil), r.persistenceErrors...)
}

func (r *Runner) validateOptions() error {
	if !r.opts.Generate && strings.TrimSpace(r.opts.TaskDir) == "" {
		return fmt.Errorf("run requires --task or --generate")
	}
	if r.opts.Generate && (strings.TrimSpace(r.opts.RepoURL) == "" || strings.TrimSpace(r.opts.Commit) == "") {
		return fmt.Errorf("--generate requires --repo and --commit")
	}
	if r.opts.Package && strings.TrimSpace(r.opts.OutputDir) == "" {
		return fmt.Errorf("--package requires a non-empty --output")
	}
	if r.opts.Package && strings.TrimSpace(r.opts.TaskName) != "" {
		taskName, err := packager.NormalizeTaskName(r.opts.TaskName)
		if err != nil {
			return err
		}
		r.opts.TaskName = taskName
	}
	if r.opts.HarborSetupTimeout <= 0 {
		r.opts.HarborSetupTimeout = 1200
	}
	if r.opts.HarborConcurrency <= 0 {
		r.opts.HarborConcurrency = 1
	}
	if r.opts.HarborAttempts <= 0 {
		r.opts.HarborAttempts = 4
	}
	if r.opts.HarborInfraRetries < 0 {
		return fmt.Errorf("--harbor-infra-retries must be non-negative")
	}
	if _, _, err := harborModelSelection(r.opts.HarborModels); err != nil {
		return err
	}
	if err := validateHarborBaseURL("--qwen-harbor-base-url", r.opts.QwenHarborBaseURL); err != nil {
		return err
	}
	if err := validateHarborBaseURL("--opus-harbor-base-url", r.opts.OpusHarborBaseURL); err != nil {
		return err
	}
	if r.opts.RunHarbor && strings.EqualFold(defaultString(r.opts.HarborAgent, "claude-code"), "claude-code") && !hasClaudeCredential(r.opts.HarborAgentEnv) {
		return fmt.Errorf("--run-harbor with claude-code requires a non-empty ANTHROPIC_AUTH_TOKEN, ANTHROPIC_API_KEY, or CLAUDE_CODE_OAUTH_TOKEN referenced via --harbor-agent-env; ${VAR} templates must resolve in the Factory process environment because host Claude OAuth is not inherited by Harbor trial containers")
	}
	return nil
}

func harborModelSelection(value string) (bool, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return true, true, nil
	}
	var qwen, opus bool
	for _, model := range strings.Split(value, ",") {
		switch strings.ToLower(strings.TrimSpace(model)) {
		case "qwen":
			qwen = true
		case "opus":
			opus = true
		case "":
			return false, false, fmt.Errorf("--harbor-models contains an empty model name")
		default:
			return false, false, fmt.Errorf("--harbor-models accepts only qwen and opus")
		}
	}
	if !qwen && !opus {
		return false, false, fmt.Errorf("--harbor-models must select qwen, opus, or both")
	}
	return qwen, opus, nil
}

func validateHarborBaseURL(label, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return fmt.Errorf("%s must be an absolute HTTP(S) URL", label)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s must not contain credentials, query parameters, or a fragment", label)
	}
	return nil
}

func upsertEnv(values []string, key, value string) []string {
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	out := make([]string, 0, len(values)+1)
	for _, item := range values {
		itemKey, _, ok := strings.Cut(item, "=")
		if ok && strings.EqualFold(strings.TrimSpace(itemKey), key) {
			continue
		}
		if strings.TrimSpace(item) != "" {
			out = append(out, item)
		}
	}
	if key != "" && value != "" {
		out = append(out, key+"="+value)
	}
	return out
}

func hasClaudeCredential(agentEnv []string) bool {
	allowed := map[string]bool{
		"ANTHROPIC_AUTH_TOKEN":    true,
		"ANTHROPIC_API_KEY":       true,
		"CLAUDE_CODE_OAUTH_TOKEN": true,
	}
	for _, item := range agentEnv {
		key, value, ok := strings.Cut(item, "=")
		if !ok || !allowed[strings.ToUpper(strings.TrimSpace(key))] {
			continue
		}
		value = strings.TrimSpace(value)
		if envName, templated := envTemplateName(value); templated {
			if resolved, exists := os.LookupEnv(envName); exists && strings.TrimSpace(resolved) != "" {
				return true
			}
			continue
		}
		if value != "" {
			return true
		}
	}
	return false
}

func envTemplateName(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) < 4 || !strings.HasPrefix(value, "${") || !strings.HasSuffix(value, "}") {
		return "", false
	}
	name := strings.TrimSpace(value[2 : len(value)-1])
	return name, safeEnvKey(name)
}

func (r *Runner) emit(eventType, nodeID, status, message, path string) {
	event := domain.RunnerEvent{
		Type:      eventType,
		NodeID:    nodeID,
		Status:    status,
		Message:   message,
		Path:      path,
		CreatedAt: time.Now().UTC(),
	}
	if strings.TrimSpace(path) != "" {
		event.Artifacts = []domain.ArtifactPreview{{Name: filepath.Base(path), Path: path}}
	}
	r.emitEvent(event)
}

func (r *Runner) emitEvent(event domain.RunnerEvent) {
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	if event.RunID == "" {
		event.RunID = r.ensureRunID()
	}
	event = sanitizeRunnerEvent(event)
	r.mu.Lock()
	r.log = append(r.log, event)
	r.mu.Unlock()
	if err := r.appendEventLog(event); err != nil {
		r.recordPersistenceError("event_log.jsonl: " + err.Error())
	}
	r.checkpointCurrent("running")
	select {
	case r.events <- event:
	default:
	}
}

func (r *Runner) appendEventLog(event domain.RunnerEvent) error {
	workspace := defaultString(r.opts.Workspace, filepath.Join(".harbor-factory", "workspace"))
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(workspace, "event_log.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	_ = file.Chmod(0o600)
	return nil
}

func (r *Runner) writeState(summary domain.RunSummary) error {
	workspace := defaultString(r.opts.Workspace, filepath.Join(".harbor-factory", "workspace"))
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return err
	}
	if summary.RunID == "" {
		summary.RunID = r.ensureRunID()
	}
	summary = sanitize.RunSummary(summary)
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	statePath := filepath.Join(workspace, "state.json")
	tmpPath := statePath + ".tmp"
	if err := os.WriteFile(tmpPath, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, statePath)
}

func applyWorkspaceEvidenceDefaults(workspace string, testsAnalysis, qwenResult, opusResult, qwenScreenshot, opusScreenshot *string) {
	workspace = defaultString(strings.TrimSpace(workspace), filepath.Join(".harbor-factory", "workspace"))
	defaultReadableFile(testsAnalysis, nodes.TestsAnalysisPath(workspace))
	defaultReadableFile(qwenResult, nodes.QwenResultPath(workspace))
	defaultReadableFile(opusResult, nodes.OpusResultPath(workspace))
	defaultResultScreenshot(qwenScreenshot, *qwenResult)
	defaultResultScreenshot(opusScreenshot, *opusResult)
}

func defaultReadableFile(target *string, candidate string) {
	if target == nil || strings.TrimSpace(*target) != "" || !regularReadableFile(candidate) {
		return
	}
	*target = candidate
}

func defaultResultScreenshot(target *string, resultPath string) {
	if target == nil || strings.TrimSpace(*target) != "" {
		return
	}
	screenshot, ok := trialResultScreenshotPath(resultPath)
	if !ok || !regularReadableFile(screenshot) {
		return
	}
	*target = screenshot
}

func trialResultScreenshotPath(resultPath string) (string, bool) {
	resultPath = strings.TrimSpace(resultPath)
	if resultPath == "" {
		return "", false
	}
	result, err := harborrun.ParseFile(resultPath)
	if err != nil {
		return "", false
	}
	screenshot := strings.TrimSpace(result.Screenshot)
	if screenshot == "" {
		return "", false
	}
	if !filepath.IsAbs(screenshot) {
		screenshot = filepath.Join(filepath.Dir(resultPath), screenshot)
	}
	return screenshot, true
}

func regularReadableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func loadReusableVerifyReport(taskDir, path string) (domain.VerifyReport, bool) {
	if !regularReadableFile(path) {
		return domain.VerifyReport{}, false
	}
	if err := packager.ValidateVerifyReport(path, taskDir); err != nil {
		return domain.VerifyReport{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.VerifyReport{}, false
	}
	var report domain.VerifyReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return domain.VerifyReport{}, false
	}
	return sanitize.VerifyReport(report), true
}

func loadReusableSimilarityReport(taskDir, path string) (domain.SimilarityReport, bool) {
	if !regularReadableFile(path) {
		return domain.SimilarityReport{}, false
	}
	report, err := packager.ValidateSimilarityReport(path, taskDir)
	if err != nil {
		return domain.SimilarityReport{}, false
	}
	return sanitize.SimilarityReport(report), true
}

func loadReusableQualityReport(taskDir, path string) (domain.QualityReport, bool) {
	if !regularReadableFile(path) {
		return domain.QualityReport{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.QualityReport{}, false
	}
	var report domain.QualityReport
	if err := json.Unmarshal(raw, &report); err != nil || report.SchemaVersion != "harbor.quality_report.v1" {
		return domain.QualityReport{}, false
	}
	want, err := filepath.Abs(taskDir)
	if err != nil {
		return domain.QualityReport{}, false
	}
	got, err := filepath.Abs(report.TaskDir)
	if err != nil || filepath.Clean(got) != filepath.Clean(want) {
		return domain.QualityReport{}, false
	}
	if strings.TrimSpace(report.TaskDigest) == "" {
		return domain.QualityReport{}, false
	}
	digest, err := harborrun.ComputeTaskDigest(taskDir)
	if err != nil || !strings.EqualFold(report.TaskDigest, digest) {
		return domain.QualityReport{}, false
	}
	return sanitize.QualityReport(report), true
}

func writeRunnerOptionsSnapshot(snapshot domain.RunnerOptionsSnapshot) error {
	workspace := defaultString(strings.TrimSpace(snapshot.Workspace), filepath.Join(".harbor-factory", "workspace"))
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return err
	}
	if strings.TrimSpace(snapshot.SchemaVersion) == "" {
		snapshot.SchemaVersion = runnerOptionsSchemaVersion
	}
	if strings.TrimSpace(snapshot.Workspace) == "" {
		snapshot.Workspace = workspace
	}
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = time.Now().UTC()
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	path := nodes.RunOptionsPath(workspace)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func runnerOptionsSnapshot(opts RunnerOptions) domain.RunnerOptionsSnapshot {
	workspace := defaultString(strings.TrimSpace(opts.Workspace), filepath.Join(".harbor-factory", "workspace"))
	snapshot := domain.RunnerOptionsSnapshot{
		SchemaVersion:            runnerOptionsSchemaVersion,
		Workspace:                workspace,
		RepoURL:                  strings.TrimSpace(opts.RepoURL),
		Commit:                   strings.TrimSpace(opts.Commit),
		AllowLocalRepo:           opts.AllowLocalRepo,
		TaskDir:                  strings.TrimSpace(opts.TaskDir),
		Generate:                 opts.Generate,
		TaskOutputDir:            strings.TrimSpace(opts.TaskOutputDir),
		TestsAnalysis:            strings.TrimSpace(opts.TestsAnalysis),
		QwenResult:               strings.TrimSpace(opts.QwenResult),
		OpusResult:               strings.TrimSpace(opts.OpusResult),
		AutoApprove:              opts.AutoApprove,
		VerifyDocker:             opts.VerifyDocker,
		QualityCheck:             opts.QualityCheck,
		QualityAgent:             opts.QualityAgent,
		SimilarityCheck:          opts.SimilarityCheck,
		SimilarityGitHub:         opts.SimilarityGitHub,
		SimilarityHistoryDirs:    compactStrings(opts.SimilarityHistoryDirs),
		SimilarityTB3Dirs:        compactStrings(opts.SimilarityTB3Dirs),
		SimilarityThreshold:      opts.SimilarityThreshold,
		GitHubTokenConfigured:    strings.TrimSpace(opts.GitHubToken) != "",
		RunHarbor:                opts.RunHarbor,
		HarborModels:             strings.TrimSpace(opts.HarborModels),
		HarborAgent:              strings.TrimSpace(opts.HarborAgent),
		HarborAgentEnvKeys:       harborAgentEnvKeys(opts.HarborAgentEnv),
		HarborAgentEnvOmitted:    len(opts.HarborAgentEnv) > 0,
		QwenModel:                strings.TrimSpace(opts.QwenModel),
		OpusModel:                strings.TrimSpace(opts.OpusModel),
		QwenHarborBaseURL:        strings.TrimSpace(opts.QwenHarborBaseURL),
		OpusHarborBaseURL:        strings.TrimSpace(opts.OpusHarborBaseURL),
		HarborTimeout:            opts.HarborTimeout,
		HarborSetupTimeout:       opts.HarborSetupTimeout,
		HarborAgentCacheDir:      strings.TrimSpace(opts.HarborAgentCacheDir),
		HarborPreflight:          &opts.HarborPreflight,
		HarborConcurrency:        opts.HarborConcurrency,
		HarborAttempts:           opts.HarborAttempts,
		HarborInfraRetries:       opts.HarborInfraRetries,
		Package:                  opts.Package,
		OutputDir:                strings.TrimSpace(opts.OutputDir),
		StrictSubmission:         opts.StrictSubmission,
		TaskName:                 strings.TrimSpace(opts.TaskName),
		CodeLang:                 strings.TrimSpace(opts.CodeLang),
		TaskType:                 strings.TrimSpace(opts.TaskType),
		Application:              strings.TrimSpace(opts.Application),
		AHT:                      strings.TrimSpace(opts.AHT),
		Description:              strings.TrimSpace(opts.Description),
		IsZeroToOne:              opts.IsZeroToOne,
		QwenScreenshot:           strings.TrimSpace(opts.QwenScreenshot),
		OpusScreenshot:           strings.TrimSpace(opts.OpusScreenshot),
		Model:                    strings.TrimSpace(opts.Model),
		Reasoning:                strings.TrimSpace(opts.Reasoning),
		CodexPath:                strings.TrimSpace(opts.CodexPath),
		AgentTimeout:             opts.AgentTimeout,
		RepairGuidance:           strings.TrimSpace(opts.RepairGuidance),
		RepairSource:             strings.TrimSpace(opts.RepairSource),
		SensitiveFieldsOmitted:   sensitiveRunnerOptionFields(opts),
		UnsupportedFieldsOmitted: unsupportedRunnerOptionFields(opts),
		CreatedAt:                time.Now().UTC(),
	}
	return snapshot
}

func runnerOptionsFromSnapshot(snapshot domain.RunnerOptionsSnapshot) RunnerOptions {
	preflight := true
	if snapshot.HarborPreflight != nil {
		preflight = *snapshot.HarborPreflight
	}
	return RunnerOptions{
		RepoURL:               snapshot.RepoURL,
		Commit:                snapshot.Commit,
		AllowLocalRepo:        snapshot.AllowLocalRepo,
		TaskDir:               snapshot.TaskDir,
		Generate:              snapshot.Generate,
		TaskOutputDir:         snapshot.TaskOutputDir,
		Workspace:             defaultString(strings.TrimSpace(snapshot.Workspace), filepath.Join(".harbor-factory", "workspace")),
		TestsAnalysis:         snapshot.TestsAnalysis,
		QwenResult:            snapshot.QwenResult,
		OpusResult:            snapshot.OpusResult,
		AutoApprove:           snapshot.AutoApprove,
		VerifyDocker:          snapshot.VerifyDocker,
		QualityCheck:          snapshot.QualityCheck,
		QualityAgent:          snapshot.QualityAgent,
		SimilarityCheck:       snapshot.SimilarityCheck,
		SimilarityGitHub:      snapshot.SimilarityGitHub,
		SimilarityHistoryDirs: append([]string(nil), snapshot.SimilarityHistoryDirs...),
		SimilarityTB3Dirs:     append([]string(nil), snapshot.SimilarityTB3Dirs...),
		SimilarityThreshold:   snapshot.SimilarityThreshold,
		RunHarbor:             snapshot.RunHarbor,
		HarborModels:          snapshot.HarborModels,
		HarborAgent:           snapshot.HarborAgent,
		HarborAgentEnv:        harborAgentEnvTemplates(snapshot.HarborAgentEnvKeys),
		QwenModel:             snapshot.QwenModel,
		OpusModel:             snapshot.OpusModel,
		QwenHarborBaseURL:     snapshot.QwenHarborBaseURL,
		OpusHarborBaseURL:     snapshot.OpusHarborBaseURL,
		HarborTimeout:         snapshot.HarborTimeout,
		HarborSetupTimeout:    defaultInt(snapshot.HarborSetupTimeout, 1200),
		HarborAgentCacheDir:   snapshot.HarborAgentCacheDir,
		HarborPreflight:       preflight,
		HarborConcurrency:     defaultInt(snapshot.HarborConcurrency, 1),
		HarborAttempts:        defaultInt(snapshot.HarborAttempts, 4),
		HarborInfraRetries:    snapshot.HarborInfraRetries,
		Package:               snapshot.Package,
		OutputDir:             snapshot.OutputDir,
		StrictSubmission:      snapshot.StrictSubmission,
		TaskName:              snapshot.TaskName,
		CodeLang:              snapshot.CodeLang,
		TaskType:              snapshot.TaskType,
		Application:           snapshot.Application,
		AHT:                   snapshot.AHT,
		Description:           snapshot.Description,
		IsZeroToOne:           snapshot.IsZeroToOne,
		QwenScreenshot:        snapshot.QwenScreenshot,
		OpusScreenshot:        snapshot.OpusScreenshot,
		Model:                 snapshot.Model,
		Reasoning:             snapshot.Reasoning,
		CodexPath:             snapshot.CodexPath,
		AgentTimeout:          snapshot.AgentTimeout,
		RepairGuidance:        snapshot.RepairGuidance,
		RepairSource:          snapshot.RepairSource,
	}
}

func harborAgentEnvTemplates(keys []string) []string {
	seen := map[string]bool{}
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if !safeEnvKey(key) || seen[key] {
			continue
		}
		seen[key] = true
		values = append(values, key+"=${"+key+"}")
	}
	return values
}

func sensitiveRunnerOptionFields(opts RunnerOptions) []string {
	var fields []string
	if strings.TrimSpace(opts.GitHubToken) != "" {
		fields = append(fields, "github_token")
	}
	if len(opts.HarborAgentEnv) > 0 {
		fields = append(fields, "harbor_agent_env_values")
	}
	sort.Strings(fields)
	return fields
}

func unsupportedRunnerOptionFields(opts RunnerOptions) []string {
	var fields []string
	if opts.HarborExec != nil {
		fields = append(fields, "harbor_exec")
	}
	if opts.VerifyExec != nil {
		fields = append(fields, "verify_exec")
	}
	if opts.Agent != nil {
		fields = append(fields, "agent_runtime")
	}
	sort.Strings(fields)
	return fields
}

func compactStrings(values []string) []string {
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func harborAgentEnvKeys(values []string) []string {
	seen := map[string]bool{}
	var keys []string
	for _, value := range values {
		key := strings.TrimSpace(value)
		if idx := strings.Index(key, "="); idx >= 0 {
			key = strings.TrimSpace(key[:idx])
		} else {
			key = ""
		}
		if key == "" || !safeEnvKey(key) || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func safeEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			if i == 0 && r >= '0' && r <= '9' {
				return false
			}
			continue
		}
		return false
	}
	return true
}

func (r *Runner) reviewGate(ctx context.Context, gateID, gateName, nodeID, message string, checklist []domain.ChecklistItem, artifacts []domain.ArtifactPreview, phase string) (domain.GateDecision, error) {
	request := domain.GateRequest{
		RequestID: stableGateRequestID(phase, gateID),
		GateID:    gateID,
		GateName:  gateName,
		NodeID:    nodeID,
		Message:   message,
		Checklist: checklist,
		Artifacts: artifacts,
		CreatedAt: time.Now().UTC(),
	}
	r.emitEvent(domain.RunnerEvent{
		Type:      "gate_requested",
		NodeID:    nodeID,
		Status:    "waiting",
		Message:   gateName + " waiting for decision",
		Artifacts: artifacts,
		Gate:      &request,
		CreatedAt: time.Now().UTC(),
	})
	var decision domain.GateDecision
	if r.opts.AutoApprove {
		decision = domain.GateDecision{RequestID: request.RequestID, GateID: gateID, Action: "approve", Approved: true, Notes: "auto-approved by headless run", DecidedAt: time.Now().UTC()}
	} else {
		poll := time.NewTicker(500 * time.Millisecond)
		defer poll.Stop()
		for {
			select {
			case <-ctx.Done():
				return domain.GateDecision{}, ctx.Err()
			case candidate := <-r.decisions:
				if candidate.RequestID != request.RequestID {
					continue
				}
				decision = candidate
				if decision.GateID == "" {
					decision.GateID = gateID
				}
				if decision.DecidedAt.IsZero() {
					decision.DecidedAt = time.Now().UTC()
				}
			case <-poll.C:
				candidate, ok, err := r.readGateDecision(phase, gateID, request.RequestID)
				if err != nil {
					r.recordPersistenceError("gate decision: " + err.Error())
					continue
				}
				if !ok {
					continue
				}
				decision = candidate
			}
			break
		}
	}
	decision = enforceGateDecision(request, decision)
	if strings.TrimSpace(decision.Action) == "" {
		if decision.Approved {
			decision.Action = "approve"
		} else {
			decision.Action = "reject"
		}
	}
	if err := r.writeGateDecision(phase, gateID, decision); err != nil {
		return domain.GateDecision{}, err
	}
	decision = sanitizeGateDecision(decision)
	r.mu.Lock()
	r.gates = append(r.gates, decision)
	r.mu.Unlock()
	return decision, nil
}

func (r *Runner) archiveGateRevision(phase, gateID string, decision domain.GateDecision, revision int) error {
	path := r.gateDecisionPath(phase, gateID)
	dir := filepath.Join(filepath.Dir(path), "revisions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sanitizeGateDecision(decision), "", "  ")
	if err != nil {
		return err
	}
	revisionPath := filepath.Join(dir, fmt.Sprintf("revision-%03d.json", revision))
	if err := os.WriteFile(revisionPath, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func stableGateRequestID(phase, gateID string) string {
	phase = strings.TrimSpace(phase)
	gateID = strings.TrimSpace(gateID)
	if phase == "" {
		phase = "phase2"
	}
	if gateID == "" {
		gateID = "gate"
	}
	return phase + ":" + gateID
}

func (r *Runner) writeGateDecision(phase, gateID string, decision domain.GateDecision) error {
	decision = sanitizeGateDecision(decision)
	path := r.gateDecisionPath(phase, gateID)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(decision, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func (r *Runner) readGateDecision(phase, gateID, requestID string) (domain.GateDecision, bool, error) {
	raw, err := os.ReadFile(r.gateDecisionPath(phase, gateID))
	if err != nil {
		if os.IsNotExist(err) {
			return domain.GateDecision{}, false, nil
		}
		return domain.GateDecision{}, false, err
	}
	var decision domain.GateDecision
	if err := json.Unmarshal(raw, &decision); err != nil {
		return domain.GateDecision{}, false, err
	}
	if decision.RequestID != requestID {
		return domain.GateDecision{}, false, nil
	}
	if decision.GateID == "" {
		decision.GateID = gateID
	}
	if decision.DecidedAt.IsZero() {
		decision.DecidedAt = time.Now().UTC()
	}
	return decision, true, nil
}

func (r *Runner) gateDecisionPath(phase, gateID string) string {
	workspace := r.opts.Workspace
	if workspace == "" {
		workspace = ".harbor-factory/workspace"
	}
	if strings.TrimSpace(phase) == "" {
		phase = "phase2"
	}
	return nodes.ReviewDecisionPath(workspace, phase, gateID)
}

func sanitizeGateDecision(decision domain.GateDecision) domain.GateDecision {
	return sanitize.GateDecision(decision)
}

func enforceGateDecision(request domain.GateRequest, decision domain.GateDecision) domain.GateDecision {
	blockers := blockingGateChecklist(request.Checklist)
	if decision.Approved && len(blockers) > 0 {
		decision.Approved = false
		note := "rejected because critical checks are failing: " + strings.Join(blockers, "; ")
		if strings.TrimSpace(decision.Notes) != "" {
			decision.Notes = strings.TrimSpace(decision.Notes) + "\n" + note
		} else {
			decision.Notes = note
		}
	}
	return decision
}

func blockingGateChecklist(checklist []domain.ChecklistItem) []string {
	var blockers []string
	for _, item := range checklist {
		if item.Critical && !item.Passed {
			label := strings.TrimSpace(item.Label)
			if label == "" {
				label = strings.TrimSpace(item.ID)
			}
			if label == "" {
				label = "unnamed critical check"
			}
			blockers = append(blockers, commandlog.RedactText(label))
		}
	}
	return blockers
}

func genChecklist(report domain.GenReport) []domain.ChecklistItem {
	proposal := report.TaskProposal
	items := []domain.ChecklistItem{
		{ID: "task_dir", Label: "generated Harbor task directory: " + report.TaskDir, Critical: true, Passed: report.TaskDir != ""},
		{ID: "repo_fixed", Label: "Docker source is fixed to commit " + proposal.CommitSHA, Critical: true, Passed: proposal.CommitSHA != ""},
		{ID: "task_metadata", Label: fmt.Sprintf("%s / %s / %s", proposal.CodeLang, proposal.TaskType, proposal.Application), Critical: true, Passed: proposal.CodeLang != "" && proposal.TaskType != "" && proposal.Application != ""},
	}
	items = append(items, generatedTaskChecklist(report.TaskDir, report.TestsAnalysisPath)...)
	return items
}

func generatedTaskChecklist(taskDir, testsAnalysisPath string) []domain.ChecklistItem {
	required := []struct {
		id    string
		label string
		path  string
	}{
		{id: "instruction_file", label: "instruction.md is present and non-empty", path: filepath.Join(taskDir, "instruction.md")},
		{id: "task_toml_file", label: "task.toml is present and non-empty", path: filepath.Join(taskDir, "task.toml")},
		{id: "dockerfile_file", label: "environment/Dockerfile is present and non-empty", path: filepath.Join(taskDir, "environment", "Dockerfile")},
		{id: "solve_file", label: "solution/solve.sh is present and non-empty", path: filepath.Join(taskDir, "solution", "solve.sh")},
		{id: "test_file", label: "tests/test.sh is present and non-empty", path: filepath.Join(taskDir, "tests", "test.sh")},
		{id: "tests_analysis_root_file", label: "tests_analysis.md is present and non-empty", path: filepath.Join(taskDir, "tests_analysis.md")},
		{id: "tests_analysis_artifact", label: "tests analysis artifact generated: " + testsAnalysisPath, path: testsAnalysisPath},
	}
	items := make([]domain.ChecklistItem, 0, len(required)+2)
	for _, item := range required {
		items = append(items, domain.ChecklistItem{
			ID:       item.id,
			Label:    item.label,
			Critical: true,
			Passed:   nonEmptyRegularFile(item.path),
		})
	}
	extras, legacy, err := generatedTaskResidue(taskDir)
	if err != nil {
		items = append(items, domain.ChecklistItem{ID: "task_file_scan", Label: "generated task file scan failed: " + err.Error(), Critical: true, Passed: false})
		return items
	}
	extraLabel := "no unexpected files in generated Harbor task directory"
	if len(extras) > 0 {
		extraLabel = "unexpected files: " + strings.Join(limitStrings(extras, 5), ", ")
	}
	items = append(items, domain.ChecklistItem{ID: "no_unexpected_files", Label: extraLabel, Critical: true, Passed: len(extras) == 0})
	legacyLabel := "no PPT/promptflow/image2 legacy residue in generated Harbor task files"
	if len(legacy) > 0 {
		legacyLabel = "legacy residue: " + strings.Join(limitStrings(legacy, 5), ", ")
	}
	items = append(items, domain.ChecklistItem{ID: "no_legacy_domain_residue", Label: legacyLabel, Critical: true, Passed: len(legacy) == 0})
	return items
}

func nonEmptyRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func generatedTaskResidue(taskDir string) ([]string, []string, error) {
	var extras []string
	var legacy []string
	if strings.TrimSpace(taskDir) == "" {
		return []string{"missing task directory"}, nil, nil
	}
	err := filepath.WalkDir(taskDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(taskDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.Type()&os.ModeSymlink != 0 {
			extras = append(extras, rel+" (symlink)")
			return nil
		}
		if !taskpolicy.IsAllowedFile(rel) {
			extras = append(extras, rel)
		}
		if taskpolicy.ContainsLegacyDomain(rel) {
			legacy = append(legacy, rel)
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if taskpolicy.ContainsLegacyDomain(string(raw)) {
			legacy = append(legacy, rel)
		}
		return nil
	})
	return extras, legacy, err
}

func limitStrings(values []string, limit int) []string {
	if limit <= 0 || len(values) <= limit {
		return values
	}
	out := append([]string(nil), values[:limit]...)
	out = append(out, "...")
	return out
}

func (r *Runner) runTaskRepair(ctx context.Context, taskDir, source, guidance string, findings []string, round int) (repair.Report, error) {
	source = normalizedRepairSource(source)
	agent := r.opts.Agent
	if agent == nil {
		agent = codexruntime.New(nil, r.opts.CodexPath, nil)
	}
	return repair.Run(ctx, repair.Options{
		TaskDir:         taskDir,
		Guidance:        guidance,
		Findings:        findings,
		Source:          source,
		Round:           round,
		Agent:           agent,
		Model:           r.opts.Model,
		ReasoningEffort: r.opts.Reasoning,
		TimeoutSeconds:  r.opts.AgentTimeout,
		LogPath:         nodes.TaskRepairAgentLogPath(r.opts.Workspace, source, round),
		WriteReport:     nodes.TaskRepairReportPath(r.opts.Workspace, source, round),
	})
}

func normalizedRepairSource(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "final_review":
		return "final_review"
	case "external_review":
		return "external_review"
	default:
		return "operator_review"
	}
}

func clearReviewEvidence(summary *domain.RunSummary, qwenResult, opusResult *string) {
	summary.Passed = true
	summary.LintReport = nil
	summary.VerifyReport = nil
	summary.QualityReport = nil
	summary.SimilarityReport = nil
	summary.QwenResult = nil
	summary.OpusResult = nil
	*qwenResult = ""
	*opusResult = ""
}

func taskProposalChecklist(proposal domain.TaskProposal) []domain.ChecklistItem {
	return []domain.ChecklistItem{
		{ID: "task_name", Label: "task name: " + proposal.TaskName, Critical: true, Passed: strings.TrimSpace(proposal.TaskName) != ""},
		{ID: "repo_commit", Label: "repo and commit are fixed: " + proposal.GitHubLink + " @ " + proposal.CommitSHA, Critical: true, Passed: strings.TrimSpace(proposal.GitHubLink) != "" && strings.TrimSpace(proposal.CommitSHA) != ""},
		{ID: "metadata", Label: fmt.Sprintf("%s / %s / %s", proposal.CodeLang, proposal.TaskType, proposal.Application), Critical: true, Passed: strings.TrimSpace(proposal.CodeLang) != "" && strings.TrimSpace(proposal.TaskType) != "" && strings.TrimSpace(proposal.Application) != ""},
		{ID: "difficulty", Label: "difficulty rationale and AHT are present", Critical: true, Passed: strings.TrimSpace(proposal.DifficultyRationale) != "" && proposal.EstimatedAHTMinutes > 0},
	}
}

func lintChecklist(report domain.LintReport) []domain.ChecklistItem {
	items := make([]domain.ChecklistItem, 0, len(report.Checks))
	for _, check := range report.Checks {
		items = append(items, domain.ChecklistItem{
			ID:       check.ID,
			Label:    check.Message,
			Critical: check.Status != domain.CheckWarn,
			Passed:   check.Status == domain.CheckPass || check.Status == domain.CheckWarn,
		})
	}
	return items
}

func qualityChecklist(report domain.QualityReport) []domain.ChecklistItem {
	items := make([]domain.ChecklistItem, 0, len(report.Checks))
	keys := make([]string, 0, len(report.Checks))
	for key := range report.Checks {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		check := report.Checks[key]
		items = append(items, domain.ChecklistItem{
			ID:       "quality_" + key,
			Label:    check.Detail,
			Critical: strings.EqualFold(check.Severity, "error"),
			Passed:   check.Passed || !strings.EqualFold(check.Severity, "error"),
		})
	}
	return items
}

func similarityChecklist(report domain.SimilarityReport) []domain.ChecklistItem {
	items := []domain.ChecklistItem{
		{
			ID:       "similarity_overall",
			Label:    fmt.Sprintf("max similarity %.3f, threshold %.3f", report.MaxScore, report.Threshold),
			Critical: true,
			Passed:   report.OverallPass,
		},
	}
	for idx, candidate := range report.Candidates {
		if idx >= 5 {
			break
		}
		label := fmt.Sprintf("%s %.3f %s", candidate.Source, candidate.Score, firstNonEmpty(candidate.Title, candidate.Path, candidate.URL))
		items = append(items, domain.ChecklistItem{
			ID:       fmt.Sprintf("similarity_candidate_%d", idx+1),
			Label:    label,
			Critical: candidate.Score >= report.Threshold,
			Passed:   candidate.Score < report.Threshold,
		})
	}
	return items
}

func resultChecklist(qwen, opus *domain.TrialResult, taskDir, qwenModel, opusModel, harborAgent, qwenScreenshot, opusScreenshot string) []domain.ChecklistItem {
	var items []domain.ChecklistItem
	items = append(items, trialResultChecklist("qwen", "Qwen", qwen, harborResultValidationOptions(taskDir, qwenModel, harborAgent, true))...)
	items = append(items, trialResultChecklist("opus", "Opus", opus, harborResultValidationOptions(taskDir, opusModel, harborAgent, false))...)
	items = append(items, screenshotChecklist("qwen_screenshot", "Qwen", qwen, qwenScreenshot), screenshotChecklist("opus_screenshot", "Opus", opus, opusScreenshot))
	return items
}

func harborResultValidationOptions(taskDir, expectedModel, harborAgent string, qwen bool) harborrun.ValidationOptions {
	return harborrun.ValidationOptions{
		Qwen:              qwen,
		ExpectedModel:     expectedModel,
		ExpectedAgent:     harborAgent,
		TaskDir:           taskDir,
		RequireRuns:       true,
		RequireTaskDigest: true,
		RequireCommandRun: true,
	}
}

func trialResultChecklist(prefix, label string, result *domain.TrialResult, validation harborrun.ValidationOptions) []domain.ChecklistItem {
	if result == nil {
		return []domain.ChecklistItem{{
			ID:       prefix + "_result_present",
			Label:    label + " Harbor result is present",
			Critical: true,
			Passed:   false,
		}}
	}
	failures := harborrun.ValidateForCodeEdgeWithOptions(*result, validation)
	items := []domain.ChecklistItem{{
		ID:       prefix + "_strict_result",
		Label:    fmt.Sprintf("%s strict Harbor result validation", label),
		Critical: true,
		Passed:   len(failures) == 0,
	}}
	for idx, failure := range failures {
		items = append(items, domain.ChecklistItem{
			ID:       fmt.Sprintf("%s_strict_failure_%d", prefix, idx+1),
			Label:    failure,
			Critical: true,
			Passed:   false,
		})
	}
	return items
}

func screenshotChecklist(id, label string, result *domain.TrialResult, provided string) domain.ChecklistItem {
	path := firstNonEmpty(provided, trialScreenshot(result))
	return domain.ChecklistItem{
		ID:       id,
		Label:    fmt.Sprintf("%s pass@4 screenshot present", label),
		Critical: true,
		Passed:   isReadableFile(path),
	}
}

func trialScreenshot(result *domain.TrialResult) string {
	if result == nil {
		return ""
	}
	return result.Screenshot
}

func (r *Runner) emitVerifyStepEvents(report domain.VerifyReport) {
	r.emitVerifyCommandEvent(nodes.DockerBuild, report.DockerBuild, "Docker build")
	r.emitVerifyCommandEvent(nodes.InitialVerify, report.InitialVerify, "Initial verification")
	r.emitVerifyCommandEvent(nodes.OracleVerify, report.OracleVerify, "Oracle verification")
}

func (r *Runner) emitVerifyCommandEvent(nodeID string, run *domain.CommandRun, label string) {
	if run == nil {
		return
	}
	status := "failed"
	eventType := "node_failed"
	if run.Passed {
		status = "succeeded"
		eventType = "node_succeeded"
	}
	r.emit(eventType, nodeID, status, fmt.Sprintf("%s exit=%d", label, run.ExitCode), verifyCommandArtifactPath(r.opts.Workspace, nodeID))
}

func verifyCommandArtifactPath(workspace, nodeID string) string {
	return nodes.PrimaryArtifactPath(workspace, nodeID)
}

func gateArtifacts(paths ...string) []domain.ArtifactPreview {
	var artifacts []domain.ArtifactPreview
	for _, path := range paths {
		if path == "" {
			continue
		}
		raw, err := os.ReadFile(path)
		content := ""
		if err == nil {
			content = commandlog.RedactText(string(raw))
			if len(content) > 8000 {
				content = content[:8000] + "\n... truncated ..."
			}
		}
		artifacts = append(artifacts, domain.ArtifactPreview{Name: filepath.Base(path), Path: path, Content: content})
	}
	return artifacts
}

func isReadableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func sanitizeRunnerEvent(event domain.RunnerEvent) domain.RunnerEvent {
	return sanitize.RunnerEvent(event)
}

func defaultString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return strings.TrimSpace(fallback)
	}
	return value
}

func defaultInt(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func formatAHT(minutes int) string {
	if minutes <= 0 {
		return ""
	}
	if minutes == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", minutes)
}

type taskDefaults struct {
	TaskName            string
	Description         string
	CodeLang            string
	TaskType            string
	Application         string
	IsZeroToOne         bool
	GitHubURL           string
	CommitID            string
	EstimatedAHTMinutes int
}

func readTaskDefaults(taskDir string) taskDefaults {
	raw, err := os.ReadFile(filepath.Join(taskDir, "task.toml"))
	if err != nil {
		return taskDefaults{}
	}
	var parsed struct {
		Task struct {
			Name        string `toml:"name"`
			Description string `toml:"description"`
		} `toml:"task"`
		Metadata struct {
			CodeLang            string `toml:"code_lang"`
			TaskType            string `toml:"task_type"`
			Application         string `toml:"application"`
			IsZeroToOne         bool   `toml:"is_0_to_1"`
			GitHubURL           string `toml:"github_url"`
			CommitID            string `toml:"commit_id"`
			EstimatedAHTMinutes int    `toml:"estimated_aht_minutes"`
		} `toml:"metadata"`
	}
	if err := toml.Unmarshal(raw, &parsed); err != nil {
		return taskDefaults{}
	}
	return taskDefaults{
		TaskName:            parsed.Task.Name,
		Description:         parsed.Task.Description,
		CodeLang:            parsed.Metadata.CodeLang,
		TaskType:            parsed.Metadata.TaskType,
		Application:         parsed.Metadata.Application,
		IsZeroToOne:         parsed.Metadata.IsZeroToOne,
		GitHubURL:           parsed.Metadata.GitHubURL,
		CommitID:            parsed.Metadata.CommitID,
		EstimatedAHTMinutes: parsed.Metadata.EstimatedAHTMinutes,
	}
}

func packageTaskName(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Trim(name, "/")
	if name == "" {
		return ""
	}
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	return strings.TrimSpace(name)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (r *Runner) gateSnapshot() []domain.GateDecision {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.GateDecision(nil), r.gates...)
}

func (r *Runner) snapshot() []domain.RunnerEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.RunnerEvent(nil), r.log...)
}
