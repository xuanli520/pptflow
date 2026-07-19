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
	if len(*checkpoints) != 4 {
		t.Fatalf("checkpoints = %+v, want ready/completed for two turns", *checkpoints)
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
	if len(*checkpoints) != 2*turns {
		t.Fatalf("checkpoints = %d, want %d", len(*checkpoints), 2*turns)
	}
	if standardAuthoringCodexTestUsageCount(*usages, "agent_turn") != turns || standardAuthoringCodexTestUsageCount(*usages, standardAuthoringCodexOutputSubmissionQuotaDimension) != 1 {
		t.Fatalf("usage records = %+v, want %d agent turns and one output submission", *usages, turns)
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
			wrongSubmissionQuota.Stage.QuotaClaims[index].Units = workflowadapter.StandardAuthoringOutputSubmissionClaimUnits - 1
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

func TestStandardAuthoringCodexAgentTurnExecutorUsesFrozenDockerfileEnvironmentPolicy(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	stage := standardAuthoringCodexTestDockerfileStage(1)
	policy := standardAuthoringCodexTestEnvironmentPolicy(t)
	wrongDockerfile := []byte("FROM registry.example.com/team/other:1.2.3@sha256:" + strings.Repeat("b", 64) + "\n")
	acceptedDockerfile := []byte("FROM " + policy.BaseImage + "\nRUN printf '%s\\n' ready\n")
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
	request, _, usages, policyBytes := standardAuthoringCodexTestDockerfileRequest(t, stage, &policy, now)
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
	if !strings.Contains(firstInput, `"name":"environment_policy"`) || !strings.Contains(firstInput, base64.StdEncoding.EncodeToString(policyBytes)) || !strings.Contains(firstInput, `"frozen_environment_policy"`) || !strings.Contains(firstInput, policy.BaseImage) {
		t.Fatalf("first request did not contain the frozen environment policy and its validated base image: %q", firstInput)
	}
	if len(runtime.conversation.submissionResponses) != 2 {
		t.Fatalf("submission responses = %+v", runtime.conversation.submissionResponses)
	}
	firstReceipt := standardAuthoringCodexTestSubmissionReceipt(t, runtime.conversation.submissionResponses[0])
	secondReceipt := standardAuthoringCodexTestSubmissionReceipt(t, runtime.conversation.submissionResponses[1])
	if firstReceipt.Accepted || len(firstReceipt.Errors) != 1 || firstReceipt.Errors[0] != "dockerfile_environment_policy_mismatch" || !secondReceipt.Accepted {
		t.Fatalf("Dockerfile submission receipts = first:%+v second:%+v", firstReceipt, secondReceipt)
	}
	if standardAuthoringCodexTestUsageCount(*usages, "agent_turn") != 1 || standardAuthoringCodexTestUsageCount(*usages, standardAuthoringCodexOutputSubmissionQuotaDimension) != 2 {
		t.Fatalf("usage records = %+v, want one turn and two submissions", *usages)
	}
}

func TestStandardAuthoringCodexAgentTurnExecutorRetriesWrappedTaskTOMLWithinTurn(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	stage := standardAuthoringCodexTestArtifactStage(1, workflowkit.StageKey(workflowadapter.TaskTOMLGen), "task_toml")
	wrapped := []byte(`{"format":"harbor.artifact.v1","version":"1","metadata":{"task_type":"feature","application":"backend"},"task_toml":"[metadata]\ntask_type = \"feature\"\napplication = \"backend\"\n"}`)
	raw := []byte("[metadata]\ntask_type = \"feature\"\napplication = \"backend\"\n")
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

func TestStandardAuthoringCodexAgentTurnExecutorRejectsMissingDockerfileEnvironmentPolicyBeforeOpeningConversation(t *testing.T) {
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
	request, _, usages, _ := standardAuthoringCodexTestDockerfileRequest(t, stage, nil, now)
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
		t.Fatalf("missing policy result = %+v", result)
	}
	if len(runtime.openRequests) != 0 || len(*usages) != 0 {
		t.Fatalf("missing policy opened runtime or charged usage: opens=%d usages=%+v", len(runtime.openRequests), *usages)
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
		CLIVersionOutput: "codex-cli 0.133.0", SandboxMode: CodexAppServerSandboxModeReadOnly,
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
		{Name: workflowadapter.StandardAuthoringEnvironmentPolicyArtifact, SchemaVersion: workflowadapter.StandardAuthoringEnvironmentPolicySchemaVersion, Required: true},
	}
	return stage
}

func standardAuthoringCodexTestArtifactStage(turns int, stageKey workflowkit.StageKey, outputName string) workflowkit.StageDescriptor {
	stage := standardAuthoringCodexTestStage(turns)
	stage.Key = stageKey
	stage.Plugin = workflowkit.PluginBinding{ID: "harborfactory." + string(stageKey), Version: "1"}
	stage.Outputs = []workflowkit.ArtifactSpec{{Name: outputName, SchemaVersion: "harbor.artifact.v1", Required: true}}
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

func standardAuthoringCodexTestRequest(stage workflowkit.StageDescriptor, content []byte, now time.Time) (workflowkit.StageExecutionRequest, *[]workflowkit.StageCheckpoint, *[]workflowkit.StageUsage) {
	binding := workflowkit.ArtifactBinding{Name: "repo_prepared", ArtifactID: "input-1", ContentDigest: workflowkit.SHA256Fingerprint(content), SchemaVersion: "harbor.artifact.v1"}
	checkpoints := []workflowkit.StageCheckpoint{}
	usages := []workflowkit.StageUsage{}
	request := workflowkit.StageExecutionRequest{
		Execution: workflowkit.FrozenExecution{ID: "execution-1"},
		Claim:     workflowkit.JobClaim{Stage: &workflowkit.StageClaim{StageAttempt: workflowkit.AttemptIdentity{ID: "stage-attempt-1"}}},
		Stage:     stage, Inputs: []workflowkit.ArtifactBinding{binding},
		ReadInput: func(_ context.Context, requested workflowkit.ArtifactBinding) ([]byte, error) {
			if requested != binding {
				return nil, errors.New("unexpected frozen input")
			}
			return append([]byte(nil), content...), nil
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

func standardAuthoringCodexTestDockerfileRequest(t *testing.T, stage workflowkit.StageDescriptor, policy *workflowadapter.StandardAuthoringEnvironmentPolicy, now time.Time) (workflowkit.StageExecutionRequest, *[]workflowkit.StageCheckpoint, *[]workflowkit.StageUsage, []byte) {
	t.Helper()
	request, checkpoints, usages := standardAuthoringCodexTestRequest(stage, []byte("unused"), now)
	type frozenInput struct {
		name   string
		schema string
		bytes  []byte
	}
	inputs := []frozenInput{
		{name: "repo_prepared", schema: "harbor.artifact.v1", bytes: []byte("prepared source")},
		{name: "task_proposal", schema: "harbor.artifact.v1", bytes: []byte("approved task")},
	}
	var policyBytes []byte
	if policy != nil {
		var err error
		policyBytes, err = policy.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, frozenInput{
			name: workflowadapter.StandardAuthoringEnvironmentPolicyArtifact, schema: workflowadapter.StandardAuthoringEnvironmentPolicySchemaVersion, bytes: policyBytes,
		})
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
	return request, checkpoints, usages, append([]byte(nil), policyBytes...)
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
