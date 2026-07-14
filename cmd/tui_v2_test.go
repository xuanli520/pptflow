package cmd

import (
	"context"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/tui"
)

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
