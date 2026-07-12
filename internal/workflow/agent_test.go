package workflow

import (
	"context"
	"testing"
)

type testAgentRuntime struct {
	request      AgentConversationRequest
	conversation *testAgentConversation
}

func (r *testAgentRuntime) OpenConversation(_ context.Context, request AgentConversationRequest) (AgentConversation, error) {
	r.request = request
	r.conversation = &testAgentConversation{}
	return r.conversation, nil
}

type testAgentConversation struct {
	request AgentTurnRequest
	closed  bool
}

func (c *testAgentConversation) Turn(_ context.Context, request AgentTurnRequest) (AgentTurnResult, error) {
	c.request = request
	return AgentTurnResult{Text: "ok", Model: request.Model}, nil
}

func (c *testAgentConversation) Close() error {
	c.closed = true
	return nil
}

func TestRunAgentTurnUsesConversationLifecycle(t *testing.T) {
	runtime := &testAgentRuntime{}
	request := AgentTurnRequest{
		ProjectPath:     "/tmp/project",
		Prompt:          "inspect",
		Model:           "test-model",
		SandboxMode:     "read-only",
		WorkspaceRoots:  []string{"/tmp/project"},
		TimeoutSeconds:  42,
		MaxOutputBytes:  1024,
		LogPath:         "/tmp/codex.log",
		ReasoningEffort: "high",
	}
	result, err := RunAgentTurn(context.Background(), runtime, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "ok" || runtime.conversation == nil || !runtime.conversation.closed {
		t.Fatalf("one-shot helper did not complete conversation lifecycle: result=%+v conversation=%+v", result, runtime.conversation)
	}
	if runtime.request.ProjectPath != request.ProjectPath || runtime.request.Model != request.Model || runtime.request.TimeoutSeconds != request.TimeoutSeconds {
		t.Fatalf("conversation request lost immutable settings: %+v", runtime.request)
	}
	if runtime.conversation.request.Prompt != request.Prompt {
		t.Fatalf("turn request lost prompt: %+v", runtime.conversation.request)
	}
}
