package similarity

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRunDetectsLocalHistorySimilarity(t *testing.T) {
	taskDir := writeSimilarityTask(t)
	history := t.TempDir()
	if err := os.WriteFile(filepath.Join(history, "old-task.md"), []byte("Fix config loader so environment override values win over file defaults. The verifier runs go test ./... and checks config.go."), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir:     taskDir,
		HistoryDirs: []string{history},
		Threshold:   0.12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OverallPass || report.MaxScore < 0.12 || len(report.Candidates) == 0 {
		t.Fatalf("expected local similarity failure, got %+v", report)
	}
	if report.Candidates[0].Source != "history" {
		t.Fatalf("candidate source = %s", report.Candidates[0].Source)
	}
}

func TestRunSearchesGitHubWhenEnabled(t *testing.T) {
	taskDir := writeSimilarityTask(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/issues" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"items":[{"html_url":"https://github.com/org/repo/issues/12","title":"config loader environment override bug","body":"environment override values should win over file defaults in config.go"}]}`)
	}))
	defer server.Close()
	report, err := Run(context.Background(), Options{
		TaskDir:       taskDir,
		RepoURL:       "https://github.com/org/repo",
		EnableGitHub:  true,
		GitHubBaseURL: server.URL,
		Threshold:     0.10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OverallPass || len(report.Candidates) == 0 {
		t.Fatalf("expected github similarity candidate, got %+v", report)
	}
	if report.Candidates[0].Source != "github_issue_pr" || report.Candidates[0].URL == "" {
		t.Fatalf("unexpected candidate: %+v", report.Candidates[0])
	}
	if len(report.SourceEvidence) != 1 {
		t.Fatalf("expected github source evidence, got %+v", report.SourceEvidence)
	}
	evidence := report.SourceEvidence[0]
	if evidence.Source != "github" || evidence.Kind != "github" || evidence.QueryCount != 2 || evidence.ResultCount != 2 || len(evidence.HTTPStatuses) != 2 {
		t.Fatalf("unexpected github source evidence: %+v", evidence)
	}
}

func TestRunIncludesExternalTestsAnalysisPath(t *testing.T) {
	taskDir := writeSimilarityTask(t)
	analysis := filepath.Join(t.TempDir(), "tests_analysis.md")
	if err := os.WriteFile(analysis, []byte("unique reviewer analysis phrase about sentinel retry backoff handling"), 0o644); err != nil {
		t.Fatal(err)
	}
	history := t.TempDir()
	if err := os.WriteFile(filepath.Join(history, "old-analysis.md"), []byte("unique reviewer analysis phrase about sentinel retry backoff handling"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir:           taskDir,
		TestsAnalysisPath: analysis,
		HistoryDirs:       []string{history},
		Threshold:         0.08,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OverallPass || len(report.Candidates) == 0 {
		t.Fatalf("expected external tests analysis to affect similarity, got %+v", report)
	}
}

func TestRunIncludesSolutionAndEnvironmentInReferenceText(t *testing.T) {
	taskDir := writeSimilarityTask(t)
	phrase := "sentinel retry backoff dockerfile solution patch marker"
	if err := os.WriteFile(filepath.Join(taskDir, "solution", "solve.sh"), []byte("#!/bin/sh\n# "+phrase+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "environment", "Dockerfile"), []byte("FROM alpine\n# "+phrase+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	history := t.TempDir()
	if err := os.WriteFile(filepath.Join(history, "old-task.md"), []byte(phrase), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir:     taskDir,
		HistoryDirs: []string{history},
		Threshold:   0.02,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OverallPass || report.MaxScore < 0.02 || len(report.Candidates) == 0 {
		t.Fatalf("expected solution/environment similarity failure, got %+v", report)
	}
}

func TestRunStrictSourcesFailsWhenNoSourcesConfigured(t *testing.T) {
	taskDir := writeSimilarityTask(t)
	report, err := Run(context.Background(), Options{
		TaskDir:       taskDir,
		StrictSources: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OverallPass || len(report.Issues) == 0 {
		t.Fatalf("expected strict source failure, got %+v", report)
	}
}

func TestRunStrictSourcesFailsWhenConfiguredSourceCannotBeScanned(t *testing.T) {
	taskDir := writeSimilarityTask(t)
	missing := filepath.Join(t.TempDir(), "missing-history")
	report, err := Run(context.Background(), Options{
		TaskDir:       taskDir,
		HistoryDirs:   []string{missing},
		StrictSources: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OverallPass || len(report.Issues) == 0 {
		t.Fatalf("expected strict unreadable source failure, got %+v", report)
	}
	if len(report.Sources) != 1 || report.Sources[0] != "history:"+missing {
		t.Fatalf("expected configured source to be recorded, got %+v", report.Sources)
	}
}

func TestRunStrictSourcesFailsWhenGitHubSearchFails(t *testing.T) {
	taskDir := writeSimilarityTask(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()
	report, err := Run(context.Background(), Options{
		TaskDir:       taskDir,
		RepoURL:       "https://github.com/org/repo",
		EnableGitHub:  true,
		GitHubBaseURL: server.URL,
		StrictSources: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OverallPass || len(report.Issues) == 0 {
		t.Fatalf("expected strict github search failure, got %+v", report)
	}
}

func TestRunRejectsCredentialedRepoURL(t *testing.T) {
	taskDir := writeSimilarityTask(t)
	reportPath := filepath.Join(t.TempDir(), "similarity_report.json")
	report, err := Run(context.Background(), Options{
		TaskDir:     taskDir,
		RepoURL:     "https://token@github.com/org/repo",
		WriteReport: reportPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OverallPass || report.RepoURL != "https://github.com/org/repo" || len(report.Issues) == 0 {
		t.Fatalf("expected credentialed repo URL failure with stripped URL, got %+v", report)
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || bytes.Contains(raw, []byte("token@github")) {
		t.Fatalf("similarity report leaked credentialed repo URL: %s", raw)
	}
}

func TestRunRejectsRepoURLQueryWithoutLeaking(t *testing.T) {
	taskDir := writeSimilarityTask(t)
	reportPath := filepath.Join(t.TempDir(), "similarity_report.json")
	report, err := Run(context.Background(), Options{
		TaskDir:     taskDir,
		RepoURL:     "https://github.com/org/repo?token=raw-query-secret",
		WriteReport: reportPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OverallPass || report.RepoURL != "https://github.com/org/repo" || len(report.Issues) == 0 {
		t.Fatalf("expected repo URL query failure with stripped URL, got %+v", report)
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("raw-query-secret")) || bytes.Contains(raw, []byte("?token=")) {
		t.Fatalf("similarity report leaked query credential: %s", raw)
	}
}

func TestRunStrictGitHubRejectsNonRepoURL(t *testing.T) {
	taskDir := writeSimilarityTask(t)
	report, err := Run(context.Background(), Options{
		TaskDir:       taskDir,
		RepoURL:       "https://github.com/org/repo/issues",
		EnableGitHub:  true,
		StrictSources: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OverallPass || len(report.Issues) == 0 {
		t.Fatalf("expected non-repo GitHub URL to fail strict github source, got %+v", report)
	}
}

func TestRunRecordsConfiguredSources(t *testing.T) {
	taskDir := writeSimilarityTask(t)
	history := t.TempDir()
	if err := os.WriteFile(filepath.Join(history, "old.md"), []byte("unrelated history note"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir:     taskDir,
		HistoryDirs: []string{history},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sources) != 1 || report.Sources[0] != "history:"+history {
		t.Fatalf("expected history source, got %+v", report.Sources)
	}
	if len(report.SuccessfulSources) != 1 || report.SuccessfulSources[0] != "history:"+history || report.ScannedFileCount != 1 {
		t.Fatalf("expected successful history scan, got sources=%+v count=%d", report.SuccessfulSources, report.ScannedFileCount)
	}
	if len(report.SourceEvidence) != 1 {
		t.Fatalf("expected local source evidence, got %+v", report.SourceEvidence)
	}
	evidence := report.SourceEvidence[0]
	if evidence.Source != "history:"+history || evidence.Kind != "history" || evidence.Path != history || evidence.ScannedFileCount != 1 || evidence.SourceDigest == "" {
		t.Fatalf("unexpected source evidence: %+v", evidence)
	}
	if report.TaskDigest == "" {
		t.Fatalf("expected task digest to be recorded: %+v", report)
	}
}

func TestBuildLocalSourceEvidenceChangesWhenSourceChanges(t *testing.T) {
	history := t.TempDir()
	path := filepath.Join(history, "old.md")
	if err := os.WriteFile(path, []byte("unrelated history note"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := BuildLocalSourceEvidence("history", history)
	if err != nil {
		t.Fatal(err)
	}
	if first.ScannedFileCount != 1 || first.SourceDigest == "" {
		t.Fatalf("unexpected first evidence: %+v", first)
	}
	if err := os.WriteFile(filepath.Join(history, "new.md"), []byte("additional historical task note"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := BuildLocalSourceEvidence("history", history)
	if err != nil {
		t.Fatal(err)
	}
	if second.ScannedFileCount != 2 || second.SourceDigest == first.SourceDigest {
		t.Fatalf("expected evidence digest to change, first=%+v second=%+v", first, second)
	}
}

func writeSimilarityTask(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"environment", "solution", "tests"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"instruction.md":    "Fix the config loader so environment override values win over file defaults while preserving fallback behavior in config.go.\n",
		"task.toml":         "schema_version = \"1.3\"\n\n[task]\nname = \"codeedge/config-override\"\n",
		"tests_analysis.md": "## 1. instruction 和 environment 已提供的信息\n- instruction 和 environment describe a config loader override task.\n\n## 2. 模型的理论通过路径\n- The model can inspect config.go and run tests/test.sh.\n\n## 3. 模型具备通过条件的依据\n- The verifier checks visible override behavior.\n",
		"tests/test.sh":     "cd /app/repo\ngo test ./...\ngrep -q Override config.go\n",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
