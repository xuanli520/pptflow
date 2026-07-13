package harborrun

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
)

var digestRoots = []string{
	"instruction.md",
	"task.toml",
	"tests_analysis.md",
	"environment",
	"solution",
	"tests",
}

// ComputeTaskDigest preserves the historical V1 task digest for existing
// reports and compatibility readers. New lifecycle code must use
// ComputeManagedTaskDigestV2 so V1 evidence cannot become V2 revision
// evidence by accident.
//
// Deprecated: use ComputeTaskDigestV1 only for legacy evidence, or
// ComputeManagedTaskDigestV2 for a V2 TaskRevision.
func ComputeTaskDigest(taskDir string) (string, error) {
	return ComputeTaskDigestV1(taskDir)
}

// ComputeTaskDigestV1 computes the legacy unversioned-on-the-wire
// sha256:<hex> digest. It is intentionally kept byte-for-byte compatible with
// the pre-V2 implementation and must be treated as read-only evidence.
func ComputeTaskDigestV1(taskDir string) (string, error) {
	taskDir = strings.TrimSpace(taskDir)
	if taskDir == "" {
		return "", fmt.Errorf("task directory is required")
	}
	info, err := os.Stat(taskDir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("task path is not a directory: %s", taskDir)
	}
	var files []string
	for _, root := range digestRoots {
		path := filepath.Join(taskDir, root)
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			files = append(files, root)
			continue
		}
		err = filepath.WalkDir(path, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeType != 0 {
				return nil
			}
			rel, err := filepath.Rel(taskDir, path)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	sort.Strings(files)
	hash := sha256.New()
	for _, rel := range files {
		path := filepath.Join(taskDir, filepath.FromSlash(rel))
		if _, err := io.WriteString(hash, rel+"\n"); err != nil {
			return "", err
		}
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(hash, file); err != nil {
			_ = file.Close()
			return "", err
		}
		if err := file.Close(); err != nil {
			return "", err
		}
		if _, err := io.WriteString(hash, "\n"); err != nil {
			return "", err
		}
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// ComputeManagedTaskDigestV2 computes the canonical digest for a managed task
// revision snapshot. The control plane owns the managed-root invariant; this
// function enforces the strict V2 Harbor file policy and digest format.
func ComputeManagedTaskDigestV2(taskDir string) (string, error) {
	return taskpolicy.ComputeManagedTaskDigestV2(taskDir)
}

// ValidateManagedTaskSnapshotV2 validates the canonical filesystem shape
// before the control plane seals a revision snapshot.
func ValidateManagedTaskSnapshotV2(taskDir string) error {
	return taskpolicy.ValidateManagedSnapshotV2(taskDir)
}

// TaskDigestVersion is re-exported at the Harbor runtime boundary so callers
// can reject cross-generation evidence without parsing digest strings.
type TaskDigestVersion = taskpolicy.TaskDigestVersion

const (
	LegacyTaskDigestPrefix = taskpolicy.LegacyTaskDigestPrefix
	TaskDigestV2Scheme     = taskpolicy.TaskDigestV2Scheme
	TaskDigestV2Prefix     = taskpolicy.TaskDigestV2Prefix

	TaskDigestUnknown  = taskpolicy.TaskDigestUnknown
	TaskDigestLegacyV1 = taskpolicy.TaskDigestLegacyV1
	TaskDigestV2       = taskpolicy.TaskDigestV2
)

// ClassifyTaskDigest identifies a valid V1 compatibility digest or explicit
// V2 revision digest. Unknown or malformed values are rejected.
func ClassifyTaskDigest(value string) (TaskDigestVersion, error) {
	return taskpolicy.ClassifyTaskDigest(value)
}

// ValidateV2TaskDigest rejects legacy or malformed task evidence at a V2
// revision boundary.
func ValidateV2TaskDigest(value string) error {
	return taskpolicy.ValidateV2TaskDigest(value)
}

// EqualTaskDigests compares only valid digests from the same evidence
// generation. It returns false for V1/V2 cross-generation comparisons.
func EqualTaskDigests(left, right string) bool {
	return taskpolicy.EqualTaskDigests(left, right)
}
