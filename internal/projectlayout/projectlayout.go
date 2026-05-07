package projectlayout

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

var (
	batchDirPattern = regexp.MustCompile(`^batch-[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	taskIDPattern   = regexp.MustCompile(`^TASK-[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

var originalSessionMarkers = []string{
	"original_sessions",
	filepath.Join("docs", "original-session"),
	filepath.Join("docs", "original_sessions"),
}

func IsBatchDir(name string) bool {
	return batchDirPattern.MatchString(strings.TrimSpace(name))
}

func IsTaskID(name string) bool {
	return taskIDPattern.MatchString(strings.TrimSpace(name))
}

func ExpectedProjectPath(root, batch, taskID string) string {
	return filepath.Clean(filepath.Join(root, batch, taskID, taskID))
}

func HasOriginalSessionMarker(projectPath string) (bool, string) {
	for _, marker := range originalSessionMarkers {
		path := filepath.Join(projectPath, marker)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return true, marker
		}
	}
	return false, ""
}

func OriginalSessionMarkers() []string {
	return append([]string{}, originalSessionMarkers...)
}

func MetadataTaskID(path string) string {
	metadataPath := path
	if filepath.Base(metadataPath) != "metadata.json" {
		metadataPath = filepath.Join(path, "metadata.json")
	}
	content, err := os.ReadFile(metadataPath)
	if err != nil {
		return ""
	}
	var data map[string]any
	if json.Unmarshal(content, &data) != nil {
		return ""
	}
	for _, key := range []string{"task_id", "taskId", "id"} {
		if value, ok := data[key].(string); ok {
			value = strings.TrimSpace(value)
			if value != "" {
				return value
			}
		}
	}
	return ""
}

func SafePathSegment(value, fallback string) string {
	cleaned, ok := safePathSegment(value)
	if ok {
		return cleaned
	}
	cleaned, ok = safePathSegment(fallback)
	if ok {
		return cleaned
	}
	return "unknown"
}

func safePathSegment(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." {
		return "", false
	}
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r == filepath.Separator || r == '/' || r == '\\' || unicode.IsControl(r):
			return "", false
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '_' || r == '-' || r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	name := strings.Trim(builder.String(), "._-")
	if name == "" || name == "." || name == ".." {
		return "", false
	}
	return name, true
}
