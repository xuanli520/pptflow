package tui_test

import (
	"context"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	tuiapp "github.com/xuanli520/p2r_tui/internal/tui"
)

func TestDockerHealthPollerMarksMissingComposeProjectStopped(t *testing.T) {
	store, cfg := tuiDockerHealthStore(t)
	ctx := context.Background()
	lost, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-AAAAAA", "https://gitlab.example/TASK-20260521-AAAAAA", cfg.ScanPath)
	if err != nil {
		t.Fatal(err)
	}
	running, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-BBBBBB", "https://gitlab.example/TASK-20260521-BBBBBB", cfg.ScanPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTaskRuntime(ctx, lost.ID, "", true, model.ComposeMeta{Project: "lost", ComposeFiles: []string{"compose.yml"}, WorkDir: lost.RepoPath}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTaskRuntime(ctx, running.ID, "", true, model.ComposeMeta{Project: "running", ComposeFiles: []string{"compose.yml"}, WorkDir: running.RepoPath}); err != nil {
		t.Fatal(err)
	}

	checked, stopped, err := tuiapp.RefreshDockerHealthForTest(ctx, store, cfg, dockerHealthExec{
		stdoutByProject: map[string]string{
			"lost":    "",
			"running": "container-id\n",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked != 2 || !slices.Contains(stopped, lost.ID) || slices.Contains(stopped, running.ID) {
		t.Fatalf("health result checked=%d stopped=%#v", checked, stopped)
	}
	lostTask, err := store.GetTask(ctx, lost.ID)
	if err != nil {
		t.Fatal(err)
	}
	runningTask, err := store.GetTask(ctx, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lostTask.DockerRunning || !runningTask.DockerRunning {
		t.Fatalf("docker flags lost=%v running=%v", lostTask.DockerRunning, runningTask.DockerRunning)
	}
}

func tuiDockerHealthStore(t *testing.T) (*db.Store, config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.ScanPath = t.TempDir()
	path := filepath.Join(t.TempDir(), "index.db")
	store, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, cfg
}

type dockerHealthExec struct {
	stdoutByProject map[string]string
}

func (dockerHealthExec) LookPath(name string) (string, error) {
	return name, nil
}

func (e dockerHealthExec) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
	project := ""
	for index, arg := range args {
		if arg == "-p" && index+1 < len(args) {
			project = args[index+1]
			break
		}
	}
	return executor.Result{
		Command: strings.Join(append([]string{name}, args...), " "),
		Stdout:  e.stdoutByProject[project],
	}
}

func (e dockerHealthExec) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput executor.OutputCallback, name string, args ...string) executor.Result {
	return e.Run(ctx, timeout, dir, env, name, args...)
}
