package workflowruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

var (
	// ErrInvalidObjectStore marks an unusable object-store configuration.
	ErrInvalidObjectStore = errors.New("workflowruntime: invalid artifact object store")
	// ErrObjectNotFound marks a missing immutable content object.
	ErrObjectNotFound = errors.New("workflowruntime: artifact object not found")
	// ErrObjectCorrupt marks a present object that does not match its immutable
	// reference. A corrupt object is never silently replaced.
	ErrObjectCorrupt = errors.New("workflowruntime: artifact object is corrupt")
	// ErrUnsafeObjectPath marks a symlink, non-regular file, or layout entry
	// that cannot safely participate in the immutable object store.
	ErrUnsafeObjectPath = errors.New("workflowruntime: unsafe artifact object path")
)

// ArtifactObjectStore persists immutable content-addressed payloads below:
//
//	<root>/sha256/<lowercase-hex-digest>
//
// The root is the objects directory from the application layout, for example
// .harbor-factory/objects. Content publication uses a temporary file and an
// atomic hard-link operation, which deliberately never replaces an existing
// digest path.
type ArtifactObjectStore struct {
	root string
}

// NewArtifactObjectStore validates a root without creating it. Object layout
// directories are created lazily by Put, while read operations report missing
// layout as a missing object.
func NewArtifactObjectStore(root string) (*ArtifactObjectStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("%w: root is required", ErrInvalidObjectStore)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve root: %v", ErrInvalidObjectStore, err)
	}
	if info, err := os.Lstat(absRoot); err == nil {
		if err := validateDirectory(absRoot, info); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: inspect root: %v", ErrInvalidObjectStore, err)
	}
	return &ArtifactObjectStore{root: absRoot}, nil
}

// NewObjectStore is a concise alias for NewArtifactObjectStore.
func NewObjectStore(root string) (*ArtifactObjectStore, error) {
	return NewArtifactObjectStore(root)
}

// Root returns the absolute objects directory configured for this store.
func (store *ArtifactObjectStore) Root() string {
	if store == nil {
		return ""
	}
	return store.root
}

// ObjectPath returns the only safe object path for reference. It does not
// assert that the object exists or create any directories.
func (store *ArtifactObjectStore) ObjectPath(reference ObjectRef) (string, error) {
	if err := store.validate(); err != nil {
		return "", err
	}
	hexDigest, err := objectDigestHex(reference)
	if err != nil {
		return "", err
	}
	algorithmRoot := filepath.Join(store.root, ObjectAlgorithm)
	path := filepath.Join(algorithmRoot, hexDigest)
	relative, err := filepath.Rel(algorithmRoot, path)
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.Base(relative) != hexDigest {
		return "", fmt.Errorf("%w: object digest resolves outside SHA-256 layout", ErrUnsafeObjectPath)
	}
	return path, nil
}

// Put stores bytes under their SHA-256 identity. Equal concurrent writes are
// idempotently deduplicated after the existing object is revalidated. A
// present object with a wrong hash or size is reported as corrupt and is never
// overwritten.
func (store *ArtifactObjectStore) Put(ctx context.Context, source io.Reader) (ObjectRef, error) {
	if err := validateContext(ctx); err != nil {
		return ObjectRef{}, err
	}
	if source == nil {
		return ObjectRef{}, fmt.Errorf("%w: content reader is required", ErrInvalidObjectStore)
	}
	if err := store.ensureLayout(); err != nil {
		return ObjectRef{}, err
	}

	algorithmRoot := filepath.Join(store.root, ObjectAlgorithm)
	temporary, err := os.CreateTemp(algorithmRoot, ".object-*")
	if err != nil {
		return ObjectRef{}, fmt.Errorf("create temporary object: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()

	size, digest, copyErr := copyWithDigest(ctx, source, temporary)
	if copyErr != nil {
		_ = temporary.Close()
		return ObjectRef{}, copyErr
	}
	if err := temporary.Chmod(0o444); err != nil {
		_ = temporary.Close()
		return ObjectRef{}, fmt.Errorf("mark immutable object read-only: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return ObjectRef{}, fmt.Errorf("sync temporary object: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return ObjectRef{}, fmt.Errorf("close temporary object: %w", err)
	}

	reference := ObjectRef{Digest: digest, SizeBytes: size}
	path, err := store.ObjectPath(reference)
	if err != nil {
		return ObjectRef{}, err
	}
	if err := validateContext(ctx); err != nil {
		return ObjectRef{}, err
	}
	if err := os.Link(temporaryPath, path); err == nil {
		if err := syncDirectory(algorithmRoot); err != nil {
			return ObjectRef{}, fmt.Errorf("sync object directory: %w", err)
		}
		return reference, nil
	} else if !errors.Is(err, os.ErrExist) {
		return ObjectRef{}, fmt.Errorf("atomically publish immutable object: %w", err)
	}

	if err := store.Verify(ctx, reference); err != nil {
		return ObjectRef{}, fmt.Errorf("existing object prevents immutable publication: %w", err)
	}
	return reference, nil
}

// PutBytes stores one byte slice as an immutable object.
func (store *ArtifactObjectStore) PutBytes(ctx context.Context, content []byte) (ObjectRef, error) {
	return store.Put(ctx, bytes.NewReader(content))
}

// PutManifest serializes a validated, reference-only manifest and stores the
// resulting JSON as an immutable object. The returned ManifestRef contains no
// inline payload data.
func (store *ArtifactObjectStore) PutManifest(ctx context.Context, manifest ArtifactManifest) (ManifestRef, error) {
	encoded, err := manifest.MarshalCanonicalJSON()
	if err != nil {
		return ManifestRef{}, err
	}
	object, err := store.PutBytes(ctx, encoded)
	if err != nil {
		return ManifestRef{}, err
	}
	return manifest.Reference(object)
}

// ReadManifest verifies and decodes a manifest object, then confirms that the
// persisted ArtifactID is the one named by its durable ManifestRef.
func (store *ArtifactObjectStore) ReadManifest(ctx context.Context, reference ManifestRef) (ArtifactManifest, error) {
	if err := reference.Validate(); err != nil {
		return ArtifactManifest{}, err
	}
	encoded, err := store.ReadAll(ctx, reference.Manifest)
	if err != nil {
		return ArtifactManifest{}, err
	}
	manifest, err := ParseArtifactManifest(encoded)
	if err != nil {
		return ArtifactManifest{}, err
	}
	if manifest.Artifact.ID != reference.ArtifactID {
		return ArtifactManifest{}, fmt.Errorf("%w: referenced artifact ID %q does not match manifest ID %q", ErrInvalidArtifactManifest, reference.ArtifactID, manifest.Artifact.ID)
	}
	return manifest, nil
}

// Open opens a regular object file after validating its reference and path.
// Open intentionally does not read the full object; use Verify or ReadAll
// when consumers must prove the bytes still match the reference.
func (store *ArtifactObjectStore) Open(ctx context.Context, reference ObjectRef) (io.ReadCloser, error) {
	if err := validateContext(ctx); err != nil {
		return nil, err
	}
	path, err := store.ObjectPath(reference)
	if err != nil {
		return nil, err
	}
	if err := store.requireReadLayout(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrObjectNotFound, reference.Digest)
	}
	if err != nil {
		return nil, fmt.Errorf("inspect object: %w", err)
	}
	if err := validateRegularObject(path, info); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrObjectNotFound, reference.Digest)
	}
	if err != nil {
		return nil, fmt.Errorf("open object: %w", err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat opened object: %w", err)
	}
	if err := validateRegularObject(path, openedInfo); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// Verify streams an object and confirms both its SHA-256 digest and expected
// byte size. It is the availability check for planner/reuse decisions.
func (store *ArtifactObjectStore) Verify(ctx context.Context, reference ObjectRef) error {
	file, err := store.Open(ctx, reference)
	if err != nil {
		return err
	}
	defer file.Close()
	size, digest, err := copyWithDigest(ctx, file, io.Discard)
	if err != nil {
		return fmt.Errorf("read object for verification: %w", err)
	}
	if digest != reference.Digest {
		return fmt.Errorf("%w: digest mismatch for %s", ErrObjectCorrupt, reference.Digest)
	}
	if size != reference.SizeBytes {
		return fmt.Errorf("%w: size mismatch for %s: expected %d, got %d", ErrObjectCorrupt, reference.Digest, reference.SizeBytes, size)
	}
	return nil
}

// ReadAll returns verified immutable bytes. It rejects missing, unsafe, or
// corrupt objects instead of exposing untrusted data to workflow consumers.
func (store *ArtifactObjectStore) ReadAll(ctx context.Context, reference ObjectRef) ([]byte, error) {
	file, err := store.Open(ctx, reference)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var content bytes.Buffer
	size, digest, err := copyWithDigest(ctx, file, &content)
	if err != nil {
		return nil, fmt.Errorf("read object: %w", err)
	}
	if digest != reference.Digest || size != reference.SizeBytes {
		return nil, fmt.Errorf("%w: content does not match %s", ErrObjectCorrupt, reference.Digest)
	}
	return content.Bytes(), nil
}

// Exists returns true only when the referenced object is present and passes
// full integrity validation. Corrupt objects are not reported as available.
func (store *ArtifactObjectStore) Exists(ctx context.Context, reference ObjectRef) (bool, error) {
	err := store.Verify(ctx, reference)
	if errors.Is(err, ErrObjectNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (store *ArtifactObjectStore) validate() error {
	if store == nil || strings.TrimSpace(store.root) == "" {
		return fmt.Errorf("%w: store is nil or root is empty", ErrInvalidObjectStore)
	}
	return nil
}

func (store *ArtifactObjectStore) ensureLayout() error {
	if err := store.validate(); err != nil {
		return err
	}
	if err := ensureDirectory(store.root); err != nil {
		return err
	}
	return ensureDirectory(filepath.Join(store.root, ObjectAlgorithm))
}

func (store *ArtifactObjectStore) requireReadLayout() error {
	if err := store.validate(); err != nil {
		return err
	}
	for _, path := range []string{store.root, filepath.Join(store.root, ObjectAlgorithm)} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: object layout %s is absent", ErrObjectNotFound, path)
		}
		if err != nil {
			return fmt.Errorf("inspect object layout: %w", err)
		}
		if err := validateDirectory(path, info); err != nil {
			return err
		}
	}
	return nil
}

func ensureDirectory(path string) error {
	if err := os.MkdirAll(path, 0o750); err != nil {
		return fmt.Errorf("create object directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect object directory: %w", err)
	}
	return validateDirectory(path, info)
}

func validateDirectory(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: %s is not a real directory", ErrUnsafeObjectPath, path)
	}
	return nil
}

func validateRegularObject(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrUnsafeObjectPath, path)
	}
	return nil
}

func objectDigestHex(reference ObjectRef) (string, error) {
	if err := reference.Validate(); err != nil {
		return "", err
	}
	prefix := ObjectAlgorithm + ":"
	digest := string(reference.Digest)
	if !strings.HasPrefix(digest, prefix) {
		return "", fmt.Errorf("%w: unsupported object algorithm", ErrInvalidObjectRef)
	}
	return strings.TrimPrefix(digest, prefix), nil
}

func copyWithDigest(ctx context.Context, source io.Reader, destination io.Writer) (int64, workflowkit.Fingerprint, error) {
	if err := validateContext(ctx); err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	buffer := make([]byte, 32*1024)
	var size int64
	for {
		if err := validateContext(ctx); err != nil {
			return 0, "", err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			if err := writeAll(destination, buffer[:read]); err != nil {
				return 0, "", err
			}
			if _, err := hash.Write(buffer[:read]); err != nil {
				return 0, "", fmt.Errorf("hash object content: %w", err)
			}
			size += int64(read)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return 0, "", fmt.Errorf("read object content: %w", readErr)
		}
		if read == 0 {
			return 0, "", fmt.Errorf("read object content: reader made no progress")
		}
	}
	return size, workflowkit.Fingerprint(ObjectAlgorithm + ":" + fmt.Sprintf("%x", hash.Sum(nil))), nil
}

func writeAll(destination io.Writer, content []byte) error {
	for len(content) > 0 {
		written, err := destination.Write(content)
		if err != nil {
			return fmt.Errorf("write object content: %w", err)
		}
		if written <= 0 {
			return fmt.Errorf("write object content: writer made no progress")
		}
		content = content[written:]
	}
	return nil
}

func validateContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", ErrInvalidObjectStore)
	}
	return ctx.Err()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
