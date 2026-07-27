package app

import (
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestCurrentStandardAuthoringRuntimeContractRejectsLegacyRun(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
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

func TestStandardAuthoringRuntimeContractRejectsCurrentDescriptorWithoutRootContract(t *testing.T) {
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
			if input.Name != workflowadapter.AuthoringContractArtifact {
				filtered = append(filtered, input)
			}
		}
		resolved.Descriptor.Stages[index].Inputs = filtered
		if resolved.Descriptor.Stages[index].AgentRole != nil {
			resolved.Descriptor.Stages[index].AgentRole.InputSchemas = append([]workflowkit.ArtifactSpec(nil), filtered...)
		}
	}
	runID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	run := store.WorkflowRun{
		ID: runID, WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringContractTemplateVersion,
	}
	err = validateCurrentStandardAuthoringFrozenContract(run, runManifest{Resolved: resolved}, workflowadapter.RunExecutionSpec{Template: template.Reference()})
	if err == nil || !strings.Contains(err.Error(), "invalid root contract") {
		t.Fatalf("current descriptor without root contract = %v, want root-contract rejection", err)
	}
}

func TestStandardAuthoringRuntimeContractRejectsRootContractSchemaDrift(t *testing.T) {
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
			if input.Name == workflowadapter.AuthoringContractArtifact {
				input.SchemaVersion = "harbor.standard-authoring-contract.v1"
			}
			filtered = append(filtered, input)
		}
		resolved.Descriptor.Stages[index].Inputs = filtered
		if resolved.Descriptor.Stages[index].AgentRole != nil {
			resolved.Descriptor.Stages[index].AgentRole.InputSchemas = append([]workflowkit.ArtifactSpec(nil), filtered...)
		}
	}
	runID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	run := store.WorkflowRun{
		ID: runID, WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringContractTemplateVersion,
	}
	err = validateCurrentStandardAuthoringFrozenContract(run, runManifest{Resolved: resolved}, workflowadapter.RunExecutionSpec{Template: template.Reference()})
	if err == nil || !strings.Contains(err.Error(), "invalid root contract") {
		t.Fatalf("current descriptor with root-contract schema drift = %v, want root-contract rejection", err)
	}
}

func TestStandardAuthoringRuntimeContractRejectsCurrentDescriptorInputDrift(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	profile := lifecycleCompleteProfileForTemplate(t, template)
	resolved, err := template.Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	for index := range resolved.Descriptor.Stages {
		if resolved.Descriptor.Stages[index].Key != workflowkit.StageKey(workflowadapter.TaskSynthesis) {
			continue
		}
		inputs := resolved.Descriptor.Stages[index].Inputs
		filtered := inputs[:0]
		for _, input := range inputs {
			if input.Name != "repo_structure_evidence" {
				filtered = append(filtered, input)
			}
		}
		resolved.Descriptor.Stages[index].Inputs = filtered
		if resolved.Descriptor.Stages[index].AgentRole != nil {
			resolved.Descriptor.Stages[index].AgentRole.InputSchemas = append([]workflowkit.ArtifactSpec(nil), filtered...)
		}
	}
	runID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	run := store.WorkflowRun{
		ID: runID, WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringContractTemplateVersion,
	}
	err = validateCurrentStandardAuthoringFrozenContract(run, runManifest{Resolved: resolved}, workflowadapter.RunExecutionSpec{Template: template.Reference()})
	if err == nil || !strings.Contains(err.Error(), "versioned input contract") {
		t.Fatalf("current descriptor without repository-structure evidence = %v, want input-contract rejection", err)
	}
}

func TestCurrentStandardAuthoringRunRejectsNonCurrentVersions(t *testing.T) {
	current := store.WorkflowRun{WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID, WorkflowTemplateVersion: workflowadapter.StandardAuthoringContractTemplateVersion}
	if !isCurrentStandardAuthoringRun(current) {
		t.Fatal("current Standard authoring version was rejected")
	}
	for _, version := range []string{
		"1.0.0",
		"1.2.0",
		"1.3.0",
		"1.4.0",
		"1.5.0",
		"1.6.0",
		"1.7.0",
		"1.8.0",
		"9.9.9",
	} {
		run := current
		run.WorkflowTemplateVersion = version
		if isCurrentStandardAuthoringRun(run) {
			t.Fatalf("non-current Standard authoring version %q was accepted", version)
		}
	}
}
