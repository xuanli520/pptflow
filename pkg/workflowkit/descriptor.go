package workflowkit

import (
	"encoding/json"
	"fmt"
	"sort"
)

// StageEffect describes the broad class of a stage's durable effects. Domain
// adapters provide their own resource vocabulary; the kernel only uses this
// classification for generic safety rules such as external-side-effect
// confirmation.
type StageEffect string

const (
	EffectReadOnly           StageEffect = "read_only"
	EffectEvidenceOnly       StageEffect = "evidence_only"
	EffectContentProducer    StageEffect = "content_producer"
	EffectContentMutator     StageEffect = "content_mutator"
	EffectExternalSideEffect StageEffect = "external_side_effect"
)

func (effect StageEffect) valid() bool {
	switch effect {
	case EffectReadOnly, EffectEvidenceOnly, EffectContentProducer, EffectContentMutator, EffectExternalSideEffect:
		return true
	default:
		return false
	}
}

// Capability describes an explicit operation a stage supports.
type Capability string

const (
	CapabilityCancel   Capability = "cancel"
	CapabilityContinue Capability = "continue"
	CapabilityApprove  Capability = "approve"
)

func (capability Capability) valid() bool {
	switch capability {
	case CapabilityCancel, CapabilityContinue, CapabilityApprove:
		return true
	default:
		return false
	}
}

// CapabilitySet is intentionally a slice so a compiled descriptor remains
// serializable and fingerprintable without a map's mutable surface.
type CapabilitySet []Capability

// Has reports whether the set contains capability.
func (set CapabilitySet) Has(capability Capability) bool {
	for _, current := range set {
		if current == capability {
			return true
		}
	}
	return false
}

// Clone returns an independent copy of the set.
func (set CapabilitySet) Clone() CapabilitySet {
	return append(CapabilitySet(nil), set...)
}

func (set CapabilitySet) validate() error {
	if err := validateUniqueStrings("capability", set, ErrInvalidDescriptor); err != nil {
		return err
	}
	for _, capability := range set {
		if !capability.valid() {
			return fmt.Errorf("%w: unsupported capability %q", ErrInvalidDescriptor, capability)
		}
	}
	return nil
}

// ReusePolicy declares whether an output may be preserved across a plan. A
// preserve decision still requires valid artifact lineage and matching inputs.
type ReusePolicy string

const (
	ReuseNever           ReusePolicy = "never"
	ReuseWhenInputsMatch ReusePolicy = "when_inputs_match"
)

func (policy ReusePolicy) valid() bool {
	return policy == ReuseNever || policy == ReuseWhenInputsMatch
}

// ArtifactSpec is a typed artifact contract. Names are local to one stage.
type ArtifactSpec struct {
	Name          string `json:"name"`
	SchemaVersion string `json:"schema_version"`
	Required      bool   `json:"required"`
}

func (spec ArtifactSpec) validate(direction string) error {
	if err := validateRequired(direction+" artifact name", spec.Name, ErrInvalidDescriptor); err != nil {
		return err
	}
	if err := validateRequired(direction+" artifact schema version", spec.SchemaVersion, ErrInvalidDescriptor); err != nil {
		return err
	}
	return nil
}

func validateArtifactSpecs(direction string, specs []ArtifactSpec) error {
	seen := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		if err := spec.validate(direction); err != nil {
			return err
		}
		if _, ok := seen[spec.Name]; ok {
			return fmt.Errorf("%w: duplicate %s artifact name %q", ErrInvalidDescriptor, direction, spec.Name)
		}
		seen[spec.Name] = struct{}{}
	}
	return nil
}

// RetryPolicy describes which classified infrastructure outcomes may be
// retried inside the budget's explicitly bounded MaxAttempts.
type RetryPolicy struct {
	Retryable []FailureClass `json:"retryable"`
}

func (policy RetryPolicy) Clone() RetryPolicy {
	policy.Retryable = append([]FailureClass(nil), policy.Retryable...)
	return policy
}

func (policy RetryPolicy) validate() error {
	if err := validateUniqueStrings("retryable failure class", policy.Retryable, ErrInvalidDescriptor); err != nil {
		return err
	}
	for _, class := range policy.Retryable {
		switch class {
		case FailureTransient, FailureTimeout, FailureRateLimited, FailureNetwork, FailureProcess:
			continue
		default:
			return fmt.Errorf("%w: failure class %q cannot be retried", ErrInvalidDescriptor, class)
		}
	}
	return nil
}

// VerdictPolicy identifies all completed verdicts that a stage may emit. The
// policy does not decide their domain meaning; it merely makes an unexpected
// verdict a compilation-time contract violation.
type VerdictPolicy struct {
	Allowed []Verdict `json:"allowed"`
}

func (policy VerdictPolicy) Clone() VerdictPolicy {
	policy.Allowed = append([]Verdict(nil), policy.Allowed...)
	return policy
}

func (policy VerdictPolicy) Allows(verdict Verdict) bool {
	for _, allowed := range policy.Allowed {
		if allowed == verdict {
			return true
		}
	}
	return false
}

func (policy VerdictPolicy) validate() error {
	if len(policy.Allowed) == 0 {
		return fmt.Errorf("%w: verdict policy must allow at least one verdict", ErrInvalidDescriptor)
	}
	if err := validateUniqueStrings("allowed verdict", policy.Allowed, ErrInvalidDescriptor); err != nil {
		return err
	}
	for _, verdict := range policy.Allowed {
		if !verdict.valid() || verdict == VerdictNone {
			return fmt.Errorf("%w: unsupported allowed verdict %q", ErrInvalidDescriptor, verdict)
		}
	}
	return nil
}

// StageDescriptor is the typed, frozen description of one executable DAG
// stage. It deliberately has no untyped configuration map: domain adapters
// decode their configuration before constructing this descriptor.
type StageDescriptor struct {
	Key          StageKey        `json:"key"`
	Version      string          `json:"version"`
	Plugin       PluginBinding   `json:"plugin"`
	Group        string          `json:"group"`
	Dependencies []StageKey      `json:"dependencies"`
	Inputs       []ArtifactSpec  `json:"inputs"`
	Outputs      []ArtifactSpec  `json:"outputs"`
	ReadSet      []ResourceKey   `json:"read_set"`
	WriteSet     []ResourceKey   `json:"write_set"`
	Effect       StageEffect     `json:"effect"`
	Budget       ExecutionBudget `json:"budget"`
	QuotaClaims  []QuotaClaim    `json:"quota_claims"`
	Retry        RetryPolicy     `json:"retry"`
	Verdicts     VerdictPolicy   `json:"verdicts"`
	Reuse        ReusePolicy     `json:"reuse"`
	Capabilities CapabilitySet   `json:"capabilities"`
}

// Clone returns a deep copy suitable for a caller to modify before freezing a
// descriptor into a workflow definition.
func (descriptor StageDescriptor) Clone() StageDescriptor {
	descriptor.Dependencies = append([]StageKey(nil), descriptor.Dependencies...)
	descriptor.Inputs = append([]ArtifactSpec(nil), descriptor.Inputs...)
	descriptor.Outputs = append([]ArtifactSpec(nil), descriptor.Outputs...)
	descriptor.ReadSet = append([]ResourceKey(nil), descriptor.ReadSet...)
	descriptor.WriteSet = append([]ResourceKey(nil), descriptor.WriteSet...)
	descriptor.Budget = descriptor.Budget.Clone()
	descriptor.QuotaClaims = append([]QuotaClaim(nil), descriptor.QuotaClaims...)
	descriptor.Retry = descriptor.Retry.Clone()
	descriptor.Verdicts = descriptor.Verdicts.Clone()
	descriptor.Capabilities = descriptor.Capabilities.Clone()
	return descriptor
}

// Validate checks all local stage invariants. Dependency existence and cycle
// checks belong to WorkflowDescriptor.Validate.
func (descriptor StageDescriptor) Validate() error {
	if err := validateRequired("stage key", string(descriptor.Key), ErrInvalidDescriptor); err != nil {
		return err
	}
	if err := validateRequired("stage version", descriptor.Version, ErrInvalidDescriptor); err != nil {
		return err
	}
	if err := descriptor.Plugin.Validate(); err != nil {
		return err
	}
	if err := validateRequired("stage group", descriptor.Group, ErrInvalidDescriptor); err != nil {
		return err
	}
	if err := validateUniqueStrings("stage dependency", descriptor.Dependencies, ErrInvalidDescriptor); err != nil {
		return err
	}
	for _, dependency := range descriptor.Dependencies {
		if dependency == descriptor.Key {
			return fmt.Errorf("%w: stage %q cannot depend on itself", ErrInvalidDescriptor, descriptor.Key)
		}
	}
	if err := validateArtifactSpecs("input", descriptor.Inputs); err != nil {
		return err
	}
	if err := validateArtifactSpecs("output", descriptor.Outputs); err != nil {
		return err
	}
	if err := validateUniqueStrings("read resource", descriptor.ReadSet, ErrInvalidDescriptor); err != nil {
		return err
	}
	if err := validateUniqueStrings("write resource", descriptor.WriteSet, ErrInvalidDescriptor); err != nil {
		return err
	}
	if !descriptor.Effect.valid() {
		return fmt.Errorf("%w: unsupported stage effect %q", ErrInvalidDescriptor, descriptor.Effect)
	}
	if descriptor.Effect == EffectReadOnly && len(descriptor.WriteSet) > 0 {
		return fmt.Errorf("%w: read-only stage %q cannot declare writes", ErrInvalidDescriptor, descriptor.Key)
	}
	if err := descriptor.Budget.Validate(); err != nil {
		return fmt.Errorf("%w: stage %q budget: %v", ErrInvalidDescriptor, descriptor.Key, err)
	}
	if _, err := NormalizeQuotaClaims(descriptor.QuotaClaims); err != nil {
		return fmt.Errorf("%w: stage %q quota claims: %v", ErrInvalidDescriptor, descriptor.Key, err)
	}
	if err := descriptor.Retry.validate(); err != nil {
		return fmt.Errorf("%w: stage %q retry policy: %v", ErrInvalidDescriptor, descriptor.Key, err)
	}
	if err := descriptor.Verdicts.validate(); err != nil {
		return fmt.Errorf("%w: stage %q verdict policy: %v", ErrInvalidDescriptor, descriptor.Key, err)
	}
	if !descriptor.Reuse.valid() {
		return fmt.Errorf("%w: unsupported reuse policy %q", ErrInvalidDescriptor, descriptor.Reuse)
	}
	if err := descriptor.Capabilities.validate(); err != nil {
		return fmt.Errorf("%w: stage %q capabilities: %v", ErrInvalidDescriptor, descriptor.Key, err)
	}
	return nil
}

// WorkflowDescriptor is a versioned DAG of typed stage descriptors.
type WorkflowDescriptor struct {
	ID      string            `json:"id"`
	Version string            `json:"version"`
	Stages  []StageDescriptor `json:"stages"`
}

// Clone returns a deep copy of the workflow definition.
func (workflow WorkflowDescriptor) Clone() WorkflowDescriptor {
	stages := workflow.Stages
	workflow.Stages = make([]StageDescriptor, len(stages))
	for index, descriptor := range stages {
		workflow.Stages[index] = descriptor.Clone()
	}
	return workflow
}

// Validate proves that the workflow is a well-formed acyclic DAG of valid
// typed stage descriptors.
func (workflow WorkflowDescriptor) Validate() error {
	if err := validateRequired("workflow id", workflow.ID, ErrInvalidDescriptor); err != nil {
		return err
	}
	if err := validateRequired("workflow version", workflow.Version, ErrInvalidDescriptor); err != nil {
		return err
	}
	if len(workflow.Stages) == 0 {
		return fmt.Errorf("%w: workflow must contain at least one stage", ErrInvalidDescriptor)
	}
	stages := make(map[StageKey]StageDescriptor, len(workflow.Stages))
	for _, descriptor := range workflow.Stages {
		if err := descriptor.Validate(); err != nil {
			return err
		}
		if _, exists := stages[descriptor.Key]; exists {
			return fmt.Errorf("%w: duplicate stage key %q", ErrInvalidDescriptor, descriptor.Key)
		}
		stages[descriptor.Key] = descriptor
	}
	for _, descriptor := range workflow.Stages {
		for _, dependency := range descriptor.Dependencies {
			if _, exists := stages[dependency]; !exists {
				return fmt.Errorf("%w: stage %q depends on unknown stage %q", ErrInvalidDescriptor, descriptor.Key, dependency)
			}
		}
	}
	if _, err := workflow.TopologicalStages(); err != nil {
		return err
	}
	return nil
}

// Stage returns a deep copy of the descriptor identified by key.
func (workflow WorkflowDescriptor) Stage(key StageKey) (StageDescriptor, bool) {
	for _, descriptor := range workflow.Stages {
		if descriptor.Key == key {
			return descriptor.Clone(), true
		}
	}
	return StageDescriptor{}, false
}

// TopologicalStages returns a deterministic dependency-before-dependent order.
// Stages that are otherwise independent retain declaration order.
func (workflow WorkflowDescriptor) TopologicalStages() ([]StageKey, error) {
	if len(workflow.Stages) == 0 {
		return nil, fmt.Errorf("%w: workflow must contain at least one stage", ErrInvalidDescriptor)
	}
	index := make(map[StageKey]int, len(workflow.Stages))
	inDegree := make(map[StageKey]int, len(workflow.Stages))
	dependents := make(map[StageKey][]StageKey, len(workflow.Stages))
	for position, descriptor := range workflow.Stages {
		if _, exists := index[descriptor.Key]; exists {
			return nil, fmt.Errorf("%w: duplicate stage key %q", ErrInvalidDescriptor, descriptor.Key)
		}
		index[descriptor.Key] = position
		inDegree[descriptor.Key] = len(descriptor.Dependencies)
	}
	for _, descriptor := range workflow.Stages {
		for _, dependency := range descriptor.Dependencies {
			if _, exists := index[dependency]; !exists {
				return nil, fmt.Errorf("%w: stage %q depends on unknown stage %q", ErrInvalidDescriptor, descriptor.Key, dependency)
			}
			dependents[dependency] = append(dependents[dependency], descriptor.Key)
		}
	}
	ready := make([]StageKey, 0, len(workflow.Stages))
	for _, descriptor := range workflow.Stages {
		if inDegree[descriptor.Key] == 0 {
			ready = append(ready, descriptor.Key)
		}
	}
	order := make([]StageKey, 0, len(workflow.Stages))
	for len(ready) > 0 {
		sort.SliceStable(ready, func(left, right int) bool { return index[ready[left]] < index[ready[right]] })
		current := ready[0]
		ready = ready[1:]
		order = append(order, current)
		for _, dependent := range dependents[current] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				ready = append(ready, dependent)
			}
		}
	}
	if len(order) != len(workflow.Stages) {
		return nil, fmt.Errorf("%w: workflow dependency graph contains a cycle", ErrInvalidDescriptor)
	}
	return order, nil
}

// Fingerprint returns a canonical fingerprint for a frozen workflow
// descriptor. Reordering declarations, dependencies, resource sets, or
// capabilities does not alter the result; semantically meaningful fields do.
func (workflow WorkflowDescriptor) Fingerprint() (Fingerprint, error) {
	if err := workflow.Validate(); err != nil {
		return "", err
	}
	canonical := workflow.Clone()
	sort.Slice(canonical.Stages, func(left, right int) bool {
		return canonical.Stages[left].Key < canonical.Stages[right].Key
	})
	for index := range canonical.Stages {
		stage := &canonical.Stages[index]
		sort.Slice(stage.Dependencies, func(left, right int) bool { return stage.Dependencies[left] < stage.Dependencies[right] })
		sort.Slice(stage.Inputs, func(left, right int) bool { return stage.Inputs[left].Name < stage.Inputs[right].Name })
		sort.Slice(stage.Outputs, func(left, right int) bool { return stage.Outputs[left].Name < stage.Outputs[right].Name })
		sort.Slice(stage.ReadSet, func(left, right int) bool { return stage.ReadSet[left] < stage.ReadSet[right] })
		sort.Slice(stage.WriteSet, func(left, right int) bool { return stage.WriteSet[left] < stage.WriteSet[right] })
		sort.Slice(stage.QuotaClaims, func(left, right int) bool {
			return stage.QuotaClaims[left].Dimension < stage.QuotaClaims[right].Dimension
		})
		sort.Slice(stage.Retry.Retryable, func(left, right int) bool { return stage.Retry.Retryable[left] < stage.Retry.Retryable[right] })
		sort.Slice(stage.Verdicts.Allowed, func(left, right int) bool { return stage.Verdicts.Allowed[left] < stage.Verdicts.Allowed[right] })
		sort.Slice(stage.Capabilities, func(left, right int) bool { return stage.Capabilities[left] < stage.Capabilities[right] })
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: encode workflow descriptor: %v", ErrInvalidDescriptor, err)
	}
	return FingerprintBytes("workflowkit.workflow-descriptor.v1", encoded)
}
