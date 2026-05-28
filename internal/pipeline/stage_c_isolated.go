package pipeline

import (
	"context"
	"encoding/json"
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

	plan, loadedPlan, err := loadStageCProxyPlan(run.ArtifactRoot)
	if err != nil {
		return fail(err.Error(), nil, nil)
	}
	if !loadedPlan {
		plan, err = buildStageCProxyPlan(runtime, repoPath, run.ArtifactRoot, r.cfg.Pipeline.StageC)
	}
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
	if !loadedPlan {
		err = writeStageCProxyPlanArtifacts(writer, plan)
	}
	if err != nil {
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

	runArgs := dockermgr.ComposeCommandArgsWithProjectDir(composeFiles, runtime.WorkDir, runtime.ComposeProject, "--profile", stageCProfileName, "run", "--rm", "-T", "--no-deps", "--name", plan.RunnerName, stageCRunnerService)
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
	if ctx == nil || ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
	}
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
	composeContent := ""
	if content, err := os.ReadFile(filepath.Join(artifactRoot, "docker_compose_stage_c_proxy_config.yml")); err == nil {
		composeContent = string(content)
	}
	return buildStageCProxyPlanWithComposeContent(runtime, repoPath, artifactRoot, cfg, composeFile, composeContent)
}

func buildStageCProxyPlanWithComposeContent(runtime RuntimeState, repoPath, artifactRoot string, cfg config.StageCConfig, composeFile, composeContent string) (stageCProxyPlan, error) {
	mappings, err := stageCProxyMappings(composeFile, composeContent)
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

func loadStageCProxyPlan(artifactRoot string) (stageCProxyPlan, bool, error) {
	path := filepath.Join(artifactRoot, "p2r_stage_c_proxy.json")
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return stageCProxyPlan{}, false, nil
	}
	if err != nil {
		return stageCProxyPlan{}, false, err
	}
	var plan stageCProxyPlan
	if err := json.Unmarshal(content, &plan); err != nil {
		return stageCProxyPlan{}, false, err
	}
	if strings.TrimSpace(plan.OverrideFile) == "" || strings.TrimSpace(plan.EnvFile) == "" {
		return stageCProxyPlan{}, false, nil
	}
	if !fileExists(plan.OverrideFile) || !fileExists(plan.EnvFile) {
		return stageCProxyPlan{}, false, nil
	}
	return plan, true, nil
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

func bestEffortStageCProxyArtifacts(record *model.StageRecord, writer ArtifactWriter, runtime RuntimeState, repoPath, artifactRoot string, cfg config.StageCConfig, composeContent string) {
	composeFile := firstRuntimeComposeFile(runtime.ComposeFiles)
	if composeFile == "" {
		composeFile = runtime.ComposeFile
	}
	plan, err := buildStageCProxyPlanWithComposeContent(runtime, repoPath, artifactRoot, cfg, composeFile, composeContent)
	if err != nil {
		recordArtifactWarning(record, newArtifactWarning("p2r_stage_c_proxy.json", "write_json", false, err))
		return
	}
	if strings.TrimSpace(composeContent) != "" {
		bestEffortStageText(record, writer, "docker_compose_stage_c_proxy_config.yml", composeContent)
	}
	bestEffortStageText(record, writer, writer.RelativePath(plan.EnvFile), plan.EnvContent)
	bestEffortStageText(record, writer, writer.RelativePath(plan.OverrideFile), plan.OverrideContent)
	bestEffortStageJSON(record, writer, writer.RelativePath(plan.ProxyConfigFile), plan)
}

func stageCProxyMappingsFromCompose(composeFile string) ([]stageCProxyMapping, error) {
	return stageCProxyMappings(composeFile, "")
}

func stageCProxyMappings(composeFile, composeContent string) ([]stageCProxyMapping, error) {
	if strings.TrimSpace(composeFile) == "" && strings.TrimSpace(composeContent) == "" {
		return nil, nil
	}
	content := []byte(composeContent)
	if strings.TrimSpace(composeContent) == "" {
		var err error
		content, err = os.ReadFile(composeFile)
		if err != nil {
			return nil, err
		}
	}
	return stageCProxyMappingsFromComposeContent(content)
}

func stageCProxyMappingsFromComposeContent(content []byte) ([]stageCProxyMapping, error) {
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
			for _, mapping := range parseStageCComposePort(serviceName, raw) {
				if mapping.Listen <= 0 || mapping.Target <= 0 {
					continue
				}
				if previous := seen[mapping.Listen]; previous != "" && previous != serviceName {
					return nil, fmt.Errorf("multiple compose services publish localhost proxy port %d: %s and %s", mapping.Listen, previous, serviceName)
				}
				seen[mapping.Listen] = serviceName
				mappings = append(mappings, mapping)
			}
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

func parseStageCComposePort(serviceName string, raw any) []stageCProxyMapping {
	switch value := raw.(type) {
	case string:
		return parseStageCShortPort(serviceName, value)
	case map[string]any:
		protocol := strings.ToLower(strings.TrimSpace(fmt.Sprint(value["protocol"])))
		if protocol == "<nil>" {
			protocol = ""
		}
		if protocol == "" {
			protocol = "tcp"
		}
		if protocol != "tcp" {
			return nil
		}
		return expandStageCPortMappings(serviceName, stageCScalarString(value["published"]), stageCScalarString(value["target"]), protocol)
	default:
		return nil
	}
}

func parseStageCShortPort(serviceName, value string) []stageCProxyMapping {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	protocol := "tcp"
	if slash := strings.LastIndex(value, "/"); slash >= 0 {
		protocol = strings.ToLower(strings.TrimSpace(value[slash+1:]))
		value = strings.TrimSpace(value[:slash])
	}
	if protocol != "tcp" {
		return nil
	}
	colon := strings.LastIndex(value, ":")
	if colon < 0 {
		return nil
	}
	target := strings.TrimSpace(value[colon+1:])
	prefix := strings.TrimSpace(value[:colon])
	if inner := strings.LastIndex(prefix, ":"); inner >= 0 {
		prefix = strings.TrimSpace(prefix[inner+1:])
	}
	return expandStageCPortMappings(serviceName, prefix, target, protocol)
}

func expandStageCPortMappings(serviceName, listenText, targetText, protocol string) []stageCProxyMapping {
	listens := stageCPortValues(listenText)
	targets := stageCPortValues(targetText)
	if len(listens) == 0 || len(targets) == 0 {
		return nil
	}
	if len(listens) != len(targets) {
		if len(listens) == 1 {
			listens = repeatStageCPort(listens[0], len(targets))
		} else if len(targets) == 1 {
			targets = repeatStageCPort(targets[0], len(listens))
		} else {
			return nil
		}
	}
	mappings := make([]stageCProxyMapping, 0, len(listens))
	for index := range listens {
		mappings = append(mappings, stageCProxyMapping{
			Listen:   listens[index],
			Service:  serviceName,
			Target:   targets[index],
			Protocol: protocol,
		})
	}
	return mappings
}

func stageCPortValues(value string) []int {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" {
		return nil
	}
	parts := strings.Split(value, "-")
	if len(parts) == 1 {
		port := atoiStrict(parts[0])
		if port <= 0 {
			return nil
		}
		return []int{port}
	}
	if len(parts) != 2 {
		return nil
	}
	start := atoiStrict(parts[0])
	end := atoiStrict(parts[1])
	if start <= 0 || end < start {
		return nil
	}
	values := make([]int, 0, end-start+1)
	for port := start; port <= end; port++ {
		values = append(values, port)
	}
	return values
}

func repeatStageCPort(port, count int) []int {
	values := make([]int, count)
	for index := range values {
		values[index] = port
	}
	return values
}

func stageCScalarString(value any) string {
	switch typed := value.(type) {
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case uint64:
		return strconv.FormatUint(typed, 10)
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strings.TrimSpace(strconv.FormatFloat(typed, 'f', -1, 64))
	case string:
		return strings.TrimSpace(typed)
	default:
		return ""
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
				"command":     stageCRunnerCommand(),
			},
		},
	}
	content, err := yaml.Marshal(override)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func stageCRunnerCommand() []string {
	script := strings.Join([]string{
		"set -eu",
		"script=run_tests.sh",
		`first=$(head -n 1 "$script" 2>/dev/null | tr -d '\r' || true)`,
		`case "$first" in`,
		`  '#!'*) interpreter=${first#\#!}; set -- $interpreter "$script"; exec "$@" ;;`,
		`  *) exec /bin/sh "$script" ;;`,
		`esac`,
	}, "\n")
	return []string{"/bin/sh", "-lc", escapeComposeInterpolation(script)}
}

func escapeComposeInterpolation(value string) string {
	return strings.ReplaceAll(value, "$", "$$")
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
