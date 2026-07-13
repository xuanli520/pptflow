package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

var ErrOutboxDispatcherConfiguration = errors.New("v2 executor: invalid outbox dispatcher configuration")

// OutboxDeliveryHandler receives one immutable event snapshot while its
// SQLite delivery lease remains live. It must return only after the local
// side effect has either completed or become safe to retry. Returning an
// error causes a fenced NACK; returning nil causes a fenced ACK.
type OutboxDeliveryHandler interface {
	DeliverOutboxEvent(context.Context, store.OutboxEvent) error
}

// OutboxDeliveryHandlerFunc adapts focused handlers and local runtime routes.
type OutboxDeliveryHandlerFunc func(context.Context, store.OutboxEvent) error

func (function OutboxDeliveryHandlerFunc) DeliverOutboxEvent(ctx context.Context, event store.OutboxEvent) error {
	return function(ctx, event)
}

// OutboxDeliveryFailure can classify a retry without persisting arbitrary
// handler text into durable state. Unclassified errors use delivery_failed.
type OutboxDeliveryFailure interface {
	error
	OutboxDeliveryErrorCode() string
}

// OutboxDispatcherConfig controls a local SQLite delivery worker. RetryDelay
// is required rather than hidden in the dispatcher, so deployment policy owns
// retry pacing explicitly.
type OutboxDispatcherConfig struct {
	Store          *store.Store
	Owner          string
	Actor          string
	Reason         string
	LeaseTTL       time.Duration
	HeartbeatEvery time.Duration
	RetryDelay     time.Duration
	PollInterval   time.Duration
	Handler        OutboxDeliveryHandler
}

// OutboxDispatcher processes local control-plane events through a SQLite
// claim/heartbeat/ack/nack protocol. A caller's UI context is never the
// lifetime authority for a claimed delivery.
type OutboxDispatcher struct {
	store          *store.Store
	owner          string
	actor          string
	reason         string
	leaseTTL       time.Duration
	heartbeatEvery time.Duration
	retryDelay     time.Duration
	pollInterval   time.Duration
	handler        OutboxDeliveryHandler
}

// NewOutboxDispatcher validates one explicit local worker profile.
func NewOutboxDispatcher(config OutboxDispatcherConfig) (*OutboxDispatcher, error) {
	if config.Store == nil {
		return nil, fmt.Errorf("%w: store is required", ErrOutboxDispatcherConfiguration)
	}
	owner := strings.TrimSpace(config.Owner)
	if owner == "" {
		return nil, fmt.Errorf("%w: owner is required", ErrOutboxDispatcherConfiguration)
	}
	if config.Handler == nil {
		return nil, fmt.Errorf("%w: handler is required", ErrOutboxDispatcherConfiguration)
	}
	if config.LeaseTTL == 0 {
		config.LeaseTTL = store.DefaultLeaseTTL
	}
	if config.HeartbeatEvery == 0 {
		config.HeartbeatEvery = store.DefaultLeaseHeartbeatInterval
	}
	if config.PollInterval == 0 {
		config.PollInterval = 250 * time.Millisecond
	}
	if config.LeaseTTL <= 0 || config.HeartbeatEvery <= 0 || config.HeartbeatEvery >= config.LeaseTTL || config.RetryDelay <= 0 || config.PollInterval <= 0 {
		return nil, fmt.Errorf("%w: lease TTL, heartbeat interval, retry delay, and poll interval must be positive; heartbeat must be shorter than TTL", ErrOutboxDispatcherConfiguration)
	}
	return &OutboxDispatcher{
		store:          config.Store,
		owner:          owner,
		actor:          defaultWorkerActor(config.Actor, owner),
		reason:         strings.TrimSpace(config.Reason),
		leaseTTL:       config.LeaseTTL,
		heartbeatEvery: config.HeartbeatEvery,
		retryDelay:     config.RetryDelay,
		pollInterval:   config.PollInterval,
		handler:        config.Handler,
	}, nil
}

// OutboxDispatcherResult describes one claim cycle. Empty is true only after
// a durable poll committed an empty result; Retried means NACK completed.
type OutboxDispatcherResult struct {
	Claim     store.OutboxDispatchClaim
	Event     *store.OutboxEvent
	Delivered bool
	Retried   bool
	Empty     bool
}

// RunOnce claims one record, keeps its fence alive while handling it, and
// projects the proven result. It never acknowledges a lease that heartbeat
// loss may have allowed another worker to reclaim.
func (dispatcher *OutboxDispatcher) RunOnce(ctx context.Context) (OutboxDispatcherResult, error) {
	if err := dispatcher.validate(); err != nil {
		return OutboxDispatcherResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return OutboxDispatcherResult{}, err
	}
	claimID, err := store.NewUUIDv7()
	if err != nil {
		return OutboxDispatcherResult{}, fmt.Errorf("allocate outbox claim key: %w", err)
	}
	claim, err := dispatcher.store.ClaimOutboxEvents(ctx, store.ClaimOutboxEventsRequest{
		IdempotencyKey: "outbox-dispatcher-claim:" + claimID,
		Owner:          dispatcher.owner,
		Limit:          1,
		LeaseTTL:       dispatcher.leaseTTL,
		Actor:          dispatcher.actor,
		Reason:         dispatcher.reasonFor("claim outbox event"),
	})
	if err != nil {
		return OutboxDispatcherResult{}, fmt.Errorf("claim outbox event: %w", err)
	}
	result := OutboxDispatcherResult{Claim: claim}
	if len(claim.Events) == 0 {
		result.Empty = true
		return result, nil
	}
	event := claim.Events[0]
	result.Event = &event

	leaseContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	heartbeats := newOutboxLeaseHeartbeats(dispatcher, event, cancel)
	heartbeats.start()
	handlerErr := dispatcher.handler.DeliverOutboxEvent(leaseContext, event)
	heartbeats.stop()
	if heartbeats.wasLost() {
		if handlerErr != nil {
			return result, fmt.Errorf("outbox event %s lease lost while handling: %w", event.ID, handlerErr)
		}
		return result, fmt.Errorf("outbox event %s lease lost while handling", event.ID)
	}
	current := heartbeats.current()
	if handlerErr != nil {
		nackID, idErr := store.NewUUIDv7()
		if idErr != nil {
			return result, fmt.Errorf("allocate outbox nack key: %w", idErr)
		}
		nacked, nackErr := dispatcher.store.NackOutboxEvent(context.Background(), store.NackOutboxEventRequest{
			IdempotencyKey:    "outbox-dispatcher-nack:" + nackID,
			OutboxEventID:     current.ID,
			Owner:             dispatcher.owner,
			ExpectedVersion:   current.Version,
			LeaseFencingToken: current.LeaseFencingToken,
			RetryDelay:        dispatcher.retryDelay,
			ErrorCode:         outboxDeliveryErrorCode(handlerErr),
			Actor:             dispatcher.actor,
			Reason:            dispatcher.reasonFor("retry outbox event"),
		})
		if nackErr != nil {
			return result, fmt.Errorf("nack outbox event %s after handler failure %w: %v", event.ID, handlerErr, nackErr)
		}
		result.Event = &nacked
		result.Retried = true
		return result, handlerErr
	}
	ackID, err := store.NewUUIDv7()
	if err != nil {
		return result, fmt.Errorf("allocate outbox acknowledgement key: %w", err)
	}
	acknowledged, err := dispatcher.store.AckOutboxEvent(context.Background(), store.AckOutboxEventRequest{
		IdempotencyKey:    "outbox-dispatcher-ack:" + ackID,
		OutboxEventID:     current.ID,
		Owner:             dispatcher.owner,
		ExpectedVersion:   current.Version,
		LeaseFencingToken: current.LeaseFencingToken,
		Actor:             dispatcher.actor,
		Reason:            dispatcher.reasonFor("acknowledge outbox event"),
	})
	if err != nil {
		return result, fmt.Errorf("acknowledge outbox event %s: %w", event.ID, err)
	}
	result.Event = &acknowledged
	result.Delivered = true
	return result, nil
}

// Run polls until its process context ends. A handler failure already NACKed
// is a durable event outcome, so it does not abandon unrelated queued work.
func (dispatcher *OutboxDispatcher) Run(ctx context.Context) error {
	if err := dispatcher.validate(); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := dispatcher.RunOnce(ctx)
		if err != nil {
			if result.Retried {
				continue
			}
			return err
		}
		if !result.Empty {
			continue
		}
		timer := time.NewTimer(dispatcher.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (dispatcher *OutboxDispatcher) validate() error {
	if dispatcher == nil || dispatcher.store == nil || dispatcher.handler == nil || dispatcher.owner == "" {
		return ErrOutboxDispatcherConfiguration
	}
	return nil
}

func (dispatcher *OutboxDispatcher) reasonFor(action string) string {
	if dispatcher.reason == "" {
		return action
	}
	return dispatcher.reason + ": " + action
}

func outboxDeliveryErrorCode(err error) string {
	var classified OutboxDeliveryFailure
	if errors.As(err, &classified) {
		if code := strings.TrimSpace(classified.OutboxDeliveryErrorCode()); code != "" {
			return code
		}
	}
	return "delivery_failed"
}

type outboxLeaseHeartbeats struct {
	dispatcher *OutboxDispatcher
	cancel     context.CancelFunc

	mu     sync.Mutex
	event  store.OutboxEvent
	lost   chan struct{}
	once   sync.Once
	stopCh chan struct{}
	doneCh chan struct{}
}

func newOutboxLeaseHeartbeats(dispatcher *OutboxDispatcher, event store.OutboxEvent, cancel context.CancelFunc) *outboxLeaseHeartbeats {
	return &outboxLeaseHeartbeats{
		dispatcher: dispatcher,
		cancel:     cancel,
		event:      event,
		lost:       make(chan struct{}),
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
}

func (heartbeats *outboxLeaseHeartbeats) start() {
	go func() {
		defer close(heartbeats.doneCh)
		ticker := time.NewTicker(heartbeats.dispatcher.heartbeatEvery)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeats.stopCh:
				return
			case <-ticker.C:
				if err := heartbeats.heartbeat(context.Background()); err != nil {
					heartbeats.once.Do(func() {
						close(heartbeats.lost)
						heartbeats.cancel()
					})
					return
				}
			}
		}
	}()
}

func (heartbeats *outboxLeaseHeartbeats) stop() {
	close(heartbeats.stopCh)
	<-heartbeats.doneCh
}

func (heartbeats *outboxLeaseHeartbeats) wasLost() bool {
	select {
	case <-heartbeats.lost:
		return true
	default:
		return false
	}
}

func (heartbeats *outboxLeaseHeartbeats) current() store.OutboxEvent {
	heartbeats.mu.Lock()
	defer heartbeats.mu.Unlock()
	return heartbeats.event
}

func (heartbeats *outboxLeaseHeartbeats) heartbeat(ctx context.Context) error {
	heartbeats.mu.Lock()
	defer heartbeats.mu.Unlock()
	key, err := store.NewUUIDv7()
	if err != nil {
		return err
	}
	updated, err := heartbeats.dispatcher.store.HeartbeatOutboxEvent(ctx, store.HeartbeatOutboxEventRequest{
		IdempotencyKey:    "outbox-dispatcher-heartbeat:" + key,
		OutboxEventID:     heartbeats.event.ID,
		Owner:             heartbeats.dispatcher.owner,
		ExpectedVersion:   heartbeats.event.Version,
		LeaseFencingToken: heartbeats.event.LeaseFencingToken,
		LeaseTTL:          heartbeats.dispatcher.leaseTTL,
		Actor:             heartbeats.dispatcher.actor,
		Reason:            heartbeats.dispatcher.reasonFor("heartbeat outbox event"),
	})
	if err != nil {
		return err
	}
	heartbeats.event = updated
	return nil
}
