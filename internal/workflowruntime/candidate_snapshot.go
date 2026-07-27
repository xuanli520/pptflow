package workflowruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

var (
	// ErrInvalidCandidateCapture marks a capture request that would expose an
	// uncontrolled workspace path or persist an unsafe filesystem object.
	ErrInvalidCandidateCapture = errors.New("workflowruntime: invalid candidate snapshot capture")
)

// CandidateFileCaptureSpec is host-supplied catalog data for one candidate
// file. Path is a logical, fixed file name rather than a model tool argument.
type CandidateFileCaptureSpec struct {
	Path          string
	SchemaVersion string
}

// CandidateSnapshotCapturer converts a fixed set of safe files in one
// attempt workspace into content-addressed objects and a workflowkit snapshot
// manifest. It stores no copy of the workspace tree: only the ordinary
// deduplicated immutable objects are retained.
type CandidateSnapshotCapturer struct {
	objects      *ArtifactObjectStore
	maxFileBytes int64
}

// NewCandidateSnapshotCapturer validates the only durable storage dependency
// needed for candidate capture. The maximum is an explicit host limit.
func NewCandidateSnapshotCapturer(objects *ArtifactObjectStore, maxFileBytes int64) (*CandidateSnapshotCapturer, error) {
	if objects == nil || objects.Root() == "" {
		return nil, fmt.Errorf("%w: immutable object store is required", ErrInvalidCandidateCapture)
	}
	if maxFileBytes <= 0 {
		return nil, fmt.Errorf("%w: maximum candidate file bytes must be positive", ErrInvalidCandidateCapture)
	}
	return &CandidateSnapshotCapturer{objects: objects, maxFileBytes: maxFileBytes}, nil
}

// Capture reads only catalog-declared regular files from one host-owned
// workspace. It rejects symlinks, unsafe directory components, duplicate
// paths, over-limit content, and files replaced while being read.
func (capturer *CandidateSnapshotCapturer) Capture(ctx context.Context, workspaceRoot string, files []CandidateFileCaptureSpec) (workflowkit.CandidateSnapshot, error) {
	if ctx == nil {
		return workflowkit.CandidateSnapshot{}, fmt.Errorf("%w: context is required", ErrInvalidCandidateCapture)
	}
	if err := ctx.Err(); err != nil {
		return workflowkit.CandidateSnapshot{}, err
	}
	if capturer == nil || capturer.objects == nil || capturer.maxFileBytes <= 0 {
		return workflowkit.CandidateSnapshot{}, fmt.Errorf("%w: capture is not configured", ErrInvalidCandidateCapture)
	}
	root, err := safeCandidateWorkspaceRoot(workspaceRoot)
	if err != nil {
		return workflowkit.CandidateSnapshot{}, err
	}
	if len(files) == 0 {
		return workflowkit.CandidateSnapshot{}, fmt.Errorf("%w: candidate capture requires at least one fixed file", ErrInvalidCandidateCapture)
	}
	seen := make(map[string]struct{}, len(files))
	captured := make([]workflowkit.CandidateFile, 0, len(files))
	for _, file := range files {
		if err := workflowkit.ValidateCandidateFilePath(file.Path); err != nil {
			return workflowkit.CandidateSnapshot{}, err
		}
		if strings.TrimSpace(file.SchemaVersion) == "" {
			return workflowkit.CandidateSnapshot{}, fmt.Errorf("%w: candidate schema version is required", ErrInvalidCandidateCapture)
		}
		if _, duplicate := seen[file.Path]; duplicate {
			return workflowkit.CandidateSnapshot{}, fmt.Errorf("%w: duplicate candidate file %q", ErrInvalidCandidateCapture, file.Path)
		}
		seen[file.Path] = struct{}{}
		content, err := capturer.readFixedWorkspaceFile(ctx, root, file.Path)
		if err != nil {
			return workflowkit.CandidateSnapshot{}, err
		}
		object, err := capturer.objects.PutBytes(ctx, content)
		if err != nil {
			return workflowkit.CandidateSnapshot{}, fmt.Errorf("store immutable candidate file %q: %w", file.Path, err)
		}
		captured = append(captured, workflowkit.CandidateFile{
			Path: file.Path, SchemaVersion: file.SchemaVersion, ContentDigest: object.Digest, SizeBytes: object.SizeBytes,
		})
	}
	snapshot, err := workflowkit.NewCandidateSnapshot(captured)
	if err != nil {
		return workflowkit.CandidateSnapshot{}, err
	}
	return snapshot, nil
}

func safeCandidateWorkspaceRoot(value string) (string, error) {
	if strings.TrimSpace(value) == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value || value == string(os.PathSeparator) {
		return "", fmt.Errorf("%w: workspace root must be a clean absolute non-root path", ErrInvalidCandidateCapture)
	}
	if err := inspectCandidateDirectory(value); err != nil {
		return "", err
	}
	return value, nil
}

func (capturer *CandidateSnapshotCapturer) readFixedWorkspaceFile(ctx context.Context, root, relative string) ([]byte, error) {
	directory := root
	for _, component := range strings.Split(filepath.ToSlash(filepath.Dir(relative)), "/") {
		if component == "." || component == "" {
			continue
		}
		directory = filepath.Join(directory, component)
		if err := inspectCandidateDirectory(directory); err != nil {
			return nil, err
		}
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > capturer.maxFileBytes {
		return nil, fmt.Errorf("%w: candidate file %q is unavailable or unsafe", ErrInvalidCandidateCapture, relative)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open candidate file %q: %w", relative, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() != info.Size() {
		return nil, fmt.Errorf("%w: candidate file %q changed while opening", ErrInvalidCandidateCapture, relative)
	}
	content, err := io.ReadAll(io.LimitReader(file, capturer.maxFileBytes+1))
	if err != nil || int64(len(content)) != info.Size() {
		return nil, fmt.Errorf("%w: candidate file %q changed while reading", ErrInvalidCandidateCapture, relative)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) || after.Size() != int64(len(content)) {
		return nil, fmt.Errorf("%w: candidate file %q changed while reading", ErrInvalidCandidateCapture, relative)
	}
	return content, nil
}

func inspectCandidateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: candidate workspace directory is unavailable or unsafe", ErrInvalidCandidateCapture)
	}
	return nil
}
