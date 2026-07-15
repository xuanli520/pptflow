package stageprovider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringRuntimeAttestorProvesBuiltinLinkedBuild(t *testing.T) {
	attestation := standardAuthoringBuiltinAttestation(t)
	attestor, err := NewStandardAuthoringRuntimeAttestor(StandardAuthoringRuntimeAttestorConfig{HarborFlowBuild: attestation.HarborFlowBuild, ContractRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := attestor.AttestDeploymentOperation(context.Background(), attestation); err != nil {
		t.Fatalf("attest exact Harbor built-in: %v", err)
	}

	driftedBuild := attestation
	driftedBuild.HarborFlowBuild.Commit = strings.Repeat("b", 40)
	if err := attestor.AttestDeploymentOperation(context.Background(), driftedBuild); err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationFailed) {
		t.Fatalf("drifted Harbor Flow build = %v, want attestation failure", err)
	}

	driftedHandler := attestation
	driftedHandler.Record = attestation.Record.Clone()
	driftedHandler.Record.HarborFlowBuiltin.HandlerID = "standard-authoring.other"
	if err := attestor.AttestDeploymentOperation(context.Background(), driftedHandler); err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationFailed) {
		t.Fatalf("drifted built-in handler = %v, want attestation failure", err)
	}
}

func TestStandardAuthoringRuntimeAttestorProvesPinnedGitAndRejectsUnknownHostCommand(t *testing.T) {
	root := t.TempDir()
	gitPath := filepath.Join(root, "git")
	contents := "#!/bin/sh\nprintf 'git version 2.47.3\\n'\n"
	writeStandardAuthoringAttestorTestFile(t, gitPath, contents)
	attestation := standardAuthoringLocalAttestation(t, workflowadapter.LocalCommandOperationPayload{
		CommandID: StandardAuthoringGitSnapshotCommandID, Arguments: []string{},
	}, LocalExecutableLock{
		CommandID: StandardAuthoringGitSnapshotCommandID, AbsolutePath: gitPath, Version: "2.47.3",
		ContentSHA256: workflowkit.SHA256Fingerprint([]byte(contents)),
	})
	attestor, err := NewStandardAuthoringRuntimeAttestor(StandardAuthoringRuntimeAttestorConfig{HarborFlowBuild: attestation.HarborFlowBuild, ContractRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := attestor.AttestDeploymentOperation(context.Background(), attestation); err != nil {
		t.Fatalf("attest pinned Git: %v", err)
	}

	wrongVersionContents := "#!/bin/sh\nprintf 'git version 2.47.2\\n'\n"
	writeStandardAuthoringAttestorTestFile(t, gitPath, wrongVersionContents)
	driftedOutput := attestation
	driftedOutput.Record = attestation.Record.Clone()
	driftedOutput.Record.LocalExecutable.ContentSHA256 = workflowkit.SHA256Fingerprint([]byte(wrongVersionContents))
	if err := attestor.AttestDeploymentOperation(context.Background(), driftedOutput); err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationFailed) {
		t.Fatalf("matching-hash Git version drift = %v, want attestation failure", err)
	}

	unknownPath := filepath.Join(root, "unknown")
	writeStandardAuthoringAttestorTestFile(t, unknownPath, contents)
	unknown := standardAuthoringLocalAttestation(t, workflowadapter.LocalCommandOperationPayload{CommandID: "standard-authoring.unknown", Arguments: []string{}}, LocalExecutableLock{
		CommandID: "standard-authoring.unknown", AbsolutePath: unknownPath, Version: "2.47.3",
		ContentSHA256: workflowkit.SHA256Fingerprint([]byte(contents)),
	})
	unknownAttestor, err := NewStandardAuthoringRuntimeAttestor(StandardAuthoringRuntimeAttestorConfig{HarborFlowBuild: unknown.HarborFlowBuild, ContractRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := unknownAttestor.AttestDeploymentOperation(context.Background(), unknown); err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationUnavailable) {
		t.Fatalf("unknown local command = %v, want unavailable", err)
	}
}

func TestStandardAuthoringRuntimeAttestorRequiresDockerDaemonMatch(t *testing.T) {
	root := t.TempDir()
	dockerPath := filepath.Join(root, "docker")
	contents := "#!/bin/sh\nprintf 'Docker version 29.5.2, build controlled\\n'\n"
	writeStandardAuthoringAttestorTestFile(t, dockerPath, contents)
	attestation := standardAuthoringLocalAttestation(t, workflowadapter.LocalCommandOperationPayload{
		CommandID: StandardAuthoringDockerCommandID, Arguments: []string{},
	}, LocalExecutableLock{
		CommandID: StandardAuthoringDockerCommandID, AbsolutePath: dockerPath, Version: "29.5.2",
		ContentSHA256: workflowkit.SHA256Fingerprint([]byte(contents)),
	})
	attestor, err := NewStandardAuthoringRuntimeAttestor(StandardAuthoringRuntimeAttestorConfig{
		HarborFlowBuild:  attestation.HarborFlowBuild,
		HostCommandProbe: standardAuthoringHostProbe{version: "Docker version 29.5.2, build controlled", client: "29.5.2", server: "29.5.2"},
		ContractRoot:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := attestor.AttestDeploymentOperation(context.Background(), attestation); err != nil {
		t.Fatalf("attest Docker client/server: %v", err)
	}

	wrongServer, err := NewStandardAuthoringRuntimeAttestor(StandardAuthoringRuntimeAttestorConfig{
		HarborFlowBuild:  attestation.HarborFlowBuild,
		HostCommandProbe: standardAuthoringHostProbe{version: "Docker version 29.5.2, build controlled", client: "29.5.2", server: "29.5.1"},
		ContractRoot:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wrongServer.AttestDeploymentOperation(context.Background(), attestation); err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationFailed) {
		t.Fatalf("Docker daemon version drift = %v, want attestation failure", err)
	}
}

func TestStandardAuthoringRuntimeAttestorDelegatesPinnedCodexAppServer(t *testing.T) {
	fixture := newCodexAppServerAttestationFixture(t)
	attestor, err := NewStandardAuthoringRuntimeAttestor(StandardAuthoringRuntimeAttestorConfig{HarborFlowBuild: fixture.attestation.HarborFlowBuild, ContractRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := attestor.AttestDeploymentOperation(context.Background(), fixture.attestation); err != nil {
		t.Fatalf("generic Codex route: %v", err)
	}
	invocation, err := attestor.AttestCodexAppServerOperation(context.Background(), fixture.attestation)
	if err != nil {
		t.Fatalf("dedicated Codex route: %v", err)
	}
	if invocation.ModelID != CodexAppServerProductionModelID || invocation.JavaScriptLauncherPath != fixture.launcher {
		t.Fatalf("Codex invocation = %+v, want exact pinned runtime", invocation)
	}
}

type standardAuthoringHostProbe struct {
	version string
	client  string
	server  string
	err     error
}

func (probe standardAuthoringHostProbe) ProbeHostCommandVersion(context.Context, LocalExecutableLock) (string, error) {
	return probe.version, probe.err
}

func (probe standardAuthoringHostProbe) ProbeDockerDaemonVersion(context.Context, LocalExecutableLock) (string, string, error) {
	return probe.client, probe.server, probe.err
}

func standardAuthoringBuiltinAttestation(t *testing.T) DeploymentOperationRuntimeAttestation {
	t.Helper()
	catalog, lock, resolutions := operationCatalogLockFixture(t, workflowadapter.RepoPrepare)
	resolution := resolutions[0].Clone()
	resolution.Operation.Payload = workflowadapter.HarborBuiltinOperationPayload{HandlerID: "standard-authoring.materialize-task"}
	catalogDocument := catalog.Catalog()
	catalogDocument.Operations[0].Operation = resolution.Operation.Clone()
	builtinCatalog, err := NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatal(err)
	}
	record := lock.Operations[0].Clone()
	record.Operation = resolution.Operation.Clone()
	record.ExecutionKind = workflowadapter.StageOperationPayloadHarborBuiltin
	record.LocalExecutable = nil
	record.HarborFlowBuiltin = &HarborFlowBuiltinOperationLock{
		Format: HarborFlowBuiltinOperationLockFormat, Version: HarborFlowBuiltinOperationLockVersion,
		HandlerID: "standard-authoring.materialize-task", HandlerVersion: "1.0.0",
	}
	builtinLock := lock.Clone()
	builtinLock.CatalogReceipt = builtinCatalog.Receipt()
	builtinLock.Operations[0] = record
	verifier, err := NewDeploymentOperationCatalogLockResolver(builtinCatalog, builtinLock)
	if err != nil {
		t.Fatal(err)
	}
	return DeploymentOperationRuntimeAttestation{
		CatalogReceipt: verifier.CatalogReceipt(), LockIdentity: verifier.LockIdentity(), HarborFlowBuild: verifier.HarborFlowBuild(),
		Record: record.Clone(), Resolution: resolution,
	}
}

func standardAuthoringLocalAttestation(t *testing.T, payload workflowadapter.LocalCommandOperationPayload, executable LocalExecutableLock) DeploymentOperationRuntimeAttestation {
	t.Helper()
	catalog, lock, resolutions := operationCatalogLockFixture(t, workflowadapter.RepoPrepare)
	resolution := resolutions[0].Clone()
	resolution.Operation.Payload = payload
	catalogDocument := catalog.Catalog()
	catalogDocument.Operations[0].Operation = resolution.Operation.Clone()
	localCatalog, err := NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatal(err)
	}
	record := lock.Operations[0].Clone()
	record.Operation = resolution.Operation.Clone()
	record.ExecutionKind = workflowadapter.StageOperationPayloadLocalCommand
	record.LocalExecutable = &executable
	localLock := lock.Clone()
	localLock.CatalogReceipt = localCatalog.Receipt()
	localLock.Operations[0] = record
	verifier, err := NewDeploymentOperationCatalogLockResolver(localCatalog, localLock)
	if err != nil {
		t.Fatal(err)
	}
	return DeploymentOperationRuntimeAttestation{
		CatalogReceipt: verifier.CatalogReceipt(), LockIdentity: verifier.LockIdentity(), HarborFlowBuild: verifier.HarborFlowBuild(),
		Record: record.Clone(), Resolution: resolution,
	}
}

func writeStandardAuthoringAttestorTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}
