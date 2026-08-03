package workflowadapter

import (
	"fmt"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// StandardAuthoringWorkflowTemplateID identifies the only source-session
	// workflow. It creates exactly one sealed task revision and then ends.
	StandardAuthoringWorkflowTemplateID = "harbor.standard-authoring"

	// StandardAuthoringContractTemplateVersion is a hard cutover. No 2.x
	// catalog, profile, operation binding, or recovery route is executable.
	StandardAuthoringContractTemplateVersion = "3.0.0"

	standardAuthoringCatalogID      = "harbor.standard-authoring-stage-catalog"
	standardAuthoringCatalogVersion = StandardAuthoringContractTemplateVersion

	StandardAuthoringAuthoringLoopMaxTurns = 3
	// StandardAuthoringRepairMaxTurns bounds the repair conversation. The
	// authoring repair agent has repeatedly demonstrated one-validation-per-turn
	// turn burn (calling harbor_validate_candidate again inside a rejected
	// turn), so the bound must leave room for several real validation rounds
	// after the protocol rejects the redundant in-turn attempts.
	StandardAuthoringRepairMaxTurns        = 12
	// Kept as a source-compatible name for callers that only need the bounded
	// authoring conversation limit. The retired task_design stage itself is not
	// present in the 3.0 catalog.
	StandardAuthoringTaskDesignMaxTurns = StandardAuthoringAuthoringLoopMaxTurns

	StandardAuthoringMaterializationReceiptArtifact      = "materialization_receipt"
	StandardAuthoringMaterializationReceiptSchemaVersion = "harbor.standard-authoring-materialization-receipt.v1"

	standardAuthoringPackageAdmissionReportArtifact      = "codeedge_package_admission_report"
	standardAuthoringPackageAdmissionReportSchemaVersion = "harbor.standard-authoring-task-package-admission.v1"

	standardAuthoringCandidateSnapshotArtifact            = "candidate_snapshot"
	standardAuthoringCandidateSnapshotSchemaVersion       = workflowkit.CandidateSnapshotFormat
	standardAuthoringVerificationContractArtifact         = "verification_contract"
	standardAuthoringVerificationContractSchemaVersion    = "harbor.verification-contract.v1"
	standardAuthoringValidationReceiptArtifact            = "validation_receipt"
	standardAuthoringValidationReceiptSchemaVersion       = workflowkit.ValidationReceiptFormat
	standardAuthoringValidationRepairContextArtifact      = StandardAuthoringValidationRepairContextArtifact
	standardAuthoringValidationRepairContextSchemaVersion = StandardAuthoringValidationRepairContextSchemaVersion
	standardAuthoringWorkflowRepairLedgerArtifact         = "workflow_repair_ledger"
	standardAuthoringWorkflowRepairLedgerSchemaVersion    = workflowkit.WorkflowRepairLedgerFormat
	standardAuthoringFinalAttestationArtifact             = "final_attestation"
	standardAuthoringFinalAttestationSchemaVersion        = "harbor.standard-authoring-final-attestation.v1"
)

var standardAuthoringStageOrder = []workflowkit.StageKey{
	workflowkit.StageKey(RepoPrepare),
	workflowkit.StageKey(RepoStructureResearch),
	workflowkit.StageKey(TestRuntimeResearch),
	workflowkit.StageKey(VerifierThreatResearch),
	workflowkit.StageKey(TaskSynthesis),
	workflowkit.StageKey(TaskReview),
	workflowkit.StageKey(AuthoringLoop),
	workflowkit.StageKey(HostCandidateVerify),
	workflowkit.StageKey(TestQualityCritic),
	workflowkit.StageKey(SolutionIntegrityCritic),
	workflowkit.StageKey(AuthoringRepair),
	workflowkit.StageKey(ContentReview),
	workflowkit.StageKey(SolutionReview),
	workflowkit.StageKey(FinalAttestation),
	workflowkit.StageKey(CodeEdgePackageAdmission),
	workflowkit.StageKey(MaterializeTask),
}

var standardAuthoringGroups = []StageGroup{
	StageSourcePrepare,
	StageTaskAnalysis,
	StageTaskDesign,
	StageTaskGeneration,
	StageRuntimeVerify,
	StageQuality,
	StageFinalReview,
}

func StandardAuthoringContractTemplateReference() TemplateReference {
	return TemplateReference{ID: StandardAuthoringWorkflowTemplateID, Version: StandardAuthoringContractTemplateVersion}
}

// StandardAuthoringCurrentTemplateReference returns the only executable
// Standard Authoring template in this binary.
func StandardAuthoringCurrentTemplateReference() TemplateReference {
	return StandardAuthoringContractTemplateReference()
}

// IsStandardAuthoringWorkflowTemplate admits only the current hard-cutover
// descriptor. Completed legacy records remain audit data in storage, never an
// executable template.
func IsStandardAuthoringWorkflowTemplate(reference TemplateReference) bool {
	return reference.Equal(StandardAuthoringContractTemplateReference())
}

func StandardAuthoringStageOrder() []workflowkit.StageKey {
	return append([]workflowkit.StageKey(nil), standardAuthoringStageOrder...)
}

func StandardAuthoringStageOrderForTemplate(reference TemplateReference) ([]workflowkit.StageKey, error) {
	if reference.Equal(StandardAuthoringContractTemplateReference()) {
		return StandardAuthoringStageOrder(), nil
	}
	return nil, fmt.Errorf("Standard authoring template %s@%s is not installed", reference.ID, reference.Version)
}

func standardAuthoringStageGroups() []StageGroup {
	return append([]StageGroup(nil), standardAuthoringGroups...)
}

func standardAuthoringDependencies() map[workflowkit.StageKey][]workflowkit.StageKey {
	return map[workflowkit.StageKey][]workflowkit.StageKey{
		workflowkit.StageKey(RepoPrepare):              nil,
		workflowkit.StageKey(RepoStructureResearch):    {workflowkit.StageKey(RepoPrepare)},
		workflowkit.StageKey(TestRuntimeResearch):      {workflowkit.StageKey(RepoPrepare)},
		workflowkit.StageKey(VerifierThreatResearch):   {workflowkit.StageKey(RepoPrepare)},
		workflowkit.StageKey(TaskSynthesis):            {workflowkit.StageKey(RepoStructureResearch), workflowkit.StageKey(TestRuntimeResearch), workflowkit.StageKey(VerifierThreatResearch)},
		workflowkit.StageKey(TaskReview):               {workflowkit.StageKey(TaskSynthesis)},
		workflowkit.StageKey(AuthoringLoop):            {workflowkit.StageKey(TaskReview)},
		workflowkit.StageKey(HostCandidateVerify):      {workflowkit.StageKey(AuthoringLoop)},
		workflowkit.StageKey(TestQualityCritic):        {workflowkit.StageKey(HostCandidateVerify)},
		workflowkit.StageKey(SolutionIntegrityCritic):  {workflowkit.StageKey(HostCandidateVerify)},
		workflowkit.StageKey(AuthoringRepair):          {workflowkit.StageKey(HostCandidateVerify), workflowkit.StageKey(TestQualityCritic), workflowkit.StageKey(SolutionIntegrityCritic)},
		workflowkit.StageKey(ContentReview):            {workflowkit.StageKey(AuthoringRepair)},
		workflowkit.StageKey(SolutionReview):           {workflowkit.StageKey(AuthoringRepair)},
		workflowkit.StageKey(FinalAttestation):         {workflowkit.StageKey(ContentReview), workflowkit.StageKey(SolutionReview)},
		workflowkit.StageKey(CodeEdgePackageAdmission): {workflowkit.StageKey(FinalAttestation)},
		workflowkit.StageKey(MaterializeTask):          {workflowkit.StageKey(CodeEdgePackageAdmission)},
	}
}

// StandardAuthoringContractWorkflowTemplate returns the complete 3.0
// source-session descriptor. It is direct rather than adapted from 2.x, so
// no old stage key, workspace protocol, or recovery branch can be reactivated.
func StandardAuthoringContractWorkflowTemplate() WorkflowTemplate {
	return WorkflowTemplate{
		ID:          StandardAuthoringWorkflowTemplateID,
		Version:     StandardAuthoringContractTemplateVersion,
		Catalog:     StandardAuthoringContractStageCatalog(),
		QuotaPolicy: StandardAuthoringContractQuotaPolicy(),
	}
}

func StandardAuthoringCurrentWorkflowTemplate() WorkflowTemplate {
	return StandardAuthoringContractWorkflowTemplate()
}

// StandardAuthoringContractStageCatalog freezes a single 3.0 orchestration
// path. Agent stages carry their role and host workspace binding in the
// descriptor; deployment assets only choose the attested implementation.
func StandardAuthoringContractStageCatalog() StageCatalog {
	stages := []StageDefinition{
		stage(RepoPrepare, StageSourcePrepare, nil, "harborfactory.repo_prepare",
			[]workflowkit.ResourceKey{resourceSourceRepository},
			[]workflowkit.ResourceKey{resourceSourceSnapshot, resourceEvidenceRepoPrepare},
			workflowkit.EffectEvidenceOnly, 1, passOnly(), artifactOutput("repo_prepared")),
		stage(RepoStructureResearch, StageTaskAnalysis, []string{RepoPrepare}, "harborfactory.repo_structure_research",
			[]workflowkit.ResourceKey{resourceSourceSnapshot}, []workflowkit.ResourceKey{"evidence/research/repo-structure"},
			workflowkit.EffectEvidenceOnly, 3, passOnly(), artifactInput("repo_prepared"), artifactOutputWithSchema("repo_structure_evidence", "workflowkit.workflow-evidence.v1")),
		stage(TestRuntimeResearch, StageTaskAnalysis, []string{RepoPrepare}, "harborfactory.test_runtime_research",
			[]workflowkit.ResourceKey{resourceSourceSnapshot}, []workflowkit.ResourceKey{"evidence/research/test-runtime"},
			workflowkit.EffectEvidenceOnly, 3, passOnly(), artifactInput("repo_prepared"), artifactOutputWithSchema("test_runtime_evidence", "workflowkit.workflow-evidence.v1")),
		stage(VerifierThreatResearch, StageTaskAnalysis, []string{RepoPrepare}, "harborfactory.verifier_threat_research",
			[]workflowkit.ResourceKey{resourceSourceSnapshot}, []workflowkit.ResourceKey{"evidence/research/verifier-threat"},
			workflowkit.EffectEvidenceOnly, 3, passOnly(), artifactInput("repo_prepared"), artifactOutputWithSchema("verifier_threat_evidence", "workflowkit.workflow-evidence.v1")),
		stage(TaskSynthesis, StageTaskDesign, []string{RepoStructureResearch, TestRuntimeResearch, VerifierThreatResearch}, "harborfactory.task_synthesis",
			[]workflowkit.ResourceKey{"evidence/research/repo-structure", "evidence/research/test-runtime", "evidence/research/verifier-threat"},
			[]workflowkit.ResourceKey{resourceTaskDesign, "task/verification-contract"}, workflowkit.EffectContentProducer, 4, contentVerdicts(),
			artifactInputWithSchema("repo_structure_evidence", "workflowkit.workflow-evidence.v1"), artifactInputWithSchema("test_runtime_evidence", "workflowkit.workflow-evidence.v1"), artifactInputWithSchema("verifier_threat_evidence", "workflowkit.workflow-evidence.v1"),
			artifactOutputWithSchema("task_specification", "harbor.task-specification.v1"), artifactOutputWithSchema(standardAuthoringVerificationContractArtifact, standardAuthoringVerificationContractSchemaVersion)),
		gateStage(TaskReview, StageTaskDesign, []string{TaskSynthesis}, ReviewTaskDirection,
			[]workflowkit.ResourceKey{resourceTaskDesign, "task/verification-contract"}, []workflowkit.ResourceKey{resourceReviewTaskDirection},
			artifactInputWithSchema("task_specification", "harbor.task-specification.v1"), artifactInputWithSchema(standardAuthoringVerificationContractArtifact, standardAuthoringVerificationContractSchemaVersion)),
		stage(AuthoringLoop, StageTaskGeneration, []string{TaskReview}, "harborfactory.authoring_loop",
			[]workflowkit.ResourceKey{resourceSourceSnapshot, resourceTaskDesign, "task/verification-contract", resourceReviewTaskDirection},
			[]workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis, "task/candidate"},
			workflowkit.EffectContentMutator, StandardAuthoringAuthoringLoopMaxTurns, contentVerdicts(),
			artifactInput("repo_prepared"), artifactInputWithSchema("task_specification", "harbor.task-specification.v1"), artifactInputWithSchema(standardAuthoringVerificationContractArtifact, standardAuthoringVerificationContractSchemaVersion), reviewDecisionInput("task_review_decision"),
			artifactOutput("instruction"), artifactOutput("task_toml"), artifactOutput("dockerfile"), artifactOutput("solve_script"), artifactOutput("test_script"), artifactOutput("tests_analysis"), artifactOutputWithSchema(standardAuthoringCandidateSnapshotArtifact, standardAuthoringCandidateSnapshotSchemaVersion)),
		stage(HostCandidateVerify, StageRuntimeVerify, []string{AuthoringLoop}, "harborfactory.host_candidate_verify",
			[]workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis, "task/candidate"},
			[]workflowkit.ResourceKey{"evidence/candidate-validation"},
			workflowkit.EffectContentMutator, 1, contentVerdicts(),
			artifactInput("instruction"), artifactInput("task_toml"), artifactInput("dockerfile"), artifactInput("solve_script"), artifactInput("test_script"), artifactInput("tests_analysis"), artifactInputWithSchema(standardAuthoringCandidateSnapshotArtifact, standardAuthoringCandidateSnapshotSchemaVersion),
			artifactInputWithSchema(standardAuthoringVerificationContractArtifact, standardAuthoringVerificationContractSchemaVersion),
			artifactOutputWithSchema(standardAuthoringValidationReceiptArtifact, standardAuthoringValidationReceiptSchemaVersion),
			artifactOutputWithSchema(standardAuthoringValidationRepairContextArtifact, standardAuthoringValidationRepairContextSchemaVersion)),
		stage(TestQualityCritic, StageQuality, []string{HostCandidateVerify}, "harborfactory.test_quality_critic",
			[]workflowkit.ResourceKey{"evidence/candidate-validation"}, []workflowkit.ResourceKey{"finding/test-quality"},
			workflowkit.EffectEvidenceOnly, 3, contentVerdicts(), artifactInput("instruction"), artifactInput("task_toml"), artifactInput("dockerfile"), artifactInput("solve_script"), artifactInput("test_script"), artifactInput("tests_analysis"), artifactInputWithSchema(standardAuthoringCandidateSnapshotArtifact, standardAuthoringCandidateSnapshotSchemaVersion), artifactInputWithSchema(standardAuthoringValidationReceiptArtifact, standardAuthoringValidationReceiptSchemaVersion), artifactOutputWithSchema("test_quality_finding", workflowkit.WorkflowFindingFormat)),
		stage(SolutionIntegrityCritic, StageQuality, []string{HostCandidateVerify}, "harborfactory.solution_integrity_critic",
			[]workflowkit.ResourceKey{"evidence/candidate-validation"}, []workflowkit.ResourceKey{"finding/solution-integrity"},
			workflowkit.EffectEvidenceOnly, 3, contentVerdicts(), artifactInput("instruction"), artifactInput("task_toml"), artifactInput("dockerfile"), artifactInput("solve_script"), artifactInput("test_script"), artifactInput("tests_analysis"), artifactInputWithSchema(standardAuthoringCandidateSnapshotArtifact, standardAuthoringCandidateSnapshotSchemaVersion), artifactInputWithSchema(standardAuthoringValidationReceiptArtifact, standardAuthoringValidationReceiptSchemaVersion), artifactOutputWithSchema("solution_integrity_finding", workflowkit.WorkflowFindingFormat)),
		stage(AuthoringRepair, StageTaskGeneration, []string{HostCandidateVerify, TestQualityCritic, SolutionIntegrityCritic}, "harborfactory.authoring_repair",
			[]workflowkit.ResourceKey{"task/candidate", "evidence/candidate-validation"}, []workflowkit.ResourceKey{"task/candidate", resourceEvidenceRepair},
			workflowkit.EffectContentMutator, StandardAuthoringRepairMaxTurns, contentVerdicts(), artifactInput("instruction"), artifactInput("task_toml"), artifactInput("dockerfile"), artifactInput("solve_script"), artifactInput("test_script"), artifactInput("tests_analysis"), artifactInputWithSchema(standardAuthoringCandidateSnapshotArtifact, standardAuthoringCandidateSnapshotSchemaVersion), artifactInputWithSchema(standardAuthoringVerificationContractArtifact, standardAuthoringVerificationContractSchemaVersion), artifactInputWithSchema(standardAuthoringValidationReceiptArtifact, standardAuthoringValidationReceiptSchemaVersion), artifactInputWithSchema(standardAuthoringValidationRepairContextArtifact, standardAuthoringValidationRepairContextSchemaVersion), optionalArtifactInputWithSchema("test_quality_finding", workflowkit.WorkflowFindingFormat), optionalArtifactInputWithSchema("solution_integrity_finding", workflowkit.WorkflowFindingFormat), artifactOutput("instruction"), artifactOutput("task_toml"), artifactOutput("dockerfile"), artifactOutput("solve_script"), artifactOutput("test_script"), artifactOutput("tests_analysis"), artifactOutputWithSchema(standardAuthoringCandidateSnapshotArtifact, standardAuthoringCandidateSnapshotSchemaVersion), artifactOutputWithSchema(standardAuthoringValidationReceiptArtifact, standardAuthoringValidationReceiptSchemaVersion), artifactOutputWithSchema(standardAuthoringWorkflowRepairLedgerArtifact, standardAuthoringWorkflowRepairLedgerSchemaVersion)),
		gateStage(ContentReview, StageFinalReview, []string{AuthoringRepair}, ReviewContent,
			[]workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment, "task/candidate", "evidence/candidate-validation"}, []workflowkit.ResourceKey{resourceReviewContent},
			artifactInput("instruction"), artifactInput("task_toml"), artifactInput("dockerfile"), artifactInput("solve_script"), artifactInput("test_script"), artifactInput("tests_analysis"), artifactInputWithSchema(standardAuthoringCandidateSnapshotArtifact, standardAuthoringCandidateSnapshotSchemaVersion), artifactInputWithSchema(standardAuthoringValidationReceiptArtifact, standardAuthoringValidationReceiptSchemaVersion)),
		gateStage(SolutionReview, StageFinalReview, []string{AuthoringRepair}, ReviewSolutionVerifier,
			[]workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis, "task/candidate", "evidence/candidate-validation"}, []workflowkit.ResourceKey{resourceReviewSolutionVerifier},
			artifactInput("instruction"), artifactInput("task_toml"), artifactInput("dockerfile"), artifactInput("solve_script"), artifactInput("test_script"), artifactInput("tests_analysis"), artifactInputWithSchema(standardAuthoringCandidateSnapshotArtifact, standardAuthoringCandidateSnapshotSchemaVersion), artifactInputWithSchema(standardAuthoringValidationReceiptArtifact, standardAuthoringValidationReceiptSchemaVersion)),
		stage(FinalAttestation, StageFinalReview, []string{ContentReview, SolutionReview}, "harborfactory.final_attestation",
			[]workflowkit.ResourceKey{resourceReviewContent, resourceReviewSolutionVerifier, "evidence/candidate-validation"}, []workflowkit.ResourceKey{"evidence/final-attestation"},
			workflowkit.EffectEvidenceOnly, 1, passOnly(), reviewDecisionInput("content_review_decision"), reviewDecisionInput("solution_review_decision"), artifactInputWithSchema(standardAuthoringValidationReceiptArtifact, standardAuthoringValidationReceiptSchemaVersion), artifactOutputWithSchema(standardAuthoringFinalAttestationArtifact, standardAuthoringFinalAttestationSchemaVersion)),
		stage(CodeEdgePackageAdmission, StageFinalReview, []string{FinalAttestation}, "harborfactory.codeedge_package_admission",
			[]workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis, "task/candidate", "evidence/candidate-validation", "evidence/final-attestation"}, []workflowkit.ResourceKey{resourceAuthoringTaskAdmission},
			workflowkit.EffectEvidenceOnly, 1, contentVerdicts(), artifactInput("instruction"), artifactInput("task_toml"), artifactInput("dockerfile"), artifactInput("solve_script"), artifactInput("test_script"), artifactInput("tests_analysis"), artifactInputWithSchema(standardAuthoringCandidateSnapshotArtifact, standardAuthoringCandidateSnapshotSchemaVersion), artifactInputWithSchema(standardAuthoringValidationReceiptArtifact, standardAuthoringValidationReceiptSchemaVersion), artifactInputWithSchema(standardAuthoringFinalAttestationArtifact, standardAuthoringFinalAttestationSchemaVersion), artifactOutputWithSchema(standardAuthoringPackageAdmissionReportArtifact, standardAuthoringPackageAdmissionReportSchemaVersion)),
		stage(MaterializeTask, StageFinalReview, []string{CodeEdgePackageAdmission}, "harborfactory.materialize_task",
			[]workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis, "task/candidate", "evidence/candidate-validation", resourceReviewSolutionVerifier, "evidence/final-attestation", resourceAuthoringTaskAdmission},
			[]workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest, resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis, resourceAuthoringMaterializationReceipt},
			workflowkit.EffectContentMutator, 1, contentVerdicts(), artifactInput("instruction"), artifactInput("task_toml"), artifactInput("dockerfile"), artifactInput("solve_script"), artifactInput("test_script"), artifactInput("tests_analysis"), artifactInputWithSchema(standardAuthoringCandidateSnapshotArtifact, standardAuthoringCandidateSnapshotSchemaVersion), artifactInputWithSchema(standardAuthoringValidationReceiptArtifact, standardAuthoringValidationReceiptSchemaVersion), artifactInputWithSchema(standardAuthoringFinalAttestationArtifact, standardAuthoringFinalAttestationSchemaVersion), reviewDecisionInput("solution_review_decision"), artifactInputWithSchema(standardAuthoringPackageAdmissionReportArtifact, standardAuthoringPackageAdmissionReportSchemaVersion), artifactOutput("task_snapshot"), artifactOutput("task_digest"), artifactOutputWithSchema(StandardAuthoringMaterializationReceiptArtifact, StandardAuthoringMaterializationReceiptSchemaVersion)),
	}
	for index := range stages {
		stages[index].Inputs = append(stages[index].Inputs, artifactInputWithSchema(AuthoringContractArtifact, AuthoringContractSchemaVersion).spec)
		stages[index].ReadSet = append(stages[index].ReadSet, resourceAuthoringContract)
	}
	standardAuthoringAttachV3AgentRoles(stages)
	return StageCatalog{Template: StandardAuthoringContractTemplateReference(), ID: standardAuthoringCatalogID, Version: standardAuthoringCatalogVersion, Stages: stages}
}

func standardAuthoringAttachV3AgentRoles(stages []StageDefinition) {
	for index := range stages {
		stage := &stages[index]
		role, workspace, maxValidationAttempts, agent := standardAuthoringV3RoleForStage(stage.Key)
		if !agent {
			continue
		}
		inputs := append([]workflowkit.ArtifactSpec(nil), stage.Inputs...)
		stage.Concurrency = &workflowkit.ConcurrencyPolicy{Workspace: workspace}
		stage.AgentRole = &workflowkit.AgentRoleSpec{
			RoleID: role, PromptAssetFingerprint: workflowkit.SHA256Fingerprint([]byte("harbor.standard-authoring@3.0.0:" + string(stage.Key))),
			InputSchemas: inputs, Workspace: workspace, OutputMode: standardAuthoringV3RoleOutput(role), MaxTurns: stage.RequiredTurns, MaxValidationAttempts: maxValidationAttempts,
			AllowedDynamicTools: standardAuthoringV3AllowedTools(role),
		}
	}
}

func standardAuthoringV3RoleForStage(key workflowkit.StageKey) (workflowkit.AgentRoleID, workflowkit.WorkspaceBinding, int, bool) {
	readSource := workflowkit.WorkspaceBinding{Mode: workflowkit.WorkspaceReadOnlySnapshot, Key: "authoring-source", SnapshotArtifact: "repo_prepared"}
	readCandidate := workflowkit.WorkspaceBinding{Mode: workflowkit.WorkspaceReadOnlySnapshot, Key: "authoring-candidate-critic", SnapshotArtifact: standardAuthoringCandidateSnapshotArtifact}
	writeCandidate := workflowkit.WorkspaceBinding{Mode: workflowkit.WorkspaceExclusiveWriter, Key: "authoring-candidate", SnapshotArtifact: standardAuthoringCandidateSnapshotArtifact}
	switch key {
	case workflowkit.StageKey(RepoStructureResearch), workflowkit.StageKey(TestRuntimeResearch), workflowkit.StageKey(VerifierThreatResearch):
		return workflowkit.AgentRoleResearcher, readSource, 0, true
	case workflowkit.StageKey(TaskSynthesis):
		return workflowkit.AgentRoleSynthesizer, workflowkit.WorkspaceBinding{Mode: workflowkit.WorkspaceNone}, 0, true
	case workflowkit.StageKey(AuthoringLoop):
		writeCandidate.SnapshotArtifact = "task_specification"
		return workflowkit.AgentRoleAuthor, writeCandidate, 8, true
	case workflowkit.StageKey(TestQualityCritic), workflowkit.StageKey(SolutionIntegrityCritic):
		return workflowkit.AgentRoleCritic, readCandidate, 0, true
	case workflowkit.StageKey(AuthoringRepair):
		return workflowkit.AgentRoleAuthor, writeCandidate, StandardAuthoringRepairMaxTurns, true
	default:
		return "", workflowkit.WorkspaceBinding{}, 0, false
	}
}

func standardAuthoringV3RoleOutput(role workflowkit.AgentRoleID) workflowkit.AgentOutputMode {
	switch role {
	case workflowkit.AgentRoleResearcher:
		return workflowkit.AgentOutputEvidence
	case workflowkit.AgentRoleSynthesizer:
		return workflowkit.AgentOutputStructuredArtifact
	case workflowkit.AgentRoleAuthor:
		return workflowkit.AgentOutputCandidateSnapshot
	default:
		return workflowkit.AgentOutputFinding
	}
}

func standardAuthoringV3AllowedTools(role workflowkit.AgentRoleID) []string {
	if role == workflowkit.AgentRoleAuthor {
		return []string{"harbor_validate_candidate"}
	}
	return []string{"harbor_submit_output"}
}
