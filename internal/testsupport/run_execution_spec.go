// Package testsupport contains explicit fixtures shared by black-box V2 tests.
package testsupport

import (
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// StageOperationResolverFunc adapts a pure test resolver function to the
// production preflight interface. It exists only so black-box application and
// TUI tests can explicitly install a controlled resolver fixture.
type StageOperationResolverFunc func(workflowadapter.StageOperationResolution) error

func (function StageOperationResolverFunc) ValidateStageOperation(resolution workflowadapter.StageOperationResolution) error {
	return function(resolution)
}

// AcceptAllStageOperationResolver accepts already-validated fixture bindings.
// Production code must install a stageprovider catalog-bound resolver instead,
// which resolves each exact provider/operation pair and its deployment lock.
func AcceptAllStageOperationResolver() workflowadapter.StageOperationResolver {
	return StageOperationResolverFunc(func(workflowadapter.StageOperationResolution) error { return nil })
}

// CompleteRunExecutionSpec returns a fully explicit, catalog-complete V2
// execution specification for tests. Production callers must provide their own
// selected references and never receive this fixture implicitly.
func CompleteRunExecutionSpec(taskID, revisionID, revisionDigest string) workflowadapter.RunExecutionSpec {
	selection := workflowadapter.RunSelectionReference{
		TaskID: taskID, RevisionID: revisionID, RevisionDigest: workflowkit.SubjectDigest(revisionDigest),
	}
	specification := workflowadapter.RunExecutionSpec{
		Format: workflowadapter.RunExecutionSpecFormat, Version: workflowadapter.RunExecutionSpecVersion,
		Template: workflowadapter.StandardTemplateReference(), Selection: selection,
		References: workflowadapter.ExecutionReferenceSet{
			Artifacts: []workflowadapter.ArtifactReference{
				{ID: "018f0a73-3b49-7000-8000-000000000003", ContentDigest: fixtureFingerprint('a'), SchemaVersion: "harbor.artifact.v1"},
				{ID: "018f0a73-3b49-7000-8000-000000000004", ContentDigest: fixtureFingerprint('b'), SchemaVersion: "harbor.artifact.v1"},
				{ID: "018f0a73-3b49-7000-8000-000000000005", ContentDigest: fixtureFingerprint('c'), SchemaVersion: "harbor.artifact.v1"},
			},
			Checkouts: []workflowadapter.CheckoutReference{
				{ID: "checkout-main", RevisionID: revisionID, RevisionDigest: workflowkit.SubjectDigest(revisionDigest)},
				{ID: "checkout-package", RevisionID: revisionID, RevisionDigest: workflowkit.SubjectDigest(revisionDigest)},
			},
			Runtimes: []workflowadapter.RuntimeReference{
				{ID: "runtime-local", Kind: "local", Version: "1"},
			},
			Providers: []workflowadapter.ProviderReference{
				{ID: "provider-local", Kind: "native", Version: "1"},
				{ID: "provider-review", Kind: "durable-review", Version: "1"},
			},
			Secrets: []workflowadapter.SecretReference{
				{ID: "secret-repository", Provider: "local-keyring", Version: "1"},
			},
		},
	}
	for _, definition := range workflowadapter.StandardStageCatalog().Stages {
		base := workflowadapter.StageBindingBase{
			Type:           fixtureBindingType(definition.Key),
			StageKey:       definition.Key,
			Plugin:         workflowkit.PluginBinding{ID: definition.Plugin.ID, Version: definition.Plugin.Version},
			CheckoutID:     "checkout-main",
			RuntimeID:      "runtime-local",
			ArtifactInputs: []workflowadapter.ArtifactInputReference{},
			SecretIDs:      []string{},
			Operation: workflowadapter.StageOperationBinding{
				ProviderID: "provider-local", OperationID: string(definition.Key), Version: "1",
				Payload: workflowadapter.LocalCommandOperationPayload{
					CommandID: "harbor-stage", Arguments: []string{string(definition.Key)},
				},
			},
		}
		switch definition.Key {
		case "task_review", "content_review", "solution_review", "final_review":
			base.Operation.ProviderID = "provider-review"
			base.Operation.Payload = workflowadapter.DurableReviewOperationPayload{PolicyID: "harbor-review.v1"}
		case "repo_prepare":
			base.SecretIDs = []string{"secret-repository"}
		case "repo_analyze":
			base.ArtifactInputs = []workflowadapter.ArtifactInputReference{{Port: "repo_prepared", ArtifactID: "018f0a73-3b49-7000-8000-000000000003"}}
		case "task_design":
			base.ArtifactInputs = []workflowadapter.ArtifactInputReference{
				{Port: "repo_prepared", ArtifactID: "018f0a73-3b49-7000-8000-000000000003"},
				{Port: "repo_analysis", ArtifactID: "018f0a73-3b49-7000-8000-000000000004"},
			}
		case "generate_task_files":
			base.ArtifactInputs = []workflowadapter.ArtifactInputReference{
				{Port: "repo_prepared", ArtifactID: "018f0a73-3b49-7000-8000-000000000003"},
				{Port: "repo_analysis", ArtifactID: "018f0a73-3b49-7000-8000-000000000004"},
				{Port: "task_proposal", ArtifactID: "018f0a73-3b49-7000-8000-000000000005"},
			}
		case "package":
			base.CheckoutID = "checkout-package"
		}
		specification.Stages = append(specification.Stages, fixtureBinding(base))
	}
	return specification
}

func fixtureFingerprint(character byte) workflowkit.Fingerprint {
	return workflowkit.Fingerprint("sha256:" + strings.Repeat(string(character), 64))
}

func fixtureBinding(base workflowadapter.StageBindingBase) workflowadapter.StageExecutionBinding {
	switch base.Type {
	case workflowadapter.StageBindingRepoPrepare:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingRepoAnalyze:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingTaskDesign:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingTaskReview:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingGenerateTaskFiles:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingInstructionGen:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingTaskTOMLGen:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingDockerfileGen:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingContentReview:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingSolveGen:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingTestGen:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingTestsAnalysis:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingSolutionReview:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingMaterializeTask:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingTaskRepair:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingRuntimeSelfCheck:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingHarborVerify:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingDockerBuild:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingInitialVerify:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingOracleVerify:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingCodeEdgeLint:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingQualityCheck:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingSimilarityCheck:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingFinalReview:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	case workflowadapter.StageBindingPackage:
		return workflowadapter.UniversalStageBinding{StageBindingBase: base}
	default:
		panic("unsupported Harbor V2 test stage binding")
	}
}

func fixtureBindingType(key workflowkit.StageKey) workflowadapter.StageBindingType {
	switch key {
	case "repo_prepare":
		return workflowadapter.StageBindingRepoPrepare
	case "repo_analyze":
		return workflowadapter.StageBindingRepoAnalyze
	case "task_design":
		return workflowadapter.StageBindingTaskDesign
	case "task_review":
		return workflowadapter.StageBindingTaskReview
	case "generate_task_files":
		return workflowadapter.StageBindingGenerateTaskFiles
	case "instruction_generate":
		return workflowadapter.StageBindingInstructionGen
	case "task_toml_generate":
		return workflowadapter.StageBindingTaskTOMLGen
	case "dockerfile_generate":
		return workflowadapter.StageBindingDockerfileGen
	case "content_review":
		return workflowadapter.StageBindingContentReview
	case "solve_generate":
		return workflowadapter.StageBindingSolveGen
	case "test_generate":
		return workflowadapter.StageBindingTestGen
	case "tests_analysis":
		return workflowadapter.StageBindingTestsAnalysis
	case "solution_review":
		return workflowadapter.StageBindingSolutionReview
	case "materialize_task":
		return workflowadapter.StageBindingMaterializeTask
	case "task_repair":
		return workflowadapter.StageBindingTaskRepair
	case "runtime_self_check":
		return workflowadapter.StageBindingRuntimeSelfCheck
	case "harbor_verify":
		return workflowadapter.StageBindingHarborVerify
	case "docker_build":
		return workflowadapter.StageBindingDockerBuild
	case "initial_verify":
		return workflowadapter.StageBindingInitialVerify
	case "oracle_verify":
		return workflowadapter.StageBindingOracleVerify
	case "codeedge_lint":
		return workflowadapter.StageBindingCodeEdgeLint
	case "quality_check":
		return workflowadapter.StageBindingQualityCheck
	case "similarity_check":
		return workflowadapter.StageBindingSimilarityCheck
	case "final_review":
		return workflowadapter.StageBindingFinalReview
	case "package":
		return workflowadapter.StageBindingPackage
	default:
		panic("unsupported Harbor V2 test stage key")
	}
}
