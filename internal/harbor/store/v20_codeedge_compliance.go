package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const codeEdgeComplianceRecordSelect = `
	SELECT id, run_id, task_id, revision_id, task_digest, status,
	       evaluator_evidence_handoff_id, evaluator_evidence_handoff_fingerprint,
	       qwen_receipt_json, opus_receipt_json, submission_receipt_json,
	       decision_json, decision_fingerprint, authorization_json,
	       authorization_fingerprint, idempotency_key, created_by, created_at
	FROM codeedge_compliance_records_v20`

// CreateCodeEdgeComplianceRecord persists one final compliance result. It
// verifies the durable Run/Task/Revision relation in the same transaction and
// does not permit a rejected result to be replaced by a later approval under
// the same Run. A new task, policy, or evidence binding needs a new Run.
func (s *Store) CreateCodeEdgeComplianceRecord(ctx context.Context, request CreateCodeEdgeComplianceRecordRequest) (CodeEdgeComplianceRecord, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	prepared, err := s.prepareCodeEdgeComplianceRecord(request)
	if err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	defer tx.Rollback()
	record, created, _, _, err := s.createCodeEdgeComplianceRecordTx(ctx, tx, prepared.record)
	if err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	if created {
		if err := s.appendCodeEdgeComplianceRecordAuditTx(ctx, tx, record, prepared.reason); err != nil {
			return CodeEdgeComplianceRecord{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	return record, nil
}

// CreateApprovedCodeEdgeComplianceRecordAndValidateRevision commits the one
// approved final-compliance record and the sealed-to-validated revision
// transition atomically. The validation evidence is derived from, and thus
// cryptographically binds, both the final decision and package authorization
// fingerprints. Replaying the same immutable record accepts only that exact
// validated revision state.
func (s *Store) CreateApprovedCodeEdgeComplianceRecordAndValidateRevision(ctx context.Context, request CreateCodeEdgeComplianceRecordRequest, expectedRevisionStateVersion int64) (CodeEdgeComplianceRecord, TaskRevision, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return CodeEdgeComplianceRecord{}, TaskRevision{}, err
	}
	if expectedRevisionStateVersion <= 0 {
		return CodeEdgeComplianceRecord{}, TaskRevision{}, fmt.Errorf("expected revision state version must be positive")
	}
	prepared, err := s.prepareCodeEdgeComplianceRecord(request)
	if err != nil {
		return CodeEdgeComplianceRecord{}, TaskRevision{}, err
	}
	if prepared.record.Status != CodeEdgeComplianceApproved {
		return CodeEdgeComplianceRecord{}, TaskRevision{}, fmt.Errorf("only approved CodeEdge compliance can validate a revision")
	}
	evidence, err := CodeEdgeRevisionValidationEvidenceFingerprint(prepared.record)
	if err != nil {
		return CodeEdgeComplianceRecord{}, TaskRevision{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CodeEdgeComplianceRecord{}, TaskRevision{}, err
	}
	defer tx.Rollback()
	record, created, revision, task, err := s.createCodeEdgeComplianceRecordTx(ctx, tx, prepared.record)
	if err != nil {
		return CodeEdgeComplianceRecord{}, TaskRevision{}, err
	}
	if record.Status != CodeEdgeComplianceApproved {
		return CodeEdgeComplianceRecord{}, TaskRevision{}, fmt.Errorf("%w: replayed CodeEdge compliance is not approved", ErrIdempotencyConflict)
	}
	evidence, err = CodeEdgeRevisionValidationEvidenceFingerprint(record)
	if err != nil {
		return CodeEdgeComplianceRecord{}, TaskRevision{}, err
	}
	now := s.now().UTC()
	switch revision.State {
	case RevisionStateSealed:
		if revision.StateVersion != expectedRevisionStateVersion {
			return CodeEdgeComplianceRecord{}, TaskRevision{}, fmt.Errorf("%w: revision %s", ErrOptimisticLock, revision.ID)
		}
		if err := s.guardTaskPurgeMutationTx(ctx, tx, task.ID, record.CreatedBy, now); err != nil {
			return CodeEdgeComplianceRecord{}, TaskRevision{}, err
		}
		revision.State = RevisionStateValidated
		revision.ValidationEvidenceManifest = evidence
		revision.StateVersion++
		revision.StateUpdatedBy = record.CreatedBy
		revision.StateUpdatedAt = now
		result, updateErr := tx.ExecContext(ctx, `
			UPDATE task_revisions
			SET state = ?, validation_evidence_manifest = ?, state_version = ?, state_updated_by = ?, state_updated_at = ?
			WHERE id = ? AND state_version = ?
		`, revision.State, revision.ValidationEvidenceManifest, revision.StateVersion, revision.StateUpdatedBy, revision.StateUpdatedAt,
			revision.ID, expectedRevisionStateVersion)
		if updateErr != nil {
			return CodeEdgeComplianceRecord{}, TaskRevision{}, updateErr
		}
		if err := requireOneRow(result, "revision", revision.ID); err != nil {
			return CodeEdgeComplianceRecord{}, TaskRevision{}, err
		}
		if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
			Actor: record.CreatedBy, EntityType: "task_revision", EntityID: revision.ID,
			Action: "task_revision.state_transitioned", Reason: prepared.reason,
			PayloadJSON: auditPayload(map[string]any{
				"state": revision.State, "state_version": revision.StateVersion,
				"validation_evidence_manifest":  revision.ValidationEvidenceManifest,
				"codeedge_compliance_record_id": record.ID,
				"decision_fingerprint":          record.DecisionFingerprint,
				"authorization_fingerprint":     record.AuthorizationFingerprint,
			}),
			CreatedAt: now,
		}); err != nil {
			return CodeEdgeComplianceRecord{}, TaskRevision{}, err
		}
	case RevisionStateValidated:
		if revision.ValidationEvidenceManifest != evidence {
			return CodeEdgeComplianceRecord{}, TaskRevision{}, fmt.Errorf("%w: revision %s was validated by different evidence", ErrIdempotencyConflict, revision.ID)
		}
	default:
		return CodeEdgeComplianceRecord{}, TaskRevision{}, fmt.Errorf("%w: revision %s cannot be validated from %s", ErrInvalidTransition, revision.ID, revision.State)
	}
	if created {
		if err := s.appendCodeEdgeComplianceRecordAuditTx(ctx, tx, record, prepared.reason); err != nil {
			return CodeEdgeComplianceRecord{}, TaskRevision{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return CodeEdgeComplianceRecord{}, TaskRevision{}, err
	}
	return record, revision, nil
}

type preparedCodeEdgeComplianceRecord struct {
	record CodeEdgeComplianceRecord
	reason string
}

// CodeEdgeRevisionValidationEvidenceFingerprint is the immutable evidence
// reference written onto a TaskRevision when final compliance approves it.
// It binds the revision transition to the exact compliance record and both
// cryptographic authorization inputs, rather than to a mutable status field.
func CodeEdgeRevisionValidationEvidenceFingerprint(record CodeEdgeComplianceRecord) (string, error) {
	if record.Status != CodeEdgeComplianceApproved {
		return "", fmt.Errorf("CodeEdge revision validation evidence requires an approved compliance record")
	}
	for label, value := range map[string]string{
		"compliance record ID":      record.ID,
		"decision fingerprint":      record.DecisionFingerprint,
		"authorization fingerprint": record.AuthorizationFingerprint,
		"run ID":                    record.RunID,
		"revision ID":               record.RevisionID,
		"task digest":               record.TaskDigest,
	} {
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("CodeEdge revision validation evidence is missing %s", label)
		}
	}
	document := struct {
		Format                   string `json:"format"`
		Version                  string `json:"version"`
		ComplianceRecordID       string `json:"compliance_record_id"`
		RunID                    string `json:"run_id"`
		RevisionID               string `json:"revision_id"`
		TaskDigest               string `json:"task_digest"`
		DecisionFingerprint      string `json:"decision_fingerprint"`
		AuthorizationFingerprint string `json:"authorization_fingerprint"`
	}{
		Format: "harbor.codeedge.revision-validation-evidence.v1", Version: "1",
		ComplianceRecordID: record.ID, RunID: record.RunID, RevisionID: record.RevisionID,
		TaskDigest: record.TaskDigest, DecisionFingerprint: record.DecisionFingerprint,
		AuthorizationFingerprint: record.AuthorizationFingerprint,
	}
	canonical, err := json.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("canonicalize CodeEdge revision validation evidence: %w", err)
	}
	fingerprint, err := workflowkit.FingerprintBytes(document.Format, canonical)
	if err != nil {
		return "", fmt.Errorf("fingerprint CodeEdge revision validation evidence: %w", err)
	}
	return string(fingerprint), nil
}

func (s *Store) prepareCodeEdgeComplianceRecord(request CreateCodeEdgeComplianceRecordRequest) (preparedCodeEdgeComplianceRecord, error) {
	if !isUUIDv7(request.RunID) || !isUUIDv7(request.TaskID) || !isUUIDv7(request.RevisionID) {
		return preparedCodeEdgeComplianceRecord{}, ErrInvalidUUIDv7Identity
	}
	if err := ValidateTaskDigestV2(request.TaskDigest); err != nil {
		return preparedCodeEdgeComplianceRecord{}, err
	}
	key := strings.TrimSpace(request.IdempotencyKey)
	if !isUUIDv7(key) {
		return preparedCodeEdgeComplianceRecord{}, ErrInvalidUUIDv7Identity
	}
	requestedID := strings.TrimSpace(request.ID)
	if requestedID != "" && requestedID != key {
		return preparedCodeEdgeComplianceRecord{}, fmt.Errorf("%w: CodeEdge compliance identity differs from idempotency key", ErrIdempotencyConflict)
	}
	id, err := s.newV2ID(key)
	if err != nil {
		return preparedCodeEdgeComplianceRecord{}, err
	}
	if !validCodeEdgeComplianceStatus(request.Status) {
		return preparedCodeEdgeComplianceRecord{}, fmt.Errorf("invalid CodeEdge compliance status %q", request.Status)
	}
	if !isUUIDv7(strings.TrimSpace(request.EvaluatorEvidenceHandoffID)) {
		return preparedCodeEdgeComplianceRecord{}, ErrInvalidUUIDv7Identity
	}
	handoffFingerprint, err := normalizeRequired(request.EvaluatorEvidenceHandoffFingerprint, "CodeEdge evaluator evidence handoff fingerprint")
	if err != nil {
		return preparedCodeEdgeComplianceRecord{}, err
	}
	qwen, err := normalizeV4JSON(request.QwenReceiptJSON, "CodeEdge Qwen receipt")
	if err != nil {
		return preparedCodeEdgeComplianceRecord{}, err
	}
	opus, err := normalizeV4JSON(request.OpusReceiptJSON, "CodeEdge Opus receipt")
	if err != nil {
		return preparedCodeEdgeComplianceRecord{}, err
	}
	submission, err := normalizeV4JSON(request.SubmissionReceiptJSON, "CodeEdge submission receipt")
	if err != nil {
		return preparedCodeEdgeComplianceRecord{}, err
	}
	decision, err := normalizeV4JSON(request.DecisionJSON, "CodeEdge final compliance decision")
	if err != nil {
		return preparedCodeEdgeComplianceRecord{}, err
	}
	decisionFingerprint, err := normalizeRequired(request.DecisionFingerprint, "CodeEdge final compliance decision fingerprint")
	if err != nil {
		return preparedCodeEdgeComplianceRecord{}, err
	}
	authorization := strings.TrimSpace(request.AuthorizationJSON)
	authorizationFingerprint := strings.TrimSpace(request.AuthorizationFingerprint)
	if request.Status == CodeEdgeComplianceApproved {
		authorization, err = normalizeV4JSON(authorization, "CodeEdge local package authorization")
		if err != nil {
			return preparedCodeEdgeComplianceRecord{}, err
		}
		authorizationFingerprint, err = normalizeRequired(authorizationFingerprint, "CodeEdge local package authorization fingerprint")
		if err != nil {
			return preparedCodeEdgeComplianceRecord{}, err
		}
	} else if authorization != "" || authorizationFingerprint != "" {
		return preparedCodeEdgeComplianceRecord{}, fmt.Errorf("rejected CodeEdge compliance record cannot contain a package authorization")
	}
	if err := validateCanonicalCodeEdgeComplianceDocuments(
		request.TaskDigest, request.Status, request.EvaluatorEvidenceHandoffID, handoffFingerprint,
		qwen, opus, submission, decision, decisionFingerprint, authorization, authorizationFingerprint,
	); err != nil {
		return preparedCodeEdgeComplianceRecord{}, err
	}
	now := s.now().UTC()
	record := CodeEdgeComplianceRecord{
		ID: id, RunID: strings.TrimSpace(request.RunID), TaskID: strings.TrimSpace(request.TaskID), RevisionID: strings.TrimSpace(request.RevisionID),
		TaskDigest: strings.TrimSpace(request.TaskDigest), Status: request.Status,
		EvaluatorEvidenceHandoffID: strings.TrimSpace(request.EvaluatorEvidenceHandoffID), EvaluatorEvidenceHandoffFingerprint: handoffFingerprint,
		QwenReceiptJSON: qwen, OpusReceiptJSON: opus, SubmissionReceiptJSON: submission,
		DecisionJSON: decision, DecisionFingerprint: decisionFingerprint,
		AuthorizationJSON: authorization, AuthorizationFingerprint: authorizationFingerprint,
		IdempotencyKey: key, CreatedBy: resolveActor(request.Actor), CreatedAt: now,
	}
	return preparedCodeEdgeComplianceRecord{record: record, reason: strings.TrimSpace(request.Reason)}, nil
}

func (s *Store) createCodeEdgeComplianceRecordTx(ctx context.Context, tx *sql.Tx, record CodeEdgeComplianceRecord) (CodeEdgeComplianceRecord, bool, TaskRevision, TaskV2, error) {
	run, err := getWorkflowRunTx(ctx, tx, record.RunID)
	if err != nil {
		return CodeEdgeComplianceRecord{}, false, TaskRevision{}, TaskV2{}, err
	}
	if run.TaskID != record.TaskID || run.RevisionID != record.RevisionID {
		return CodeEdgeComplianceRecord{}, false, TaskRevision{}, TaskV2{}, fmt.Errorf("CodeEdge compliance Run does not match task/revision")
	}
	revision, err := getTaskRevisionTx(ctx, tx, record.RevisionID)
	if err != nil {
		return CodeEdgeComplianceRecord{}, false, TaskRevision{}, TaskV2{}, err
	}
	if revision.TaskID != record.TaskID || revision.TaskDigest != record.TaskDigest {
		return CodeEdgeComplianceRecord{}, false, TaskRevision{}, TaskV2{}, fmt.Errorf("CodeEdge compliance task/revision/digest binding does not match durable revision")
	}
	task, err := getTaskV2Tx(ctx, tx, record.TaskID)
	if err != nil {
		return CodeEdgeComplianceRecord{}, false, TaskRevision{}, TaskV2{}, err
	}
	handoff, err := scanCodeEdgeEvaluatorEvidenceHandoff(tx.QueryRowContext(ctx, codeEdgeEvaluatorEvidenceHandoffSelect+" WHERE id = ?", record.EvaluatorEvidenceHandoffID))
	if err == sql.ErrNoRows {
		return CodeEdgeComplianceRecord{}, false, TaskRevision{}, TaskV2{}, fmt.Errorf("%w: CodeEdge evaluator evidence handoff %s", ErrNotFound, record.EvaluatorEvidenceHandoffID)
	}
	if err != nil {
		return CodeEdgeComplianceRecord{}, false, TaskRevision{}, TaskV2{}, err
	}
	if handoff.ParentRunID != record.RunID || handoff.TaskID != record.TaskID || handoff.RevisionID != record.RevisionID || handoff.TaskDigest != record.TaskDigest || handoff.HandoffFingerprint != record.EvaluatorEvidenceHandoffFingerprint {
		return CodeEdgeComplianceRecord{}, false, TaskRevision{}, TaskV2{}, fmt.Errorf("CodeEdge compliance handoff does not match parent Run or frozen task")
	}
	if err := validateCodeEdgeComplianceHandoffReceiptBinding(handoff, record); err != nil {
		return CodeEdgeComplianceRecord{}, false, TaskRevision{}, TaskV2{}, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO codeedge_compliance_records_v20 (
			id, run_id, task_id, revision_id, task_digest, status,
			evaluator_evidence_handoff_id, evaluator_evidence_handoff_fingerprint,
			qwen_receipt_json, opus_receipt_json, submission_receipt_json,
			decision_json, decision_fingerprint, authorization_json,
			authorization_fingerprint, idempotency_key, created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, record.ID, record.RunID, record.TaskID, record.RevisionID, record.TaskDigest, record.Status,
		record.EvaluatorEvidenceHandoffID, record.EvaluatorEvidenceHandoffFingerprint,
		record.QwenReceiptJSON, record.OpusReceiptJSON, record.SubmissionReceiptJSON,
		record.DecisionJSON, record.DecisionFingerprint, record.AuthorizationJSON,
		record.AuthorizationFingerprint, record.IdempotencyKey, record.CreatedBy, record.CreatedAt)
	if err != nil {
		if !isUniqueConstraint(err) && !isGlobalIdentityCollision(err) {
			return CodeEdgeComplianceRecord{}, false, TaskRevision{}, TaskV2{}, err
		}
		if existing, existingErr := getCodeEdgeComplianceRecordByKeyTx(ctx, tx, record.IdempotencyKey); existingErr == nil {
			if sameCodeEdgeComplianceRecord(existing, record) {
				return existing, false, revision, task, nil
			}
			return CodeEdgeComplianceRecord{}, false, TaskRevision{}, TaskV2{}, fmt.Errorf("%w: CodeEdge compliance key %s", ErrIdempotencyConflict, record.IdempotencyKey)
		} else if !isNotFound(existingErr) {
			return CodeEdgeComplianceRecord{}, false, TaskRevision{}, TaskV2{}, existingErr
		}
		if existing, existingErr := getCodeEdgeComplianceRecordByRunTx(ctx, tx, record.RunID); existingErr == nil {
			if sameCodeEdgeComplianceRecord(existing, record) {
				return existing, false, revision, task, nil
			}
			return CodeEdgeComplianceRecord{}, false, TaskRevision{}, TaskV2{}, fmt.Errorf("%w: CodeEdge compliance Run %s", ErrIdempotencyConflict, record.RunID)
		} else if !isNotFound(existingErr) {
			return CodeEdgeComplianceRecord{}, false, TaskRevision{}, TaskV2{}, existingErr
		}
		return CodeEdgeComplianceRecord{}, false, TaskRevision{}, TaskV2{}, fmt.Errorf("%w: CodeEdge compliance record %s", ErrIdentityCollision, record.ID)
	}
	return record, true, revision, task, nil
}

func (s *Store) appendCodeEdgeComplianceRecordAuditTx(ctx context.Context, tx *sql.Tx, record CodeEdgeComplianceRecord, reason string) error {
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: record.CreatedBy, EntityType: "codeedge_compliance_record", EntityID: record.ID,
		Action: "codeedge_compliance.recorded", Reason: reason,
		PayloadJSON: auditPayload(map[string]any{
			"run_id": record.RunID, "revision_id": record.RevisionID, "status": record.Status,
			"evaluator_evidence_handoff_id": record.EvaluatorEvidenceHandoffID, "evaluator_evidence_handoff_fingerprint": record.EvaluatorEvidenceHandoffFingerprint,
			"decision_fingerprint": record.DecisionFingerprint, "authorization_fingerprint": record.AuthorizationFingerprint,
		}),
		CreatedAt: record.CreatedAt,
	}); err != nil {
		return err
	}
	return nil
}

// GetCodeEdgeComplianceRecordForRun returns the one immutable final
// compliance record for a CodeEdge Run, if final compliance has been
// recorded. A nil result means packaging is not authorized.
func (s *Store) GetCodeEdgeComplianceRecordForRun(ctx context.Context, runID string) (*CodeEdgeComplianceRecord, error) {
	if !isUUIDv7(runID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	record, err := scanCodeEdgeComplianceRecord(s.db.QueryRowContext(ctx, codeEdgeComplianceRecordSelect+" WHERE run_id = ?", runID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func getCodeEdgeComplianceRecordByRunTx(ctx context.Context, tx *sql.Tx, runID string) (CodeEdgeComplianceRecord, error) {
	record, err := scanCodeEdgeComplianceRecord(tx.QueryRowContext(ctx, codeEdgeComplianceRecordSelect+" WHERE run_id = ?", runID))
	if err == sql.ErrNoRows {
		return CodeEdgeComplianceRecord{}, fmt.Errorf("%w: CodeEdge compliance Run %s", ErrNotFound, runID)
	}
	return record, err
}

func getCodeEdgeComplianceRecordByKeyTx(ctx context.Context, tx *sql.Tx, key string) (CodeEdgeComplianceRecord, error) {
	record, err := scanCodeEdgeComplianceRecord(tx.QueryRowContext(ctx, codeEdgeComplianceRecordSelect+" WHERE idempotency_key = ?", key))
	if err == sql.ErrNoRows {
		return CodeEdgeComplianceRecord{}, fmt.Errorf("%w: CodeEdge compliance key %s", ErrNotFound, key)
	}
	return record, err
}

func scanCodeEdgeComplianceRecord(scanner rowScanner) (CodeEdgeComplianceRecord, error) {
	var record CodeEdgeComplianceRecord
	if err := scanner.Scan(
		&record.ID, &record.RunID, &record.TaskID, &record.RevisionID, &record.TaskDigest, &record.Status,
		&record.EvaluatorEvidenceHandoffID, &record.EvaluatorEvidenceHandoffFingerprint,
		&record.QwenReceiptJSON, &record.OpusReceiptJSON, &record.SubmissionReceiptJSON,
		&record.DecisionJSON, &record.DecisionFingerprint, &record.AuthorizationJSON,
		&record.AuthorizationFingerprint, &record.IdempotencyKey, &record.CreatedBy, &record.CreatedAt,
	); err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	record.CreatedAt = record.CreatedAt.UTC()
	return record, nil
}

func validCodeEdgeComplianceStatus(status CodeEdgeComplianceStatus) bool {
	return status == CodeEdgeComplianceApproved || status == CodeEdgeComplianceRejected
}

// validateCanonicalCodeEdgeComplianceDocuments keeps the immutable V20 record
// safe even when Store is called outside the application service. The app
// performs stronger Run/catalog/artifact verification; this boundary rejects
// malformed, non-canonical, internally inconsistent, or forged typed evidence
// before it can become durable state.
func validateCanonicalCodeEdgeComplianceDocuments(taskDigest string, status CodeEdgeComplianceStatus, handoffID, handoffFingerprint, qwenRaw, opusRaw, submissionRaw, decisionRaw, decisionFingerprint, authorizationRaw, authorizationFingerprint string) error {
	var qwen, opus codeedge.EvaluationReceipt
	var submission codeedge.SubmissionCheckReceipt
	var decision codeedge.FinalComplianceDecision
	if err := decodeCanonicalCodeEdgeDocument(qwenRaw, "CodeEdge Qwen receipt", &qwen, codeedge.EvaluationReceipt.CanonicalJSON); err != nil {
		return err
	}
	if err := decodeCanonicalCodeEdgeDocument(opusRaw, "CodeEdge Opus receipt", &opus, codeedge.EvaluationReceipt.CanonicalJSON); err != nil {
		return err
	}
	if err := decodeCanonicalCodeEdgeDocument(submissionRaw, "CodeEdge submission receipt", &submission, codeedge.SubmissionCheckReceipt.CanonicalJSON); err != nil {
		return err
	}
	if err := decodeCanonicalCodeEdgeDocument(decisionRaw, "CodeEdge final compliance decision", &decision, codeedge.FinalComplianceDecision.CanonicalJSON); err != nil {
		return err
	}

	computedDecisionFingerprint, err := decision.Fingerprint()
	if err != nil {
		return err
	}
	if decisionFingerprint != string(computedDecisionFingerprint) {
		return fmt.Errorf("CodeEdge final compliance decision fingerprint does not match canonical decision")
	}
	if decision.Binding.TaskSnapshotDigest != workflowkit.SubjectDigest(taskDigest) {
		return fmt.Errorf("CodeEdge final compliance decision is bound to another task digest")
	}
	if !isUUIDv7(strings.TrimSpace(handoffID)) || decision.EvaluatorEvidenceHandoffFingerprint != workflowkit.Fingerprint(handoffFingerprint) {
		return fmt.Errorf("CodeEdge final compliance decision does not match evaluator evidence handoff")
	}
	if decision.Status == codeedge.FinalComplianceApproved && status != CodeEdgeComplianceApproved {
		return fmt.Errorf("approved CodeEdge final decision requires approved record status")
	}
	if decision.Status == codeedge.FinalComplianceRejected && status != CodeEdgeComplianceRejected {
		return fmt.Errorf("rejected CodeEdge final decision requires rejected record status")
	}

	if err := verifyCodeEdgeReceiptDecisionBinding("Qwen", qwen, decision); err != nil {
		return err
	}
	if err := verifyCodeEdgeReceiptDecisionBinding("Opus", opus, decision); err != nil {
		return err
	}
	submissionFingerprint, err := submission.Fingerprint()
	if err != nil {
		return err
	}
	if submission.Binding != decision.Binding || decision.SubmissionReceiptFingerprint != submissionFingerprint {
		return fmt.Errorf("CodeEdge submission receipt does not match final compliance decision")
	}

	if status == CodeEdgeComplianceRejected {
		return nil
	}
	var authorization codeedge.LocalPackageAuthorization
	if err := decodeCanonicalCodeEdgeDocument(authorizationRaw, "CodeEdge local package authorization", &authorization, codeedge.LocalPackageAuthorization.CanonicalJSON); err != nil {
		return err
	}
	computedAuthorizationFingerprint, err := authorization.Fingerprint()
	if err != nil {
		return err
	}
	if authorizationFingerprint != string(computedAuthorizationFingerprint) {
		return fmt.Errorf("CodeEdge local package authorization fingerprint does not match canonical authorization")
	}
	decisionCanonical, err := decision.CanonicalJSON()
	if err != nil {
		return err
	}
	authorizationDecisionCanonical, err := authorization.Decision.CanonicalJSON()
	if err != nil {
		return err
	}
	if !bytes.Equal(decisionCanonical, authorizationDecisionCanonical) || authorization.DecisionFingerprint != computedDecisionFingerprint {
		return fmt.Errorf("CodeEdge local package authorization does not match final compliance decision")
	}
	return nil
}

// validateCodeEdgeComplianceHandoffReceiptBinding makes the immutable handoff
// authoritative even for direct Store callers. The application layer rebuilds
// child artifacts; this transaction-level proof prevents a caller from pairing
// a real handoff fingerprint with another syntactically valid receipt/decision.
func validateCodeEdgeComplianceHandoffReceiptBinding(handoff CodeEdgeEvaluatorEvidenceHandoff, record CodeEdgeComplianceRecord) error {
	var document codeedge.EvaluatorEvidenceHandoff
	if err := decodeCanonicalCodeEdgeDocument(handoff.HandoffJSON, "CodeEdge evaluator evidence handoff", &document, codeedge.EvaluatorEvidenceHandoff.CanonicalJSON); err != nil {
		return err
	}
	fingerprint, err := document.Fingerprint()
	if err != nil {
		return err
	}
	if string(fingerprint) != handoff.HandoffFingerprint || handoff.HandoffFingerprint != record.EvaluatorEvidenceHandoffFingerprint {
		return fmt.Errorf("CodeEdge evaluator evidence handoff fingerprint does not match canonical handoff")
	}
	if document.ParentRunID != record.RunID || document.ChildRunID != handoff.ChildRunID ||
		string(document.ParentBinding.TaskSnapshotDigest) != record.TaskDigest ||
		string(document.ParentDefinitionFingerprint) != handoff.ParentDefinitionFingerprint ||
		string(document.ChildDefinitionFingerprint) != handoff.ChildDefinitionFingerprint ||
		document.Qwen.ChildStageAttemptID != handoff.QwenStageAttemptID ||
		document.Opus.ChildStageAttemptID != handoff.OpusStageAttemptID ||
		string(document.Qwen.TrialSetFingerprint) != handoff.QwenTrialSetFingerprint ||
		string(document.Opus.TrialSetFingerprint) != handoff.OpusTrialSetFingerprint {
		return fmt.Errorf("CodeEdge evaluator evidence handoff document does not match durable linkage")
	}
	var qwen, opus codeedge.EvaluationReceipt
	if err := decodeCanonicalCodeEdgeDocument(record.QwenReceiptJSON, "CodeEdge Qwen receipt", &qwen, codeedge.EvaluationReceipt.CanonicalJSON); err != nil {
		return err
	}
	if err := decodeCanonicalCodeEdgeDocument(record.OpusReceiptJSON, "CodeEdge Opus receipt", &opus, codeedge.EvaluationReceipt.CanonicalJSON); err != nil {
		return err
	}
	if err := requireSameCanonicalCodeEdgeEvaluationReceipt("Qwen", qwen, document.Qwen.Receipt); err != nil {
		return err
	}
	if err := requireSameCanonicalCodeEdgeEvaluationReceipt("Opus", opus, document.Opus.Receipt); err != nil {
		return err
	}
	return nil
}

func requireSameCanonicalCodeEdgeEvaluationReceipt(role string, left, right codeedge.EvaluationReceipt) error {
	leftCanonical, err := left.CanonicalJSON()
	if err != nil {
		return err
	}
	rightCanonical, err := right.CanonicalJSON()
	if err != nil {
		return err
	}
	if !bytes.Equal(leftCanonical, rightCanonical) {
		return fmt.Errorf("CodeEdge %s receipt does not match evaluator evidence handoff", role)
	}
	return nil
}

func decodeCanonicalCodeEdgeDocument[T any](raw, name string, target *T, canonical func(T) ([]byte, error)) error {
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	encoded, err := canonical(*target)
	if err != nil {
		return fmt.Errorf("validate %s: %w", name, err)
	}
	if !bytes.Equal(encoded, []byte(raw)) {
		return fmt.Errorf("%s must use its canonical JSON representation", name)
	}
	return nil
}

func verifyCodeEdgeReceiptDecisionBinding(role string, receipt codeedge.EvaluationReceipt, decision codeedge.FinalComplianceDecision) error {
	fingerprint, err := receipt.Fingerprint()
	if err != nil {
		return err
	}
	expected := decision.QwenReceiptFingerprint
	if role == "Opus" {
		expected = decision.OpusReceiptFingerprint
	}
	if expected != fingerprint {
		return fmt.Errorf("CodeEdge %s receipt does not match final compliance decision", role)
	}
	return nil
}

func sameCodeEdgeComplianceRecord(left, right CodeEdgeComplianceRecord) bool {
	return left.ID == right.ID &&
		left.RunID == right.RunID &&
		left.TaskID == right.TaskID &&
		left.RevisionID == right.RevisionID &&
		left.TaskDigest == right.TaskDigest &&
		left.Status == right.Status &&
		left.EvaluatorEvidenceHandoffID == right.EvaluatorEvidenceHandoffID &&
		left.EvaluatorEvidenceHandoffFingerprint == right.EvaluatorEvidenceHandoffFingerprint &&
		left.QwenReceiptJSON == right.QwenReceiptJSON &&
		left.OpusReceiptJSON == right.OpusReceiptJSON &&
		left.SubmissionReceiptJSON == right.SubmissionReceiptJSON &&
		left.DecisionJSON == right.DecisionJSON &&
		left.DecisionFingerprint == right.DecisionFingerprint &&
		left.AuthorizationJSON == right.AuthorizationJSON &&
		left.AuthorizationFingerprint == right.AuthorizationFingerprint &&
		left.IdempotencyKey == right.IdempotencyKey &&
		left.CreatedBy == right.CreatedBy
}
