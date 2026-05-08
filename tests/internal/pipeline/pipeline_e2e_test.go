package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	pipelinepkg "github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func TestRunPersistsRunningStageAndStreamsCodexLog(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "batch-1", "TASK-HANG", "TASK-HANG")
	for _, dir := range []string{"docs", "repo", "original_sessions"} {
		if err := os.MkdirAll(filepath.Join(projectDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectDir, "metadata.json"), []byte(`{"task_id":"TASK-HANG","prompt":"build a small app"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "repo", "self_test_report.md"), []byte("self test passed"), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "codex"), `#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = "--version" ]; then
  echo "codex-cli 0.999.0"
  exit 0
fi
if [ "${1:-}" = "exec" ] && [ "${2:-}" = "--help" ]; then
  echo "--sandbox --ask-for-approval --cd -C --ephemeral --skip-git-repo-check --ignore-user-config"
  exit 0
fi
if [ "${1:-}" = "exec" ]; then
  prompt="$(cat)"
  stage="D"
  if printf '%s' "$prompt" | grep -q 'Run p2r stage F'; then
    stage="F"
  elif printf '%s' "$prompt" | grep -q 'Run p2r stage E'; then
    stage="E"
  fi
  echo "fake-codex-start" >&2
  sleep "${FAKE_CODEX_SLEEP:-2}"
  echo "# Fake Report"
  echo "- High: simulated finding from fake codex"
  cat <<JSON
<!-- p2r:static-review-json:start -->
{
  "schema_version": "p2r.static_review.v1",
  "stage": "$stage",
  "findings": [
    {
      "severity": "High",
      "title": "simulated finding from fake codex",
      "rule": "Fake Codex test rule",
      "evidence": "repo/fake.go:1",
      "impact": "The fake reviewer reported a controlled issue.",
      "minimum_fix": "Keep the fake output contract valid."
    }
  ]
}
<!-- p2r:static-review-json:end -->
JSON
  exit 0
fi
echo "unexpected fake codex args: $*" >&2
exit 2
`)
	writeExecutable(t, filepath.Join(fakeBin, "node"), `#!/usr/bin/env bash
echo "v25.0.0"
`)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CODEX_SLEEP", "2")

	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(root, ".qa-control", "index.db")
	cfg.Codex.PromptProfilesDir = filepath.Join(root, ".qa-control", "prompt_profiles")
	cfg.Pipeline.StageTimeouts["D"] = 10
	cfg.Pipeline.StageTimeouts["F"] = 10
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scan, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertProjects(context.Background(), scan.Projects); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resultCh := make(chan error, 1)
	go func() {
		_, err := pipelinepkg.NewRunner(store, cfg).Run(ctx, "TASK-HANG", pipelinepkg.RunOptions{Stage: "D"})
		resultCh <- err
	}()

	run := waitForLatestRun(t, ctx, store, "TASK-HANG")
	if run.Status != model.RunRunning {
		t.Fatalf("latest run status while command is active = %s, want running", run.Status)
	}
	waitForStageLog(t, ctx, store, run.RunID, "D", "fake-codex-start")

	if err := <-resultCh; err != nil {
		t.Fatal(err)
	}
	finalRun, err := store.GetRun(context.Background(), run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if finalRun.Status == model.RunRunning {
		t.Fatalf("run stayed running after completion: %#v", finalRun)
	}
}

func TestRunStageACancelPersistsAbortedRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake PATH python shim is Unix-specific")
	}
	root := t.TempDir()
	projectDir := filepath.Join(root, "batch-1", "TASK-CANCEL", "TASK-CANCEL")
	for _, dir := range []string{"docs", "repo", "original_sessions"} {
		if err := os.MkdirAll(filepath.Join(projectDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectDir, "metadata.json"), []byte(`{"task_id":"TASK-CANCEL","prompt":"build a small app"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "python-invocations.txt")
	writeExecutable(t, filepath.Join(fakeBin, "python"), `#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = "--version" ]; then
  echo "Python 3.11.0"
  exit 0
fi
printf '%s\n' "${1:-}" >> "$FAKE_PYTHON_MARKER"
sleep 10
exit 0
`)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_PYTHON_MARKER", marker)

	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(root, ".qa-control", "index.db")
	cfg.Codex.PromptProfilesDir = filepath.Join(root, ".qa-control", "prompt_profiles")
	cfg.Pipeline.StageTimeouts["A"] = 20
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scan, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertProjects(context.Background(), scan.Projects); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan struct {
		result pipelinepkg.Result
		err    error
	}, 1)
	go func() {
		result, err := pipelinepkg.NewRunner(store, cfg).Run(ctx, "TASK-CANCEL", pipelinepkg.RunOptions{Stage: "A"})
		resultCh <- struct {
			result pipelinepkg.Result
			err    error
		}{result: result, err: err}
	}()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	for {
		if content, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(content)) != "" {
			cancel()
			break
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("timed out waiting for fake Stage A script to start: %v", waitCtx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}

	var outcome struct {
		result pipelinepkg.Result
		err    error
	}
	select {
	case outcome = <-resultCh:
	case <-time.After(8 * time.Second):
		t.Fatal("pipeline did not return after cancellation")
	}
	if !errors.Is(outcome.err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", outcome.err)
	}
	if outcome.result.Run.Status != model.RunAborted {
		t.Fatalf("result status = %s, want aborted", outcome.result.Run.Status)
	}
	finalRun, err := store.GetRun(context.Background(), outcome.result.Run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if finalRun.Status != model.RunAborted {
		t.Fatalf("stored run status = %s, want aborted", finalRun.Status)
	}
	if _, err := os.Stat(filepath.Join(outcome.result.Run.ArtifactRoot, "abort_summary.json")); err != nil {
		t.Fatalf("abort summary should be written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outcome.result.Run.ArtifactRoot, "crash_summary.json")); !os.IsNotExist(err) {
		t.Fatalf("cancelled run should not be marked crashed, stat err: %v", err)
	}
	content, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Fields(string(content))); got != 1 {
		t.Fatalf("Stage A should stop launching helper scripts after cancellation, got %d invocations:\n%s", got, content)
	}
}

func TestRunCapturesCodexOutputLastMessageWhenStdoutIsEmpty(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "batch-1", "TASK-FILE", "TASK-FILE")
	for _, dir := range []string{"docs", "repo", "original_sessions"} {
		if err := os.MkdirAll(filepath.Join(projectDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectDir, "metadata.json"), []byte(`{"task_id":"TASK-FILE","prompt":"build a small app"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "repo", "self_test_report.md"), []byte("self test passed"), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "codex"), `#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = "--version" ]; then
  echo "codex-cli 0.999.0"
  exit 0
fi
if [ "${1:-}" = "exec" ] && [ "${2:-}" = "--help" ]; then
  echo "--sandbox --ask-for-approval --cd -C --ephemeral --skip-git-repo-check --ignore-user-config --output-last-message"
  exit 0
fi
if [ "${1:-}" = "exec" ]; then
  prompt="$(cat)"
  stage="D"
  if printf '%s' "$prompt" | grep -q 'Run p2r stage F'; then
    stage="F"
  elif printf '%s' "$prompt" | grep -q 'Run p2r stage E'; then
    stage="E"
  fi
  output=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -o|--output-last-message)
        output="${2:-}"
        shift 2
        ;;
      *)
        shift
        ;;
    esac
  done
  if [ -z "$output" ]; then
    echo "missing --output-last-message" >&2
    exit 3
  fi
  cat > "$output" <<JSON
# File Only Report
- High: file-only finding from fake codex

<!-- p2r:static-review-json:start -->
{
  "schema_version": "p2r.static_review.v1",
  "stage": "$stage",
  "findings": [
    {
      "severity": "High",
      "title": "file-only finding from fake codex",
      "rule": "Fake Codex output-last-message rule",
      "evidence": "repo/fake.go:2",
      "impact": "The fake reviewer wrote the report via output-last-message.",
      "minimum_fix": "Keep output-last-message capture working."
    }
  ]
}
<!-- p2r:static-review-json:end -->
JSON
  exit 0
fi
echo "unexpected fake codex args: $*" >&2
exit 2
`)
	writeExecutable(t, filepath.Join(fakeBin, "node"), `#!/usr/bin/env bash
echo "v25.0.0"
`)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(root, ".qa-control", "index.db")
	cfg.Codex.PromptProfilesDir = filepath.Join(root, ".qa-control", "prompt_profiles")
	cfg.Pipeline.StageTimeouts["D"] = 10
	cfg.Pipeline.StageTimeouts["F"] = 10
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scan, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertProjects(context.Background(), scan.Projects); err != nil {
		t.Fatal(err)
	}

	result, err := pipelinepkg.NewRunner(store, cfg).Run(context.Background(), "TASK-FILE", pipelinepkg.RunOptions{Stage: "D"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Run.ArtifactRoot, filepath.Join("result", "batch-1", "TASK-FILE")) {
		t.Fatalf("artifact root should use result/batch/task layout: %s", result.Run.ArtifactRoot)
	}
	manifestContent, err := os.ReadFile(filepath.Join(result.Run.ArtifactRoot, "run_manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(manifestContent, &manifest); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"batch":          "batch-1",
		"artifact_root":  result.Run.ArtifactRoot,
		"started_at_utc": result.Run.StartedAt,
		"timezone":       "Asia/Shanghai",
	} {
		if got, _ := manifest[key].(string); got != want {
			t.Fatalf("manifest[%s] = %q, want %q", key, got, want)
		}
	}
	if local, _ := manifest["started_at_local"].(string); !strings.Contains(local, "+08:00") {
		t.Fatalf("manifest started_at_local should be Shanghai offset: %#v", manifest)
	}
	stageD := stageByName(result.Stages, "D")
	if stageD.Status != model.StageDone {
		t.Fatalf("stage D status = %s, want done; error=%s", stageD.Status, stageD.ErrorSummary)
	}
	reportPath := filepath.Join(result.Run.ArtifactRoot, "tests_coverage_report.md")
	content, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "# File Only Report") {
		t.Fatalf("report did not come from --output-last-message:\n%s", content)
	}
	if strings.Contains(string(content), "Manual Verification Required") {
		t.Fatalf("report fell back to unavailable artifact:\n%s", content)
	}
	logContent, err := os.ReadFile(filepath.Join(result.Run.ArtifactRoot, "logs", "D_tests_coverage_static.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logContent), "--output-last-message") {
		t.Fatalf("log should show output-last-message capture command:\n%s", logContent)
	}
}

func TestRunMarksStaticReviewUnavailableWhenReportSchemaInvalid(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "batch-1", "TASK-SCHEMA", "TASK-SCHEMA")
	for _, dir := range []string{"docs", "repo", "original_sessions"} {
		if err := os.MkdirAll(filepath.Join(projectDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectDir, "metadata.json"), []byte(`{"task_id":"TASK-SCHEMA","prompt":"build a small app"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "repo", "self_test_report.md"), []byte("self test passed"), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "codex"), `#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = "--version" ]; then
  echo "codex-cli 0.999.0"
  exit 0
fi
if [ "${1:-}" = "exec" ] && [ "${2:-}" = "--help" ]; then
  echo "--sandbox --ask-for-approval --cd -C --ephemeral --skip-git-repo-check --ignore-user-config"
  exit 0
fi
if [ "${1:-}" = "exec" ]; then
  cat >/dev/null
  echo "# Legacy Report"
  echo "- High: plain text finding without JSON contract"
  exit 0
fi
exit 2
`)
	writeExecutable(t, filepath.Join(fakeBin, "node"), `#!/usr/bin/env bash
echo "v25.0.0"
`)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(root, ".qa-control", "index.db")
	cfg.Codex.PromptProfilesDir = filepath.Join(root, ".qa-control", "prompt_profiles")
	cfg.Pipeline.StageTimeouts["D"] = 10
	cfg.Pipeline.StageTimeouts["F"] = 10
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scan, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertProjects(context.Background(), scan.Projects); err != nil {
		t.Fatal(err)
	}

	result, err := pipelinepkg.NewRunner(store, cfg).Run(context.Background(), "TASK-SCHEMA", pipelinepkg.RunOptions{Stage: "D"})
	if err != nil {
		t.Fatal(err)
	}
	stageD := stageByName(result.Stages, "D")
	if stageD.Status != model.StageFailed || stageD.ErrorSummary != "static review schema invalid" {
		t.Fatalf("stage D = %#v, want schema failure", stageD)
	}
	reportPath := filepath.Join(result.Run.ArtifactRoot, "tests_coverage_report.md")
	content, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Manual Verification Required") {
		t.Fatalf("schema-invalid report should be replaced with unavailable artifact:\n%s", content)
	}
}

func stageByName(stages []model.StageRecord, name string) model.StageRecord {
	for _, stage := range stages {
		if stage.Stage == name {
			return stage
		}
	}
	return model.StageRecord{}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func waitForLatestRun(t *testing.T, ctx context.Context, store *db.Store, taskID string) model.RunRecord {
	t.Helper()
	for {
		run, err := store.LatestRunForTask(context.Background(), taskID)
		if err == nil {
			return run
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for latest run: %v", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func waitForStageLog(t *testing.T, ctx context.Context, store *db.Store, runID, stage, want string) {
	t.Helper()
	for {
		stages, err := store.Stages(context.Background(), runID)
		if err == nil {
			for _, item := range stages {
				if item.Stage != stage || item.Status != model.StageRunning || item.LogPath == "" {
					continue
				}
				content, _ := os.ReadFile(item.LogPath)
				if strings.Contains(string(content), want) {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for running stage %s log containing %q", stage, want)
		case <-time.After(25 * time.Millisecond):
		}
	}
}
