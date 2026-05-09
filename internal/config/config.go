package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"gopkg.in/yaml.v3"
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
	Docs     DocsConfig
	Codex    CodexConfig
	TUI      TUIConfig
}

type PipelineConfig struct {
	StaticOnly         bool
	StageTimeouts      map[string]int
	SelfTestReportPath string
	MaxConcurrent      int
}

type DockerConfig struct {
	ManagedLabel                string
	ComposeProjectPrefix        string
	KeepFailedContainersMinutes int
	HealthCheckTimeoutSeconds   int
	CleanupPolicy               string
	CleanupImages               bool
	CleanupVolumes              bool
	CleanupBuildCache           bool
	BuildCachePruneUntil        string
	KeepRuntime                 bool
}

type DocsConfig struct {
	MaxAttachmentBytes   int64
	InlineTextLimitBytes int64
	StageInlineMaxBytes  int64
}

type CodexConfig struct {
	SandboxImage      string
	PromptProfilesDir string
	Network           string
	MaxOutputBytes    int
	WritableTmp       bool
	Env               map[string]string
	ExtraArgs         []string
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

type rawConfig struct {
	ScanPath *string            `yaml:"scan_path"`
	DBPath   *string            `yaml:"db_path"`
	Pipeline *rawPipelineConfig `yaml:"pipeline"`
	Docker   *rawDockerConfig   `yaml:"docker"`
	Docs     *rawDocsConfig     `yaml:"docs"`
	Codex    *rawCodexConfig    `yaml:"codex"`
	TUI      *rawTUIConfig      `yaml:"tui"`
}

type rawPipelineConfig struct {
	StaticOnly         *bool          `yaml:"static_only"`
	StageTimeouts      map[string]int `yaml:"stage_timeouts"`
	SelfTestReportPath *string        `yaml:"self_test_report_path"`
	MaxConcurrent      *int           `yaml:"max_concurrent"`
}

type rawDockerConfig struct {
	ManagedLabel                *string `yaml:"managed_label"`
	ComposeProjectPrefix        *string `yaml:"compose_project_prefix"`
	KeepFailedContainersMinutes *int    `yaml:"keep_failed_containers_minutes"`
	HealthCheckTimeoutSeconds   *int    `yaml:"health_check_timeout_seconds"`
	CleanupPolicy               *string `yaml:"cleanup_policy"`
	CleanupImages               *bool   `yaml:"cleanup_images"`
	CleanupVolumes              *bool   `yaml:"cleanup_volumes"`
	CleanupBuildCache           *bool   `yaml:"cleanup_build_cache"`
	BuildCachePruneUntil        *string `yaml:"build_cache_prune_until"`
	KeepRuntime                 *bool   `yaml:"keep_runtime"`
}

type rawDocsConfig struct {
	MaxAttachmentBytes   *int64 `yaml:"max_attachment_bytes"`
	InlineTextLimitBytes *int64 `yaml:"inline_text_limit_bytes"`
	StageInlineMaxBytes  *int64 `yaml:"stage_inline_max_bytes"`
}

type rawCodexConfig struct {
	SandboxImage      *string           `yaml:"sandbox_image"`
	PromptProfilesDir *string           `yaml:"prompt_profiles_dir"`
	Network           *string           `yaml:"network"`
	MaxOutputBytes    *int              `yaml:"max_output_bytes"`
	WritableTmp       *bool             `yaml:"writable_tmp"`
	Env               map[string]string `yaml:"env"`
	ExtraArgs         []string          `yaml:"extra_args"`
}

type rawTUIConfig struct {
	RefreshIntervalMS *int `yaml:"refresh_interval_ms"`
	LogMaxLines       *int `yaml:"log_max_lines"`
}

func Default() Config {
	return Config{
		ScanPath: "./projects-qa",
		DBPath:   "./projects-qa/.qa-control/index.db",
		Pipeline: PipelineConfig{
			StageTimeouts:      map[string]int{"A": 60, "B": 900, "B_PULL": 300, "B_BUILD": 600, "B_UP": 300, "B_HEALTH": 60, "B_PORT": 30, "C": 300, "D": 2700, "E": 2700, "F": 2700},
			SelfTestReportPath: "repo/self_test_report.md",
			MaxConcurrent:      3,
		},
		Docker: DockerConfig{
			ManagedLabel:                "managed_by=p2rqa",
			ComposeProjectPrefix:        "p2rqa",
			KeepFailedContainersMinutes: 60,
			HealthCheckTimeoutSeconds:   60,
			CleanupPolicy:               "always",
			CleanupImages:               true,
			CleanupVolumes:              true,
			CleanupBuildCache:           false,
			BuildCachePruneUntil:        "24h",
			KeepRuntime:                 false,
		},
		Docs: DocsConfig{
			MaxAttachmentBytes:   64 << 20,
			InlineTextLimitBytes: 1 << 20,
			StageInlineMaxBytes:  4 << 20,
		},
		Codex: CodexConfig{
			SandboxImage:      "codex:latest",
			PromptProfilesDir: "./projects-qa/.qa-control/prompt_profiles",
			Network:           "none",
			MaxOutputBytes:    1048576,
			WritableTmp:       false,
			Env:               map[string]string{},
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
	normalize(&cfg)
	if err := Validate(cfg); err != nil {
		return cfg, err
	}
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
	content, err := os.ReadFile(path)
	if err != nil {
		return settings, err
	}

	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	var raw rawConfig
	if err := decoder.Decode(&raw); err != nil {
		if err == io.EOF {
			return settings, nil
		}
		return settings, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := applyRawConfig(cfg, raw, &settings); err != nil {
		return settings, err
	}
	return settings, nil
}

func applyRawConfig(cfg *Config, raw rawConfig, settings *fileSettings) error {
	if raw.ScanPath != nil {
		cfg.ScanPath = *raw.ScanPath
		settings.ScanPath = true
	}
	if raw.DBPath != nil {
		cfg.DBPath = *raw.DBPath
		settings.DBPath = true
	}
	if raw.Pipeline != nil {
		if raw.Pipeline.StaticOnly != nil {
			cfg.Pipeline.StaticOnly = *raw.Pipeline.StaticOnly
		}
		for key, value := range raw.Pipeline.StageTimeouts {
			normalized, err := validateStageTimeoutKey(key)
			if err != nil {
				return err
			}
			cfg.Pipeline.StageTimeouts[normalized] = value
		}
		if raw.Pipeline.SelfTestReportPath != nil {
			cfg.Pipeline.SelfTestReportPath = *raw.Pipeline.SelfTestReportPath
		}
		if raw.Pipeline.MaxConcurrent != nil {
			cfg.Pipeline.MaxConcurrent = *raw.Pipeline.MaxConcurrent
		}
	}
	if raw.Docker != nil {
		if raw.Docker.ManagedLabel != nil {
			cfg.Docker.ManagedLabel = *raw.Docker.ManagedLabel
		}
		if raw.Docker.ComposeProjectPrefix != nil {
			cfg.Docker.ComposeProjectPrefix = *raw.Docker.ComposeProjectPrefix
		}
		if raw.Docker.KeepFailedContainersMinutes != nil {
			cfg.Docker.KeepFailedContainersMinutes = *raw.Docker.KeepFailedContainersMinutes
		}
		if raw.Docker.HealthCheckTimeoutSeconds != nil {
			cfg.Docker.HealthCheckTimeoutSeconds = *raw.Docker.HealthCheckTimeoutSeconds
		}
		if raw.Docker.CleanupPolicy != nil {
			cfg.Docker.CleanupPolicy = *raw.Docker.CleanupPolicy
		}
		if raw.Docker.CleanupImages != nil {
			cfg.Docker.CleanupImages = *raw.Docker.CleanupImages
		}
		if raw.Docker.CleanupVolumes != nil {
			cfg.Docker.CleanupVolumes = *raw.Docker.CleanupVolumes
		}
		if raw.Docker.CleanupBuildCache != nil {
			cfg.Docker.CleanupBuildCache = *raw.Docker.CleanupBuildCache
		}
		if raw.Docker.BuildCachePruneUntil != nil {
			cfg.Docker.BuildCachePruneUntil = *raw.Docker.BuildCachePruneUntil
		}
		if raw.Docker.KeepRuntime != nil {
			cfg.Docker.KeepRuntime = *raw.Docker.KeepRuntime
		}
	}
	if raw.Docs != nil {
		if raw.Docs.MaxAttachmentBytes != nil {
			cfg.Docs.MaxAttachmentBytes = *raw.Docs.MaxAttachmentBytes
		}
		if raw.Docs.InlineTextLimitBytes != nil {
			cfg.Docs.InlineTextLimitBytes = *raw.Docs.InlineTextLimitBytes
		}
		if raw.Docs.StageInlineMaxBytes != nil {
			cfg.Docs.StageInlineMaxBytes = *raw.Docs.StageInlineMaxBytes
		}
	}
	if raw.Codex != nil {
		if raw.Codex.SandboxImage != nil {
			cfg.Codex.SandboxImage = *raw.Codex.SandboxImage
		}
		if raw.Codex.PromptProfilesDir != nil {
			cfg.Codex.PromptProfilesDir = *raw.Codex.PromptProfilesDir
			settings.PromptProfilesDir = true
		}
		if raw.Codex.Network != nil {
			cfg.Codex.Network = *raw.Codex.Network
		}
		if raw.Codex.MaxOutputBytes != nil {
			cfg.Codex.MaxOutputBytes = *raw.Codex.MaxOutputBytes
		}
		if raw.Codex.WritableTmp != nil {
			cfg.Codex.WritableTmp = *raw.Codex.WritableTmp
		}
		if raw.Codex.Env != nil {
			if cfg.Codex.Env == nil {
				cfg.Codex.Env = map[string]string{}
			}
			for key, value := range raw.Codex.Env {
				expanded, err := expandEnvRefs(value)
				if err != nil {
					return err
				}
				cfg.Codex.Env[key] = expanded
			}
		}
		if raw.Codex.ExtraArgs != nil {
			cfg.Codex.ExtraArgs = append([]string(nil), raw.Codex.ExtraArgs...)
		}
	}
	if raw.TUI != nil {
		if raw.TUI.RefreshIntervalMS != nil {
			cfg.TUI.RefreshIntervalMS = *raw.TUI.RefreshIntervalMS
		}
		if raw.TUI.LogMaxLines != nil {
			cfg.TUI.LogMaxLines = *raw.TUI.LogMaxLines
		}
	}
	return nil
}

func normalize(cfg *Config) {
	if cfg.Pipeline.MaxConcurrent <= 0 {
		cfg.Pipeline.MaxConcurrent = 3
	}
	if cfg.Pipeline.MaxConcurrent > 8 {
		cfg.Pipeline.MaxConcurrent = 8
	}
}

func Validate(cfg Config) error {
	for key, seconds := range cfg.Pipeline.StageTimeouts {
		if _, err := validateStageTimeoutKey(key); err != nil {
			return err
		}
		if seconds <= 0 {
			return fmt.Errorf("pipeline.stage_timeouts.%s must be greater than 0", key)
		}
	}
	if cfg.Pipeline.MaxConcurrent <= 0 {
		return fmt.Errorf("pipeline.max_concurrent must be greater than 0")
	}
	if cfg.Docker.KeepFailedContainersMinutes < 0 {
		return fmt.Errorf("docker.keep_failed_containers_minutes must be greater than or equal to 0")
	}
	if cfg.Docker.HealthCheckTimeoutSeconds <= 0 {
		return fmt.Errorf("docker.health_check_timeout_seconds must be greater than 0")
	}
	if strings.TrimSpace(cfg.Docker.ManagedLabel) == "" {
		return fmt.Errorf("docker.managed_label must not be empty")
	}
	if strings.TrimSpace(cfg.Docker.ComposeProjectPrefix) == "" {
		return fmt.Errorf("docker.compose_project_prefix must not be empty")
	}
	if strings.TrimSpace(cfg.Docker.CleanupPolicy) == "" {
		return fmt.Errorf("docker.cleanup_policy must not be empty")
	}
	if cfg.Docs.MaxAttachmentBytes <= 0 {
		return fmt.Errorf("docs.max_attachment_bytes must be greater than 0")
	}
	if cfg.Docs.InlineTextLimitBytes <= 0 {
		return fmt.Errorf("docs.inline_text_limit_bytes must be greater than 0")
	}
	if cfg.Docs.StageInlineMaxBytes <= 0 {
		return fmt.Errorf("docs.stage_inline_max_bytes must be greater than 0")
	}
	if strings.TrimSpace(cfg.Codex.SandboxImage) == "" {
		return fmt.Errorf("codex.sandbox_image must not be empty")
	}
	if strings.TrimSpace(cfg.Codex.Network) == "" {
		return fmt.Errorf("codex.network must not be empty")
	}
	if cfg.Codex.MaxOutputBytes <= 0 {
		return fmt.Errorf("codex.max_output_bytes must be greater than 0")
	}
	if cfg.TUI.RefreshIntervalMS <= 0 {
		return fmt.Errorf("tui.refresh_interval_ms must be greater than 0")
	}
	if cfg.TUI.LogMaxLines <= 0 {
		return fmt.Errorf("tui.log_max_lines must be greater than 0")
	}
	return nil
}

func normalizeStageTimeoutKey(key string) string {
	key = strings.TrimSpace(key)
	key = strings.ReplaceAll(key, "-", "_")
	return strings.ToUpper(key)
}

func validateStageTimeoutKey(key string) (string, error) {
	normalized := normalizeStageTimeoutKey(key)
	if normalized == "" {
		return "", fmt.Errorf("pipeline.stage_timeouts contains an empty key")
	}
	stage := normalized
	if index := strings.Index(stage, "_"); index >= 0 {
		stage = stage[:index]
	}
	if !model.IsStageID(stage) {
		return "", fmt.Errorf("pipeline.stage_timeouts.%s uses unknown stage %q", key, stage)
	}
	return normalized, nil
}

func expandEnvRefs(value string) (string, error) {
	var builder strings.Builder
	for {
		start := strings.Index(value, "${")
		if start < 0 {
			builder.WriteString(value)
			return builder.String(), nil
		}
		builder.WriteString(value[:start])
		rest := value[start+2:]
		end := strings.Index(rest, "}")
		if end < 0 {
			builder.WriteString(value[start:])
			return builder.String(), nil
		}
		name := rest[:end]
		envValue, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("environment variable %s referenced by config is not set", name)
		}
		builder.WriteString(envValue)
		value = rest[end+1:]
	}
}
