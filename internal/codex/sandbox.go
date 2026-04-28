package codex

import (
	"os"
	"path/filepath"
)

type Sandbox struct {
	ProjectPath  string
	ArtifactRoot string
	Home         string
}

func NewSandbox(projectPath, artifactRoot, stage string) (Sandbox, error) {
	home := filepath.Join(artifactRoot, ".codex-home-"+stage)
	if err := os.RemoveAll(home); err != nil {
		return Sandbox{}, err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return Sandbox{}, err
	}
	return Sandbox{ProjectPath: projectPath, ArtifactRoot: artifactRoot, Home: home}, nil
}

func (s Sandbox) Env(base []string) []string {
	env := append([]string{}, base...)
	env = append(env, "HOME="+s.Home, "USERPROFILE="+s.Home)
	return env
}
