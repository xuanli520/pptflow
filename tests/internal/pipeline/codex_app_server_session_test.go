package pipeline_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	_ "unsafe"

	"github.com/xuanli520/p2r_tui/internal/codex"
	pipelinepkg "github.com/xuanli520/p2r_tui/internal/pipeline"
)

//go:linkname newAppServerCodexReviewSession github.com/xuanli520/p2r_tui/internal/pipeline.newAppServerCodexReviewSession
func newAppServerCodexReviewSession(envKeys []string) pipelinepkg.CodexReviewSession

func TestAppServerSessionUsesTurnSteerForGuidance(t *testing.T) {
	dir := t.TempDir()
	codexPath := filepath.Join(dir, "codex")
	if err := os.WriteFile(codexPath, []byte(fakeSteerableAppServer()), 0o755); err != nil {
		t.Fatal(err)
	}
	steerLog := filepath.Join(dir, "steer.log")
	session := newAppServerCodexReviewSession(nil)
	deadline := codexGuidanceSchedule(45*time.Minute, "E")[0]
	deadline.Label = "test guidance"
	deadline.After = 20 * time.Millisecond
	result, err := runCodexReviewSessionWithGuidance(context.Background(), session, pipelinepkg.CodexReviewRequest{
		Timeout:        5 * time.Second,
		ProjectPath:    dir,
		LogPath:        filepath.Join(dir, "codex.log"),
		Env:            []string{"PATH=" + os.Getenv("PATH"), "STEER_LOG=" + steerLog},
		Prompt:         "Run p2r stage E as a pure static review.",
		Capability:     codex.Capability{Path: codexPath, HasAppServer: true, HasConfig: true},
		MaxOutputBytes: 1 << 20,
	}, []pipelinepkg.CodexGuidanceDeadline{deadline})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Result.Stdout, "# Steered Report") {
		t.Fatalf("missing app-server final report: %#v", result.Result)
	}
	if len(result.GuidanceEvents) != 1 || result.GuidanceEvents[0].Error != "" {
		t.Fatalf("unexpected guidance events: %#v", result.GuidanceEvents)
	}
	content, err := os.ReadFile(steerLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"20 minutes", "<!-- p2r:static-review-json:start -->"} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("steer log missing %q:\n%s", want, content)
		}
	}
}

func fakeSteerableAppServer() string {
	return `#!/usr/bin/env python3
import json
import os
import sys
import threading
import time

if len(sys.argv) > 1 and sys.argv[1] == "app-server" and "--help" in sys.argv:
    print("Usage: codex app-server [OPTIONS] [COMMAND]")
    print("  -c, --config <KEY=VALUE>")
    print("      --listen <URL>")
    sys.exit(0)

if len(sys.argv) > 1 and sys.argv[1] == "app-server":
    thread_id = "thread-test"
    turn_id = "turn-test"
    steer_log = os.environ.get("STEER_LOG")
    initialized = False

    def send(payload):
        print(json.dumps(payload), flush=True)

    report = """# Steered Report

<!-- p2r:static-review-json:start -->
{
  "schema_version": "p2r.static_review.v1",
  "stage": "E",
  "findings": []
}
<!-- p2r:static-review-json:end -->
"""

    for line in sys.stdin:
        if not line.strip():
            continue
        request = json.loads(line)
        request_id = request.get("id")
        method = request.get("method")
        if method == "initialize":
            send({"id": request_id, "result": {"userAgent": "fake-codex", "codexHome": "/tmp/fake", "platformFamily": "unix", "platformOs": "linux"}})
        elif method == "initialized":
            initialized = True
        elif method == "thread/start":
            if not initialized:
                send({"id": request_id, "error": {"code": -32000, "message": "missing initialized notification"}})
                continue
            send({"id": request_id, "result": {"thread": {"id": thread_id}}})
        elif method == "turn/start":
            send({"id": request_id, "result": {"turn": {"id": turn_id, "items": [], "status": "running"}}})
            def complete():
                time.sleep(0.2)
                send({"method": "item/completed", "params": {"threadId": thread_id, "turnId": turn_id, "item": {"id": "item-1", "type": "agentMessage", "text": report}}})
                send({"method": "turn/completed", "params": {"threadId": thread_id, "turn": {"id": turn_id, "items": [], "status": "completed"}}})
            threading.Thread(target=complete, daemon=True).start()
        elif method == "turn/steer":
            if steer_log:
                with open(steer_log, "a", encoding="utf-8") as handle:
                    handle.write(request["params"]["input"][0]["text"])
            send({"id": request_id, "result": {"turnId": turn_id}})
        else:
            send({"id": request_id, "error": {"code": -32601, "message": "unknown"}})
    sys.exit(0)

sys.exit(2)
`
}
