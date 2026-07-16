package appserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const defaultDynamicToolJSONBytes = 3 << 20

type dynamicToolCallParams struct {
	ThreadID  string          `json:"threadId"`
	TurnID    string          `json:"turnId"`
	CallID    string          `json:"callId"`
	Namespace *string         `json:"namespace"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

type dynamicToolCallResponse struct {
	ContentItems []dynamicToolOutputItem `json:"contentItems"`
	Success      bool                    `json:"success"`
}

type dynamicToolOutputItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func normalizeDynamicTools(tools []DynamicTool) (map[string]DynamicTool, error) {
	result := make(map[string]DynamicTool, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" || strings.IndexFunc(name, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
			return nil, fmt.Errorf("codex app-server dynamic tool name is invalid")
		}
		if strings.TrimSpace(tool.Description) == "" {
			return nil, fmt.Errorf("codex app-server dynamic tool %q description is required", name)
		}
		if tool.Handler == nil {
			return nil, fmt.Errorf("codex app-server dynamic tool %q handler is required", name)
		}
		schema, err := normalizeJSONSchema(tool.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("codex app-server dynamic tool %q input schema: %w", name, err)
		}
		if _, duplicate := result[name]; duplicate {
			return nil, fmt.Errorf("codex app-server dynamic tool %q is duplicated", name)
		}
		result[name] = DynamicTool{
			Name:        name,
			Description: strings.TrimSpace(tool.Description),
			InputSchema: schema,
			Handler:     tool.Handler,
		}
	}
	return result, nil
}

func normalizedDynamicToolSlice(tools []DynamicTool, normalized map[string]DynamicTool) []DynamicTool {
	if len(tools) == 0 {
		return nil
	}
	result := make([]DynamicTool, 0, len(tools))
	for _, tool := range tools {
		if normalizedTool, found := normalized[strings.TrimSpace(tool.Name)]; found {
			result = append(result, normalizedTool)
		}
	}
	return result
}

func appServerDynamicToolSpecs(tools []DynamicTool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		result = append(result, map[string]any{
			"type":        "function",
			"name":        tool.Name,
			"description": tool.Description,
			"inputSchema": append(json.RawMessage(nil), tool.InputSchema...),
		})
	}
	return result
}

func normalizeJSONValue(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, fmt.Errorf("a valid JSON value is required")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, fmt.Errorf("compact JSON value: %w", err)
	}
	return json.RawMessage(compact.Bytes()), nil
}

func normalizeJSONSchema(raw json.RawMessage) (json.RawMessage, error) {
	normalized, err := normalizeJSONValue(raw)
	if err != nil {
		return nil, err
	}
	var schema any
	if err := json.Unmarshal(normalized, &schema); err != nil {
		return nil, fmt.Errorf("decode JSON Schema: %w", err)
	}
	switch schema.(type) {
	case bool, map[string]any:
		return normalized, nil
	default:
		return nil, fmt.Errorf("a JSON Schema object or boolean is required")
	}
}

func (s *appServerSession) respondDynamicToolCall(id, rawParams json.RawMessage) {
	response := s.dynamicToolCallResponse(rawParams)
	if err := s.respondResult(id, response); err != nil {
		s.appendLog("Codex app-server dynamic tool response could not be sent\n")
	}
}

func (s *appServerSession) dynamicToolCallResponse(rawParams json.RawMessage) dynamicToolCallResponse {
	var params dynamicToolCallParams
	if err := json.Unmarshal(rawParams, &params); err != nil || !validDynamicToolCallParams(params) {
		return failedDynamicToolCall("invalid_tool_call", rawParams)
	}
	if params.Namespace != nil && strings.TrimSpace(*params.Namespace) != "" {
		return failedDynamicToolCall("unknown_tool", params.Arguments)
	}

	tool, callContext, limit, failure := s.dynamicToolForCall(params)
	if failure != "" {
		return failedDynamicToolCall(failure, params.Arguments)
	}
	if len(params.Arguments) > limit {
		return failedDynamicToolCall("tool_arguments_too_large", params.Arguments)
	}
	arguments, err := normalizeJSONValue(params.Arguments)
	if err != nil {
		return failedDynamicToolCall("invalid_tool_arguments", params.Arguments)
	}
	response, err := invokeDynamicTool(callContext, tool.Handler, arguments)
	if err != nil {
		if callContext.Err() != nil {
			return failedDynamicToolCall("tool_context_cancelled", arguments)
		}
		return failedDynamicToolCall("tool_failed", arguments)
	}
	if len(response) > limit {
		return failedDynamicToolCall("tool_response_too_large", arguments)
	}
	response, err = normalizeJSONValue(response)
	if err != nil {
		return failedDynamicToolCall("invalid_tool_response", arguments)
	}
	return dynamicToolCallResponse{
		ContentItems: []dynamicToolOutputItem{{Type: "inputText", Text: string(response)}},
		Success:      true,
	}
}

func validDynamicToolCallParams(params dynamicToolCallParams) bool {
	return strings.TrimSpace(params.ThreadID) != "" &&
		strings.TrimSpace(params.TurnID) != "" &&
		strings.TrimSpace(params.CallID) != "" &&
		strings.TrimSpace(params.Tool) != "" &&
		len(params.Arguments) != 0
}

func (s *appServerSession) dynamicToolForCall(params dynamicToolCallParams) (DynamicTool, context.Context, int, string) {
	if s == nil {
		return DynamicTool{}, nil, 0, "tool_unavailable"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.completed || s.threadID == "" || params.ThreadID != s.threadID || s.turnDone == nil || s.turnContext == nil {
		return DynamicTool{}, nil, 0, "tool_unavailable"
	}
	// recordTurnStartResponse installs turnID on the stdout reader before it
	// handles a following request, so a tool call is never authorized until the
	// server has acknowledged turn/start with that exact identity.
	if s.turnID == "" || params.TurnID != s.turnID {
		return DynamicTool{}, nil, 0, "tool_unavailable"
	}
	tool, found := s.dynamicTools[params.Tool]
	if !found {
		return DynamicTool{}, nil, 0, "unknown_tool"
	}
	// The transport cap is deliberately wider than a stage's final-output
	// budget. A stage-owned handler must see an over-budget candidate so it can
	// consume its bounded submission attempt and return the stable, digest-only
	// receipt required by its contract. This generic cap still prevents an
	// unbounded JSON-RPC request from reaching any handler.
	limit := s.req.MaxOutputBytes
	if limit < defaultDynamicToolJSONBytes {
		limit = defaultDynamicToolJSONBytes
	}
	return tool, s.turnContext, limit, ""
}

func invokeDynamicTool(ctx context.Context, handler DynamicToolHandler, arguments json.RawMessage) (json.RawMessage, error) {
	if handler == nil {
		return nil, fmt.Errorf("dynamic tool handler is unavailable")
	}
	type outcome struct {
		response json.RawMessage
		err      error
	}
	completed := make(chan outcome, 1)
	go func() {
		response, err := handler(ctx, append(json.RawMessage(nil), arguments...))
		completed <- outcome{response: response, err: err}
	}()
	select {
	case outcome := <-completed:
		return outcome.response, outcome.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func failedDynamicToolCall(code string, raw []byte) dynamicToolCallResponse {
	// A failed request may contain a rejected stage candidate. Echoing it would
	// turn the App Server transcript into durable-sensitive data, so every
	// transport-level failure carries only its stable code and a full digest.
	encoded, _ := json.Marshal(struct {
		Error  string `json:"error"`
		Digest string `json:"digest"`
	}{Error: code, Digest: dynamicToolPayloadDigest(raw)})
	return dynamicToolCallResponse{
		ContentItems: []dynamicToolOutputItem{{Type: "inputText", Text: string(encoded)}},
		Success:      false,
	}
}

func dynamicToolPayloadDigest(raw []byte) string {
	return "sha256:" + sha256Text(string(raw))
}

func (s *appServerSession) respondResult(id json.RawMessage, result any) error {
	if len(id) == 0 || !json.Valid(id) {
		return fmt.Errorf("codex app-server server request id is invalid")
	}
	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(append([]byte(nil), id...)),
		"result":  result,
	})
	if err != nil {
		return err
	}
	return s.writeMessage(data)
}
