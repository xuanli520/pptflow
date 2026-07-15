package codex

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// SanitizeEnvironment carries only execution-neutral process values forward
// and overlays explicitly configured values.  Provider credentials, provider
// endpoints, homes, config roots, proxies, and custom CA paths are deliberately
// excluded from the ambient base: callers must pass them in configured so they
// are visible at the operation boundary instead of being inherited accidentally.
func SanitizeEnvironment(base []string, configured map[string]string) []string {
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
	keys := make([]string, 0, len(values))
	for canonical := range values {
		keys = append(keys, canonical)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(values))
	for _, canonical := range keys {
		value := values[canonical]
		key := casing[canonical]
		env = append(env, key+"="+value)
	}
	return withSystemBinOnPATH(env)
}

func allowedBaseEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	switch upper {
	case "PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC", "TEMP", "TMP", "TMPDIR", "LANG", "LC_ALL", "TZ", "TERM", "COLORTERM":
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
