package packager

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/evidence"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	similaritycheck "github.com/purplevoid/harbor-factory/internal/harbor/similarity"
)

func TestPackageCreatesZipWithSingleTaskRootAndSubmissionReport(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult := filepath.Join(outputDir, "qwen.json")
	opusResult := filepath.Join(outputDir, "opus.json")
	if err := os.WriteFile(qwenResult, []byte(packageTrialResultJSON(t, outputDir, taskDir, "qwen3.7-max", []domain.TrialRun{
		{Trial: 1, Turns: 22, Reward: 0},
		{Trial: 2, Passed: true, Turns: 24, Reward: 1},
		{Trial: 3, Turns: 23, Reward: 0},
		{Trial: 4, Turns: 23, Reward: 0},
	}, "qwen.png")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opusResult, []byte(packageTrialResultJSON(t, outputDir, taskDir, "claude-opus-4-6", []domain.TrialRun{
		{Trial: 1, Passed: true, Turns: 28, Reward: 1},
		{Trial: 2, Passed: true, Turns: 29, Reward: 1},
		{Trial: 3, Passed: true, Turns: 27, Reward: 1},
		{Trial: 4, Turns: 28, Reward: 0},
	}, "opus.png")), 0o644); err != nil {
		t.Fatal(err)
	}
	qwenScreenshot := filepath.Join(outputDir, "qwen.png")
	opusScreenshot := filepath.Join(outputDir, "opus.png")
	writePackageScreenshot(t, qwenScreenshot, "harbor_run_qwen", "qwen3.7-max", 1)
	writePackageScreenshot(t, opusScreenshot, "harbor_run_opus", "claude-opus-4-6", 3)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	report, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		QualityReport:    "quality_report.json",
		SimilarityReport: similarityReport,
		VerifyReport:     verifyReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Fatalf("report = %+v", report)
	}
	if _, err := os.Stat(report.ReportPath); err != nil {
		t.Fatal(err)
	}
	var submission map[string]any
	raw, err := os.ReadFile(report.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &submission); err != nil {
		t.Fatal(err)
	}
	if submission["code_lang"] != "go" || submission["task_type"] != "bug-fix" || submission["application"] != "backend" {
		t.Fatalf("submission missing claim fields: %+v", submission)
	}
	if submission["qwen_pass4_screenshot"] != qwenScreenshot || submission["opus_pass4_screenshot"] != opusScreenshot {
		t.Fatalf("submission missing screenshot fields: %+v", submission)
	}
	if submission["quality_report"] != "quality_report.json" {
		t.Fatalf("submission missing quality report: %+v", submission)
	}
	if submission["verify_report"] != verifyReport {
		t.Fatalf("submission missing verify report: %+v", submission)
	}
	if submission["similarity_report"] != similarityReport {
		t.Fatalf("submission missing similarity report: %+v", submission)
	}
	sources, ok := submission["similarity_sources"].([]any)
	if !ok || len(sources) != 1 || !strings.HasPrefix(sources[0].(string), "history:") || submission["similarity_passed"] != true {
		t.Fatalf("submission missing similarity summary: %+v", submission)
	}
	averageTurns := submission["average_turns"].(map[string]any)
	if averageTurns["qwen"].(float64) != 23 || averageTurns["opus"].(float64) != 28 {
		t.Fatalf("submission missing average turns: %+v", submission)
	}
	zr, err := zip.OpenReader(report.OutputZip)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	want := map[string]bool{
		"sample-task/instruction.md":         false,
		"sample-task/task.toml":              false,
		"sample-task/tests_analysis.md":      false,
		"sample-task/environment/Dockerfile": false,
		"sample-task/solution/solve.sh":      false,
		"sample-task/tests/test.sh":          false,
	}
	for _, file := range zr.File {
		if _, ok := want[file.Name]; ok {
			want[file.Name] = true
		}
		if filepath.Dir(file.Name) == "." {
			t.Fatalf("file without task root: %s", file.Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("zip missing %s", name)
		}
	}
}

func TestPackageRejectsUnsafeTaskName(t *testing.T) {
	taskDir := writePackageTask(t)
	for _, name := range []string{"../escape", "/tmp/escape", "too/many/segments", `nested\name`, ".", "..", ".hidden", "bad:name", "bad name"} {
		t.Run(name, func(t *testing.T) {
			_, err := Package(Options{
				TaskDir:   taskDir,
				OutputDir: t.TempDir(),
				TaskName:  name,
			})
			if err == nil || !strings.Contains(err.Error(), "task name") {
				t.Fatalf("expected task name error for %q, got %v", name, err)
			}
		})
	}
}

func TestNormalizeTaskNameAcceptsRegistryName(t *testing.T) {
	got, err := NormalizeTaskName("codeedge/sample-harbor-task")
	if err != nil {
		t.Fatal(err)
	}
	if got != "sample-harbor-task" {
		t.Fatalf("normalized task name = %q", got)
	}
}

func TestPackageRejectsMismatchedHarborCommandOutputEvidence(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	qwenStdout := filepath.Join(outputDir, "qwen3.7-max_command_run_stdout.txt")
	if err := os.WriteFile(qwenStdout, []byte("tampered stdout"), 0o644); err != nil {
		t.Fatal(err)
	}
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		SimilarityReport: similarityReport,
		VerifyReport:     verifyReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "stdout_path content does not match") {
		t.Fatalf("expected command stdout evidence mismatch failure, got %v", err)
	}
}

func TestWriteZipIncludesTaskRootTestsAnalysis(t *testing.T) {
	taskDir := writePackageTask(t)
	expected, err := os.ReadFile(filepath.Join(taskDir, "tests_analysis.md"))
	if err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(t.TempDir(), "sample-task.zip")
	if err := writeZip(taskDir, "sample-task", zipPath); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var found bool
	for _, file := range zr.File {
		if file.Name != "sample-task/tests_analysis.md" {
			continue
		}
		found = true
		rc, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != string(expected) {
			t.Fatalf("zip tests_analysis.md did not use task root file: %q", data)
		}
	}
	if !found {
		t.Fatal("zip missing tests_analysis.md")
	}
}

func TestPackageRejectsMismatchedTestsAnalysisEvidence(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	externalAnalysis := filepath.Join(t.TempDir(), "tests_analysis.md")
	if err := os.WriteFile(externalAnalysis, []byte("different tests analysis\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    externalAnalysis,
	})
	if err == nil || !strings.Contains(err.Error(), "must match task root tests_analysis.md") {
		t.Fatalf("expected mismatched tests_analysis failure, got %v", err)
	}
}

func TestValidateSubmissionFieldsRejectsCredentialedGitHubURL(t *testing.T) {
	err := validateSubmissionFields(Options{
		CodeLang:      "go",
		TaskType:      "bug-fix",
		Application:   "backend",
		AHT:           "45 minutes",
		Description:   "sample",
		TestsAnalysis: "tests_analysis.md",
		GitHubURL:     "https://token@github.com/org/repo",
		CommitID:      "abc1234",
	})
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("expected credentialed GitHub URL failure, got %v", err)
	}
}

func TestValidateSubmissionFieldsRejectsNonGitHubURL(t *testing.T) {
	err := validateSubmissionFields(Options{
		CodeLang:      "go",
		TaskType:      "bug-fix",
		Application:   "backend",
		AHT:           "45 minutes",
		Description:   "sample",
		TestsAnalysis: "tests_analysis.md",
		GitHubURL:     "https://gitlab.com/org/repo",
		CommitID:      "abc1234",
	})
	if err == nil || !strings.Contains(err.Error(), "GitHub repository URL") {
		t.Fatalf("expected non-GitHub URL failure, got %v", err)
	}
}

func TestValidatePackageFileSetFailsMissingRootTestsAnalysis(t *testing.T) {
	taskDir := writePackageTask(t)
	if err := os.Remove(filepath.Join(taskDir, "tests_analysis.md")); err != nil {
		t.Fatal(err)
	}
	err := validatePackageFileSet(taskDir)
	if err == nil || !strings.Contains(err.Error(), "missing required Harbor package file: tests_analysis.md") {
		t.Fatalf("expected missing root tests_analysis.md failure, got %v", err)
	}
}

func TestPackageFailsWhenTaskDirMissingRootTestsAnalysisEvenWithExternalEvidence(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	externalAnalysis := filepath.Join(t.TempDir(), "tests_analysis.md")
	if err := os.Rename(filepath.Join(taskDir, "tests_analysis.md"), externalAnalysis); err != nil {
		t.Fatal(err)
	}
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    externalAnalysis,
	})
	if err == nil || !strings.Contains(err.Error(), "task root tests_analysis.md is required") {
		t.Fatalf("expected missing root tests_analysis.md package failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "sample-task.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("zip should not be created, stat err=%v", statErr)
	}
}

func TestPackageFailsMissingParsedSubmissionEvidence(t *testing.T) {
	taskDir := writePackageTask(t)
	_, err := Package(Options{
		TaskDir:     taskDir,
		OutputDir:   t.TempDir(),
		TaskName:    "sample-task",
		CodeLang:    "go",
		TaskType:    "bug-fix",
		Application: "backend",
		AHT:         "45 minutes",
		Description: "sample description",
		GitHubURL:   "https://github.com/org/repo",
		CommitID:    "abc1234",
	})
	if err == nil {
		t.Fatal("expected package failure without harbor results and screenshots")
	}
}

func TestPackageFailsWhenVerifyReportDoesNotProveOracleFlow(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, false, true)
	_, err := Package(Options{
		TaskDir:       taskDir,
		OutputDir:     outputDir,
		TaskName:      "sample-task",
		CodeLang:      "go",
		TaskType:      "bug-fix",
		Application:   "backend",
		AHT:           "45 minutes",
		Description:   "sample description",
		GitHubURL:     "https://github.com/org/repo",
		CommitID:      "abc1234",
		VerifyReport:  verifyReport,
		QwenResult:    qwenResult,
		OpusResult:    opusResult,
		TestsAnalysis: "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "initial verification exposes the issue") {
		t.Fatalf("expected verify report failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "sample-task.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("zip should not be created, stat err=%v", statErr)
	}
}

func TestPackageFailsWhenVerifyReportDigestDoesNotMatchTask(t *testing.T) {
	taskDir := writePackageTask(t)
	otherTask := writePackageTask(t)
	if err := os.WriteFile(filepath.Join(otherTask, "instruction.md"), []byte("Different task instruction for digest mismatch.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, otherTask, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "verify report task_digest does not match current task") {
		t.Fatalf("expected verify digest mismatch failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "sample-task.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("zip should not be created, stat err=%v", statErr)
	}
}

func TestPackageFailsWhenVerifyReportHasNoCommandLogProvenance(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	raw, err := os.ReadFile(verifyReport)
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	delete(report, "command_logs")
	raw, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(verifyReport, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "verify report command_logs") {
		t.Fatalf("expected verify provenance failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "sample-task.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("zip should not be created, stat err=%v", statErr)
	}
}

func TestPackageFailsWhenVerifyReportCommandLogUsesNonDockerArgv(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	mutateVerifyCommandLogs(t, verifyReport, func(logs []map[string]any) {
		logs[0]["argv"] = []string{"true"}
	})
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "docker_build argv") {
		t.Fatalf("expected verify argv failure, got %v", err)
	}
}

func TestPackageFailsWhenVerifyReportCommandOutputEvidenceMismatches(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	if err := os.WriteFile(filepath.Join(outputDir, "command_logs", "docker_build", "stdout.txt"), []byte("tampered docker stdout"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "stdout_path content does not match") {
		t.Fatalf("expected verify stdout evidence mismatch failure, got %v", err)
	}
}

func TestPackageFailsWhenInitialVerifyArgvMountsSolution(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	mutateVerifyCommandLogs(t, verifyReport, func(logs []map[string]any) {
		logs[1]["argv"] = []string{"docker", "run", "-v", "/task/solution:/solution:ro", "-v", "/task/tests:/tests:ro", "image", "/bin/sh", "-c", "/tests/test.sh"}
	})
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "tests-only initial verification") {
		t.Fatalf("expected initial verify argv failure, got %v", err)
	}
}

func TestPackageFailsWhenSimilarityReportDigestDoesNotMatchTask(t *testing.T) {
	taskDir := writePackageTask(t)
	otherTask := writePackageTask(t)
	if err := os.WriteFile(filepath.Join(otherTask, "instruction.md"), []byte("Different task instruction for similarity digest mismatch.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, otherTask, true, []string{"history:/tmp/history"})
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "similarity report task_digest does not match current task") {
		t.Fatalf("expected similarity digest mismatch failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "sample-task.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("zip should not be created, stat err=%v", statErr)
	}
}

func TestPackageFailsWhenSimilarityReportHasNoSources(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, nil)
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "similarity report has no configured sources") {
		t.Fatalf("expected similarity source failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "sample-task.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("zip should not be created, stat err=%v", statErr)
	}
}

func TestPackageFailsWhenSimilarityReportHasUnsupportedSource(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"manual"})
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported source") {
		t.Fatalf("expected unsupported similarity source failure, got %v", err)
	}
}

func TestPackageFailsWhenGitHubSimilaritySuccessHasNoSourceEvidence(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"github"})
	mutateSimilarityReport(t, similarityReport, func(report map[string]any) {
		delete(report, "source_evidence")
	})
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "github successful source requires source_evidence") {
		t.Fatalf("expected missing github similarity evidence failure, got %v", err)
	}
}

func TestPackageFailsWhenSimilarityReportRaisesThreshold(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	mutateSimilarityReport(t, similarityReport, func(report map[string]any) {
		report["threshold"] = 0.99
		report["max_score"] = 0.50
	})
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "threshold") {
		t.Fatalf("expected raised similarity threshold failure, got %v", err)
	}
}

func TestPackageFailsWhenLocalSimilaritySuccessScannedNoFiles(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	raw, err := os.ReadFile(similarityReport)
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	report["scanned_file_count"] = 0
	raw, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(similarityReport, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "scanned_file_count") {
		t.Fatalf("expected zero scanned file count failure, got %v", err)
	}
}

func TestPackageFailsWhenLocalSimilaritySuccessHasNoSourceEvidence(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	mutateSimilarityReport(t, similarityReport, func(report map[string]any) {
		delete(report, "source_evidence")
	})
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "source_evidence") {
		t.Fatalf("expected missing local similarity evidence failure, got %v", err)
	}
}

func TestPackageFailsWhenLocalSimilaritySourceDigestChanges(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	var sourcePath string
	mutateSimilarityReport(t, similarityReport, func(report map[string]any) {
		evidence, ok := report["source_evidence"].([]any)
		if !ok || len(evidence) == 0 {
			t.Fatalf("source_evidence has unexpected shape: %+v", report["source_evidence"])
		}
		item, ok := evidence[0].(map[string]any)
		if !ok {
			t.Fatalf("source_evidence item has unexpected shape: %+v", evidence[0])
		}
		sourcePath, _ = item["path"].(string)
	})
	if sourcePath == "" {
		t.Fatal("missing source evidence path")
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "unrelated.md"), []byte("changed historical task material that keeps file count stable\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "source_digest does not match") {
		t.Fatalf("expected local similarity digest mismatch failure, got %v", err)
	}
}

func TestPackageFailsWhenSimilarityReportHasNoSuccessfulSources(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	raw, err := os.ReadFile(similarityReport)
	if err != nil {
		t.Fatal(err)
	}
	var report domain.SimilarityReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	report.SuccessfulSources = nil
	report.ScannedFileCount = 0
	raw, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(similarityReport, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "similarity report has no successfully scanned sources") {
		t.Fatalf("expected successful source failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "sample-task.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("zip should not be created, stat err=%v", statErr)
	}
}

func TestPackageFailsWhenSimilarityReportDoesNotPass(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, false, []string{"history:/tmp/history"})
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "similarity report did not pass") {
		t.Fatalf("expected similarity report failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "sample-task.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("zip should not be created, stat err=%v", statErr)
	}
}

func TestPackageFailsWhenHarborResultViolatesThresholds(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	qwenResult := filepath.Join(outputDir, "qwen.json")
	opusResult := filepath.Join(outputDir, "opus.json")
	if err := os.WriteFile(qwenResult, []byte(`{"model":"qwen3.7-max","trials":4,"pass_count":2,"pass_at_4":0.50,"average_turns":23,"screenshot":"qwen.png"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opusResult, []byte(`{"model":"claude-opus-4-6","trials":4,"pass_count":3,"pass_at_4":0.75,"average_turns":28,"screenshot":"opus.png"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "qwen harbor result does not meet CodeEdge thresholds") {
		t.Fatalf("expected qwen threshold failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "sample-task.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("zip should not be created, stat err=%v", statErr)
	}
}

func TestPackageFailsWhenEvidenceFilesAreUnreadable(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	missingAnalysis := filepath.Join(taskDir, "missing-tests-analysis.md")
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    missingAnalysis,
	})
	if err == nil || !strings.Contains(err.Error(), "tests_analysis is not a readable file") {
		t.Fatalf("expected missing tests analysis failure, got %v", err)
	}

	if err := os.Remove(filepath.Join(outputDir, "qwen.png")); err != nil {
		t.Fatal(err)
	}
	_, err = Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "qwen pass@4 screenshot is not a readable file") {
		t.Fatalf("expected missing qwen screenshot failure, got %v", err)
	}
}

func TestPackageFailsWhenExternalEvidenceContainsSecret(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	secretValue := "raw-result-token"
	if err := os.WriteFile(qwenResult, []byte(`{"model":"qwen3.7-max","trials":4,"pass_count":1,"pass_at_4":0.25,"average_turns":23,"screenshot":"qwen.png","API_TOKEN":"`+secretValue+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "qwen harbor result contains secret-like values") || strings.Contains(err.Error(), secretValue) {
		t.Fatalf("expected redacted result secret failure, got %v", err)
	}
}

func TestPackageFailsWhenTaskContainsSecret(t *testing.T) {
	taskDir := writePackageTask(t)
	if err := os.WriteFile(filepath.Join(taskDir, "notes.md"), []byte("OPENAI_API_KEY=raw-api-value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || strings.Contains(err.Error(), "raw-api-value") {
		t.Fatalf("expected redacted secret failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "sample-task.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("zip should not be created, stat err=%v", statErr)
	}
}

func TestPackageFailsWhenSubmissionContainsSecret(t *testing.T) {
	taskDir := writePackageTask(t)
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "OPENAI_API_KEY=raw-api-value",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || strings.Contains(err.Error(), "raw-api-value") {
		t.Fatalf("expected redacted submission secret failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "sample-task.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("zip should be removed, stat err=%v", statErr)
	}
}

func TestPackageFailsWhenTaskContainsUnexpectedFile(t *testing.T) {
	taskDir := writePackageTask(t)
	if err := os.WriteFile(filepath.Join(taskDir, "notes.md"), []byte("benign extra notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "task_file_set_allowed") {
		t.Fatalf("expected unexpected file failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "sample-task.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("zip should not be created, stat err=%v", statErr)
	}
}

func TestPackageFailsWhenTaskContainsLegacyDomainContent(t *testing.T) {
	taskDir := writePackageTask(t)
	dockerfile := filepath.Join(taskDir, "environment", "Dockerfile")
	raw, err := os.ReadFile(dockerfile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dockerfile, append(raw, []byte("\n# promptflow image2 presentation residue\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	_, err = Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "task_file_set_legacy") {
		t.Fatalf("expected legacy content failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "sample-task.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("zip should not be created, stat err=%v", statErr)
	}
}

func TestValidatePackageFileSetAllowsWordsContainingLegacySubstrings(t *testing.T) {
	taskDir := writePackageTask(t)
	solution := filepath.Join(taskDir, "solution", "solve.sh")
	raw, err := os.ReadFile(solution)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(solution, append(raw, []byte("\n# encoded representation and slider state\n")...), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validatePackageFileSet(taskDir); err != nil {
		t.Fatalf("normal identifier substrings were rejected: %v", err)
	}
}

func TestPackageFailsWhenTaskContainsLegacyDomainFileInAllowedDir(t *testing.T) {
	taskDir := writePackageTask(t)
	if err := os.WriteFile(filepath.Join(taskDir, "environment", "promptflow_runner.py"), []byte("print('legacy')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "task_file_set_allowed") || !strings.Contains(err.Error(), "task_file_set_legacy") {
		t.Fatalf("expected unexpected legacy file failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "sample-task.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("zip should not be created, stat err=%v", statErr)
	}
}

func TestPackageFailsWhenTaskContainsSymlink(t *testing.T) {
	taskDir := writePackageTask(t)
	target := filepath.Join(t.TempDir(), "solve.sh")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nset -eu\nprintf 'apply minimal fix\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	solvePath := filepath.Join(taskDir, "solution", "solve.sh")
	if err := os.Remove(solvePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, solvePath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "task_file_set_regular") {
		t.Fatalf("expected symlink failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "sample-task.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("zip should not be created, stat err=%v", statErr)
	}
}

func TestPackageFailsWhenStrictLintFails(t *testing.T) {
	taskDir := writePackageTask(t)
	if err := os.WriteFile(filepath.Join(taskDir, "tests", "test.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	outputDir := t.TempDir()
	qwenResult, opusResult := writeTrialResults(t, outputDir, taskDir)
	verifyReport := writeVerifyReport(t, outputDir, taskDir, true, true, true)
	similarityReport := writeSimilarityReport(t, outputDir, taskDir, true, []string{"history:/tmp/history"})
	_, err := Package(Options{
		TaskDir:          taskDir,
		OutputDir:        outputDir,
		TaskName:         "sample-task",
		CodeLang:         "go",
		TaskType:         "bug-fix",
		Application:      "backend",
		AHT:              "45 minutes",
		Description:      "sample description",
		GitHubURL:        "https://github.com/org/repo",
		CommitID:         "abc1234",
		VerifyReport:     verifyReport,
		SimilarityReport: similarityReport,
		QwenResult:       qwenResult,
		OpusResult:       opusResult,
		TestsAnalysis:    "tests_analysis.md",
	})
	if err == nil || !strings.Contains(err.Error(), "package lint failed") || !strings.Contains(err.Error(), "test_has_assertion") {
		t.Fatalf("expected strict lint failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "sample-task.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("zip should not be created, stat err=%v", statErr)
	}
}

func writePackageTask(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"environment", "solution", "tests"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"instruction.md": "Fix the visible regression in the public repository at the pinned commit. The verifier runs tests/test.sh after the solution is applied.\n",
		"task.toml": `schema_version = "1.3"

[task]
name = "codeedge/sample"
description = "Fix a reproducible backend regression at a pinned public commit."
keywords = ["go", "backend", "regression"]

[metadata]
code_lang = "go"
task_type = "bug-fix"
application = "backend"
is_0_to_1 = false
github_url = "https://github.com/org/repo"
commit_id = "abc1234"
estimated_aht_minutes = 45
difficulty_explanation = "The task requires tracing a behavior regression, making a minimal code change, and validating it with the provided verifier rather than relying on hidden tests."
target_files = ["pkg/service/service.go"]

[verifier]
timeout_sec = 120

[agent]
timeout_sec = 1800

[environment]
build_timeout_sec = 600
network_mode = "no-network"
os = "linux"
`,
		"environment/Dockerfile": "FROM alpine\nRUN apk add --no-cache git\nRUN git clone https://github.com/org/repo /workspace/repo && cd /workspace/repo && git checkout abc1234\nWORKDIR /workspace/repo\n",
		"solution/solve.sh":      "#!/bin/sh\nset -eu\nprintf 'apply minimal fix\\n'\n",
		"tests/test.sh":          "#!/bin/sh\nset -eu\nprintf 'fixed behavior\\n' > /tmp/actual.txt\ngrep -q 'fixed behavior' /tmp/actual.txt\n",
		"tests_analysis.md":      "## 1. instruction 和 environment 已提供的信息\n- instruction 和 environment 明确给出公开 GitHub 仓库、固定 commit、容器构建方式以及 tests/test.sh 作为 verifier 入口，测试依据来自可见文件而不是隐藏测试。\n\n## 2. 模型的理论通过路径\n- 模型可以阅读 instruction、environment/Dockerfile 和仓库源码，定位目标文件中的回归，应用最小修复后运行 tests/test.sh 验证行为输出是否满足公开 verifier。\n\n## 3. 模型具备通过条件的依据\n- verifier 检查点由 instruction 和 environment 中的公开事实推导，tests/test.sh 使用行为内容断言验证修复效果，不依赖 reward 文件、隐藏测试或不可见服务。\n",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func writeTrialResults(t *testing.T, outputDir, taskDir string) (string, string) {
	t.Helper()
	qwenResult := filepath.Join(outputDir, "qwen.json")
	opusResult := filepath.Join(outputDir, "opus.json")
	if err := os.WriteFile(qwenResult, []byte(packageTrialResultJSON(t, outputDir, taskDir, "qwen3.7-max", []domain.TrialRun{
		{Trial: 1, Turns: 22, Reward: 0},
		{Trial: 2, Passed: true, Turns: 24, Reward: 1},
		{Trial: 3, Turns: 23, Reward: 0},
		{Trial: 4, Turns: 23, Reward: 0},
	}, "qwen.png")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opusResult, []byte(packageTrialResultJSON(t, outputDir, taskDir, "claude-opus-4-6", []domain.TrialRun{
		{Trial: 1, Passed: true, Turns: 28, Reward: 1},
		{Trial: 2, Passed: true, Turns: 29, Reward: 1},
		{Trial: 3, Passed: true, Turns: 27, Reward: 1},
		{Trial: 4, Turns: 28, Reward: 0},
	}, "opus.png")), 0o644); err != nil {
		t.Fatal(err)
	}
	writePackageScreenshot(t, filepath.Join(outputDir, "qwen.png"), "harbor_run_qwen", "qwen3.7-max", 1)
	writePackageScreenshot(t, filepath.Join(outputDir, "opus.png"), "harbor_run_opus", "claude-opus-4-6", 3)
	return qwenResult, opusResult
}

func writePackageScreenshot(t *testing.T, path, slot, model string, passCount int) {
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

func packageTrialResultJSON(t *testing.T, outputDir, taskDir, model string, runs []domain.TrialRun, screenshot string) string {
	t.Helper()
	digest, err := harborrun.ComputeTaskDigest(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	commandPath := filepath.Join(outputDir, strings.NewReplacer("/", "_", ":", "_").Replace(model)+"_command_run.json")
	stdoutPath := strings.TrimSuffix(commandPath, ".json") + "_stdout.txt"
	stderrPath := strings.TrimSuffix(commandPath, ".json") + "_stderr.txt"
	rawResultPath := strings.TrimSuffix(commandPath, ".json") + "_raw_result.json"
	rawResult := []byte(`{"schema_version":"harbor.raw.fixture.v1"}`)
	if err := os.WriteFile(rawResultPath, rawResult, 0o644); err != nil {
		t.Fatal(err)
	}
	commandRun := domain.CommandRun{
		Name:       "harbor_run_" + model,
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
	passCount := 0
	totalTurns := 0
	for _, run := range runs {
		if run.Passed {
			passCount++
		}
		totalTurns += run.Turns
	}
	result := domain.TrialResult{
		SchemaVersion:   "harbor.trial_result.v1",
		Model:           model,
		Agent:           "claude-code",
		Trials:          len(runs),
		PassCount:       passCount,
		PassAt4:         float64(passCount) / float64(len(runs)),
		AverageTurns:    float64(totalTurns) / float64(len(runs)),
		Runs:            runs,
		TaskDigest:      digest,
		RawResultPath:   rawResultPath,
		RawResultSHA256: packageSHA256(rawResult),
		CommandRunPath:  commandPath,
		Screenshot:      screenshot,
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func packageSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeVerifyReport(t *testing.T, outputDir, taskDir string, passed, initialExposesIssue, oraclePassed bool) string {
	t.Helper()
	path := filepath.Join(outputDir, "verify_report.json")
	digest, err := harborrun.ComputeTaskDigest(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	report := map[string]any{
		"schema_version":        "harbor.verify_report.v1",
		"task_dir":              taskDir,
		"task_digest":           digest,
		"image_tag":             "harbor-task-test",
		"passed":                passed,
		"initial_exposes_issue": initialExposesIssue,
		"oracle_verify":         map[string]any{"name": "oracle_verify", "exit_code": 0, "passed": oraclePassed},
		"initial_verify":        map[string]any{"name": "initial_verify", "argv": []string{"docker", "run", "--rm", "-v", filepath.Join(taskDir, "tests") + ":/tests:ro", "harbor-task-test", "/bin/sh", "-c", "/tests/test.sh"}, "exit_code": 1, "passed": !initialExposesIssue},
		"docker_build":          map[string]any{"name": "docker_build", "exit_code": 0, "passed": true},
		"command_logs": []any{
			writeVerifyCommandLog(t, outputDir, "docker_build", []string{"docker", "build", "-t", "harbor-task-test", "-f", filepath.Join(taskDir, "environment", "Dockerfile"), filepath.Join(taskDir, "environment")}, 0, true),
			writeVerifyCommandLog(t, outputDir, "initial_verify", []string{"docker", "run", "--rm", "-v", filepath.Join(taskDir, "tests") + ":/tests:ro", "harbor-task-test", "/bin/sh", "-c", "/tests/test.sh"}, 1, !initialExposesIssue),
			writeVerifyCommandLog(t, outputDir, "oracle_verify", []string{"docker", "run", "--rm", "-v", filepath.Join(taskDir, "solution") + ":/solution:ro", "-v", filepath.Join(taskDir, "tests") + ":/tests:ro", "harbor-task-test", "/bin/sh", "-c", "/solution/solve.sh && /tests/test.sh"}, 0, oraclePassed),
		},
		"created_at": "2026-07-09T00:00:00Z",
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeVerifyCommandLog(t *testing.T, outputDir, name string, argv []string, exitCode int, passed bool) map[string]any {
	t.Helper()
	stdoutRel := filepath.Join("command_logs", name, "stdout.txt")
	stderrRel := filepath.Join("command_logs", name, "stderr.txt")
	stdout := name + " stdout"
	stderr := ""
	if exitCode != 0 {
		stderr = name + " stderr"
	}
	if err := os.MkdirAll(filepath.Join(outputDir, "command_logs", name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, stdoutRel), []byte(stdout), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, stderrRel), []byte(stderr), 0o644); err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"name":        name,
		"argv":        argv,
		"exit_code":   exitCode,
		"stdout":      stdout,
		"stderr":      stderr,
		"stdout_path": stdoutRel,
		"stderr_path": stderrRel,
		"passed":      passed,
	}
}

func mutateVerifyCommandLogs(t *testing.T, path string, mutate func([]map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	rawLogs, ok := report["command_logs"].([]any)
	if !ok {
		t.Fatalf("command_logs has unexpected shape: %+v", report["command_logs"])
	}
	logs := make([]map[string]any, 0, len(rawLogs))
	for _, item := range rawLogs {
		object, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("command log has unexpected shape: %+v", item)
		}
		logs = append(logs, object)
	}
	mutate(logs)
	out := make([]any, 0, len(logs))
	for _, item := range logs {
		out = append(out, item)
	}
	report["command_logs"] = out
	raw, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mutateSimilarityReport(t *testing.T, path string, mutate func(map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	mutate(report)
	raw, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeSimilarityReport(t *testing.T, outputDir, taskDir string, passed bool, sources []string) string {
	t.Helper()
	path := filepath.Join(outputDir, "similarity_report.json")
	digest, err := harborrun.ComputeTaskDigest(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	normalizedSources, sourceEvidence, scanned := writeSimilaritySources(t, outputDir, sources)
	report := domain.SimilarityReport{
		SchemaVersion:     "harbor.similarity_report.v1",
		TaskDir:           "task",
		TaskDigest:        digest,
		RepoURL:           "https://github.com/org/repo",
		Sources:           normalizedSources,
		SuccessfulSources: normalizedSources,
		ScannedFileCount:  scanned,
		SourceEvidence:    sourceEvidence,
		Threshold:         0.42,
		OverallPass:       passed,
	}
	if !passed {
		report.Issues = []string{"duplicate-risk candidates found"}
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeSimilaritySources(t *testing.T, outputDir string, sources []string) ([]string, []domain.SimilaritySourceEvidence, int) {
	t.Helper()
	var normalized []string
	var evidence []domain.SimilaritySourceEvidence
	scanned := 0
	for idx, source := range sources {
		switch {
		case source == "github":
			normalized = append(normalized, source)
			evidence = append(evidence, domain.SimilaritySourceEvidence{
				Source:       "github",
				Kind:         "github",
				QueryCount:   1,
				ResultCount:  0,
				HTTPStatuses: []int{200},
			})
		case strings.HasPrefix(source, "history:"):
			dir := ensureSimilaritySourceDir(t, outputDir, "history", idx, strings.TrimPrefix(source, "history:"))
			item, err := similaritycheck.BuildLocalSourceEvidence("history", dir)
			if err != nil {
				t.Fatal(err)
			}
			normalized = append(normalized, item.Source)
			evidence = append(evidence, item)
			scanned += item.ScannedFileCount
		case strings.HasPrefix(source, "tb3:"):
			dir := ensureSimilaritySourceDir(t, outputDir, "tb3", idx, strings.TrimPrefix(source, "tb3:"))
			item, err := similaritycheck.BuildLocalSourceEvidence("tb3", dir)
			if err != nil {
				t.Fatal(err)
			}
			normalized = append(normalized, item.Source)
			evidence = append(evidence, item)
			scanned += item.ScannedFileCount
		default:
			normalized = append(normalized, source)
		}
	}
	return normalized, evidence, scanned
}

func ensureSimilaritySourceDir(t *testing.T, outputDir, kind string, idx int, configured string) string {
	t.Helper()
	dir := strings.TrimSpace(configured)
	if dir == "" || strings.HasPrefix(dir, "/tmp/") || strings.HasPrefix(dir, "/var/") {
		dir = filepath.Join(outputDir, "similarity_sources", kind, string(rune('a'+idx)))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "unrelated.md")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		content := "Unrelated historical Harbor task note about desktop widgets and cache telemetry with no overlapping verifier behavior.\n"
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
