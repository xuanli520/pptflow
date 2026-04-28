package docker

import (
	"regexp"
	"strings"
)

func ComposeProjectName(prefix, taskID, runID string) string {
	value := strings.ToLower(prefix + "_" + taskID + "_" + runID)
	value = regexp.MustCompile(`[^a-z0-9_-]+`).ReplaceAllString(value, "_")
	if len(value) > 63 {
		return value[:63]
	}
	return value
}
