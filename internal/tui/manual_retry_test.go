package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
)

func TestOverviewRetryOpensPlanConfirmation(t *testing.T) {
	restoreRetryAdapters(t)
	planManualNodeRetry = func(_ app.RunnerOptions, nodeID string) (nodeRetryPlan, error) {
		return nodeRetryPlan{
			RequestedNode: nodeID,
			RestartNode:   nodes.DockerBuild,
			Affected:      []string{nodes.DockerBuild, nodes.InitialVerify, nodes.OracleVerify, nodes.CodeEdgeLint},
		}, nil
	}
	m := retryTestModel(t, viewOverview, "failed")

	updated, cmd := m.Update(runeKey("r"))
	got := updated.(model)
	if cmd == nil || got.confirm == nil || got.confirm.Action != confirmRetryNode {
		t.Fatalf("retry did not open confirmation: confirm=%+v cmd=%v", got.confirm, cmd)
	}
	if got.confirm.NodeID != nodes.CodeEdgeLint || got.confirm.RestartNode != nodes.DockerBuild || len(got.confirm.Affected) != 4 {
		t.Fatalf("retry plan not preserved in dialog: %+v", got.confirm)
	}
	rendered := got.confirm.View(80, 20)
	for _, want := range []string{"确认重试节点", "代码检查", "实际重试起点", "Docker 构建", "受影响"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("confirmation missing %q:\n%s", want, rendered)
		}
	}
}

func TestDetailRetryConfirmationStartsReplacementRunner(t *testing.T) {
	restoreRetryAdapters(t)
	planManualNodeRetry = func(_ app.RunnerOptions, nodeID string) (nodeRetryPlan, error) {
		return nodeRetryPlan{RequestedNode: nodeID, RestartNode: nodeID, Affected: []string{nodeID, nodes.FinalReview}}, nil
	}
	replacement := app.NewRunner(app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	newManualRetryRunner = func(opts app.RunnerOptions, nodeID string) (*app.Runner, error) {
		if nodeID != nodes.CodeEdgeLint {
			t.Fatalf("retry runner node=%q", nodeID)
		}
		return replacement, nil
	}
	m := retryTestModel(t, viewNodeDetail, "failed")
	m.done = true

	opened, _ := m.Update(runeKey("r"))
	m = opened.(model)
	confirmed, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = confirmed.(model)
	if cmd == nil || m.confirm != nil {
		t.Fatalf("confirmation did not prepare retry: confirm=%+v cmd=%v", m.confirm, cmd)
	}
	msg := cmd()
	prepared, runCmd := m.Update(msg)
	got := prepared.(model)
	if got.runner != replacement || got.done || got.readOnly || got.selectedNode != nodes.CodeEdgeLint {
		t.Fatalf("replacement runner state is incorrect: runner=%p done=%v readonly=%v selected=%q", got.runner, got.done, got.readOnly, got.selectedNode)
	}
	if runCmd == nil || !strings.Contains(got.notice, "节点重试") {
		t.Fatalf("retry did not start workflow commands: notice=%q cmd=%v", got.notice, runCmd)
	}
}

func TestRetryRejectsReadOnlyAndNonTerminalNodes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		readOnly bool
		status   string
		want     string
	}{
		{name: "readonly", readOnly: true, status: "failed", want: "只读"},
		{name: "running", status: "running", want: "只有失败、已取消或已跳过节点"},
		{name: "waiting", status: "waiting", want: "只有失败、已取消或已跳过节点"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := retryTestModel(t, viewOverview, tc.status)
			m.readOnly = tc.readOnly
			updated, cmd := m.Update(runeKey("r"))
			got := updated.(model)
			if got.confirm != nil || got.err == nil || !strings.Contains(got.err.Error(), tc.want) || cmd == nil {
				t.Fatalf("invalid retry state not rejected: confirm=%+v err=%v cmd=%v", got.confirm, got.err, cmd)
			}
		})
	}
}

func TestRetryPlanAndRunnerErrorsAreVisible(t *testing.T) {
	restoreRetryAdapters(t)
	planManualNodeRetry = func(app.RunnerOptions, string) (nodeRetryPlan, error) {
		return nodeRetryPlan{}, errors.New("plan failed")
	}
	m := retryTestModel(t, viewOverview, "failed")
	updated, cmd := m.Update(runeKey("r"))
	got := updated.(model)
	if got.err == nil || !strings.Contains(got.err.Error(), "plan failed") || cmd == nil {
		t.Fatalf("plan error not surfaced: err=%v cmd=%v", got.err, cmd)
	}

	planManualNodeRetry = func(_ app.RunnerOptions, nodeID string) (nodeRetryPlan, error) {
		return nodeRetryPlan{RequestedNode: nodeID, RestartNode: nodeID, Affected: []string{nodeID}}, nil
	}
	newManualRetryRunner = func(app.RunnerOptions, string) (*app.Runner, error) {
		return nil, errors.New("runner failed")
	}
	opened, _ := m.Update(runeKey("r"))
	m = opened.(model)
	confirmed, startCmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = confirmed.(model)
	failed, toastCmd := m.Update(startCmd())
	got = failed.(model)
	if got.err == nil || !strings.Contains(got.err.Error(), "runner failed") || toastCmd == nil {
		t.Fatalf("runner error not surfaced: err=%v cmd=%v", got.err, toastCmd)
	}
}

func TestRetryHasMouseFooterAndChineseHelpEntry(t *testing.T) {
	restoreRetryAdapters(t)
	planManualNodeRetry = func(_ app.RunnerOptions, nodeID string) (nodeRetryPlan, error) {
		return nodeRetryPlan{RequestedNode: nodeID, RestartNode: nodeID, Affected: []string{nodeID}}, nil
	}
	m := retryTestModel(t, viewOverview, "failed")
	m.width, m.height = 120, 30
	if footer := m.footer(); !strings.Contains(footer, "r 重试节点") {
		t.Fatalf("overview footer missing retry action: %s", footer)
	}
	help := (&helpOverlay{view: viewNodeDetail}).View(100, 24)
	if !strings.Contains(help, "r 重试选中节点") {
		t.Fatalf("detail help missing retry action: %s", help)
	}
	cmd := clickRenderedMarker(t, &m, "[r 重试节点]")
	if cmd == nil || m.confirm == nil || m.confirm.Action != confirmRetryNode {
		t.Fatalf("mouse retry did not open confirmation: confirm=%+v cmd=%v", m.confirm, cmd)
	}
}

func TestReadOnlyViewsHideRetryAction(t *testing.T) {
	m := retryTestModel(t, viewOverview, "failed")
	m.readOnly = true
	if footer := m.footer(); strings.Contains(footer, "重试节点") {
		t.Fatalf("read-only overview advertised retry: %s", footer)
	}
	m.view = viewNodeDetail
	if rendered := m.nodeDetailView(); strings.Contains(rendered, "重试节点") {
		t.Fatalf("read-only detail advertised retry: %s", rendered)
	}
	if help := (&helpOverlay{view: viewNodeDetail, readOnly: true}).View(100, 24); strings.Contains(help, "重试选中节点") {
		t.Fatalf("read-only help advertised retry: %s", help)
	}
}

func TestManualRetryEventsHaveLocalizedPresentation(t *testing.T) {
	previous := domain.RunnerEvent{NodeID: nodes.CodeEdgeLint, Type: "node_failed", Status: "failed", Message: "old failure"}
	retry := mergeNodeEvent(previous, domain.RunnerEvent{NodeID: nodes.CodeEdgeLint, Type: "manual_retry_started", Status: "requeued"})
	if retry.Status != "requeued" || retry.Message != "手动节点重试已开始" || localizeStatus(retry.Status) != "等待重试" || statusGlyph(retry.Status) != "↻" {
		t.Fatalf("manual retry presentation is incomplete: %+v status=%q glyph=%q", retry, localizeStatus(retry.Status), statusGlyph(retry.Status))
	}
	preserved := mergeNodeEvent(domain.RunnerEvent{}, domain.RunnerEvent{NodeID: nodes.RepoPrepare, Type: "node_preserved", Status: "canceled"})
	if preserved.Message != "节点状态已保留" {
		t.Fatalf("preserved event was not localized: %+v", preserved)
	}
}

func retryTestModel(t *testing.T, view viewMode, status string) model {
	t.Helper()
	workspace, taskDir := t.TempDir(), t.TempDir()
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace, TaskDir: taskDir})
	m.width, m.height = 100, 30
	m.view = view
	m.selectedNode = nodes.CodeEdgeLint
	m.nodes[nodes.CodeEdgeLint] = domain.RunnerEvent{NodeID: nodes.CodeEdgeLint, Type: "node_" + status, Status: status, Message: status}
	return m
}

func restoreRetryAdapters(t *testing.T) {
	t.Helper()
	oldPlan, oldRunner := planManualNodeRetry, newManualRetryRunner
	t.Cleanup(func() {
		planManualNodeRetry = oldPlan
		newManualRetryRunner = oldRunner
	})
}
