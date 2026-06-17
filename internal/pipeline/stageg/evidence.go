package stageg

import (
	"fmt"
	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/frontende2e"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"net/url"
	"strings"
)

func stageGPositiveEvidenceOutcome(candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction, reason string) (FrontendE2ESummary, bool) {
	profile := stageGEvidenceProfileForObservations(observations)
	if !profile.DeterministicPassReady() {
		return FrontendE2ESummary{}, false
	}
	summary := stageGSummary("passed", reason, candidates, observations, blocked)
	if blockReason := stageGFinishScreenshotBlockReasonForSummary(summary, observations); blockReason != "" {
		return FrontendE2ESummary{}, false
	}
	summary.Notes = append(summary.Notes, stageGEvidenceNote(observations))
	return summary, true
}

type stageGEvidenceProfile struct {
	HasUnrecoveredProductFailure bool
	HasPostAuthSessionLoss       bool
	HasRenderedFrontend          bool
	AuthSuccess                  bool
	AuthenticatedState           bool
	BusinessEndpointCount        int
	BusinessUISignalCount        int
	InteractiveProductUICount    int
	InteractiveProductUIStates   int
	ProductNavigationChangeCount int
	DistinctMeaningfulStates     int
	SupportScreenshotCount       int
	KeyScreenshotCount           int
}

func stageGEvidenceProfileForObservations(observations []browserpkg.Observation) stageGEvidenceProfile {
	evidenceObservations := observations
	authSuccess := false
	if authIndex := stageGCredentialedAuthSuccessIndex(observations); authIndex >= 0 {
		evidenceObservations = observations[authIndex:]
		authSuccess = true
	}
	return stageGEvidenceProfile{
		HasUnrecoveredProductFailure: stageGHasUnrecoveredProductFailure(observations),
		HasPostAuthSessionLoss:       stageGPostAuthSessionLossEvidence(observations) != "",
		HasRenderedFrontend:          stageGHasRenderedFrontendEvidence(evidenceObservations),
		AuthSuccess:                  authSuccess,
		AuthenticatedState:           stageGHasAuthenticatedObservation(evidenceObservations),
		BusinessEndpointCount:        stageGSuccessfulBusinessNetworkEvidenceCount(evidenceObservations),
		BusinessUISignalCount:        stageGAuthenticatedBusinessUISignalCount(evidenceObservations),
		InteractiveProductUICount:    stageGInteractiveProductUICount(evidenceObservations),
		InteractiveProductUIStates:   stageGDistinctInteractiveProductUIStateCount(evidenceObservations),
		ProductNavigationChangeCount: stageGProductNavigationChangeCount(evidenceObservations),
		DistinctMeaningfulStates:     stageGDistinctMeaningfulStateCount(evidenceObservations),
		SupportScreenshotCount:       stageGSupportScreenshotCount(observations),
		KeyScreenshotCount:           stageGBrowserScreenshotCount(observations),
	}
}

func (profile stageGEvidenceProfile) DeterministicPassReady() bool {
	return !profile.HasUnrecoveredProductFailure &&
		!profile.HasPostAuthSessionLoss &&
		profile.HasRenderedFrontend &&
		profile.SupportScreenshotCount >= 2 &&
		profile.CoreBusinessWorkflowReady()
}

func (profile stageGEvidenceProfile) CoreBusinessWorkflowReady() bool {
	hasNetworkWorkflow := profile.BusinessEndpointCount >= 2
	hasStructuredUIWorkflow := profile.BusinessEndpointCount == 0 && profile.InteractiveProductUIStates >= 2 && profile.ProductNavigationChangeCount >= 1
	hasNamedUIWorkflow := profile.BusinessEndpointCount == 0 && profile.BusinessUISignalCount >= 2 && profile.DistinctMeaningfulStates >= 2
	hasAuthWorkflow := profile.AuthSuccess && profile.AuthenticatedState && (hasNetworkWorkflow || hasStructuredUIWorkflow || hasNamedUIWorkflow)
	hasPublicWorkflow := (hasNetworkWorkflow && profile.DistinctMeaningfulStates >= 2) || hasStructuredUIWorkflow || hasNamedUIWorkflow
	return hasAuthWorkflow || hasPublicWorkflow
}

func stageGDeterministicPositiveEvidenceReady(observations []browserpkg.Observation) bool {
	return stageGEvidenceProfileForObservations(observations).DeterministicPassReady()
}

func stageGSupportScreenshotCount(observations []browserpkg.Observation) int {
	count := 0
	for index := range observations {
		if stageGScreenshotObservationCanSupportSummary(index, observations) {
			count++
		}
	}
	return count
}

func stageGHasUnrecoveredProductFailure(observations []browserpkg.Observation) bool {
	for index := range observations {
		if stageGObservationHasProductFailureEvidenceAt(index, observations) {
			return true
		}
	}
	return false
}

func stageGHasRenderedFrontendEvidence(observations []browserpkg.Observation) bool {
	for _, observation := range observations {
		if !observation.OK {
			continue
		}
		if strings.TrimSpace(observation.CurrentURL) == "" {
			continue
		}
		if len(strings.TrimSpace(observation.VisibleText)) >= 20 {
			return true
		}
	}
	return false
}

func stageGHasAuthenticatedObservation(observations []browserpkg.Observation) bool {
	for _, observation := range observations {
		if (stageGObservationLooksAuthenticated(observation) || stageGObservationLooksInteractiveProductUI(observation)) && !stageGObservationLooksAuthGate(observation) {
			return true
		}
	}
	return false
}

func stageGHasSuccessfulBusinessNetworkEvidence(observations []browserpkg.Observation) bool {
	return stageGSuccessfulBusinessNetworkEvidenceCount(observations) > 0
}

func stageGSuccessfulBusinessNetworkEvidenceCount(observations []browserpkg.Observation) int {
	seen := map[string]bool{}
	for _, observation := range observations {
		for _, event := range observation.NetworkEvents {
			if stageGNetworkEventLooksSuccessfulBusiness(event) {
				key := stageGNetworkURLKey(event.URL)
				if key == "" {
					key = strings.TrimSpace(event.URL)
				}
				if key != "" {
					seen[key] = true
				}
			}
		}
	}
	return len(seen)
}

func stageGNetworkEventLooksSuccessfulBusiness(event browserpkg.NetworkEvent) bool {
	if event.Status < 200 || event.Status >= 400 || strings.TrimSpace(event.Error) != "" {
		return false
	}
	if stageGNetworkURLLooksAuth(event.URL) || stageGNetworkURLLooksLogout(event.URL) {
		return false
	}
	if stageGNetworkURLLooksFrameworkNoise(event.URL) {
		return false
	}
	resourceType := strings.ToLower(strings.TrimSpace(event.ResourceType))
	if resourceType == "image" || resourceType == "font" || resourceType == "stylesheet" || resourceType == "script" || resourceType == "websocket" {
		return false
	}
	parsed, err := url.Parse(strings.TrimSpace(event.URL))
	if err != nil {
		return false
	}
	path := strings.ToLower(parsed.Path)
	if path == "" || path == "/" {
		return false
	}
	for _, marker := range []string{"/api/", "/graphql", "/dashboard", "/admin", "/project", "/projects", "/user", "/users", "/audit", "/analytics", "/settings", "/module", "/modules", "/order", "/orders", "/product", "/products"} {
		if strings.Contains(path, marker) {
			return true
		}
	}
	method := strings.ToUpper(strings.TrimSpace(event.Method))
	return method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
}

func stageGNetworkFailureLooksIgnorableNoise(raw, resourceType string, status int, message string) bool {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return false
	}
	parsed, err := url.Parse(raw)
	target := raw
	if err == nil {
		target = strings.ToLower(parsed.Path)
	}
	resourceType = strings.ToLower(strings.TrimSpace(resourceType))
	if resourceType == "websocket" || resourceType == "eventsource" {
		return stageGNetworkURLLooksFrameworkNoise(raw)
	}
	for _, prefix := range []string{"/@vite", "/__vite", "/sockjs-node", "/socket.io", "/webpack"} {
		if strings.HasPrefix(target, prefix) {
			return true
		}
	}
	for _, marker := range []string{"hot-update", "hmr", "livereload", "__webpack", "vite_ping"} {
		if strings.Contains(target, marker) {
			return true
		}
	}
	if status >= 500 {
		return false
	}
	if strings.HasPrefix(target, "/_blazor") && (strings.Contains(target, "negotiate") || resourceType == "fetch" || strings.TrimSpace(message) != "" || status == 0) {
		return true
	}
	for _, suffix := range []string{".map", ".png", ".jpg", ".jpeg", ".svg", ".ico", ".woff", ".woff2", ".ttf"} {
		if strings.HasSuffix(target, suffix) {
			return true
		}
	}
	return false
}

func stageGNetworkURLLooksFrameworkNoise(raw string) bool {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return false
	}
	parsed, err := url.Parse(raw)
	target := raw
	if err == nil {
		target = strings.ToLower(parsed.Path)
	}
	for _, prefix := range []string{
		"/_blazor",
		"/_framework",
		"/_next/",
		"/@vite",
		"/__vite",
		"/sockjs-node",
		"/socket.io",
		"/webpack",
		"/vite",
	} {
		if strings.HasPrefix(target, prefix) {
			return true
		}
	}
	for _, marker := range []string{"hot-update", "hmr", "livereload", "__webpack", "vite_ping"} {
		if strings.Contains(target, marker) {
			return true
		}
	}
	for _, suffix := range []string{".js", ".css", ".map", ".png", ".jpg", ".jpeg", ".svg", ".ico", ".woff", ".woff2", ".ttf"} {
		if strings.HasSuffix(target, suffix) {
			return true
		}
	}
	return false
}

func stageGAuthenticatedBusinessUISignalCount(observations []browserpkg.Observation) int {
	signals := map[string]bool{}
	for _, observation := range observations {
		if !observation.OK || stageGObservationLooksAuthGate(observation) || stageGObservationHasPasswordControl(observation) {
			continue
		}
		stageGAddBusinessUISignals(signals, observation.CurrentURL)
		stageGAddBusinessUISignals(signals, observation.Title)
		stageGAddBusinessUISignals(signals, observation.VisibleText)
		for _, control := range observation.Controls {
			stageGAddBusinessUISignals(signals, strings.Join([]string{control.Role, control.Text, control.Name, control.Placeholder, control.Type}, " "))
		}
	}
	return len(signals)
}

func stageGInteractiveProductUICount(observations []browserpkg.Observation) int {
	count := 0
	for _, observation := range observations {
		if stageGObservationLooksInteractiveProductUI(observation) {
			count++
		}
	}
	return count
}

func stageGDistinctInteractiveProductUIStateCount(observations []browserpkg.Observation) int {
	seen := map[string]bool{}
	for _, observation := range observations {
		if !stageGObservationLooksInteractiveProductUI(observation) {
			continue
		}
		key := stageGObservationStateKey(observation)
		if key != "" {
			seen[key] = true
		}
	}
	return len(seen)
}

func stageGProductNavigationChangeCount(observations []browserpkg.Observation) int {
	changes := 0
	previous := ""
	for _, observation := range observations {
		if !stageGObservationLooksInteractiveProductUI(observation) {
			continue
		}
		current := stageGObservationURLPathKey(observation.CurrentURL)
		if current == "" {
			continue
		}
		if previous != "" && current != previous {
			changes++
		}
		previous = current
	}
	return changes
}

func stageGObservationURLPathKey(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return strings.ToLower(raw)
	}
	path := strings.TrimSpace(parsed.Path)
	if path == "" {
		path = "/"
	}
	return strings.ToLower(path)
}

func stageGObservationLooksInteractiveProductUI(observation browserpkg.Observation) bool {
	if !observation.OK || stageGObservationLooksAuthGate(observation) || stageGObservationLooksMarketingPage(observation) {
		return false
	}
	if stageGObservationHasPasswordControl(observation) {
		return false
	}
	text := strings.TrimSpace(observation.VisibleText)
	if len(text) < 60 {
		return false
	}
	links, buttons, inputs := stageGControlCounts(observation)
	controlCount := links + buttons + inputs
	if controlCount < 4 {
		return false
	}
	signals := 0
	if links >= 2 {
		signals++
	}
	if buttons >= 2 {
		signals++
	}
	if inputs >= 1 {
		signals++
	}
	if controlCount >= 6 {
		signals++
	}
	if stageGTextLooksDataOrWorkflow(text) {
		signals++
	}
	return signals >= 3
}

func stageGControlCounts(observation browserpkg.Observation) (links, buttons, inputs int) {
	for _, control := range observation.Controls {
		role := strings.ToLower(strings.TrimSpace(control.Role))
		target := strings.ToLower(strings.Join([]string{control.Role, control.Text, control.Name, control.Placeholder, control.Type}, " "))
		switch role {
		case "link":
			links++
		case "button":
			buttons++
		default:
			if strings.Contains(target, "password") || strings.Contains(target, "passwd") {
				continue
			}
			inputs++
		}
	}
	return links, buttons, inputs
}

func stageGTextLooksDataOrWorkflow(value string) bool {
	value = strings.ToLower(strings.Join(strings.Fields(value), " "))
	matches := 0
	for _, marker := range []string{"search", "filter", "status", "priority", "owner", "assigned", "recent", "activity", "queue", "review", "date", "total", "items", "calendar", "export"} {
		if strings.Contains(value, marker) {
			matches++
		}
	}
	return matches >= 3
}

func stageGObservationLooksMarketingPage(observation browserpkg.Observation) bool {
	urlValue := strings.ToLower(strings.TrimSpace(observation.CurrentURL))
	text := strings.ToLower(strings.Join(strings.Fields(observation.VisibleText), " "))
	if strings.Contains(urlValue, "/pricing") || strings.Contains(urlValue, "/contact") || strings.Contains(urlValue, "/about") || strings.Contains(urlValue, "/docs") {
		return true
	}
	matches := 0
	for _, marker := range []string{"pricing", "contact sales", "get started", "book a demo", "features", "testimonials", "hero", "learn more"} {
		if strings.Contains(text, marker) {
			matches++
		}
	}
	links, buttons, inputs := stageGControlCounts(observation)
	return matches >= 2 && inputs == 0 && links+buttons < 5
}

func stageGAddBusinessUISignals(signals map[string]bool, value string) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return
	}
	markers := map[string][]string{
		"dashboard":  {"dashboard", "/dashboard"},
		"admin":      {"admin", "/admin"},
		"analytics":  {"analytics", "metrics"},
		"users":      {"user management", "users", "/users"},
		"projects":   {"projects", "/projects"},
		"settings":   {"settings", "/settings"},
		"modules":    {"modules", "/modules"},
		"orders":     {"orders", "/orders"},
		"products":   {"products", "/products", "catalog"},
		"reports":    {"reports", "/reports"},
		"audit":      {"audit", "activity log"},
		"management": {"management"},
		"create":     {"create", "new "},
		"edit":       {"edit", "update"},
		"save":       {"save", "publish"},
	}
	for signal, candidates := range markers {
		for _, marker := range candidates {
			if strings.Contains(value, marker) {
				signals[signal] = true
				break
			}
		}
	}
}

func stageGDistinctMeaningfulStateCount(observations []browserpkg.Observation) int {
	seen := map[string]bool{}
	for _, observation := range observations {
		if !observation.OK || stageGObservationLooksAuthGate(observation) {
			continue
		}
		key := stageGObservationStateKey(observation)
		if key != "" {
			seen[key] = true
		}
	}
	return len(seen)
}

func stageGEvidenceNote(observations []browserpkg.Observation) string {
	profile := stageGEvidenceProfileForObservations(observations)
	return fmt.Sprintf("Deterministic Stage G evidence: auth_success=%t authenticated_state=%t business_network_endpoints=%d business_ui_signals=%d interactive_product_ui=%d interactive_product_states=%d product_navigation_changes=%d support_screenshots=%d key_screenshots=%d distinct_states=%d.", profile.AuthSuccess, profile.AuthenticatedState, profile.BusinessEndpointCount, profile.BusinessUISignalCount, profile.InteractiveProductUICount, profile.InteractiveProductUIStates, profile.ProductNavigationChangeCount, profile.SupportScreenshotCount, profile.KeyScreenshotCount, profile.DistinctMeaningfulStates)
}

func stageGPartialProductBlockerFinding(observations []browserpkg.Observation, reason string) (model.Finding, bool) {
	if evidence := stageGPostAuthSessionLossEvidence(observations); evidence != "" {
		return model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      "Authenticated browser session was lost during Stage G",
			Rule:       "Stage G must preserve an authenticated browser session while exploring the documented product workflow.",
			Evidence:   evidence + " " + strings.TrimSpace(reason),
			Impact:     "Stage G cannot trust later browser evidence because the workflow returned to an unauthenticated state.",
			MinimumFix: "Avoid logout/session-reset actions during E2E exploration and persist browser session state across Stage G browser actions.",
			SourcePath: "frontend_e2e_observations.json",
		}, true
	}
	if evidence := stageGAuthAcceptedNoTransitionEvidence(observations); evidence != "" {
		return model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      "Authentication response did not reach authenticated browser workflow",
			Rule:       "A successful credentialed authentication response should establish an authenticated browser session and route to the documented product workflow.",
			Evidence:   evidence + " " + strings.TrimSpace(reason),
			Impact:     "Stage G cannot verify the authenticated product workflow even though the auth request returned success.",
			MinimumFix: "Persist the browser session after login and redirect/render the authenticated workflow after successful credentials.",
			SourcePath: "frontend_e2e_observations.json",
		}, true
	}
	if evidence := stageGAuthGateBlockerEvidence(observations); evidence != "" {
		return model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      "Authentication gate prevented browser workflow coverage",
			Rule:       "README-provided local credentials or safe registration paths should allow Stage G to reach authenticated browser workflows, or the app should expose deterministic test authentication.",
			Evidence:   evidence + " " + strings.TrimSpace(reason),
			Impact:     "Stage G cannot verify the authenticated product workflow from the browser UI.",
			MinimumFix: "Provide deterministic E2E credentials or a test-mode CAPTCHA bypass, and ensure the registration/login controls are reachable with stable selectors.",
			SourcePath: "frontend_e2e_observations.json",
		}, true
	}
	if evidence := stageGAuthSelectorBlockerEvidence(observations); evidence != "" {
		return model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      "Authentication controls prevented browser workflow coverage",
			Rule:       "README-provided local credentials or safe registration paths should be usable through stable browser controls.",
			Evidence:   evidence + " " + strings.TrimSpace(reason),
			Impact:     "Stage G cannot verify the authenticated product workflow from the browser UI.",
			MinimumFix: "Expose stable login/register selectors or provide deterministic E2E authentication controls.",
			SourcePath: "frontend_e2e_observations.json",
		}, true
	}
	if evidence := stageGAuthGateStallEvidence(observations); evidence != "" {
		return model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      "Authentication gate prevented browser workflow coverage",
			Rule:       "README-provided local credentials or safe registration paths should allow Stage G to reach authenticated browser workflows, or the app should expose deterministic test authentication.",
			Evidence:   evidence + " " + strings.TrimSpace(reason),
			Impact:     "Stage G cannot verify the authenticated product workflow from the browser UI.",
			MinimumFix: "Provide deterministic E2E credentials or a test-mode auth bypass, and ensure successful login redirects to the documented product workflow.",
			SourcePath: "frontend_e2e_observations.json",
		}, true
	}
	if finding, ok := stageGNativeDialogBoundaryFinding(observations, reason); ok {
		return finding, true
	}
	for index := range observations {
		if stageGObservationHasProductFailureEvidenceAt(index, observations) {
			return model.Finding{
				Stage:      string(model.StageG),
				Severity:   "High",
				Title:      "Frontend workflow could not progress after product error",
				Rule:       "Browser workflows should either reach the documented business state or expose a clear product failure.",
				Evidence:   stageGPartialFailureEvidence(index, observations, reason),
				Impact:     "Stage G could not complete the documented frontend workflow.",
				MinimumFix: "Fix the observed frontend/API failure and rerun Stage G.",
				SourcePath: "frontend_e2e_observations.json",
			}, true
		}
	}
	return model.Finding{}, false
}

func stageGAuthSelectorBlockerEvidence(observations []browserpkg.Observation) string {
	if stageGObservedAuthRecovery(observations) {
		return ""
	}
	credentialEvidence := false
	selectorFailures := 0
	var firstFailure browserpkg.Observation
	for _, observation := range observations {
		if stageGObservationHasCredentialEvidence(observation) {
			credentialEvidence = true
		}
		if observation.OK || !stageGObservationLooksAuthSelectorFailure(observation) {
			continue
		}
		if !credentialEvidence && !stageGObservationLooksAuthInputAction(observation) {
			continue
		}
		if selectorFailures == 0 {
			firstFailure = observation
		}
		selectorFailures++
	}
	if selectorFailures < 2 {
		return ""
	}
	target := strings.TrimSpace(firstFailure.CurrentURL)
	if target == "" {
		target = "the authentication UI"
	}
	return fmt.Sprintf("Credentialed authentication control failed on %s after %d selector failure(s): %s", target, selectorFailures, strings.TrimSpace(firstFailure.Error))
}

func stageGObservationLooksAuthSelectorFailure(observation browserpkg.Observation) bool {
	errorText := strings.ToLower(strings.TrimSpace(observation.Error))
	if errorText == "" || !strings.Contains(errorText, "timeout") {
		return false
	}
	target := strings.ToLower(strings.Join([]string{observation.Action, observation.Error, observation.VisibleText, observation.CurrentURL}, " "))
	for _, marker := range []string{"email", "password", "username", "login", "sign in", "signin", "register", "auth", "form input"} {
		if strings.Contains(target, marker) {
			return true
		}
	}
	return false
}

func stageGObservationLooksAuthInputAction(observation browserpkg.Observation) bool {
	target := strings.ToLower(strings.Join([]string{observation.Action, observation.Error, observation.VisibleText, observation.CurrentURL}, " "))
	return strings.Contains(target, "fill_input") &&
		(strings.Contains(target, "email") || strings.Contains(target, "password") || strings.Contains(target, "username") || strings.Contains(target, "login"))
}

func stageGObservationStopReason(observations []browserpkg.Observation) string {
	if evidence := stageGPostAuthSessionLossEvidence(observations); evidence != "" {
		return "Stage G stopped after authenticated session was lost. " + evidence
	}
	if evidence := stageGAuthAcceptedNoTransitionEvidence(observations); evidence != "" {
		return "Stage G stopped after accepted authentication did not reach a product workflow. " + evidence
	}
	if stageGAuthGateStallEvidence(observations) != "" {
		return "Stage G stopped after repeated authentication-gate attempts did not reach a product workflow."
	}
	if evidence := stageGRepeatedStateStallEvidence(observations); evidence != "" {
		return "Stage G stopped after repeated unchanged browser observations. " + evidence
	}
	return ""
}

func stageGAuthGateBlockerEvidence(observations []browserpkg.Observation) string {
	authFailure := ""
	authSuccess := false
	stayedOnAuthGate := false
	registerSelectorFailures := 0
	captchaBoundary := false
	for _, observation := range observations {
		text := strings.ToLower(observation.VisibleText)
		urlValue := strings.ToLower(observation.CurrentURL)
		if strings.Contains(text, "captcha") || strings.Contains(text, "sign in") || strings.Contains(text, "register") || strings.Contains(urlValue, "/login") {
			stayedOnAuthGate = true
		}
		if !observation.OK && strings.Contains(strings.ToLower(observation.Error), "timeout") {
			target := strings.ToLower(strings.Join([]string{observation.Action, observation.Error, observation.VisibleText}, " "))
			if strings.Contains(target, "register") || strings.Contains(target, "captcha") || strings.Contains(target, "sign in") || strings.Contains(target, "login") {
				registerSelectorFailures++
			}
		}
		for _, issue := range observation.NetworkIssues {
			if stageGNetworkURLLooksAuth(issue.URL) && stageGRecoverableAuthClientStatus(issue.Status) {
				authFailure = fmt.Sprintf("%s status=%d", issue.URL, issue.Status)
				if stageGObservationAuthFailureLooksCaptchaBoundary(observation, issue.URL, issue.Status) {
					captchaBoundary = true
				}
			}
		}
		for _, event := range observation.NetworkEvents {
			if stageGNetworkURLLooksAuth(event.URL) && !stageGNetworkURLLooksLogout(event.URL) && event.Status >= 200 && event.Status < 400 && strings.ToUpper(strings.TrimSpace(event.Method)) == "POST" {
				authSuccess = true
			}
			if stageGNetworkURLLooksAuth(event.URL) && stageGRecoverableAuthClientStatus(event.Status) {
				authFailure = fmt.Sprintf("%s status=%d", event.URL, event.Status)
				if stageGObservationAuthFailureLooksCaptchaBoundary(observation, event.URL, event.Status) {
					captchaBoundary = true
				}
			}
		}
	}
	if authFailure == "" || authSuccess {
		return ""
	}
	credentialedFailures := stageGCredentialedAuthClientFailureCount(observations)
	if !captchaBoundary && credentialedFailures < stageGAuthGateSubmitStallLimit {
		return ""
	}
	evidence := "Observed authentication gate failure: " + authFailure + "."
	if stayedOnAuthGate {
		evidence += " Browser remained on login/register/CAPTCHA UI."
	}
	if credentialedFailures > 0 {
		evidence += fmt.Sprintf(" Credentialed auth failure attempts: %d.", credentialedFailures)
	}
	if registerSelectorFailures > 0 {
		evidence += fmt.Sprintf(" Repeated auth/register selector attempts failed %d time(s).", registerSelectorFailures)
	}
	return evidence
}

func stageGCredentialedAuthClientFailureCount(observations []browserpkg.Observation) int {
	return frontende2e.CredentialedAuthClientFailureCount(observations)
}

func stageGObservationAuthClientFailure(observation browserpkg.Observation) (string, int, bool) {
	return frontende2e.ObservationAuthClientFailure(observation)
}

func stageGObservationAuthFailureLooksCaptchaBoundary(observation browserpkg.Observation, raw string, status int) bool {
	return frontende2e.ObservationAuthFailureLooksCaptchaBoundary(observation, raw, status)
}

func stageGPostAuthSessionLossEvidence(observations []browserpkg.Observation) string {
	establishedAt := -1
	establishedBy := ""
	authenticatedStateSeen := false
	for index, observation := range observations {
		if establishedAt >= 0 {
			if stageGObservationHasSessionEndingError(observation) {
				return fmt.Sprintf("After %s at observation %d, action %s attempted a session-ending browser operation: %s.", establishedBy, establishedAt+1, strings.TrimSpace(observation.Action), strings.TrimSpace(observation.Error))
			}
			for _, event := range observation.NetworkEvents {
				if stageGNetworkURLLooksLogout(event.URL) && (event.Status >= 200 && event.Status < 400 || strings.TrimSpace(event.Error) != "") {
					return fmt.Sprintf("After %s at observation %d, action %s triggered session-ending request %s.", establishedBy, establishedAt+1, strings.TrimSpace(observation.Action), stageGNetworkEventText(event))
				}
			}
			for _, issue := range observation.NetworkIssues {
				if stageGNetworkURLLooksLogout(issue.URL) && (issue.Status >= 200 || strings.TrimSpace(issue.Error) != "") {
					return fmt.Sprintf("After %s at observation %d, action %s hit session-ending request %s.", establishedBy, establishedAt+1, strings.TrimSpace(observation.Action), stageGNetworkIssueText(issue))
				}
			}
			if authenticatedStateSeen && stageGObservationLooksAuthGate(observation) {
				target := strings.TrimSpace(observation.CurrentURL)
				if target == "" {
					target = "login/register UI"
				}
				return fmt.Sprintf("After %s at observation %d, browser returned to %s during action %s.", establishedBy, establishedAt+1, target, strings.TrimSpace(observation.Action))
			}
			if stageGObservationLooksAuthenticated(observation) && !stageGObservationLooksAuthGate(observation) {
				authenticatedStateSeen = true
				establishedBy = "authenticated browser state"
			}
			continue
		}
		if stageGObservationLooksAuthenticated(observation) && !stageGObservationLooksAuthGate(observation) {
			establishedAt = index
			establishedBy = "authenticated browser state"
			authenticatedStateSeen = true
			continue
		}
		for _, event := range observation.NetworkEvents {
			if strings.ToUpper(strings.TrimSpace(event.Method)) == "POST" &&
				event.Status >= 200 && event.Status < 400 &&
				stageGNetworkURLLooksAuth(event.URL) &&
				!stageGNetworkURLLooksLogout(event.URL) {
				establishedAt = index
				establishedBy = "successful credentialed authentication"
				break
			}
		}
	}
	return ""
}

func stageGObservationHasSessionEndingError(observation browserpkg.Observation) bool {
	errorText := strings.ToLower(strings.TrimSpace(observation.Error))
	return strings.Contains(errorText, "session-ending browser target blocked") ||
		strings.Contains(errorText, "session-ending browser request blocked")
}

func stageGAuthGateStallEvidence(observations []browserpkg.Observation) string {
	if stageGObservedAuthRecovery(observations) || len(observations) == 0 {
		return ""
	}
	last := observations[len(observations)-1]
	if !stageGObservationLooksAuthGate(last) {
		return ""
	}
	submits := 0
	authGateObservations := 0
	credentialEvidence := false
	for _, observation := range observations {
		if !stageGObservationLooksAuthGate(observation) {
			continue
		}
		authGateObservations++
		if stageGObservationHasCredentialEvidence(observation) {
			credentialEvidence = true
		}
		if stageGObservationLooksInputAttempt(observation) {
			credentialEvidence = true
		}
		if stageGObservationLooksCompletedAuthSubmitAttempt(observation) {
			submits++
		}
	}
	if submits < stageGAuthGateSubmitStallLimit || !credentialEvidence {
		return ""
	}
	target := strings.TrimSpace(last.CurrentURL)
	if target == "" {
		target = "the login/register UI"
	}
	return fmt.Sprintf("Browser remained on %s after %d credentialed submit attempt(s) across %d auth-gate observation(s), with no successful auth transition observed.", target, submits, authGateObservations)
}

func stageGRepeatedStateStallEvidence(observations []browserpkg.Observation) string {
	if len(observations) < stageGRepeatedStateStallLimit {
		return ""
	}
	last := observations[len(observations)-1]
	lastKey := stageGObservationStateKey(last)
	if lastKey == "" {
		return ""
	}
	if stageGObservationLooksAuthGate(last) {
		return ""
	}
	sameState := 0
	activeActions := 0
	for index := len(observations) - 1; index >= 0; index-- {
		if stageGObservationStateKey(observations[index]) != lastKey {
			break
		}
		sameState++
		if stageGObservationCountsAsProgressAttempt(observations[index]) {
			activeActions++
		}
	}
	if sameState < stageGRepeatedStateStallLimit || activeActions < 2 {
		return ""
	}
	target := strings.TrimSpace(last.CurrentURL)
	if target == "" {
		target = strings.TrimSpace(last.Title)
	}
	if target == "" {
		target = "the same browser state"
	}
	return fmt.Sprintf("Browser stayed at %s with unchanged visible state for %d consecutive observation(s) after %d progress attempt(s).", target, sameState, activeActions)
}

func stageGObservedAuthSuccess(observations []browserpkg.Observation) bool {
	return stageGCredentialedAuthSuccessIndex(observations) >= 0
}

func stageGObservedAuthRecovery(observations []browserpkg.Observation) bool {
	if stageGObservedAuthNetworkSuccess(observations) {
		return true
	}
	return stageGObservedCredentialedAuthTransition(observations)
}

func stageGObservedAuthNetworkSuccess(observations []browserpkg.Observation) bool {
	for _, observation := range observations {
		for _, event := range observation.NetworkEvents {
			if strings.ToUpper(strings.TrimSpace(event.Method)) == "POST" &&
				event.Status >= 200 && event.Status < 400 &&
				stageGNetworkURLLooksAuth(event.URL) &&
				!stageGNetworkURLLooksLogout(event.URL) {
				return true
			}
		}
	}
	return false
}

func stageGCredentialedAuthSuccessIndex(observations []browserpkg.Observation) int {
	if index, _, ok := stageGCredentialedAuthNetworkSuccess(observations); ok {
		return index
	}
	return stageGCredentialedAuthTransitionIndex(observations)
}

func stageGObservedCredentialedAuthTransition(observations []browserpkg.Observation) bool {
	return stageGCredentialedAuthTransitionIndex(observations) >= 0
}

func stageGCredentialedAuthTransitionIndex(observations []browserpkg.Observation) int {
	credentialed := false
	submitted := false
	for index, observation := range observations {
		if credentialed && stageGObservationLooksSubmitAttempt(observation) {
			submitted = true
		}
		if stageGObservationLooksAuthGate(observation) {
			if stageGObservationHasCredentialEvidence(observation) {
				credentialed = true
			}
			continue
		}
		if submitted && stageGObservationLooksAuthenticated(observation) {
			return index
		}
	}
	return -1
}

func stageGAuthAcceptedNoTransitionEvidence(observations []browserpkg.Observation) string {
	successIndex, event, ok := stageGCredentialedAuthNetworkSuccess(observations)
	if !ok {
		return ""
	}
	if successIndex >= len(observations)-1 {
		return ""
	}
	for index := successIndex + 1; index < len(observations); index++ {
		if stageGObservationLooksAuthenticated(observations[index]) && !stageGObservationLooksAuthGate(observations[index]) {
			return ""
		}
	}
	last := observations[len(observations)-1]
	if !stageGObservationLooksAuthGate(last) {
		return ""
	}
	target := strings.TrimSpace(last.CurrentURL)
	if target == "" {
		target = "the login/register UI"
	}
	return fmt.Sprintf("Credentialed authentication request succeeded (%s status=%d), but browser remained on %s for %d later observation(s) with no authenticated product UI.", event.URL, event.Status, target, len(observations)-successIndex-1)
}

func stageGCredentialedAuthNetworkSuccess(observations []browserpkg.Observation) (int, browserpkg.NetworkEvent, bool) {
	credentialed := false
	submitted := false
	for index, observation := range observations {
		if stageGObservationLooksAuthGate(observation) && stageGObservationHasCredentialEvidence(observation) {
			credentialed = true
		}
		if credentialed && stageGObservationLooksSubmitAttempt(observation) {
			submitted = true
		}
		authenticatedTransition := stageGObservationLooksAuthenticated(observation) && !stageGObservationLooksAuthGate(observation)
		for _, event := range observation.NetworkEvents {
			if (submitted || authenticatedTransition) &&
				strings.ToUpper(strings.TrimSpace(event.Method)) == "POST" &&
				event.Status >= 200 && event.Status < 400 &&
				stageGNetworkURLLooksAuth(event.URL) &&
				!stageGNetworkURLLooksLogout(event.URL) {
				return index, event, true
			}
		}
	}
	return -1, browserpkg.NetworkEvent{}, false
}

func stageGObservationLooksAuthGate(observation browserpkg.Observation) bool {
	return frontende2e.ObservationLooksAuthGate(observation)
}

func stageGObservationLooksAuthenticated(observation browserpkg.Observation) bool {
	return frontende2e.ObservationLooksAuthenticated(observation)
}

func stageGObservationHasCredentialEvidence(observation browserpkg.Observation) bool {
	return frontende2e.ObservationHasCredentialEvidence(observation)
}

func stageGObservationHasPasswordControl(observation browserpkg.Observation) bool {
	return frontende2e.ObservationHasPasswordControl(observation)
}

func stageGObservationLooksInputAttempt(observation browserpkg.Observation) bool {
	return frontende2e.ObservationLooksInputAttempt(observation)
}

func stageGObservationLooksSubmitAttempt(observation browserpkg.Observation) bool {
	return frontende2e.ObservationLooksSubmitAttempt(observation)
}

func stageGObservationLooksCompletedAuthSubmitAttempt(observation browserpkg.Observation) bool {
	if !stageGObservationLooksSubmitAttempt(observation) {
		return false
	}
	if observation.OK {
		return true
	}
	_, _, ok := stageGObservationAuthClientFailure(observation)
	return ok
}

func stageGObservationCountsAsProgressAttempt(observation browserpkg.Observation) bool {
	switch strings.TrimSpace(observation.Action) {
	case "open_candidate", "click_button", "submit_local_form", "click_navigation", "go_back":
		return true
	default:
		return false
	}
}

func stageGPartialFailureEvidence(index int, observations []browserpkg.Observation, reason string) string {
	var parts []string
	if index < 0 || index >= len(observations) {
		return strings.TrimSpace(reason)
	}
	observation := observations[index]
	if observation.CurrentURL != "" {
		parts = append(parts, "URL: "+observation.CurrentURL)
	}
	for _, issue := range observation.NetworkIssues {
		if stageGNetworkIssueBlocksEvidence(index, issue, observations) && issue.Status >= 400 {
			parts = append(parts, fmt.Sprintf("%s status=%d", issue.URL, issue.Status))
		}
	}
	for _, event := range observation.NetworkEvents {
		if stageGNetworkEventBlocksEvidence(index, event, observations) && event.Status >= 400 {
			parts = append(parts, fmt.Sprintf("%s status=%d", event.URL, event.Status))
		}
	}
	if strings.TrimSpace(observation.Error) != "" {
		parts = append(parts, strings.TrimSpace(observation.Error))
	}
	if strings.TrimSpace(reason) != "" {
		parts = append(parts, strings.TrimSpace(reason))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("Observation %d showed a product failure.", index+1)
	}
	return strings.Join(parts, " ")
}
