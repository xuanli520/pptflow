package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
	"gopkg.in/yaml.v3"
)

const (
	stageCProfileName   = "p2r-stage-c"
	stageCProxyService  = "p2r_stage_c_proxy"
	stageCRunnerService = "p2r_stage_c_runner"
)

type stageCProxyMapping struct {
	Listen   int    `json:"listen"`
	Service  string `json:"service"`
	Target   int    `json:"target"`
	Protocol string `json:"protocol,omitempty"`
}

type stageCProxyPlan struct {
	ComposeProject  string               `json:"compose_project"`
	ComposeFiles    []string             `json:"compose_files"`
	WorkDir         string               `json:"work_dir"`
	RunnerName      string               `json:"runner_name"`
	RunnerImage     string               `json:"runner_image,omitempty"`
	ProxyImage      string               `json:"proxy_image"`
	OverrideFile    string               `json:"override_file"`
	EnvFile         string               `json:"env_file"`
	ProxyConfigFile string               `json:"proxy_config_file"`
	Mappings        []stageCProxyMapping `json:"mappings"`
	ServiceURLs     map[string]string    `json:"service_urls,omitempty"`
	Env             []string             `json:"env"`
	OverrideContent string               `json:"-"`
	EnvContent      string               `json:"-"`
}

func (r Runner) stageCIsolated(ctx context.Context, run model.RunRecord, project scanner.Project, runtime RuntimeState, prior map[string]model.StageRecord, progress func(RunProgress)) model.StageRecord {
	runtime.Normalize()
	start := time.Now()
	record := startStage("C")
	logPath := filepath.Join(run.ArtifactRoot, "logs", "C_tests.log")
	screenshotPath := qaArtifactPath(run.ArtifactRoot, "run_tests_screenshot.png")
	summaryPath := filepath.Join(run.ArtifactRoot, "test_runtime_summary.json")
	proxyPath := filepath.Join(run.ArtifactRoot, "p2r_stage_c_proxy.json")
	envPath := filepath.Join(run.ArtifactRoot, "p2r_ports.env")
	overridePath := filepath.Join(run.ArtifactRoot, "stage_c.runner.override.yml")
	record.LogPath = logPath
	record.ArtifactPaths = append(record.ArtifactPaths, logPath, screenshotPath, summaryPath, proxyPath, envPath, overridePath)
	writer := NewArtifactWriter(run.ArtifactRoot)
	repoPath := filepath.Join(project.Path, "repo")

	fail := func(reason string, finding *model.Finding, extra map[string]any) model.StageRecord {
		if extra == nil {
			extra = map[string]any{}
		}
		extra["mode"] = "isolated"
		if fileExists(logPath) {
			bestEffortStageAppend(&record, writer, writer.RelativePath(logPath), "\nERROR SUMMARY:\n"+reason+"\n")
		} else {
			record = requiredStageText(record, writer, writer.RelativePath(logPath), reason)
		}
		pages, err := renderLogFile(logPath, screenshotPath)
		if err != nil {
			record = recordArtifactWriteError(record, err, screenshotPath)
		}
		record.ArtifactPaths = append([]string{logPath}, pages...)
		record.ArtifactPaths = append(record.ArtifactPaths, summaryPath, proxyPath, envPath, overridePath)
		record = requiredStageJSON(record, writer, writer.RelativePath(summaryPath), stageCRuntimeSummary(false, reason, runtime, prior, extra))
		if finding != nil {
			record.Findings = append(record.Findings, *finding)
		}
		if record.ErrorSummary == "" {
			record.ErrorSummary = reason
		}
		return finishStage(record, model.StageFailed, start)
	}

	runnerImage := strings.TrimSpace(r.cfg.Pipeline.StageC.RunnerImage)
	if runnerImage == "" {
		reason := "pipeline.stage_c.runner_image is required when Stage C execution is isolated."
		return fail(reason, &model.Finding{
			Stage:      "C",
			Severity:   "High",
			Title:      "Stage C isolated runner image is not configured",
			Rule:       "Isolated Stage C requires pipeline.stage_c.runner_image.",
			Evidence:   reason,
			Impact:     "Runtime test evidence cannot be collected in the compose network.",
			MinimumFix: "Configure pipeline.stage_c.runner_image with an image that can run repo/run_tests.sh.",
		}, nil)
	}

	plan, err := buildStageCProxyPlan(runtime, repoPath, run.ArtifactRoot, r.cfg.Pipeline.StageC)
	if err != nil {
		reason := "Stage C isolated proxy plan failed: " + err.Error()
		return fail(reason, &model.Finding{
			Stage:      "C",
			Severity:   "High",
			Title:      "Stage C isolated proxy plan failed",
			Rule:       "Stage C isolated mode requires readable compose port metadata.",
			Evidence:   reason,
			Impact:     "Runtime test evidence cannot be collected in the compose network.",
			MinimumFix: "Fix compose ports or rerun Stage B.",
		}, nil)
	}
	if err := writeStageCProxyPlanArtifacts(writer, plan); err != nil {
		return fail(err.Error(), nil, map[string]any{"proxy_plan": plan})
	}
	if unmapped := unmappedLocalhostPorts(filepath.Join(repoPath, "run_tests.sh"), plan.Mappings); len(unmapped) > 0 && r.cfg.Pipeline.StageC.FailOnUnmappedLocalhost {
		ports := make([]string, 0, len(unmapped))
		for _, port := range unmapped {
			ports = append(ports, "localhost:"+strconv.Itoa(port))
		}
		reason := "run_tests.sh references unmapped localhost ports: " + strings.Join(ports, ", ")
		return fail(reason, &model.Finding{
			Stage:      "C",
			Severity:   "High",
			Title:      "run_tests.sh references unmapped localhost ports",
			Rule:       "Isolated Stage C can proxy only localhost ports declared by compose service ports.",
			Evidence:   reason,
			Impact:     "Hardcoded localhost calls would miss the QA compose service network.",
			MinimumFix: "Expose the target service in compose ports or update run_tests.sh to use injected service URL variables.",
		}, map[string]any{"proxy_plan": plan})
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		record = recordArtifactWriteError(record, err, logPath)
		return finishStage(record, model.StageFailed, start)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		record = recordArtifactWriteError(record, err, logPath)
		return finishStage(record, model.StageFailed, start)
	}
	defer logFile.Close()
	fmt.Fprintln(logFile, "=== C isolated run_tests.sh start ===")
	appendStreamProgress(run.RunID, "C", "=== C isolated run_tests.sh start ===", "p2r", false, progress)
	for _, item := range plan.Env {
		fmt.Fprintln(logFile, item)
		appendStreamProgress(run.RunID, "C", item, "p2r", false, progress)
	}

	timeout := r.stageTimeout("C", 300)
	composeFiles := append(append([]string{}, plan.ComposeFiles...), plan.OverrideFile)
	upArgs := dockermgr.ComposeCommandArgsWithProjectDir(composeFiles, runtime.WorkDir, runtime.ComposeProject, "--profile", stageCProfileName, "up", "-d", stageCProxyService)
	up := r.exec.RunStreamingWithOutput(ctx, timeout, runtime.WorkDir, dockerCommandEnv(), logFile, stageCOutput(run.RunID, progress), "docker", upArgs...)
	if up.Err != nil {
		cleanup := r.cleanupStageCIsolated(ctx, runtime, composeFiles, plan.RunnerName, logFile)
		reason := "Stage C proxy startup failed: " + stageCTrimResult(up)
		return fail(reason, &model.Finding{
			Stage:      "C",
			Severity:   "High",
			Title:      "Stage C proxy startup failed",
			Rule:       "Isolated Stage C must start the localhost proxy before running tests.",
			Evidence:   strings.TrimSpace(up.Stderr),
			Impact:     "Runtime test evidence cannot be collected.",
			MinimumFix: "Inspect p2r_stage_c_proxy logs and fix compose networking.",
		}, map[string]any{"proxy_plan": plan, "cleanup": cleanup, "command": up.Command, "exit_code": up.ExitCode, "timeout": up.Timeout})
	}

	runArgs := dockermgr.ComposeCommandArgsWithProjectDir(composeFiles, runtime.WorkDir, runtime.ComposeProject, "--profile", stageCProfileName, "run", "--rm", "--no-deps", "--name", plan.RunnerName, stageCRunnerService)
	result := r.exec.RunStreamingWithOutput(ctx, timeout, runtime.WorkDir, dockerCommandEnv(), logFile, stageCOutput(run.RunID, progress), "docker", runArgs...)
	cleanup := r.cleanupStageCIsolated(ctx, runtime, composeFiles, plan.RunnerName, logFile)
	endLine := fmt.Sprintf("=== C isolated run_tests.sh end: exit=%d timeout=%t err=%v ===", result.ExitCode, result.Timeout, result.Err)
	fmt.Fprintf(logFile, "\n%s\n", endLine)
	appendStreamProgress(run.RunID, "C", endLine, "p2r", true, progress)
	pages, renderErr := renderLogFile(logPath, screenshotPath)
	if renderErr != nil {
		record = recordArtifactWriteError(record, renderErr, screenshotPath)
	}
	record.ArtifactPaths = append([]string{logPath}, pages...)
	record.ArtifactPaths = append(record.ArtifactPaths, summaryPath, proxyPath, envPath, overridePath)
	extra := map[string]any{
		"mode":       "isolated",
		"exit_code":  result.ExitCode,
		"timeout":    result.Timeout,
		"command":    "docker compose run " + stageCRunnerService,
		"proxy_plan": plan,
		"cleanup":    cleanup,
	}
	record = requiredStageJSON(record, writer, writer.RelativePath(summaryPath), stageCRuntimeSummary(result.Err == nil, "", runtime, prior, extra))
	if result.Err != nil {
		record.Findings = append(record.Findings, model.Finding{
			Stage:      "C",
			Severity:   "High",
			Title:      "run_tests runtime evidence failed",
			Rule:       "Stage C must execute the unified test entrypoint successfully.",
			Evidence:   strings.TrimSpace(result.Stderr),
			Impact:     "The delivery package does not currently have passing runtime test evidence.",
			MinimumFix: "Fix the test entrypoint or application runtime and rerun C.",
		})
		if record.ErrorSummary == "" {
			record.ErrorSummary = "run_tests failed"
		}
		return finishStage(record, model.StageFailed, start)
	}
	return finishStage(record, model.StageDone, start)
}

func stageCOutput(runID string, progress func(RunProgress)) func(string, string) {
	return func(line string, source string) {
		appendStreamProgress(runID, "C", line, source, false, progress)
	}
}

type stageCIsolatedCleanup struct {
	RunnerRemoved bool     `json:"runner_removed"`
	ProxyRemoved  bool     `json:"proxy_removed"`
	Commands      []string `json:"commands,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

func (r Runner) cleanupStageCIsolated(ctx context.Context, runtime RuntimeState, composeFiles []string, runnerName string, logFile *os.File) stageCIsolatedCleanup {
	var cleanup stageCIsolatedCleanup
	rmRunner := r.exec.Run(ctx, 30*time.Second, runtime.WorkDir, dockerCommandEnv(), "docker", "rm", "-f", runnerName)
	cleanup.Commands = append(cleanup.Commands, rmRunner.Command)
	cleanup.RunnerRemoved = rmRunner.Err == nil
	if rmRunner.Err != nil {
		cleanup.Warnings = append(cleanup.Warnings, "runner cleanup failed: "+stageCTrimResult(rmRunner))
	}
	rmProxyArgs := dockermgr.ComposeCommandArgsWithProjectDir(composeFiles, runtime.WorkDir, runtime.ComposeProject, "--profile", stageCProfileName, "rm", "-sf", stageCProxyService)
	rmProxy := r.exec.Run(ctx, 30*time.Second, runtime.WorkDir, dockerCommandEnv(), "docker", rmProxyArgs...)
	cleanup.Commands = append(cleanup.Commands, rmProxy.Command)
	cleanup.ProxyRemoved = rmProxy.Err == nil
	if rmProxy.Err != nil {
		cleanup.Warnings = append(cleanup.Warnings, "proxy cleanup failed: "+stageCTrimResult(rmProxy))
	}
	if logFile != nil {
		for _, warning := range cleanup.Warnings {
			fmt.Fprintln(logFile, "Stage C isolated cleanup warning: "+warning)
		}
	}
	return cleanup
}

func buildStageCProxyPlan(runtime RuntimeState, repoPath, artifactRoot string, cfg config.StageCConfig) (stageCProxyPlan, error) {
	runtime.Normalize()
	composeFile := firstRuntimeComposeFile(runtime.ComposeFiles)
	if composeFile == "" {
		composeFile = runtime.ComposeFile
	}
	mappings, err := stageCProxyMappingsFromCompose(composeFile)
	if err != nil {
		return stageCProxyPlan{}, err
	}
	proxyImage := strings.TrimSpace(cfg.ProxyImage)
	if proxyImage == "" {
		proxyImage = "alpine/socat:latest"
	}
	plan := stageCProxyPlan{
		ComposeProject:  runtime.ComposeProject,
		ComposeFiles:    append([]string{}, runtime.ComposeFiles...),
		WorkDir:         runtime.WorkDir,
		RunnerName:      stageCRunnerName(runtime.ComposeProject),
		RunnerImage:     strings.TrimSpace(cfg.RunnerImage),
		ProxyImage:      proxyImage,
		OverrideFile:    filepath.Join(artifactRoot, "stage_c.runner.override.yml"),
		EnvFile:         filepath.Join(artifactRoot, "p2r_ports.env"),
		ProxyConfigFile: filepath.Join(artifactRoot, "p2r_stage_c_proxy.json"),
		Mappings:        mappings,
		ServiceURLs:     isolatedServiceURLs(runtime, mappings),
	}
	plan.EnvContent, plan.Env = stageCProxyEnv(plan, runtime)
	content, err := stageCProxyOverride(plan, repoPath, artifactRoot)
	if err != nil {
		return stageCProxyPlan{}, err
	}
	plan.OverrideContent = content
	return plan, nil
}

func stageCTrimResult(result executor.Result) string {
	value := strings.TrimSpace(result.Stderr)
	if value == "" {
		value = strings.TrimSpace(result.Stdout)
	}
	if value == "" && result.Err != nil {
		value = result.Err.Error()
	}
	if value == "" {
		value = result.Command
	}
	return value
}

func firstRuntimeComposeFile(files []string) string {
	for _, file := range files {
		if strings.TrimSpace(file) != "" {
			return file
		}
	}
	return ""
}

func writeStageCProxyPlanArtifacts(writer ArtifactWriter, plan stageCProxyPlan) error {
	if err := writer.RequiredText(writer.RelativePath(plan.OverrideFile), plan.OverrideContent); err != nil {
		return err
	}
	if err := writer.RequiredText(writer.RelativePath(plan.EnvFile), plan.EnvContent); err != nil {
		return err
	}
	if err := writer.RequiredJSON(writer.RelativePath(plan.ProxyConfigFile), plan); err != nil {
		return err
	}
	return nil
}

func bestEffortStageCProxyArtifacts(record *model.StageRecord, writer ArtifactWriter, runtime RuntimeState, repoPath, artifactRoot string, cfg config.StageCConfig) {
	plan, err := buildStageCProxyPlan(runtime, repoPath, artifactRoot, cfg)
	if err != nil {
		recordArtifactWarning(record, newArtifactWarning("p2r_stage_c_proxy.json", "write_json", false, err))
		return
	}
	bestEffortStageText(record, writer, writer.RelativePath(plan.EnvFile), plan.EnvContent)
	bestEffortStageJSON(record, writer, writer.RelativePath(plan.ProxyConfigFile), plan)
}

func stageCProxyMappingsFromCompose(composeFile string) ([]stageCProxyMapping, error) {
	if strings.TrimSpace(composeFile) == "" {
		return nil, nil
	}
	content, err := os.ReadFile(composeFile)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := yaml.Unmarshal(content, &payload); err != nil {
		return nil, err
	}
	services, _ := payload["services"].(map[string]any)
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	var mappings []stageCProxyMapping
	seen := map[int]string{}
	for _, serviceName := range names {
		service, _ := services[serviceName].(map[string]any)
		ports, _ := service["ports"].([]any)
		for _, raw := range ports {
			listen, target, protocol, ok := parseStageCComposePort(raw)
			if !ok || listen <= 0 || target <= 0 {
				continue
			}
			if previous := seen[listen]; previous != "" && previous != serviceName {
				return nil, fmt.Errorf("multiple compose services publish localhost proxy port %d: %s and %s", listen, previous, serviceName)
			}
			seen[listen] = serviceName
			mappings = append(mappings, stageCProxyMapping{
				Listen:   listen,
				Service:  serviceName,
				Target:   target,
				Protocol: protocol,
			})
		}
	}
	sort.SliceStable(mappings, func(i, j int) bool {
		if mappings[i].Listen != mappings[j].Listen {
			return mappings[i].Listen < mappings[j].Listen
		}
		if mappings[i].Service != mappings[j].Service {
			return mappings[i].Service < mappings[j].Service
		}
		return mappings[i].Target < mappings[j].Target
	})
	return mappings, nil
}

func parseStageCComposePort(raw any) (listen, target int, protocol string, ok bool) {
	switch value := raw.(type) {
	case string:
		return parseStageCShortPort(value)
	case map[string]any:
		target = intScalar(value["target"])
		listen = intScalar(value["published"])
		protocol = strings.ToLower(strings.TrimSpace(fmt.Sprint(value["protocol"])))
		if protocol == "<nil>" {
			protocol = ""
		}
		if protocol == "" {
			protocol = "tcp"
		}
		if protocol != "tcp" {
			return 0, 0, protocol, false
		}
		return listen, target, protocol, listen > 0 && target > 0
	default:
		return 0, 0, "", false
	}
}

func parseStageCShortPort(value string) (listen, target int, protocol string, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, 0, "", false
	}
	protocol = "tcp"
	if slash := strings.LastIndex(value, "/"); slash >= 0 {
		protocol = strings.ToLower(strings.TrimSpace(value[slash+1:]))
		value = strings.TrimSpace(value[:slash])
	}
	if protocol != "tcp" {
		return 0, 0, protocol, false
	}
	colon := strings.LastIndex(value, ":")
	if colon < 0 {
		return 0, 0, protocol, false
	}
	target = atoiStrict(value[colon+1:])
	prefix := strings.TrimSpace(value[:colon])
	if inner := strings.LastIndex(prefix, ":"); inner >= 0 {
		prefix = strings.TrimSpace(prefix[inner+1:])
	}
	listen = atoiStrict(prefix)
	return listen, target, protocol, listen > 0 && target > 0
}

func intScalar(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case uint64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		return atoiStrict(typed)
	default:
		return 0
	}
}

func atoiStrict(value string) int {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "-") {
		return 0
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return n
}

func isolatedServiceURLs(runtime RuntimeState, mappings []stageCProxyMapping) map[string]string {
	urls := map[string]string{}
	for _, mapping := range mappings {
		if urls[mapping.Service] == "" {
			urls[mapping.Service] = serviceTargetURL(mapping.Service, mapping.Target)
		}
	}
	names := append([]string{}, runtime.Services...)
	if len(names) == 0 {
		for service := range runtime.Mappings {
			names = append(names, service)
		}
		sort.Strings(names)
	}
	for _, service := range names {
		if urls[service] != "" {
			continue
		}
		for _, mapping := range runtime.Mappings[service] {
			if mapping.Container > 0 {
				urls[service] = serviceTargetURL(service, mapping.Container)
				break
			}
		}
	}
	return urls
}

func serviceTargetURL(service string, port int) string {
	scheme := "http"
	if port == 443 {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, service, port)
}

func stageCProxyEnv(plan stageCProxyPlan, runtime RuntimeState) (string, []string) {
	values := map[string]string{}
	if runtime.ComposeProject != "" {
		values["COMPOSE_PROJECT_NAME"] = runtime.ComposeProject
	}
	if len(runtime.ComposeFiles) > 0 {
		values["COMPOSE_FILE"] = strings.Join(runtime.ComposeFiles, string(os.PathListSeparator))
	}
	values["P2R_PORTS_ENV"] = "/p2r-artifacts/p2r_ports.env"
	for service, url := range plan.ServiceURLs {
		values[sanitizeEnvKey(service)+"_URL"] = url
	}
	for _, mapping := range plan.Mappings {
		values["P2R_"+sanitizeEnvKey(mapping.Service)+"_LOCALHOST_URL"] = fmt.Sprintf("http://localhost:%d", mapping.Listen)
		values[fmt.Sprintf("P2R_LOCALHOST_%d_URL", mapping.Listen)] = fmt.Sprintf("http://localhost:%d", mapping.Listen)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return strings.Join(env, "\n") + "\n", env
}

func stageCProxyOverride(plan stageCProxyPlan, repoPath, artifactRoot string) (string, error) {
	proxyScript := "sleep infinity"
	if len(plan.Mappings) > 0 {
		lines := []string{"set -eu"}
		for _, mapping := range plan.Mappings {
			lines = append(lines, fmt.Sprintf("socat TCP-LISTEN:%d,fork,reuseaddr,bind=127.0.0.1 TCP:%s:%d &", mapping.Listen, shellSingleQuote(mapping.Service), mapping.Target))
		}
		lines = append(lines, "wait")
		proxyScript = strings.Join(lines, "\n")
	}
	override := map[string]any{
		"services": map[string]any{
			stageCProxyService: map[string]any{
				"image":    plan.ProxyImage,
				"profiles": []string{stageCProfileName},
				"command":  []string{"/bin/sh", "-lc", proxyScript},
			},
			stageCRunnerService: map[string]any{
				"image":        plan.RunnerImage,
				"profiles":     []string{stageCProfileName},
				"network_mode": "service:" + stageCProxyService,
				"working_dir":  "/workspace",
				"volumes": []string{
					filepath.Clean(repoPath) + ":/workspace",
					filepath.Clean(artifactRoot) + ":/p2r-artifacts",
				},
				"environment": plan.Env,
				"command":     []string{"bash", "run_tests.sh"},
			},
		},
	}
	content, err := yaml.Marshal(override)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func unmappedLocalhostPorts(scriptPath string, mappings []stageCProxyMapping) []int {
	content, err := os.ReadFile(scriptPath)
	if err != nil {
		return nil
	}
	allowed := map[int]bool{}
	for _, mapping := range mappings {
		allowed[mapping.Listen] = true
	}
	re := regexp.MustCompile(`(?i)(?:localhost|127\.0\.0\.1):([0-9]{2,5})`)
	seen := map[int]bool{}
	var ports []int
	for _, match := range re.FindAllStringSubmatch(string(content), -1) {
		port := atoiStrict(match[1])
		if port <= 0 || allowed[port] || seen[port] {
			continue
		}
		seen[port] = true
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

func stageCRunnerName(composeProject string) string {
	value := strings.ToLower(strings.TrimSpace(composeProject))
	value = regexp.MustCompile(`[^a-z0-9_-]+`).ReplaceAllString(value, "_")
	value = strings.Trim(value, "_-")
	if value == "" {
		value = "run"
	}
	value = "p2r_stage_c_" + value
	if len(value) <= 63 {
		return value
	}
	return strings.TrimRight(value[:63], "_-")
}
