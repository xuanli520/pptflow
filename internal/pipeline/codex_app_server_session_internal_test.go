package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/executor"
)

func TestAppServerSessionIgnoresClosedPipeErrorsAfterCompletion(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "codex.log")
	session := &appServerCodexReviewSession{
		req:       CodexReviewRequest{LogPath: logPath},
		done:      make(chan struct{}),
		responses: map[int]chan appServerRPCMessage{},
		items:     map[string]string{},
		deltas:    map[string]string{},
	}

	session.complete(executor.Result{Command: "codex app-server"}, nil)
	session.completeStreamError("stdout", fmt.Errorf("read |0: %w", os.ErrClosed))
	session.completeStreamError("stderr", fmt.Errorf("read |0: %w", os.ErrClosed))

	session.mu.Lock()
	err := session.err
	session.mu.Unlock()
	if err != nil {
		t.Fatalf("closed pipe after completion changed result error: %v", err)
	}
	content, readErr := os.ReadFile(logPath)
	if readErr == nil && strings.Contains(string(content), "stream error") {
		t.Fatalf("closed pipe after completion should not be logged as an error:\n%s", content)
	}
}

func TestAppServerSessionKeepsPrematureClosedPipeErrors(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "codex.log")
	session := &appServerCodexReviewSession{
		req:       CodexReviewRequest{LogPath: logPath},
		done:      make(chan struct{}),
		responses: map[int]chan appServerRPCMessage{},
		items:     map[string]string{},
		deltas:    map[string]string{},
	}

	session.completeStreamError("stdout", fmt.Errorf("read |0: %w", os.ErrClosed))

	session.mu.Lock()
	err := session.err
	session.mu.Unlock()
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
	cancel()
	session := &appServerCodexReviewSession{
		req:        CodexReviewRequest{LogPath: logPath},
		processCtx: processCtx,
		done:       make(chan struct{}),
		responses:  map[int]chan appServerRPCMessage{},
		items:      map[string]string{},
		deltas:     map[string]string{},
	}

	session.completeStreamError("stdout", fmt.Errorf("read |0: %w", os.ErrClosed))

	session.mu.Lock()
	err := session.err
	completed := session.completed
	session.mu.Unlock()
	if err != nil || completed {
		t.Fatalf("cancel-driven closed pipe should wait for process result, completed=%t err=%v", completed, err)
	}
	content, readErr := os.ReadFile(logPath)
	if readErr == nil && strings.Contains(string(content), "stream error") {
		t.Fatalf("cancel-driven closed pipe should not be logged as stream error:\n%s", content)
	}
}

func TestAppServerSessionCompactsDeltaLogsAndKeepsDeltaReport(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "codex.log")
	session := &appServerCodexReviewSession{
		req:       CodexReviewRequest{LogPath: logPath, MaxOutputBytes: 1 << 20},
		done:      make(chan struct{}),
		responses: map[int]chan appServerRPCMessage{},
		turnID:    "turn-test",
		items:     map[string]string{},
		deltas:    map[string]string{},
	}

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

	session.readStdout(strings.NewReader(stream))

	session.mu.Lock()
	stdout := session.result.Result.Stdout
	err := session.err
	session.mu.Unlock()
	if err != nil {
		t.Fatalf("readStdout completed with error: %v", err)
	}
	if stdout != "Hello world" {
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
	for _, want := range []string{"item/agentMessage/delta", "delta_bytes=6", "delta_bytes=5", "contract_starts=0", "contract_ends=0", "turn/completed"} {
		if !strings.Contains(logText, want) {
			t.Fatalf("compact log missing %q:\n%s", want, logText)
		}
	}
}

func TestAppServerSessionCompactsCompletedItemLogs(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "codex.log")
	session := &appServerCodexReviewSession{
		req:       CodexReviewRequest{LogPath: logPath, MaxOutputBytes: 1 << 20},
		done:      make(chan struct{}),
		responses: map[int]chan appServerRPCMessage{},
		turnID:    "turn-test",
		items:     map[string]string{},
		deltas:    map[string]string{},
	}
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

	session.readStdout(strings.NewReader(stream))

	session.mu.Lock()
	stdout := session.result.Result.Stdout
	err := session.err
	session.mu.Unlock()
	if err != nil {
		t.Fatalf("readStdout completed with error: %v", err)
	}
	if stdout != report {
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

func mustRPCLine(t *testing.T, payload map[string]any) string {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
