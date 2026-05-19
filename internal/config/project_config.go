package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

func SaveProjectDaemonMirrors(path string, mirrors DockerDaemonMirrorsConfig) error {
	path = filepath.Clean(path)
	data := map[string]any{}
	if content, err := os.ReadFile(path); err == nil && len(content) > 0 {
		if err := yaml.Unmarshal(content, &data); err != nil {
			return err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	dockerNode, _ := data["docker"].(map[string]any)
	if dockerNode == nil {
		dockerNode = map[string]any{}
	}
	dockerNode["daemon_mirrors"] = map[string]any{
		"enabled":              mirrors.Enabled,
		"daemon_json":          mirrors.DaemonJSON,
		"backup_dir":           mirrors.BackupDir,
		"registry_mirrors":     append([]string(nil), mirrors.RegistryMirrors...),
		"require_manual_apply": mirrors.RequireManualApply,
	}
	data["docker"] = dockerNode
	content, err := yaml.Marshal(data)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o644)
}
