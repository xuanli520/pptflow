package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

func TestDurableWorkerClaimsProjectsAndReleasesOneJob(t *testing.T) {
	ctx := context.Background()
	dataStore, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	job, err := dataStore.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "test.execute", EntityType: "test", EntityID: "subject", PayloadJSON: `{}`,
		IdempotencyKey: "durable-worker-success", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	handled := make(chan DurableJobExecution, 1)
	worker, err := NewDurableWorker(DurableWorkerConfig{
		Store: dataStore, Owner: "worker-success", Actor: "tester", Reason: "test worker",
		Handler: DurableJobHandlerFunc(func(_ context.Context, execution DurableJobExecution) (store.JobState, error) {
			handled <- execution
			return store.JobSucceeded, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Empty || result.FinalState != store.JobSucceeded || result.Job == nil || result.Job.ID != job.ID {
		t.Fatalf("worker result = %+v, want claimed succeeded job %s", result, job.ID)
	}
	select {
	case execution := <-handled:
		if execution.Claim.Job == nil || execution.Claim.Job.ID != job.ID || execution.Claim.DispatchLease == nil {
			t.Fatalf("handler execution = %+v", execution)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not invoke handler")
	}
	persisted, err := dataStore.GetDurableJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted == nil || persisted.State != store.JobSucceeded || persisted.FinishedAt == nil {
		t.Fatalf("durable job after worker = %+v", persisted)
	}
	lease, err := dataStore.GetLease(ctx, result.Claim.DispatchLease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease == nil || lease.State != store.LeaseReleased {
		t.Fatalf("dispatch lease after terminal job = %+v, want released", lease)
	}
	second, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Empty {
		t.Fatalf("second worker cycle = %+v, want empty", second)
	}
}

func TestDurableWorkerProjectsHandlerFailureWithoutFalseSuccess(t *testing.T) {
	ctx := context.Background()
	dataStore, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	job, err := dataStore.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "test.execute", EntityType: "test", EntityID: "subject", PayloadJSON: `{}`,
		IdempotencyKey: "durable-worker-failure", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("real executor failure")
	worker, err := NewDurableWorker(DurableWorkerConfig{
		Store: dataStore, Owner: "worker-failure", Actor: "tester",
		Handler: DurableJobHandlerFunc(func(context.Context, DurableJobExecution) (store.JobState, error) {
			return "", want
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.RunOnce(ctx)
	if !errors.Is(err, want) {
		t.Fatalf("worker failure = %v, want wrapped handler failure", err)
	}
	if result.FinalState != store.JobFailed {
		t.Fatalf("worker final state = %q, want failed", result.FinalState)
	}
	persisted, err := dataStore.GetDurableJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted == nil || persisted.State != store.JobFailed {
		t.Fatalf("handler failure was projected as %+v, want failed", persisted)
	}
}

func TestDurableWorkerHeartbeatsDispatchFenceDuringActiveHandler(t *testing.T) {
	ctx := context.Background()
	dataStore, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	if _, err := dataStore.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "test.execute", EntityType: "test", EntityID: "subject", PayloadJSON: `{}`,
		IdempotencyKey: "durable-worker-heartbeat", Actor: "tester", Reason: "fixture",
	}); err != nil {
		t.Fatal(err)
	}
	type heartbeatObservation struct {
		lease *store.Lease
		err   error
	}
	heartbeated := make(chan heartbeatObservation, 1)
	worker, err := NewDurableWorker(DurableWorkerConfig{
		Store: dataStore, Owner: "worker-heartbeat", Actor: "tester", LeaseTTL: 5 * time.Second, HeartbeatEvery: 20 * time.Millisecond,
		Handler: DurableJobHandlerFunc(func(_ context.Context, execution DurableJobExecution) (store.JobState, error) {
			deadline := time.NewTimer(3 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(25 * time.Millisecond)
			defer ticker.Stop()
			for {
				lease, lookupErr := dataStore.GetLease(context.Background(), execution.Claim.DispatchLease.ID)
				if lookupErr != nil {
					heartbeated <- heartbeatObservation{lease: lease, err: lookupErr}
					return "", lookupErr
				}
				if lease != nil && lease.State == store.LeaseActive && lease.Version > execution.Claim.DispatchLease.Version {
					heartbeated <- heartbeatObservation{lease: lease}
					return store.JobSucceeded, nil
				}
				select {
				case <-deadline.C:
					heartbeated <- heartbeatObservation{lease: lease}
					return store.JobFailed, errors.New("dispatch heartbeat was not observed before deadline")
				case <-ticker.C:
				}
			}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case observed := <-heartbeated:
		if observed.err != nil || observed.lease == nil || observed.lease.State != store.LeaseActive || observed.lease.Version <= 1 {
			t.Fatalf("worker did not heartbeat active dispatch lease: %+v err=%v", observed.lease, observed.err)
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not observe heartbeat")
	}
}

func TestDurableWorkerRunContinuesAfterProjectedJobFailure(t *testing.T) {
	dataStore, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	ctx := context.Background()
	first, err := dataStore.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "test.execute", EntityType: "test", EntityID: "first", PayloadJSON: `{}`,
		IdempotencyKey: "durable-worker-run-first", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := dataStore.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "test.execute", EntityType: "test", EntityID: "second", PayloadJSON: `{}`,
		IdempotencyKey: "durable-worker-run-second", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	worker, err := NewDurableWorker(DurableWorkerConfig{
		Store: dataStore, Owner: "worker-run", Actor: "tester", PollInterval: time.Millisecond,
		Handler: DurableJobHandlerFunc(func(_ context.Context, _ DurableJobExecution) (store.JobState, error) {
			if calls.Add(1) == 1 {
				return "", errors.New("first job failed")
			}
			cancel()
			return store.JobSucceeded, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Run(runCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("worker run error = %v, want context cancellation after second job", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("handler calls = %d, want both queued jobs", calls.Load())
	}
	for _, expectation := range []struct {
		job   store.DurableJob
		state store.JobState
	}{
		{job: first, state: store.JobFailed},
		{job: second, state: store.JobSucceeded},
	} {
		persisted, err := dataStore.GetDurableJob(ctx, expectation.job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted == nil || persisted.State != expectation.state {
			t.Fatalf("job %s state = %+v, want %s", expectation.job.ID, persisted, expectation.state)
		}
	}
}
