package cmd

import (
	"io"
	"testing"

	"github.com/spf13/cobra"
)

func TestRootCommandUsesV2LifecycleHardCutover(t *testing.T) {
	root := NewRootCommand()
	available := make(map[string]bool)
	for _, command := range root.Commands() {
		available[command.Name()] = true
	}
	for _, name := range []string{"task", "revision", "run", "review", "authoring", "release", "budget", "workspace", "tui", "doctor"} {
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

func TestDoctorCommandHasNoLegacyWorkspaceInput(t *testing.T) {
	command := newDoctorCommand()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	if command.Flags().Lookup("workspace") != nil {
		t.Fatal("doctor retains retired --workspace input")
	}
	command.SetArgs([]string{"--workspace", t.TempDir()})
	if err := command.Execute(); err == nil {
		t.Fatal("doctor accepted retired --workspace input")
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
		{name: "authoring", new: newAuthoringCommand, args: []string{"legacy-authoring-command"}},
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
		{name: "authoring review", new: newAuthoringCommand, child: "review"},
		{name: "authoring recover", new: newAuthoringCommand, child: "recover"},
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

func TestTaskCommandDoesNotExposeLegacyImport(t *testing.T) {
	command := newTaskCommand(&lifecycleCLIConfig{root: t.TempDir()})
	child, _, err := command.Find([]string{"import-legacy"})
	if err == nil && child != nil && child.Name() == "import-legacy" {
		t.Fatal("legacy import command remained registered")
	}
}

func TestTypedLifecycleMutationCommandsExposeUUIDv7KeysAndRetireUnownedIdentityFlags(t *testing.T) {
	config := &lifecycleCLIConfig{root: t.TempDir()}
	for _, testCase := range []struct {
		name string
		new  func(*lifecycleCLIConfig) *cobra.Command
		path []string
	}{
		{name: "task create", new: newTaskCommand, path: []string{"create"}},
		{name: "task import", new: newTaskCommand, path: []string{"import"}},
		{name: "task fork", new: newTaskCommand, path: []string{"fork"}},
		{name: "task archive", new: newTaskCommand, path: []string{"archive"}},
		{name: "task delete", new: newTaskCommand, path: []string{"delete"}},
		{name: "task restore", new: newTaskCommand, path: []string{"restore"}},
		{name: "run start", new: newRunCommandV2, path: []string{"start"}},
		{name: "review decide", new: newReviewCommand, path: []string{"decide"}},
		{name: "authoring review decide", new: newAuthoringCommand, path: []string{"review", "decide"}},
		{name: "authoring recover", new: newAuthoringCommand, path: []string{"recover"}},
		{name: "release package", new: newReleaseCommand, path: []string{"package"}},
		{name: "release withdraw", new: newReleaseCommand, path: []string{"withdraw"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			command, _, err := testCase.new(config).Find(testCase.path)
			if err != nil || command == nil {
				t.Fatalf("find command %q: %v", testCase.path, err)
			}
			if command.Flags().Lookup("idempotency-key") == nil {
				t.Fatalf("%s is missing its required lifecycle idempotency-key flag", testCase.name)
			}
		})
	}
	packageCommand, _, err := newReleaseCommand(config).Find([]string{"package"})
	if err != nil || packageCommand == nil || packageCommand.Flags().Lookup("run") == nil {
		t.Fatalf("release package must expose the explicit CodeEdge --run selector: command=%v err=%v", packageCommand, err)
	}

	for _, testCase := range []struct {
		name  string
		new   func(*lifecycleCLIConfig) *cobra.Command
		path  []string
		flags []string
	}{
		{name: "task create", new: newTaskCommand, path: []string{"create"}, flags: []string{"id"}},
		{name: "task import", new: newTaskCommand, path: []string{"import"}, flags: []string{"id"}},
		{name: "task fork", new: newTaskCommand, path: []string{"fork"}, flags: []string{"id"}},
		{name: "run start", new: newRunCommandV2, path: []string{"start"}, flags: []string{"id", "parent-run", "execution-epoch"}},
		{name: "release package", new: newReleaseCommand, path: []string{"package"}, flags: []string{"channel", "expected-channel-version"}},
	} {
		t.Run(testCase.name+" retired flags", func(t *testing.T) {
			command, _, err := testCase.new(config).Find(testCase.path)
			if err != nil || command == nil {
				t.Fatalf("find command %q: %v", testCase.path, err)
			}
			for _, flag := range testCase.flags {
				if command.Flags().Lookup(flag) != nil {
					t.Fatalf("%s retains an unowned legacy flag --%s", testCase.name, flag)
				}
			}
		})
	}
}
