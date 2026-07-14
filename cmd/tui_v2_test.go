package cmd

import (
	"context"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/internal/tui"
)

func TestLifecycleTUICommandUsesConfiguredServiceFactory(t *testing.T) {
	root := t.TempDir()
	factoryCalls := 0
	runnerCalls := 0
	config := &lifecycleCLIConfig{
		root: root,
		newLifecycleService: func(factoryRoot string, database *store.Store) (*app.LifecycleServices, error) {
			factoryCalls++
			return app.NewLifecycleServicesWithOptions(factoryRoot, database, app.LifecycleServicesOptions{
				OperationResolver: testsupport.AcceptAllStageOperationResolver(),
			})
		},
	}
	command := newLifecycleTUICommandWithRunner(config, func(ctx context.Context, lifecycle tui.TaskHubLifecycleService) error {
		runnerCalls++
		if lifecycle == nil || ctx == nil {
			t.Fatal("TUI runner did not receive the composed lifecycle adapter")
		}
		return nil
	})
	if err := command.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("run TUI through composed lifecycle services: %v", err)
	}
	if factoryCalls != 1 || runnerCalls != 1 {
		t.Fatalf("composition calls = factory:%d runner:%d, want 1/1", factoryCalls, runnerCalls)
	}
}

func TestLifecycleTUICompositionEnablesPerRunWorkerHandoff(t *testing.T) {
	ctx := context.Background()
	services := openCommandLifecycle(t, t.TempDir())
	defer services.Store().Close()

	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{
			Slug: "tui-composition-handoff", Actor: "tester", Reason: "create TUI handoff fixture",
		},
		SourceDirectory: writeCommandTaskSnapshot(t, "TUI composition handoff fixture\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID,
		Profile: commandCompleteProfile(t), ExecutionSpec: commandExecutionSpec(task.ID, revision.ID, revision.TaskDigest),
		Trigger: "tui-composition-handoff", Actor: "tester", Reason: "start TUI handoff fixture",
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := newLifecycleTUIAdapter(services).QueryTaskHub(ctx, tui.TaskHubQuery{Tab: tui.TaskHubRunsTab})
	if err != nil {
		t.Fatal(err)
	}
	for _, projected := range snapshot.Runs {
		if projected.RunID != run.ID {
			continue
		}
		if !projected.Handoff.Enabled {
			t.Fatalf("TUI composition disabled active Run handoff: %+v", projected.Handoff)
		}
		return
	}
	t.Fatalf("TUI snapshot did not project Run %s", run.ID)
}
