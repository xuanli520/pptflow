package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type preparedQuotaUsageBatchEntry struct {
	id           string
	operationKey string
	leaseID      string
	fencingToken uint64
	units        int64
	occurredAt   time.Time
	actor        string
	reason       string
}

// RecordQuotaUsages atomically charges a related set of quota leases. Stage
// runtime uses it for the task/actor pair of one emitted usage fact, so a Store
// failure cannot leave one scope charged while the other remains reserved.
func (s *Store) RecordQuotaUsages(ctx context.Context, requests []RecordQuotaUsageRequest) ([]QuotaAccount, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return nil, err
	}
	prepared := make([]preparedQuotaUsageBatchEntry, len(requests))
	seenLeases := make(map[string]struct{}, len(requests))
	for index, request := range requests {
		if !isUUIDv7(request.LeaseID) {
			return nil, ErrInvalidUUIDv7Identity
		}
		if _, exists := seenLeases[request.LeaseID]; exists {
			return nil, fmt.Errorf("%w: duplicate quota usage lease %s", ErrInvalidQuota, request.LeaseID)
		}
		seenLeases[request.LeaseID] = struct{}{}
		id, err := s.newV2ID(request.ID)
		if err != nil {
			return nil, err
		}
		operationKey, err := normalizeRequired(request.OperationKey, "quota usage operation key")
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
		}
		if request.FencingToken == 0 || request.Units <= 0 || request.OccurredAt.IsZero() {
			return nil, fmt.Errorf("%w: usage requires fencing token, positive units, and occurrence time", ErrInvalidQuota)
		}
		prepared[index] = preparedQuotaUsageBatchEntry{
			id: id, operationKey: operationKey, leaseID: request.LeaseID, fencingToken: request.FencingToken,
			units: request.Units, occurredAt: request.OccurredAt.UTC(), actor: resolveActor(request.Actor), reason: strings.TrimSpace(request.Reason),
		}
	}
	if len(prepared) == 0 {
		return []QuotaAccount{}, nil
	}

	tx, releaseFence, err := s.beginDispatchFenceTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	defer releaseFence()
	now := s.now().UTC()
	expiredLeaseID := ""
	for _, entry := range prepared {
		lease, err := getDurableQuotaLeaseTx(ctx, tx, entry.leaseID)
		if err != nil {
			return nil, err
		}
		if existing, err := getQuotaUsageEventTx(ctx, tx, lease.ID, entry.operationKey); err != nil {
			return nil, err
		} else if existing != nil {
			if !samePreparedQuotaUsage(*existing, entry) {
				return nil, fmt.Errorf("%w: quota usage operation key %s", ErrIdempotencyConflict, entry.operationKey)
			}
			continue
		}
		if lease.State == DurableQuotaLeaseActive && !lease.ExpiresAt.After(now) {
			if err := s.expireQuotaLeaseTx(ctx, tx, lease, entry.actor, now, "usage observed expired quota lease"); err != nil {
				return nil, err
			}
			expiredLeaseID = lease.ID
			continue
		}
		if lease.State == DurableQuotaLeaseExpired {
			expiredLeaseID = lease.ID
			continue
		}
		if lease.State != DurableQuotaLeaseActive {
			return nil, fmt.Errorf("%w: quota lease %s is %s", ErrInvalidQuota, lease.ID, lease.State)
		}
		if lease.FencingToken != entry.fencingToken {
			return nil, fmt.Errorf("%w: quota lease %s", ErrFencingToken, lease.ID)
		}
	}
	if expiredLeaseID != "" {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: quota lease %s", ErrQuotaLeaseExpired, expiredLeaseID)
	}

	accounts := make([]QuotaAccount, len(prepared))
	for index, entry := range prepared {
		account, err := s.recordPreparedQuotaUsageTx(ctx, tx, entry, now)
		if err != nil {
			return nil, err
		}
		accounts[index] = account
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return accounts, nil
}

func samePreparedQuotaUsage(existing QuotaUsageEvent, entry preparedQuotaUsageBatchEntry) bool {
	return existing.FencingToken == entry.fencingToken && existing.Units == entry.units && existing.OccurredAt.Equal(entry.occurredAt) &&
		existing.Actor == entry.actor && existing.Reason == entry.reason
}

func (s *Store) recordPreparedQuotaUsageTx(ctx context.Context, tx *sql.Tx, entry preparedQuotaUsageBatchEntry, now time.Time) (QuotaAccount, error) {
	lease, err := getDurableQuotaLeaseTx(ctx, tx, entry.leaseID)
	if err != nil {
		return QuotaAccount{}, err
	}
	if existing, err := getQuotaUsageEventTx(ctx, tx, lease.ID, entry.operationKey); err != nil {
		return QuotaAccount{}, err
	} else if existing != nil {
		if !samePreparedQuotaUsage(*existing, entry) {
			return QuotaAccount{}, fmt.Errorf("%w: quota usage operation key %s", ErrIdempotencyConflict, entry.operationKey)
		}
		return getQuotaAccountTx(ctx, tx, lease.AccountID)
	}
	if lease.State != DurableQuotaLeaseActive || lease.FencingToken != entry.fencingToken {
		return QuotaAccount{}, fmt.Errorf("%w: quota lease %s", ErrFencingToken, lease.ID)
	}
	if entry.units > lease.RemainingUnits() {
		return QuotaAccount{}, fmt.Errorf("%w: quota lease %s has %d remaining", ErrQuotaExhausted, lease.ID, lease.RemainingUnits())
	}
	account, err := getQuotaAccountTx(ctx, tx, lease.AccountID)
	if err != nil {
		return QuotaAccount{}, err
	}
	if account.ReservedUnits < entry.units {
		return QuotaAccount{}, fmt.Errorf("%w: quota account %s reservation projection is invalid", ErrInvalidQuota, account.ID)
	}
	account.ReservedUnits -= entry.units
	account.ConsumedUnits += entry.units
	account.UpdatedAt = now
	account.Version++
	if err := updateQuotaAccountTx(ctx, tx, account, account.Version-1); err != nil {
		return QuotaAccount{}, err
	}
	lease.ConsumedUnits += entry.units
	lease.UpdatedAt = now
	lease.Version++
	if err := updateDurableQuotaLeaseTx(ctx, tx, lease, lease.Version-1); err != nil {
		return QuotaAccount{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quota_usage_events_v5 (
			id, operation_key, lease_id, fencing_token, units, occurred_at, actor, reason, recorded_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, entry.id, entry.operationKey, lease.ID, int64(entry.fencingToken), entry.units, entry.occurredAt, entry.actor, entry.reason, now); err != nil {
		if isUniqueConstraint(err) {
			return QuotaAccount{}, fmt.Errorf("%w: quota usage event %s", ErrIdentityCollision, entry.id)
		}
		return QuotaAccount{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: entry.actor, EntityType: "quota_lease", EntityID: lease.ID, Action: "quota_lease.charged", Reason: entry.reason,
		PayloadJSON:  auditPayload(map[string]any{"usage_event_id": entry.id, "units": entry.units, "consumed_units": lease.ConsumedUnits}),
		OperationKey: entry.operationKey, CreatedAt: now,
	}); err != nil {
		return QuotaAccount{}, err
	}
	return account, nil
}

type preparedQuotaHeartbeatBatchEntry struct {
	id             string
	idempotencyKey string
	leaseID        string
	owner          string
	fencingToken   uint64
	ttl            time.Duration
	actor          string
	reason         string
}

// HeartbeatQuotaLeases atomically renews every quota fence owned by one stage.
// A retry uses the same per-lease keys, so an uncertain commit converges to the
// exact committed batch instead of splitting task and actor ownership.
func (s *Store) HeartbeatQuotaLeases(ctx context.Context, requests []HeartbeatQuotaLeaseRequest) ([]DurableQuotaLease, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return nil, err
	}
	prepared := make([]preparedQuotaHeartbeatBatchEntry, len(requests))
	seenLeases := make(map[string]struct{}, len(requests))
	for index, request := range requests {
		if !isUUIDv7(request.LeaseID) {
			return nil, ErrInvalidUUIDv7Identity
		}
		if _, exists := seenLeases[request.LeaseID]; exists {
			return nil, fmt.Errorf("%w: duplicate quota heartbeat lease %s", ErrInvalidQuota, request.LeaseID)
		}
		seenLeases[request.LeaseID] = struct{}{}
		id, err := s.newV2ID(request.ID)
		if err != nil {
			return nil, err
		}
		key, err := normalizeRequired(request.IdempotencyKey, "quota heartbeat idempotency key")
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
		}
		owner, err := normalizeRequired(request.Owner, "quota lease owner")
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
		}
		ttl := time.Duration(durationMilliseconds(request.TTL)) * time.Millisecond
		if request.FencingToken == 0 || ttl <= 0 {
			return nil, fmt.Errorf("%w: quota heartbeat needs fencing token and positive ttl", ErrInvalidQuota)
		}
		prepared[index] = preparedQuotaHeartbeatBatchEntry{
			id: id, idempotencyKey: key, leaseID: request.LeaseID, owner: owner, fencingToken: request.FencingToken,
			ttl: ttl, actor: resolveActor(request.Actor), reason: strings.TrimSpace(request.Reason),
		}
	}
	if len(prepared) == 0 {
		return []DurableQuotaLease{}, nil
	}
	tx, releaseFence, err := s.beginDispatchFenceTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	defer releaseFence()
	now := s.now().UTC()
	leases := make([]DurableQuotaLease, len(prepared))
	replayed := make([]bool, len(prepared))
	expiredLeaseID := ""
	for index, entry := range prepared {
		if record, err := getQuotaHeartbeatByKeyTx(ctx, tx, entry.idempotencyKey); err != nil {
			return nil, err
		} else if record != nil {
			if record.LeaseID != entry.leaseID || record.Owner != entry.owner || record.FencingToken != entry.fencingToken || record.TTL != entry.ttl {
				return nil, fmt.Errorf("%w: quota heartbeat key %s", ErrIdempotencyConflict, entry.idempotencyKey)
			}
			lease, err := getDurableQuotaLeaseTx(ctx, tx, entry.leaseID)
			if err != nil {
				return nil, err
			}
			leases[index], replayed[index] = lease, true
			continue
		}
		lease, err := getDurableQuotaLeaseTx(ctx, tx, entry.leaseID)
		if err != nil {
			return nil, err
		}
		if lease.State == DurableQuotaLeaseActive && !lease.ExpiresAt.After(now) {
			if err := s.expireQuotaLeaseTx(ctx, tx, lease, entry.actor, now, "heartbeat observed expired quota lease"); err != nil {
				return nil, err
			}
			expiredLeaseID = lease.ID
			continue
		}
		if lease.State == DurableQuotaLeaseExpired {
			expiredLeaseID = lease.ID
			continue
		}
		if lease.State != DurableQuotaLeaseActive {
			return nil, fmt.Errorf("%w: quota lease %s is %s", ErrInvalidQuota, lease.ID, lease.State)
		}
		if lease.Owner != entry.owner || lease.FencingToken != entry.fencingToken {
			return nil, fmt.Errorf("%w: quota lease %s", ErrFencingToken, lease.ID)
		}
		leases[index] = lease
	}
	if expiredLeaseID != "" {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: quota lease %s", ErrQuotaLeaseExpired, expiredLeaseID)
	}
	for index, entry := range prepared {
		if replayed[index] {
			continue
		}
		lease := leases[index]
		lease.ExpiresAt = now.Add(entry.ttl)
		lease.UpdatedAt = now
		lease.Version++
		if err := updateDurableQuotaLeaseTx(ctx, tx, lease, lease.Version-1); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO quota_heartbeats_v5 (
				id, idempotency_key, lease_id, owner, fencing_token, ttl_ms, expires_at, actor, reason, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, entry.id, entry.idempotencyKey, lease.ID, entry.owner, int64(entry.fencingToken), durationMilliseconds(entry.ttl),
			lease.ExpiresAt, entry.actor, entry.reason, now); err != nil {
			if isUniqueConstraint(err) {
				return nil, fmt.Errorf("%w: quota heartbeat %s", ErrIdentityCollision, entry.id)
			}
			return nil, err
		}
		if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
			Actor: entry.actor, EntityType: "quota_lease", EntityID: lease.ID, Action: "quota_lease.heartbeated", Reason: entry.reason,
			PayloadJSON:  auditPayload(map[string]any{"expires_at": lease.ExpiresAt, "fencing_token": lease.FencingToken}),
			OperationKey: entry.idempotencyKey, CreatedAt: now,
		}); err != nil {
			return nil, err
		}
		leases[index] = lease
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return leases, nil
}

type preparedQuotaSettlementBatchEntry struct {
	id             string
	idempotencyKey string
	leaseID        string
	owner          string
	fencingToken   uint64
	kind           QuotaSettlementKind
	outcome        QuotaSettlementOutcome
	actor          string
	reason         string
}

// SettleQuotaLeases atomically closes every quota lease admitted for a stage.
// Either the complete stage accounting boundary is durable or none of its
// settlements are visible.
func (s *Store) SettleQuotaLeases(ctx context.Context, requests []SettleQuotaLeaseRequest) ([]DurableQuotaSettlement, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return nil, err
	}
	prepared := make([]preparedQuotaSettlementBatchEntry, len(requests))
	seenLeases := make(map[string]struct{}, len(requests))
	for index, request := range requests {
		if !isUUIDv7(request.LeaseID) {
			return nil, ErrInvalidUUIDv7Identity
		}
		if _, exists := seenLeases[request.LeaseID]; exists {
			return nil, fmt.Errorf("%w: duplicate quota settlement lease %s", ErrInvalidQuota, request.LeaseID)
		}
		seenLeases[request.LeaseID] = struct{}{}
		id, err := s.newV2ID(request.ID)
		if err != nil {
			return nil, err
		}
		key, err := normalizeRequired(request.IdempotencyKey, "quota settlement idempotency key")
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
		}
		owner, err := normalizeRequired(request.Owner, "quota lease owner")
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
		}
		if request.FencingToken == 0 || !validQuotaSettlementOutcome(request.Outcome) {
			return nil, fmt.Errorf("%w: invalid quota settlement", ErrInvalidQuota)
		}
		prepared[index] = preparedQuotaSettlementBatchEntry{
			id: id, idempotencyKey: key, leaseID: request.LeaseID, owner: owner, fencingToken: request.FencingToken,
			kind: QuotaSettlementDirect, outcome: request.Outcome, actor: resolveActor(request.Actor), reason: strings.TrimSpace(request.Reason),
		}
	}
	if len(prepared) == 0 {
		return []DurableQuotaSettlement{}, nil
	}
	return s.applyPreparedQuotaSettlementBatch(ctx, prepared)
}

// ReconcileQuotaLeases atomically resolves a related set of expired or
// uncertain stage reservations after an external outcome becomes known.
func (s *Store) ReconcileQuotaLeases(ctx context.Context, requests []ReconcileQuotaLeaseRequest) ([]DurableQuotaSettlement, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return nil, err
	}
	prepared := make([]preparedQuotaSettlementBatchEntry, len(requests))
	seenLeases := make(map[string]struct{}, len(requests))
	for index, request := range requests {
		if !isUUIDv7(request.LeaseID) {
			return nil, ErrInvalidUUIDv7Identity
		}
		if _, exists := seenLeases[request.LeaseID]; exists {
			return nil, fmt.Errorf("%w: duplicate quota reconciliation lease %s", ErrInvalidQuota, request.LeaseID)
		}
		seenLeases[request.LeaseID] = struct{}{}
		id, err := s.newV2ID(request.ID)
		if err != nil {
			return nil, err
		}
		key, err := normalizeRequired(request.IdempotencyKey, "quota reconciliation idempotency key")
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
		}
		owner, err := normalizeRequired(request.Owner, "quota lease owner")
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidQuota, err)
		}
		if request.FencingToken == 0 || !validQuotaSettlementOutcome(request.Outcome) || request.Outcome == QuotaSettlementUncertain {
			return nil, fmt.Errorf("%w: reconciliation requires a known quota outcome", ErrInvalidQuota)
		}
		prepared[index] = preparedQuotaSettlementBatchEntry{
			id: id, idempotencyKey: key, leaseID: request.LeaseID, owner: owner, fencingToken: request.FencingToken,
			kind: QuotaSettlementReconcile, outcome: request.Outcome, actor: resolveActor(request.Actor), reason: strings.TrimSpace(request.Reason),
		}
	}
	if len(prepared) == 0 {
		return []DurableQuotaSettlement{}, nil
	}
	return s.applyPreparedQuotaSettlementBatch(ctx, prepared)
}

func (s *Store) applyPreparedQuotaSettlementBatch(ctx context.Context, prepared []preparedQuotaSettlementBatchEntry) ([]DurableQuotaSettlement, error) {
	tx, releaseFence, err := s.beginDispatchFenceTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	defer releaseFence()
	now := s.now().UTC()
	results := make([]DurableQuotaSettlement, len(prepared))
	replayed := make([]bool, len(prepared))
	expiredLeaseID := ""
	for index, entry := range prepared {
		if existing, err := getDurableQuotaSettlementByKeyTx(ctx, tx, entry.idempotencyKey); err != nil {
			return nil, err
		} else if existing != nil {
			if existing.LeaseID != entry.leaseID || existing.Kind != entry.kind || existing.Outcome != entry.outcome || existing.Owner != entry.owner || existing.FencingToken != entry.fencingToken {
				return nil, fmt.Errorf("%w: quota settlement key %s", ErrIdempotencyConflict, entry.idempotencyKey)
			}
			lease, err := getDurableQuotaLeaseTx(ctx, tx, existing.LeaseID)
			if err != nil {
				return nil, err
			}
			results[index] = DurableQuotaSettlement{ID: existing.ID, LeaseID: existing.LeaseID, Kind: existing.Kind, Outcome: existing.Outcome, ConsumedUnits: existing.ConsumedUnits, ReleasedUnits: existing.ReleasedUnits, Actor: existing.Actor, SettledAt: existing.SettledAt, Lease: lease}
			replayed[index] = true
			continue
		}
		lease, err := getDurableQuotaLeaseTx(ctx, tx, entry.leaseID)
		if err != nil {
			return nil, err
		}
		if entry.kind == QuotaSettlementDirect && lease.State == DurableQuotaLeaseActive && !lease.ExpiresAt.After(now) {
			if err := s.expireQuotaLeaseTx(ctx, tx, lease, entry.actor, now, "settlement observed expired quota lease"); err != nil {
				return nil, err
			}
			expiredLeaseID = lease.ID
			continue
		}
		if entry.kind == QuotaSettlementDirect && lease.State == DurableQuotaLeaseExpired {
			expiredLeaseID = lease.ID
			continue
		}
		if entry.kind == QuotaSettlementDirect && lease.State != DurableQuotaLeaseActive {
			return nil, fmt.Errorf("%w: quota lease %s is %s", ErrInvalidQuota, lease.ID, lease.State)
		}
		if entry.kind == QuotaSettlementReconcile && lease.State != DurableQuotaLeaseExpired && lease.State != DurableQuotaLeaseUncertain {
			return nil, fmt.Errorf("%w: quota lease %s is not awaiting reconciliation", ErrInvalidQuota, lease.ID)
		}
		if lease.Owner != entry.owner || lease.FencingToken != entry.fencingToken {
			return nil, fmt.Errorf("%w: quota lease %s", ErrFencingToken, lease.ID)
		}
	}
	if expiredLeaseID != "" {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: quota lease %s", ErrQuotaLeaseExpired, expiredLeaseID)
	}
	for index, entry := range prepared {
		if replayed[index] {
			continue
		}
		settlement, err := s.settlePreparedQuotaLeaseTx(ctx, tx, entry, now)
		if err != nil {
			return nil, err
		}
		results[index] = settlement
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}

func (s *Store) settlePreparedQuotaLeaseTx(ctx context.Context, tx *sql.Tx, entry preparedQuotaSettlementBatchEntry, now time.Time) (DurableQuotaSettlement, error) {
	lease, err := getDurableQuotaLeaseTx(ctx, tx, entry.leaseID)
	if err != nil {
		return DurableQuotaSettlement{}, err
	}
	account, err := getQuotaAccountTx(ctx, tx, lease.AccountID)
	if err != nil {
		return DurableQuotaSettlement{}, err
	}
	remaining := lease.RemainingUnits()
	if entry.outcome == QuotaSettlementUncertain {
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
	row := durableQuotaSettlementRow{
		ID: entry.id, IdempotencyKey: entry.idempotencyKey, LeaseID: lease.ID, Kind: entry.kind, Outcome: entry.outcome,
		ConsumedUnits: lease.ConsumedUnits, ReleasedUnits: lease.ReleasedUnits, Owner: entry.owner,
		FencingToken: entry.fencingToken, Actor: entry.actor, Reason: entry.reason, SettledAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quota_settlements_v5 (
			id, idempotency_key, lease_id, kind, outcome, consumed_units, released_units,
			owner, fencing_token, actor, reason, settled_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, row.ID, row.IdempotencyKey, row.LeaseID, row.Kind, row.Outcome, row.ConsumedUnits, row.ReleasedUnits,
		row.Owner, int64(row.FencingToken), row.Actor, row.Reason, row.SettledAt); err != nil {
		if isUniqueConstraint(err) {
			return DurableQuotaSettlement{}, fmt.Errorf("%w: quota settlement %s", ErrIdentityCollision, row.ID)
		}
		return DurableQuotaSettlement{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: entry.actor, EntityType: "quota_lease", EntityID: lease.ID, Action: "quota_lease." + string(entry.kind), Reason: entry.reason,
		PayloadJSON:  auditPayload(map[string]any{"outcome": entry.outcome, "consumed_units": lease.ConsumedUnits, "released_units": lease.ReleasedUnits, "state": lease.State}),
		OperationKey: entry.idempotencyKey, CreatedAt: now,
	}); err != nil {
		return DurableQuotaSettlement{}, err
	}
	return DurableQuotaSettlement{ID: row.ID, LeaseID: row.LeaseID, Kind: row.Kind, Outcome: row.Outcome, ConsumedUnits: row.ConsumedUnits, ReleasedUnits: row.ReleasedUnits, Actor: row.Actor, SettledAt: row.SettledAt, Lease: lease}, nil
}
