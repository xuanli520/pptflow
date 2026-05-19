package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
		evidence := "Package spec violation: repo/run_tests.sh was not found. Stage C uses the host run_tests.sh entrypoint only."
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
	if !runtime.HasServiceMappings() {
		evidence := "Stage B runtime evidence is missing port mappings. Run Stage B successfully before Stage C."
		recordRequiredEvidence(evidence, stageCRuntimeSummary(false, evidence, runtime, prior, nil))
		record.Findings = append(record.Findings, model.Finding{
			Stage:      "C",
			Severity:   "High",
			Title:      "Stage B runtime evidence is missing",
			Rule:       "Stage C requires Stage B port_map.json mappings.",
			Evidence:   evidence,
			Impact:     "Runtime test evidence cannot be collected from host service URLs.",
			MinimumFix: "Rerun B successfully and ensure published ports are recorded.",
		})
		if record.ErrorSummary == "" {
			record.ErrorSummary = evidence
		}
		return finishStage(record, model.StageFailed, start)
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
	stageEnv := stageCEnvironment(runtime)
	env := runtimeCommandEnv(stageEnv.Env)
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
	fmt.Fprintln(logFile)
	onOutput := func(line string, source string) {
		appendStreamProgress(run.RunID, "C", line, source, false, progress)
	}
	result := r.exec.RunStreamingWithOutput(ctx, timeout, repoPath, env, logFile, onOutput, bash, "run_tests.sh")
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
	record = requiredStageJSON(record, writer, writer.RelativePath(summaryPath), stageCRuntimeSummary(result.Err == nil, "", runtime, prior, map[string]any{"exit_code": result.ExitCode, "timeout": result.Timeout, "command": "bash run_tests.sh", "env_keys": stageEnv.Keys, "runtime_env": stageEnv.Values, "service_urls": stageEnv.Service.Mapping}))
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
