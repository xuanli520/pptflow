package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
)

func TestCloneRunnerOptionsCreatesIndependentWorkspaceWithSelectedEvidence(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	target := filepath.Join(t.TempDir(), "target")
	taskDir := t.TempDir()
	if _, err := SaveRunnerOptions(RunnerOptions{
		Workspace:       source,
		TaskDir:         taskDir,
		TaskName:        "clone-me",
		VerifyDocker:    true,
		QualityCheck:    true,
		SimilarityCheck: true,
		RunHarbor:       true,
	}); err != nil {
		t.Fatal(err)
	}
	writeCloneJSON(t, nodes.VerifyReportPath(source), domain.VerifyReport{SchemaVersion: "harbor.verify_report.v1", TaskDir: taskDir, Passed: true})
	writeCloneJSON(t, nodes.QualityReportPath(source), domain.QualityReport{SchemaVersion: "harbor.quality_report.v1", TaskDir: taskDir, OverallPass: true})
	writeCloneJSON(t, nodes.SimilarityReportPath(source), domain.SimilarityReport{SchemaVersion: "harbor.similarity_report.v1", TaskDir: taskDir, OverallPass: true})
	writeCloneJSON(t, nodes.QwenResultPath(source), domain.TrialResult{SchemaVersion: "harbor.trial_result.v1", Model: "qwen", Trials: 4})
	if err := os.WriteFile(filepath.Join(source, "state.json"), []byte(`{"run_id":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "event_log.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	opts, manifest, err := CloneRunnerOptions(CloneWorkspaceOptions{
		SourceWorkspace:  source,
		TargetWorkspace:  target,
		ReuseDocker:      true,
		ReuseQuality:     true,
		ReuseSimilarity:  true,
		ReuseHarbor:      true,
		AutoApproveGates: true,
		RuntimeOptions: RunnerOptions{
			HarborAgentEnv:    []string{"ANTHROPIC_AUTH_TOKEN=${ANTHROPIC_AUTH_TOKEN}"},
			QwenHarborBaseURL: "https://qwen.example",
			OpusHarborBaseURL: "https://opus.example",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Workspace != target || !opts.AutoApprove || opts.QwenResult != nodes.QwenResultPath(source) || len(opts.HarborAgentEnv) != 1 || opts.QwenHarborBaseURL != "https://qwen.example" || opts.OpusHarborBaseURL != "https://opus.example" {
		t.Fatalf("unexpected cloned options: %+v", opts)
	}
	if len(manifest.ReusedEvidence) != 4 {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	for _, path := range []string{nodes.RunOptionsPath(target), nodes.VerifyReportPath(target), nodes.QualityReportPath(target), nodes.SimilarityReportPath(target), filepath.Join(target, cloneManifestName)} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected cloned file %s: %v", path, err)
		}
	}
	for _, name := range []string{"state.json", "event_log.jsonl"} {
		if _, err := os.Stat(filepath.Join(target, name)); !os.IsNotExist(err) {
			t.Fatalf("clone inherited lifecycle file %s", name)
		}
	}
}

func TestCloneRunnerOptionsRejectsNonEmptyTarget(t *testing.T) {
	source := t.TempDir()
	if _, err := SaveRunnerOptions(RunnerOptions{Workspace: source, TaskDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CloneRunnerOptions(CloneWorkspaceOptions{SourceWorkspace: source, TargetWorkspace: target}); err == nil {
		t.Fatal("expected non-empty target to be rejected")
	}
	if _, err := os.Stat(filepath.Join(target, "keep")); err != nil {
		t.Fatalf("clone modified rejected target: %v", err)
	}
}

func writeCloneJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
