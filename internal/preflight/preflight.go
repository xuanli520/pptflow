package preflight

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/codex"
	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
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
	Details any      `json:"details,omitempty"`
}

func Run(ctx context.Context, exec executor.CommandRunner, cfg config.Config) CheckResult {
	var result CheckResult
	node := checkBinary(ctx, exec, "node", []string{"--version"}, nodeCandidates(), nil, "Node.js is required by Codex CLI.")
	result.Checks = append(result.Checks, node)
	result.Checks = append(result.Checks, checkBinary(ctx, exec, "docker", []string{"--version"}, dockerCandidates(), []string{string(model.StageB)}, "Docker is required for Stage B runtime evidence."))
	if stageCPreflightRequiresHostBash(cfg.Pipeline.StageC) {
		result.Checks = append(result.Checks, checkBinary(ctx, exec, "bash", []string{"--version"}, bashCandidates(), []string{string(model.StageC)}, "bash is required to run repo/run_tests.sh on the host."))
	}
	result.Checks = append(result.Checks, checkPython(ctx, exec))
	codexCheck := checkCodex(ctx, exec, cfg)
	if node.Status == "missing" {
		if cap, ok := codexCheck.Details.(codex.Capability); ok && cap.NodePath != "" {
			node.Status = "degraded"
			node.Message = "node is not on PATH, but Codex-local Node.js will be prepended for static review."
			node.Path = cap.NodePath
		}
	}
	result.Checks = append(result.Checks, codexCheck)
	return result
}

func stageCPreflightRequiresHostBash(cfg config.StageCConfig) bool {
	switch strings.ToLower(strings.TrimSpace(cfg.Execution)) {
	case "isolated":
		return false
	case "auto":
		return strings.TrimSpace(cfg.RunnerImage) == ""
	default:
		return true
	}
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

func checkBinary(ctx context.Context, exec executor.CommandRunner, name string, versionArgs []string, candidates []string, stages []string, missing string) Check {
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

func checkPython(ctx context.Context, exec executor.CommandRunner) Check {
	for _, name := range []string{"python", "python3", "uv"} {
		path, err := exec.LookPath(name)
		if err == nil && path != "" {
			args := []string{"--version"}
			if name == "uv" {
				args = []string{"--version"}
			}
			out := exec.Run(ctx, 5*time.Second, "", nil, path, args...)
			version := strings.TrimSpace(firstNonEmpty(out.Stdout, out.Stderr))
			return Check{Name: "python", Status: "ok", Path: path, Version: firstLine(version), Stages: []string{string(model.StageA)}}
		}
	}
	path := firstExecutable(pythonCandidates())
	if path != "" {
		return Check{Name: "python", Status: "ok", Path: path, Stages: []string{string(model.StageA)}}
	}
	return Check{Name: "python", Status: "missing", Message: "python/python3 or uv is required for Stage A scripts.", Stages: []string{string(model.StageA)}}
}

func checkCodex(ctx context.Context, exec executor.CommandRunner, cfg config.Config) Check {
	path, err := exec.LookPath("codex")
	if err != nil || path == "" {
		path = firstExecutable(codexCandidates())
	}
	if path == "" {
		return Check{Name: "codex", Status: "missing", Message: "Codex CLI is required for static review stages. Searched PATH and known install locations.", Stages: staticReviewStages()}
	}
	capability := codex.DetectCLI(ctx, exec, path)
	check := Check{Name: "codex", Status: "ok", Path: capability.Path, Version: capability.Version, Stages: staticReviewStages(), Details: capability}
	if err := validateExtraArgs(cfg.Codex.ExtraArgs); err != "" {
		check.Status = "missing"
		check.Message = err
		return check
	}
	if err := codex.ValidateAppServerCapability(capability); err != nil {
		check.Status = "missing"
		check.Message = err.Error()
		return check
	}
	if capability.OptionalMissingMessage != "" {
		check.Status = "degraded"
		var messages []string
		for _, message := range []string{capability.OptionalMissingMessage} {
			if strings.TrimSpace(message) != "" {
				messages = append(messages, message)
			}
		}
		check.Message = strings.Join(messages, "; ")
	}
	return check
}

func staticReviewStages() []string {
	var stages []string
	for _, spec := range model.AllStageSpecs() {
		if spec.Static && spec.ID != model.StageA {
			stages = append(stages, string(spec.ID))
		}
	}
	return stages
}

func validateExtraArgs(args []string) string {
	if _, err := codex.ValidateAppServerExtraArgs(args); err != nil {
		return err.Error()
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
