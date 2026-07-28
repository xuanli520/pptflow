package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const agentTurnTranscriptSelect = `
	SELECT id, node_attempt_id, turn, response_text, response_sha256,
	       response_bytes, model_id, submission_status,
	       protocol_rejection_code, failure_code, created_at, expires_at,
	       expired_at, version
	FROM agent_turn_transcripts`

const agentTurnTranscriptSubmissionSelect = `
	SELECT id, transcript_id, ordinal, status, raw_request_json,
	       validation_json, receipt_json, rejection_code, created_at,
	       expired_at, version
	FROM agent_turn_transcript_submissions`

const agentTurnTranscriptLegalHoldSelect = `
	SELECT id, transcript_id, hold_key, reason, created_by, created_at,
	       released_by, release_reason, released_at, version
	FROM agent_turn_transcript_legal_holds`

type preparedAgentTurnTranscriptSubmission struct {
	id             string
	ordinal        int
	status         AgentTurnSubmissionStatus
	rawRequestJSON string
	validationJSON string
	receiptJSON    string
	rejectionCode  string
}

type preparedAgentTurnTranscript struct {
	id                    string
	nodeAttemptID         string
	turn                  int
	responseText          string
	responseSHA256        string
	responseBytes         int64
	modelID               string
	submissionStatus      AgentTurnSubmissionStatus
	protocolRejectionCode string
	failureCode           string
	occurredAt            time.Time
	actor                 string
	reason                string
	submissions           []preparedAgentTurnTranscriptSubmission
}

// RecordAgentTurnTranscriptWithCheckpoint creates and completes the
// turn_completed checkpoint together with the transcript. No caller can
// observe a completed turn checkpoint whose real model response is absent.
func (s *Store) RecordAgentTurnTranscriptWithCheckpoint(ctx context.Context, request RecordAgentTurnTranscriptWithCheckpointRequest) (RecordAgentTurnTranscriptWithCheckpointResult, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return RecordAgentTurnTranscriptWithCheckpointResult{}, err
	}
	if strings.TrimSpace(request.Transcript.Actor) == "" {
		request.Transcript.Actor = request.Actor
	}
	if strings.TrimSpace(request.Transcript.Reason) == "" {
		request.Transcript.Reason = request.Reason
	}
	prepared, err := prepareAgentTurnTranscriptRequest(s, request.Transcript)
	if err != nil {
		return RecordAgentTurnTranscriptWithCheckpointResult{}, err
	}
	checkpoint, err := prepareCompletedAgentTurnCheckpoint(s, request.Checkpoint, prepared)
	if err != nil {
		return RecordAgentTurnTranscriptWithCheckpointResult{}, err
	}
	tx, releaseFence, err := s.beginDispatchFenceTx(ctx)
	if err != nil {
		return RecordAgentTurnTranscriptWithCheckpointResult{}, err
	}
	defer tx.Rollback()
	defer releaseFence()

	transcript, replayed, err := s.recordAgentTurnTranscriptTx(ctx, tx, prepared)
	if err != nil {
		return RecordAgentTurnTranscriptWithCheckpointResult{}, err
	}
	existingCheckpoint, err := getTurnCheckpointByCoordinateTx(ctx, tx, checkpoint.NodeAttemptID, checkpoint.Turn, checkpoint.Substep)
	if err != nil {
		return RecordAgentTurnTranscriptWithCheckpointResult{}, err
	}
	if replayed {
		if existingCheckpoint == nil || !sameCompletedAgentTurnCheckpoint(*existingCheckpoint, checkpoint) {
			return RecordAgentTurnTranscriptWithCheckpointResult{}, fmt.Errorf("%w: transcript %s does not match completed checkpoint", ErrIdentityCollision, transcript.ID)
		}
		if err := tx.Commit(); err != nil {
			return RecordAgentTurnTranscriptWithCheckpointResult{}, err
		}
		return RecordAgentTurnTranscriptWithCheckpointResult{Transcript: transcript, Checkpoint: *existingCheckpoint, Replayed: true}, nil
	}
	if existingCheckpoint != nil {
		return RecordAgentTurnTranscriptWithCheckpointResult{}, fmt.Errorf("%w: turn checkpoint for node attempt %s turn %d", ErrIdentityCollision, checkpoint.NodeAttemptID, checkpoint.Turn)
	}
	if err := insertAndCompleteAgentTurnCheckpointTx(ctx, s, tx, checkpoint, prepared.actor, prepared.reason); err != nil {
		return RecordAgentTurnTranscriptWithCheckpointResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RecordAgentTurnTranscriptWithCheckpointResult{}, err
	}
	return RecordAgentTurnTranscriptWithCheckpointResult{Transcript: transcript, Checkpoint: checkpoint}, nil
}

func (s *Store) GetAgentTurnTranscript(ctx context.Context, transcriptID string) (*AgentTurnTranscript, error) {
	if !isUUIDv7(transcriptID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	transcript, err := scanAgentTurnTranscript(s.db.QueryRowContext(ctx, agentTurnTranscriptSelect+" WHERE id = ?", transcriptID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &transcript, nil
}

func (s *Store) ListAgentTurnTranscripts(ctx context.Context, nodeAttemptID string) ([]AgentTurnTranscript, error) {
	if !isUUIDv7(nodeAttemptID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	return s.listAgentTurnTranscripts(ctx, agentTurnTranscriptSelect+" WHERE node_attempt_id = ? ORDER BY turn ASC", nodeAttemptID)
}

// ListAgentTurnTranscriptsForStageAttempt is an audit projection only. The
// durable source of the stage relation remains node_attempts.
func (s *Store) ListAgentTurnTranscriptsForStageAttempt(ctx context.Context, stageAttemptID string) ([]AgentTurnTranscript, error) {
	if !isUUIDv7(stageAttemptID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	query := agentTurnTranscriptSelect + `
		WHERE node_attempt_id IN (
			SELECT id FROM node_attempts WHERE stage_attempt_id = ?
		)
		ORDER BY created_at ASC, id ASC`
	return s.listAgentTurnTranscripts(ctx, query, stageAttemptID)
}

func (s *Store) listAgentTurnTranscripts(ctx context.Context, query string, args ...any) ([]AgentTurnTranscript, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	transcripts := make([]AgentTurnTranscript, 0)
	for rows.Next() {
		transcript, err := scanAgentTurnTranscript(rows)
		if err != nil {
			return nil, err
		}
		transcripts = append(transcripts, transcript)
	}
	return transcripts, rows.Err()
}

func (s *Store) ListAgentTurnTranscriptSubmissions(ctx context.Context, transcriptID string) ([]AgentTurnTranscriptSubmission, error) {
	if !isUUIDv7(transcriptID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, agentTurnTranscriptSubmissionSelect+" WHERE transcript_id = ? ORDER BY ordinal ASC", transcriptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	submissions := make([]AgentTurnTranscriptSubmission, 0)
	for rows.Next() {
		submission, err := scanAgentTurnTranscriptSubmission(rows)
		if err != nil {
			return nil, err
		}
		submissions = append(submissions, submission)
	}
	return submissions, rows.Err()
}

// ExpireAgentTurnTranscript removes only retention-limited raw material. It
// refuses active work and unreleased legal holds, retaining an audit fact for
// both successful expiration and an explicit retention refusal.
func (s *Store) ExpireAgentTurnTranscript(ctx context.Context, request ExpireAgentTurnTranscriptRequest) (ExpireAgentTurnTranscriptResult, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ExpireAgentTurnTranscriptResult{}, err
	}
	if !isUUIDv7(request.TranscriptID) {
		return ExpireAgentTurnTranscriptResult{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return ExpireAgentTurnTranscriptResult{}, fmt.Errorf("expected transcript version must be positive")
	}
	tx, releaseFence, err := s.beginDispatchFenceTx(ctx)
	if err != nil {
		return ExpireAgentTurnTranscriptResult{}, err
	}
	defer tx.Rollback()
	defer releaseFence()
	transcript, err := getAgentTurnTranscriptTx(ctx, tx, request.TranscriptID)
	if err != nil {
		return ExpireAgentTurnTranscriptResult{}, err
	}
	if transcript.ExpiredAt != nil {
		if request.ExpectedVersion != transcript.Version && request.ExpectedVersion != transcript.Version-1 {
			return ExpireAgentTurnTranscriptResult{}, fmt.Errorf("%w: agent turn transcript %s", ErrOptimisticLock, transcript.ID)
		}
		if err := tx.Commit(); err != nil {
			return ExpireAgentTurnTranscriptResult{}, err
		}
		return ExpireAgentTurnTranscriptResult{Transcript: transcript, Replayed: true}, nil
	}
	if transcript.Version != request.ExpectedVersion {
		return ExpireAgentTurnTranscriptResult{}, fmt.Errorf("%w: agent turn transcript %s", ErrOptimisticLock, transcript.ID)
	}
	now := s.now().UTC()
	if transcript.ExpiresAt.After(now) {
		return ExpireAgentTurnTranscriptResult{}, fmt.Errorf("%w: agent turn transcript %s is not retention eligible", ErrInvalidTransition, transcript.ID)
	}
	block, err := agentTurnTranscriptExpiryBlockTx(ctx, tx, transcript)
	if err != nil {
		return ExpireAgentTurnTranscriptResult{}, err
	}
	if block != "" {
		if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
			Actor: request.Actor, EntityType: "agent_turn_transcript", EntityID: transcript.ID,
			Action: "agent_turn_transcript.expiry_blocked", Reason: request.Reason,
			PayloadJSON: auditPayload(map[string]any{"block": block, "version": transcript.Version}), CreatedAt: now,
		}); err != nil {
			return ExpireAgentTurnTranscriptResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return ExpireAgentTurnTranscriptResult{}, err
		}
		return ExpireAgentTurnTranscriptResult{Transcript: transcript, Block: block}, fmt.Errorf("%w: agent turn transcript %s is bound by %s", ErrTranscriptRetentionBlocked, transcript.ID, block)
	}
	previousVersion := transcript.Version
	expiredAt := now
	transcript.ResponseText = ""
	transcript.ExpiredAt = &expiredAt
	transcript.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE agent_turn_transcripts
		SET response_text = '', expired_at = ?, version = ?
		WHERE id = ? AND version = ? AND expired_at IS NULL
	`, transcript.ExpiredAt, transcript.Version, transcript.ID, previousVersion)
	if err != nil {
		return ExpireAgentTurnTranscriptResult{}, err
	}
	if err := requireOneAgentTurnTranscriptRow(result, transcript.ID); err != nil {
		return ExpireAgentTurnTranscriptResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_turn_transcript_submissions
		SET raw_request_json = '', validation_json = '', receipt_json = '',
		    expired_at = ?, version = version + 1
		WHERE transcript_id = ? AND expired_at IS NULL
	`, transcript.ExpiredAt, transcript.ID); err != nil {
		return ExpireAgentTurnTranscriptResult{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: request.Actor, EntityType: "agent_turn_transcript", EntityID: transcript.ID,
		Action: "agent_turn_transcript.expired", Reason: request.Reason,
		PayloadJSON: auditPayload(map[string]any{"response_sha256": transcript.ResponseSHA256, "response_bytes": transcript.ResponseBytes, "version": transcript.Version}), CreatedAt: now,
	}); err != nil {
		return ExpireAgentTurnTranscriptResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ExpireAgentTurnTranscriptResult{}, err
	}
	return ExpireAgentTurnTranscriptResult{Transcript: transcript, Expired: true}, nil
}

// SweepExpiredAgentTurnTranscripts is safe for periodic workers. A successful
// replay is reported as expired work while active and legal-retention records
// are returned separately for operator visibility.
func (s *Store) SweepExpiredAgentTurnTranscripts(ctx context.Context, request SweepExpiredAgentTurnTranscriptsRequest) (SweepExpiredAgentTurnTranscriptsResult, error) {
	if request.Limit < 0 {
		return SweepExpiredAgentTurnTranscriptsResult{}, fmt.Errorf("agent turn transcript retention limit cannot be negative")
	}
	query := agentTurnTranscriptSelect + `
		WHERE expires_at <= ? AND expired_at IS NULL
		ORDER BY expires_at ASC, id ASC`
	args := []any{s.now().UTC()}
	if request.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, request.Limit)
	}
	transcripts, err := s.listAgentTurnTranscripts(ctx, query, args...)
	if err != nil {
		return SweepExpiredAgentTurnTranscriptsResult{}, err
	}
	result := SweepExpiredAgentTurnTranscriptsResult{
		Expired: make([]ExpireAgentTurnTranscriptResult, 0, len(transcripts)),
		Blocked: make([]ExpireAgentTurnTranscriptResult, 0),
	}
	for _, transcript := range transcripts {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		expired, err := s.ExpireAgentTurnTranscript(ctx, ExpireAgentTurnTranscriptRequest{
			TranscriptID: transcript.ID, ExpectedVersion: transcript.Version, Actor: request.Actor, Reason: request.Reason,
		})
		if err == nil {
			result.Expired = append(result.Expired, expired)
			continue
		}
		if errors.Is(err, ErrTranscriptRetentionBlocked) {
			result.Blocked = append(result.Blocked, expired)
			continue
		}
		return result, err
	}
	return result, nil
}

func (s *Store) CreateAgentTurnTranscriptLegalHold(ctx context.Context, request CreateAgentTurnTranscriptLegalHoldRequest) (AgentTurnTranscriptLegalHold, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return AgentTurnTranscriptLegalHold{}, err
	}
	if !isUUIDv7(request.TranscriptID) {
		return AgentTurnTranscriptLegalHold{}, ErrInvalidUUIDv7Identity
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return AgentTurnTranscriptLegalHold{}, err
	}
	holdKey, err := normalizeRequired(request.HoldKey, "legal hold key")
	if err != nil {
		return AgentTurnTranscriptLegalHold{}, err
	}
	now := s.now().UTC()
	hold := AgentTurnTranscriptLegalHold{ID: id, TranscriptID: request.TranscriptID, HoldKey: holdKey, Reason: strings.TrimSpace(request.Reason), CreatedBy: resolveActor(request.Actor), CreatedAt: now, Version: 1}
	tx, releaseFence, err := s.beginDispatchFenceTx(ctx)
	if err != nil {
		return AgentTurnTranscriptLegalHold{}, err
	}
	defer tx.Rollback()
	defer releaseFence()
	if _, err := getAgentTurnTranscriptTx(ctx, tx, hold.TranscriptID); err != nil {
		return AgentTurnTranscriptLegalHold{}, err
	}
	existing, err := getAgentTurnTranscriptLegalHoldByKeyTx(ctx, tx, hold.TranscriptID, hold.HoldKey)
	if err != nil {
		return AgentTurnTranscriptLegalHold{}, err
	}
	if existing != nil {
		if existing.Reason != hold.Reason || existing.CreatedBy != hold.CreatedBy {
			return AgentTurnTranscriptLegalHold{}, fmt.Errorf("%w: legal hold %s", ErrIdempotencyConflict, hold.HoldKey)
		}
		if err := tx.Commit(); err != nil {
			return AgentTurnTranscriptLegalHold{}, err
		}
		return *existing, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agent_turn_transcript_legal_holds (
			id, transcript_id, hold_key, reason, created_by, created_at,
			released_by, release_reason, released_at, version
		) VALUES (?, ?, ?, ?, ?, ?, '', '', NULL, ?)
	`, hold.ID, hold.TranscriptID, hold.HoldKey, hold.Reason, hold.CreatedBy, hold.CreatedAt, hold.Version); err != nil {
		if isUniqueConstraint(err) {
			return AgentTurnTranscriptLegalHold{}, fmt.Errorf("%w: legal hold %s", ErrIdentityCollision, hold.ID)
		}
		return AgentTurnTranscriptLegalHold{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: hold.CreatedBy, EntityType: "agent_turn_transcript_legal_hold", EntityID: hold.ID,
		Action: "agent_turn_transcript.legal_hold_created", Reason: hold.Reason,
		PayloadJSON: auditPayload(map[string]any{"transcript_id": hold.TranscriptID, "hold_key": hold.HoldKey}), CreatedAt: now,
	}); err != nil {
		return AgentTurnTranscriptLegalHold{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentTurnTranscriptLegalHold{}, err
	}
	return hold, nil
}

func (s *Store) ReleaseAgentTurnTranscriptLegalHold(ctx context.Context, request ReleaseAgentTurnTranscriptLegalHoldRequest) (AgentTurnTranscriptLegalHold, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return AgentTurnTranscriptLegalHold{}, err
	}
	if !isUUIDv7(request.HoldID) {
		return AgentTurnTranscriptLegalHold{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return AgentTurnTranscriptLegalHold{}, fmt.Errorf("expected legal hold version must be positive")
	}
	releaseReason, err := normalizeRequired(request.Reason, "legal hold release reason")
	if err != nil {
		return AgentTurnTranscriptLegalHold{}, err
	}
	tx, releaseFence, err := s.beginDispatchFenceTx(ctx)
	if err != nil {
		return AgentTurnTranscriptLegalHold{}, err
	}
	defer tx.Rollback()
	defer releaseFence()
	hold, err := getAgentTurnTranscriptLegalHoldTx(ctx, tx, request.HoldID)
	if err != nil {
		return AgentTurnTranscriptLegalHold{}, err
	}
	if hold.ReleasedAt != nil {
		if hold.Version != request.ExpectedVersion && hold.Version-1 != request.ExpectedVersion {
			return AgentTurnTranscriptLegalHold{}, fmt.Errorf("%w: legal hold %s", ErrOptimisticLock, hold.ID)
		}
		if err := tx.Commit(); err != nil {
			return AgentTurnTranscriptLegalHold{}, err
		}
		return hold, nil
	}
	if hold.Version != request.ExpectedVersion {
		return AgentTurnTranscriptLegalHold{}, fmt.Errorf("%w: legal hold %s", ErrOptimisticLock, hold.ID)
	}
	now := s.now().UTC()
	hold.ReleasedBy = resolveActor(request.Actor)
	hold.ReleaseReason = releaseReason
	hold.ReleasedAt = &now
	hold.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE agent_turn_transcript_legal_holds
		SET released_by = ?, release_reason = ?, released_at = ?, version = ?
		WHERE id = ? AND version = ? AND released_at IS NULL
	`, hold.ReleasedBy, hold.ReleaseReason, hold.ReleasedAt, hold.Version, hold.ID, request.ExpectedVersion)
	if err != nil {
		return AgentTurnTranscriptLegalHold{}, err
	}
	if err := requireOneAgentTurnTranscriptRow(result, hold.ID); err != nil {
		return AgentTurnTranscriptLegalHold{}, fmt.Errorf("%w: legal hold %s", ErrOptimisticLock, hold.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: hold.ReleasedBy, EntityType: "agent_turn_transcript_legal_hold", EntityID: hold.ID,
		Action: "agent_turn_transcript.legal_hold_released", Reason: hold.ReleaseReason,
		PayloadJSON: auditPayload(map[string]any{"transcript_id": hold.TranscriptID, "hold_key": hold.HoldKey, "version": hold.Version}), CreatedAt: now,
	}); err != nil {
		return AgentTurnTranscriptLegalHold{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentTurnTranscriptLegalHold{}, err
	}
	return hold, nil
}

func prepareAgentTurnTranscriptRequest(s *Store, request CreateAgentTurnTranscriptRequest) (preparedAgentTurnTranscript, error) {
	if !isUUIDv7(request.NodeAttemptID) {
		return preparedAgentTurnTranscript{}, ErrInvalidUUIDv7Identity
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedAgentTurnTranscript{}, err
	}
	if request.Turn <= 0 {
		return preparedAgentTurnTranscript{}, fmt.Errorf("agent turn must be positive")
	}
	modelID, err := normalizeRequired(request.ModelID, "agent model ID")
	if err != nil {
		return preparedAgentTurnTranscript{}, err
	}
	status, err := normalizedAgentTurnSubmissionStatus(request.SubmissionStatus, len(request.Submissions))
	if err != nil {
		return preparedAgentTurnTranscript{}, err
	}
	occurredAt := request.OccurredAt.UTC()
	if occurredAt.IsZero() {
		occurredAt = s.now().UTC()
	}
	prepared := preparedAgentTurnTranscript{
		id: id, nodeAttemptID: request.NodeAttemptID, turn: request.Turn, responseText: request.ResponseText,
		responseSHA256: string(workflowkit.SHA256Fingerprint([]byte(request.ResponseText))), responseBytes: int64(len([]byte(request.ResponseText))),
		modelID: modelID, submissionStatus: status, protocolRejectionCode: strings.TrimSpace(request.ProtocolRejectionCode),
		failureCode: strings.TrimSpace(request.FailureCode), occurredAt: occurredAt, actor: request.Actor, reason: request.Reason,
		submissions: make([]preparedAgentTurnTranscriptSubmission, 0, len(request.Submissions)),
	}
	usedOrdinals := make(map[int]struct{}, len(request.Submissions))
	for index, item := range request.Submissions {
		ordinal := item.Ordinal
		if ordinal == 0 {
			ordinal = index + 1
		}
		if ordinal <= 0 {
			return preparedAgentTurnTranscript{}, fmt.Errorf("agent turn submission ordinal must be positive")
		}
		if _, exists := usedOrdinals[ordinal]; exists {
			return preparedAgentTurnTranscript{}, fmt.Errorf("agent turn submission ordinal %d is duplicated", ordinal)
		}
		usedOrdinals[ordinal] = struct{}{}
		submissionID, err := s.newV2ID(item.ID)
		if err != nil {
			return preparedAgentTurnTranscript{}, err
		}
		submissionStatus, err := normalizedAgentTurnSubmissionStatus(item.Status, 1)
		if err != nil || submissionStatus == AgentTurnSubmissionNotSubmitted {
			return preparedAgentTurnTranscript{}, fmt.Errorf("invalid agent turn submission status %q", item.Status)
		}
		prepared.submissions = append(prepared.submissions, preparedAgentTurnTranscriptSubmission{
			id: submissionID, ordinal: ordinal, status: submissionStatus, rawRequestJSON: item.RawRequestJSON,
			validationJSON: item.ValidationJSON, receiptJSON: item.ReceiptJSON, rejectionCode: strings.TrimSpace(item.RejectionCode),
		})
	}
	sort.Slice(prepared.submissions, func(left, right int) bool {
		return prepared.submissions[left].ordinal < prepared.submissions[right].ordinal
	})
	return prepared, nil
}

func normalizedAgentTurnSubmissionStatus(value AgentTurnSubmissionStatus, submissions int) (AgentTurnSubmissionStatus, error) {
	if value == "" && submissions == 0 {
		return AgentTurnSubmissionNotSubmitted, nil
	}
	if value == "" {
		return "", fmt.Errorf("agent turn submission status is required when submissions are present")
	}
	switch value {
	case AgentTurnSubmissionNotSubmitted:
		if submissions != 0 {
			return "", fmt.Errorf("not_submitted agent turn cannot contain submissions")
		}
	case AgentTurnSubmissionAccepted, AgentTurnSubmissionRejected, AgentTurnSubmissionRuntimeError:
	default:
		return "", fmt.Errorf("invalid agent turn submission status %q", value)
	}
	return value, nil
}

func (s *Store) recordAgentTurnTranscriptTx(ctx context.Context, tx *sql.Tx, prepared preparedAgentTurnTranscript) (AgentTurnTranscript, bool, error) {
	if _, err := getNodeAttemptTx(ctx, tx, prepared.nodeAttemptID); err != nil {
		return AgentTurnTranscript{}, false, err
	}
	existing, err := getAgentTurnTranscriptByCoordinateTx(ctx, tx, prepared.nodeAttemptID, prepared.turn)
	if err != nil {
		return AgentTurnTranscript{}, false, err
	}
	if existing != nil {
		submissions, err := listAgentTurnTranscriptSubmissionsTx(ctx, tx, existing.ID)
		if err != nil {
			return AgentTurnTranscript{}, false, err
		}
		if samePreparedAgentTurnTranscript(*existing, submissions, prepared) {
			return *existing, true, nil
		}
		if existing.ExpiredAt != nil {
			return AgentTurnTranscript{}, false, fmt.Errorf("%w: expired agent turn transcript %s", ErrImmutable, existing.ID)
		}
		return AgentTurnTranscript{}, false, fmt.Errorf("%w: agent turn transcript node attempt %s turn %d", ErrIdentityCollision, prepared.nodeAttemptID, prepared.turn)
	}
	expiresAt := prepared.occurredAt.Add(AgentTurnTranscriptRetention)
	transcript := AgentTurnTranscript{
		ID: prepared.id, NodeAttemptID: prepared.nodeAttemptID, Turn: prepared.turn, ResponseText: prepared.responseText,
		ResponseSHA256: prepared.responseSHA256, ResponseBytes: prepared.responseBytes, ModelID: prepared.modelID,
		SubmissionStatus: prepared.submissionStatus, ProtocolRejectionCode: prepared.protocolRejectionCode, FailureCode: prepared.failureCode,
		CreatedAt: prepared.occurredAt, ExpiresAt: expiresAt, Version: 1,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agent_turn_transcripts (
			id, node_attempt_id, turn, response_text, response_sha256,
			response_bytes, model_id, submission_status, protocol_rejection_code,
			failure_code, created_at, expires_at, expired_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
	`, transcript.ID, transcript.NodeAttemptID, transcript.Turn, transcript.ResponseText, transcript.ResponseSHA256,
		transcript.ResponseBytes, transcript.ModelID, transcript.SubmissionStatus, transcript.ProtocolRejectionCode,
		transcript.FailureCode, transcript.CreatedAt, transcript.ExpiresAt, transcript.Version); err != nil {
		if isUniqueConstraint(err) {
			return AgentTurnTranscript{}, false, fmt.Errorf("%w: agent turn transcript %s", ErrIdentityCollision, transcript.ID)
		}
		return AgentTurnTranscript{}, false, err
	}
	for _, submission := range prepared.submissions {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO agent_turn_transcript_submissions (
				id, transcript_id, ordinal, status, raw_request_json, validation_json,
				receipt_json, rejection_code, created_at, expired_at, version
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
		`, submission.id, transcript.ID, submission.ordinal, submission.status, submission.rawRequestJSON,
			submission.validationJSON, submission.receiptJSON, submission.rejectionCode, transcript.CreatedAt, 1); err != nil {
			if isUniqueConstraint(err) {
				return AgentTurnTranscript{}, false, fmt.Errorf("%w: agent turn transcript submission %s", ErrIdentityCollision, submission.id)
			}
			return AgentTurnTranscript{}, false, err
		}
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: prepared.actor, EntityType: "agent_turn_transcript", EntityID: transcript.ID,
		Action: "agent_turn_transcript.recorded", Reason: prepared.reason,
		PayloadJSON: auditPayload(map[string]any{
			"node_attempt_id": transcript.NodeAttemptID, "turn": transcript.Turn, "response_sha256": transcript.ResponseSHA256,
			"response_bytes": transcript.ResponseBytes, "model_id": transcript.ModelID, "submission_status": transcript.SubmissionStatus,
			"submission_count": len(prepared.submissions), "protocol_rejection_code": transcript.ProtocolRejectionCode, "failure_code": transcript.FailureCode,
		}), CreatedAt: transcript.CreatedAt,
	}); err != nil {
		return AgentTurnTranscript{}, false, err
	}
	return transcript, false, nil
}

func prepareCompletedAgentTurnCheckpoint(s *Store, request AgentTurnTranscriptCheckpoint, transcript preparedAgentTurnTranscript) (TurnCheckpoint, error) {
	if request.NodeAttemptID != transcript.nodeAttemptID || request.Turn != transcript.turn {
		return TurnCheckpoint{}, fmt.Errorf("agent turn transcript checkpoint does not match transcript coordinate")
	}
	if request.Substep != agentTurnTranscriptCompletedSubstep {
		return TurnCheckpoint{}, fmt.Errorf("agent turn transcript checkpoint substep must be turn_completed")
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return TurnCheckpoint{}, err
	}
	inputDigest, err := normalizeRequired(request.InputDigest, "checkpoint input digest")
	if err != nil {
		return TurnCheckpoint{}, err
	}
	payload, err := normalizeJSON(request.PayloadJSON, "checkpoint payload")
	if err != nil {
		return TurnCheckpoint{}, err
	}
	finishedAt := transcript.occurredAt
	return TurnCheckpoint{
		ID: id, NodeAttemptID: request.NodeAttemptID, Turn: request.Turn, Substep: request.Substep,
		Status: TurnCheckpointCompleted, InputDigest: inputDigest, ArtifactID: strings.TrimSpace(request.ArtifactID), PayloadJSON: payload,
		CreatedAt: transcript.occurredAt, FinishedAt: &finishedAt, Version: 2,
	}, nil
}

func insertAndCompleteAgentTurnCheckpointTx(ctx context.Context, s *Store, tx *sql.Tx, checkpoint TurnCheckpoint, actor, reason string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO turn_checkpoints (
			id, node_attempt_id, turn, substep, status, input_digest, artifact_id, payload_json, created_at, finished_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
	`, checkpoint.ID, checkpoint.NodeAttemptID, checkpoint.Turn, checkpoint.Substep, TurnCheckpointStarted,
		checkpoint.InputDigest, checkpoint.ArtifactID, checkpoint.PayloadJSON, checkpoint.CreatedAt, 1); err != nil {
		if isUniqueConstraint(err) {
			return fmt.Errorf("%w: checkpoint %s", ErrIdentityCollision, checkpoint.ID)
		}
		return err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: actor, EntityType: "turn_checkpoint", EntityID: checkpoint.ID, Action: "turn_checkpoint.created", Reason: reason,
		PayloadJSON: auditPayload(map[string]any{"node_attempt_id": checkpoint.NodeAttemptID, "turn": checkpoint.Turn, "substep": checkpoint.Substep, "input_digest": checkpoint.InputDigest}), CreatedAt: checkpoint.CreatedAt,
	}); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE turn_checkpoints SET status = ?, finished_at = ?, version = ?
		WHERE id = ? AND status = ? AND version = ?
	`, checkpoint.Status, checkpoint.FinishedAt, checkpoint.Version, checkpoint.ID, TurnCheckpointStarted, 1)
	if err != nil {
		return err
	}
	if err := requireOneAgentTurnTranscriptRow(result, checkpoint.ID); err != nil {
		return fmt.Errorf("%w: checkpoint %s", ErrOptimisticLock, checkpoint.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: actor, EntityType: "turn_checkpoint", EntityID: checkpoint.ID, Action: "turn_checkpoint.transitioned", Reason: reason,
		PayloadJSON: auditPayload(map[string]any{"status": checkpoint.Status, "version": checkpoint.Version, "artifact_id": checkpoint.ArtifactID}), CreatedAt: checkpoint.FinishedAt.UTC(),
	}); err != nil {
		return err
	}
	return nil
}

func sameCompletedAgentTurnCheckpoint(existing, wanted TurnCheckpoint) bool {
	return existing.NodeAttemptID == wanted.NodeAttemptID && existing.Turn == wanted.Turn && existing.Substep == wanted.Substep &&
		existing.Status == TurnCheckpointCompleted && existing.InputDigest == wanted.InputDigest && existing.ArtifactID == wanted.ArtifactID &&
		existing.PayloadJSON == wanted.PayloadJSON && existing.FinishedAt != nil
}

func samePreparedAgentTurnTranscript(existing AgentTurnTranscript, submissions []AgentTurnTranscriptSubmission, prepared preparedAgentTurnTranscript) bool {
	if existing.NodeAttemptID != prepared.nodeAttemptID || existing.Turn != prepared.turn || existing.ResponseText != prepared.responseText ||
		existing.ResponseSHA256 != prepared.responseSHA256 || existing.ResponseBytes != prepared.responseBytes || existing.ModelID != prepared.modelID ||
		existing.SubmissionStatus != prepared.submissionStatus || existing.ProtocolRejectionCode != prepared.protocolRejectionCode || existing.FailureCode != prepared.failureCode || len(submissions) != len(prepared.submissions) {
		return false
	}
	for index, existingSubmission := range submissions {
		wanted := prepared.submissions[index]
		if existingSubmission.Ordinal != wanted.ordinal || existingSubmission.Status != wanted.status || existingSubmission.RawRequestJSON != wanted.rawRequestJSON ||
			existingSubmission.ValidationJSON != wanted.validationJSON || existingSubmission.ReceiptJSON != wanted.receiptJSON || existingSubmission.RejectionCode != wanted.rejectionCode {
			return false
		}
	}
	return true
}

func agentTurnTranscriptExpiryBlockTx(ctx context.Context, tx *sql.Tx, transcript AgentTurnTranscript) (AgentTurnTranscriptExpiryBlock, error) {
	nodeAttempt, err := getNodeAttemptTx(ctx, tx, transcript.NodeAttemptID)
	if err != nil {
		return "", err
	}
	if !isTerminalNodeAttemptStatus(nodeAttempt.Status) {
		return AgentTurnTranscriptExpiryBlockedActiveAttempt, nil
	}
	stageAttempt, err := getStageAttemptTx(ctx, tx, nodeAttempt.StageAttemptID)
	if err != nil {
		return "", err
	}
	if !isTerminalStageExecutionStatus(stageAttempt.ExecutionStatus) {
		return AgentTurnTranscriptExpiryBlockedActiveAttempt, nil
	}
	var legalHold int
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM agent_turn_transcript_legal_holds
			WHERE transcript_id = ? AND released_at IS NULL
		)
	`, transcript.ID).Scan(&legalHold); err != nil {
		return "", err
	}
	if legalHold != 0 {
		return AgentTurnTranscriptExpiryBlockedLegalHold, nil
	}
	var activeWorker int
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM stage_attempts AS stage
			WHERE stage.id = ?
			  AND (
				EXISTS (
					SELECT 1 FROM run_worker_handoffs_v16 AS handoff
					WHERE handoff.run_id = stage.run_id
					  AND handoff.state IN ('launching', 'handed_off')
				)
				OR EXISTS (
					SELECT 1 FROM leases AS lease
					WHERE lease.resource_type = ?
					  AND lease.resource_id = stage.run_id
					  AND lease.state = 'active'
				)
			  )
		)
	`, stageAttempt.ID, RunWorkerSupervisorLeaseResourceType).Scan(&activeWorker); err != nil {
		return "", err
	}
	if activeWorker != 0 {
		return AgentTurnTranscriptExpiryBlockedActiveWorker, nil
	}
	return "", nil
}

func getAgentTurnTranscriptTx(ctx context.Context, tx *sql.Tx, transcriptID string) (AgentTurnTranscript, error) {
	transcript, err := scanAgentTurnTranscript(tx.QueryRowContext(ctx, agentTurnTranscriptSelect+" WHERE id = ?", transcriptID))
	if errors.Is(err, sql.ErrNoRows) {
		return AgentTurnTranscript{}, fmt.Errorf("%w: agent turn transcript %s", ErrNotFound, transcriptID)
	}
	return transcript, err
}

func getAgentTurnTranscriptByCoordinateTx(ctx context.Context, tx *sql.Tx, nodeAttemptID string, turn int) (*AgentTurnTranscript, error) {
	transcript, err := scanAgentTurnTranscript(tx.QueryRowContext(ctx, agentTurnTranscriptSelect+" WHERE node_attempt_id = ? AND turn = ?", nodeAttemptID, turn))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &transcript, nil
}

func getTurnCheckpointByCoordinateTx(ctx context.Context, tx *sql.Tx, nodeAttemptID string, turn int, substep string) (*TurnCheckpoint, error) {
	checkpoint, err := scanTurnCheckpoint(tx.QueryRowContext(ctx, turnCheckpointSelect+" WHERE node_attempt_id = ? AND turn = ? AND substep = ?", nodeAttemptID, turn, substep))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &checkpoint, nil
}

func listAgentTurnTranscriptSubmissionsTx(ctx context.Context, tx *sql.Tx, transcriptID string) ([]AgentTurnTranscriptSubmission, error) {
	rows, err := tx.QueryContext(ctx, agentTurnTranscriptSubmissionSelect+" WHERE transcript_id = ? ORDER BY ordinal ASC", transcriptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AgentTurnTranscriptSubmission, 0)
	for rows.Next() {
		item, err := scanAgentTurnTranscriptSubmission(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func getAgentTurnTranscriptLegalHoldByKeyTx(ctx context.Context, tx *sql.Tx, transcriptID, holdKey string) (*AgentTurnTranscriptLegalHold, error) {
	hold, err := scanAgentTurnTranscriptLegalHold(tx.QueryRowContext(ctx, agentTurnTranscriptLegalHoldSelect+" WHERE transcript_id = ? AND hold_key = ?", transcriptID, holdKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &hold, nil
}

func getAgentTurnTranscriptLegalHoldTx(ctx context.Context, tx *sql.Tx, holdID string) (AgentTurnTranscriptLegalHold, error) {
	hold, err := scanAgentTurnTranscriptLegalHold(tx.QueryRowContext(ctx, agentTurnTranscriptLegalHoldSelect+" WHERE id = ?", holdID))
	if errors.Is(err, sql.ErrNoRows) {
		return AgentTurnTranscriptLegalHold{}, fmt.Errorf("%w: agent turn transcript legal hold %s", ErrNotFound, holdID)
	}
	return hold, err
}

func scanAgentTurnTranscript(scanner rowScanner) (AgentTurnTranscript, error) {
	var transcript AgentTurnTranscript
	var expiredAt sql.NullTime
	if err := scanner.Scan(&transcript.ID, &transcript.NodeAttemptID, &transcript.Turn, &transcript.ResponseText, &transcript.ResponseSHA256,
		&transcript.ResponseBytes, &transcript.ModelID, &transcript.SubmissionStatus, &transcript.ProtocolRejectionCode,
		&transcript.FailureCode, &transcript.CreatedAt, &transcript.ExpiresAt, &expiredAt, &transcript.Version); err != nil {
		return AgentTurnTranscript{}, err
	}
	transcript.CreatedAt = transcript.CreatedAt.UTC()
	transcript.ExpiresAt = transcript.ExpiresAt.UTC()
	transcript.ExpiredAt = nullableTimePtr(expiredAt)
	return transcript, nil
}

func scanAgentTurnTranscriptSubmission(scanner rowScanner) (AgentTurnTranscriptSubmission, error) {
	var submission AgentTurnTranscriptSubmission
	var expiredAt sql.NullTime
	if err := scanner.Scan(&submission.ID, &submission.TranscriptID, &submission.Ordinal, &submission.Status, &submission.RawRequestJSON,
		&submission.ValidationJSON, &submission.ReceiptJSON, &submission.RejectionCode, &submission.CreatedAt, &expiredAt, &submission.Version); err != nil {
		return AgentTurnTranscriptSubmission{}, err
	}
	submission.CreatedAt = submission.CreatedAt.UTC()
	submission.ExpiredAt = nullableTimePtr(expiredAt)
	return submission, nil
}

func scanAgentTurnTranscriptLegalHold(scanner rowScanner) (AgentTurnTranscriptLegalHold, error) {
	var hold AgentTurnTranscriptLegalHold
	var releasedAt sql.NullTime
	if err := scanner.Scan(&hold.ID, &hold.TranscriptID, &hold.HoldKey, &hold.Reason, &hold.CreatedBy, &hold.CreatedAt,
		&hold.ReleasedBy, &hold.ReleaseReason, &releasedAt, &hold.Version); err != nil {
		return AgentTurnTranscriptLegalHold{}, err
	}
	hold.CreatedAt = hold.CreatedAt.UTC()
	hold.ReleasedAt = nullableTimePtr(releasedAt)
	return hold, nil
}

func requireOneAgentTurnTranscriptRow(result sql.Result, id string) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: agent turn transcript %s", ErrOptimisticLock, id)
	}
	return nil
}
