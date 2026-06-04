package appserver_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/codex/appserver"
)

func TestMain(m *testing.M) {
	if os.Getenv("P2R_FAKE_STEERABLE_APPSERVER") == "1" {
		os.Exit(runFakeSteerableAppServer())
	}
	os.Exit(m.Run())
}

func newAppServerCodexReviewSession(envKeys []string) appserver.Session {
	return appserver.New(envKeys)
}

func TestAppServerSessionUsesTurnSteerForGuidance(t *testing.T) {
	dir := t.TempDir()
	codexPath := writeFakeSteerableAppServer(t, dir)
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
	codexPath := writeFakeSteerableAppServer(t, dir)
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
	codexPath := writeFakeSteerableAppServer(t, dir)
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
	codexPath := writeFakeSteerableAppServer(t, dir)
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
	codexPath := writeFakeSteerableAppServer(t, dir)
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

func writeFakeSteerableAppServer(t *testing.T, dir string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		codexPath := filepath.Join(dir, "codex")
		content := "#!/usr/bin/env sh\nP2R_FAKE_STEERABLE_APPSERVER=1 exec \"" + exe + "\" \"$@\"\n"
		if err := os.WriteFile(codexPath, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		return codexPath
	}
	codexPath := filepath.Join(dir, "codex.cmd")
	wrapper := "@echo off\r\nset P2R_FAKE_STEERABLE_APPSERVER=1\r\n\"" + exe + "\" %*\r\n"
	if err := os.WriteFile(codexPath, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	return codexPath
}

func runFakeSteerableAppServer() int {
	if len(os.Args) > 1 && os.Args[1] == "app-server" && hasArg("--help") {
		fmt.Println("Usage: codex app-server [OPTIONS] [COMMAND]")
		fmt.Println("  -c, --config <KEY=VALUE>")
		fmt.Println("      --listen <URL>")
		return 0
	}
	if len(os.Args) <= 1 || os.Args[1] != "app-server" {
		fmt.Fprintln(os.Stderr, "unexpected fake codex args: "+strings.Join(os.Args[1:], " "))
		return 2
	}
	threadID := "thread-test"
	turnID := "turn-test"
	steerLog := os.Getenv("STEER_LOG")
	initialized := false
	send := func(payload map[string]any) {
		_ = json.NewEncoder(os.Stdout).Encode(payload)
	}
	report := `# Steered Report

<!-- p2r:static-review-json:start -->
{
  "schema_version": "p2r.static_review.v1",
  "stage": "E",
  "findings": []
}
<!-- p2r:static-review-json:end -->
`
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var request map[string]any
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			continue
		}
		requestID := request["id"]
		method, _ := request["method"].(string)
		params, _ := request["params"].(map[string]any)
		if method == "thread/start" || method == "turn/start" {
			if os.Getenv("REJECT_NULL_MODEL") == "1" {
				if value, ok := params["model"]; ok && value == nil {
					send(map[string]any{"id": requestID, "error": map[string]any{"code": -32602, "message": "model must be omitted when unset"}})
					continue
				}
			}
			if required := os.Getenv("REQUIRE_MODEL"); required != "" && params["model"] != required {
				send(map[string]any{"id": requestID, "error": map[string]any{"code": -32602, "message": "missing required model"}})
				continue
			}
		}
		switch method {
		case "initialize":
			if diagnostic := os.Getenv("STDOUT_DIAGNOSTIC_ON_INITIALIZE"); diagnostic != "" {
				fmt.Println(diagnostic)
				send(map[string]any{"id": requestID, "error": map[string]any{"code": -32001, "message": "diagnostic requested"}})
				continue
			}
			send(map[string]any{"id": requestID, "result": map[string]any{"userAgent": "fake-codex", "codexHome": "/tmp/fake", "platformFamily": "unix", "platformOs": "linux"}})
		case "initialized":
			initialized = true
		case "thread/start":
			if !initialized {
				send(map[string]any{"id": requestID, "error": map[string]any{"code": -32000, "message": "missing initialized notification"}})
				continue
			}
			send(map[string]any{"id": requestID, "result": map[string]any{"thread": map[string]any{"id": threadID}}})
		case "turn/start":
			send(map[string]any{"id": requestID, "result": map[string]any{"turn": map[string]any{"id": turnID, "items": []any{}, "status": "running"}}})
			go func() {
				time.Sleep(200 * time.Millisecond)
				status := os.Getenv("TURN_STATUS")
				if status == "" {
					status = "completed"
				}
				if os.Getenv("TURN_ITEMS_ONLY") == "1" {
					send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "items": []any{map[string]any{"id": "item-1", "type": "agentMessage", "text": report}}, "status": status}}})
					return
				}
				send(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": threadID, "turnId": turnID, "item": map[string]any{"id": "item-1", "type": "agentMessage", "text": report}}})
				send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "items": []any{}, "status": status}}})
			}()
		case "turn/steer":
			if steerLog != "" {
				if input, ok := params["input"].([]any); ok && len(input) > 0 {
					if item, ok := input[0].(map[string]any); ok {
						if text, ok := item["text"].(string); ok {
							_ = os.WriteFile(steerLog, []byte(text), 0o644)
						}
					}
				}
			}
			send(map[string]any{"id": requestID, "result": map[string]any{"turnId": turnID}})
		default:
			send(map[string]any{"id": requestID, "error": map[string]any{"code": -32601, "message": "unknown"}})
		}
	}
	return 0
}

func hasArg(target string) bool {
	for _, arg := range os.Args[1:] {
		if arg == target {
			return true
		}
	}
	return false
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
