package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/spf13/cobra"
)

func TestRunCommandsExposePublicDetachAndHideWorkerEndpoint(t *testing.T) {
	run := newRunCommandV2(&lifecycleCLIConfig{root: t.TempDir()})
	detach, _, err := run.Find([]string{"detach"})
	if err != nil || detach == nil || detach.Name() != "detach" {
		t.Fatalf("find public run detach command: command=%v err=%v", detach, err)
	}
	if detach.Hidden {
		t.Fatal("run detach must be an operator-facing command")
	}
	worker, _, err := run.Find([]string{"worker"})
	if err != nil || worker == nil || worker.Name() != "worker" {
		t.Fatalf("find controlled worker endpoint: command=%v err=%v", worker, err)
	}
	if !worker.Hidden {
		t.Fatal("run worker must remain an internal controlled-child endpoint")
	}
	for _, command := range []*cobra.Command{detach, worker} {
		for _, name := range []string{"run", "owner", "reason"} {
			if command.Flags().Lookup(name) == nil {
				t.Fatalf("run %s is missing --%s", command.Name(), name)
			}
		}
	}
	if worker.Flags().Lookup("detach") != nil {
		t.Fatal("hidden run worker endpoint must not expose the operator detach mode")
	}
	if detach.Flags().Lookup("idempotency-key") == nil {
		t.Fatal("run detach is missing its required handoff idempotency key")
	}
}

func TestRunDetachCommandUsesControlledChildLauncherAndValidatesRunIdentity(t *testing.T) {
	ctx := context.Background()
	root, run := newRunWorkerCommandFixture(t, ctx)
	launcher := &recordingRunWorkerLauncher{handoff: detachedRunWorkerHandoff{
		RunID: run.ID, Owner: "detach-owner", PID: 4242, StartedAt: time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC), LogPath: "/controlled/worker.log",
	}}
	command := newRunDetachCommandWithLauncher(&lifecycleCLIConfig{root: root}, launcher)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	idempotencyKey := commandLifecycleUUID(t)
	command.SetArgs([]string{"--run", run.ID, "--owner", "detach-owner", "--reason", "launch controlled child", "--idempotency-key", idempotencyKey})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatalf("run detach: %v\n%s", err, output.String())
	}
	var handoff store.RunWorkerHandoff
	if err := json.Unmarshal(output.Bytes(), &handoff); err != nil {
		t.Fatalf("decode detached worker handoff: %v\n%s", err, output.String())
	}
	if handoff.ID == "" || handoff.IdempotencyKey != idempotencyKey || handoff.RunID != run.ID || handoff.Owner != launcher.handoff.Owner || handoff.State != store.RunWorkerHandoffLaunching || handoff.ProcessID != launcher.handoff.PID || handoff.LogPath != launcher.handoff.LogPath || handoff.SpawnedAt == nil {
		t.Fatalf("detach handoff = %+v", handoff)
	}
	if len(launcher.requests) != 1 {
		t.Fatalf("launcher requests = %+v, want one request", launcher.requests)
	}
	if request := launcher.requests[0]; request.Root != root || request.RunID != run.ID || request.Owner != "detach-owner" || request.Reason != "launch controlled child" || request.HandoffOperationID != handoff.ID {
		t.Fatalf("detach request = %+v", request)
	}

	invalid := newRunDetachCommandWithLauncher(&lifecycleCLIConfig{root: root}, launcher)
	invalid.SetArgs([]string{"--run", "not-a-uuidv7", "--owner", "detach-owner", "--reason", "reject malformed run", "--idempotency-key", commandLifecycleUUID(t)})
	err := invalid.ExecuteContext(ctx)
	if !errors.Is(err, store.ErrInvalidUUIDv7Identity) {
		t.Fatalf("detach malformed run error = %v, want UUIDv7 validation failure", err)
	}
	if len(launcher.requests) != 1 {
		t.Fatalf("invalid run reached child launcher: %+v", launcher.requests)
	}
}

func TestRunWorkerSessionWithSignalsPersistsSIGINTBeforeStopping(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	actor := defaultLifecycleActor()
	if actor == "" {
		t.Skip("local OS actor is unavailable in this test environment")
	}
	services, run := newRunWorkerCommandServices(t, ctx, root, actor)
	defer services.Store().Close()
	run, err := services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: actor, Reason: "activate signal worker fixture",
	})
	if err != nil {
		t.Fatal(err)
	}

	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	var startOnce sync.Once
	session, err := app.NewRunWorkerSession(app.RunWorkerSessionConfig{
		Services: services, RunID: run.ID, Owner: "signal-worker", Actor: actor, Reason: "test controlled signal handoff",
		Handler: app.DurableJobHandlerFunc(func(context.Context, app.DurableJobExecution) (app.DurableJobResult, error) {
			startOnce.Do(func() { close(handlerStarted) })
			<-releaseHandler
			return app.DurableJobResult{State: store.JobSucceeded}, nil
		}),
		LeaseTTL: time.Second, HeartbeatEvery: 50 * time.Millisecond, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	signals := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() {
		_, runErr := runWorkerSessionWithSignals(ctx, session, signals)
		done <- runErr
	}()
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("controlled worker did not begin its durable job")
	}
	signals <- os.Interrupt
	operation := waitForRunWorkerControl(t, ctx, services, run.ID)
	if operation.Action != store.ControlActionPause || operation.Status != store.ControlOperationRequested {
		t.Fatalf("SIGINT durable control = %+v", operation)
	}
	current, err := services.Runs.Get(ctx, run.ID)
	if err != nil || current.Status != store.WorkflowRunPauseRequested {
		t.Fatalf("run after SIGINT = %+v, %v", current, err)
	}

	cancel()
	close(releaseHandler)
	select {
	case runErr := <-done:
		if !errors.Is(runErr, context.Canceled) {
			t.Fatalf("worker result after explicit shutdown = %v, want context cancellation", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after explicit process-context cancellation")
	}
}

func TestSignalControlAction(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		signal  os.Signal
		want    store.ControlAction
		handled bool
	}{
		{name: "interrupt pauses", signal: os.Interrupt, want: store.ControlActionPause, handled: true},
		{name: "terminate stops", signal: syscall.SIGTERM, want: store.ControlActionTerminate, handled: true},
		{name: "unhandled signal", signal: os.Kill, handled: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, handled := signalControlAction(testCase.signal)
			if got != testCase.want || handled != testCase.handled {
				t.Fatalf("signal control = (%q, %t), want (%q, %t)", got, handled, testCase.want, testCase.handled)
			}
		})
	}
}

type recordingRunWorkerLauncher struct {
	requests []detachedRunWorkerRequest
	handoff  detachedRunWorkerHandoff
	err      error
}

func (launcher *recordingRunWorkerLauncher) LaunchDetachedRunWorker(_ context.Context, request detachedRunWorkerRequest) (detachedRunWorkerHandoff, error) {
	launcher.requests = append(launcher.requests, request)
	if launcher.err != nil {
		return detachedRunWorkerHandoff{}, launcher.err
	}
	return launcher.handoff, nil
}

func newRunWorkerCommandFixture(t *testing.T, ctx context.Context) (string, store.WorkflowRun) {
	t.Helper()
	actor := defaultLifecycleActor()
	if actor == "" {
		t.Skip("local OS actor is unavailable in this test environment")
	}
	root := t.TempDir()
	services, run := newRunWorkerCommandServices(t, ctx, root, actor)
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}
	return root, run
}

func newRunWorkerCommandServices(t *testing.T, ctx context.Context, root, actor string) (*app.LifecycleServices, store.WorkflowRun) {
	t.Helper()
	services := openCommandLifecycle(t, root)
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{Slug: "run-worker-command", Actor: actor, Reason: "create controlled worker fixture"},
		SourceDirectory:        writeCommandTaskSnapshot(t, "controlled worker fixture\n"),
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: commandCompleteProfile(t), ExecutionSpec: commandExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "run-worker-command", Actor: actor, Reason: "start controlled worker fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	return services, run
}

func waitForRunWorkerControl(t *testing.T, ctx context.Context, services *app.LifecycleServices, runID string) store.DurableControlOperation {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		operations, err := services.Control.ListForRun(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		if len(operations) == 1 {
			return operations[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for durable signal control for run %s", runID)
	return store.DurableControlOperation{}
}
