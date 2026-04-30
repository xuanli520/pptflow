package preflight_test

import (
	"strings"
	"testing"
	_ "unsafe"

	_ "github.com/xuanli520/p2r_tui/internal/preflight"
)

//go:linkname validateExtraArgs github.com/xuanli520/p2r_tui/internal/preflight.validateExtraArgs
func validateExtraArgs(args []string) string

func TestValidateExtraArgsRejectsBoundaryFlags(t *testing.T) {
	if err := validateExtraArgs([]string{"--model", "gpt-5.4"}); err != "" {
		t.Fatalf("safe args rejected: %s", err)
	}
	for _, flag := range []string{"--full-auto", "--search", "--dangerously-bypass-approvals-and-sandbox"} {
		err := validateExtraArgs([]string{flag})
		if !strings.Contains(err, flag) {
			t.Fatalf("expected %s to be rejected, got %q", flag, err)
		}
	}
	if err := validateExtraArgs([]string{"--search=true"}); !strings.Contains(err, "--search") {
		t.Fatalf("expected --search=... to be rejected, got %q", err)
	}
}
