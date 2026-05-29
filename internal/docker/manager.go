package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/executor"
)

func ComposeProjectName(prefix, taskID, runID string) string {
	cleanPrefix := dockerToken(prefix)
	if cleanPrefix == "" {
		cleanPrefix = "p2rqa"
	}
	hash := shortHash(taskID + "\x00" + runID)
	value := cleanPrefix + "_" + dockerToken(taskID) + "_" + dockerToken(runID) + "_" + hash
	if len(value) <= 63 {
		return value
	}
	suffix := "_" + hash
	keep := 63 - len(suffix)
	if keep < len(cleanPrefix)+1 {
		return strings.TrimRight(cleanPrefix[:min(keep, len(cleanPrefix))], "_-") + suffix
	}
	value = strings.TrimRight(value[:keep], "_-") + suffix
	return value
}

type CleanupSummary struct {
	Status         string   `json:"status"`
	ComposeFile    string   `json:"compose_file,omitempty"`
	ComposeFiles   []string `json:"compose_files,omitempty"`
	EnvFiles       []string `json:"env_files,omitempty"`
	ComposeProject string   `json:"compose_project,omitempty"`
	WorkDir        string   `json:"work_dir,omitempty"`
	Command        string   `json:"command,omitempty"`
	ExitCode       int      `json:"exit_code,omitempty"`
	Stdout         string   `json:"stdout,omitempty"`
	Stderr         string   `json:"stderr,omitempty"`
	Error          string   `json:"error,omitempty"`
	ManualCommand  string   `json:"manual_command,omitempty"`
	Verification   string   `json:"verification,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
}

type ComposeProject struct {
	Name         string            `json:"name"`
	WorkDir      string            `json:"work_dir,omitempty"`
	ComposeFiles []string          `json:"compose_files,omitempty"`
	ContainerIDs []string          `json:"container_ids,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

func CleanupComposeProject(ctx context.Context, exec executor.CommandRunner, cfg config.DockerConfig, composeFile, projectName, workDir string) CleanupSummary {
	return CleanupComposeProjectFiles(ctx, exec, cfg, composeFilesFromLegacy(composeFile, nil), projectName, workDir)
}

func CleanupComposeProjectFiles(ctx context.Context, exec executor.CommandRunner, cfg config.DockerConfig, composeFiles []string, projectName, workDir string) CleanupSummary {
	return CleanupComposeProjectFilesWithEnvFiles(ctx, exec, cfg, composeFiles, nil, projectName, workDir)
}

func CleanupComposeProjectFilesWithEnvFiles(ctx context.Context, exec executor.CommandRunner, cfg config.DockerConfig, composeFiles, envFiles []string, projectName, workDir string) CleanupSummary {
	composeFiles = normalizeComposeFiles(composeFiles)
	envFiles = normalizeComposeFiles(envFiles)
	summary := CleanupSummary{Status: "not_applicable", ComposeFiles: composeFiles, EnvFiles: envFiles, ComposeProject: projectName, WorkDir: workDir}
	if len(composeFiles) > 0 {
		summary.ComposeFile = composeFiles[0]
	}
	if strings.TrimSpace(projectName) == "" {
		summary.Warnings = append(summary.Warnings, "compose project is empty")
		return summary
	}
	args := CleanupComposeArgsFilesWithProjectDirAndEnvFiles(cfg, composeFiles, envFiles, projectName, workDir)
	result := exec.Run(ctx, 2*time.Minute, workDir, nil, "docker", args...)
	summary.Status = "ok"
	summary.Command = result.Command
	summary.ExitCode = result.ExitCode
	summary.Stdout = result.Stdout
	summary.Stderr = result.Stderr
	summary.ManualCommand = CommandLine("docker", args)
	if result.Err != nil {
		summary.Status = "failed"
		summary.Error = result.Err.Error()
		return summary
	}
	psArgs := ComposeCommandArgsWithProjectDirAndEnvFiles(composeFiles, workDir, projectName, envFiles, "ps", "-q")
	verify := exec.Run(ctx, 30*time.Second, workDir, nil, "docker", psArgs...)
	summary.Verification = strings.TrimSpace(verify.Stdout + "\n" + verify.Stderr)
	if strings.TrimSpace(verify.Stdout) != "" {
		summary.Status = "failed"
		summary.Warnings = append(summary.Warnings, "compose ps still reports containers after cleanup")
	}
	if cfg.CleanupBuildCache {
		until := strings.TrimSpace(cfg.BuildCachePruneUntil)
		if until == "" {
			until = "24h"
		}
		prune := exec.Run(ctx, 2*time.Minute, "", nil, "docker", "builder", "prune", "--force", "--filter", "until="+until)
		if prune.Err != nil {
			summary.Warnings = append(summary.Warnings, "docker builder prune failed: "+prune.Err.Error())
		}
	}
	return summary
}

func ComposeProjectRunning(ctx context.Context, exec executor.CommandRunner, composeFiles []string, projectName, workDir string) (bool, error) {
	return IsRunning(ctx, exec, composeFiles, projectName, workDir)
}

func IsRunning(ctx context.Context, exec executor.CommandRunner, composeFiles []string, projectName, workDir string) (bool, error) {
	return IsRunningWithEnvFiles(ctx, exec, composeFiles, nil, projectName, workDir)
}

func IsRunningWithEnvFiles(ctx context.Context, exec executor.CommandRunner, composeFiles, envFiles []string, projectName, workDir string) (bool, error) {
	composeFiles = normalizeComposeFiles(composeFiles)
	if strings.TrimSpace(projectName) == "" {
		return false, nil
	}
	args := ComposeCommandArgsWithProjectDirAndEnvFiles(composeFiles, workDir, projectName, envFiles, "ps", "-q")
	result := exec.Run(ctx, 10*time.Second, workDir, nil, "docker", args...)
	if result.Err != nil {
		return false, result.Err
	}
	return strings.TrimSpace(result.Stdout) != "", nil
}

func GetFrontendURL(ctx context.Context, exec executor.CommandRunner, composeFiles []string, projectName, workDir string) (string, error) {
	return GetFrontendURLWithEnvFiles(ctx, exec, composeFiles, nil, projectName, workDir)
}

func GetFrontendURLWithEnvFiles(ctx context.Context, exec executor.CommandRunner, composeFiles, envFiles []string, projectName, workDir string) (string, error) {
	composeFiles = normalizeComposeFiles(composeFiles)
	if strings.TrimSpace(projectName) == "" {
		return "", nil
	}
	args := ComposeCommandArgsWithProjectDirAndEnvFiles(composeFiles, workDir, projectName, envFiles, "ps", "--format", "json")
	result := exec.Run(ctx, 10*time.Second, workDir, nil, "docker", args...)
	if result.Err != nil {
		return "", result.Err
	}
	mappings, services := ParseComposePS(result.Stdout)
	return firstFrontendURL(mappings, services), nil
}

func ListAllProjects(ctx context.Context, exec executor.CommandRunner, cfg config.DockerConfig) ([]ComposeProject, error) {
	projectsByName := map[string]*ComposeProject{}
	seenContainers := map[string]map[string]bool{}
	var errs []error
	for _, query := range gcListQueries("container", cfg) {
		result := exec.Run(ctx, time.Minute, "", nil, "docker", query.Args...)
		if result.Err != nil {
			errs = append(errs, result.Err)
			continue
		}
		for _, candidate := range parseGCCandidates(result.Stdout) {
			if query.Reason == "compose_project_prefix" && !composeProjectOwned(candidate, cfg.ComposeProjectPrefix) {
				continue
			}
			name := composeProjectNameFromLabels(candidate.Labels)
			if name == "" {
				continue
			}
			project := projectsByName[name]
			if project == nil {
				project = &ComposeProject{Name: name, Labels: map[string]string{}}
				projectsByName[name] = project
				seenContainers[name] = map[string]bool{}
			}
			project.mergeCandidate(candidate)
			container := candidate.ID
			if container == "" {
				container = candidate.Name
			}
			if container != "" && !seenContainers[name][container] {
				seenContainers[name][container] = true
				project.ContainerIDs = append(project.ContainerIDs, container)
			}
		}
	}
	projects := make([]ComposeProject, 0, len(projectsByName))
	for _, project := range projectsByName {
		sort.Strings(project.ContainerIDs)
		projects = append(projects, *project)
	}
	sort.Slice(projects, func(i, j int) bool {
		return projects[i].Name < projects[j].Name
	})
	return projects, errors.Join(errs...)
}

func CleanupComposeArgs(cfg config.DockerConfig, composeFile, projectName string) []string {
	return CleanupComposeArgsFiles(cfg, composeFilesFromLegacy(composeFile, nil), projectName)
}

func CleanupComposeArgsFiles(cfg config.DockerConfig, composeFiles []string, projectName string) []string {
	return CleanupComposeArgsFilesWithProjectDir(cfg, composeFiles, projectName, "")
}

func CleanupComposeArgsFilesWithProjectDir(cfg config.DockerConfig, composeFiles []string, projectName, projectDir string) []string {
	return CleanupComposeArgsFilesWithProjectDirAndEnvFiles(cfg, composeFiles, nil, projectName, projectDir)
}

func CleanupComposeArgsFilesWithProjectDirAndEnvFiles(cfg config.DockerConfig, composeFiles, envFiles []string, projectName, projectDir string) []string {
	args := []string{"compose"}
	if strings.TrimSpace(projectDir) != "" {
		args = append(args, "--project-directory", projectDir)
	}
	for _, envFile := range envFiles {
		if strings.TrimSpace(envFile) != "" {
			args = append(args, "--env-file", envFile)
		}
	}
	args = append(args, ComposeFileArgs(normalizeComposeFiles(composeFiles))...)
	args = append(args, "-p", projectName, "down")
	args = append(args, "--timeout", "30")
	if cfg.CleanupVolumes {
		args = append(args, "-v")
	}
	args = append(args, "--remove-orphans")
	if cfg.CleanupImages {
		args = append(args, "--rmi", "local")
	}
	return args
}

func normalizeComposeFiles(files []string) []string {
	result := make([]string, 0, len(files))
	seen := map[string]bool{}
	for _, file := range files {
		file = strings.TrimSpace(file)
		if file == "" || seen[file] {
			continue
		}
		seen[file] = true
		result = append(result, file)
	}
	return result
}

func composeFilesFromLegacy(composeFile string, composeFiles []string) []string {
	composeFiles = normalizeComposeFiles(composeFiles)
	if len(composeFiles) == 0 && strings.TrimSpace(composeFile) != "" {
		composeFiles = []string{strings.TrimSpace(composeFile)}
	}
	return composeFiles
}

func CommandLine(name string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, shellQuote(name))
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	if strings.IndexFunc(value, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-' || r == '.' || r == '/' || r == ':' || r == '=')
	}) < 0 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func dockerToken(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = regexp.MustCompile(`[^a-z0-9_-]+`).ReplaceAllString(value, "_")
	value = strings.Trim(value, "_-")
	if value == "" {
		return "x"
	}
	return value
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:8]
}

func firstFrontendURL(mappings map[string][]PortMapping, services []string) string {
	for _, service := range services {
		if url := frontendURLForMappings(mappings[service]); url != "" {
			return url
		}
	}
	serviceNames := make([]string, 0, len(mappings))
	for service := range mappings {
		serviceNames = append(serviceNames, service)
	}
	sort.Strings(serviceNames)
	for _, service := range serviceNames {
		if url := frontendURLForMappings(mappings[service]); url != "" {
			return url
		}
	}
	return ""
}

func frontendURLForMappings(mappings []PortMapping) string {
	for _, mapping := range mappings {
		if mapping.Host == 0 {
			continue
		}
		scheme := "http"
		if mapping.Container == 443 || mapping.Host == 443 {
			scheme = "https"
		}
		return fmt.Sprintf("%s://%s:%d", scheme, normalizePortHost(mapping.URL), mapping.Host)
	}
	return ""
}

func normalizePortHost(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "http://")
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.Trim(raw, "[]")
	if raw == "" || raw == "0.0.0.0" || raw == "::" || raw == "127.0.0.1" {
		return "localhost"
	}
	if strings.Contains(raw, ":") {
		host, _, ok := strings.Cut(raw, ":")
		if ok {
			return normalizePortHost(host)
		}
	}
	return raw
}

func composeProjectNameFromLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	return strings.TrimSpace(labels["com.docker.compose.project"])
}

func (p *ComposeProject) mergeCandidate(candidate GCCandidate) {
	if p == nil {
		return
	}
	for key, value := range candidate.Labels {
		if _, ok := p.Labels[key]; !ok {
			p.Labels[key] = value
		}
	}
	if p.WorkDir == "" {
		p.WorkDir = strings.TrimSpace(candidate.Labels["com.docker.compose.project.working_dir"])
	}
	if len(p.ComposeFiles) == 0 {
		p.ComposeFiles = splitComposeConfigFiles(candidate.Labels["com.docker.compose.project.config_files"])
	}
}

func splitComposeConfigFiles(raw string) []string {
	var files []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			files = append(files, item)
		}
	}
	return files
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
