package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const reviewGateBindingV15Select = `
	SELECT stage_attempt_id, review_request_id, run_id, revision_id, revision_digest,
	       definition_hash, stage_key, review_kind, node_attempt_id,
	       input_bindings_json, input_fingerprint, evidence_manifest_digest, created_at
	FROM review_gate_bindings_v15`

// GetReviewGateBindingByReviewRequest returns the frozen gate binding for a
// generic review envelope. A nil binding means the review remains an ordinary
// lifecycle review and must use the existing generic review APIs.
func (s *Store) GetReviewGateBindingByReviewRequest(ctx context.Context, reviewRequestID string) (*ReviewGateBinding, error) {
	if !isUUIDv7(reviewRequestID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	binding, err := getReviewGateBindingByReviewRequestTx(ctx, s.db, reviewRequestID)
	if err != nil {
		return nil, err
	}
	return binding, nil
}

// GetReviewGateBindingByStageAttempt returns the immutable review binding for
// a stage attempt, if that stage is a human review gate.
func (s *Store) GetReviewGateBindingByStageAttempt(ctx context.Context, stageAttemptID string) (*ReviewGateBinding, error) {
	if !isUUIDv7(stageAttemptID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	binding, err := getReviewGateBindingByStageAttemptTx(ctx, s.db, stageAttemptID)
	if err != nil {
		return nil, err
	}
	return binding, nil
}

// ListReviewGateBindingsForRevision exposes gate review history without
// allowing callers to infer it from mutable run state.
func (s *Store) ListReviewGateBindingsForRevision(ctx context.Context, revisionID string) ([]ReviewGateBinding, error) {
	if !isUUIDv7(revisionID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, reviewGateBindingV15Select+" WHERE revision_id = ? ORDER BY created_at DESC, stage_attempt_id ASC", revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bindings := make([]ReviewGateBinding, 0)
	for rows.Next() {
		binding, err := scanReviewGateBinding(rows)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, rows.Err()
}

// OpenReviewGate atomically turns a frozen stage into a durable human-review
// wait. It intentionally does not create a job or acquire a quota lease: the
// stage worker has finished its activation work before the human wait begins.
func (s *Store) OpenReviewGate(ctx context.Context, request OpenReviewGateRequest) (ReviewGateOpenResult, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ReviewGateOpenResult{}, err
	}
	prepared, err := prepareOpenReviewGateRequest(request)
	if err != nil {
		return ReviewGateOpenResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewGateOpenResult{}, err
	}
	defer tx.Rollback()

	if existing, err := getReviewGateBindingByStageAttemptTx(ctx, tx, prepared.stageAttemptID); err != nil {
		return ReviewGateOpenResult{}, err
	} else if existing != nil {
		result, err := s.replayOpenedReviewGateTx(ctx, tx, *existing, prepared)
		if err != nil {
			return ReviewGateOpenResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return ReviewGateOpenResult{}, err
		}
		return result, nil
	}
	if prepared.reviewRequestID != "" {
		existing, err := getReviewGateBindingByReviewRequestTx(ctx, tx, prepared.reviewRequestID)
		if err != nil {
			return ReviewGateOpenResult{}, err
		}
		if existing != nil {
			return ReviewGateOpenResult{}, fmt.Errorf("%w: review request %s is already bound to stage attempt %s", ErrIdempotencyConflict, prepared.reviewRequestID, existing.StageAttemptID)
		}
	}

	run, err := getWorkflowRunTx(ctx, tx, prepared.runID)
	if err != nil {
		return ReviewGateOpenResult{}, err
	}
	if run.Version != prepared.expectedRunVersion {
		return ReviewGateOpenResult{}, fmt.Errorf("%w: workflow run %s", ErrOptimisticLock, run.ID)
	}
	if run.Status != WorkflowRunRunning {
		return ReviewGateOpenResult{}, fmt.Errorf("%w: workflow run %s is %s, not running", ErrInvalidTransition, run.ID, run.Status)
	}
	revision, err := getTaskRevisionTx(ctx, tx, prepared.revisionID)
	if err != nil {
		return ReviewGateOpenResult{}, err
	}
	if revision.ID != run.RevisionID || revision.TaskID != run.TaskID || revision.TaskDigest != prepared.revisionDigest {
		return ReviewGateOpenResult{}, fmt.Errorf("%w: review gate revision does not match frozen workflow run", ErrImmutable)
	}
	if run.DefinitionHash != prepared.definitionHash {
		return ReviewGateOpenResult{}, fmt.Errorf("%w: review gate definition hash does not match frozen workflow run", ErrImmutable)
	}
	stage, err := getStageAttemptTx(ctx, tx, prepared.stageAttemptID)
	if err != nil {
		return ReviewGateOpenResult{}, err
	}
	if stage.Version != prepared.expectedStageAttemptVersion {
		return ReviewGateOpenResult{}, fmt.Errorf("%w: stage attempt %s", ErrOptimisticLock, stage.ID)
	}
	if stage.RunID != run.ID || stage.StageKey != prepared.stageKey || stage.InputFingerprint != prepared.inputFingerprint {
		return ReviewGateOpenResult{}, fmt.Errorf("%w: review gate stage does not match frozen input lineage", ErrImmutable)
	}
	if stage.ExecutionStatus != StageExecutionQueued && stage.ExecutionStatus != StageExecutionRunning {
		return ReviewGateOpenResult{}, fmt.Errorf("%w: stage attempt %s is %s, cannot open a review gate", ErrInvalidTransition, stage.ID, stage.ExecutionStatus)
	}

	reviewRequestID := prepared.reviewRequestID
	if reviewRequestID == "" {
		reviewRequestID, err = s.newV2ID("")
		if err != nil {
			return ReviewGateOpenResult{}, err
		}
	}
	nodeAttemptID := prepared.nodeAttemptID
	if nodeAttemptID == "" {
		nodeAttemptID, err = s.newV2ID("")
		if err != nil {
			return ReviewGateOpenResult{}, err
		}
	}
	now := s.now().UTC()
	actor := resolveActor(prepared.actor)
	review := ReviewRequest{
		ID:                     reviewRequestID,
		RevisionID:             revision.ID,
		EvidenceManifestDigest: prepared.evidenceManifestDigest,
		State:                  "open",
		CreatedBy:              actor,
		CreatedAt:              now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO review_requests (id, revision_id, evidence_manifest_digest, state, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, review.ID, review.RevisionID, review.EvidenceManifestDigest, review.State, review.CreatedBy, review.CreatedAt); err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return ReviewGateOpenResult{}, fmt.Errorf("%w: review request %s", ErrIdentityCollision, review.ID)
		}
		return ReviewGateOpenResult{}, err
	}
	node := NodeAttempt{
		ID:             nodeAttemptID,
		StageAttemptID: stage.ID,
		NodeID:         stage.StageKey,
		Generation:     prepared.nodeGeneration,
		Attempt:        prepared.nodeAttempt,
		Status:         NodeAttemptWaiting,
		IdempotencyKey: reviewGateNodeAttemptKey(stage.ID),
		CreatedAt:      now,
		StartedAt:      &now,
		Version:        1,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO node_attempts (
			id, stage_attempt_id, node_id, generation, attempt, status, idempotency_key,
			started_at, finished_at, error_text, created_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, '', ?, ?)
	`, node.ID, node.StageAttemptID, node.NodeID, node.Generation, node.Attempt, node.Status, node.IdempotencyKey,
		node.StartedAt, node.CreatedAt, node.Version); err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return ReviewGateOpenResult{}, fmt.Errorf("%w: review gate node attempt %s", ErrIdentityCollision, node.ID)
		}
		return ReviewGateOpenResult{}, err
	}
	binding := ReviewGateBinding{
		StageAttemptID:         stage.ID,
		ReviewRequestID:        review.ID,
		RunID:                  run.ID,
		RevisionID:             revision.ID,
		RevisionDigest:         revision.TaskDigest,
		DefinitionHash:         run.DefinitionHash,
		StageKey:               stage.StageKey,
		ReviewKind:             prepared.reviewKind,
		NodeAttemptID:          node.ID,
		InputBindingsJSON:      prepared.inputBindingsJSON,
		InputFingerprint:       stage.InputFingerprint,
		EvidenceManifestDigest: review.EvidenceManifestDigest,
		CreatedAt:              now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO review_gate_bindings_v15 (
			stage_attempt_id, review_request_id, run_id, revision_id, revision_digest,
			definition_hash, stage_key, review_kind, node_attempt_id,
			input_bindings_json, input_fingerprint, evidence_manifest_digest, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, binding.StageAttemptID, binding.ReviewRequestID, binding.RunID, binding.RevisionID, binding.RevisionDigest,
		binding.DefinitionHash, binding.StageKey, binding.ReviewKind, binding.NodeAttemptID,
		binding.InputBindingsJSON, binding.InputFingerprint, binding.EvidenceManifestDigest, binding.CreatedAt); err != nil {
		if isUniqueConstraint(err) {
			return ReviewGateOpenResult{}, fmt.Errorf("%w: review gate stage attempt %s", ErrIdempotencyConflict, binding.StageAttemptID)
		}
		return ReviewGateOpenResult{}, err
	}

	stage.ExecutionStatus = StageExecutionWaiting
	if stage.StartedAt == nil {
		stage.StartedAt = &now
	}
	stage.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE stage_attempts
		SET execution_status = ?, started_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, stage.ExecutionStatus, stage.StartedAt, stage.Version, stage.ID, prepared.expectedStageAttemptVersion)
	if err != nil {
		return ReviewGateOpenResult{}, err
	}
	if err := requireOneRow(result, "stage attempt", stage.ID); err != nil {
		return ReviewGateOpenResult{}, err
	}
	run.Status = WorkflowRunWaitingReview
	if run.StartedAt == nil {
		run.StartedAt = &now
	}
	run.Version++
	result, err = tx.ExecContext(ctx, `
		UPDATE workflow_runs SET status = ?, started_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, run.Status, run.StartedAt, run.Version, run.ID, prepared.expectedRunVersion)
	if err != nil {
		return ReviewGateOpenResult{}, err
	}
	if err := requireOneRow(result, "workflow run", run.ID); err != nil {
		return ReviewGateOpenResult{}, err
	}
	if err := appendReviewGateOpenAudits(ctx, s, tx, binding, review, node, stage, run, prepared.actor, prepared.reason, now); err != nil {
		return ReviewGateOpenResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReviewGateOpenResult{}, err
	}
	return ReviewGateOpenResult{Binding: binding, Review: review, Run: run, StageAttempt: stage, NodeAttempt: node}, nil
}

// RecordReviewGateDecision is deliberately separate from RecordReviewDecision:
// a gate decision must enqueue exactly one local resolution job, while an
// ordinary lifecycle review must not acquire workflow execution semantics.
func (s *Store) RecordReviewGateDecision(ctx context.Context, request RecordReviewGateDecisionRequest) (ReviewGateDecisionResult, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ReviewGateDecisionResult{}, err
	}
	prepared, err := prepareRecordReviewGateDecisionRequest(request)
	if err != nil {
		return ReviewGateDecisionResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewGateDecisionResult{}, err
	}
	defer tx.Rollback()
	state, err := loadReviewGateStateTx(ctx, tx, prepared.reviewRequestID)
	if err != nil {
		return ReviewGateDecisionResult{}, err
	}
	if err := prepared.matchesBinding(state.Binding); err != nil {
		return ReviewGateDecisionResult{}, err
	}
	if existing, err := getReviewDecisionForRequestTx(ctx, tx, state.Review.ID); err != nil {
		return ReviewGateDecisionResult{}, err
	} else if existing != nil {
		result, err := replayReviewGateDecisionTx(ctx, tx, state.Binding, *existing, prepared)
		if err != nil {
			return ReviewGateDecisionResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return ReviewGateDecisionResult{}, err
		}
		return result, nil
	}
	if state.Review.State != "open" {
		return ReviewGateDecisionResult{}, fmt.Errorf("%w: review request %s is %s", ErrImmutable, state.Review.ID, state.Review.State)
	}
	if state.Run.Version != prepared.expectedRunVersion {
		return ReviewGateDecisionResult{}, fmt.Errorf("%w: workflow run %s", ErrOptimisticLock, state.Run.ID)
	}
	if state.Stage.Version != prepared.expectedStageAttemptVersion {
		return ReviewGateDecisionResult{}, fmt.Errorf("%w: stage attempt %s", ErrOptimisticLock, state.Stage.ID)
	}
	if state.Run.Status != WorkflowRunWaitingReview || state.Stage.ExecutionStatus != StageExecutionWaiting || state.Node.Status != NodeAttemptWaiting {
		return ReviewGateDecisionResult{}, fmt.Errorf("%w: review gate is not waiting for a decision", ErrInvalidTransition)
	}

	decisionID := prepared.decisionID
	if decisionID == "" {
		decisionID, err = s.newV2ID("")
		if err != nil {
			return ReviewGateDecisionResult{}, err
		}
	}
	jobID := prepared.resolutionJobID
	if jobID == "" {
		jobID, err = s.newV2ID("")
		if err != nil {
			return ReviewGateDecisionResult{}, err
		}
	}
	now := s.now().UTC()
	decision := ReviewDecision{
		ID:                     decisionID,
		ReviewRequestID:        state.Review.ID,
		RevisionID:             state.Revision.ID,
		Action:                 prepared.action,
		ExpectedRevisionDigest: state.Revision.TaskDigest,
		Actor:                  resolveActor(prepared.actor),
		Reason:                 prepared.reason,
		CreatedAt:              now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO review_decisions (id, review_request_id, revision_id, action, expected_revision_digest, actor, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, decision.ID, decision.ReviewRequestID, decision.RevisionID, decision.Action, decision.ExpectedRevisionDigest,
		decision.Actor, decision.Reason, decision.CreatedAt); err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return ReviewGateDecisionResult{}, fmt.Errorf("%w: review decision %s", ErrIdentityCollision, decision.ID)
		}
		return ReviewGateDecisionResult{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE review_requests SET state = 'closed', closed_at = ? WHERE id = ? AND state = 'open'`, now, state.Review.ID)
	if err != nil {
		return ReviewGateDecisionResult{}, err
	}
	if err := requireOneRow(result, "review request", state.Review.ID); err != nil {
		return ReviewGateDecisionResult{}, err
	}
	job := DurableJob{
		ID:             jobID,
		CommandType:    ReviewGateResolutionCommandType,
		EntityType:     reviewGateResolutionEntityType,
		EntityID:       state.Stage.ID,
		RunID:          state.Run.ID,
		StageAttemptID: state.Stage.ID,
		State:          JobQueued,
		Priority:       prepared.resolutionPriority,
		PayloadJSON:    prepared.resolutionPayloadJSON,
		IdempotencyKey: ReviewGateResolutionJobKey(state.Stage.ID, decision.ID),
		CreatedBy:      decision.Actor,
		CreatedAt:      now,
		UpdatedAt:      now,
		Version:        1,
	}
	if err := insertReviewGateResolutionJobTx(ctx, tx, job); err != nil {
		return ReviewGateDecisionResult{}, err
	}
	if err := appendReviewGateDecisionAudits(ctx, s, tx, state.Binding, decision, job, prepared.actor, prepared.reason, now); err != nil {
		return ReviewGateDecisionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReviewGateDecisionResult{}, err
	}
	return ReviewGateDecisionResult{Binding: state.Binding, Decision: decision, ResolutionJob: job}, nil
}

// CompleteReviewGateResolution verifies the artifact lineage materialized by
// the resolution worker and atomically projects the gate's terminal outcome.
// The run intentionally remains waiting_review; the runtime owns the next
// coordinator or terminal run projection after this durable predecessor fact.
func (s *Store) CompleteReviewGateResolution(ctx context.Context, request CompleteReviewGateResolutionRequest) (ReviewGateResolutionResult, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ReviewGateResolutionResult{}, err
	}
	prepared, err := prepareCompleteReviewGateResolutionRequest(request)
	if err != nil {
		return ReviewGateResolutionResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewGateResolutionResult{}, err
	}
	defer tx.Rollback()
	state, err := loadReviewGateStateTx(ctx, tx, prepared.reviewRequestID)
	if err != nil {
		return ReviewGateResolutionResult{}, err
	}
	if state.Run.ID != prepared.runID || state.Stage.ID != prepared.stageAttemptID {
		return ReviewGateResolutionResult{}, fmt.Errorf("%w: review gate resolution does not match frozen run/stage", ErrImmutable)
	}
	decision, err := getReviewDecisionTx(ctx, tx, prepared.reviewDecisionID)
	if err != nil {
		return ReviewGateResolutionResult{}, err
	}
	if decision.ReviewRequestID != state.Review.ID || decision.RevisionID != state.Revision.ID || decision.ExpectedRevisionDigest != state.Binding.RevisionDigest {
		return ReviewGateResolutionResult{}, fmt.Errorf("%w: review decision does not match gate binding", ErrImmutable)
	}
	if state.Review.State != "closed" {
		return ReviewGateResolutionResult{}, fmt.Errorf("%w: review gate decision is not closed", ErrInvalidTransition)
	}
	if _, err := getDurableJobByIdempotencyTx(ctx, tx, ReviewGateResolutionJobKey(state.Stage.ID, decision.ID)); err != nil {
		return ReviewGateResolutionResult{}, fmt.Errorf("review gate resolution job: %w", err)
	}
	if err := validateReviewGateResolutionManifestTx(ctx, tx, prepared.artifactManifestID, state.Binding); err != nil {
		return ReviewGateResolutionResult{}, err
	}
	verdict, err := reviewGateVerdict(decision.Action)
	if err != nil {
		return ReviewGateResolutionResult{}, err
	}
	if state.Stage.ExecutionStatus == StageExecutionCompleted {
		if state.Stage.Verdict != verdict || state.Stage.ArtifactManifestID != prepared.artifactManifestID || state.Node.Status != NodeAttemptCompleted {
			return ReviewGateResolutionResult{}, fmt.Errorf("%w: review gate %s was already resolved differently", ErrIdempotencyConflict, state.Stage.ID)
		}
		if err := tx.Commit(); err != nil {
			return ReviewGateResolutionResult{}, err
		}
		return ReviewGateResolutionResult{Binding: state.Binding, Decision: decision, Run: state.Run, StageAttempt: state.Stage, NodeAttempt: state.Node}, nil
	}
	if state.Run.Version != prepared.expectedRunVersion {
		return ReviewGateResolutionResult{}, fmt.Errorf("%w: workflow run %s", ErrOptimisticLock, state.Run.ID)
	}
	if state.Stage.Version != prepared.expectedStageAttemptVersion {
		return ReviewGateResolutionResult{}, fmt.Errorf("%w: stage attempt %s", ErrOptimisticLock, state.Stage.ID)
	}
	if state.Node.Version != prepared.expectedNodeAttemptVersion {
		return ReviewGateResolutionResult{}, fmt.Errorf("%w: node attempt %s", ErrOptimisticLock, state.Node.ID)
	}
	if state.Run.Status != WorkflowRunWaitingReview || state.Stage.ExecutionStatus != StageExecutionWaiting || state.Node.Status != NodeAttemptWaiting {
		return ReviewGateResolutionResult{}, fmt.Errorf("%w: review gate is not waiting for resolution", ErrInvalidTransition)
	}

	now := s.now().UTC()
	state.Node.Status = NodeAttemptCompleted
	state.Node.FinishedAt = &now
	state.Node.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE node_attempts SET status = ?, finished_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, state.Node.Status, state.Node.FinishedAt, state.Node.Version, state.Node.ID, prepared.expectedNodeAttemptVersion)
	if err != nil {
		return ReviewGateResolutionResult{}, err
	}
	if err := requireOneRow(result, "node attempt", state.Node.ID); err != nil {
		return ReviewGateResolutionResult{}, err
	}
	state.Stage.ExecutionStatus = StageExecutionCompleted
	state.Stage.Verdict = verdict
	state.Stage.ArtifactManifestID = prepared.artifactManifestID
	state.Stage.FinishedAt = &now
	state.Stage.Version++
	result, err = tx.ExecContext(ctx, `
		UPDATE stage_attempts
		SET execution_status = ?, verdict = ?, artifact_manifest_id = ?, finished_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, state.Stage.ExecutionStatus, state.Stage.Verdict, state.Stage.ArtifactManifestID, state.Stage.FinishedAt,
		state.Stage.Version, state.Stage.ID, prepared.expectedStageAttemptVersion)
	if err != nil {
		return ReviewGateResolutionResult{}, err
	}
	if err := requireOneRow(result, "stage attempt", state.Stage.ID); err != nil {
		return ReviewGateResolutionResult{}, err
	}
	if err := appendReviewGateResolutionAudits(ctx, s, tx, state.Binding, decision, state.Node, state.Stage, prepared.actor, prepared.reason, now); err != nil {
		return ReviewGateResolutionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReviewGateResolutionResult{}, err
	}
	return ReviewGateResolutionResult{Binding: state.Binding, Decision: decision, Run: state.Run, StageAttempt: state.Stage, NodeAttempt: state.Node}, nil
}

type preparedOpenReviewGate struct {
	reviewRequestID             string
	nodeAttemptID               string
	runID                       string
	expectedRunVersion          int64
	revisionID                  string
	revisionDigest              string
	definitionHash              string
	stageAttemptID              string
	expectedStageAttemptVersion int64
	stageKey                    string
	reviewKind                  string
	nodeGeneration              int
	nodeAttempt                 int
	inputBindingsJSON           string
	inputFingerprint            string
	evidenceManifestDigest      string
	actor                       string
	reason                      string
}

func prepareOpenReviewGateRequest(request OpenReviewGateRequest) (preparedOpenReviewGate, error) {
	reviewRequestID, err := normalizeOptionalReviewGateID(request.ReviewRequestID)
	if err != nil {
		return preparedOpenReviewGate{}, err
	}
	nodeAttemptID, err := normalizeOptionalReviewGateID(request.NodeAttemptID)
	if err != nil {
		return preparedOpenReviewGate{}, err
	}
	if !isUUIDv7(request.RunID) || !isUUIDv7(request.RevisionID) || !isUUIDv7(request.StageAttemptID) {
		return preparedOpenReviewGate{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedRunVersion <= 0 || request.ExpectedStageAttemptVersion <= 0 {
		return preparedOpenReviewGate{}, fmt.Errorf("expected workflow run and stage attempt versions must be positive")
	}
	if request.NodeGeneration < 0 || request.NodeAttempt <= 0 {
		return preparedOpenReviewGate{}, fmt.Errorf("review gate node generation must be non-negative and attempt must be positive")
	}
	revisionDigest, err := normalizeRequired(request.RevisionDigest, "review gate revision digest")
	if err != nil {
		return preparedOpenReviewGate{}, err
	}
	definitionHash, err := normalizeRequired(request.DefinitionHash, "review gate definition hash")
	if err != nil {
		return preparedOpenReviewGate{}, err
	}
	stageKey, err := normalizeRequired(request.StageKey, "review gate stage key")
	if err != nil {
		return preparedOpenReviewGate{}, err
	}
	reviewKind, err := normalizeRequired(request.ReviewKind, "review gate kind")
	if err != nil {
		return preparedOpenReviewGate{}, err
	}
	inputBindingsJSON, err := normalizeV4JSON(request.InputBindingsJSON, "review gate input bindings")
	if err != nil {
		return preparedOpenReviewGate{}, err
	}
	inputFingerprint, err := normalizeRequired(request.InputFingerprint, "review gate input fingerprint")
	if err != nil {
		return preparedOpenReviewGate{}, err
	}
	evidenceManifestDigest, err := normalizeRequired(request.EvidenceManifestDigest, "review gate evidence manifest digest")
	if err != nil {
		return preparedOpenReviewGate{}, err
	}
	return preparedOpenReviewGate{
		reviewRequestID: reviewRequestID, nodeAttemptID: nodeAttemptID,
		runID: request.RunID, expectedRunVersion: request.ExpectedRunVersion,
		revisionID: request.RevisionID, revisionDigest: revisionDigest, definitionHash: definitionHash,
		stageAttemptID: request.StageAttemptID, expectedStageAttemptVersion: request.ExpectedStageAttemptVersion,
		stageKey: stageKey, reviewKind: reviewKind, nodeGeneration: request.NodeGeneration, nodeAttempt: request.NodeAttempt,
		inputBindingsJSON: inputBindingsJSON, inputFingerprint: inputFingerprint, evidenceManifestDigest: evidenceManifestDigest,
		actor: request.Actor, reason: strings.TrimSpace(request.Reason),
	}, nil
}

func (request preparedOpenReviewGate) matchesBinding(binding ReviewGateBinding, node NodeAttempt) error {
	if binding.RunID != request.runID || binding.RevisionID != request.revisionID || binding.RevisionDigest != request.revisionDigest ||
		binding.DefinitionHash != request.definitionHash || binding.StageAttemptID != request.stageAttemptID || binding.StageKey != request.stageKey ||
		binding.ReviewKind != request.reviewKind || binding.InputBindingsJSON != request.inputBindingsJSON ||
		binding.InputFingerprint != request.inputFingerprint || binding.EvidenceManifestDigest != request.evidenceManifestDigest ||
		(request.reviewRequestID != "" && binding.ReviewRequestID != request.reviewRequestID) ||
		(request.nodeAttemptID != "" && binding.NodeAttemptID != request.nodeAttemptID) ||
		node.Generation != request.nodeGeneration || node.Attempt != request.nodeAttempt {
		return fmt.Errorf("%w: review gate open request differs from immutable binding", ErrIdempotencyConflict)
	}
	return nil
}

type preparedReviewGateDecision struct {
	decisionID                  string
	reviewRequestID             string
	runID                       string
	revisionID                  string
	stageAttemptID              string
	expectedRevisionDigest      string
	expectedRunVersion          int64
	expectedStageAttemptVersion int64
	action                      ReviewDecisionAction
	resolutionJobID             string
	resolutionPayloadJSON       string
	resolutionPriority          int
	actor                       string
	reason                      string
}

func prepareRecordReviewGateDecisionRequest(request RecordReviewGateDecisionRequest) (preparedReviewGateDecision, error) {
	decisionID, err := normalizeOptionalReviewGateID(request.ID)
	if err != nil {
		return preparedReviewGateDecision{}, err
	}
	resolutionJobID, err := normalizeOptionalReviewGateID(request.ResolutionJobID)
	if err != nil {
		return preparedReviewGateDecision{}, err
	}
	if !isUUIDv7(request.ReviewRequestID) || !isUUIDv7(request.RunID) || !isUUIDv7(request.RevisionID) || !isUUIDv7(request.StageAttemptID) {
		return preparedReviewGateDecision{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedRunVersion <= 0 || request.ExpectedStageAttemptVersion <= 0 {
		return preparedReviewGateDecision{}, fmt.Errorf("expected workflow run and stage attempt versions must be positive")
	}
	if !validReviewDecisionAction(request.Action) {
		return preparedReviewGateDecision{}, fmt.Errorf("invalid review decision action %q", request.Action)
	}
	expectedRevisionDigest, err := normalizeRequired(request.ExpectedRevisionDigest, "expected review gate revision digest")
	if err != nil {
		return preparedReviewGateDecision{}, err
	}
	payload, err := normalizeJSON(request.ResolutionPayloadJSON, "review gate resolution payload")
	if err != nil {
		return preparedReviewGateDecision{}, err
	}
	return preparedReviewGateDecision{
		decisionID: decisionID, reviewRequestID: request.ReviewRequestID, runID: request.RunID, revisionID: request.RevisionID,
		stageAttemptID: request.StageAttemptID, expectedRevisionDigest: expectedRevisionDigest,
		expectedRunVersion: request.ExpectedRunVersion, expectedStageAttemptVersion: request.ExpectedStageAttemptVersion,
		action: request.Action, resolutionJobID: resolutionJobID, resolutionPayloadJSON: payload,
		resolutionPriority: request.ResolutionPriority, actor: request.Actor, reason: strings.TrimSpace(request.Reason),
	}, nil
}

func (request preparedReviewGateDecision) matchesBinding(binding ReviewGateBinding) error {
	if binding.ReviewRequestID != request.reviewRequestID || binding.RunID != request.runID || binding.RevisionID != request.revisionID ||
		binding.StageAttemptID != request.stageAttemptID || binding.RevisionDigest != request.expectedRevisionDigest {
		return fmt.Errorf("%w: review gate decision does not match immutable binding", ErrImmutable)
	}
	return nil
}

type preparedReviewGateResolution struct {
	reviewRequestID             string
	reviewDecisionID            string
	runID                       string
	stageAttemptID              string
	expectedRunVersion          int64
	expectedStageAttemptVersion int64
	expectedNodeAttemptVersion  int64
	artifactManifestID          string
	actor                       string
	reason                      string
}

func prepareCompleteReviewGateResolutionRequest(request CompleteReviewGateResolutionRequest) (preparedReviewGateResolution, error) {
	if !isUUIDv7(request.ReviewRequestID) || !isUUIDv7(request.ReviewDecisionID) || !isUUIDv7(request.RunID) || !isUUIDv7(request.StageAttemptID) {
		return preparedReviewGateResolution{}, ErrInvalidUUIDv7Identity
	}
	artifactManifestID, err := requireV4ID(request.ArtifactManifestID, "review gate resolution artifact manifest ID")
	if err != nil {
		return preparedReviewGateResolution{}, err
	}
	if request.ExpectedRunVersion <= 0 || request.ExpectedStageAttemptVersion <= 0 || request.ExpectedNodeAttemptVersion <= 0 {
		return preparedReviewGateResolution{}, fmt.Errorf("expected workflow run, stage attempt, and node attempt versions must be positive")
	}
	return preparedReviewGateResolution{
		reviewRequestID: request.ReviewRequestID, reviewDecisionID: request.ReviewDecisionID,
		runID: request.RunID, stageAttemptID: request.StageAttemptID,
		expectedRunVersion: request.ExpectedRunVersion, expectedStageAttemptVersion: request.ExpectedStageAttemptVersion,
		expectedNodeAttemptVersion: request.ExpectedNodeAttemptVersion, artifactManifestID: artifactManifestID,
		actor: request.Actor, reason: strings.TrimSpace(request.Reason),
	}, nil
}

type reviewGateState struct {
	Binding  ReviewGateBinding
	Review   ReviewRequest
	Run      WorkflowRun
	Revision TaskRevision
	Stage    StageAttempt
	Node     NodeAttempt
}

func loadReviewGateStateTx(ctx context.Context, tx *sql.Tx, reviewRequestID string) (reviewGateState, error) {
	binding, err := getReviewGateBindingByReviewRequestTx(ctx, tx, reviewRequestID)
	if err != nil {
		return reviewGateState{}, err
	}
	if binding == nil {
		return reviewGateState{}, fmt.Errorf("%w: review request %s is not a review gate", ErrNotFound, reviewRequestID)
	}
	review, err := getReviewRequestTx(ctx, tx, binding.ReviewRequestID)
	if err != nil {
		return reviewGateState{}, err
	}
	run, err := getWorkflowRunTx(ctx, tx, binding.RunID)
	if err != nil {
		return reviewGateState{}, err
	}
	revision, err := getTaskRevisionTx(ctx, tx, binding.RevisionID)
	if err != nil {
		return reviewGateState{}, err
	}
	stage, err := getStageAttemptTx(ctx, tx, binding.StageAttemptID)
	if err != nil {
		return reviewGateState{}, err
	}
	node, err := getNodeAttemptTx(ctx, tx, binding.NodeAttemptID)
	if err != nil {
		return reviewGateState{}, err
	}
	if review.RevisionID != binding.RevisionID || review.EvidenceManifestDigest != binding.EvidenceManifestDigest ||
		run.RevisionID != binding.RevisionID || revision.TaskID != run.TaskID || revision.TaskDigest != binding.RevisionDigest ||
		run.DefinitionHash != binding.DefinitionHash || stage.RunID != binding.RunID || stage.StageKey != binding.StageKey ||
		stage.InputFingerprint != binding.InputFingerprint || node.StageAttemptID != binding.StageAttemptID || node.NodeID != binding.StageKey {
		return reviewGateState{}, fmt.Errorf("%w: persisted review gate binding has inconsistent lineage", ErrImmutable)
	}
	return reviewGateState{Binding: *binding, Review: review, Run: run, Revision: revision, Stage: stage, Node: node}, nil
}

func (s *Store) replayOpenedReviewGateTx(ctx context.Context, tx *sql.Tx, binding ReviewGateBinding, request preparedOpenReviewGate) (ReviewGateOpenResult, error) {
	state, err := loadReviewGateStateTx(ctx, tx, binding.ReviewRequestID)
	if err != nil {
		return ReviewGateOpenResult{}, err
	}
	if err := request.matchesBinding(state.Binding, state.Node); err != nil {
		return ReviewGateOpenResult{}, err
	}
	return ReviewGateOpenResult{Binding: state.Binding, Review: state.Review, Run: state.Run, StageAttempt: state.Stage, NodeAttempt: state.Node}, nil
}

func replayReviewGateDecisionTx(ctx context.Context, tx *sql.Tx, binding ReviewGateBinding, decision ReviewDecision, request preparedReviewGateDecision) (ReviewGateDecisionResult, error) {
	if (request.decisionID != "" && decision.ID != request.decisionID) || decision.Action != request.action ||
		decision.ExpectedRevisionDigest != request.expectedRevisionDigest || decision.Actor != resolveActor(request.actor) || decision.Reason != request.reason {
		return ReviewGateDecisionResult{}, fmt.Errorf("%w: review gate decision replay differs from stored decision", ErrIdempotencyConflict)
	}
	job, err := getDurableJobByIdempotencyTx(ctx, tx, ReviewGateResolutionJobKey(binding.StageAttemptID, decision.ID))
	if err != nil {
		return ReviewGateDecisionResult{}, fmt.Errorf("review gate resolution job: %w", err)
	}
	if (request.resolutionJobID != "" && job.ID != request.resolutionJobID) || job.CommandType != ReviewGateResolutionCommandType ||
		job.EntityType != reviewGateResolutionEntityType || job.EntityID != binding.StageAttemptID || job.RunID != binding.RunID ||
		job.StageAttemptID != binding.StageAttemptID || job.Priority != request.resolutionPriority || job.PayloadJSON != request.resolutionPayloadJSON ||
		job.CreatedBy != decision.Actor {
		return ReviewGateDecisionResult{}, fmt.Errorf("%w: review gate resolution replay differs from stored job", ErrIdempotencyConflict)
	}
	return ReviewGateDecisionResult{Binding: binding, Decision: decision, ResolutionJob: job}, nil
}

func insertReviewGateResolutionJobTx(ctx context.Context, tx *sql.Tx, job DurableJob) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO jobs (
			id, command_type, entity_type, entity_id, run_id, stage_attempt_id, state,
			priority, payload_json, idempotency_key, created_by, created_at, updated_at,
			started_at, finished_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)
	`, job.ID, job.CommandType, job.EntityType, job.EntityID, job.RunID, job.StageAttemptID, job.State,
		job.Priority, job.PayloadJSON, job.IdempotencyKey, job.CreatedBy, job.CreatedAt, job.UpdatedAt, job.Version)
	if err == nil {
		return nil
	}
	if isGlobalIdentityCollision(err) {
		return fmt.Errorf("%w: review gate resolution job %s", ErrIdentityCollision, job.ID)
	}
	if !isUniqueConstraint(err) {
		return err
	}
	existing, existingErr := getDurableJobByIdempotencyTx(ctx, tx, job.IdempotencyKey)
	if existingErr != nil {
		return existingErr
	}
	if !sameDurableJobRequest(existing, job) || existing.ID != job.ID {
		return fmt.Errorf("%w: review gate resolution job %s", ErrIdempotencyConflict, job.IdempotencyKey)
	}
	return nil
}

func validateReviewGateResolutionManifestTx(ctx context.Context, tx *sql.Tx, manifestID string, binding ReviewGateBinding) error {
	manifest, err := getArtifactManifestTx(ctx, tx, manifestID)
	if err != nil {
		return fmt.Errorf("review gate resolution manifest: %w", err)
	}
	if manifest.SubjectRevisionID != binding.RevisionID || manifest.SubjectDigest != binding.RevisionDigest || manifest.WorkflowFingerprint != binding.DefinitionHash {
		return fmt.Errorf("%w: review gate resolution manifest does not match frozen revision/run lineage", ErrImmutable)
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
			reference.SubjectRevisionID != binding.RevisionID || reference.SubjectDigest != binding.RevisionDigest ||
			reference.WorkflowFingerprint != binding.DefinitionHash || reference.InputBindingsJSON != binding.InputBindingsJSON ||
			reference.InputFingerprint != binding.InputFingerprint {
			return fmt.Errorf("%w: review gate resolution artifact ref %s does not match frozen lineage", ErrImmutable, reference.ID)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("%w: review gate resolution manifest has no typed artifact refs", ErrImmutable)
	}
	return nil
}

func getReviewGateBindingByReviewRequestTx(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, reviewRequestID string) (*ReviewGateBinding, error) {
	binding, err := scanReviewGateBinding(queryer.QueryRowContext(ctx, reviewGateBindingV15Select+" WHERE review_request_id = ?", reviewRequestID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &binding, nil
}

func getReviewGateBindingByStageAttemptTx(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, stageAttemptID string) (*ReviewGateBinding, error) {
	binding, err := scanReviewGateBinding(queryer.QueryRowContext(ctx, reviewGateBindingV15Select+" WHERE stage_attempt_id = ?", stageAttemptID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &binding, nil
}

func getReviewRequestTx(ctx context.Context, tx *sql.Tx, reviewRequestID string) (ReviewRequest, error) {
	review, err := scanReviewRequest(tx.QueryRowContext(ctx, reviewRequestSelect+" WHERE id = ?", reviewRequestID))
	if err == sql.ErrNoRows {
		return ReviewRequest{}, fmt.Errorf("%w: review request %s", ErrNotFound, reviewRequestID)
	}
	return review, err
}

func getReviewDecisionForRequestTx(ctx context.Context, tx *sql.Tx, reviewRequestID string) (*ReviewDecision, error) {
	rows, err := tx.QueryContext(ctx, reviewDecisionSelect+" WHERE review_request_id = ? ORDER BY created_at ASC, id ASC", reviewRequestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	decision, err := scanReviewDecision(rows)
	if err != nil {
		return nil, err
	}
	if rows.Next() {
		return nil, fmt.Errorf("%w: review request %s has multiple decisions", ErrImmutable, reviewRequestID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &decision, nil
}

func getReviewDecisionTx(ctx context.Context, tx *sql.Tx, decisionID string) (ReviewDecision, error) {
	decision, err := scanReviewDecision(tx.QueryRowContext(ctx, reviewDecisionSelect+" WHERE id = ?", decisionID))
	if err == sql.ErrNoRows {
		return ReviewDecision{}, fmt.Errorf("%w: review decision %s", ErrNotFound, decisionID)
	}
	return decision, err
}

func scanReviewGateBinding(scanner rowScanner) (ReviewGateBinding, error) {
	var binding ReviewGateBinding
	if err := scanner.Scan(
		&binding.StageAttemptID, &binding.ReviewRequestID, &binding.RunID, &binding.RevisionID, &binding.RevisionDigest,
		&binding.DefinitionHash, &binding.StageKey, &binding.ReviewKind, &binding.NodeAttemptID,
		&binding.InputBindingsJSON, &binding.InputFingerprint, &binding.EvidenceManifestDigest, &binding.CreatedAt,
	); err != nil {
		return ReviewGateBinding{}, err
	}
	binding.CreatedAt = binding.CreatedAt.UTC()
	return binding, nil
}

func reviewGateVerdict(action ReviewDecisionAction) (Verdict, error) {
	switch action {
	case ReviewDecisionApprove:
		return VerdictPass, nil
	case ReviewDecisionRequestChanges:
		return VerdictNeedsRepair, nil
	case ReviewDecisionRejectTerminal:
		return VerdictReject, nil
	default:
		return "", fmt.Errorf("invalid review gate decision action %q", action)
	}
}

func reviewGateNodeAttemptKey(stageAttemptID string) string {
	return "review-gate-node:" + stageAttemptID
}

func normalizeOptionalReviewGateID(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	if !isUUIDv7(value) {
		return "", ErrInvalidUUIDv7Identity
	}
	return value, nil
}

func requireOneRow(result sql.Result, entityType, entityID string) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: %s %s", ErrOptimisticLock, entityType, entityID)
	}
	return nil
}

func appendReviewGateOpenAudits(ctx context.Context, s *Store, tx *sql.Tx, binding ReviewGateBinding, review ReviewRequest, node NodeAttempt, stage StageAttempt, run WorkflowRun, actor, reason string, now time.Time) error {
	events := []AuditEvent{
		{
			Actor: actor, EntityType: "review_request", EntityID: review.ID, Action: "review.requested", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"revision_id": review.RevisionID, "evidence_manifest_digest": review.EvidenceManifestDigest, "gate_stage_attempt_id": binding.StageAttemptID}), CreatedAt: now,
		},
		{
			Actor: actor, EntityType: "review_gate_binding", EntityID: binding.StageAttemptID, Action: "review_gate.opened", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"review_request_id": binding.ReviewRequestID, "run_id": binding.RunID, "revision_id": binding.RevisionID, "definition_hash": binding.DefinitionHash, "stage_key": binding.StageKey, "review_kind": binding.ReviewKind, "input_fingerprint": binding.InputFingerprint}), CreatedAt: now,
		},
		{
			Actor: actor, EntityType: "node_attempt", EntityID: node.ID, Action: "node_attempt.waiting_for_review", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"stage_attempt_id": node.StageAttemptID, "status": node.Status, "version": node.Version}), CreatedAt: now,
		},
		{
			Actor: actor, EntityType: "stage_attempt", EntityID: stage.ID, Action: "stage_attempt.waiting_for_review", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"execution_status": stage.ExecutionStatus, "version": stage.Version, "review_request_id": binding.ReviewRequestID}), CreatedAt: now,
		},
		{
			Actor: actor, EntityType: "workflow_run", EntityID: run.ID, Action: "workflow_run.waiting_for_review", Reason: reason,
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

func appendReviewGateDecisionAudits(ctx context.Context, s *Store, tx *sql.Tx, binding ReviewGateBinding, decision ReviewDecision, job DurableJob, actor, reason string, now time.Time) error {
	events := []AuditEvent{
		{
			Actor: actor, EntityType: "review_decision", EntityID: decision.ID, Action: "review.decided", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"review_request_id": decision.ReviewRequestID, "revision_id": decision.RevisionID, "action": decision.Action, "gate_stage_attempt_id": binding.StageAttemptID}), CreatedAt: now,
		},
		{
			Actor: actor, EntityType: "job", EntityID: job.ID, Action: "job.created", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"command_type": job.CommandType, "entity_type": job.EntityType, "entity_id": job.EntityID, "idempotency_key": job.IdempotencyKey, "review_decision_id": decision.ID}), CreatedAt: now,
		},
	}
	for _, event := range events {
		if _, err := s.appendAuditTx(ctx, tx, event); err != nil {
			return err
		}
	}
	return nil
}

func appendReviewGateResolutionAudits(ctx context.Context, s *Store, tx *sql.Tx, binding ReviewGateBinding, decision ReviewDecision, node NodeAttempt, stage StageAttempt, actor, reason string, now time.Time) error {
	events := []AuditEvent{
		{
			Actor: actor, EntityType: "node_attempt", EntityID: node.ID, Action: "node_attempt.completed_for_review", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"stage_attempt_id": node.StageAttemptID, "status": node.Status, "version": node.Version, "review_decision_id": decision.ID}), CreatedAt: now,
		},
		{
			Actor: actor, EntityType: "stage_attempt", EntityID: stage.ID, Action: "review_gate.resolved", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"review_request_id": binding.ReviewRequestID, "review_decision_id": decision.ID, "execution_status": stage.ExecutionStatus, "verdict": stage.Verdict, "artifact_manifest_id": stage.ArtifactManifestID, "version": stage.Version}), CreatedAt: now,
		},
	}
	for _, event := range events {
		if _, err := s.appendAuditTx(ctx, tx, event); err != nil {
			return err
		}
	}
	return nil
}
