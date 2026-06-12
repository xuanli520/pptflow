package frontende2e

import (
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

func AllowlistOrigins(candidates []BrowserURLCandidate) []string {
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
