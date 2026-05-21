package config

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"gopkg.in/yaml.v3"
)

const (
	EnvConfig   = "P2R_CONFIG"
	EnvScanPath = "P2R_SCAN_PATH"
	EnvDBPath   = "P2R_DB_PATH"

	DefaultMaxConcurrent = 10
	MaxConcurrentLimit   = 10
)

type Config struct {
	ScanPath          string
	DBPath            string
	ProjectConfigPath string
	Pipeline          PipelineConfig
	Git               GitConfig
	Docker            DockerConfig
	Docs              DocsConfig
	Codex             CodexConfig
	TUI               TUIConfig
}

type PipelineConfig struct {
	StaticOnly         bool
	StageTimeouts      map[string]int
	SelfTestReportPath string
	MaxConcurrent      int
}

type GitConfig struct {
	BaseURL      string
	CloneTimeout time.Duration
	ShallowClone bool
	LFSEnabled   bool
}

type DockerConfig struct {
	ManagedLabel                string
	ComposeProjectPrefix        string
	KeepFailedContainersMinutes int
	HealthCheckTimeoutSeconds   int
	PullPolicy                  string
	DaemonMirrors               DockerDaemonMirrorsConfig
	BuildMirrors                DockerBuildMirrorsConfig
	CleanupPolicy               string
	CleanupImages               bool
	CleanupVolumes              bool
	CleanupBuildCache           bool
	BuildCachePruneUntil        string
	KeepRuntime                 bool
	GC                          DockerGCConfig
}

type DockerDaemonMirrorsConfig struct {
	Enabled            bool
	DaemonJSON         string
	BackupDir          string
	RegistryMirrors    []string
	RequireManualApply bool
}

type DockerBuildMirrorsConfig struct {
	Enabled            bool
	Mode               string
	FallbackToOriginal bool
	VerifyOverride     bool
	Profile            string
	AptMirror          string
	UbuntuMirror       string
	ApkMirror          string
	YumMirror          string
	NPMRegistry        string
	PipIndexURL        string
	GoProxy            string
	CargoRegistry      string
}

type DockerGCConfig struct {
	Enabled               bool
	RunOnTUIStart         bool
	RunBeforeCLIRun       bool
	Interval              string
	P2ROnly               bool
	PruneExitedContainers bool
	PruneNetworks         bool
	PruneVolumes          bool
	PruneImages           bool
	PruneBuilderCache     bool
	BuilderCacheUntil     string
}

type DocsConfig struct {
	MaxAttachmentBytes   int64
	InlineTextLimitBytes int64
	StageInlineMaxBytes  int64
}

type CodexConfig struct {
	SandboxImage         string
	PromptProfilesDir    string
	Network              string
	MaxOutputBytes       int
	WritableTmp          bool
	IncludePriorFindings bool
	Env                  map[string]string
	ExtraArgs            []string
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
	ScanPath              string
	DBPath                string
	PromptProfilesDir     string
	DaemonMirrorBackupDir string
}

type fileSettings struct {
	ScanPath              bool
	DBPath                bool
	PromptProfilesDir     bool
	DaemonMirrorBackupDir bool
}

type rawConfig struct {
	ScanPath *string            `yaml:"scan_path"`
	DBPath   *string            `yaml:"db_path"`
	Pipeline *rawPipelineConfig `yaml:"pipeline"`
	Git      *rawGitConfig      `yaml:"git"`
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

type rawGitConfig struct {
	BaseURL      *string `yaml:"base_url"`
	CloneTimeout *string `yaml:"clone_timeout"`
	ShallowClone *bool   `yaml:"shallow_clone"`
	LFSEnabled   *bool   `yaml:"lfs_enabled"`
}

type rawDockerConfig struct {
	ManagedLabel                *string                       `yaml:"managed_label"`
	ComposeProjectPrefix        *string                       `yaml:"compose_project_prefix"`
	KeepFailedContainersMinutes *int                          `yaml:"keep_failed_containers_minutes"`
	HealthCheckTimeoutSeconds   *int                          `yaml:"health_check_timeout_seconds"`
	PullPolicy                  *string                       `yaml:"pull_policy"`
	DaemonMirrors               *rawDockerDaemonMirrorsConfig `yaml:"daemon_mirrors"`
	BuildMirrors                *rawDockerBuildMirrorsConfig  `yaml:"build_mirrors"`
	CleanupPolicy               *string                       `yaml:"cleanup_policy"`
	CleanupImages               *bool                         `yaml:"cleanup_images"`
	CleanupVolumes              *bool                         `yaml:"cleanup_volumes"`
	CleanupBuildCache           *bool                         `yaml:"cleanup_build_cache"`
	BuildCachePruneUntil        *string                       `yaml:"build_cache_prune_until"`
	KeepRuntime                 *bool                         `yaml:"keep_runtime"`
	GC                          *rawDockerGCConfig            `yaml:"gc"`
}

type rawDockerDaemonMirrorsConfig struct {
	Enabled            *bool    `yaml:"enabled"`
	DaemonJSON         *string  `yaml:"daemon_json"`
	BackupDir          *string  `yaml:"backup_dir"`
	RegistryMirrors    []string `yaml:"registry_mirrors"`
	RequireManualApply *bool    `yaml:"require_manual_apply"`
}

type rawDockerBuildMirrorsConfig struct {
	Enabled            *bool   `yaml:"enabled"`
	Mode               *string `yaml:"mode"`
	FallbackToOriginal *bool   `yaml:"fallback_to_original"`
	VerifyOverride     *bool   `yaml:"verify_override"`
	Profile            *string `yaml:"profile"`
	AptMirror          *string `yaml:"apt_mirror"`
	UbuntuMirror       *string `yaml:"ubuntu_mirror"`
	ApkMirror          *string `yaml:"apk_mirror"`
	YumMirror          *string `yaml:"yum_mirror"`
	NPMRegistry        *string `yaml:"npm_registry"`
	PipIndexURL        *string `yaml:"pip_index_url"`
	GoProxy            *string `yaml:"go_proxy"`
	CargoRegistry      *string `yaml:"cargo_registry"`
}

type rawDockerGCConfig struct {
	Enabled               *bool   `yaml:"enabled"`
	RunOnTUIStart         *bool   `yaml:"run_on_tui_start"`
	RunBeforeCLIRun       *bool   `yaml:"run_before_cli_run"`
	Interval              *string `yaml:"interval"`
	P2ROnly               *bool   `yaml:"p2r_only"`
	PruneExitedContainers *bool   `yaml:"prune_exited_containers"`
	PruneNetworks         *bool   `yaml:"prune_networks"`
	PruneVolumes          *bool   `yaml:"prune_volumes"`
	PruneImages           *bool   `yaml:"prune_images"`
	PruneBuilderCache     *bool   `yaml:"prune_builder_cache"`
	BuilderCacheUntil     *string `yaml:"builder_cache_until"`
}

type rawDocsConfig struct {
	MaxAttachmentBytes   *int64 `yaml:"max_attachment_bytes"`
	InlineTextLimitBytes *int64 `yaml:"inline_text_limit_bytes"`
	StageInlineMaxBytes  *int64 `yaml:"stage_inline_max_bytes"`
}

type rawCodexConfig struct {
	SandboxImage         *string           `yaml:"sandbox_image"`
	PromptProfilesDir    *string           `yaml:"prompt_profiles_dir"`
	Network              *string           `yaml:"network"`
	MaxOutputBytes       *int              `yaml:"max_output_bytes"`
	WritableTmp          *bool             `yaml:"writable_tmp"`
	IncludePriorFindings *bool             `yaml:"include_prior_findings"`
	Env                  map[string]string `yaml:"env"`
	ExtraArgs            []string          `yaml:"extra_args"`
}

type rawTUIConfig struct {
	RefreshIntervalMS *int `yaml:"refresh_interval_ms"`
	LogMaxLines       *int `yaml:"log_max_lines"`
}

func Default() Config {
	return Config{
		ScanPath:          "./projects-qa",
		DBPath:            "./projects-qa/.qa-control/index.db",
		ProjectConfigPath: ".p2r.yaml",
		Pipeline: PipelineConfig{
			StageTimeouts:      map[string]int{"A": 60, "B": 900, "B_PULL": 300, "B_BUILD": 600, "B_UP": 300, "B_HEALTH": 60, "B_PORT": 30, "C": 300, "D": 2700, "E": 2700, "F": 2700},
			SelfTestReportPath: "repo/self_test_report.md",
			MaxConcurrent:      DefaultMaxConcurrent,
		},
		Git: GitConfig{
			BaseURL:      "https://gitlab.mindflow.com.cn/Prompt2Repo/fullstack/",
			CloneTimeout: 10 * time.Minute,
			ShallowClone: true,
			LFSEnabled:   false,
		},
		Docker: DockerConfig{
			ManagedLabel:                "managed_by=p2rqa",
			ComposeProjectPrefix:        "p2rqa",
			KeepFailedContainersMinutes: 60,
			HealthCheckTimeoutSeconds:   60,
			PullPolicy:                  "best_effort",
			DaemonMirrors: DockerDaemonMirrorsConfig{
				Enabled:            false,
				DaemonJSON:         "/etc/docker/daemon.json",
				RegistryMirrors:    []string{},
				RequireManualApply: true,
			},
			BuildMirrors: DockerBuildMirrorsConfig{
				Enabled:            true,
				Mode:               "patch_dockerfile",
				FallbackToOriginal: true,
				VerifyOverride:     true,
				Profile:            "cn",
				NPMRegistry:        "https://registry.npmmirror.com",
				PipIndexURL:        "https://pypi.tuna.tsinghua.edu.cn/simple",
				GoProxy:            "https://goproxy.cn,direct",
			},
			CleanupPolicy:        "always",
			CleanupImages:        true,
			CleanupVolumes:       true,
			CleanupBuildCache:    false,
			BuildCachePruneUntil: "24h",
			KeepRuntime:          false,
			GC: DockerGCConfig{
				Enabled:               true,
				RunOnTUIStart:         true,
				RunBeforeCLIRun:       false,
				Interval:              "24h",
				P2ROnly:               true,
				PruneExitedContainers: true,
				PruneNetworks:         true,
				PruneVolumes:          false,
				PruneImages:           false,
				PruneBuilderCache:     false,
				BuilderCacheUntil:     "72h",
			},
		},
		Docs: DocsConfig{
			MaxAttachmentBytes:   64 << 20,
			InlineTextLimitBytes: 1 << 20,
			StageInlineMaxBytes:  4 << 20,
		},
		Codex: CodexConfig{
			SandboxImage:         "codex:latest",
			PromptProfilesDir:    "./projects-qa/.qa-control/prompt_profiles",
			Network:              "none",
			MaxOutputBytes:       1048576,
			WritableTmp:          false,
			IncludePriorFindings: false,
			Env:                  map[string]string{},
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
		ScanPath:              cwd,
		DBPath:                cwd,
		PromptProfilesDir:     cwd,
		DaemonMirrorBackupDir: cwd,
	}
	cfg.ProjectConfigPath = filepath.Join(cwd, ".p2r.yaml")

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
		if settings.DaemonMirrorBackupDir {
			bases.DaemonMirrorBackupDir = base
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
	if strings.TrimSpace(cfg.Docker.DaemonMirrors.BackupDir) == "" {
		cfg.Docker.DaemonMirrors.BackupDir = filepath.Join(cfg.ScanPath, ".qa-control", "docker-daemon-backups")
	} else {
		cfg.Docker.DaemonMirrors.BackupDir = absFrom(bases.DaemonMirrorBackupDir, cfg.Docker.DaemonMirrors.BackupDir)
	}
	cfg.ProjectConfigPath = absFrom(cwd, cfg.ProjectConfigPath)
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
	if raw.Git != nil {
		if raw.Git.BaseURL != nil {
			cfg.Git.BaseURL = *raw.Git.BaseURL
		}
		if raw.Git.CloneTimeout != nil {
			duration, err := time.ParseDuration(strings.TrimSpace(*raw.Git.CloneTimeout))
			if err != nil {
				return fmt.Errorf("git.clone_timeout must be a Go duration: %w", err)
			}
			cfg.Git.CloneTimeout = duration
		}
		if raw.Git.ShallowClone != nil {
			cfg.Git.ShallowClone = *raw.Git.ShallowClone
		}
		if raw.Git.LFSEnabled != nil {
			cfg.Git.LFSEnabled = *raw.Git.LFSEnabled
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
		if raw.Docker.PullPolicy != nil {
			cfg.Docker.PullPolicy = *raw.Docker.PullPolicy
		}
		if raw.Docker.DaemonMirrors != nil {
			applyRawDockerDaemonMirrors(&cfg.Docker.DaemonMirrors, raw.Docker.DaemonMirrors, settings)
		}
		if raw.Docker.BuildMirrors != nil {
			applyRawDockerBuildMirrors(&cfg.Docker.BuildMirrors, raw.Docker.BuildMirrors)
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
		if raw.Docker.GC != nil {
			applyRawDockerGC(&cfg.Docker.GC, raw.Docker.GC)
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
		if raw.Codex.IncludePriorFindings != nil {
			cfg.Codex.IncludePriorFindings = *raw.Codex.IncludePriorFindings
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

func applyRawDockerDaemonMirrors(cfg *DockerDaemonMirrorsConfig, raw *rawDockerDaemonMirrorsConfig, settings *fileSettings) {
	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
	}
	if raw.DaemonJSON != nil {
		cfg.DaemonJSON = *raw.DaemonJSON
	}
	if raw.BackupDir != nil {
		cfg.BackupDir = *raw.BackupDir
		settings.DaemonMirrorBackupDir = true
	}
	if raw.RegistryMirrors != nil {
		cfg.RegistryMirrors = append([]string(nil), raw.RegistryMirrors...)
	}
	if raw.RequireManualApply != nil {
		cfg.RequireManualApply = *raw.RequireManualApply
	}
}

func applyRawDockerBuildMirrors(cfg *DockerBuildMirrorsConfig, raw *rawDockerBuildMirrorsConfig) {
	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
	}
	if raw.Mode != nil {
		cfg.Mode = *raw.Mode
	}
	if raw.FallbackToOriginal != nil {
		cfg.FallbackToOriginal = *raw.FallbackToOriginal
	}
	if raw.VerifyOverride != nil {
		cfg.VerifyOverride = *raw.VerifyOverride
	}
	if raw.Profile != nil {
		cfg.Profile = *raw.Profile
	}
	if raw.AptMirror != nil {
		cfg.AptMirror = *raw.AptMirror
	}
	if raw.UbuntuMirror != nil {
		cfg.UbuntuMirror = *raw.UbuntuMirror
	}
	if raw.ApkMirror != nil {
		cfg.ApkMirror = *raw.ApkMirror
	}
	if raw.YumMirror != nil {
		cfg.YumMirror = *raw.YumMirror
	}
	if raw.NPMRegistry != nil {
		cfg.NPMRegistry = *raw.NPMRegistry
	}
	if raw.PipIndexURL != nil {
		cfg.PipIndexURL = *raw.PipIndexURL
	}
	if raw.GoProxy != nil {
		cfg.GoProxy = *raw.GoProxy
	}
	if raw.CargoRegistry != nil {
		cfg.CargoRegistry = *raw.CargoRegistry
	}
}

func applyRawDockerGC(cfg *DockerGCConfig, raw *rawDockerGCConfig) {
	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
	}
	if raw.RunOnTUIStart != nil {
		cfg.RunOnTUIStart = *raw.RunOnTUIStart
	}
	if raw.RunBeforeCLIRun != nil {
		cfg.RunBeforeCLIRun = *raw.RunBeforeCLIRun
	}
	if raw.Interval != nil {
		cfg.Interval = *raw.Interval
	}
	if raw.P2ROnly != nil {
		cfg.P2ROnly = *raw.P2ROnly
	}
	if raw.PruneExitedContainers != nil {
		cfg.PruneExitedContainers = *raw.PruneExitedContainers
	}
	if raw.PruneNetworks != nil {
		cfg.PruneNetworks = *raw.PruneNetworks
	}
	if raw.PruneVolumes != nil {
		cfg.PruneVolumes = *raw.PruneVolumes
	}
	if raw.PruneImages != nil {
		cfg.PruneImages = *raw.PruneImages
	}
	if raw.PruneBuilderCache != nil {
		cfg.PruneBuilderCache = *raw.PruneBuilderCache
	}
	if raw.BuilderCacheUntil != nil {
		cfg.BuilderCacheUntil = *raw.BuilderCacheUntil
	}
}

func normalize(cfg *Config) {
	cfg.Pipeline.MaxConcurrent = NormalizeMaxConcurrent(cfg.Pipeline.MaxConcurrent)
	cfg.Git.BaseURL = strings.TrimSpace(cfg.Git.BaseURL)
	if cfg.Git.CloneTimeout <= 0 {
		cfg.Git.CloneTimeout = 10 * time.Minute
	}
}

func NormalizeMaxConcurrent(value int) int {
	if value <= 0 {
		return DefaultMaxConcurrent
	}
	if value > MaxConcurrentLimit {
		return MaxConcurrentLimit
	}
	return value
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
	if strings.TrimSpace(cfg.Git.BaseURL) == "" {
		return fmt.Errorf("git.base_url must not be empty")
	}
	if !validGitBaseURL(cfg.Git.BaseURL) {
		return fmt.Errorf("git.base_url must be an absolute URL")
	}
	if cfg.Git.CloneTimeout <= 0 {
		return fmt.Errorf("git.clone_timeout must be greater than 0")
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
	if err := validateOneOf("docker.pull_policy", cfg.Docker.PullPolicy, "required", "best_effort", "skip"); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Docker.DaemonMirrors.DaemonJSON) == "" {
		return fmt.Errorf("docker.daemon_mirrors.daemon_json must not be empty")
	}
	if strings.TrimSpace(cfg.Docker.DaemonMirrors.BackupDir) == "" {
		return fmt.Errorf("docker.daemon_mirrors.backup_dir must not be empty")
	}
	if err := validateOneOf("docker.build_mirrors.mode", cfg.Docker.BuildMirrors.Mode, "off", "env_only", "patch_dockerfile"); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Docker.CleanupPolicy) == "" {
		return fmt.Errorf("docker.cleanup_policy must not be empty")
	}
	if _, err := time.ParseDuration(cfg.Docker.BuildCachePruneUntil); err != nil {
		return fmt.Errorf("docker.build_cache_prune_until must be a Go duration: %w", err)
	}
	if _, err := time.ParseDuration(cfg.Docker.GC.Interval); err != nil {
		return fmt.Errorf("docker.gc.interval must be a Go duration: %w", err)
	}
	if _, err := time.ParseDuration(cfg.Docker.GC.BuilderCacheUntil); err != nil {
		return fmt.Errorf("docker.gc.builder_cache_until must be a Go duration: %w", err)
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

func validGitBaseURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" {
		return false
	}
	switch parsed.Scheme {
	case "http", "https", "ssh", "file":
		return parsed.Host != "" || parsed.Scheme == "file"
	default:
		return false
	}
}

func validateOneOf(name, value string, allowed ...string) error {
	value = strings.TrimSpace(value)
	for _, item := range allowed {
		if value == item {
			return nil
		}
	}
	return fmt.Errorf("%s must be one of %s", name, strings.Join(allowed, ", "))
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
