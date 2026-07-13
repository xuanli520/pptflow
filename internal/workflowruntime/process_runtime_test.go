package workflowruntime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestControlledChildRuntimePauseUsesScopedCheckpointAndReceipt(t *testing.T) {
	worker := newFakeControlledChildWorker()
	worker.checkpoint = RuntimeCheckpointAcknowledgement{CheckpointID: "checkpoint-7", Acknowledged: true, Resumable: true}
	worker.exitOnGraceful = true
	runtime, launcher, factory := newTestControlledRuntime(t, worker)
	scope := testStageScope("run-pause", "stage-attempt-1", "generate")

	handle, err := runtime.Start(context.Background(), scope)
	if err != nil {
		t.Fatalf("start worker: %v", err)
	}
	runToken, found := runtime.RunToken(scope.Run)
	if !found {
		t.Fatal("run token not registered")
	}
	receipt, err := runtime.RequestControl(context.Background(), RuntimeControlRequest{
		OperationKey: "pause-operation",
		Scope:        scope,
		Action:       RuntimeControlPause,
		GracePeriod:  time.Second,
	})
	if err != nil {
		t.Fatalf("pause worker: %v", err)
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("validate pause receipt: %v", err)
	}
	if receipt.Result() != TerminationResultPaused || receipt.Method() != TerminationMethodGraceful {
		t.Fatalf("pause receipt result/method = %s/%s, want paused/graceful", receipt.Result(), receipt.Method())
	}
	checkpoint := receipt.Checkpoint()
	if !checkpoint.Acknowledged || !checkpoint.Resumable || checkpoint.CheckpointID != "checkpoint-7" {
		t.Fatalf("checkpoint receipt = %#v, want acknowledged resumable checkpoint", checkpoint)
	}
	if got := worker.checkpointCallCount(); got != 1 {
		t.Fatalf("checkpoint calls = %d, want 1", got)
	}
	if got := worker.gracefulStopCallCount(); got != 1 {
		t.Fatalf("graceful stop calls = %d, want 1", got)
	}
	if got := worker.forceCallCount(); got != 0 {
		t.Fatalf("force calls = %d, want 0", got)
	}
	if handle.ExecutionToken.Intent() != CancellationPause {
		t.Fatalf("stage token intent = %q, want pause", handle.ExecutionToken.Intent())
	}
	assertCanceled(t, handle.ExecutionToken.Done(), "paused stage token")
	assertNotCanceled(t, runToken.Done(), "sibling-safe run token")

	if len(factory.requests()) != 1 || factory.requests()[0].Detached {
		t.Fatalf("factory requests = %#v, want one foreground request", factory.requests())
	}
	if len(launcher.requests()) != 1 || launcher.requests()[0].Detached {
		t.Fatalf("launcher requests = %#v, want one foreground request", launcher.requests())
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal immutable receipt: %v", err)
	}
	parsed, err := ParseRuntimeTerminationReceipt(encoded)
	if err != nil {
		t.Fatalf("parse immutable receipt: %v", err)
	}
	if parsed.OperationKey() != receipt.OperationKey() || parsed.Result() != receipt.Result() || parsed.Checkpoint().CheckpointID != "checkpoint-7" {
		t.Fatalf("parsed receipt differs: operation=%q result=%q checkpoint=%q", parsed.OperationKey(), parsed.Result(), parsed.Checkpoint().CheckpointID)
	}
}

func TestControlledChildRuntimeTerminateEscalatesAfterGrace(t *testing.T) {
	worker := newFakeControlledChildWorker()
	worker.checkpoint = RuntimeCheckpointAcknowledgement{CheckpointID: "partial-1", Acknowledged: true, Resumable: false}
	worker.exitOnForce = true
	runtime, _, _ := newTestControlledRuntime(t, worker)
	scope := testStageScope("run-terminate", "stage-attempt-1", "verify")
	handle, err := runtime.Start(context.Background(), scope)
	if err != nil {
		t.Fatalf("start worker: %v", err)
	}

	receipt, err := runtime.RequestControl(context.Background(), RuntimeControlRequest{
		OperationKey: "terminate-operation",
		Scope:        scope,
		Action:       RuntimeControlTerminate,
		GracePeriod:  15 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("terminate worker: %v", err)
	}
	if receipt.Result() != TerminationResultTerminated || receipt.Method() != TerminationMethodForced || receipt.EscalatedAt().IsZero() {
		t.Fatalf("terminate receipt = result %s method %s escalated %s, want terminated/forced/nonzero", receipt.Result(), receipt.Method(), receipt.EscalatedAt())
	}
	if got := worker.gracefulStopCallCount(); got != 1 {
		t.Fatalf("graceful stop calls = %d, want 1", got)
	}
	if got := worker.forceCallCount(); got != 1 {
		t.Fatalf("force calls = %d, want 1", got)
	}
	if checkpoint := receipt.Checkpoint(); !checkpoint.Acknowledged || checkpoint.Resumable {
		t.Fatalf("terminate checkpoint = %#v, want acknowledged non-resumable partial checkpoint", checkpoint)
	}
	if handle.ExecutionToken.Intent() != CancellationTerminate {
		t.Fatalf("stage token intent = %q, want terminate", handle.ExecutionToken.Intent())
	}
	assertCanceled(t, handle.ExecutionToken.Done(), "terminated stage token")
}

func TestControlledChildRuntimeRejectsUnacknowledgedPauseWithoutStoppingWorker(t *testing.T) {
	worker := newFakeControlledChildWorker()
	worker.checkpoint = RuntimeCheckpointAcknowledgement{CheckpointID: "partial", Acknowledged: true, Resumable: false}
	runtime, _, _ := newTestControlledRuntime(t, worker)
	scope := testStageScope("run-checkpoint", "stage-attempt-1", "generate")
	handle, err := runtime.Start(context.Background(), scope)
	if err != nil {
		t.Fatalf("start worker: %v", err)
	}

	receipt, err := runtime.RequestControl(context.Background(), RuntimeControlRequest{
		OperationKey: "pause-unresumable",
		Scope:        scope,
		Action:       RuntimeControlPause,
		GracePeriod:  time.Second,
	})
	if !errors.Is(err, ErrCheckpointRejected) {
		t.Fatalf("pause error = %v, want ErrCheckpointRejected", err)
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("validate rejected receipt: %v", err)
	}
	if receipt.Result() != TerminationResultCheckpointRejected {
		t.Fatalf("receipt result = %q, want checkpoint_rejected", receipt.Result())
	}
	if got := worker.gracefulStopCallCount(); got != 0 {
		t.Fatalf("graceful stop calls = %d, want 0", got)
	}
	if got := worker.forceCallCount(); got != 0 {
		t.Fatalf("force calls = %d, want 0", got)
	}
	assertNotCanceled(t, handle.ExecutionToken.Done(), "unacknowledged pause stage token")
}

func TestControlledChildRuntimeControlOperationIsIdempotent(t *testing.T) {
	worker := newFakeControlledChildWorker()
	worker.checkpoint = RuntimeCheckpointAcknowledgement{CheckpointID: "checkpoint-idempotent", Acknowledged: true, Resumable: true}
	worker.exitOnGraceful = true
	worker.checkpointStarted = make(chan struct{})
	worker.checkpointGate = make(chan struct{})
	runtime, _, _ := newTestControlledRuntime(t, worker)
	scope := testStageScope("run-idempotent", "stage-attempt-1", "quality")
	if _, err := runtime.Start(context.Background(), scope); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	request := RuntimeControlRequest{
		OperationKey: "same-operation",
		Scope:        scope,
		Action:       RuntimeControlPause,
		GracePeriod:  time.Second,
	}
	type result struct {
		receipt RuntimeTerminationReceipt
		err     error
	}
	results := make(chan result, 2)
	go func() {
		receipt, err := runtime.RequestControl(context.Background(), request)
		results <- result{receipt: receipt, err: err}
	}()
	select {
	case <-worker.checkpointStarted:
	case <-time.After(time.Second):
		t.Fatal("first operation did not reach checkpoint")
	}
	go func() {
		receipt, err := runtime.RequestControl(context.Background(), request)
		results <- result{receipt: receipt, err: err}
	}()
	close(worker.checkpointGate)
	first := <-results
	second := <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("idempotent results = (%v, %v), want nil errors", first.err, second.err)
	}
	if first.receipt.OperationKey() != second.receipt.OperationKey() || first.receipt.Result() != second.receipt.Result() {
		t.Fatalf("idempotent receipts differ: %#v %#v", first.receipt, second.receipt)
	}
	if got := worker.checkpointCallCount(); got != 1 {
		t.Fatalf("checkpoint calls = %d, want exactly one", got)
	}
	if got := worker.gracefulStopCallCount(); got != 1 {
		t.Fatalf("graceful stop calls = %d, want exactly one", got)
	}
	conflict := request
	conflict.Action = RuntimeControlTerminate
	if _, err := runtime.RequestControl(context.Background(), conflict); !errors.Is(err, ErrRuntimeControlConflict) {
		t.Fatalf("mismatched operation key error = %v, want ErrRuntimeControlConflict", err)
	}
}

func TestControlledChildRuntimeDetachUsesDedicatedScopedToken(t *testing.T) {
	worker := newFakeControlledChildWorker()
	runtime, launcher, factory := newTestControlledRuntime(t, worker)
	scope := testStageScope("run-detach", "stage-attempt-1", "package")
	caller, cancel := context.WithCancel(context.Background())
	handle, err := runtime.Detach(caller, scope)
	if err != nil {
		t.Fatalf("detach worker: %v", err)
	}
	cancel()
	assertNotCanceled(t, handle.ExecutionToken.Done(), "detached worker token after caller cancellation")
	if !handle.Detached {
		t.Fatal("detached handle did not retain detached flag")
	}
	if got := factory.requests(); len(got) != 1 || !got[0].Detached || got[0].Scope != scope {
		t.Fatalf("factory detach request = %#v, want detached scope %#v", got, scope)
	}
	if got := launcher.requests(); len(got) != 1 || !got[0].Detached || got[0].ExecutionToken.Scope() != scope {
		t.Fatalf("launcher detach request = %#v, want detached scoped token", got)
	}
}

func TestControlledChildRuntimeTerminateRunUsesRunAndStageScopes(t *testing.T) {
	first := newFakeControlledChildWorker()
	first.checkpoint = RuntimeCheckpointAcknowledgement{CheckpointID: "first", Acknowledged: true, Resumable: true}
	first.exitOnGraceful = true
	second := newFakeControlledChildWorker()
	second.checkpoint = RuntimeCheckpointAcknowledgement{CheckpointID: "second", Acknowledged: true, Resumable: true}
	second.exitOnGraceful = true
	factory := &fakeChildWorkerFactory{command: ChildWorkerCommand{Program: "harbor-factory-worker"}}
	launcher := &fakeChildWorkerLauncher{workers: []ControlledChildWorker{first, second}}
	runtime, err := NewControlledChildRuntime(ControlledChildRuntimeConfig{CommandFactory: factory, Launcher: launcher})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	run := RunScope{RunID: "run-wide"}
	firstScope := StageScope{Run: run, StageAttemptID: "attempt-a", StageKey: "generate"}
	secondScope := StageScope{Run: run, StageAttemptID: "attempt-b", StageKey: "verify"}
	if _, err := runtime.Start(context.Background(), firstScope); err != nil {
		t.Fatalf("start first worker: %v", err)
	}
	if _, err := runtime.Start(context.Background(), secondScope); err != nil {
		t.Fatalf("start second worker: %v", err)
	}
	runToken, found := runtime.RunToken(run)
	if !found {
		t.Fatal("run token not registered")
	}
	receipts, err := runtime.TerminateRun(context.Background(), RuntimeRunTerminationRequest{
		OperationKey: "terminate-run-wide",
		Run:          run,
		GracePeriod:  time.Second,
	})
	if err != nil {
		t.Fatalf("terminate run: %v", err)
	}
	if len(receipts) != 2 {
		t.Fatalf("run termination receipts = %d, want 2", len(receipts))
	}
	if runToken.Intent() != CancellationTerminate {
		t.Fatalf("run token intent = %q, want terminate", runToken.Intent())
	}
	assertCanceled(t, runToken.Done(), "run termination token")
	if first.checkpointCallCount() != 1 || second.checkpointCallCount() != 1 {
		t.Fatalf("stage checkpoint calls = %d/%d, want 1/1", first.checkpointCallCount(), second.checkpointCallCount())
	}
}

type fakeChildWorkerFactory struct {
	mu      sync.Mutex
	command ChildWorkerCommand
	err     error
	seen    []ChildWorkerCommandRequest
}

func (factory *fakeChildWorkerFactory) BuildChildWorkerCommand(_ context.Context, request ChildWorkerCommandRequest) (ChildWorkerCommand, error) {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	factory.seen = append(factory.seen, request)
	return factory.command.Clone(), factory.err
}

func (factory *fakeChildWorkerFactory) requests() []ChildWorkerCommandRequest {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	return append([]ChildWorkerCommandRequest(nil), factory.seen...)
}

type fakeChildWorkerLauncher struct {
	mu      sync.Mutex
	workers []ControlledChildWorker
	err     error
	seen    []ChildWorkerLaunchRequest
}

func (launcher *fakeChildWorkerLauncher) LaunchChildWorker(_ context.Context, request ChildWorkerLaunchRequest) (ControlledChildWorker, error) {
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	launcher.seen = append(launcher.seen, request)
	if launcher.err != nil {
		return nil, launcher.err
	}
	if len(launcher.workers) == 0 {
		return nil, errors.New("no fake child worker available")
	}
	worker := launcher.workers[0]
	launcher.workers = launcher.workers[1:]
	return worker, nil
}

func (launcher *fakeChildWorkerLauncher) requests() []ChildWorkerLaunchRequest {
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	requests := make([]ChildWorkerLaunchRequest, len(launcher.seen))
	copy(requests, launcher.seen)
	return requests
}

type fakeControlledChildWorker struct {
	mu sync.Mutex

	checkpoint    RuntimeCheckpointAcknowledgement
	checkpointErr error
	gracefulErr   error
	forceErr      error

	exitOnGraceful bool
	exitOnForce    bool
	exit           ChildWorkerExit
	done           chan ChildWorkerExit
	exitOnce       sync.Once

	checkpointRequests []RuntimeCheckpointRequest
	stopRequests       []RuntimeStopRequest
	checkpointStarted  chan struct{}
	checkpointGate     chan struct{}
}

func newFakeControlledChildWorker() *fakeControlledChildWorker {
	return &fakeControlledChildWorker{
		done: make(chan ChildWorkerExit, 1),
		exit: ChildWorkerExit{ExitCode: 0, Reason: "controlled stop"},
	}
}

func (worker *fakeControlledChildWorker) RequestCheckpoint(_ context.Context, request RuntimeCheckpointRequest) (RuntimeCheckpointAcknowledgement, error) {
	worker.mu.Lock()
	worker.checkpointRequests = append(worker.checkpointRequests, request)
	started := worker.checkpointStarted
	gate := worker.checkpointGate
	acknowledgement := worker.checkpoint
	err := worker.checkpointErr
	worker.mu.Unlock()
	if started != nil {
		select {
		case <-started:
		default:
			close(started)
		}
	}
	if gate != nil {
		<-gate
	}
	return acknowledgement, err
}

func (worker *fakeControlledChildWorker) RequestGracefulStop(_ context.Context, request RuntimeStopRequest) error {
	worker.mu.Lock()
	worker.stopRequests = append(worker.stopRequests, request)
	exitOnGraceful := worker.exitOnGraceful
	err := worker.gracefulErr
	worker.mu.Unlock()
	if exitOnGraceful {
		worker.finish()
	}
	return err
}

func (worker *fakeControlledChildWorker) ForceTerminate(_ context.Context, request RuntimeStopRequest) error {
	worker.mu.Lock()
	worker.stopRequests = append(worker.stopRequests, request)
	exitOnForce := worker.exitOnForce
	err := worker.forceErr
	worker.mu.Unlock()
	if exitOnForce {
		worker.finish()
	}
	return err
}

func (worker *fakeControlledChildWorker) Done() <-chan ChildWorkerExit { return worker.done }

func (worker *fakeControlledChildWorker) finish() {
	worker.exitOnce.Do(func() {
		worker.mu.Lock()
		exit := worker.exit
		if exit.ExitedAt.IsZero() {
			exit.ExitedAt = time.Now().UTC()
		}
		worker.mu.Unlock()
		worker.done <- exit
		close(worker.done)
	})
}

func (worker *fakeControlledChildWorker) checkpointCallCount() int {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	return len(worker.checkpointRequests)
}

func (worker *fakeControlledChildWorker) gracefulStopCallCount() int {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	count := 0
	for _, request := range worker.stopRequests {
		if !request.Force {
			count++
		}
	}
	return count
}

func (worker *fakeControlledChildWorker) forceCallCount() int {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	count := 0
	for _, request := range worker.stopRequests {
		if request.Force {
			count++
		}
	}
	return count
}

func newTestControlledRuntime(t *testing.T, worker ControlledChildWorker) (*ControlledChildRuntime, *fakeChildWorkerLauncher, *fakeChildWorkerFactory) {
	t.Helper()
	factory := &fakeChildWorkerFactory{command: ChildWorkerCommand{Program: "harbor-factory-worker", Arguments: []string{"run"}}}
	launcher := &fakeChildWorkerLauncher{workers: []ControlledChildWorker{worker}}
	runtime, err := NewControlledChildRuntime(ControlledChildRuntimeConfig{CommandFactory: factory, Launcher: launcher})
	if err != nil {
		t.Fatalf("new controlled runtime: %v", err)
	}
	return runtime, launcher, factory
}

func testStageScope(runID, attemptID, stage string) StageScope {
	return StageScope{Run: RunScope{RunID: runID}, StageAttemptID: workflowkit.AttemptID(attemptID), StageKey: workflowkit.StageKey(stage)}
}

func assertCanceled(t *testing.T, done <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("%s was not canceled", label)
	}
}

func assertNotCanceled(t *testing.T, done <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("%s was unexpectedly canceled", label)
	default:
	}
}
