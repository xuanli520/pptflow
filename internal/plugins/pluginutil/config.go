package pluginutil

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/workflow"
)

func RequiredString(spec workflow.NodeSpec, key string) error {
	if StringValue(spec.Config[key]) == "" {
		return fmt.Errorf("%s node %s missing %s", spec.Kind, spec.ID, key)
	}
	return nil
}

func String(req workflow.NodeRequest, key string) string {
	if value, ok := req.Input[key]; ok {
		if text := StringValue(value); text != "" {
			return text
		}
	}
	return StringValue(req.Spec.Config[key])
}

func StringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func Bool(req workflow.NodeRequest, key string) bool {
	value, ok := req.Input[key]
	if !ok {
		value = req.Spec.Config[key]
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		parsed, _ := strconv.ParseBool(strings.TrimSpace(typed))
		return parsed
	default:
		return false
	}
}

func Int(req workflow.NodeRequest, key string) int {
	value, ok := req.Input[key]
	if !ok {
		value = req.Spec.Config[key]
	}
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		parsed, _ := strconv.Atoi(strings.TrimSpace(typed))
		return parsed
	default:
		return 0
	}
}

func Float(req workflow.NodeRequest, key string) float64 {
	value, ok := req.Input[key]
	if !ok {
		value = req.Spec.Config[key]
	}
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case string:
		parsed, _ := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed
	default:
		return 0
	}
}

func Strings(req workflow.NodeRequest, key string) []string {
	value, ok := req.Input[key]
	if !ok {
		value = req.Spec.Config[key]
	}
	switch typed := value.(type) {
	case []string:
		return compactStrings(typed)
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := StringValue(item); text != "" {
				result = append(result, text)
			}
		}
		return result
	case string:
		return compactStrings(strings.Split(typed, ","))
	default:
		return nil
	}
}

func ArtifactName(req workflow.NodeRequest, fallback string) string {
	if name := String(req, "artifact_name"); name != "" {
		return name
	}
	return fallback
}

func compactStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}
