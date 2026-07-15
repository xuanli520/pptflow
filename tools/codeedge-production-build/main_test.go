package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
)

func TestProductionBuildLDFlagsAcceptsLockedManifestAndEmitsReviewedBaseline(t *testing.T) {
	catalogPath := filepath.Join("..", "..", "deployments", "codeedge-phase1", "operation-catalog.v1.json")
	lockPath := filepath.Join("..", "..", "deployments", "codeedge-phase1", "operation-catalog.lock.json")
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := stageprovider.ParseDeploymentOperationCatalogLockJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	reviewedBaseline := lock.HarborFlowBuild.Commit
	manifest := string(lock.HarborFlowBuild.ContentSHA256)
	catalogReceiptFingerprint, err := lock.CatalogReceipt.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	lockFingerprint, err := lock.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	flags, err := productionBuildLDFlags(catalogPath, lockPath, manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"cmd.codeEdgeProductionBuildModule=github.com/purplevoid/harbor-factory",
		"cmd.codeEdgeProductionBuildVersion=" + lock.HarborFlowBuild.Version,
		"cmd.codeEdgeProductionBuildCommit=" + reviewedBaseline,
		"cmd.codeEdgeProductionBuildContentSHA256=" + manifest,
		"cmd.codeEdgeProductionBuildCatalogReceiptFingerprint=" + string(catalogReceiptFingerprint),
		"cmd.codeEdgeProductionBuildLockID=" + lock.LockID,
		"cmd.codeEdgeProductionBuildLockVersion=" + lock.LockVersion,
		"cmd.codeEdgeProductionBuildLockFingerprint=" + string(lockFingerprint),
	} {
		if !strings.Contains(flags, want) {
			t.Fatalf("ldflags omit %q", want)
		}
	}
	if _, err := productionBuildLDFlags(catalogPath, lockPath, "sha256:"+strings.Repeat("b", 64)); err == nil {
		t.Fatal("mismatched source manifest was accepted")
	}

	catalogRaw, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	driftedCatalogPath := filepath.Join(t.TempDir(), "operation-catalog.v1.json")
	driftedCatalog := strings.Replace(string(catalogRaw), `"catalog_version": "2026.07.14.1"`, `"catalog_version": "2026.07.14.drift"`, 1)
	if err := os.WriteFile(driftedCatalogPath, []byte(driftedCatalog), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := productionBuildLDFlags(driftedCatalogPath, lockPath, manifest); err == nil {
		t.Fatal("catalog/lock receipt drift was accepted")
	}
}
