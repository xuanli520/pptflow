package workflowadapter

import (
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestCodeEdgeEvaluatorChildTemplateFreezesSerialEvidenceContract(t *testing.T) {
	template := CodeEdgeEvaluatorChildWorkflowTemplate()
	if err := template.Validate(); err != nil {
		t.Fatalf("validate CodeEdge evaluator child template: %v", err)
	}
	if !template.Reference().Equal(CodeEdgeEvaluatorChildTemplateReference()) {
		t.Fatalf("child template reference = %#v, want %#v", template.Reference(), CodeEdgeEvaluatorChildTemplateReference())
	}
	if _, present := template.Catalog.Stage(workflowkit.StageKey(Package)); present {
		t.Fatal("CodeEdge evaluator child unexpectedly contains a local package stage")
	}
	if template.Catalog.Template.Equal(CodeEdgePhase1TemplateReference()) {
		t.Fatal("CodeEdge evaluator child aliases the complete CodeEdge Phase-1 template")
	}
	if IsCodeEdgeEvaluatorWorkflowTemplate(CodeEdgePhase1TemplateReference()) || !IsCodeEdgeEvaluatorWorkflowTemplate(CodeEdgeEvaluatorChildTemplateReference()) || IsCodeEdgeEvaluatorWorkflowTemplate(StandardTemplateReference()) {
		t.Fatal("CodeEdge evaluator template classification is not closed to the evaluator child")
	}

	resolved, err := template.Compile(explicitProfile(template.Catalog))
	if err != nil {
		t.Fatalf("compile CodeEdge evaluator child: %v", err)
	}
	if len(resolved.ReviewStages) != 0 {
		t.Fatalf("evaluator child review stages = %#v, want no package/compliance gate authority", resolved.ReviewStages)
	}
	order, err := resolved.Descriptor.TopologicalStages()
	if err != nil {
		t.Fatal(err)
	}
	if !sameStageKeySet(order, CodeEdgeEvaluatorChildStageOrder()) || len(order) != 2 || order[0] != workflowkit.StageKey(HarborRunQwen) || order[1] != workflowkit.StageKey(HarborRunOpus) {
		t.Fatalf("evaluator child topological order = %#v, want Qwen then Opus", order)
	}
	plan, err := workflowkit.CompileDependencyExecutionPlan(resolved.Descriptor)
	if err != nil {
		t.Fatalf("compile evaluator child execution plan: %v", err)
	}
	if len(plan.Batches) != 2 || len(plan.Batches[0].NodeIDs) != 1 || len(plan.Batches[1].NodeIDs) != 1 ||
		plan.Batches[0].NodeIDs[0] != workflowkit.NodeID(HarborRunQwen) || plan.Batches[1].NodeIDs[0] != workflowkit.NodeID(HarborRunOpus) {
		t.Fatalf("evaluator child dependency plan = %#v, want Qwen then Opus serial batches", plan.Batches)
	}

	qwen, qwenPresent := template.Catalog.Stage(workflowkit.StageKey(HarborRunQwen))
	opus, opusPresent := template.Catalog.Stage(workflowkit.StageKey(HarborRunOpus))
	if !qwenPresent || !opusPresent {
		t.Fatalf("evaluator child stages present qwen=%t opus=%t", qwenPresent, opusPresent)
	}
	if len(qwen.Dependencies) != 0 || !sameStageKeySet(opus.Dependencies, []workflowkit.StageKey{workflowkit.StageKey(HarborRunQwen)}) {
		t.Fatalf("evaluator child dependencies qwen=%#v opus=%#v, want root Qwen then Opus", qwen.Dependencies, opus.Dependencies)
	}
	assertCodeEdgeEvaluatorChildStage(t, qwen, CodeEdgeEvaluatorQwenBundleArtifact, CodeEdgeEvaluatorQwenScreenshotArtifact)
	assertCodeEdgeEvaluatorChildStage(t, opus, CodeEdgeEvaluatorOpusBundleArtifact, CodeEdgeEvaluatorOpusScreenshotArtifact)

	compiledQwen, _ := resolved.Descriptor.Stage(workflowkit.StageKey(HarborRunQwen))
	compiledOpus, _ := resolved.Descriptor.Stage(workflowkit.StageKey(HarborRunOpus))
	for _, stage := range []workflowkit.StageDescriptor{compiledQwen, compiledOpus} {
		if !hasQuotaClaim(stage.QuotaClaims, "stage_attempt", 1) || !hasQuotaClaim(stage.QuotaClaims, "trial", 4) {
			t.Fatalf("evaluator child stage %q quota claims = %#v, want one stage attempt and four logical trials", stage.Key, stage.QuotaClaims)
		}
		if len(stage.Retry.Retryable) != 0 || stage.Reuse != workflowkit.ReuseNever {
			t.Fatalf("evaluator child stage %q generic retry/reuse = %#v/%q, want none/never", stage.Key, stage.Retry, stage.Reuse)
		}
		if stage.Effect != workflowkit.EffectExternalSideEffect {
			t.Fatalf("evaluator child stage %q effect = %q, want external side effect", stage.Key, stage.Effect)
		}
	}
}

func TestCodeEdgeEvaluatorChildCatalogRejectsEvidenceAndRetryDrift(t *testing.T) {
	for name, mutate := range map[string]func(*StageCatalog){
		"screenshot schema": func(catalog *StageCatalog) {
			for index := range catalog.Stages {
				if catalog.Stages[index].Key != workflowkit.StageKey(HarborRunQwen) {
					continue
				}
				catalog.Stages[index].Outputs[1].SchemaVersion = "harbor.artifact.v1"
			}
		},
		"generic retry": func(catalog *StageCatalog) {
			for index := range catalog.Stages {
				if catalog.Stages[index].Key == workflowkit.StageKey(HarborRunOpus) {
					catalog.Stages[index].Retry.Retryable = []workflowkit.FailureClass{workflowkit.FailureTransient}
				}
			}
		},
		"reusable output": func(catalog *StageCatalog) {
			for index := range catalog.Stages {
				if catalog.Stages[index].Key == workflowkit.StageKey(HarborRunOpus) {
					catalog.Stages[index].Reuse = workflowkit.ReuseWhenInputsMatch
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			catalog := CodeEdgeEvaluatorChildStageCatalog()
			mutate(&catalog)
			if err := catalog.Validate(); err == nil || !strings.Contains(err.Error(), "does not match frozen descriptor") {
				t.Fatalf("mutated %s catalog validation = %v, want frozen descriptor rejection", name, err)
			}
		})
	}

	catalog := CodeEdgeEvaluatorChildStageCatalog()
	for index := range catalog.Stages {
		if catalog.Stages[index].Key == workflowkit.StageKey(HarborRunOpus) {
			catalog.Stages[index].Dependencies = nil
		}
	}
	if err := catalog.Validate(); err == nil || !strings.Contains(err.Error(), "frozen template topology") {
		t.Fatalf("parallel evaluator catalog validation = %v, want frozen topology rejection", err)
	}
}

func TestCodeEdgeEvaluatorChildSpecBindsOneFrozenSnapshotToBothModels(t *testing.T) {
	specification := testCodeEdgeEvaluatorChildRunExecutionSpec(t)
	if err := specification.Validate(); err != nil {
		t.Fatalf("validate evaluator child execution specification: %v", err)
	}
	canonical, err := specification.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonicalize evaluator child execution specification: %v", err)
	}
	parsed, err := ParseRunExecutionSpecJSON(canonical)
	if err != nil {
		t.Fatalf("parse evaluator child execution specification: %v", err)
	}
	if parsed.Template != CodeEdgeEvaluatorChildTemplateReference() {
		t.Fatalf("parsed evaluator child template = %#v, want %#v", parsed.Template, CodeEdgeEvaluatorChildTemplateReference())
	}

	for _, key := range []workflowkit.StageKey{workflowkit.StageKey(HarborRunQwen), workflowkit.StageKey(HarborRunOpus)} {
		resolution, err := specification.ResolveStageOperation(key)
		if err != nil {
			t.Fatalf("resolve child stage %q: %v", key, err)
		}
		if resolution.StageType != bindingTypeForTest(key) || len(resolution.ArtifactInputs) != 1 || resolution.ArtifactInputs[0].Port != CodeEdgeEvaluatorTaskSnapshotArtifact || resolution.ArtifactInputs[0].ArtifactID != "018f0a73-3b49-7000-8000-000000000042" {
			t.Fatalf("child evaluator resolution %q = %#v, want one frozen task_snapshot input", key, resolution)
		}
	}

	missingSnapshot := specification.Clone()
	replaceCodeEdgeEvaluatorChildBinding(t, &missingSnapshot, workflowkit.StageKey(HarborRunOpus), func(base *StageBindingBase) {
		base.ArtifactInputs = []ArtifactInputReference{}
	})
	if err := missingSnapshot.Validate(); err == nil || !strings.Contains(err.Error(), "requires a managed task_snapshot") {
		t.Fatalf("child spec without Opus task_snapshot = %v, want rejection", err)
	}

	differentSnapshot := specification.Clone()
	differentSnapshot.References.Artifacts = append(differentSnapshot.References.Artifacts, ArtifactReference{
		ID: "018f0a73-3b49-7000-8000-000000000043", ContentDigest: workflowkit.Fingerprint("sha256:" + strings.Repeat("e", 64)), SchemaVersion: CodeEdgeEvaluatorTaskSnapshotSchemaVersion,
	})
	replaceCodeEdgeEvaluatorChildBinding(t, &differentSnapshot, workflowkit.StageKey(HarborRunOpus), func(base *StageBindingBase) {
		base.ArtifactInputs[0].ArtifactID = "018f0a73-3b49-7000-8000-000000000043"
	})
	if err := differentSnapshot.Validate(); err == nil || !strings.Contains(err.Error(), "must bind the same task_snapshot") {
		t.Fatalf("child spec with model-specific snapshot = %v, want rejection", err)
	}

	withParentCompliance := specification.Clone()
	withParentCompliance.CodeEdgeFinalCompliancePolicy = &codeedge.FinalCompliancePolicy{}
	if err := withParentCompliance.Validate(); err == nil || !strings.Contains(err.Error(), "not accepted") {
		t.Fatalf("child spec with parent final compliance policy = %v, want rejection", err)
	}
}

func TestCodeEdgeEvaluatorChildBindManagedTaskSnapshotAndRejectsGenericRetryBudget(t *testing.T) {
	specification := testCodeEdgeEvaluatorChildRunExecutionSpec(t)
	specification.References.Artifacts = nil
	for index, binding := range specification.Stages {
		base, ok := stageBindingBaseOf(binding)
		if !ok {
			t.Fatalf("extract child binding %T", binding)
		}
		base.ArtifactInputs = []ArtifactInputReference{}
		specification.Stages[index] = replaceStageBindingBase(binding, base)
	}
	bound, err := specification.BindManagedArtifactInput(CodeEdgeEvaluatorTaskSnapshotArtifact, ArtifactReference{
		ID: "018f0a73-3b49-7000-8000-000000000044", ContentDigest: workflowkit.Fingerprint("sha256:" + strings.Repeat("f", 64)), SchemaVersion: CodeEdgeEvaluatorTaskSnapshotSchemaVersion,
	})
	if err != nil {
		t.Fatalf("bind evaluator child managed task_snapshot: %v", err)
	}
	if err := bound.Validate(); err != nil {
		t.Fatalf("validate bound evaluator child spec: %v", err)
	}
	for _, key := range []workflowkit.StageKey{workflowkit.StageKey(HarborRunQwen), workflowkit.StageKey(HarborRunOpus)} {
		binding, present := bound.StageBinding(key)
		if !present {
			t.Fatalf("bound evaluator child omitted %q", key)
		}
		base, _ := stageBindingBaseOf(binding)
		if len(base.ArtifactInputs) != 1 || base.ArtifactInputs[0].ArtifactID != "018f0a73-3b49-7000-8000-000000000044" {
			t.Fatalf("bound evaluator child %q inputs = %#v, want the managed snapshot", key, base.ArtifactInputs)
		}
	}

	profile := explicitProfile(CodeEdgeEvaluatorChildStageCatalog())
	for index := range profile.Stages {
		if profile.Stages[index].StageKey == workflowkit.StageKey(HarborRunOpus) {
			profile.Stages[index].Budget.MaxAttempts = 2
			profile.Stages[index].Budget.Backoff.RetryDelays = []time.Duration{0}
			profile.Stages[index].Budget.MaxElapsed = 2 * profile.Stages[index].Budget.AttemptTimeout
		}
	}
	if _, err := CodeEdgeEvaluatorChildWorkflowTemplate().Compile(profile); err == nil || !strings.Contains(err.Error(), "requires max_attempts=1") {
		t.Fatalf("evaluator child profile with generic retry budget = %v, want rejection", err)
	}
}

func assertCodeEdgeEvaluatorChildStage(t *testing.T, stage StageDefinition, bundleName, screenshotName string) {
	t.Helper()
	if stage.Group != StageEvaluation || stage.Plugin.Version != pluginDescriptorVersion || stage.Dispatch != workflowkit.StageDispatchAutomatic {
		t.Fatalf("evaluator child stage %q descriptor = %#v", stage.Key, stage)
	}
	if len(stage.Inputs) != 1 || stage.Inputs[0] != (workflowkit.ArtifactSpec{Name: CodeEdgeEvaluatorTaskSnapshotArtifact, SchemaVersion: CodeEdgeEvaluatorTaskSnapshotSchemaVersion, Required: true}) {
		t.Fatalf("evaluator child stage %q inputs = %#v, want one frozen task_snapshot", stage.Key, stage.Inputs)
	}
	if len(stage.Outputs) != 2 || stage.Outputs[0] != (workflowkit.ArtifactSpec{Name: bundleName, SchemaVersion: codeedge.HarborRunBundleV018Format, Required: true}) || stage.Outputs[1] != (workflowkit.ArtifactSpec{Name: screenshotName, SchemaVersion: CodeEdgeEvaluatorScreenshotSchemaVersion, Required: true}) {
		t.Fatalf("evaluator child stage %q outputs = %#v, want Harbor bundle plus canonical PNG", stage.Key, stage.Outputs)
	}
	if len(stage.Retry.Retryable) != 0 || stage.Reuse != workflowkit.ReuseNever || stage.Effect != workflowkit.EffectExternalSideEffect {
		t.Fatalf("evaluator child stage %q retry/reuse/effect = %#v/%q/%q", stage.Key, stage.Retry, stage.Reuse, stage.Effect)
	}
}

func testCodeEdgeEvaluatorChildRunExecutionSpec(t *testing.T) RunExecutionSpec {
	t.Helper()
	selection := RunSelectionReference{
		TaskID: "018f0a73-3b49-7000-8000-000000000040", RevisionID: "018f0a73-3b49-7000-8000-000000000041",
		RevisionDigest: workflowkit.SubjectDigest("harbor.task.v2:sha256:" + strings.Repeat("c", 64)),
	}
	taskSnapshot := ArtifactReference{
		ID: "018f0a73-3b49-7000-8000-000000000042", ContentDigest: workflowkit.Fingerprint("sha256:" + strings.Repeat("d", 64)), SchemaVersion: CodeEdgeEvaluatorTaskSnapshotSchemaVersion,
	}
	qwen := StageBindingBase{
		Type: StageBindingHarborRunQwen, StageKey: workflowkit.StageKey(HarborRunQwen),
		Plugin:         workflowkit.PluginBinding{ID: "harborfactory.harbor_run_qwen", Version: pluginDescriptorVersion},
		ArtifactInputs: []ArtifactInputReference{{Port: CodeEdgeEvaluatorTaskSnapshotArtifact, ArtifactID: taskSnapshot.ID}},
		CheckoutID:     "checkout-codeedge-evaluator", RuntimeID: "runtime-codeedge-evaluator", SecretIDs: []string{},
		Operation: StageOperationBinding{ProviderID: "provider-codeedge-evaluator", OperationID: "codeedge.qwen.pass-at-four", Version: "1", Payload: LocalCommandOperationPayload{CommandID: "codeedge-qwen-pass4", Arguments: []string{}}},
	}
	opus := StageBindingBase{
		Type: StageBindingHarborRunOpus, StageKey: workflowkit.StageKey(HarborRunOpus),
		Plugin:         workflowkit.PluginBinding{ID: "harborfactory.harbor_run_opus", Version: pluginDescriptorVersion},
		ArtifactInputs: []ArtifactInputReference{{Port: CodeEdgeEvaluatorTaskSnapshotArtifact, ArtifactID: taskSnapshot.ID}},
		CheckoutID:     "checkout-codeedge-evaluator", RuntimeID: "runtime-codeedge-evaluator", SecretIDs: []string{},
		Operation: StageOperationBinding{ProviderID: "provider-codeedge-evaluator", OperationID: "codeedge.opus.pass-at-four", Version: "1", Payload: LocalCommandOperationPayload{CommandID: "codeedge-opus-pass4", Arguments: []string{}}},
	}
	specification := RunExecutionSpec{
		Format: RunExecutionSpecFormat, Version: RunExecutionSpecVersion, Template: CodeEdgeEvaluatorChildTemplateReference(), Selection: selection,
		References: ExecutionReferenceSet{
			Artifacts: []ArtifactReference{taskSnapshot},
			Checkouts: []CheckoutReference{{ID: "checkout-codeedge-evaluator", RevisionID: selection.RevisionID, RevisionDigest: selection.RevisionDigest}},
			Runtimes:  []RuntimeReference{{ID: "runtime-codeedge-evaluator", Kind: "controlled", Version: "1"}},
			Providers: []ProviderReference{{ID: "provider-codeedge-evaluator", Kind: "evaluation", Version: "1"}},
		},
		Stages: []StageExecutionBinding{
			UniversalStageBinding{StageBindingBase: qwen},
			UniversalStageBinding{StageBindingBase: opus},
		},
	}
	if err := specification.Validate(); err != nil {
		t.Fatalf("build CodeEdge evaluator child execution spec: %v", err)
	}
	return specification
}

func replaceCodeEdgeEvaluatorChildBinding(t *testing.T, specification *RunExecutionSpec, key workflowkit.StageKey, mutate func(*StageBindingBase)) {
	t.Helper()
	for index, binding := range specification.Stages {
		base, ok := stageBindingBaseOf(binding)
		if !ok || base.StageKey != key {
			continue
		}
		mutate(&base)
		specification.Stages[index] = replaceStageBindingBase(binding, base)
		return
	}
	t.Fatalf("CodeEdge evaluator child binding %q is missing", key)
}
