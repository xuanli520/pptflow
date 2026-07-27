package workflowkit

import (
	"errors"
	"reflect"
	"testing"
)

func TestPlanWorkflowRepairInvalidatesOnlyTargetAndResourceDependencyClosure(t *testing.T) {
	workflow := repairTestWorkflow()
	finding := repairTestFinding(t)
	plan, err := PlanWorkflowRepair(workflow, finding, repairTestRules(), nil)
	if err != nil {
		t.Fatalf("plan workflow repair: %v", err)
	}
	if !plan.RequiresFencedConversation || plan.CandidateRepairRound != 1 {
		t.Fatalf("repair plan fencing/round = %+v, want fenced first repair", plan)
	}
	if got, want := plan.InvalidatedNodes, []NodeID{"author", "resource_consumer", "critic", "materialize"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("invalidated nodes = %#v, want %#v", got, want)
	}
	for _, nodeID := range plan.InvalidatedNodes {
		if nodeID == "source" || nodeID == "independent" {
			t.Fatalf("repair invalidated unrelated node %q: %#v", nodeID, plan.InvalidatedNodes)
		}
	}
	if len(plan.Schedule) != 3 || !reflect.DeepEqual(plan.Schedule[0].NodeIDs, []NodeID{"author"}) || !reflect.DeepEqual(plan.Schedule[1].NodeIDs, []NodeID{"resource_consumer", "critic"}) || !reflect.DeepEqual(plan.Schedule[2].NodeIDs, []NodeID{"materialize"}) {
		t.Fatalf("repair schedule = %#v, want conflict-safe repair batches", plan.Schedule)
	}
}

func TestPlanWorkflowRepairSkipsPreRepairCandidateVerifier(t *testing.T) {
	workflow := WorkflowDescriptor{ID: "repair-order", Version: "1", Stages: []StageDescriptor{
		testStage("author", nil, EffectContentProducer, nil, []ResourceKey{"candidate/main"}),
		testStage("verify", []StageKey{"author"}, EffectEvidenceOnly, []ResourceKey{"candidate/main"}, []ResourceKey{"evidence/validation"}),
		testStage("critic", []StageKey{"verify"}, EffectEvidenceOnly, []ResourceKey{"candidate/main"}, []ResourceKey{"finding/quality"}),
		testStage("repair", []StageKey{"critic"}, EffectContentMutator, []ResourceKey{"candidate/main", "finding/quality"}, []ResourceKey{"candidate/main"}),
		testStage("review", []StageKey{"repair"}, EffectEvidenceOnly, []ResourceKey{"candidate/main"}, []ResourceKey{"review/content"}),
		testStage("admission", []StageKey{"review"}, EffectEvidenceOnly, []ResourceKey{"candidate/main", "task/metadata"}, []ResourceKey{"admission/report"}),
		testStage("materialize", []StageKey{"admission"}, EffectContentMutator, []ResourceKey{"candidate/main", "admission/report"}, []ResourceKey{"task/metadata"}),
	}}
	finding, err := NewWorkflowFinding(WorkflowFinding{
		Code: "candidate_integrity", ProducingStage: "critic", TargetWriter: "repair",
		EvidenceDigest: SHA256Fingerprint([]byte("critic-evidence")), CandidateDigest: SHA256Fingerprint([]byte("candidate")),
		DiagnosticDigest: SHA256Fingerprint([]byte("diagnostics")),
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanWorkflowRepair(workflow, finding, []WorkflowRepairRule{{
		FindingCode: "candidate_integrity", ProducingStage: "critic", TargetWriter: "repair",
		RequiresCandidateSnapshot: true, ConsumesCandidateRepair: true,
	}}, nil)
	if err != nil {
		t.Fatalf("plan repair after validation rejection: %v", err)
	}
	if got, want := plan.InvalidatedNodes, []NodeID{"repair", "review", "admission", "materialize"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("invalidated nodes = %#v, want %#v", got, want)
	}
}

func TestPlanWorkflowRepairEnforcesRulesSnapshotAndBoundedCandidateRounds(t *testing.T) {
	workflow := repairTestWorkflow()
	finding := repairTestFinding(t)
	missingSnapshot := finding
	missingSnapshot.CandidateDigest = ""
	if _, err := PlanWorkflowRepair(workflow, missingSnapshot, repairTestRules(), nil); !errors.Is(err, ErrInvalidWorkflowFinding) {
		t.Fatalf("missing candidate snapshot error = %v, want ErrInvalidWorkflowFinding", err)
	}
	wrongProducer := finding
	wrongProducer.ProducingStage = "source"
	if _, err := PlanWorkflowRepair(workflow, wrongProducer, repairTestRules(), nil); !errors.Is(err, ErrInvalidWorkflowFinding) {
		t.Fatalf("unruled producer error = %v, want ErrInvalidWorkflowFinding", err)
	}
	ledger := []WorkflowRepairLedgerEntry{
		{Finding: finding, ConsumedCandidateRound: true},
		{Finding: finding, ConsumedCandidateRound: true},
	}
	if _, err := PlanWorkflowRepair(workflow, finding, repairTestRules(), ledger); !errors.Is(err, ErrCandidateRepairExhausted) {
		t.Fatalf("exhausted candidate repair error = %v, want ErrCandidateRepairExhausted", err)
	}

	environmentRule := repairTestRules()[0]
	environmentRule.FindingCode = "environment_unavailable"
	environmentRule.RequiresCandidateSnapshot = false
	environmentRule.ConsumesCandidateRepair = false
	environmentFinding := finding
	environmentFinding.Code = environmentRule.FindingCode
	environmentFinding.CandidateDigest = ""
	environmentFinding.DiagnosticDigest = ""
	plan, err := PlanWorkflowRepair(workflow, environmentFinding, []WorkflowRepairRule{environmentRule}, ledger)
	if err != nil {
		t.Fatalf("plan environment repair without candidate budget: %v", err)
	}
	if plan.CandidateRepairRound != 0 {
		t.Fatalf("environment repair round = %d, want 0", plan.CandidateRepairRound)
	}
}

func TestWorkflowRepairLedgerCanonicalizesAndChargesOneRoundPerWriter(t *testing.T) {
	first := repairTestFinding(t)
	second := first
	second.Code = "second_defect"
	second.EvidenceDigest = SHA256Fingerprint([]byte("second evidence"))
	ledger, err := NewWorkflowRepairLedger([]WorkflowRepairLedgerEntry{
		{Finding: second}, {Finding: first, ConsumedCandidateRound: true},
	})
	if err != nil {
		t.Fatalf("create repair ledger: %v", err)
	}
	if len(ledger.Entries) != 2 || ledger.Entries[0].Finding.Code != "candidate_integrity" {
		t.Fatalf("canonical ledger entries = %+v", ledger.Entries)
	}
	duplicateCharge := ledger
	duplicateCharge.Entries[1].ConsumedCandidateRound = true
	if err := duplicateCharge.Validate(); !errors.Is(err, ErrInvalidWorkflowFinding) {
		t.Fatalf("duplicate candidate charge error = %v, want ErrInvalidWorkflowFinding", err)
	}
}

func repairTestWorkflow() WorkflowDescriptor {
	return WorkflowDescriptor{ID: "repair-workflow", Version: "1", Stages: []StageDescriptor{
		testStage("source", nil, EffectEvidenceOnly, nil, []ResourceKey{"source/snapshot"}),
		testStage("author", []StageKey{"source"}, EffectContentProducer, []ResourceKey{"source/snapshot"}, []ResourceKey{"candidate/main"}),
		testStage("resource_consumer", nil, EffectEvidenceOnly, []ResourceKey{"candidate/main"}, []ResourceKey{"evidence/resource"}),
		testStage("critic", []StageKey{"author"}, EffectEvidenceOnly, []ResourceKey{"candidate/main"}, []ResourceKey{"evidence/critic"}),
		testStage("materialize", []StageKey{"critic"}, EffectContentMutator, []ResourceKey{"evidence/critic"}, []ResourceKey{"task/materialized"}),
		testStage("independent", nil, EffectEvidenceOnly, nil, []ResourceKey{"evidence/independent"}),
	}}
}

func repairTestFinding(t *testing.T) WorkflowFinding {
	t.Helper()
	finding, err := NewWorkflowFinding(WorkflowFinding{
		Code: "candidate_integrity", ProducingStage: "critic", TargetWriter: "author",
		EvidenceDigest: SHA256Fingerprint([]byte("critic-evidence")), CandidateDigest: SHA256Fingerprint([]byte("candidate")),
		DiagnosticDigest: SHA256Fingerprint([]byte("diagnostics")),
	})
	if err != nil {
		t.Fatal(err)
	}
	return finding
}

func repairTestRules() []WorkflowRepairRule {
	return []WorkflowRepairRule{{
		FindingCode: "candidate_integrity", ProducingStage: "critic", TargetWriter: "author",
		RequiresCandidateSnapshot: true, ConsumesCandidateRepair: true,
	}}
}
