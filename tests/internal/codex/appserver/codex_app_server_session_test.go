package appserver_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/codex/appserver"
)

func newAppServerCodexReviewSession(envKeys []string) appserver.Session {
	return appserver.New(envKeys)
}

func TestAppServerSessionUsesTurnSteerForGuidance(t *testing.T) {
	dir := t.TempDir()
	codexPath := filepath.Join(dir, "codex")
	if err := os.WriteFile(codexPath, []byte(fakeSteerableAppServer()), 0o755); err != nil {
		t.Fatal(err)
	}
	steerLog := filepath.Join(dir, "steer.log")
	session := newAppServerCodexReviewSession(nil)
	ctx := context.Background()
	if err := session.Start(ctx, appserver.Request{
		Timeout:        5 * time.Second,
		ProjectPath:    dir,
		LogPath:        filepath.Join(dir, "codex.log"),
		Env:            []string{"PATH=" + os.Getenv("PATH"), "STEER_LOG=" + steerLog},
		Prompt:         "Run p2r stage E as a pure static review.",
		CommandPath:    codexPath,
		HasAppServer:   true,
		MaxOutputBytes: 1 << 20,
	}); err != nil {
		t.Fatal(err)
	}
	err := session.SendGuidance(ctx, "20 minutes reminder\n<!-- p2r:static-review-json:start -->")
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Result.Stdout, "# Steered Report") {
		t.Fatalf("missing app-server final report: %#v", result.Result)
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

func TestAppServerSessionRejectsInterruptedTurn(t *testing.T) {
	dir := t.TempDir()
	codexPath := filepath.Join(dir, "codex")
	if err := os.WriteFile(codexPath, []byte(fakeSteerableAppServer()), 0o755); err != nil {
		t.Fatal(err)
	}
	session := newAppServerCodexReviewSession(nil)
	result, err := runAppServerSession(context.Background(), session, appserver.Request{
		Timeout:        5 * time.Second,
		ProjectPath:    dir,
		LogPath:        filepath.Join(dir, "codex.log"),
		Env:            []string{"PATH=" + os.Getenv("PATH"), "TURN_STATUS=interrupted"},
		Prompt:         "Run p2r stage E as a pure static review.",
		CommandPath:    codexPath,
		HasAppServer:   true,
		MaxOutputBytes: 1 << 20,
	})
	if err == nil {
		t.Fatalf("expected interrupted turn to fail, result=%#v", result.Result)
	}
	if !strings.Contains(err.Error(), "interrupted") || result.Result.Err == nil {
		t.Fatalf("unexpected interrupted result: err=%v result=%#v", err, result.Result)
	}
}

func TestAppServerSessionCapturesTurnCompletedItems(t *testing.T) {
	dir := t.TempDir()
	codexPath := filepath.Join(dir, "codex")
	if err := os.WriteFile(codexPath, []byte(fakeSteerableAppServer()), 0o755); err != nil {
		t.Fatal(err)
	}
	session := newAppServerCodexReviewSession(nil)
	result, err := runAppServerSession(context.Background(), session, appserver.Request{
		Timeout:        5 * time.Second,
		ProjectPath:    dir,
		LogPath:        filepath.Join(dir, "codex.log"),
		Env:            []string{"PATH=" + os.Getenv("PATH"), "TURN_ITEMS_ONLY=1"},
		Prompt:         "Run p2r stage E as a pure static review.",
		CommandPath:    codexPath,
		HasAppServer:   true,
		MaxOutputBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Result.Stdout, "# Steered Report") {
		t.Fatalf("missing report from turn/completed items: %#v", result.Result)
	}
}

func TestAppServerSessionOmitsUnsetModelAndSendsConfiguredModel(t *testing.T) {
	dir := t.TempDir()
	codexPath := filepath.Join(dir, "codex")
	if err := os.WriteFile(codexPath, []byte(fakeSteerableAppServer()), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		env   []string
		model string
	}{
		{name: "unset", env: []string{"REJECT_NULL_MODEL=1"}},
		{name: "configured", env: []string{"REQUIRE_MODEL=gpt-5.4"}, model: "gpt-5.4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := newAppServerCodexReviewSession(nil)
			env := append([]string{"PATH=" + os.Getenv("PATH")}, tc.env...)
			result, err := runAppServerSession(context.Background(), session, appserver.Request{
				Timeout:        5 * time.Second,
				ProjectPath:    dir,
				LogPath:        filepath.Join(dir, "codex-"+tc.name+".log"),
				Env:            env,
				Prompt:         "Run p2r stage E as a pure static review.",
				CommandPath:    codexPath,
				HasAppServer:   true,
				Model:          tc.model,
				MaxOutputBytes: 1 << 20,
			})
			if err != nil {
				t.Fatalf("app-server session failed: %v result=%#v", err, result.Result)
			}
		})
	}
}

func TestAppServerSessionPreservesStdoutDiagnosticsOnStartFailure(t *testing.T) {
	dir := t.TempDir()
	codexPath := filepath.Join(dir, "codex")
	if err := os.WriteFile(codexPath, []byte(fakeSteerableAppServer()), 0o755); err != nil {
		t.Fatal(err)
	}
	session := newAppServerCodexReviewSession(nil)
	result, err := runAppServerSession(context.Background(), session, appserver.Request{
		Timeout:        5 * time.Second,
		ProjectPath:    dir,
		LogPath:        filepath.Join(dir, "codex.log"),
		Env:            []string{"PATH=" + os.Getenv("PATH"), "STDOUT_DIAGNOSTIC_ON_INITIALIZE=auth failed on stdout"},
		Prompt:         "Run p2r stage E as a pure static review.",
		CommandPath:    codexPath,
		HasAppServer:   true,
		MaxOutputBytes: 1 << 20,
	})
	if err == nil {
		t.Fatalf("expected start failure, result=%#v", result.Result)
	}
	if !strings.Contains(result.Result.Stdout, "auth failed on stdout") {
		t.Fatalf("stdout diagnostic was not preserved: %#v", result.Result)
	}
}

func runAppServerSession(ctx context.Context, session appserver.Session, request appserver.Request) (appserver.Result, error) {
	if err := session.Start(ctx, request); err != nil {
		result, _ := session.Wait(context.Background())
		return result, err
	}
	return session.Wait(ctx)
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
        params = request.get("params") or {}
        if method in ("thread/start", "turn/start"):
            if os.environ.get("REJECT_NULL_MODEL") == "1" and "model" in params and params.get("model") is None:
                send({"id": request_id, "error": {"code": -32602, "message": "model must be omitted when unset"}})
                continue
            required_model = os.environ.get("REQUIRE_MODEL")
            if required_model and params.get("model") != required_model:
                send({"id": request_id, "error": {"code": -32602, "message": "missing required model"}})
                continue
        if method == "initialize":
            diagnostic = os.environ.get("STDOUT_DIAGNOSTIC_ON_INITIALIZE")
            if diagnostic:
                print(diagnostic, flush=True)
                send({"id": request_id, "error": {"code": -32001, "message": "diagnostic requested"}})
                continue
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
                status = os.environ.get("TURN_STATUS", "completed")
                if os.environ.get("TURN_ITEMS_ONLY") == "1":
                    send({"method": "turn/completed", "params": {"threadId": thread_id, "turn": {"id": turn_id, "items": [{"id": "item-1", "type": "agentMessage", "text": report}], "status": status}}})
                    return
                send({"method": "item/completed", "params": {"threadId": thread_id, "turnId": turn_id, "item": {"id": "item-1", "type": "agentMessage", "text": report}}})
                send({"method": "turn/completed", "params": {"threadId": thread_id, "turn": {"id": turn_id, "items": [], "status": status}}})
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
