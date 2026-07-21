package stageprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestCodexAppServerOperationLockValidatesTypedRuntimeContract(t *testing.T) {
	fixture := newCodexAppServerAttestationFixture(t)
	cases := []struct {
		name   string
		mutate func(*CodexAppServerOperationLock)
	}{
		{
			name: "legacy v1 format",
			mutate: func(lock *CodexAppServerOperationLock) {
				lock.Format = "harbor.codex-app-server-operation.v1"
			},
		},
		{
			name: "legacy v1 version",
			mutate: func(lock *CodexAppServerOperationLock) {
				lock.Version = "1"
			},
		},
		{
			name: "unversioned launcher",
			mutate: func(lock *CodexAppServerOperationLock) {
				lock.JavaScriptLauncher.Version = "latest"
			},
		},
		{
			name: "relative CODEX_HOME",
			mutate: func(lock *CodexAppServerOperationLock) {
				lock.CodexHomeDirectory = "relative/codex-home"
			},
		},
		{
			name: "wrong node command id",
			mutate: func(lock *CodexAppServerOperationLock) {
				lock.NodeExecutable.CommandID = "node"
			},
		},
		{
			name: "node basename cannot redirect strict shebang",
			mutate: func(lock *CodexAppServerOperationLock) {
				lock.NodeExecutable.AbsolutePath = filepath.Join(t.TempDir(), "node-custom")
			},
		},
		{
			name: "node PATH separator injection",
			mutate: func(lock *CodexAppServerOperationLock) {
				lock.NodeExecutable.AbsolutePath = "/controlled" + string(os.PathListSeparator) + "/redirect/node"
			},
		},
		{
			name: "version output drift",
			mutate: func(lock *CodexAppServerOperationLock) {
				lock.CLIVersionOutput = "codex-cli 0.133.1"
			},
		},
		{
			name: "workspace write sandbox",
			mutate: func(lock *CodexAppServerOperationLock) {
				lock.SandboxMode = "workspace-write"
			},
		},
		{
			name: "workspace write policy",
			mutate: func(lock *CodexAppServerOperationLock) {
				lock.SandboxPolicy = "workspaceWrite"
			},
		},
		{
			name: "network enabled",
			mutate: func(lock *CodexAppServerOperationLock) {
				lock.NetworkAccess = true
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			candidate := fixture.lock.Clone()
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
				t.Fatalf("lock validation error = %v, want invalid deployment lock", err)
			}
		})
	}
	for name, mutate := range map[string]func(*DeploymentOperationCatalogLockRecord){
		"unapproved model": func(candidate *DeploymentOperationCatalogLockRecord) {
			candidate.AgentModel.ModelID = "gpt-5.4"
			payload := candidate.Operation.Payload.(workflowadapter.AgentTurnOperationPayload)
			payload.ModelID = candidate.AgentModel.ModelID
			candidate.Operation.Payload = payload
		},
		"legacy missing reasoning effort": func(candidate *DeploymentOperationCatalogLockRecord) {
			payload := candidate.Operation.Payload.(workflowadapter.AgentTurnOperationPayload)
			payload.ReasoningEffort = ""
			candidate.Operation.Payload = payload
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := fixture.attestation.Record.Clone()
			mutate(&candidate)
			if err := candidate.Validate(); err != nil {
				t.Fatalf("historical Codex lock record should remain decodable: %v", err)
			}
			attestation := fixture.attestation
			attestation.Record = candidate
			attestor, err := NewCodexAppServerRuntimeAttestor(CodexAppServerRuntimeAttestorConfig{HarborFlowBuild: fixture.attestation.HarborFlowBuild})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := attestor.AttestCodexAppServerOperation(context.Background(), attestation); err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationFailed) {
				t.Fatalf("legacy/unapproved Codex execution error = %v, want runtime attestation failure", err)
			}
		})
	}

	t.Run("extension is only valid for agent turn", func(t *testing.T) {
		resolution := operationCatalogLockTestResolution(t, workflowadapter.RepoPrepare)
		catalogDocument := operationCatalogLockCatalogForResolutions(t, resolution)
		record := operationCatalogLockRecord(t, catalogDocument.Operations[0])
		candidate := fixture.lock.Clone()
		record.CodexAppServer = &candidate
		if err := record.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
			t.Fatalf("non-agent Codex extension error = %v, want invalid deployment lock", err)
		}
	})
}

func TestCodexAppServerOperationLockV3AllowsOnlyApprovalNeverWorkspaceWrite(t *testing.T) {
	fixture := newCodexAppServerAttestationFixture(t)
	writable := fixture.lock.Clone()
	writable.Format = CodexAppServerOperationLockFormatV3
	writable.Version = CodexAppServerOperationLockVersionV3
	writable.ApprovalPolicy = CodexAppServerApprovalPolicyNever
	writable.SandboxMode = CodexAppServerSandboxModeWorkspaceWrite
	writable.SandboxPolicy = CodexAppServerSandboxPolicyWorkspaceWrite
	if err := writable.Validate(); err != nil {
		t.Fatalf("validate v3 workspace-write lock: %v", err)
	}

	invocation := codexAppServerInvocationFromLock(writable, *fixture.attestation.Record.AgentModel, CodexAppServerProductionReasoningEffort)
	if invocation.ApprovalPolicy != CodexAppServerApprovalPolicyNever || invocation.SandboxMode != CodexAppServerSandboxModeWorkspaceWrite || invocation.SandboxPolicy != CodexAppServerSandboxPolicyWorkspaceWrite || invocation.NetworkAccess {
		t.Fatalf("workspace-write invocation policy = %+v", invocation)
	}
	writableFixture := newCodexAppServerAttestationFixtureWithLock(t, func(lock *CodexAppServerOperationLock) {
		lock.Format = CodexAppServerOperationLockFormatV3
		lock.Version = CodexAppServerOperationLockVersionV3
		lock.ApprovalPolicy = CodexAppServerApprovalPolicyNever
		lock.SandboxMode = CodexAppServerSandboxModeWorkspaceWrite
		lock.SandboxPolicy = CodexAppServerSandboxPolicyWorkspaceWrite
	})
	attestor, err := NewCodexAppServerRuntimeAttestor(CodexAppServerRuntimeAttestorConfig{HarborFlowBuild: writableFixture.attestation.HarborFlowBuild})
	if err != nil {
		t.Fatal(err)
	}
	attested, err := attestor.AttestCodexAppServerOperation(context.Background(), writableFixture.attestation)
	if err != nil {
		t.Fatalf("attest v3 workspace-write runtime: %v", err)
	}
	if attested.ApprovalPolicy != CodexAppServerApprovalPolicyNever || attested.SandboxMode != CodexAppServerSandboxModeWorkspaceWrite || attested.SandboxPolicy != CodexAppServerSandboxPolicyWorkspaceWrite || attested.NetworkAccess {
		t.Fatalf("attested v3 workspace-write policy = %+v", attested)
	}

	for name, mutate := range map[string]func(*CodexAppServerOperationLock){
		"missing approval policy": func(lock *CodexAppServerOperationLock) { lock.ApprovalPolicy = "" },
		"interactive approval":    func(lock *CodexAppServerOperationLock) { lock.ApprovalPolicy = "on-request" },
		"mismatched read-only mode": func(lock *CodexAppServerOperationLock) {
			lock.SandboxMode = CodexAppServerSandboxModeReadOnly
		},
		"mismatched read-only policy": func(lock *CodexAppServerOperationLock) {
			lock.SandboxPolicy = CodexAppServerSandboxPolicyReadOnly
		},
		"network enabled": func(lock *CodexAppServerOperationLock) { lock.NetworkAccess = true },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := writable.Clone()
			mutate(&candidate)
			if err := candidate.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
				t.Fatalf("v3 lock validation error = %v, want invalid deployment lock", err)
			}
		})
	}

	legacy := fixture.lock.Clone()
	legacy.ApprovalPolicy = ""
	if err := legacy.Validate(); err != nil {
		t.Fatalf("historical v2 read-only lock no longer validates: %v", err)
	}
	legacyInvocation := codexAppServerInvocationFromLock(legacy, *fixture.attestation.Record.AgentModel, CodexAppServerProductionReasoningEffort)
	if legacyInvocation.ApprovalPolicy != CodexAppServerApprovalPolicyNever || legacyInvocation.SandboxMode != CodexAppServerSandboxModeReadOnly || legacyInvocation.SandboxPolicy != CodexAppServerSandboxPolicyReadOnly {
		t.Fatalf("historical v2 invocation policy = %+v", legacyInvocation)
	}
}

func TestCodexAppServerRuntimeAttestorProvesPinnedRuntimeWithExplicitSecretFreeEnvironment(t *testing.T) {
	fixture := newCodexAppServerAttestationFixture(t)
	// These values intentionally exist in the parent process. The fake Node
	// executable exits unsuccessfully if the attestation probes inherit either
	// one, proving that only the lock's explicit environment reaches probes.
	t.Setenv("OPENAI_API_KEY", "ambient-secret-must-not-reach-probe")
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "ambient-codex-home"))

	attestor, err := NewCodexAppServerRuntimeAttestor(CodexAppServerRuntimeAttestorConfig{HarborFlowBuild: fixture.attestation.HarborFlowBuild})
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := attestor.AttestCodexAppServerOperation(context.Background(), fixture.attestation)
	if err != nil {
		t.Fatalf("attest pinned Codex runtime: %v", err)
	}
	if invocation.JavaScriptLauncherPath != fixture.launcher || invocation.NodeExecutablePath != fixture.node || invocation.CodexHomeDirectory != fixture.home {
		t.Fatalf("invocation paths = %+v, want launcher/node/home from immutable lock", invocation)
	}
	if invocation.ModelID != CodexAppServerProductionModelID || invocation.ReasoningEffort != CodexAppServerProductionReasoningEffort || invocation.AgentID == "" || invocation.AgentVersion == "" || invocation.ModelVersion == "" {
		t.Fatalf("invocation agent configuration = %+v, want frozen production identity", invocation)
	}
	if invocation.CLIVersionOutput != "codex-cli 0.133.0" || invocation.ApprovalPolicy != CodexAppServerApprovalPolicyNever || invocation.SandboxMode != CodexAppServerSandboxModeReadOnly || invocation.SandboxPolicy != CodexAppServerSandboxPolicyReadOnly || invocation.NetworkAccess {
		t.Fatalf("invocation policy = %+v, want locked read-only/network-disabled Codex policy", invocation)
	}
	environment := invocation.Environment()
	if len(environment) != 2 || environment["CODEX_HOME"] != fixture.home || !strings.HasPrefix(environment["PATH"], filepath.Dir(fixture.node)+string(os.PathListSeparator)) {
		t.Fatalf("invocation environment = %#v, want only explicit CODEX_HOME and node-first PATH", environment)
	}
	if _, present := environment["OPENAI_API_KEY"]; present {
		t.Fatalf("invocation environment exposed a secret key: %#v", environment)
	}
	encoded, err := json.Marshal(invocation)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "ambient-secret-must-not-reach-probe") {
		t.Fatalf("secret-free invocation serialized an ambient secret: %s", encoded)
	}
	if err := attestor.AttestDeploymentOperation(context.Background(), fixture.attestation); err != nil {
		t.Fatalf("generic attestation boundary rejected exact Codex operation: %v", err)
	}
}

func TestReadBoundedShebangLineReadsOnlyTheFirstLine(t *testing.T) {
	line, err := readBoundedShebangLine(strings.NewReader("#!/usr/bin/env node\n"+strings.Repeat("x", 16*1024)), codexAppServerShebangLimit)
	if err != nil || string(line) != "#!/usr/bin/env node" {
		t.Fatalf("large launcher shebang = %q, %v", line, err)
	}
	line, err = readBoundedShebangLine(strings.NewReader(strings.Repeat("x", codexAppServerShebangLimit)+"\n"), codexAppServerShebangLimit)
	if err != nil || len(line) != codexAppServerShebangLimit {
		t.Fatalf("boundary shebang length = %d, %v", len(line), err)
	}
	for name, input := range map[string]string{
		"overlong":     strings.Repeat("x", codexAppServerShebangLimit+1) + "\n",
		"unterminated": "#!/usr/bin/env node",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readBoundedShebangLine(strings.NewReader(input), codexAppServerShebangLimit); err == nil {
				t.Fatal("readBoundedShebangLine unexpectedly succeeded")
			}
		})
	}
}

func TestCodexAppServerRuntimeAttestorFailsClosedOnDriftOrMissingTypedLock(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, fixture *codexAppServerAttestationFixture)
		want   error
	}{
		{
			name: "launcher content drift",
			mutate: func(t *testing.T, fixture *codexAppServerAttestationFixture) {
				writeCodexAppServerTestFile(t, fixture.launcher, "#!/usr/bin/env node\n// changed\n", 0o700)
			},
			want: ErrDeploymentOperationRuntimeAttestationFailed,
		},
		{
			name: "launcher shebang drift after matching content hash",
			mutate: func(t *testing.T, fixture *codexAppServerAttestationFixture) {
				contents := "#!/usr/bin/env node --unsafe\n// changed but rehashed\n"
				writeCodexAppServerTestFile(t, fixture.launcher, contents, 0o700)
				fixture.attestation.Record.CodexAppServer.JavaScriptLauncher.ContentSHA256 = workflowkit.SHA256Fingerprint([]byte(contents))
			},
			want: ErrDeploymentOperationRuntimeAttestationFailed,
		},
		{
			name: "node version output drift after matching content hash",
			mutate: func(t *testing.T, fixture *codexAppServerAttestationFixture) {
				contents := strings.Replace(fixture.nodeContents, "codex-cli 0.133.0", "codex-cli 0.133.1", 1)
				writeCodexAppServerTestFile(t, fixture.node, contents, 0o700)
				fixture.attestation.Record.CodexAppServer.NodeExecutable.ContentSHA256 = workflowkit.SHA256Fingerprint([]byte(contents))
			},
			want: ErrDeploymentOperationRuntimeAttestationFailed,
		},
		{
			name: "app server capability drift after matching content hash",
			mutate: func(t *testing.T, fixture *codexAppServerAttestationFixture) {
				contents := strings.Replace(fixture.nodeContents, "--listen --config", "--listen", 1)
				writeCodexAppServerTestFile(t, fixture.node, contents, 0o700)
				fixture.attestation.Record.CodexAppServer.NodeExecutable.ContentSHA256 = workflowkit.SHA256Fingerprint([]byte(contents))
			},
			want: ErrDeploymentOperationRuntimeAttestationFailed,
		},
		{
			name: "CODEX_HOME symlink",
			mutate: func(t *testing.T, fixture *codexAppServerAttestationFixture) {
				link := filepath.Join(t.TempDir(), "codex-home-link")
				if err := os.Symlink(fixture.home, link); err != nil {
					t.Skipf("create CODEX_HOME symlink: %v", err)
				}
				fixture.attestation.Record.CodexAppServer.CodexHomeDirectory = link
			},
			want: ErrDeploymentOperationRuntimeAttestationFailed,
		},
		{
			name: "missing typed lock",
			mutate: func(_ *testing.T, fixture *codexAppServerAttestationFixture) {
				fixture.attestation.Record.CodexAppServer = nil
			},
			want: ErrDeploymentOperationRuntimeAttestationUnavailable,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCodexAppServerAttestationFixture(t)
			test.mutate(t, fixture)
			attestor, err := NewCodexAppServerRuntimeAttestor(CodexAppServerRuntimeAttestorConfig{HarborFlowBuild: fixture.attestation.HarborFlowBuild})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := attestor.AttestCodexAppServerOperation(context.Background(), fixture.attestation); err == nil || !errors.Is(err, test.want) {
				t.Fatalf("drift error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestCodexAppServerLockRoundTripsThroughStrictOperationLockJSON(t *testing.T) {
	fixture := newCodexAppServerAttestationFixture(t)
	catalogDocument := operationCatalogLockCatalogForResolutions(t, fixture.attestation.Resolution)
	catalog, err := NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatal(err)
	}
	lock := runtimeAttestorLock(t, catalog, fixture.attestation.Record)
	canonical, err := lock.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseDeploymentOperationCatalogLockJSON(canonical)
	if err != nil {
		t.Fatalf("parse canonical Codex operation lock: %v", err)
	}
	if len(parsed.Operations) != 1 || parsed.Operations[0].CodexAppServer == nil || parsed.Operations[0].CodexAppServer.CLIVersionOutput != "codex-cli 0.133.0" {
		t.Fatalf("parsed Codex operation lock = %+v, want complete typed Codex extension", parsed)
	}
	unknownNested := []byte(strings.Replace(string(canonical), `"network_access":false`, `"network_access":false,"unexpected":true`, 1))
	if _, err := ParseDeploymentOperationCatalogLockJSON(unknownNested); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("unknown Codex lock field error = %v, want invalid deployment lock", err)
	}
}

type codexAppServerAttestationFixture struct {
	attestation  DeploymentOperationRuntimeAttestation
	lock         CodexAppServerOperationLock
	launcher     string
	node         string
	home         string
	nodeContents string
}

func newCodexAppServerAttestationFixture(t *testing.T) *codexAppServerAttestationFixture {
	return newCodexAppServerAttestationFixtureWithLock(t, nil)
}

func newCodexAppServerAttestationFixtureWithLock(t *testing.T, mutate func(*CodexAppServerOperationLock)) *codexAppServerAttestationFixture {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "codex-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	nodeDirectory := filepath.Join(root, "node-bin")
	if err := os.Mkdir(nodeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(nodeDirectory, "node")
	launcher := filepath.Join(root, "codex.js")
	launcherContents := "#!/usr/bin/env node\n" + strings.Repeat("// controlled Codex JavaScript launcher\n", 128)
	writeCodexAppServerTestFile(t, launcher, launcherContents, 0o700)
	// The pinned launcher reaches this fake Node through its strict env-based
	// shebang. It also rejects inherited credentials or an ambient CODEX_HOME.
	nodeContents := fmt.Sprintf(`#!/bin/sh
if [ "${OPENAI_API_KEY+x}" = x ]; then exit 91; fi
if [ "$CODEX_HOME" != %q ]; then exit 92; fi
case "$PATH" in
  %q:*) ;;
  *) exit 93 ;;
esac
if [ "$1" = "--version" ]; then printf 'v26.2.0\n'; exit 0; fi
if [ "$2" = "--version" ]; then printf 'codex-cli 0.133.0\n'; exit 0; fi
if [ "$2" = "app-server" ] && [ "$3" = "--help" ]; then printf '%%s\n' '--listen --config'; exit 0; fi
exit 94
`, home, nodeDirectory)
	writeCodexAppServerTestFile(t, node, nodeContents, 0o700)

	resolution := agentTurnRuntimeAttestationResolution(t)
	payload := resolution.Operation.Payload.(workflowadapter.AgentTurnOperationPayload)
	payload.AgentID = CodexAppServerProductionAgentID
	payload.ModelID = CodexAppServerProductionModelID
	payload.ReasoningEffort = CodexAppServerProductionReasoningEffort
	resolution.Operation.Payload = payload
	catalogDocument := operationCatalogLockCatalogForResolutions(t, resolution)
	catalog, err := NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatal(err)
	}
	codexLock := CodexAppServerOperationLock{
		Format:  CodexAppServerOperationLockFormat,
		Version: CodexAppServerOperationLockVersion,
		JavaScriptLauncher: LocalExecutableLock{
			CommandID: CodexAppServerJavaScriptLauncherCommandID, AbsolutePath: launcher, Version: "0.133.0",
			ContentSHA256: workflowkit.SHA256Fingerprint([]byte(launcherContents)),
		},
		NodeExecutable: LocalExecutableLock{
			CommandID: CodexAppServerNodeExecutableCommandID, AbsolutePath: node, Version: "v26.2.0",
			ContentSHA256: workflowkit.SHA256Fingerprint([]byte(nodeContents)),
		},
		CodexHomeDirectory: home,
		CLIVersionOutput:   "codex-cli 0.133.0",
		SandboxMode:        CodexAppServerSandboxModeReadOnly,
		SandboxPolicy:      CodexAppServerSandboxPolicyReadOnly,
		NetworkAccess:      false,
	}
	if mutate != nil {
		mutate(&codexLock)
	}
	record := operationCatalogLockRecord(t, catalogDocument.Operations[0])
	record.CodexAppServer = &codexLock
	lock := runtimeAttestorLock(t, catalog, record)
	verifier, err := NewDeploymentOperationCatalogLockResolver(catalog, lock)
	if err != nil {
		t.Fatal(err)
	}
	return &codexAppServerAttestationFixture{
		launcher: launcher, node: node, home: home, nodeContents: nodeContents, lock: codexLock.Clone(),
		attestation: DeploymentOperationRuntimeAttestation{
			CatalogReceipt: verifier.CatalogReceipt(), LockIdentity: verifier.LockIdentity(), HarborFlowBuild: verifier.HarborFlowBuild(),
			Record: record.Clone(), Resolution: resolution.Clone(),
		},
	}
}

func writeCodexAppServerTestFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}
