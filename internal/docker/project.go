package docker

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type ComposeProjectSelection struct {
	Name      string   `json:"name"`
	Source    string   `json:"source"`
	Generated string   `json:"generated,omitempty"`
	Declared  string   `json:"declared,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

type ComposeProjectSelectionRequest struct {
	Prefix        string
	TaskID        string
	RunID         string
	ComposeFiles  []string
	WorkDir       string
	ReadmeCommand []string
	Env           []string
	EnvFiles      []string
}

var composeProjectNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func SelectComposeProjectName(req ComposeProjectSelectionRequest) ComposeProjectSelection {
	generated := ComposeProjectName(req.Prefix, req.TaskID, req.RunID)
	selection := ComposeProjectSelection{Name: generated, Source: "generated", Generated: generated}
	if name, ok := composeProjectFlag(req.ReadmeCommand); ok {
		selection.Name = name
		selection.Source = "readme_project_flag"
		selection.Declared = name
		return selection
	}
	env := composeProjectEnv(req.WorkDir, req.Env, req.EnvFiles)
	if name := strings.TrimSpace(env["COMPOSE_PROJECT_NAME"]); name != "" {
		selection.Declared = name
		if validComposeProjectName(name) {
			selection.Name = name
			selection.Source = "compose_project_env"
			return selection
		}
		selection.Warnings = append(selection.Warnings, "COMPOSE_PROJECT_NAME is not a valid Docker Compose project name; using generated p2r project name")
	}
	if len(req.ComposeFiles) == 0 {
		return selection
	}
	declared, ok, warnings := declaredComposeProjectName(req.ComposeFiles, env)
	selection.Warnings = append(selection.Warnings, warnings...)
	if !ok {
		return selection
	}
	selection.Declared = declared
	if validComposeProjectName(declared) {
		selection.Name = declared
		selection.Source = "compose_name"
		return selection
	}
	selection.Warnings = append(selection.Warnings, "declared compose project name is not a valid Docker Compose project name; using generated p2r project name")
	return selection
}

func composeProjectFlag(fields []string) (string, bool) {
	for index := 0; index < len(fields); index++ {
		field := strings.TrimSpace(fields[index])
		switch {
		case field == "-p" || field == "--project-name":
			if index+1 < len(fields) {
				name := strings.TrimSpace(fields[index+1])
				return name, validComposeProjectName(name)
			}
			return "", false
		case strings.HasPrefix(field, "-p="):
			name := strings.TrimSpace(strings.TrimPrefix(field, "-p="))
			return name, validComposeProjectName(name)
		case strings.HasPrefix(field, "--project-name="):
			name := strings.TrimSpace(strings.TrimPrefix(field, "--project-name="))
			return name, validComposeProjectName(name)
		}
	}
	return "", false
}

func declaredComposeProjectName(composeFiles []string, env map[string]string) (string, bool, []string) {
	var raw string
	var warnings []string
	for _, composeFile := range composeFiles {
		composeFile = strings.TrimSpace(composeFile)
		if composeFile == "" {
			continue
		}
		content, err := os.ReadFile(composeFile)
		if err != nil {
			warnings = append(warnings, "declared compose project name skipped: "+err.Error())
			continue
		}
		var document yaml.Node
		if err := yaml.Unmarshal(content, &document); err != nil {
			warnings = append(warnings, "declared compose project name skipped: parse compose: "+err.Error())
			continue
		}
		node := yamlMappingValue(yamlDocumentRoot(&document), "name")
		if node == nil || node.Kind != yaml.ScalarNode {
			continue
		}
		value := strings.TrimSpace(node.Value)
		if value != "" {
			raw = value
		}
	}
	if raw == "" {
		return "", false, warnings
	}
	resolved := resolveComposeProjectNameTemplate(raw, env)
	if strings.TrimSpace(resolved) == "" {
		warnings = append(warnings, "declared compose project name resolved to an empty value; using generated p2r project name")
		return "", false, warnings
	}
	return strings.TrimSpace(resolved), true, warnings
}

func resolveComposeProjectNameTemplate(value string, env map[string]string) string {
	value = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-|-)([^}]*)\}`).ReplaceAllStringFunc(value, func(match string) string {
		parts := regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)(:-|-)([^}]*)\}$`).FindStringSubmatch(match)
		if len(parts) != 4 {
			return match
		}
		if current, ok := env[parts[1]]; ok && (parts[2] == "-" || current != "") {
			return current
		}
		return parts[3]
	})
	return os.Expand(value, func(key string) string {
		return env[key]
	})
}

func composeProjectEnv(workDir string, commandEnv, envFiles []string) map[string]string {
	env := map[string]string{}
	loadFile := func(path string) {
		content, err := os.ReadFile(path)
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(content), "\n") {
			key, value, ok := parseComposeEnvLine(line)
			if ok {
				env[key] = value
			}
		}
	}
	workDir = strings.TrimSpace(workDir)
	if len(envFiles) > 0 {
		for _, envFile := range envFiles {
			if strings.TrimSpace(envFile) != "" {
				loadFile(envFile)
			}
		}
	} else if workDir != "" {
		loadFile(filepath.Join(workDir, ".env"))
	}
	for _, item := range commandEnv {
		key, value, ok := strings.Cut(item, "=")
		key = strings.TrimSpace(key)
		if ok && key != "" {
			env[key] = value
		}
	}
	return env
}

func parseComposeEnvLine(line string) (string, string, bool) {
	line = strings.TrimSpace(strings.TrimRight(line, "\r"))
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	key, value, ok := strings.Cut(line, "=")
	key = strings.TrimSpace(key)
	if !ok || !validComposeEnvName(key) {
		return "", "", false
	}
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		quote := value[0]
		if (quote == '\'' || quote == '"') && value[len(value)-1] == quote {
			value = value[1 : len(value)-1]
		}
	}
	return key, value, true
}

func validComposeEnvName(key string) bool {
	if key == "" {
		return false
	}
	for index, char := range key {
		if char == '_' || char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || index > 0 && char >= '0' && char <= '9' {
			continue
		}
		return false
	}
	return true
}

func validComposeProjectName(name string) bool {
	name = strings.TrimSpace(name)
	return len(name) <= 63 && composeProjectNamePattern.MatchString(name)
}
