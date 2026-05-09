package preflight_test

import (
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/codex"
)

func TestValidateExtraArgsRejectsBoundaryFlags(t *testing.T) {
	if _, err := codex.ValidateAppServerExtraArgs([]string{"--model", "gpt-5.4"}); err != nil {
		t.Fatalf("safe args rejected: %s", err)
	}
	for _, flag := range []string{"--full-auto", "--search", "--dangerously-bypass-approvals-and-sandbox"} {
		_, err := codex.ValidateAppServerExtraArgs([]string{flag})
		if err == nil || !strings.Contains(err.Error(), flag) {
			t.Fatalf("expected %s to be rejected, got %v", flag, err)
		}
	}
	if _, err := codex.ValidateAppServerExtraArgs([]string{"--search=true"}); err == nil || !strings.Contains(err.Error(), "--search") {
		t.Fatalf("expected --search=... to be rejected, got %v", err)
	}
}
