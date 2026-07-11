package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestTUICommandRejectsAutoApprove(t *testing.T) {
	cmd := newTUICommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--auto-approve"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected tui --auto-approve to fail")
	}
	if !strings.Contains(err.Error(), "manual review gates") {
		t.Fatalf("unexpected error: %v", err)
	}
}
