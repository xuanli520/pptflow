package frontende2e

import (
	"fmt"
	"net/url"
	"strings"

	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
)

func ObservationLooksAuthGate(observation browserpkg.Observation) bool {
	urlValue := strings.ToLower(strings.TrimSpace(observation.CurrentURL))
	for _, marker := range []string{"/login", "/signin", "/sign-in", "/register", "/signup", "/sign-up", "/auth"} {
		if strings.Contains(urlValue, marker) {
			return true
		}
	}
	text := strings.ToLower(strings.TrimSpace(observation.VisibleText))
	if text == "" {
		return false
	}
	if strings.Contains(text, "captcha") {
		return true
	}
	hasPassword := strings.Contains(text, "password") || ObservationHasPasswordControl(observation)
	hasAuthLabel := strings.Contains(text, "login") ||
		strings.Contains(text, "log in") ||
		strings.Contains(text, "sign in") ||
		strings.Contains(text, "signin") ||
		strings.Contains(text, "register") ||
		strings.Contains(text, "create account")
	return hasPassword && hasAuthLabel && !ObservationLooksAuthenticated(observation)
}

func ObservationLooksAuthenticated(observation browserpkg.Observation) bool {
	urlValue := strings.ToLower(strings.TrimSpace(observation.CurrentURL))
	for _, marker := range []string{"/dashboard", "/app", "/studio", "/account", "/profile", "/orders", "/cart", "/analytics", "/catalog"} {
		if strings.Contains(urlValue, marker) {
			return true
		}
	}
	text := strings.ToLower(strings.TrimSpace(observation.VisibleText))
	if text == "" {
		return false
	}
	for _, marker := range []string{"dashboard", "logout", "log out", "sign out", "user management"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func ObservationHasCredentialEvidence(observation browserpkg.Observation) bool {
	hasPasswordValue := false
	hasAccountValue := false
	for _, control := range observation.Controls {
		if !control.HasValue {
			continue
		}
		target := strings.ToLower(strings.Join([]string{control.Role, control.Text, control.Name, control.Placeholder, control.Type}, " "))
		if strings.Contains(target, "password") || strings.Contains(target, "passwd") {
			hasPasswordValue = true
			continue
		}
		for _, marker := range []string{"email", "user", "username", "account", "login"} {
			if strings.Contains(target, marker) {
				hasAccountValue = true
				break
			}
		}
	}
	return hasPasswordValue && hasAccountValue
}

func ObservationHasPasswordControl(observation browserpkg.Observation) bool {
	for _, control := range observation.Controls {
		target := strings.ToLower(strings.Join([]string{control.Role, control.Text, control.Name, control.Placeholder, control.Type}, " "))
		if strings.Contains(target, "password") || strings.Contains(target, "passwd") {
			return true
		}
	}
	return false
}

func ObservationLooksInputAttempt(observation browserpkg.Observation) bool {
	return strings.TrimSpace(observation.Action) == "fill_input"
}

func ObservationLooksSubmitAttempt(observation browserpkg.Observation) bool {
	switch strings.TrimSpace(observation.Action) {
	case "click_button", "submit_local_form":
		return true
	default:
		return false
	}
}

func CredentialedAuthClientFailureCount(observations []browserpkg.Observation) int {
	count := 0
	for _, observation := range observations {
		if !ObservationLooksAuthGate(observation) {
			continue
		}
		if !ObservationLooksSubmitAttempt(observation) && !ObservationHasCredentialEvidence(observation) {
			continue
		}
		if _, _, ok := ObservationAuthClientFailure(observation); ok {
			count++
		}
	}
	return count
}

func ObservationAuthClientFailure(observation browserpkg.Observation) (string, int, bool) {
	for _, issue := range observation.NetworkIssues {
		if NetworkURLLooksAuth(issue.URL) && RecoverableAuthClientStatus(issue.Status) && !NetworkURLLooksLogout(issue.URL) {
			return issue.URL, issue.Status, true
		}
	}
	for _, event := range observation.NetworkEvents {
		if NetworkURLLooksAuth(event.URL) && RecoverableAuthClientStatus(event.Status) && !NetworkURLLooksLogout(event.URL) {
			return event.URL, event.Status, true
		}
	}
	return "", 0, false
}

func ObservationAuthFailureLooksCaptchaBoundary(observation browserpkg.Observation, raw string, status int) bool {
	if !RecoverableAuthClientStatus(status) || !NetworkURLLooksAuth(raw) || NetworkURLLooksLogout(raw) {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(observation.VisibleText))
	return strings.Contains(text, "captcha")
}

func NetworkFailureLooksAuthGateNoise(index int, raw string, status int, observations []browserpkg.Observation) bool {
	if index < 0 || index >= len(observations) {
		return false
	}
	switch status {
	case 401, 403:
	default:
		return false
	}
	observation := observations[index]
	if !ObservationLooksAuthGate(observation) {
		return false
	}
	if ObservationLooksSubmitAttempt(observation) || ObservationHasCredentialEvidence(observation) {
		return false
	}
	if NetworkURLLooksLogout(raw) {
		return false
	}
	return true
}

func NetworkFailureLooksPendingAuthRetry(index int, raw string, status int, observations []browserpkg.Observation) bool {
	if index < 0 || index >= len(observations) {
		return false
	}
	if !RecoverableAuthClientStatus(status) || !NetworkURLLooksAuth(raw) || NetworkURLLooksLogout(raw) {
		return false
	}
	observation := observations[index]
	if !ObservationLooksAuthGate(observation) {
		return false
	}
	if ObservationAuthFailureLooksCaptchaBoundary(observation, raw, status) {
		return false
	}
	if !ObservationLooksSubmitAttempt(observation) && !ObservationHasCredentialEvidence(observation) {
		return false
	}
	return CredentialedAuthClientFailureCount(observations) < AuthGateSubmitStallLimit
}

func NetworkURLLooksAuth(raw string) bool {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return false
	}
	parsed, err := url.Parse(raw)
	target := raw
	if err == nil {
		target = strings.ToLower(parsed.Path)
	}
	for _, keyword := range []string{"auth", "login", "signin", "sign-in", "session", "token"} {
		if strings.Contains(target, keyword) {
			return true
		}
	}
	return false
}

func NetworkURLLooksLogout(raw string) bool {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return false
	}
	parsed, err := url.Parse(raw)
	target := raw
	if err == nil {
		target = strings.ToLower(parsed.Path)
	}
	for _, keyword := range []string{"logout", "log-out", "log_off", "logoff", "signout", "sign-out", "sign_off", "signoff"} {
		if strings.Contains(target, keyword) {
			return true
		}
	}
	return browserActionTextEndsSession(target)
}

func NetworkURLKey(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return strings.ToLower(raw)
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.ToLower(parsed.String())
}

func RecoverableAuthClientStatus(status int) bool {
	switch status {
	case 400, 401, 403, 422:
		return true
	default:
		return false
	}
}

func AuthClientFailureEvidence(urlValue string, status int) string {
	return fmt.Sprintf("%s status=%d", urlValue, status)
}
