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
	if !isUUIDv7(request.RunID) || !isUUIDv7(request.TaskID) || !isUUIDv7(request.RevisionID) {
		return CodeEdgeComplianceRecord{}, ErrInvalidUUIDv7Identity
	}
	if err := ValidateTaskDigestV2(request.TaskDigest); err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	key := strings.TrimSpace(request.IdempotencyKey)
	if !isUUIDv7(key) {
		return CodeEdgeComplianceRecord{}, ErrInvalidUUIDv7Identity
	}
	requestedID := strings.TrimSpace(request.ID)
	if requestedID != "" && requestedID != key {
		return CodeEdgeComplianceRecord{}, fmt.Errorf("%w: CodeEdge compliance identity differs from idempotency key", ErrIdempotencyConflict)
	}
	id, err := s.newV2ID(key)
	if err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	if !validCodeEdgeComplianceStatus(request.Status) {
		return CodeEdgeComplianceRecord{}, fmt.Errorf("invalid CodeEdge compliance status %q", request.Status)
	}
	qwen, err := normalizeV4JSON(request.QwenReceiptJSON, "CodeEdge Qwen receipt")
	if err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	opus, err := normalizeV4JSON(request.OpusReceiptJSON, "CodeEdge Opus receipt")
	if err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	submission, err := normalizeV4JSON(request.SubmissionReceiptJSON, "CodeEdge submission receipt")
	if err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	decision, err := normalizeV4JSON(request.DecisionJSON, "CodeEdge final compliance decision")
	if err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	decisionFingerprint, err := normalizeRequired(request.DecisionFingerprint, "CodeEdge final compliance decision fingerprint")
	if err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	authorization := strings.TrimSpace(request.AuthorizationJSON)
	authorizationFingerprint := strings.TrimSpace(request.AuthorizationFingerprint)
	if request.Status == CodeEdgeComplianceApproved {
		authorization, err = normalizeV4JSON(authorization, "CodeEdge local package authorization")
		if err != nil {
			return CodeEdgeComplianceRecord{}, err
		}
		authorizationFingerprint, err = normalizeRequired(authorizationFingerprint, "CodeEdge local package authorization fingerprint")
		if err != nil {
			return CodeEdgeComplianceRecord{}, err
		}
	} else if authorization != "" || authorizationFingerprint != "" {
		return CodeEdgeComplianceRecord{}, fmt.Errorf("rejected CodeEdge compliance record cannot contain a package authorization")
	}
	if err := validateCanonicalCodeEdgeComplianceDocuments(
		request.TaskDigest, request.Status, qwen, opus, submission, decision, decisionFingerprint, authorization, authorizationFingerprint,
	); err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	now := s.now().UTC()
	record := CodeEdgeComplianceRecord{
		ID: id, RunID: strings.TrimSpace(request.RunID), TaskID: strings.TrimSpace(request.TaskID), RevisionID: strings.TrimSpace(request.RevisionID),
		TaskDigest: strings.TrimSpace(request.TaskDigest), Status: request.Status,
		QwenReceiptJSON: qwen, OpusReceiptJSON: opus, SubmissionReceiptJSON: submission,
		DecisionJSON: decision, DecisionFingerprint: decisionFingerprint,
		AuthorizationJSON: authorization, AuthorizationFingerprint: authorizationFingerprint,
		IdempotencyKey: key, CreatedBy: resolveActor(request.Actor), CreatedAt: now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	defer tx.Rollback()
	run, err := getWorkflowRunTx(ctx, tx, record.RunID)
	if err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	if run.TaskID != record.TaskID || run.RevisionID != record.RevisionID {
		return CodeEdgeComplianceRecord{}, fmt.Errorf("CodeEdge compliance Run does not match task/revision")
	}
	revision, err := getTaskRevisionTx(ctx, tx, record.RevisionID)
	if err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	if revision.TaskID != record.TaskID || revision.TaskDigest != record.TaskDigest {
		return CodeEdgeComplianceRecord{}, fmt.Errorf("CodeEdge compliance task/revision/digest binding does not match durable revision")
	}
	if _, err := getTaskV2Tx(ctx, tx, record.TaskID); err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO codeedge_compliance_records_v20 (
			id, run_id, task_id, revision_id, task_digest, status,
			qwen_receipt_json, opus_receipt_json, submission_receipt_json,
			decision_json, decision_fingerprint, authorization_json,
			authorization_fingerprint, idempotency_key, created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, record.ID, record.RunID, record.TaskID, record.RevisionID, record.TaskDigest, record.Status,
		record.QwenReceiptJSON, record.OpusReceiptJSON, record.SubmissionReceiptJSON,
		record.DecisionJSON, record.DecisionFingerprint, record.AuthorizationJSON,
		record.AuthorizationFingerprint, record.IdempotencyKey, record.CreatedBy, record.CreatedAt)
	if err != nil {
		if !isUniqueConstraint(err) && !isGlobalIdentityCollision(err) {
			return CodeEdgeComplianceRecord{}, err
		}
		if existing, existingErr := getCodeEdgeComplianceRecordByKeyTx(ctx, tx, record.IdempotencyKey); existingErr == nil {
			if sameCodeEdgeComplianceRecord(existing, record) {
				if err := tx.Commit(); err != nil {
					return CodeEdgeComplianceRecord{}, err
				}
				return existing, nil
			}
			return CodeEdgeComplianceRecord{}, fmt.Errorf("%w: CodeEdge compliance key %s", ErrIdempotencyConflict, record.IdempotencyKey)
		} else if !isNotFound(existingErr) {
			return CodeEdgeComplianceRecord{}, existingErr
		}
		if existing, existingErr := getCodeEdgeComplianceRecordByRunTx(ctx, tx, record.RunID); existingErr == nil {
			if sameCodeEdgeComplianceRecord(existing, record) {
				if err := tx.Commit(); err != nil {
					return CodeEdgeComplianceRecord{}, err
				}
				return existing, nil
			}
			return CodeEdgeComplianceRecord{}, fmt.Errorf("%w: CodeEdge compliance Run %s", ErrIdempotencyConflict, record.RunID)
		} else if !isNotFound(existingErr) {
			return CodeEdgeComplianceRecord{}, existingErr
		}
		return CodeEdgeComplianceRecord{}, fmt.Errorf("%w: CodeEdge compliance record %s", ErrIdentityCollision, record.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: record.CreatedBy, EntityType: "codeedge_compliance_record", EntityID: record.ID,
		Action: "codeedge_compliance.recorded", Reason: request.Reason,
		PayloadJSON: auditPayload(map[string]any{
			"run_id": record.RunID, "revision_id": record.RevisionID, "status": record.Status,
			"decision_fingerprint": record.DecisionFingerprint, "authorization_fingerprint": record.AuthorizationFingerprint,
		}),
		CreatedAt: now,
	}); err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return CodeEdgeComplianceRecord{}, err
	}
	return record, nil
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
func validateCanonicalCodeEdgeComplianceDocuments(taskDigest string, status CodeEdgeComplianceStatus, qwenRaw, opusRaw, submissionRaw, decisionRaw, decisionFingerprint, authorizationRaw, authorizationFingerprint string) error {
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
	binding := codeedge.FrozenRunBinding{
		TaskSnapshotDigest:  receipt.TaskSnapshotDigest,
		CatalogFingerprint:  receipt.CatalogFingerprint,
		LockFingerprint:     receipt.LockFingerprint,
		ManifestFingerprint: receipt.ManifestFingerprint,
	}
	expected := decision.QwenReceiptFingerprint
	if role == "Opus" {
		expected = decision.OpusReceiptFingerprint
	}
	if binding != decision.Binding || expected != fingerprint {
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
