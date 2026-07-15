package codeedge

import (
	"encoding/json"
	"fmt"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// EvaluatorEvidenceHandoffFormat identifies the immutable parent-to-child
	// provenance document. It deliberately references the child artifacts and
	// receipts instead of copying them into parent-stage lineage.
	EvaluatorEvidenceHandoffFormat            = "codeedge.phase1.evaluator-evidence-handoff.v2"
	EvaluatorEvidenceHandoffVersion           = "2"
	evaluatorEvidenceHandoffFingerprintDomain = "harbor.codeedge.phase1.evaluator-evidence-handoff.v2"
)

// EvaluatorEvidenceSource identifies one completed child evaluator stage and
// the canonical receipt rebuilt from its immutable bundle and screenshot.
// Artifact bindings remain child-owned. The parent owns only the handoff
// record that adopts their verified evidence for its final decision.
type EvaluatorEvidenceSource struct {
	ChildStageAttemptID         string                      `json:"child_stage_attempt_id"`
	ArtifactManifestFingerprint workflowkit.Fingerprint     `json:"artifact_manifest_fingerprint"`
	RunBundle                   workflowkit.ArtifactBinding `json:"run_bundle"`
	CanonicalScreenshot         workflowkit.ArtifactBinding `json:"canonical_screenshot"`
	TrialSetFingerprint         workflowkit.Fingerprint     `json:"trial_set_fingerprint"`
	Receipt                     EvaluationReceipt           `json:"receipt"`
	ReceiptFingerprint          workflowkit.Fingerprint     `json:"receipt_fingerprint"`
}

func (source EvaluatorEvidenceSource) Clone() EvaluatorEvidenceSource {
	return source
}

func (source EvaluatorEvidenceSource) validate(stageKey, bundleName, screenshotName string, binding FrozenRunBinding) error {
	if err := validateFinalComplianceText("child stage attempt id", source.ChildStageAttemptID); err != nil {
		return err
	}
	if err := source.ArtifactManifestFingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: child artifact manifest fingerprint: %v", ErrInvalidFinalCompliance, err)
	}
	if err := source.RunBundle.Validate(); err != nil || source.RunBundle.Name != bundleName {
		return fmt.Errorf("%w: %s run bundle binding", ErrInvalidFinalCompliance, stageKey)
	}
	if err := source.CanonicalScreenshot.Validate(); err != nil || source.CanonicalScreenshot.Name != screenshotName {
		return fmt.Errorf("%w: %s screenshot binding", ErrInvalidFinalCompliance, stageKey)
	}
	if err := source.TrialSetFingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: %s trial-set fingerprint: %v", ErrInvalidFinalCompliance, stageKey, err)
	}
	if err := source.Receipt.Validate(); err != nil {
		return fmt.Errorf("%w: %s receipt: %v", ErrInvalidFinalCompliance, stageKey, err)
	}
	if source.Receipt.RunBundleArtifactID != source.RunBundle.ArtifactID ||
		source.Receipt.RunBundleContentDigest != source.RunBundle.ContentDigest ||
		source.Receipt.ScreenshotArtifactID != source.CanonicalScreenshot.ArtifactID ||
		source.Receipt.ScreenshotContentDigest != source.CanonicalScreenshot.ContentDigest ||
		source.Receipt.ScreenshotMediaType != "image/png" {
		return fmt.Errorf("%w: %s receipt does not match child artifact bindings", ErrInvalidFinalCompliance, stageKey)
	}
	receiptBinding := FrozenRunBinding{
		TaskSnapshotDigest:  source.Receipt.TaskSnapshotDigest,
		CatalogFingerprint:  source.Receipt.CatalogFingerprint,
		LockFingerprint:     source.Receipt.LockFingerprint,
		ManifestFingerprint: source.Receipt.ManifestFingerprint,
	}
	if !sameFrozenRunBinding(receiptBinding, binding) {
		return fmt.Errorf("%w: %s receipt does not match child frozen binding", ErrInvalidFinalCompliance, stageKey)
	}
	fingerprint, err := source.Receipt.Fingerprint()
	if err != nil {
		return err
	}
	if source.ReceiptFingerprint != fingerprint {
		return fmt.Errorf("%w: %s receipt fingerprint does not match canonical receipt", ErrInvalidFinalCompliance, stageKey)
	}
	return nil
}

// EvaluatorEvidenceHandoff binds one Phase-1 parent Run to exactly one
// completed evaluator child Run. Parent and child deployment contracts are
// both visible and intentionally distinct: a child result is never presented
// as a parent stage output.
type EvaluatorEvidenceHandoff struct {
	Format                      string                  `json:"format"`
	Version                     string                  `json:"version"`
	ParentRunID                 string                  `json:"parent_run_id"`
	ParentDefinitionFingerprint workflowkit.Fingerprint `json:"parent_definition_fingerprint"`
	ParentBinding               FrozenRunBinding        `json:"parent_binding"`
	ChildRunID                  string                  `json:"child_run_id"`
	ChildTemplateID             string                  `json:"child_template_id"`
	ChildTemplateVersion        string                  `json:"child_template_version"`
	ChildDefinitionFingerprint  workflowkit.Fingerprint `json:"child_definition_fingerprint"`
	ChildBinding                FrozenRunBinding        `json:"child_binding"`
	Qwen                        EvaluatorEvidenceSource `json:"qwen"`
	Opus                        EvaluatorEvidenceSource `json:"opus"`
}

func (handoff EvaluatorEvidenceHandoff) Clone() EvaluatorEvidenceHandoff {
	handoff.Qwen = handoff.Qwen.Clone()
	handoff.Opus = handoff.Opus.Clone()
	return handoff
}

// Validate verifies provenance identities and internal receipt bindings. It
// does not read artifact bytes; the application handoff service is responsible
// for re-reading and rebuilding both receipts before persistence.
func (handoff EvaluatorEvidenceHandoff) Validate() error {
	if handoff.Format != EvaluatorEvidenceHandoffFormat || handoff.Version != EvaluatorEvidenceHandoffVersion {
		return fmt.Errorf("%w: unsupported evaluator evidence handoff format/version %q/%q", ErrInvalidFinalCompliance, handoff.Format, handoff.Version)
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"parent run id", handoff.ParentRunID},
		{"child run id", handoff.ChildRunID},
		{"child template id", handoff.ChildTemplateID},
		{"child template version", handoff.ChildTemplateVersion},
	} {
		if err := validateFinalComplianceText(field.name, field.value); err != nil {
			return err
		}
	}
	if handoff.ParentRunID == handoff.ChildRunID {
		return fmt.Errorf("%w: evaluator evidence handoff parent and child Run IDs must differ", ErrInvalidFinalCompliance)
	}
	for _, field := range []struct {
		name  string
		value workflowkit.Fingerprint
	}{
		{"parent definition fingerprint", handoff.ParentDefinitionFingerprint},
		{"child definition fingerprint", handoff.ChildDefinitionFingerprint},
	} {
		if err := field.value.Validate(); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInvalidFinalCompliance, field.name, err)
		}
	}
	if err := handoff.ParentBinding.Validate(); err != nil {
		return err
	}
	if err := handoff.ChildBinding.Validate(); err != nil {
		return err
	}
	if handoff.ParentBinding.TaskSnapshotDigest != handoff.ChildBinding.TaskSnapshotDigest {
		return fmt.Errorf("%w: evaluator evidence handoff child task digest differs from parent", ErrInvalidFinalCompliance)
	}
	if handoff.ParentBinding.ManifestFingerprint == handoff.ChildBinding.ManifestFingerprint {
		return fmt.Errorf("%w: evaluator evidence handoff cannot claim parent and child have one manifest", ErrInvalidFinalCompliance)
	}
	if err := handoff.Qwen.validate("Qwen", "qwen_trial_result", "qwen_pass4_evidence", handoff.ChildBinding); err != nil {
		return err
	}
	if err := handoff.Opus.validate("Opus", "opus_trial_result", "opus_pass4_evidence", handoff.ChildBinding); err != nil {
		return err
	}
	if handoff.Qwen.ChildStageAttemptID == handoff.Opus.ChildStageAttemptID ||
		handoff.Qwen.RunBundle.ArtifactID == handoff.Opus.RunBundle.ArtifactID ||
		handoff.Qwen.CanonicalScreenshot.ArtifactID == handoff.Opus.CanonicalScreenshot.ArtifactID {
		return fmt.Errorf("%w: Qwen and Opus handoff sources must remain distinct", ErrInvalidFinalCompliance)
	}
	return nil
}

func (handoff EvaluatorEvidenceHandoff) CanonicalJSON() ([]byte, error) {
	if err := handoff.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(handoff.Clone())
	if err != nil {
		return nil, fmt.Errorf("%w: encode evaluator evidence handoff: %v", ErrInvalidFinalCompliance, err)
	}
	return encoded, nil
}

func (handoff EvaluatorEvidenceHandoff) Fingerprint() (workflowkit.Fingerprint, error) {
	canonical, err := handoff.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return workflowkit.FingerprintBytes(evaluatorEvidenceHandoffFingerprintDomain, canonical)
}
