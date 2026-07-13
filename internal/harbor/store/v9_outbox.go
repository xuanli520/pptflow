package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const outboxDeliveryOperationV9Select = `
	SELECT id, idempotency_key, kind, request_fingerprint, response_json, created_at
	FROM outbox_delivery_operations_v9`

const (
	outboxDeliveryOperationClaim     = "claim"
	outboxDeliveryOperationHeartbeat = "heartbeat"
	outboxDeliveryOperationAck       = "ack"
	outboxDeliveryOperationNack      = "nack"
)

type outboxDeliveryOperation struct {
	ID                 string
	IdempotencyKey     string
	Kind               string
	RequestFingerprint string
	ResponseJSON       string
	CreatedAt          time.Time
}

type preparedOutboxClaim struct {
	ID             string
	IdempotencyKey string
	Owner          string
	Limit          int
	LeaseTTL       time.Duration
	Actor          string
	Reason         string
	Fingerprint    string
}

type preparedOutboxHeartbeat struct {
	ID                string
	IdempotencyKey    string
	OutboxEventID     string
	Owner             string
	ExpectedVersion   int64
	LeaseFencingToken uint64
	LeaseTTL          time.Duration
	Actor             string
	Reason            string
	Fingerprint       string
}

type preparedOutboxAck struct {
	ID                string
	IdempotencyKey    string
	OutboxEventID     string
	Owner             string
	ExpectedVersion   int64
	LeaseFencingToken uint64
	Actor             string
	Reason            string
	Fingerprint       string
}

type preparedOutboxNack struct {
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
	Fingerprint       string
}

// ClaimOutboxEvents reclaims expired records and leases the next ready batch
// in stable availability/creation/identity order. The persisted claim result
// is returned verbatim on idempotent replay, including the original fence.
func (s *Store) ClaimOutboxEvents(ctx context.Context, request ClaimOutboxEventsRequest) (OutboxDispatchClaim, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return OutboxDispatchClaim{}, err
	}
	prepared, err := prepareOutboxClaim(s, request)
	if err != nil {
		return OutboxDispatchClaim{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OutboxDispatchClaim{}, err
	}
	defer tx.Rollback()
	if replay, found, err := loadOutboxDispatchReplay[OutboxDispatchClaim](ctx, tx, prepared.IdempotencyKey, outboxDeliveryOperationClaim, prepared.Fingerprint); err != nil {
		return OutboxDispatchClaim{}, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return OutboxDispatchClaim{}, err
		}
		return replay, nil
	}

	now := s.now().UTC()
	if err := s.requeueExpiredOutboxEventsTx(ctx, tx, now); err != nil {
		return OutboxDispatchClaim{}, err
	}
	rows, err := tx.QueryContext(ctx, outboxEventSelect+`
		WHERE state = 'pending' AND available_at <= ?
		ORDER BY available_at ASC, created_at ASC, id ASC
		LIMIT ?`, now, prepared.Limit)
	if err != nil {
		return OutboxDispatchClaim{}, err
	}
	var candidates []OutboxEvent
	for rows.Next() {
		event, scanErr := scanOutboxEvent(rows)
		if scanErr != nil {
			_ = rows.Close()
			return OutboxDispatchClaim{}, scanErr
		}
		candidates = append(candidates, event)
	}
	if err := rows.Close(); err != nil {
		return OutboxDispatchClaim{}, err
	}
	if err := rows.Err(); err != nil {
		return OutboxDispatchClaim{}, err
	}

	claim := OutboxDispatchClaim{
		ID:             prepared.ID,
		IdempotencyKey: prepared.IdempotencyKey,
		Owner:          prepared.Owner,
		Limit:          prepared.Limit,
		LeaseTTL:       prepared.LeaseTTL,
		Events:         make([]OutboxEvent, 0, len(candidates)),
		ClaimedAt:      now,
	}
	for _, candidate := range candidates {
		if candidate.LeaseFencingToken >= uint64(maxStoreInt64) {
			return OutboxDispatchClaim{}, fmt.Errorf("%w: outbox event %s fencing token overflow", ErrInvalidDispatch, candidate.ID)
		}
		leaseExpiresAt := now.Add(prepared.LeaseTTL)
		candidate.State = OutboxLeased
		candidate.LeaseOwner = prepared.Owner
		candidate.LeaseExpiresAt = &leaseExpiresAt
		candidate.LeaseFencingToken++
		candidate.DeliveryCount++
		candidate.UpdatedAt = now
		candidate.Version++
		result, updateErr := tx.ExecContext(ctx, `
			UPDATE outbox_events
			SET state = ?, lease_owner = ?, lease_expires_at = ?, lease_fencing_token = ?,
				delivery_count = ?, updated_at = ?, version = ?
			WHERE id = ? AND state = 'pending' AND available_at <= ? AND version = ?
		`, candidate.State, candidate.LeaseOwner, candidate.LeaseExpiresAt,
			int64(candidate.LeaseFencingToken), candidate.DeliveryCount, candidate.UpdatedAt,
			candidate.Version, candidate.ID, now, candidate.Version-1)
		if updateErr != nil {
			return OutboxDispatchClaim{}, updateErr
		}
		changed, updateErr := result.RowsAffected()
		if updateErr != nil {
			return OutboxDispatchClaim{}, updateErr
		}
		if changed != 1 {
			return OutboxDispatchClaim{}, fmt.Errorf("%w: outbox event %s", ErrOptimisticLock, candidate.ID)
		}
		if _, auditErr := s.appendAuditTx(ctx, tx, AuditEvent{
			Actor:        prepared.Actor,
			EntityType:   "outbox_event",
			EntityID:     candidate.ID,
			Action:       "outbox_event.claimed",
			Reason:       prepared.Reason,
			PayloadJSON:  auditPayload(map[string]any{"owner": candidate.LeaseOwner, "fencing_token": candidate.LeaseFencingToken, "delivery_count": candidate.DeliveryCount, "expires_at": candidate.LeaseExpiresAt, "version": candidate.Version}),
			OperationKey: prepared.IdempotencyKey,
			CreatedAt:    now,
		}); auditErr != nil {
			return OutboxDispatchClaim{}, auditErr
		}
		claim.Events = append(claim.Events, candidate)
	}
	if err := insertOutboxDispatchOperationTx(ctx, tx, outboxDeliveryOperation{
		ID: prepared.ID, IdempotencyKey: prepared.IdempotencyKey, Kind: outboxDeliveryOperationClaim,
		RequestFingerprint: prepared.Fingerprint, ResponseJSON: encodeOutboxDispatchResponse(claim), CreatedAt: now,
	}); err != nil {
		return OutboxDispatchClaim{}, err
	}
	if err := tx.Commit(); err != nil {
		return OutboxDispatchClaim{}, err
	}
	return claim, nil
}

// HeartbeatOutboxEvent extends only the exact record lease carried by a
// worker. A matching owner alone is insufficient: version and fence both
// must match the currently committed lease.
func (s *Store) HeartbeatOutboxEvent(ctx context.Context, request HeartbeatOutboxEventRequest) (OutboxEvent, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return OutboxEvent{}, err
	}
	prepared, err := prepareOutboxHeartbeat(s, request)
	if err != nil {
		return OutboxEvent{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OutboxEvent{}, err
	}
	defer tx.Rollback()
	if replay, found, err := loadOutboxDispatchReplay[OutboxEvent](ctx, tx, prepared.IdempotencyKey, outboxDeliveryOperationHeartbeat, prepared.Fingerprint); err != nil {
		return OutboxEvent{}, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return OutboxEvent{}, err
		}
		return replay, nil
	}
	now := s.now().UTC()
	if err := s.requeueExpiredOutboxEventsTx(ctx, tx, now); err != nil {
		return OutboxEvent{}, err
	}
	event, err := getOutboxEventTx(ctx, tx, prepared.OutboxEventID)
	if err != nil {
		return OutboxEvent{}, err
	}
	if err := verifyOutboxLease(event, prepared.Owner, prepared.ExpectedVersion, prepared.LeaseFencingToken, now); err != nil {
		return OutboxEvent{}, err
	}
	leaseExpiresAt := now.Add(prepared.LeaseTTL)
	event.LeaseExpiresAt = &leaseExpiresAt
	event.UpdatedAt = now
	event.Version++
	if err := updateOutboxLeaseTx(ctx, tx, event, prepared.ExpectedVersion, prepared.LeaseFencingToken); err != nil {
		return OutboxEvent{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: prepared.Actor, EntityType: "outbox_event", EntityID: event.ID, Action: "outbox_event.heartbeated",
		Reason: prepared.Reason, PayloadJSON: auditPayload(map[string]any{"owner": event.LeaseOwner, "fencing_token": event.LeaseFencingToken, "expires_at": event.LeaseExpiresAt, "version": event.Version}),
		OperationKey: prepared.IdempotencyKey, CreatedAt: now,
	}); err != nil {
		return OutboxEvent{}, err
	}
	if err := insertOutboxDispatchOperationTx(ctx, tx, outboxDeliveryOperation{
		ID: prepared.ID, IdempotencyKey: prepared.IdempotencyKey, Kind: outboxDeliveryOperationHeartbeat,
		RequestFingerprint: prepared.Fingerprint, ResponseJSON: encodeOutboxDispatchResponse(event), CreatedAt: now,
	}); err != nil {
		return OutboxEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return OutboxEvent{}, err
	}
	return event, nil
}

// AckOutboxEvent marks a successfully handled event published. The caller
// must prove ownership of the live claim with both version and fence.
func (s *Store) AckOutboxEvent(ctx context.Context, request AckOutboxEventRequest) (OutboxEvent, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return OutboxEvent{}, err
	}
	prepared, err := prepareOutboxAck(s, request)
	if err != nil {
		return OutboxEvent{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OutboxEvent{}, err
	}
	defer tx.Rollback()
	if replay, found, err := loadOutboxDispatchReplay[OutboxEvent](ctx, tx, prepared.IdempotencyKey, outboxDeliveryOperationAck, prepared.Fingerprint); err != nil {
		return OutboxEvent{}, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return OutboxEvent{}, err
		}
		return replay, nil
	}
	now := s.now().UTC()
	if err := s.requeueExpiredOutboxEventsTx(ctx, tx, now); err != nil {
		return OutboxEvent{}, err
	}
	event, err := getOutboxEventTx(ctx, tx, prepared.OutboxEventID)
	if err != nil {
		return OutboxEvent{}, err
	}
	if err := verifyOutboxLease(event, prepared.Owner, prepared.ExpectedVersion, prepared.LeaseFencingToken, now); err != nil {
		return OutboxEvent{}, err
	}
	event.State = OutboxPublished
	event.LeaseOwner = ""
	event.LeaseExpiresAt = nil
	event.PublishedAt = &now
	event.UpdatedAt = now
	event.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE outbox_events
		SET state = ?, lease_owner = '', lease_expires_at = NULL, published_at = ?, updated_at = ?, version = ?
		WHERE id = ? AND state = 'leased' AND lease_owner = ? AND version = ? AND lease_fencing_token = ?
	`, event.State, event.PublishedAt, event.UpdatedAt, event.Version, event.ID, prepared.Owner,
		prepared.ExpectedVersion, int64(prepared.LeaseFencingToken))
	if err != nil {
		return OutboxEvent{}, err
	}
	if err := requireOneOutboxRow(result, event.ID); err != nil {
		return OutboxEvent{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: prepared.Actor, EntityType: "outbox_event", EntityID: event.ID, Action: "outbox_event.acknowledged",
		Reason: prepared.Reason, PayloadJSON: auditPayload(map[string]any{"fencing_token": event.LeaseFencingToken, "delivery_count": event.DeliveryCount, "version": event.Version}),
		OperationKey: prepared.IdempotencyKey, CreatedAt: now,
	}); err != nil {
		return OutboxEvent{}, err
	}
	if err := insertOutboxDispatchOperationTx(ctx, tx, outboxDeliveryOperation{
		ID: prepared.ID, IdempotencyKey: prepared.IdempotencyKey, Kind: outboxDeliveryOperationAck,
		RequestFingerprint: prepared.Fingerprint, ResponseJSON: encodeOutboxDispatchResponse(event), CreatedAt: now,
	}); err != nil {
		return OutboxEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return OutboxEvent{}, err
	}
	return event, nil
}

// NackOutboxEvent records a failed fenced delivery and makes it claimable only
// after RetryDelay. It never changes the immutable topic, subject, payload,
// or enqueue idempotency identity.
func (s *Store) NackOutboxEvent(ctx context.Context, request NackOutboxEventRequest) (OutboxEvent, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return OutboxEvent{}, err
	}
	prepared, err := prepareOutboxNack(s, request)
	if err != nil {
		return OutboxEvent{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OutboxEvent{}, err
	}
	defer tx.Rollback()
	if replay, found, err := loadOutboxDispatchReplay[OutboxEvent](ctx, tx, prepared.IdempotencyKey, outboxDeliveryOperationNack, prepared.Fingerprint); err != nil {
		return OutboxEvent{}, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return OutboxEvent{}, err
		}
		return replay, nil
	}
	now := s.now().UTC()
	if err := s.requeueExpiredOutboxEventsTx(ctx, tx, now); err != nil {
		return OutboxEvent{}, err
	}
	event, err := getOutboxEventTx(ctx, tx, prepared.OutboxEventID)
	if err != nil {
		return OutboxEvent{}, err
	}
	if err := verifyOutboxLease(event, prepared.Owner, prepared.ExpectedVersion, prepared.LeaseFencingToken, now); err != nil {
		return OutboxEvent{}, err
	}
	event.State = OutboxPending
	event.LeaseOwner = ""
	event.LeaseExpiresAt = nil
	event.AvailableAt = now.Add(prepared.RetryDelay)
	event.LastError = prepared.ErrorCode
	event.UpdatedAt = now
	event.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE outbox_events
		SET state = ?, lease_owner = '', lease_expires_at = NULL, available_at = ?,
			last_error = ?, updated_at = ?, version = ?
		WHERE id = ? AND state = 'leased' AND lease_owner = ? AND version = ? AND lease_fencing_token = ?
	`, event.State, event.AvailableAt, event.LastError, event.UpdatedAt, event.Version,
		event.ID, prepared.Owner, prepared.ExpectedVersion, int64(prepared.LeaseFencingToken))
	if err != nil {
		return OutboxEvent{}, err
	}
	if err := requireOneOutboxRow(result, event.ID); err != nil {
		return OutboxEvent{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: prepared.Actor, EntityType: "outbox_event", EntityID: event.ID, Action: "outbox_event.nacked",
		Reason: prepared.Reason, PayloadJSON: auditPayload(map[string]any{"fencing_token": event.LeaseFencingToken, "retry_at": event.AvailableAt, "error_code": event.LastError, "version": event.Version}),
		OperationKey: prepared.IdempotencyKey, CreatedAt: now,
	}); err != nil {
		return OutboxEvent{}, err
	}
	if err := insertOutboxDispatchOperationTx(ctx, tx, outboxDeliveryOperation{
		ID: prepared.ID, IdempotencyKey: prepared.IdempotencyKey, Kind: outboxDeliveryOperationNack,
		RequestFingerprint: prepared.Fingerprint, ResponseJSON: encodeOutboxDispatchResponse(event), CreatedAt: now,
	}); err != nil {
		return OutboxEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return OutboxEvent{}, err
	}
	return event, nil
}

func prepareOutboxClaim(s *Store, request ClaimOutboxEventsRequest) (preparedOutboxClaim, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedOutboxClaim{}, err
	}
	key, err := normalizeRequired(request.IdempotencyKey, "outbox claim idempotency key")
	if err != nil {
		return preparedOutboxClaim{}, fmt.Errorf("%w: %v", ErrInvalidDispatch, err)
	}
	owner, err := normalizeRequired(request.Owner, "outbox claim owner")
	if err != nil {
		return preparedOutboxClaim{}, fmt.Errorf("%w: %v", ErrInvalidDispatch, err)
	}
	if request.Limit <= 0 || request.LeaseTTL <= 0 {
		return preparedOutboxClaim{}, fmt.Errorf("%w: outbox claim limit and lease TTL must be positive", ErrInvalidDispatch)
	}
	ttl := normalizedOutboxDuration(request.LeaseTTL)
	prepared := preparedOutboxClaim{ID: id, IdempotencyKey: key, Owner: owner, Limit: request.Limit, LeaseTTL: ttl, Actor: resolveActor(request.Actor), Reason: strings.TrimSpace(request.Reason)}
	prepared.Fingerprint = outboxDispatchFingerprint(struct {
		Owner      string `json:"owner"`
		Limit      int    `json:"limit"`
		LeaseTTLMS int64  `json:"lease_ttl_ms"`
	}{Owner: prepared.Owner, Limit: prepared.Limit, LeaseTTLMS: durationMilliseconds(prepared.LeaseTTL)})
	return prepared, nil
}

func prepareOutboxHeartbeat(s *Store, request HeartbeatOutboxEventRequest) (preparedOutboxHeartbeat, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedOutboxHeartbeat{}, err
	}
	key, eventID, owner, err := prepareOutboxOperationIdentity(request.IdempotencyKey, request.OutboxEventID, request.Owner)
	if err != nil {
		return preparedOutboxHeartbeat{}, err
	}
	if request.ExpectedVersion <= 0 || request.LeaseFencingToken == 0 || request.LeaseTTL <= 0 {
		return preparedOutboxHeartbeat{}, fmt.Errorf("%w: outbox heartbeat requires a positive version, fence, and lease TTL", ErrInvalidDispatch)
	}
	prepared := preparedOutboxHeartbeat{ID: id, IdempotencyKey: key, OutboxEventID: eventID, Owner: owner, ExpectedVersion: request.ExpectedVersion, LeaseFencingToken: request.LeaseFencingToken, LeaseTTL: normalizedOutboxDuration(request.LeaseTTL), Actor: resolveActor(request.Actor), Reason: strings.TrimSpace(request.Reason)}
	prepared.Fingerprint = outboxDispatchFingerprint(struct {
		OutboxEventID string `json:"outbox_event_id"`
		Owner         string `json:"owner"`
		Version       int64  `json:"version"`
		Fence         uint64 `json:"fence"`
		LeaseTTLMS    int64  `json:"lease_ttl_ms"`
	}{OutboxEventID: prepared.OutboxEventID, Owner: prepared.Owner, Version: prepared.ExpectedVersion, Fence: prepared.LeaseFencingToken, LeaseTTLMS: durationMilliseconds(prepared.LeaseTTL)})
	return prepared, nil
}

func prepareOutboxAck(s *Store, request AckOutboxEventRequest) (preparedOutboxAck, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedOutboxAck{}, err
	}
	key, eventID, owner, err := prepareOutboxOperationIdentity(request.IdempotencyKey, request.OutboxEventID, request.Owner)
	if err != nil {
		return preparedOutboxAck{}, err
	}
	if request.ExpectedVersion <= 0 || request.LeaseFencingToken == 0 {
		return preparedOutboxAck{}, fmt.Errorf("%w: outbox acknowledgement requires a positive version and fence", ErrInvalidDispatch)
	}
	prepared := preparedOutboxAck{ID: id, IdempotencyKey: key, OutboxEventID: eventID, Owner: owner, ExpectedVersion: request.ExpectedVersion, LeaseFencingToken: request.LeaseFencingToken, Actor: resolveActor(request.Actor), Reason: strings.TrimSpace(request.Reason)}
	prepared.Fingerprint = outboxDispatchFingerprint(struct {
		OutboxEventID string `json:"outbox_event_id"`
		Owner         string `json:"owner"`
		Version       int64  `json:"version"`
		Fence         uint64 `json:"fence"`
	}{OutboxEventID: prepared.OutboxEventID, Owner: prepared.Owner, Version: prepared.ExpectedVersion, Fence: prepared.LeaseFencingToken})
	return prepared, nil
}

func prepareOutboxNack(s *Store, request NackOutboxEventRequest) (preparedOutboxNack, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedOutboxNack{}, err
	}
	key, eventID, owner, err := prepareOutboxOperationIdentity(request.IdempotencyKey, request.OutboxEventID, request.Owner)
	if err != nil {
		return preparedOutboxNack{}, err
	}
	errorCode, err := normalizeRequired(request.ErrorCode, "outbox nack error code")
	if err != nil {
		return preparedOutboxNack{}, fmt.Errorf("%w: %v", ErrInvalidDispatch, err)
	}
	if request.ExpectedVersion <= 0 || request.LeaseFencingToken == 0 || request.RetryDelay < 0 {
		return preparedOutboxNack{}, fmt.Errorf("%w: outbox nack requires a positive version and fence with a non-negative retry delay", ErrInvalidDispatch)
	}
	prepared := preparedOutboxNack{ID: id, IdempotencyKey: key, OutboxEventID: eventID, Owner: owner, ExpectedVersion: request.ExpectedVersion, LeaseFencingToken: request.LeaseFencingToken, RetryDelay: normalizedOutboxRetryDelay(request.RetryDelay), ErrorCode: errorCode, Actor: resolveActor(request.Actor), Reason: strings.TrimSpace(request.Reason)}
	prepared.Fingerprint = outboxDispatchFingerprint(struct {
		OutboxEventID string `json:"outbox_event_id"`
		Owner         string `json:"owner"`
		Version       int64  `json:"version"`
		Fence         uint64 `json:"fence"`
		RetryDelayMS  int64  `json:"retry_delay_ms"`
		ErrorCode     string `json:"error_code"`
	}{OutboxEventID: prepared.OutboxEventID, Owner: prepared.Owner, Version: prepared.ExpectedVersion, Fence: prepared.LeaseFencingToken, RetryDelayMS: durationMilliseconds(prepared.RetryDelay), ErrorCode: prepared.ErrorCode})
	return prepared, nil
}

func prepareOutboxOperationIdentity(key, eventID, owner string) (string, string, string, error) {
	key, err := normalizeRequired(key, "outbox operation idempotency key")
	if err != nil {
		return "", "", "", fmt.Errorf("%w: %v", ErrInvalidDispatch, err)
	}
	if !isUUIDv7(eventID) {
		return "", "", "", ErrInvalidUUIDv7Identity
	}
	owner, err = normalizeRequired(owner, "outbox operation owner")
	if err != nil {
		return "", "", "", fmt.Errorf("%w: %v", ErrInvalidDispatch, err)
	}
	return key, eventID, owner, nil
}

func normalizedOutboxDuration(value time.Duration) time.Duration {
	return time.Duration(durationMilliseconds(value)) * time.Millisecond
}

func normalizedOutboxRetryDelay(value time.Duration) time.Duration {
	if value == 0 {
		return 0
	}
	return normalizedOutboxDuration(value)
}

func outboxDispatchFingerprint(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func encodeOutboxDispatchResponse(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `{"serialization_error":true}`
	}
	return string(encoded)
}

func loadOutboxDispatchReplay[T any](ctx context.Context, tx *sql.Tx, key, kind, fingerprint string) (T, bool, error) {
	var zero T
	operation, err := scanOutboxDeliveryOperation(tx.QueryRowContext(ctx, outboxDeliveryOperationV9Select+" WHERE idempotency_key = ?", key))
	if err == sql.ErrNoRows {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, err
	}
	if operation.Kind != kind || operation.RequestFingerprint != fingerprint {
		return zero, false, fmt.Errorf("%w: outbox %s operation key %s", ErrIdempotencyConflict, kind, key)
	}
	var response T
	if err := json.Unmarshal([]byte(operation.ResponseJSON), &response); err != nil {
		return zero, false, fmt.Errorf("%w: decode persisted outbox %s operation %s: %v", ErrInvalidDispatch, kind, operation.ID, err)
	}
	return response, true, nil
}

func insertOutboxDispatchOperationTx(ctx context.Context, tx *sql.Tx, operation outboxDeliveryOperation) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO outbox_delivery_operations_v9 (id, idempotency_key, kind, request_fingerprint, response_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, operation.ID, operation.IdempotencyKey, operation.Kind, operation.RequestFingerprint, operation.ResponseJSON, operation.CreatedAt)
	if err != nil {
		if isGlobalIdentityCollision(err) {
			return fmt.Errorf("%w: outbox delivery operation %s", ErrIdentityCollision, operation.ID)
		}
		if isUniqueConstraint(err) {
			return fmt.Errorf("%w: outbox delivery operation key %s", ErrIdempotencyConflict, operation.IdempotencyKey)
		}
		return err
	}
	return nil
}

func scanOutboxDeliveryOperation(scanner rowScanner) (outboxDeliveryOperation, error) {
	var operation outboxDeliveryOperation
	if err := scanner.Scan(&operation.ID, &operation.IdempotencyKey, &operation.Kind, &operation.RequestFingerprint, &operation.ResponseJSON, &operation.CreatedAt); err != nil {
		return outboxDeliveryOperation{}, err
	}
	operation.CreatedAt = operation.CreatedAt.UTC()
	return operation, nil
}

func (s *Store) requeueExpiredOutboxEventsTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE outbox_events
		SET state = 'pending', lease_owner = '', lease_expires_at = NULL, available_at = ?,
			last_error = 'lease_expired', updated_at = ?, version = version + 1
		WHERE state = 'leased' AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?
	`, now, now, now)
	return err
}

func verifyOutboxLease(event OutboxEvent, owner string, expectedVersion int64, fence uint64, now time.Time) error {
	if event.State != OutboxLeased || event.LeaseOwner != owner || event.Version != expectedVersion || event.LeaseFencingToken != fence {
		return fmt.Errorf("%w: outbox event %s", ErrFencingToken, event.ID)
	}
	if event.LeaseExpiresAt == nil || !event.LeaseExpiresAt.After(now) {
		return fmt.Errorf("%w: outbox event %s lease expired", ErrFencingToken, event.ID)
	}
	return nil
}

func updateOutboxLeaseTx(ctx context.Context, tx *sql.Tx, event OutboxEvent, expectedVersion int64, fence uint64) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE outbox_events
		SET lease_expires_at = ?, updated_at = ?, version = ?
		WHERE id = ? AND state = 'leased' AND lease_owner = ? AND version = ? AND lease_fencing_token = ?
	`, event.LeaseExpiresAt, event.UpdatedAt, event.Version, event.ID, event.LeaseOwner,
		expectedVersion, int64(fence))
	if err != nil {
		return err
	}
	return requireOneOutboxRow(result, event.ID)
}

func requireOneOutboxRow(result sql.Result, eventID string) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: outbox event %s", ErrOptimisticLock, eventID)
	}
	return nil
}
