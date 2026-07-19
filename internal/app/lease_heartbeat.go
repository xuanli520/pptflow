package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

// LeaseHeartbeatErrorClass is shared by every long-running lease owner. The
// class is stable and safe to persist or expose without leaking raw Store text.
type LeaseHeartbeatErrorClass string

const (
	LeaseHeartbeatFenceInvalid   LeaseHeartbeatErrorClass = "fence_invalid"
	LeaseHeartbeatStoreTransient LeaseHeartbeatErrorClass = "store_transient"
	LeaseHeartbeatStoreFailure   LeaseHeartbeatErrorClass = "store_failure"
	LeaseHeartbeatRetryExhausted LeaseHeartbeatErrorClass = "retry_window_exhausted"
)

// LeaseHeartbeatError preserves the cause for local errors.Is/errors.As checks
// while giving supervisors one stable classification and bounded deadline.
type LeaseHeartbeatError struct {
	Class    LeaseHeartbeatErrorClass
	Deadline time.Time
	Cause    error
}

func (err *LeaseHeartbeatError) Error() string {
	if err == nil {
		return "lease heartbeat failed"
	}
	return fmt.Sprintf("lease heartbeat failed (%s)", err.Class)
}

func (err *LeaseHeartbeatError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

func classifyLeaseHeartbeatError(err error) LeaseHeartbeatErrorClass {
	if errors.Is(err, context.DeadlineExceeded) {
		return LeaseHeartbeatRetryExhausted
	}
	if errors.Is(err, store.ErrDispatchFenceLost) || errors.Is(err, store.ErrFencingToken) ||
		errors.Is(err, store.ErrImmutable) || errors.Is(err, store.ErrLeaseHeld) ||
		errors.Is(err, store.ErrQuotaLeaseExpired) || errors.Is(err, store.ErrNotFound) {
		return LeaseHeartbeatFenceInvalid
	}
	if errors.Is(err, store.ErrOptimisticLock) || transientSQLiteError(err) {
		return LeaseHeartbeatStoreTransient
	}
	return LeaseHeartbeatStoreFailure
}

func transientSQLiteError(err error) bool {
	type sqliteCoder interface{ Code() int }
	var coded sqliteCoder
	if !errors.As(err, &coded) {
		return false
	}
	switch coded.Code() & 0xff {
	case 5, 6, 10: // SQLITE_BUSY, SQLITE_LOCKED, SQLITE_IOERR
		return true
	default:
		return false
	}
}

func leaseHeartbeatCallContext(parent context.Context, deadline time.Time) (context.Context, context.CancelFunc, error) {
	if parent == nil {
		parent = context.Background()
	}
	if deadline.IsZero() || !time.Now().UTC().Before(deadline) {
		return nil, nil, &LeaseHeartbeatError{Class: LeaseHeartbeatRetryExhausted, Deadline: deadline, Cause: context.DeadlineExceeded}
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	return ctx, cancel, nil
}

func leaseHeartbeatRetryDelay(interval time.Duration, deadline time.Time) time.Duration {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0
	}
	delay := interval / 4
	if delay <= 0 || delay > 250*time.Millisecond {
		delay = 250 * time.Millisecond
	}
	if delay > remaining {
		delay = remaining
	}
	return delay
}

func waitLeaseHeartbeatRetry(parent context.Context, stop <-chan struct{}, delay time.Duration) bool {
	if delay <= 0 {
		return false
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	if parent == nil {
		parent = context.Background()
	}
	select {
	case <-parent.Done():
		return false
	case <-stop:
		return false
	case <-timer.C:
		return true
	}
}

// retryIdempotentLeaseHeartbeat retries the exact same durable operation. It
// is suitable for quota and outbox heartbeats, whose idempotency keys recover a
// commit whose acknowledgement was lost.
func retryIdempotentLeaseHeartbeat[T any](parent context.Context, stop <-chan struct{}, interval time.Duration, deadline time.Time, heartbeat func(context.Context) (T, error)) (T, error) {
	var zero T
	for {
		callCtx, cancel, err := leaseHeartbeatCallContext(parent, deadline)
		if err != nil {
			return zero, err
		}
		updated, heartbeatErr := heartbeat(callCtx)
		cancel()
		if heartbeatErr == nil {
			return updated, nil
		}
		class := classifyLeaseHeartbeatError(heartbeatErr)
		if class != LeaseHeartbeatStoreTransient {
			return zero, &LeaseHeartbeatError{Class: class, Deadline: deadline, Cause: heartbeatErr}
		}
		delay := leaseHeartbeatRetryDelay(interval, deadline)
		if !waitLeaseHeartbeatRetry(parent, stop, delay) {
			return zero, &LeaseHeartbeatError{Class: LeaseHeartbeatRetryExhausted, Deadline: deadline, Cause: heartbeatErr}
		}
	}
}

// retryVersionedLeaseHeartbeat handles generic CAS leases, which have no
// heartbeat idempotency key. After a transient error it reloads the fence: an
// advanced matching version proves that this or another same-fence heartbeat
// committed; an unchanged version is safe to retry.
func retryVersionedLeaseHeartbeat(parent context.Context, stop <-chan struct{}, interval time.Duration, lease store.Lease,
	heartbeat func(context.Context, store.Lease) (store.Lease, error), load func(context.Context, string) (*store.Lease, error),
) (store.Lease, error) {
	current := lease
	for {
		deadline := current.ExpiresAt
		callCtx, cancel, err := leaseHeartbeatCallContext(parent, deadline)
		if err != nil {
			return current, err
		}
		updated, heartbeatErr := heartbeat(callCtx, current)
		cancel()
		if heartbeatErr == nil {
			return updated, nil
		}
		class := classifyLeaseHeartbeatError(heartbeatErr)
		if class != LeaseHeartbeatStoreTransient {
			return current, &LeaseHeartbeatError{Class: class, Deadline: deadline, Cause: heartbeatErr}
		}

		loadCtx, loadCancel, contextErr := leaseHeartbeatCallContext(parent, deadline)
		if contextErr != nil {
			return current, contextErr
		}
		loaded, loadErr := load(loadCtx, current.ID)
		loadCancel()
		if loadErr == nil && loaded != nil {
			if loaded.State != store.LeaseActive || loaded.Owner != current.Owner || loaded.FencingToken != current.FencingToken || !loaded.ExpiresAt.After(time.Now().UTC()) {
				return current, &LeaseHeartbeatError{Class: LeaseHeartbeatFenceInvalid, Deadline: deadline, Cause: store.ErrFencingToken}
			}
			if loaded.Version > current.Version {
				return *loaded, nil
			}
			current = *loaded
		} else if loadErr != nil && classifyLeaseHeartbeatError(loadErr) != LeaseHeartbeatStoreTransient {
			return current, &LeaseHeartbeatError{Class: classifyLeaseHeartbeatError(loadErr), Deadline: deadline, Cause: loadErr}
		}
		delay := leaseHeartbeatRetryDelay(interval, current.ExpiresAt)
		if !waitLeaseHeartbeatRetry(parent, stop, delay) {
			return current, &LeaseHeartbeatError{Class: LeaseHeartbeatRetryExhausted, Deadline: current.ExpiresAt, Cause: heartbeatErr}
		}
	}
}
