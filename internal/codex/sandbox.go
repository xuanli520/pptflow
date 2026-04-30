package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Sandbox struct {
	ProjectPath  string
	ArtifactRoot string
	Home         string
}

func NewSandbox(projectPath, artifactRoot, stage string) (Sandbox, error) {
	root, err := filepath.Abs(filepath.Clean(artifactRoot))
	if err != nil {
		return Sandbox{}, err
	}
	if root == "" || root == "." || root == string(filepath.Separator) {
		return Sandbox{}, fmt.Errorf("refusing to prepare codex home under unsafe artifact root %q", artifactRoot)
	}
	home := filepath.Join(root, ".codex-home-"+stage)
	if err := validateHome(root, home); err != nil {
		return Sandbox{}, err
	}
	if err := os.RemoveAll(home); err != nil {
		return Sandbox{}, err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return Sandbox{}, err
	}
	return Sandbox{ProjectPath: projectPath, ArtifactRoot: root, Home: home}, nil
}

func validateHome(root, home string) error {
	base := filepath.Base(home)
	if !strings.HasPrefix(base, ".codex-home-") {
		return fmt.Errorf("refusing to remove non-codex home directory %s", home)
	}
	rel, err := filepath.Rel(root, home)
	if err != nil {
		return err
	}
	if rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return fmt.Errorf("refusing to remove codex home outside artifact root: %s", home)
	}
	return nil
}

func (s Sandbox) Env(base []string, configured map[string]string) []string {
	values := map[string]string{}
	casing := map[string]string{}
	for _, item := range base {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		canonical := canonicalEnvKey(key)
		values[canonical] = value
		casing[canonical] = key
	}
	for key, value := range configured {
		canonical := canonicalEnvKey(key)
		values[canonical] = value
		casing[canonical] = key
	}
	for key, value := range map[string]string{
		"HOME":        s.Home,
		"USERPROFILE": s.Home,
		"CODEX_HOME":  s.Home,
	} {
		canonical := canonicalEnvKey(key)
		values[canonical] = value
		casing[canonical] = key
	}
	env := make([]string, 0, len(values))
	for canonical, value := range values {
		key := casing[canonical]
		env = append(env, key+"="+value)
	}
	return env
}

func canonicalEnvKey(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}
