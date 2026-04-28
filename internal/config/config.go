package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	ScanPath string
	DBPath   string
	Pipeline PipelineConfig
	Docker   DockerConfig
	Codex    CodexConfig
	TUI      TUIConfig
}

type PipelineConfig struct {
	StaticOnly    bool
	StageTimeouts map[string]int
}

type DockerConfig struct {
	ManagedLabel                string
	ComposeProjectPrefix        string
	KeepFailedContainersMinutes int
	HealthCheckTimeoutSeconds   int
}

type CodexConfig struct {
	SandboxImage      string
	PromptProfilesDir string
	Network           string
	MaxOutputBytes    int
	WritableTmp       bool
}

type TUIConfig struct {
	RefreshIntervalMS int
	LogMaxLines       int
}

type Overrides struct {
	ScanPath string
	DBPath   string
}

func Default() Config {
	return Config{
		ScanPath: "./projects-qa",
		DBPath:   "./projects-qa/.qa-control/index.db",
		Pipeline: PipelineConfig{
			StageTimeouts: map[string]int{"A": 60, "B": 120, "C": 300, "D": 300, "E": 600, "F": 60},
		},
		Docker: DockerConfig{
			ManagedLabel:                "managed_by=p2rqa",
			ComposeProjectPrefix:        "p2rqa",
			KeepFailedContainersMinutes: 60,
			HealthCheckTimeoutSeconds:   60,
		},
		Codex: CodexConfig{
			SandboxImage:      "codex:latest",
			PromptProfilesDir: "./projects-qa/.qa-control/prompt_profiles",
			Network:           "none",
			MaxOutputBytes:    1048576,
			WritableTmp:       true,
		},
		TUI: TUIConfig{
			RefreshIntervalMS: 100,
			LogMaxLines:       10000,
		},
	}
}

func Load(cwd string, overrides Overrides) (Config, error) {
	cfg := Default()
	path := filepath.Join(cwd, ".p2r.yaml")
	if _, err := os.Stat(path); err == nil {
		if err := applyFile(&cfg, path); err != nil {
			return cfg, err
		}
	} else if !os.IsNotExist(err) {
		return cfg, err
	}
	if overrides.ScanPath != "" {
		cfg.ScanPath = overrides.ScanPath
	}
	if overrides.DBPath != "" {
		cfg.DBPath = overrides.DBPath
	}
	cfg.ScanPath = absFrom(cwd, cfg.ScanPath)
	cfg.DBPath = absFrom(cwd, cfg.DBPath)
	cfg.Codex.PromptProfilesDir = absFrom(cwd, cfg.Codex.PromptProfilesDir)
	return cfg, nil
}

func absFrom(cwd, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(cwd, path))
}

func applyFile(cfg *Config, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	var section string
	var subSection string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		raw := scanner.Text()
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		if strings.HasSuffix(line, ":") {
			key := strings.TrimSuffix(line, ":")
			if indent == 0 {
				section, subSection = key, ""
			} else if indent == 2 {
				subSection = key
			}
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		switch {
		case section == "" && key == "scan_path":
			cfg.ScanPath = value
		case section == "" && key == "db_path":
			cfg.DBPath = value
		case section == "pipeline" && key == "static_only":
			cfg.Pipeline.StaticOnly = parseBool(value)
		case section == "pipeline" && subSection == "stage_timeouts":
			cfg.Pipeline.StageTimeouts[strings.ToUpper(key)] = parseInt(value, cfg.Pipeline.StageTimeouts[strings.ToUpper(key)])
		case section == "docker" && key == "managed_label":
			cfg.Docker.ManagedLabel = value
		case section == "docker" && key == "compose_project_prefix":
			cfg.Docker.ComposeProjectPrefix = value
		case section == "docker" && key == "keep_failed_containers_minutes":
			cfg.Docker.KeepFailedContainersMinutes = parseInt(value, cfg.Docker.KeepFailedContainersMinutes)
		case section == "docker" && key == "health_check_timeout_seconds":
			cfg.Docker.HealthCheckTimeoutSeconds = parseInt(value, cfg.Docker.HealthCheckTimeoutSeconds)
		case section == "codex" && key == "sandbox_image":
			cfg.Codex.SandboxImage = value
		case section == "codex" && key == "prompt_profiles_dir":
			cfg.Codex.PromptProfilesDir = value
		case section == "codex" && key == "network":
			cfg.Codex.Network = value
		case section == "codex" && key == "max_output_bytes":
			cfg.Codex.MaxOutputBytes = parseInt(value, cfg.Codex.MaxOutputBytes)
		case section == "codex" && key == "writable_tmp":
			cfg.Codex.WritableTmp = parseBool(value)
		case section == "tui" && key == "refresh_interval_ms":
			cfg.TUI.RefreshIntervalMS = parseInt(value, cfg.TUI.RefreshIntervalMS)
		case section == "tui" && key == "log_max_lines":
			cfg.TUI.LogMaxLines = parseInt(value, cfg.TUI.LogMaxLines)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	return nil
}

func stripComment(line string) string {
	if idx := strings.Index(line, "#"); idx >= 0 {
		return line[:idx]
	}
	return line
}

func parseInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func parseBool(value string) bool {
	switch strings.ToLower(value) {
	case "true", "yes", "1", "on":
		return true
	default:
		return false
	}
}
