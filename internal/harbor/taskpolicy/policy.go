package taskpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// LegacyTaskDigestPrefix is the historical, unversioned-on-the-wire task
	// digest form. It is retained solely to read and verify V1 evidence.
	LegacyTaskDigestPrefix = "sha256:"

	// TaskDigestV2Scheme identifies a digest of a managed Harbor task revision
	// snapshot. V2 evidence must use TaskDigestV2Prefix, never the legacy
	// sha256: form.
	TaskDigestV2Scheme = "harbor.task.v2:sha256"
	TaskDigestV2Prefix = TaskDigestV2Scheme + ":"
)

var canonicalFiles = []CanonicalFile{
	{Path: "instruction.md", Mode: 0o644, Required: true},
	{Path: "task.toml", Mode: 0o644, Required: true},
	{Path: "tests_analysis.md", Mode: 0o644, Required: true},
	{Path: "environment/Dockerfile", Mode: 0o644, Environment: true},
	{Path: "environment/docker-compose.yaml", Mode: 0o644, Environment: true},
	{Path: "solution/solve.sh", Mode: 0o755, Required: true},
	{Path: "tests/test.sh", Mode: 0o755, Required: true},
}

var allowedFiles = func() map[string]CanonicalFile {
	files := make(map[string]CanonicalFile, len(canonicalFiles))
	for _, file := range canonicalFiles {
		files[file.Path] = file
	}
	return files
}()

// CanonicalFile defines one path in the V2 Harbor task snapshot policy. Mode
// is the policy-assigned mode used in the V2 manifest, not the source mode.
type CanonicalFile struct {
	Path        string
	Mode        os.FileMode
	Required    bool
	Environment bool
}

var legacyIdentifiers = []string{
	"pptflow",
	"promptflow",
	"image2",
}

func IsAllowedFile(path string) bool {
	path = filepath.ToSlash(filepath.Clean(path))
	_, ok := allowedFiles[path]
	return ok
}

// CanonicalFiles returns a copy of the versioned V2 file policy in its
// declaration order. The digest implementation separately sorts paths before
// writing its manifest. Callers cannot mutate the policy through this slice.
func CanonicalFiles() []CanonicalFile {
	files := make([]CanonicalFile, len(canonicalFiles))
	copy(files, canonicalFiles)
	return files
}

// CanonicalMode returns the policy-assigned mode for a V2 snapshot file. The
// source file's mode deliberately does not affect the canonical digest.
func CanonicalMode(path string) (os.FileMode, bool) {
	path = filepath.ToSlash(filepath.Clean(path))
	file, ok := allowedFiles[path]
	if !ok {
		return 0, false
	}
	return file.Mode, true
}

// ContainsLegacyDomain matches complete legacy identifiers. Substring matching
// is intentionally avoided because ordinary terms such as "representation"
// and "slider" contain legacy words without referring to the legacy domain.
func ContainsLegacyDomain(value string) bool {
	lower := strings.ToLower(value)
	for _, identifier := range legacyIdentifiers {
		for start := 0; start < len(lower); {
			rel := strings.Index(lower[start:], identifier)
			if rel < 0 {
				break
			}
			matchStart := start + rel
			matchEnd := matchStart + len(identifier)
			if identifierBoundary(lower, matchStart, matchEnd) {
				return true
			}
			start = matchStart + 1
		}
	}
	return false
}

func identifierBoundary(value string, start, end int) bool {
	return (start == 0 || !identifierRuneBefore(value, start)) &&
		(end == len(value) || !identifierRuneAt(value, end))
}

func identifierRuneBefore(value string, pos int) bool {
	r, _ := utf8.DecodeLastRuneInString(value[:pos])
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func identifierRuneAt(value string, pos int) bool {
	r, _ := utf8.DecodeRuneInString(value[pos:])
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}
