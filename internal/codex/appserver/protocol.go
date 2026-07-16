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

func (s *appServerSession) sendRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id, ch := s.registerResponse()
	if method == "turn/start" {
		s.mu.Lock()
		s.turnStartRequestID = id
		s.mu.Unlock()
	}
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
	writeErr := s.writeMessage(data)
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

func (s *appServerSession) sendNotification(ctx context.Context, method string, params any) error {
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
	return s.writeMessage(data)
}

func (s *appServerSession) registerResponse() (int, chan appServerRPCMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := s.nextID
	ch := make(chan appServerRPCMessage, 1)
	s.responses[id] = ch
	return id, ch
}

func (s *appServerSession) unregisterResponse(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.responses, id)
	if s.turnStartRequestID == id {
		s.turnStartRequestID = 0
	}
}

func (s *appServerSession) dispatchResponse(id int, message appServerRPCMessage) {
	s.mu.Lock()
	ch := s.responses[id]
	delete(s.responses, id)
	s.mu.Unlock()
	if ch != nil {
		ch <- message
	}
}

// recordTurnStartResponse makes the server-issued turn ID visible before the
// stdout reader processes any following server request. A dynamic tool request
// can legally arrive immediately after the turn/start response, but never
// before it; requiring this state prevents a forged early turn ID from calling
// a session-private handler.
func (s *appServerSession) recordTurnStartResponse(id int, message appServerRPCMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnStartRequestID != id {
		return
	}
	s.turnStartRequestID = 0
	if message.Error != nil || s.turnDone == nil || s.completed {
		return
	}
	if turnID := stringAtPath(message.Result, "turn", "id"); turnID != "" {
		s.turnID = turnID
	}
}

func (s *appServerSession) respondUnsupported(id json.RawMessage, method string) {
	response := map[string]any{
		"jsonrpc": "2.0",
		"error": map[string]any{
			"code":    -32601,
			"message": "agent-runtime app-server client does not implement server request method " + method,
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
	_ = s.writeMessage(data)
}

// writeMessage serializes JSON-RPC writes with shutdown.  The process may
// exit while a caller is starting or steering a turn; report that condition
// instead of dereferencing a closed stdin pipe.
func (s *appServerSession) writeMessage(data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	stdin := s.stdin
	s.mu.Unlock()
	if stdin == nil {
		return fmt.Errorf("codex app-server stdin is not available")
	}
	_, err := stdin.Write(append(data, '\n'))
	return err
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
	if len(request.DynamicTools) > 0 {
		params["dynamicTools"] = appServerDynamicToolSpecs(request.DynamicTools)
	}
	setAppServerModelParam(params, request.Model)
	return params
}

func appServerClientInfo(request Request) map[string]any {
	name := strings.TrimSpace(request.ClientName)
	if name == "" {
		name = "agent-runtime"
	}
	version := strings.TrimSpace(request.ClientVersion)
	if version == "" {
		version = "1"
	}
	return map[string]any{"name": name, "version": version}
}

func appServerTurnStartParams(request Request, threadID string) map[string]any {
	return appServerTurnStartParamsWithOutputSchema(request, threadID, nil)
}

func appServerTurnStartParamsWithOutputSchema(request Request, threadID string, outputSchema json.RawMessage) map[string]any {
	params := map[string]any{
		"approvalPolicy": "never",
		"cwd":            request.ProjectPath,
		"input":          appServerInput(request),
		"sandboxPolicy":  appServerSandboxPolicy(request),
		"threadId":       threadID,
	}
	if len(request.WorkspaceRoots) > 0 {
		params["runtimeWorkspaceRoots"] = append([]string{}, request.WorkspaceRoots...)
	}
	if len(outputSchema) > 0 {
		params["outputSchema"] = append(json.RawMessage(nil), outputSchema...)
	}
	setAppServerModelParam(params, request.Model)
	return params
}

func appServerSandboxPolicy(request Request) map[string]any {
	policyType := normalizeSandboxPolicy(request.SandboxPolicy)
	policy := map[string]any{"type": policyType}
	switch policyType {
	case "workspaceWrite":
		policy["networkAccess"] = request.NetworkAccess
		if len(request.WorkspaceRoots) > 0 {
			policy["writableRoots"] = append([]string{}, request.WorkspaceRoots...)
		}
	case "readOnly":
		policy["networkAccess"] = request.NetworkAccess
	}
	return policy
}

func appServerInput(request Request) []map[string]any {
	input := make([]map[string]any, 0, len(request.Input)+1)
	if strings.TrimSpace(request.Prompt) != "" {
		input = append(input, map[string]any{"type": "text", "text": request.Prompt})
	}
	for _, part := range request.Input {
		item := appServerInputPart(part)
		if item != nil {
			input = append(input, item)
		}
	}
	if len(input) == 0 {
		return []map[string]any{{"type": "text", "text": ""}}
	}
	return input
}

func appServerInputPart(part InputPart) map[string]any {
	switch strings.TrimSpace(part.Type) {
	case "text":
		if strings.TrimSpace(part.Text) == "" {
			return nil
		}
		return map[string]any{"type": "text", "text": part.Text}
	case "image":
		if strings.TrimSpace(part.URL) == "" {
			return nil
		}
		item := map[string]any{"type": "image", "url": strings.TrimSpace(part.URL)}
		setAppServerInputDetail(item, part.Detail)
		return item
	case "localImage":
		if strings.TrimSpace(part.Path) == "" {
			return nil
		}
		item := map[string]any{"type": "localImage", "path": strings.TrimSpace(part.Path)}
		setAppServerInputDetail(item, part.Detail)
		return item
	default:
		return nil
	}
}

func setAppServerInputDetail(item map[string]any, detail string) {
	detail = strings.TrimSpace(detail)
	if detail == "high" || detail == "original" {
		item["detail"] = detail
	}
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
	switch value {
	case "", "read-only", "read_only":
		return "readOnly"
	case "workspace-write", "workspace_write", "readWrite":
		return "workspaceWrite"
	case "danger-full-access", "danger_full_access":
		return "dangerFullAccess"
	default:
		return value
	}
}

func setAppServerModelParam(params map[string]any, model string) {
	if strings.TrimSpace(model) != "" {
		params["model"] = model
	}
}
