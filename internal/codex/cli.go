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

func DetectCLI(ctx context.Context, exec executor.CommandRunner, preferredPath string) Capability {
	cap := Capability{}
	path := strings.TrimSpace(preferredPath)
	if path == "" {
		found, err := exec.LookPath("codex")
		if err != nil {
			cap.DetectionError = err.Error()
			return cap
		}
		path = found
	}
	cap.Path = path
	cap.ResolvedPath = resolvePath(path)
	cap.NodePath, cap.PathPrependedForNode, cap.NodeDetectionMessage = detectNodeForCodex(cap.Path, cap.ResolvedPath, os.Getenv("PATH"))
	env := os.Environ()
	if cap.NodePath != "" {
		env = WithNodeOnPATH(env, cap.NodePath)
	}
	version := exec.Run(ctx, 5*time.Second, "", env, path, "--version")
	cap.Version = firstLine(firstNonEmpty(version.Stdout, version.Stderr))
	if version.Err != nil && cap.DetectionError == "" {
		cap.DetectionError = version.Err.Error()
	}
	help := exec.Run(ctx, 5*time.Second, "", env, path, "app-server", "--help")
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
		return fmt.Errorf("codex CLI does not expose app-server; static review requires codex app-server turn/steer")
	}
	if !cap.HasConfig {
		return fmt.Errorf("codex app-server does not expose -c/--config; cannot configure approval policy and sandbox mode")
	}
	return nil
}

func ValidateAppServerExtraArgs(args []string) ([]string, error) {
	modelSeen := false
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		switch {
		case arg == "--model" || arg == "-m":
			if modelSeen {
				return nil, fmt.Errorf("codex.extra_args contains duplicate model selection")
			}
			if i+1 >= len(args) {
				return nil, missingAppServerModelValueError(arg)
			}
			if err := validateAppServerModelValue(arg, args[i+1]); err != nil {
				return nil, err
			}
			modelSeen = true
			i++
		case strings.HasPrefix(arg, "--model="), strings.HasPrefix(arg, "-m="):
			if modelSeen {
				return nil, fmt.Errorf("codex.extra_args contains duplicate model selection")
			}
			flag, value, _ := strings.Cut(arg, "=")
			if err := validateAppServerModelValue(flag, value); err != nil {
				return nil, err
			}
			modelSeen = true
		default:
			return nil, fmt.Errorf("codex.extra_args contains unsupported app-server argument: %s", arg)
		}
	}
	return append([]string{}, args...), nil
}

func validateAppServerModelValue(flag, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return missingAppServerModelValueError(flag)
	}
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("codex.extra_args %s requires a model value, got option-like value %q", flag, value)
	}
	return nil
}

func missingAppServerModelValueError(flag string) error {
	return fmt.Errorf("codex.extra_args %s requires a model value", flag)
}

func AppServerModelFromArgs(args []string) string {
	for i, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "--model" || arg == "-m" {
			if i+1 < len(args) {
				return strings.TrimSpace(args[i+1])
			}
			return ""
		}
		for _, prefix := range []string{"--model=", "-m="} {
			if strings.HasPrefix(arg, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(arg, prefix))
			}
		}
	}
	return ""
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

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
