package pipeline_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/executor"
	pipelinepkg "github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func TestRunAbortPersistenceErrorLeavesCrashEvidence(t *testing.T) {
	root := t.TempDir()
	projectPath := writePipelinePackage(t, root, "batch-1", "TASK-ABORT")
	store := &lifecycleFakeStore{
		project:   scanner.Project{TaskID: "TASK-ABORT", Batch: "batch-1", Path: projectPath},
		finishErr: errors.New("sqlite busy"),
	}
	cfg := config.Default()
	cfg.ScanPath = root

	ctx, cancel := context.WithCancel(context.Background())
	result, err := pipelinepkg.NewRunner(store, cfg, pipelinepkg.WithCommandRunner(lifecycleCommandRunner{})).Run(ctx, "TASK-ABORT", pipelinepkg.RunOptions{
		Stage: "A",
		Progress: func(update pipelinepkg.RunProgress) {
			if update.Event == pipelinepkg.EventRunCreated {
				cancel()
			}
		},
	})
	if err == nil || !strings.Contains(err.Error(), "finish aborted run: sqlite busy") {
		t.Fatalf("abort error = %v, want terminal persistence error", err)
	}
	if result.Run.Status != model.RunAborted {
		t.Fatalf("result run status = %q, want aborted", result.Run.Status)
	}
	if !slices.Contains(store.finishStatuses(), model.RunAborted) || !slices.Contains(store.finishStatuses(), model.RunCrashed) {
		t.Fatalf("finish statuses = %#v, want aborted attempt followed by crash recovery", store.finishStatuses())
	}
	content, err := os.ReadFile(filepath.Join(result.Run.ArtifactRoot, "abort_summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "finish aborted run: sqlite busy") {
		t.Fatalf("abort summary should retain persistence error:\n%s", content)
	}
	if _, err := os.Stat(filepath.Join(result.Run.ArtifactRoot, "crash_summary.json")); err != nil {
		t.Fatalf("crash summary should be written when abort terminal persistence fails: %v", err)
	}
}

func TestRunCrashSummaryRecordsCleanupPersistenceErrors(t *testing.T) {
	root := t.TempDir()
	taskID := "TASK-CRASH"
	projectPath := writePipelinePackage(t, root, "batch-1", taskID)
	store := &lifecycleFakeStore{
		project:        scanner.Project{TaskID: taskID, Batch: "batch-1", Path: projectPath},
		putStageErrFor: "B",
	}
	cfg := config.Default()
	cfg.ScanPath = root

	_, err := pipelinepkg.NewRunner(store, cfg, pipelinepkg.WithCommandRunner(lifecycleCommandRunner{})).Run(context.Background(), taskID, pipelinepkg.RunOptions{
		Stage: "B",
		Progress: func(update pipelinepkg.RunProgress) {
			switch update.Event {
			case pipelinepkg.EventRunCreated:
				artifactRoot := filepath.Join(root, "result", "batch-1", taskID, update.RunID)
				writeRuntimeEvidence(t, artifactRoot)
			case pipelinepkg.EventStagePending:
				if update.Stage == "A" {
					artifactRoot := filepath.Join(root, "result", "batch-1", taskID, update.RunID)
					if err := os.WriteFile(filepath.Join(artifactRoot, "run_manifest.json"), []byte("{bad json"), 0o644); err != nil {
						t.Errorf("corrupt run manifest: %v", err)
					}
				}
			}
		},
	})
	if err == nil || !strings.Contains(err.Error(), "put stage B") {
		t.Fatalf("run error = %v, want stage persistence failure", err)
	}
	if !slices.Contains(store.finishStatuses(), model.RunCrashed) {
		t.Fatalf("finish statuses = %#v, want crashed", store.finishStatuses())
	}
	crashSummary := filepath.Join(root, "result", "batch-1", taskID, store.runID(), "crash_summary.json")
	content, err := os.ReadFile(crashSummary)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if !strings.Contains(text, `"cleanup_status": "ok"`) || !strings.Contains(text, "merge cleanup into run_manifest.json") {
		t.Fatalf("crash summary should record cleanup status and manifest persistence errors:\n%s", text)
	}
}

type lifecycleFakeStore struct {
	mu             sync.Mutex
	project        scanner.Project
	run            model.RunRecord
	putStageErrFor string
	finishErr      error
	statuses       []string
}

func (s *lifecycleFakeStore) GetProject(context.Context, string) (scanner.Project, error) {
	return s.project, nil
}

func (s *lifecycleFakeStore) GetRun(context.Context, string) (model.RunRecord, error) {
	return model.RunRecord{}, nil
}

func (s *lifecycleFakeStore) ListRunsForTask(context.Context, string) ([]model.RunRecord, error) {
	return nil, nil
}

func (s *lifecycleFakeStore) CreateRun(_ context.Context, run model.RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.run = run
	return nil
}

func (s *lifecycleFakeStore) PutStage(_ context.Context, _ string, stage model.StageRecord) error {
	if stage.Stage == s.putStageErrFor {
		return errors.New("put stage " + stage.Stage)
	}
	return nil
}

func (s *lifecycleFakeStore) InsertFindings(context.Context, string, []model.Finding) error {
	return nil
}

func (s *lifecycleFakeStore) FinishRun(_ context.Context, _ string, _ string, status string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses = append(s.statuses, status)
	return s.finishErr
}

func (s *lifecycleFakeStore) finishStatuses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.statuses...)
}

func (s *lifecycleFakeStore) runID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run.RunID
}

type lifecycleCommandRunner struct{}

func (lifecycleCommandRunner) LookPath(name string) (string, error) {
	return name, nil
}

func (lifecycleCommandRunner) Run(_ context.Context, _ time.Duration, _ string, _ []string, name string, args ...string) executor.Result {
	return executor.Result{Command: strings.Join(append([]string{name}, args...), " ")}
}

func (r lifecycleCommandRunner) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, _ io.Writer, _ executor.OutputCallback, name string, args ...string) executor.Result {
	return r.Run(ctx, timeout, dir, env, name, args...)
}

func writeRuntimeEvidence(t *testing.T, artifactRoot string) {
	t.Helper()
	content := `{
  "compose_project": "p2r_test",
  "compose_file": "compose.yaml",
  "work_dir": "/tmp",
  "services": ["web"],
  "mappings": {},
  "probes": []
}`
	if err := os.WriteFile(filepath.Join(artifactRoot, "port_map.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
