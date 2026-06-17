package docker

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type RuntimeNetworkIPAMSummary struct {
	Generated   bool                        `json:"generated"`
	ComposeFile string                      `json:"compose_file,omitempty"`
	Networks    []RuntimeNetworkIPAMNetwork `json:"networks,omitempty"`
	Warnings    []string                    `json:"warnings,omitempty"`
}

type RuntimeNetworkIPAMNetwork struct {
	Network string `json:"network"`
	Subnet  string `json:"subnet"`
}

type networkIPAMPreparation struct {
	Summary RuntimeNetworkIPAMSummary
}

func prepareRuntimeNetworkIPAMOverride(effectiveConfig, artifactRoot, projectName string) networkIPAMPreparation {
	path := filepath.Join(artifactRoot, "docker_runtime", "compose.networks.yml")
	return networkIPAMPreparation{Summary: PrepareRuntimeNetworkIPAMOverrideFile(effectiveConfig, path, projectName)}
}

func PrepareRuntimeNetworkIPAMOverrideFile(effectiveConfig, path, projectName string) RuntimeNetworkIPAMSummary {
	summary := RuntimeNetworkIPAMSummary{}
	if strings.TrimSpace(path) == "" {
		summary.Warnings = append(summary.Warnings, "runtime network ipam override skipped: output path is empty")
		return summary
	}
	networks, parsed, warnings := internalComposeNetworks(effectiveConfig)
	summary.Warnings = append(summary.Warnings, warnings...)
	if !parsed {
		return summary
	}
	if len(networks) == 0 {
		networks = []string{"default"}
	}
	usedSlots := map[int]bool{}
	entries := make([]RuntimeNetworkIPAMNetwork, 0, len(networks))
	for _, network := range networks {
		slot := deterministicNetworkSubnetSlot(projectName, network, usedSlots)
		entries = append(entries, RuntimeNetworkIPAMNetwork{
			Network: network,
			Subnet:  fmt.Sprintf("100.%d.%d.0/24", 96+slot/256, slot%256),
		})
	}
	content, err := marshalRuntimeNetworkIPAMOverride(entries)
	if err != nil {
		summary.Warnings = append(summary.Warnings, "runtime network ipam override skipped: marshal compose: "+err.Error())
		return summary
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		summary.Warnings = append(summary.Warnings, "runtime network ipam override skipped: create artifact dir: "+err.Error())
		return summary
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		summary.Warnings = append(summary.Warnings, "runtime network ipam override skipped: write compose override: "+err.Error())
		return summary
	}
	summary.Generated = true
	summary.ComposeFile = path
	summary.Networks = entries
	return summary
}

func internalComposeNetworks(effectiveConfig string) ([]string, bool, []string) {
	var payload map[string]any
	if err := yaml.Unmarshal([]byte(effectiveConfig), &payload); err != nil {
		return nil, false, []string{"runtime network ipam override skipped: parse compose config: " + err.Error()}
	}
	rawNetworks, ok := payload["networks"].(map[string]any)
	if !ok || len(rawNetworks) == 0 {
		return nil, true, nil
	}
	networks := make([]string, 0, len(rawNetworks))
	for name, raw := range rawNetworks {
		name = strings.TrimSpace(name)
		if name == "" || composeNetworkIsExternal(raw) {
			continue
		}
		networks = append(networks, name)
	}
	sort.Strings(networks)
	return networks, true, nil
}

func composeNetworkIsExternal(raw any) bool {
	switch typed := raw.(type) {
	case map[string]any:
		return truthyComposeValue(typed["external"])
	case map[any]any:
		return truthyComposeValue(typed["external"])
	default:
		return false
	}
}

func truthyComposeValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "yes", "y", "1":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func deterministicNetworkSubnetSlot(projectName, network string, used map[int]bool) int {
	sum := sha256.Sum256([]byte(projectName + "\x00" + network))
	slot := (int(sum[0])<<8 | int(sum[1])) % 2048
	for used[slot] {
		slot = (slot + 1) % 2048
	}
	used[slot] = true
	return slot
}

func marshalRuntimeNetworkIPAMOverride(networks []RuntimeNetworkIPAMNetwork) ([]byte, error) {
	root := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	networksNode := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	root.Content = append(root.Content, yamlScalar("networks"), &networksNode)
	for _, item := range networks {
		networkNode := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		ipamNode := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		configNode := yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		subnetNode := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		subnetNode.Content = append(subnetNode.Content, yamlScalar("subnet"), yamlScalar(item.Subnet))
		configNode.Content = append(configNode.Content, &subnetNode)
		ipamNode.Content = append(ipamNode.Content, yamlScalar("config"), &configNode)
		networkNode.Content = append(networkNode.Content, yamlScalar("ipam"), &ipamNode)
		networksNode.Content = append(networksNode.Content, yamlScalar(item.Network), &networkNode)
	}
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		_ = encoder.Close()
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
