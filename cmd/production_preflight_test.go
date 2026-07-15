package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninjectedProductionBindingFailsLifecycleCLIBeforeOpeningStore(t *testing.T) {
	clearCodeEdgeProductionBuildBindingForTest(t)
	managedRoot := filepath.Join(t.TempDir(), "managed-control-plane")
	command := NewRootCommand()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--root", managedRoot, "task", "list"})

	err := command.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "build with scripts/build-codeedge-production.sh") {
		t.Fatalf("uninjected lifecycle CLI error = %v, want production binding rejection", err)
	}
	assertNoPreflightManagedRoot(t, managedRoot)
}

func TestUninjectedProductionBindingFailsLifecycleTUIBeforeOpeningStore(t *testing.T) {
	clearCodeEdgeProductionBuildBindingForTest(t)
	managedRoot := filepath.Join(t.TempDir(), "managed-control-plane")
	command := NewRootCommand()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--root", managedRoot, "tui"})

	err := command.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "build with scripts/build-codeedge-production.sh") {
		t.Fatalf("uninjected lifecycle TUI error = %v, want production binding rejection", err)
	}
	assertNoPreflightManagedRoot(t, managedRoot)
}

func clearCodeEdgeProductionBuildBindingForTest(t *testing.T) {
	t.Helper()
	originalModule := codeEdgeProductionBuildModule
	originalVersion := codeEdgeProductionBuildVersion
	originalCommit := codeEdgeProductionBuildCommit
	originalContent := codeEdgeProductionBuildContentSHA256
	originalCatalogReceipt := codeEdgeProductionBuildCatalogReceiptFingerprint
	originalLockID := codeEdgeProductionBuildLockID
	originalLockVersion := codeEdgeProductionBuildLockVersion
	originalLockFingerprint := codeEdgeProductionBuildLockFingerprint
	codeEdgeProductionBuildModule = ""
	codeEdgeProductionBuildVersion = ""
	codeEdgeProductionBuildCommit = ""
	codeEdgeProductionBuildContentSHA256 = ""
	codeEdgeProductionBuildCatalogReceiptFingerprint = ""
	codeEdgeProductionBuildLockID = ""
	codeEdgeProductionBuildLockVersion = ""
	codeEdgeProductionBuildLockFingerprint = ""
	t.Cleanup(func() {
		codeEdgeProductionBuildModule = originalModule
		codeEdgeProductionBuildVersion = originalVersion
		codeEdgeProductionBuildCommit = originalCommit
		codeEdgeProductionBuildContentSHA256 = originalContent
		codeEdgeProductionBuildCatalogReceiptFingerprint = originalCatalogReceipt
		codeEdgeProductionBuildLockID = originalLockID
		codeEdgeProductionBuildLockVersion = originalLockVersion
		codeEdgeProductionBuildLockFingerprint = originalLockFingerprint
	})
}

func assertNoPreflightManagedRoot(t *testing.T, managedRoot string) {
	t.Helper()
	if _, err := os.Lstat(managedRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed production preflight created managed root %q: %v", managedRoot, err)
	}
}
