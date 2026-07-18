package app

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

func TestDurableJobFailureErrorRedactsRawCause(t *testing.T) {
	cause := errors.New("provider returned model output from /private/run with sk-sensitive-token")
	err := newDurableJobFailureError(store.DurableJobFailure{
		Code: "handoff.storage_unavailable", Message: "The persisted handoff could not be read safely.", DetailsJSON: `{}`,
	}, cause)
	if got := err.Error(); got != "handoff.storage_unavailable" || strings.Contains(got, "private") || strings.Contains(got, "sk-") {
		t.Fatalf("durable failure error exposed raw cause: %q", got)
	}
	if !errors.Is(err, cause) {
		t.Fatal("durable failure error no longer unwraps its cause for internal classification")
	}
}

func TestDurableWorkerClaimsProjectsAndReleasesOneJob(t *testing.T) {
	ctx := context.Background()
	dataStore, err := store.OpenForTest(t.TempDir())
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
		Handler: DurableJobHandlerFunc(func(_ context.Context, execution DurableJobExecution) (DurableJobResult, error) {
			handled <- execution
			return DurableJobResult{State: store.JobSucceeded}, nil
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
	dataStore, err := store.OpenForTest(t.TempDir())
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
		Handler: DurableJobHandlerFunc(func(_ context.Context, execution DurableJobExecution) (DurableJobResult, error) {
			return DurableJobResult{
				State:   store.JobFailed,
				Failure: newDurableJobFailure("test.executor_failed", "The test executor reported a deterministic failure.", durableJobFailureDetails(*execution.Claim.Job, "executor")),
			}, want
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.RunOnce(ctx)
	if !errors.Is(err, want) {
		t.Fatalf("worker failure = %v, want wrapped handler failure", err)
	}
	if strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("worker failure exposed raw handler error: %v", err)
	}
	if result.FinalState != store.JobFailed {
		t.Fatalf("worker final state = %q, want failed", result.FinalState)
	}
	persisted, err := dataStore.GetDurableJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted == nil || persisted.State != store.JobFailed || persisted.Failure == nil || persisted.Failure.Code != "test.executor_failed" {
		t.Fatalf("handler failure was projected as %+v, want failed", persisted)
	}
}

func TestDurableWorkerRejectsInvalidRunProjectionBeforeTransition(t *testing.T) {
	ctx := context.Background()
	dataStore, err := store.OpenForTest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	job, err := dataStore.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "test.execute", EntityType: "test", EntityID: "subject", PayloadJSON: `{}`,
		IdempotencyKey: "durable-worker-invalid-projection", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewDurableWorker(DurableWorkerConfig{
		Store: dataStore, Owner: "worker-invalid-projection", Actor: "tester",
		Handler: DurableJobHandlerFunc(func(_ context.Context, execution DurableJobExecution) (DurableJobResult, error) {
			return DurableJobResult{
				State:         store.JobFailed,
				Failure:       newDurableJobFailure("test.executor_failed", "The test executor reported a deterministic failure.", durableJobFailureDetails(*execution.Claim.Job, "executor")),
				RunProjection: &store.DurableJobRunProjection{Status: store.WorkflowRunFailedTerminal},
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.RunOnce(ctx)
	if !errors.Is(err, ErrDurableWorkerConfiguration) || result.FinalState != store.JobFailed {
		t.Fatalf("invalid projection worker result = %+v, %v", result, err)
	}
	persisted, err := dataStore.GetDurableJob(ctx, job.ID)
	if err != nil || persisted == nil || persisted.State != store.JobFailed || persisted.Failure == nil || persisted.Failure.Code != "worker.handler_result_invalid" {
		t.Fatalf("invalid projection durable job = %+v, %v", persisted, err)
	}
}

func TestDurableWorkerProjectsInDoubtAsDeliveryFinalWithoutAutoRetry(t *testing.T) {
	ctx := context.Background()
	dataStore, err := store.OpenForTest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	job, err := dataStore.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "test.execute", EntityType: "test", EntityID: "subject", PayloadJSON: `{}`,
		IdempotencyKey: "durable-worker-in-doubt", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := dataStore.ConfigureCapacityPool(ctx, store.ConfigureCapacityPoolRequest{
		PoolKey: "durable-worker-in-doubt", Capacity: 1, Actor: "tester", Reason: "reserve in_doubt worker capacity",
	})
	if err != nil {
		t.Fatal(err)
	}
	hold := errors.New("controlled definition is unavailable")
	worker, err := NewDurableWorker(DurableWorkerConfig{
		Store: dataStore, Owner: "worker-in-doubt", Actor: "tester", Reason: "test in_doubt delivery", CapacityPoolKey: pool.PoolKey,
		Handler: DurableJobHandlerFunc(func(_ context.Context, execution DurableJobExecution) (DurableJobResult, error) {
			return DurableJobResult{
				State:   store.JobInDoubt,
				Failure: newDurableJobFailure("handoff.definition_unavailable", "The controlled definition is unavailable.", durableJobFailureDetails(*execution.Claim.Job, "definition")),
			}, hold
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.RunOnce(ctx)
	if !errors.Is(err, hold) || result.FinalState != store.JobInDoubt || result.Job == nil || result.Job.ID != job.ID {
		t.Fatalf("in_doubt worker result = %+v, %v", result, err)
	}
	persisted, err := dataStore.GetDurableJob(ctx, job.ID)
	if err != nil || persisted == nil || persisted.State != store.JobInDoubt || persisted.FinishedAt == nil || persisted.Failure == nil || persisted.Failure.Code != "handoff.definition_unavailable" {
		t.Fatalf("in_doubt durable job = %+v, %v", persisted, err)
	}
	if _, err := dataStore.TransitionDurableJob(ctx, store.TransitionDurableJobRequest{
		JobID: persisted.ID, ExpectedVersion: persisted.Version, State: store.JobRunning, Actor: "tester", Reason: "attempt implicit retry",
	}); !errors.Is(err, store.ErrInvalidTransition) {
		t.Fatalf("in_doubt delivery resumed without explicit redrive = %v, want invalid transition", err)
	}
	for _, leaseID := range []string{result.Claim.DispatchLease.ID, result.Claim.CapacityLease.ID} {
		lease, leaseErr := dataStore.GetLease(ctx, leaseID)
		if leaseErr != nil || lease == nil || lease.State != store.LeaseReleased {
			t.Fatalf("in_doubt lease %s = %+v, %v; want released", leaseID, lease, leaseErr)
		}
	}
	recoveries, err := dataStore.ScanExpiredDurableJobsForReconcile(ctx, store.ScanExpiredDurableJobsRequest{Actor: "tester", Reason: "verify in_doubt has no active dispatch"})
	if err != nil || len(recoveries) != 0 {
		t.Fatalf("in_doubt job entered expired recovery despite released delivery: %+v, %v", recoveries, err)
	}
	second, err := worker.RunOnce(ctx)
	if err != nil || !second.Empty {
		t.Fatalf("in_doubt job was retried without explicit redrive: %+v, %v", second, err)
	}
}

func TestDurableWorkerHeartbeatsDispatchFenceDuringActiveHandler(t *testing.T) {
	ctx := context.Background()
	dataStore, err := store.OpenForTest(t.TempDir())
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
		Handler: DurableJobHandlerFunc(func(_ context.Context, execution DurableJobExecution) (DurableJobResult, error) {
			deadline := time.NewTimer(3 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(25 * time.Millisecond)
			defer ticker.Stop()
			for {
				lease, lookupErr := dataStore.GetLease(context.Background(), execution.Claim.DispatchLease.ID)
				if lookupErr != nil {
					heartbeated <- heartbeatObservation{lease: lease, err: lookupErr}
					return DurableJobResult{State: store.JobFailed, Failure: newDurableJobFailure("test.lease_lookup_failed", "The test could not read its dispatch lease.", durableJobFailureDetails(*execution.Claim.Job, "dispatch_lease"))}, lookupErr
				}
				if lease != nil && lease.State == store.LeaseActive && lease.Version > execution.Claim.DispatchLease.Version {
					heartbeated <- heartbeatObservation{lease: lease}
					return DurableJobResult{State: store.JobSucceeded}, nil
				}
				select {
				case <-deadline.C:
					heartbeated <- heartbeatObservation{lease: lease}
					return DurableJobResult{State: store.JobFailed, Failure: newDurableJobFailure("test.heartbeat_missing", "The dispatch heartbeat was not observed.", durableJobFailureDetails(*execution.Claim.Job, "dispatch_lease"))}, errors.New("dispatch heartbeat was not observed before deadline")
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

func TestDurableWorkerRetriesTransientHeartbeatWithinSafeWindow(t *testing.T) {
	ctx := context.Background()
	dataStore, err := store.OpenForTest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	if _, err := dataStore.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "test.execute", EntityType: "test", EntityID: "transient-heartbeat", PayloadJSON: `{}`,
		IdempotencyKey: "durable-worker-transient-heartbeat", Actor: "tester", Reason: "fixture",
	}); err != nil {
		t.Fatal(err)
	}
	worker, err := NewDurableWorker(DurableWorkerConfig{
		Store: dataStore, Owner: "worker-transient-heartbeat", Actor: "tester", LeaseTTL: 500 * time.Millisecond, HeartbeatEvery: 10 * time.Millisecond,
		Handler: DurableJobHandlerFunc(func(_ context.Context, execution DurableJobExecution) (DurableJobResult, error) {
			deadline := time.NewTimer(time.Second)
			defer deadline.Stop()
			for {
				lease, err := dataStore.GetLease(context.Background(), execution.DispatchFence.LeaseID)
				if err != nil {
					return DurableJobResult{State: store.JobFailed, Failure: newDurableJobFailure("test.lease_read_failed", "The test lease could not be read.", durableJobFailureDetails(*execution.Claim.Job, "lease"))}, err
				}
				if lease != nil && lease.Version > execution.Claim.DispatchLease.Version {
					return DurableJobResult{State: store.JobSucceeded}, nil
				}
				select {
				case <-deadline.C:
					return DurableJobResult{State: store.JobFailed, Failure: newDurableJobFailure("test.heartbeat_timeout", "The transient heartbeat did not recover.", durableJobFailureDetails(*execution.Claim.Job, "lease"))}, errors.New("heartbeat retry timed out")
				case <-time.After(5 * time.Millisecond):
				}
			}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	actualHeartbeat := worker.heartbeatLease
	var calls atomic.Int32
	worker.heartbeatLease = func(ctx context.Context, request store.HeartbeatLeaseRequest) (store.Lease, error) {
		if calls.Add(1) == 1 {
			return store.Lease{}, store.ErrOptimisticLock
		}
		return actualHeartbeat(ctx, request)
	}
	result, err := worker.RunOnce(ctx)
	if err != nil || result.FinalState != store.JobSucceeded {
		t.Fatalf("transient heartbeat result = %+v, %v", result, err)
	}
	if result.HeartbeatFirstErrorClass != DurableJobHeartbeatStoreTransient || result.HeartbeatFinalErrorClass != DurableJobHeartbeatRecovered {
		t.Fatalf("transient heartbeat classes = %q/%q", result.HeartbeatFirstErrorClass, result.HeartbeatFinalErrorClass)
	}
}

func TestDurableWorkerLeaseLossLeavesJobForScannerRecovery(t *testing.T) {
	ctx := context.Background()
	dataStore, err := store.OpenForTest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	job, err := dataStore.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "test.execute", EntityType: "test", EntityID: "lost-heartbeat", PayloadJSON: `{}`,
		IdempotencyKey: "durable-worker-lost-heartbeat", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewDurableWorker(DurableWorkerConfig{
		Store: dataStore, Owner: "worker-lost-heartbeat", Actor: "tester", LeaseTTL: 80 * time.Millisecond, HeartbeatEvery: 10 * time.Millisecond,
		Handler: DurableJobHandlerFunc(func(_ context.Context, execution DurableJobExecution) (DurableJobResult, error) {
			<-execution.LeaseLost
			return DurableJobResult{State: store.JobSucceeded}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	worker.heartbeatLease = func(context.Context, store.HeartbeatLeaseRequest) (store.Lease, error) {
		return store.Lease{}, store.ErrFencingToken
	}
	result, err := worker.RunOnce(ctx)
	if !errors.Is(err, ErrDurableJobLeaseLost) || result.FinalState != "" || result.HeartbeatFinalErrorClass != DurableJobHeartbeatFenceInvalid {
		t.Fatalf("lost heartbeat result = %+v, %v", result, err)
	}
	persisted, err := dataStore.GetDurableJob(ctx, job.ID)
	if err != nil || persisted == nil || persisted.State != store.JobRunning || persisted.FinishedAt != nil {
		t.Fatalf("stale worker projected terminal job state = %+v, %v", persisted, err)
	}
	time.Sleep(100 * time.Millisecond)
	recoveries, err := dataStore.ScanExpiredDurableJobsForReconcile(ctx, store.ScanExpiredDurableJobsRequest{Actor: "tester", Reason: "recover lost heartbeat fixture"})
	if err != nil || len(recoveries) != 1 || recoveries[0].Job.ID != job.ID || recoveries[0].Job.State != store.JobInDoubt {
		t.Fatalf("scanner recovery = %+v, %v", recoveries, err)
	}
}

func TestDurableWorkerRunContinuesAfterProjectedJobFailure(t *testing.T) {
	dataStore, err := store.OpenForTest(t.TempDir())
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
		Handler: DurableJobHandlerFunc(func(_ context.Context, execution DurableJobExecution) (DurableJobResult, error) {
			if calls.Add(1) == 1 {
				return DurableJobResult{State: store.JobFailed, Failure: newDurableJobFailure("test.first_job_failed", "The first test job failed.", durableJobFailureDetails(*execution.Claim.Job, "executor"))}, errors.New("first job failed")
			}
			cancel()
			return DurableJobResult{State: store.JobSucceeded}, nil
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
