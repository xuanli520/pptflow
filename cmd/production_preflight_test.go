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
	clearStandardAuthoringProductionBuildBindingForTest(t)
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
	clearStandardAuthoringProductionBuildBindingForTest(t)
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

func clearStandardAuthoringProductionBuildBindingForTest(t *testing.T) {
	t.Helper()
	originalModule := standardAuthoringProductionBuildModule
	originalVersion := standardAuthoringProductionBuildVersion
	originalCommit := standardAuthoringProductionBuildCommit
	originalContent := standardAuthoringProductionBuildContentSHA256
	originalCatalogReceipt := standardAuthoringProductionBuildCatalogReceiptFingerprint
	originalLockID := standardAuthoringProductionBuildLockID
	originalLockVersion := standardAuthoringProductionBuildLockVersion
	originalLockFingerprint := standardAuthoringProductionBuildLockFingerprint
	standardAuthoringProductionBuildModule = ""
	standardAuthoringProductionBuildVersion = ""
	standardAuthoringProductionBuildCommit = ""
	standardAuthoringProductionBuildContentSHA256 = ""
	standardAuthoringProductionBuildCatalogReceiptFingerprint = ""
	standardAuthoringProductionBuildLockID = ""
	standardAuthoringProductionBuildLockVersion = ""
	standardAuthoringProductionBuildLockFingerprint = ""
	t.Cleanup(func() {
		standardAuthoringProductionBuildModule = originalModule
		standardAuthoringProductionBuildVersion = originalVersion
		standardAuthoringProductionBuildCommit = originalCommit
		standardAuthoringProductionBuildContentSHA256 = originalContent
		standardAuthoringProductionBuildCatalogReceiptFingerprint = originalCatalogReceipt
		standardAuthoringProductionBuildLockID = originalLockID
		standardAuthoringProductionBuildLockVersion = originalLockVersion
		standardAuthoringProductionBuildLockFingerprint = originalLockFingerprint
	})
}

func assertNoPreflightManagedRoot(t *testing.T, managedRoot string) {
	t.Helper()
	if _, err := os.Lstat(managedRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed production preflight created managed root %q: %v", managedRoot, err)
	}
}
