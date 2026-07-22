package taskpolicy

import (
	"bytes"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

var standardAuthoringHarborTaskNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateStandardAuthoringTaskTOML verifies the complete task.toml contract
// emitted by Harbor Factory Standard Authoring. The contract deliberately
// uses only Harbor 0.18 TaskConfig fields. Dockerfile and test-script bytes
// belong to environment/Dockerfile and tests/test.sh respectively, so the
// task TOML must not contain factory-only fields that Harbor silently ignores.
func ValidateStandardAuthoringTaskTOML(content []byte) error {
	if len(bytes.TrimSpace(content)) == 0 || !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return fmt.Errorf("task.toml must be non-empty UTF-8 text")
	}
	var document map[string]any
	if err := toml.NewDecoder(bytes.NewReader(content)).Decode(&document); err != nil {
		return fmt.Errorf("task.toml must be valid TOML: %w", err)
	}
	metadata, ok := standardAuthoringTaskTOMLTable(document, "metadata")
	if !ok || !standardAuthoringTaskTOMLRequiredString(metadata, "code_lang") ||
		!standardAuthoringTaskTOMLRequiredString(metadata, "task_type") ||
		!standardAuthoringTaskTOMLRequiredString(metadata, "application") ||
		!standardAuthoringTaskTOMLRequiredBool(metadata, "is_0_to_1") {
		return fmt.Errorf("task.toml [metadata] must define code_lang, task_type, application, and is_0_to_1")
	}
	task, ok := standardAuthoringTaskTOMLTable(document, "task")
	if !ok || !standardAuthoringTaskTOMLRequiredHarborName(task, "name") ||
		!standardAuthoringTaskTOMLRequiredString(task, "description") {
		return fmt.Errorf("task.toml [task] must define a valid org/name name and a non-empty description")
	}
	if _, found := document["verification"]; found {
		return fmt.Errorf("task.toml must not declare [verification]: Harbor ignores it; verification belongs in tests/test.sh")
	}
	environment, ok := standardAuthoringTaskTOMLTable(document, "environment")
	if !ok || !standardAuthoringTaskTOMLRequiredPositiveNumber(environment, "build_timeout_sec") ||
		!standardAuthoringTaskTOMLRequiredNoNetworkMode(environment, "network_mode") ||
		!standardAuthoringTaskTOMLRequiredSourceWorkdir(environment, "workdir") {
		return fmt.Errorf("task.toml [environment] must define positive build_timeout_sec, network_mode = no-network, and workdir = /workspace/source")
	}
	if _, found := environment["dockerfile"]; found {
		return fmt.Errorf("task.toml [environment].dockerfile is not a Harbor TaskConfig field; use environment/Dockerfile")
	}
	verifier, ok := standardAuthoringTaskTOMLTable(document, "verifier")
	if !ok || !standardAuthoringTaskTOMLRequiredPositiveNumber(verifier, "timeout_sec") {
		return fmt.Errorf("task.toml [verifier] must define positive timeout_sec")
	}
	return nil
}

func standardAuthoringTaskTOMLTable(document map[string]any, name string) (map[string]any, bool) {
	value, found := document[name]
	table, ok := value.(map[string]any)
	return table, found && ok
}

func standardAuthoringTaskTOMLRequiredString(table map[string]any, name string) bool {
	value, found := table[name]
	text, ok := value.(string)
	return found && ok && standardAuthoringTaskTOMLText(text)
}

func standardAuthoringTaskTOMLRequiredBool(table map[string]any, name string) bool {
	value, found := table[name]
	_, ok := value.(bool)
	return found && ok
}

func standardAuthoringTaskTOMLRequiredPositiveNumber(table map[string]any, name string) bool {
	value, found := table[name]
	if !found {
		return false
	}
	var number float64
	switch value := value.(type) {
	case int64:
		number = float64(value)
	case float64:
		number = value
	default:
		return false
	}
	return number > 0 && !math.IsNaN(number) && !math.IsInf(number, 0)
}

func standardAuthoringTaskTOMLRequiredNoNetworkMode(table map[string]any, name string) bool {
	value, found := table[name]
	text, ok := value.(string)
	return found && ok && text == "no-network"
}

func standardAuthoringTaskTOMLRequiredSourceWorkdir(table map[string]any, name string) bool {
	value, found := table[name]
	text, ok := value.(string)
	return found && ok && text == "/workspace/source"
}

func standardAuthoringTaskTOMLRequiredHarborName(table map[string]any, name string) bool {
	value, found := table[name]
	text, ok := value.(string)
	return found && ok && standardAuthoringTaskTOMLText(text) && !strings.Contains(text, "..") &&
		standardAuthoringHarborTaskNamePattern.MatchString(text)
}

func standardAuthoringTaskTOMLText(value string) bool {
	return strings.TrimSpace(value) != "" && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}
