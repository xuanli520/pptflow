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

func TestStandardAuthoringV3SubmissionUsesRawTypedContent(t *testing.T) {
	stage, found := workflowadapter.StandardAuthoringContractStageCatalog().Stage(workflowkit.StageKey(workflowadapter.RepoStructureResearch))
	if !found || stage.AgentRole == nil {
		t.Fatal("3.0 research stage is unavailable")
	}
	submission := newStandardAuthoringV3Submission(standardAuthoringV3TestDescriptor(stage), workflowkit.AgentRoleResearcher, "", 1024)
	response, err := submission.handle(context.Background(), json.RawMessage(`{"verdict":"pass","artifacts":[{"name":"repo_structure_evidence","content":"src/lib.rs:12"}]}`))
	if err != nil || string(response) != `{"accepted":true}` {
		t.Fatalf("raw structured submission = %s, %v", response, err)
	}
	result, accepted := submission.acceptedResult()
	if !accepted || len(result.Artifacts) != 1 || string(result.Artifacts[0].Content) != "src/lib.rs:12" {
		t.Fatalf("raw structured result = %+v accepted=%t", result, accepted)
	}

	rejected := newStandardAuthoringV3Submission(standardAuthoringV3TestDescriptor(stage), workflowkit.AgentRoleResearcher, "", 1024)
	response, err = rejected.handle(context.Background(), json.RawMessage(`{"verdict":"pass","artifacts":[{"name":"repo_structure_evidence","content_base64":"c3JjL2xpYi5yczoxMg=="}]}`))
	if err != nil || string(response) != `{"accepted":false,"reason":"invalid_payload"}` {
		t.Fatalf("base64 protocol was accepted: %s, %v", response, err)
	}
}

func TestStandardAuthoringV3AuthorCapturesOnlyFixedWorkspaceFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"instruction.md":                         "# Task\n",
		"task.toml":                              "name = \"task\"\n",
		authoringharness.DockerfileRelativePath:  "FROM alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n",
		authoringharness.SolveScriptRelativePath: "#!/bin/sh\ntrue\n",
		authoringharness.TestScriptRelativePath:  "#!/bin/sh\nfalse\n",
		"tests_analysis.json":                    `{"format":"harbor.tests-analysis.v1"}`,
	}
	for path, content := range files {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stage, found := workflowadapter.StandardAuthoringContractStageCatalog().Stage(workflowkit.StageKey(workflowadapter.AuthoringLoop))
	if !found || stage.AgentRole == nil {
		t.Fatal("3.0 author stage is unavailable")
	}
	submission := newStandardAuthoringV3Submission(standardAuthoringV3TestDescriptor(stage), workflowkit.AgentRoleAuthor, root, 64<<10)
	response, err := submission.handle(context.Background(), json.RawMessage(`{"verdict":"pass"}`))
	if err != nil || string(response) == "" {
		t.Fatalf("capture candidate = %s, %v", response, err)
	}
	result, accepted := submission.acceptedResult()
	if !accepted || len(result.Artifacts) != 7 || result.Artifacts[6].Name != "candidate_snapshot" {
		t.Fatalf("captured candidate artifacts = %+v accepted=%t", result.Artifacts, accepted)
	}
	var snapshot workflowkit.CandidateSnapshot
	if err := json.Unmarshal(result.Artifacts[6].Content, &snapshot); err != nil || snapshot.Validate() != nil || len(snapshot.Files) != 6 {
		t.Fatalf("captured snapshot is invalid: %+v, %v", snapshot, err)
	}

	withArtifacts := newStandardAuthoringV3Submission(standardAuthoringV3TestDescriptor(stage), workflowkit.AgentRoleAuthor, root, 64<<10)
	response, err = withArtifacts.handle(context.Background(), json.RawMessage(`{"verdict":"pass","artifacts":[{"name":"instruction","content":"not authoritative"}]}`))
	if err != nil || string(response) != `{"accepted":false,"reason":"candidate_tool_does_not_accept_artifacts"}` {
		t.Fatalf("author tool accepted direct artifacts: %s, %v", response, err)
	}
}

func TestStandardAuthoringV3RepairSubmissionRequiresPassingFreshValidationReceipt(t *testing.T) {
	root := standardAuthoringV3CandidateWorkspace(t)
	stage, found := workflowadapter.StandardAuthoringContractStageCatalog().Stage(workflowkit.StageKey(workflowadapter.AuthoringRepair))
	if !found || stage.AgentRole == nil {
		t.Fatal("3.0 repair stage is unavailable")
	}
	attempts := 0
	submission := newStandardAuthoringV3Submission(standardAuthoringV3TestDescriptor(stage), workflowkit.AgentRoleAuthor, root, 64<<10)
	submission.maxValidationAttempts = workflowadapter.StandardAuthoringRepairMaxTurns
	finding, err := workflowkit.NewWorkflowFinding(workflowkit.WorkflowFinding{
		Code: "test_quality_defect", ProducingStage: workflowkit.StageKey(workflowadapter.TestQualityCritic), TargetWriter: workflowkit.StageKey(workflowadapter.AuthoringRepair),
		EvidenceDigest: workflowkit.SHA256Fingerprint([]byte("evidence")), CandidateDigest: workflowkit.SHA256Fingerprint([]byte("candidate")), DiagnosticDigest: workflowkit.SHA256Fingerprint([]byte("diagnostic")),
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := workflowkit.NewWorkflowRepairLedger([]workflowkit.WorkflowRepairLedgerEntry{{Finding: finding, ConsumedCandidateRound: true}})
	if err != nil {
		t.Fatal(err)
	}
	ledgerJSON, err := json.Marshal(ledger)
	if err != nil {
		t.Fatal(err)
	}
	submission.repairLedger = func() ([]byte, error) { return ledgerJSON, nil }
	submission.candidateValidator = func(_ context.Context, snapshot workflowkit.CandidateSnapshot, _ map[string][]byte) (workflowkit.ValidationReceipt, error) {
		attempts++
		verdict := workflowkit.ValidationReject
		if attempts == 2 {
			verdict = workflowkit.ValidationPass
		}
		failureCode := workflowkit.AgentFailureValidatorReject
		if verdict == workflowkit.ValidationPass {
			failureCode = ""
		}
		contract, err := workflowkit.NewCandidateValidationContract(workflowkit.SHA256Fingerprint([]byte("runtime")), workflowkit.SHA256Fingerprint([]byte("verification")))
		if err != nil {
			return workflowkit.ValidationReceipt{}, err
		}
		contractDigest, err := contract.Fingerprint()
		if err != nil {
			return workflowkit.ValidationReceipt{}, err
		}
		now := time.Now().UTC()
		return workflowkit.NewValidationReceipt(workflowkit.ValidationReceipt{
			SnapshotDigest: snapshot.Digest, ContractDigest: contractDigest, Verdict: verdict,
			FailureCode: failureCode,
			Diagnostics: []workflowkit.AgentCommandReport{{CommandID: "baseline_verify", ExitCode: 1, TestStarted: true, StderrTail: "redacted"}},
			IssuedAt:    now, ExpiresAt: now.Add(time.Minute),
		})
	}

	first, err := submission.handle(context.Background(), json.RawMessage(`{"verdict":"pass"}`))
	if err != nil || !strings.Contains(string(first), `"reason":"candidate_rejected"`) || strings.Contains(string(first), "redacted") {
		t.Fatalf("rejected repair validation response = %s, %v", first, err)
	}
	if _, accepted := submission.acceptedResult(); accepted {
		t.Fatal("rejected repair candidate was accepted")
	}
	second, err := submission.handle(context.Background(), json.RawMessage(`{"verdict":"pass"}`))
	if err != nil || !strings.Contains(string(second), `"accepted":true`) {
		t.Fatalf("passing repair validation response = %s, %v", second, err)
	}
	result, accepted := submission.acceptedResult()
	if !accepted || attempts != 2 || len(result.Artifacts) != 9 || result.Artifacts[7].Name != "validation_receipt" || result.Artifacts[8].Name != "workflow_repair_ledger" {
		t.Fatalf("repair result = %+v accepted=%t attempts=%d", result, accepted, attempts)
	}
	var receipt workflowkit.ValidationReceipt
	if err := json.Unmarshal(result.Artifacts[7].Content, &receipt); err != nil || receipt.Verdict != workflowkit.ValidationPass {
		t.Fatalf("repair receipt = %+v, %v", receipt, err)
	}
}

func standardAuthoringV3CandidateWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for path, content := range map[string]string{
		"instruction.md": "# Task\n", "task.toml": "name = \"task\"\n", authoringharness.DockerfileRelativePath: "FROM scratch\n",
		authoringharness.SolveScriptRelativePath: "#!/bin/sh\ntrue\n", authoringharness.TestScriptRelativePath: "#!/bin/sh\nfalse\n", "tests_analysis.json": "{}\n",
	} {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func standardAuthoringV3TestDescriptor(stage workflowadapter.StageDefinition) workflowkit.StageDescriptor {
	return workflowkit.StageDescriptor{Key: stage.Key, Outputs: append([]workflowkit.ArtifactSpec(nil), stage.Outputs...), AgentRole: stage.AgentRole}
}

func TestStandardAuthoringV3AgentSchemaRejectsBase64AndNonAgentStages(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentTemplateReference()
	if err := ValidateStandardAuthoringV3AgentOutputSchemaAsset(template, workflowkit.StageKey(workflowadapter.RepoStructureResearch), []byte(standardAuthoringV3AgentOutputSchemaCanonicalJSON)); err != nil {
		t.Fatalf("validate V3 schema: %v", err)
	}
	legacy := []byte(`{"$id":"legacy","properties":{"content_base64":{"type":"string"}}}`)
	if err := ValidateStandardAuthoringV3AgentOutputSchemaAsset(template, workflowkit.StageKey(workflowadapter.RepoStructureResearch), legacy); err == nil {
		t.Fatal("accepted a Base64 schema")
	}
	if err := ValidateStandardAuthoringV3AgentOutputSchemaAsset(template, workflowkit.StageKey(workflowadapter.HostCandidateVerify), []byte(standardAuthoringV3AgentOutputSchemaCanonicalJSON)); err == nil {
		t.Fatal("accepted the Agent schema for a host-owned stage")
	}
}

func TestStandardAuthoringCodexSandboxMatchesWorkspaceCapability(t *testing.T) {
	for _, test := range []struct {
		name       string
		mode       workflowkit.WorkspaceMode
		wantMode   string
		wantPolicy string
	}{
		{name: "no workspace", mode: workflowkit.WorkspaceNone, wantMode: CodexAppServerSandboxModeReadOnly, wantPolicy: CodexAppServerSandboxPolicyReadOnly},
		{name: "read-only snapshot", mode: workflowkit.WorkspaceReadOnlySnapshot, wantMode: CodexAppServerSandboxModeReadOnly, wantPolicy: CodexAppServerSandboxPolicyReadOnly},
		{name: "exclusive writer", mode: workflowkit.WorkspaceExclusiveWriter, wantMode: CodexAppServerSandboxModeWorkspaceWrite, wantPolicy: CodexAppServerSandboxPolicyWorkspaceWrite},
	} {
		t.Run(test.name, func(t *testing.T) {
			mode, policy, err := StandardAuthoringCodexSandboxForWorkspace(test.mode)
			if err != nil || mode != test.wantMode || policy != test.wantPolicy {
				t.Fatalf("sandbox for %q = %q/%q, %v; want %q/%q", test.mode, mode, policy, err, test.wantMode, test.wantPolicy)
			}
		})
	}
	if _, _, err := StandardAuthoringCodexSandboxForWorkspace("unsupported"); err == nil {
		t.Fatal("unsupported workspace mode received a Codex sandbox")
	}
}
