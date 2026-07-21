package app

import (
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestCurrentStandardAuthoringRuntimeContractRejectsLegacyRun(t *testing.T) {
	template := workflowadapter.StandardAuthoringWorkflowTemplate()
	profile := lifecycleCompleteProfileForTemplate(t, template)
	resolved, err := template.Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	runID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	run := store.WorkflowRun{
		ID: runID, WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion: "1.0.0",
	}
	err = validateCurrentStandardAuthoringFrozenContract(run, runManifest{Resolved: resolved}, workflowadapter.RunExecutionSpec{Template: template.Reference()})
	if err == nil || !strings.Contains(err.Error(), "requires current template") {
		t.Fatalf("legacy Standard authoring Run contract = %v, want current-template rejection", err)
	}
}

func TestStandardAuthoringRuntimeContractRejectsCurrentDescriptorWithoutPolicy(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	profile := lifecycleCompleteProfileForTemplate(t, template)
	resolved, err := template.Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	for index := range resolved.Descriptor.Stages {
		inputs := resolved.Descriptor.Stages[index].Inputs
		filtered := inputs[:0]
		for _, input := range inputs {
			if input.Name != workflowadapter.StandardAuthoringEnvironmentPolicyArtifact {
				filtered = append(filtered, input)
			}
		}
		resolved.Descriptor.Stages[index].Inputs = filtered
	}
	runID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	run := store.WorkflowRun{
		ID: runID, WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringFixedFileTemplateVersion,
	}
	err = validateCurrentStandardAuthoringFrozenContract(run, runManifest{Resolved: resolved}, workflowadapter.RunExecutionSpec{Template: template.Reference()})
	if err == nil || !strings.Contains(err.Error(), "environment policy contract") {
		t.Fatalf("current descriptor without environment policy = %v, want policy-contract rejection", err)
	}
}

func TestStandardAuthoringRuntimeContractRejectsCurrentDescriptorWithoutBrief(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	profile := lifecycleCompleteProfileForTemplate(t, template)
	resolved, err := template.Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	for index := range resolved.Descriptor.Stages {
		inputs := resolved.Descriptor.Stages[index].Inputs
		filtered := inputs[:0]
		for _, input := range inputs {
			if input.Name != workflowadapter.StandardAuthoringBriefArtifact {
				filtered = append(filtered, input)
			}
		}
		resolved.Descriptor.Stages[index].Inputs = filtered
	}
	runID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	run := store.WorkflowRun{
		ID: runID, WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringFixedFileTemplateVersion,
	}
	err = validateCurrentStandardAuthoringFrozenContract(run, runManifest{Resolved: resolved}, workflowadapter.RunExecutionSpec{Template: template.Reference()})
	if err == nil || !strings.Contains(err.Error(), "brief contract") {
		t.Fatalf("current descriptor without brief = %v, want brief-contract rejection", err)
	}
}

func TestStandardAuthoringRuntimeContractRejectsCurrentDescriptorWithoutRepairFeedback(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	profile := lifecycleCompleteProfileForTemplate(t, template)
	resolved, err := template.Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	for index := range resolved.Descriptor.Stages {
		if resolved.Descriptor.Stages[index].Key != workflowkit.StageKey(workflowadapter.RepoAnalyze) {
			continue
		}
		inputs := resolved.Descriptor.Stages[index].Inputs
		filtered := inputs[:0]
		for _, input := range inputs {
			if input.Name != "task_review_decision" {
				filtered = append(filtered, input)
			}
		}
		resolved.Descriptor.Stages[index].Inputs = filtered
	}
	runID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	run := store.WorkflowRun{
		ID: runID, WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringFixedFileTemplateVersion,
	}
	err = validateCurrentStandardAuthoringFrozenContract(run, runManifest{Resolved: resolved}, workflowadapter.RunExecutionSpec{Template: template.Reference()})
	if err == nil || !strings.Contains(err.Error(), "versioned input contract") {
		t.Fatalf("current descriptor without repair feedback = %v, want input-contract rejection", err)
	}
}

func TestCurrentStandardAuthoringRunRejectsNonCurrentVersions(t *testing.T) {
	current := store.WorkflowRun{WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID, WorkflowTemplateVersion: workflowadapter.StandardAuthoringFixedFileTemplateVersion}
	if !isCurrentStandardAuthoringRun(current) {
		t.Fatal("current Standard authoring version was rejected")
	}
	for _, version := range []string{
		workflowadapter.StandardAuthoringWorkflowTemplateVersion,
		workflowadapter.StandardAuthoringTaskAdmissionTemplateVersion,
		workflowadapter.StandardAuthoringBriefTemplateVersion,
		workflowadapter.StandardAuthoringRepairFeedbackTemplateVersion,
		workflowadapter.StandardAuthoringTestsAnalysisInputTemplateVersion,
		workflowadapter.StandardAuthoringHarnessTemplateVersion,
		"9.9.9",
	} {
		run := current
		run.WorkflowTemplateVersion = version
		if isCurrentStandardAuthoringRun(run) {
			t.Fatalf("non-current Standard authoring version %q was accepted", version)
		}
	}
}
