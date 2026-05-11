package pipeline

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/xuanli520/p2r_tui/internal/codex"
	"github.com/xuanli520/p2r_tui/internal/executor"
)

type appServerCodexReviewSession struct {
	mu                    sync.Mutex
	writeMu               sync.Mutex
	req                   CodexReviewRequest
	cmd                   *exec.Cmd
	stdin                 io.WriteCloser
	processCtx            context.Context
	cancel                context.CancelFunc
	done                  chan struct{}
	result                CodexReviewResult
	err                   error
	nextID                int
	responses             map[int]chan appServerRPCMessage
	threadID              string
	turnID                string
	items                 map[string]string
	deltas                map[string]string
	deltaLogged           map[string]bool
	deltaPreview          map[string]string
	deltaPreviewTruncated map[string]bool
	activityPreview       string
	activityTruncated     bool
	agentPreviewStarted   bool
	itemDone              map[string]bool
	itemOrder             []string
	stderr                bytes.Buffer
	completed             bool
	envKeys               []string
	warnings              []ArtifactWarning
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
	s.deltaLogged = map[string]bool{}
	s.deltaPreview = map[string]string{}
	s.deltaPreviewTruncated = map[string]bool{}
	s.itemDone = map[string]bool{}
	s.mu.Unlock()

	if !request.Capability.HasAppServer {
		err := fmt.Errorf("codex CLI does not expose app-server; active-turn guidance requires codex app-server turn/steer")
		s.complete(executor.Result{Command: request.Capability.Path + " app-server --listen stdio://", Err: err, Stderr: err.Error()}, err)
		return err
	}
	runCtx := ctx
	var cancel context.CancelFunc
	if request.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, request.Timeout)
	} else {
		runCtx, cancel = context.WithCancel(ctx)
	}
	s.mu.Lock()
	s.processCtx = runCtx
	s.cancel = cancel
	s.mu.Unlock()
	args := []string{"app-server", "-c", `approval_policy="never"`, "-c", `sandbox_mode="read-only"`, "--listen", "stdio://"}
	cmd := exec.CommandContext(runCtx, request.Capability.Path, args...)
	executor.ConfigureCommand(cmd)
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
		"\n\n=== codex app-server JSON-RPC compact event log start ===\n"
	if err := writeText(request.LogPath, preamble); err != nil {
		s.addArtifactWarning(newArtifactWarning(request.LogPath, "write_text", false, err))
	}
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
	threadResult, err := s.sendRequest(initCtx, "thread/start", appServerThreadStartParams(request))
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
	turnResult, err := s.sendRequest(initCtx, "turn/start", appServerTurnStartParams(request, threadID))
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
	scanner.Buffer(make([]byte, 0, 64*1024), appServerMaxLineBytes(s.maxOutputBytes()))
	for scanner.Scan() {
		line := scanner.Text()
		var message appServerRPCMessage
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			s.appendLog("STDOUT: " + truncateAppServerLogValue(line) + "\n")
			continue
		}
		isDeltaNotification := len(message.ID) == 0 && message.Method == "item/agentMessage/delta"
		if !isDeltaNotification {
			s.appendLog(formatAppServerRPCLogLine(message))
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
	if err := scanner.Err(); err != nil {
		s.completeStreamError("stdout", err)
	}
}

func (s *appServerCodexReviewSession) readStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), appServerMaxLineBytes(s.maxOutputBytes()))
	for scanner.Scan() {
		line := scanner.Text()
		s.appendLog("STDERR: " + line + "\n")
		s.recordStderrLine(line)
	}
	if err := scanner.Err(); err != nil {
		s.completeStreamError("stderr", err)
	}
}

func appServerMaxLineBytes(maxOutputBytes int) int {
	const (
		defaultLimit = 4 * 1024 * 1024
		noLimitCap   = 64 * 1024 * 1024
	)
	if maxOutputBytes <= 0 {
		return noLimitCap
	}
	limit := maxOutputBytes + 1024*1024
	if limit < defaultLimit {
		return defaultLimit
	}
	return limit
}

func (s *appServerCodexReviewSession) maxOutputBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.req.MaxOutputBytes
}

func (s *appServerCodexReviewSession) recordStderrLine(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := s.req.MaxOutputBytes
	if limit <= 0 {
		s.stderr.WriteString(line)
		s.stderr.WriteByte('\n')
		return
	}
	remaining := limit - s.stderr.Len()
	if remaining <= 0 {
		return
	}
	if len(line) >= remaining {
		s.stderr.WriteString(line[:remaining])
		return
	}
	s.stderr.WriteString(line)
	if s.stderr.Len() < limit {
		s.stderr.WriteByte('\n')
	}
}

func (s *appServerCodexReviewSession) completeStreamError(stream string, err error) {
	if err == nil {
		return
	}
	if s.ignoreStreamError(err) {
		return
	}
	streamErr := fmt.Errorf("codex app-server %s stream error: %w", stream, err)
	s.appendLog("Codex app-server " + stream + " stream error: " + err.Error() + "\n")
	s.mu.Lock()
	command := ""
	if s.cmd != nil {
		command = commandString(s.cmd.Path, s.cmd.Args[1:])
	}
	stderr := s.stderr.String()
	s.mu.Unlock()
	s.complete(executor.Result{Command: command, Stderr: stderr, Err: streamErr}, streamErr)
}

func (s *appServerCodexReviewSession) ignoreStreamError(err error) bool {
	s.mu.Lock()
	completed := s.completed
	processCtx := s.processCtx
	s.mu.Unlock()
	if completed {
		return true
	}
	return processCtx != nil && processCtx.Err() != nil && isClosedPipeReadError(err)
}

func isClosedPipeReadError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrClosed) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "file already closed") || strings.Contains(text, "use of closed file")
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

func formatAppServerRPCLogLine(message appServerRPCMessage) string {
	id := appServerRPCIDLog(message.ID)
	if len(message.ID) > 0 && message.Method == "" {
		if message.Error != nil {
			return fmt.Sprintf("JSON-RPC response id=%s error code=%d message=%q\n", id, message.Error.Code, truncateAppServerLogValue(message.Error.Message))
		}
		return fmt.Sprintf("JSON-RPC response id=%s result_%s\n", id, appServerJSONSummary(message.Result))
	}
	if len(message.ID) > 0 && message.Method != "" {
		return fmt.Sprintf("JSON-RPC server request id=%s method=%s params_%s\n", id, message.Method, appServerJSONSummary(message.Params))
	}
	if message.Method == "" {
		return fmt.Sprintf("JSON-RPC message params_%s\n", appServerJSONSummary(message.Params))
	}
	switch message.Method {
	case "item/agentMessage/delta":
		var params struct {
			TurnID string `json:"turnId"`
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		if json.Unmarshal(message.Params, &params) == nil {
			starts, ends := staticReviewMarkerCounts(params.Delta)
			return fmt.Sprintf(
				"JSON-RPC notification item/agentMessage/delta turn=%s item=%s delta_bytes=%d delta_sha256=%s contract_starts=%d contract_ends=%d\n",
				compactAppServerLogID(params.TurnID),
				compactAppServerLogID(params.ItemID),
				len(params.Delta),
				shortAppServerLogHash(params.Delta),
				starts,
				ends,
			)
		}
	case "item/completed":
		var params struct {
			TurnID string        `json:"turnId"`
			Item   appServerItem `json:"item"`
		}
		if json.Unmarshal(message.Params, &params) == nil {
			starts, ends := staticReviewMarkerCounts(params.Item.Text)
			return fmt.Sprintf(
				"JSON-RPC notification item/completed turn=%s item=%s type=%s text_bytes=%d text_sha256=%s contract_starts=%d contract_ends=%d\n",
				compactAppServerLogID(params.TurnID),
				compactAppServerLogID(params.Item.ID),
				truncateAppServerLogValue(params.Item.Type),
				len(params.Item.Text),
				shortAppServerLogHash(params.Item.Text),
				starts,
				ends,
			)
		}
	case "turn/completed":
		var params struct {
			Turn struct {
				ID     string          `json:"id"`
				Items  []appServerItem `json:"items"`
				Status string          `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(message.Params, &params) == nil {
			if params.Turn.Error != nil && strings.TrimSpace(params.Turn.Error.Message) != "" {
				return fmt.Sprintf(
					"JSON-RPC notification turn/completed turn=%s status=%s items=%d error=%q\n",
					compactAppServerLogID(params.Turn.ID),
					truncateAppServerLogValue(params.Turn.Status),
					len(params.Turn.Items),
					truncateAppServerLogValue(params.Turn.Error.Message),
				)
			}
			return fmt.Sprintf(
				"JSON-RPC notification turn/completed turn=%s status=%s items=%d\n",
				compactAppServerLogID(params.Turn.ID),
				truncateAppServerLogValue(params.Turn.Status),
				len(params.Turn.Items),
			)
		}
	}
	return fmt.Sprintf("JSON-RPC notification %s params_%s\n", message.Method, appServerJSONSummary(message.Params))
}

func appServerRPCIDLog(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "-"
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return compactAppServerLogID(text)
	}
	return truncateAppServerLogValue(string(raw))
}

func appServerJSONSummary(raw json.RawMessage) string {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" || strings.TrimSpace(string(raw)) == "null" {
		return "empty"
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil && object != nil {
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if len(keys) > 8 {
			keys = append(keys[:8], fmt.Sprintf("+%d", len(keys)-8))
		}
		return fmt.Sprintf("keys=%s bytes=%d sha256=%s", strings.Join(keys, ","), len(raw), shortAppServerLogHash(string(raw)))
	}
	return fmt.Sprintf("bytes=%d sha256=%s", len(raw), shortAppServerLogHash(string(raw)))
}

func compactAppServerLogID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	if len(value) <= 32 {
		return value
	}
	return value[:18] + "..." + value[len(value)-10:]
}

func truncateAppServerLogValue(value string) string {
	const limit = 512
	value = strings.ReplaceAll(value, "\r", "\\r")
	value = strings.ReplaceAll(value, "\n", "\\n")
	value = strings.ReplaceAll(value, "\t", "\\t")
	return truncateStringPrefix(value, limit)
}

func shortAppServerLogHash(value string) string {
	const length = 12
	sum := sha256Text(value)
	if len(sum) <= length {
		return sum
	}
	return sum[:length]
}

func prefixRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	for index := range value {
		if limit == 0 {
			return value[:index]
		}
		limit--
	}
	return value
}

type aggregatedDeltaLog struct {
	turnID string
	itemID string
	text   string
}

func formatAggregatedDeltaLogLine(log aggregatedDeltaLog) string {
	starts, ends := staticReviewMarkerCounts(log.text)
	return fmt.Sprintf(
		"JSON-RPC notification item/agentMessage/delta aggregated turn=%s item=%s total_bytes=%d delta_sha256=%s contract_starts=%d contract_ends=%d text_prefix=%q\n",
		compactAppServerLogID(log.turnID),
		compactAppServerLogID(log.itemID),
		len(log.text),
		shortAppServerLogHash(log.text),
		starts,
		ends,
		truncateAppServerLogValue(prefixRunes(log.text, 10)),
	)
}

func (s *appServerCodexReviewSession) aggregatedDeltaLogForItem(itemID string) (aggregatedDeltaLog, bool) {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return aggregatedDeltaLog{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	text := s.deltas[itemID]
	if text == "" {
		return aggregatedDeltaLog{}, false
	}
	if s.deltaLogged == nil {
		s.deltaLogged = map[string]bool{}
	}
	if s.deltaLogged[itemID] {
		return aggregatedDeltaLog{}, false
	}
	s.deltaLogged[itemID] = true
	return aggregatedDeltaLog{turnID: s.turnID, itemID: itemID, text: text}, true
}

func (s *appServerCodexReviewSession) logAggregatedDelta(itemID string) {
	log, ok := s.aggregatedDeltaLogForItem(itemID)
	if !ok {
		return
	}
	s.appendLog(formatAggregatedDeltaLogLine(log))
}

func (s *appServerCodexReviewSession) remainingAggregatedDeltaLogs() []aggregatedDeltaLog {
	s.mu.Lock()
	if s.deltaLogged == nil {
		s.deltaLogged = map[string]bool{}
	}
	logs := make([]aggregatedDeltaLog, 0, len(s.deltas))
	for itemID, text := range s.deltas {
		if text == "" || s.deltaLogged[itemID] {
			continue
		}
		s.deltaLogged[itemID] = true
		logs = append(logs, aggregatedDeltaLog{turnID: s.turnID, itemID: itemID, text: text})
	}
	s.mu.Unlock()
	sort.Slice(logs, func(i, j int) bool {
		return logs[i].itemID < logs[j].itemID
	})
	return logs
}

func (s *appServerCodexReviewSession) logRemainingAggregatedDeltas() {
	for _, log := range s.remainingAggregatedDeltaLogs() {
		s.appendLog(formatAggregatedDeltaLogLine(log))
	}
}

const appServerDeltaPreviewMaxBytes = 64 * 1024

func appendDeltaPreview(current, delta string, truncated bool) (string, bool) {
	return deltaPreviewText(current+delta, truncated)
}

func deltaPreviewText(text string, truncated bool) (string, bool) {
	if len(text) <= appServerDeltaPreviewMaxBytes {
		return strings.ToValidUTF8(text, ""), truncated
	}
	return utf8SafeSuffix(text, appServerDeltaPreviewMaxBytes), true
}

func utf8SafeSuffix(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return strings.ToValidUTF8(value, "")
	}
	start := len(value) - limit
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return strings.ToValidUTF8(value[start:], "")
}

func (s *appServerCodexReviewSession) emitDeltaUpdate(update CodexDeltaUpdate, ok bool) {
	if !ok || s.req.OnDelta == nil {
		return
	}
	s.req.OnDelta(update)
}

func (s *appServerCodexReviewSession) handleNotification(message appServerRPCMessage) {
	switch message.Method {
	case "item/started":
		var params struct {
			ThreadID string          `json:"threadId"`
			TurnID   string          `json:"turnId"`
			Item     json.RawMessage `json:"item"`
		}
		if json.Unmarshal(message.Params, &params) == nil {
			item := appServerItemFromRaw(params.Item)
			s.recordItemActivity(params.TurnID, item.ID, item.Type, params.Item, false)
		}
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
			ThreadID string        `json:"threadId"`
			TurnID   string        `json:"turnId"`
			Item     appServerItem `json:"item"`
		}
		if json.Unmarshal(message.Params, &params) == nil && params.Item.Type == "agentMessage" {
			s.recordCompletedItem(params.TurnID, params.Item.ID, params.Item.Text)
			s.logAggregatedDelta(params.Item.ID)
		} else if json.Unmarshal(message.Params, &params) == nil {
			s.recordItemActivity(params.TurnID, params.Item.ID, params.Item.Type, message.Params, true)
		}
	case "turn/completed":
		var params struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID     string          `json:"id"`
				Items  []appServerItem `json:"items"`
				Status string          `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(message.Params, &params) == nil {
			for _, item := range params.Turn.Items {
				if item.Type == "agentMessage" {
					s.recordCompletedItem(params.Turn.ID, item.ID, item.Text)
				}
			}
			s.logRemainingAggregatedDeltas()
			s.completeTurn(params.Turn.ID, params.Turn.Status, params.Turn.Error)
		}
	}
}

func appServerItemFromRaw(raw json.RawMessage) appServerItem {
	var item appServerItem
	_ = json.Unmarshal(raw, &item)
	return item
}

func (s *appServerCodexReviewSession) recordItemActivity(turnID, itemID, itemType string, raw json.RawMessage, done bool) {
	itemType = strings.TrimSpace(itemType)
	if itemType == "" || itemType == "userMessage" {
		return
	}
	s.mu.Lock()
	if s.completed || (s.turnID != "" && turnID != s.turnID) || s.agentPreviewStarted {
		s.mu.Unlock()
		return
	}
	line := codexActivityLine(itemType, raw, done)
	if line == "" {
		s.mu.Unlock()
		return
	}
	if itemID == "" {
		itemID = "__codex_activity__"
	}
	preview, truncated := deltaPreviewText(s.activityPreview+line+"\n", s.activityTruncated)
	s.activityPreview = preview
	s.activityTruncated = truncated
	update := CodexDeltaUpdate{
		TurnID:    firstNonEmpty(turnID, s.turnID),
		ItemID:    itemID,
		Delta:     line + "\n",
		Text:      preview,
		Truncated: truncated,
	}
	s.mu.Unlock()
	s.emitDeltaUpdate(update, true)
}

func codexActivityLine(itemType string, raw json.RawMessage, done bool) string {
	detail := appServerActivityDetail(raw)
	switch itemType {
	case "agentMessage":
		if done {
			return "Codex 已完成回复。"
		}
		return "Codex 正在生成回复..."
	case "reasoning":
		if done {
			return "Codex 完成一段分析。"
		}
		return "Codex 正在分析..."
	case "commandExecution":
		if done {
			if detail != "" {
				return "Codex 完成命令: " + detail
			}
			return "Codex 完成命令执行。"
		}
		if detail != "" {
			return "Codex 正在执行命令: " + detail
		}
		return "Codex 正在执行命令..."
	default:
		if done {
			return "Codex 完成事件: " + truncateAppServerLogValue(itemType)
		}
		return "Codex 事件: " + truncateAppServerLogValue(itemType)
	}
}

func appServerActivityDetail(raw json.RawMessage) string {
	for _, path := range [][]string{
		{"item", "command"},
		{"item", "cmd"},
		{"item", "name"},
		{"item", "title"},
		{"command"},
		{"cmd"},
		{"name"},
		{"title"},
	} {
		if value := appServerStringAtPath(raw, path...); value != "" {
			return truncateAppServerLogValue(value)
		}
	}
	return ""
}

func appServerStringAtPath(raw json.RawMessage, path ...string) string {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	for _, key := range path {
		object, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		value = object[key]
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				parts = append(parts, strings.TrimSpace(text))
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

func (s *appServerCodexReviewSession) recordDelta(turnID, itemID, delta string) {
	if strings.TrimSpace(itemID) == "" {
		return
	}
	s.mu.Lock()
	if s.completed || (s.turnID != "" && turnID != s.turnID) {
		s.mu.Unlock()
		return
	}
	if s.deltas == nil {
		s.deltas = map[string]string{}
	}
	if s.deltaPreview == nil {
		s.deltaPreview = map[string]string{}
	}
	if s.deltaPreviewTruncated == nil {
		s.deltaPreviewTruncated = map[string]bool{}
	}
	if s.itemDone == nil {
		s.itemDone = map[string]bool{}
	}
	if _, ok := s.deltas[itemID]; !ok {
		if _, hasItem := s.items[itemID]; !hasItem {
			s.itemOrder = append(s.itemOrder, itemID)
		}
	}
	s.deltas[itemID] += delta
	s.agentPreviewStarted = true
	if s.itemDone[itemID] {
		s.mu.Unlock()
		return
	}
	preview, truncated := appendDeltaPreview(s.deltaPreview[itemID], delta, s.deltaPreviewTruncated[itemID])
	s.deltaPreview[itemID] = preview
	s.deltaPreviewTruncated[itemID] = truncated
	update := CodexDeltaUpdate{
		TurnID:    firstNonEmpty(turnID, s.turnID),
		ItemID:    itemID,
		Delta:     delta,
		Text:      preview,
		Truncated: truncated,
	}
	s.mu.Unlock()
	s.emitDeltaUpdate(update, true)
}

func (s *appServerCodexReviewSession) recordCompletedItem(turnID, itemID, text string) {
	if strings.TrimSpace(itemID) == "" {
		return
	}
	s.mu.Lock()
	if s.completed || (s.turnID != "" && turnID != s.turnID) {
		s.mu.Unlock()
		return
	}
	if s.items == nil {
		s.items = map[string]string{}
	}
	if s.deltas == nil {
		s.deltas = map[string]string{}
	}
	if s.deltaPreview == nil {
		s.deltaPreview = map[string]string{}
	}
	if s.deltaPreviewTruncated == nil {
		s.deltaPreviewTruncated = map[string]bool{}
	}
	if s.itemDone == nil {
		s.itemDone = map[string]bool{}
	}
	if _, ok := s.items[itemID]; !ok && s.deltas[itemID] == "" {
		s.itemOrder = append(s.itemOrder, itemID)
	}
	s.items[itemID] = text
	hadPreview := s.deltaPreview[itemID] != "" || s.deltas[itemID] != ""
	s.itemDone[itemID] = true
	if text != "" {
		s.agentPreviewStarted = true
	}
	preview := s.deltaPreview[itemID]
	truncated := s.deltaPreviewTruncated[itemID]
	if text != "" {
		preview, truncated = deltaPreviewText(text, false)
	}
	update := CodexDeltaUpdate{
		TurnID:    firstNonEmpty(turnID, s.turnID),
		ItemID:    itemID,
		Text:      preview,
		Done:      true,
		Truncated: truncated,
	}
	s.mu.Unlock()
	s.emitDeltaUpdate(update, hadPreview || text != "")
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
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed":
	case "failed":
		message := "codex app-server turn failed"
		if turnErr != nil && strings.TrimSpace(turnErr.Message) != "" {
			message = turnErr.Message
		}
		err = fmt.Errorf("%s", message)
		result.Err = err
	case "":
		err = fmt.Errorf("codex app-server turn completed without a status")
		result.Err = err
	default:
		message := fmt.Sprintf("codex app-server turn ended with status %q", status)
		if turnErr != nil && strings.TrimSpace(turnErr.Message) != "" {
			message += ": " + strings.TrimSpace(turnErr.Message)
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
	cancel := s.cancel
	s.responses = map[int]chan appServerRPCMessage{}
	done := s.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.logRemainingAggregatedDeltas()
	s.mu.Lock()
	s.result.Result = result
	s.result.ArtifactWarnings = append(s.result.ArtifactWarnings, s.warnings...)
	s.err = err
	s.mu.Unlock()
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
	if err := appendText(path, content); err != nil {
		s.addArtifactWarning(newArtifactWarning(path, "append_text", false, err))
	}
}

func (s *appServerCodexReviewSession) addArtifactWarning(warning ArtifactWarning) {
	if warning.OK() {
		return
	}
	s.mu.Lock()
	s.warnings = append(s.warnings, warning)
	s.mu.Unlock()
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

func appServerThreadStartParams(request CodexReviewRequest) map[string]any {
	params := map[string]any{
		"approvalPolicy": "never",
		"cwd":            request.ProjectPath,
		"ephemeral":      true,
		"sandbox":        "read-only",
	}
	setAppServerModelParam(params, request.Args)
	return params
}

func appServerTurnStartParams(request CodexReviewRequest, threadID string) map[string]any {
	params := map[string]any{
		"approvalPolicy": "never",
		"cwd":            request.ProjectPath,
		"input": []map[string]any{{
			"type": "text",
			"text": request.Prompt,
		}},
		"sandboxPolicy": map[string]any{
			"type":          "readOnly",
			"networkAccess": false,
		},
		"threadId": threadID,
	}
	setAppServerModelParam(params, request.Args)
	return params
}

func setAppServerModelParam(params map[string]any, args []string) {
	if model := codex.AppServerModelFromArgs(args); model != "" {
		params["model"] = model
	}
}
