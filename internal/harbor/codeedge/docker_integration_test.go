package codeedge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPhase1DockerIsolationAndOracleFixture exercises the confirmed local
// Docker integration boundary. The evaluator container receives no task
// checkout at all, while the oracle receives a distinct throwaway checkout.
// Harbor, model providers, and publication remain outside this fixture.
func TestPhase1DockerIsolationAndOracleFixture(t *testing.T) {
	docker := requireLocalDocker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	root := writePhase1Task(t, phase1TaskOptions{
		taskTOML: `
schema_version = "1.3"

[task]
name = "codeedge/docker-integration"

[metadata]
code_lang = "sh"
task_type = "bug-fix"
application = "integration"
is_0_to_1 = true
github_url = ""
commit_id = ""
`,
		dockerfile: "FROM alpine:3.22\nWORKDIR /evaluator\n",
	})
	writeTaskFile(t, root, "solution/solve.sh", "#!/bin/sh\nprintf 'fixed\\n' > result.txt\n")
	writeTaskFile(t, root, "tests/test.sh", "#!/bin/sh\ntest \"$(cat result.txt)\" = fixed\n")
	if err := validatePhase1Task(root); err != nil {
		t.Fatalf("preflight Docker integration fixture: %v", err)
	}

	tag := "harbor-flow-codeedge-fixture:" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() { _ = runDocker(context.Background(), docker, "image", "rm", "--force", tag) })
	if err := runDocker(ctx, docker, "build", "--pull=false", "--tag", tag, "--file", filepath.Join(root, "environment", "Dockerfile"), filepath.Join(root, "environment")); err != nil {
		t.Fatalf("build isolated CodeEdge environment: %v", err)
	}
	if err := runDocker(ctx, docker, "run", "--rm", "--network", "none", "--read-only", tag, "sh", "-ec", "test ! -e /evaluator/tests && test ! -e /evaluator/solution && test ! -e /evaluator/reward"); err != nil {
		t.Fatalf("evaluator image leaked solution/test/reward material: %v", err)
	}

	oracle := t.TempDir()
	copyDockerFixtureFile(t, root, oracle, "solution/solve.sh")
	copyDockerFixtureFile(t, root, oracle, "tests/test.sh")
	if err := runDocker(ctx, docker, "run", "--rm", "--network", "none", "--mount", "type=bind,src="+oracle+",dst=/oracle", "--workdir", "/oracle", tag, "sh", "-ec", "sh ./solution/solve.sh && sh ./tests/test.sh"); err != nil {
		t.Fatalf("isolated Oracle solution/test execution: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "result.txt")); !os.IsNotExist(err) {
		t.Fatalf("Oracle execution modified the managed snapshot: stat result.txt = %v", err)
	}
}

func requireLocalDocker(t *testing.T) string {
	t.Helper()
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("local Docker integration fixture requires docker")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := runDocker(ctx, docker, "version", "--format", "{{.Server.Version}}"); err != nil {
		t.Skipf("local Docker integration fixture requires a reachable daemon: %v", err)
	}
	return docker
}

func runDocker(ctx context.Context, docker string, arguments ...string) error {
	command := exec.CommandContext(ctx, docker, arguments...)
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(string(output))
	if message == "" {
		message = err.Error()
	}
	return fmt.Errorf("docker %s: %s", strings.Join(arguments, " "), message)
}

func copyDockerFixtureFile(t *testing.T, sourceRoot, destinationRoot, relativePath string) {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(sourceRoot, filepath.FromSlash(relativePath)))
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(destinationRoot, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, contents, 0o700); err != nil {
		t.Fatal(err)
	}
}
