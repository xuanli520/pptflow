package stageprovider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	finalOutput := standardAuthoringCodexTestOutput(t, stage, workflowkit.VerdictPass, []byte("sealed analysis"))
	runtime := &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{
		results: []agent.TurnResult{
			{Model: CodexAppServerProductionModelID, Text: `{"progress":"internal only"}`},
			{Model: CodexAppServerProductionModelID, Text: finalOutput},
		},
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
	if open.Model != CodexAppServerProductionModelID || open.ReasoningEffort != string(CodexAppServerProductionReasoningEffort) || open.NetworkAccess || open.SandboxMode != CodexAppServerSandboxModeWorkspaceWrite || open.SandboxPolicy != CodexAppServerSandboxPolicyWorkspaceWrite || open.LogPath != os.DevNull {
		t.Fatalf("controlled conversation request = %+v", open)
	}
	if len(runtime.conversation.requests) != 2 || runtime.conversation.requests[0].Model != CodexAppServerProductionModelID || runtime.conversation.requests[0].ReasoningEffort != string(CodexAppServerProductionReasoningEffort) || runtime.conversation.requests[1].ReasoningEffort != string(CodexAppServerProductionReasoningEffort) || len(runtime.conversation.requests[0].Input) != 1 || len(runtime.conversation.requests[1].Input) != 0 {
		t.Fatalf("turn requests = %+v", runtime.conversation.requests)
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
	if len(*usages) != 2 {
		t.Fatalf("usage records = %+v, want two consumed agent turns", *usages)
	}
	for _, usage := range *usages {
		if usage.Dimension != "agent_turn" || usage.Units != 1 || !strings.HasPrefix(usage.OperationKey, "standard-authoring-codex-usage:sha256:") {
			t.Fatalf("usage = %+v, want a secret-free frozen agent-turn charge", usage)
		}
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
			name: "malformed final output",
			runtime: &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{
				results: []agent.TurnResult{{Model: CodexAppServerProductionModelID, Text: `{"format":"` + StandardAuthoringCodexTurnOutputFormat + `","secret":"` + secret + `"}`}},
			}},
			wantCode: standardAuthoringCodexFailureOutput,
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
		Model: CodexAppServerProductionModelID, Text: standardAuthoringCodexTestOutput(t, stage, workflowkit.VerdictPass, []byte("analysis")),
	}}}}
	executor, err := NewStandardAuthoringCodexAgentTurnExecutor(StandardAuthoringCodexAgentTurnExecutorConfig{
		InvocationFactory: standardAuthoringCodexTestInvocationFactory(standardAuthoringCodexTestInvocation(t)), WorkspaceRoot: root,
		WorkspaceMode: StandardAuthoringCodexWorkspaceRunScoped, RuntimeFactory: standardAuthoringCodexTestRuntimeFactory(runtime),
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
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	result, err = executor.ExecuteAgentTurn(context.Background(), invocation, standardAuthoringCodexTestPayload(1))
	if err != nil || result.Outcome.Status != workflowkit.StatusCompleted || len(runtime.openRequests) != 1 {
		t.Fatalf("prepared RunScoped workspace result=%+v err=%v opens=%d", result, err, len(runtime.openRequests))
	}
	if runtime.openRequests[0].ProjectPath != workspace || len(runtime.openRequests[0].WorkspaceRoots) != 1 || runtime.openRequests[0].WorkspaceRoots[0] != workspace {
		t.Fatalf("RunScoped Codex workspace = %+v, want %q", runtime.openRequests[0], workspace)
	}
}

func TestParseStandardAuthoringCodexTurnOutputRejectsNoncanonicalOrDuplicateDocuments(t *testing.T) {
	t.Parallel()
	stage := standardAuthoringCodexTestStage(1)
	duplicate := `{"format":"` + StandardAuthoringCodexTurnOutputFormat + `","format":"` + StandardAuthoringCodexTurnOutputFormat + `","version":"1","verdict":"pass","artifacts":[]}`
	if _, err := parseStandardAuthoringCodexTurnOutput([]byte(duplicate), stage, 1); err == nil {
		t.Fatal("duplicate-key output was accepted")
	}
	canonical := standardAuthoringCodexTestOutput(t, stage, workflowkit.VerdictPass, []byte("analysis"))
	if _, err := parseStandardAuthoringCodexTurnOutput([]byte(canonical+"\n"), stage, 1); err == nil {
		t.Fatal("noncanonical output whitespace was accepted")
	}
}

func standardAuthoringCodexTestExecutor(t *testing.T, runtime agent.Runtime, now time.Time, turns int) (*StandardAuthoringCodexAgentTurnExecutor, StandardAuthoringCodexTurnProgram) {
	t.Helper()
	program, err := NewStandardAuthoringCodexTurnProgram("standard-authoring.repo-analysis", "1", standardAuthoringCodexTestPrompts(turns), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewStandardAuthoringCodexAgentTurnExecutor(StandardAuthoringCodexAgentTurnExecutorConfig{
		InvocationFactory: standardAuthoringCodexTestInvocationFactory(standardAuthoringCodexTestInvocation(t)), WorkspaceRoot: t.TempDir(),
		RuntimeFactory: standardAuthoringCodexTestRuntimeFactory(runtime),
		ProgramByStage: map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{"repo_analyze": program},
		Now:            func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return executor, program
}

func standardAuthoringCodexTestPrompts(turns int) []string {
	prompts := make([]string, 0, turns)
	for index := 1; index <= turns; index++ {
		prompts = append(prompts, "perform frozen authoring turn "+string(rune('0'+index)))
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
		CLIVersionOutput: "codex-cli 0.133.0", SandboxMode: CodexAppServerSandboxModeWorkspaceWrite,
		SandboxPolicy: CodexAppServerSandboxPolicyWorkspaceWrite, NetworkAccess: false,
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

func standardAuthoringCodexTestStage(turns int) workflowkit.StageDescriptor {
	return workflowkit.StageDescriptor{
		Key: "repo_analyze", Outputs: []workflowkit.ArtifactSpec{{Name: "repo_analysis", SchemaVersion: "harbor.artifact.v1", Required: true}},
		Budget:      workflowkit.ExecutionBudget{MaxTurns: turns, TurnTimeout: 5 * time.Second},
		QuotaClaims: []workflowkit.QuotaClaim{{Dimension: "agent_turn", Units: int64(turns), ReclaimPolicy: workflowkit.ReclaimUnused}},
		Verdicts:    workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass, workflowkit.VerdictNeedsRepair}},
	}
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

func standardAuthoringCodexTestOutput(t *testing.T, stage workflowkit.StageDescriptor, verdict workflowkit.Verdict, content []byte) string {
	t.Helper()
	output := standardAuthoringCodexTurnOutput{
		Format: StandardAuthoringCodexTurnOutputFormat, Version: StandardAuthoringCodexTurnOutputVersion, Verdict: verdict,
		Artifacts: []standardAuthoringCodexTurnOutputArtifact{{
			Name: stage.Outputs[0].Name, SchemaVersion: stage.Outputs[0].SchemaVersion, ContentBase64: base64.StdEncoding.EncodeToString(content),
		}},
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
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
	return runtime.conversation, nil
}

type standardAuthoringCodexConversationStub struct {
	requests []agent.TurnRequest
	results  []agent.TurnResult
	errors   []error
	closed   int
	closeErr error
}

func (conversation *standardAuthoringCodexConversationStub) Turn(_ context.Context, request agent.TurnRequest) (agent.TurnResult, error) {
	conversation.requests = append(conversation.requests, request)
	index := len(conversation.requests) - 1
	if index < len(conversation.errors) && conversation.errors[index] != nil {
		return agent.TurnResult{}, conversation.errors[index]
	}
	if index >= len(conversation.results) {
		return agent.TurnResult{}, errors.New("missing test result")
	}
	return conversation.results[index], nil
}

func (conversation *standardAuthoringCodexConversationStub) Close() error {
	conversation.closed++
	return conversation.closeErr
}

var _ agent.Runtime = (*standardAuthoringCodexRuntimeStub)(nil)
var _ agent.Conversation = (*standardAuthoringCodexConversationStub)(nil)
