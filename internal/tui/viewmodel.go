package tui

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/table"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/displaytime"
	"github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
	"github.com/xuanli520/p2r_tui/internal/taskdocs"
)

type overviewItem struct {
	TaskID        string
	Batch         string
	Path          string
	LastRunID     string
	LastRun       string
	RunStatus     string
	ManualVerdict string
	FailedStage   string
	Blocking      int
	High          int
	DocsCount     int
	CleanupStatus string
	Mode          string
	SearchText    string
}

type docsSummary struct {
	Count          int
	ManifestPath   string
	ManifestExists bool
	Docs           []taskdocs.Document
	Error          string
}

type cleanupSummary struct {
	Path          string
	Status        string
	Text          string
	ManualCommand string
}

type stageView struct {
	model.StageRecord
	DisplayName string
}

type executionViewModel struct {
	TaskID                string
	ProjectPath           string
	HasRun                bool
	Run                   model.RunRecord
	Stages                []stageView
	Findings              []model.Finding
	RefRuns               []model.RunRecord
	DocsSummary           docsSummary
	RunManifestPath       string
	PathWarnings          []pipeline.ProjectPathWarning
	PreflightPath         string
	PreflightText         string
	CleanupPath           string
	CleanupStatus         string
	CleanupText           string
	ArtifactRoot          string
	SelfTestState         string
	LogTailByStage        map[string]string
	GuidanceEventsByStage map[string][]string
}

func buildOverviewItems(cfg config.Config, projects []db.ProjectSummary) []overviewItem {
	items := make([]overviewItem, 0, len(projects))
	for _, project := range projects {
		item := overviewItem{
			TaskID:        project.TaskID,
			Batch:         project.Batch,
			Path:          project.Path,
			LastRunID:     project.LastRunID,
			LastRun:       project.LastRunAt,
			RunStatus:     project.RunStatus,
			ManualVerdict: project.ManualVerdict,
			FailedStage:   project.FailedStage,
			Blocking:      project.Blocking,
			High:          project.High,
			DocsCount:     taskdocs.Count(cfg.ScanPath, project.TaskID),
			CleanupStatus: "none",
			Mode:          "initial",
		}
		if project.LatestArtifactRoot != "" {
			run := model.RunRecord{StaticOnly: project.LatestStaticOnly, ArtifactRoot: project.LatestArtifactRoot}
			item.Mode = runMode(run)
			item.CleanupStatus = cleanupStatus(project.LatestArtifactRoot)
		}
		item.SearchText = overviewSearchText(item)
		items = append(items, item)
	}
	return items
}

func overviewDisplayRow(item overviewItem, specs []overviewColumnSpec) table.Row {
	row := make(table.Row, 0, len(specs))
	for _, spec := range specs {
		width := spec.Width
		switch spec.Key {
		case "task_id":
			row = append(row, truncateMiddleDisplay(item.TaskID, width))
		case "run_status":
			row = append(row, truncateDisplay(localizeRunStatus(empty(item.RunStatus, "unknown")), width))
		case "failed_stage":
			row = append(row, truncateDisplay(localizeFailedStage(item.FailedStage), width))
		case "blocker":
			row = append(row, truncateDisplay(fmt.Sprint(item.Blocking), width))
		case "high":
			row = append(row, truncateDisplay(fmt.Sprint(item.High), width))
		case "manual_verdict":
			row = append(row, truncateDisplay(localizeManualVerdict(item.ManualVerdict), width))
		case "docs":
			row = append(row, truncateDisplay(fmt.Sprint(item.DocsCount), width))
		case "cleanup":
			row = append(row, truncateDisplay(localizeCleanupStatus(item.CleanupStatus), width))
		case "batch":
			row = append(row, truncateDisplay(item.Batch, width))
		case "last_run":
			row = append(row, shortTime(item.LastRun))
		case "mode":
			row = append(row, truncateDisplay(localizeMode(item.Mode), width))
		}
	}
	return row
}

func buildExecutionViewModel(ctx context.Context, store executionStore, cfg config.Config, taskID string) (executionViewModel, error) {
	project, err := store.GetProject(ctx, taskID)
	if err != nil {
		return executionViewModel{}, err
	}
	vm := executionViewModel{
		TaskID:                taskID,
		ProjectPath:           project.Path,
		DocsSummary:           readDocsSummary(cfg, taskID),
		SelfTestState:         selfTestState(project, cfg),
		LogTailByStage:        map[string]string{},
		GuidanceEventsByStage: map[string][]string{},
	}
	run, err := store.LatestRunForTask(ctx, taskID)
	if err != nil {
		if err == sql.ErrNoRows {
			vm.Stages = normalizeStageViews(nil)
			return vm, nil
		}
		return vm, err
	}
	vm.HasRun = true
	vm.Run = run
	vm.ArtifactRoot = run.ArtifactRoot
	vm.RunManifestPath = filepath.Join(run.ArtifactRoot, "run_manifest.json")
	manifest := readRunManifestView(vm.RunManifestPath)
	if manifest.ProjectPath != "" {
		vm.ProjectPath = manifest.ProjectPath
		project.Path = manifest.ProjectPath
		vm.SelfTestState = selfTestState(project, cfg)
	}
	vm.PathWarnings = manifest.PathWarnings
	vm.PreflightPath = filepath.Join(run.ArtifactRoot, "preflight.json")
	vm.PreflightText = readPreflightSummary(vm.PreflightPath)
	cleanup := readCleanupSummary(run.ArtifactRoot)
	vm.CleanupPath = cleanup.Path
	vm.CleanupStatus = cleanup.Status
	vm.CleanupText = cleanup.Text

	stages, _ := store.Stages(ctx, run.RunID)
	vm.Stages = normalizeStageViews(stages)
	findings, _ := store.Findings(ctx, run.RunID)
	vm.Findings = prioritizeFindings(findings)
	runs, _ := store.ListRunsForTask(ctx, taskID)
	for _, candidate := range runs {
		if !eligibleRefRun(candidate) {
			continue
		}
		vm.RefRuns = append(vm.RefRuns, candidate)
	}
	for _, stage := range vm.Stages {
		if stage.LogPath == "" {
			continue
		}
		vm.LogTailByStage[stage.Stage] = readLogTail(stage.LogPath, cfg.TUI.LogMaxLines)
		vm.GuidanceEventsByStage[stage.Stage] = readGuidanceEvents(stage.LogPath)
	}
	return vm, nil
}

func eligibleRefRun(run model.RunRecord) bool {
	switch run.Status {
	case model.RunCompletedClean, model.RunCompletedWithFindings:
	default:
		return false
	}
	return dirExists(run.ArtifactRoot)
}

func normalizeStageViews(stages []model.StageRecord) []stageView {
	byStage := map[string]model.StageRecord{}
	for _, stage := range stages {
		if stage.Name == "" {
			stage.Name = model.StageDisplayName(stage.Stage)
		}
		byStage[stage.Stage] = stage
	}
	result := make([]stageView, 0, 6)
	for _, letter := range model.AllStages() {
		stage, ok := byStage[letter]
		if !ok {
			stage = model.StageRecord{
				Stage:        letter,
				Name:         model.StageDisplayName(letter),
				Status:       model.StageSkipped,
				ErrorSummary: "本次运行未记录该阶段",
			}
		}
		result = append(result, stageView{
			StageRecord: stage,
			DisplayName: localizeStageName(stage.Stage, stage.Name),
		})
	}
	return result
}

func preferredStageIndex(stages []stageView) int {
	for index, stage := range stages {
		if stage.Status == model.StageFailed || stage.Status == model.StageBlocked {
			return index
		}
	}
	return 0
}

func buildDetailContent(vm executionViewModel, selectedStage string, width int) string {
	if vm.TaskID == "" {
		return "未选择已索引的项目\n请先执行 `p2r scan --path <projects-qa>`"
	}
	if !vm.HasRun {
		return strings.Join([]string{
			"任务: " + vm.TaskID,
			"模式: " + localizeMode("initial"),
			"自测报告: " + vm.SelfTestState,
			"文档: " + docsSummaryLine(vm.DocsSummary),
			"",
			"暂无运行记录，按 Ctrl+R 启动流水线",
		}, "\n")
	}

	stage := stageForKey(vm.Stages, selectedStage)
	var builder strings.Builder
	icon, _ := stageStatusIcon(stage.Status)
	builder.WriteString(fmt.Sprintf("阶段 %s - %s\n", stage.Stage, stage.DisplayName))
	builder.WriteString(fmt.Sprintf("状态: %s %s  耗时: %dms\n", icon, localizeStageStatus(stage.Status), stage.DurationMS))
	if len(stage.BlockedBy) > 0 {
		builder.WriteString("阻塞来源: " + strings.Join(stage.BlockedBy, ", ") + "\n")
	}
	if stage.ErrorSummary != "" {
		builder.WriteString("原因:\n")
		for _, line := range wrapDisplay(localizeSummary(stage.ErrorSummary), max(20, width-4)) {
			builder.WriteString("  " + line + "\n")
		}
	}
	if len(vm.PathWarnings) > 0 {
		builder.WriteString("\n路径警告:\n")
		for _, warning := range vm.PathWarnings {
			builder.WriteString("  " + compactPath(warning.DBPath, vm) + " -> " + compactPath(warning.CanonicalPath, vm) + "\n")
		}
	}
	if codexStage(stage.Stage) {
		reportPath := primaryCodexReportPath(stage)
		if reportPath != "" {
			builder.WriteString("\n最终报告:\n")
			builder.WriteString("  路径: " + compactPath(reportPath, vm) + "\n")
			if summary := reportSummary(reportPath); summary != "" {
				for _, line := range wrapDisplay(summary, max(20, width-6)) {
					builder.WriteString("  摘要: " + line + "\n")
				}
			}
		}
		if events := vm.GuidanceEventsByStage[stage.Stage]; len(events) > 0 {
			builder.WriteString("\nCodex deadline guidance:\n")
			for _, event := range events {
				builder.WriteString("  " + event + "\n")
			}
		}
	}
	builder.WriteString("\n本阶段发现:\n")
	stageFindings := findingsForStage(vm.Findings, stage.Stage)
	if len(stageFindings) == 0 {
		builder.WriteString("  无阻断/严重发现\n")
	} else {
		for _, finding := range stageFindings {
			label := severityStyle(finding.Severity).Render("[" + localizeSeverity(finding.Severity) + "]")
			builder.WriteString(fmt.Sprintf("  %s %s\n", label, finding.Title))
			for _, detail := range []string{finding.Rule, finding.Evidence, finding.SourcePath} {
				if strings.TrimSpace(detail) == "" {
					continue
				}
				for _, line := range wrapDisplay(compactDetailText(detail, vm), max(20, width-6)) {
					builder.WriteString("    " + line + "\n")
				}
			}
		}
	}
	builder.WriteString("\n证据入口:\n")
	builder.WriteString("  运行目录: " + compactPath(empty(vm.ArtifactRoot, "未生成"), vm) + "\n")
	if stage.LogPath != "" {
		builder.WriteString("  日志: " + compactPath(stage.LogPath, vm) + "\n")
	}
	if len(stage.ArtifactPaths) == 0 {
		builder.WriteString("  阶段产物: 未生成\n")
	} else {
		builder.WriteString("  阶段产物:\n")
		for _, path := range stage.ArtifactPaths {
			builder.WriteString("    - " + compactPath(path, vm) + "\n")
		}
	}
	builder.WriteString("\n文档:\n")
	builder.WriteString("  " + docsSummaryLineCompact(vm.DocsSummary, vm) + "\n")
	for _, line := range docsDetailLines(vm.DocsSummary) {
		builder.WriteString("  " + line + "\n")
	}
	builder.WriteString("\n预检:\n")
	builder.WriteString("  路径: " + compactPath(empty(vm.PreflightPath, "未生成"), vm) + "\n")
	for _, line := range strings.Split(empty(vm.PreflightText, "未生成"), "\n") {
		builder.WriteString("  " + line + "\n")
	}
	builder.WriteString("\n清理:\n")
	builder.WriteString("  路径: " + compactPath(empty(vm.CleanupPath, "未生成"), vm) + "\n")
	builder.WriteString("  状态: " + localizeCleanupStatus(vm.CleanupStatus) + "\n")
	for _, line := range strings.Split(empty(vm.CleanupText, "未生成"), "\n") {
		builder.WriteString("  " + line + "\n")
	}
	builder.WriteString("\n日志尾部:\n")
	if tail := strings.TrimSpace(vm.LogTailByStage[stage.Stage]); tail != "" {
		builder.WriteString(tail)
		builder.WriteString("\n")
	} else {
		builder.WriteString("  未生成\n")
	}
	return builder.String()
}

func stageForKey(stages []stageView, key string) stageView {
	for _, stage := range stages {
		if stage.Stage == key {
			return stage
		}
	}
	if len(stages) == 0 {
		return stageView{}
	}
	return stages[0]
}

func findingsForStage(findings []model.Finding, stage string) []model.Finding {
	var result []model.Finding
	for _, finding := range findings {
		if finding.Stage == stage {
			result = append(result, finding)
		}
	}
	if len(result) > 8 {
		return result[:8]
	}
	return result
}

func prioritizeFindings(findings []model.Finding) []model.Finding {
	result := append([]model.Finding(nil), findings...)
	sort.SliceStable(result, func(i, j int) bool {
		if severityRank(result[i].Severity) != severityRank(result[j].Severity) {
			return severityRank(result[i].Severity) < severityRank(result[j].Severity)
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func severityRank(severity string) int {
	switch severity {
	case "Blocker":
		return 0
	case "High":
		return 1
	case "Medium":
		return 2
	case "Low":
		return 3
	default:
		return 4
	}
}

func readDocsSummary(cfg config.Config, taskID string) docsSummary {
	path := taskdocs.ManifestPath(cfg.ScanPath, taskID)
	manifest, err := taskdocs.ReadManifest(cfg.ScanPath, taskID)
	summary := docsSummary{
		ManifestPath:   path,
		ManifestExists: fileExists(path),
		Docs:           manifest.Docs,
		Count:          len(manifest.Docs),
	}
	if err != nil {
		summary.Error = err.Error()
	}
	return summary
}

type runManifestView struct {
	ProjectPath  string
	PathWarnings []pipeline.ProjectPathWarning
}

func readRunManifestView(path string) runManifestView {
	content, err := os.ReadFile(path)
	if err != nil {
		return runManifestView{}
	}
	var payload struct {
		ProjectPath  string                        `json:"project_path"`
		PathWarnings []pipeline.ProjectPathWarning `json:"path_warnings"`
	}
	if json.Unmarshal(content, &payload) != nil {
		return runManifestView{}
	}
	return runManifestView{ProjectPath: payload.ProjectPath, PathWarnings: payload.PathWarnings}
}

func docsSummaryLine(summary docsSummary) string {
	status := "未生成"
	if summary.ManifestExists {
		status = "已生成"
	}
	if summary.Error != "" {
		status = "读取失败: " + summary.Error
	}
	return fmt.Sprintf("%d 个，文档清单: %s（%s）", summary.Count, summary.ManifestPath, status)
}

func docsSummaryLineCompact(summary docsSummary, vm executionViewModel) string {
	status := "未生成"
	if summary.ManifestExists {
		status = "已生成"
	}
	if summary.Error != "" {
		status = "读取失败: " + summary.Error
	}
	return fmt.Sprintf("%d 个，文档清单: %s（%s）", summary.Count, compactPath(summary.ManifestPath, vm), status)
}

func docsDetailLines(summary docsSummary) []string {
	if len(summary.Docs) == 0 {
		return []string{"无补充文档"}
	}
	lines := make([]string, 0, len(summary.Docs))
	for _, doc := range summary.Docs {
		state := "可进入静态审查上下文"
		if !doc.TextIncluded {
			state = "跳过: " + empty(doc.SkipReason, "未说明")
		}
		lines = append(lines, fmt.Sprintf("%s %s %d bytes %s", doc.DocID, doc.OriginalName, doc.SizeBytes, state))
	}
	return lines
}

func selfTestState(project scanner.Project, cfg config.Config) string {
	for _, candidate := range pipeline.SelfTestReportCandidates(project.Path, cfg) {
		if fileExists(candidate) {
			return "正常 " + candidate
		}
	}
	return "未生成，检查路径: " + strings.Join(pipeline.SelfTestReportCandidates(project.Path, cfg), ", ")
}

func readPreflightSummary(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "未生成"
		}
		return "读取失败: " + err.Error()
	}
	var payload struct {
		Checks []struct {
			Name    string   `json:"name"`
			Status  string   `json:"status"`
			Message string   `json:"message"`
			Stages  []string `json:"stages"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(content, &payload); err != nil {
		return "读取失败: " + err.Error()
	}
	var lines []string
	for _, check := range payload.Checks {
		if check.Status == "ok" {
			continue
		}
		line := fmt.Sprintf("%s: %s", check.Name, localizePreflightStatus(check.Status))
		if len(check.Stages) > 0 {
			line += " [" + strings.Join(check.Stages, ",") + "]"
		}
		if check.Message != "" {
			line += " - " + localizePreflightMessage(check.Message)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "正常"
	}
	return strings.Join(lines, "\n")
}

func readCleanupSummary(artifactRoot string) cleanupSummary {
	path := filepath.Join(artifactRoot, "cleanup_summary.json")
	summary := cleanupSummary{Path: path, Status: "none", Text: "未生成"}
	content, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			summary.Status = "unknown"
			summary.Text = "读取失败: " + err.Error()
		}
		return summary
	}
	var data map[string]any
	if err := json.Unmarshal(content, &data); err != nil {
		summary.Status = "unknown"
		summary.Text = "读取失败: " + err.Error()
		return summary
	}
	summary.Status, _ = data["status"].(string)
	if summary.Status == "" {
		summary.Status = "unknown"
	}
	var lines []string
	for _, key := range []string{"compose_project", "manual_command", "verification", "error"} {
		if value, ok := data[key].(string); ok && strings.TrimSpace(value) != "" {
			lines = append(lines, key+": "+value)
			if key == "manual_command" {
				summary.ManualCommand = value
			}
		}
	}
	if warnings, ok := data["warnings"].([]any); ok {
		for _, warning := range warnings {
			if text, ok := warning.(string); ok {
				lines = append(lines, "warning: "+text)
			}
		}
	}
	if len(lines) == 0 {
		lines = append(lines, localizeCleanupStatus(summary.Status))
	}
	summary.Text = strings.Join(lines, "\n")
	return summary
}

func cleanupStatus(artifactRoot string) string {
	return readCleanupSummary(artifactRoot).Status
}

func runMode(run model.RunRecord) string {
	if run.StaticOnly {
		return "static-only"
	}
	content, err := os.ReadFile(filepath.Join(run.ArtifactRoot, "run_manifest.json"))
	if err == nil {
		var data map[string]any
		if json.Unmarshal(content, &data) == nil {
			if mode, ok := data["qa_mode"].(string); ok && mode != "" {
				return mode
			}
		}
	}
	return "initial"
}

func overviewSearchText(item overviewItem) string {
	values := []string{
		item.TaskID,
		item.Batch,
		item.RunStatus,
		localizeRunStatus(item.RunStatus),
		item.ManualVerdict,
		localizeManualVerdict(item.ManualVerdict),
		item.FailedStage,
		localizeStageName(item.FailedStage, ""),
		item.CleanupStatus,
		localizeCleanupStatus(item.CleanupStatus),
		item.Mode,
		localizeMode(item.Mode),
	}
	return strings.Join(values, " ")
}

func localizeFailedStage(stage string) string {
	stage = strings.TrimSpace(stage)
	if stage == "" {
		return "-"
	}
	return stage
}

func readLogTail(path string, maxLines int) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return "读取失败: " + err.Error()
	}
	text := strings.TrimRight(string(content), "\r\n")
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	if maxLines <= 0 {
		maxLines = 200
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

func readGuidanceEvents(path string) []string {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var events []string
	inSection := false
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "=== codex guidance events ===" {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		if strings.HasPrefix(trimmed, "===") {
			inSection = false
			continue
		}
		if strings.Contains(trimmed, "guidance sent") {
			events = append(events, trimmed)
		}
	}
	return events
}

func codexStage(stage string) bool {
	switch strings.TrimSpace(stage) {
	case "D", "E", "F":
		return true
	default:
		return false
	}
}

func primaryCodexReportPath(stage stageView) string {
	if !codexStage(stage.Stage) {
		return ""
	}
	known := map[string][]string{
		"D": {"tests_coverage_report.md", "4_测试有效性报告_api端点真实性.md", "4_测试有效性报告_api端点真实性_确认修复报告.md"},
		"E": {"static_acceptance_audit_report.md", "1_质检AI测试报告.md", "1_质检AI测试报告_确认修复报告.md"},
		"F": {"3_标注员AI报告问题的修复报告.md", "3_标注员AI报告问题_确认修复报告.md"},
	}
	for _, name := range known[stage.Stage] {
		for _, path := range stage.ArtifactPaths {
			if filepath.Base(path) == name {
				return path
			}
		}
	}
	for _, path := range stage.ArtifactPaths {
		if strings.EqualFold(filepath.Ext(path), ".md") && filepath.Base(path) != "short_comment.txt" {
			return path
		}
	}
	return ""
}

func reportSummary(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return "读取失败: " + err.Error()
	}
	var lines []string
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "<!-- p2r:static-review-json:") {
			continue
		}
		lines = append(lines, trimmed)
		if len(lines) >= 3 {
			break
		}
	}
	return strings.Join(lines, " ")
}

func compactDetailText(text string, vm executionViewModel) string {
	if strings.TrimSpace(vm.ArtifactRoot) != "" {
		text = strings.ReplaceAll(text, filepath.Clean(vm.ArtifactRoot), "$RUN")
	}
	if strings.TrimSpace(vm.ProjectPath) != "" {
		text = strings.ReplaceAll(text, filepath.Clean(vm.ProjectPath), "$PROJECT")
	}
	return text
}

func compactPath(path string, vm executionViewModel) string {
	path = strings.TrimSpace(path)
	if path == "" || path == "未生成" {
		return empty(path, "未生成")
	}
	cleaned := filepath.Clean(path)
	for _, base := range []struct {
		path  string
		label string
	}{
		{vm.ArtifactRoot, "$RUN"},
		{vm.ProjectPath, "$PROJECT"},
	} {
		if strings.TrimSpace(base.path) == "" {
			continue
		}
		rel, err := filepath.Rel(filepath.Clean(base.path), cleaned)
		if err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			return base.label + "/" + filepath.ToSlash(rel)
		}
		if err == nil && rel == "." {
			return base.label
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, cleaned); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return truncateMiddleDisplay(cleaned, 96)
}

func stageLogPreview(path string, maxLines int) string {
	content := readLogTail(path, maxLines)
	if strings.HasPrefix(content, "读取失败: ") {
		return "Log: " + path + "\n" + strings.TrimPrefix(content, "读取失败: ") + "\n"
	}
	return "Log: " + path + "\n" + content
}

func affectedStages(stage string) []string {
	switch stage {
	case string(model.StageA):
		return []string{string(model.StageA), string(model.StageF)}
	case string(model.StageB):
		return []string{string(model.StageB), string(model.StageC), string(model.StageF)}
	case string(model.StageC):
		return []string{string(model.StageC), string(model.StageF)}
	case string(model.StageD), string(model.StageE):
		return []string{stage, string(model.StageF)}
	default:
		return []string{stage}
	}
}

func stageLetter(index int) string {
	stages := model.AllStages()
	if index < 0 || index >= len(stages) {
		return string(model.StageA)
	}
	return stages[index]
}

func shortTime(value string) string {
	return displaytime.FormatMinute(value)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func empty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
