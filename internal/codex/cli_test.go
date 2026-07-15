package codex

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/executor"
)

func TestInspectCLIRequiresAbsoluteExecutableAndNeverDiscoversPath(t *testing.T) {
	runner := &recordingRunner{}
	capability := InspectCLI(context.Background(), runner, "codex")
	if capability.DetectionError == "" || !strings.Contains(capability.DetectionError, "must be absolute") {
		t.Fatalf("relative path capability = %+v", capability)
	}
	if runner.lookPathCalls != 0 || len(runner.calls) != 0 {
		t.Fatalf("InspectCLI unexpectedly discovered or probed a relative command: lookPath=%d calls=%v", runner.lookPathCalls, runner.calls)
	}

	path := writeExecutable(t)
	capability = InspectCLI(context.Background(), runner, path)
	if capability.Path != path || !capability.HasAppServer || !capability.HasConfig || capability.DetectionError != "" {
		t.Fatalf("explicit capability = %+v", capability)
	}
	if runner.lookPathCalls != 0 {
		t.Fatalf("InspectCLI must not call LookPath: %d", runner.lookPathCalls)
	}
	if err := ValidateControlledAppServerCapability(capability); err != nil {
		t.Fatalf("validate explicit capability: %v", err)
	}
}

func TestDetectCLIRetainsDiagnosticPathDiscoveryOnly(t *testing.T) {
	runner := &recordingRunner{lookPathResult: "/explicit/from-diagnostic"}
	capability := DetectCLI(context.Background(), runner, "")
	if runner.lookPathCalls != 1 {
		t.Fatalf("diagnostic discovery calls = %d, want 1", runner.lookPathCalls)
	}
	if capability.Path != "/explicit/from-diagnostic" {
		t.Fatalf("diagnostic capability path = %q", capability.Path)
	}
}

func TestValidateControlledAppServerCapabilityRejectsProbeError(t *testing.T) {
	err := ValidateControlledAppServerCapability(Capability{
		Path:           "/approved/codex",
		HasAppServer:   true,
		HasConfig:      true,
		DetectionError: "permission denied",
	})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("probe error validation = %v", err)
	}
}

func TestInspectCLIWithEnvironmentUsesOnlySuppliedEnvironment(t *testing.T) {
	path := writeExecutable(t)
	t.Setenv("CODEX_HOME", "/ambient/home")
	t.Setenv("OPENAI_API_KEY", "ambient-key")
	runner := &recordingRunner{}
	capability := InspectCLIWithEnvironment(context.Background(), runner, path, []string{
		"PATH=/approved/bin",
		"CODEX_HOME=/approved/home",
	})
	if capability.DetectionError != "" || capability.NodePath != "" {
		t.Fatalf("controlled capability = %+v", capability)
	}
	if !strings.Contains(capability.NodeDetectionMessage, "configured process environment") {
		t.Fatalf("controlled node behavior = %q", capability.NodeDetectionMessage)
	}
	if len(runner.envs) != 2 {
		t.Fatalf("probe environment count = %d", len(runner.envs))
	}
	for _, env := range runner.envs {
		got := strings.Join(env, "\n")
		if strings.Contains(got, "/ambient/home") || strings.Contains(got, "ambient-key") {
			t.Fatalf("controlled probe inherited ambient environment: %q", got)
		}
		if !strings.Contains(got, "CODEX_HOME=/approved/home") {
			t.Fatalf("controlled probe missed configured environment: %q", got)
		}
	}
}

type recordingRunner struct {
	lookPathCalls  int
	lookPathResult string
	calls          [][]string
	envs           [][]string
}

func (r *recordingRunner) LookPath(string) (string, error) {
	r.lookPathCalls++
	if r.lookPathResult == "" {
		return "", os.ErrNotExist
	}
	return r.lookPathResult, nil
}

func (r *recordingRunner) Run(_ context.Context, _ time.Duration, _ string, env []string, _ string, args ...string) executor.Result {
	r.calls = append(r.calls, append([]string(nil), args...))
	r.envs = append(r.envs, append([]string(nil), env...))
	switch strings.Join(args, " ") {
	case "--version":
		return executor.Result{Stdout: "codex 1.2.3\n"}
	case "app-server --help":
		return executor.Result{Stdout: "--listen\n--config\n"}
	default:
		return executor.Result{}
	}
}

func (r *recordingRunner) RunStreamingWithOutput(context.Context, time.Duration, string, []string, io.Writer, executor.OutputCallback, string, ...string) executor.Result {
	return executor.Result{}
}

func writeExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
