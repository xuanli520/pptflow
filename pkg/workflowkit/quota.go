package workflowkit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

var (
	// ErrInvalidQuota marks malformed generic quota requests and ledger facts.
	ErrInvalidQuota = errors.New("workflowkit: invalid quota")
	// ErrQuotaExhausted marks an otherwise valid request that cannot be
	// admitted without exceeding a configured bucket.
	ErrQuotaExhausted = errors.New("workflowkit: quota exhausted")
	// ErrQuotaNotConfigured marks a claim for which no generic quota bucket is
	// available. Domain adapters own the decision to configure a bucket.
	ErrQuotaNotConfigured = errors.New("workflowkit: quota bucket not configured")
	// ErrQuotaLeaseExpired marks a lease that must be reconciled rather than
	// charged, heartbeated, or blindly settled.
	ErrQuotaLeaseExpired = errors.New("workflowkit: quota lease expired")
	// ErrStaleQuotaGrant marks a grant whose optimistic bucket version is no
	// longer current.
	ErrStaleQuotaGrant = errors.New("workflowkit: stale quota grant")
	// ErrIdempotencyConflict is returned when an idempotency key is replayed
	// with a different immutable request.
	ErrIdempotencyConflict = errors.New("workflowkit: idempotency conflict")
)

// Clock supplies time to deterministic in-memory kernel implementations.
// Production adapters may pass a wall clock; tests can pass a controlled one.
type Clock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

func resolveClock(clock Clock) Clock {
	if clock == nil {
		return wallClock{}
	}
	return clock
}

// ResourceScope names one generic quota ownership boundary. A caller that
// needs more than one boundary for an operation submits multiple claims; the
// admission controller reserves all of them atomically.
type ResourceScope struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Validate verifies a stable, non-domain-specific scope identity.
func (scope ResourceScope) Validate() error {
	if err := validateRequired("quota scope kind", scope.Kind, ErrInvalidQuota); err != nil {
		return err
	}
	if err := validateRequired("quota scope id", scope.ID, ErrInvalidQuota); err != nil {
		return err
	}
	return nil
}

// ReclaimPolicy describes what happens to an unused reservation after a known
// settlement. Unknown outcomes never reclaim capacity until reconciliation.
type ReclaimPolicy string

const (
	// ReclaimUnused releases unused reservation units on a known completion or
	// cancellation while retaining units already charged.
	ReclaimUnused ReclaimPolicy = "reclaim_unused"
	// ReclaimNever converts all remaining reservation units into consumption on
	// known settlement. This is useful for units committed at admission time.
	ReclaimNever ReclaimPolicy = "reclaim_never"
)

func (policy ReclaimPolicy) valid() bool {
	return policy == ReclaimUnused || policy == ReclaimNever
}

// QuotaRequest is one explicit resource claim. It has no product-specific
// dimensions: adapters choose dimension names and scopes.
type QuotaRequest struct {
	Dimension     string        `json:"dimension"`
	Units         int64         `json:"units"`
	Scope         ResourceScope `json:"scope"`
	ReclaimPolicy ReclaimPolicy `json:"reclaim_policy"`
}

// QuotaClaim is a scope-free, frozen resource demand. A workflow descriptor
// carries claims before a concrete task and actor are known; admission expands
// every claim to the authoritative task and actor scopes atomically.
//
// It is deliberately distinct from QuotaRequest. A descriptor must never
// freeze a caller-provided scope identity, while an actual reservation always
// has one through QuotaRequest.
type QuotaClaim struct {
	Dimension     string        `json:"dimension"`
	Units         int64         `json:"units"`
	ReclaimPolicy ReclaimPolicy `json:"reclaim_policy"`
}

// Validate proves that a descriptor claim is complete and positive. Product
// adapters decide which dimensions exist; workflowkit only preserves their
// durable accounting semantics.
func (claim QuotaClaim) Validate() error {
	if err := validateRequired("quota claim dimension", claim.Dimension, ErrInvalidQuota); err != nil {
		return err
	}
	if claim.Units <= 0 {
		return fmt.Errorf("%w: quota claim units must be positive", ErrInvalidQuota)
	}
	if !claim.ReclaimPolicy.valid() {
		return fmt.Errorf("%w: unsupported quota claim reclaim policy %q", ErrInvalidQuota, claim.ReclaimPolicy)
	}
	return nil
}

// NormalizeQuotaClaims validates claims and returns a stable, independently
// owned dimension order. One stage may claim one unit total per dimension;
// callers must aggregate intentionally before freezing a descriptor.
func NormalizeQuotaClaims(claims []QuotaClaim) ([]QuotaClaim, error) {
	normalized := append([]QuotaClaim(nil), claims...)
	sort.Slice(normalized, func(left, right int) bool {
		return normalized[left].Dimension < normalized[right].Dimension
	})
	for index, claim := range normalized {
		if err := claim.Validate(); err != nil {
			return nil, err
		}
		if index > 0 && normalized[index-1].Dimension == claim.Dimension {
			return nil, fmt.Errorf("%w: duplicate quota claim dimension %q", ErrInvalidQuota, claim.Dimension)
		}
	}
	return normalized, nil
}

// Validate verifies a positive, explicitly scoped quota request.
func (request QuotaRequest) Validate() error {
	if err := validateRequired("quota dimension", request.Dimension, ErrInvalidQuota); err != nil {
		return err
	}
	if request.Units <= 0 {
		return fmt.Errorf("%w: quota units must be positive", ErrInvalidQuota)
	}
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if !request.ReclaimPolicy.valid() {
		return fmt.Errorf("%w: unsupported reclaim policy %q", ErrInvalidQuota, request.ReclaimPolicy)
	}
	return nil
}

// QuotaLimit configures one durable ledger bucket. Version is an optimistic
// concurrency value; zero is normalized to one when installed.
type QuotaLimit struct {
	Scope     ResourceScope `json:"scope"`
	Dimension string        `json:"dimension"`
	Limit     int64         `json:"limit"`
	Version   int64         `json:"version"`
}

func (limit QuotaLimit) validate() error {
	if err := limit.Scope.Validate(); err != nil {
		return err
	}
	if err := validateRequired("quota limit dimension", limit.Dimension, ErrInvalidQuota); err != nil {
		return err
	}
	if limit.Limit < 0 {
		return fmt.Errorf("%w: quota limit cannot be negative", ErrInvalidQuota)
	}
	if limit.Version < 0 {
		return fmt.Errorf("%w: quota limit version cannot be negative", ErrInvalidQuota)
	}
	return nil
}

type quotaBucketKey struct {
	Scope     ResourceScope
	Dimension string
}

func quotaKey(scope ResourceScope, dimension string) quotaBucketKey {
	return quotaBucketKey{Scope: scope, Dimension: dimension}
}

type quotaBucket struct {
	Limit    int64
	Consumed int64
	Reserved int64
	Version  int64
}

func (bucket quotaBucket) available() int64 {
	available := bucket.Limit - bucket.Consumed - bucket.Reserved
	if available < 0 {
		return 0
	}
	return available
}

// QuotaSnapshot is the current projection for one configured bucket.
type QuotaSnapshot struct {
	Scope     ResourceScope `json:"scope"`
	Dimension string        `json:"dimension"`
	Limit     int64         `json:"limit"`
	Consumed  int64         `json:"consumed"`
	Reserved  int64         `json:"reserved"`
	Available int64         `json:"available"`
	Version   int64         `json:"version"`
	AsOf      time.Time     `json:"as_of"`
}

// LeaseStatus describes whether a reservation may accept usage or must first
// be reconciled. Expired and uncertain reservations deliberately retain their
// unused capacity until a trustworthy outcome is recorded.
type LeaseStatus string

const (
	LeaseActive    LeaseStatus = "active"
	LeaseSettled   LeaseStatus = "settled"
	LeaseUncertain LeaseStatus = "uncertain"
	LeaseExpired   LeaseStatus = "expired"
)

func (status LeaseStatus) valid() bool {
	switch status {
	case LeaseActive, LeaseSettled, LeaseUncertain, LeaseExpired:
		return true
	default:
		return false
	}
}

// QuotaLease is an append-only-accounting projection of one reservation.
// Consumed units are never decremented by settlement or later continuation.
type QuotaLease struct {
	LeaseID       string        `json:"lease_id"`
	Owner         string        `json:"owner"`
	Scope         ResourceScope `json:"scope"`
	Dimension     string        `json:"dimension"`
	Reserved      int64         `json:"reserved"`
	Consumed      int64         `json:"consumed"`
	Released      int64         `json:"released"`
	ReclaimPolicy ReclaimPolicy `json:"reclaim_policy"`
	FencingToken  uint64        `json:"fencing_token"`
	ExpiresAt     time.Time     `json:"expires_at"`
	Status        LeaseStatus   `json:"status"`
	Version       int64         `json:"version"`
}

func (lease QuotaLease) remaining() int64 {
	remaining := lease.Reserved - lease.Consumed - lease.Released
	if remaining < 0 {
		return 0
	}
	return remaining
}

// ReserveRequest creates one lease. LeaseID and IdempotencyKey are supplied by
// the durable caller rather than generated by process-local state.
type ReserveRequest struct {
	LeaseID        string        `json:"lease_id"`
	IdempotencyKey string        `json:"idempotency_key"`
	Owner          string        `json:"owner"`
	Claim          QuotaRequest  `json:"claim"`
	TTL            time.Duration `json:"ttl"`
}

func (request ReserveRequest) validate() error {
	if err := validateRequired("quota lease id", request.LeaseID, ErrInvalidQuota); err != nil {
		return err
	}
	if err := validateRequired("quota reserve idempotency key", request.IdempotencyKey, ErrInvalidQuota); err != nil {
		return err
	}
	if err := validateRequired("quota lease owner", request.Owner, ErrInvalidQuota); err != nil {
		return err
	}
	if request.TTL <= 0 {
		return fmt.Errorf("%w: quota lease ttl must be positive", ErrInvalidQuota)
	}
	return request.Claim.Validate()
}

// UsageEvent records usage known to have occurred. EventID is an immutable
// operation key so duplicate reports cannot charge a lease twice.
type UsageEvent struct {
	EventID      string    `json:"event_id"`
	LeaseID      string    `json:"lease_id"`
	FencingToken uint64    `json:"fencing_token"`
	Units        int64     `json:"units"`
	OccurredAt   time.Time `json:"occurred_at"`
}

func (event UsageEvent) validate() error {
	if err := validateRequired("usage event id", event.EventID, ErrInvalidQuota); err != nil {
		return err
	}
	if err := validateRequired("usage event lease id", event.LeaseID, ErrInvalidQuota); err != nil {
		return err
	}
	if event.FencingToken == 0 {
		return fmt.Errorf("%w: usage event fencing token is required", ErrInvalidQuota)
	}
	if event.Units <= 0 {
		return fmt.Errorf("%w: usage event units must be positive", ErrInvalidQuota)
	}
	if event.OccurredAt.IsZero() {
		return fmt.Errorf("%w: usage event time is required", ErrInvalidQuota)
	}
	return nil
}

// LeaseHeartbeat extends a live lease without changing its fencing token.
// HeartbeatID makes retries idempotent.
type LeaseHeartbeat struct {
	HeartbeatID  string        `json:"heartbeat_id"`
	LeaseID      string        `json:"lease_id"`
	Owner        string        `json:"owner"`
	FencingToken uint64        `json:"fencing_token"`
	TTL          time.Duration `json:"ttl"`
}

func (heartbeat LeaseHeartbeat) validate() error {
	if err := validateRequired("quota heartbeat id", heartbeat.HeartbeatID, ErrInvalidQuota); err != nil {
		return err
	}
	if err := validateRequired("quota heartbeat lease id", heartbeat.LeaseID, ErrInvalidQuota); err != nil {
		return err
	}
	if err := validateRequired("quota heartbeat owner", heartbeat.Owner, ErrInvalidQuota); err != nil {
		return err
	}
	if heartbeat.FencingToken == 0 || heartbeat.TTL <= 0 {
		return fmt.Errorf("%w: quota heartbeat needs fencing token and positive ttl", ErrInvalidQuota)
	}
	return nil
}

// SettlementOutcome records whether the caller knows the final execution
// result. Uncertain keeps the remaining capacity reserved until Reconcile.
type SettlementOutcome string

const (
	SettlementCompleted SettlementOutcome = "completed"
	SettlementCanceled  SettlementOutcome = "canceled"
	SettlementUncertain SettlementOutcome = "uncertain"
)

func (outcome SettlementOutcome) valid() bool {
	switch outcome {
	case SettlementCompleted, SettlementCanceled, SettlementUncertain:
		return true
	default:
		return false
	}
}

// SettlementRequest settles a live lease after all known usage events have
// been charged. It may not resolve an expired or uncertain lease; use
// QuotaReconcileRequest for that explicit recovery path.
type SettlementRequest struct {
	SettlementID   string            `json:"settlement_id"`
	IdempotencyKey string            `json:"idempotency_key"`
	LeaseID        string            `json:"lease_id"`
	Owner          string            `json:"owner"`
	FencingToken   uint64            `json:"fencing_token"`
	Outcome        SettlementOutcome `json:"outcome"`
}

func (request SettlementRequest) validate() error {
	if err := validateRequired("quota settlement id", request.SettlementID, ErrInvalidQuota); err != nil {
		return err
	}
	if err := validateRequired("quota settlement idempotency key", request.IdempotencyKey, ErrInvalidQuota); err != nil {
		return err
	}
	if err := validateRequired("quota settlement lease id", request.LeaseID, ErrInvalidQuota); err != nil {
		return err
	}
	if err := validateRequired("quota settlement owner", request.Owner, ErrInvalidQuota); err != nil {
		return err
	}
	if request.FencingToken == 0 || !request.Outcome.valid() {
		return fmt.Errorf("%w: quota settlement has invalid token or outcome", ErrInvalidQuota)
	}
	return nil
}

// QuotaReconcileRequest settles an expired or uncertain lease only after a
// recovery adapter has established a known outcome.
type QuotaReconcileRequest struct {
	ReconcileID    string            `json:"reconcile_id"`
	IdempotencyKey string            `json:"idempotency_key"`
	LeaseID        string            `json:"lease_id"`
	Owner          string            `json:"owner"`
	FencingToken   uint64            `json:"fencing_token"`
	Outcome        SettlementOutcome `json:"outcome"`
}

func (request QuotaReconcileRequest) validate() error {
	if err := validateRequired("quota reconcile id", request.ReconcileID, ErrInvalidQuota); err != nil {
		return err
	}
	if err := validateRequired("quota reconcile idempotency key", request.IdempotencyKey, ErrInvalidQuota); err != nil {
		return err
	}
	if err := validateRequired("quota reconcile lease id", request.LeaseID, ErrInvalidQuota); err != nil {
		return err
	}
	if err := validateRequired("quota reconcile owner", request.Owner, ErrInvalidQuota); err != nil {
		return err
	}
	if request.FencingToken == 0 || (request.Outcome != SettlementCompleted && request.Outcome != SettlementCanceled) {
		return fmt.Errorf("%w: quota reconcile needs a known outcome and fencing token", ErrInvalidQuota)
	}
	return nil
}

// QuotaSettlement is the immutable result of settling or reconciling one
// lease. The returned Lease is a snapshot, not a mutable handle.
type QuotaSettlement struct {
	SettlementID string            `json:"settlement_id"`
	LeaseID      string            `json:"lease_id"`
	Outcome      SettlementOutcome `json:"outcome"`
	Consumed     int64             `json:"consumed"`
	Released     int64             `json:"released"`
	SettledAt    time.Time         `json:"settled_at"`
	Lease        QuotaLease        `json:"lease"`
}

func (settlement QuotaSettlement) clone() QuotaSettlement { return settlement }

// BudgetGrantRequest increases a configured quota bucket using an optimistic
// version and a durable idempotency key. Authorization belongs to a domain
// adapter; the kernel only protects the accounting contract.
type BudgetGrantRequest struct {
	GrantID         string        `json:"grant_id"`
	IdempotencyKey  string        `json:"idempotency_key"`
	Scope           ResourceScope `json:"scope"`
	Dimension       string        `json:"dimension"`
	Delta           int64         `json:"delta"`
	ExpectedVersion int64         `json:"expected_version"`
	Actor           string        `json:"actor"`
	Reason          string        `json:"reason"`
}

func (request BudgetGrantRequest) validate() error {
	if err := validateRequired("budget grant id", request.GrantID, ErrInvalidQuota); err != nil {
		return err
	}
	if err := validateRequired("budget grant idempotency key", request.IdempotencyKey, ErrInvalidQuota); err != nil {
		return err
	}
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if err := validateRequired("budget grant dimension", request.Dimension, ErrInvalidQuota); err != nil {
		return err
	}
	if request.Delta <= 0 || request.ExpectedVersion <= 0 {
		return fmt.Errorf("%w: budget grant delta and expected version must be positive", ErrInvalidQuota)
	}
	if err := validateRequired("budget grant actor", request.Actor, ErrInvalidQuota); err != nil {
		return err
	}
	return validateRequired("budget grant reason", request.Reason, ErrInvalidQuota)
}

// BudgetGrant records an accepted grant and resulting bucket version.
type BudgetGrant struct {
	GrantID         string        `json:"grant_id"`
	Scope           ResourceScope `json:"scope"`
	Dimension       string        `json:"dimension"`
	Delta           int64         `json:"delta"`
	Actor           string        `json:"actor"`
	Reason          string        `json:"reason"`
	PreviousVersion int64         `json:"previous_version"`
	Version         int64         `json:"version"`
	Limit           int64         `json:"limit"`
	GrantedAt       time.Time     `json:"granted_at"`
}

// QuotaManager is the generic durable-ledger contract. Implementations must
// make each idempotency key replay-safe and must never reverse charged usage.
type QuotaManager interface {
	Reserve(context.Context, ReserveRequest) (QuotaLease, error)
	Charge(context.Context, UsageEvent) (QuotaSnapshot, error)
	Heartbeat(context.Context, LeaseHeartbeat) (QuotaLease, error)
	Settle(context.Context, SettlementRequest) (QuotaSettlement, error)
	Reconcile(context.Context, QuotaReconcileRequest) (QuotaSettlement, error)
	Grant(context.Context, BudgetGrantRequest) (BudgetGrant, error)
}

// InMemoryQuotaManager is a mutex-protected deterministic implementation for
// non-domain workflow tests and local adapters. It uses caller-issued durable
// IDs, making all results reproducible with a controlled Clock.
type InMemoryQuotaManager struct {
	mu sync.Mutex

	clock Clock

	buckets map[quotaBucketKey]*quotaBucket
	leases  map[string]*QuotaLease

	reserveByKey    map[string]reserveRecord
	usageByID       map[string]UsageEvent
	heartbeatByID   map[string]quotaHeartbeatRecord
	settlementByKey map[string]settlementRecord
	grantByKey      map[string]grantRecord

	nextFencingToken uint64
}

type reserveRecord struct {
	request ReserveRequest
	leaseID string
}

type quotaHeartbeatRecord struct {
	heartbeat LeaseHeartbeat
	lease     QuotaLease
}

type settlementRecord struct {
	requestFingerprint string
	settlement         QuotaSettlement
}

type grantRecord struct {
	request BudgetGrantRequest
	grant   BudgetGrant
}

// NewInMemoryQuotaManager installs a finite set of generic buckets. Passing a
// nil clock uses UTC wall time; deterministic tests should inject a Clock.
func NewInMemoryQuotaManager(limits []QuotaLimit, clock Clock) (*InMemoryQuotaManager, error) {
	manager := &InMemoryQuotaManager{
		clock:           resolveClock(clock),
		buckets:         make(map[quotaBucketKey]*quotaBucket, len(limits)),
		leases:          make(map[string]*QuotaLease),
		reserveByKey:    make(map[string]reserveRecord),
		usageByID:       make(map[string]UsageEvent),
		heartbeatByID:   make(map[string]quotaHeartbeatRecord),
		settlementByKey: make(map[string]settlementRecord),
		grantByKey:      make(map[string]grantRecord),
	}
	for _, limit := range limits {
		if err := limit.validate(); err != nil {
			return nil, err
		}
		key := quotaKey(limit.Scope, limit.Dimension)
		if _, exists := manager.buckets[key]; exists {
			return nil, fmt.Errorf("%w: duplicate quota bucket %q/%q", ErrInvalidQuota, limit.Scope.Kind, limit.Dimension)
		}
		version := limit.Version
		if version == 0 {
			version = 1
		}
		manager.buckets[key] = &quotaBucket{Limit: limit.Limit, Version: version}
	}
	return manager, nil
}

// Reserve atomically creates one active lease or returns a prior exact result
// for the same idempotency key. A rejected reservation does not consume the
// key, allowing a later explicitly retried request after capacity changes.
func (manager *InMemoryQuotaManager) Reserve(ctx context.Context, request ReserveRequest) (QuotaLease, error) {
	if err := contextError(ctx); err != nil {
		return QuotaLease{}, err
	}
	if err := request.validate(); err != nil {
		return QuotaLease{}, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	now := manager.now()
	manager.expireLeasesLocked(now)
	if record, exists := manager.reserveByKey[request.IdempotencyKey]; exists {
		if !sameReserveRequest(record.request, request) {
			return QuotaLease{}, fmt.Errorf("%w: reserve key %q", ErrIdempotencyConflict, request.IdempotencyKey)
		}
		return manager.cloneLeaseByID(record.leaseID)
	}
	if _, exists := manager.leases[request.LeaseID]; exists {
		return QuotaLease{}, fmt.Errorf("%w: quota lease id %q already exists", ErrIdempotencyConflict, request.LeaseID)
	}
	lease, err := manager.reserveLocked(request, now)
	if err != nil {
		return QuotaLease{}, err
	}
	manager.reserveByKey[request.IdempotencyKey] = reserveRecord{request: request, leaseID: lease.LeaseID}
	return lease, nil
}

func (manager *InMemoryQuotaManager) reserveLocked(request ReserveRequest, now time.Time) (QuotaLease, error) {
	bucket, exists := manager.buckets[quotaKey(request.Claim.Scope, request.Claim.Dimension)]
	if !exists {
		return QuotaLease{}, fmt.Errorf("%w: %s/%s", ErrQuotaNotConfigured, request.Claim.Scope.Kind, request.Claim.Dimension)
	}
	if request.Claim.Units > bucket.available() {
		return QuotaLease{}, fmt.Errorf("%w: %s/%s requested %d, available %d", ErrQuotaExhausted, request.Claim.Scope.Kind, request.Claim.Dimension, request.Claim.Units, bucket.available())
	}
	if !canAdd(bucket.Reserved, request.Claim.Units) {
		return QuotaLease{}, fmt.Errorf("%w: quota reservation overflows", ErrInvalidQuota)
	}
	bucket.Reserved += request.Claim.Units
	bucket.Version++
	manager.nextFencingToken++
	lease := &QuotaLease{
		LeaseID:       request.LeaseID,
		Owner:         request.Owner,
		Scope:         request.Claim.Scope,
		Dimension:     request.Claim.Dimension,
		Reserved:      request.Claim.Units,
		ReclaimPolicy: request.Claim.ReclaimPolicy,
		FencingToken:  manager.nextFencingToken,
		ExpiresAt:     now.Add(request.TTL),
		Status:        LeaseActive,
		Version:       1,
	}
	manager.leases[lease.LeaseID] = lease
	return *lease, nil
}

// Charge moves known usage from a live reservation into permanent bucket
// consumption. Replaying the same EventID returns the current bucket snapshot
// without consuming a second time.
func (manager *InMemoryQuotaManager) Charge(ctx context.Context, event UsageEvent) (QuotaSnapshot, error) {
	if err := contextError(ctx); err != nil {
		return QuotaSnapshot{}, err
	}
	if err := event.validate(); err != nil {
		return QuotaSnapshot{}, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	now := manager.now()
	manager.expireLeasesLocked(now)
	if previous, exists := manager.usageByID[event.EventID]; exists {
		if previous != event {
			return QuotaSnapshot{}, fmt.Errorf("%w: usage event %q", ErrIdempotencyConflict, event.EventID)
		}
		lease, exists := manager.leases[event.LeaseID]
		if !exists {
			return QuotaSnapshot{}, fmt.Errorf("%w: usage event lease %q", ErrInvalidQuota, event.LeaseID)
		}
		return manager.snapshotLocked(lease.Scope, lease.Dimension, now)
	}
	lease, exists := manager.leases[event.LeaseID]
	if !exists {
		return QuotaSnapshot{}, fmt.Errorf("%w: quota lease %q", ErrInvalidQuota, event.LeaseID)
	}
	if err := validateLeaseOwnerAndToken(*lease, "", event.FencingToken); err != nil {
		return QuotaSnapshot{}, err
	}
	if lease.Status == LeaseExpired {
		return QuotaSnapshot{}, ErrQuotaLeaseExpired
	}
	if lease.Status != LeaseActive {
		return QuotaSnapshot{}, fmt.Errorf("%w: quota lease %q is %s", ErrInvalidQuota, lease.LeaseID, lease.Status)
	}
	if event.Units > lease.remaining() {
		return QuotaSnapshot{}, fmt.Errorf("%w: usage event exceeds remaining reservation", ErrQuotaExhausted)
	}
	bucket := manager.buckets[quotaKey(lease.Scope, lease.Dimension)]
	if !canAdd(bucket.Consumed, event.Units) {
		return QuotaSnapshot{}, fmt.Errorf("%w: quota consumption overflows", ErrInvalidQuota)
	}
	lease.Consumed += event.Units
	bucket.Reserved -= event.Units
	bucket.Consumed += event.Units
	bucket.Version++
	lease.Version++
	manager.usageByID[event.EventID] = event
	return manager.snapshotLocked(lease.Scope, lease.Dimension, now)
}

// Heartbeat renews an active lease and makes duplicate heartbeat delivery
// harmless. A heartbeat cannot resurrect an expired reservation.
func (manager *InMemoryQuotaManager) Heartbeat(ctx context.Context, heartbeat LeaseHeartbeat) (QuotaLease, error) {
	if err := contextError(ctx); err != nil {
		return QuotaLease{}, err
	}
	if err := heartbeat.validate(); err != nil {
		return QuotaLease{}, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	now := manager.now()
	manager.expireLeasesLocked(now)
	if previous, exists := manager.heartbeatByID[heartbeat.HeartbeatID]; exists {
		if previous.heartbeat != heartbeat {
			return QuotaLease{}, fmt.Errorf("%w: heartbeat %q", ErrIdempotencyConflict, heartbeat.HeartbeatID)
		}
		return previous.lease, nil
	}
	lease, exists := manager.leases[heartbeat.LeaseID]
	if !exists {
		return QuotaLease{}, fmt.Errorf("%w: quota lease %q", ErrInvalidQuota, heartbeat.LeaseID)
	}
	if err := validateLeaseOwnerAndToken(*lease, heartbeat.Owner, heartbeat.FencingToken); err != nil {
		return QuotaLease{}, err
	}
	if lease.Status == LeaseExpired {
		return QuotaLease{}, ErrQuotaLeaseExpired
	}
	if lease.Status != LeaseActive {
		return QuotaLease{}, fmt.Errorf("%w: quota lease %q is %s", ErrInvalidQuota, lease.LeaseID, lease.Status)
	}
	lease.ExpiresAt = now.Add(heartbeat.TTL)
	lease.Version++
	copyLease := *lease
	manager.heartbeatByID[heartbeat.HeartbeatID] = quotaHeartbeatRecord{heartbeat: heartbeat, lease: copyLease}
	return copyLease, nil
}

// Settle records a known or uncertain terminal result for an active lease.
// Unknown results retain unconsumed reservation until Reconcile is called.
func (manager *InMemoryQuotaManager) Settle(ctx context.Context, request SettlementRequest) (QuotaSettlement, error) {
	if err := contextError(ctx); err != nil {
		return QuotaSettlement{}, err
	}
	if err := request.validate(); err != nil {
		return QuotaSettlement{}, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	now := manager.now()
	manager.expireLeasesLocked(now)
	fingerprint := settlementRequestFingerprint(request)
	if record, exists := manager.settlementByKey[request.IdempotencyKey]; exists {
		if record.requestFingerprint != fingerprint {
			return QuotaSettlement{}, fmt.Errorf("%w: settlement key %q", ErrIdempotencyConflict, request.IdempotencyKey)
		}
		return record.settlement.clone(), nil
	}
	lease, exists := manager.leases[request.LeaseID]
	if !exists {
		return QuotaSettlement{}, fmt.Errorf("%w: quota lease %q", ErrInvalidQuota, request.LeaseID)
	}
	if err := validateLeaseOwnerAndToken(*lease, request.Owner, request.FencingToken); err != nil {
		return QuotaSettlement{}, err
	}
	if lease.Status == LeaseExpired {
		return QuotaSettlement{}, ErrQuotaLeaseExpired
	}
	if lease.Status != LeaseActive {
		return QuotaSettlement{}, fmt.Errorf("%w: quota lease %q is %s", ErrInvalidQuota, lease.LeaseID, lease.Status)
	}
	settlement, err := manager.settleLocked(request.SettlementID, request.LeaseID, request.Outcome, now)
	if err != nil {
		return QuotaSettlement{}, err
	}
	manager.settlementByKey[request.IdempotencyKey] = settlementRecord{requestFingerprint: fingerprint, settlement: settlement}
	return settlement.clone(), nil
}

// Reconcile resolves an expired or uncertain reservation only after an
// external recovery adapter has produced a known completed or canceled fact.
func (manager *InMemoryQuotaManager) Reconcile(ctx context.Context, request QuotaReconcileRequest) (QuotaSettlement, error) {
	if err := contextError(ctx); err != nil {
		return QuotaSettlement{}, err
	}
	if err := request.validate(); err != nil {
		return QuotaSettlement{}, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	now := manager.now()
	manager.expireLeasesLocked(now)
	fingerprint := reconcileRequestFingerprint(request)
	if record, exists := manager.settlementByKey[request.IdempotencyKey]; exists {
		if record.requestFingerprint != fingerprint {
			return QuotaSettlement{}, fmt.Errorf("%w: reconcile key %q", ErrIdempotencyConflict, request.IdempotencyKey)
		}
		return record.settlement.clone(), nil
	}
	lease, exists := manager.leases[request.LeaseID]
	if !exists {
		return QuotaSettlement{}, fmt.Errorf("%w: quota lease %q", ErrInvalidQuota, request.LeaseID)
	}
	if err := validateLeaseOwnerAndToken(*lease, request.Owner, request.FencingToken); err != nil {
		return QuotaSettlement{}, err
	}
	if lease.Status != LeaseUncertain && lease.Status != LeaseExpired {
		return QuotaSettlement{}, fmt.Errorf("%w: quota lease %q is not recoverable", ErrInvalidQuota, lease.LeaseID)
	}
	settlement, err := manager.settleLocked(request.ReconcileID, request.LeaseID, request.Outcome, now)
	if err != nil {
		return QuotaSettlement{}, err
	}
	manager.settlementByKey[request.IdempotencyKey] = settlementRecord{requestFingerprint: fingerprint, settlement: settlement}
	return settlement.clone(), nil
}

func (manager *InMemoryQuotaManager) settleLocked(settlementID, leaseID string, outcome SettlementOutcome, now time.Time) (QuotaSettlement, error) {
	lease := manager.leases[leaseID]
	bucket := manager.buckets[quotaKey(lease.Scope, lease.Dimension)]
	remaining := lease.remaining()
	previousReleased := lease.Released
	if outcome == SettlementUncertain {
		lease.Status = LeaseUncertain
		lease.Version++
		return QuotaSettlement{
			SettlementID: settlementID,
			LeaseID:      leaseID,
			Outcome:      outcome,
			Consumed:     lease.Consumed,
			Released:     0,
			SettledAt:    now,
			Lease:        *lease,
		}, nil
	}
	switch lease.ReclaimPolicy {
	case ReclaimUnused:
		lease.Released += remaining
		bucket.Reserved -= remaining
	case ReclaimNever:
		if !canAdd(bucket.Consumed, remaining) {
			return QuotaSettlement{}, fmt.Errorf("%w: quota settlement consumption overflows", ErrInvalidQuota)
		}
		lease.Consumed += remaining
		bucket.Reserved -= remaining
		bucket.Consumed += remaining
	default:
		return QuotaSettlement{}, fmt.Errorf("%w: lease %q has invalid reclaim policy", ErrInvalidQuota, lease.LeaseID)
	}
	bucket.Version++
	lease.Status = LeaseSettled
	lease.Version++
	return QuotaSettlement{
		SettlementID: settlementID,
		LeaseID:      leaseID,
		Outcome:      outcome,
		Consumed:     lease.Consumed,
		Released:     lease.Released - previousReleased,
		SettledAt:    now,
		Lease:        *lease,
	}, nil
}

// Grant applies an idempotent optimistic increase to one bucket.
func (manager *InMemoryQuotaManager) Grant(ctx context.Context, request BudgetGrantRequest) (BudgetGrant, error) {
	if err := contextError(ctx); err != nil {
		return BudgetGrant{}, err
	}
	if err := request.validate(); err != nil {
		return BudgetGrant{}, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	now := manager.now()
	if record, exists := manager.grantByKey[request.IdempotencyKey]; exists {
		if record.request != request {
			return BudgetGrant{}, fmt.Errorf("%w: grant key %q", ErrIdempotencyConflict, request.IdempotencyKey)
		}
		return record.grant, nil
	}
	bucket, exists := manager.buckets[quotaKey(request.Scope, request.Dimension)]
	if !exists {
		return BudgetGrant{}, fmt.Errorf("%w: %s/%s", ErrQuotaNotConfigured, request.Scope.Kind, request.Dimension)
	}
	if bucket.Version != request.ExpectedVersion {
		return BudgetGrant{}, fmt.Errorf("%w: expected %d, current %d", ErrStaleQuotaGrant, request.ExpectedVersion, bucket.Version)
	}
	if !canAdd(bucket.Limit, request.Delta) {
		return BudgetGrant{}, fmt.Errorf("%w: quota grant overflows limit", ErrInvalidQuota)
	}
	previousVersion := bucket.Version
	bucket.Limit += request.Delta
	bucket.Version++
	grant := BudgetGrant{
		GrantID:         request.GrantID,
		Scope:           request.Scope,
		Dimension:       request.Dimension,
		Delta:           request.Delta,
		Actor:           request.Actor,
		Reason:          request.Reason,
		PreviousVersion: previousVersion,
		Version:         bucket.Version,
		Limit:           bucket.Limit,
		GrantedAt:       now,
	}
	manager.grantByKey[request.IdempotencyKey] = grantRecord{request: request, grant: grant}
	return grant, nil
}

// Snapshot returns the current immutable projection of one configured bucket.
func (manager *InMemoryQuotaManager) Snapshot(ctx context.Context, scope ResourceScope, dimension string) (QuotaSnapshot, error) {
	if err := contextError(ctx); err != nil {
		return QuotaSnapshot{}, err
	}
	if err := scope.Validate(); err != nil {
		return QuotaSnapshot{}, err
	}
	if err := validateRequired("quota snapshot dimension", dimension, ErrInvalidQuota); err != nil {
		return QuotaSnapshot{}, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	now := manager.now()
	manager.expireLeasesLocked(now)
	return manager.snapshotLocked(scope, dimension, now)
}

// Lease returns a copy of any known lease, including settled history.
func (manager *InMemoryQuotaManager) Lease(ctx context.Context, leaseID string) (QuotaLease, bool, error) {
	if err := contextError(ctx); err != nil {
		return QuotaLease{}, false, err
	}
	if err := validateRequired("quota lease id", leaseID, ErrInvalidQuota); err != nil {
		return QuotaLease{}, false, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.expireLeasesLocked(manager.now())
	lease, exists := manager.leases[leaseID]
	if !exists {
		return QuotaLease{}, false, nil
	}
	return *lease, true, nil
}

func (manager *InMemoryQuotaManager) snapshotLocked(scope ResourceScope, dimension string, now time.Time) (QuotaSnapshot, error) {
	bucket, exists := manager.buckets[quotaKey(scope, dimension)]
	if !exists {
		return QuotaSnapshot{}, fmt.Errorf("%w: %s/%s", ErrQuotaNotConfigured, scope.Kind, dimension)
	}
	return QuotaSnapshot{
		Scope:     scope,
		Dimension: dimension,
		Limit:     bucket.Limit,
		Consumed:  bucket.Consumed,
		Reserved:  bucket.Reserved,
		Available: bucket.available(),
		Version:   bucket.Version,
		AsOf:      now,
	}, nil
}

func (manager *InMemoryQuotaManager) cloneLeaseByID(leaseID string) (QuotaLease, error) {
	lease, exists := manager.leases[leaseID]
	if !exists {
		return QuotaLease{}, fmt.Errorf("%w: stored quota lease %q", ErrInvalidQuota, leaseID)
	}
	return *lease, nil
}

func (manager *InMemoryQuotaManager) expireLeasesLocked(now time.Time) {
	for _, lease := range manager.leases {
		if lease.Status == LeaseActive && !now.Before(lease.ExpiresAt) {
			lease.Status = LeaseExpired
			lease.Version++
		}
	}
}

func (manager *InMemoryQuotaManager) now() time.Time { return manager.clock.Now().UTC() }

func validateLeaseOwnerAndToken(lease QuotaLease, owner string, token uint64) error {
	if owner != "" && lease.Owner != owner {
		return fmt.Errorf("%w: quota lease owner does not match", ErrInvalidQuota)
	}
	if lease.FencingToken != token {
		return fmt.Errorf("%w: quota lease fencing token does not match", ErrInvalidQuota)
	}
	return nil
}

func sameReserveRequest(left, right ReserveRequest) bool {
	return left == right
}

func settlementRequestFingerprint(request SettlementRequest) string {
	return "settle\x00" + request.SettlementID + "\x00" + request.LeaseID + "\x00" + request.Owner + "\x00" + fmt.Sprintf("%d", request.FencingToken) + "\x00" + string(request.Outcome)
}

func reconcileRequestFingerprint(request QuotaReconcileRequest) string {
	return "reconcile\x00" + request.ReconcileID + "\x00" + request.LeaseID + "\x00" + request.Owner + "\x00" + fmt.Sprintf("%d", request.FencingToken) + "\x00" + string(request.Outcome)
}

func canAdd(left, right int64) bool {
	return right >= 0 && left <= math.MaxInt64-right
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// sortedQuotaRequests provides a stable helper for callers that want to
// construct canonical multi-scope admission claims without relying on maps.
func sortedQuotaRequests(requests []QuotaRequest) []QuotaRequest {
	copyRequests := append([]QuotaRequest(nil), requests...)
	sort.Slice(copyRequests, func(left, right int) bool {
		if copyRequests[left].Scope.Kind != copyRequests[right].Scope.Kind {
			return copyRequests[left].Scope.Kind < copyRequests[right].Scope.Kind
		}
		if copyRequests[left].Scope.ID != copyRequests[right].Scope.ID {
			return copyRequests[left].Scope.ID < copyRequests[right].Scope.ID
		}
		if copyRequests[left].Dimension != copyRequests[right].Dimension {
			return copyRequests[left].Dimension < copyRequests[right].Dimension
		}
		return copyRequests[left].ReclaimPolicy < copyRequests[right].ReclaimPolicy
	})
	return copyRequests
}
