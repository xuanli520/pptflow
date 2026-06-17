package pipeline

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

type runTestsComposeUsage struct {
	Uses            bool
	StartsStack     bool
	ExplicitProject bool
	ProjectName     string
	Files           []string
	ProjectDir      string
	EnvFiles        []string
}

type runTestsRuntimeUsage struct {
	UsesDocker             bool
	StartsDockerRuntime    bool
	ReferencesRuntimePorts bool
	RuntimeEndpointHints   []string
	Compose                runTestsComposeUsage
}

func runTestsUsesDockerCompose(repoPath string) bool {
	return inspectRunTestsCompose(repoPath).Uses
}

func runTestsStartsDockerComposeStack(repoPath string) bool {
	return inspectRunTestsCompose(repoPath).StartsStack
}

func inspectRunTestsCompose(repoPath string) runTestsComposeUsage {
	return inspectRunTestsRuntime(repoPath).Compose
}

func inspectRunTestsRuntime(repoPath string) runTestsRuntimeUsage {
	content, err := os.ReadFile(filepath.Join(repoPath, "run_tests.sh"))
	if err != nil {
		return runTestsRuntimeUsage{}
	}
	dockerVars := map[string]bool{}
	composeVars := map[string]bool{}
	var usage runTestsRuntimeUsage
	endpointHints := map[string]bool{}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for _, hint := range runTestsRuntimeEndpointHints(line) {
			endpointHints[hint] = true
		}
		words := shellWords(line)
		for _, word := range words {
			key, value, ok := shellAssignment(word)
			if !ok {
				continue
			}
			if dockerCommandValue(value) {
				dockerVars[key] = true
			}
			if composeCommandValue(value) {
				composeVars[key] = true
			}
		}
		dockerCommand := classifyDockerCommand(words, dockerVars, composeVars)
		if dockerCommand.Uses {
			usage.UsesDocker = true
			usage.StartsDockerRuntime = usage.StartsDockerRuntime || dockerCommand.StartsRuntime
		}
		composeCommand := classifyComposeCommand(words, composeVars)
		if composeCommand.Uses {
			usage.Compose.Uses = true
			usage.Compose.StartsStack = usage.Compose.StartsStack || composeCommand.StartsStack
			usage.Compose.ExplicitProject = usage.Compose.ExplicitProject || composeCommand.StartsStack && composeCommand.ExplicitProject
			if composeCommand.StartsStack {
				usage.Compose.ProjectName = firstNonEmpty(usage.Compose.ProjectName, composeCommand.ProjectName)
				usage.Compose.ProjectDir = firstNonEmpty(usage.Compose.ProjectDir, composeCommand.ProjectDir)
				usage.Compose.Files = append(usage.Compose.Files, composeCommand.Files...)
				usage.Compose.EnvFiles = append(usage.Compose.EnvFiles, composeCommand.EnvFiles...)
			}
		}
	}
	if len(endpointHints) > 0 {
		usage.ReferencesRuntimePorts = true
		usage.RuntimeEndpointHints = sortedStringKeys(endpointHints)
	}
	return usage
}

type runTestsComposeCommand struct {
	Uses            bool
	StartsStack     bool
	ExplicitProject bool
	ProjectName     string
	Files           []string
	ProjectDir      string
	EnvFiles        []string
}

func classifyComposeCommand(words []string, composeVars map[string]bool) runTestsComposeCommand {
	var result runTestsComposeCommand
	for _, index := range shellCommandStartIndexes(words) {
		command := classifyComposeCommandAt(words, index, composeVars)
		result.Uses = result.Uses || command.Uses
		result.StartsStack = result.StartsStack || command.StartsStack
		result.ExplicitProject = result.ExplicitProject || command.StartsStack && command.ExplicitProject
		if command.StartsStack {
			result.ProjectName = firstNonEmpty(result.ProjectName, command.ProjectName)
			result.ProjectDir = firstNonEmpty(result.ProjectDir, command.ProjectDir)
			result.Files = append(result.Files, command.Files...)
			result.EnvFiles = append(result.EnvFiles, command.EnvFiles...)
		}
	}
	return result
}

func classifyComposeCommandAt(words []string, index int, composeVars map[string]bool) runTestsComposeCommand {
	if index < 0 || index >= len(words) {
		return runTestsComposeCommand{}
	}
	if _, _, ok := shellAssignment(words[index]); ok {
		return runTestsComposeCommand{}
	}
	envProject := shellAssignmentsSetProject(words[:index])
	for index < len(words) && shellCommandWrapper(words[index]) {
		index++
	}
	if index >= len(words) {
		return runTestsComposeCommand{}
	}
	switch {
	case composeVars[shellVariableName(words[index])]:
		return classifyComposeSubcommand(words, index+1, envProject)
	case dockerComposeBinary(words[index]):
		return classifyComposeSubcommand(words, index+1, envProject)
	case dockerBinary(words[index]):
		composeIndex := dockerComposePluginIndex(words, index+1)
		if composeIndex < 0 {
			return runTestsComposeCommand{}
		}
		return classifyComposeSubcommand(words, composeIndex+1, envProject)
	default:
		return runTestsComposeCommand{}
	}
}

type runTestsDockerCommand struct {
	Uses          bool
	StartsRuntime bool
}

func classifyDockerCommand(words []string, dockerVars map[string]bool, composeVars map[string]bool) runTestsDockerCommand {
	var result runTestsDockerCommand
	for _, index := range shellCommandStartIndexes(words) {
		command := classifyDockerCommandAt(words, index, dockerVars, composeVars)
		result.Uses = result.Uses || command.Uses
		result.StartsRuntime = result.StartsRuntime || command.StartsRuntime
	}
	return result
}

func shellCommandStartIndexes(words []string) []int {
	seen := map[int]bool{}
	var indexes []int
	add := func(index int) {
		if index < 0 || index >= len(words) || seen[index] {
			return
		}
		seen[index] = true
		indexes = append(indexes, index)
	}
	for index := range words {
		if index > 0 && !shellCommandSeparator(words[index-1]) {
			continue
		}
		if commandIndex := shellCommandIndex(words[index:]); commandIndex >= 0 {
			add(index + commandIndex)
		}
	}
	return indexes
}

func classifyDockerCommandAt(words []string, index int, dockerVars map[string]bool, composeVars map[string]bool) runTestsDockerCommand {
	if index < 0 || index >= len(words) {
		return runTestsDockerCommand{}
	}
	if _, _, ok := shellAssignment(words[index]); ok {
		return runTestsDockerCommand{}
	}
	for index < len(words) && shellCommandWrapper(words[index]) {
		index++
	}
	if index >= len(words) {
		return runTestsDockerCommand{}
	}
	composeCommand := classifyComposeCommandAt(words, index, composeVars)
	if composeCommand.Uses {
		return runTestsDockerCommand{Uses: true, StartsRuntime: composeCommand.StartsStack}
	}
	if !dockerVars[shellVariableName(words[index])] && !dockerBinary(words[index]) {
		return runTestsDockerCommand{}
	}
	subcommand := dockerSubcommandIndex(words, index+1)
	if subcommand < 0 {
		return runTestsDockerCommand{Uses: true}
	}
	return runTestsDockerCommand{Uses: true, StartsRuntime: dockerStartSubcommand(words[subcommand])}
}

func shellCommandIndex(words []string) int {
	for index, word := range words {
		if _, _, ok := shellAssignment(word); ok {
			continue
		}
		if word == "env" {
			next := index + 1
			for next < len(words) {
				if _, _, ok := shellAssignment(words[next]); ok {
					next++
					continue
				}
				if shellOptionTakesValue(words[next]) {
					next += 2
					continue
				}
				if strings.HasPrefix(words[next], "-") {
					next++
					continue
				}
				break
			}
			if next < len(words) {
				return next
			}
			return -1
		}
		return index
	}
	return -1
}

func dockerComposePluginIndex(words []string, index int) int {
	for index < len(words) {
		word := words[index]
		if word == "compose" {
			return index
		}
		if !strings.HasPrefix(word, "-") {
			return -1
		}
		index += 1 + shellOptionValueCount(word)
	}
	return -1
}

func dockerSubcommandIndex(words []string, index int) int {
	for index < len(words) {
		word := words[index]
		if _, _, ok := shellAssignment(word); ok {
			index++
			continue
		}
		if shellCommandSeparator(word) {
			return -1
		}
		if !strings.HasPrefix(word, "-") {
			return index
		}
		index += 1 + shellOptionValueCount(word)
	}
	return -1
}

func classifyComposeSubcommand(words []string, index int, explicitProject bool) runTestsComposeCommand {
	command := runTestsComposeCommand{Uses: true, ExplicitProject: explicitProject}
	for index < len(words) {
		word := words[index]
		if value, ok := shellOptionValue(words, index, "-p", "--project-name"); ok {
			command.ExplicitProject = true
			command.ProjectName = firstNonEmpty(command.ProjectName, value)
		}
		if value, ok := shellOptionValue(words, index, "-f", "--file"); ok {
			command.Files = append(command.Files, value)
		}
		if value, ok := shellOptionValue(words, index, "", "--project-directory"); ok {
			command.ProjectDir = firstNonEmpty(command.ProjectDir, value)
		}
		if value, ok := shellOptionValue(words, index, "", "--env-file"); ok {
			command.EnvFiles = append(command.EnvFiles, value)
		}
		if !strings.HasPrefix(word, "-") {
			command.StartsStack = runTestsComposeStartSubcommand(word)
			return command
		}
		index += 1 + shellOptionValueCount(word)
	}
	return command
}

func shellOptionValue(words []string, index int, short, long string) (string, bool) {
	if index < 0 || index >= len(words) {
		return "", false
	}
	word := strings.TrimSpace(words[index])
	switch {
	case word == short || word == long:
		if index+1 < len(words) {
			return strings.TrimSpace(words[index+1]), true
		}
		return "", true
	case short != "" && strings.HasPrefix(word, short+"="):
		return strings.TrimSpace(strings.TrimPrefix(word, short+"=")), true
	case short != "" && strings.HasPrefix(word, short) && !strings.HasPrefix(word, "--") && len(word) > len(short):
		return strings.TrimSpace(strings.TrimPrefix(word, short)), true
	case long != "" && strings.HasPrefix(word, long+"="):
		return strings.TrimSpace(strings.TrimPrefix(word, long+"=")), true
	default:
		return "", false
	}
}

func shellAssignmentsSetProject(words []string) bool {
	for _, word := range words {
		key, _, ok := shellAssignment(word)
		if ok && key == "COMPOSE_PROJECT_NAME" {
			return true
		}
	}
	return false
}

func shellOptionTakesValue(word string) bool {
	return shellOptionValueCount(word) > 0
}

func shellOptionValueCount(word string) int {
	if strings.Contains(word, "=") {
		return 0
	}
	switch word {
	case "-f", "--file", "-p", "--project-name", "--project-directory", "--env-file", "--profile", "--context", "-c", "-H", "--host", "--config":
		return 1
	default:
		return 0
	}
}

func runTestsComposeStartSubcommand(word string) bool {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "up", "run", "create", "start":
		return true
	default:
		return false
	}
}

func dockerStartSubcommand(word string) bool {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "run", "create", "start":
		return true
	default:
		return false
	}
}

func dockerCommandValue(value string) bool {
	words := shellWords(value)
	if len(words) == 0 {
		return false
	}
	command := classifyDockerCommand(words, nil, nil)
	return command.Uses
}

func composeCommandValue(value string) bool {
	words := shellWords(value)
	if len(words) == 0 {
		return false
	}
	command := classifyComposeCommand(words, nil)
	return command.Uses
}

func dockerBinary(word string) bool {
	base := commandBase(word)
	return base == "docker" || base == "docker.exe"
}

func dockerComposeBinary(word string) bool {
	base := commandBase(word)
	return base == "docker-compose" || base == "docker-compose.exe"
}

func commandBase(word string) string {
	word = strings.TrimSpace(word)
	word = strings.Trim(word, `"'()`)
	word = strings.ReplaceAll(word, `\`, `/`)
	if slash := strings.LastIndex(word, "/"); slash >= 0 {
		word = word[slash+1:]
	}
	return strings.ToLower(word)
}

func shellCommandWrapper(word string) bool {
	switch strings.Trim(word, `"'()`) {
	case "!", "command", "exec", "if", "elif", "then", "do", "sudo", "time", "until", "while":
		return true
	default:
		return false
	}
}

func shellCommandSeparator(word string) bool {
	switch strings.TrimSpace(word) {
	case ";", "&&", "||", "|":
		return true
	default:
		return false
	}
}

var shellAssignmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func shellAssignment(word string) (string, string, bool) {
	key, value, ok := strings.Cut(word, "=")
	if !ok || !shellAssignmentNamePattern.MatchString(key) {
		return "", "", false
	}
	return key, value, true
}

func shellVariableName(word string) string {
	word = strings.TrimSpace(word)
	word = strings.Trim(word, `"'()`)
	if strings.HasPrefix(word, "${") && strings.HasSuffix(word, "}") {
		return strings.TrimSuffix(strings.TrimPrefix(word, "${"), "}")
	}
	if strings.HasPrefix(word, "$") {
		return strings.TrimPrefix(word, "$")
	}
	return ""
}

var runTestsURLPattern = regexp.MustCompile(`(?i)https?://([A-Za-z0-9_.-]+):([0-9]{2,5})(/[A-Za-z0-9_./?&=%:+-]*)?`)

func runTestsRuntimeEndpointHints(line string) []string {
	matches := runTestsURLPattern.FindAllStringSubmatch(line, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var hints []string
	for _, match := range matches {
		host := strings.ToLower(strings.TrimSpace(match[1]))
		port := strings.TrimSpace(match[2])
		if !runTestsRuntimeEndpointHost(host) {
			continue
		}
		hint := host + ":" + port
		if seen[hint] {
			continue
		}
		seen[hint] = true
		hints = append(hints, hint)
	}
	return hints
}

func runTestsRuntimeEndpointHost(host string) bool {
	switch host {
	case "", "0.0.0.0", "127.0.0.1", "localhost":
		return true
	default:
		return !strings.Contains(host, ".")
	}
}

func sortedStringKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func shellWords(line string) []string {
	var words []string
	var builder strings.Builder
	var quote rune
	escaped := false
	started := false
	flush := func() {
		if !started {
			return
		}
		words = append(words, builder.String())
		builder.Reset()
		started = false
	}
	for _, char := range line {
		if escaped {
			builder.WriteRune(char)
			started = true
			escaped = false
			continue
		}
		if quote != '\'' && char == '\\' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			} else {
				builder.WriteRune(char)
				started = true
			}
			continue
		}
		switch {
		case char == '\'' || char == '"':
			quote = char
			started = true
		case char == '#':
			if !started {
				return words
			}
			builder.WriteRune(char)
			started = true
		case char == ';':
			flush()
			words = append(words, ";")
		case unicode.IsSpace(char):
			flush()
		default:
			builder.WriteRune(char)
			started = true
		}
	}
	if escaped {
		builder.WriteRune('\\')
	}
	flush()
	return words
}
