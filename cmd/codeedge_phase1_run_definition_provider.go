package cmd

import (
	"context"
	"fmt"
	"sort"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// codeEdgePhase1RunDefinitionProvider is the only production source for the
// parent definition started after a Standard authoring materialization. Its
// profile, final-compliance policy, operations, runtimes, checkouts, providers
// and secret references are copied from one already-verified parent lock at
// composition time. The handoff request contributes only the sealed
// task-revision identity; it cannot select a deployment resource or mutate a
// parent policy.
type codeEdgePhase1RunDefinitionProvider struct {
	profile workflowadapter.ExecutionProfile
	policy  codeedge.FinalCompliancePolicy
	records map[workflowkit.StageKey]stageprovider.DeploymentOperationCatalogLockRecord
}

// newCodeEdgePhase1RunDefinitionProvider accepts a resolver, rather than raw
// catalog/lock bytes, so construction is impossible until their exact
// inventory and receipt binding have been verified. There is deliberately no
// constructor that accepts a caller profile, evaluator child profile, or
// unbound policy.
func newCodeEdgePhase1RunDefinitionProvider(verifier *stageprovider.DeploymentOperationCatalogLockResolver) (*codeEdgePhase1RunDefinitionProvider, error) {
	if verifier == nil {
		return nil, app.ErrCodeEdgePhase1DefinitionUnavailable
	}
	if !verifier.CatalogReceipt().Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		return nil, fmt.Errorf("CodeEdge Phase-1 definition verifier does not bind %s@%s", workflowadapter.CodeEdgePhase1WorkflowTemplateID, workflowadapter.CodeEdgePhase1WorkflowTemplateVersion)
	}
	if err := verifier.VerifyLockIdentity(verifier.LockIdentity()); err != nil {
		return nil, fmt.Errorf("verify CodeEdge Phase-1 definition lock identity: %w", err)
	}
	lock := verifier.Lock()
	if !lock.CatalogReceipt.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		return nil, fmt.Errorf("CodeEdge Phase-1 definition lock does not bind the parent template")
	}
	profile, err := lock.CodeEdgePhase1Profile()
	if err != nil {
		return nil, fmt.Errorf("CodeEdge Phase-1 definition lock has no complete parent-owned execution profile: %w", err)
	}
	policy, err := lock.CodeEdgePhase1FinalCompliance()
	if err != nil {
		return nil, fmt.Errorf("CodeEdge Phase-1 definition lock has no complete parent-owned final compliance policy: %w", err)
	}
	records, err := codeEdgePhase1DefinitionRecords(lock)
	if err != nil {
		return nil, err
	}
	return &codeEdgePhase1RunDefinitionProvider{profile: profile, policy: policy, records: records}, nil
}

// DefinitionForCodeEdgePhase1Run builds the task-revision parent definition
// from the installed lock only. Authoring IDs and task snapshot coordinates
// are validated as handoff lineage, but never used to select resources. The
// RunService subsequently replaces the empty intrinsic task_snapshot binding
// with its fresh managed archive after it has proved the sealed revision.
func (provider *codeEdgePhase1RunDefinitionProvider) DefinitionForCodeEdgePhase1Run(ctx context.Context, request app.CodeEdgePhase1RunDefinitionRequest) (app.CodeEdgePhase1RunDefinition, error) {
	if provider == nil {
		return app.CodeEdgePhase1RunDefinition{}, app.ErrCodeEdgePhase1DefinitionUnavailable
	}
	if ctx == nil {
		return app.CodeEdgePhase1RunDefinition{}, fmt.Errorf("CodeEdge Phase-1 definition context is required")
	}
	if err := ctx.Err(); err != nil {
		return app.CodeEdgePhase1RunDefinition{}, err
	}
	if err := validateCodeEdgePhase1DefinitionRequest(request); err != nil {
		return app.CodeEdgePhase1RunDefinition{}, err
	}
	specification, err := provider.executionSpec(request)
	if err != nil {
		return app.CodeEdgePhase1RunDefinition{}, fmt.Errorf("construct lock-owned CodeEdge Phase-1 execution specification: %w", err)
	}
	return app.CodeEdgePhase1RunDefinition{Profile: provider.profile.Clone(), ExecutionSpec: specification}, nil
}

var _ app.CodeEdgePhase1RunDefinitionProvider = (*codeEdgePhase1RunDefinitionProvider)(nil)

func validateCodeEdgePhase1DefinitionRequest(request app.CodeEdgePhase1RunDefinitionRequest) error {
	for _, identity := range []struct {
		label string
		value string
	}{
		{"Task", request.TaskID},
		{"TaskRevision", request.RevisionID},
		{"Standard authoring Run", request.AuthoringRunID},
		{"Standard authoring source", request.AuthoringSourceID},
		{"Standard authoring session", request.AuthoringSessionID},
		{"Standard authoring task snapshot artifact", string(request.TaskSnapshot.ID)},
	} {
		if err := store.ValidateUUIDv7(identity.value); err != nil {
			return fmt.Errorf("CodeEdge Phase-1 definition %s ID: %w", identity.label, err)
		}
	}
	if err := request.RevisionDigest.Validate(); err != nil {
		return fmt.Errorf("CodeEdge Phase-1 definition TaskRevision digest: %w", err)
	}
	if err := request.TaskSnapshot.ContentDigest.Validate(); err != nil {
		return fmt.Errorf("CodeEdge Phase-1 definition task snapshot digest: %w", err)
	}
	if request.TaskSnapshot.SchemaVersion != "harbor.artifact.v1" {
		return fmt.Errorf("CodeEdge Phase-1 definition task snapshot schema %q is not approved", request.TaskSnapshot.SchemaVersion)
	}
	return nil
}

func codeEdgePhase1DefinitionRecords(lock stageprovider.DeploymentOperationCatalogLock) (map[workflowkit.StageKey]stageprovider.DeploymentOperationCatalogLockRecord, error) {
	stageOrder := workflowadapter.CodeEdgePhase1StageOrder()
	if len(lock.Operations) != len(stageOrder) {
		return nil, fmt.Errorf("CodeEdge Phase-1 definition lock must contain exactly %d parent operations", len(stageOrder))
	}
	parent := workflowadapter.CodeEdgePhase1WorkflowTemplate()
	expectedStages := make(map[workflowkit.StageKey]workflowadapter.StageDefinition, len(parent.Catalog.Stages))
	for _, stage := range parent.Catalog.Stages {
		expectedStages[stage.Key] = stage
	}
	provider := workflowadapter.ProviderReference{ID: stageprovider.CodeEdgePhase1ProviderID, Kind: stageprovider.CodeEdgePhase1ProviderKind, Version: stageprovider.CodeEdgePhase1ProviderVersion}
	records := make(map[workflowkit.StageKey]stageprovider.DeploymentOperationCatalogLockRecord, len(lock.Operations))
	for _, record := range lock.Operations {
		if err := record.Validate(); err != nil {
			return nil, fmt.Errorf("CodeEdge Phase-1 definition record: %w", err)
		}
		if record.Provider != provider {
			return nil, fmt.Errorf("CodeEdge Phase-1 definition lock record %q uses unapproved provider %s (%s@%s)", record.Stage.Key, record.Provider.ID, record.Provider.Kind, record.Provider.Version)
		}
		expected, found := expectedStages[record.Stage.Key]
		if !found || record.Stage.Type != codeEdgePhase1BindingType(record.Stage.Key) ||
			record.Stage.Group != expected.Group || record.Stage.Plugin.ID != expected.Plugin.ID || record.Stage.Plugin.Version != expected.Plugin.Version {
			return nil, fmt.Errorf("CodeEdge Phase-1 definition lock has an invalid parent stage contract for %q", record.Stage.Key)
		}
		if _, duplicate := records[record.Stage.Key]; duplicate {
			return nil, fmt.Errorf("CodeEdge Phase-1 definition lock duplicates stage %q", record.Stage.Key)
		}
		records[record.Stage.Key] = record.Clone()
	}
	for _, stageKey := range stageOrder {
		if _, found := records[stageKey]; !found {
			return nil, fmt.Errorf("CodeEdge Phase-1 definition lock omits stage %q", stageKey)
		}
	}
	return records, nil
}

func (provider *codeEdgePhase1RunDefinitionProvider) executionSpec(request app.CodeEdgePhase1RunDefinitionRequest) (workflowadapter.RunExecutionSpec, error) {
	if provider == nil {
		return workflowadapter.RunExecutionSpec{}, app.ErrCodeEdgePhase1DefinitionUnavailable
	}
	if err := provider.profile.Validate(); err != nil {
		return workflowadapter.RunExecutionSpec{}, fmt.Errorf("stored parent execution profile: %w", err)
	}
	if err := provider.policy.Validate(); err != nil {
		return workflowadapter.RunExecutionSpec{}, fmt.Errorf("stored parent final compliance policy: %w", err)
	}
	policy := provider.policy.Clone()
	specification := workflowadapter.RunExecutionSpec{
		Format: workflowadapter.RunExecutionSpecFormat, Version: workflowadapter.RunExecutionSpecVersion,
		Template: workflowadapter.CodeEdgePhase1TemplateReference(),
		Selection: workflowadapter.RunSelectionReference{
			Kind: workflowadapter.RunSelectionTaskRevision, TaskID: request.TaskID, RevisionID: request.RevisionID, RevisionDigest: request.RevisionDigest,
		},
		References:                    workflowadapter.ExecutionReferenceSet{},
		Stages:                        make([]workflowadapter.StageExecutionBinding, 0, len(workflowadapter.CodeEdgePhase1StageOrder())),
		CodeEdgeFinalCompliancePolicy: &policy,
	}
	checkouts := make(map[string]workflowadapter.CheckoutReference)
	runtimes := make(map[string]workflowadapter.RuntimeReference)
	providers := make(map[string]workflowadapter.ProviderReference)
	secrets := make(map[string]workflowadapter.SecretReference)
	for _, stageKey := range workflowadapter.CodeEdgePhase1StageOrder() {
		record, found := provider.records[stageKey]
		if !found {
			return workflowadapter.RunExecutionSpec{}, fmt.Errorf("stored parent definition omits stage %q", stageKey)
		}
		checkout := workflowadapter.CheckoutReference{ID: record.Checkout.ID, RevisionID: request.RevisionID, RevisionDigest: request.RevisionDigest}
		if existing, present := checkouts[checkout.ID]; present && existing != checkout {
			return workflowadapter.RunExecutionSpec{}, fmt.Errorf("stored parent definition has conflicting checkout %q", checkout.ID)
		}
		checkouts[checkout.ID] = checkout
		if existing, present := runtimes[record.Runtime.ID]; present && existing != record.Runtime {
			return workflowadapter.RunExecutionSpec{}, fmt.Errorf("stored parent definition has conflicting runtime %q", record.Runtime.ID)
		}
		runtimes[record.Runtime.ID] = record.Runtime
		if existing, present := providers[record.Provider.ID]; present && existing != record.Provider {
			return workflowadapter.RunExecutionSpec{}, fmt.Errorf("stored parent definition has conflicting provider %q", record.Provider.ID)
		}
		providers[record.Provider.ID] = record.Provider
		secretIDs := make([]string, 0, len(record.Secrets))
		for _, secret := range record.Secrets {
			if existing, present := secrets[secret.ID]; present && existing != secret {
				return workflowadapter.RunExecutionSpec{}, fmt.Errorf("stored parent definition has conflicting secret %q", secret.ID)
			}
			secrets[secret.ID] = secret
			secretIDs = append(secretIDs, secret.ID)
		}
		sort.Strings(secretIDs)
		binding, err := codeEdgePhase1CatalogStageBinding(workflowadapter.StageBindingBase{
			Type:           record.Stage.Type,
			StageKey:       record.Stage.Key,
			Plugin:         record.Stage.Plugin,
			ArtifactInputs: []workflowadapter.ArtifactInputReference{},
			CheckoutID:     checkout.ID,
			RuntimeID:      record.Runtime.ID,
			Operation:      record.Operation.Clone(),
			SecretIDs:      secretIDs,
		})
		if err != nil {
			return workflowadapter.RunExecutionSpec{}, err
		}
		specification.Stages = append(specification.Stages, binding)
	}
	for _, checkout := range checkouts {
		specification.References.Checkouts = append(specification.References.Checkouts, checkout)
	}
	for _, runtime := range runtimes {
		specification.References.Runtimes = append(specification.References.Runtimes, runtime)
	}
	for _, provider := range providers {
		specification.References.Providers = append(specification.References.Providers, provider)
	}
	for _, secret := range secrets {
		specification.References.Secrets = append(specification.References.Secrets, secret)
	}
	sort.Slice(specification.References.Checkouts, func(left, right int) bool {
		return specification.References.Checkouts[left].ID < specification.References.Checkouts[right].ID
	})
	sort.Slice(specification.References.Runtimes, func(left, right int) bool {
		return specification.References.Runtimes[left].ID < specification.References.Runtimes[right].ID
	})
	sort.Slice(specification.References.Providers, func(left, right int) bool {
		return specification.References.Providers[left].ID < specification.References.Providers[right].ID
	})
	sort.Slice(specification.References.Secrets, func(left, right int) bool {
		return specification.References.Secrets[left].ID < specification.References.Secrets[right].ID
	})
	if err := specification.Validate(); err != nil {
		return workflowadapter.RunExecutionSpec{}, err
	}
	return specification, nil
}

func codeEdgePhase1CatalogStageBinding(base workflowadapter.StageBindingBase) (workflowadapter.StageExecutionBinding, error) {
	switch base.Type {
	case workflowadapter.StageBindingRepoPrepare:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingRepoAnalyze:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingCodeEdgeLint:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingDockerBuild:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingInitialVerify:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingOracleVerify:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingTestsAnalysis:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingSolutionReview:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingQualityCheck:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingSimilarityCheck:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingFinalReview:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingEvaluatorEvidenceHandoff:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingSubmissionLint:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingResultReview:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	case workflowadapter.StageBindingPackage:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}, nil
	default:
		return nil, fmt.Errorf("CodeEdge Phase-1 definition has unsupported stage binding type %q", base.Type)
	}
}

func codeEdgePhase1BindingType(key workflowkit.StageKey) workflowadapter.StageBindingType {
	switch key {
	case workflowadapter.RepoPrepare:
		return workflowadapter.StageBindingRepoPrepare
	case workflowadapter.RepoAnalyze:
		return workflowadapter.StageBindingRepoAnalyze
	case workflowadapter.CodeEdgeLint:
		return workflowadapter.StageBindingCodeEdgeLint
	case workflowadapter.DockerBuild:
		return workflowadapter.StageBindingDockerBuild
	case workflowadapter.InitialVerify:
		return workflowadapter.StageBindingInitialVerify
	case workflowadapter.OracleVerify:
		return workflowadapter.StageBindingOracleVerify
	case workflowadapter.TestsAnalysis:
		return workflowadapter.StageBindingTestsAnalysis
	case workflowadapter.SolutionReview:
		return workflowadapter.StageBindingSolutionReview
	case workflowadapter.QualityCheck:
		return workflowadapter.StageBindingQualityCheck
	case workflowadapter.SimilarityCheck:
		return workflowadapter.StageBindingSimilarityCheck
	case workflowadapter.FinalReview:
		return workflowadapter.StageBindingFinalReview
	case workflowadapter.EvaluatorEvidenceHandoff:
		return workflowadapter.StageBindingEvaluatorEvidenceHandoff
	case workflowadapter.SubmissionLint:
		return workflowadapter.StageBindingSubmissionLint
	case workflowadapter.ResultReview:
		return workflowadapter.StageBindingResultReview
	case workflowadapter.Package:
		return workflowadapter.StageBindingPackage
	default:
		return ""
	}
}
