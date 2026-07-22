package evaluator

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestHarborEvaluatorLocalCommandExecutorRunsFrozenQwenAndReturnsTrustedEvidence(t *testing.T) {
	taskRoot := evaluatorTestTask(t)
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(taskRoot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := evaluatorTestSnapshotZIP(t, taskRoot)
	environment := map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "unit-token-value-not-for-artifacts",
		"QWEN_HARBOR_BASE_URL": "https://qwen.example.test/anthropic",
		"OPUS_HARBOR_BASE_URL": "https://opus.example.test/anthropic",
		"HARBOR_API_KEY":       "hub-token-must-not-reach-the-evaluator",
	}
	runner := &evaluatorFakeRunner{t: t, expectedDigest: workflowkit.SubjectDigest(digest), agentVersion: "2.1.207", model: "qwen3.7-max", stdout: []byte("mock Harbor completed endpoint=https://qwen.example.test/anthropic token=unit-token-value-not-for-artifacts")}
	executor := evaluatorTestExecutor(t, environment, runner)
	request := evaluatorTestRequest(snapshot, workflowkit.SubjectDigest(digest), workflowadapter.HarborRunQwen, "run-001", "attempt-001")

	result, err := executor.ExecuteLocalCommand(context.Background(), stageprovider.StageOperationInvocation{Request: request}, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.HarborEvaluatorQwenCommandID})
	if err != nil {
		t.Fatalf("execute frozen Qwen evaluator: %v", err)
	}
	if result.Outcome.Status != workflowkit.StatusCompleted || result.Outcome.Verdict != workflowkit.VerdictPass || len(result.Artifacts) != 2 {
		t.Fatalf("evaluator result = %+v, want completed bundle + PNG", result)
	}
	if result.Artifacts[0].Name != workflowadapter.CodeEdgeEvaluatorQwenBundleArtifact || result.Artifacts[1].Name != workflowadapter.CodeEdgeEvaluatorQwenScreenshotArtifact {
		t.Fatalf("evaluator artifact names = %#v", result.Artifacts)
	}
	if _, format, decodeErr := image.Decode(bytes.NewReader(result.Artifacts[1].Content)); decodeErr != nil || format != "png" {
		t.Fatalf("evaluator PNG decode = %q, %v", format, decodeErr)
	}
	if !runner.ran {
		t.Fatal("fake Harbor runner was not called")
	}
	assertFixedEvaluatorArgs(t, runner.command.Args, "qwen3.7-max", "2.1.207")
	assertStagedClaudeCodeMount(t, runner.command.Args)
	wantDockerPATH := executor.invocations[stageprovider.HarborEvaluatorQwenCommandID].DockerPATH
	if got := runner.command.Env; len(got) != 4 || !contains(got, "LANG=C.UTF-8") || !containsPrefix(got, "HOME=") || !containsPrefix(got, "DOCKER_CONFIG=") || !contains(got, "PATH="+wantDockerPATH) {
		t.Fatalf("Harbor process environment = %#v, want only isolated non-secret base keys", got)
	}
	if strings.Contains(strings.Join(runner.command.Args, " "), "--agent-env") || strings.Contains(strings.Join(runner.command.Args, " "), "ANTHROPIC_AUTH_TOKEN") {
		t.Fatalf("Harbor argv exposed an agent credential: %#v", runner.command.Args)
	}
	if !strings.Contains(runner.envFile, "ANTHROPIC_AUTH_TOKEN=unit-token-value-not-for-artifacts") || !strings.Contains(runner.envFile, "ANTHROPIC_BASE_URL=https://qwen.example.test/anthropic") {
		t.Fatalf("approved temporary env file missing expected controlled mapping")
	}
	if strings.Contains(runner.envFile, "HARBOR_API_KEY") || strings.Contains(runner.envFile, "hub-token-must-not-reach-the-evaluator") {
		t.Fatalf("local evaluator env file contains a forbidden Hub credential: %q", runner.envFile)
	}
	if runner.envPath == "" {
		t.Fatal("fake runner did not observe temporary env file")
	}
	if _, statErr := os.Stat(runner.envPath); !os.IsNotExist(statErr) {
		t.Fatalf("temporary Harbor env file remains after invocation: %v", statErr)
	}
	workspace := filepath.Dir(runner.jobsRoot)
	transcript, readErr := os.ReadFile(filepath.Join(workspace, "terminal-transcript.txt"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(transcript), "unit-token-value-not-for-artifacts") || strings.Contains(string(transcript), "unrelated-ambient") {
		t.Fatalf("terminal transcript leaked an environment value: %q", transcript)
	}
	if strings.Contains(string(transcript), "https://qwen.example.test/anthropic") || !strings.Contains(string(transcript), "raw_output_sha256=sha256:") {
		t.Fatalf("terminal transcript did not redact endpoint or bind raw output: %q", transcript)
	}
	if !strings.Contains(string(result.Artifacts[0].Content), "harbor-flow-provenance.json") || strings.Contains(string(result.Artifacts[0].Content), "unit-token-value-not-for-artifacts") {
		t.Fatalf("canonical Harbor bundle lacks safe output provenance or contains a secret")
	}
}

func TestHarborEvaluatorLocalCommandExecutorRunsFrozenOpusWithItsApprovedEndpoint(t *testing.T) {
	taskRoot := evaluatorTestTask(t)
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(taskRoot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := evaluatorTestSnapshotZIP(t, taskRoot)
	environment := map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "unit-token-value-not-for-artifacts",
		"QWEN_HARBOR_BASE_URL": "https://qwen.example.test/anthropic",
		"OPUS_HARBOR_BASE_URL": "https://opus.example.test/anthropic",
	}
	runner := &evaluatorFakeRunner{
		t: t, expectedDigest: workflowkit.SubjectDigest(digest), agentVersion: "2.1.207", model: "claude-opus-4-6",
		stdout: []byte("mock Harbor completed endpoint=https://opus.example.test/anthropic token=unit-token-value-not-for-artifacts"),
	}
	executor := evaluatorTestExecutor(t, environment, runner)
	request := evaluatorTestRequest(snapshot, workflowkit.SubjectDigest(digest), workflowadapter.HarborRunOpus, "run-002", "attempt-002")

	result, err := executor.ExecuteLocalCommand(context.Background(), stageprovider.StageOperationInvocation{Request: request}, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.HarborEvaluatorOpusCommandID})
	if err != nil {
		t.Fatalf("execute frozen Opus evaluator: %v", err)
	}
	if result.Outcome.Status != workflowkit.StatusCompleted || result.Outcome.Verdict != workflowkit.VerdictPass || len(result.Artifacts) != 2 {
		t.Fatalf("evaluator result = %+v, want completed bundle + PNG", result)
	}
	if result.Artifacts[0].Name != workflowadapter.CodeEdgeEvaluatorOpusBundleArtifact || result.Artifacts[1].Name != workflowadapter.CodeEdgeEvaluatorOpusScreenshotArtifact {
		t.Fatalf("evaluator artifact names = %#v", result.Artifacts)
	}
	assertFixedEvaluatorArgs(t, runner.command.Args, "claude-opus-4-6", "2.1.207")
	assertStagedClaudeCodeMount(t, runner.command.Args)
	if strings.Contains(strings.Join(runner.command.Args, " "), "ANTHROPIC_AUTH_TOKEN") || strings.Contains(strings.Join(runner.command.Args, " "), "https://") {
		t.Fatalf("Harbor argv exposed a credential or endpoint: %#v", runner.command.Args)
	}
	if !strings.Contains(runner.envFile, "ANTHROPIC_AUTH_TOKEN=unit-token-value-not-for-artifacts") || !strings.Contains(runner.envFile, "ANTHROPIC_BASE_URL=https://opus.example.test/anthropic") || strings.Contains(runner.envFile, "https://qwen.example.test/anthropic") {
		t.Fatalf("approved temporary env file did not bind only the Opus endpoint")
	}
	workspace := filepath.Dir(runner.jobsRoot)
	transcript, readErr := os.ReadFile(filepath.Join(workspace, "terminal-transcript.txt"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(transcript), "unit-token-value-not-for-artifacts") || strings.Contains(string(transcript), "https://opus.example.test/anthropic") {
		t.Fatalf("Opus terminal transcript leaked a private environment value: %q", transcript)
	}
}

func TestHarborEvaluatorLocalCommandExecutorFailsClosedForMissingOrUntrustedInput(t *testing.T) {
	taskRoot := evaluatorTestTask(t)
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(taskRoot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := evaluatorTestSnapshotZIP(t, taskRoot)
	environment := map[string]string{"ANTHROPIC_AUTH_TOKEN": "unit-token", "QWEN_HARBOR_BASE_URL": "https://qwen.example.test/anthropic", "OPUS_HARBOR_BASE_URL": "https://opus.example.test/anthropic"}
	runner := &evaluatorFakeRunner{t: t, expectedDigest: workflowkit.SubjectDigest(digest), agentVersion: "2.1.207", model: "qwen3.7-max"}
	executor := evaluatorTestExecutor(t, environment, runner)

	request := evaluatorTestRequest(snapshot, workflowkit.SubjectDigest(digest), workflowadapter.HarborRunQwen, "run-002", "attempt-002")
	_, err = executor.ExecuteLocalCommand(context.Background(), stageprovider.StageOperationInvocation{Request: request}, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.HarborEvaluatorOpusCommandID})
	if err == nil || runner.ran {
		t.Fatalf("mismatched command/stage error = %v, ran=%t; want fail closed before runner", err, runner.ran)
	}

	unsafe := evaluatorUnsafeZIP(t)
	unsafeRequest := evaluatorTestRequest(unsafe, workflowkit.SubjectDigest(digest), workflowadapter.HarborRunQwen, "run-003", "attempt-003")
	_, err = executor.ExecuteLocalCommand(context.Background(), stageprovider.StageOperationInvocation{Request: unsafeRequest}, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.HarborEvaluatorQwenCommandID})
	if err == nil || runner.ran {
		t.Fatalf("unsafe ZIP error = %v, ran=%t; want fail closed before runner", err, runner.ran)
	}

	delete(environment, "QWEN_HARBOR_BASE_URL")
	_, err = executor.ExecuteLocalCommand(context.Background(), stageprovider.StageOperationInvocation{Request: request}, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.HarborEvaluatorQwenCommandID})
	if err == nil || !strings.Contains(err.Error(), "endpoint environment") {
		t.Fatalf("missing endpoint error = %v, want controlled environment failure", err)
	}
}

func TestHarborEvaluatorLocalCommandExecutorRejectsProtectedTaskEnvironmentBeforeCredentialInjection(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		mutate  func(t *testing.T, taskRoot string)
		wantRef string
	}{
		{
			name: "Dockerfile interpolation",
			id:   "docker",
			mutate: func(t *testing.T, taskRoot string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(taskRoot, "environment", "Dockerfile"), []byte("FROM alpine:3.22\nRUN printf '%s' \"${ANTHROPIC_AUTH_TOKEN}\"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantRef: "ANTHROPIC_AUTH_TOKEN",
		},
		{
			name: "Compose pass-through",
			id:   "compose",
			mutate: func(t *testing.T, taskRoot string) {
				t.Helper()
				if err := os.Remove(filepath.Join(taskRoot, "environment", "Dockerfile")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(taskRoot, "environment", "docker-compose.yaml"), []byte("services:\n  main:\n    environment:\n      - ANTHROPIC_AUTH_TOKEN\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantRef: "ANTHROPIC_AUTH_TOKEN",
		},
		{
			name: "task TOML direct protected definition",
			id:   "task-toml-direct",
			mutate: func(t *testing.T, taskRoot string) {
				t.Helper()
				path := filepath.Join(taskRoot, "task.toml")
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				content = append(content, []byte("\n[environment.env]\nANTHROPIC_AUTH_TOKEN = \"literal-task-value\"\n")...)
				if err := os.WriteFile(path, content, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantRef: "ANTHROPIC_AUTH_TOKEN",
		},
		{
			name: "task TOML protected bare pass-through",
			id:   "task-toml-pass-through",
			mutate: func(t *testing.T, taskRoot string) {
				t.Helper()
				path := filepath.Join(taskRoot, "task.toml")
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				content = append(content, []byte("\n[environment.env]\nANTHROPIC_AUTH_TOKEN = \"${ANTHROPIC_AUTH_TOKEN}\"\n")...)
				if err := os.WriteFile(path, content, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantRef: "ANTHROPIC_AUTH_TOKEN",
		},
		{
			name: "task TOML protected alias interpolation",
			id:   "task-toml-alias",
			mutate: func(t *testing.T, taskRoot string) {
				t.Helper()
				path := filepath.Join(taskRoot, "task.toml")
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				content = append(content, []byte("\n[environment.env]\nLEAK = \"${ANTHROPIC_AUTH_TOKEN:-fallback}\"\n")...)
				if err := os.WriteFile(path, content, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantRef: "ANTHROPIC_AUTH_TOKEN",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			taskRoot := evaluatorTestTask(t)
			test.mutate(t, taskRoot)
			digest, err := taskpolicy.ComputeManagedTaskDigestV2(taskRoot)
			if err != nil {
				t.Fatal(err)
			}
			snapshot := evaluatorTestSnapshotZIP(t, taskRoot)
			environment := map[string]string{"ANTHROPIC_AUTH_TOKEN": "unit-token", "QWEN_HARBOR_BASE_URL": "https://qwen.example.test/anthropic", "OPUS_HARBOR_BASE_URL": "https://opus.example.test/anthropic"}
			runner := &evaluatorFakeRunner{t: t, expectedDigest: workflowkit.SubjectDigest(digest), agentVersion: "2.1.207", model: "qwen3.7-max"}
			executor := evaluatorTestExecutor(t, environment, runner)
			credentialLookups := 0
			lookup := executor.lookupEnv
			executor.lookupEnv = func(key string) (string, bool) {
				credentialLookups++
				return lookup(key)
			}
			request := evaluatorTestRequest(snapshot, workflowkit.SubjectDigest(digest), workflowadapter.HarborRunQwen, "run-preflight-"+test.id, "attempt-preflight-"+test.id)

			_, err = executor.ExecuteLocalCommand(context.Background(), stageprovider.StageOperationInvocation{Request: request}, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.HarborEvaluatorQwenCommandID})
			if err == nil || !strings.Contains(err.Error(), "preflight frozen Harbor task environment") || !strings.Contains(err.Error(), test.wantRef) {
				t.Fatalf("protected task environment error = %v, want preflight rejection for %s", err, test.wantRef)
			}
			if runner.ran {
				t.Fatal("Harbor runner was called after protected task environment preflight rejection")
			}
			if credentialLookups != 0 {
				t.Fatalf("preflight rejection read %d credential environment values", credentialLookups)
			}
			workspace := filepath.Join(executor.root, request.Execution.ID, "external-evaluators", string(request.Claim.Stage.StageAttempt.ID), "initial")
			if _, statErr := os.Stat(filepath.Join(workspace, "harbor.env")); !os.IsNotExist(statErr) {
				t.Fatalf("credential env file exists after preflight rejection: %v", statErr)
			}
		})
	}
}

func TestHarborEvaluatorLocalCommandExecutorClassifiesNonzeroWithoutSuccessEvidence(t *testing.T) {
	taskRoot := evaluatorTestTask(t)
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(taskRoot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := evaluatorTestSnapshotZIP(t, taskRoot)
	environment := map[string]string{"ANTHROPIC_AUTH_TOKEN": "unit-token", "QWEN_HARBOR_BASE_URL": "https://qwen.example.test/anthropic", "OPUS_HARBOR_BASE_URL": "https://opus.example.test/anthropic"}
	runner := &evaluatorFakeRunner{t: t, exitCode: 12}
	executor := evaluatorTestExecutor(t, environment, runner)
	request := evaluatorTestRequest(snapshot, workflowkit.SubjectDigest(digest), workflowadapter.HarborRunQwen, "run-004", "attempt-004")
	result, err := executor.ExecuteLocalCommand(context.Background(), stageprovider.StageOperationInvocation{Request: request}, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.HarborEvaluatorQwenCommandID})
	if err != nil {
		t.Fatalf("nonzero Harbor command execution: %v", err)
	}
	if result.Outcome.Status != workflowkit.StatusInfraFailed || result.Outcome.Failure != workflowkit.FailureProcess || len(result.Artifacts) != 0 {
		t.Fatalf("nonzero evaluator result = %+v, want infra_failed without evidence", result)
	}
}

func TestHarborEvaluatorLocalCommandExecutorRejectsObservedAgentVersionDrift(t *testing.T) {
	taskRoot := evaluatorTestTask(t)
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(taskRoot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := evaluatorTestSnapshotZIP(t, taskRoot)
	environment := map[string]string{"ANTHROPIC_AUTH_TOKEN": "unit-token", "QWEN_HARBOR_BASE_URL": "https://qwen.example.test/anthropic", "OPUS_HARBOR_BASE_URL": "https://opus.example.test/anthropic"}
	runner := &evaluatorFakeRunner{t: t, expectedDigest: workflowkit.SubjectDigest(digest), agentVersion: "2.1.143", model: "qwen3.7-max"}
	executor := evaluatorTestExecutor(t, environment, runner)
	request := evaluatorTestRequest(snapshot, workflowkit.SubjectDigest(digest), workflowadapter.HarborRunQwen, "run-005", "attempt-005")
	_, err = executor.ExecuteLocalCommand(context.Background(), stageprovider.StageOperationInvocation{Request: request}, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.HarborEvaluatorQwenCommandID})
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("agent version drift error = %v, want frozen identity rejection", err)
	}
}

func TestHarborEvaluatorLocalCommandExecutorRejectsPrelaunchDriftBeforeCredentialLookup(t *testing.T) {
	taskRoot := evaluatorTestTask(t)
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(taskRoot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := evaluatorTestSnapshotZIP(t, taskRoot)
	environment := map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "unit-token",
		"QWEN_HARBOR_BASE_URL": "https://qwen.example.test/anthropic",
		"OPUS_HARBOR_BASE_URL": "https://opus.example.test/anthropic",
	}
	runner := &evaluatorFakeRunner{t: t}
	executor := evaluatorTestExecutor(t, environment, runner)
	prelaunch := &evaluatorFakePrelaunchAttestor{t: t, runner: runner, err: errors.New("runtime drift")}
	executor.prelaunchAttestor = prelaunch
	credentialLookups := 0
	lookup := executor.lookupEnv
	executor.lookupEnv = func(key string) (string, bool) {
		credentialLookups++
		return lookup(key)
	}
	request := evaluatorTestRequest(snapshot, workflowkit.SubjectDigest(digest), workflowadapter.HarborRunQwen, "run-prelaunch-drift", "attempt-prelaunch-drift")
	_, err = executor.ExecuteLocalCommand(context.Background(), stageprovider.StageOperationInvocation{Request: request}, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.HarborEvaluatorQwenCommandID})
	if err == nil || !strings.Contains(err.Error(), "immediately before launch") {
		t.Fatalf("prelaunch drift error = %v, want launch-time attestation rejection", err)
	}
	if prelaunch.calls != 1 || runner.ran || credentialLookups != 0 {
		t.Fatalf("prelaunch rejection calls=%d runner=%t credential_lookups=%d", prelaunch.calls, runner.ran, credentialLookups)
	}
}

func TestHarborEvaluatorLocalCommandExecutorRejectsDriftAfterCredentialMaterializationBeforeRunner(t *testing.T) {
	taskRoot := evaluatorTestTask(t)
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(taskRoot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := evaluatorTestSnapshotZIP(t, taskRoot)
	environment := map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "unit-token",
		"QWEN_HARBOR_BASE_URL": "https://qwen.example.test/anthropic",
		"OPUS_HARBOR_BASE_URL": "https://opus.example.test/anthropic",
	}
	runner := &evaluatorFakeRunner{t: t}
	executor := evaluatorTestExecutor(t, environment, runner)
	prelaunch := &evaluatorFakePrelaunchAttestor{t: t, runner: runner, err: errors.New("late runtime drift"), failOnCall: 2}
	executor.prelaunchAttestor = prelaunch
	credentialLookups := 0
	lookup := executor.lookupEnv
	executor.lookupEnv = func(key string) (string, bool) {
		credentialLookups++
		return lookup(key)
	}
	request := evaluatorTestRequest(snapshot, workflowkit.SubjectDigest(digest), workflowadapter.HarborRunQwen, "run-late-prelaunch-drift", "attempt-late-prelaunch-drift")
	_, err = executor.ExecuteLocalCommand(context.Background(), stageprovider.StageOperationInvocation{Request: request}, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.HarborEvaluatorQwenCommandID})
	if err == nil || !strings.Contains(err.Error(), "immediately before process start") {
		t.Fatalf("late prelaunch drift error = %v, want final launch-time attestation rejection", err)
	}
	if prelaunch.calls != 2 || runner.ran || credentialLookups == 0 {
		t.Fatalf("late prelaunch rejection calls=%d runner=%t credential_lookups=%d", prelaunch.calls, runner.ran, credentialLookups)
	}
	workspace := filepath.Join(executor.root, request.Execution.ID, "external-evaluators", string(request.Claim.Stage.StageAttempt.ID), "initial")
	if _, statErr := os.Stat(filepath.Join(workspace, "harbor.env")); !os.IsNotExist(statErr) {
		t.Fatalf("temporary credential file remains after final prelaunch rejection: %v", statErr)
	}
}

func TestHarborEvaluatorLocalCommandExecutorObservesCompletedJobWithoutRerun(t *testing.T) {
	taskRoot := evaluatorTestTask(t)
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(taskRoot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := evaluatorTestSnapshotZIP(t, taskRoot)
	environment := map[string]string{"ANTHROPIC_AUTH_TOKEN": "unit-token", "QWEN_HARBOR_BASE_URL": "https://qwen.example.test/anthropic", "OPUS_HARBOR_BASE_URL": "https://opus.example.test/anthropic"}
	runner := &evaluatorFakeRunner{t: t, expectedDigest: workflowkit.SubjectDigest(digest), agentVersion: "2.1.207", model: "qwen3.7-max"}
	executor := evaluatorTestExecutor(t, environment, runner)
	request := evaluatorTestRequest(snapshot, workflowkit.SubjectDigest(digest), workflowadapter.HarborRunQwen, "run-006", "attempt-006")
	invocation := stageprovider.StageOperationInvocation{Request: request}
	payload := workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.HarborEvaluatorQwenCommandID}
	if _, err := executor.ExecuteLocalCommand(context.Background(), invocation, payload); err != nil {
		t.Fatal(err)
	}
	calls := runner.calls
	observed, complete, err := executor.ObserveCompletedHarborEvaluator(context.Background(), invocation, payload)
	if err != nil || !complete || observed.Outcome.Status != workflowkit.StatusCompleted || len(observed.Artifacts) != 2 {
		t.Fatalf("read-only evaluator observation = %+v complete=%t err=%v", observed, complete, err)
	}
	if runner.calls != calls {
		t.Fatalf("reconciliation reran Harbor: calls before=%d after=%d", calls, runner.calls)
	}

	if err := os.WriteFile(filepath.Join(runner.jobsRoot, evaluatorJobName(request, payload.CommandID), "harbor-flow-provenance.json"), []byte(`{"format":"wrong"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, complete, err = executor.ObserveCompletedHarborEvaluator(context.Background(), invocation, payload)
	if err == nil || complete {
		t.Fatalf("malformed local provenance observed=%t err=%v, want fail closed", complete, err)
	}
}

func TestHarborEvaluatorLocalCommandExecutorObservationLeavesMissingWorkspaceUnresolved(t *testing.T) {
	taskRoot := evaluatorTestTask(t)
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(taskRoot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := evaluatorTestSnapshotZIP(t, taskRoot)
	environment := map[string]string{"ANTHROPIC_AUTH_TOKEN": "unit-token", "QWEN_HARBOR_BASE_URL": "https://qwen.example.test/anthropic", "OPUS_HARBOR_BASE_URL": "https://opus.example.test/anthropic"}
	runner := &evaluatorFakeRunner{t: t}
	executor := evaluatorTestExecutor(t, environment, runner)
	request := evaluatorTestRequest(snapshot, workflowkit.SubjectDigest(digest), workflowadapter.HarborRunQwen, "run-007", "attempt-007")
	result, observed, err := executor.ObserveCompletedHarborEvaluator(context.Background(), stageprovider.StageOperationInvocation{Request: request}, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.HarborEvaluatorQwenCommandID})
	if err != nil || observed || result.Outcome.Status != "" || runner.ran {
		t.Fatalf("missing workspace observation = %+v observed=%t err=%v ran=%t", result, observed, err, runner.ran)
	}
}

func TestHarborEvaluatorLocalCommandExecutorRejectsHubCredentialMapping(t *testing.T) {
	environment := map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "unit-token",
		"QWEN_HARBOR_BASE_URL": "https://qwen.example.test/anthropic",
		"OPUS_HARBOR_BASE_URL": "https://opus.example.test/anthropic",
	}
	executor := evaluatorTestExecutor(t, environment, &evaluatorFakeRunner{t: t})
	invocation := executor.invocations[stageprovider.HarborEvaluatorQwenCommandID].Clone()
	invocation.SecretEnvTemplates[0].HostEnvKey = "HARBOR_API_KEY"
	if err := validateInvocation(invocation); err == nil || !strings.Contains(err.Error(), "HARBOR_API_KEY") {
		t.Fatalf("Hub credential mapping error = %v, want local-only rejection", err)
	}
}

func TestHarborEvaluatorLocalCommandExecutorRejectsUnboundDockerCLI(t *testing.T) {
	environment := map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "unit-token",
		"QWEN_HARBOR_BASE_URL": "https://qwen.example.test/anthropic",
		"OPUS_HARBOR_BASE_URL": "https://opus.example.test/anthropic",
	}
	executor := evaluatorTestExecutor(t, environment, &evaluatorFakeRunner{t: t})
	invocation := executor.invocations[stageprovider.HarborEvaluatorQwenCommandID].Clone()

	invocation.DockerCLIPath = "docker"
	if err := validateInvocation(invocation); err == nil {
		t.Fatal("relative Docker CLI path was accepted")
	}

	invocation.DockerCLIPath = "/locked/docker/bin/docker"
	invocation.DockerVersion = "29.4.1"
	if err := validateInvocation(invocation); err == nil {
		t.Fatal("drifted Docker CLI version was accepted")
	}

	invocation.DockerCLIPath = "/locked/docker/bin/not-docker"
	invocation.DockerVersion = stageprovider.HarborEvaluatorDockerVersion
	if err := validateInvocation(invocation); err == nil {
		t.Fatal("Docker CLI with a non-docker basename was accepted")
	}

	invocation.DockerCLIPath = "/locked/docker:shadow/bin/docker"
	invocation.DockerVersion = stageprovider.HarborEvaluatorDockerVersion
	if err := validateInvocation(invocation); err == nil {
		t.Fatal("Docker CLI path containing a PATH separator was accepted")
	}

	invocation = executor.invocations[stageprovider.HarborEvaluatorQwenCommandID].Clone()
	invocation.DockerPATH = "/usr/bin:/bin"
	if err := validateInvocation(invocation); err == nil {
		t.Fatal("Docker PATH not derived from the locked CLI was accepted")
	}

	invocation = executor.invocations[stageprovider.HarborEvaluatorQwenCommandID].Clone()
	invocation.DockerComposePluginPath = "/locked/docker/cli-plugins/compose"
	if err := validateInvocation(invocation); err == nil {
		t.Fatal("Compose plugin with a non-docker-compose basename was accepted")
	}

	for name, mutate := range map[string]func(*stageprovider.HarborEvaluatorInvocation){
		"missing launcher version": func(candidate *stageprovider.HarborEvaluatorInvocation) { candidate.LauncherVersion = "" },
		"relative Claude Code executable": func(candidate *stageprovider.HarborEvaluatorInvocation) {
			candidate.ClaudeCodeExecutablePath = "claude"
		},
		"missing Claude Code version":        func(candidate *stageprovider.HarborEvaluatorInvocation) { candidate.ClaudeCodeVersion = "" },
		"Claude Code agent version drift":    func(candidate *stageprovider.HarborEvaluatorInvocation) { candidate.ClaudeCodeVersion = "2.1.206" },
		"missing Claude Code digest":         func(candidate *stageprovider.HarborEvaluatorInvocation) { candidate.ClaudeCodeContentSHA256 = "" },
		"relative Python interpreter":        func(candidate *stageprovider.HarborEvaluatorInvocation) { candidate.PythonInterpreterPath = "python" },
		"missing Python interpreter version": func(candidate *stageprovider.HarborEvaluatorInvocation) { candidate.PythonInterpreterVersion = "" },
		"missing Python interpreter digest": func(candidate *stageprovider.HarborEvaluatorInvocation) {
			candidate.PythonInterpreterContentSHA256 = ""
		},
		"relative Python source tree": func(candidate *stageprovider.HarborEvaluatorInvocation) {
			candidate.PythonSourceTreePath = "site-packages/harbor"
		},
		"missing Python source digest": func(candidate *stageprovider.HarborEvaluatorInvocation) { candidate.PythonSourceFilesSHA256 = "" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := executor.invocations[stageprovider.HarborEvaluatorQwenCommandID].Clone()
			mutate(&candidate)
			if err := validateInvocation(candidate); err == nil {
				t.Fatalf("invalid Python/launcher runtime identity was accepted: %+v", candidate)
			}
		})
	}
}

func TestHarborEvaluatorLocalCommandExecutorRejectsClaudeCodeStagingDrift(t *testing.T) {
	taskRoot := evaluatorTestTask(t)
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(taskRoot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := evaluatorTestSnapshotZIP(t, taskRoot)
	environment := map[string]string{"ANTHROPIC_AUTH_TOKEN": "unit-token", "QWEN_HARBOR_BASE_URL": "https://qwen.example.test/anthropic", "OPUS_HARBOR_BASE_URL": "https://opus.example.test/anthropic"}
	runner := &evaluatorFakeRunner{t: t}
	executor := evaluatorTestExecutor(t, environment, runner)
	config := executor.invocations[stageprovider.HarborEvaluatorQwenCommandID]
	if err := os.WriteFile(config.ClaudeCodeExecutablePath, []byte("drifted Claude Code fixture\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	request := evaluatorTestRequest(snapshot, workflowkit.SubjectDigest(digest), workflowadapter.HarborRunQwen, "run-claude-drift", "attempt-claude-drift")
	_, err = executor.ExecuteLocalCommand(context.Background(), stageprovider.StageOperationInvocation{Request: request}, workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.HarborEvaluatorQwenCommandID})
	if err == nil || !strings.Contains(err.Error(), "stage locked Claude Code executable") || runner.ran {
		t.Fatalf("Claude Code staging drift error = %v, runner=%t; want fail closed before Harbor", err, runner.ran)
	}
}

type evaluatorFakeRunner struct {
	t              *testing.T
	expectedDigest workflowkit.SubjectDigest
	agentVersion   string
	model          string
	exitCode       int
	ran            bool
	calls          int
	command        Command
	envPath        string
	envFile        string
	jobsRoot       string
	stdout         []byte
}

type evaluatorFakePrelaunchAttestor struct {
	t          *testing.T
	runner     CommandRunner
	err        error
	failOnCall int
	calls      int
}

func (attestor *evaluatorFakePrelaunchAttestor) AttestHarborEvaluatorInvocationBeforeLaunch(_ context.Context, invocation stageprovider.HarborEvaluatorInvocation, home string) ([]string, error) {
	attestor.t.Helper()
	attestor.calls++
	if fake, ok := attestor.runner.(*evaluatorFakeRunner); ok && fake.ran {
		attestor.t.Fatal("prelaunch attestation ran after the Harbor command")
	}
	if attestor.err != nil && (attestor.failOnCall == 0 || attestor.calls == attestor.failOnCall) {
		return nil, attestor.err
	}
	for _, directory := range []string{home, filepath.Join(home, ".docker")} {
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			attestor.t.Fatalf("prelaunch directory %q is invalid: %v", directory, err)
		}
	}
	return []string{
		"DOCKER_CONFIG=" + filepath.Join(home, ".docker"),
		"HOME=" + home,
		"LANG=C.UTF-8",
		"PATH=" + invocation.DockerPATH,
	}, nil
}

func (runner *evaluatorFakeRunner) Run(_ context.Context, command Command) (CommandResult, error) {
	runner.t.Helper()
	runner.ran = true
	runner.calls++
	runner.command = command
	arguments := evaluatorArgs(command.Args)
	runner.envPath = arguments["--env-file"]
	runner.jobsRoot = arguments["--jobs-dir"]
	raw, err := os.ReadFile(runner.envPath)
	if err != nil {
		runner.t.Fatalf("read controlled env file: %v", err)
	}
	runner.envFile = string(raw)
	if runner.exitCode != 0 {
		return CommandResult{ExitCode: runner.exitCode, Stderr: []byte("ANTHROPIC_AUTH_TOKEN=unit-token\nmock Harbor failure")}, nil
	}
	jobRoot := filepath.Join(arguments["--jobs-dir"], arguments["--job-name"])
	evaluatorWriteMockHarborJob(runner.t, jobRoot, arguments["--path"], runner.expectedDigest, runner.agentVersion, runner.model)
	stdout := runner.stdout
	if len(stdout) == 0 {
		stdout = []byte("mock Harbor completed")
	}
	return CommandResult{ExitCode: 0, Stdout: stdout}, nil
}

func evaluatorTestExecutor(t *testing.T, environment map[string]string, runner CommandRunner) *HarborEvaluatorLocalCommandExecutor {
	t.Helper()
	qwenEndpoint, err := stageprovider.CanonicalHarborEvaluatorEndpointFingerprint(environment["QWEN_HARBOR_BASE_URL"])
	if err != nil {
		t.Fatal(err)
	}
	opusEndpoint, err := stageprovider.CanonicalHarborEvaluatorEndpointFingerprint(environment["OPUS_HARBOR_BASE_URL"])
	if err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	launcherPath := filepath.Join(runtimeRoot, "bin", "harbor")
	pythonPath := filepath.Join(runtimeRoot, "bin", "python")
	sourceTreePath := filepath.Join(runtimeRoot, "site-packages", "harbor")
	claudePath := filepath.Join(runtimeRoot, "bin", "claude")
	dockerPath := filepath.Join(runtimeRoot, "bin", "docker")
	composePath := filepath.Join(runtimeRoot, "libexec", "docker", "cli-plugins", "docker-compose")
	buildxPath := filepath.Join(runtimeRoot, "libexec", "docker", "cli-plugins", "docker-buildx")
	launcherContents := []byte("#!/bin/sh\nexit 0\n# locked Harbor launcher fixture\n")
	pythonContents := []byte("#!/bin/sh\nexit 0\n# locked Python fixture\n")
	claudeContents := []byte("#!/bin/sh\necho '2.1.207 (Claude Code)'\n# locked Claude Code fixture\n")
	sourceContents := []byte("VERSION = '0.18.0'\n")
	dockerContents := []byte("#!/bin/sh\nexit 0\n# locked Docker CLI fixture\n")
	composeContents := []byte("#!/bin/sh\nexit 0\n# locked Docker Compose fixture\n")
	buildxContents := []byte("#!/bin/sh\nexit 0\n# locked Docker Buildx fixture\n")
	for path, contents := range map[string][]byte{
		launcherPath: launcherContents,
		claudePath:   claudeContents,
		pythonPath:   pythonContents,
		filepath.Join(sourceTreePath, "__init__.py"): sourceContents,
		dockerPath:  dockerContents,
		composePath: composeContents,
		buildxPath:  buildxContents,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	makeInvocation := func(commandID, model, endpointName string, fingerprint workflowkit.Fingerprint) stageprovider.HarborEvaluatorInvocation {
		dockerPATH, err := stageprovider.HarborEvaluatorDockerPATH(dockerPath)
		if err != nil {
			t.Fatal(err)
		}
		return stageprovider.HarborEvaluatorInvocation{
			CommandID: commandID, LauncherPath: launcherPath, LauncherVersion: "0.18.0-test", LauncherContentSHA256: workflowkit.SHA256Fingerprint(launcherContents),
			ClaudeCodeExecutablePath: claudePath, ClaudeCodeVersion: "2.1.207", ClaudeCodeContentSHA256: workflowkit.SHA256Fingerprint(claudeContents),
			PythonInterpreterPath: pythonPath, PythonInterpreterVersion: "3.13.5", PythonInterpreterContentSHA256: workflowkit.SHA256Fingerprint(pythonContents),
			PythonSourceTreePath: sourceTreePath, PythonSourceFilesSHA256: workflowkit.SHA256Fingerprint(sourceContents),
			DockerCLIPath: dockerPath, DockerCLIContentSHA256: workflowkit.SHA256Fingerprint(dockerContents), DockerPATH: dockerPATH,
			DockerVersion: stageprovider.HarborEvaluatorDockerVersion, DockerServerVersion: stageprovider.HarborEvaluatorDockerServerVersion,
			DockerComposePluginPath: composePath, DockerComposeContentSHA256: workflowkit.SHA256Fingerprint(composeContents), DockerComposeVersion: stageprovider.HarborEvaluatorDockerComposeVersion, DockerComposeVersionOutput: stageprovider.HarborEvaluatorDockerComposeVersionOutput,
			DockerBuildxPluginPath: buildxPath, DockerBuildxContentSHA256: workflowkit.SHA256Fingerprint(buildxContents), DockerBuildxVersion: stageprovider.HarborEvaluatorDockerBuildxVersion, DockerBuildxVersionOutput: stageprovider.HarborEvaluatorDockerBuildxVersionOutput,
			HarborVersion: stageprovider.HarborEvaluatorHarborVersion, ResultABIFormat: stageprovider.HarborEvaluatorResultABIFormat, ResultABIVersion: stageprovider.HarborEvaluatorResultABIVersion,
			TaskArtifactPort: stageprovider.HarborEvaluatorTaskArtifactPort, TaskArtifactSchema: stageprovider.HarborEvaluatorTaskArtifactSchema,
			AgentID: "claude-code", AgentVersion: "2.1.207", ModelID: model, ModelVersion: "frozen",
			EndpointEnvName: endpointName, EndpointChildEnvKey: "ANTHROPIC_BASE_URL", EndpointFingerprint: fingerprint,
			SecretEnvTemplates: []stageprovider.HarborEvaluatorSecretEnvTemplate{{Secret: workflowadapter.SecretReference{ID: "anthropic-auth", Provider: "environment", Version: "1"}, HostEnvKey: "ANTHROPIC_AUTH_TOKEN", ChildEnvKey: "ANTHROPIC_AUTH_TOKEN", Template: stageprovider.HarborEvaluatorSecretValueTemplate}},
			Attempts:           4, ConcurrentTrials: 1, MaxRetries: stageprovider.HarborEvaluatorMaxRetries, RequireTrajectory: true,
			ScreenshotRenderer: stageprovider.HarborEvaluatorScreenshotRenderer{ID: stageprovider.HarborEvaluatorTerminalPNGRendererID, Version: stageprovider.HarborEvaluatorTerminalPNGRendererVersion, SchemaVersion: stageprovider.HarborEvaluatorTerminalPNGRendererSchemaVersion},
		}
	}
	executor, err := NewHarborEvaluatorLocalCommandExecutor(HarborEvaluatorLocalCommandExecutorConfig{
		WorkspaceRoot: filepath.Join(t.TempDir(), "managed-runs"),
		Invocations: []stageprovider.HarborEvaluatorInvocation{
			makeInvocation(stageprovider.HarborEvaluatorQwenCommandID, "qwen3.7-max", "QWEN_HARBOR_BASE_URL", qwenEndpoint),
			makeInvocation(stageprovider.HarborEvaluatorOpusCommandID, "claude-opus-4-6", "OPUS_HARBOR_BASE_URL", opusEndpoint),
		},
		LookupEnv:         func(key string) (string, bool) { value, found := environment[key]; return value, found },
		PrelaunchAttestor: &evaluatorFakePrelaunchAttestor{t: t, runner: runner},
		Runner:            runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func evaluatorTestRequest(snapshot []byte, digest workflowkit.SubjectDigest, stage string, runID, attemptID string) workflowkit.StageExecutionRequest {
	binding := workflowkit.ArtifactBinding{Name: stageprovider.HarborEvaluatorTaskArtifactPort, ArtifactID: "snapshot", ContentDigest: workflowkit.SHA256Fingerprint(snapshot), SchemaVersion: stageprovider.HarborEvaluatorTaskArtifactSchema}
	return workflowkit.StageExecutionRequest{
		Execution: workflowkit.FrozenExecution{ID: runID, Subject: workflowkit.SubjectBinding{SubjectID: "task", RevisionID: "revision", Digest: digest}},
		Claim:     workflowkit.JobClaim{Stage: &workflowkit.StageClaim{StageAttempt: workflowkit.AttemptIdentity{ID: workflowkit.AttemptID(attemptID)}}},
		Stage:     workflowkit.StageDescriptor{Key: workflowkit.StageKey(stage)}, Inputs: []workflowkit.ArtifactBinding{binding},
		ReadInput: func(_ context.Context, input workflowkit.ArtifactBinding) ([]byte, error) {
			if input != binding {
				return nil, os.ErrNotExist
			}
			return append([]byte(nil), snapshot...), nil
		},
	}
}

func evaluatorTestTask(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "task")
	files := map[string]string{
		"instruction.md": "Fix the issue.\n", "task.toml": "[task]\nname = \"fixture\"\n", "tests_analysis.md": "analysis\n",
		"environment/Dockerfile": "FROM alpine:3.22\n", "solution/solve.sh": "#!/bin/sh\nexit 0\n", "tests/test.sh": "#!/bin/sh\nexit 0\n",
	}
	for relative, content := range files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(relative, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func evaluatorTestSnapshotZIP(t *testing.T, root string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, file := range taskpolicy.CanonicalFiles() {
		path := filepath.Join(root, filepath.FromSlash(file.Path))
		content, err := os.ReadFile(path)
		if os.IsNotExist(err) && file.Environment {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		header := &zip.FileHeader{Name: "task/" + file.Path, Method: zip.Deflate}
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

func evaluatorUnsafeZIP(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	output, err := writer.Create("task/../escape")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte("escape")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func evaluatorWriteMockHarborJob(t *testing.T, root, taskRoot string, digest workflowkit.SubjectDigest, agentVersion, model string) {
	t.Helper()
	evaluatorWriteJSON(t, filepath.Join(root, "config.json"), map[string]any{"n_attempts": 4, "tasks": []any{map[string]any{"path": taskRoot}}, "datasets": []any{}, "agents": []any{map[string]any{"name": "claude-code", "model_name": model}}})
	evaluatorWriteJSON(t, filepath.Join(root, "lock.json"), map[string]any{"schema_version": 2, "harbor": map[string]any{"version": "0.18.0"}, "n_concurrent_trials": 1, "retry": map[string]any{"max_retries": 3}, "trials": []any{}})
	evaluatorWriteJSON(t, filepath.Join(root, "result.json"), map[string]any{
		"id": "mock-job", "started_at": "2026-07-14T00:00:00Z", "finished_at": "2026-07-14T00:05:00Z", "n_total_trials": 4,
		"stats": map[string]any{
			"n_running_trials": 0, "n_pending_trials": 0, "n_retries": 0,
			"evals": map[string]any{
				"claude-code__" + model + "__adhoc": map[string]any{"pass_at_k": map[string]any{"4": 1}},
			},
		},
	})
	for index := 0; index < 4; index++ {
		name := "task__trial-" + string(rune('a'+index))
		trial := filepath.Join(root, name)
		evaluatorWriteJSON(t, filepath.Join(trial, "config.json"), map[string]any{"job_id": "mock-job"})
		evaluatorWriteJSON(t, filepath.Join(trial, "lock.json"), map[string]any{"task": map[string]any{"digest": "sha256:" + strings.Repeat("a", 64)}})
		evaluatorWriteJSON(t, filepath.Join(trial, "result.json"), map[string]any{"id": "trial-" + string(rune('a'+index)), "trial_name": name, "task_checksum": "dirhash-" + string(rune('a'+index)), "config": map[string]any{"job_id": "mock-job"}, "started_at": "2026-07-14T00:00:00Z", "finished_at": "2026-07-14T00:01:00Z", "agent_info": map[string]any{"name": "claude-code", "version": agentVersion, "model_info": map[string]any{"name": model}}, "verifier_result": map[string]any{"rewards": map[string]any{"reward": map[bool]int{true: 1, false: 0}[index == 0]}}})
		evaluatorWriteJSON(t, filepath.Join(trial, "agent", "trajectory.json"), map[string]any{"final_metrics": map[string]any{"total_steps": 20}})
	}
	_ = digest // The capture path independently proves the materialized digest.
}

func evaluatorWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func evaluatorArgs(arguments []string) map[string]string {
	values := make(map[string]string)
	for index := 0; index+1 < len(arguments); index++ {
		if strings.HasPrefix(arguments[index], "--") {
			values[arguments[index]] = arguments[index+1]
			index++
		}
	}
	return values
}

func assertFixedEvaluatorArgs(t *testing.T, arguments []string, model, version string) {
	t.Helper()
	values := evaluatorArgs(arguments)
	for flag, want := range map[string]string{"--agent": "claude-code", "--model": model, "--n-attempts": "4", "--n-concurrent": "1", "--max-retries": "3"} {
		if values[flag] != want {
			t.Fatalf("Harbor argv %s = %q, want %q; argv=%#v", flag, values[flag], want, arguments)
		}
	}
	if !containsPair(arguments, "--agent-kwarg", "version="+version) || contains(arguments, "--upload") || contains(arguments, "--private") || contains(arguments, "--public") || contains(arguments, "--share-org") || contains(arguments, "--share-user") || !contains(arguments, "--quiet") || !contains(arguments, "--yes") {
		t.Fatalf("Harbor argv does not freeze the local-only evaluator command: %#v", arguments)
	}
}

func assertStagedClaudeCodeMount(t *testing.T, arguments []string) {
	t.Helper()
	mountJSON := evaluatorArgs(arguments)["--mounts"]
	var mounts []map[string]any
	if err := json.Unmarshal([]byte(mountJSON), &mounts); err != nil {
		t.Fatalf("Harbor --mounts JSON = %q: %v", mountJSON, err)
	}
	if len(mounts) != 1 || mounts[0]["type"] != "bind" || mounts[0]["target"] != claudeCodeContainerPath || mounts[0]["read_only"] != true {
		t.Fatalf("Harbor --mounts = %#v, want one fixed read-only Claude Code mount", mounts)
	}
	bind, ok := mounts[0]["bind"].(map[string]any)
	if !ok || bind["create_host_path"] != false {
		t.Fatalf("Harbor --mounts bind policy = %#v, want create_host_path=false", mounts[0]["bind"])
	}
	source, ok := mounts[0]["source"].(string)
	if !ok || filepath.Base(source) != stagedClaudeCodeFilename || filepath.Base(filepath.Dir(source)) != stagedClaudeCodeDirectory {
		t.Fatalf("Harbor --mounts source = %q, want controlled attempt-local Claude binary", source)
	}
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o555 {
		t.Fatalf("staged Claude Code executable %q is invalid: %v", source, err)
	}
	// encoding/json sorts map keys, so this exact value is stable evidence of
	// the fixed compose mount ABI rather than merely a semantically similar map.
	want := `[{"bind":{"create_host_path":false},"read_only":true,"source":"` + source + `","target":"/usr/local/bin/claude","type":"bind"}]`
	if mountJSON != want {
		t.Fatalf("canonical Harbor --mounts = %q, want %q", mountJSON, want)
	}
}

func contains(values []string, wanted string) bool {
	return anyEqual(values, wanted)
}

func anyEqual(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsPair(values []string, left, right string) bool {
	for index := 0; index+1 < len(values); index++ {
		if values[index] == left && (right == "" || values[index+1] == right) {
			return true
		}
	}
	return false
}

func containsPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
