package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

func TestAuthoringStartCommandExposesSourceCoordinateAndClosedExecutionInputs(t *testing.T) {
	command, _, err := newAuthoringCommand(&lifecycleCLIConfig{root: t.TempDir()}).Find([]string{"start"})
	if err != nil || command == nil || command.Name() != "start" {
		t.Fatalf("find authoring start command: command=%v err=%v", command, err)
	}
	for _, required := range []string{"repository-url", "commit-sha", "base-image", "slug", "title", "task-type", "application", "objective", "metadata-json", "idempotency-key", "reason"} {
		if command.Flags().Lookup(required) == nil {
			t.Fatalf("authoring start is missing --%s", required)
		}
	}
	for _, forbidden := range []string{
		"repo", "source", "profile", "execution-spec", "model", "provider", "agent", "secret", "id", "parent-run", "execution-epoch",
	} {
		if command.Flags().Lookup(forbidden) != nil {
			t.Fatalf("authoring start exposes deployment-owned override --%s", forbidden)
		}
	}
}

func TestAuthoringRecoverCommandExposesOnlyFrozenRecoveryInputs(t *testing.T) {
	command, _, err := newAuthoringCommand(&lifecycleCLIConfig{root: t.TempDir()}).Find([]string{"recover"})
	if err != nil || command == nil || command.Name() != "recover" {
		t.Fatalf("find authoring recover command: command=%v err=%v", command, err)
	}
	for _, required := range []string{"run", "idempotency-key", "reason", "dry-run"} {
		if command.Flags().Lookup(required) == nil {
			t.Fatalf("authoring recover is missing --%s", required)
		}
	}
	for _, forbidden := range []string{
		"model", "reasoning-effort", "profile", "execution-spec", "from-stage", "source", "repo", "id", "parent-run", "execution-epoch",
	} {
		if command.Flags().Lookup(forbidden) != nil {
			t.Fatalf("authoring recover exposes frozen-definition override --%s", forbidden)
		}
	}
}

func TestAuthoringStartCommandFailsClosedWithoutDeploymentCapabilityAndCreatesNoTask(t *testing.T) {
	root := t.TempDir()
	config := &lifecycleCLIConfig{
		root: root,
		newLifecycleService: func(factoryRoot string, database *store.Store) (*app.LifecycleServices, error) {
			return app.NewLifecycleServices(factoryRoot, database)
		},
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	command := newAuthoringCommand(config)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{
		"start",
		"--repository-url", "https://github.com/example/fixture-repository.git",
		"--commit-sha", "0123456789abcdef0123456789abcdef01234567",
		"--base-image", "docker.io/library/rust:1.65.0-bullseye@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"--slug", "fixture-authoring",
		"--title", "Fixture authoring",
		"--task-type", "feature",
		"--application", "backend",
		"--objective", "Add a bounded backend feature",
		"--metadata-json", `{"difficulty":"hard"}`,
		"--idempotency-key", key,
		"--reason", "verify closed deployment boundary",
	})
	err = command.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), app.ErrStandardAuthoringLaunchUnavailable.Error()) {
		t.Fatalf("uninjected authoring start error = %v, want %q", err, app.ErrStandardAuthoringLaunchUnavailable)
	}
	if strings.Contains(output.String(), `"task_id"`) || strings.Contains(output.String(), `"run_id"`) {
		t.Fatalf("uninjected authoring start wrote a mutation receipt: %q", output.String())
	}
	database, err := store.OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	tasks, err := database.ListTasksV2(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("uninjected authoring start created tasks: %+v", tasks)
	}
}

func TestAuthoringStartCommandRequiresBaseImage(t *testing.T) {
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	command := newAuthoringCommand(&lifecycleCLIConfig{root: t.TempDir()})
	command.SetArgs([]string{
		"start",
		"--repository-url", "https://github.com/example/fixture-repository.git",
		"--commit-sha", "0123456789abcdef0123456789abcdef01234567",
		"--slug", "fixture-authoring",
		"--title", "Fixture authoring",
		"--idempotency-key", key,
		"--reason", "verify immutable task environment is required",
	})
	if err := command.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "base-image is required") {
		t.Fatalf("missing base-image error = %v", err)
	}
}

func TestAuthoringStartCommandRequiresCompleteAuthoringBrief(t *testing.T) {
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	baseArgs := []string{
		"start",
		"--repository-url", "https://github.com/example/fixture-repository.git",
		"--commit-sha", "0123456789abcdef0123456789abcdef01234567",
		"--base-image", "docker.io/library/rust:1.65.0-bullseye@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"--slug", "fixture-authoring",
		"--title", "Fixture authoring",
		"--idempotency-key", key,
		"--reason", "verify immutable authoring brief is required",
	}
	for _, test := range []struct {
		name  string
		extra []string
		want  string
	}{
		{name: "task type", want: "task-type is required"},
		{name: "application", extra: []string{"--task-type", "feature"}, want: "application is required"},
		{name: "objective", extra: []string{"--task-type", "feature", "--application", "backend"}, want: "objective is required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := newAuthoringCommand(&lifecycleCLIConfig{root: t.TempDir()})
			command.SetArgs(append(append([]string(nil), baseArgs...), test.extra...))
			if err := command.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("incomplete authoring brief error = %v, want %q", err, test.want)
			}
		})
	}
}
