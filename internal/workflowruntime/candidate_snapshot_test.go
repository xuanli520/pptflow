package workflowruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestCandidateSnapshotCaptureStoresOnlyImmutableObjects(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	writeCandidateFile(t, workspace, "solution/solve.sh", "#!/bin/sh\necho fixed\n")
	writeCandidateFile(t, workspace, "tests/test.sh", "#!/bin/sh\nexit 0\n")
	objects, err := NewArtifactObjectStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	capturer, err := NewCandidateSnapshotCapturer(objects, 1024)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := capturer.Capture(context.Background(), workspace, []CandidateFileCaptureSpec{
		{Path: "tests/test.sh", SchemaVersion: "task-file/v1"},
		{Path: "solution/solve.sh", SchemaVersion: "task-file/v1"},
	})
	if err != nil {
		t.Fatalf("capture candidate snapshot: %v", err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("validate candidate snapshot: %v", err)
	}
	if len(snapshot.Files) != 2 || snapshot.Files[0].Path != "solution/solve.sh" {
		t.Fatalf("captured manifest = %#v, want sorted two-file snapshot", snapshot.Files)
	}
	for _, file := range snapshot.Files {
		object := ObjectRef{Digest: file.ContentDigest, SizeBytes: file.SizeBytes}
		if err := objects.Verify(context.Background(), object); err != nil {
			t.Fatalf("captured object %q unavailable: %v", file.Path, err)
		}
	}
	entries, err := os.ReadDir(objects.Root())
	if err != nil || len(entries) != 1 || entries[0].Name() != ObjectAlgorithm {
		t.Fatalf("object root entries = %#v, %v; want only %s", entries, err, ObjectAlgorithm)
	}
}

func TestCandidateSnapshotCaptureRejectsSymlinkAndReplacementSurfaces(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	writeCandidateFile(t, workspace, "solution/solve.sh", "safe")
	objects, err := NewArtifactObjectStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	capturer, err := NewCandidateSnapshotCapturer(objects, 16)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capturer.Capture(context.Background(), workspace, []CandidateFileCaptureSpec{{Path: "solution/solve.sh", SchemaVersion: "task-file/v1"}}); err != nil {
		t.Fatalf("capture safe file: %v", err)
	}

	unsafe := filepath.Join(workspace, "solution", "unsafe.sh")
	if err := os.Symlink("solve.sh", unsafe); err != nil {
		t.Skipf("create symlink fixture: %v", err)
	}
	if _, err := capturer.Capture(context.Background(), workspace, []CandidateFileCaptureSpec{{Path: "solution/unsafe.sh", SchemaVersion: "task-file/v1"}}); !errors.Is(err, ErrInvalidCandidateCapture) {
		t.Fatalf("symlink capture error = %v, want ErrInvalidCandidateCapture", err)
	}
	if _, err := capturer.Capture(context.Background(), workspace, []CandidateFileCaptureSpec{{Path: "solution/solve.sh", SchemaVersion: "task-file/v1"}, {Path: "solution/solve.sh", SchemaVersion: "task-file/v1"}}); !errors.Is(err, ErrInvalidCandidateCapture) {
		t.Fatalf("duplicate capture specification error = %v, want ErrInvalidCandidateCapture", err)
	}
	if _, err := capturer.Capture(context.Background(), workspace, []CandidateFileCaptureSpec{{Path: "solution/solve.sh", SchemaVersion: "task-file/v1"}, {Path: "../../escape", SchemaVersion: "task-file/v1"}}); !errors.Is(err, workflowkit.ErrInvalidCandidateSnapshot) {
		t.Fatalf("unsafe capture path error = %v, want ErrInvalidCandidateSnapshot", err)
	}
}

func writeCandidateFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}
