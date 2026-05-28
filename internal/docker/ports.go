package docker

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type RuntimePortRewriteSummary struct {
	Generated   bool                        `json:"generated"`
	ComposeFile string                      `json:"compose_file,omitempty"`
	Services    []RuntimePortRewriteService `json:"services,omitempty"`
	Warnings    []string                    `json:"warnings,omitempty"`
}

type RuntimePortRewriteService struct {
	Service string   `json:"service"`
	Ports   []string `json:"ports"`
}

type portRewritePreparation struct {
	Summary RuntimePortRewriteSummary
}

func prepareRuntimePortRewrite(composeFile, artifactRoot string) portRewritePreparation {
	summary := RuntimePortRewriteSummary{}
	if strings.TrimSpace(composeFile) == "" {
		return portRewritePreparation{Summary: summary}
	}
	if strings.TrimSpace(artifactRoot) == "" {
		summary.Warnings = append(summary.Warnings, "runtime port rewrite skipped: artifact root is empty")
		return portRewritePreparation{Summary: summary}
	}
	content, err := os.ReadFile(composeFile)
	if err != nil {
		summary.Warnings = append(summary.Warnings, "runtime port rewrite skipped: "+err.Error())
		return portRewritePreparation{Summary: summary}
	}
	var payload map[string]any
	if err := yaml.Unmarshal(content, &payload); err != nil {
		summary.Warnings = append(summary.Warnings, "runtime port rewrite skipped: parse compose: "+err.Error())
		return portRewritePreparation{Summary: summary}
	}
	services, ok := payload["services"].(map[string]any)
	if !ok {
		return portRewritePreparation{Summary: summary}
	}
	override := map[string]any{"services": map[string]any{}}
	overrideServices := override["services"].(map[string]any)
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		service, _ := services[name].(map[string]any)
		if len(service) == 0 {
			continue
		}
		ports, exists := service["ports"]
		if !exists {
			continue
		}
		rewritten, changed, warnings := rewriteComposeServicePorts(ports)
		summary.Warnings = append(summary.Warnings, warnings...)
		if !changed {
			continue
		}
		overrideServices[name] = map[string]any{"ports": rewritten}
		summary.Services = append(summary.Services, RuntimePortRewriteService{
			Service: name,
			Ports:   stringPortEntries(rewritten),
		})
	}
	if len(summary.Services) == 0 {
		return portRewritePreparation{Summary: summary}
	}
	content, err = yaml.Marshal(override)
	if err != nil {
		summary.Services = nil
		summary.Warnings = append(summary.Warnings, "runtime port rewrite skipped: marshal compose: "+err.Error())
		return portRewritePreparation{Summary: summary}
	}
	dir := filepath.Join(artifactRoot, "docker_runtime")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		summary.Services = nil
		summary.Warnings = append(summary.Warnings, "runtime port rewrite skipped: create artifact dir: "+err.Error())
		return portRewritePreparation{Summary: summary}
	}
	path := filepath.Join(dir, "compose.ports.yml")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		summary.Services = nil
		summary.Warnings = append(summary.Warnings, "runtime port rewrite skipped: write compose override: "+err.Error())
		return portRewritePreparation{Summary: summary}
	}
	summary.Generated = true
	summary.ComposeFile = path
	return portRewritePreparation{Summary: summary}
}

type composePortRewriteEntry struct {
	target    string
	protocol  string
	published bool
	raw       any
	parsed    bool
}

func rewriteComposeServicePorts(raw any) ([]any, bool, []string) {
	entries, ok := raw.([]any)
	if !ok {
		return nil, false, []string{fmt.Sprintf("runtime port rewrite skipped unsupported ports value %T", raw)}
	}
	parsed := make([]composePortRewriteEntry, 0, len(entries))
	needsRewrite := false
	for _, entry := range entries {
		target, protocol, published, ok := parseComposePortEntry(entry)
		parsed = append(parsed, composePortRewriteEntry{
			target:    target,
			protocol:  protocol,
			published: published,
			raw:       entry,
			parsed:    ok,
		})
		if ok && published {
			needsRewrite = true
		}
	}
	if !needsRewrite {
		return nil, false, nil
	}
	rewritten := make([]any, 0, len(parsed))
	seen := map[string]bool{}
	for _, entry := range parsed {
		if !entry.parsed {
			rewritten = append(rewritten, entry.raw)
			continue
		}
		value := composePortTarget(entry.target, entry.protocol)
		if seen[value] {
			continue
		}
		seen[value] = true
		rewritten = append(rewritten, value)
	}
	return rewritten, true, nil
}

func parseComposePortEntry(raw any) (target, protocol string, published bool, ok bool) {
	switch value := raw.(type) {
	case string:
		return parseComposeShortPort(value)
	case int:
		if value <= 0 {
			return "", "", false, false
		}
		return strconv.Itoa(value), "", false, true
	case int64:
		if value <= 0 {
			return "", "", false, false
		}
		return strconv.FormatInt(value, 10), "", false, true
	case map[string]any:
		return parseComposeLongPort(value)
	default:
		return "", "", false, false
	}
}

func parseComposeLongPort(value map[string]any) (target, protocol string, published bool, ok bool) {
	target = scalarString(value["target"])
	if target == "" {
		return "", "", false, false
	}
	protocol = scalarString(value["protocol"])
	published = composePublishedPortIsFixed(value["published"])
	return target, protocol, published, true
}

func parseComposeShortPort(value string) (target, protocol string, published bool, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", false, false
	}
	if slash := strings.LastIndex(value, "/"); slash >= 0 {
		protocol = strings.TrimSpace(value[slash+1:])
		value = strings.TrimSpace(value[:slash])
	}
	if colon := strings.LastIndex(value, ":"); colon >= 0 {
		prefix := strings.TrimSpace(value[:colon])
		target = strings.TrimSpace(value[colon+1:])
		published = composeHostPortPrefixIsFixed(prefix)
	} else {
		target = strings.TrimSpace(value)
	}
	if target == "" {
		return "", "", false, false
	}
	return target, protocol, published, true
}

func composePublishedPortIsFixed(value any) bool {
	text := scalarString(value)
	return text != "" && text != "0"
}

func composeHostPortPrefixIsFixed(prefix string) bool {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" || strings.HasSuffix(prefix, ":") {
		return false
	}
	if colon := strings.LastIndex(prefix, ":"); colon >= 0 {
		prefix = strings.TrimSpace(prefix[colon+1:])
	}
	return prefix != "" && prefix != "0"
}

func composePortTarget(target, protocol string) string {
	target = strings.TrimSpace(target)
	protocol = strings.TrimSpace(protocol)
	if protocol == "" {
		return target
	}
	return target + "/" + protocol
}

func scalarString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case uint64:
		return strconv.FormatUint(typed, 10)
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strings.TrimSpace(strconv.FormatFloat(typed, 'f', -1, 64))
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func stringPortEntries(values []any) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text := scalarString(value); text != "" {
			result = append(result, text)
		}
	}
	return result
}
