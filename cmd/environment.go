package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var allowedHarborEnvironment = map[string]bool{
	"ANTHROPIC_AUTH_TOKEN":    true,
	"ANTHROPIC_API_KEY":       true,
	"CLAUDE_CODE_OAUTH_TOKEN": true,
	"ANTHROPIC_BASE_URL":      true,
	"QWEN_HARBOR_BASE_URL":    true,
	"OPUS_HARBOR_BASE_URL":    true,
	"GITHUB_TOKEN":            true,
}

func loadHarborEnvironment() error {
	path, err := harborEnvironmentPath()
	if err != nil {
		return err
	}
	if strings.TrimSpace(path) == "" {
		return nil
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Harbor environment file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Harbor environment path is not a regular file: %s", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("Harbor environment file must not be accessible by group or others: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open Harbor environment file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !allowedHarborEnvironment[key] {
			continue
		}
		if strings.TrimSpace(os.Getenv(key)) != "" {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		if value != "" {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("load Harbor environment variable %s: %w", key, err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Harbor environment file: %w", err)
	}
	return nil
}

func harborEnvironmentPath() (string, error) {
	if path := strings.TrimSpace(os.Getenv("HARBOR_FACTORY_ENV_FILE")); path != "" {
		return path, nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(configDir, "harbor-factory", "env"), nil
}
