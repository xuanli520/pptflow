package workflowadapter

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// TemplateReference identifies one closed, code-versioned Harbor workflow
// template. It is deliberately a value, rather than an open configuration
// name: every profile and execution specification must freeze both fields.
//
// A reference only becomes executable when it resolves through
// DefaultTemplateRegistry. There is no "current", "standard", or versionless
// fallback.
type TemplateReference struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// StandardTemplateReference returns the exact identity of Harbor's complete
// V2 lifecycle template. Callers receive a value copy and therefore cannot
// mutate a process-wide template selector.
func StandardTemplateReference() TemplateReference {
	return TemplateReference{ID: StandardWorkflowTemplateID, Version: StandardWorkflowTemplateVersion}
}

// Equal reports whether two frozen template identities are exactly equal.
func (reference TemplateReference) Equal(other TemplateReference) bool {
	return reference.ID == other.ID && reference.Version == other.Version
}

// Validate checks the local shape and membership of a closed template
// reference. A syntactically valid but unregistered template is intentionally
// rejected; accepting it would turn a caller-provided template name into an
// execution policy selection surface.
func (reference TemplateReference) Validate() error {
	if strings.TrimSpace(reference.ID) == "" {
		return fmt.Errorf("%w: workflow template reference id is required", errInvalidCatalog)
	}
	if strings.TrimSpace(reference.Version) == "" {
		return fmt.Errorf("%w: workflow template reference version is required", errInvalidCatalog)
	}
	if !isBuiltinTemplateReference(reference) {
		return fmt.Errorf("%w: workflow template %s@%s is not registered", errInvalidCatalog, reference.ID, reference.Version)
	}
	return nil
}

// TemplateResolver resolves only a closed set of complete workflow templates.
// It has no Register method on purpose: deployment code selects an installed
// template by its exact frozen reference, rather than accepting a mutable
// caller-supplied catalog.
type TemplateResolver interface {
	ResolveTemplate(TemplateReference) (WorkflowTemplate, error)
}

// TemplateRegistry is an immutable snapshot of Harbor's built-in workflow
// templates. Its map is private, and ResolveTemplate returns clones, so a
// caller cannot mutate registry state or use it as a dynamic template source.
type TemplateRegistry struct {
	templates map[templateReferenceKey]WorkflowTemplate
}

type templateReferenceKey struct {
	id      string
	version string
}

func templateKey(reference TemplateReference) templateReferenceKey {
	return templateReferenceKey{id: reference.ID, version: reference.Version}
}

// DefaultTemplateRegistry returns the complete closed template set compiled
// into this Harbor build. New templates require source changes and a versioned
// constructor; they cannot be registered through CLI, TUI, profile, or spec
// input.
func DefaultTemplateRegistry() TemplateRegistry {
	standard := StandardWorkflowTemplate()
	standardAuthoring := StandardAuthoringWorkflowTemplate()
	standardAuthoringAdmission := StandardAuthoringTaskAdmissionWorkflowTemplate()
	codeEdge := CodeEdgePhase1WorkflowTemplate()
	codeEdgeEvaluator := CodeEdgeEvaluatorChildWorkflowTemplate()
	return TemplateRegistry{templates: map[templateReferenceKey]WorkflowTemplate{
		templateKey(standard.Reference()):                   standard,
		templateKey(standardAuthoring.Reference()):          standardAuthoring,
		templateKey(standardAuthoringAdmission.Reference()): standardAuthoringAdmission,
		templateKey(codeEdge.Reference()):                   codeEdge,
		templateKey(codeEdgeEvaluator.Reference()):          codeEdgeEvaluator,
	}}
}

// ResolveTemplate returns an independent snapshot of the exact registered
// template. It never substitutes another version or a Standard template.
func (registry TemplateRegistry) ResolveTemplate(reference TemplateReference) (WorkflowTemplate, error) {
	if err := reference.Validate(); err != nil {
		return WorkflowTemplate{}, err
	}
	template, present := registry.templates[templateKey(reference)]
	if !present {
		return WorkflowTemplate{}, fmt.Errorf("%w: workflow template %s@%s is unavailable in this deployment", errInvalidCatalog, reference.ID, reference.Version)
	}
	return template.Clone(), nil
}

// ResolveWorkflowTemplate resolves a reference against the closed built-in
// registry. It is the safe default for profile/spec parsing and validation.
func ResolveWorkflowTemplate(reference TemplateReference) (WorkflowTemplate, error) {
	return DefaultTemplateRegistry().ResolveTemplate(reference)
}

// BuiltinTemplateReferences returns the executable built-in templates in
// canonical order. The returned slice and entries are copies.
func BuiltinTemplateReferences() []TemplateReference {
	references := []TemplateReference{
		StandardTemplateReference(),
		StandardAuthoringTemplateReference(),
		StandardAuthoringTaskAdmissionTemplateReference(),
		CodeEdgePhase1TemplateReference(),
		CodeEdgeEvaluatorChildTemplateReference(),
	}
	sort.Slice(references, func(left, right int) bool {
		if references[left].ID != references[right].ID {
			return references[left].ID < references[right].ID
		}
		return references[left].Version < references[right].Version
	})
	return references
}

func isBuiltinTemplateReference(reference TemplateReference) bool {
	return reference.Equal(StandardTemplateReference()) ||
		reference.Equal(StandardAuthoringTemplateReference()) ||
		reference.Equal(StandardAuthoringTaskAdmissionTemplateReference()) ||
		reference.Equal(CodeEdgePhase1TemplateReference()) ||
		reference.Equal(CodeEdgeEvaluatorChildTemplateReference())
}

// errTemplateMismatch lets callers distinguish a cross-template profile/spec
// from general catalog validation failures without weakening the closed
// resolver boundary.
var errTemplateMismatch = errors.New("harbor workflow adapter: template reference mismatch")
