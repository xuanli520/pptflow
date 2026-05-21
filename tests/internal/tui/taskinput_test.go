package tui_test

import (
	"strings"
	"testing"

	tuiapp "github.com/xuanli520/p2r_tui/internal/tui"
)

func TestValidateTaskID(t *testing.T) {
	got, err := tuiapp.ValidateTaskID("  TASK-20260521-ABCDEF  ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "TASK-20260521-ABCDEF" {
		t.Fatalf("task id = %q", got)
	}

	for _, value := range []string{
		"TASK-20260521-abcdef",
		"TASK-20260521-ABCDE",
		"TASK-2026052-ABCDEF",
		"TASK-20260521-ABCDEG",
		"TASK-20260521-ABCDEF/../../x",
		strings.Repeat("A", 65),
	} {
		if _, err := tuiapp.ValidateTaskID(value); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}
