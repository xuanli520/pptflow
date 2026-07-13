package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
)

func TestNodeEventReducerHandlesSparseEngineSequence(t *testing.T) {
	events := []domain.RunnerEvent{
		{RunID: "run-1", NodeID: nodes.QualityCheck, Type: "node_started", Status: "running", Message: nodes.QualityCheck},
		{RunID: "run-1", NodeID: nodes.QualityCheck, Type: "node_attempt_started", Status: "running", Attempt: 1},
		{RunID: "run-1", NodeID: nodes.QualityCheck, Type: "node_progress", Message: "正在检查任务质量", Path: "/tmp/quality.log"},
		{RunID: "run-1", NodeID: nodes.QualityCheck, Type: "node_succeeded", Status: "succeeded", Attempt: 1, Artifacts: []domain.ArtifactPreview{{Name: "quality_report.json", Path: "/tmp/quality_report.json"}}},
	}

	progress := reduceNodeEvents(events[:3])[nodes.QualityCheck]
	if progress.Status != "running" || progress.Message != "正在检查任务质量" || progress.Attempt != 1 {
		t.Fatalf("sparse progress event erased node state: %+v", progress)
	}

	want := reduceNodeEvents(events)[nodes.QualityCheck]
	if want.Status != "succeeded" || want.Message != "节点成功" || want.Path != "/tmp/quality.log" || want.Attempt != 1 || len(want.Artifacts) != 1 {
		t.Fatalf("terminal node read model is incomplete: %+v", want)
	}

	live := initialModel(context.Background(), func() {}, app.RunnerOptions{})
	for _, event := range events {
		live.applyRunnerEvent(event)
	}
	snapshot := initialModel(context.Background(), func() {}, app.RunnerOptions{})
	snapshot.applyWorkspaceSnapshot(domain.RunSummary{RunID: "run-1", Status: "running"}, events)
	for name, got := range map[string]domain.RunnerEvent{
		"live":     live.nodes[nodes.QualityCheck],
		"snapshot": snapshot.nodes[nodes.QualityCheck],
	} {
		if got.Status != want.Status || got.Message != want.Message || got.Path != want.Path || got.Attempt != want.Attempt || len(got.Artifacts) != len(want.Artifacts) {
			t.Fatalf("%s event path diverged from canonical reducer: got=%+v want=%+v", name, got, want)
		}
	}
}

func TestOverviewTableRendersPlainVisibleStatusCells(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{})
	m.width, m.height = 100, 30
	m.selectedNode = nodes.RepoPrepare
	m.nodes[nodes.RepoPrepare] = domain.RunnerEvent{
		NodeID: nodes.RepoPrepare, Type: "node_succeeded", Status: "succeeded", Message: "节点成功",
	}

	rendered := ansi.Strip(m.overview())
	if !strings.Contains(rendered, "✓ 成功") {
		t.Fatalf("overview status is not visible after ANSI stripping:\n%s", rendered)
	}
	if !strings.Contains(rendered, "○ 等待中") {
		t.Fatalf("overview pending status is not visible:\n%s", rendered)
	}
	if !strings.Contains(rendered, "节点成功") {
		t.Fatalf("overview message is not visible:\n%s", rendered)
	}
	for _, row := range m.overviewTable.Rows() {
		if len(row) > 0 && strings.Contains(row[0], "\x1b[") {
			t.Fatalf("overview table status cell contains ANSI escape data: %q", row[0])
		}
	}
}
