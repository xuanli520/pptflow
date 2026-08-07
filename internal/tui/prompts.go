package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"

	"github.com/purplevoid/harbor-factory/internal/app"
)

// This file holds the two modal prompts that collect an audit reason before a
// durable mutation. Both are deliberately small and stateless beyond their
// input: every identity they act on is re-read from the model at submit time
// and revalidated by the application layer.

// reasonPromptRows bounds a prompt's reason editor so a long reason scrolls
// inside the editor instead of pushing the body off the window.
const reasonPromptRows = 3

// reviewPrompt collects the operator's reason for a review decision. The
// reason is multi-line because it is written verbatim into an immutable
// decision record, and a one-line field silently discouraged useful detail.
type reviewPrompt struct {
	decision      app.TaskBoardReviewDecision
	reasonInput   textarea.Model
	validationErr string
}

func newReviewPrompt(decision app.TaskBoardReviewDecision) *reviewPrompt {
	input := textarea.New()
	input.Placeholder = "说明审核决定的原因"
	input.ShowLineNumbers = false
	input.SetHeight(reasonPromptRows)
	input.CharLimit = 4000
	input.Focus()
	return &reviewPrompt{decision: decision, reasonInput: input}
}

func (prompt *reviewPrompt) View(width int) string {
	label := "审核通过"
	if prompt.decision == app.TaskBoardRequestChanges {
		label = "要求返修"
	}
	prompt.reasonInput.SetWidth(max(8, width-4))
	content := detailSectionTitleStyle.Render(label) + "\n" +
		mutedStyle.Render("审核意见将写入不可变决策记录") + "\n" +
		prompt.reasonInput.View()
	if prompt.validationErr != "" {
		content += "\n" + styleFail.Render(prompt.validationErr)
	}
	// inputStyle's rounded border adds framedPaneRows columns on top of Width, so
	// the pane is sized to the content budget minus that frame.
	return inputStyle.Width(max(1, width-framedPaneRows)).Render(content)
}

// runActionPrompt collects a reason and, for recovery-bearing strategies, shows
// the plan the operator is about to confirm. The prompt never derives the plan
// itself; it displays exactly what the application layer returned.
type runActionPrompt struct {
	kind             taskBoardRunActionKind
	strategy         app.TaskBoardRetryStrategy
	reasonInput      textinput.Model
	validationErr    string
	requiresReason   bool
	recoveryPreview  *app.TaskBoardRecoveryPreview
	protocolPreview  *app.TaskBoardStandardProtocolRetryPreview
	protocolPrepared *app.TaskBoardPreparedStandardProtocolRetry
}

func newRunActionPrompt(kind taskBoardRunActionKind, strategy app.TaskBoardRetryStrategy) *runActionPrompt {
	input := textinput.New()
	input.Prompt = "原因 "
	input.Placeholder = "记录本次操作的原因"
	input.CharLimit = 240
	input.Width = 52
	input.Focus()
	return &runActionPrompt{kind: kind, strategy: strategy, reasonInput: input, requiresReason: kind != taskBoardRetryAuthoringLaunchAction}
}

func (prompt *runActionPrompt) View(width int) string {
	label := "重试当前 Run"
	switch prompt.kind {
	case taskBoardRetryAction:
		if prompt.strategy == app.TaskBoardRetryStrategyTaskContinuation {
			label = "断点恢复创题 Run"
		} else if prompt.strategy == app.TaskBoardRetryStrategyStandardProtocolStage {
			label = "重试当前 Standard 阶段"
		}
	case taskBoardRetryAuthoringLaunchAction:
		label = "重试源码捕获"
	case taskBoardCancelAction:
		label = "取消当前 Run"
	}
	content := detailSectionTitleStyle.Render(label)
	if prompt.recoveryPreview != nil {
		content += "\n" + recoveryPreviewView(*prompt.recoveryPreview, max(1, width-4))
	} else if prompt.protocolPrepared != nil {
		content += "\n" + standardProtocolRetryPreviewView(prompt.protocolPrepared.TaskBoardStandardProtocolRetryPreview, "重试准备", max(1, width-4))
	} else if prompt.protocolPreview != nil {
		content += "\n" + standardProtocolRetryPreviewView(*prompt.protocolPreview, "协议重试预览", max(1, width-4))
	} else if prompt.requiresReason {
		content += "\n" + prompt.reasonInput.View()
	}
	if prompt.validationErr != "" {
		content += "\n" + styleFail.Render(prompt.validationErr)
	}
	// inputStyle's rounded border adds framedPaneRows columns on top of Width, so
	// the pane is sized to the content budget minus that frame.
	return inputStyle.Width(max(1, width-framedPaneRows)).Render(content)
}

func standardProtocolRetryPreviewView(preview app.TaskBoardStandardProtocolRetryPreview, title string, width int) string {
	fields := []string{
		detailField("目标阶段", displayStageName(preview.StageKey), width),
		detailField("失败 attempt", preview.Source.StageAttemptID, width),
		detailField("Transcript", preview.Source.TranscriptID, width),
		detailField("协议状态", preview.Status, width),
		detailField("失败码", preview.Source.FailureCode, width),
		detailField("模型", preview.ModelID, width),
		detailField("响应摘要", fmt.Sprintf("%d bytes · %s", preview.ResponseSize, preview.ResponseSHA), width),
	}
	return renderDetailSection(title, detailFields(fields...), width)
}

func recoveryPreviewView(preview app.TaskBoardRecoveryPreview, width int) string {
	fields := []string{
		detailField("目标阶段", recoveryPreviewStageList(preview.TargetStages), width),
		detailField("复用阶段", recoveryPreviewStageList(preview.ReusedStages), width),
		detailField("重新调度", recoveryPreviewStageList(preview.ScheduledStages), width),
		detailField("执行 Epoch", fmt.Sprintf("%d -> %d", preview.CurrentExecutionEpoch, preview.NextExecutionEpoch), width),
		detailField("断点序列", fmt.Sprintf("%d", preview.CheckpointSequence), width),
		detailField("输入校验", "复用阶段的产物与输入指纹已核验", width),
		detailField("工作流指纹", preview.WorkflowFingerprint, width),
	}
	if reasons := recoveryPreviewReasonList(preview); reasons != "" {
		fields = append(fields, detailField("计划原因", reasons, width))
	}
	if len(preview.InvalidatedStages) > 0 {
		fields = append(fields, detailField("未调度阶段", recoveryPreviewStageList(preview.InvalidatedStages), width))
	}
	if len(preview.OperatorOnlyStages) > 0 {
		fields = append(fields, detailField("人工阶段", recoveryPreviewStageList(preview.OperatorOnlyStages), width))
	}
	return renderDetailSection("断点恢复计划", detailFields(fields...), width)
}

func recoveryPreviewStageList(stages []string) string {
	if len(stages) == 0 {
		return "无"
	}
	names := make([]string, 0, len(stages))
	for _, stage := range stages {
		names = append(names, displayStageName(stage))
	}
	return strings.Join(names, ", ")
}

func recoveryPreviewReasonList(preview app.TaskBoardRecoveryPreview) string {
	seen := make(map[string]struct{})
	stages := make([]string, 0, len(preview.TargetStages)+len(preview.ReusedStages)+len(preview.ScheduledStages)+len(preview.InvalidatedStages)+len(preview.OperatorOnlyStages))
	stages = append(stages, preview.TargetStages...)
	stages = append(stages, preview.ReusedStages...)
	stages = append(stages, preview.ScheduledStages...)
	stages = append(stages, preview.InvalidatedStages...)
	stages = append(stages, preview.OperatorOnlyStages...)
	parts := make([]string, 0, len(stages))
	for _, stage := range stages {
		if _, duplicate := seen[stage]; duplicate {
			continue
		}
		seen[stage] = struct{}{}
		reasons := preview.StageReasons[stage]
		if len(reasons) == 0 {
			continue
		}
		labels := make([]string, 0, len(reasons))
		for _, reason := range reasons {
			labels = append(labels, recoveryPreviewReasonLabel(reason))
		}
		parts = append(parts, displayStageName(stage)+": "+strings.Join(labels, ", "))
	}
	return strings.Join(parts, "；")
}

func recoveryPreviewReasonLabel(reason string) string {
	switch reason {
	case "artifact_unavailable":
		return "上游产物缺失或损坏"
	case "input_fingerprint_drift":
		return "输入指纹不一致"
	case "dependency_invalidated":
		return "依赖阶段需重跑"
	case "retry_requested":
		return "失败阶段重试"
	case "force_recompute":
		return "请求重新生成"
	default:
		return reason
	}
}
