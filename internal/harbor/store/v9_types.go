package store

import "time"

// ClaimOutboxEventsRequest atomically leases ready messages. A claim is
// idempotent by IdempotencyKey: retries receive the exact snapshot committed
// by the original transaction instead of reselecting newer work.
type ClaimOutboxEventsRequest struct {
	ID             string
	IdempotencyKey string
	Owner          string
	// Topics optionally restricts the claim to matching event topics. Nil and
	// empty slices retain the legacy behavior of claiming every ready topic.
	Topics   []string
	Limit    int
	LeaseTTL time.Duration
	Actor    string
	Reason   string
}

// OutboxDispatchClaim is the batch-level durable result of one claim poll.
// Events carry their per-record lease fencing token and version.
type OutboxDispatchClaim struct {
	ID             string
	IdempotencyKey string
	Owner          string
	Limit          int
	LeaseTTL       time.Duration
	Events         []OutboxEvent
	ClaimedAt      time.Time
}

// HeartbeatOutboxEventRequest extends an active record lease. ExpectedVersion
// and LeaseFencingToken make a stale worker unable to extend a reclaimed
// delivery.
type HeartbeatOutboxEventRequest struct {
	ID                string
	IdempotencyKey    string
	OutboxEventID     string
	Owner             string
	ExpectedVersion   int64
	LeaseFencingToken uint64
	LeaseTTL          time.Duration
	Actor             string
	Reason            string
}

// AckOutboxEventRequest permanently completes one fenced delivery. There is
// intentionally no public unfenced publish shortcut.
type AckOutboxEventRequest struct {
	ID                string
	IdempotencyKey    string
	OutboxEventID     string
	Owner             string
	ExpectedVersion   int64
	LeaseFencingToken uint64
	Actor             string
	Reason            string
}

// NackOutboxEventRequest returns one fenced delivery to the queue after an
// explicit retry delay. Payload and routing identity remain immutable.
type NackOutboxEventRequest struct {
	ID                string
	IdempotencyKey    string
	OutboxEventID     string
	Owner             string
	ExpectedVersion   int64
	LeaseFencingToken uint64
	RetryDelay        time.Duration
	ErrorCode         string
	Actor             string
	Reason            string
}
