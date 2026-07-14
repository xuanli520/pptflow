package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	taskHubRunManifestFormat       = "harbor.workflow-run-manifest.v2"
	taskHubRunManifestInputsFormat = "harbor.run-manifest-inputs.v1"
)

// taskHubRunManifestProjection is deliberately a narrow, read-only view of
// the persisted app manifest. It accepts future fields without carrying their
// raw JSON into TaskHubDetail, while every field rendered by the TUI is
// checked against the durable Run identity before it is exposed.
type taskHubRunManifestProjection struct {
	Format                        string                                                `json:"format"`
	RunID                         string                                                `json:"run_id"`
	TaskID                        string                                                `json:"task_id"`
	RevisionID                    string                                                `json:"revision_id"`
	Resolved                      taskHubResolvedWorkflowProjection                     `json:"resolved_workflow"`
	InitialExecutionPlan          taskHubExecutionPlanProjection                        `json:"initial_execution_plan"`
	Inputs                        *taskHubRunManifestInputsProjection                   `json:"inputs,omitempty"`
	DeploymentCatalog             json.RawMessage                                       `json:"deployment_catalog_receipt,omitempty"`
	DeploymentCatalogLockIdentity *stageprovider.DeploymentOperationCatalogLockIdentity `json:"deployment_catalog_lock_identity,omitempty"`
}

type taskHubResolvedWorkflowProjection struct {
	Template struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	} `json:"template"`
	TemplateID                  string        `json:"template_id"`
	TemplateVersion             string        `json:"template_version"`
	ExecutionProfileID          string        `json:"execution_profile_id"`
	ExecutionProfileVersion     string        `json:"execution_profile_version"`
	ContinuationPlanTTL         time.Duration `json:"continuation_plan_ttl"`
	ControlGracePeriod          time.Duration `json:"control_grace_period"`
	TemplateFingerprint         string        `json:"template_fingerprint"`
	ExecutionProfileFingerprint string        `json:"execution_profile_fingerprint"`
	DefinitionFingerprint       string        `json:"definition_fingerprint"`
	ManifestFingerprint         string        `json:"manifest_fingerprint"`
}

type taskHubExecutionPlanProjection struct {
	Fingerprint string `json:"fingerprint"`
}

type taskHubRunManifestInputsProjection struct {
	Format                   string `json:"format"`
	BundleID                 string `json:"bundle_id,omitempty"`
	ProfileFingerprint       string `json:"profile_fingerprint"`
	ExecutionSpecFingerprint string `json:"execution_spec_fingerprint"`
}

// taskHubFrozenExecutionFromRun constructs the only manifest projection that
// crosses the app-adapter/TUI boundary. A malformed, duplicated, or
// identity-drifting document is represented as invalid rather than partially
// rendered. This is a presentation safety check, not a replacement for the
// worker's catalog-lock and runtime-attestation verification.
func taskHubFrozenExecutionFromRun(run store.WorkflowRun) TaskHubFrozenExecutionFact {
	fact := TaskHubFrozenExecutionFact{
		RunID: run.ID,
		State: TaskHubFrozenExecutionUnavailable,
		DeploymentCatalog: TaskHubDeploymentCatalogFact{
			State: TaskHubDeploymentCatalogNotRecorded,
		},
	}
	raw := []byte(strings.TrimSpace(run.RunManifestJSON))
	if len(raw) == 0 {
		return fact
	}
	manifest, err := parseTaskHubRunManifest(raw)
	if err != nil || !taskHubManifestBindsRun(manifest, run) {
		fact.State = TaskHubFrozenExecutionInvalid
		return fact
	}
	fact.State = TaskHubFrozenExecutionBound
	fact.TemplateID = manifest.Resolved.TemplateID
	fact.TemplateVersion = manifest.Resolved.TemplateVersion
	fact.ExecutionProfileID = manifest.Resolved.ExecutionProfileID
	fact.ExecutionProfileVersion = manifest.Resolved.ExecutionProfileVersion
	fact.ContinuationPlanTTL = manifest.Resolved.ContinuationPlanTTL
	fact.ControlGracePeriod = manifest.Resolved.ControlGracePeriod
	fact.TemplateFingerprint = manifest.Resolved.TemplateFingerprint
	fact.ProfileFingerprint = manifest.Resolved.ExecutionProfileFingerprint
	fact.DefinitionFingerprint = manifest.Resolved.DefinitionFingerprint
	fact.ResolvedManifestFingerprint = manifest.Resolved.ManifestFingerprint
	fact.InitialPlanFingerprint = manifest.InitialExecutionPlan.Fingerprint
	fact.InputBundleID = manifest.Inputs.BundleID
	fact.ExecutionSpecFingerprint = manifest.Inputs.ExecutionSpecFingerprint
	fact.DeploymentCatalog = taskHubCatalogFactFromManifest(manifest)
	return fact
}

func parseTaskHubRunManifest(raw []byte) (taskHubRunManifestProjection, error) {
	if err := rejectDuplicateTaskHubManifestJSONKeys(raw); err != nil {
		return taskHubRunManifestProjection{}, err
	}
	var manifest taskHubRunManifestProjection
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&manifest); err != nil {
		return taskHubRunManifestProjection{}, fmt.Errorf("decode frozen Run manifest: %w", err)
	}
	if err := ensureTaskHubJSONEOF(decoder); err != nil {
		return taskHubRunManifestProjection{}, err
	}
	return manifest, nil
}

func taskHubManifestBindsRun(manifest taskHubRunManifestProjection, run store.WorkflowRun) bool {
	if manifest.Format != taskHubRunManifestFormat || manifest.RunID != run.ID || manifest.TaskID != run.TaskID || manifest.RevisionID != run.RevisionID || manifest.Inputs == nil || manifest.Inputs.Format != taskHubRunManifestInputsFormat {
		return false
	}
	resolved := manifest.Resolved
	if resolved.Template.ID != resolved.TemplateID || resolved.Template.Version != resolved.TemplateVersion || resolved.TemplateID != run.WorkflowTemplateID || resolved.TemplateVersion != run.WorkflowTemplateVersion ||
		resolved.ExecutionProfileFingerprint != run.ResolvedProfileHash || resolved.DefinitionFingerprint != run.DefinitionHash ||
		strings.TrimSpace(resolved.ExecutionProfileID) == "" || strings.TrimSpace(resolved.ExecutionProfileVersion) == "" ||
		strings.TrimSpace(manifest.Inputs.ExecutionSpecFingerprint) == "" || manifest.Inputs.ProfileFingerprint != resolved.ExecutionProfileFingerprint ||
		resolved.ContinuationPlanTTL != workflowadapter.RequiredContinuationPlanTTL || resolved.ControlGracePeriod < 0 ||
		!taskHubSafeManifestText(resolved.TemplateID) || !taskHubSafeManifestText(resolved.TemplateVersion) ||
		!taskHubSafeManifestText(resolved.ExecutionProfileID) || !taskHubSafeManifestText(resolved.ExecutionProfileVersion) {
		return false
	}
	if bundleID := strings.TrimSpace(manifest.Inputs.BundleID); bundleID != "" {
		if bundleID != manifest.Inputs.BundleID || store.ValidateUUIDv7(bundleID) != nil {
			return false
		}
	}
	for _, fingerprint := range []string{
		resolved.TemplateFingerprint,
		resolved.ExecutionProfileFingerprint,
		resolved.DefinitionFingerprint,
		resolved.ManifestFingerprint,
		manifest.InitialExecutionPlan.Fingerprint,
		manifest.Inputs.ProfileFingerprint,
		manifest.Inputs.ExecutionSpecFingerprint,
	} {
		if err := workflowkit.Fingerprint(fingerprint).Validate(); err != nil {
			return false
		}
	}
	return true
}

func taskHubSafeManifestText(value string) bool {
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func taskHubCatalogFactFromManifest(manifest taskHubRunManifestProjection) TaskHubDeploymentCatalogFact {
	lock := taskHubCatalogLockFactFromManifest(manifest)
	raw := bytes.TrimSpace(manifest.DeploymentCatalog)
	if len(raw) == 0 {
		return TaskHubDeploymentCatalogFact{State: TaskHubDeploymentCatalogNotRecorded, LockState: lock.State}
	}
	receipt, err := stageprovider.ParseDeploymentOperationCatalogReceiptJSON(raw)
	if err != nil || receipt.Template.ID != manifest.Resolved.TemplateID || receipt.Template.Version != manifest.Resolved.TemplateVersion {
		return TaskHubDeploymentCatalogFact{State: TaskHubDeploymentCatalogInvalid, LockState: lock.State}
	}
	return TaskHubDeploymentCatalogFact{
		State:              TaskHubDeploymentCatalogBound,
		CatalogID:          receipt.CatalogID,
		CatalogVersion:     receipt.CatalogVersion,
		TemplateID:         receipt.Template.ID,
		TemplateVersion:    receipt.Template.Version,
		CatalogFingerprint: string(receipt.CatalogFingerprint),
		LockState:          lock.State,
		LockID:             lock.ID,
		LockVersion:        lock.Version,
		LockFingerprint:    lock.Fingerprint,
	}
}

type taskHubCatalogLockFact struct {
	State       TaskHubDeploymentCatalogLockState
	ID          string
	Version     string
	Fingerprint string
}

func taskHubCatalogLockFactFromManifest(manifest taskHubRunManifestProjection) taskHubCatalogLockFact {
	identity := manifest.DeploymentCatalogLockIdentity
	if identity == nil {
		return taskHubCatalogLockFact{State: TaskHubDeploymentCatalogLockNotRecorded}
	}
	if err := identity.Validate(); err != nil {
		return taskHubCatalogLockFact{State: TaskHubDeploymentCatalogLockInvalid}
	}
	return taskHubCatalogLockFact{
		State:       TaskHubDeploymentCatalogLockBound,
		ID:          identity.LockID,
		Version:     identity.LockVersion,
		Fingerprint: string(identity.Fingerprint),
	}
}

// rejectDuplicateTaskHubManifestJSONKeys preserves one important part of the
// app's strict manifest contract in this UI projection: a duplicate key must
// never choose a display value based on decoder order. Unknown future fields
// are otherwise tolerated because they are not rendered by this version of
// the Task Hub.
func rejectDuplicateTaskHubManifestJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := scanTaskHubManifestJSONValue(decoder); err != nil {
		return err
	}
	return ensureTaskHubJSONEOF(decoder)
}

func ensureTaskHubJSONEOF(decoder *json.Decoder) error {
	if decoder == nil {
		return fmt.Errorf("nil JSON decoder")
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return fmt.Errorf("read trailing JSON: %w", err)
	}
	return nil
}

func scanTaskHubManifestJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, okay := keyToken.(string)
			if !okay {
				return fmt.Errorf("object key is not a string")
			}
			if _, duplicate := keys[key]; duplicate {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			keys[key] = struct{}{}
			if err := scanTaskHubManifestJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("unterminated JSON object")
		}
		return nil
	case '[':
		for decoder.More() {
			if err := scanTaskHubManifestJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("unterminated JSON array")
		}
		return nil
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}
