package pipeline

import (
	"context"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/codex"
	"github.com/xuanli520/p2r_tui/internal/executor"
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

type TestAppServerSessionProbe struct {
	session *appServerCodexReviewSession
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

func SafeCodexExtraArgsForTest(args []string) ([]string, error) {
	return safeCodexExtraArgs(args)
}

func CapabilitySummaryForTest(capability codex.Capability) string {
	return capabilitySummary(capability)
}

func RunCodexReviewSessionWithGuidanceForTest(ctx context.Context, session CodexReviewSession, request CodexReviewRequest, deadlines []CodexGuidanceDeadline) (CodexReviewResult, error) {
	return runCodexReviewSessionWithGuidance(ctx, session, request, deadlines)
}

func CodexGuidanceScheduleForTest(timeout time.Duration, stage string) []CodexGuidanceDeadline {
	return codexGuidanceSchedule(timeout, stage)
}

func NewAppServerCodexReviewSessionForTest(envKeys []string) CodexReviewSession {
	return newAppServerCodexReviewSession(envKeys)
}

func NewAppServerSessionProbeForTest(logPath, turnID string, maxOutputBytes int, onDelta func(CodexDeltaUpdate)) *TestAppServerSessionProbe {
	return newAppServerSessionProbeForTest(CodexReviewRequest{
		LogPath:        logPath,
		MaxOutputBytes: maxOutputBytes,
		OnDelta:        onDelta,
	}, turnID, nil)
}

func NewAppServerSessionProbeWithProcessContextForTest(logPath string, processCtx context.Context) *TestAppServerSessionProbe {
	return newAppServerSessionProbeForTest(CodexReviewRequest{LogPath: logPath}, "", processCtx)
}

func FormatAggregatedDeltaLogLineForTest(turnID, itemID, text string) string {
	return formatAggregatedDeltaLogLine(aggregatedDeltaLog{turnID: turnID, itemID: itemID, text: text})
}

func newAppServerSessionProbeForTest(req CodexReviewRequest, turnID string, processCtx context.Context) *TestAppServerSessionProbe {
	return &TestAppServerSessionProbe{session: &appServerCodexReviewSession{
		req:                   req,
		processCtx:            processCtx,
		done:                  make(chan struct{}),
		responses:             map[int]chan appServerRPCMessage{},
		turnID:                turnID,
		items:                 map[string]string{},
		deltas:                map[string]string{},
		deltaLogged:           map[string]bool{},
		deltaPreview:          map[string]string{},
		deltaPreviewTruncated: map[string]bool{},
		itemDone:              map[string]bool{},
	}}
}

func (p *TestAppServerSessionProbe) Complete(command string, err error) {
	if p == nil || p.session == nil {
		return
	}
	p.session.complete(executor.Result{Command: command, Err: err}, err)
}

func (p *TestAppServerSessionProbe) CompleteStreamError(stream string, err error) {
	if p == nil || p.session == nil {
		return
	}
	p.session.completeStreamError(stream, err)
}

func (p *TestAppServerSessionProbe) ReadStdout(stream string) {
	if p == nil || p.session == nil {
		return
	}
	p.session.readStdout(strings.NewReader(stream))
}

func (p *TestAppServerSessionProbe) RecordDelta(turnID, itemID, delta string) {
	if p == nil || p.session == nil {
		return
	}
	p.session.recordDelta(turnID, itemID, delta)
}

func (p *TestAppServerSessionProbe) RecordCompletedItem(turnID, itemID, text string) {
	if p == nil || p.session == nil {
		return
	}
	p.session.recordCompletedItem(turnID, itemID, text)
}

func (p *TestAppServerSessionProbe) ResultStdout() string {
	if p == nil || p.session == nil {
		return ""
	}
	p.session.mu.Lock()
	defer p.session.mu.Unlock()
	return p.session.result.Result.Stdout
}

func (p *TestAppServerSessionProbe) Err() error {
	if p == nil || p.session == nil {
		return nil
	}
	p.session.mu.Lock()
	defer p.session.mu.Unlock()
	return p.session.err
}

func (p *TestAppServerSessionProbe) Completed() bool {
	if p == nil || p.session == nil {
		return false
	}
	p.session.mu.Lock()
	defer p.session.mu.Unlock()
	return p.session.completed
}

func (p *TestAppServerSessionProbe) DoneClosed() bool {
	if p == nil || p.session == nil || p.session.done == nil {
		return false
	}
	select {
	case <-p.session.done:
		return true
	default:
		return false
	}
}

func (p *TestAppServerSessionProbe) FinalReport() string {
	if p == nil || p.session == nil {
		return ""
	}
	p.session.mu.Lock()
	defer p.session.mu.Unlock()
	return p.session.finalReportLocked()
}

func (p *TestAppServerSessionProbe) DeltaForItem(itemID string) string {
	if p == nil || p.session == nil {
		return ""
	}
	p.session.mu.Lock()
	defer p.session.mu.Unlock()
	return p.session.deltas[itemID]
}

func (p *TestAppServerSessionProbe) ItemOrderLen() int {
	if p == nil || p.session == nil {
		return 0
	}
	p.session.mu.Lock()
	defer p.session.mu.Unlock()
	return len(p.session.itemOrder)
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
