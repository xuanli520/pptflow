package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const outboxEventSelect = `
	SELECT id, topic, entity_type, entity_id, payload_json, idempotency_key, state,
	       available_at, lease_owner, lease_expires_at, lease_fencing_token,
	       delivery_count, last_error, created_at, updated_at, published_at, version
	FROM outbox_events`

// CreateOutboxEvent records local durable delivery work. This package does not
// execute a provider call or any remote publish operation.
func (s *Store) CreateOutboxEvent(ctx context.Context, request CreateOutboxEventRequest) (OutboxEvent, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return OutboxEvent{}, err
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return OutboxEvent{}, err
	}
	topic, err := normalizeRequired(request.Topic, "outbox topic")
	if err != nil {
		return OutboxEvent{}, err
	}
	entityType, err := normalizeRequired(request.EntityType, "outbox entity type")
	if err != nil {
		return OutboxEvent{}, err
	}
	entityID, err := normalizeRequired(request.EntityID, "outbox entity ID")
	if err != nil {
		return OutboxEvent{}, err
	}
	key, err := normalizeRequired(request.IdempotencyKey, "outbox idempotency key")
	if err != nil {
		return OutboxEvent{}, err
	}
	payload, err := normalizeJSON(request.PayloadJSON, "outbox payload")
	if err != nil {
		return OutboxEvent{}, err
	}
	now := s.now().UTC()
	availableAt := request.AvailableAt
	if availableAt.IsZero() {
		availableAt = now
	} else {
		availableAt = availableAt.UTC()
	}
	event := OutboxEvent{
		ID:             id,
		Topic:          topic,
		EntityType:     entityType,
		EntityID:       entityID,
		PayloadJSON:    payload,
		IdempotencyKey: key,
		State:          OutboxPending,
		AvailableAt:    availableAt,
		CreatedAt:      now,
		UpdatedAt:      now,
		Version:        1,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OutboxEvent{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO outbox_events (
			id, topic, entity_type, entity_id, payload_json, idempotency_key, state,
			created_at, published_at, version, available_at, lease_owner, lease_expires_at,
			lease_fencing_token, delivery_count, last_error, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, '', NULL, 0, 0, '', ?)
	`, event.ID, event.Topic, event.EntityType, event.EntityID, event.PayloadJSON, event.IdempotencyKey,
		event.State, event.CreatedAt, event.Version, event.AvailableAt, event.UpdatedAt)
	if err != nil {
		if isGlobalIdentityCollision(err) {
			return OutboxEvent{}, fmt.Errorf("%w: outbox event %s", ErrIdentityCollision, event.ID)
		}
		if !isUniqueConstraint(err) {
			return OutboxEvent{}, err
		}
		if existingByID, idErr := getOutboxEventTx(ctx, tx, event.ID); idErr == nil && existingByID.ID != "" {
			return OutboxEvent{}, fmt.Errorf("%w: outbox event %s", ErrIdentityCollision, event.ID)
		} else if idErr != nil && !isNotFound(idErr) {
			return OutboxEvent{}, idErr
		}
		existing, existingErr := getOutboxByIdempotencyTx(ctx, tx, event.Topic, event.EntityType, event.EntityID, event.IdempotencyKey)
		if existingErr != nil {
			return OutboxEvent{}, existingErr
		}
		if existing.PayloadJSON == event.PayloadJSON && (request.AvailableAt.IsZero() || existing.AvailableAt.Equal(event.AvailableAt)) {
			if err := tx.Commit(); err != nil {
				return OutboxEvent{}, err
			}
			return existing, nil
		}
		return OutboxEvent{}, fmt.Errorf("%w: outbox key %s", ErrIdempotencyConflict, event.IdempotencyKey)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "outbox_event",
		EntityID:    event.ID,
		Action:      "outbox_event.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"topic": event.Topic, "entity_type": event.EntityType, "entity_id": event.EntityID, "idempotency_key": event.IdempotencyKey}),
		CreatedAt:   now,
	}); err != nil {
		return OutboxEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return OutboxEvent{}, err
	}
	return event, nil
}

func (s *Store) GetOutboxEvent(ctx context.Context, eventID string) (*OutboxEvent, error) {
	if !isUUIDv7(eventID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	event, err := scanOutboxEvent(s.db.QueryRowContext(ctx, outboxEventSelect+" WHERE id = ?", eventID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &event, nil
}

func (s *Store) ListPendingOutboxEvents(ctx context.Context, limit int) ([]OutboxEvent, error) {
	query := outboxEventSelect + " WHERE state = 'pending' ORDER BY created_at ASC, id ASC"
	var args []any
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []OutboxEvent
	for rows.Next() {
		event, err := scanOutboxEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

const localPackageReleaseSelect = `
	SELECT id, version, revision_id, task_id, task_digest, package_ref, evidence_ref,
	       published_at, withdrawn_at, withdrawn_by, created_by, record_version
	FROM releases`

// CreateLocalPackageRelease records a package already materialized locally.
// It deliberately has no provider client, upload URL, or remote side effect.
func (s *Store) CreateLocalPackageRelease(ctx context.Context, request CreateLocalPackageReleaseRequest) (LocalPackageRelease, error) {
	if !isUUIDv7(request.TaskID) || !isUUIDv7(request.RevisionID) {
		return LocalPackageRelease{}, ErrInvalidUUIDv7Identity
	}
	if err := ValidateTaskDigestV2(request.TaskDigest); err != nil {
		return LocalPackageRelease{}, err
	}
	requestedID := strings.TrimSpace(request.ID)
	idempotencyKey := strings.TrimSpace(request.IdempotencyKey)
	if idempotencyKey != "" {
		if !isUUIDv7(idempotencyKey) {
			return LocalPackageRelease{}, ErrInvalidUUIDv7Identity
		}
		if requestedID != "" && requestedID != idempotencyKey {
			return LocalPackageRelease{}, fmt.Errorf("%w: local package release identity differs from idempotency key", ErrIdempotencyConflict)
		}
		requestedID = idempotencyKey
	}
	id, err := s.newV2ID(requestedID)
	if err != nil {
		return LocalPackageRelease{}, err
	}
	releaseVersion, err := normalizeRequired(request.ReleaseVersion, "release version")
	if err != nil {
		return LocalPackageRelease{}, err
	}
	packageRef, err := normalizeRequired(request.PackageRef, "local package reference")
	if err != nil {
		return LocalPackageRelease{}, err
	}
	evidenceRef, err := normalizeRequired(request.EvidenceRef, "release evidence reference")
	if err != nil {
		return LocalPackageRelease{}, err
	}
	if _, err := s.BackupBeforeCriticalOperation(ctx, "local_package_release"); err != nil {
		return LocalPackageRelease{}, err
	}
	now := s.now().UTC()
	release := LocalPackageRelease{
		ID:             id,
		ReleaseVersion: releaseVersion,
		RevisionID:     request.RevisionID,
		TaskID:         request.TaskID,
		TaskDigest:     strings.TrimSpace(request.TaskDigest),
		PackageRef:     packageRef,
		EvidenceRef:    evidenceRef,
		PublishedAt:    now,
		CreatedBy:      resolveActor(request.Actor),
		RecordVersion:  1,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LocalPackageRelease{}, err
	}
	defer tx.Rollback()
	revision, err := getTaskRevisionTx(ctx, tx, release.RevisionID)
	if err != nil {
		return LocalPackageRelease{}, err
	}
	if revision.TaskID != release.TaskID || revision.TaskDigest != release.TaskDigest {
		return LocalPackageRelease{}, fmt.Errorf("release task/revision/digest binding does not match durable revision")
	}
	if revision.State != RevisionStateReleased {
		return LocalPackageRelease{}, fmt.Errorf("%w: revision %s is %s", ErrRevisionNotValidated, revision.ID, revision.State)
	}
	if _, err := getTaskV2Tx(ctx, tx, release.TaskID); err != nil {
		return LocalPackageRelease{}, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO releases (
			id, version, revision_id, task_id, task_digest, package_ref, evidence_ref,
			published_at, withdrawn_at, withdrawn_by, created_by, record_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, '', ?, ?)
	`, release.ID, release.ReleaseVersion, release.RevisionID, release.TaskID, release.TaskDigest, release.PackageRef,
		release.EvidenceRef, release.PublishedAt, release.CreatedBy, release.RecordVersion)
	if err != nil {
		if isUniqueConstraint(err) {
			existing, existingErr := getLocalPackageReleaseTx(ctx, tx, release.ID)
			if existingErr == nil {
				if sameLocalPackageRelease(existing, release) {
					if err := tx.Commit(); err != nil {
						return LocalPackageRelease{}, err
					}
					return existing, nil
				}
				return LocalPackageRelease{}, fmt.Errorf("%w: local package release key %s", ErrIdempotencyConflict, release.ID)
			}
			if !isNotFound(existingErr) {
				return LocalPackageRelease{}, existingErr
			}
			return LocalPackageRelease{}, fmt.Errorf("%w: release %s or version %s", ErrIdentityCollision, release.ID, release.ReleaseVersion)
		}
		return LocalPackageRelease{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "release",
		EntityID:    release.ID,
		Action:      "release.local_package_recorded",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"version": release.ReleaseVersion, "task_id": release.TaskID, "revision_id": release.RevisionID, "package_ref": release.PackageRef, "evidence_ref": release.EvidenceRef}),
		CreatedAt:   now,
	}); err != nil {
		return LocalPackageRelease{}, err
	}
	if err := tx.Commit(); err != nil {
		return LocalPackageRelease{}, err
	}
	return release, nil
}

func sameLocalPackageRelease(left, right LocalPackageRelease) bool {
	return left.ID == right.ID &&
		left.ReleaseVersion == right.ReleaseVersion &&
		left.RevisionID == right.RevisionID &&
		left.TaskID == right.TaskID &&
		left.TaskDigest == right.TaskDigest &&
		left.PackageRef == right.PackageRef &&
		left.EvidenceRef == right.EvidenceRef &&
		left.CreatedBy == right.CreatedBy
}

func (s *Store) GetLocalPackageRelease(ctx context.Context, releaseID string) (*LocalPackageRelease, error) {
	if !isUUIDv7(releaseID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	release, err := scanLocalPackageRelease(s.db.QueryRowContext(ctx, localPackageReleaseSelect+" WHERE id = ?", releaseID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &release, nil
}

// GetLocalPackageReleaseByVersion finds the globally immutable local package
// release identity used to reconcile a process that crashed after writing its
// immutable receipt but before returning to the caller.
func (s *Store) GetLocalPackageReleaseByVersion(ctx context.Context, releaseVersion string) (*LocalPackageRelease, error) {
	releaseVersion, err := normalizeRequired(releaseVersion, "release version")
	if err != nil {
		return nil, err
	}
	release, err := scanLocalPackageRelease(s.db.QueryRowContext(ctx, localPackageReleaseSelect+" WHERE version = ?", releaseVersion))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &release, nil
}

func (s *Store) ListLocalPackageReleasesForTask(ctx context.Context, taskID string) ([]LocalPackageRelease, error) {
	if !isUUIDv7(taskID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, localPackageReleaseSelect+" WHERE task_id = ? ORDER BY published_at DESC, id DESC", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var releases []LocalPackageRelease
	for rows.Next() {
		release, err := scanLocalPackageRelease(rows)
		if err != nil {
			return nil, err
		}
		releases = append(releases, release)
	}
	return releases, rows.Err()
}

func (s *Store) GetReleaseChannel(ctx context.Context, channel string) (*ReleaseChannel, error) {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		return nil, fmt.Errorf("release channel is required")
	}
	pointer, err := scanReleaseChannel(s.db.QueryRowContext(ctx, `SELECT channel, release_id, updated_at, updated_by, version FROM release_channels WHERE channel = ?`, channel))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pointer, nil
}

func (s *Store) SetReleaseChannel(ctx context.Context, request SetReleaseChannelRequest) (ReleaseChannel, error) {
	channel, err := normalizeRequired(request.Channel, "release channel")
	if err != nil {
		return ReleaseChannel{}, err
	}
	if !isUUIDv7(request.ReleaseID) {
		return ReleaseChannel{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion < 0 {
		return ReleaseChannel{}, fmt.Errorf("expected channel version cannot be negative")
	}
	if _, err := s.BackupBeforeCriticalOperation(ctx, "release_channel_promotion"); err != nil {
		return ReleaseChannel{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReleaseChannel{}, err
	}
	defer tx.Rollback()
	release, err := getLocalPackageReleaseTx(ctx, tx, request.ReleaseID)
	if err != nil {
		return ReleaseChannel{}, err
	}
	if release.WithdrawnAt != nil {
		return ReleaseChannel{}, fmt.Errorf("cannot point a channel at withdrawn release %s", release.ID)
	}
	now := s.now().UTC()
	actor := resolveActor(request.Actor)
	pointer, err := getReleaseChannelTx(ctx, tx, channel)
	if err != nil && !isNotFound(err) {
		return ReleaseChannel{}, err
	}
	if isNotFound(err) {
		if request.ExpectedVersion != 0 {
			return ReleaseChannel{}, fmt.Errorf("%w: release channel %s", ErrOptimisticLock, channel)
		}
		pointer = ReleaseChannel{Channel: channel, ReleaseID: release.ID, UpdatedAt: now, UpdatedBy: actor, Version: 1}
		_, err = tx.ExecContext(ctx, `INSERT INTO release_channels (channel, release_id, updated_at, updated_by, version) VALUES (?, ?, ?, ?, ?)`, pointer.Channel, pointer.ReleaseID, pointer.UpdatedAt, pointer.UpdatedBy, pointer.Version)
		if err != nil {
			if isUniqueConstraint(err) {
				return ReleaseChannel{}, fmt.Errorf("%w: release channel %s", ErrOptimisticLock, channel)
			}
			return ReleaseChannel{}, err
		}
	} else {
		if pointer.Version != request.ExpectedVersion {
			return ReleaseChannel{}, fmt.Errorf("%w: release channel %s", ErrOptimisticLock, channel)
		}
		if pointer.ReleaseID == release.ID {
			if err := tx.Commit(); err != nil {
				return ReleaseChannel{}, err
			}
			return pointer, nil
		}
		pointer.ReleaseID = release.ID
		pointer.UpdatedAt = now
		pointer.UpdatedBy = actor
		pointer.Version++
		result, err := tx.ExecContext(ctx, `
			UPDATE release_channels SET release_id = ?, updated_at = ?, updated_by = ?, version = ?
			WHERE channel = ? AND version = ?
		`, pointer.ReleaseID, pointer.UpdatedAt, pointer.UpdatedBy, pointer.Version, pointer.Channel, request.ExpectedVersion)
		if err != nil {
			return ReleaseChannel{}, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return ReleaseChannel{}, err
		}
		if changed != 1 {
			return ReleaseChannel{}, fmt.Errorf("%w: release channel %s", ErrOptimisticLock, channel)
		}
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       actor,
		EntityType:  "release_channel",
		EntityID:    pointer.Channel,
		Action:      "release_channel.set",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"release_id": pointer.ReleaseID, "version": pointer.Version}),
		CreatedAt:   now,
	}); err != nil {
		return ReleaseChannel{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReleaseChannel{}, err
	}
	return pointer, nil
}

const deletionRecordSelect = `
	SELECT id, entity_type, entity_id, action, state, actor, reason, created_at, completed_at, version
	FROM deletion_records`

func (s *Store) CreateDeletionRecord(ctx context.Context, request CreateDeletionRecordRequest) (DeletionRecord, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return DeletionRecord{}, err
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return DeletionRecord{}, err
	}
	entityType, err := normalizeRequired(request.EntityType, "deletion entity type")
	if err != nil {
		return DeletionRecord{}, err
	}
	entityID, err := normalizeRequired(request.EntityID, "deletion entity ID")
	if err != nil {
		return DeletionRecord{}, err
	}
	action, err := normalizeRequired(request.Action, "deletion action")
	if err != nil {
		return DeletionRecord{}, err
	}
	now := s.now().UTC()
	record := DeletionRecord{ID: id, EntityType: entityType, EntityID: entityID, Action: action, State: DeletionRequested, Actor: resolveActor(request.Actor), Reason: strings.TrimSpace(request.Reason), CreatedAt: now, Version: 1}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeletionRecord{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO deletion_records (id, entity_type, entity_id, action, state, actor, reason, created_at, completed_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
	`, record.ID, record.EntityType, record.EntityID, record.Action, record.State, record.Actor, record.Reason, record.CreatedAt, record.Version)
	if err != nil {
		if isUniqueConstraint(err) {
			return DeletionRecord{}, fmt.Errorf("%w: deletion record %s", ErrIdentityCollision, record.ID)
		}
		return DeletionRecord{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       record.Actor,
		EntityType:  "deletion_record",
		EntityID:    record.ID,
		Action:      "deletion_record.created",
		Reason:      record.Reason,
		PayloadJSON: auditPayload(map[string]any{"entity_type": record.EntityType, "entity_id": record.EntityID, "action": record.Action}),
		CreatedAt:   now,
	}); err != nil {
		return DeletionRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeletionRecord{}, err
	}
	return record, nil
}

func (s *Store) GetDeletionRecord(ctx context.Context, deletionRecordID string) (*DeletionRecord, error) {
	if !isUUIDv7(deletionRecordID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	record, err := scanDeletionRecord(s.db.QueryRowContext(ctx, deletionRecordSelect+" WHERE id = ?", deletionRecordID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func (s *Store) TransitionDeletionRecord(ctx context.Context, request TransitionDeletionRecordRequest) (DeletionRecord, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return DeletionRecord{}, err
	}
	if !isUUIDv7(request.DeletionRecordID) {
		return DeletionRecord{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return DeletionRecord{}, fmt.Errorf("expected deletion record version must be positive")
	}
	if !validDeletionRecordState(request.State) {
		return DeletionRecord{}, fmt.Errorf("invalid deletion record state %q", request.State)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeletionRecord{}, err
	}
	defer tx.Rollback()
	record, err := getDeletionRecordTx(ctx, tx, request.DeletionRecordID)
	if err != nil {
		return DeletionRecord{}, err
	}
	if record.Version != request.ExpectedVersion {
		return DeletionRecord{}, fmt.Errorf("%w: deletion record %s", ErrOptimisticLock, record.ID)
	}
	if !validDeletionRecordTransition(record.State, request.State) {
		return DeletionRecord{}, fmt.Errorf("%w: deletion record %s from %s to %s", ErrInvalidTransition, record.ID, record.State, request.State)
	}
	now := s.now().UTC()
	record.State = request.State
	record.Actor = resolveActor(request.Actor)
	if value := strings.TrimSpace(request.Reason); value != "" {
		record.Reason = value
	}
	if record.State == DeletionCompleted {
		record.CompletedAt = &now
	}
	record.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE deletion_records SET state = ?, actor = ?, reason = ?, completed_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, record.State, record.Actor, record.Reason, record.CompletedAt, record.Version, record.ID, request.ExpectedVersion)
	if err != nil {
		return DeletionRecord{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return DeletionRecord{}, err
	}
	if changed != 1 {
		return DeletionRecord{}, fmt.Errorf("%w: deletion record %s", ErrOptimisticLock, record.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       record.Actor,
		EntityType:  "deletion_record",
		EntityID:    record.ID,
		Action:      "deletion_record.transitioned",
		Reason:      record.Reason,
		PayloadJSON: auditPayload(map[string]any{"state": record.State, "version": record.Version}),
		CreatedAt:   now,
	}); err != nil {
		return DeletionRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeletionRecord{}, err
	}
	return record, nil
}

// QueryPurgeDependencies is read-only. Application services decide whether to
// create a deletion record or perform filesystem cleanup after examining this
// report; permanent releases intentionally remain blockers forever.
func (s *Store) QueryPurgeDependencies(ctx context.Context, query PurgeDependencyQuery) (PurgeDependencyReport, error) {
	taskID, err := s.resolvePurgeTaskID(ctx, query)
	if err != nil {
		return PurgeDependencyReport{}, err
	}
	return queryPurgeDependenciesForTask(ctx, s.db, taskID)
}

// purgeDependencyQuerier is implemented by both *sql.DB and *sql.Tx. Keeping
// the dependency query usable inside a write transaction closes the race
// between a successful preflight and irreversible filesystem cleanup.
type purgeDependencyQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryPurgeDependenciesForTask(ctx context.Context, db purgeDependencyQuerier, taskID string) (PurgeDependencyReport, error) {
	report := PurgeDependencyReport{TaskID: taskID}
	var err error
	if report.ActiveWorkspaceIDs, err = queryIDs(ctx, db, `SELECT id FROM workspaces_v2 WHERE task_id = ? AND state = 'active' ORDER BY id`, taskID); err != nil {
		return PurgeDependencyReport{}, err
	}
	if report.ActiveRunIDs, err = queryIDs(ctx, db, `
		SELECT id FROM workflow_runs
		WHERE task_id = ? AND status NOT IN ('succeeded', 'failed_recoverable', 'failed_terminal', 'canceled', 'interrupted')
		ORDER BY id
	`, taskID); err != nil {
		return PurgeDependencyReport{}, err
	}
	if report.ActiveJobIDs, err = queryIDs(ctx, db, `
		SELECT DISTINCT j.id
		FROM jobs j
		LEFT JOIN workflow_runs direct_run ON direct_run.id = j.run_id
		LEFT JOIN stage_attempts stage ON stage.id = j.stage_attempt_id
		LEFT JOIN workflow_runs stage_run ON stage_run.id = stage.run_id
		LEFT JOIN workspaces_v2 workspace ON workspace.id = j.entity_id AND j.entity_type = 'workspace'
		WHERE j.state NOT IN ('canceled', 'succeeded', 'failed', 'interrupted')
		  AND (
			(j.entity_type = 'task' AND j.entity_id = ?)
			OR (j.entity_type = 'task_revision' AND j.entity_id IN (SELECT id FROM task_revisions WHERE task_id = ?))
			OR (j.entity_type = 'workflow_run' AND j.entity_id IN (SELECT id FROM workflow_runs WHERE task_id = ?))
			OR direct_run.task_id = ? OR stage_run.task_id = ? OR workspace.task_id = ?
		  )
		ORDER BY j.id
	`, taskID, taskID, taskID, taskID, taskID, taskID); err != nil {
		return PurgeDependencyReport{}, err
	}
	if report.ActiveLeaseIDs, err = queryIDs(ctx, db, `
		SELECT id FROM leases
		WHERE state = 'active' AND (
			(resource_type = 'task' AND resource_id = ?)
			OR (resource_type = 'task_revision' AND resource_id IN (SELECT id FROM task_revisions WHERE task_id = ?))
			OR (resource_type = 'workflow_run' AND resource_id IN (SELECT id FROM workflow_runs WHERE task_id = ?))
			OR (resource_type = 'workspace' AND resource_id IN (SELECT id FROM workspaces_v2 WHERE task_id = ?))
		)
		ORDER BY id
	`, taskID, taskID, taskID, taskID); err != nil {
		return PurgeDependencyReport{}, err
	}
	if report.PendingOutboxIDs, err = queryIDs(ctx, db, `
		SELECT DISTINCT o.id
		FROM outbox_events o
		LEFT JOIN workspaces_v2 workspace ON workspace.id = o.entity_id AND o.entity_type = 'workspace'
		WHERE o.state IN ('pending', 'leased') AND (
			(o.entity_type = 'task' AND o.entity_id = ?)
			OR (o.entity_type = 'task_revision' AND o.entity_id IN (SELECT id FROM task_revisions WHERE task_id = ?))
			OR (o.entity_type = 'workflow_run' AND o.entity_id IN (SELECT id FROM workflow_runs WHERE task_id = ?))
			OR workspace.task_id = ?
		)
		ORDER BY o.id
	`, taskID, taskID, taskID, taskID); err != nil {
		return PurgeDependencyReport{}, err
	}
	if report.ArtifactManifestIDs, err = queryIDs(ctx, db, `
		SELECT id FROM artifact_manifests_v4
		WHERE subject_revision_id IN (SELECT id FROM task_revisions WHERE task_id = ?)
		ORDER BY id
	`, taskID); err != nil {
		return PurgeDependencyReport{}, err
	}
	if report.ArtifactRefIDs, err = queryIDs(ctx, db, `
		SELECT id FROM artifact_refs_v4
		WHERE subject_revision_id IN (SELECT id FROM task_revisions WHERE task_id = ?)
		ORDER BY id
	`, taskID); err != nil {
		return PurgeDependencyReport{}, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id, package_ref, evidence_ref FROM releases WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return PurgeDependencyReport{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var releaseID, packageRef, evidenceRef string
		if err := rows.Scan(&releaseID, &packageRef, &evidenceRef); err != nil {
			return PurgeDependencyReport{}, err
		}
		report.ReleaseIDs = append(report.ReleaseIDs, releaseID)
		report.PackageRefs = append(report.PackageRefs, packageRef)
		report.EvidenceRefs = append(report.EvidenceRefs, evidenceRef)
	}
	if err := rows.Err(); err != nil {
		return PurgeDependencyReport{}, err
	}
	return report, nil
}

func (s *Store) resolvePurgeTaskID(ctx context.Context, query PurgeDependencyQuery) (string, error) {
	if !isUUIDv7(query.EntityID) {
		return "", ErrInvalidUUIDv7Identity
	}
	switch strings.TrimSpace(query.EntityType) {
	case "task":
		task, err := s.GetTaskV2(ctx, query.EntityID)
		if err != nil || task == nil {
			if err != nil {
				return "", err
			}
			return "", fmt.Errorf("%w: task %s", ErrNotFound, query.EntityID)
		}
		return query.EntityID, nil
	case "task_revision":
		revision, err := s.GetTaskRevision(ctx, query.EntityID)
		if err != nil {
			return "", err
		}
		if revision == nil {
			return "", fmt.Errorf("%w: revision %s", ErrNotFound, query.EntityID)
		}
		return revision.TaskID, nil
	case "workspace":
		workspace, err := s.GetManagedWorkspace(ctx, query.EntityID)
		if err != nil {
			return "", err
		}
		if workspace == nil {
			return "", fmt.Errorf("%w: workspace %s", ErrNotFound, query.EntityID)
		}
		return workspace.TaskID, nil
	case "workflow_run":
		run, err := s.GetWorkflowRun(ctx, query.EntityID)
		if err != nil {
			return "", err
		}
		if run == nil {
			return "", fmt.Errorf("%w: workflow run %s", ErrNotFound, query.EntityID)
		}
		return run.TaskID, nil
	default:
		return "", fmt.Errorf("unsupported purge dependency entity type %q", query.EntityType)
	}
}

func getOutboxEventTx(ctx context.Context, tx *sql.Tx, eventID string) (OutboxEvent, error) {
	event, err := scanOutboxEvent(tx.QueryRowContext(ctx, outboxEventSelect+" WHERE id = ?", eventID))
	if err == sql.ErrNoRows {
		return OutboxEvent{}, fmt.Errorf("%w: outbox event %s", ErrNotFound, eventID)
	}
	return event, err
}

func getOutboxByIdempotencyTx(ctx context.Context, tx *sql.Tx, topic, entityType, entityID, key string) (OutboxEvent, error) {
	event, err := scanOutboxEvent(tx.QueryRowContext(ctx, outboxEventSelect+" WHERE topic = ? AND entity_type = ? AND entity_id = ? AND idempotency_key = ?", topic, entityType, entityID, key))
	if err == sql.ErrNoRows {
		return OutboxEvent{}, fmt.Errorf("%w: outbox idempotency key %s", ErrNotFound, key)
	}
	return event, err
}

func getLocalPackageReleaseTx(ctx context.Context, tx *sql.Tx, releaseID string) (LocalPackageRelease, error) {
	release, err := scanLocalPackageRelease(tx.QueryRowContext(ctx, localPackageReleaseSelect+" WHERE id = ?", releaseID))
	if err == sql.ErrNoRows {
		return LocalPackageRelease{}, fmt.Errorf("%w: release %s", ErrNotFound, releaseID)
	}
	return release, err
}

func getReleaseChannelTx(ctx context.Context, tx *sql.Tx, channel string) (ReleaseChannel, error) {
	pointer, err := scanReleaseChannel(tx.QueryRowContext(ctx, `SELECT channel, release_id, updated_at, updated_by, version FROM release_channels WHERE channel = ?`, channel))
	if err == sql.ErrNoRows {
		return ReleaseChannel{}, fmt.Errorf("%w: release channel %s", ErrNotFound, channel)
	}
	return pointer, err
}

func getDeletionRecordTx(ctx context.Context, tx *sql.Tx, recordID string) (DeletionRecord, error) {
	record, err := scanDeletionRecord(tx.QueryRowContext(ctx, deletionRecordSelect+" WHERE id = ?", recordID))
	if err == sql.ErrNoRows {
		return DeletionRecord{}, fmt.Errorf("%w: deletion record %s", ErrNotFound, recordID)
	}
	return record, err
}

func scanOutboxEvent(scanner rowScanner) (OutboxEvent, error) {
	var event OutboxEvent
	var leaseExpiresAt, publishedAt sql.NullTime
	if err := scanner.Scan(
		&event.ID, &event.Topic, &event.EntityType, &event.EntityID, &event.PayloadJSON,
		&event.IdempotencyKey, &event.State, &event.AvailableAt, &event.LeaseOwner,
		&leaseExpiresAt, &event.LeaseFencingToken, &event.DeliveryCount, &event.LastError,
		&event.CreatedAt, &event.UpdatedAt, &publishedAt, &event.Version,
	); err != nil {
		return OutboxEvent{}, err
	}
	event.AvailableAt = event.AvailableAt.UTC()
	event.CreatedAt = event.CreatedAt.UTC()
	event.UpdatedAt = event.UpdatedAt.UTC()
	event.LeaseExpiresAt = nullableTimePtr(leaseExpiresAt)
	event.PublishedAt = nullableTimePtr(publishedAt)
	return event, nil
}

func scanLocalPackageRelease(scanner rowScanner) (LocalPackageRelease, error) {
	var release LocalPackageRelease
	var withdrawnAt sql.NullTime
	if err := scanner.Scan(&release.ID, &release.ReleaseVersion, &release.RevisionID, &release.TaskID, &release.TaskDigest, &release.PackageRef, &release.EvidenceRef, &release.PublishedAt, &withdrawnAt, &release.WithdrawnBy, &release.CreatedBy, &release.RecordVersion); err != nil {
		return LocalPackageRelease{}, err
	}
	release.PublishedAt = release.PublishedAt.UTC()
	release.WithdrawnAt = nullableTimePtr(withdrawnAt)
	return release, nil
}

func scanReleaseChannel(scanner rowScanner) (ReleaseChannel, error) {
	var pointer ReleaseChannel
	if err := scanner.Scan(&pointer.Channel, &pointer.ReleaseID, &pointer.UpdatedAt, &pointer.UpdatedBy, &pointer.Version); err != nil {
		return ReleaseChannel{}, err
	}
	pointer.UpdatedAt = pointer.UpdatedAt.UTC()
	return pointer, nil
}

func scanDeletionRecord(scanner rowScanner) (DeletionRecord, error) {
	var record DeletionRecord
	var completedAt sql.NullTime
	if err := scanner.Scan(&record.ID, &record.EntityType, &record.EntityID, &record.Action, &record.State, &record.Actor, &record.Reason, &record.CreatedAt, &completedAt, &record.Version); err != nil {
		return DeletionRecord{}, err
	}
	record.CreatedAt = record.CreatedAt.UTC()
	record.CompletedAt = nullableTimePtr(completedAt)
	return record, nil
}

func queryIDs(ctx context.Context, db purgeDependencyQuerier, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func isNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}

func validDeletionRecordState(state DeletionRecordState) bool {
	switch state {
	case DeletionRequested, DeletionBlocked, DeletionCompleted, DeletionCanceled:
		return true
	default:
		return false
	}
}

func validDeletionRecordTransition(from, to DeletionRecordState) bool {
	if from == to || from == DeletionCompleted || from == DeletionCanceled {
		return false
	}
	switch from {
	case DeletionRequested:
		return to == DeletionBlocked || to == DeletionCompleted || to == DeletionCanceled
	case DeletionBlocked:
		return to == DeletionRequested || to == DeletionCompleted || to == DeletionCanceled
	default:
		return false
	}
}
