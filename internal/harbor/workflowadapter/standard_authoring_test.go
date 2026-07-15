package workflowadapter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringTemplateIsClosedPreMaterializationWorkflow(t *testing.T) {
	template := StandardAuthoringWorkflowTemplate()
	if err := template.Validate(); err != nil {
		t.Fatalf("validate Standard authoring template: %v", err)
	}
	if !template.Reference().Equal(StandardAuthoringTemplateReference()) {
		t.Fatalf("template reference = %+v", template.Reference())
	}
	if len(template.Catalog.Stages) != len(StandardAuthoringStageOrder()) {
		t.Fatalf("authoring stage count = %d, want %d", len(template.Catalog.Stages), len(StandardAuthoringStageOrder()))
	}
	for _, forbidden := range []workflowkit.StageKey{
		workflowkit.StageKey(TaskRepair), workflowkit.StageKey(RuntimeSelfCheck), workflowkit.StageKey(HarborRunQwen), workflowkit.StageKey(Package),
	} {
		if _, present := template.Catalog.Stage(forbidden); present {
			t.Fatalf("source-session authoring catalog unexpectedly contains task-bound stage %q", forbidden)
		}
	}
	materialize, present := template.Catalog.Stage(workflowkit.StageKey(MaterializeTask))
	if !present {
		t.Fatal("authoring catalog lacks materialize_task")
	}
	foundHandoff := false
	for _, output := range materialize.Outputs {
		if output.Name == StandardAuthoringTaskHandoffArtifact && output.SchemaVersion == StandardAuthoringTaskHandoffSchemaVersion && output.Required {
			foundHandoff = true
		}
	}
	if !foundHandoff {
		t.Fatalf("materialize_task outputs = %+v, want required immutable handoff", materialize.Outputs)
	}
	if template.QuotaPolicy.ID != StandardAuthoringQuotaPolicyID || template.QuotaPolicy.Version != StandardAuthoringQuotaPolicyVersion {
		t.Fatalf("authoring quota policy = %s@%s", template.QuotaPolicy.ID, template.QuotaPolicy.Version)
	}
}

func TestStandardAuthoringTaskHandoffStrictlyBindsCodeEdgeChild(t *testing.T) {
	handoff := standardAuthoringTaskHandoffFixture()
	if err := handoff.Validate(); err != nil {
		t.Fatal(err)
	}
	selection, err := handoff.ChildSelection()
	if err != nil {
		t.Fatal(err)
	}
	if !selection.IsTaskRevision() || selection.TaskID != handoff.TaskID || selection.RevisionID != handoff.RevisionID || selection.RevisionDigest != handoff.RevisionDigest {
		t.Fatalf("child selection = %+v, want generated task revision", selection)
	}
	canonical, err := handoff.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseStandardAuthoringTaskHandoffJSON(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != handoff {
		t.Fatalf("parsed handoff = %+v, want %+v", parsed, handoff)
	}
	fingerprint, err := handoff.Fingerprint()
	if err != nil || fingerprint == "" {
		t.Fatalf("handoff fingerprint = %q, %v", fingerprint, err)
	}
	var direct StandardAuthoringTaskHandoff
	if err := json.Unmarshal(canonical, &direct); err != nil || direct != handoff {
		t.Fatalf("direct strict handoff decode = %+v, %v", direct, err)
	}

	wrongChild := handoff
	wrongChild.ChildTemplate = StandardTemplateReference()
	if err := wrongChild.Validate(); err == nil || !strings.Contains(err.Error(), "child template") {
		t.Fatalf("non-CodeEdge child = %v, want exact child-template rejection", err)
	}
	unknown := []byte(strings.Replace(string(canonical), `"version":"1"`, `"version":"1","unexpected":true`, 1))
	if _, err := ParseStandardAuthoringTaskHandoffJSON(unknown); err == nil {
		t.Fatal("handoff accepted unknown field")
	}
}

func standardAuthoringTaskHandoffFixture() StandardAuthoringTaskHandoff {
	return StandardAuthoringTaskHandoff{
		Format: StandardAuthoringTaskHandoffFormat, Version: StandardAuthoringTaskHandoffVersion,
		AuthoringSourceID:     "018f0a73-3b49-7000-8000-000000000010",
		AuthoringSessionID:    "018f0a73-3b49-7000-8000-000000000011",
		AuthoringRunID:        "018f0a73-3b49-7000-8000-000000000012",
		AuthoringSourceDigest: workflowkit.SubjectDigest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		TaskID:                "018f0a73-3b49-7000-8000-000000000013",
		RevisionID:            "018f0a73-3b49-7000-8000-000000000014",
		RevisionDigest:        workflowkit.SubjectDigest("harbor.task.v2:sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		TaskSnapshot: ArtifactReference{
			ID: "018f0a73-3b49-7000-8000-000000000015", ContentDigest: workflowkit.SHA256Fingerprint([]byte("task snapshot")), SchemaVersion: "harbor.artifact.v1",
		},
		ChildTemplate: CodeEdgePhase1TemplateReference(),
	}
}
