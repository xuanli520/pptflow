package workflowadapter

import (
	"reflect"
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringV3CatalogFreezesRoleAndWorkspaceAuthority(t *testing.T) {
	template := StandardAuthoringCurrentWorkflowTemplate()
	if err := template.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, key := range []workflowkit.StageKey{
		workflowkit.StageKey(RepoStructureResearch), workflowkit.StageKey(TestRuntimeResearch), workflowkit.StageKey(VerifierThreatResearch),
		workflowkit.StageKey(TaskSynthesis), workflowkit.StageKey(AuthoringLoop), workflowkit.StageKey(TestQualityCritic), workflowkit.StageKey(SolutionIntegrityCritic), workflowkit.StageKey(AuthoringRepair),
	} {
		stage, found := template.Catalog.Stage(key)
		if !found || stage.AgentRole == nil || stage.Concurrency == nil {
			t.Fatalf("3.0 agent stage %q = %+v, found=%t", key, stage, found)
		}
	}

	author, _ := template.Catalog.Stage(workflowkit.StageKey(AuthoringLoop))
	repair, _ := template.Catalog.Stage(workflowkit.StageKey(AuthoringRepair))
	for _, stage := range []StageDefinition{author, repair} {
		wantValidationAttempts := 8
		if stage.Key == workflowkit.StageKey(AuthoringRepair) {
			wantValidationAttempts = StandardAuthoringRepairMaxTurns
		}
		if stage.AgentRole.RoleID != workflowkit.AgentRoleAuthor || stage.AgentRole.OutputMode != workflowkit.AgentOutputCandidateSnapshot ||
			stage.Concurrency.Workspace.Mode != workflowkit.WorkspaceExclusiveWriter || stage.Concurrency.Workspace.Key != "authoring-candidate" ||
			stage.AgentRole.MaxValidationAttempts != wantValidationAttempts {
			t.Fatalf("writer stage %q authority = %+v", stage.Key, stage.AgentRole)
		}
	}
	for _, key := range []workflowkit.StageKey{workflowkit.StageKey(TestQualityCritic), workflowkit.StageKey(SolutionIntegrityCritic)} {
		critic, _ := template.Catalog.Stage(key)
		if critic.AgentRole.RoleID != workflowkit.AgentRoleCritic || critic.AgentRole.OutputMode != workflowkit.AgentOutputFinding ||
			critic.Concurrency.Workspace.Mode != workflowkit.WorkspaceReadOnlySnapshot || critic.Concurrency.Workspace.Key != "authoring-candidate-critic" {
			t.Fatalf("critic stage %q authority = %+v", key, critic.AgentRole)
		}
	}
}

func TestStandardAuthoringV3TopologySchedulesParallelResearchAndCritics(t *testing.T) {
	template := StandardAuthoringCurrentWorkflowTemplate()
	resolved, err := template.Compile(explicitProfile(template.Catalog))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := workflowkit.CompileDependencyExecutionPlan(resolved.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) < 6 {
		t.Fatalf("3.0 schedule = %#v", plan.Batches)
	}
	if !scheduleContainsBatch(plan.Batches, []workflowkit.NodeID{workflowkit.NodeID(RepoStructureResearch), workflowkit.NodeID(TestRuntimeResearch), workflowkit.NodeID(VerifierThreatResearch)}) {
		t.Fatalf("schedule omitted concurrent research batch: %#v", plan.Batches)
	}
	if !scheduleContainsBatch(plan.Batches, []workflowkit.NodeID{workflowkit.NodeID(TestQualityCritic), workflowkit.NodeID(SolutionIntegrityCritic)}) {
		t.Fatalf("schedule omitted concurrent critic batch: %#v", plan.Batches)
	}
	if !scheduleContainsBatch(plan.Batches, []workflowkit.NodeID{workflowkit.NodeID(AuthoringRepair)}) {
		t.Fatalf("schedule omitted repair batch after critics: %#v", plan.Batches)
	}
	if !batchAfter(plan.Batches, []workflowkit.NodeID{workflowkit.NodeID(TestQualityCritic), workflowkit.NodeID(SolutionIntegrityCritic)}, []workflowkit.NodeID{workflowkit.NodeID(AuthoringRepair)}) {
		t.Fatalf("repair batch must follow critic batch so findings are frozen into its input fingerprint: %#v", plan.Batches)
	}
	policy := template.QuotaPolicy
	for _, limit := range policy.AccountLimits {
		if limit.Dimension == "repair_round" && limit.TaskLimitUnits != 8 {
			t.Fatalf("candidate repair limit = %+v, want eight rounds", limit)
		}
	}
}

func scheduleContainsBatch(batches []workflowkit.ScheduleBatch, wanted []workflowkit.NodeID) bool {
	for _, batch := range batches {
		if reflect.DeepEqual(batch.NodeIDs, wanted) {
			return true
		}
	}
	return false
}

func batchAfter(batches []workflowkit.ScheduleBatch, earlier, later []workflowkit.NodeID) bool {
	earlierIndex, laterIndex := -1, -1
	for index, batch := range batches {
		if reflect.DeepEqual(batch.NodeIDs, earlier) {
			earlierIndex = index
		}
		if reflect.DeepEqual(batch.NodeIDs, later) {
			laterIndex = index
		}
	}
	return earlierIndex >= 0 && laterIndex > earlierIndex
}
