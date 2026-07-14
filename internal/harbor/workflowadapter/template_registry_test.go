package workflowadapter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestClosedTemplateRegistryResolvesExactVersionsWithoutFallback(t *testing.T) {
	registry := DefaultTemplateRegistry()
	for _, reference := range []TemplateReference{StandardTemplateReference(), CodeEdgePhase1TemplateReference()} {
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
		{ID: "harbor.not-installed", Version: "1.0.0"},
		{ID: StandardWorkflowTemplateID},
	} {
		if _, err := registry.ResolveTemplate(reference); err == nil {
			t.Fatalf("unregistered reference %#v unexpectedly resolved", reference)
		}
	}
}

func TestExplicitTemplateBindingRejectsCrossTemplateAndTemplateLessDocuments(t *testing.T) {
	standardTemplate := StandardWorkflowTemplate()
	codeEdgeTemplate := CodeEdgePhase1WorkflowTemplate()
	standardProfile := explicitProfile(standardTemplate.Catalog)
	codeEdgeProfile := explicitProfile(codeEdgeTemplate.Catalog)

	if _, err := standardTemplate.Compile(codeEdgeProfile); err == nil || !strings.Contains(err.Error(), "template reference mismatch") {
		t.Fatalf("standard compile with CodeEdge profile = %v, want exact-template rejection", err)
	}
	if _, err := codeEdgeTemplate.Compile(standardProfile); err == nil || !strings.Contains(err.Error(), "template reference mismatch") {
		t.Fatalf("CodeEdge compile with Standard profile = %v, want exact-template rejection", err)
	}

	templateLessProfile := standardProfile.Clone()
	templateLessProfile.Template = TemplateReference{}
	if err := templateLessProfile.Validate(); err == nil || !strings.Contains(err.Error(), "template") {
		t.Fatalf("template-less profile validation = %v, want rejection", err)
	}

	standardSpec := testRunExecutionSpec(t)
	if err := standardSpec.ValidateFor(codeEdgeTemplate.Catalog); err == nil || !strings.Contains(err.Error(), "template reference mismatch") {
		t.Fatalf("Standard spec against CodeEdge catalog = %v, want cross-template rejection", err)
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
	codeEdgeProfile := explicitProfile(CodeEdgePhase1StageCatalog())
	standardProfileFingerprint, err := standardProfile.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	codeEdgeProfileFingerprint, err := codeEdgeProfile.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if standardProfileFingerprint == codeEdgeProfileFingerprint {
		t.Fatal("template-bound profiles unexpectedly share a fingerprint")
	}

	standardSpec := testRunExecutionSpec(t)
	codeEdgeSpec := testCodeEdgePhase1RunExecutionSpec(t)
	standardSpecFingerprint, err := standardSpec.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	codeEdgeSpecFingerprint, err := codeEdgeSpec.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if standardSpecFingerprint == codeEdgeSpecFingerprint {
		t.Fatal("template-bound specifications unexpectedly share a fingerprint")
	}

	resolved, err := CodeEdgePhase1WorkflowTemplate().Compile(codeEdgeProfile)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Template.Equal(CodeEdgePhase1TemplateReference()) || resolved.TemplateID != CodeEdgePhase1WorkflowTemplateID || resolved.TemplateVersion != CodeEdgePhase1WorkflowTemplateVersion {
		t.Fatalf("resolved CodeEdge template identity = %#v / %s@%s", resolved.Template, resolved.TemplateID, resolved.TemplateVersion)
	}
}

func TestCodeEdgePhase1TopologyKeepsPackageAfterEvaluationAndFinalCompliance(t *testing.T) {
	template := CodeEdgePhase1WorkflowTemplate()
	if err := template.Validate(); err != nil {
		t.Fatalf("validate CodeEdge Phase-1 template: %v", err)
	}
	if got, want := len(template.Catalog.Stages), len(CodeEdgePhase1StageOrder()); got != want {
		t.Fatalf("CodeEdge stage count = %d, want %d", got, want)
	}

	resolved, err := template.Compile(explicitProfile(template.Catalog))
	if err != nil {
		t.Fatalf("compile CodeEdge Phase-1 template: %v", err)
	}
	order, err := resolved.Descriptor.TopologicalStages()
	if err != nil {
		t.Fatal(err)
	}
	positions := make(map[workflowkit.StageKey]int, len(order))
	for index, key := range order {
		positions[key] = index
	}
	for _, relation := range [][2]workflowkit.StageKey{
		{workflowkit.StageKey(RepoPrepare), workflowkit.StageKey(DockerBuild)},
		{workflowkit.StageKey(DockerBuild), workflowkit.StageKey(InitialVerify)},
		{workflowkit.StageKey(InitialVerify), workflowkit.StageKey(OracleVerify)},
		{workflowkit.StageKey(OracleVerify), workflowkit.StageKey(TestsAnalysis)},
		{workflowkit.StageKey(TestsAnalysis), workflowkit.StageKey(SolutionReview)},
		{workflowkit.StageKey(FinalReview), workflowkit.StageKey(HarborRunQwen)},
		{workflowkit.StageKey(FinalReview), workflowkit.StageKey(HarborRunOpus)},
		{workflowkit.StageKey(HarborRunOpus), workflowkit.StageKey(ResultReview)},
		{workflowkit.StageKey(ResultReview), workflowkit.StageKey(Package)},
	} {
		if positions[relation[0]] >= positions[relation[1]] {
			t.Fatalf("CodeEdge topology puts %q at %d after %q at %d", relation[0], positions[relation[0]], relation[1], positions[relation[1]])
		}
	}
	packageStage, present := template.Catalog.Stage(workflowkit.StageKey(Package))
	if !present || !packageStage.Dispatch.IsOperatorOnly() || !sameStageKeySet(packageStage.Dependencies, []workflowkit.StageKey{workflowkit.StageKey(ResultReview)}) {
		t.Fatalf("package dependencies = %#v, want only final ResultReview", packageStage.Dependencies)
	}
	initialPlan, err := workflowkit.CompileDependencyExecutionPlan(resolved.Descriptor)
	if err != nil {
		t.Fatalf("compile CodeEdge Phase-1 initial execution plan: %v", err)
	}
	var evaluatorBatch []workflowkit.NodeID
	for _, batch := range initialPlan.Batches {
		containsQwen := false
		containsOpus := false
		for _, nodeID := range batch.NodeIDs {
			if nodeID == workflowkit.NodeID(Package) {
				t.Fatalf("CodeEdge initial plan scheduled operator-only package: %#v", initialPlan.Batches)
			}
			containsQwen = containsQwen || nodeID == workflowkit.NodeID(HarborRunQwen)
			containsOpus = containsOpus || nodeID == workflowkit.NodeID(HarborRunOpus)
		}
		if containsQwen || containsOpus {
			if !containsQwen || !containsOpus {
				t.Fatalf("CodeEdge evaluator dependency layer split Qwen and Opus: %#v", initialPlan.Batches)
			}
			evaluatorBatch = append([]workflowkit.NodeID(nil), batch.NodeIDs...)
		}
	}
	if len(evaluatorBatch) != 2 {
		t.Fatalf("CodeEdge evaluator dependency layer = %#v, want concurrent Qwen/Opus batch", evaluatorBatch)
	}
	qwen, _ := resolved.Descriptor.Stage(workflowkit.StageKey(HarborRunQwen))
	opus, _ := resolved.Descriptor.Stage(workflowkit.StageKey(HarborRunOpus))
	for _, evaluator := range []workflowkit.StageDescriptor{qwen, opus} {
		if !sameStageKeySet(evaluator.Dependencies, []workflowkit.StageKey{workflowkit.StageKey(FinalReview)}) {
			t.Fatalf("CodeEdge evaluator %q dependencies = %#v, want only FinalReview", evaluator.Key, evaluator.Dependencies)
		}
	}
	if !hasQuotaClaim(qwen.QuotaClaims, "trial", 4) || !hasQuotaClaim(opus.QuotaClaims, "trial", 4) {
		t.Fatalf("CodeEdge evaluator quota claims qwen=%+v opus=%+v, want exactly four logical trials each", qwen.QuotaClaims, opus.QuotaClaims)
	}
	for _, evaluator := range []workflowkit.StageDescriptor{qwen, opus} {
		if evaluator.Effect != workflowkit.EffectExternalSideEffect {
			t.Fatalf("CodeEdge evaluator %q effect = %q, want external side effect", evaluator.Key, evaluator.Effect)
		}
		if len(evaluator.Retry.Retryable) != 0 || evaluator.Reuse != workflowkit.ReuseNever {
			t.Fatalf("CodeEdge evaluator %q has generic retry/reuse policy %#v/%q; logical trials must reconcile below the stage", evaluator.Key, evaluator.Retry, evaluator.Reuse)
		}
	}

	broken := template.Catalog.Clone()
	for index := range broken.Stages {
		if broken.Stages[index].Key == workflowkit.StageKey(Package) {
			broken.Stages[index].Dependencies = []workflowkit.StageKey{workflowkit.StageKey(FinalReview)}
		}
	}
	if err := broken.Validate(); err == nil || !strings.Contains(err.Error(), "frozen template topology") {
		t.Fatalf("package-before-compliance topology mutation = %v, want rejection", err)
	}
}

func TestCodeEdgePhase1SubmissionReportUsesTheVersionedTypedContract(t *testing.T) {
	template := CodeEdgePhase1WorkflowTemplate()
	if template.Version != "2.1.1" || template.Catalog.Version != "2.1.1" {
		t.Fatalf("CodeEdge semantic schema change did not receive template/catalog versions: %s / %s", template.Version, template.Catalog.Version)
	}
	stages := make(map[workflowkit.StageKey]StageDefinition, len(template.Catalog.Stages))
	for _, stage := range template.Catalog.Stages {
		stages[stage.Key] = stage
	}
	assertSchema := func(stage workflowkit.StageKey, artifacts []workflowkit.ArtifactSpec) {
		t.Helper()
		found := false
		for _, artifact := range artifacts {
			if artifact.Name != "submission_lint_report" {
				continue
			}
			found = true
			if artifact.SchemaVersion != CodeEdgeSubmissionReportSchemaVersion {
				t.Fatalf("stage %s submission_lint_report schema = %q, want %q", stage, artifact.SchemaVersion, CodeEdgeSubmissionReportSchemaVersion)
			}
		}
		if !found {
			t.Fatalf("stage %s omitted submission_lint_report", stage)
		}
	}
	submission, submissionPresent := stages[workflowkit.StageKey(SubmissionLint)]
	resultReview, resultReviewPresent := stages[workflowkit.StageKey(ResultReview)]
	localPackage, packagePresent := stages[workflowkit.StageKey(Package)]
	if !submissionPresent || !resultReviewPresent || !packagePresent {
		t.Fatalf("CodeEdge schema contract stages = submission:%t review:%t package:%t", submissionPresent, resultReviewPresent, packagePresent)
	}
	assertSchema(submission.Key, submission.Outputs)
	assertSchema(resultReview.Key, resultReview.Inputs)
	assertSchema(localPackage.Key, localPackage.Inputs)
}

func testCodeEdgePhase1RunExecutionSpec(t *testing.T) RunExecutionSpec {
	t.Helper()
	const digest = "harbor.task.v2:sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	selection := RunSelectionReference{
		TaskID: "018f0a73-3b49-7000-8000-000000000011", RevisionID: "018f0a73-3b49-7000-8000-000000000012", RevisionDigest: workflowkit.SubjectDigest(digest),
	}
	policy := testCodeEdgeFinalCompliancePolicy()
	specification := RunExecutionSpec{
		Format: RunExecutionSpecFormat, Version: RunExecutionSpecVersion, Template: CodeEdgePhase1TemplateReference(), Selection: selection,
		CodeEdgeFinalCompliancePolicy: &policy,
		References: ExecutionReferenceSet{
			Artifacts: []ArtifactReference{{ID: "018f0a73-3b49-7000-8000-000000000013", ContentDigest: testFingerprint('d'), SchemaVersion: "harbor.artifact.v1"}},
			Checkouts: []CheckoutReference{{ID: "checkout-codeedge", RevisionID: selection.RevisionID, RevisionDigest: selection.RevisionDigest}},
			Runtimes:  []RuntimeReference{{ID: "runtime-codeedge", Kind: "controlled", Version: "1"}},
			Providers: []ProviderReference{
				{ID: "provider-codeedge-local", Kind: "native", Version: "1"},
				{ID: "provider-codeedge-review", Kind: "durable-review", Version: "1"},
				{ID: "provider-codeedge-evaluator", Kind: "evaluation", Version: "1"},
			},
		},
	}
	for _, definition := range CodeEdgePhase1StageCatalog().Stages {
		base := StageBindingBase{
			Type: bindingTypeForTest(definition.Key), StageKey: definition.Key,
			Plugin:     workflowkit.PluginBinding{ID: definition.Plugin.ID, Version: definition.Plugin.Version},
			CheckoutID: "checkout-codeedge", RuntimeID: "runtime-codeedge", ArtifactInputs: []ArtifactInputReference{}, SecretIDs: []string{},
			Operation: StageOperationBinding{ProviderID: "provider-codeedge-local", OperationID: string(definition.Key), Version: "1", Payload: LocalCommandOperationPayload{CommandID: "codeedge-stage", Arguments: []string{string(definition.Key)}}},
		}
		switch definition.Key {
		case workflowkit.StageKey(RepoPrepare):
			base.ArtifactInputs = []ArtifactInputReference{{Port: "task_snapshot", ArtifactID: "018f0a73-3b49-7000-8000-000000000013"}}
		case workflowkit.StageKey(SolutionReview), workflowkit.StageKey(FinalReview), workflowkit.StageKey(ResultReview):
			base.Operation.ProviderID = "provider-codeedge-review"
			base.Operation.Payload = DurableReviewOperationPayload{PolicyID: "codeedge-review.v1"}
		case workflowkit.StageKey(HarborRunQwen), workflowkit.StageKey(HarborRunOpus):
			base.Operation.ProviderID = "provider-codeedge-evaluator"
		}
		specification.Stages = append(specification.Stages, bindingForTest(t, base))
	}
	if err := specification.Validate(); err != nil {
		t.Fatalf("build CodeEdge execution-spec fixture: %v", err)
	}
	return specification
}
