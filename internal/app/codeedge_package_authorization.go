package app

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// resolveCodeEdgePackageAuthorization makes the package boundary prove the
// same frozen evidence again. Recording an approved V20 record is necessary
// but not sufficient: catalog/lock drift, changed managed inputs, a forged
// receipt, or a mismatched selected Run must still block the local ZIP.
func (service *ReleaseService) resolveCodeEdgePackageAuthorization(ctx context.Context, task store.TaskV2, revision store.TaskRevision, request PackageRevisionRequest) (*codeEdgeLocalPackageReceipt, error) {
	runs, err := service.core.store.ListWorkflowRunsForTask(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	hasCodeEdgeRun := false
	for _, run := range runs {
		if run.RevisionID == revision.ID && isCodeEdgePhase1Run(run) {
			hasCodeEdgeRun = true
			break
		}
	}
	if !hasCodeEdgeRun {
		if strings.TrimSpace(request.RunID) != "" || strings.TrimSpace(request.ExpectedComplianceRecordID) != "" || strings.TrimSpace(request.ExpectedAuthorizationFingerprint) != "" {
			return nil, fmt.Errorf("non-CodeEdge package request cannot carry a CodeEdge authorization")
		}
		return nil, nil
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(request.RunID)); err != nil {
		return nil, fmt.Errorf("CodeEdge package requires an approved Run ID: %w", err)
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(request.ExpectedComplianceRecordID)); err != nil {
		return nil, fmt.Errorf("CodeEdge package requires its approved compliance record ID: %w", err)
	}
	expectedAuthorizationFingerprint := workflowkit.Fingerprint(strings.TrimSpace(request.ExpectedAuthorizationFingerprint))
	if err := expectedAuthorizationFingerprint.Validate(); err != nil {
		return nil, fmt.Errorf("CodeEdge package requires its approved authorization fingerprint: %w", err)
	}
	frozen, err := service.core.loadFrozenCodeEdgeRun(ctx, request.RunID)
	if err != nil {
		return nil, err
	}
	if frozen.Run.TaskID != task.ID || frozen.Revision.ID != revision.ID || frozen.Revision.TaskDigest != revision.TaskDigest {
		return nil, fmt.Errorf("CodeEdge package Run does not match the requested task revision")
	}
	record, err := service.core.store.GetCodeEdgeComplianceRecordForRun(ctx, frozen.Run.ID)
	if err != nil {
		return nil, err
	}
	if record == nil || record.Status != store.CodeEdgeComplianceApproved || record.ID != request.ExpectedComplianceRecordID || record.AuthorizationFingerprint != string(expectedAuthorizationFingerprint) || record.TaskID != task.ID || record.RevisionID != revision.ID || record.TaskDigest != revision.TaskDigest {
		return nil, fmt.Errorf("CodeEdge package authorization does not match the selected frozen Run")
	}

	checker := &CodeEdgeComplianceService{core: service.core}
	handoff, err := checker.loadVerifiedCodeEdgeEvaluatorEvidenceHandoff(ctx, frozen, record.EvaluatorEvidenceHandoffID)
	if err != nil {
		return nil, err
	}
	handoffFingerprint, err := handoff.Fingerprint()
	if err != nil || string(handoffFingerprint) != record.EvaluatorEvidenceHandoffFingerprint {
		return nil, fmt.Errorf("CodeEdge package authorization handoff fingerprint drift")
	}
	qwen, err := decodeCanonicalCodeEdgeEvaluationReceipt(record.QwenReceiptJSON, "Qwen")
	if err != nil {
		return nil, err
	}
	opus, err := decodeCanonicalCodeEdgeEvaluationReceipt(record.OpusReceiptJSON, "Opus")
	if err != nil {
		return nil, err
	}
	submission, err := decodeCanonicalCodeEdgeSubmissionReceipt(record.SubmissionReceiptJSON)
	if err != nil {
		return nil, err
	}
	decision, err := decodeCanonicalCodeEdgeDecision(record.DecisionJSON)
	if err != nil {
		return nil, err
	}
	authorization, err := decodeCanonicalCodeEdgeAuthorization(record.AuthorizationJSON)
	if err != nil {
		return nil, err
	}
	if err := requireSameCodeEdgeEvaluationReceipt("Qwen", qwen, handoff.Qwen.Receipt); err != nil {
		return nil, err
	}
	if err := requireSameCodeEdgeEvaluationReceipt("Opus", opus, handoff.Opus.Receipt); err != nil {
		return nil, err
	}
	if err := checker.verifyCodeEdgeSubmissionReceipt(ctx, frozen, submission); err != nil {
		return nil, err
	}
	if err := requireApprovedCodeEdgeReviewGate(ctx, service.core.store, frozen.Run, frozen.Revision, workflowadapter.FinalReview, workflowadapter.ReviewFinalQuality); err != nil {
		return nil, err
	}
	if err := requireApprovedCodeEdgeReviewGate(ctx, service.core.store, frozen.Run, frozen.Revision, workflowadapter.EvaluatorEvidenceHandoff, workflowadapter.ReviewEvaluatorEvidence); err != nil {
		return nil, err
	}
	if err := requireApprovedCodeEdgeReviewGate(ctx, service.core.store, frozen.Run, frozen.Revision, workflowadapter.ResultReview, workflowadapter.ReviewModelResult); err != nil {
		return nil, err
	}

	result, err := (codeedge.FinalComplianceService{}).Evaluate(codeedge.FinalComplianceInput{
		Policy: frozen.Policy, Binding: frozen.Binding, Handoff: handoff, Submission: submission,
	})
	if err != nil {
		return nil, fmt.Errorf("re-evaluate stored CodeEdge final compliance: %w", err)
	}
	if result.Decision.Status != codeedge.FinalComplianceApproved || result.Authorization == nil {
		return nil, fmt.Errorf("stored CodeEdge final compliance no longer authorizes a local package")
	}
	if err := requireSameCodeEdgeDecision(record, decision, result.Decision); err != nil {
		return nil, err
	}
	if err := requireSameCodeEdgeAuthorization(record, authorization, *result.Authorization); err != nil {
		return nil, err
	}
	return &codeEdgeLocalPackageReceipt{
		ComplianceRecordID:       record.ID,
		RunID:                    frozen.Run.ID,
		DecisionFingerprint:      workflowkit.Fingerprint(record.DecisionFingerprint),
		AuthorizationFingerprint: expectedAuthorizationFingerprint,
		Authorization:            authorization,
	}, nil
}

func requireSameCodeEdgeEvaluationReceipt(role string, stored, rebuilt codeedge.EvaluationReceipt) error {
	storedCanonical, err := stored.CanonicalJSON()
	if err != nil {
		return err
	}
	rebuiltCanonical, err := rebuilt.CanonicalJSON()
	if err != nil {
		return err
	}
	if !bytes.Equal(storedCanonical, rebuiltCanonical) {
		return fmt.Errorf("stored CodeEdge %s receipt differs from verified evaluator evidence handoff", role)
	}
	return nil
}

func decodeCanonicalCodeEdgeEvaluationReceipt(raw, role string) (codeedge.EvaluationReceipt, error) {
	var receipt codeedge.EvaluationReceipt
	if err := decodeStrictJSON(raw, &receipt); err != nil {
		return codeedge.EvaluationReceipt{}, fmt.Errorf("decode stored CodeEdge %s receipt: %w", role, err)
	}
	canonical, err := receipt.CanonicalJSON()
	if err != nil {
		return codeedge.EvaluationReceipt{}, err
	}
	if !bytes.Equal(canonical, []byte(raw)) {
		return codeedge.EvaluationReceipt{}, fmt.Errorf("stored CodeEdge %s receipt is not canonical", role)
	}
	return receipt, nil
}

func decodeCanonicalCodeEdgeSubmissionReceipt(raw string) (codeedge.SubmissionCheckReceipt, error) {
	var receipt codeedge.SubmissionCheckReceipt
	if err := decodeStrictJSON(raw, &receipt); err != nil {
		return codeedge.SubmissionCheckReceipt{}, fmt.Errorf("decode stored CodeEdge submission receipt: %w", err)
	}
	canonical, err := receipt.CanonicalJSON()
	if err != nil {
		return codeedge.SubmissionCheckReceipt{}, err
	}
	if !bytes.Equal(canonical, []byte(raw)) {
		return codeedge.SubmissionCheckReceipt{}, fmt.Errorf("stored CodeEdge submission receipt is not canonical")
	}
	return receipt, nil
}

func decodeCanonicalCodeEdgeDecision(raw string) (codeedge.FinalComplianceDecision, error) {
	var decision codeedge.FinalComplianceDecision
	if err := decodeStrictJSON(raw, &decision); err != nil {
		return codeedge.FinalComplianceDecision{}, fmt.Errorf("decode stored CodeEdge final decision: %w", err)
	}
	canonical, err := decision.CanonicalJSON()
	if err != nil {
		return codeedge.FinalComplianceDecision{}, err
	}
	if !bytes.Equal(canonical, []byte(raw)) {
		return codeedge.FinalComplianceDecision{}, fmt.Errorf("stored CodeEdge final decision is not canonical")
	}
	return decision, nil
}

func decodeCanonicalCodeEdgeAuthorization(raw string) (codeedge.LocalPackageAuthorization, error) {
	var authorization codeedge.LocalPackageAuthorization
	if err := decodeStrictJSON(raw, &authorization); err != nil {
		return codeedge.LocalPackageAuthorization{}, fmt.Errorf("decode stored CodeEdge package authorization: %w", err)
	}
	canonical, err := authorization.CanonicalJSON()
	if err != nil {
		return codeedge.LocalPackageAuthorization{}, err
	}
	if !bytes.Equal(canonical, []byte(raw)) {
		return codeedge.LocalPackageAuthorization{}, fmt.Errorf("stored CodeEdge package authorization is not canonical")
	}
	return authorization, nil
}

func requireSameCodeEdgeDecision(record *store.CodeEdgeComplianceRecord, stored, rebuilt codeedge.FinalComplianceDecision) error {
	if record == nil || stored.Status != codeedge.FinalComplianceApproved || rebuilt.Status != codeedge.FinalComplianceApproved {
		return fmt.Errorf("CodeEdge decision is not an approved package decision")
	}
	storedFingerprint, err := stored.Fingerprint()
	if err != nil {
		return err
	}
	rebuiltFingerprint, err := rebuilt.Fingerprint()
	if err != nil {
		return err
	}
	if record.DecisionFingerprint != string(storedFingerprint) || storedFingerprint != rebuiltFingerprint {
		return fmt.Errorf("CodeEdge final decision fingerprint drift")
	}
	storedCanonical, err := stored.CanonicalJSON()
	if err != nil {
		return err
	}
	rebuiltCanonical, err := rebuilt.CanonicalJSON()
	if err != nil {
		return err
	}
	if !bytes.Equal(storedCanonical, rebuiltCanonical) {
		return fmt.Errorf("CodeEdge final decision differs from re-evaluated evidence")
	}
	return nil
}

func requireSameCodeEdgeAuthorization(record *store.CodeEdgeComplianceRecord, stored, rebuilt codeedge.LocalPackageAuthorization) error {
	if record == nil {
		return fmt.Errorf("CodeEdge compliance record is required")
	}
	storedFingerprint, err := stored.Fingerprint()
	if err != nil {
		return err
	}
	rebuiltFingerprint, err := rebuilt.Fingerprint()
	if err != nil {
		return err
	}
	if record.AuthorizationFingerprint != string(storedFingerprint) || storedFingerprint != rebuiltFingerprint {
		return fmt.Errorf("CodeEdge package authorization fingerprint drift")
	}
	storedCanonical, err := stored.CanonicalJSON()
	if err != nil {
		return err
	}
	rebuiltCanonical, err := rebuilt.CanonicalJSON()
	if err != nil {
		return err
	}
	if !bytes.Equal(storedCanonical, rebuiltCanonical) {
		return fmt.Errorf("CodeEdge package authorization differs from re-evaluated decision")
	}
	return nil
}
