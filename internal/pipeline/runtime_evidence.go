package pipeline

import (
	"fmt"
	"sort"
	"strings"
)

type serviceURLEnv struct {
	Env     []string              `json:"env"`
	Keys    []string              `json:"keys"`
	Mapping map[string]serviceURL `json:"mapping"`
}

type serviceURL struct {
	EnvKey string `json:"env_key"`
	URL    string `json:"url"`
}

type stageCCommandEnv struct {
	Env     []string
	Keys    []string
	Values  map[string]string
	Service serviceURLEnv
}

func stageCEnvironment(evidence runtimeEvidence) stageCCommandEnv {
	service := serviceURLEnvironment(evidence)
	result := stageCCommandEnv{
		Env:     append([]string{}, service.Env...),
		Keys:    append([]string{}, service.Keys...),
		Values:  envValueMap(service.Env),
		Service: service,
	}
	result.add("COMPOSE_PROJECT_NAME", evidence.ComposeProject)
	result.add("COMPOSE_FILE", evidence.ComposeFile)
	return result
}

func (e *stageCCommandEnv) add(key, value string) {
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" || value == "" {
		return
	}
	e.Env = append(e.Env, key+"="+value)
	e.Keys = append(e.Keys, key)
	if e.Values == nil {
		e.Values = map[string]string{}
	}
	e.Values[key] = value
}

func envValueMap(env []string) map[string]string {
	values := map[string]string{}
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			continue
		}
		values[key] = value
	}
	return values
}

func serviceURLEnvironment(evidence runtimeEvidence) serviceURLEnv {
	names := make([]string, 0, len(evidence.Mappings))
	for service := range evidence.Mappings {
		names = append(names, service)
	}
	sort.Strings(names)
	used := map[string]int{}
	result := serviceURLEnv{Mapping: map[string]serviceURL{}}
	for _, service := range names {
		url := preferredServiceURL(service, evidence.Mappings[service], evidence.Probes)
		if url == "" {
			continue
		}
		base := sanitizeEnvKey(service) + "_URL"
		used[base]++
		key := base
		if used[base] > 1 {
			key = fmt.Sprintf("%s_%d_URL", strings.TrimSuffix(base, "_URL"), used[base])
		}
		result.Env = append(result.Env, key+"="+url)
		result.Keys = append(result.Keys, key)
		result.Mapping[service] = serviceURL{EnvKey: key, URL: url}
	}
	return result
}

func preferredServiceURL(service string, mappings []portMapping, probes []probeResult) string {
	for _, probe := range probes {
		if probe.Service == service && probe.OK && strings.HasPrefix(probe.URL, "http") {
			return normalizeServiceURL(probe.URL)
		}
	}
	for _, mapping := range mappings {
		if mapping.Host == 0 {
			continue
		}
		scheme := "http"
		if mapping.Container == 443 || mapping.Host == 443 {
			scheme = "https"
		}
		host := normalizeHost(mapping.URL)
		return fmt.Sprintf("%s://%s:%d", scheme, host, mapping.Host)
	}
	return ""
}

func normalizeServiceURL(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.Replace(raw, "://0.0.0.0", "://localhost", 1)
	raw = strings.Replace(raw, "://[::]", "://localhost", 1)
	raw = strings.Replace(raw, "://::", "://localhost", 1)
	raw = strings.Replace(raw, "://127.0.0.1", "://localhost", 1)
	return raw
}

func normalizeHost(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "http://")
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.Trim(raw, "[]")
	if raw == "" || raw == "0.0.0.0" || raw == "::" || raw == "127.0.0.1" {
		return "localhost"
	}
	if strings.Contains(raw, ":") {
		host, _, ok := strings.Cut(raw, ":")
		if ok {
			return normalizeHost(host)
		}
	}
	return raw
}

func sanitizeEnvKey(service string) string {
	var builder strings.Builder
	lastUnderscore := false
	for _, r := range service {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			builder.WriteByte('_')
			lastUnderscore = true
		}
	}
	key := strings.Trim(builder.String(), "_")
	if key == "" {
		key = "SERVICE"
	}
	return strings.ToUpper(key)
}
