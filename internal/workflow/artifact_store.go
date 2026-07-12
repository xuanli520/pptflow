package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const artifactIndexName = ".artifact-index.json"

type artifactIndex struct {
	SchemaVersion string        `json:"schema_version"`
	Sequence      int           `json:"sequence"`
	Artifacts     []ArtifactRef `json:"artifacts"`
}

// FileArtifactStore is a durable workspace-local artifact catalog. Artifact
// content and its index are committed atomically, and the catalog is restored
// when a process reopens the same root.
type FileArtifactStore struct {
	root     string
	mu       sync.RWMutex
	sequence int
	refs     []ArtifactRef
}

func NewFileArtifactStore(root string) (*FileArtifactStore, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "" || root == "." {
		return nil, fmt.Errorf("artifact root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	s := &FileArtifactStore{root: abs}
	if err := s.loadIndex(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *FileArtifactStore) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

func (s *FileArtifactStore) Put(ctx context.Context, req PutArtifactRequest) (ArtifactRef, error) {
	if err := ctx.Err(); err != nil {
		return ArtifactRef{}, err
	}
	if s == nil {
		return ArtifactRef{}, fmt.Errorf("artifact store is nil")
	}
	if req.Content == nil {
		return ArtifactRef{}, fmt.Errorf("artifact content is required")
	}
	target, rel, err := s.resolveForWrite(req.Name)
	if err != nil {
		return ArtifactRef{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return ArtifactRef{}, err
	}
	if err := s.ensureResolvedWithinRoot(filepath.Dir(target)); err != nil {
		return ArtifactRef{}, err
	}
	digest, size, err := atomicWriteReader(target, req.Content, 0o644)
	if err != nil {
		return ArtifactRef{}, err
	}
	s.sequence++
	ref := ArtifactRef{
		ID:        fmt.Sprintf("artifact-%04d", s.sequence),
		Name:      filepath.ToSlash(rel),
		Path:      target,
		Type:      strings.TrimSpace(req.Type),
		Producer:  strings.TrimSpace(req.Producer),
		Metadata:  copyStringMap(req.Metadata),
		SHA256:    "sha256:" + digest,
		SizeBytes: size,
		CreatedAt: time.Now().UTC(),
	}
	s.recordRefLocked(ref)
	if err := s.persistIndexLocked(); err != nil {
		return ArtifactRef{}, err
	}
	return ref, nil
}

func (s *FileArtifactStore) PutJSON(ctx context.Context, name, artifactType, producer string, value any) (ArtifactRef, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return ArtifactRef{}, err
	}
	return s.Put(ctx, PutArtifactRequest{Name: name, Type: artifactType, Producer: producer, Content: bytes.NewReader(append(data, '\n'))})
}

func (s *FileArtifactStore) PutText(ctx context.Context, name, artifactType, producer, value string) (ArtifactRef, error) {
	return s.Put(ctx, PutArtifactRequest{Name: name, Type: artifactType, Producer: producer, Content: strings.NewReader(value)})
}

func (s *FileArtifactStore) Register(ctx context.Context, req RegisterArtifactRequest) (ArtifactRef, error) {
	if err := ctx.Err(); err != nil {
		return ArtifactRef{}, err
	}
	if s == nil {
		return ArtifactRef{}, fmt.Errorf("artifact store is nil")
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		var err error
		path, err = s.Path(req.Name)
		if err != nil {
			return ArtifactRef{}, err
		}
	}
	clean, err := filepath.Abs(filepath.Clean(path))
	if err != nil || !withinRoot(clean, s.root) {
		return ArtifactRef{}, fmt.Errorf("artifact path escapes root: %s", path)
	}
	if err := s.ensureResolvedWithinRoot(clean); err != nil {
		return ArtifactRef{}, err
	}
	info, err := os.Stat(clean)
	if err != nil {
		return ArtifactRef{}, err
	}
	if !info.Mode().IsRegular() {
		return ArtifactRef{}, fmt.Errorf("artifact path is not a regular file: %s", path)
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		rel, relErr := filepath.Rel(s.root, clean)
		if relErr != nil {
			return ArtifactRef{}, relErr
		}
		name = filepath.ToSlash(rel)
	}
	target, rel, err := s.resolveForWrite(name)
	if err != nil {
		return ArtifactRef{}, err
	}
	if filepath.Clean(target) != clean {
		return ArtifactRef{}, fmt.Errorf("registered artifact name %s resolves to %s, got %s", name, target, clean)
	}
	digest, size, err := digestFile(clean)
	if err != nil {
		return ArtifactRef{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sequence++
	ref := ArtifactRef{ID: fmt.Sprintf("artifact-%04d", s.sequence), Name: filepath.ToSlash(rel), Path: clean, Type: strings.TrimSpace(req.Type), Producer: strings.TrimSpace(req.Producer), Metadata: copyStringMap(req.Metadata), SHA256: "sha256:" + digest, SizeBytes: size, CreatedAt: time.Now().UTC()}
	s.recordRefLocked(ref)
	if err := s.persistIndexLocked(); err != nil {
		return ArtifactRef{}, err
	}
	return ref, nil
}

func (s *FileArtifactStore) Get(ctx context.Context, ref ArtifactRef) (io.ReadCloser, ArtifactMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, ArtifactMeta{}, err
	}
	path := strings.TrimSpace(ref.Path)
	if path == "" {
		var err error
		path, err = s.Path(ref.Name)
		if err != nil {
			return nil, ArtifactMeta{}, err
		}
	}
	if !withinRoot(path, s.root) {
		return nil, ArtifactMeta{}, fmt.Errorf("artifact path escapes root: %s", path)
	}
	if err := s.ensureResolvedWithinRoot(path); err != nil {
		return nil, ArtifactMeta{}, err
	}
	if strings.TrimSpace(ref.SHA256) != "" {
		digest, size, err := digestFile(path)
		if err != nil {
			return nil, ArtifactMeta{}, err
		}
		if !strings.EqualFold(strings.TrimPrefix(ref.SHA256, "sha256:"), digest) {
			return nil, ArtifactMeta{}, fmt.Errorf("artifact digest mismatch: %s", ref.Name)
		}
		if ref.SizeBytes > 0 && ref.SizeBytes != size {
			return nil, ArtifactMeta{}, fmt.Errorf("artifact size mismatch: %s", ref.Name)
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ArtifactMeta{}, err
	}
	return file, cloneArtifactRef(ref), nil
}

func (s *FileArtifactStore) ReadJSON(ctx context.Context, name string, target any) (ArtifactRef, error) {
	path, err := s.Path(name)
	if err != nil {
		return ArtifactRef{}, err
	}
	if err := ctx.Err(); err != nil {
		return ArtifactRef{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ArtifactRef{}, err
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return ArtifactRef{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ref := range s.refs {
		if filepath.Clean(ref.Path) == filepath.Clean(path) {
			return cloneArtifactRef(ref), nil
		}
	}
	return ArtifactRef{Name: filepath.ToSlash(name), Path: path}, nil
}

func (s *FileArtifactStore) List(ctx context.Context, prefix string) ([]ArtifactMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prefix = filepath.ToSlash(strings.TrimSpace(prefix))
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]ArtifactMeta, 0, len(s.refs))
	for _, ref := range s.refs {
		if prefix == "" || strings.HasPrefix(filepath.ToSlash(ref.Name), prefix) {
			result = append(result, cloneArtifactRef(ref))
		}
	}
	return result, nil
}

func (s *FileArtifactStore) InvalidateProducers(ctx context.Context, producers []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("artifact store is nil")
	}
	invalid := make(map[string]bool, len(producers))
	for _, producer := range producers {
		if producer = strings.TrimSpace(producer); producer != "" {
			invalid[producer] = true
		}
	}
	if len(invalid) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]ArtifactRef, 0, len(s.refs))
	for _, ref := range s.refs {
		if !invalid[ref.Producer] {
			kept = append(kept, ref)
			continue
		}
		if withinRoot(ref.Path, s.root) {
			if err := os.Remove(ref.Path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove invalidated artifact %s: %w", ref.Name, err)
			}
		}
	}
	s.refs = kept
	return s.persistIndexLocked()
}

func (s *FileArtifactStore) Path(name string) (string, error) {
	target, _, err := s.resolveForWrite(name)
	return target, err
}

func (s *FileArtifactStore) resolveForWrite(name string) (string, string, error) {
	if s == nil {
		return "", "", fmt.Errorf("artifact store is nil")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", fmt.Errorf("artifact name is required")
	}
	if filepath.IsAbs(name) {
		clean := filepath.Clean(name)
		if !withinRoot(clean, s.root) {
			return "", "", fmt.Errorf("artifact path escapes root: %s", name)
		}
		rel, err := filepath.Rel(s.root, clean)
		return clean, rel, err
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("artifact path escapes root: %s", name)
	}
	target := filepath.Join(s.root, clean)
	if !withinRoot(target, s.root) {
		return "", "", fmt.Errorf("artifact path escapes root: %s", name)
	}
	return target, clean, nil
}

func withinRoot(path, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func (s *FileArtifactStore) ensureResolvedWithinRoot(path string) error {
	resolvedRoot, err := filepath.EvalSymlinks(s.root)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if !withinRoot(resolved, resolvedRoot) {
		return fmt.Errorf("artifact path resolves outside root: %s", path)
	}
	return nil
}

func copyStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func (s *FileArtifactStore) recordRefLocked(ref ArtifactRef) {
	ref = cloneArtifactRef(ref)
	cleanPath, cleanName := filepath.Clean(ref.Path), filepath.ToSlash(ref.Name)
	for i, existing := range s.refs {
		if filepath.Clean(existing.Path) == cleanPath || filepath.ToSlash(existing.Name) == cleanName {
			s.refs[i] = ref
			return
		}
	}
	s.refs = append(s.refs, ref)
}

func (s *FileArtifactStore) loadIndex() error {
	raw, err := os.ReadFile(filepath.Join(s.root, artifactIndexName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read artifact index: %w", err)
	}
	var index artifactIndex
	if err := json.Unmarshal(raw, &index); err != nil {
		return fmt.Errorf("parse artifact index: %w", err)
	}
	if index.SchemaVersion != "workflow.artifacts.v1" {
		return fmt.Errorf("unsupported artifact index schema %q", index.SchemaVersion)
	}
	for _, ref := range index.Artifacts {
		if !withinRoot(ref.Path, s.root) {
			return fmt.Errorf("artifact index path escapes root: %s", ref.Path)
		}
	}
	s.sequence, s.refs = index.Sequence, append([]ArtifactRef(nil), index.Artifacts...)
	return nil
}

func (s *FileArtifactStore) persistIndexLocked() error {
	refs := make([]ArtifactRef, 0, len(s.refs))
	for _, ref := range s.refs {
		refs = append(refs, cloneArtifactRef(ref))
	}
	index := artifactIndex{SchemaVersion: "workflow.artifacts.v1", Sequence: s.sequence, Artifacts: refs}
	raw, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	_, _, err = atomicWriteReader(filepath.Join(s.root, artifactIndexName), bytes.NewReader(append(raw, '\n')), 0o600)
	return err
}

func atomicWriteReader(path string, content io.Reader, mode os.FileMode) (string, int64, error) {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return "", 0, err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return "", 0, err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(file, hash), content)
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr := file.Close()
	if copyErr != nil {
		return "", 0, copyErr
	}
	if closeErr != nil {
		return "", 0, closeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", 0, err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func digestFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func cloneArtifactRef(ref ArtifactRef) ArtifactRef {
	ref.Metadata = copyStringMap(ref.Metadata)
	return ref
}
