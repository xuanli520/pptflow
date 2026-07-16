package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSessionServesPrivateDynamicToolCallsAndForwardsOutputSchema(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake app-server program uses a POSIX shell")
	}
	tempDir := t.TempDir()
	requestLog := filepath.Join(tempDir, "requests.jsonl")
	commandPath := filepath.Join(tempDir, "fake-codex")
	script := `#!/bin/sh
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$FAKE_CODEX_REQUEST_LOG"
  id=$(printf '%s\n' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    *'"method":"thread/start"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"thread-1"}}}\n' "$id"
      ;;
    *'"method":"turn/start"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn-1"}}}\n' "$id"
      printf '{"jsonrpc":"2.0","id":91,"method":"item/tool/call","params":{"threadId":"thread-1","turnId":"turn-1","callId":"call-1","namespace":null,"tool":"submit_output","arguments":{"candidate":"candidate-value"}}}\n'
      ;;
    *'"id":91'*)
      printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-1","type":"agentMessage","text":"accepted"}}}\n'
      printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","items":[],"status":"completed"}}}\n'
      ;;
  esac
done
`
	if err := os.WriteFile(commandPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	arguments := make(chan json.RawMessage, 1)
	session := New(nil)
	if err := session.Start(context.Background(), Request{
		ProjectPath:       tempDir,
		LogPath:           filepath.Join(tempDir, "codex.log"),
		Env:               append(os.Environ(), "FAKE_CODEX_REQUEST_LOG="+requestLog),
		CommandPath:       commandPath,
		CapabilitySummary: "fake",
		HasAppServer:      true,
		SandboxMode:       "read-only",
		SandboxPolicy:     "readOnly",
		MaxOutputBytes:    1024,
		DynamicTools: []DynamicTool{{
			Name:        "submit_output",
			Description: "Submit one verified JSON value.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"candidate":{"type":"string"}},"required":["candidate"]}`),
			Handler: func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
				arguments <- append(json.RawMessage(nil), input...)
				return json.RawMessage(`{"accepted":true}`), nil
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	result, err := session.Turn(context.Background(), TurnRequest{
		Prompt:       "submit the candidate",
		Timeout:      time.Second,
		OutputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"accepted":{"type":"boolean"}},"required":["accepted"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Result.Stdout != "accepted" {
		t.Fatalf("turn output = %q", result.Result.Stdout)
	}
	select {
	case got := <-arguments:
		if string(got) != `{"candidate":"candidate-value"}` {
			t.Fatalf("tool arguments = %s", got)
		}
	default:
		t.Fatal("dynamic tool handler was not called")
	}

	requests := readDynamicToolTestRequests(t, requestLog)
	threadStart := requestWithMethod(t, requests, "thread/start")
	var threadParams struct {
		DynamicTools []struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"dynamicTools"`
	}
	if err := json.Unmarshal(threadStart.Params, &threadParams); err != nil {
		t.Fatal(err)
	}
	if len(threadParams.DynamicTools) != 1 || threadParams.DynamicTools[0].Type != "function" || threadParams.DynamicTools[0].Name != "submit_output" || !json.Valid(threadParams.DynamicTools[0].InputSchema) {
		t.Fatalf("thread dynamic tools = %+v", threadParams.DynamicTools)
	}
	turnStart := requestWithMethod(t, requests, "turn/start")
	var turnParams struct {
		OutputSchema json.RawMessage `json:"outputSchema"`
	}
	if err := json.Unmarshal(turnStart.Params, &turnParams); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(turnParams.OutputSchema) || !strings.Contains(string(turnParams.OutputSchema), `"accepted"`) {
		t.Fatalf("turn output schema = %s", turnParams.OutputSchema)
	}
	toolResponse := responseWithID(t, requests, 91)
	var toolResult dynamicToolCallResponse
	if err := json.Unmarshal(toolResponse.Result, &toolResult); err != nil {
		t.Fatal(err)
	}
	if !toolResult.Success || len(toolResult.ContentItems) != 1 || toolResult.ContentItems[0].Type != "inputText" || toolResult.ContentItems[0].Text != `{"accepted":true}` {
		t.Fatalf("tool response = %+v", toolResult)
	}
	logBytes, err := os.ReadFile(filepath.Join(tempDir, "codex.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logBytes), "candidate-value") || strings.Contains(string(logBytes), `{"accepted":true}`) {
		t.Fatalf("dynamic tool payload leaked to app-server log: %s", logBytes)
	}
}

func TestSessionRejectsToolCallBeforeTurnStartAcknowledgement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake app-server program uses a POSIX shell")
	}
	tempDir := t.TempDir()
	requestLog := filepath.Join(tempDir, "requests.jsonl")
	commandPath := filepath.Join(tempDir, "fake-codex")
	script := `#!/bin/sh
turn_start_id=
early_pending=0
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$FAKE_CODEX_REQUEST_LOG"
  id=$(printf '%s\n' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    *'"method":"thread/start"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"thread-1"}}}\n' "$id"
      ;;
    *'"method":"turn/start"'*)
      turn_start_id="$id"
      early_pending=1
      # This forged call uses the eventual turn ID before the client has
      # received the server acknowledgement. It must not reach the handler.
      printf '{"jsonrpc":"2.0","id":91,"method":"item/tool/call","params":{"threadId":"thread-1","turnId":"turn-1","callId":"early","namespace":null,"tool":"submit_output","arguments":{"candidate":"forged"}}}\n'
      ;;
    *'"id":91'*)
      if [ "$early_pending" = 1 ]; then
        early_pending=0
        printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn-1"}}}\n' "$turn_start_id"
        # This legal call follows the acknowledgement immediately, exercising
        # the stdout-reader ordering guarantee in recordTurnStartResponse.
        printf '{"jsonrpc":"2.0","id":92,"method":"item/tool/call","params":{"threadId":"thread-1","turnId":"turn-1","callId":"legal","namespace":null,"tool":"submit_output","arguments":{"candidate":"accepted"}}}\n'
      fi
      ;;
    *'"id":92'*)
      printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-1","type":"agentMessage","text":"done"}}}\n'
      printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","items":[],"status":"completed"}}}\n'
      ;;
  esac
done
`
	if err := os.WriteFile(commandPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	calls := make(chan json.RawMessage, 2)
	session := New(nil)
	if err := session.Start(context.Background(), Request{
		ProjectPath:       tempDir,
		LogPath:           filepath.Join(tempDir, "codex.log"),
		Env:               append(os.Environ(), "FAKE_CODEX_REQUEST_LOG="+requestLog),
		CommandPath:       commandPath,
		CapabilitySummary: "fake",
		HasAppServer:      true,
		SandboxMode:       "read-only",
		SandboxPolicy:     "readOnly",
		DynamicTools: []DynamicTool{{
			Name:        "submit_output",
			Description: "Submit one verified JSON value.",
			InputSchema: json.RawMessage(`{"type":"object"}`),
			Handler: func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
				calls <- append(json.RawMessage(nil), input...)
				return json.RawMessage(`{"accepted":true}`), nil
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	result, err := session.Turn(context.Background(), TurnRequest{Prompt: "submit", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if result.Result.Stdout != "done" {
		t.Fatalf("turn output = %q", result.Result.Stdout)
	}
	select {
	case call := <-calls:
		if string(call) != `{"candidate":"accepted"}` {
			t.Fatalf("handler arguments = %s, want only acknowledged call", call)
		}
	default:
		t.Fatal("acknowledged tool call did not reach handler")
	}
	select {
	case unexpected := <-calls:
		t.Fatalf("forged tool call reached handler: %s", unexpected)
	default:
	}

	requests := readDynamicToolTestRequests(t, requestLog)
	early := responseWithID(t, requests, 91)
	var earlyResult dynamicToolCallResponse
	if err := json.Unmarshal(early.Result, &earlyResult); err != nil {
		t.Fatal(err)
	}
	if earlyResult.Success || len(earlyResult.ContentItems) != 1 {
		t.Fatalf("early tool response = %+v", earlyResult)
	}
	var earlyFailure struct {
		Error  string `json:"error"`
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal([]byte(earlyResult.ContentItems[0].Text), &earlyFailure); err != nil {
		t.Fatal(err)
	}
	if earlyFailure.Error != "tool_unavailable" || earlyFailure.Digest != dynamicToolPayloadDigest([]byte(`{"candidate":"forged"}`)) {
		t.Fatalf("early failure = %+v", earlyFailure)
	}
	legal := responseWithID(t, requests, 92)
	var legalResult dynamicToolCallResponse
	if err := json.Unmarshal(legal.Result, &legalResult); err != nil {
		t.Fatal(err)
	}
	if !legalResult.Success || len(legalResult.ContentItems) != 1 || legalResult.ContentItems[0].Text != `{"accepted":true}` {
		t.Fatalf("acknowledged tool response = %+v", legalResult)
	}
}

func TestDynamicToolCallReturnsStableFailureWithoutLeakingHandlerError(t *testing.T) {
	ctx := context.Background()
	session := &appServerSession{
		req:         Request{MaxOutputBytes: 1024},
		threadID:    "thread-1",
		turnID:      "turn-1",
		turnDone:    make(chan struct{}),
		turnContext: ctx,
		dynamicTools: map[string]DynamicTool{
			"submit_output": {
				Name:        "submit_output",
				Description: "Submit one value.",
				InputSchema: json.RawMessage(`{"type":"object"}`),
				Handler: func(context.Context, json.RawMessage) (json.RawMessage, error) {
					return nil, errors.New("provider detail must not be exposed")
				},
			},
		},
	}
	response := session.dynamicToolCallResponse(json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","callId":"call-1","namespace":null,"tool":"submit_output","arguments":{}}`))
	if response.Success || len(response.ContentItems) != 1 {
		t.Fatalf("failed response = %+v", response)
	}
	var failure struct {
		Error  string `json:"error"`
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal([]byte(response.ContentItems[0].Text), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Error != "tool_failed" || failure.Digest != dynamicToolPayloadDigest([]byte(`{}`)) {
		t.Fatalf("failed response = %+v", failure)
	}
	if strings.Contains(response.ContentItems[0].Text, "provider detail") {
		t.Fatalf("handler detail leaked in response: %+v", response)
	}
}

func TestDynamicToolTransportDefersSmallerStageByteLimitToHandler(t *testing.T) {
	called := false
	inputBytes := 0
	session := &appServerSession{
		req:         Request{MaxOutputBytes: 8},
		threadID:    "thread-1",
		turnID:      "turn-1",
		turnDone:    make(chan struct{}),
		turnContext: context.Background(),
		dynamicTools: map[string]DynamicTool{
			"submit_output": {
				Name:        "submit_output",
				Description: "Submit one value.",
				InputSchema: json.RawMessage(`{"type":"object"}`),
				Handler: func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
					called = true
					inputBytes = len(input)
					return json.RawMessage(`{"accepted":false,"errors":["byte_limit_exceeded"],"remaining":2,"digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}`), nil
				},
			},
		},
	}
	response := session.dynamicToolCallResponse(json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","callId":"call-1","namespace":null,"tool":"submit_output","arguments":{"candidate":"larger-than-eight"}}`))
	if !called {
		t.Fatal("transport rejected an over-stage-limit candidate before its handler")
	}
	if inputBytes <= 8 {
		t.Fatalf("test candidate did not exceed the stage byte limit: %d", inputBytes)
	}
	if !response.Success || len(response.ContentItems) != 1 || !strings.Contains(response.ContentItems[0].Text, "byte_limit_exceeded") {
		t.Fatalf("byte-limit handler receipt = %+v", response)
	}
}

func TestDynamicToolTransportRejectsOversizedArgumentsWithDigestOnly(t *testing.T) {
	const secret = "oversized-candidate-must-not-escape"
	arguments := json.RawMessage(`{"candidate":"` + strings.Repeat("x", defaultDynamicToolJSONBytes) + secret + `"}`)
	called := false
	session := &appServerSession{
		req:         Request{MaxOutputBytes: 8},
		threadID:    "thread-1",
		turnID:      "turn-1",
		turnDone:    make(chan struct{}),
		turnContext: context.Background(),
		dynamicTools: map[string]DynamicTool{
			"submit_output": {
				Name:        "submit_output",
				Description: "Submit one value.",
				InputSchema: json.RawMessage(`{"type":"object"}`),
				Handler: func(context.Context, json.RawMessage) (json.RawMessage, error) {
					called = true
					return json.RawMessage(`{}`), nil
				},
			},
		},
	}
	params, err := json.Marshal(dynamicToolCallParams{
		ThreadID: "thread-1", TurnID: "turn-1", CallID: "call-1", Tool: "submit_output", Arguments: arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := session.dynamicToolCallResponse(params)
	if response.Success || called || len(response.ContentItems) != 1 {
		t.Fatalf("oversized tool response = %+v called=%v", response, called)
	}
	var failure struct {
		Error  string `json:"error"`
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal([]byte(response.ContentItems[0].Text), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Error != "tool_arguments_too_large" || failure.Digest != dynamicToolPayloadDigest(arguments) {
		t.Fatalf("oversized failure = %+v", failure)
	}
	if strings.Contains(response.ContentItems[0].Text, secret) {
		t.Fatalf("oversized failure leaked candidate content: %s", response.ContentItems[0].Text)
	}
}

func TestDynamicToolSchemasRejectNonSchemaJSONValues(t *testing.T) {
	if _, err := normalizeDynamicTools([]DynamicTool{{
		Name:        "submit_output",
		Description: "Submit one value.",
		InputSchema: json.RawMessage(`"not-a-schema"`),
		Handler: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	}}); err == nil {
		t.Fatal("non-schema dynamic tool input was accepted")
	}
	if _, err := normalizeJSONSchema(json.RawMessage(`["not-a-schema"]`)); err == nil {
		t.Fatal("non-schema output value was accepted")
	}
}

type dynamicToolTestMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
}

func readDynamicToolTestRequests(t *testing.T, path string) []dynamicToolTestMessage {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	messages := make([]dynamicToolTestMessage, 0, len(lines))
	for _, line := range lines {
		var message dynamicToolTestMessage
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Fatalf("decode request %q: %v", line, err)
		}
		messages = append(messages, message)
	}
	return messages
}

func requestWithMethod(t *testing.T, messages []dynamicToolTestMessage, method string) dynamicToolTestMessage {
	t.Helper()
	for _, message := range messages {
		if message.Method == method {
			return message
		}
	}
	t.Fatalf("request method %q was not recorded", method)
	return dynamicToolTestMessage{}
}

func responseWithID(t *testing.T, messages []dynamicToolTestMessage, id int) dynamicToolTestMessage {
	t.Helper()
	for _, message := range messages {
		var actual int
		if json.Unmarshal(message.ID, &actual) == nil && actual == id && len(message.Result) != 0 {
			return message
		}
	}
	t.Fatalf("response id %d was not recorded", id)
	return dynamicToolTestMessage{}
}
