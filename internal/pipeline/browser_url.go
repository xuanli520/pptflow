package pipeline

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

type BrowserURLCandidate struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	Origin        string `json:"origin"`
	Service       string `json:"service"`
	Source        string `json:"source"`
	ProbeOK       bool   `json:"probe_ok"`
	ProbeStatus   int    `json:"probe_status,omitempty"`
	ProbeError    string `json:"probe_error,omitempty"`
	ContainerPort int    `json:"container_port"`
	HostPort      int    `json:"host_port"`
	Protocol      string `json:"protocol,omitempty"`
}

func browserURLCandidates(runtime RuntimeState) []BrowserURLCandidate {
	runtime.Normalize()
	var services []string
	seen := map[string]bool{}
	for _, service := range runtime.Services {
		service = strings.TrimSpace(service)
		if service == "" || seen[service] {
			continue
		}
		seen[service] = true
		services = append(services, service)
	}
	var extras []string
	for service := range runtime.Mappings {
		if !seen[service] {
			extras = append(extras, service)
		}
	}
	sort.Strings(extras)
	services = append(services, extras...)

	var candidates []BrowserURLCandidate
	seenCandidates := map[string]bool{}
	for _, service := range services {
		for _, mapping := range runtime.Mappings[service] {
			if mapping.Host <= 0 {
				continue
			}
			probe := matchingProbe(service, mapping.Host, runtime.Probes)
			candidateURL := browserCandidateURL(mapping)
			parsed, err := url.Parse(candidateURL)
			if err != nil || parsed.Scheme == "" || parsed.Host == "" {
				continue
			}
			source := "mapping"
			if successfulHTTPProbe(probe) {
				source = "probe"
				candidateURL = browserProbeURL(probe, mapping)
				parsed, err = url.Parse(candidateURL)
				if err != nil || parsed.Scheme == "" || parsed.Host == "" {
					continue
				}
			}
			key := browserCandidateKey(service, mapping, candidateURL)
			if seenCandidates[key] {
				continue
			}
			seenCandidates[key] = true
			candidates = append(candidates, BrowserURLCandidate{
				ID:            fmt.Sprintf("url_%d", len(candidates)+1),
				URL:           candidateURL,
				Origin:        parsed.Scheme + "://" + parsed.Host,
				Service:       service,
				Source:        source,
				ProbeOK:       probe.OK,
				ProbeStatus:   probe.Status,
				ProbeError:    probe.Error,
				ContainerPort: mapping.Container,
				HostPort:      mapping.Host,
				Protocol:      mapping.Protocol,
			})
		}
	}
	return candidates
}

func browserCandidateKey(service string, mapping portMapping, candidateURL string) string {
	return strings.Join([]string{
		strings.TrimSpace(service),
		strings.TrimSpace(candidateURL),
		fmt.Sprint(mapping.Host),
		fmt.Sprint(mapping.Container),
		strings.TrimSpace(mapping.Protocol),
	}, "\x00")
}

func browserAllowlistOrigins(candidates []BrowserURLCandidate) []string {
	seen := map[string]bool{}
	var origins []string
	for _, candidate := range candidates {
		origin := strings.TrimSpace(candidate.Origin)
		if origin == "" || seen[origin] {
			continue
		}
		seen[origin] = true
		origins = append(origins, origin)
	}
	sort.Strings(origins)
	return origins
}

func browserCandidateURL(mapping portMapping) string {
	scheme := "http"
	if mapping.Container == 443 || mapping.Host == 443 {
		scheme = "https"
	}
	return fmt.Sprintf("%s://127.0.0.1:%d", scheme, mapping.Host)
}

func browserProbeURL(probe probeResult, mapping portMapping) string {
	scheme := "http"
	parsed, err := url.Parse(strings.TrimSpace(probe.URL))
	if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		scheme = parsed.Scheme
	}
	return fmt.Sprintf("%s://127.0.0.1:%d", scheme, mapping.Host)
}

func successfulHTTPProbe(probe probeResult) bool {
	return probe.OK && strings.HasPrefix(strings.ToLower(strings.TrimSpace(probe.URL)), "http")
}

func matchingProbe(service string, hostPort int, probes []probeResult) probeResult {
	for _, probe := range probes {
		if probe.Service != service {
			continue
		}
		parsed, err := url.Parse(strings.TrimSpace(probe.URL))
		if err != nil {
			continue
		}
		if parsed.Port() == fmt.Sprint(hostPort) {
			return probe
		}
	}
	return probeResult{}
}
