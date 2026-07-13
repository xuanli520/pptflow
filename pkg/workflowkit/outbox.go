package workflowkit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var ErrInvalidOutbox = errors.New("workflowkit: invalid outbox")

// OutboxStatus is the generic delivery lifecycle of one durable message.
type OutboxStatus string

const (
	OutboxPending   OutboxStatus = "pending"
	OutboxLeased    OutboxStatus = "leased"
	OutboxDelivered OutboxStatus = "delivered"
)

func (status OutboxStatus) valid() bool {
	switch status {
	case OutboxPending, OutboxLeased, OutboxDelivered:
		return true
	default:
		return false
	}
}

// OutboxEnqueueRequest creates a generic message. Payload remains opaque to
// workflowkit; topic and subject identity are stable routing metadata.
type OutboxEnqueueRequest struct {
	MessageID      string    `json:"message_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	Topic          string    `json:"topic"`
	SubjectID      string    `json:"subject_id"`
	Payload        []byte    `json:"payload"`
	AvailableAt    time.Time `json:"available_at"`
}

// Clone returns an independent enqueue request.
func (request OutboxEnqueueRequest) Clone() OutboxEnqueueRequest {
	request.Payload = append([]byte(nil), request.Payload...)
	return request
}

func (request OutboxEnqueueRequest) validate() error {
	if err := validateRequired("outbox message id", request.MessageID, ErrInvalidOutbox); err != nil {
		return err
	}
	if err := validateRequired("outbox idempotency key", request.IdempotencyKey, ErrInvalidOutbox); err != nil {
		return err
	}
	if err := validateRequired("outbox topic", request.Topic, ErrInvalidOutbox); err != nil {
		return err
	}
	if err := validateRequired("outbox subject id", request.SubjectID, ErrInvalidOutbox); err != nil {
		return err
	}
	if request.AvailableAt.IsZero() {
		return fmt.Errorf("%w: outbox availability time is required", ErrInvalidOutbox)
	}
	return nil
}

// OutboxMessage is a copied, immutable projection. Version is a fencing value
// for heartbeat, acknowledgement, and negative acknowledgement operations.
type OutboxMessage struct {
	MessageID      string       `json:"message_id"`
	IdempotencyKey string       `json:"idempotency_key"`
	Topic          string       `json:"topic"`
	SubjectID      string       `json:"subject_id"`
	Payload        []byte       `json:"payload"`
	AvailableAt    time.Time    `json:"available_at"`
	Status         OutboxStatus `json:"status"`
	LeaseOwner     string       `json:"lease_owner,omitempty"`
	LeaseExpiresAt time.Time    `json:"lease_expires_at,omitempty"`
	DeliveryCount  int          `json:"delivery_count"`
	LastError      string       `json:"last_error,omitempty"`
	Version        int64        `json:"version"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

// Clone returns an independent message snapshot.
func (message OutboxMessage) Clone() OutboxMessage {
	message.Payload = append([]byte(nil), message.Payload...)
	return message
}

// OutboxClaimRequest leases a bounded number of ready messages for one worker.
// ClaimID makes repeated poll acknowledgements idempotent.
type OutboxClaimRequest struct {
	ClaimID string        `json:"claim_id"`
	Owner   string        `json:"owner"`
	Limit   int           `json:"limit"`
	TTL     time.Duration `json:"ttl"`
}

func (request OutboxClaimRequest) validate() error {
	if err := validateRequired("outbox claim id", request.ClaimID, ErrInvalidOutbox); err != nil {
		return err
	}
	if err := validateRequired("outbox claim owner", request.Owner, ErrInvalidOutbox); err != nil {
		return err
	}
	if request.Limit < 1 || request.TTL <= 0 {
		return fmt.Errorf("%w: outbox claim limit and ttl must be positive", ErrInvalidOutbox)
	}
	return nil
}

// OutboxHeartbeatRequest extends one active lease. HeartbeatID makes a lost
// response retry-safe, while ExpectedVersion prevents a stale worker from
// extending a lease that has already been reclaimed.
type OutboxHeartbeatRequest struct {
	HeartbeatID     string        `json:"heartbeat_id"`
	MessageID       string        `json:"message_id"`
	Owner           string        `json:"owner"`
	ExpectedVersion int64         `json:"expected_version"`
	TTL             time.Duration `json:"ttl"`
}

func (request OutboxHeartbeatRequest) validate() error {
	if err := validateRequired("outbox heartbeat id", request.HeartbeatID, ErrInvalidOutbox); err != nil {
		return err
	}
	if err := validateRequired("outbox heartbeat message id", request.MessageID, ErrInvalidOutbox); err != nil {
		return err
	}
	if err := validateRequired("outbox heartbeat owner", request.Owner, ErrInvalidOutbox); err != nil {
		return err
	}
	if request.ExpectedVersion <= 0 || request.TTL <= 0 {
		return fmt.Errorf("%w: outbox heartbeat expected version and ttl must be positive", ErrInvalidOutbox)
	}
	return nil
}

// OutboxAckRequest confirms exactly one leased delivery.
type OutboxAckRequest struct {
	AckID           string `json:"ack_id"`
	MessageID       string `json:"message_id"`
	Owner           string `json:"owner"`
	ExpectedVersion int64  `json:"expected_version"`
}

func (request OutboxAckRequest) validate() error {
	if err := validateRequired("outbox ack id", request.AckID, ErrInvalidOutbox); err != nil {
		return err
	}
	if err := validateRequired("outbox ack message id", request.MessageID, ErrInvalidOutbox); err != nil {
		return err
	}
	if err := validateRequired("outbox ack owner", request.Owner, ErrInvalidOutbox); err != nil {
		return err
	}
	if request.ExpectedVersion <= 0 {
		return fmt.Errorf("%w: outbox ack expected version must be positive", ErrInvalidOutbox)
	}
	return nil
}

// OutboxNackRequest returns one leased message to pending state at a bounded
// future time. The message's payload and idempotency identity never change.
type OutboxNackRequest struct {
	NackID          string        `json:"nack_id"`
	MessageID       string        `json:"message_id"`
	Owner           string        `json:"owner"`
	ExpectedVersion int64         `json:"expected_version"`
	Delay           time.Duration `json:"delay"`
	ErrorCode       string        `json:"error_code"`
}

func (request OutboxNackRequest) validate() error {
	if err := validateRequired("outbox nack id", request.NackID, ErrInvalidOutbox); err != nil {
		return err
	}
	if err := validateRequired("outbox nack message id", request.MessageID, ErrInvalidOutbox); err != nil {
		return err
	}
	if err := validateRequired("outbox nack owner", request.Owner, ErrInvalidOutbox); err != nil {
		return err
	}
	if request.ExpectedVersion <= 0 || request.Delay < 0 {
		return fmt.Errorf("%w: outbox nack expected version and delay are invalid", ErrInvalidOutbox)
	}
	return validateRequired("outbox nack error code", request.ErrorCode, ErrInvalidOutbox)
}

// Outbox is a generic transactional-outbox abstraction. Implementations may
// persist it with jobs and control operations, while the in-memory version is
// deterministic enough for workflowkit contract tests.
type Outbox interface {
	Enqueue(context.Context, OutboxEnqueueRequest) (OutboxMessage, error)
	Claim(context.Context, OutboxClaimRequest) ([]OutboxMessage, error)
	Heartbeat(context.Context, OutboxHeartbeatRequest) (OutboxMessage, error)
	Ack(context.Context, OutboxAckRequest) (OutboxMessage, error)
	Nack(context.Context, OutboxNackRequest) (OutboxMessage, error)
	Get(context.Context, string) (OutboxMessage, bool, error)
}

type enqueueRecord struct {
	request OutboxEnqueueRequest
	message OutboxMessage
}

type claimRecord struct {
	request  OutboxClaimRequest
	messages []OutboxMessage
}

type outboxHeartbeatRecord struct {
	request OutboxHeartbeatRequest
	message OutboxMessage
}

type ackRecord struct {
	request OutboxAckRequest
	message OutboxMessage
}

type nackRecord struct {
	request OutboxNackRequest
	message OutboxMessage
}

// InMemoryOutbox provides stable order, leases, idempotent message operations,
// and no background goroutine. Tests advance its injected Clock explicitly.
type InMemoryOutbox struct {
	mu sync.Mutex

	clock Clock

	messages   map[string]OutboxMessage
	enqueues   map[string]enqueueRecord
	claims     map[string]claimRecord
	heartbeats map[string]outboxHeartbeatRecord
	acks       map[string]ackRecord
	nacks      map[string]nackRecord
}

var _ Outbox = (*InMemoryOutbox)(nil)

// NewInMemoryOutbox creates an empty deterministic outbox.
func NewInMemoryOutbox(clock Clock) *InMemoryOutbox {
	return &InMemoryOutbox{
		clock:      resolveClock(clock),
		messages:   make(map[string]OutboxMessage),
		enqueues:   make(map[string]enqueueRecord),
		claims:     make(map[string]claimRecord),
		heartbeats: make(map[string]outboxHeartbeatRecord),
		acks:       make(map[string]ackRecord),
		nacks:      make(map[string]nackRecord),
	}
}

// Heartbeat extends an active delivery lease and advances its fence. The
// returned version must be used by subsequent Heartbeat, Ack, or Nack calls.
func (outbox *InMemoryOutbox) Heartbeat(ctx context.Context, request OutboxHeartbeatRequest) (OutboxMessage, error) {
	if err := contextError(ctx); err != nil {
		return OutboxMessage{}, err
	}
	if err := request.validate(); err != nil {
		return OutboxMessage{}, err
	}
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	now := outbox.now()
	outbox.releaseExpiredLocked(now)
	if record, exists := outbox.heartbeats[request.HeartbeatID]; exists {
		if record.request != request {
			return OutboxMessage{}, fmt.Errorf("%w: outbox heartbeat id %q", ErrIdempotencyConflict, request.HeartbeatID)
		}
		return record.message.Clone(), nil
	}
	message, err := outbox.leasedMessageLocked(request.MessageID, request.Owner, request.ExpectedVersion)
	if err != nil {
		return OutboxMessage{}, err
	}
	message.LeaseExpiresAt = now.Add(request.TTL)
	message.Version++
	message.UpdatedAt = now
	outbox.messages[message.MessageID] = message
	outbox.heartbeats[request.HeartbeatID] = outboxHeartbeatRecord{request: request, message: message.Clone()}
	return message.Clone(), nil
}

// Enqueue writes an idempotent pending message without dispatching it.
func (outbox *InMemoryOutbox) Enqueue(ctx context.Context, request OutboxEnqueueRequest) (OutboxMessage, error) {
	if err := contextError(ctx); err != nil {
		return OutboxMessage{}, err
	}
	if err := request.validate(); err != nil {
		return OutboxMessage{}, err
	}
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	if record, exists := outbox.enqueues[request.IdempotencyKey]; exists {
		if !sameEnqueueRequest(record.request, request) {
			return OutboxMessage{}, fmt.Errorf("%w: outbox enqueue key %q", ErrIdempotencyConflict, request.IdempotencyKey)
		}
		return record.message.Clone(), nil
	}
	if _, exists := outbox.messages[request.MessageID]; exists {
		return OutboxMessage{}, fmt.Errorf("%w: outbox message id %q", ErrIdempotencyConflict, request.MessageID)
	}
	now := outbox.now()
	message := OutboxMessage{
		MessageID:      request.MessageID,
		IdempotencyKey: request.IdempotencyKey,
		Topic:          request.Topic,
		SubjectID:      request.SubjectID,
		Payload:        append([]byte(nil), request.Payload...),
		AvailableAt:    request.AvailableAt,
		Status:         OutboxPending,
		Version:        1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	outbox.messages[message.MessageID] = message
	outbox.enqueues[request.IdempotencyKey] = enqueueRecord{request: request.Clone(), message: message.Clone()}
	return message.Clone(), nil
}

// Claim leases ready messages in AvailableAt/CreatedAt/ID order. Expired
// delivery leases return to pending before the next claim is selected.
func (outbox *InMemoryOutbox) Claim(ctx context.Context, request OutboxClaimRequest) ([]OutboxMessage, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := request.validate(); err != nil {
		return nil, err
	}
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	now := outbox.now()
	outbox.releaseExpiredLocked(now)
	if record, exists := outbox.claims[request.ClaimID]; exists {
		if record.request != request {
			return nil, fmt.Errorf("%w: outbox claim id %q", ErrIdempotencyConflict, request.ClaimID)
		}
		return cloneMessages(record.messages), nil
	}
	ready := make([]OutboxMessage, 0, len(outbox.messages))
	for _, message := range outbox.messages {
		if message.Status == OutboxPending && !now.Before(message.AvailableAt) {
			ready = append(ready, message)
		}
	}
	sort.Slice(ready, func(left, right int) bool {
		if !ready[left].AvailableAt.Equal(ready[right].AvailableAt) {
			return ready[left].AvailableAt.Before(ready[right].AvailableAt)
		}
		if !ready[left].CreatedAt.Equal(ready[right].CreatedAt) {
			return ready[left].CreatedAt.Before(ready[right].CreatedAt)
		}
		return ready[left].MessageID < ready[right].MessageID
	})
	if len(ready) > request.Limit {
		ready = ready[:request.Limit]
	}
	claimed := make([]OutboxMessage, 0, len(ready))
	for _, candidate := range ready {
		message := outbox.messages[candidate.MessageID]
		message.Status = OutboxLeased
		message.LeaseOwner = request.Owner
		message.LeaseExpiresAt = now.Add(request.TTL)
		message.DeliveryCount++
		message.Version++
		message.UpdatedAt = now
		outbox.messages[message.MessageID] = message
		claimed = append(claimed, message.Clone())
	}
	outbox.claims[request.ClaimID] = claimRecord{request: request, messages: cloneMessages(claimed)}
	return cloneMessages(claimed), nil
}

// Ack permanently records one successful delivery using the message lease
// owner and version as a fencing check.
func (outbox *InMemoryOutbox) Ack(ctx context.Context, request OutboxAckRequest) (OutboxMessage, error) {
	if err := contextError(ctx); err != nil {
		return OutboxMessage{}, err
	}
	if err := request.validate(); err != nil {
		return OutboxMessage{}, err
	}
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	now := outbox.now()
	outbox.releaseExpiredLocked(now)
	if record, exists := outbox.acks[request.AckID]; exists {
		if record.request != request {
			return OutboxMessage{}, fmt.Errorf("%w: outbox ack id %q", ErrIdempotencyConflict, request.AckID)
		}
		return record.message.Clone(), nil
	}
	message, err := outbox.leasedMessageLocked(request.MessageID, request.Owner, request.ExpectedVersion)
	if err != nil {
		return OutboxMessage{}, err
	}
	message.Status = OutboxDelivered
	message.LeaseOwner = ""
	message.LeaseExpiresAt = time.Time{}
	message.Version++
	message.UpdatedAt = now
	outbox.messages[message.MessageID] = message
	outbox.acks[request.AckID] = ackRecord{request: request, message: message.Clone()}
	return message.Clone(), nil
}

// Nack returns a failed delivery to pending state after a caller-selected,
// explicit delay. It cannot mutate payload, routing, or idempotency identity.
func (outbox *InMemoryOutbox) Nack(ctx context.Context, request OutboxNackRequest) (OutboxMessage, error) {
	if err := contextError(ctx); err != nil {
		return OutboxMessage{}, err
	}
	if err := request.validate(); err != nil {
		return OutboxMessage{}, err
	}
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	now := outbox.now()
	outbox.releaseExpiredLocked(now)
	if record, exists := outbox.nacks[request.NackID]; exists {
		if record.request != request {
			return OutboxMessage{}, fmt.Errorf("%w: outbox nack id %q", ErrIdempotencyConflict, request.NackID)
		}
		return record.message.Clone(), nil
	}
	message, err := outbox.leasedMessageLocked(request.MessageID, request.Owner, request.ExpectedVersion)
	if err != nil {
		return OutboxMessage{}, err
	}
	message.Status = OutboxPending
	message.LeaseOwner = ""
	message.LeaseExpiresAt = time.Time{}
	message.AvailableAt = now.Add(request.Delay)
	message.LastError = request.ErrorCode
	message.Version++
	message.UpdatedAt = now
	outbox.messages[message.MessageID] = message
	outbox.nacks[request.NackID] = nackRecord{request: request, message: message.Clone()}
	return message.Clone(), nil
}

// Get returns a copied message projection, including delivered history.
func (outbox *InMemoryOutbox) Get(ctx context.Context, messageID string) (OutboxMessage, bool, error) {
	if err := contextError(ctx); err != nil {
		return OutboxMessage{}, false, err
	}
	if err := validateRequired("outbox message id", messageID, ErrInvalidOutbox); err != nil {
		return OutboxMessage{}, false, err
	}
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	outbox.releaseExpiredLocked(outbox.now())
	message, exists := outbox.messages[messageID]
	if !exists {
		return OutboxMessage{}, false, nil
	}
	return message.Clone(), true, nil
}

func (outbox *InMemoryOutbox) leasedMessageLocked(messageID, owner string, expectedVersion int64) (OutboxMessage, error) {
	message, exists := outbox.messages[messageID]
	if !exists {
		return OutboxMessage{}, fmt.Errorf("%w: outbox message %q does not exist", ErrInvalidOutbox, messageID)
	}
	if message.Status != OutboxLeased || message.LeaseOwner != owner {
		return OutboxMessage{}, fmt.Errorf("%w: outbox message %q is not leased by owner", ErrInvalidOutbox, messageID)
	}
	if message.Version != expectedVersion {
		return OutboxMessage{}, fmt.Errorf("%w: outbox message %q expected version %d, current %d", ErrInvalidOutbox, messageID, expectedVersion, message.Version)
	}
	return message, nil
}

func (outbox *InMemoryOutbox) releaseExpiredLocked(now time.Time) {
	for id, message := range outbox.messages {
		if message.Status != OutboxLeased || now.Before(message.LeaseExpiresAt) {
			continue
		}
		message.Status = OutboxPending
		message.LeaseOwner = ""
		message.LeaseExpiresAt = time.Time{}
		message.AvailableAt = now
		message.Version++
		message.UpdatedAt = now
		outbox.messages[id] = message
	}
}

func (outbox *InMemoryOutbox) now() time.Time { return outbox.clock.Now().UTC() }

func sameEnqueueRequest(left, right OutboxEnqueueRequest) bool {
	if left.MessageID != right.MessageID || left.IdempotencyKey != right.IdempotencyKey || left.Topic != right.Topic || left.SubjectID != right.SubjectID || !left.AvailableAt.Equal(right.AvailableAt) || len(left.Payload) != len(right.Payload) {
		return false
	}
	for index := range left.Payload {
		if left.Payload[index] != right.Payload[index] {
			return false
		}
	}
	return true
}

func cloneMessages(messages []OutboxMessage) []OutboxMessage {
	copyMessages := make([]OutboxMessage, len(messages))
	for index, message := range messages {
		copyMessages[index] = message.Clone()
	}
	return copyMessages
}
