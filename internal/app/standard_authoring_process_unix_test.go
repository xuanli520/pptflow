//go:build unix

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStandardAuthoringGitCancellationTerminatesTransportProcessGroup(t *testing.T) {
	directory := t.TempDir()
	pidPath := filepath.Join(directory, "child.pid")
	scriptPath := filepath.Join(directory, "git")
	script := fmt.Sprintf("#!/bin/sh\nsleep 30 &\necho $! > %q\nwait\n", pidPath)
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	capturer, err := NewStandardAuthoringGitArchiveSourceCapturer(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = capturer.runGit(ctx, directory, "fetch")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled Git command error = %v", err)
	}
	raw, readErr := os.ReadFile(pidPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if parseErr != nil || processStillRunning(pid) {
		t.Fatalf("Git transport child remained running: pid=%d parse=%v", pid, parseErr)
	}
}

func processStillRunning(pid int) bool {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	fields := strings.Fields(string(raw))
	return len(fields) < 3 || fields[2] != "Z"
}
