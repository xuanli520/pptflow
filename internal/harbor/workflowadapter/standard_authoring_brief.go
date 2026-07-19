package workflowadapter

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// StandardAuthoringBriefFormat identifies the immutable task-direction
	// contract frozen before a Standard authoring source is captured.
	StandardAuthoringBriefFormat  = "harbor.standard-authoring-brief.v1"
	StandardAuthoringBriefVersion = "1"

	// StandardAuthoringBriefArtifact is the intrinsic AuthoringSession input
	// consumed by stages whose output must remain within the caller's frozen
	// task type, application, and objective.
	StandardAuthoringBriefArtifact      = "authoring_brief"
	StandardAuthoringBriefSchemaVersion = StandardAuthoringBriefFormat

	StandardAuthoringBriefObjectiveMaxBytes = 512
)

// StandardAuthoringBrief is the complete caller-selected semantic target for
// one Standard authoring launch. It contains no repository, model, runtime,
// prompt, or mutable deployment configuration.
type StandardAuthoringBrief struct {
	Format      string `json:"format"`
	Version     string `json:"version"`
	TaskType    string `json:"task_type"`
	Application string `json:"application"`
	Objective   string `json:"objective"`
}

// NewStandardAuthoringBrief trims the three caller fields and returns their
// validated canonical representation.
func NewStandardAuthoringBrief(taskType, application, objective string) (StandardAuthoringBrief, error) {
	return (StandardAuthoringBrief{
		Format: StandardAuthoringBriefFormat, Version: StandardAuthoringBriefVersion,
		TaskType: taskType, Application: application, Objective: objective,
	}).Canonical()
}

// Canonical returns a value with all semantic fields trimmed. Token and
// objective validation is applied after trimming, while objective control
// characters are rejected before trimming so a newline cannot be hidden at a
// document boundary.
func (brief StandardAuthoringBrief) Canonical() (StandardAuthoringBrief, error) {
	if brief.Format != StandardAuthoringBriefFormat {
		return StandardAuthoringBrief{}, fmt.Errorf("%w: unsupported Standard authoring brief format %q", errInvalidCatalog, brief.Format)
	}
	if brief.Version != StandardAuthoringBriefVersion {
		return StandardAuthoringBrief{}, fmt.Errorf("%w: unsupported Standard authoring brief version %q", errInvalidCatalog, brief.Version)
	}
	for _, character := range brief.Objective {
		if unicode.IsControl(character) {
			return StandardAuthoringBrief{}, fmt.Errorf("%w: Standard authoring brief objective contains a control character", errInvalidCatalog)
		}
	}
	canonical := brief
	canonical.TaskType = strings.TrimSpace(brief.TaskType)
	canonical.Application = strings.TrimSpace(brief.Application)
	canonical.Objective = strings.TrimSpace(brief.Objective)
	if !standardAuthoringBriefToken(canonical.TaskType) {
		return StandardAuthoringBrief{}, fmt.Errorf("%w: Standard authoring brief task_type must match [a-z][a-z0-9-]{0,63}", errInvalidCatalog)
	}
	if !standardAuthoringBriefToken(canonical.Application) {
		return StandardAuthoringBrief{}, fmt.Errorf("%w: Standard authoring brief application must match [a-z][a-z0-9-]{0,63}", errInvalidCatalog)
	}
	if !utf8.ValidString(canonical.Objective) || len(canonical.Objective) == 0 || len(canonical.Objective) > StandardAuthoringBriefObjectiveMaxBytes {
		return StandardAuthoringBrief{}, fmt.Errorf("%w: Standard authoring brief objective must contain 1..%d UTF-8 bytes", errInvalidCatalog, StandardAuthoringBriefObjectiveMaxBytes)
	}
	if strings.ContainsAny(canonical.Objective, "\r\n") {
		return StandardAuthoringBrief{}, fmt.Errorf("%w: Standard authoring brief objective must be a single line", errInvalidCatalog)
	}
	return canonical, nil
}

// Validate requires the in-memory value itself to be canonical. Callers that
// accept user input should use NewStandardAuthoringBrief or Canonical first.
func (brief StandardAuthoringBrief) Validate() error {
	canonical, err := brief.Canonical()
	if err != nil {
		return err
	}
	if canonical != brief {
		return fmt.Errorf("%w: Standard authoring brief fields are not canonical", errInvalidCatalog)
	}
	return nil
}

// CanonicalJSON returns the stable bytes bound into the session manifest and
// exposed through the authoring_brief artifact input.
func (brief StandardAuthoringBrief) CanonicalJSON() ([]byte, error) {
	canonical, err := brief.Canonical()
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("%w: encode Standard authoring brief: %v", errInvalidCatalog, err)
	}
	return encoded, nil
}

// ContentDigest is the immutable object identity used by execution-spec and
// stage input bindings.
func (brief StandardAuthoringBrief) ContentDigest() (workflowkit.Fingerprint, error) {
	canonical, err := brief.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return workflowkit.SHA256Fingerprint(canonical), nil
}

// Fingerprint is equivalent to ContentDigest because the canonical brief bytes
// themselves are the frozen artifact content.
func (brief StandardAuthoringBrief) Fingerprint() (workflowkit.Fingerprint, error) {
	return brief.ContentDigest()
}

// ParseStandardAuthoringBriefJSON strictly decodes one brief document and
// returns its trimmed canonical value.
func ParseStandardAuthoringBriefJSON(raw []byte) (StandardAuthoringBrief, error) {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return StandardAuthoringBrief{}, fmt.Errorf("decode Standard authoring brief: %w", err)
	}
	var document standardAuthoringBriefDocument
	if err := decodeExecutionSpecJSON(raw, &document); err != nil {
		return StandardAuthoringBrief{}, fmt.Errorf("decode Standard authoring brief: %w", err)
	}
	return StandardAuthoringBrief(document).Canonical()
}

type standardAuthoringBriefDocument StandardAuthoringBrief

// UnmarshalJSON preserves the same strict boundary as the named parser.
func (brief *StandardAuthoringBrief) UnmarshalJSON(raw []byte) error {
	if brief == nil {
		return fmt.Errorf("%w: nil Standard authoring brief", errInvalidCatalog)
	}
	parsed, err := ParseStandardAuthoringBriefJSON(raw)
	if err != nil {
		return err
	}
	*brief = parsed
	return nil
}

func standardAuthoringBriefToken(value string) bool {
	if len(value) == 0 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}
