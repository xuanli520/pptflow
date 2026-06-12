package frontende2e

import (
	"encoding/json"
	"fmt"
	"strings"

	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
)

type BrowserActionRisk string

const (
	BrowserRiskReadOnly      BrowserActionRisk = "read_only"
	BrowserRiskNavigation    BrowserActionRisk = "navigation"
	BrowserRiskLocalStateful BrowserActionRisk = "local_stateful"
	BrowserRiskDestructive   BrowserActionRisk = "destructive"
)

type BrowserAction struct {
	Action     string          `json:"action"`
	Reason     string          `json:"reason"`
	URLID      string          `json:"url_id,omitempty"`
	URL        string          `json:"url,omitempty"`
	Selector   string          `json:"selector,omitempty"`
	Text       string          `json:"text,omitempty"`
	Value      string          `json:"value,omitempty"`
	OutputPath string          `json:"output_path,omitempty"`
	Summary    json.RawMessage `json:"summary,omitempty"`
}

type BlockedBrowserAction struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
	Risk   string `json:"risk,omitempty"`
	Raw    string `json:"raw,omitempty"`
}

type browserActionValidation struct {
	Action  BrowserAction
	Risk    BrowserActionRisk
	Blocked *BlockedBrowserAction
}

type BrowserActionValidation = browserActionValidation

var browserActionRisks = map[string]BrowserActionRisk{
	"open_candidate":    BrowserRiskNavigation,
	"wait":              BrowserRiskReadOnly,
	"snapshot":          BrowserRiskReadOnly,
	"collect_console":   BrowserRiskReadOnly,
	"collect_network":   BrowserRiskReadOnly,
	"click_navigation":  BrowserRiskNavigation,
	"click_button":      BrowserRiskLocalStateful,
	"fill_input":        BrowserRiskLocalStateful,
	"submit_local_form": BrowserRiskLocalStateful,
	"go_back":           BrowserRiskNavigation,
	"finish":            BrowserRiskReadOnly,
	"delete_storage":    BrowserRiskDestructive,
	"confirm_delete":    BrowserRiskDestructive,
}

func parseBrowserAction(raw string, candidates []BrowserURLCandidate) browserActionValidation {
	var action BrowserAction
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &action); err != nil {
		return invalidBrowserAction("", "invalid action JSON: "+err.Error(), "", raw)
	}
	return validateBrowserAction(action, candidates, raw)
}

func ParseBrowserAction(raw string, candidates []BrowserURLCandidate) BrowserActionValidation {
	return parseBrowserAction(raw, candidates)
}

func validateBrowserAction(action BrowserAction, candidates []BrowserURLCandidate, raw string) browserActionValidation {
	action.Action = strings.TrimSpace(action.Action)
	action.Reason = strings.TrimSpace(action.Reason)
	action.URLID = strings.TrimSpace(action.URLID)
	action.URL = strings.TrimSpace(action.URL)
	action.Selector = strings.TrimSpace(action.Selector)
	action.Text = strings.TrimSpace(action.Text)
	action.OutputPath = strings.TrimSpace(action.OutputPath)
	risk, ok := browserActionRisks[action.Action]
	if !ok {
		return invalidBrowserAction(action.Action, "unsupported browser action", "", raw)
	}
	if risk == BrowserRiskDestructive {
		return invalidBrowserAction(action.Action, "destructive browser action is not allowed", string(risk), raw)
	}
	if action.Reason == "" || len(action.Reason) > 500 {
		return invalidBrowserAction(action.Action, "reason is required and must be at most 500 characters", string(risk), raw)
	}
	if action.URL != "" {
		return invalidBrowserAction(action.Action, "arbitrary URL fields are not accepted; use url_id from candidates", string(risk), raw)
	}
	if action.OutputPath != "" {
		return invalidBrowserAction(action.Action, "Codex-specified output paths are not accepted", string(risk), raw)
	}
	if browserActionEndsSession(action) {
		return invalidBrowserAction(action.Action, "session-ending browser actions are not allowed during Stage G", string(risk), raw)
	}
	switch action.Action {
	case "open_candidate":
		if !browserCandidateExists(action.URLID, candidates) {
			return invalidBrowserAction(action.Action, "url_id must reference an allowed candidate", string(risk), raw)
		}
	case "click_navigation", "click_button", "fill_input", "submit_local_form":
		if action.Selector == "" && action.Text == "" {
			return invalidBrowserAction(action.Action, "selector or text target is required", string(risk), raw)
		}
	}
	if len(action.Selector) > 512 {
		return invalidBrowserAction(action.Action, "selector exceeds 512 characters", string(risk), raw)
	}
	if len(action.Text) > 240 {
		return invalidBrowserAction(action.Action, "text target exceeds 240 characters", string(risk), raw)
	}
	if len(action.Value) > 2000 {
		return invalidBrowserAction(action.Action, "input value exceeds 2000 characters", string(risk), raw)
	}
	return browserActionValidation{Action: action, Risk: risk}
}

func ValidateBrowserAction(action BrowserAction, candidates []BrowserURLCandidate, raw string) BrowserActionValidation {
	return validateBrowserAction(action, candidates, raw)
}

func browserActionEndsSession(action BrowserAction) bool {
	switch action.Action {
	case "click_navigation", "click_button", "submit_local_form":
	default:
		return false
	}
	return browserActionTextEndsSession(strings.Join([]string{action.Selector, action.Text, action.Reason}, " "))
}

func browserActionTextEndsSession(value string) bool {
	tokens := browserActionSessionTokens(value)
	for index, token := range tokens {
		switch token {
		case "logout", "signout", "logoff", "signoff":
			return true
		}
		if index+1 >= len(tokens) {
			continue
		}
		next := tokens[index+1]
		if (token == "log" || token == "sign") && (next == "out" || next == "off") {
			return true
		}
		if token == "end" && next == "session" {
			return true
		}
		if (token == "session" && next == "exit") || (token == "exit" && next == "session") {
			return true
		}
	}
	return false
}

func browserActionSessionTokens(value string) []string {
	var tokens []string
	var builder strings.Builder
	for _, ch := range strings.ToLower(value) {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
			builder.WriteRune(ch)
			continue
		}
		if builder.Len() > 0 {
			tokens = append(tokens, builder.String())
			builder.Reset()
		}
	}
	if builder.Len() > 0 {
		tokens = append(tokens, builder.String())
	}
	return tokens
}

func invalidBrowserAction(action, reason, risk, raw string) browserActionValidation {
	return browserActionValidation{Blocked: &BlockedBrowserAction{
		Action: action,
		Reason: reason,
		Risk:   risk,
		Raw:    truncateString(strings.TrimSpace(raw), 2000),
	}}
}

func browserCandidateExists(id string, candidates []BrowserURLCandidate) bool {
	for _, candidate := range candidates {
		if candidate.ID == id {
			return true
		}
	}
	return false
}

func browserCandidateByID(id string, candidates []BrowserURLCandidate) (BrowserURLCandidate, error) {
	for _, candidate := range candidates {
		if candidate.ID == id {
			return candidate, nil
		}
	}
	return BrowserURLCandidate{}, fmt.Errorf("unknown url_id %q", id)
}

func BrowserActionForWrapper(action BrowserAction, candidates []BrowserURLCandidate) (browserpkg.Action, error) {
	result := browserpkg.Action{
		Name:     action.Action,
		Selector: action.Selector,
		Text:     action.Text,
		Value:    action.Value,
		Reason:   action.Reason,
	}
	if action.Action == "open_candidate" {
		candidate, err := browserCandidateByID(action.URLID, candidates)
		if err != nil {
			return result, err
		}
		result.URL = candidate.URL
	}
	if action.Action == "wait" {
		result.WaitMS = 1000
	}
	return result, nil
}

func BrowserActionTextEndsSession(value string) bool {
	return browserActionTextEndsSession(value)
}
