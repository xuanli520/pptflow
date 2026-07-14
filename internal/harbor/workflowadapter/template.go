package workflowadapter

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// WorkflowTemplate is Harbor's code-versioned source of workflow policy. It
// contains no request-specific paths, models, or budgets; those are supplied
// through typed application requests and ExecutionProfile respectively.
type WorkflowTemplate struct {
	ID          string       `json:"id"`
	Version     string       `json:"version"`
	Catalog     StageCatalog `json:"catalog"`
	QuotaPolicy QuotaPolicy  `json:"quota_policy"`
}

// Reference returns the exact immutable identity of this template.
func (template WorkflowTemplate) Reference() TemplateReference {
	return TemplateReference{ID: template.ID, Version: template.Version}
}

// Clone returns an independent template snapshot.
func (template WorkflowTemplate) Clone() WorkflowTemplate {
	template.Catalog = template.Catalog.Clone()
	template.QuotaPolicy = template.QuotaPolicy.Clone()
	return template
}

// StandardWorkflowTemplate returns the current code-versioned Harbor V2
// template. Callers must still provide an explicit ExecutionProfile to compile
// it; this function intentionally provides no production budget default.
func StandardWorkflowTemplate() WorkflowTemplate {
	return WorkflowTemplate{
		ID:          StandardWorkflowTemplateID,
		Version:     StandardWorkflowTemplateVersion,
		Catalog:     StandardStageCatalog(),
		QuotaPolicy: StandardQuotaPolicy(),
	}
}

// Validate proves that the template has a complete valid Harbor catalog.
func (template WorkflowTemplate) Validate() error {
	reference := template.Reference()
	if err := reference.Validate(); err != nil {
		return err
	}
	if !template.Catalog.Template.Equal(reference) {
		return fmt.Errorf("%w: %w: workflow template %s@%s cannot compile catalog bound to %s@%s", errInvalidCatalog, errTemplateMismatch, reference.ID, reference.Version, template.Catalog.Template.ID, template.Catalog.Template.Version)
	}
	if err := template.Catalog.Validate(); err != nil {
		return err
	}
	return template.QuotaPolicy.ValidateFor(template.Catalog)
}

// Fingerprint returns the stable identity that must be frozen into every run
// manifest. Any template or catalog policy change changes this value.
func (template WorkflowTemplate) Fingerprint() (workflowkit.Fingerprint, error) {
	if err := template.Validate(); err != nil {
		return "", err
	}
	catalogFingerprint, err := template.Catalog.Fingerprint()
	if err != nil {
		return "", err
	}
	quotaPolicyFingerprint, err := template.QuotaPolicy.Fingerprint()
	if err != nil {
		return "", err
	}
	return workflowkit.FingerprintParts("harbor.workflowadapter.workflow-template.v2", []workflowkit.FingerprintPart{
		{Name: "catalog_fingerprint", Value: []byte(catalogFingerprint)},
		{Name: "id", Value: []byte(template.ID)},
		{Name: "quota_policy_fingerprint", Value: []byte(quotaPolicyFingerprint)},
		{Name: "version", Value: []byte(template.Version)},
	})
}

// StageBudget binds one fully resolved execution budget to one Harbor stage.
// There is deliberately no map or default fallback: a persisted profile lists
// every stage exactly once.
type StageBudget struct {
	StageKey workflowkit.StageKey        `json:"stage_key"`
	Budget   workflowkit.ExecutionBudget `json:"budget"`
}

// CandidateProviderBudget freezes the timeout and lease policy for a
// content-revision provider. It is intentionally independent of any workflow
// stage name: templates that have no task_repair stage can still create a
// fenced candidate through the same lifecycle protocol.
type CandidateProviderBudget struct {
	AttemptTimeout time.Duration `json:"attempt_timeout"`
	StartupGrace   time.Duration `json:"startup_grace"`
	ShutdownGrace  time.Duration `json:"shutdown_grace"`
}

func (budget CandidateProviderBudget) Validate() error {
	if budget.AttemptTimeout <= 0 {
		return fmt.Errorf("candidate provider attempt timeout must be positive")
	}
	if budget.StartupGrace < 0 || budget.ShutdownGrace < 0 {
		return fmt.Errorf("candidate provider grace periods cannot be negative")
	}
	if budget.AttemptTimeout <= budget.StartupGrace+budget.ShutdownGrace {
		return fmt.Errorf("candidate provider attempt timeout must exceed startup and shutdown grace")
	}
	return nil
}

// ExecutionTimeout is the only provider execution window. Startup and
// shutdown grace remain inside the frozen attempt boundary rather than
// silently extending a provider's write authority.
func (budget CandidateProviderBudget) ExecutionTimeout() (time.Duration, error) {
	if err := budget.Validate(); err != nil {
		return 0, err
	}
	return budget.AttemptTimeout - budget.StartupGrace - budget.ShutdownGrace, nil
}

// LeaseTTL is bounded by the whole frozen attempt. Candidate lease renewal
// may keep a fence current during execution, but may never create a longer
// authority window than the declared attempt.
func (budget CandidateProviderBudget) LeaseTTL() (time.Duration, error) {
	if err := budget.Validate(); err != nil {
		return 0, err
	}
	return budget.AttemptTimeout, nil
}

// RequiredContinuationPlanTTL is the confirmed lifetime of a frozen
// continuation plan. It is intentionally a versioned ExecutionProfile field
// rather than a service default, so every Run manifest preserves the policy
// used when its plan was created.
const RequiredContinuationPlanTTL = 24 * time.Hour

// Clone returns an independent stage budget.
func (budget StageBudget) Clone() StageBudget {
	budget.Budget = budget.Budget.Clone()
	return budget
}

// ExecutionProfile is a code-versioned, complete execution envelope supplied
// explicitly by every API or CLI request. It intentionally has no implicit
// production instance because the confirmed policy requires full request
// budgets instead of defaults.
type ExecutionProfile struct {
	Template                 TemplateReference      `json:"template"`
	ID                       string                 `json:"id"`
	Version                  string                 `json:"version"`
	ContinuationPlanTTL      time.Duration          `json:"continuation_plan_ttl"`
	ControlGracePeriod       time.Duration          `json:"control_grace_period"`
	CandidateProviderBudget  CandidateProviderBudget `json:"candidate_provider_budget"`
	Stages                   []StageBudget          `json:"stages"`
}

// Clone returns an independent profile snapshot.
func (profile ExecutionProfile) Clone() ExecutionProfile {
	stages := profile.Stages
	profile.Stages = make([]StageBudget, len(stages))
	for index, stage := range stages {
		profile.Stages[index] = stage.Clone()
	}
	return profile
}

// Budget returns a copy of the explicit budget for key.
func (profile ExecutionProfile) Budget(key workflowkit.StageKey) (workflowkit.ExecutionBudget, bool) {
	for _, stage := range profile.Stages {
		if stage.StageKey == key {
			return stage.Budget.Clone(), true
		}
	}
	return workflowkit.ExecutionBudget{}, false
}

// Validate checks local profile shape, every budget hierarchy, and exact
// coverage for the closed template explicitly bound into the profile.
func (profile ExecutionProfile) Validate() error {
	template, err := ResolveWorkflowTemplate(profile.Template)
	if err != nil {
		return fmt.Errorf("%w: execution profile template: %v", errInvalidCatalog, err)
	}
	return profile.ValidateFor(template.Catalog)
}

func (profile ExecutionProfile) validateLocal() error {
	if strings.TrimSpace(profile.ID) == "" {
		return fmt.Errorf("%w: execution profile id is required", errInvalidCatalog)
	}
	if strings.TrimSpace(profile.Version) == "" {
		return fmt.Errorf("%w: execution profile version is required", errInvalidCatalog)
	}
	if profile.ContinuationPlanTTL != RequiredContinuationPlanTTL {
		return fmt.Errorf("%w: continuation plan TTL must be exactly %s", errInvalidCatalog, RequiredContinuationPlanTTL)
	}
	if profile.ControlGracePeriod < 0 {
		return fmt.Errorf("%w: control grace period cannot be negative", errInvalidCatalog)
	}
	if err := profile.CandidateProviderBudget.Validate(); err != nil {
		return fmt.Errorf("%w: candidate provider budget: %v", errInvalidCatalog, err)
	}
	if len(profile.Stages) == 0 {
		return fmt.Errorf("%w: execution profile has no stage budgets", errInvalidCatalog)
	}
	seen := make(map[workflowkit.StageKey]struct{}, len(profile.Stages))
	for _, stage := range profile.Stages {
		if strings.TrimSpace(string(stage.StageKey)) == "" {
			return fmt.Errorf("%w: stage budget key is required", errInvalidCatalog)
		}
		if _, duplicate := seen[stage.StageKey]; duplicate {
			return fmt.Errorf("%w: duplicate stage budget %q", errInvalidCatalog, stage.StageKey)
		}
		if err := stage.Budget.Validate(); err != nil {
			return fmt.Errorf("%w: stage budget %q: %v", errInvalidCatalog, stage.StageKey, err)
		}
		seen[stage.StageKey] = struct{}{}
	}
	return nil
}

// ValidateFor proves profile coverage is exact for one catalog, that the
// profile/candidate catalog share an exact template reference, and that every
// multi-turn Harbor stage receives enough turns in its explicit budget.
func (profile ExecutionProfile) ValidateFor(catalog StageCatalog) error {
	if err := profile.validateLocal(); err != nil {
		return err
	}
	if err := catalog.Validate(); err != nil {
		return err
	}
	if !profile.Template.Equal(catalog.Template) {
		return fmt.Errorf("%w: %w: execution profile template %s@%s does not match catalog template %s@%s", errInvalidCatalog, errTemplateMismatch, profile.Template.ID, profile.Template.Version, catalog.Template.ID, catalog.Template.Version)
	}
	profileStages := make(map[workflowkit.StageKey]StageBudget, len(profile.Stages))
	for _, stage := range profile.Stages {
		profileStages[stage.StageKey] = stage
	}
	for _, definition := range catalog.Stages {
		stage, present := profileStages[definition.Key]
		if !present {
			return fmt.Errorf("%w: execution profile omits Harbor node %q", errInvalidCatalog, definition.Key)
		}
		if stage.Budget.MaxTurns < definition.RequiredTurns {
			return fmt.Errorf("%w: stage %q max turns %d is less than required %d", errInvalidCatalog, definition.Key, stage.Budget.MaxTurns, definition.RequiredTurns)
		}
	}
	for key := range profileStages {
		if _, present := catalog.Stage(key); !present {
			return fmt.Errorf("%w: execution profile contains unknown Harbor node %q", errInvalidCatalog, key)
		}
	}
	return nil
}

// Fingerprint returns a canonical profile hash. Stage declaration order does
// not matter; all fields inside a resolved budget remain significant.
func (profile ExecutionProfile) Fingerprint() (workflowkit.Fingerprint, error) {
	if err := profile.Validate(); err != nil {
		return "", err
	}
	canonical := profile.Clone()
	sort.Slice(canonical.Stages, func(left, right int) bool {
		return canonical.Stages[left].StageKey < canonical.Stages[right].StageKey
	})
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: encode execution profile: %v", errInvalidCatalog, err)
	}
	return workflowkit.FingerprintBytes("harbor.workflowadapter.execution-profile.v2", encoded)
}

// ReviewStage captures the review capability that must be persisted alongside
// its durable StageAttempt. It is derived during compile so a runtime never
// needs to infer review behavior from a node identifier.
type ReviewStage struct {
	StageKey         workflowkit.StageKey     `json:"stage_key"`
	ReviewKind       ReviewKind               `json:"review_kind"`
	DecisionArtifact workflowkit.ArtifactSpec `json:"decision_artifact"`
}

// ResolvedWorkflow is the frozen product of a versioned template and explicit
// profile. All fingerprints are persisted as one run-manifest contract.
type ResolvedWorkflow struct {
	Template                    TemplateReference              `json:"template"`
	TemplateID                  string                         `json:"template_id"`
	TemplateVersion             string                         `json:"template_version"`
	ExecutionProfileID          string                         `json:"execution_profile_id"`
	ExecutionProfileVersion     string                         `json:"execution_profile_version"`
	ContinuationPlanTTL         time.Duration                  `json:"continuation_plan_ttl"`
	ControlGracePeriod          time.Duration                  `json:"control_grace_period"`
	CandidateProviderBudget     CandidateProviderBudget         `json:"candidate_provider_budget"`
	TemplateFingerprint         workflowkit.Fingerprint        `json:"template_fingerprint"`
	ExecutionProfileFingerprint workflowkit.Fingerprint        `json:"execution_profile_fingerprint"`
	DefinitionFingerprint       workflowkit.Fingerprint        `json:"definition_fingerprint"`
	ManifestFingerprint         workflowkit.Fingerprint        `json:"manifest_fingerprint"`
	Descriptor                  workflowkit.WorkflowDescriptor `json:"descriptor"`
	QuotaPolicy                 ResolvedQuotaPolicy            `json:"quota_policy"`
	ReviewStages                []ReviewStage                  `json:"review_stages"`
}

// Clone returns an independent frozen result.
func (resolved ResolvedWorkflow) Clone() ResolvedWorkflow {
	resolved.Descriptor = resolved.Descriptor.Clone()
	resolved.QuotaPolicy = resolved.QuotaPolicy.Clone()
	resolved.ReviewStages = append([]ReviewStage(nil), resolved.ReviewStages...)
	return resolved
}

// ReviewStage returns a review capability record for key.
func (resolved ResolvedWorkflow) ReviewStage(key workflowkit.StageKey) (ReviewStage, bool) {
	for _, review := range resolved.ReviewStages {
		if review.StageKey == key {
			return review, true
		}
	}
	return ReviewStage{}, false
}

// Compile combines the static code-versioned policy with a complete explicit
// profile. No filesystem state, report discovery, or legacy configuration is
// consulted, so the result can be frozen before a run starts.
func (template WorkflowTemplate) Compile(profile ExecutionProfile) (ResolvedWorkflow, error) {
	if err := template.Validate(); err != nil {
		return ResolvedWorkflow{}, err
	}
	if !profile.Template.Equal(template.Reference()) {
		return ResolvedWorkflow{}, fmt.Errorf("%w: %w: execution profile template %s@%s does not match workflow template %s@%s", errInvalidCatalog, errTemplateMismatch, profile.Template.ID, profile.Template.Version, template.ID, template.Version)
	}
	if err := profile.ValidateFor(template.Catalog); err != nil {
		return ResolvedWorkflow{}, err
	}
	templateFingerprint, err := template.Fingerprint()
	if err != nil {
		return ResolvedWorkflow{}, err
	}
	profileFingerprint, err := profile.Fingerprint()
	if err != nil {
		return ResolvedWorkflow{}, err
	}
	quotaPolicy, err := template.QuotaPolicy.ResolveFor(template.Catalog)
	if err != nil {
		return ResolvedWorkflow{}, err
	}
	descriptor := workflowkit.WorkflowDescriptor{
		ID:      template.ID,
		Version: template.Version,
		Stages:  make([]workflowkit.StageDescriptor, 0, len(template.Catalog.Stages)),
	}
	reviews := make([]ReviewStage, 0, len(template.Catalog.Stages))
	for _, definition := range template.Catalog.Stages {
		budget, present := profile.Budget(definition.Key)
		if !present {
			return ResolvedWorkflow{}, fmt.Errorf("%w: profile budget for stage %q disappeared during compile", errInvalidCatalog, definition.Key)
		}
		quotaClaims, present := quotaPolicy.ClaimsFor(definition.Key)
		if !present {
			return ResolvedWorkflow{}, fmt.Errorf("%w: quota policy for stage %q disappeared during compile", errInvalidCatalog, definition.Key)
		}
		descriptor.Stages = append(descriptor.Stages, workflowkit.StageDescriptor{
			Key:          definition.Key,
			Version:      definition.Version,
			Plugin:       workflowkit.PluginBinding{ID: definition.Plugin.ID, Version: definition.Plugin.Version},
			Group:        string(definition.Group),
			Dependencies: append([]workflowkit.StageKey(nil), definition.Dependencies...),
			Inputs:       append([]workflowkit.ArtifactSpec(nil), definition.Inputs...),
			Outputs:      append([]workflowkit.ArtifactSpec(nil), definition.Outputs...),
			ReadSet:      append([]workflowkit.ResourceKey(nil), definition.ReadSet...),
			WriteSet:     append([]workflowkit.ResourceKey(nil), definition.WriteSet...),
			Effect:       definition.Effect,
			Dispatch:     definition.Dispatch,
			Budget:       budget,
			QuotaClaims:  quotaClaims,
			Retry:        definition.Retry.Clone(),
			Verdicts:     definition.Verdicts.Clone(),
			Reuse:        definition.Reuse,
			Capabilities: definition.Capabilities.Clone(),
		})
		if definition.Gate != nil {
			reviews = append(reviews, ReviewStage{StageKey: definition.Key, ReviewKind: definition.Gate.ReviewKind, DecisionArtifact: definition.Gate.DecisionArtifact})
		}
	}
	if err := descriptor.Validate(); err != nil {
		return ResolvedWorkflow{}, fmt.Errorf("%w: compiled workflow descriptor: %v", errInvalidCatalog, err)
	}
	if err := quotaPolicy.ValidateForDescriptor(descriptor); err != nil {
		return ResolvedWorkflow{}, fmt.Errorf("%w: compiled quota policy: %v", errInvalidCatalog, err)
	}
	definitionFingerprint, err := descriptor.Fingerprint()
	if err != nil {
		return ResolvedWorkflow{}, err
	}
	manifestFingerprint, err := workflowkit.FingerprintParts("harbor.workflowadapter.resolved-workflow.v2", []workflowkit.FingerprintPart{
		{Name: "definition", Value: []byte(definitionFingerprint)},
		{Name: "execution_profile", Value: []byte(profileFingerprint)},
		{Name: "quota_policy", Value: []byte(quotaPolicy.Fingerprint)},
		{Name: "template", Value: []byte(templateFingerprint)},
	})
	if err != nil {
		return ResolvedWorkflow{}, err
	}
	return ResolvedWorkflow{
		Template:                    template.Reference(),
		TemplateID:                  template.ID,
		TemplateVersion:             template.Version,
		ExecutionProfileID:          profile.ID,
		ExecutionProfileVersion:     profile.Version,
		ContinuationPlanTTL:         profile.ContinuationPlanTTL,
		ControlGracePeriod:          profile.ControlGracePeriod,
		CandidateProviderBudget:     profile.CandidateProviderBudget,
		TemplateFingerprint:         templateFingerprint,
		ExecutionProfileFingerprint: profileFingerprint,
		DefinitionFingerprint:       definitionFingerprint,
		ManifestFingerprint:         manifestFingerprint,
		Descriptor:                  descriptor,
		QuotaPolicy:                 quotaPolicy,
		ReviewStages:                reviews,
	}, nil
}
