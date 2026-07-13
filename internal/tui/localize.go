package tui

import (
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
)

// User-visible vocabulary lives in this file. Unknown values are deliberately
// returned unchanged so plugin/custom nodes remain diagnosable.
func localizeStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "succeeded", "success", "passed":
		return "成功"
	case string(domain.CheckPass):
		return "通过"
	case "failed", "failure", string(domain.CheckFail):
		return "失败"
	case "canceled", "cancelled":
		return "已取消"
	case "running", "started":
		return "运行中"
	case "waiting", "gate_requested":
		return "等待审查"
	case "requeued":
		return "等待重试"
	case "pending", "":
		return "等待中"
	case "blocked":
		return "已阻塞"
	case "skipped":
		return "已跳过"
	case string(domain.CheckWarn):
		return "警告"
	default:
		return status
	}
}

func localizeNode(id string) string {
	names := map[string]string{
		nodes.RepoPrepare: "仓库准备", nodes.RepoAnalyze: "仓库分析", nodes.TaskDesign: "任务设计",
		nodes.TaskReview: "任务方向 [关卡]", nodes.GenerateTaskFiles: "生成任务文件",
		nodes.InstructionGen: "生成指令文档", nodes.TaskTOMLGen: "生成任务配置",
		nodes.DockerfileGen: "生成 Docker 配置", nodes.SolveGen: "生成解答脚本",
		nodes.TestGen: "生成测试脚本", nodes.TestsAnalysis: "测试分析", nodes.MaterializeTask: "物化任务文件", nodes.PublishTask: "发布任务文件",
		nodes.TaskRepair: "修复任务文件", nodes.RuntimeSelfCheck: "模型运行时自检",
		nodes.ContentReview: "内容审查 [关卡]", nodes.SolutionReview: "解答与测试审查 [关卡]", nodes.CodeEdgeLint: "代码检查", nodes.HarborVerify: "Harbor 验证",
		nodes.DockerBuild: "Docker 构建", nodes.InitialVerify: "初始验证", nodes.OracleVerify: "Oracle 验证",
		nodes.QualityCheck: "质量检查", nodes.SimilarityCheck: "相似度检查",
		nodes.HarborRunQwen: "Qwen 模型运行", nodes.HarborRunOpus: "Opus 模型运行",
		nodes.SubmissionLint: "提交检查", nodes.ResultReview: "结果审查 [关卡]",
		nodes.FinalReview: "最终发布 [关卡]", nodes.Package: "打包",
	}
	if name, ok := names[id]; ok {
		return name
	}
	return id
}

func localizeGate(id, fallback string) string {
	switch id {
	case nodes.TaskReview:
		return "任务方向"
	case nodes.ContentReview:
		return "内容审查"
	case nodes.SolutionReview:
		return "解答与测试审查"
	case nodes.FinalReview:
		return "最终发布"
	case nodes.ResultReview:
		return "结果审查"
	}
	if fallback != "" {
		return fallback
	}
	return id
}

func localizeAction(action string) string {
	switch action {
	case "approve":
		return "批准"
	case "reject":
		return "拒绝"
	case "revise":
		return "修订并重新运行检查"
	case "refresh":
		return "刷新截图证据"
	default:
		return action
	}
}

func localizeBool(v bool) string {
	if v {
		return "是"
	}
	return "否"
}

func localizeEventType(eventType string) string {
	switch eventType {
	case "run_started":
		return "运行已开始"
	case "node_started":
		return "节点已开始"
	case "node_succeeded":
		return "节点成功"
	case "node_failed":
		return "节点失败"
	case "node_canceled":
		return "节点已取消"
	case "node_skipped":
		return "节点已跳过"
	case "node_requeued":
		return "节点等待重新执行"
	case "node_preserved":
		return "节点状态已保留"
	case "node_reused":
		return "已复用节点结果"
	case "gate_requested":
		return "等待审查"
	case "run_succeeded":
		return "运行成功"
	case "run_failed":
		return "运行失败"
	case "run_recovered":
		return "运行已恢复"
	default:
		return eventType
	}
}

func localizeField(field startField) string {
	labels := map[startField]string{
		startFieldMode: "模式", startFieldTaskDir: "任务路径", startFieldRepoURL: "仓库地址", startFieldCommit: "提交哈希",
		startFieldWorkspace: "工作区路径", startFieldTaskOutput: "任务输出目录", startFieldTestsAnalysis: "测试分析路径",
		startFieldQwenResult: "Qwen 结果路径", startFieldOpusResult: "Opus 结果路径", startFieldQwenScreenshot: "Qwen 截图路径",
		startFieldOpusScreenshot: "Opus 截图路径", startFieldQualityCheck: "质量检查", startFieldQualityAgent: "质量检查代理",
		startFieldSimilarityCheck: "相似度检查", startFieldSimilarityGitHub: "GitHub 相似度", startFieldSimilarityThreshold: "相似度阈值",
		startFieldHistoryDirs: "历史目录", startFieldTB3Dirs: "TB3 目录", startFieldOutput: "打包输出目录",
		startFieldVerifyDocker: "Docker 验证", startFieldRunHarbor: "运行 Harbor", startFieldHarborAgent: "Harbor 代理",
		startFieldQwenModel: "Qwen 模型", startFieldOpusModel: "Opus 模型", startFieldQwenHarborBaseURL: "Qwen Harbor 地址",
		startFieldOpusHarborBaseURL: "Opus Harbor 地址", startFieldHarborTimeout: "Harbor 超时", startFieldHarborSetupTimeout: "Harbor 启动超时",
		startFieldHarborPreflight: "Harbor 预检", startFieldHarborConcurrency: "Harbor 并发数", startFieldHarborAttempts: "Harbor 尝试次数",
		startFieldHarborInfraRetries: "Harbor 基础设施重试", startFieldPackage: "打包", startFieldTaskName: "任务名称",
		startFieldCodeLang: "编程语言", startFieldTaskType: "任务类型", startFieldApplication: "应用领域", startFieldAHT: "预估耗时",
		startFieldDescription: "描述", startFieldZeroToOne: "从零到一", startFieldCodexModel: "Codex 模型",
		startFieldCodexReasoning: "Codex 推理", startFieldCodexPath: "Codex 路径", startFieldAgentTimeout: "代理超时",
	}
	if label, ok := labels[field]; ok {
		return label
	}
	return "字段"
}

func fieldUnit(field startField) string {
	switch field {
	case startFieldHarborTimeout, startFieldHarborSetupTimeout, startFieldAgentTimeout:
		return "秒"
	case startFieldAHT:
		return "分钟"
	case startFieldSimilarityThreshold:
		return "(0-1)"
	default:
		return ""
	}
}

func localizeRuntimeError(err error) string {
	if err == nil {
		return ""
	}
	original := err.Error()
	text := original
	replacements := []struct{ old, new string }{
		{"workspace snapshot is read-only", "工作区快照为只读"},
		{"cannot approve gate with failing critical checks", "无法批准：存在未通过的关键检查项"},
		{"revise/refresh is available at Final Review and Result Review", "修订/刷新仅在最终审查和结果审查中可用"},
		{"artifact path is outside allowed TUI roots", "工件路径超出允许的 TUI 根目录"},
		{"artifact path is not an editable Harbor artifact", "工件路径不是可编辑的 Harbor 工件"},
	}
	for _, replacement := range replacements {
		text = strings.ReplaceAll(text, replacement.old, replacement.new)
	}
	return text
}

func localizeCount(current, total int) string { return fmt.Sprintf("%d/%d", current, total) }
