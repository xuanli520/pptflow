package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

type transientSQLiteFixtureError struct{ code int }

func (err transientSQLiteFixtureError) Error() string { return "transient sqlite fixture" }
func (err transientSQLiteFixtureError) Code() int     { return err.code }

func TestRetryIdempotentLeaseHeartbeatRecoversTransientStoreError(t *testing.T) {
	calls := 0
	result, err := retryIdempotentLeaseHeartbeat(context.Background(), nil, 20*time.Millisecond, time.Now().Add(time.Second),
		func(context.Context) (string, error) {
			calls++
			if calls == 1 {
				return "", transientSQLiteFixtureError{code: 5}
			}
			return "renewed", nil
		})
	if err != nil || result != "renewed" || calls != 2 {
		t.Fatalf("idempotent heartbeat retry = %q calls=%d err=%v", result, calls, err)
	}
}

func TestRetryVersionedLeaseHeartbeatReloadsFenceBeforeRetry(t *testing.T) {
	id, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	lease := store.Lease{ID: id, Owner: "worker", FencingToken: 7, State: store.LeaseActive, Version: 1, ExpiresAt: time.Now().Add(time.Second)}
	calls := 0
	updated, err := retryVersionedLeaseHeartbeat(context.Background(), nil, 20*time.Millisecond, lease,
		func(_ context.Context, current store.Lease) (store.Lease, error) {
			calls++
			if calls == 1 {
				return store.Lease{}, transientSQLiteFixtureError{code: 5}
			}
			current.Version++
			current.ExpiresAt = time.Now().Add(time.Second)
			return current, nil
		}, func(context.Context, string) (*store.Lease, error) {
			copyLease := lease
			return &copyLease, nil
		})
	if err != nil || updated.Version != 2 || calls != 2 {
		t.Fatalf("versioned heartbeat retry = %+v calls=%d err=%v", updated, calls, err)
	}
}

func TestRetryLeaseHeartbeatClassifiesExpiredWindow(t *testing.T) {
	_, err := retryIdempotentLeaseHeartbeat(context.Background(), nil, time.Millisecond, time.Now().Add(-time.Millisecond),
		func(context.Context) (string, error) { return "", nil })
	var heartbeatErr *LeaseHeartbeatError
	if !errors.As(err, &heartbeatErr) || heartbeatErr.Class != LeaseHeartbeatRetryExhausted {
		t.Fatalf("expired heartbeat window error = %#v", err)
	}
}
