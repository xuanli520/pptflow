package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type scriptedRuntime struct {
	openRequest  ConversationRequest
	conversation *scriptedConversation
	openErr      error
}

func (runtime *scriptedRuntime) OpenConversation(_ context.Context, request ConversationRequest) (Conversation, error) {
	runtime.openRequest = request
	if runtime.openErr != nil {
		return nil, runtime.openErr
	}
	return runtime.conversation, nil
}

type scriptedConversation struct {
	request  TurnRequest
	result   TurnResult
	turnErr  error
	closeErr error
	turns    int
	closes   int
}

func (conversation *scriptedConversation) Turn(_ context.Context, request TurnRequest) (TurnResult, error) {
	conversation.request = request
	conversation.turns++
	return conversation.result, conversation.turnErr
}

func (conversation *scriptedConversation) Close() error {
	conversation.closes++
	return conversation.closeErr
}

func TestRunTurnUsesConversationLifecycleAndCopiesOpenSettings(t *testing.T) {
	conversation := &scriptedConversation{result: TurnResult{Text: "ok", Model: "test-model"}}
	runtime := &scriptedRuntime{conversation: conversation}
	request := TurnRequest{
		ProjectPath:       "/tmp/project",
		Prompt:            "inspect",
		Input:             []InputPart{{Type: "text", Text: "context"}},
		Model:             "test-model",
		ReasoningEffort:   "high",
		SandboxMode:       "read-only",
		SandboxPolicy:     "readOnly",
		NetworkAccess:     true,
		WorkspaceRoots:    []string{"/tmp/project", "/tmp/other"},
		TimeoutSeconds:    42,
		MaxOutputBytes:    1024,
		CapabilitySummary: "test capability",
		LogPath:           "/tmp/codex.log",
	}

	result, err := RunTurn(context.Background(), runtime, request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, conversation.result) || conversation.turns != 1 || conversation.closes != 1 {
		t.Fatalf("run result/lifecycle = result=%+v turns=%d closes=%d", result, conversation.turns, conversation.closes)
	}
	if runtime.openRequest.ProjectPath != request.ProjectPath || runtime.openRequest.Model != request.Model || runtime.openRequest.ReasoningEffort != request.ReasoningEffort || runtime.openRequest.SandboxMode != request.SandboxMode || runtime.openRequest.SandboxPolicy != request.SandboxPolicy || runtime.openRequest.NetworkAccess != request.NetworkAccess || runtime.openRequest.TimeoutSeconds != request.TimeoutSeconds || runtime.openRequest.MaxOutputBytes != request.MaxOutputBytes || runtime.openRequest.CapabilitySummary != request.CapabilitySummary || runtime.openRequest.LogPath != request.LogPath {
		t.Fatalf("open request lost immutable settings: %+v", runtime.openRequest)
	}
	if len(runtime.openRequest.WorkspaceRoots) != len(request.WorkspaceRoots) || runtime.openRequest.WorkspaceRoots[0] != request.WorkspaceRoots[0] || runtime.openRequest.WorkspaceRoots[1] != request.WorkspaceRoots[1] {
		t.Fatalf("open request workspace roots = %#v, want %#v", runtime.openRequest.WorkspaceRoots, request.WorkspaceRoots)
	}
	runtime.openRequest.WorkspaceRoots[0] = "/tmp/mutated"
	if request.WorkspaceRoots[0] != "/tmp/project" {
		t.Fatalf("open request aliases caller workspace roots: %#v", request.WorkspaceRoots)
	}
	if !reflect.DeepEqual(conversation.request, request) {
		t.Fatalf("turn request changed: got=%+v want=%+v", conversation.request, request)
	}
}

func TestRunTurnClosesAfterTurnFailureAndPreservesPrimaryError(t *testing.T) {
	turnErr := errors.New("turn failed")
	closeErr := errors.New("close failed")
	conversation := &scriptedConversation{turnErr: turnErr, closeErr: closeErr}
	_, err := RunTurn(context.Background(), &scriptedRuntime{conversation: conversation}, TurnRequest{Prompt: "inspect"})
	if !errors.Is(err, turnErr) {
		t.Fatalf("error = %v, want turn error", err)
	}
	if conversation.turns != 1 || conversation.closes != 1 {
		t.Fatalf("lifecycle = turns=%d closes=%d, want one each", conversation.turns, conversation.closes)
	}
}

func TestRunTurnReturnsCloseFailureAfterSuccessfulTurn(t *testing.T) {
	closeErr := errors.New("close failed")
	conversation := &scriptedConversation{result: TurnResult{Text: "ok"}, closeErr: closeErr}
	_, err := RunTurn(context.Background(), &scriptedRuntime{conversation: conversation}, TurnRequest{Prompt: "inspect"})
	if !errors.Is(err, closeErr) {
		t.Fatalf("error = %v, want close error", err)
	}
	if conversation.turns != 1 || conversation.closes != 1 {
		t.Fatalf("lifecycle = turns=%d closes=%d, want one each", conversation.turns, conversation.closes)
	}
}

func TestRunTurnDoesNotTurnOrCloseWhenOpenFails(t *testing.T) {
	openErr := errors.New("open failed")
	conversation := &scriptedConversation{}
	_, err := RunTurn(context.Background(), &scriptedRuntime{conversation: conversation, openErr: openErr}, TurnRequest{Prompt: "inspect"})
	if !errors.Is(err, openErr) {
		t.Fatalf("error = %v, want open error", err)
	}
	if conversation.turns != 0 || conversation.closes != 0 {
		t.Fatalf("lifecycle = turns=%d closes=%d, want zero each", conversation.turns, conversation.closes)
	}
}
