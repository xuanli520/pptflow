package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const authoringPhase1HandoffSelect = `
	SELECT id, authoring_run_id, authoring_session_id, authoring_source_id,
	       handoff_artifact_id, handoff_fingerprint, task_id, revision_id,
	       task_digest, child_run_id, idempotency_key, created_by, created_at
	FROM authoring_phase1_handoffs_v2`

type preparedAuthoringPhase1Handoff struct {
	requestedID        string
	authoringRunID     string
	authoringSessionID string
	authoringSourceID  string
	handoffArtifactID  string
	handoffFingerprint string
	taskID             string
	revisionID         string
	taskDigest         string
	childRunID         string
	idempotencyKey     string
	actor              string
	reason             string
}

// GetAuthoringPhase1HandoffForAuthoringRun returns the one immutable bridge
// prepared for a materialized Standard authoring Run. It intentionally does
// not infer a child from workflow_runs.parent_run_id: that relation is not
// unique by itself and is not proof of a persisted handoff artifact.
func (s *Store) GetAuthoringPhase1HandoffForAuthoringRun(ctx context.Context, authoringRunID string) (*AuthoringPhase1Handoff, error) {
	if _, err := requireV4ID(authoringRunID, "authoring workflow run ID"); err != nil {
		return nil, err
	}
	handoff, err := scanAuthoringPhase1Handoff(s.db.QueryRowContext(ctx, authoringPhase1HandoffSelect+" WHERE authoring_run_id = ?", authoringRunID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &handoff, nil
}

// GetAuthoringPhase1Handoff reads one handoff by its globally unique entity
// identity. It is used by CreateWorkflowRun to prove that the only permitted
// authoring-session parent was prepared by the sealed handoff path.
func (s *Store) GetAuthoringPhase1Handoff(ctx context.Context, handoffID string) (*AuthoringPhase1Handoff, error) {
	if _, err := requireV4ID(handoffID, "authoring Phase-1 handoff ID"); err != nil {
		return nil, err
	}
	handoff, err := scanAuthoringPhase1Handoff(s.db.QueryRowContext(ctx, authoringPhase1HandoffSelect+" WHERE id = ?", handoffID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &handoff, nil
}

// PrepareAuthoringPhase1Handoff creates the one durable child identity before
// any child Run filesystem or workflow row exists. A concurrent/replayed
// caller receives the original record if all authoritative lineage facts
// match; supplying a different ChildRunID is intentionally ignored on that
// replay so the first allocated identity remains the only one usable.
func (s *Store) PrepareAuthoringPhase1Handoff(ctx context.Context, request PrepareAuthoringPhase1HandoffRequest) (AuthoringPhase1Handoff, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return AuthoringPhase1Handoff{}, err
	}
	prepared, err := prepareAuthoringPhase1Handoff(request)
	if err != nil {
		return AuthoringPhase1Handoff{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuthoringPhase1Handoff{}, err
	}
	defer tx.Rollback()
	if existing, existingErr := getAuthoringPhase1HandoffByAuthoringRunTx(ctx, tx, prepared.authoringRunID); existingErr == nil {
		if !sameAuthoringPhase1Handoff(existing, prepared, false) {
			return AuthoringPhase1Handoff{}, fmt.Errorf("%w: Standard authoring Phase-1 handoff for Run %s", ErrIdempotencyConflict, prepared.authoringRunID)
		}
		if err := tx.Commit(); err != nil {
			return AuthoringPhase1Handoff{}, err
		}
		return existing, nil
	} else if !isNotFound(existingErr) {
		return AuthoringPhase1Handoff{}, existingErr
	}
	if existing, existingErr := getAuthoringPhase1HandoffByArtifactTx(ctx, tx, prepared.handoffArtifactID); existingErr == nil {
		if !sameAuthoringPhase1Handoff(existing, prepared, false) {
			return AuthoringPhase1Handoff{}, fmt.Errorf("%w: Standard authoring Phase-1 handoff artifact %s", ErrIdempotencyConflict, prepared.handoffArtifactID)
		}
		if err := tx.Commit(); err != nil {
			return AuthoringPhase1Handoff{}, err
		}
		return existing, nil
	} else if !isNotFound(existingErr) {
		return AuthoringPhase1Handoff{}, existingErr
	}
	if existing, existingErr := getAuthoringPhase1HandoffByKeyTx(ctx, tx, prepared.idempotencyKey); existingErr == nil {
		if !sameAuthoringPhase1Handoff(existing, prepared, true) {
			return AuthoringPhase1Handoff{}, fmt.Errorf("%w: Standard authoring Phase-1 handoff key %s", ErrIdempotencyConflict, prepared.idempotencyKey)
		}
		if err := tx.Commit(); err != nil {
			return AuthoringPhase1Handoff{}, err
		}
		return existing, nil
	} else if !isNotFound(existingErr) {
		return AuthoringPhase1Handoff{}, existingErr
	}
	id, err := s.newV2ID(prepared.requestedID)
	if err != nil {
		return AuthoringPhase1Handoff{}, err
	}
	now := s.now().UTC()
	handoff := AuthoringPhase1Handoff{
		ID: id, AuthoringRunID: prepared.authoringRunID, AuthoringSessionID: prepared.authoringSessionID,
		AuthoringSourceID: prepared.authoringSourceID, HandoffArtifactID: prepared.handoffArtifactID,
		HandoffFingerprint: prepared.handoffFingerprint, TaskID: prepared.taskID, RevisionID: prepared.revisionID,
		TaskDigest: prepared.taskDigest, ChildRunID: prepared.childRunID, IdempotencyKey: prepared.idempotencyKey,
		CreatedBy: prepared.actor, CreatedAt: now,
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO authoring_phase1_handoffs_v2 (
			id, authoring_run_id, authoring_session_id, authoring_source_id,
			handoff_artifact_id, handoff_fingerprint, task_id, revision_id,
			task_digest, child_run_id, idempotency_key, created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, handoff.ID, handoff.AuthoringRunID, handoff.AuthoringSessionID, handoff.AuthoringSourceID,
		handoff.HandoffArtifactID, handoff.HandoffFingerprint, handoff.TaskID, handoff.RevisionID,
		handoff.TaskDigest, handoff.ChildRunID, handoff.IdempotencyKey, handoff.CreatedBy, handoff.CreatedAt)
	if err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return AuthoringPhase1Handoff{}, fmt.Errorf("%w: Standard authoring Phase-1 handoff %s", ErrIdentityCollision, handoff.ID)
		}
		if isAuthoringPhase1HandoffLineageConstraint(err) {
			return AuthoringPhase1Handoff{}, fmt.Errorf("%w: %v", ErrImmutable, err)
		}
		return AuthoringPhase1Handoff{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: handoff.CreatedBy, EntityType: "authoring_phase1_handoff", EntityID: handoff.ID,
		Action: "authoring_phase1_handoff.prepared", Reason: prepared.reason,
		PayloadJSON:  auditPayload(map[string]any{"authoring_run_id": handoff.AuthoringRunID, "handoff_artifact_id": handoff.HandoffArtifactID, "task_id": handoff.TaskID, "revision_id": handoff.RevisionID, "child_run_id": handoff.ChildRunID}),
		OperationKey: handoff.IdempotencyKey, CreatedAt: now,
	}); err != nil {
		return AuthoringPhase1Handoff{}, err
	}
	if err := tx.Commit(); err != nil {
		return AuthoringPhase1Handoff{}, err
	}
	return handoff, nil
}

func isAuthoringPhase1HandoffLineageConstraint(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "authoring phase-1 handoff does not match persisted materialization lineage")
}

func prepareAuthoringPhase1Handoff(request PrepareAuthoringPhase1HandoffRequest) (preparedAuthoringPhase1Handoff, error) {
	requestedID := strings.TrimSpace(request.ID)
	if requestedID != "" && !isUUIDv7(requestedID) {
		return preparedAuthoringPhase1Handoff{}, ErrInvalidUUIDv7Identity
	}
	values := []struct {
		label string
		value string
	}{
		{"authoring Run", request.AuthoringRunID}, {"authoring session", request.AuthoringSessionID},
		{"authoring source", request.AuthoringSourceID}, {"handoff artifact", request.HandoffArtifactID},
		{"task", request.TaskID}, {"revision", request.RevisionID}, {"child Run", request.ChildRunID},
	}
	for _, value := range values {
		if _, err := requireV4ID(value.value, value.label+" ID"); err != nil {
			return preparedAuthoringPhase1Handoff{}, err
		}
	}
	fingerprint, err := normalizeAuthoringSHA256(request.HandoffFingerprint, "authoring Phase-1 handoff fingerprint")
	if err != nil {
		return preparedAuthoringPhase1Handoff{}, err
	}
	taskDigest, err := normalizeRequired(request.TaskDigest, "authoring Phase-1 task digest")
	if err != nil {
		return preparedAuthoringPhase1Handoff{}, err
	}
	if err := ValidateTaskDigestV2(taskDigest); err != nil {
		return preparedAuthoringPhase1Handoff{}, fmt.Errorf("authoring Phase-1 task digest: %w", err)
	}
	key, err := normalizeRequired(request.IdempotencyKey, "authoring Phase-1 handoff idempotency key")
	if err != nil {
		return preparedAuthoringPhase1Handoff{}, err
	}
	return preparedAuthoringPhase1Handoff{
		requestedID: requestedID, authoringRunID: strings.TrimSpace(request.AuthoringRunID), authoringSessionID: strings.TrimSpace(request.AuthoringSessionID),
		authoringSourceID: strings.TrimSpace(request.AuthoringSourceID), handoffArtifactID: strings.TrimSpace(request.HandoffArtifactID),
		handoffFingerprint: fingerprint, taskID: strings.TrimSpace(request.TaskID), revisionID: strings.TrimSpace(request.RevisionID),
		taskDigest: taskDigest, childRunID: strings.TrimSpace(request.ChildRunID), idempotencyKey: key,
		actor: resolveActor(request.Actor), reason: strings.TrimSpace(request.Reason),
	}, nil
}

func getAuthoringPhase1HandoffByAuthoringRunTx(ctx context.Context, tx *sql.Tx, runID string) (AuthoringPhase1Handoff, error) {
	return getAuthoringPhase1HandoffTx(ctx, tx, "authoring_run_id", runID)
}

func getAuthoringPhase1HandoffByArtifactTx(ctx context.Context, tx *sql.Tx, artifactID string) (AuthoringPhase1Handoff, error) {
	return getAuthoringPhase1HandoffTx(ctx, tx, "handoff_artifact_id", artifactID)
}

func getAuthoringPhase1HandoffByKeyTx(ctx context.Context, tx *sql.Tx, key string) (AuthoringPhase1Handoff, error) {
	return getAuthoringPhase1HandoffTx(ctx, tx, "idempotency_key", key)
}

func getAuthoringPhase1HandoffTx(ctx context.Context, tx *sql.Tx, column, value string) (AuthoringPhase1Handoff, error) {
	// column is selected only by fixed Store helpers above.
	handoff, err := scanAuthoringPhase1Handoff(tx.QueryRowContext(ctx, authoringPhase1HandoffSelect+" WHERE "+column+" = ?", value))
	if err == sql.ErrNoRows {
		return AuthoringPhase1Handoff{}, fmt.Errorf("%w: authoring Phase-1 handoff %s", ErrNotFound, value)
	}
	return handoff, err
}

func scanAuthoringPhase1Handoff(scanner rowScanner) (AuthoringPhase1Handoff, error) {
	var handoff AuthoringPhase1Handoff
	if err := scanner.Scan(
		&handoff.ID, &handoff.AuthoringRunID, &handoff.AuthoringSessionID, &handoff.AuthoringSourceID,
		&handoff.HandoffArtifactID, &handoff.HandoffFingerprint, &handoff.TaskID, &handoff.RevisionID,
		&handoff.TaskDigest, &handoff.ChildRunID, &handoff.IdempotencyKey, &handoff.CreatedBy, &handoff.CreatedAt,
	); err != nil {
		return AuthoringPhase1Handoff{}, err
	}
	for _, value := range []string{handoff.ID, handoff.AuthoringRunID, handoff.AuthoringSessionID, handoff.AuthoringSourceID, handoff.HandoffArtifactID, handoff.TaskID, handoff.RevisionID, handoff.ChildRunID} {
		if !isUUIDv7(value) {
			return AuthoringPhase1Handoff{}, fmt.Errorf("invalid persisted authoring Phase-1 handoff identity")
		}
	}
	if _, err := normalizeAuthoringSHA256(handoff.HandoffFingerprint, "persisted authoring Phase-1 handoff fingerprint"); err != nil {
		return AuthoringPhase1Handoff{}, err
	}
	if err := ValidateTaskDigestV2(handoff.TaskDigest); err != nil {
		return AuthoringPhase1Handoff{}, fmt.Errorf("persisted authoring Phase-1 task digest: %w", err)
	}
	if strings.TrimSpace(handoff.IdempotencyKey) == "" || strings.TrimSpace(handoff.CreatedBy) == "" {
		return AuthoringPhase1Handoff{}, fmt.Errorf("invalid persisted authoring Phase-1 handoff")
	}
	handoff.CreatedAt = handoff.CreatedAt.UTC()
	return handoff, nil
}

func sameAuthoringPhase1Handoff(existing AuthoringPhase1Handoff, request preparedAuthoringPhase1Handoff, includeChildRunID bool) bool {
	if existing.AuthoringRunID != request.authoringRunID || existing.AuthoringSessionID != request.authoringSessionID || existing.AuthoringSourceID != request.authoringSourceID ||
		existing.HandoffArtifactID != request.handoffArtifactID || existing.HandoffFingerprint != request.handoffFingerprint || existing.TaskID != request.taskID ||
		existing.RevisionID != request.revisionID || existing.TaskDigest != request.taskDigest || existing.IdempotencyKey != request.idempotencyKey {
		return false
	}
	return !includeChildRunID || existing.ChildRunID == request.childRunID
}
