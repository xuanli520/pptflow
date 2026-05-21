package docker_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/executor"
)

func TestComposeProjectNamePreservesHashWhenTruncated(t *testing.T) {
	name := dockermgr.ComposeProjectName("p2rqa", strings.Repeat("TASK-LONG-", 10), "run-20260430-123456-999999")
	if len(name) > 63 {
		t.Fatalf("compose project name too long: %d %s", len(name), name)
	}
	parts := strings.Split(name, "_")
	if len(parts[len(parts)-1]) != 8 {
		t.Fatalf("hash suffix not preserved: %s", name)
	}
}

func TestCleanupComposeArgsRespectConfiguredDestructiveOptions(t *testing.T) {
	cfg := config.Default().Docker
	cfg.CleanupImages = false
	cfg.CleanupVolumes = false

	args := dockermgr.CleanupComposeArgs(cfg, "compose file.yml", "p2r_test")
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"-v", "--rmi"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("cleanup args should respect disabled %s option: %#v", forbidden, args)
		}
	}
	if !containsArg(args, "--remove-orphans") {
		t.Fatalf("cleanup args should still remove orphans: %#v", args)
	}
}

func TestCommandLineShellQuotesUnsafeArguments(t *testing.T) {
	line := dockermgr.CommandLine("docker", []string{"compose", "-f", "compose file.yml", "-p", "p2r;rm", "down"})
	for _, want := range []string{"'compose file.yml'", "'p2r;rm'"} {
		if !strings.Contains(line, want) {
			t.Fatalf("command line missing quoted arg %q: %s", want, line)
		}
	}
}

func TestIsRunningUsesComposePSWithFilesAndProjectDir(t *testing.T) {
	runner := &managerRunner{run: func(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
		if dir != "/repo" {
			t.Fatalf("command dir = %q, want /repo", dir)
		}
		got := strings.Join(args, " ")
		for _, want := range []string{"compose", "--project-directory /repo", "-f compose.yml", "-p p2r_task", "ps -q"} {
			if !strings.Contains(got, want) {
				t.Fatalf("args missing %q: %#v", want, args)
			}
		}
		return executor.Result{Stdout: "container-id\n"}
	}}

	running, err := dockermgr.IsRunning(context.Background(), runner, []string{"compose.yml"}, "p2r_task", "/repo")
	if err != nil || !running {
		t.Fatalf("IsRunning = %v, %v; want true, nil", running, err)
	}
}

func TestGetFrontendURLParsesComposePS(t *testing.T) {
	runner := &managerRunner{run: func(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
		return executor.Result{Stdout: `{"Service":"web","Publishers":[{"URL":"0.0.0.0","TargetPort":3000,"PublishedPort":34152,"Protocol":"tcp"}]}`}
	}}

	url, err := dockermgr.GetFrontendURL(context.Background(), runner, []string{"compose.yml"}, "p2r_task", "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if url != "http://localhost:34152" {
		t.Fatalf("frontend URL = %q", url)
	}
}

func TestListAllProjectsGroupsManagedComposeContainers(t *testing.T) {
	cfg := config.Default().Docker
	runner := &managerRunner{run: func(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, "label="+cfg.ManagedLabel):
			return executor.Result{Stdout: `{"ID":"c2","Labels":{"managed_by":"p2rqa","com.docker.compose.project":"p2rqa_task","com.docker.compose.project.working_dir":"/repo","com.docker.compose.project.config_files":"/repo/compose.yml,/tmp/p2r-labels.yml"}}
{"ID":"c1","Labels":{"managed_by":"p2rqa","com.docker.compose.project":"p2rqa_task","com.docker.compose.project.working_dir":"/repo","com.docker.compose.project.config_files":"/repo/compose.yml,/tmp/p2r-labels.yml"}}`}
		case strings.Contains(command, "label=com.docker.compose.project"):
			return executor.Result{Stdout: `{"ID":"legacy","Labels":{"com.docker.compose.project":"p2rqa_legacy","com.docker.compose.project.working_dir":"/legacy"}}`}
		default:
			t.Fatalf("unexpected docker args: %#v", args)
			return executor.Result{}
		}
	}}

	projects, err := dockermgr.ListAllProjects(context.Background(), runner, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 {
		t.Fatalf("projects = %#v", projects)
	}
	task := projects[1]
	if task.Name != "p2rqa_task" || task.WorkDir != "/repo" || strings.Join(task.ComposeFiles, ",") != "/repo/compose.yml,/tmp/p2r-labels.yml" {
		t.Fatalf("managed project = %#v", task)
	}
	if strings.Join(task.ContainerIDs, ",") != "c1,c2" {
		t.Fatalf("container IDs should be sorted and grouped: %#v", task.ContainerIDs)
	}
	if projects[0].Name != "p2rqa_legacy" {
		t.Fatalf("prefix-owned project missing or unsorted: %#v", projects)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

type managerRunner struct {
	run func(context.Context, time.Duration, string, []string, string, ...string) executor.Result
}

func (r *managerRunner) LookPath(name string) (string, error) {
	return name, nil
}

func (r *managerRunner) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
	if r.run != nil {
		return r.run(ctx, timeout, dir, env, name, args...)
	}
	return executor.Result{}
}

func (r *managerRunner) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput executor.OutputCallback, name string, args ...string) executor.Result {
	return r.Run(ctx, timeout, dir, env, name, args...)
}
