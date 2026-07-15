package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const authoringReviewRequestV22Select = `
	SELECT id, run_id, authoring_session_id, authoring_source_id,
	       source_snapshot_digest, definition_hash, evidence_manifest_digest,
	       request_fingerprint, idempotency_key, created_by, created_at
	FROM authoring_review_requests_v22`

const authoringReviewGateBindingV22Select = `
	SELECT id, review_request_id, run_id, authoring_session_id, authoring_source_id,
	       source_snapshot_digest, definition_hash, stage_attempt_id, stage_key,
	       node_attempt_id, node_generation, node_attempt_ordinal, review_kind,
	       input_bindings_json, input_fingerprint, evidence_manifest_digest,
	       binding_fingerprint, created_at
	FROM authoring_review_gate_bindings_v22`

const authoringReviewDecisionV22Select = `
	SELECT id, review_request_id, binding_id, action, decision_fingerprint,
	       idempotency_key, actor, reason, created_at
	FROM authoring_review_decisions_v22`

const authoringReviewGateResolutionV22Select = `
	SELECT id, review_request_id, binding_id, decision_id, verdict,
	       artifact_manifest_id, resolution_evidence_digest, resolution_payload_json,
	       resolution_fingerprint, idempotency_key, created_by, created_at
	FROM authoring_review_gate_resolutions_v22`

// GetAuthoringReviewGateBindingByRequest returns the immutable binding for an
// AuthoringSession review request. It never falls back to task-review tables.
func (s *Store) GetAuthoringReviewGateBindingByRequest(ctx context.Context, reviewRequestID string) (*AuthoringReviewGateBinding, error) {
	if !isUUIDv7(reviewRequestID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	return getAuthoringReviewGateBindingByRequestTx(ctx, s.db, reviewRequestID)
}

// GetAuthoringReviewGateBindingByStageAttempt returns the source/session gate
// binding attached to one durable StageAttempt.
func (s *Store) GetAuthoringReviewGateBindingByStageAttempt(ctx context.Context, stageAttemptID string) (*AuthoringReviewGateBinding, error) {
	if !isUUIDv7(stageAttemptID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	return getAuthoringReviewGateBindingByStageAttemptTx(ctx, s.db, stageAttemptID)
}

// GetAuthoringReviewGateBindingByNodeAttempt returns the source/session gate
// binding attached to the gate's waiting NodeAttempt.
func (s *Store) GetAuthoringReviewGateBindingByNodeAttempt(ctx context.Context, nodeAttemptID string) (*AuthoringReviewGateBinding, error) {
	if !isUUIDv7(nodeAttemptID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	return getAuthoringReviewGateBindingByNodeAttemptTx(ctx, s.db, nodeAttemptID)
}

// GetAuthoringReviewRequest returns one immutable AuthoringSession review
// envelope by its UUIDv7 identity.
func (s *Store) GetAuthoringReviewRequest(ctx context.Context, reviewRequestID string) (*AuthoringReviewRequest, error) {
	if !isUUIDv7(reviewRequestID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	request, err := scanAuthoringReviewRequest(s.db.QueryRowContext(ctx, authoringReviewRequestV22Select+" WHERE id = ?", reviewRequestID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &request, nil
}

// ListAuthoringReviewDecisionsForRequest returns the append-only operator
// decision history for one authoring request. Schema permits one decision, but
// this list-shaped API makes the audit surface explicit to callers.
func (s *Store) ListAuthoringReviewDecisionsForRequest(ctx context.Context, reviewRequestID string) ([]AuthoringReviewDecision, error) {
	if !isUUIDv7(reviewRequestID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, authoringReviewDecisionV22Select+" WHERE review_request_id = ? ORDER BY created_at ASC, id ASC", reviewRequestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	decisions := make([]AuthoringReviewDecision, 0)
	for rows.Next() {
		decision, err := scanAuthoringReviewDecision(rows)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	return decisions, rows.Err()
}

// GetAuthoringReviewGateResolution returns the immutable completion receipt,
// if the request has reached its completed derived state.
func (s *Store) GetAuthoringReviewGateResolution(ctx context.Context, reviewRequestID string) (*AuthoringReviewGateResolution, error) {
	if !isUUIDv7(reviewRequestID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	return getAuthoringReviewGateResolutionByRequestTx(ctx, s.db, reviewRequestID)
}

// GetAuthoringReviewGateState derives the current operator-review state from
// immutable facts rather than mutating an envelope state column.
func (s *Store) GetAuthoringReviewGateState(ctx context.Context, reviewRequestID string) (AuthoringReviewGateState, error) {
	if !isUUIDv7(reviewRequestID) {
		return "", ErrInvalidUUIDv7Identity
	}
	if binding, err := getAuthoringReviewGateBindingByRequestTx(ctx, s.db, reviewRequestID); err != nil {
		return "", err
	} else if binding == nil {
		return "", fmt.Errorf("%w: authoring review request %s", ErrNotFound, reviewRequestID)
	}
	if resolution, err := getAuthoringReviewGateResolutionByRequestTx(ctx, s.db, reviewRequestID); err != nil {
		return "", err
	} else if resolution != nil {
		return AuthoringReviewGateCompleted, nil
	}
	if decision, err := getAuthoringReviewDecisionByRequestTx(ctx, s.db, reviewRequestID); err != nil {
		return "", err
	} else if decision != nil {
		return AuthoringReviewGateDecided, nil
	}
	return AuthoringReviewGateOpen, nil
}

// OpenAuthoringReviewGate atomically freezes a pre-materialization source /
// session review gate, creates its waiting node, and moves the Run and stage
// into durable review waiting state. It never creates a ReviewRequest or a
// synthetic TaskRevision.
func (s *Store) OpenAuthoringReviewGate(ctx context.Context, request OpenAuthoringReviewGateRequest) (AuthoringReviewGateOpenResult, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return AuthoringReviewGateOpenResult{}, err
	}
	prepared, err := prepareOpenAuthoringReviewGateRequest(request)
	if err != nil {
		return AuthoringReviewGateOpenResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuthoringReviewGateOpenResult{}, err
	}
	defer tx.Rollback()

	if binding, err := getAuthoringReviewGateBindingByStageAttemptTx(ctx, tx, prepared.stageAttemptID); err != nil {
		return AuthoringReviewGateOpenResult{}, err
	} else if binding != nil {
		result, err := replayOpenedAuthoringReviewGateTx(ctx, tx, *binding, prepared)
		if err != nil {
			return AuthoringReviewGateOpenResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return AuthoringReviewGateOpenResult{}, err
		}
		return result, nil
	}
	if existing, err := getAuthoringReviewRequestByKeyTx(ctx, tx, prepared.idempotencyKey); err != nil {
		return AuthoringReviewGateOpenResult{}, err
	} else if existing != nil {
		binding, err := getAuthoringReviewGateBindingByRequestTx(ctx, tx, existing.ID)
		if err != nil {
			return AuthoringReviewGateOpenResult{}, err
		}
		if binding == nil {
			return AuthoringReviewGateOpenResult{}, fmt.Errorf("%w: authoring review request %s is missing its immutable binding", ErrImmutable, existing.ID)
		}
		result, err := replayOpenedAuthoringReviewGateTx(ctx, tx, *binding, prepared)
		if err != nil {
			return AuthoringReviewGateOpenResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return AuthoringReviewGateOpenResult{}, err
		}
		return result, nil
	}
	if prepared.reviewRequestID != "" {
		if binding, err := getAuthoringReviewGateBindingByRequestTx(ctx, tx, prepared.reviewRequestID); err != nil {
			return AuthoringReviewGateOpenResult{}, err
		} else if binding != nil {
			return AuthoringReviewGateOpenResult{}, fmt.Errorf("%w: authoring review request %s is already bound to stage attempt %s", ErrIdempotencyConflict, prepared.reviewRequestID, binding.StageAttemptID)
		}
	}

	state, err := loadAuthoringReviewOpenStateTx(ctx, tx, prepared)
	if err != nil {
		return AuthoringReviewGateOpenResult{}, err
	}
	reviewRequestID := prepared.reviewRequestID
	if reviewRequestID == "" {
		reviewRequestID, err = s.newV2ID("")
		if err != nil {
			return AuthoringReviewGateOpenResult{}, err
		}
	}
	bindingID := prepared.bindingID
	if bindingID == "" {
		bindingID, err = s.newV2ID("")
		if err != nil {
			return AuthoringReviewGateOpenResult{}, err
		}
	}
	nodeAttemptID := prepared.nodeAttemptID
	if nodeAttemptID == "" {
		nodeAttemptID, err = s.newV2ID("")
		if err != nil {
			return AuthoringReviewGateOpenResult{}, err
		}
	}
	now := s.now().UTC()
	actor := resolveActor(prepared.actor)
	review := AuthoringReviewRequest{
		ID: reviewRequestID, RunID: state.Run.ID, AuthoringSessionID: state.Session.ID,
		AuthoringSourceID: state.Source.ID, SourceSnapshotDigest: state.Source.SnapshotContentDigest,
		DefinitionHash: state.Run.DefinitionHash, EvidenceManifestDigest: prepared.evidenceManifestDigest,
		RequestFingerprint: prepared.requestFingerprint, IdempotencyKey: prepared.idempotencyKey,
		CreatedBy: actor, CreatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO authoring_review_requests_v22 (
			id, run_id, authoring_session_id, authoring_source_id,
			source_snapshot_digest, definition_hash, evidence_manifest_digest,
			request_fingerprint, idempotency_key, created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, review.ID, review.RunID, review.AuthoringSessionID, review.AuthoringSourceID,
		review.SourceSnapshotDigest, review.DefinitionHash, review.EvidenceManifestDigest,
		review.RequestFingerprint, review.IdempotencyKey, review.CreatedBy, review.CreatedAt); err != nil {
		if isGlobalIdentityCollision(err) {
			return AuthoringReviewGateOpenResult{}, fmt.Errorf("%w: authoring review request %s", ErrIdentityCollision, review.ID)
		}
		if isUniqueConstraint(err) {
			return AuthoringReviewGateOpenResult{}, fmt.Errorf("%w: authoring review request %s", ErrIdempotencyConflict, review.ID)
		}
		return AuthoringReviewGateOpenResult{}, err
	}
	node := NodeAttempt{
		ID: nodeAttemptID, StageAttemptID: state.Stage.ID, NodeID: state.Stage.StageKey,
		Generation: prepared.nodeGeneration, Attempt: prepared.nodeAttemptOrdinal,
		Status: NodeAttemptWaiting, IdempotencyKey: authoringReviewGateNodeAttemptKey(state.Stage.ID),
		CreatedAt: now, StartedAt: &now, Version: 1,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO node_attempts (
			id, stage_attempt_id, node_id, generation, attempt, status, idempotency_key,
			started_at, finished_at, error_text, created_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, '', ?, ?)
	`, node.ID, node.StageAttemptID, node.NodeID, node.Generation, node.Attempt, node.Status,
		node.IdempotencyKey, node.StartedAt, node.CreatedAt, node.Version); err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return AuthoringReviewGateOpenResult{}, fmt.Errorf("%w: authoring review node attempt %s", ErrIdentityCollision, node.ID)
		}
		return AuthoringReviewGateOpenResult{}, err
	}
	binding := AuthoringReviewGateBinding{
		ID: bindingID, ReviewRequestID: review.ID, RunID: state.Run.ID,
		AuthoringSessionID: state.Session.ID, AuthoringSourceID: state.Source.ID,
		SourceSnapshotDigest: state.Source.SnapshotContentDigest, DefinitionHash: state.Run.DefinitionHash,
		StageAttemptID: state.Stage.ID, StageKey: state.Stage.StageKey, NodeAttemptID: node.ID,
		NodeGeneration: node.Generation, NodeAttemptOrdinal: node.Attempt, ReviewKind: prepared.reviewKind,
		InputBindingsJSON: prepared.inputBindingsJSON, InputFingerprint: state.Stage.InputFingerprint,
		EvidenceManifestDigest: review.EvidenceManifestDigest,
		BindingFingerprint:     prepared.bindingFingerprint, CreatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO authoring_review_gate_bindings_v22 (
			id, review_request_id, run_id, authoring_session_id, authoring_source_id,
			source_snapshot_digest, definition_hash, stage_attempt_id, stage_key,
			node_attempt_id, node_generation, node_attempt_ordinal, review_kind,
			input_bindings_json, input_fingerprint, evidence_manifest_digest,
			binding_fingerprint, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, binding.ID, binding.ReviewRequestID, binding.RunID, binding.AuthoringSessionID, binding.AuthoringSourceID,
		binding.SourceSnapshotDigest, binding.DefinitionHash, binding.StageAttemptID, binding.StageKey,
		binding.NodeAttemptID, binding.NodeGeneration, binding.NodeAttemptOrdinal, binding.ReviewKind,
		binding.InputBindingsJSON, binding.InputFingerprint, binding.EvidenceManifestDigest,
		binding.BindingFingerprint, binding.CreatedAt); err != nil {
		if isGlobalIdentityCollision(err) {
			return AuthoringReviewGateOpenResult{}, fmt.Errorf("%w: authoring review gate binding %s", ErrIdentityCollision, binding.ID)
		}
		if isUniqueConstraint(err) {
			return AuthoringReviewGateOpenResult{}, fmt.Errorf("%w: authoring review gate binding %s", ErrIdempotencyConflict, binding.ID)
		}
		return AuthoringReviewGateOpenResult{}, err
	}
	state.Stage.ExecutionStatus = StageExecutionWaiting
	if state.Stage.StartedAt == nil {
		state.Stage.StartedAt = &now
	}
	state.Stage.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE stage_attempts SET execution_status = ?, started_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, state.Stage.ExecutionStatus, state.Stage.StartedAt, state.Stage.Version, state.Stage.ID, prepared.expectedStageAttemptVersion)
	if err != nil {
		return AuthoringReviewGateOpenResult{}, err
	}
	if err := requireOneRow(result, "stage attempt", state.Stage.ID); err != nil {
		return AuthoringReviewGateOpenResult{}, err
	}
	state.Run.Status = WorkflowRunWaitingReview
	if state.Run.StartedAt == nil {
		state.Run.StartedAt = &now
	}
	state.Run.Version++
	result, err = tx.ExecContext(ctx, `
		UPDATE workflow_runs SET status = ?, started_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, state.Run.Status, state.Run.StartedAt, state.Run.Version, state.Run.ID, prepared.expectedRunVersion)
	if err != nil {
		return AuthoringReviewGateOpenResult{}, err
	}
	if err := requireOneRow(result, "workflow run", state.Run.ID); err != nil {
		return AuthoringReviewGateOpenResult{}, err
	}
	if err := appendAuthoringReviewGateOpenAudits(ctx, s, tx, review, binding, node, state.Stage, state.Run, prepared.actor, prepared.reason, now); err != nil {
		return AuthoringReviewGateOpenResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return AuthoringReviewGateOpenResult{}, err
	}
	return AuthoringReviewGateOpenResult{Request: review, Binding: binding, Run: state.Run, StageAttempt: state.Stage, NodeAttempt: node}, nil
}

// DecideAuthoringReviewGate appends one operator decision to an immutable
// AuthoringSession gate and atomically queues its local resolution job.
func (s *Store) DecideAuthoringReviewGate(ctx context.Context, request DecideAuthoringReviewGateRequest) (AuthoringReviewGateDecisionResult, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return AuthoringReviewGateDecisionResult{}, err
	}
	prepared, err := prepareDecideAuthoringReviewGateRequest(request)
	if err != nil {
		return AuthoringReviewGateDecisionResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuthoringReviewGateDecisionResult{}, err
	}
	defer tx.Rollback()
	state, err := loadAuthoringReviewGateStateTx(ctx, tx, prepared.reviewRequestID)
	if err != nil {
		return AuthoringReviewGateDecisionResult{}, err
	}
	if err := prepared.matchesBinding(state.Request, state.Binding); err != nil {
		return AuthoringReviewGateDecisionResult{}, err
	}
	if existing, err := getAuthoringReviewDecisionByRequestTx(ctx, tx, state.Request.ID); err != nil {
		return AuthoringReviewGateDecisionResult{}, err
	} else if existing != nil {
		result, err := replayDecidedAuthoringReviewGateTx(ctx, tx, state, *existing, prepared)
		if err != nil {
			return AuthoringReviewGateDecisionResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return AuthoringReviewGateDecisionResult{}, err
		}
		return result, nil
	}
	if state.Run.Version != prepared.expectedRunVersion {
		return AuthoringReviewGateDecisionResult{}, fmt.Errorf("%w: workflow run %s", ErrOptimisticLock, state.Run.ID)
	}
	if state.Stage.Version != prepared.expectedStageAttemptVersion {
		return AuthoringReviewGateDecisionResult{}, fmt.Errorf("%w: stage attempt %s", ErrOptimisticLock, state.Stage.ID)
	}
	if state.Run.Status != WorkflowRunWaitingReview || state.Stage.ExecutionStatus != StageExecutionWaiting || state.Node.Status != NodeAttemptWaiting {
		return AuthoringReviewGateDecisionResult{}, fmt.Errorf("%w: authoring review gate is not waiting for an operator decision", ErrInvalidTransition)
	}
	decisionID := prepared.decisionID
	if decisionID == "" {
		decisionID, err = s.newV2ID("")
		if err != nil {
			return AuthoringReviewGateDecisionResult{}, err
		}
	}
	jobID := prepared.resolutionJobID
	if jobID == "" {
		jobID, err = s.newV2ID("")
		if err != nil {
			return AuthoringReviewGateDecisionResult{}, err
		}
	}
	now := s.now().UTC()
	decision := AuthoringReviewDecision{
		ID: decisionID, ReviewRequestID: state.Request.ID, BindingID: state.Binding.ID,
		Action: prepared.action, DecisionFingerprint: prepared.decisionFingerprint,
		IdempotencyKey: prepared.idempotencyKey, Actor: resolveActor(prepared.actor),
		Reason: prepared.reason, CreatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO authoring_review_decisions_v22 (
			id, review_request_id, binding_id, action, decision_fingerprint,
			idempotency_key, actor, reason, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, decision.ID, decision.ReviewRequestID, decision.BindingID, decision.Action, decision.DecisionFingerprint,
		decision.IdempotencyKey, decision.Actor, decision.Reason, decision.CreatedAt); err != nil {
		if isGlobalIdentityCollision(err) {
			return AuthoringReviewGateDecisionResult{}, fmt.Errorf("%w: authoring review decision %s", ErrIdentityCollision, decision.ID)
		}
		if isUniqueConstraint(err) {
			return AuthoringReviewGateDecisionResult{}, fmt.Errorf("%w: authoring review decision %s", ErrIdempotencyConflict, decision.ID)
		}
		return AuthoringReviewGateDecisionResult{}, err
	}
	job := DurableJob{
		ID: jobID, CommandType: AuthoringReviewGateResolutionCommandType,
		EntityType: authoringReviewGateResolutionEntityType, EntityID: state.Binding.ID,
		RunID: state.Run.ID, StageAttemptID: state.Stage.ID, State: JobQueued,
		Priority: prepared.resolutionPriority, PayloadJSON: prepared.resolutionPayloadJSON,
		IdempotencyKey: AuthoringReviewGateResolutionJobKey(state.Binding.ID, decision.ID),
		CreatedBy:      decision.Actor, CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	jobCreated, err := insertAuthoringReviewGateResolutionJobTx(ctx, tx, job)
	if err != nil {
		return AuthoringReviewGateDecisionResult{}, err
	}
	if jobCreated {
		if err := s.appendDurableJobQueuedOutboxTx(ctx, tx, job, now); err != nil {
			return AuthoringReviewGateDecisionResult{}, err
		}
	}
	if err := appendAuthoringReviewGateDecisionAudits(ctx, s, tx, state.Request, state.Binding, decision, job, prepared.actor, prepared.reason, now); err != nil {
		return AuthoringReviewGateDecisionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return AuthoringReviewGateDecisionResult{}, err
	}
	return AuthoringReviewGateDecisionResult{Request: state.Request, Binding: state.Binding, Decision: decision, ResolutionJob: job}, nil
}

// CompleteAuthoringReviewGateResolution appends one immutable completion
// receipt and atomically completes the gate NodeAttempt and StageAttempt. The
// source/session Run stays waiting_review for the generic runtime to advance.
func (s *Store) CompleteAuthoringReviewGateResolution(ctx context.Context, request CompleteAuthoringReviewGateResolutionRequest) (AuthoringReviewGateResolutionResult, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return AuthoringReviewGateResolutionResult{}, err
	}
	prepared, err := prepareCompleteAuthoringReviewGateResolutionRequest(request)
	if err != nil {
		return AuthoringReviewGateResolutionResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuthoringReviewGateResolutionResult{}, err
	}
	defer tx.Rollback()
	state, err := loadAuthoringReviewGateStateTx(ctx, tx, prepared.reviewRequestID)
	if err != nil {
		return AuthoringReviewGateResolutionResult{}, err
	}
	if err := prepared.matchesBinding(state.Request, state.Binding); err != nil {
		return AuthoringReviewGateResolutionResult{}, err
	}
	decision, err := getAuthoringReviewDecisionTx(ctx, tx, prepared.decisionID)
	if err != nil {
		return AuthoringReviewGateResolutionResult{}, err
	}
	if decision.ReviewRequestID != state.Request.ID || decision.BindingID != state.Binding.ID {
		return AuthoringReviewGateResolutionResult{}, fmt.Errorf("%w: authoring review decision does not match immutable gate binding", ErrImmutable)
	}
	verdict, err := reviewGateVerdict(decision.Action)
	if err != nil {
		return AuthoringReviewGateResolutionResult{}, err
	}
	if existing, err := getAuthoringReviewGateResolutionByRequestTx(ctx, tx, state.Request.ID); err != nil {
		return AuthoringReviewGateResolutionResult{}, err
	} else if existing != nil {
		result, err := replayCompletedAuthoringReviewGateTx(ctx, tx, state, decision, *existing, prepared, verdict)
		if err != nil {
			return AuthoringReviewGateResolutionResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return AuthoringReviewGateResolutionResult{}, err
		}
		return result, nil
	}
	if _, err := getDurableJobByIdempotencyTx(ctx, tx, AuthoringReviewGateResolutionJobKey(state.Binding.ID, decision.ID)); err != nil {
		return AuthoringReviewGateResolutionResult{}, fmt.Errorf("authoring review gate resolution job: %w", err)
	}
	if state.Run.Version != prepared.expectedRunVersion {
		return AuthoringReviewGateResolutionResult{}, fmt.Errorf("%w: workflow run %s", ErrOptimisticLock, state.Run.ID)
	}
	if state.Stage.Version != prepared.expectedStageAttemptVersion {
		return AuthoringReviewGateResolutionResult{}, fmt.Errorf("%w: stage attempt %s", ErrOptimisticLock, state.Stage.ID)
	}
	if state.Node.Version != prepared.expectedNodeAttemptVersion {
		return AuthoringReviewGateResolutionResult{}, fmt.Errorf("%w: node attempt %s", ErrOptimisticLock, state.Node.ID)
	}
	if state.Run.Status != WorkflowRunWaitingReview || state.Stage.ExecutionStatus != StageExecutionWaiting || state.Node.Status != NodeAttemptWaiting {
		return AuthoringReviewGateResolutionResult{}, fmt.Errorf("%w: authoring review gate is not waiting for resolution", ErrInvalidTransition)
	}
	if prepared.artifactManifestID != "" {
		if err := validateAuthoringReviewGateResolutionManifestTx(ctx, tx, prepared.artifactManifestID, state.Binding); err != nil {
			return AuthoringReviewGateResolutionResult{}, err
		}
	}
	resolutionID := prepared.resolutionID
	if resolutionID == "" {
		resolutionID, err = s.newV2ID("")
		if err != nil {
			return AuthoringReviewGateResolutionResult{}, err
		}
	}
	now := s.now().UTC()
	resolution := AuthoringReviewGateResolution{
		ID: resolutionID, ReviewRequestID: state.Request.ID, BindingID: state.Binding.ID,
		DecisionID: decision.ID, Verdict: verdict, ArtifactManifestID: prepared.artifactManifestID,
		ResolutionEvidenceDigest: prepared.resolutionEvidenceDigest,
		ResolutionPayloadJSON:    prepared.resolutionPayloadJSON, ResolutionFingerprint: prepared.resolutionFingerprint,
		IdempotencyKey: prepared.idempotencyKey, CreatedBy: resolveActor(prepared.actor), CreatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO authoring_review_gate_resolutions_v22 (
			id, review_request_id, binding_id, decision_id, verdict,
			artifact_manifest_id, resolution_evidence_digest, resolution_payload_json,
			resolution_fingerprint, idempotency_key, created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, resolution.ID, resolution.ReviewRequestID, resolution.BindingID, resolution.DecisionID, resolution.Verdict,
		resolution.ArtifactManifestID, resolution.ResolutionEvidenceDigest, resolution.ResolutionPayloadJSON,
		resolution.ResolutionFingerprint, resolution.IdempotencyKey, resolution.CreatedBy, resolution.CreatedAt); err != nil {
		if isGlobalIdentityCollision(err) {
			return AuthoringReviewGateResolutionResult{}, fmt.Errorf("%w: authoring review gate resolution %s", ErrIdentityCollision, resolution.ID)
		}
		if isUniqueConstraint(err) {
			return AuthoringReviewGateResolutionResult{}, fmt.Errorf("%w: authoring review gate resolution %s", ErrIdempotencyConflict, resolution.ID)
		}
		return AuthoringReviewGateResolutionResult{}, err
	}
	state.Node.Status = NodeAttemptCompleted
	state.Node.FinishedAt = &now
	state.Node.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE node_attempts SET status = ?, finished_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, state.Node.Status, state.Node.FinishedAt, state.Node.Version, state.Node.ID, prepared.expectedNodeAttemptVersion)
	if err != nil {
		return AuthoringReviewGateResolutionResult{}, err
	}
	if err := requireOneRow(result, "node attempt", state.Node.ID); err != nil {
		return AuthoringReviewGateResolutionResult{}, err
	}
	state.Stage.ExecutionStatus = StageExecutionCompleted
	state.Stage.Verdict = verdict
	state.Stage.ArtifactManifestID = prepared.artifactManifestID
	state.Stage.FinishedAt = &now
	state.Stage.Version++
	result, err = tx.ExecContext(ctx, `
		UPDATE stage_attempts SET execution_status = ?, verdict = ?, artifact_manifest_id = ?, finished_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, state.Stage.ExecutionStatus, state.Stage.Verdict, state.Stage.ArtifactManifestID, state.Stage.FinishedAt,
		state.Stage.Version, state.Stage.ID, prepared.expectedStageAttemptVersion)
	if err != nil {
		return AuthoringReviewGateResolutionResult{}, err
	}
	if err := requireOneRow(result, "stage attempt", state.Stage.ID); err != nil {
		return AuthoringReviewGateResolutionResult{}, err
	}
	if err := appendAuthoringReviewGateResolutionAudits(ctx, s, tx, state.Request, state.Binding, decision, resolution, state.Node, state.Stage, prepared.actor, prepared.reason, now); err != nil {
		return AuthoringReviewGateResolutionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return AuthoringReviewGateResolutionResult{}, err
	}
	return AuthoringReviewGateResolutionResult{Request: state.Request, Binding: state.Binding, Decision: decision, Resolution: resolution, Run: state.Run, StageAttempt: state.Stage, NodeAttempt: state.Node}, nil
}

type preparedOpenAuthoringReviewGate struct {
	reviewRequestID             string
	bindingID                   string
	nodeAttemptID               string
	idempotencyKey              string
	runID                       string
	authoringSessionID          string
	authoringSourceID           string
	sourceSnapshotDigest        string
	expectedRunVersion          int64
	definitionHash              string
	stageAttemptID              string
	expectedStageAttemptVersion int64
	stageKey                    string
	reviewKind                  string
	nodeGeneration              int
	nodeAttemptOrdinal          int
	inputBindingsJSON           string
	inputFingerprint            string
	evidenceManifestDigest      string
	requestFingerprint          string
	bindingFingerprint          string
	actor                       string
	reason                      string
}

func prepareOpenAuthoringReviewGateRequest(request OpenAuthoringReviewGateRequest) (preparedOpenAuthoringReviewGate, error) {
	reviewRequestID, err := normalizeOptionalAuthoringReviewID(request.ReviewRequestID)
	if err != nil {
		return preparedOpenAuthoringReviewGate{}, err
	}
	bindingID, err := normalizeOptionalAuthoringReviewID(request.BindingID)
	if err != nil {
		return preparedOpenAuthoringReviewGate{}, err
	}
	nodeAttemptID, err := normalizeOptionalAuthoringReviewID(request.NodeAttemptID)
	if err != nil {
		return preparedOpenAuthoringReviewGate{}, err
	}
	if !isUUIDv7(request.RunID) || !isUUIDv7(request.AuthoringSessionID) || !isUUIDv7(request.AuthoringSourceID) || !isUUIDv7(request.StageAttemptID) {
		return preparedOpenAuthoringReviewGate{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedRunVersion <= 0 || request.ExpectedStageAttemptVersion <= 0 {
		return preparedOpenAuthoringReviewGate{}, fmt.Errorf("expected workflow run and stage attempt versions must be positive")
	}
	if request.NodeGeneration < 0 || request.NodeAttemptOrdinal <= 0 {
		return preparedOpenAuthoringReviewGate{}, fmt.Errorf("authoring review gate node generation must be non-negative and ordinal must be positive")
	}
	idempotencyKey, err := normalizeRequired(request.IdempotencyKey, "authoring review gate idempotency key")
	if err != nil {
		return preparedOpenAuthoringReviewGate{}, err
	}
	sourceSnapshotDigest, err := normalizeAuthoringSHA256(request.SourceSnapshotDigest, "authoring review gate source snapshot digest")
	if err != nil {
		return preparedOpenAuthoringReviewGate{}, err
	}
	definitionHash, err := normalizeRequired(request.DefinitionHash, "authoring review gate definition hash")
	if err != nil {
		return preparedOpenAuthoringReviewGate{}, err
	}
	stageKey, err := normalizeRequired(request.StageKey, "authoring review gate stage key")
	if err != nil {
		return preparedOpenAuthoringReviewGate{}, err
	}
	reviewKind, err := normalizeRequired(request.ReviewKind, "authoring review gate kind")
	if err != nil {
		return preparedOpenAuthoringReviewGate{}, err
	}
	inputBindingsJSON, err := normalizeV4JSON(request.InputBindingsJSON, "authoring review gate input bindings")
	if err != nil {
		return preparedOpenAuthoringReviewGate{}, err
	}
	inputFingerprint, err := normalizeRequired(request.InputFingerprint, "authoring review gate input fingerprint")
	if err != nil {
		return preparedOpenAuthoringReviewGate{}, err
	}
	evidenceManifestDigest, err := normalizeRequired(request.EvidenceManifestDigest, "authoring review gate evidence manifest digest")
	if err != nil {
		return preparedOpenAuthoringReviewGate{}, err
	}
	prepared := preparedOpenAuthoringReviewGate{
		reviewRequestID: reviewRequestID, bindingID: bindingID, nodeAttemptID: nodeAttemptID, idempotencyKey: idempotencyKey,
		runID: request.RunID, authoringSessionID: request.AuthoringSessionID, authoringSourceID: request.AuthoringSourceID,
		sourceSnapshotDigest: sourceSnapshotDigest, expectedRunVersion: request.ExpectedRunVersion, definitionHash: definitionHash,
		stageAttemptID: request.StageAttemptID, expectedStageAttemptVersion: request.ExpectedStageAttemptVersion,
		stageKey: stageKey, reviewKind: reviewKind, nodeGeneration: request.NodeGeneration, nodeAttemptOrdinal: request.NodeAttemptOrdinal,
		inputBindingsJSON: inputBindingsJSON, inputFingerprint: inputFingerprint, evidenceManifestDigest: evidenceManifestDigest,
		actor: request.Actor, reason: strings.TrimSpace(request.Reason),
	}
	prepared.requestFingerprint = authoringReviewGateOpenFingerprint(prepared)
	prepared.bindingFingerprint = authoringReviewGateBindingFingerprint(prepared)
	return prepared, nil
}

type authoringReviewGateOpenState struct {
	Run     WorkflowRun
	Session AuthoringSession
	Source  AuthoringSource
	Stage   StageAttempt
}

func loadAuthoringReviewOpenStateTx(ctx context.Context, tx *sql.Tx, request preparedOpenAuthoringReviewGate) (authoringReviewGateOpenState, error) {
	run, err := getWorkflowRunTx(ctx, tx, request.runID)
	if err != nil {
		return authoringReviewGateOpenState{}, err
	}
	if run.Version != request.expectedRunVersion {
		return authoringReviewGateOpenState{}, fmt.Errorf("%w: workflow run %s", ErrOptimisticLock, run.ID)
	}
	if run.Status != WorkflowRunRunning {
		return authoringReviewGateOpenState{}, fmt.Errorf("%w: workflow run %s is %s, not running", ErrInvalidTransition, run.ID, run.Status)
	}
	session, err := getAuthoringSessionTx(ctx, tx, request.authoringSessionID)
	if err != nil {
		return authoringReviewGateOpenState{}, err
	}
	source, err := getAuthoringSourceTx(ctx, tx, request.authoringSourceID)
	if err != nil {
		return authoringReviewGateOpenState{}, err
	}
	if err := validateAuthoringReviewRunBinding(run, session, source, request.authoringSessionID, request.authoringSourceID, request.sourceSnapshotDigest, request.definitionHash); err != nil {
		return authoringReviewGateOpenState{}, err
	}
	stage, err := getStageAttemptTx(ctx, tx, request.stageAttemptID)
	if err != nil {
		return authoringReviewGateOpenState{}, err
	}
	if stage.Version != request.expectedStageAttemptVersion {
		return authoringReviewGateOpenState{}, fmt.Errorf("%w: stage attempt %s", ErrOptimisticLock, stage.ID)
	}
	if stage.RunID != run.ID || stage.StageKey != request.stageKey || stage.InputFingerprint != request.inputFingerprint {
		return authoringReviewGateOpenState{}, fmt.Errorf("%w: authoring review stage does not match frozen input lineage", ErrImmutable)
	}
	if stage.ExecutionStatus != StageExecutionQueued && stage.ExecutionStatus != StageExecutionRunning {
		return authoringReviewGateOpenState{}, fmt.Errorf("%w: stage attempt %s is %s, cannot open authoring review", ErrInvalidTransition, stage.ID, stage.ExecutionStatus)
	}
	return authoringReviewGateOpenState{Run: run, Session: session, Source: source, Stage: stage}, nil
}

func validateAuthoringReviewRunBinding(run WorkflowRun, session AuthoringSession, source AuthoringSource, sessionID, sourceID, snapshotDigest, definitionHash string) error {
	if run.SubjectKind != WorkflowRunSubjectAuthoringSession || run.TaskID != "" || run.RevisionID != "" ||
		run.AuthoringSessionID != sessionID || run.SubjectID != sourceID || run.SubjectRevisionID != sessionID ||
		run.SubjectDigest != snapshotDigest || run.DefinitionHash != definitionHash || session.ID != sessionID ||
		session.SourceID != sourceID || source.ID != sourceID || source.SnapshotContentDigest != snapshotDigest {
		return fmt.Errorf("%w: authoring review gate does not match immutable source/session run lineage", ErrImmutable)
	}
	return nil
}

func (request preparedOpenAuthoringReviewGate) matchesBinding(review AuthoringReviewRequest, binding AuthoringReviewGateBinding, node NodeAttempt) error {
	if review.RunID != request.runID || review.AuthoringSessionID != request.authoringSessionID || review.AuthoringSourceID != request.authoringSourceID ||
		review.SourceSnapshotDigest != request.sourceSnapshotDigest || review.DefinitionHash != request.definitionHash ||
		review.EvidenceManifestDigest != request.evidenceManifestDigest || review.IdempotencyKey != request.idempotencyKey ||
		review.RequestFingerprint != request.requestFingerprint || binding.ReviewRequestID != review.ID ||
		binding.RunID != request.runID || binding.AuthoringSessionID != request.authoringSessionID || binding.AuthoringSourceID != request.authoringSourceID ||
		binding.SourceSnapshotDigest != request.sourceSnapshotDigest || binding.DefinitionHash != request.definitionHash ||
		binding.StageAttemptID != request.stageAttemptID || binding.StageKey != request.stageKey || binding.ReviewKind != request.reviewKind ||
		binding.InputBindingsJSON != request.inputBindingsJSON || binding.InputFingerprint != request.inputFingerprint ||
		binding.EvidenceManifestDigest != request.evidenceManifestDigest || binding.BindingFingerprint != request.bindingFingerprint ||
		(request.reviewRequestID != "" && review.ID != request.reviewRequestID) || (request.bindingID != "" && binding.ID != request.bindingID) ||
		(request.nodeAttemptID != "" && binding.NodeAttemptID != request.nodeAttemptID) || node.ID != binding.NodeAttemptID ||
		node.Generation != request.nodeGeneration || node.Attempt != request.nodeAttemptOrdinal {
		return fmt.Errorf("%w: authoring review gate open request differs from immutable binding", ErrIdempotencyConflict)
	}
	return nil
}

func replayOpenedAuthoringReviewGateTx(ctx context.Context, tx *sql.Tx, binding AuthoringReviewGateBinding, request preparedOpenAuthoringReviewGate) (AuthoringReviewGateOpenResult, error) {
	state, err := loadAuthoringReviewGateStateTx(ctx, tx, binding.ReviewRequestID)
	if err != nil {
		return AuthoringReviewGateOpenResult{}, err
	}
	if err := request.matchesBinding(state.Request, state.Binding, state.Node); err != nil {
		return AuthoringReviewGateOpenResult{}, err
	}
	return AuthoringReviewGateOpenResult{Request: state.Request, Binding: state.Binding, Run: state.Run, StageAttempt: state.Stage, NodeAttempt: state.Node}, nil
}

type preparedDecideAuthoringReviewGate struct {
	decisionID                  string
	resolutionJobID             string
	idempotencyKey              string
	reviewRequestID             string
	bindingID                   string
	runID                       string
	authoringSessionID          string
	authoringSourceID           string
	sourceSnapshotDigest        string
	definitionHash              string
	stageAttemptID              string
	inputFingerprint            string
	evidenceManifestDigest      string
	expectedRunVersion          int64
	expectedStageAttemptVersion int64
	action                      ReviewDecisionAction
	resolutionPayloadJSON       string
	resolutionPriority          int
	decisionFingerprint         string
	actor                       string
	reason                      string
}

func prepareDecideAuthoringReviewGateRequest(request DecideAuthoringReviewGateRequest) (preparedDecideAuthoringReviewGate, error) {
	decisionID, err := normalizeOptionalAuthoringReviewID(request.ID)
	if err != nil {
		return preparedDecideAuthoringReviewGate{}, err
	}
	resolutionJobID, err := normalizeOptionalAuthoringReviewID(request.ResolutionJobID)
	if err != nil {
		return preparedDecideAuthoringReviewGate{}, err
	}
	if !isUUIDv7(request.ReviewRequestID) || !isUUIDv7(request.BindingID) || !isUUIDv7(request.RunID) || !isUUIDv7(request.AuthoringSessionID) ||
		!isUUIDv7(request.AuthoringSourceID) || !isUUIDv7(request.StageAttemptID) {
		return preparedDecideAuthoringReviewGate{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedRunVersion <= 0 || request.ExpectedStageAttemptVersion <= 0 {
		return preparedDecideAuthoringReviewGate{}, fmt.Errorf("expected workflow run and stage attempt versions must be positive")
	}
	if !validReviewDecisionAction(request.Action) {
		return preparedDecideAuthoringReviewGate{}, fmt.Errorf("invalid authoring review decision action %q", request.Action)
	}
	idempotencyKey, err := normalizeRequired(request.IdempotencyKey, "authoring review decision idempotency key")
	if err != nil {
		return preparedDecideAuthoringReviewGate{}, err
	}
	sourceSnapshotDigest, err := normalizeAuthoringSHA256(request.SourceSnapshotDigest, "authoring review decision source snapshot digest")
	if err != nil {
		return preparedDecideAuthoringReviewGate{}, err
	}
	definitionHash, err := normalizeRequired(request.DefinitionHash, "authoring review decision definition hash")
	if err != nil {
		return preparedDecideAuthoringReviewGate{}, err
	}
	inputFingerprint, err := normalizeRequired(request.InputFingerprint, "authoring review decision input fingerprint")
	if err != nil {
		return preparedDecideAuthoringReviewGate{}, err
	}
	evidenceManifestDigest, err := normalizeRequired(request.EvidenceManifestDigest, "authoring review decision evidence manifest digest")
	if err != nil {
		return preparedDecideAuthoringReviewGate{}, err
	}
	payload, err := normalizeV4JSON(request.ResolutionPayloadJSON, "authoring review decision resolution payload")
	if err != nil {
		return preparedDecideAuthoringReviewGate{}, err
	}
	prepared := preparedDecideAuthoringReviewGate{
		decisionID: decisionID, resolutionJobID: resolutionJobID, idempotencyKey: idempotencyKey,
		reviewRequestID: request.ReviewRequestID, bindingID: request.BindingID, runID: request.RunID,
		authoringSessionID: request.AuthoringSessionID, authoringSourceID: request.AuthoringSourceID,
		sourceSnapshotDigest: sourceSnapshotDigest, definitionHash: definitionHash, stageAttemptID: request.StageAttemptID,
		inputFingerprint: inputFingerprint, evidenceManifestDigest: evidenceManifestDigest,
		expectedRunVersion: request.ExpectedRunVersion, expectedStageAttemptVersion: request.ExpectedStageAttemptVersion,
		action: request.Action, resolutionPayloadJSON: payload, resolutionPriority: request.ResolutionPriority,
		actor: request.Actor, reason: strings.TrimSpace(request.Reason),
	}
	prepared.decisionFingerprint = authoringReviewGateDecisionFingerprint(prepared)
	return prepared, nil
}

func (request preparedDecideAuthoringReviewGate) matchesBinding(review AuthoringReviewRequest, binding AuthoringReviewGateBinding) error {
	if review.ID != request.reviewRequestID || binding.ID != request.bindingID || binding.ReviewRequestID != review.ID ||
		binding.RunID != request.runID || binding.AuthoringSessionID != request.authoringSessionID ||
		binding.AuthoringSourceID != request.authoringSourceID || binding.SourceSnapshotDigest != request.sourceSnapshotDigest ||
		binding.DefinitionHash != request.definitionHash || binding.StageAttemptID != request.stageAttemptID ||
		binding.InputFingerprint != request.inputFingerprint || binding.EvidenceManifestDigest != request.evidenceManifestDigest {
		return fmt.Errorf("%w: authoring review decision does not match immutable gate binding", ErrImmutable)
	}
	return nil
}

func replayDecidedAuthoringReviewGateTx(ctx context.Context, tx *sql.Tx, state authoringReviewGateState, decision AuthoringReviewDecision, request preparedDecideAuthoringReviewGate) (AuthoringReviewGateDecisionResult, error) {
	if (request.decisionID != "" && decision.ID != request.decisionID) || decision.Action != request.action ||
		decision.DecisionFingerprint != request.decisionFingerprint || decision.IdempotencyKey != request.idempotencyKey ||
		decision.Actor != resolveActor(request.actor) || decision.Reason != request.reason {
		return AuthoringReviewGateDecisionResult{}, fmt.Errorf("%w: authoring review decision replay differs from stored decision", ErrIdempotencyConflict)
	}
	job, err := getDurableJobByIdempotencyTx(ctx, tx, AuthoringReviewGateResolutionJobKey(state.Binding.ID, decision.ID))
	if err != nil {
		return AuthoringReviewGateDecisionResult{}, fmt.Errorf("authoring review gate resolution job: %w", err)
	}
	if (request.resolutionJobID != "" && job.ID != request.resolutionJobID) || job.CommandType != AuthoringReviewGateResolutionCommandType ||
		job.EntityType != authoringReviewGateResolutionEntityType || job.EntityID != state.Binding.ID || job.RunID != state.Run.ID ||
		job.StageAttemptID != state.Stage.ID || job.Priority != request.resolutionPriority || job.PayloadJSON != request.resolutionPayloadJSON ||
		job.CreatedBy != decision.Actor {
		return AuthoringReviewGateDecisionResult{}, fmt.Errorf("%w: authoring review resolution job replay differs from stored job", ErrIdempotencyConflict)
	}
	return AuthoringReviewGateDecisionResult{Request: state.Request, Binding: state.Binding, Decision: decision, ResolutionJob: job}, nil
}

type preparedCompleteAuthoringReviewGate struct {
	resolutionID                string
	idempotencyKey              string
	reviewRequestID             string
	bindingID                   string
	decisionID                  string
	runID                       string
	authoringSessionID          string
	authoringSourceID           string
	sourceSnapshotDigest        string
	definitionHash              string
	stageAttemptID              string
	nodeAttemptID               string
	inputFingerprint            string
	evidenceManifestDigest      string
	expectedRunVersion          int64
	expectedStageAttemptVersion int64
	expectedNodeAttemptVersion  int64
	artifactManifestID          string
	resolutionEvidenceDigest    string
	resolutionPayloadJSON       string
	resolutionFingerprint       string
	actor                       string
	reason                      string
}

func prepareCompleteAuthoringReviewGateResolutionRequest(request CompleteAuthoringReviewGateResolutionRequest) (preparedCompleteAuthoringReviewGate, error) {
	resolutionID, err := normalizeOptionalAuthoringReviewID(request.ID)
	if err != nil {
		return preparedCompleteAuthoringReviewGate{}, err
	}
	if !isUUIDv7(request.ReviewRequestID) || !isUUIDv7(request.BindingID) || !isUUIDv7(request.DecisionID) || !isUUIDv7(request.RunID) ||
		!isUUIDv7(request.AuthoringSessionID) || !isUUIDv7(request.AuthoringSourceID) || !isUUIDv7(request.StageAttemptID) || !isUUIDv7(request.NodeAttemptID) {
		return preparedCompleteAuthoringReviewGate{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedRunVersion <= 0 || request.ExpectedStageAttemptVersion <= 0 || request.ExpectedNodeAttemptVersion <= 0 {
		return preparedCompleteAuthoringReviewGate{}, fmt.Errorf("expected workflow run, stage attempt, and node attempt versions must be positive")
	}
	artifactManifestID, err := optionalV4ID(request.ArtifactManifestID, "authoring review resolution artifact manifest ID")
	if err != nil {
		return preparedCompleteAuthoringReviewGate{}, err
	}
	idempotencyKey, err := normalizeRequired(request.IdempotencyKey, "authoring review resolution idempotency key")
	if err != nil {
		return preparedCompleteAuthoringReviewGate{}, err
	}
	sourceSnapshotDigest, err := normalizeAuthoringSHA256(request.SourceSnapshotDigest, "authoring review resolution source snapshot digest")
	if err != nil {
		return preparedCompleteAuthoringReviewGate{}, err
	}
	definitionHash, err := normalizeRequired(request.DefinitionHash, "authoring review resolution definition hash")
	if err != nil {
		return preparedCompleteAuthoringReviewGate{}, err
	}
	inputFingerprint, err := normalizeRequired(request.InputFingerprint, "authoring review resolution input fingerprint")
	if err != nil {
		return preparedCompleteAuthoringReviewGate{}, err
	}
	evidenceManifestDigest, err := normalizeRequired(request.EvidenceManifestDigest, "authoring review resolution evidence manifest digest")
	if err != nil {
		return preparedCompleteAuthoringReviewGate{}, err
	}
	resolutionEvidenceDigest, err := normalizeRequired(request.ResolutionEvidenceDigest, "authoring review resolution result evidence digest")
	if err != nil {
		return preparedCompleteAuthoringReviewGate{}, err
	}
	payload, err := normalizeV4JSON(request.ResolutionPayloadJSON, "authoring review resolution payload")
	if err != nil {
		return preparedCompleteAuthoringReviewGate{}, err
	}
	prepared := preparedCompleteAuthoringReviewGate{
		resolutionID: resolutionID, idempotencyKey: idempotencyKey, reviewRequestID: request.ReviewRequestID,
		bindingID: request.BindingID, decisionID: request.DecisionID, runID: request.RunID,
		authoringSessionID: request.AuthoringSessionID, authoringSourceID: request.AuthoringSourceID,
		sourceSnapshotDigest: sourceSnapshotDigest, definitionHash: definitionHash, stageAttemptID: request.StageAttemptID,
		nodeAttemptID: request.NodeAttemptID, inputFingerprint: inputFingerprint, evidenceManifestDigest: evidenceManifestDigest,
		expectedRunVersion: request.ExpectedRunVersion, expectedStageAttemptVersion: request.ExpectedStageAttemptVersion,
		expectedNodeAttemptVersion: request.ExpectedNodeAttemptVersion, artifactManifestID: artifactManifestID,
		resolutionEvidenceDigest: resolutionEvidenceDigest,
		resolutionPayloadJSON:    payload, actor: request.Actor, reason: strings.TrimSpace(request.Reason),
	}
	prepared.resolutionFingerprint = authoringReviewGateResolutionFingerprint(prepared)
	return prepared, nil
}

func (request preparedCompleteAuthoringReviewGate) matchesBinding(review AuthoringReviewRequest, binding AuthoringReviewGateBinding) error {
	if review.ID != request.reviewRequestID || binding.ID != request.bindingID || binding.ReviewRequestID != review.ID ||
		binding.RunID != request.runID || binding.AuthoringSessionID != request.authoringSessionID ||
		binding.AuthoringSourceID != request.authoringSourceID || binding.SourceSnapshotDigest != request.sourceSnapshotDigest ||
		binding.DefinitionHash != request.definitionHash || binding.StageAttemptID != request.stageAttemptID ||
		binding.NodeAttemptID != request.nodeAttemptID || binding.InputFingerprint != request.inputFingerprint ||
		binding.EvidenceManifestDigest != request.evidenceManifestDigest {
		return fmt.Errorf("%w: authoring review resolution does not match immutable gate binding", ErrImmutable)
	}
	return nil
}

func replayCompletedAuthoringReviewGateTx(ctx context.Context, tx *sql.Tx, state authoringReviewGateState, decision AuthoringReviewDecision, resolution AuthoringReviewGateResolution, request preparedCompleteAuthoringReviewGate, verdict Verdict) (AuthoringReviewGateResolutionResult, error) {
	if (request.resolutionID != "" && resolution.ID != request.resolutionID) || resolution.DecisionID != decision.ID ||
		resolution.Verdict != verdict || resolution.ArtifactManifestID != request.artifactManifestID ||
		resolution.ResolutionEvidenceDigest != request.resolutionEvidenceDigest ||
		resolution.ResolutionPayloadJSON != request.resolutionPayloadJSON || resolution.ResolutionFingerprint != request.resolutionFingerprint ||
		resolution.IdempotencyKey != request.idempotencyKey || resolution.CreatedBy != resolveActor(request.actor) {
		return AuthoringReviewGateResolutionResult{}, fmt.Errorf("%w: authoring review resolution replay differs from stored receipt", ErrIdempotencyConflict)
	}
	if state.Stage.ExecutionStatus != StageExecutionCompleted || state.Stage.Verdict != verdict ||
		state.Stage.ArtifactManifestID != resolution.ArtifactManifestID || state.Node.Status != NodeAttemptCompleted {
		return AuthoringReviewGateResolutionResult{}, fmt.Errorf("%w: authoring review gate %s was already resolved inconsistently", ErrImmutable, state.Binding.ID)
	}
	return AuthoringReviewGateResolutionResult{Request: state.Request, Binding: state.Binding, Decision: decision, Resolution: resolution, Run: state.Run, StageAttempt: state.Stage, NodeAttempt: state.Node}, nil
}

type authoringReviewGateState struct {
	Request AuthoringReviewRequest
	Binding AuthoringReviewGateBinding
	Run     WorkflowRun
	Session AuthoringSession
	Source  AuthoringSource
	Stage   StageAttempt
	Node    NodeAttempt
}

func loadAuthoringReviewGateStateTx(ctx context.Context, tx *sql.Tx, reviewRequestID string) (authoringReviewGateState, error) {
	binding, err := getAuthoringReviewGateBindingByRequestTx(ctx, tx, reviewRequestID)
	if err != nil {
		return authoringReviewGateState{}, err
	}
	if binding == nil {
		return authoringReviewGateState{}, fmt.Errorf("%w: authoring review request %s is not a source/session review gate", ErrNotFound, reviewRequestID)
	}
	review, err := getAuthoringReviewRequestTx(ctx, tx, binding.ReviewRequestID)
	if err != nil {
		return authoringReviewGateState{}, err
	}
	run, err := getWorkflowRunTx(ctx, tx, binding.RunID)
	if err != nil {
		return authoringReviewGateState{}, err
	}
	session, err := getAuthoringSessionTx(ctx, tx, binding.AuthoringSessionID)
	if err != nil {
		return authoringReviewGateState{}, err
	}
	source, err := getAuthoringSourceTx(ctx, tx, binding.AuthoringSourceID)
	if err != nil {
		return authoringReviewGateState{}, err
	}
	stage, err := getStageAttemptTx(ctx, tx, binding.StageAttemptID)
	if err != nil {
		return authoringReviewGateState{}, err
	}
	node, err := getNodeAttemptTx(ctx, tx, binding.NodeAttemptID)
	if err != nil {
		return authoringReviewGateState{}, err
	}
	if review.RunID != binding.RunID || review.AuthoringSessionID != binding.AuthoringSessionID ||
		review.AuthoringSourceID != binding.AuthoringSourceID || review.SourceSnapshotDigest != binding.SourceSnapshotDigest ||
		review.DefinitionHash != binding.DefinitionHash || review.EvidenceManifestDigest != binding.EvidenceManifestDigest {
		return authoringReviewGateState{}, fmt.Errorf("%w: persisted authoring review request and binding differ", ErrImmutable)
	}
	if err := validateAuthoringReviewRunBinding(run, session, source, binding.AuthoringSessionID, binding.AuthoringSourceID, binding.SourceSnapshotDigest, binding.DefinitionHash); err != nil {
		return authoringReviewGateState{}, err
	}
	if stage.RunID != binding.RunID || stage.StageKey != binding.StageKey || stage.InputFingerprint != binding.InputFingerprint ||
		node.StageAttemptID != binding.StageAttemptID || node.NodeID != binding.StageKey || node.Generation != binding.NodeGeneration ||
		node.Attempt != binding.NodeAttemptOrdinal {
		return authoringReviewGateState{}, fmt.Errorf("%w: persisted authoring review binding has inconsistent stage/node lineage", ErrImmutable)
	}
	return authoringReviewGateState{Request: review, Binding: *binding, Run: run, Session: session, Source: source, Stage: stage, Node: node}, nil
}

func getAuthoringReviewRequestTx(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, reviewRequestID string) (AuthoringReviewRequest, error) {
	review, err := scanAuthoringReviewRequest(queryer.QueryRowContext(ctx, authoringReviewRequestV22Select+" WHERE id = ?", reviewRequestID))
	if err == sql.ErrNoRows {
		return AuthoringReviewRequest{}, fmt.Errorf("%w: authoring review request %s", ErrNotFound, reviewRequestID)
	}
	return review, err
}

func getAuthoringReviewRequestByKeyTx(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, idempotencyKey string) (*AuthoringReviewRequest, error) {
	review, err := scanAuthoringReviewRequest(queryer.QueryRowContext(ctx, authoringReviewRequestV22Select+" WHERE idempotency_key = ?", idempotencyKey))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &review, nil
}

func getAuthoringReviewGateBindingByRequestTx(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, reviewRequestID string) (*AuthoringReviewGateBinding, error) {
	binding, err := scanAuthoringReviewGateBinding(queryer.QueryRowContext(ctx, authoringReviewGateBindingV22Select+" WHERE review_request_id = ?", reviewRequestID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &binding, nil
}

func getAuthoringReviewGateBindingByStageAttemptTx(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, stageAttemptID string) (*AuthoringReviewGateBinding, error) {
	binding, err := scanAuthoringReviewGateBinding(queryer.QueryRowContext(ctx, authoringReviewGateBindingV22Select+" WHERE stage_attempt_id = ?", stageAttemptID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &binding, nil
}

func getAuthoringReviewGateBindingByNodeAttemptTx(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, nodeAttemptID string) (*AuthoringReviewGateBinding, error) {
	binding, err := scanAuthoringReviewGateBinding(queryer.QueryRowContext(ctx, authoringReviewGateBindingV22Select+" WHERE node_attempt_id = ?", nodeAttemptID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &binding, nil
}

func getAuthoringReviewDecisionByRequestTx(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, reviewRequestID string) (*AuthoringReviewDecision, error) {
	decision, err := scanAuthoringReviewDecision(queryer.QueryRowContext(ctx, authoringReviewDecisionV22Select+" WHERE review_request_id = ?", reviewRequestID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &decision, nil
}

func getAuthoringReviewDecisionTx(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, decisionID string) (AuthoringReviewDecision, error) {
	decision, err := scanAuthoringReviewDecision(queryer.QueryRowContext(ctx, authoringReviewDecisionV22Select+" WHERE id = ?", decisionID))
	if err == sql.ErrNoRows {
		return AuthoringReviewDecision{}, fmt.Errorf("%w: authoring review decision %s", ErrNotFound, decisionID)
	}
	return decision, err
}

func getAuthoringReviewGateResolutionByRequestTx(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, reviewRequestID string) (*AuthoringReviewGateResolution, error) {
	resolution, err := scanAuthoringReviewGateResolution(queryer.QueryRowContext(ctx, authoringReviewGateResolutionV22Select+" WHERE review_request_id = ?", reviewRequestID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &resolution, nil
}

func scanAuthoringReviewRequest(scanner rowScanner) (AuthoringReviewRequest, error) {
	var review AuthoringReviewRequest
	if err := scanner.Scan(
		&review.ID, &review.RunID, &review.AuthoringSessionID, &review.AuthoringSourceID,
		&review.SourceSnapshotDigest, &review.DefinitionHash, &review.EvidenceManifestDigest,
		&review.RequestFingerprint, &review.IdempotencyKey, &review.CreatedBy, &review.CreatedAt,
	); err != nil {
		return AuthoringReviewRequest{}, err
	}
	review.CreatedAt = review.CreatedAt.UTC()
	return review, nil
}

func scanAuthoringReviewGateBinding(scanner rowScanner) (AuthoringReviewGateBinding, error) {
	var binding AuthoringReviewGateBinding
	if err := scanner.Scan(
		&binding.ID, &binding.ReviewRequestID, &binding.RunID, &binding.AuthoringSessionID, &binding.AuthoringSourceID,
		&binding.SourceSnapshotDigest, &binding.DefinitionHash, &binding.StageAttemptID, &binding.StageKey,
		&binding.NodeAttemptID, &binding.NodeGeneration, &binding.NodeAttemptOrdinal, &binding.ReviewKind,
		&binding.InputBindingsJSON, &binding.InputFingerprint, &binding.EvidenceManifestDigest,
		&binding.BindingFingerprint, &binding.CreatedAt,
	); err != nil {
		return AuthoringReviewGateBinding{}, err
	}
	binding.CreatedAt = binding.CreatedAt.UTC()
	return binding, nil
}

func scanAuthoringReviewDecision(scanner rowScanner) (AuthoringReviewDecision, error) {
	var decision AuthoringReviewDecision
	if err := scanner.Scan(
		&decision.ID, &decision.ReviewRequestID, &decision.BindingID, &decision.Action, &decision.DecisionFingerprint,
		&decision.IdempotencyKey, &decision.Actor, &decision.Reason, &decision.CreatedAt,
	); err != nil {
		return AuthoringReviewDecision{}, err
	}
	decision.CreatedAt = decision.CreatedAt.UTC()
	return decision, nil
}

func scanAuthoringReviewGateResolution(scanner rowScanner) (AuthoringReviewGateResolution, error) {
	var resolution AuthoringReviewGateResolution
	if err := scanner.Scan(
		&resolution.ID, &resolution.ReviewRequestID, &resolution.BindingID, &resolution.DecisionID, &resolution.Verdict,
		&resolution.ArtifactManifestID, &resolution.ResolutionEvidenceDigest, &resolution.ResolutionPayloadJSON,
		&resolution.ResolutionFingerprint, &resolution.IdempotencyKey, &resolution.CreatedBy, &resolution.CreatedAt,
	); err != nil {
		return AuthoringReviewGateResolution{}, err
	}
	resolution.CreatedAt = resolution.CreatedAt.UTC()
	return resolution, nil
}

func insertAuthoringReviewGateResolutionJobTx(ctx context.Context, tx *sql.Tx, job DurableJob) (bool, error) {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO jobs (
			id, command_type, entity_type, entity_id, run_id, stage_attempt_id, state,
			priority, payload_json, idempotency_key, created_by, created_at, updated_at,
			started_at, finished_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)
	`, job.ID, job.CommandType, job.EntityType, job.EntityID, job.RunID, job.StageAttemptID, job.State,
		job.Priority, job.PayloadJSON, job.IdempotencyKey, job.CreatedBy, job.CreatedAt, job.UpdatedAt, job.Version)
	if err == nil {
		return true, nil
	}
	if isGlobalIdentityCollision(err) {
		return false, fmt.Errorf("%w: authoring review resolution job %s", ErrIdentityCollision, job.ID)
	}
	if !isUniqueConstraint(err) {
		return false, err
	}
	existing, existingErr := getDurableJobByIdempotencyTx(ctx, tx, job.IdempotencyKey)
	if existingErr != nil {
		return false, existingErr
	}
	if !sameDurableJobRequest(existing, job) || existing.ID != job.ID {
		return false, fmt.Errorf("%w: authoring review resolution job %s", ErrIdempotencyConflict, job.IdempotencyKey)
	}
	return false, nil
}

// validateAuthoringReviewGateResolutionManifestTx verifies generic artifact
// lineage against the immutable AuthoringSession subject. Unlike the V15
// task-review helper, SessionID is the subject revision coordinate here and no
// TaskRevision lookup is performed or implied.
func validateAuthoringReviewGateResolutionManifestTx(ctx context.Context, tx *sql.Tx, manifestID string, binding AuthoringReviewGateBinding) error {
	manifest, err := getArtifactManifestTx(ctx, tx, manifestID)
	if err != nil {
		return fmt.Errorf("authoring review gate resolution manifest: %w", err)
	}
	if manifest.SubjectRevisionID != binding.AuthoringSessionID || manifest.SubjectDigest != binding.SourceSnapshotDigest || manifest.WorkflowFingerprint != binding.DefinitionHash {
		return fmt.Errorf("%w: authoring review resolution manifest does not match frozen source/session/run lineage", ErrImmutable)
	}
	rows, err := tx.QueryContext(ctx, artifactRefV4Select+" WHERE manifest_id = ? ORDER BY artifact_key ASC, id ASC", manifestID)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		reference, err := scanArtifactRef(rows)
		if err != nil {
			return err
		}
		count++
		if reference.RunID != binding.RunID || reference.StageKey != binding.StageKey || reference.AttemptID != binding.StageAttemptID ||
			reference.SubjectRevisionID != binding.AuthoringSessionID || reference.SubjectDigest != binding.SourceSnapshotDigest ||
			reference.WorkflowFingerprint != binding.DefinitionHash || reference.InputBindingsJSON != binding.InputBindingsJSON ||
			reference.InputFingerprint != binding.InputFingerprint {
			return fmt.Errorf("%w: authoring review resolution artifact ref %s does not match frozen lineage", ErrImmutable, reference.ID)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("%w: authoring review resolution manifest has no typed artifact refs", ErrImmutable)
	}
	return nil
}

func normalizeOptionalAuthoringReviewID(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	if !isUUIDv7(value) {
		return "", ErrInvalidUUIDv7Identity
	}
	return value, nil
}

func authoringReviewGateNodeAttemptKey(stageAttemptID string) string {
	return "authoring-review-gate-node:" + stageAttemptID
}

func authoringReviewGateOpenFingerprint(request preparedOpenAuthoringReviewGate) string {
	payload, _ := json.Marshal(struct {
		Domain                 string `json:"domain"`
		RunID                  string `json:"run_id"`
		SessionID              string `json:"authoring_session_id"`
		SourceID               string `json:"authoring_source_id"`
		SnapshotDigest         string `json:"source_snapshot_digest"`
		DefinitionHash         string `json:"definition_hash"`
		StageAttemptID         string `json:"stage_attempt_id"`
		StageKey               string `json:"stage_key"`
		ReviewKind             string `json:"review_kind"`
		NodeGeneration         int    `json:"node_generation"`
		NodeAttemptOrdinal     int    `json:"node_attempt_ordinal"`
		InputBindingsJSON      string `json:"input_bindings_json"`
		InputFingerprint       string `json:"input_fingerprint"`
		EvidenceManifestDigest string `json:"evidence_manifest_digest"`
	}{
		"harbor.authoring-review-gate-open.v22", request.runID, request.authoringSessionID, request.authoringSourceID,
		request.sourceSnapshotDigest, request.definitionHash, request.stageAttemptID, request.stageKey, request.reviewKind,
		request.nodeGeneration, request.nodeAttemptOrdinal, request.inputBindingsJSON, request.inputFingerprint, request.evidenceManifestDigest,
	})
	return sha256Fingerprint(payload)
}

func authoringReviewGateBindingFingerprint(request preparedOpenAuthoringReviewGate) string {
	payload, _ := json.Marshal(struct {
		Domain             string `json:"domain"`
		RequestFingerprint string `json:"request_fingerprint"`
		RunID              string `json:"run_id"`
		SessionID          string `json:"authoring_session_id"`
		SourceID           string `json:"authoring_source_id"`
		SnapshotDigest     string `json:"source_snapshot_digest"`
		DefinitionHash     string `json:"definition_hash"`
		StageAttemptID     string `json:"stage_attempt_id"`
		StageKey           string `json:"stage_key"`
		ReviewKind         string `json:"review_kind"`
		InputBindingsJSON  string `json:"input_bindings_json"`
		InputFingerprint   string `json:"input_fingerprint"`
	}{
		"harbor.authoring-review-gate-binding.v22", request.requestFingerprint, request.runID, request.authoringSessionID,
		request.authoringSourceID, request.sourceSnapshotDigest, request.definitionHash, request.stageAttemptID,
		request.stageKey, request.reviewKind, request.inputBindingsJSON, request.inputFingerprint,
	})
	return sha256Fingerprint(payload)
}

func authoringReviewGateDecisionFingerprint(request preparedDecideAuthoringReviewGate) string {
	payload, _ := json.Marshal(struct {
		Domain             string               `json:"domain"`
		ReviewRequestID    string               `json:"review_request_id"`
		BindingID          string               `json:"binding_id"`
		Action             ReviewDecisionAction `json:"action"`
		ResolutionPayload  string               `json:"resolution_payload_json"`
		ResolutionPriority int                  `json:"resolution_priority"`
		Actor              string               `json:"actor"`
		Reason             string               `json:"reason"`
	}{
		"harbor.authoring-review-gate-decision.v22", request.reviewRequestID, request.bindingID, request.action,
		request.resolutionPayloadJSON, request.resolutionPriority, resolveActor(request.actor), request.reason,
	})
	return sha256Fingerprint(payload)
}

func authoringReviewGateResolutionFingerprint(request preparedCompleteAuthoringReviewGate) string {
	payload, _ := json.Marshal(struct {
		Domain                   string `json:"domain"`
		ReviewRequestID          string `json:"review_request_id"`
		BindingID                string `json:"binding_id"`
		DecisionID               string `json:"decision_id"`
		ArtifactManifestID       string `json:"artifact_manifest_id"`
		ResolutionEvidenceDigest string `json:"resolution_evidence_digest"`
		ResolutionPayloadJSON    string `json:"resolution_payload_json"`
		Actor                    string `json:"actor"`
		Reason                   string `json:"reason"`
	}{
		"harbor.authoring-review-gate-resolution.v22", request.reviewRequestID, request.bindingID, request.decisionID,
		request.artifactManifestID, request.resolutionEvidenceDigest, request.resolutionPayloadJSON, resolveActor(request.actor), request.reason,
	})
	return sha256Fingerprint(payload)
}

func appendAuthoringReviewGateOpenAudits(ctx context.Context, s *Store, tx *sql.Tx, review AuthoringReviewRequest, binding AuthoringReviewGateBinding, node NodeAttempt, stage StageAttempt, run WorkflowRun, actor, reason string, now time.Time) error {
	events := []AuditEvent{
		{
			Actor: actor, EntityType: "authoring_review_request", EntityID: review.ID, Action: "authoring_review.requested", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"run_id": review.RunID, "authoring_session_id": review.AuthoringSessionID, "authoring_source_id": review.AuthoringSourceID, "source_snapshot_digest": review.SourceSnapshotDigest, "evidence_manifest_digest": review.EvidenceManifestDigest}), CreatedAt: now,
		},
		{
			Actor: actor, EntityType: "authoring_review_gate_binding", EntityID: binding.ID, Action: "authoring_review_gate.opened", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"review_request_id": binding.ReviewRequestID, "run_id": binding.RunID, "stage_attempt_id": binding.StageAttemptID, "node_attempt_id": binding.NodeAttemptID, "definition_hash": binding.DefinitionHash, "stage_key": binding.StageKey, "review_kind": binding.ReviewKind, "input_fingerprint": binding.InputFingerprint}), CreatedAt: now,
		},
		{
			Actor: actor, EntityType: "node_attempt", EntityID: node.ID, Action: "node_attempt.waiting_for_authoring_review", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"stage_attempt_id": node.StageAttemptID, "status": node.Status, "version": node.Version}), CreatedAt: now,
		},
		{
			Actor: actor, EntityType: "stage_attempt", EntityID: stage.ID, Action: "stage_attempt.waiting_for_authoring_review", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"execution_status": stage.ExecutionStatus, "version": stage.Version, "review_request_id": binding.ReviewRequestID}), CreatedAt: now,
		},
		{
			Actor: actor, EntityType: "workflow_run", EntityID: run.ID, Action: "workflow_run.waiting_for_authoring_review", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"status": run.Status, "version": run.Version, "stage_attempt_id": stage.ID}), CreatedAt: now,
		},
	}
	for _, event := range events {
		if _, err := s.appendAuditTx(ctx, tx, event); err != nil {
			return err
		}
	}
	return nil
}

func appendAuthoringReviewGateDecisionAudits(ctx context.Context, s *Store, tx *sql.Tx, review AuthoringReviewRequest, binding AuthoringReviewGateBinding, decision AuthoringReviewDecision, job DurableJob, actor, reason string, now time.Time) error {
	events := []AuditEvent{
		{
			Actor: actor, EntityType: "authoring_review_decision", EntityID: decision.ID, Action: "authoring_review.decided", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"review_request_id": review.ID, "binding_id": binding.ID, "action": decision.Action, "stage_attempt_id": binding.StageAttemptID}), CreatedAt: now,
		},
		{
			Actor: actor, EntityType: "job", EntityID: job.ID, Action: "job.created", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"command_type": job.CommandType, "entity_type": job.EntityType, "entity_id": job.EntityID, "idempotency_key": job.IdempotencyKey, "authoring_review_decision_id": decision.ID}), CreatedAt: now,
		},
	}
	for _, event := range events {
		if _, err := s.appendAuditTx(ctx, tx, event); err != nil {
			return err
		}
	}
	return nil
}

func appendAuthoringReviewGateResolutionAudits(ctx context.Context, s *Store, tx *sql.Tx, review AuthoringReviewRequest, binding AuthoringReviewGateBinding, decision AuthoringReviewDecision, resolution AuthoringReviewGateResolution, node NodeAttempt, stage StageAttempt, actor, reason string, now time.Time) error {
	events := []AuditEvent{
		{
			Actor: actor, EntityType: "authoring_review_gate_resolution", EntityID: resolution.ID, Action: "authoring_review_gate.resolved", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"review_request_id": review.ID, "binding_id": binding.ID, "decision_id": decision.ID, "resolution_evidence_digest": resolution.ResolutionEvidenceDigest, "verdict": resolution.Verdict}), CreatedAt: now,
		},
		{
			Actor: actor, EntityType: "node_attempt", EntityID: node.ID, Action: "node_attempt.completed_for_authoring_review", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"stage_attempt_id": node.StageAttemptID, "status": node.Status, "version": node.Version, "review_decision_id": decision.ID}), CreatedAt: now,
		},
		{
			Actor: actor, EntityType: "stage_attempt", EntityID: stage.ID, Action: "authoring_review_gate.stage_completed", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"review_request_id": review.ID, "review_decision_id": decision.ID, "execution_status": stage.ExecutionStatus, "verdict": stage.Verdict, "version": stage.Version}), CreatedAt: now,
		},
	}
	for _, event := range events {
		if _, err := s.appendAuditTx(ctx, tx, event); err != nil {
			return err
		}
	}
	return nil
}
