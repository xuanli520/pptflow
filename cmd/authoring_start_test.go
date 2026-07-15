package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

func TestAuthoringStartCommandExposesOnlyClosedSourceSessionInputs(t *testing.T) {
	command, _, err := newAuthoringCommand(&lifecycleCLIConfig{root: t.TempDir()}).Find([]string{"start"})
	if err != nil || command == nil || command.Name() != "start" {
		t.Fatalf("find authoring start command: command=%v err=%v", command, err)
	}
	for _, required := range []string{"slug", "title", "metadata-json", "idempotency-key", "reason"} {
		if command.Flags().Lookup(required) == nil {
			t.Fatalf("authoring start is missing --%s", required)
		}
	}
	for _, forbidden := range []string{
		"repo", "repository", "commit", "source", "profile", "execution-spec", "model", "provider", "agent", "secret", "id", "parent-run", "execution-epoch",
	} {
		if command.Flags().Lookup(forbidden) != nil {
			t.Fatalf("authoring start exposes deployment-owned override --%s", forbidden)
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
		"--slug", "tower-http-authoring",
		"--title", "Tower HTTP authoring",
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
