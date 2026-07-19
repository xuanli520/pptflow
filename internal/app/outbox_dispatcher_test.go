package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

type classifiedOutboxFailure struct{ cause error }

func (failure classifiedOutboxFailure) Error() string { return failure.cause.Error() }
func (failure classifiedOutboxFailure) Unwrap() error { return failure.cause }
func (failure classifiedOutboxFailure) OutboxDeliveryErrorCode() string {
	return "transient_fixture_failure"
}

func TestOutboxDispatcherAcknowledgesSuccessfulDelivery(t *testing.T) {
	ctx := context.Background()
	dataStore, err := store.OpenForTest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	event, err := dataStore.CreateOutboxEvent(ctx, store.CreateOutboxEventRequest{
		Topic: "workflow_run.execute", EntityType: "workflow_run", EntityID: "run-success", PayloadJSON: `{"run":"success"}`,
		IdempotencyKey: "dispatcher-success-event", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	handled := make(chan store.OutboxEvent, 1)
	dispatcher, err := NewOutboxDispatcher(OutboxDispatcherConfig{
		Store: dataStore, Owner: "outbox-worker-success", Actor: "tester", Reason: "test dispatcher",
		RetryDelay: time.Second,
		Handler: OutboxDeliveryHandlerFunc(func(_ context.Context, received store.OutboxEvent) error {
			handled <- received
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Empty || !result.Delivered || result.Retried || result.Event == nil || result.Event.ID != event.ID || result.Event.State != store.OutboxPublished {
		t.Fatalf("dispatcher result = %+v", result)
	}
	select {
	case received := <-handled:
		if received.ID != event.ID || received.State != store.OutboxLeased || received.LeaseOwner != "outbox-worker-success" {
			t.Fatalf("handler event = %+v", received)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not invoke handler")
	}
	persisted, err := dataStore.GetOutboxEvent(ctx, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted == nil || persisted.State != store.OutboxPublished || persisted.PublishedAt == nil || persisted.LeaseOwner != "" {
		t.Fatalf("persisted delivery = %+v", persisted)
	}
	empty, err := dispatcher.RunOnce(ctx)
	if err != nil || !empty.Empty {
		t.Fatalf("second dispatcher cycle = %+v, %v", empty, err)
	}
}

func TestOutboxDispatcherClaimsOnlyConfiguredTopics(t *testing.T) {
	ctx := context.Background()
	dataStore, err := store.OpenForTest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	unrelated, err := dataStore.CreateOutboxEvent(ctx, store.CreateOutboxEventRequest{
		Topic: "control.requested", EntityType: "workflow_run", EntityID: "run-unrelated", PayloadJSON: `{}`,
		IdempotencyKey: "dispatcher-unrelated-event", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := dataStore.CreateOutboxEvent(ctx, store.CreateOutboxEventRequest{
		Topic: "workflow_run.execute", EntityType: "workflow_run", EntityID: "run-target", PayloadJSON: `{}`,
		IdempotencyKey: "dispatcher-target-event", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	handled := make(chan store.OutboxEvent, 1)
	dispatcher, err := NewOutboxDispatcher(OutboxDispatcherConfig{
		Store: dataStore, Owner: "outbox-worker-filtered", Topics: []string{"workflow_run.execute", "workflow_run.execute"}, Actor: "tester", RetryDelay: time.Second,
		Handler: OutboxDeliveryHandlerFunc(func(_ context.Context, event store.OutboxEvent) error {
			handled <- event
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Delivered || result.Event == nil || result.Event.ID != target.ID {
		t.Fatalf("filtered dispatcher result = %+v", result)
	}
	select {
	case event := <-handled:
		if event.ID != target.ID || event.Topic != "workflow_run.execute" {
			t.Fatalf("filtered handler event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("filtered dispatcher did not invoke handler")
	}
	pending, err := dataStore.GetOutboxEvent(ctx, unrelated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending == nil || pending.State != store.OutboxPending {
		t.Fatalf("unrelated event = %+v, want pending", pending)
	}
}

func TestOutboxDispatcherNacksHandlerFailureWithClassifiedRetry(t *testing.T) {
	ctx := context.Background()
	dataStore, err := store.OpenForTest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	event, err := dataStore.CreateOutboxEvent(ctx, store.CreateOutboxEventRequest{
		Topic: "control.requested", EntityType: "workflow_run", EntityID: "run-retry", PayloadJSON: `{}`,
		IdempotencyKey: "dispatcher-retry-event", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("provider temporarily unavailable")
	dispatcher, err := NewOutboxDispatcher(OutboxDispatcherConfig{
		Store: dataStore, Owner: "outbox-worker-retry", Actor: "tester", RetryDelay: time.Minute,
		Handler: OutboxDeliveryHandlerFunc(func(context.Context, store.OutboxEvent) error {
			return classifiedOutboxFailure{cause: want}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.RunOnce(ctx)
	if !errors.Is(err, want) {
		t.Fatalf("dispatcher failure = %v, want handler failure", err)
	}
	if !result.Retried || result.Delivered || result.Event == nil || result.Event.State != store.OutboxPending || result.Event.LastError != "transient_fixture_failure" || !result.Event.AvailableAt.After(time.Now().UTC()) {
		t.Fatalf("retry result = %+v", result)
	}
	persisted, err := dataStore.GetOutboxEvent(ctx, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted == nil || persisted.State != store.OutboxPending || persisted.LastError != "transient_fixture_failure" || persisted.LeaseOwner != "" {
		t.Fatalf("persisted retry = %+v", persisted)
	}
}

func TestOutboxDispatcherHeartbeatsBeforeAcknowledgement(t *testing.T) {
	ctx := context.Background()
	dataStore, err := store.OpenForTest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	if _, err := dataStore.CreateOutboxEvent(ctx, store.CreateOutboxEventRequest{
		Topic: "workflow_run.execute", EntityType: "workflow_run", EntityID: "run-heartbeat", PayloadJSON: `{}`,
		IdempotencyKey: "dispatcher-heartbeat-event", Actor: "tester", Reason: "fixture",
	}); err != nil {
		t.Fatal(err)
	}
	observed := make(chan store.OutboxEvent, 1)
	dispatcher, err := NewOutboxDispatcher(OutboxDispatcherConfig{
		Store: dataStore, Owner: "outbox-worker-heartbeat", Actor: "tester", LeaseTTL: 2 * time.Second,
		HeartbeatEvery: 100 * time.Millisecond, RetryDelay: time.Second,
		Handler: OutboxDeliveryHandlerFunc(func(handlerCtx context.Context, event store.OutboxEvent) error {
			deadline := time.NewTimer(3 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(25 * time.Millisecond)
			defer ticker.Stop()
			for {
				current, lookupErr := dataStore.GetOutboxEvent(context.Background(), event.ID)
				if lookupErr != nil {
					return lookupErr
				}
				if current != nil && current.State == store.OutboxLeased && current.Version > event.Version && current.LeaseFencingToken == event.LeaseFencingToken {
					observed <- *current
					return nil
				}
				select {
				case <-handlerCtx.Done():
					return handlerCtx.Err()
				case <-deadline.C:
					return errors.New("outbox heartbeat was not observed")
				case <-ticker.C:
				}
			}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Delivered || result.Event == nil || result.Event.State != store.OutboxPublished {
		t.Fatalf("heartbeat dispatcher result = %+v", result)
	}
	select {
	case event := <-observed:
		if event.Version <= 2 || event.LeaseExpiresAt == nil {
			t.Fatalf("heartbeat observation = %+v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not observe outbox heartbeat")
	}
}

func TestOutboxDispatcherRetriesTransientHeartbeatInsideLeaseWindow(t *testing.T) {
	ctx := context.Background()
	dataStore, err := store.OpenForTest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	if _, err := dataStore.CreateOutboxEvent(ctx, store.CreateOutboxEventRequest{
		Topic: "fixture", EntityType: "workflow_run", EntityID: "transient-heartbeat", PayloadJSON: `{}`,
		IdempotencyKey: "transient-heartbeat-event", Actor: "tester",
	}); err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewOutboxDispatcher(OutboxDispatcherConfig{
		Store: dataStore, Owner: "transient-heartbeat-worker", LeaseTTL: 500 * time.Millisecond, HeartbeatEvery: 20 * time.Millisecond,
		RetryDelay: time.Second, Handler: OutboxDeliveryHandlerFunc(func(ctx context.Context, _ store.OutboxEvent) error {
			timer := time.NewTimer(80 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	actualHeartbeat := dispatcher.heartbeatEvent
	calls := 0
	dispatcher.heartbeatEvent = func(ctx context.Context, request store.HeartbeatOutboxEventRequest) (store.OutboxEvent, error) {
		calls++
		if calls == 1 {
			return store.OutboxEvent{}, transientSQLiteFixtureError{code: 5}
		}
		return actualHeartbeat(ctx, request)
	}
	result, err := dispatcher.RunOnce(ctx)
	if err != nil || !result.Delivered || calls < 2 {
		t.Fatalf("transient outbox heartbeat result = %+v calls=%d err=%v", result, calls, err)
	}
}

func TestOutboxDispatcherRequiresExplicitRetryDelay(t *testing.T) {
	dataStore, err := store.OpenForTest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	if _, err := NewOutboxDispatcher(OutboxDispatcherConfig{
		Store: dataStore, Owner: "worker", Handler: OutboxDeliveryHandlerFunc(func(context.Context, store.OutboxEvent) error { return nil }),
	}); !errors.Is(err, ErrOutboxDispatcherConfiguration) {
		t.Fatalf("missing retry delay = %v, want configuration error", err)
	}
}
