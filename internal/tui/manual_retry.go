package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

type nodeRetryPlan struct {
	RequestedNode string
	RestartNode   string
	Affected      []string
}

type manualRetryPreparedMsg struct {
	runner   *app.Runner
	opts     app.RunnerOptions
	nodeID   string
	affected []string
	err      error
}

// These adapters keep the TUI independent from retry persistence details.
// The app package supplies the production implementations once the retry plan
// and runner constructors are available; tests replace them with deterministic
// in-memory implementations.
var planManualNodeRetry = func(opts app.RunnerOptions, nodeID string) (nodeRetryPlan, error) {
	plan, err := app.PlanNodeRetry(opts, nodeID)
	if err != nil {
		return nodeRetryPlan{}, err
	}
	return nodeRetryPlan{
		RequestedNode: plan.RequestedNodeID,
		RestartNode:   plan.RestartNodeID,
		Affected:      append([]string(nil), plan.AffectedNodes...),
	}, nil
}

var newManualRetryRunner = func(opts app.RunnerOptions, nodeID string) (*app.Runner, error) {
	return app.NewRetryRunner(opts, nodeID)
}

func (m *model) confirmSelectedNodeRetry() tea.Cmd {
	if m.readOnly {
		m.err = fmt.Errorf("工作区快照为只读，不能重试节点")
		return m.showToast("只读工作区不能重试节点", toastWarning)
	}
	if m.runner == nil {
		m.err = fmt.Errorf("当前没有可接管节点重试的运行器")
		return m.showToast("当前运行不支持节点重试", toastWarning)
	}
	nodeID := strings.TrimSpace(m.selectedNode)
	if nodeID == "" {
		m.err = fmt.Errorf("请先选择要重试的节点")
		return m.showToast("请先选择节点", toastWarning)
	}
	event, exists := m.nodes[nodeID]
	if !exists {
		m.err = fmt.Errorf("节点 %s 尚未执行，不能重试", nodeID)
		return m.showToast("尚未执行的节点不能重试", toastWarning)
	}
	if !retryableNodeEvent(event) {
		m.err = fmt.Errorf("节点 %s 当前状态为 %s，只有失败、已取消或已跳过节点可以重试", nodeID, localizeStatus(event.Status))
		return m.showToast("当前节点状态不能重试", toastWarning)
	}
	plan, err := planManualNodeRetry(m.opts, nodeID)
	if err != nil {
		m.err = err
		return m.showToast("无法生成节点重试计划", toastError)
	}
	if plan.RequestedNode == "" {
		plan.RequestedNode = nodeID
	}
	if plan.RestartNode == "" {
		plan.RestartNode = plan.RequestedNode
	}
	dialog := newConfirmDialog(confirmRetryNode, "确认重试节点", "重试会使相关下游结果失效并重新执行，是否继续？")
	dialog.NodeID = plan.RequestedNode
	dialog.RestartNode = plan.RestartNode
	dialog.Affected = append([]string(nil), plan.Affected...)
	m.err = nil
	m.openConfirm(dialog)
	return func() tea.Msg { return confirmOpenedMsg{} }
}

func retryableNodeEvent(event domain.RunnerEvent) bool {
	switch event.Type {
	case "node_failed", "node_canceled", "node_skipped":
		return true
	}
	switch event.Status {
	case "failed", "canceled", "skipped":
		return true
	}
	return false
}

func (m *model) prepareManualNodeRetry(nodeID string, affected []string) tea.Cmd {
	opts := m.opts
	return func() tea.Msg {
		runner, err := newManualRetryRunner(opts, nodeID)
		return manualRetryPreparedMsg{runner: runner, opts: opts, nodeID: nodeID, affected: append([]string(nil), affected...), err: err}
	}
}

func retryAffectedLabel(affected []string) string {
	if len(affected) == 0 {
		return "仅重试起点节点"
	}
	const previewLimit = 3
	preview := affected
	if len(preview) > previewLimit {
		preview = preview[:previewLimit]
	}
	labels := make([]string, 0, len(preview))
	for _, id := range preview {
		labels = append(labels, localizeNode(id))
	}
	label := strings.Join(labels, "、")
	if remaining := len(affected) - len(preview); remaining > 0 {
		label += fmt.Sprintf(" 等 %d 个节点", len(affected))
	}
	return label
}
