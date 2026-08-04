package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func standardAuthoringV3CandidateFiles(instruction, taskTOML, dockerfile, solveScript, testScript, testsAnalysis []byte) map[string][]byte {
	return map[string][]byte{
		standardAuthoringV3InstructionPath: instruction, standardAuthoringV3TaskTOMLPath: taskTOML,
		standardAuthoringV3DockerfilePath: dockerfile, standardAuthoringV3SolveScriptPath: solveScript,
		standardAuthoringV3TestScriptPath: testScript, standardAuthoringV3TestsAnalysisPath: testsAnalysis,
	}
}

// validateStandardAuthoringV3MaterializationEvidence repeats the last trust
// boundary before package admission and materialization. A passed package
// report is insufficient when its candidate or final review evidence names a
// different snapshot.
func validateStandardAuthoringV3MaterializationEvidence(snapshotRaw, receiptRaw, attestationRaw []byte, files map[string][]byte) error {
	var snapshot workflowkit.CandidateSnapshot
	if err := decodeStrictJSON(string(snapshotRaw), &snapshot); err != nil || snapshot.Validate() != nil {
		return fmt.Errorf("Standard authoring V3 candidate snapshot is invalid")
	}
	if err := validateStandardAuthoringV3CandidateFiles(snapshot, files); err != nil {
		return err
	}
	// The receipt is admitted by structural and digest binding, not by wall
	// clock freshness: materialization is an operator-paced flow that can
	// legitimately resume hours after HostCandidateVerify, and every stage
	// artifact below still proves the receipt names this exact candidate.
	var receipt workflowkit.ValidationReceipt
	if err := decodeStrictJSON(string(receiptRaw), &receipt); err != nil || receipt.Validate() != nil || receipt.Verdict != workflowkit.ValidationPass || receipt.SnapshotDigest != snapshot.Digest {
		return fmt.Errorf("Standard authoring V3 validation receipt is not a passing receipt for the candidate")
	}
	var attestation standardAuthoringFinalAttestation
	if err := decodeStrictJSON(string(attestationRaw), &attestation); err != nil ||
		attestation.Format != "harbor.standard-authoring-final-attestation.v1" || attestation.Version != "1" ||
		attestation.SnapshotDigest != snapshot.Digest || attestation.ValidationReceiptDigest != receipt.Digest ||
		attestation.ContentReviewDigest == "" || attestation.SolutionReviewDigest == "" {
		return fmt.Errorf("Standard authoring V3 final attestation does not bind the candidate receipt")
	}
	return nil
}

// standardAuthoringFinalAttestation is intentionally host-created. It binds
// the candidate validation result and the two durable review decisions without
// preserving their unbounded free-text rationale.
type standardAuthoringFinalAttestation struct {
	Format                  string                  `json:"format"`
	Version                 string                  `json:"version"`
	SnapshotDigest          workflowkit.Fingerprint `json:"snapshot_digest"`
	ValidationReceiptDigest workflowkit.Fingerprint `json:"validation_receipt_digest"`
	ContentReviewDigest     workflowkit.Fingerprint `json:"content_review_digest"`
	SolutionReviewDigest    workflowkit.Fingerprint `json:"solution_review_digest"`
}

func (executor *StandardAuthoringMaterializeExecutor) executeHostCandidateVerify(ctx context.Context, invocation stageprovider.StageOperationInvocation) (workflowkit.StageExecutionResult, error) {
	if executor.candidateHarness == nil || invocation.Resolution.StageKey != workflowkit.StageKey(workflowadapter.HostCandidateVerify) || invocation.Request.Stage.Key != workflowkit.StageKey(workflowadapter.HostCandidateVerify) {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring host candidate verifier received an unbound operation")
	}
	inputs, err := standardAuthoringV3BuiltinInputs(ctx, invocation.Request, []string{
		"instruction", "task_toml", "dockerfile", "solve_script", "test_script", "tests_analysis", "candidate_snapshot", "verification_contract",
	})
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	var snapshot workflowkit.CandidateSnapshot
	if err := decodeStrictJSON(string(inputs["candidate_snapshot"]), &snapshot); err != nil || snapshot.Validate() != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring host candidate verifier rejected the candidate snapshot")
	}
	files := map[string][]byte{
		standardAuthoringV3InstructionPath: inputs["instruction"], standardAuthoringV3TaskTOMLPath: inputs["task_toml"],
		standardAuthoringV3DockerfilePath: inputs["dockerfile"], standardAuthoringV3SolveScriptPath: inputs["solve_script"],
		standardAuthoringV3TestScriptPath: inputs["test_script"], standardAuthoringV3TestsAnalysisPath: inputs["tests_analysis"],
	}
	if err := validateStandardAuthoringV3CandidateFiles(snapshot, files); err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	verification, err := ParseStandardAuthoringVerificationContractJSON(inputs["verification_contract"])
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	runtimeContract, err := NewStandardAuthoringRuntimeContractV2()
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	validationContract, err := workflowkit.NewCandidateValidationContract(runtimeContract.Fingerprint, workflowkit.SHA256Fingerprint(inputs["verification_contract"]))
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if invocation.Request.Execution.ID == "" || invocation.Request.Claim.Stage == nil || invocation.Request.Claim.Stage.StageAttempt.ID == "" {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring host candidate verifier requires a frozen stage attempt")
	}
	validator := standardAuthoringV3CandidateValidator{
		harness: executor.candidateHarness, runID: invocation.Request.Execution.ID, stageAttemptID: string(invocation.Request.Claim.Stage.StageAttempt.ID),
		files: files, verification: verification, now: executor.core.now,
	}
	receipt, err := validator.ValidateCandidate(ctx, snapshot, validationContract)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	receiptJSON, err := json.Marshal(receipt)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	repairContext, err := workflowadapter.NewStandardAuthoringValidationRepairContext(receipt, standardAuthoringV3EditableFiles())
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	repairContextJSON, err := json.Marshal(repairContext)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	artifacts := []workflowkit.StageArtifact{
		{Name: "validation_receipt", SchemaVersion: workflowkit.ValidationReceiptFormat, Content: receiptJSON},
		{Name: workflowadapter.StandardAuthoringValidationRepairContextArtifact, SchemaVersion: workflowadapter.StandardAuthoringValidationRepairContextSchemaVersion, Content: repairContextJSON},
	}
	// A rejected receipt is structured evidence for repair, not a terminal
	// workflow verdict. authoring_repair binds its next candidate to this
	// receipt through validation_repair_context and its repair ledger.
	return workflowkit.StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass}, Artifacts: artifacts}, nil
}

func (executor *StandardAuthoringMaterializeExecutor) executeFinalAttestation(ctx context.Context, invocation stageprovider.StageOperationInvocation) (workflowkit.StageExecutionResult, error) {
	if invocation.Resolution.StageKey != workflowkit.StageKey(workflowadapter.FinalAttestation) || invocation.Request.Stage.Key != workflowkit.StageKey(workflowadapter.FinalAttestation) {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring final attestation received an unbound operation")
	}
	inputs, err := standardAuthoringV3BuiltinInputs(ctx, invocation.Request, []string{"content_review_decision", "solution_review_decision", "validation_receipt"})
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	// See validateStandardAuthoringV3MaterializationEvidence: a stale receipt
	// is still cryptographically bound to the candidate and contract; the
	// recovery path re-runs HostCandidateVerify when a fresh receipt is
	// needed instead of rejecting an operator-paced review.
	var receipt workflowkit.ValidationReceipt
	if err := decodeStrictJSON(string(inputs["validation_receipt"]), &receipt); err != nil || receipt.Validate() != nil || receipt.Verdict != workflowkit.ValidationPass {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("Standard authoring final attestation requires a passing validation receipt")
	}
	attestation := standardAuthoringFinalAttestation{
		Format: "harbor.standard-authoring-final-attestation.v1", Version: "1", SnapshotDigest: receipt.SnapshotDigest,
		ValidationReceiptDigest: receipt.Digest, ContentReviewDigest: workflowkit.SHA256Fingerprint(inputs["content_review_decision"]),
		SolutionReviewDigest: workflowkit.SHA256Fingerprint(inputs["solution_review_decision"]),
	}
	encoded, err := json.Marshal(attestation)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	return workflowkit.StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass}, Artifacts: []workflowkit.StageArtifact{{Name: "final_attestation", SchemaVersion: "harbor.standard-authoring-final-attestation.v1", Content: encoded}}}, nil
}

func standardAuthoringV3BuiltinInputs(ctx context.Context, request workflowkit.StageExecutionRequest, names []string) (map[string][]byte, error) {
	if request.ReadInput == nil {
		return nil, fmt.Errorf("Standard authoring built-in has no frozen input reader")
	}
	bindings := make(map[string]workflowkit.ArtifactBinding, len(request.Inputs))
	for _, binding := range request.Inputs {
		if _, exists := bindings[binding.Name]; exists {
			return nil, fmt.Errorf("Standard authoring built-in has duplicate input %q", binding.Name)
		}
		bindings[binding.Name] = binding
	}
	result := make(map[string][]byte, len(names))
	for _, name := range names {
		binding, present := bindings[name]
		if !present {
			return nil, fmt.Errorf("Standard authoring built-in is missing input %q", name)
		}
		content, err := request.ReadInput(ctx, binding)
		if err != nil || workflowkit.SHA256Fingerprint(content) != binding.ContentDigest {
			return nil, fmt.Errorf("Standard authoring built-in input %q is unavailable or changed", name)
		}
		result[name] = append([]byte(nil), content...)
	}
	return result, nil
}
