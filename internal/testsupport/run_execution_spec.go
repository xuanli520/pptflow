// Package testsupport contains explicit fixtures shared by black-box V2 tests.
package testsupport

import (
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
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
				{ID: "runtime-evaluator", Kind: "container", Version: "1"},
			},
			Providers: []workflowadapter.ProviderReference{
				{ID: "provider-local", Kind: "native", Version: "1"},
				{ID: "provider-evaluator", Kind: "evaluation", Version: "1"},
				{ID: "provider-review", Kind: "durable-review", Version: "1"},
			},
			Secrets: []workflowadapter.SecretReference{
				{ID: "secret-repository", Provider: "local-keyring", Version: "1"},
				{ID: "secret-evaluator", Provider: "local-keyring", Version: "1"},
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
		case "task_review", "content_review", "solution_review", "final_review", "result_review":
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
		case "harbor_run_qwen":
			base.RuntimeID = "runtime-evaluator"
			base.Operation.ProviderID = "provider-evaluator"
			base.Operation.Payload = workflowadapter.ContainerCommandOperationPayload{
				ImageDigest: "registry.example/harbor/evaluator@sha256:" + strings.Repeat("f", 64),
				Command:     []string{"harbor-evaluator", string(definition.Key)},
			}
			base.SecretIDs = []string{"secret-evaluator", "secret-repository"}
		case "harbor_run_opus":
			base.RuntimeID = "runtime-evaluator"
			base.Operation.ProviderID = "provider-evaluator"
			base.Operation.Payload = workflowadapter.ContainerCommandOperationPayload{
				ImageDigest: "registry.example/harbor/evaluator@sha256:" + strings.Repeat("f", 64),
				Command:     []string{"harbor-evaluator", string(definition.Key)},
			}
		case "package":
			base.CheckoutID = "checkout-package"
		}
		specification.Stages = append(specification.Stages, fixtureBinding(base))
	}
	return specification
}

// CompleteCodeEdgePhase1RunExecutionSpec returns an explicit fixture bound to
// the closed CodeEdge Phase-1 template. It intentionally builds bindings from
// that template's catalog rather than relabeling the Standard fixture, so a
// test cannot accidentally exercise a Standard fallback through shared stage
// names or plugins.
func CompleteCodeEdgePhase1RunExecutionSpec(taskID, revisionID, revisionDigest string) workflowadapter.RunExecutionSpec {
	selection := workflowadapter.RunSelectionReference{
		TaskID: taskID, RevisionID: revisionID, RevisionDigest: workflowkit.SubjectDigest(revisionDigest),
	}
	policy := completeCodeEdgePhase1FinalCompliancePolicy()
	catalog := workflowadapter.CodeEdgePhase1StageCatalog()
	artifactIDs := map[string]string{
		"harbor.artifact.v1":                                  "018f0a73-3b49-7000-8000-000000000006",
		"harbor.review-decision.v1":                           "018f0a73-3b49-7000-8000-000000000007",
		workflowadapter.CodeEdgeSubmissionReportSchemaVersion: "018f0a73-3b49-7000-8000-000000000008",
	}
	artifactDigests := map[string]workflowkit.Fingerprint{
		"harbor.artifact.v1":                                  fixtureFingerprint('d'),
		"harbor.review-decision.v1":                           fixtureFingerprint('e'),
		workflowadapter.CodeEdgeSubmissionReportSchemaVersion: fixtureFingerprint('f'),
	}
	usedSchemas := make(map[string]struct{})
	for _, definition := range catalog.Stages {
		for _, input := range definition.Inputs {
			if _, supported := artifactIDs[input.SchemaVersion]; !supported {
				panic("testsupport: CodeEdge fixture has unsupported artifact schema " + input.SchemaVersion)
			}
			usedSchemas[input.SchemaVersion] = struct{}{}
		}
	}
	artifacts := make([]workflowadapter.ArtifactReference, 0, len(usedSchemas))
	for _, schema := range []string{"harbor.artifact.v1", "harbor.review-decision.v1", workflowadapter.CodeEdgeSubmissionReportSchemaVersion} {
		if _, used := usedSchemas[schema]; !used {
			continue
		}
		artifacts = append(artifacts, workflowadapter.ArtifactReference{
			ID: workflowkit.ArtifactID(artifactIDs[schema]), ContentDigest: artifactDigests[schema], SchemaVersion: schema,
		})
	}
	specification := workflowadapter.RunExecutionSpec{
		Format: workflowadapter.RunExecutionSpecFormat, Version: workflowadapter.RunExecutionSpecVersion,
		Template: workflowadapter.CodeEdgePhase1TemplateReference(), Selection: selection,
		CodeEdgeFinalCompliancePolicy: &policy,
		References: workflowadapter.ExecutionReferenceSet{
			Artifacts: artifacts,
			Checkouts: []workflowadapter.CheckoutReference{
				{ID: "checkout-main", RevisionID: revisionID, RevisionDigest: workflowkit.SubjectDigest(revisionDigest)},
				{ID: "checkout-package", RevisionID: revisionID, RevisionDigest: workflowkit.SubjectDigest(revisionDigest)},
			},
			Runtimes: []workflowadapter.RuntimeReference{
				{ID: "runtime-local", Kind: "local", Version: "1"},
				{ID: "runtime-evaluator", Kind: "container", Version: "1"},
			},
			Providers: []workflowadapter.ProviderReference{
				{ID: "provider-local", Kind: "native", Version: "1"},
				{ID: "provider-evaluator", Kind: "evaluation", Version: "1"},
				{ID: "provider-review", Kind: "durable-review", Version: "1"},
			},
			Secrets: []workflowadapter.SecretReference{
				{ID: "secret-repository", Provider: "local-keyring", Version: "1"},
				{ID: "secret-evaluator", Provider: "local-keyring", Version: "1"},
			},
		},
	}
	for _, definition := range catalog.Stages {
		artifactInputs := make([]workflowadapter.ArtifactInputReference, 0, len(definition.Inputs))
		for _, input := range definition.Inputs {
			artifactInputs = append(artifactInputs, workflowadapter.ArtifactInputReference{Port: input.Name, ArtifactID: workflowkit.ArtifactID(artifactIDs[input.SchemaVersion])})
		}
		base := workflowadapter.StageBindingBase{
			Type: fixtureBindingType(definition.Key), StageKey: definition.Key,
			Plugin:     workflowkit.PluginBinding{ID: definition.Plugin.ID, Version: definition.Plugin.Version},
			CheckoutID: "checkout-main", RuntimeID: "runtime-local", ArtifactInputs: artifactInputs, SecretIDs: []string{},
			Operation: workflowadapter.StageOperationBinding{
				ProviderID: "provider-local", OperationID: string(definition.Key), Version: "1",
				Payload: workflowadapter.LocalCommandOperationPayload{CommandID: "harbor-stage", Arguments: []string{string(definition.Key)}},
			},
		}
		switch definition.Key {
		case workflowadapter.SolutionReview, workflowadapter.FinalReview, workflowadapter.ResultReview:
			base.Operation.ProviderID = "provider-review"
			base.Operation.Payload = workflowadapter.DurableReviewOperationPayload{PolicyID: "harbor-review.v1"}
		case workflowadapter.RepoPrepare:
			base.SecretIDs = []string{"secret-repository"}
		case workflowadapter.HarborRunQwen:
			base.RuntimeID = "runtime-evaluator"
			base.Operation.ProviderID = "provider-evaluator"
			base.Operation.Payload = workflowadapter.ContainerCommandOperationPayload{
				ImageDigest: "registry.example/harbor/evaluator@sha256:" + strings.Repeat("f", 64),
				Command:     []string{"harbor-evaluator", string(definition.Key)},
			}
			base.SecretIDs = []string{"secret-evaluator", "secret-repository"}
		case workflowadapter.HarborRunOpus:
			base.RuntimeID = "runtime-evaluator"
			base.Operation.ProviderID = "provider-evaluator"
			base.Operation.Payload = workflowadapter.ContainerCommandOperationPayload{
				ImageDigest: "registry.example/harbor/evaluator@sha256:" + strings.Repeat("f", 64),
				Command:     []string{"harbor-evaluator", string(definition.Key)},
			}
		case workflowadapter.Package:
			base.CheckoutID = "checkout-package"
		}
		specification.Stages = append(specification.Stages, fixtureBinding(base))
	}
	return specification
}

func completeCodeEdgePhase1FinalCompliancePolicy() codeedge.FinalCompliancePolicy {
	maximumPassingTrials := 1
	qwen := codeedge.EvaluationPolicy{
		ID:                 "codeedge.qwen.pass-at-four",
		Version:            "1",
		HarborResultFormat: codeedge.HarborJobResultV018,
		Evaluator: codeedge.EvaluatorIdentity{
			ProfileID: "codeedge-qwen-profile", ProfileVersion: "1",
			AgentName: "codeedge-agent", AgentVersion: "1",
			ModelName: "qwen-approved-model", ModelProvider: "controlled-provider",
		},
		LogicalTrialCount:        4,
		PassRewardKey:            "reward",
		PassRewardAtLeast:        1,
		MaxPassingTrials:         &maximumPassingTrials,
		MinimumAverageTurns:      20,
		ScreenshotMediaType:      "image/png",
		FailureClassifierID:      "codeedge-infra-classifier",
		FailureClassifierVersion: "1",
		InfraExceptionTypes:      []string{"DockerBuildError", "NetworkError"},
	}
	opus := qwen.Clone()
	opus.ID = "codeedge.opus.reference"
	opus.Evaluator.ProfileID = "codeedge-opus-profile"
	opus.Evaluator.ModelName = "opus-reference-model"
	opus.MaxPassingTrials = nil
	return codeedge.FinalCompliancePolicy{
		ID:                            "codeedge.phase1.final-compliance",
		Version:                       "1",
		QwenPolicy:                    qwen,
		OpusPolicy:                    opus,
		SubmissionCheckerID:           "codeedge.submission-check",
		SubmissionCheckerVersion:      "1",
		SubmissionReportSchemaVersion: workflowadapter.CodeEdgeSubmissionReportSchemaVersion,
	}
}

func fixtureFingerprint(character byte) workflowkit.Fingerprint {
	return workflowkit.Fingerprint("sha256:" + strings.Repeat(string(character), 64))
}

func fixtureBinding(base workflowadapter.StageBindingBase) workflowadapter.StageExecutionBinding {
	switch base.Type {
	case workflowadapter.StageBindingRepoPrepare:
		return workflowadapter.RepoPrepareBinding{StageBindingBase: base}
	case workflowadapter.StageBindingRepoAnalyze:
		return workflowadapter.RepoAnalyzeBinding{StageBindingBase: base}
	case workflowadapter.StageBindingTaskDesign:
		return workflowadapter.TaskDesignBinding{StageBindingBase: base}
	case workflowadapter.StageBindingTaskReview:
		return workflowadapter.TaskReviewBinding{StageBindingBase: base}
	case workflowadapter.StageBindingGenerateTaskFiles:
		return workflowadapter.GenerateTaskFilesBinding{StageBindingBase: base}
	case workflowadapter.StageBindingInstructionGen:
		return workflowadapter.InstructionGenBinding{StageBindingBase: base}
	case workflowadapter.StageBindingTaskTOMLGen:
		return workflowadapter.TaskTOMLGenBinding{StageBindingBase: base}
	case workflowadapter.StageBindingDockerfileGen:
		return workflowadapter.DockerfileGenBinding{StageBindingBase: base}
	case workflowadapter.StageBindingContentReview:
		return workflowadapter.ContentReviewBinding{StageBindingBase: base}
	case workflowadapter.StageBindingSolveGen:
		return workflowadapter.SolveGenBinding{StageBindingBase: base}
	case workflowadapter.StageBindingTestGen:
		return workflowadapter.TestGenBinding{StageBindingBase: base}
	case workflowadapter.StageBindingTestsAnalysis:
		return workflowadapter.TestsAnalysisBinding{StageBindingBase: base}
	case workflowadapter.StageBindingSolutionReview:
		return workflowadapter.SolutionReviewBinding{StageBindingBase: base}
	case workflowadapter.StageBindingMaterializeTask:
		return workflowadapter.MaterializeTaskBinding{StageBindingBase: base}
	case workflowadapter.StageBindingTaskRepair:
		return workflowadapter.TaskRepairBinding{StageBindingBase: base}
	case workflowadapter.StageBindingRuntimeSelfCheck:
		return workflowadapter.RuntimeSelfCheckBinding{StageBindingBase: base}
	case workflowadapter.StageBindingHarborVerify:
		return workflowadapter.HarborVerifyBinding{StageBindingBase: base}
	case workflowadapter.StageBindingDockerBuild:
		return workflowadapter.DockerBuildBinding{StageBindingBase: base}
	case workflowadapter.StageBindingInitialVerify:
		return workflowadapter.InitialVerifyBinding{StageBindingBase: base}
	case workflowadapter.StageBindingOracleVerify:
		return workflowadapter.OracleVerifyBinding{StageBindingBase: base}
	case workflowadapter.StageBindingCodeEdgeLint:
		return workflowadapter.CodeEdgeLintBinding{StageBindingBase: base}
	case workflowadapter.StageBindingQualityCheck:
		return workflowadapter.QualityCheckBinding{StageBindingBase: base}
	case workflowadapter.StageBindingSimilarityCheck:
		return workflowadapter.SimilarityCheckBinding{StageBindingBase: base}
	case workflowadapter.StageBindingFinalReview:
		return workflowadapter.FinalReviewBinding{StageBindingBase: base}
	case workflowadapter.StageBindingHarborRunQwen:
		return workflowadapter.HarborRunQwenBinding{StageBindingBase: base}
	case workflowadapter.StageBindingHarborRunOpus:
		return workflowadapter.HarborRunOpusBinding{StageBindingBase: base}
	case workflowadapter.StageBindingResultReview:
		return workflowadapter.ResultReviewBinding{StageBindingBase: base}
	case workflowadapter.StageBindingSubmissionLint:
		return workflowadapter.SubmissionLintBinding{StageBindingBase: base}
	case workflowadapter.StageBindingPackage:
		return workflowadapter.PackageBinding{StageBindingBase: base}
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
	case "harbor_run_qwen":
		return workflowadapter.StageBindingHarborRunQwen
	case "harbor_run_opus":
		return workflowadapter.StageBindingHarborRunOpus
	case "result_review":
		return workflowadapter.StageBindingResultReview
	case "submission_lint":
		return workflowadapter.StageBindingSubmissionLint
	case "package":
		return workflowadapter.StageBindingPackage
	default:
		panic("unsupported Harbor V2 test stage key")
	}
}
