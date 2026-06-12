package pipeline

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/frontende2e"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

type BrowserURLCandidate = frontende2e.BrowserURLCandidate
type BrowserActionRisk = frontende2e.BrowserActionRisk

const (
	BrowserRiskReadOnly      = frontende2e.BrowserRiskReadOnly
	BrowserRiskNavigation    = frontende2e.BrowserRiskNavigation
	BrowserRiskLocalStateful = frontende2e.BrowserRiskLocalStateful
	BrowserRiskDestructive   = frontende2e.BrowserRiskDestructive
)

type BrowserAction = frontende2e.BrowserAction
type BlockedBrowserAction = frontende2e.BlockedBrowserAction
type FrontendE2ESummary = frontende2e.FrontendE2ESummary
type FrontendE2EFinding = frontende2e.FrontendE2EFinding

const frontendE2ESchemaVersion = frontende2e.SchemaVersion

type browserActionValidation = frontende2e.BrowserActionValidation

func parseBrowserAction(raw string, candidates []BrowserURLCandidate) browserActionValidation {
	return frontende2e.ParseBrowserAction(raw, candidates)
}

func validateBrowserAction(action BrowserAction, candidates []BrowserURLCandidate, raw string) browserActionValidation {
	return frontende2e.ValidateBrowserAction(action, candidates, raw)
}

func browserActionForWrapper(action BrowserAction, candidates []BrowserURLCandidate) (browserpkg.Action, error) {
	return frontende2e.BrowserActionForWrapper(action, candidates)
}

func browserActionTextEndsSession(value string) bool {
	return frontende2e.BrowserActionTextEndsSession(value)
}

func parseFrontendE2ESummary(raw json.RawMessage) (FrontendE2ESummary, error) {
	return frontende2e.ParseSummary(raw)
}

func validateFrontendE2ESummary(summary FrontendE2ESummary) error {
	return frontende2e.ValidateSummary(summary)
}

func frontendE2EFindings(summary FrontendE2ESummary, sourcePath string) []model.Finding {
	return frontende2e.Findings(summary, sourcePath)
}

func frontendE2ESchemaFailureFinding(sourcePath string, err error) model.Finding {
	return frontende2e.SchemaFailureFinding(sourcePath, err)
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
	return frontende2e.AllowlistOrigins(candidates)
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
