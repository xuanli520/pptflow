package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/executor"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/runlock"
	"github.com/purplevoid/harbor-factory/internal/harbor/secretscan"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

func TestRunnerEmitsGateAndPersistsDecision(t *testing.T) {
	taskDir := writeRunnerTask(t)
	workspace := t.TempDir()
	runner := NewRunner(RunnerOptions{
		TaskDir:   taskDir,
		Workspace: workspace,
	})
	done := make(chan error, 1)
	go func() {
		summary, err := runner.Run(context.Background())
		if err != nil {
			done <- err
			return
		}
		if !summary.Passed {
			done <- os.ErrInvalid
			return
		}
		done <- nil
	}()

	var gate *domain.GateRequest
	timeout := time.After(5 * time.Second)
	for gate == nil {
		select {
		case event := <-runner.Events():
			if event.Type == "gate_requested" {
				gate = event.Gate
			}
		case <-timeout:
			t.Fatal("timed out waiting for gate")
		}
	}
	stateRaw, err := os.ReadFile(filepath.Join(workspace, "state.json"))
	if err != nil {
		t.Fatalf("state.json should exist while gate is waiting: %v", err)
	}
	var waitingState domain.RunSummary
	if err := json.Unmarshal(stateRaw, &waitingState); err != nil {
		t.Fatalf("state.json should be parseable while gate is waiting: %v\n%s", err, stateRaw)
	}
	if waitingState.RunID == "" || waitingState.Status != "running" {
		t.Fatalf("unexpected waiting state: %+v", waitingState)
	}
	runner.SubmitGateDecision(domain.GateDecision{RequestID: gate.RequestID, GateID: gate.GateID, Approved: true, Notes: "reviewed"})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	decisionPath := filepath.Join(workspace, "phase2", "artifacts", "reviews", "final_review", "decision.json")
	raw, err := os.ReadFile(decisionPath)
	if err != nil {
		t.Fatal(err)
	}
	var decision domain.GateDecision
	if err := json.Unmarshal(raw, &decision); err != nil {
		t.Fatal(err)
	}
	if !decision.Approved || decision.Notes != "reviewed" {
		t.Fatalf("decision = %+v", decision)
	}
	if _, err := os.Stat(filepath.Join(workspace, "state.json")); err != nil {
		t.Fatalf("missing state.json: %v", err)
	}
	eventLog, err := os.ReadFile(filepath.Join(workspace, "event_log.jsonl"))
	if err != nil {
		t.Fatalf("missing event_log.jsonl: %v", err)
	}
	if !strings.Contains(string(eventLog), "gate_requested") || !strings.Contains(string(eventLog), "run_succeeded") {
		t.Fatalf("event log missing expected events: %s", eventLog)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(eventLog)), "\n") {
		var event domain.RunnerEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("event log line is not JSON: %v\n%s", err, line)
		}
		if event.RunID == "" {
			t.Fatalf("event missing run_id: %+v", event)
		}
	}
}

func TestRunnerRejectsConcurrentWorkspaceOwnerWithoutPersisting(t *testing.T) {
	workspace := t.TempDir()
	marker := []byte("existing event\n")
	eventPath := filepath.Join(workspace, "event_log.jsonl")
	if err := os.WriteFile(eventPath, marker, 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := runlock.Acquire(workspace, runlock.Metadata{RunID: "run-owner"})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	runner := NewRunner(RunnerOptions{Workspace: workspace, TaskDir: writeRunnerTask(t), AutoApprove: true})
	summary, err := runner.Run(context.Background())
	if !errors.Is(err, runlock.ErrActive) {
		t.Fatalf("concurrent runner error = %v, want ErrActive", err)
	}
	if summary.RunID != "" || summary.Status != "" {
		t.Fatalf("rejected runner should not claim a run: %+v", summary)
	}
	raw, readErr := os.ReadFile(eventPath)
	if readErr != nil || string(raw) != string(marker) {
		t.Fatalf("rejected runner modified event log: err=%v raw=%q", readErr, raw)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "state.json")); !os.IsNotExist(statErr) {
		t.Fatalf("rejected runner wrote state.json: %v", statErr)
	}
}

func TestRunnerRedactsGateArtifactsInEventLog(t *testing.T) {
	workspace := t.TempDir()
	runner := NewRunner(RunnerOptions{Workspace: workspace})
	runner.emitEvent(domain.RunnerEvent{
		Type:    "gate_requested",
		NodeID:  "codeedge_lint",
		Status:  "waiting",
		Message: "review API_KEY=unit-test-secret",
		Artifacts: []domain.ArtifactPreview{{
			Name:    "lint_report.json",
			Path:    filepath.Join(workspace, "lint_report.json"),
			Content: "API_KEY=unit-test-secret and token=unit-test-secret",
		}},
		Gate: &domain.GateRequest{
			GateID:  "final_review",
			Message: "gate Bearer unit-test-secret-token",
			Artifacts: []domain.ArtifactPreview{{
				Name:    "lint_report.json",
				Path:    filepath.Join(workspace, "lint_report.json"),
				Content: "API_TOKEN=unit-test-secret",
			}},
			Checklist: []domain.ChecklistItem{{ID: "secret", Label: "Bearer unit-test-secret-token"}},
		},
	})

	event := <-runner.Events()
	if strings.Contains(event.Message, "unit-test-secret") || strings.Contains(event.Artifacts[0].Content, "unit-test-secret") || strings.Contains(event.Gate.Message, "unit-test-secret") || strings.Contains(event.Gate.Artifacts[0].Content, "unit-test-secret") {
		t.Fatalf("event was not redacted: %+v", event)
	}
	raw, err := os.ReadFile(filepath.Join(workspace, "event_log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	logText := string(raw)
	if strings.Contains(logText, "unit-test-secret") {
		t.Fatalf("event log contains unredacted secret: %s", logText)
	}
	if !strings.Contains(logText, "redacted") {
		t.Fatalf("event log missing redaction marker: %s", logText)
	}
}

func TestRunnerRedactsEventLogsInEventLog(t *testing.T) {
	workspace := t.TempDir()
	runner := NewRunner(RunnerOptions{Workspace: workspace})
	runner.emitEvent(domain.RunnerEvent{
		Type:    "node_failed",
		NodeID:  "harbor_run_qwen",
		Status:  "failed",
		Message: "failed",
		Logs: []domain.ArtifactPreview{{
			Name:    "stderr.txt",
			Path:    filepath.Join(workspace, "stderr.txt"),
			Content: `{"OPENAI_API_KEY":"raw-log-secret"}`,
		}},
	})

	event := <-runner.Events()
	if strings.Contains(event.Logs[0].Content, "raw-log-secret") {
		t.Fatalf("event log preview was not redacted: %+v", event.Logs[0])
	}
	raw, err := os.ReadFile(filepath.Join(workspace, "event_log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "raw-log-secret") {
		t.Fatalf("event_log.jsonl leaked log content secret: %s", raw)
	}
}

func TestRunnerWriteStateRedactsSummary(t *testing.T) {
	workspace := t.TempDir()
	runner := NewRunner(RunnerOptions{Workspace: workspace})
	secret := "rawstatesecret"
	summary := domain.RunSummary{
		RunID:     "run-secret",
		Workspace: workspace,
		Status:    "failed",
		Passed:    false,
		RepoPrepared: &domain.RepoPrepared{
			RepoURL:    "https://token@example.com/repo.git",
			SourcePath: filepath.Join(workspace, "API_KEY="+secret),
		},
		QualityReport: &domain.QualityReport{
			TaskDir:     filepath.Join(workspace, "task"),
			Checks:      map[string]domain.QualityCheck{"agent": {Detail: `{"API_TOKEN":"` + secret + `"}`, Severity: "warning", Source: "agent"}},
			AgentOutput: "Bearer " + secret,
			Issues:      []string{"OPENAI_API_KEY=" + secret},
		},
		QwenResult: &domain.TrialResult{
			Model:          "qwen3.7-max",
			Trials:         4,
			Runs:           []domain.TrialRun{{Trial: 1, FailureReason: "github_pat_" + secret + "123456"}},
			ResultPath:     filepath.Join(workspace, "API_TOKEN="+secret+".json"),
			Screenshot:     filepath.Join(workspace, "API_TOKEN="+secret+".png"),
			CommandRunPath: filepath.Join(workspace, "API_TOKEN="+secret+"-command.json"),
		},
		VerifyReport: &domain.VerifyReport{
			TaskDir: filepath.Join(workspace, "task"),
			DockerBuild: &domain.CommandRun{
				Command: "docker build",
				Argv:    []string{"docker", "build", "--secret", secret},
				Env:     []string{"OPENAI_API_KEY=" + secret},
				Stdout:  `{"OPENAI_API_KEY":"` + secret + `"}`,
				Stderr:  "Bearer " + secret,
			},
		},
		PersistenceErrors: []string{"API_KEY=" + secret},
		Events: []domain.RunnerEvent{{
			Type:    "node_failed",
			Message: "Bearer " + secret,
			Logs:    []domain.ArtifactPreview{{Content: "API_KEY=" + secret}},
		}},
		GateDecisions: []domain.GateDecision{{
			GateID: "final_review",
			Notes:  "API_KEY=" + secret,
		}},
	}
	if err := runner.writeState(summary); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(workspace, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "token@") {
		t.Fatalf("state.json leaked secret-like content: %s", raw)
	}
	if !strings.Contains(string(raw), "redacted") {
		t.Fatalf("state.json missing redaction marker: %s", raw)
	}
}

func TestSaveRunnerOptionsPersistsResumableSnapshotWithoutSecrets(t *testing.T) {
	clearClaudeEnvironment(t)
	workspace := t.TempDir()
	secret := "raw-run-options-secret"
	t.Setenv("ANTHROPIC_AUTH_TOKEN", secret)
	t.Setenv("ANTHROPIC_BASE_URL", "https://example.invalid")
	snapshot, err := SaveRunnerOptions(RunnerOptions{
		Workspace:             workspace,
		TaskDir:               "/tmp/task",
		TestsAnalysis:         "/tmp/task/tests_analysis.md",
		AutoApprove:           true,
		QualityCheck:          true,
		SimilarityHistoryDirs: []string{"/tmp/history"},
		SimilarityThreshold:   0.37,
		GitHubToken:           "github_pat_" + secret + "123456",
		RunHarbor:             true,
		HarborModels:          "opus",
		HarborAgent:           "claude-code",
		HarborAgentEnv:        []string{"ANTHROPIC_AUTH_TOKEN=" + secret, "ANTHROPIC_BASE_URL=https://example.invalid"},
		QwenModel:             "qwen3.7-max",
		OpusModel:             "claude-opus-4-8",
		QwenHarborBaseURL:     "https://qwen.example/v1",
		OpusHarborBaseURL:     "https://opus.example/v1",
		HarborTimeout:         123,
		HarborSetupTimeout:    321,
		HarborPreflight:       true,
		HarborConcurrency:     2,
		HarborAttempts:        4,
		HarborInfraRetries:    3,
		Package:               true,
		OutputDir:             "/tmp/output",
		Description:           "Bearer " + secret,
		VerifyExec:            &runnerResultExec{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.GitHubTokenConfigured || !snapshot.HarborAgentEnvOmitted {
		t.Fatalf("snapshot should record omitted sensitive fields: %+v", snapshot)
	}
	raw, err := os.ReadFile(nodes.RunOptionsPath(workspace))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, leaked := range []string{secret, "github_pat_", "ANTHROPIC_AUTH_TOKEN=" + secret} {
		if strings.Contains(text, leaked) {
			t.Fatalf("run_options.json leaked %q: %s", leaked, text)
		}
	}
	if !strings.Contains(text, "harbor_agent_env_values") || !strings.Contains(text, "verify_exec") || !strings.Contains(text, "redacted") {
		t.Fatalf("run_options.json missing omission/redaction evidence: %s", text)
	}
	if findings := secretscan.ScanBytes("run_options.json", raw); len(findings) > 0 {
		t.Fatalf("run_options.json should not trigger secret scanner: %+v\n%s", findings, text)
	}
	t.Setenv("ANTHROPIC_BASE_URL", "")

	loaded, loadedSnapshot, err := LoadRunnerOptions(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace != workspace || loaded.TaskDir != "/tmp/task" || !loaded.AutoApprove || !loaded.QualityCheck || loaded.SimilarityThreshold != 0.37 || !loaded.RunHarbor || loaded.HarborModels != "opus" || loaded.QwenHarborBaseURL != "https://qwen.example/v1" || loaded.OpusHarborBaseURL != "https://opus.example/v1" || loaded.HarborTimeout != 123 || loaded.HarborSetupTimeout != 321 || !loaded.HarborPreflight || loaded.HarborConcurrency != 2 || loaded.HarborAttempts != 4 || loaded.HarborInfraRetries != 3 || !loaded.Package {
		t.Fatalf("loaded options lost non-sensitive fields: %+v", loaded)
	}
	if loaded.GitHubToken != "" || loaded.VerifyExec != nil || loaded.Agent != nil {
		t.Fatalf("loaded options restored sensitive/unsupported fields: %+v", loaded)
	}
	wantAgentEnv := []string{"ANTHROPIC_BASE_URL=${ANTHROPIC_BASE_URL}", "ANTHROPIC_AUTH_TOKEN=${ANTHROPIC_AUTH_TOKEN}"}
	if strings.Join(loaded.HarborAgentEnv, "\n") != strings.Join(wantAgentEnv, "\n") {
		t.Fatalf("loaded options did not restore safe environment templates: got=%q want=%q", loaded.HarborAgentEnv, wantAgentEnv)
	}
	if !loadedSnapshot.HarborAgentEnvOmitted || len(loadedSnapshot.HarborAgentEnvKeys) != 2 {
		t.Fatalf("loaded snapshot missing env omission metadata: %+v", loadedSnapshot)
	}
}

func TestRunnerRedactsPersistedGateDecision(t *testing.T) {
	workspace := t.TempDir()
	runner := NewRunner(RunnerOptions{Workspace: workspace})
	secret := "raw-decision-secret"
	err := runner.writeGateDecision("phase2", "final_review", domain.GateDecision{
		RequestID: "req-1",
		GateID:    "final_review",
		Approved:  true,
		Notes:     "API_KEY=" + secret,
		EditedFiles: map[string]string{
			"https://token@github.com/org/repo": "Bearer " + secret,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(workspace, "phase2", "artifacts", "reviews", "final_review", "decision.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, secret) || strings.Contains(text, "token@github") {
		t.Fatalf("decision file leaked secret-like content: %s", text)
	}
	if !strings.Contains(text, "redacted") {
		t.Fatalf("decision file missing redaction marker: %s", text)
	}
}

func TestRunnerFinalReviewCanReviseAndRerunChecks(t *testing.T) {
	taskDir := writeRunnerTask(t)
	workspace := t.TempDir()
	runner := NewRunner(RunnerOptions{TaskDir: taskDir, Workspace: workspace})
	type runResult struct {
		summary domain.RunSummary
		err     error
	}
	done := make(chan runResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		summary, err := runner.Run(ctx)
		done <- runResult{summary: summary, err: err}
	}()

	revisionSubmitted := false
	approvalSubmitted := false
	lintPasses := 0
	for !approvalSubmitted {
		select {
		case event := <-runner.Events():
			if event.NodeID == nodes.CodeEdgeLint && event.Status == "succeeded" {
				lintPasses++
			}
			if event.Type != "gate_requested" || event.Gate == nil || event.Gate.GateID != nodes.FinalReview {
				continue
			}
			if !revisionSubmitted {
				instruction := filepath.Join(taskDir, "instruction.md")
				if err := os.WriteFile(instruction, []byte("Fix the task and preserve public behavior.\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				runner.SubmitGateDecision(domain.GateDecision{RequestID: event.Gate.RequestID, GateID: nodes.FinalReview, Action: "revise", EditedFiles: map[string]string{instruction: "updated"}})
				revisionSubmitted = true
				continue
			}
			runner.SubmitGateDecision(domain.GateDecision{RequestID: event.Gate.RequestID, GateID: nodes.FinalReview, Action: "approve", Approved: true})
			approvalSubmitted = true
		case <-ctx.Done():
			t.Fatal("timed out waiting for revised Final Review")
		}
	}
	result := <-done
	if result.err != nil || !result.summary.Passed {
		t.Fatalf("revised run failed: summary=%+v err=%v", result.summary, result.err)
	}
	if lintPasses < 2 {
		t.Fatalf("expected lint to rerun, observed %d successful passes", lintPasses)
	}
	revisionPath := filepath.Join(workspace, "phase2", "artifacts", "reviews", nodes.FinalReview, "revisions", "revision-001.json")
	if _, err := os.Stat(revisionPath); err != nil {
		t.Fatalf("revision chain was not persisted: %v", err)
	}
}

func TestRunnerLabelsRecoveredRunAndNodeReuseBoundary(t *testing.T) {
	taskDir := writeRunnerTask(t)
	workspace := t.TempDir()
	previous := domain.RunSummary{RunID: "run-before-crash", Workspace: workspace, Status: "running"}
	raw, err := json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(RunnerOptions{TaskDir: taskDir, Workspace: workspace, AutoApprove: true})
	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Recovered || summary.PreviousRunID != "run-before-crash" || summary.RunID == summary.PreviousRunID {
		t.Fatalf("recovery boundary missing: %+v", summary)
	}
	if !eventMessageSeen(summary.Events, "", "previous=run-before-crash") || len(summary.RerunNodes) == 0 {
		t.Fatalf("recovery evidence missing: %+v", summary)
	}
}

func TestRunnerRejectsClaudeHarborRunWithoutContainerCredential(t *testing.T) {
	clearClaudeEnvironment(t)
	runner := NewRunner(RunnerOptions{TaskDir: writeRunnerTask(t), Workspace: t.TempDir(), RunHarbor: true, AutoApprove: true})
	summary, err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "host Claude OAuth is not inherited") {
		t.Fatalf("expected actionable Harbor container credential error, summary=%+v err=%v", summary, err)
	}
}

func TestRunnerRejectsClaudeHarborRunWithUnresolvedCredentialTemplate(t *testing.T) {
	clearClaudeEnvironment(t)
	t.Setenv("MISSING_HARBOR_TOKEN", "")
	runner := NewRunner(RunnerOptions{
		TaskDir:        writeRunnerTask(t),
		Workspace:      t.TempDir(),
		RunHarbor:      true,
		AutoApprove:    true,
		HarborAgentEnv: []string{"ANTHROPIC_AUTH_TOKEN=${MISSING_HARBOR_TOKEN}"},
	})
	summary, err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "host Claude OAuth is not inherited") {
		t.Fatalf("expected unresolved Harbor credential template error, summary=%+v err=%v", summary, err)
	}
}

func TestRunnerAcceptsResolvedCredentialTemplate(t *testing.T) {
	t.Setenv("HARBOR_TEST_TOKEN", "runtime-only-token")
	if !hasClaudeCredential([]string{"ANTHROPIC_AUTH_TOKEN=${HARBOR_TEST_TOKEN}"}) {
		t.Fatal("expected credential template backed by a non-empty host variable to be accepted")
	}
}

func TestCancelQwenStageContinuesToOpus(t *testing.T) {
	taskDir := writeRunnerTask(t)
	workspace := t.TempDir()
	exec := &runnerCancelableHarborExec{delegate: runnerHarborExec{outputs: []string{
		runnerTrialResultJSON("claude-opus-4-8", []domain.TrialRun{
			{Trial: 1, Passed: true, Turns: 28, Reward: 1},
			{Trial: 2, Passed: true, Turns: 29, Reward: 1},
			{Trial: 3, Passed: true, Turns: 27, Reward: 1},
			{Trial: 4, Turns: 28},
		}, ""),
	}}}
	runner := NewRunner(RunnerOptions{
		TaskDir:           taskDir,
		Workspace:         workspace,
		AutoApprove:       true,
		RunHarbor:         true,
		HarborAgentEnv:    []string{"ANTHROPIC_AUTH_TOKEN=test-token"},
		QwenHarborBaseURL: "https://qwen.example/v1",
		OpusHarborBaseURL: "https://opus.example/v1",
		HarborExec:        exec,
	})
	type runResult struct {
		summary domain.RunSummary
		err     error
	}
	done := make(chan runResult, 1)
	go func() {
		summary, err := runner.Run(context.Background())
		done <- runResult{summary: summary, err: err}
	}()
	cancelRequested := false
	for event := range runner.Events() {
		if event.NodeID == nodes.HarborRunQwen && event.Type == "node_started" {
			cancelRequested = runner.CancelNode(nodes.HarborRunQwen)
		}
	}
	result := <-done
	if result.err != nil {
		t.Fatalf("single-stage cancellation should not abort the runner: %v", result.err)
	}
	if !cancelRequested || result.summary.Passed {
		t.Fatalf("expected an audited canceled stage and failing summary: requested=%v summary=%+v", cancelRequested, result.summary)
	}
	if result.summary.OpusResult == nil || !eventSeen(result.summary.Events, nodes.HarborRunOpus, "succeeded") || !eventSeen(result.summary.Events, nodes.HarborRunQwen, "canceled") {
		t.Fatalf("Opus did not continue after Qwen cancellation: %+v", result.summary)
	}
}

func TestFailedQwenStageStillRunsOpus(t *testing.T) {
	taskDir := writeRunnerTask(t)
	workspace := t.TempDir()
	exec := &runnerFailFirstHarborExec{delegate: runnerHarborExec{outputs: []string{
		runnerTrialResultJSON("claude-opus-4-8", []domain.TrialRun{
			{Trial: 1, Passed: true, Turns: 28, Reward: 1},
			{Trial: 2, Passed: true, Turns: 29, Reward: 1},
			{Trial: 3, Passed: true, Turns: 27, Reward: 1},
			{Trial: 4, Turns: 28},
		}, ""),
	}}}
	runner := NewRunner(RunnerOptions{
		TaskDir:        taskDir,
		Workspace:      workspace,
		AutoApprove:    true,
		RunHarbor:      true,
		HarborAgentEnv: []string{"ANTHROPIC_AUTH_TOKEN=test-token"},
		HarborExec:     exec,
	})
	summary, err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Qwen Harbor stage") {
		t.Fatalf("expected aggregated Qwen failure, summary=%+v err=%v", summary, err)
	}
	if summary.Passed || summary.QwenResult == nil || summary.OpusResult == nil {
		t.Fatalf("expected failed Qwen evidence and successful Opus evidence: %+v", summary)
	}
	if !eventSeen(summary.Events, nodes.HarborRunQwen, "failed") || !eventSeen(summary.Events, nodes.HarborRunOpus, "succeeded") {
		t.Fatalf("both model stages were not attempted: %+v", summary.Events)
	}
}

func TestRunnerCanRunOnlyOpusForEvidenceRecovery(t *testing.T) {
	taskDir := writeRunnerTask(t)
	exec := &runnerHarborExec{outputs: []string{
		runnerTrialResultJSON("claude-opus-4-8", []domain.TrialRun{
			{Trial: 1, Passed: true, Turns: 28, Reward: 1},
			{Trial: 2, Passed: true, Turns: 29, Reward: 1},
			{Trial: 3, Passed: true, Turns: 27, Reward: 1},
			{Trial: 4, Turns: 28},
		}, ""),
	}}
	runner := NewRunner(RunnerOptions{
		TaskDir:        taskDir,
		Workspace:      t.TempDir(),
		AutoApprove:    true,
		RunHarbor:      true,
		HarborModels:   "opus",
		HarborAgentEnv: []string{"ANTHROPIC_AUTH_TOKEN=test-token"},
		HarborExec:     exec,
	})
	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Passed || summary.QwenResult != nil || summary.OpusResult == nil || exec.counter != 1 {
		t.Fatalf("expected one Opus-only recovery run with incomplete final evidence: executions=%d summary=%+v", exec.counter, summary)
	}
	if eventSeen(summary.Events, nodes.HarborRunQwen, "running") || !eventSeen(summary.Events, nodes.HarborRunOpus, "succeeded") {
		t.Fatalf("unexpected model stages: %+v", summary.Events)
	}
}

func TestRunnerRejectsUnknownHarborModelSelection(t *testing.T) {
	runner := NewRunner(RunnerOptions{TaskDir: writeRunnerTask(t), Workspace: t.TempDir(), RunHarbor: true, HarborModels: "opus,other", HarborAgent: "other", AutoApprove: true})
	_, err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "accepts only qwen and opus") {
		t.Fatalf("expected invalid Harbor model selection error, got %v", err)
	}
}

func TestRunnerRejectsCredentialedModelRouteURL(t *testing.T) {
	runner := NewRunner(RunnerOptions{
		TaskDir:           writeRunnerTask(t),
		Workspace:         t.TempDir(),
		RunHarbor:         true,
		HarborAgentEnv:    []string{"ANTHROPIC_AUTH_TOKEN=test-token"},
		QwenHarborBaseURL: "https://user:password@example.invalid/v1",
		AutoApprove:       true,
	})
	_, err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "must not contain credentials") {
		t.Fatalf("expected credentialed per-model route rejection, got %v", err)
	}
}

func TestRunnerRejectsApprovedFailingCriticalGate(t *testing.T) {
	decision := enforceGateDecision(domain.GateRequest{
		RequestID: "req-1",
		GateID:    "final_review",
		Checklist: []domain.ChecklistItem{{ID: "critical", Label: "blocking check", Critical: true, Passed: false}},
	}, domain.GateDecision{
		RequestID: "req-1",
		GateID:    "final_review",
		Approved:  true,
		Notes:     "looks fine",
		DecidedAt: time.Now(),
	})
	if decision.Approved {
		t.Fatalf("failing critical gate should reject approval: %+v", decision)
	}
	if !strings.Contains(decision.Notes, "blocking check") {
		t.Fatalf("decision notes missing blocker: %+v", decision)
	}
}

func TestGeneratedTaskChecklistBlocksDirtyLegacyOutput(t *testing.T) {
	taskDir := writeRunnerTask(t)
	analysis := filepath.Join(t.TempDir(), "tests_analysis.md")
	if err := os.WriteFile(analysis, []byte(validRunnerTestsAnalysis()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "environment", "promptflow_runner.py"), []byte("legacy image2 presentation residue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	items := generatedTaskChecklist(taskDir, analysis)
	blockers := blockingGateChecklist(items)
	joined := strings.Join(blockers, "\n")
	if !strings.Contains(joined, "unexpected files") || !strings.Contains(joined, "legacy residue") {
		t.Fatalf("expected dirty output blockers, got items=%+v blockers=%v", items, blockers)
	}
}

func TestGeneratedTaskChecklistAllowsWordsContainingLegacySubstrings(t *testing.T) {
	taskDir := writeRunnerTask(t)
	solution := filepath.Join(taskDir, "solution", "solve.sh")
	raw, err := os.ReadFile(solution)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(solution, append(raw, []byte("\n# encoded representation and slider state\n")...), 0o755); err != nil {
		t.Fatal(err)
	}
	extras, legacy, err := generatedTaskResidue(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(extras) != 0 || len(legacy) != 0 {
		t.Fatalf("normal identifier substrings were rejected: extras=%v legacy=%v", extras, legacy)
	}
}

func TestRunnerReadsWorkspaceGateDecision(t *testing.T) {
	workspace := t.TempDir()
	runner := NewRunner(RunnerOptions{Workspace: workspace})
	expectedRequestID := "phase2:final_review"
	path := nodes.ReviewDecisionPath(workspace, "phase2", nodes.FinalReview)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(domain.GateDecision{
		RequestID: expectedRequestID,
		GateID:    nodes.FinalReview,
		Approved:  true,
		Notes:     "from file",
		DecidedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	decision, err := runner.reviewGate(ctx, nodes.FinalReview, "Final Review", nodes.FinalReview, "Review", []domain.ChecklistItem{{ID: "ok", Label: "ok", Critical: true, Passed: true}}, nil, "phase2")
	if err != nil {
		t.Fatal(err)
	}
	if decision.RequestID != expectedRequestID || !decision.Approved || decision.Notes != "from file" {
		t.Fatalf("unexpected decision: %+v", decision)
	}

	select {
	case event := <-runner.Events():
		if event.Type != "gate_requested" || event.Gate == nil {
			t.Fatalf("expected gate event, got %+v", event)
		}
		if event.Gate.RequestID != expectedRequestID {
			t.Fatalf("gate request_id = %q, want %q", event.Gate.RequestID, expectedRequestID)
		}
	default:
		t.Fatal("expected gate event")
	}
}

func TestRunnerRejectsEmptyRun(t *testing.T) {
	runner := NewRunner(RunnerOptions{Workspace: t.TempDir(), AutoApprove: true})
	summary, err := runner.Run(context.Background())
	if err == nil {
		t.Fatal("expected empty run error")
	}
	if summary.Passed {
		t.Fatalf("empty run should not pass: %+v", summary)
	}
	if !strings.Contains(err.Error(), "--task or --generate") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunnerGenerateThenLintWithAutoApprovedGates(t *testing.T) {
	repoURL, commit := writeGitRepo(t)
	workspace := t.TempDir()
	agent := &runnerFakeAgent{outputs: []string{
		jsonText(t, domain.RepoAnalysis{
			SchemaVersion: "harbor.repo_analysis.v1",
			RepoURL:       repoURL,
			CommitSHA:     commit,
			Language:      "go",
			BuildSystem:   "go modules",
			TestFramework: "go test",
		}),
		jsonText(t, domain.TaskProposal{
			SchemaVersion:         "harbor.task_proposal.v1",
			TaskName:              "codeedge/fix-config-loader",
			OneLineDescription:    "Fix config loader environment override handling.",
			CodeLang:              "go",
			TaskType:              "bug-fix",
			Application:           "backend",
			IsZeroToOne:           true,
			GitHubLink:            repoURL,
			CommitSHA:             commit,
			EstimatedAHTMinutes:   45,
			TargetFiles:           []string{"config.go"},
			DifficultyRationale:   "Requires understanding config precedence.",
			SuggestedVerification: "go test ./...",
		}),
		jsonText(t, domain.GeneratedTaskFiles{
			SchemaVersion: "harbor.generated_task_files.v1",
			InstructionMD: "Fix config loader environment overrides without breaking defaults.\n",
			SolveSH:       "cd /app/repo\nprintf fixed > config.go\n",
			TestSH:        "cd /app/repo\ntest -f config.go\ngrep -q package config.go\n",
			TestsAnalysis: validRunnerTestsAnalysis(),
		}),
	}}
	runner := NewRunner(RunnerOptions{
		RepoURL:        repoURL,
		Commit:         commit,
		AllowLocalRepo: true,
		Generate:       true,
		Workspace:      workspace,
		AutoApprove:    true,
		Agent:          agent,
	})
	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Passed {
		t.Fatalf("summary did not pass: %+v", summary)
	}
	if summary.GenReport == nil || summary.GenReport.TaskDir == "" {
		t.Fatalf("missing gen report: %+v", summary.GenReport)
	}
	if summary.LintReport == nil || !summary.LintReport.Passed {
		t.Fatalf("missing or failing lint report: %+v", summary.LintReport)
	}
	for _, rel := range []string{
		filepath.Join("phase1", "artifacts", "instruction_generate", "instruction.md"),
		filepath.Join("phase1", "artifacts", "task_toml_generate", "task.toml"),
		filepath.Join("phase1", "artifacts", "dockerfile_generate", "Dockerfile"),
		filepath.Join("phase2", "artifacts", "solve_generate", "solve.sh"),
		filepath.Join("phase2", "artifacts", "test_generate", "test.sh"),
		filepath.Join("phase3", "artifacts", "tests_analysis", "tests_analysis.md"),
		filepath.Join("phase1", "artifacts", "reviews", "task_review", "decision.json"),
		filepath.Join("phase1", "artifacts", "reviews", "content_review", "decision.json"),
		filepath.Join("phase2", "artifacts", "reviews", "final_review", "decision.json"),
	} {
		if _, err := os.Stat(filepath.Join(workspace, rel)); err != nil {
			t.Fatalf("missing workflow artifact %s: %v", rel, err)
		}
	}
	for _, nodeID := range []string{"repo_analyze", "task_design", "task_review", "instruction_generate", "task_toml_generate", "dockerfile_generate", "solve_generate", "test_generate", "tests_analysis", "codeedge_lint"} {
		if !eventSeen(summary.Events, nodeID, "succeeded") {
			t.Fatalf("events missing generated flow node %s: %+v", nodeID, summary.Events)
		}
	}
}

func TestRunnerRunsHarborAfterLintAndThenSubmissionLint(t *testing.T) {
	taskDir := writeRunnerTask(t)
	workspace := t.TempDir()
	qwenScreenshot, opusScreenshot := writeRunnerScreenshots(t)
	exec := &runnerHarborExec{outputs: []string{
		runnerTrialResultJSON("qwen3.7-max", []domain.TrialRun{
			{Trial: 1, Turns: 22, Reward: 0},
			{Trial: 2, Passed: true, Turns: 24, Reward: 1},
			{Trial: 3, Turns: 23, Reward: 0},
			{Trial: 4, Turns: 23, Reward: 0},
		}, ""),
		runnerTrialResultJSON("claude-opus-4-8", []domain.TrialRun{
			{Trial: 1, Passed: true, Turns: 28, Reward: 1},
			{Trial: 2, Passed: true, Turns: 29, Reward: 1},
			{Trial: 3, Passed: true, Turns: 27, Reward: 1},
			{Trial: 4, Turns: 28, Reward: 0},
		}, ""),
	}}
	runner := NewRunner(RunnerOptions{
		TaskDir:           taskDir,
		Workspace:         workspace,
		AutoApprove:       true,
		RunHarbor:         true,
		HarborAgentEnv:    []string{"ANTHROPIC_AUTH_TOKEN=test-token"},
		QwenHarborBaseURL: "https://qwen.example/v1",
		OpusHarborBaseURL: "https://opus.example/v1",
		HarborExec:        exec,
		QwenScreenshot:    qwenScreenshot,
		OpusScreenshot:    opusScreenshot,
	})
	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Passed {
		t.Fatalf("summary did not pass: %+v", summary)
	}
	if summary.QwenResult == nil || summary.QwenResult.PassCount != 1 || summary.QwenResult.AverageTurns != 23 {
		t.Fatalf("unexpected qwen result: %+v", summary.QwenResult)
	}
	if summary.OpusResult == nil || summary.OpusResult.AverageTurns != 28 {
		t.Fatalf("unexpected opus result: %+v", summary.OpusResult)
	}
	if filepath.Base(summary.QwenResult.ResultPath) != "qwen_result.json" || filepath.Base(summary.OpusResult.ResultPath) != "opus_result.json" {
		t.Fatalf("harbor result aliases not used: qwen=%s opus=%s", summary.QwenResult.ResultPath, summary.OpusResult.ResultPath)
	}
	if len(exec.commands) != 2 || !strings.Contains(exec.commands[0], "ANTHROPIC_BASE_URL=https://qwen.example/v1") || strings.Contains(exec.commands[0], "opus.example") || !strings.Contains(exec.commands[1], "ANTHROPIC_BASE_URL=https://opus.example/v1") || strings.Contains(exec.commands[1], "qwen.example") {
		t.Fatalf("per-model Harbor routes were not isolated: %v", exec.commands)
	}
	if !strings.Contains(exec.commands[0], "CLAUDE_CODE_SUBAGENT_MODEL=qwen3.7-max") || !strings.Contains(exec.commands[0], "ANTHROPIC_DEFAULT_OPUS_MODEL=qwen3.7-max") || !strings.Contains(exec.commands[1], "CLAUDE_CODE_SUBAGENT_MODEL=claude-opus-4-8") || !strings.Contains(exec.commands[1], "ANTHROPIC_DEFAULT_OPUS_MODEL=claude-opus-4-8") {
		t.Fatalf("nested Claude model routes were not pinned to their stages: %v", exec.commands)
	}
	for _, rel := range []string{
		filepath.Join("phase3", "artifacts", "harbor_run_qwen", "qwen_result.json"),
		filepath.Join("phase3", "artifacts", "harbor_run_qwen", "trial_result.json"),
		filepath.Join("phase3", "artifacts", "harbor_run_opus", "opus_result.json"),
		filepath.Join("phase3", "artifacts", "harbor_run_opus", "trial_result.json"),
		filepath.Join("phase3", "artifacts", "reviews", "result_review", "decision.json"),
	} {
		if _, err := os.Stat(filepath.Join(workspace, rel)); err != nil {
			t.Fatalf("missing harbor run result %s: %v", rel, err)
		}
	}
	if !eventSeen(summary.Events, "harbor_run_qwen", "succeeded") || !eventSeen(summary.Events, "harbor_run_opus", "succeeded") || !eventSeen(summary.Events, "submission_lint", "succeeded") || !eventSeen(summary.Events, "result_review", "succeeded") {
		t.Fatalf("events missing harbor run nodes: %+v", summary.Events)
	}
	if !eventMessageSeen(summary.Events, nodes.HarborRunQwen, "trial started") {
		t.Fatalf("Harbor live progress was not emitted: %+v", summary.Events)
	}
	if eventIndex(summary.Events, "codeedge_lint", "succeeded") > eventIndex(summary.Events, "harbor_run_qwen", "running") {
		t.Fatalf("harbor started before lint passed: %+v", summary.Events)
	}
	if eventIndex(summary.Events, "submission_lint", "succeeded") < eventIndex(summary.Events, "harbor_run_opus", "succeeded") {
		t.Fatalf("submission lint should run after harbor results: %+v", summary.Events)
	}
	if eventIndex(summary.Events, "result_review", "succeeded") < eventIndex(summary.Events, "submission_lint", "succeeded") {
		t.Fatalf("result review should run after submission lint: %+v", summary.Events)
	}
}

func TestRunnerResumeDiscoversCompletedQwenAndRunsOnlyMissingOpus(t *testing.T) {
	taskDir := writeRunnerTask(t)
	workspace := t.TempDir()
	qwenScreenshot, opusScreenshot := writeRunnerScreenshots(t)
	digest, err := harborrun.ComputeTaskDigest(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	qwenPath := nodes.QwenResultPath(workspace)
	if err := os.MkdirAll(filepath.Dir(qwenPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeRunnerTrialResult(t, qwenPath, taskDir, "qwen3.7-max", []domain.TrialRun{
		{Trial: 1, Turns: 22, Reward: 0},
		{Trial: 2, Passed: true, Turns: 24, Reward: 1},
		{Trial: 3, Turns: 23, Reward: 0},
		{Trial: 4, Turns: 23, Reward: 0},
	}, digest)
	exec := &runnerHarborExec{outputs: []string{
		runnerTrialResultJSON("claude-opus-4-8", []domain.TrialRun{
			{Trial: 1, Passed: true, Turns: 28, Reward: 1},
			{Trial: 2, Passed: true, Turns: 29, Reward: 1},
			{Trial: 3, Passed: true, Turns: 27, Reward: 1},
			{Trial: 4, Turns: 28, Reward: 0},
		}, ""),
	}}
	runner := NewRunner(RunnerOptions{
		TaskDir:        taskDir,
		Workspace:      workspace,
		AutoApprove:    true,
		RunHarbor:      true,
		HarborAgentEnv: []string{"ANTHROPIC_AUTH_TOKEN=test-token"},
		HarborExec:     exec,
		QwenScreenshot: qwenScreenshot,
		OpusScreenshot: opusScreenshot,
	})
	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Passed {
		t.Fatalf("summary did not pass: %+v", summary)
	}
	if exec.counter != 1 {
		t.Fatalf("expected only missing Opus Harbor run to execute, got %d executions", exec.counter)
	}
	if summary.QwenResult == nil || summary.QwenResult.ResultPath != qwenPath {
		t.Fatalf("existing qwen result was not loaded from workspace: %+v", summary.QwenResult)
	}
	if summary.OpusResult == nil || filepath.Base(summary.OpusResult.ResultPath) != "opus_result.json" {
		t.Fatalf("missing opus result was not generated: %+v", summary.OpusResult)
	}
	if eventSeen(summary.Events, nodes.HarborRunQwen, "running") {
		t.Fatalf("qwen should not be re-run when workspace evidence exists: %+v", summary.Events)
	}
	if !eventSeen(summary.Events, nodes.HarborRunQwen, "succeeded") || !eventSeen(summary.Events, nodes.HarborRunOpus, "succeeded") {
		t.Fatalf("expected loaded qwen and generated opus success events: %+v", summary.Events)
	}
}

func TestApplyWorkspaceEvidenceDefaultsUsesResultScreenshotFallback(t *testing.T) {
	workspace := t.TempDir()
	resultPath := nodes.QwenResultPath(workspace)
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
		t.Fatal(err)
	}
	screenshot := filepath.Join(filepath.Dir(resultPath), "qwen.png")
	if err := os.WriteFile(screenshot, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := domain.TrialResult{
		SchemaVersion: "harbor.trial_result.v1",
		Model:         "qwen3.7-max",
		Screenshot:    "qwen.png",
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	var testsAnalysis, qwenResult, opusResult, qwenShot, opusShot string
	applyWorkspaceEvidenceDefaults(workspace, &testsAnalysis, &qwenResult, &opusResult, &qwenShot, &opusShot)
	if qwenResult != resultPath || qwenShot != screenshot {
		t.Fatalf("workspace evidence defaults did not use result screenshot fallback: result=%q screenshot=%q", qwenResult, qwenShot)
	}
}

func TestRunnerReviewsProvidedHarborResults(t *testing.T) {
	taskDir := writeRunnerTask(t)
	workspace := t.TempDir()
	resultDir := t.TempDir()
	qwenScreenshot, opusScreenshot := writeRunnerScreenshots(t)
	digest, err := harborrun.ComputeTaskDigest(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	qwenPath := filepath.Join(resultDir, "qwen_result.json")
	opusPath := filepath.Join(resultDir, "opus_result.json")
	writeRunnerTrialResult(t, qwenPath, taskDir, "qwen3.7-max", []domain.TrialRun{
		{Trial: 1, Turns: 22, Reward: 0},
		{Trial: 2, Passed: true, Turns: 24, Reward: 1},
		{Trial: 3, Turns: 23, Reward: 0},
		{Trial: 4, Turns: 23, Reward: 0},
	}, digest)
	writeRunnerTrialResult(t, opusPath, taskDir, "claude-opus-4-8", []domain.TrialRun{
		{Trial: 1, Passed: true, Turns: 28, Reward: 1},
		{Trial: 2, Passed: true, Turns: 29, Reward: 1},
		{Trial: 3, Passed: true, Turns: 27, Reward: 1},
		{Trial: 4, Turns: 28, Reward: 0},
	}, digest)
	runner := NewRunner(RunnerOptions{
		TaskDir:        taskDir,
		Workspace:      workspace,
		AutoApprove:    true,
		QwenResult:     qwenPath,
		OpusResult:     opusPath,
		QwenScreenshot: qwenScreenshot,
		OpusScreenshot: opusScreenshot,
	})
	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Passed {
		t.Fatalf("summary did not pass: %+v", summary)
	}
	if summary.QwenResult == nil || summary.QwenResult.ResultPath != qwenPath || summary.OpusResult == nil || summary.OpusResult.ResultPath != opusPath {
		t.Fatalf("provided results not loaded: qwen=%+v opus=%+v", summary.QwenResult, summary.OpusResult)
	}
	if !eventSeen(summary.Events, "result_review", "succeeded") {
		t.Fatalf("result review did not run for provided results: %+v", summary.Events)
	}
	if _, err := os.Stat(filepath.Join(workspace, "phase3", "artifacts", "reviews", "result_review", "decision.json")); err != nil {
		t.Fatalf("missing result review decision: %v", err)
	}
}

func TestRunnerRejectsProvidedHarborResultWithoutCommandEvidence(t *testing.T) {
	taskDir := writeRunnerTask(t)
	workspace := t.TempDir()
	resultDir := t.TempDir()
	qwenScreenshot, opusScreenshot := writeRunnerScreenshots(t)
	digest, err := harborrun.ComputeTaskDigest(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	qwenPath := filepath.Join(resultDir, "qwen_result.json")
	opusPath := filepath.Join(resultDir, "opus_result.json")
	if err := os.WriteFile(qwenPath, []byte(runnerTrialResultJSON("qwen3.7-max", []domain.TrialRun{
		{Trial: 1, Turns: 22, Reward: 0},
		{Trial: 2, Passed: true, Turns: 24, Reward: 1},
		{Trial: 3, Turns: 23, Reward: 0},
		{Trial: 4, Turns: 23, Reward: 0},
	}, digest)), 0o644); err != nil {
		t.Fatal(err)
	}
	writeRunnerTrialResult(t, opusPath, taskDir, "claude-opus-4-8", []domain.TrialRun{
		{Trial: 1, Passed: true, Turns: 28, Reward: 1},
		{Trial: 2, Passed: true, Turns: 29, Reward: 1},
		{Trial: 3, Passed: true, Turns: 27, Reward: 1},
		{Trial: 4, Turns: 28, Reward: 0},
	}, digest)
	runner := NewRunner(RunnerOptions{
		TaskDir:        taskDir,
		Workspace:      workspace,
		AutoApprove:    true,
		QwenResult:     qwenPath,
		OpusResult:     opusPath,
		QwenScreenshot: qwenScreenshot,
		OpusScreenshot: opusScreenshot,
	})
	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Passed {
		t.Fatalf("expected provided result without command evidence to fail, got %+v", summary)
	}
	if !eventSeen(summary.Events, nodes.HarborRunQwen, "failed") {
		t.Fatalf("missing qwen evidence failure event: %+v", summary.Events)
	}
	if eventSeen(summary.Events, nodes.ResultReview, "succeeded") {
		t.Fatalf("result review should not succeed after strict evidence failure: %+v", summary.Events)
	}
}

func TestResultChecklistRequiresCommandRunEvidence(t *testing.T) {
	taskDir := writeRunnerTask(t)
	digest, err := harborrun.ComputeTaskDigest(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	var result domain.TrialResult
	if err := json.Unmarshal([]byte(runnerTrialResultJSON("qwen3.7-max", []domain.TrialRun{
		{Trial: 1, Turns: 22, Reward: 0},
		{Trial: 2, Passed: true, Turns: 24, Reward: 1},
		{Trial: 3, Turns: 23, Reward: 0},
		{Trial: 4, Turns: 23, Reward: 0},
	}, digest)), &result); err != nil {
		t.Fatal(err)
	}
	items := trialResultChecklist("qwen", "Qwen", &result, harborResultValidationOptions(taskDir, "qwen3.7-max", "claude-code", true))
	var found bool
	for _, item := range items {
		if strings.Contains(item.Label, "command_run_path is required") && item.Critical && !item.Passed {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected command_run_path critical failure, got %+v", items)
	}
}

func TestRunnerQualityCheckPersistsReport(t *testing.T) {
	taskDir := writeRunnerTask(t)
	analysis := filepath.Join(taskDir, "tests_analysis.md")
	if err := os.WriteFile(analysis, []byte(validRunnerTestsAnalysis()), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	runner := NewRunner(RunnerOptions{
		TaskDir:       taskDir,
		Workspace:     workspace,
		TestsAnalysis: analysis,
		AutoApprove:   true,
		QualityCheck:  true,
	})
	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Passed {
		t.Fatalf("summary did not pass: %+v", summary)
	}
	if summary.QualityReport == nil || !summary.QualityReport.OverallPass {
		t.Fatalf("unexpected quality report: %+v", summary.QualityReport)
	}
	if summary.QualityReport.TaskDigest == "" {
		t.Fatal("quality report must include the reviewed task digest")
	}
	qualityPath := filepath.Join(workspace, "phase2", "artifacts", "quality_check", "quality_report.json")
	if _, err := os.Stat(qualityPath); err != nil {
		t.Fatalf("missing quality report: %v", err)
	}
	if !eventSeen(summary.Events, "quality_check", "succeeded") {
		t.Fatalf("quality_check event missing: %+v", summary.Events)
	}
}

func TestLoadReusableQualityReportRequiresCurrentTaskDigest(t *testing.T) {
	taskDir := writeRunnerTask(t)
	digest, err := harborrun.ComputeTaskDigest(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(t.TempDir(), "quality_report.json")
	writeReport := func(taskDigest string) {
		t.Helper()
		report := domain.QualityReport{
			SchemaVersion: "harbor.quality_report.v1",
			TaskDir:       taskDir,
			TaskDigest:    taskDigest,
			Checks:        map[string]domain.QualityCheck{},
			OverallPass:   true,
			CreatedAt:     time.Now().UTC(),
		}
		raw, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(reportPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeReport("")
	if _, ok := loadReusableQualityReport(taskDir, reportPath); ok {
		t.Fatal("legacy quality report without task_digest must not be reused")
	}

	writeReport(digest)
	if _, ok := loadReusableQualityReport(taskDir, reportPath); !ok {
		t.Fatal("quality report with the current task digest should be reusable")
	}

	if err := os.WriteFile(filepath.Join(taskDir, "instruction.md"), []byte("changed task instruction\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadReusableQualityReport(taskDir, reportPath); ok {
		t.Fatal("quality report must not be reused after task content changes")
	}
}

func TestRunnerReusesExistingVerifyReportWhenDigestMatches(t *testing.T) {
	taskDir := writeRunnerTask(t)
	workspace := t.TempDir()
	verifyExec := &runnerResultExec{results: []executor.Result{
		{Command: "docker build", ExitCode: 0},
		{Command: "docker run initial", ExitCode: 1, Err: os.ErrInvalid},
		{Command: "docker run oracle", ExitCode: 0},
	}}
	first := NewRunner(RunnerOptions{
		TaskDir:      taskDir,
		Workspace:    workspace,
		AutoApprove:  true,
		VerifyDocker: true,
		VerifyExec:   verifyExec,
	})
	firstSummary, err := first.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !firstSummary.Passed || firstSummary.VerifyReport == nil {
		t.Fatalf("initial verify run did not pass: %+v", firstSummary)
	}

	second := NewRunner(RunnerOptions{
		TaskDir:      taskDir,
		Workspace:    workspace,
		AutoApprove:  true,
		VerifyDocker: true,
		VerifyExec:   &runnerResultExec{},
	})
	summary, err := second.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Passed || summary.VerifyReport == nil || summary.VerifyReport.TaskDigest != firstSummary.VerifyReport.TaskDigest {
		t.Fatalf("reused verify run did not pass: %+v", summary)
	}
	if eventSeen(summary.Events, nodes.HarborVerify, "running") {
		t.Fatalf("verify should not re-run when reusable report exists: %+v", summary.Events)
	}
	if !eventMessageSeen(summary.Events, nodes.HarborVerify, "reused existing Docker/oracle verification report") {
		t.Fatalf("verify reuse event missing: %+v", summary.Events)
	}
}

func TestRunnerSimilarityCheckPersistsReport(t *testing.T) {
	taskDir := writeRunnerTask(t)
	workspace := t.TempDir()
	runner := NewRunner(RunnerOptions{
		TaskDir:         taskDir,
		Workspace:       workspace,
		AutoApprove:     true,
		SimilarityCheck: true,
	})
	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Passed {
		t.Fatalf("summary did not pass: %+v", summary)
	}
	if summary.SimilarityReport == nil || !summary.SimilarityReport.OverallPass {
		t.Fatalf("unexpected similarity report: %+v", summary.SimilarityReport)
	}
	reportPath := filepath.Join(workspace, "phase2", "artifacts", "similarity_check", "similarity_report.json")
	if _, err := os.Stat(reportPath); err != nil {
		t.Fatalf("missing similarity report: %v", err)
	}
	if !eventSeen(summary.Events, "similarity_check", "succeeded") {
		t.Fatalf("similarity_check event missing: %+v", summary.Events)
	}
}

func TestRunnerReusesExistingSimilarityReportWhenDigestMatches(t *testing.T) {
	taskDir := writeRunnerTask(t)
	workspace := t.TempDir()
	history := t.TempDir()
	if err := os.WriteFile(filepath.Join(history, "unrelated.md"), []byte("Unrelated desktop clipboard notes and image export settings."), 0o644); err != nil {
		t.Fatal(err)
	}
	first := NewRunner(RunnerOptions{
		TaskDir:               taskDir,
		Workspace:             workspace,
		AutoApprove:           true,
		SimilarityCheck:       true,
		SimilarityHistoryDirs: []string{history},
	})
	firstSummary, err := first.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !firstSummary.Passed || firstSummary.SimilarityReport == nil || len(firstSummary.SimilarityReport.SuccessfulSources) == 0 {
		t.Fatalf("initial similarity run did not produce reusable report: %+v", firstSummary)
	}

	second := NewRunner(RunnerOptions{
		TaskDir:         taskDir,
		Workspace:       workspace,
		AutoApprove:     true,
		SimilarityCheck: true,
	})
	summary, err := second.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Passed || summary.SimilarityReport == nil || summary.SimilarityReport.TaskDigest != firstSummary.SimilarityReport.TaskDigest {
		t.Fatalf("reused similarity run did not pass: %+v", summary)
	}
	if eventSeen(summary.Events, nodes.SimilarityCheck, "running") {
		t.Fatalf("similarity should not re-run when reusable report exists: %+v", summary.Events)
	}
	if !eventMessageSeen(summary.Events, nodes.SimilarityCheck, "reused existing similarity report") {
		t.Fatalf("similarity reuse event missing: %+v", summary.Events)
	}
}

func TestRunnerSimilarityCheckFailsOnHistoryDuplicate(t *testing.T) {
	taskDir := writeRunnerTask(t)
	history := t.TempDir()
	if err := os.WriteFile(filepath.Join(history, "old.md"), []byte("Fix the task. name sample fixed solve test instruction Harbor task."), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(RunnerOptions{
		TaskDir:               taskDir,
		Workspace:             t.TempDir(),
		AutoApprove:           true,
		SimilarityCheck:       true,
		SimilarityHistoryDirs: []string{history},
		SimilarityThreshold:   0.01,
	})
	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Passed || summary.SimilarityReport == nil || summary.SimilarityReport.OverallPass {
		t.Fatalf("expected similarity failure, got summary=%+v", summary)
	}
	if !eventSeen(summary.Events, "similarity_check", "failed") {
		t.Fatalf("similarity_check failure event missing: %+v", summary.Events)
	}
}

func TestRunnerPackageForcesVerifyHarborAndSubmissionLint(t *testing.T) {
	taskDir := writeRunnerTask(t)
	workspace := t.TempDir()
	outputDir := t.TempDir()
	analysis := filepath.Join(taskDir, "tests_analysis.md")
	if err := os.WriteFile(analysis, []byte(validRunnerTestsAnalysis()), 0o644); err != nil {
		t.Fatal(err)
	}
	qwenScreenshot := filepath.Join(t.TempDir(), "qwen.png")
	opusScreenshot := filepath.Join(t.TempDir(), "opus.png")
	if err := os.WriteFile(qwenScreenshot, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opusScreenshot, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	history := t.TempDir()
	if err := os.WriteFile(filepath.Join(history, "unrelated.md"), []byte("Legacy desktop widget telemetry notes with unrelated installation wording and no matching verifier behavior."), 0o644); err != nil {
		t.Fatal(err)
	}
	writeRunnerTaskTOML(t, taskDir)
	verifyExec := &runnerResultExec{results: []executor.Result{
		{Command: "docker build", ExitCode: 0},
		{Command: "docker run initial", ExitCode: 1, Err: os.ErrInvalid},
		{Command: "docker run oracle", ExitCode: 0},
	}}
	harborExec := &runnerHarborExec{outputs: []string{
		runnerTrialResultJSON("qwen3.7-max", []domain.TrialRun{
			{Trial: 1, Turns: 22, Reward: 0},
			{Trial: 2, Passed: true, Turns: 24, Reward: 1},
			{Trial: 3, Turns: 23, Reward: 0},
			{Trial: 4, Turns: 23, Reward: 0},
		}, ""),
		runnerTrialResultJSON("claude-opus-4-8", []domain.TrialRun{
			{Trial: 1, Passed: true, Turns: 28, Reward: 1},
			{Trial: 2, Passed: true, Turns: 29, Reward: 1},
			{Trial: 3, Passed: true, Turns: 27, Reward: 1},
			{Trial: 4, Turns: 28, Reward: 0},
		}, ""),
	}}
	runner := NewRunner(RunnerOptions{
		TaskDir:       taskDir,
		Workspace:     workspace,
		AutoApprove:   true,
		Package:       true,
		OutputDir:     outputDir,
		RepoURL:       "https://github.com/org/repo",
		Commit:        "abc1234",
		TestsAnalysis: analysis,
		TaskName:      "sample-task",
		CodeLang:      "go",
		TaskType:      "bug-fix",
		Application:   "backend",
		AHT:           "45 minutes",
		Description:   "sample description",
		SimilarityHistoryDirs: []string{
			history,
		},
		QwenScreenshot: qwenScreenshot,
		OpusScreenshot: opusScreenshot,
		VerifyExec:     verifyExec,
		HarborExec:     harborExec,
	})
	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Passed || summary.VerifyReport == nil || summary.QwenResult == nil || summary.OpusResult == nil || summary.PackageReport == nil {
		t.Fatalf("expected complete package run, got %+v", summary)
	}
	if !strings.Contains(summary.VerifyReport.ImageTag, summary.RunID) || !strings.Contains(summary.VerifyReport.ImageTag, nodes.HarborVerify) {
		t.Fatalf("verify image tag should include run and node identity: run=%s tag=%s", summary.RunID, summary.VerifyReport.ImageTag)
	}
	if _, err := os.Stat(summary.PackageReport.OutputZip); err != nil {
		t.Fatalf("missing package zip: %v", err)
	}
	for _, nodeID := range []string{
		nodes.HarborVerify,
		nodes.DockerBuild,
		nodes.InitialVerify,
		nodes.OracleVerify,
		nodes.SubmissionLint,
		nodes.SimilarityCheck,
		nodes.ResultReview,
		nodes.Package,
	} {
		if !eventSeen(summary.Events, nodeID, "succeeded") {
			t.Fatalf("package run missing forced node %s: %+v", nodeID, summary.Events)
		}
	}
	for _, rel := range []string{
		"phase2/artifacts/docker_build/build_result.json",
		"phase2/artifacts/initial_verify/initial_result.json",
		"phase2/artifacts/oracle_verify/oracle_result.json",
	} {
		if _, err := os.Stat(filepath.Join(workspace, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("missing split verify artifact %s: %v", rel, err)
		}
	}
	if eventIndex(summary.Events, "harbor_verify", "succeeded") > eventIndex(summary.Events, "harbor_run_qwen", "running") {
		t.Fatalf("harbor started before verify passed: %+v", summary.Events)
	}
}

func TestRunnerPackageFailsWhenSimilaritySourceUnreadable(t *testing.T) {
	taskDir := writeRunnerTask(t)
	outputDir := t.TempDir()
	analysis := filepath.Join(taskDir, "tests_analysis.md")
	if err := os.WriteFile(analysis, []byte(validRunnerTestsAnalysis()), 0o644); err != nil {
		t.Fatal(err)
	}
	verifyExec := &runnerResultExec{results: []executor.Result{
		{Command: "docker build", ExitCode: 0},
		{Command: "docker run initial", ExitCode: 1, Err: os.ErrInvalid},
		{Command: "docker run oracle", ExitCode: 0},
	}}
	missingHistory := filepath.Join(t.TempDir(), "missing-history")
	runner := NewRunner(RunnerOptions{
		TaskDir:               taskDir,
		Workspace:             t.TempDir(),
		AutoApprove:           true,
		Package:               true,
		OutputDir:             outputDir,
		RepoURL:               "https://github.com/org/repo",
		Commit:                "abc1234",
		TestsAnalysis:         analysis,
		SimilarityHistoryDirs: []string{missingHistory},
		VerifyExec:            verifyExec,
	})
	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Passed || summary.SimilarityReport == nil || summary.SimilarityReport.OverallPass {
		t.Fatalf("expected similarity failure, got %+v", summary)
	}
	if summary.PackageReport != nil {
		t.Fatalf("package should not run after similarity failure: %+v", summary.PackageReport)
	}
	if !eventSeen(summary.Events, nodes.SimilarityCheck, "failed") {
		t.Fatalf("similarity_check failure event missing: %+v", summary.Events)
	}
	if eventSeen(summary.Events, nodes.Package, "succeeded") {
		t.Fatalf("package should not succeed after similarity failure: %+v", summary.Events)
	}
}

func TestRunnerPackageUsesDefaultWorkspaceForEvidenceReports(t *testing.T) {
	t.Chdir(t.TempDir())
	taskDir := writeRunnerTask(t)
	outputDir := t.TempDir()
	analysis := filepath.Join(taskDir, "tests_analysis.md")
	if err := os.WriteFile(analysis, []byte(validRunnerTestsAnalysis()), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := harborrun.ComputeTaskDigest(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	qwenResult := filepath.Join(outputDir, "qwen.json")
	opusResult := filepath.Join(outputDir, "opus.json")
	writeRunnerTrialResult(t, qwenResult, taskDir, "qwen3.7-max", []domain.TrialRun{
		{Trial: 1, Turns: 22, Reward: 0},
		{Trial: 2, Passed: true, Turns: 24, Reward: 1},
		{Trial: 3, Turns: 23, Reward: 0},
		{Trial: 4, Turns: 23, Reward: 0},
	}, digest)
	writeRunnerTrialResult(t, opusResult, taskDir, "claude-opus-4-8", []domain.TrialRun{
		{Trial: 1, Passed: true, Turns: 28, Reward: 1},
		{Trial: 2, Passed: true, Turns: 29, Reward: 1},
		{Trial: 3, Passed: true, Turns: 27, Reward: 1},
		{Trial: 4, Turns: 28, Reward: 0},
	}, digest)
	qwenScreenshot := filepath.Join(outputDir, "qwen.png")
	opusScreenshot := filepath.Join(outputDir, "opus.png")
	if err := os.WriteFile(qwenScreenshot, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opusScreenshot, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	history := t.TempDir()
	if err := os.WriteFile(filepath.Join(history, "unrelated.md"), []byte("Unrelated notes about desktop clipboard rendering and theme preferences."), 0o644); err != nil {
		t.Fatal(err)
	}
	verifyExec := &runnerResultExec{results: []executor.Result{
		{Command: "docker build", ExitCode: 0},
		{Command: "docker run initial", ExitCode: 1, Err: os.ErrInvalid},
		{Command: "docker run oracle", ExitCode: 0},
	}}
	runner := NewRunner(RunnerOptions{
		TaskDir:               taskDir,
		AutoApprove:           true,
		Package:               true,
		OutputDir:             outputDir,
		TestsAnalysis:         analysis,
		QwenResult:            qwenResult,
		OpusResult:            opusResult,
		SimilarityHistoryDirs: []string{history},
		QwenScreenshot:        qwenScreenshot,
		OpusScreenshot:        opusScreenshot,
		VerifyExec:            verifyExec,
	})
	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Passed || summary.PackageReport == nil {
		t.Fatalf("expected package success with default workspace, got %+v", summary)
	}
	for _, path := range []string{nodes.VerifyReportPath(""), nodes.SimilarityReportPath("")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing default workspace artifact %s: %v", path, err)
		}
	}
	raw, err := os.ReadFile(summary.PackageReport.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	var submission map[string]any
	if err := json.Unmarshal(raw, &submission); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"task_name":            "sample",
		"code_lang":            "go",
		"task_type":            "bug-fix",
		"application":          "backend",
		"aht":                  "45 minutes",
		"one_line_description": "sample task",
		"github_url":           "https://github.com/org/repo",
		"commit_id":            "abc1234",
	} {
		if submission[key] != want {
			t.Fatalf("submission %s = %v, want %s; full=%+v", key, submission[key], want, submission)
		}
	}
}

func writeRunnerTask(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"environment", "solution", "tests"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, content string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("instruction.md", "Fix the task.\n", 0o644)
	writeRunnerTaskTOML(t, root)
	write(filepath.Join("environment", "Dockerfile"), "FROM alpine\nRUN git clone https://github.com/org/repo /src && cd /src && git checkout abc1234\n", 0o644)
	write(filepath.Join("solution", "solve.sh"), "#!/bin/sh\nset -eu\nprintf 'package config\\n' > config.go\n", 0o755)
	write(filepath.Join("tests", "test.sh"), "#!/bin/sh\nset -eu\ngrep -q 'package config' config.go\n", 0o755)
	write("tests_analysis.md", validRunnerTestsAnalysis(), 0o644)
	return root
}

func validRunnerTestsAnalysis() string {
	return `## 1. instruction 和 environment 已提供的信息
- instruction 要求修复 config loader 的环境变量覆盖行为，environment 固定公开仓库 commit 并提供容器运行入口。
- 可见约束包括保留默认配置、不读取 solution/tests，并只依据 instruction、源码和 environment 判断任务边界。

---

## 2. 模型的理论通过路径
- 模型阅读 instruction 与 config.go 相关代码，定位配置优先级问题并实现最小修复。
- 修复后运行 tests/test.sh 或等价 verifier，确认 config.go 内容和可观察行为满足任务描述。

---

## 3. 模型具备通过条件的依据
- verifier 检查点可从 instruction 和 environment 推导，tests/test.sh 只验证公开配置行为。
- 通过条件不依赖隐藏业务要求、私有凭证或 reward 文件，模型无需读取测试源码才能理解目标。
`
}

func writeRunnerTaskTOML(t *testing.T, taskDir string) {
	t.Helper()
	content := `schema_version = "1.3"

[task]
name = "codeedge/sample"
description = "sample task"

[metadata]
code_lang = "go"
task_type = "bug-fix"
application = "backend"
is_0_to_1 = false
github_url = "https://github.com/org/repo"
commit_id = "abc1234"
estimated_aht_minutes = 45
difficulty_explanation = "Requires understanding repository behavior."
target_files = ["config.go"]

[verifier]
timeout_sec = 600.0

[agent]
timeout_sec = 1800.0

[environment]
build_timeout_sec = 600.0
network_mode = "public"
os = "linux"
`
	if err := os.WriteFile(filepath.Join(taskDir, "task.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runnerTrialResultJSON(model string, runs []domain.TrialRun, taskDigest string) string {
	passCount := 0
	totalTurns := 0
	for _, run := range runs {
		if run.Passed {
			passCount++
		}
		totalTurns += run.Turns
	}
	average := 0.0
	if len(runs) > 0 {
		average = float64(totalTurns) / float64(len(runs))
	}
	result := domain.TrialResult{
		SchemaVersion: "harbor.trial_result.v1",
		Model:         model,
		Trials:        len(runs),
		PassCount:     passCount,
		PassAt4:       float64(passCount) / float64(len(runs)),
		AverageTurns:  average,
		Runs:          runs,
		TaskDigest:    taskDigest,
	}
	raw, err := json.Marshal(result)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func writeRunnerTrialResult(t *testing.T, path, taskDir, model string, runs []domain.TrialRun, taskDigest string) {
	t.Helper()
	commandPath := strings.TrimSuffix(path, filepath.Ext(path)) + "_command_run.json"
	stdoutPath := strings.TrimSuffix(path, filepath.Ext(path)) + "_stdout.txt"
	stderrPath := strings.TrimSuffix(path, filepath.Ext(path)) + "_stderr.txt"
	rawResultPath := strings.TrimSuffix(path, filepath.Ext(path)) + "_raw_result.json"
	rawResult := []byte(`{"schema_version":"harbor.raw.fixture.v1"}`)
	if err := os.WriteFile(rawResultPath, rawResult, 0o644); err != nil {
		t.Fatal(err)
	}
	commandRun := domain.CommandRun{
		Name:       "harbor_run_" + strings.NewReplacer("/", "_", ":", "_").Replace(model),
		Argv:       []string{"harbor", "run", "-p", taskDir, "-a", "claude-code", "-m", model, "-n", "4", "-k", "4"},
		ExitCode:   0,
		Stdout:     "Raw result evidence: " + rawResultPath + "\n",
		Stderr:     "",
		StdoutPath: stdoutPath,
		StderrPath: stderrPath,
		Passed:     true,
	}
	if err := os.WriteFile(stdoutPath, []byte(commandRun.Stdout), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderrPath, []byte(commandRun.Stderr), 0o644); err != nil {
		t.Fatal(err)
	}
	commandRaw, err := json.Marshal(commandRun)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(commandPath, commandRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	var result domain.TrialResult
	if err := json.Unmarshal([]byte(runnerTrialResultJSON(model, runs, taskDigest)), &result); err != nil {
		t.Fatal(err)
	}
	result.Agent = "claude-code"
	result.CommandRunPath = commandPath
	result.RawResultPath = rawResultPath
	result.RawResultSHA256 = runnerSHA256(rawResult)
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRunnerScreenshots(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	qwen := filepath.Join(dir, "qwen.png")
	opus := filepath.Join(dir, "opus.png")
	if err := os.WriteFile(qwen, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opus, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	return qwen, opus
}

type runnerResultExec struct {
	results  []executor.Result
	commands []string
}

func (e *runnerResultExec) LookPath(name string) (string, error) {
	return name, nil
}

func (e *runnerResultExec) Run(_ context.Context, _ time.Duration, _ string, _ []string, name string, args ...string) executor.Result {
	e.commands = append(e.commands, name+" "+strings.Join(args, " "))
	if len(e.results) == 0 {
		return executor.Result{Command: name, Err: os.ErrInvalid}
	}
	result := e.results[0]
	e.results = e.results[1:]
	if result.Command == "" {
		result.Command = name + " " + strings.Join(args, " ")
	}
	return result
}

func (e *runnerResultExec) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, _ io.Writer, _ executor.OutputCallback, name string, args ...string) executor.Result {
	return e.Run(ctx, timeout, dir, env, name, args...)
}

type runnerFakeAgent struct {
	outputs []string
}

func (f *runnerFakeAgent) Turn(_ context.Context, req workflow.AgentTurnRequest) (workflow.AgentTurnResult, error) {
	if len(f.outputs) == 0 {
		return workflow.AgentTurnResult{}, os.ErrInvalid
	}
	out := f.outputs[0]
	f.outputs = f.outputs[1:]
	return workflow.AgentTurnResult{Text: out, Model: req.Model}, nil
}

type runnerHarborExec struct {
	outputs  []string
	counter  int
	commands []string
}

type runnerCancelableHarborExec struct {
	calls    int
	delegate runnerHarborExec
}

type runnerFailFirstHarborExec struct {
	calls    int
	delegate runnerHarborExec
}

func (e *runnerFailFirstHarborExec) LookPath(name string) (string, error) {
	return name, nil
}

func (e *runnerFailFirstHarborExec) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
	return e.RunStreamingWithOutput(ctx, timeout, dir, env, io.Discard, nil, name, args...)
}

func (e *runnerFailFirstHarborExec) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, _ io.Writer, callback executor.OutputCallback, name string, args ...string) executor.Result {
	if e.calls == 0 {
		e.calls++
		return executor.Result{Command: name + " " + strings.Join(args, " "), ExitCode: 35, Err: errors.New("transient Qwen failure")}
	}
	e.calls++
	return e.delegate.RunStreamingWithOutput(ctx, timeout, dir, env, io.Discard, callback, name, args...)
}

func (e *runnerCancelableHarborExec) LookPath(name string) (string, error) {
	return name, nil
}

func (e *runnerCancelableHarborExec) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
	return e.RunStreamingWithOutput(ctx, timeout, dir, env, io.Discard, nil, name, args...)
}

func (e *runnerCancelableHarborExec) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, _ io.Writer, callback executor.OutputCallback, name string, args ...string) executor.Result {
	if e.calls == 0 {
		e.calls++
		<-ctx.Done()
		return executor.Result{Command: name + " " + strings.Join(args, " "), ExitCode: -1, Err: ctx.Err()}
	}
	e.calls++
	if callback != nil {
		callback("opus trial started\n", "stdout")
	}
	return e.delegate.Run(ctx, timeout, dir, env, name, args...)
}

func (e *runnerHarborExec) LookPath(name string) (string, error) {
	return name, nil
}

func (e *runnerHarborExec) Run(_ context.Context, _ time.Duration, _ string, _ []string, name string, args ...string) executor.Result {
	e.commands = append(e.commands, name+" "+strings.Join(args, " "))
	if len(e.outputs) == 0 {
		return executor.Result{Command: name, Err: os.ErrInvalid}
	}
	out := e.outputs[0]
	e.outputs = e.outputs[1:]
	if stdout, err := e.materializeOutput(args, out); err == nil {
		out = stdout
	}
	return executor.Result{Command: name + " " + strings.Join(args, " "), Stdout: out}
}

func (e *runnerHarborExec) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, _ io.Writer, callback executor.OutputCallback, name string, args ...string) executor.Result {
	if callback != nil {
		callback("trial started\n", "stdout")
	}
	return e.Run(ctx, timeout, dir, env, name, args...)
}

func (e *runnerHarborExec) materializeOutput(args []string, out string) (string, error) {
	var result domain.TrialResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return "", err
	}
	jobsDir, ok := runnerArgValue(args, "-o")
	if !ok {
		return "", fmt.Errorf("-o not found")
	}
	idx := e.counter
	e.counter++
	resultDir := filepath.Join(jobsDir, "fake-"+strconv.Itoa(idx))
	rawDir := filepath.Join(jobsDir, "fake-raw-"+strconv.Itoa(idx))
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		return "", err
	}
	rawResultPath := filepath.Join(rawDir, "result.json")
	rawResult := []byte(`{"schema_version":"harbor.raw.fixture.v1"}`)
	if err := os.WriteFile(rawResultPath, rawResult, 0o644); err != nil {
		return "", err
	}
	result.RawResultPath = rawResultPath
	result.RawResultSHA256 = runnerSHA256(rawResult)
	resultPath := filepath.Join(resultDir, "result.json")
	normalized, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(resultPath, normalized, 0o644); err != nil {
		return "", err
	}
	return "Results written to " + resultPath + "\nRaw result evidence: " + rawResultPath + "\n", nil
}

func runnerArgValue(args []string, flag string) (string, bool) {
	for i := 0; i < len(args); i++ {
		if args[i] == flag && i+1 < len(args) {
			return args[i+1], true
		}
		if strings.HasPrefix(args[i], flag+"=") {
			return strings.TrimPrefix(args[i], flag+"="), true
		}
	}
	return "", false
}

func runnerSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeGitRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/repo\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "config.go"), []byte("package repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "initial")
	out := runGit(t, repo, "rev-parse", "HEAD")
	return repo, stringTrim(out)
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

func eventSeen(events []domain.RunnerEvent, nodeID, status string) bool {
	for _, event := range events {
		if event.NodeID == nodeID && event.Status == status {
			return true
		}
	}
	return false
}

func eventMessageSeen(events []domain.RunnerEvent, nodeID, message string) bool {
	for _, event := range events {
		if event.NodeID == nodeID && strings.Contains(event.Message, message) {
			return true
		}
	}
	return false
}

func eventIndex(events []domain.RunnerEvent, nodeID, status string) int {
	for i, event := range events {
		if event.NodeID == nodeID && event.Status == status {
			return i
		}
	}
	return -1
}

func jsonText(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func stringTrim(value string) string {
	for len(value) > 0 && (value[len(value)-1] == '\n' || value[len(value)-1] == '\r' || value[len(value)-1] == ' ' || value[len(value)-1] == '\t') {
		value = value[:len(value)-1]
	}
	return value
}
