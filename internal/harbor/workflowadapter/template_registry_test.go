package workflowadapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClosedTemplateRegistryResolvesExactVersionsWithoutFallback(t *testing.T) {
	registry := DefaultTemplateRegistry()
	for _, reference := range []TemplateReference{
		StandardTemplateReference(),
		StandardAuthoringCurrentTemplateReference(),
	} {
		template, err := registry.ResolveTemplate(reference)
		if err != nil {
			t.Fatalf("resolve %s@%s: %v", reference.ID, reference.Version, err)
		}
		if !template.Reference().Equal(reference) {
			t.Fatalf("resolved template reference = %#v, want %#v", template.Reference(), reference)
		}
	}

	standard, err := registry.ResolveTemplate(StandardTemplateReference())
	if err != nil {
		t.Fatal(err)
	}
	standard.Catalog.Stages[0].Plugin.Version = "mutated-test-copy"
	again, err := registry.ResolveTemplate(StandardTemplateReference())
	if err != nil {
		t.Fatal(err)
	}
	if again.Catalog.Stages[0].Plugin.Version == "mutated-test-copy" {
		t.Fatal("closed registry leaked a mutable template snapshot")
	}

	for _, reference := range []TemplateReference{
		{ID: StandardWorkflowTemplateID, Version: "2.1.1"},
		{ID: StandardAuthoringWorkflowTemplateID, Version: "1.0.0"},
		{ID: "harbor.not-installed", Version: "1.0.0"},
		{ID: StandardWorkflowTemplateID},
	} {
		if _, err := registry.ResolveTemplate(reference); err == nil {
			t.Fatalf("unregistered reference %#v unexpectedly resolved", reference)
		}
	}
}

func TestStandardAuthoringV2RegistryRejectsAllLegacyVersions(t *testing.T) {
	current := StandardAuthoringCurrentWorkflowTemplate()
	if current.Version != StandardAuthoringContractTemplateVersion || current.Catalog.Version != StandardAuthoringContractTemplateVersion || !current.Reference().Equal(StandardAuthoringCurrentTemplateReference()) {
		t.Fatalf("current Standard authoring template = %s@%s catalog %s@%s", current.ID, current.Version, current.Catalog.ID, current.Catalog.Version)
	}
	if err := current.Validate(); err != nil {
		t.Fatalf("validate current Standard authoring template: %v", err)
	}

	for _, version := range []string{"1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0", "1.7.0", "1.8.0"} {
		retired := TemplateReference{ID: StandardAuthoringWorkflowTemplateID, Version: version}
		if err := retired.Validate(); err == nil {
			t.Fatalf("legacy template %s was accepted", version)
		}
		if _, err := ResolveWorkflowTemplate(retired); err == nil {
			t.Fatalf("legacy template %s resolved from the default registry", version)
		}
		if IsStandardAuthoringWorkflowTemplate(retired) {
			t.Fatalf("legacy template %s was recognized as executable", version)
		}
	}
}

func TestExplicitTemplateBindingRejectsCrossTemplateAndTemplateLessDocuments(t *testing.T) {
	standardTemplate := StandardWorkflowTemplate()
	authoringTemplate := StandardAuthoringCurrentWorkflowTemplate()
	standardProfile := explicitProfile(standardTemplate.Catalog)
	authoringProfile := explicitProfile(authoringTemplate.Catalog)

	if _, err := standardTemplate.Compile(authoringProfile); err == nil || !strings.Contains(err.Error(), "template reference mismatch") {
		t.Fatalf("standard compile with authoring profile = %v, want exact-template rejection", err)
	}
	if _, err := authoringTemplate.Compile(standardProfile); err == nil || !strings.Contains(err.Error(), "template reference mismatch") {
		t.Fatalf("authoring compile with Standard profile = %v, want exact-template rejection", err)
	}

	templateLessProfile := standardProfile.Clone()
	templateLessProfile.Template = TemplateReference{}
	if err := templateLessProfile.Validate(); err == nil || !strings.Contains(err.Error(), "template") {
		t.Fatalf("template-less profile validation = %v, want rejection", err)
	}

	standardSpec := testRunExecutionSpec(t)
	if err := standardSpec.ValidateFor(authoringTemplate.Catalog); err == nil || !strings.Contains(err.Error(), "template reference mismatch") {
		t.Fatalf("Standard spec against authoring catalog = %v, want cross-template rejection", err)
	}
	templateLessSpec := standardSpec.Clone()
	templateLessSpec.Template = TemplateReference{}
	if err := templateLessSpec.Validate(); err == nil || !strings.Contains(err.Error(), "template") {
		t.Fatalf("template-less specification validation = %v, want rejection", err)
	}

	profileRaw, err := standardProfile.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var profileDocument map[string]any
	if err := json.Unmarshal(profileRaw, &profileDocument); err != nil {
		t.Fatal(err)
	}
	delete(profileDocument, "template")
	templateLessProfileRaw, err := json.Marshal(profileDocument)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseExecutionProfileJSON(templateLessProfileRaw); err == nil {
		t.Fatal("parser accepted a legacy template-less execution profile")
	}

	specRaw, err := json.Marshal(standardSpec)
	if err != nil {
		t.Fatal(err)
	}
	var specDocument map[string]any
	if err := json.Unmarshal(specRaw, &specDocument); err != nil {
		t.Fatal(err)
	}
	delete(specDocument, "template")
	templateLessSpecRaw, err := json.Marshal(specDocument)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRunExecutionSpecJSON(templateLessSpecRaw); err == nil {
		t.Fatal("parser accepted a legacy template-less run execution specification")
	}
}

func TestTemplateReferenceParticipatesInProfileAndSpecFingerprints(t *testing.T) {
	standardProfile := explicitProfile(StandardStageCatalog())
	authoringProfile := explicitProfile(StandardAuthoringCurrentWorkflowTemplate().Catalog)
	standardProfileFingerprint, err := standardProfile.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	authoringProfileFingerprint, err := authoringProfile.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if standardProfileFingerprint == authoringProfileFingerprint {
		t.Fatal("template-bound profiles unexpectedly share a fingerprint")
	}

	standardSpec := testRunExecutionSpec(t)
	authoringSpec := testStandardAuthoringExecutionSpec(t)
	standardSpecFingerprint, err := standardSpec.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	authoringSpecFingerprint, err := authoringSpec.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if standardSpecFingerprint == authoringSpecFingerprint {
		t.Fatal("template-bound specifications unexpectedly share a fingerprint")
	}

	resolved, err := StandardAuthoringCurrentWorkflowTemplate().Compile(authoringProfile)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Template.Equal(StandardAuthoringCurrentTemplateReference()) || resolved.TemplateID != StandardAuthoringWorkflowTemplateID || resolved.TemplateVersion != StandardAuthoringContractTemplateVersion {
		t.Fatalf("resolved authoring template identity = %#v / %s@%s", resolved.Template, resolved.TemplateID, resolved.TemplateVersion)
	}
}
