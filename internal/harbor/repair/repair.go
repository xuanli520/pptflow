package repair

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/commandlog"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	prompttemplates "github.com/purplevoid/harbor-factory/internal/templates"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

type Options struct {
	TaskDir         string
	Guidance        string
	Findings        []string
	Source          string
	Round           int
	Agent           workflow.AgentRuntime
	Model           string
	ReasoningEffort string
	TimeoutSeconds  int
	LogPath         string
	WriteReport     string
}

type Report struct {
	SchemaVersion string    `json:"schema_version"`
	TaskDir       string    `json:"task_dir"`
	Source        string    `json:"source"`
	Round         int       `json:"round"`
	Guidance      string    `json:"guidance,omitempty"`
	Findings      []string  `json:"findings,omitempty"`
	BeforeDigest  string    `json:"before_digest"`
	AfterDigest   string    `json:"after_digest,omitempty"`
	AgentModel    string    `json:"agent_model,omitempty"`
	AgentOutput   string    `json:"agent_output,omitempty"`
	Changed       bool      `json:"changed"`
	CreatedAt     time.Time `json:"created_at"`
}

func Run(ctx context.Context, opts Options) (Report, error) {
	report := Report{
		SchemaVersion: "harbor.task_repair_report.v1",
		TaskDir:       strings.TrimSpace(opts.TaskDir),
		Source:        commandlog.RedactText(strings.TrimSpace(opts.Source)),
		Round:         opts.Round,
		Guidance:      commandlog.RedactText(strings.TrimSpace(opts.Guidance)),
		Findings:      redactStrings(opts.Findings),
		CreatedAt:     time.Now().UTC(),
	}
	if report.TaskDir == "" {
		return finish(report, opts.WriteReport, fmt.Errorf("task directory is required"))
	}
	if opts.Agent == nil {
		return finish(report, opts.WriteReport, fmt.Errorf("Codex repair agent is required"))
	}
	before, err := harborrun.ComputeTaskDigest(report.TaskDir)
	if err != nil {
		return finish(report, opts.WriteReport, fmt.Errorf("compute task digest before repair: %w", err))
	}
	report.BeforeDigest = before
	timeout := opts.TimeoutSeconds
	if timeout <= 0 {
		timeout = 600
	}
	prompt, err := buildPrompt(report)
	if err != nil {
		return finish(report, opts.WriteReport, fmt.Errorf("render task repair prompt: %w", err))
	}
	result, err := workflow.RunAgentTurn(ctx, opts.Agent, workflow.AgentTurnRequest{
		ProjectPath:     report.TaskDir,
		Prompt:          prompt,
		Model:           opts.Model,
		ReasoningEffort: opts.ReasoningEffort,
		SandboxMode:     "workspace-write",
		SandboxPolicy:   "workspace-write",
		NetworkAccess:   false,
		WorkspaceRoots:  []string{report.TaskDir},
		TimeoutSeconds:  timeout,
		MaxOutputBytes:  2 << 20,
		LogPath:         opts.LogPath,
	})
	if err != nil {
		return finish(report, opts.WriteReport, fmt.Errorf("Codex task repair: %w", err))
	}
	report.AgentModel = commandlog.RedactText(strings.TrimSpace(result.Model))
	report.AgentOutput = truncate(commandlog.RedactText(result.Text), 12000)
	after, err := harborrun.ComputeTaskDigest(report.TaskDir)
	if err != nil {
		return finish(report, opts.WriteReport, fmt.Errorf("compute task digest after repair: %w", err))
	}
	report.AfterDigest = after
	report.Changed = !strings.EqualFold(before, after)
	if !report.Changed {
		return finish(report, opts.WriteReport, fmt.Errorf("Codex repair completed without changing task files"))
	}
	return finish(report, opts.WriteReport, nil)
}

func Reusable(path, taskDir, guidance, source string) (Report, bool) {
	raw, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return Report{}, false
	}
	var report Report
	if json.Unmarshal(raw, &report) != nil || report.SchemaVersion != "harbor.task_repair_report.v1" || !report.Changed {
		return Report{}, false
	}
	want, err := filepath.Abs(taskDir)
	if err != nil {
		return Report{}, false
	}
	got, err := filepath.Abs(report.TaskDir)
	if err != nil || filepath.Clean(got) != filepath.Clean(want) {
		return Report{}, false
	}
	current, err := harborrun.ComputeTaskDigest(taskDir)
	if err != nil || !strings.EqualFold(current, report.AfterDigest) {
		return Report{}, false
	}
	if strings.TrimSpace(commandlog.RedactText(guidance)) != report.Guidance || strings.TrimSpace(commandlog.RedactText(source)) != report.Source {
		return Report{}, false
	}
	return report, true
}

type repairPromptData struct {
	Source       string
	Round        int
	BeforeDigest string
	Findings     string
	Guidance     string
}

func buildPrompt(report Report) (string, error) {
	findings := "- No machine-check finding was supplied. Inspect the task files and operator guidance carefully."
	if len(report.Findings) > 0 {
		var lines []string
		for _, finding := range report.Findings {
			lines = append(lines, "- "+finding)
		}
		findings = strings.Join(lines, "\n")
	}
	guidance := report.Guidance
	if guidance == "" {
		guidance = "No additional operator guidance. Resolve the listed review failures with the smallest coherent task changes."
	}
	engine, err := prompttemplates.Default()
	if err != nil {
		return "", fmt.Errorf("load prompt templates: %w", err)
	}
	return engine.Render("phase2/task_repair", repairPromptData{
		Source:       report.Source,
		Round:        report.Round,
		BeforeDigest: report.BeforeDigest,
		Findings:     findings,
		Guidance:     guidance,
	})
}

func finish(report Report, path string, runErr error) (Report, error) {
	path = strings.TrimSpace(path)
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil && runErr == nil {
			runErr = err
		} else if err == nil {
			raw, marshalErr := json.MarshalIndent(report, "", "  ")
			if marshalErr != nil && runErr == nil {
				runErr = marshalErr
			} else if marshalErr == nil {
				if writeErr := os.WriteFile(path, append(raw, '\n'), 0o600); writeErr != nil && runErr == nil {
					runErr = writeErr
				}
			}
		}
	}
	return report, runErr
}

func redactStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(commandlog.RedactText(value)); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "\n... truncated ..."
}
