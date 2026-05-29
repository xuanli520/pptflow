package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func (r Runner) stageC(ctx context.Context, run model.RunRecord, project scanner.Project, runtime RuntimeState, prior map[string]model.StageRecord, progress func(RunProgress)) model.StageRecord {
	start := time.Now()
	record := startStage("C")
	logPath := filepath.Join(run.ArtifactRoot, "logs", "C_tests.log")
	screenshotPath := qaArtifactPath(run.ArtifactRoot, "run_tests_screenshot.png")
	summaryPath := filepath.Join(run.ArtifactRoot, "test_runtime_summary.json")
	record.LogPath = logPath
	record.ArtifactPaths = append(record.ArtifactPaths, logPath, screenshotPath, summaryPath)
	writer := NewArtifactWriter(run.ArtifactRoot)
	recordRequiredEvidence := func(evidence string, summary map[string]any) {
		record = requiredStageText(record, writer, writer.RelativePath(logPath), evidence)
		pages, err := renderLogFile(logPath, screenshotPath)
		if err != nil {
			record = recordArtifactWriteError(record, err, screenshotPath)
		}
		record.ArtifactPaths = append([]string{logPath}, pages...)
		record.ArtifactPaths = append(record.ArtifactPaths, summaryPath)
		record = requiredStageJSON(record, writer, writer.RelativePath(summaryPath), summary)
	}
	repoPath := filepath.Join(project.Path, "repo")
	script := filepath.Join(repoPath, "run_tests.sh")
	if !fileExists(script) {
		evidence := "Package spec violation: repo/run_tests.sh was not found. Stage C requires the repo/run_tests.sh entrypoint."
		recordRequiredEvidence(evidence, stageCRuntimeSummary(false, evidence, runtime, prior, nil))
		record.Findings = append(record.Findings, model.Finding{
			Stage:      "C",
			Severity:   "High",
			Title:      "Unified test entrypoint is missing",
			Rule:       "Stage C requires repo/run_tests.sh.",
			Evidence:   evidence,
			Impact:     "Runtime test evidence cannot be collected.",
			MinimumFix: "Add a runnable repo/run_tests.sh entrypoint.",
		})
		if record.ErrorSummary == "" {
			record.ErrorSummary = evidence
		}
		return finishStage(record, model.StageFailed, start)
	}
	if !runtime.HasCleanupTarget() {
		evidence := "Stage B runtime evidence is missing. Run Stage B successfully before Stage C."
		recordRequiredEvidence(evidence, stageCRuntimeSummary(false, evidence, runtime, prior, nil))
		record.Findings = append(record.Findings, model.Finding{
			Stage:      "C",
			Severity:   "High",
			Title:      "Stage B runtime evidence is missing",
			Rule:       "Stage C requires Stage B Docker runtime metadata.",
			Evidence:   evidence,
			Impact:     "Runtime test evidence cannot be collected.",
			MinimumFix: "Rerun B successfully and ensure Docker starts.",
		})
		if record.ErrorSummary == "" {
			record.ErrorSummary = evidence
		}
		return finishStage(record, model.StageFailed, start)
	}
	if strings.EqualFold(strings.TrimSpace(r.cfg.Pipeline.StageC.Execution), "isolated") {
		return r.stageCIsolated(ctx, run, project, runtime, prior, progress)
	}
	bash := findHostBash(r.exec)
	if bash == "" {
		evidence := "bash executable not found on PATH. Stage C requires host bash to run repo/run_tests.sh."
		recordRequiredEvidence(evidence, stageCRuntimeSummary(false, evidence, runtime, prior, nil))
		record.Findings = append(record.Findings, model.Finding{
			Stage:      "C",
			Severity:   "High",
			Title:      "bash unavailable for host runtime tests",
			Rule:       "Stage C must execute repo/run_tests.sh on the host.",
			Evidence:   evidence,
			Impact:     "Runtime test evidence cannot be collected.",
			MinimumFix: "Install bash, such as Git Bash on Windows, and rerun C.",
		})
		if record.ErrorSummary == "" {
			record.ErrorSummary = evidence
		}
		return finishStage(record, model.StageFailed, start)
	}
	composeUsage := inspectRunTestsCompose(repoPath)
	runTestsOwnsCompose := composeUsage.StartsStack
	stageEnv := stageCEnvironment(runtime)
	if runTestsOwnsCompose {
		stageEnv = stageEnv.withoutComposeVars()
		stageEnv.add("COMPOSE_PROJECT_NAME", stageCTestComposeProject(r.cfg.Docker.ComposeProjectPrefix, run.TaskID, run.RunID))
		if composeFiles := runTestsHostComposeFiles(runtime); len(composeFiles) > 0 {
			stageEnv.add("COMPOSE_FILE", strings.Join(composeFiles, string(os.PathListSeparator)))
		}
	}
	envExtra := append([]string{}, stageEnv.Env...)
	var envFileValues []string
	var envFileWarnings []string
	if composeUsage.Uses {
		envFileValues, envFileWarnings = runtimeEnvFileValues(runtime.EnvFiles)
		envExtra = append(envExtra, envFileValues...)
	}
	env := runtimeCommandEnv(envExtra)
	if composeUsage.Uses {
		env = dockerRuntimeCommandEnv(envExtra)
	}
	timeout := r.stageTimeout("C", 300)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		record = recordArtifactWriteError(record, err, logPath)
		return finishStage(record, model.StageFailed, start)
	}
	fmt.Fprintln(logFile, "=== C host run_tests.sh start ===")
	appendStreamProgress(run.RunID, "C", "=== C host run_tests.sh start ===", "p2r", false, progress)
	fmt.Fprintln(logFile, "Stage C injected runtime env:")
	appendStreamProgress(run.RunID, "C", "Stage C injected runtime env:", "p2r", false, progress)
	for _, key := range stageEnv.Keys {
		line := fmt.Sprintf("%s=%s", key, stageEnv.Values[key])
		fmt.Fprintln(logFile, line)
		appendStreamProgress(run.RunID, "C", line, "p2r", false, progress)
	}
	for _, warning := range envFileWarnings {
		fmt.Fprintln(logFile, "Stage C env warning: "+warning)
	}
	fmt.Fprintln(logFile)
	onOutput := func(line string, source string) {
		appendStreamProgress(run.RunID, "C", line, source, false, progress)
	}
	result := r.exec.RunStreamingWithOutput(ctx, timeout, repoPath, env, logFile, onOutput, bash, "run_tests.sh")
	var composeCleanup *stageCTestComposeCleanup
	if runTestsOwnsCompose {
		cleanup := r.cleanupStageCTestCompose(ctx, repoPath, env, logFile, composeUsage)
		composeCleanup = &cleanup
	}
	cleanup := cleanupStageCTestArtifacts(repoPath)
	cleanup.Compose = composeCleanup
	for _, removed := range cleanup.Removed {
		fmt.Fprintln(logFile, "Stage C cleaned test artifact: "+removed)
	}
	for _, warning := range cleanup.Warnings {
		fmt.Fprintln(logFile, "Stage C cleanup warning: "+warning)
	}
	endLine := fmt.Sprintf("=== C host run_tests.sh end: exit=%d timeout=%t err=%v ===", result.ExitCode, result.Timeout, result.Err)
	fmt.Fprintf(logFile, "\n%s\n", endLine)
	appendStreamProgress(run.RunID, "C", endLine, "p2r", true, progress)
	_ = logFile.Close()
	log := result.Command + "\n\nSTDOUT:\n" + result.Stdout + "\nSTDERR:\n" + result.Stderr
	if strings.TrimSpace(result.Stdout+result.Stderr) == "" {
		bestEffortStageAppend(&record, writer, writer.RelativePath(logPath), log)
	}
	pages, renderErr := renderLogFile(logPath, screenshotPath)
	if renderErr != nil {
		record = recordArtifactWriteError(record, renderErr, screenshotPath)
	}
	record.ArtifactPaths = append([]string{logPath}, pages...)
	record.ArtifactPaths = append(record.ArtifactPaths, summaryPath)
	summaryExtra := map[string]any{"exit_code": result.ExitCode, "timeout": result.Timeout, "command": "bash run_tests.sh", "env_keys": stageEnv.Keys, "runtime_env": stageEnv.Values, "service_urls": stageEnv.Service.Mapping, "cleanup": cleanup}
	if len(envFileWarnings) > 0 {
		summaryExtra["env_file_warnings"] = envFileWarnings
	}
	record = requiredStageJSON(record, writer, writer.RelativePath(summaryPath), stageCRuntimeSummary(result.Err == nil, "", runtime, prior, summaryExtra))
	if result.Err != nil {
		record.Findings = append(record.Findings, model.Finding{
			Stage:      "C",
			Severity:   "High",
			Title:      "run_tests runtime evidence failed",
			Rule:       "Stage C must execute the unified test entrypoint successfully.",
			Evidence:   stageCTrimResult(result),
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

type stageCTestArtifactCleanup struct {
	Removed  []string                  `json:"removed"`
	Warnings []string                  `json:"warnings,omitempty"`
	Compose  *stageCTestComposeCleanup `json:"compose,omitempty"`
}

type stageCTestComposeCleanup struct {
	Command       string `json:"command,omitempty"`
	ExitCode      int    `json:"exit_code,omitempty"`
	Stdout        string `json:"stdout,omitempty"`
	Stderr        string `json:"stderr,omitempty"`
	Error         string `json:"error,omitempty"`
	SkippedReason string `json:"skipped_reason,omitempty"`
}

func (r Runner) cleanupStageCTestCompose(ctx context.Context, repoPath string, env []string, logFile *os.File, usage runTestsComposeUsage) stageCTestComposeCleanup {
	if usage.ExplicitProject {
		reason := "run_tests.sh uses an explicit Docker Compose project; relying on its own cleanup trap"
		if logFile != nil {
			fmt.Fprintln(logFile, "Stage C compose cleanup skipped: "+reason)
		}
		return stageCTestComposeCleanup{SkippedReason: reason}
	}
	if ctx == nil || ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
	}
	result := r.exec.Run(ctx, 2*time.Minute, repoPath, env, "docker", "compose", "down", "--volumes", "--remove-orphans")
	cleanup := stageCTestComposeCleanup{
		Command:  result.Command,
		ExitCode: result.ExitCode,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
	}
	if result.Err != nil {
		cleanup.Error = result.Err.Error()
	}
	if logFile != nil {
		fmt.Fprintln(logFile, "Stage C compose cleanup: "+result.Command)
		if strings.TrimSpace(result.Stdout) != "" {
			fmt.Fprintln(logFile, result.Stdout)
		}
		if strings.TrimSpace(result.Stderr) != "" {
			fmt.Fprintln(logFile, result.Stderr)
		}
		if result.Err != nil {
			fmt.Fprintln(logFile, "Stage C compose cleanup warning: "+stageCTrimResult(result))
		}
	}
	return cleanup
}

var stageCTestArtifactNames = map[string]bool{
	".coverage":         true,
	".nyc_output":       true,
	".pytest_cache":     true,
	".venv-tests":       true,
	"__pycache__":       true,
	"playwright-report": true,
	"test-results":      true,
}

var stageCTestArtifactTraversalSkips = map[string]bool{
	".git":         true,
	".venv":        true,
	"node_modules": true,
	"venv":         true,
}

func cleanupStageCTestArtifacts(repoPath string) stageCTestArtifactCleanup {
	var cleanup stageCTestArtifactCleanup
	repoPath = filepath.Clean(repoPath)
	err := filepath.WalkDir(repoPath, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			cleanup.Warnings = append(cleanup.Warnings, err.Error())
			return nil
		}
		if path == repoPath {
			return nil
		}
		name := entry.Name()
		if entry.IsDir() && stageCTestArtifactTraversalSkips[name] {
			return filepath.SkipDir
		}
		if !stageCTestArtifactNames[name] {
			return nil
		}
		rel, relErr := filepath.Rel(repoPath, path)
		if relErr != nil {
			rel = path
		}
		if removeErr := os.RemoveAll(path); removeErr != nil {
			cleanup.Warnings = append(cleanup.Warnings, rel+": "+removeErr.Error())
			return nil
		}
		cleanup.Removed = append(cleanup.Removed, filepath.ToSlash(rel))
		if entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		cleanup.Warnings = append(cleanup.Warnings, err.Error())
	}
	return cleanup
}

func runTestsHostComposeFiles(runtime RuntimeState) []string {
	runtime.Normalize()
	files := runtime.ComposeFiles
	if len(files) == 0 && strings.TrimSpace(runtime.ComposeFile) != "" {
		files = []string{runtime.ComposeFile}
	}
	result := make([]string, 0, len(files))
	for _, file := range files {
		switch filepath.Base(file) {
		case "compose.env.yml", "compose.ports.yml", "runtime_labels.compose.yml", "stage_c.runner.override.yml":
			continue
		}
		result = append(result, file)
	}
	return result
}

func stageCTestComposeProject(prefix, taskID, runID string) string {
	return dockermgr.ComposeProjectName(prefix, taskID, runID+"-tests")
}

func stageCRuntimeSummary(ok bool, reason string, runtime RuntimeState, prior map[string]model.StageRecord, extra map[string]any) map[string]any {
	classification := stageCRuntimeEvidenceClassification(runtime, prior)
	summary := map[string]any{
		"ok":                              ok,
		"reason":                          reason,
		"mode":                            "host",
		"script":                          "repo/run_tests.sh",
		"compose_project":                 runtime.ComposeProject,
		"compose_files":                   runtime.ComposeFiles,
		"build_mirror_enabled":            runtime.Mirror.BuildMirrorEnabled,
		"build_mirror_mode":               runtime.Mirror.BuildMirrorMode,
		"build_mirror_fallback_used":      runtime.Mirror.BuildMirrorFallbackUsed,
		"runtime_evidence_classification": classification,
	}
	if len(runtime.ComposeFiles) == 0 && runtime.ComposeFile != "" {
		summary["compose_files"] = []string{runtime.ComposeFile}
	}
	for key, value := range extra {
		summary[key] = value
	}
	return summary
}

func stageCRuntimeEvidenceClassification(runtime RuntimeState, prior map[string]model.StageRecord) string {
	if stageB, ok := prior[string(model.StageB)]; ok && stageB.Status == model.StageFailed {
		return "stage_b_failed"
	}
	if !runtime.HasCleanupTarget() {
		return "missing_runtime_evidence"
	}
	if !runtime.HasServiceMappings() {
		return "missing_port_mapping"
	}
	return "ok"
}

func findHostBash(exec CommandRunner) string {
	if path, err := exec.LookPath("bash"); err == nil {
		return path
	}
	for _, candidate := range []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files\Git\usr\bin\bash.exe`,
		`C:\msys64\usr\bin\bash.exe`,
		"/usr/bin/bash",
		"/bin/bash",
	} {
		if fileExists(candidate) {
			return candidate
		}
	}
	return ""
}
