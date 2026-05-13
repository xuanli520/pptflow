package pipeline

import (
	"context"
	"errors"
	"time"

	"github.com/xuanli520/p2r_tui/internal/codex"
	"github.com/xuanli520/p2r_tui/internal/codex/appserver"
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
	WorkDir        string                       `json:"work_dir"`
	Services       []string                     `json:"services"`
	Mappings       map[string][]TestPortMapping `json:"mappings"`
	Probes         []TestProbeResult            `json:"probes"`
}

type TestStageCCommandEnv struct {
	Env     []string
	Keys    []string
	Values  map[string]string
	Service TestServiceURLEnv
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

func SubmitArtifactNamesForTest(mode string) []string {
	return submitArtifactNames(mode)
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
	env := stageCEnvironment(runtimeEvidence{
		ComposeProject: evidence.ComposeProject,
		ComposeFile:    evidence.ComposeFile,
		WorkDir:        evidence.WorkDir,
		Services:       append([]string{}, evidence.Services...),
		Mappings:       runtimePortMappingMap(evidence.Mappings),
		Probes:         runtimeProbeResults(evidence.Probes),
	})
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

func FilteredRuntimeEnvForTest(environ, extra []string, docker bool) []string {
	return filteredRuntimeEnv(environ, extra, docker)
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
