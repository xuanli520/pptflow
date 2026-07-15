package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/executor"
)

type Capability struct {
	Path                   string `json:"path"`
	ResolvedPath           string `json:"resolved_path,omitempty"`
	Version                string `json:"version,omitempty"`
	HasConfig              bool   `json:"has_config"`
	HasAppServer           bool   `json:"has_app_server"`
	NodePath               string `json:"node_path,omitempty"`
	PathPrependedForNode   bool   `json:"path_prepended_for_node"`
	AppServerHelpAvailable bool   `json:"app_server_help_available"`
	DetectionError         string `json:"detection_error,omitempty"`
	NodeDetectionMessage   string `json:"node_detection_message,omitempty"`
	OptionalMissingMessage string `json:"optional_missing_message,omitempty"`
}

// DetectCLI is a diagnostic helper that may discover Codex on PATH.  It is
// intentionally separate from InspectCLI: controlled runtimes must use an
// explicitly configured executable and never call this helper.
func DetectCLI(ctx context.Context, exec executor.CommandRunner, preferredPath string) Capability {
	if exec == nil {
		exec = executor.New()
	}
	path := strings.TrimSpace(preferredPath)
	if path == "" {
		found, err := exec.LookPath("codex")
		if err != nil {
			return Capability{DetectionError: err.Error()}
		}
		path = found
	}
	return inspectCLI(ctx, exec, path)
}

// InspectCLI probes one explicitly configured Codex executable.  It never
// searches PATH and rejects relative or non-executable paths before invoking
// the command.  It is suitable for a composition that has already selected
// and attested the executable.
func InspectCLI(ctx context.Context, exec executor.CommandRunner, commandPath string) Capability {
	if exec == nil {
		exec = executor.New()
	}
	return inspectExplicitCLI(ctx, exec, commandPath, os.Environ(), true)
}

// InspectCLIWithEnvironment probes an explicitly configured executable using
// exactly the supplied environment.  It does not discover a Node runtime or
// alter PATH: a controlled composition must provide every process dependency
// it intends the executable to use.
func InspectCLIWithEnvironment(ctx context.Context, exec executor.CommandRunner, commandPath string, env []string) Capability {
	if exec == nil {
		exec = executor.New()
	}
	return inspectExplicitCLI(ctx, exec, commandPath, env, false)
}

func inspectExplicitCLI(ctx context.Context, exec executor.CommandRunner, commandPath string, env []string, discoverNode bool) Capability {
	path := strings.TrimSpace(commandPath)
	if path == "" {
		return Capability{DetectionError: "codex executable path is required"}
	}
	if !filepath.IsAbs(path) {
		return Capability{Path: path, DetectionError: "codex executable path must be absolute"}
	}
	info, err := os.Stat(path)
	if err != nil {
		return Capability{Path: path, DetectionError: fmt.Sprintf("stat codex executable: %v", err)}
	}
	if info.IsDir() || !isExecutableFile(path) {
		return Capability{Path: path, DetectionError: "codex executable path is not an executable file"}
	}
	return inspectCLIWithEnvironment(ctx, exec, path, env, discoverNode)
}

func inspectCLI(ctx context.Context, exec executor.CommandRunner, path string) Capability {
	return inspectCLIWithEnvironment(ctx, exec, path, os.Environ(), true)
}

func inspectCLIWithEnvironment(ctx context.Context, exec executor.CommandRunner, path string, env []string, discoverNode bool) Capability {
	cap := Capability{}
	cap.Path = path
	cap.ResolvedPath = resolvePath(path)
	probeEnv := append([]string(nil), env...)
	if discoverNode {
		cap.NodePath, cap.PathPrependedForNode, cap.NodeDetectionMessage = detectNodeForCodex(cap.Path, cap.ResolvedPath, environmentValue(probeEnv, "PATH"))
		if cap.NodePath != "" {
			probeEnv = WithNodeOnPATH(probeEnv, cap.NodePath)
		}
	} else {
		cap.NodeDetectionMessage = "node resolution is delegated to the configured process environment"
	}
	version := exec.Run(ctx, 5*time.Second, "", probeEnv, path, "--version")
	cap.Version = firstLine(firstNonEmpty(version.Stdout, version.Stderr))
	if version.Err != nil && cap.DetectionError == "" {
		cap.DetectionError = version.Err.Error()
	}
	help := exec.Run(ctx, 5*time.Second, "", probeEnv, path, "app-server", "--help")
	helpText := help.Stdout + "\n" + help.Stderr
	if strings.TrimSpace(helpText) != "" && help.Err == nil {
		cap.AppServerHelpAvailable = true
	}
	ApplyAppServerHelp(&cap, helpText)
	cap.OptionalMissingMessage = optionalMissingMessage(cap)
	if help.Err != nil && cap.DetectionError == "" {
		cap.DetectionError = help.Err.Error()
	}
	return cap
}

func ApplyAppServerHelp(cap *Capability, help string) {
	cap.HasAppServer = hasHelpToken(help, "--listen")
	cap.HasConfig = hasHelpToken(help, "--config") || hasHelpToken(help, "-c,")
}

func ValidateAppServerCapability(cap Capability) error {
	if strings.TrimSpace(cap.Path) == "" {
		return fmt.Errorf("codex executable not found")
	}
	if !cap.HasAppServer {
		return fmt.Errorf("codex CLI does not expose app-server; interactive agent turns require codex app-server")
	}
	if !cap.HasConfig {
		return fmt.Errorf("codex app-server does not expose -c/--config; cannot configure approval policy and sandbox mode")
	}
	return nil
}

// ValidateControlledAppServerCapability validates a capability returned by
// InspectCLI.  It additionally rejects any probe failure so a caller cannot
// accidentally execute an unverified or PATH-discovered command.
func ValidateControlledAppServerCapability(cap Capability) error {
	if strings.TrimSpace(cap.DetectionError) != "" {
		return fmt.Errorf("inspect codex executable: %s", cap.DetectionError)
	}
	if !filepath.IsAbs(cap.Path) {
		return fmt.Errorf("codex executable path must be absolute")
	}
	return ValidateAppServerCapability(cap)
}

func WithNodeOnPATH(env []string, nodePath string) []string {
	nodePath = strings.TrimSpace(nodePath)
	if nodePath == "" {
		return env
	}
	nodeDir := filepath.Dir(nodePath)
	if nodeDir == "." || nodeDir == string(filepath.Separator) {
		return env
	}
	result := append([]string{}, env...)
	for i, item := range result {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if canonicalEnvKey(key) != canonicalEnvKey("PATH") {
			continue
		}
		if pathListContains(value, nodeDir) {
			return result
		}
		result[i] = key + "=" + nodeDir + string(os.PathListSeparator) + value
		return result
	}
	return append(result, "PATH="+nodeDir)
}

func optionalMissingMessage(cap Capability) string {
	var missing []string
	if !cap.HasConfig {
		missing = append(missing, "-c/--config")
	}
	if !cap.HasAppServer {
		missing = append(missing, "app-server")
	}
	if len(missing) == 0 {
		return ""
	}
	return "optional flags unavailable: " + strings.Join(missing, ", ")
}

func detectNodeForCodex(path, resolvedPath, basePATH string) (string, bool, string) {
	candidates := nodeCandidatesForCodex(path, resolvedPath)
	for _, candidate := range candidates {
		if isExecutableFile(candidate) {
			dir := filepath.Dir(candidate)
			return candidate, !pathListContains(basePATH, dir), "node found near Codex CLI"
		}
	}
	if found, err := executor.New().LookPath("node"); err == nil && found != "" {
		return found, false, "node found on PATH"
	}
	return "", false, "node was not found near Codex CLI or on PATH"
}

func nodeCandidatesForCodex(path, resolvedPath string) []string {
	seen := map[string]bool{}
	var candidates []string
	add := func(path string) {
		path = filepath.Clean(path)
		if path == "." || seen[path] {
			return
		}
		seen[path] = true
		candidates = append(candidates, path)
	}
	nodeName := exeName("node")
	for _, codexPath := range []string{path, resolvedPath} {
		if strings.TrimSpace(codexPath) == "" {
			continue
		}
		dir := filepath.Dir(codexPath)
		add(filepath.Join(dir, nodeName))
		add(filepath.Join(filepath.Dir(dir), "bin", nodeName))
	}
	if appData := os.Getenv("APPDATA"); appData != "" {
		add(filepath.Join(appData, "npm", nodeName))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, ".volta", "bin", nodeName))
		for _, pattern := range []string{
			filepath.Join(home, ".nvm", "versions", "node", "*", "bin", nodeName),
			filepath.Join(home, ".fnm", "node-versions", "*", "installation", "bin", nodeName),
		} {
			matches, _ := filepath.Glob(pattern)
			sort.Strings(matches)
			for i := len(matches) - 1; i >= 0; i-- {
				add(matches[i])
			}
		}
	}
	for _, candidate := range []string{
		"/usr/local/bin/node",
		"/usr/bin/node",
		"/opt/homebrew/bin/node",
		`C:\Program Files\nodejs\node.exe`,
	} {
		add(candidate)
	}
	return candidates
}

func hasHelpToken(help, token string) bool {
	return strings.Contains(help, token)
}

func resolvePath(path string) string {
	if path == "" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return resolved
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}

func pathListContains(list, dir string) bool {
	dir = filepath.Clean(dir)
	for _, item := range filepath.SplitList(list) {
		if filepath.Clean(item) == dir {
			return true
		}
	}
	return false
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if idx := strings.Index(value, "\n"); idx >= 0 {
		return strings.TrimSpace(value[:idx])
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func environmentValue(env []string, key string) string {
	for _, item := range env {
		name, value, ok := strings.Cut(item, "=")
		if ok && strings.EqualFold(strings.TrimSpace(name), key) {
			return value
		}
	}
	return ""
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
