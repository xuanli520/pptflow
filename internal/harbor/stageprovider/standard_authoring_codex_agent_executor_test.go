package stageprovider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringCodexAgentTurnExecutorRunsOnlyFrozenProgram(t *testing.T) {
	t.Parallel()
	const secret = "not-for-durable-logs"
	now := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)
	stage := standardAuthoringCodexTestStage(2)
	acceptedCandidate := standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, []byte("sealed analysis"))
	runtime := &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{
		results: []agent.TurnResult{
			{Model: CodexAppServerProductionModelID, Text: `{"progress":"internal only"}`},
			// Free assistant text is deliberately not an output authority. The
			// accepted candidate below is the only artifact source.
			{Model: CodexAppServerProductionModelID, Text: `{"verdict":"needs_repair","artifacts":[{"content_base64":"aWdub3JlZCBmcmVlIHRleHQ="}]}`},
		},
		submissions: [][]json.RawMessage{nil, {acceptedCandidate}},
	}}
	executor, program := standardAuthoringCodexTestExecutor(t, runtime, now, 2)
	request, checkpoints, usages := standardAuthoringCodexTestRequest(stage, []byte(secret), now)

	result, err := executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: request,
		Resolution: workflowadapter.StageOperationResolution{
			StageKey:  stage.Key,
			Operation: workflowadapter.StageOperationBinding{Payload: standardAuthoringCodexTestPayload(len(program.TurnPrompts))},
		},
	}, standardAuthoringCodexTestPayload(2))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Status != workflowkit.StatusCompleted || result.Outcome.Verdict != workflowkit.VerdictPass || result.ErrorText != "" {
		t.Fatalf("result = %+v, want completed pass", result)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].Name != "repo_analysis" || result.Artifacts[0].SchemaVersion != "harbor.artifact.v1" || string(result.Artifacts[0].Content) != "sealed analysis" || result.Artifacts[0].TurnOrdinal != 2 {
		t.Fatalf("artifacts = %+v, want one frozen final-turn artifact", result.Artifacts)
	}
	if len(runtime.openRequests) != 1 {
		t.Fatalf("open requests = %d, want one", len(runtime.openRequests))
	}
	open := runtime.openRequests[0]
	if open.Model != CodexAppServerProductionModelID || open.ReasoningEffort != string(CodexAppServerProductionReasoningEffort) || open.NetworkAccess || open.SandboxMode != CodexAppServerSandboxModeReadOnly || open.SandboxPolicy != CodexAppServerSandboxPolicyReadOnly || open.LogPath != os.DevNull {
		t.Fatalf("controlled conversation request = %+v", open)
	}
	if len(open.DynamicTools) != 1 || open.DynamicTools[0].Name != standardAuthoringCodexSubmitToolName || open.DynamicTools[0].Handler == nil || !json.Valid(open.DynamicTools[0].InputSchema) {
		t.Fatalf("conversation dynamic tools = %+v, want one valid private submit tool", open.DynamicTools)
	}
	if len(runtime.conversation.requests) != 2 || runtime.conversation.requests[0].Model != CodexAppServerProductionModelID || runtime.conversation.requests[0].ReasoningEffort != string(CodexAppServerProductionReasoningEffort) || runtime.conversation.requests[1].ReasoningEffort != string(CodexAppServerProductionReasoningEffort) || len(runtime.conversation.requests[0].Input) != 1 || len(runtime.conversation.requests[1].Input) != 0 {
		t.Fatalf("turn requests = %+v", runtime.conversation.requests)
	}
	for _, turn := range runtime.conversation.requests {
		if !json.Valid(turn.OutputSchema) {
			t.Fatalf("turn output schema = %q, want valid stage-derived JSON Schema", turn.OutputSchema)
		}
	}
	firstInput := runtime.conversation.requests[0].Input[0].Text
	if strings.Contains(firstInput, secret) || !strings.Contains(firstInput, base64.StdEncoding.EncodeToString([]byte(secret))) || !strings.Contains(firstInput, string(program.Fingerprint)) {
		t.Fatalf("first structured prompt input omitted the frozen/base64 contract or exposed raw content: %q", firstInput)
	}
	if runtime.conversation.closed != 1 {
		t.Fatalf("conversation close count = %d, want one", runtime.conversation.closed)
	}
	if len(*checkpoints) != 3 {
		t.Fatalf("checkpoints = %+v, want a completed first turn and the accepted second-turn preflight", *checkpoints)
	}
	for _, checkpoint := range *checkpoints {
		if checkpoint.Resumable || strings.Contains(string(checkpoint.Payload), secret) || checkpoint.TurnOrdinal < 1 || checkpoint.TurnOrdinal > 2 {
			t.Fatalf("checkpoint leaked state or misrepresented resume semantics: %+v", checkpoint)
		}
	}
	if len(*usages) != 3 {
		t.Fatalf("usage records = %+v, want two agent turns and one output submission", *usages)
	}
	agentTurns := 0
	outputSubmissions := 0
	for _, usage := range *usages {
		if usage.Units != 1 || strings.Contains(usage.OperationKey, secret) {
			t.Fatalf("usage = %+v, want one secret-free usage unit", usage)
		}
		switch usage.Dimension {
		case "agent_turn":
			agentTurns++
			if !strings.HasPrefix(usage.OperationKey, "standard-authoring-codex-usage:sha256:") {
				t.Fatalf("agent-turn usage = %+v, want frozen operation key", usage)
			}
		case standardAuthoringCodexOutputSubmissionQuotaDimension:
			outputSubmissions++
			if !strings.HasPrefix(usage.OperationKey, "standard-authoring-codex-output-submission:sha256:") {
				t.Fatalf("output-submission usage = %+v, want frozen operation key", usage)
			}
		default:
			t.Fatalf("unexpected usage dimension: %+v", usage)
		}
	}
	if agentTurns != 2 || outputSubmissions != 1 {
		t.Fatalf("usage dimensions = agent_turn:%d output_submission:%d, want 2 and 1", agentTurns, outputSubmissions)
	}
}

func TestStandardAuthoringCodexAgentTurnRequestCarriesFrozenReviewFeedback(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	stage := standardAuthoringCodexTestStage(1)
	stage.Inputs = []workflowkit.ArtifactSpec{
		{Name: "repo_prepared", SchemaVersion: "harbor.artifact.v1", Required: true},
		{Name: workflowadapter.AuthoringContractArtifact, SchemaVersion: workflowadapter.AuthoringContractSchemaVersion, Required: true},
		{Name: "task_review_decision", SchemaVersion: "harbor.review-decision.v1"},
	}
	runtime := &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{
		results:     []agent.TurnResult{{Model: CodexAppServerProductionModelID, Text: `{"submitted":true}`}},
		submissions: [][]json.RawMessage{{standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, []byte("repaired analysis"))}},
	}}
	executor, program := standardAuthoringCodexTestExecutor(t, runtime, now, 1)
	source := []byte("prepared source")
	reason := "Correct the misspelled tower-http path."
	feedback := []byte(`{"format":"harbor.authoring-review-gate-decision.v1","action":"request_changes","decision_reason":"` + reason + `"}`)
	sourceBinding := workflowkit.ArtifactBinding{
		Name: "repo_prepared", ArtifactID: "source-input", ContentDigest: workflowkit.SHA256Fingerprint(source), SchemaVersion: "harbor.artifact.v1",
	}
	feedbackBinding := workflowkit.ArtifactBinding{
		Name: "task_review_decision", ArtifactID: "feedback-input", ContentDigest: workflowkit.SHA256Fingerprint(feedback), SchemaVersion: "harbor.review-decision.v1",
	}
	request, _, _ := standardAuthoringCodexTestRequest(stage, source, now)
	rootBinding := request.Inputs[1]
	rootContent, err := request.ReadInput(context.Background(), rootBinding)
	if err != nil {
		t.Fatal(err)
	}
	request.Inputs = []workflowkit.ArtifactBinding{rootBinding, sourceBinding, feedbackBinding}
	request.ReadInput = func(_ context.Context, binding workflowkit.ArtifactBinding) ([]byte, error) {
		switch binding {
		case rootBinding:
			return append([]byte(nil), rootContent...), nil
		case sourceBinding:
			return append([]byte(nil), source...), nil
		case feedbackBinding:
			return append([]byte(nil), feedback...), nil
		default:
			return nil, errors.New("unexpected frozen input")
		}
	}
	result, err := executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: request,
		Resolution: workflowadapter.StageOperationResolution{
			StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{Payload: standardAuthoringCodexTestPayload(len(program.TurnPrompts))},
		},
	}, standardAuthoringCodexTestPayload(1))
	if err != nil || result.Outcome.Verdict != workflowkit.VerdictPass {
		t.Fatalf("execute repair feedback turn = %+v, %v", result, err)
	}
	if len(runtime.conversation.requests) != 1 || len(runtime.conversation.requests[0].Input) != 1 {
		t.Fatalf("repair feedback requests = %+v", runtime.conversation.requests)
	}
	document := runtime.conversation.requests[0].Input[0].Text
	if strings.Contains(document, reason) || !strings.Contains(document, "task_review_decision") ||
		!strings.Contains(document, base64.StdEncoding.EncodeToString(feedback)) || !strings.Contains(document, string(feedbackBinding.ContentDigest)) {
		t.Fatalf("frozen repair feedback request document = %s", document)
	}
}

func TestStandardAuthoringCodexReadInputsRejectsBindingsOutsideFrozenDescriptor(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		mutate func(*workflowkit.StageExecutionRequest)
	}{
		{
			name: "undeclared input",
			mutate: func(request *workflowkit.StageExecutionRequest) {
				content := []byte("ambient context")
				request.Inputs = append(request.Inputs, workflowkit.ArtifactBinding{
					Name: "ambient_instruction", ArtifactID: "ambient-input", ContentDigest: workflowkit.SHA256Fingerprint(content), SchemaVersion: "harbor.artifact.v1",
				})
			},
		},
		{
			name: "duplicate input",
			mutate: func(request *workflowkit.StageExecutionRequest) {
				request.Inputs = append(request.Inputs, request.Inputs[0])
			},
		},
		{
			name: "missing root contract",
			mutate: func(request *workflowkit.StageExecutionRequest) {
				request.Inputs = request.Inputs[:1]
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request, _, _ := standardAuthoringCodexTestRequest(standardAuthoringCodexTestStage(1), []byte("prepared source"), now)
			testCase.mutate(&request)
			if _, _, err := standardAuthoringCodexReadInputs(context.Background(), request); err == nil {
				t.Fatal("read frozen inputs succeeded, want descriptor-bound input rejection")
			}
		})
	}
}

func TestStandardAuthoringCodexRequestDocumentLabelsOnlyOptionalReviewFeedbackAsRepair(t *testing.T) {
	t.Parallel()
	contract := standardAuthoringCodexTestRootContract()
	rootContent, err := contract.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	program, err := NewStandardAuthoringCodexTurnProgram("standard-authoring.context", "1", []string{"apply frozen context"}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name         string
		decisionName string
		required     bool
		wantRepair   bool
	}{
		{name: "required review gate is an ordinary input", decisionName: "task_review_decision", required: true},
		{name: "optional continuation feedback is a repair", decisionName: "solution_review_decision", wantRepair: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stage := standardAuthoringCodexTestStage(1)
			stage.Inputs = append(stage.Inputs, workflowkit.ArtifactSpec{Name: testCase.decisionName, SchemaVersion: "harbor.review-decision.v1", Required: testCase.required})
			decisionContent := []byte(`{"action":"request_changes"}`)
			decision := standardAuthoringCodexTestContextInput(testCase.decisionName, "review-decision", "harbor.review-decision.v1", decisionContent)
			request := workflowkit.StageExecutionRequest{
				Stage: stage,
				Claim: workflowkit.JobClaim{Stage: &workflowkit.StageClaim{
					StageAttempt: workflowkit.AttemptIdentity{ID: "stage-attempt-1", Ordinal: 3},
				}},
			}
			inputs := []standardAuthoringCodexInput{
				standardAuthoringCodexTestContextInput(workflowadapter.AuthoringContractArtifact, "root-contract", workflowadapter.AuthoringContractSchemaVersion, rootContent),
				standardAuthoringCodexTestContextInput("repo_prepared", "prepared-repo", "harbor.artifact.v1", []byte("prepared source")),
				decision,
			}
			document, err := standardAuthoringCodexRequestDocument(request, workflowadapter.StandardAuthoringCurrentTemplateReference(), program, inputs, workflowkit.SHA256Fingerprint([]byte("inputs")), contract)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Context workflowadapter.AuthoringContextEnvelope `json:"context"`
			}
			if err := json.Unmarshal(document, &decoded); err != nil {
				t.Fatal(err)
			}
			decisionInInputs := false
			for _, input := range decoded.Context.Inputs {
				if input.Name == testCase.decisionName && input.ArtifactID == decision.ArtifactID {
					decisionInInputs = true
				}
			}
			if !decisionInInputs {
				t.Fatalf("context inputs = %+v, want review decision as ordinary input", decoded.Context.Inputs)
			}
			if testCase.wantRepair {
				if len(decoded.Context.Repairs) != 1 || decoded.Context.Repairs[0].ID != decision.ArtifactID || decoded.Context.Repairs[0].EvidenceDigest != string(decision.ContentDigest) {
					t.Fatalf("context repairs = %+v, want optional continuation review evidence", decoded.Context.Repairs)
				}
				return
			}
			if len(decoded.Context.Repairs) != 0 {
				t.Fatalf("context repairs = %+v, want no repair for required review gate", decoded.Context.Repairs)
			}
		})
	}
}

func TestStandardAuthoringCodexRequestDocumentRejectsOversizeSerializedContext(t *testing.T) {
	t.Parallel()
	contract := standardAuthoringCodexTestRootContract()
	rootContent, err := contract.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	program, err := NewStandardAuthoringCodexTurnProgram("standard-authoring.context", "1", []string{"apply frozen context"}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	oversizeInput := standardAuthoringCodexTestContextInput("repo_prepared", "prepared-repo", "harbor.artifact.v1", []byte("prepared source"))
	oversizeInput.ContentBase64 = strings.Repeat("A", standardAuthoringCodexContextMaxDocumentBytes)
	request := workflowkit.StageExecutionRequest{Stage: standardAuthoringCodexTestStage(1)}
	_, err = standardAuthoringCodexRequestDocument(request, workflowadapter.StandardAuthoringCurrentTemplateReference(), program, []standardAuthoringCodexInput{
		standardAuthoringCodexTestContextInput(workflowadapter.AuthoringContractArtifact, "root-contract", workflowadapter.AuthoringContractSchemaVersion, rootContent),
		oversizeInput,
	}, workflowkit.SHA256Fingerprint([]byte("inputs")), contract)
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversize document error = %v, want size-limit rejection", err)
	}
}

func TestStandardAuthoringCodexAgentTurnExecutorAcceptsSubmissionOnThirtiethTurn(t *testing.T) {
	t.Parallel()
	const turns = 30
	now := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	stage := standardAuthoringCodexTestStage(turns)
	results := make([]agent.TurnResult, turns)
	submissions := make([][]json.RawMessage, turns)
	for index := range results {
		results[index] = agent.TurnResult{Model: CodexAppServerProductionModelID, Text: `{"progress":"continue"}`}
	}
	submissions[turns-1] = []json.RawMessage{standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, []byte("thirtieth-turn-analysis"))}
	runtime := &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{results: results, submissions: submissions}}
	executor, program := standardAuthoringCodexTestExecutor(t, runtime, now, turns)
	request, checkpoints, usages := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), now)

	result, err := executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: request,
		Resolution: workflowadapter.StageOperationResolution{
			StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{Payload: standardAuthoringCodexTestPayload(len(program.TurnPrompts))},
		},
	}, standardAuthoringCodexTestPayload(turns))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Status != workflowkit.StatusCompleted || len(result.Artifacts) != 1 || result.Artifacts[0].TurnOrdinal != turns {
		t.Fatalf("thirtieth-turn result = %+v", result)
	}
	if len(runtime.conversation.requests) != turns || runtime.conversation.closed != 1 {
		t.Fatalf("conversation requests=%d closes=%d, want %d/1", len(runtime.conversation.requests), runtime.conversation.closed, turns)
	}
	if len(*checkpoints) != 2*turns-1 {
		t.Fatalf("checkpoints = %d, want %d before the accepted terminal turn short-circuits", len(*checkpoints), 2*turns-1)
	}
	if standardAuthoringCodexTestUsageCount(*usages, "agent_turn") != turns || standardAuthoringCodexTestUsageCount(*usages, standardAuthoringCodexOutputSubmissionQuotaDimension) != 1 {
		t.Fatalf("usage records = %+v, want %d agent turns and one output submission", *usages, turns)
	}
}

func TestStandardAuthoringCodexAgentTurnExecutorShortCircuitsAcceptedSubmission(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	stage := standardAuthoringCodexTestStage(1)
	conversation := &standardAuthoringCodexAcceptanceBlockingConversation{
		candidate:  standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, []byte("accepted-before-provider-completion")),
		turnExited: make(chan struct{}),
	}
	runtime := &standardAuthoringCodexAcceptanceBlockingRuntime{conversation: conversation}
	executor, program := standardAuthoringCodexTestExecutor(t, runtime, now, 1)
	request, checkpoints, usages := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), now)
	ctx, cancel := context.WithCancel(context.Background())
	conversation.afterSubmission = cancel
	t.Cleanup(cancel)

	type executionOutcome struct {
		result workflowkit.StageExecutionResult
		err    error
	}
	completed := make(chan executionOutcome, 1)
	go func() {
		result, err := executor.ExecuteAgentTurn(ctx, StageOperationInvocation{
			Request: request,
			Resolution: workflowadapter.StageOperationResolution{
				StageKey:  stage.Key,
				Operation: workflowadapter.StageOperationBinding{Payload: standardAuthoringCodexTestPayload(len(program.TurnPrompts))},
			},
		}, standardAuthoringCodexTestPayload(1))
		completed <- executionOutcome{result: result, err: err}
	}()

	var outcome executionOutcome
	select {
	case outcome = <-completed:
	case <-time.After(time.Second):
		cancel()
		select {
		case <-completed:
		case <-time.After(time.Second):
			t.Fatal("accepted submission left the provider turn running after cancellation")
		}
		t.Fatal("accepted submission did not short-circuit the provider turn")
	}
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if outcome.result.Outcome.Status != workflowkit.StatusCompleted || outcome.result.Outcome.Verdict != workflowkit.VerdictPass || len(outcome.result.Artifacts) != 1 || string(outcome.result.Artifacts[0].Content) != "accepted-before-provider-completion" {
		t.Fatalf("short-circuit result = %+v", outcome.result)
	}
	if conversation.closed != 1 || !conversation.closedAfterTurn {
		t.Fatalf("conversation closure = closes:%d after-turn:%t, want 1/true", conversation.closed, conversation.closedAfterTurn)
	}
	receipt := standardAuthoringCodexTestSubmissionReceipt(t, conversation.submissionResponse)
	if !receipt.Accepted || conversation.submissionErr != nil {
		t.Fatalf("submission receipt=%+v err=%v", receipt, conversation.submissionErr)
	}
	if len(*checkpoints) != 1 || (*checkpoints)[0].Substep != "turn_ready" {
		t.Fatalf("checkpoints = %+v, want only the pre-turn checkpoint", *checkpoints)
	}
	if standardAuthoringCodexTestUsageCount(*usages, "agent_turn") != 1 || standardAuthoringCodexTestUsageCount(*usages, standardAuthoringCodexOutputSubmissionQuotaDimension) != 1 {
		t.Fatalf("usage records = %+v, want one turn and one accepted submission", *usages)
	}
}

func TestStandardAuthoringCodexAgentTurnExecutorFailsClosedWithoutLeakingProviderOrInputData(t *testing.T) {
	t.Parallel()
	const secret = "do-not-include-this-secret"
	now := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)
	stage := standardAuthoringCodexTestStage(1)
	cases := []struct {
		name       string
		runtime    *standardAuthoringCodexRuntimeStub
		requestMut func(*workflowkit.StageExecutionRequest)
		wantCode   string
	}{
		{
			name:     "provider error",
			runtime:  &standardAuthoringCodexRuntimeStub{openErr: errors.New("provider failed with " + secret)},
			wantCode: standardAuthoringCodexFailureRuntime,
		},
		{
			name:    "input reader error",
			runtime: &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{}},
			requestMut: func(request *workflowkit.StageExecutionRequest) {
				request.ReadInput = func(context.Context, workflowkit.ArtifactBinding) ([]byte, error) { return nil, errors.New(secret) }
			},
			wantCode: standardAuthoringCodexFailureInput,
		},
		{
			name:    "checkpoint error",
			runtime: &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{}},
			requestMut: func(request *workflowkit.StageExecutionRequest) {
				request.Checkpoint = func(context.Context, workflowkit.StageCheckpoint) (workflowkit.CheckpointReceipt, error) {
					return workflowkit.CheckpointReceipt{}, errors.New(secret)
				}
			},
			wantCode: standardAuthoringCodexFailureCheckpoint,
		},
		{
			name: "free text without tool submission",
			runtime: &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{
				results: []agent.TurnResult{{Model: CodexAppServerProductionModelID, Text: `{"secret":"` + secret + `","verdict":"pass"}`}},
			}},
			wantCode: standardAuthoringCodexSubmissionFailureAbsent,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			executor, program := standardAuthoringCodexTestExecutor(t, testCase.runtime, now, 1)
			request, _, _ := standardAuthoringCodexTestRequest(stage, []byte(secret), now)
			if testCase.requestMut != nil {
				testCase.requestMut(&request)
			}
			result, err := executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
				Request: request,
				Resolution: workflowadapter.StageOperationResolution{StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{
					Payload: standardAuthoringCodexTestPayload(len(program.TurnPrompts)),
				}},
			}, standardAuthoringCodexTestPayload(1))
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome.Status != workflowkit.StatusInfraFailed || result.ErrorText != testCase.wantCode || strings.Contains(result.ErrorText, secret) {
				t.Fatalf("result = %+v, want safe failure code %q", result, testCase.wantCode)
			}
		})
	}
}

func TestStandardAuthoringCodexAgentTurnExecutorRejectsAgentConfigurationAndPromptProgramDrift(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)
	stage := standardAuthoringCodexTestStage(1)
	runtime := &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{}}
	executor, program := standardAuthoringCodexTestExecutor(t, runtime, now, 1)
	request, _, _ := standardAuthoringCodexTestRequest(stage, []byte("input"), now)
	driftedPayload := standardAuthoringCodexTestPayload(1)
	driftedPayload.ModelID = "other-model"
	result, err := executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: request,
		Resolution: workflowadapter.StageOperationResolution{StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{
			Payload: standardAuthoringCodexTestPayload(len(program.TurnPrompts)),
		}},
	}, driftedPayload)
	if err != nil {
		t.Fatal(err)
	}
	if result.ErrorText != standardAuthoringCodexFailureConfiguration || len(runtime.openRequests) != 0 {
		t.Fatalf("model drift result = %+v opens=%d", result, len(runtime.openRequests))
	}
	effortDrift := standardAuthoringCodexTestPayload(1)
	effortDrift.ReasoningEffort = workflowadapter.AgentReasoningEffortHigh
	result, err = executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: request,
		Resolution: workflowadapter.StageOperationResolution{StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{
			Payload: standardAuthoringCodexTestPayload(len(program.TurnPrompts)),
		}},
	}, effortDrift)
	if err != nil {
		t.Fatal(err)
	}
	if result.ErrorText != standardAuthoringCodexFailureConfiguration || len(runtime.openRequests) != 0 {
		t.Fatalf("reasoning effort drift result = %+v opens=%d", result, len(runtime.openRequests))
	}
	wrongSubmissionQuota := request
	wrongSubmissionQuota.Stage = request.Stage.Clone()
	for index := range wrongSubmissionQuota.Stage.QuotaClaims {
		if wrongSubmissionQuota.Stage.QuotaClaims[index].Dimension == standardAuthoringCodexOutputSubmissionQuotaDimension {
			wrongSubmissionQuota.Stage.QuotaClaims[index].Units = 0
		}
	}
	result, err = executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: wrongSubmissionQuota,
		Resolution: workflowadapter.StageOperationResolution{StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{
			Payload: standardAuthoringCodexTestPayload(len(program.TurnPrompts)),
		}},
	}, standardAuthoringCodexTestPayload(1))
	if err != nil {
		t.Fatal(err)
	}
	if result.ErrorText != standardAuthoringCodexFailureConfiguration || len(runtime.openRequests) != 0 {
		t.Fatalf("output-submission quota drift result = %+v opens=%d", result, len(runtime.openRequests))
	}

	tampered := program
	tampered.TurnPrompts[0] = "changed after fingerprint"
	_, err = NewStandardAuthoringCodexAgentTurnExecutor(StandardAuthoringCodexAgentTurnExecutorConfig{
		InvocationFactory: standardAuthoringCodexTestInvocationFactory(standardAuthoringCodexTestInvocation(t)), WorkspaceRoot: t.TempDir(),
		RuntimeFactory: standardAuthoringCodexTestRuntimeFactory(runtime),
		ProgramByStage: map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{stage.Key: tampered}, Now: func() time.Time { return now },
	})
	if !errors.Is(err, ErrStandardAuthoringCodexAgentTurnConfiguration) {
		t.Fatalf("tampered prompt program error = %v, want configuration failure", err)
	}
}

func TestStandardAuthoringCodexAgentTurnExecutorRunScopedWorkspaceRequiresPreparedRunDirectory(t *testing.T) {
	stage := standardAuthoringCodexTestStage(1)
	now := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)
	root := t.TempDir()
	runID := "018f0a73-3b49-7000-8000-0000000000d1"
	program, err := NewStandardAuthoringCodexTurnProgram("standard-authoring.repo-analysis", "1", standardAuthoringCodexTestPrompts(1), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{results: []agent.TurnResult{{
		Model: CodexAppServerProductionModelID, Text: `{"ignored":"free text"}`,
	}}, submissions: [][]json.RawMessage{{standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, []byte("analysis"))}}}}
	_, err = NewStandardAuthoringCodexAgentTurnExecutor(StandardAuthoringCodexAgentTurnExecutorConfig{
		InvocationFactory: standardAuthoringCodexTestInvocationFactory(standardAuthoringCodexTestInvocation(t)), WorkspaceRoot: root,
		WorkspaceMode: StandardAuthoringCodexWorkspaceRunScoped, RuntimeFactory: standardAuthoringCodexTestRuntimeFactory(runtime),
		ProgramByStage: map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{stage.Key: program}, Now: func() time.Time { return now },
	})
	if !errors.Is(err, ErrStandardAuthoringCodexAgentTurnConfiguration) {
		t.Fatalf("RunScoped executor without frozen source verifier error = %v", err)
	}
	executor, err := NewStandardAuthoringCodexAgentTurnExecutor(StandardAuthoringCodexAgentTurnExecutorConfig{
		InvocationFactory: standardAuthoringCodexTestInvocationFactory(standardAuthoringCodexTestInvocation(t)), WorkspaceRoot: root,
		WorkspaceMode: StandardAuthoringCodexWorkspaceRunScoped, SourceVerifier: standardAuthoringCodexTestFrozenSourceVerifier{}, RuntimeFactory: standardAuthoringCodexTestRuntimeFactory(runtime),
		ProgramByStage: map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{stage.Key: program}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _, _ := standardAuthoringCodexTestRequest(stage, []byte("input"), now)
	request.Execution.ID = runID
	invocation := StageOperationInvocation{Request: request, Resolution: workflowadapter.StageOperationResolution{
		StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{Payload: standardAuthoringCodexTestPayload(1)},
	}}
	result, err := executor.ExecuteAgentTurn(context.Background(), invocation, standardAuthoringCodexTestPayload(1))
	if err != nil || result.ErrorText != standardAuthoringCodexFailureConfiguration || len(runtime.openRequests) != 0 {
		t.Fatalf("unprepared RunScoped workspace result=%+v err=%v opens=%d", result, err, len(runtime.openRequests))
	}
	workspace := filepath.Join(root, runID, StandardAuthoringCodexRunSourceDirectory)
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "src", "lib.rs"), []byte("pub fn fixture() {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	standardAuthoringCodexSealTestSourceTree(t, workspace)
	result, err = executor.ExecuteAgentTurn(context.Background(), invocation, standardAuthoringCodexTestPayload(1))
	if err != nil || result.Outcome.Status != workflowkit.StatusCompleted || len(runtime.openRequests) != 1 {
		t.Fatalf("prepared RunScoped workspace result=%+v err=%v opens=%d", result, err, len(runtime.openRequests))
	}
	if runtime.openRequests[0].ProjectPath != workspace || len(runtime.openRequests[0].WorkspaceRoots) != 1 || runtime.openRequests[0].WorkspaceRoots[0] != workspace {
		t.Fatalf("RunScoped Codex workspace = %+v, want %q", runtime.openRequests[0], workspace)
	}
}

func TestStandardAuthoringCodexAgentTurnExecutorWorkspaceWriteUsesIsolatedAttemptCopy(t *testing.T) {
	stage := standardAuthoringCodexTestStage(1)
	now := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	root := t.TempDir()
	t.Cleanup(func() { standardAuthoringCodexRemoveTree(root) })
	runID := "019f8397-7a65-7000-8000-0000000000a1"
	sourceRoot := filepath.Join(root, runID, StandardAuthoringCodexRunSourceDirectory)
	if err := os.MkdirAll(filepath.Join(sourceRoot, "src"), 0o750); err != nil {
		t.Fatal(err)
	}
	sourceFile := filepath.Join(sourceRoot, "src", "lib.rs")
	if err := os.WriteFile(sourceFile, []byte("pub fn frozen() {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	standardAuthoringCodexSealTestSourceTree(t, sourceRoot)

	request, _, _ := standardAuthoringCodexTestRequest(stage, []byte("input"), now)
	request.Execution.ID = runID
	request.Claim.Stage.StageAttempt.ID = "stage-attempt-write-1"
	workRoot, err := StandardAuthoringAttemptWorkspacePath(root, runID, stage.Key, string(request.Claim.Stage.StageAttempt.ID))
	if err != nil {
		t.Fatal(err)
	}
	workSourceFile := filepath.Join(workRoot, StandardAuthoringCodexAttemptSourceDirectory, "src", "lib.rs")
	workTaskFile := filepath.Join(workRoot, StandardAuthoringCodexAttemptTaskDirectory, "environment", "Dockerfile")
	conversation := &standardAuthoringCodexConversationStub{
		results:     []agent.TurnResult{{Model: CodexAppServerProductionModelID, Text: `{"ignored":"free text"}`}},
		submissions: [][]json.RawMessage{{standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, []byte("analysis"))}},
		afterSubmissions: func(int) error {
			if err := os.MkdirAll(filepath.Dir(workTaskFile), 0o750); err != nil {
				return err
			}
			return os.WriteFile(workTaskFile, []byte("FROM scratch\n"), 0o640)
		},
	}
	runtime := &standardAuthoringCodexRuntimeStub{conversation: conversation}
	invocation := standardAuthoringCodexTestInvocation(t)
	invocation.SandboxMode = CodexAppServerSandboxModeWorkspaceWrite
	invocation.SandboxPolicy = CodexAppServerSandboxPolicyWorkspaceWrite
	program, err := NewStandardAuthoringCodexTurnProgram("standard-authoring.repo-analysis", "1", standardAuthoringCodexTestPrompts(1), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewStandardAuthoringCodexAgentTurnExecutor(StandardAuthoringCodexAgentTurnExecutorConfig{
		InvocationFactory: standardAuthoringCodexTestInvocationFactory(invocation), WorkspaceRoot: root,
		WorkspaceMode: StandardAuthoringCodexWorkspaceRunScoped, SourceVerifier: standardAuthoringCodexTestFrozenSourceVerifier{}, RuntimeFactory: standardAuthoringCodexTestRuntimeFactory(runtime),
		ProgramByStage: map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{stage.Key: program}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: request, Resolution: workflowadapter.StageOperationResolution{
			StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{Payload: standardAuthoringCodexTestPayload(1)},
		},
	}, standardAuthoringCodexTestPayload(1))
	if err != nil || result.Outcome.Status != workflowkit.StatusCompleted || result.Outcome.Verdict != workflowkit.VerdictPass {
		t.Fatalf("workspace-write result=%+v err=%v", result, err)
	}
	if len(runtime.openRequests) != 1 {
		t.Fatalf("workspace-write opens=%d, want one", len(runtime.openRequests))
	}
	open := runtime.openRequests[0]
	if open.ProjectPath != workRoot || len(open.WorkspaceRoots) != 1 || open.WorkspaceRoots[0] != workRoot || open.SandboxMode != CodexAppServerSandboxModeWorkspaceWrite || open.SandboxPolicy != CodexAppServerSandboxPolicyWorkspaceWrite || open.NetworkAccess {
		t.Fatalf("workspace-write request = %+v, want isolated %q", open, workRoot)
	}
	if err := ValidateStandardAuthoringAttemptWorkspacePath(root, runID, stage.Key, string(request.Claim.Stage.StageAttempt.ID), workRoot); err != nil {
		t.Fatalf("validate published attempt workspace: %v", err)
	}
	frozenContent, err := os.ReadFile(sourceFile)
	if err != nil || string(frozenContent) != "pub fn frozen() {}\n" {
		t.Fatalf("immutable source content = %q, %v", frozenContent, err)
	}
	frozenInfo, err := os.Lstat(sourceFile)
	if err != nil || frozenInfo.Mode().Perm() != 0o444 {
		t.Fatalf("immutable source mode = %v, %v", frozenInfo, err)
	}
	workContent, err := os.ReadFile(workSourceFile)
	if err != nil || string(workContent) != "pub fn frozen() {}\n" {
		t.Fatalf("read-only source copy content = %q, %v", workContent, err)
	}
	workInfo, err := os.Lstat(workSourceFile)
	if err != nil || !workInfo.Mode().IsRegular() || workInfo.Mode().Perm()&0o222 != 0 || os.SameFile(frozenInfo, workInfo) {
		t.Fatalf("source copy is not an independent read-only file: frozen=%v work=%v err=%v", frozenInfo, workInfo, err)
	}
	for _, path := range []string{
		filepath.Join(workRoot, StandardAuthoringCodexAttemptSourceDirectory),
		filepath.Join(workRoot, StandardAuthoringCodexAttemptSourceDirectory, "src"),
	} {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.IsDir() || info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("read-only attempt source path %q = %v, %v", path, info, statErr)
		}
	}
	workRootInfo, err := os.Lstat(workRoot)
	if err != nil || !workRootInfo.IsDir() || workRootInfo.Mode().Perm()&0o200 == 0 {
		t.Fatalf("owner-writable attempt root for Codex sandbox state = %v, %v", workRootInfo, err)
	}
	taskInfo, err := os.Lstat(filepath.Join(workRoot, StandardAuthoringCodexAttemptTaskDirectory))
	if err != nil || !taskInfo.IsDir() || taskInfo.Mode().Perm()&0o200 == 0 {
		t.Fatalf("writable task root = %v, %v", taskInfo, err)
	}
	if taskContent, err := os.ReadFile(workTaskFile); err != nil || string(taskContent) != "FROM scratch\n" {
		t.Fatalf("fixed task candidate = %q, %v", taskContent, err)
	}
	replayed, err := executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: request, Resolution: workflowadapter.StageOperationResolution{
			StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{Payload: standardAuthoringCodexTestPayload(1)},
		},
	}, standardAuthoringCodexTestPayload(1))
	if err != nil || replayed.ErrorText != standardAuthoringCodexFailureWorkspace || len(runtime.openRequests) != 1 {
		t.Fatalf("same-attempt replay result=%+v err=%v opens=%d, want fenced workspace failure", replayed, err, len(runtime.openRequests))
	}

	request2, _, _ := standardAuthoringCodexTestRequest(stage, []byte("input"), now)
	request2.Execution.ID = runID
	request2.Claim.Stage.StageAttempt.ID = "stage-attempt-write-2"
	workRoot2, err := StandardAuthoringAttemptWorkspacePath(root, runID, stage.Key, string(request2.Claim.Stage.StageAttempt.ID))
	if err != nil {
		t.Fatal(err)
	}
	runtime2 := &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{
		results:     []agent.TurnResult{{Model: CodexAppServerProductionModelID, Text: `{"ignored":"free text"}`}},
		submissions: [][]json.RawMessage{{standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, []byte("analysis-2"))}},
	}}
	executor2, err := NewStandardAuthoringCodexAgentTurnExecutor(StandardAuthoringCodexAgentTurnExecutorConfig{
		InvocationFactory: standardAuthoringCodexTestInvocationFactory(invocation), WorkspaceRoot: root,
		WorkspaceMode: StandardAuthoringCodexWorkspaceRunScoped, SourceVerifier: standardAuthoringCodexTestFrozenSourceVerifier{}, RuntimeFactory: standardAuthoringCodexTestRuntimeFactory(runtime2),
		ProgramByStage: map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{stage.Key: program}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	result2, err := executor2.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: request2, Resolution: workflowadapter.StageOperationResolution{
			StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{Payload: standardAuthoringCodexTestPayload(1)},
		},
	}, standardAuthoringCodexTestPayload(1))
	if err != nil || result2.Outcome.Status != workflowkit.StatusCompleted || workRoot2 == workRoot || len(runtime2.openRequests) != 1 || runtime2.openRequests[0].ProjectPath != workRoot2 {
		t.Fatalf("second attempt result=%+v err=%v root=%q opens=%+v", result2, err, workRoot2, runtime2.openRequests)
	}
	workSourceFile2 := filepath.Join(workRoot2, StandardAuthoringCodexAttemptSourceDirectory, "src", "lib.rs")
	workContent2, err := os.ReadFile(workSourceFile2)
	if err != nil || string(workContent2) != "pub fn frozen() {}\n" {
		t.Fatalf("second attempt did not receive a fresh baseline copy: %q, %v", workContent2, err)
	}
	workInfo2, err := os.Lstat(workSourceFile2)
	if err != nil || os.SameFile(workInfo, workInfo2) {
		t.Fatalf("attempt workspaces share a source-copy inode: first=%v second=%v err=%v", workInfo, workInfo2, err)
	}

	hardlink := filepath.Join(workRoot, StandardAuthoringCodexAttemptTaskDirectory, "linked-source")
	if err := os.Link(workSourceFile, hardlink); err != nil {
		t.Fatal(err)
	}
	if err := ValidateStandardAuthoringAttemptWorkspacePath(root, runID, stage.Key, string(request.Claim.Stage.StageAttempt.ID), workRoot); !errors.Is(err, ErrStandardAuthoringCodexAgentTurnConfiguration) {
		t.Fatalf("hard-linked work entry validation error = %v", err)
	}
	if err := os.Remove(hardlink); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(workRoot, StandardAuthoringCodexAttemptTaskDirectory, "linked-outside")
	if err := os.Symlink(t.TempDir(), symlink); err != nil {
		t.Fatal(err)
	}
	if err := ValidateStandardAuthoringAttemptWorkspacePath(root, runID, stage.Key, string(request.Claim.Stage.StageAttempt.ID), workRoot); !errors.Is(err, ErrStandardAuthoringCodexAgentTurnConfiguration) {
		t.Fatalf("symlinked work entry validation error = %v", err)
	}
}

func TestStandardAuthoringCodexCopySourceTreePreservesOnlyInRootLinks(t *testing.T) {
	ctx := context.Background()
	sourceRoot := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(filepath.Join(sourceRoot, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "nested", "target.txt"), []byte("frozen\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/target.txt", filepath.Join(sourceRoot, "file-link")); err != nil {
		t.Skipf("create file link: %v", err)
	}
	if err := os.Symlink("nested", filepath.Join(sourceRoot, "directory-link")); err != nil {
		t.Skipf("create directory link: %v", err)
	}
	workRoot := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(workRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { standardAuthoringCodexRemoveTree(workRoot) })
	destinationRoot := filepath.Join(workRoot, StandardAuthoringCodexAttemptSourceDirectory)
	if err := standardAuthoringCodexCopySourceTree(ctx, sourceRoot, destinationRoot); err != nil {
		t.Fatalf("copy source tree with in-root links: %v", err)
	}
	for name, want := range map[string]string{"file-link": "nested/target.txt", "directory-link": "nested"} {
		target, err := os.Readlink(filepath.Join(destinationRoot, name))
		if err != nil || target != want {
			t.Fatalf("copied link %q = %q, %v; want %q", name, target, err, want)
		}
	}
	if content, err := os.ReadFile(filepath.Join(destinationRoot, "file-link")); err != nil || string(content) != "frozen\n" {
		t.Fatalf("copied file link content = %q, %v", content, err)
	}
	if err := standardAuthoringCodexValidateWorkTree(workRoot); err != nil {
		t.Fatalf("validate attempt tree with safe source links: %v", err)
	}

	unsafeRoot := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(unsafeRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside", filepath.Join(unsafeRoot, "escape")); err != nil {
		t.Skipf("create unsafe link: %v", err)
	}
	if err := standardAuthoringCodexCopySourceTree(ctx, unsafeRoot, filepath.Join(t.TempDir(), "destination")); !errors.Is(err, ErrStandardAuthoringCodexAgentTurnConfiguration) {
		t.Fatalf("copy source tree with escaping link error = %v", err)
	}
}

func TestStandardAuthoringAttemptWorkspacePathRejectsUnsafeIdentityAndSymlinkAncestor(t *testing.T) {
	root := t.TempDir()
	runID := "019f8397-7a65-7000-8000-0000000000a2"
	runRoot := filepath.Join(root, runID)
	if err := os.Mkdir(runRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		stage   workflowkit.StageKey
		attempt string
	}{
		"stage traversal":   {stage: "../repo_analyze", attempt: "attempt-1"},
		"attempt traversal": {stage: "repo_analyze", attempt: "../attempt-1"},
		"attempt separator": {stage: "repo_analyze", attempt: "nested/attempt"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := StandardAuthoringAttemptWorkspacePath(root, runID, test.stage, test.attempt); !errors.Is(err, ErrStandardAuthoringCodexAgentTurnConfiguration) {
				t.Fatalf("unsafe identity error = %v", err)
			}
		})
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(runRoot, StandardAuthoringCodexRunAttemptsDirectory)); err != nil {
		t.Fatal(err)
	}
	if _, err := StandardAuthoringAttemptWorkspacePath(root, runID, "repo_analyze", "attempt-1"); !errors.Is(err, ErrStandardAuthoringCodexAgentTurnConfiguration) {
		t.Fatalf("symlinked attempt ancestor error = %v", err)
	}
}

func TestStandardAuthoringCodexAgentTurnExecutorRejectsWritableSourceEntryBeforeOpeningConversation(t *testing.T) {
	stage := standardAuthoringCodexTestStage(1)
	now := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)
	root := t.TempDir()
	runID := "018f0a73-3b49-7000-8000-0000000000d2"
	workspace := filepath.Join(root, runID, StandardAuthoringCodexRunSourceDirectory)
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	writable := filepath.Join(workspace, "Cargo.toml")
	if err := os.WriteFile(writable, []byte("[package]\nname = \"fixture\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	standardAuthoringCodexSealTestSourceTree(t, workspace)
	if err := os.Chmod(writable, 0o640); err != nil {
		t.Fatal(err)
	}

	program, err := NewStandardAuthoringCodexTurnProgram("standard-authoring.repo-analysis", "1", standardAuthoringCodexTestPrompts(1), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{}}
	executor, err := NewStandardAuthoringCodexAgentTurnExecutor(StandardAuthoringCodexAgentTurnExecutorConfig{
		InvocationFactory: standardAuthoringCodexTestInvocationFactory(standardAuthoringCodexTestInvocation(t)), WorkspaceRoot: root,
		WorkspaceMode: StandardAuthoringCodexWorkspaceRunScoped, SourceVerifier: standardAuthoringCodexTestFrozenSourceVerifier{}, RuntimeFactory: standardAuthoringCodexTestRuntimeFactory(runtime),
		ProgramByStage: map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{stage.Key: program}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _, _ := standardAuthoringCodexTestRequest(stage, []byte("input"), now)
	request.Execution.ID = runID
	result, err := executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: request, Resolution: workflowadapter.StageOperationResolution{
			StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{Payload: standardAuthoringCodexTestPayload(1)},
		},
	}, standardAuthoringCodexTestPayload(1))
	if err != nil || result.ErrorText != standardAuthoringCodexFailureSource || len(runtime.openRequests) != 0 {
		t.Fatalf("writable RunScoped source result=%+v err=%v opens=%d", result, err, len(runtime.openRequests))
	}
}

func TestStandardAuthoringCodexAgentTurnExecutorRejectsSubmittedOutputAfterTurnMutatesFrozenSourceAndRestoresMode(t *testing.T) {
	stage := standardAuthoringCodexTestStage(1)
	now := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	root := t.TempDir()
	runID := "018f0a73-3b49-7000-8000-0000000000d3"
	workspace := filepath.Join(root, runID, StandardAuthoringCodexRunSourceDirectory)
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "src", "lib.rs")
	if err := os.WriteFile(target, []byte("pub fn frozen() {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	standardAuthoringCodexSealTestSourceTree(t, workspace)
	baseline, err := standardAuthoringCodexSourceTreeIdentity(workspace)
	if err != nil {
		t.Fatal(err)
	}
	verifications := 0
	verifier := standardAuthoringCodexFrozenSourceVerifierFunc(func(_ context.Context, _ workflowkit.FrozenExecution, sourceRoot string) (workflowkit.Fingerprint, error) {
		verifications++
		identity, verifyErr := standardAuthoringCodexSourceTreeIdentity(sourceRoot)
		if verifyErr != nil {
			return "", verifyErr
		}
		if identity != baseline {
			return "", errors.New("source differs from frozen snapshot")
		}
		return identity, nil
	})
	conversation := &standardAuthoringCodexConversationStub{
		results:     []agent.TurnResult{{Model: CodexAppServerProductionModelID, Text: `{"ignored":"free text"}`}},
		submissions: [][]json.RawMessage{{standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, []byte("must-not-be-accepted"))}},
		afterSubmissions: func(int) error {
			if err := os.Chmod(target, 0o644); err != nil {
				return err
			}
			if err := os.WriteFile(target, []byte("pub fn attacker_controlled() {}\n"), 0o644); err != nil {
				return err
			}
			return os.Chmod(target, 0o444)
		},
	}
	runtime := &standardAuthoringCodexRuntimeStub{conversation: conversation}
	program, err := NewStandardAuthoringCodexTurnProgram("standard-authoring.repo-analysis", "1", standardAuthoringCodexTestPrompts(1), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewStandardAuthoringCodexAgentTurnExecutor(StandardAuthoringCodexAgentTurnExecutorConfig{
		InvocationFactory: standardAuthoringCodexTestInvocationFactory(standardAuthoringCodexTestInvocation(t)), WorkspaceRoot: root,
		WorkspaceMode: StandardAuthoringCodexWorkspaceRunScoped, SourceVerifier: verifier, RuntimeFactory: standardAuthoringCodexTestRuntimeFactory(runtime),
		ProgramByStage: map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{stage.Key: program}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _, _ := standardAuthoringCodexTestRequest(stage, []byte("input"), now)
	request.Execution.ID = runID
	result, err := executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: request, Resolution: workflowadapter.StageOperationResolution{
			StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{Payload: standardAuthoringCodexTestPayload(1)},
		},
	}, standardAuthoringCodexTestPayload(1))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Status != workflowkit.StatusInfraFailed || result.Outcome.Failure != workflowkit.FailurePolicy || result.ErrorText != standardAuthoringCodexFailureSource || len(result.Artifacts) != 0 {
		t.Fatalf("source mutation result = %+v, want rejected source-integrity failure", result)
	}
	if len(runtime.openRequests) != 1 || runtime.openRequests[0].SandboxMode != CodexAppServerSandboxModeReadOnly || runtime.openRequests[0].SandboxPolicy != CodexAppServerSandboxPolicyReadOnly {
		t.Fatalf("source mutation runtime policy = %+v, want one read-only turn", runtime.openRequests)
	}
	if verifications != 3 || len(conversation.submissionResponses) != 1 || conversation.closed != 1 {
		t.Fatalf("source mutation proof calls=%d submissions=%d closes=%d, want 3/1/1", verifications, len(conversation.submissionResponses), conversation.closed)
	}
}

func TestStandardAuthoringCodexAgentTurnExecutorWorkspaceWriteStillRejectsFrozenSourceMutation(t *testing.T) {
	stage := standardAuthoringCodexTestStage(1)
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	root := t.TempDir()
	t.Cleanup(func() { standardAuthoringCodexRemoveTree(root) })
	runID := "019f8397-7a65-7000-8000-0000000000a3"
	sourceRoot := filepath.Join(root, runID, StandardAuthoringCodexRunSourceDirectory)
	if err := os.MkdirAll(filepath.Join(sourceRoot, "src"), 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(sourceRoot, "src", "lib.rs")
	if err := os.WriteFile(target, []byte("pub fn frozen() {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	standardAuthoringCodexSealTestSourceTree(t, sourceRoot)
	baseline, err := standardAuthoringCodexSourceTreeIdentity(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	verifications := 0
	verifier := standardAuthoringCodexFrozenSourceVerifierFunc(func(_ context.Context, _ workflowkit.FrozenExecution, checkedRoot string) (workflowkit.Fingerprint, error) {
		verifications++
		identity, verifyErr := standardAuthoringCodexSourceTreeIdentity(checkedRoot)
		if verifyErr != nil {
			return "", verifyErr
		}
		if identity != baseline {
			return "", errors.New("source differs from frozen snapshot")
		}
		return identity, nil
	})
	conversation := &standardAuthoringCodexConversationStub{
		results:     []agent.TurnResult{{Model: CodexAppServerProductionModelID, Text: `{"ignored":"free text"}`}},
		submissions: [][]json.RawMessage{{standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, []byte("must-not-pass"))}},
		afterSubmissions: func(int) error {
			if err := os.Chmod(target, 0o644); err != nil {
				return err
			}
			if err := os.WriteFile(target, []byte("pub fn changed() {}\n"), 0o644); err != nil {
				return err
			}
			return os.Chmod(target, 0o444)
		},
	}
	runtime := &standardAuthoringCodexRuntimeStub{conversation: conversation}
	invocation := standardAuthoringCodexTestInvocation(t)
	invocation.SandboxMode = CodexAppServerSandboxModeWorkspaceWrite
	invocation.SandboxPolicy = CodexAppServerSandboxPolicyWorkspaceWrite
	program, err := NewStandardAuthoringCodexTurnProgram("standard-authoring.repo-analysis", "1", standardAuthoringCodexTestPrompts(1), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewStandardAuthoringCodexAgentTurnExecutor(StandardAuthoringCodexAgentTurnExecutorConfig{
		InvocationFactory: standardAuthoringCodexTestInvocationFactory(invocation), WorkspaceRoot: root,
		WorkspaceMode: StandardAuthoringCodexWorkspaceRunScoped, SourceVerifier: verifier, RuntimeFactory: standardAuthoringCodexTestRuntimeFactory(runtime),
		ProgramByStage: map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{stage.Key: program}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _, _ := standardAuthoringCodexTestRequest(stage, []byte("input"), now)
	request.Execution.ID = runID
	request.Claim.Stage.StageAttempt.ID = "stage-attempt-mutation"
	result, err := executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: request, Resolution: workflowadapter.StageOperationResolution{
			StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{Payload: standardAuthoringCodexTestPayload(1)},
		},
	}, standardAuthoringCodexTestPayload(1))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Status != workflowkit.StatusInfraFailed || result.Outcome.Failure != workflowkit.FailurePolicy || result.ErrorText != standardAuthoringCodexFailureSource || len(result.Artifacts) != 0 {
		t.Fatalf("workspace-write source mutation result = %+v", result)
	}
	if verifications != 4 || len(runtime.openRequests) != 1 || runtime.openRequests[0].ProjectPath == sourceRoot || conversation.closed != 1 {
		t.Fatalf("workspace-write mutation proof calls=%d opens=%+v closes=%d", verifications, runtime.openRequests, conversation.closed)
	}
}

func TestStandardAuthoringCodexAgentTurnExecutorUsesRootContractDockerfileEnvironment(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	stage := standardAuthoringCodexTestDockerfileStage(1)
	contract := standardAuthoringCodexTestRootContract()
	wrongDockerfile := []byte("FROM registry.example.com/team/other:1.2.3@sha256:" + strings.Repeat("b", 64) + "\n")
	acceptedDockerfile := []byte("FROM " + contract.Environment.BaseImage + "\nRUN printf '%s\\n' ready\n")
	runtime := &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{
		results: []agent.TurnResult{{Model: CodexAppServerProductionModelID, Text: `{"ignored":"free text"}`}},
		submissions: [][]json.RawMessage{{
			standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, wrongDockerfile),
			standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, acceptedDockerfile),
		}},
	}}
	program, err := NewStandardAuthoringCodexTurnProgram("standard-authoring.dockerfile-generate", "1", standardAuthoringCodexTestPrompts(1), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewStandardAuthoringCodexAgentTurnExecutor(StandardAuthoringCodexAgentTurnExecutorConfig{
		InvocationFactory: standardAuthoringCodexTestInvocationFactory(standardAuthoringCodexTestInvocation(t)), WorkspaceRoot: standardAuthoringCodexReadOnlyTestWorkspace(t),
		RuntimeFactory: standardAuthoringCodexTestRuntimeFactory(runtime),
		ProgramByStage: map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{stage.Key: program},
		Now:            func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _, usages := standardAuthoringCodexTestDockerfileRequest(t, stage, now)
	result, err := executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: request,
		Resolution: workflowadapter.StageOperationResolution{
			StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{Payload: standardAuthoringCodexTestPayload(1)},
		},
	}, standardAuthoringCodexTestPayload(1))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Status != workflowkit.StatusCompleted || result.Outcome.Verdict != workflowkit.VerdictPass || len(result.Artifacts) != 1 || string(result.Artifacts[0].Content) != string(acceptedDockerfile) {
		t.Fatalf("Dockerfile result = %+v", result)
	}
	if len(runtime.conversation.requests) != 1 || len(runtime.conversation.requests[0].Input) != 1 {
		t.Fatalf("turn requests = %+v", runtime.conversation.requests)
	}
	firstInput := runtime.conversation.requests[0].Input[0].Text
	if !strings.Contains(firstInput, `"artifact_id":"input-authoring_contract"`) || strings.Contains(firstInput, `"environment_policy"`) || !strings.Contains(firstInput, `"context"`) || !strings.Contains(firstInput, contract.Environment.BaseImage) {
		t.Fatalf("first request did not contain the root contract and its validated base image: %q", firstInput)
	}
	if len(runtime.conversation.submissionResponses) != 2 {
		t.Fatalf("submission responses = %+v", runtime.conversation.submissionResponses)
	}
	firstReceipt := standardAuthoringCodexTestSubmissionReceipt(t, runtime.conversation.submissionResponses[0])
	secondReceipt := standardAuthoringCodexTestSubmissionReceipt(t, runtime.conversation.submissionResponses[1])
	if firstReceipt.Accepted || len(firstReceipt.Errors) != 1 || firstReceipt.Errors[0] != "dockerfile_contract_base_image_mismatch" || !secondReceipt.Accepted {
		t.Fatalf("Dockerfile submission receipts = first:%+v second:%+v", firstReceipt, secondReceipt)
	}
	if standardAuthoringCodexTestUsageCount(*usages, "agent_turn") != 1 || standardAuthoringCodexTestUsageCount(*usages, standardAuthoringCodexOutputSubmissionQuotaDimension) != 2 {
		t.Fatalf("usage records = %+v, want one turn and two submissions", *usages)
	}
}

func TestStandardAuthoringCodexAgentTurnExecutorRoutesV2FixedFileScriptSubmission(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 30, 0, 0, time.UTC)
	root := t.TempDir()
	t.Cleanup(func() { standardAuthoringCodexRemoveTree(root) })
	const runID = "019f8397-7a65-7000-8000-0000000000b1"
	standardAuthoringCodexTestPrepareRunScopedSource(t, root, runID)
	template := workflowadapter.StandardAuthoringCurrentTemplateReference()
	stage := standardAuthoringCodexTestAuthoringScriptStage(t, template, workflowkit.StageKey(workflowadapter.SolveGen))
	program, err := NewStandardAuthoringCodexTurnProgram("standard-authoring.solve-generate", "test", standardAuthoringCodexTestPrompts(1), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	contract := standardAuthoringCodexStructuredClaimTestContract(t)
	request, contractDigest := standardAuthoringCodexContractBoundTestRequest(t, stage, contract, now)
	request.Execution = standardAuthoringCodexTestSealedExecution(t, template, runID)
	request.Claim.Stage.StageAttempt.ID = "fixed-file-attempt"
	content := []byte("#!/bin/sh\nset -eu\nprintf 'generated script\\n'\n")
	conversation := &standardAuthoringCodexConversationStub{
		results:     []agent.TurnResult{{Model: CodexAppServerProductionModelID, Text: `{"ignored":"free text"}`}},
		submissions: [][]json.RawMessage{{standardAuthoringCodexFixedFilePassTestCandidate(t, contractDigest)}},
		beforeSubmissions: func(int) error {
			workRoot, err := StandardAuthoringAttemptWorkspacePath(root, runID, stage.Key, "fixed-file-attempt")
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(workRoot, StandardAuthoringCodexAttemptTaskDirectory, "solution", "solve.sh"), content, 0o750)
		},
	}
	runtime := &standardAuthoringCodexRuntimeStub{conversation: conversation}
	invocation := standardAuthoringCodexTestInvocation(t)
	invocation.SandboxMode = CodexAppServerSandboxModeWorkspaceWrite
	invocation.SandboxPolicy = CodexAppServerSandboxPolicyWorkspaceWrite
	executor, err := NewStandardAuthoringCodexAgentTurnExecutor(StandardAuthoringCodexAgentTurnExecutorConfig{
		InvocationFactory: standardAuthoringCodexTestInvocationFactory(invocation), WorkspaceRoot: root,
		WorkspaceMode: StandardAuthoringCodexWorkspaceRunScoped, SourceVerifier: standardAuthoringCodexTestFrozenSourceVerifier{},
		RuntimeFactory: standardAuthoringCodexTestRuntimeFactory(runtime),
		ProgramByStage: map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{stage.Key: program},
		Now:            func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := standardAuthoringCodexTestPayload(1)
	result, err := executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: request,
		Resolution: workflowadapter.StageOperationResolution{
			Template: template, StageKey: stage.Key,
			Operation: workflowadapter.StageOperationBinding{Payload: payload},
		},
	}, payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Status != workflowkit.StatusCompleted || result.Outcome.Verdict != workflowkit.VerdictPass || len(result.Artifacts) != 1 || string(result.Artifacts[0].Content) != string(content) {
		t.Fatalf("v2 fixed-file script result = %+v", result)
	}
	if len(conversation.dynamicTools) != 1 || !json.Valid(conversation.dynamicTools[0].InputSchema) {
		t.Fatalf("v2 fixed-file dynamic tools = %+v", conversation.dynamicTools)
	}
	var toolSchema map[string]any
	if err := json.Unmarshal(conversation.dynamicTools[0].InputSchema, &toolSchema); err != nil {
		t.Fatal(err)
	}
	properties, ok := toolSchema["properties"].(map[string]any)
	if !ok || properties["artifacts"] != nil || properties["contract_digest"] == nil {
		t.Fatalf("v2 fixed-file tool schema = %#v", toolSchema)
	}
	if len(conversation.requests) != 1 || len(conversation.requests[0].Input) != 1 {
		t.Fatalf("v2 fixed-file turn requests = %+v", conversation.requests)
	}
	var document struct {
		OutputSchemaFingerprint workflowkit.Fingerprint `json:"output_schema_fingerprint"`
	}
	if err := json.Unmarshal([]byte(conversation.requests[0].Input[0].Text), &document); err != nil {
		t.Fatal(err)
	}
	if document.OutputSchemaFingerprint != StandardAuthoringCodexFixedFileOutputSchemaFingerprint() {
		t.Fatalf("v2 fixed-file output schema fingerprint = %q", document.OutputSchemaFingerprint)
	}
}

func TestStandardAuthoringCodexFixedFileInvocationRequiresSealedTemplateConsistency(t *testing.T) {
	const runID = "019f8397-7a65-7000-8000-0000000000b2"
	fixed := workflowadapter.StandardAuthoringCurrentTemplateReference()
	mismatch := workflowadapter.TemplateReference{ID: fixed.ID, Version: "1.9.9"}
	stage := standardAuthoringCodexTestAuthoringScriptStage(t, fixed, workflowkit.StageKey(workflowadapter.SolveGen))
	request, _, _ := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), time.Date(2026, 7, 21, 10, 30, 0, 0, time.UTC))
	request.Execution = standardAuthoringCodexTestSealedExecution(t, fixed, runID)
	invocation := StageOperationInvocation{
		Request: request,
		Resolution: workflowadapter.StageOperationResolution{Template: fixed, StageKey: stage.Key,
			Operation: workflowadapter.StageOperationBinding{Payload: standardAuthoringCodexTestPayload(1)}},
	}
	sealed, usesFixedFile, err := standardAuthoringCodexFixedFileInvocation(invocation)
	if err != nil || !sealed.Equal(fixed) || !usesFixedFile {
		t.Fatalf("matching fixed template invocation = template:%+v fixed:%t err:%v", sealed, usesFixedFile, err)
	}

	for _, testCase := range []struct {
		name   string
		mutate func(*StageOperationInvocation)
	}{
		{
			name: "request and resolution name an uninstalled version",
			mutate: func(candidate *StageOperationInvocation) {
				candidate.Request.Execution.Workflow.ID = mismatch.ID
				candidate.Request.Execution.Workflow.Version = mismatch.Version
				candidate.Resolution.Template = mismatch
			},
		},
		{
			name: "resolution names an uninstalled version",
			mutate: func(candidate *StageOperationInvocation) {
				candidate.Resolution.Template = mismatch
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := invocation.clone()
			testCase.mutate(&candidate)
			if _, _, err := standardAuthoringCodexFixedFileInvocation(candidate); err == nil {
				t.Fatal("fixed-file template mismatch was accepted")
			}
		})
	}

}

func standardAuthoringCodexTestPrepareRunScopedSource(t *testing.T, root, runID string) {
	t.Helper()
	sourceRoot := filepath.Join(root, runID, StandardAuthoringCodexRunSourceDirectory)
	if err := os.MkdirAll(filepath.Join(sourceRoot, "src"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "src", "lib.rs"), []byte("pub fn frozen() {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	standardAuthoringCodexSealTestSourceTree(t, sourceRoot)
}

func standardAuthoringCodexTestAuthoringScriptStage(t *testing.T, template workflowadapter.TemplateReference, key workflowkit.StageKey) workflowkit.StageDescriptor {
	t.Helper()
	workflow, err := workflowadapter.ResolveWorkflowTemplate(template)
	if err != nil {
		t.Fatal(err)
	}
	expected, found := workflow.Catalog.Stage(key)
	if !found || len(expected.Outputs) != 1 {
		t.Fatalf("authoring script stage %q = %+v found=%t", key, expected, found)
	}
	stage := standardAuthoringCodexTestArtifactStage(1, key, expected.Outputs[0].Name)
	stage.Version = expected.Version
	stage.Plugin = workflowkit.PluginBinding{ID: expected.Plugin.ID, Version: expected.Plugin.Version}
	// Fixed-file submission tests isolate the contract binding; upstream package
	// inputs are verified by the catalog and materialization suites.
	stage.Inputs = []workflowkit.ArtifactSpec{{Name: workflowadapter.AuthoringContractArtifact, SchemaVersion: workflowadapter.AuthoringContractSchemaVersion, Required: true}}
	stage.Outputs = append([]workflowkit.ArtifactSpec(nil), expected.Outputs...)
	stage.Verdicts = expected.Verdicts.Clone()
	return stage
}

func standardAuthoringCodexTestSealedExecution(t *testing.T, template workflowadapter.TemplateReference, runID string) workflowkit.FrozenExecution {
	t.Helper()
	workflow, err := workflowadapter.ResolveWorkflowTemplate(template)
	if err != nil {
		t.Fatal(err)
	}
	selection := workflowadapter.RunSelectionReference{
		Kind:                  workflowadapter.RunSelectionAuthoringSession,
		AuthoringSourceID:     "019f8397-7a65-7000-8000-0000000000c1",
		AuthoringSessionID:    "019f8397-7a65-7000-8000-0000000000c2",
		AuthoringSourceDigest: workflowkit.SubjectDigest("sha256:" + strings.Repeat("c", 64)),
	}
	specification := workflowadapter.RunExecutionSpec{
		Format: workflowadapter.RunExecutionSpecFormat, Version: workflowadapter.RunExecutionSpecVersion,
		Template: template, Selection: selection,
		References: workflowadapter.ExecutionReferenceSet{
			Checkouts: []workflowadapter.CheckoutReference{{ID: "checkout-test", RevisionID: selection.AuthoringSessionID, RevisionDigest: selection.AuthoringSourceDigest}},
			Runtimes:  []workflowadapter.RuntimeReference{{ID: "runtime-test", Kind: "test", Version: "1"}},
			Providers: []workflowadapter.ProviderReference{{ID: "provider-test", Kind: "test", Version: "1"}},
		},
	}
	for _, definition := range workflow.Catalog.Stages {
		specification.Stages = append(specification.Stages, workflowadapter.UniversalStageBinding{StageBindingBase: workflowadapter.StageBindingBase{
			Type: standardAuthoringCodexTestAuthoringStageBindingType(t, definition.Key), StageKey: definition.Key,
			Plugin:         workflowkit.PluginBinding{ID: definition.Plugin.ID, Version: definition.Plugin.Version},
			ArtifactInputs: []workflowadapter.ArtifactInputReference{}, CheckoutID: "checkout-test", RuntimeID: "runtime-test", SecretIDs: []string{},
			Operation: workflowadapter.StageOperationBinding{
				ProviderID: "provider-test", OperationID: "test." + string(definition.Key), Version: "1", Payload: standardAuthoringCodexTestPayload(1),
			},
		}})
	}
	canonical, err := specification.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical %s@%s test execution spec: %v", template.ID, template.Version, err)
	}
	binding, err := workflowkit.NewOpaqueExecutionBinding(workflowadapter.RunExecutionSpecFormat, workflowadapter.RunExecutionSpecVersion, canonical)
	if err != nil {
		t.Fatal(err)
	}
	subject, err := selection.SubjectBinding()
	if err != nil {
		t.Fatal(err)
	}
	return workflowkit.FrozenExecution{
		ID: runID, Subject: subject, Workflow: workflowkit.WorkflowDescriptor{ID: template.ID, Version: template.Version}, Binding: binding,
	}
}

func standardAuthoringCodexTestAuthoringStageBindingType(t *testing.T, key workflowkit.StageKey) workflowadapter.StageBindingType {
	t.Helper()
	switch key {
	case workflowkit.StageKey(workflowadapter.RepoPrepare):
		return workflowadapter.StageBindingRepoPrepare
	case workflowkit.StageKey(workflowadapter.RepoAnalyze):
		return workflowadapter.StageBindingRepoAnalyze
	case workflowkit.StageKey(workflowadapter.TaskDesign):
		return workflowadapter.StageBindingTaskDesign
	case workflowkit.StageKey(workflowadapter.TaskReview):
		return workflowadapter.StageBindingTaskReview
	case workflowkit.StageKey(workflowadapter.GenerateTaskFiles):
		return workflowadapter.StageBindingGenerateTaskFiles
	case workflowkit.StageKey(workflowadapter.InstructionGen):
		return workflowadapter.StageBindingInstructionGen
	case workflowkit.StageKey(workflowadapter.TaskTOMLGen):
		return workflowadapter.StageBindingTaskTOMLGen
	case workflowkit.StageKey(workflowadapter.DockerfileGen):
		return workflowadapter.StageBindingDockerfileGen
	case workflowkit.StageKey(workflowadapter.DockerfileBuildValidate):
		return workflowadapter.StageBindingDockerfileBuildValidate
	case workflowkit.StageKey(workflowadapter.ContentReview):
		return workflowadapter.StageBindingContentReview
	case workflowkit.StageKey(workflowadapter.SolveGen):
		return workflowadapter.StageBindingSolveGen
	case workflowkit.StageKey(workflowadapter.TestGen):
		return workflowadapter.StageBindingTestGen
	case workflowkit.StageKey(workflowadapter.AuthoringHarness):
		return workflowadapter.StageBindingAuthoringHarness
	case workflowkit.StageKey(workflowadapter.TestsAnalysis):
		return workflowadapter.StageBindingTestsAnalysis
	case workflowkit.StageKey(workflowadapter.CodeEdgePackageAdmission):
		return workflowadapter.StageBindingCodeEdgePackageAdmission
	case workflowkit.StageKey(workflowadapter.SolutionReview):
		return workflowadapter.StageBindingSolutionReview
	case workflowkit.StageKey(workflowadapter.MaterializeTask):
		return workflowadapter.StageBindingMaterializeTask
	default:
		t.Fatalf("unsupported authoring test stage %q", key)
		return ""
	}
}

func TestStandardAuthoringCodexAgentTurnExecutorRetriesWrappedTaskTOMLWithinTurn(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	stage := standardAuthoringCodexTestArtifactStage(1, workflowkit.StageKey(workflowadapter.TaskTOMLGen), "task_toml")
	wrapped := []byte(`{"format":"harbor.artifact.v1","version":"1","metadata":{"task_type":"feature","application":"backend"},"task_toml":"[metadata]\ntask_type = \"feature\"\napplication = \"backend\"\n"}`)
	raw := []byte("schema_version = \"1.0\"\n\n[metadata]\ncode_lang = \"rust\"\ntask_type = \"feature\"\napplication = \"backend\"\nis_0_to_1 = false\n\n[task]\nname = \"harbor/request-header-count-limit\"\ndescription = \"Add request header count limiting middleware.\"\n\n[environment]\nbuild_timeout_sec = 900.0\nnetwork_mode = \"no-network\"\nworkdir = \"/workspace/source\"\n\n[verifier]\ntimeout_sec = 1800.0\n")
	runtime := &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{
		results: []agent.TurnResult{{Model: CodexAppServerProductionModelID, Text: `{"ignored":"free text"}`}},
		submissions: [][]json.RawMessage{{
			standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, wrapped),
			standardAuthoringCodexTestCandidate(t, workflowkit.VerdictPass, raw),
		}},
	}}
	program, err := NewStandardAuthoringCodexTurnProgram("standard-authoring.task-toml-generate", "1", standardAuthoringCodexTestPrompts(1), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewStandardAuthoringCodexAgentTurnExecutor(StandardAuthoringCodexAgentTurnExecutorConfig{
		InvocationFactory: standardAuthoringCodexTestInvocationFactory(standardAuthoringCodexTestInvocation(t)), WorkspaceRoot: standardAuthoringCodexReadOnlyTestWorkspace(t),
		RuntimeFactory: standardAuthoringCodexTestRuntimeFactory(runtime),
		ProgramByStage: map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{stage.Key: program},
		Now:            func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _, usages := standardAuthoringCodexTestRequest(stage, []byte("frozen input"), now)
	result, err := executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: request,
		Resolution: workflowadapter.StageOperationResolution{
			StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{Payload: standardAuthoringCodexTestPayload(1)},
		},
	}, standardAuthoringCodexTestPayload(1))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Status != workflowkit.StatusCompleted || result.Outcome.Verdict != workflowkit.VerdictPass || len(result.Artifacts) != 1 || string(result.Artifacts[0].Content) != string(raw) {
		t.Fatalf("task TOML result = %+v", result)
	}
	if len(runtime.conversation.submissionResponses) != 2 {
		t.Fatalf("submission responses = %+v", runtime.conversation.submissionResponses)
	}
	first := standardAuthoringCodexTestSubmissionReceipt(t, runtime.conversation.submissionResponses[0])
	second := standardAuthoringCodexTestSubmissionReceipt(t, runtime.conversation.submissionResponses[1])
	if first.Accepted || len(first.Errors) != 1 || first.Errors[0] != "task_toml_invalid" || !second.Accepted || len(second.Errors) != 0 {
		t.Fatalf("task TOML receipts = first:%+v second:%+v", first, second)
	}
	if standardAuthoringCodexTestUsageCount(*usages, "agent_turn") != 1 || standardAuthoringCodexTestUsageCount(*usages, standardAuthoringCodexOutputSubmissionQuotaDimension) != 2 {
		t.Fatalf("usage records = %+v, want one turn and two submissions", *usages)
	}
}

func TestStandardAuthoringCodexAgentTurnExecutorRejectsMissingRootContractBeforeOpeningConversation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	stage := standardAuthoringCodexTestDockerfileStage(1)
	runtime := &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{}}
	program, err := NewStandardAuthoringCodexTurnProgram("standard-authoring.dockerfile-generate", "1", standardAuthoringCodexTestPrompts(1), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewStandardAuthoringCodexAgentTurnExecutor(StandardAuthoringCodexAgentTurnExecutorConfig{
		InvocationFactory: standardAuthoringCodexTestInvocationFactory(standardAuthoringCodexTestInvocation(t)), WorkspaceRoot: standardAuthoringCodexReadOnlyTestWorkspace(t),
		RuntimeFactory: standardAuthoringCodexTestRuntimeFactory(runtime),
		ProgramByStage: map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{stage.Key: program},
		Now:            func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _, usages := standardAuthoringCodexTestDockerfileRequest(t, stage, now)
	request.Inputs = request.Inputs[1:]
	result, err := executor.ExecuteAgentTurn(context.Background(), StageOperationInvocation{
		Request: request,
		Resolution: workflowadapter.StageOperationResolution{
			StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{Payload: standardAuthoringCodexTestPayload(1)},
		},
	}, standardAuthoringCodexTestPayload(1))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Status != workflowkit.StatusInfraFailed || result.Outcome.Failure != workflowkit.FailurePermanent || result.ErrorText != standardAuthoringCodexFailureInput {
		t.Fatalf("missing root contract result = %+v", result)
	}
	if len(runtime.openRequests) != 0 || len(*usages) != 0 {
		t.Fatalf("missing root contract opened runtime or charged usage: opens=%d usages=%+v", len(runtime.openRequests), *usages)
	}
}

func standardAuthoringCodexTestExecutor(t *testing.T, runtime agent.Runtime, now time.Time, turns int) (*StandardAuthoringCodexAgentTurnExecutor, StandardAuthoringCodexTurnProgram) {
	t.Helper()
	program, err := NewStandardAuthoringCodexTurnProgram("standard-authoring.repo-analysis", "1", standardAuthoringCodexTestPrompts(turns), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewStandardAuthoringCodexAgentTurnExecutor(StandardAuthoringCodexAgentTurnExecutorConfig{
		InvocationFactory: standardAuthoringCodexTestInvocationFactory(standardAuthoringCodexTestInvocation(t)), WorkspaceRoot: standardAuthoringCodexReadOnlyTestWorkspace(t),
		RuntimeFactory: standardAuthoringCodexTestRuntimeFactory(runtime),
		ProgramByStage: map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{"repo_analyze": program},
		Now:            func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return executor, program
}

func standardAuthoringCodexReadOnlyTestWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "lib.rs"), []byte("pub fn fixture() {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	standardAuthoringCodexSealTestSourceTree(t, root)
	return root
}

func standardAuthoringCodexSealTestSourceTree(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o555)
		}
		return os.Chmod(path, 0o444)
	}); err != nil {
		t.Fatal(err)
	}
}

func standardAuthoringCodexTestPrompts(turns int) []string {
	prompts := make([]string, 0, turns)
	for index := 1; index <= turns; index++ {
		prompts = append(prompts, "perform frozen authoring turn "+strconv.Itoa(index))
	}
	return prompts
}

func standardAuthoringCodexTestPayload(turns int) workflowadapter.AgentTurnOperationPayload {
	return workflowadapter.AgentTurnOperationPayload{
		AgentID: CodexAppServerProductionAgentID, ModelID: CodexAppServerProductionModelID,
		ReasoningEffort: CodexAppServerProductionReasoningEffort, MaxTurns: turns,
	}
}

func standardAuthoringCodexTestInvocation(t *testing.T) CodexAppServerInvocation {
	t.Helper()
	root := t.TempDir()
	return CodexAppServerInvocation{
		AgentID: CodexAppServerProductionAgentID, AgentVersion: "1.0.0", ModelID: CodexAppServerProductionModelID, ModelVersion: "2026-07-15",
		ReasoningEffort:        CodexAppServerProductionReasoningEffort,
		JavaScriptLauncherPath: root + "/codex", NodeExecutablePath: root + "/node", CodexHomeDirectory: root,
		CLIVersionOutput: "codex-cli 0.133.0", ApprovalPolicy: CodexAppServerApprovalPolicyNever, SandboxMode: CodexAppServerSandboxModeReadOnly,
		SandboxPolicy: CodexAppServerSandboxPolicyReadOnly, NetworkAccess: false,
	}
}

func standardAuthoringCodexTestInvocationFactory(invocation CodexAppServerInvocation) StandardAuthoringCodexInvocationFactory {
	return func(context.Context, StageOperationInvocation, workflowadapter.AgentTurnOperationPayload) (CodexAppServerInvocation, error) {
		return invocation, nil
	}
}

func standardAuthoringCodexTestRuntimeFactory(runtime agent.Runtime) StandardAuthoringCodexRuntimeFactory {
	return func(CodexAppServerInvocation) agent.Runtime { return runtime }
}

type standardAuthoringCodexTestFrozenSourceVerifier struct{}

func (standardAuthoringCodexTestFrozenSourceVerifier) VerifyStandardAuthoringCodexFrozenSource(_ context.Context, _ workflowkit.FrozenExecution, root string) (workflowkit.Fingerprint, error) {
	return standardAuthoringCodexSourceTreeIdentity(root)
}

type standardAuthoringCodexFrozenSourceVerifierFunc func(context.Context, workflowkit.FrozenExecution, string) (workflowkit.Fingerprint, error)

func (verify standardAuthoringCodexFrozenSourceVerifierFunc) VerifyStandardAuthoringCodexFrozenSource(ctx context.Context, execution workflowkit.FrozenExecution, root string) (workflowkit.Fingerprint, error) {
	return verify(ctx, execution, root)
}

func standardAuthoringCodexTestStage(turns int) workflowkit.StageDescriptor {
	turnTimeout := 5 * time.Second
	attemptTimeout := time.Duration(turns) * turnTimeout
	return workflowkit.StageDescriptor{
		Key: "repo_analyze", Version: "1", Plugin: workflowkit.PluginBinding{ID: "harborfactory.repo_analyze", Version: "1"}, Group: "test",
		Inputs: []workflowkit.ArtifactSpec{
			{Name: "repo_prepared", SchemaVersion: "harbor.artifact.v1", Required: true},
			{Name: workflowadapter.AuthoringContractArtifact, SchemaVersion: workflowadapter.AuthoringContractSchemaVersion, Required: true},
		},
		Outputs:  []workflowkit.ArtifactSpec{{Name: "repo_analysis", SchemaVersion: "harbor.artifact.v1", Required: true}},
		ReadSet:  []workflowkit.ResourceKey{"test/input"},
		WriteSet: []workflowkit.ResourceKey{"test/output"},
		Effect:   workflowkit.EffectEvidenceOnly, Dispatch: workflowkit.StageDispatchAutomatic,
		Budget: workflowkit.ExecutionBudget{
			MaxTurns: turns, TurnTimeout: turnTimeout, AttemptTimeout: attemptTimeout, MaxAttempts: 1, MaxElapsed: attemptTimeout,
			Backoff: workflowkit.BackoffPolicy{RetryDelays: []time.Duration{}},
		},
		QuotaClaims: []workflowkit.QuotaClaim{
			{Dimension: "agent_turn", Units: int64(turns), ReclaimPolicy: workflowkit.ReclaimUnused},
			{Dimension: standardAuthoringCodexOutputSubmissionQuotaDimension, Units: workflowadapter.StandardAuthoringOutputSubmissionClaimUnits, ReclaimPolicy: workflowkit.ReclaimUnused},
		},
		Retry:    workflowkit.RetryPolicy{},
		Verdicts: workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass, workflowkit.VerdictNeedsRepair}},
		Reuse:    workflowkit.ReuseWhenInputsMatch,
	}
}

func standardAuthoringCodexTestDockerfileStage(turns int) workflowkit.StageDescriptor {
	stage := standardAuthoringCodexTestStage(turns)
	stage.Key = workflowkit.StageKey(workflowadapter.DockerfileGen)
	stage.Plugin = workflowkit.PluginBinding{ID: "harborfactory.dockerfile_generate", Version: "1"}
	stage.Outputs = []workflowkit.ArtifactSpec{{Name: "dockerfile", SchemaVersion: "harbor.artifact.v1", Required: true}}
	stage.Inputs = []workflowkit.ArtifactSpec{
		{Name: "repo_prepared", SchemaVersion: "harbor.artifact.v1", Required: true},
		{Name: "task_proposal", SchemaVersion: "harbor.artifact.v1", Required: true},
		{Name: workflowadapter.AuthoringContractArtifact, SchemaVersion: workflowadapter.AuthoringContractSchemaVersion, Required: true},
	}
	return stage
}

func standardAuthoringCodexTestEnvironmentPolicy(t *testing.T) workflowadapter.StandardAuthoringEnvironmentPolicy {
	t.Helper()
	policy, err := workflowadapter.NewStandardAuthoringEnvironmentPolicy("registry.example.com/team/runtime:1.2.3@sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func standardAuthoringCodexTestArtifactStage(turns int, stageKey workflowkit.StageKey, outputName string) workflowkit.StageDescriptor {
	stage := standardAuthoringCodexTestStage(turns)
	stage.Key = stageKey
	stage.Plugin = workflowkit.PluginBinding{ID: "harborfactory." + string(stageKey), Version: "1"}
	stage.Outputs = []workflowkit.ArtifactSpec{{Name: outputName, SchemaVersion: "harbor.artifact.v1", Required: true}}
	return stage
}

func standardAuthoringCodexTestRequest(stage workflowkit.StageDescriptor, content []byte, now time.Time) (workflowkit.StageExecutionRequest, *[]workflowkit.StageCheckpoint, *[]workflowkit.StageUsage) {
	binding := workflowkit.ArtifactBinding{Name: "repo_prepared", ArtifactID: "input-1", ContentDigest: workflowkit.SHA256Fingerprint(content), SchemaVersion: "harbor.artifact.v1"}
	rootContract := standardAuthoringCodexTestRootContract()
	rootContent, err := rootContract.CanonicalJSON()
	if err != nil {
		panic(err)
	}
	rootBinding := workflowkit.ArtifactBinding{
		Name: workflowadapter.AuthoringContractArtifact, ArtifactID: "authoring-contract-input",
		ContentDigest: workflowkit.SHA256Fingerprint(rootContent), SchemaVersion: workflowadapter.AuthoringContractSchemaVersion,
	}
	checkpoints := []workflowkit.StageCheckpoint{}
	usages := []workflowkit.StageUsage{}
	request := workflowkit.StageExecutionRequest{
		Execution: workflowkit.FrozenExecution{ID: "execution-1"},
		Claim:     workflowkit.JobClaim{Stage: &workflowkit.StageClaim{StageAttempt: workflowkit.AttemptIdentity{ID: "stage-attempt-1"}}},
		Stage:     stage, Inputs: []workflowkit.ArtifactBinding{binding, rootBinding},
		ReadInput: func(_ context.Context, requested workflowkit.ArtifactBinding) ([]byte, error) {
			switch requested {
			case binding:
				return append([]byte(nil), content...), nil
			case rootBinding:
				return append([]byte(nil), rootContent...), nil
			default:
				return nil, errors.New("unexpected frozen input")
			}
		},
		Checkpoint: func(_ context.Context, checkpoint workflowkit.StageCheckpoint) (workflowkit.CheckpointReceipt, error) {
			checkpoints = append(checkpoints, checkpoint.Clone())
			return workflowkit.CheckpointReceipt{CheckpointID: checkpoint.CheckpointID}, nil
		},
		Charge: func(_ context.Context, usage workflowkit.StageUsage) error {
			usages = append(usages, usage)
			return nil
		},
	}
	_ = now
	return request, &checkpoints, &usages
}

func standardAuthoringCodexTestRootContract() workflowadapter.AuthoringContract {
	contract, err := workflowadapter.NewAuthoringContract(
		workflowadapter.AuthoringContractTask{
			ID: "018f0a73-3b49-7000-8000-000000000001", Slug: "fixture-task", Title: "Fixture task",
			CodeLang: "rust", TaskType: "bugfix", Application: "service", Is0To1: false,
		},
		workflowadapter.AuthoringContractSource{
			RepositoryURL: "https://example.com/acme/fixture.git", CommitSHA: strings.Repeat("a", 40),
			SnapshotDigest: string(workflowkit.SHA256Fingerprint([]byte("frozen source snapshot"))), CheckoutRoot: "source",
		},
		"registry.example.com/team/runtime:1.2.3@sha256:"+strings.Repeat("a", 64),
		"Implement the requested fixture behavior.", string(workflowkit.SHA256Fingerprint([]byte("profile"))),
	)
	if err != nil {
		panic(err)
	}
	return contract
}

func standardAuthoringCodexTestDockerfileRequest(t *testing.T, stage workflowkit.StageDescriptor, now time.Time) (workflowkit.StageExecutionRequest, *[]workflowkit.StageCheckpoint, *[]workflowkit.StageUsage) {
	t.Helper()
	request, checkpoints, usages := standardAuthoringCodexTestRequest(stage, []byte("unused"), now)
	type frozenInput struct {
		name   string
		schema string
		bytes  []byte
	}
	inputs := []frozenInput{
		{name: workflowadapter.AuthoringContractArtifact, schema: workflowadapter.AuthoringContractSchemaVersion, bytes: standardAuthoringCodexTestRootContractBytes()},
		{name: "repo_prepared", schema: "harbor.artifact.v1", bytes: []byte("prepared source")},
		{name: "task_proposal", schema: "harbor.artifact.v1", bytes: []byte("approved task")},
	}
	contents := make(map[string][]byte, len(inputs))
	bindings := make([]workflowkit.ArtifactBinding, 0, len(inputs))
	for _, input := range inputs {
		contents[input.name] = append([]byte(nil), input.bytes...)
		bindings = append(bindings, workflowkit.ArtifactBinding{
			Name: input.name, ArtifactID: workflowkit.ArtifactID("input-" + input.name), ContentDigest: workflowkit.SHA256Fingerprint(input.bytes), SchemaVersion: input.schema,
		})
	}
	request.Inputs = bindings
	request.ReadInput = func(_ context.Context, requested workflowkit.ArtifactBinding) ([]byte, error) {
		for _, binding := range bindings {
			if requested == binding {
				return append([]byte(nil), contents[requested.Name]...), nil
			}
		}
		return nil, errors.New("unexpected frozen input")
	}
	return request, checkpoints, usages
}

func standardAuthoringCodexTestRootContractBytes() []byte {
	canonical, err := standardAuthoringCodexTestRootContract().CanonicalJSON()
	if err != nil {
		panic(err)
	}
	return canonical
}

func standardAuthoringCodexTestContextInput(name, artifactID, schemaVersion string, content []byte) standardAuthoringCodexInput {
	return standardAuthoringCodexInput{
		Name: name, ArtifactID: artifactID, SchemaVersion: schemaVersion,
		ContentDigest: workflowkit.SHA256Fingerprint(content), ContentBase64: base64.StdEncoding.EncodeToString(content),
	}
}

func standardAuthoringCodexTestCandidate(t *testing.T, verdict workflowkit.Verdict, contents ...[]byte) json.RawMessage {
	t.Helper()
	type candidatePart struct {
		ContentBase64 string `json:"content_base64"`
	}
	candidate := struct {
		Verdict   workflowkit.Verdict `json:"verdict"`
		Artifacts []candidatePart     `json:"artifacts"`
	}{Verdict: verdict, Artifacts: make([]candidatePart, 0, len(contents))}
	for _, content := range contents {
		candidate.Artifacts = append(candidate.Artifacts, candidatePart{ContentBase64: base64.StdEncoding.EncodeToString(content)})
	}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	return json.RawMessage(encoded)
}

type standardAuthoringCodexRuntimeStub struct {
	openRequests []agent.ConversationRequest
	conversation *standardAuthoringCodexConversationStub
	openErr      error
}

func (runtime *standardAuthoringCodexRuntimeStub) OpenConversation(_ context.Context, request agent.ConversationRequest) (agent.Conversation, error) {
	runtime.openRequests = append(runtime.openRequests, request)
	if runtime.openErr != nil {
		return nil, runtime.openErr
	}
	if runtime.conversation == nil {
		return nil, errors.New("missing test conversation")
	}
	runtime.conversation.dynamicTools = append([]agent.DynamicTool(nil), request.DynamicTools...)
	return runtime.conversation, nil
}

type standardAuthoringCodexConversationStub struct {
	requests []agent.TurnRequest
	results  []agent.TurnResult
	errors   []error
	// submissions are invoked in order during each Turn. This makes the test
	// runtime exercise the same conversation-private DynamicTool handler that
	// the App Server invokes in production.
	submissions         [][]json.RawMessage
	dynamicTools        []agent.DynamicTool
	submissionResponses []json.RawMessage
	submissionErrors    []error
	beforeSubmissions   func(int) error
	afterSubmissions    func(int) error
	closed              int
	closeErr            error
}

func (conversation *standardAuthoringCodexConversationStub) Turn(ctx context.Context, request agent.TurnRequest) (agent.TurnResult, error) {
	conversation.requests = append(conversation.requests, request)
	index := len(conversation.requests) - 1
	if index < len(conversation.errors) && conversation.errors[index] != nil {
		return agent.TurnResult{}, conversation.errors[index]
	}
	if conversation.beforeSubmissions != nil {
		if err := conversation.beforeSubmissions(index); err != nil {
			return agent.TurnResult{}, err
		}
	}
	if index < len(conversation.submissions) {
		tool, found := standardAuthoringCodexTestDynamicTool(conversation.dynamicTools, standardAuthoringCodexSubmitToolName)
		if !found || tool.Handler == nil {
			return agent.TurnResult{}, errors.New("missing test output submission tool")
		}
		for _, candidate := range conversation.submissions[index] {
			response, err := tool.Handler(ctx, append(json.RawMessage(nil), candidate...))
			conversation.submissionResponses = append(conversation.submissionResponses, append(json.RawMessage(nil), response...))
			conversation.submissionErrors = append(conversation.submissionErrors, err)
			if err != nil {
				return agent.TurnResult{}, err
			}
		}
	}
	if conversation.afterSubmissions != nil {
		if err := conversation.afterSubmissions(index); err != nil {
			return agent.TurnResult{}, err
		}
	}
	if index >= len(conversation.results) {
		return agent.TurnResult{}, errors.New("missing test result")
	}
	return conversation.results[index], nil
}

func standardAuthoringCodexTestDynamicTool(tools []agent.DynamicTool, name string) (agent.DynamicTool, bool) {
	for _, tool := range tools {
		if tool.Name == name {
			return tool, true
		}
	}
	return agent.DynamicTool{}, false
}

func (conversation *standardAuthoringCodexConversationStub) Close() error {
	conversation.closed++
	return conversation.closeErr
}

var _ agent.Runtime = (*standardAuthoringCodexRuntimeStub)(nil)
var _ agent.Conversation = (*standardAuthoringCodexConversationStub)(nil)

// standardAuthoringCodexAcceptanceBlockingConversation models the precise
// App Server failure mode: the host-owned dynamic tool has accepted immutable
// output, while the provider keeps its turn open until the host cancels it.
// Close blocks until Turn has returned, matching the production conversation
// contract and proving the executor waits for provider cleanup before
// materializing.
type standardAuthoringCodexAcceptanceBlockingRuntime struct {
	conversation *standardAuthoringCodexAcceptanceBlockingConversation
}

func (runtime *standardAuthoringCodexAcceptanceBlockingRuntime) OpenConversation(_ context.Context, request agent.ConversationRequest) (agent.Conversation, error) {
	if runtime == nil || runtime.conversation == nil {
		return nil, errors.New("missing acceptance-blocking test conversation")
	}
	runtime.conversation.dynamicTools = append([]agent.DynamicTool(nil), request.DynamicTools...)
	return runtime.conversation, nil
}

type standardAuthoringCodexAcceptanceBlockingConversation struct {
	dynamicTools       []agent.DynamicTool
	candidate          json.RawMessage
	afterSubmission    func()
	submissionResponse json.RawMessage
	submissionErr      error
	turnExited         chan struct{}
	closed             int
	closedAfterTurn    bool
}

func (conversation *standardAuthoringCodexAcceptanceBlockingConversation) Turn(ctx context.Context, _ agent.TurnRequest) (agent.TurnResult, error) {
	tool, found := standardAuthoringCodexTestDynamicTool(conversation.dynamicTools, standardAuthoringCodexSubmitToolName)
	if !found || tool.Handler == nil {
		return agent.TurnResult{}, errors.New("missing acceptance-blocking test output submission tool")
	}
	response, err := tool.Handler(ctx, append(json.RawMessage(nil), conversation.candidate...))
	conversation.submissionResponse = append(json.RawMessage(nil), response...)
	conversation.submissionErr = err
	if err != nil {
		close(conversation.turnExited)
		return agent.TurnResult{}, err
	}
	if conversation.afterSubmission != nil {
		conversation.afterSubmission()
	}
	<-ctx.Done()
	close(conversation.turnExited)
	return agent.TurnResult{}, ctx.Err()
}

func (conversation *standardAuthoringCodexAcceptanceBlockingConversation) Close() error {
	<-conversation.turnExited
	conversation.closedAfterTurn = true
	conversation.closed++
	return nil
}

var _ agent.Runtime = (*standardAuthoringCodexAcceptanceBlockingRuntime)(nil)
var _ agent.Conversation = (*standardAuthoringCodexAcceptanceBlockingConversation)(nil)
