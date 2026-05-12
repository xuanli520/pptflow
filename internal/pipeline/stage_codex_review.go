package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/codex"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

type CodexReviewStage struct {
	runner Runner
	spec   CodexReviewStageSpec
}

type CodexReviewStageSpec struct {
	ID                     string
	Profile                string
	Output                 string
	RecheckOutput          string
	CompatOutputs          []string
	RecheckCompatOutputs   []string
	ArtifactPaths          func(StageContext, CodexReviewPaths) []string
	BeforeReview           func(*model.StageRecord, ArtifactWriter, StageContext, CodexReviewPaths)
	BuildContext           func(context.Context, Runner, StageContext) (string, error)
	UnavailableFinding     func(CodexReviewUnavailable) model.Finding
	ErrorSummary           func(codexReviewFailureKind) string
	ValidateExtraArgsEarly bool
}

type CodexReviewPaths struct {
	LogPath     string
	OutputPath  string
	CompatPaths []string
}

type CodexReviewUnavailable struct {
	Stage       string
	Profile     string
	ProjectPath string
	OutputPath  string
	Reason      string
	Kind        codexReviewFailureKind
}

type codexReviewFailureKind string

const (
	codexReviewProfileFailure     codexReviewFailureKind = "profile"
	codexReviewNetworkFailure     codexReviewFailureKind = "network"
	codexReviewWritableTmpFailure codexReviewFailureKind = "writable_tmp"
	codexReviewCapabilityFailure  codexReviewFailureKind = "capability"
	codexReviewContextFailure     codexReviewFailureKind = "context"
	codexReviewExtraArgsFailure   codexReviewFailureKind = "extra_args"
	codexReviewSandboxFailure     codexReviewFailureKind = "sandbox"
)

func (s CodexReviewStage) ID() string {
	return s.spec.ID
}

func (s CodexReviewStage) Execute(ctx context.Context, sc StageContext) StageOutcome {
	return StageOutcome{Record: s.execute(ctx, sc)}
}

func (s CodexReviewStage) MaterializesBlockedPreflight() bool {
	return true
}

func (s CodexReviewStage) execute(ctx context.Context, sc StageContext) model.StageRecord {
	start := time.Now()
	spec := s.spec.withDefaults()
	stage := spec.ID
	record := startStage(stage)
	writer := artifactWriterForStageContext(sc)
	paths := spec.paths(sc)
	record.LogPath = paths.LogPath
	record.ArtifactPaths = spec.artifactPaths(sc, paths)

	if spec.BeforeReview != nil {
		spec.BeforeReview(&record, writer, sc, paths)
	}

	profilePath := filepath.Join(s.runner.cfg.Codex.PromptProfilesDir, spec.Profile)
	profileContent, readErr := os.ReadFile(profilePath)
	if readErr != nil {
		return s.finishUnavailable(record, writer, paths, start, spec, sc.Project.Path, codexReviewProfileFailure, "prompt profile not readable: "+readErr.Error())
	}
	if s.runner.cfg.Codex.Network != "none" {
		return s.finishUnavailable(record, writer, paths, start, spec, sc.Project.Path, codexReviewNetworkFailure, "configured Codex network mode is unsupported by the current safe sandbox: "+s.runner.cfg.Codex.Network)
	}
	if s.runner.cfg.Codex.WritableTmp {
		return s.finishUnavailable(record, writer, paths, start, spec, sc.Project.Path, codexReviewWritableTmpFailure, "configured writable_tmp=true is unsupported without widening write access in the current Codex CLI sandbox")
	}
	var extraArgs []string
	if spec.ValidateExtraArgsEarly {
		var extraErr error
		extraArgs, extraErr = safeCodexExtraArgs(s.runner.cfg.Codex.ExtraArgs)
		if extraErr != nil {
			return s.finishUnavailable(record, writer, paths, start, spec, sc.Project.Path, codexReviewExtraArgsFailure, extraErr.Error())
		}
	}
	reviewPath := codexReviewPath(sc.Run, sc.Project.Path)
	capability := codex.DetectCLI(ctx, s.runner.exec, "")
	if capabilityErr := codex.ValidateAppServerCapability(capability); capabilityErr != nil {
		return s.finishUnavailable(record, writer, paths, start, spec, sc.Project.Path, codexReviewCapabilityFailure, capabilityErr.Error())
	}
	contextText, contextErr := spec.buildContext(ctx, s.runner, sc)
	if contextErr != nil {
		return s.finishUnavailable(record, writer, paths, start, spec, sc.Project.Path, codexReviewContextFailure, contextErr.Error())
	}
	if !spec.ValidateExtraArgsEarly {
		var extraErr error
		extraArgs, extraErr = safeCodexExtraArgs(s.runner.cfg.Codex.ExtraArgs)
		if extraErr != nil {
			return s.finishUnavailable(record, writer, paths, start, spec, sc.Project.Path, codexReviewExtraArgsFailure, extraErr.Error())
		}
	}
	sandbox, sandboxErr := codex.NewSandbox(reviewPath, sc.Run.ArtifactRoot, stage)
	if sandboxErr != nil {
		return s.finishUnavailable(record, writer, paths, start, spec, sc.Project.Path, codexReviewSandboxFailure, sandboxErr.Error())
	}
	defer os.RemoveAll(sandbox.Home)

	env := sandbox.EnvWithNode(os.Environ(), s.runner.cfg.Codex.Env, capability.NodePath)
	timeout := stageTimeoutForStageContext(sc, s.runner, stage, 300)
	prompt := codexPrompt(stage, spec.Profile, reviewPath, sc.Project.Path, sc.Run.ArtifactRoot, string(profileContent), contextText)
	args := append([]string{}, extraArgs...)
	review := s.runner.runCodexReviewWithLog(ctx, timeout, reviewPath, paths.LogPath, env, prompt, capability, args, codexDeltaProgress(sc.Run.RunID, stage, sc.Progress))
	recordArtifactWarnings(&record, writer, review.ArtifactWarnings)
	outcome := finalizeStaticReviewReport(stage, spec.Profile, sc.Project.Path, paths.OutputPath, review.Result, s.runner.cfg.Codex.MaxOutputBytes)
	record.Findings = append(record.Findings, outcome.Findings...)
	record = s.writeReports(record, writer, paths, outcome.Report+"\n")
	if outcome.ErrorSummary != "" {
		if record.ErrorSummary == "" {
			record.ErrorSummary = outcome.ErrorSummary
		}
		return finishStage(record, model.StageFailed, start)
	}
	return finishStage(record, model.StageDone, start)
}

func (s CodexReviewStage) finishUnavailable(record model.StageRecord, writer ArtifactWriter, paths CodexReviewPaths, start time.Time, spec CodexReviewStageSpec, projectPath string, kind codexReviewFailureKind, reason string) model.StageRecord {
	report := staticUnavailableReport(spec.ID, spec.Profile, projectPath, reason)
	record = s.writeReports(record, writer, paths, report)
	bestEffortStageText(&record, writer, writer.RelativePath(paths.LogPath), report)
	record.Findings = append(record.Findings, spec.unavailableFinding(CodexReviewUnavailable{
		Stage:       spec.ID,
		Profile:     spec.Profile,
		ProjectPath: projectPath,
		OutputPath:  paths.OutputPath,
		Reason:      reason,
		Kind:        kind,
	}))
	if record.ErrorSummary == "" {
		record.ErrorSummary = spec.errorSummary(kind)
	}
	return finishStage(record, model.StageFailed, start)
}

func (s CodexReviewStage) writeReports(record model.StageRecord, writer ArtifactWriter, paths CodexReviewPaths, content string) model.StageRecord {
	record = requiredStageText(record, writer, writer.RelativePath(paths.OutputPath), content)
	for _, path := range paths.CompatPaths {
		bestEffortStageText(&record, writer, writer.RelativePath(path), content)
	}
	return record
}

func (s CodexReviewStageSpec) withDefaults() CodexReviewStageSpec {
	if s.UnavailableFinding == nil {
		s.UnavailableFinding = defaultCodexReviewUnavailableFinding
	}
	if s.ErrorSummary == nil {
		s.ErrorSummary = defaultCodexReviewErrorSummary
	}
	return s
}

func (s CodexReviewStageSpec) paths(sc StageContext) CodexReviewPaths {
	output := s.Output
	if sc.Options.Mode == "recheck" && strings.TrimSpace(s.RecheckOutput) != "" {
		output = s.RecheckOutput
	}
	compatOutputs := s.CompatOutputs
	if sc.Options.Mode == "recheck" && s.RecheckCompatOutputs != nil {
		compatOutputs = s.RecheckCompatOutputs
	}
	outputPath := qaArtifactPath(sc.Run.ArtifactRoot, output)
	var compatPaths []string
	for _, name := range compatOutputs {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		path := qaArtifactPath(sc.Run.ArtifactRoot, name)
		if path == outputPath || containsPath(compatPaths, path) {
			continue
		}
		compatPaths = append(compatPaths, path)
	}
	return CodexReviewPaths{
		LogPath:     stageLogPath(sc.Run.ArtifactRoot, s.ID),
		OutputPath:  outputPath,
		CompatPaths: compatPaths,
	}
}

func (s CodexReviewStageSpec) artifactPaths(sc StageContext, paths CodexReviewPaths) []string {
	if s.ArtifactPaths != nil {
		return s.ArtifactPaths(sc, paths)
	}
	result := appendUniqueArtifactPath(nil, paths.OutputPath)
	for _, path := range paths.CompatPaths {
		result = appendUniqueArtifactPath(result, path)
	}
	return result
}

func (s CodexReviewStageSpec) buildContext(ctx context.Context, runner Runner, sc StageContext) (string, error) {
	if s.BuildContext == nil {
		return runner.codexContext(ctx, sc.Project, sc.Options, s.ID)
	}
	return s.BuildContext(ctx, runner, sc)
}

func (s CodexReviewStageSpec) unavailableFinding(info CodexReviewUnavailable) model.Finding {
	return s.UnavailableFinding(info)
}

func (s CodexReviewStageSpec) errorSummary(kind codexReviewFailureKind) string {
	return s.ErrorSummary(kind)
}

func artifactWriterForStageContext(sc StageContext) ArtifactWriter {
	if strings.TrimSpace(sc.Writer.Root) != "" {
		return sc.Writer
	}
	return NewArtifactWriter(sc.Run.ArtifactRoot)
}

func stageTimeoutForStageContext(sc StageContext, runner Runner, stage string, fallback int) time.Duration {
	if sc.Timeout != nil {
		return sc.Timeout(stage, fallback)
	}
	return runner.stageTimeout(stage, fallback)
}

func defaultCodexReviewUnavailableFinding(info CodexReviewUnavailable) model.Finding {
	switch info.Kind {
	case codexReviewProfileFailure:
		return model.Finding{
			Stage:      info.Stage,
			Severity:   "High",
			Title:      stageName(info.Stage) + " profile missing",
			Rule:       "Static review stages require an embedded prompt profile.",
			Evidence:   strings.TrimPrefix(info.Reason, "prompt profile not readable: "),
			Impact:     "Static review evidence is incomplete and requires manual verification.",
			MinimumFix: "Ensure assets were released to .qa-control and rerun this stage.",
		}
	case codexReviewNetworkFailure:
		return model.Finding{
			Stage:      info.Stage,
			Severity:   "High",
			Title:      stageName(info.Stage) + " network policy unsupported",
			Rule:       "D/E must execute under an enforceable no-network static sandbox for MVP.",
			Evidence:   "codex.network=" + strings.TrimPrefix(info.Reason, "configured Codex network mode is unsupported by the current safe sandbox: "),
			Impact:     "Static review evidence is incomplete because requested network behavior cannot be safely enforced.",
			MinimumFix: "Set codex.network to none or implement a dedicated network-controlled sandbox runner.",
		}
	case codexReviewWritableTmpFailure:
		return model.Finding{
			Stage:      info.Stage,
			Severity:   "High",
			Title:      stageName(info.Stage) + " writable tmp policy unsupported",
			Rule:       "D/E must not gain project write access during static review.",
			Evidence:   "codex.writable_tmp=true",
			Impact:     "Static review evidence is incomplete because artifact-only writes cannot be safely enforced.",
			MinimumFix: "Set codex.writable_tmp to false or implement artifact-only writable sandbox mounting.",
		}
	case codexReviewExtraArgsFailure:
		return model.Finding{
			Stage:      info.Stage,
			Severity:   "High",
			Title:      stageName(info.Stage) + " extra_args invalid",
			Rule:       "codex.extra_args for app-server static review may only select the model; boundary-changing or protocol-level arguments are rejected.",
			Evidence:   info.Reason,
			Impact:     "Static review cannot start because the configured Codex arguments would make the app-server contract ambiguous or unsafe.",
			MinimumFix: "Remove unsupported codex.extra_args, or keep only --model/-m with a non-empty model name.",
			SourcePath: info.OutputPath,
		}
	case codexReviewSandboxFailure:
		return model.Finding{
			Stage:      info.Stage,
			Severity:   "High",
			Title:      stageName(info.Stage) + " sandbox setup failed",
			Rule:       "Static review stages require a writable stage scratch directory.",
			Evidence:   info.Reason,
			Impact:     "Static review evidence is incomplete and requires manual verification.",
			MinimumFix: "Ensure the run artifact directory is writable and rerun this stage.",
			SourcePath: info.OutputPath,
		}
	case codexReviewContextFailure:
		return model.Finding{
			Stage:      info.Stage,
			Severity:   "High",
			Title:      stageName(info.Stage) + " audit input unavailable",
			Rule:       "Static review inputs supplied for recheck or extra-doc workflows must be readable and stay within size limits.",
			Evidence:   info.Reason,
			Impact:     "Static review evidence is incomplete.",
			MinimumFix: "Fix unavailable recheck or extra-doc inputs and rerun.",
		}
	default:
		return model.Finding{
			Stage:      info.Stage,
			Severity:   "High",
			Title:      stageName(info.Stage) + " unavailable",
			Rule:       "Static review stages require Codex app-server active-turn steering support.",
			Evidence:   info.Reason,
			Impact:     "Static review evidence is incomplete and requires manual verification.",
			MinimumFix: "Install a Codex CLI version with app-server support and rerun this stage.",
		}
	}
}

func defaultCodexReviewErrorSummary(kind codexReviewFailureKind) string {
	switch kind {
	case codexReviewProfileFailure:
		return "prompt profile unavailable"
	case codexReviewNetworkFailure:
		return "codex network policy unsupported"
	case codexReviewWritableTmpFailure:
		return "codex writable tmp policy unsupported"
	case codexReviewExtraArgsFailure:
		return "unsafe codex extra_args"
	case codexReviewContextFailure:
		return "audit input unavailable"
	case codexReviewSandboxFailure:
		return "codex sandbox unavailable"
	default:
		return "codex unavailable"
	}
}

func codexReviewStageSpecs() []CodexReviewStageSpec {
	return []CodexReviewStageSpec{
		stageDCodexReviewSpec(),
		stageECodexReviewSpec(),
		stageFCodexReviewSpec(),
	}
}

func stageDCodexReviewSpec() CodexReviewStageSpec {
	return CodexReviewStageSpec{
		ID:            string(model.StageD),
		Profile:       "tests_coverage_report.md",
		Output:        "test_effectiveness_report.md",
		RecheckOutput: "test_effectiveness_verification.md",
	}
}

func stageECodexReviewSpec() CodexReviewStageSpec {
	return CodexReviewStageSpec{
		ID:            string(model.StageE),
		Profile:       "static_acceptance_audit.md",
		Output:        "codex_report.md",
		RecheckOutput: "codex_report_verification.md",
	}
}

func stageFCodexReviewSpec() CodexReviewStageSpec {
	return CodexReviewStageSpec{
		ID:            string(model.StageF),
		Profile:       "annotator_fix.md",
		Output:        "operator_prompt_requirements_verification.md",
		RecheckOutput: "prompt_requirements_verification.md",
		ArtifactPaths: func(sc StageContext, paths CodexReviewPaths) []string {
			return []string{
				filepath.Join(sc.Run.ArtifactRoot, "repair_summary.json"),
				paths.OutputPath,
				stageFIssueReportPath(sc.Run.ArtifactRoot, sc.Options),
			}
		},
		BeforeReview: func(record *model.StageRecord, writer ArtifactWriter, sc StageContext, paths CodexReviewPaths) {
			stageStatuses, priorFindings := priorStageSnapshot(sc.Prior)
			writeRepairSupplements(record, writer, sc.Run, stageStatuses, priorFindings, filepath.Join(sc.Run.ArtifactRoot, "repair_summary.json"), stageFIssueReportPath(sc.Run.ArtifactRoot, sc.Options))
		},
		BuildContext: func(ctx context.Context, runner Runner, sc StageContext) (string, error) {
			stageStatuses, priorFindings := priorStageSnapshot(sc.Prior)
			contextText, err := runner.codexContext(ctx, sc.Project, sc.Options, "F")
			if err != nil {
				return "", err
			}
			return contextText + "\n" + stageFPreviousFindingsContext(stageStatuses, priorFindings), nil
		},
		UnavailableFinding: stageFUnavailableFinding,
		ErrorSummary: func(codexReviewFailureKind) string {
			return "codex unavailable"
		},
		ValidateExtraArgsEarly: true,
	}
}

func stageFUnavailableFinding(info CodexReviewUnavailable) model.Finding {
	return model.Finding{
		Stage:      string(model.StageF),
		Severity:   "High",
		Title:      "annotator repair static reviewer unavailable",
		Rule:       "Stage F requires a safe Codex static reviewer or explicit manual replacement.",
		Evidence:   info.Reason,
		Impact:     "Human manual review is required before relying on the repair report.",
		MinimumFix: "Restore Codex static review capability or manually complete the Stage F report.",
		SourcePath: info.OutputPath,
	}
}
