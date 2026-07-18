package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrDispatchFenceLost = errors.New("store: durable job dispatch fence is no longer active")

// DispatchFence is the immutable execution authority returned with a claimed
// durable job. The lease row remains the source of truth; this value only
// carries the identity that each worker-owned mutation must prove in its own
// transaction.
type DispatchFence struct {
	LeaseID      string
	Owner        string
	FencingToken uint64
}

// DispatchFenceAdmission serializes heartbeat state changes with Store
// mutation admission. Implementations must hold their admission lock from a
// successful BeginDispatchFenceMutation call until EndDispatchFenceMutation.
type DispatchFenceAdmission interface {
	BeginDispatchFenceMutation() error
	EndDispatchFenceMutation()
}

type dispatchFenceContextKey struct{}

type dispatchFenceBinding struct {
	fence     DispatchFence
	admission DispatchFenceAdmission
}

// WithDispatchFence binds worker execution authority to a context. Read APIs
// ignore the binding; guarded mutation APIs validate it in their transaction.
func WithDispatchFence(ctx context.Context, fence DispatchFence, admission DispatchFenceAdmission) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, dispatchFenceContextKey{}, dispatchFenceBinding{fence: fence, admission: admission})
}

// beginDispatchFenceTx establishes the in-memory admission order before it
// reserves a database connection. Heartbeats use the same guard-before-Store
// order, preventing a one-connection pool from deadlocking with a mutation
// that already owns the transaction while waiting for the guard.
func (s *Store) beginDispatchFenceTx(ctx context.Context) (*sql.Tx, func(), error) {
	binding, ok := ctx.Value(dispatchFenceContextKey{}).(dispatchFenceBinding)
	if ok && binding.admission != nil {
		if err := binding.admission.BeginDispatchFenceMutation(); err != nil {
			return nil, nil, err
		}
	}
	release := func() {
		if ok && binding.admission != nil {
			binding.admission.EndDispatchFenceMutation()
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		release()
		return nil, nil, err
	}
	if ok {
		if err := assertActiveDispatchFenceTx(ctx, tx, binding.fence, s.now().UTC()); err != nil {
			_ = tx.Rollback()
			release()
			return nil, nil, err
		}
	}
	return tx, release, nil
}

// assertActiveDispatchFenceTx is the final worker write-permission check. It
// deliberately ignores mutable lease versions: identity, owner, fencing token,
// active state, and expiration are the transaction-local authority facts.
func assertActiveDispatchFenceTx(ctx context.Context, tx *sql.Tx, fence DispatchFence, now time.Time) error {
	if !isUUIDv7(fence.LeaseID) || strings.TrimSpace(fence.Owner) == "" || fence.FencingToken == 0 {
		return fmt.Errorf("%w: invalid dispatch fence identity", ErrDispatchFenceLost)
	}
	lease, err := getLeaseTx(ctx, tx, fence.LeaseID)
	if err != nil {
		return fmt.Errorf("%w: lease unavailable", ErrDispatchFenceLost)
	}
	if lease.ResourceType != "job_dispatch" || lease.Owner != fence.Owner || lease.FencingToken != fence.FencingToken || lease.State != LeaseActive || !lease.ExpiresAt.After(now.UTC()) {
		return fmt.Errorf("%w: lease inactive", ErrDispatchFenceLost)
	}
	return nil
}
