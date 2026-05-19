package pipeline

import (
	"path/filepath"
)

func qaArtifactName(name string) string {
	return name
}

func qaArtifactPath(root, name string) string {
	return filepath.Join(root, name)
}
