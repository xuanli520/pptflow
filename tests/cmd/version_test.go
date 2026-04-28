package cmd_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/cmd"
)

func TestVersionCommand(t *testing.T) {
	command := cmd.NewRootCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"version"})

	if err := command.Execute(); err != nil {
		t.Fatalf("version command failed: %v", err)
	}

	got := output.String()
	for _, want := range []string{"p2r ", "commit:", "built:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("version output %q does not contain %q", got, want)
		}
	}
}
