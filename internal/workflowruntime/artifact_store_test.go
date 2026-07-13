package workflowruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestArtifactObjectStorePutDeduplicatesWithoutOverwrite(t *testing.T) {
	t.Parallel()
	store := newTestObjectStore(t)
	content := []byte("immutable artifact payload")

	first, err := store.PutBytes(context.Background(), content)
	if err != nil {
		t.Fatalf("put first object: %v", err)
	}
	path, err := store.ObjectPath(first)
	if err != nil {
		t.Fatalf("object path: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read first object: %v", err)
	}

	second, err := store.Put(context.Background(), bytes.NewReader(content))
	if err != nil {
		t.Fatalf("deduplicate object: %v", err)
	}
	if second != first {
		t.Fatalf("deduplicated reference = %#v, want %#v", second, first)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read deduplicated object: %v", err)
	}
	if !bytes.Equal(after, before) || !bytes.Equal(after, content) {
		t.Fatalf("deduplication changed immutable object: got %q, want %q", after, content)
	}
	if err := store.Verify(context.Background(), first); err != nil {
		t.Fatalf("verify deduplicated object: %v", err)
	}
}

func TestArtifactObjectStoreRefusesToOverwriteCorruptExistingObject(t *testing.T) {
	t.Parallel()
	store := newTestObjectStore(t)
	expected := []byte("expected immutable bytes")
	reference := ObjectRef{
		Digest:    workflowkit.SHA256Fingerprint(expected),
		SizeBytes: int64(len(expected)),
	}
	path, err := store.ObjectPath(reference)
	if err != nil {
		t.Fatalf("object path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create object layout: %v", err)
	}
	corrupt := []byte("different bytes already at the digest path")
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatalf("seed corrupt object: %v", err)
	}

	_, err = store.PutBytes(context.Background(), expected)
	if !errors.Is(err, ErrObjectCorrupt) {
		t.Fatalf("put over corrupt object error = %v, want ErrObjectCorrupt", err)
	}
	actual, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read seeded object: %v", readErr)
	}
	if !bytes.Equal(actual, corrupt) {
		t.Fatalf("put overwrote existing object: got %q, want %q", actual, corrupt)
	}
}

func TestArtifactObjectStoreIntegrityMissingAndCorrupt(t *testing.T) {
	t.Parallel()
	store := newTestObjectStore(t)
	missing := ObjectRef{Digest: workflowkit.SHA256Fingerprint([]byte("missing")), SizeBytes: int64(len("missing"))}
	if err := store.Verify(context.Background(), missing); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("verify missing object error = %v, want ErrObjectNotFound", err)
	}
	available, err := store.Exists(context.Background(), missing)
	if err != nil || available {
		t.Fatalf("missing exists = (%t, %v), want (false, nil)", available, err)
	}

	reference, err := store.PutBytes(context.Background(), []byte("verified bytes"))
	if err != nil {
		t.Fatalf("put object: %v", err)
	}
	path, err := store.ObjectPath(reference)
	if err != nil {
		t.Fatalf("object path: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("make object corruptible for test: %v", err)
	}
	if err := os.WriteFile(path, []byte("tampered bytes"), 0o644); err != nil {
		t.Fatalf("tamper object: %v", err)
	}
	if err := store.Verify(context.Background(), reference); !errors.Is(err, ErrObjectCorrupt) {
		t.Fatalf("verify corrupt object error = %v, want ErrObjectCorrupt", err)
	}
	if _, err := store.ReadAll(context.Background(), reference); !errors.Is(err, ErrObjectCorrupt) {
		t.Fatalf("read corrupt object error = %v, want ErrObjectCorrupt", err)
	}
	available, err = store.Exists(context.Background(), reference)
	if available || !errors.Is(err, ErrObjectCorrupt) {
		t.Fatalf("corrupt exists = (%t, %v), want (false, ErrObjectCorrupt)", available, err)
	}
}

func TestArtifactObjectStoreConcurrentPutIsAtomicAndIdempotent(t *testing.T) {
	t.Parallel()
	store := newTestObjectStore(t)
	content := bytes.Repeat([]byte("same immutable payload\n"), 64*1024)

	const writers = 24
	start := make(chan struct{})
	references := make(chan ObjectRef, writers)
	errorsByWriter := make(chan error, writers)
	var wait sync.WaitGroup
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			reference, err := store.PutBytes(context.Background(), content)
			if err != nil {
				errorsByWriter <- err
				return
			}
			references <- reference
		}()
	}
	close(start)
	wait.Wait()
	close(references)
	close(errorsByWriter)

	for err := range errorsByWriter {
		t.Errorf("concurrent put: %v", err)
	}
	var first ObjectRef
	for reference := range references {
		if first.Digest == "" {
			first = reference
			continue
		}
		if reference != first {
			t.Errorf("concurrent reference = %#v, want %#v", reference, first)
		}
	}
	if first.Digest == "" {
		t.Fatal("no concurrent writer returned an object reference")
	}
	if err := store.Verify(context.Background(), first); err != nil {
		t.Fatalf("verify concurrent object: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(store.Root(), ObjectAlgorithm))
	if err != nil {
		t.Fatalf("read object directory: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("object directory entries = %v, want only %s", names, strings.TrimPrefix(string(first.Digest), ObjectAlgorithm+":"))
	}
	if got := entries[0].Name(); got != strings.TrimPrefix(string(first.Digest), ObjectAlgorithm+":") {
		t.Fatalf("object filename = %q, want digest filename", got)
	}
}

func TestArtifactManifestCanonicalSerializationAndStateDerivation(t *testing.T) {
	t.Parallel()
	store := newTestObjectStore(t)
	payload, err := store.PutBytes(context.Background(), []byte("artifact payload"))
	if err != nil {
		t.Fatalf("put payload: %v", err)
	}
	inputDigest, err := workflowkit.FingerprintArtifactBindings([]workflowkit.ArtifactBinding{
		{Name: "alpha", ArtifactID: "input-a", ContentDigest: workflowkit.SHA256Fingerprint([]byte("a")), SchemaVersion: "v1"},
		{Name: "beta", ArtifactID: "input-b", ContentDigest: workflowkit.SHA256Fingerprint([]byte("b")), SchemaVersion: "v1"},
	})
	if err != nil {
		t.Fatalf("fingerprint bindings: %v", err)
	}
	artifact := workflowkit.ArtifactRef{
		ID:                  "artifact-1",
		ContentDigest:       payload.Digest,
		SchemaVersion:       "artifact.schema.v1",
		RunID:               "run-1",
		StageKey:            "quality",
		AttemptID:           "attempt-1",
		TurnOrdinal:         2,
		WorkflowFingerprint: workflowkit.SHA256Fingerprint([]byte("workflow")),
		SubjectRevisionID:   "revision-1",
		SubjectDigest:       workflowkit.SubjectDigest(workflowkit.SHA256Fingerprint([]byte("subject"))),
		InputBindings: []workflowkit.ArtifactBinding{
			{Name: "beta", ArtifactID: "input-b", ContentDigest: workflowkit.SHA256Fingerprint([]byte("b")), SchemaVersion: "v1"},
			{Name: "alpha", ArtifactID: "input-a", ContentDigest: workflowkit.SHA256Fingerprint([]byte("a")), SchemaVersion: "v1"},
		},
		InputFingerprint: inputDigest,
		ProducerVersion:  "producer.v1",
		CreatedAt:        time.Date(2026, 7, 13, 15, 30, 0, 0, time.FixedZone("UTC+8", 8*60*60)),
		State:            ArtifactActive,
	}
	manifest, err := NewArtifactManifest(artifact, payload)
	if err != nil {
		t.Fatalf("new manifest: %v", err)
	}
	if got := manifest.Artifact.InputBindings[0].Name; got != "alpha" {
		t.Fatalf("canonical input ordering starts with %q, want alpha", got)
	}
	firstJSON, err := manifest.MarshalCanonicalJSON()
	if err != nil {
		t.Fatalf("serialize manifest: %v", err)
	}
	secondJSON, err := manifest.MarshalCanonicalJSON()
	if err != nil {
		t.Fatalf("serialize manifest again: %v", err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("manifest serialization is not deterministic:\n%s\n%s", firstJSON, secondJSON)
	}
	if bytes.Contains(firstJSON, []byte("artifact payload")) {
		t.Fatalf("reference-friendly manifest embeds payload bytes: %s", firstJSON)
	}

	superseded, err := manifest.WithState(ArtifactSuperseded)
	if err != nil {
		t.Fatalf("derive superseded state: %v", err)
	}
	if manifest.Artifact.State != ArtifactActive || superseded.Artifact.State != ArtifactSuperseded {
		t.Fatalf("state derivation mutated original or did not derive projection: original=%s derived=%s", manifest.Artifact.State, superseded.Artifact.State)
	}
	manifestRef, err := store.PutManifest(context.Background(), superseded)
	if err != nil {
		t.Fatalf("put manifest: %v", err)
	}
	if err := manifestRef.Validate(); err != nil {
		t.Fatalf("validate manifest ref: %v", err)
	}
	stored, err := store.ReadAll(context.Background(), manifestRef.Manifest)
	if err != nil {
		t.Fatalf("read manifest object: %v", err)
	}
	if !bytes.Equal(stored, mustCanonicalJSON(t, superseded)) {
		t.Fatalf("stored manifest bytes differ from canonical serialization")
	}
	decoded, err := store.ReadManifest(context.Background(), manifestRef)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if decoded.Artifact.State != ArtifactSuperseded || decoded.Artifact.ID != superseded.Artifact.ID {
		t.Fatalf("decoded manifest = %#v, want superseded artifact %q", decoded, superseded.Artifact.ID)
	}
}

func TestArtifactObjectStoreRejectsUnsafeObjectPaths(t *testing.T) {
	t.Parallel()
	store := newTestObjectStore(t)
	unsafe := ObjectRef{Digest: "sha256:../not-a-digest", SizeBytes: 1}
	if _, err := store.ObjectPath(unsafe); !errors.Is(err, ErrInvalidObjectRef) {
		t.Fatalf("unsafe digest path error = %v, want ErrInvalidObjectRef", err)
	}
	reference, err := store.PutBytes(context.Background(), []byte("safe path target"))
	if err != nil {
		t.Fatalf("put path target: %v", err)
	}
	path, err := store.ObjectPath(reference)
	if err != nil {
		t.Fatalf("object path: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove object to replace with symlink: %v", err)
	}
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("outside object root"), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("filesystem does not permit symlink safety test: %v", err)
	}
	if err := store.Verify(context.Background(), reference); !errors.Is(err, ErrUnsafeObjectPath) {
		t.Fatalf("verify symlink object error = %v, want ErrUnsafeObjectPath", err)
	}
}

func newTestObjectStore(t *testing.T) *ArtifactObjectStore {
	t.Helper()
	store, err := NewArtifactObjectStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatalf("new object store: %v", err)
	}
	return store
}

func mustCanonicalJSON(t *testing.T, manifest ArtifactManifest) []byte {
	t.Helper()
	encoded, err := manifest.MarshalCanonicalJSON()
	if err != nil {
		t.Fatalf("canonical manifest JSON: %v", err)
	}
	return encoded
}
