package preflight

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/executor"
)

type CheckResult struct {
	Checks []Check `json:"checks"`
}

type Check struct {
	Name    string   `json:"name"`
	Status  string   `json:"status"`
	Path    string   `json:"path,omitempty"`
	Version string   `json:"version,omitempty"`
	Message string   `json:"message,omitempty"`
	Stages  []string `json:"stages,omitempty"`
}

func Run(ctx context.Context, exec executor.Runner, cfg config.Config) CheckResult {
	var result CheckResult
	node := checkBinary(ctx, exec, "node", []string{"--version"}, nodeCandidates(), []string{"D", "E"}, "Node.js is required by Codex CLI.")
	result.Checks = append(result.Checks, node)
	result.Checks = append(result.Checks, checkBinary(ctx, exec, "docker", []string{"--version"}, dockerCandidates(), []string{"B"}, "Docker is required for Stage B runtime evidence."))
	result.Checks = append(result.Checks, checkBinary(ctx, exec, "bash", []string{"--version"}, bashCandidates(), []string{"C"}, "bash is required to run repo/run_tests.sh on the host."))
	result.Checks = append(result.Checks, checkPython(ctx, exec))
	codex := checkBinary(ctx, exec, "codex", []string{"--version"}, codexCandidates(), []string{"D", "E"}, "Codex CLI is required for static review stages.")
	if node.Status == "missing" {
		codex.Status = "missing"
		codex.Message = "Node.js is missing; install Node.js before running Codex CLI."
	}
	if codex.Status == "ok" {
		flagCheck := checkCodexFlags(ctx, exec, codex.Path)
		if flagCheck.Status != "ok" {
			codex.Status = flagCheck.Status
			codex.Message = flagCheck.Message
		}
	}
	if err := validateExtraArgs(cfg.Codex.ExtraArgs); err != "" {
		codex.Status = "missing"
		codex.Message = err
	}
	result.Checks = append(result.Checks, codex)
	return result
}

func (r CheckResult) BlockingCheck(stage string) (Check, bool) {
	for _, check := range r.Checks {
		if check.Status == "ok" || check.Status == "degraded" {
			continue
		}
		for _, affected := range check.Stages {
			if affected == stage {
				return check, true
			}
		}
	}
	return Check{}, false
}

func checkBinary(ctx context.Context, exec executor.Runner, name string, versionArgs []string, candidates []string, stages []string, missing string) Check {
	path, err := exec.LookPath(name)
	if err != nil || path == "" {
		path = firstExecutable(candidates)
	}
	if path == "" {
		return Check{Name: name, Status: "missing", Message: missing + " Searched PATH and known install locations.", Stages: stages}
	}
	check := Check{Name: name, Status: "ok", Path: path, Stages: stages}
	if len(versionArgs) > 0 {
		out := exec.Run(ctx, 5*time.Second, "", nil, path, versionArgs...)
		version := strings.TrimSpace(out.Stdout)
		if version == "" {
			version = strings.TrimSpace(out.Stderr)
		}
		check.Version = firstLine(version)
		if out.Err != nil {
			check.Status = "degraded"
			check.Message = out.Err.Error()
		}
	}
	return check
}

func checkPython(ctx context.Context, exec executor.Runner) Check {
	for _, name := range []string{"python", "python3", "uv"} {
		path, err := exec.LookPath(name)
		if err == nil && path != "" {
			args := []string{"--version"}
			if name == "uv" {
				args = []string{"--version"}
			}
			out := exec.Run(ctx, 5*time.Second, "", nil, path, args...)
			version := strings.TrimSpace(firstNonEmpty(out.Stdout, out.Stderr))
			return Check{Name: "python", Status: "ok", Path: path, Version: firstLine(version), Stages: []string{"A"}}
		}
	}
	path := firstExecutable(pythonCandidates())
	if path != "" {
		return Check{Name: "python", Status: "ok", Path: path, Stages: []string{"A"}}
	}
	return Check{Name: "python", Status: "missing", Message: "python/python3 or uv is required for Stage A scripts.", Stages: []string{"A"}}
}

func checkCodexFlags(ctx context.Context, exec executor.Runner, path string) Check {
	out := exec.Run(ctx, 5*time.Second, "", nil, path, "exec", "--help")
	help := out.Stdout + "\n" + out.Stderr
	missing := []string{}
	for _, flag := range []string{"--ask-for-approval", "--sandbox", "--cd", "--ephemeral"} {
		if !strings.Contains(help, flag) {
			missing = append(missing, flag)
		}
	}
	if len(missing) > 0 {
		return Check{Name: "codex_flags", Status: "missing", Message: "Codex CLI is missing required exec flags: " + strings.Join(missing, ", "), Stages: []string{"D", "E"}}
	}
	return Check{Name: "codex_flags", Status: "ok"}
}

func validateExtraArgs(args []string) string {
	dangerous := map[string]bool{
		"--sandbox":          true,
		"--ask-for-approval": true,
		"-a":                 true,
		"--cd":               true,
		"-C":                 true,
		"--dangerously-bypass-approvals-and-sandbox": true,
		"--add-dir": true,
	}
	for _, arg := range args {
		key := arg
		if before, _, ok := strings.Cut(arg, "="); ok {
			key = before
		}
		if dangerous[key] {
			return "codex.extra_args contains unsafe boundary-changing argument: " + key
		}
	}
	return ""
}

func dockerCandidates() []string {
	return []string{
		`C:\Program Files\Docker\Docker\resources\bin\docker.exe`,
		`C:\Program Files\Rancher Desktop\resources\resources\win32\bin\docker.exe`,
		"/usr/bin/docker",
		"/usr/local/bin/docker",
	}
}

func codexCandidates() []string {
	var candidates []string
	if appData := os.Getenv("APPDATA"); appData != "" {
		candidates = append(candidates, filepath.Join(appData, "npm", exeName("codex")))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".nvm"),
			filepath.Join(home, ".fnm"),
			filepath.Join(home, ".volta", "bin", exeName("codex")),
		)
	}
	return append(candidates, "/usr/local/bin/codex", "/usr/bin/codex")
}

func nodeCandidates() []string {
	return []string{
		`C:\Program Files\nodejs\node.exe`,
		"/usr/local/bin/node",
		"/usr/bin/node",
	}
}

func bashCandidates() []string {
	return []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files\Git\usr\bin\bash.exe`,
		`C:\msys64\usr\bin\bash.exe`,
		"/usr/bin/bash",
		"/bin/bash",
	}
}

func pythonCandidates() []string {
	var candidates []string
	if localApp := os.Getenv("LOCALAPPDATA"); localApp != "" {
		candidates = append(candidates, filepath.Join(localApp, "Programs", "Python"))
	}
	return append(candidates, "/usr/bin/python3", "/usr/local/bin/python3")
}

func firstExecutable(candidates []string) string {
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
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
		return name + ".cmd"
	}
	return name
}
