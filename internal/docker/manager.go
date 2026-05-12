package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
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

func CleanupComposeProject(ctx context.Context, exec executor.CommandRunner, cfg config.DockerConfig, composeFile, projectName, workDir string) CleanupSummary {
	summary := CleanupSummary{Status: "not_applicable", ComposeFile: composeFile, ComposeProject: projectName, WorkDir: workDir}
	if strings.TrimSpace(projectName) == "" {
		summary.Warnings = append(summary.Warnings, "compose project is empty")
		return summary
	}
	args := CleanupComposeArgs(cfg, composeFile, projectName)
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
	psArgs := []string{"compose"}
	if strings.TrimSpace(composeFile) != "" {
		psArgs = append(psArgs, "-f", composeFile)
	}
	psArgs = append(psArgs, "-p", projectName, "ps", "-q")
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

func CleanupComposeArgs(cfg config.DockerConfig, composeFile, projectName string) []string {
	args := []string{"compose"}
	if strings.TrimSpace(composeFile) != "" {
		args = append(args, "-f", composeFile)
	}
	args = append(args, "-p", projectName, "down")
	if cfg.CleanupVolumes {
		args = append(args, "-v")
	}
	args = append(args, "--remove-orphans")
	if cfg.CleanupImages {
		args = append(args, "--rmi", "local")
	}
	return args
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
