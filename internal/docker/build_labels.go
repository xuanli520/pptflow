package docker

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type runtimeLabelOverride struct {
	File     string
	Warnings []string
}

func prepareRuntimeLabelOverride(effectiveConfig, artifactRoot string, labels map[string]string) runtimeLabelOverride {
	labels = cleanLabels(labels)
	if len(labels) == 0 {
		return runtimeLabelOverride{}
	}
	var payload struct {
		Services map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(effectiveConfig), &payload); err != nil {
		return runtimeLabelOverride{Warnings: []string{"runtime label override skipped: parse compose config: " + err.Error()}}
	}
	if len(payload.Services) == 0 {
		return runtimeLabelOverride{Warnings: []string{"runtime label override skipped: compose config has no services"}}
	}
	if strings.TrimSpace(artifactRoot) == "" {
		return runtimeLabelOverride{Warnings: []string{"runtime label override skipped: artifact root is empty"}}
	}
	serviceNames := make([]string, 0, len(payload.Services))
	for name := range payload.Services {
		if strings.TrimSpace(name) != "" {
			serviceNames = append(serviceNames, name)
		}
	}
	sort.Strings(serviceNames)
	if len(serviceNames) == 0 {
		return runtimeLabelOverride{Warnings: []string{"runtime label override skipped: compose config has no named services"}}
	}
	services := map[string]any{}
	for _, name := range serviceNames {
		services[name] = map[string]any{"labels": labels}
	}
	content, err := yaml.Marshal(map[string]any{"services": services})
	if err != nil {
		return runtimeLabelOverride{Warnings: []string{"runtime label override skipped: marshal override: " + err.Error()}}
	}
	if err := os.MkdirAll(artifactRoot, 0o755); err != nil {
		return runtimeLabelOverride{Warnings: []string{"runtime label override skipped: create artifact root: " + err.Error()}}
	}
	path := filepath.Join(artifactRoot, "runtime_labels.compose.yml")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return runtimeLabelOverride{Warnings: []string{fmt.Sprintf("runtime label override skipped: write %s: %v", path, err)}}
	}
	return runtimeLabelOverride{File: path}
}

func cleanLabels(labels map[string]string) map[string]string {
	cleaned := map[string]string{}
	for key, value := range labels {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		cleaned[key] = value
	}
	return cleaned
}
