package stageprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// WorkflowkitRegistryOptions supplies the exact provider resolver used by the
// Harbor adapter for the public generic Engine.
type WorkflowkitRegistryOptions struct {
	Providers WorkflowkitProviderOperationResolver
	// Template is the exact closed Harbor template whose plugin descriptors
	// this registry may execute. It remains a convenient single-template form;
	// it cannot be combined with Templates or omitted in favour of Standard.
	Template workflowadapter.TemplateReference
	// Templates is an explicit closed set for a process which is deliberately
	// able to run more than one installed Harbor template. A worker still
	// dispatches only the exact template sealed in its opaque RunExecutionSpec;
	// this is not a "current" or Standard fallback.
	Templates []workflowadapter.TemplateReference
}

// NewWorkflowkitStageExecutorRegistry creates a public workflowkit plugin
// registry for the full Harbor catalog. Each plugin adapter parses only the
// canonical opaque RunExecutionSpec frozen with the execution and then
// resolves the exact provider/operation binding from that document.
func NewWorkflowkitStageExecutorRegistry(configurations ...WorkflowkitRegistryOptions) (*workflowkit.ControlledPluginRegistry[workflowkit.StageExecutor], error) {
	if len(configurations) > 1 {
		return nil, fmt.Errorf("at most one public-engine Harbor provider registry configuration is allowed")
	}
	if len(configurations) == 0 {
		return nil, fmt.Errorf("public-engine Harbor template configuration is required")
	}
	options := configurations[0]
	if options.Providers == nil {
		options.Providers = rejectingWorkflowkitProviderResolver{}
	}
	templates, err := resolveWorkflowkitRegistryTemplates(options)
	if err != nil {
		return nil, err
	}

	// A plugin ID/version can legitimately occur in more than one closed
	// template. workflowkit's registry remains keyed only by that frozen plugin
	// binding, so the adapter below dispatches a shared binding by the equally
	// frozen template inside the opaque execution spec.
	byBinding := make(map[workflowkit.PluginBinding]map[workflowadapter.TemplateReference]workflowkitSpecPluginExecutor)
	for _, template := range templates {
		catalog := template.Catalog
		if err := catalog.Validate(); err != nil {
			return nil, fmt.Errorf("validate Harbor stage catalog for template %s@%s: %w", template.ID, template.Version, err)
		}
		stagesByBinding := make(map[workflowkit.PluginBinding]map[workflowkit.StageKey]workflowadapter.StageDefinition)
		for _, definition := range catalog.Stages {
			binding := catalogPluginBinding(definition.Plugin)
			stages := stagesByBinding[binding]
			if stages == nil {
				stages = make(map[workflowkit.StageKey]workflowadapter.StageDefinition)
				stagesByBinding[binding] = stages
			}
			stages[definition.Key] = definition.Clone()
		}
		for binding, stages := range stagesByBinding {
			byTemplate := byBinding[binding]
			if byTemplate == nil {
				byTemplate = make(map[workflowadapter.TemplateReference]workflowkitSpecPluginExecutor)
				byBinding[binding] = byTemplate
			}
			reference := template.Reference()
			if _, duplicate := byTemplate[reference]; duplicate {
				return nil, fmt.Errorf("duplicate public-engine Harbor template %s@%s for plugin %s@%s", reference.ID, reference.Version, binding.ID, binding.Version)
			}
			byTemplate[reference] = workflowkitSpecPluginExecutor{binding: binding, template: reference, stages: stages, providers: options.Providers}
		}
	}

	bindings := make([]workflowkit.PluginBinding, 0, len(byBinding))
	for binding := range byBinding {
		bindings = append(bindings, binding)
	}
	sort.Slice(bindings, func(left, right int) bool {
		if bindings[left].ID != bindings[right].ID {
			return bindings[left].ID < bindings[right].ID
		}
		return bindings[left].Version < bindings[right].Version
	})
	registrations := make([]workflowkit.PluginRegistration[workflowkit.StageExecutor], 0, len(bindings))
	for _, binding := range bindings {
		registrations = append(registrations, workflowkit.PluginRegistration[workflowkit.StageExecutor]{
			Binding: binding, Implementation: workflowkitTemplatePluginExecutor{binding: binding, templates: byBinding[binding]},
		})
	}
	registry, err := workflowkit.NewControlledPluginRegistry(registrations)
	if err != nil {
		return nil, fmt.Errorf("create Harbor public-engine executor registry: %w", err)
	}
	return registry, nil
}

func resolveWorkflowkitRegistryTemplates(options WorkflowkitRegistryOptions) ([]workflowadapter.WorkflowTemplate, error) {
	hasSingle := options.Template.ID != "" || options.Template.Version != ""
	if hasSingle && len(options.Templates) != 0 {
		return nil, fmt.Errorf("public-engine Harbor registry cannot combine Template and Templates")
	}
	references := append([]workflowadapter.TemplateReference(nil), options.Templates...)
	if hasSingle {
		references = []workflowadapter.TemplateReference{options.Template}
	}
	if len(references) == 0 {
		return nil, fmt.Errorf("public-engine Harbor template configuration is required")
	}
	sort.Slice(references, func(left, right int) bool {
		if references[left].ID != references[right].ID {
			return references[left].ID < references[right].ID
		}
		return references[left].Version < references[right].Version
	})
	templates := make([]workflowadapter.WorkflowTemplate, 0, len(references))
	seen := make(map[workflowadapter.TemplateReference]struct{}, len(references))
	for _, reference := range references {
		if _, duplicate := seen[reference]; duplicate {
			return nil, fmt.Errorf("duplicate public-engine Harbor template %s@%s", reference.ID, reference.Version)
		}
		seen[reference] = struct{}{}
		template, err := workflowadapter.ResolveWorkflowTemplate(reference)
		if err != nil {
			return nil, fmt.Errorf("resolve public-engine Harbor template: %w", err)
		}
		templates = append(templates, template)
	}
	return templates, nil
}

func catalogPluginBinding(plugin workflowadapter.PluginDescriptor) workflowkit.PluginBinding {
	return workflowkit.PluginBinding{ID: plugin.ID, Version: plugin.Version}
}

// workflowkitSpecPluginExecutor remains domain-only: it has no app runtime,
// SQLite, CLI, TUI, filesystem, or mutable Run manifest dependency.
type workflowkitSpecPluginExecutor struct {
	binding   workflowkit.PluginBinding
	template  workflowadapter.TemplateReference
	stages    map[workflowkit.StageKey]workflowadapter.StageDefinition
	providers WorkflowkitProviderOperationResolver
}

func (executor workflowkitSpecPluginExecutor) ExecuteStage(ctx context.Context, request workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
	specification, err := workflowkitRequestExecutionSpec(request)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	return executor.executeStageForSpec(ctx, request, specification)
}

func (executor workflowkitSpecPluginExecutor) executeStageForSpec(ctx context.Context, request workflowkit.StageExecutionRequest, specification workflowadapter.RunExecutionSpec) (workflowkit.StageExecutionResult, error) {
	if request.Stage.Plugin != executor.binding {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: plugin executor %s@%s received %s@%s", ErrInvalidStageOperation, executor.binding.ID, executor.binding.Version, request.Stage.Plugin.ID, request.Stage.Plugin.Version)
	}
	definition, found := executor.stages[request.Stage.Key]
	if !found {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: plugin executor %s@%s has no catalog stage %q", ErrInvalidStageOperation, executor.binding.ID, executor.binding.Version, request.Stage.Key)
	}
	if err := validateWorkflowkitStageContract(definition, request.Stage); err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if executor.providers == nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: provider resolver is not configured", ErrFrozenExecutionSpec)
	}
	if !specification.Template.Equal(executor.template) {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: frozen execution specification template %s@%s does not match registry template %s@%s", ErrFrozenExecutionSpec, specification.Template.ID, specification.Template.Version, executor.template.ID, executor.template.Version)
	}
	resolution, err := specification.ResolveStageOperation(request.Stage.Key)
	if err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("resolve frozen operation for stage %q: %w", request.Stage.Key, err)
	}
	if resolution.Plugin != request.Stage.Plugin {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: frozen operation plugin %s@%s does not match stage %q plugin %s@%s", ErrFrozenExecutionSpec, resolution.Plugin.ID, resolution.Plugin.Version, request.Stage.Key, request.Stage.Plugin.ID, request.Stage.Plugin.Version)
	}
	operation, err := executor.providers.ResolveWorkflowkitStageOperation(resolution)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if definition.IsGate() {
		// Resolving the operation proves the durable review policy is installed;
		// only this adapter can project a nonterminal external decision wait.
		_ = operation
		return workflowkitReviewGateWait(request, resolution)
	}
	return operation.ExecuteStage(ctx, request)
}

// workflowkitTemplatePluginExecutor dispatches one exact plugin binding to
// the matching template-specific contract after reading the canonical frozen
// RunExecutionSpec. It permits a single worker composition to support an
// explicitly listed template set without weakening template selection.
type workflowkitTemplatePluginExecutor struct {
	binding   workflowkit.PluginBinding
	templates map[workflowadapter.TemplateReference]workflowkitSpecPluginExecutor
}

func (executor workflowkitTemplatePluginExecutor) ExecuteStage(ctx context.Context, request workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
	if request.Stage.Plugin != executor.binding {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: plugin executor %s@%s received %s@%s", ErrInvalidStageOperation, executor.binding.ID, executor.binding.Version, request.Stage.Plugin.ID, request.Stage.Plugin.Version)
	}
	specification, err := workflowkitRequestExecutionSpec(request)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	templateExecutor, found := executor.templates[specification.Template]
	if !found {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: frozen execution specification template %s@%s is not installed for plugin %s@%s", ErrFrozenExecutionSpec, specification.Template.ID, specification.Template.Version, executor.binding.ID, executor.binding.Version)
	}
	return templateExecutor.executeStageForSpec(ctx, request, specification)
}

func workflowkitRequestExecutionSpec(request workflowkit.StageExecutionRequest) (workflowadapter.RunExecutionSpec, error) {
	binding := request.Execution.Binding
	if err := binding.Validate(); err != nil {
		return workflowadapter.RunExecutionSpec{}, fmt.Errorf("%w: opaque binding: %v", ErrFrozenExecutionSpec, err)
	}
	if binding.Format != workflowadapter.RunExecutionSpecFormat || binding.Version != workflowadapter.RunExecutionSpecVersion {
		return workflowadapter.RunExecutionSpec{}, fmt.Errorf("%w: binding format/version %q/%q is not a Harbor execution spec", ErrFrozenExecutionSpec, binding.Format, binding.Version)
	}
	specification, err := workflowadapter.ParseRunExecutionSpecJSON(binding.Canonical)
	if err != nil {
		return workflowadapter.RunExecutionSpec{}, fmt.Errorf("%w: parse execution spec: %v", ErrFrozenExecutionSpec, err)
	}
	canonical, err := specification.CanonicalJSON()
	if err != nil || !bytes.Equal(canonical, binding.Canonical) {
		return workflowadapter.RunExecutionSpec{}, fmt.Errorf("%w: execution spec is not canonical", ErrFrozenExecutionSpec)
	}
	// The generic kernel sees only an opaque SubjectBinding.  A Harbor
	// RunExecutionSpec can project either a sealed TaskRevision or the
	// pre-materialization AuthoringSource/AuthoringSession pair onto that
	// binding.  Do not read the task-only fields here: doing so would force an
	// authoring workflow to fabricate a TaskRevision just to enter the common
	// workflowkit runtime.
	selectionBinding, err := specification.Selection.SubjectBinding()
	if err != nil {
		return workflowadapter.RunExecutionSpec{}, fmt.Errorf("%w: execution spec selection: %v", ErrFrozenExecutionSpec, err)
	}
	if selectionBinding != request.Execution.Subject {
		return workflowadapter.RunExecutionSpec{}, fmt.Errorf("%w: execution spec selection does not match frozen subject", ErrFrozenExecutionSpec)
	}
	return specification, nil
}

func validateWorkflowkitStageContract(definition workflowadapter.StageDefinition, stage workflowkit.StageDescriptor) error {
	if stage.Key != definition.Key || stage.Plugin != catalogPluginBinding(definition.Plugin) || stage.Version != definition.Version || stage.Effect != definition.Effect ||
		!sameArtifactSpecs(stage.Inputs, definition.Inputs) || !sameArtifactSpecs(stage.Outputs, definition.Outputs) ||
		!sameVerdictPolicy(stage.Verdicts, definition.Verdicts) || !sameCapabilities(stage.Capabilities, definition.Capabilities) {
		return fmt.Errorf("%w: stage %q does not match its frozen catalog contract", ErrInvalidStageOperation, stage.Key)
	}
	return nil
}

func workflowkitReviewGateWait(request workflowkit.StageExecutionRequest, resolution workflowadapter.StageOperationResolution) (workflowkit.StageExecutionResult, error) {
	if request.Claim.Stage == nil || strings.TrimSpace(string(request.Claim.Stage.StageAttempt.ID)) == "" {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: review gate %q has no frozen stage attempt", ErrInvalidStageOperation, request.Stage.Key)
	}
	canonical, err := json.Marshal(struct {
		ExecutionID string                                `json:"execution_id"`
		Subject     workflowkit.SubjectBinding            `json:"subject"`
		StageKey    workflowkit.StageKey                  `json:"stage_key"`
		StageType   workflowadapter.StageBindingType      `json:"stage_type"`
		Plugin      workflowkit.PluginBinding             `json:"plugin"`
		Provider    workflowadapter.ProviderReference     `json:"provider"`
		Operation   workflowadapter.StageOperationBinding `json:"operation"`
	}{
		ExecutionID: request.Execution.ID,
		Subject:     request.Execution.Subject,
		StageKey:    resolution.StageKey,
		StageType:   resolution.StageType,
		Plugin:      resolution.Plugin,
		Provider:    resolution.Provider,
		Operation:   resolution.Operation,
	})
	if err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: encode review wait binding: %v", ErrInvalidStageOperation, err)
	}
	binding, err := workflowkit.NewOpaqueExecutionBinding("harbor.review-gate-wait", "1", canonical)
	if err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: freeze review wait binding: %v", ErrInvalidStageOperation, err)
	}
	return workflowkit.StageExecutionResult{Wait: &workflowkit.StageWait{
		Kind:            workflowkit.StageWaitExternalDecision,
		OperationKey:    "review-gate:" + request.Execution.ID + ":" + string(request.Stage.Key) + ":" + string(request.Claim.Stage.StageAttempt.ID),
		DecisionBinding: binding,
	}}, nil
}

type rejectingWorkflowkitProviderResolver struct{}

func (rejectingWorkflowkitProviderResolver) ResolveWorkflowkitStageOperation(resolution workflowadapter.StageOperationResolution) (workflowkit.StageExecutor, error) {
	return nil, fmt.Errorf("%w: provider %q operation %q@%q for stage %q", ErrProviderUnavailable, resolution.Provider.ID, resolution.Operation.OperationID, resolution.Operation.Version, resolution.StageKey)
}

func (resolver rejectingWorkflowkitProviderResolver) ValidateStageOperation(resolution workflowadapter.StageOperationResolution) error {
	_, err := resolver.ResolveWorkflowkitStageOperation(resolution)
	return err
}

func sameArtifactSpecs(left, right []workflowkit.ArtifactSpec) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]workflowkit.ArtifactSpec(nil), left...)
	right = append([]workflowkit.ArtifactSpec(nil), right...)
	sort.Slice(left, func(first, second int) bool { return left[first].Name < left[second].Name })
	sort.Slice(right, func(first, second int) bool { return right[first].Name < right[second].Name })
	return reflect.DeepEqual(left, right)
}

func sameVerdictPolicy(left, right workflowkit.VerdictPolicy) bool {
	left = left.Clone()
	right = right.Clone()
	sort.Slice(left.Allowed, func(first, second int) bool { return left.Allowed[first] < left.Allowed[second] })
	sort.Slice(right.Allowed, func(first, second int) bool { return right.Allowed[first] < right.Allowed[second] })
	return reflect.DeepEqual(left, right)
}

func sameCapabilities(left, right workflowkit.CapabilitySet) bool {
	left = left.Clone()
	right = right.Clone()
	sort.Slice(left, func(first, second int) bool { return left[first] < left[second] })
	sort.Slice(right, func(first, second int) bool { return right[first] < right[second] })
	return reflect.DeepEqual(left, right)
}

var _ workflowkit.StageExecutor = workflowkitSpecPluginExecutor{}
var _ workflowkit.StageExecutor = workflowkitTemplatePluginExecutor{}
var _ WorkflowkitProviderOperationResolver = rejectingWorkflowkitProviderResolver{}
