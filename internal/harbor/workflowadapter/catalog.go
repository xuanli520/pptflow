// Package workflowadapter owns Harbor's versioned workflow policy. It maps
// Harbor node identifiers and resource vocabulary onto the domain-neutral
// workflowkit descriptors without making workflowkit depend on Harbor.
package workflowadapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// StandardWorkflowTemplateID and StandardWorkflowTemplateVersion identify
	// Harbor's complete V2 lifecycle template. Profiles and execution specs must
	// use the exact template reference; no compatibility aliases remain.
	StandardWorkflowTemplateID      = "harbor.task-lifecycle"
	StandardWorkflowTemplateVersion = "2.2.0"

	standardCatalogID       = "harbor.standard-stage-catalog"
	standardCatalogVersion  = "2.2.0"
	stageDescriptorVersion  = "2.1.0"
	pluginDescriptorVersion = "1.0.0"
)

var errInvalidCatalog = errors.New("harbor workflow adapter: invalid catalog")

// StageGroup is the stable Harbor-facing grouping used for selection and
// presentation. It is intentionally separate from the descriptor DAG: group
// membership never determines invalidation semantics.
type StageGroup string

const (
	StageSourcePrepare  StageGroup = "source_prepare"
	StageTaskAnalysis   StageGroup = "task_analysis"
	StageTaskDesign     StageGroup = "task_design"
	StageTaskGeneration StageGroup = "task_generation"
	StageRuntimeVerify  StageGroup = "runtime_verify"
	StageQuality        StageGroup = "quality"
	StageSimilarity     StageGroup = "similarity"
	StageFinalReview    StageGroup = "final_review"
	StageEvaluation     StageGroup = "evaluation"
	StageSubmission     StageGroup = "submission"
	StageDelivery       StageGroup = "delivery"
)

var standardStageGroups = []StageGroup{
	StageSourcePrepare,
	StageTaskAnalysis,
	StageTaskDesign,
	StageTaskGeneration,
	StageRuntimeVerify,
	StageQuality,
	StageSimilarity,
	StageFinalReview,
	StageEvaluation,
	StageSubmission,
	StageDelivery,
}

// StandardStageGroups returns the confirmed eleven group keys in their stable
// presentation order.
func StandardStageGroups() []StageGroup {
	return append([]StageGroup(nil), standardStageGroups...)
}

func (group StageGroup) valid() bool {
	for _, expected := range standardStageGroups {
		if group == expected {
			return true
		}
	}
	return false
}

// ReviewKind identifies a durable review request. A review gate is still an
// ordinary executable stage for attempt accounting; its review capability is
// explicit rather than inferred from a node ID at runtime.
type ReviewKind string

const (
	ReviewTaskDirection    ReviewKind = "task_direction"
	ReviewContent          ReviewKind = "content"
	ReviewSolutionVerifier ReviewKind = "solution_verifier"
	ReviewFinalQuality     ReviewKind = "final_quality"
	// ReviewEvaluatorEvidence approves the immutable parent-to-child evidence
	// handoff after the child evaluator Run has been independently verified.
	ReviewEvaluatorEvidence ReviewKind = "evaluator_evidence_handoff"
	ReviewModelResult       ReviewKind = "model_result"
)

func (kind ReviewKind) valid() bool {
	switch kind {
	case ReviewTaskDirection, ReviewContent, ReviewSolutionVerifier, ReviewFinalQuality, ReviewEvaluatorEvidence, ReviewModelResult:
		return true
	default:
		return false
	}
}

// GateDefinition declares the review-specific capability of a durable stage
// attempt. Waiting for a human decision is not an execution timeout; runtimes
// must project it as a durable waiting StageAttempt.
type GateDefinition struct {
	ReviewKind       ReviewKind               `json:"review_kind"`
	DecisionArtifact workflowkit.ArtifactSpec `json:"decision_artifact"`
}

func (gate GateDefinition) clone() GateDefinition { return gate }

// PluginDescriptor identifies the typed Harbor plugin bound to a stage. The
// plugin identifier belongs here, not in workflowkit.
type PluginDescriptor struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

func (plugin PluginDescriptor) validate() error {
	if strings.TrimSpace(plugin.ID) == "" {
		return fmt.Errorf("%w: plugin id is required", errInvalidCatalog)
	}
	if strings.TrimSpace(plugin.Version) == "" {
		return fmt.Errorf("%w: plugin version is required", errInvalidCatalog)
	}
	return nil
}

// StageDefinition is Harbor's static contribution to a workflowkit stage.
// ExecutionBudget is deliberately excluded: every run must supply its fully
// explicit budget through an ExecutionProfile.
type StageDefinition struct {
	Key           workflowkit.StageKey            `json:"key"`
	Version       string                          `json:"version"`
	Group         StageGroup                      `json:"group"`
	Dependencies  []workflowkit.StageKey          `json:"dependencies"`
	Plugin        PluginDescriptor                `json:"plugin"`
	Inputs        []workflowkit.ArtifactSpec      `json:"inputs"`
	Outputs       []workflowkit.ArtifactSpec      `json:"outputs"`
	ReadSet       []workflowkit.ResourceKey       `json:"read_set"`
	WriteSet      []workflowkit.ResourceKey       `json:"write_set"`
	Effect        workflowkit.StageEffect         `json:"effect"`
	Dispatch      workflowkit.StageDispatchPolicy `json:"dispatch,omitempty"`
	RequiredTurns int                             `json:"required_turns"`
	Retry         workflowkit.RetryPolicy         `json:"retry"`
	Verdicts      workflowkit.VerdictPolicy       `json:"verdicts"`
	Reuse         workflowkit.ReusePolicy         `json:"reuse"`
	Capabilities  workflowkit.CapabilitySet       `json:"capabilities"`
	Gate          *GateDefinition                 `json:"gate,omitempty"`
}

// Clone returns an independently mutable definition.
func (stage StageDefinition) Clone() StageDefinition {
	stage.Dependencies = append([]workflowkit.StageKey(nil), stage.Dependencies...)
	stage.Inputs = append([]workflowkit.ArtifactSpec(nil), stage.Inputs...)
	stage.Outputs = append([]workflowkit.ArtifactSpec(nil), stage.Outputs...)
	stage.ReadSet = append([]workflowkit.ResourceKey(nil), stage.ReadSet...)
	stage.WriteSet = append([]workflowkit.ResourceKey(nil), stage.WriteSet...)
	stage.Retry = stage.Retry.Clone()
	stage.Verdicts = stage.Verdicts.Clone()
	stage.Capabilities = stage.Capabilities.Clone()
	if stage.Gate != nil {
		gate := stage.Gate.clone()
		stage.Gate = &gate
	}
	return stage
}

// IsGate reports whether this descriptor is a durable review StageAttempt.
func (stage StageDefinition) IsGate() bool { return stage.Gate != nil }

// AllowsVerdict reports whether the completed stage may emit verdict.
func (stage StageDefinition) AllowsVerdict(verdict workflowkit.Verdict) bool {
	return stage.Verdicts.Allows(verdict)
}

// StageCatalog is a complete, code-versioned Harbor mapping. Every current
// Harbor node must appear exactly once, while the group list remains exactly
// the confirmed eleven groups.
type StageCatalog struct {
	Template TemplateReference `json:"template"`
	ID       string            `json:"id"`
	Version  string            `json:"version"`
	Stages   []StageDefinition `json:"stages"`
}

// Clone returns an independent catalog snapshot.
func (catalog StageCatalog) Clone() StageCatalog {
	stages := catalog.Stages
	catalog.Stages = make([]StageDefinition, len(stages))
	for index, stage := range stages {
		catalog.Stages[index] = stage.Clone()
	}
	return catalog
}

// Stage returns a clone of the catalog entry for key.
func (catalog StageCatalog) Stage(key workflowkit.StageKey) (StageDefinition, bool) {
	for _, stage := range catalog.Stages {
		if stage.Key == key {
			return stage.Clone(), true
		}
	}
	return StageDefinition{}, false
}

// StandardStageCatalog returns the complete V2 Harbor catalog. The graph is
// the full workflow shape with all current nodes represented; request-specific
// compilers may select optional verification, evaluator, or delivery work but
// cannot alter this node policy.
func StandardStageCatalog() StageCatalog {
	return StageCatalog{
		Template: StandardTemplateReference(),
		ID:       standardCatalogID,
		Version:  standardCatalogVersion,
		Stages: []StageDefinition{
			stage(RepoPrepare, StageSourcePrepare, nil, "harborfactory.repo_prepare", []workflowkit.ResourceKey{resourceSourceRepository}, []workflowkit.ResourceKey{resourceSourceSnapshot, resourceEvidenceRepoPrepare}, workflowkit.EffectEvidenceOnly, 1, passOnly(), artifactOutput("repo_prepared")),
			stage(RepoAnalyze, StageTaskAnalysis, []string{RepoPrepare}, "harborfactory.repo_analyze", []workflowkit.ResourceKey{resourceSourceSnapshot}, []workflowkit.ResourceKey{resourceAnalysisRepository}, workflowkit.EffectEvidenceOnly, 3, passOnly(), artifactInput("repo_prepared"), artifactOutput("repo_analysis")),
			stage(TaskDesign, StageTaskDesign, []string{RepoAnalyze}, "harborfactory.task_design", []workflowkit.ResourceKey{resourceSourceSnapshot, resourceAnalysisRepository}, []workflowkit.ResourceKey{resourceTaskDesign}, workflowkit.EffectContentProducer, 3, contentVerdicts(), artifactInput("repo_prepared"), artifactInput("repo_analysis"), artifactOutput("task_proposal")),
			gateStage(TaskReview, StageTaskDesign, []string{TaskDesign}, ReviewTaskDirection, []workflowkit.ResourceKey{resourceAnalysisRepository, resourceTaskDesign}, []workflowkit.ResourceKey{resourceReviewTaskDirection}, artifactInput("repo_analysis"), artifactInput("task_proposal")),
			stage(GenerateTaskFiles, StageTaskGeneration, []string{TaskReview}, "harborfactory.generate_task_files", []workflowkit.ResourceKey{resourceSourceSnapshot, resourceAnalysisRepository, resourceTaskDesign, resourceReviewTaskDirection}, []workflowkit.ResourceKey{resourceTaskGeneratedFiles}, workflowkit.EffectContentProducer, 3, contentVerdicts(), artifactInput("repo_prepared"), artifactInput("repo_analysis"), artifactInput("task_proposal"), reviewDecisionInput("task_review_decision"), artifactOutput("generated_task_files")),
			stage(InstructionGen, StageTaskGeneration, []string{GenerateTaskFiles}, "harborfactory.instruction_generate", []workflowkit.ResourceKey{resourceTaskGeneratedFiles}, []workflowkit.ResourceKey{resourceTaskInstruction}, workflowkit.EffectContentProducer, 1, contentVerdicts(), artifactInput("generated_task_files"), artifactOutput("instruction")),
			stage(TaskTOMLGen, StageTaskGeneration, []string{GenerateTaskFiles}, "harborfactory.task_toml_generate", []workflowkit.ResourceKey{resourceTaskGeneratedFiles, resourceTaskDesign}, []workflowkit.ResourceKey{resourceTaskMetadata}, workflowkit.EffectContentProducer, 1, contentVerdicts(), artifactInput("generated_task_files"), artifactInput("task_proposal"), artifactOutput("task_toml")),
			stage(DockerfileGen, StageTaskGeneration, []string{GenerateTaskFiles}, "harborfactory.dockerfile_generate", []workflowkit.ResourceKey{resourceSourceSnapshot, resourceTaskDesign}, []workflowkit.ResourceKey{resourceTaskEnvironment}, workflowkit.EffectContentProducer, 1, contentVerdicts(), artifactInput("repo_prepared"), artifactInput("task_proposal"), artifactOutput("dockerfile")),
			gateStage(ContentReview, StageTaskGeneration, []string{InstructionGen, TaskTOMLGen, DockerfileGen}, ReviewContent, []workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment}, []workflowkit.ResourceKey{resourceReviewContent}, artifactInput("instruction"), artifactInput("task_toml"), artifactInput("dockerfile")),
			stage(SolveGen, StageTaskGeneration, []string{ContentReview}, "harborfactory.solve_generate", []workflowkit.ResourceKey{resourceTaskGeneratedFiles}, []workflowkit.ResourceKey{resourceTaskSolution}, workflowkit.EffectContentProducer, 1, contentVerdicts(), artifactInput("generated_task_files"), artifactOutput("solve_script")),
			stage(TestGen, StageTaskGeneration, []string{ContentReview}, "harborfactory.test_generate", []workflowkit.ResourceKey{resourceTaskGeneratedFiles}, []workflowkit.ResourceKey{resourceTaskTests}, workflowkit.EffectContentProducer, 1, contentVerdicts(), artifactInput("generated_task_files"), artifactOutput("test_script")),
			stage(TestsAnalysis, StageTaskGeneration, []string{ContentReview}, "harborfactory.tests_analysis", []workflowkit.ResourceKey{resourceTaskGeneratedFiles, resourceTaskDesign}, []workflowkit.ResourceKey{resourceTaskTestsAnalysis}, workflowkit.EffectContentProducer, 1, contentVerdicts(), artifactInput("generated_task_files"), artifactInput("task_proposal"), artifactOutput("tests_analysis")),
			gateStage(SolutionReview, StageTaskGeneration, []string{SolveGen, TestGen, TestsAnalysis}, ReviewSolutionVerifier, []workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis}, []workflowkit.ResourceKey{resourceReviewSolutionVerifier}, artifactInput("instruction"), artifactInput("solve_script"), artifactInput("test_script"), artifactInput("tests_analysis")),
			stage(MaterializeTask, StageTaskGeneration, []string{SolutionReview}, "harborfactory.materialize_task", []workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis, resourceReviewSolutionVerifier}, []workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest, resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis}, workflowkit.EffectContentMutator, 1, contentVerdicts(), artifactInput("instruction"), artifactInput("task_toml"), artifactInput("dockerfile"), artifactInput("solve_script"), artifactInput("test_script"), artifactInput("tests_analysis"), reviewDecisionInput("solution_review_decision"), artifactOutput("task_snapshot"), artifactOutput("task_digest")),
			stage(TaskRepair, StageFinalReview, []string{MaterializeTask}, "harborfactory.task_repair", []workflowkit.ResourceKey{resourceTaskWildcard, resourceFindingWildcard}, []workflowkit.ResourceKey{resourceTaskWildcard, resourceTaskDigest, resourceEvidenceRepair}, workflowkit.EffectContentMutator, 1, contentVerdicts(), artifactInput("task_snapshot"), artifactInput("repair_findings"), artifactOutput("repair_receipt"), artifactOutput("task_snapshot"), artifactOutput("task_digest")),
			stage(RuntimeSelfCheck, StageRuntimeVerify, []string{TaskRepair}, "harborfactory.runtime_self_check", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest}, []workflowkit.ResourceKey{resourceEvidenceRuntimeSelfCheck}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), artifactOutput("runtime_self_check_report")),
			stage(HarborVerify, StageRuntimeVerify, []string{RuntimeSelfCheck}, "harborfactory.verify_report_import", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest}, []workflowkit.ResourceKey{resourceEvidenceVerification}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), artifactOutput("verify_report")),
			stage(DockerBuild, StageRuntimeVerify, []string{RuntimeSelfCheck}, "harborfactory.docker_build", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest}, []workflowkit.ResourceKey{resourceEvidenceDockerBuild}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), artifactOutput("docker_build_report")),
			stage(InitialVerify, StageRuntimeVerify, []string{DockerBuild}, "harborfactory.initial_verify", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest, resourceEvidenceDockerBuild}, []workflowkit.ResourceKey{resourceEvidenceInitialVerify}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), artifactInput("docker_build_report"), artifactOutput("initial_verify_report")),
			stage(OracleVerify, StageRuntimeVerify, []string{InitialVerify}, "harborfactory.oracle_verify", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest, resourceEvidenceInitialVerify}, []workflowkit.ResourceKey{resourceEvidenceOracleVerify}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), artifactInput("initial_verify_report"), artifactOutput("oracle_verify_report")),
			stage(CodeEdgeLint, StageQuality, []string{OracleVerify}, "harborfactory.codeedge_lint", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest, resourceTaskTestsAnalysis, resourceEvidenceOracleVerify}, []workflowkit.ResourceKey{resourceEvidenceLint}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), artifactInput("tests_analysis"), artifactInput("oracle_verify_report"), artifactOutput("lint_report")),
			stage(QualityCheck, StageQuality, []string{CodeEdgeLint}, "harborfactory.quality_check", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest, resourceTaskTestsAnalysis, resourceEvidenceLint}, []workflowkit.ResourceKey{resourceEvidenceQuality}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), artifactInput("tests_analysis"), artifactInput("lint_report"), artifactOutput("quality_report")),
			stage(SimilarityCheck, StageSimilarity, []string{QualityCheck}, "harborfactory.similarity_check", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest, resourceTaskTestsAnalysis, resourceEvidenceQuality}, []workflowkit.ResourceKey{resourceEvidenceSimilarity}, workflowkit.EffectEvidenceOnly, 1, similarityVerdicts(), artifactInput("task_snapshot"), artifactInput("quality_report"), artifactOutput("similarity_report")),
			gateStage(FinalReview, StageFinalReview, []string{SimilarityCheck}, ReviewFinalQuality, []workflowkit.ResourceKey{resourceTaskDigest, resourceEvidenceVerification, resourceEvidenceLint, resourceEvidenceQuality, resourceEvidenceSimilarity}, []workflowkit.ResourceKey{resourceReviewFinalQuality}, artifactInput("lint_report"), artifactInput("verify_report"), artifactInput("quality_report"), artifactInput("similarity_report")),
			stage(HarborRunQwen, StageEvaluation, []string{FinalReview}, "harborfactory.harbor_run_qwen", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest, resourceReviewFinalQuality}, []workflowkit.ResourceKey{resourceEvidenceEvaluationQwen}, workflowkit.EffectEvidenceOnly, 1, evaluationVerdicts(), artifactInput("task_snapshot"), reviewDecisionInput("final_review_decision"), artifactOutput("qwen_trial_result"), artifactOutput("qwen_pass4_evidence")),
			stage(HarborRunOpus, StageEvaluation, []string{FinalReview}, "harborfactory.harbor_run_opus", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest, resourceReviewFinalQuality}, []workflowkit.ResourceKey{resourceEvidenceEvaluationOpus}, workflowkit.EffectEvidenceOnly, 1, evaluationVerdicts(), artifactInput("task_snapshot"), reviewDecisionInput("final_review_decision"), artifactOutput("opus_trial_result"), artifactOutput("opus_pass4_evidence")),
			gateStage(ResultReview, StageSubmission, []string{HarborRunQwen, HarborRunOpus}, ReviewModelResult, []workflowkit.ResourceKey{resourceTaskDigest, resourceReviewFinalQuality, resourceEvidenceEvaluationQwen, resourceEvidenceEvaluationOpus}, []workflowkit.ResourceKey{resourceReviewModelResult}, artifactInput("qwen_trial_result"), artifactInput("opus_trial_result")),
			stage(SubmissionLint, StageSubmission, []string{ResultReview}, "harborfactory.codeedge_lint", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest, resourceTaskTestsAnalysis, resourceReviewModelResult, resourceEvidenceEvaluationQwen, resourceEvidenceEvaluationOpus}, []workflowkit.ResourceKey{resourceEvidenceSubmissionLint}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), artifactInput("tests_analysis"), reviewDecisionInput("model_result_decision"), artifactOutput("submission_lint_report")),
			operatorOnlyLocalPackageStage([]string{SubmissionLint}, []workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest, resourceEvidenceSubmissionLint}, []workflowkit.ResourceKey{resourceDeliveryPackage}, artifactInput("task_snapshot"), artifactInput("submission_lint_report"), artifactOutput("package_bundle")),
		},
	}
}

func stage(key string, group StageGroup, dependencies []string, pluginID string, reads, writes []workflowkit.ResourceKey, effect workflowkit.StageEffect, requiredTurns int, verdicts workflowkit.VerdictPolicy, artifacts ...stageArtifact) StageDefinition {
	inputs, outputs := splitStageArtifacts(artifacts)
	return StageDefinition{
		Key:           workflowkit.StageKey(key),
		Version:       stageDescriptorVersion,
		Group:         group,
		Dependencies:  stageKeys(dependencies),
		Plugin:        PluginDescriptor{ID: pluginID, Version: pluginDescriptorVersion},
		Inputs:        inputs,
		Outputs:       outputs,
		ReadSet:       append([]workflowkit.ResourceKey(nil), reads...),
		WriteSet:      append([]workflowkit.ResourceKey(nil), writes...),
		Effect:        effect,
		Dispatch:      workflowkit.StageDispatchAutomatic,
		RequiredTurns: requiredTurns,
		Retry: workflowkit.RetryPolicy{Retryable: []workflowkit.FailureClass{
			workflowkit.FailureTransient,
			workflowkit.FailureTimeout,
			workflowkit.FailureRateLimited,
			workflowkit.FailureNetwork,
			workflowkit.FailureProcess,
		}},
		Verdicts:     verdicts,
		Reuse:        workflowkit.ReuseWhenInputsMatch,
		Capabilities: workflowkit.CapabilitySet{workflowkit.CapabilityCancel, workflowkit.CapabilityContinue},
	}
}

// operatorOnlyLocalPackageStage keeps local package readiness and dependencies
// in the frozen workflow contract while reserving actual materialization for
// LifecycleMutationService.Package/ReleaseService. A normal Run or
// continuation must never create a StageAttempt for this stage.
func operatorOnlyLocalPackageStage(dependencies []string, reads, writes []workflowkit.ResourceKey, artifacts ...stageArtifact) StageDefinition {
	definition := stage(Package, StageDelivery, dependencies, "harborfactory.local_package", reads, writes, workflowkit.EffectExternalSideEffect, 1, deliveryVerdicts(), artifacts...)
	definition.Dispatch = workflowkit.StageDispatchOperatorOnly
	definition.Retry = workflowkit.RetryPolicy{}
	definition.Reuse = workflowkit.ReuseNever
	return definition
}

func gateStage(key string, group StageGroup, dependencies []string, reviewKind ReviewKind, reads, writes []workflowkit.ResourceKey, artifacts ...stageArtifact) StageDefinition {
	definition := stage(key, group, dependencies, "harborfactory.human_gate", reads, writes, workflowkit.EffectEvidenceOnly, 1, gateVerdicts(), artifacts...)
	definition.Capabilities = workflowkit.CapabilitySet{workflowkit.CapabilityApprove}
	decision := reviewDecisionArtifact(reviewKind)
	definition.Gate = &GateDefinition{ReviewKind: reviewKind, DecisionArtifact: decision}
	definition.Outputs = append(definition.Outputs, decision)
	return definition
}

type stageArtifact struct {
	spec   workflowkit.ArtifactSpec
	input  bool
	output bool
}

func artifactInput(name string) stageArtifact {
	return artifactInputWithSchema(name, "harbor.artifact.v1")
}

func artifactInputWithSchema(name, schemaVersion string) stageArtifact {
	return stageArtifact{spec: workflowkit.ArtifactSpec{Name: name, SchemaVersion: schemaVersion, Required: true}, input: true}
}

func reviewDecisionInput(name string) stageArtifact {
	return stageArtifact{spec: workflowkit.ArtifactSpec{Name: name, SchemaVersion: "harbor.review-decision.v1", Required: true}, input: true}
}

func artifactOutput(name string) stageArtifact {
	return artifactOutputWithSchema(name, "harbor.artifact.v1")
}

func artifactOutputWithSchema(name, schemaVersion string) stageArtifact {
	return stageArtifact{spec: workflowkit.ArtifactSpec{Name: name, SchemaVersion: schemaVersion, Required: true}, output: true}
}

func reviewDecisionArtifact(kind ReviewKind) workflowkit.ArtifactSpec {
	name := ""
	switch kind {
	case ReviewTaskDirection:
		name = "task_review_decision"
	case ReviewContent:
		name = "content_review_decision"
	case ReviewSolutionVerifier:
		name = "solution_review_decision"
	case ReviewFinalQuality:
		name = "final_review_decision"
	case ReviewEvaluatorEvidence:
		name = "evaluator_evidence_handoff_decision"
	case ReviewModelResult:
		name = "model_result_decision"
	}
	return workflowkit.ArtifactSpec{Name: name, SchemaVersion: "harbor.review-decision.v1", Required: true}
}

func splitStageArtifacts(artifacts []stageArtifact) ([]workflowkit.ArtifactSpec, []workflowkit.ArtifactSpec) {
	inputs := make([]workflowkit.ArtifactSpec, 0, len(artifacts))
	outputs := make([]workflowkit.ArtifactSpec, 0, len(artifacts))
	for _, artifact := range artifacts {
		if artifact.input {
			inputs = append(inputs, artifact.spec)
		}
		if artifact.output {
			outputs = append(outputs, artifact.spec)
		}
	}
	return inputs, outputs
}

func stageKeys(keys []string) []workflowkit.StageKey {
	result := make([]workflowkit.StageKey, len(keys))
	for index, key := range keys {
		result[index] = workflowkit.StageKey(key)
	}
	return result
}

func passOnly() workflowkit.VerdictPolicy {
	return workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass}}
}

func contentVerdicts() workflowkit.VerdictPolicy {
	return workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass, workflowkit.VerdictNeedsRepair, workflowkit.VerdictReject}}
}

func checkVerdicts() workflowkit.VerdictPolicy {
	return workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass, workflowkit.VerdictNeedsRepair, workflowkit.VerdictReject, workflowkit.VerdictAdvisory}}
}

func similarityVerdicts() workflowkit.VerdictPolicy {
	return workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass, workflowkit.VerdictNeedsRepair, workflowkit.VerdictAdvisory}}
}

func gateVerdicts() workflowkit.VerdictPolicy {
	return workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass, workflowkit.VerdictNeedsRepair, workflowkit.VerdictReject}}
}

func evaluationVerdicts() workflowkit.VerdictPolicy {
	return workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass, workflowkit.VerdictNeedsRepair, workflowkit.VerdictAdvisory}}
}

func deliveryVerdicts() workflowkit.VerdictPolicy {
	return workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass, workflowkit.VerdictNeedsRepair, workflowkit.VerdictReject}}
}

// Validate proves that the catalog belongs to one exact closed template and
// implements that template's complete stage topology. Standard and CodeEdge
// Phase-1 intentionally have different closed shapes; neither may be treated
// as a permissive subset or a fallback for the other.
func (catalog StageCatalog) Validate() error {
	policy, err := catalogPolicyFor(catalog.Template)
	if err != nil {
		return err
	}
	if strings.TrimSpace(catalog.ID) == "" {
		return fmt.Errorf("%w: catalog id is required", errInvalidCatalog)
	}
	if strings.TrimSpace(catalog.Version) == "" {
		return fmt.Errorf("%w: catalog version is required", errInvalidCatalog)
	}
	if catalog.ID != policy.catalogID || catalog.Version != policy.catalogVersion {
		return fmt.Errorf("%w: catalog %s@%s does not match template %s@%s catalog identity %s@%s", errInvalidCatalog, catalog.ID, catalog.Version, catalog.Template.ID, catalog.Template.Version, policy.catalogID, policy.catalogVersion)
	}
	if len(catalog.Stages) == 0 {
		return fmt.Errorf("%w: catalog has no stages", errInvalidCatalog)
	}

	expectedOrder := policy.stageOrder
	expectedNodes := make(map[workflowkit.StageKey]struct{}, len(expectedOrder))
	for _, node := range expectedOrder {
		expectedNodes[node] = struct{}{}
	}
	stages := make(map[workflowkit.StageKey]StageDefinition, len(catalog.Stages))
	groups := make(map[StageGroup]struct{}, len(policy.groups))
	actualGates := make(map[workflowkit.StageKey]struct{})
	for _, definition := range catalog.Stages {
		if err := definition.validate(); err != nil {
			return err
		}
		if _, expected := expectedNodes[definition.Key]; !expected {
			return fmt.Errorf("%w: unknown Harbor node %q", errInvalidCatalog, definition.Key)
		}
		if _, duplicate := stages[definition.Key]; duplicate {
			return fmt.Errorf("%w: duplicate Harbor node %q", errInvalidCatalog, definition.Key)
		}
		stages[definition.Key] = definition
		groups[definition.Group] = struct{}{}
		if definition.IsGate() {
			actualGates[definition.Key] = struct{}{}
		}
	}
	for node := range expectedNodes {
		if _, present := stages[node]; !present {
			return fmt.Errorf("%w: template %s@%s stage %q is not cataloged", errInvalidCatalog, catalog.Template.ID, catalog.Template.Version, node)
		}
	}
	if policy.requiresOperatorOnlyPackage {
		packageStage := stages[workflowkit.StageKey(Package)]
		if !packageStage.Dispatch.IsOperatorOnly() {
			return fmt.Errorf("%w: local package stage must be operator-only", errInvalidCatalog)
		}
	}
	if len(groups) != len(policy.groups) {
		return fmt.Errorf("%w: template %s@%s got %d stage groups; want %d", errInvalidCatalog, catalog.Template.ID, catalog.Template.Version, len(groups), len(policy.groups))
	}
	for _, group := range policy.groups {
		if _, present := groups[group]; !present {
			return fmt.Errorf("%w: template %s@%s stage group %q has no node", errInvalidCatalog, catalog.Template.ID, catalog.Template.Version, group)
		}
	}
	if err := validateDependencies(stages); err != nil {
		return err
	}
	if err := validateGateCoverageFor(actualGates, policy.gates); err != nil {
		return err
	}
	if err := policy.validateTopology(stages); err != nil {
		return err
	}
	if err := policy.validateStageDefinitions(stages); err != nil {
		return err
	}
	return nil
}

func (definition StageDefinition) validate() error {
	if strings.TrimSpace(string(definition.Key)) == "" {
		return fmt.Errorf("%w: stage key is required", errInvalidCatalog)
	}
	if strings.TrimSpace(definition.Version) == "" {
		return fmt.Errorf("%w: stage %q version is required", errInvalidCatalog, definition.Key)
	}
	if !definition.Group.valid() {
		return fmt.Errorf("%w: stage %q has unsupported group %q", errInvalidCatalog, definition.Key, definition.Group)
	}
	if err := definition.Plugin.validate(); err != nil {
		return fmt.Errorf("%w: stage %q: %v", errInvalidCatalog, definition.Key, err)
	}
	if definition.RequiredTurns < 1 {
		return fmt.Errorf("%w: stage %q must require at least one turn", errInvalidCatalog, definition.Key)
	}
	if err := validateUniqueStageKeys("dependency", definition.Dependencies); err != nil {
		return fmt.Errorf("%w: stage %q: %v", errInvalidCatalog, definition.Key, err)
	}
	for _, dependency := range definition.Dependencies {
		if dependency == definition.Key {
			return fmt.Errorf("%w: stage %q cannot depend on itself", errInvalidCatalog, definition.Key)
		}
	}
	if err := validateArtifactSpecs("input", definition.Inputs); err != nil {
		return fmt.Errorf("%w: stage %q: %v", errInvalidCatalog, definition.Key, err)
	}
	if err := validateArtifactSpecs("output", definition.Outputs); err != nil {
		return fmt.Errorf("%w: stage %q: %v", errInvalidCatalog, definition.Key, err)
	}
	if err := validateUniqueResources("read resource", definition.ReadSet); err != nil {
		return fmt.Errorf("%w: stage %q: %v", errInvalidCatalog, definition.Key, err)
	}
	if err := validateUniqueResources("write resource", definition.WriteSet); err != nil {
		return fmt.Errorf("%w: stage %q: %v", errInvalidCatalog, definition.Key, err)
	}
	if !validEffect(definition.Effect) {
		return fmt.Errorf("%w: stage %q has unsupported effect %q", errInvalidCatalog, definition.Key, definition.Effect)
	}
	if err := definition.Dispatch.Validate(); err != nil {
		return fmt.Errorf("%w: stage %q: %v", errInvalidCatalog, definition.Key, err)
	}
	if definition.Effect == workflowkit.EffectReadOnly && len(definition.WriteSet) > 0 {
		return fmt.Errorf("%w: read-only stage %q declares writes", errInvalidCatalog, definition.Key)
	}
	if err := validateRetryPolicy(definition.Retry); err != nil {
		return fmt.Errorf("%w: stage %q: %v", errInvalidCatalog, definition.Key, err)
	}
	if err := validateVerdictPolicy(definition.Verdicts); err != nil {
		return fmt.Errorf("%w: stage %q: %v", errInvalidCatalog, definition.Key, err)
	}
	if definition.Reuse != workflowkit.ReuseNever && definition.Reuse != workflowkit.ReuseWhenInputsMatch {
		return fmt.Errorf("%w: stage %q has unsupported reuse policy %q", errInvalidCatalog, definition.Key, definition.Reuse)
	}
	if err := validateCapabilities(definition.Capabilities); err != nil {
		return fmt.Errorf("%w: stage %q: %v", errInvalidCatalog, definition.Key, err)
	}
	if err := validateGateDefinition(definition); err != nil {
		return err
	}
	return nil
}

func validateDependencies(stages map[workflowkit.StageKey]StageDefinition) error {
	inDegree := make(map[workflowkit.StageKey]int, len(stages))
	dependents := make(map[workflowkit.StageKey][]workflowkit.StageKey, len(stages))
	for key, stage := range stages {
		inDegree[key] = len(stage.Dependencies)
		for _, dependency := range stage.Dependencies {
			if _, present := stages[dependency]; !present {
				return fmt.Errorf("%w: stage %q depends on missing node %q", errInvalidCatalog, key, dependency)
			}
			dependents[dependency] = append(dependents[dependency], key)
		}
	}
	ready := make([]workflowkit.StageKey, 0, len(stages))
	for key, degree := range inDegree {
		if degree == 0 {
			ready = append(ready, key)
		}
	}
	seen := 0
	for len(ready) > 0 {
		sort.Slice(ready, func(left, right int) bool { return ready[left] < ready[right] })
		key := ready[0]
		ready = ready[1:]
		seen++
		for _, dependent := range dependents[key] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				ready = append(ready, dependent)
			}
		}
	}
	if seen != len(stages) {
		return fmt.Errorf("%w: dependency graph contains a cycle", errInvalidCatalog)
	}
	return nil
}

func validateGateCoverage(actual map[workflowkit.StageKey]struct{}) error {
	return validateGateCoverageFor(actual, []workflowkit.StageKey{
		workflowkit.StageKey(TaskReview),
		workflowkit.StageKey(ContentReview),
		workflowkit.StageKey(SolutionReview),
		workflowkit.StageKey(FinalReview),
		workflowkit.StageKey(ResultReview),
	})
}

func validateGateCoverageFor(actual map[workflowkit.StageKey]struct{}, expectedKeys []workflowkit.StageKey) error {
	expected := make(map[workflowkit.StageKey]struct{}, len(expectedKeys))
	for _, key := range expectedKeys {
		expected[key] = struct{}{}
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("%w: got %d review gates; want %d", errInvalidCatalog, len(actual), len(expected))
	}
	for key := range expected {
		if _, present := actual[key]; !present {
			return fmt.Errorf("%w: review gate %q is missing", errInvalidCatalog, key)
		}
	}
	return nil
}

func validateGateDefinition(stage StageDefinition) error {
	if stage.Gate == nil {
		if stage.Capabilities.Has(workflowkit.CapabilityApprove) {
			return fmt.Errorf("%w: non-gate stage %q declares approve capability", errInvalidCatalog, stage.Key)
		}
		return nil
	}
	if !stage.Gate.ReviewKind.valid() {
		return fmt.Errorf("%w: gate %q has invalid review kind %q", errInvalidCatalog, stage.Key, stage.Gate.ReviewKind)
	}
	if stage.Effect != workflowkit.EffectEvidenceOnly {
		return fmt.Errorf("%w: gate %q must be evidence-only", errInvalidCatalog, stage.Key)
	}
	if !stage.Capabilities.Has(workflowkit.CapabilityApprove) {
		return fmt.Errorf("%w: gate %q must declare approve capability", errInvalidCatalog, stage.Key)
	}
	if !stage.AllowsVerdict(workflowkit.VerdictPass) || !stage.AllowsVerdict(workflowkit.VerdictNeedsRepair) || !stage.AllowsVerdict(workflowkit.VerdictReject) {
		return fmt.Errorf("%w: gate %q must support approve, repair, and reject decisions", errInvalidCatalog, stage.Key)
	}
	decision := stage.Gate.DecisionArtifact
	if strings.TrimSpace(decision.Name) == "" || decision.SchemaVersion != "harbor.review-decision.v1" || !decision.Required {
		return fmt.Errorf("%w: gate %q has invalid frozen decision artifact", errInvalidCatalog, stage.Key)
	}
	if len(stage.Outputs) != 1 || stage.Outputs[0] != decision {
		return fmt.Errorf("%w: gate %q must output exactly its frozen decision artifact", errInvalidCatalog, stage.Key)
	}
	return nil
}

func validateUniqueStageKeys(label string, values []workflowkit.StageKey) error {
	seen := make(map[workflowkit.StageKey]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(string(value)) == "" {
			return fmt.Errorf("%s is required", label)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("duplicate %s %q", label, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateArtifactSpecs(label string, specs []workflowkit.ArtifactSpec) error {
	seen := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		if strings.TrimSpace(spec.Name) == "" || strings.TrimSpace(spec.SchemaVersion) == "" {
			return fmt.Errorf("%s artifact name and schema version are required", label)
		}
		if _, duplicate := seen[spec.Name]; duplicate {
			return fmt.Errorf("duplicate %s artifact %q", label, spec.Name)
		}
		seen[spec.Name] = struct{}{}
	}
	return nil
}

func validateUniqueResources(label string, resources []workflowkit.ResourceKey) error {
	seen := make(map[workflowkit.ResourceKey]struct{}, len(resources))
	for _, resource := range resources {
		if strings.TrimSpace(string(resource)) == "" {
			return fmt.Errorf("%s is required", label)
		}
		if _, duplicate := seen[resource]; duplicate {
			return fmt.Errorf("duplicate %s %q", label, resource)
		}
		seen[resource] = struct{}{}
	}
	return nil
}

func validEffect(effect workflowkit.StageEffect) bool {
	switch effect {
	case workflowkit.EffectReadOnly, workflowkit.EffectEvidenceOnly, workflowkit.EffectContentProducer, workflowkit.EffectContentMutator, workflowkit.EffectExternalSideEffect:
		return true
	default:
		return false
	}
}

func validateRetryPolicy(policy workflowkit.RetryPolicy) error {
	seen := make(map[workflowkit.FailureClass]struct{}, len(policy.Retryable))
	for _, failure := range policy.Retryable {
		switch failure {
		case workflowkit.FailureTransient, workflowkit.FailureTimeout, workflowkit.FailureRateLimited, workflowkit.FailureNetwork, workflowkit.FailureProcess:
		default:
			return fmt.Errorf("failure class %q is not retryable", failure)
		}
		if _, duplicate := seen[failure]; duplicate {
			return fmt.Errorf("duplicate retryable failure class %q", failure)
		}
		seen[failure] = struct{}{}
	}
	return nil
}

func validateVerdictPolicy(policy workflowkit.VerdictPolicy) error {
	if len(policy.Allowed) == 0 {
		return errors.New("at least one verdict is required")
	}
	seen := make(map[workflowkit.Verdict]struct{}, len(policy.Allowed))
	for _, verdict := range policy.Allowed {
		switch verdict {
		case workflowkit.VerdictPass, workflowkit.VerdictNeedsRepair, workflowkit.VerdictReject, workflowkit.VerdictAdvisory:
		default:
			return fmt.Errorf("unsupported verdict %q", verdict)
		}
		if _, duplicate := seen[verdict]; duplicate {
			return fmt.Errorf("duplicate verdict %q", verdict)
		}
		seen[verdict] = struct{}{}
	}
	return nil
}

func validateCapabilities(capabilities workflowkit.CapabilitySet) error {
	seen := make(map[workflowkit.Capability]struct{}, len(capabilities))
	for _, capability := range capabilities {
		switch capability {
		case workflowkit.CapabilityCancel, workflowkit.CapabilityContinue, workflowkit.CapabilityApprove:
		default:
			return fmt.Errorf("unsupported capability %q", capability)
		}
		if _, duplicate := seen[capability]; duplicate {
			return fmt.Errorf("duplicate capability %q", capability)
		}
		seen[capability] = struct{}{}
	}
	return nil
}

// Fingerprint returns a canonical hash. Declaration ordering and set ordering
// do not alter it, while any policy, plugin, resource, or gate change does.
func (catalog StageCatalog) Fingerprint() (workflowkit.Fingerprint, error) {
	if err := catalog.Validate(); err != nil {
		return "", err
	}
	canonical := catalog.Clone()
	sort.Slice(canonical.Stages, func(left, right int) bool {
		return canonical.Stages[left].Key < canonical.Stages[right].Key
	})
	for index := range canonical.Stages {
		canonicalizeStage(&canonical.Stages[index])
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: encode catalog: %v", errInvalidCatalog, err)
	}
	return workflowkit.FingerprintBytes("harbor.workflowadapter.stage-catalog.v2", encoded)
}

func canonicalizeStage(stage *StageDefinition) {
	sort.Slice(stage.Dependencies, func(left, right int) bool { return stage.Dependencies[left] < stage.Dependencies[right] })
	sort.Slice(stage.Inputs, func(left, right int) bool { return stage.Inputs[left].Name < stage.Inputs[right].Name })
	sort.Slice(stage.Outputs, func(left, right int) bool { return stage.Outputs[left].Name < stage.Outputs[right].Name })
	sort.Slice(stage.ReadSet, func(left, right int) bool { return stage.ReadSet[left] < stage.ReadSet[right] })
	sort.Slice(stage.WriteSet, func(left, right int) bool { return stage.WriteSet[left] < stage.WriteSet[right] })
	sort.Slice(stage.Retry.Retryable, func(left, right int) bool { return stage.Retry.Retryable[left] < stage.Retry.Retryable[right] })
	sort.Slice(stage.Verdicts.Allowed, func(left, right int) bool { return stage.Verdicts.Allowed[left] < stage.Verdicts.Allowed[right] })
	sort.Slice(stage.Capabilities, func(left, right int) bool { return stage.Capabilities[left] < stage.Capabilities[right] })
}

const (
	resourceSourceRepository   workflowkit.ResourceKey = "source/repository"
	resourceSourceSnapshot     workflowkit.ResourceKey = "source/snapshot"
	resourceAnalysisRepository workflowkit.ResourceKey = "analysis/repository"
	resourceTaskDesign         workflowkit.ResourceKey = "task/design"
	resourceTaskGeneratedFiles workflowkit.ResourceKey = "task/generated-files"
	resourceTaskInstruction    workflowkit.ResourceKey = "task/instruction"
	resourceTaskMetadata       workflowkit.ResourceKey = "task/metadata"
	resourceTaskEnvironment    workflowkit.ResourceKey = "task/environment"
	// resourceAuthoringEnvironmentPolicy is the immutable per-session base
	// image contract. It belongs only to the pre-materialization authoring
	// graph; no task-revision workflow may infer it from a Dockerfile.
	resourceAuthoringEnvironmentPolicy workflowkit.ResourceKey = "authoring/environment-policy"
	// resourceAuthoringBrief is the immutable caller-selected task direction.
	// It is separate from task/design because no authoring stage may rewrite it.
	resourceAuthoringBrief    workflowkit.ResourceKey = "authoring/brief"
	resourceTaskSolution      workflowkit.ResourceKey = "task/solution"
	resourceTaskTests         workflowkit.ResourceKey = "task/tests"
	resourceTaskTestsAnalysis workflowkit.ResourceKey = "task/tests-analysis"
	resourceTaskSnapshot      workflowkit.ResourceKey = "task/snapshot"
	resourceTaskDigest        workflowkit.ResourceKey = "task/digest"
	// resourceAuthoringTaskHandoff is written exactly once by the closed
	// AuthoringSession materialize_task stage. A task-bound child Run consumes
	// its immutable receipt; it must never keep executing under the source
	// session subject after this write.
	resourceAuthoringTaskHandoff         workflowkit.ResourceKey = "authoring/task-handoff"
	resourceAuthoringTaskAdmission       workflowkit.ResourceKey = "authoring/task-admission"
	resourceTaskWildcard                 workflowkit.ResourceKey = "task/**"
	resourceFindingWildcard              workflowkit.ResourceKey = "finding/**"
	resourceReviewTaskDirection          workflowkit.ResourceKey = "review/task-direction"
	resourceReviewContent                workflowkit.ResourceKey = "review/content"
	resourceReviewSolutionVerifier       workflowkit.ResourceKey = "review/solution-verifier"
	resourceReviewFinalQuality           workflowkit.ResourceKey = "review/final-quality"
	resourceReviewEvaluatorEvidence      workflowkit.ResourceKey = "review/evaluator-evidence-handoff"
	resourceReviewModelResult            workflowkit.ResourceKey = "review/model-result"
	resourceEvidenceRepoPrepare          workflowkit.ResourceKey = "evidence/repo-prepare"
	resourceEvidenceTaskLayout           workflowkit.ResourceKey = "evidence/codeedge/task-layout"
	resourceEvidenceRepoProvenance       workflowkit.ResourceKey = "evidence/codeedge/repo-provenance"
	resourceEvidenceEnvironmentIsolation workflowkit.ResourceKey = "evidence/codeedge/environment-isolation"
	resourceEvidenceTestsAnalysis        workflowkit.ResourceKey = "evidence/codeedge/tests-analysis"
	resourceEvidenceRepair               workflowkit.ResourceKey = "evidence/repair"
	resourceEvidenceRuntimeSelfCheck     workflowkit.ResourceKey = "evidence/runtime-self-check"
	resourceEvidenceVerification         workflowkit.ResourceKey = "evidence/verification"
	resourceEvidenceDockerBuild          workflowkit.ResourceKey = "evidence/docker-build"
	resourceEvidenceInitialVerify        workflowkit.ResourceKey = "evidence/initial-verify"
	resourceEvidenceOracleVerify         workflowkit.ResourceKey = "evidence/oracle-verify"
	resourceEvidenceLint                 workflowkit.ResourceKey = "evidence/lint"
	resourceEvidenceQuality              workflowkit.ResourceKey = "evidence/quality"
	resourceEvidenceSimilarity           workflowkit.ResourceKey = "evidence/similarity"
	resourceEvidenceEvaluationQwen       workflowkit.ResourceKey = "evidence/evaluation/qwen"
	resourceEvidenceEvaluationOpus       workflowkit.ResourceKey = "evidence/evaluation/opus"
	resourceEvidenceEvaluatorHandoff     workflowkit.ResourceKey = "evidence/evaluation/handoff"
	resourceEvidenceSubmissionLint       workflowkit.ResourceKey = "evidence/submission-lint"
	resourceDeliveryPublish              workflowkit.ResourceKey = "delivery/publish"
	resourceDeliveryPackage              workflowkit.ResourceKey = "delivery/package"
)
