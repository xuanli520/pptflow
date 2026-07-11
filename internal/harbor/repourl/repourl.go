package repourl

import (
	"fmt"
	"net/url"
	"strings"
)

func HasCredentials(raw string) bool {
	parsed, ok := parse(raw)
	if !ok || parsed.User == nil {
		return false
	}
	return true
}

func RejectCredentials(raw string) error {
	if HasCredentials(raw) {
		return fmt.Errorf("repo URL must not include credentials")
	}
	if HasQueryOrFragment(raw) {
		return fmt.Errorf("repo URL must not include query or fragment")
	}
	return nil
}

func HasQueryOrFragment(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	parsed, ok := parse(raw)
	if !ok {
		return strings.ContainsAny(raw, "?#")
	}
	return parsed.RawQuery != "" || parsed.Fragment != ""
}

func StripCredentials(raw string) string {
	raw = strings.TrimSpace(raw)
	parsed, ok := parse(raw)
	if !ok {
		if idx := strings.IndexAny(raw, "?#"); idx >= 0 {
			return strings.TrimSpace(raw[:idx])
		}
		return raw
	}
	copy := *parsed
	copy.User = nil
	copy.RawQuery = ""
	copy.Fragment = ""
	return copy.String()
}

func IsGitHubRepo(raw string) bool {
	_, _, ok := GitHubOwnerRepo(raw)
	return ok
}

func GitHubPublicHTTPSURL(raw string) (string, bool) {
	owner, repo, ok := GitHubOwnerRepo(raw)
	if !ok {
		return "", false
	}
	return "https://github.com/" + owner + "/" + repo + ".git", true
}

func GitHubOwnerRepo(raw string) (string, string, bool) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimSuffix(raw, ".git")
	if raw == "" {
		return "", "", false
	}
	if strings.HasPrefix(raw, "git@github.com:") {
		return splitOwnerRepo(strings.TrimPrefix(raw, "git@github.com:"))
	}
	if strings.HasPrefix(raw, "git@github.com/") {
		return splitOwnerRepo(strings.TrimPrefix(raw, "git@github.com/"))
	}
	parsed, ok := parse(raw)
	if !ok || !strings.EqualFold(parsed.Hostname(), "github.com") {
		return "", "", false
	}
	if parsed.Port() != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", false
	}
	return splitOwnerRepo(strings.Trim(parsed.Path, "/"))
}

func parse(raw string) (*url.URL, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.Contains(raw, "://") {
		return nil, false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil {
		return nil, false
	}
	return parsed, true
}

func splitOwnerRepo(path string) (string, string, bool) {
	if strings.ContainsAny(path, "?#") {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 {
		return "", "", false
	}
	owner := strings.TrimSpace(parts[0])
	repo := strings.TrimSuffix(strings.TrimSpace(parts[1]), ".git")
	if owner == "" || repo == "" || strings.ContainsAny(owner+repo, " \t\r\n") {
		return "", "", false
	}
	return owner, repo, true
}
