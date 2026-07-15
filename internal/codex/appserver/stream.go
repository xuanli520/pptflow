package appserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/executor"
)

func (s *appServerSession) readStdout(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), appServerMaxLineBytes(s.maxOutputBytes()))
	for scanner.Scan() {
		line := scanner.Text()
		var message appServerRPCMessage
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			s.appendLog("STDOUT: " + truncateAppServerLogValue(line) + "\n")
			s.recordStdoutDiagnosticLine(line)
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

func (s *appServerSession) readStderr(stderr io.Reader) {
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

func (s *appServerSession) maxOutputBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.req.MaxOutputBytes
}

func (s *appServerSession) recordStderrLine(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	appendBoundedLine(&s.stderr, line, s.req.MaxOutputBytes)
}

func (s *appServerSession) recordStdoutDiagnosticLine(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	appendBoundedLine(&s.stdoutDiagnostics, line, s.req.MaxOutputBytes)
}

func appendBoundedLine(buffer *bytes.Buffer, line string, limit int) {
	if buffer == nil {
		return
	}
	if limit <= 0 {
		buffer.WriteString(line)
		buffer.WriteByte('\n')
		return
	}
	remaining := limit - buffer.Len()
	if remaining <= 0 {
		return
	}
	if len(line) >= remaining {
		buffer.WriteString(line[:remaining])
		return
	}
	buffer.WriteString(line)
	if buffer.Len() < limit {
		buffer.WriteByte('\n')
	}
}

func (s *appServerSession) completeStreamError(stream string, err error) {
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
	stdout := s.stdoutDiagnostics.String()
	stderr := s.stderr.String()
	s.mu.Unlock()
	s.complete(executor.Result{Command: command, Stdout: stdout, Stderr: stderr, Err: streamErr}, streamErr)
}

func (s *appServerSession) ignoreStreamError(err error) bool {
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

func (s *appServerSession) handleNotification(message appServerRPCMessage) {
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

func (s *appServerSession) completeTurn(turnID, status string, turnErr *struct {
	Message string `json:"message"`
}) {
	s.mu.Lock()
	if s.turnDone == nil || (s.turnID != "" && turnID != s.turnID) {
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
	s.finishTurn(Result{Result: result}, err)
}

func (s *appServerSession) resetTurnCaptureLocked() {
	s.items = map[string]string{}
	s.deltas = map[string]string{}
	s.deltaLogged = map[string]bool{}
	s.deltaPreview = map[string]string{}
	s.deltaPreviewTruncated = map[string]bool{}
	s.activityPreview = ""
	s.activityTruncated = false
	s.agentPreviewStarted = false
	s.itemDone = map[string]bool{}
	s.itemOrder = nil
}

func (s *appServerSession) waitProcess(ctx context.Context, command string) {
	err := s.cmd.Wait()
	s.mu.Lock()
	completed := s.completed
	stdout := s.stdoutDiagnostics.String()
	stderr := s.stderr.String()
	s.mu.Unlock()
	if completed {
		return
	}
	result := executor.Result{Command: command, Stdout: stdout, Stderr: stderr, Err: err}
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

func (s *appServerSession) complete(result executor.Result, err error) {
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
	s.shutdownProcess(cancel)
	s.logRemainingAggregatedDeltas()
	s.mu.Lock()
	s.result.Result = result
	s.result.Warnings = append(s.result.Warnings, s.warnings...)
	s.err = err
	s.finishTurnLocked(Result{Result: result}, err)
	s.mu.Unlock()
	if done != nil {
		close(done)
	}
}

func (s *appServerSession) stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	s.shutdownProcess(cancel)
}

func (s *appServerSession) shutdownProcess(cancel context.CancelFunc) {
	s.shutdownOnce.Do(func() {
		s.mu.Lock()
		stdin := s.stdin
		stdout := s.stdoutPipe
		stderr := s.stderrPipe
		cmd := s.cmd
		s.stdin = nil
		s.stdoutPipe = nil
		s.stderrPipe = nil
		s.mu.Unlock()
		s.writeMu.Lock()
		if stdin != nil {
			_ = stdin.Close()
		}
		s.writeMu.Unlock()
		if cancel != nil {
			cancel()
		}
		if cmd != nil && cmd.Cancel != nil {
			if err := cmd.Cancel(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				_ = err
			}
		}
		if stdout != nil {
			_ = stdout.Close()
		}
		if stderr != nil {
			_ = stderr.Close()
		}
	})
}

func (s *appServerSession) failStart(command string, err error) {
	s.mu.Lock()
	stdout := s.stdoutDiagnostics.String()
	stderr := s.stderr.String()
	s.mu.Unlock()
	if strings.TrimSpace(stderr) == "" && err != nil {
		stderr = err.Error()
	}
	s.complete(executor.Result{Command: command, Stdout: stdout, Stderr: stderr, Err: err}, err)
}
