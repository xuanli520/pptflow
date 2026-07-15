package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const codeEdgeEvaluatorEvidenceHandoffSelect = `
	SELECT id, parent_run_id, child_run_id, task_id, revision_id, task_digest,
	       parent_catalog_fingerprint, parent_lock_fingerprint, parent_manifest_fingerprint, parent_definition_fingerprint,
	       child_catalog_fingerprint, child_lock_fingerprint, child_manifest_fingerprint, child_definition_fingerprint,
	       qwen_stage_attempt_id, qwen_bundle_artifact_id, qwen_bundle_content_digest, qwen_bundle_schema_version,
	       qwen_screenshot_artifact_id, qwen_screenshot_content_digest, qwen_screenshot_schema_version, qwen_trial_set_fingerprint,
	       opus_stage_attempt_id, opus_bundle_artifact_id, opus_bundle_content_digest, opus_bundle_schema_version,
	       opus_screenshot_artifact_id, opus_screenshot_content_digest, opus_screenshot_schema_version, opus_trial_set_fingerprint,
	       handoff_json, handoff_fingerprint, idempotency_key, created_by, created_at
	FROM codeedge_evaluator_evidence_handoffs_v2`

// CreateCodeEdgeEvaluatorEvidenceHandoff records the one immutable evidence
// adoption allowed for a parent and child Run pair. The application service
// has already rebuilt the receipts from child artifact bytes; Store repeats
// all durable identity/lineage checks so a direct caller cannot link foreign
// runs or artifact rows.
func (s *Store) CreateCodeEdgeEvaluatorEvidenceHandoff(ctx context.Context, request CreateCodeEdgeEvaluatorEvidenceHandoffRequest) (CodeEdgeEvaluatorEvidenceHandoff, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	prepared, err := prepareCodeEdgeEvaluatorEvidenceHandoff(request)
	if err != nil {
		return CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	prepared.CreatedAt = s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	defer tx.Rollback()
	if err := validateCodeEdgeEvaluatorEvidenceHandoffLineage(ctx, tx, prepared); err != nil {
		return CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	if existing, lookupErr := getCodeEdgeEvaluatorEvidenceHandoffByKeyTx(ctx, tx, prepared.IdempotencyKey); lookupErr == nil {
		if sameCodeEdgeEvaluatorEvidenceHandoff(existing, prepared) {
			if err := tx.Commit(); err != nil {
				return CodeEdgeEvaluatorEvidenceHandoff{}, err
			}
			return existing, nil
		}
		return CodeEdgeEvaluatorEvidenceHandoff{}, fmt.Errorf("%w: CodeEdge evaluator evidence handoff key %s", ErrIdempotencyConflict, prepared.IdempotencyKey)
	} else if !isNotFound(lookupErr) {
		return CodeEdgeEvaluatorEvidenceHandoff{}, lookupErr
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO codeedge_evaluator_evidence_handoffs_v2 (
			id, parent_run_id, child_run_id, task_id, revision_id, task_digest,
			parent_catalog_fingerprint, parent_lock_fingerprint, parent_manifest_fingerprint, parent_definition_fingerprint,
			child_catalog_fingerprint, child_lock_fingerprint, child_manifest_fingerprint, child_definition_fingerprint,
			qwen_stage_attempt_id, qwen_bundle_artifact_id, qwen_bundle_content_digest, qwen_bundle_schema_version,
			qwen_screenshot_artifact_id, qwen_screenshot_content_digest, qwen_screenshot_schema_version, qwen_trial_set_fingerprint,
			opus_stage_attempt_id, opus_bundle_artifact_id, opus_bundle_content_digest, opus_bundle_schema_version,
			opus_screenshot_artifact_id, opus_screenshot_content_digest, opus_screenshot_schema_version, opus_trial_set_fingerprint,
			handoff_json, handoff_fingerprint, idempotency_key, created_by, created_at
		) VALUES (
			?, ?, ?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?, ?
		)
	`, prepared.ID, prepared.ParentRunID, prepared.ChildRunID, prepared.TaskID, prepared.RevisionID, prepared.TaskDigest,
		prepared.ParentCatalogFingerprint, prepared.ParentLockFingerprint, prepared.ParentManifestFingerprint, prepared.ParentDefinitionFingerprint,
		prepared.ChildCatalogFingerprint, prepared.ChildLockFingerprint, prepared.ChildManifestFingerprint, prepared.ChildDefinitionFingerprint,
		prepared.QwenStageAttemptID, prepared.QwenBundle.ArtifactID, prepared.QwenBundle.ContentDigest, prepared.QwenBundle.SchemaVersion,
		prepared.QwenScreenshot.ArtifactID, prepared.QwenScreenshot.ContentDigest, prepared.QwenScreenshot.SchemaVersion, prepared.QwenTrialSetFingerprint,
		prepared.OpusStageAttemptID, prepared.OpusBundle.ArtifactID, prepared.OpusBundle.ContentDigest, prepared.OpusBundle.SchemaVersion,
		prepared.OpusScreenshot.ArtifactID, prepared.OpusScreenshot.ContentDigest, prepared.OpusScreenshot.SchemaVersion, prepared.OpusTrialSetFingerprint,
		prepared.HandoffJSON, prepared.HandoffFingerprint, prepared.IdempotencyKey, prepared.CreatedBy, prepared.CreatedAt)
	if err != nil {
		if !isUniqueConstraint(err) && !isGlobalIdentityCollision(err) {
			return CodeEdgeEvaluatorEvidenceHandoff{}, err
		}
		if existing, lookupErr := getCodeEdgeEvaluatorEvidenceHandoffByKeyTx(ctx, tx, prepared.IdempotencyKey); lookupErr == nil {
			if sameCodeEdgeEvaluatorEvidenceHandoff(existing, prepared) {
				if err := tx.Commit(); err != nil {
					return CodeEdgeEvaluatorEvidenceHandoff{}, err
				}
				return existing, nil
			}
			return CodeEdgeEvaluatorEvidenceHandoff{}, fmt.Errorf("%w: CodeEdge evaluator evidence handoff key %s", ErrIdempotencyConflict, prepared.IdempotencyKey)
		} else if !isNotFound(lookupErr) {
			return CodeEdgeEvaluatorEvidenceHandoff{}, lookupErr
		}
		return CodeEdgeEvaluatorEvidenceHandoff{}, fmt.Errorf("%w: CodeEdge evaluator evidence handoff %s", ErrIdentityCollision, prepared.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: prepared.CreatedBy, EntityType: "codeedge_evaluator_evidence_handoff", EntityID: prepared.ID,
		Action: "codeedge_evaluator_evidence_handoff.recorded", Reason: request.Reason,
		PayloadJSON: auditPayload(map[string]any{
			"parent_run_id": prepared.ParentRunID, "child_run_id": prepared.ChildRunID,
			"handoff_fingerprint": prepared.HandoffFingerprint,
		}),
		CreatedAt: prepared.CreatedAt,
	}); err != nil {
		return CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	if err := tx.Commit(); err != nil {
		return CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	return prepared, nil
}

func (s *Store) GetCodeEdgeEvaluatorEvidenceHandoff(ctx context.Context, handoffID string) (*CodeEdgeEvaluatorEvidenceHandoff, error) {
	if !isUUIDv7(strings.TrimSpace(handoffID)) {
		return nil, ErrInvalidUUIDv7Identity
	}
	handoff, err := scanCodeEdgeEvaluatorEvidenceHandoff(s.db.QueryRowContext(ctx, codeEdgeEvaluatorEvidenceHandoffSelect+" WHERE id = ?", handoffID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &handoff, nil
}

func (s *Store) GetCodeEdgeEvaluatorEvidenceHandoffForParentRun(ctx context.Context, parentRunID string) (*CodeEdgeEvaluatorEvidenceHandoff, error) {
	if !isUUIDv7(strings.TrimSpace(parentRunID)) {
		return nil, ErrInvalidUUIDv7Identity
	}
	handoff, err := scanCodeEdgeEvaluatorEvidenceHandoff(s.db.QueryRowContext(ctx, codeEdgeEvaluatorEvidenceHandoffSelect+" WHERE parent_run_id = ?", parentRunID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &handoff, nil
}

func prepareCodeEdgeEvaluatorEvidenceHandoff(request CreateCodeEdgeEvaluatorEvidenceHandoffRequest) (CodeEdgeEvaluatorEvidenceHandoff, error) {
	key := strings.TrimSpace(request.IdempotencyKey)
	if !isUUIDv7(key) {
		return CodeEdgeEvaluatorEvidenceHandoff{}, ErrInvalidUUIDv7Identity
	}
	if requested := strings.TrimSpace(request.ID); requested != "" && requested != key {
		return CodeEdgeEvaluatorEvidenceHandoff{}, fmt.Errorf("%w: CodeEdge evaluator evidence handoff identity differs from idempotency key", ErrIdempotencyConflict)
	}
	if !isUUIDv7(request.ParentRunID) || !isUUIDv7(request.ChildRunID) || !isUUIDv7(request.TaskID) || !isUUIDv7(request.RevisionID) ||
		!isUUIDv7(request.QwenStageAttemptID) || !isUUIDv7(request.OpusStageAttemptID) ||
		!isUUIDv7(request.QwenBundle.ArtifactID) || !isUUIDv7(request.QwenScreenshot.ArtifactID) ||
		!isUUIDv7(request.OpusBundle.ArtifactID) || !isUUIDv7(request.OpusScreenshot.ArtifactID) {
		return CodeEdgeEvaluatorEvidenceHandoff{}, ErrInvalidUUIDv7Identity
	}
	if err := ValidateTaskDigestV2(request.TaskDigest); err != nil {
		return CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	var document codeedge.EvaluatorEvidenceHandoff
	if err := decodeCanonicalCodeEdgeDocument(request.HandoffJSON, "CodeEdge evaluator evidence handoff", &document, codeedge.EvaluatorEvidenceHandoff.CanonicalJSON); err != nil {
		return CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	documentFingerprint, err := document.Fingerprint()
	if err != nil {
		return CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	if strings.TrimSpace(request.HandoffFingerprint) != string(documentFingerprint) {
		return CodeEdgeEvaluatorEvidenceHandoff{}, fmt.Errorf("CodeEdge evaluator evidence handoff fingerprint does not match canonical document")
	}
	if err := matchCodeEdgeEvaluatorEvidenceHandoffRequest(request, document); err != nil {
		return CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"parent catalog fingerprint", request.ParentCatalogFingerprint},
		{"parent lock fingerprint", request.ParentLockFingerprint},
		{"parent manifest fingerprint", request.ParentManifestFingerprint},
		{"parent definition fingerprint", request.ParentDefinitionFingerprint},
		{"child catalog fingerprint", request.ChildCatalogFingerprint},
		{"child lock fingerprint", request.ChildLockFingerprint},
		{"child manifest fingerprint", request.ChildManifestFingerprint},
		{"child definition fingerprint", request.ChildDefinitionFingerprint},
		{"Qwen trial set fingerprint", request.QwenTrialSetFingerprint},
		{"Opus trial set fingerprint", request.OpusTrialSetFingerprint},
		{"handoff JSON", request.HandoffJSON},
		{"handoff fingerprint", request.HandoffFingerprint},
		{"Qwen bundle digest", request.QwenBundle.ContentDigest},
		{"Qwen bundle schema", request.QwenBundle.SchemaVersion},
		{"Qwen screenshot digest", request.QwenScreenshot.ContentDigest},
		{"Qwen screenshot schema", request.QwenScreenshot.SchemaVersion},
		{"Opus bundle digest", request.OpusBundle.ContentDigest},
		{"Opus bundle schema", request.OpusBundle.SchemaVersion},
		{"Opus screenshot digest", request.OpusScreenshot.ContentDigest},
		{"Opus screenshot schema", request.OpusScreenshot.SchemaVersion},
	} {
		if _, err := normalizeRequired(field.value, field.name); err != nil {
			return CodeEdgeEvaluatorEvidenceHandoff{}, err
		}
	}
	return CodeEdgeEvaluatorEvidenceHandoff{
		ID: key, ParentRunID: strings.TrimSpace(request.ParentRunID), ChildRunID: strings.TrimSpace(request.ChildRunID),
		TaskID: strings.TrimSpace(request.TaskID), RevisionID: strings.TrimSpace(request.RevisionID), TaskDigest: strings.TrimSpace(request.TaskDigest),
		ParentCatalogFingerprint: strings.TrimSpace(request.ParentCatalogFingerprint), ParentLockFingerprint: strings.TrimSpace(request.ParentLockFingerprint),
		ParentManifestFingerprint: strings.TrimSpace(request.ParentManifestFingerprint), ParentDefinitionFingerprint: strings.TrimSpace(request.ParentDefinitionFingerprint),
		ChildCatalogFingerprint: strings.TrimSpace(request.ChildCatalogFingerprint), ChildLockFingerprint: strings.TrimSpace(request.ChildLockFingerprint),
		ChildManifestFingerprint: strings.TrimSpace(request.ChildManifestFingerprint), ChildDefinitionFingerprint: strings.TrimSpace(request.ChildDefinitionFingerprint),
		QwenStageAttemptID: strings.TrimSpace(request.QwenStageAttemptID), QwenBundle: request.QwenBundle, QwenScreenshot: request.QwenScreenshot,
		QwenTrialSetFingerprint: strings.TrimSpace(request.QwenTrialSetFingerprint), OpusStageAttemptID: strings.TrimSpace(request.OpusStageAttemptID),
		OpusBundle: request.OpusBundle, OpusScreenshot: request.OpusScreenshot, OpusTrialSetFingerprint: strings.TrimSpace(request.OpusTrialSetFingerprint),
		HandoffJSON: strings.TrimSpace(request.HandoffJSON), HandoffFingerprint: strings.TrimSpace(request.HandoffFingerprint),
		IdempotencyKey: key, CreatedBy: resolveActor(request.Actor),
	}, nil
}

func matchCodeEdgeEvaluatorEvidenceHandoffRequest(request CreateCodeEdgeEvaluatorEvidenceHandoffRequest, document codeedge.EvaluatorEvidenceHandoff) error {
	if document.ParentRunID != strings.TrimSpace(request.ParentRunID) || document.ChildRunID != strings.TrimSpace(request.ChildRunID) ||
		string(document.ParentBinding.TaskSnapshotDigest) != strings.TrimSpace(request.TaskDigest) ||
		string(document.ParentBinding.CatalogFingerprint) != strings.TrimSpace(request.ParentCatalogFingerprint) ||
		string(document.ParentBinding.LockFingerprint) != strings.TrimSpace(request.ParentLockFingerprint) ||
		string(document.ParentBinding.ManifestFingerprint) != strings.TrimSpace(request.ParentManifestFingerprint) ||
		string(document.ParentDefinitionFingerprint) != strings.TrimSpace(request.ParentDefinitionFingerprint) ||
		string(document.ChildBinding.CatalogFingerprint) != strings.TrimSpace(request.ChildCatalogFingerprint) ||
		string(document.ChildBinding.LockFingerprint) != strings.TrimSpace(request.ChildLockFingerprint) ||
		string(document.ChildBinding.ManifestFingerprint) != strings.TrimSpace(request.ChildManifestFingerprint) ||
		string(document.ChildDefinitionFingerprint) != strings.TrimSpace(request.ChildDefinitionFingerprint) ||
		document.Qwen.ChildStageAttemptID != strings.TrimSpace(request.QwenStageAttemptID) ||
		document.Opus.ChildStageAttemptID != strings.TrimSpace(request.OpusStageAttemptID) ||
		string(document.Qwen.RunBundle.ArtifactID) != strings.TrimSpace(request.QwenBundle.ArtifactID) ||
		string(document.Qwen.RunBundle.ContentDigest) != strings.TrimSpace(request.QwenBundle.ContentDigest) ||
		document.Qwen.RunBundle.SchemaVersion != strings.TrimSpace(request.QwenBundle.SchemaVersion) ||
		string(document.Qwen.CanonicalScreenshot.ArtifactID) != strings.TrimSpace(request.QwenScreenshot.ArtifactID) ||
		string(document.Qwen.CanonicalScreenshot.ContentDigest) != strings.TrimSpace(request.QwenScreenshot.ContentDigest) ||
		document.Qwen.CanonicalScreenshot.SchemaVersion != strings.TrimSpace(request.QwenScreenshot.SchemaVersion) ||
		string(document.Opus.RunBundle.ArtifactID) != strings.TrimSpace(request.OpusBundle.ArtifactID) ||
		string(document.Opus.RunBundle.ContentDigest) != strings.TrimSpace(request.OpusBundle.ContentDigest) ||
		document.Opus.RunBundle.SchemaVersion != strings.TrimSpace(request.OpusBundle.SchemaVersion) ||
		string(document.Opus.CanonicalScreenshot.ArtifactID) != strings.TrimSpace(request.OpusScreenshot.ArtifactID) ||
		string(document.Opus.CanonicalScreenshot.ContentDigest) != strings.TrimSpace(request.OpusScreenshot.ContentDigest) ||
		document.Opus.CanonicalScreenshot.SchemaVersion != strings.TrimSpace(request.OpusScreenshot.SchemaVersion) ||
		string(document.Qwen.TrialSetFingerprint) != strings.TrimSpace(request.QwenTrialSetFingerprint) ||
		string(document.Opus.TrialSetFingerprint) != strings.TrimSpace(request.OpusTrialSetFingerprint) {
		return fmt.Errorf("%w: CodeEdge evaluator evidence handoff request differs from canonical document", ErrIdempotencyConflict)
	}
	return nil
}

func validateCodeEdgeEvaluatorEvidenceHandoffLineage(ctx context.Context, tx *sql.Tx, handoff CodeEdgeEvaluatorEvidenceHandoff) error {
	parent, err := getWorkflowRunTx(ctx, tx, handoff.ParentRunID)
	if err != nil {
		return err
	}
	child, err := getWorkflowRunTx(ctx, tx, handoff.ChildRunID)
	if err != nil {
		return err
	}
	if parent.TaskID != handoff.TaskID || parent.RevisionID != handoff.RevisionID || child.TaskID != handoff.TaskID || child.RevisionID != handoff.RevisionID || child.ParentRunID != parent.ID {
		return fmt.Errorf("CodeEdge evaluator evidence handoff parent/child Run lineage does not match task revision")
	}
	if parent.WorkflowTemplateID != workflowadapter.CodeEdgePhase1WorkflowTemplateID || parent.WorkflowTemplateVersion != workflowadapter.CodeEdgePhase1WorkflowTemplateVersion ||
		child.WorkflowTemplateID != workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID || child.WorkflowTemplateVersion != workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion ||
		parent.DefinitionHash != handoff.ParentDefinitionFingerprint || child.DefinitionHash != handoff.ChildDefinitionFingerprint {
		return fmt.Errorf("CodeEdge evaluator evidence handoff does not match the closed parent/child workflow definitions")
	}
	if child.Status != WorkflowRunSucceeded {
		return fmt.Errorf("CodeEdge evaluator evidence handoff child Run is not succeeded")
	}
	revision, err := getTaskRevisionTx(ctx, tx, handoff.RevisionID)
	if err != nil {
		return err
	}
	if revision.TaskID != handoff.TaskID || revision.TaskDigest != handoff.TaskDigest {
		return fmt.Errorf("CodeEdge evaluator evidence handoff task revision digest does not match durable revision")
	}
	if err := validateCodeEdgeEvaluatorEvidenceHandoffStage(ctx, tx, handoff.ChildRunID, handoff.RevisionID, handoff.TaskDigest, handoff.QwenStageAttemptID, "harbor_run_qwen", "qwen_trial_result", handoff.QwenBundle, "qwen_pass4_evidence", handoff.QwenScreenshot); err != nil {
		return err
	}
	if err := validateCodeEdgeEvaluatorEvidenceHandoffStage(ctx, tx, handoff.ChildRunID, handoff.RevisionID, handoff.TaskDigest, handoff.OpusStageAttemptID, "harbor_run_opus", "opus_trial_result", handoff.OpusBundle, "opus_pass4_evidence", handoff.OpusScreenshot); err != nil {
		return err
	}
	if err := validateCodeEdgeEvaluatorEvidenceHandoffTrialSet(ctx, tx, child, handoff.QwenStageAttemptID, handoff.QwenTrialSetFingerprint); err != nil {
		return err
	}
	return validateCodeEdgeEvaluatorEvidenceHandoffTrialSet(ctx, tx, child, handoff.OpusStageAttemptID, handoff.OpusTrialSetFingerprint)
}

func validateCodeEdgeEvaluatorEvidenceHandoffTrialSet(ctx context.Context, tx *sql.Tx, child WorkflowRun, stageID, expectedFingerprint string) error {
	rows, err := tx.QueryContext(ctx, trialExecutionSelect+" WHERE stage_attempt_id = ? ORDER BY ordinal ASC, id ASC", stageID)
	if err != nil {
		return err
	}
	executions, err := scanTrialExecutions(rows)
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if len(executions) != 4 {
		return fmt.Errorf("CodeEdge evaluator evidence handoff stage %s must retain exactly four logical trials", stageID)
	}
	parts := make([]workflowkit.FingerprintPart, 0, len(executions)*2)
	for index, execution := range executions {
		if execution.RunID != child.ID || execution.StageAttemptID != stageID || execution.Ordinal != index+1 || execution.Status != TrialExecutionCompleted {
			return fmt.Errorf("CodeEdge evaluator evidence handoff trial does not match completed child stage")
		}
		attemptRows, attemptErr := tx.QueryContext(ctx, trialAttemptSelect+" WHERE trial_execution_id = ? ORDER BY ordinal ASC, id ASC", execution.ID)
		if attemptErr != nil {
			return attemptErr
		}
		attempts, scanErr := scanTrialAttempts(attemptRows)
		closeAttemptErr := attemptRows.Close()
		if scanErr != nil {
			return scanErr
		}
		if closeAttemptErr != nil {
			return closeAttemptErr
		}
		if len(attempts) == 0 || attempts[len(attempts)-1].Status != TrialAttemptCompleted {
			return fmt.Errorf("CodeEdge evaluator evidence handoff trial has no completed final technical attempt")
		}
		parts = append(parts,
			workflowkit.FingerprintPart{Name: fmt.Sprintf("trial_%d", execution.Ordinal), Value: []byte(execution.ID)},
			workflowkit.FingerprintPart{Name: fmt.Sprintf("attempt_%d", execution.Ordinal), Value: []byte(attempts[len(attempts)-1].ID)},
		)
	}
	fingerprint, err := workflowkit.FingerprintParts("harbor.codeedge.evaluator-child-trial-set.v1", parts)
	if err != nil {
		return err
	}
	if string(fingerprint) != expectedFingerprint {
		return fmt.Errorf("CodeEdge evaluator evidence handoff trial set fingerprint does not match completed child trials")
	}
	return nil
}

func validateCodeEdgeEvaluatorEvidenceHandoffStage(ctx context.Context, tx *sql.Tx, childRunID, revisionID, digest, stageID, stageKey, bundleKey string, bundle CodeEdgeEvaluatorEvidenceArtifact, screenshotKey string, screenshot CodeEdgeEvaluatorEvidenceArtifact) error {
	stage, err := getStageAttemptTx(ctx, tx, stageID)
	if err != nil {
		return err
	}
	if stage.RunID != childRunID || stage.StageKey != stageKey || stage.ExecutionStatus != StageExecutionCompleted {
		return fmt.Errorf("CodeEdge evaluator evidence handoff child stage %s is not a completed %s stage", stageID, stageKey)
	}
	if err := validateCodeEdgeEvaluatorEvidenceArtifact(ctx, tx, childRunID, revisionID, digest, stage, bundleKey, bundle); err != nil {
		return err
	}
	return validateCodeEdgeEvaluatorEvidenceArtifact(ctx, tx, childRunID, revisionID, digest, stage, screenshotKey, screenshot)
}

func validateCodeEdgeEvaluatorEvidenceArtifact(ctx context.Context, tx *sql.Tx, childRunID, revisionID, digest string, stage StageAttempt, key string, expected CodeEdgeEvaluatorEvidenceArtifact) error {
	ref, err := scanArtifactRef(tx.QueryRowContext(ctx, artifactRefV4Select+" WHERE id = ?", expected.ArtifactID))
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: CodeEdge evaluator evidence artifact %s", ErrNotFound, expected.ArtifactID)
	}
	if err != nil {
		return err
	}
	if ref.RunID != childRunID || ref.StageKey != stage.StageKey || ref.AttemptID != stage.ID || ref.ArtifactKey != key ||
		ref.SubjectRevisionID != revisionID || ref.SubjectDigest != digest || ref.ContentDigest != expected.ContentDigest || ref.SchemaVersion != expected.SchemaVersion {
		return fmt.Errorf("CodeEdge evaluator evidence artifact %s does not match child stage lineage", expected.ArtifactID)
	}
	return nil
}

func getCodeEdgeEvaluatorEvidenceHandoffByKeyTx(ctx context.Context, tx *sql.Tx, key string) (CodeEdgeEvaluatorEvidenceHandoff, error) {
	handoff, err := scanCodeEdgeEvaluatorEvidenceHandoff(tx.QueryRowContext(ctx, codeEdgeEvaluatorEvidenceHandoffSelect+" WHERE idempotency_key = ?", key))
	if err == sql.ErrNoRows {
		return CodeEdgeEvaluatorEvidenceHandoff{}, fmt.Errorf("%w: CodeEdge evaluator evidence handoff key %s", ErrNotFound, key)
	}
	return handoff, err
}

func scanCodeEdgeEvaluatorEvidenceHandoff(scanner rowScanner) (CodeEdgeEvaluatorEvidenceHandoff, error) {
	var handoff CodeEdgeEvaluatorEvidenceHandoff
	if err := scanner.Scan(
		&handoff.ID, &handoff.ParentRunID, &handoff.ChildRunID, &handoff.TaskID, &handoff.RevisionID, &handoff.TaskDigest,
		&handoff.ParentCatalogFingerprint, &handoff.ParentLockFingerprint, &handoff.ParentManifestFingerprint, &handoff.ParentDefinitionFingerprint,
		&handoff.ChildCatalogFingerprint, &handoff.ChildLockFingerprint, &handoff.ChildManifestFingerprint, &handoff.ChildDefinitionFingerprint,
		&handoff.QwenStageAttemptID, &handoff.QwenBundle.ArtifactID, &handoff.QwenBundle.ContentDigest, &handoff.QwenBundle.SchemaVersion,
		&handoff.QwenScreenshot.ArtifactID, &handoff.QwenScreenshot.ContentDigest, &handoff.QwenScreenshot.SchemaVersion, &handoff.QwenTrialSetFingerprint,
		&handoff.OpusStageAttemptID, &handoff.OpusBundle.ArtifactID, &handoff.OpusBundle.ContentDigest, &handoff.OpusBundle.SchemaVersion,
		&handoff.OpusScreenshot.ArtifactID, &handoff.OpusScreenshot.ContentDigest, &handoff.OpusScreenshot.SchemaVersion, &handoff.OpusTrialSetFingerprint,
		&handoff.HandoffJSON, &handoff.HandoffFingerprint, &handoff.IdempotencyKey, &handoff.CreatedBy, &handoff.CreatedAt,
	); err != nil {
		return CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	handoff.CreatedAt = handoff.CreatedAt.UTC()
	return handoff, nil
}

func sameCodeEdgeEvaluatorEvidenceHandoff(left, right CodeEdgeEvaluatorEvidenceHandoff) bool {
	return left.ID == right.ID && left.ParentRunID == right.ParentRunID && left.ChildRunID == right.ChildRunID &&
		left.TaskID == right.TaskID && left.RevisionID == right.RevisionID && left.TaskDigest == right.TaskDigest &&
		left.ParentCatalogFingerprint == right.ParentCatalogFingerprint && left.ParentLockFingerprint == right.ParentLockFingerprint &&
		left.ParentManifestFingerprint == right.ParentManifestFingerprint && left.ParentDefinitionFingerprint == right.ParentDefinitionFingerprint &&
		left.ChildCatalogFingerprint == right.ChildCatalogFingerprint && left.ChildLockFingerprint == right.ChildLockFingerprint &&
		left.ChildManifestFingerprint == right.ChildManifestFingerprint && left.ChildDefinitionFingerprint == right.ChildDefinitionFingerprint &&
		left.QwenStageAttemptID == right.QwenStageAttemptID && left.QwenBundle == right.QwenBundle && left.QwenScreenshot == right.QwenScreenshot &&
		left.QwenTrialSetFingerprint == right.QwenTrialSetFingerprint && left.OpusStageAttemptID == right.OpusStageAttemptID &&
		left.OpusBundle == right.OpusBundle && left.OpusScreenshot == right.OpusScreenshot && left.OpusTrialSetFingerprint == right.OpusTrialSetFingerprint &&
		left.HandoffJSON == right.HandoffJSON && left.HandoffFingerprint == right.HandoffFingerprint &&
		left.IdempotencyKey == right.IdempotencyKey && left.CreatedBy == right.CreatedBy
}
