package workflowadapter

import (
	"reflect"
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringRepairFeedbackTemplateFreezesOptionalFeedbackMatrix(t *testing.T) {
	template := StandardAuthoringRepairFeedbackWorkflowTemplate()
	if err := template.Validate(); err != nil {
		t.Fatalf("validate 1.5.0 template: %v", err)
	}
	if !template.Reference().Equal(StandardAuthoringRepairFeedbackTemplateReference()) ||
		template.Catalog.Version != StandardAuthoringRepairFeedbackTemplateVersion ||
		template.QuotaPolicy.Version != StandardAuthoringRepairFeedbackQuotaPolicyVersion {
		t.Fatalf("repair-feedback identities = template %s catalog %s quota %s", template.Version, template.Catalog.Version, template.QuotaPolicy.Version)
	}
	handoffSchema, err := StandardAuthoringTaskHandoffSchemaForTemplate(template.Reference())
	if err != nil || handoffSchema != StandardAuthoringTaskAdmissionHandoffSchemaVersion {
		t.Fatalf("repair-feedback handoff schema = %q, %v", handoffSchema, err)
	}

	expected := map[workflowkit.StageKey]map[string]workflowkit.ResourceKey{
		workflowkit.StageKey(RepoAnalyze): {
			"task_review_decision": resourceReviewTaskDirection,
		},
		workflowkit.StageKey(TaskDesign): {
			"task_review_decision": resourceReviewTaskDirection,
		},
		workflowkit.StageKey(InstructionGen): {
			"content_review_decision":           resourceReviewContent,
			"solution_review_decision":          resourceReviewSolutionVerifier,
			"codeedge_package_admission_report": resourceAuthoringTaskAdmission,
		},
		workflowkit.StageKey(TaskTOMLGen): {
			"content_review_decision":           resourceReviewContent,
			"solution_review_decision":          resourceReviewSolutionVerifier,
			"codeedge_package_admission_report": resourceAuthoringTaskAdmission,
		},
		workflowkit.StageKey(DockerfileGen): {
			"content_review_decision":           resourceReviewContent,
			"solution_review_decision":          resourceReviewSolutionVerifier,
			"codeedge_package_admission_report": resourceAuthoringTaskAdmission,
		},
		workflowkit.StageKey(SolveGen): {
			"solution_review_decision":          resourceReviewSolutionVerifier,
			"codeedge_package_admission_report": resourceAuthoringTaskAdmission,
		},
		workflowkit.StageKey(TestGen): {
			"solution_review_decision":          resourceReviewSolutionVerifier,
			"codeedge_package_admission_report": resourceAuthoringTaskAdmission,
		},
		workflowkit.StageKey(TestsAnalysis): {
			"solution_review_decision":          resourceReviewSolutionVerifier,
			"codeedge_package_admission_report": resourceAuthoringTaskAdmission,
		},
	}

	optionalCount := 0
	for _, stage := range template.Catalog.Stages {
		for _, input := range stage.Inputs {
			if input.Required {
				continue
			}
			optionalCount++
			resource, expectedInput := expected[stage.Key][input.Name]
			if !expectedInput {
				t.Fatalf("stage %q has unexpected optional input %+v", stage.Key, input)
			}
			wantSchema := "harbor.review-decision.v1"
			if input.Name == "codeedge_package_admission_report" {
				wantSchema = "harbor.standard-authoring-task-package-admission.v1"
			}
			if input.SchemaVersion != wantSchema || stageCatalogResourceCount(stage.ReadSet, resource) != 1 {
				t.Fatalf("stage %q optional input %+v does not match resource %q", stage.Key, input, resource)
			}
		}
		for name, resource := range expected[stage.Key] {
			input, present := stageArtifactSpec(stage.Inputs, name)
			if !present || input.Required || stageCatalogResourceCount(stage.ReadSet, resource) != 1 {
				t.Fatalf("stage %q repair feedback %q = %+v, present=%t, reads=%v", stage.Key, name, input, present, stage.ReadSet)
			}
		}
	}
	if optionalCount != 17 {
		t.Fatalf("repair-feedback optional input count = %d, want 17", optionalCount)
	}

	historical := StandardAuthoringBriefWorkflowTemplate()
	if err := historical.Validate(); err != nil {
		t.Fatalf("validate historical 1.4.0 template: %v", err)
	}
	for key, contracts := range expected {
		stage, present := historical.Catalog.Stage(key)
		if !present {
			t.Fatalf("historical template omits stage %q", key)
		}
		for name, resource := range contracts {
			if _, present := stageArtifactSpec(stage.Inputs, name); present || stageCatalogResourceCount(stage.ReadSet, resource) != 0 {
				t.Fatalf("historical 1.4.0 stage %q acquired repair feedback %q/%q", key, name, resource)
			}
		}
	}

	if !reflect.DeepEqual(template.QuotaPolicy.AccountLimits, historical.QuotaPolicy.AccountLimits) {
		t.Fatalf("1.5.0 quota limits changed: got %+v, want %+v", template.QuotaPolicy.AccountLimits, historical.QuotaPolicy.AccountLimits)
	}
	for _, stage := range historical.QuotaPolicy.Stages {
		claims, present := quotaClaimsForStage(template.QuotaPolicy, stage.StageKey)
		if !present || !reflect.DeepEqual(claims, stage.Claims) {
			t.Fatalf("1.5.0 stage %q quota claims = %+v, want %+v", stage.StageKey, claims, stage.Claims)
		}
	}
}

func TestStandardAuthoringRepairFeedbackCatalogRejectsInputResourceDrift(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(*StageCatalog)
	}{
		{
			name: "feedback becomes required",
			mutate: func(catalog *StageCatalog) {
				stage := mutableCatalogStage(t, catalog, workflowkit.StageKey(RepoAnalyze))
				input := mutableStageInput(t, stage, "task_review_decision")
				input.Required = true
			},
		},
		{
			name: "feedback resource omitted",
			mutate: func(catalog *StageCatalog) {
				stage := mutableCatalogStage(t, catalog, workflowkit.StageKey(InstructionGen))
				stage.ReadSet = removeStageResource(stage.ReadSet, resourceAuthoringTaskAdmission)
			},
		},
		{
			name: "feedback added to undeclared consumer",
			mutate: func(catalog *StageCatalog) {
				stage := mutableCatalogStage(t, catalog, workflowkit.StageKey(RepoPrepare))
				stage.Inputs = append(stage.Inputs, optionalReviewDecisionInput("task_review_decision").spec)
				stage.ReadSet = append(stage.ReadSet, resourceReviewTaskDirection)
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			catalog := StandardAuthoringRepairFeedbackStageCatalog()
			testCase.mutate(&catalog)
			if err := catalog.Validate(); err == nil {
				t.Fatal("drifted repair-feedback catalog validated")
			}
		})
	}
}

func TestStandardAuthoringBriefCatalogRejectsRepairFeedbackBackport(t *testing.T) {
	catalog := StandardAuthoringBriefStageCatalog()
	stage := mutableCatalogStage(t, &catalog, workflowkit.StageKey(RepoAnalyze))
	stage.Inputs = append(stage.Inputs, optionalReviewDecisionInput("task_review_decision").spec)
	stage.ReadSet = append(stage.ReadSet, resourceReviewTaskDirection)
	if err := catalog.Validate(); err == nil {
		t.Fatal("historical 1.4.0 catalog accepted 1.5.0 repair feedback")
	}
}

func quotaClaimsForStage(policy QuotaPolicy, key workflowkit.StageKey) ([]workflowkit.QuotaClaim, bool) {
	for _, stage := range policy.Stages {
		if stage.StageKey == key {
			return stage.Claims, true
		}
	}
	return nil, false
}

func mutableCatalogStage(t *testing.T, catalog *StageCatalog, key workflowkit.StageKey) *StageDefinition {
	t.Helper()
	for index := range catalog.Stages {
		if catalog.Stages[index].Key == key {
			return &catalog.Stages[index]
		}
	}
	t.Fatalf("catalog omits stage %q", key)
	return nil
}

func mutableStageInput(t *testing.T, stage *StageDefinition, name string) *workflowkit.ArtifactSpec {
	t.Helper()
	for index := range stage.Inputs {
		if stage.Inputs[index].Name == name {
			return &stage.Inputs[index]
		}
	}
	t.Fatalf("stage %q omits input %q", stage.Key, name)
	return nil
}

func removeStageResource(resources []workflowkit.ResourceKey, target workflowkit.ResourceKey) []workflowkit.ResourceKey {
	result := make([]workflowkit.ResourceKey, 0, len(resources))
	for _, resource := range resources {
		if resource != target {
			result = append(result, resource)
		}
	}
	return result
}
