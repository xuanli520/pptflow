package appserver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/xuanli520/pptflow/internal/executor"
)

type appServerCodexReviewSession struct {
	mu                    sync.Mutex
	writeMu               sync.Mutex
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
	stdoutDiagnostics     bytes.Buffer
	stderr                bytes.Buffer
	completed             bool
	envKeys               []string
	warnings              []Warning
}

func (s *appServerCodexReviewSession) Start(ctx context.Context, request Request) error {
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

	if !request.HasAppServer {
		err := fmt.Errorf("codex CLI does not expose app-server; active-turn guidance requires codex app-server turn/steer")
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
		"\n\nPrompt: supplied via app-server turn/start; sha256=" + sha256Text(request.Prompt) +
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
		"clientInfo": map[string]any{"name": "pptflow", "version": "0"},
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
	turnResult, err := s.sendRequest(initCtx, "turn/start", appServerTurnStartParams(request, threadID))
	if err != nil {
		s.failStart(commandString(request.CommandPath, args), err)
		return err
	}
	turnID := stringAtPath(turnResult, "turn", "id")
	if turnID == "" {
		err := fmt.Errorf("codex app-server turn/start response missing turn.id")
		s.failStart(commandString(request.CommandPath, args), err)
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
