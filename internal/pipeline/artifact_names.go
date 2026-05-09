package pipeline

import (
	"path/filepath"
	"strings"
)

const qaArtifactPrefix = "QA_"

func qaArtifactName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	dir, base := filepath.Split(name)
	if strings.HasPrefix(base, qaArtifactPrefix) {
		return filepath.Join(dir, base)
	}
	return filepath.Join(dir, qaArtifactPrefix+base)
}

func qaArtifactPath(root, name string) string {
	return filepath.Join(root, qaArtifactName(name))
}

func qaArtifactCandidates(names ...string) []string {
	seen := map[string]bool{}
	var result []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		result = append(result, name)
	}
	for _, name := range names {
		add(qaArtifactName(name))
		add(name)
	}
	return result
}
