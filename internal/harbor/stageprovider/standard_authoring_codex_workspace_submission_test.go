package stageprovider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/authoringharness"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

type standardAuthoringHarnessValidatorFunc func(context.Context, authoringharness.Request) (authoringharness.Result, error)

func (validate standardAuthoringHarnessValidatorFunc) Validate(ctx context.Context, request authoringharness.Request) (authoringharness.Result, error) {
	return validate(ctx, request)
}

func TestStandardAuthoringWorkspaceSubmissionReturnsFeedbackThenFreezesHostFiles(t *testing.T) {
	now := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	policy := standardAuthoringCodexTestEnvironmentPolicy(t)
	taskRoot := standardAuthoringWorkspaceSubmissionTaskRoot(t, policy, authoringharness.ModeDockerfileBuild)
	stage := standardAuthoringWorkspaceSubmissionStage(workflowadapter.DockerfileBuildValidate)
	request, _, usages := standardAuthoringCodexTestRequest(stage, []byte("input"), now)

	calls := 0
	validator := standardAuthoringHarnessValidatorFunc(func(_ context.Context, validationRequest authoringharness.Request) (authoringharness.Result, error) {
		calls++
		candidate, err := authoringharness.ReadCandidate(taskRoot, authoringharness.ModeDockerfileBuild)
		if err != nil {
			return authoringharness.Result{}, err
		}
		passed := calls > 1
		findings := []string{"controlled Docker build failed"}
		if passed {
			findings = []string{}
		}
		return authoringharness.Finalize(authoringharness.Result{
			Mode: validationRequest.Mode, RunID: validationRequest.RunID, StageKey: validationRequest.StageKey, StageAttemptID: validationRequest.StageAttemptID,
			Passed: passed, Step: "docker_build", ExitCode: map[bool]int{true: 0, false: 1}[passed], Findings: findings,
			StderrTail: "bounded build output", CandidateDigest: candidate.CandidateDigest, EnvironmentDigest: candidate.EnvironmentDigest,
			Steps: []authoringharness.StepResult{{
				Step: "docker_build", Passed: passed, ExitCode: map[bool]int{true: 0, false: 1}[passed], Findings: findings,
				StderrTail: "bounded build output", OutputFingerprint: workflowkit.SHA256Fingerprint([]byte("docker-output")),
			}},
		})
	})
	submission, err := newStandardAuthoringCodexWorkspaceSubmission(request, taskRoot, 8, func() time.Time { return now }, validator, &policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := submission.beginTurn(1); err != nil {
		t.Fatal(err)
	}

	firstRaw := json.RawMessage(`{"verdict":"pass"}`)
	first, err := submission.handle(context.Background(), firstRaw)
	if err != nil {
		t.Fatal(err)
	}
	var firstReceipt standardAuthoringCodexWorkspaceSubmissionReceipt
	if err := json.Unmarshal(first, &firstReceipt); err != nil {
		t.Fatal(err)
	}
	if firstReceipt.Accepted || firstReceipt.Step != "docker_build" || firstReceipt.ExitCode != 1 || firstReceipt.StderrTail != "bounded build output" || len(firstReceipt.Errors) != 1 {
		t.Fatalf("first validation receipt = %+v", firstReceipt)
	}

	dockerfile := filepath.Join(taskRoot, filepath.FromSlash(authoringharness.DockerfileRelativePath))
	updated := []byte("FROM " + policy.BaseImage + "\nRUN printf '%s\\n' ready\n")
	if err := os.WriteFile(dockerfile, updated, 0o640); err != nil {
		t.Fatal(err)
	}
	second, err := submission.handle(context.Background(), firstRaw)
	if err != nil {
		t.Fatal(err)
	}
	var secondReceipt standardAuthoringCodexWorkspaceSubmissionReceipt
	if err := json.Unmarshal(second, &secondReceipt); err != nil {
		t.Fatal(err)
	}
	if !secondReceipt.Accepted || len(secondReceipt.Errors) != 0 || calls != 2 {
		t.Fatalf("second validation receipt = %+v calls=%d", secondReceipt, calls)
	}
	accepted, ok := submission.acceptedResult()
	if !ok || len(accepted.Artifacts) != 2 || string(accepted.Artifacts[0].Content) != string(updated) {
		t.Fatalf("accepted workspace result = %+v", accepted)
	}
	if accepted.Artifacts[0].Name != workflowadapter.StandardAuthoringValidatedDockerfileArtifact || accepted.Artifacts[1].Name != workflowadapter.StandardAuthoringDockerfileBuildReportArtifact {
		t.Fatalf("accepted workspace artifacts = %+v", accepted.Artifacts)
	}
	if len(*usages) != 4 {
		t.Fatalf("workspace usage = %+v, want two charges per validation", *usages)
	}
	for index := 0; index < len(*usages); index += 2 {
		if (*usages)[index].Dimension != standardAuthoringCodexOutputSubmissionQuotaDimension || (*usages)[index+1].Dimension != workflowadapter.StandardAuthoringValidationQuotaDimension {
			t.Fatalf("workspace usage dimensions = %+v", *usages)
		}
	}
}

func TestStandardAuthoringWorkspaceSubmissionRejectsDockerfileDriftAndTOCTOU(t *testing.T) {
	now := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	policy := standardAuthoringCodexTestEnvironmentPolicy(t)
	taskRoot := standardAuthoringWorkspaceSubmissionTaskRoot(t, policy, authoringharness.ModeInitialOracle)
	stage := standardAuthoringWorkspaceSubmissionStage(workflowadapter.AuthoringHarness)
	request, _, _ := standardAuthoringCodexTestRequest(stage, []byte("input"), now)
	validatorCalls := 0
	validator := standardAuthoringHarnessValidatorFunc(func(_ context.Context, validationRequest authoringharness.Request) (authoringharness.Result, error) {
		validatorCalls++
		candidate, err := authoringharness.ReadCandidate(taskRoot, authoringharness.ModeInitialOracle)
		if err != nil {
			return authoringharness.Result{}, err
		}
		result, err := authoringharness.Finalize(authoringharness.Result{
			Mode: validationRequest.Mode, RunID: validationRequest.RunID, StageKey: validationRequest.StageKey, StageAttemptID: validationRequest.StageAttemptID,
			Passed: true, Step: "oracle_verify", ExitCode: 0, Findings: []string{}, CandidateDigest: candidate.CandidateDigest, EnvironmentDigest: candidate.EnvironmentDigest,
			Steps: []authoringharness.StepResult{{Step: "oracle_verify", Passed: true, ExitCode: 0, Findings: []string{}, OutputFingerprint: workflowkit.SHA256Fingerprint([]byte("oracle-output"))}},
		})
		if err != nil {
			return authoringharness.Result{}, err
		}
		if err := os.WriteFile(filepath.Join(taskRoot, filepath.FromSlash(authoringharness.SolveScriptRelativePath)), []byte("#!/bin/sh\necho changed\n"), 0o750); err != nil {
			return authoringharness.Result{}, err
		}
		return result, nil
	})
	submission, err := newStandardAuthoringCodexWorkspaceSubmission(request, taskRoot, 8, func() time.Time { return now }, validator, &policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := submission.beginTurn(1); err != nil {
		t.Fatal(err)
	}

	dockerfile := filepath.Join(taskRoot, filepath.FromSlash(authoringharness.DockerfileRelativePath))
	if err := os.WriteFile(dockerfile, []byte("FROM "+policy.BaseImage+"\nRUN true\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	drift, err := submission.handle(context.Background(), json.RawMessage(`{"verdict":"pass"}`))
	if err != nil {
		t.Fatal(err)
	}
	var driftReceipt standardAuthoringCodexWorkspaceSubmissionReceipt
	if err := json.Unmarshal(drift, &driftReceipt); err != nil {
		t.Fatal(err)
	}
	if driftReceipt.Accepted || len(driftReceipt.Errors) != 1 || driftReceipt.Errors[0] != "validated_dockerfile_changed" || validatorCalls != 0 {
		t.Fatalf("Dockerfile drift receipt = %+v calls=%d", driftReceipt, validatorCalls)
	}

	// Restore the frozen environment, then let the validator race a script edit.
	if err := os.WriteFile(dockerfile, []byte("FROM "+policy.BaseImage+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	toctou, err := submission.handle(context.Background(), json.RawMessage(`{"verdict":"pass"}`))
	if err != nil {
		t.Fatal(err)
	}
	var toctouReceipt standardAuthoringCodexWorkspaceSubmissionReceipt
	if err := json.Unmarshal(toctou, &toctouReceipt); err != nil {
		t.Fatal(err)
	}
	if toctouReceipt.Accepted || len(toctouReceipt.Errors) != 1 || toctouReceipt.Errors[0] != "candidate_changed_after_validation" || validatorCalls != 1 {
		t.Fatalf("TOCTOU receipt = %+v calls=%d", toctouReceipt, validatorCalls)
	}
	if _, accepted := submission.acceptedResult(); accepted {
		t.Fatal("raced candidate became accepted")
	}
}

func standardAuthoringWorkspaceSubmissionStage(stageKey string) workflowkit.StageDescriptor {
	stage := standardAuthoringCodexTestStage(1)
	stage.Key = workflowkit.StageKey(stageKey)
	stage.Plugin = workflowkit.PluginBinding{ID: "harborfactory." + stageKey, Version: "1"}
	stage.Verdicts = workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass}}
	stage.QuotaClaims = []workflowkit.QuotaClaim{
		{Dimension: "agent_turn", Units: 1, ReclaimPolicy: workflowkit.ReclaimUnused},
		{Dimension: standardAuthoringCodexOutputSubmissionQuotaDimension, Units: workflowadapter.StandardAuthoringHarnessSubmissionClaimUnits, ReclaimPolicy: workflowkit.ReclaimUnused},
		{Dimension: workflowadapter.StandardAuthoringValidationQuotaDimension, Units: workflowadapter.StandardAuthoringValidationClaimUnits, ReclaimPolicy: workflowkit.ReclaimUnused},
	}
	if stageKey == workflowadapter.DockerfileBuildValidate {
		stage.Outputs = []workflowkit.ArtifactSpec{
			{Name: workflowadapter.StandardAuthoringValidatedDockerfileArtifact, SchemaVersion: "harbor.artifact.v1", Required: true},
			{Name: workflowadapter.StandardAuthoringDockerfileBuildReportArtifact, SchemaVersion: workflowadapter.StandardAuthoringDockerfileBuildReportSchemaVersion, Required: true},
		}
	} else {
		stage.Outputs = []workflowkit.ArtifactSpec{
			{Name: workflowadapter.StandardAuthoringValidatedSolveScriptArtifact, SchemaVersion: "harbor.artifact.v1", Required: true},
			{Name: workflowadapter.StandardAuthoringValidatedTestScriptArtifact, SchemaVersion: "harbor.artifact.v1", Required: true},
			{Name: workflowadapter.StandardAuthoringHarnessReportArtifact, SchemaVersion: workflowadapter.StandardAuthoringHarnessReportSchemaVersion, Required: true},
		}
	}
	return stage
}

func standardAuthoringWorkspaceSubmissionTaskRoot(t *testing.T, policy workflowadapter.StandardAuthoringEnvironmentPolicy, mode authoringharness.Mode) string {
	t.Helper()
	taskRoot := filepath.Join(t.TempDir(), "task")
	if err := os.Mkdir(taskRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{authoringharness.DockerfileRelativePath: "FROM " + policy.BaseImage + "\n"}
	if mode == authoringharness.ModeInitialOracle {
		files[authoringharness.SolveScriptRelativePath] = "#!/bin/sh\nexit 0\n"
		files[authoringharness.TestScriptRelativePath] = "#!/bin/sh\nexit 1\n"
	}
	for relative, content := range files {
		path := filepath.Join(taskRoot, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o640)
		if strings.HasSuffix(relative, ".sh") {
			mode = 0o750
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return taskRoot
}
