package docker

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/xuanli520/p2r_tui/internal/config"
	"gopkg.in/yaml.v3"
)

type buildMirrorPreparation struct {
	Summary      BuildMirrorSummary
	ComposeFiles []string
}

func prepareBuildMirror(repoPath, composeFile, artifactRoot string, cfg config.DockerConfig) buildMirrorPreparation {
	summary := BuildMirrorSummary{
		Enabled:      cfg.BuildMirrors.Enabled,
		Mode:         cfg.BuildMirrors.Mode,
		Profile:      cfg.BuildMirrors.Profile,
		RepoModified: false,
		ComposeFile:  composeFile,
		ComposeFiles: []string{composeFile},
	}
	if !cfg.BuildMirrors.Enabled || cfg.BuildMirrors.Mode == "off" {
		summary.Enabled = cfg.BuildMirrors.Enabled
		summary.Mode = cfg.BuildMirrors.Mode
		return buildMirrorPreparation{Summary: summary, ComposeFiles: []string{composeFile}}
	}
	if strings.TrimSpace(composeFile) == "" {
		summary.Warnings = append(summary.Warnings, "README compose command mode: Dockerfile patch skipped")
		return buildMirrorPreparation{Summary: summary, ComposeFiles: []string{}}
	}
	definitions, err := composeBuildDefinitions(repoPath, composeFile)
	if err != nil {
		summary.Warnings = append(summary.Warnings, err.Error())
		return buildMirrorPreparation{Summary: summary, ComposeFiles: []string{composeFile}}
	}
	if len(definitions) == 0 {
		summary.Warnings = append(summary.Warnings, "no compose build definitions eligible for mirror override")
		return buildMirrorPreparation{Summary: summary, ComposeFiles: []string{composeFile}}
	}
	mirrorDir := filepath.Join(artifactRoot, "docker_mirror")
	if err := os.MkdirAll(mirrorDir, 0o755); err != nil {
		summary.Warnings = append(summary.Warnings, "create docker mirror artifact dir: "+err.Error())
		return buildMirrorPreparation{Summary: summary, ComposeFiles: []string{composeFile}}
	}
	override := map[string]any{"services": map[string]any{}}
	servicesNode := override["services"].(map[string]any)
	patchedCount := 0
	for _, definition := range definitions {
		serviceSummary := BuildMirrorServiceSummary{
			Service:            definition.Service,
			Context:            definition.Context,
			OriginalDockerfile: definition.Dockerfile,
		}
		if definition.SkippedReason != "" {
			serviceSummary.SkippedReason = definition.SkippedReason
			summary.Services = append(summary.Services, serviceSummary)
			continue
		}
		build := copyMap(definition.Build)
		build["context"] = definition.Context
		if cfg.BuildMirrors.Mode == "env_only" {
			build["args"] = mergeBuildArgs(build["args"], buildMirrorArgs(cfg.BuildMirrors))
			serviceSummary.Patched = false
			serviceSummary.Injected = sortedKeys(buildMirrorArgs(cfg.BuildMirrors))
			servicesNode[definition.Service] = map[string]any{"build": build}
			summary.Services = append(summary.Services, serviceSummary)
			patchedCount++
			continue
		}
		patch, err := patchDockerfile(definition.Dockerfile, filepath.Join(mirrorDir, safeArtifactName(definition.Service)+".Dockerfile.p2r"), cfg.BuildMirrors)
		if err != nil {
			serviceSummary.SkippedReason = err.Error()
			summary.Services = append(summary.Services, serviceSummary)
			continue
		}
		serviceSummary.PatchedDockerfile = patch.Path
		serviceSummary.Patched = patch.Patched
		serviceSummary.Injected = patch.Injected
		serviceSummary.Warnings = patch.Warnings
		if !patch.Patched {
			if serviceSummary.SkippedReason == "" {
				serviceSummary.SkippedReason = "no supported package manager RUN instruction found"
			}
			summary.Services = append(summary.Services, serviceSummary)
			continue
		}
		build["dockerfile"] = patch.Path
		servicesNode[definition.Service] = map[string]any{"build": build}
		summary.Services = append(summary.Services, serviceSummary)
		patchedCount++
	}
	if patchedCount == 0 {
		summary.Warnings = append(summary.Warnings, "no build mirror override generated")
		return buildMirrorPreparation{Summary: summary, ComposeFiles: []string{composeFile}}
	}
	if cfg.BuildMirrors.Mode == "env_only" {
		summary.Coverage = "requires Dockerfile ARG usage; does not affect arbitrary RUN environment"
	}
	content, err := yaml.Marshal(override)
	if err != nil {
		summary.Warnings = append(summary.Warnings, "marshal mirror override: "+err.Error())
		return buildMirrorPreparation{Summary: summary, ComposeFiles: []string{composeFile}}
	}
	overrideFile := filepath.Join(mirrorDir, "compose.mirror.override.yml")
	if err := os.WriteFile(overrideFile, content, 0o644); err != nil {
		summary.Warnings = append(summary.Warnings, "write mirror override: "+err.Error())
		return buildMirrorPreparation{Summary: summary, ComposeFiles: []string{composeFile}}
	}
	summary.OverrideFile = overrideFile
	summary.OverrideGenerated = true
	summary.ComposeFiles = []string{composeFile, overrideFile}
	return buildMirrorPreparation{Summary: summary, ComposeFiles: []string{composeFile, overrideFile}}
}

type composeBuildDefinition struct {
	Service       string
	Context       string
	Dockerfile    string
	Build         map[string]any
	SkippedReason string
}

func composeBuildDefinitions(repoPath, composeFile string) ([]composeBuildDefinition, error) {
	content, err := os.ReadFile(composeFile)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := yaml.Unmarshal(content, &payload); err != nil {
		return nil, err
	}
	services, ok := payload["services"].(map[string]any)
	if !ok {
		return nil, nil
	}
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	var result []composeBuildDefinition
	for _, name := range names {
		service, _ := services[name].(map[string]any)
		if len(service) == 0 {
			continue
		}
		if service["extends"] != nil {
			result = append(result, composeBuildDefinition{Service: name, SkippedReason: "extends build definition is skipped"})
			continue
		}
		if service["dockerfile_inline"] != nil {
			result = append(result, composeBuildDefinition{Service: name, SkippedReason: "dockerfile_inline is skipped"})
			continue
		}
		buildValue, exists := service["build"]
		if !exists {
			continue
		}
		definition := composeBuildDefinition{Service: name, Build: map[string]any{}}
		switch typed := buildValue.(type) {
		case string:
			definition.Build["context"] = typed
		case map[string]any:
			definition.Build = copyMap(typed)
		default:
			definition.SkippedReason = "unsupported compose build definition"
		}
		contextValue, _ := definition.Build["context"].(string)
		contextValue = strings.TrimSpace(contextValue)
		if contextValue == "" {
			definition.SkippedReason = "empty build context is skipped"
			result = append(result, definition)
			continue
		}
		if remoteBuildContext(contextValue) {
			definition.SkippedReason = "remote build context is skipped"
			result = append(result, definition)
			continue
		}
		contextPath := contextValue
		if !filepath.IsAbs(contextPath) {
			contextPath = filepath.Join(filepath.Dir(composeFile), contextPath)
		}
		contextPath = filepath.Clean(contextPath)
		if !pathWithinRoot(contextPath, repoPath) {
			definition.SkippedReason = "build context outside repo is skipped"
			result = append(result, definition)
			continue
		}
		dockerfileValue, _ := definition.Build["dockerfile"].(string)
		if strings.TrimSpace(dockerfileValue) == "" {
			dockerfileValue = "Dockerfile"
		}
		dockerfilePath := dockerfileValue
		if !filepath.IsAbs(dockerfilePath) {
			dockerfilePath = filepath.Join(contextPath, dockerfileValue)
		}
		dockerfilePath = filepath.Clean(dockerfilePath)
		if !pathWithinRoot(dockerfilePath, repoPath) {
			definition.SkippedReason = "Dockerfile outside repo is skipped"
			result = append(result, definition)
			continue
		}
		if _, err := os.Stat(dockerfilePath); err != nil {
			definition.SkippedReason = "Dockerfile not found"
			result = append(result, definition)
			continue
		}
		definition.Context = contextPath
		definition.Dockerfile = dockerfilePath
		result = append(result, definition)
	}
	return result, nil
}

type dockerfilePatchResult struct {
	Path     string
	Patched  bool
	Injected []string
	Warnings []string
}

func patchDockerfile(source, target string, mirrors config.DockerBuildMirrorsConfig) (dockerfilePatchResult, error) {
	content, err := os.ReadFile(source)
	if err != nil {
		return dockerfilePatchResult{}, err
	}
	parsed, err := parser.Parse(bytes.NewReader(content))
	if err != nil {
		return dockerfilePatchResult{}, err
	}
	if _, _, err := instructions.Parse(parsed.AST, nil); err != nil {
		return dockerfilePatchResult{}, err
	}
	stages := dockerfileStages(parsed.AST)
	lines := splitDockerfileLines(string(content))
	insertions := map[int][]string{}
	injectedSet := map[string]bool{}
	var warnings []string
	for _, stage := range stages {
		if stage.Skip {
			continue
		}
		stageManagers := map[string]int{}
		for _, run := range stage.Runs {
			managers := detectPackageManagers(run.Text)
			for _, manager := range managers {
				if _, ok := stageManagers[manager]; !ok {
					stageManagers[manager] = run.StartLine
				}
			}
			if strings.Contains(strings.ToLower(run.Text), "install.sh") {
				warnings = append(warnings, "RUN install script detected; deep mirror rewrite skipped for script body")
			}
		}
		if len(stageManagers) == 0 {
			continue
		}
		insertLine := 0
		var managers []string
		for manager, line := range stageManagers {
			if injectionAvailable(manager, mirrors) {
				managers = append(managers, manager)
				if insertLine == 0 || line < insertLine {
					insertLine = line
				}
				injectedSet[manager] = true
			} else {
				warnings = append(warnings, fmt.Sprintf("%s mirror is empty; injection skipped", manager))
			}
		}
		if insertLine > 0 {
			sort.Strings(managers)
			insertions[insertLine] = append(insertions[insertLine], dockerfileInjectionLines(managers, mirrors)...)
		}
	}
	if len(insertions) == 0 {
		return dockerfilePatchResult{Path: target, Warnings: warnings}, nil
	}
	var out []string
	for index, line := range lines {
		lineNo := index + 1
		if block := insertions[lineNo]; len(block) > 0 {
			out = append(out, block...)
		}
		out = append(out, line)
	}
	if err := os.WriteFile(target, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		return dockerfilePatchResult{}, err
	}
	return dockerfilePatchResult{Path: target, Patched: true, Injected: sortedKeys(injectedSet), Warnings: warnings}, nil
}

type dockerfileStage struct {
	Base string
	Skip bool
	Runs []dockerfileRun
}

type dockerfileRun struct {
	StartLine int
	Text      string
}

func dockerfileStages(ast *parser.Node) []dockerfileStage {
	var stages []dockerfileStage
	current := -1
	for _, node := range ast.Children {
		switch strings.ToUpper(node.Value) {
		case "FROM":
			base := dockerfileInstructionText(node)
			stage := dockerfileStage{Base: base, Skip: skipDockerfileStage(base)}
			stages = append(stages, stage)
			current = len(stages) - 1
		case "RUN":
			if current < 0 {
				continue
			}
			stages[current].Runs = append(stages[current].Runs, dockerfileRun{
				StartLine: node.StartLine,
				Text:      dockerfileInstructionText(node),
			})
		}
	}
	return stages
}

func dockerfileInstructionText(node *parser.Node) string {
	if node == nil {
		return ""
	}
	if strings.TrimSpace(node.Original) != "" {
		return node.Original
	}
	if node.Next != nil {
		return node.Next.Value
	}
	return node.Value
}

func skipDockerfileStage(from string) bool {
	lower := strings.ToLower(from)
	return strings.Contains(lower, "from scratch") || strings.Contains(lower, "distroless")
}

func detectPackageManagers(run string) []string {
	lower := strings.ToLower(run)
	var result []string
	if strings.Contains(lower, "apt-get ") || strings.Contains(lower, "apt ") {
		result = append(result, "apt")
	}
	if strings.Contains(lower, "apk add") {
		result = append(result, "apk")
	}
	if strings.Contains(lower, "yum install") || strings.Contains(lower, "dnf install") {
		result = append(result, "yum")
	}
	if strings.Contains(lower, "pip install") || strings.Contains(lower, "python -m pip") {
		result = append(result, "pip")
	}
	if strings.Contains(lower, "npm install") || strings.Contains(lower, "npm ci") || strings.Contains(lower, "yarn ") || strings.Contains(lower, "pnpm ") {
		result = append(result, "npm")
	}
	if strings.Contains(lower, "go mod download") || strings.Contains(lower, "go build") || strings.Contains(lower, "go test") {
		result = append(result, "go")
	}
	if strings.Contains(lower, "cargo build") || strings.Contains(lower, "cargo test") || strings.Contains(lower, "cargo fetch") {
		result = append(result, "cargo")
	}
	return result
}

func injectionAvailable(manager string, mirrors config.DockerBuildMirrorsConfig) bool {
	switch manager {
	case "apt":
		return mirrors.AptMirror != "" || mirrors.UbuntuMirror != ""
	case "apk":
		return mirrors.ApkMirror != ""
	case "yum":
		return mirrors.YumMirror != ""
	case "pip":
		return mirrors.PipIndexURL != ""
	case "npm":
		return mirrors.NPMRegistry != ""
	case "go":
		return mirrors.GoProxy != ""
	case "cargo":
		return mirrors.CargoRegistry != ""
	default:
		return false
	}
}

func dockerfileInjectionLines(managers []string, mirrors config.DockerBuildMirrorsConfig) []string {
	var lines []string
	for _, manager := range managers {
		switch manager {
		case "apt":
			lines = append(lines, aptInjectionLine(mirrors))
		case "apk":
			lines = append(lines, fmt.Sprintf("RUN sed -i 's#https://dl-cdn.alpinelinux.org/alpine#%s#g' /etc/apk/repositories", mirrors.ApkMirror))
		case "yum":
			lines = append(lines, fmt.Sprintf("RUN sed -i 's#http://mirror.centos.org#%s#g; s#https://mirror.centos.org#%s#g' /etc/yum.repos.d/*.repo || true", mirrors.YumMirror, mirrors.YumMirror))
		case "pip":
			lines = append(lines, "ENV PIP_INDEX_URL="+mirrors.PipIndexURL)
			if host := trustedHost(mirrors.PipIndexURL); host != "" {
				lines = append(lines, "ENV PIP_TRUSTED_HOST="+host)
			}
		case "npm":
			lines = append(lines, "ENV NPM_CONFIG_REGISTRY="+mirrors.NPMRegistry)
		case "go":
			lines = append(lines, "ENV GOPROXY="+mirrors.GoProxy)
		case "cargo":
			lines = append(lines, fmt.Sprintf("RUN mkdir -p \"${CARGO_HOME:-/usr/local/cargo}\" && printf '[source.crates-io]\\nreplace-with = \"p2r-mirror\"\\n[source.p2r-mirror]\\nregistry = \"%s\"\\n' > \"${CARGO_HOME:-/usr/local/cargo}/config.toml\"", mirrors.CargoRegistry))
		}
	}
	return lines
}

func aptInjectionLine(mirrors config.DockerBuildMirrorsConfig) string {
	aptMirror := mirrors.AptMirror
	if aptMirror == "" {
		aptMirror = mirrors.UbuntuMirror
	}
	ubuntuMirror := mirrors.UbuntuMirror
	if ubuntuMirror == "" {
		ubuntuMirror = mirrors.AptMirror
	}
	return fmt.Sprintf("RUN set -eux; if [ -f /etc/apt/sources.list ]; then sed -i 's#http://deb.debian.org/debian#%s#g; s#http://security.debian.org/debian-security#%s#g; s#http://archive.ubuntu.com/ubuntu#%s#g; s#http://security.ubuntu.com/ubuntu#%s#g' /etc/apt/sources.list; fi; if ls /etc/apt/sources.list.d/*.sources >/dev/null 2>&1; then sed -i 's#http://deb.debian.org/debian#%s#g; s#http://security.debian.org/debian-security#%s#g; s#http://archive.ubuntu.com/ubuntu#%s#g; s#http://security.ubuntu.com/ubuntu#%s#g' /etc/apt/sources.list.d/*.sources; fi", aptMirror, aptMirror, ubuntuMirror, ubuntuMirror, aptMirror, aptMirror, ubuntuMirror, ubuntuMirror)
}

func trustedHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" {
		return ""
	}
	return parsed.Hostname()
}

func mergeBuildArgs(existing any, values map[string]bool) any {
	args := map[string]any{}
	switch typed := existing.(type) {
	case map[string]any:
		for key, value := range typed {
			args[key] = value
		}
	case []any:
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				continue
			}
			key, value, ok := strings.Cut(text, "=")
			if ok {
				args[key] = value
			} else {
				args[text] = nil
			}
		}
	}
	for key := range values {
		args[key] = ""
	}
	return args
}

func buildMirrorArgs(mirrors config.DockerBuildMirrorsConfig) map[string]bool {
	args := map[string]bool{}
	if mirrors.PipIndexURL != "" {
		args["PIP_INDEX_URL"] = true
	}
	if mirrors.NPMRegistry != "" {
		args["NPM_CONFIG_REGISTRY"] = true
	}
	if mirrors.GoProxy != "" {
		args["GOPROXY"] = true
	}
	if mirrors.CargoRegistry != "" {
		args["CARGO_REGISTRY"] = true
	}
	return args
}

func copyMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func safeArtifactName(value string) string {
	value = dockerToken(value)
	if value == "" {
		return "service"
	}
	return value
}

func remoteBuildContext(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.Contains(lower, "://") || strings.HasPrefix(lower, "git@")
}

func splitDockerfileLines(content string) []string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}
