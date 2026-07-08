package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type FileArtifactStore struct {
	root     string
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
	return &FileArtifactStore{root: abs}, nil
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
	target, rel, err := s.resolveForWrite(req.Name)
	if err != nil {
		return ArtifactRef{}, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return ArtifactRef{}, err
	}
	out, err := os.Create(target)
	if err != nil {
		return ArtifactRef{}, err
	}
	_, copyErr := io.Copy(out, req.Content)
	closeErr := out.Close()
	if copyErr != nil {
		return ArtifactRef{}, copyErr
	}
	if closeErr != nil {
		return ArtifactRef{}, closeErr
	}
	s.sequence++
	ref := ArtifactRef{
		ID:        fmt.Sprintf("artifact-%04d", s.sequence),
		Name:      filepath.ToSlash(rel),
		Path:      target,
		Type:      strings.TrimSpace(req.Type),
		Producer:  strings.TrimSpace(req.Producer),
		Metadata:  copyStringMap(req.Metadata),
		CreatedAt: time.Now().UTC(),
	}
	s.recordRef(ref)
	return ref, nil
}

func (s *FileArtifactStore) PutJSON(ctx context.Context, name, artifactType, producer string, value any) (ArtifactRef, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return ArtifactRef{}, err
	}
	data = append(data, '\n')
	return s.Put(ctx, PutArtifactRequest{Name: name, Type: artifactType, Producer: producer, Content: bytes.NewReader(data)})
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
	clean := filepath.Clean(path)
	if !withinRoot(clean, s.root) {
		return ArtifactRef{}, fmt.Errorf("artifact path escapes root: %s", path)
	}
	info, err := os.Stat(clean)
	if err != nil {
		return ArtifactRef{}, err
	}
	if info.IsDir() {
		return ArtifactRef{}, fmt.Errorf("artifact path is a directory: %s", path)
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		rel, err := filepath.Rel(s.root, clean)
		if err != nil {
			return ArtifactRef{}, err
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
	s.sequence++
	ref := ArtifactRef{
		ID:        fmt.Sprintf("artifact-%04d", s.sequence),
		Name:      filepath.ToSlash(rel),
		Path:      clean,
		Type:      strings.TrimSpace(req.Type),
		Producer:  strings.TrimSpace(req.Producer),
		Metadata:  copyStringMap(req.Metadata),
		CreatedAt: time.Now().UTC(),
	}
	s.recordRef(ref)
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
	file, err := os.Open(path)
	if err != nil {
		return nil, ArtifactMeta{}, err
	}
	return file, ref, nil
}

func (s *FileArtifactStore) ReadJSON(ctx context.Context, name string, target any) (ArtifactRef, error) {
	path, err := s.Path(name)
	if err != nil {
		return ArtifactRef{}, err
	}
	if err := ctx.Err(); err != nil {
		return ArtifactRef{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ArtifactRef{}, err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return ArtifactRef{}, err
	}
	for _, ref := range s.refs {
		if filepath.Clean(ref.Path) == filepath.Clean(path) {
			return ref, nil
		}
	}
	return ArtifactRef{Name: filepath.ToSlash(name), Path: path}, nil
}

func (s *FileArtifactStore) List(ctx context.Context, prefix string) ([]ArtifactMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prefix = filepath.ToSlash(strings.TrimSpace(prefix))
	result := make([]ArtifactMeta, 0, len(s.refs))
	for _, ref := range s.refs {
		if prefix == "" || strings.HasPrefix(filepath.ToSlash(ref.Name), prefix) {
			result = append(result, ref)
		}
	}
	return result, nil
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

func (s *FileArtifactStore) recordRef(ref ArtifactRef) {
	cleanPath := filepath.Clean(ref.Path)
	cleanName := filepath.ToSlash(ref.Name)
	for i, existing := range s.refs {
		if filepath.Clean(existing.Path) == cleanPath || filepath.ToSlash(existing.Name) == cleanName {
			s.refs[i] = ref
			return
		}
	}
	s.refs = append(s.refs, ref)
}
