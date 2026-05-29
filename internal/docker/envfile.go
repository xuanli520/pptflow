package docker

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type runtimeEnvFilePreparation struct {
	Generated   []string
	EnvFiles    []string
	ComposeFile string
	Warnings    []string
}

type runtimeEnvFileServiceOverride struct {
	Service  string
	EnvFiles []*yaml.Node
}

func prepareRuntimeEnvFiles(composeFile, workDir, artifactRoot string) runtimeEnvFilePreparation {
	var result runtimeEnvFilePreparation
	if workDir == "" || fileExists(filepath.Join(workDir, ".env")) {
		return result
	}
	examplePath := filepath.Join(workDir, ".env.example")
	content, err := os.ReadFile(examplePath)
	if os.IsNotExist(err) {
		return result
	}
	if err != nil {
		result.Warnings = append(result.Warnings, "runtime env preparation skipped: "+err.Error())
		return result
	}
	if strings.TrimSpace(artifactRoot) == "" {
		result.Warnings = append(result.Warnings, "runtime env preparation skipped: artifact root is empty")
		return result
	}
	dir := filepath.Join(artifactRoot, "docker_runtime")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		result.Warnings = append(result.Warnings, "runtime env preparation skipped: "+err.Error())
		return result
	}
	envPath := filepath.Join(dir, "runtime.env")
	if err := os.WriteFile(envPath, content, 0o644); err != nil {
		result.Warnings = append(result.Warnings, "runtime env preparation skipped: "+err.Error())
		return result
	}
	result.Generated = append(result.Generated, envPath)
	result.EnvFiles = append(result.EnvFiles, envPath)

	overrides, warnings := runtimeEnvFileOverrides(composeFile, workDir, envPath)
	result.Warnings = append(result.Warnings, warnings...)
	if len(overrides) == 0 {
		return result
	}
	overrideContent, err := marshalRuntimeEnvFileOverride(overrides)
	if err != nil {
		result.Warnings = append(result.Warnings, "runtime env override skipped: "+err.Error())
		return result
	}
	overridePath := filepath.Join(dir, "compose.env.yml")
	if err := os.WriteFile(overridePath, overrideContent, 0o644); err != nil {
		result.Warnings = append(result.Warnings, "runtime env override skipped: "+err.Error())
		return result
	}
	result.ComposeFile = overridePath
	return result
}

func runtimeEnvFileOverrides(composeFile, workDir, runtimeEnvFile string) ([]runtimeEnvFileServiceOverride, []string) {
	content, err := os.ReadFile(composeFile)
	if err != nil {
		return nil, []string{"runtime env override skipped: " + err.Error()}
	}
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil {
		return nil, []string{"runtime env override skipped: parse compose: " + err.Error()}
	}
	services := yamlMappingValue(yamlDocumentRoot(&document), "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return nil, nil
	}
	serviceNodes := map[string]*yaml.Node{}
	for index := 0; index+1 < len(services.Content); index += 2 {
		name := strings.TrimSpace(services.Content[index].Value)
		if name != "" {
			serviceNodes[name] = services.Content[index+1]
		}
	}
	names := make([]string, 0, len(serviceNodes))
	for name := range serviceNodes {
		names = append(names, name)
	}
	sort.Strings(names)
	var result []runtimeEnvFileServiceOverride
	var warnings []string
	for _, name := range names {
		envFile := yamlMappingValue(serviceNodes[name], "env_file")
		envFiles, changed, ok := rewriteRuntimeEnvFileEntries(envFile, workDir, runtimeEnvFile)
		if !ok {
			warnings = append(warnings, "runtime env override skipped unsupported env_file for service "+name)
			continue
		}
		if changed {
			result = append(result, runtimeEnvFileServiceOverride{Service: name, EnvFiles: envFiles})
		}
	}
	return result, warnings
}

func rewriteRuntimeEnvFileEntries(node *yaml.Node, workDir, runtimeEnvFile string) ([]*yaml.Node, bool, bool) {
	if node == nil {
		return nil, false, true
	}
	switch node.Kind {
	case yaml.ScalarNode:
		if referencesRuntimeDotEnv(node.Value, workDir) {
			return []*yaml.Node{yamlScalar(runtimeEnvFile)}, true, true
		}
		return []*yaml.Node{cloneYAMLNode(node)}, false, true
	case yaml.SequenceNode:
		entries := make([]*yaml.Node, 0, len(node.Content))
		changed := false
		for _, item := range node.Content {
			path, ok := runtimeEnvFileEntryPath(item)
			if !ok {
				return nil, false, false
			}
			if referencesRuntimeDotEnv(path, workDir) {
				entries = append(entries, runtimeEnvFileEntryWithPath(item, runtimeEnvFile))
				changed = true
			} else {
				entries = append(entries, cloneYAMLNode(item))
			}
		}
		return entries, changed, true
	default:
		return nil, false, false
	}
}

func runtimeEnvFileEntryPath(node *yaml.Node) (string, bool) {
	if node == nil {
		return "", false
	}
	switch node.Kind {
	case yaml.ScalarNode:
		return strings.TrimSpace(node.Value), true
	case yaml.MappingNode:
		path := yamlMappingValue(node, "path")
		if path == nil || path.Kind != yaml.ScalarNode {
			return "", false
		}
		value := strings.TrimSpace(path.Value)
		return value, value != ""
	default:
		return "", false
	}
}

func runtimeEnvFileEntryWithPath(node *yaml.Node, path string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return yamlScalar(path)
	}
	clone := cloneYAMLNode(node)
	for index := 0; index+1 < len(clone.Content); index += 2 {
		if clone.Content[index].Value == "path" {
			clone.Content[index+1] = yamlScalar(path)
			return clone
		}
	}
	clone.Content = append(clone.Content, yamlScalar("path"), yamlScalar(path))
	return clone
}

func yamlDocumentRoot(document *yaml.Node) *yaml.Node {
	if document == nil {
		return nil
	}
	if document.Kind == yaml.DocumentNode && len(document.Content) > 0 {
		return document.Content[0]
	}
	return document
}

func yamlMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

func cloneYAMLNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	clone := *node
	if len(node.Content) > 0 {
		clone.Content = make([]*yaml.Node, 0, len(node.Content))
		for _, child := range node.Content {
			clone.Content = append(clone.Content, cloneYAMLNode(child))
		}
	}
	return &clone
}

func referencesRuntimeDotEnv(value, workDir string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(workDir, path)
	}
	return filepath.Clean(path) == filepath.Join(filepath.Clean(workDir), ".env")
}

func marshalRuntimeEnvFileOverride(overrides []runtimeEnvFileServiceOverride) ([]byte, error) {
	root := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	services := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	root.Content = append(root.Content, yamlScalar("services"), &services)
	for _, override := range overrides {
		service := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		envFiles := yaml.Node{Kind: yaml.SequenceNode, Tag: "!override"}
		for _, value := range override.EnvFiles {
			envFiles.Content = append(envFiles.Content, cloneYAMLNode(value))
		}
		service.Content = append(service.Content, yamlScalar("env_file"), &envFiles)
		services.Content = append(services.Content, yamlScalar(override.Service), &service)
	}
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		_ = encoder.Close()
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
