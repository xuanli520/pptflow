package stageprovider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	deploymentCatalogTestTaskID     = "018f0a73-3b49-7000-8000-000000000061"
	deploymentCatalogTestRevisionID = "018f0a73-3b49-7000-8000-000000000062"
	deploymentCatalogTestDigest     = "harbor.task.v2:sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

func TestDeploymentOperationCatalogResolverAcceptsOnlyExactFrozenContract(t *testing.T) {
	resolution := deploymentCatalogTestResolution(t, workflowadapter.RepoPrepare)
	catalog := deploymentCatalogForResolutions(t, resolution)
	resolver, err := NewDeploymentOperationCatalogResolver(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolver.ValidateStageOperation(resolution); err != nil {
		t.Fatalf("validate exact frozen resolution: %v", err)
	}
	registration, err := resolver.ResolveStageOperation(resolution)
	if err != nil {
		t.Fatal(err)
	}
	if registration.Stage.Key != resolution.StageKey || registration.Runtime != resolution.Runtime || registration.Checkout.ID != resolution.Checkout.ID {
		t.Fatalf("resolved static registration = %+v", registration)
	}

	fingerprint, err := catalog.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	identity := resolver.CatalogIdentity()
	if identity.CatalogID != catalog.CatalogID || identity.CatalogVersion != catalog.CatalogVersion || identity.Fingerprint != fingerprint {
		t.Fatalf("catalog identity = %+v, want %s/%s/%s", identity, catalog.CatalogID, catalog.CatalogVersion, fingerprint)
	}

	// Constructor and accessor copies must prevent a caller from turning an
	// installed production resolver into a dynamically registered one.
	catalog.Operations[0].Operation.Payload = workflowadapter.LocalCommandOperationPayload{CommandID: "harbor-stage", Arguments: []string{"changed"}}
	if err := resolver.ValidateStageOperation(resolution); err != nil {
		t.Fatalf("mutating constructor input changed installed catalog: %v", err)
	}
	returned := resolver.Catalog()
	returned.Operations[0].Runtime.Version = "changed"
	if err := resolver.ValidateStageOperation(resolution); err != nil {
		t.Fatalf("mutating Catalog result changed installed catalog: %v", err)
	}
}

func TestDeploymentOperationCatalogBindsExactClosedTemplateWithoutFallback(t *testing.T) {
	codeEdgeSpec := testsupport.CompleteCodeEdgePhase1RunExecutionSpec(deploymentCatalogTestTaskID, deploymentCatalogTestRevisionID, deploymentCatalogTestDigest)
	codeEdgeCatalog := deploymentCatalogForExecutionSpec(t, codeEdgeSpec)
	resolver, err := NewDeploymentOperationCatalogResolver(codeEdgeCatalog)
	if err != nil {
		t.Fatalf("construct CodeEdge catalog resolver: %v", err)
	}
	if got, want := resolver.Template(), workflowadapter.CodeEdgePhase1TemplateReference(); !got.Equal(want) {
		t.Fatalf("catalog template = %+v, want %+v", got, want)
	}
	if err := resolver.ValidateExecutionSpec(codeEdgeSpec); err != nil {
		t.Fatalf("validate exact CodeEdge frozen execution spec: %v", err)
	}
	if _, err := resolver.ResolveExecutionSpecStageOperation(codeEdgeSpec, workflowadapter.CodeEdgeLint); err != nil {
		t.Fatalf("resolve CodeEdge-only template stage: %v", err)
	}

	standardSpec := testsupport.CompleteRunExecutionSpec(deploymentCatalogTestTaskID, deploymentCatalogTestRevisionID, deploymentCatalogTestDigest)
	if err := resolver.ValidateExecutionSpec(standardSpec); err == nil || !errors.Is(err, ErrDeploymentOperationCatalogDrift) {
		t.Fatalf("Standard spec against CodeEdge catalog = %v, want template drift", err)
	}
	if _, err := resolver.ResolveExecutionSpecStageOperation(standardSpec, workflowadapter.RepoPrepare); err == nil || !errors.Is(err, ErrDeploymentOperationCatalogDrift) {
		t.Fatalf("Standard stage resolution against CodeEdge catalog = %v, want template drift", err)
	}

	missingTemplate := codeEdgeCatalog.Clone()
	missingTemplate.Template = workflowadapter.TemplateReference{}
	if err := missingTemplate.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalog) {
		t.Fatalf("template-less catalog validation = %v, want invalid catalog", err)
	}
	wrongTemplate := codeEdgeCatalog.Clone()
	wrongTemplate.Template = workflowadapter.StandardTemplateReference()
	if err := wrongTemplate.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalog) {
		t.Fatalf("CodeEdge registrations under Standard template = %v, want invalid catalog", err)
	}
}

func TestDeploymentOperationCatalogFingerprintIsCanonicalAndFieldSensitive(t *testing.T) {
	first := deploymentCatalogTestResolution(t, workflowadapter.RepoPrepare)
	second := deploymentCatalogTestResolution(t, workflowadapter.RepoAnalyze)
	catalog := deploymentCatalogForResolutions(t, first, second)
	baselineJSON, err := catalog.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := catalog.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}

	reordered := catalog.Clone()
	reordered.Operations[0], reordered.Operations[1] = reordered.Operations[1], reordered.Operations[0]
	canonicalJSON, err := reordered.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := reordered.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if string(canonicalJSON) != string(baselineJSON) || canonical != baseline {
		t.Fatalf("reordered catalog changed canonical identity: json=%s fingerprint=%s, want %s", canonicalJSON, canonical, baseline)
	}

	changed := catalog.Clone()
	changed.Operations[0].Runtime.Version = "2"
	changedFingerprint, err := changed.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if changedFingerprint == baseline {
		t.Fatal("runtime version drift did not change catalog fingerprint")
	}
}

func TestDeploymentOperationCatalogStrictJSONDecoder(t *testing.T) {
	resolution := deploymentCatalogTestResolution(t, workflowadapter.RepoPrepare)
	catalog := deploymentCatalogForResolutions(t, resolution)
	raw, err := catalog.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseDeploymentOperationCatalogJSON(raw)
	if err != nil {
		t.Fatalf("parse canonical catalog: %v", err)
	}
	if got, want := len(parsed.Operations), 1; got != want {
		t.Fatalf("parsed operation count = %d, want %d", got, want)
	}
	var unmarshaled DeploymentOperationCatalog
	if err := json.Unmarshal(raw, &unmarshaled); err != nil {
		t.Fatalf("direct unmarshal must remain strict and valid: %v", err)
	}

	unknownRoot := []byte(strings.Replace(string(raw), `"catalog_version":"test-v1"`, `"catalog_version":"test-v1","unexpected":true`, 1))
	unknownStage := []byte(strings.Replace(string(raw), `"type":"repo_prepare"`, `"type":"repo_prepare","unexpected":true`, 1))
	unknownPayload := []byte(strings.Replace(string(raw), `"command_id":"harbor-stage"`, `"command_id":"harbor-stage","unexpected":true`, 1))
	duplicateKey := []byte(strings.Replace(string(raw), `"catalog_id":"codeedge-phase1-test"`, `"catalog_id":"codeedge-phase1-test","catalog_id":"codeedge-phase1-test"`, 1))
	trailing := append(append([]byte(nil), raw...), []byte(" null")...)
	for name, malformed := range map[string][]byte{
		"unknown root field":    unknownRoot,
		"unknown stage field":   unknownStage,
		"unknown payload field": unknownPayload,
		"duplicate key":         duplicateKey,
		"trailing value":        trailing,
		"null operations":       []byte(`{"format":"harbor.deployment-operation-catalog.v1","version":"1","catalog_id":"codeedge-phase1-test","catalog_version":"test-v1","operations":null}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseDeploymentOperationCatalogJSON(malformed); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalog) {
				t.Fatalf("malformed catalog error = %v, want invalid catalog", err)
			}
		})
	}
}

func TestDeploymentOperationCatalogRejectsDuplicatesUnversionedAndCatalogDrift(t *testing.T) {
	resolution := deploymentCatalogTestResolution(t, workflowadapter.RepoPrepare)
	base := deploymentCatalogForResolutions(t, resolution)
	cases := []struct {
		name   string
		mutate func(*DeploymentOperationCatalog)
	}{
		{
			name: "duplicate operation coordinate",
			mutate: func(catalog *DeploymentOperationCatalog) {
				catalog.Operations = append(catalog.Operations, catalog.Operations[0].Clone())
			},
		},
		{
			name: "unversioned provider",
			mutate: func(catalog *DeploymentOperationCatalog) {
				catalog.Operations[0].Provider.Version = ""
			},
		},
		{
			name: "unversioned runtime",
			mutate: func(catalog *DeploymentOperationCatalog) {
				catalog.Operations[0].Runtime.Version = ""
			},
		},
		{
			name: "unversioned operation",
			mutate: func(catalog *DeploymentOperationCatalog) {
				catalog.Operations[0].Operation.Version = ""
			},
		},
		{
			name: "unversioned secret",
			mutate: func(catalog *DeploymentOperationCatalog) {
				catalog.Operations[0].Secrets[0].Version = ""
			},
		},
		{
			name: "missing checkout purpose",
			mutate: func(catalog *DeploymentOperationCatalog) {
				catalog.Operations[0].Checkout.Purpose = ""
			},
		},
		{
			name: "unknown stage",
			mutate: func(catalog *DeploymentOperationCatalog) {
				catalog.Operations[0].Stage.Key = "not-a-harbor-stage"
			},
		},
		{
			name: "stage plugin drift",
			mutate: func(catalog *DeploymentOperationCatalog) {
				catalog.Operations[0].Stage.Plugin.Version = "drifted"
			},
		},
		{
			name: "implicit secret array",
			mutate: func(catalog *DeploymentOperationCatalog) {
				catalog.Operations[0].Secrets = nil
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			catalog := base.Clone()
			test.mutate(&catalog)
			if err := catalog.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalog) {
				t.Fatalf("catalog validation error = %v, want invalid catalog", err)
			}
		})
	}
}

func TestDeploymentOperationCatalogResolverRejectsEveryFrozenResolutionDrift(t *testing.T) {
	resolution := deploymentCatalogTestResolution(t, workflowadapter.RepoPrepare)
	resolver, err := NewDeploymentOperationCatalogResolver(deploymentCatalogForResolutions(t, resolution))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		mutate    func(*workflowadapter.StageOperationResolution)
		wantError error
	}{
		{
			name: "unknown provider",
			mutate: func(value *workflowadapter.StageOperationResolution) {
				value.Provider.ID = "not-installed"
				value.Operation.ProviderID = "not-installed"
			},
			wantError: ErrProviderUnavailable,
		},
		{
			name: "provider version drift",
			mutate: func(value *workflowadapter.StageOperationResolution) {
				value.Provider.Version = "2"
			},
			wantError: ErrProviderVersionMismatch,
		},
		{
			name: "provider kind drift",
			mutate: func(value *workflowadapter.StageOperationResolution) {
				value.Provider.Kind = "different"
			},
			wantError: ErrProviderVersionMismatch,
		},
		{
			name: "unknown operation version",
			mutate: func(value *workflowadapter.StageOperationResolution) {
				value.Operation.Version = "2"
			},
			wantError: ErrStageOperationUnavailable,
		},
		{
			name: "payload drift",
			mutate: func(value *workflowadapter.StageOperationResolution) {
				value.Operation.Payload = workflowadapter.LocalCommandOperationPayload{CommandID: "harbor-stage", Arguments: []string{"changed"}}
			},
			wantError: ErrFrozenOperationPayloadMismatch,
		},
		{
			name: "stage plugin drift",
			mutate: func(value *workflowadapter.StageOperationResolution) {
				value.Plugin.Version = "drifted"
			},
			wantError: ErrDeploymentOperationCatalogDrift,
		},
		{
			name: "stage type drift",
			mutate: func(value *workflowadapter.StageOperationResolution) {
				value.StageType = "different"
			},
			wantError: ErrDeploymentOperationCatalogDrift,
		},
		{
			name: "runtime drift",
			mutate: func(value *workflowadapter.StageOperationResolution) {
				value.Runtime.Version = "2"
			},
			wantError: ErrDeploymentOperationCatalogDrift,
		},
		{
			name: "checkout drift",
			mutate: func(value *workflowadapter.StageOperationResolution) {
				value.Checkout.ID = "checkout-different"
			},
			wantError: ErrDeploymentOperationCatalogDrift,
		},
		{
			name: "secret version drift",
			mutate: func(value *workflowadapter.StageOperationResolution) {
				value.Secrets[0].Version = "2"
			},
			wantError: ErrDeploymentOperationCatalogDrift,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			candidate := resolution.Clone()
			test.mutate(&candidate)
			if _, err := resolver.ResolveStageOperation(candidate); err == nil || !errors.Is(err, test.wantError) {
				t.Fatalf("resolve drift error = %v, want %v", err, test.wantError)
			}
		})
	}
	if got := len(resolver.Catalog().Operations); got != 1 {
		t.Fatalf("rejected resolutions dynamically changed catalog to %d operations", got)
	}
}

func TestDeploymentOperationCatalogAllowsExplicitDenyAllDeployment(t *testing.T) {
	catalog := DeploymentOperationCatalog{
		Format: DeploymentOperationCatalogFormat, Version: DeploymentOperationCatalogVersion,
		CatalogID: "codeedge-phase1-unconfigured", CatalogVersion: "1", Template: workflowadapter.StandardTemplateReference(), Operations: []DeploymentOperationRegistration{},
	}
	resolver, err := NewDeploymentOperationCatalogResolver(catalog)
	if err != nil {
		t.Fatalf("explicit empty deny-all catalog: %v", err)
	}
	resolution := deploymentCatalogTestResolution(t, workflowadapter.RepoPrepare)
	if err := resolver.ValidateStageOperation(resolution); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("deny-all resolver error = %v, want provider unavailable", err)
	}
}

func TestDeploymentOperationCatalogFileLoaderAndFrozenReceipt(t *testing.T) {
	resolution := deploymentCatalogTestResolution(t, workflowadapter.RepoPrepare)
	catalog := deploymentCatalogForResolutions(t, resolution)
	source, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "operation-catalog.v1.json")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := LoadDeploymentOperationCatalogFile(path)
	if err != nil {
		t.Fatalf("load catalog file: %v", err)
	}
	wantCanonical, err := catalog.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	gotCanonical := resolver.CanonicalCatalogJSON()
	if string(gotCanonical) != string(wantCanonical) {
		t.Fatalf("loaded canonical catalog = %s, want %s", gotCanonical, wantCanonical)
	}
	gotCanonical[0] = 'x'
	if string(resolver.CanonicalCatalogJSON()) != string(wantCanonical) {
		t.Fatal("mutating canonical catalog result changed resolver state")
	}

	receiptBytes, err := resolver.CanonicalReceiptJSON()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := ParseDeploymentOperationCatalogReceiptJSON(receiptBytes)
	if err != nil {
		t.Fatalf("parse catalog receipt: %v", err)
	}
	if err := resolver.VerifyReceipt(receipt); err != nil {
		t.Fatalf("verify own receipt: %v", err)
	}
	if err := resolver.VerifyCatalogIdentity(resolver.CatalogIdentity()); err != nil {
		t.Fatalf("verify own catalog identity: %v", err)
	}
	var directReceipt DeploymentOperationCatalogReceipt
	if err := json.Unmarshal(receiptBytes, &directReceipt); err != nil {
		t.Fatalf("direct receipt unmarshal: %v", err)
	}

	drifted := receipt
	drifted.CatalogVersion = "other"
	if err := resolver.VerifyReceipt(drifted); !errors.Is(err, ErrDeploymentOperationCatalogDrift) {
		t.Fatalf("drifted receipt error = %v, want catalog drift", err)
	}
	malformed := []byte(strings.Replace(string(receiptBytes), `"catalog_version":"test-v1"`, `"catalog_version":"test-v1","unexpected":true`, 1))
	if _, err := ParseDeploymentOperationCatalogReceiptJSON(malformed); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalog) {
		t.Fatalf("malformed receipt error = %v, want invalid catalog", err)
	}
}

func TestCatalogBoundWorkflowkitResolverRequiresInstalledHandlerAfterCatalogAdmission(t *testing.T) {
	specification := testsupport.CompleteRunExecutionSpec(deploymentCatalogTestTaskID, deploymentCatalogTestRevisionID, deploymentCatalogTestDigest)
	resolution, err := specification.ResolveStageOperation(workflowadapter.RepoPrepare)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := NewDeploymentOperationCatalogResolver(deploymentCatalogForResolutions(t, resolution))
	if err != nil {
		t.Fatal(err)
	}
	emptyHandlers, err := NewControlledWorkflowkitProviderRegistry([]WorkflowkitProviderRegistration{})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewCatalogBoundWorkflowkitProviderOperationResolver(catalog, emptyHandlers)
	if err != nil {
		t.Fatal(err)
	}
	// RunExecutionSpec uses this exact interface during StartRun preflight. The
	// catalog admits repo_prepare, but no local implementation is installed.
	if err := specification.ValidateWithOperationResolver(resolver); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("catalog-admitted spec without handler = %v, want provider unavailable", err)
	}
	if resolver.CatalogIdentity() != catalog.CatalogIdentity() {
		t.Fatalf("bound resolver identity = %+v, want %+v", resolver.CatalogIdentity(), catalog.CatalogIdentity())
	}

	wrongVersion := resolution.Provider
	wrongVersion.Version = "2"
	mismatchedHandlers, err := NewControlledWorkflowkitProviderRegistry([]WorkflowkitProviderRegistration{{
		Provider: wrongVersion, Adapter: &countingCatalogBoundProvider{},
	}})
	if err != nil {
		t.Fatal(err)
	}
	mismatched, err := NewCatalogBoundWorkflowkitProviderOperationResolver(catalog, mismatchedHandlers)
	if err != nil {
		t.Fatal(err)
	}
	if err := mismatched.ValidateStageOperation(resolution); !errors.Is(err, ErrProviderVersionMismatch) {
		t.Fatalf("catalog-admitted spec with version-mismatched handler = %v, want provider version mismatch", err)
	}
}

func TestCatalogBoundWorkflowkitResolverRejectsDriftBeforeDelegate(t *testing.T) {
	resolution := deploymentCatalogTestResolution(t, workflowadapter.RepoPrepare)
	catalog, err := NewDeploymentOperationCatalogResolver(deploymentCatalogForResolutions(t, resolution))
	if err != nil {
		t.Fatal(err)
	}
	handler := &countingCatalogBoundProvider{}
	providers, err := NewControlledWorkflowkitProviderRegistry([]WorkflowkitProviderRegistration{{
		Provider: resolution.Provider, Adapter: handler,
	}})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewCatalogBoundWorkflowkitProviderOperationResolver(catalog, providers)
	if err != nil {
		t.Fatal(err)
	}

	drifted := resolution.Clone()
	drifted.Operation.Payload = workflowadapter.LocalCommandOperationPayload{CommandID: "harbor-stage", Arguments: []string{"drifted"}}
	if _, err := resolver.ResolveWorkflowkitStageOperation(drifted); !errors.Is(err, ErrFrozenOperationPayloadMismatch) {
		t.Fatalf("drifted catalog-bound resolution = %v, want payload mismatch", err)
	}
	if handler.calls != 0 {
		t.Fatalf("catalog drift reached provider delegate %d time(s)", handler.calls)
	}
	if _, err := resolver.ResolveWorkflowkitStageOperation(resolution); err != nil {
		t.Fatalf("exact catalog-bound resolution: %v", err)
	}
	if handler.calls != 1 {
		t.Fatalf("exact resolution delegate calls = %d, want 1", handler.calls)
	}
}

type countingCatalogBoundProvider struct{ calls int }

func (provider *countingCatalogBoundProvider) ResolveWorkflowkitStageOperation(workflowadapter.StageOperationResolution) (workflowkit.StageExecutor, error) {
	provider.calls++
	return workflowkit.StageExecutorFunc(func(context.Context, workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
		return workflowkit.StageExecutionResult{}, nil
	}), nil
}

func deploymentCatalogTestResolution(t *testing.T, key workflowkit.StageKey) workflowadapter.StageOperationResolution {
	t.Helper()
	specification := testsupport.CompleteRunExecutionSpec(deploymentCatalogTestTaskID, deploymentCatalogTestRevisionID, deploymentCatalogTestDigest)
	resolution, err := specification.ResolveStageOperation(key)
	if err != nil {
		t.Fatalf("resolve %q fixture operation: %v", key, err)
	}
	return resolution
}

func deploymentCatalogForResolutions(t *testing.T, resolutions ...workflowadapter.StageOperationResolution) DeploymentOperationCatalog {
	t.Helper()
	operations := make([]DeploymentOperationRegistration, 0, len(resolutions))
	for _, resolution := range resolutions {
		definition, found := workflowadapter.StandardStageCatalog().Stage(resolution.StageKey)
		if !found {
			t.Fatalf("standard stage %q is missing", resolution.StageKey)
		}
		operations = append(operations, DeploymentOperationRegistration{
			Stage: DeploymentStageContract{
				Key: resolution.StageKey, Type: resolution.StageType, Group: definition.Group, Plugin: resolution.Plugin,
			},
			Provider:  resolution.Provider,
			Operation: resolution.Operation.Clone(),
			Runtime:   resolution.Runtime,
			Checkout:  DeploymentCheckoutContract{ID: resolution.Checkout.ID, Purpose: "codeedge-phase1-controlled"},
			Secrets:   cloneDeploymentSecrets(resolution.Secrets),
		})
	}
	return DeploymentOperationCatalog{
		Format: DeploymentOperationCatalogFormat, Version: DeploymentOperationCatalogVersion,
		CatalogID: "codeedge-phase1-test", CatalogVersion: "test-v1", Template: workflowadapter.StandardTemplateReference(), Operations: operations,
	}
}

func deploymentCatalogForExecutionSpec(t *testing.T, specification workflowadapter.RunExecutionSpec) DeploymentOperationCatalog {
	t.Helper()
	template, err := workflowadapter.ResolveWorkflowTemplate(specification.Template)
	if err != nil {
		t.Fatalf("resolve fixture template: %v", err)
	}
	operations := make([]DeploymentOperationRegistration, 0, len(template.Catalog.Stages))
	for _, definition := range template.Catalog.Stages {
		resolution, err := specification.ResolveStageOperation(definition.Key)
		if err != nil {
			t.Fatalf("resolve fixture stage %q: %v", definition.Key, err)
		}
		operations = append(operations, DeploymentOperationRegistration{
			Stage:    DeploymentStageContract{Key: resolution.StageKey, Type: resolution.StageType, Group: definition.Group, Plugin: resolution.Plugin},
			Provider: resolution.Provider, Operation: resolution.Operation.Clone(), Runtime: resolution.Runtime,
			Checkout: DeploymentCheckoutContract{ID: resolution.Checkout.ID, Purpose: "codeedge-phase1-controlled"}, Secrets: cloneDeploymentSecrets(resolution.Secrets),
		})
	}
	return DeploymentOperationCatalog{
		Format: DeploymentOperationCatalogFormat, Version: DeploymentOperationCatalogVersion,
		CatalogID: "codeedge-phase1-template-test", CatalogVersion: "test-v1", Template: specification.Template, Operations: operations,
	}
}
