package app

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// isCurrentStandardAuthoringRun admits exactly the template version compiled
// into this binary. It is the strict identity used by new-run creation and
// launch flows, which must always bind the current contract.
func isCurrentStandardAuthoringRun(run store.WorkflowRun) bool {
	return workflowadapter.StandardAuthoringCurrentTemplateReference().Equal(workflowadapter.TemplateReference{
		ID: run.WorkflowTemplateID, Version: run.WorkflowTemplateVersion,
	})
}

// isAdmissibleStandardAuthoringRun admits the current template plus same-major
// family versions. A small version upgrade (for example 3.0.0 to 3.0.1) must
// not orphan Runs that are already queued, running, or waiting for review:
// their frozen manifests are self-describing, and
// validateCurrentStandardAuthoringFrozenContract still proves the frozen
// stage input shapes match the current catalog before any execution. Legacy
// major families (for example the 2.x pre-materialization contract) remain a
// hard cutover.
func isAdmissibleStandardAuthoringRun(run store.WorkflowRun) bool {
	if run.WorkflowTemplateID != workflowadapter.StandardAuthoringWorkflowTemplateID {
		return false
	}
	current, ok := standardAuthoringTemplateFamilyVersion(workflowadapter.StandardAuthoringCurrentTemplateReference().Version)
	if !ok {
		return false
	}
	actual, ok := standardAuthoringTemplateFamilyVersion(run.WorkflowTemplateVersion)
	return ok && actual == current
}

// standardAuthoringTemplateFamilyVersion parses the leading major component of
// a semantic template version ("3.0.0" -> 3). Non-semantic versions are not
// admissible because there is no principled compatibility boundary for them.
func standardAuthoringTemplateFamilyVersion(version string) (int, bool) {
	version = strings.TrimSpace(version)
	if version == "" {
		return 0, false
	}
	majorText, _, _ := strings.Cut(version, ".")
	major, err := strconv.Atoi(majorText)
	if err != nil || major < 0 {
		return 0, false
	}
	return major, true
}

// validateCurrentStandardAuthoringFrozenContract proves that an executable
// AuthoringSession Run is consistently bound to the current policy-bearing
// template family. It admits the current version and same-major versions whose
// frozen descriptor still matches the current catalog's versioned input
// contract; a historical or drifted descriptor is rejected before any
// provider, artifact, or handoff path can reinterpret it under the current
// contract.
func validateCurrentStandardAuthoringFrozenContract(run store.WorkflowRun, manifest runManifest, specification workflowadapter.RunExecutionSpec) error {
	templateReference := workflowadapter.TemplateReference{ID: run.WorkflowTemplateID, Version: run.WorkflowTemplateVersion}
	if !isCurrentStandardAuthoringRun(run) && !isAdmissibleStandardAuthoringRun(run) {
		return fmt.Errorf("Standard authoring Run %s requires current template registration for source/session execution", run.ID)
	}
	if !manifest.Resolved.Template.Equal(templateReference) || manifest.Resolved.TemplateID != templateReference.ID ||
		manifest.Resolved.TemplateVersion != templateReference.Version || !specification.Template.Equal(templateReference) ||
		manifest.Resolved.Descriptor.ID != templateReference.ID || manifest.Resolved.Descriptor.Version != templateReference.Version {
		return fmt.Errorf("Standard authoring Run %s template differs from its frozen current execution contract", run.ID)
	}
	if err := manifest.Resolved.Descriptor.Validate(); err != nil {
		return fmt.Errorf("validate Standard authoring Run %s frozen descriptor: %w", run.ID, err)
	}
	template, err := workflowadapter.ResolveWorkflowTemplate(workflowadapter.StandardAuthoringCurrentTemplateReference())
	if err != nil {
		return fmt.Errorf("resolve current Standard authoring template: %w", err)
	}
	for _, expectedStage := range template.Catalog.Stages {
		actualStage, found := manifest.Resolved.Descriptor.Stage(expectedStage.Key)
		if !found {
			return fmt.Errorf("Standard authoring Run %s frozen descriptor omits stage %q", run.ID, expectedStage.Key)
		}
		expectedContract, expectedUsesContract := standardAuthoringArtifactSpec(expectedStage.Inputs, workflowadapter.AuthoringContractArtifact)
		actualContract, actualUsesContract := standardAuthoringArtifactSpec(actualStage.Inputs, workflowadapter.AuthoringContractArtifact)
		if !expectedUsesContract || !actualUsesContract || expectedContract != actualContract || !actualContract.Required || actualContract.SchemaVersion != workflowadapter.AuthoringContractSchemaVersion {
			return fmt.Errorf("Standard authoring Run %s frozen descriptor has an invalid root contract for stage %q", run.ID, expectedStage.Key)
		}
		if !standardAuthoringInputContractSuperset(expectedStage.Inputs, actualStage.Inputs) || !reflect.DeepEqual(expectedStage.ReadSet, actualStage.ReadSet) {
			return fmt.Errorf("Standard authoring Run %s frozen descriptor changes the versioned input contract for stage %q", run.ID, expectedStage.Key)
		}
	}
	return nil
}

// standardAuthoringInputContractSuperset reports whether the frozen stage
// input contract is compatible with the current catalog. Every catalog
// required input must be present with the identical spec so execution never
// misses a mandatory port, and every frozen input must still be declared
// identically by the catalog so a drifted or removed shape is a hard cutover.
// A newer catalog may add optional inputs (for example a later
// package-admission report port) without orphaning Runs frozen against the
// previous contract.
func standardAuthoringInputContractSuperset(catalog, frozen []workflowkit.ArtifactSpec) bool {
	catalogByName := make(map[string]workflowkit.ArtifactSpec, len(catalog))
	for _, spec := range catalog {
		catalogByName[spec.Name] = spec
	}
	for _, spec := range catalog {
		if !spec.Required {
			continue
		}
		frozenSpec, found := frozenSpecByName(frozen, spec.Name)
		if !found || !reflect.DeepEqual(frozenSpec, spec) {
			return false
		}
	}
	for _, spec := range frozen {
		current, found := catalogByName[spec.Name]
		if !found || !reflect.DeepEqual(current, spec) {
			return false
		}
	}
	return true
}

func frozenSpecByName(frozen []workflowkit.ArtifactSpec, name string) (workflowkit.ArtifactSpec, bool) {
	for _, spec := range frozen {
		if spec.Name == name {
			return spec, true
		}
	}
	return workflowkit.ArtifactSpec{}, false
}

func standardAuthoringArtifactSpec(specifications []workflowkit.ArtifactSpec, name string) (workflowkit.ArtifactSpec, bool) {
	for _, specification := range specifications {
		if specification.Name == name {
			return specification, true
		}
	}
	return workflowkit.ArtifactSpec{}, false
}
