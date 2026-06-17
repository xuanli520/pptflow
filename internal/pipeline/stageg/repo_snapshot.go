package stageg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

type repoSnapshot map[string]string

func snapshotRepo(repoPath string) (repoSnapshot, error) {
	repoPath = filepath.Clean(repoPath)
	result := repoSnapshot{}
	err := filepath.WalkDir(repoPath, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == repoPath {
			return nil
		}
		rel, relErr := filepath.Rel(repoPath, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if repoSnapshotSkipDir(entry.Name()) || repoSnapshotSkipPath(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || repoSnapshotSkipPath(rel) {
			return nil
		}
		sum, err := fileSHA256(path)
		if err != nil {
			return err
		}
		result[rel] = sum
		return nil
	})
	return result, err
}

func repoSnapshotDiff(before, after repoSnapshot) []string {
	seen := map[string]bool{}
	var changes []string
	for path, hash := range before {
		seen[path] = true
		if after[path] != hash {
			changes = append(changes, path)
		}
	}
	for path := range after {
		if !seen[path] {
			changes = append(changes, path)
		}
	}
	sort.Strings(changes)
	return changes
}

func repoSnapshotSkipDir(name string) bool {
	switch name {
	case ".git", ".next", ".nuxt", ".pytest_cache", ".venv", "__pycache__", "build", "coverage", "dist", "node_modules", "playwright-report", "test-results", "venv":
		return true
	default:
		return false
	}
}

func repoSnapshotSkipPath(path string) bool {
	base := filepath.Base(filepath.FromSlash(path))
	if strings.HasSuffix(base, ".pyc") || strings.HasSuffix(base, ".log") {
		return true
	}
	switch base {
	case ".coverage":
		return true
	default:
		return false
	}
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func repoChangedFinding(changes []string, sourcePath string) model.Finding {
	return model.Finding{
		Stage:      string(model.StageG),
		Severity:   "High",
		Title:      "Stage G modified repository source files",
		Rule:       "Browser E2E validation must write only p2r artifacts, never delivery package source.",
		Evidence:   fmt.Sprintf("changed files: %s", strings.Join(changes, ", ")),
		Impact:     "Runtime validation is no longer a read-only assessment of the submitted package.",
		MinimumFix: "Inspect the changed files, revert unintended source changes, and rerun Stage G.",
		SourcePath: sourcePath,
	}
}
