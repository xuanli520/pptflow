package pipeline

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var runTestsDefaultURLPattern = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)=["']?\$\{[A-Za-z_][A-Za-z0-9_]*:-https?://([A-Za-z0-9_.-]+):([0-9]{2,5})([^}"'\s]*)\}["']?`)

func runTestsHostURLDefaultEnv(scriptPath string, runtime RuntimeState) []string {
	content, err := os.ReadFile(scriptPath)
	if err != nil {
		return nil
	}
	runtime.Normalize()
	values := map[string]string{}
	for _, match := range runTestsDefaultURLPattern.FindAllStringSubmatch(string(content), -1) {
		key := strings.TrimSpace(match[1])
		host := strings.ToLower(strings.TrimSpace(match[2]))
		port, err := strconv.Atoi(strings.TrimSpace(match[3]))
		if key == "" || err != nil || port <= 0 {
			continue
		}
		baseURL := runtimeHostURLForScriptDefault(runtime, host, port)
		if baseURL == "" {
			continue
		}
		values[key] = baseURL + match[4]
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

func runtimeHostURLForScriptDefault(runtime RuntimeState, host string, containerPort int) string {
	if host == "" {
		return ""
	}
	names := []string{host}
	if host == "localhost" || host == "127.0.0.1" || host == "0.0.0.0" {
		names = append([]string{}, runtime.Services...)
	}
	for _, service := range names {
		for _, mapping := range runtime.Mappings[service] {
			if mapping.Host <= 0 {
				continue
			}
			if mapping.Container != containerPort && mapping.Host != containerPort {
				continue
			}
			scheme := "http"
			if mapping.Container == 443 || mapping.Host == 443 {
				scheme = "https"
			}
			return fmt.Sprintf("%s://%s:%d", scheme, normalizeHost(mapping.URL), mapping.Host)
		}
	}
	return ""
}
