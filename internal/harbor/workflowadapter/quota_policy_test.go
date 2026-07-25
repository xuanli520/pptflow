package workflowadapter

import (
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardQuotaPolicyCompilesExplicitAccountLimitsAndStageClaims(t *testing.T) {
	template := StandardWorkflowTemplate()
	policy := template.QuotaPolicy
	if policy.ID != StandardQuotaPolicyID || policy.Version != StandardQuotaPolicyVersion {
		t.Fatalf("standard policy = %s@%s, want %s@%s", policy.ID, policy.Version, StandardQuotaPolicyID, StandardQuotaPolicyVersion)
	}
	if err := policy.ValidateFor(template.Catalog); err != nil {
		t.Fatalf("validate standard quota policy: %v", err)
	}

	resolved, err := template.Compile(explicitProfile(template.Catalog))
	if err != nil {
		t.Fatalf("compile workflow with standard quota policy: %v", err)
	}
	if err := resolved.QuotaPolicy.ValidateForDescriptor(resolved.Descriptor); err != nil {
		t.Fatalf("resolved policy/descriptor mismatch: %v", err)
	}
	if resolved.QuotaPolicy.Fingerprint == "" {
		t.Fatal("resolved quota policy did not persist a fingerprint")
	}

	limits := make(map[string]QuotaAccountLimit, len(resolved.QuotaPolicy.AccountLimits))
	for _, limit := range resolved.QuotaPolicy.AccountLimits {
		limits[limit.Dimension] = limit
	}
	for dimension, want := range map[string]QuotaAccountLimit{
		"stage_attempt": {Dimension: "stage_attempt", TaskLimitUnits: 120, ActorLimitUnits: 1200},
		"agent_turn":    {Dimension: "agent_turn", TaskLimitUnits: 64, ActorLimitUnits: 640},
		"trial":         {Dimension: "trial", TaskLimitUnits: 32, ActorLimitUnits: 320},
		"repair_round":  {Dimension: "repair_round", TaskLimitUnits: 3, ActorLimitUnits: 30},
	} {
		if got, present := limits[dimension]; !present || got != want {
			t.Fatalf("quota limit %q = %+v, present=%v; want %+v", dimension, got, present, want)
		}
	}

	for _, definition := range template.Catalog.Stages {
		stage, present := resolved.Descriptor.Stage(definition.Key)
		if !present {
			t.Fatalf("compiled descriptor omitted %q", definition.Key)
		}
		claims, present := resolved.QuotaPolicy.ClaimsFor(definition.Key)
		if !present || !sameFrozenQuotaClaims(t, stage.QuotaClaims, claims) {
			t.Fatalf("stage %q claims = %+v, policy claims = %+v", definition.Key, stage.QuotaClaims, claims)
		}
		if definition.IsGate() && len(stage.QuotaClaims) != 0 {
			t.Fatalf("gate %q unexpectedly claims quota: %+v", definition.Key, stage.QuotaClaims)
		}
		if !definition.IsGate() && !hasQuotaClaim(stage.QuotaClaims, "stage_attempt", 1) {
			t.Fatalf("executable stage %q lacks stage_attempt claim: %+v", definition.Key, stage.QuotaClaims)
		}
	}

	repoAnalyze, _ := resolved.Descriptor.Stage(workflowkit.StageKey(RepoAnalyze))
	if !hasQuotaClaim(repoAnalyze.QuotaClaims, "agent_turn", 3) {
		t.Fatalf("repo analysis claims = %+v, want three frozen agent turns", repoAnalyze.QuotaClaims)
	}
	qwen, _ := resolved.Descriptor.Stage(workflowkit.StageKey(HarborRunQwen))
	if !hasQuotaClaim(qwen.QuotaClaims, "trial", 4) {
		t.Fatalf("Qwen evaluation claims = %+v, want four logical trials", qwen.QuotaClaims)
	}
	repair, _ := resolved.Descriptor.Stage(workflowkit.StageKey(TaskRepair))
	if !hasQuotaClaim(repair.QuotaClaims, "repair_round", 1) {
		t.Fatalf("repair claims = %+v, want one repair round", repair.QuotaClaims)
	}
}

func TestStandardAuthoringQuotaPolicySeparatelyBoundsOutputSubmissions(t *testing.T) {
	template := StandardAuthoringCurrentWorkflowTemplate()
	policy := template.QuotaPolicy
	if policy.ID != StandardAuthoringQuotaPolicyID || policy.Version != StandardAuthoringContractQuotaPolicyVersion {
		t.Fatalf("Standard authoring policy = %s@%s, want %s@%s", policy.ID, policy.Version, StandardAuthoringQuotaPolicyID, StandardAuthoringContractQuotaPolicyVersion)
	}
	if err := policy.ValidateFor(template.Catalog); err != nil {
		t.Fatalf("validate Standard authoring quota policy: %v", err)
	}
	resolved, err := policy.ResolveFor(template.Catalog)
	if err != nil {
		t.Fatalf("resolve Standard authoring quota policy: %v", err)
	}

	limits := make(map[string]QuotaAccountLimit, len(policy.AccountLimits))
	for _, limit := range policy.AccountLimits {
		limits[limit.Dimension] = limit
	}
	if got, present := limits["output_submission"]; !present || got != (QuotaAccountLimit{Dimension: "output_submission", TaskLimitUnits: 64, ActorLimitUnits: 640}) {
		t.Fatalf("output-submission account limit = %+v, present=%v", got, present)
	}

	for _, definition := range template.Catalog.Stages {
		claims, present := resolved.ClaimsFor(definition.Key)
		if !present {
			t.Fatalf("policy omitted stage %q", definition.Key)
		}
		_, isAgentStage := standardAgentQuotaStages[definition.Key]
		if isAgentStage {
			submissionUnits := StandardAuthoringOutputSubmissionClaimUnits
			if definition.Key == workflowkit.StageKey(DockerfileBuildValidate) || definition.Key == workflowkit.StageKey(AuthoringHarness) {
				submissionUnits = StandardAuthoringWorkspaceSubmissionClaimUnits
			}
			if !hasQuotaClaim(claims, "agent_turn", int64(definition.RequiredTurns)) || !hasQuotaClaim(claims, "output_submission", submissionUnits) {
				t.Fatalf("agent stage %q claims = %+v", definition.Key, claims)
			}
			continue
		}
		if hasQuotaClaim(claims, "output_submission", StandardAuthoringOutputSubmissionClaimUnits) {
			t.Fatalf("non-agent stage %q unexpectedly has output-submission quota: %+v", definition.Key, claims)
		}
	}
}

func TestCodeEdgePhase1QuotaPolicyLeavesEvaluatorTrialsToChild(t *testing.T) {
	parent := CodeEdgePhase1WorkflowTemplate()
	if parent.QuotaPolicy.ID != CodeEdgePhase1QuotaPolicyID || parent.QuotaPolicy.Version != CodeEdgePhase1QuotaPolicyVersion {
		t.Fatalf("CodeEdge parent quota policy = %s@%s, want %s@%s", parent.QuotaPolicy.ID, parent.QuotaPolicy.Version, CodeEdgePhase1QuotaPolicyID, CodeEdgePhase1QuotaPolicyVersion)
	}
	if err := parent.QuotaPolicy.ValidateFor(parent.Catalog); err != nil {
		t.Fatalf("validate CodeEdge parent quota policy: %v", err)
	}
	for _, limit := range parent.QuotaPolicy.AccountLimits {
		if limit.Dimension == "trial" {
			t.Fatalf("parent CodeEdge quota policy retained evaluator trial account: %#v", parent.QuotaPolicy.AccountLimits)
		}
	}
	for _, stage := range parent.QuotaPolicy.Stages {
		for _, claim := range stage.Claims {
			if claim.Dimension == "trial" {
				t.Fatalf("parent CodeEdge stage %q retained evaluator trial claim: %#v", stage.StageKey, stage.Claims)
			}
		}
	}
	resolved, err := parent.Compile(explicitProfile(parent.Catalog))
	if err != nil {
		t.Fatalf("compile CodeEdge parent: %v", err)
	}
	handoff, present := resolved.Descriptor.Stage(workflowkit.StageKey(EvaluatorEvidenceHandoff))
	if !present || len(handoff.QuotaClaims) != 0 {
		t.Fatalf("CodeEdge evaluator evidence handoff claims = %#v, want durable-gate empty claims", handoff.QuotaClaims)
	}

	child := CodeEdgeEvaluatorChildWorkflowTemplate()
	resolvedChild, err := child.Compile(explicitProfile(child.Catalog))
	if err != nil {
		t.Fatalf("compile CodeEdge evaluator child: %v", err)
	}
	qwen, present := resolvedChild.QuotaPolicy.ClaimsFor(workflowkit.StageKey(HarborRunQwen))
	if !present || !hasQuotaClaim(qwen, "trial", 4) {
		t.Fatalf("CodeEdge evaluator child Qwen claims = %#v, want four logical trials", qwen)
	}
}

func TestQuotaPolicyMutationChangesFrozenManifestAndDescriptorAndRejectsDrift(t *testing.T) {
	template := StandardWorkflowTemplate()
	profile := explicitProfile(template.Catalog)
	baseline, err := template.Compile(profile)
	if err != nil {
		t.Fatal(err)
	}

	changedAccount := template.Clone()
	changedAccount.QuotaPolicy.AccountLimits[0].TaskLimitUnits++
	withChangedAccount, err := changedAccount.Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.QuotaPolicy.Fingerprint == withChangedAccount.QuotaPolicy.Fingerprint || baseline.ManifestFingerprint == withChangedAccount.ManifestFingerprint {
		t.Fatal("account bootstrap change did not alter frozen policy and manifest fingerprints")
	}

	changedClaim := template.Clone()
	for index := range changedClaim.QuotaPolicy.Stages {
		if changedClaim.QuotaPolicy.Stages[index].StageKey != workflowkit.StageKey(RepoAnalyze) {
			continue
		}
		for claim := range changedClaim.QuotaPolicy.Stages[index].Claims {
			if changedClaim.QuotaPolicy.Stages[index].Claims[claim].Dimension == "agent_turn" {
				changedClaim.QuotaPolicy.Stages[index].Claims[claim].Units++
			}
		}
	}
	withChangedClaim, err := changedClaim.Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.DefinitionFingerprint == withChangedClaim.DefinitionFingerprint {
		t.Fatal("stage claim change did not alter frozen descriptor fingerprint")
	}

	tampered := baseline.Descriptor.Clone()
	for index := range tampered.Stages {
		if tampered.Stages[index].Key == workflowkit.StageKey(RepoAnalyze) {
			tampered.Stages[index].QuotaClaims[0].Units++
			break
		}
	}
	if err := baseline.QuotaPolicy.ValidateForDescriptor(tampered); err == nil {
		t.Fatal("tampered descriptor claims passed frozen policy validation")
	}
}

func hasQuotaClaim(claims []workflowkit.QuotaClaim, dimension string, units int64) bool {
	for _, claim := range claims {
		if claim.Dimension == dimension && claim.Units == units {
			return true
		}
	}
	return false
}

func sameFrozenQuotaClaims(t *testing.T, left, right []workflowkit.QuotaClaim) bool {
	t.Helper()
	left, err := workflowkit.NormalizeQuotaClaims(left)
	if err != nil {
		t.Fatalf("normalize left quota claims: %v", err)
	}
	right, err = workflowkit.NormalizeQuotaClaims(right)
	if err != nil {
		t.Fatalf("normalize right quota claims: %v", err)
	}
	return sameQuotaClaims(left, right)
}
