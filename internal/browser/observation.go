package browser

import (
	"net/url"
	"regexp"
	"strings"
)

type Action struct {
	Name     string `json:"name"`
	URL      string `json:"url,omitempty"`
	Selector string `json:"selector,omitempty"`
	Text     string `json:"text,omitempty"`
	Value    string `json:"value,omitempty"`
	Reason   string `json:"reason,omitempty"`
	WaitMS   int    `json:"wait_ms,omitempty"`
}

type Policy struct {
	AllowlistOrigins  []string `json:"allowlist_origins"`
	ArtifactRoot      string   `json:"artifact_root"`
	ScreenshotPath    string   `json:"screenshot_path"`
	DisableScreenshot bool     `json:"disable_screenshot,omitempty"`
	StorageStatePath  string   `json:"storage_state_path"`
	LastURLPath       string   `json:"last_url_path"`
	FormStatePath     string   `json:"form_state_path"`
}

type Observation struct {
	Action          string            `json:"action"`
	OK              bool              `json:"ok"`
	CurrentURL      string            `json:"current_url,omitempty"`
	Title           string            `json:"title,omitempty"`
	VisibleText     string            `json:"visible_text,omitempty"`
	Controls        []ControlSummary  `json:"controls,omitempty"`
	ConsoleErrors   []string          `json:"console_errors,omitempty"`
	PageErrors      []string          `json:"page_errors,omitempty"`
	NetworkIssues   []NetworkIssue    `json:"network_issues,omitempty"`
	NetworkEvents   []NetworkEvent    `json:"network_events,omitempty"`
	BlockedRequests []BlockedRequest  `json:"blocked_requests,omitempty"`
	ScreenshotPath  string            `json:"screenshot_path,omitempty"`
	Error           string            `json:"error,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

type ControlSummary struct {
	Role        string `json:"role,omitempty"`
	Text        string `json:"text,omitempty"`
	Selector    string `json:"selector,omitempty"`
	Name        string `json:"name,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Type        string `json:"type,omitempty"`
	HasValue    bool   `json:"has_value,omitempty"`
}

type NetworkIssue struct {
	URL    string `json:"url"`
	Status int    `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

type NetworkEvent struct {
	URL          string `json:"url"`
	Method       string `json:"method,omitempty"`
	Status       int    `json:"status,omitempty"`
	ResourceType string `json:"resource_type,omitempty"`
	Error        string `json:"error,omitempty"`
}

type BlockedRequest struct {
	URL    string `json:"url"`
	Origin string `json:"origin,omitempty"`
}

var (
	secretPairPattern = regexp.MustCompile(`(?i)\b(access[_-]?token|refresh[_-]?token|api[_-]?key|apikey|authorization|cookie|jwt|password|passwd|secret|session[_-]?id|sessionid|token)(\s*[:=]\s*)([^\s,;&"']+)`)
	bearerPattern     = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
	jwtPattern        = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)
	emailPattern      = regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
	urlPattern        = regexp.MustCompile(`https?://[^\s"'<>]+`)
)

func sanitizeObservation(observation Observation) Observation {
	observation.CurrentURL = sanitizeURL(observation.CurrentURL)
	observation.Title = redactText(observation.Title)
	observation.VisibleText = redactText(observation.VisibleText)
	observation.Error = redactText(observation.Error)
	observation.ConsoleErrors = redactTexts(observation.ConsoleErrors)
	observation.PageErrors = redactTexts(observation.PageErrors)
	for index := range observation.Controls {
		observation.Controls[index].Text = redactText(observation.Controls[index].Text)
		observation.Controls[index].Selector = redactText(observation.Controls[index].Selector)
		observation.Controls[index].Name = redactText(observation.Controls[index].Name)
		observation.Controls[index].Placeholder = redactText(observation.Controls[index].Placeholder)
		observation.Controls[index].Type = redactText(observation.Controls[index].Type)
	}
	for index := range observation.NetworkIssues {
		observation.NetworkIssues[index].URL = sanitizeURL(observation.NetworkIssues[index].URL)
		observation.NetworkIssues[index].Error = redactText(observation.NetworkIssues[index].Error)
	}
	for index := range observation.NetworkEvents {
		observation.NetworkEvents[index].URL = sanitizeURL(observation.NetworkEvents[index].URL)
		observation.NetworkEvents[index].Method = redactText(observation.NetworkEvents[index].Method)
		observation.NetworkEvents[index].ResourceType = redactText(observation.NetworkEvents[index].ResourceType)
		observation.NetworkEvents[index].Error = redactText(observation.NetworkEvents[index].Error)
	}
	for index := range observation.BlockedRequests {
		observation.BlockedRequests[index].URL = sanitizeURL(observation.BlockedRequests[index].URL)
		observation.BlockedRequests[index].Origin = sanitizeURL(observation.BlockedRequests[index].Origin)
	}
	if len(observation.Metadata) > 0 {
		for key, value := range observation.Metadata {
			observation.Metadata[key] = redactText(value)
		}
	}
	return observation
}

func redactTexts(values []string) []string {
	for index := range values {
		values[index] = redactText(values[index])
	}
	return values
}

func redactText(value string) string {
	value = urlPattern.ReplaceAllStringFunc(value, sanitizeURL)
	return redactPlainText(value)
}

func redactPlainText(value string) string {
	value = secretPairPattern.ReplaceAllString(value, `$1$2[REDACTED]`)
	value = bearerPattern.ReplaceAllString(value, `Bearer [REDACTED]`)
	value = jwtPattern.ReplaceAllString(value, `[REDACTED_JWT]`)
	value = emailPattern.ReplaceAllString(value, `[REDACTED_EMAIL]`)
	return strings.TrimSpace(value)
}

func sanitizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" {
		return redactPlainText(raw)
	}
	if parsed.RawQuery != "" {
		parsed.RawQuery = "REDACTED"
	}
	parsed.Fragment = ""
	return parsed.String()
}
