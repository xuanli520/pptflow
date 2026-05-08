package pipeline

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xuanli520/p2r_tui/internal/executor"
)

type appServerCodexReviewSession struct {
	mu        sync.Mutex
	writeMu   sync.Mutex
	req       CodexReviewRequest
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	cancel    context.CancelFunc
	done      chan struct{}
	result    CodexReviewResult
	err       error
	nextID    int
	responses map[int]chan appServerRPCMessage
	threadID  string
	turnID    string
	items     map[string]string
	deltas    map[string]string
	itemOrder []string
	stderr    bytes.Buffer
	completed bool
	envKeys   []string
}

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

func (e *appServerRPCError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) == "" {
		return fmt.Sprintf("codex app-server JSON-RPC error %d", e.Code)
	}
	return fmt.Sprintf("codex app-server JSON-RPC error %d: %s", e.Code, e.Message)
}

func (s *appServerCodexReviewSession) Start(ctx context.Context, request CodexReviewRequest) error {
	s.mu.Lock()
	if s.done != nil {
		s.mu.Unlock()
		return fmt.Errorf("codex app-server review session already started")
	}
	s.req = request
	s.done = make(chan struct{})
	s.responses = map[int]chan appServerRPCMessage{}
	s.items = map[string]string{}
	s.deltas = map[string]string{}
	s.mu.Unlock()

	if !request.Capability.HasAppServer {
		err := fmt.Errorf("codex CLI does not expose app-server; active-turn guidance requires codex app-server turn/steer")
		s.complete(executor.Result{Command: request.Capability.Path + " app-server --listen stdio://", Err: err, Stderr: err.Error()}, err)
		return err
	}
	runCtx := ctx
	if request.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, request.Timeout)
		s.cancel = cancel
	} else {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithCancel(ctx)
		s.cancel = cancel
	}
	args := []string{"app-server", "-c", `approval_policy="never"`, "-c", `sandbox_mode="read-only"`, "--listen", "stdio://"}
	cmd := exec.CommandContext(runCtx, request.Capability.Path, args...)
	cmd.Dir = request.ProjectPath
	if len(request.Env) > 0 {
		cmd.Env = request.Env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		s.complete(executor.Result{Command: commandString(request.Capability.Path, args), Err: err, Stderr: err.Error()}, err)
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.complete(executor.Result{Command: commandString(request.Capability.Path, args), Err: err, Stderr: err.Error()}, err)
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.complete(executor.Result{Command: commandString(request.Capability.Path, args), Err: err, Stderr: err.Error()}, err)
		return err
	}
	preamble := commandString(request.Capability.Path, args) +
		"\n\nPrompt: supplied via app-server turn/start; sha256=" + sha256Text(request.Prompt) +
		"\nCodex capability: " + capabilitySummary(request.Capability) +
		"\nCodex env keys: " + strings.Join(s.envKeys, ",") +
		"\nTimeout: " + request.Timeout.String() +
		"\nStarted: " + time.Now().UTC().Format(time.RFC3339) +
		"\n\n=== codex app-server JSON-RPC stream start ===\n"
	_ = writeText(request.LogPath, preamble)
	if err := cmd.Start(); err != nil {
		s.complete(executor.Result{Command: commandString(request.Capability.Path, args), Err: err, Stderr: err.Error()}, err)
		return err
	}
	s.mu.Lock()
	s.cmd = cmd
	s.stdin = stdin
	s.mu.Unlock()
	go s.readStdout(stdout)
	go s.readStderr(stderr)
	go s.waitProcess(runCtx, commandString(request.Capability.Path, args))

	initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := s.sendRequest(initCtx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "p2r_tui", "version": "0"},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	}); err != nil {
		s.failStart(commandString(request.Capability.Path, args), err)
		return err
	}
	if err := s.sendNotification(initCtx, "initialized", nil); err != nil {
		s.failStart(commandString(request.Capability.Path, args), err)
		return err
	}
	threadResult, err := s.sendRequest(initCtx, "thread/start", map[string]any{
		"approvalPolicy": "never",
		"cwd":            request.ProjectPath,
		"ephemeral":      true,
		"model":          codexModelFromArgs(request.Args),
		"sandbox":        "read-only",
	})
	if err != nil {
		s.failStart(commandString(request.Capability.Path, args), err)
		return err
	}
	threadID := stringAtPath(threadResult, "thread", "id")
	if threadID == "" {
		err := fmt.Errorf("codex app-server thread/start response missing thread.id")
		s.failStart(commandString(request.Capability.Path, args), err)
		return err
	}
	s.mu.Lock()
	s.threadID = threadID
	s.mu.Unlock()
	turnResult, err := s.sendRequest(initCtx, "turn/start", map[string]any{
		"approvalPolicy": "never",
		"cwd":            request.ProjectPath,
		"input": []map[string]any{{
			"type": "text",
			"text": request.Prompt,
		}},
		"model": codexModelFromArgs(request.Args),
		"sandboxPolicy": map[string]any{
			"type":          "readOnly",
			"networkAccess": false,
		},
		"threadId": threadID,
	})
	if err != nil {
		s.failStart(commandString(request.Capability.Path, args), err)
		return err
	}
	turnID := stringAtPath(turnResult, "turn", "id")
	if turnID == "" {
		err := fmt.Errorf("codex app-server turn/start response missing turn.id")
		s.failStart(commandString(request.Capability.Path, args), err)
		return err
	}
	s.mu.Lock()
	s.turnID = turnID
	s.mu.Unlock()
	s.appendLog(fmt.Sprintf("Codex app-server thread=%s turn=%s\n", threadID, turnID))
	return nil
}

func (s *appServerCodexReviewSession) SendGuidance(ctx context.Context, message string) error {
	s.mu.Lock()
	done := s.done
	threadID := s.threadID
	turnID := s.turnID
	s.mu.Unlock()
	if done == nil {
		return fmt.Errorf("codex app-server review session is not started")
	}
	select {
	case <-done:
		return nil
	default:
	}
	if threadID == "" || turnID == "" {
		return fmt.Errorf("codex app-server turn is not ready for guidance")
	}
	s.appendLog(fmt.Sprintf("\n=== codex app-server turn/steer requested ===\n%s\n%s\n", time.Now().UTC().Format(time.RFC3339), message))
	guideCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := s.sendRequest(guideCtx, "turn/steer", map[string]any{
		"expectedTurnId": turnID,
		"input": []map[string]any{{
			"type": "text",
			"text": message,
		}},
		"threadId": threadID,
	})
	if err != nil {
		s.appendLog("Codex app-server turn/steer failed: " + err.Error() + "\n")
		return err
	}
	s.appendLog("Codex app-server turn/steer accepted.\n")
	return nil
}

func (s *appServerCodexReviewSession) Wait(ctx context.Context) (CodexReviewResult, error) {
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done == nil {
		return CodexReviewResult{}, fmt.Errorf("codex app-server review session is not started")
	}
	select {
	case <-done:
	case <-ctx.Done():
		s.stop()
		<-done
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, s.err
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

func (s *appServerCodexReviewSession) readStdout(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		s.appendLog(line + "\n")
		var message appServerRPCMessage
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			continue
		}
		if len(message.ID) > 0 && message.Method == "" {
			if id, ok := rpcIDInt(message.ID); ok {
				s.dispatchResponse(id, message)
			}
			continue
		}
		if len(message.ID) > 0 && message.Method != "" {
			s.respondUnsupported(message.ID, message.Method)
			continue
		}
		if message.Method != "" {
			s.handleNotification(message)
		}
	}
}

func (s *appServerCodexReviewSession) readStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		s.appendLog("STDERR: " + line + "\n")
		s.mu.Lock()
		if s.stderr.Len() < s.req.MaxOutputBytes {
			s.stderr.WriteString(line)
			s.stderr.WriteByte('\n')
		}
		s.mu.Unlock()
	}
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
			"message": "p2r app-server client does not implement server request method " + method,
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

func (s *appServerCodexReviewSession) handleNotification(message appServerRPCMessage) {
	switch message.Method {
	case "item/agentMessage/delta":
		var params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		}
		if json.Unmarshal(message.Params, &params) == nil {
			s.recordDelta(params.TurnID, params.ItemID, params.Delta)
		}
	case "item/completed":
		var params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			Item     struct {
				ID   string `json:"id"`
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if json.Unmarshal(message.Params, &params) == nil && params.Item.Type == "agentMessage" {
			s.recordCompletedItem(params.TurnID, params.Item.ID, params.Item.Text)
		}
	case "turn/completed":
		var params struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(message.Params, &params) == nil {
			s.completeTurn(params.Turn.ID, params.Turn.Status, params.Turn.Error)
		}
	}
}

func (s *appServerCodexReviewSession) recordDelta(turnID, itemID, delta string) {
	if strings.TrimSpace(itemID) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnID != "" && turnID != s.turnID {
		return
	}
	if _, ok := s.deltas[itemID]; !ok {
		s.itemOrder = append(s.itemOrder, itemID)
	}
	s.deltas[itemID] += delta
}

func (s *appServerCodexReviewSession) recordCompletedItem(turnID, itemID, text string) {
	if strings.TrimSpace(itemID) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnID != "" && turnID != s.turnID {
		return
	}
	if _, ok := s.items[itemID]; !ok && s.deltas[itemID] == "" {
		s.itemOrder = append(s.itemOrder, itemID)
	}
	s.items[itemID] = text
}

func (s *appServerCodexReviewSession) completeTurn(turnID, status string, turnErr *struct {
	Message string `json:"message"`
}) {
	s.mu.Lock()
	if s.turnID != "" && turnID != s.turnID {
		s.mu.Unlock()
		return
	}
	report := s.finalReportLocked()
	stderr := s.stderr.String()
	command := ""
	if s.cmd != nil {
		command = commandString(s.cmd.Path, s.cmd.Args[1:])
	}
	s.mu.Unlock()
	result := executor.Result{Command: command, Stdout: report, Stderr: stderr}
	var err error
	if strings.EqualFold(status, "failed") {
		message := "codex app-server turn failed"
		if turnErr != nil && strings.TrimSpace(turnErr.Message) != "" {
			message = turnErr.Message
		}
		err = fmt.Errorf("%s", message)
		result.Err = err
	}
	s.complete(result, err)
}

func (s *appServerCodexReviewSession) finalReportLocked() string {
	if len(s.itemOrder) == 0 {
		for id := range s.items {
			s.itemOrder = append(s.itemOrder, id)
		}
		for id := range s.deltas {
			if _, ok := s.items[id]; !ok {
				s.itemOrder = append(s.itemOrder, id)
			}
		}
		sort.Strings(s.itemOrder)
	}
	var parts []string
	for _, id := range s.itemOrder {
		if text := strings.TrimSpace(s.items[id]); text != "" {
			parts = append(parts, text)
			continue
		}
		if text := strings.TrimSpace(s.deltas[id]); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func (s *appServerCodexReviewSession) waitProcess(ctx context.Context, command string) {
	err := s.cmd.Wait()
	s.mu.Lock()
	completed := s.completed
	stderr := s.stderr.String()
	s.mu.Unlock()
	if completed {
		return
	}
	result := executor.Result{Command: command, Stderr: stderr, Err: err}
	if ctxErr := ctx.Err(); ctxErr != nil {
		result.Timeout = ctxErr == context.DeadlineExceeded
		result.Err = ctxErr
		err = ctxErr
	}
	if err != nil {
		s.complete(result, err)
		return
	}
	err = fmt.Errorf("codex app-server exited before turn completed")
	result.Err = err
	s.complete(result, err)
}

func (s *appServerCodexReviewSession) complete(result executor.Result, err error) {
	s.mu.Lock()
	if s.completed {
		s.mu.Unlock()
		return
	}
	s.completed = true
	s.result.Result = result
	s.err = err
	cancel := s.cancel
	s.responses = map[int]chan appServerRPCMessage{}
	done := s.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		close(done)
	}
}

func (s *appServerCodexReviewSession) stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *appServerCodexReviewSession) failStart(command string, err error) {
	s.mu.Lock()
	stderr := s.stderr.String()
	s.mu.Unlock()
	if strings.TrimSpace(stderr) == "" && err != nil {
		stderr = err.Error()
	}
	s.complete(executor.Result{Command: command, Stderr: stderr, Err: err}, err)
}

func (s *appServerCodexReviewSession) appendLog(content string) {
	s.mu.Lock()
	path := s.req.LogPath
	s.mu.Unlock()
	if strings.TrimSpace(path) == "" {
		return
	}
	_ = appendText(path, content)
}

func commandString(name string, args []string) string {
	return strings.Join(append([]string{name}, args...), " ")
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

func codexModelFromArgs(args []string) any {
	for i, arg := range args {
		if arg == "--model" || arg == "-m" {
			if i+1 < len(args) && strings.TrimSpace(args[i+1]) != "" {
				return strings.TrimSpace(args[i+1])
			}
			return nil
		}
		for _, prefix := range []string{"--model=", "-m="} {
			if strings.HasPrefix(arg, prefix) && strings.TrimSpace(strings.TrimPrefix(arg, prefix)) != "" {
				return strings.TrimSpace(strings.TrimPrefix(arg, prefix))
			}
		}
	}
	return nil
}
