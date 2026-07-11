package status

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/sanitize"
)

type WorkspaceStatus struct {
	SchemaVersion     string                        `json:"schema_version"`
	Workspace         string                        `json:"workspace"`
	StatePath         string                        `json:"state_path"`
	EventLogPath      string                        `json:"event_log_path"`
	RunOptionsPath    string                        `json:"run_options_path"`
	StatePresent      bool                          `json:"state_present"`
	EventLogPresent   bool                          `json:"event_log_present"`
	RunOptionsPresent bool                          `json:"run_options_present"`
	RunID             string                        `json:"run_id,omitempty"`
	Status            string                        `json:"status,omitempty"`
	Passed            bool                          `json:"passed"`
	StartedAt         time.Time                     `json:"started_at,omitempty"`
	FinishedAt        time.Time                     `json:"finished_at,omitempty"`
	EventCount        int                           `json:"event_count"`
	LatestEvent       *domain.RunnerEvent           `json:"latest_event,omitempty"`
	RunOptions        *domain.RunnerOptionsSnapshot `json:"run_options,omitempty"`
	Resumable         bool                          `json:"resumable"`
	ResumeMode        string                        `json:"resume_mode,omitempty"`
	ResumeWarnings    []string                      `json:"resume_warnings,omitempty"`
	PersistenceErrors []string                      `json:"persistence_errors,omitempty"`
	Issues            []string                      `json:"issues,omitempty"`
}

const maxEventLogLineBytes = 4 << 20

func ReadWorkspace(workspace string) (WorkspaceStatus, error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		workspace = filepath.Join(".harbor-factory", "workspace")
	}
	statePath := filepath.Join(workspace, "state.json")
	eventLogPath := filepath.Join(workspace, "event_log.jsonl")
	runOptionsPath := nodes.RunOptionsPath(workspace)
	report := WorkspaceStatus{
		SchemaVersion:  "harbor.workspace_status.v1",
		Workspace:      sanitize.Text(workspace),
		StatePath:      sanitize.Text(statePath),
		EventLogPath:   sanitize.Text(eventLogPath),
		RunOptionsPath: sanitize.Text(runOptionsPath),
	}
	var summary domain.RunSummary
	if raw, err := os.ReadFile(statePath); err == nil {
		report.StatePresent = true
		if err := json.Unmarshal(raw, &summary); err != nil {
			report.Issues = append(report.Issues, sanitize.Text("state.json is not valid JSON: "+err.Error()))
		} else {
			summary = sanitize.RunSummary(summary)
			report.RunID = summary.RunID
			report.Status = summary.Status
			report.Passed = summary.Passed
			report.StartedAt = summary.StartedAt
			report.FinishedAt = summary.FinishedAt
			report.PersistenceErrors = append(report.PersistenceErrors, sanitize.StringSlice(summary.PersistenceErrors)...)
		}
	} else if !os.IsNotExist(err) {
		report.Issues = append(report.Issues, sanitize.Text("cannot read state.json: "+err.Error()))
	}
	events, err := readEvents(eventLogPath, report.RunID)
	if err == nil {
		report.EventLogPresent = true
		report.EventCount = len(events)
		if len(events) > 0 {
			latest := publicRunnerEvent(events[len(events)-1])
			report.LatestEvent = &latest
		}
	} else if !os.IsNotExist(err) {
		report.Issues = append(report.Issues, sanitize.Text("cannot read event_log.jsonl: "+err.Error()))
	}
	if raw, err := os.ReadFile(runOptionsPath); err == nil {
		report.RunOptionsPresent = true
		var snapshot domain.RunnerOptionsSnapshot
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			report.Issues = append(report.Issues, sanitize.Text("run_options.json is not valid JSON: "+err.Error()))
		} else {
			if strings.TrimSpace(snapshot.Workspace) == "" {
				snapshot.Workspace = workspace
			}
			snapshot = sanitize.RunnerOptionsSnapshot(snapshot)
			report.RunOptions = &snapshot
			report.Resumable, report.ResumeMode, report.ResumeWarnings = runOptionsResumability(summary, snapshot)
		}
	} else if !os.IsNotExist(err) {
		report.Issues = append(report.Issues, sanitize.Text("cannot read run_options.json: "+err.Error()))
	}
	if !report.StatePresent && !report.EventLogPresent {
		report.Issues = append(report.Issues, "workspace has no state.json or event_log.jsonl")
		return report, fmt.Errorf("workspace has no state.json or event_log.jsonl: %s", sanitize.Text(workspace))
	}
	if report.StatePresent && report.EventLogPresent && report.EventCount == 0 {
		report.Issues = append(report.Issues, "event_log.jsonl has no events for current run_id")
	}
	if !report.RunOptionsPresent && !isTerminalSummary(summary) {
		report.ResumeWarnings = append(report.ResumeWarnings, "run_options.json is missing; TUI can inspect the workspace but cannot reconstruct a runner from workspace alone")
	}
	return report, nil
}

func readEvents(path, runID string) ([]domain.RunnerEvent, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var events []domain.RunnerEvent
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxEventLogLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event domain.RunnerEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		event = sanitize.RunnerEvent(event)
		if runID != "" && event.RunID != "" && event.RunID != runID {
			continue
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func publicRunnerEvent(event domain.RunnerEvent) domain.RunnerEvent {
	event = sanitize.RunnerEvent(event)
	for i := range event.Artifacts {
		event.Artifacts[i].Content = ""
	}
	for i := range event.Logs {
		event.Logs[i].Content = ""
	}
	if event.Gate != nil {
		gate := *event.Gate
		for i := range gate.Artifacts {
			gate.Artifacts[i].Content = ""
		}
		event.Gate = &gate
	}
	return event
}

func runOptionsResumability(summary domain.RunSummary, snapshot domain.RunnerOptionsSnapshot) (bool, string, []string) {
	if isTerminalSummary(summary) {
		return false, "snapshot", []string{"run is terminal; workspace opens as a read-only snapshot"}
	}
	var warnings []string
	mode := "task"
	if snapshot.Generate {
		mode = "generate"
		if strings.TrimSpace(snapshot.RepoURL) == "" || strings.TrimSpace(snapshot.Commit) == "" {
			return false, mode, []string{"generate resume requires repo_url and commit in run_options.json"}
		}
	} else if strings.TrimSpace(snapshot.TaskDir) == "" {
		return false, mode, []string{"task resume requires task_dir in run_options.json"}
	}
	if snapshot.HarborAgentEnvOmitted {
		warnings = append(warnings, "harbor agent env values were intentionally omitted; provide them externally before resuming Harbor runs")
	}
	if snapshot.GitHubTokenConfigured {
		warnings = append(warnings, "GitHub token value was intentionally omitted; GitHub similarity resume may require external token configuration")
	}
	for _, field := range snapshot.UnsupportedFieldsOmitted {
		warnings = append(warnings, "unsupported runtime field omitted from resumable options: "+field)
	}
	for i := range warnings {
		warnings[i] = sanitize.Text(warnings[i])
	}
	return true, mode, warnings
}

func isTerminalSummary(summary domain.RunSummary) bool {
	status := strings.TrimSpace(summary.Status)
	return !summary.FinishedAt.IsZero() || status == "succeeded" || status == "failed"
}
