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
