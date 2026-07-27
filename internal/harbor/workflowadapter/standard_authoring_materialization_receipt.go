package workflowadapter

import (
	"encoding/json"
	"fmt"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	StandardAuthoringMaterializationReceiptFormat  = "harbor.standard-authoring-materialization-receipt.v1"
	StandardAuthoringMaterializationReceiptVersion = "1"

	standardAuthoringMaterializationReceiptFingerprintDomain = "harbor.workflowadapter.standard-authoring-materialization-receipt.v1"
)

// StandardAuthoringMaterializationReceipt records the sealed first revision
// created by the terminal materialize_task stage. It is immutable audit
// evidence and deliberately has no child workflow or run-selection field.
type StandardAuthoringMaterializationReceipt struct {
	Format                string                    `json:"format"`
	Version               string                    `json:"version"`
	AuthoringSourceID     string                    `json:"authoring_source_id"`
	AuthoringSessionID    string                    `json:"authoring_session_id"`
	AuthoringRunID        string                    `json:"authoring_run_id"`
	AuthoringSourceDigest workflowkit.SubjectDigest `json:"authoring_source_digest"`
	TaskID                string                    `json:"task_id"`
	RevisionID            string                    `json:"revision_id"`
	RevisionDigest        workflowkit.SubjectDigest `json:"revision_digest"`
	TaskSnapshot          ArtifactReference         `json:"task_snapshot"`
	AdmissionReceipt      ArtifactReference         `json:"admission_receipt"`
}

func (receipt StandardAuthoringMaterializationReceipt) Validate() error {
	if receipt.Format != StandardAuthoringMaterializationReceiptFormat || receipt.Version != StandardAuthoringMaterializationReceiptVersion {
		return fmt.Errorf("%w: unsupported Standard authoring materialization receipt", errInvalidExecutionSpec)
	}
	for _, field := range []struct {
		label string
		value string
	}{
		{"authoring source ID", receipt.AuthoringSourceID},
		{"authoring session ID", receipt.AuthoringSessionID},
		{"authoring Run ID", receipt.AuthoringRunID},
		{"task ID", receipt.TaskID},
		{"revision ID", receipt.RevisionID},
	} {
		if err := validatePersistentUUIDv7(field.label, field.value); err != nil {
			return err
		}
	}
	if err := receipt.AuthoringSourceDigest.Validate(); err != nil {
		return fmt.Errorf("%w: authoring source digest: %v", errInvalidExecutionSpec, err)
	}
	if err := receipt.RevisionDigest.Validate(); err != nil {
		return fmt.Errorf("%w: generated task revision digest: %v", errInvalidExecutionSpec, err)
	}
	if err := receipt.TaskSnapshot.validate(); err != nil || receipt.TaskSnapshot.SchemaVersion != "harbor.artifact.v1" {
		return fmt.Errorf("%w: invalid task snapshot", errInvalidExecutionSpec)
	}
	if err := receipt.AdmissionReceipt.validate(); err != nil || receipt.AdmissionReceipt.SchemaVersion != standardAuthoringPackageAdmissionReportSchemaVersion {
		return fmt.Errorf("%w: invalid package admission receipt", errInvalidExecutionSpec)
	}
	return nil
}

func (receipt StandardAuthoringMaterializationReceipt) CanonicalJSON() ([]byte, error) {
	if err := receipt.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return nil, fmt.Errorf("%w: encode Standard authoring materialization receipt: %v", errInvalidExecutionSpec, err)
	}
	return encoded, nil
}

func (receipt StandardAuthoringMaterializationReceipt) Fingerprint() (workflowkit.Fingerprint, error) {
	canonical, err := receipt.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return workflowkit.FingerprintBytes(standardAuthoringMaterializationReceiptFingerprintDomain, canonical)
}

func ParseStandardAuthoringMaterializationReceiptJSON(raw []byte) (StandardAuthoringMaterializationReceipt, error) {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return StandardAuthoringMaterializationReceipt{}, fmt.Errorf("%w: decode Standard authoring materialization receipt: %v", errInvalidExecutionSpec, err)
	}
	var receipt StandardAuthoringMaterializationReceipt
	if err := decodeExecutionSpecJSON(raw, &receipt); err != nil {
		return StandardAuthoringMaterializationReceipt{}, fmt.Errorf("%w: decode Standard authoring materialization receipt: %v", errInvalidExecutionSpec, err)
	}
	if err := receipt.Validate(); err != nil {
		return StandardAuthoringMaterializationReceipt{}, err
	}
	return receipt, nil
}
