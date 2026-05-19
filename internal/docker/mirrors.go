package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
)

type BuildMirrorSummary struct {
	Enabled           bool                        `json:"enabled"`
	Mode              string                      `json:"mode"`
	Profile           string                      `json:"profile,omitempty"`
	RepoModified      bool                        `json:"repo_modified"`
	ComposeFile       string                      `json:"compose_file,omitempty"`
	ComposeFiles      []string                    `json:"compose_files,omitempty"`
	OverrideFile      string                      `json:"override_file,omitempty"`
	OverrideGenerated bool                        `json:"override_generated"`
	OverrideVerified  bool                        `json:"override_verified"`
	FallbackUsed      bool                        `json:"fallback_used"`
	FallbackReason    string                      `json:"fallback_reason,omitempty"`
	FallbackFrom      []string                    `json:"fallback_from,omitempty"`
	FallbackTo        []string                    `json:"fallback_to,omitempty"`
	Coverage          string                      `json:"coverage,omitempty"`
	Services          []BuildMirrorServiceSummary `json:"services"`
	Warnings          []string                    `json:"warnings,omitempty"`
}

type BuildMirrorServiceSummary struct {
	Service            string   `json:"service"`
	Context            string   `json:"context,omitempty"`
	OriginalDockerfile string   `json:"original_dockerfile,omitempty"`
	PatchedDockerfile  string   `json:"patched_dockerfile,omitempty"`
	Patched            bool     `json:"patched"`
	SkippedReason      string   `json:"skipped_reason,omitempty"`
	Injected           []string `json:"injected,omitempty"`
	Warnings           []string `json:"warnings,omitempty"`
}

type DaemonMirrorSummary struct {
	Operation              string           `json:"operation"`
	OK                     bool             `json:"ok"`
	DaemonJSON             string           `json:"daemon_json"`
	BackupPath             string           `json:"backup_path,omitempty"`
	BeforeSHA256           string           `json:"before_sha256,omitempty"`
	AfterSHA256            string           `json:"after_sha256,omitempty"`
	CurrentRegistryMirrors []string         `json:"current_registry_mirrors,omitempty"`
	DesiredRegistryMirrors []string         `json:"desired_registry_mirrors,omitempty"`
	Changed                bool             `json:"changed"`
	Diff                   DaemonMirrorDiff `json:"diff"`
	ManualApplyPath        string           `json:"manual_apply_path,omitempty"`
	ManualApplyCommand     string           `json:"manual_apply_command,omitempty"`
	RestartRequired        bool             `json:"restart_required"`
	RestartCommand         string           `json:"restart_command"`
	RecordedAt             string           `json:"recorded_at"`
	Error                  string           `json:"error,omitempty"`
	Warnings               []string         `json:"warnings,omitempty"`
}

type DaemonMirrorDiff struct {
	RegistryMirrorsAdded   []string `json:"registry_mirrors_added,omitempty"`
	RegistryMirrorsRemoved []string `json:"registry_mirrors_removed,omitempty"`
}

func DaemonMirrorSummaryPath(scanPath string) string {
	return filepath.Join(scanPath, ".qa-control", "daemon_mirror_summary.json")
}

func DaemonMirrorManualApplyPath(scanPath string) string {
	return filepath.Join(scanPath, ".qa-control", "docker-daemon", "daemon.json")
}

func DaemonMirrorManualRestorePath(scanPath string) string {
	return filepath.Join(scanPath, ".qa-control", "docker-daemon", "daemon.restore.json")
}

func DockerGCSummaryPath(scanPath string) string {
	return filepath.Join(scanPath, ".qa-control", "docker_gc_summary.json")
}

func DaemonMirrorStatus(cfg config.Config) DaemonMirrorSummary {
	summary := baseDaemonMirrorSummary("status", cfg.Docker.DaemonMirrors)
	current, content, err := readDaemonMirrors(cfg.Docker.DaemonMirrors.DaemonJSON)
	if err != nil {
		summary.OK = false
		summary.Error = err.Error()
		return summary
	}
	summary.OK = true
	summary.BeforeSHA256 = sha256Hex(content)
	summary.CurrentRegistryMirrors = current
	summary.DesiredRegistryMirrors = append([]string(nil), cfg.Docker.DaemonMirrors.RegistryMirrors...)
	summary.Diff = diffMirrors(current, summary.DesiredRegistryMirrors)
	summary.Changed = len(summary.Diff.RegistryMirrorsAdded) > 0 || len(summary.Diff.RegistryMirrorsRemoved) > 0
	return summary
}

func PlanDaemonMirrors(cfg config.Config) (DaemonMirrorSummary, error) {
	summary := baseDaemonMirrorSummary("manual_apply", cfg.Docker.DaemonMirrors)
	if !cfg.Docker.DaemonMirrors.Enabled {
		summary.OK = true
		summary.Warnings = append(summary.Warnings, "Docker daemon 镜像源未启用，未生成手动应用文件")
		_ = writeDaemonMirrorSummary(cfg.ScanPath, summary)
		return summary, nil
	}
	updated, current, beforeContent, afterContent, err := mergeDaemonMirrorsFile(cfg.Docker.DaemonMirrors.DaemonJSON, cfg.Docker.DaemonMirrors.RegistryMirrors)
	if err != nil {
		summary.OK = false
		summary.Error = err.Error()
		_ = writeDaemonMirrorSummary(cfg.ScanPath, summary)
		return summary, err
	}
	summary.CurrentRegistryMirrors = current
	summary.DesiredRegistryMirrors = append([]string(nil), cfg.Docker.DaemonMirrors.RegistryMirrors...)
	summary.Diff = diffMirrors(current, summary.DesiredRegistryMirrors)
	summary.Changed = string(beforeContent) != string(afterContent)
	summary.BeforeSHA256 = sha256Hex(beforeContent)
	summary.AfterSHA256 = sha256Hex(afterContent)
	if summary.Changed {
		path, err := writeManualDaemonJSON(DaemonMirrorManualApplyPath(cfg.ScanPath), updated)
		if err != nil {
			summary.OK = false
			summary.Error = err.Error()
			_ = writeDaemonMirrorSummary(cfg.ScanPath, summary)
			return summary, err
		}
		summary.ManualApplyPath = path
		summary.ManualApplyCommand = manualInstallCommand(path, cfg.Docker.DaemonMirrors.DaemonJSON)
		summary.RestartRequired = true
		summary.Warnings = append(summary.Warnings, "已生成手动应用文件，未直接写入 Docker daemon.json")
	}
	summary.OK = true
	if err := writeDaemonMirrorSummary(cfg.ScanPath, summary); err != nil {
		return summary, err
	}
	return summary, nil
}

func ApplyDaemonMirrors(cfg config.Config, yes bool) (DaemonMirrorSummary, error) {
	summary := baseDaemonMirrorSummary("apply", cfg.Docker.DaemonMirrors)
	if cfg.Docker.DaemonMirrors.RequireManualApply && !yes {
		err := fmt.Errorf("docker mirror apply requires --yes; target %s mirrors %s", cfg.Docker.DaemonMirrors.DaemonJSON, strings.Join(cfg.Docker.DaemonMirrors.RegistryMirrors, ", "))
		summary.OK = false
		summary.Error = err.Error()
		_ = writeDaemonMirrorSummary(cfg.ScanPath, summary)
		return summary, err
	}
	if !cfg.Docker.DaemonMirrors.Enabled {
		summary.OK = true
		summary.Warnings = append(summary.Warnings, "Docker daemon 镜像源未启用，未写入 daemon.json")
		_ = writeDaemonMirrorSummary(cfg.ScanPath, summary)
		return summary, nil
	}
	updated, current, beforeContent, afterContent, err := mergeDaemonMirrorsFile(cfg.Docker.DaemonMirrors.DaemonJSON, cfg.Docker.DaemonMirrors.RegistryMirrors)
	if err != nil {
		summary.OK = false
		summary.Error = err.Error()
		_ = writeDaemonMirrorSummary(cfg.ScanPath, summary)
		return summary, err
	}
	summary.CurrentRegistryMirrors = current
	summary.DesiredRegistryMirrors = append([]string(nil), cfg.Docker.DaemonMirrors.RegistryMirrors...)
	summary.Diff = diffMirrors(current, summary.DesiredRegistryMirrors)
	summary.Changed = string(beforeContent) != string(afterContent)
	summary.BeforeSHA256 = sha256Hex(beforeContent)
	summary.AfterSHA256 = sha256Hex(afterContent)
	if summary.Changed {
		backup, err := backupDaemonJSON(cfg.Docker.DaemonMirrors.BackupDir, cfg.Docker.DaemonMirrors.DaemonJSON, beforeContent)
		if err != nil {
			summary.OK = false
			summary.Error = err.Error()
			_ = writeDaemonMirrorSummary(cfg.ScanPath, summary)
			return summary, err
		}
		summary.BackupPath = backup
		if err := os.WriteFile(cfg.Docker.DaemonMirrors.DaemonJSON, updated, 0o644); err != nil {
			summary.OK = false
			summary.Error = err.Error()
			_ = writeDaemonMirrorSummary(cfg.ScanPath, summary)
			return summary, err
		}
		summary.RestartRequired = true
	}
	summary.OK = true
	if err := writeDaemonMirrorSummary(cfg.ScanPath, summary); err != nil {
		return summary, err
	}
	return summary, nil
}

func RestoreDaemonMirrors(cfg config.Config, backup string, yes bool) (DaemonMirrorSummary, error) {
	summary := baseDaemonMirrorSummary("restore", cfg.Docker.DaemonMirrors)
	backup = strings.TrimSpace(backup)
	if backup == "" || !yes {
		err := fmt.Errorf("docker mirror restore requires --backup and --yes")
		summary.OK = false
		summary.Error = err.Error()
		_ = writeDaemonMirrorSummary(cfg.ScanPath, summary)
		return summary, err
	}
	before, err := os.ReadFile(cfg.Docker.DaemonMirrors.DaemonJSON)
	if err != nil && !os.IsNotExist(err) {
		summary.OK = false
		summary.Error = err.Error()
		_ = writeDaemonMirrorSummary(cfg.ScanPath, summary)
		return summary, err
	}
	restoreContent, err := os.ReadFile(backup)
	if err != nil {
		summary.OK = false
		summary.Error = err.Error()
		_ = writeDaemonMirrorSummary(cfg.ScanPath, summary)
		return summary, err
	}
	if _, err := decodeDaemonJSON(restoreContent); err != nil {
		summary.OK = false
		summary.Error = err.Error()
		_ = writeDaemonMirrorSummary(cfg.ScanPath, summary)
		return summary, err
	}
	if err := os.WriteFile(cfg.Docker.DaemonMirrors.DaemonJSON, restoreContent, 0o644); err != nil {
		summary.OK = false
		summary.Error = err.Error()
		_ = writeDaemonMirrorSummary(cfg.ScanPath, summary)
		return summary, err
	}
	current, _, _ := readDaemonMirrors(cfg.Docker.DaemonMirrors.DaemonJSON)
	summary.OK = true
	summary.BackupPath = backup
	summary.BeforeSHA256 = sha256Hex(before)
	summary.AfterSHA256 = sha256Hex(restoreContent)
	summary.CurrentRegistryMirrors = current
	summary.DesiredRegistryMirrors = append([]string(nil), cfg.Docker.DaemonMirrors.RegistryMirrors...)
	summary.Diff = diffMirrors(summary.DesiredRegistryMirrors, current)
	summary.Changed = string(before) != string(restoreContent)
	summary.RestartRequired = summary.Changed
	if err := writeDaemonMirrorSummary(cfg.ScanPath, summary); err != nil {
		return summary, err
	}
	return summary, nil
}

func PlanRestoreDaemonMirrors(cfg config.Config, backup string) (DaemonMirrorSummary, error) {
	summary := baseDaemonMirrorSummary("manual_restore", cfg.Docker.DaemonMirrors)
	backup = strings.TrimSpace(backup)
	if backup == "" {
		err := fmt.Errorf("docker mirror restore requires --backup")
		summary.OK = false
		summary.Error = err.Error()
		_ = writeDaemonMirrorSummary(cfg.ScanPath, summary)
		return summary, err
	}
	before, err := os.ReadFile(cfg.Docker.DaemonMirrors.DaemonJSON)
	if err != nil {
		if !os.IsNotExist(err) {
			summary.Warnings = append(summary.Warnings, "无法读取当前 daemon.json: "+err.Error())
		}
		before = nil
	}
	restoreContent, err := os.ReadFile(backup)
	if err != nil {
		summary.OK = false
		summary.Error = err.Error()
		_ = writeDaemonMirrorSummary(cfg.ScanPath, summary)
		return summary, err
	}
	data, err := decodeDaemonJSON(restoreContent)
	if err != nil {
		summary.OK = false
		summary.Error = err.Error()
		_ = writeDaemonMirrorSummary(cfg.ScanPath, summary)
		return summary, err
	}
	path, err := writeManualDaemonJSON(DaemonMirrorManualRestorePath(cfg.ScanPath), restoreContent)
	if err != nil {
		summary.OK = false
		summary.Error = err.Error()
		_ = writeDaemonMirrorSummary(cfg.ScanPath, summary)
		return summary, err
	}
	summary.OK = true
	summary.BackupPath = backup
	summary.ManualApplyPath = path
	summary.ManualApplyCommand = manualInstallCommand(path, cfg.Docker.DaemonMirrors.DaemonJSON)
	summary.BeforeSHA256 = sha256Hex(before)
	summary.AfterSHA256 = sha256Hex(restoreContent)
	if len(before) > 0 {
		currentData, err := decodeDaemonJSON(before)
		if err != nil {
			summary.Warnings = append(summary.Warnings, "无法解析当前 daemon.json: "+err.Error())
		} else {
			summary.CurrentRegistryMirrors = stringSlice(currentData["registry-mirrors"])
		}
	}
	summary.DesiredRegistryMirrors = stringSlice(data["registry-mirrors"])
	summary.Diff = diffMirrors(summary.CurrentRegistryMirrors, summary.DesiredRegistryMirrors)
	summary.Changed = string(before) != string(restoreContent)
	summary.RestartRequired = summary.Changed
	summary.Warnings = append(summary.Warnings, "已生成手动恢复文件，未直接写入 Docker daemon.json")
	if err := writeDaemonMirrorSummary(cfg.ScanPath, summary); err != nil {
		return summary, err
	}
	return summary, nil
}

func baseDaemonMirrorSummary(operation string, cfg config.DockerDaemonMirrorsConfig) DaemonMirrorSummary {
	return DaemonMirrorSummary{
		Operation:              operation,
		DaemonJSON:             cfg.DaemonJSON,
		RestartCommand:         "sudo systemctl restart docker",
		RecordedAt:             time.Now().UTC().Format(time.RFC3339),
		DesiredRegistryMirrors: append([]string(nil), cfg.RegistryMirrors...),
	}
}

func readDaemonMirrors(path string) ([]string, []byte, error) {
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, []byte("{}"), nil
	}
	if err != nil {
		return nil, nil, err
	}
	data, err := decodeDaemonJSON(content)
	if err != nil {
		return nil, content, err
	}
	return stringSlice(data["registry-mirrors"]), content, nil
}

func mergeDaemonMirrorsFile(path string, desired []string) ([]byte, []string, []byte, []byte, error) {
	current, before, err := readDaemonMirrors(path)
	if err != nil {
		return nil, nil, before, nil, err
	}
	data, err := decodeDaemonJSON(before)
	if err != nil {
		return nil, nil, before, nil, err
	}
	data["registry-mirrors"] = append([]string(nil), desired...)
	after, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return nil, nil, before, nil, err
	}
	after = append(after, '\n')
	return after, current, before, after, nil
}

func decodeDaemonJSON(content []byte) (map[string]any, error) {
	if len(strings.TrimSpace(string(content))) == 0 {
		return map[string]any{}, nil
	}
	data := map[string]any{}
	if err := json.Unmarshal(content, &data); err != nil {
		return nil, fmt.Errorf("parse daemon.json: %w", err)
	}
	return data, nil
}

func stringSlice(value any) []string {
	raw, ok := value.([]any)
	if !ok {
		if typed, ok := value.([]string); ok {
			return append([]string(nil), typed...)
		}
		return nil
	}
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func backupDaemonJSON(dir, daemonPath string, content []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("daemon-%s-%s.json", time.Now().UTC().Format("20060102T150405Z"), sha256Hex(content)[:8])
	target := filepath.Join(dir, name)
	return target, os.WriteFile(target, content, 0o644)
}

func writeManualDaemonJSON(path string, content []byte) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, content, 0o644)
}

func manualInstallCommand(source, target string) string {
	return "sudo install -m 0644 " + shellQuote(source) + " " + shellQuote(target) + " && sudo systemctl restart docker"
}

func diffMirrors(current, desired []string) DaemonMirrorDiff {
	cur := map[string]bool{}
	des := map[string]bool{}
	for _, item := range current {
		cur[item] = true
	}
	for _, item := range desired {
		des[item] = true
	}
	var diff DaemonMirrorDiff
	for item := range des {
		if !cur[item] {
			diff.RegistryMirrorsAdded = append(diff.RegistryMirrorsAdded, item)
		}
	}
	for item := range cur {
		if !des[item] {
			diff.RegistryMirrorsRemoved = append(diff.RegistryMirrorsRemoved, item)
		}
	}
	sort.Strings(diff.RegistryMirrorsAdded)
	sort.Strings(diff.RegistryMirrorsRemoved)
	return diff
}

func writeDaemonMirrorSummary(scanPath string, summary DaemonMirrorSummary) error {
	path := DaemonMirrorSummaryPath(scanPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(content, '\n'), 0o644)
}

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
