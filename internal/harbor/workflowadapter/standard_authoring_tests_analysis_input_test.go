package workflowadapter

import (
	"reflect"
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringTestsAnalysisInputTemplateBindsCurrentGeneratedArtifacts(t *testing.T) {
	template := StandardAuthoringTestsAnalysisInputWorkflowTemplate()
	if err := template.Validate(); err != nil {
		t.Fatalf("validate 1.6.0 template: %v", err)
	}
	if !template.Reference().Equal(StandardAuthoringTestsAnalysisInputTemplateReference()) ||
		!StandardAuthoringCurrentTemplateReference().Equal(template.Reference()) ||
		!StandardAuthoringCurrentWorkflowTemplate().Reference().Equal(template.Reference()) ||
		template.Catalog.Version != StandardAuthoringTestsAnalysisInputTemplateVersion ||
		template.QuotaPolicy.Version != StandardAuthoringTestsAnalysisInputQuotaPolicyVersion {
		t.Fatalf("tests-analysis-input identities = template %s catalog %s quota %s", template.Version, template.Catalog.Version, template.QuotaPolicy.Version)
	}

	for _, contract := range []struct {
		stage        workflowkit.StageKey
		artifact     string
		resource     workflowkit.ResourceKey
		dependencies []workflowkit.StageKey
	}{
		{stage: workflowkit.StageKey(SolveGen), artifact: "dockerfile", resource: resourceTaskEnvironment, dependencies: []workflowkit.StageKey{workflowkit.StageKey(ContentReview)}},
		{stage: workflowkit.StageKey(TestGen), artifact: "dockerfile", resource: resourceTaskEnvironment, dependencies: []workflowkit.StageKey{workflowkit.StageKey(ContentReview)}},
		{stage: workflowkit.StageKey(TestsAnalysis), artifact: "test_script", resource: resourceTaskTests, dependencies: []workflowkit.StageKey{workflowkit.StageKey(TestGen)}},
	} {
		stage, present := template.Catalog.Stage(contract.stage)
		if !present {
			t.Fatalf("1.6.0 catalog omits stage %q", contract.stage)
		}
		input, present := stageArtifactSpec(stage.Inputs, contract.artifact)
		if !present || !input.Required || input.SchemaVersion != "harbor.artifact.v1" || stageCatalogResourceCount(stage.ReadSet, contract.resource) != 1 {
			t.Fatalf("stage %q current artifact contract = input %+v present=%t reads=%v", contract.stage, input, present, stage.ReadSet)
		}
		if !sameStageKeySet(stage.Dependencies, contract.dependencies) {
			t.Fatalf("stage %q dependencies = %v, want %v", contract.stage, stage.Dependencies, contract.dependencies)
		}
		if contract.stage == workflowkit.StageKey(TestsAnalysis) && !reflect.DeepEqual(stage.Verdicts, passOnly()) {
			t.Fatalf("1.6.0 tests_analysis verdicts = %v, want pass-only evidence", stage.Verdicts.Allowed)
		}
	}

	handoffSchema, err := StandardAuthoringTaskHandoffSchemaForTemplate(template.Reference())
	if err != nil || handoffSchema != StandardAuthoringTaskAdmissionHandoffSchemaVersion {
		t.Fatalf("1.6.0 handoff schema = %q, %v", handoffSchema, err)
	}
	order, err := StandardAuthoringStageOrderForTemplate(template.Reference())
	if err != nil || !reflect.DeepEqual(order, StandardAuthoringTaskAdmissionStageOrder()) {
		t.Fatalf("1.6.0 stage order = %v, %v", order, err)
	}
	historicalQuota := StandardAuthoringRepairFeedbackQuotaPolicy()
	if !reflect.DeepEqual(template.QuotaPolicy.AccountLimits, historicalQuota.AccountLimits) {
		t.Fatalf("1.6.0 quota limits changed: got %+v, want %+v", template.QuotaPolicy.AccountLimits, historicalQuota.AccountLimits)
	}
	for _, stage := range historicalQuota.Stages {
		claims, present := quotaClaimsForStage(template.QuotaPolicy, stage.StageKey)
		if !present || !reflect.DeepEqual(claims, stage.Claims) {
			t.Fatalf("1.6.0 stage %q quota claims = %+v, want %+v", stage.StageKey, claims, stage.Claims)
		}
	}
}

func TestStandardAuthoringRepairFeedbackTemplateRemainsHistorical(t *testing.T) {
	historical := StandardAuthoringRepairFeedbackWorkflowTemplate()
	if err := historical.Validate(); err != nil {
		t.Fatalf("validate historical 1.5.0 template: %v", err)
	}
	if historical.Version != "1.5.0" || historical.Catalog.Version != "1.5.0" || historical.QuotaPolicy.Version != "1.5.0" {
		t.Fatalf("historical identities = template %s catalog %s quota %s", historical.Version, historical.Catalog.Version, historical.QuotaPolicy.Version)
	}

	for _, contract := range []struct {
		stage        workflowkit.StageKey
		artifact     string
		resource     workflowkit.ResourceKey
		dependencies []workflowkit.StageKey
	}{
		{stage: workflowkit.StageKey(SolveGen), artifact: "dockerfile", resource: resourceTaskEnvironment, dependencies: []workflowkit.StageKey{workflowkit.StageKey(ContentReview)}},
		{stage: workflowkit.StageKey(TestGen), artifact: "dockerfile", resource: resourceTaskEnvironment, dependencies: []workflowkit.StageKey{workflowkit.StageKey(ContentReview)}},
		{stage: workflowkit.StageKey(TestsAnalysis), artifact: "test_script", resource: resourceTaskTests, dependencies: []workflowkit.StageKey{workflowkit.StageKey(ContentReview)}},
	} {
		stage, present := historical.Catalog.Stage(contract.stage)
		if !present {
			t.Fatalf("historical catalog omits stage %q", contract.stage)
		}
		if _, present := stageArtifactSpec(stage.Inputs, contract.artifact); present || stageCatalogResourceCount(stage.ReadSet, contract.resource) != 0 {
			t.Fatalf("historical stage %q acquired 1.6.0 artifact contract %q/%q", contract.stage, contract.artifact, contract.resource)
		}
		if !sameStageKeySet(stage.Dependencies, contract.dependencies) {
			t.Fatalf("historical stage %q dependencies = %v, want %v", contract.stage, stage.Dependencies, contract.dependencies)
		}
		if contract.stage == workflowkit.StageKey(TestsAnalysis) && !reflect.DeepEqual(stage.Verdicts, contentVerdicts()) {
			t.Fatalf("historical tests_analysis verdicts changed: %v", stage.Verdicts.Allowed)
		}
	}

	registry := DefaultTemplateRegistry()
	resolved, err := registry.ResolveTemplate(StandardAuthoringRepairFeedbackTemplateReference())
	if err != nil || !resolved.Reference().Equal(historical.Reference()) {
		t.Fatalf("resolve historical 1.5.0 template = %s@%s, %v", resolved.ID, resolved.Version, err)
	}
}

func TestStandardAuthoringTestsAnalysisInputCatalogRejectsCurrentArtifactDrift(t *testing.T) {
	testCases := []struct {
		name   string
		stage  workflowkit.StageKey
		mutate func(*StageDefinition)
	}{
		{
			name:  "solve Dockerfile becomes optional",
			stage: workflowkit.StageKey(SolveGen),
			mutate: func(stage *StageDefinition) {
				mutableStageInput(t, stage, "dockerfile").Required = false
			},
		},
		{
			name:  "test Dockerfile resource omitted",
			stage: workflowkit.StageKey(TestGen),
			mutate: func(stage *StageDefinition) {
				stage.ReadSet = removeStageResource(stage.ReadSet, resourceTaskEnvironment)
			},
		},
		{
			name:  "tests analysis dependency regresses",
			stage: workflowkit.StageKey(TestsAnalysis),
			mutate: func(stage *StageDefinition) {
				stage.Dependencies = []workflowkit.StageKey{workflowkit.StageKey(ContentReview)}
			},
		},
		{
			name:  "test script becomes optional",
			stage: workflowkit.StageKey(TestsAnalysis),
			mutate: func(stage *StageDefinition) {
				mutableStageInput(t, stage, "test_script").Required = false
			},
		},
		{
			name:  "test script resource omitted",
			stage: workflowkit.StageKey(TestsAnalysis),
			mutate: func(stage *StageDefinition) {
				stage.ReadSet = removeStageResource(stage.ReadSet, resourceTaskTests)
			},
		},
		{
			name:  "tests analysis permits needs repair",
			stage: workflowkit.StageKey(TestsAnalysis),
			mutate: func(stage *StageDefinition) {
				stage.Verdicts.Allowed = append(stage.Verdicts.Allowed, workflowkit.VerdictNeedsRepair)
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			catalog := StandardAuthoringTestsAnalysisInputStageCatalog()
			stage := mutableCatalogStage(t, &catalog, testCase.stage)
			testCase.mutate(stage)
			if err := catalog.Validate(); err == nil {
				t.Fatal("drifted 1.6.0 catalog validated")
			}
		})
	}
}
