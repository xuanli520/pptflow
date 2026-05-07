package displaytime

import (
	"fmt"
	"strings"
	"time"
)

const Timezone = "Asia/Shanghai"

func LoadLocation() *time.Location {
	location, err := time.LoadLocation(Timezone)
	if err == nil {
		return location
	}
	return time.FixedZone("CST", 8*60*60)
}

func FormatMinute(value string) string {
	parsed, ok := parseRFC3339(value)
	if !ok {
		return widthSafeFallback(value, len("2006-01-02 15:04"))
	}
	return parsed.In(LoadLocation()).Format("2006-01-02 15:04")
}

func FormatSecond(value string) string {
	parsed, ok := parseRFC3339(value)
	if !ok {
		return strings.TrimSpace(value)
	}
	return parsed.In(LoadLocation()).Format("2006-01-02 15:04:05 MST")
}

func RunID(start time.Time) string {
	local := start.In(LoadLocation())
	return fmt.Sprintf("run-%s-%06d", local.Format("20060102-150405"), start.Nanosecond()/1000)
}

func LocalRFC3339(value string) string {
	parsed, ok := parseRFC3339(value)
	if !ok {
		return strings.TrimSpace(value)
	}
	return parsed.In(LoadLocation()).Format(time.RFC3339)
}

func parseRFC3339(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, value)
	return parsed, err == nil
}

func widthSafeFallback(value string, width int) string {
	value = strings.TrimSpace(value)
	if len(value) <= width {
		return value
	}
	return value[:width]
}
