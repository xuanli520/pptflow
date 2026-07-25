package workflowadapter

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringV2IsClosedPreMaterializationWorkflow(t *testing.T) {
	template := StandardAuthoringCurrentWorkflowTemplate()
	if err := template.Validate(); err != nil {
		t.Fatalf("validate Standard authoring v2 template: %v", err)
	}
	if !template.Reference().Equal(StandardAuthoringContractTemplateReference()) || !IsStandardAuthoringWorkflowTemplate(template.Reference()) {
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
	if !present || !stageHasArtifact(materialize.Outputs, StandardAuthoringTaskHandoffArtifact, StandardAuthoringTaskHandoffSchemaVersion) {
		t.Fatalf("materialize_task = %+v, want required immutable v2 handoff", materialize)
	}
	if template.QuotaPolicy.ID != StandardAuthoringQuotaPolicyID || template.QuotaPolicy.Version != StandardAuthoringContractQuotaPolicyVersion {
		t.Fatalf("authoring quota policy = %s@%s", template.QuotaPolicy.ID, template.QuotaPolicy.Version)
	}
}

func TestStandardAuthoringTaskDesignTurnBudgetIsScopedToAuthoring(t *testing.T) {
	authoring, found := StandardAuthoringCurrentWorkflowTemplate().Catalog.Stage(workflowkit.StageKey(TaskDesign))
	if !found || authoring.RequiredTurns != StandardAuthoringTaskDesignMaxTurns {
		t.Fatalf("authoring task_design turns = %+v, found=%t; want %d", authoring, found, StandardAuthoringTaskDesignMaxTurns)
	}
	policy := StandardAuthoringContractQuotaPolicy()
	var claims []workflowkit.QuotaClaim
	found = false
	for _, stage := range policy.Stages {
		if stage.StageKey != workflowkit.StageKey(TaskDesign) {
			continue
		}
		claims = append([]workflowkit.QuotaClaim(nil), stage.Claims...)
		found = true
		break
	}
	if !found || !hasQuotaClaim(claims, "agent_turn", int64(StandardAuthoringTaskDesignMaxTurns)) {
		t.Fatalf("authoring task_design quota claims = %+v, found=%t", claims, found)
	}
	var totalAgentTurns int64
	for _, stage := range policy.Stages {
		for _, claim := range stage.Claims {
			if claim.Dimension == "agent_turn" {
				totalAgentTurns += claim.Units
			}
		}
	}
	var taskLimit int64
	for _, limit := range policy.AccountLimits {
		if limit.Dimension == "agent_turn" {
			taskLimit = limit.TaskLimitUnits
			break
		}
	}
	if taskLimit != 64 || totalAgentTurns > taskLimit {
		t.Fatalf("authoring agent-turn reservation = %d/%d", totalAgentTurns, taskLimit)
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
	if !reflect.DeepEqual(parsed, handoff) {
		t.Fatalf("parsed handoff = %+v, want %+v", parsed, handoff)
	}
	fingerprint, err := handoff.Fingerprint()
	if err != nil || fingerprint == "" {
		t.Fatalf("handoff fingerprint = %q, %v", fingerprint, err)
	}
	var direct StandardAuthoringTaskHandoff
	if err := json.Unmarshal(canonical, &direct); err != nil || !reflect.DeepEqual(direct, handoff) {
		t.Fatalf("direct strict handoff decode = %+v, %v", direct, err)
	}

	wrongChild := handoff
	wrongChild.ChildTemplate = StandardTemplateReference()
	if err := wrongChild.Validate(); err == nil || !strings.Contains(err.Error(), "child template") {
		t.Fatalf("non-CodeEdge child = %v, want exact child-template rejection", err)
	}
	unknown := []byte(strings.Replace(string(canonical), `"version":"2"`, `"version":"2","unexpected":true`, 1))
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
		AdmissionReceipt: &ArtifactReference{
			ID: "018f0a73-3b49-7000-8000-000000000016", ContentDigest: workflowkit.SHA256Fingerprint([]byte("admission receipt")), SchemaVersion: "harbor.standard-authoring-task-package-admission.v1",
		},
		ChildTemplate: CodeEdgePhase1TemplateReference(),
	}
}
