package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scheduler"
)

func localizeRunStatus(status string) string {
	switch strings.TrimSpace(status) {
	case model.RunCompletedClean:
		return "通过"
	case model.RunCompletedWithFindings:
		return "有发现"
	case model.RunRunning:
		return "运行中"
	case model.RunAborted:
		return "已中止"
	case model.RunCrashed:
		return "崩溃"
	case "":
		return "未知"
	default:
		return status
	}
}

func localizeStageStatus(status string) string {
	switch strings.TrimSpace(status) {
	case model.StageDone:
		return "完成"
	case model.StageFailed:
		return "失败"
	case model.StageBlocked:
		return "已阻塞"
	case model.StageRunning:
		return "运行中"
	case model.StagePending:
		return "等待中"
	case model.StageSkipped:
		return "已跳过"
	case "":
		return "未知"
	default:
		return status
	}
}

func localizeManualVerdict(verdict string) string {
	switch strings.TrimSpace(verdict) {
	case model.ManualUnset, "":
		return "未判定"
	case model.ManualPass:
		return "通过"
	case model.ManualRework:
		return "返工"
	case model.ManualFail:
		return "不通过"
	default:
		return verdict
	}
}

func localizeSeverity(severity string) string {
	switch strings.TrimSpace(severity) {
	case "Blocker":
		return "阻断"
	case "High":
		return "严重"
	case "Medium":
		return "中等"
	case "Low":
		return "低"
	case "":
		return "未知"
	default:
		return severity
	}
}

func localizeStageName(stage, name string) string {
	switch strings.TrimSpace(stage) {
	case "A":
		return "结构与规则检查"
	case "B":
		return "Docker运行时证据"
	case "C":
		return "测试运行时证据"
	case "D":
		return "测试有效性静态审查"
	case "E":
		return "静态验收审计"
	case "F":
		return "标注员修复静态审查"
	}
	switch strings.TrimSpace(name) {
	case "", "unknown":
		return "未知阶段"
	case "structure and rules check":
		return "结构与规则检查"
	case "Docker runtime evidence":
		return "Docker运行时证据"
	case "run_tests runtime evidence":
		return "测试运行时证据"
	case "tests effectiveness static review":
		return "测试有效性静态审查"
	case "static acceptance audit":
		return "静态验收审计"
	case "annotator repair static review":
		return "标注员修复静态审查"
	default:
		return name
	}
}

func localizeCleanupStatus(status string) string {
	switch strings.TrimSpace(status) {
	case "", "none":
		return "未生成"
	case "ok":
		return "正常"
	case "failed":
		return "失败"
	case "not_applicable":
		return "不适用"
	case "kept_by_operator_request":
		return "按要求保留"
	case "unknown":
		return "未知"
	default:
		return status
	}
}

func localizeMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case "recheck":
		return "打回重检"
	case "static-only":
		return "静态质检"
	case "runtime":
		return "运行时"
	case "initial", "":
		return "首次质检"
	default:
		return mode
	}
}

func localizeJobState(state scheduler.JobState) string {
	switch state {
	case scheduler.JobQueued:
		return "排队中"
	case scheduler.JobRunning:
		return "运行中"
	case scheduler.JobDone:
		return "已完成"
	case scheduler.JobCancelled:
		return "已终止"
	case scheduler.JobFailed:
		return "失败"
	default:
		return "未知"
	}
}

func localizePreflightStatus(status string) string {
	switch strings.TrimSpace(status) {
	case "ok":
		return "正常"
	case "degraded":
		return "降级"
	case "missing":
		return "缺失"
	case "failed":
		return "失败"
	case "":
		return "未知"
	default:
		return status
	}
}

func localizeSummary(summary string) string {
	trimmed := strings.TrimSpace(summary)
	switch trimmed {
	case "":
		return ""
	case "Not selected for this run.":
		return "本次未选择"
	case "--static-only skips Docker and run_tests evidence.":
		return "静态模式跳过运行时证据"
	case "codex app-server failed":
		return "Codex 执行失败"
	case "codex unavailable":
		return "Codex 不可用"
	case "audit input unavailable":
		return "审查输入不可用"
	case "prompt profile unavailable":
		return "提示词模板不可用"
	case "codex network policy unsupported":
		return "Codex 网络策略不受支持"
	case "codex writable tmp policy unsupported":
		return "Codex 临时写入策略不受支持"
	}
	var count int
	if _, err := fmt.Sscanf(trimmed, "%d acceptance finding(s)", &count); err == nil {
		return fmt.Sprintf("%d 个验收发现", count)
	}
	return localizePreflightMessage(trimmed)
}

func localizePreflightMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	replacements := map[string]string{
		"optional flags unavailable": "可选参数不可用",
		"node found near Codex CLI":  "已找到 Codex CLI 附近的 Node.js",
		"node found on PATH":         "已在 PATH 中找到 Node.js",
		"Codex CLI is required for static review stages. Searched PATH and known install locations.": "静态审查阶段需要 Codex CLI，已检查 PATH 和常见安装位置。",
		"Codex CLI is missing required exec flags":                                                   "Codex CLI 缺少必需执行参数",
	}
	for from, to := range replacements {
		message = strings.ReplaceAll(message, from, to)
	}
	return message
}

func stageStatusIcon(status string) (string, lipgloss.Color) {
	switch strings.TrimSpace(status) {
	case model.StageDone:
		return "✓", lipgloss.Color("#00CC66")
	case model.StageFailed:
		return "✗", lipgloss.Color("#FF4444")
	case model.StageBlocked:
		return "⊘", lipgloss.Color("#DDAA00")
	case model.StageRunning:
		return "▶", lipgloss.Color("#4488FF")
	case model.StagePending:
		return "○", lipgloss.Color("#666666")
	case model.StageSkipped:
		return "-", lipgloss.Color("#666666")
	default:
		return "?", lipgloss.Color("#888888")
	}
}

func severityStyle(severity string) lipgloss.Style {
	switch strings.TrimSpace(severity) {
	case "Blocker":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#FF4444")).Bold(true)
	case "High":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#FFAA00")).Bold(true)
	case "Medium":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))
	case "Low":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#666666"))
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))
	}
}
