package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestMigrateV8OutboxEventToFencedDispatcher(t *testing.T) {
	root := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(root, dbFileName)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrationV1); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (1)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	for _, migration := range []struct {
		version int
		body    string
	}{
		{version: 2, body: migrationV2},
		{version: 3, body: migrationV3},
		{version: 4, body: migrationV4},
		{version: 5, body: migrationV5},
	} {
		if _, err := db.Exec(migration.body); err != nil {
			db.Close()
			t.Fatalf("apply v%d fixture: %v", migration.version, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (?)`, migration.version); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	for _, migration := range []struct {
		version int
		apply   func(*sql.Tx) error
	}{
		{version: 6, apply: applyMigrationV6},
		{version: 7, apply: applyMigrationV7},
		{version: 8, apply: applyMigrationV8},
	} {
		tx, err := db.Begin()
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		if err := migration.apply(tx); err != nil {
			tx.Rollback()
			db.Close()
			t.Fatalf("apply v%d fixture: %v", migration.version, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, migration.version); err != nil {
			tx.Rollback()
			db.Close()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	id, err := NewUUIDv7()
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`
		INSERT INTO outbox_events (id, topic, entity_type, entity_id, payload_json, idempotency_key, state, created_at, published_at, version)
		VALUES (?, 'legacy.pending', 'task', 'legacy-task', '{}', 'legacy-outbox', 'pending', ?, NULL, 1)
	`, id, createdAt); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	event, err := s.GetOutboxEvent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if event == nil || event.State != OutboxPending || !event.AvailableAt.Equal(createdAt) || !event.UpdatedAt.Equal(createdAt) || event.LeaseFencingToken != 0 || event.DeliveryCount != 0 {
		t.Fatalf("migrated legacy outbox event = %+v", event)
	}
	s.now = func() time.Time { return createdAt.Add(time.Hour) }
	claim, err := s.ClaimOutboxEvents(context.Background(), ClaimOutboxEventsRequest{
		IdempotencyKey: "legacy-outbox-claim", Owner: "migration-worker", Limit: 1, LeaseTTL: time.Minute, Actor: "tester",
	})
	if err != nil || len(claim.Events) != 1 || claim.Events[0].ID != id || claim.Events[0].LeaseFencingToken != 1 {
		t.Fatalf("migrated event claim = %+v, %v", claim, err)
	}
}

func TestOutboxDispatcherClaimHeartbeatAckAndExactReplay(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	clock := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	event, err := s.CreateOutboxEvent(ctx, CreateOutboxEventRequest{
		Topic: "workflow_run.execute", EntityType: "workflow_run", EntityID: "run-a", PayloadJSON: `{"run":"a"}`,
		IdempotencyKey: "outbox-event-a", Actor: "tester", Reason: "queue fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimOutboxEvents(ctx, ClaimOutboxEventsRequest{
		IdempotencyKey: "claim-a", Owner: "dispatcher-a", Limit: 1, LeaseTTL: time.Minute, Actor: "tester", Reason: "dispatch",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claim.Events) != 1 {
		t.Fatalf("claim events = %+v, want one", claim.Events)
	}
	claimed := claim.Events[0]
	if claimed.ID != event.ID || claimed.State != OutboxLeased || claimed.LeaseOwner != "dispatcher-a" || claimed.LeaseFencingToken != 1 || claimed.DeliveryCount != 1 || claimed.LeaseExpiresAt == nil || !claimed.LeaseExpiresAt.Equal(clock.Add(time.Minute)) {
		t.Fatalf("claimed event = %+v", claimed)
	}

	clock = clock.Add(10 * time.Second)
	heartbeated, err := s.HeartbeatOutboxEvent(ctx, HeartbeatOutboxEventRequest{
		IdempotencyKey: "heartbeat-a", OutboxEventID: claimed.ID, Owner: "dispatcher-a", ExpectedVersion: claimed.Version,
		LeaseFencingToken: claimed.LeaseFencingToken, LeaseTTL: 2 * time.Minute, Actor: "tester", Reason: "still working",
	})
	if err != nil {
		t.Fatal(err)
	}
	if heartbeated.Version != claimed.Version+1 || heartbeated.LeaseFencingToken != claimed.LeaseFencingToken || heartbeated.LeaseExpiresAt == nil || !heartbeated.LeaseExpiresAt.Equal(clock.Add(2*time.Minute)) {
		t.Fatalf("heartbeat result = %+v", heartbeated)
	}
	heartbeatReplay, err := s.HeartbeatOutboxEvent(ctx, HeartbeatOutboxEventRequest{
		IdempotencyKey: "heartbeat-a", OutboxEventID: claimed.ID, Owner: "dispatcher-a", ExpectedVersion: claimed.Version,
		LeaseFencingToken: claimed.LeaseFencingToken, LeaseTTL: 2 * time.Minute, Actor: "another-actor", Reason: "retry",
	})
	if err != nil || heartbeatReplay.Version != heartbeated.Version || heartbeatReplay.LeaseExpiresAt == nil || !heartbeatReplay.LeaseExpiresAt.Equal(*heartbeated.LeaseExpiresAt) {
		t.Fatalf("heartbeat replay = %+v, %v", heartbeatReplay, err)
	}

	acknowledged, err := s.AckOutboxEvent(ctx, AckOutboxEventRequest{
		IdempotencyKey: "ack-a", OutboxEventID: event.ID, Owner: "dispatcher-a", ExpectedVersion: heartbeated.Version,
		LeaseFencingToken: heartbeated.LeaseFencingToken, Actor: "tester", Reason: "handler succeeded",
	})
	if err != nil {
		t.Fatal(err)
	}
	if acknowledged.State != OutboxPublished || acknowledged.LeaseOwner != "" || acknowledged.LeaseExpiresAt != nil || acknowledged.PublishedAt == nil {
		t.Fatalf("acknowledged event = %+v", acknowledged)
	}
	ackReplay, err := s.AckOutboxEvent(ctx, AckOutboxEventRequest{
		IdempotencyKey: "ack-a", OutboxEventID: event.ID, Owner: "dispatcher-a", ExpectedVersion: heartbeated.Version,
		LeaseFencingToken: heartbeated.LeaseFencingToken, Actor: "another-actor", Reason: "retry",
	})
	if err != nil || ackReplay.Version != acknowledged.Version || ackReplay.State != OutboxPublished {
		t.Fatalf("ack replay = %+v, %v", ackReplay, err)
	}
	claimReplay, err := s.ClaimOutboxEvents(ctx, ClaimOutboxEventsRequest{
		IdempotencyKey: "claim-a", Owner: "dispatcher-a", Limit: 1, LeaseTTL: time.Minute, Actor: "tester", Reason: "claim retry",
	})
	if err != nil || len(claimReplay.Events) != 1 || claimReplay.Events[0].Version != claimed.Version || claimReplay.Events[0].State != OutboxLeased || claimReplay.Events[0].LeaseFencingToken != claimed.LeaseFencingToken {
		t.Fatalf("claim replay did not preserve committed claim snapshot: %+v, %v", claimReplay, err)
	}
	if _, err := s.ClaimOutboxEvents(ctx, ClaimOutboxEventsRequest{
		IdempotencyKey: "claim-a", Owner: "another-dispatcher", Limit: 1, LeaseTTL: time.Minute, Actor: "tester",
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting claim replay = %v, want idempotency conflict", err)
	}
}

func TestOutboxDispatcherNackRetryExpiryAndStaleFence(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	clock := time.Date(2026, 7, 13, 13, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	event, err := s.CreateOutboxEvent(ctx, CreateOutboxEventRequest{
		Topic: "control.requested", EntityType: "workflow_run", EntityID: "run-b", PayloadJSON: `{}`,
		IdempotencyKey: "outbox-event-b", Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimOutboxEvents(ctx, ClaimOutboxEventsRequest{IdempotencyKey: "claim-b1", Owner: "dispatcher-a", Limit: 1, LeaseTTL: time.Minute, Actor: "tester"})
	if err != nil || len(claim.Events) != 1 {
		t.Fatalf("initial claim = %+v, %v", claim, err)
	}
	first := claim.Events[0]
	nacked, err := s.NackOutboxEvent(ctx, NackOutboxEventRequest{
		IdempotencyKey: "nack-b1", OutboxEventID: first.ID, Owner: first.LeaseOwner, ExpectedVersion: first.Version,
		LeaseFencingToken: first.LeaseFencingToken, RetryDelay: 5 * time.Minute, ErrorCode: "temporary_provider_failure", Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	if nacked.State != OutboxPending || nacked.LeaseOwner != "" || nacked.LastError != "temporary_provider_failure" || !nacked.AvailableAt.Equal(clock.Add(5*time.Minute)) {
		t.Fatalf("nacked event = %+v", nacked)
	}
	empty, err := s.ClaimOutboxEvents(ctx, ClaimOutboxEventsRequest{IdempotencyKey: "claim-b-before-delay", Owner: "dispatcher-b", Limit: 1, LeaseTTL: time.Minute, Actor: "tester"})
	if err != nil || len(empty.Events) != 0 {
		t.Fatalf("claim before retry time = %+v, %v", empty, err)
	}
	clock = clock.Add(5 * time.Minute)
	secondClaim, err := s.ClaimOutboxEvents(ctx, ClaimOutboxEventsRequest{IdempotencyKey: "claim-b2", Owner: "dispatcher-b", Limit: 1, LeaseTTL: time.Minute, Actor: "tester"})
	if err != nil || len(secondClaim.Events) != 1 {
		t.Fatalf("claim after retry time = %+v, %v", secondClaim, err)
	}
	second := secondClaim.Events[0]
	if second.ID != event.ID || second.LeaseFencingToken != first.LeaseFencingToken+1 || second.DeliveryCount != 2 {
		t.Fatalf("retry claim = %+v", second)
	}

	clock = clock.Add(time.Minute)
	thirdClaim, err := s.ClaimOutboxEvents(ctx, ClaimOutboxEventsRequest{IdempotencyKey: "claim-b3", Owner: "dispatcher-c", Limit: 1, LeaseTTL: time.Minute, Actor: "tester"})
	if err != nil || len(thirdClaim.Events) != 1 {
		t.Fatalf("claim after lease expiry = %+v, %v", thirdClaim, err)
	}
	third := thirdClaim.Events[0]
	if third.LeaseFencingToken != second.LeaseFencingToken+1 || third.LeaseOwner != "dispatcher-c" {
		t.Fatalf("reclaimed event = %+v", third)
	}
	if _, err := s.AckOutboxEvent(ctx, AckOutboxEventRequest{
		IdempotencyKey: "ack-stale-b", OutboxEventID: second.ID, Owner: second.LeaseOwner, ExpectedVersion: second.Version,
		LeaseFencingToken: second.LeaseFencingToken, Actor: "tester",
	}); !errors.Is(err, ErrFencingToken) {
		t.Fatalf("stale acknowledgement = %v, want fencing error", err)
	}
	if _, err := s.HeartbeatOutboxEvent(ctx, HeartbeatOutboxEventRequest{
		IdempotencyKey: "heartbeat-stale-b", OutboxEventID: second.ID, Owner: second.LeaseOwner, ExpectedVersion: second.Version,
		LeaseFencingToken: second.LeaseFencingToken, LeaseTTL: time.Minute, Actor: "tester",
	}); !errors.Is(err, ErrFencingToken) {
		t.Fatalf("stale heartbeat = %v, want fencing error", err)
	}
	if _, err := s.NackOutboxEvent(ctx, NackOutboxEventRequest{
		IdempotencyKey: "nack-stale-b", OutboxEventID: second.ID, Owner: second.LeaseOwner, ExpectedVersion: second.Version,
		LeaseFencingToken: second.LeaseFencingToken, RetryDelay: time.Minute, ErrorCode: "stale_worker", Actor: "tester",
	}); !errors.Is(err, ErrFencingToken) {
		t.Fatalf("stale nack = %v, want fencing error", err)
	}
	if _, err := s.AckOutboxEvent(ctx, AckOutboxEventRequest{
		IdempotencyKey: "ack-current-b", OutboxEventID: third.ID, Owner: third.LeaseOwner, ExpectedVersion: third.Version,
		LeaseFencingToken: third.LeaseFencingToken, Actor: "tester",
	}); err != nil {
		t.Fatalf("current acknowledgement: %v", err)
	}
}

func TestOutboxDispatcherClaimsAreExclusiveAndLeasedEventsBlockPurge(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, _ := createValidatedTaskAndRevision(t, s)
	for ordinal := 0; ordinal < 8; ordinal++ {
		key := strconv.Itoa(ordinal)
		if _, err := s.CreateOutboxEvent(ctx, CreateOutboxEventRequest{
			Topic: "workflow_run.execute", EntityType: "task", EntityID: task.ID, PayloadJSON: `{"ordinal":` + key + `}`,
			IdempotencyKey: "concurrent-outbox-" + key, Actor: "tester",
		}); err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		claim OutboxDispatchClaim
		err   error
	}
	results := make(chan result, 8)
	var group sync.WaitGroup
	for ordinal := 0; ordinal < 8; ordinal++ {
		group.Add(1)
		go func(ordinal int) {
			defer group.Done()
			key := strconv.Itoa(ordinal)
			claim, err := s.ClaimOutboxEvents(context.Background(), ClaimOutboxEventsRequest{
				IdempotencyKey: "concurrent-claim-" + key, Owner: "worker-" + key, Limit: 1,
				LeaseTTL: time.Minute, Actor: "tester",
			})
			results <- result{claim: claim, err: err}
		}(ordinal)
	}
	group.Wait()
	close(results)
	ids := make([]string, 0, 8)
	for result := range results {
		if result.err != nil || len(result.claim.Events) != 1 {
			t.Fatalf("concurrent claim = %+v, %v", result.claim, result.err)
		}
		ids = append(ids, result.claim.Events[0].ID)
	}
	sort.Strings(ids)
	for index := 1; index < len(ids); index++ {
		if ids[index] == ids[index-1] {
			t.Fatalf("concurrent claims leased event %s twice: %v", ids[index], ids)
		}
	}
	dependencies, err := s.QueryPurgeDependencies(ctx, PurgeDependencyQuery{EntityType: "task", EntityID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(dependencies.PendingOutboxIDs) != 8 {
		t.Fatalf("leased outbox records did not block purge: %+v", dependencies)
	}
}

func TestOutboxDispatcherOperationIdentityIsGloballyFenced(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, _ := createValidatedTaskAndRevision(t, s)
	if _, err := s.CreateOutboxEvent(ctx, CreateOutboxEventRequest{
		Topic: "fixture", EntityType: "task", EntityID: task.ID, PayloadJSON: `{}`, IdempotencyKey: "operation-collision-event", Actor: "tester",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimOutboxEvents(ctx, ClaimOutboxEventsRequest{
		ID: task.ID, IdempotencyKey: "operation-collision-claim", Owner: "worker", Limit: 1, LeaseTTL: time.Minute, Actor: "tester",
	}); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("delivery operation cross-entity collision = %v, want identity collision", err)
	}
	if _, err := s.ClaimOutboxEvents(ctx, ClaimOutboxEventsRequest{
		IdempotencyKey: "operation-after-collision", Owner: "worker", Limit: 1, LeaseTTL: time.Minute, Actor: "tester",
	}); err != nil {
		t.Fatalf("failed operation insertion left partial event claim: %v", err)
	}
}
