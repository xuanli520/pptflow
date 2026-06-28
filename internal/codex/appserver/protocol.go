package appserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type appServerRPCMessage struct {
	JSONRPC string             `json:"jsonrpc,omitempty"`
	ID      json.RawMessage    `json:"id,omitempty"`
	Method  string             `json:"method,omitempty"`
	Params  json.RawMessage    `json:"params,omitempty"`
	Result  json.RawMessage    `json:"result,omitempty"`
	Error   *appServerRPCError `json:"error,omitempty"`
}

type appServerRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type appServerItem struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Text string `json:"text"`
}

func (e *appServerRPCError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) == "" {
		return fmt.Sprintf("codex app-server JSON-RPC error %d", e.Code)
	}
	return fmt.Sprintf("codex app-server JSON-RPC error %d: %s", e.Code, e.Message)
}

func (s *appServerCodexReviewSession) sendRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id, ch := s.registerResponse()
	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	data, err := json.Marshal(request)
	if err != nil {
		s.unregisterResponse(id)
		return nil, err
	}
	s.writeMu.Lock()
	_, writeErr := s.stdin.Write(append(data, '\n'))
	s.writeMu.Unlock()
	if writeErr != nil {
		s.unregisterResponse(id)
		return nil, writeErr
	}
	select {
	case response := <-ch:
		if response.Error != nil {
			return nil, response.Error
		}
		return response.Result, nil
	case <-ctx.Done():
		s.unregisterResponse(id)
		return nil, ctx.Err()
	case <-s.done:
		return nil, fmt.Errorf("codex app-server exited before %s completed", method)
	}
}

func (s *appServerCodexReviewSession) sendNotification(ctx context.Context, method string, params any) error {
	notification := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		notification["params"] = params
	}
	data, err := json.Marshal(notification)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return fmt.Errorf("codex app-server exited before %s notification was sent", method)
	default:
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.stdin == nil {
		return fmt.Errorf("codex app-server stdin is not available")
	}
	_, err = s.stdin.Write(append(data, '\n'))
	return err
}

func (s *appServerCodexReviewSession) registerResponse() (int, chan appServerRPCMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := s.nextID
	ch := make(chan appServerRPCMessage, 1)
	s.responses[id] = ch
	return id, ch
}

func (s *appServerCodexReviewSession) unregisterResponse(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.responses, id)
}

func (s *appServerCodexReviewSession) dispatchResponse(id int, message appServerRPCMessage) {
	s.mu.Lock()
	ch := s.responses[id]
	delete(s.responses, id)
	s.mu.Unlock()
	if ch != nil {
		ch <- message
	}
}

func (s *appServerCodexReviewSession) respondUnsupported(id json.RawMessage, method string) {
	response := map[string]any{
		"jsonrpc": "2.0",
		"error": map[string]any{
			"code":    -32601,
			"message": "pptflow app-server client does not implement server request method " + method,
		},
	}
	var payload map[string]json.RawMessage
	data, err := json.Marshal(response)
	if err != nil {
		return
	}
	if json.Unmarshal(data, &payload) != nil {
		return
	}
	payload["id"] = id
	data, err = json.Marshal(payload)
	if err != nil {
		return
	}
	s.writeMu.Lock()
	if s.stdin != nil {
		_, _ = s.stdin.Write(append(data, '\n'))
	}
	s.writeMu.Unlock()
}

func rpcIDInt(raw json.RawMessage) (int, bool) {
	var id int
	if json.Unmarshal(raw, &id) == nil {
		return id, true
	}
	return 0, false
}

func stringAtPath(raw json.RawMessage, path ...string) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	for _, key := range path {
		object, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		value = object[key]
	}
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func appServerThreadStartParams(request Request) map[string]any {
	params := map[string]any{
		"approvalPolicy": "never",
		"cwd":            request.ProjectPath,
		"ephemeral":      true,
		"sandbox":        normalizeSandboxMode(request.SandboxMode),
	}
	setAppServerModelParam(params, request.Model)
	return params
}

func appServerTurnStartParams(request Request, threadID string) map[string]any {
	params := map[string]any{
		"approvalPolicy": "never",
		"cwd":            request.ProjectPath,
		"input": []map[string]any{{
			"type": "text",
			"text": request.Prompt,
		}},
		"sandboxPolicy": map[string]any{
			"type":          normalizeSandboxPolicy(request.SandboxPolicy),
			"networkAccess": request.NetworkAccess,
		},
		"threadId": threadID,
	}
	setAppServerModelParam(params, request.Model)
	return params
}

func normalizeSandboxMode(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "read-only"
	}
	return value
}

func normalizeSandboxPolicy(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "readOnly"
	}
	return value
}

func setAppServerModelParam(params map[string]any, model string) {
	if strings.TrimSpace(model) != "" {
		params["model"] = model
	}
}
