package taskpolicy

import (
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

var allowedFiles = map[string]struct{}{
	"instruction.md":                  {},
	"task.toml":                       {},
	"tests_analysis.md":               {},
	"environment/Dockerfile":          {},
	"environment/docker-compose.yaml": {},
	"solution/solve.sh":               {},
	"tests/test.sh":                   {},
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
