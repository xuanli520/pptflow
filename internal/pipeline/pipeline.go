package pipeline

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/assets"
	"github.com/xuanli520/p2r_tui/internal/codex"
	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

type RunOptions struct {
	Stage      string
	From       string
	StaticOnly bool
	Stages     []string
}

type Result struct {
	Run    model.RunRecord
	Stages []model.StageRecord
}

type Runner struct {
	store *db.Store
	cfg   config.Config
	exec  executor.Runner
}

func NewRunner(store *db.Store, cfg config.Config) Runner {
	return Runner{store: store, cfg: cfg, exec: executor.New()}
}

func (r Runner) Run(ctx context.Context, taskID string, opts RunOptions) (Result, error) {
	project, err := r.store.GetProject(ctx, taskID)
	if err != nil {
		return Result{}, db.FormatNotFound("task", taskID)
	}
	start := time.Now().UTC()
	runID := fmt.Sprintf("run-%s-%d", start.Format("20060102-150405"), start.UnixNano()%1000000)
	artifactRoot := filepath.Join(project.Path, "qa", "runs", runID)
	if err := os.MkdirAll(filepath.Join(artifactRoot, "logs"), 0o755); err != nil {
		return Result{}, err
	}
	controlDir := filepath.Join(r.cfg.ScanPath, ".qa-control")
	released, releaseErr := assets.Release(controlDir)
	toolVersions, _ := json.Marshal(map[string]any{"assets": released})
	if releaseErr != nil {
		toolVersions, _ = json.Marshal(map[string]any{"assets_error": releaseErr.Error()})
	}
	staticOnly := opts.StaticOnly || r.cfg.Pipeline.StaticOnly
	run := model.RunRecord{
		RunID:          runID,
		TaskID:         taskID,
		StartedAt:      start.Format(time.RFC3339),
		Status:         model.RunRunning,
		ManualVerdict:  model.ManualUnset,
		StaticOnly:     staticOnly,
		ArtifactRoot:   artifactRoot,
		ToolVersions:   string(toolVersions),
		PromptVersions: string(toolVersions),
	}
	if err := r.store.CreateRun(ctx, run); err != nil {
		return Result{}, err
	}
	stages := initialStages(selectedStages(opts, staticOnly), staticOnly)
	if err := r.writeRunManifest(run, project, opts, released, releaseErr); err != nil {
		return Result{}, err
	}
	if err := r.writeStageStatus(runID, artifactRoot, stages); err != nil {
		return Result{}, err
	}

	results := map[string]model.StageRecord{}
	for index := range stages {
		stage := stages[index].Stage
		if stages[index].Status == model.StageSkipped {
			record := r.materializeSkippedStage(run, stages[index])
			stages[index] = record
			results[stage] = record
			_ = r.store.PutStage(ctx, runID, record)
			_ = r.writeStageStatus(runID, artifactRoot, stages)
			continue
		}
		record := r.executeStage(ctx, run, project, stage, results)
		if stage != "F" {
			record.Findings = assignFindingIDs(stage, record.Findings)
		}
		stages[index] = record
		results[stage] = record
		_ = r.store.PutStage(ctx, runID, record)
		_ = r.store.InsertFindings(ctx, runID, record.Findings)
		_ = r.writeStageStatus(runID, artifactRoot, stages)
	}

	status := runStatus(stages)
	if err := r.store.FinishRun(ctx, runID, taskID, status, time.Since(start)); err != nil {
		return Result{}, err
	}
	run.Status = status
	run.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	run.DurationMS = time.Since(start).Milliseconds()
	return Result{Run: run, Stages: stages}, nil
}

func (r Runner) executeStage(ctx context.Context, run model.RunRecord, project scanner.Project, stage string, prior map[string]model.StageRecord) model.StageRecord {
	switch stage {
	case "A":
		return r.stageA(run, project)
	case "B":
		if prior["A"].Status == model.StageFailed {
			return r.materializeSkippedStage(run, blockedStage("B", stageName("B"), []string{"A"}, "Stage A failed; runtime evidence is blocked."))
		}
		return r.stageB(ctx, run, project)
	case "C":
		if prior["B"].Status != model.StageDone {
			return r.materializeSkippedStage(run, blockedStage("C", stageName("C"), []string{"B"}, "Stage C requires successful Docker/runtime evidence from B."))
		}
		return r.stageC(ctx, run, project)
	case "D":
		return r.stageCodex(ctx, run, project, "D", "tests_coverage_report.md", "tests_coverage_report.md", "4_测试有效性报告_api端点真实性.md")
	case "E":
		return r.stageCodex(ctx, run, project, "E", "static_acceptance_audit.md", "static_acceptance_audit_report.md", "1_质检AI测试报告.md")
	case "F":
		return r.stageF(run, prior)
	default:
		return skippedStage(stage, "Unknown stage.")
	}
}

func selectedStages(opts RunOptions, staticOnly bool) map[string]bool {
	selected := map[string]bool{"A": true, "B": true, "C": true, "D": true, "E": true, "F": true}
	if len(opts.Stages) > 0 {
		selected = map[string]bool{"F": true}
		for _, stage := range opts.Stages {
			selected[strings.ToUpper(stage)] = true
		}
		return selected
	}
	if staticOnly {
		return map[string]bool{"A": true, "D": true, "E": true, "F": true}
	}
	if opts.Stage != "" {
		stage := strings.ToUpper(opts.Stage)
		selected := map[string]bool{stage: true}
		if stage != "F" {
			selected["F"] = true
		}
		return selected
	}
	if opts.From != "" {
		selected = map[string]bool{}
		include := false
		for _, stage := range []string{"A", "B", "C", "D", "E", "F"} {
			if stage == strings.ToUpper(opts.From) {
				include = true
			}
			if include {
				selected[stage] = true
			}
		}
		return selected
	}
	return selected
}

func initialStages(selected map[string]bool, staticOnly bool) []model.StageRecord {
	stages := make([]model.StageRecord, 0, 6)
	for _, stage := range []string{"A", "B", "C", "D", "E", "F"} {
		if selected[stage] {
			stages = append(stages, model.StageRecord{
				Stage:         stage,
				Name:          stageName(stage),
				Status:        model.StagePending,
				ArtifactPaths: []string{},
			})
			continue
		}
		reason := "Not selected for this run."
		if staticOnly && (stage == "B" || stage == "C") {
			reason = "--static-only skips Docker and run_tests evidence."
		}
		stages = append(stages, skippedStage(stage, reason))
	}
	return stages
}

func stageName(stage string) string {
	switch stage {
	case "A":
		return "structure and rules check"
	case "B":
		return "Docker runtime evidence"
	case "C":
		return "run_tests runtime evidence"
	case "D":
		return "tests effectiveness static review"
	case "E":
		return "static acceptance audit"
	case "F":
		return "repair summary and short_comment"
	default:
		return "unknown"
	}
}

func startStage(stage string) model.StageRecord {
	return model.StageRecord{
		Stage:         stage,
		Name:          stageName(stage),
		Status:        model.StageRunning,
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
		ArtifactPaths: []string{},
	}
}

func finishStage(record model.StageRecord, status string, started time.Time) model.StageRecord {
	record.Status = status
	record.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	record.DurationMS = time.Since(started).Milliseconds()
	return record
}

func skippedStage(stage, reason string) model.StageRecord {
	return model.StageRecord{
		Stage:         stage,
		Name:          stageName(stage),
		Status:        model.StageSkipped,
		ArtifactPaths: []string{},
		ErrorSummary:  reason,
	}
}

func blockedStage(stage, name string, blockedBy []string, reason string) model.StageRecord {
	return model.StageRecord{
		Stage:         stage,
		Name:          name,
		Status:        model.StageBlocked,
		BlockedBy:     blockedBy,
		ArtifactPaths: []string{},
		ErrorSummary:  reason,
		Findings: []model.Finding{{
			Stage:      stage,
			Severity:   "High",
			Title:      fmt.Sprintf("Stage %s blocked", stage),
			Rule:       "Pipeline dependency",
			Evidence:   reason,
			Impact:     "Required evidence was not collected.",
			MinimumFix: "Fix the upstream stage and rerun the affected dependency chain.",
		}},
	}
}

func (r Runner) materializeSkippedStage(run model.RunRecord, record model.StageRecord) model.StageRecord {
	switch record.Stage {
	case "B":
		logPath := filepath.Join(run.ArtifactRoot, "logs", "B_docker.log")
		portMapPath := filepath.Join(run.ArtifactRoot, "port_map.json")
		screenshotPath := filepath.Join(run.ArtifactRoot, "5_Docker启动截图.png")
		reason := record.ErrorSummary
		if reason == "" {
			reason = "Stage B was not executed."
		}
		_ = writeText(logPath, reason)
		_ = writeJSON(portMapPath, map[string]any{"run_id": run.RunID, "mappings": map[string]any{}, "reason": reason})
		_ = writePlaceholderPNG(screenshotPath)
		record.LogPath = logPath
		record.ArtifactPaths = []string{portMapPath, screenshotPath}
	case "C":
		logPath := filepath.Join(run.ArtifactRoot, "logs", "C_tests.log")
		screenshotPath := filepath.Join(run.ArtifactRoot, "6_run_tests.sh运行截图.png")
		summaryPath := filepath.Join(run.ArtifactRoot, "test_runtime_summary.json")
		reason := record.ErrorSummary
		if reason == "" {
			reason = "Stage C was not executed."
		}
		_ = writeText(logPath, reason)
		_ = writeJSON(summaryPath, map[string]any{"ok": false, "reason": reason})
		_ = writePlaceholderPNG(screenshotPath)
		record.LogPath = logPath
		record.ArtifactPaths = []string{logPath, screenshotPath, summaryPath}
	}
	return record
}

func (r Runner) stageA(run model.RunRecord, project scanner.Project) model.StageRecord {
	start := time.Now()
	record := startStage("A")
	logPath := filepath.Join(run.ArtifactRoot, "logs", "A_validate.log")
	record.LogPath = logPath

	scriptRoot := project.Path
	snapshotPath := filepath.Join(run.ArtifactRoot, "script_input_snapshot")
	snapshotErr := copyPackageSnapshot(project.Path, snapshotPath)
	if snapshotErr == nil {
		scriptRoot = snapshotPath
	}

	required := map[string]bool{
		"docs":              dirExists(filepath.Join(project.Path, "docs")),
		"repo":              dirExists(filepath.Join(project.Path, "repo")),
		"original_sessions": dirExists(filepath.Join(project.Path, "original_sessions")),
		"metadata.json":     fileExists(filepath.Join(project.Path, "metadata.json")),
	}
	findings := structuralFindings(project, required)
	if snapshotErr != nil {
		findings = append(findings, model.Finding{
			Stage:      "A",
			Severity:   "High",
			Title:      "Script input snapshot could not be created",
			Rule:       "Stage A scripts should inspect the delivery package without prior p2r QA artifacts.",
			Evidence:   snapshotErr.Error(),
			Impact:     "Checks may include previous qa/ artifacts and produce noisy findings.",
			MinimumFix: "Ensure the run artifact directory is writable and rerun Stage A.",
		})
	}

	acceptancePath := filepath.Join(run.ArtifactRoot, "acceptance.json")
	reportPath := filepath.Join(run.ArtifactRoot, "validation_report.md")
	requiredPath := filepath.Join(run.ArtifactRoot, "required_artifacts.json")
	readmeAlignmentPath := filepath.Join(run.ArtifactRoot, "readme_alignment.json")
	localDependencyPath := filepath.Join(run.ArtifactRoot, "local_dependency.json")
	fakeImplPath := filepath.Join(run.ArtifactRoot, "fake_impl.json")
	testsInspectionPath := filepath.Join(run.ArtifactRoot, "tests_inspection.json")
	englishOnlyPath := filepath.Join(run.ArtifactRoot, "english_only.json")

	scriptResults := r.runStageAScripts(project, scriptRoot, logPath, map[string]string{
		"acceptance":       acceptancePath,
		"validation":       reportPath,
		"required":         requiredPath,
		"readme_alignment": readmeAlignmentPath,
		"local_dependency": localDependencyPath,
		"fake_impl":        fakeImplPath,
		"tests_inspection": testsInspectionPath,
		"english_only":     englishOnlyPath,
	})

	if !fileExists(acceptancePath) {
		_ = writeJSON(acceptancePath, scriptResults["run_acceptance.py"])
	}
	if !fileExists(reportPath) {
		_ = writeText(reportPath, validationMarkdown(project, required, findings))
	}
	if result, ok := scriptResults["run_acceptance.py"]; ok && !result.OK {
		findings = append(findings, model.Finding{
			Stage:      "A",
			Severity:   "High",
			Title:      "run_acceptance.py did not complete cleanly",
			Rule:       "Stage A must collect primary acceptance evidence from the bundled script.",
			Evidence:   result.summary(),
			Impact:     "Primary acceptance evidence may be incomplete.",
			MinimumFix: "Ensure Python/uv can run the embedded scripts and rerun Stage A.",
			SourcePath: acceptancePath,
		})
	}
	findings = append(findings, acceptanceFindings(acceptancePath)...)

	artifactPaths := []string{acceptancePath, reportPath, requiredPath, readmeAlignmentPath, localDependencyPath}
	for _, path := range []string{fakeImplPath, testsInspectionPath, englishOnlyPath} {
		if fileExists(path) {
			artifactPaths = append(artifactPaths, path)
		}
	}
	record.ArtifactPaths = artifactPaths
	record.Findings = findings
	_ = appendText(logPath, "\n\n"+validationMarkdown(project, required, findings))

	status := model.StageDone
	if hasHardStageAFailure(record.Findings, scriptResults["run_acceptance.py"]) {
		status = model.StageFailed
		record.ErrorSummary = fmt.Sprintf("%d acceptance finding(s)", len(record.Findings))
	}
	return finishStage(record, status, start)
}

func structuralFindings(project scanner.Project, required map[string]bool) []model.Finding {
	findings := []model.Finding{}
	for name, ok := range required {
		if !ok {
			findings = append(findings, model.Finding{
				Stage:      "A",
				Severity:   "Blocker",
				Title:      "Missing required delivery artifact: " + name,
				Rule:       "A package must contain docs/, repo/, original_sessions/, and metadata.json.",
				Evidence:   filepath.Join(project.Path, name),
				Impact:     "The package cannot be validated as a prompt2repo delivery.",
				MinimumFix: "Add the missing required artifact and rerun p2r scan/run.",
			})
		}
	}
	if project.MetadataPromptMissing {
		findings = append(findings, model.Finding{
			Stage:      "A",
			Severity:   "Blocker",
			Title:      "metadata.json prompt is missing",
			Rule:       "metadata.json.prompt is required for acceptance mapping.",
			Evidence:   filepath.Join(project.Path, "metadata.json"),
			Impact:     "Static audit cannot confidently map implementation to the original prompt.",
			MinimumFix: "Populate metadata.json.prompt with the source prompt.",
		})
	}
	return findings
}

func (r Runner) runStageAScripts(project scanner.Project, scriptRoot, logPath string, outputs map[string]string) map[string]scriptExecution {
	results := map[string]scriptExecution{}
	projectTypeArgs := projectTypeArgs(scriptRoot)
	results["run_acceptance.py"] = r.runStageAScript(project.Path, scriptRoot, "run_acceptance.py", acceptanceScriptArgs(outputs, projectTypeArgs))

	checks := []struct {
		script string
		output string
		args   []string
	}{
		{"check_required_artifacts.py", outputs["required"], projectTypeArgs},
		{"check_readme_alignment.py", outputs["readme_alignment"], projectTypeArgs},
		{"check_local_dependency.py", outputs["local_dependency"], nil},
		{"check_fake_impl.py", outputs["fake_impl"], nil},
		{"inspect_tests.py", outputs["tests_inspection"], projectTypeArgs},
	}
	if promptLooksEnglish(filepath.Join(project.Path, "metadata.json")) {
		checks = append(checks, struct {
			script string
			output string
			args   []string
		}{"check_english_only.py", outputs["english_only"], nil})
	}
	var log strings.Builder
	log.WriteString(results["run_acceptance.py"].logBlock())
	for _, check := range checks {
		result := r.runStageAScript(project.Path, scriptRoot, check.script, check.args)
		results[check.script] = result
		_ = writeJSON(check.output, result)
		log.WriteString(result.logBlock())
	}
	_ = writeText(logPath, log.String())
	return results
}

func acceptanceScriptArgs(outputs map[string]string, projectTypeArgs []string) []string {
	args := []string{
		"--output-json", outputs["acceptance"],
		"--output-md", outputs["validation"],
	}
	return append(args, projectTypeArgs...)
}

type scriptExecution struct {
	Script    string `json:"script"`
	Command   string `json:"command,omitempty"`
	InputRoot string `json:"input_root,omitempty"`
	OK        bool   `json:"ok"`
	ExitCode  int    `json:"exit_code"`
	Timeout   bool   `json:"timeout,omitempty"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (r Runner) runStageAScript(workDir, inputRoot, script string, extraArgs []string) scriptExecution {
	scriptPath := filepath.Join(r.cfg.ScanPath, ".qa-control", "scripts", script)
	result := scriptExecution{Script: script, InputRoot: inputRoot}
	python, prefix, pythonErr := r.pythonInvocation(workDir)
	if pythonErr != "" {
		result.Error = pythonErr
		return result
	}
	if !fileExists(scriptPath) {
		result.Error = "script not found: " + scriptPath
		return result
	}
	args := append(append([]string{}, prefix...), scriptPath, inputRoot)
	args = append(args, extraArgs...)
	env := append(os.Environ(), "UV_CACHE_DIR="+filepath.Join(r.cfg.ScanPath, ".qa-control", ".uv-cache"))
	timeout := time.Duration(r.cfg.Pipeline.StageTimeouts["A"]) * time.Second
	execResult := r.exec.Run(context.Background(), timeout, inputRoot, env, python, args...)
	result.Command = execResult.Command
	result.Stdout = execResult.Stdout
	result.Stderr = execResult.Stderr
	result.ExitCode = execResult.ExitCode
	result.Timeout = execResult.Timeout
	result.OK = execResult.Err == nil
	if execResult.Err != nil {
		result.Error = execResult.Err.Error()
	}
	return result
}

func (s scriptExecution) logBlock() string {
	var builder strings.Builder
	builder.WriteString(s.Command)
	if s.Command == "" {
		builder.WriteString(s.Script)
	}
	builder.WriteString("\nINPUT_ROOT:\n" + s.InputRoot + "\nSTDOUT:\n" + s.Stdout + "\nSTDERR:\n" + s.Stderr)
	if s.Error != "" {
		builder.WriteString("\nERROR:\n" + s.Error)
	}
	builder.WriteString("\n\n")
	return builder.String()
}

func (s scriptExecution) summary() string {
	parts := []string{}
	if s.Error != "" {
		parts = append(parts, s.Error)
	}
	if text := strings.TrimSpace(s.Stderr); text != "" {
		parts = append(parts, text)
	}
	if text := strings.TrimSpace(s.Stdout); text != "" {
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		return "script produced no output"
	}
	return strings.Join(parts, "\n")
}

func (r Runner) pythonInvocation(workDir string) (string, []string, string) {
	for _, name := range []string{"python", "python3"} {
		path, err := r.exec.LookPath(name)
		if err != nil {
			continue
		}
		result := r.exec.Run(context.Background(), 5*time.Second, workDir, nil, path, "--version")
		if result.Err == nil && (strings.Contains(result.Stdout, "Python") || strings.Contains(result.Stderr, "Python")) {
			return path, nil, ""
		}
	}
	if path, err := r.exec.LookPath("uv"); err == nil {
		return path, []string{"run", "python"}, ""
	}
	return "", nil, "python interpreter not found"
}

func (r Runner) stageB(ctx context.Context, run model.RunRecord, project scanner.Project) model.StageRecord {
	start := time.Now()
	record := startStage("B")
	logPath := filepath.Join(run.ArtifactRoot, "logs", "B_docker.log")
	record.LogPath = logPath
	portMapPath := filepath.Join(run.ArtifactRoot, "port_map.json")
	screenshotPath := filepath.Join(run.ArtifactRoot, "5_Docker启动截图.png")
	record.ArtifactPaths = append(record.ArtifactPaths, portMapPath, screenshotPath)
	repoPath := filepath.Join(project.Path, "repo")
	compose := findCompose(repoPath)
	readmeCommand := []string{}
	if compose == "" {
		readmeCommand = readmeComposeCommand(repoPath)
	}
	if compose == "" && len(readmeCommand) == 0 {
		return r.failB(record, start, logPath, portMapPath, screenshotPath, "No docker-compose.yml or README-declared docker compose startup command found in repo/.", "Add repo/docker-compose.yml, document a docker compose startup command, or run --static-only.")
	}
	if _, err := r.exec.LookPath("docker"); err != nil {
		return r.failB(record, start, logPath, portMapPath, screenshotPath, "docker executable not found on PATH.", "Install Docker or run --static-only.")
	}
	projectName := dockermgr.ComposeProjectName(r.cfg.Docker.ComposeProjectPrefix, project.TaskID, run.RunID)
	timeout := time.Duration(r.cfg.Pipeline.StageTimeouts["B"]) * time.Second
	workDir := repoPath
	upArgs := []string{"compose", "-f", compose, "-p", projectName, "up", "-d", "--build"}
	psArgs := []string{"compose", "-f", compose, "-p", projectName, "ps", "--format", "json"}
	psQArgs := []string{"compose", "-f", compose, "-p", projectName, "ps", "-q"}
	servicesArgs := []string{"compose", "-f", compose, "-p", projectName, "config", "--services"}
	if compose != "" {
		workDir = filepath.Dir(compose)
	} else {
		upArgs = composeArgsWithProject(readmeCommand, projectName)
		psArgs = composePSArgs(readmeCommand, projectName)
		psQArgs = composePSQArgs(readmeCommand, projectName)
		servicesArgs = composeServicesArgs(readmeCommand, projectName)
	}
	up := r.exec.Run(ctx, timeout, workDir, nil, "docker", upArgs...)
	log := up.Command + "\n\nSTDOUT:\n" + up.Stdout + "\nSTDERR:\n" + up.Stderr
	if up.Err != nil {
		reason := "docker compose up failed"
		if up.Timeout {
			reason = "docker compose up timed out"
		}
		return r.failB(record, start, logPath, portMapPath, screenshotPath, reason+": "+strings.TrimSpace(up.Stderr), "Fix Docker startup and rerun stage B.")
	}
	ps := r.exec.Run(ctx, 30*time.Second, workDir, nil, "docker", psArgs...)
	log += "\n\n" + ps.Command + "\n\nSTDOUT:\n" + ps.Stdout + "\nSTDERR:\n" + ps.Stderr
	mappings, services := parseComposePS(ps.Stdout)
	if ps.Err != nil || len(mappings) == 0 {
		fallbackMappings, fallbackServices, fallbackLog := r.dockerPortFallback(ctx, workDir, psQArgs, servicesArgs)
		log += fallbackLog
		if len(fallbackMappings) > 0 {
			mappings = fallbackMappings
			services = fallbackServices
		}
	}
	_ = writeText(logPath, log)
	probes := probeMappings(mappings, time.Duration(r.cfg.Docker.HealthCheckTimeoutSeconds)*time.Second)
	_ = writeJSON(portMapPath, map[string]any{
		"run_id":          run.RunID,
		"compose_project": projectName,
		"compose_file":    compose,
		"work_dir":        workDir,
		"services":        services,
		"raw_compose_ps":  ps.Stdout,
		"mappings":        mappings,
		"probes":          probes,
	})
	_ = writePlaceholderPNG(screenshotPath)
	if ps.Err != nil && len(mappings) == 0 {
		record.Findings = []model.Finding{{
			Stage:      "B",
			Severity:   "High",
			Title:      "Docker started but port inspection failed",
			Rule:       "B evidence requires Docker/Compose inspection.",
			Evidence:   strings.TrimSpace(ps.Stderr),
			Impact:     "Runtime ports could not be recorded in port_map.json.",
			MinimumFix: "Ensure docker compose ps --format json works for the project.",
		}}
		record.ErrorSummary = "port inspection failed"
		return finishStage(record, model.StageFailed, start)
	}
	if len(mappings) == 0 {
		record.Findings = []model.Finding{{
			Stage:      "B",
			Severity:   "High",
			Title:      "Docker port mappings were empty",
			Rule:       "Stage B must record real Docker/Compose port mappings.",
			Evidence:   "docker compose ps and docker port fallback returned no published ports.",
			Impact:     "External browser/runtime evidence cannot be collected from mapped host ports.",
			MinimumFix: "Expose the service ports in docker-compose.yml and rerun B.",
		}}
		record.ErrorSummary = "no published ports"
		return finishStage(record, model.StageFailed, start)
	}
	return finishStage(record, model.StageDone, start)
}

func (r Runner) dockerPortFallback(ctx context.Context, workDir string, psQArgs, servicesArgs []string) (map[string][]portMapping, []string, string) {
	var log strings.Builder
	servicesResult := r.exec.Run(ctx, 30*time.Second, workDir, nil, "docker", servicesArgs...)
	log.WriteString("\n\n" + servicesResult.Command + "\nSTDOUT:\n" + servicesResult.Stdout + "\nSTDERR:\n" + servicesResult.Stderr)
	serviceNames := splitNonEmptyLines(servicesResult.Stdout)
	psQ := r.exec.Run(ctx, 30*time.Second, workDir, nil, "docker", psQArgs...)
	log.WriteString("\n\n" + psQ.Command + "\nSTDOUT:\n" + psQ.Stdout + "\nSTDERR:\n" + psQ.Stderr)
	containers := splitNonEmptyLines(psQ.Stdout)
	mappings := map[string][]portMapping{}
	var services []string
	for index, container := range containers {
		service := container
		if index < len(serviceNames) {
			service = serviceNames[index]
		}
		services = append(services, service)
		port := r.exec.Run(ctx, 30*time.Second, workDir, nil, "docker", "port", container)
		log.WriteString("\n\n" + port.Command + "\nSTDOUT:\n" + port.Stdout + "\nSTDERR:\n" + port.Stderr)
		mappings[service] = append(mappings[service], parseDockerPort(service, port.Stdout)...)
	}
	return mappings, services, log.String()
}

func (r Runner) failB(record model.StageRecord, start time.Time, logPath, portMapPath, screenshotPath, evidence, fix string) model.StageRecord {
	_ = writeText(logPath, evidence)
	_ = writeJSON(portMapPath, map[string]any{"mappings": map[string]any{}, "reason": evidence})
	_ = writePlaceholderPNG(screenshotPath)
	record.Findings = []model.Finding{{
		Stage:      "B",
		Severity:   "High",
		Title:      "Docker runtime evidence was not collected",
		Rule:       "Stage B must collect runtime evidence from Docker/Compose.",
		Evidence:   evidence,
		Impact:     "Stage C runtime tests cannot run from this evidence chain.",
		MinimumFix: fix,
	}}
	record.ErrorSummary = evidence
	return finishStage(record, model.StageFailed, start)
}

func (r Runner) stageC(ctx context.Context, run model.RunRecord, project scanner.Project) model.StageRecord {
	start := time.Now()
	record := startStage("C")
	logPath := filepath.Join(run.ArtifactRoot, "logs", "C_tests.log")
	screenshotPath := filepath.Join(run.ArtifactRoot, "6_run_tests.sh运行截图.png")
	summaryPath := filepath.Join(run.ArtifactRoot, "test_runtime_summary.json")
	record.LogPath = logPath
	record.ArtifactPaths = append(record.ArtifactPaths, logPath, screenshotPath, summaryPath)
	script, _, _ := findRunTests(filepath.Join(project.Path, "repo"), r.exec)
	if script == "" {
		evidence := "No run_tests.sh, run_tests.ps1, or run_tests.py found in repo/."
		_ = writeText(logPath, evidence)
		_ = writePlaceholderPNG(screenshotPath)
		_ = writeJSON(summaryPath, map[string]any{"ok": false, "reason": evidence})
		record.Findings = []model.Finding{{
			Stage:      "C",
			Severity:   "High",
			Title:      "Unified test entrypoint is missing",
			Rule:       "Stage C requires repo/run_tests.*.",
			Evidence:   evidence,
			Impact:     "Runtime test evidence cannot be collected.",
			MinimumFix: "Add a runnable repo/run_tests.sh, .ps1, or .py entrypoint.",
		}}
		record.ErrorSummary = evidence
		return finishStage(record, model.StageFailed, start)
	}
	runtime, runtimeErr := readRuntimeEvidence(filepath.Join(run.ArtifactRoot, "port_map.json"))
	if runtimeErr != nil || runtime.ComposeProject == "" || len(runtime.Services) == 0 {
		evidence := "Stage B runtime evidence is missing compose project or service information."
		if runtimeErr != nil {
			evidence = runtimeErr.Error()
		}
		_ = writeText(logPath, evidence)
		_ = writePlaceholderPNG(screenshotPath)
		_ = writeJSON(summaryPath, map[string]any{"ok": false, "reason": evidence})
		record.Findings = []model.Finding{{
			Stage:      "C",
			Severity:   "High",
			Title:      "Cannot execute run_tests inside Compose network",
			Rule:       "Stage C requires Stage B Compose runtime metadata.",
			Evidence:   evidence,
			Impact:     "Runtime test evidence cannot be collected inside the service network.",
			MinimumFix: "Rerun B successfully and ensure compose service metadata is present.",
		}}
		record.ErrorSummary = evidence
		return finishStage(record, model.StageFailed, start)
	}
	if _, err := r.exec.LookPath("docker"); err != nil {
		evidence := "docker executable not found on PATH."
		_ = writeText(logPath, evidence)
		_ = writePlaceholderPNG(screenshotPath)
		_ = writeJSON(summaryPath, map[string]any{"ok": false, "reason": evidence})
		record.Findings = []model.Finding{{
			Stage:      "C",
			Severity:   "High",
			Title:      "Docker unavailable for runtime tests",
			Rule:       "Stage C must run tests in the Compose runtime context.",
			Evidence:   evidence,
			Impact:     "Runtime test evidence cannot be collected.",
			MinimumFix: "Install Docker and rerun B/C.",
		}}
		record.ErrorSummary = evidence
		return finishStage(record, model.StageFailed, start)
	}
	timeout := time.Duration(r.cfg.Pipeline.StageTimeouts["C"]) * time.Second
	args := []string{"compose"}
	if runtime.ComposeFile != "" {
		args = append(args, "-f", runtime.ComposeFile)
	}
	args = append(args, "-p", runtime.ComposeProject, "exec", "-T", runtime.Services[0], "sh", "-lc", containerRunTestsCommand())
	result := r.exec.Run(ctx, timeout, runtime.WorkDir, nil, "docker", args...)
	log := result.Command + "\n\nSTDOUT:\n" + result.Stdout + "\nSTDERR:\n" + result.Stderr
	_ = writeText(logPath, log)
	_ = writePlaceholderPNG(screenshotPath)
	_ = writeJSON(summaryPath, map[string]any{"ok": result.Err == nil, "exit_code": result.ExitCode, "timeout": result.Timeout, "script": script, "service": runtime.Services[0], "compose_project": runtime.ComposeProject})
	if result.Err != nil {
		record.Findings = []model.Finding{{
			Stage:      "C",
			Severity:   "High",
			Title:      "run_tests runtime evidence failed",
			Rule:       "Stage C must execute the unified test entrypoint successfully.",
			Evidence:   strings.TrimSpace(result.Stderr),
			Impact:     "The delivery package does not currently have passing runtime test evidence.",
			MinimumFix: "Fix the test entrypoint or application runtime and rerun C.",
		}}
		record.ErrorSummary = "run_tests failed"
		return finishStage(record, model.StageFailed, start)
	}
	return finishStage(record, model.StageDone, start)
}

func (r Runner) stageCodex(ctx context.Context, run model.RunRecord, project scanner.Project, stage, profile, output, compat string) model.StageRecord {
	start := time.Now()
	record := startStage(stage)
	logPath := filepath.Join(run.ArtifactRoot, "logs", fmt.Sprintf("%s_static.log", stage))
	if stage == "D" {
		logPath = filepath.Join(run.ArtifactRoot, "logs", "D_tests_coverage_static.log")
	}
	if stage == "E" {
		logPath = filepath.Join(run.ArtifactRoot, "logs", "E_static_audit.log")
	}
	outputPath := filepath.Join(run.ArtifactRoot, output)
	compatPath := filepath.Join(run.ArtifactRoot, compat)
	record.LogPath = logPath
	record.ArtifactPaths = append(record.ArtifactPaths, outputPath, compatPath)
	profilePath := filepath.Join(r.cfg.Codex.PromptProfilesDir, profile)
	profileContent, readErr := os.ReadFile(profilePath)
	if readErr != nil {
		report := staticUnavailableReport(stage, profile, project.Path, "prompt profile not readable: "+readErr.Error())
		_ = writeText(outputPath, report)
		_ = writeText(compatPath, report)
		_ = writeText(logPath, report)
		record.Findings = []model.Finding{{
			Stage:      stage,
			Severity:   "High",
			Title:      stageName(stage) + " profile missing",
			Rule:       "Static review stages require an embedded prompt profile.",
			Evidence:   readErr.Error(),
			Impact:     "Static review evidence is incomplete and requires manual verification.",
			MinimumFix: "Ensure assets were released to .qa-control and rerun this stage.",
		}}
		record.ErrorSummary = "prompt profile unavailable"
		return finishStage(record, model.StageFailed, start)
	}
	if r.cfg.Codex.Network != "none" {
		report := staticUnavailableReport(stage, profile, project.Path, "configured Codex network mode is unsupported by the current safe sandbox: "+r.cfg.Codex.Network)
		_ = writeText(outputPath, report)
		_ = writeText(compatPath, report)
		_ = writeText(logPath, report)
		record.Findings = []model.Finding{{
			Stage:      stage,
			Severity:   "High",
			Title:      stageName(stage) + " network policy unsupported",
			Rule:       "D/E must execute under an enforceable no-network static sandbox for MVP.",
			Evidence:   "codex.network=" + r.cfg.Codex.Network,
			Impact:     "Static review evidence is incomplete because requested network behavior cannot be safely enforced.",
			MinimumFix: "Set codex.network to none or implement a dedicated network-controlled sandbox runner.",
		}}
		record.ErrorSummary = "codex network policy unsupported"
		return finishStage(record, model.StageFailed, start)
	}
	if r.cfg.Codex.WritableTmp {
		report := staticUnavailableReport(stage, profile, project.Path, "configured writable_tmp=true is unsupported without widening write access in the current Codex CLI sandbox")
		_ = writeText(outputPath, report)
		_ = writeText(compatPath, report)
		_ = writeText(logPath, report)
		record.Findings = []model.Finding{{
			Stage:      stage,
			Severity:   "High",
			Title:      stageName(stage) + " writable tmp policy unsupported",
			Rule:       "D/E must not gain project write access during static review.",
			Evidence:   "codex.writable_tmp=true",
			Impact:     "Static review evidence is incomplete because artifact-only writes cannot be safely enforced.",
			MinimumFix: "Set codex.writable_tmp to false or implement artifact-only writable sandbox mounting.",
		}}
		record.ErrorSummary = "codex writable tmp policy unsupported"
		return finishStage(record, model.StageFailed, start)
	}
	if _, err := r.exec.LookPath("codex"); err != nil {
		report := staticUnavailableReport(stage, profile, project.Path, "codex executable not found on PATH")
		_ = writeText(outputPath, report)
		_ = writeText(compatPath, report)
		_ = writeText(logPath, report)
		record.Findings = []model.Finding{{
			Stage:      stage,
			Severity:   "High",
			Title:      stageName(stage) + " unavailable",
			Rule:       "Static review stages require codex exec or an equivalent reviewer.",
			Evidence:   "codex executable not found on PATH",
			Impact:     "Static review evidence is incomplete and requires manual verification.",
			MinimumFix: "Install Codex CLI or run the static template manually, then rerun this stage.",
		}}
		record.ErrorSummary = "codex unavailable"
		return finishStage(record, model.StageFailed, start)
	}
	sandbox, sandboxErr := codex.NewSandbox(project.Path, run.ArtifactRoot, "static")
	if sandboxErr != nil {
		record.Findings = []model.Finding{{
			Stage:      stage,
			Severity:   "High",
			Title:      stageName(stage) + " sandbox setup failed",
			Rule:       "Static review stages require an isolated writable HOME.",
			Evidence:   sandboxErr.Error(),
			Impact:     "Static review evidence is incomplete and requires manual verification.",
			MinimumFix: "Ensure the run artifact directory is writable and rerun this stage.",
		}}
		record.ErrorSummary = "codex sandbox unavailable"
		return finishStage(record, model.StageFailed, start)
	}
	env := sandbox.Env(os.Environ())
	timeout := time.Duration(r.cfg.Pipeline.StageTimeouts[stage]) * time.Second
	prompt := codexPrompt(stage, profile, project.Path, run.ArtifactRoot, string(profileContent))
	sandboxMode := "read-only"
	result := r.exec.Run(ctx, timeout, project.Path, env, "codex", "exec", "--skip-git-repo-check", "--sandbox", sandboxMode, prompt)
	report := strings.TrimSpace(result.Stdout)
	if report == "" {
		report = staticUnavailableReport(stage, profile, project.Path, strings.TrimSpace(result.Stderr))
	}
	report = truncateString(report, r.cfg.Codex.MaxOutputBytes)
	_ = writeText(outputPath, report+"\n")
	_ = writeText(compatPath, report+"\n")
	_ = writeText(logPath, result.Command+"\n\nSTDOUT:\n"+truncateString(result.Stdout, r.cfg.Codex.MaxOutputBytes)+"\nSTDERR:\n"+truncateString(result.Stderr, r.cfg.Codex.MaxOutputBytes))
	if result.Err != nil {
		record.Findings = []model.Finding{{
			Stage:      stage,
			Severity:   "High",
			Title:      stageName(stage) + " failed",
			Rule:       "Static Codex review must complete without runtime actions.",
			Evidence:   strings.TrimSpace(result.Stderr),
			Impact:     "Static review report may be incomplete.",
			MinimumFix: "Inspect the static review log and rerun the stage.",
		}}
		record.ErrorSummary = "codex exec failed"
		return finishStage(record, model.StageFailed, start)
	}
	record.Findings = extractFindingsFromReport(stage, report, outputPath)
	return finishStage(record, model.StageDone, start)
}

func codexPrompt(stage, profile, projectPath, artifactRoot, profileContent string) string {
	return fmt.Sprintf(`Run p2r stage %s as a pure static review.

Project path: %s
Artifact root: %s
Prompt profile: %s

Hard boundaries:
- Do not start services.
- Do not run Docker.
- Do not run tests.
- Do not modify files.
- Cite file:line evidence for strong claims.
- Mark runtime-only conclusions as Manual Verification Required unless citing existing B/C artifacts.

Profile:
%s
`, stage, projectPath, artifactRoot, profile, profileContent)
}

func (r Runner) stageF(run model.RunRecord, prior map[string]model.StageRecord) model.StageRecord {
	start := time.Now()
	record := startStage("F")
	logPath := filepath.Join(run.ArtifactRoot, "logs", "F_repair.log")
	summaryPath := filepath.Join(run.ArtifactRoot, "repair_summary.json")
	reportPath := filepath.Join(run.ArtifactRoot, "3_标注员AI报告问题的修复报告.md")
	shortPath := filepath.Join(run.ArtifactRoot, "short_comment.txt")
	record.LogPath = logPath
	record.ArtifactPaths = append(record.ArtifactPaths, summaryPath, reportPath, shortPath)
	var findings []model.Finding
	stageStatuses := map[string]string{}
	for _, stage := range []string{"A", "B", "C", "D", "E"} {
		if item, ok := prior[stage]; ok {
			stageStatuses[stage] = item.Status
			findings = append(findings, item.Findings...)
		}
	}
	sortFindings(findings)
	summary := map[string]any{
		"run_id":         run.RunID,
		"stage_statuses": stageStatuses,
		"findings":       findings,
		"highest_risk":   highestRisk(findings),
	}
	_ = writeJSON(summaryPath, summary)
	report := repairMarkdown(stageStatuses, findings)
	_ = writeText(reportPath, report)
	_ = writeText(logPath, report)
	_ = writeText(shortPath, shortComment(stageStatuses, findings))
	record.Findings = findings
	return finishStage(record, model.StageDone, start)
}

func (r Runner) writeRunManifest(run model.RunRecord, project scanner.Project, opts RunOptions, released []assets.ReleasedFile, releaseErr error) error {
	manifest := map[string]any{
		"run_id":        run.RunID,
		"task_id":       run.TaskID,
		"started_at":    run.StartedAt,
		"project_path":  project.Path,
		"static_only":   run.StaticOnly,
		"stage":         opts.Stage,
		"from":          opts.From,
		"stages":        opts.Stages,
		"tool_versions": map[string]string{"p2r": "dev"},
		"assets":        released,
		"codex_policy": map[string]any{
			"sandbox_image":       r.cfg.Codex.SandboxImage,
			"network":             r.cfg.Codex.Network,
			"max_output_bytes":    r.cfg.Codex.MaxOutputBytes,
			"writable_tmp":        r.cfg.Codex.WritableTmp,
			"sandbox_mode":        "read-only",
			"home_reuse_strategy": "same .codex-home-static path, removed and recreated between D/E stages",
			"docker_socket":       "not mounted",
		},
	}
	if releaseErr != nil {
		manifest["asset_release_error"] = releaseErr.Error()
	}
	return writeJSON(filepath.Join(run.ArtifactRoot, "run_manifest.json"), manifest)
}

func (r Runner) writeStageStatus(runID, artifactRoot string, stages []model.StageRecord) error {
	return writeJSON(filepath.Join(artifactRoot, "stage_status.json"), model.StageStatusFile{RunID: runID, Stages: stages})
}

func runStatus(stages []model.StageRecord) string {
	for _, stage := range stages {
		if stage.Status == model.StageFailed || stage.Status == model.StageBlocked || len(stage.Findings) > 0 {
			return model.RunCompletedWithFindings
		}
	}
	return model.RunCompletedClean
}

func copyPackageSnapshot(source, dest string) error {
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	skipTopLevel := map[string]bool{
		".git":        true,
		".qa-control": true,
		"qa":          true,
	}
	skipDirNames := map[string]bool{
		".next":         true,
		".pytest_cache": true,
		".venv":         true,
		"__pycache__":   true,
		"build":         true,
		"coverage":      true,
		"dist":          true,
		"node_modules":  true,
		"venv":          true,
	}
	return filepath.WalkDir(source, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil || rel == "." {
			return err
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) > 0 && skipTopLevel[parts[0]] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() && skipDirNames[d.Name()] {
			return filepath.SkipDir
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(path, target, info.Mode())
	})
}

func copyFile(source, dest string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func projectTypeArgs(root string) []string {
	content, err := os.ReadFile(filepath.Join(root, "metadata.json"))
	if err != nil {
		return nil
	}
	var data map[string]any
	if json.Unmarshal(content, &data) != nil {
		return nil
	}
	value := strings.ToLower(strings.TrimSpace(fmt.Sprint(data["project_type"])))
	switch value {
	case "pure_frontend", "pure_backend", "fullstack":
		return []string{"--project-type", value}
	case "web", "android", "ios", "desktop":
		return []string{"--project-type", "pure_frontend"}
	case "server":
		return []string{"--project-type", "pure_backend"}
	default:
		return nil
	}
}

func acceptanceFindings(path string) []model.Finding {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(content, &payload); err != nil {
		return []model.Finding{{
			Stage:      "A",
			Severity:   "High",
			Title:      "acceptance.json was not valid JSON",
			Rule:       "run_acceptance.py must emit machine-readable JSON.",
			Evidence:   err.Error(),
			Impact:     "Acceptance findings could not be extracted.",
			MinimumFix: "Inspect Stage A logs and rerun after fixing script execution.",
			SourcePath: path,
		}}
	}
	var findings []model.Finding
	findings = append(findings, issueFindings("blocking_issues", payload["blocking_issues"], path)...)
	findings = append(findings, issueFindings("non_blocking_issues", payload["non_blocking_issues"], path)...)
	return findings
}

func issueFindings(section string, raw any, sourcePath string) []model.Finding {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	findings := make([]model.Finding, 0, len(items))
	for _, item := range items {
		issue, ok := item.(map[string]any)
		if !ok {
			continue
		}
		issueID := issueString(issue, "issue_id", "acceptance issue")
		if section == "non_blocking_issues" && issueID == "runtime-verification-missing" {
			continue
		}
		findings = append(findings, model.Finding{
			Stage:      "A",
			Severity:   acceptanceSeverity(section, issue["severity"]),
			Title:      issueID,
			Rule:       issueString(issue, "rule", ""),
			Evidence:   issueString(issue, "evidence", ""),
			Impact:     issueString(issue, "done_criteria", ""),
			MinimumFix: issueString(issue, "repair_action", ""),
			SourcePath: sourcePath,
		})
	}
	return findings
}

func issueString(issue map[string]any, key, fallback string) string {
	value, ok := issue[key]
	if !ok || value == nil {
		return fallback
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" {
		return fallback
	}
	return text
}

func acceptanceSeverity(section string, raw any) string {
	value := strings.ToLower(strings.TrimSpace(fmt.Sprint(raw)))
	if section == "blocking_issues" || value == "blocking" || value == "blocker" || value == "critical" {
		return "Blocker"
	}
	switch value {
	case "major", "high":
		return "High"
	case "minor", "medium":
		return "Medium"
	case "low":
		return "Low"
	default:
		return "High"
	}
}

func hasHardStageAFailure(findings []model.Finding, primary scriptExecution) bool {
	if !primary.OK {
		return true
	}
	for _, finding := range findings {
		if finding.Severity == "Blocker" {
			return true
		}
	}
	return false
}

func assignFindingIDs(stage string, findings []model.Finding) []model.Finding {
	counts := map[string]int{}
	for i := range findings {
		findings[i].Stage = stage
		short := severityShort(findings[i].Severity)
		counts[short]++
		findings[i].ID = fmt.Sprintf("P2R-%s-%s-%03d", stage, short, counts[short])
	}
	return findings
}

func severityShort(severity string) string {
	switch strings.ToLower(severity) {
	case "blocker":
		return "BLK"
	case "high":
		return "HIGH"
	case "medium":
		return "MED"
	default:
		return "LOW"
	}
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(content, '\n'), 0o644)
}

func writeText(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func appendText(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(content)
	return err
}

func writePlaceholderPNG(path string) error {
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGOSHzRgAAAAABJRU5ErkJggg==")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func promptLooksEnglish(metadataPath string) bool {
	content, err := os.ReadFile(metadataPath)
	if err != nil {
		return false
	}
	var data map[string]any
	if json.Unmarshal(content, &data) != nil {
		return false
	}
	prompt, ok := data["prompt"].(string)
	if !ok || strings.TrimSpace(prompt) == "" {
		return false
	}
	ascii := 0
	for _, ch := range prompt {
		if ch < 128 {
			ascii++
		}
	}
	return float64(ascii)/float64(len([]rune(prompt))) > 0.9
}

func validationMarkdown(project scanner.Project, required map[string]bool, findings []model.Finding) string {
	var builder strings.Builder
	builder.WriteString("# Validation Report\n\n")
	builder.WriteString("- Task: " + project.TaskID + "\n")
	builder.WriteString("- Path: " + project.Path + "\n\n")
	builder.WriteString("## Required Artifacts\n\n")
	keys := make([]string, 0, len(required))
	for key := range required {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		status := "missing"
		if required[key] {
			status = "present"
		}
		builder.WriteString(fmt.Sprintf("- %s: %s\n", key, status))
	}
	if len(findings) == 0 {
		builder.WriteString("\nNo structural blockers found.\n")
		return builder.String()
	}
	builder.WriteString("\n## Findings\n\n")
	for _, finding := range findings {
		builder.WriteString(fmt.Sprintf("- %s: %s\n", finding.Severity, finding.Title))
	}
	return builder.String()
}

func staticUnavailableReport(stage, profile, projectPath, reason string) string {
	return fmt.Sprintf("# %s\n\nManual Verification Required.\n\nProfile: `%s`\nProject: `%s`\nReason: %s\n\nNo runtime conclusion is made by this fallback artifact.\n", stageName(stage), profile, projectPath, reason)
}

func repairMarkdown(stageStatuses map[string]string, findings []model.Finding) string {
	var builder strings.Builder
	builder.WriteString("# Repair Summary\n\n")
	builder.WriteString("## Stage Statuses\n\n")
	for _, stage := range []string{"A", "B", "C", "D", "E"} {
		builder.WriteString(fmt.Sprintf("- %s: %s\n", stage, stageStatuses[stage]))
	}
	builder.WriteString("\n## Priority Findings\n\n")
	if len(findings) == 0 {
		builder.WriteString("No Blocker/High findings were recorded.\n")
		return builder.String()
	}
	for _, finding := range findings {
		builder.WriteString(fmt.Sprintf("- %s %s: %s\n  Impact: %s\n  Minimum fix: %s\n", finding.Severity, finding.ID, finding.Title, finding.Impact, finding.MinimumFix))
	}
	return builder.String()
}

func shortComment(stageStatuses map[string]string, findings []model.Finding) string {
	runtime := fmt.Sprintf("1.<可运行性结论: B=%s, C=%s. Runtime conclusions are based only on collected B/C artifacts or explicit missing evidence.>", stageStatuses["B"], stageStatuses["C"])
	blocker, high := countSeverity(findings, "Blocker"), countSeverity(findings, "High")
	match := fmt.Sprintf("2.<验收匹配度结论: Static acceptance requires human review; recorded Blocker=%d, High=%d from pipeline findings.>", blocker, high)
	risk := "3.<最高风险问题: No Blocker/High finding recorded.>"
	if len(findings) > 0 {
		top := highestRisk(findings)
		risk = fmt.Sprintf("3.<最高风险问题: %s %s - %s>", top.ID, top.Title, top.Impact)
	}
	return runtime + "\n" + match + "\n" + risk + "\n<[ ] PASS  [ ] REWORK  [ ] FAIL>\n"
}

func extractFindingsFromReport(stage, report, sourcePath string) []model.Finding {
	var findings []model.Finding
	seen := map[string]bool{}
	for index, line := range strings.Split(report, "\n") {
		title := strings.TrimSpace(strings.TrimLeft(line, "#-*0123456789.> \t"))
		if title == "" {
			continue
		}
		lower := strings.ToLower(title)
		if strings.Contains(lower, "no blocker") || strings.Contains(lower, "no high") || strings.Contains(lower, "no issue") {
			continue
		}
		severity := ""
		switch {
		case strings.Contains(lower, "blocker") || strings.Contains(lower, "critical"):
			severity = "Blocker"
		case strings.Contains(lower, "high"):
			severity = "High"
		case strings.Contains(lower, "medium"):
			severity = "Medium"
		case strings.Contains(lower, "low"):
			severity = "Low"
		}
		if severity == "" {
			continue
		}
		key := severity + title
		if seen[key] {
			continue
		}
		seen[key] = true
		findings = append(findings, model.Finding{
			Stage:      stage,
			Severity:   severity,
			Title:      title,
			Rule:       "Static review report finding",
			Evidence:   fmt.Sprintf("%s:%d", sourcePath, index+1),
			Impact:     "See the static review report for full context.",
			MinimumFix: "Review the cited report item and repair the delivery package.",
			SourcePath: sourcePath,
		})
	}
	return findings
}

func truncateString(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "\n[truncated]\n"
}

func countSeverity(findings []model.Finding, severity string) int {
	count := 0
	for _, finding := range findings {
		if finding.Severity == severity {
			count++
		}
	}
	return count
}

func highestRisk(findings []model.Finding) model.Finding {
	if len(findings) == 0 {
		return model.Finding{}
	}
	sortFindings(findings)
	return findings[0]
}

func sortFindings(findings []model.Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		ri := severityRank(findings[i].Severity)
		rj := severityRank(findings[j].Severity)
		if ri != rj {
			return ri < rj
		}
		return findings[i].ID < findings[j].ID
	})
}

func severityRank(severity string) int {
	switch strings.ToLower(severity) {
	case "blocker":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	default:
		return 3
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func readmes(repoPath string) []string {
	matches, _ := filepath.Glob(filepath.Join(repoPath, "README*"))
	return matches
}

type portMapping struct {
	Service   string `json:"service"`
	URL       string `json:"url,omitempty"`
	Host      int    `json:"host"`
	Container int    `json:"container"`
	Protocol  string `json:"protocol,omitempty"`
}

type probeResult struct {
	Service string `json:"service"`
	URL     string `json:"url"`
	OK      bool   `json:"ok"`
	Status  int    `json:"status,omitempty"`
	Error   string `json:"error,omitempty"`
}

type composePSService struct {
	Service    string `json:"Service"`
	Name       string `json:"Name"`
	Publishers []struct {
		URL           string `json:"URL"`
		TargetPort    int    `json:"TargetPort"`
		PublishedPort int    `json:"PublishedPort"`
		Protocol      string `json:"Protocol"`
	} `json:"Publishers"`
}

func findCompose(repoPath string) string {
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		path := filepath.Join(repoPath, name)
		if fileExists(path) {
			return path
		}
	}
	return ""
}

func readmeComposeCommand(repoPath string) []string {
	readmeFiles := readmes(repoPath)
	for _, readme := range readmeFiles {
		content, err := os.ReadFile(readme)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(content), "\n") {
			line = strings.TrimSpace(line)
			line = strings.TrimPrefix(line, "$")
			line = strings.TrimPrefix(line, ">")
			line = strings.TrimSpace(strings.Trim(line, "`"))
			lower := strings.ToLower(line)
			if !(strings.Contains(lower, "docker compose") || strings.Contains(lower, "docker-compose")) || !strings.Contains(lower, " up") {
				continue
			}
			fields := strings.Fields(line)
			clean := make([]string, 0, len(fields))
			for _, field := range fields {
				if field == "&&" || field == ";" || field == "|" {
					break
				}
				clean = append(clean, field)
			}
			if len(clean) >= 3 && clean[0] == "docker" && clean[1] == "compose" {
				return clean
			}
			if len(clean) >= 2 && clean[0] == "docker-compose" {
				return append([]string{"docker", "compose"}, clean[1:]...)
			}
		}
	}
	return nil
}

func composeArgsWithProject(fields []string, projectName string) []string {
	args := append([]string{}, fields[1:]...)
	commandIndex := composeCommandIndex(args)
	if commandIndex < 0 {
		commandIndex = len(args)
	}
	if !hasFlag(args, "-p", "--project-name") {
		args = append(args[:commandIndex], append([]string{"-p", projectName}, args[commandIndex:]...)...)
		commandIndex += 2
	}
	if !hasFlag(args, "-d", "--detach") {
		upIndex := indexOf(args, "up")
		if upIndex >= 0 {
			args = append(args, "-d")
		}
	}
	return args
}

func composePSArgs(fields []string, projectName string) []string {
	args := append([]string{}, fields[1:]...)
	commandIndex := composeCommandIndex(args)
	if commandIndex < 0 {
		commandIndex = len(args)
	}
	globals := append([]string{}, args[:commandIndex]...)
	if !hasFlag(globals, "-p", "--project-name") {
		globals = append(globals, "-p", projectName)
	}
	return append(globals, "ps", "--format", "json")
}

func composePSQArgs(fields []string, projectName string) []string {
	globals := composeGlobals(fields, projectName)
	return append(globals, "ps", "-q")
}

func composeServicesArgs(fields []string, projectName string) []string {
	globals := composeGlobals(fields, projectName)
	return append(globals, "config", "--services")
}

func composeGlobals(fields []string, projectName string) []string {
	args := append([]string{}, fields[1:]...)
	commandIndex := composeCommandIndex(args)
	if commandIndex < 0 {
		commandIndex = len(args)
	}
	globals := append([]string{}, args[:commandIndex]...)
	if !hasFlag(globals, "-p", "--project-name") {
		globals = append(globals, "-p", projectName)
	}
	return globals
}

func composeCommandIndex(args []string) int {
	for i, arg := range args {
		switch arg {
		case "up", "start", "run", "ps", "exec":
			return i
		}
	}
	return -1
}

func splitNonEmptyLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func hasFlag(args []string, names ...string) bool {
	for _, arg := range args {
		for _, name := range names {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				return true
			}
		}
	}
	return false
}

func indexOf(args []string, value string) int {
	for i, arg := range args {
		if arg == value {
			return i
		}
	}
	return -1
}

func parseComposePS(raw string) (map[string][]portMapping, []string) {
	mappings := map[string][]portMapping{}
	serviceSet := map[string]bool{}
	var services []composePSService
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return mappings, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		_ = json.Unmarshal([]byte(trimmed), &services)
	} else {
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var service composePSService
			if json.Unmarshal([]byte(line), &service) == nil {
				services = append(services, service)
			}
		}
	}
	var serviceNames []string
	for _, service := range services {
		name := service.Service
		if name == "" {
			name = service.Name
		}
		if name == "" {
			continue
		}
		if !serviceSet[name] {
			serviceNames = append(serviceNames, name)
			serviceSet[name] = true
		}
		for _, publisher := range service.Publishers {
			if publisher.PublishedPort == 0 {
				continue
			}
			mappings[name] = append(mappings[name], portMapping{
				Service:   name,
				URL:       publisher.URL,
				Host:      publisher.PublishedPort,
				Container: publisher.TargetPort,
				Protocol:  publisher.Protocol,
			})
		}
	}
	return mappings, serviceNames
}

func probeMappings(mappings map[string][]portMapping, timeout time.Duration) []probeResult {
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	client := http.Client{Timeout: timeout}
	var results []probeResult
	for service, ports := range mappings {
		for _, mapping := range ports {
			if mapping.Host == 0 {
				continue
			}
			url := "http://127.0.0.1:" + strconv.Itoa(mapping.Host)
			response, err := client.Get(url)
			item := probeResult{Service: service, URL: url}
			if err != nil {
				item.Error = err.Error()
			} else {
				item.OK = response.StatusCode >= 200 && response.StatusCode < 500
				item.Status = response.StatusCode
				_ = response.Body.Close()
			}
			results = append(results, item)
		}
	}
	return results
}

func parseDockerPort(service, raw string) []portMapping {
	var mappings []portMapping
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "->") {
			continue
		}
		parts := strings.SplitN(line, "->", 2)
		left := strings.TrimSpace(parts[0])
		right := strings.TrimSpace(parts[1])
		containerPort := 0
		protocol := ""
		if portParts := strings.SplitN(left, "/", 2); len(portParts) == 2 {
			containerPort, _ = strconv.Atoi(portParts[0])
			protocol = portParts[1]
		}
		lastColon := strings.LastIndex(right, ":")
		if lastColon < 0 {
			continue
		}
		hostText := right[lastColon+1:]
		hostPort, _ := strconv.Atoi(hostText)
		if hostPort == 0 {
			continue
		}
		mappings = append(mappings, portMapping{
			Service:   service,
			URL:       strings.TrimSuffix(right[:lastColon], ":"),
			Host:      hostPort,
			Container: containerPort,
			Protocol:  protocol,
		})
	}
	return mappings
}

type runtimeEvidence struct {
	ComposeProject string   `json:"compose_project"`
	ComposeFile    string   `json:"compose_file"`
	WorkDir        string   `json:"work_dir"`
	Services       []string `json:"services"`
}

func readRuntimeEvidence(path string) (runtimeEvidence, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return runtimeEvidence{}, err
	}
	var evidence runtimeEvidence
	if err := json.Unmarshal(content, &evidence); err != nil {
		return runtimeEvidence{}, err
	}
	return evidence, nil
}

func containerRunTestsCommand() string {
	return `for d in . /app /workspace /repo; do if [ -d "$d" ]; then cd "$d" 2>/dev/null || true; fi; if [ -f run_tests.sh ]; then sh run_tests.sh; exit $?; fi; if [ -f run_tests.py ]; then python run_tests.py; exit $?; fi; if [ -f run_tests.ps1 ] && command -v pwsh >/dev/null 2>&1; then pwsh -File run_tests.ps1; exit $?; fi; done; echo "run_tests.* not found inside container"; exit 127`
}

func findRunTests(repoPath string, exec executor.Runner) (string, string, []string) {
	candidates := []string{"run_tests.sh", "run_tests.ps1", "run_tests.py"}
	for _, name := range candidates {
		path := filepath.Join(repoPath, name)
		if !fileExists(path) {
			continue
		}
		switch filepath.Ext(path) {
		case ".sh":
			if shell, err := exec.LookPath("bash"); err == nil {
				return path, shell, []string{path}
			}
			if shell, err := exec.LookPath("sh"); err == nil {
				return path, shell, []string{path}
			}
		case ".ps1":
			return path, "powershell", []string{"-ExecutionPolicy", "Bypass", "-File", path}
		case ".py":
			if py, err := exec.LookPath("python"); err == nil {
				return path, py, []string{path}
			}
			if py, err := exec.LookPath("python3"); err == nil {
				return path, py, []string{path}
			}
		}
	}
	return "", "", nil
}
