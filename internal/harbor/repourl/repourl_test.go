package repourl

import "testing"

func TestRejectCredentials(t *testing.T) {
	cases := map[string]bool{
		"https://github.com/org/repo.git":             false,
		"git@github.com:org/repo.git":                 false,
		"/tmp/local/repo":                             false,
		"ssh://git@github.com/org/repo.git":           true,
		"https://token@github.com/org/repo.git":       true,
		"https://user:pass@github.com/org/repo.git":   true,
		"ssh://git:secret@github.com/org/repo.git":    true,
		"ssh://token@github.com/org/repo.git":         true,
		"https://x-token:secret@example.com/org/repo": true,
		"https://github.com/org/repo?token=secret":    true,
		"https://github.com/org/repo#token":           true,
		"git@github.com:org/repo?token=secret":        true,
		"/tmp/local/repo#token":                       true,
	}
	for raw, wantReject := range cases {
		err := RejectCredentials(raw)
		if wantReject && err == nil {
			t.Fatalf("expected credential rejection for %q", raw)
		}
		if !wantReject && err != nil {
			t.Fatalf("unexpected credential rejection for %q: %v", raw, err)
		}
	}
}

func TestStripCredentials(t *testing.T) {
	got := StripCredentials("https://user:pass@github.com/org/repo.git")
	if got != "https://github.com/org/repo.git" {
		t.Fatalf("StripCredentials = %q", got)
	}
	if got := StripCredentials("https://github.com/org/repo?token=secret#frag"); got != "https://github.com/org/repo" {
		t.Fatalf("query/fragment should be stripped, got %q", got)
	}
	if got := StripCredentials("git@github.com:org/repo?token=secret"); got != "git@github.com:org/repo" {
		t.Fatalf("scp-style query should be stripped, got %q", got)
	}
	if got := StripCredentials("git@github.com:org/repo.git"); got != "git@github.com:org/repo.git" {
		t.Fatalf("scp-style URL should be unchanged, got %q", got)
	}
}

func TestIsGitHubRepo(t *testing.T) {
	cases := map[string]bool{
		"https://github.com/org/repo":        true,
		"https://github.com/org/repo.git":    true,
		"git@github.com:org/repo.git":        true,
		"ssh://git@github.com/org/repo.git":  true,
		"ssh://git@github.com:22/org/repo":   false,
		"https://gitlab.com/org/repo":        false,
		"/tmp/local/repo":                    false,
		"https://github.com/org":             false,
		"https://github.com/org/repo/issues": false,
		"https://github.com/org/repo?x=1":    false,
		"https://github.com/org/repo#main":   false,
		"https://github.com/org/re po":       false,
	}
	for raw, want := range cases {
		if got := IsGitHubRepo(raw); got != want {
			t.Fatalf("IsGitHubRepo(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestGitHubPublicHTTPSURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/org/repo":       "https://github.com/org/repo.git",
		"https://github.com/org/repo.git":   "https://github.com/org/repo.git",
		"git@github.com:org/repo.git":       "https://github.com/org/repo.git",
		"ssh://git@github.com/org/repo.git": "https://github.com/org/repo.git",
	}
	for raw, want := range cases {
		got, ok := GitHubPublicHTTPSURL(raw)
		if !ok || got != want {
			t.Fatalf("GitHubPublicHTTPSURL(%q) = %q, %v; want %q, true", raw, got, ok, want)
		}
	}
	if got, ok := GitHubPublicHTTPSURL("https://gitlab.com/org/repo"); ok || got != "" {
		t.Fatalf("GitHubPublicHTTPSURL should reject non-GitHub URL, got %q %v", got, ok)
	}
	if got, ok := GitHubPublicHTTPSURL("ssh://git@github.com:22/org/repo.git"); ok || got != "" {
		t.Fatalf("GitHubPublicHTTPSURL should reject port-qualified SSH URL, got %q %v", got, ok)
	}
}
