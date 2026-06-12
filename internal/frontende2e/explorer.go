package frontende2e

import (
	"context"
	"fmt"
	"time"

	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

const (
	MaxActions               = 30
	MaxInvalidActions        = 3
	MinBrowserScreenshots    = 5
	MaxBrowserScreenshots    = 10
	AuthGateSubmitStallLimit = 2
	RepeatedStateStallLimit  = 7
)

type ExplorerState string

const (
	ExplorerStateStop        ExplorerState = "stop"
	ExplorerStatePlannerTurn ExplorerState = "planner_turn"
)

type ExplorerDecision struct {
	State         ExplorerState
	Reason        string
	Action        BrowserAction
	ActionTimeout time.Duration
	TurnTimeout   time.Duration
}

type PlannerFunc func(ctx context.Context, candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction, round int, timeout time.Duration) (string, []model.ArtifactWarning, error)

type ActionRunnerFunc func(ctx context.Context, action browserpkg.Action, policy browserpkg.Policy, timeout time.Duration) (browserpkg.Observation, error)

type ExplorerPolicy struct {
	ShouldCaptureActionScreenshot    func(BrowserAction, []browserpkg.Observation) bool
	RuntimeScreenshotPath            func(round int, action string) string
	PlannerTimedOut                  func(context.Context, error) bool
	ObservationStopReason            func([]browserpkg.Observation) string
	FinishSummaryEvidenceBlockReason func(FrontendE2ESummary, []browserpkg.Observation) string
	FinishScreenshotBlockReason      func(FrontendE2ESummary, []browserpkg.Observation) string
	Summary                          func(status, reason string, candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction) FrontendE2ESummary
	LogPlannedAction                 func(round int, action BrowserAction) string
	LogObservation                   func(round int, observation browserpkg.Observation) string
	SchemaFailureFinding             func(sourcePath string, err error) model.Finding
	SummaryFindings                  func(summary FrontendE2ESummary, sourcePath string) []model.Finding
	IncludeActionFailureFallback     func(summary FrontendE2ESummary, summaryFindings []model.Finding) bool
	ObservationFindings              func(observations []browserpkg.Observation, screenshot string, includeActionFailures bool) []model.Finding
}

type ExplorerEvents struct {
	AppendLog         func(string)
	WriteObservations func([]browserpkg.Observation)
	RecordWarnings    func([]model.ArtifactWarning)
	Progress          func(round int, action string, ok bool)
}

type ExplorerFinishers struct {
	EvidenceVerdict func(observations []browserpkg.Observation, blocked []BlockedBrowserAction, reason string) (model.StageRecord, bool)
	Partial         func(observations []browserpkg.Observation, blocked []BlockedBrowserAction, reason string) model.StageRecord
	Unavailable     func(reason, status string, summary FrontendE2ESummary, observations []browserpkg.Observation, findings []model.Finding) model.StageRecord
	AcceptedSummary func(summary FrontendE2ESummary, observations []browserpkg.Observation, summaryFindings []model.Finding, observationFindings []model.Finding) model.StageRecord
}

type Explorer struct {
	Ctx            context.Context
	Candidates     []BrowserURLCandidate
	Policy         browserpkg.Policy
	Deadline       time.Time
	SummaryPath    string
	ScreenshotPath string
	LogPath        string
	Planner        PlannerFunc
	ActionRunner   ActionRunnerFunc
	Rules          ExplorerPolicy
	Events         ExplorerEvents
	Finishers      ExplorerFinishers

	observations []browserpkg.Observation
	blocked      []BlockedBrowserAction
	invalidCount int
}

func (e *Explorer) Run() model.StageRecord {
	for round := 1; round <= MaxActions; round++ {
		decision := e.NextDecision()
		switch decision.State {
		case ExplorerStateStop:
			return e.finishPartial(decision.Reason)
		case ExplorerStatePlannerTurn:
			if finished, record := e.runPlannerTurn(round, decision.TurnTimeout); finished {
				return record
			}
		}
	}
	return e.finishPartial("Stage G reached the maximum browser action count.")
}

func (e *Explorer) NextDecision() ExplorerDecision {
	turnTimeout := time.Until(e.Deadline)
	if turnTimeout <= 0 || e.Ctx.Err() != nil {
		return ExplorerDecision{State: ExplorerStateStop, Reason: "Stage G timeout reached."}
	}
	if turnTimeout < 30*time.Second {
		return ExplorerDecision{State: ExplorerStateStop, Reason: "Stage G timeout reached before another browser planning turn could complete."}
	}
	if turnTimeout > 120*time.Second {
		turnTimeout = 120 * time.Second
	}
	return ExplorerDecision{State: ExplorerStatePlannerTurn, TurnTimeout: turnTimeout}
}

func BrowserActionTimeout(turnTimeout time.Duration) time.Duration {
	actionTimeout := 45 * time.Second
	if turnTimeout-2*time.Second < actionTimeout {
		actionTimeout = turnTimeout - 2*time.Second
	}
	return actionTimeout
}

func (e *Explorer) executeBrowserAction(round int, planned BrowserAction, timeout time.Duration) (bool, model.StageRecord) {
	e.appendLog(e.Rules.LogPlannedAction(round, planned))
	observation, err := e.executeAction(round, planned, timeout)
	if err != nil {
		observation = actionErrorObservation(planned, err)
	}
	e.recordObservation(round, planned.Action, observation)
	if reason := e.Rules.ObservationStopReason(e.observations); reason != "" {
		return true, e.finishPartial(reason)
	}
	return false, model.StageRecord{}
}

func (e *Explorer) runPlannerTurn(round int, turnTimeout time.Duration) (bool, model.StageRecord) {
	rawAction, warnings, err := e.Planner(e.Ctx, e.Candidates, e.observations, e.blocked, round, turnTimeout)
	e.recordWarnings(warnings)
	if err != nil {
		if e.Rules.PlannerTimedOut(e.Ctx, err) {
			return true, e.finishPartial("Stage G timeout reached before Codex browser planner returned.")
		}
		if finished, ok := e.finishEvidenceVerdict("Codex browser planner failed after sufficient validated browser evidence"); ok {
			return true, finished
		}
		finding := model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      "Stage G Codex browser planner failed",
			Rule:       "Stage G requires Codex to return a validated browser action JSON.",
			Evidence:   err.Error(),
			Impact:     "Browser E2E exploration stopped before completion.",
			MinimumFix: "Inspect the Stage G Codex round log and rerun Stage G.",
			SourcePath: e.LogPath,
		}
		return true, e.finishUnavailable("Codex browser planner failed", model.StageFailed, e.Rules.Summary("failed", "Codex browser planner failed", e.Candidates, e.observations, e.blocked), e.observations, []model.Finding{finding})
	}
	validation := parseBrowserAction(rawAction, e.Candidates)
	if validation.Blocked != nil {
		return e.handleBlockedPlannerAction(round, validation)
	}
	if validation.Action.Action == "finish" {
		return e.handleFinishAction(round, rawAction, validation)
	}
	if _, err := BrowserActionForWrapper(validation.Action, e.Candidates); err != nil {
		e.blocked = append(e.blocked, BlockedBrowserAction{Action: validation.Action.Action, Reason: err.Error(), Risk: string(validation.Risk), Raw: rawAction})
		return false, model.StageRecord{}
	}
	e.invalidCount = 0
	return e.executeBrowserAction(round, validation.Action, 45*time.Second)
}

func (e *Explorer) handleBlockedPlannerAction(round int, validation browserActionValidation) (bool, model.StageRecord) {
	e.blocked = append(e.blocked, *validation.Blocked)
	e.invalidCount++
	e.appendLog(fmt.Sprintf("blocked action round %d: %s\n", round, validation.Blocked.Reason))
	if finished, ok := e.finishEvidenceVerdict("Stage G completed from collected browser evidence after planner returned an invalid action."); ok {
		return true, finished
	}
	if e.invalidCount > MaxInvalidActions {
		finding := model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      "Stage G received too many invalid browser actions",
			Rule:       "Codex browser planning must stay within the p2r action policy.",
			Evidence:   validation.Blocked.Reason,
			Impact:     "Browser E2E exploration stopped before completion.",
			MinimumFix: "Rerun Stage G after tightening the browser action prompt or inspect the planner output.",
			SourcePath: e.LogPath,
		}
		return true, e.finishUnavailable("too many invalid actions", model.StageFailed, e.Rules.Summary("blocked", "too many invalid actions", e.Candidates, e.observations, e.blocked), e.observations, []model.Finding{finding})
	}
	return false, model.StageRecord{}
}

func (e *Explorer) handleFinishAction(round int, rawAction string, validation browserActionValidation) (bool, model.StageRecord) {
	summary, err := parseFrontendE2ESummary(validation.Action.Summary)
	if err != nil {
		if finished, ok := e.finishEvidenceVerdict("Stage G completed from collected browser evidence after planner returned an invalid finish summary."); ok {
			return true, finished
		}
		return true, e.finishUnavailable("frontend E2E summary schema invalid", model.StageFailed, e.Rules.Summary("failed", "frontend E2E summary schema invalid", e.Candidates, e.observations, e.blocked), e.observations, []model.Finding{e.Rules.SchemaFailureFinding(e.SummaryPath, err)})
	}
	if summary.Status == "passed" {
		return e.handlePassedFinish(round, rawAction, validation)
	}
	if reason := e.Rules.FinishSummaryEvidenceBlockReason(summary, e.observations); reason != "" {
		return e.blockUnsupportedFinish(round, rawAction, validation, reason, "Stage G received too many unsupported finish summaries", "Stage G failed, partial, and blocked summaries must be backed by browser observations.", "too many unsupported finish summaries", "Continue collecting concrete browser failure evidence before returning a non-passed summary.")
	}
	e.invalidCount = 0
	if reason := e.Rules.FinishScreenshotBlockReason(summary, e.observations); reason != "" {
		e.blocked = append(e.blocked, BlockedBrowserAction{Action: validation.Action.Action, Reason: reason, Risk: string(validation.Risk), Raw: rawAction})
		e.appendLog(fmt.Sprintf("blocked action round %d: %s\n", round, reason))
		return false, model.StageRecord{}
	}
	summary.URLCandidates = e.Candidates
	summary.BlockedActions = e.blocked
	summaryFindings := e.Rules.SummaryFindings(summary, e.SummaryPath)
	includeActionFailures := e.Rules.IncludeActionFailureFallback(summary, summaryFindings)
	observationFindings := e.Rules.ObservationFindings(e.observations, e.ScreenshotPath, includeActionFailures)
	return true, e.Finishers.AcceptedSummary(summary, e.observations, summaryFindings, observationFindings)
}

func (e *Explorer) handlePassedFinish(round int, rawAction string, validation browserActionValidation) (bool, model.StageRecord) {
	if finished, ok := e.finishEvidenceVerdict("Stage G completed from validated browser evidence after planner requested pass."); ok {
		return true, finished
	}
	if reason := e.Rules.ObservationStopReason(e.observations); reason != "" {
		return true, e.finishPartial(reason)
	}
	reason := "planner passed finish requires validated Stage G evidence"
	return e.blockUnsupportedFinish(round, rawAction, validation, reason, "Stage G received too many unsupported pass summaries", "Stage G pass status must be backed by validated browser evidence.", "too many unsupported pass summaries", "Continue collecting authenticated product workflow evidence before returning a passed summary.")
}

func (e *Explorer) blockUnsupportedFinish(round int, rawAction string, validation browserActionValidation, reason, title, rule, finishReason, minimumFix string) (bool, model.StageRecord) {
	e.blocked = append(e.blocked, BlockedBrowserAction{Action: validation.Action.Action, Reason: reason, Risk: string(validation.Risk), Raw: rawAction})
	e.invalidCount++
	e.appendLog(fmt.Sprintf("blocked action round %d: %s\n", round, reason))
	if e.invalidCount > MaxInvalidActions {
		finding := model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      title,
			Rule:       rule,
			Evidence:   reason,
			Impact:     "Browser E2E exploration stopped before completion.",
			MinimumFix: minimumFix,
			SourcePath: e.LogPath,
		}
		return true, e.finishUnavailable(finishReason, model.StageFailed, e.Rules.Summary("blocked", finishReason, e.Candidates, e.observations, e.blocked), e.observations, []model.Finding{finding})
	}
	return false, model.StageRecord{}
}

func (e *Explorer) executeAction(round int, planned BrowserAction, timeout time.Duration) (browserpkg.Observation, error) {
	action, err := BrowserActionForWrapper(planned, e.Candidates)
	if err != nil {
		return browserpkg.Observation{}, err
	}
	actionPolicy := e.Policy
	if e.Rules.ShouldCaptureActionScreenshot(planned, e.observations) {
		actionPolicy.ScreenshotPath = e.Rules.RuntimeScreenshotPath(round, action.Name)
	} else {
		actionPolicy.DisableScreenshot = true
	}
	return e.ActionRunner(e.Ctx, action, actionPolicy, timeout)
}

func actionErrorObservation(action BrowserAction, err error) browserpkg.Observation {
	message := ""
	if err != nil {
		message = err.Error()
	}
	return browserpkg.Observation{
		Action: action.Action,
		OK:     false,
		Error:  message,
		Metadata: map[string]string{
			"p2r_error_kind": "browser_action_runner_error",
			"p2r_reason":     action.Reason,
		},
	}
}

func (e *Explorer) recordObservation(round int, actionName string, observation browserpkg.Observation) {
	e.observations = append(e.observations, observation)
	if e.Events.WriteObservations != nil {
		e.Events.WriteObservations(e.observations)
	}
	e.appendLog(e.Rules.LogObservation(round, observation))
	if e.Events.Progress != nil {
		e.Events.Progress(round, actionName, observation.OK)
	}
}

func (e *Explorer) finishEvidenceVerdict(reason string) (model.StageRecord, bool) {
	return e.Finishers.EvidenceVerdict(e.observations, e.blocked, reason)
}

func (e *Explorer) finishPartial(reason string) model.StageRecord {
	return e.Finishers.Partial(e.observations, e.blocked, reason)
}

func (e *Explorer) finishUnavailable(reason, status string, summary FrontendE2ESummary, observations []browserpkg.Observation, findings []model.Finding) model.StageRecord {
	return e.Finishers.Unavailable(reason, status, summary, observations, findings)
}

func (e *Explorer) appendLog(content string) {
	if e.Events.AppendLog != nil {
		e.Events.AppendLog(content)
	}
}

func (e *Explorer) recordWarnings(warnings []model.ArtifactWarning) {
	if e.Events.RecordWarnings != nil {
		e.Events.RecordWarnings(warnings)
	}
}
