package app

import (
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// isCurrentStandardAuthoringRun is the sole app-level execution admission
// predicate for source/session authoring. Historical records can remain
// inspectable, but a new consolidated-V2 root has no legacy provider lock and
// must never execute their stage or handoff contracts.
func isCurrentStandardAuthoringRun(run store.WorkflowRun) bool {
	return run.WorkflowTemplateID == workflowadapter.StandardAuthoringWorkflowTemplateID &&
		run.WorkflowTemplateVersion == workflowadapter.StandardAuthoringWorkflowTemplateVersion
}

// validateCurrentStandardAuthoringFrozenContract proves that an executable
// AuthoringSession Run is consistently bound to the current policy-bearing
// template. It rejects a historical descriptor before any provider, artifact,
// or handoff path can reinterpret it under the current contract.
func validateCurrentStandardAuthoringFrozenContract(run store.WorkflowRun, manifest runManifest, specification workflowadapter.RunExecutionSpec) error {
	templateReference := workflowadapter.StandardAuthoringTemplateReference()
	if !isCurrentStandardAuthoringRun(run) {
		return fmt.Errorf("Standard authoring Run %s requires current template %s@%s", run.ID, templateReference.ID, templateReference.Version)
	}
	if !manifest.Resolved.Template.Equal(templateReference) || manifest.Resolved.TemplateID != templateReference.ID ||
		manifest.Resolved.TemplateVersion != templateReference.Version || !specification.Template.Equal(templateReference) ||
		manifest.Resolved.Descriptor.ID != templateReference.ID || manifest.Resolved.Descriptor.Version != templateReference.Version {
		return fmt.Errorf("Standard authoring Run %s template differs from its frozen current execution contract", run.ID)
	}
	if err := manifest.Resolved.Descriptor.Validate(); err != nil {
		return fmt.Errorf("validate Standard authoring Run %s frozen descriptor: %w", run.ID, err)
	}
	template, err := workflowadapter.ResolveWorkflowTemplate(templateReference)
	if err != nil {
		return fmt.Errorf("resolve Standard authoring Run %s frozen template: %w", run.ID, err)
	}
	for _, expectedStage := range template.Catalog.Stages {
		actualStage, found := manifest.Resolved.Descriptor.Stage(expectedStage.Key)
		if !found {
			return fmt.Errorf("Standard authoring Run %s frozen descriptor omits stage %q", run.ID, expectedStage.Key)
		}
		expectedPolicy, expectedUsesPolicy := standardAuthoringArtifactSpec(expectedStage.Inputs, workflowadapter.StandardAuthoringEnvironmentPolicyArtifact)
		actualPolicy, actualUsesPolicy := standardAuthoringArtifactSpec(actualStage.Inputs, workflowadapter.StandardAuthoringEnvironmentPolicyArtifact)
		if expectedUsesPolicy != actualUsesPolicy {
			return fmt.Errorf("Standard authoring Run %s frozen descriptor changes environment policy contract for stage %q", run.ID, expectedStage.Key)
		}
		if expectedUsesPolicy && (expectedPolicy != actualPolicy || !actualPolicy.Required || actualPolicy.SchemaVersion != workflowadapter.StandardAuthoringEnvironmentPolicySchemaVersion) {
			return fmt.Errorf("Standard authoring Run %s frozen descriptor has an invalid environment policy contract for stage %q", run.ID, expectedStage.Key)
		}
	}
	return nil
}

func standardAuthoringArtifactSpec(specifications []workflowkit.ArtifactSpec, name string) (workflowkit.ArtifactSpec, bool) {
	for _, specification := range specifications {
		if specification.Name == name {
			return specification, true
		}
	}
	return workflowkit.ArtifactSpec{}, false
}
