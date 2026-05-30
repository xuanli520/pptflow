package pipeline

import (
	"os"
	"strings"
)

func dockerCommandEnv() []string {
	return filteredRuntimeEnv(os.Environ(), nil, true)
}

func runtimeCommandEnv(extra []string) []string {
	return filteredRuntimeEnv(os.Environ(), extra, false)
}

func dockerRuntimeCommandEnv(extra []string) []string {
	return filteredRuntimeEnv(os.Environ(), extra, true)
}

func hostRuntimeToolEnv() []string {
	keys := []string{
		"HOME",
		"USERPROFILE",
		"XDG_CACHE_HOME",
		"GOCACHE",
		"GOMODCACHE",
		"GOPATH",
		"NPM_CONFIG_CACHE",
		"PLAYWRIGHT_BROWSERS_PATH",
		"CHROME_BIN",
		"CI",
	}
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		value := strings.TrimSpace(os.Getenv(key))
		if value == "" || runtimeEnvSensitive(key) {
			continue
		}
		env = append(env, key+"="+value)
	}
	return env
}

func filteredRuntimeEnv(environ, extra []string, docker bool) []string {
	values := map[string]string{}
	var order []string
	add := func(item string, trusted bool) {
		key, value, ok := strings.Cut(item, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return
		}
		if !trusted && (!runtimeEnvAllowed(key, docker) || runtimeEnvSensitive(key)) {
			return
		}
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}
	for _, item := range environ {
		add(item, false)
	}
	for _, item := range extra {
		add(item, true)
	}
	result := make([]string, 0, len(order))
	for _, key := range order {
		result = append(result, key+"="+values[key])
	}
	return result
}

func runtimeEnvFileValues(paths []string) ([]string, []string) {
	var values []string
	var warnings []string
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			warnings = append(warnings, "runtime env file skipped: "+err.Error())
			continue
		}
		for _, line := range strings.Split(string(content), "\n") {
			key, value, ok := parseRuntimeEnvFileLine(line)
			if !ok || key == "COMPOSE_FILE" || key == "COMPOSE_PROJECT_NAME" {
				continue
			}
			values = append(values, key+"="+value)
		}
	}
	return values, warnings
}

func parseRuntimeEnvFileLine(line string) (string, string, bool) {
	line = strings.TrimSpace(strings.TrimRight(line, "\r"))
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	key, value, ok := strings.Cut(line, "=")
	key = strings.TrimSpace(key)
	if !ok || !validRuntimeEnvName(key) {
		return "", "", false
	}
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		quote := value[0]
		if (quote == '\'' || quote == '"') && value[len(value)-1] == quote {
			value = value[1 : len(value)-1]
		}
	}
	return key, value, true
}

func validRuntimeEnvName(key string) bool {
	if key == "" {
		return false
	}
	for index, char := range key {
		if char == '_' || char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || index > 0 && char >= '0' && char <= '9' {
			continue
		}
		return false
	}
	return true
}

func runtimeEnvAllowed(key string, docker bool) bool {
	upper := strings.ToUpper(strings.TrimSpace(key))
	switch upper {
	case "PATH", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "LC_CTYPE":
		return true
	}
	if !docker {
		return false
	}
	switch upper {
	case "HOME", "USERPROFILE", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY":
		return true
	default:
		return false
	}
}

func runtimeEnvSensitive(key string) bool {
	upper := strings.ToUpper(strings.TrimSpace(key))
	if upper == "" {
		return true
	}
	sensitiveTokens := []string{
		"TOKEN",
		"SECRET",
		"PASSWORD",
		"PASSWD",
		"PRIVATE_KEY",
		"ACCESS_KEY",
		"API_KEY",
		"CREDENTIAL",
		"SESSION",
		"COOKIE",
	}
	for _, token := range sensitiveTokens {
		if strings.Contains(upper, token) {
			return true
		}
	}
	for _, prefix := range []string{"AWS_", "AZURE_", "GOOGLE_", "GCP_", "OPENAI_", "ANTHROPIC_", "CODEX_"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}
