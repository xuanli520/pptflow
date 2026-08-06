package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringDockerHarnessValidatesOnlyV3SnapshotFiles(t *testing.T) {
	root := t.TempDir()
	runner := &standardAuthoringHarnessRunner{t: t, imageID: "sha256:" + strings.Repeat("e", 64)}
	harness := newStandardAuthoringDockerHarnessForTest(t, root, runner)
	runID := standardAuthoringHarnessUUID(t)
	attemptID := standardAuthoringHarnessUUID(t)
	authoringRoot, err := StandardAuthoringCodexWorkspaceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(authoringRoot, runID, stageprovider.StandardAuthoringCodexRunSourceDirectory), 0o750); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		standardAuthoringV3InstructionPath:   []byte("# task\n"),
		standardAuthoringV3TaskTOMLPath:      []byte("title = 'task'\n"),
		standardAuthoringV3DockerfilePath:    []byte("FROM scratch\n"),
		standardAuthoringV3SolveScriptPath:   []byte("#!/bin/sh\nexit 0\n"),
		standardAuthoringV3TestScriptPath:    []byte("#!/bin/sh\nexit 1\n"),
		standardAuthoringV3TestsAnalysisPath: []byte("{}\n"),
	}
	manifest := make([]workflowkit.CandidateFile, 0, len(files))
	for path, content := range files {
		manifest = append(manifest, workflowkit.CandidateFile{Path: path, SchemaVersion: "harbor.artifact.v1", ContentDigest: workflowkit.SHA256Fingerprint(content), SizeBytes: int64(len(content))})
	}
	snapshot, err := workflowkit.NewCandidateSnapshot(manifest)
	if err != nil {
		t.Fatal(err)
	}
	verification := standardAuthoringV3TestVerificationContract(t)
	result, err := harness.ValidateV3Candidate(context.Background(), runID, attemptID, snapshot, files, verification)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Passed || result.Step != "integrity_verify" {
		t.Fatalf("v3 validation result = %+v", result)
	}
	wantSteps := []string{"layout_probe", "environment_build", "source_access", "baseline_verify", "oracle_verify", "coverage_verify", "integrity_verify"}
	if len(result.Steps) != len(wantSteps) {
		t.Fatalf("v3 validation steps = %+v", result.Steps)
	}
	for index, want := range wantSteps {
		if result.Steps[index].Step != want {
			t.Fatalf("v3 validation step %d = %q, want %q", index, result.Steps[index].Step, want)
		}
	}
	sourceAccessProgram := ""
	initialProgram := ""
	oracleProgram := ""
	for _, command := range runner.commands {
		if len(command.Args) == 0 || command.Args[0] != "run" {
			continue
		}
		program := command.Args[len(command.Args)-1]
		switch {
		case program == standardAuthoringDockerHarnessSourceAccessProgram:
			sourceAccessProgram = program
		case strings.Contains(program, "/task/solution/solve.sh"):
			oracleProgram = program
		case strings.Contains(program, "/task/tests/test.sh"):
			initialProgram = program
		}
	}
	if !strings.Contains(sourceAccessProgram, "chmod -R u+rwX /work") ||
		!strings.Contains(sourceAccessProgram, "test -w \"$probe\"") ||
		!strings.Contains(initialProgram, "chmod -R u+rwX /work") ||
		!strings.Contains(oracleProgram, "chmod -R u+rwX /work") {
		t.Fatalf("v3 verifier programs do not restore copied source writability: source=%q initial=%q oracle=%q", sourceAccessProgram, initialProgram, oracleProgram)
	}
	if err := result.ValidateReportJSON(); err != nil {
		t.Fatal(err)
	}

	tampered := snapshot.Clone()
	tampered.Files[0].ContentDigest = workflowkit.SHA256Fingerprint([]byte("substituted"))
	if _, err := harness.ValidateV3Candidate(context.Background(), runID, attemptID, tampered, files, verification); err == nil {
		t.Fatal("v3 candidate validator accepted a tampered snapshot")
	}
}

func standardAuthoringV3TestVerificationContract(t *testing.T) StandardAuthoringVerificationContract {
	t.Helper()
	contract, err := ParseStandardAuthoringVerificationContractJSON([]byte(`{"format":"harbor.verification-contract.v1","version":"1","command":["sh","/task/tests/test.sh"],"workdir":".","coverage_mode":"integration","allowed_solution_paths":["src"]}`))
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

type standardAuthoringHarnessRunner struct {
	t                *testing.T
	imageID          string
	sourceAccessExit int
	commands         []lockedDockerCommand
}

func (runner *standardAuthoringHarnessRunner) Run(_ context.Context, command lockedDockerCommand) (lockedDockerCommandResult, error) {
	runner.t.Helper()
	copy := command
	copy.Args = append([]string(nil), command.Args...)
	copy.Env = append([]string(nil), command.Env...)
	runner.commands = append(runner.commands, copy)
	if len(command.Args) == 0 {
		return lockedDockerCommandResult{}, nil
	}
	switch command.Args[0] {
	case "build":
		return lockedDockerCommandResult{Stdout: []byte("build completed\n")}, nil
	case "image":
		return lockedDockerCommandResult{Stdout: []byte(runner.imageID + "\n")}, nil
	case "run":
		taskRoot := standardAuthoringHarnessMountSource(command.Args, "/task")
		workRoot := standardAuthoringHarnessMountSource(command.Args, "/work")
		program := command.Args[len(command.Args)-1]
		if program == standardAuthoringDockerHarnessSourceAccessProgram {
			if runner.sourceAccessExit != 0 {
				return lockedDockerCommandResult{ExitCode: runner.sourceAccessExit, Stderr: []byte("source is inaccessible\n")}, nil
			}
			return lockedDockerCommandResult{Stdout: []byte("source copied\n")}, nil
		}
		if strings.Contains(program, "/work") && workRoot != "" {
			if err := os.MkdirAll(workRoot, 0o755); err != nil {
				runner.t.Fatal(err)
			}
		}
		if strings.Contains(program, "tests/test.sh") {
			if strings.Contains(program, "solution/solve.sh") {
				return lockedDockerCommandResult{Stdout: []byte(command.Dir + " sk-abcdefghijklmnop\n")}, nil
			}
			testBytes, err := os.ReadFile(filepath.Join(taskRoot, "tests", "test.sh"))
			if err != nil {
				runner.t.Fatal(err)
			}
			if strings.Contains(string(testBytes), "INITIAL_PASS") {
				return lockedDockerCommandResult{}, nil
			}
			return lockedDockerCommandResult{ExitCode: 1, Stderr: []byte("expected initial failure\n")}, nil
		}
		return lockedDockerCommandResult{Stdout: []byte(command.Dir + " sk-abcdefghijklmnop\n")}, nil
	default:
		return lockedDockerCommandResult{}, nil
	}
}

func standardAuthoringHarnessMountSource(args []string, destination string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] != "--mount" {
			continue
		}
		mount := args[index+1]
		if !strings.Contains(mount, ",dst="+destination) {
			continue
		}
		return strings.TrimPrefix(strings.Split(mount, ",dst="+destination)[0], "type=bind,src=")
	}
	return ""
}

func newStandardAuthoringDockerHarnessForTest(t *testing.T, root string, runner lockedDockerCommandRunner) *StandardAuthoringDockerHarness {
	t.Helper()
	harness, err := NewStandardAuthoringDockerHarness(StandardAuthoringDockerHarnessConfig{
		ManagedRoot: root,
		LockedCommands: []stageprovider.LocalExecutableLock{
			{CommandID: stageprovider.StandardAuthoringDockerBuildCommandID, AbsolutePath: "/opt/locked/docker-build", Version: "29.5.2", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("docker-build"))},
			{CommandID: stageprovider.StandardAuthoringInitialVerifyCommandID, AbsolutePath: "/opt/locked/docker-initial", Version: "29.5.2", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("docker-initial"))},
			{CommandID: stageprovider.StandardAuthoringOracleVerifyCommandID, AbsolutePath: "/opt/locked/docker-oracle", Version: "29.5.2", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("docker-oracle"))},
		},
		Runner: runner, CommandTimeout: time.Minute,
		ExecutableAttestor: func(context.Context, stageprovider.LocalExecutableLock) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return harness
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
