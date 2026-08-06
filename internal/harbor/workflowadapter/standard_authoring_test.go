package workflowadapter

import (
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringV3IsClosedPreMaterializationWorkflow(t *testing.T) {
	template := StandardAuthoringCurrentWorkflowTemplate()
	if err := template.Validate(); err != nil {
		t.Fatalf("validate Standard authoring 3.0 template: %v", err)
	}
	if !template.Reference().Equal(StandardAuthoringContractTemplateReference()) || !IsStandardAuthoringWorkflowTemplate(template.Reference()) {
		t.Fatalf("template reference = %+v", template.Reference())
	}
	if len(template.Catalog.Stages) != len(StandardAuthoringStageOrder()) {
		t.Fatalf("authoring stage count = %d, want %d", len(template.Catalog.Stages), len(StandardAuthoringStageOrder()))
	}
	for _, forbidden := range []workflowkit.StageKey{
		workflowkit.StageKey(RepoAnalyze), workflowkit.StageKey(TaskDesign), workflowkit.StageKey(GenerateTaskFiles), workflowkit.StageKey(AuthoringHarness), workflowkit.StageKey(TaskRepair), workflowkit.StageKey(RuntimeSelfCheck), workflowkit.StageKey(Package),
	} {
		if _, present := template.Catalog.Stage(forbidden); present {
			t.Fatalf("source-session authoring catalog unexpectedly contains task-bound stage %q", forbidden)
		}
	}
	materialize, present := template.Catalog.Stage(workflowkit.StageKey(MaterializeTask))
	if !present || !stageHasArtifact(materialize.Outputs, StandardAuthoringMaterializationReceiptArtifact, StandardAuthoringMaterializationReceiptSchemaVersion) {
		t.Fatalf("materialize_task = %+v, want required materialization receipt", materialize)
	}
	if template.QuotaPolicy.ID != StandardAuthoringQuotaPolicyID || template.QuotaPolicy.Version != StandardAuthoringContractQuotaPolicyVersion {
		t.Fatalf("authoring quota policy = %s@%s", template.QuotaPolicy.ID, template.QuotaPolicy.Version)
	}
}

func TestStandardAuthoringAuthoringLoopTurnBudgetIsScopedToAuthoring(t *testing.T) {
	authoring, found := StandardAuthoringCurrentWorkflowTemplate().Catalog.Stage(workflowkit.StageKey(AuthoringLoop))
	if !found || authoring.RequiredTurns != StandardAuthoringAuthoringLoopMaxTurns {
		t.Fatalf("authoring_loop turns = %+v, found=%t; want %d", authoring, found, StandardAuthoringAuthoringLoopMaxTurns)
	}
	policy := StandardAuthoringContractQuotaPolicy()
	var claims []workflowkit.QuotaClaim
	found = false
	for _, stage := range policy.Stages {
		if stage.StageKey != workflowkit.StageKey(AuthoringLoop) {
			continue
		}
		claims = append([]workflowkit.QuotaClaim(nil), stage.Claims...)
		found = true
		break
	}
	if !found || !hasQuotaClaim(claims, "agent_turn", int64(StandardAuthoringAuthoringLoopMaxTurns)) {
		t.Fatalf("authoring_loop quota claims = %+v, found=%t", claims, found)
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
