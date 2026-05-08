package executor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
