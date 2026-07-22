package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/authoringharness"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringDockerHarnessBuildsOnceReattestsAndCompletesFullValidation(t *testing.T) {
	root := t.TempDir()
	runner := &standardAuthoringHarnessRunner{t: t, imageID: "sha256:" + strings.Repeat("a", 64)}
	harness := newStandardAuthoringDockerHarnessForTest(t, root, runner)
	runID := standardAuthoringHarnessUUID(t)

	buildRequest, buildTaskRoot := standardAuthoringHarnessAttempt(t, root, runID, workflowkit.StageKey("dockerfile_build_validate"), authoringharness.ModeDockerfileBuild)
	writeStandardAuthoringHarnessCandidate(t, buildTaskRoot, "FROM scratch\n", "#!/bin/sh\nexit 0\n", "#!/bin/sh\nexit 1\n")
	build, err := harness.Validate(context.Background(), buildRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !build.Passed || build.Step != "source_access" || build.ImageReused || len(build.Steps) != 2 {
		t.Fatalf("build result = %+v", build)
	}
	if err := build.ValidateReportJSON(); err != nil {
		t.Fatal(err)
	}

	fullRequest, fullTaskRoot := standardAuthoringHarnessAttempt(t, root, runID, workflowkit.StageKey("authoring_harness"), authoringharness.ModeInitialOracle)
	writeStandardAuthoringHarnessCandidate(t, fullTaskRoot, "FROM scratch\n", "#!/bin/sh\nexit 0\n", "#!/bin/sh\nexit 1\n")
	full, err := harness.Validate(context.Background(), fullRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !full.Passed || full.Step != "oracle_verify" || !full.ImageReused || len(full.Steps) != 3 {
		t.Fatalf("full result = %+v", full)
	}
	if err := full.ValidateReportJSON(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(full.ReportJSON), "sk-abcdefghijklmnop") || !strings.Contains(string(full.ReportJSON), "<redacted-token>") || strings.Contains(string(full.ReportJSON), harness.stateRoot) {
		t.Fatalf("report was not redacted: %s", full.ReportJSON)
	}

	buildCount, runCount, inspectCount := 0, 0, 0
	for _, command := range runner.commands {
		if len(command.Args) == 0 {
			t.Fatalf("empty Docker argv: %+v", command)
		}
		switch command.Args[0] {
		case "build":
			buildCount++
			if !strings.HasPrefix(command.Dir, harness.stateRoot+string(filepath.Separator)) || containsParentArg(command.Args, buildTaskRoot) {
				t.Fatalf("build did not use host snapshot: dir=%q argv=%#v", command.Dir, command.Args)
			}
		case "run":
			runCount++
			for _, required := range []string{"--network", "none", "--read-only", "--cap-drop", "ALL", "no-new-privileges", "/tmp:rw,noexec,nosuid,size=64m", "--entrypoint", "/bin/sh"} {
				if !containsParentArg(command.Args, required) {
					t.Fatalf("verification command missing %q: %#v", required, command.Args)
				}
			}
		case "image":
			inspectCount++
		}
		wantEnv := lockedDockerCommandEnvironment(command.Dir)
		if strings.Join(command.Env, "\x00") != strings.Join(wantEnv, "\x00") {
			t.Fatalf("command environment = %#v, want %#v", command.Env, wantEnv)
		}
	}
	if buildCount != 1 || runCount != 3 || inspectCount != 5 {
		t.Fatalf("Docker command counts build=%d run=%d inspect=%d commands=%#v", buildCount, runCount, inspectCount, runner.commands)
	}
}

func TestStandardAuthoringDockerHarnessRejectsInaccessibleRuntimeSource(t *testing.T) {
	root := t.TempDir()
	runner := &standardAuthoringHarnessRunner{t: t, imageID: "sha256:" + strings.Repeat("c", 64), sourceAccessExit: 1}
	harness := newStandardAuthoringDockerHarnessForTest(t, root, runner)
	runID := standardAuthoringHarnessUUID(t)
	request, taskRoot := standardAuthoringHarnessAttempt(t, root, runID, workflowkit.StageKey("dockerfile_build_validate"), authoringharness.ModeDockerfileBuild)
	writeStandardAuthoringHarnessCandidate(t, taskRoot, "FROM scratch\n", "#!/bin/sh\nexit 0\n", "#!/bin/sh\nexit 1\n")

	result, err := harness.Validate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Passed || result.Step != "source_access" || result.ExitCode != 1 || len(result.Steps) != 2 || len(result.Findings) != 1 || !strings.Contains(result.Findings[0], "writable Oracle worktree") {
		t.Fatalf("source-access feedback = %+v", result)
	}
	foundProbe := false
	for _, command := range runner.commands {
		if len(command.Args) == 0 || command.Args[0] != "run" {
			continue
		}
		program := command.Args[len(command.Args)-1]
		if program != standardAuthoringDockerHarnessSourceAccessProgram {
			continue
		}
		foundProbe = true
		mount := standardAuthoringHarnessArgAfter(command.Args, "--mount")
		if command.Path != "/opt/locked/docker-build" || !strings.Contains(program, "cp -R /workspace/source/. /oracle/worktree/") || !strings.Contains(mount, ",dst=/oracle") || !containsParentArg(command.Args, "--read-only") || !containsParentArg(command.Args, "ALL") {
			t.Fatalf("source-access probe command = %#v", command.Args)
		}
	}
	if !foundProbe {
		t.Fatal("runtime source-access probe was not invoked")
	}
}

func TestStandardAuthoringDockerHarnessReturnsRepairFeedbackThenReusesImageAfterEdit(t *testing.T) {
	root := t.TempDir()
	runner := &standardAuthoringHarnessRunner{t: t, imageID: "sha256:" + strings.Repeat("b", 64)}
	harness := newStandardAuthoringDockerHarnessForTest(t, root, runner)
	runID := standardAuthoringHarnessUUID(t)
	request, taskRoot := standardAuthoringHarnessAttempt(t, root, runID, workflowkit.StageKey("authoring_harness"), authoringharness.ModeInitialOracle)
	writeStandardAuthoringHarnessCandidate(t, taskRoot, "FROM scratch\n", "#!/bin/sh\nexit 0\n", "#!/bin/sh\n# INITIAL_PASS\nexit 0\n")

	failed, err := harness.Validate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Passed || failed.Step != "initial_verify" || failed.ExitCode != 0 || len(failed.Findings) != 1 || !strings.Contains(failed.Findings[0], "passed before") {
		t.Fatalf("initial feedback = %+v", failed)
	}
	before := failed.CandidateDigest
	if err := os.WriteFile(filepath.Join(taskRoot, "tests", "test.sh"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	passed, err := harness.Validate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !passed.Passed || !passed.ImageReused || passed.CandidateDigest == before {
		t.Fatalf("repaired validation = %+v", passed)
	}
	buildCount := 0
	for _, command := range runner.commands {
		if len(command.Args) > 0 && command.Args[0] == "build" {
			buildCount++
		}
	}
	if buildCount != 1 {
		t.Fatalf("Dockerfile-stable repair rebuilt image %d times", buildCount)
	}
}

func TestStandardAuthoringDockerHarnessCheckoutAllowsNonRootWorktreeAndSealsScripts(t *testing.T) {
	candidate, err := authoringharness.CandidateFromBytes(
		authoringharness.ModeInitialOracle,
		[]byte("FROM scratch\n"),
		[]byte("#!/bin/sh\nexit 0\n"),
		[]byte("#!/bin/sh\nexit 1\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	if err := writeStandardAuthoringHarnessSnapshot(snapshot, candidate); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(t.TempDir(), "verification", "oracle")
	if _, err := copyStandardAuthoringHarnessScripts(checkout, snapshot, []string{
		authoringharness.SolveScriptRelativePath,
		authoringharness.TestScriptRelativePath,
	}); err != nil {
		t.Fatal(err)
	}

	rootInfo, err := os.Lstat(checkout)
	if err != nil || rootInfo.Mode().Perm() != 0o777 || rootInfo.Mode()&os.ModeSticky == 0 {
		t.Fatalf("Oracle checkout root mode = %v, %v; want sticky 1777", rootInfo, err)
	}
	for _, relative := range []string{"solution", "tests"} {
		info, statErr := os.Lstat(filepath.Join(checkout, relative))
		if statErr != nil || !info.IsDir() || info.Mode().Perm() != 0o755 {
			t.Fatalf("Oracle script directory %q = %v, %v; want 0755", relative, info, statErr)
		}
	}
	for _, relative := range []string{authoringharness.SolveScriptRelativePath, authoringharness.TestScriptRelativePath} {
		info, statErr := os.Lstat(filepath.Join(checkout, filepath.FromSlash(relative)))
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o444 {
			t.Fatalf("Oracle script %q = %v, %v; want 0444", relative, info, statErr)
		}
	}
}

type standardAuthoringHarnessRunner struct {
	t                *testing.T
	imageID          string
	sourceAccessExit int
	commands         []CodeEdgePhase1Command
}

func (runner *standardAuthoringHarnessRunner) Run(_ context.Context, command CodeEdgePhase1Command) (CodeEdgePhase1CommandResult, error) {
	runner.t.Helper()
	copy := command
	copy.Args = append([]string(nil), command.Args...)
	copy.Env = append([]string(nil), command.Env...)
	runner.commands = append(runner.commands, copy)
	if len(command.Args) == 0 {
		return CodeEdgePhase1CommandResult{}, nil
	}
	switch command.Args[0] {
	case "build":
		return CodeEdgePhase1CommandResult{Stdout: []byte("build completed\n")}, nil
	case "image":
		return CodeEdgePhase1CommandResult{Stdout: []byte(runner.imageID + "\n")}, nil
	case "run":
		mount := standardAuthoringHarnessArgAfter(command.Args, "--mount")
		checkout := strings.TrimPrefix(strings.Split(mount, ",dst=/oracle")[0], "type=bind,src=")
		program := command.Args[len(command.Args)-1]
		if program == standardAuthoringDockerHarnessSourceAccessProgram {
			if runner.sourceAccessExit != 0 {
				return CodeEdgePhase1CommandResult{ExitCode: runner.sourceAccessExit, Stderr: []byte("source is inaccessible\n")}, nil
			}
			return CodeEdgePhase1CommandResult{Stdout: []byte("source copied\n")}, nil
		}
		if program == "sh ./tests/test.sh" {
			testBytes, err := os.ReadFile(filepath.Join(checkout, "tests", "test.sh"))
			if err != nil {
				runner.t.Fatal(err)
			}
			if strings.Contains(string(testBytes), "INITIAL_PASS") {
				return CodeEdgePhase1CommandResult{}, nil
			}
			return CodeEdgePhase1CommandResult{ExitCode: 1, Stderr: []byte("expected initial failure\n")}, nil
		}
		return CodeEdgePhase1CommandResult{Stdout: []byte(command.Dir + " sk-abcdefghijklmnop\n")}, nil
	default:
		return CodeEdgePhase1CommandResult{}, nil
	}
}

func newStandardAuthoringDockerHarnessForTest(t *testing.T, root string, runner CodeEdgePhase1CommandRunner) *StandardAuthoringDockerHarness {
	t.Helper()
	harness, err := NewStandardAuthoringDockerHarness(StandardAuthoringDockerHarnessConfig{
		ManagedRoot: root,
		LockedCommands: []stageprovider.LocalExecutableLock{
			{CommandID: stageprovider.CodeEdgePhase1DockerBuildCommandID, AbsolutePath: "/opt/locked/docker-build", Version: "29.5.2", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("docker-build"))},
			{CommandID: stageprovider.CodeEdgePhase1InitialVerifyCommandID, AbsolutePath: "/opt/locked/docker-initial", Version: "29.5.2", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("docker-initial"))},
			{CommandID: stageprovider.CodeEdgePhase1OracleVerifyCommandID, AbsolutePath: "/opt/locked/docker-oracle", Version: "29.5.2", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("docker-oracle"))},
		},
		Runner: runner, CommandTimeout: time.Minute,
		ExecutableAttestor: func(context.Context, stageprovider.LocalExecutableLock) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return harness
}

func standardAuthoringHarnessAttempt(t *testing.T, root, runID string, stageKey workflowkit.StageKey, mode authoringharness.Mode) (authoringharness.Request, string) {
	t.Helper()
	attemptID := standardAuthoringHarnessUUID(t)
	authoringRoot, err := StandardAuthoringCodexWorkspaceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	workRoot, err := stageprovider.StandardAuthoringAttemptWorkspacePath(authoringRoot, runID, stageKey, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{
		filepath.Join(workRoot, stageprovider.StandardAuthoringCodexAttemptSourceDirectory),
		filepath.Join(workRoot, stageprovider.StandardAuthoringCodexAttemptTaskDirectory),
	} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return authoringharness.Request{Mode: mode, RunID: runID, StageKey: stageKey, StageAttemptID: attemptID}, filepath.Join(workRoot, stageprovider.StandardAuthoringCodexAttemptTaskDirectory)
}

func writeStandardAuthoringHarnessCandidate(t *testing.T, taskRoot, dockerfile, solve, test string) {
	t.Helper()
	for _, directory := range []string{"environment", "solution", "tests"} {
		if err := os.MkdirAll(filepath.Join(taskRoot, directory), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for relative, content := range map[string]string{
		authoringharness.DockerfileRelativePath:  dockerfile,
		authoringharness.SolveScriptRelativePath: solve,
		authoringharness.TestScriptRelativePath:  test,
	} {
		if err := os.WriteFile(filepath.Join(taskRoot, filepath.FromSlash(relative)), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func standardAuthoringHarnessArgAfter(args []string, key string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == key {
			return args[index+1]
		}
	}
	return ""
}

func standardAuthoringHarnessUUID(t *testing.T) string {
	t.Helper()
	id, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
