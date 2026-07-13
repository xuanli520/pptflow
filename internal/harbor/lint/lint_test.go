package lint

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/evidence"
)

func TestRunPassesValidDockerfileTask(t *testing.T) {
	taskDir := writeTask(t, false)
	analysis := writeTestsAnalysis(t, taskDir)
	report, err := Run(context.Background(), Options{
		TaskDir:       taskDir,
		RepoURL:       "https://github.com/org/repo",
		Commit:        "abc1234",
		TestsAnalysis: analysis,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Fatalf("expected lint pass, got checks: %+v", report.Checks)
	}
}

func TestRunFailsDuplicateRepoBootstrapNoOpCDAndBuildTimeTests(t *testing.T) {
	taskDir := writeTask(t, false)
	dockerfile := strings.Join([]string{
		"FROM alpine",
		"RUN git clone https://github.com/org/repo /app/repo && cd /app/repo && git checkout abc1234",
		"RUN cd /app/repo && git clone https://github.com/org/repo",
		"RUN cd /app/repo && cd repo",
		"RUN cd /app/repo && git reset --hard abc1234",
		"RUN cd /app/repo && cargo test --all-features",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(taskDir, "environment", "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{TaskDir: taskDir, RepoURL: "https://github.com/org/repo", Commit: "abc1234"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"dockerfile_repo_bootstrap_unique", "dockerfile_repo_pin_unique", "dockerfile_no_noop_cd_layer", "dockerfile_no_build_time_tests"} {
		if !hasFail(report.Checks, id) {
			t.Fatalf("missing Dockerfile semantic failure %s: %+v", id, report.Checks)
		}
	}
}

func TestRunRejectsUnguardedLockedCargoFetch(t *testing.T) {
	taskDir := writeTask(t, false)
	path := filepath.Join(taskDir, "environment", "Dockerfile")
	unsafe := "FROM rust:1.96.0-bookworm\nRUN git clone https://github.com/org/repo /app/repo && cd /app/repo && git checkout abc1234\nRUN cd /app/repo && cargo fetch --locked\n"
	if err := os.WriteFile(path, []byte(unsafe), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{TaskDir: taskDir})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFail(report.Checks, "dockerfile_cargo_lock_guard") {
		t.Fatalf("unguarded cargo fetch --locked was not rejected: %+v", report.Checks)
	}

	guarded := strings.Replace(unsafe, "cargo fetch --locked", "if [ -f Cargo.lock ]; then cargo fetch --locked; else cargo fetch; fi", 1)
	if err := os.WriteFile(path, []byte(guarded), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err = Run(context.Background(), Options{TaskDir: taskDir})
	if err != nil {
		t.Fatal(err)
	}
	if hasFail(report.Checks, "dockerfile_cargo_lock_guard") {
		t.Fatalf("guarded Cargo.lock fetch was rejected: %+v", report.Checks)
	}
}

func TestRunRequiresPinnedModernRustToolchain(t *testing.T) {
	taskDir := writeTask(t, false)
	taskTOML := filepath.Join(taskDir, "task.toml")
	raw, err := os.ReadFile(taskTOML)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `code_lang = "go"`, `code_lang = "rust"`, 1))
	if err := os.WriteFile(taskTOML, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	dockerfile := filepath.Join(taskDir, "environment", "Dockerfile")
	legacy := "FROM ubuntu:24.04\nRUN apt-get update && apt-get install -y cargo rustc git\nRUN git clone https://github.com/org/repo /app/repo && cd /app/repo && git checkout abc1234\n"
	if err := os.WriteFile(dockerfile, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{TaskDir: taskDir})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFail(report.Checks, "dockerfile_rust_toolchain_pinned") {
		t.Fatalf("legacy apt Rust toolchain was not rejected: %+v", report.Checks)
	}

	modern := "FROM rust:1.96.0-bookworm\nRUN apt-get update && apt-get install -y git\nRUN git clone https://github.com/org/repo /app/repo && cd /app/repo && git checkout abc1234\n"
	if err := os.WriteFile(dockerfile, []byte(modern), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err = Run(context.Background(), Options{TaskDir: taskDir})
	if err != nil {
		t.Fatal(err)
	}
	if hasFail(report.Checks, "dockerfile_rust_toolchain_pinned") {
		t.Fatalf("pinned Rust image was rejected: %+v", report.Checks)
	}
}

func TestRunAllowsWordsContainingLegacySubstrings(t *testing.T) {
	taskDir := writeTask(t, false)
	solution := filepath.Join(taskDir, "solution", "solve.sh")
	raw, err := os.ReadFile(solution)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("\n# Preserve the encoded representation and slider state.\n")...)
	if err := os.WriteFile(solution, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{TaskDir: taskDir})
	if err != nil {
		t.Fatal(err)
	}
	if hasFail(report.Checks, "task_file_set_legacy") {
		t.Fatalf("normal identifier substrings were rejected: %+v", report.Checks)
	}
}

func TestRunRequiresRootTestsAnalysisFile(t *testing.T) {
	taskDir := writeTask(t, false)
	if err := os.Remove(filepath.Join(taskDir, "tests_analysis.md")); err != nil {
		t.Fatal(err)
	}
	externalAnalysis := filepath.Join(t.TempDir(), "tests_analysis.md")
	if err := os.WriteFile(externalAnalysis, []byte(validTestsAnalysis()), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir:       taskDir,
		RepoURL:       "https://github.com/org/repo",
		Commit:        "abc1234",
		TestsAnalysis: externalAnalysis,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "required_tests_analysis_md") {
		t.Fatalf("expected missing root tests_analysis.md failure: %+v", report.Checks)
	}
}

func TestRunZipRequiresRootTestsAnalysisFile(t *testing.T) {
	taskDir := writeTask(t, false)
	if err := os.Remove(filepath.Join(taskDir, "tests_analysis.md")); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(t.TempDir(), "task.zip")
	writeZip(t, taskDir, "sample-task", zipPath)
	report, err := Run(context.Background(), Options{ZipPath: zipPath})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "zip_required_tests_analysis_md") {
		t.Fatalf("expected missing zip tests_analysis.md failure: %+v", report.Checks)
	}
}

func TestRunStrictSubmissionFailsMissingRequiredEvidence(t *testing.T) {
	taskDir := writeTask(t, false)
	report, err := Run(context.Background(), Options{
		TaskDir:          taskDir,
		RepoURL:          "https://github.com/org/repo",
		Commit:           "abc1234",
		StrictSubmission: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed {
		t.Fatalf("expected strict submission failure, got checks: %+v", report.Checks)
	}
	for _, id := range []string{"tests_analysis_present", "qwen_result", "opus_result", "qwen_screenshot", "opus_screenshot"} {
		if !hasFail(report.Checks, id) {
			t.Fatalf("missing strict failure %s: %+v", id, report.Checks)
		}
	}
}

func TestRunFailsInvalidTaskTOML(t *testing.T) {
	taskDir := writeTask(t, false)
	if err := os.WriteFile(filepath.Join(taskDir, "task.toml"), []byte("schema_version = [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{TaskDir: taskDir})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "task_toml_parse") {
		t.Fatalf("expected parse failure: %+v", report.Checks)
	}
}

func TestRunFailsWrongTaskTOMLSchema(t *testing.T) {
	taskDir := writeTask(t, false)
	raw, err := os.ReadFile(filepath.Join(taskDir, "task.toml"))
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `schema_version = "1.3"`, `schema_version = "1.2"`, 1))
	if err := os.WriteFile(filepath.Join(taskDir, "task.toml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{TaskDir: taskDir})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "task_toml_schema") {
		t.Fatalf("expected schema failure: %+v", report.Checks)
	}
}

func TestRunFailsUnsupportedTaskNetworkMode(t *testing.T) {
	taskDir := writeTask(t, false)
	path := filepath.Join(taskDir, "task.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `network_mode = "public"`, `network_mode = "none"`, 1))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{TaskDir: taskDir})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "task_toml_network_mode") {
		t.Fatalf("unsupported Harbor network mode must fail: %+v", report.Checks)
	}
}

func TestRunStrictSubmissionAllowsZeroToOneWithoutRepoCommit(t *testing.T) {
	taskDir := writeTask(t, false)
	raw, err := os.ReadFile(filepath.Join(taskDir, "task.toml"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(raw), "is_0_to_1 = false", "is_0_to_1 = true", 1)
	text = strings.Replace(text, "github_url = \"https://github.com/org/repo\"\n", "", 1)
	text = strings.Replace(text, "commit_id = \"abc1234\"\n", "", 1)
	if err := os.WriteFile(filepath.Join(taskDir, "task.toml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir:          taskDir,
		StrictSubmission: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasFail(report.Checks, "task_toml_github_match") || hasFail(report.Checks, "task_toml_commit_match") {
		t.Fatalf("0-1 task should not require repo/commit: %+v", report.Checks)
	}
}

func TestRunStrictSubmissionRequiresRepoCommitForNonZeroToOne(t *testing.T) {
	taskDir := writeTask(t, false)
	raw, err := os.ReadFile(filepath.Join(taskDir, "task.toml"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(raw), "github_url = \"https://github.com/org/repo\"\n", "", 1)
	text = strings.Replace(text, "commit_id = \"abc1234\"\n", "", 1)
	if err := os.WriteFile(filepath.Join(taskDir, "task.toml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir:          taskDir,
		StrictSubmission: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFail(report.Checks, "task_toml_github_match") || !hasFail(report.Checks, "task_toml_commit_match") {
		t.Fatalf("non 0-1 task should require repo/commit: %+v", report.Checks)
	}
}

func TestRunFailsUnsafeDockerfileAndSolution(t *testing.T) {
	taskDir := writeTask(t, true)
	report, err := Run(context.Background(), Options{
		TaskDir: " " + taskDir + " ",
		RepoURL: "https://github.com/org/repo",
		Commit:  "abc1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed {
		t.Fatalf("expected lint failure, got checks: %+v", report.Checks)
	}
	if !hasFail(report.Checks, "dockerfile_no_solution_tests") {
		t.Fatalf("missing dockerfile failure: %+v", report.Checks)
	}
	if !hasFail(report.Checks, "solution_no_bypass") {
		t.Fatalf("missing solution failure: %+v", report.Checks)
	}
}

func TestRunRejectsCodexOnlyApplyPatchInSolution(t *testing.T) {
	taskDir := writeTask(t, false)
	path := filepath.Join(taskDir, "solution", "solve.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\napply_patch <<'PATCH'\n*** Begin Patch\n*** End Patch\nPATCH\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{TaskDir: taskDir})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFail(report.Checks, "solution_portable_commands") {
		t.Fatalf("Codex-only apply_patch was not rejected: %+v", report.Checks)
	}
}

func TestRunRejectsCodexPatchMarkersPassedToGitApply(t *testing.T) {
	taskDir := writeTask(t, false)
	path := filepath.Join(taskDir, "solution", "solve.sh")
	content := "#!/bin/sh\ngit apply <<'PATCH'\n*** Begin Patch\n*** Update File: file.go\n*** End Patch\nPATCH\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{TaskDir: taskDir})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFail(report.Checks, "solution_patch_format") {
		t.Fatalf("invalid git apply payload was not rejected: %+v", report.Checks)
	}
}

func TestRunUsesTaskTOMLMetadataForDockerfileRepoCommit(t *testing.T) {
	taskDir := writeTask(t, false)
	if err := os.WriteFile(filepath.Join(taskDir, "environment", "Dockerfile"), []byte("FROM alpine\nRUN git clone https://github.com/other/repo /src && cd /src && git checkout deadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{TaskDir: taskDir})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "dockerfile_repo_match") || !hasFail(report.Checks, "dockerfile_commit_match") {
		t.Fatalf("expected Dockerfile mismatch from task.toml metadata: %+v", report.Checks)
	}
}

func TestRunDoesNotAcceptCommentedDockerfileCommit(t *testing.T) {
	taskDir := writeTask(t, false)
	if err := os.WriteFile(filepath.Join(taskDir, "environment", "Dockerfile"), []byte("FROM alpine\n# git checkout abc1234\nRUN git clone https://github.com/org/repo /src\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir: taskDir,
		RepoURL: "https://github.com/org/repo",
		Commit:  "abc1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "dockerfile_commit_match") {
		t.Fatalf("expected missing real checkout failure: %+v", report.Checks)
	}
}

func TestRunAcceptsEquivalentSSHGitHubDockerfileURL(t *testing.T) {
	taskDir := writeTask(t, false)
	if err := os.WriteFile(filepath.Join(taskDir, "environment", "Dockerfile"), []byte("FROM alpine\nRUN git clone git@github.com:org/repo.git /src && cd /src && git reset --hard abc1234\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir: taskDir,
		RepoURL: "https://github.com/org/repo",
		Commit:  "abc1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasFail(report.Checks, "dockerfile_repo_match") || hasFail(report.Checks, "dockerfile_commit_match") {
		t.Fatalf("equivalent git URL should pass repo/commit checks: %+v", report.Checks)
	}
}

func TestRunAcceptsEquivalentTaskMetadataGitHubURL(t *testing.T) {
	taskDir := writeTask(t, false)
	report, err := Run(context.Background(), Options{
		TaskDir: taskDir,
		RepoURL: "https://github.com/org/repo.git",
		Commit:  "abc1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasFail(report.Checks, "task_toml_github_match") {
		t.Fatalf("equivalent task metadata URL should pass: %+v", report.Checks)
	}
}

func TestRunRejectsCredentialedSubmittedRepoURL(t *testing.T) {
	taskDir := writeTask(t, false)
	reportPath := filepath.Join(t.TempDir(), "lint_report.json")
	report, err := Run(context.Background(), Options{
		TaskDir:     taskDir,
		RepoURL:     "https://token@github.com/org/repo",
		Commit:      "abc1234",
		WriteReport: reportPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "repo_url_no_credentials") {
		t.Fatalf("expected credentialed repo URL failure: %+v", report.Checks)
	}
	if report.RepoURL != "https://github.com/org/repo" {
		t.Fatalf("report repo URL should be stripped, got %q", report.RepoURL)
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "token@github") {
		t.Fatalf("lint report leaked credentialed repo URL: %s", raw)
	}
}

func TestRunRejectsSubmittedRepoURLQueryWithoutLeaking(t *testing.T) {
	taskDir := writeTask(t, false)
	reportPath := filepath.Join(t.TempDir(), "lint_report.json")
	report, err := Run(context.Background(), Options{
		TaskDir:     taskDir,
		RepoURL:     "https://github.com/org/repo?token=raw-query-secret",
		Commit:      "abc1234",
		WriteReport: reportPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "repo_url_no_credentials") {
		t.Fatalf("expected repo URL query failure: %+v", report.Checks)
	}
	if report.RepoURL != "https://github.com/org/repo" {
		t.Fatalf("report repo URL should strip query, got %q", report.RepoURL)
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "raw-query-secret") || strings.Contains(string(raw), "?token=") {
		t.Fatalf("lint report leaked query credential: %s", raw)
	}
}

func TestRunRejectsCredentialedTaskTOMLGitHubURL(t *testing.T) {
	taskDir := writeTask(t, false)
	taskTOMLPath := filepath.Join(taskDir, "task.toml")
	raw, err := os.ReadFile(taskTOMLPath)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(raw), `github_url = "https://github.com/org/repo"`, `github_url = "https://token@github.com/org/repo"`, 1)
	if err := os.WriteFile(taskTOMLPath, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(t.TempDir(), "lint_report.json")
	report, err := Run(context.Background(), Options{
		TaskDir:     taskDir,
		RepoURL:     "https://github.com/org/repo",
		Commit:      "abc1234",
		WriteReport: reportPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "task_toml_github_no_credentials") {
		t.Fatalf("expected credentialed task.toml URL failure: %+v", report.Checks)
	}
	raw, err = os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "token@github") {
		t.Fatalf("lint report leaked credentialed task.toml URL: %s", raw)
	}
}

func TestRunRejectsTaskTOMLGitHubURLFragmentWithoutLeaking(t *testing.T) {
	taskDir := writeTask(t, false)
	taskTOMLPath := filepath.Join(taskDir, "task.toml")
	raw, err := os.ReadFile(taskTOMLPath)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(raw), `github_url = "https://github.com/org/repo"`, `github_url = "https://github.com/org/repo#raw-fragment-secret"`, 1)
	if err := os.WriteFile(taskTOMLPath, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(t.TempDir(), "lint_report.json")
	report, err := Run(context.Background(), Options{
		TaskDir:     taskDir,
		RepoURL:     "https://github.com/org/repo",
		Commit:      "abc1234",
		WriteReport: reportPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "task_toml_github_no_credentials") {
		t.Fatalf("expected task.toml URL fragment failure: %+v", report.Checks)
	}
	raw, err = os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "raw-fragment-secret") {
		t.Fatalf("lint report leaked task.toml URL fragment: %s", raw)
	}
}

func TestRunRejectsNonGitHubTaskTOMLGitHubURL(t *testing.T) {
	taskDir := writeTask(t, false)
	taskTOMLPath := filepath.Join(taskDir, "task.toml")
	raw, err := os.ReadFile(taskTOMLPath)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(raw), `github_url = "https://github.com/org/repo"`, `github_url = "https://gitlab.com/org/repo"`, 1)
	if err := os.WriteFile(taskTOMLPath, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{TaskDir: taskDir, Commit: "abc1234"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "task_toml_github_url") {
		t.Fatalf("expected non-GitHub task.toml URL failure: %+v", report.Checks)
	}
}

func TestRunFailsUnexpectedTaskFile(t *testing.T) {
	taskDir := writeTask(t, false)
	if err := os.WriteFile(filepath.Join(taskDir, "environment", "extra.py"), []byte("print('extra')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{TaskDir: taskDir, RepoURL: "https://github.com/org/repo", Commit: "abc1234"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "task_file_set_allowed") {
		t.Fatalf("expected unexpected file failure: %+v", report.Checks)
	}
}

func TestRunFailsTaskSymlink(t *testing.T) {
	taskDir := writeTask(t, false)
	target := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(target, []byte("external\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(taskDir, "solution", "solve.sh")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(taskDir, "solution", "solve.sh")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	report, err := Run(context.Background(), Options{TaskDir: taskDir, RepoURL: "https://github.com/org/repo", Commit: "abc1234"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "task_file_set_regular") {
		t.Fatalf("expected symlink file-set failure: %+v", report.Checks)
	}
}

func TestRunFailsFileExistenceOnlyTest(t *testing.T) {
	taskDir := writeTask(t, false)
	if err := os.WriteFile(filepath.Join(taskDir, "tests", "test.sh"), []byte("#!/bin/sh\nset -eu\ntest -f /tmp/fixed\nstat /tmp/fixed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir: taskDir,
		RepoURL: "https://github.com/org/repo",
		Commit:  "abc1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "test_not_file_existence_only") {
		t.Fatalf("expected weak test failure: %+v", report.Checks)
	}
}

func TestRunPassesContentAssertionTest(t *testing.T) {
	taskDir := writeTask(t, false)
	if err := os.WriteFile(filepath.Join(taskDir, "tests", "test.sh"), []byte("#!/bin/sh\nset -eu\ntest -f /tmp/fixed\ngrep -q fixed /tmp/fixed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir: taskDir,
		RepoURL: "https://github.com/org/repo",
		Commit:  "abc1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasStatus(report.Checks, "test_not_file_existence_only", domain.CheckPass) || !hasStatus(report.Checks, "test_has_assertion", domain.CheckPass) {
		t.Fatalf("expected strong test checks to pass: %+v", report.Checks)
	}
}

func TestRunFailsTestReadingSolution(t *testing.T) {
	taskDir := writeTask(t, false)
	if err := os.WriteFile(filepath.Join(taskDir, "tests", "test.sh"), []byte("#!/bin/sh\nset -eu\ngrep -q fixed /solution/solve.sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir:       taskDir,
		RepoURL:       "https://github.com/org/repo",
		Commit:        "abc1234",
		TestsAnalysis: filepath.Join(taskDir, "tests_analysis.md"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "test_no_solution_bypass") {
		t.Fatalf("expected solution-reading test failure: %+v", report.Checks)
	}
}

func TestRunFailsTestsWithoutStrongAssertions(t *testing.T) {
	cases := map[string]string{
		"echo_only":         "#!/bin/sh\nset -eu\necho ok\n",
		"true_only":         "#!/bin/sh\nset -eu\ntrue\n",
		"chmod_only":        "#!/bin/sh\nset -eu\nchmod +x /tmp/app\n",
		"exit_zero":         "#!/bin/sh\nset -eu\nexit 0\n",
		"permission_only":   "#!/bin/sh\nset -eu\ntest -x /tmp/app\n[ -r /tmp/app ]\n",
		"cat_without_check": "#!/bin/sh\nset -eu\ncat /tmp/output.txt\n",
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			taskDir := writeTask(t, false)
			if err := os.WriteFile(filepath.Join(taskDir, "tests", "test.sh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			report, err := Run(context.Background(), Options{
				TaskDir: taskDir,
				RepoURL: "https://github.com/org/repo",
				Commit:  "abc1234",
			})
			if err != nil {
				t.Fatal(err)
			}
			if report.Passed || !hasFail(report.Checks, "test_has_assertion") {
				t.Fatalf("expected no strong assertion failure: %+v", report.Checks)
			}
		})
	}
}

func TestRunFailsSwallowedStrongAssertion(t *testing.T) {
	taskDir := writeTask(t, false)
	if err := os.WriteFile(filepath.Join(taskDir, "tests", "test.sh"), []byte("#!/bin/sh\nset -eu\ngrep -q fixed /tmp/fixed || true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir: taskDir,
		RepoURL: "https://github.com/org/repo",
		Commit:  "abc1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "test_no_swallowed_assertions") || !hasFail(report.Checks, "test_has_assertion") {
		t.Fatalf("expected swallowed assertion failure: %+v", report.Checks)
	}
}

func TestRunPassesStrongAssertionForms(t *testing.T) {
	cases := map[string]string{
		"diff":    "#!/bin/sh\nset -eu\ndiff -u /tmp/expected.json /tmp/actual.json\n",
		"go_test": "#!/bin/sh\nset -eu\ncd /app/repo\ngo test ./...\n",
		"http":    "#!/bin/sh\nset -eu\ncurl -fsS http://localhost:8080/health | grep -q '\"status\":\"ok\"'\n",
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			taskDir := writeTask(t, false)
			if err := os.WriteFile(filepath.Join(taskDir, "tests", "test.sh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			report, err := Run(context.Background(), Options{
				TaskDir: taskDir,
				RepoURL: "https://github.com/org/repo",
				Commit:  "abc1234",
			})
			if err != nil {
				t.Fatal(err)
			}
			if !hasStatus(report.Checks, "test_has_assertion", domain.CheckPass) || !hasStatus(report.Checks, "test_no_swallowed_assertions", domain.CheckPass) {
				t.Fatalf("expected strong assertion pass: %+v", report.Checks)
			}
		})
	}
}

func TestRunFailsInvalidQwenResultThresholds(t *testing.T) {
	taskDir := writeTask(t, false)
	analysis := writeTestsAnalysis(t, taskDir)
	qwenResult := filepath.Join(taskDir, "qwen_result.json")
	if err := os.WriteFile(qwenResult, []byte(`{"model":"qwen3.7-max","trials":4,"pass_count":2,"pass_at_4":0.5,"average_turns":12}`), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir:       taskDir,
		RepoURL:       "https://github.com/org/repo",
		Commit:        "abc1234",
		TestsAnalysis: analysis,
		QwenResult:    qwenResult,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed {
		t.Fatalf("expected qwen threshold failure, got checks: %+v", report.Checks)
	}
	if !hasFail(report.Checks, "qwen_result") {
		t.Fatalf("missing qwen result failure: %+v", report.Checks)
	}
}

func TestRunFailsTaskSecretScanWithoutLeakingValue(t *testing.T) {
	taskDir := writeTask(t, false)
	secretValue := "raw-token-value"
	if err := os.WriteFile(filepath.Join(taskDir, "notes.md"), []byte("OPENAI_API_KEY="+secretValue+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir: taskDir,
		RepoURL: "https://github.com/org/repo",
		Commit:  "abc1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "task_secret_scan") {
		t.Fatalf("expected task secret scan failure: %+v", report.Checks)
	}
	if strings.Contains(checksText(report.Checks), secretValue) {
		t.Fatalf("secret leaked in lint report: %+v", report.Checks)
	}
}

func TestRunFailsZipSecretScan(t *testing.T) {
	taskDir := writeTask(t, false)
	if err := os.WriteFile(filepath.Join(taskDir, "notes.md"), []byte("AUTH_TOKEN=raw-token-value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(t.TempDir(), "task.zip")
	writeZip(t, taskDir, "sample-task", zipPath)
	report, err := Run(context.Background(), Options{ZipPath: zipPath})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "zip_secret_scan") || !hasFail(report.Checks, "zip_file_set") {
		t.Fatalf("expected zip secret and file-set failures: %+v", report.Checks)
	}
}

func TestRunZipFailsUnexpectedFile(t *testing.T) {
	taskDir := writeTask(t, false)
	if err := os.WriteFile(filepath.Join(taskDir, "notes.md"), []byte("extra note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(t.TempDir(), "task.zip")
	writeZip(t, taskDir, "sample-task", zipPath)
	report, err := Run(context.Background(), Options{ZipPath: zipPath})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "zip_file_set") {
		t.Fatalf("expected zip file-set failure: %+v", report.Checks)
	}
}

func TestRunChecksScreenshotPathsWhenProvided(t *testing.T) {
	taskDir := writeTask(t, false)
	analysis := writeTestsAnalysis(t, taskDir)
	evidenceDir := t.TempDir()
	screenshot := filepath.Join(evidenceDir, "qwen.png")
	writeLintScreenshot(t, screenshot, "harbor_run_qwen", "qwen3.7-max", 1)
	report, err := Run(context.Background(), Options{
		TaskDir:        taskDir,
		RepoURL:        "https://github.com/org/repo",
		Commit:         "abc1234",
		TestsAnalysis:  analysis,
		QwenScreenshot: screenshot,
		OpusScreenshot: filepath.Join(evidenceDir, "missing.png"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasStatus(report.Checks, "qwen_screenshot", domain.CheckPass) || !hasFail(report.Checks, "opus_screenshot") {
		t.Fatalf("expected screenshot pass/fail checks: %+v", report.Checks)
	}
}

func TestRunDiscoversRelativeScreenshotsFromHarborResults(t *testing.T) {
	taskDir := writeTask(t, false)
	evidenceDir := t.TempDir()
	qwenResult := filepath.Join(evidenceDir, "qwen_result.json")
	opusResult := filepath.Join(evidenceDir, "opus_result.json")
	writeLintScreenshot(t, filepath.Join(evidenceDir, "qwen.png"), "harbor_run_qwen", "qwen3.7-max", 1)
	writeLintScreenshot(t, filepath.Join(evidenceDir, "opus.png"), "harbor_run_opus", "claude-opus-4-6", 3)
	if err := os.WriteFile(qwenResult, []byte(`{"model":"qwen3.7-max","screenshot":"qwen.png"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opusResult, []byte(`{"model":"claude-opus-4-6","screenshot":"opus.png"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{TaskDir: taskDir, QwenResult: qwenResult, OpusResult: opusResult})
	if err != nil {
		t.Fatal(err)
	}
	if !hasStatus(report.Checks, "qwen_screenshot", domain.CheckPass) || !hasStatus(report.Checks, "opus_screenshot", domain.CheckPass) || !hasStatus(report.Checks, "screenshots_distinct", domain.CheckPass) {
		t.Fatalf("result-declared screenshots were not discovered: %+v", report.Checks)
	}
}

func writeLintScreenshot(t *testing.T, path, slot, model string, passCount int) {
	t.Helper()
	runs := make([]domain.TrialRun, 4)
	for i := range runs {
		runs[i] = domain.TrialRun{Trial: i + 1, Passed: i < passCount, Turns: 20 + i}
	}
	pngData, err := evidence.RenderPassAt4PNG(slot, domain.TrialResult{
		Model: model, Trials: 4, PassCount: passCount, PassAt4: float64(passCount) / 4,
		AverageTurns: 21.5, Runs: runs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pngData, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunFailsThinTestsAnalysis(t *testing.T) {
	taskDir := writeTask(t, false)
	analysis := filepath.Join(taskDir, "tests_analysis.md")
	if err := os.WriteFile(analysis, []byte("## 1. instruction 和 environment 已提供的信息\nok\n## 2. 模型的理论通过路径\nok\n## 3. 模型具备通过条件的依据\nok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir:       taskDir,
		RepoURL:       "https://github.com/org/repo",
		Commit:        "abc1234",
		TestsAnalysis: analysis,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "tests_analysis_substance") {
		t.Fatalf("expected thin tests analysis failure: %+v", report.Checks)
	}
}

func TestRunChecksZipSingleTaskRoot(t *testing.T) {
	taskDir := writeTask(t, false)
	zipPath := filepath.Join(t.TempDir(), "task.zip")
	writeZip(t, taskDir, "sample-task", zipPath)
	report, err := Run(context.Background(), Options{ZipPath: zipPath})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Fatalf("expected zip lint pass, got checks: %+v", report.Checks)
	}

	badZip := filepath.Join(t.TempDir(), "bad.zip")
	writeMultiRootZip(t, badZip)
	report, err = Run(context.Background(), Options{ZipPath: badZip})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "zip_single_root") {
		t.Fatalf("expected zip_single_root failure, got checks: %+v", report.Checks)
	}
}

func TestRunZipPathDeepLintsExtractedTask(t *testing.T) {
	taskDir := writeTask(t, true)
	zipPath := filepath.Join(t.TempDir(), "task.zip")
	writeZip(t, taskDir, "sample-task", zipPath)
	report, err := Run(context.Background(), Options{
		ZipPath: zipPath,
		RepoURL: "https://github.com/org/repo",
		Commit:  "abc1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "dockerfile_no_solution_tests") || !hasFail(report.Checks, "solution_no_bypass") {
		t.Fatalf("expected deep zip lint failures, got checks: %+v", report.Checks)
	}
	if !hasStatus(report.Checks, "zip_extract", domain.CheckPass) {
		t.Fatalf("expected zip extract check: %+v", report.Checks)
	}
}

func TestRunWarnsWholeContextCopyAndFailsUnsafeComposeContext(t *testing.T) {
	taskDir := writeTask(t, false)
	if err := os.WriteFile(filepath.Join(taskDir, "environment", "Dockerfile"), []byte("FROM alpine\nCOPY . /app\nRUN git clone https://github.com/org/repo /src && cd /src && git checkout abc1234\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir: taskDir,
		RepoURL: "https://github.com/org/repo",
		Commit:  "abc1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasStatus(report.Checks, "dockerfile_copy_context", domain.CheckWarn) {
		t.Fatalf("expected copy context warning: %+v", report.Checks)
	}

	taskDir = writeTask(t, false)
	writeCompose(t, taskDir, `services:
  main:
    build:
      context: ..
    volumes:
      - ./solution:/solution:ro
      - ./tests:/tests:ro
`)
	report, err = Run(context.Background(), Options{TaskDir: taskDir})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "compose_build_context") || !hasFail(report.Checks, "compose_no_dangerous_volumes") {
		t.Fatalf("expected compose failures: %+v", report.Checks)
	}
}

func TestRunPassesStructuredComposeTask(t *testing.T) {
	taskDir := writeTask(t, false)
	writeCompose(t, taskDir, `services:
  main:
    build:
      context: .
`)
	if err := os.WriteFile(filepath.Join(taskDir, "environment", "Dockerfile"), []byte("FROM alpine\nRUN git clone https://github.com/org/repo /src && cd /src && git reset --hard abc1234\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{TaskDir: taskDir})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Fatalf("expected compose lint pass, got checks: %+v", report.Checks)
	}
	for _, id := range []string{
		"compose_parse",
		"compose_yaml_map",
		"compose_services_map",
		"compose_main_service",
		"compose_main_image_or_build",
		"compose_build_context",
		"compose_no_dangerous_volumes",
		"compose_repo_commit",
		"dockerfile_repo_match",
		"dockerfile_commit_match",
	} {
		if !hasStatus(report.Checks, id, domain.CheckPass) {
			t.Fatalf("missing compose pass %s: %+v", id, report.Checks)
		}
	}
}

func TestRunFailsComposeImageOnlyForRepoTask(t *testing.T) {
	taskDir := writeTask(t, false)
	writeCompose(t, taskDir, `services:
  main:
    image: alpine:3.20
`)
	report, err := Run(context.Background(), Options{TaskDir: taskDir})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "compose_repo_commit") {
		t.Fatalf("expected compose image-only provenance failure: %+v", report.Checks)
	}
}

func TestRunFailsStructuredComposeShape(t *testing.T) {
	cases := map[string]struct {
		compose string
		failID  string
	}{
		"not_map": {
			compose: "- services\n",
			failID:  "compose_yaml_map",
		},
		"missing_services": {
			compose: "name: sample\n",
			failID:  "compose_services_map",
		},
		"services_not_map": {
			compose: "services: []\n",
			failID:  "compose_services_map",
		},
		"missing_main": {
			compose: "services:\n  worker:\n    image: alpine\n",
			failID:  "compose_main_service",
		},
		"main_not_map": {
			compose: "services:\n  main: alpine\n",
			failID:  "compose_main_service",
		},
		"missing_image_build": {
			compose: "services:\n  main:\n    volumes:\n      - ./cache:/cache\n",
			failID:  "compose_main_image_or_build",
		},
		"empty_build_context": {
			compose: "services:\n  main:\n    build:\n      context: \"\"\n    volumes:\n      - ./solution:/solution:ro\n      - ./tests:/tests:ro\n",
			failID:  "compose_build_context",
		},
		"build_map_without_context": {
			compose: "services:\n  main:\n    build:\n      dockerfile: Dockerfile\n    volumes:\n      - ./solution:/solution:ro\n      - ./tests:/tests:ro\n",
			failID:  "compose_build_context",
		},
		"remote_build_context": {
			compose: "services:\n  main:\n    build:\n      context: https://github.com/org/repo.git\n",
			failID:  "compose_build_context",
		},
		"absolute_build_context": {
			compose: "services:\n  main:\n    build:\n      context: /tmp/context\n",
			failID:  "compose_build_context",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			taskDir := writeTask(t, false)
			writeCompose(t, taskDir, tc.compose)
			report, err := Run(context.Background(), Options{TaskDir: taskDir})
			if err != nil {
				t.Fatal(err)
			}
			if report.Passed || !hasFail(report.Checks, tc.failID) {
				t.Fatalf("expected %s failure: %+v", tc.failID, report.Checks)
			}
		})
	}
}

func TestRunFailsComposeAbsoluteDockerfile(t *testing.T) {
	taskDir := writeTask(t, false)
	writeCompose(t, taskDir, `services:
  main:
    build:
      context: .
      dockerfile: /tmp/Dockerfile
`)
	report, err := Run(context.Background(), Options{TaskDir: taskDir})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || !hasFail(report.Checks, "compose_repo_commit") {
		t.Fatalf("expected compose absolute dockerfile provenance failure: %+v", report.Checks)
	}
}

func TestRunFailsDangerousComposeVolumes(t *testing.T) {
	cases := map[string]string{
		"task_root": `services:
  main:
    image: alpine:3.20
    volumes:
      - .:/workspace
      - ./solution:/solution:ro
      - ./tests:/tests:ro
`,
		"docker_socket": `services:
  main:
    image: alpine:3.20
    volumes:
      - ./solution:/solution:ro
      - ./tests:/tests:ro
      - /var/run/docker.sock:/var/run/docker.sock
`,
		"solution_source": `services:
  main:
    image: alpine:3.20
    volumes:
      - ./solution:/workspace/solution:ro
`,
		"tests_target": `services:
  main:
    image: alpine:3.20
    volumes:
      - ./cache:/tests:ro
`,
	}
	for name, compose := range cases {
		t.Run(name, func(t *testing.T) {
			taskDir := writeTask(t, false)
			writeCompose(t, taskDir, compose)
			report, err := Run(context.Background(), Options{TaskDir: taskDir})
			if err != nil {
				t.Fatal(err)
			}
			if report.Passed || !hasFail(report.Checks, "compose_no_dangerous_volumes") {
				t.Fatalf("expected dangerous compose volume failure: %+v", report.Checks)
			}
		})
	}
}

func writeCompose(t *testing.T, taskDir, content string) {
	t.Helper()
	if err := os.Remove(filepath.Join(taskDir, "environment", "Dockerfile")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "environment", "docker-compose.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTask(t *testing.T, unsafe bool) string {
	t.Helper()
	root := t.TempDir()
	mkdirs := []string{"environment", "solution", "tests"}
	for _, dir := range mkdirs {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("instruction.md", "Fix the repository behavior.\n")
	write("task.toml", `schema_version = "1.3"

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
	`)
	dockerfile := "FROM alpine\nRUN git clone https://github.com/org/repo /src && cd /src && git checkout abc1234\n"
	solve := "#!/bin/sh\nset -eu\necho fixed > /tmp/fixed\n"
	if unsafe {
		dockerfile += "COPY solution /solution\n"
		solve += "echo 1 > /logs/verifier/reward.txt\n"
	}
	write(filepath.Join("environment", "Dockerfile"), dockerfile)
	write(filepath.Join("solution", "solve.sh"), solve)
	write(filepath.Join("tests", "test.sh"), "#!/bin/sh\nset -eu\ngrep -q fixed /tmp/fixed\n")
	write("tests_analysis.md", validTestsAnalysis())
	return root
}

func hasFail(checks []domain.CheckResult, id string) bool {
	return hasStatus(checks, id, domain.CheckFail)
}

func hasStatus(checks []domain.CheckResult, id string, status domain.CheckStatus) bool {
	for _, check := range checks {
		if check.ID == id && check.Status == status {
			return true
		}
	}
	return false
}

func checksText(checks []domain.CheckResult) string {
	var b strings.Builder
	for _, check := range checks {
		b.WriteString(check.Message)
		b.WriteByte('\n')
	}
	return b.String()
}

func writeTestsAnalysis(t *testing.T, taskDir string) string {
	t.Helper()
	path := filepath.Join(taskDir, "tests_analysis.md")
	if err := os.WriteFile(path, []byte(validTestsAnalysis()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func validTestsAnalysis() string {
	return `## 1. instruction 和 environment 已提供的信息
- instruction 要求修复仓库行为，environment 固定公开 GitHub commit 并说明在容器内运行测试。
- 可见约束包括不能读取 solution/tests，模型只能根据 instruction、源码和 environment 判断任务边界。

---

## 2. 模型的理论通过路径
- 模型需要阅读 instruction 与仓库代码，定位 config.go 相关行为，再实现符合现有接口的修复。
- 完成后运行 tests/test.sh 或等价 verifier 命令，确认行为输出满足任务描述而不是依赖隐藏文件。

---

## 3. 模型具备通过条件的依据
- verifier 的核心检查点可从 instruction 和 environment 推导，tests/test.sh 只验证公开行为结果。
- 通过条件不依赖隐藏业务要求，也不要求模型读取测试源码之外的私有凭证或 reward 文件。
`
}

func writeZip(t *testing.T, taskDir, rootName, zipPath string) {
	t.Helper()
	out, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	defer zw.Close()
	err = filepath.WalkDir(taskDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		rel, err := filepath.Rel(taskDir, path)
		if err != nil {
			return err
		}
		writer, err := zw.Create(filepath.ToSlash(filepath.Join(rootName, rel)))
		if err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(writer, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func writeMultiRootZip(t *testing.T, zipPath string) {
	t.Helper()
	out, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	defer zw.Close()
	for _, name := range []string{"one/instruction.md", "two/instruction.md"} {
		writer, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
}
