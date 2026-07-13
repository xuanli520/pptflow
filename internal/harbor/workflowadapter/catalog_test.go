package workflowadapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardStageCatalogCoversAllNodesAndGroups(t *testing.T) {
	catalog := StandardStageCatalog()
	if err := catalog.Validate(); err != nil {
		t.Fatalf("validate standard catalog: %v", err)
	}
	if got, want := len(catalog.Stages), len(lifecycleNodeOrder()); got != want {
		t.Fatalf("catalog node count = %d, want %d", got, want)
	}
	if got, want := len(StandardStageGroups()), 11; got != want {
		t.Fatalf("standard group count = %d, want %d", got, want)
	}
	for _, node := range lifecycleNodeOrder() {
		if _, present := catalog.Stage(workflowkit.StageKey(node)); !present {
			t.Errorf("current Harbor node %q is absent from catalog", node)
		}
	}
	if _, present := catalog.Stage(workflowkit.StageKey(nodes.PublishTask)); present {
		t.Fatal("V2 catalog must not expose the legacy publish task stage")
	}
	for _, group := range StandardStageGroups() {
		found := false
		for _, stage := range catalog.Stages {
			if stage.Group == group {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("confirmed group %q has no catalog stage", group)
		}
	}
}

func TestCatalogRepresentsGatesAsDurableReviewStages(t *testing.T) {
	template := StandardWorkflowTemplate()
	profile := explicitProfile(template.Catalog)
	resolved, err := template.Compile(profile)
	if err != nil {
		t.Fatalf("compile template: %v", err)
	}

	expected := map[workflowkit.StageKey]ReviewKind{
		workflowkit.StageKey(nodes.TaskReview):     ReviewTaskDirection,
		workflowkit.StageKey(nodes.ContentReview):  ReviewContent,
		workflowkit.StageKey(nodes.SolutionReview): ReviewSolutionVerifier,
		workflowkit.StageKey(nodes.FinalReview):    ReviewFinalQuality,
		workflowkit.StageKey(nodes.ResultReview):   ReviewModelResult,
	}
	if got, want := len(resolved.ReviewStages), len(expected); got != want {
		t.Fatalf("review stage count = %d, want %d", got, want)
	}
	for key, kind := range expected {
		definition, present := template.Catalog.Stage(key)
		if !present {
			t.Fatalf("missing gate %q", key)
		}
		if !definition.IsGate() {
			t.Errorf("%q is not a gate", key)
		}
		if definition.Effect != workflowkit.EffectEvidenceOnly {
			t.Errorf("gate %q effect = %q, want evidence-only", key, definition.Effect)
		}
		if !definition.Capabilities.Has(workflowkit.CapabilityApprove) {
			t.Errorf("gate %q does not advertise approve capability", key)
		}
		if !definition.AllowsVerdict(workflowkit.VerdictPass) || !definition.AllowsVerdict(workflowkit.VerdictNeedsRepair) || !definition.AllowsVerdict(workflowkit.VerdictReject) {
			t.Errorf("gate %q does not support all review outcomes", key)
		}
		review, present := resolved.ReviewStage(key)
		if !present || review.ReviewKind != kind {
			t.Errorf("compiled review %q = %#v, want kind %q", key, review, kind)
		}
		descriptor, present := resolved.Descriptor.Stage(key)
		if !present || !descriptor.Capabilities.Has(workflowkit.CapabilityApprove) {
			t.Errorf("compiled gate %q lost approve capability", key)
		}
	}

	identity := workflowkit.AttemptIdentity{
		ID:      "gate-attempt-1",
		Kind:    workflowkit.AttemptStage,
		ScopeID: string(nodes.FinalReview),
		Ordinal: 1,
	}
	if _, err := workflowkit.NewOpenedAttemptRecord("gate-record-1", 1, identity, workflowkit.StatusQueued, time.Now().UTC()); err != nil {
		t.Fatalf("review gate cannot be represented as durable stage attempt: %v", err)
	}
}

func TestExplicitProfileIsRequiredAndPreservesTurnContract(t *testing.T) {
	template := StandardWorkflowTemplate()
	profile := explicitProfile(template.Catalog)
	if _, err := template.Compile(profile); err != nil {
		t.Fatalf("compile complete profile: %v", err)
	}

	missing := profile.Clone()
	missing.Stages = missing.Stages[:len(missing.Stages)-1]
	if _, err := template.Compile(missing); err == nil || !strings.Contains(err.Error(), "omits Harbor node") {
		t.Fatalf("compile missing profile error = %v, want missing-node failure", err)
	}

	tooFewTurns := profile.Clone()
	for index := range tooFewTurns.Stages {
		if tooFewTurns.Stages[index].StageKey != workflowkit.StageKey(nodes.GenerateTaskFiles) {
			continue
		}
		tooFewTurns.Stages[index].Budget = budgetForTurns(1)
	}
	if _, err := template.Compile(tooFewTurns); err == nil || !strings.Contains(err.Error(), "less than required") {
		t.Fatalf("compile insufficient turn budget error = %v, want required-turn failure", err)
	}

	unknown := profile.Clone()
	unknown.Stages = append(unknown.Stages, StageBudget{StageKey: "not-a-harbor-node", Budget: budgetForTurns(1)})
	if _, err := template.Compile(unknown); err == nil || !strings.Contains(err.Error(), "unknown Harbor node") {
		t.Fatalf("compile profile with unknown stage error = %v, want unknown-node failure", err)
	}

	wrongContinuationTTL := profile.Clone()
	wrongContinuationTTL.ContinuationPlanTTL = 23 * time.Hour
	if _, err := template.Compile(wrongContinuationTTL); err == nil || !strings.Contains(err.Error(), "continuation plan TTL") {
		t.Fatalf("compile profile with a non-24h continuation TTL error = %v, want TTL failure", err)
	}
}

func TestCatalogAndProfileFingerprintsAreCanonicalAndSensitive(t *testing.T) {
	template := StandardWorkflowTemplate()
	profile := explicitProfile(template.Catalog)
	catalogFingerprint, err := template.Catalog.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint catalog: %v", err)
	}
	profileFingerprint, err := profile.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint profile: %v", err)
	}

	reorderedCatalog := template.Catalog.Clone()
	reverseStages(reorderedCatalog.Stages)
	for index := range reorderedCatalog.Stages {
		reverseStageSets(&reorderedCatalog.Stages[index])
	}
	reorderedCatalogFingerprint, err := reorderedCatalog.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint reordered catalog: %v", err)
	}
	if catalogFingerprint != reorderedCatalogFingerprint {
		t.Fatalf("catalog fingerprint changed after declaration reorder: %q != %q", catalogFingerprint, reorderedCatalogFingerprint)
	}

	reorderedProfile := profile.Clone()
	reverseBudgets(reorderedProfile.Stages)
	reorderedProfileFingerprint, err := reorderedProfile.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint reordered profile: %v", err)
	}
	if profileFingerprint != reorderedProfileFingerprint {
		t.Fatalf("profile fingerprint changed after declaration reorder: %q != %q", profileFingerprint, reorderedProfileFingerprint)
	}

	changedCatalog := template.Catalog.Clone()
	changedCatalog.Stages[0].Plugin.Version = "changed"
	changedCatalogFingerprint, err := changedCatalog.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint changed catalog: %v", err)
	}
	if catalogFingerprint == changedCatalogFingerprint {
		t.Fatal("catalog fingerprint did not change after plugin policy change")
	}

	changedProfile := profile.Clone()
	changedProfile.Stages[0].Budget.TurnTimeout += time.Second
	changedProfile.Stages[0].Budget.AttemptTimeout += time.Second
	changedProfile.Stages[0].Budget.MaxElapsed += time.Second
	changedProfileFingerprint, err := changedProfile.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint changed profile: %v", err)
	}
	if profileFingerprint == changedProfileFingerprint {
		t.Fatal("profile fingerprint did not change after budget policy change")
	}
}

func TestCompileFreezesTemplateProfileAndDescriptorFingerprints(t *testing.T) {
	template := StandardWorkflowTemplate()
	profile := explicitProfile(template.Catalog)
	resolved, err := template.Compile(profile)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if err := resolved.Descriptor.Validate(); err != nil {
		t.Fatalf("compiled descriptor invalid: %v", err)
	}
	if got, want := len(resolved.Descriptor.Stages), len(lifecycleNodeOrder()); got != want {
		t.Fatalf("compiled stage count = %d, want %d", got, want)
	}
	if resolved.ContinuationPlanTTL != RequiredContinuationPlanTTL {
		t.Fatalf("resolved continuation plan TTL = %s, want %s", resolved.ContinuationPlanTTL, RequiredContinuationPlanTTL)
	}
	for name, fingerprint := range map[string]workflowkit.Fingerprint{
		"template":   resolved.TemplateFingerprint,
		"profile":    resolved.ExecutionProfileFingerprint,
		"definition": resolved.DefinitionFingerprint,
		"manifest":   resolved.ManifestFingerprint,
	} {
		if err := fingerprint.Validate(); err != nil {
			t.Errorf("%s fingerprint invalid: %v", name, err)
		}
	}
	order, err := resolved.Descriptor.TopologicalStages()
	if err != nil {
		t.Fatalf("topological order: %v", err)
	}
	if got, want := len(order), len(lifecycleNodeOrder()); got != want {
		t.Fatalf("topological order count = %d, want %d", got, want)
	}
	for _, definition := range template.Catalog.Stages {
		stage, present := resolved.Descriptor.Stage(definition.Key)
		if !present {
			t.Fatalf("compiled descriptor omitted stage %q", definition.Key)
		}
		want := workflowkit.PluginBinding{ID: definition.Plugin.ID, Version: definition.Plugin.Version}
		if stage.Plugin != want {
			t.Fatalf("compiled stage %q plugin = %#v, want %#v", definition.Key, stage.Plugin, want)
		}
	}
}

func TestCompiledDescriptorFingerprintAndJSONFreezePluginBinding(t *testing.T) {
	template := StandardWorkflowTemplate()
	profile := explicitProfile(template.Catalog)
	resolved, err := template.Compile(profile)
	if err != nil {
		t.Fatalf("compile baseline template: %v", err)
	}

	changed := template.Clone()
	changed.Catalog.Stages[0].Plugin.Version = "2.0.0"
	changedResolved, err := changed.Compile(profile)
	if err != nil {
		t.Fatalf("compile changed plugin template: %v", err)
	}
	if resolved.DefinitionFingerprint == changedResolved.DefinitionFingerprint {
		t.Fatal("definition fingerprint did not change after frozen plugin version changed")
	}

	raw, err := json.Marshal(resolved)
	if err != nil {
		t.Fatalf("marshal resolved workflow: %v", err)
	}
	var decoded ResolvedWorkflow
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal resolved workflow: %v", err)
	}
	for _, stage := range resolved.Descriptor.Stages {
		decodedStage, present := decoded.Descriptor.Stage(stage.Key)
		if !present {
			t.Fatalf("decoded descriptor omitted stage %q", stage.Key)
		}
		if decodedStage.Plugin != stage.Plugin {
			t.Fatalf("decoded stage %q plugin = %#v, want %#v", stage.Key, decodedStage.Plugin, stage.Plugin)
		}
	}
}

func TestRepairFirstPolicyAndResourceMapping(t *testing.T) {
	testCases := []struct {
		name    string
		class   FindingClass
		failure workflowkit.FailureClass
		status  workflowkit.ExecutionStatus
		verdict workflowkit.Verdict
	}{
		{name: "content", class: FindingContentFailure, status: workflowkit.StatusCompleted, verdict: workflowkit.VerdictNeedsRepair},
		{name: "security", class: FindingSecurityViolation, status: workflowkit.StatusCompleted, verdict: workflowkit.VerdictReject},
		{name: "policy", class: FindingPolicyViolation, status: workflowkit.StatusCompleted, verdict: workflowkit.VerdictReject},
		{name: "warning", class: FindingWarning, status: workflowkit.StatusCompleted, verdict: workflowkit.VerdictAdvisory},
		{name: "infrastructure", class: FindingInfrastructure, failure: workflowkit.FailureTimeout, status: workflowkit.StatusInfraFailed},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			outcome, err := RepairFirstOutcome(testCase.class, testCase.failure)
			if err != nil {
				t.Fatalf("classify outcome: %v", err)
			}
			if outcome.Status != testCase.status || outcome.Verdict != testCase.verdict {
				t.Fatalf("outcome = %#v, want status %q verdict %q", outcome, testCase.status, testCase.verdict)
			}
		})
	}
	if _, err := RepairFirstOutcome(FindingInfrastructure, workflowkit.FailureNone); err == nil {
		t.Fatal("infrastructure outcome without a failure class unexpectedly succeeded")
	}

	catalog := StandardStageCatalog()
	materialize, _ := catalog.Stage(workflowkit.StageKey(nodes.MaterializeTask))
	if materialize.Effect != workflowkit.EffectContentMutator || !containsResource(materialize.WriteSet, resourceTaskDigest) {
		t.Fatalf("materialize mapping = %#v, want content mutation and task digest write", materialize)
	}
	repair, _ := catalog.Stage(workflowkit.StageKey(nodes.TaskRepair))
	if repair.Effect != workflowkit.EffectContentMutator || !containsResource(repair.WriteSet, resourceTaskWildcard) {
		t.Fatalf("repair mapping = %#v, want task-wide content mutation", repair)
	}
	if _, present := catalog.Stage(workflowkit.StageKey(nodes.PublishTask)); present {
		t.Fatal("legacy publish stage is still exposed by the V2 catalog")
	}
	localPackage, present := catalog.Stage(workflowkit.StageKey(nodes.Package))
	if !present || localPackage.Effect != workflowkit.EffectExternalSideEffect || localPackage.Plugin.ID != "harborfactory.local_package" {
		t.Fatalf("local package delivery mapping = %#v, want the managed local package stage", localPackage)
	}
	if len(localPackage.Dependencies) != 1 || localPackage.Dependencies[0] != workflowkit.StageKey(nodes.SubmissionLint) {
		t.Fatalf("local package must depend directly on submission lint: %#v", localPackage.Dependencies)
	}
	quality, _ := catalog.Stage(workflowkit.StageKey(nodes.QualityCheck))
	for _, verdict := range []workflowkit.Verdict{workflowkit.VerdictPass, workflowkit.VerdictNeedsRepair, workflowkit.VerdictReject, workflowkit.VerdictAdvisory} {
		if !quality.AllowsVerdict(verdict) {
			t.Errorf("quality stage does not allow repair-first verdict %q", verdict)
		}
	}
	if !HarborResourceMatch(resourceTaskWildcard, resourceTaskInstruction) || !HarborResourceMatch(resourceTaskInstruction, resourceTaskWildcard) {
		t.Fatal("task wildcard does not invalidate individual task resources bidirectionally")
	}
	if HarborResourceMatch(resourceTaskWildcard, resourceEvidenceQuality) {
		t.Fatal("task wildcard unexpectedly matches evidence resource")
	}
}

func TestCatalogValidationRejectsBrokenGateAndCoverage(t *testing.T) {
	catalog := StandardStageCatalog()
	for index := range catalog.Stages {
		if catalog.Stages[index].Key == workflowkit.StageKey(nodes.FinalReview) {
			catalog.Stages[index].Capabilities = nil
			break
		}
	}
	if err := catalog.Validate(); err == nil || !strings.Contains(err.Error(), "approve capability") {
		t.Fatalf("broken gate validation error = %v, want approve-capability failure", err)
	}

	catalog = StandardStageCatalog()
	catalog.Stages = catalog.Stages[:len(catalog.Stages)-1]
	if err := catalog.Validate(); err == nil || !strings.Contains(err.Error(), "not cataloged") {
		t.Fatalf("incomplete catalog validation error = %v, want coverage failure", err)
	}
}

func TestWorkflowKitDoesNotContainHarborPolicyVocabulary(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve source path for architecture boundary test")
	}
	kernelDir := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "pkg", "workflowkit"))
	err := filepath.WalkDir(kernelDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(string(content)), "harbor") {
			t.Errorf("workflowkit source %s contains Harbor policy vocabulary", filepath.Base(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk workflowkit source: %v", err)
	}
}

func explicitProfile(catalog StageCatalog) ExecutionProfile {
	profile := ExecutionProfile{ID: "test-explicit", Version: "1.0.0", ContinuationPlanTTL: RequiredContinuationPlanTTL, ControlGracePeriod: 30 * time.Second, Stages: make([]StageBudget, 0, len(catalog.Stages))}
	for _, stage := range catalog.Stages {
		profile.Stages = append(profile.Stages, StageBudget{StageKey: stage.Key, Budget: budgetForTurns(stage.RequiredTurns)})
	}
	return profile
}

func budgetForTurns(turns int) workflowkit.ExecutionBudget {
	turnTimeout := time.Minute
	attemptTimeout := time.Duration(turns) * turnTimeout
	return workflowkit.ExecutionBudget{
		TurnTimeout:    turnTimeout,
		MaxTurns:       turns,
		AttemptTimeout: attemptTimeout,
		MaxAttempts:    1,
		MaxElapsed:     attemptTimeout,
	}
}

func reverseStages(stages []StageDefinition) {
	for left, right := 0, len(stages)-1; left < right; left, right = left+1, right-1 {
		stages[left], stages[right] = stages[right], stages[left]
	}
}

func reverseBudgets(stages []StageBudget) {
	for left, right := 0, len(stages)-1; left < right; left, right = left+1, right-1 {
		stages[left], stages[right] = stages[right], stages[left]
	}
}

func reverseStageSets(stage *StageDefinition) {
	reverseStageKeys(stage.Dependencies)
	reverseArtifactSpecs(stage.Inputs)
	reverseArtifactSpecs(stage.Outputs)
	reverseResources(stage.ReadSet)
	reverseResources(stage.WriteSet)
	reverseFailures(stage.Retry.Retryable)
	reverseVerdicts(stage.Verdicts.Allowed)
	reverseCapabilities(stage.Capabilities)
}

func reverseStageKeys(values []workflowkit.StageKey) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseArtifactSpecs(values []workflowkit.ArtifactSpec) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseResources(values []workflowkit.ResourceKey) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseFailures(values []workflowkit.FailureClass) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseVerdicts(values []workflowkit.Verdict) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseCapabilities(values workflowkit.CapabilitySet) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func containsResource(values []workflowkit.ResourceKey, target workflowkit.ResourceKey) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
