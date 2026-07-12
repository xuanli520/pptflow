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
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/packager"
	"github.com/purplevoid/harbor-factory/internal/harbor/sanitize"
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

	stateMu           sync.Mutex
	runID             string
	stageMu           sync.Mutex
	stageCancels      map[string]context.CancelFunc
	stageCancelQueued map[string]bool
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
	return &Runner{opts: opts, events: make(chan domain.RunnerEvent, 64), decisions: make(chan domain.GateDecision, 8), stageCancels: map[string]context.CancelFunc{}, stageCancelQueued: map[string]bool{}}
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
	if cancel == nil {
		r.stageCancelQueued[nodeID] = true
	}
	r.stageMu.Unlock()
	if cancel == nil {
		return true
	}
	cancel()
	return true
}

func (r *Runner) registerStageCancel(nodeID string, cancel context.CancelFunc) func() {
	r.stageMu.Lock()
	r.stageCancels[nodeID] = cancel
	queued := r.stageCancelQueued[nodeID]
	delete(r.stageCancelQueued, nodeID)
	r.stageMu.Unlock()
	if queued {
		cancel()
	}
	return func() {
		r.stageMu.Lock()
		delete(r.stageCancels, nodeID)
		r.stageMu.Unlock()
	}
}

func (r *Runner) Run(ctx context.Context) (domain.RunSummary, error) {
	return r.runWithEngine(ctx)
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

func (r *Runner) ensureRunID() string {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.runID == "" {
		r.runID = fmt.Sprintf("run-%d", time.Now().UTC().UnixNano())
	}
	return r.runID
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

func (r *Runner) snapshot() []domain.RunnerEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.RunnerEvent(nil), r.log...)
}
