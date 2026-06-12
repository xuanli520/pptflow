package pipeline

import (
	"context"
	"errors"
	"os"
	"time"

	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/codex"
	"github.com/xuanli520/p2r_tui/internal/codex/appserver"
	"github.com/xuanli520/p2r_tui/internal/config"
	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

type TestPortMapping struct {
	Service   string `json:"service"`
	URL       string `json:"url,omitempty"`
	Host      int    `json:"host"`
	Container int    `json:"container"`
	Protocol  string `json:"protocol,omitempty"`
}

type TestProbeResult struct {
	Service string `json:"service"`
	URL     string `json:"url"`
	OK      bool   `json:"ok"`
	Status  int    `json:"status,omitempty"`
	Error   string `json:"error,omitempty"`
}

type TestRuntimeEvidence struct {
	ComposeProject string                       `json:"compose_project"`
	ComposeFile    string                       `json:"compose_file"`
	ComposeFiles   []string                     `json:"compose_files"`
	EnvFiles       []string                     `json:"env_files"`
	WorkDir        string                       `json:"work_dir"`
	Services       []string                     `json:"services"`
	Mappings       map[string][]TestPortMapping `json:"mappings"`
	Probes         []TestProbeResult            `json:"probes"`
	Mirror         TestRuntimeMirrorState       `json:"mirror,omitempty"`
}

type TestRuntimeMirrorState struct {
	BuildMirrorEnabled      bool   `json:"build_mirror_enabled,omitempty"`
	BuildMirrorMode         string `json:"build_mirror_mode,omitempty"`
	BuildMirrorFallbackUsed bool   `json:"build_mirror_fallback_used,omitempty"`
	BuildMirrorSummary      string `json:"build_mirror_summary,omitempty"`
}

type TestRunTestsComposeUsage struct {
	Uses            bool
	StartsStack     bool
	ExplicitProject bool
}

type TestStageCExecutionDecision struct {
	Requested                string
	Selected                 string
	Reason                   string
	UsesDocker               bool
	StartsDockerRuntime      bool
	UsesDockerCompose        bool
	StartsDockerComposeStack bool
	ExplicitComposeProject   bool
	ReferencesRuntimePorts   bool
	RuntimeEndpointHints     []string
}

type TestStageCCommandEnv struct {
	Env     []string
	Keys    []string
	Values  map[string]string
	Service TestServiceURLEnv
}

type TestStageCTestArtifactCleanup struct {
	Removed  []string `json:"removed"`
	Warnings []string `json:"warnings,omitempty"`
}

type TestSubmitArtifactCopy struct {
	Name        string
	Stage       string
	Source      string
	Target      string
	Optional    bool
	OK          bool
	NotSelected bool
	Error       string
}

type TestServiceURLEnv struct {
	Env     []string
	Keys    []string
	Mapping map[string]TestServiceURL
}

type TestServiceURL struct {
	EnvKey string `json:"env_key"`
	URL    string `json:"url"`
}

type TestBrowserURLCandidate = BrowserURLCandidate
type TestBrowserAction = BrowserAction
type TestBlockedBrowserAction = BlockedBrowserAction
type TestBrowserObservation = browserpkg.Observation
type TestFrontendE2ESummary = FrontendE2ESummary

type TestStageCProxyPlan struct {
	ComposeProject  string
	ComposeFiles    []string
	WorkDir         string
	RunnerName      string
	RunnerImage     string
	ProxyImage      string
	OverrideFile    string
	EnvFile         string
	ProxyConfigFile string
	Mappings        []TestStageCProxyMapping
	ServiceURLs     map[string]string
	Env             []string
	OverrideContent string
	EnvContent      string
}

type TestStageCProxyMapping struct {
	Listen   int
	Service  string
	Target   int
	Protocol string
}

func SelectedStagesForTest(opts RunOptions, staticOnly bool) map[string]bool {
	return selectedStages(opts, staticOnly)
}

func AssignFindingIDsForTest(stage string, findings []model.Finding) []model.Finding {
	return assignFindingIDs(stage, findings)
}

func ShortCommentForTest(stageStatuses map[string]string, findings []model.Finding) string {
	return shortComment(stageStatuses, findings)
}

func SplitStageFCodexReportForTest(report string) (string, string) {
	result := splitStageFCodexReport(report)
	return result.report1, result.report2
}

type StageFSplitResultForTest struct {
	Report1 string
	Report2 string
	Kind    string
}

func SplitStageFCodexReportFullForTest(report string) StageFSplitResultForTest {
	result := splitStageFCodexReport(report)
	return StageFSplitResultForTest{
		Report1: result.report1,
		Report2: result.report2,
		Kind:    string(result.kind),
	}
}

func ValidateStageFSplitForTest(splitResult StageFSplitResultForTest, report string) []model.Finding {
	return validateStageFSplit(stageFSplitResult{
		report1: splitResult.Report1,
		report2: splitResult.Report2,
		kind:    stageFSplitKind(splitResult.Kind),
	}, report)
}

func ReadmeComposeCommandForTest(repoPath string) []string {
	return readmeComposeCommand(repoPath)
}

func ExtractFindingsFromReportForTest(stage, report, sourcePath string) []model.Finding {
	return extractFindingsFromReport(stage, report, sourcePath)
}

func StaticReviewFindingsFromReportForTest(stage, report, sourcePath string) ([]model.Finding, error) {
	return staticReviewFindingsFromReport(stage, report, sourcePath)
}

func NormalizeStaticReviewReportForTest(report string) (string, error) {
	return normalizeStaticReviewReport(report)
}

func TruncateStaticReviewReportForTest(report string, limit int) string {
	return truncateStaticReviewReport(report, limit)
}

func StaticUnavailableReportForTest(stage, profile, projectPath, reason string) string {
	return staticUnavailableReport(stage, profile, projectPath, reason)
}

func AcceptanceFindingsForTest(path string) []model.Finding {
	return acceptanceFindings(path)
}

func AcceptanceScriptArgsForTest(outputs map[string]string, projectTypeArgs []string) []string {
	return acceptanceScriptArgs(outputs, projectTypeArgs)
}

func ValidationScriptArgsForTest(outputs map[string]string, projectTypeArgs []string) []string {
	return validationScriptArgs(outputs, projectTypeArgs)
}

func RunArtifactRootForTest(scanPath string, project scanner.Project, runID string) string {
	return runArtifactRoot(scanPath, project, runID)
}

func CopyPackageSnapshotForTest(source, dest string) error {
	return copyPackageSnapshot(source, dest)
}

func TerminalScreenshotLinesForTest(text string) []string {
	return terminalScreenshotLines(text)
}

func PackageTrajectorySummaryForTest(projectPath, scriptRoot string, snapshotErr error) string {
	return packageTrajectorySummary(projectPath, scriptRoot, snapshotErr)
}

func SafeCodexExtraArgsForTest(args []string) ([]string, error) {
	return safeCodexExtraArgs(args)
}

func CapabilitySummaryForTest(capability codex.Capability) string {
	return capabilitySummary(capability)
}

func RunCodexReviewSessionWithGuidanceForTest(ctx context.Context, session any, request CodexReviewRequest, deadlines []CodexGuidanceDeadline) (CodexReviewResult, error) {
	switch typed := session.(type) {
	case CodexReviewSession:
		return runCodexReviewSessionWithGuidance(ctx, typed, request, deadlines)
	case appserver.Session:
		return runCodexReviewSessionWithGuidance(ctx, appServerSessionAdapter{session: typed}, request, deadlines)
	default:
		return CodexReviewResult{}, errors.New("test codex review session does not implement a supported session interface")
	}
}

func CodexGuidanceScheduleForTest(timeout time.Duration, stage string) []CodexGuidanceDeadline {
	return codexGuidanceSchedule(timeout, stage)
}

func CodexReviewPathForTest(run model.RunRecord, projectPath string) string {
	return codexReviewPath(run, projectPath)
}

func (r Runner) CodexContextForTest(ctx context.Context, project scanner.Project, opts RunOptions, stage string) (string, error) {
	return r.codexContext(ctx, project, opts, stage)
}

func (r Runner) StageCodexForTest(ctx context.Context, run model.RunRecord, project scanner.Project, opts RunOptions, stage, profile, output string, compat ...string) model.StageRecord {
	return r.stageCodex(ctx, run, project, opts, stage, profile, output, nil, compat...)
}

func (r Runner) StageAForTest(ctx context.Context, run model.RunRecord, project scanner.Project) model.StageRecord {
	return r.stageA(ctx, run, project, nil)
}

func (r Runner) StageBForTest(ctx context.Context, run model.RunRecord, project scanner.Project) StageOutcome {
	return r.stageB(ctx, run, project, nil)
}

func (r Runner) StageFForTest(ctx context.Context, run model.RunRecord, project scanner.Project, opts RunOptions, prior map[string]model.StageRecord) model.StageRecord {
	return r.stageF(ctx, run, project, opts, prior, nil)
}

func (r Runner) StageGForTest(ctx context.Context, run model.RunRecord, project scanner.Project, runtime TestRuntimeEvidence) model.StageRecord {
	return r.stageG(ctx, StageContext{
		Run:     run,
		Project: project,
		Runtime: runtimeEvidenceFromTest(runtime),
		Writer:  NewArtifactWriter(run.ArtifactRoot),
		Timeout: r.stageTimeout,
	})
}

func WithStageGBrowserPlannerForTest(planner func(context.Context, StageContext, string, string, string, []TestBrowserURLCandidate, []TestBrowserObservation, []TestBlockedBrowserAction, int, time.Duration) (string, []ArtifactWarning, error)) RunnerOption {
	return func(r *Runner) {
		if planner == nil {
			return
		}
		r.stageGBrowserPlan = func(ctx context.Context, sc StageContext, promptTemplate, profile, contextText string, candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction, round int, timeout time.Duration) (string, []ArtifactWarning, error) {
			return planner(ctx, sc, promptTemplate, profile, contextText, candidates, observations, blocked, round, timeout)
		}
	}
}

func WithStageGBrowserActionRunnerForTest(runner func(context.Context, browserpkg.Action, browserpkg.Policy, time.Duration) (TestBrowserObservation, error)) RunnerOption {
	return func(r *Runner) {
		if runner == nil {
			return
		}
		r.stageGBrowserAction = func(ctx context.Context, action browserpkg.Action, policy browserpkg.Policy, timeout time.Duration) (browserpkg.Observation, error) {
			return runner(ctx, action, policy, timeout)
		}
	}
}

func SubmitArtifactNamesForTest(mode string) []string {
	return submitArtifactNames(mode)
}

func AggregateSubmitArtifactsForTest(artifactRoot, submitDir, mode string, selected map[string]bool) ([]TestSubmitArtifactCopy, error) {
	copies, err := aggregateSubmitArtifacts(artifactRoot, submitDir, submitArtifactSpecs(mode), selected)
	result := make([]TestSubmitArtifactCopy, 0, len(copies))
	for _, copy := range copies {
		result = append(result, TestSubmitArtifactCopy{
			Name:        copy.Name,
			Stage:       copy.Stage,
			Source:      copy.Source,
			Target:      copy.Target,
			Optional:    copy.Optional,
			OK:          copy.OK,
			NotSelected: copy.NotSelected,
			Error:       copy.Error,
		})
	}
	return result, err
}

func StructuralFindingsForTest(project scanner.Project, required map[string]bool) []model.Finding {
	return structuralFindings(project, required)
}

func ParseComposePSForTest(raw string) (map[string][]TestPortMapping, []string) {
	mappings, services := parseComposePS(raw)
	return testPortMappingMap(mappings), services
}

func ParseDockerPortForTest(service, raw string) []TestPortMapping {
	return testPortMappings(parseDockerPort(service, raw))
}

func StageCEnvironmentForTest(evidence TestRuntimeEvidence) TestStageCCommandEnv {
	env := stageCEnvironment(runtimeEvidenceFromTest(evidence))
	return TestStageCCommandEnv{
		Env:    append([]string{}, env.Env...),
		Keys:   append([]string{}, env.Keys...),
		Values: copyStringMap(env.Values),
		Service: TestServiceURLEnv{
			Env:     append([]string{}, env.Service.Env...),
			Keys:    append([]string{}, env.Service.Keys...),
			Mapping: testServiceURLMap(env.Service.Mapping),
		},
	}
}

func StageCProxyPlanForTest(evidence TestRuntimeEvidence, repoPath, artifactRoot, runnerImage, proxyImage string) (TestStageCProxyPlan, error) {
	plan, err := buildStageCProxyPlan(runtimeEvidenceFromTest(evidence), repoPath, artifactRoot, config.StageCConfig{
		RunnerImage: runnerImage,
		ProxyImage:  proxyImage,
	})
	if err != nil {
		return TestStageCProxyPlan{}, err
	}
	return testStageCProxyPlan(plan), nil
}

func StageCProxyPlanFromComposeContentForTest(evidence TestRuntimeEvidence, repoPath, artifactRoot, runnerImage, proxyImage, composeContent string) (TestStageCProxyPlan, error) {
	plan, err := buildStageCProxyPlanWithComposeContent(runtimeEvidenceFromTest(evidence), repoPath, artifactRoot, config.StageCConfig{
		RunnerImage: runnerImage,
		ProxyImage:  proxyImage,
	}, evidence.ComposeFile, composeContent)
	if err != nil {
		return TestStageCProxyPlan{}, err
	}
	return testStageCProxyPlan(plan), nil
}

func (r Runner) StageCForTest(ctx context.Context, run model.RunRecord, project scanner.Project, runtime TestRuntimeEvidence, prior map[string]model.StageRecord) model.StageRecord {
	return r.stageC(ctx, run, project, runtimeEvidenceFromTest(runtime), prior, nil)
}

func RunTestsComposeUsageForTest(repoPath string) TestRunTestsComposeUsage {
	usage := inspectRunTestsCompose(repoPath)
	return TestRunTestsComposeUsage{
		Uses:            usage.Uses,
		StartsStack:     usage.StartsStack,
		ExplicitProject: usage.ExplicitProject,
	}
}

func StageCExecutionDecisionForTest(cfg config.StageCConfig, repoPath string) TestStageCExecutionDecision {
	decision := selectStageCExecution(cfg, repoPath)
	return TestStageCExecutionDecision{
		Requested:                decision.Requested,
		Selected:                 decision.Selected,
		Reason:                   decision.Reason,
		UsesDocker:               decision.Usage.UsesDocker,
		StartsDockerRuntime:      decision.Usage.StartsDockerRuntime,
		UsesDockerCompose:        decision.Usage.Compose.Uses,
		StartsDockerComposeStack: decision.Usage.Compose.StartsStack,
		ExplicitComposeProject:   decision.Usage.Compose.ExplicitProject,
		ReferencesRuntimePorts:   decision.Usage.ReferencesRuntimePorts,
		RuntimeEndpointHints:     append([]string{}, decision.Usage.RuntimeEndpointHints...),
	}
}

func CleanupStageCTestArtifactsForTest(repoPath string) TestStageCTestArtifactCleanup {
	cleanup := cleanupStageCTestArtifacts(repoPath)
	return TestStageCTestArtifactCleanup{
		Removed:  append([]string{}, cleanup.Removed...),
		Warnings: append([]string{}, cleanup.Warnings...),
	}
}

func RuntimeCleanupPointForTest(stage string, stages []model.StageRecord) bool {
	return runtimeCleanupPoint(stage, stages)
}

func BlockedDependentsForTest(stage string) []string {
	return blockedDependents(stage)
}

func BrowserURLCandidatesForTest(evidence TestRuntimeEvidence) []TestBrowserURLCandidate {
	return browserURLCandidates(runtimeEvidenceFromTest(evidence))
}

func BrowserAllowlistOriginsForTest(candidates []TestBrowserURLCandidate) []string {
	return browserAllowlistOrigins(candidates)
}

func BrowserCodexEnvForTest(base []string, configured map[string]string, nodePath string) []string {
	root, err := os.MkdirTemp("", "p2r-browser-codex-env-test-*")
	if err != nil {
		return nil
	}
	defer os.RemoveAll(root)
	sandbox, _ := codex.NewSandbox("/repo", root, "G")
	return browserCodexEnv(sandbox, base, configured, nodePath)
}

func ExtractJSONObjectForTest(raw string) (string, error) {
	return extractJSONObject(raw)
}

func StageGBrowserContextForTest(projectPath string) string {
	return stageGBrowserContext(projectPath)
}

func BrowserActionPromptForTest(templateText, profile, contextText string) (string, error) {
	candidates := []BrowserURLCandidate{{
		ID:      "url_1",
		URL:     "http://127.0.0.1:3000",
		Origin:  "http://127.0.0.1:3000",
		Service: "frontend",
		Source:  "probe",
		ProbeOK: true,
	}}
	sc := StageContext{
		Run:     model.RunRecord{RunID: "run-test", ArtifactRoot: "/tmp/p2r-run"},
		Project: scanner.Project{TaskID: "TASK-TEST", Path: "/tmp/project"},
	}
	return browserActionPrompt(templateText, browserActionPromptDataForStage(sc, profile, contextText, candidates, nil, nil, 1))
}

func ValidateBrowserActionForTest(action TestBrowserAction, candidates []TestBrowserURLCandidate) *TestBlockedBrowserAction {
	validation := validateBrowserAction(action, candidates, "")
	if validation.Blocked == nil {
		return nil
	}
	blocked := *validation.Blocked
	return &blocked
}

func ParseFrontendE2ESummaryForTest(raw []byte) (TestFrontendE2ESummary, error) {
	return parseFrontendE2ESummary(raw)
}

func FrontendE2EObservationFindingsForTest(observations []TestBrowserObservation, includeActionFailures bool) []model.Finding {
	return frontendE2EObservationFindings(observations, "frontend_e2e_screenshot.png", includeActionFailures)
}

func StageGLogObservationForTest(round int, observation TestBrowserObservation) string {
	return stageGLogObservation(round, observation)
}

func StageGFinishedStatusForTest(record model.StageRecord) string {
	return stageGFinishedStatus(record)
}

func StageGFinishScreenshotBlockReasonForTest(observations []TestBrowserObservation) string {
	return stageGFinishScreenshotBlockReason(observations)
}

func StageGFinishScreenshotBlockReasonForSummaryForTest(summary TestFrontendE2ESummary, observations []TestBrowserObservation) string {
	return stageGFinishScreenshotBlockReasonForSummary(summary, observations)
}

func StageGPartialProductBlockerFindingForTest(observations []TestBrowserObservation, reason string) (model.Finding, bool) {
	return stageGPartialProductBlockerFinding(observations, reason)
}

func StageGNativeDialogBoundaryEvidenceForTest(observations []TestBrowserObservation) string {
	return stageGNativeDialogBoundaryEvidence(observations)
}

func StageGPositiveEvidenceOutcomeForTest(candidates []TestBrowserURLCandidate, observations []TestBrowserObservation, blocked []TestBlockedBrowserAction, reason string) (TestFrontendE2ESummary, bool) {
	return stageGPositiveEvidenceOutcome(candidates, observations, blocked, reason)
}

func AppendStageGRepoSnapshotFindingsForTest(record model.StageRecord, summary TestFrontendE2ESummary, repoPath string, before map[string]string) (model.StageRecord, TestFrontendE2ESummary) {
	return appendStageGRepoSnapshotFindings(record, summary, repoPath, before)
}

func StageGObservationStopReasonForTest(observations []TestBrowserObservation) string {
	return stageGObservationStopReason(observations)
}

func StageGAuthGateStallEvidenceForTest(observations []TestBrowserObservation) string {
	return stageGAuthGateStallEvidence(observations)
}

func StageGRepeatedStateStallEvidenceForTest(observations []TestBrowserObservation) string {
	return stageGRepeatedStateStallEvidence(observations)
}

func StageGKeyScreenshotObservationIndexesForTest(observations []TestBrowserObservation) []int {
	return stageGKeyScreenshotObservationIndexes(observations)
}

func MaterializeStageGScreenshotArtifactsForTest(root string, summary TestFrontendE2ESummary, observations []TestBrowserObservation) (TestFrontendE2ESummary, []TestBrowserObservation, model.StageRecord) {
	record := model.StageRecord{Stage: string(model.StageG)}
	writer := NewArtifactWriter(root)
	record, summary, observations = materializeStageGScreenshotArtifacts(record, writer, summary, observations)
	return summary, observations, record
}

func IncludeStageGActionFailureFallbackForTest(summary TestFrontendE2ESummary, summaryFindings []model.Finding) bool {
	return includeStageGActionFailureFallback(summary, summaryFindings)
}

func SnapshotRepoForTest(repoPath string) (map[string]string, error) {
	return snapshotRepo(repoPath)
}

func RepoSnapshotDiffForTest(before, after map[string]string) []string {
	return repoSnapshotDiff(before, after)
}

func WriteStageStatusForTest(runID, artifactRoot string, stages []model.StageRecord) error {
	return Runner{}.writeStageStatus(runID, artifactRoot, stages)
}

func FilteredRuntimeEnvForTest(environ, extra []string, docker bool) []string {
	return filteredRuntimeEnv(environ, extra, docker)
}

func runtimeEvidenceFromTest(evidence TestRuntimeEvidence) runtimeEvidence {
	return runtimeEvidence{
		ComposeProject: evidence.ComposeProject,
		ComposeFile:    evidence.ComposeFile,
		ComposeFiles:   append([]string{}, evidence.ComposeFiles...),
		EnvFiles:       append([]string{}, evidence.EnvFiles...),
		WorkDir:        evidence.WorkDir,
		Services:       append([]string{}, evidence.Services...),
		Mappings:       runtimePortMappingMap(evidence.Mappings),
		Probes:         runtimeProbeResults(evidence.Probes),
		Mirror: dockermgr.RuntimeMirrorState{
			BuildMirrorEnabled:      evidence.Mirror.BuildMirrorEnabled,
			BuildMirrorMode:         evidence.Mirror.BuildMirrorMode,
			BuildMirrorFallbackUsed: evidence.Mirror.BuildMirrorFallbackUsed,
			BuildMirrorSummary:      evidence.Mirror.BuildMirrorSummary,
		},
	}
}

func testStageCProxyPlan(plan stageCProxyPlan) TestStageCProxyPlan {
	mappings := make([]TestStageCProxyMapping, 0, len(plan.Mappings))
	for _, mapping := range plan.Mappings {
		mappings = append(mappings, TestStageCProxyMapping(mapping))
	}
	return TestStageCProxyPlan{
		ComposeProject:  plan.ComposeProject,
		ComposeFiles:    append([]string{}, plan.ComposeFiles...),
		WorkDir:         plan.WorkDir,
		RunnerName:      plan.RunnerName,
		RunnerImage:     plan.RunnerImage,
		ProxyImage:      plan.ProxyImage,
		OverrideFile:    plan.OverrideFile,
		EnvFile:         plan.EnvFile,
		ProxyConfigFile: plan.ProxyConfigFile,
		Mappings:        mappings,
		ServiceURLs:     copyStringMap(plan.ServiceURLs),
		Env:             append([]string{}, plan.Env...),
		OverrideContent: plan.OverrideContent,
		EnvContent:      plan.EnvContent,
	}
}

func testPortMappingMap(values map[string][]portMapping) map[string][]TestPortMapping {
	result := make(map[string][]TestPortMapping, len(values))
	for service, mappings := range values {
		result[service] = testPortMappings(mappings)
	}
	return result
}

func testPortMappings(values []portMapping) []TestPortMapping {
	result := make([]TestPortMapping, 0, len(values))
	for _, value := range values {
		result = append(result, TestPortMapping(value))
	}
	return result
}

func runtimePortMappingMap(values map[string][]TestPortMapping) map[string][]portMapping {
	result := make(map[string][]portMapping, len(values))
	for service, mappings := range values {
		for _, mapping := range mappings {
			result[service] = append(result[service], portMapping(mapping))
		}
	}
	return result
}

func runtimeProbeResults(values []TestProbeResult) []probeResult {
	result := make([]probeResult, 0, len(values))
	for _, value := range values {
		result = append(result, probeResult(value))
	}
	return result
}

func copyStringMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func testServiceURLMap(values map[string]serviceURL) map[string]TestServiceURL {
	result := make(map[string]TestServiceURL, len(values))
	for key, value := range values {
		result[key] = TestServiceURL(value)
	}
	return result
}
