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
	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// RunExecutionSpecFormat identifies the strict, persisted request contract
	// accepted by the Harbor V2 Run entrypoint.
	RunExecutionSpecFormat = "harbor.run-execution-spec.v1"
	// RunExecutionSpecVersion is deliberately separate from Format so an
	// incompatible document revision cannot be accepted by accident.
	RunExecutionSpecVersion = "4"
)

var errInvalidExecutionSpec = errors.New("harbor workflow adapter: invalid run execution spec")

// RunSelectionKind names the immutable subject family bound to one execution.
// The workflow kernel deliberately sees only SubjectBinding; this Harbor
// adapter records the domain identity needed to resolve that binding without
// pretending that an unpublished source-authoring session is already a
// TaskRevision.
type RunSelectionKind string

const (
	// RunSelectionTaskRevision is the normal lifecycle subject: one sealed
	// Harbor TaskRevision.
	RunSelectionTaskRevision RunSelectionKind = "task_revision"
	// RunSelectionAuthoringSession is the pre-materialization Standard subject:
	// one immutable source snapshot plus one durable authoring session. It is
	// intentionally distinct from a TaskRevision; materialize_task is the only
	// operation allowed to create the latter.
	RunSelectionAuthoringSession RunSelectionKind = "authoring_session"
)

// RunSelectionReference freezes the subject selected for execution. It uses
// durable IDs and a subject digest, never a mutable workspace path. The
// task-revision fields and authoring-session fields are a closed union; callers
// must never populate both families in one specification.
type RunSelectionReference struct {
	Kind                  RunSelectionKind          `json:"kind"`
	TaskID                string                    `json:"task_id"`
	RevisionID            string                    `json:"revision_id"`
	RevisionDigest        workflowkit.SubjectDigest `json:"revision_digest"`
	AuthoringSourceID     string                    `json:"authoring_source_id"`
	AuthoringSessionID    string                    `json:"authoring_session_id"`
	AuthoringSourceDigest workflowkit.SubjectDigest `json:"authoring_source_digest"`
}

func (selection RunSelectionReference) validate() error {
	kind, err := selection.resolvedKind()
	if err != nil {
		return err
	}
	switch kind {
	case RunSelectionTaskRevision:
		if strings.TrimSpace(selection.AuthoringSourceID) != "" || strings.TrimSpace(selection.AuthoringSessionID) != "" || selection.AuthoringSourceDigest != "" {
			return fmt.Errorf("%w: task-revision selection cannot contain authoring-session fields", errInvalidExecutionSpec)
		}
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
	case RunSelectionAuthoringSession:
		if strings.TrimSpace(selection.TaskID) != "" || strings.TrimSpace(selection.RevisionID) != "" || selection.RevisionDigest != "" {
			return fmt.Errorf("%w: authoring-session selection cannot contain task-revision fields", errInvalidExecutionSpec)
		}
		if err := validatePersistentUUIDv7("selection authoring source id", selection.AuthoringSourceID); err != nil {
			return err
		}
		if err := validatePersistentUUIDv7("selection authoring session id", selection.AuthoringSessionID); err != nil {
			return err
		}
		if err := selection.AuthoringSourceDigest.Validate(); err != nil {
			return fmt.Errorf("%w: selection authoring source digest: %v", errInvalidExecutionSpec, err)
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported selection kind %q", errInvalidExecutionSpec, kind)
	}
}

// resolvedKind retains strict union validation while accepting source-built
// in-memory task-revision specifications that predate the explicit kind field.
// CanonicalJSON always writes the resolved discriminator, so managed V2 Run
// inputs never preserve the implicit form.
func (selection RunSelectionReference) resolvedKind() (RunSelectionKind, error) {
	kind := selection.Kind
	if kind == "" {
		hasTask := strings.TrimSpace(selection.TaskID) != "" || strings.TrimSpace(selection.RevisionID) != "" || selection.RevisionDigest != ""
		hasAuthoring := strings.TrimSpace(selection.AuthoringSourceID) != "" || strings.TrimSpace(selection.AuthoringSessionID) != "" || selection.AuthoringSourceDigest != ""
		switch {
		case hasTask && !hasAuthoring:
			return RunSelectionTaskRevision, nil
		case hasAuthoring && !hasTask:
			return RunSelectionAuthoringSession, nil
		default:
			return "", fmt.Errorf("%w: selection kind is required for an empty or mixed subject union", errInvalidExecutionSpec)
		}
	}
	if kind != RunSelectionTaskRevision && kind != RunSelectionAuthoringSession {
		return "", fmt.Errorf("%w: unsupported selection kind %q", errInvalidExecutionSpec, kind)
	}
	return kind, nil
}

// Canonical returns the same selection with its inferred kind materialized.
func (selection RunSelectionReference) Canonical() (RunSelectionReference, error) {
	if err := selection.validate(); err != nil {
		return RunSelectionReference{}, err
	}
	kind, _ := selection.resolvedKind()
	selection.Kind = kind
	return selection, nil
}

// SubjectBinding projects this closed Harbor selection onto workflowkit's
// domain-neutral immutable subject contract.
func (selection RunSelectionReference) SubjectBinding() (workflowkit.SubjectBinding, error) {
	canonical, err := selection.Canonical()
	if err != nil {
		return workflowkit.SubjectBinding{}, err
	}
	switch canonical.Kind {
	case RunSelectionTaskRevision:
		return workflowkit.SubjectBinding{SubjectID: canonical.TaskID, RevisionID: canonical.RevisionID, Digest: canonical.RevisionDigest}, nil
	case RunSelectionAuthoringSession:
		return workflowkit.SubjectBinding{SubjectID: canonical.AuthoringSourceID, RevisionID: canonical.AuthoringSessionID, Digest: canonical.AuthoringSourceDigest}, nil
	default:
		return workflowkit.SubjectBinding{}, fmt.Errorf("%w: unsupported selection kind %q", errInvalidExecutionSpec, canonical.Kind)
	}
}

// SubjectRevisionID returns the opaque revision/session identity that a
// controlled checkout must bind. It is not necessarily a TaskRevision ID.
func (selection RunSelectionReference) SubjectRevisionID() (string, error) {
	binding, err := selection.SubjectBinding()
	if err != nil {
		return "", err
	}
	return binding.RevisionID, nil
}

// SubjectDigest returns the opaque immutable source digest used by a
// controlled checkout and the generic workflow engine.
func (selection RunSelectionReference) SubjectDigest() (workflowkit.SubjectDigest, error) {
	binding, err := selection.SubjectBinding()
	if err != nil {
		return "", err
	}
	return binding.Digest, nil
}

// IsTaskRevision reports whether this selection names a sealed task revision.
func (selection RunSelectionReference) IsTaskRevision() bool {
	kind, err := selection.resolvedKind()
	return err == nil && kind == RunSelectionTaskRevision
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
	subjectRevisionID, err := selection.SubjectRevisionID()
	if err != nil {
		return err
	}
	if reference.RevisionID != subjectRevisionID {
		return fmt.Errorf("%w: checkout %q revision id does not match selected subject revision", errInvalidExecutionSpec, reference.ID)
	}
	subjectDigest, err := selection.SubjectDigest()
	if err != nil {
		return err
	}
	if reference.RevisionDigest != subjectDigest {
		return fmt.Errorf("%w: checkout %q revision digest does not match selected subject digest", errInvalidExecutionSpec, reference.ID)
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
	StageBindingRepoPrepare              StageBindingType = "repo_prepare"
	StageBindingRepoAnalyze              StageBindingType = "repo_analyze"
	StageBindingTaskDesign               StageBindingType = "task_design"
	StageBindingTaskReview               StageBindingType = "task_review"
	StageBindingGenerateTaskFiles        StageBindingType = "generate_task_files"
	StageBindingInstructionGen           StageBindingType = "instruction_generate"
	StageBindingTaskTOMLGen              StageBindingType = "task_toml_generate"
	StageBindingDockerfileGen            StageBindingType = "dockerfile_generate"
	StageBindingDockerfileBuildValidate  StageBindingType = "dockerfile_build_validate"
	StageBindingContentReview            StageBindingType = "content_review"
	StageBindingSolveGen                 StageBindingType = "solve_generate"
	StageBindingTestGen                  StageBindingType = "test_generate"
	StageBindingAuthoringHarness         StageBindingType = "authoring_harness"
	StageBindingTestsAnalysis            StageBindingType = "tests_analysis"
	StageBindingCodeEdgePackageAdmission StageBindingType = "codeedge_package_admission"
	StageBindingSolutionReview           StageBindingType = "solution_review"
	StageBindingMaterializeTask          StageBindingType = "materialize_task"
	StageBindingTaskRepair               StageBindingType = "task_repair"
	StageBindingRuntimeSelfCheck         StageBindingType = "runtime_self_check"
	StageBindingHarborVerify             StageBindingType = "harbor_verify"
	StageBindingDockerBuild              StageBindingType = "docker_build"
	StageBindingInitialVerify            StageBindingType = "initial_verify"
	StageBindingOracleVerify             StageBindingType = "oracle_verify"
	StageBindingCodeEdgeLint             StageBindingType = "codeedge_lint"
	StageBindingQualityCheck             StageBindingType = "quality_check"
	StageBindingSimilarityCheck          StageBindingType = "similarity_check"
	StageBindingFinalReview              StageBindingType = "final_review"
	StageBindingHarborRunQwen            StageBindingType = "harbor_run_qwen"
	StageBindingHarborRunOpus            StageBindingType = "harbor_run_opus"
	StageBindingEvaluatorEvidenceHandoff StageBindingType = "evaluator_evidence_handoff"
	StageBindingResultReview             StageBindingType = "result_review"
	StageBindingSubmissionLint           StageBindingType = "submission_lint"
	StageBindingPackage                  StageBindingType = "package"
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

// StageExecutionBinding is the sealed interface for all stage binding types.
// UniversalStageBinding is the sole implementation.
type StageExecutionBinding interface {
	stageExecutionBinding()
}

// UniversalStageBinding is the single stage binding type. All 30 stage kinds use the
// same struct; differentiation is by the Type and StageKey fields on StageBindingBase.
// This replaces the previous 30 identical concrete types while preserving the JSON
// wire format through the existing StageBindingType discriminator.
type UniversalStageBinding struct{ StageBindingBase }

func (UniversalStageBinding) stageExecutionBinding() {}

// RunExecutionSpec is the V2-only typed execution selection, reference set,
// and complete per-stage binding union. It is intentionally independent from
// ExecutionProfile: profile carries budget policy while this document carries
// the immutable runtime inputs selected for one Run.
type RunExecutionSpec struct {
	Format                        string                          `json:"format"`
	Version                       string                          `json:"version"`
	Template                      TemplateReference               `json:"template"`
	Selection                     RunSelectionReference           `json:"selection"`
	References                    ExecutionReferenceSet           `json:"references"`
	Stages                        []StageExecutionBinding         `json:"stages"`
	CodeEdgeFinalCompliancePolicy *codeedge.FinalCompliancePolicy `json:"codeedge_final_compliance_policy,omitempty"`
}

// StageOperationResolution is the complete immutable selection that a
// controlled Harbor provider must validate before it obtains an executable
// operation. It exposes stable references only; provider implementations own
// their own controlled checkout/runtime/secret resolution and never receive
// ambient paths or secret values from this document.
type StageOperationResolution struct {
	// Template is copied from the enclosing frozen RunExecutionSpec.  A
	// multi-template deployment resolver uses this exact identity to select its
	// one catalog/lock/provider bundle; it is never inferred from a stage key,
	// provider ID, or operation name.  The field is derived rather than
	// serialized because a StageOperationResolution is an execution-time view
	// of the already-canonical specification.
	Template       TemplateReference
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
	if spec.CodeEdgeFinalCompliancePolicy != nil {
		policy := spec.CodeEdgeFinalCompliancePolicy.Clone()
		spec.CodeEdgeFinalCompliancePolicy = &policy
	}
	stages := spec.Stages
	spec.Stages = make([]StageExecutionBinding, len(stages))
	for index, binding := range stages {
		spec.Stages[index] = cloneStageExecutionBinding(binding)
	}
	return spec
}

// BindManagedArtifactInput binds one intrinsic catalog input port to a
// Harbor-managed immutable artifact. It is deliberately limited to ports with
// no workflow producer: stage-produced lineage continues to be selected from
// completed StageAttempts and cannot be overwritten by Run admission.
//
// Existing caller-provided bindings for the port are replaced and references
// made unused by that replacement are removed before the returned spec is
// revalidated. This lets StartRun replace a provisional input reference without
// retaining an unreachable or fake artifact in the final frozen contract.
func (spec RunExecutionSpec) BindManagedArtifactInput(port string, artifact ArtifactReference) (RunExecutionSpec, error) {
	if err := validateExecutionSpecString("managed artifact input port", port); err != nil {
		return RunExecutionSpec{}, err
	}
	if err := artifact.validate(); err != nil {
		return RunExecutionSpec{}, err
	}
	template, err := ResolveWorkflowTemplate(spec.Template)
	if err != nil {
		return RunExecutionSpec{}, fmt.Errorf("%w: managed artifact input template: %v", errInvalidExecutionSpec, err)
	}
	consumers := make(map[workflowkit.StageKey]struct{})
	for _, definition := range template.Catalog.Stages {
		for _, output := range definition.Outputs {
			if output.Name == port {
				return RunExecutionSpec{}, fmt.Errorf("%w: managed artifact input port %q has workflow producer %q", errInvalidExecutionSpec, port, definition.Key)
			}
		}
		for _, input := range definition.Inputs {
			if input.Name != port {
				continue
			}
			if input.SchemaVersion != artifact.SchemaVersion {
				return RunExecutionSpec{}, fmt.Errorf("%w: managed artifact input port %q schema %q, want %q", errInvalidExecutionSpec, port, artifact.SchemaVersion, input.SchemaVersion)
			}
			consumers[definition.Key] = struct{}{}
		}
	}
	if len(consumers) == 0 {
		return RunExecutionSpec{}, fmt.Errorf("%w: managed artifact input port %q is not declared by the frozen template", errInvalidExecutionSpec, port)
	}

	bound := spec.Clone()
	for index, binding := range bound.Stages {
		base, ok := stageBindingBaseOf(binding)
		if !ok {
			return RunExecutionSpec{}, fmt.Errorf("%w: unsupported concrete stage binding %T", errInvalidExecutionSpec, binding)
		}
		if _, consumes := consumers[base.StageKey]; !consumes {
			continue
		}
		inputs := make([]ArtifactInputReference, 0, len(base.ArtifactInputs)+1)
		for _, input := range base.ArtifactInputs {
			if input.Port != port {
				inputs = append(inputs, input)
			}
		}
		inputs = append(inputs, ArtifactInputReference{Port: port, ArtifactID: artifact.ID})
		base.ArtifactInputs = inputs
		bound.Stages[index] = replaceStageBindingBase(binding, base)
	}

	// Keep exactly the references used by the final bindings. This preserves
	// the closed spec's no-unreachable-reference invariant while removing any
	// provisional artifact that StartRun just superseded.
	available := make(map[workflowkit.ArtifactID]ArtifactReference, len(bound.References.Artifacts)+1)
	for _, reference := range bound.References.Artifacts {
		available[reference.ID] = reference
	}
	available[artifact.ID] = artifact
	used := make(map[workflowkit.ArtifactID]struct{})
	for _, binding := range bound.Stages {
		base, _ := stageBindingBaseOf(binding)
		for _, input := range base.ArtifactInputs {
			used[input.ArtifactID] = struct{}{}
		}
	}
	bound.References.Artifacts = make([]ArtifactReference, 0, len(used))
	for id := range used {
		reference, present := available[id]
		if !present {
			return RunExecutionSpec{}, fmt.Errorf("%w: stage binding references unknown artifact %q", errInvalidExecutionSpec, id)
		}
		bound.References.Artifacts = append(bound.References.Artifacts, reference)
	}
	if err := bound.Validate(); err != nil {
		return RunExecutionSpec{}, err
	}
	return bound, nil
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
		Template: spec.Template, StageKey: key, StageType: stageType, Plugin: base.Plugin, Provider: provider,
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
	if err := spec.validateTemplateExtension(); err != nil {
		return err
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
	if err := spec.validateCodeEdgeEvaluatorChildBindings(); err != nil {
		return err
	}
	if err := index.validateAllUsed(); err != nil {
		return err
	}
	return nil
}

// validateTemplateExtension keeps deployment-specific policy out of generic
// executions while requiring CodeEdge Phase-1 to freeze every final-compliance
// decision input with its Run.
func (spec RunExecutionSpec) validateTemplateExtension() error {
	selectionKind, err := spec.Selection.resolvedKind()
	if err != nil {
		return err
	}
	if IsStandardAuthoringWorkflowTemplate(spec.Template) {
		if selectionKind != RunSelectionAuthoringSession {
			return fmt.Errorf("%w: Standard authoring execution specification requires an authoring-session selection", errInvalidExecutionSpec)
		}
	} else if selectionKind == RunSelectionAuthoringSession {
		return fmt.Errorf("%w: authoring-session selection is only accepted by Standard authoring template versions registered in this binary", errInvalidExecutionSpec)
	}
	if spec.Template.Equal(CodeEdgePhase1TemplateReference()) {
		if spec.CodeEdgeFinalCompliancePolicy == nil {
			return fmt.Errorf("%w: CodeEdge Phase-1 execution specification requires a final compliance policy", errInvalidExecutionSpec)
		}
		if err := spec.CodeEdgeFinalCompliancePolicy.Validate(); err != nil {
			return fmt.Errorf("%w: CodeEdge Phase-1 final compliance policy: %v", errInvalidExecutionSpec, err)
		}
		return nil
	}
	if spec.CodeEdgeFinalCompliancePolicy != nil {
		return fmt.Errorf("%w: CodeEdge Phase-1 final compliance policy is not accepted by template %s@%s", errInvalidExecutionSpec, spec.Template.ID, spec.Template.Version)
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
	policy, err := parseCodeEdgeFinalCompliancePolicy(document.CodeEdgeFinalCompliancePolicy)
	if err != nil {
		return RunExecutionSpec{}, fmt.Errorf("decode CodeEdge final compliance policy: %w", err)
	}
	spec.CodeEdgeFinalCompliancePolicy = policy
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
	Format                        string                `json:"format"`
	Version                       string                `json:"version"`
	Template                      TemplateReference     `json:"template"`
	Selection                     RunSelectionReference `json:"selection"`
	References                    ExecutionReferenceSet `json:"references"`
	Stages                        []json.RawMessage     `json:"stages"`
	CodeEdgeFinalCompliancePolicy json.RawMessage       `json:"codeedge_final_compliance_policy"`
}

func parseCodeEdgeFinalCompliancePolicy(raw json.RawMessage) (*codeedge.FinalCompliancePolicy, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errors.New("final compliance policy must be an object, not null")
	}
	var policy codeedge.FinalCompliancePolicy
	if err := decodeExecutionSpecJSON(raw, &policy); err != nil {
		return nil, err
	}
	return &policy, nil
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
	if !isKnownStageBindingType(discriminator.Type) {
		return nil, fmt.Errorf("%w: unsupported stage binding type %q", errInvalidExecutionSpec, discriminator.Type)
	}
	return decode(&UniversalStageBinding{})
}

func dereferenceStageBinding(binding StageExecutionBinding) StageExecutionBinding {
	if typed, ok := binding.(*UniversalStageBinding); ok {
		return *typed
	}
	return binding
}

var knownStageBindingTypes = map[StageBindingType]bool{
	StageBindingRepoPrepare:              true,
	StageBindingRepoAnalyze:              true,
	StageBindingTaskDesign:               true,
	StageBindingTaskReview:               true,
	StageBindingGenerateTaskFiles:        true,
	StageBindingInstructionGen:           true,
	StageBindingTaskTOMLGen:              true,
	StageBindingDockerfileGen:            true,
	StageBindingDockerfileBuildValidate:  true,
	StageBindingContentReview:            true,
	StageBindingSolveGen:                 true,
	StageBindingTestGen:                  true,
	StageBindingAuthoringHarness:         true,
	StageBindingTestsAnalysis:            true,
	StageBindingCodeEdgePackageAdmission: true,
	StageBindingSolutionReview:           true,
	StageBindingMaterializeTask:          true,
	StageBindingTaskRepair:               true,
	StageBindingRuntimeSelfCheck:         true,
	StageBindingHarborVerify:             true,
	StageBindingDockerBuild:              true,
	StageBindingInitialVerify:            true,
	StageBindingOracleVerify:             true,
	StageBindingCodeEdgeLint:             true,
	StageBindingQualityCheck:             true,
	StageBindingSimilarityCheck:          true,
	StageBindingFinalReview:              true,
	StageBindingHarborRunQwen:            true,
	StageBindingHarborRunOpus:            true,
	StageBindingEvaluatorEvidenceHandoff: true,
	StageBindingResultReview:             true,
	StageBindingSubmissionLint:           true,
	StageBindingPackage:                  true,
}

func isKnownStageBindingType(typ StageBindingType) bool {
	return knownStageBindingTypes[typ]
}

func stageBindingBaseOf(binding StageExecutionBinding) (StageBindingBase, bool) {
	if typed, ok := binding.(UniversalStageBinding); ok {
		return typed.StageBindingBase, true
	}
	return StageBindingBase{}, false
}

func stageBindingIdentity(binding StageExecutionBinding) (workflowkit.StageKey, StageBindingType, bool) {
	if typed, ok := binding.(UniversalStageBinding); ok {
		return typed.StageKey, typed.Type, true
	}
	return "", "", false
}

func cloneStageExecutionBinding(binding StageExecutionBinding) StageExecutionBinding {
	base, ok := stageBindingBaseOf(binding)
	if !ok {
		return binding
	}
	return UniversalStageBinding{StageBindingBase: base.Clone()}
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
	selection, err := spec.Selection.Canonical()
	if err == nil {
		spec.Selection = selection
	}
	if spec.CodeEdgeFinalCompliancePolicy != nil {
		policy := spec.CodeEdgeFinalCompliancePolicy.Clone()
		sort.Strings(policy.QwenPolicy.InfraExceptionTypes)
		sort.Strings(policy.OpusPolicy.InfraExceptionTypes)
		spec.CodeEdgeFinalCompliancePolicy = &policy
	}
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
	if _, ok := binding.(UniversalStageBinding); ok {
		return UniversalStageBinding{StageBindingBase: base}
	}
	return binding
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
