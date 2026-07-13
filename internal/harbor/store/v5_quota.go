package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const quotaAccountV5Select = `
	SELECT id, scope_kind, scope_id, dimension, limit_units, consumed_units,
	       reserved_units, version, created_at, updated_at
	FROM quota_accounts_v5`

const quotaAccountPolicyBindingV11Select = `
	SELECT account_id, policy_id, policy_version, policy_fingerprint,
	       initial_limit_units, bound_at
	FROM quota_account_policy_bindings_v11`

const durableQuotaLeaseV5Select = `
	SELECT id, account_id, admission_id, owner, scope_kind, scope_id, dimension,
	       reserved_units, consumed_units, released_units, reclaim_policy,
	       fencing_token, ttl_ms, expires_at, state, created_at, updated_at, settled_at, version
	FROM quota_leases_v5`

const maxStoreInt64 = int64(^uint64(0) >> 1)

func (s *Store) CreateQuotaAccount(ctx context.Context, request CreateQuotaAccountRequest) (QuotaAccount, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return QuotaAccount{}, err
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return QuotaAccount{}, err
	}
	if err := validateQuotaScope(request.ScopeKind, request.ScopeID); err != nil {
		return QuotaAccount{}, err
	}
	dimension, err := normalizeQuotaDimension(request.Dimension)
	if err != nil {
		return QuotaAccount{}, err
	}
	if request.LimitUnits < 0 {
		return QuotaAccount{}, fmt.Errorf("%w: quota limit cannot be negative", ErrInvalidQuota)
	}
	now := s.now().UTC()
	account := QuotaAccount{
		ID:         id,
		ScopeKind:  request.ScopeKind,
		ScopeID:    strings.TrimSpace(request.ScopeID),
		Dimension:  dimension,
		LimitUnits: request.LimitUnits,
		Version:    1,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return QuotaAccount{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO quota_accounts_v5 (
			id, scope_kind, scope_id, dimension, limit_units, consumed_units,
			reserved_units, version, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 0, 0, ?, ?, ?)
	`, account.ID, account.ScopeKind, account.ScopeID, account.Dimension, account.LimitUnits,
		account.Version, account.CreatedAt, account.UpdatedAt)
	if err != nil {
		if isUniqueConstraint(err) {
			return QuotaAccount{}, fmt.Errorf("%w: quota account %s/%s/%s", ErrIdentityCollision, account.ScopeKind, account.ScopeID, account.Dimension)
		}
		return QuotaAccount{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "quota_account",
		EntityID:    account.ID,
		Action:      "quota_account.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"scope_kind": account.ScopeKind, "scope_id": account.ScopeID, "dimension": account.Dimension, "limit_units": account.LimitUnits}),
		CreatedAt:   now,
	}); err != nil {
		return QuotaAccount{}, err
	}
	if err := tx.Commit(); err != nil {
		return QuotaAccount{}, err
	}
	return account, nil
}

func (s *Store) GetQuotaAccount(ctx context.Context, accountID string) (*QuotaAccount, error) {
	if !isUUIDv7(accountID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	account, err := scanQuotaAccount(s.db.QueryRowContext(ctx, quotaAccountV5Select+" WHERE id = ?", accountID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &account, nil
}

func (s *Store) GetQuotaAccountForScope(ctx context.Context, scopeKind QuotaScopeKind, scopeID, dimension string) (*QuotaAccount, error) {
	if err := validateQuotaScope(scopeKind, scopeID); err != nil {
		return nil, err
	}
	dimension, err := normalizeQuotaDimension(dimension)
	if err != nil {
		return nil, err
	}
	account, err := scanQuotaAccount(s.db.QueryRowContext(ctx, quotaAccountV5Select+" WHERE scope_kind = ? AND scope_id = ? AND dimension = ?", scopeKind, strings.TrimSpace(scopeID), dimension))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &account, nil
}

// GetQuotaAccountPolicyBinding returns the immutable bootstrap provenance for
// one account. A nil result means the account predates policy-backed admission
// or was manually configured; admission may adopt it only at an exact frozen
// limit and will then write a binding in the same transaction.
func (s *Store) GetQuotaAccountPolicyBinding(ctx context.Context, accountID string) (*QuotaAccountPolicyBinding, error) {
	if !isUUIDv7(accountID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	binding, err := scanQuotaAccountPolicyBinding(s.db.QueryRowContext(ctx, quotaAccountPolicyBindingV11Select+" WHERE account_id = ?", accountID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &binding, nil
}

// ListQuotaAccountsForScope returns the durable accounts for one authoritative
// scope. It is read-only so callers can display the exact version needed for a
// later optimistic budget grant without reimplementing quota storage queries.
func (s *Store) ListQuotaAccountsForScope(ctx context.Context, scopeKind QuotaScopeKind, scopeID string) ([]QuotaAccount, error) {
	if err := validateQuotaScope(scopeKind, scopeID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, quotaAccountV5Select+" WHERE scope_kind = ? AND scope_id = ? ORDER BY dimension ASC", scopeKind, strings.TrimSpace(scopeID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accounts := make([]QuotaAccount, 0)
	for rows.Next() {
		account, err := scanQuotaAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func (s *Store) ReserveQuota(ctx context.Context, request QuotaLeaseRequest) (DurableQuotaLease, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return DurableQuotaLease{}, err
	}
	prepared, err := prepareQuotaLeaseRequest(s, request)
	if err != nil {
		return DurableQuotaLease{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DurableQuotaLease{}, err
	}
	defer tx.Rollback()
	if existing, err := getDurableQuotaLeaseByKeyTx(ctx, tx, prepared.IdempotencyKey); err != nil {
		return DurableQuotaLease{}, err
	} else if existing != nil {
		if !sameQuotaLeaseRequest(*existing, prepared) {
			return DurableQuotaLease{}, fmt.Errorf("%w: quota lease key %s", ErrIdempotencyConflict, prepared.IdempotencyKey)
		}
		if err := tx.Commit(); err != nil {
			return DurableQuotaLease{}, err
		}
		return *existing, nil
	}
	account, err := getQuotaAccountForScopeTx(ctx, tx, prepared.ScopeKind, prepared.ScopeID, prepared.Dimension)
	if err != nil {
		return DurableQuotaLease{}, err
	}
	if account.AvailableUnits() < prepared.Units {
		return DurableQuotaLease{}, fmt.Errorf("%w: %s/%s/%s needs %d, available %d", ErrQuotaExhausted, account.ScopeKind, account.ScopeID, account.Dimension, prepared.Units, account.AvailableUnits())
	}
	lease, err := s.reserveQuotaLeaseTx(ctx, tx, account, prepared, s.now().UTC())
	if err != nil {
		return DurableQuotaLease{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:        prepared.Actor,
		EntityType:   "quota_lease",
		EntityID:     lease.ID,
		Action:       "quota_lease.reserved",
		Reason:       prepared.Reason,
		PayloadJSON:  auditPayload(map[string]any{"account_id": lease.AccountID, "dimension": lease.Dimension, "reserved_units": lease.ReservedUnits, "fencing_token": lease.FencingToken, "admission_id": lease.AdmissionID}),
		OperationKey: prepared.IdempotencyKey,
		CreatedAt:    lease.CreatedAt,
	}); err != nil {
		return DurableQuotaLease{}, err
	}
	if err := tx.Commit(); err != nil {
		return DurableQuotaLease{}, err
	}
	return lease, nil
}

func (s *Store) GetDurableQuotaLease(ctx context.Context, leaseID string) (*DurableQuotaLease, error) {
	if !isUUIDv7(leaseID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	lease, err := scanDurableQuotaLease(s.db.QueryRowContext(ctx, durableQuotaLeaseV5Select+" WHERE id = ?", leaseID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &lease, nil
}

// ListDurableQuotaLeasesForScope returns durable reservation history for one
// task or actor scope. It is read-only and includes terminal/uncertain leases
// so an attach surface can distinguish a settled account from unresolved use.
func (s *Store) ListDurableQuotaLeasesForScope(ctx context.Context, scopeKind QuotaScopeKind, scopeID string) ([]DurableQuotaLease, error) {
	if err := validateQuotaScope(scopeKind, scopeID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, durableQuotaLeaseV5Select+`
		WHERE scope_kind = ? AND scope_id = ?
		ORDER BY created_at DESC, id DESC`, scopeKind, strings.TrimSpace(scopeID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	leases := make([]DurableQuotaLease, 0)
	for rows.Next() {
		lease, err := scanDurableQuotaLease(rows)
		if err != nil {
			return nil, err
		}
		leases = append(leases, lease)
	}
	return leases, rows.Err()
}

func (s *Store) RecordQuotaUsage(ctx context.Context, request RecordQuotaUsageRequest) (QuotaAccount, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return QuotaAccount{}, err
	}
	if !isUUIDv7(request.LeaseID) {
		return QuotaAccount{}, ErrInvalidUUIDv7Identity
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return QuotaAccount{}, err
	}
	operationKey, err := normalizeRequired(request.OperationKey, "quota usage operation key")
	if err != nil {
		return QuotaAccount{}, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
	}
	if request.FencingToken == 0 || request.Units <= 0 || request.OccurredAt.IsZero() {
		return QuotaAccount{}, fmt.Errorf("%w: usage requires fencing token, positive units, and occurrence time", ErrInvalidQuota)
	}
	request.OccurredAt = request.OccurredAt.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return QuotaAccount{}, err
	}
	defer tx.Rollback()
	lease, err := getDurableQuotaLeaseTx(ctx, tx, request.LeaseID)
	if err != nil {
		return QuotaAccount{}, err
	}
	if existing, err := getQuotaUsageEventTx(ctx, tx, lease.ID, operationKey); err != nil {
		return QuotaAccount{}, err
	} else if existing != nil {
		if existing.FencingToken != request.FencingToken || existing.Units != request.Units || !existing.OccurredAt.Equal(request.OccurredAt) || existing.Actor != resolveActor(request.Actor) || existing.Reason != strings.TrimSpace(request.Reason) {
			return QuotaAccount{}, fmt.Errorf("%w: quota usage operation key %s", ErrIdempotencyConflict, operationKey)
		}
		account, err := getQuotaAccountTx(ctx, tx, lease.AccountID)
		if err != nil {
			return QuotaAccount{}, err
		}
		if err := tx.Commit(); err != nil {
			return QuotaAccount{}, err
		}
		return account, nil
	}
	now := s.now().UTC()
	if lease.State == DurableQuotaLeaseActive && !lease.ExpiresAt.After(now) {
		if err := s.expireQuotaLeaseTx(ctx, tx, lease, resolveActor(request.Actor), now, "usage observed expired quota lease"); err != nil {
			return QuotaAccount{}, err
		}
		return QuotaAccount{}, fmt.Errorf("%w: quota lease %s", ErrQuotaLeaseExpired, lease.ID)
	}
	if lease.State == DurableQuotaLeaseExpired {
		return QuotaAccount{}, fmt.Errorf("%w: quota lease %s", ErrQuotaLeaseExpired, lease.ID)
	}
	if lease.State != DurableQuotaLeaseActive {
		return QuotaAccount{}, fmt.Errorf("%w: quota lease %s is %s", ErrInvalidQuota, lease.ID, lease.State)
	}
	if lease.FencingToken != request.FencingToken {
		return QuotaAccount{}, fmt.Errorf("%w: quota lease %s", ErrFencingToken, lease.ID)
	}
	if request.Units > lease.RemainingUnits() {
		return QuotaAccount{}, fmt.Errorf("%w: quota lease %s has %d remaining", ErrQuotaExhausted, lease.ID, lease.RemainingUnits())
	}
	account, err := getQuotaAccountTx(ctx, tx, lease.AccountID)
	if err != nil {
		return QuotaAccount{}, err
	}
	if account.ReservedUnits < request.Units {
		return QuotaAccount{}, fmt.Errorf("%w: quota account %s reservation projection is invalid", ErrInvalidQuota, account.ID)
	}
	account.ReservedUnits -= request.Units
	account.ConsumedUnits += request.Units
	account.UpdatedAt = now
	account.Version++
	if err := updateQuotaAccountTx(ctx, tx, account, account.Version-1); err != nil {
		return QuotaAccount{}, err
	}
	lease.ConsumedUnits += request.Units
	lease.UpdatedAt = now
	lease.Version++
	if err := updateDurableQuotaLeaseTx(ctx, tx, lease, lease.Version-1); err != nil {
		return QuotaAccount{}, err
	}
	event := QuotaUsageEvent{
		ID: id, OperationKey: operationKey, LeaseID: lease.ID, FencingToken: request.FencingToken,
		Units: request.Units, OccurredAt: request.OccurredAt, Actor: resolveActor(request.Actor),
		Reason: strings.TrimSpace(request.Reason), RecordedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quota_usage_events_v5 (
			id, operation_key, lease_id, fencing_token, units, occurred_at, actor, reason, recorded_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, event.ID, event.OperationKey, event.LeaseID, int64(event.FencingToken), event.Units,
		event.OccurredAt, event.Actor, event.Reason, event.RecordedAt); err != nil {
		if isUniqueConstraint(err) {
			return QuotaAccount{}, fmt.Errorf("%w: quota usage event %s", ErrIdentityCollision, event.ID)
		}
		return QuotaAccount{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:        request.Actor,
		EntityType:   "quota_lease",
		EntityID:     lease.ID,
		Action:       "quota_lease.charged",
		Reason:       request.Reason,
		PayloadJSON:  auditPayload(map[string]any{"usage_event_id": event.ID, "units": event.Units, "consumed_units": lease.ConsumedUnits}),
		OperationKey: operationKey,
		CreatedAt:    now,
	}); err != nil {
		return QuotaAccount{}, err
	}
	if err := tx.Commit(); err != nil {
		return QuotaAccount{}, err
	}
	return account, nil
}

func (s *Store) HeartbeatQuotaLease(ctx context.Context, request HeartbeatQuotaLeaseRequest) (DurableQuotaLease, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return DurableQuotaLease{}, err
	}
	if !isUUIDv7(request.LeaseID) {
		return DurableQuotaLease{}, ErrInvalidUUIDv7Identity
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return DurableQuotaLease{}, err
	}
	key, err := normalizeRequired(request.IdempotencyKey, "quota heartbeat idempotency key")
	if err != nil {
		return DurableQuotaLease{}, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
	}
	owner, err := normalizeRequired(request.Owner, "quota lease owner")
	if err != nil {
		return DurableQuotaLease{}, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
	}
	if request.FencingToken == 0 || request.TTL <= 0 {
		return DurableQuotaLease{}, fmt.Errorf("%w: quota heartbeat needs fencing token and positive ttl", ErrInvalidQuota)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DurableQuotaLease{}, err
	}
	defer tx.Rollback()
	if record, err := getQuotaHeartbeatByKeyTx(ctx, tx, key); err != nil {
		return DurableQuotaLease{}, err
	} else if record != nil {
		if record.LeaseID != request.LeaseID || record.Owner != owner || record.FencingToken != request.FencingToken || record.TTL != time.Duration(durationMilliseconds(request.TTL))*time.Millisecond {
			return DurableQuotaLease{}, fmt.Errorf("%w: quota heartbeat key %s", ErrIdempotencyConflict, key)
		}
		lease, err := getDurableQuotaLeaseTx(ctx, tx, request.LeaseID)
		if err != nil {
			return DurableQuotaLease{}, err
		}
		if err := tx.Commit(); err != nil {
			return DurableQuotaLease{}, err
		}
		return lease, nil
	}
	lease, err := getDurableQuotaLeaseTx(ctx, tx, request.LeaseID)
	if err != nil {
		return DurableQuotaLease{}, err
	}
	now := s.now().UTC()
	if lease.State == DurableQuotaLeaseActive && !lease.ExpiresAt.After(now) {
		if err := s.expireQuotaLeaseTx(ctx, tx, lease, resolveActor(request.Actor), now, "heartbeat observed expired quota lease"); err != nil {
			return DurableQuotaLease{}, err
		}
		return DurableQuotaLease{}, fmt.Errorf("%w: quota lease %s", ErrQuotaLeaseExpired, lease.ID)
	}
	if lease.State == DurableQuotaLeaseExpired {
		return DurableQuotaLease{}, fmt.Errorf("%w: quota lease %s", ErrQuotaLeaseExpired, lease.ID)
	}
	if lease.State != DurableQuotaLeaseActive {
		return DurableQuotaLease{}, fmt.Errorf("%w: quota lease %s is %s", ErrInvalidQuota, lease.ID, lease.State)
	}
	if lease.Owner != owner || lease.FencingToken != request.FencingToken {
		return DurableQuotaLease{}, fmt.Errorf("%w: quota lease %s", ErrFencingToken, lease.ID)
	}
	lease.ExpiresAt = now.Add(request.TTL)
	lease.UpdatedAt = now
	lease.Version++
	if err := updateDurableQuotaLeaseTx(ctx, tx, lease, lease.Version-1); err != nil {
		return DurableQuotaLease{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quota_heartbeats_v5 (
			id, idempotency_key, lease_id, owner, fencing_token, ttl_ms, expires_at, actor, reason, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, key, lease.ID, owner, int64(request.FencingToken), durationMilliseconds(request.TTL), lease.ExpiresAt,
		resolveActor(request.Actor), strings.TrimSpace(request.Reason), now); err != nil {
		if isUniqueConstraint(err) {
			return DurableQuotaLease{}, fmt.Errorf("%w: quota heartbeat %s", ErrIdentityCollision, id)
		}
		return DurableQuotaLease{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:        request.Actor,
		EntityType:   "quota_lease",
		EntityID:     lease.ID,
		Action:       "quota_lease.heartbeated",
		Reason:       request.Reason,
		PayloadJSON:  auditPayload(map[string]any{"expires_at": lease.ExpiresAt, "fencing_token": lease.FencingToken}),
		OperationKey: key,
		CreatedAt:    now,
	}); err != nil {
		return DurableQuotaLease{}, err
	}
	if err := tx.Commit(); err != nil {
		return DurableQuotaLease{}, err
	}
	return lease, nil
}

func (s *Store) ExpireQuotaLeases(ctx context.Context) (int, error) {
	return s.expireQuotaLeases(ctx, "", nil, "system", "recovery quota lease expiration")
}

// ExpireQuotaLeasesForScope expires only reservations belonging to one task
// or actor scope. It never infers a provider outcome or settles an uncertain
// reservation; those facts remain visible for an explicit reconciler.
func (s *Store) ExpireQuotaLeasesForScope(ctx context.Context, scopeKind QuotaScopeKind, scopeID, actor, reason string) (int, error) {
	if err := validateQuotaScope(scopeKind, scopeID); err != nil {
		return 0, err
	}
	reason, err := normalizeRequired(reason, "quota reconciliation reason")
	if err != nil {
		return 0, err
	}
	return s.expireQuotaLeases(ctx, " AND scope_kind = ? AND scope_id = ?", []any{scopeKind, strings.TrimSpace(scopeID)}, resolveActor(actor), reason)
}

func (s *Store) expireQuotaLeases(ctx context.Context, scopeClause string, scopeArgs []any, actor, reason string) (int, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return 0, err
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	query := durableQuotaLeaseV5Select + " WHERE state = 'active' AND expires_at <= ?" + scopeClause + " ORDER BY expires_at, id"
	args := append([]any{now}, scopeArgs...)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	var leases []DurableQuotaLease
	for rows.Next() {
		lease, err := scanDurableQuotaLease(rows)
		if err != nil {
			_ = rows.Close()
			return 0, err
		}
		leases = append(leases, lease)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, lease := range leases {
		if err := s.expireQuotaLeaseTx(ctx, tx, lease, actor, now, reason); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(leases), nil
}

func (s *Store) SettleQuotaLease(ctx context.Context, request SettleQuotaLeaseRequest) (DurableQuotaSettlement, error) {
	return s.settleDurableQuotaLease(ctx, quotaSettlementInput{
		ID: request.ID, IdempotencyKey: request.IdempotencyKey, LeaseID: request.LeaseID,
		Owner: request.Owner, FencingToken: request.FencingToken, Outcome: request.Outcome,
		Kind: QuotaSettlementDirect, Actor: request.Actor, Reason: request.Reason,
	})
}

func (s *Store) ReconcileQuotaLease(ctx context.Context, request ReconcileQuotaLeaseRequest) (DurableQuotaSettlement, error) {
	if request.Outcome == QuotaSettlementUncertain {
		return DurableQuotaSettlement{}, fmt.Errorf("%w: reconciliation requires a known outcome", ErrInvalidQuota)
	}
	return s.settleDurableQuotaLease(ctx, quotaSettlementInput{
		ID: request.ID, IdempotencyKey: request.IdempotencyKey, LeaseID: request.LeaseID,
		Owner: request.Owner, FencingToken: request.FencingToken, Outcome: request.Outcome,
		Kind: QuotaSettlementReconcile, Actor: request.Actor, Reason: request.Reason,
	})
}

type quotaSettlementInput struct {
	ID             string
	IdempotencyKey string
	LeaseID        string
	Owner          string
	FencingToken   uint64
	Outcome        QuotaSettlementOutcome
	Kind           QuotaSettlementKind
	Actor          string
	Reason         string
}

func (s *Store) settleDurableQuotaLease(ctx context.Context, input quotaSettlementInput) (DurableQuotaSettlement, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return DurableQuotaSettlement{}, err
	}
	if !isUUIDv7(input.LeaseID) {
		return DurableQuotaSettlement{}, ErrInvalidUUIDv7Identity
	}
	id, err := s.newV2ID(input.ID)
	if err != nil {
		return DurableQuotaSettlement{}, err
	}
	key, err := normalizeRequired(input.IdempotencyKey, "quota settlement idempotency key")
	if err != nil {
		return DurableQuotaSettlement{}, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
	}
	owner, err := normalizeRequired(input.Owner, "quota lease owner")
	if err != nil {
		return DurableQuotaSettlement{}, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
	}
	if input.FencingToken == 0 || !validQuotaSettlementKind(input.Kind) || !validQuotaSettlementOutcome(input.Outcome) {
		return DurableQuotaSettlement{}, fmt.Errorf("%w: invalid quota settlement", ErrInvalidQuota)
	}
	if input.Kind == QuotaSettlementReconcile && input.Outcome == QuotaSettlementUncertain {
		return DurableQuotaSettlement{}, fmt.Errorf("%w: reconciliation requires a known outcome", ErrInvalidQuota)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DurableQuotaSettlement{}, err
	}
	defer tx.Rollback()
	if existing, err := getDurableQuotaSettlementByKeyTx(ctx, tx, key); err != nil {
		return DurableQuotaSettlement{}, err
	} else if existing != nil {
		if existing.LeaseID != input.LeaseID || existing.Kind != input.Kind || existing.Outcome != input.Outcome || existing.Owner != owner || existing.FencingToken != input.FencingToken {
			return DurableQuotaSettlement{}, fmt.Errorf("%w: quota settlement key %s", ErrIdempotencyConflict, key)
		}
		lease, err := getDurableQuotaLeaseTx(ctx, tx, existing.LeaseID)
		if err != nil {
			return DurableQuotaSettlement{}, err
		}
		if err := tx.Commit(); err != nil {
			return DurableQuotaSettlement{}, err
		}
		return DurableQuotaSettlement{ID: existing.ID, LeaseID: existing.LeaseID, Kind: existing.Kind, Outcome: existing.Outcome, ConsumedUnits: existing.ConsumedUnits, ReleasedUnits: existing.ReleasedUnits, Actor: existing.Actor, SettledAt: existing.SettledAt, Lease: lease}, nil
	}
	lease, err := getDurableQuotaLeaseTx(ctx, tx, input.LeaseID)
	if err != nil {
		return DurableQuotaSettlement{}, err
	}
	now := s.now().UTC()
	if input.Kind == QuotaSettlementDirect && lease.State == DurableQuotaLeaseActive && !lease.ExpiresAt.After(now) {
		if err := s.expireQuotaLeaseTx(ctx, tx, lease, resolveActor(input.Actor), now, "settlement observed expired quota lease"); err != nil {
			return DurableQuotaSettlement{}, err
		}
		return DurableQuotaSettlement{}, fmt.Errorf("%w: quota lease %s", ErrQuotaLeaseExpired, lease.ID)
	}
	if input.Kind == QuotaSettlementDirect && lease.State != DurableQuotaLeaseActive {
		if lease.State == DurableQuotaLeaseExpired {
			return DurableQuotaSettlement{}, fmt.Errorf("%w: quota lease %s", ErrQuotaLeaseExpired, lease.ID)
		}
		return DurableQuotaSettlement{}, fmt.Errorf("%w: quota lease %s is %s", ErrInvalidQuota, lease.ID, lease.State)
	}
	if input.Kind == QuotaSettlementReconcile && lease.State != DurableQuotaLeaseExpired && lease.State != DurableQuotaLeaseUncertain {
		return DurableQuotaSettlement{}, fmt.Errorf("%w: quota lease %s is not awaiting reconciliation", ErrInvalidQuota, lease.ID)
	}
	if lease.Owner != owner || lease.FencingToken != input.FencingToken {
		return DurableQuotaSettlement{}, fmt.Errorf("%w: quota lease %s", ErrFencingToken, lease.ID)
	}
	account, err := getQuotaAccountTx(ctx, tx, lease.AccountID)
	if err != nil {
		return DurableQuotaSettlement{}, err
	}
	remaining := lease.RemainingUnits()
	if input.Outcome == QuotaSettlementUncertain {
		lease.State = DurableQuotaLeaseUncertain
		lease.UpdatedAt = now
		lease.Version++
		if err := updateDurableQuotaLeaseTx(ctx, tx, lease, lease.Version-1); err != nil {
			return DurableQuotaSettlement{}, err
		}
	} else {
		if account.ReservedUnits < remaining {
			return DurableQuotaSettlement{}, fmt.Errorf("%w: quota account %s reservation projection is invalid", ErrInvalidQuota, account.ID)
		}
		account.ReservedUnits -= remaining
		if lease.ReclaimPolicy == QuotaReclaimNever {
			account.ConsumedUnits += remaining
			lease.ConsumedUnits += remaining
		} else {
			lease.ReleasedUnits += remaining
		}
		account.UpdatedAt = now
		account.Version++
		if err := updateQuotaAccountTx(ctx, tx, account, account.Version-1); err != nil {
			return DurableQuotaSettlement{}, err
		}
		lease.State = DurableQuotaLeaseSettled
		lease.UpdatedAt = now
		lease.SettledAt = &now
		lease.Version++
		if err := updateDurableQuotaLeaseTx(ctx, tx, lease, lease.Version-1); err != nil {
			return DurableQuotaSettlement{}, err
		}
	}
	settlement := durableQuotaSettlementRow{
		ID: id, IdempotencyKey: key, LeaseID: lease.ID, Kind: input.Kind, Outcome: input.Outcome,
		ConsumedUnits: lease.ConsumedUnits, ReleasedUnits: lease.ReleasedUnits, Owner: owner,
		FencingToken: input.FencingToken, Actor: resolveActor(input.Actor), Reason: strings.TrimSpace(input.Reason), SettledAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quota_settlements_v5 (
			id, idempotency_key, lease_id, kind, outcome, consumed_units, released_units,
			owner, fencing_token, actor, reason, settled_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, settlement.ID, settlement.IdempotencyKey, settlement.LeaseID, settlement.Kind, settlement.Outcome,
		settlement.ConsumedUnits, settlement.ReleasedUnits, settlement.Owner, int64(settlement.FencingToken),
		settlement.Actor, settlement.Reason, settlement.SettledAt); err != nil {
		if isUniqueConstraint(err) {
			return DurableQuotaSettlement{}, fmt.Errorf("%w: quota settlement %s", ErrIdentityCollision, settlement.ID)
		}
		return DurableQuotaSettlement{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:        input.Actor,
		EntityType:   "quota_lease",
		EntityID:     lease.ID,
		Action:       "quota_lease." + string(input.Kind),
		Reason:       input.Reason,
		PayloadJSON:  auditPayload(map[string]any{"outcome": input.Outcome, "consumed_units": lease.ConsumedUnits, "released_units": lease.ReleasedUnits, "state": lease.State}),
		OperationKey: key,
		CreatedAt:    now,
	}); err != nil {
		return DurableQuotaSettlement{}, err
	}
	if err := tx.Commit(); err != nil {
		return DurableQuotaSettlement{}, err
	}
	return DurableQuotaSettlement{ID: settlement.ID, LeaseID: settlement.LeaseID, Kind: settlement.Kind, Outcome: settlement.Outcome, ConsumedUnits: settlement.ConsumedUnits, ReleasedUnits: settlement.ReleasedUnits, Actor: settlement.Actor, SettledAt: settlement.SettledAt, Lease: lease}, nil
}

func (s *Store) GrantQuota(ctx context.Context, request GrantBudgetRequest) (DurableBudgetGrant, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return DurableBudgetGrant{}, err
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return DurableBudgetGrant{}, err
	}
	key, err := normalizeRequired(request.IdempotencyKey, "budget grant idempotency key")
	if err != nil {
		return DurableBudgetGrant{}, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
	}
	if err := validateQuotaScope(request.ScopeKind, request.ScopeID); err != nil {
		return DurableBudgetGrant{}, err
	}
	dimension, err := normalizeQuotaDimension(request.Dimension)
	if err != nil {
		return DurableBudgetGrant{}, err
	}
	actor, err := normalizeRequired(request.Actor, "budget grant actor")
	if err != nil {
		return DurableBudgetGrant{}, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
	}
	reason, err := normalizeRequired(request.Reason, "budget grant reason")
	if err != nil {
		return DurableBudgetGrant{}, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
	}
	if request.DeltaUnits <= 0 || request.ExpectedVersion <= 0 {
		return DurableBudgetGrant{}, fmt.Errorf("%w: grant delta and expected version must be positive", ErrInvalidQuota)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DurableBudgetGrant{}, err
	}
	defer tx.Rollback()
	if existing, err := getDurableBudgetGrantByKeyTx(ctx, tx, key); err != nil {
		return DurableBudgetGrant{}, err
	} else if existing != nil {
		if existing.ScopeKind != request.ScopeKind || existing.ScopeID != strings.TrimSpace(request.ScopeID) || existing.Dimension != dimension || existing.DeltaUnits != request.DeltaUnits || existing.PreviousVersion != request.ExpectedVersion || existing.Actor != actor || existing.Reason != reason {
			return DurableBudgetGrant{}, fmt.Errorf("%w: budget grant key %s", ErrIdempotencyConflict, key)
		}
		if err := tx.Commit(); err != nil {
			return DurableBudgetGrant{}, err
		}
		return *existing, nil
	}
	account, err := getQuotaAccountForScopeTx(ctx, tx, request.ScopeKind, request.ScopeID, dimension)
	if err != nil {
		return DurableBudgetGrant{}, err
	}
	if account.Version != request.ExpectedVersion {
		return DurableBudgetGrant{}, fmt.Errorf("%w: quota account %s expected version %d, current %d", ErrStaleQuotaGrant, account.ID, request.ExpectedVersion, account.Version)
	}
	if account.LimitUnits > maxStoreInt64-request.DeltaUnits {
		return DurableBudgetGrant{}, fmt.Errorf("%w: quota limit overflow", ErrInvalidQuota)
	}
	now := s.now().UTC()
	grant := DurableBudgetGrant{
		ID: id, AccountID: account.ID, ScopeKind: account.ScopeKind, ScopeID: account.ScopeID,
		Dimension: account.Dimension, DeltaUnits: request.DeltaUnits, PreviousVersion: account.Version,
		Version: account.Version + 1, LimitUnits: account.LimitUnits + request.DeltaUnits,
		Actor: actor, Reason: reason, GrantedAt: now,
	}
	account.LimitUnits = grant.LimitUnits
	account.Version = grant.Version
	account.UpdatedAt = now
	if err := updateQuotaAccountTx(ctx, tx, account, grant.PreviousVersion); err != nil {
		return DurableBudgetGrant{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quota_budget_grants_v5 (
			id, idempotency_key, account_id, scope_kind, scope_id, dimension, delta_units,
			previous_version, version, limit_units, actor, reason, granted_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, grant.ID, key, grant.AccountID, grant.ScopeKind, grant.ScopeID, grant.Dimension, grant.DeltaUnits,
		grant.PreviousVersion, grant.Version, grant.LimitUnits, grant.Actor, grant.Reason, grant.GrantedAt); err != nil {
		if isUniqueConstraint(err) {
			return DurableBudgetGrant{}, fmt.Errorf("%w: budget grant %s", ErrIdentityCollision, grant.ID)
		}
		return DurableBudgetGrant{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:        actor,
		EntityType:   "quota_account",
		EntityID:     account.ID,
		Action:       "quota_account.granted",
		Reason:       reason,
		PayloadJSON:  auditPayload(map[string]any{"grant_id": grant.ID, "delta_units": grant.DeltaUnits, "version": grant.Version, "limit_units": grant.LimitUnits}),
		OperationKey: key,
		CreatedAt:    now,
	}); err != nil {
		return DurableBudgetGrant{}, err
	}
	if err := tx.Commit(); err != nil {
		return DurableBudgetGrant{}, err
	}
	return grant, nil
}

func (s *Store) AdmitTaskActorQuota(ctx context.Context, request AdmitTaskActorQuotaRequest) (DurableAdmissionDecision, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return DurableAdmissionDecision{}, err
	}
	if !isUUIDv7(request.TaskID) {
		return DurableAdmissionDecision{}, ErrInvalidUUIDv7Identity
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return DurableAdmissionDecision{}, err
	}
	key, err := normalizeRequired(request.IdempotencyKey, "admission idempotency key")
	if err != nil {
		return DurableAdmissionDecision{}, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
	}
	actor, err := normalizeRequired(request.Actor, "admission actor")
	if err != nil {
		return DurableAdmissionDecision{}, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
	}
	owner, err := normalizeRequired(request.LeaseOwner, "admission lease owner")
	if err != nil {
		return DurableAdmissionDecision{}, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
	}
	if request.LeaseTTL <= 0 {
		return DurableAdmissionDecision{}, fmt.Errorf("%w: admission lease ttl must be positive", ErrInvalidQuota)
	}
	prepared, err := normalizeAdmissionRequest(request)
	if err != nil {
		return DurableAdmissionDecision{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DurableAdmissionDecision{}, err
	}
	defer tx.Rollback()
	if existing, err := getDurableAdmissionDecisionByKeyTx(ctx, tx, key); err != nil {
		return DurableAdmissionDecision{}, err
	} else if existing != nil {
		if existing.TaskID != request.TaskID || existing.Actor != actor || existing.IdempotencyKey != key || existing.RequestFingerprint != prepared.Fingerprint {
			return DurableAdmissionDecision{}, fmt.Errorf("%w: admission key %s", ErrIdempotencyConflict, key)
		}
		leases, err := listDurableQuotaLeasesForAdmissionTx(ctx, tx, existing.ID)
		if err != nil {
			return DurableAdmissionDecision{}, err
		}
		existing.Leases = leases
		if err := tx.Commit(); err != nil {
			return DurableAdmissionDecision{}, err
		}
		return existing.DurableAdmissionDecision, nil
	}

	now := s.now().UTC()
	accounts := make(map[quotaAdmissionAccountKey]QuotaAccount, len(prepared.BootstrapAccounts)*2)
	for _, bootstrap := range prepared.BootstrapAccounts {
		taskAccount, err := s.ensureQuotaAccountFromPolicyTx(ctx, tx, QuotaScopeTask, request.TaskID, bootstrap.Dimension,
			bootstrap.TaskLimitUnits, prepared.Policy, actor, request.Reason, now)
		if err != nil {
			return DurableAdmissionDecision{}, err
		}
		accounts[quotaAdmissionAccountKey{ScopeKind: QuotaScopeTask, ScopeID: request.TaskID, Dimension: bootstrap.Dimension}] = taskAccount

		actorAccount, err := s.ensureQuotaAccountFromPolicyTx(ctx, tx, QuotaScopeActor, actor, bootstrap.Dimension,
			bootstrap.ActorLimitUnits, prepared.Policy, actor, request.Reason, now)
		if err != nil {
			return DurableAdmissionDecision{}, err
		}
		accounts[quotaAdmissionAccountKey{ScopeKind: QuotaScopeActor, ScopeID: actor, Dimension: bootstrap.Dimension}] = actorAccount
	}
	accepted := true
	for _, claim := range prepared.Claims {
		for _, scope := range []struct {
			kind QuotaScopeKind
			id   string
		}{{QuotaScopeTask, request.TaskID}, {QuotaScopeActor, actor}} {
			account, present := accounts[quotaAdmissionAccountKey{ScopeKind: scope.kind, ScopeID: scope.id, Dimension: claim.Dimension}]
			if !present {
				return DurableAdmissionDecision{}, fmt.Errorf("%w: frozen quota policy omitted bootstrap account %s/%s", ErrInvalidQuota, scope.kind, claim.Dimension)
			}
			if account.AvailableUnits() < claim.Units {
				accepted = false
			}
		}
	}
	decision := DurableAdmissionDecision{ID: id, IdempotencyKey: key, TaskID: request.TaskID, Actor: actor, Accepted: accepted, DecidedAt: now}
	if accepted {
		decision.Reason = AdmissionReasonAccepted
	} else {
		decision.Reason = AdmissionReasonQuotaExhausted
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO admission_decisions_v5 (id, idempotency_key, request_fingerprint, task_id, actor, accepted, reason, decided_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, decision.ID, decision.IdempotencyKey, prepared.Fingerprint, decision.TaskID, decision.Actor, decision.Accepted, decision.Reason, decision.DecidedAt); err != nil {
		if isUniqueConstraint(err) {
			return DurableAdmissionDecision{}, fmt.Errorf("%w: admission decision %s", ErrIdentityCollision, decision.ID)
		}
		return DurableAdmissionDecision{}, err
	}
	if accepted {
		for _, claim := range prepared.Claims {
			for _, scope := range []struct {
				kind QuotaScopeKind
				id   string
			}{{QuotaScopeTask, request.TaskID}, {QuotaScopeActor, actor}} {
				account := accounts[quotaAdmissionAccountKey{ScopeKind: scope.kind, ScopeID: scope.id, Dimension: claim.Dimension}]
				leaseID, err := s.newV2ID("")
				if err != nil {
					return DurableAdmissionDecision{}, err
				}
				lease, err := s.reserveQuotaLeaseTx(ctx, tx, account, preparedQuotaLeaseRequest{
					ID: leaseID, IdempotencyKey: "admission:" + key + ":" + string(scope.kind) + ":" + claim.Dimension,
					Owner: owner, ScopeKind: scope.kind, ScopeID: scope.id, Dimension: claim.Dimension,
					Units: claim.Units, ReclaimPolicy: claim.ReclaimPolicy, TTL: request.LeaseTTL, AdmissionID: decision.ID,
					Actor: actor, Reason: request.Reason,
				}, now)
				if err != nil {
					return DurableAdmissionDecision{}, err
				}
				decision.Leases = append(decision.Leases, lease)
			}
		}
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:        actor,
		EntityType:   "admission_decision",
		EntityID:     decision.ID,
		Action:       "admission.decided",
		Reason:       request.Reason,
		PayloadJSON:  auditPayload(map[string]any{"task_id": decision.TaskID, "accepted": decision.Accepted, "reason": decision.Reason, "lease_count": len(decision.Leases)}),
		OperationKey: key,
		CreatedAt:    now,
	}); err != nil {
		return DurableAdmissionDecision{}, err
	}
	if err := tx.Commit(); err != nil {
		return DurableAdmissionDecision{}, err
	}
	return decision, nil
}

type quotaAdmissionAccountKey struct {
	ScopeKind QuotaScopeKind
	ScopeID   string
	Dimension string
}

type preparedQuotaLeaseRequest struct {
	ID             string
	IdempotencyKey string
	Owner          string
	ScopeKind      QuotaScopeKind
	ScopeID        string
	Dimension      string
	Units          int64
	ReclaimPolicy  QuotaReclaimPolicy
	TTL            time.Duration
	AdmissionID    string
	Actor          string
	Reason         string
}

func prepareQuotaLeaseRequest(s *Store, request QuotaLeaseRequest) (preparedQuotaLeaseRequest, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedQuotaLeaseRequest{}, err
	}
	key, err := normalizeRequired(request.IdempotencyKey, "quota lease idempotency key")
	if err != nil {
		return preparedQuotaLeaseRequest{}, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
	}
	owner, err := normalizeRequired(request.Owner, "quota lease owner")
	if err != nil {
		return preparedQuotaLeaseRequest{}, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
	}
	if err := validateQuotaScope(request.ScopeKind, request.ScopeID); err != nil {
		return preparedQuotaLeaseRequest{}, err
	}
	dimension, err := normalizeQuotaDimension(request.Dimension)
	if err != nil {
		return preparedQuotaLeaseRequest{}, err
	}
	if request.Units <= 0 || request.TTL <= 0 || !validQuotaReclaimPolicy(request.ReclaimPolicy) {
		return preparedQuotaLeaseRequest{}, fmt.Errorf("%w: quota lease needs positive units, ttl, and supported reclaim policy", ErrInvalidQuota)
	}
	if request.AdmissionID != "" && !isUUIDv7(request.AdmissionID) {
		return preparedQuotaLeaseRequest{}, ErrInvalidUUIDv7Identity
	}
	return preparedQuotaLeaseRequest{
		ID: id, IdempotencyKey: key, Owner: owner, ScopeKind: request.ScopeKind,
		ScopeID: strings.TrimSpace(request.ScopeID), Dimension: dimension, Units: request.Units,
		ReclaimPolicy: request.ReclaimPolicy, TTL: request.TTL, AdmissionID: strings.TrimSpace(request.AdmissionID),
		Actor: resolveActor(request.Actor), Reason: strings.TrimSpace(request.Reason),
	}, nil
}

func validateQuotaScope(kind QuotaScopeKind, scopeID string) error {
	if kind != QuotaScopeTask && kind != QuotaScopeActor {
		return fmt.Errorf("%w: unsupported quota scope kind %q", ErrInvalidQuota, kind)
	}
	scopeID = strings.TrimSpace(scopeID)
	if scopeID == "" {
		return fmt.Errorf("%w: quota scope id is required", ErrInvalidQuota)
	}
	if kind == QuotaScopeTask && !isUUIDv7(scopeID) {
		return ErrInvalidUUIDv7Identity
	}
	return nil
}

func normalizeQuotaDimension(value string) (string, error) {
	value, err := normalizeRequired(value, "quota dimension")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidQuota, err)
	}
	return value, nil
}

func validQuotaReclaimPolicy(policy QuotaReclaimPolicy) bool {
	return policy == QuotaReclaimUnused || policy == QuotaReclaimNever
}

func validQuotaSettlementOutcome(outcome QuotaSettlementOutcome) bool {
	return outcome == QuotaSettlementCompleted || outcome == QuotaSettlementCanceled || outcome == QuotaSettlementUncertain
}

func validQuotaSettlementKind(kind QuotaSettlementKind) bool {
	return kind == QuotaSettlementDirect || kind == QuotaSettlementReconcile
}

func durationMilliseconds(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	return int64((value + time.Millisecond - 1) / time.Millisecond)
}

func (s *Store) reserveQuotaLeaseTx(ctx context.Context, tx *sql.Tx, account QuotaAccount, request preparedQuotaLeaseRequest, now time.Time) (DurableQuotaLease, error) {
	if account.AvailableUnits() < request.Units {
		return DurableQuotaLease{}, fmt.Errorf("%w: quota account %s", ErrQuotaExhausted, account.ID)
	}
	var maximumToken int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(fencing_token), 0) FROM quota_leases_v5 WHERE account_id = ?`, account.ID).Scan(&maximumToken); err != nil {
		return DurableQuotaLease{}, err
	}
	if maximumToken < 0 || maximumToken == maxStoreInt64 {
		return DurableQuotaLease{}, fmt.Errorf("%w: quota lease fencing token overflow", ErrInvalidQuota)
	}
	if account.ReservedUnits > maxStoreInt64-request.Units {
		return DurableQuotaLease{}, fmt.Errorf("%w: quota reservation overflow", ErrInvalidQuota)
	}
	account.ReservedUnits += request.Units
	account.UpdatedAt = now
	account.Version++
	if err := updateQuotaAccountTx(ctx, tx, account, account.Version-1); err != nil {
		return DurableQuotaLease{}, err
	}
	lease := DurableQuotaLease{
		ID: request.ID, AccountID: account.ID, AdmissionID: request.AdmissionID, Owner: request.Owner,
		ScopeKind: request.ScopeKind, ScopeID: request.ScopeID, Dimension: request.Dimension,
		ReservedUnits: request.Units, ReclaimPolicy: request.ReclaimPolicy, FencingToken: uint64(maximumToken + 1),
		TTL: request.TTL, ExpiresAt: now.Add(request.TTL), State: DurableQuotaLeaseActive,
		CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quota_leases_v5 (
			id, account_id, admission_id, idempotency_key, owner, scope_kind, scope_id, dimension,
			reserved_units, consumed_units, released_units, reclaim_policy, fencing_token, ttl_ms,
			expires_at, state, created_at, updated_at, settled_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
	`, lease.ID, lease.AccountID, lease.AdmissionID, request.IdempotencyKey, lease.Owner, lease.ScopeKind,
		lease.ScopeID, lease.Dimension, lease.ReservedUnits, lease.ReclaimPolicy, int64(lease.FencingToken),
		durationMilliseconds(lease.TTL), lease.ExpiresAt, lease.State, lease.CreatedAt, lease.UpdatedAt, lease.Version); err != nil {
		if isUniqueConstraint(err) {
			return DurableQuotaLease{}, fmt.Errorf("%w: quota lease %s", ErrIdentityCollision, lease.ID)
		}
		return DurableQuotaLease{}, err
	}
	return lease, nil
}

func getQuotaAccountTx(ctx context.Context, tx *sql.Tx, accountID string) (QuotaAccount, error) {
	account, err := scanQuotaAccount(tx.QueryRowContext(ctx, quotaAccountV5Select+" WHERE id = ?", accountID))
	if err == sql.ErrNoRows {
		return QuotaAccount{}, fmt.Errorf("%w: quota account %s", ErrNotFound, accountID)
	}
	return account, err
}

func getQuotaAccountForScopeTx(ctx context.Context, tx *sql.Tx, kind QuotaScopeKind, scopeID, dimension string) (QuotaAccount, error) {
	account, err := scanQuotaAccount(tx.QueryRowContext(ctx, quotaAccountV5Select+" WHERE scope_kind = ? AND scope_id = ? AND dimension = ?", kind, strings.TrimSpace(scopeID), dimension))
	if err == sql.ErrNoRows {
		return QuotaAccount{}, fmt.Errorf("%w: %s/%s/%s", ErrQuotaNotConfigured, kind, strings.TrimSpace(scopeID), dimension)
	}
	return account, err
}

func findQuotaAccountForScopeTx(ctx context.Context, tx *sql.Tx, kind QuotaScopeKind, scopeID, dimension string) (*QuotaAccount, error) {
	account, err := scanQuotaAccount(tx.QueryRowContext(ctx, quotaAccountV5Select+" WHERE scope_kind = ? AND scope_id = ? AND dimension = ?", kind, strings.TrimSpace(scopeID), dimension))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &account, nil
}

// ensureQuotaAccountFromPolicyTx is the only account-initialization path used
// by task+actor admission. It deliberately never changes an existing numeric
// limit: policy upgrades require the owner to make an explicit CAS grant until
// the account already equals the new frozen bootstrap value.
func (s *Store) ensureQuotaAccountFromPolicyTx(ctx context.Context, tx *sql.Tx, kind QuotaScopeKind, scopeID, dimension string, limitUnits int64, policy QuotaPolicyBinding, actor, reason string, now time.Time) (QuotaAccount, error) {
	if err := validateQuotaScope(kind, scopeID); err != nil {
		return QuotaAccount{}, err
	}
	var err error
	dimension, err = normalizeQuotaDimension(dimension)
	if err != nil {
		return QuotaAccount{}, err
	}
	if limitUnits <= 0 {
		return QuotaAccount{}, fmt.Errorf("%w: frozen quota bootstrap limit must be positive", ErrInvalidQuota)
	}
	policy, err = normalizeQuotaPolicyBinding(policy)
	if err != nil {
		return QuotaAccount{}, err
	}
	scopeID = strings.TrimSpace(scopeID)
	account, err := findQuotaAccountForScopeTx(ctx, tx, kind, scopeID, dimension)
	if err != nil {
		return QuotaAccount{}, err
	}
	if account == nil {
		accountID, err := s.newV2ID("")
		if err != nil {
			return QuotaAccount{}, err
		}
		created := QuotaAccount{
			ID: accountID, ScopeKind: kind, ScopeID: scopeID, Dimension: dimension,
			LimitUnits: limitUnits, Version: 1, CreatedAt: now, UpdatedAt: now,
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO quota_accounts_v5 (
				id, scope_kind, scope_id, dimension, limit_units, consumed_units,
				reserved_units, version, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, 0, 0, ?, ?, ?)
		`, created.ID, created.ScopeKind, created.ScopeID, created.Dimension, created.LimitUnits,
			created.Version, created.CreatedAt, created.UpdatedAt); err != nil {
			if isUniqueConstraint(err) {
				return QuotaAccount{}, fmt.Errorf("%w: quota account %s/%s/%s", ErrIdentityCollision, kind, scopeID, dimension)
			}
			return QuotaAccount{}, err
		}
		if err := insertQuotaAccountPolicyBindingTx(ctx, tx, created.ID, policy, limitUnits, now); err != nil {
			return QuotaAccount{}, err
		}
		if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
			Actor: actor, EntityType: "quota_account", EntityID: created.ID, Action: "quota_account.initialized_from_policy",
			Reason: reason, PayloadJSON: auditPayload(map[string]any{
				"scope_kind": kind, "scope_id": scopeID, "dimension": dimension, "limit_units": limitUnits,
				"policy_id": policy.PolicyID, "policy_version": policy.PolicyVersion, "policy_fingerprint": policy.PolicyFingerprint,
			}), CreatedAt: now,
		}); err != nil {
			return QuotaAccount{}, err
		}
		return created, nil
	}

	binding, err := getQuotaAccountPolicyBindingTx(ctx, tx, account.ID)
	if err != nil {
		return QuotaAccount{}, err
	}
	if binding == nil {
		if account.LimitUnits != limitUnits {
			return QuotaAccount{}, quotaPolicyAccountMismatch(*account, limitUnits, policy)
		}
		if err := insertQuotaAccountPolicyBindingTx(ctx, tx, account.ID, policy, limitUnits, now); err != nil {
			return QuotaAccount{}, err
		}
		if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
			Actor: actor, EntityType: "quota_account", EntityID: account.ID, Action: "quota_account.policy_binding_adopted",
			Reason: reason, PayloadJSON: auditPayload(map[string]any{
				"scope_kind": kind, "scope_id": scopeID, "dimension": dimension, "limit_units": limitUnits,
				"policy_id": policy.PolicyID, "policy_version": policy.PolicyVersion, "policy_fingerprint": policy.PolicyFingerprint,
			}), CreatedAt: now,
		}); err != nil {
			return QuotaAccount{}, err
		}
		return *account, nil
	}
	if binding.InitialLimitUnits != limitUnits && account.LimitUnits != limitUnits {
		return QuotaAccount{}, quotaPolicyAccountMismatch(*account, limitUnits, policy)
	}
	return *account, nil
}

func quotaPolicyAccountMismatch(account QuotaAccount, requestedLimit int64, policy QuotaPolicyBinding) error {
	return fmt.Errorf("%w: %s/%s/%s currently has limit %d; policy %s@%s requires %d", ErrQuotaPolicyAccountMismatch,
		account.ScopeKind, account.ScopeID, account.Dimension, account.LimitUnits, policy.PolicyID, policy.PolicyVersion, requestedLimit)
}

func getQuotaAccountPolicyBindingTx(ctx context.Context, tx *sql.Tx, accountID string) (*QuotaAccountPolicyBinding, error) {
	binding, err := scanQuotaAccountPolicyBinding(tx.QueryRowContext(ctx, quotaAccountPolicyBindingV11Select+" WHERE account_id = ?", accountID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &binding, nil
}

func insertQuotaAccountPolicyBindingTx(ctx context.Context, tx *sql.Tx, accountID string, policy QuotaPolicyBinding, initialLimitUnits int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quota_account_policy_bindings_v11 (
			account_id, policy_id, policy_version, policy_fingerprint, initial_limit_units, bound_at
		) VALUES (?, ?, ?, ?, ?, ?)
	`, accountID, policy.PolicyID, policy.PolicyVersion, policy.PolicyFingerprint, initialLimitUnits, now); err != nil {
		if isUniqueConstraint(err) {
			return fmt.Errorf("%w: quota account policy binding %s", ErrIdentityCollision, accountID)
		}
		return err
	}
	return nil
}

func getDurableQuotaLeaseTx(ctx context.Context, tx *sql.Tx, leaseID string) (DurableQuotaLease, error) {
	lease, err := scanDurableQuotaLease(tx.QueryRowContext(ctx, durableQuotaLeaseV5Select+" WHERE id = ?", leaseID))
	if err == sql.ErrNoRows {
		return DurableQuotaLease{}, fmt.Errorf("%w: quota lease %s", ErrNotFound, leaseID)
	}
	return lease, err
}

func getDurableQuotaLeaseByKeyTx(ctx context.Context, tx *sql.Tx, key string) (*DurableQuotaLease, error) {
	lease, err := scanDurableQuotaLease(tx.QueryRowContext(ctx, durableQuotaLeaseV5Select+" WHERE idempotency_key = ?", key))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &lease, nil
}

func updateQuotaAccountTx(ctx context.Context, tx *sql.Tx, account QuotaAccount, expectedVersion int64) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE quota_accounts_v5
		SET limit_units = ?, consumed_units = ?, reserved_units = ?, version = ?, updated_at = ?
		WHERE id = ? AND version = ?
	`, account.LimitUnits, account.ConsumedUnits, account.ReservedUnits, account.Version, account.UpdatedAt, account.ID, expectedVersion)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: quota account %s", ErrOptimisticLock, account.ID)
	}
	return nil
}

func updateDurableQuotaLeaseTx(ctx context.Context, tx *sql.Tx, lease DurableQuotaLease, expectedVersion int64) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE quota_leases_v5
		SET consumed_units = ?, released_units = ?, expires_at = ?, state = ?, updated_at = ?, settled_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, lease.ConsumedUnits, lease.ReleasedUnits, lease.ExpiresAt, lease.State, lease.UpdatedAt, lease.SettledAt, lease.Version, lease.ID, expectedVersion)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: quota lease %s", ErrOptimisticLock, lease.ID)
	}
	return nil
}

func (s *Store) expireQuotaLeaseTx(ctx context.Context, tx *sql.Tx, lease DurableQuotaLease, actor string, now time.Time, reason string) error {
	if lease.State != DurableQuotaLeaseActive {
		return nil
	}
	lease.State = DurableQuotaLeaseExpired
	lease.UpdatedAt = now
	lease.Version++
	if err := updateDurableQuotaLeaseTx(ctx, tx, lease, lease.Version-1); err != nil {
		return err
	}
	_, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       actor,
		EntityType:  "quota_lease",
		EntityID:    lease.ID,
		Action:      "quota_lease.expired",
		Reason:      reason,
		PayloadJSON: auditPayload(map[string]any{"account_id": lease.AccountID, "fencing_token": lease.FencingToken, "reserved_remaining": lease.RemainingUnits()}),
		CreatedAt:   now,
	})
	return err
}

func scanQuotaAccount(scanner rowScanner) (QuotaAccount, error) {
	var account QuotaAccount
	if err := scanner.Scan(&account.ID, &account.ScopeKind, &account.ScopeID, &account.Dimension,
		&account.LimitUnits, &account.ConsumedUnits, &account.ReservedUnits, &account.Version,
		&account.CreatedAt, &account.UpdatedAt); err != nil {
		return QuotaAccount{}, err
	}
	account.CreatedAt = account.CreatedAt.UTC()
	account.UpdatedAt = account.UpdatedAt.UTC()
	return account, nil
}

func scanQuotaAccountPolicyBinding(scanner rowScanner) (QuotaAccountPolicyBinding, error) {
	var binding QuotaAccountPolicyBinding
	if err := scanner.Scan(&binding.AccountID, &binding.PolicyID, &binding.PolicyVersion, &binding.PolicyFingerprint,
		&binding.InitialLimitUnits, &binding.BoundAt); err != nil {
		return QuotaAccountPolicyBinding{}, err
	}
	if binding.InitialLimitUnits <= 0 || strings.TrimSpace(binding.PolicyID) == "" || strings.TrimSpace(binding.PolicyVersion) == "" || strings.TrimSpace(binding.PolicyFingerprint) == "" {
		return QuotaAccountPolicyBinding{}, fmt.Errorf("%w: invalid persisted quota account policy binding %s", ErrInvalidQuota, binding.AccountID)
	}
	binding.BoundAt = binding.BoundAt.UTC()
	return binding, nil
}

func scanDurableQuotaLease(scanner rowScanner) (DurableQuotaLease, error) {
	var lease DurableQuotaLease
	var admissionID sql.NullString
	var fencing, ttlMilliseconds int64
	var settledAt sql.NullTime
	if err := scanner.Scan(&lease.ID, &lease.AccountID, &admissionID, &lease.Owner, &lease.ScopeKind,
		&lease.ScopeID, &lease.Dimension, &lease.ReservedUnits, &lease.ConsumedUnits, &lease.ReleasedUnits,
		&lease.ReclaimPolicy, &fencing, &ttlMilliseconds, &lease.ExpiresAt, &lease.State, &lease.CreatedAt,
		&lease.UpdatedAt, &settledAt, &lease.Version); err != nil {
		return DurableQuotaLease{}, err
	}
	if fencing <= 0 || ttlMilliseconds <= 0 || lease.ConsumedUnits+lease.ReleasedUnits > lease.ReservedUnits {
		return DurableQuotaLease{}, fmt.Errorf("%w: invalid persisted quota lease %s", ErrInvalidQuota, lease.ID)
	}
	lease.AdmissionID = nullableStringValue(admissionID)
	lease.FencingToken = uint64(fencing)
	lease.TTL = time.Duration(ttlMilliseconds) * time.Millisecond
	lease.ExpiresAt = lease.ExpiresAt.UTC()
	lease.CreatedAt = lease.CreatedAt.UTC()
	lease.UpdatedAt = lease.UpdatedAt.UTC()
	lease.SettledAt = nullableTimePtr(settledAt)
	return lease, nil
}

func sameQuotaLeaseRequest(existing DurableQuotaLease, request preparedQuotaLeaseRequest) bool {
	return existing.AdmissionID == request.AdmissionID && existing.Owner == request.Owner &&
		existing.ScopeKind == request.ScopeKind && existing.ScopeID == request.ScopeID &&
		existing.Dimension == request.Dimension && existing.ReservedUnits == request.Units &&
		existing.ReclaimPolicy == request.ReclaimPolicy && existing.TTL == time.Duration(durationMilliseconds(request.TTL))*time.Millisecond
}

func getQuotaUsageEventTx(ctx context.Context, tx *sql.Tx, leaseID, operationKey string) (*QuotaUsageEvent, error) {
	var event QuotaUsageEvent
	var fencing int64
	err := tx.QueryRowContext(ctx, `
		SELECT id, operation_key, lease_id, fencing_token, units, occurred_at, actor, reason, recorded_at
		FROM quota_usage_events_v5 WHERE lease_id = ? AND operation_key = ?
	`, leaseID, operationKey).Scan(&event.ID, &event.OperationKey, &event.LeaseID, &fencing, &event.Units,
		&event.OccurredAt, &event.Actor, &event.Reason, &event.RecordedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if fencing <= 0 {
		return nil, fmt.Errorf("%w: invalid persisted quota usage event %s", ErrInvalidQuota, event.ID)
	}
	event.FencingToken = uint64(fencing)
	event.OccurredAt = event.OccurredAt.UTC()
	event.RecordedAt = event.RecordedAt.UTC()
	return &event, nil
}

type quotaHeartbeatRecord struct {
	LeaseID      string
	Owner        string
	FencingToken uint64
	TTL          time.Duration
}

func getQuotaHeartbeatByKeyTx(ctx context.Context, tx *sql.Tx, key string) (*quotaHeartbeatRecord, error) {
	var record quotaHeartbeatRecord
	var fencing, ttlMilliseconds int64
	err := tx.QueryRowContext(ctx, `
		SELECT lease_id, owner, fencing_token, ttl_ms FROM quota_heartbeats_v5 WHERE idempotency_key = ?
	`, key).Scan(&record.LeaseID, &record.Owner, &fencing, &ttlMilliseconds)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if fencing <= 0 || ttlMilliseconds <= 0 {
		return nil, fmt.Errorf("%w: invalid persisted quota heartbeat", ErrInvalidQuota)
	}
	record.FencingToken = uint64(fencing)
	record.TTL = time.Duration(ttlMilliseconds) * time.Millisecond
	return &record, nil
}

type durableQuotaSettlementRow struct {
	ID             string
	IdempotencyKey string
	LeaseID        string
	Kind           QuotaSettlementKind
	Outcome        QuotaSettlementOutcome
	ConsumedUnits  int64
	ReleasedUnits  int64
	Owner          string
	FencingToken   uint64
	Actor          string
	Reason         string
	SettledAt      time.Time
}

func getDurableQuotaSettlementByKeyTx(ctx context.Context, tx *sql.Tx, key string) (*durableQuotaSettlementRow, error) {
	var settlement durableQuotaSettlementRow
	var fencing int64
	err := tx.QueryRowContext(ctx, `
		SELECT id, idempotency_key, lease_id, kind, outcome, consumed_units, released_units,
		       owner, fencing_token, actor, reason, settled_at
		FROM quota_settlements_v5 WHERE idempotency_key = ?
	`, key).Scan(&settlement.ID, &settlement.IdempotencyKey, &settlement.LeaseID, &settlement.Kind,
		&settlement.Outcome, &settlement.ConsumedUnits, &settlement.ReleasedUnits, &settlement.Owner,
		&fencing, &settlement.Actor, &settlement.Reason, &settlement.SettledAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if fencing <= 0 {
		return nil, fmt.Errorf("%w: invalid persisted quota settlement %s", ErrInvalidQuota, settlement.ID)
	}
	settlement.FencingToken = uint64(fencing)
	settlement.SettledAt = settlement.SettledAt.UTC()
	return &settlement, nil
}

func getDurableBudgetGrantByKeyTx(ctx context.Context, tx *sql.Tx, key string) (*DurableBudgetGrant, error) {
	var grant DurableBudgetGrant
	err := tx.QueryRowContext(ctx, `
		SELECT id, account_id, scope_kind, scope_id, dimension, delta_units, previous_version,
		       version, limit_units, actor, reason, granted_at
		FROM quota_budget_grants_v5 WHERE idempotency_key = ?
	`, key).Scan(&grant.ID, &grant.AccountID, &grant.ScopeKind, &grant.ScopeID, &grant.Dimension,
		&grant.DeltaUnits, &grant.PreviousVersion, &grant.Version, &grant.LimitUnits, &grant.Actor,
		&grant.Reason, &grant.GrantedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	grant.GrantedAt = grant.GrantedAt.UTC()
	return &grant, nil
}

type admissionDecisionRow struct {
	DurableAdmissionDecision
	RequestFingerprint string
}

func getDurableAdmissionDecisionByKeyTx(ctx context.Context, tx *sql.Tx, key string) (*admissionDecisionRow, error) {
	var row admissionDecisionRow
	err := tx.QueryRowContext(ctx, `
		SELECT id, idempotency_key, request_fingerprint, task_id, actor, accepted, reason, decided_at
		FROM admission_decisions_v5 WHERE idempotency_key = ?
	`, key).Scan(&row.ID, &row.IdempotencyKey, &row.RequestFingerprint, &row.TaskID, &row.Actor,
		&row.Accepted, &row.Reason, &row.DecidedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row.DecidedAt = row.DecidedAt.UTC()
	return &row, nil
}

func listDurableQuotaLeasesForAdmissionTx(ctx context.Context, tx *sql.Tx, admissionID string) ([]DurableQuotaLease, error) {
	rows, err := tx.QueryContext(ctx, durableQuotaLeaseV5Select+" WHERE admission_id = ? ORDER BY scope_kind, dimension, id", admissionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var leases []DurableQuotaLease
	for rows.Next() {
		lease, err := scanDurableQuotaLease(rows)
		if err != nil {
			return nil, err
		}
		leases = append(leases, lease)
	}
	return leases, rows.Err()
}

type normalizedAdmissionRequest struct {
	Policy            QuotaPolicyBinding
	BootstrapAccounts []QuotaAccountBootstrap
	Claims            []TaskActorQuotaClaim
	Fingerprint       string
}

func normalizeAdmissionRequest(request AdmitTaskActorQuotaRequest) (normalizedAdmissionRequest, error) {
	if len(request.Claims) == 0 {
		return normalizedAdmissionRequest{}, fmt.Errorf("%w: admission must contain at least one claim", ErrInvalidQuota)
	}
	policy, err := normalizeQuotaPolicyBinding(request.Policy)
	if err != nil {
		return normalizedAdmissionRequest{}, err
	}
	bootstraps, err := normalizeQuotaAccountBootstraps(request.BootstrapAccounts)
	if err != nil {
		return normalizedAdmissionRequest{}, err
	}
	bootstrapDimensions := make(map[string]struct{}, len(bootstraps))
	for _, bootstrap := range bootstraps {
		bootstrapDimensions[bootstrap.Dimension] = struct{}{}
	}
	claims := append([]TaskActorQuotaClaim(nil), request.Claims...)
	for index := range claims {
		dimension, err := normalizeQuotaDimension(claims[index].Dimension)
		if err != nil {
			return normalizedAdmissionRequest{}, err
		}
		claims[index].Dimension = dimension
		if claims[index].Units <= 0 || !validQuotaReclaimPolicy(claims[index].ReclaimPolicy) {
			return normalizedAdmissionRequest{}, fmt.Errorf("%w: invalid admission claim for %s", ErrInvalidQuota, dimension)
		}
		if _, present := bootstrapDimensions[dimension]; !present {
			return normalizedAdmissionRequest{}, fmt.Errorf("%w: admission claim dimension %s has no frozen bootstrap account", ErrInvalidQuota, dimension)
		}
	}
	sort.Slice(claims, func(left, right int) bool {
		if claims[left].Dimension != claims[right].Dimension {
			return claims[left].Dimension < claims[right].Dimension
		}
		if claims[left].Units != claims[right].Units {
			return claims[left].Units < claims[right].Units
		}
		return claims[left].ReclaimPolicy < claims[right].ReclaimPolicy
	})
	for index := 1; index < len(claims); index++ {
		if claims[index-1].Dimension == claims[index].Dimension {
			return normalizedAdmissionRequest{}, fmt.Errorf("%w: duplicate admission quota dimension %s", ErrInvalidQuota, claims[index].Dimension)
		}
	}
	payload := struct {
		TaskID            string                  `json:"task_id"`
		Actor             string                  `json:"actor"`
		LeaseOwner        string                  `json:"lease_owner"`
		LeaseTTLMS        int64                   `json:"lease_ttl_ms"`
		Policy            QuotaPolicyBinding      `json:"policy"`
		BootstrapAccounts []QuotaAccountBootstrap `json:"bootstrap_accounts"`
		Claims            []TaskActorQuotaClaim   `json:"claims"`
	}{
		TaskID: strings.TrimSpace(request.TaskID), Actor: strings.TrimSpace(request.Actor),
		LeaseOwner: strings.TrimSpace(request.LeaseOwner), LeaseTTLMS: durationMilliseconds(request.LeaseTTL),
		Policy: policy, BootstrapAccounts: bootstraps, Claims: claims,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return normalizedAdmissionRequest{}, err
	}
	digest := sha256.Sum256(encoded)
	return normalizedAdmissionRequest{
		Policy:            policy,
		BootstrapAccounts: bootstraps,
		Claims:            claims,
		Fingerprint:       "sha256:" + hex.EncodeToString(digest[:]),
	}, nil
}

func normalizeQuotaPolicyBinding(binding QuotaPolicyBinding) (QuotaPolicyBinding, error) {
	var err error
	if binding.PolicyID, err = normalizeRequired(binding.PolicyID, "quota policy id"); err != nil {
		return QuotaPolicyBinding{}, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
	}
	if binding.PolicyVersion, err = normalizeRequired(binding.PolicyVersion, "quota policy version"); err != nil {
		return QuotaPolicyBinding{}, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
	}
	if binding.PolicyFingerprint, err = normalizeRequired(binding.PolicyFingerprint, "quota policy fingerprint"); err != nil {
		return QuotaPolicyBinding{}, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
	}
	return binding, nil
}

func normalizeQuotaAccountBootstraps(values []QuotaAccountBootstrap) ([]QuotaAccountBootstrap, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("%w: admission frozen bootstrap accounts are required", ErrInvalidQuota)
	}
	bootstraps := append([]QuotaAccountBootstrap(nil), values...)
	for index := range bootstraps {
		dimension, err := normalizeQuotaDimension(bootstraps[index].Dimension)
		if err != nil {
			return nil, err
		}
		bootstraps[index].Dimension = dimension
		if bootstraps[index].TaskLimitUnits <= 0 || bootstraps[index].ActorLimitUnits <= 0 {
			return nil, fmt.Errorf("%w: frozen bootstrap limits for %s must be positive", ErrInvalidQuota, dimension)
		}
	}
	sort.Slice(bootstraps, func(left, right int) bool {
		return bootstraps[left].Dimension < bootstraps[right].Dimension
	})
	for index := 1; index < len(bootstraps); index++ {
		if bootstraps[index-1].Dimension == bootstraps[index].Dimension {
			return nil, fmt.Errorf("%w: duplicate frozen bootstrap dimension %s", ErrInvalidQuota, bootstraps[index].Dimension)
		}
	}
	return bootstraps, nil
}
