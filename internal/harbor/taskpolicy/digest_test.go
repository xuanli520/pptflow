package taskpolicy

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestComputeManagedTaskDigestV2HashesCanonicalLengthPrefixedManifest(t *testing.T) {
	root := writeCanonicalSnapshot(t, "docker")
	if err := os.WriteFile(filepath.Join(root, "instruction.md"), []byte("first line\r\nsecond\x00line\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ComputeManagedTaskDigestV2(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, TaskDigestV2Prefix) {
		t.Fatalf("digest prefix = %q, want %q", got, TaskDigestV2Prefix)
	}
	want := expectedV2Digest(t, root)
	if got != want {
		t.Fatalf("canonical V2 digest = %q, want %q", got, want)
	}
	withSurroundingWhitespace, err := ComputeManagedTaskDigestV2(" " + root + " ")
	if err != nil || withSurroundingWhitespace != got {
		t.Fatalf("whitespace-trimmed root digest = (%q, %v), want (%q, nil)", withSurroundingWhitespace, err, got)
	}

	if err := os.WriteFile(filepath.Join(root, "instruction.md"), []byte("first line\nsecond\x00line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := ComputeManagedTaskDigestV2(root)
	if err != nil {
		t.Fatal(err)
	}
	if changed == got {
		t.Fatal("V2 digest normalized raw bytes that differ only by line ending")
	}
}

func TestComputeManagedTaskDigestV2IgnoresSourcePathModeAndTimestamp(t *testing.T) {
	left := writeCanonicalSnapshot(t, "docker")
	right := writeCanonicalSnapshot(t, "docker")
	for _, root := range []string{left, right} {
		for _, file := range CanonicalFiles() {
			path := filepath.Join(root, filepath.FromSlash(file.Path))
			if _, err := os.Stat(path); os.IsNotExist(err) {
				continue
			} else if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, time.Unix(1, 0), time.Unix(2, 0)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Chmod(filepath.Join(right, "solution", "solve.sh"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(right, "tests", "test.sh"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(right, "instruction.md"), time.Unix(10_000, 0), time.Unix(20_000, 0)); err != nil {
		t.Fatal(err)
	}

	leftDigest, err := ComputeManagedTaskDigestV2(left)
	if err != nil {
		t.Fatal(err)
	}
	rightDigest, err := ComputeManagedTaskDigestV2(right)
	if err != nil {
		t.Fatal(err)
	}
	if leftDigest != rightDigest {
		t.Fatalf("identical bytes at different roots/source modes/timestamps have different V2 digests:\nleft  %s\nright %s", leftDigest, rightDigest)
	}
}

func TestValidateManagedSnapshotV2RequiresCanonicalFileSet(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(t *testing.T, root string)
		wantDetail string
	}{
		{
			name: "missing core file",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Remove(filepath.Join(root, "task.toml")); err != nil {
					t.Fatal(err)
				}
			},
			wantDetail: "missing required file: task.toml",
		},
		{
			name: "no environment file",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Remove(filepath.Join(root, "environment", "Dockerfile")); err != nil {
					t.Fatal(err)
				}
			},
			wantDetail: "at least one environment file is required",
		},
		{
			name: "extra file",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("not canonical\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantDetail: "unexpected file: README.md",
		},
		{
			name: "extra directory",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(root, "scratch"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantDetail: "unexpected directory: scratch",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeCanonicalSnapshot(t, "docker")
			test.mutate(t, root)
			assertSnapshotValidationFailure(t, root, test.wantDetail)
		})
	}
}

func TestValidateManagedSnapshotV2RejectsSymlink(t *testing.T) {
	root := writeCanonicalSnapshot(t, "docker")
	path := filepath.Join(root, "instruction.md")
	external := filepath.Join(t.TempDir(), "instruction.md")
	if err := os.WriteFile(external, []byte("external\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, path); err != nil {
		t.Skipf("symlinks are unavailable in this test environment: %v", err)
	}
	assertSnapshotValidationFailure(t, root, "symlink is not allowed: instruction.md")
}

func TestValidateManagedSnapshotV2AllowsEitherEnvironmentFile(t *testing.T) {
	for _, environment := range []string{"docker", "compose", "both"} {
		t.Run(environment, func(t *testing.T) {
			root := writeCanonicalSnapshot(t, environment)
			if err := ValidateManagedSnapshotV2(root); err != nil {
				t.Fatalf("%s environment snapshot rejected: %v", environment, err)
			}
			if _, err := ComputeManagedTaskDigestV2(root); err != nil {
				t.Fatalf("%s environment digest failed: %v", environment, err)
			}
		})
	}
}

func TestTaskDigestVersionsAreIsolated(t *testing.T) {
	root := writeCanonicalSnapshot(t, "docker")
	v2, err := ComputeManagedTaskDigestV2(root)
	if err != nil {
		t.Fatal(err)
	}
	v1 := LegacyTaskDigestPrefix + strings.Repeat("a", sha256.Size*2)

	for _, test := range []struct {
		name  string
		value string
		want  TaskDigestVersion
	}{
		{name: "legacy", value: v1, want: TaskDigestLegacyV1},
		{name: "v2", value: v2, want: TaskDigestV2},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ClassifyTaskDigest(test.value)
			if err != nil || got != test.want {
				t.Fatalf("ClassifyTaskDigest(%q) = (%q, %v), want (%q, nil)", test.value, got, err, test.want)
			}
		})
	}
	if !IsLegacyV1TaskDigest(v1) || IsV2TaskDigest(v1) {
		t.Fatalf("legacy digest was not isolated: %q", v1)
	}
	if !IsV2TaskDigest(v2) || IsLegacyV1TaskDigest(v2) {
		t.Fatalf("V2 digest was not isolated: %q", v2)
	}
	if err := ValidateV2TaskDigest(v1); err == nil {
		t.Fatal("legacy evidence was accepted at a V2 revision boundary")
	}
	if err := ValidateV2TaskDigest(v2); err != nil {
		t.Fatalf("V2 evidence rejected at V2 revision boundary: %v", err)
	}
	if err := ValidateV2TaskDigest(" " + v2); err == nil {
		t.Fatal("non-canonical whitespace-wrapped V2 evidence was accepted")
	}
	if EqualTaskDigests(v1, v2) {
		t.Fatal("cross-generation V1/V2 evidence compared equal")
	}
	if _, err := ClassifyTaskDigest(TaskDigestV2Prefix + "not-a-digest"); err == nil {
		t.Fatal("malformed V2 digest was accepted")
	}
}

func assertSnapshotValidationFailure(t *testing.T, root, wantDetail string) {
	t.Helper()
	err := ValidateManagedSnapshotV2(root)
	if err == nil {
		t.Fatalf("ValidateManagedSnapshotV2(%q) unexpectedly succeeded", root)
	}
	var validation *SnapshotValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("validation error type = %T (%v), want *SnapshotValidationError", err, err)
	}
	if !strings.Contains(err.Error(), wantDetail) {
		t.Fatalf("validation error = %q, want detail %q", err, wantDetail)
	}
	if _, err := ComputeManagedTaskDigestV2(root); err == nil {
		t.Fatalf("ComputeManagedTaskDigestV2(%q) unexpectedly accepted invalid snapshot", root)
	}
}

func writeCanonicalSnapshot(t *testing.T, environment string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string][]byte{
		"instruction.md":    []byte("Fix the regression.\n"),
		"task.toml":         []byte("schema_version = \"1.3\"\n"),
		"tests_analysis.md": []byte("Visible verifier analysis.\n"),
		"solution/solve.sh": []byte("#!/bin/sh\nexit 0\n"),
		"tests/test.sh":     []byte("#!/bin/sh\nexit 1\n"),
	}
	switch environment {
	case "docker":
		files["environment/Dockerfile"] = []byte("FROM alpine\n")
	case "compose":
		files["environment/docker-compose.yaml"] = []byte("services: {}\n")
	case "both":
		files["environment/Dockerfile"] = []byte("FROM alpine\n")
		files["environment/docker-compose.yaml"] = []byte("services: {}\n")
	default:
		t.Fatalf("unknown environment fixture %q", environment)
	}
	for rel, raw := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func expectedV2Digest(t *testing.T, root string) string {
	t.Helper()
	records := []struct {
		path string
		mode os.FileMode
	}{
		{path: "instruction.md", mode: 0o644},
		{path: "task.toml", mode: 0o644},
		{path: "tests_analysis.md", mode: 0o644},
		{path: "environment/Dockerfile", mode: 0o644},
		{path: "solution/solve.sh", mode: 0o755},
		{path: "tests/test.sh", mode: 0o755},
	}
	sort.Slice(records, func(i, j int) bool { return records[i].path < records[j].path })
	hash := sha256.New()
	writeExpectedField(t, hash, []byte(canonicalManifestV2Domain))
	writeExpectedField(t, hash, expectedUint64(uint64(len(records))))
	for _, record := range records {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(record.path)))
		if err != nil {
			t.Fatal(err)
		}
		content := sha256.Sum256(raw)
		writeExpectedField(t, hash, []byte(record.path))
		writeExpectedField(t, hash, expectedUint32(uint32(record.mode)))
		writeExpectedField(t, hash, expectedUint64(uint64(len(raw))))
		writeExpectedField(t, hash, content[:])
	}
	return TaskDigestV2Prefix + hex.EncodeToString(hash.Sum(nil))
}

func writeExpectedField(t *testing.T, writer io.Writer, value []byte) {
	t.Helper()
	if _, err := writer.Write(expectedUint64(uint64(len(value)))); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(value); err != nil {
		t.Fatal(err)
	}
}

func expectedUint32(value uint32) []byte {
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], value)
	return out[:]
}

func expectedUint64(value uint64) []byte {
	var out [8]byte
	binary.BigEndian.PutUint64(out[:], value)
	return out[:]
}
