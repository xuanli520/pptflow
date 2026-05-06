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
	return s.EnvWithNode(base, configured, "")
}

func (s Sandbox) EnvWithNode(base []string, configured map[string]string, nodePath string) []string {
	values := map[string]string{}
	casing := map[string]string{}
	for _, item := range base {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if !allowedBaseEnvKey(key) {
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
	env := make([]string, 0, len(values))
	for canonical, value := range values {
		key := casing[canonical]
		env = append(env, key+"="+value)
	}
	env = WithNodeOnPATH(env, nodePath)
	return withSystemBinOnPATH(env)
}

func allowedBaseEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	switch upper {
	case "PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC", "HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "CODEX_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "CODEX_API_KEY", "OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_ORG_ID", "OPENAI_ORGANIZATION", "OPENAI_PROJECT", "TEMP", "TMP", "TMPDIR", "LANG", "LC_ALL", "TZ", "TERM", "COLORTERM", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
		return true
	default:
		return strings.HasPrefix(upper, "LC_")
	}
}

var systemBinPaths = []string{"/usr/bin", "/usr/local/bin", "/bin"}

func withSystemBinOnPATH(env []string) []string {
	if runtime.GOOS == "windows" {
		return env
	}
	for _, dir := range systemBinPaths {
		env = withDirOnPATH(env, dir)
	}
	return env
}

func withDirOnPATH(env []string, dir string) []string {
	dir = filepath.Clean(dir)
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if canonicalEnvKey(key) == canonicalEnvKey("PATH") {
			if pathListContains(value, dir) {
				return env
			}
		}
	}
	// Not on PATH – append.
	for i, item := range env {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if canonicalEnvKey(key) == canonicalEnvKey("PATH") {
			env[i] = key + "=" + env[i][len(key)+1:] + string(os.PathListSeparator) + dir
			return env
		}
	}
	return append(env, "PATH="+dir)
}

func canonicalEnvKey(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}
