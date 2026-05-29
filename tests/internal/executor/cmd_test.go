package executor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/executor"
)

func TestRunTimeoutKillsSpawnedProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group timeout behavior is Unix-specific")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "spawn-child.sh")
	if err := os.WriteFile(script, []byte(`#!/usr/bin/env bash
set -euo pipefail
(while true; do echo child-still-writing; sleep 0.1; done) &
wait
`), 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	result := executor.New().Run(context.Background(), 100*time.Millisecond, dir, nil, script)
	if !result.Timeout {
		t.Fatalf("expected timeout, got result: %#v", result)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout command took too long to return: %s", elapsed)
	}
}

func TestRunTimeoutAllowsShellTrapBeforeKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell trap timeout behavior is Unix-specific")
	}
	dir := t.TempDir()
	trapFile := filepath.Join(dir, "trap-ran")
	script := filepath.Join(dir, "trap.sh")
	if err := os.WriteFile(script, []byte(`#!/usr/bin/env bash
set -euo pipefail
trap 'sleep 3; echo trap > trap-ran' EXIT
sleep 10
`), 0o755); err != nil {
		t.Fatal(err)
	}

	result := executor.New().Run(context.Background(), 100*time.Millisecond, dir, nil, script)
	if !result.Timeout {
		t.Fatalf("expected timeout, got result: %#v", result)
	}
	content, err := os.ReadFile(trapFile)
	if err != nil {
		t.Fatalf("expected EXIT trap to run before hard kill: %v", err)
	}
	if strings.TrimSpace(string(content)) != "trap" {
		t.Fatalf("unexpected trap file content: %q", content)
	}
}

func TestRunStreamingWithOutputCapturesStdoutAndStderrLines(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell streaming test is Unix-specific")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "stream.sh")
	if err := os.WriteFile(script, []byte(`#!/usr/bin/env bash
set -euo pipefail
echo out-one
echo err-one >&2
echo out-two
printf tail-without-newline
`), 0o755); err != nil {
		t.Fatal(err)
	}

	var writer strings.Builder
	var events []string
	result := executor.New().RunStreamingWithOutput(context.Background(), time.Second, dir, nil, &writer, func(line string, source string) {
		events = append(events, source+":"+strings.TrimSpace(line))
	}, script)

	if result.Err != nil {
		t.Fatalf("streaming command failed: %#v", result)
	}
	if !strings.Contains(result.Stdout, "out-one") || !strings.Contains(result.Stdout, "out-two") || !strings.Contains(result.Stdout, "tail-without-newline") || !strings.Contains(result.Stderr, "err-one") {
		t.Fatalf("stdout/stderr not captured: %#v", result)
	}
	if !strings.Contains(writer.String(), "out-one") || !strings.Contains(writer.String(), "err-one") {
		t.Fatalf("writer did not receive both streams:\n%s", writer.String())
	}
	got := strings.Join(events, ",")
	for _, want := range []string{"stdout:out-one", "stdout:out-two", "stdout:tail-without-newline", "stderr:err-one"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing output callback %q in %q", want, got)
		}
	}
}

func TestRunStreamingWithOutputReportsWriterErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell streaming test is Unix-specific")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "stream.sh")
	if err := os.WriteFile(script, []byte(`#!/usr/bin/env bash
set -euo pipefail
echo out-one
`), 0o755); err != nil {
		t.Fatal(err)
	}

	result := executor.New().RunStreamingWithOutput(context.Background(), time.Second, dir, nil, failingWriter{}, nil, script)

	if result.Err == nil || !strings.Contains(result.Err.Error(), "write boom") {
		t.Fatalf("writer error should be returned, got %#v", result)
	}
	if !strings.Contains(result.Stdout, "out-one") {
		t.Fatalf("stdout should still capture bytes before writer failure: %#v", result)
	}
}

func TestRunParentCancelIsNotTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell cancellation test is Unix-specific")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "sleep.sh")
	if err := os.WriteFile(script, []byte(`#!/usr/bin/env bash
set -euo pipefail
sleep 5
`), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
		close(done)
	}()
	result := executor.New().Run(ctx, 5*time.Second, dir, nil, script)
	<-done
	if !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", result.Err)
	}
	if result.Timeout {
		t.Fatalf("parent cancellation should not be reported as timeout: %#v", result)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write boom")
}
