package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/authoringharness"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// standardAuthoringValidationReceiptTTL bounds how long a host validation
// receipt remains current after it is issued. The final attestation admits a
// receipt only while it is still current, and that boundary is reached after
// the human content and solution review gates, so the TTL must comfortably
// cover the operator review window rather than only the validator run.
const standardAuthoringValidationReceiptTTL = 2 * time.Hour

// standardAuthoringV3CandidateValidator is created by the host after it has
// read frozen artifact bindings. Its file bytes and Run identity are not tool
// inputs, so CandidateValidator retains its narrow snapshot-and-contract API.
type standardAuthoringV3CandidateValidator struct {
	harness        *StandardAuthoringDockerHarness
	runID          string
	stageAttemptID string
	files          map[string][]byte
	verification   StandardAuthoringVerificationContract
	now            func() time.Time
}

func (validator standardAuthoringV3CandidateValidator) ValidateCandidate(ctx context.Context, snapshot workflowkit.CandidateSnapshot, contract workflowkit.CandidateValidationContract) (workflowkit.ValidationReceipt, error) {
	if validator.harness == nil || validator.now == nil {
		return workflowkit.ValidationReceipt{}, fmt.Errorf("Standard authoring candidate validator is not configured")
	}
	if err := contract.Validate(); err != nil {
		return workflowkit.ValidationReceipt{}, err
	}
	if err := validator.verification.Validate(); err != nil {
		return workflowkit.ValidationReceipt{}, err
	}
	result, err := validator.harness.ValidateV3Candidate(ctx, validator.runID, validator.stageAttemptID, snapshot, cloneStandardAuthoringCandidateFiles(validator.files), validator.verification)
	if err != nil {
		return workflowkit.ValidationReceipt{}, err
	}
	contractDigest, err := contract.Fingerprint()
	if err != nil {
		return workflowkit.ValidationReceipt{}, err
	}
	now := validator.now().UTC()
	receipt := workflowkit.ValidationReceipt{
		SnapshotDigest: snapshot.Digest, ContractDigest: contractDigest,
		Verdict: workflowkit.ValidationPass, Diagnostics: standardAuthoringV3ReceiptDiagnostics(result),
		IssuedAt: now, ExpiresAt: now.Add(standardAuthoringValidationReceiptTTL),
	}
	if !result.Passed {
		receipt.Verdict = workflowkit.ValidationReject
		receipt.FailureCode = standardAuthoringV3FailureCode(result)
	}
	return workflowkit.NewValidationReceipt(receipt)
}

func cloneStandardAuthoringCandidateFiles(files map[string][]byte) map[string][]byte {
	clone := make(map[string][]byte, len(files))
	for path, content := range files {
		clone[path] = append([]byte(nil), content...)
	}
	return clone
}

// standardAuthoringV3FailureCode classifies a failed host validation by its
// first failing step. Candidate defects stay validator_reject (and consume a
// repair opportunity), while environment-build and source-access failures are
// host/environment problems that must never eat into the authoring repair
// budget: the frozen source cannot be edited, so a read-only /source mount or
// an image whose USER drops access to it is fixed in environment/Dockerfile,
// not by a candidate repair.
func standardAuthoringV3FailureCode(result authoringharness.Result) workflowkit.AgentFailureCode {
	for _, step := range result.Steps {
		if step.Passed {
			continue
		}
		if step.Step == "environment_build" || step.Step == "source_access" {
			return workflowkit.AgentFailureEnvironmentFault
		}
		return workflowkit.AgentFailureValidatorReject
	}
	return workflowkit.AgentFailureValidatorReject
}

func standardAuthoringV3ReceiptDiagnostics(result authoringharness.Result) []workflowkit.AgentCommandReport {
	diagnostics := make([]workflowkit.AgentCommandReport, 0, len(result.Steps))
	for _, step := range result.Steps {
		diagnostics = append(diagnostics, workflowkit.AgentCommandReport{
			CommandID: step.Step, ExitCode: step.ExitCode,
			TestStarted: step.Step == "baseline_verify" || step.Step == "oracle_verify" || step.Step == "coverage_verify",
			StdoutTail:  standardAuthoringV3DiagnosticTail(step.StdoutTail), StderrTail: standardAuthoringV3DiagnosticTail(step.StderrTail),
		})
	}
	return diagnostics
}

func standardAuthoringV3DiagnosticTail(value string) string {
	if len(value) <= workflowkit.MaxRedactedDiagnosticTail {
		return value
	}
	return strings.ToValidUTF8(value[len(value)-workflowkit.MaxRedactedDiagnosticTail:], "")
}

var _ workflowkit.CandidateValidator = standardAuthoringV3CandidateValidator{}

// ValidateStandardAuthoringCandidate is the repair-conversation bridge for
// the host-owned verifier. The agent supplies neither paths nor commands; the
// stage provider passes only bytes it has just captured from the fenced
// workspace and the frozen verification-contract artifact.
func (harness *StandardAuthoringDockerHarness) ValidateStandardAuthoringCandidate(ctx context.Context, request stageprovider.StandardAuthoringCandidateValidationRequest) (workflowkit.ValidationReceipt, error) {
	if harness == nil {
		return workflowkit.ValidationReceipt{}, fmt.Errorf("Standard authoring candidate validator is not configured")
	}
	verification, err := ParseStandardAuthoringVerificationContractJSON(request.VerificationContract)
	if err != nil {
		return workflowkit.ValidationReceipt{}, err
	}
	runtimeContract, err := NewStandardAuthoringRuntimeContractV2()
	if err != nil {
		return workflowkit.ValidationReceipt{}, err
	}
	validationContract, err := workflowkit.NewCandidateValidationContract(runtimeContract.Fingerprint, workflowkit.SHA256Fingerprint(request.VerificationContract))
	if err != nil {
		return workflowkit.ValidationReceipt{}, err
	}
	validator := standardAuthoringV3CandidateValidator{
		harness: harness, runID: request.RunID, stageAttemptID: request.StageAttemptID,
		files: cloneStandardAuthoringCandidateFiles(request.Files), verification: verification, now: time.Now,
	}
	return validator.ValidateCandidate(ctx, request.Snapshot, validationContract)
}

var _ stageprovider.StandardAuthoringCandidateValidationTool = (*StandardAuthoringDockerHarness)(nil)
