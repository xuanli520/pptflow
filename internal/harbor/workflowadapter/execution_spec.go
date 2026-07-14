package workflowadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// RunExecutionSpecFormat identifies the strict, persisted request contract
	// accepted by the Harbor V2 Run entrypoint.
	RunExecutionSpecFormat = "harbor.run-execution-spec.v1"
	// RunExecutionSpecVersion is deliberately separate from Format so an
	// incompatible document revision cannot be accepted by accident.
	RunExecutionSpecVersion = "3"
)

var errInvalidExecutionSpec = errors.New("harbor workflow adapter: invalid run execution spec")

// RunSelectionReference freezes the subject selected for execution. It uses
// durable IDs and a subject digest, never a mutable workspace path.
type RunSelectionReference struct {
	TaskID         string                    `json:"task_id"`
	RevisionID     string                    `json:"revision_id"`
	RevisionDigest workflowkit.SubjectDigest `json:"revision_digest"`
}

func (selection RunSelectionReference) validate() error {
	if err := validatePersistentUUIDv7("selection task id", selection.TaskID); err != nil {
		return err
	}
	if err := validatePersistentUUIDv7("selection revision id", selection.RevisionID); err != nil {
		return err
	}
	if err := selection.RevisionDigest.Validate(); err != nil {
		return fmt.Errorf("%w: selection revision digest: %v", errInvalidExecutionSpec, err)
	}
	return nil
}

// ArtifactReference identifies immutable artifact content that may be bound to
// a stage input port. The content itself remains in the managed object store.
type ArtifactReference struct {
	ID            workflowkit.ArtifactID  `json:"id"`
	ContentDigest workflowkit.Fingerprint `json:"content_digest"`
	SchemaVersion string                  `json:"schema_version"`
}

func (reference ArtifactReference) validate() error {
	if err := validatePersistentUUIDv7("artifact id", string(reference.ID)); err != nil {
		return err
	}
	if err := reference.ContentDigest.Validate(); err != nil {
		return fmt.Errorf("%w: artifact %q content digest: %v", errInvalidExecutionSpec, reference.ID, err)
	}
	if err := validateExecutionSpecString("artifact schema version", reference.SchemaVersion); err != nil {
		return err
	}
	return nil
}

// CheckoutReference is a logical managed checkout handle. It intentionally
// does not expose a host path: runtime adapters resolve the handle under their
// own controlled root after validating the frozen revision identity.
type CheckoutReference struct {
	ID             string                    `json:"id"`
	RevisionID     string                    `json:"revision_id"`
	RevisionDigest workflowkit.SubjectDigest `json:"revision_digest"`
}

func (reference CheckoutReference) validate(selection RunSelectionReference) error {
	if err := validateExecutionSpecString("checkout id", reference.ID); err != nil {
		return err
	}
	if reference.RevisionID != selection.RevisionID {
		return fmt.Errorf("%w: checkout %q revision id does not match selected revision", errInvalidExecutionSpec, reference.ID)
	}
	if reference.RevisionDigest != selection.RevisionDigest {
		return fmt.Errorf("%w: checkout %q revision digest does not match selected revision", errInvalidExecutionSpec, reference.ID)
	}
	return nil
}

// RuntimeReference selects an exact controlled runtime implementation. The
// runtime registry is outside this package; this contract only freezes its
// stable identity and version.
type RuntimeReference struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Version string `json:"version"`
}

// ProviderReference identifies one exact, controlled operation provider. A
// provider is distinct from a runtime: the provider owns the stage operation
// implementation, while a runtime is one immutable execution environment that
// operation may select. Both identities are frozen so changing a local
// provider registration cannot silently alter an admitted Run.
type ProviderReference struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Version string `json:"version"`
}

func (reference ProviderReference) validate() error {
	if err := validateExecutionSpecString("provider id", reference.ID); err != nil {
		return err
	}
	if err := validateExecutionSpecString("provider kind", reference.Kind); err != nil {
		return err
	}
	return validateExecutionSpecString("provider version", reference.Version)
}

func (reference RuntimeReference) validate() error {
	if err := validateExecutionSpecString("runtime id", reference.ID); err != nil {
		return err
	}
	if err := validateExecutionSpecString("runtime kind", reference.Kind); err != nil {
		return err
	}
	if err := validateExecutionSpecString("runtime version", reference.Version); err != nil {
		return err
	}
	return nil
}

// SecretReference identifies a secret held by an external controlled secret
// provider. A spec never contains secret material, environment values, or a
// path to a secret file.
type SecretReference struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Version  string `json:"version"`
}

func (reference SecretReference) validate() error {
	if err := validateExecutionSpecString("secret id", reference.ID); err != nil {
		return err
	}
	if err := validateExecutionSpecString("secret provider", reference.Provider); err != nil {
		return err
	}
	if err := validateExecutionSpecString("secret version", reference.Version); err != nil {
		return err
	}
	return nil
}

// ExecutionReferenceSet is the complete immutable reference inventory for a
// RunExecutionSpec. Stage bindings reference entries by ID; all entries must
// be used, preventing unrelated secrets or mutable ambient configuration from
// being smuggled into a frozen run.
type ExecutionReferenceSet struct {
	Artifacts []ArtifactReference `json:"artifacts"`
	Checkouts []CheckoutReference `json:"checkouts"`
	Runtimes  []RuntimeReference  `json:"runtimes"`
	Providers []ProviderReference `json:"providers"`
	Secrets   []SecretReference   `json:"secrets"`
}

// Clone returns an independently mutable reference inventory.
func (references ExecutionReferenceSet) Clone() ExecutionReferenceSet {
	references.Artifacts = append([]ArtifactReference(nil), references.Artifacts...)
	references.Checkouts = append([]CheckoutReference(nil), references.Checkouts...)
	references.Runtimes = append([]RuntimeReference(nil), references.Runtimes...)
	references.Providers = append([]ProviderReference(nil), references.Providers...)
	references.Secrets = append([]SecretReference(nil), references.Secrets...)
	return references
}

// ArtifactInputReference binds one catalog input port to an immutable artifact
// entry in ExecutionReferenceSet. Runtime-produced predecessor artifacts are
// supplied by verified stage lineage and therefore need not appear here.
type ArtifactInputReference struct {
	Port       string                 `json:"port"`
	ArtifactID workflowkit.ArtifactID `json:"artifact_id"`
}

// StageOperationBinding pins the exact operation selected from a controlled
// provider for one concrete Harbor stage. It has no config bag: all
// stage-specific mutable input must be represented by the surrounding sealed
// binding, immutable reference inventory, or a future additive typed field.
type StageOperationBinding struct {
	ProviderID  string                `json:"provider_id"`
	OperationID string                `json:"operation_id"`
	Version     string                `json:"version"`
	Payload     StageOperationPayload `json:"payload"`
}

func (binding StageOperationBinding) validate() error {
	if err := validateExecutionSpecString("stage operation provider id", binding.ProviderID); err != nil {
		return err
	}
	if err := validateExecutionSpecString("stage operation id", binding.OperationID); err != nil {
		return err
	}
	if err := validateExecutionSpecString("stage operation version", binding.Version); err != nil {
		return err
	}
	return validateStageOperationPayload(binding.Payload)
}

// Clone returns an independently owned exact operation binding.
func (binding StageOperationBinding) Clone() StageOperationBinding {
	binding.Payload = CloneStageOperationPayload(binding.Payload)
	return binding
}

func (reference ArtifactInputReference) validate() error {
	if err := validateExecutionSpecString("artifact input port", reference.Port); err != nil {
		return err
	}
	return validateExecutionSpecString("artifact input artifact id", string(reference.ArtifactID))
}

// StageBindingType is a closed discriminator for the exact Harbor stage
// binding union. It is not a plugin identifier: plugin ID and version are
// frozen separately and cross-validated against the catalog.
type StageBindingType string

const (
	StageBindingRepoPrepare       StageBindingType = "repo_prepare"
	StageBindingRepoAnalyze       StageBindingType = "repo_analyze"
	StageBindingTaskDesign        StageBindingType = "task_design"
	StageBindingTaskReview        StageBindingType = "task_review"
	StageBindingGenerateTaskFiles StageBindingType = "generate_task_files"
	StageBindingInstructionGen    StageBindingType = "instruction_generate"
	StageBindingTaskTOMLGen       StageBindingType = "task_toml_generate"
	StageBindingDockerfileGen     StageBindingType = "dockerfile_generate"
	StageBindingContentReview     StageBindingType = "content_review"
	StageBindingSolveGen          StageBindingType = "solve_generate"
	StageBindingTestGen           StageBindingType = "test_generate"
	StageBindingTestsAnalysis     StageBindingType = "tests_analysis"
	StageBindingSolutionReview    StageBindingType = "solution_review"
	StageBindingMaterializeTask   StageBindingType = "materialize_task"
	StageBindingTaskRepair        StageBindingType = "task_repair"
	StageBindingRuntimeSelfCheck  StageBindingType = "runtime_self_check"
	StageBindingHarborVerify      StageBindingType = "harbor_verify"
	StageBindingDockerBuild       StageBindingType = "docker_build"
	StageBindingInitialVerify     StageBindingType = "initial_verify"
	StageBindingOracleVerify      StageBindingType = "oracle_verify"
	StageBindingCodeEdgeLint      StageBindingType = "codeedge_lint"
	StageBindingQualityCheck      StageBindingType = "quality_check"
	StageBindingSimilarityCheck   StageBindingType = "similarity_check"
	StageBindingFinalReview       StageBindingType = "final_review"
	StageBindingHarborRunQwen     StageBindingType = "harbor_run_qwen"
	StageBindingHarborRunOpus     StageBindingType = "harbor_run_opus"
	StageBindingResultReview      StageBindingType = "result_review"
	StageBindingSubmissionLint    StageBindingType = "submission_lint"
	StageBindingPackage           StageBindingType = "package"
)

// StageBindingBase is shared only by the sealed, concrete Harbor stage union
// below. Its fields are all references or exact immutable bindings; it has no
// untyped config bag and no filesystem or secret value escape hatch.
type StageBindingBase struct {
	Type           StageBindingType          `json:"type"`
	StageKey       workflowkit.StageKey      `json:"stage_key"`
	Plugin         workflowkit.PluginBinding `json:"plugin"`
	ArtifactInputs []ArtifactInputReference  `json:"artifact_inputs"`
	CheckoutID     string                    `json:"checkout_id"`
	RuntimeID      string                    `json:"runtime_id"`
	Operation      StageOperationBinding     `json:"operation"`
	SecretIDs      []string                  `json:"secret_ids"`
}

// Clone returns an independently mutable common binding payload.
func (binding StageBindingBase) Clone() StageBindingBase {
	binding.ArtifactInputs = append([]ArtifactInputReference(nil), binding.ArtifactInputs...)
	binding.SecretIDs = append([]string(nil), binding.SecretIDs...)
	binding.Operation = binding.Operation.Clone()
	return binding
}

// StageExecutionBinding is deliberately sealed. A parser can only construct
// one of the concrete types declared by this versioned Harbor contract.
type StageExecutionBinding interface {
	stageExecutionBinding()
}

// The following structs are the full per-Harbor-stage binding union. Even
// stages that currently need only the common frozen references have their own
// concrete type, so a later stage-specific field is an additive, reviewable
// contract change rather than an untyped configuration branch.
type RepoPrepareBinding struct{ StageBindingBase }
type RepoAnalyzeBinding struct{ StageBindingBase }
type TaskDesignBinding struct{ StageBindingBase }
type TaskReviewBinding struct{ StageBindingBase }
type GenerateTaskFilesBinding struct{ StageBindingBase }
type InstructionGenBinding struct{ StageBindingBase }
type TaskTOMLGenBinding struct{ StageBindingBase }
type DockerfileGenBinding struct{ StageBindingBase }
type ContentReviewBinding struct{ StageBindingBase }
type SolveGenBinding struct{ StageBindingBase }
type TestGenBinding struct{ StageBindingBase }
type TestsAnalysisBinding struct{ StageBindingBase }
type SolutionReviewBinding struct{ StageBindingBase }
type MaterializeTaskBinding struct{ StageBindingBase }
type TaskRepairBinding struct{ StageBindingBase }
type RuntimeSelfCheckBinding struct{ StageBindingBase }
type HarborVerifyBinding struct{ StageBindingBase }
type DockerBuildBinding struct{ StageBindingBase }
type InitialVerifyBinding struct{ StageBindingBase }
type OracleVerifyBinding struct{ StageBindingBase }
type CodeEdgeLintBinding struct{ StageBindingBase }
type QualityCheckBinding struct{ StageBindingBase }
type SimilarityCheckBinding struct{ StageBindingBase }
type FinalReviewBinding struct{ StageBindingBase }
type HarborRunQwenBinding struct{ StageBindingBase }
type HarborRunOpusBinding struct{ StageBindingBase }
type ResultReviewBinding struct{ StageBindingBase }
type SubmissionLintBinding struct{ StageBindingBase }
type PackageBinding struct{ StageBindingBase }

func (RepoPrepareBinding) stageExecutionBinding()       {}
func (RepoAnalyzeBinding) stageExecutionBinding()       {}
func (TaskDesignBinding) stageExecutionBinding()        {}
func (TaskReviewBinding) stageExecutionBinding()        {}
func (GenerateTaskFilesBinding) stageExecutionBinding() {}
func (InstructionGenBinding) stageExecutionBinding()    {}
func (TaskTOMLGenBinding) stageExecutionBinding()       {}
func (DockerfileGenBinding) stageExecutionBinding()     {}
func (ContentReviewBinding) stageExecutionBinding()     {}
func (SolveGenBinding) stageExecutionBinding()          {}
func (TestGenBinding) stageExecutionBinding()           {}
func (TestsAnalysisBinding) stageExecutionBinding()     {}
func (SolutionReviewBinding) stageExecutionBinding()    {}
func (MaterializeTaskBinding) stageExecutionBinding()   {}
func (TaskRepairBinding) stageExecutionBinding()        {}
func (RuntimeSelfCheckBinding) stageExecutionBinding()  {}
func (HarborVerifyBinding) stageExecutionBinding()      {}
func (DockerBuildBinding) stageExecutionBinding()       {}
func (InitialVerifyBinding) stageExecutionBinding()     {}
func (OracleVerifyBinding) stageExecutionBinding()      {}
func (CodeEdgeLintBinding) stageExecutionBinding()      {}
func (QualityCheckBinding) stageExecutionBinding()      {}
func (SimilarityCheckBinding) stageExecutionBinding()   {}
func (FinalReviewBinding) stageExecutionBinding()       {}
func (HarborRunQwenBinding) stageExecutionBinding()     {}
func (HarborRunOpusBinding) stageExecutionBinding()     {}
func (ResultReviewBinding) stageExecutionBinding()      {}
func (SubmissionLintBinding) stageExecutionBinding()    {}
func (PackageBinding) stageExecutionBinding()           {}

// RunExecutionSpec is the V2-only typed execution selection, reference set,
// and complete per-stage binding union. It is intentionally independent from
// ExecutionProfile: profile carries budget policy while this document carries
// the immutable runtime inputs selected for one Run.
type RunExecutionSpec struct {
	Format     string                  `json:"format"`
	Version    string                  `json:"version"`
	Template   TemplateReference       `json:"template"`
	Selection  RunSelectionReference   `json:"selection"`
	References ExecutionReferenceSet   `json:"references"`
	Stages     []StageExecutionBinding `json:"stages"`
}

// StageOperationResolution is the complete immutable selection that a
// controlled Harbor provider must validate before it obtains an executable
// operation. It exposes stable references only; provider implementations own
// their own controlled checkout/runtime/secret resolution and never receive
// ambient paths or secret values from this document.
type StageOperationResolution struct {
	StageKey       workflowkit.StageKey
	StageType      StageBindingType
	Plugin         workflowkit.PluginBinding
	Provider       ProviderReference
	Operation      StageOperationBinding
	Checkout       CheckoutReference
	Runtime        RuntimeReference
	ArtifactInputs []ArtifactInputReference
	Secrets        []SecretReference
}

// Clone returns independently owned reference slices.
func (resolution StageOperationResolution) Clone() StageOperationResolution {
	resolution.Operation = resolution.Operation.Clone()
	resolution.ArtifactInputs = append([]ArtifactInputReference(nil), resolution.ArtifactInputs...)
	resolution.Secrets = append([]SecretReference(nil), resolution.Secrets...)
	return resolution
}

// StageOperationResolver is the prepare-time capability boundary used to
// prove that a frozen execution specification can be executed locally. Its
// implementation must only validate exact controlled registrations; it must
// not start a provider process, create a checkout, consume a secret, or make a
// remote call.
type StageOperationResolver interface {
	ValidateStageOperation(StageOperationResolution) error
}

// Clone returns a deep copy. Concrete stage binding types are preserved.
func (spec RunExecutionSpec) Clone() RunExecutionSpec {
	spec.References = spec.References.Clone()
	stages := spec.Stages
	spec.Stages = make([]StageExecutionBinding, len(stages))
	for index, binding := range stages {
		spec.Stages[index] = cloneStageExecutionBinding(binding)
	}
	return spec
}

// StageBinding returns a deep copy of the frozen concrete binding for key.
func (spec RunExecutionSpec) StageBinding(key workflowkit.StageKey) (StageExecutionBinding, bool) {
	for _, binding := range spec.Stages {
		base, ok := stageBindingBaseOf(binding)
		if ok && base.StageKey == key {
			return cloneStageExecutionBinding(binding), true
		}
	}
	return nil, false
}

// ResolveStageOperation returns the exact operation/provider selection for a
// catalog stage after validating all cross-reference and sealed-binding
// invariants. It is the execution-time counterpart to ValidateWithOperations.
func (spec RunExecutionSpec) ResolveStageOperation(key workflowkit.StageKey) (StageOperationResolution, error) {
	if err := spec.Validate(); err != nil {
		return StageOperationResolution{}, err
	}
	binding, found := spec.StageBinding(key)
	if !found {
		return StageOperationResolution{}, fmt.Errorf("%w: stage binding %q is missing", errInvalidExecutionSpec, key)
	}
	base, ok := stageBindingBaseOf(binding)
	if !ok {
		return StageOperationResolution{}, fmt.Errorf("%w: stage binding %q is unsupported", errInvalidExecutionSpec, key)
	}
	stageKey, stageType, ok := stageBindingIdentity(binding)
	if !ok || stageKey != key || stageType != base.Type {
		return StageOperationResolution{}, fmt.Errorf("%w: stage binding %q identity is invalid", errInvalidExecutionSpec, key)
	}
	index, err := newExecutionReferenceIndex(spec.References, spec.Selection)
	if err != nil {
		return StageOperationResolution{}, err
	}
	provider, present := index.providers[base.Operation.ProviderID]
	if !present {
		return StageOperationResolution{}, fmt.Errorf("%w: stage %q operation provider %q is missing", errInvalidExecutionSpec, key, base.Operation.ProviderID)
	}
	checkout, present := index.checkouts[base.CheckoutID]
	if !present {
		return StageOperationResolution{}, fmt.Errorf("%w: stage %q checkout %q is missing", errInvalidExecutionSpec, key, base.CheckoutID)
	}
	runtime, present := index.runtimes[base.RuntimeID]
	if !present {
		return StageOperationResolution{}, fmt.Errorf("%w: stage %q runtime %q is missing", errInvalidExecutionSpec, key, base.RuntimeID)
	}
	secrets := make([]SecretReference, 0, len(base.SecretIDs))
	for _, secretID := range base.SecretIDs {
		secret, present := index.secrets[secretID]
		if !present {
			return StageOperationResolution{}, fmt.Errorf("%w: stage %q secret %q is missing", errInvalidExecutionSpec, key, secretID)
		}
		secrets = append(secrets, secret)
	}
	return StageOperationResolution{
		StageKey: key, StageType: stageType, Plugin: base.Plugin, Provider: provider,
		Operation: base.Operation, Checkout: checkout, Runtime: runtime,
		ArtifactInputs: append([]ArtifactInputReference(nil), base.ArtifactInputs...), Secrets: secrets,
	}, nil
}

// ValidateWithOperationResolver validates the structural spec and then proves
// every sealed stage binding can resolve to one locally installed, exact
// provider operation. Call this during StartRun prepare, before any lifecycle
// mutation or durable job is created.
func (spec RunExecutionSpec) ValidateWithOperationResolver(resolver StageOperationResolver) error {
	if resolver == nil {
		return fmt.Errorf("%w: stage operation resolver is required", errInvalidExecutionSpec)
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	template, err := ResolveWorkflowTemplate(spec.Template)
	if err != nil {
		return fmt.Errorf("%w: execution specification template: %v", errInvalidExecutionSpec, err)
	}
	for _, definition := range template.Catalog.Stages {
		resolution, err := spec.ResolveStageOperation(definition.Key)
		if err != nil {
			return err
		}
		if err := resolver.ValidateStageOperation(resolution.Clone()); err != nil {
			return fmt.Errorf("%w: stage %q operation %s@%s via provider %s@%s: %w", errInvalidExecutionSpec, definition.Key, resolution.Operation.OperationID, resolution.Operation.Version, resolution.Provider.ID, resolution.Provider.Version, err)
		}
	}
	return nil
}

// Validate checks this spec against the exact closed template frozen into the
// document. It never falls back to StandardStageCatalog.
func (spec RunExecutionSpec) Validate() error {
	template, err := ResolveWorkflowTemplate(spec.Template)
	if err != nil {
		return fmt.Errorf("%w: execution specification template: %v", errInvalidExecutionSpec, err)
	}
	return spec.ValidateFor(template.Catalog)
}

// ValidateFor checks the full typed binding union against a specific catalog.
// It is useful to compilers that already hold a frozen catalog snapshot.
func (spec RunExecutionSpec) ValidateFor(catalog StageCatalog) error {
	if spec.Format != RunExecutionSpecFormat {
		return fmt.Errorf("%w: unsupported format %q", errInvalidExecutionSpec, spec.Format)
	}
	if spec.Version != RunExecutionSpecVersion {
		return fmt.Errorf("%w: unsupported version %q", errInvalidExecutionSpec, spec.Version)
	}
	if err := spec.Template.Validate(); err != nil {
		return fmt.Errorf("%w: execution specification template: %v", errInvalidExecutionSpec, err)
	}
	if err := spec.Selection.validate(); err != nil {
		return err
	}
	if err := catalog.Validate(); err != nil {
		return fmt.Errorf("%w: catalog: %v", errInvalidExecutionSpec, err)
	}
	if !spec.Template.Equal(catalog.Template) {
		return fmt.Errorf("%w: %w: execution specification template %s@%s does not match catalog template %s@%s", errInvalidExecutionSpec, errTemplateMismatch, spec.Template.ID, spec.Template.Version, catalog.Template.ID, catalog.Template.Version)
	}
	index, err := newExecutionReferenceIndex(spec.References, spec.Selection)
	if err != nil {
		return err
	}
	if len(spec.Stages) != len(catalog.Stages) {
		return fmt.Errorf("%w: got %d stage bindings; want %d", errInvalidExecutionSpec, len(spec.Stages), len(catalog.Stages))
	}
	seen := make(map[workflowkit.StageKey]struct{}, len(spec.Stages))
	for _, binding := range spec.Stages {
		base, ok := stageBindingBaseOf(binding)
		if !ok {
			return fmt.Errorf("%w: unsupported concrete stage binding %T", errInvalidExecutionSpec, binding)
		}
		expectedKey, expectedType, ok := stageBindingIdentity(binding)
		if !ok {
			return fmt.Errorf("%w: unsupported concrete stage binding %T", errInvalidExecutionSpec, binding)
		}
		if base.Type != expectedType {
			return fmt.Errorf("%w: stage binding %q type %q, want %q", errInvalidExecutionSpec, base.StageKey, base.Type, expectedType)
		}
		if base.StageKey != expectedKey {
			return fmt.Errorf("%w: binding type %q has stage key %q, want %q", errInvalidExecutionSpec, base.Type, base.StageKey, expectedKey)
		}
		if _, duplicate := seen[base.StageKey]; duplicate {
			return fmt.Errorf("%w: duplicate stage binding %q", errInvalidExecutionSpec, base.StageKey)
		}
		seen[base.StageKey] = struct{}{}
		definition, present := catalog.Stage(base.StageKey)
		if !present {
			return fmt.Errorf("%w: stage binding %q is not in the catalog", errInvalidExecutionSpec, base.StageKey)
		}
		if base.Plugin.ID != definition.Plugin.ID || base.Plugin.Version != definition.Plugin.Version {
			return fmt.Errorf("%w: stage %q plugin %q@%q does not match catalog %q@%q", errInvalidExecutionSpec, base.StageKey, base.Plugin.ID, base.Plugin.Version, definition.Plugin.ID, definition.Plugin.Version)
		}
		if err := validateStageBindingBase(base, definition, index); err != nil {
			return err
		}
	}
	for _, definition := range catalog.Stages {
		if _, present := seen[definition.Key]; !present {
			return fmt.Errorf("%w: missing stage binding %q", errInvalidExecutionSpec, definition.Key)
		}
	}
	if err := index.validateAllUsed(); err != nil {
		return err
	}
	return nil
}

// CanonicalJSON returns a validated canonical representation. Semantically
// unordered reference and binding entries are sorted, while every field value
// remains fingerprint-significant.
func (spec RunExecutionSpec) CanonicalJSON() ([]byte, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	canonical := spec.Clone()
	canonical.normalize()
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("%w: encode canonical execution spec: %v", errInvalidExecutionSpec, err)
	}
	return encoded, nil
}

// Fingerprint returns a stable execution-spec identity. Reordering stage or
// reference declarations does not change it; changing any frozen field does.
func (spec RunExecutionSpec) Fingerprint() (workflowkit.Fingerprint, error) {
	canonical, err := spec.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return workflowkit.FingerprintBytes("harbor.workflowadapter.run-execution-spec.v2", canonical)
}

// ParseRunExecutionSpecJSON strictly decodes the versioned public document.
// It rejects unknown fields, duplicate object keys, trailing values, unknown
// discriminators, incomplete binding coverage, and catalog/plugin drift.
func ParseRunExecutionSpecJSON(raw []byte) (RunExecutionSpec, error) {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return RunExecutionSpec{}, fmt.Errorf("decode run execution spec: %w", err)
	}
	var document runExecutionSpecDocument
	if err := decodeExecutionSpecJSON(raw, &document); err != nil {
		return RunExecutionSpec{}, fmt.Errorf("decode run execution spec: %w", err)
	}
	spec := RunExecutionSpec{
		Format: document.Format, Version: document.Version, Template: document.Template, Selection: document.Selection,
		References: document.References, Stages: make([]StageExecutionBinding, 0, len(document.Stages)),
	}
	for index, rawBinding := range document.Stages {
		binding, err := parseStageExecutionBinding(rawBinding)
		if err != nil {
			return RunExecutionSpec{}, fmt.Errorf("decode stage binding %d: %w", index, err)
		}
		spec.Stages = append(spec.Stages, binding)
	}
	if err := spec.Validate(); err != nil {
		return RunExecutionSpec{}, err
	}
	return spec, nil
}

type runExecutionSpecDocument struct {
	Format     string                `json:"format"`
	Version    string                `json:"version"`
	Template   TemplateReference     `json:"template"`
	Selection  RunSelectionReference `json:"selection"`
	References ExecutionReferenceSet `json:"references"`
	Stages     []json.RawMessage     `json:"stages"`
}

type stageBindingDiscriminator struct {
	Type StageBindingType `json:"type"`
}

func parseStageExecutionBinding(raw json.RawMessage) (StageExecutionBinding, error) {
	var discriminator stageBindingDiscriminator
	if err := json.Unmarshal(raw, &discriminator); err != nil {
		return nil, fmt.Errorf("decode stage binding discriminator: %w", err)
	}
	decode := func(destination StageExecutionBinding) (StageExecutionBinding, error) {
		if err := decodeExecutionSpecJSON(raw, destination); err != nil {
			return nil, err
		}
		return dereferenceStageBinding(destination), nil
	}
	switch discriminator.Type {
	case StageBindingRepoPrepare:
		return decode(&RepoPrepareBinding{})
	case StageBindingRepoAnalyze:
		return decode(&RepoAnalyzeBinding{})
	case StageBindingTaskDesign:
		return decode(&TaskDesignBinding{})
	case StageBindingTaskReview:
		return decode(&TaskReviewBinding{})
	case StageBindingGenerateTaskFiles:
		return decode(&GenerateTaskFilesBinding{})
	case StageBindingInstructionGen:
		return decode(&InstructionGenBinding{})
	case StageBindingTaskTOMLGen:
		return decode(&TaskTOMLGenBinding{})
	case StageBindingDockerfileGen:
		return decode(&DockerfileGenBinding{})
	case StageBindingContentReview:
		return decode(&ContentReviewBinding{})
	case StageBindingSolveGen:
		return decode(&SolveGenBinding{})
	case StageBindingTestGen:
		return decode(&TestGenBinding{})
	case StageBindingTestsAnalysis:
		return decode(&TestsAnalysisBinding{})
	case StageBindingSolutionReview:
		return decode(&SolutionReviewBinding{})
	case StageBindingMaterializeTask:
		return decode(&MaterializeTaskBinding{})
	case StageBindingTaskRepair:
		return decode(&TaskRepairBinding{})
	case StageBindingRuntimeSelfCheck:
		return decode(&RuntimeSelfCheckBinding{})
	case StageBindingHarborVerify:
		return decode(&HarborVerifyBinding{})
	case StageBindingDockerBuild:
		return decode(&DockerBuildBinding{})
	case StageBindingInitialVerify:
		return decode(&InitialVerifyBinding{})
	case StageBindingOracleVerify:
		return decode(&OracleVerifyBinding{})
	case StageBindingCodeEdgeLint:
		return decode(&CodeEdgeLintBinding{})
	case StageBindingQualityCheck:
		return decode(&QualityCheckBinding{})
	case StageBindingSimilarityCheck:
		return decode(&SimilarityCheckBinding{})
	case StageBindingFinalReview:
		return decode(&FinalReviewBinding{})
	case StageBindingHarborRunQwen:
		return decode(&HarborRunQwenBinding{})
	case StageBindingHarborRunOpus:
		return decode(&HarborRunOpusBinding{})
	case StageBindingResultReview:
		return decode(&ResultReviewBinding{})
	case StageBindingSubmissionLint:
		return decode(&SubmissionLintBinding{})
	case StageBindingPackage:
		return decode(&PackageBinding{})
	default:
		return nil, fmt.Errorf("%w: unsupported stage binding type %q", errInvalidExecutionSpec, discriminator.Type)
	}
}

func dereferenceStageBinding(binding StageExecutionBinding) StageExecutionBinding {
	switch typed := binding.(type) {
	case *RepoPrepareBinding:
		return *typed
	case *RepoAnalyzeBinding:
		return *typed
	case *TaskDesignBinding:
		return *typed
	case *TaskReviewBinding:
		return *typed
	case *GenerateTaskFilesBinding:
		return *typed
	case *InstructionGenBinding:
		return *typed
	case *TaskTOMLGenBinding:
		return *typed
	case *DockerfileGenBinding:
		return *typed
	case *ContentReviewBinding:
		return *typed
	case *SolveGenBinding:
		return *typed
	case *TestGenBinding:
		return *typed
	case *TestsAnalysisBinding:
		return *typed
	case *SolutionReviewBinding:
		return *typed
	case *MaterializeTaskBinding:
		return *typed
	case *TaskRepairBinding:
		return *typed
	case *RuntimeSelfCheckBinding:
		return *typed
	case *HarborVerifyBinding:
		return *typed
	case *DockerBuildBinding:
		return *typed
	case *InitialVerifyBinding:
		return *typed
	case *OracleVerifyBinding:
		return *typed
	case *CodeEdgeLintBinding:
		return *typed
	case *QualityCheckBinding:
		return *typed
	case *SimilarityCheckBinding:
		return *typed
	case *FinalReviewBinding:
		return *typed
	case *HarborRunQwenBinding:
		return *typed
	case *HarborRunOpusBinding:
		return *typed
	case *ResultReviewBinding:
		return *typed
	case *SubmissionLintBinding:
		return *typed
	case *PackageBinding:
		return *typed
	default:
		return binding
	}
}

func stageBindingBaseOf(binding StageExecutionBinding) (StageBindingBase, bool) {
	switch typed := binding.(type) {
	case RepoPrepareBinding:
		return typed.StageBindingBase, true
	case RepoAnalyzeBinding:
		return typed.StageBindingBase, true
	case TaskDesignBinding:
		return typed.StageBindingBase, true
	case TaskReviewBinding:
		return typed.StageBindingBase, true
	case GenerateTaskFilesBinding:
		return typed.StageBindingBase, true
	case InstructionGenBinding:
		return typed.StageBindingBase, true
	case TaskTOMLGenBinding:
		return typed.StageBindingBase, true
	case DockerfileGenBinding:
		return typed.StageBindingBase, true
	case ContentReviewBinding:
		return typed.StageBindingBase, true
	case SolveGenBinding:
		return typed.StageBindingBase, true
	case TestGenBinding:
		return typed.StageBindingBase, true
	case TestsAnalysisBinding:
		return typed.StageBindingBase, true
	case SolutionReviewBinding:
		return typed.StageBindingBase, true
	case MaterializeTaskBinding:
		return typed.StageBindingBase, true
	case TaskRepairBinding:
		return typed.StageBindingBase, true
	case RuntimeSelfCheckBinding:
		return typed.StageBindingBase, true
	case HarborVerifyBinding:
		return typed.StageBindingBase, true
	case DockerBuildBinding:
		return typed.StageBindingBase, true
	case InitialVerifyBinding:
		return typed.StageBindingBase, true
	case OracleVerifyBinding:
		return typed.StageBindingBase, true
	case CodeEdgeLintBinding:
		return typed.StageBindingBase, true
	case QualityCheckBinding:
		return typed.StageBindingBase, true
	case SimilarityCheckBinding:
		return typed.StageBindingBase, true
	case FinalReviewBinding:
		return typed.StageBindingBase, true
	case HarborRunQwenBinding:
		return typed.StageBindingBase, true
	case HarborRunOpusBinding:
		return typed.StageBindingBase, true
	case ResultReviewBinding:
		return typed.StageBindingBase, true
	case SubmissionLintBinding:
		return typed.StageBindingBase, true
	case PackageBinding:
		return typed.StageBindingBase, true
	default:
		return StageBindingBase{}, false
	}
}

func stageBindingIdentity(binding StageExecutionBinding) (workflowkit.StageKey, StageBindingType, bool) {
	switch binding.(type) {
	case RepoPrepareBinding:
		return "repo_prepare", StageBindingRepoPrepare, true
	case RepoAnalyzeBinding:
		return "repo_analyze", StageBindingRepoAnalyze, true
	case TaskDesignBinding:
		return "task_design", StageBindingTaskDesign, true
	case TaskReviewBinding:
		return "task_review", StageBindingTaskReview, true
	case GenerateTaskFilesBinding:
		return "generate_task_files", StageBindingGenerateTaskFiles, true
	case InstructionGenBinding:
		return "instruction_generate", StageBindingInstructionGen, true
	case TaskTOMLGenBinding:
		return "task_toml_generate", StageBindingTaskTOMLGen, true
	case DockerfileGenBinding:
		return "dockerfile_generate", StageBindingDockerfileGen, true
	case ContentReviewBinding:
		return "content_review", StageBindingContentReview, true
	case SolveGenBinding:
		return "solve_generate", StageBindingSolveGen, true
	case TestGenBinding:
		return "test_generate", StageBindingTestGen, true
	case TestsAnalysisBinding:
		return "tests_analysis", StageBindingTestsAnalysis, true
	case SolutionReviewBinding:
		return "solution_review", StageBindingSolutionReview, true
	case MaterializeTaskBinding:
		return "materialize_task", StageBindingMaterializeTask, true
	case TaskRepairBinding:
		return "task_repair", StageBindingTaskRepair, true
	case RuntimeSelfCheckBinding:
		return "runtime_self_check", StageBindingRuntimeSelfCheck, true
	case HarborVerifyBinding:
		return "harbor_verify", StageBindingHarborVerify, true
	case DockerBuildBinding:
		return "docker_build", StageBindingDockerBuild, true
	case InitialVerifyBinding:
		return "initial_verify", StageBindingInitialVerify, true
	case OracleVerifyBinding:
		return "oracle_verify", StageBindingOracleVerify, true
	case CodeEdgeLintBinding:
		return "codeedge_lint", StageBindingCodeEdgeLint, true
	case QualityCheckBinding:
		return "quality_check", StageBindingQualityCheck, true
	case SimilarityCheckBinding:
		return "similarity_check", StageBindingSimilarityCheck, true
	case FinalReviewBinding:
		return "final_review", StageBindingFinalReview, true
	case HarborRunQwenBinding:
		return "harbor_run_qwen", StageBindingHarborRunQwen, true
	case HarborRunOpusBinding:
		return "harbor_run_opus", StageBindingHarborRunOpus, true
	case ResultReviewBinding:
		return "result_review", StageBindingResultReview, true
	case SubmissionLintBinding:
		return "submission_lint", StageBindingSubmissionLint, true
	case PackageBinding:
		return "package", StageBindingPackage, true
	default:
		return "", "", false
	}
}

func cloneStageExecutionBinding(binding StageExecutionBinding) StageExecutionBinding {
	base, ok := stageBindingBaseOf(binding)
	if !ok {
		return binding
	}
	base = base.Clone()
	switch binding.(type) {
	case RepoPrepareBinding:
		return RepoPrepareBinding{StageBindingBase: base}
	case RepoAnalyzeBinding:
		return RepoAnalyzeBinding{StageBindingBase: base}
	case TaskDesignBinding:
		return TaskDesignBinding{StageBindingBase: base}
	case TaskReviewBinding:
		return TaskReviewBinding{StageBindingBase: base}
	case GenerateTaskFilesBinding:
		return GenerateTaskFilesBinding{StageBindingBase: base}
	case InstructionGenBinding:
		return InstructionGenBinding{StageBindingBase: base}
	case TaskTOMLGenBinding:
		return TaskTOMLGenBinding{StageBindingBase: base}
	case DockerfileGenBinding:
		return DockerfileGenBinding{StageBindingBase: base}
	case ContentReviewBinding:
		return ContentReviewBinding{StageBindingBase: base}
	case SolveGenBinding:
		return SolveGenBinding{StageBindingBase: base}
	case TestGenBinding:
		return TestGenBinding{StageBindingBase: base}
	case TestsAnalysisBinding:
		return TestsAnalysisBinding{StageBindingBase: base}
	case SolutionReviewBinding:
		return SolutionReviewBinding{StageBindingBase: base}
	case MaterializeTaskBinding:
		return MaterializeTaskBinding{StageBindingBase: base}
	case TaskRepairBinding:
		return TaskRepairBinding{StageBindingBase: base}
	case RuntimeSelfCheckBinding:
		return RuntimeSelfCheckBinding{StageBindingBase: base}
	case HarborVerifyBinding:
		return HarborVerifyBinding{StageBindingBase: base}
	case DockerBuildBinding:
		return DockerBuildBinding{StageBindingBase: base}
	case InitialVerifyBinding:
		return InitialVerifyBinding{StageBindingBase: base}
	case OracleVerifyBinding:
		return OracleVerifyBinding{StageBindingBase: base}
	case CodeEdgeLintBinding:
		return CodeEdgeLintBinding{StageBindingBase: base}
	case QualityCheckBinding:
		return QualityCheckBinding{StageBindingBase: base}
	case SimilarityCheckBinding:
		return SimilarityCheckBinding{StageBindingBase: base}
	case FinalReviewBinding:
		return FinalReviewBinding{StageBindingBase: base}
	case HarborRunQwenBinding:
		return HarborRunQwenBinding{StageBindingBase: base}
	case HarborRunOpusBinding:
		return HarborRunOpusBinding{StageBindingBase: base}
	case ResultReviewBinding:
		return ResultReviewBinding{StageBindingBase: base}
	case SubmissionLintBinding:
		return SubmissionLintBinding{StageBindingBase: base}
	case PackageBinding:
		return PackageBinding{StageBindingBase: base}
	default:
		return binding
	}
}

type executionReferenceIndex struct {
	artifacts     map[workflowkit.ArtifactID]ArtifactReference
	checkouts     map[string]CheckoutReference
	runtimes      map[string]RuntimeReference
	providers     map[string]ProviderReference
	secrets       map[string]SecretReference
	usedArtifacts map[workflowkit.ArtifactID]struct{}
	usedCheckouts map[string]struct{}
	usedRuntimes  map[string]struct{}
	usedProviders map[string]struct{}
	usedSecrets   map[string]struct{}
}

func newExecutionReferenceIndex(references ExecutionReferenceSet, selection RunSelectionReference) (executionReferenceIndex, error) {
	index := executionReferenceIndex{
		artifacts:     make(map[workflowkit.ArtifactID]ArtifactReference, len(references.Artifacts)),
		checkouts:     make(map[string]CheckoutReference, len(references.Checkouts)),
		runtimes:      make(map[string]RuntimeReference, len(references.Runtimes)),
		providers:     make(map[string]ProviderReference, len(references.Providers)),
		secrets:       make(map[string]SecretReference, len(references.Secrets)),
		usedArtifacts: make(map[workflowkit.ArtifactID]struct{}), usedCheckouts: make(map[string]struct{}),
		usedRuntimes: make(map[string]struct{}), usedProviders: make(map[string]struct{}), usedSecrets: make(map[string]struct{}),
	}
	for _, artifact := range references.Artifacts {
		if err := artifact.validate(); err != nil {
			return executionReferenceIndex{}, err
		}
		if _, duplicate := index.artifacts[artifact.ID]; duplicate {
			return executionReferenceIndex{}, fmt.Errorf("%w: duplicate artifact reference %q", errInvalidExecutionSpec, artifact.ID)
		}
		index.artifacts[artifact.ID] = artifact
	}
	for _, checkout := range references.Checkouts {
		if err := checkout.validate(selection); err != nil {
			return executionReferenceIndex{}, err
		}
		if _, duplicate := index.checkouts[checkout.ID]; duplicate {
			return executionReferenceIndex{}, fmt.Errorf("%w: duplicate checkout reference %q", errInvalidExecutionSpec, checkout.ID)
		}
		index.checkouts[checkout.ID] = checkout
	}
	for _, runtime := range references.Runtimes {
		if err := runtime.validate(); err != nil {
			return executionReferenceIndex{}, err
		}
		if _, duplicate := index.runtimes[runtime.ID]; duplicate {
			return executionReferenceIndex{}, fmt.Errorf("%w: duplicate runtime reference %q", errInvalidExecutionSpec, runtime.ID)
		}
		index.runtimes[runtime.ID] = runtime
	}
	for _, provider := range references.Providers {
		if err := provider.validate(); err != nil {
			return executionReferenceIndex{}, err
		}
		if _, duplicate := index.providers[provider.ID]; duplicate {
			return executionReferenceIndex{}, fmt.Errorf("%w: duplicate provider reference %q", errInvalidExecutionSpec, provider.ID)
		}
		index.providers[provider.ID] = provider
	}
	for _, secret := range references.Secrets {
		if err := secret.validate(); err != nil {
			return executionReferenceIndex{}, err
		}
		if _, duplicate := index.secrets[secret.ID]; duplicate {
			return executionReferenceIndex{}, fmt.Errorf("%w: duplicate secret reference %q", errInvalidExecutionSpec, secret.ID)
		}
		index.secrets[secret.ID] = secret
	}
	return index, nil
}

func validateStageBindingBase(binding StageBindingBase, definition StageDefinition, index executionReferenceIndex) error {
	if err := binding.Plugin.Validate(); err != nil {
		return fmt.Errorf("%w: stage %q plugin: %v", errInvalidExecutionSpec, binding.StageKey, err)
	}
	if err := validateExecutionSpecString("stage checkout id", binding.CheckoutID); err != nil {
		return err
	}
	if _, present := index.checkouts[binding.CheckoutID]; !present {
		return fmt.Errorf("%w: stage %q references unknown checkout %q", errInvalidExecutionSpec, binding.StageKey, binding.CheckoutID)
	}
	index.usedCheckouts[binding.CheckoutID] = struct{}{}
	if err := validateExecutionSpecString("stage runtime id", binding.RuntimeID); err != nil {
		return err
	}
	if _, present := index.runtimes[binding.RuntimeID]; !present {
		return fmt.Errorf("%w: stage %q references unknown runtime %q", errInvalidExecutionSpec, binding.StageKey, binding.RuntimeID)
	}
	index.usedRuntimes[binding.RuntimeID] = struct{}{}
	if err := binding.Operation.validate(); err != nil {
		return fmt.Errorf("%w: stage %q operation: %v", errInvalidExecutionSpec, binding.StageKey, err)
	}
	if _, present := index.providers[binding.Operation.ProviderID]; !present {
		return fmt.Errorf("%w: stage %q references unknown operation provider %q", errInvalidExecutionSpec, binding.StageKey, binding.Operation.ProviderID)
	}
	index.usedProviders[binding.Operation.ProviderID] = struct{}{}
	inputSpecs := make(map[string]workflowkit.ArtifactSpec, len(definition.Inputs))
	for _, input := range definition.Inputs {
		inputSpecs[input.Name] = input
	}
	seenPorts := make(map[string]struct{}, len(binding.ArtifactInputs))
	for _, input := range binding.ArtifactInputs {
		if err := input.validate(); err != nil {
			return fmt.Errorf("%w: stage %q: %v", errInvalidExecutionSpec, binding.StageKey, err)
		}
		if _, duplicate := seenPorts[input.Port]; duplicate {
			return fmt.Errorf("%w: stage %q has duplicate artifact input port %q", errInvalidExecutionSpec, binding.StageKey, input.Port)
		}
		seenPorts[input.Port] = struct{}{}
		specification, declared := inputSpecs[input.Port]
		if !declared {
			return fmt.Errorf("%w: stage %q binds undeclared artifact input port %q", errInvalidExecutionSpec, binding.StageKey, input.Port)
		}
		artifact, present := index.artifacts[input.ArtifactID]
		if !present {
			return fmt.Errorf("%w: stage %q references unknown artifact %q", errInvalidExecutionSpec, binding.StageKey, input.ArtifactID)
		}
		if artifact.SchemaVersion != specification.SchemaVersion {
			return fmt.Errorf("%w: stage %q artifact %q schema %q, want %q", errInvalidExecutionSpec, binding.StageKey, input.ArtifactID, artifact.SchemaVersion, specification.SchemaVersion)
		}
		index.usedArtifacts[input.ArtifactID] = struct{}{}
	}
	seenSecrets := make(map[string]struct{}, len(binding.SecretIDs))
	for _, secretID := range binding.SecretIDs {
		if err := validateExecutionSpecString("stage secret id", secretID); err != nil {
			return err
		}
		if _, duplicate := seenSecrets[secretID]; duplicate {
			return fmt.Errorf("%w: stage %q has duplicate secret reference %q", errInvalidExecutionSpec, binding.StageKey, secretID)
		}
		seenSecrets[secretID] = struct{}{}
		if _, present := index.secrets[secretID]; !present {
			return fmt.Errorf("%w: stage %q references unknown secret %q", errInvalidExecutionSpec, binding.StageKey, secretID)
		}
		index.usedSecrets[secretID] = struct{}{}
	}
	return nil
}

func (index executionReferenceIndex) validateAllUsed() error {
	for id := range index.artifacts {
		if _, used := index.usedArtifacts[id]; !used {
			return fmt.Errorf("%w: artifact reference %q is unused", errInvalidExecutionSpec, id)
		}
	}
	for id := range index.checkouts {
		if _, used := index.usedCheckouts[id]; !used {
			return fmt.Errorf("%w: checkout reference %q is unused", errInvalidExecutionSpec, id)
		}
	}
	for id := range index.runtimes {
		if _, used := index.usedRuntimes[id]; !used {
			return fmt.Errorf("%w: runtime reference %q is unused", errInvalidExecutionSpec, id)
		}
	}
	for id := range index.providers {
		if _, used := index.usedProviders[id]; !used {
			return fmt.Errorf("%w: provider reference %q is unused", errInvalidExecutionSpec, id)
		}
	}
	for id := range index.secrets {
		if _, used := index.usedSecrets[id]; !used {
			return fmt.Errorf("%w: secret reference %q is unused", errInvalidExecutionSpec, id)
		}
	}
	return nil
}

func (spec *RunExecutionSpec) normalize() {
	sort.Slice(spec.References.Artifacts, func(left, right int) bool {
		return spec.References.Artifacts[left].ID < spec.References.Artifacts[right].ID
	})
	sort.Slice(spec.References.Checkouts, func(left, right int) bool {
		return spec.References.Checkouts[left].ID < spec.References.Checkouts[right].ID
	})
	sort.Slice(spec.References.Runtimes, func(left, right int) bool {
		return spec.References.Runtimes[left].ID < spec.References.Runtimes[right].ID
	})
	sort.Slice(spec.References.Providers, func(left, right int) bool {
		return spec.References.Providers[left].ID < spec.References.Providers[right].ID
	})
	sort.Slice(spec.References.Secrets, func(left, right int) bool {
		return spec.References.Secrets[left].ID < spec.References.Secrets[right].ID
	})
	for index, binding := range spec.Stages {
		base, ok := stageBindingBaseOf(binding)
		if !ok {
			continue
		}
		sort.Slice(base.ArtifactInputs, func(left, right int) bool {
			if base.ArtifactInputs[left].Port != base.ArtifactInputs[right].Port {
				return base.ArtifactInputs[left].Port < base.ArtifactInputs[right].Port
			}
			return base.ArtifactInputs[left].ArtifactID < base.ArtifactInputs[right].ArtifactID
		})
		sort.Strings(base.SecretIDs)
		spec.Stages[index] = replaceStageBindingBase(binding, base)
	}
	sort.Slice(spec.Stages, func(left, right int) bool {
		leftBase, _ := stageBindingBaseOf(spec.Stages[left])
		rightBase, _ := stageBindingBaseOf(spec.Stages[right])
		return leftBase.StageKey < rightBase.StageKey
	})
}

func replaceStageBindingBase(binding StageExecutionBinding, base StageBindingBase) StageExecutionBinding {
	switch binding.(type) {
	case RepoPrepareBinding:
		return RepoPrepareBinding{StageBindingBase: base}
	case RepoAnalyzeBinding:
		return RepoAnalyzeBinding{StageBindingBase: base}
	case TaskDesignBinding:
		return TaskDesignBinding{StageBindingBase: base}
	case TaskReviewBinding:
		return TaskReviewBinding{StageBindingBase: base}
	case GenerateTaskFilesBinding:
		return GenerateTaskFilesBinding{StageBindingBase: base}
	case InstructionGenBinding:
		return InstructionGenBinding{StageBindingBase: base}
	case TaskTOMLGenBinding:
		return TaskTOMLGenBinding{StageBindingBase: base}
	case DockerfileGenBinding:
		return DockerfileGenBinding{StageBindingBase: base}
	case ContentReviewBinding:
		return ContentReviewBinding{StageBindingBase: base}
	case SolveGenBinding:
		return SolveGenBinding{StageBindingBase: base}
	case TestGenBinding:
		return TestGenBinding{StageBindingBase: base}
	case TestsAnalysisBinding:
		return TestsAnalysisBinding{StageBindingBase: base}
	case SolutionReviewBinding:
		return SolutionReviewBinding{StageBindingBase: base}
	case MaterializeTaskBinding:
		return MaterializeTaskBinding{StageBindingBase: base}
	case TaskRepairBinding:
		return TaskRepairBinding{StageBindingBase: base}
	case RuntimeSelfCheckBinding:
		return RuntimeSelfCheckBinding{StageBindingBase: base}
	case HarborVerifyBinding:
		return HarborVerifyBinding{StageBindingBase: base}
	case DockerBuildBinding:
		return DockerBuildBinding{StageBindingBase: base}
	case InitialVerifyBinding:
		return InitialVerifyBinding{StageBindingBase: base}
	case OracleVerifyBinding:
		return OracleVerifyBinding{StageBindingBase: base}
	case CodeEdgeLintBinding:
		return CodeEdgeLintBinding{StageBindingBase: base}
	case QualityCheckBinding:
		return QualityCheckBinding{StageBindingBase: base}
	case SimilarityCheckBinding:
		return SimilarityCheckBinding{StageBindingBase: base}
	case FinalReviewBinding:
		return FinalReviewBinding{StageBindingBase: base}
	case HarborRunQwenBinding:
		return HarborRunQwenBinding{StageBindingBase: base}
	case HarborRunOpusBinding:
		return HarborRunOpusBinding{StageBindingBase: base}
	case ResultReviewBinding:
		return ResultReviewBinding{StageBindingBase: base}
	case SubmissionLintBinding:
		return SubmissionLintBinding{StageBindingBase: base}
	case PackageBinding:
		return PackageBinding{StageBindingBase: base}
	default:
		return binding
	}
}

func decodeExecutionSpecJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return fmt.Errorf("decode trailing JSON value: %w", err)
	}
	return nil
}

func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := walkJSONValue(decoder, "$"); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return fmt.Errorf("decode trailing JSON value: %w", err)
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder, location string) error {
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
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key at %s is not a string", location)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object key %q at %s", key, location)
			}
			seen[key] = struct{}{}
			if err := walkJSONValue(decoder, location+"."+key); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("object at %s is not closed", location)
		}
	case '[':
		index := 0
		for decoder.More() {
			if err := walkJSONValue(decoder, fmt.Sprintf("%s[%d]", location, index)); err != nil {
				return err
			}
			index++
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("array at %s is not closed", location)
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q at %s", delimiter, location)
	}
	return nil
}

func validateExecutionSpecString(label, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s is required", errInvalidExecutionSpec, label)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%w: %s contains a control character", errInvalidExecutionSpec, label)
		}
	}
	return nil
}

// validatePersistentUUIDv7 keeps the public document aligned with the global
// V2 identity namespace. Human-readable checkout/runtime/secret handles are
// intentionally validated elsewhere as non-persistent registry keys, but
// Task, TaskRevision, and ArtifactRef IDs must be canonical UUIDv7 values.
func validatePersistentUUIDv7(label, value string) error {
	if err := validateExecutionSpecString(label, value); err != nil {
		return err
	}
	parsed, err := uuid.Parse(value)
	if err != nil || parsed.Version() != uuid.Version(7) || parsed.String() != value {
		return fmt.Errorf("%w: %s must be a canonical UUIDv7", errInvalidExecutionSpec, label)
	}
	return nil
}
