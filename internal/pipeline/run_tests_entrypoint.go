package pipeline

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

type runTestsComposeUsage struct {
	Uses            bool
	StartsStack     bool
	ExplicitProject bool
}

func runTestsUsesDockerCompose(repoPath string) bool {
	return inspectRunTestsCompose(repoPath).Uses
}

func runTestsStartsDockerComposeStack(repoPath string) bool {
	return inspectRunTestsCompose(repoPath).StartsStack
}

func inspectRunTestsCompose(repoPath string) runTestsComposeUsage {
	content, err := os.ReadFile(filepath.Join(repoPath, "run_tests.sh"))
	if err != nil {
		return runTestsComposeUsage{}
	}
	composeVars := map[string]bool{}
	var usage runTestsComposeUsage
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		words := shellWords(line)
		for _, word := range words {
			key, value, ok := shellAssignment(word)
			if ok && composeCommandValue(value) {
				composeVars[key] = true
			}
		}
		command := classifyComposeCommand(words, composeVars)
		if !command.Uses {
			continue
		}
		usage.Uses = true
		usage.StartsStack = usage.StartsStack || command.StartsStack
		usage.ExplicitProject = usage.ExplicitProject || command.StartsStack && command.ExplicitProject
	}
	return usage
}

type runTestsComposeCommand struct {
	Uses            bool
	StartsStack     bool
	ExplicitProject bool
}

func classifyComposeCommand(words []string, composeVars map[string]bool) runTestsComposeCommand {
	index := shellCommandIndex(words)
	if index < 0 {
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

func classifyComposeSubcommand(words []string, index int, explicitProject bool) runTestsComposeCommand {
	command := runTestsComposeCommand{Uses: true, ExplicitProject: explicitProject}
	for index < len(words) {
		word := words[index]
		if word == "-p" || (strings.HasPrefix(word, "-p") && !strings.HasPrefix(word, "--")) || word == "--project-name" || strings.HasPrefix(word, "--project-name=") {
			command.ExplicitProject = true
		}
		if !strings.HasPrefix(word, "-") {
			command.StartsStack = runTestsComposeStartSubcommand(word)
			return command
		}
		index += 1 + shellOptionValueCount(word)
	}
	return command
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
	word = strings.Trim(word, `"'`)
	word = strings.ReplaceAll(word, `\`, `/`)
	if slash := strings.LastIndex(word, "/"); slash >= 0 {
		word = word[slash+1:]
	}
	return strings.ToLower(word)
}

func shellCommandWrapper(word string) bool {
	switch word {
	case "!", "command", "exec", "if", "time", "until", "while":
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
	if strings.HasPrefix(word, "${") && strings.HasSuffix(word, "}") {
		return strings.TrimSuffix(strings.TrimPrefix(word, "${"), "}")
	}
	if strings.HasPrefix(word, "$") {
		return strings.TrimPrefix(word, "$")
	}
	return ""
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
