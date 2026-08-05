package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	runWorkerSessionReadyTimeout  = 5 * time.Second
	runWorkerSessionExitTimeout   = 3 * time.Second
	runWorkerFenceLossStopTimeout = 2 * time.Second
)

func TestRunWorkerSessionFencesOneRunAndReleasesItsSupervisorLease(t *testing.T) {
	ctx := context.Background()
	_, services, _, _, run := newLocalRuntimeServiceFixture(t, "run-worker-owner")
	handler := DurableJobHandlerFunc(func(context.Context, DurableJobExecution) (DurableJobResult, error) {
		return DurableJobResult{State: store.JobSucceeded}, nil
	})
	first, err := NewRunWorkerSession(RunWorkerSessionConfig{
		Services: services, RunID: run.ID, Owner: "run-worker-a", Actor: "run-worker-owner", Reason: "test controlled child worker",
		Handler: handler,
	})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := first.Run(workerCtx)
		done <- err
	}()
	lease := waitForRunWorkerLease(t, ctx, services, run.ID, true)
	if lease.Owner != "run-worker-a" || lease.ResourceType != RunWorkerLeaseResourceType || lease.ResourceID != run.ID {
		t.Fatalf("run worker supervisor lease = %+v", lease)
	}

	second, err := NewRunWorkerSession(RunWorkerSessionConfig{
		Services: services, RunID: run.ID, Owner: "run-worker-b", Actor: "run-worker-owner", Reason: "test competing child worker",
		Handler: handler,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Run(context.Background()); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("second run worker error = %v, want held supervisor lease", err)
	}
	waitForRunWorkerInitialDelivery(t, ctx, services, run.ID)

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first worker result = %v, want context cancellation", err)
		}
	case <-time.After(runWorkerSessionExitTimeout):
		t.Fatal("first worker did not leave after its process context was canceled")
	}
	waitForRunWorkerLease(t, ctx, services, run.ID, false)
}

func TestRunWorkerSessionConsumesReservedHandoffLeaseAndReleasesIt(t *testing.T) {
	ctx := context.Background()
	_, services, _, _, run := newLocalRuntimeServiceFixture(t, "run-worker-handoff")
	operationID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := services.WorkerHandoffs.ReserveRunWorkerHandoff(ctx, ReserveRunWorkerHandoffCommand{
		ID: operationID, IdempotencyKey: key, RunID: run.ID,
		Expected: RunWorkerHandoffCheckpoint{RunVersion: run.Version, ExecutionEpoch: run.ExecutionEpoch, DefinitionHash: run.DefinitionHash},
		Owner:    "handoff-child", Actor: "run-worker-handoff", Reason: "handoff session integration", LaunchTTL: time.Minute,
	})
	if err != nil || !reserved.Launch || reserved.Handoff.State != store.RunWorkerHandoffLaunching {
		t.Fatalf("reserve controlled handoff = %+v, %v", reserved, err)
	}
	replayed, err := services.WorkerHandoffs.ReserveRunWorkerHandoff(ctx, ReserveRunWorkerHandoffCommand{
		ID: operationID, IdempotencyKey: key, RunID: run.ID,
		Expected: RunWorkerHandoffCheckpoint{RunVersion: run.Version, ExecutionEpoch: run.ExecutionEpoch, DefinitionHash: run.DefinitionHash},
		Owner:    "handoff-child", Actor: "run-worker-handoff", Reason: "handoff session integration", LaunchTTL: time.Minute,
	})
	if err != nil || !replayed.Replayed || replayed.Launch {
		t.Fatalf("reserve handoff replay = %+v, %v", replayed, err)
	}

	session, err := NewRunWorkerSession(RunWorkerSessionConfig{
		Services: services, RunID: run.ID, Owner: "handoff-child", Actor: "run-worker-handoff", Reason: "handoff child worker",
		HandoffOperationID: operationID, HandoffProcessID: 4242, HandoffLogPath: "/managed/worker.log",
		Handler: DurableJobHandlerFunc(func(context.Context, DurableJobExecution) (DurableJobResult, error) {
			return DurableJobResult{State: store.JobSucceeded}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		result RunWorkerSessionResult
		err    error
	}, 1)
	go func() {
		result, runErr := session.Run(workerCtx)
		done <- struct {
			result RunWorkerSessionResult
			err    error
		}{result: result, err: runErr}
	}()

	claimed := waitForRunWorkerHandoffState(t, ctx, services, run.ID, store.RunWorkerHandoffHandedOff)
	if claimed.WorkerLeaseID == "" || claimed.WorkerLeaseOwner != "handoff-child" {
		t.Fatalf("claimed handoff = %+v", claimed)
	}
	leases, err := services.Store().ListLeasesForResource(ctx, RunWorkerLeaseResourceType, run.ID)
	if err != nil || len(leases) != 1 || leases[0].ID != claimed.WorkerLeaseID || leases[0].Owner != "handoff-child" {
		t.Fatalf("handoff worker lease = %+v, %v", leases, err)
	}
	waitForRunWorkerInitialDelivery(t, ctx, services, run.ID)

	cancel()
	select {
	case completed := <-done:
		if !errors.Is(completed.err, context.Canceled) {
			t.Fatalf("handoff worker result = %+v, %v", completed.result, completed.err)
		}
		if completed.result.Handoff == nil || completed.result.Handoff.ID != operationID {
			t.Fatalf("handoff worker result omitted claimed operation: %+v", completed.result)
		}
	case <-time.After(runWorkerSessionExitTimeout):
		t.Fatal("handoff worker did not stop after process context cancellation")
	}
	released := waitForRunWorkerHandoffState(t, ctx, services, run.ID, store.RunWorkerHandoffReleased)
	if released.ID != operationID || released.WorkerLeaseID != claimed.WorkerLeaseID {
		t.Fatalf("released handoff = %+v, want %s", released, operationID)
	}
}

func TestRunWorkerSessionSignalControlsAreDurableIdempotentAndTerminateWins(t *testing.T) {
	ctx := context.Background()
	_, services, _, _, run := newLocalRuntimeServiceFixture(t, "run-worker-signal")
	var mu sync.Mutex
	keys := []string{}
	session, err := NewRunWorkerSession(RunWorkerSessionConfig{
		Services: services, RunID: run.ID, Owner: "run-worker-signal-owner", Actor: "run-worker-signal", Reason: "test signal control",
		Handler: DurableJobHandlerFunc(func(context.Context, DurableJobExecution) (DurableJobResult, error) {
			return DurableJobResult{State: store.JobSucceeded}, nil
		}),
		NewOperationKey: func() (string, error) {
			key, err := store.NewUUIDv7()
			if err == nil {
				mu.Lock()
				keys = append(keys, key)
				mu.Unlock()
			}
			return key, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	pause, err := session.RequestSignalControl(ctx, store.ControlActionPause)
	if err != nil || pause.Status != store.ControlOperationRequested || pause.Action != store.ControlActionPause {
		t.Fatalf("SIGINT pause operation = %+v, %v", pause, err)
	}
	replayedPause, err := session.RequestSignalControl(ctx, store.ControlActionPause)
	if err != nil || replayedPause.ID != pause.ID {
		t.Fatalf("repeated SIGINT = %+v, %v, want operation %s", replayedPause, err, pause.ID)
	}

	terminate, err := session.RequestSignalControl(ctx, store.ControlActionTerminate)
	if err != nil || terminate.Status != store.ControlOperationRequested || terminate.Action != store.ControlActionTerminate {
		t.Fatalf("SIGTERM terminate operation = %+v, %v", terminate, err)
	}
	if terminate.ID == pause.ID {
		t.Fatal("SIGTERM reused the weaker pause operation")
	}
	postTerminatePause, err := session.RequestSignalControl(ctx, store.ControlActionPause)
	if err != nil || postTerminatePause.ID != terminate.ID {
		t.Fatalf("pause after SIGTERM = %+v, %v, want terminate operation %s", postTerminatePause, err, terminate.ID)
	}
	mu.Lock()
	keyCount := len(keys)
	mu.Unlock()
	if keyCount != 2 {
		t.Fatalf("signal operation keys = %d, want one pause and one terminate", keyCount)
	}
	current, err := services.Runs.Get(ctx, run.ID)
	if err != nil || current.Status != store.WorkflowRunCancelRequested {
		t.Fatalf("run after SIGTERM = %+v, %v", current, err)
	}
}

func TestRunWorkerSessionSignalControlsReplayAcrossControlledChildRestarts(t *testing.T) {
	ctx := context.Background()
	_, services, _, _, run := newLocalRuntimeServiceFixture(t, "run-worker-signal-restart")
	newSession := func(owner string, calls *int) *RunWorkerSession {
		t.Helper()
		session, err := NewRunWorkerSession(RunWorkerSessionConfig{
			Services: services, RunID: run.ID, Owner: owner, Actor: "run-worker-signal-restart", Reason: "test restarted child signal control",
			Handler: DurableJobHandlerFunc(func(context.Context, DurableJobExecution) (DurableJobResult, error) {
				return DurableJobResult{State: store.JobSucceeded}, nil
			}),
			NewOperationKey: func() (string, error) {
				*calls++
				return store.NewUUIDv7()
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	firstCalls := 0
	pause, err := newSession("run-worker-first", &firstCalls).RequestSignalControl(ctx, store.ControlActionPause)
	if err != nil || firstCalls != 1 {
		t.Fatalf("first child pause = %+v, %v, key calls=%d", pause, err, firstCalls)
	}
	secondCalls := 0
	replayedPause, err := newSession("run-worker-second", &secondCalls).RequestSignalControl(ctx, store.ControlActionPause)
	if err != nil || replayedPause.ID != pause.ID || secondCalls != 0 {
		t.Fatalf("restarted child pause = %+v, %v, key calls=%d; want original %s without a new key", replayedPause, err, secondCalls, pause.ID)
	}

	terminate, err := newSession("run-worker-second", &secondCalls).RequestSignalControl(ctx, store.ControlActionTerminate)
	if err != nil || terminate.ID == pause.ID || secondCalls != 1 {
		t.Fatalf("restarted child terminate = %+v, %v, key calls=%d", terminate, err, secondCalls)
	}
	thirdCalls := 0
	postTerminatePause, err := newSession("run-worker-third", &thirdCalls).RequestSignalControl(ctx, store.ControlActionPause)
	if err != nil || postTerminatePause.ID != terminate.ID || thirdCalls != 0 {
		t.Fatalf("restarted child pause after terminate = %+v, %v, key calls=%d; want terminate %s", postTerminatePause, err, thirdCalls, terminate.ID)
	}
}

func TestRunWorkerSessionKeepsSupervisorThroughDispatchLeaseLossRecovery(t *testing.T) {
	ctx := context.Background()
	fixture := newFrozenRuntimeFixture(t)
	defer fixture.store.Close()
	stageStarted := make(chan struct{})
	var stageStartedOnce sync.Once
	runtime := newFrozenRuntime(t, fixture.services, frozenRuntimeRegistry(t, fixture.frozen.Workflow, func(ctx context.Context, _ workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
		stageStartedOnce.Do(func() { close(stageStarted) })
		<-ctx.Done()
		return workflowkit.StageExecutionResult{}, ctx.Err()
	}))
	session, err := NewRunWorkerSession(RunWorkerSessionConfig{
		Services: fixture.services, RunID: fixture.run.ID, Owner: "run-worker-lease-loss", Actor: runtimeFixtureActor,
		Reason: "test automatic dispatch lease-loss recovery", Handler: runtime,
		LeaseTTL: 750 * time.Millisecond, HeartbeatEvery: 25 * time.Millisecond, PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	actualHeartbeat := session.worker.heartbeatLease
	session.worker.heartbeatLease = func(ctx context.Context, request store.HeartbeatLeaseRequest) (store.Lease, error) {
		select {
		case <-stageStarted:
			return store.Lease{}, errors.New("test transient store outage")
		default:
			return actualHeartbeat(ctx, request)
		}
	}
	result, err := session.Run(ctx)
	if err != nil {
		t.Fatalf("lease-loss session result = %+v, %v", result, err)
	}
	if result.StoppedFor != store.WorkflowRunFailedRecoverable || result.Run.Status != store.WorkflowRunFailedRecoverable {
		t.Fatalf("lease-loss session did not converge = %+v", result)
	}
	job, payload := requireRuntimeStageJob(t, ctx, fixture.store, fixture.run.ID, runtimeFixtureSourceStage)
	persistedJob, err := fixture.store.GetDurableJob(ctx, job.ID)
	if err != nil || persistedJob == nil || persistedJob.State != store.JobInterrupted || persistedJob.Failure != nil {
		t.Fatalf("lease-loss session job = %+v, %v", persistedJob, err)
	}
	events, err := fixture.store.ListAuditEvents(ctx, store.ListAuditEventsRequest{EntityType: "job", EntityID: job.ID})
	if err != nil {
		t.Fatal(err)
	}
	settled := 0
	for _, event := range events {
		if event.Action == "job.settled" && strings.Contains(event.PayloadJSON, `"failure_code":"job.lease_lost"`) {
			settled++
		}
	}
	if settled != 1 {
		t.Fatalf("lease-loss settle audit events = %d, want 1; events = %+v", settled, events)
	}
	stage, err := fixture.store.GetStageAttempt(ctx, payload.StageAttemptID)
	if err != nil || stage == nil || stage.ExecutionStatus != store.StageExecutionInterrupted {
		t.Fatalf("lease-loss session stage = %+v, %v", stage, err)
	}
	nodes, err := fixture.store.ListNodeAttempts(ctx, payload.StageAttemptID)
	if err != nil || len(nodes) != 1 || nodes[0].Status != store.NodeAttemptInterrupted {
		t.Fatalf("lease-loss session nodes = %+v, %v", nodes, err)
	}
}

func TestRunWorkerSupervisorRetriesTransientHeartbeat(t *testing.T) {
	ctx := context.Background()
	dataStore, err := store.OpenForTest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	lease, err := dataStore.AcquireLease(ctx, store.AcquireLeaseRequest{
		ResourceType: RunWorkerLeaseResourceType, ResourceID: "heartbeat-retry", Owner: "worker", TTL: time.Second, Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	session := &RunWorkerSession{
		owner: "worker", actor: "tester", reason: "heartbeat retry", leaseTTL: time.Second, heartbeatEvery: 20 * time.Millisecond,
		heartbeatLease: dataStore.HeartbeatLease, getLease: dataStore.GetLease,
	}
	actualHeartbeat := session.heartbeatLease
	calls := 0
	session.heartbeatLease = func(ctx context.Context, request store.HeartbeatLeaseRequest) (store.Lease, error) {
		calls++
		if calls == 1 {
			return store.Lease{}, transientSQLiteFixtureError{code: 5}
		}
		return actualHeartbeat(ctx, request)
	}
	heartbeats := newRunWorkerLeaseHeartbeats(session, lease)
	if err := heartbeats.heartbeat(); err != nil || calls != 2 || heartbeats.latest().Version != lease.Version+1 {
		t.Fatalf("supervisor heartbeat retry calls=%d lease=%+v err=%v", calls, heartbeats.latest(), err)
	}
}

func TestRunWorkerSupervisorFenceLossCancelsActiveExecution(t *testing.T) {
	id, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	lease := store.Lease{ID: id, Owner: "worker", FencingToken: 1, State: store.LeaseActive, Version: 1, ExpiresAt: time.Now().Add(time.Second)}
	canceled := make(chan struct{})
	var cancelOnce sync.Once
	worker := &DurableWorker{}
	worker.setActiveExecutionCancel(func() { cancelOnce.Do(func() { close(canceled) }) })
	session := &RunWorkerSession{
		owner: "worker", actor: "tester", reason: "fence loss", worker: worker,
		leaseTTL: 30 * time.Millisecond, heartbeatEvery: 10 * time.Millisecond,
		heartbeatLease: func(context.Context, store.HeartbeatLeaseRequest) (store.Lease, error) {
			return store.Lease{}, store.ErrFencingToken
		},
		getLease: func(context.Context, string) (*store.Lease, error) { return &lease, nil },
	}
	heartbeats := newRunWorkerLeaseHeartbeats(session, lease)
	heartbeats.start()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("supervisor fence loss did not cancel active execution")
	}
	heartbeats.stop()
	var heartbeatErr *LeaseHeartbeatError
	if !errors.As(heartbeats.err(), &heartbeatErr) || heartbeatErr.Class != LeaseHeartbeatFenceInvalid {
		t.Fatalf("supervisor heartbeat error = %v", heartbeats.err())
	}
}

func TestRunWorkerSupervisorFenceLossStopsLongStage(t *testing.T) {
	ctx := context.Background()
	fixture := newFrozenRuntimeFixture(t)
	defer fixture.store.Close()
	stageStarted := make(chan struct{})
	var stageOnce sync.Once
	runtime := newFrozenRuntime(t, fixture.services, frozenRuntimeRegistry(t, fixture.frozen.Workflow, func(ctx context.Context, _ workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
		stageOnce.Do(func() { close(stageStarted) })
		<-ctx.Done()
		return workflowkit.StageExecutionResult{}, ctx.Err()
	}))
	session, err := NewRunWorkerSession(RunWorkerSessionConfig{
		Services: fixture.services, RunID: fixture.run.ID, Owner: "supervisor-fence-loss", Actor: runtimeFixtureActor,
		Reason: "test supervisor fence loss cancellation", Handler: runtime,
		LeaseTTL: 500 * time.Millisecond, HeartbeatEvery: 20 * time.Millisecond, PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	actualHeartbeat := session.heartbeatLease
	session.heartbeatLease = func(ctx context.Context, request store.HeartbeatLeaseRequest) (store.Lease, error) {
		select {
		case <-stageStarted:
			return store.Lease{}, store.ErrFencingToken
		default:
			return actualHeartbeat(ctx, request)
		}
	}
	startedAt := time.Now()
	_, err = session.Run(ctx)
	if !errors.Is(err, ErrRunWorkerLeaseLost) {
		t.Fatalf("supervisor fence loss result = %v, want lease lost", err)
	}
	if elapsed := time.Since(startedAt); elapsed >= runWorkerFenceLossStopTimeout {
		t.Fatalf("supervisor fence loss took %s to stop long stage", elapsed)
	}
	_, payload := requireRuntimeStageJob(t, ctx, fixture.store, fixture.run.ID, runtimeFixtureSourceStage)
	stage, err := fixture.store.GetStageAttempt(ctx, payload.StageAttemptID)
	if err != nil || stage == nil || (stage.ExecutionStatus != store.StageExecutionInterrupted && stage.ExecutionStatus != store.StageExecutionInfraFailed) {
		t.Fatalf("stage after supervisor fence loss = %+v, %v", stage, err)
	}
}

func TestLocalRuntimeAttachAndReconcileIncludeControlledWorkerLease(t *testing.T) {
	ctx := context.Background()
	_, services, _, _, run := newLocalRuntimeServiceFixture(t, "run-worker-reconcile")
	lease, err := services.Store().AcquireLease(ctx, store.AcquireLeaseRequest{
		ResourceType: RunWorkerLeaseResourceType, ResourceID: run.ID, Owner: "stale-controlled-worker", TTL: time.Second,
		Actor: "run-worker-reconcile", Reason: "fixture controlled worker lease",
	})
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := services.LocalRuntime.AttachRun(ctx, AttachRunRequest{RunID: run.ID})
	if err != nil || len(attachment.WorkerLeases) != 1 || attachment.WorkerLeases[0].Lease.ID != lease.ID || !attachment.WorkerLeases[0].Valid {
		t.Fatalf("attached worker leases = %+v, %v", attachment.WorkerLeases, err)
	}
	time.Sleep(1500 * time.Millisecond)
	reconciled, err := services.LocalRuntime.ReconcileRun(ctx, ReconcileRunRequest{
		RunID: run.ID, Actor: "run-worker-reconcile", Reason: "recover stale controlled worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.ExpiredWorkerLeases != 1 || len(reconciled.Attachment.WorkerLeases) != 1 || reconciled.Attachment.WorkerLeases[0].Valid || reconciled.Attachment.WorkerLeases[0].Lease.State != store.LeaseExpired {
		t.Fatalf("worker lease reconcile result = %+v", reconciled)
	}
}

func waitForRunWorkerLease(t *testing.T, ctx context.Context, services *LifecycleServices, runID string, active bool) store.Lease {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		leases, err := services.Store().ListLeasesForResource(ctx, RunWorkerLeaseResourceType, runID)
		if err != nil {
			t.Fatal(err)
		}
		if len(leases) != 0 {
			lease := leases[len(leases)-1]
			if (lease.State == store.LeaseActive) == active {
				return lease
			}
		}
		time.Sleep(time.Millisecond)
	}
	if active {
		t.Fatalf("timed out waiting for active worker lease for run %s", runID)
	}
	t.Fatalf("timed out waiting for released worker lease for run %s", runID)
	return store.Lease{}
}

func waitForRunWorkerInitialDelivery(t *testing.T, ctx context.Context, services *LifecycleServices, runID string) {
	t.Helper()
	deadline := time.NewTimer(runWorkerSessionReadyTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, err := services.Store().GetDurableJobByIdempotency(ctx, "workflow-run-execution:"+runID)
		if err != nil {
			t.Fatal(err)
		}
		if job != nil && job.State == store.JobSucceeded {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for initial durable delivery for run %s: %+v", runID, job)
		case <-ticker.C:
		}
	}
}

func waitForRunWorkerHandoffState(t *testing.T, ctx context.Context, services *LifecycleServices, runID string, wanted store.RunWorkerHandoffState) store.RunWorkerHandoff {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		handoffs, err := services.Store().ListRunWorkerHandoffsForRun(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		for _, handoff := range handoffs {
			if handoff.State == wanted {
				return handoff
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for run-worker handoff %s for run %s", wanted, runID)
	return store.RunWorkerHandoff{}
}
