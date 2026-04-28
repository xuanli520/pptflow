package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	EnvConfig   = "P2R_CONFIG"
	EnvScanPath = "P2R_SCAN_PATH"
	EnvDBPath   = "P2R_DB_PATH"
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

type pathBases struct {
	ScanPath          string
	DBPath            string
	PromptProfilesDir string
}

type fileSettings struct {
	ScanPath          bool
	DBPath            bool
	PromptProfilesDir bool
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
			WritableTmp:       false,
		},
		TUI: TUIConfig{
			RefreshIntervalMS: 100,
			LogMaxLines:       10000,
		},
	}
}

func Load(cwd string, overrides Overrides) (Config, error) {
	cfg := Default()
	cwd = filepath.Clean(cwd)
	bases := pathBases{
		ScanPath:          cwd,
		DBPath:            cwd,
		PromptProfilesDir: cwd,
	}

	path, err := discoverConfig(cwd)
	if err != nil {
		return cfg, err
	}
	if path != "" {
		settings, err := applyFile(&cfg, path)
		if err != nil {
			return cfg, err
		}
		base := filepath.Dir(path)
		if settings.ScanPath {
			bases.ScanPath = base
		}
		if settings.DBPath {
			bases.DBPath = base
		}
		if settings.PromptProfilesDir {
			bases.PromptProfilesDir = base
		}
	}
	if value := strings.TrimSpace(os.Getenv(EnvScanPath)); value != "" {
		cfg.ScanPath = value
		bases.ScanPath = cwd
	}
	if value := strings.TrimSpace(os.Getenv(EnvDBPath)); value != "" {
		cfg.DBPath = value
		bases.DBPath = cwd
	}
	if overrides.ScanPath != "" {
		cfg.ScanPath = overrides.ScanPath
		bases.ScanPath = cwd
	}
	if overrides.DBPath != "" {
		cfg.DBPath = overrides.DBPath
		bases.DBPath = cwd
	}
	cfg.ScanPath = absFrom(bases.ScanPath, cfg.ScanPath)
	cfg.DBPath = absFrom(bases.DBPath, cfg.DBPath)
	cfg.Codex.PromptProfilesDir = absFrom(bases.PromptProfilesDir, cfg.Codex.PromptProfilesDir)
	return cfg, nil
}

func discoverConfig(cwd string) (string, error) {
	if explicit := strings.TrimSpace(os.Getenv(EnvConfig)); explicit != "" {
		path := absFrom(cwd, explicit)
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				return "", fmt.Errorf("%s points to missing config %s", EnvConfig, path)
			}
			return "", err
		}
		return path, nil
	}

	localPath := filepath.Join(cwd, ".p2r.yaml")
	if _, err := os.Stat(localPath); err == nil {
		return localPath, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	for _, path := range userConfigCandidates() {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	return "", nil
}

func userConfigCandidates() []string {
	var candidates []string
	if configDir, err := os.UserConfigDir(); err == nil && configDir != "" {
		candidates = append(candidates, filepath.Join(configDir, "p2r", "config.yaml"))
	}
	if homeDir, err := os.UserHomeDir(); err == nil && homeDir != "" {
		candidates = append(candidates, filepath.Join(homeDir, ".p2r.yaml"))
	}
	return candidates
}

func absFrom(cwd, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(cwd, path))
}

func applyFile(cfg *Config, path string) (fileSettings, error) {
	var settings fileSettings
	file, err := os.Open(path)
	if err != nil {
		return settings, err
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
			settings.ScanPath = true
		case section == "" && key == "db_path":
			cfg.DBPath = value
			settings.DBPath = true
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
			settings.PromptProfilesDir = true
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
		return settings, fmt.Errorf("read config: %w", err)
	}
	return settings, nil
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
