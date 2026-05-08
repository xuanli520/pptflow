package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/codex"
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

func (r Runner) runCodexReviewWithLog(ctx context.Context, timeout time.Duration, projectPath, logPath string, env []string, prompt string, capability codex.Capability, args []string) CodexReviewResult {
	schedule := codexGuidanceSchedule(timeout, codexStageFromPrompt(prompt))
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
	session := newAppServerCodexReviewSession(configuredEnvKeys(r.cfg.Codex.Env))
	result, err := runCodexReviewSessionWithGuidance(ctx, session, request, schedule)
	if err != nil && result.Result.Err == nil {
		result.Result = executor.Result{
			Command: capability.Path + " app-server --listen stdio://",
			Stderr:  err.Error(),
			Err:     err,
		}
	}
	appendCodexGuidanceEvents(logPath, result.GuidanceEvents)
	return result
}

func newAppServerCodexReviewSession(envKeys []string) CodexReviewSession {
	return &appServerCodexReviewSession{envKeys: envKeys}
}

func codexStageFromPrompt(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 4 && strings.EqualFold(fields[0], "Run") && strings.EqualFold(fields[1], "p2r") && strings.EqualFold(fields[2], "stage") {
			return strings.ToUpper(strings.Trim(fields[3], ".:;"))
		}
	}
	return ""
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
	finish := func(outcome waitResult) (CodexReviewResult, error) {
		outcome.result.GuidanceEvents = append(outcome.result.GuidanceEvents, events...)
		return outcome.result, outcome.err
	}
	finishAfterCancel := func() (CodexReviewResult, error) {
		select {
		case outcome := <-waitCh:
			return finish(outcome)
		default:
		}
		cancelErr := ctx.Err()
		return finish(waitResult{result: CodexReviewResult{Result: executor.Result{Err: cancelErr}}, err: cancelErr})
	}
	for _, deadline := range deadlines {
		wait := time.Until(start.Add(deadline.After))
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case outcome := <-waitCh:
			timer.Stop()
			return finish(outcome)
		case <-ctx.Done():
			timer.Stop()
			return finishAfterCancel()
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
	select {
	case outcome := <-waitCh:
		return finish(outcome)
	case <-ctx.Done():
		return finishAfterCancel()
	}
}

func codexGuidanceSchedule(timeout time.Duration, stage string) []CodexGuidanceDeadline {
	var deadlines []CodexGuidanceDeadline
	for _, deadline := range defaultCodexGuidanceDeadlines {
		if timeout > 0 && deadline.After >= timeout {
			continue
		}
		deadline.Message = codexGuidanceMessageWithContract(deadline.Message, stage)
		deadlines = append(deadlines, deadline)
	}
	return deadlines
}

func codexGuidanceMessageWithContract(message, stage string) string {
	stage = strings.ToUpper(strings.TrimSpace(stage))
	if stage == "" {
		stage = "<stage from the original p2r prompt>"
	}
	return fmt.Sprintf(`%s

Final response format is still mandatory. Return a complete Markdown review report and include exactly one static-review JSON contract block:
%s
{
  "schema_version": "%s",
  "stage": "%s",
  "findings": []
}
%s

Replace findings with confirmed findings when present; use findings: [] only when none are confirmed. Do not return a prose-only summary.`, strings.TrimSpace(message), staticReviewJSONStart, staticReviewSchemaVersion, stage, staticReviewJSONEnd)
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

func sha256Text(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
