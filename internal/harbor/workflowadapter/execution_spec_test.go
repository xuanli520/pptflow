package workflowadapter

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestRunExecutionSpecCoversEveryCatalogStageWithConcreteBinding(t *testing.T) {
	spec := testRunExecutionSpec(t)
	if err := spec.Validate(); err != nil {
		t.Fatalf("validate execution spec: %v", err)
	}
	catalog := StandardStageCatalog()
	if got, want := len(spec.Stages), len(catalog.Stages); got != want {
		t.Fatalf("stage binding count = %d, want %d", got, want)
	}
	for _, definition := range catalog.Stages {
		binding, present := spec.StageBinding(definition.Key)
		if !present {
			t.Errorf("missing binding for catalog stage %q", definition.Key)
			continue
		}
		base, ok := stageBindingBaseOf(binding)
		if !ok {
			t.Errorf("binding for %q is not a supported concrete union member: %T", definition.Key, binding)
			continue
		}
		if base.Plugin.ID != definition.Plugin.ID || base.Plugin.Version != definition.Plugin.Version {
			t.Errorf("binding %q plugin = %#v, want %q@%q", definition.Key, base.Plugin, definition.Plugin.ID, definition.Plugin.Version)
		}
		if key, kind, ok := stageBindingIdentity(binding); !ok || key != definition.Key || kind != base.Type {
			t.Errorf("binding %q concrete identity = %q/%q/%t", definition.Key, key, kind, ok)
		}
	}
}

func TestRunExecutionSpecSupportsAuthoringSessionSubjectWithoutSyntheticTaskRevision(t *testing.T) {
	spec := testStandardAuthoringExecutionSpec(t)
	const sourceDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	spec.Selection = RunSelectionReference{
		Kind:                  RunSelectionAuthoringSession,
		AuthoringSourceID:     "018f0a73-3b49-7000-8000-000000000010",
		AuthoringSessionID:    "018f0a73-3b49-7000-8000-000000000011",
		AuthoringSourceDigest: workflowkit.SubjectDigest(sourceDigest),
	}
	for index := range spec.References.Checkouts {
		spec.References.Checkouts[index].RevisionID = spec.Selection.AuthoringSessionID
		spec.References.Checkouts[index].RevisionDigest = spec.Selection.AuthoringSourceDigest
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("validate authoring-session execution spec: %v", err)
	}
	binding, err := spec.Selection.SubjectBinding()
	if err != nil {
		t.Fatalf("project authoring selection to generic subject: %v", err)
	}
	if binding.SubjectID != spec.Selection.AuthoringSourceID || binding.RevisionID != spec.Selection.AuthoringSessionID || binding.Digest != spec.Selection.AuthoringSourceDigest {
		t.Fatalf("authoring subject binding = %+v", binding)
	}
	canonical, err := spec.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical authoring-session execution spec: %v", err)
	}
	decoded, err := ParseRunExecutionSpecJSON(canonical)
	if err != nil {
		t.Fatalf("parse canonical authoring-session execution spec: %v", err)
	}
	if decoded.Selection.Kind != RunSelectionAuthoringSession || decoded.Selection.TaskID != "" || decoded.Selection.RevisionID != "" {
		t.Fatalf("decoded authoring selection = %+v", decoded.Selection)
	}

	mixed := spec.Clone()
	mixed.Selection.TaskID = "018f0a73-3b49-7000-8000-000000000001"
	if err := mixed.Validate(); err == nil || !strings.Contains(err.Error(), "cannot contain task-revision") {
		t.Fatalf("mixed selection validation = %v, want closed-union failure", err)
	}
}

func TestRunExecutionSpecRejectsAuthoringSessionOnTaskBoundTemplate(t *testing.T) {
	spec := testRunExecutionSpec(t)
	spec.Selection = RunSelectionReference{
		Kind:                  RunSelectionAuthoringSession,
		AuthoringSourceID:     "018f0a73-3b49-7000-8000-000000000010",
		AuthoringSessionID:    "018f0a73-3b49-7000-8000-000000000011",
		AuthoringSourceDigest: workflowkit.SubjectDigest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
	}
	for index := range spec.References.Checkouts {
		spec.References.Checkouts[index].RevisionID = spec.Selection.AuthoringSessionID
		spec.References.Checkouts[index].RevisionDigest = spec.Selection.AuthoringSourceDigest
	}
	if err := spec.Validate(); err == nil || !strings.Contains(err.Error(), "only accepted by Standard authoring") {
		t.Fatalf("full Standard authoring-session selection = %v, want template-bound rejection", err)
	}
}

func TestRunExecutionSpecRejectsMissingBindingAndPluginDrift(t *testing.T) {
	missing := testRunExecutionSpec(t)
	missing.Stages = missing.Stages[:len(missing.Stages)-1]
	if err := missing.Validate(); err == nil || !strings.Contains(err.Error(), "stage bindings") {
		t.Fatalf("missing binding validation = %v, want binding coverage failure", err)
	}

	drifted := testRunExecutionSpec(t)
	base, ok := stageBindingBaseOf(drifted.Stages[0])
	if !ok {
		t.Fatal("fixture did not use a concrete binding")
	}
	base.Plugin.Version = "drifted"
	drifted.Stages[0] = replaceStageBindingBase(drifted.Stages[0], base)
	if err := drifted.Validate(); err == nil || !strings.Contains(err.Error(), "does not match catalog") {
		t.Fatalf("plugin drift validation = %v, want catalog binding mismatch", err)
	}
}

func TestRunExecutionSpecStrictJSONDecoder(t *testing.T) {
	spec := testRunExecutionSpec(t)
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	parsed, err := ParseRunExecutionSpecJSON(raw)
	if err != nil {
		t.Fatalf("parse valid spec: %v", err)
	}
	if _, ok := parsed.Stages[0].(RepoPrepareBinding); !ok {
		t.Fatalf("first parsed binding type = %T, want RepoPrepareBinding", parsed.Stages[0])
	}
	if got, want := len(parsed.Stages), len(spec.Stages); got != want {
		t.Fatalf("parsed stage count = %d, want %d", got, want)
	}

	unknownRoot := append([]byte(`{"unexpected":true,`), raw[1:]...)
	unknownBinding := []byte(strings.Replace(string(raw), `"type":"repo_prepare"`, `"type":"repo_prepare","unexpected":true`, 1))
	duplicateRoot := []byte(strings.Replace(string(raw), `"format":"harbor.run-execution-spec.v1"`, `"format":"harbor.run-execution-spec.v1","format":"harbor.run-execution-spec.v1"`, 1))
	unknownType := []byte(strings.Replace(string(raw), `"type":"repo_prepare"`, `"type":"not-a-stage"`, 1))
	legacyDeploymentContract := append([]byte(`{"codeedge_phase1_deployment_contract":{"id":"legacy","version":"1","fingerprint":"sha256:legacy"},`), raw[1:]...)
	trailing := append(append([]byte(nil), raw...), []byte(" null")...)
	for name, malformed := range map[string][]byte{
		"unknown root field":                unknownRoot,
		"unknown binding field":             unknownBinding,
		"duplicate root field":              duplicateRoot,
		"unknown discriminator":             unknownType,
		"removed deployment contract field": legacyDeploymentContract,
		"trailing value":                    trailing,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRunExecutionSpecJSON(malformed); err == nil {
				t.Fatalf("malformed JSON accepted: %s", malformed)
			}
		})
	}
}

func TestRunExecutionSpecScopesCodeEdgeFinalCompliancePolicyToCodeEdgeTemplate(t *testing.T) {
	standard := testRunExecutionSpec(t)
	policy := testCodeEdgeFinalCompliancePolicy()
	standard.CodeEdgeFinalCompliancePolicy = &policy
	if err := standard.Validate(); err == nil || !strings.Contains(err.Error(), "not accepted") {
		t.Fatalf("standard execution spec with CodeEdge policy = %v, want template-scoped rejection", err)
	}

	standardRaw, err := json.Marshal(testRunExecutionSpec(t))
	if err != nil {
		t.Fatalf("marshal standard execution spec: %v", err)
	}
	if strings.Contains(string(standardRaw), `"codeedge_final_compliance_policy"`) {
		t.Fatal("standard execution spec marshal emitted a CodeEdge-only policy field")
	}
	standardWithNullPolicy := append([]byte(`{"codeedge_final_compliance_policy":null,`), standardRaw[1:]...)
	if _, err := ParseRunExecutionSpecJSON(standardWithNullPolicy); err == nil {
		t.Fatal("standard execution spec parser accepted an explicit CodeEdge policy field")
	}

	codeEdge := testCodeEdgePhase1RunExecutionSpec(t)
	if codeEdge.CodeEdgeFinalCompliancePolicy == nil {
		t.Fatal("CodeEdge fixture has no final compliance policy")
	}
	codeEdge.CodeEdgeFinalCompliancePolicy = nil
	if err := codeEdge.Validate(); err == nil || !strings.Contains(err.Error(), "requires a final compliance policy") {
		t.Fatalf("CodeEdge execution spec without policy = %v, want required-policy rejection", err)
	}
}

func TestCodeEdgeFinalCompliancePolicyRoundTripStrictnessCanonicalizationAndClone(t *testing.T) {
	specification := testCodeEdgePhase1RunExecutionSpec(t)
	raw, err := json.Marshal(specification)
	if err != nil {
		t.Fatalf("marshal CodeEdge execution spec: %v", err)
	}
	if !strings.Contains(string(raw), `"codeedge_final_compliance_policy"`) {
		t.Fatal("CodeEdge execution spec marshal omitted final compliance policy")
	}
	parsed, err := ParseRunExecutionSpecJSON(raw)
	if err != nil {
		t.Fatalf("parse valid CodeEdge execution spec: %v", err)
	}
	if parsed.CodeEdgeFinalCompliancePolicy == nil {
		t.Fatal("parsed CodeEdge execution spec lost final compliance policy")
	}
	if got, want := parsed.CodeEdgeFinalCompliancePolicy.QwenPolicy.Evaluator.ModelName, specification.CodeEdgeFinalCompliancePolicy.QwenPolicy.Evaluator.ModelName; got != want {
		t.Fatalf("parsed Qwen model = %q, want %q", got, want)
	}
	canonical, err := specification.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonicalize original CodeEdge execution spec: %v", err)
	}
	parsedCanonical, err := parsed.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonicalize parsed CodeEdge execution spec: %v", err)
	}
	if string(parsedCanonical) != string(canonical) {
		t.Fatalf("CodeEdge execution spec canonical round trip changed:\n%s\n!=\n%s", parsedCanonical, canonical)
	}
	remarshaled, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("re-marshal parsed CodeEdge execution spec: %v", err)
	}
	reparsed, err := ParseRunExecutionSpecJSON(remarshaled)
	if err != nil {
		t.Fatalf("re-parse marshaled CodeEdge execution spec: %v", err)
	}
	originalPolicyFingerprint, err := specification.CodeEdgeFinalCompliancePolicy.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint original CodeEdge policy: %v", err)
	}
	reparsedPolicyFingerprint, err := reparsed.CodeEdgeFinalCompliancePolicy.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint re-parsed CodeEdge policy: %v", err)
	}
	if reparsedPolicyFingerprint != originalPolicyFingerprint {
		t.Fatalf("CodeEdge policy marshal/parse changed fingerprint: %q != %q", reparsedPolicyFingerprint, originalPolicyFingerprint)
	}

	baseline, err := specification.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint CodeEdge execution spec: %v", err)
	}
	reordered := specification.Clone()
	reverseStrings(reordered.CodeEdgeFinalCompliancePolicy.QwenPolicy.InfraExceptionTypes)
	reverseStrings(reordered.CodeEdgeFinalCompliancePolicy.OpusPolicy.InfraExceptionTypes)
	reorderedFingerprint, err := reordered.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint reordered CodeEdge policy: %v", err)
	}
	if reorderedFingerprint != baseline {
		t.Fatalf("policy exception order changed execution-spec fingerprint: %q != %q", reorderedFingerprint, baseline)
	}

	changed := specification.Clone()
	changed.CodeEdgeFinalCompliancePolicy.QwenPolicy.Evaluator.ModelName = "another-approved-model"
	changedFingerprint, err := changed.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint changed CodeEdge policy: %v", err)
	}
	if changedFingerprint == baseline {
		t.Fatal("changing the frozen CodeEdge policy did not change execution-spec fingerprint")
	}
	clone := specification.Clone()
	clone.CodeEdgeFinalCompliancePolicy.QwenPolicy.InfraExceptionTypes[0] = "ChangedException"
	if specification.CodeEdgeFinalCompliancePolicy.QwenPolicy.InfraExceptionTypes[0] == "ChangedException" {
		t.Fatal("CodeEdge policy clone mutated the original execution spec")
	}

	unknownPolicyField := []byte(strings.Replace(string(raw), `"id":"codeedge.phase1.final-compliance"`, `"id":"codeedge.phase1.final-compliance","unexpected":true`, 1))
	duplicatePolicyField := []byte(strings.Replace(string(raw), `"id":"codeedge.phase1.final-compliance"`, `"id":"codeedge.phase1.final-compliance","id":"codeedge.phase1.final-compliance"`, 1))
	for name, malformed := range map[string][]byte{
		"unknown policy field":   unknownPolicyField,
		"duplicate policy field": duplicatePolicyField,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRunExecutionSpecJSON(malformed); err == nil {
				t.Fatalf("malformed CodeEdge policy accepted: %s", malformed)
			}
		})
	}
}

func TestRunExecutionSpecFingerprintIsCanonicalAndFieldSensitive(t *testing.T) {
	spec := testRunExecutionSpec(t)
	baseline, err := spec.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint baseline: %v", err)
	}

	reordered := spec.Clone()
	reverseStageBindings(reordered.Stages)
	reverseArtifacts(reordered.References.Artifacts)
	reverseCheckouts(reordered.References.Checkouts)
	reverseRuntimes(reordered.References.Runtimes)
	reverseProviders(reordered.References.Providers)
	reverseSecrets(reordered.References.Secrets)
	for index, binding := range reordered.Stages {
		base, ok := stageBindingBaseOf(binding)
		if !ok {
			t.Fatalf("fixture binding %d is not concrete", index)
		}
		reverseArtifactInputs(base.ArtifactInputs)
		reverseStrings(base.SecretIDs)
		reordered.Stages[index] = replaceStageBindingBase(binding, base)
	}
	canonical, err := reordered.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint reordered: %v", err)
	}
	if canonical != baseline {
		t.Fatalf("fingerprint changed after entry reorder: %q != %q", canonical, baseline)
	}

	changed := spec.Clone()
	changed.References.Runtimes[0].Version = "2"
	changedFingerprint, err := changed.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint changed runtime: %v", err)
	}
	if changedFingerprint == baseline {
		t.Fatal("fingerprint did not change after frozen runtime version changed")
	}
}

func TestRunExecutionSpecCloneIsDeep(t *testing.T) {
	spec := testRunExecutionSpec(t)
	clone := spec.Clone()
	clone.References.Artifacts[0].SchemaVersion = "changed"
	base, ok := stageBindingBaseOf(clone.Stages[1])
	if !ok || len(base.ArtifactInputs) == 0 {
		t.Fatal("fixture did not expose mutable artifact inputs")
	}
	base.ArtifactInputs[0].Port = "changed"
	clone.Stages[1] = replaceStageBindingBase(clone.Stages[1], base)

	if spec.References.Artifacts[0].SchemaVersion == "changed" {
		t.Fatal("clone mutated original artifact references")
	}
	original, ok := stageBindingBaseOf(spec.Stages[1])
	if !ok || original.ArtifactInputs[0].Port == "changed" {
		t.Fatal("clone mutated original stage binding inputs")
	}
}

func TestRunExecutionSpecValidatesArtifactReferencesAndUsage(t *testing.T) {
	schemaDrift := testRunExecutionSpec(t)
	schemaDrift.References.Artifacts[0].SchemaVersion = "unexpected.schema"
	if err := schemaDrift.Validate(); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("artifact schema drift = %v, want schema mismatch", err)
	}

	unknownSecret := testRunExecutionSpec(t)
	base, ok := stageBindingBaseOf(unknownSecret.Stages[0])
	if !ok {
		t.Fatal("fixture did not use a concrete binding")
	}
	base.SecretIDs = []string{"not-configured"}
	unknownSecret.Stages[0] = replaceStageBindingBase(unknownSecret.Stages[0], base)
	if err := unknownSecret.Validate(); err == nil || !strings.Contains(err.Error(), "unknown secret") {
		t.Fatalf("unknown secret validation = %v, want unknown secret failure", err)
	}
}

func TestRunExecutionSpecBindsOnlyIntrinsicManagedArtifactInput(t *testing.T) {
	specification := testCodeEdgePhase1RunExecutionSpec(t)
	managed := ArtifactReference{
		ID: "018f0a73-3b49-7000-8000-0000000000ff", ContentDigest: testFingerprint('e'), SchemaVersion: "harbor.artifact.v1",
	}
	bound, err := specification.BindManagedArtifactInput("task_snapshot", managed)
	if err != nil {
		t.Fatalf("bind intrinsic task snapshot: %v", err)
	}
	if err := bound.Validate(); err != nil {
		t.Fatalf("validate bound execution spec: %v", err)
	}
	foundReference := false
	for _, reference := range bound.References.Artifacts {
		if reference.ID == managed.ID {
			foundReference = reference.ContentDigest == managed.ContentDigest && reference.SchemaVersion == managed.SchemaVersion
		}
	}
	if !foundReference {
		t.Fatalf("bound execution spec did not retain managed artifact reference: %+v", bound.References.Artifacts)
	}
	for _, stage := range CodeEdgePhase1StageCatalog().Stages {
		usesSnapshot := false
		for _, input := range stage.Inputs {
			usesSnapshot = usesSnapshot || input.Name == "task_snapshot"
		}
		if !usesSnapshot {
			continue
		}
		resolution, err := bound.ResolveStageOperation(stage.Key)
		if err != nil {
			t.Fatal(err)
		}
		matched := false
		for _, input := range resolution.ArtifactInputs {
			if input.Port == "task_snapshot" {
				matched = input.ArtifactID == managed.ID
			}
		}
		if !matched {
			t.Fatalf("stage %q does not bind managed task_snapshot", stage.Key)
		}
	}
	if _, err := testRunExecutionSpec(t).BindManagedArtifactInput("task_snapshot", managed); err == nil {
		t.Fatal("accepted a managed binding for standard task_snapshot, which has workflow producers")
	}
}

func TestRunExecutionSpecRequiresExactStageOperationProviderBinding(t *testing.T) {
	missingOperation := testRunExecutionSpec(t)
	base, ok := stageBindingBaseOf(missingOperation.Stages[0])
	if !ok {
		t.Fatal("fixture did not use a concrete binding")
	}
	base.Operation = StageOperationBinding{}
	missingOperation.Stages[0] = replaceStageBindingBase(missingOperation.Stages[0], base)
	if err := missingOperation.Validate(); err == nil || !strings.Contains(err.Error(), "operation") {
		t.Fatalf("missing operation validation = %v, want operation failure", err)
	}

	unknownProvider := testRunExecutionSpec(t)
	base, ok = stageBindingBaseOf(unknownProvider.Stages[0])
	if !ok {
		t.Fatal("fixture did not use a concrete binding")
	}
	base.Operation.ProviderID = "not-configured"
	unknownProvider.Stages[0] = replaceStageBindingBase(unknownProvider.Stages[0], base)
	if err := unknownProvider.Validate(); err == nil || !strings.Contains(err.Error(), "operation provider") {
		t.Fatalf("unknown provider validation = %v, want provider failure", err)
	}
}

func TestRunExecutionSpecResolvesAndPreflightsEveryFrozenOperation(t *testing.T) {
	spec := testRunExecutionSpec(t)
	resolved, err := spec.ResolveStageOperation("harbor_run_qwen")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.StageType != StageBindingHarborRunQwen || resolved.Provider.ID != "provider-evaluator" || resolved.Operation.OperationID != "harbor_run_qwen" || resolved.Plugin.ID != "harborfactory.harbor_run_qwen" {
		t.Fatalf("unexpected resolved operation: %+v", resolved)
	}
	if resolved.Runtime.ID != "runtime-evaluator" || len(resolved.Secrets) != 2 {
		t.Fatalf("resolved evaluator references = %+v", resolved)
	}

	seen := make(map[workflowkit.StageKey]StageOperationResolution)
	if err := spec.ValidateWithOperationResolver(stageOperationResolverFunc(func(resolution StageOperationResolution) error {
		seen[resolution.StageKey] = resolution
		if resolution.Operation.ProviderID != resolution.Provider.ID {
			return errors.New("operation/provider mismatch")
		}
		return nil
	})); err != nil {
		t.Fatalf("preflight valid operations: %v", err)
	}
	if got, want := len(seen), len(StandardStageCatalog().Stages); got != want {
		t.Fatalf("preflight operation count = %d, want %d", got, want)
	}

	rejected := errors.New("provider operation not installed")
	if err := spec.ValidateWithOperationResolver(stageOperationResolverFunc(func(StageOperationResolution) error { return rejected })); !errors.Is(err, rejected) {
		t.Fatalf("rejected provider operation error = %v, want %v", err, rejected)
	}
}

func testRunExecutionSpec(t *testing.T) RunExecutionSpec {
	t.Helper()
	const digest = "harbor.task.v2:sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	selection := RunSelectionReference{TaskID: "018f0a73-3b49-7000-8000-000000000001", RevisionID: "018f0a73-3b49-7000-8000-000000000002", RevisionDigest: workflowkit.SubjectDigest(digest)}
	spec := RunExecutionSpec{
		Format: RunExecutionSpecFormat, Version: RunExecutionSpecVersion, Template: StandardTemplateReference(), Selection: selection,
		References: ExecutionReferenceSet{
			Artifacts: []ArtifactReference{
				{ID: "018f0a73-3b49-7000-8000-000000000003", ContentDigest: testFingerprint('a'), SchemaVersion: "harbor.artifact.v1"},
				{ID: "018f0a73-3b49-7000-8000-000000000004", ContentDigest: testFingerprint('b'), SchemaVersion: "harbor.artifact.v1"},
				{ID: "018f0a73-3b49-7000-8000-000000000005", ContentDigest: testFingerprint('c'), SchemaVersion: "harbor.artifact.v1"},
			},
			Checkouts: []CheckoutReference{
				{ID: "checkout-main", RevisionID: selection.RevisionID, RevisionDigest: selection.RevisionDigest},
				{ID: "checkout-package", RevisionID: selection.RevisionID, RevisionDigest: selection.RevisionDigest},
			},
			Runtimes: []RuntimeReference{
				{ID: "runtime-local", Kind: "local", Version: "1"},
				{ID: "runtime-evaluator", Kind: "container", Version: "1"},
			},
			Providers: []ProviderReference{
				{ID: "provider-local", Kind: "native", Version: "1"},
				{ID: "provider-evaluator", Kind: "evaluation", Version: "1"},
				{ID: "provider-review", Kind: "durable-review", Version: "1"},
			},
			Secrets: []SecretReference{
				{ID: "secret-repository", Provider: "local-keyring", Version: "1"},
				{ID: "secret-evaluator", Provider: "local-keyring", Version: "1"},
			},
		},
	}
	for _, definition := range StandardStageCatalog().Stages {
		base := StageBindingBase{
			Type: bindingTypeForTest(definition.Key), StageKey: definition.Key,
			Plugin:     workflowkit.PluginBinding{ID: definition.Plugin.ID, Version: definition.Plugin.Version},
			CheckoutID: "checkout-main", RuntimeID: "runtime-local", ArtifactInputs: []ArtifactInputReference{}, SecretIDs: []string{},
			Operation: StageOperationBinding{
				ProviderID: "provider-local", OperationID: string(definition.Key), Version: "1",
				Payload: LocalCommandOperationPayload{CommandID: "harbor-stage", Arguments: []string{string(definition.Key)}},
			},
		}
		switch definition.Key {
		case "task_review", "content_review", "solution_review", "final_review", "result_review":
			base.Operation.ProviderID = "provider-review"
			base.Operation.Payload = DurableReviewOperationPayload{PolicyID: "harbor-review.v1"}
		case "repo_prepare":
			base.SecretIDs = []string{"secret-repository"}
		case "repo_analyze":
			base.ArtifactInputs = []ArtifactInputReference{{Port: "repo_prepared", ArtifactID: "018f0a73-3b49-7000-8000-000000000003"}}
		case "task_design":
			base.ArtifactInputs = []ArtifactInputReference{{Port: "repo_prepared", ArtifactID: "018f0a73-3b49-7000-8000-000000000003"}, {Port: "repo_analysis", ArtifactID: "018f0a73-3b49-7000-8000-000000000004"}}
		case "generate_task_files":
			base.ArtifactInputs = []ArtifactInputReference{{Port: "repo_prepared", ArtifactID: "018f0a73-3b49-7000-8000-000000000003"}, {Port: "repo_analysis", ArtifactID: "018f0a73-3b49-7000-8000-000000000004"}, {Port: "task_proposal", ArtifactID: "018f0a73-3b49-7000-8000-000000000005"}}
		case "harbor_run_qwen":
			base.RuntimeID = "runtime-evaluator"
			base.Operation.ProviderID = "provider-evaluator"
			base.Operation.Payload = ContainerCommandOperationPayload{
				ImageDigest: "registry.example/harbor/evaluator@sha256:" + strings.Repeat("f", 64),
				Command:     []string{"harbor-evaluator", string(definition.Key)},
			}
			base.SecretIDs = []string{"secret-evaluator", "secret-repository"}
		case "harbor_run_opus":
			base.RuntimeID = "runtime-evaluator"
			base.Operation.ProviderID = "provider-evaluator"
			base.Operation.Payload = ContainerCommandOperationPayload{
				ImageDigest: "registry.example/harbor/evaluator@sha256:" + strings.Repeat("f", 64),
				Command:     []string{"harbor-evaluator", string(definition.Key)},
			}
		case "package":
			base.CheckoutID = "checkout-package"
		}
		spec.Stages = append(spec.Stages, bindingForTest(t, base))
	}
	return spec
}

func testStandardAuthoringExecutionSpec(t *testing.T) RunExecutionSpec {
	t.Helper()
	spec := testRunExecutionSpec(t)
	spec.Template = StandardAuthoringTemplateReference()
	spec.Selection = RunSelectionReference{
		Kind:                  RunSelectionAuthoringSession,
		AuthoringSourceID:     "018f0a73-3b49-7000-8000-000000000010",
		AuthoringSessionID:    "018f0a73-3b49-7000-8000-000000000011",
		AuthoringSourceDigest: workflowkit.SubjectDigest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
	}
	for index := range spec.References.Checkouts {
		spec.References.Checkouts[index].RevisionID = spec.Selection.AuthoringSessionID
		spec.References.Checkouts[index].RevisionDigest = spec.Selection.AuthoringSourceDigest
	}
	allowed := make(map[workflowkit.StageKey]struct{}, len(StandardAuthoringStageOrder()))
	for _, key := range StandardAuthoringStageOrder() {
		allowed[key] = struct{}{}
	}
	stages := make([]StageExecutionBinding, 0, len(allowed))
	for _, binding := range spec.Stages {
		base, ok := stageBindingBaseOf(binding)
		if ok {
			if _, present := allowed[base.StageKey]; present {
				stages = append(stages, binding)
			}
		}
	}
	spec.Stages = stages
	spec.References.Checkouts = []CheckoutReference{spec.References.Checkouts[0]}
	spec.References.Runtimes = []RuntimeReference{spec.References.Runtimes[0]}
	spec.References.Providers = []ProviderReference{spec.References.Providers[0], spec.References.Providers[2]}
	spec.References.Secrets = []SecretReference{spec.References.Secrets[0]}
	return spec
}

func testCodeEdgeFinalCompliancePolicy() codeedge.FinalCompliancePolicy {
	maximumPassingTrials := 1
	qwen := codeedge.EvaluationPolicy{
		ID:                   "codeedge.qwen.pass-at-four",
		Version:              "1",
		HarborEvidenceFormat: codeedge.HarborRunBundleV018Format,
		Evaluator: codeedge.EvaluatorIdentity{
			ProfileID: "codeedge-qwen-profile", ProfileVersion: "1",
			AgentName: "codeedge-agent", AgentVersion: "1",
			ModelName: "qwen-approved-model", ModelProvider: "controlled-provider",
		},
		LogicalTrialCount:        4,
		PassRewardKey:            "reward",
		PassRewardAtLeast:        1,
		MaxPassingTrials:         &maximumPassingTrials,
		MinimumAverageTurns:      20,
		ScreenshotMediaType:      "image/png",
		FailureClassifierID:      "codeedge-infra-classifier",
		FailureClassifierVersion: "1",
		InfraExceptionTypes:      []string{"DockerBuildError", "NetworkError"},
	}
	opus := qwen.Clone()
	opus.ID = "codeedge.opus.reference"
	opus.Evaluator.ProfileID = "codeedge-opus-profile"
	opus.Evaluator.ModelName = "opus-reference-model"
	opus.MaxPassingTrials = nil
	return codeedge.FinalCompliancePolicy{
		ID:                            "codeedge.phase1.final-compliance",
		Version:                       "1",
		QwenPolicy:                    qwen,
		OpusPolicy:                    opus,
		SubmissionCheckerID:           "codeedge.submission-check",
		SubmissionCheckerVersion:      "1",
		SubmissionReportSchemaVersion: CodeEdgeSubmissionReportSchemaVersion,
	}
}

func bindingForTest(t *testing.T, base StageBindingBase) StageExecutionBinding {
	t.Helper()
	switch base.Type {
	case StageBindingRepoPrepare:
		return RepoPrepareBinding{StageBindingBase: base}
	case StageBindingRepoAnalyze:
		return RepoAnalyzeBinding{StageBindingBase: base}
	case StageBindingTaskDesign:
		return TaskDesignBinding{StageBindingBase: base}
	case StageBindingTaskReview:
		return TaskReviewBinding{StageBindingBase: base}
	case StageBindingGenerateTaskFiles:
		return GenerateTaskFilesBinding{StageBindingBase: base}
	case StageBindingInstructionGen:
		return InstructionGenBinding{StageBindingBase: base}
	case StageBindingTaskTOMLGen:
		return TaskTOMLGenBinding{StageBindingBase: base}
	case StageBindingDockerfileGen:
		return DockerfileGenBinding{StageBindingBase: base}
	case StageBindingContentReview:
		return ContentReviewBinding{StageBindingBase: base}
	case StageBindingSolveGen:
		return SolveGenBinding{StageBindingBase: base}
	case StageBindingTestGen:
		return TestGenBinding{StageBindingBase: base}
	case StageBindingTestsAnalysis:
		return TestsAnalysisBinding{StageBindingBase: base}
	case StageBindingSolutionReview:
		return SolutionReviewBinding{StageBindingBase: base}
	case StageBindingMaterializeTask:
		return MaterializeTaskBinding{StageBindingBase: base}
	case StageBindingTaskRepair:
		return TaskRepairBinding{StageBindingBase: base}
	case StageBindingRuntimeSelfCheck:
		return RuntimeSelfCheckBinding{StageBindingBase: base}
	case StageBindingHarborVerify:
		return HarborVerifyBinding{StageBindingBase: base}
	case StageBindingDockerBuild:
		return DockerBuildBinding{StageBindingBase: base}
	case StageBindingInitialVerify:
		return InitialVerifyBinding{StageBindingBase: base}
	case StageBindingOracleVerify:
		return OracleVerifyBinding{StageBindingBase: base}
	case StageBindingCodeEdgeLint:
		return CodeEdgeLintBinding{StageBindingBase: base}
	case StageBindingQualityCheck:
		return QualityCheckBinding{StageBindingBase: base}
	case StageBindingSimilarityCheck:
		return SimilarityCheckBinding{StageBindingBase: base}
	case StageBindingFinalReview:
		return FinalReviewBinding{StageBindingBase: base}
	case StageBindingHarborRunQwen:
		return HarborRunQwenBinding{StageBindingBase: base}
	case StageBindingHarborRunOpus:
		return HarborRunOpusBinding{StageBindingBase: base}
	case StageBindingEvaluatorEvidenceHandoff:
		return EvaluatorEvidenceHandoffBinding{StageBindingBase: base}
	case StageBindingResultReview:
		return ResultReviewBinding{StageBindingBase: base}
	case StageBindingSubmissionLint:
		return SubmissionLintBinding{StageBindingBase: base}
	case StageBindingPackage:
		return PackageBinding{StageBindingBase: base}
	default:
		t.Fatalf("unknown fixture binding type %q", base.Type)
		return nil
	}
}

func bindingTypeForTest(key workflowkit.StageKey) StageBindingType {
	switch key {
	case "repo_prepare":
		return StageBindingRepoPrepare
	case "repo_analyze":
		return StageBindingRepoAnalyze
	case "task_design":
		return StageBindingTaskDesign
	case "task_review":
		return StageBindingTaskReview
	case "generate_task_files":
		return StageBindingGenerateTaskFiles
	case "instruction_generate":
		return StageBindingInstructionGen
	case "task_toml_generate":
		return StageBindingTaskTOMLGen
	case "dockerfile_generate":
		return StageBindingDockerfileGen
	case "content_review":
		return StageBindingContentReview
	case "solve_generate":
		return StageBindingSolveGen
	case "test_generate":
		return StageBindingTestGen
	case "tests_analysis":
		return StageBindingTestsAnalysis
	case "solution_review":
		return StageBindingSolutionReview
	case "materialize_task":
		return StageBindingMaterializeTask
	case "task_repair":
		return StageBindingTaskRepair
	case "runtime_self_check":
		return StageBindingRuntimeSelfCheck
	case "harbor_verify":
		return StageBindingHarborVerify
	case "docker_build":
		return StageBindingDockerBuild
	case "initial_verify":
		return StageBindingInitialVerify
	case "oracle_verify":
		return StageBindingOracleVerify
	case "codeedge_lint":
		return StageBindingCodeEdgeLint
	case "quality_check":
		return StageBindingQualityCheck
	case "similarity_check":
		return StageBindingSimilarityCheck
	case "final_review":
		return StageBindingFinalReview
	case "harbor_run_qwen":
		return StageBindingHarborRunQwen
	case "harbor_run_opus":
		return StageBindingHarborRunOpus
	case "evaluator_evidence_handoff":
		return StageBindingEvaluatorEvidenceHandoff
	case "result_review":
		return StageBindingResultReview
	case "submission_lint":
		return StageBindingSubmissionLint
	case "package":
		return StageBindingPackage
	default:
		return ""
	}
}

func testFingerprint(character byte) workflowkit.Fingerprint {
	return workflowkit.Fingerprint("sha256:" + strings.Repeat(string(character), 64))
}

func reverseStageBindings(values []StageExecutionBinding) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseArtifacts(values []ArtifactReference) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseCheckouts(values []CheckoutReference) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseRuntimes(values []RuntimeReference) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseProviders(values []ProviderReference) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseSecrets(values []SecretReference) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseArtifactInputs(values []ArtifactInputReference) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseStrings(values []string) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

type stageOperationResolverFunc func(StageOperationResolution) error

func (function stageOperationResolverFunc) ValidateStageOperation(resolution StageOperationResolution) error {
	return function(resolution)
}
