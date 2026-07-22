package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/commandlog"
	"github.com/spf13/cobra"
)

type doctorReport struct {
	SchemaVersion string        `json:"schema_version"`
	GeneratedAt   time.Time     `json:"generated_at"`
	Tools         []toolCheck   `json:"tools"`
	Secrets       []secretCheck `json:"secrets,omitempty"`
	Passed        bool          `json:"passed"`
	Issues        []string      `json:"issues,omitempty"`
}

type toolCheck struct {
	Name       string      `json:"name"`
	Binary     string      `json:"binary"`
	Required   bool        `json:"required"`
	Found      bool        `json:"found"`
	Healthy    bool        `json:"healthy"`
	Path       string      `json:"path,omitempty"`
	Version    string      `json:"version,omitempty"`
	MinVersion string      `json:"min_version,omitempty"`
	Probes     []toolProbe `json:"probes,omitempty"`
	Error      string      `json:"error,omitempty"`
}

type toolProbe struct {
	Name     string   `json:"name"`
	Args     []string `json:"args,omitempty"`
	Required bool     `json:"required"`
	Passed   bool     `json:"passed"`
	Output   string   `json:"output,omitempty"`
	Error    string   `json:"error,omitempty"`
}

type secretCheck struct {
	Name     string `json:"name"`
	EnvVar   string `json:"env_var"`
	Required bool   `json:"required"`
	Present  bool   `json:"present"`
}

func newDoctorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check local dependencies for Harbor factory E2E runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			report := runDoctor()
			data, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(data))
			if !report.Passed {
				return fmt.Errorf("doctor checks failed")
			}
			return nil
		},
	}
	return cmd
}

func runDoctor() doctorReport {
	report := doctorReport{
		SchemaVersion: "harbor.doctor_report.v2",
		GeneratedAt:   time.Now().UTC(),
		Passed:        true,
	}
	for _, item := range doctorToolSpecs() {
		check := checkTool(item)
		report.Tools = append(report.Tools, check)
		if check.Required && !check.Found {
			report.Passed = false
			report.Issues = append(report.Issues, item.name+" CLI not found in PATH")
		}
		if check.Required && check.Found && !check.Healthy {
			report.Passed = false
			if item.name == "docker" && strings.Contains(check.Error, "docker daemon") {
				report.Issues = append(report.Issues, "docker daemon unavailable")
			} else {
				report.Issues = append(report.Issues, item.name+" CLI health check failed")
			}
		}
	}
	report.Secrets = append(report.Secrets, secretCheck{
		Name:     "gpt-image-2 API key",
		EnvVar:   "GPT_IMAGE_API_KEY",
		Required: false,
		Present:  strings.TrimSpace(os.Getenv("GPT_IMAGE_API_KEY")) != "",
	})
	return report
}

type toolSpec struct {
	name       string
	binary     string
	required   bool
	minVersion string
	probes     []probeSpec
}

type probeSpec struct {
	name       string
	args       []string
	required   bool
	version    bool
	requireAll []string
	goMin      string
	timeout    time.Duration
}

func doctorToolSpecs() []toolSpec {
	return []toolSpec{
		{name: "git", binary: "git", required: true, probes: []probeSpec{
			{name: "git version", args: []string{"--version"}, required: true, version: true},
		}},
		{name: "docker", binary: "docker", required: true, probes: []probeSpec{
			{name: "docker CLI version", args: []string{"--version"}, required: true, version: true},
			{name: "docker daemon", args: []string{"info", "--format", "{{json .ServerVersion}}"}, required: true, timeout: 3 * time.Second},
		}},
		{name: "harbor", binary: "harbor", required: true, probes: []probeSpec{
			{name: "harbor version", args: []string{"--version"}, required: false, version: true},
			{name: "harbor run capability", args: []string{"run", "--help"}, required: true, requireAll: []string{"--path", "--agent", "--model", "--n-attempts", "--n-concurrent", "--max-retries", "--env-file", "--mounts"}},
		}},
		{name: "go", binary: "go", required: true, minVersion: "1.26", probes: []probeSpec{
			{name: "go version", args: []string{"version"}, required: true, version: true, goMin: "1.26"},
		}},
	}
}

func checkTool(spec toolSpec) toolCheck {
	path, err := exec.LookPath(spec.binary)
	check := toolCheck{Name: spec.name, Binary: spec.binary, Required: spec.required, MinVersion: spec.minVersion}
	if err != nil {
		check.Error = err.Error()
		return check
	}
	check.Found = true
	check.Healthy = true
	check.Path = filepath.Clean(path)
	for _, probe := range spec.probes {
		result := runToolProbe(check.Path, probe)
		check.Probes = append(check.Probes, result)
		if probe.version && result.Passed && check.Version == "" {
			check.Version = firstLine(result.Output)
		}
		if probe.required && !result.Passed {
			check.Healthy = false
			if check.Error == "" {
				check.Error = result.Name + ": " + firstNonEmpty(result.Error, result.Output, "failed")
			}
		}
	}
	return check
}

func runToolProbe(path string, spec probeSpec) toolProbe {
	timeout := spec.timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, spec.args...)
	output, err := cmd.CombinedOutput()
	text := strings.TrimSpace(commandlog.RedactText(string(output)))
	probe := toolProbe{Name: spec.name, Args: spec.args, Required: spec.required, Output: truncateDoctorOutput(text)}
	if ctx.Err() != nil {
		probe.Error = spec.name + " timed out"
		return probe
	}
	if err != nil {
		detail := text
		if detail == "" {
			detail = err.Error()
		}
		probe.Error = commandlog.RedactText(detail)
		return probe
	}
	for _, required := range spec.requireAll {
		if !containsCapabilityMarker(text, required) {
			probe.Error = fmt.Sprintf("missing required capability marker %q", required)
			return probe
		}
	}
	if spec.goMin != "" {
		if ok, version := goVersionAtLeast(text, spec.goMin); !ok {
			probe.Error = fmt.Sprintf("go version %s is below required %s", version, spec.goMin)
			return probe
		}
	}
	probe.Passed = true
	return probe
}

func containsCapabilityMarker(text, marker string) bool {
	text = strings.TrimSpace(text)
	marker = strings.TrimSpace(marker)
	if text == "" || marker == "" {
		return false
	}
	if !strings.HasPrefix(marker, "-") {
		return strings.Contains(text, marker)
	}
	pattern := `(^|[\s,\[\]\(\)\{\}|])` + regexp.QuoteMeta(marker) + `($|[\s,=\[\]\(\)\{\}|])`
	return regexp.MustCompile(pattern).FindStringIndex(text) != nil
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	line, _, _ := strings.Cut(text, "\n")
	return strings.TrimSpace(line)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func truncateDoctorOutput(text string) string {
	if len(text) <= 1000 {
		return text
	}
	return text[:1000] + "\n... truncated ..."
}

var goVersionPattern = regexp.MustCompile(`go([0-9]+)\.([0-9]+)(?:\.([0-9]+))?`)

func goVersionAtLeast(output, min string) (bool, string) {
	current := parseGoVersion(output)
	required := parseGoVersion("go" + min)
	if current == "" || required == "" {
		return false, firstLine(output)
	}
	currentParts := versionParts(current)
	requiredParts := versionParts(required)
	for i := 0; i < len(requiredParts); i++ {
		if currentParts[i] > requiredParts[i] {
			return true, current
		}
		if currentParts[i] < requiredParts[i] {
			return false, current
		}
	}
	return true, current
}

func parseGoVersion(output string) string {
	match := goVersionPattern.FindStringSubmatch(output)
	if len(match) == 0 {
		return ""
	}
	patch := match[3]
	if patch == "" {
		patch = "0"
	}
	return match[1] + "." + match[2] + "." + patch
}

func versionParts(version string) [3]int {
	parts := strings.Split(version, ".")
	var out [3]int
	for i := 0; i < len(parts) && i < len(out); i++ {
		value, _ := strconv.Atoi(parts[i])
		out[i] = value
	}
	return out
}
