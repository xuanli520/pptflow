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

func TestNewLocalFilesystemRuntimeAttestorRejectsInvalidBuildIdentity(t *testing.T) {
	_, err := NewLocalFilesystemRuntimeAttestor(LocalFilesystemRuntimeAttestorConfig{})
	if err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationFailed) || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("new attestor error = %v, want failed invalid lock error", err)
	}
}

func TestLocalFilesystemRuntimeAttestorAttestsExactBuildAndLockedRegularFile(t *testing.T) {
	contents := []byte("#!/bin/sh\necho controlled-stage\n")
	path := writeRuntimeAttestorFile(t, "harbor-stage", contents)
	attestation := localFilesystemRuntimeAttestation(t, path, contents)
	attestor, err := NewLocalFilesystemRuntimeAttestor(LocalFilesystemRuntimeAttestorConfig{HarborFlowBuild: attestation.HarborFlowBuild})
	if err != nil {
		t.Fatalf("construct local filesystem runtime attestor: %v", err)
	}
	if got := attestor.HarborFlowBuild(); got != attestation.HarborFlowBuild {
		t.Fatalf("configured Harbor Flow build = %+v, want %+v", got, attestation.HarborFlowBuild)
	}
	if err := attestor.AttestDeploymentOperation(context.Background(), attestation); err != nil {
		t.Fatalf("attest exact locked local executable: %v", err)
	}
}

func TestLocalFilesystemRuntimeAttestorRejectsBuildAndFileDrift(t *testing.T) {
	contents := []byte("controlled binary v1")
	path := writeRuntimeAttestorFile(t, "harbor-stage", contents)
	attestation := localFilesystemRuntimeAttestation(t, path, contents)

	t.Run("Harbor Flow build identity drift", func(t *testing.T) {
		driftedBuild := attestation.HarborFlowBuild
		driftedBuild.ContentSHA256 = workflowkit.SHA256Fingerprint([]byte("different build"))
		attestor, err := NewLocalFilesystemRuntimeAttestor(LocalFilesystemRuntimeAttestorConfig{HarborFlowBuild: driftedBuild})
		if err != nil {
			t.Fatal(err)
		}
		if err := attestor.AttestDeploymentOperation(context.Background(), attestation); err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationFailed) {
			t.Fatalf("build drift error = %v, want runtime attestation failed", err)
		}
	})

	t.Run("locked executable content drift", func(t *testing.T) {
		if err := os.WriteFile(path, []byte("controlled binary v2"), 0o700); err != nil {
			t.Fatal(err)
		}
		attestor, err := NewLocalFilesystemRuntimeAttestor(LocalFilesystemRuntimeAttestorConfig{HarborFlowBuild: attestation.HarborFlowBuild})
		if err != nil {
			t.Fatal(err)
		}
		if err := attestor.AttestDeploymentOperation(context.Background(), attestation); err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationFailed) {
			t.Fatalf("content drift error = %v, want runtime attestation failed", err)
		}
	})
}

func TestLocalFilesystemRuntimeAttestorRejectsSymbolicLinksAndDirectories(t *testing.T) {
	contents := []byte("controlled binary")
	target := writeRuntimeAttestorFile(t, "real-harbor-stage", contents)

	t.Run("symbolic link", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "harbor-stage-link")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("create symbolic link: %v", err)
		}
		attestation := localFilesystemRuntimeAttestation(t, link, contents)
		attestor, err := NewLocalFilesystemRuntimeAttestor(LocalFilesystemRuntimeAttestorConfig{HarborFlowBuild: attestation.HarborFlowBuild})
		if err != nil {
			t.Fatal(err)
		}
		if err := attestor.AttestDeploymentOperation(context.Background(), attestation); err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationFailed) {
			t.Fatalf("symbolic link error = %v, want runtime attestation failed", err)
		}
	})

	t.Run("symbolic link parent", func(t *testing.T) {
		parent := t.TempDir()
		linkedParent := filepath.Join(parent, "current")
		if err := os.Symlink(filepath.Dir(target), linkedParent); err != nil {
			t.Skipf("create parent symbolic link: %v", err)
		}
		attestation := localFilesystemRuntimeAttestation(t, filepath.Join(linkedParent, filepath.Base(target)), contents)
		attestor, err := NewLocalFilesystemRuntimeAttestor(LocalFilesystemRuntimeAttestorConfig{HarborFlowBuild: attestation.HarborFlowBuild})
		if err != nil {
			t.Fatal(err)
		}
		if err := attestor.AttestDeploymentOperation(context.Background(), attestation); err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationFailed) {
			t.Fatalf("parent symbolic link error = %v, want runtime attestation failed", err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		directory := t.TempDir()
		attestation := localFilesystemRuntimeAttestation(t, directory, contents)
		attestor, err := NewLocalFilesystemRuntimeAttestor(LocalFilesystemRuntimeAttestorConfig{HarborFlowBuild: attestation.HarborFlowBuild})
		if err != nil {
			t.Fatal(err)
		}
		if err := attestor.AttestDeploymentOperation(context.Background(), attestation); err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationFailed) {
			t.Fatalf("directory error = %v, want runtime attestation failed", err)
		}
	})
}

func TestLocalFilesystemRuntimeAttestorFailsClosedForNonLocalOperations(t *testing.T) {
	cases := []struct {
		name        string
		attestation DeploymentOperationRuntimeAttestation
	}{
		{
			name:        "container command",
			attestation: operationCatalogLockRuntimeAttestation(t, operationCatalogLockTestResolution(t, workflowadapter.HarborRunQwen)),
		},
		{
			name:        "agent turn",
			attestation: operationCatalogLockRuntimeAttestation(t, agentTurnRuntimeAttestationResolution(t)),
		},
		{
			name:        "durable review",
			attestation: operationCatalogLockRuntimeAttestation(t, operationCatalogLockTestResolution(t, workflowadapter.ResultReview)),
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			attestor, err := NewLocalFilesystemRuntimeAttestor(LocalFilesystemRuntimeAttestorConfig{HarborFlowBuild: test.attestation.HarborFlowBuild})
			if err != nil {
				t.Fatal(err)
			}
			err = attestor.AttestDeploymentOperation(context.Background(), test.attestation)
			if err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationUnavailable) {
				t.Fatalf("non-local attestation error = %v, want unavailable", err)
			}
		})
	}
}

func TestLocalFilesystemRuntimeAttestorRejectsCanceledContext(t *testing.T) {
	contents := []byte("controlled binary")
	path := writeRuntimeAttestorFile(t, "harbor-stage", contents)
	attestation := localFilesystemRuntimeAttestation(t, path, contents)
	attestor, err := NewLocalFilesystemRuntimeAttestor(LocalFilesystemRuntimeAttestorConfig{HarborFlowBuild: attestation.HarborFlowBuild})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = attestor.AttestDeploymentOperation(ctx, attestation)
	if err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationFailed) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context error = %v, want failed canceled context", err)
	}
}

func writeRuntimeAttestorFile(t *testing.T, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func localFilesystemRuntimeAttestation(t *testing.T, path string, contents []byte) DeploymentOperationRuntimeAttestation {
	t.Helper()
	resolution := operationCatalogLockTestResolution(t, workflowadapter.RepoPrepare)
	catalogDocument := operationCatalogLockCatalogForResolutions(t, resolution)
	catalog, err := NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatal(err)
	}
	record := operationCatalogLockRecord(t, catalogDocument.Operations[0])
	record.LocalExecutable.AbsolutePath = path
	record.LocalExecutable.ContentSHA256 = workflowkit.SHA256Fingerprint(contents)
	lock := runtimeAttestorLock(t, catalog, record)
	verifier, err := NewDeploymentOperationCatalogLockResolver(catalog, lock)
	if err != nil {
		t.Fatal(err)
	}
	return DeploymentOperationRuntimeAttestation{
		CatalogReceipt:  verifier.CatalogReceipt(),
		LockIdentity:    verifier.LockIdentity(),
		HarborFlowBuild: verifier.HarborFlowBuild(),
		Record:          record.Clone(),
		Resolution:      resolution.Clone(),
	}
}

func operationCatalogLockRuntimeAttestation(t *testing.T, resolution workflowadapter.StageOperationResolution) DeploymentOperationRuntimeAttestation {
	t.Helper()
	catalogDocument := operationCatalogLockCatalogForResolutions(t, resolution)
	catalog, err := NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatal(err)
	}
	record := operationCatalogLockRecord(t, catalogDocument.Operations[0])
	lock := runtimeAttestorLock(t, catalog, record)
	verifier, err := NewDeploymentOperationCatalogLockResolver(catalog, lock)
	if err != nil {
		t.Fatal(err)
	}
	return DeploymentOperationRuntimeAttestation{
		CatalogReceipt:  verifier.CatalogReceipt(),
		LockIdentity:    verifier.LockIdentity(),
		HarborFlowBuild: verifier.HarborFlowBuild(),
		Record:          record.Clone(),
		Resolution:      resolution.Clone(),
	}
}

func agentTurnRuntimeAttestationResolution(t *testing.T) workflowadapter.StageOperationResolution {
	t.Helper()
	resolution := operationCatalogLockTestResolution(t, workflowadapter.RepoPrepare)
	resolution.Operation.Payload = workflowadapter.AgentTurnOperationPayload{
		AgentID: "codeedge-agent", ModelID: "codeedge-model", ReasoningEffort: workflowadapter.AgentReasoningEffortHigh, MaxTurns: 4,
	}
	return resolution
}

func runtimeAttestorLock(t *testing.T, catalog *DeploymentOperationCatalogResolver, record DeploymentOperationCatalogLockRecord) DeploymentOperationCatalogLock {
	t.Helper()
	return DeploymentOperationCatalogLock{
		Format:          DeploymentOperationCatalogLockFormat,
		Version:         DeploymentOperationCatalogLockVersion,
		LockID:          "local-filesystem-runtime-attestor-test",
		LockVersion:     "test-v1",
		CatalogReceipt:  catalog.Receipt(),
		HarborFlowBuild: runtimeAttestorBuildIdentity(),
		Operations:      []DeploymentOperationCatalogLockRecord{record.Clone()},
	}
}

func runtimeAttestorBuildIdentity() HarborFlowBuildIdentity {
	return HarborFlowBuildIdentity{
		Module:        "github.com/purplevoid/harbor-factory",
		Version:       "v2.0.0",
		Commit:        strings.Repeat("b", 40),
		ContentSHA256: workflowkit.SHA256Fingerprint([]byte("local-filesystem-runtime-attestor-build")),
	}
}
