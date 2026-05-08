package pipeline

import (
	"strings"
	"unicode/utf8"
)

func truncateString(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return truncateStringPrefix(value, limit) + "\n[truncated]\n"
}

func truncateStringPrefix(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
