package pathutil

import (
	"path/filepath"
	"strings"
)

func AbsClean(path string) string {
	cleaned := filepath.Clean(path)
	if abs, err := filepath.Abs(cleaned); err == nil {
		return abs
	}
	return cleaned
}

func RelUnderRoot(root, path string) (string, bool) {
	root = AbsClean(root)
	path = AbsClean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.Clean(rel), true
}

func PathWithin(path, parent string) bool {
	_, ok := RelUnderRoot(parent, path)
	return ok
}
