package stageprovider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	operationCatalogLockTestTaskID     = "018f0a73-3b49-7000-8000-000000000071"
	operationCatalogLockTestRevisionID = "018f0a73-3b49-7000-8000-000000000072"
	operationCatalogLockTestDigest     = "harbor.task.v2:sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

func TestDeploymentOperationCatalogLockCanonicalStrictJSONAndFingerprint(t *testing.T) {
	catalog, lock, _ := operationCatalogLockFixture(t, workflowadapter.RepoPrepare, workflowadapter.HarborRunQwen)
	resolver, err := NewDeploymentOperationCatalogLockResolver(catalog, lock)
	if err != nil {
		t.Fatalf("construct catalog/lock resolver: %v", err)
	}
	if err := resolver.VerifyCatalogReceipt(catalog.Receipt()); err != nil {
		t.Fatalf("verify configured catalog receipt: %v", err)
	}
	if err := resolver.VerifyLockIdentity(resolver.LockIdentity()); err != nil {
		t.Fatalf("verify configured lock identity: %v", err)
	}

	baselineJSON, err := lock.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := lock.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseDeploymentOperationCatalogLockJSON(baselineJSON)
	if err != nil {
		t.Fatalf("parse canonical lock: %v", err)
	}
	if parsed.LockID != lock.LockID || len(parsed.Operations) != len(lock.Operations) {
		t.Fatalf("parsed lock = %+v, want %q with %d operations", parsed, lock.LockID, len(lock.Operations))
	}
	legacy := lock.Clone()
	legacy.Version = "2"
	if err := legacy.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("legacy v2 lock validation error = %v, want invalid lock", err)
	}
	var direct DeploymentOperationCatalogLock
	if err := json.Unmarshal(baselineJSON, &direct); err != nil {
		t.Fatalf("direct unmarshal must remain strict and valid: %v", err)
	}

	reordered := lock.Clone()
	reordered.Operations[0], reordered.Operations[1] = reordered.Operations[1], reordered.Operations[0]
	// The evaluator fixture has more than one stable secret reference. Sorting
	// the slice remains canonical for either cardinality and tests defensive
	// copying as well.
	if len(reordered.Operations[0].Secrets) > 1 {
		reordered.Operations[0].Secrets[0], reordered.Operations[0].Secrets[1] = reordered.Operations[0].Secrets[1], reordered.Operations[0].Secrets[0]
	}
	canonicalJSON, err := reordered.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := reordered.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if string(canonicalJSON) != string(baselineJSON) || canonical != baseline {
		t.Fatalf("reordered lock changed canonical identity: json=%s fingerprint=%s, want %s", canonicalJSON, canonical, baseline)
	}

	unknownRoot := []byte(strings.Replace(string(baselineJSON), `"lock_version":"test-v1"`, `"lock_version":"test-v1","unexpected":true`, 1))
	unknownNested := []byte(strings.Replace(string(baselineJSON), `"absolute_path":"/opt/harbor/bin/harbor-stage"`, `"absolute_path":"/opt/harbor/bin/harbor-stage","unexpected":true`, 1))
	duplicateKey := []byte(strings.Replace(string(baselineJSON), `"lock_id":"codeedge-phase1-operation-lock-test"`, `"lock_id":"codeedge-phase1-operation-lock-test","lock_id":"codeedge-phase1-operation-lock-test"`, 1))
	trailing := append(append([]byte(nil), baselineJSON...), []byte(" null")...)
	for name, malformed := range map[string][]byte{
		"unknown root field":   unknownRoot,
		"unknown nested field": unknownNested,
		"duplicate key":        duplicateKey,
		"trailing value":       trailing,
		"null operations":      []byte(`{"format":"harbor.operation-catalog.lock.v1","version":"1","lock_id":"codeedge-phase1-operation-lock-test","lock_version":"test-v1","catalog_receipt":{"format":"harbor.deployment-operation-catalog-receipt.v1","version":"1","catalog_format":"harbor.deployment-operation-catalog.v1","catalog_schema_version":"1","catalog_id":"codeedge-phase1-lock-test","catalog_version":"test-v1","catalog_fingerprint":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"harbor_flow_build":{"module":"github.com/purplevoid/harbor-factory","version":"v2.0.0","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","content_sha256":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"operations":null}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseDeploymentOperationCatalogLockJSON(malformed); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
				t.Fatalf("malformed lock error = %v, want invalid lock", err)
			}
		})
	}
}

func TestDeploymentOperationCatalogLockRequiresLockOwnedStandardAuthoringProfile(t *testing.T) {
	catalog, lock, _ := standardAuthoringProviderCompositionFixture(t)
	if _, err := NewDeploymentOperationCatalogLockResolver(catalog, lock); err != nil {
		t.Fatalf("valid Standard authoring lock was rejected: %v", err)
	}
	profile, err := lock.StandardAuthoringProfile()
	if err != nil {
		t.Fatalf("read lock-owned Standard authoring profile: %v", err)
	}
	if len(profile.Stages) == 0 {
		t.Fatal("Standard authoring test profile has no stage budgets")
	}
	profile.Stages[0].Budget.MaxAttempts = 99
	again, err := lock.StandardAuthoringProfile()
	if err != nil {
		t.Fatal(err)
	}
	if again.Stages[0].Budget.MaxAttempts == 99 {
		t.Fatal("Standard authoring profile accessor leaked mutable lock state")
	}

	baseline, err := lock.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	changed := lock.Clone()
	changed.StandardAuthoringExecutionProfile.Profile.Version = "1.0.1"
	updated, err := changed.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if updated == baseline {
		t.Fatal("Standard authoring execution profile did not participate in lock fingerprint")
	}
	canonical, err := lock.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseDeploymentOperationCatalogLockJSON(canonical)
	if err != nil {
		t.Fatalf("parse canonical Standard authoring lock: %v", err)
	}
	parsedProfile, err := parsed.StandardAuthoringProfile()
	if err != nil {
		t.Fatalf("read Standard authoring profile after canonical round trip: %v", err)
	}
	parsedFingerprint, err := parsedProfile.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if parsedFingerprint != baselineProfileFingerprint(t, lock) {
		t.Fatal("canonical Standard authoring lock did not retain the execution profile")
	}

	missing := lock.Clone()
	missing.StandardAuthoringExecutionProfile = nil
	if err := missing.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("Standard authoring lock without profile error = %v, want invalid lock", err)
	}
	missingTransport := lock.Clone()
	missingTransport.StandardAuthoringSSHTransport = nil
	if err := missingTransport.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("Standard authoring lock without SSH transport error = %v, want invalid lock", err)
	}
	wrongKnownHostsVersion := lock.Clone()
	wrongKnownHostsVersion.StandardAuthoringSSHTransport.KnownHosts.Version = "2"
	if err := wrongKnownHostsVersion.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("Standard authoring lock with unsupported known_hosts version error = %v, want invalid lock", err)
	}
	wrongProfile := lock.Clone()
	wrongProfile.StandardAuthoringExecutionProfile.Profile.Template = workflowadapter.StandardTemplateReference()
	if err := wrongProfile.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("Standard authoring lock with another template's profile error = %v, want invalid lock", err)
	}
	mismatchedProfile := lock.Clone()
	mismatchedProfile.StandardAuthoringExecutionProfile.Profile.Template = workflowadapter.TemplateReference{ID: workflowadapter.StandardAuthoringWorkflowTemplateID, Version: "1.9.9"}
	if err := mismatchedProfile.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("Standard authoring lock with another installed profile version error = %v, want invalid lock", err)
	}
	_, wrongTurns, _ := standardAuthoringAgentOnlyCompositionFixture(t)
	if err := wrongTurns.Validate(); err != nil {
		t.Fatalf("valid Standard authoring agent lock was rejected: %v", err)
	}
	payload := wrongTurns.Operations[0].Operation.Payload.(workflowadapter.AgentTurnOperationPayload)
	payload.MaxTurns--
	wrongTurns.Operations[0].Operation.Payload = payload
	if err := wrongTurns.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("Standard authoring lock with drifting agent turns error = %v, want invalid lock", err)
	}
	_, nonStandardLock, _ := operationCatalogLockFixture(t, workflowadapter.RepoPrepare)
	nonStandardLock.StandardAuthoringExecutionProfile = &StandardAuthoringExecutionProfileLock{Profile: standardAuthoringTestExecutionProfile(t)}
	if err := nonStandardLock.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("non-Standard lock carrying Standard profile error = %v, want invalid lock", err)
	}
	nonStandardLock.StandardAuthoringExecutionProfile = nil
	nonStandardLock.StandardAuthoringSSHTransport = standardAuthoringSSHTransportTestLock(t, []byte(standardAuthoringSSHTransportTestKnownHosts))
	if err := nonStandardLock.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("non-Standard lock carrying Standard SSH transport error = %v, want invalid lock", err)
	}
}

func baselineProfileFingerprint(t *testing.T, lock DeploymentOperationCatalogLock) workflowkit.Fingerprint {
	t.Helper()
	profile, err := lock.StandardAuthoringProfile()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := profile.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

func TestDeploymentOperationCatalogLockRejectsDuplicateUnknownUnversionedAndReceiptDrift(t *testing.T) {
	catalog, lock, _ := operationCatalogLockFixture(t, workflowadapter.RepoPrepare)
	cases := []struct {
		name   string
		mutate func(*DeploymentOperationCatalogLock)
	}{
		{
			name: "duplicate operation record",
			mutate: func(candidate *DeploymentOperationCatalogLock) {
				candidate.Operations = append(candidate.Operations, candidate.Operations[0].Clone())
			},
		},
		{
			name: "unversioned lock",
			mutate: func(candidate *DeploymentOperationCatalogLock) {
				candidate.LockVersion = "latest"
			},
		},
		{
			name: "unversioned local executable",
			mutate: func(candidate *DeploymentOperationCatalogLock) {
				candidate.Operations[0].LocalExecutable.Version = ""
			},
		},
		{
			name: "unversioned build",
			mutate: func(candidate *DeploymentOperationCatalogLock) {
				candidate.HarborFlowBuild.Version = "unknown"
			},
		},
		{
			name: "unversioned secret",
			mutate: func(candidate *DeploymentOperationCatalogLock) {
				candidate.Operations[0].Secrets[0].Version = ""
			},
		},
		{
			name: "relative executable path",
			mutate: func(candidate *DeploymentOperationCatalogLock) {
				candidate.Operations[0].LocalExecutable.AbsolutePath = "bin/harbor-stage"
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			candidate := lock.Clone()
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
				t.Fatalf("lock validation error = %v, want invalid lock", err)
			}
		})
	}

	receiptDrift := lock.Clone()
	receiptDrift.CatalogReceipt.CatalogVersion = "other-v1"
	if _, err := NewDeploymentOperationCatalogLockResolver(catalog, receiptDrift); err == nil || !errors.Is(err, ErrDeploymentOperationCatalogLockDrift) {
		t.Fatalf("receipt drift error = %v, want lock drift", err)
	}

	unknown := lock.Clone()
	unknown.Operations[0].Operation.OperationID = "not-in-catalog"
	if _, err := NewDeploymentOperationCatalogLockResolver(catalog, unknown); err == nil || !errors.Is(err, ErrDeploymentOperationCatalogLockDrift) {
		t.Fatalf("unknown operation error = %v, want lock drift", err)
	}

	missingCatalog, missingLock, _ := operationCatalogLockFixture(t, workflowadapter.RepoPrepare, workflowadapter.HarborRunQwen)
	missingLock.Operations = missingLock.Operations[:1]
	if _, err := NewDeploymentOperationCatalogLockResolver(missingCatalog, missingLock); err == nil || !errors.Is(err, ErrDeploymentOperationCatalogLockDrift) {
		t.Fatalf("missing catalog operation error = %v, want lock drift", err)
	}
}

func TestDeploymentOperationCatalogLockRecordPinsContainerAndAgentIdentities(t *testing.T) {
	_, containerLock, _ := operationCatalogLockFixture(t, workflowadapter.HarborRunQwen)
	container := containerLock.Operations[0]
	if container.ContainerRuntime == nil {
		t.Fatal("container fixture did not create a pinned runtime record")
	}
	if err := container.Validate(); err != nil {
		t.Fatalf("validate pinned container record: %v", err)
	}
	containerRuntimeDrift := container.Clone()
	containerRuntimeDrift.ContainerRuntime.Runtime.Version = "v2.0.0"
	if err := containerRuntimeDrift.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("container runtime drift error = %v, want invalid lock", err)
	}
	containerImageDrift := container.Clone()
	containerImageDrift.ContainerRuntime.ImageDigest = "registry.example/harbor/evaluator@sha256:" + strings.Repeat("a", 64)
	if err := containerImageDrift.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("container image drift error = %v, want invalid lock", err)
	}

	localCatalog, localLock, _ := operationCatalogLockFixture(t, workflowadapter.RepoPrepare)
	agent := localLock.Operations[0].Clone()
	agent.Operation.Payload = workflowadapter.AgentTurnOperationPayload{
		AgentID: "codeedge-agent", ModelID: "codeedge-model", ReasoningEffort: workflowadapter.AgentReasoningEffortHigh, MaxTurns: 4,
	}
	agent.ExecutionKind = workflowadapter.StageOperationPayloadAgentTurn
	agent.LocalExecutable = nil
	agent.AgentModel = &AgentModelLock{
		AgentID: "codeedge-agent", AgentVersion: "v1.0.0", ModelID: "codeedge-model", ModelVersion: "v2026.07",
	}
	if err := agent.Validate(); err != nil {
		t.Fatalf("validate pinned agent/model/secret record: %v", err)
	}
	agentVersionDrift := agent.Clone()
	agentVersionDrift.AgentModel.ModelVersion = ""
	if err := agentVersionDrift.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("unversioned agent model error = %v, want invalid lock", err)
	}
	agentModelDrift := agent.Clone()
	agentModelDrift.AgentModel.ModelID = "other-model"
	if err := agentModelDrift.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("agent model identity drift error = %v, want invalid lock", err)
	}

	// The lock compares the versioned secret-reference set with the catalog,
	// not merely its shape. Construct an agent catalog to prove that a changed
	// still-versioned secret is rejected as contract drift.
	agentCatalogDocument := localCatalog.Catalog()
	agentCatalogDocument.Operations[0].Operation.Payload = agent.Operation.Payload
	agentCatalog, err := NewDeploymentOperationCatalogResolver(agentCatalogDocument)
	if err != nil {
		t.Fatalf("construct agent catalog: %v", err)
	}
	agentLock := localLock.Clone()
	agentLock.CatalogReceipt = agentCatalog.Receipt()
	agentLock.Operations[0] = agent
	if _, err := NewDeploymentOperationCatalogLockResolver(agentCatalog, agentLock); err != nil {
		t.Fatalf("construct exact agent catalog/lock: %v", err)
	}
	secretDrift := agentLock.Clone()
	secretDrift.Operations[0].Secrets[0].Version = "v2.0.0"
	if _, err := NewDeploymentOperationCatalogLockResolver(agentCatalog, secretDrift); err == nil || !errors.Is(err, ErrDeploymentOperationCatalogLockDrift) {
		t.Fatalf("agent secret reference drift error = %v, want lock drift", err)
	}

	// Older frozen records did not carry reasoning_effort. They must remain
	// readable and canonicalizable for audit/reconciliation even though the
	// current Standard authoring runtime will refuse to execute them.
	legacyCatalogDocument := agentCatalog.Catalog()
	legacyPayload := agent.Operation.Payload.(workflowadapter.AgentTurnOperationPayload)
	legacyPayload.ReasoningEffort = ""
	legacyCatalogDocument.Operations[0].Operation.Payload = legacyPayload
	legacyCatalog, err := NewDeploymentOperationCatalogResolver(legacyCatalogDocument)
	if err != nil {
		t.Fatalf("decode legacy agent catalog: %v", err)
	}
	legacyLock := agentLock.Clone()
	legacyLock.CatalogReceipt = legacyCatalog.Receipt()
	legacyLock.Operations[0].Operation.Payload = legacyPayload
	legacyCanonical, err := legacyLock.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonicalize legacy agent lock: %v", err)
	}
	if strings.Contains(string(legacyCanonical), "reasoning_effort") {
		t.Fatalf("legacy lock canonicalization introduced a new field: %s", legacyCanonical)
	}
	parsedLegacy, err := ParseDeploymentOperationCatalogLockJSON(legacyCanonical)
	if err != nil {
		t.Fatalf("parse legacy agent lock: %v", err)
	}
	parsedCanonical, err := parsedLegacy.CanonicalJSON()
	if err != nil {
		t.Fatalf("recall canonical legacy agent lock: %v", err)
	}
	if string(parsedCanonical) != string(legacyCanonical) {
		t.Fatalf("legacy agent lock canonical bytes drifted:\n got %s\nwant %s", parsedCanonical, legacyCanonical)
	}
	if _, err := NewDeploymentOperationCatalogLockResolver(legacyCatalog, parsedLegacy); err != nil {
		t.Fatalf("resolve historical agent lock for audit: %v", err)
	}
}

func TestDeploymentOperationCatalogLockRecordPinsHarborBuiltinIdentity(t *testing.T) {
	catalog, lock, _ := operationCatalogLockFixture(t, workflowadapter.RepoPrepare)
	builtin := lock.Operations[0].Clone()
	builtin.Operation.Payload = workflowadapter.HarborBuiltinOperationPayload{HandlerID: "standard-authoring.materialize-task"}
	builtin.ExecutionKind = workflowadapter.StageOperationPayloadHarborBuiltin
	builtin.LocalExecutable = nil
	builtin.HarborFlowBuiltin = &HarborFlowBuiltinOperationLock{
		Format: HarborFlowBuiltinOperationLockFormat, Version: HarborFlowBuiltinOperationLockVersion,
		HandlerID: "standard-authoring.materialize-task", HandlerVersion: "1.0.0",
	}
	if err := builtin.Validate(); err != nil {
		t.Fatalf("validate Harbor built-in record: %v", err)
	}

	wrongHandler := builtin.Clone()
	wrongHandler.HarborFlowBuiltin.HandlerID = "standard-authoring.other"
	if err := wrongHandler.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("mismatched Harbor built-in handler = %v, want invalid lock", err)
	}
	withExecutable := builtin.Clone()
	withExecutable.LocalExecutable = &LocalExecutableLock{
		CommandID: "unrelated", AbsolutePath: "/opt/harbor/bin/unrelated", Version: "v1.0.0",
		ContentSHA256: workflowkit.SHA256Fingerprint([]byte("unrelated")),
	}
	if err := withExecutable.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("Harbor built-in with executable = %v, want invalid lock", err)
	}

	catalogDocument := catalog.Catalog()
	catalogDocument.Operations[0].Operation = builtin.Operation.Clone()
	builtinCatalog, err := NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatal(err)
	}
	builtinLock := lock.Clone()
	builtinLock.CatalogReceipt = builtinCatalog.Receipt()
	builtinLock.Operations[0] = builtin
	if _, err := NewDeploymentOperationCatalogLockResolver(builtinCatalog, builtinLock); err != nil {
		t.Fatalf("construct exact Harbor built-in catalog/lock: %v", err)
	}
}

func TestCatalogLockAttestedResolverRejectsStaticDriftAndMissingOrFailedAttestationBeforeDelegate(t *testing.T) {
	catalog, lock, resolutions := operationCatalogLockFixture(t, workflowadapter.RepoPrepare)
	verifier, err := NewDeploymentOperationCatalogLockResolver(catalog, lock)
	if err != nil {
		t.Fatal(err)
	}
	resolution := resolutions[0]

	t.Run("forwards installed lock identity verification", func(t *testing.T) {
		resolver, err := NewCatalogLockAttestedWorkflowkitProviderOperationResolver(verifier, &countingOperationCatalogLockDelegate{executor: &countingOperationCatalogLockExecutor{}}, &countingOperationCatalogLockAttestor{})
		if err != nil {
			t.Fatal(err)
		}
		if err := resolver.VerifyLockIdentity(verifier.LockIdentity()); err != nil {
			t.Fatalf("verify exact forwarded lock identity: %v", err)
		}
		drifted := verifier.LockIdentity()
		drifted.LockVersion = "other-v1"
		if err := resolver.VerifyLockIdentity(drifted); !errors.Is(err, ErrDeploymentOperationCatalogLockDrift) {
			t.Fatalf("forwarded drifted lock identity = %v, want lock drift", err)
		}
	})

	t.Run("static drift never reaches delegate resolver", func(t *testing.T) {
		delegate := &countingOperationCatalogLockDelegate{executor: &countingOperationCatalogLockExecutor{}}
		resolver, err := NewCatalogLockAttestedWorkflowkitProviderOperationResolver(verifier, delegate, &countingOperationCatalogLockAttestor{})
		if err != nil {
			t.Fatal(err)
		}
		drifted := resolution.Clone()
		drifted.Operation.Payload = workflowadapter.LocalCommandOperationPayload{CommandID: "harbor-stage", Arguments: []string{"drifted"}}
		if _, err := resolver.ResolveWorkflowkitStageOperation(drifted); err == nil || !errors.Is(err, ErrFrozenOperationPayloadMismatch) {
			t.Fatalf("static drift error = %v, want frozen payload mismatch", err)
		}
		if delegate.resolveCalls != 0 || delegate.executor.calls != 0 {
			t.Fatalf("static drift reached delegate: resolves=%d executes=%d", delegate.resolveCalls, delegate.executor.calls)
		}
	})

	t.Run("missing attestor blocks executor", func(t *testing.T) {
		delegate := &countingOperationCatalogLockDelegate{executor: &countingOperationCatalogLockExecutor{}}
		resolver, err := NewCatalogLockAttestedWorkflowkitProviderOperationResolver(verifier, delegate, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := resolver.ResolveWorkflowkitStageOperation(resolution); !errors.Is(err, ErrDeploymentOperationRuntimeAttestationUnavailable) {
			t.Fatalf("missing attestation error = %v, want unavailable", err)
		}
		if delegate.resolveCalls != 0 || delegate.executor.calls != 0 {
			t.Fatalf("missing attestation reached delegate: resolves=%d executes=%d", delegate.resolveCalls, delegate.executor.calls)
		}
	})

	t.Run("failed attestor blocks executor", func(t *testing.T) {
		delegate := &countingOperationCatalogLockDelegate{executor: &countingOperationCatalogLockExecutor{}}
		attestor := &countingOperationCatalogLockAttestor{err: errors.New("binary content changed")}
		resolver, err := NewCatalogLockAttestedWorkflowkitProviderOperationResolver(verifier, delegate, attestor)
		if err != nil {
			t.Fatal(err)
		}
		executor, err := resolver.ResolveWorkflowkitStageOperation(resolution)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := executor.ExecuteStage(context.Background(), operationCatalogLockStageRequest(resolution)); !errors.Is(err, ErrDeploymentOperationRuntimeAttestationFailed) {
			t.Fatalf("failed attestation error = %v, want failed", err)
		}
		if attestor.calls != 1 || delegate.executor.calls != 0 {
			t.Fatalf("failed attestation calls=%d delegate executes=%d, want 1/0", attestor.calls, delegate.executor.calls)
		}
	})
}

func TestCatalogLockAttestedResolverRechecksStaticLockAndAttestsEveryExecution(t *testing.T) {
	catalog, lock, resolutions := operationCatalogLockFixture(t, workflowadapter.RepoPrepare)
	base, err := NewDeploymentOperationCatalogLockResolver(catalog, lock)
	if err != nil {
		t.Fatal(err)
	}
	resolution := resolutions[0]

	t.Run("static lock is rechecked immediately before executor", func(t *testing.T) {
		flipped := &flipOperationCatalogLockVerifier{base: base, failOnStageVerification: 2}
		delegate := &countingOperationCatalogLockDelegate{executor: &countingOperationCatalogLockExecutor{}}
		attestor := &countingOperationCatalogLockAttestor{}
		resolver, err := NewCatalogLockAttestedWorkflowkitProviderOperationResolver(flipped, delegate, attestor)
		if err != nil {
			t.Fatal(err)
		}
		executor, err := resolver.ResolveWorkflowkitStageOperation(resolution)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := executor.ExecuteStage(context.Background(), operationCatalogLockStageRequest(resolution)); !errors.Is(err, ErrDeploymentOperationCatalogLockDrift) {
			t.Fatalf("rechecked static lock error = %v, want lock drift", err)
		}
		if attestor.calls != 0 || delegate.executor.calls != 0 {
			t.Fatalf("static lock drift reached attestor/delegate: %d/%d", attestor.calls, delegate.executor.calls)
		}
	})

	t.Run("attestor runs for each execution", func(t *testing.T) {
		delegate := &countingOperationCatalogLockDelegate{executor: &countingOperationCatalogLockExecutor{}}
		attestor := &countingOperationCatalogLockAttestor{}
		resolver, err := NewCatalogLockAttestedWorkflowkitProviderOperationResolver(base, delegate, attestor)
		if err != nil {
			t.Fatal(err)
		}
		executor, err := resolver.ResolveWorkflowkitStageOperation(resolution)
		if err != nil {
			t.Fatal(err)
		}
		for attempt := 0; attempt < 2; attempt++ {
			if _, err := executor.ExecuteStage(context.Background(), operationCatalogLockStageRequest(resolution)); err != nil {
				t.Fatalf("execution %d: %v", attempt, err)
			}
		}
		if attestor.calls != 2 || delegate.executor.calls != 2 {
			t.Fatalf("attestor/delegate calls = %d/%d, want 2/2", attestor.calls, delegate.executor.calls)
		}
		if attestor.last.Record.LocalExecutable == nil || attestor.last.Resolution.StageKey != resolution.StageKey || attestor.last.LockIdentity != base.LockIdentity() || attestor.last.HarborFlowBuild != base.HarborFlowBuild() {
			t.Fatalf("runtime attestation evidence = %+v, want frozen lock record/resolution/identity", attestor.last)
		}
	})
}

func operationCatalogLockFixture(t *testing.T, keys ...workflowkit.StageKey) (*DeploymentOperationCatalogResolver, DeploymentOperationCatalogLock, []workflowadapter.StageOperationResolution) {
	t.Helper()
	resolutions := make([]workflowadapter.StageOperationResolution, 0, len(keys))
	for _, key := range keys {
		resolutions = append(resolutions, operationCatalogLockTestResolution(t, key))
	}
	catalogDocument := operationCatalogLockCatalogForResolutions(t, resolutions...)
	catalog, err := NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatal(err)
	}
	lock := DeploymentOperationCatalogLock{
		Format: DeploymentOperationCatalogLockFormat, Version: DeploymentOperationCatalogLockVersion,
		LockID: "codeedge-phase1-operation-lock-test", LockVersion: "test-v1",
		CatalogReceipt: catalog.Receipt(),
		HarborFlowBuild: HarborFlowBuildIdentity{
			Module: "github.com/purplevoid/harbor-factory", Version: "v2.0.0",
			Commit: strings.Repeat("a", 40), ContentSHA256: workflowkit.SHA256Fingerprint([]byte("harbor-flow-build-test")),
		},
		Operations: make([]DeploymentOperationCatalogLockRecord, 0, len(catalogDocument.Operations)),
	}
	for _, registration := range catalogDocument.Operations {
		lock.Operations = append(lock.Operations, operationCatalogLockRecord(t, registration))
	}
	return catalog, lock, resolutions
}

func operationCatalogLockTestResolution(t *testing.T, key workflowkit.StageKey) workflowadapter.StageOperationResolution {
	t.Helper()
	specification := testsupport.CompleteRunExecutionSpec(operationCatalogLockTestTaskID, operationCatalogLockTestRevisionID, operationCatalogLockTestDigest)
	resolution, err := specification.ResolveStageOperation(key)
	if err != nil {
		t.Fatalf("resolve fixture stage %q: %v", key, err)
	}
	return resolution
}

func operationCatalogLockCatalogForResolutions(t *testing.T, resolutions ...workflowadapter.StageOperationResolution) DeploymentOperationCatalog {
	t.Helper()
	operations := make([]DeploymentOperationRegistration, 0, len(resolutions))
	for _, resolution := range resolutions {
		definition, found := workflowadapter.StandardStageCatalog().Stage(resolution.StageKey)
		if !found {
			t.Fatalf("standard stage %q is missing", resolution.StageKey)
		}
		operations = append(operations, DeploymentOperationRegistration{
			Stage:    DeploymentStageContract{Key: resolution.StageKey, Type: resolution.StageType, Group: definition.Group, Plugin: resolution.Plugin},
			Provider: resolution.Provider, Operation: resolution.Operation.Clone(), Runtime: resolution.Runtime,
			Checkout: DeploymentCheckoutContract{ID: resolution.Checkout.ID, Purpose: "codeedge-phase1-controlled"}, Secrets: cloneDeploymentSecrets(resolution.Secrets),
		})
	}
	return DeploymentOperationCatalog{
		Format: DeploymentOperationCatalogFormat, Version: DeploymentOperationCatalogVersion,
		CatalogID: "codeedge-phase1-lock-test", CatalogVersion: "test-v1", Template: workflowadapter.StandardTemplateReference(), Operations: operations,
	}
}

func operationCatalogLockRecord(t *testing.T, registration DeploymentOperationRegistration) DeploymentOperationCatalogLockRecord {
	t.Helper()
	record := DeploymentOperationCatalogLockRecord{
		Stage: registration.Stage, Provider: registration.Provider, Operation: registration.Operation.Clone(), Runtime: registration.Runtime,
		Checkout: registration.Checkout, Secrets: cloneDeploymentSecrets(registration.Secrets),
		PromptContentFingerprint: workflowkit.SHA256Fingerprint([]byte("prompt:" + string(registration.Stage.Key))),
		SchemaContentFingerprint: workflowkit.SHA256Fingerprint([]byte("schema:" + string(registration.Stage.Key))),
		ExecutionKind:            registration.Operation.Payload.Kind(),
	}
	switch payload := registration.Operation.Payload.(type) {
	case workflowadapter.LocalCommandOperationPayload:
		record.LocalExecutable = &LocalExecutableLock{
			CommandID: payload.CommandID, AbsolutePath: "/opt/harbor/bin/" + payload.CommandID,
			Version: "v1.2.3", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("local:" + payload.CommandID)),
		}
	case workflowadapter.ContainerCommandOperationPayload:
		record.ContainerRuntime = &PinnedContainerRuntimeLock{ImageDigest: payload.ImageDigest, Runtime: registration.Runtime}
	case workflowadapter.AgentTurnOperationPayload:
		record.AgentModel = &AgentModelLock{
			AgentID: payload.AgentID, AgentVersion: "v1.0.0", ModelID: payload.ModelID, ModelVersion: "v1.0.0",
		}
	case workflowadapter.DurableReviewOperationPayload:
		record.DurableReviewPolicy = &DurableReviewPolicyLock{PolicyID: payload.PolicyID, Version: "v1.0.0"}
	default:
		t.Fatalf("unsupported fixture payload %T", registration.Operation.Payload)
	}
	return record
}

func operationCatalogLockStageRequest(resolution workflowadapter.StageOperationResolution) workflowkit.StageExecutionRequest {
	return workflowkit.StageExecutionRequest{Stage: workflowkit.StageDescriptor{Key: resolution.StageKey}}
}

type countingOperationCatalogLockDelegate struct {
	validateCalls int
	resolveCalls  int
	executor      *countingOperationCatalogLockExecutor
}

func (delegate *countingOperationCatalogLockDelegate) ValidateStageOperation(workflowadapter.StageOperationResolution) error {
	delegate.validateCalls++
	return nil
}

func (delegate *countingOperationCatalogLockDelegate) ResolveWorkflowkitStageOperation(workflowadapter.StageOperationResolution) (workflowkit.StageExecutor, error) {
	delegate.resolveCalls++
	return delegate.executor, nil
}

type countingOperationCatalogLockExecutor struct{ calls int }

func (executor *countingOperationCatalogLockExecutor) ExecuteStage(context.Context, workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
	executor.calls++
	return workflowkit.StageExecutionResult{}, nil
}

type countingOperationCatalogLockAttestor struct {
	calls int
	err   error
	last  DeploymentOperationRuntimeAttestation
}

func (attestor *countingOperationCatalogLockAttestor) AttestDeploymentOperation(_ context.Context, evidence DeploymentOperationRuntimeAttestation) error {
	attestor.calls++
	attestor.last = evidence
	return attestor.err
}

type flipOperationCatalogLockVerifier struct {
	base                    DeploymentOperationCatalogLockVerifier
	stageVerifications      int
	failOnStageVerification int
}

func (verifier *flipOperationCatalogLockVerifier) CatalogIdentity() DeploymentOperationCatalogIdentity {
	return verifier.base.CatalogIdentity()
}

func (verifier *flipOperationCatalogLockVerifier) CatalogReceipt() DeploymentOperationCatalogReceipt {
	return verifier.base.CatalogReceipt()
}

func (verifier *flipOperationCatalogLockVerifier) LockIdentity() DeploymentOperationCatalogLockIdentity {
	return verifier.base.LockIdentity()
}

func (verifier *flipOperationCatalogLockVerifier) HarborFlowBuild() HarborFlowBuildIdentity {
	return verifier.base.HarborFlowBuild()
}

func (verifier *flipOperationCatalogLockVerifier) VerifyCatalogReceipt(receipt DeploymentOperationCatalogReceipt) error {
	return verifier.base.VerifyCatalogReceipt(receipt)
}

func (verifier *flipOperationCatalogLockVerifier) VerifyLockIdentity(identity DeploymentOperationCatalogLockIdentity) error {
	return verifier.base.VerifyLockIdentity(identity)
}

func (verifier *flipOperationCatalogLockVerifier) VerifyStageOperation(resolution workflowadapter.StageOperationResolution) (DeploymentOperationCatalogLockRecord, error) {
	verifier.stageVerifications++
	if verifier.stageVerifications >= verifier.failOnStageVerification {
		return DeploymentOperationCatalogLockRecord{}, ErrDeploymentOperationCatalogLockDrift
	}
	return verifier.base.VerifyStageOperation(resolution)
}

var _ WorkflowkitProviderOperationResolver = (*countingOperationCatalogLockDelegate)(nil)
var _ DeploymentOperationRuntimeAttestor = (*countingOperationCatalogLockAttestor)(nil)
var _ DeploymentOperationCatalogLockVerifier = (*flipOperationCatalogLockVerifier)(nil)
