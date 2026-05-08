package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/xuanli520/p2r_tui/internal/codex"
	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/executor"
)

type CodexReviewRequest struct {
	Timeout        time.Duration
	ProjectPath    string
	LogPath        string
	Env            []string
	Prompt         string
	Capability     codex.Capability
	Args           []string
	MaxOutputBytes int
}

type CodexReviewResult struct {
	Result         executor.Result
	GuidanceEvents []CodexGuidanceEvent
}

type CodexGuidanceEvent struct {
	Label   string `json:"label"`
	Message string `json:"message"`
	SentAt  string `json:"sent_at"`
	Error   string `json:"error,omitempty"`
}

type CodexGuidanceDeadline struct {
	Label   string
	After   time.Duration
	Message string
}

type CodexReviewSession interface {
	Start(ctx context.Context, request CodexReviewRequest) error
	SendGuidance(ctx context.Context, message string) error
	Wait(ctx context.Context) (CodexReviewResult, error)
}

var defaultCodexGuidanceDeadlines = []CodexGuidanceDeadline{
	{
		Label:   "20m guidance sent",
		After:   20 * time.Minute,
		Message: "You have been running for 20 minutes without a final result. Please accelerate, focus on the highest-risk review points, and prioritize confirmed findings and the final conclusion.",
	},
	{
		Label:   "30m deadline guidance sent",
		After:   30 * time.Minute,
		Message: "You have been running for 30 minutes without a final result. Please complete the review and return the final response within the next 10 minutes. Avoid expanding the review scope.",
	},
	{
		Label:   "40m final-summary guidance sent",
		After:   40 * time.Minute,
		Message: "You have been running for 40 minutes. Stop starting new exploration, summarize the conclusions already confirmed, and return the final review response now. p2r will persist your final response to the required artifact files.",
	},
}

type execCodexReviewSession struct {
	exec      executor.Runner
	cfg       config.Config
	mu        sync.Mutex
	req       CodexReviewRequest
	done      chan struct{}
	result    CodexReviewResult
	err       error
	sessionID string
}

func (s *execCodexReviewSession) Start(ctx context.Context, request CodexReviewRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done != nil {
		return fmt.Errorf("codex review session already started")
	}
	s.req = request
	s.done = make(chan struct{})
	go s.run(ctx, request)
	return nil
}

func (s *execCodexReviewSession) SendGuidance(ctx context.Context, message string) error {
	s.mu.Lock()
	req := s.req
	done := s.done
	sessionID := s.sessionID
	s.mu.Unlock()
	if done == nil {
		return fmt.Errorf("codex review session is not started")
	}
	select {
	case <-done:
		return nil
	default:
	}
	s.appendLog(fmt.Sprintf("\n=== codex guidance message requested ===\n%s\n%s\n", time.Now().UTC().Format(time.RFC3339), message))
	if !req.Capability.HasResume {
		s.appendLog("Codex resume guidance channel is unavailable; recorded guidance event without interrupting the running exec process.\n")
		return nil
	}
	args := []string{"exec", "resume"}
	if req.Capability.HasSkipGitRepoCheck {
		args = append(args, "--skip-git-repo-check")
	}
	if sessionID != "" {
		args = append(args, sessionID)
	} else {
		args = append(args, "--last")
	}
	args = append(args, message)
	resume := s.exec.Run(ctx, 30*time.Second, req.ProjectPath, req.Env, req.Capability.Path, args...)
	s.appendLog(fmt.Sprintf("Guidance delivery command: %s\nGuidance delivery exit=%d timeout=%t err=%v\nSTDOUT:\n%s\nSTDERR:\n%s\n",
		resume.Command,
		resume.ExitCode,
		resume.Timeout,
		resume.Err,
		truncateString(resume.Stdout, req.MaxOutputBytes),
		truncateString(resume.Stderr, req.MaxOutputBytes),
	))
	return resume.Err
}

func (s *execCodexReviewSession) Wait(ctx context.Context) (CodexReviewResult, error) {
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done == nil {
		return CodexReviewResult{}, fmt.Errorf("codex review session is not started")
	}
	select {
	case <-done:
	case <-ctx.Done():
		<-done
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, s.err
}

func (s *execCodexReviewSession) run(ctx context.Context, request CodexReviewRequest) {
	result := s.runCommand(ctx, request)
	s.mu.Lock()
	s.result.Result = result
	close(s.done)
	s.mu.Unlock()
}

func (s *execCodexReviewSession) runCommand(ctx context.Context, request CodexReviewRequest) executor.Result {
	commandText := strings.Join(append([]string{request.Capability.Path}, request.Args...), " ")
	preamble := commandText +
		"\n\nPrompt: supplied via stdin; sha256=" + sha256Text(request.Prompt) +
		"\nCodex capability: " + capabilitySummary(request.Capability) +
		"\nCodex env keys: " + strings.Join(configuredEnvKeys(s.cfg.Codex.Env), ",") +
		"\nTimeout: " + request.Timeout.String() +
		"\nStarted: " + time.Now().UTC().Format(time.RFC3339) +
		"\n\n=== codex stream start ===\n"
	_ = writeText(request.LogPath, preamble)
	logFile, err := os.OpenFile(request.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return s.exec.RunWithInput(ctx, request.Timeout, request.ProjectPath, request.Env, strings.NewReader(request.Prompt), request.Capability.Path, request.Args...)
	}
	defer logFile.Close()
	streamLog := &codexSessionLogWriter{writer: logFile, onSessionID: s.setSessionID}
	result := s.exec.RunWithInputStreaming(ctx, request.Timeout, request.ProjectPath, request.Env, strings.NewReader(request.Prompt), streamLog, request.Capability.Path, request.Args...)
	fmt.Fprintf(logFile, "\n=== codex stream end: exit=%d timeout=%t err=%v ===\n", result.ExitCode, result.Timeout, result.Err)
	fmt.Fprintf(logFile, "\n=== captured stdout/stderr tail ===\nSTDOUT:\n%s\nSTDERR:\n%s\n", truncateString(result.Stdout, request.MaxOutputBytes), truncateString(result.Stderr, request.MaxOutputBytes))
	return result
}

func (s *execCodexReviewSession) appendLog(content string) {
	s.mu.Lock()
	path := s.req.LogPath
	s.mu.Unlock()
	if strings.TrimSpace(path) == "" {
		_, _ = io.WriteString(os.Stderr, content)
		return
	}
	_ = appendText(path, content)
}

func (s *execCodexReviewSession) setSessionID(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	if s.sessionID == "" {
		s.sessionID = sessionID
	}
	s.mu.Unlock()
}

func (r Runner) runCodexReviewWithLog(ctx context.Context, timeout time.Duration, projectPath, logPath string, env []string, prompt string, capability codex.Capability, args []string) CodexReviewResult {
	schedule := codexGuidanceSchedule(timeout)
	if len(schedule) > 0 && capability.HasResume {
		args = removeCodexArg(args, "--ephemeral")
	}
	request := CodexReviewRequest{
		Timeout:        timeout,
		ProjectPath:    projectPath,
		LogPath:        logPath,
		Env:            env,
		Prompt:         prompt,
		Capability:     capability,
		Args:           args,
		MaxOutputBytes: r.cfg.Codex.MaxOutputBytes,
	}
	session := &execCodexReviewSession{exec: r.exec, cfg: r.cfg}
	result, _ := runCodexReviewSessionWithGuidance(ctx, session, request, schedule)
	appendCodexGuidanceEvents(logPath, result.GuidanceEvents)
	return result
}

func runCodexReviewSessionWithGuidance(ctx context.Context, session CodexReviewSession, request CodexReviewRequest, deadlines []CodexGuidanceDeadline) (CodexReviewResult, error) {
	if err := session.Start(ctx, request); err != nil {
		return CodexReviewResult{}, err
	}
	type waitResult struct {
		result CodexReviewResult
		err    error
	}
	waitCh := make(chan waitResult, 1)
	go func() {
		result, err := session.Wait(ctx)
		waitCh <- waitResult{result: result, err: err}
	}()

	start := time.Now()
	var events []CodexGuidanceEvent
	for _, deadline := range deadlines {
		wait := time.Until(start.Add(deadline.After))
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case outcome := <-waitCh:
			timer.Stop()
			outcome.result.GuidanceEvents = append(outcome.result.GuidanceEvents, events...)
			return outcome.result, outcome.err
		case <-timer.C:
			if ctx.Err() != nil {
				continue
			}
			err := session.SendGuidance(ctx, deadline.Message)
			event := CodexGuidanceEvent{
				Label:   deadline.Label,
				Message: deadline.Message,
				SentAt:  time.Now().UTC().Format(time.RFC3339),
			}
			if err != nil {
				event.Error = err.Error()
			}
			events = append(events, event)
		}
	}
	outcome := <-waitCh
	outcome.result.GuidanceEvents = append(outcome.result.GuidanceEvents, events...)
	return outcome.result, outcome.err
}

func codexGuidanceSchedule(timeout time.Duration) []CodexGuidanceDeadline {
	var deadlines []CodexGuidanceDeadline
	for _, deadline := range defaultCodexGuidanceDeadlines {
		if timeout > 0 && deadline.After >= timeout {
			continue
		}
		deadlines = append(deadlines, deadline)
	}
	return deadlines
}

func appendCodexGuidanceEvents(logPath string, events []CodexGuidanceEvent) {
	if len(events) == 0 {
		return
	}
	var builder strings.Builder
	builder.WriteString("\n=== codex guidance events ===\n")
	for _, event := range events {
		builder.WriteString(event.Label)
		if event.SentAt != "" {
			builder.WriteString(" at ")
			builder.WriteString(event.SentAt)
		}
		if event.Error != "" {
			builder.WriteString(" delivery_error=")
			builder.WriteString(event.Error)
		}
		builder.WriteString("\n")
	}
	_ = appendText(logPath, builder.String())
}

func removeCodexArg(args []string, remove string) []string {
	var result []string
	for _, arg := range args {
		if arg == remove {
			continue
		}
		result = append(result, arg)
	}
	return result
}

type codexSessionLogWriter struct {
	writer      io.Writer
	onSessionID func(string)
	mu          sync.Mutex
	partial     string
}

func (w *codexSessionLogWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.capture(string(p))
	return n, err
}

func (w *codexSessionLogWriter) capture(chunk string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	text := w.partial + chunk
	lines := strings.Split(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		w.partial = lines[len(lines)-1]
		lines = lines[:len(lines)-1]
	} else {
		w.partial = ""
	}
	for _, line := range lines {
		if sessionID := sessionIDFromJSONLine(line); sessionID != "" {
			w.onSessionID(sessionID)
		}
	}
}

func sessionIDFromJSONLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "{") {
		return ""
	}
	var payload map[string]any
	if json.Unmarshal([]byte(line), &payload) != nil {
		return ""
	}
	return sessionIDFromMap(payload, 0)
}

func sessionIDFromMap(payload map[string]any, depth int) string {
	for _, key := range []string{"session_id", "sessionId", "conversation_id", "conversationId", "thread_id", "threadId"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	if depth >= 2 {
		return ""
	}
	for _, value := range payload {
		if nested, ok := value.(map[string]any); ok {
			if id := sessionIDFromMap(nested, depth+1); id != "" {
				return id
			}
		}
	}
	return ""
}

func sha256Text(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
