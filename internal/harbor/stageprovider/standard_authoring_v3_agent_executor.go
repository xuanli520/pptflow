package stageprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/internal/harbor/authoringharness"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	standardAuthoringV3SubmitOutputTool = "harbor_submit_output"
	standardAuthoringV3ValidateTool     = "harbor_validate_candidate"
	standardAuthoringV3ContextLimit     = 2 << 20

	// A returned model turn is an audit fact even when the worker that made the
	// call is being canceled. Keep the completion write bounded while preserving
	// the caller's context values for the checkpoint boundary.
	standardAuthoringV3TranscriptPersistTimeout = 10 * time.Second

	standardAuthoringV3AgentProtocolPrefix = "standard_authoring_v3_agent_protocol."

	// StandardAuthoringProtocolFailureMissingSubmission records a prose-only
	// response: no terminal submission tool was called.
	StandardAuthoringProtocolFailureMissingSubmission = standardAuthoringV3AgentProtocolPrefix + "missing_submission"
	// StandardAuthoringProtocolFailureEmptySubmission records a terminal tool
	// call without the required structured output.
	StandardAuthoringProtocolFailureEmptySubmission = standardAuthoringV3AgentProtocolPrefix + "empty_submission"
	// StandardAuthoringProtocolFailureUndeclaredOutput records output names or
	// counts that do not match the frozen stage contract.
	StandardAuthoringProtocolFailureUndeclaredOutput = standardAuthoringV3AgentProtocolPrefix + "undeclared_output"
	// StandardAuthoringProtocolFailureTypedArtifactInvalid records a malformed
	// structured submission or typed artifact validation failure.
	StandardAuthoringProtocolFailureTypedArtifactInvalid = standardAuthoringV3AgentProtocolPrefix + "typed_artifact_invalid"
)

// IsStandardAuthoringProtocolFailure reports the narrow class of submission
// failures that may be retried against the same frozen execution inputs. It
// deliberately excludes runtime, source, policy, quota, and host-validation
// failures.
func IsStandardAuthoringProtocolFailure(code string) bool {
	switch strings.TrimSpace(code) {
	case StandardAuthoringProtocolFailureMissingSubmission,
		StandardAuthoringProtocolFailureEmptySubmission,
		StandardAuthoringProtocolFailureUndeclaredOutput,
		StandardAuthoringProtocolFailureTypedArtifactInvalid:
		return true
	default:
		return false
	}
}

const standardAuthoringV3AgentOutputSchemaCanonicalJSON = `{"$id":"harbor.standard-authoring-v3-agent-output.v1","$schema":"http://json-schema.org/draft-07/schema#","oneOf":[{"additionalProperties":false,"properties":{"verdict":{"const":"pass"}},"required":["verdict"],"type":"object"},{"additionalProperties":false,"properties":{"artifacts":{"items":{"additionalProperties":false,"properties":{"content":{"type":"string"},"name":{"type":"string"}},"required":["name","content"],"type":"object"},"minItems":1,"type":"array"},"verdict":{"const":"pass"}},"required":["verdict","artifacts"],"type":"object"}]}`

// ValidateStandardAuthoringV3AgentOutputSchemaAsset accepts the single
// raw-content schema pinned for 3.0 Agent submissions. It deliberately has
// no Base64 property: artifacts are typed host inputs and raw structured
// strings only, while author candidates are read from the fenced workspace.
func ValidateStandardAuthoringV3AgentOutputSchemaAsset(template workflowadapter.TemplateReference, stageKey workflowkit.StageKey, raw []byte) error {
	if !template.Equal(workflowadapter.StandardAuthoringCurrentTemplateReference()) {
		return fmt.Errorf("%w: V3 Agent output schema names an uninstalled template", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	stage, found := workflowadapter.StandardAuthoringCurrentWorkflowTemplate().Catalog.Stage(stageKey)
	if !found || stage.AgentRole == nil {
		return fmt.Errorf("%w: V3 Agent output schema names a non-Agent stage", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if len(raw) == 0 || len(raw) > standardAuthoringCodexContractAssetLimit || !json.Valid(raw) || rejectDuplicateDeploymentCatalogJSONKeys(raw) != nil || !bytes.Equal(standardAuthoringCodexCanonicalAssetBody(raw), []byte(standardAuthoringV3AgentOutputSchemaCanonicalJSON)) {
		return fmt.Errorf("%w: V3 Agent output schema is not the locked raw-content template", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	return nil
}

// StandardAuthoringCodexOutputSchemaFingerprintForTemplateStage remains as a
// read-only audit helper for frozen records. Every executable 3.0 Agent stage
// resolves to the raw-content V3 schema above; it does not select a legacy
// fixed-file schema.
func StandardAuthoringCodexOutputSchemaFingerprintForTemplateStage(template workflowadapter.TemplateReference, stageKey workflowkit.StageKey) workflowkit.Fingerprint {
	if err := ValidateStandardAuthoringV3AgentOutputSchemaAsset(template, stageKey, []byte(standardAuthoringV3AgentOutputSchemaCanonicalJSON)); err != nil {
		return ""
	}
	return workflowkit.SHA256Fingerprint([]byte(standardAuthoringV3AgentOutputSchemaCanonicalJSON))
}

func (executor *StandardAuthoringCodexAgentTurnExecutor) executeV3AgentTurn(ctx context.Context, invocation StageOperationInvocation, payload workflowadapter.AgentTurnOperationPayload, program StandardAuthoringCodexTurnProgram) (workflowkit.StageExecutionResult, error) {
	request := invocation.Request
	role := request.Stage.AgentRole
	if role == nil || role.MaxTurns != payload.MaxTurns || role.PromptAssetFingerprint == "" {
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureConfiguration), nil
	}
	wantTool := standardAuthoringV3SubmitOutputTool
	if role.RoleID == workflowkit.AgentRoleAuthor {
		wantTool = standardAuthoringV3ValidateTool
	}
	if len(role.AllowedDynamicTools) != 1 || role.AllowedDynamicTools[0] != wantTool {
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureConfiguration), nil
	}
	inputs, inputDigest, err := standardAuthoringV3ReadInputs(ctx, request)
	if err != nil {
		return standardAuthoringCodexFailure(workflowkit.FailurePermanent, standardAuthoringCodexFailureInput), nil
	}
	sourceRoot, err := executor.workspaceForExecution(request.Execution.ID)
	if err != nil {
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureWorkspace), nil
	}
	sourceIdentity, err := executor.verifyFrozenSource(ctx, request.Execution, sourceRoot)
	if err != nil {
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureSource), nil
	}
	workspace := sourceRoot
	cleanupWorkspace := func() {}
	if role.Workspace.Mode == workflowkit.WorkspaceNone {
		workspace, err = os.MkdirTemp("", "harbor-standard-authoring-v3-stage-")
		if err != nil {
			return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureWorkspace), nil
		}
		if err := os.Chmod(workspace, 0o700); err != nil {
			_ = os.RemoveAll(workspace)
			return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureWorkspace), nil
		}
		cleanupWorkspace = func() { _ = os.RemoveAll(workspace) }
	}
	defer cleanupWorkspace()
	var taskRoot string
	if role.Workspace.Mode == workflowkit.WorkspaceExclusiveWriter {
		if request.Claim.Stage == nil {
			return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureConfiguration), nil
		}
		workspace, err = executor.prepareAttemptWorkspace(ctx, request, sourceRoot)
		if err != nil {
			return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureWorkspace), nil
		}
		taskRoot = filepath.Join(workspace, StandardAuthoringCodexAttemptTaskDirectory)
		if err := standardAuthoringV3PrepareCandidateWorkspace(taskRoot, inputs); err != nil {
			return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureWorkspace), nil
		}
		if copiedIdentity, err := executor.verifyFrozenSource(ctx, request.Execution, sourceRoot); err != nil || copiedIdentity != sourceIdentity {
			return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureSource), nil
		}
	}
	attested, runtime, failure := executor.runtimeForEffect(ctx, invocation, payload)
	if failure != "" {
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, failure), nil
	}
	requiredSandboxMode, requiredSandboxPolicy, err := StandardAuthoringCodexSandboxForWorkspace(role.Workspace.Mode)
	if err != nil || attested.SandboxMode != requiredSandboxMode || attested.SandboxPolicy != requiredSandboxPolicy {
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureConfiguration), nil
	}
	submission := newStandardAuthoringV3Submission(request.Stage, role.RoleID, taskRoot, program.MaxOutputBytes)
	if request.Stage.Key == workflowkit.StageKey(workflowadapter.AuthoringRepair) {
		if executor.candidateValidator == nil || request.Claim.Stage == nil || request.Claim.Stage.StageAttempt.ID == "" {
			return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureConfiguration), nil
		}
		verificationContract, found := inputs["verification_contract"]
		if !found {
			return standardAuthoringCodexFailure(workflowkit.FailurePermanent, standardAuthoringCodexFailureInput), nil
		}
		if err := standardAuthoringV3ValidateRepairInputs(request.Execution.Workflow, inputs); err != nil {
			return standardAuthoringCodexFailure(workflowkit.FailurePermanent, standardAuthoringCodexFailureInput), nil
		}
		submission.repairLedger = func() ([]byte, error) {
			return standardAuthoringV3RepairLedger(request.Execution.Workflow, inputs)
		}
		submission.maxValidationAttempts = role.MaxValidationAttempts
		submission.oneValidationPerTurn = true
		submission.candidateValidator = func(validationCtx context.Context, snapshot workflowkit.CandidateSnapshot, files map[string][]byte) (workflowkit.ValidationReceipt, error) {
			return executor.candidateValidator.ValidateStandardAuthoringCandidate(validationCtx, StandardAuthoringCandidateValidationRequest{
				RunID: request.Execution.ID, StageAttemptID: string(request.Claim.Stage.StageAttempt.ID), Snapshot: snapshot,
				Files: cloneStandardAuthoringV3CandidateFiles(files), VerificationContract: append([]byte(nil), verificationContract...),
			})
		}
	}
	accepted := make(chan workflowkit.StageExecutionResult, 1)
	tool := submission.dynamicTool()
	handler := tool.Handler
	tool.Handler = func(toolCtx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		response, err := handler(toolCtx, raw)
		if err == nil {
			if result, ok := submission.acceptedResult(); ok {
				select {
				case accepted <- result:
				default:
				}
			}
		}
		return response, err
	}
	logPath, cleanupLog, err := executor.controlledTurnLogPath(request, sourceRoot)
	if err != nil {
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureWorkspace), nil
	}
	defer cleanupLog()
	conversation, err := runtime.OpenConversation(ctx, agent.ConversationRequest{
		ProjectPath: workspace, Model: attested.ModelID, ReasoningEffort: string(attested.ReasoningEffort),
		SandboxMode: attested.SandboxMode, SandboxPolicy: attested.SandboxPolicy, NetworkAccess: false,
		WorkspaceRoots: []string{workspace}, TimeoutSeconds: standardAuthoringCodexTimeoutSeconds(request.Stage.Budget.TurnTimeout),
		MaxOutputBytes: program.MaxOutputBytes, CapabilitySummary: attested.CLIVersionOutput, LogPath: logPath,
		DynamicTools: []agent.DynamicTool{tool},
	})
	if err != nil {
		return standardAuthoringCodexFailure(workflowkit.FailureProcess, standardAuthoringCodexFailureRuntime), nil
	}
	defer conversation.Close()
	contextDocument, err := standardAuthoringV3ContextDocument(request, program, inputDigest, inputs, role.Workspace.Mode == workflowkit.WorkspaceExclusiveWriter)
	if err != nil {
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureConfiguration), nil
	}
	for ordinal, prompt := range program.TurnPrompts {
		turn := ordinal + 1
		if err := executor.checkpoint(ctx, request, program, inputDigest, turn, "turn_ready", "", nil); err != nil {
			if contextError(ctx) != nil {
				return standardAuthoringCodexInterrupted(), nil
			}
			return standardAuthoringCodexFailure(workflowkit.FailureUnknown, standardAuthoringCodexFailureCheckpoint), nil
		}
		if err := request.Charge(ctx, workflowkit.StageUsage{OperationKey: standardAuthoringCodexUsageKey(request, turn), Dimension: "agent_turn", Units: 1, OccurredAt: executor.now().UTC()}); err != nil {
			return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureQuota), nil
		}
		if currentIdentity, err := executor.verifyFrozenSource(ctx, request.Execution, sourceRoot); err != nil || currentIdentity != sourceIdentity {
			return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureSource), nil
		}
		turnRequest := agent.TurnRequest{ProjectPath: workspace, Prompt: prompt, Model: attested.ModelID, ReasoningEffort: string(attested.ReasoningEffort), SandboxMode: attested.SandboxMode, SandboxPolicy: attested.SandboxPolicy, NetworkAccess: false, WorkspaceRoots: []string{workspace}, TimeoutSeconds: standardAuthoringCodexTimeoutSeconds(request.Stage.Budget.TurnTimeout), MaxOutputBytes: program.MaxOutputBytes, CapabilitySummary: attested.CLIVersionOutput, LogPath: logPath}
		if turn == 1 {
			turnRequest.Input = []agent.InputPart{{Type: "text", Text: string(contextDocument)}}
		}
		submission.beginTurn()
		submissionStart := submission.submissionCount()
		result, turnErr, acceptedResult, acceptedDuringTurn, closeErr := standardAuthoringCodexRunTurnUntilAccepted(ctx, conversation, turnRequest, accepted)
		if acceptedDuringTurn {
			failureCode := ""
			if closeErr != nil {
				failureCode = standardAuthoringCodexFailureRuntime
			}
			if failureCode == "" {
				if currentIdentity, err := executor.verifyFrozenSource(ctx, request.Execution, sourceRoot); err != nil || currentIdentity != sourceIdentity {
					failureCode = standardAuthoringCodexFailureSource
				}
			}
			if err := executor.checkpointCompletedTurn(ctx, request, program, inputDigest, turn, result, attested.ModelID, submission, submissionStart, failureCode); err != nil {
				if contextError(ctx) != nil {
					return standardAuthoringCodexInterrupted(), nil
				}
				return standardAuthoringCodexFailure(workflowkit.FailureUnknown, standardAuthoringCodexFailureCheckpoint), nil
			}
			if failureCode != "" {
				if failureCode == standardAuthoringCodexFailureRuntime {
					return standardAuthoringCodexFailure(workflowkit.FailureProcess, failureCode), nil
				}
				return standardAuthoringCodexFailure(workflowkit.FailurePolicy, failureCode), nil
			}
			return acceptedResult, nil
		}
		if acceptedResult, ok := submission.acceptedResult(); ok {
			failureCode := ""
			if currentIdentity, err := executor.verifyFrozenSource(ctx, request.Execution, sourceRoot); err != nil || currentIdentity != sourceIdentity {
				failureCode = standardAuthoringCodexFailureSource
			}
			if err := executor.checkpointCompletedTurn(ctx, request, program, inputDigest, turn, result, attested.ModelID, submission, submissionStart, failureCode); err != nil {
				if contextError(ctx) != nil {
					return standardAuthoringCodexInterrupted(), nil
				}
				return standardAuthoringCodexFailure(workflowkit.FailureUnknown, standardAuthoringCodexFailureCheckpoint), nil
			}
			if failureCode != "" {
				return standardAuthoringCodexFailure(workflowkit.FailurePolicy, failureCode), nil
			}
			return acceptedResult, nil
		}
		if turnErr != nil || result.Model != attested.ModelID {
			if err := executor.checkpointCompletedTurn(ctx, request, program, inputDigest, turn, result, attested.ModelID, submission, submissionStart, standardAuthoringCodexFailureRuntime); err != nil {
				if contextError(ctx) != nil {
					return standardAuthoringCodexInterrupted(), nil
				}
				return standardAuthoringCodexFailure(workflowkit.FailureUnknown, standardAuthoringCodexFailureCheckpoint), nil
			}
			return standardAuthoringCodexFailure(workflowkit.FailureProcess, standardAuthoringCodexFailureRuntime), nil
		}
		failureCode := ""
		if turn == len(program.TurnPrompts) {
			failureCode = submission.terminalFailureCode()
		}
		if err := executor.checkpointCompletedTurn(ctx, request, program, inputDigest, turn, result, attested.ModelID, submission, submissionStart, failureCode); err != nil {
			if contextError(ctx) != nil {
				return standardAuthoringCodexInterrupted(), nil
			}
			return standardAuthoringCodexFailure(workflowkit.FailureUnknown, standardAuthoringCodexFailureCheckpoint), nil
		}
		if failureCode != "" {
			return standardAuthoringCodexFailure(workflowkit.FailureProcess, failureCode), nil
		}
	}
	return standardAuthoringCodexFailure(workflowkit.FailureProcess, StandardAuthoringProtocolFailureMissingSubmission), nil
}

func (executor *StandardAuthoringCodexAgentTurnExecutor) checkpointCompletedTurn(ctx context.Context, request workflowkit.StageExecutionRequest, program StandardAuthoringCodexTurnProgram, inputFingerprint workflowkit.Fingerprint, turn int, result agent.TurnResult, expectedModel string, submission *standardAuthoringV3Submission, submissionStart int, failureCode string) error {
	if submission == nil {
		return fmt.Errorf("missing V3 submission diagnostics")
	}
	transcript := submission.transcriptSince(submissionStart, result, expectedModel, failureCode)
	durableCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), standardAuthoringV3TranscriptPersistTimeout)
	defer cancel()
	return executor.checkpoint(durableCtx, request, program, inputFingerprint, turn, "turn_completed", string(workflowkit.SHA256Fingerprint([]byte(result.Text))), &transcript)
}

// controlledTurnLogPath keeps App Server JSON-RPC diagnostics out of the
// immutable source tree. Production RunScoped work writes beneath the managed
// run root; the static test/embed seam uses a private temporary directory so a
// log cannot alter the source identity that the executor re-attests.
func (executor *StandardAuthoringCodexAgentTurnExecutor) controlledTurnLogPath(request workflowkit.StageExecutionRequest, sourceRoot string) (string, func(), error) {
	key := standardAuthoringCodexExecutionKey(request, 0, "app_server_log")
	if key == "invalid" {
		return "", nil, fmt.Errorf("invalid controlled Agent log identity")
	}
	if executor.workspaceMode == StandardAuthoringCodexWorkspaceRunScoped {
		runRoot := filepath.Dir(sourceRoot)
		if !standardAuthoringPathWithin(executor.workspaceRoot, runRoot) {
			return "", nil, fmt.Errorf("controlled Agent log root escapes managed workspace")
		}
		return filepath.Join(runRoot, "agent-turn-logs", key+".log"), func() {}, nil
	}
	directory, err := os.MkdirTemp("", "harbor-standard-authoring-agent-turn-log-")
	if err != nil {
		return "", nil, err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return "", nil, err
	}
	return filepath.Join(directory, key+".log"), func() { _ = os.RemoveAll(directory) }, nil
}

func standardAuthoringV3ValidateRepairInputs(workflow workflowkit.WorkflowDescriptor, inputs map[string][]byte) error {
	_, err := standardAuthoringV3RepairLedger(workflow, inputs)
	return err
}

func standardAuthoringV3RepairLedger(workflow workflowkit.WorkflowDescriptor, inputs map[string][]byte) ([]byte, error) {
	identity, found, err := standardAuthoringV3CandidateValidationIdentityFromInputs(inputs)
	if err != nil || !found {
		return nil, fmt.Errorf("candidate validation identity is unavailable")
	}
	rawRepairContext, found := inputs[workflowadapter.StandardAuthoringValidationRepairContextArtifact]
	if !found {
		return nil, fmt.Errorf("validation repair context is unavailable")
	}
	repairContext, err := workflowadapter.ParseStandardAuthoringValidationRepairContextJSON(rawRepairContext)
	if err != nil {
		return nil, fmt.Errorf("validation repair context is invalid")
	}
	repairContextDigest := workflowkit.SHA256Fingerprint(rawRepairContext)
	if repairContext.CandidateDigest != identity.CandidateSnapshotDigest || repairContext.ReceiptDigest != identity.ValidationReceiptDigest {
		return nil, fmt.Errorf("validation repair context is not bound to the reviewed candidate receipt")
	}
	rules := []workflowkit.WorkflowRepairRule{
		{FindingCode: "host_validation_reject", ProducingStage: workflowkit.StageKey(workflowadapter.HostCandidateVerify), TargetWriter: workflowkit.StageKey(workflowadapter.AuthoringRepair), RequiresCandidateSnapshot: true, ConsumesCandidateRepair: true},
		{FindingCode: "host_validation_pass", ProducingStage: workflowkit.StageKey(workflowadapter.HostCandidateVerify), TargetWriter: workflowkit.StageKey(workflowadapter.AuthoringRepair), RequiresCandidateSnapshot: false, ConsumesCandidateRepair: false},
		{FindingCode: "test_quality_defect", ProducingStage: workflowkit.StageKey(workflowadapter.TestQualityCritic), TargetWriter: workflowkit.StageKey(workflowadapter.AuthoringRepair), RequiresCandidateSnapshot: true, ConsumesCandidateRepair: true},
		{FindingCode: "solution_integrity_defect", ProducingStage: workflowkit.StageKey(workflowadapter.SolutionIntegrityCritic), TargetWriter: workflowkit.StageKey(workflowadapter.AuthoringRepair), RequiresCandidateSnapshot: true, ConsumesCandidateRepair: true},
	}
	entries := make([]workflowkit.WorkflowRepairLedgerEntry, 0, 3)
	chargedTargets := make(map[workflowkit.StageKey]struct{})
	hostCode := "host_validation_pass"
	if repairContext.ValidationVerdict == workflowkit.ValidationReject {
		hostCode = "host_validation_reject"
	}
	hostFinding, err := workflowkit.NewWorkflowFinding(workflowkit.WorkflowFinding{
		Code: hostCode, ProducingStage: workflowkit.StageKey(workflowadapter.HostCandidateVerify),
		TargetWriter: workflowkit.StageKey(workflowadapter.AuthoringRepair), EvidenceDigest: identity.ValidationReceiptDigest,
		CandidateDigest: identity.CandidateSnapshotDigest, DiagnosticDigest: repairContextDigest,
	})
	if err != nil {
		return nil, err
	}
	hostPlan, err := workflowkit.PlanWorkflowRepair(workflow, hostFinding, rules, nil)
	if err != nil {
		return nil, fmt.Errorf("validation repair context is not an allowed fenced repair")
	}
	entries = append(entries, workflowkit.WorkflowRepairLedgerEntry{Finding: hostFinding, ConsumedCandidateRound: hostPlan.CandidateRepairRound > 0})
	if hostPlan.CandidateRepairRound > 0 {
		chargedTargets[hostFinding.TargetWriter] = struct{}{}
	}
	for _, name := range []string{"test_quality_finding", "solution_integrity_finding"} {
		rawFinding, found := inputs[name]
		if !found {
			continue
		}
		var finding workflowkit.WorkflowFinding
		if err := standardAuthoringV3DecodeTypedInput(rawFinding, &finding); err != nil || finding.Validate() != nil {
			return nil, fmt.Errorf("finding %q is invalid", name)
		}
		if finding.CandidateDigest != identity.CandidateSnapshotDigest || finding.EvidenceDigest != identity.ValidationReceiptDigest || finding.DiagnosticDigest != identity.ValidationReceiptDigest {
			return nil, fmt.Errorf("finding %q is not bound to the reviewed candidate receipt", name)
		}
		plan, err := workflowkit.PlanWorkflowRepair(workflow, finding, rules, nil)
		if err != nil {
			return nil, fmt.Errorf("finding %q is not an allowed fenced repair", name)
		}
		_, alreadyCharged := chargedTargets[finding.TargetWriter]
		entries = append(entries, workflowkit.WorkflowRepairLedgerEntry{Finding: finding, ConsumedCandidateRound: plan.CandidateRepairRound > 0 && !alreadyCharged})
		if plan.CandidateRepairRound > 0 {
			chargedTargets[finding.TargetWriter] = struct{}{}
		}
	}
	ledger, err := workflowkit.NewWorkflowRepairLedger(entries)
	if err != nil {
		return nil, err
	}
	return json.Marshal(ledger)
}

func standardAuthoringV3CandidateValidationIdentityFromInputs(inputs map[string][]byte) (standardAuthoringV3CandidateValidationIdentity, bool, error) {
	rawSnapshot, hasSnapshot := inputs["candidate_snapshot"]
	rawReceipt, hasReceipt := inputs["validation_receipt"]
	if !hasSnapshot && !hasReceipt {
		return standardAuthoringV3CandidateValidationIdentity{}, false, nil
	}
	if !hasSnapshot || !hasReceipt {
		return standardAuthoringV3CandidateValidationIdentity{}, false, fmt.Errorf("candidate validation inputs are incomplete")
	}
	var snapshot workflowkit.CandidateSnapshot
	if err := standardAuthoringV3DecodeTypedInput(rawSnapshot, &snapshot); err != nil || snapshot.Validate() != nil {
		return standardAuthoringV3CandidateValidationIdentity{}, false, fmt.Errorf("candidate snapshot is invalid")
	}
	var receipt workflowkit.ValidationReceipt
	if err := standardAuthoringV3DecodeTypedInput(rawReceipt, &receipt); err != nil || receipt.Validate() != nil || (receipt.Verdict != workflowkit.ValidationPass && receipt.Verdict != workflowkit.ValidationReject) || receipt.SnapshotDigest != snapshot.Digest {
		return standardAuthoringV3CandidateValidationIdentity{}, false, fmt.Errorf("validation receipt is not a current candidate receipt")
	}
	return standardAuthoringV3CandidateValidationIdentity{CandidateSnapshotDigest: snapshot.Digest, ValidationReceiptDigest: receipt.Digest}, true, nil
}

func standardAuthoringV3DecodeTypedInput(raw []byte, destination any) error {
	if len(raw) == 0 || rejectDuplicateDeploymentCatalogJSONKeys(raw) != nil {
		return fmt.Errorf("invalid typed input")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing typed input")
	}
	return nil
}

type standardAuthoringV3Input struct {
	Name          string                  `json:"name"`
	SchemaVersion string                  `json:"schema_version"`
	Digest        workflowkit.Fingerprint `json:"digest"`
	Content       string                  `json:"content"`
}

// standardAuthoringV3CandidateValidationIdentity makes the semantic identities
// inside typed candidate artifacts explicit. Artifact digests attest raw JSON
// containers; repair findings must bind the identities inside those containers.
type standardAuthoringV3CandidateValidationIdentity struct {
	CandidateSnapshotDigest workflowkit.Fingerprint `json:"candidate_snapshot_digest"`
	ValidationReceiptDigest workflowkit.Fingerprint `json:"validation_receipt_digest"`
}

// standardAuthoringV3TerminalSubmission makes the otherwise host-private
// completion contract visible in the agent's frozen context. Without the
// declared names, a structured-output agent cannot construct a valid tool
// call even when its prompt requires one.
type standardAuthoringV3TerminalSubmission struct {
	Tool               string                     `json:"tool"`
	Mode               string                     `json:"mode"`
	Required           bool                       `json:"required"`
	RequiredOutputs    []workflowkit.ArtifactSpec `json:"required_outputs,omitempty"`
	CandidateDirectory string                     `json:"candidate_directory,omitempty"`
	CandidateFiles     []string                   `json:"candidate_files,omitempty"`
	Instructions       string                     `json:"instructions"`
}

type standardAuthoringV3ValidationEnvironmentContract struct {
	Format               string                                           `json:"format"`
	Version              string                                           `json:"version"`
	AuthoringWorkspace   standardAuthoringV3AuthoringWorkspaceContract    `json:"authoring_workspace"`
	ValidatorRuntime     standardAuthoringV3ValidatorRuntimeContract      `json:"validator_runtime"`
	RuntimeConstraints   standardAuthoringV3RuntimeConstraintsContract    `json:"runtime_constraints"`
	ScriptProtocol       []string                                         `json:"script_protocol"`
	RustCargoBrowserWASM *standardAuthoringV3RustCargoBrowserWASMContract `json:"rust_cargo_browser_wasm,omitempty"`
}

type standardAuthoringV3AuthoringWorkspaceContract struct {
	Source standardAuthoringV3PathContract `json:"source"`
	Task   standardAuthoringV3PathContract `json:"task"`
}

type standardAuthoringV3ValidatorRuntimeContract struct {
	Source       standardAuthoringV3PathContract `json:"source"`
	Task         standardAuthoringV3PathContract `json:"task"`
	Work         standardAuthoringV3PathContract `json:"work"`
	CopyProtocol string                          `json:"copy_protocol"`
}

type standardAuthoringV3RuntimeConstraintsContract struct {
	Network            string                         `json:"network"`
	RootFilesystem     string                         `json:"root_filesystem"`
	Tmpfs              []standardAuthoringV3Tmpfs     `json:"tmpfs"`
	Environment        []standardAuthoringV3NameValue `json:"environment"`
	DockerBuildNetwork string                         `json:"docker_build_network"`
}

type standardAuthoringV3PathContract struct {
	Path    string `json:"path"`
	Access  string `json:"access"`
	Purpose string `json:"purpose"`
}

type standardAuthoringV3Tmpfs struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Size    string `json:"size"`
	Purpose string `json:"purpose"`
}

type standardAuthoringV3NameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type standardAuthoringV3RustCargoBrowserWASMContract struct {
	Applies               bool                           `json:"applies"`
	ToolchainProvisioning string                         `json:"toolchain_provisioning"`
	DependencySources     []string                       `json:"dependency_sources"`
	RuntimeEnvironment    []standardAuthoringV3NameValue `json:"runtime_environment"`
	CachePolicy           string                         `json:"cache_policy"`
}

func standardAuthoringV3ReadInputs(ctx context.Context, request workflowkit.StageExecutionRequest) (map[string][]byte, workflowkit.Fingerprint, error) {
	if request.ReadInput == nil {
		return nil, "", fmt.Errorf("missing frozen input reader")
	}
	bindings := append([]workflowkit.ArtifactBinding(nil), request.Inputs...)
	sort.Slice(bindings, func(left, right int) bool { return bindings[left].Name < bindings[right].Name })
	digest, err := workflowkit.FingerprintArtifactBindings(bindings)
	if err != nil {
		return nil, "", err
	}
	inputs := make(map[string][]byte, len(bindings))
	total := 0
	for _, binding := range bindings {
		if _, exists := inputs[binding.Name]; exists {
			return nil, "", fmt.Errorf("duplicate input")
		}
		content, err := request.ReadInput(ctx, binding)
		if err != nil || workflowkit.SHA256Fingerprint(content) != binding.ContentDigest || len(content) > standardAuthoringV3ContextLimit-total {
			return nil, "", fmt.Errorf("frozen input is unavailable or oversized")
		}
		total += len(content)
		inputs[binding.Name] = append([]byte(nil), content...)
	}
	return inputs, digest, nil
}

func standardAuthoringV3ContextDocument(request workflowkit.StageExecutionRequest, program StandardAuthoringCodexTurnProgram, digest workflowkit.Fingerprint, inputs map[string][]byte, candidateWriter bool) ([]byte, error) {
	keys := make([]string, 0, len(inputs))
	for key := range inputs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]standardAuthoringV3Input, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, standardAuthoringV3Input{Name: key, Digest: workflowkit.SHA256Fingerprint(inputs[key]), Content: string(inputs[key])})
	}
	submission := standardAuthoringV3TerminalSubmission{
		Tool:     standardAuthoringV3SubmitOutputTool,
		Mode:     "structured_artifacts",
		Required: true,
		Instructions: "Call this tool with verdict pass and one raw content artifact for every required output. " +
			"If the tool returns accepted:false, correct the rejected payload and call it again in the same or a later turn. " +
			"Only an accepted:true receipt completes this stage; a prose final answer never completes it.",
		RequiredOutputs: append([]workflowkit.ArtifactSpec(nil), request.Stage.Outputs...),
	}
	if candidateWriter {
		submission = standardAuthoringV3TerminalSubmission{
			Tool:               standardAuthoringV3ValidateTool,
			Mode:               "candidate_workspace",
			Required:           true,
			CandidateDirectory: StandardAuthoringCodexAttemptTaskDirectory,
			CandidateFiles: []string{
				"instruction.md",
				"task.toml",
				authoringharness.DockerfileRelativePath,
				authoringharness.SolveScriptRelativePath,
				authoringharness.TestScriptRelativePath,
				"tests_analysis.json",
			},
			Instructions: "Write only the listed candidate files below candidate_directory, then call this tool with {\"verdict\":\"pass\"} and no artifacts. " +
				"If validation rejects the candidate, apply its bounded diagnostics and call again; a passing call completes this stage.",
		}
	}
	candidateValidationIdentity, hasCandidateValidationIdentity, err := standardAuthoringV3CandidateValidationIdentityFromInputs(inputs)
	if err != nil {
		return nil, err
	}
	var repairContext *workflowadapter.StandardAuthoringValidationRepairContext
	if raw, found := inputs[workflowadapter.StandardAuthoringValidationRepairContextArtifact]; found {
		parsed, err := workflowadapter.ParseStandardAuthoringValidationRepairContextJSON(raw)
		if err != nil {
			return nil, err
		}
		repairContext = &parsed
	}
	var validationEnvironmentContract *standardAuthoringV3ValidationEnvironmentContract
	if candidateWriter {
		contract, err := standardAuthoringV3ValidationEnvironmentContractForInputs(inputs)
		if err != nil {
			return nil, err
		}
		validationEnvironmentContract = &contract
	}
	document := struct {
		Format                        string                                                    `json:"format"`
		Version                       string                                                    `json:"version"`
		Stage                         string                                                    `json:"stage"`
		Program                       workflowkit.Fingerprint                                   `json:"program_fingerprint"`
		Inputs                        workflowkit.Fingerprint                                   `json:"inputs_fingerprint"`
		CandidateWorkspace            bool                                                      `json:"candidate_workspace"`
		CandidateValidationIdentity   *standardAuthoringV3CandidateValidationIdentity           `json:"candidate_validation_identity,omitempty"`
		ValidationRepairContext       *workflowadapter.StandardAuthoringValidationRepairContext `json:"validation_repair_context,omitempty"`
		ValidationEnvironmentContract *standardAuthoringV3ValidationEnvironmentContract         `json:"validation_environment_contract,omitempty"`
		TerminalSubmission            standardAuthoringV3TerminalSubmission                     `json:"terminal_submission"`
		Artifacts                     []standardAuthoringV3Input                                `json:"artifacts"`
	}{Format: "harbor.standard-authoring-v3-context.v1", Version: "1", Stage: string(request.Stage.Key), Program: program.Fingerprint, Inputs: digest, CandidateWorkspace: candidateWriter, ValidationRepairContext: repairContext, ValidationEnvironmentContract: validationEnvironmentContract, TerminalSubmission: submission, Artifacts: entries}
	if hasCandidateValidationIdentity {
		document.CandidateValidationIdentity = &candidateValidationIdentity
	}
	raw, err := json.Marshal(document)
	if err != nil || len(raw) > standardAuthoringV3ContextLimit {
		return nil, fmt.Errorf("context document is invalid")
	}
	return raw, nil
}

func standardAuthoringV3ValidationEnvironmentContractForInputs(inputs map[string][]byte) (standardAuthoringV3ValidationEnvironmentContract, error) {
	contract := standardAuthoringV3ValidationEnvironmentContract{
		Format:  "harbor.standard-authoring.validation-environment-contract.v1",
		Version: "1",
		AuthoringWorkspace: standardAuthoringV3AuthoringWorkspaceContract{
			Source: standardAuthoringV3PathContract{Path: StandardAuthoringCodexAttemptSourceDirectory, Access: "read_only_reference", Purpose: "Frozen source copy for inspection only; do not edit this tree."},
			Task:   standardAuthoringV3PathContract{Path: StandardAuthoringCodexAttemptTaskDirectory, Access: "writable_candidate_files_only", Purpose: "Write exactly the fixed candidate files captured by harbor_validate_candidate."},
		},
		ValidatorRuntime: standardAuthoringV3ValidatorRuntimeContract{
			Source:       standardAuthoringV3PathContract{Path: "/source", Access: "read_only_frozen_source", Purpose: "Immutable source mount owned by the validator."},
			Task:         standardAuthoringV3PathContract{Path: "/task", Access: "read_only_candidate_files", Purpose: "Immutable candidate scripts and metadata captured from task/."},
			Work:         standardAuthoringV3PathContract{Path: "/work", Access: "read_write_source_copy", Purpose: "Fresh writable copy of /source used by tests and solution scripts."},
			CopyProtocol: "Each validation starts by copying /source into /work and applying user-writable modes; solution and test commands run from the frozen VerificationContract workdir below /work.",
		},
		RuntimeConstraints: standardAuthoringV3RuntimeConstraintsContract{
			Network:        "none",
			RootFilesystem: "read_only",
			Tmpfs: []standardAuthoringV3Tmpfs{
				{Path: "/tmp", Mode: "rw,exec,nosuid", Size: "2g", Purpose: "Ephemeral writable runtime scratch; do not copy large registries or build caches here."},
				{Path: "/logs", Mode: "rw,noexec,nosuid", Size: "8m", Purpose: "Validator-owned binary reward/log protocol output."},
			},
			Environment: []standardAuthoringV3NameValue{
				{Name: "HOME", Value: "/tmp/harbor-home"},
				{Name: "XDG_CACHE_HOME", Value: "/tmp/harbor-cache"},
				{Name: "XDG_CONFIG_HOME", Value: "/tmp/harbor-config"},
				{Name: "HARBOR_SOURCE", Value: "/source"},
				{Name: "HARBOR_WORKSPACE", Value: "/work"},
				{Name: "HARBOR_TASK_ROOT", Value: "/task"},
			},
			DockerBuildNetwork: "default; Debian APT provisioning must use HTTPS sources, and runtime validation still has no network.",
		},
		ScriptProtocol: []string{
			"Authoring edits belong only under task/ fixed candidate files.",
			"solution/solve.sh and tests/test.sh must operate on the writable /work source copy during validation.",
			"Do not write authoring source/ or validator /source; those paths are immutable provenance inputs.",
		},
	}
	if guidance, err := standardAuthoringV3RustCargoBrowserWASMContractForInputs(inputs); err != nil {
		return standardAuthoringV3ValidationEnvironmentContract{}, err
	} else if guidance != nil {
		contract.RustCargoBrowserWASM = guidance
	}
	return contract, nil
}

func standardAuthoringV3RustCargoBrowserWASMContractForInputs(inputs map[string][]byte) (*standardAuthoringV3RustCargoBrowserWASMContract, error) {
	raw, found := inputs["verification_contract"]
	if !found {
		return nil, nil
	}
	contract, err := workflowadapter.ParseStandardAuthoringVerificationContractJSON(raw)
	if err != nil {
		return nil, err
	}
	if contract.CoverageMode != workflowadapter.StandardAuthoringCoverageBrowserWASM || !standardAuthoringV3CommandMentionsRustCargoBrowserWASM(contract.Command) {
		return nil, nil
	}
	return &standardAuthoringV3RustCargoBrowserWASMContract{
		Applies:               true,
		ToolchainProvisioning: "Install or prefetch generic Rust wasm/browser tools in environment/Dockerfile during Docker build. Docker build context is environment/, not the frozen source tree.",
		DependencySources: []string{
			"dependencies already present in the digest-pinned base image",
			"repository-vendored dependencies committed in the frozen source",
			"other explicitly frozen task inputs",
		},
		RuntimeEnvironment: []standardAuthoringV3NameValue{
			{Name: "CARGO_HOME", Value: "/work/.cargo-home"},
			{Name: "CARGO_TARGET_DIR", Value: "/work/.cargo-target"},
		},
		CachePolicy: "Runtime has no network and limited /tmp; keep Cargo target/cache data under /work and do not copy full registry or target caches into /tmp.",
	}, nil
}

func standardAuthoringV3CommandMentionsRustCargoBrowserWASM(command []string) bool {
	joined := strings.ToLower(strings.Join(command, " "))
	for _, token := range []string{"cargo", "wasm", "wasm-pack", "wasm-bindgen", "trunk", "playwright", "browser"} {
		if strings.Contains(joined, token) {
			return true
		}
	}
	return false
}

func standardAuthoringV3PrepareCandidateWorkspace(root string, inputs map[string][]byte) error {
	for _, relative := range []string{"instruction.md", "task.toml", authoringharness.DockerfileRelativePath, authoringharness.SolveScriptRelativePath, authoringharness.TestScriptRelativePath, "tests_analysis.json"} {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if existing, found := inputs[standardAuthoringV3ArtifactName(relative)]; found {
			if err := os.WriteFile(path, existing, 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

func standardAuthoringV3ArtifactName(path string) string {
	switch path {
	case "instruction.md":
		return "instruction"
	case "task.toml":
		return "task_toml"
	case authoringharness.DockerfileRelativePath:
		return "dockerfile"
	case authoringharness.SolveScriptRelativePath:
		return "solve_script"
	case authoringharness.TestScriptRelativePath:
		return "test_script"
	default:
		return "tests_analysis"
	}
}

type standardAuthoringV3Submission struct {
	mu                     sync.Mutex
	stage                  workflowkit.StageDescriptor
	role                   workflowkit.AgentRoleID
	taskRoot               string
	limit                  int
	maxValidationAttempts  int
	validationAttempts     int
	oneValidationPerTurn   bool
	turnValidationAttempts int
	candidateValidator     func(context.Context, workflowkit.CandidateSnapshot, map[string][]byte) (workflowkit.ValidationReceipt, error)
	repairLedger           func() ([]byte, error)
	accepted               *workflowkit.StageExecutionResult
	submissions            []workflowkit.AgentTurnSubmissionAttempt
	lastRejectionCode      string
}
type standardAuthoringV3SubmissionRequest struct {
	Verdict   string `json:"verdict"`
	Artifacts []struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	} `json:"artifacts,omitempty"`
}

func newStandardAuthoringV3Submission(stage workflowkit.StageDescriptor, role workflowkit.AgentRoleID, taskRoot string, limit int) *standardAuthoringV3Submission {
	return &standardAuthoringV3Submission{stage: stage, role: role, taskRoot: taskRoot, limit: limit}
}

func (submission *standardAuthoringV3Submission) beginTurn() {
	submission.mu.Lock()
	defer submission.mu.Unlock()
	submission.turnValidationAttempts = 0
}

func (submission *standardAuthoringV3Submission) dynamicTool() agent.DynamicTool {
	name := standardAuthoringV3SubmitOutputTool
	schema := json.RawMessage(standardAuthoringV3AgentOutputSchemaCanonicalJSON)
	description := "Required terminal submission for this frozen stage. A prose response never completes the stage. Call with verdict pass and one raw content artifact for every declared output, using exact output names. If accepted:false is returned, correct the payload and call again in the same or a later turn."
	if submission.role == workflowkit.AgentRoleAuthor {
		name = standardAuthoringV3ValidateTool
		schema = json.RawMessage(`{"additionalProperties":false,"properties":{"verdict":{"const":"pass"}},"required":["verdict"],"type":"object"}`)
		description = "Required candidate validation for this frozen stage. After writing the fixed candidate files in task/, call with {\"verdict\":\"pass\"} and no artifacts. A rejected validation returns bounded diagnostics; a passing validation completes the stage."
		if submission.oneValidationPerTurn {
			description += " This repair stage permits one candidate validation per agent turn. After a rejected receipt, wait for the next prompt before validating another correction."
		}
	} else {
		names := make([]string, 0, len(submission.stage.Outputs))
		for _, output := range submission.stage.Outputs {
			names = append(names, output.Name)
		}
		description = fmt.Sprintf("Required terminal submission for this frozen stage. A prose response never completes the stage. Call with verdict pass and one raw content artifact for every declared output. If accepted:false is returned, correct the payload and call again in the same or a later turn. Required output names: %s.", strings.Join(names, ", "))
	}
	return agent.DynamicTool{Name: name, Description: description, InputSchema: schema, Handler: submission.handleAndRecord}
}

func (submission *standardAuthoringV3Submission) handleAndRecord(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	response, err := submission.handle(ctx, raw)
	submission.recordSubmission(raw, response, err)
	return response, err
}

func (submission *standardAuthoringV3Submission) handle(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	submission.mu.Lock()
	defer submission.mu.Unlock()
	submission.lastRejectionCode = ""
	if submission.accepted != nil {
		submission.lastRejectionCode = "already_accepted"
		return json.RawMessage(`{"accepted":false,"reason":"already_accepted"}`), nil
	}
	if err := rejectDuplicateDeploymentCatalogJSONKeys(raw); err != nil {
		submission.lastRejectionCode = "typed_artifact_invalid"
		return json.RawMessage(`{"accepted":false,"reason":"invalid_payload"}`), nil
	}
	var request standardAuthoringV3SubmissionRequest
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Verdict != "pass" {
		submission.lastRejectionCode = "typed_artifact_invalid"
		return json.RawMessage(`{"accepted":false,"reason":"invalid_payload"}`), nil
	}
	if submission.role == workflowkit.AgentRoleAuthor {
		if len(request.Artifacts) != 0 {
			submission.lastRejectionCode = "typed_artifact_invalid"
			return json.RawMessage(`{"accepted":false,"reason":"candidate_tool_does_not_accept_artifacts"}`), nil
		}
		result, snapshot, files, err := submission.captureCandidate()
		if err != nil {
			submission.lastRejectionCode = "typed_artifact_invalid"
			return json.RawMessage(`{"accepted":false,"reason":"candidate_invalid"}`), nil
		}
		if submission.candidateValidator != nil {
			if submission.maxValidationAttempts <= 0 || submission.validationAttempts >= submission.maxValidationAttempts {
				submission.lastRejectionCode = "repair_budget_exhausted"
				return json.RawMessage(`{"accepted":false,"reason":"repair_budget_exhausted"}`), nil
			}
			if submission.oneValidationPerTurn && submission.turnValidationAttempts >= 1 {
				submission.lastRejectionCode = "validation_turn_limit_reached"
				return json.RawMessage(`{"accepted":false,"reason":"validation_turn_limit_reached"}`), nil
			}
			submission.turnValidationAttempts++
			receipt, validationErr := submission.candidateValidator(ctx, snapshot, files)
			if validationErr != nil {
				submission.lastRejectionCode = "validator_unavailable"
				return json.RawMessage(`{"accepted":false,"reason":"validator_unavailable"}`), nil
			}
			submission.validationAttempts++
			if err := receipt.Validate(); err != nil || receipt.SnapshotDigest != snapshot.Digest {
				submission.lastRejectionCode = "validator_unavailable"
				return json.RawMessage(`{"accepted":false,"reason":"validator_unavailable"}`), nil
			}
			if receipt.Verdict != workflowkit.ValidationPass {
				submission.lastRejectionCode = "candidate_rejected"
				return standardAuthoringV3ValidationToolResponse(false, "candidate_rejected", snapshot.Digest, receipt), nil
			}
			receiptJSON, err := json.Marshal(receipt)
			if err != nil {
				submission.lastRejectionCode = "validator_unavailable"
				return json.RawMessage(`{"accepted":false,"reason":"validator_unavailable"}`), nil
			}
			result.Artifacts = append(result.Artifacts, workflowkit.StageArtifact{Name: "validation_receipt", SchemaVersion: workflowkit.ValidationReceiptFormat, Content: receiptJSON})
		}
		if submission.repairLedger != nil {
			ledger, err := submission.repairLedger()
			if err != nil {
				submission.lastRejectionCode = "repair_ledger_invalid"
				return json.RawMessage(`{"accepted":false,"reason":"repair_ledger_invalid"}`), nil
			}
			result.Artifacts = append(result.Artifacts, workflowkit.StageArtifact{Name: "workflow_repair_ledger", SchemaVersion: workflowkit.WorkflowRepairLedgerFormat, Content: ledger})
		}
		submission.accepted = &result
		response, _ := json.Marshal(struct {
			Accepted    bool                    `json:"accepted"`
			Snapshot    workflowkit.Fingerprint `json:"snapshot_digest"`
			Diagnostics []string                `json:"diagnostics"`
		}{Accepted: true, Snapshot: snapshot.Digest, Diagnostics: []string{}})
		return response, nil
	}
	if len(request.Artifacts) == 0 {
		submission.lastRejectionCode = "empty_submission"
		return json.RawMessage(`{"accepted":false,"reason":"structured_output_required"}`), nil
	}
	result, err := submission.captureStructured(request)
	if err != nil {
		rejectionCode := standardAuthoringV3StructuredRejectionCode(err)
		submission.lastRejectionCode = rejectionCode
		return standardAuthoringV3StructuredToolResponse(false, "structured_output_invalid", rejectionCode), nil
	}
	submission.accepted = &result
	return json.RawMessage(`{"accepted":true}`), nil
}

func (submission *standardAuthoringV3Submission) recordSubmission(raw, receipt json.RawMessage, handlerErr error) {
	attempt := workflowkit.AgentTurnSubmissionAttempt{
		RawRequestJSON: string(append(json.RawMessage(nil), raw...)),
		ReceiptJSON:    string(append(json.RawMessage(nil), receipt...)),
	}
	if handlerErr != nil {
		attempt.Status = workflowkit.AgentTurnSubmissionRuntimeError
		attempt.RejectionCode = "tool_error"
	} else {
		var response struct {
			Accepted bool   `json:"accepted"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal(receipt, &response); err != nil {
			attempt.Status = workflowkit.AgentTurnSubmissionRuntimeError
			attempt.RejectionCode = "tool_error"
		} else if response.Accepted {
			attempt.Status = workflowkit.AgentTurnSubmissionAccepted
		} else {
			attempt.Status = workflowkit.AgentTurnSubmissionRejected
			attempt.RejectionCode = strings.TrimSpace(response.Reason)
			if attempt.RejectionCode == "" {
				attempt.RejectionCode = "tool_rejected"
			}
		}
	}
	validation, err := json.Marshal(struct {
		Accepted      bool   `json:"accepted"`
		RejectionCode string `json:"rejection_code,omitempty"`
	}{Accepted: attempt.Status == workflowkit.AgentTurnSubmissionAccepted, RejectionCode: attempt.RejectionCode})
	if err != nil {
		validation = []byte(`{"accepted":false,"rejection_code":"tool_error"}`)
	}
	attempt.ValidationJSON = string(validation)

	submission.mu.Lock()
	defer submission.mu.Unlock()
	if attempt.Status == workflowkit.AgentTurnSubmissionRejected && submission.lastRejectionCode != "" {
		attempt.RejectionCode = submission.lastRejectionCode
		if validation, err := json.Marshal(struct {
			Accepted      bool   `json:"accepted"`
			RejectionCode string `json:"rejection_code,omitempty"`
		}{Accepted: false, RejectionCode: attempt.RejectionCode}); err == nil {
			attempt.ValidationJSON = string(validation)
		}
	}
	submission.lastRejectionCode = ""
	submission.submissions = append(submission.submissions, attempt)
}

func (submission *standardAuthoringV3Submission) submissionCount() int {
	submission.mu.Lock()
	defer submission.mu.Unlock()
	return len(submission.submissions)
}

func (submission *standardAuthoringV3Submission) transcriptSince(start int, result agent.TurnResult, expectedModel, failureCode string) workflowkit.AgentTurnTranscript {
	submission.mu.Lock()
	defer submission.mu.Unlock()
	if start < 0 || start > len(submission.submissions) {
		start = len(submission.submissions)
	}
	attempts := append([]workflowkit.AgentTurnSubmissionAttempt(nil), submission.submissions[start:]...)
	status := workflowkit.AgentTurnSubmissionNotSubmitted
	if standardAuthoringV3RuntimeFailure(failureCode) {
		status = workflowkit.AgentTurnSubmissionRuntimeError
	} else if submission.accepted != nil {
		status = workflowkit.AgentTurnSubmissionAccepted
	} else if len(attempts) > 0 {
		status = workflowkit.AgentTurnSubmissionRejected
	}
	modelID := strings.TrimSpace(result.Model)
	if modelID == "" {
		modelID = expectedModel
	}
	transcript := workflowkit.AgentTurnTranscript{
		ResponseText: result.Text, ModelID: modelID, SubmissionStatus: status, Submissions: attempts, FailureCode: failureCode,
	}
	for index := len(attempts) - 1; index >= 0; index-- {
		if attempts[index].RejectionCode != "" {
			transcript.ProtocolRejectionCode = attempts[index].RejectionCode
			break
		}
	}
	if IsStandardAuthoringProtocolFailure(failureCode) {
		transcript.ProtocolRejectionCode = failureCode
	}
	return transcript
}

func (submission *standardAuthoringV3Submission) terminalFailureCode() string {
	submission.mu.Lock()
	defer submission.mu.Unlock()
	if len(submission.submissions) == 0 {
		return StandardAuthoringProtocolFailureMissingSubmission
	}
	for index := len(submission.submissions) - 1; index >= 0; index-- {
		switch submission.submissions[index].RejectionCode {
		case "empty_submission", "structured_output_required":
			return StandardAuthoringProtocolFailureEmptySubmission
		case "undeclared_output", "unexpected_output", "duplicate_output":
			return StandardAuthoringProtocolFailureUndeclaredOutput
		case "typed_artifact_invalid", "structured_output_invalid", "candidate_invalid", "candidate_tool_does_not_accept_artifacts", "invalid_payload":
			return StandardAuthoringProtocolFailureTypedArtifactInvalid
		case "validator_unavailable":
			return standardAuthoringV3AgentProtocolPrefix + "validator_unavailable"
		case "candidate_rejected":
			return standardAuthoringV3AgentProtocolPrefix + "candidate_rejected"
		case "repair_budget_exhausted":
			return standardAuthoringV3AgentProtocolPrefix + "repair_budget_exhausted"
		case "validation_turn_limit_reached":
			return standardAuthoringV3AgentProtocolPrefix + "candidate_rejected"
		case "repair_ledger_invalid":
			return standardAuthoringV3AgentProtocolPrefix + "repair_ledger_invalid"
		case "tool_error":
			return standardAuthoringCodexFailureRuntime
		}
	}
	return StandardAuthoringProtocolFailureTypedArtifactInvalid
}

func standardAuthoringV3RuntimeFailure(code string) bool {
	return strings.TrimSpace(code) == standardAuthoringCodexFailureRuntime
}

func standardAuthoringV3ValidationToolResponse(accepted bool, reason string, snapshot workflowkit.Fingerprint, receipt workflowkit.ValidationReceipt) json.RawMessage {
	type diagnostic struct {
		CommandID   string `json:"command_id"`
		ExitCode    int    `json:"exit_code"`
		TestStarted bool   `json:"test_started"`
		StdoutTail  string `json:"stdout_tail,omitempty"`
		StderrTail  string `json:"stderr_tail,omitempty"`
	}
	response := struct {
		Accepted       bool                                                      `json:"accepted"`
		Reason         string                                                    `json:"reason,omitempty"`
		Snapshot       workflowkit.Fingerprint                                   `json:"snapshot_digest"`
		FailureCode    workflowkit.AgentFailureCode                              `json:"failure_code,omitempty"`
		Diagnostics    []diagnostic                                              `json:"diagnostics"`
		RepairGuidance []string                                                  `json:"repair_guidance,omitempty"`
		RepairContext  *workflowadapter.StandardAuthoringValidationRepairContext `json:"validation_repair_context,omitempty"`
	}{Accepted: accepted, Reason: reason, Snapshot: snapshot, FailureCode: receipt.FailureCode, Diagnostics: make([]diagnostic, 0, len(receipt.Diagnostics))}
	for _, item := range receipt.Diagnostics {
		response.Diagnostics = append(response.Diagnostics, diagnostic{
			CommandID: item.CommandID, ExitCode: item.ExitCode, TestStarted: item.TestStarted,
			StdoutTail: item.StdoutTail, StderrTail: item.StderrTail,
		})
	}
	if !accepted {
		response.RepairGuidance = standardAuthoringV3RepairGuidance(receipt)
		if repairContext, err := workflowadapter.NewStandardAuthoringValidationRepairContext(receipt, standardAuthoringV3EditableCandidatePaths()); err == nil {
			response.RepairContext = &repairContext
		}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return json.RawMessage(`{"accepted":false,"reason":"validator_unavailable"}`)
	}
	return encoded
}

func standardAuthoringV3RepairGuidance(receipt workflowkit.ValidationReceipt) []string {
	failed, found := standardAuthoringV3FailedDiagnostic(receipt.Diagnostics)
	if !found {
		return []string{"Read validation_repair_context and repair only the fixed candidate files under task/ before calling harbor_validate_candidate again."}
	}
	text := strings.ToLower(failed.CommandID + "\n" + failed.StdoutTail + "\n" + failed.StderrTail)
	guidance := make([]string, 0, 4)
	if strings.Contains(text, "permission denied") || strings.Contains(text, "read-only file system") {
		if strings.Contains(text, "/source") || strings.Contains(text, "source") {
			guidance = append(guidance, "Do not write frozen source/ or validator /source; write candidate files under task/ and have solution/test scripts modify the writable /work copy.")
		} else {
			guidance = append(guidance, "Use writable runtime paths such as /work or /tmp/harbor-*; the validator root filesystem and /task are read-only.")
		}
	}
	if standardAuthoringV3LooksLikeNetworkFailure(text) {
		guidance = append(guidance, "Runtime validation has --network none; prepare generic tools in environment/Dockerfile during Docker build and use only base-image, vendored, or frozen-input dependencies at runtime.")
	}
	if standardAuthoringV3LooksLikeRustCargoBrowserWASM(text) {
		guidance = append(guidance, "For Rust/Cargo browser-wasm validation, set CARGO_HOME and CARGO_TARGET_DIR to writable executable paths such as /work/.cargo-home and /work/.cargo-target; do not copy full Cargo registries or target caches into limited /tmp.")
	}
	switch failed.CommandID {
	case "environment_build":
		guidance = append(guidance, "Fix provisioning in environment/Dockerfile. Its Docker build context is environment/, so it cannot read the frozen source tree.")
	case "source_access":
		guidance = append(guidance, "The validator must be able to copy /source into writable /work; avoid source archive modes or Dockerfile user changes that make /source unreadable after capabilities are dropped.")
	case "baseline_verify", "oracle_verify", "coverage_verify":
		guidance = append(guidance, "Keep the VerificationContract command unchanged; repair solution/solve.sh or tests/test.sh so they run against the writable /work checkout.")
	}
	if len(guidance) == 0 {
		guidance = append(guidance, "Use validation_repair_context, stdout_tail, and stderr_tail to make one focused candidate-file correction before validating again.")
	}
	return guidance
}

func standardAuthoringV3FailedDiagnostic(diagnostics []workflowkit.AgentCommandReport) (workflowkit.AgentCommandReport, bool) {
	if len(diagnostics) == 0 {
		return workflowkit.AgentCommandReport{}, false
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.ExitCode != 0 {
			return diagnostic, true
		}
	}
	return diagnostics[len(diagnostics)-1], true
}

func standardAuthoringV3LooksLikeNetworkFailure(text string) bool {
	for _, token := range []string{
		"could not resolve", "temporary failure resolving", "network is unreachable", "no route to host",
		"connection timed out", "failed to download", "unable to download", "download failed",
		"crates.io", "git fetch", "npm error", "playwright install",
	} {
		if strings.Contains(text, token) {
			return true
		}
	}
	return false
}

func standardAuthoringV3LooksLikeRustCargoBrowserWASM(text string) bool {
	for _, token := range []string{"cargo", "rustc", "wasm", "wasm-pack", "wasm-bindgen", "trunk", "playwright"} {
		if strings.Contains(text, token) {
			return true
		}
	}
	return false
}

func standardAuthoringV3EditableCandidatePaths() []string {
	return []string{
		"instruction.md",
		"task.toml",
		authoringharness.DockerfileRelativePath,
		authoringharness.SolveScriptRelativePath,
		authoringharness.TestScriptRelativePath,
		"tests_analysis.json",
	}
}

func standardAuthoringV3StructuredToolResponse(accepted bool, reason, rejectionCode string) json.RawMessage {
	response := struct {
		Accepted      bool   `json:"accepted"`
		Reason        string `json:"reason,omitempty"`
		RejectionCode string `json:"rejection_code,omitempty"`
		Instruction   string `json:"instruction,omitempty"`
	}{
		Accepted:      accepted,
		Reason:        reason,
		RejectionCode: rejectionCode,
	}
	if !accepted {
		response.Instruction = "Correct the structured artifact payload and call harbor_submit_output again in the same or a later turn. Only an accepted:true receipt completes the stage."
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return json.RawMessage(`{"accepted":false,"reason":"structured_output_invalid"}`)
	}
	return encoded
}
func (submission *standardAuthoringV3Submission) acceptedResult() (workflowkit.StageExecutionResult, bool) {
	if submission.accepted == nil {
		return workflowkit.StageExecutionResult{}, false
	}
	return *submission.accepted, true
}

type standardAuthoringV3StructuredSubmissionError struct{ rejectionCode string }

func (err standardAuthoringV3StructuredSubmissionError) Error() string { return err.rejectionCode }

func standardAuthoringV3StructuredRejectionCode(err error) string {
	if typed, ok := err.(standardAuthoringV3StructuredSubmissionError); ok {
		return typed.rejectionCode
	}
	return "typed_artifact_invalid"
}

func (submission *standardAuthoringV3Submission) captureStructured(request standardAuthoringV3SubmissionRequest) (workflowkit.StageExecutionResult, error) {
	if len(request.Artifacts) != len(submission.stage.Outputs) {
		return workflowkit.StageExecutionResult{}, standardAuthoringV3StructuredSubmissionError{rejectionCode: "undeclared_output"}
	}
	expected := make(map[string]workflowkit.ArtifactSpec, len(submission.stage.Outputs))
	for _, output := range submission.stage.Outputs {
		expected[output.Name] = output
	}
	artifacts := make([]workflowkit.StageArtifact, 0, len(request.Artifacts))
	seen := map[string]struct{}{}
	for _, item := range request.Artifacts {
		output, found := expected[item.Name]
		if !found {
			return workflowkit.StageExecutionResult{}, standardAuthoringV3StructuredSubmissionError{rejectionCode: "undeclared_output"}
		}
		if len(item.Content) == 0 || len(item.Content) > submission.limit {
			return workflowkit.StageExecutionResult{}, standardAuthoringV3StructuredSubmissionError{rejectionCode: "typed_artifact_invalid"}
		}
		if _, duplicate := seen[item.Name]; duplicate {
			return workflowkit.StageExecutionResult{}, standardAuthoringV3StructuredSubmissionError{rejectionCode: "undeclared_output"}
		}
		if err := standardAuthoringV3ValidateStructuredOutput(output, []byte(item.Content)); err != nil {
			return workflowkit.StageExecutionResult{}, standardAuthoringV3StructuredSubmissionError{rejectionCode: "typed_artifact_invalid"}
		}
		seen[item.Name] = struct{}{}
		artifacts = append(artifacts, workflowkit.StageArtifact{Name: item.Name, SchemaVersion: output.SchemaVersion, Content: []byte(item.Content)})
	}
	return workflowkit.StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass}, Artifacts: artifacts}, nil
}

func standardAuthoringV3ValidateStructuredOutput(output workflowkit.ArtifactSpec, content []byte) error {
	switch output.SchemaVersion {
	case workflowkit.WorkflowFindingFormat:
		var finding workflowkit.WorkflowFinding
		if err := standardAuthoringV3DecodeTypedInput(content, &finding); err != nil {
			return fmt.Errorf("workflow finding output %q is invalid", output.Name)
		}
		if err := finding.Validate(); err != nil {
			return fmt.Errorf("workflow finding output %q is invalid", output.Name)
		}
	case workflowadapter.StandardAuthoringVerificationContractFormat:
		if output.Name != "verification_contract" {
			return fmt.Errorf("verification contract output %q is invalid", output.Name)
		}
		if _, err := workflowadapter.ParseStandardAuthoringVerificationContractJSON(content); err != nil {
			return fmt.Errorf("verification contract output %q is invalid", output.Name)
		}
	}
	return nil
}

func (submission *standardAuthoringV3Submission) captureCandidate() (workflowkit.StageExecutionResult, workflowkit.CandidateSnapshot, map[string][]byte, error) {
	if submission.taskRoot == "" {
		return workflowkit.StageExecutionResult{}, workflowkit.CandidateSnapshot{}, nil, fmt.Errorf("missing candidate workspace")
	}
	paths := []string{"instruction.md", "task.toml", authoringharness.DockerfileRelativePath, authoringharness.SolveScriptRelativePath, authoringharness.TestScriptRelativePath, "tests_analysis.json"}
	artifacts := make([]workflowkit.StageArtifact, 0, len(paths)+1)
	manifest := make([]workflowkit.CandidateFile, 0, len(paths))
	files := make(map[string][]byte, len(paths))
	for _, path := range paths {
		content, err := authoringharness.ReadFixedFile(submission.taskRoot, path)
		if err != nil || len(content) > submission.limit {
			return workflowkit.StageExecutionResult{}, workflowkit.CandidateSnapshot{}, nil, fmt.Errorf("candidate file unavailable")
		}
		name := standardAuthoringV3ArtifactName(path)
		schema := "harbor.artifact.v1"
		artifacts = append(artifacts, workflowkit.StageArtifact{Name: name, SchemaVersion: schema, Content: content})
		files[path] = append([]byte(nil), content...)
		manifest = append(manifest, workflowkit.CandidateFile{Path: path, SchemaVersion: schema, ContentDigest: workflowkit.SHA256Fingerprint(content), SizeBytes: int64(len(content))})
	}
	snapshot, err := workflowkit.NewCandidateSnapshot(manifest)
	if err != nil {
		return workflowkit.StageExecutionResult{}, workflowkit.CandidateSnapshot{}, nil, err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return workflowkit.StageExecutionResult{}, workflowkit.CandidateSnapshot{}, nil, err
	}
	artifacts = append(artifacts, workflowkit.StageArtifact{Name: "candidate_snapshot", SchemaVersion: workflowkit.CandidateSnapshotFormat, Content: raw})
	return workflowkit.StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass}, Artifacts: artifacts}, snapshot, files, nil
}

func cloneStandardAuthoringV3CandidateFiles(files map[string][]byte) map[string][]byte {
	clone := make(map[string][]byte, len(files))
	for path, content := range files {
		clone[path] = append([]byte(nil), content...)
	}
	return clone
}
