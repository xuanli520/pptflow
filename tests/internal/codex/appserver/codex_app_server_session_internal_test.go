package appserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/xuanli520/p2r_tui/internal/codex/appserver"
)

func TestAppServerSessionIgnoresClosedPipeErrorsAfterCompletion(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "codex.log")
	session := appserver.NewSessionProbeForTest(logPath, "", 0, nil)

	session.Complete("codex app-server", nil)
	session.CompleteStreamError("stdout", fmt.Errorf("read |0: %w", os.ErrClosed))
	session.CompleteStreamError("stderr", fmt.Errorf("read |0: %w", os.ErrClosed))

	if err := session.Err(); err != nil {
		t.Fatalf("closed pipe after completion changed result error: %v", err)
	}
	content, readErr := os.ReadFile(logPath)
	if readErr == nil && strings.Contains(string(content), "stream error") {
		t.Fatalf("closed pipe after completion should not be logged as an error:\n%s", content)
	}
}

func TestAppServerSessionKeepsPrematureClosedPipeErrors(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "codex.log")
	session := appserver.NewSessionProbeForTest(logPath, "", 0, nil)

	session.CompleteStreamError("stdout", fmt.Errorf("read |0: %w", os.ErrClosed))

	err := session.Err()
	if err == nil || !strings.Contains(err.Error(), "stream error") {
		t.Fatalf("premature closed pipe should still fail the session, got %v", err)
	}
	content, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(content), "stream error") {
		t.Fatalf("premature closed pipe should be logged:\n%s", content)
	}
}

func TestAppServerSessionIgnoresClosedPipeErrorsAfterProcessContextCancelled(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "codex.log")
	processCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancel()
	session := appserver.NewSessionProbeWithProcessContextForTest(logPath, processCtx)

	session.CompleteStreamError("stdout", fmt.Errorf("read |0: %w", os.ErrClosed))

	if err := session.Err(); err != nil || session.Completed() {
		t.Fatalf("cancel-driven closed pipe should wait for process result, completed=%t err=%v", session.Completed(), err)
	}
	content, readErr := os.ReadFile(logPath)
	if readErr == nil && strings.Contains(string(content), "stream error") {
		t.Fatalf("cancel-driven closed pipe should not be logged as stream error:\n%s", content)
	}
}

func TestAppServerSessionCompactsDeltaLogsAndKeepsDeltaReport(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "codex.log")
	session := appserver.NewSessionProbeForTest(logPath, "turn-test", 1<<20, nil)

	stream := strings.Join([]string{
		mustRPCLine(t, map[string]any{
			"method": "item/agentMessage/delta",
			"params": map[string]any{"threadId": "thread-test", "turnId": "turn-test", "itemId": "item-1", "delta": "Hello "},
		}),
		mustRPCLine(t, map[string]any{
			"method": "item/agentMessage/delta",
			"params": map[string]any{"threadId": "thread-test", "turnId": "turn-test", "itemId": "item-1", "delta": "world"},
		}),
		mustRPCLine(t, map[string]any{
			"method": "turn/completed",
			"params": map[string]any{"threadId": "thread-test", "turn": map[string]any{"id": "turn-test", "items": []any{}, "status": "completed"}},
		}),
	}, "\n") + "\n"

	session.ReadStdout(stream)

	if err := session.Err(); err != nil {
		t.Fatalf("readStdout completed with error: %v", err)
	}
	if stdout := session.ResultStdout(); stdout != "Hello world" {
		t.Fatalf("stdout = %q, want delta-assembled report", stdout)
	}
	content, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	logText := string(content)
	for _, forbidden := range []string{`"params"`, `"delta"`, "Hello world"} {
		if strings.Contains(logText, forbidden) {
			t.Fatalf("log should contain compact event summaries, not raw delta JSON/content %q:\n%s", forbidden, logText)
		}
	}
	if got := strings.Count(logText, "item/agentMessage/delta aggregated"); got != 1 {
		t.Fatalf("aggregated delta log count = %d, want 1:\n%s", got, logText)
	}
	if strings.Contains(logText, "delta_bytes=") {
		t.Fatalf("raw per-delta summaries should not be logged:\n%s", logText)
	}
	for _, want := range []string{"item/agentMessage/delta aggregated", "total_bytes=11", "text_prefix=\"Hello worl\"", "contract_starts=0", "contract_ends=0", "turn/completed"} {
		if !strings.Contains(logText, want) {
			t.Fatalf("compact log missing %q:\n%s", want, logText)
		}
	}
}

func TestAppServerSessionCompactsCompletedItemLogs(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "codex.log")
	session := appserver.NewSessionProbeForTest(logPath, "turn-test", 1<<20, nil)
	report := "# App Server Report\n\nDetailed final text."
	stream := strings.Join([]string{
		mustRPCLine(t, map[string]any{
			"method": "item/completed",
			"params": map[string]any{
				"threadId": "thread-test",
				"turnId":   "turn-test",
				"item":     map[string]any{"id": "item-1", "type": "agentMessage", "text": report},
			},
		}),
		mustRPCLine(t, map[string]any{
			"method": "turn/completed",
			"params": map[string]any{"threadId": "thread-test", "turn": map[string]any{"id": "turn-test", "items": []any{}, "status": "completed"}},
		}),
	}, "\n") + "\n"

	session.ReadStdout(stream)

	if err := session.Err(); err != nil {
		t.Fatalf("readStdout completed with error: %v", err)
	}
	if stdout := session.ResultStdout(); stdout != report {
		t.Fatalf("stdout = %q, want completed item text", stdout)
	}
	content, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	logText := string(content)
	if strings.Contains(logText, report) || strings.Contains(logText, `"text"`) {
		t.Fatalf("completed item log should not include raw report text or JSON envelope:\n%s", logText)
	}
	if !strings.Contains(logText, "item/completed") || !strings.Contains(logText, "text_bytes=") || !strings.Contains(logText, "contract_starts=0") || !strings.Contains(logText, "contract_ends=0") {
		t.Fatalf("completed item compact log missing summary:\n%s", logText)
	}
}

func TestAppServerSessionEmitsActivityPreviewWithoutAgentDelta(t *testing.T) {
	var updates []appserver.Update
	session := appserver.NewSessionProbeForTest("", "turn-test", 0, func(update appserver.Update) {
		updates = append(updates, update)
	})
	stream := strings.Join([]string{
		mustRPCLine(t, map[string]any{
			"method": "item/started",
			"params": map[string]any{
				"threadId": "thread-test",
				"turnId":   "turn-test",
				"item": map[string]any{
					"id":      "call-1",
					"type":    "commandExecution",
					"command": "rg TODO",
				},
			},
		}),
		mustRPCLine(t, map[string]any{
			"method": "item/completed",
			"params": map[string]any{
				"threadId": "thread-test",
				"turnId":   "turn-test",
				"item": map[string]any{
					"id":      "call-1",
					"type":    "commandExecution",
					"command": "rg TODO",
				},
			},
		}),
	}, "\n") + "\n"

	session.ReadStdout(stream)

	if len(updates) != 2 {
		t.Fatalf("activity updates = %#v, want start and completion", updates)
	}
	if !strings.Contains(updates[0].Text, "Codex 正在执行命令: rg TODO") {
		t.Fatalf("activity preview missing command start: %#v", updates)
	}
	if updates[0].Done || updates[1].Done {
		t.Fatalf("activity updates should not mark agent response done: %#v", updates)
	}
	if report := session.FinalReport(); report != "" {
		t.Fatalf("activity preview should not affect final report, got %q", report)
	}
}

func TestAppServerSessionCompletedOnlyAgentMessageEmitsPreview(t *testing.T) {
	var updates []appserver.Update
	session := appserver.NewSessionProbeForTest("", "turn-test", 0, func(update appserver.Update) {
		updates = append(updates, update)
	})

	session.RecordCompletedItem("turn-test", "item-1", "completed-only final text")

	if len(updates) != 1 {
		t.Fatalf("completed-only agent updates = %#v, want one", updates)
	}
	if !updates[0].Done || updates[0].Text != "completed-only final text" {
		t.Fatalf("completed-only update = %#v, want done final text", updates[0])
	}
}

func TestAppServerSessionAggregatesDeltaOnceAfterItemCompleted(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "codex.log")
	session := appserver.NewSessionProbeForTest(logPath, "turn-test", 1<<20, nil)
	stream := strings.Join([]string{
		mustRPCLine(t, map[string]any{
			"method": "item/agentMessage/delta",
			"params": map[string]any{"threadId": "thread-test", "turnId": "turn-test", "itemId": "item-1", "delta": "partial"},
		}),
		mustRPCLine(t, map[string]any{
			"method": "item/completed",
			"params": map[string]any{
				"threadId": "thread-test",
				"turnId":   "turn-test",
				"item":     map[string]any{"id": "item-1", "type": "agentMessage", "text": "completed text"},
			},
		}),
		mustRPCLine(t, map[string]any{
			"method": "turn/completed",
			"params": map[string]any{"threadId": "thread-test", "turn": map[string]any{"id": "turn-test", "items": []any{}, "status": "completed"}},
		}),
	}, "\n") + "\n"

	session.ReadStdout(stream)

	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(content), "item/agentMessage/delta aggregated"); got != 1 {
		t.Fatalf("aggregated delta should be logged once, got %d:\n%s", got, content)
	}
	if stdout := session.ResultStdout(); stdout != "completed text" {
		t.Fatalf("completed item text should win over delta fallback, got %q", stdout)
	}
}

func TestAppServerSessionAggregatesDeltaOnCompleteFallback(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "codex.log")
	session := appserver.NewSessionProbeForTest(logPath, "turn-test", 1<<20, nil)
	session.RecordDelta("turn-test", "item-1", "leftover")
	session.Complete("codex app-server", fmt.Errorf("boom"))

	if !session.DoneClosed() {
		t.Fatal("complete should close done after writing aggregate log")
	}
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(content), "item/agentMessage/delta aggregated"); got != 1 {
		t.Fatalf("fallback aggregate count = %d, want 1:\n%s", got, content)
	}
	if !strings.Contains(string(content), "total_bytes=8") {
		t.Fatalf("fallback aggregate missing byte count:\n%s", content)
	}
}

func TestAppServerSessionBoundsAccumulatedDeltaReport(t *testing.T) {
	session := appserver.NewSessionProbeForTest("", "turn-test", 10, nil)

	session.RecordDelta("turn-test", "item-1", strings.Repeat("a", 8))
	session.RecordDelta("turn-test", "item-1", strings.Repeat("b", 8))

	if got := session.DeltaForItem("item-1"); got != "aaaaaaaabb" {
		t.Fatalf("bounded delta = %q, want first 10 bytes", got)
	}
	if report := session.FinalReport(); len(report) > 10 {
		t.Fatalf("final report exceeded max output bytes: len=%d report=%q", len(report), report)
	}
}

func TestAppServerSessionBoundsCompletedItemReport(t *testing.T) {
	session := appserver.NewSessionProbeForTest("", "turn-test", 10, nil)

	session.RecordCompletedItem("turn-test", "item-1", strings.Repeat("x", 20))

	if report := session.FinalReport(); report != strings.Repeat("x", 10) {
		t.Fatalf("bounded completed report = %q, want 10 bytes", report)
	}
}

func TestAppServerSessionAggregatedDeltaPrefixUsesRunes(t *testing.T) {
	text := "一二三四五六七八九十十一🙂"
	line := appserver.FormatAggregatedDeltaLogLineForTest("turn", "item", text)
	if !utf8.ValidString(line) {
		t.Fatalf("aggregated log line is not valid UTF-8: %q", line)
	}
	if !strings.Contains(line, `text_prefix="一二三四五六七八九十"`) {
		t.Fatalf("prefix should be the first 10 runes:\n%s", line)
	}
	if strings.Contains(line, "十一") || strings.Contains(line, "🙂") {
		t.Fatalf("prefix should not include content after first 10 runes:\n%s", line)
	}
}

func TestAppServerSessionLateDeltaAfterItemDoneDoesNotReopenPreview(t *testing.T) {
	var updates []appserver.Update
	session := appserver.NewSessionProbeForTest("", "turn-test", 0, func(update appserver.Update) {
		updates = append(updates, update)
	})
	session.RecordDelta("turn-test", "item-1", "hello")
	session.RecordCompletedItem("turn-test", "item-1", "completed")
	session.RecordDelta("turn-test", "item-1", " late")

	if len(updates) != 2 {
		t.Fatalf("late delta should not emit a third update: %#v", updates)
	}
	if !updates[1].Done || updates[1].Text != "completed" {
		t.Fatalf("item completion should emit done update with completed text: %#v", updates)
	}
	if report, delta := session.FinalReport(), session.DeltaForItem("item-1"); report != "completed" || delta != "hello late" {
		t.Fatalf("report/delta = %q / %q, want completed text and diagnostic late delta", report, delta)
	}
}

func TestAppServerSessionLateDeltaAfterCompletedOnlyDoesNotDuplicateReport(t *testing.T) {
	session := appserver.NewSessionProbeForTest("", "turn-test", 0, nil)
	session.RecordCompletedItem("turn-test", "item-1", "completed")
	session.RecordDelta("turn-test", "item-1", "late")

	if report, orderLen := session.FinalReport(), session.ItemOrderLen(); report != "completed" || orderLen != 1 {
		t.Fatalf("late delta after completed-only item duplicated report/order: report=%q orderLen=%d", report, orderLen)
	}
}

func mustRPCLine(t *testing.T, payload map[string]any) string {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
