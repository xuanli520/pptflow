package taskpolicy

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// ManagedSnapshotZIPRoot is the only root directory accepted in the
	// deterministic task snapshot archive emitted by the V2 lifecycle.
	ManagedSnapshotZIPRoot = "task"

	// ManagedSnapshotZIPMaxBytes, ManagedSnapshotFileMaxBytes and
	// ManagedSnapshotTotalMaxBytes bound untrusted compressed input before it
	// is materialized under a controlled workspace.
	ManagedSnapshotZIPMaxBytes   = 32 << 20
	ManagedSnapshotFileMaxBytes  = 16 << 20
	ManagedSnapshotTotalMaxBytes = 48 << 20
)

// ExtractManagedSnapshotV2ZIP verifies and extracts the one canonical V2
// task archive to a new, caller-controlled destination. It accepts no archive
// path selection, follows no archive symlinks, and validates the resulting
// file set and digest against the frozen TaskRevision digest.
//
// The destination must not exist. Callers remain responsible for deriving it
// from a managed root rather than a user-supplied workspace path.
func ExtractManagedSnapshotV2ZIP(ctx context.Context, raw []byte, destination, expectedTaskDigest string) error {
	if ctx == nil {
		return errors.New("managed task snapshot extraction context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateV2TaskDigest(expectedTaskDigest); err != nil {
		return fmt.Errorf("expected task digest: %w", err)
	}
	if len(raw) == 0 || len(raw) > ManagedSnapshotZIPMaxBytes {
		return errors.New("managed task snapshot ZIP has invalid size")
	}
	destination = strings.TrimSpace(destination)
	if destination == "" || !filepath.IsAbs(destination) {
		return errors.New("managed task snapshot destination must be an absolute path")
	}
	if filepath.Clean(destination) != destination {
		return errors.New("managed task snapshot destination is not clean")
	}

	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return fmt.Errorf("read managed task snapshot ZIP: %w", err)
	}
	allowed := make(map[string]CanonicalFile, len(canonicalFiles))
	for _, file := range CanonicalFiles() {
		allowed[ManagedSnapshotZIPRoot+"/"+file.Path] = file
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return fmt.Errorf("create managed task snapshot destination: %w", err)
	}

	created := true
	defer func() {
		if created {
			_ = os.RemoveAll(destination)
		}
	}()
	seen := make(map[string]struct{}, len(allowed))
	var total uint64
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !safeManagedSnapshotZIPPath(entry.Name) {
			return errors.New("managed task snapshot ZIP contains an unsafe path")
		}
		canonical, allowedEntry := allowed[entry.Name]
		if !allowedEntry || entry.FileInfo().IsDir() || entry.FileInfo().Mode()&os.ModeSymlink != 0 || !entry.FileInfo().Mode().IsRegular() {
			return errors.New("managed task snapshot ZIP contains an unexpected or non-regular entry")
		}
		if _, duplicate := seen[entry.Name]; duplicate {
			return errors.New("managed task snapshot ZIP contains a duplicate entry")
		}
		if entry.UncompressedSize64 > ManagedSnapshotFileMaxBytes || entry.UncompressedSize64 > ManagedSnapshotTotalMaxBytes-total {
			return errors.New("managed task snapshot ZIP exceeds extraction limits")
		}
		seen[entry.Name] = struct{}{}
		total += entry.UncompressedSize64
		path := filepath.Join(destination, filepath.FromSlash(canonical.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create managed task snapshot parent: %w", err)
		}
		input, openErr := entry.Open()
		if openErr != nil {
			return fmt.Errorf("open managed task snapshot entry: %w", openErr)
		}
		output, createErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, canonical.Mode)
		if createErr != nil {
			_ = input.Close()
			return fmt.Errorf("create managed task snapshot entry: %w", createErr)
		}
		written, copyErr := io.Copy(output, io.LimitReader(input, int64(entry.UncompressedSize64)+1))
		closeOutputErr := output.Close()
		closeInputErr := input.Close()
		if copyErr != nil || closeOutputErr != nil || closeInputErr != nil || uint64(written) != entry.UncompressedSize64 {
			return errors.New("extract managed task snapshot ZIP entry")
		}
		if err := os.Chmod(path, canonical.Mode); err != nil {
			return fmt.Errorf("set managed task snapshot entry mode: %w", err)
		}
	}
	if len(seen) == 0 {
		return errors.New("managed task snapshot ZIP is empty")
	}
	if err := ValidateManagedSnapshotV2(destination); err != nil {
		return err
	}
	digest, err := ComputeManagedTaskDigestV2(destination)
	if err != nil {
		return err
	}
	if digest != expectedTaskDigest {
		return errors.New("materialized task snapshot digest does not equal frozen task digest")
	}
	created = false
	return nil
}

func safeManagedSnapshotZIPPath(value string) bool {
	if value == "" || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "../") || strings.Contains(value, "../") {
		return false
	}
	return filepath.Clean(value) == value
}
