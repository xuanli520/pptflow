package appserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/purplevoid/harbor-factory/internal/executor"
)

type appServerSession struct {
	mu                    sync.Mutex
	writeMu               sync.Mutex
	turnMu                sync.Mutex
	req                   Request
	cmd                   *exec.Cmd
	stdin                 io.WriteCloser
	stdoutPipe            io.Closer
	stderrPipe            io.Closer
	processCtx            context.Context
	cancel                context.CancelFunc
	done                  chan struct{}
	wg                    sync.WaitGroup
	shutdownOnce          sync.Once
	result                Result
	err                   error
	turnDone              chan struct{}
	turnResult            Result
	turnErr               error
	turnContext           context.Context
	turnStartRequestID    int
	nextID                int
	responses             map[int]chan appServerRPCMessage
	dynamicTools          map[string]DynamicTool
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
	stdoutDiagnostics     bytes.Buffer
	stderr                bytes.Buffer
	completed             bool
	envKeys               []string
	warnings              []Warning
}

func (s *appServerSession) Start(ctx context.Context, request Request) error {
	dynamicTools, err := normalizeDynamicTools(request.DynamicTools)
	if err != nil {
		return err
	}
	request.DynamicTools = normalizedDynamicToolSlice(request.DynamicTools, dynamicTools)
	s.mu.Lock()
	if s.done != nil {
		s.mu.Unlock()
		return fmt.Errorf("codex app-server session already started")
	}
	s.req = request
	s.done = make(chan struct{})
	s.responses = map[int]chan appServerRPCMessage{}
	s.dynamicTools = dynamicTools
	s.items = map[string]string{}
	s.deltas = map[string]string{}
	s.deltaLogged = map[string]bool{}
	s.deltaPreview = map[string]string{}
	s.deltaPreviewTruncated = map[string]bool{}
	s.itemDone = map[string]bool{}
	s.mu.Unlock()

	if !request.HasAppServer {
		err := fmt.Errorf("codex CLI does not expose app-server; interactive agent turns require codex app-server")
		s.complete(executor.Result{Command: request.CommandPath + " app-server --listen stdio://", Err: err, Stderr: err.Error()}, err)
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
	args := []string{"app-server", "-c", `approval_policy="never"`, "-c", fmt.Sprintf(`sandbox_mode="%s"`, normalizeSandboxMode(request.SandboxMode))}
	if effort := strings.TrimSpace(request.ReasoningEffort); effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", effort))
	}
	args = append(args, "--listen", "stdio://")
	cmd := exec.CommandContext(runCtx, request.CommandPath, args...)
	executor.ConfigureCommand(cmd)
	cmd.Dir = request.ProjectPath
	if len(request.Env) > 0 {
		cmd.Env = request.Env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		s.complete(executor.Result{Command: commandString(request.CommandPath, args), Err: err, Stderr: err.Error()}, err)
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.complete(executor.Result{Command: commandString(request.CommandPath, args), Err: err, Stderr: err.Error()}, err)
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.complete(executor.Result{Command: commandString(request.CommandPath, args), Err: err, Stderr: err.Error()}, err)
		return err
	}
	preamble := commandString(request.CommandPath, args) +
		"\n\nConversation: one ephemeral app-server thread with caller-driven turns" +
		"\nCodex capability: " + request.CapabilitySummary +
		"\nCodex env keys: " + strings.Join(s.envKeys, ",") +
		"\nSandbox mode: " + normalizeSandboxMode(request.SandboxMode) +
		"\nSandbox policy: " + normalizeSandboxPolicy(request.SandboxPolicy) +
		"\nNetwork access: " + fmt.Sprint(request.NetworkAccess) +
		"\nTimeout: " + request.Timeout.String() +
		"\nStarted: " + time.Now().UTC().Format(time.RFC3339) +
		"\n\n=== codex app-server JSON-RPC compact event log start ===\n"
	if err := writeText(request.LogPath, preamble); err != nil {
		s.addWarning(newWarning(request.LogPath, "write_text", false, err))
	}
	if err := cmd.Start(); err != nil {
		s.complete(executor.Result{Command: commandString(request.CommandPath, args), Err: err, Stderr: err.Error()}, err)
		return err
	}
	s.mu.Lock()
	s.cmd = cmd
	s.stdin = stdin
	s.stdoutPipe = stdout
	s.stderrPipe = stderr
	s.wg.Add(3)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		s.readStdout(stdout)
	}()
	go func() {
		defer s.wg.Done()
		s.readStderr(stderr)
	}()
	go func() {
		defer s.wg.Done()
		s.waitProcess(runCtx, commandString(request.CommandPath, args))
	}()

	initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := s.sendRequest(initCtx, "initialize", map[string]any{
		"clientInfo": appServerClientInfo(request),
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	}); err != nil {
		s.failStart(commandString(request.CommandPath, args), err)
		return err
	}
	if err := s.sendNotification(initCtx, "initialized", nil); err != nil {
		s.failStart(commandString(request.CommandPath, args), err)
		return err
	}
	threadResult, err := s.sendRequest(initCtx, "thread/start", appServerThreadStartParams(request))
	if err != nil {
		s.failStart(commandString(request.CommandPath, args), err)
		return err
	}
	threadID := stringAtPath(threadResult, "thread", "id")
	if threadID == "" {
		err := fmt.Errorf("codex app-server thread/start response missing thread.id")
		s.failStart(commandString(request.CommandPath, args), err)
		return err
	}
	s.mu.Lock()
	s.threadID = threadID
	s.mu.Unlock()
	s.appendLog(fmt.Sprintf("Codex app-server thread=%s ready\n", threadID))
	return nil
}

func (s *appServerSession) Turn(ctx context.Context, request TurnRequest) (Result, error) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	var outputSchema json.RawMessage
	if len(request.OutputSchema) > 0 {
		var err error
		outputSchema, err = normalizeJSONSchema(request.OutputSchema)
		if err != nil {
			return Result{}, fmt.Errorf("codex app-server turn output schema: %w", err)
		}
	}

	s.mu.Lock()
	if s.done == nil || s.threadID == "" {
		s.mu.Unlock()
		return Result{}, fmt.Errorf("codex app-server conversation is not started")
	}
	if s.completed {
		err := s.err
		s.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("codex app-server conversation is closed")
		}
		return Result{}, err
	}
	turnRequest := s.req
	turnRequest.Prompt = request.Prompt
	turnRequest.Input = append([]InputPart(nil), request.Input...)
	if strings.TrimSpace(request.LogPath) != "" {
		turnRequest.LogPath = request.LogPath
	}
	if request.MaxOutputBytes > 0 {
		turnRequest.MaxOutputBytes = request.MaxOutputBytes
	}
	turnRequest.OnDelta = request.OnDelta
	s.req = turnRequest
	s.turnID = ""
	s.turnDone = make(chan struct{})
	s.turnResult = Result{}
	s.turnErr = nil
	s.stdoutDiagnostics.Reset()
	s.stderr.Reset()
	s.resetTurnCaptureLocked()
	turnDone := s.turnDone
	processDone := s.done
	threadID := s.threadID
	s.mu.Unlock()

	s.appendLog("\n=== codex app-server turn requested ===\n" +
		time.Now().UTC().Format(time.RFC3339) +
		"\nprompt_sha256=" + sha256Text(request.Prompt) + "\n")

	turnCtx := ctx
	cancel := func() {}
	if request.Timeout > 0 {
		turnCtx, cancel = context.WithTimeout(ctx, request.Timeout)
	}
	defer cancel()
	s.mu.Lock()
	if s.turnDone == turnDone {
		s.turnContext = turnCtx
	}
	s.mu.Unlock()
	startCtx, startCancel := context.WithTimeout(turnCtx, 30*time.Second)
	turnStartResult, err := s.sendRequest(startCtx, "turn/start", appServerTurnStartParamsWithOutputSchema(turnRequest, threadID, outputSchema))
	startCancel()
	if err != nil {
		s.finishTurn(Result{}, fmt.Errorf("start Codex turn: %w", err))
	} else {
		turnID := stringAtPath(turnStartResult, "turn", "id")
		if turnID == "" {
			s.finishTurn(Result{}, fmt.Errorf("codex app-server turn/start response missing turn.id"))
		} else {
			s.mu.Lock()
			if s.turnDone == turnDone {
				s.turnID = turnID
			}
			s.mu.Unlock()
			s.appendLog(fmt.Sprintf("Codex app-server thread=%s turn=%s\n", threadID, turnID))
		}
	}

	select {
	case <-turnDone:
	case <-processDone:
	case <-turnCtx.Done():
		s.stop()
		<-processDone
	}
	s.mu.Lock()
	result := s.turnResult
	turnErr := s.turnErr
	if turnErr == nil && turnCtx.Err() != nil {
		turnErr = turnCtx.Err()
	}
	if s.turnDone == turnDone {
		s.turnDone = nil
	}
	if s.turnContext == turnCtx {
		s.turnContext = nil
	}
	s.turnID = ""
	s.turnStartRequestID = 0
	s.mu.Unlock()
	return result, turnErr
}

func (s *appServerSession) finishTurn(result Result, err error) {
	s.mu.Lock()
	s.finishTurnLocked(result, err)
	s.mu.Unlock()
}

func (s *appServerSession) finishTurnLocked(result Result, err error) {
	if s.turnDone == nil {
		return
	}
	result.Warnings = append(result.Warnings, s.warnings...)
	s.warnings = nil
	s.turnResult = result
	s.turnErr = err
	s.turnID = ""
	s.turnStartRequestID = 0
	done := s.turnDone
	s.turnDone = nil
	close(done)
}

func (s *appServerSession) Close() error {
	s.mu.Lock()
	done := s.done
	completed := s.completed
	s.mu.Unlock()
	if done == nil {
		return nil
	}
	if !completed {
		s.stop()
		<-done
	}
	s.wg.Wait()
	return nil
}

func (s *appServerSession) SendGuidance(ctx context.Context, message string) error {
	s.mu.Lock()
	done := s.done
	threadID := s.threadID
	turnID := s.turnID
	s.mu.Unlock()
	if done == nil {
		return fmt.Errorf("codex app-server session is not started")
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
		"input":          appServerInput(Request{Prompt: message}),
		"threadId":       threadID,
	})
	if err != nil {
		s.appendLog("Codex app-server turn/steer failed: " + err.Error() + "\n")
		return err
	}
	s.appendLog("Codex app-server turn/steer accepted.\n")
	return nil
}
