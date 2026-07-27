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

func TestStandardAuthoringV3CriticSubmissionRejectsUnknownFindingFields(t *testing.T) {
	stage, found := workflowadapter.StandardAuthoringContractStageCatalog().Stage(workflowkit.StageKey(workflowadapter.TestQualityCritic))
	if !found || stage.AgentRole == nil {
		t.Fatal("3.0 test quality critic stage is unavailable")
	}
	finding, err := workflowkit.NewWorkflowFinding(workflowkit.WorkflowFinding{
		Code:             "test_quality_defect",
		ProducingStage:   workflowkit.StageKey(workflowadapter.TestQualityCritic),
		TargetWriter:     workflowkit.StageKey(workflowadapter.AuthoringRepair),
		EvidenceDigest:   workflowkit.SHA256Fingerprint([]byte("evidence")),
		CandidateDigest:  workflowkit.SHA256Fingerprint([]byte("candidate")),
		DiagnosticDigest: workflowkit.SHA256Fingerprint([]byte("diagnostic")),
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(finding)
	if err != nil {
		t.Fatal(err)
	}
	withUnknownField := append(append([]byte(nil), canonical[:len(canonical)-1]...), []byte(`,"finding":{"severity":"critical"}}`)...)
	invalidPayload, err := json.Marshal(map[string]any{
		"verdict":   "pass",
		"artifacts": []map[string]string{{"name": "test_quality_finding", "content": string(withUnknownField)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rejected := newStandardAuthoringV3Submission(standardAuthoringV3TestDescriptor(stage), workflowkit.AgentRoleCritic, "", 64<<10)
	response, err := rejected.handle(context.Background(), invalidPayload)
	if err != nil || string(response) != `{"accepted":false,"reason":"structured_output_invalid"}` {
		t.Fatalf("critic accepted finding with unknown field: %s, %v", response, err)
	}
	if _, accepted := rejected.acceptedResult(); accepted {
		t.Fatal("unknown finding field produced a durable output")
	}

	validPayload, err := json.Marshal(map[string]any{
		"verdict":   "pass",
		"artifacts": []map[string]string{{"name": "test_quality_finding", "content": string(canonical)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted := newStandardAuthoringV3Submission(standardAuthoringV3TestDescriptor(stage), workflowkit.AgentRoleCritic, "", 64<<10)
	response, err = accepted.handle(context.Background(), validPayload)
	if err != nil || string(response) != `{"accepted":true}` {
		t.Fatalf("canonical critic finding was rejected: %s, %v", response, err)
	}
}

func TestStandardAuthoringV3TaskSynthesisRejectsNonCanonicalVerificationContract(t *testing.T) {
	stage, found := workflowadapter.StandardAuthoringContractStageCatalog().Stage(workflowkit.StageKey(workflowadapter.TaskSynthesis))
	if !found || stage.AgentRole == nil {
		t.Fatal("3.0 task synthesis stage is unavailable")
	}
	submission := func(verificationContract string) *standardAuthoringV3Submission {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"verdict": "pass",
			"artifacts": []map[string]string{
				{"name": "task_specification", "content": "Yew Router task specification"},
				{"name": "verification_contract", "content": verificationContract},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		candidate := newStandardAuthoringV3Submission(standardAuthoringV3TestDescriptor(stage), workflowkit.AgentRoleSynthesizer, "", 64<<10)
		response, err := candidate.handle(context.Background(), payload)
		if err != nil {
			t.Fatal(err)
		}
		if string(response) != `{"accepted":false,"reason":"structured_output_invalid"}` {
			t.Fatalf("verification contract response = %s", response)
		}
		return candidate
	}

	if _, accepted := submission(`{"schema_version":"harbor.verification-contract.v1"}`).acceptedResult(); accepted {
		t.Fatal("legacy task-level verification object produced a durable output")
	}

	canonical := `{"format":"harbor.verification-contract.v1","version":"1","command":["sh","/oracle/tests/test.sh","wasm32-unknown-unknown"],"workdir":".","coverage_mode":"browser_wasm","allowed_solution_paths":["packages/yew-router/src/router.rs"]}`
	payload, err := json.Marshal(map[string]any{
		"verdict": "pass",
		"artifacts": []map[string]string{
			{"name": "task_specification", "content": "Yew Router task specification"},
			{"name": "verification_contract", "content": canonical},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	acceptedSubmission := newStandardAuthoringV3Submission(standardAuthoringV3TestDescriptor(stage), workflowkit.AgentRoleSynthesizer, "", 64<<10)
	response, err := acceptedSubmission.handle(context.Background(), payload)
	if err != nil || string(response) != `{"accepted":true}` {
		t.Fatalf("canonical verification contract was rejected: %s, %v", response, err)
	}
}

func TestStandardAuthoringV3ContextDocumentDeclaresTerminalSubmission(t *testing.T) {
	researchStage, found := workflowadapter.StandardAuthoringContractStageCatalog().Stage(workflowkit.StageKey(workflowadapter.RepoStructureResearch))
	if !found || researchStage.AgentRole == nil {
		t.Fatal("3.0 research stage is unavailable")
	}
	researchContext, err := standardAuthoringV3ContextDocument(
		workflowkit.StageExecutionRequest{Stage: standardAuthoringV3TestDescriptor(researchStage)},
		StandardAuthoringCodexTurnProgram{Fingerprint: workflowkit.SHA256Fingerprint([]byte("research-program"))},
		workflowkit.SHA256Fingerprint([]byte("research-inputs")), map[string][]byte{"authoring_contract": []byte(`{"format":"test"}`)}, false,
	)
	if err != nil {
		t.Fatalf("research context: %v", err)
	}
	var researchDocument struct {
		TerminalSubmission standardAuthoringV3TerminalSubmission `json:"terminal_submission"`
	}
	if err := json.Unmarshal(researchContext, &researchDocument); err != nil {
		t.Fatalf("decode research context: %v", err)
	}
	if got := researchDocument.TerminalSubmission; got.Tool != standardAuthoringV3SubmitOutputTool || got.Mode != "structured_artifacts" || !got.Required || len(got.RequiredOutputs) != 1 || got.RequiredOutputs[0].Name != "repo_structure_evidence" {
		t.Fatalf("research terminal submission = %+v", got)
	}

	authorStage, found := workflowadapter.StandardAuthoringContractStageCatalog().Stage(workflowkit.StageKey(workflowadapter.AuthoringLoop))
	if !found || authorStage.AgentRole == nil {
		t.Fatal("3.0 author stage is unavailable")
	}
	authorContext, err := standardAuthoringV3ContextDocument(
		workflowkit.StageExecutionRequest{Stage: standardAuthoringV3TestDescriptor(authorStage)},
		StandardAuthoringCodexTurnProgram{Fingerprint: workflowkit.SHA256Fingerprint([]byte("author-program"))},
		workflowkit.SHA256Fingerprint([]byte("author-inputs")), nil, true,
	)
	if err != nil {
		t.Fatalf("author context: %v", err)
	}
	var authorDocument struct {
		TerminalSubmission standardAuthoringV3TerminalSubmission `json:"terminal_submission"`
	}
	if err := json.Unmarshal(authorContext, &authorDocument); err != nil {
		t.Fatalf("decode author context: %v", err)
	}
	if got := authorDocument.TerminalSubmission; got.Tool != standardAuthoringV3ValidateTool || got.Mode != "candidate_workspace" || got.CandidateDirectory != StandardAuthoringCodexAttemptTaskDirectory || len(got.RequiredOutputs) != 0 || len(got.CandidateFiles) != 6 {
		t.Fatalf("author terminal submission = %+v", got)
	}
}

func TestStandardAuthoringV3ContextDocumentProjectsCandidateValidationIdentity(t *testing.T) {
	snapshot, err := workflowkit.NewCandidateSnapshot([]workflowkit.CandidateFile{{
		Path: "instruction.md", SchemaVersion: "harbor.artifact.v1", ContentDigest: workflowkit.SHA256Fingerprint([]byte("candidate")), SizeBytes: int64(len("candidate")),
	}})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := workflowkit.NewCandidateValidationContract(workflowkit.SHA256Fingerprint([]byte("runtime")), workflowkit.SHA256Fingerprint([]byte("verification")))
	if err != nil {
		t.Fatal(err)
	}
	contractDigest, err := contract.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	receipt, err := workflowkit.NewValidationReceipt(workflowkit.ValidationReceipt{
		SnapshotDigest: snapshot.Digest, ContractDigest: contractDigest, Verdict: workflowkit.ValidationReject, FailureCode: workflowkit.AgentFailureValidatorReject,
		Diagnostics: []workflowkit.AgentCommandReport{{CommandID: "environment_build", ExitCode: 1, TestStarted: false}}, IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshotRaw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	receiptRaw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	contextDocument, err := standardAuthoringV3ContextDocument(
		workflowkit.StageExecutionRequest{Stage: workflowkit.StageDescriptor{Key: workflowkit.StageKey(workflowadapter.TestQualityCritic)}},
		StandardAuthoringCodexTurnProgram{Fingerprint: workflowkit.SHA256Fingerprint([]byte("critic-program"))}, workflowkit.SHA256Fingerprint([]byte("critic-inputs")),
		map[string][]byte{"candidate_snapshot": snapshotRaw, "validation_receipt": receiptRaw}, false,
	)
	if err != nil {
		t.Fatalf("critic context: %v", err)
	}
	var document struct {
		CandidateValidationIdentity *standardAuthoringV3CandidateValidationIdentity `json:"candidate_validation_identity"`
	}
	if err := json.Unmarshal(contextDocument, &document); err != nil {
		t.Fatalf("decode critic context: %v", err)
	}
	if document.CandidateValidationIdentity == nil || document.CandidateValidationIdentity.CandidateSnapshotDigest != snapshot.Digest || document.CandidateValidationIdentity.ValidationReceiptDigest != receipt.Digest {
		t.Fatalf("candidate validation identity = %+v, want snapshot=%s receipt=%s", document.CandidateValidationIdentity, snapshot.Digest, receipt.Digest)
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

func TestStandardAuthoringV3RepairLedgerAcceptsRejectedHostReceipt(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	resolved, err := template.Compile(standardAuthoringTestExecutionProfile(t))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := workflowkit.NewCandidateSnapshot([]workflowkit.CandidateFile{{
		Path: "candidate.txt", SchemaVersion: "harbor.artifact.v1", ContentDigest: workflowkit.SHA256Fingerprint([]byte("candidate")), SizeBytes: int64(len("candidate")),
	}})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := workflowkit.NewCandidateValidationContract(workflowkit.SHA256Fingerprint([]byte("runtime")), workflowkit.SHA256Fingerprint([]byte("verification")))
	if err != nil {
		t.Fatal(err)
	}
	contractDigest, err := contract.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	receipt, err := workflowkit.NewValidationReceipt(workflowkit.ValidationReceipt{
		SnapshotDigest: snapshot.Digest, ContractDigest: contractDigest, Verdict: workflowkit.ValidationReject, FailureCode: workflowkit.AgentFailureValidatorReject,
		IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshotRaw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	receiptRaw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string][]byte{"candidate_snapshot": snapshotRaw, "validation_receipt": receiptRaw}
	rules := []workflowkit.WorkflowRepairRule{
		{FindingCode: "test_quality_defect", ProducingStage: workflowkit.StageKey(workflowadapter.TestQualityCritic), TargetWriter: workflowkit.StageKey(workflowadapter.AuthoringRepair), RequiresCandidateSnapshot: true, ConsumesCandidateRepair: true},
		{FindingCode: "solution_integrity_defect", ProducingStage: workflowkit.StageKey(workflowadapter.SolutionIntegrityCritic), TargetWriter: workflowkit.StageKey(workflowadapter.AuthoringRepair), RequiresCandidateSnapshot: true, ConsumesCandidateRepair: true},
	}
	for name, findingSpec := range map[string]struct {
		code  string
		stage workflowkit.StageKey
	}{
		"test_quality_finding":       {code: "test_quality_defect", stage: workflowkit.StageKey(workflowadapter.TestQualityCritic)},
		"solution_integrity_finding": {code: "solution_integrity_defect", stage: workflowkit.StageKey(workflowadapter.SolutionIntegrityCritic)},
	} {
		finding, err := workflowkit.NewWorkflowFinding(workflowkit.WorkflowFinding{
			Code: findingSpec.code, ProducingStage: findingSpec.stage, TargetWriter: workflowkit.StageKey(workflowadapter.AuthoringRepair),
			EvidenceDigest: receipt.Digest, CandidateDigest: snapshot.Digest, DiagnosticDigest: receipt.Digest,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := workflowkit.PlanWorkflowRepair(resolved.Descriptor, finding, rules, nil); err != nil {
			t.Fatalf("frozen workflow rejects a repair finding: %v", err)
		}
		findingRaw, err := json.Marshal(finding)
		if err != nil {
			t.Fatal(err)
		}
		inputs[name] = findingRaw
	}
	ledgerRaw, err := standardAuthoringV3RepairLedger(resolved.Descriptor, inputs)
	if err != nil {
		t.Fatalf("repair ledger rejected a host validation rejection: %v", err)
	}
	var ledger workflowkit.WorkflowRepairLedger
	if err := json.Unmarshal(ledgerRaw, &ledger); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(ledger.Entries) != 2 || (ledger.Entries[0].ConsumedCandidateRound == ledger.Entries[1].ConsumedCandidateRound) {
		t.Fatalf("repair ledger = %+v, want two findings with exactly one candidate repair charge", ledger)
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
