package taskpolicy

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const canonicalManifestV2Domain = "harbor.task.v2"

// SnapshotValidationError reports all deterministic V2 policy violations
// discovered while inspecting a prospective managed snapshot.
type SnapshotValidationError struct {
	Violations []string
}

func (e *SnapshotValidationError) Error() string {
	if e == nil || len(e.Violations) == 0 {
		return "invalid managed Harbor task snapshot"
	}
	return "invalid managed Harbor task snapshot: " + strings.Join(e.Violations, "; ")
}

type snapshotFile struct {
	path string
	mode os.FileMode
}

// ValidateManagedSnapshotV2 confirms that root has exactly the strict Harbor
// V2 layout. Ownership and immutability of the root are control-plane
// invariants; this function validates the bytes and filesystem shape that may
// be bound to a managed revision.
func ValidateManagedSnapshotV2(root string) error {
	_, err := inspectManagedSnapshotV2(root)
	return err
}

// ComputeManagedTaskDigestV2 returns the canonical digest for a managed task
// revision snapshot. It hashes a length-prefixed binary manifest containing
// the domain, record count, then for each lexically sorted canonical path its
// path, policy-assigned mode, raw byte length, and raw content SHA-256 value.
// Every field is encoded as an unsigned 64-bit big-endian length followed by
// its bytes. Source path, timestamps, ownership, ACLs, and source permissions
// intentionally do not affect the result.
func ComputeManagedTaskDigestV2(root string) (string, error) {
	root = strings.TrimSpace(root)
	files, err := inspectManagedSnapshotV2(root)
	if err != nil {
		return "", err
	}

	manifest := sha256.New()
	if err := writeManifestField(manifest, []byte(canonicalManifestV2Domain)); err != nil {
		return "", fmt.Errorf("write V2 manifest domain: %w", err)
	}
	if err := writeManifestField(manifest, uint64Bytes(uint64(len(files)))); err != nil {
		return "", fmt.Errorf("write V2 manifest file count: %w", err)
	}
	for _, file := range files {
		contentDigest, size, err := digestRawRegularFile(filepath.Join(root, filepath.FromSlash(file.path)))
		if err != nil {
			return "", fmt.Errorf("digest managed snapshot file %s: %w", file.path, err)
		}
		if err := writeManifestField(manifest, []byte(file.path)); err != nil {
			return "", fmt.Errorf("write V2 manifest path %s: %w", file.path, err)
		}
		if err := writeManifestField(manifest, uint32Bytes(uint32(file.mode.Perm()))); err != nil {
			return "", fmt.Errorf("write V2 manifest mode %s: %w", file.path, err)
		}
		if err := writeManifestField(manifest, uint64Bytes(size)); err != nil {
			return "", fmt.Errorf("write V2 manifest size %s: %w", file.path, err)
		}
		if err := writeManifestField(manifest, contentDigest[:]); err != nil {
			return "", fmt.Errorf("write V2 manifest content digest %s: %w", file.path, err)
		}
	}
	return TaskDigestV2Prefix + hex.EncodeToString(manifest.Sum(nil)), nil
}

// IsV2TaskDigest reports whether value is a valid canonical V2 digest.
func IsV2TaskDigest(value string) bool {
	return ValidateV2TaskDigest(value) == nil
}

// ValidateV2TaskDigest validates the only task-digest format accepted by the
// V2 revision boundary. Control-plane persistence calls it before binding a
// digest to a TaskRevision.
func ValidateV2TaskDigest(value string) error {
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("V2 task digest must not contain surrounding whitespace")
	}
	if !strings.HasPrefix(value, TaskDigestV2Prefix) {
		return fmt.Errorf("V2 task digest must use %s", TaskDigestV2Prefix)
	}
	if err := validateDigestHex(strings.TrimPrefix(value, TaskDigestV2Prefix)); err != nil {
		return fmt.Errorf("invalid %s digest: %w", TaskDigestV2Scheme, err)
	}
	return nil
}

func inspectManagedSnapshotV2(root string) ([]snapshotFile, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("managed task snapshot root is required")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect managed task snapshot root: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, &SnapshotValidationError{Violations: []string{"snapshot root must not be a symlink"}}
	}
	if !rootInfo.IsDir() {
		return nil, &SnapshotValidationError{Violations: []string{"snapshot root must be a directory"}}
	}

	expectedDirectories := map[string]struct{}{
		"environment": {},
		"solution":    {},
		"tests":       {},
	}
	seen := make(map[string]CanonicalFile, len(canonicalFiles))
	var violations []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.Type()&os.ModeSymlink != 0 {
			violations = append(violations, "symlink is not allowed: "+rel)
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			if _, ok := expectedDirectories[rel]; !ok {
				violations = append(violations, "unexpected directory: "+rel)
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			violations = append(violations, "non-regular file is not allowed: "+rel)
			return nil
		}
		file, ok := allowedFiles[rel]
		if !ok {
			violations = append(violations, "unexpected file: "+rel)
			return nil
		}
		seen[rel] = file
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk managed task snapshot: %w", err)
	}

	var files []snapshotFile
	environmentCount := 0
	for _, file := range canonicalFiles {
		if _, ok := seen[file.Path]; !ok {
			if file.Required {
				violations = append(violations, "missing required file: "+file.Path)
			}
			continue
		}
		if file.Environment {
			environmentCount++
		}
		files = append(files, snapshotFile{path: file.Path, mode: file.Mode})
	}
	if environmentCount == 0 {
		violations = append(violations, "at least one environment file is required")
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		return nil, &SnapshotValidationError{Violations: violations}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, nil
}

func digestRawRegularFile(path string) ([sha256.Size]byte, uint64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return [sha256.Size]byte{}, 0, fmt.Errorf("file is no longer a regular non-symlink")
	}
	file, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return [sha256.Size]byte{}, 0, fmt.Errorf("file changed while opening")
	}

	content := sha256.New()
	size, err := io.Copy(content, file)
	if err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	if size < 0 {
		return [sha256.Size]byte{}, 0, fmt.Errorf("negative file size")
	}
	after, err := file.Stat()
	if err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(info, after) || after.Size() != size {
		return [sha256.Size]byte{}, 0, fmt.Errorf("file changed while reading")
	}
	var digest [sha256.Size]byte
	copy(digest[:], content.Sum(nil))
	return digest, uint64(size), nil
}

func writeManifestField(writer io.Writer, value []byte) error {
	if _, err := writer.Write(uint64Bytes(uint64(len(value)))); err != nil {
		return err
	}
	_, err := writer.Write(value)
	return err
}

func uint32Bytes(value uint32) []byte {
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], value)
	return out[:]
}

func uint64Bytes(value uint64) []byte {
	var out [8]byte
	binary.BigEndian.PutUint64(out[:], value)
	return out[:]
}

func validateDigestHex(value string) error {
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("expected %d hexadecimal characters", sha256.Size*2)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("invalid hexadecimal digest: %w", err)
	}
	return nil
}
