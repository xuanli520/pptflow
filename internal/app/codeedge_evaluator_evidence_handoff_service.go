package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// CodeEdgeEvaluatorEvidenceHandoffService adopts one completed evaluator
// child Run for its already-approved Phase-1 parent. It does not execute a
// provider, copy artifact bytes, create parent TrialExecutions, or decide
// final package eligibility; it only records verified provenance.
type CodeEdgeEvaluatorEvidenceHandoffService struct{ core *lifecycleServiceCore }

type RecordCodeEdgeEvaluatorEvidenceHandoffRequest struct {
	ID             string
	IdempotencyKey string
	ParentRunID    string
	ChildRunID     string
	Actor          string
	Reason         string
}

// CodeEdgeEvaluatorEvidenceHandoffPlan is a read-only, UI-safe preflight for
// adopting a completed evaluator child. It contains only durable identities
// and the canonical handoff fingerprint; caller-supplied artifact JSON never
// crosses the application boundary.
type CodeEdgeEvaluatorEvidenceHandoffPlan struct {
	ParentRunID          string
	ChildRunID           string
	TaskID               string
	RevisionID           string
	HandoffFingerprint   workflowkit.Fingerprint
	QwenTrialFingerprint workflowkit.Fingerprint
	OpusTrialFingerprint workflowkit.Fingerprint
}

// Plan verifies the same immutable facts that Record will persist, without
// creating a handoff, lifecycle operation, artifact, trial, or provider side
// effect. CLI and TUI use it for their first confirmation screen.
func (service *CodeEdgeEvaluatorEvidenceHandoffService) Plan(ctx context.Context, parentRunID, childRunID string) (CodeEdgeEvaluatorEvidenceHandoffPlan, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return CodeEdgeEvaluatorEvidenceHandoffPlan{}, fmt.Errorf("CodeEdge evaluator evidence handoff service is not configured")
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(parentRunID)); err != nil {
		return CodeEdgeEvaluatorEvidenceHandoffPlan{}, err
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(childRunID)); err != nil {
		return CodeEdgeEvaluatorEvidenceHandoffPlan{}, err
	}
	parent, err := service.core.loadFrozenCodeEdgeRun(ctx, parentRunID)
	if err != nil {
		return CodeEdgeEvaluatorEvidenceHandoffPlan{}, err
	}
	if err := requireApprovedCodeEdgeReviewGate(ctx, service.core.store, parent.Run, parent.Revision, workflowadapter.FinalReview, workflowadapter.ReviewFinalQuality); err != nil {
		return CodeEdgeEvaluatorEvidenceHandoffPlan{}, fmt.Errorf("CodeEdge evaluator evidence handoff requires an approved parent final review: %w", err)
	}
	handoff, child, _, _, _, qwenTrials, opusTrials, err := service.buildHandoff(ctx, parent, childRunID)
	if err != nil {
		return CodeEdgeEvaluatorEvidenceHandoffPlan{}, err
	}
	fingerprint, err := handoff.Fingerprint()
	if err != nil {
		return CodeEdgeEvaluatorEvidenceHandoffPlan{}, err
	}
	return CodeEdgeEvaluatorEvidenceHandoffPlan{
		ParentRunID:          parent.Run.ID,
		ChildRunID:           child.ID,
		TaskID:               parent.Run.TaskID,
		RevisionID:           parent.Revision.ID,
		HandoffFingerprint:   fingerprint,
		QwenTrialFingerprint: qwenTrials,
		OpusTrialFingerprint: opusTrials,
	}, nil
}

func (service *CodeEdgeEvaluatorEvidenceHandoffService) Record(ctx context.Context, request RecordCodeEdgeEvaluatorEvidenceHandoffRequest) (store.CodeEdgeEvaluatorEvidenceHandoff, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return store.CodeEdgeEvaluatorEvidenceHandoff{}, fmt.Errorf("CodeEdge evaluator evidence handoff service is not configured")
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(request.ParentRunID)); err != nil {
		return store.CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(request.ChildRunID)); err != nil {
		return store.CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(request.IdempotencyKey)); err != nil {
		return store.CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	if requested := strings.TrimSpace(request.ID); requested != "" && requested != strings.TrimSpace(request.IdempotencyKey) {
		return store.CodeEdgeEvaluatorEvidenceHandoff{}, fmt.Errorf("%w: CodeEdge evaluator evidence handoff identity differs from idempotency key", store.ErrIdempotencyConflict)
	}
	parent, err := service.core.loadFrozenCodeEdgeRun(ctx, request.ParentRunID)
	if err != nil {
		return store.CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	if err := requireApprovedCodeEdgeReviewGate(ctx, service.core.store, parent.Run, parent.Revision, workflowadapter.FinalReview, workflowadapter.ReviewFinalQuality); err != nil {
		return store.CodeEdgeEvaluatorEvidenceHandoff{}, fmt.Errorf("CodeEdge evaluator evidence handoff requires an approved parent final review: %w", err)
	}
	handoff, child, _, qwenStage, opusStage, qwenTrials, opusTrials, err := service.buildHandoff(ctx, parent, request.ChildRunID)
	if err != nil {
		return store.CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	handoffRaw, err := handoff.CanonicalJSON()
	if err != nil {
		return store.CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	handoffFingerprint, err := handoff.Fingerprint()
	if err != nil {
		return store.CodeEdgeEvaluatorEvidenceHandoff{}, err
	}
	return service.core.store.CreateCodeEdgeEvaluatorEvidenceHandoff(ctx, store.CreateCodeEdgeEvaluatorEvidenceHandoffRequest{
		ID: request.ID, IdempotencyKey: request.IdempotencyKey, ParentRunID: parent.Run.ID, ChildRunID: child.ID,
		TaskID: parent.Run.TaskID, RevisionID: parent.Revision.ID, TaskDigest: parent.Revision.TaskDigest,
		ParentCatalogFingerprint: string(parent.Binding.CatalogFingerprint), ParentLockFingerprint: string(parent.Binding.LockFingerprint),
		ParentManifestFingerprint: string(parent.Binding.ManifestFingerprint), ParentDefinitionFingerprint: parent.Run.DefinitionHash,
		ChildCatalogFingerprint: string(handoff.ChildBinding.CatalogFingerprint), ChildLockFingerprint: string(handoff.ChildBinding.LockFingerprint),
		ChildManifestFingerprint: string(handoff.ChildBinding.ManifestFingerprint), ChildDefinitionFingerprint: child.DefinitionHash,
		QwenStageAttemptID: qwenStage.ID, QwenBundle: store.CodeEdgeEvaluatorEvidenceArtifact{
			ArtifactID: string(handoff.Qwen.RunBundle.ArtifactID), ContentDigest: string(handoff.Qwen.RunBundle.ContentDigest), SchemaVersion: handoff.Qwen.RunBundle.SchemaVersion,
		}, QwenScreenshot: store.CodeEdgeEvaluatorEvidenceArtifact{
			ArtifactID: string(handoff.Qwen.CanonicalScreenshot.ArtifactID), ContentDigest: string(handoff.Qwen.CanonicalScreenshot.ContentDigest), SchemaVersion: handoff.Qwen.CanonicalScreenshot.SchemaVersion,
		}, QwenTrialSetFingerprint: string(qwenTrials),
		OpusStageAttemptID: opusStage.ID, OpusBundle: store.CodeEdgeEvaluatorEvidenceArtifact{
			ArtifactID: string(handoff.Opus.RunBundle.ArtifactID), ContentDigest: string(handoff.Opus.RunBundle.ContentDigest), SchemaVersion: handoff.Opus.RunBundle.SchemaVersion,
		}, OpusScreenshot: store.CodeEdgeEvaluatorEvidenceArtifact{
			ArtifactID: string(handoff.Opus.CanonicalScreenshot.ArtifactID), ContentDigest: string(handoff.Opus.CanonicalScreenshot.ContentDigest), SchemaVersion: handoff.Opus.CanonicalScreenshot.SchemaVersion,
		}, OpusTrialSetFingerprint: string(opusTrials),
		HandoffJSON: string(handoffRaw), HandoffFingerprint: string(handoffFingerprint), Actor: request.Actor, Reason: request.Reason,
	})
}

// verifiedCodeEdgeEvaluatorEvidenceHandoffGateBinding is persisted inside the
// evaluator-evidence review decision artifact. The parent gate remains unable
// to advance if either the immutable handoff or the child-owned evidence has
// drifted since the operator inspected it.
type verifiedCodeEdgeEvaluatorEvidenceHandoffGateBinding struct {
	ID          string
	Fingerprint workflowkit.Fingerprint
}

func (core *lifecycleServiceCore) verifyCodeEdgeEvaluatorEvidenceHandoffGate(ctx context.Context, binding store.ReviewGateBinding) (verifiedCodeEdgeEvaluatorEvidenceHandoffGateBinding, error) {
	if binding.StageKey != workflowadapter.EvaluatorEvidenceHandoff || binding.ReviewKind != string(workflowadapter.ReviewEvaluatorEvidence) {
		return verifiedCodeEdgeEvaluatorEvidenceHandoffGateBinding{}, nil
	}
	if core == nil || core.store == nil {
		return verifiedCodeEdgeEvaluatorEvidenceHandoffGateBinding{}, fmt.Errorf("CodeEdge evaluator evidence gate verifier is not configured")
	}
	parent, err := core.loadFrozenCodeEdgeRun(ctx, binding.RunID)
	if err != nil {
		return verifiedCodeEdgeEvaluatorEvidenceHandoffGateBinding{}, err
	}
	if parent.Revision.ID != binding.RevisionID || parent.Revision.TaskDigest != binding.RevisionDigest {
		return verifiedCodeEdgeEvaluatorEvidenceHandoffGateBinding{}, fmt.Errorf("CodeEdge evaluator evidence gate does not match its frozen parent revision")
	}
	record, err := core.store.GetCodeEdgeEvaluatorEvidenceHandoffForParentRun(ctx, parent.Run.ID)
	if err != nil {
		return verifiedCodeEdgeEvaluatorEvidenceHandoffGateBinding{}, err
	}
	if record == nil {
		return verifiedCodeEdgeEvaluatorEvidenceHandoffGateBinding{}, fmt.Errorf("CodeEdge evaluator evidence gate requires an adopted immutable handoff")
	}
	verified, err := (&CodeEdgeComplianceService{core: core}).loadVerifiedCodeEdgeEvaluatorEvidenceHandoff(ctx, parent, record.ID)
	if err != nil {
		return verifiedCodeEdgeEvaluatorEvidenceHandoffGateBinding{}, fmt.Errorf("verify adopted CodeEdge evaluator evidence handoff: %w", err)
	}
	fingerprint, err := verified.Fingerprint()
	if err != nil || fingerprint != workflowkit.Fingerprint(record.HandoffFingerprint) {
		return verifiedCodeEdgeEvaluatorEvidenceHandoffGateBinding{}, fmt.Errorf("CodeEdge evaluator evidence handoff fingerprint drifted")
	}
	return verifiedCodeEdgeEvaluatorEvidenceHandoffGateBinding{ID: record.ID, Fingerprint: fingerprint}, nil
}

// loadVerifiedCodeEdgeEvaluatorEvidenceHandoff re-reads the durable record
// and child evidence during final-compliance/package replay. A stored receipt
// is never authority by itself: artifact bytes, stage identity, child trials,
// and both frozen bindings must still agree.
func (service *CodeEdgeComplianceService) loadVerifiedCodeEdgeEvaluatorEvidenceHandoff(ctx context.Context, parent frozenCodeEdgeRun, handoffID string) (codeedge.EvaluatorEvidenceHandoff, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return codeedge.EvaluatorEvidenceHandoff{}, fmt.Errorf("CodeEdge evaluator evidence verifier is not configured")
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(handoffID)); err != nil {
		return codeedge.EvaluatorEvidenceHandoff{}, err
	}
	record, err := service.core.store.GetCodeEdgeEvaluatorEvidenceHandoff(ctx, handoffID)
	if err != nil {
		return codeedge.EvaluatorEvidenceHandoff{}, err
	}
	if record == nil || record.ParentRunID != parent.Run.ID || record.TaskID != parent.Run.TaskID || record.RevisionID != parent.Revision.ID || record.TaskDigest != parent.Revision.TaskDigest {
		return codeedge.EvaluatorEvidenceHandoff{}, fmt.Errorf("CodeEdge evaluator evidence handoff does not belong to the frozen parent Run")
	}
	handoff, err := parseCanonicalCodeEdgeEvaluatorEvidenceHandoff(record.HandoffJSON)
	if err != nil {
		return codeedge.EvaluatorEvidenceHandoff{}, err
	}
	fingerprint, err := handoff.Fingerprint()
	if err != nil || string(fingerprint) != record.HandoffFingerprint || handoff.ParentRunID != parent.Run.ID || handoff.ParentBinding != parent.Binding || handoff.ParentDefinitionFingerprint != workflowkit.Fingerprint(parent.Run.DefinitionHash) {
		return codeedge.EvaluatorEvidenceHandoff{}, fmt.Errorf("CodeEdge evaluator evidence handoff fingerprint or parent binding drifted")
	}
	checker := &CodeEdgeEvaluatorEvidenceHandoffService{core: service.core}
	verified, _, _, _, _, qwenTrials, opusTrials, err := checker.buildHandoff(ctx, parent, record.ChildRunID)
	if err != nil {
		return codeedge.EvaluatorEvidenceHandoff{}, err
	}
	verifiedFingerprint, err := verified.Fingerprint()
	if err != nil || verifiedFingerprint != fingerprint || string(qwenTrials) != record.QwenTrialSetFingerprint || string(opusTrials) != record.OpusTrialSetFingerprint {
		return codeedge.EvaluatorEvidenceHandoff{}, fmt.Errorf("CodeEdge evaluator evidence handoff no longer matches child evidence")
	}
	return verified, nil
}

func (service *CodeEdgeEvaluatorEvidenceHandoffService) buildHandoff(ctx context.Context, parent frozenCodeEdgeRun, childRunID string) (codeedge.EvaluatorEvidenceHandoff, store.WorkflowRun, store.TaskRevision, store.StageAttempt, store.StageAttempt, workflowkit.Fingerprint, workflowkit.Fingerprint, error) {
	child, revision, childBinding, err := service.loadFrozenEvaluatorChild(ctx, parent, childRunID)
	if err != nil {
		return codeedge.EvaluatorEvidenceHandoff{}, store.WorkflowRun{}, store.TaskRevision{}, store.StageAttempt{}, store.StageAttempt{}, "", "", err
	}
	qwenStage, err := requireCodeEdgeCompletedStage(ctx, service.core.store, child, mustChildStageID(ctx, service.core.store, child, workflowadapter.HarborRunQwen), workflowadapter.HarborRunQwen)
	if err != nil {
		return codeedge.EvaluatorEvidenceHandoff{}, store.WorkflowRun{}, store.TaskRevision{}, store.StageAttempt{}, store.StageAttempt{}, "", "", err
	}
	opusStage, err := requireCodeEdgeCompletedStage(ctx, service.core.store, child, mustChildStageID(ctx, service.core.store, child, workflowadapter.HarborRunOpus), workflowadapter.HarborRunOpus)
	if err != nil {
		return codeedge.EvaluatorEvidenceHandoff{}, store.WorkflowRun{}, store.TaskRevision{}, store.StageAttempt{}, store.StageAttempt{}, "", "", err
	}
	qwenTrialSet, err := verifyCompletedCodeEdgeEvaluatorTrials(ctx, service.core.store, child, qwenStage)
	if err != nil {
		return codeedge.EvaluatorEvidenceHandoff{}, store.WorkflowRun{}, store.TaskRevision{}, store.StageAttempt{}, store.StageAttempt{}, "", "", err
	}
	opusTrialSet, err := verifyCompletedCodeEdgeEvaluatorTrials(ctx, service.core.store, child, opusStage)
	if err != nil {
		return codeedge.EvaluatorEvidenceHandoff{}, store.WorkflowRun{}, store.TaskRevision{}, store.StageAttempt{}, store.StageAttempt{}, "", "", err
	}
	childFrozen := frozenCodeEdgeRun{Run: child, Revision: revision, Binding: childBinding}
	qwen, qwenBundle, qwenScreenshot, err := service.rebuildChildEvaluationReceipt(ctx, childFrozen, qwenStage, workflowadapter.HarborRunQwen, "qwen_trial_result", "qwen_pass4_evidence", parent.Policy.QwenPolicy)
	if err != nil {
		return codeedge.EvaluatorEvidenceHandoff{}, store.WorkflowRun{}, store.TaskRevision{}, store.StageAttempt{}, store.StageAttempt{}, "", "", err
	}
	opus, opusBundle, opusScreenshot, err := service.rebuildChildEvaluationReceipt(ctx, childFrozen, opusStage, workflowadapter.HarborRunOpus, "opus_trial_result", "opus_pass4_evidence", parent.Policy.OpusPolicy)
	if err != nil {
		return codeedge.EvaluatorEvidenceHandoff{}, store.WorkflowRun{}, store.TaskRevision{}, store.StageAttempt{}, store.StageAttempt{}, "", "", err
	}
	qwenSource, err := codeEdgeHandoffSource(ctx, service.core.store, qwenStage, qwenBundle, qwenScreenshot, qwenTrialSet, qwen)
	if err != nil {
		return codeedge.EvaluatorEvidenceHandoff{}, store.WorkflowRun{}, store.TaskRevision{}, store.StageAttempt{}, store.StageAttempt{}, "", "", err
	}
	opusSource, err := codeEdgeHandoffSource(ctx, service.core.store, opusStage, opusBundle, opusScreenshot, opusTrialSet, opus)
	if err != nil {
		return codeedge.EvaluatorEvidenceHandoff{}, store.WorkflowRun{}, store.TaskRevision{}, store.StageAttempt{}, store.StageAttempt{}, "", "", err
	}
	handoff := codeedge.EvaluatorEvidenceHandoff{
		Format: codeedge.EvaluatorEvidenceHandoffFormat, Version: codeedge.EvaluatorEvidenceHandoffVersion,
		ParentRunID: parent.Run.ID, ParentDefinitionFingerprint: workflowkit.Fingerprint(parent.Run.DefinitionHash), ParentBinding: parent.Binding,
		ChildRunID: child.ID, ChildTemplateID: child.WorkflowTemplateID, ChildTemplateVersion: child.WorkflowTemplateVersion,
		ChildDefinitionFingerprint: workflowkit.Fingerprint(child.DefinitionHash), ChildBinding: childBinding,
		Qwen: qwenSource, Opus: opusSource,
	}
	if err := handoff.Validate(); err != nil {
		return codeedge.EvaluatorEvidenceHandoff{}, store.WorkflowRun{}, store.TaskRevision{}, store.StageAttempt{}, store.StageAttempt{}, "", "", err
	}
	return handoff, child, revision, qwenStage, opusStage, qwenTrialSet, opusTrialSet, nil
}

func (service *CodeEdgeEvaluatorEvidenceHandoffService) loadFrozenEvaluatorChild(ctx context.Context, parent frozenCodeEdgeRun, childRunID string) (store.WorkflowRun, store.TaskRevision, codeedge.FrozenRunBinding, error) {
	if err := store.ValidateUUIDv7(strings.TrimSpace(childRunID)); err != nil {
		return store.WorkflowRun{}, store.TaskRevision{}, codeedge.FrozenRunBinding{}, err
	}
	child, err := service.core.store.GetWorkflowRun(ctx, childRunID)
	if err != nil {
		return store.WorkflowRun{}, store.TaskRevision{}, codeedge.FrozenRunBinding{}, err
	}
	if child == nil || child.ParentRunID != parent.Run.ID || child.TaskID != parent.Run.TaskID || child.RevisionID != parent.Revision.ID ||
		child.WorkflowTemplateID != workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID || child.WorkflowTemplateVersion != workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion || child.Status != store.WorkflowRunSucceeded {
		return store.WorkflowRun{}, store.TaskRevision{}, codeedge.FrozenRunBinding{}, fmt.Errorf("CodeEdge evaluator child Run does not match completed parent-bound evaluator contract")
	}
	revision, err := service.core.store.GetTaskRevision(ctx, child.RevisionID)
	if err != nil {
		return store.WorkflowRun{}, store.TaskRevision{}, codeedge.FrozenRunBinding{}, err
	}
	if revision == nil || revision.TaskID != child.TaskID || revision.TaskDigest != parent.Revision.TaskDigest {
		return store.WorkflowRun{}, store.TaskRevision{}, codeedge.FrozenRunBinding{}, fmt.Errorf("CodeEdge evaluator child revision does not match parent frozen task")
	}
	profile, specification, err := service.core.verifyRunManagedExecutionInputs(ctx, *child)
	if err != nil || !profile.Template.Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) || !specification.Template.Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) {
		return store.WorkflowRun{}, store.TaskRevision{}, codeedge.FrozenRunBinding{}, fmt.Errorf("CodeEdge evaluator child has invalid frozen execution inputs")
	}
	manifest, manifestFingerprint, err := service.core.verifyManagedRunManifestForTemplate(*child, workflowadapter.CodeEdgeEvaluatorChildTemplateReference(), "CodeEdge evaluator child Run")
	if err != nil {
		return store.WorkflowRun{}, store.TaskRevision{}, codeedge.FrozenRunBinding{}, err
	}
	if err := service.core.verifyPersistedCodeEdgeCatalogProof(*child, manifest, workflowadapter.CodeEdgeEvaluatorChildTemplateReference()); err != nil {
		return store.WorkflowRun{}, store.TaskRevision{}, codeedge.FrozenRunBinding{}, err
	}
	catalogRaw, err := canonicalManifestDeploymentCatalogReceipt(manifest)
	if err != nil || len(catalogRaw) == 0 {
		return store.WorkflowRun{}, store.TaskRevision{}, codeedge.FrozenRunBinding{}, fmt.Errorf("CodeEdge evaluator child has no valid frozen catalog receipt")
	}
	catalogReceipt, err := stageprovider.ParseDeploymentOperationCatalogReceiptJSON(catalogRaw)
	if err != nil || !catalogReceipt.Template.Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) {
		return store.WorkflowRun{}, store.TaskRevision{}, codeedge.FrozenRunBinding{}, fmt.Errorf("CodeEdge evaluator child catalog receipt names another template")
	}
	childTemplateBinding, configured := service.core.configuredDeploymentCatalogBindingForTemplate(workflowadapter.CodeEdgeEvaluatorChildTemplateReference())
	var lockFingerprint workflowkit.Fingerprint
	if configured && childTemplateBinding != nil && childTemplateBinding.lockResolver != nil {
		lockFingerprint = childTemplateBinding.lockResolver.LockIdentity().Fingerprint
	}
	binding := codeedge.FrozenRunBinding{TaskSnapshotDigest: workflowkit.SubjectDigest(revision.TaskDigest), CatalogFingerprint: catalogReceipt.CatalogFingerprint, LockFingerprint: lockFingerprint, ManifestFingerprint: manifestFingerprint}
	if err := binding.Validate(); err != nil {
		return store.WorkflowRun{}, store.TaskRevision{}, codeedge.FrozenRunBinding{}, err
	}
	return *child, *revision, binding, nil
}

func (core *lifecycleServiceCore) verifyPersistedCodeEdgeCatalogProof(run store.WorkflowRun, manifest runManifest, template workflowadapter.TemplateReference) error {
	catalogRaw, err := canonicalManifestDeploymentCatalogReceipt(manifest)
	if err != nil || len(catalogRaw) == 0 {
		return fmt.Errorf("CodeEdge Run has no valid frozen catalog receipt")
	}
	receipt, err := stageprovider.ParseDeploymentOperationCatalogReceiptJSON(catalogRaw)
	if err != nil || !receipt.Template.Equal(template) {
		return fmt.Errorf("CodeEdge Run frozen catalog receipt names another template")
	}
	path := filepath.Join(core.layout.runDirectory(run.ID), deploymentCatalogReceiptFileName)
	raw, err := readManagedRunExecutionInputFile(path, "deployment catalog receipt")
	if err != nil || !bytes.Equal(raw, catalogRaw) {
		return fmt.Errorf("CodeEdge Run managed catalog receipt differs from frozen manifest")
	}
	if binding, configured := core.configuredDeploymentCatalogBindingForTemplate(template); configured && binding != nil && binding.lockResolver != nil {
		if err := binding.lockResolver.VerifyLockIdentity(binding.lockResolver.LockIdentity()); err != nil {
			return err
		}
		if err := core.verifyRunDeploymentCatalogReceipt(run); err != nil {
			return err
		}
	}
	return nil
}

func mustChildStageID(ctx context.Context, dataStore *store.Store, child store.WorkflowRun, stageKey string) string {
	attempts, err := dataStore.ListStageAttemptsForRun(ctx, child.ID)
	if err != nil {
		return ""
	}
	var found string
	for _, attempt := range attempts {
		if attempt.StageKey == stageKey {
			if found != "" {
				return ""
			}
			found = attempt.ID
		}
	}
	return found
}

func verifyCompletedCodeEdgeEvaluatorTrials(ctx context.Context, dataStore *store.Store, child store.WorkflowRun, stage store.StageAttempt) (workflowkit.Fingerprint, error) {
	trials, err := dataStore.ListTrialExecutionsForStageAttempt(ctx, stage.ID)
	if err != nil {
		return "", err
	}
	if len(trials) != codeEdgeEvaluatorTrialCount {
		return "", fmt.Errorf("CodeEdge evaluator child stage %s must retain exactly four logical trials", stage.ID)
	}
	sort.Slice(trials, func(left, right int) bool { return trials[left].Ordinal < trials[right].Ordinal })
	parts := make([]workflowkit.FingerprintPart, 0, len(trials)*2)
	for index, trial := range trials {
		if trial.RunID != child.ID || trial.StageAttemptID != stage.ID || trial.StageKey != stage.StageKey || trial.Ordinal != index+1 || trial.Status != store.TrialExecutionCompleted {
			return "", fmt.Errorf("CodeEdge evaluator child logical trial does not match completed stage")
		}
		attempts, err := dataStore.ListTrialAttemptsForTrialExecution(ctx, trial.ID)
		if err != nil || len(attempts) == 0 {
			return "", fmt.Errorf("CodeEdge evaluator child logical trial has no completed technical attempt")
		}
		last := attempts[len(attempts)-1]
		if last.Status != store.TrialAttemptCompleted {
			return "", fmt.Errorf("CodeEdge evaluator child logical trial has no completed final technical attempt")
		}
		parts = append(parts, workflowkit.FingerprintPart{Name: fmt.Sprintf("trial_%d", trial.Ordinal), Value: []byte(trial.ID)}, workflowkit.FingerprintPart{Name: fmt.Sprintf("attempt_%d", trial.Ordinal), Value: []byte(last.ID)})
	}
	return workflowkit.FingerprintParts("harbor.codeedge.evaluator-child-trial-set.v1", parts)
}

func (service *CodeEdgeEvaluatorEvidenceHandoffService) rebuildChildEvaluationReceipt(ctx context.Context, frozen frozenCodeEdgeRun, stage store.StageAttempt, stageKey, bundleKey, screenshotKey string, policy codeedge.EvaluationPolicy) (codeedge.EvaluationReceipt, store.ArtifactRef, store.ArtifactRef, error) {
	bundle, bundleBytes, err := service.readCodeEdgeStageArtifactReference(ctx, frozen, stage, bundleKey)
	if err != nil {
		return codeedge.EvaluationReceipt{}, store.ArtifactRef{}, store.ArtifactRef{}, err
	}
	screenshot, screenshotBytes, err := service.readCodeEdgeStageArtifactReference(ctx, frozen, stage, screenshotKey)
	if err != nil {
		return codeedge.EvaluationReceipt{}, store.ArtifactRef{}, store.ArtifactRef{}, err
	}
	receipt, err := codeedge.BuildEvaluationReceipt(codeedge.EvaluationInput{
		Policy:              policy,
		Binding:             codeedge.EvaluationBinding{TaskSnapshotDigest: frozen.Binding.TaskSnapshotDigest, CatalogFingerprint: frozen.Binding.CatalogFingerprint, LockFingerprint: frozen.Binding.LockFingerprint, ManifestFingerprint: frozen.Binding.ManifestFingerprint},
		HarborRunBundle:     codeedge.EvaluationEvidence{ArtifactID: workflowkit.ArtifactID(bundle.ID), ContentDigest: workflowkit.Fingerprint(bundle.ContentDigest), SchemaVersion: bundle.SchemaVersion, MediaType: "application/json", Bytes: bundleBytes},
		CanonicalScreenshot: codeedge.EvaluationEvidence{ArtifactID: workflowkit.ArtifactID(screenshot.ID), ContentDigest: workflowkit.Fingerprint(screenshot.ContentDigest), SchemaVersion: screenshot.SchemaVersion, MediaType: "image/png", Bytes: screenshotBytes},
	})
	if err != nil {
		return codeedge.EvaluationReceipt{}, store.ArtifactRef{}, store.ArtifactRef{}, fmt.Errorf("rebuild trusted CodeEdge child %s receipt: %w", stageKey, err)
	}
	return receipt, bundle, screenshot, nil
}

func (service *CodeEdgeEvaluatorEvidenceHandoffService) readCodeEdgeStageArtifactReference(ctx context.Context, frozen frozenCodeEdgeRun, stage store.StageAttempt, artifactKey string) (store.ArtifactRef, []byte, error) {
	refs, err := service.core.store.ListArtifactRefs(ctx, stage.ArtifactManifestID)
	if err != nil {
		return store.ArtifactRef{}, nil, err
	}
	var selected *store.ArtifactRef
	for index := range refs {
		ref := refs[index]
		if ref.ArtifactKey != artifactKey {
			continue
		}
		if selected != nil {
			return store.ArtifactRef{}, nil, fmt.Errorf("CodeEdge child stage has duplicate artifact %q", artifactKey)
		}
		selected = &ref
	}
	if selected == nil || selected.RunID != frozen.Run.ID || selected.StageKey != stage.StageKey || selected.AttemptID != stage.ID || selected.SubjectRevisionID != frozen.Revision.ID || selected.SubjectDigest != frozen.Revision.TaskDigest || selected.WorkflowFingerprint != frozen.Run.DefinitionHash {
		return store.ArtifactRef{}, nil, fmt.Errorf("CodeEdge child artifact %q does not match frozen stage lineage", artifactKey)
	}
	if err := verifyStageArtifactCandidate(ctx, service.core.store, service.core.objects, frozen.Run, frozen.Revision, stageArtifactCandidate{attempt: stage, ref: *selected}); err != nil {
		return store.ArtifactRef{}, nil, err
	}
	index, err := loadStageArtifactManifestIndex(ctx, service.core.store, selected.ManifestID)
	if err != nil {
		return store.ArtifactRef{}, nil, err
	}
	object, err := index.objectFor(*selected)
	if err != nil {
		return store.ArtifactRef{}, nil, err
	}
	content, err := service.core.objects.ReadAll(ctx, object)
	if err != nil {
		return store.ArtifactRef{}, nil, err
	}
	return *selected, content, nil
}

func codeEdgeHandoffSource(ctx context.Context, dataStore *store.Store, stage store.StageAttempt, bundle, screenshot store.ArtifactRef, trialSetFingerprint workflowkit.Fingerprint, receipt codeedge.EvaluationReceipt) (codeedge.EvaluatorEvidenceSource, error) {
	manifest, err := dataStore.GetArtifactManifest(ctx, bundle.ManifestID)
	if err != nil || manifest == nil || manifest.ID != screenshot.ManifestID {
		return codeedge.EvaluatorEvidenceSource{}, fmt.Errorf("CodeEdge child evidence artifacts do not share a valid manifest")
	}
	fingerprint, err := receipt.Fingerprint()
	if err != nil {
		return codeedge.EvaluatorEvidenceSource{}, err
	}
	return codeedge.EvaluatorEvidenceSource{
		ChildStageAttemptID: stage.ID, ArtifactManifestFingerprint: workflowkit.Fingerprint(manifest.ManifestFingerprint),
		RunBundle:           workflowkit.ArtifactBinding{Name: bundle.ArtifactKey, ArtifactID: workflowkit.ArtifactID(bundle.ID), ContentDigest: workflowkit.Fingerprint(bundle.ContentDigest), SchemaVersion: bundle.SchemaVersion},
		CanonicalScreenshot: workflowkit.ArtifactBinding{Name: screenshot.ArtifactKey, ArtifactID: workflowkit.ArtifactID(screenshot.ID), ContentDigest: workflowkit.Fingerprint(screenshot.ContentDigest), SchemaVersion: screenshot.SchemaVersion},
		TrialSetFingerprint: trialSetFingerprint,
		Receipt:             receipt, ReceiptFingerprint: fingerprint,
	}, nil
}

func parseCanonicalCodeEdgeEvaluatorEvidenceHandoff(raw string) (codeedge.EvaluatorEvidenceHandoff, error) {
	var handoff codeedge.EvaluatorEvidenceHandoff
	if err := json.Unmarshal([]byte(raw), &handoff); err != nil {
		return codeedge.EvaluatorEvidenceHandoff{}, err
	}
	canonical, err := handoff.CanonicalJSON()
	if err != nil || !bytes.Equal(canonical, []byte(raw)) {
		return codeedge.EvaluatorEvidenceHandoff{}, fmt.Errorf("CodeEdge evaluator evidence handoff is not canonical")
	}
	return handoff, nil
}

var _ = os.ErrNotExist
