package pipeline

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/projectlayout"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func (r Runner) stageA(ctx context.Context, run model.RunRecord, project scanner.Project, progress func(RunProgress)) model.StageRecord {
	start := time.Now()
	record := startStage("A")
	logPath := filepath.Join(run.ArtifactRoot, "logs", "A_validate.log")
	record.LogPath = logPath
	writer := NewArtifactWriter(run.ArtifactRoot)

	scriptRoot := project.Path
	snapshotPath := filepath.Join(run.ArtifactRoot, "script_input_snapshot")
	snapshotErr := copyPackageSnapshot(project.Path, snapshotPath)
	if snapshotErr == nil {
		scriptRoot = snapshotPath
	}

	validation := projectlayout.ValidatePackageRoot(project.Path)
	required := map[string]bool{
		"docs":                    !containsString(validation.Missing, "docs/"),
		"repo":                    !containsString(validation.Missing, "repo/"),
		"original_session_marker": validation.OriginalSessionMarker != "",
		"metadata.json":           !containsString(validation.Missing, "metadata.json"),
	}
	findings := structuralFindings(project, required)
	if snapshotErr != nil {
		findings = append(findings, model.Finding{
			Stage:      "A",
			Severity:   "Low",
			Title:      "Script input snapshot could not be created",
			Rule:       "Stage A scripts should inspect the delivery package without prior p2r QA artifacts.",
			Evidence:   snapshotErr.Error(),
			Impact:     "Checks may include previous qa/ artifacts and produce noisy findings.",
			MinimumFix: "Ensure the run artifact directory is writable and rerun Stage A.",
		})
	}

	acceptancePath := filepath.Join(run.ArtifactRoot, "acceptance.json")
	acceptanceReportPath := qaArtifactPath(run.ArtifactRoot, "acceptance_report.md")
	validationReportPath := qaArtifactPath(run.ArtifactRoot, "validation_report.md")
	trajectoryPath := qaArtifactPath(run.ArtifactRoot, "trajectory_archive.png")
	requiredPath := filepath.Join(run.ArtifactRoot, "required_artifacts.json")
	readmeAlignmentPath := filepath.Join(run.ArtifactRoot, "readme_alignment.json")
	localDependencyPath := filepath.Join(run.ArtifactRoot, "local_dependency.json")
	fakeImplPath := filepath.Join(run.ArtifactRoot, "fake_impl.json")
	testsInspectionPath := filepath.Join(run.ArtifactRoot, "tests_inspection.json")
	englishOnlyPath := filepath.Join(run.ArtifactRoot, "english_only.json")

	scriptResults := r.runStageAScripts(ctx, project, scriptRoot, logPath, writer, &record, map[string]string{
		"acceptance":       acceptancePath,
		"acceptance_md":    acceptanceReportPath,
		"validation_md":    validationReportPath,
		"required":         requiredPath,
		"readme_alignment": readmeAlignmentPath,
		"local_dependency": localDependencyPath,
		"fake_impl":        fakeImplPath,
		"tests_inspection": testsInspectionPath,
		"english_only":     englishOnlyPath,
	})

	if !fileExists(acceptancePath) {
		record = requiredStageJSON(record, writer, writer.RelativePath(acceptancePath), scriptResults["run_acceptance.py"])
	}
	if result, ok := scriptResults["run_acceptance.py"]; ok && !result.OK {
		findings = append(findings, model.Finding{
			Stage:      "A",
			Severity:   "Low",
			Title:      "run_acceptance.py did not complete cleanly",
			Rule:       "Stage A must collect primary acceptance evidence from the bundled script.",
			Evidence:   result.summary(),
			Impact:     "Primary acceptance evidence may be incomplete.",
			MinimumFix: "Ensure Python/uv can run the embedded scripts and rerun Stage A.",
			SourcePath: acceptancePath,
		})
	}
	if result, ok := scriptResults["run_validate.py"]; ok && !result.OK {
		findings = append(findings, model.Finding{
			Stage:      "A",
			Severity:   "High",
			Title:      "run_validate.py did not complete cleanly",
			Rule:       "Stage A must collect " + filepath.Base(validationReportPath) + " from the bundled validation wrapper.",
			Evidence:   result.summary(),
			Impact:     "The validation markdown report may be incomplete or missing.",
			MinimumFix: "Ensure Python/uv can run assets/scripts/run_validate.py and rerun Stage A.",
			SourcePath: validationReportPath,
		})
	}
	findings = append(findings, acceptanceFindings(acceptancePath)...)
	if !fileExists(validationReportPath) {
		findings = append(findings, missingValidationReportFinding(validationReportPath, scriptResults["run_validate.py"]))
	}
	trajectoryPages, trajectoryErr := renderTerminalLog(packageTrajectorySummary(project.Path, scriptRoot, snapshotErr), trajectoryPath)
	if trajectoryErr != nil {
		record = recordArtifactWriteError(record, trajectoryErr, trajectoryPath)
	}

	artifactPaths := []string{acceptancePath, acceptanceReportPath, requiredPath, readmeAlignmentPath, localDependencyPath}
	if fileExists(validationReportPath) {
		artifactPaths = append(artifactPaths, validationReportPath)
	}
	artifactPaths = append(artifactPaths, trajectoryPages...)
	for _, path := range []string{fakeImplPath, testsInspectionPath, englishOnlyPath} {
		if fileExists(path) {
			artifactPaths = append(artifactPaths, path)
		}
	}
	record.ArtifactPaths = artifactPaths
	record.Findings = append(record.Findings, findings...)
	bestEffortStageAppend(&record, writer, writer.RelativePath(logPath), "\n\n"+validationMarkdown(project, required, findings))

	status := model.StageDone
	if hasHardStageAFailure(record.Findings, scriptResults["run_acceptance.py"], scriptResults["run_validate.py"]) {
		status = model.StageFailed
		if record.ErrorSummary == "" {
			record.ErrorSummary = fmt.Sprintf("%d validation finding(s)", len(record.Findings))
		}
	}
	return finishStage(record, status, start)
}

func packageTrajectorySummary(projectPath, scriptRoot string, snapshotErr error) string {
	lines := make([]string, 0, terminalScreenshotMaxLines)
	truncated := false
	appendLine := func(line string) bool {
		if len(lines) >= terminalScreenshotMaxLines {
			truncated = true
			return false
		}
		lines = append(lines, line)
		return true
	}

	appendLine("Stage A trajectory archive internal structure")
	appendLine("source: zip archive contents under the original session marker")
	appendLine("project_path: " + projectPath)
	appendLine("script_input_root: " + scriptRoot)
	if snapshotErr != nil {
		appendLine("snapshot_error: " + snapshotErr.Error())
	} else {
		appendLine("snapshot_status: copied")
	}

	ok, marker := projectlayout.HasOriginalSessionMarker(projectPath)
	if !ok {
		appendLine("original_session_marker: missing")
		return strings.Join(lines, "\n") + "\n"
	}
	markerPath := filepath.Join(projectPath, marker)
	appendLine("original_session_marker: " + filepath.ToSlash(marker))
	archives, err := trajectoryZipArchives(markerPath)
	if err != nil {
		appendLine("archive_scan_error: " + err.Error())
		return strings.Join(lines, "\n") + "\n"
	}
	if len(archives) == 0 {
		appendLine("archive_count: 0")
		appendLine("archive: no .zip files found under " + filepath.ToSlash(marker))
		return strings.Join(lines, "\n") + "\n"
	}
	appendLine(fmt.Sprintf("archive_count: %d", len(archives)))
	for _, archivePath := range archives {
		if !appendLine("") {
			break
		}
		appendLine("archive: " + displayPathRelativeTo(projectPath, archivePath))
		treeLines, err := zipArchiveTreeLines(archivePath)
		if err != nil {
			appendLine("archive_error: " + err.Error())
			continue
		}
		for _, line := range treeLines {
			if !appendLine(line) {
				break
			}
		}
		if truncated {
			break
		}
	}
	if truncated && len(lines) > 0 {
		lines[len(lines)-1] = "... truncated"
	}
	return strings.Join(lines, "\n") + "\n"
}

func trajectoryZipArchives(markerPath string) ([]string, error) {
	var archives []string
	err := filepath.WalkDir(markerPath, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(entry.Name()), ".zip") {
			archives = append(archives, path)
		}
		return nil
	})
	sort.Strings(archives)
	return archives, err
}

func zipArchiveTreeLines(archivePath string) ([]string, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	root := newArchiveTreeNode()
	entryCount := 0
	for _, file := range reader.File {
		name := cleanZipEntryName(file.Name)
		if name == "" {
			continue
		}
		entryCount++
		parts := strings.Split(name, "/")
		node := root
		for index, part := range parts {
			node = node.child(part)
			if index < len(parts)-1 {
				node.dir = true
				continue
			}
			if file.FileInfo().IsDir() || strings.HasSuffix(file.Name, "/") {
				node.dir = true
			} else {
				node.file = true
			}
		}
	}

	lines := []string{fmt.Sprintf("entries: %d", entryCount), "/"}
	appendArchiveTreeNode(&lines, root, "")
	return lines, nil
}

type archiveTreeNode struct {
	children map[string]*archiveTreeNode
	dir      bool
	file     bool
}

func newArchiveTreeNode() *archiveTreeNode {
	return &archiveTreeNode{children: map[string]*archiveTreeNode{}}
}

func (node *archiveTreeNode) child(name string) *archiveTreeNode {
	child, ok := node.children[name]
	if !ok {
		child = newArchiveTreeNode()
		node.children[name] = child
	}
	return child
}

func appendArchiveTreeNode(lines *[]string, node *archiveTreeNode, prefix string) {
	names := make([]string, 0, len(node.children))
	for name := range node.children {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		left := node.children[names[i]]
		right := node.children[names[j]]
		if (left.dir || len(left.children) > 0) != (right.dir || len(right.children) > 0) {
			return left.dir || len(left.children) > 0
		}
		return names[i] < names[j]
	})

	for index, name := range names {
		child := node.children[name]
		connector := "|-- "
		nextPrefix := prefix + "|   "
		if index == len(names)-1 {
			connector = "`-- "
			nextPrefix = prefix + "    "
		}
		displayName := name
		if child.dir || len(child.children) > 0 {
			displayName += "/"
		}
		*lines = append(*lines, prefix+connector+displayName)
		appendArchiveTreeNode(lines, child, nextPrefix)
	}
}

func cleanZipEntryName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.Trim(name, "/")
	if name == "" {
		return ""
	}
	name = pathpkg.Clean(name)
	if name == "." {
		return ""
	}
	return name
}

func displayPathRelativeTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return path
	}
	return filepath.ToSlash(rel)
}

func structuralFindings(project scanner.Project, required map[string]bool) []model.Finding {
	findings := []model.Finding{}
	for name, ok := range required {
		if !ok {
			rule := "A package must contain docs/, repo/, an original session marker, and metadata.json."
			evidence := filepath.Join(project.Path, name)
			minimumFix := "Add the missing required artifact and rerun p2r scan/run."
			if name == "original_session_marker" {
				evidence = project.Path
				minimumFix = "Add one of: " + strings.Join(projectlayout.OriginalSessionMarkers(), ", ") + "."
			}
			findings = append(findings, model.Finding{
				Stage:      "A",
				Severity:   "Low",
				Title:      "Missing required delivery artifact: " + name,
				Rule:       rule,
				Evidence:   evidence,
				Impact:     "The package cannot be validated as a prompt2repo delivery.",
				MinimumFix: minimumFix,
			})
		}
	}
	if metadataTaskID := projectlayout.MetadataTaskID(project.Path); metadataTaskID != "" && metadataTaskID != project.TaskID {
		findings = append(findings, model.Finding{
			Stage:      "A",
			Severity:   "Low",
			Title:      "metadata.json task_id does not match directory task ID",
			Rule:       "The canonical task ID is the <batch>/<task-id>/<task-id> directory name; metadata.json.task_id must match it.",
			Evidence:   fmt.Sprintf("directory task_id=%s metadata task_id=%s", project.TaskID, metadataTaskID),
			Impact:     "Historical snapshots or copied metadata can otherwise pollute the QA index.",
			MinimumFix: "Update metadata.json.task_id to match the directory task ID, or move the package under the intended TASK-* directory.",
			SourcePath: filepath.Join(project.Path, "metadata.json"),
		})
	}
	if project.MetadataPromptMissing {
		findings = append(findings, model.Finding{
			Stage:      "A",
			Severity:   "Low",
			Title:      "metadata.json prompt is missing",
			Rule:       "metadata.json.prompt is required for acceptance mapping.",
			Evidence:   filepath.Join(project.Path, "metadata.json"),
			Impact:     "Static audit cannot confidently map implementation to the original prompt.",
			MinimumFix: "Populate metadata.json.prompt with the source prompt.",
		})
	}
	return findings
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (r Runner) runStageAScripts(ctx context.Context, project scanner.Project, scriptRoot, logPath string, writer ArtifactWriter, record *model.StageRecord, outputs map[string]string) map[string]scriptExecution {
	results := map[string]scriptExecution{}
	projectTypeArgs := projectTypeArgs(scriptRoot)
	run := func(script string, args []string) scriptExecution {
		if err := ctx.Err(); err != nil {
			return cancelledScriptExecution(script, scriptRoot, err)
		}
		return r.runStageAScript(ctx, project.Path, scriptRoot, script, args)
	}
	results["run_acceptance.py"] = run("run_acceptance.py", acceptanceScriptArgs(outputs, projectTypeArgs))
	removeScriptOwnedFile(outputs["validation_md"])
	if err := cleanValidationInputRoot(scriptRoot); err != nil {
		results["run_validate.py"] = scriptExecution{
			Script:    "run_validate.py",
			InputRoot: scriptRoot,
			Error:     "validation input root cleanup failed: " + err.Error(),
		}
	} else {
		results["run_validate.py"] = run("run_validate.py", validationScriptArgs(outputs, projectTypeArgs))
	}

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
	log.WriteString(results["run_validate.py"].logBlock())
	for _, check := range checks {
		result := run(check.script, check.args)
		results[check.script] = result
		*record = requiredStageJSON(*record, writer, writer.RelativePath(check.output), result)
		log.WriteString(result.logBlock())
	}
	bestEffortStageText(record, writer, writer.RelativePath(logPath), log.String())
	return results
}

func acceptanceScriptArgs(outputs map[string]string, projectTypeArgs []string) []string {
	args := []string{
		"--output-json", outputs["acceptance"],
		"--output-md", outputs["acceptance_md"],
	}
	return append(args, projectTypeArgs...)
}

func validationScriptArgs(outputs map[string]string, _ []string) []string {
	return []string{
		"--output-md", outputs["validation_md"],
	}
}

func removeScriptOwnedFile(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return
	}
	_ = os.Remove(path)
}

func missingValidationReportFinding(path string, result scriptExecution) model.Finding {
	evidence := "run_validate.py completed without writing " + filepath.Base(path)
	if result.Script != "" && !result.OK {
		evidence = result.summary()
	}
	return model.Finding{
		Stage:      "A",
		Severity:   "Blocker",
		Title:      "run_validate.py did not emit validation_report.md",
		Rule:       "validation_report.md must be produced by run_validate.py, not by run_acceptance.py or a pipeline fallback.",
		Evidence:   evidence,
		Impact:     "The submit package would otherwise contain validation evidence with the wrong provenance.",
		MinimumFix: "Ensure assets/scripts/run_validate.py writes --output-md to " + filepath.Base(path) + " and rerun Stage A.",
		SourcePath: path,
	}
}

func cleanValidationInputRoot(root string) error {
	root = strings.TrimSpace(root)
	if root == "" {
		return fmt.Errorf("empty validation input root")
	}
	root = filepath.Clean(root)
	if filepath.Base(root) != "script_input_snapshot" {
		return fmt.Errorf("refusing to clean non-snapshot root %q", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	allowed := map[string]bool{
		"docs":              true,
		"metadata.json":     true,
		"original_sessions": true,
		"repo":              true,
	}
	for _, entry := range entries {
		if allowed[entry.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
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

func (r Runner) runStageAScript(ctx context.Context, workDir, inputRoot, script string, extraArgs []string) scriptExecution {
	scriptPath := filepath.Join(r.cfg.ScanPath, ".qa-control", "scripts", script)
	result := scriptExecution{Script: script, InputRoot: inputRoot}
	if err := ctx.Err(); err != nil {
		return cancelledScriptExecution(script, inputRoot, err)
	}
	python, prefix, pythonErr := r.pythonInvocation(ctx, workDir)
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
	execResult := r.exec.Run(ctx, timeout, inputRoot, env, python, args...)
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

func cancelledScriptExecution(script, inputRoot string, err error) scriptExecution {
	return scriptExecution{
		Script:    script,
		InputRoot: inputRoot,
		OK:        false,
		Error:     err.Error(),
	}
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

func (r Runner) pythonInvocation(ctx context.Context, workDir string) (string, []string, string) {
	for _, name := range []string{"python", "python3"} {
		if err := ctx.Err(); err != nil {
			return "", nil, err.Error()
		}
		path, err := r.exec.LookPath(name)
		if err != nil {
			continue
		}
		result := r.exec.Run(ctx, 5*time.Second, workDir, nil, path, "--version")
		if result.Err == nil && (strings.Contains(result.Stdout, "Python") || strings.Contains(result.Stderr, "Python")) {
			return path, nil, ""
		}
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err.Error()
	}
	if path, err := r.exec.LookPath("uv"); err == nil {
		return path, []string{"run", "python"}, ""
	}
	return "", nil, "python interpreter not found"
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
			Severity:   "Low",
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
			Stage:        "A",
			Severity:     acceptanceSeverity(section, issue["severity"]),
			Title:        issueID,
			Rule:         issueString(issue, "rule", ""),
			Evidence:     issueString(issue, "evidence", ""),
			Impact:       issueString(issue, "impact", ""),
			DoneCriteria: issueString(issue, "done_criteria", ""),
			MinimumFix:   issueString(issue, "repair_action", ""),
			SourcePath:   sourcePath,
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
	switch value {
	case "blocker", "blocking", "critical", "fatal":
		return "Blocker"
	case "high", "major", "error":
		return "High"
	case "medium", "moderate", "warning":
		return "Medium"
	case "low", "minor", "info", "informational":
		return "Low"
	}
	if section == "blocking_issues" {
		return "Blocker"
	}
	return "Low"
}

func hasHardStageAFailure(findings []model.Finding, acceptance scriptExecution, validate scriptExecution) bool {
	if !validate.OK {
		return true
	}
	for _, finding := range findings {
		if finding.Severity == "Blocker" {
			return true
		}
	}
	return false
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
