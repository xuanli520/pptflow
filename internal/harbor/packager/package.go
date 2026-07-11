package packager

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/harbor/lint"
	"github.com/purplevoid/harbor-factory/internal/harbor/repourl"
	"github.com/purplevoid/harbor-factory/internal/harbor/secretscan"
	similaritycheck "github.com/purplevoid/harbor-factory/internal/harbor/similarity"
)

type Options struct {
	TaskDir          string
	OutputDir        string
	TaskName         string
	CodeLang         string
	TaskType         string
	Application      string
	AHT              string
	Description      string
	IsZeroToOne      bool
	GitHubURL        string
	CommitID         string
	TestsAnalysis    string
	VerifyReport     string
	QualityReport    string
	SimilarityReport string
	QwenResult       string
	OpusResult       string
	QwenScreenshot   string
	OpusScreenshot   string
}

func Package(opts Options) (domain.PackageReport, error) {
	taskDir := strings.TrimSpace(opts.TaskDir)
	if taskDir == "" {
		return domain.PackageReport{}, fmt.Errorf("task directory is required")
	}
	info, err := os.Stat(taskDir)
	if err != nil || !info.IsDir() {
		return domain.PackageReport{}, fmt.Errorf("task directory does not exist: %s", taskDir)
	}
	taskName := strings.TrimSpace(opts.TaskName)
	if taskName == "" {
		taskName = filepath.Base(taskDir)
	}
	taskName, err = validatePackageTaskName(taskName)
	if err != nil {
		return domain.PackageReport{}, err
	}
	outputDir := strings.TrimSpace(opts.OutputDir)
	if outputDir == "" {
		outputDir = filepath.Join(".harbor-factory", "output")
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return domain.PackageReport{}, err
	}
	if err := validateSubmissionFields(opts); err != nil {
		return domain.PackageReport{}, err
	}
	testsAnalysis, err := requireReadableEvidence("tests_analysis", opts.TestsAnalysis, taskDir)
	if err != nil {
		return domain.PackageReport{}, err
	}
	testsAnalysis, err = requireMatchingRootTestsAnalysis(taskDir, testsAnalysis)
	if err != nil {
		return domain.PackageReport{}, err
	}
	if err := ensureNoEvidenceSecrets("tests_analysis", testsAnalysis); err != nil {
		return domain.PackageReport{}, err
	}
	if err := ensureNoEvidenceLegacy("tests_analysis", testsAnalysis); err != nil {
		return domain.PackageReport{}, err
	}
	verifyReport, err := requireReadableEvidence("verify report", opts.VerifyReport, taskDir)
	if err != nil {
		return domain.PackageReport{}, err
	}
	if err := ensureNoEvidenceSecrets("verify report", verifyReport); err != nil {
		return domain.PackageReport{}, err
	}
	if err := validateVerifyReport(verifyReport, taskDir); err != nil {
		return domain.PackageReport{}, err
	}
	similarityReport, err := requireReadableEvidence("similarity report", opts.SimilarityReport, taskDir)
	if err != nil {
		return domain.PackageReport{}, err
	}
	if err := ensureNoEvidenceSecrets("similarity report", similarityReport); err != nil {
		return domain.PackageReport{}, err
	}
	similarity, err := validateSimilarityReport(similarityReport, taskDir)
	if err != nil {
		return domain.PackageReport{}, err
	}
	if err := ensureNoDirSecrets(taskDir); err != nil {
		return domain.PackageReport{}, err
	}
	qwen, err := parseTrial("qwen", opts.QwenResult)
	if err != nil {
		return domain.PackageReport{}, err
	}
	if err := ensureNoEvidenceSecrets("qwen harbor result", opts.QwenResult); err != nil {
		return domain.PackageReport{}, err
	}
	if err := validateTrial("qwen", qwen, true, taskDir); err != nil {
		return domain.PackageReport{}, err
	}
	opus, err := parseTrial("opus", opts.OpusResult)
	if err != nil {
		return domain.PackageReport{}, err
	}
	if err := ensureNoEvidenceSecrets("opus harbor result", opts.OpusResult); err != nil {
		return domain.PackageReport{}, err
	}
	if err := validateTrial("opus", opus, false, taskDir); err != nil {
		return domain.PackageReport{}, err
	}
	qwenScreenshot := firstNonEmpty(opts.QwenScreenshot, qwen.Screenshot)
	opusScreenshot := firstNonEmpty(opts.OpusScreenshot, opus.Screenshot)
	qwenScreenshot, err = requireReadableEvidence("qwen pass@4 screenshot", qwenScreenshot, filepath.Dir(opts.QwenResult), taskDir)
	if err != nil {
		return domain.PackageReport{}, err
	}
	opusScreenshot, err = requireReadableEvidence("opus pass@4 screenshot", opusScreenshot, filepath.Dir(opts.OpusResult), taskDir)
	if err != nil {
		return domain.PackageReport{}, err
	}
	if err := validateScreenshotEvidence("qwen pass@4 screenshot", qwenScreenshot); err != nil {
		return domain.PackageReport{}, err
	}
	if err := validateScreenshotEvidence("opus pass@4 screenshot", opusScreenshot); err != nil {
		return domain.PackageReport{}, err
	}
	if qwenScreenshot == opusScreenshot {
		return domain.PackageReport{}, fmt.Errorf("Qwen and Opus pass@4 screenshots must be distinct files")
	}
	if err := ensureLintPasses(taskDir, opts, testsAnalysis, qwenScreenshot, opusScreenshot); err != nil {
		return domain.PackageReport{}, err
	}
	if err := validatePackageFileSet(taskDir); err != nil {
		return domain.PackageReport{}, err
	}
	zipPath := filepath.Join(outputDir, taskName+".zip")
	if err := writeZip(taskDir, taskName, zipPath); err != nil {
		return domain.PackageReport{}, err
	}
	if err := ensureNoZipSecrets(zipPath); err != nil {
		_ = os.Remove(zipPath)
		return domain.PackageReport{}, err
	}
	submissionPath := filepath.Join(outputDir, "submission_report.json")
	submission := map[string]any{
		"schema_version":        "harbor.submission_report.v1",
		"task_name":             taskName,
		"code_lang":             opts.CodeLang,
		"task_type":             opts.TaskType,
		"application":           opts.Application,
		"aht":                   opts.AHT,
		"one_line_description":  opts.Description,
		"is_0_to_1":             opts.IsZeroToOne,
		"github_url":            opts.GitHubURL,
		"commit_id":             opts.CommitID,
		"tests_analysis":        testsAnalysis,
		"verify_report":         verifyReport,
		"quality_report":        opts.QualityReport,
		"similarity_report":     similarityReport,
		"similarity_sources":    similarity.Sources,
		"similarity_passed":     similarity.OverallPass,
		"similarity_max_score":  similarity.MaxScore,
		"qwen_result":           opts.QwenResult,
		"opus_result":           opts.OpusResult,
		"qwen_pass4_screenshot": qwenScreenshot,
		"opus_pass4_screenshot": opusScreenshot,
		"pass_at_4": map[string]float64{
			"qwen": qwen.PassAt4,
			"opus": opus.PassAt4,
		},
		"pass_count": map[string]int{
			"qwen": qwen.PassCount,
			"opus": opus.PassCount,
		},
		"average_turns": map[string]float64{
			"qwen": qwen.AverageTurns,
			"opus": opus.AverageTurns,
		},
		"created_at": time.Now().UTC(),
	}
	data, err := json.MarshalIndent(submission, "", "  ")
	if err != nil {
		return domain.PackageReport{}, err
	}
	if err := ensureNoSubmissionSecrets(data); err != nil {
		_ = os.Remove(zipPath)
		return domain.PackageReport{}, err
	}
	if err := os.WriteFile(submissionPath, append(data, '\n'), 0o644); err != nil {
		return domain.PackageReport{}, err
	}
	return domain.PackageReport{
		SchemaVersion: "harbor.package_report.v1",
		TaskDir:       taskDir,
		OutputZip:     zipPath,
		ReportPath:    submissionPath,
		TaskName:      taskName,
		CreatedAt:     time.Now().UTC(),
		Passed:        true,
	}, nil
}

func ensureNoDirSecrets(taskDir string) error {
	findings, err := secretscan.ScanDir(taskDir)
	if err != nil {
		return fmt.Errorf("task secret scan failed: %w", err)
	}
	if len(findings) > 0 {
		return fmt.Errorf("task contains secret-like values: %s", secretscan.Summary(findings, 5))
	}
	return nil
}

func ensureNoZipSecrets(zipPath string) error {
	findings, err := secretscan.ScanZip(zipPath)
	if err != nil {
		return fmt.Errorf("zip secret scan failed: %w", err)
	}
	if len(findings) > 0 {
		return fmt.Errorf("zip contains secret-like values: %s", secretscan.Summary(findings, 5))
	}
	return nil
}

func ensureNoSubmissionSecrets(data []byte) error {
	findings := secretscan.ScanBytes("submission_report.json", data)
	if len(findings) > 0 {
		return fmt.Errorf("submission report contains secret-like values: %s", secretscan.Summary(findings, 5))
	}
	return nil
}

func ensureNoEvidenceSecrets(label, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s cannot be read for secret scan: %w", label, err)
	}
	findings := secretscan.ScanBytes(filepath.Base(path), raw)
	if len(findings) > 0 {
		return fmt.Errorf("%s contains secret-like values: %s", label, secretscan.Summary(findings, 5))
	}
	return nil
}

func ensureNoEvidenceLegacy(label, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s cannot be read for legacy residue scan: %w", label, err)
	}
	if legacyDomainMatch(string(raw)) {
		return fmt.Errorf("%s contains legacy non-Harbor domain content", label)
	}
	return nil
}

func requireMatchingRootTestsAnalysis(taskDir, evidencePath string) (string, error) {
	rootPath := filepath.Join(taskDir, "tests_analysis.md")
	rootInfo, err := os.Stat(rootPath)
	if err != nil {
		return "", fmt.Errorf("task root tests_analysis.md is required: %w", err)
	}
	if !rootInfo.Mode().IsRegular() {
		return "", fmt.Errorf("task root tests_analysis.md must be a regular file")
	}
	rootAbs, err := filepath.Abs(rootPath)
	if err != nil {
		rootAbs = filepath.Clean(rootPath)
	}
	evidenceAbs, err := filepath.Abs(evidencePath)
	if err != nil {
		evidenceAbs = filepath.Clean(evidencePath)
	}
	if rootAbs == evidenceAbs {
		return rootAbs, nil
	}
	rootRaw, err := os.ReadFile(rootAbs)
	if err != nil {
		return "", fmt.Errorf("task root tests_analysis.md cannot be read: %w", err)
	}
	evidenceRaw, err := os.ReadFile(evidenceAbs)
	if err != nil {
		return "", fmt.Errorf("tests_analysis evidence cannot be read: %w", err)
	}
	if !bytes.Equal(rootRaw, evidenceRaw) {
		return "", fmt.Errorf("tests_analysis evidence must match task root tests_analysis.md")
	}
	return rootAbs, nil
}

func ValidateVerifyReport(path, taskDir string) error {
	return validateVerifyReport(path, taskDir)
}

func validateVerifyReport(path, taskDir string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("verify report cannot be read: %w", err)
	}
	var report domain.VerifyReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return fmt.Errorf("verify report cannot be parsed: %w", err)
	}
	if strings.TrimSpace(report.TaskDigest) == "" {
		return fmt.Errorf("verify report task_digest is required")
	}
	digest, err := harborrun.ComputeTaskDigest(taskDir)
	if err != nil {
		return fmt.Errorf("current task digest cannot be computed: %w", err)
	}
	if !strings.EqualFold(report.TaskDigest, digest) {
		return fmt.Errorf("verify report task_digest does not match current task")
	}
	if !report.Passed {
		return fmt.Errorf("verify report did not pass")
	}
	if report.DockerBuild == nil || !report.DockerBuild.Passed {
		return fmt.Errorf("verify report does not prove docker build passed")
	}
	if report.InitialVerify == nil || report.InitialVerify.ExitCode == 0 || !report.InitialExposesIssue {
		return fmt.Errorf("verify report does not prove initial verification exposes the issue")
	}
	if !report.InitialExposesIssue {
		return fmt.Errorf("verify report does not prove initial verification exposes the issue")
	}
	if report.OracleVerify == nil || !report.OracleVerify.Passed {
		return fmt.Errorf("verify report does not prove oracle verification passed")
	}
	if err := validateVerifyCommandLogs(report, filepath.Dir(path)); err != nil {
		return err
	}
	return nil
}

func validateVerifyCommandLogs(report domain.VerifyReport, evidenceRoot string) error {
	if len(report.CommandLogs) < 3 {
		return fmt.Errorf("verify report command_logs must include docker_build, initial_verify, and oracle_verify")
	}
	required := map[string]bool{
		"docker_build":   false,
		"initial_verify": false,
		"oracle_verify":  false,
	}
	for _, run := range report.CommandLogs {
		name := strings.TrimSpace(run.Name)
		if _, ok := required[name]; !ok {
			continue
		}
		if len(run.Argv) == 0 {
			return fmt.Errorf("verify report command log %s is missing argv provenance", name)
		}
		if err := validateVerifyCommandOutputFiles(run, evidenceRoot); err != nil {
			return err
		}
		switch name {
		case "docker_build":
			if !run.Passed {
				return fmt.Errorf("verify report command log docker_build did not pass")
			}
			if !verifyDockerBuildArgv(run.Argv, report) {
				return fmt.Errorf("verify report command log docker_build argv does not prove docker build")
			}
		case "initial_verify":
			if run.ExitCode == 0 {
				return fmt.Errorf("verify report command log initial_verify should expose the issue")
			}
			if !verifyInitialArgv(run.Argv, report) {
				return fmt.Errorf("verify report command log initial_verify argv does not prove tests-only initial verification")
			}
		case "oracle_verify":
			if !run.Passed {
				return fmt.Errorf("verify report command log oracle_verify did not pass")
			}
			if !verifyOracleArgv(run.Argv, report) {
				return fmt.Errorf("verify report command log oracle_verify argv does not prove solution plus tests verification")
			}
		}
		required[name] = true
	}
	for name, found := range required {
		if !found {
			return fmt.Errorf("verify report command_logs missing %s", name)
		}
	}
	return nil
}

func validateVerifyCommandOutputFiles(run domain.CommandRun, evidenceRoot string) error {
	if err := validateVerifyCommandOutputFile(run.Name, "stdout_path", run.StdoutPath, run.Stdout, evidenceRoot); err != nil {
		return err
	}
	if err := validateVerifyCommandOutputFile(run.Name, "stderr_path", run.StderrPath, run.Stderr, evidenceRoot); err != nil {
		return err
	}
	return nil
}

func validateVerifyCommandOutputFile(commandName, label, outputPath, expected, evidenceRoot string) error {
	outputPath = strings.TrimSpace(outputPath)
	if outputPath == "" {
		return fmt.Errorf("verify report command log %s %s is required", commandName, label)
	}
	resolved := resolveVerifyOutputPath(outputPath, evidenceRoot)
	if !pathWithinDir(resolved, evidenceRoot) {
		return fmt.Errorf("verify report command log %s %s must stay under verify report directory", commandName, label)
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() {
		return fmt.Errorf("verify report command log %s %s is not a readable file", commandName, label)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return fmt.Errorf("verify report command log %s %s cannot be read: %w", commandName, label, err)
	}
	if findings := secretscan.ScanBytes(filepath.Base(resolved), raw); len(findings) > 0 {
		return fmt.Errorf("verify report command log %s %s contains secret-like values: %s", commandName, label, secretscan.Summary(findings, 3))
	}
	if string(raw) != expected {
		return fmt.Errorf("verify report command log %s %s content does not match command log %s", commandName, label, strings.TrimSuffix(label, "_path"))
	}
	return nil
}

func resolveVerifyOutputPath(path, evidenceRoot string) string {
	if filepath.IsAbs(path) {
		return path
	}
	if pathWithinDir(path, evidenceRoot) {
		return path
	}
	return filepath.Join(evidenceRoot, path)
}

func verifyDockerBuildArgv(argv []string, report domain.VerifyReport) bool {
	taskDir := strings.TrimSpace(report.TaskDir)
	if argvHasPrefix(argv, "docker", "build") {
		if strings.TrimSpace(report.ImageTag) != "" && !argvFlagValue(argv, "-t", report.ImageTag) {
			return false
		}
		return argvFlagPath(argv, "-f", "", filepath.Join(taskDir, "environment", "Dockerfile")) &&
			argvHasPath(argv, "", filepath.Join(taskDir, "environment"))
	}
	if argvHasSubsequence(argv, "docker", "compose") && argvContains(argv, "build") {
		return argvFlagPath(argv, "-f", "", filepath.Join(taskDir, "environment", "docker-compose.yaml")) &&
			argvFlagPath(argv, "--project-directory", "", taskDir) &&
			argvContainsExact(argv, "main")
	}
	return false
}

func verifyInitialArgv(argv []string, report domain.VerifyReport) bool {
	taskDir := strings.TrimSpace(report.TaskDir)
	if !argvContains(argv, "/tests/test.sh") || argvContains(argv, "/solution") || argvContains(argv, "solve.sh") {
		return false
	}
	if argvHasSubsequence(argv, "docker", "run") {
		return strings.TrimSpace(report.ImageTag) != "" &&
			argvContainsExact(argv, report.ImageTag) &&
			argvHasReadOnlyVolume(argv, filepath.Join(taskDir, "tests"), "/tests")
	}
	if argvHasSubsequence(argv, "docker", "compose", "run") {
		return argvFlagPath(argv, "-f", "", filepath.Join(taskDir, "environment", "docker-compose.yaml")) &&
			argvFlagPath(argv, "--project-directory", "", taskDir) &&
			argvHasReadOnlyVolume(argv, filepath.Join(taskDir, "tests"), "/tests") &&
			argvContainsExact(argv, "main")
	}
	return false
}

func verifyOracleArgv(argv []string, report domain.VerifyReport) bool {
	taskDir := strings.TrimSpace(report.TaskDir)
	if !argvContains(argv, "/solution/solve.sh") || !argvContains(argv, "/tests/test.sh") {
		return false
	}
	if argvHasSubsequence(argv, "docker", "run") {
		return strings.TrimSpace(report.ImageTag) != "" &&
			argvContainsExact(argv, report.ImageTag) &&
			argvHasReadOnlyVolume(argv, filepath.Join(taskDir, "solution"), "/solution") &&
			argvHasReadOnlyVolume(argv, filepath.Join(taskDir, "tests"), "/tests")
	}
	if argvHasSubsequence(argv, "docker", "compose", "run") {
		return argvFlagPath(argv, "-f", "", filepath.Join(taskDir, "environment", "docker-compose.yaml")) &&
			argvFlagPath(argv, "--project-directory", "", taskDir) &&
			argvHasReadOnlyVolume(argv, filepath.Join(taskDir, "solution"), "/solution") &&
			argvHasReadOnlyVolume(argv, filepath.Join(taskDir, "tests"), "/tests") &&
			argvContainsExact(argv, "main")
	}
	return false
}

func validatePackageFileSet(taskDir string) error {
	seen := map[string]bool{}
	if err := filepath.WalkDir(taskDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			rel, _ := filepath.Rel(taskDir, path)
			return fmt.Errorf("symlink is not allowed in Harbor package: %s", filepath.ToSlash(rel))
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			rel, _ := filepath.Rel(taskDir, path)
			return fmt.Errorf("non-regular file is not allowed in Harbor package: %s", filepath.ToSlash(rel))
		}
		rel, err := filepath.Rel(taskDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !isAllowedPackageFile(rel) {
			return fmt.Errorf("unexpected file in Harbor package: %s", rel)
		}
		seen[rel] = true
		if legacyDomainMatch(rel) {
			return fmt.Errorf("legacy non-Harbor domain file is not allowed in package: %s", rel)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if legacyDomainMatch(string(raw)) {
			return fmt.Errorf("legacy non-Harbor domain content is not allowed in package: %s", rel)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, rel := range requiredPackageFiles() {
		if !seen[rel] {
			return fmt.Errorf("missing required Harbor package file: %s", rel)
		}
	}
	return nil
}

func legacyDomainMatch(value string) bool {
	lower := strings.ToLower(value)
	for _, term := range []string{
		"pptflow",
		"promptflow",
		"image2",
		"powerpoint",
		"presentation",
		"slide",
	} {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

func isAllowedPackageFile(rel string) bool {
	switch rel {
	case "instruction.md",
		"task.toml",
		"tests_analysis.md",
		"environment/Dockerfile",
		"environment/docker-compose.yaml",
		"solution/solve.sh",
		"tests/test.sh":
		return true
	}
	return false
}

func requiredPackageFiles() []string {
	return []string{
		"instruction.md",
		"task.toml",
		"tests_analysis.md",
		"solution/solve.sh",
		"tests/test.sh",
	}
}

func ValidateSimilarityReport(path, taskDir string) (domain.SimilarityReport, error) {
	return validateSimilarityReport(path, taskDir)
}

func validateSimilarityReport(path, taskDir string) (domain.SimilarityReport, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.SimilarityReport{}, fmt.Errorf("similarity report cannot be read: %w", err)
	}
	var report domain.SimilarityReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return domain.SimilarityReport{}, fmt.Errorf("similarity report cannot be parsed: %w", err)
	}
	if strings.TrimSpace(report.TaskDigest) == "" {
		return domain.SimilarityReport{}, fmt.Errorf("similarity report task_digest is required")
	}
	digest, err := harborrun.ComputeTaskDigest(taskDir)
	if err != nil {
		return domain.SimilarityReport{}, fmt.Errorf("current task digest cannot be computed: %w", err)
	}
	if !strings.EqualFold(report.TaskDigest, digest) {
		return domain.SimilarityReport{}, fmt.Errorf("similarity report task_digest does not match current task")
	}
	if len(report.Sources) == 0 {
		return domain.SimilarityReport{}, fmt.Errorf("similarity report has no configured sources")
	}
	if len(report.SuccessfulSources) == 0 {
		return domain.SimilarityReport{}, fmt.Errorf("similarity report has no successfully scanned sources")
	}
	if err := validateSimilaritySources(report, filepath.Dir(path)); err != nil {
		return domain.SimilarityReport{}, err
	}
	threshold := report.Threshold
	if threshold <= 0 {
		threshold = 0.42
	}
	if threshold > 0.42 {
		return domain.SimilarityReport{}, fmt.Errorf("similarity report threshold %.3f exceeds factory maximum 0.420", threshold)
	}
	if report.MaxScore >= threshold {
		return domain.SimilarityReport{}, fmt.Errorf("similarity report max_score exceeds threshold")
	}
	if !report.OverallPass {
		return domain.SimilarityReport{}, fmt.Errorf("similarity report did not pass")
	}
	if len(report.Issues) > 0 {
		return domain.SimilarityReport{}, fmt.Errorf("similarity report contains unresolved issues")
	}
	return report, nil
}

func validateSimilaritySources(report domain.SimilarityReport, reportDir string) error {
	configured := map[string]bool{}
	evidenceBySource := map[string]domain.SimilaritySourceEvidence{}
	hasLocalSuccess := false
	hasGitHubSuccess := false
	localEvidenceScanned := 0
	for _, source := range report.Sources {
		source = strings.TrimSpace(source)
		if !validSimilaritySource(source) {
			return fmt.Errorf("similarity report has unsupported source: %s", source)
		}
		configured[source] = true
	}
	for _, evidence := range report.SourceEvidence {
		source := strings.TrimSpace(evidence.Source)
		if source == "" {
			return fmt.Errorf("similarity report source_evidence has empty source")
		}
		if !validSimilaritySource(source) {
			return fmt.Errorf("similarity report source_evidence has unsupported source: %s", source)
		}
		if !configured[source] {
			return fmt.Errorf("similarity report source_evidence source was not configured: %s", source)
		}
		if _, ok := localSimilaritySource(source); !ok && source != "github" {
			return fmt.Errorf("similarity report source_evidence is only supported for local or github sources: %s", source)
		}
		if evidenceBySource[source].Source != "" {
			return fmt.Errorf("similarity report source_evidence duplicates source: %s", source)
		}
		evidenceBySource[source] = evidence
	}
	for _, source := range report.SuccessfulSources {
		source = strings.TrimSpace(source)
		if !validSimilaritySource(source) {
			return fmt.Errorf("similarity report has unsupported successful source: %s", source)
		}
		if !configured[source] {
			return fmt.Errorf("similarity report successful source was not configured: %s", source)
		}
		if kind, ok := localSimilaritySource(source); ok {
			hasLocalSuccess = true
			evidence, ok := evidenceBySource[source]
			if !ok {
				return fmt.Errorf("similarity report local successful source requires source_evidence: %s", source)
			}
			scanned, err := validateLocalSimilarityEvidence(kind, source, evidence, reportDir)
			if err != nil {
				return err
			}
			localEvidenceScanned += scanned
		}
		if source == "github" {
			hasGitHubSuccess = true
			evidence, ok := evidenceBySource[source]
			if !ok {
				return fmt.Errorf("similarity report github successful source requires source_evidence")
			}
			if err := validateGitHubSimilarityEvidence(evidence); err != nil {
				return err
			}
		}
	}
	if hasLocalSuccess && report.ScannedFileCount <= 0 {
		return fmt.Errorf("similarity report local successful source requires scanned_file_count > 0")
	}
	if hasLocalSuccess && localEvidenceScanned != report.ScannedFileCount {
		return fmt.Errorf("similarity report scanned_file_count does not match local source_evidence")
	}
	if hasGitHubSuccess && strings.TrimSpace(report.RepoURL) == "" {
		return fmt.Errorf("similarity report github successful source requires repo_url")
	}
	if hasGitHubSuccess {
		if err := repourl.RejectCredentials(report.RepoURL); err != nil {
			return fmt.Errorf("similarity report github repo_url is invalid: %w", err)
		}
		if !repourl.IsGitHubRepo(report.RepoURL) {
			return fmt.Errorf("similarity report github successful source requires GitHub repo_url")
		}
	}
	return nil
}

func validateGitHubSimilarityEvidence(evidence domain.SimilaritySourceEvidence) error {
	if strings.TrimSpace(evidence.Kind) != "github" {
		return fmt.Errorf("similarity report github source_evidence kind must be github")
	}
	if evidence.QueryCount <= 0 {
		return fmt.Errorf("similarity report github source_evidence query_count must be > 0")
	}
	if evidence.ResultCount < 0 {
		return fmt.Errorf("similarity report github source_evidence result_count must be >= 0")
	}
	if len(evidence.HTTPStatuses) == 0 {
		return fmt.Errorf("similarity report github source_evidence http_statuses is required")
	}
	for _, status := range evidence.HTTPStatuses {
		if status < 200 || status >= 300 {
			return fmt.Errorf("similarity report github source_evidence contains non-success HTTP status: %d", status)
		}
	}
	return nil
}

func validateLocalSimilarityEvidence(kind, source string, evidence domain.SimilaritySourceEvidence, reportDir string) (int, error) {
	if strings.TrimSpace(evidence.Kind) != kind {
		return 0, fmt.Errorf("similarity report source_evidence kind does not match source: %s", source)
	}
	if evidence.ScannedFileCount <= 0 {
		return 0, fmt.Errorf("similarity report source_evidence scanned_file_count must be > 0: %s", source)
	}
	if strings.TrimSpace(evidence.SourceDigest) == "" {
		return 0, fmt.Errorf("similarity report source_evidence source_digest is required: %s", source)
	}
	evidencePath := strings.TrimSpace(evidence.Path)
	if evidencePath == "" {
		return 0, fmt.Errorf("similarity report source_evidence path is required: %s", source)
	}
	sourcePath := strings.TrimPrefix(source, kind+":")
	if strings.TrimSpace(sourcePath) == "" {
		return 0, fmt.Errorf("similarity report local source path is required: %s", source)
	}
	if !sameLocalSourcePath(evidencePath, sourcePath, reportDir) {
		return 0, fmt.Errorf("similarity report source_evidence path does not match source: %s", source)
	}
	recomputed, err := similaritycheck.BuildLocalSourceEvidence(kind, evidencePath)
	if err != nil {
		return 0, fmt.Errorf("similarity report source_evidence cannot be recomputed for %s: %w", source, err)
	}
	if recomputed.ScannedFileCount != evidence.ScannedFileCount {
		return 0, fmt.Errorf("similarity report source_evidence scanned_file_count does not match current source: %s", source)
	}
	if recomputed.SourceDigest != evidence.SourceDigest {
		return 0, fmt.Errorf("similarity report source_evidence source_digest does not match current source: %s", source)
	}
	return evidence.ScannedFileCount, nil
}

func localSimilaritySource(source string) (string, bool) {
	source = strings.TrimSpace(source)
	for _, kind := range []string{"history", "tb3"} {
		if strings.HasPrefix(source, kind+":") && strings.TrimSpace(strings.TrimPrefix(source, kind+":")) != "" {
			return kind, true
		}
	}
	return "", false
}

func sameLocalSourcePath(evidencePath, sourcePath, reportDir string) bool {
	if sameFilesystemPath(evidencePath, "", sourcePath) {
		return true
	}
	if !filepath.IsAbs(sourcePath) && strings.TrimSpace(reportDir) != "" {
		return sameFilesystemPath(evidencePath, "", filepath.Join(reportDir, sourcePath))
	}
	return false
}

func validSimilaritySource(source string) bool {
	source = strings.TrimSpace(source)
	return source == "github" || strings.HasPrefix(source, "history:") || strings.HasPrefix(source, "tb3:")
}

func argvHasPrefix(argv []string, values ...string) bool {
	if len(argv) < len(values) {
		return false
	}
	for i, value := range values {
		if strings.TrimSpace(argv[i]) != value {
			return false
		}
	}
	return true
}

func argvHasSubsequence(argv []string, values ...string) bool {
	if len(values) == 0 {
		return true
	}
	for start := 0; start+len(values) <= len(argv); start++ {
		ok := true
		for i, value := range values {
			if strings.TrimSpace(argv[start+i]) != value {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func argvContains(argv []string, needle string) bool {
	for _, item := range argv {
		if strings.Contains(strings.TrimSpace(item), needle) {
			return true
		}
	}
	return false
}

func argvContainsExact(argv []string, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, item := range argv {
		if strings.TrimSpace(item) == value {
			return true
		}
	}
	return false
}

func argvFlagValue(argv []string, flag, value string) bool {
	got, ok := argvFlagText(argv, flag)
	return ok && strings.TrimSpace(got) == strings.TrimSpace(value)
}

func argvFlagPath(argv []string, flag, cwd, expected string) bool {
	got, ok := argvFlagText(argv, flag)
	return ok && sameFilesystemPath(got, cwd, expected)
}

func argvHasPath(argv []string, cwd, expected string) bool {
	for _, item := range argv {
		if sameFilesystemPath(item, cwd, expected) {
			return true
		}
	}
	return false
}

func argvFlagText(argv []string, flag string) (string, bool) {
	for i := 0; i < len(argv); i++ {
		item := strings.TrimSpace(argv[i])
		if item == flag && i+1 < len(argv) {
			return strings.TrimSpace(argv[i+1]), true
		}
		if strings.HasPrefix(item, flag+"=") {
			return strings.TrimSpace(strings.TrimPrefix(item, flag+"=")), true
		}
	}
	return "", false
}

func argvHasReadOnlyVolume(argv []string, source, target string) bool {
	for i := 0; i < len(argv); i++ {
		item := strings.TrimSpace(argv[i])
		var spec string
		switch {
		case item == "-v" || item == "--volume":
			if i+1 >= len(argv) {
				continue
			}
			spec = strings.TrimSpace(argv[i+1])
		case strings.HasPrefix(item, "-v="):
			spec = strings.TrimSpace(strings.TrimPrefix(item, "-v="))
		case strings.HasPrefix(item, "--volume="):
			spec = strings.TrimSpace(strings.TrimPrefix(item, "--volume="))
		default:
			continue
		}
		if volumeSpecMatches(spec, source, target) {
			return true
		}
	}
	return false
}

func volumeSpecMatches(spec, source, target string) bool {
	parts := strings.Split(spec, ":")
	if len(parts) < 3 {
		return false
	}
	mode := parts[len(parts)-1]
	mountTarget := parts[len(parts)-2]
	mountSource := strings.Join(parts[:len(parts)-2], ":")
	return strings.Contains(mode, "ro") &&
		strings.TrimSpace(mountTarget) == target &&
		sameFilesystemPath(mountSource, "", source)
}

func sameFilesystemPath(actual, cwd, expected string) bool {
	actual = strings.TrimSpace(actual)
	expected = strings.TrimSpace(expected)
	if actual == "" || expected == "" {
		return false
	}
	if !filepath.IsAbs(actual) && strings.TrimSpace(cwd) != "" {
		actual = filepath.Join(cwd, actual)
	}
	actualAbs, err := filepath.Abs(actual)
	if err != nil {
		return false
	}
	expectedAbs, err := filepath.Abs(expected)
	if err != nil {
		return false
	}
	if evaluated, err := filepath.EvalSymlinks(actualAbs); err == nil {
		actualAbs = evaluated
	}
	if evaluated, err := filepath.EvalSymlinks(expectedAbs); err == nil {
		expectedAbs = evaluated
	}
	return filepath.Clean(actualAbs) == filepath.Clean(expectedAbs)
}

func pathWithinDir(path, dir string) bool {
	path = strings.TrimSpace(path)
	dir = strings.TrimSpace(dir)
	if path == "" || dir == "" {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	if evaluated, err := filepath.EvalSymlinks(absPath); err == nil {
		absPath = evaluated
	}
	if evaluated, err := filepath.EvalSymlinks(absDir); err == nil {
		absDir = evaluated
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func validateScreenshotEvidence(label, path string) error {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return fmt.Errorf("%s is not a readable file: %s", label, path)
	}
	if info.Size() == 0 {
		return fmt.Errorf("%s is empty: %s", label, path)
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".webp":
		return nil
	default:
		return fmt.Errorf("%s must be a png, jpg, jpeg, or webp file: %s", label, path)
	}
}

func ensureLintPasses(taskDir string, opts Options, testsAnalysis, qwenScreenshot, opusScreenshot string) error {
	report, err := lint.Run(context.Background(), lint.Options{
		TaskDir:          taskDir,
		RepoURL:          opts.GitHubURL,
		Commit:           opts.CommitID,
		QwenResult:       opts.QwenResult,
		OpusResult:       opts.OpusResult,
		QwenScreenshot:   qwenScreenshot,
		OpusScreenshot:   opusScreenshot,
		TestsAnalysis:    testsAnalysis,
		StrictSubmission: true,
	})
	if err != nil {
		return fmt.Errorf("package lint failed: %w", err)
	}
	if !report.Passed {
		return fmt.Errorf("package lint failed: %s", lintFailureSummary(report.Checks, 5))
	}
	return nil
}

func lintFailureSummary(checks []domain.CheckResult, limit int) string {
	var parts []string
	for _, check := range checks {
		if check.Status != domain.CheckFail {
			continue
		}
		parts = append(parts, check.ID+": "+check.Message)
		if limit > 0 && len(parts) >= limit {
			break
		}
	}
	if len(parts) == 0 {
		return "no failing checks reported"
	}
	return strings.Join(parts, "; ")
}

func validateSubmissionFields(opts Options) error {
	required := map[string]string{
		"code_lang":            opts.CodeLang,
		"task_type":            opts.TaskType,
		"application":          opts.Application,
		"aht":                  opts.AHT,
		"one_line_description": opts.Description,
		"tests_analysis":       opts.TestsAnalysis,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required for submission package", name)
		}
	}
	if !opts.IsZeroToOne {
		if strings.TrimSpace(opts.GitHubURL) == "" {
			return fmt.Errorf("github_url is required for non 0-1 submission package")
		}
		if err := repourl.RejectCredentials(opts.GitHubURL); err != nil {
			return fmt.Errorf("github_url %w", err)
		}
		if !repourl.IsGitHubRepo(opts.GitHubURL) {
			return fmt.Errorf("github_url must be a GitHub repository URL for non 0-1 submission package")
		}
		if strings.TrimSpace(opts.CommitID) == "" {
			return fmt.Errorf("commit_id is required for non 0-1 submission package")
		}
	}
	return nil
}

func requireReadableEvidence(label, path string, bases ...string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	candidates := []string{path}
	if !filepath.IsAbs(path) {
		for _, base := range bases {
			base = strings.TrimSpace(base)
			if base == "" {
				continue
			}
			candidates = append(candidates, filepath.Join(base, path))
		}
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			cleaned := filepath.Clean(candidate)
			if abs, absErr := filepath.Abs(cleaned); absErr == nil {
				return abs, nil
			}
			return cleaned, nil
		}
	}
	return "", fmt.Errorf("%s is not a readable file: %s", label, path)
}

func parseTrial(label, path string) (domain.TrialResult, error) {
	if strings.TrimSpace(path) == "" {
		return domain.TrialResult{}, fmt.Errorf("%s harbor result is required", label)
	}
	result, err := harborrun.ParseFile(path)
	if err != nil {
		return domain.TrialResult{}, fmt.Errorf("%s harbor result cannot be parsed: %w", label, err)
	}
	return result, nil
}

func validateTrial(label string, result domain.TrialResult, qwen bool, taskDir string) error {
	expectedModel := "claude-opus-4-6"
	if qwen {
		expectedModel = "qwen3.7-max"
	}
	failures := harborrun.ValidateForCodeEdgeWithOptions(result, harborrun.ValidationOptions{
		Qwen:              qwen,
		ExpectedModel:     expectedModel,
		TaskDir:           taskDir,
		RequireRuns:       true,
		RequireTaskDigest: true,
		RequireCommandRun: true,
	})
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("%s harbor result does not meet CodeEdge thresholds: %s", label, strings.Join(failures, "; "))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func validatePackageTaskName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("task name is required")
	}
	if value == "." || value == ".." || filepath.IsAbs(value) || filepath.Base(value) != value || strings.ContainsAny(value, `/\:`) {
		return "", fmt.Errorf("task name must be a single safe path segment: %q", value)
	}
	if len(value) > 128 {
		return "", fmt.Errorf("task name is too long: %d", len(value))
	}
	for idx, r := range value {
		if isPackageTaskNameChar(r) {
			continue
		}
		return "", fmt.Errorf("task name contains unsupported character at position %d: %q", idx, r)
	}
	first := value[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || (first >= '0' && first <= '9')) {
		return "", fmt.Errorf("task name must start with a letter or digit: %q", value)
	}
	return value, nil
}

func isPackageTaskNameChar(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '-' ||
		r == '_' ||
		r == '.'
}

func writeZip(taskDir, taskName, zipPath string) error {
	var err error
	taskName, err = validatePackageTaskName(taskName)
	if err != nil {
		return err
	}
	out, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	defer zw.Close()
	err = filepath.WalkDir(taskDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			rel, _ := filepath.Rel(taskDir, path)
			return fmt.Errorf("symlink is not allowed in Harbor package: %s", filepath.ToSlash(rel))
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(taskDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !isAllowedPackageFile(rel) {
			return fmt.Errorf("unexpected file in Harbor package: %s", rel)
		}
		name := filepath.ToSlash(filepath.Join(taskName, rel))
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular file is not allowed in Harbor package: %s", rel)
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = name
		header.Method = zip.Deflate
		writer, err := zw.CreateHeader(header)
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
		return err
	}
	return nil
}
