package docker

import (
	"encoding/json"
	"strings"
)

type PortMapping struct {
	Service   string `json:"service"`
	URL       string `json:"url,omitempty"`
	Host      int    `json:"host"`
	Container int    `json:"container"`
	Protocol  string `json:"protocol,omitempty"`
}

type ProbeResult struct {
	Service string `json:"service"`
	URL     string `json:"url"`
	OK      bool   `json:"ok"`
	Status  int    `json:"status,omitempty"`
	Error   string `json:"error,omitempty"`
}

type RuntimeMirrorState struct {
	BuildMirrorEnabled      bool   `json:"build_mirror_enabled,omitempty"`
	BuildMirrorMode         string `json:"build_mirror_mode,omitempty"`
	BuildMirrorFallbackUsed bool   `json:"build_mirror_fallback_used,omitempty"`
	BuildMirrorSummary      string `json:"build_mirror_summary,omitempty"`
}

type RuntimeState struct {
	ComposeProject string                   `json:"compose_project"`
	ComposeFile    string                   `json:"compose_file,omitempty"`
	ComposeFiles   []string                 `json:"compose_files,omitempty"`
	WorkDir        string                   `json:"work_dir"`
	Services       []string                 `json:"services"`
	Mappings       map[string][]PortMapping `json:"mappings"`
	Probes         []ProbeResult            `json:"probes"`
	Mirror         RuntimeMirrorState       `json:"mirror,omitempty"`
}

func (s RuntimeState) HasCleanupTarget() bool {
	return strings.TrimSpace(s.ComposeProject) != ""
}

func (s RuntimeState) HasServiceMappings() bool {
	for _, mappings := range s.Mappings {
		if len(mappings) > 0 {
			return true
		}
	}
	return false
}

func (s RuntimeState) Normalized() RuntimeState {
	s.Normalize()
	return s
}

func (s *RuntimeState) Normalize() {
	if s == nil {
		return
	}
	if len(s.ComposeFiles) == 0 && strings.TrimSpace(s.ComposeFile) != "" {
		s.ComposeFiles = []string{s.ComposeFile}
	}
	if strings.TrimSpace(s.ComposeFile) == "" && len(s.ComposeFiles) > 0 {
		s.ComposeFile = s.ComposeFiles[0]
	}
	if s.Mappings == nil {
		s.Mappings = map[string][]PortMapping{}
	}
}

func (s *RuntimeState) UnmarshalJSON(content []byte) error {
	type alias RuntimeState
	var value alias
	if err := json.Unmarshal(content, &value); err != nil {
		return err
	}
	*s = RuntimeState(value)
	s.Normalize()
	return nil
}
