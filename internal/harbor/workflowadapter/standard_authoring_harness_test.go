package workflowadapter

import (
	"reflect"
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringHarnessTemplateBindsValidatedArtifacts(t *testing.T) {
	template := StandardAuthoringHarnessWorkflowTemplate()
	if err := template.Validate(); err != nil {
		t.Fatalf("validate 1.7.0 template: %v", err)
	}
	if !template.Reference().Equal(StandardAuthoringHarnessTemplateReference()) ||
		template.Catalog.Version != StandardAuthoringHarnessTemplateVersion ||
		template.QuotaPolicy.Version != StandardAuthoringHarnessQuotaPolicyVersion {
		t.Fatalf("harness identities = template %s catalog %s quota %s", template.Version, template.Catalog.Version, template.QuotaPolicy.Version)
	}
	if StandardAuthoringCurrentTemplateReference().Equal(template.Reference()) || StandardAuthoringCurrentWorkflowTemplate().Reference().Equal(template.Reference()) {
		t.Fatal("historical 1.7.0 harness template remained current after fixed-file release")
	}
	order, err := StandardAuthoringStageOrderForTemplate(template.Reference())
	if err != nil || !reflect.DeepEqual(order, StandardAuthoringHarnessStageOrder()) || len(order) != len(template.Catalog.Stages) {
		t.Fatalf("1.7.0 stage order = %v catalog=%d err=%v", order, len(template.Catalog.Stages), err)
	}
	if handoff, err := StandardAuthoringTaskHandoffSchemaForTemplate(template.Reference()); err != nil || handoff != StandardAuthoringTaskAdmissionHandoffSchemaVersion {
		t.Fatalf("1.7.0 handoff schema = %q, %v", handoff, err)
	}

	build := requireAuthoringHarnessStage(t, template.Catalog, DockerfileBuildValidate)
	if build.Effect != workflowkit.EffectContentMutator || !reflect.DeepEqual(build.Verdicts, passOnly()) ||
		!stageHasArtifact(build.Outputs, StandardAuthoringValidatedDockerfileArtifact, "harbor.artifact.v1") ||
		!stageHasArtifact(build.Outputs, StandardAuthoringDockerfileBuildReportArtifact, StandardAuthoringDockerfileBuildReportSchemaVersion) {
		t.Fatalf("Dockerfile build validator contract = %+v", build)
	}

	harness := requireAuthoringHarnessStage(t, template.Catalog, AuthoringHarness)
	if harness.Effect != workflowkit.EffectContentMutator || !reflect.DeepEqual(harness.Verdicts, passOnly()) ||
		!stageHasArtifact(harness.Inputs, "solve_script", "harbor.artifact.v1") ||
		!stageHasArtifact(harness.Inputs, "test_script", "harbor.artifact.v1") ||
		!stageHasArtifact(harness.Outputs, StandardAuthoringValidatedSolveScriptArtifact, "harbor.artifact.v1") ||
		!stageHasArtifact(harness.Outputs, StandardAuthoringValidatedTestScriptArtifact, "harbor.artifact.v1") ||
		!stageHasArtifact(harness.Outputs, StandardAuthoringHarnessReportArtifact, StandardAuthoringHarnessReportSchemaVersion) {
		t.Fatalf("authoring harness contract = %+v", harness)
	}

	for _, contract := range []struct {
		stage    string
		artifact string
		resource workflowkit.ResourceKey
	}{
		{stage: ContentReview, artifact: StandardAuthoringValidatedDockerfileArtifact, resource: resourceTaskValidatedEnvironment},
		{stage: SolveGen, artifact: StandardAuthoringValidatedDockerfileArtifact, resource: resourceTaskValidatedEnvironment},
		{stage: TestGen, artifact: StandardAuthoringValidatedDockerfileArtifact, resource: resourceTaskValidatedEnvironment},
		{stage: TestsAnalysis, artifact: StandardAuthoringValidatedTestScriptArtifact, resource: resourceTaskValidatedTests},
		{stage: CodeEdgePackageAdmission, artifact: StandardAuthoringValidatedDockerfileArtifact, resource: resourceTaskValidatedEnvironment},
		{stage: CodeEdgePackageAdmission, artifact: StandardAuthoringValidatedSolveScriptArtifact, resource: resourceTaskValidatedSolution},
		{stage: CodeEdgePackageAdmission, artifact: StandardAuthoringValidatedTestScriptArtifact, resource: resourceTaskValidatedTests},
		{stage: SolutionReview, artifact: StandardAuthoringValidatedSolveScriptArtifact, resource: resourceTaskValidatedSolution},
		{stage: SolutionReview, artifact: StandardAuthoringValidatedTestScriptArtifact, resource: resourceTaskValidatedTests},
		{stage: MaterializeTask, artifact: StandardAuthoringValidatedDockerfileArtifact, resource: resourceTaskValidatedEnvironment},
		{stage: MaterializeTask, artifact: StandardAuthoringValidatedSolveScriptArtifact, resource: resourceTaskValidatedSolution},
		{stage: MaterializeTask, artifact: StandardAuthoringValidatedTestScriptArtifact, resource: resourceTaskValidatedTests},
	} {
		stage := requireAuthoringHarnessStage(t, template.Catalog, contract.stage)
		if !stageHasArtifact(stage.Inputs, contract.artifact, "harbor.artifact.v1") || stageCatalogResourceCount(stage.ReadSet, contract.resource) != 1 {
			t.Fatalf("stage %q does not consume validated artifact %q through %q", contract.stage, contract.artifact, contract.resource)
		}
	}

	materialize := requireAuthoringHarnessStage(t, template.Catalog, MaterializeTask)
	if !stageHasArtifact(materialize.Inputs, StandardAuthoringDockerfileBuildReportArtifact, StandardAuthoringDockerfileBuildReportSchemaVersion) ||
		!stageHasArtifact(materialize.Inputs, StandardAuthoringHarnessReportArtifact, StandardAuthoringHarnessReportSchemaVersion) {
		t.Fatalf("materialize validation evidence inputs = %+v", materialize.Inputs)
	}
	for _, output := range materialize.Outputs {
		if output.Name == StandardAuthoringDockerfileBuildReportArtifact || output.Name == StandardAuthoringHarnessReportArtifact {
			t.Fatalf("materialize leaked validation report %q into task outputs", output.Name)
		}
	}
}

func TestStandardAuthoringFixedFileTemplateIsCurrentWithoutTopologyDrift(t *testing.T) {
	template := StandardAuthoringFixedFileWorkflowTemplate()
	if err := template.Validate(); err != nil {
		t.Fatalf("validate 1.8.0 template: %v", err)
	}
	if !template.Reference().Equal(StandardAuthoringFixedFileTemplateReference()) ||
		!StandardAuthoringCurrentTemplateReference().Equal(template.Reference()) ||
		!StandardAuthoringCurrentWorkflowTemplate().Reference().Equal(template.Reference()) ||
		template.Catalog.Version != StandardAuthoringFixedFileTemplateVersion ||
		template.QuotaPolicy.Version != StandardAuthoringFixedFileQuotaPolicyVersion {
		t.Fatalf("fixed-file identities = template %s catalog %s quota %s", template.Version, template.Catalog.Version, template.QuotaPolicy.Version)
	}
	order, err := StandardAuthoringStageOrderForTemplate(template.Reference())
	if err != nil || !reflect.DeepEqual(order, StandardAuthoringHarnessStageOrder()) || len(order) != len(template.Catalog.Stages) {
		t.Fatalf("1.8.0 stage order = %v catalog=%d err=%v", order, len(template.Catalog.Stages), err)
	}
	harness := StandardAuthoringHarnessStageCatalog()
	for _, fixed := range template.Catalog.Stages {
		legacy, found := harness.Stage(fixed.Key)
		if !found {
			t.Fatalf("1.8.0 stage %q was not present in 1.7.0", fixed.Key)
		}
		switch fixed.Key {
		case workflowkit.StageKey(SolveGen), workflowkit.StageKey(TestGen):
			if !reflect.DeepEqual(fixed.Verdicts, passOnly()) || reflect.DeepEqual(legacy.Verdicts, fixed.Verdicts) {
				t.Fatalf("fixed-file script verdicts = fixed:%+v legacy:%+v", fixed.Verdicts, legacy.Verdicts)
			}
			legacy.Verdicts = fixed.Verdicts.Clone()
		}
		if !reflect.DeepEqual(fixed, legacy) {
			t.Fatalf("1.8.0 stage %q drifted beyond its fixed-file verdict contract", fixed.Key)
		}
	}
}

func TestStandardAuthoringHarnessQuotaSeparatelyBoundsValidation(t *testing.T) {
	policy := StandardAuthoringHarnessQuotaPolicy()
	if err := policy.ValidateFor(StandardAuthoringHarnessStageCatalog()); err != nil {
		t.Fatalf("validate 1.7.0 quota policy: %v", err)
	}
	limits := make(map[string]QuotaAccountLimit, len(policy.AccountLimits))
	for _, limit := range policy.AccountLimits {
		limits[limit.Dimension] = limit
	}
	if got := limits[StandardAuthoringValidationQuotaDimension]; got != (QuotaAccountLimit{
		Dimension: StandardAuthoringValidationQuotaDimension, TaskLimitUnits: 64, ActorLimitUnits: 640,
	}) {
		t.Fatalf("authoring validation account = %+v", got)
	}
	for _, key := range []workflowkit.StageKey{workflowkit.StageKey(DockerfileBuildValidate), workflowkit.StageKey(AuthoringHarness)} {
		claims, present := quotaClaimsForStage(policy, key)
		if !present || !hasQuotaClaim(claims, "stage_attempt", 1) || !hasQuotaClaim(claims, "agent_turn", 1) ||
			!hasQuotaClaim(claims, "output_submission", StandardAuthoringHarnessSubmissionClaimUnits) ||
			!hasQuotaClaim(claims, StandardAuthoringValidationQuotaDimension, StandardAuthoringValidationClaimUnits) {
			t.Fatalf("1.7.0 stage %q claims = %+v", key, claims)
		}
	}
	for _, stage := range policy.Stages {
		if stage.StageKey == workflowkit.StageKey(DockerfileBuildValidate) || stage.StageKey == workflowkit.StageKey(AuthoringHarness) {
			continue
		}
		if hasQuotaClaim(stage.Claims, StandardAuthoringValidationQuotaDimension, StandardAuthoringValidationClaimUnits) {
			t.Fatalf("stage %q acquired authoring validation quota", stage.StageKey)
		}
	}
}

func TestStandardAuthoringTestsAnalysisInputTemplateRemainsHistoricalAfterHarness(t *testing.T) {
	historical := StandardAuthoringTestsAnalysisInputWorkflowTemplate()
	if err := historical.Validate(); err != nil {
		t.Fatalf("validate historical 1.6.0 template: %v", err)
	}
	for _, key := range []string{DockerfileBuildValidate, AuthoringHarness} {
		if _, present := historical.Catalog.Stage(workflowkit.StageKey(key)); present {
			t.Fatalf("historical 1.6.0 catalog acquired stage %q", key)
		}
	}
	for _, stage := range historical.Catalog.Stages {
		for _, artifact := range append(append([]workflowkit.ArtifactSpec(nil), stage.Inputs...), stage.Outputs...) {
			switch artifact.Name {
			case StandardAuthoringValidatedDockerfileArtifact, StandardAuthoringDockerfileBuildReportArtifact,
				StandardAuthoringValidatedSolveScriptArtifact, StandardAuthoringValidatedTestScriptArtifact,
				StandardAuthoringHarnessReportArtifact:
				t.Fatalf("historical 1.6.0 stage %q acquired 1.7.0 artifact %q", stage.Key, artifact.Name)
			}
		}
	}
	for _, limit := range historical.QuotaPolicy.AccountLimits {
		if limit.Dimension == StandardAuthoringValidationQuotaDimension {
			t.Fatal("historical 1.6.0 quota policy acquired authoring validation account")
		}
	}
}

func TestStandardAuthoringHarnessCatalogRejectsValidationDrift(t *testing.T) {
	testCases := []struct {
		name   string
		stage  workflowkit.StageKey
		mutate func(*StageDefinition)
	}{
		{
			name:  "build validator permits repair verdict",
			stage: workflowkit.StageKey(DockerfileBuildValidate),
			mutate: func(stage *StageDefinition) {
				stage.Verdicts.Allowed = append(stage.Verdicts.Allowed, workflowkit.VerdictNeedsRepair)
			},
		},
		{
			name:  "content review consumes raw Dockerfile",
			stage: workflowkit.StageKey(ContentReview),
			mutate: func(stage *StageDefinition) {
				mutableStageInput(t, stage, StandardAuthoringValidatedDockerfileArtifact).Name = "dockerfile"
			},
		},
		{
			name:  "harness report schema drifts",
			stage: workflowkit.StageKey(AuthoringHarness),
			mutate: func(stage *StageDefinition) {
				mutableAuthoringHarnessOutput(t, stage, StandardAuthoringHarnessReportArtifact).SchemaVersion = "harbor.invalid.v1"
			},
		},
		{
			name:  "tests analysis consumes raw test",
			stage: workflowkit.StageKey(TestsAnalysis),
			mutate: func(stage *StageDefinition) {
				mutableStageInput(t, stage, StandardAuthoringValidatedTestScriptArtifact).Name = "test_script"
			},
		},
		{
			name:  "admission consumes raw solution",
			stage: workflowkit.StageKey(CodeEdgePackageAdmission),
			mutate: func(stage *StageDefinition) {
				mutableStageInput(t, stage, StandardAuthoringValidatedSolveScriptArtifact).Name = "solve_script"
			},
		},
		{
			name:  "materialize omits harness evidence",
			stage: workflowkit.StageKey(MaterializeTask),
			mutate: func(stage *StageDefinition) {
				stage.Inputs = removeStageArtifact(stage.Inputs, StandardAuthoringHarnessReportArtifact)
				stage.ReadSet = removeStageResource(stage.ReadSet, resourceEvidenceAuthoringHarness)
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			catalog := StandardAuthoringHarnessStageCatalog()
			stage := mutableCatalogStage(t, &catalog, testCase.stage)
			testCase.mutate(stage)
			if err := catalog.Validate(); err == nil {
				t.Fatal("drifted 1.7.0 catalog validated")
			}
		})
	}
}

func requireAuthoringHarnessStage(t *testing.T, catalog StageCatalog, key string) StageDefinition {
	t.Helper()
	stage, present := catalog.Stage(workflowkit.StageKey(key))
	if !present {
		t.Fatalf("1.7.0 catalog omits stage %q", key)
	}
	return stage
}

func mutableAuthoringHarnessOutput(t *testing.T, stage *StageDefinition, name string) *workflowkit.ArtifactSpec {
	t.Helper()
	for index := range stage.Outputs {
		if stage.Outputs[index].Name == name {
			return &stage.Outputs[index]
		}
	}
	t.Fatalf("stage %q omits output %q", stage.Key, name)
	return nil
}

func removeStageArtifact(artifacts []workflowkit.ArtifactSpec, name string) []workflowkit.ArtifactSpec {
	result := make([]workflowkit.ArtifactSpec, 0, len(artifacts))
	for _, artifact := range artifacts {
		if artifact.Name != name {
			result = append(result, artifact)
		}
	}
	return result
}
