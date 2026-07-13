package harborrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyDigestRemainsCompatibleAndCannotBindV2Revision(t *testing.T) {
	taskDir := writeHarborRunTask(t)
	legacy, err := ComputeTaskDigest(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	explicitLegacy, err := ComputeTaskDigestV1(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	if legacy != explicitLegacy {
		t.Fatalf("ComputeTaskDigest changed V1 compatibility: got %q, want %q", legacy, explicitLegacy)
	}
	if !strings.HasPrefix(legacy, "sha256:") {
		t.Fatalf("legacy digest prefix = %q, want sha256:<hex>", legacy)
	}

	v2, err := ComputeManagedTaskDigestV2(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v2, "harbor.task.v2:sha256:") {
		t.Fatalf("V2 digest prefix = %q", v2)
	}
	if legacy == v2 || EqualTaskDigests(legacy, v2) {
		t.Fatalf("V1 and V2 evidence were not isolated: v1=%q v2=%q", legacy, v2)
	}
	if err := ValidateV2TaskDigest(legacy); err == nil {
		t.Fatal("legacy V1 evidence was accepted for V2 revision binding")
	}
	if err := ValidateV2TaskDigest(v2); err != nil {
		t.Fatalf("V2 evidence rejected: %v", err)
	}

	if err := os.WriteFile(filepath.Join(taskDir, "legacy-report.json"), []byte("legacy sidecar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyWithSidecar, err := ComputeTaskDigest(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	if legacyWithSidecar != legacy {
		t.Fatalf("legacy digest behavior changed after non-V1-root sidecar: got %q, want %q", legacyWithSidecar, legacy)
	}
	if _, err := ComputeManagedTaskDigestV2(taskDir); err == nil {
		t.Fatal("V2 digest accepted a legacy sidecar outside the strict snapshot policy")
	}
}
