package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
)

const (
	managedTasksDirectory = "tasks"
	managedRunsDirectory  = "runs"
	managedTrashDirectory = "trash"
)

// managedLayout owns the V2 filesystem layout. A task snapshot deliberately
// lives below a separate snapshot directory so revision metadata never enters
// the strict seven-file Harbor digest boundary.
type managedLayout struct {
	root string
}

func newManagedLayout(root string) (managedLayout, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return managedLayout{}, fmt.Errorf("managed root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return managedLayout{}, fmt.Errorf("resolve managed root: %w", err)
	}
	if info, err := os.Lstat(abs); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return managedLayout{}, fmt.Errorf("managed root is not a real directory: %s", abs)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return managedLayout{}, fmt.Errorf("inspect managed root: %w", err)
	}
	return managedLayout{root: abs}, nil
}

func (layout managedLayout) taskDirectory(taskID string) string {
	return filepath.Join(layout.root, managedTasksDirectory, taskID)
}

func (layout managedLayout) revisionDirectory(taskID, revisionID string) string {
	return filepath.Join(layout.taskDirectory(taskID), "revisions", revisionID)
}

func (layout managedLayout) snapshotDirectory(taskID, revisionID string) string {
	return filepath.Join(layout.revisionDirectory(taskID, revisionID), "snapshot")
}

func (layout managedLayout) revisionManifestPath(taskID, revisionID string) string {
	return filepath.Join(layout.revisionDirectory(taskID, revisionID), "manifest.json")
}

// candidateDirectory is deliberately separate from immutable revisions. Its
// checkout is the sole mutable task tree permitted during a ChangeProvider
// operation and is always addressed by stable Task/Candidate UUIDv7 IDs.
func (layout managedLayout) candidateDirectory(taskID, candidateID string) string {
	return filepath.Join(layout.taskDirectory(taskID), "candidates", candidateID)
}

func (layout managedLayout) candidateCheckoutDirectory(taskID, candidateID string) string {
	return filepath.Join(layout.candidateDirectory(taskID, candidateID), "checkout")
}

func (layout managedLayout) candidateCheckoutRelpath(taskID, candidateID string) string {
	return filepath.ToSlash(filepath.Join(managedTasksDirectory, taskID, "candidates", candidateID, "checkout"))
}

func (layout managedLayout) runDirectory(runID string) string {
	return filepath.Join(layout.root, managedRunsDirectory, runID)
}

func (layout managedLayout) releaseDirectory(version string) string {
	return filepath.Join(layout.root, "packages", version)
}

func (layout managedLayout) trashDirectory(entityType, entityID string) string {
	return filepath.Join(layout.root, managedTrashDirectory, entityType, entityID)
}

func (layout managedLayout) ensureRoot() error {
	if err := os.MkdirAll(layout.root, 0o750); err != nil {
		return fmt.Errorf("create managed root: %w", err)
	}
	info, err := os.Lstat(layout.root)
	if err != nil {
		return fmt.Errorf("inspect managed root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("managed root is not a real directory: %s", layout.root)
	}
	return nil
}

// materializeManagedSnapshot validates source against the V2 task policy,
// then creates a new, non-overwriting snapshot using source bytes exactly as
// read. It deliberately copies only the policy's file list, assigns the
// canonical file modes, and validates the result again before returning its
// V2 digest.
func materializeManagedSnapshot(ctx context.Context, source, destination string) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return "", fmt.Errorf("source task snapshot is required")
	}
	if err := taskpolicy.ValidateManagedSnapshotV2(source); err != nil {
		return "", fmt.Errorf("validate source task snapshot: %w", err)
	}
	if err := os.Mkdir(destination, 0o750); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("managed task snapshot already exists: %s", destination)
		}
		return "", fmt.Errorf("create managed task snapshot: %w", err)
	}

	for _, file := range taskpolicy.CanonicalFiles() {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		sourcePath := filepath.Join(source, filepath.FromSlash(file.Path))
		info, err := os.Lstat(sourcePath)
		if errors.Is(err, os.ErrNotExist) && file.Environment {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("inspect source snapshot file %s: %w", file.Path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", fmt.Errorf("source snapshot file is not a regular non-symlink: %s", file.Path)
		}
		destinationPath := filepath.Join(destination, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(destinationPath), 0o750); err != nil {
			return "", fmt.Errorf("create snapshot parent for %s: %w", file.Path, err)
		}
		if err := copyCanonicalSnapshotFile(ctx, sourcePath, destinationPath, file.Mode); err != nil {
			return "", fmt.Errorf("copy snapshot file %s: %w", file.Path, err)
		}
	}
	if err := taskpolicy.ValidateManagedSnapshotV2(destination); err != nil {
		return "", fmt.Errorf("validate materialized task snapshot: %w", err)
	}
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(destination)
	if err != nil {
		return "", fmt.Errorf("digest materialized task snapshot: %w", err)
	}
	return digest, nil
}

func copyCanonicalSnapshotFile(ctx context.Context, source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() {
		return fmt.Errorf("source changed while opening")
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	copyErr := copyContext(ctx, output, input)
	if copyErr == nil {
		copyErr = output.Chmod(mode)
	}
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil {
		_ = os.Remove(destination)
		return copyErr
	}
	return closeErr
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) error {
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			written := 0
			for written < count {
				if err := ctx.Err(); err != nil {
					return err
				}
				n, writeErr := destination.Write(buffer[written:count])
				if writeErr != nil {
					return writeErr
				}
				if n <= 0 {
					return io.ErrShortWrite
				}
				written += n
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if count == 0 {
			return fmt.Errorf("source reader made no progress")
		}
	}
}

func writeNewJSON(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
