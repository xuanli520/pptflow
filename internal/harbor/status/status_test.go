package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
)

func TestReadWorkspaceLoadsStateAndFiltersEvents(t *testing.T) {
	workspace := t.TempDir()
	summary := domain.RunSummary{
		RunID:      "run-current",
		Workspace:  workspace,
		Status:     "failed",
		Passed:     false,
		StartedAt:  time.Now().Add(-time.Minute).UTC(),
		FinishedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	eventLog := strings.Join([]string{
		`{"run_id":"run-old","type":"run_succeeded","status":"succeeded","message":"old"}`,
		`{"run_id":"run-current","type":"node_failed","node_id":"codeedge_lint","status":"failed","message":"lint failed"}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(workspace, "event_log.jsonl"), []byte(eventLog), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := ReadWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !report.StatePresent || !report.EventLogPresent || report.RunID != "run-current" || report.EventCount != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if report.LatestEvent == nil || report.LatestEvent.Message != "lint failed" {
		t.Fatalf("unexpected latest event: %+v", report.LatestEvent)
	}
}

func TestReadWorkspaceRedactsLatestEvent(t *testing.T) {
	workspace := t.TempDir()
	secret := "raw-status-secret"
	summary := domain.RunSummary{
		RunID:     "run-current",
		Workspace: workspace,
		Status:    "running",
		Passed:    true,
	}
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	eventLog := `{"run_id":"run-current","type":"node_failed","node_id":"quality_check","status":"failed","message":"Bearer ` + secret + `","artifacts":[{"name":"quality_report.json","path":"API_KEY=` + secret + `","content":"API_KEY=` + secret + `"}],"logs":[{"name":"stderr.txt","content":"{\"OPENAI_API_KEY\":\"` + secret + `\"}"}],"gate":{"gate_id":"final_review","message":"API_TOKEN=` + secret + `","artifacts":[{"name":"gate.json","content":"API_TOKEN=` + secret + `"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(workspace, "event_log.jsonl"), []byte(eventLog), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := ReadWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, secret) {
		t.Fatalf("workspace status leaked secret: %s", text)
	}
	if report.LatestEvent == nil || len(report.LatestEvent.Artifacts) == 0 || report.LatestEvent.Artifacts[0].Content != "" {
		t.Fatalf("latest event should strip artifact content: %+v", report.LatestEvent)
	}
	if len(report.LatestEvent.Logs) == 0 || report.LatestEvent.Logs[0].Content != "" {
		t.Fatalf("latest event should strip log content: %+v", report.LatestEvent)
	}
	if report.LatestEvent.Gate == nil || len(report.LatestEvent.Gate.Artifacts) == 0 || report.LatestEvent.Gate.Artifacts[0].Content != "" {
		t.Fatalf("latest event should strip gate artifact content: %+v", report.LatestEvent)
	}
	if !strings.Contains(text, "redacted") {
		t.Fatalf("workspace status missing redaction marker: %s", text)
	}
}

func TestReadWorkspaceRedactsStateSummaryFields(t *testing.T) {
	workspace := t.TempDir()
	secret := "sk-rawstatestatussecret123456"
	summary := domain.RunSummary{
		RunID:             "run-" + secret,
		Workspace:         workspace,
		Status:            "failed API_KEY=" + secret,
		PersistenceErrors: []string{"Bearer " + secret},
	}
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "event_log.jsonl"), []byte(`{"run_id":"run-`+secret+`","type":"run_failed","status":"failed","message":"done"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := ReadWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, secret) {
		t.Fatalf("workspace status leaked state summary secret: %s", text)
	}
	if !strings.Contains(text, "redacted") {
		t.Fatalf("workspace status missing redaction marker: %s", text)
	}
}

func TestReadWorkspaceReportsRunOptionsResumability(t *testing.T) {
	workspace := t.TempDir()
	summary := domain.RunSummary{
		RunID:     "run-current",
		Workspace: workspace,
		Status:    "running",
	}
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "event_log.jsonl"), []byte(`{"run_id":"run-current","type":"run_started","status":"running","message":"started"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := "raw-status-options-secret"
	options := domain.RunnerOptionsSnapshot{
		SchemaVersion:          "harbor.runner_options.v1",
		Workspace:              workspace,
		TaskDir:                "/tmp/task",
		RunHarbor:              true,
		HarborAgentEnvOmitted:  true,
		GitHubTokenConfigured:  true,
		Description:            "Bearer " + secret,
		SensitiveFieldsOmitted: []string{"github_token", "harbor_agent_env_values"},
	}
	raw, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodes.RunOptionsPath(workspace), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := ReadWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !report.RunOptionsPresent || !report.Resumable || report.ResumeMode != "task" || report.RunOptions == nil {
		t.Fatalf("expected resumable run options, got %+v", report)
	}
	if len(report.ResumeWarnings) < 2 {
		t.Fatalf("expected sensitive omission warnings, got %+v", report.ResumeWarnings)
	}
	statusRaw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	text := string(statusRaw)
	if strings.Contains(text, secret) {
		t.Fatalf("workspace status leaked run options secret: %s", text)
	}
	if !strings.Contains(text, "redacted") {
		t.Fatalf("workspace status missing run options redaction marker: %s", text)
	}
}

func TestReadWorkspaceReportsMissingWorkspace(t *testing.T) {
	report, err := ReadWorkspace(filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("expected missing workspace error")
	}
	if len(report.Issues) == 0 || !strings.Contains(report.Issues[0], "no state.json") {
		t.Fatalf("expected diagnostic issue, got %+v", report)
	}
}
