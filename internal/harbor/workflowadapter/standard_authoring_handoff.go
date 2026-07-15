package workflowadapter

import (
	"encoding/json"
	"fmt"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// StandardAuthoringTaskHandoffFormat identifies the immutable receipt
	// emitted by the final source-session materialize_task operation.
	StandardAuthoringTaskHandoffFormat  = "harbor.standard-authoring-task-handoff.v1"
	StandardAuthoringTaskHandoffVersion = "1"

	standardAuthoringTaskHandoffFingerprintDomain = "harbor.workflowadapter.standard-authoring-task-handoff.v1"
)

// StandardAuthoringTaskHandoff is the only typed bridge from an
// AuthoringSession Run to a task-revision Run. It proves that materialization
// created a real immutable revision, names the task-snapshot artifact the
// child may consume, and fixes CodeEdge Phase-1 as the next workflow template.
//
// It deliberately contains no workspace path, source checkout path, prompt,
// provider configuration, secret, or unverified model/evaluation result.
type StandardAuthoringTaskHandoff struct {
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
	ChildTemplate         TemplateReference         `json:"child_template"`
}

// Validate proves this handoff cannot relabel an authoring session as a task
// revision or dispatch an arbitrary child template. It is intentionally
// usable by the application handoff service before it starts the child Run.
func (handoff StandardAuthoringTaskHandoff) Validate() error {
	if handoff.Format != StandardAuthoringTaskHandoffFormat {
		return fmt.Errorf("%w: unsupported Standard authoring handoff format %q", errInvalidExecutionSpec, handoff.Format)
	}
	if handoff.Version != StandardAuthoringTaskHandoffVersion {
		return fmt.Errorf("%w: unsupported Standard authoring handoff version %q", errInvalidExecutionSpec, handoff.Version)
	}
	for _, field := range []struct {
		label string
		value string
	}{
		{"authoring source id", handoff.AuthoringSourceID},
		{"authoring session id", handoff.AuthoringSessionID},
		{"authoring run id", handoff.AuthoringRunID},
		{"task id", handoff.TaskID},
		{"revision id", handoff.RevisionID},
	} {
		if err := validatePersistentUUIDv7(field.label, field.value); err != nil {
			return err
		}
	}
	if err := handoff.AuthoringSourceDigest.Validate(); err != nil {
		return fmt.Errorf("%w: authoring source digest: %v", errInvalidExecutionSpec, err)
	}
	if err := handoff.RevisionDigest.Validate(); err != nil {
		return fmt.Errorf("%w: generated task revision digest: %v", errInvalidExecutionSpec, err)
	}
	if err := handoff.TaskSnapshot.validate(); err != nil {
		return err
	}
	if handoff.TaskSnapshot.SchemaVersion != "harbor.artifact.v1" {
		return fmt.Errorf("%w: authoring handoff task snapshot schema %q must be harbor.artifact.v1", errInvalidExecutionSpec, handoff.TaskSnapshot.SchemaVersion)
	}
	if !handoff.ChildTemplate.Equal(CodeEdgePhase1TemplateReference()) {
		return fmt.Errorf("%w: Standard authoring handoff child template must be %s@%s", errInvalidExecutionSpec, CodeEdgePhase1WorkflowTemplateID, CodeEdgePhase1WorkflowTemplateVersion)
	}
	if err := handoff.ChildTemplate.Validate(); err != nil {
		return fmt.Errorf("%w: authoring handoff child template: %v", errInvalidExecutionSpec, err)
	}
	return nil
}

// ChildSelection returns the exact task-revision subject a handoff service
// must use when it atomically prepares the CodeEdge Phase-1 child Run.
func (handoff StandardAuthoringTaskHandoff) ChildSelection() (RunSelectionReference, error) {
	if err := handoff.Validate(); err != nil {
		return RunSelectionReference{}, err
	}
	return RunSelectionReference{
		Kind: RunSelectionTaskRevision, TaskID: handoff.TaskID, RevisionID: handoff.RevisionID, RevisionDigest: handoff.RevisionDigest,
	}, nil
}

// CanonicalJSON returns the strict immutable handoff document used as the
// materialize_task output content. It has no map fields, so json.Marshal's
// struct field order is the canonical versioned representation.
func (handoff StandardAuthoringTaskHandoff) CanonicalJSON() ([]byte, error) {
	if err := handoff.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(handoff)
	if err != nil {
		return nil, fmt.Errorf("%w: encode Standard authoring handoff: %v", errInvalidExecutionSpec, err)
	}
	return encoded, nil
}

// Fingerprint returns a domain-separated immutable handoff identity suitable
// for durable materialization and child-run idempotency records.
func (handoff StandardAuthoringTaskHandoff) Fingerprint() (workflowkit.Fingerprint, error) {
	canonical, err := handoff.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return workflowkit.FingerprintBytes(standardAuthoringTaskHandoffFingerprintDomain, canonical)
}

// ParseStandardAuthoringTaskHandoffJSON strictly decodes one handoff receipt.
// Unknown or duplicate fields and trailing JSON values are rejected before a
// child Run can use it as an authorization boundary.
func ParseStandardAuthoringTaskHandoffJSON(raw []byte) (StandardAuthoringTaskHandoff, error) {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return StandardAuthoringTaskHandoff{}, fmt.Errorf("%w: decode Standard authoring handoff: %v", errInvalidExecutionSpec, err)
	}
	var document standardAuthoringTaskHandoffDocument
	if err := decodeExecutionSpecJSON(raw, &document); err != nil {
		return StandardAuthoringTaskHandoff{}, fmt.Errorf("%w: decode Standard authoring handoff: %v", errInvalidExecutionSpec, err)
	}
	handoff := StandardAuthoringTaskHandoff(document)
	if err := handoff.Validate(); err != nil {
		return StandardAuthoringTaskHandoff{}, err
	}
	return handoff, nil
}

// The alias prevents ParseStandardAuthoringTaskHandoffJSON from recursively
// invoking StandardAuthoringTaskHandoff.UnmarshalJSON while retaining the
// exact same JSON field layout.
type standardAuthoringTaskHandoffDocument StandardAuthoringTaskHandoff

// UnmarshalJSON retains strict parsing for callers that use encoding/json
// directly instead of the named parser.
func (handoff *StandardAuthoringTaskHandoff) UnmarshalJSON(raw []byte) error {
	if handoff == nil {
		return fmt.Errorf("%w: nil Standard authoring handoff", errInvalidExecutionSpec)
	}
	parsed, err := ParseStandardAuthoringTaskHandoffJSON(raw)
	if err != nil {
		return err
	}
	*handoff = parsed
	return nil
}
