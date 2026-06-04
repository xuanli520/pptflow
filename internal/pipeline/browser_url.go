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
			if probe.URL != "" {
				source = "probe"
			}
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
	for _, probe := range probes {
		if probe.Service == service {
			return probe
		}
	}
	return probeResult{}
}
