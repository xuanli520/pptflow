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

	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/internal/harbor/authoringharness"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	standardAuthoringV3SubmitOutputTool = "harbor_submit_output"
	standardAuthoringV3ValidateTool     = "harbor_validate_candidate"
	standardAuthoringV3ContextLimit     = 2 << 20
)

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
	conversation, err := runtime.OpenConversation(ctx, agent.ConversationRequest{
		ProjectPath: workspace, Model: attested.ModelID, ReasoningEffort: string(attested.ReasoningEffort),
		SandboxMode: attested.SandboxMode, SandboxPolicy: attested.SandboxPolicy, NetworkAccess: false,
		WorkspaceRoots: []string{workspace}, TimeoutSeconds: standardAuthoringCodexTimeoutSeconds(request.Stage.Budget.TurnTimeout),
		MaxOutputBytes: program.MaxOutputBytes, CapabilitySummary: attested.CLIVersionOutput, LogPath: os.DevNull,
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
		if err := executor.checkpoint(ctx, request, program, inputDigest, turn, "turn_ready", ""); err != nil {
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
		turnRequest := agent.TurnRequest{ProjectPath: workspace, Prompt: prompt, Model: attested.ModelID, ReasoningEffort: string(attested.ReasoningEffort), SandboxMode: attested.SandboxMode, SandboxPolicy: attested.SandboxPolicy, NetworkAccess: false, WorkspaceRoots: []string{workspace}, TimeoutSeconds: standardAuthoringCodexTimeoutSeconds(request.Stage.Budget.TurnTimeout), MaxOutputBytes: program.MaxOutputBytes, CapabilitySummary: attested.CLIVersionOutput, LogPath: os.DevNull}
		if turn == 1 {
			turnRequest.Input = []agent.InputPart{{Type: "text", Text: string(contextDocument)}}
		}
		result, turnErr, acceptedResult, acceptedDuringTurn, closeErr := standardAuthoringCodexRunTurnUntilAccepted(ctx, conversation, turnRequest, accepted)
		if acceptedDuringTurn {
			if closeErr != nil {
				return standardAuthoringCodexFailure(workflowkit.FailureProcess, standardAuthoringCodexFailureRuntime), nil
			}
			if currentIdentity, err := executor.verifyFrozenSource(ctx, request.Execution, sourceRoot); err != nil || currentIdentity != sourceIdentity {
				return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureSource), nil
			}
			return acceptedResult, nil
		}
		if acceptedResult, ok := submission.acceptedResult(); ok {
			if currentIdentity, err := executor.verifyFrozenSource(ctx, request.Execution, sourceRoot); err != nil || currentIdentity != sourceIdentity {
				return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureSource), nil
			}
			return acceptedResult, nil
		}
		if turnErr != nil || result.Model != attested.ModelID {
			return standardAuthoringCodexFailure(workflowkit.FailureProcess, standardAuthoringCodexFailureRuntime), nil
		}
		if err := executor.checkpoint(ctx, request, program, inputDigest, turn, "turn_completed", string(workflowkit.SHA256Fingerprint([]byte(result.Text)))); err != nil {
			if contextError(ctx) != nil {
				return standardAuthoringCodexInterrupted(), nil
			}
			return standardAuthoringCodexFailure(workflowkit.FailureUnknown, standardAuthoringCodexFailureCheckpoint), nil
		}
	}
	return standardAuthoringCodexFailure(workflowkit.FailureProcess, "standard_authoring_v3_agent_protocol.missing_submission"), nil
}

func standardAuthoringV3ValidateRepairInputs(workflow workflowkit.WorkflowDescriptor, inputs map[string][]byte) error {
	_, err := standardAuthoringV3RepairLedger(workflow, inputs)
	return err
}

func standardAuthoringV3RepairLedger(workflow workflowkit.WorkflowDescriptor, inputs map[string][]byte) ([]byte, error) {
	var snapshot workflowkit.CandidateSnapshot
	if err := standardAuthoringV3DecodeTypedInput(inputs["candidate_snapshot"], &snapshot); err != nil || snapshot.Validate() != nil {
		return nil, fmt.Errorf("candidate snapshot is invalid")
	}
	var receipt workflowkit.ValidationReceipt
	if err := standardAuthoringV3DecodeTypedInput(inputs["validation_receipt"], &receipt); err != nil || receipt.Validate() != nil || (receipt.Verdict != workflowkit.ValidationPass && receipt.Verdict != workflowkit.ValidationReject) || receipt.SnapshotDigest != snapshot.Digest {
		return nil, fmt.Errorf("validation receipt is not a current candidate receipt")
	}
	rules := []workflowkit.WorkflowRepairRule{
		{FindingCode: "test_quality_defect", ProducingStage: workflowkit.StageKey(workflowadapter.TestQualityCritic), TargetWriter: workflowkit.StageKey(workflowadapter.AuthoringRepair), RequiresCandidateSnapshot: true, ConsumesCandidateRepair: true},
		{FindingCode: "solution_integrity_defect", ProducingStage: workflowkit.StageKey(workflowadapter.SolutionIntegrityCritic), TargetWriter: workflowkit.StageKey(workflowadapter.AuthoringRepair), RequiresCandidateSnapshot: true, ConsumesCandidateRepair: true},
	}
	entries := make([]workflowkit.WorkflowRepairLedgerEntry, 0, 2)
	chargedTargets := make(map[workflowkit.StageKey]struct{})
	for _, name := range []string{"test_quality_finding", "solution_integrity_finding"} {
		var finding workflowkit.WorkflowFinding
		if err := standardAuthoringV3DecodeTypedInput(inputs[name], &finding); err != nil || finding.Validate() != nil {
			return nil, fmt.Errorf("finding %q is invalid", name)
		}
		if finding.CandidateDigest != snapshot.Digest || finding.EvidenceDigest != receipt.Digest || finding.DiagnosticDigest != receipt.Digest {
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
		Instructions: "Call this tool exactly once with verdict pass and one raw content artifact for every required output. " +
			"A prose final answer never completes this stage.",
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
	document := struct {
		Format             string                                `json:"format"`
		Version            string                                `json:"version"`
		Stage              string                                `json:"stage"`
		Program            workflowkit.Fingerprint               `json:"program_fingerprint"`
		Inputs             workflowkit.Fingerprint               `json:"inputs_fingerprint"`
		CandidateWorkspace bool                                  `json:"candidate_workspace"`
		TerminalSubmission standardAuthoringV3TerminalSubmission `json:"terminal_submission"`
		Artifacts          []standardAuthoringV3Input            `json:"artifacts"`
	}{Format: "harbor.standard-authoring-v3-context.v1", Version: "1", Stage: string(request.Stage.Key), Program: program.Fingerprint, Inputs: digest, CandidateWorkspace: candidateWriter, TerminalSubmission: submission, Artifacts: entries}
	raw, err := json.Marshal(document)
	if err != nil || len(raw) > standardAuthoringV3ContextLimit {
		return nil, fmt.Errorf("context document is invalid")
	}
	return raw, nil
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
	stage                 workflowkit.StageDescriptor
	role                  workflowkit.AgentRoleID
	taskRoot              string
	limit                 int
	maxValidationAttempts int
	validationAttempts    int
	candidateValidator    func(context.Context, workflowkit.CandidateSnapshot, map[string][]byte) (workflowkit.ValidationReceipt, error)
	repairLedger          func() ([]byte, error)
	accepted              *workflowkit.StageExecutionResult
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
func (submission *standardAuthoringV3Submission) dynamicTool() agent.DynamicTool {
	name := standardAuthoringV3SubmitOutputTool
	schema := json.RawMessage(standardAuthoringV3AgentOutputSchemaCanonicalJSON)
	description := "Required terminal submission for this frozen stage. A prose response never completes the stage. Call exactly once with verdict pass and one raw content artifact for every declared output, using exact output names."
	if submission.role == workflowkit.AgentRoleAuthor {
		name = standardAuthoringV3ValidateTool
		schema = json.RawMessage(`{"additionalProperties":false,"properties":{"verdict":{"const":"pass"}},"required":["verdict"],"type":"object"}`)
		description = "Required candidate validation for this frozen stage. After writing the fixed candidate files in task/, call with {\"verdict\":\"pass\"} and no artifacts. A rejected validation returns bounded diagnostics; a passing validation completes the stage."
	} else {
		names := make([]string, 0, len(submission.stage.Outputs))
		for _, output := range submission.stage.Outputs {
			names = append(names, output.Name)
		}
		description = fmt.Sprintf("Required terminal submission for this frozen stage. A prose response never completes the stage. Call exactly once with verdict pass and one raw content artifact for every declared output. Required output names: %s.", strings.Join(names, ", "))
	}
	return agent.DynamicTool{Name: name, Description: description, InputSchema: schema, Handler: submission.handle}
}
func (submission *standardAuthoringV3Submission) handle(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if submission.accepted != nil {
		return json.RawMessage(`{"accepted":false,"reason":"already_accepted"}`), nil
	}
	if err := rejectDuplicateDeploymentCatalogJSONKeys(raw); err != nil {
		return json.RawMessage(`{"accepted":false,"reason":"invalid_payload"}`), nil
	}
	var request standardAuthoringV3SubmissionRequest
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Verdict != "pass" {
		return json.RawMessage(`{"accepted":false,"reason":"invalid_payload"}`), nil
	}
	if submission.role == workflowkit.AgentRoleAuthor {
		if len(request.Artifacts) != 0 {
			return json.RawMessage(`{"accepted":false,"reason":"candidate_tool_does_not_accept_artifacts"}`), nil
		}
		result, snapshot, files, err := submission.captureCandidate()
		if err != nil {
			return json.RawMessage(`{"accepted":false,"reason":"candidate_invalid"}`), nil
		}
		if submission.candidateValidator != nil {
			if submission.maxValidationAttempts <= 0 || submission.validationAttempts >= submission.maxValidationAttempts {
				return json.RawMessage(`{"accepted":false,"reason":"repair_budget_exhausted"}`), nil
			}
			receipt, validationErr := submission.candidateValidator(ctx, snapshot, files)
			if validationErr != nil {
				return json.RawMessage(`{"accepted":false,"reason":"validator_unavailable"}`), nil
			}
			submission.validationAttempts++
			if err := receipt.Validate(); err != nil || receipt.SnapshotDigest != snapshot.Digest {
				return json.RawMessage(`{"accepted":false,"reason":"validator_unavailable"}`), nil
			}
			if receipt.Verdict != workflowkit.ValidationPass {
				return standardAuthoringV3ValidationToolResponse(false, "candidate_rejected", snapshot.Digest, receipt), nil
			}
			receiptJSON, err := json.Marshal(receipt)
			if err != nil {
				return json.RawMessage(`{"accepted":false,"reason":"validator_unavailable"}`), nil
			}
			result.Artifacts = append(result.Artifacts, workflowkit.StageArtifact{Name: "validation_receipt", SchemaVersion: workflowkit.ValidationReceiptFormat, Content: receiptJSON})
		}
		if submission.repairLedger != nil {
			ledger, err := submission.repairLedger()
			if err != nil {
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
		return json.RawMessage(`{"accepted":false,"reason":"structured_output_required"}`), nil
	}
	result, err := submission.captureStructured(request)
	if err != nil {
		return json.RawMessage(`{"accepted":false,"reason":"structured_output_invalid"}`), nil
	}
	submission.accepted = &result
	return json.RawMessage(`{"accepted":true}`), nil
}

func standardAuthoringV3ValidationToolResponse(accepted bool, reason string, snapshot workflowkit.Fingerprint, receipt workflowkit.ValidationReceipt) json.RawMessage {
	type diagnostic struct {
		CommandID   string `json:"command_id"`
		ExitCode    int    `json:"exit_code"`
		TestStarted bool   `json:"test_started"`
	}
	response := struct {
		Accepted    bool                         `json:"accepted"`
		Reason      string                       `json:"reason,omitempty"`
		Snapshot    workflowkit.Fingerprint      `json:"snapshot_digest"`
		FailureCode workflowkit.AgentFailureCode `json:"failure_code,omitempty"`
		Diagnostics []diagnostic                 `json:"diagnostics"`
	}{Accepted: accepted, Reason: reason, Snapshot: snapshot, FailureCode: receipt.FailureCode, Diagnostics: make([]diagnostic, 0, len(receipt.Diagnostics))}
	for _, item := range receipt.Diagnostics {
		response.Diagnostics = append(response.Diagnostics, diagnostic{CommandID: item.CommandID, ExitCode: item.ExitCode, TestStarted: item.TestStarted})
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return json.RawMessage(`{"accepted":false,"reason":"validator_unavailable"}`)
	}
	return encoded
}
func (submission *standardAuthoringV3Submission) acceptedResult() (workflowkit.StageExecutionResult, bool) {
	if submission.accepted == nil {
		return workflowkit.StageExecutionResult{}, false
	}
	return *submission.accepted, true
}
func (submission *standardAuthoringV3Submission) captureStructured(request standardAuthoringV3SubmissionRequest) (workflowkit.StageExecutionResult, error) {
	if len(request.Artifacts) != len(submission.stage.Outputs) {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("unexpected output count")
	}
	expected := make(map[string]workflowkit.ArtifactSpec, len(submission.stage.Outputs))
	for _, output := range submission.stage.Outputs {
		expected[output.Name] = output
	}
	artifacts := make([]workflowkit.StageArtifact, 0, len(request.Artifacts))
	seen := map[string]struct{}{}
	for _, item := range request.Artifacts {
		output, found := expected[item.Name]
		if !found || len(item.Content) == 0 || len(item.Content) > submission.limit {
			return workflowkit.StageExecutionResult{}, fmt.Errorf("invalid structured output")
		}
		if _, duplicate := seen[item.Name]; duplicate {
			return workflowkit.StageExecutionResult{}, fmt.Errorf("duplicate output")
		}
		seen[item.Name] = struct{}{}
		artifacts = append(artifacts, workflowkit.StageArtifact{Name: item.Name, SchemaVersion: output.SchemaVersion, Content: []byte(item.Content)})
	}
	return workflowkit.StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass}, Artifacts: artifacts}, nil
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
