package stageprovider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/internal/harbor/authoringharness"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringV3ExecutorPersistsProseOnlyTranscript(t *testing.T) {
	executor, invocation, payload, checkpoints, opened := standardAuthoringV3TranscriptExecutor(t, standardAuthoringV3TranscriptRuntime{result: agent.TurnResult{Text: "I inspected the repository but will not submit.", Model: CodexAppServerProductionModelID}})
	result, err := executor.ExecuteAgentTurn(context.Background(), invocation, payload)
	if err != nil {
		t.Fatalf("execute prose-only turn: %v", err)
	}
	if result.ErrorText != StandardAuthoringProtocolFailureMissingSubmission {
		t.Fatalf("prose-only failure = %q, want %q", result.ErrorText, StandardAuthoringProtocolFailureMissingSubmission)
	}
	completed := standardAuthoringV3CompletedTranscript(t, checkpoints())
	if completed.ResponseText != "I inspected the repository but will not submit." || completed.ModelID != CodexAppServerProductionModelID || completed.SubmissionStatus != workflowkit.AgentTurnSubmissionNotSubmitted || completed.FailureCode != StandardAuthoringProtocolFailureMissingSubmission || completed.ProtocolRejectionCode != StandardAuthoringProtocolFailureMissingSubmission || len(completed.Submissions) != 0 {
		t.Fatalf("prose-only transcript = %+v", completed)
	}
	if logPath := opened().LogPath; logPath == "" || logPath == os.DevNull {
		t.Fatalf("Agent turn log path = %q, want controlled non-null path", logPath)
	}
}

func TestStandardAuthoringV3ExecutorPersistsRejectedSubmissionDiagnostics(t *testing.T) {
	raw := json.RawMessage(`{"verdict":"pass","artifacts":[{"name":"unknown_output","content":"not allowed"}]}`)
	executor, invocation, payload, checkpoints, _ := standardAuthoringV3TranscriptExecutor(t, standardAuthoringV3TranscriptRuntime{
		result: agent.TurnResult{Text: "submitted an output", Model: CodexAppServerProductionModelID}, rawSubmission: raw,
	})
	result, err := executor.ExecuteAgentTurn(context.Background(), invocation, payload)
	if err != nil {
		t.Fatalf("execute rejected submission turn: %v", err)
	}
	if result.ErrorText != StandardAuthoringProtocolFailureUndeclaredOutput {
		t.Fatalf("rejected submission failure = %q, want %q", result.ErrorText, StandardAuthoringProtocolFailureUndeclaredOutput)
	}
	completed := standardAuthoringV3CompletedTranscript(t, checkpoints())
	if completed.SubmissionStatus != workflowkit.AgentTurnSubmissionRejected || completed.ResponseText != "submitted an output" || completed.FailureCode != StandardAuthoringProtocolFailureUndeclaredOutput || len(completed.Submissions) != 1 {
		t.Fatalf("rejected submission transcript = %+v", completed)
	}
	submission := completed.Submissions[0]
	if submission.Status != workflowkit.AgentTurnSubmissionRejected || submission.RawRequestJSON != string(raw) || submission.RejectionCode != "undeclared_output" || !json.Valid([]byte(submission.ValidationJSON)) {
		t.Fatalf("rejected submission diagnostic = %+v", submission)
	}
	assertStandardAuthoringV3StructuredRejected(t, json.RawMessage(submission.ReceiptJSON), "structured_output_invalid", "undeclared_output")
	var validation struct {
		Accepted      bool   `json:"accepted"`
		RejectionCode string `json:"rejection_code"`
	}
	if err := json.Unmarshal([]byte(submission.ValidationJSON), &validation); err != nil || validation.Accepted || validation.RejectionCode != "undeclared_output" {
		t.Fatalf("rejected submission validation = %+v, %v", validation, err)
	}
}

func TestStandardAuthoringV3ExecutorPersistsRuntimeTurnFailure(t *testing.T) {
	executor, invocation, payload, checkpoints, _ := standardAuthoringV3TranscriptExecutor(t, standardAuthoringV3TranscriptRuntime{turnErr: errors.New("runtime unavailable")})
	result, err := executor.ExecuteAgentTurn(context.Background(), invocation, payload)
	if err != nil {
		t.Fatalf("execute failing turn: %v", err)
	}
	if result.ErrorText != standardAuthoringCodexFailureRuntime {
		t.Fatalf("runtime failure = %q, want %q", result.ErrorText, standardAuthoringCodexFailureRuntime)
	}
	completed := standardAuthoringV3CompletedTranscript(t, checkpoints())
	if completed.ResponseText != "" || completed.ModelID != CodexAppServerProductionModelID || completed.SubmissionStatus != workflowkit.AgentTurnSubmissionRuntimeError || completed.FailureCode != standardAuthoringCodexFailureRuntime || len(completed.Submissions) != 0 {
		t.Fatalf("runtime transcript = %+v", completed)
	}
}

func TestStandardAuthoringV3ExecutorPersistsCompletedTurnAfterExecutionContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := standardAuthoringV3TranscriptRuntime{
		result:    agent.TurnResult{Text: "the turn completed before cancellation", Model: CodexAppServerProductionModelID},
		afterTurn: cancel,
	}
	executor, invocation, payload, checkpoints, _ := standardAuthoringV3TranscriptExecutor(t, runtime)
	checkpointContextWasLive := false
	originalCheckpoint := invocation.Request.Checkpoint
	invocation.Request.Checkpoint = func(checkpointCtx context.Context, checkpoint workflowkit.StageCheckpoint) (workflowkit.CheckpointReceipt, error) {
		if checkpoint.Substep == "turn_completed" {
			checkpointContextWasLive = checkpointCtx.Err() == nil
		}
		return originalCheckpoint(checkpointCtx, checkpoint)
	}

	result, err := executor.ExecuteAgentTurn(ctx, invocation, payload)
	if err != nil {
		t.Fatalf("execute canceled-after-turn: %v", err)
	}
	if result.ErrorText != StandardAuthoringProtocolFailureMissingSubmission || !checkpointContextWasLive {
		t.Fatalf("canceled-after-turn result=%+v checkpointContextWasLive=%t", result, checkpointContextWasLive)
	}
	completed := standardAuthoringV3CompletedTranscript(t, checkpoints())
	if completed.ResponseText != "the turn completed before cancellation" || completed.FailureCode != StandardAuthoringProtocolFailureMissingSubmission {
		t.Fatalf("canceled-after-turn transcript = %+v", completed)
	}
}

func TestIsStandardAuthoringProtocolFailureIsNarrow(t *testing.T) {
	for _, code := range []string{
		StandardAuthoringProtocolFailureMissingSubmission,
		StandardAuthoringProtocolFailureEmptySubmission,
		StandardAuthoringProtocolFailureUndeclaredOutput,
		StandardAuthoringProtocolFailureTypedArtifactInvalid,
	} {
		if !IsStandardAuthoringProtocolFailure(code) {
			t.Fatalf("protocol failure %q was not eligible", code)
		}
	}
	for _, code := range []string{
		standardAuthoringCodexFailureRuntime,
		standardAuthoringCodexFailureSource,
		standardAuthoringCodexFailureQuota,
		standardAuthoringV3AgentProtocolPrefix + "validator_unavailable",
	} {
		if IsStandardAuthoringProtocolFailure(code) {
			t.Fatalf("non-protocol failure %q became eligible", code)
		}
	}
}

type standardAuthoringV3TranscriptRuntime struct {
	result        agent.TurnResult
	turnErr       error
	rawSubmission json.RawMessage
	openRequest   agent.ConversationRequest
	afterTurn     func()
}

func (runtime *standardAuthoringV3TranscriptRuntime) OpenConversation(_ context.Context, request agent.ConversationRequest) (agent.Conversation, error) {
	runtime.openRequest = request
	return standardAuthoringV3TranscriptConversation{runtime: runtime}, nil
}

type standardAuthoringV3TranscriptConversation struct {
	runtime *standardAuthoringV3TranscriptRuntime
}

func (conversation standardAuthoringV3TranscriptConversation) Turn(ctx context.Context, _ agent.TurnRequest) (agent.TurnResult, error) {
	if len(conversation.runtime.rawSubmission) != 0 {
		if len(conversation.runtime.openRequest.DynamicTools) != 1 {
			return agent.TurnResult{}, errors.New("expected one dynamic tool")
		}
		if _, err := conversation.runtime.openRequest.DynamicTools[0].Handler(ctx, conversation.runtime.rawSubmission); err != nil {
			return agent.TurnResult{}, err
		}
	}
	if conversation.runtime.afterTurn != nil {
		conversation.runtime.afterTurn()
	}
	return conversation.runtime.result, conversation.runtime.turnErr
}

func (standardAuthoringV3TranscriptConversation) Close() error { return nil }

func standardAuthoringV3TranscriptExecutor(t *testing.T, runtime standardAuthoringV3TranscriptRuntime) (*StandardAuthoringCodexAgentTurnExecutor, StageOperationInvocation, workflowadapter.AgentTurnOperationPayload, func() []workflowkit.StageCheckpoint, func() agent.ConversationRequest) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	stage, found := workflowadapter.StandardAuthoringContractStageCatalog().Stage(workflowkit.StageKey(workflowadapter.RepoStructureResearch))
	if !found || stage.AgentRole == nil {
		t.Fatal("research Agent stage is unavailable")
	}
	descriptor := standardAuthoringV3TestDescriptor(stage)
	role := descriptor.AgentRole.Clone()
	role.MaxTurns = 1
	descriptor.AgentRole = &role
	descriptor.Budget = workflowkit.ExecutionBudget{TurnTimeout: time.Minute, MaxTurns: 1}
	descriptor.QuotaClaims = []workflowkit.QuotaClaim{{Dimension: "agent_turn", Units: 1, ReclaimPolicy: workflowkit.ReclaimNever}}
	payload := workflowadapter.AgentTurnOperationPayload{
		AgentID: CodexAppServerProductionAgentID, ModelID: CodexAppServerProductionModelID, ReasoningEffort: CodexAppServerProductionReasoningEffort, MaxTurns: 1,
	}
	program, err := NewStandardAuthoringCodexTurnProgram("transcript-test", "1", []string{"complete the frozen task"}, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints := make([]workflowkit.StageCheckpoint, 0, 2)
	now := time.Date(2026, time.July, 28, 0, 0, 0, 0, time.UTC)
	executor, err := NewStandardAuthoringCodexAgentTurnExecutor(StandardAuthoringCodexAgentTurnExecutorConfig{
		InvocationFactory: func(context.Context, StageOperationInvocation, workflowadapter.AgentTurnOperationPayload) (CodexAppServerInvocation, error) {
			return standardAuthoringV3TranscriptInvocation(), nil
		},
		WorkspaceRoot:  root,
		ProgramByStage: map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{descriptor.Key: program},
		RuntimeFactory: func(CodexAppServerInvocation) agent.Runtime { return &runtime },
		Now:            func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	invocation := StageOperationInvocation{
		Request: workflowkit.StageExecutionRequest{
			Execution: workflowkit.FrozenExecution{ID: "static-transcript-run"},
			Claim: workflowkit.JobClaim{Stage: &workflowkit.StageClaim{
				StageAttempt: workflowkit.AttemptIdentity{ID: "static-transcript-attempt", Kind: workflowkit.AttemptStage, ScopeID: "static", Ordinal: 1}, Stage: descriptor,
			}},
			Stage: descriptor,
			ReadInput: func(context.Context, workflowkit.ArtifactBinding) ([]byte, error) {
				return nil, errors.New("unexpected input")
			},
			Checkpoint: func(_ context.Context, checkpoint workflowkit.StageCheckpoint) (workflowkit.CheckpointReceipt, error) {
				checkpoints = append(checkpoints, checkpoint.Clone())
				return workflowkit.CheckpointReceipt{CheckpointID: checkpoint.CheckpointID}, nil
			},
			Charge: func(context.Context, workflowkit.StageUsage) error { return nil },
		},
		Resolution: workflowadapter.StageOperationResolution{StageKey: descriptor.Key, Operation: workflowadapter.StageOperationBinding{Payload: payload}},
	}
	return executor, invocation, payload,
		func() []workflowkit.StageCheckpoint {
			return append([]workflowkit.StageCheckpoint(nil), checkpoints...)
		},
		func() agent.ConversationRequest { return runtime.openRequest }
}

func standardAuthoringV3TranscriptInvocation() CodexAppServerInvocation {
	return CodexAppServerInvocation{
		AgentID: CodexAppServerProductionAgentID, AgentVersion: "1", ModelID: CodexAppServerProductionModelID, ModelVersion: "1", ReasoningEffort: CodexAppServerProductionReasoningEffort,
		JavaScriptLauncherPath: "/controlled/codex.js", NodeExecutablePath: "/controlled/node", CodexHomeDirectory: "/controlled/home", CLIVersionOutput: "codex-cli 1",
		ApprovalPolicy: CodexAppServerApprovalPolicyNever, SandboxMode: CodexAppServerSandboxModeReadOnly, SandboxPolicy: CodexAppServerSandboxPolicyReadOnly,
	}
}

func standardAuthoringV3CompletedTranscript(t *testing.T, checkpoints []workflowkit.StageCheckpoint) workflowkit.AgentTurnTranscript {
	t.Helper()
	for _, checkpoint := range checkpoints {
		if checkpoint.Substep == "turn_completed" && checkpoint.AgentTurnTranscript != nil {
			return checkpoint.AgentTurnTranscript.Clone()
		}
	}
	t.Fatalf("completed transcript checkpoint not found: %+v", checkpoints)
	return workflowkit.AgentTurnTranscript{}
}

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
	if err != nil {
		t.Fatalf("critic accepted finding with unknown field: %s, %v", response, err)
	}
	assertStandardAuthoringV3StructuredRejected(t, response, "structured_output_invalid", "typed_artifact_invalid")
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

func TestStandardAuthoringV3TaskSynthesisRejectsNonCanonicalVerificationContractThenAcceptsCorrection(t *testing.T) {
	stage, found := workflowadapter.StandardAuthoringContractStageCatalog().Stage(workflowkit.StageKey(workflowadapter.TaskSynthesis))
	if !found || stage.AgentRole == nil {
		t.Fatal("3.0 task synthesis stage is unavailable")
	}
	payloadFor := func(verificationContract string) json.RawMessage {
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
		return payload
	}

	submission := newStandardAuthoringV3Submission(standardAuthoringV3TestDescriptor(stage), workflowkit.AgentRoleSynthesizer, "", 64<<10)
	response, err := submission.handle(context.Background(), payloadFor(`{"schema_version":"harbor.verification-contract.v1"}`))
	if err != nil {
		t.Fatal(err)
	}
	assertStandardAuthoringV3StructuredRejected(t, response, "structured_output_invalid", "typed_artifact_invalid")
	if _, accepted := submission.acceptedResult(); accepted {
		t.Fatal("legacy task-level verification object produced a durable output")
	}

	canonical := `{"format":"harbor.verification-contract.v1","version":"1","command":["sh","/task/tests/test.sh","wasm32-unknown-unknown"],"workdir":".","coverage_mode":"browser_wasm","allowed_solution_paths":["packages/yew-router/src/router.rs"]}`
	response, err = submission.handle(context.Background(), payloadFor(canonical))
	if err != nil || string(response) != `{"accepted":true}` {
		t.Fatalf("canonical verification contract was rejected: %s, %v", response, err)
	}
	if result, accepted := submission.acceptedResult(); !accepted || len(result.Artifacts) != 2 {
		t.Fatalf("corrected verification contract did not produce a durable result: %+v accepted=%t", result, accepted)
	}
	response, err = submission.handle(context.Background(), payloadFor(canonical))
	if err != nil || string(response) != `{"accepted":false,"reason":"already_accepted"}` {
		t.Fatalf("accepted submission did not remain immutable: %s, %v", response, err)
	}
}

func assertStandardAuthoringV3StructuredRejected(t *testing.T, response json.RawMessage, reason, rejectionCode string) {
	t.Helper()
	var receipt struct {
		Accepted      bool   `json:"accepted"`
		Reason        string `json:"reason"`
		RejectionCode string `json:"rejection_code"`
		Instruction   string `json:"instruction"`
	}
	if err := json.Unmarshal(response, &receipt); err != nil {
		t.Fatalf("decode structured rejection response %s: %v", response, err)
	}
	if receipt.Accepted || receipt.Reason != reason || receipt.RejectionCode != rejectionCode || !strings.Contains(receipt.Instruction, "accepted:true") || !strings.Contains(receipt.Instruction, "same or a later turn") {
		t.Fatalf("structured rejection response = %+v, want reason=%q rejection=%q with correction instruction", receipt, reason, rejectionCode)
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

func TestStandardAuthoringV3WriterContextIncludesValidationEnvironmentContract(t *testing.T) {
	verification := []byte(`{"format":"harbor.verification-contract.v1","version":"1","command":["sh","/task/tests/test.sh"],"workdir":".","coverage_mode":"integration","allowed_solution_paths":["src"]}`)
	for _, stageKey := range []string{workflowadapter.AuthoringLoop, workflowadapter.AuthoringRepair} {
		stage, found := workflowadapter.StandardAuthoringContractStageCatalog().Stage(workflowkit.StageKey(stageKey))
		if !found || stage.AgentRole == nil {
			t.Fatalf("3.0 writer stage %q is unavailable", stageKey)
		}
		contextDocument, err := standardAuthoringV3ContextDocument(
			workflowkit.StageExecutionRequest{Stage: standardAuthoringV3TestDescriptor(stage)},
			StandardAuthoringCodexTurnProgram{Fingerprint: workflowkit.SHA256Fingerprint([]byte("writer-program-" + string(stageKey)))},
			workflowkit.SHA256Fingerprint([]byte("writer-inputs-"+string(stageKey))),
			map[string][]byte{"verification_contract": verification}, true,
		)
		if err != nil {
			t.Fatalf("writer context for %q: %v", stageKey, err)
		}
		var document struct {
			ValidationEnvironmentContract *standardAuthoringV3ValidationEnvironmentContract `json:"validation_environment_contract"`
		}
		if err := json.Unmarshal(contextDocument, &document); err != nil {
			t.Fatalf("decode writer context for %q: %v", stageKey, err)
		}
		contract := document.ValidationEnvironmentContract
		if contract == nil {
			t.Fatalf("writer context for %q omitted validation_environment_contract", stageKey)
		}
		if contract.AuthoringWorkspace.Source.Path != StandardAuthoringCodexAttemptSourceDirectory || contract.AuthoringWorkspace.Source.Access != "read_only_reference" {
			t.Fatalf("authoring source contract for %q = %+v", stageKey, contract.AuthoringWorkspace.Source)
		}
		if contract.AuthoringWorkspace.Task.Path != StandardAuthoringCodexAttemptTaskDirectory || contract.AuthoringWorkspace.Task.Access != "writable_candidate_files_only" {
			t.Fatalf("authoring task contract for %q = %+v", stageKey, contract.AuthoringWorkspace.Task)
		}
		if contract.ValidatorRuntime.Source.Path != "/source" || contract.ValidatorRuntime.Source.Access != "read_only_frozen_source" ||
			contract.ValidatorRuntime.Task.Path != "/task" || contract.ValidatorRuntime.Task.Access != "read_only_candidate_files" ||
			contract.ValidatorRuntime.Work.Path != "/work" || contract.ValidatorRuntime.Work.Access != "read_write_source_copy" {
			t.Fatalf("validator runtime contract for %q = %+v", stageKey, contract.ValidatorRuntime)
		}
		if contract.RuntimeConstraints.Network != "none" || contract.RuntimeConstraints.RootFilesystem != "read_only" {
			t.Fatalf("runtime constraints for %q = %+v", stageKey, contract.RuntimeConstraints)
		}
		if !standardAuthoringV3NameValueContains(contract.RuntimeConstraints.Environment, "XDG_CACHE_HOME", "/tmp/harbor-cache") ||
			!standardAuthoringV3NameValueContains(contract.RuntimeConstraints.Environment, "HARBOR_WORKSPACE", "/work") {
			t.Fatalf("runtime env contract for %q = %+v", stageKey, contract.RuntimeConstraints.Environment)
		}
		if len(contract.ScriptProtocol) == 0 || !strings.Contains(strings.Join(contract.ScriptProtocol, "\n"), "Do not write") {
			t.Fatalf("script protocol for %q = %+v", stageKey, contract.ScriptProtocol)
		}
	}
}

func TestStandardAuthoringV3ContextIncludesRustCargoBrowserWASMGuidance(t *testing.T) {
	stage, found := workflowadapter.StandardAuthoringContractStageCatalog().Stage(workflowkit.StageKey(workflowadapter.AuthoringLoop))
	if !found || stage.AgentRole == nil {
		t.Fatal("3.0 author stage is unavailable")
	}
	verification := []byte(`{"format":"harbor.verification-contract.v1","version":"1","command":["wasm-pack","test","--headless","--firefox"],"workdir":".","coverage_mode":"browser_wasm","allowed_solution_paths":["src/lib.rs"]}`)
	contextDocument, err := standardAuthoringV3ContextDocument(
		workflowkit.StageExecutionRequest{Stage: standardAuthoringV3TestDescriptor(stage)},
		StandardAuthoringCodexTurnProgram{Fingerprint: workflowkit.SHA256Fingerprint([]byte("browser-wasm-program"))},
		workflowkit.SHA256Fingerprint([]byte("browser-wasm-inputs")),
		map[string][]byte{"verification_contract": verification}, true,
	)
	if err != nil {
		t.Fatalf("browser-wasm context: %v", err)
	}
	var document struct {
		ValidationEnvironmentContract *standardAuthoringV3ValidationEnvironmentContract `json:"validation_environment_contract"`
	}
	if err := json.Unmarshal(contextDocument, &document); err != nil {
		t.Fatalf("decode browser-wasm context: %v", err)
	}
	guidance := document.ValidationEnvironmentContract.RustCargoBrowserWASM
	if guidance == nil || !guidance.Applies {
		t.Fatalf("browser-wasm context omitted Rust/Cargo guidance: %+v", document.ValidationEnvironmentContract)
	}
	if !strings.Contains(guidance.ToolchainProvisioning, "environment/Dockerfile") ||
		!strings.Contains(guidance.ToolchainProvisioning, "Docker build context is environment/") ||
		!strings.Contains(guidance.CachePolicy, "limited /tmp") ||
		!standardAuthoringV3NameValueContains(guidance.RuntimeEnvironment, "CARGO_TARGET_DIR", "/work/.cargo-target") ||
		!standardAuthoringV3NameValueContains(guidance.RuntimeEnvironment, "CARGO_HOME", "/work/.cargo-home") {
		t.Fatalf("browser-wasm guidance = %+v", guidance)
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

func TestStandardAuthoringV3RejectedValidationResponseAddsNonDurableRepairGuidance(t *testing.T) {
	tests := []struct {
		name       string
		commandID  string
		stderrTail string
		want       []string
	}{
		{
			name:       "environment build network",
			commandID:  "environment_build",
			stderrTail: "apt-get update failed: Could not resolve deb.debian.org",
			want:       []string{"Runtime validation has --network none", "Fix provisioning in environment/Dockerfile"},
		},
		{
			name:       "source access permission",
			commandID:  "source_access",
			stderrTail: "touch: cannot touch '/source/.cargo/config.toml': Permission denied",
			want:       []string{"Do not write frozen source", "copy /source into writable /work"},
		},
		{
			name:       "oracle cargo wasm offline",
			commandID:  "oracle_verify",
			stderrTail: "cargo failed to download wasm-bindgen from crates.io",
			want:       []string{"Runtime validation has --network none", "CARGO_TARGET_DIR", "writable /work checkout"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			receipt := standardAuthoringV3RejectedReceipt(t, test.commandID, test.stderrTail)
			beforeDigest := receipt.Digest
			response := standardAuthoringV3ValidationToolResponse(false, "candidate_rejected", receipt.SnapshotDigest, receipt)
			if receipt.Digest != beforeDigest {
				t.Fatal("repair guidance mutated the durable validation receipt identity")
			}
			var decoded struct {
				Accepted       bool                                                      `json:"accepted"`
				Reason         string                                                    `json:"reason"`
				RepairGuidance []string                                                  `json:"repair_guidance"`
				RepairContext  *workflowadapter.StandardAuthoringValidationRepairContext `json:"validation_repair_context"`
			}
			if err := json.Unmarshal(response, &decoded); err != nil {
				t.Fatalf("decode validation response %s: %v", response, err)
			}
			joined := strings.Join(decoded.RepairGuidance, "\n")
			if decoded.Accepted || decoded.Reason != "candidate_rejected" || decoded.RepairContext == nil {
				t.Fatalf("validation response = %+v", decoded)
			}
			for _, want := range test.want {
				if !strings.Contains(joined, want) {
					t.Fatalf("repair guidance %q does not contain %q", joined, want)
				}
			}
			expected, err := workflowadapter.NewStandardAuthoringValidationRepairContext(receipt, standardAuthoringV3EditableCandidatePaths())
			if err != nil {
				t.Fatal(err)
			}
			if decoded.RepairContext.CandidateDigest != expected.CandidateDigest || decoded.RepairContext.ReceiptDigest != expected.ReceiptDigest ||
				decoded.RepairContext.FailedStep != expected.FailedStep {
				t.Fatalf("repair context identity changed: got %+v want %+v", decoded.RepairContext, expected)
			}
		})
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

func standardAuthoringV3NameValueContains(items []standardAuthoringV3NameValue, name, value string) bool {
	for _, item := range items {
		if item.Name == name && item.Value == value {
			return true
		}
	}
	return false
}

func standardAuthoringV3RejectedReceipt(t *testing.T, commandID, stderrTail string) workflowkit.ValidationReceipt {
	t.Helper()
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
		SnapshotDigest: workflowkit.SHA256Fingerprint([]byte("candidate")), ContractDigest: contractDigest,
		Verdict: workflowkit.ValidationReject, FailureCode: workflowkit.AgentFailureValidatorReject,
		Diagnostics: []workflowkit.AgentCommandReport{{CommandID: commandID, ExitCode: 1, TestStarted: commandID != "environment_build", StderrTail: stderrTail}},
		IssuedAt:    now, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	return receipt
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
			Diagnostics: []workflowkit.AgentCommandReport{{CommandID: "baseline_verify", ExitCode: 1, TestStarted: true, StderrTail: "patch failed after redaction"}},
			IssuedAt:    now, ExpiresAt: now.Add(time.Minute),
		})
	}

	first, err := submission.handle(context.Background(), json.RawMessage(`{"verdict":"pass"}`))
	if err != nil || !strings.Contains(string(first), `"reason":"candidate_rejected"`) || !strings.Contains(string(first), `"stderr_tail":"patch failed after redaction"`) {
		t.Fatalf("rejected repair validation response = %s, %v", first, err)
	}
	if _, accepted := submission.acceptedResult(); accepted {
		t.Fatal("rejected repair candidate was accepted")
	}
	secondInSameTurn, err := submission.handle(context.Background(), json.RawMessage(`{"verdict":"pass"}`))
	if err != nil || !strings.Contains(string(secondInSameTurn), `"accepted":true`) {
		t.Fatalf("second repair validation in one turn = %s, %v; attempts=%d", secondInSameTurn, err, attempts)
	}
	result, accepted := submission.acceptedResult()
	if !accepted || attempts != 2 || len(result.Artifacts) != 9 || result.Artifacts[7].Name != "validation_receipt" || result.Artifacts[8].Name != "workflow_repair_ledger" {
		t.Fatalf("repair result = %+v accepted=%t attempts=%d", result, accepted, attempts)
	}
	var receipt workflowkit.ValidationReceipt
	if err := json.Unmarshal(result.Artifacts[7].Content, &receipt); err != nil || receipt.Verdict != workflowkit.ValidationPass {
		t.Fatalf("repair receipt = %+v, %v", receipt, err)
	}
	thirdAfterAcceptance, err := submission.handle(context.Background(), json.RawMessage(`{"verdict":"pass"}`))
	if err != nil || string(thirdAfterAcceptance) != `{"accepted":false,"reason":"already_accepted"}` || attempts != 2 {
		t.Fatalf("repair validation after acceptance = %s, %v; attempts=%d", thirdAfterAcceptance, err, attempts)
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
	repairContext, err := workflowadapter.NewStandardAuthoringValidationRepairContext(receipt, nil)
	if err != nil {
		t.Fatal(err)
	}
	repairContextRaw, err := json.Marshal(repairContext)
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string][]byte{
		"candidate_snapshot": snapshotRaw, "validation_receipt": receiptRaw,
		workflowadapter.StandardAuthoringValidationRepairContextArtifact: repairContextRaw,
	}
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
	if len(ledger.Entries) != 3 || ledger.Entries[0].Finding.Code != "host_validation_reject" || !ledger.Entries[0].ConsumedCandidateRound ||
		ledger.Entries[1].ConsumedCandidateRound || ledger.Entries[2].ConsumedCandidateRound {
		t.Fatalf("repair ledger = %+v, want host rejection plus optional critic findings with one candidate repair charge", ledger)
	}
}

func TestStandardAuthoringV3CriticRejectsFindingNotBoundToCandidateReceipt(t *testing.T) {
	stage, found := workflowadapter.StandardAuthoringContractStageCatalog().Stage(workflowkit.StageKey(workflowadapter.TestQualityCritic))
	if !found || stage.AgentRole == nil {
		t.Fatal("3.0 test quality critic stage is unavailable")
	}
	contract, err := workflowkit.NewCandidateValidationContract(workflowkit.SHA256Fingerprint([]byte("runtime")), workflowkit.SHA256Fingerprint([]byte("verification")))
	if err != nil {
		t.Fatal(err)
	}
	contractDigest, err := contract.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := workflowkit.NewCandidateSnapshot([]workflowkit.CandidateFile{{
		Path: "instruction.md", SchemaVersion: "harbor.artifact.v1", ContentDigest: workflowkit.SHA256Fingerprint([]byte("candidate")), SizeBytes: int64(len("candidate")),
	}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	receipt, err := workflowkit.NewValidationReceipt(workflowkit.ValidationReceipt{
		SnapshotDigest: snapshot.Digest, ContractDigest: contractDigest, Verdict: workflowkit.ValidationReject, FailureCode: workflowkit.AgentFailureValidatorReject,
		Diagnostics: []workflowkit.AgentCommandReport{{CommandID: "environment_build", ExitCode: 1, TestStarted: false, StderrTail: "wasm-bindgen-cli version mismatch"}},
		IssuedAt:    now, ExpiresAt: now.Add(time.Minute),
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
	identity, found, err := standardAuthoringV3CandidateValidationIdentityFromInputs(map[string][]byte{"candidate_snapshot": snapshotRaw, "validation_receipt": receiptRaw})
	if err != nil || !found {
		t.Fatalf("candidate validation identity = %+v, %t, %v", identity, found, err)
	}
	payloadFor := func(finding workflowkit.WorkflowFinding) json.RawMessage {
		t.Helper()
		findingRaw, err := json.Marshal(finding)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(map[string]any{
			"verdict":   "pass",
			"artifacts": []map[string]string{{"name": "test_quality_finding", "content": string(findingRaw)}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}

	unbound := newStandardAuthoringV3Submission(standardAuthoringV3TestDescriptor(stage), workflowkit.AgentRoleCritic, "", 64<<10)
	unbound.candidateValidationIdentity = &identity
	unboundFinding, err := workflowkit.NewWorkflowFinding(workflowkit.WorkflowFinding{
		Code: "test_quality_defect", ProducingStage: workflowkit.StageKey(workflowadapter.TestQualityCritic), TargetWriter: workflowkit.StageKey(workflowadapter.AuthoringRepair),
		EvidenceDigest: workflowkit.SHA256Fingerprint([]byte("stale-evidence")), CandidateDigest: identity.CandidateSnapshotDigest, DiagnosticDigest: identity.ValidationReceiptDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := unbound.handle(context.Background(), payloadFor(unboundFinding))
	if err != nil {
		t.Fatalf("critic unbound finding: %s, %v", response, err)
	}
	assertStandardAuthoringV3StructuredRejected(t, response, "structured_output_invalid", "finding_not_bound")
	if unbound.lastRejectionCode != "finding_not_bound" {
		t.Fatalf("lastRejectionCode = %q, want finding_not_bound", unbound.lastRejectionCode)
	}
	if _, accepted := unbound.acceptedResult(); accepted {
		t.Fatal("unbound finding produced a durable output")
	}

	bound := newStandardAuthoringV3Submission(standardAuthoringV3TestDescriptor(stage), workflowkit.AgentRoleCritic, "", 64<<10)
	bound.candidateValidationIdentity = &identity
	boundFinding, err := workflowkit.NewWorkflowFinding(workflowkit.WorkflowFinding{
		Code: "test_quality_defect", ProducingStage: workflowkit.StageKey(workflowadapter.TestQualityCritic), TargetWriter: workflowkit.StageKey(workflowadapter.AuthoringRepair),
		EvidenceDigest: identity.ValidationReceiptDigest, CandidateDigest: identity.CandidateSnapshotDigest, DiagnosticDigest: identity.ValidationReceiptDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err = bound.handle(context.Background(), payloadFor(boundFinding))
	if err != nil || string(response) != `{"accepted":true}` {
		t.Fatalf("receipt-bound critic finding was rejected: %s, %v", response, err)
	}
	result, accepted := bound.acceptedResult()
	if !accepted || len(result.Artifacts) != 1 || result.Artifacts[0].Name != "test_quality_finding" {
		t.Fatalf("bound critic artifacts = %+v accepted=%t", result.Artifacts, accepted)
	}
}

func TestStandardAuthoringV3RepairLedgerSkipsUnboundFindings(t *testing.T) {
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
	repairContext, err := workflowadapter.NewStandardAuthoringValidationRepairContext(receipt, nil)
	if err != nil {
		t.Fatal(err)
	}
	repairContextRaw, err := json.Marshal(repairContext)
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string][]byte{
		"candidate_snapshot": snapshotRaw, "validation_receipt": receiptRaw,
		workflowadapter.StandardAuthoringValidationRepairContextArtifact: repairContextRaw,
		"unbound_test_quality_finding":                                   []byte(`{"schema_version":"harbor.workflow-finding.v1","finding":{"code":"test_quality_defect","producing_stage":"test_quality_critic","target_writer":"authoring_repair","evidence_digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","candidate_digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222","diagnostic_digest":"sha256:3333333333333333333333333333333333333333333333333333333333333333"}}`),
	}
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
		findingRaw, err := json.Marshal(finding)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := workflowkit.PlanWorkflowRepair(resolved.Descriptor, finding, rules, nil); err != nil {
			t.Fatal(err)
		}
		inputs[name] = findingRaw
	}
	ledgerRaw, err := standardAuthoringV3RepairLedger(resolved.Descriptor, inputs)
	if err != nil {
		t.Fatalf("repair ledger rejected inputs with an unbound finding: %v", err)
	}
	var ledger workflowkit.WorkflowRepairLedger
	if err := json.Unmarshal(ledgerRaw, &ledger); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(ledger.Entries) != 3 {
		t.Fatalf("repair ledger = %+v, want host rejection plus bound findings only (unbound skipped)", ledger)
	}
	for _, entry := range ledger.Entries {
		if entry.Finding.Code == "test_quality_defect" && entry.Finding.EvidenceDigest != receipt.Digest {
			t.Fatalf("ledger contains an unbound finding: %+v", entry.Finding)
		}
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
