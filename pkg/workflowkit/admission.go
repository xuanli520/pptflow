package workflowkit

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrInvalidAdmission = errors.New("workflowkit: invalid admission")

// AdmissionReason explains a denied request without exposing a domain policy.
type AdmissionReason string

const (
	AdmissionAccepted       AdmissionReason = "accepted"
	AdmissionQuotaExhausted AdmissionReason = "quota_exhausted"
)

func (reason AdmissionReason) valid() bool {
	return reason == AdmissionAccepted || reason == AdmissionQuotaExhausted
}

// AdmissionRequest reserves all claims together before durable work is queued.
// The caller supplies stable identity and idempotency keys; the controller
// derives one quota lease per claim in canonical claim order.
type AdmissionRequest struct {
	AdmissionID    string         `json:"admission_id"`
	IdempotencyKey string         `json:"idempotency_key"`
	Owner          string         `json:"owner"`
	LeaseTTL       time.Duration  `json:"lease_ttl"`
	Claims         []QuotaRequest `json:"claims"`
}

// Clone returns an independent request snapshot.
func (request AdmissionRequest) Clone() AdmissionRequest {
	request.Claims = append([]QuotaRequest(nil), request.Claims...)
	return request
}

func (request AdmissionRequest) validate() error {
	if err := validateRequired("admission id", request.AdmissionID, ErrInvalidAdmission); err != nil {
		return err
	}
	if err := validateRequired("admission idempotency key", request.IdempotencyKey, ErrInvalidAdmission); err != nil {
		return err
	}
	if err := validateRequired("admission owner", request.Owner, ErrInvalidAdmission); err != nil {
		return err
	}
	if request.LeaseTTL <= 0 {
		return fmt.Errorf("%w: admission lease ttl must be positive", ErrInvalidAdmission)
	}
	if len(request.Claims) == 0 {
		return fmt.Errorf("%w: admission needs at least one claim", ErrInvalidAdmission)
	}
	for _, claim := range request.Claims {
		if err := claim.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidAdmission, err)
		}
	}
	return nil
}

// AdmissionDecision is the immutable result of an atomic admission attempt.
// A quota-exhausted decision is a normal non-error result and is itself
// idempotent for the caller's request key.
type AdmissionDecision struct {
	AdmissionID string          `json:"admission_id"`
	Accepted    bool            `json:"accepted"`
	Reason      AdmissionReason `json:"reason"`
	Leases      []QuotaLease    `json:"leases,omitempty"`
	DecidedAt   time.Time       `json:"decided_at"`
}

// Clone returns an independent decision snapshot.
func (decision AdmissionDecision) Clone() AdmissionDecision {
	decision.Leases = append([]QuotaLease(nil), decision.Leases...)
	return decision
}

// DispatchRequest reserves ephemeral capacity at dispatch time. Its shape is
// intentionally equivalent to AdmissionRequest while keeping queue admission
// and worker-capacity permits distinct in durable history.
type DispatchRequest struct {
	DispatchID     string         `json:"dispatch_id"`
	IdempotencyKey string         `json:"idempotency_key"`
	Owner          string         `json:"owner"`
	LeaseTTL       time.Duration  `json:"lease_ttl"`
	CapacityClaims []QuotaRequest `json:"capacity_claims"`
}

// Clone returns an independent request snapshot.
func (request DispatchRequest) Clone() DispatchRequest {
	request.CapacityClaims = append([]QuotaRequest(nil), request.CapacityClaims...)
	return request
}

func (request DispatchRequest) validate() error {
	if err := validateRequired("dispatch id", request.DispatchID, ErrInvalidAdmission); err != nil {
		return err
	}
	if err := validateRequired("dispatch idempotency key", request.IdempotencyKey, ErrInvalidAdmission); err != nil {
		return err
	}
	if err := validateRequired("dispatch owner", request.Owner, ErrInvalidAdmission); err != nil {
		return err
	}
	if request.LeaseTTL <= 0 || len(request.CapacityClaims) == 0 {
		return fmt.Errorf("%w: dispatch needs positive ttl and at least one capacity claim", ErrInvalidAdmission)
	}
	for _, claim := range request.CapacityClaims {
		if err := claim.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidAdmission, err)
		}
	}
	return nil
}

// DispatchPermit is the result of dispatch-time capacity admission.
type DispatchPermit struct {
	DispatchID string          `json:"dispatch_id"`
	Accepted   bool            `json:"accepted"`
	Reason     AdmissionReason `json:"reason"`
	Leases     []QuotaLease    `json:"leases,omitempty"`
	DecidedAt  time.Time       `json:"decided_at"`
}

// Clone returns an independent permit snapshot.
func (permit DispatchPermit) Clone() DispatchPermit {
	permit.Leases = append([]QuotaLease(nil), permit.Leases...)
	return permit
}

// AdmissionControl represents the two generic admission phases: durable plan
// reservation and short-lived worker capacity permit acquisition.
type AdmissionControl interface {
	AdmitAndReserve(context.Context, AdmissionRequest) (AdmissionDecision, error)
	AdmitDispatch(context.Context, DispatchRequest) (DispatchPermit, error)
}

type admissionRecord struct {
	request  AdmissionRequest
	decision AdmissionDecision
}

type dispatchRecord struct {
	request DispatchRequest
	permit  DispatchPermit
}

// InMemoryAdmissionControl atomically reserves all claims through an
// InMemoryQuotaManager. It never makes a product-specific interpretation of
// claim dimensions or scope kinds.
type InMemoryAdmissionControl struct {
	quotas *InMemoryQuotaManager

	admissions map[string]admissionRecord
	dispatches map[string]dispatchRecord
}

// NewInMemoryAdmissionControl binds admission to one in-memory quota ledger.
func NewInMemoryAdmissionControl(quotas *InMemoryQuotaManager) (*InMemoryAdmissionControl, error) {
	if quotas == nil {
		return nil, fmt.Errorf("%w: quota manager is required", ErrInvalidAdmission)
	}
	return &InMemoryAdmissionControl{
		quotas:     quotas,
		admissions: make(map[string]admissionRecord),
		dispatches: make(map[string]dispatchRecord),
	}, nil
}

// AdmitAndReserve creates all leases or none. A denied quota decision is
// persisted by idempotency key, so a command retry cannot unexpectedly change
// its original admission result after a later grant.
func (control *InMemoryAdmissionControl) AdmitAndReserve(ctx context.Context, request AdmissionRequest) (AdmissionDecision, error) {
	if err := contextError(ctx); err != nil {
		return AdmissionDecision{}, err
	}
	if err := request.validate(); err != nil {
		return AdmissionDecision{}, err
	}
	manager := control.quotas
	manager.mu.Lock()
	defer manager.mu.Unlock()
	now := manager.now()
	manager.expireLeasesLocked(now)
	if record, exists := control.admissions[request.IdempotencyKey]; exists {
		if !sameAdmissionRequest(record.request, request) {
			return AdmissionDecision{}, fmt.Errorf("%w: admission key %q", ErrIdempotencyConflict, request.IdempotencyKey)
		}
		return record.decision.Clone(), nil
	}
	claims := sortedQuotaRequests(request.Claims)
	if err := manager.canReserveClaimsLocked(claims); err != nil {
		if errors.Is(err, ErrQuotaExhausted) {
			decision := AdmissionDecision{AdmissionID: request.AdmissionID, Accepted: false, Reason: AdmissionQuotaExhausted, DecidedAt: now}
			control.admissions[request.IdempotencyKey] = admissionRecord{request: canonicalAdmissionRequest(request), decision: decision.Clone()}
			return decision, nil
		}
		return AdmissionDecision{}, err
	}
	leases, err := manager.reserveClaimsLocked("admission", request.AdmissionID, request.Owner, request.LeaseTTL, claims, now)
	if err != nil {
		return AdmissionDecision{}, err
	}
	decision := AdmissionDecision{AdmissionID: request.AdmissionID, Accepted: true, Reason: AdmissionAccepted, Leases: leases, DecidedAt: now}
	control.admissions[request.IdempotencyKey] = admissionRecord{request: canonicalAdmissionRequest(request), decision: decision.Clone()}
	return decision, nil
}

// AdmitDispatch reserves short-lived generic capacity claims independently of
// queued-work admission while preserving the same all-or-nothing semantics.
func (control *InMemoryAdmissionControl) AdmitDispatch(ctx context.Context, request DispatchRequest) (DispatchPermit, error) {
	if err := contextError(ctx); err != nil {
		return DispatchPermit{}, err
	}
	if err := request.validate(); err != nil {
		return DispatchPermit{}, err
	}
	manager := control.quotas
	manager.mu.Lock()
	defer manager.mu.Unlock()
	now := manager.now()
	manager.expireLeasesLocked(now)
	if record, exists := control.dispatches[request.IdempotencyKey]; exists {
		if !sameDispatchRequest(record.request, request) {
			return DispatchPermit{}, fmt.Errorf("%w: dispatch key %q", ErrIdempotencyConflict, request.IdempotencyKey)
		}
		return record.permit.Clone(), nil
	}
	claims := sortedQuotaRequests(request.CapacityClaims)
	if err := manager.canReserveClaimsLocked(claims); err != nil {
		if errors.Is(err, ErrQuotaExhausted) {
			permit := DispatchPermit{DispatchID: request.DispatchID, Accepted: false, Reason: AdmissionQuotaExhausted, DecidedAt: now}
			control.dispatches[request.IdempotencyKey] = dispatchRecord{request: canonicalDispatchRequest(request), permit: permit.Clone()}
			return permit, nil
		}
		return DispatchPermit{}, err
	}
	leases, err := manager.reserveClaimsLocked("dispatch", request.DispatchID, request.Owner, request.LeaseTTL, claims, now)
	if err != nil {
		return DispatchPermit{}, err
	}
	permit := DispatchPermit{DispatchID: request.DispatchID, Accepted: true, Reason: AdmissionAccepted, Leases: leases, DecidedAt: now}
	control.dispatches[request.IdempotencyKey] = dispatchRecord{request: canonicalDispatchRequest(request), permit: permit.Clone()}
	return permit, nil
}

func (manager *InMemoryQuotaManager) canReserveClaimsLocked(claims []QuotaRequest) error {
	needed := make(map[quotaBucketKey]int64, len(claims))
	for _, claim := range claims {
		key := quotaKey(claim.Scope, claim.Dimension)
		bucket, exists := manager.buckets[key]
		if !exists {
			return fmt.Errorf("%w: %s/%s", ErrQuotaNotConfigured, claim.Scope.Kind, claim.Dimension)
		}
		if !canAdd(needed[key], claim.Units) {
			return fmt.Errorf("%w: combined quota claim overflows", ErrInvalidAdmission)
		}
		needed[key] += claim.Units
		if needed[key] > bucket.available() {
			return fmt.Errorf("%w: %s/%s requested %d, available %d", ErrQuotaExhausted, claim.Scope.Kind, claim.Dimension, needed[key], bucket.available())
		}
	}
	return nil
}

func (manager *InMemoryQuotaManager) reserveClaimsLocked(namespace, requestID, owner string, ttl time.Duration, claims []QuotaRequest, now time.Time) ([]QuotaLease, error) {
	for index := range claims {
		leaseID := derivedLeaseID(namespace, requestID, index+1)
		if _, exists := manager.leases[leaseID]; exists {
			return nil, fmt.Errorf("%w: derived quota lease id %q already exists", ErrIdempotencyConflict, leaseID)
		}
	}
	leases := make([]QuotaLease, 0, len(claims))
	for index, claim := range claims {
		leaseID := derivedLeaseID(namespace, requestID, index+1)
		lease, err := manager.reserveLocked(ReserveRequest{
			LeaseID:        leaseID,
			IdempotencyKey: namespace + "-internal-" + requestID + "-" + fmt.Sprintf("%d", index+1),
			Owner:          owner,
			Claim:          claim,
			TTL:            ttl,
		}, now)
		if err != nil {
			return nil, err
		}
		leases = append(leases, lease)
	}
	return leases, nil
}

func derivedLeaseID(namespace, requestID string, ordinal int) string {
	return namespace + ":" + requestID + ":" + fmt.Sprintf("%d", ordinal)
}

func canonicalAdmissionRequest(request AdmissionRequest) AdmissionRequest {
	request = request.Clone()
	request.Claims = sortedQuotaRequests(request.Claims)
	return request
}

func canonicalDispatchRequest(request DispatchRequest) DispatchRequest {
	request = request.Clone()
	request.CapacityClaims = sortedQuotaRequests(request.CapacityClaims)
	return request
}

func sameAdmissionRequest(left, right AdmissionRequest) bool {
	left = canonicalAdmissionRequest(left)
	right = canonicalAdmissionRequest(right)
	if left.AdmissionID != right.AdmissionID || left.IdempotencyKey != right.IdempotencyKey || left.Owner != right.Owner || left.LeaseTTL != right.LeaseTTL || len(left.Claims) != len(right.Claims) {
		return false
	}
	for index := range left.Claims {
		if left.Claims[index] != right.Claims[index] {
			return false
		}
	}
	return true
}

func sameDispatchRequest(left, right DispatchRequest) bool {
	left = canonicalDispatchRequest(left)
	right = canonicalDispatchRequest(right)
	if left.DispatchID != right.DispatchID || left.IdempotencyKey != right.IdempotencyKey || left.Owner != right.Owner || left.LeaseTTL != right.LeaseTTL || len(left.CapacityClaims) != len(right.CapacityClaims) {
		return false
	}
	for index := range left.CapacityClaims {
		if left.CapacityClaims[index] != right.CapacityClaims[index] {
			return false
		}
	}
	return true
}
