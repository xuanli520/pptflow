package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestRootCommandUsesV2LifecycleHardCutover(t *testing.T) {
	root := NewRootCommand()
	available := make(map[string]bool)
	for _, command := range root.Commands() {
		available[command.Name()] = true
	}
	for _, name := range []string{"task", "revision", "run", "review", "release", "budget", "workspace", "tui", "doctor"} {
		if !available[name] {
			t.Fatalf("V2 root command %q is not registered: %v", name, available)
		}
	}
	for _, legacy := range []string{"environment", "lint", "publish", "repair", "repo-prepare", "status"} {
		if available[legacy] {
			t.Fatalf("legacy command %q remained registered after hard cutover", legacy)
		}
	}
	if root.PersistentFlags().Lookup("root") == nil {
		t.Fatal("V2 root command is missing the durable root flag")
	}
	if root.PersistentFlags().Lookup("actor") != nil {
		t.Fatal("V2 root command must derive audit actor from the local OS instead of accepting --actor")
	}
}

func TestRunCommandDoesNotExposeLegacyRetryOrCloneMutations(t *testing.T) {
	run := newRunCommandV2(&lifecycleCLIConfig{root: t.TempDir()})
	available := make(map[string]bool)
	for _, command := range run.Commands() {
		available[command.Name()] = true
	}
	for _, legacy := range []string{"retry-stage", "rerun", "resume"} {
		if available[legacy] {
			t.Fatalf("legacy run mutation %q remained registered: %v", legacy, available)
		}
	}
}

func TestLifecycleTUICommandHasNoLegacyRunnerFlags(t *testing.T) {
	command := newLifecycleTUICommand(&lifecycleCLIConfig{root: t.TempDir()})
	for _, legacy := range []string{"workspace", "workspace-root", "rescan", "task-concurrency", "auto-approve", "harbor-concurrency"} {
		if command.Flags().Lookup(legacy) != nil {
			t.Fatalf("V2 TUI exposes legacy runner flag %q", legacy)
		}
	}
}

func TestV2CommandGroupsRejectUnexpectedPositionalArguments(t *testing.T) {
	config := &lifecycleCLIConfig{root: t.TempDir()}
	for _, testCase := range []struct {
		name string
		new  func(*lifecycleCLIConfig) *cobra.Command
		args []string
	}{
		{name: "task", new: newTaskCommand, args: []string{"legacy-task-command"}},
		{name: "revision", new: newRevisionCommand, args: []string{"legacy-revision-command"}},
		{name: "review", new: newReviewCommand, args: []string{"legacy-review-command"}},
		{name: "run retry-stage", new: newRunCommandV2, args: []string{"retry-stage"}},
		{name: "run rerun", new: newRunCommandV2, args: []string{"rerun"}},
		{name: "release", new: newReleaseCommand, args: []string{"legacy-release-command"}},
		{name: "workspace", new: newWorkspaceCommand, args: []string{"legacy-workspace-command"}},
		{name: "budget", new: newBudgetCommand, args: []string{"legacy-budget-command"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			command := testCase.new(config)
			command.SetArgs(testCase.args)
			if err := command.Execute(); err == nil {
				t.Fatalf("%s accepted unexpected positional arguments %q", testCase.name, testCase.args)
			}
		})
	}
}

func TestV2CommandGroupsStillResolveKnownSubcommands(t *testing.T) {
	config := &lifecycleCLIConfig{root: t.TempDir()}
	for _, testCase := range []struct {
		name  string
		new   func(*lifecycleCLIConfig) *cobra.Command
		child string
	}{
		{name: "task import", new: newTaskCommand, child: "import"},
		{name: "task purge", new: newTaskCommand, child: "purge"},
		{name: "revision list", new: newRevisionCommand, child: "list"},
		{name: "review decide", new: newReviewCommand, child: "decide"},
		{name: "run start", new: newRunCommandV2, child: "start"},
		{name: "release package", new: newReleaseCommand, child: "package"},
		{name: "workspace list", new: newWorkspaceCommand, child: "list"},
		{name: "budget grant", new: newBudgetCommand, child: "grant"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			child, _, err := testCase.new(config).Find([]string{testCase.child})
			if err != nil || child == nil || child.Name() != testCase.child {
				t.Fatalf("known V2 child %q did not resolve: child=%v err=%v", testCase.child, child, err)
			}
		})
	}
}
