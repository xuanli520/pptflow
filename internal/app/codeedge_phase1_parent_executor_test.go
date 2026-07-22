package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestCodeEdgePhase1ParentBuiltinReturnsStructuredRepairFinding(t *testing.T) {
	root, snapshot, digest := codeEdgePhase1ParentTaskSnapshot(t, true)
	executor := newCodeEdgePhase1ParentExecutorForTest(t, root, &codeEdgePhase1RecordingRunner{})
	request, invocation := codeEdgePhase1ParentBuiltinInvocation(t, snapshot, digest, workflowadapter.RepoPrepare, "task_layout_report", "harbor.artifact.v1", stageprovider.CodeEdgePhase1TaskLayoutPreflightHandlerID)

	result, err := executor.ExecuteHarborBuiltin(context.Background(), invocation, workflowadapter.HarborBuiltinOperationPayload{HandlerID: stageprovider.CodeEdgePhase1TaskLayoutPreflightHandlerID})
	if err != nil {
		t.Fatalf("ExecuteHarborBuiltin() error = %v", err)
	}
	if result.Outcome != (workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictNeedsRepair}) {
		t.Fatalf("outcome = %+v, want completed needs_repair", result.Outcome)
	}
	report := codeEdgePhase1DecodeReport(t, result, "task_layout_report", "harbor.artifact.v1")
	if report.Stage != workflowadapter.RepoPrepare || len(report.Findings) == 0 {
		t.Fatalf("repair report = %+v, want repo_prepare findings", report)
	}
	if !strings.Contains(strings.Join(report.Findings, "\n"), "metadata.code_lang") {
		t.Fatalf("repair findings = %#v, want metadata validation", report.Findings)
	}
	if report.Command != nil || report.ReservedArtifactID != "" {
		t.Fatalf("builtin repair report unexpectedly has command/reserved artifact: %+v", report)
	}
	_ = request
}

func TestCodeEdgePhase1ParentSubmissionReservesStableUUIDv7Artifact(t *testing.T) {
	root, snapshot, digest := codeEdgePhase1ParentTaskSnapshot(t, false)
	executor := newCodeEdgePhase1ParentExecutorForTest(t, root, &codeEdgePhase1RecordingRunner{})
	_, invocation := codeEdgePhase1ParentBuiltinInvocation(t, snapshot, digest, workflowadapter.SubmissionLint, "submission_lint_report", workflowadapter.CodeEdgeSubmissionReportSchemaVersion, stageprovider.CodeEdgePhase1SubmissionLintHandlerID)
	payload := workflowadapter.HarborBuiltinOperationPayload{HandlerID: stageprovider.CodeEdgePhase1SubmissionLintHandlerID}

	first, err := executor.ExecuteHarborBuiltin(context.Background(), invocation, payload)
	if err != nil {
		t.Fatalf("first ExecuteHarborBuiltin() error = %v", err)
	}
	second, err := executor.ExecuteHarborBuiltin(context.Background(), invocation, payload)
	if err != nil {
		t.Fatalf("replayed ExecuteHarborBuiltin() error = %v", err)
	}
	firstReport := codeEdgePhase1DecodeReport(t, first, "submission_lint_report", workflowadapter.CodeEdgeSubmissionReportSchemaVersion)
	secondReport := codeEdgePhase1DecodeReport(t, second, "submission_lint_report", workflowadapter.CodeEdgeSubmissionReportSchemaVersion)
	if first.Artifacts[0].ID == "" || first.Artifacts[0].ID != second.Artifacts[0].ID || firstReport.ReservedArtifactID != string(first.Artifacts[0].ID) || secondReport.ReservedArtifactID != string(second.Artifacts[0].ID) {
		t.Fatalf("reserved submission artifact IDs differ: first=%+v second=%+v", first, second)
	}
	if err := store.ValidateUUIDv7(string(first.Artifacts[0].ID)); err != nil {
		t.Fatalf("reserved artifact ID %q is not UUIDv7: %v", first.Artifacts[0].ID, err)
	}
	if first.Outcome.Verdict != workflowkit.VerdictPass || first.Artifacts[0].SchemaVersion != workflowadapter.CodeEdgeSubmissionReportSchemaVersion {
		t.Fatalf("submission result = %+v", first)
	}
}

func TestCodeEdgePhase1ParentDockerBuildUsesOnlyLockedDirectCommand(t *testing.T) {
	root, snapshot, digest := codeEdgePhase1ParentTaskSnapshot(t, false)
	runner := &codeEdgePhase1RecordingRunner{results: []CodeEdgePhase1CommandResult{
		{Stdout: []byte("build output is not an image identity contract\n")},
		{Stdout: []byte("sha256:" + strings.Repeat("a", 64) + "\n")},
	}}
	executor := newCodeEdgePhase1ParentExecutorForTest(t, root, runner)
	request, invocation := codeEdgePhase1ParentLocalInvocation(t, snapshot, digest, workflowadapter.DockerBuild, "docker_build_report", "harbor.artifact.v1", stageprovider.CodeEdgePhase1DockerBuildCommandID)

	result, err := executor.ExecuteLocalCommand(context.Background(), invocation, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.CodeEdgePhase1DockerBuildCommandID, Arguments: []string{}})
	if err != nil {
		t.Fatalf("ExecuteLocalCommand() error = %v", err)
	}
	if result.Outcome != (workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass}) {
		t.Fatalf("Docker build outcome = %+v", result.Outcome)
	}
	report := codeEdgePhase1DecodeReport(t, result, "docker_build_report", "harbor.artifact.v1")
	if report.Command == nil || report.Command.CommandID != stageprovider.CodeEdgePhase1DockerBuildCommandID {
		t.Fatalf("Docker build report command = %+v", report.Command)
	}
	if len(runner.commands) != 2 {
		t.Fatalf("commands = %d, want build + image inspection", len(runner.commands))
	}
	command := runner.commands[0]
	if command.Path != "/opt/locked/docker-build" || len(command.Env) != 4 || !containsParentArg(command.Env, "PATH=/nonexistent") || !containsParentArg(command.Args, "--pull=false") || !containsParentArg(command.Args, "--network=default") {
		t.Fatalf("locked Docker command = %+v", command)
	}
	if containsParentArg(command.Args, "solution/solve.sh") || containsParentArg(command.Args, "tests/test.sh") || !strings.HasSuffix(command.Args[len(command.Args)-1], "/task/environment") {
		t.Fatalf("Docker build command leaked task scripts or used an unsafe context: %#v", command.Args)
	}
	inspect := runner.commands[1]
	if inspect.Path != "/opt/locked/docker-build" || !containsParentArg(inspect.Args, "inspect") || !containsParentArg(inspect.Args, "{{.Id}}") || !containsParentArg(inspect.Env, "PATH=/nonexistent") {
		t.Fatalf("Docker image identity inspection = %+v", inspect)
	}
	if request.Execution.ID == "" {
		t.Fatal("fixture request was unexpectedly empty")
	}
}

func TestCodeEdgePhase1ParentInitialAndOracleUseSeparateControlledMounts(t *testing.T) {
	root, snapshot, digest := codeEdgePhase1ParentTaskSnapshot(t, false)
	runner := &codeEdgePhase1RecordingRunner{results: []CodeEdgePhase1CommandResult{
		{Stdout: []byte("modern BuildKit output\n")},
		{Stdout: []byte("sha256:" + strings.Repeat("b", 64) + "\n")},
		{Stdout: []byte("sha256:" + strings.Repeat("b", 64) + "\n")},
		{ExitCode: 1},
		{Stdout: []byte("sha256:" + strings.Repeat("b", 64) + "\n")},
		{ExitCode: 0},
	}}
	executor := newCodeEdgePhase1ParentExecutorForTest(t, root, runner)
	_, buildInvocation := codeEdgePhase1ParentLocalInvocation(t, snapshot, digest, workflowadapter.DockerBuild, "docker_build_report", "harbor.artifact.v1", stageprovider.CodeEdgePhase1DockerBuildCommandID)
	if _, err := executor.ExecuteLocalCommand(context.Background(), buildInvocation, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.CodeEdgePhase1DockerBuildCommandID, Arguments: []string{}}); err != nil {
		t.Fatal(err)
	}
	_, initialInvocation := codeEdgePhase1ParentLocalInvocationForRun(t, snapshot, digest, buildInvocation.Request.Execution.ID, workflowadapter.InitialVerify, "initial_verify_report", "harbor.artifact.v1", stageprovider.CodeEdgePhase1InitialVerifyCommandID)
	initial, err := executor.ExecuteLocalCommand(context.Background(), initialInvocation, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.CodeEdgePhase1InitialVerifyCommandID, Arguments: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if initial.Outcome.Verdict != workflowkit.VerdictPass {
		t.Fatalf("initial verifier non-zero exit should prove the initial defect: %+v", initial.Outcome)
	}
	_, oracleInvocation := codeEdgePhase1ParentLocalInvocationForRun(t, snapshot, digest, buildInvocation.Request.Execution.ID, workflowadapter.OracleVerify, "oracle_verify_report", "harbor.artifact.v1", stageprovider.CodeEdgePhase1OracleVerifyCommandID)
	oracle, err := executor.ExecuteLocalCommand(context.Background(), oracleInvocation, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.CodeEdgePhase1OracleVerifyCommandID, Arguments: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if oracle.Outcome.Verdict != workflowkit.VerdictPass {
		t.Fatalf("Oracle zero exit should pass: %+v", oracle.Outcome)
	}
	if len(runner.commands) != 6 {
		t.Fatalf("commands = %d, want build + build inspection + two image attestations + initial + oracle", len(runner.commands))
	}
	if !containsParentArg(runner.commands[1].Args, "inspect") || !containsParentArg(runner.commands[2].Args, "inspect") || !containsParentArg(runner.commands[4].Args, "inspect") {
		t.Fatalf("image identity checks = %#v / %#v / %#v", runner.commands[1].Args, runner.commands[2].Args, runner.commands[4].Args)
	}
	initialCommand, oracleCommand := runner.commands[3], runner.commands[5]
	if !containsParentArg(initialCommand.Args, "sh ./tests/test.sh") || containsParentArg(initialCommand.Args, "solution/solve.sh") || !containsParentArg(oracleCommand.Args, "sh ./solution/solve.sh && sh ./tests/test.sh") {
		t.Fatalf("controlled verifier programs = initial=%#v oracle=%#v", initialCommand.Args, oracleCommand.Args)
	}
	for _, command := range []CodeEdgePhase1Command{initialCommand, oracleCommand} {
		if !containsParentArg(command.Args, "--network") || !containsParentArg(command.Args, "none") || !containsParentArg(command.Args, "--read-only") || !containsParentArg(command.Args, "--entrypoint") || !containsParentArg(command.Args, "/bin/sh") {
			t.Fatalf("verification command missed isolation flag: %#v", command.Args)
		}
		mount := codeEdgePhase1TestArgAfter(command.Args, "--mount")
		if !strings.HasPrefix(mount, "type=bind,src=") || !strings.HasSuffix(mount, ",dst=/oracle") || strings.Contains(mount, ",rw") {
			t.Fatalf("verification mount = %q, want writable Docker --mount syntax", mount)
		}
	}
}

func TestCodeEdgePhase1VerificationCheckoutAllowsNonRootWorktreeAndSealsScripts(t *testing.T) {
	workspace := t.TempDir()
	taskRoot := filepath.Join(workspace, "task")
	for _, relative := range []string{"solution", "tests"} {
		if err := os.MkdirAll(filepath.Join(taskRoot, relative), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for relative, content := range map[string][]byte{
		"solution/solve.sh": []byte("#!/bin/sh\nexit 0\n"),
		"tests/test.sh":     []byte("#!/bin/sh\nexit 1\n"),
	} {
		if err := os.WriteFile(filepath.Join(taskRoot, filepath.FromSlash(relative)), content, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	request := workflowkit.StageExecutionRequest{Claim: workflowkit.JobClaim{Stage: &workflowkit.StageClaim{
		StageAttempt: workflowkit.AttemptIdentity{ID: workflowkit.AttemptID("verification-attempt")},
	}}}
	checkout, _, err := codeEdgePhase1PrepareVerificationCheckout(workspace, request, taskRoot, "oracle", []string{"solution/solve.sh", "tests/test.sh"})
	if err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Lstat(checkout)
	if err != nil || rootInfo.Mode().Perm() != 0o777 || rootInfo.Mode()&os.ModeSticky == 0 {
		t.Fatalf("verification checkout root mode = %v, %v; want sticky 1777", rootInfo, err)
	}
	for _, relative := range []string{"solution", "tests"} {
		info, statErr := os.Lstat(filepath.Join(checkout, relative))
		if statErr != nil || !info.IsDir() || info.Mode().Perm() != 0o755 {
			t.Fatalf("verification script directory %q = %v, %v; want 0755", relative, info, statErr)
		}
	}
	for _, relative := range []string{"solution/solve.sh", "tests/test.sh"} {
		info, statErr := os.Lstat(filepath.Join(checkout, filepath.FromSlash(relative)))
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o444 {
			t.Fatalf("verification script %q = %v, %v; want 0444", relative, info, statErr)
		}
	}
}

func codeEdgePhase1TestArgAfter(args []string, key string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == key {
			return args[index+1]
		}
	}
	return ""
}

func TestCodeEdgePhase1ParentRejectsImageIdentityDriftBeforeInitialVerifier(t *testing.T) {
	root, snapshot, digest := codeEdgePhase1ParentTaskSnapshot(t, false)
	runner := &codeEdgePhase1RecordingRunner{results: []CodeEdgePhase1CommandResult{
		{Stdout: []byte("build output\n")},
		{Stdout: []byte("sha256:" + strings.Repeat("a", 64) + "\n")},
		{Stdout: []byte("sha256:" + strings.Repeat("b", 64) + "\n")},
	}}
	executor := newCodeEdgePhase1ParentExecutorForTest(t, root, runner)
	_, buildInvocation := codeEdgePhase1ParentLocalInvocation(t, snapshot, digest, workflowadapter.DockerBuild, "docker_build_report", "harbor.artifact.v1", stageprovider.CodeEdgePhase1DockerBuildCommandID)
	if _, err := executor.ExecuteLocalCommand(context.Background(), buildInvocation, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.CodeEdgePhase1DockerBuildCommandID}); err != nil {
		t.Fatalf("build: %v", err)
	}
	_, initialInvocation := codeEdgePhase1ParentLocalInvocationForRun(t, snapshot, digest, buildInvocation.Request.Execution.ID, workflowadapter.InitialVerify, "initial_verify_report", "harbor.artifact.v1", stageprovider.CodeEdgePhase1InitialVerifyCommandID)
	result, err := executor.ExecuteLocalCommand(context.Background(), initialInvocation, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.CodeEdgePhase1InitialVerifyCommandID})
	if err != nil {
		t.Fatalf("initial verify: %v", err)
	}
	if result.Outcome.Status != workflowkit.StatusInfraFailed || result.Outcome.Failure != workflowkit.FailureProcess || !strings.Contains(result.ErrorText, "cannot be re-attested") {
		t.Fatalf("identity drift result = %+v", result)
	}
	if len(runner.commands) != 3 || containsParentArg(runner.commands[2].Args, "run") {
		t.Fatalf("image drift must stop before the verifier: %#v", runner.commands)
	}
}

func TestCodeEdgePhase1ParentRejectsArbitraryLocalArguments(t *testing.T) {
	root, snapshot, digest := codeEdgePhase1ParentTaskSnapshot(t, false)
	executor := newCodeEdgePhase1ParentExecutorForTest(t, root, &codeEdgePhase1RecordingRunner{})
	_, invocation := codeEdgePhase1ParentLocalInvocation(t, snapshot, digest, workflowadapter.DockerBuild, "docker_build_report", "harbor.artifact.v1", stageprovider.CodeEdgePhase1DockerBuildCommandID)
	_, err := executor.ExecuteLocalCommand(context.Background(), invocation, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.CodeEdgePhase1DockerBuildCommandID, Arguments: []string{"--host", "unix:///tmp/attacker.sock"}})
	if err == nil || !strings.Contains(err.Error(), "do not accept catalog argv") {
		t.Fatalf("arbitrary local argv error = %v", err)
	}
}

type codeEdgePhase1RecordingRunner struct {
	commands []CodeEdgePhase1Command
	results  []CodeEdgePhase1CommandResult
	err      error
}

func (runner *codeEdgePhase1RecordingRunner) Run(_ context.Context, command CodeEdgePhase1Command) (CodeEdgePhase1CommandResult, error) {
	copy := command
	copy.Args = append([]string(nil), command.Args...)
	copy.Env = append([]string(nil), command.Env...)
	runner.commands = append(runner.commands, copy)
	if runner.err != nil {
		return CodeEdgePhase1CommandResult{}, runner.err
	}
	index := len(runner.commands) - 1
	if index < len(runner.results) {
		return runner.results[index], nil
	}
	return CodeEdgePhase1CommandResult{}, nil
}

func newCodeEdgePhase1ParentExecutorForTest(t *testing.T, root string, runner CodeEdgePhase1CommandRunner) *CodeEdgePhase1ParentExecutor {
	t.Helper()
	executor, err := NewCodeEdgePhase1ParentExecutor(CodeEdgePhase1ParentExecutorConfig{
		ManagedRoot:      root,
		PreflightProfile: codeEdgePhase1ParentTestProfile(),
		LockedCommands: []stageprovider.LocalExecutableLock{
			{CommandID: stageprovider.CodeEdgePhase1DockerBuildCommandID, AbsolutePath: "/opt/locked/docker-build", Version: "29.5.2", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("docker-build"))},
			{CommandID: stageprovider.CodeEdgePhase1InitialVerifyCommandID, AbsolutePath: "/opt/locked/docker-initial", Version: "29.5.2", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("docker-initial"))},
			{CommandID: stageprovider.CodeEdgePhase1OracleVerifyCommandID, AbsolutePath: "/opt/locked/docker-oracle", Version: "29.5.2", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("docker-oracle"))},
		},
		Runner:         runner,
		CommandTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func codeEdgePhase1ParentTestProfile() codeedge.Profile {
	return codeedge.Profile{Metadata: codeedge.MetadataFieldMapping{
		CodeLang: codeedge.TOMLPath{"metadata", "code_lang"}, TaskType: codeedge.TOMLPath{"metadata", "task_type"},
		Application: codeedge.TOMLPath{"metadata", "application"}, IsZeroToOne: codeedge.TOMLPath{"metadata", "is_0_to_1"},
		GitHubURL: codeedge.TOMLPath{"metadata", "github_url"}, CommitID: codeedge.TOMLPath{"metadata", "commit_id"},
	}, ProtectedEnvironmentVariables: []string{"ANTHROPIC_AUTH_TOKEN", "QWEN_HARBOR_BASE_URL", "OPUS_HARBOR_BASE_URL"}}
}

func codeEdgePhase1ParentTaskSnapshot(t *testing.T, invalidMetadata bool) (string, []byte, workflowkit.SubjectDigest) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "managed")
	task := filepath.Join(root, "source-task")
	metadata := "code_lang = \"go\"\n"
	if invalidMetadata {
		metadata = ""
	}
	files := map[string]string{
		"instruction.md":         "Fix the documented behavior.\n",
		"task.toml":              "schema_version = \"1.3\"\n\n[task]\nname = \"codeedge/parent-executor\"\n\n[metadata]\n" + metadata + "task_type = \"bug-fix\"\napplication = \"backend\"\nis_0_to_1 = true\n",
		"tests_analysis.md":      "## 1. instruction 和 environment 已提供的信息\n- The task provides a visible contract.\n\n## 2. 模型的理论通过路径\n- Read the repository and implement the behavior.\n\n## 3. 模型具备通过条件的依据\n- Tests derive from the visible contract.\n",
		"environment/Dockerfile": "FROM alpine:3.22\nWORKDIR /workspace\n",
		"solution/solve.sh":      "#!/bin/sh\nexit 0\n",
		"tests/test.sh":          "#!/bin/sh\nexit 0\n",
	}
	for _, file := range taskpolicy.CanonicalFiles() {
		content, found := files[file.Path]
		if !found && file.Environment {
			continue
		}
		if !found {
			t.Fatalf("fixture missed canonical file %s", file.Path)
		}
		path := filepath.Join(task, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), file.Mode); err != nil {
			t.Fatal(err)
		}
	}
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(task)
	if err != nil {
		t.Fatal(err)
	}
	return root, codeEdgePhase1ParentZIP(t, task), workflowkit.SubjectDigest(digest)
}

func codeEdgePhase1ParentZIP(t *testing.T, task string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, file := range taskpolicy.CanonicalFiles() {
		content, err := os.ReadFile(filepath.Join(task, filepath.FromSlash(file.Path)))
		if os.IsNotExist(err) && file.Environment {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		header := &zip.FileHeader{Name: taskpolicy.ManagedSnapshotZIPRoot + "/" + file.Path, Method: zip.Deflate}
		header.SetMode(file.Mode)
		output, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := output.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func codeEdgePhase1ParentBuiltinInvocation(t *testing.T, snapshot []byte, digest workflowkit.SubjectDigest, key, outputName, schema, handlerID string) (workflowkit.StageExecutionRequest, stageprovider.StageOperationInvocation) {
	t.Helper()
	request, invocation := codeEdgePhase1ParentInvocation(t, snapshot, digest, key, outputName, schema)
	payload := workflowadapter.HarborBuiltinOperationPayload{HandlerID: handlerID}
	invocation.Resolution.Operation = workflowadapter.StageOperationBinding{ProviderID: stageprovider.CodeEdgePhase1ProviderID, OperationID: "test." + string(key), Version: "1", Payload: payload}
	return request, invocation
}

func codeEdgePhase1ParentLocalInvocation(t *testing.T, snapshot []byte, digest workflowkit.SubjectDigest, key, outputName, schema, commandID string) (workflowkit.StageExecutionRequest, stageprovider.StageOperationInvocation) {
	t.Helper()
	return codeEdgePhase1ParentLocalInvocationForRun(t, snapshot, digest, codeEdgePhase1TestUUID(t), key, outputName, schema, commandID)
}

func codeEdgePhase1ParentLocalInvocationForRun(t *testing.T, snapshot []byte, digest workflowkit.SubjectDigest, runID, key, outputName, schema, commandID string) (workflowkit.StageExecutionRequest, stageprovider.StageOperationInvocation) {
	t.Helper()
	request, invocation := codeEdgePhase1ParentInvocationWithRun(t, snapshot, digest, runID, key, outputName, schema)
	payload := workflowadapter.LocalCommandOperationPayload{CommandID: commandID, Arguments: []string{}}
	invocation.Resolution.Operation = workflowadapter.StageOperationBinding{ProviderID: stageprovider.CodeEdgePhase1ProviderID, OperationID: "test." + string(key), Version: "1", Payload: payload}
	return request, invocation
}

func codeEdgePhase1ParentInvocation(t *testing.T, snapshot []byte, digest workflowkit.SubjectDigest, key, outputName, schema string) (workflowkit.StageExecutionRequest, stageprovider.StageOperationInvocation) {
	t.Helper()
	return codeEdgePhase1ParentInvocationWithRun(t, snapshot, digest, codeEdgePhase1TestUUID(t), key, outputName, schema)
}

func codeEdgePhase1ParentInvocationWithRun(t *testing.T, snapshot []byte, digest workflowkit.SubjectDigest, runID, key, outputName, schema string) (workflowkit.StageExecutionRequest, stageprovider.StageOperationInvocation) {
	t.Helper()
	attemptID := codeEdgePhase1TestUUID(t)
	binding := workflowkit.ArtifactBinding{Name: "task_snapshot", ArtifactID: workflowkit.ArtifactID(codeEdgePhase1TestUUID(t)), ContentDigest: workflowkit.SHA256Fingerprint(snapshot), SchemaVersion: "harbor.artifact.v1"}
	plugin := workflowkit.PluginBinding{ID: "test.codeedge." + key, Version: "1"}
	stage := workflowkit.StageDescriptor{Key: workflowkit.StageKey(key), Plugin: plugin, Outputs: []workflowkit.ArtifactSpec{{Name: outputName, SchemaVersion: schema, Required: true}}}
	request := workflowkit.StageExecutionRequest{
		Execution: workflowkit.FrozenExecution{ID: runID, Subject: workflowkit.SubjectBinding{SubjectID: codeEdgePhase1TestUUID(t), RevisionID: codeEdgePhase1TestUUID(t), Digest: digest}, Workflow: workflowkit.WorkflowDescriptor{ID: workflowadapter.CodeEdgePhase1WorkflowTemplateID, Version: workflowadapter.CodeEdgePhase1WorkflowTemplateVersion}},
		Claim:     workflowkit.JobClaim{Stage: &workflowkit.StageClaim{StageAttempt: workflowkit.AttemptIdentity{ID: workflowkit.AttemptID(attemptID)}}},
		Stage:     stage,
		Inputs:    []workflowkit.ArtifactBinding{binding},
		ReadInput: func(_ context.Context, input workflowkit.ArtifactBinding) ([]byte, error) {
			if input != binding {
				return nil, errors.New("unexpected stage input")
			}
			return append([]byte(nil), snapshot...), nil
		},
	}
	return request, stageprovider.StageOperationInvocation{Request: request, Resolution: workflowadapter.StageOperationResolution{Template: workflowadapter.CodeEdgePhase1TemplateReference(), StageKey: stage.Key, Plugin: plugin}}
}

func codeEdgePhase1TestUUID(t *testing.T) string {
	t.Helper()
	id, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func codeEdgePhase1DecodeReport(t *testing.T, result workflowkit.StageExecutionResult, name, schema string) codeEdgePhase1StageReport {
	t.Helper()
	if len(result.Artifacts) != 1 || result.Artifacts[0].Name != name || result.Artifacts[0].SchemaVersion != schema {
		t.Fatalf("artifacts = %+v, want one %s@%s", result.Artifacts, name, schema)
	}
	var report codeEdgePhase1StageReport
	if err := json.Unmarshal(result.Artifacts[0].Content, &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if report.Format != codeEdgePhase1ReportFormat || report.Version != codeEdgePhase1ReportVersion {
		t.Fatalf("report format = %+v", report)
	}
	return report
}

func containsParentArg(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
