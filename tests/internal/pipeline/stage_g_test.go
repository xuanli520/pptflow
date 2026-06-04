package pipeline_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/executor"
	pipelinepkg "github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func TestBrowserURLCandidatesUseLocalhostAllowlist(t *testing.T) {
	candidates := pipelinepkg.BrowserURLCandidatesForTest(pipelinepkg.TestRuntimeEvidence{
		Services: []string{"web", "api"},
		Mappings: map[string][]pipelinepkg.TestPortMapping{
			"web": {
				{Service: "web", URL: "0.0.0.0", Host: 38080, Container: 3000, Protocol: "tcp"},
				{Service: "web", URL: "localhost", Host: 0, Container: 9999, Protocol: "tcp"},
			},
			"api": {
				{Service: "api", URL: "[::]", Host: 39090, Container: 8080, Protocol: "tcp"},
			},
		},
		Probes: []pipelinepkg.TestProbeResult{
			{Service: "web", URL: "http://127.0.0.1:38080", OK: true, Status: 200},
			{Service: "api", URL: "http://localhost:39090", OK: false, Status: 500, Error: "server error"},
		},
	})
	if len(candidates) != 2 {
		t.Fatalf("candidates = %#v", candidates)
	}
	if candidates[0].URL != "http://127.0.0.1:38080" || candidates[1].URL != "http://127.0.0.1:39090" {
		t.Fatalf("URLs should be normalized to 127.0.0.1: %#v", candidates)
	}
	origins := pipelinepkg.BrowserAllowlistOriginsForTest(candidates)
	if strings.Join(origins, ",") != "http://127.0.0.1:38080,http://127.0.0.1:39090" {
		t.Fatalf("origins = %#v", origins)
	}
}

func TestStageBBlocksFrontendAndRuntimeDependents(t *testing.T) {
	got := pipelinepkg.BlockedDependentsForTest("B")
	want := []string{"G", "C"}
	if !slices.Equal(got, want) {
		t.Fatalf("blocked dependents = %#v, want %#v", got, want)
	}
}

func TestRunBlocksGAndCWhenStageBPreflightBlocked(t *testing.T) {
	root := t.TempDir()
	taskID := "TASK-20260604-B10C0D"
	projectPath := writePipelinePackage(t, root, "batch-1", taskID)
	cfg := config.Default()
	cfg.ScanPath = root
	ctx := context.Background()
	store := &runtimeBlockStore{project: scanner.Project{TaskID: taskID, Batch: "batch-1", Path: projectPath}}
	result, err := pipelinepkg.NewRunner(store, cfg, pipelinepkg.WithCommandRunner(runtimeBlockedRunner{})).Run(ctx, taskID, pipelinepkg.RunOptions{From: "B"})
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"B", "G", "C"} {
		record := stageByName(result.Stages, stage)
		if record.Status != model.StageBlocked {
			t.Fatalf("stage %s = %#v, want blocked", stage, record)
		}
	}
	for _, name := range []string{"frontend_e2e_summary.json", "frontend_e2e_report.md", "test_runtime_summary.json"} {
		if _, err := os.Stat(filepath.Join(result.Run.ArtifactRoot, name)); err != nil {
			t.Fatalf("expected blocked artifact %s: %v", name, err)
		}
	}
}

func TestBrowserActionValidatorRejectsUnsafeActions(t *testing.T) {
	candidates := []pipelinepkg.TestBrowserURLCandidate{{ID: "url_1", URL: "http://127.0.0.1:3000", Origin: "http://127.0.0.1:3000"}}
	cases := []pipelinepkg.TestBrowserAction{
		{Action: "shell", Reason: "run command"},
		{Action: "open_candidate", URL: "https://example.com", URLID: "url_1", Reason: "external"},
		{Action: "snapshot", OutputPath: "/tmp/out.png", Reason: "write"},
		{Action: "delete_storage", Reason: "clear state"},
	}
	for _, tc := range cases {
		if blocked := pipelinepkg.ValidateBrowserActionForTest(tc, candidates); blocked == nil {
			t.Fatalf("expected action %#v to be blocked", tc)
		}
	}
	if blocked := pipelinepkg.ValidateBrowserActionForTest(pipelinepkg.TestBrowserAction{Action: "open_candidate", URLID: "url_1", Reason: "open app"}, candidates); blocked != nil {
		t.Fatalf("valid action blocked: %#v", blocked)
	}
}

type runtimeBlockedRunner struct{}

func (runtimeBlockedRunner) LookPath(name string) (string, error) {
	if name == "docker" {
		return "", errors.New("docker missing")
	}
	return name, nil
}

func (runtimeBlockedRunner) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
	return executor.Result{Command: strings.Join(append([]string{name}, args...), " "), Stdout: name + " version\n"}
}

func (r runtimeBlockedRunner) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput executor.OutputCallback, name string, args ...string) executor.Result {
	return r.Run(ctx, timeout, dir, env, name, args...)
}

type runtimeBlockStore struct {
	project scanner.Project
}

func (s *runtimeBlockStore) GetProject(context.Context, string) (scanner.Project, error) {
	return s.project, nil
}

func (s *runtimeBlockStore) GetRun(context.Context, string) (model.RunRecord, error) {
	return model.RunRecord{}, nil
}

func (s *runtimeBlockStore) ListRunsForTask(context.Context, string) ([]model.RunRecord, error) {
	return nil, nil
}

func (s *runtimeBlockStore) CreateRun(context.Context, model.RunRecord) error {
	return nil
}

func (s *runtimeBlockStore) PutStage(context.Context, string, model.StageRecord) error {
	return nil
}

func (s *runtimeBlockStore) PutStageAndRecordTaskRuntime(context.Context, string, model.StageRecord, string, string, bool, model.ComposeMeta) error {
	return nil
}

func (s *runtimeBlockStore) InsertFindings(context.Context, string, []model.Finding) error {
	return nil
}

func (s *runtimeBlockStore) FinishRun(context.Context, string, string, string, time.Duration) error {
	return nil
}

func TestFrontendE2ESummarySchemaValidation(t *testing.T) {
	valid := []byte(`{"schema_version":"p2r.frontend_e2e.v1","status":"failed","findings":[{"severity":"High","title":"blank page"}]}`)
	if _, err := pipelinepkg.ParseFrontendE2ESummaryForTest(valid); err != nil {
		t.Fatal(err)
	}
	invalid := []byte(`{"schema_version":"p2r.frontend_e2e.v1","status":"failed","findings":[{"severity":"Critical","title":"bad"}]}`)
	if _, err := pipelinepkg.ParseFrontendE2ESummaryForTest(invalid); err == nil {
		t.Fatal("expected invalid severity to fail")
	}
}

func TestRepoSnapshotDetectsSourceChangesAndIgnoresCaches(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "app.js"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := pipelinepkg.SnapshotRepoForTest(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".pytest_cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".pytest_cache", "README.md"), []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "app.js"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := pipelinepkg.SnapshotRepoForTest(repo)
	if err != nil {
		t.Fatal(err)
	}
	diff := pipelinepkg.RepoSnapshotDiffForTest(before, after)
	if len(diff) != 1 || diff[0] != "src/app.js" {
		t.Fatalf("diff = %#v", diff)
	}
}
