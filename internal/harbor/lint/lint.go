package lint

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/harbor/repourl"
	"github.com/purplevoid/harbor-factory/internal/harbor/sanitize"
	"github.com/purplevoid/harbor-factory/internal/harbor/secretscan"
	"gopkg.in/yaml.v3"
)

type Options struct {
	TaskDir          string
	ZipPath          string
	RepoURL          string
	Commit           string
	QwenResult       string
	OpusResult       string
	QwenScreenshot   string
	OpusScreenshot   string
	TestsAnalysis    string
	StrictSubmission bool
	WriteReport      string
}

var commitPattern = regexp.MustCompile(`(?i)\b[0-9a-f]{7,40}\b`)

type taskTOML struct {
	SchemaVersion string              `toml:"schema_version"`
	Task          taskTOMLTask        `toml:"task"`
	Metadata      taskTOMLMetadata    `toml:"metadata"`
	Verifier      taskTOMLTimeout     `toml:"verifier"`
	Agent         taskTOMLTimeout     `toml:"agent"`
	Environment   taskTOMLEnvironment `toml:"environment"`
}

type taskTOMLTask struct {
	Name        string   `toml:"name"`
	Description string   `toml:"description"`
	Keywords    []string `toml:"keywords"`
}

type taskTOMLMetadata struct {
	CodeLang              string   `toml:"code_lang"`
	TaskType              string   `toml:"task_type"`
	Application           string   `toml:"application"`
	IsZeroToOne           bool     `toml:"is_0_to_1"`
	GitHubURL             string   `toml:"github_url"`
	CommitID              string   `toml:"commit_id"`
	EstimatedAHTMinutes   int      `toml:"estimated_aht_minutes"`
	DifficultyExplanation string   `toml:"difficulty_explanation"`
	TargetFiles           []string `toml:"target_files"`
}

type taskTOMLTimeout struct {
	TimeoutSec float64 `toml:"timeout_sec"`
}

type taskTOMLEnvironment struct {
	BuildTimeoutSec float64 `toml:"build_timeout_sec"`
	NetworkMode     string  `toml:"network_mode"`
	OS              string  `toml:"os"`
}

func Run(ctx context.Context, opts Options) (domain.LintReport, error) {
	_ = ctx
	taskDir := strings.TrimSpace(opts.TaskDir)
	zipPath := strings.TrimSpace(opts.ZipPath)
	report := domain.LintReport{
		SchemaVersion: "harbor.lint_report.v1",
		TaskDir:       taskDir,
		RepoURL:       repourl.StripCredentials(strings.TrimSpace(opts.RepoURL)),
		Commit:        strings.TrimSpace(opts.Commit),
		Passed:        true,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repourl.RejectCredentials(opts.RepoURL); err != nil {
		report.Add("repo_url_no_credentials", domain.CheckFail, "submitted repo URL must not include credentials, query, or fragment", report.RepoURL)
	}
	if zipPath != "" {
		rootName, ok := checkZip(&report, zipPath)
		checkZipSecrets(&report, zipPath)
		if ok && taskDir == "" {
			extractedDir, cleanup, err := extractZipRoot(zipPath, rootName)
			if err != nil {
				report.Add("zip_extract", domain.CheckFail, "zip task root cannot be extracted for deep lint: "+err.Error(), zipPath)
			} else {
				defer cleanup()
				taskDir = extractedDir
				report.TaskDir = taskDir
				report.Add("zip_extract", domain.CheckPass, "zip task root extracted for full lint", taskDir)
			}
		}
	}
	if taskDir == "" {
		if zipPath == "" {
			report.Add("task_dir", domain.CheckFail, "task directory or zip path is required", "")
		}
		return finish(report, opts.WriteReport)
	}
	info, err := os.Stat(taskDir)
	if err != nil || !info.IsDir() {
		report.Add("task_dir", domain.CheckFail, "task directory does not exist or is not a directory", taskDir)
		return finish(report, opts.WriteReport)
	}
	checkTaskSecrets(&report, taskDir)
	checkTaskFileSet(&report, taskDir)
	metadata, hasMetadata := checkRequiredFiles(&report, taskDir, opts.StrictSubmission)
	envRepoURL := report.RepoURL
	envCommit := report.Commit
	if hasMetadata {
		if envRepoURL == "" {
			envRepoURL = strings.TrimSpace(metadata.GitHubURL)
		}
		if envCommit == "" {
			envCommit = strings.TrimSpace(metadata.CommitID)
		}
	}
	checkEnvironment(&report, taskDir, envRepoURL, envCommit)
	checkSolution(&report, taskDir)
	checkTest(&report, taskDir)
	checkTestsAnalysis(&report, opts.TestsAnalysis, opts.StrictSubmission)
	checkHarborResult(&report, "qwen_result", opts.QwenResult, taskDir, true, opts.StrictSubmission)
	checkHarborResult(&report, "opus_result", opts.OpusResult, taskDir, false, opts.StrictSubmission)
	checkScreenshot(&report, "qwen_screenshot", opts.QwenScreenshot, opts.StrictSubmission)
	checkScreenshot(&report, "opus_screenshot", opts.OpusScreenshot, opts.StrictSubmission)
	checkDistinctScreenshots(&report, opts.QwenScreenshot, opts.OpusScreenshot, opts.StrictSubmission)
	return finish(report, opts.WriteReport)
}

func checkTaskSecrets(report *domain.LintReport, taskDir string) {
	findings, err := secretscan.ScanDir(taskDir)
	if err != nil {
		report.Add("task_secret_scan", domain.CheckFail, "task secret scan failed: "+err.Error(), taskDir)
		return
	}
	if len(findings) > 0 {
		report.Add("task_secret_scan", domain.CheckFail, "task contains secret-like values: "+secretscan.Summary(findings, 5), taskDir)
		return
	}
	report.Add("task_secret_scan", domain.CheckPass, "task files contain no secret-like values", taskDir)
}

func checkTaskFileSet(report *domain.LintReport, taskDir string) {
	if strings.TrimSpace(taskDir) == "" {
		report.Add("task_file_set", domain.CheckFail, "task directory is required for file set scan", taskDir)
		return
	}
	var extras []string
	var legacy []string
	var nonRegular []string
	err := filepath.WalkDir(taskDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(taskDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.Type()&os.ModeSymlink != 0 {
			nonRegular = append(nonRegular, rel+" (symlink)")
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			nonRegular = append(nonRegular, rel)
			return nil
		}
		if !isAllowedTaskFile(rel) {
			extras = append(extras, rel)
		}
		if legacyDomainMatch(rel) {
			legacy = append(legacy, rel)
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if legacyDomainMatch(string(raw)) {
			legacy = append(legacy, rel)
		}
		return nil
	})
	if err != nil {
		report.Add("task_file_set", domain.CheckFail, "task file set scan failed: "+err.Error(), taskDir)
		return
	}
	if len(nonRegular) > 0 {
		report.Add("task_file_set_regular", domain.CheckFail, "task contains symlink or non-regular files: "+strings.Join(limitStrings(nonRegular, 5), ", "), taskDir)
	} else {
		report.Add("task_file_set_regular", domain.CheckPass, "task files are regular files", taskDir)
	}
	if len(extras) > 0 {
		report.Add("task_file_set_allowed", domain.CheckFail, "task contains unexpected files: "+strings.Join(limitStrings(extras, 5), ", "), taskDir)
	} else {
		report.Add("task_file_set_allowed", domain.CheckPass, "task contains only standard Harbor files", taskDir)
	}
	if len(legacy) > 0 {
		report.Add("task_file_set_legacy", domain.CheckFail, "task contains non-Harbor legacy residue: "+strings.Join(limitStrings(legacy, 5), ", "), taskDir)
	} else {
		report.Add("task_file_set_legacy", domain.CheckPass, "task contains no PPT/promptflow/image2 legacy residue", taskDir)
	}
}

func isAllowedTaskFile(rel string) bool {
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

func legacyDomainMatch(value string) bool {
	lower := strings.ToLower(value)
	for _, term := range []string{"pptflow", "promptflow", "image2", "powerpoint", "presentation", "slide"} {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

func limitStrings(values []string, limit int) []string {
	if limit <= 0 || len(values) <= limit {
		return values
	}
	out := append([]string(nil), values[:limit]...)
	out = append(out, "...")
	return out
}

func checkZipSecrets(report *domain.LintReport, zipPath string) {
	findings, err := secretscan.ScanZip(zipPath)
	if err != nil {
		report.Add("zip_secret_scan", domain.CheckFail, "zip secret scan failed: "+err.Error(), zipPath)
		return
	}
	if len(findings) > 0 {
		report.Add("zip_secret_scan", domain.CheckFail, "zip contains secret-like values: "+secretscan.Summary(findings, 5), zipPath)
		return
	}
	report.Add("zip_secret_scan", domain.CheckPass, "zip contains no secret-like values", zipPath)
}

func finish(report domain.LintReport, writePath string) (domain.LintReport, error) {
	report = sanitize.LintReport(report)
	if strings.TrimSpace(writePath) == "" {
		return report, nil
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return report, err
	}
	if err := os.MkdirAll(filepath.Dir(writePath), 0o755); err != nil {
		return report, err
	}
	return report, os.WriteFile(writePath, append(data, '\n'), 0o644)
}

func checkRequiredFiles(report *domain.LintReport, taskDir string, strict bool) (taskTOMLMetadata, bool) {
	required := []string{
		"instruction.md",
		"task.toml",
		"tests_analysis.md",
		filepath.Join("environment"),
		filepath.Join("solution", "solve.sh"),
		filepath.Join("tests", "test.sh"),
	}
	for _, rel := range required {
		path := filepath.Join(taskDir, rel)
		if _, err := os.Stat(path); err != nil {
			report.Add("required_"+safeID(rel), domain.CheckFail, "required Harbor task path is missing", path)
			continue
		}
		report.Add("required_"+safeID(rel), domain.CheckPass, "required Harbor task path exists", path)
	}
	return checkTaskTOML(report, filepath.Join(taskDir, "task.toml"), strict)
}

func checkZip(report *domain.LintReport, zipPath string) (string, bool) {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		report.Add("zip_readable", domain.CheckFail, "task zip cannot be opened", zipPath)
		return "", false
	}
	defer reader.Close()
	report.Add("zip_readable", domain.CheckPass, "task zip is readable", zipPath)
	roots := map[string]bool{}
	entries := map[string]bool{}
	for _, file := range reader.File {
		name := strings.Trim(file.Name, "/")
		if name == "" || strings.Contains(name, "..") {
			continue
		}
		parts := strings.Split(name, "/")
		if len(parts) == 0 || parts[0] == "" {
			continue
		}
		roots[parts[0]] = true
		if len(parts) > 1 && !file.FileInfo().IsDir() {
			entries[strings.Join(parts[1:], "/")] = true
		}
	}
	if len(roots) != 1 {
		report.Add("zip_single_root", domain.CheckFail, fmt.Sprintf("zip must contain exactly one task root, found %d", len(roots)), zipPath)
		return "", false
	}
	rootNames := sortedKeys(roots)
	report.Add("zip_single_root", domain.CheckPass, "zip contains one task root: "+rootNames[0], zipPath)
	required := []string{"instruction.md", "task.toml", "tests_analysis.md", "solution/solve.sh", "tests/test.sh"}
	for _, rel := range required {
		if !entries[rel] {
			report.Add("zip_required_"+safeID(rel), domain.CheckFail, "zip task root missing required file", zipPath)
			return rootNames[0], false
		}
	}
	rootPrefix := rootNames[0] + "/"
	for _, file := range reader.File {
		name := strings.Trim(file.Name, "/")
		if name == "" || name == rootNames[0] || file.FileInfo().IsDir() {
			continue
		}
		if !strings.HasPrefix(name, rootPrefix) {
			report.Add("zip_file_set", domain.CheckFail, "zip entry is outside task root", zipPath)
			return rootNames[0], false
		}
		rel := strings.TrimPrefix(name, rootPrefix)
		if file.FileInfo().Mode()&os.ModeSymlink != 0 {
			report.Add("zip_file_set", domain.CheckFail, "zip task root contains symlink: "+rel, zipPath)
			return rootNames[0], false
		}
		if !file.FileInfo().Mode().IsRegular() {
			report.Add("zip_file_set", domain.CheckFail, "zip task root contains non-regular file: "+rel, zipPath)
			return rootNames[0], false
		}
		if !isAllowedTaskFile(rel) {
			report.Add("zip_file_set", domain.CheckFail, "zip task root contains unexpected file: "+rel, zipPath)
			return rootNames[0], false
		}
		if legacyDomainMatch(rel) {
			report.Add("zip_file_set", domain.CheckFail, "zip task root contains legacy non-Harbor file: "+rel, zipPath)
			return rootNames[0], false
		}
	}
	report.Add("zip_file_set", domain.CheckPass, "zip task root contains only standard regular Harbor files", zipPath)
	hasEnvironment := false
	for entry := range entries {
		if entry == "environment/Dockerfile" || entry == "environment/docker-compose.yaml" {
			hasEnvironment = true
			break
		}
	}
	if !hasEnvironment {
		report.Add("zip_environment", domain.CheckFail, "zip task root missing environment/Dockerfile or environment/docker-compose.yaml", zipPath)
		return rootNames[0], false
	}
	report.Add("zip_required_files", domain.CheckPass, "zip task root contains required Harbor files", zipPath)
	return rootNames[0], true
}

func extractZipRoot(zipPath, rootName string) (string, func(), error) {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", func() {}, err
	}
	tempDir, err := os.MkdirTemp("", "harbor-lint-zip-*")
	if err != nil {
		_ = reader.Close()
		return "", func() {}, err
	}
	cleanup := func() {
		_ = reader.Close()
		_ = os.RemoveAll(tempDir)
	}
	rootPrefix := strings.Trim(rootName, "/") + "/"
	for _, file := range reader.File {
		name := strings.Trim(file.Name, "/")
		if name == "" {
			continue
		}
		if filepath.IsAbs(name) || strings.Contains(name, "..") {
			cleanup()
			return "", func() {}, fmt.Errorf("unsafe zip entry %q", file.Name)
		}
		if name == rootName {
			continue
		}
		if !strings.HasPrefix(name, rootPrefix) {
			cleanup()
			return "", func() {}, fmt.Errorf("zip entry outside root %q", file.Name)
		}
		rel := strings.TrimPrefix(name, rootPrefix)
		if rel == "" {
			continue
		}
		target := filepath.Join(tempDir, filepath.FromSlash(rel))
		if !strings.HasPrefix(target, filepath.Clean(tempDir)+string(filepath.Separator)) {
			cleanup()
			return "", func() {}, fmt.Errorf("unsafe zip entry %q", file.Name)
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				cleanup()
				return "", func() {}, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			cleanup()
			return "", func() {}, err
		}
		in, err := file.Open()
		if err != nil {
			cleanup()
			return "", func() {}, err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, file.FileInfo().Mode().Perm())
		if err != nil {
			_ = in.Close()
			cleanup()
			return "", func() {}, err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		_ = in.Close()
		if copyErr != nil {
			cleanup()
			return "", func() {}, copyErr
		}
		if closeErr != nil {
			cleanup()
			return "", func() {}, closeErr
		}
	}
	return tempDir, cleanup, nil
}

func checkTaskTOML(report *domain.LintReport, path string, strict bool) (taskTOMLMetadata, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		report.Add("task_toml_read", domain.CheckFail, "cannot read task.toml", path)
		return taskTOMLMetadata{}, false
	}
	text := string(raw)
	if strings.TrimSpace(text) == "" {
		report.Add("task_toml_nonempty", domain.CheckFail, "task.toml is empty", path)
		return taskTOMLMetadata{}, false
	}
	report.Add("task_toml_nonempty", domain.CheckPass, "task.toml is non-empty", path)
	var task taskTOML
	if err := toml.Unmarshal(raw, &task); err != nil {
		report.Add("task_toml_parse", domain.CheckFail, "task.toml is not valid TOML: "+err.Error(), path)
		return taskTOMLMetadata{}, false
	}
	report.Add("task_toml_parse", domain.CheckPass, "task.toml parses as TOML", path)
	if strings.TrimSpace(task.Task.Name) == "" {
		report.Add("task_toml_task_section", domain.CheckFail, "task.toml must define task metadata/name", path)
	} else {
		report.Add("task_toml_task_section", domain.CheckPass, "task.toml defines task metadata/name", path)
	}
	if strings.TrimSpace(task.Task.Description) == "" {
		report.Add("task_toml_description", domain.CheckFail, "task.toml [task].description is required", path)
	} else {
		report.Add("task_toml_description", domain.CheckPass, "task.toml includes [task].description", path)
	}
	schema := strings.TrimSpace(task.SchemaVersion)
	if schema == "" {
		report.Add("task_toml_schema", submissionStatus(strict), "task.toml missing schema_version 1.3", path)
	} else if schema != "1.3" {
		report.Add("task_toml_schema", domain.CheckFail, "task.toml schema_version must be 1.3", path)
	} else {
		report.Add("task_toml_schema", domain.CheckPass, "task.toml schema_version is 1.3", path)
	}
	checkRequiredString(report, "task_toml_code_lang", task.Metadata.CodeLang, submissionStatus(strict), "task.toml missing CodeEdge metadata field code_lang", "task.toml includes CodeEdge metadata field code_lang", path)
	checkRequiredString(report, "task_toml_task_type", task.Metadata.TaskType, submissionStatus(strict), "task.toml missing CodeEdge metadata field task_type", "task.toml includes CodeEdge metadata field task_type", path)
	checkRequiredString(report, "task_toml_application", task.Metadata.Application, submissionStatus(strict), "task.toml missing CodeEdge metadata field application", "task.toml includes CodeEdge metadata field application", path)
	if task.Metadata.EstimatedAHTMinutes <= 0 {
		report.Add("task_toml_aht", submissionStatus(strict), "task.toml metadata.estimated_aht_minutes must be positive", path)
	} else {
		report.Add("task_toml_aht", domain.CheckPass, "task.toml includes positive estimated_aht_minutes", path)
	}
	if strings.TrimSpace(task.Metadata.DifficultyExplanation) == "" {
		report.Add("task_toml_difficulty", submissionStatus(strict), "task.toml metadata.difficulty_explanation is required", path)
	} else {
		report.Add("task_toml_difficulty", domain.CheckPass, "task.toml includes difficulty explanation", path)
	}
	if len(task.Metadata.TargetFiles) == 0 {
		report.Add("task_toml_target_files", submissionStatus(strict), "task.toml metadata.target_files should identify affected files", path)
	} else {
		report.Add("task_toml_target_files", domain.CheckPass, "task.toml includes target files", path)
	}
	repo := strings.TrimSpace(task.Metadata.GitHubURL)
	if err := repourl.RejectCredentials(repo); err != nil {
		report.Add("task_toml_github_no_credentials", domain.CheckFail, "task.toml github_url must not include credentials, query, or fragment", path)
		repo = repourl.StripCredentials(repo)
	}
	if repo != "" && !task.Metadata.IsZeroToOne && !repourl.IsGitHubRepo(repo) {
		report.Add("task_toml_github_url", domain.CheckFail, "task.toml github_url must be a GitHub repository URL for non 0-1 tasks", path)
	} else if repo != "" || task.Metadata.IsZeroToOne {
		report.Add("task_toml_github_url", domain.CheckPass, "task.toml github_url is acceptable for task type", path)
	}
	if repo == "" && strict && !task.Metadata.IsZeroToOne {
		report.Add("task_toml_github_match", domain.CheckFail, "task.toml missing github_url for non 0-1 task", path)
	} else if repo != "" && report.RepoURL != "" && repo != report.RepoURL {
		report.Add("task_toml_github_match", domain.CheckFail, "task.toml github_url does not match submitted repo URL", path)
	} else if repo != "" || report.RepoURL == "" || task.Metadata.IsZeroToOne {
		report.Add("task_toml_github_match", domain.CheckPass, "task.toml github_url matches or repo URL was not provided", path)
	} else {
		report.Add("task_toml_github_match", submissionStatus(strict), "task.toml missing github_url for non 0-1 task", path)
	}
	commit := strings.TrimSpace(task.Metadata.CommitID)
	if commit == "" && strict && !task.Metadata.IsZeroToOne {
		report.Add("task_toml_commit_match", domain.CheckFail, "task.toml missing commit_id for non 0-1 task", path)
	} else if commit != "" && report.Commit != "" && !strings.EqualFold(commit, report.Commit) {
		report.Add("task_toml_commit_match", domain.CheckFail, "task.toml commit_id does not match submitted commit", path)
	} else if commit != "" && !commitPattern.MatchString(commit) {
		report.Add("task_toml_commit_match", domain.CheckFail, "task.toml commit_id is not a concrete 7-40 hex commit", path)
	} else if commit != "" || report.Commit == "" || task.Metadata.IsZeroToOne {
		report.Add("task_toml_commit_match", domain.CheckPass, "task.toml commit_id matches or commit was not provided", path)
	} else {
		report.Add("task_toml_commit_match", submissionStatus(strict), "task.toml missing commit_id for non 0-1 task", path)
	}
	checkPositiveFloat(report, "task_toml_verifier_timeout", task.Verifier.TimeoutSec, domain.CheckFail, "task.toml [verifier].timeout_sec must be positive", "task.toml verifier timeout is positive", path)
	checkPositiveFloat(report, "task_toml_agent_timeout", task.Agent.TimeoutSec, submissionStatus(strict), "task.toml [agent].timeout_sec must be positive", "task.toml agent timeout is positive", path)
	checkPositiveFloat(report, "task_toml_build_timeout", task.Environment.BuildTimeoutSec, submissionStatus(strict), "task.toml [environment].build_timeout_sec must be positive", "task.toml environment build timeout is positive", path)
	checkRequiredString(report, "task_toml_environment_os", task.Environment.OS, submissionStatus(strict), "task.toml [environment].os is required", "task.toml environment OS is present", path)
	checkTaskNetworkMode(report, task.Environment.NetworkMode, path)
	return task.Metadata, true
}

func checkTaskNetworkMode(report *domain.LintReport, value, path string) {
	value = strings.TrimSpace(value)
	if value == "" {
		report.Add("task_toml_network_mode", domain.CheckWarn, "task.toml [environment].network_mode should be explicit", path)
		return
	}
	switch value {
	case "public", "no-network", "allowlist":
		report.Add("task_toml_network_mode", domain.CheckPass, "task.toml environment network mode is valid", path)
	default:
		report.Add("task_toml_network_mode", domain.CheckFail, "task.toml environment network_mode must be one of public, no-network, or allowlist", path)
	}
}

func checkRequiredString(report *domain.LintReport, id, value string, missingStatus domain.CheckStatus, missingMessage, passMessage, path string) {
	if strings.TrimSpace(value) == "" {
		report.Add(id, missingStatus, missingMessage, path)
		return
	}
	report.Add(id, domain.CheckPass, passMessage, path)
}

func checkPositiveFloat(report *domain.LintReport, id string, value float64, missingStatus domain.CheckStatus, missingMessage, passMessage, path string) {
	if value <= 0 {
		report.Add(id, missingStatus, missingMessage, path)
		return
	}
	report.Add(id, domain.CheckPass, passMessage, path)
}

func checkEnvironment(report *domain.LintReport, taskDir, repoURL, commit string) {
	envDir := filepath.Join(taskDir, "environment")
	dockerfile := filepath.Join(envDir, "Dockerfile")
	compose := filepath.Join(envDir, "docker-compose.yaml")
	_, dockerErr := os.Stat(dockerfile)
	_, composeErr := os.Stat(compose)
	switch {
	case dockerErr == nil && composeErr == nil:
		report.Add("environment_single_mode", domain.CheckPass, "environment includes Dockerfile and docker-compose.yaml; compose source will be linted with Dockerfile provenance", envDir)
		report.Add("environment_compose", domain.CheckPass, "docker-compose.yaml found and will be structurally linted", compose)
		checkCompose(report, compose, repoURL, commit)
	case dockerErr != nil && composeErr != nil:
		report.Add("environment_present", domain.CheckFail, "environment must contain Dockerfile or docker-compose.yaml", envDir)
	case dockerErr == nil:
		report.Add("environment_dockerfile", domain.CheckPass, "Dockerfile found", dockerfile)
		checkDockerfile(report, dockerfile, repoURL, commit)
	case composeErr == nil:
		report.Add("environment_compose", domain.CheckPass, "docker-compose.yaml found and will be structurally linted", compose)
		checkCompose(report, compose, repoURL, commit)
	}
}

func checkDockerfile(report *domain.LintReport, path, repoURL, commit string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		report.Add("dockerfile_read", domain.CheckFail, "cannot read file", path)
		return
	}
	content := strings.ToLower(stripDockerfileComments(string(raw)))
	report.Add("dockerfile_read", domain.CheckPass, "Dockerfile is readable", path)
	for _, forbidden := range []string{"copy tests", "copy ./tests", "copy solution", "copy ./solution", "add tests", "add ./tests", "add solution", "add ./solution"} {
		if strings.Contains(content, forbidden) {
			report.Add("dockerfile_no_solution_tests", domain.CheckFail, "Dockerfile must not copy tests or solution into the image", path)
			return
		}
	}
	report.Add("dockerfile_no_solution_tests", domain.CheckPass, "Dockerfile does not directly copy tests or solution", path)
	if dockerfileCopiesWholeContext(content) {
		report.Add("dockerfile_copy_context", domain.CheckWarn, "Dockerfile copies the whole build context; ensure docker build context is environment/ only", path)
	} else {
		report.Add("dockerfile_copy_context", domain.CheckPass, "Dockerfile does not copy the whole build context", path)
	}
	if strings.Contains(content, "reward.txt") || strings.Contains(content, "reward.json") || strings.Contains(content, "/logs/verifier/reward") {
		report.Add("dockerfile_no_reward", domain.CheckFail, "Dockerfile must not prewrite verifier reward", path)
	} else {
		report.Add("dockerfile_no_reward", domain.CheckPass, "Dockerfile does not prewrite verifier reward", path)
	}
	if strings.Contains(content, "git clone") {
		cloneURL := firstDockerfileGitCloneURL(content)
		if cloneURL == "" {
			report.Add("dockerfile_repo_match", domain.CheckFail, "Dockerfile git clone URL could not be parsed", path)
		} else if repoURL != "" && normalizeRepoURL(cloneURL) != normalizeRepoURL(repoURL) {
			report.Add("dockerfile_repo_match", domain.CheckFail, "Dockerfile git clone URL does not match submitted GitHub URL", path)
		} else {
			report.Add("dockerfile_repo_match", domain.CheckPass, "Dockerfile git clone URL matches or repo URL was not provided", path)
		}
		if commit != "" && !dockerfileChecksOutCommit(content, commit) {
			report.Add("dockerfile_commit_match", domain.CheckFail, "Dockerfile does not checkout/reset the submitted commit", path)
		} else if dockerfileChecksOutConcreteCommit(content) {
			report.Add("dockerfile_commit_match", domain.CheckPass, "Dockerfile pins a concrete commit", path)
		} else {
			report.Add("dockerfile_commit_match", domain.CheckFail, "Dockerfile git clone must be pinned to a concrete commit", path)
		}
	} else if repoURL != "" && commit != "" {
		report.Add("dockerfile_git_clone", domain.CheckFail, "non 0-1 task Dockerfile must git clone the submitted public repo", path)
	}
}

func checkCompose(report *domain.LintReport, path, repoURL, commit string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		report.Add("compose_read", domain.CheckFail, "cannot read file", path)
		return
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		report.Add("compose_parse", domain.CheckFail, "docker-compose.yaml is not valid YAML: "+err.Error(), path)
		return
	}
	report.Add("compose_parse", domain.CheckPass, "docker-compose.yaml parses as YAML", path)

	root := yamlDocumentRoot(&doc)
	if root == nil || root.Kind != yaml.MappingNode {
		report.Add("compose_yaml_map", domain.CheckFail, "docker-compose.yaml must be a YAML map", path)
		return
	}
	report.Add("compose_yaml_map", domain.CheckPass, "docker-compose.yaml is a YAML map", path)

	services := yamlMapValue(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		report.Add("compose_services_map", domain.CheckFail, "docker-compose.yaml must define services map", path)
		return
	}
	report.Add("compose_services_map", domain.CheckPass, "docker-compose.yaml defines services map", path)

	main := yamlMapValue(services, "main")
	if main == nil || main.Kind != yaml.MappingNode {
		report.Add("compose_main_service", domain.CheckFail, "docker-compose.yaml must define service main as a map", path)
		return
	}
	report.Add("compose_main_service", domain.CheckPass, "docker-compose.yaml defines service main", path)

	checkComposeMainSource(report, main, path)
	checkComposeVolumes(report, main, path)
	checkComposeRepoCommit(report, main, path, repoURL, commit)
}

func checkComposeMainSource(report *domain.LintReport, main *yaml.Node, path string) {
	build := yamlMapValue(main, "build")
	image := yamlMapValue(main, "image")
	if !yamlHasValue(build) && !yamlHasValue(image) {
		report.Add("compose_main_image_or_build", domain.CheckFail, "compose service main must define build or image", path)
		return
	}
	report.Add("compose_main_image_or_build", domain.CheckPass, "compose service main defines build or image", path)
	if !yamlHasValue(build) {
		report.Add("compose_build_context", domain.CheckPass, "compose service main uses image; no build context needed", path)
		return
	}

	build = resolveYAMLAlias(build)
	switch build.Kind {
	case yaml.MappingNode:
		contextNode := yamlMapValue(build, "context")
		contextValue, _ := yamlScalarValue(contextNode)
		if strings.TrimSpace(contextValue) == "" {
			report.Add("compose_build_context", domain.CheckFail, "compose build map must include non-empty context", path)
			return
		}
		checkComposeBuildContext(report, path, contextValue)
	case yaml.ScalarNode:
		contextValue, _ := yamlScalarValue(build)
		if strings.TrimSpace(contextValue) == "" {
			report.Add("compose_build_context", domain.CheckFail, "compose build context must be non-empty", path)
			return
		}
		checkComposeBuildContext(report, path, contextValue)
	default:
		report.Add("compose_build_context", domain.CheckFail, "compose build must be a string or map with context", path)
	}
}

func checkComposeBuildContext(report *domain.LintReport, path, contextValue string) {
	contextValue = strings.TrimSpace(contextValue)
	if isRemoteComposePath(contextValue) {
		report.Add("compose_build_context", domain.CheckFail, "compose build context must be package-local, not remote or named", path)
		return
	}
	if filepath.IsAbs(contextValue) {
		report.Add("compose_build_context", domain.CheckFail, "compose build context must be relative and package-local", path)
		return
	}
	clean := cleanComposePath(contextValue)
	switch {
	case isTaskRootPath(clean) && filepath.Base(filepath.Dir(path)) == "environment":
		report.Add("compose_build_context", domain.CheckPass, "compose build context is scoped to environment directory", path)
	case isTaskRootPath(clean), isParentPath(clean), clean == "/":
		report.Add("compose_build_context", domain.CheckFail, "compose build context must not include task root with tests/solution", path)
	case pathHasSegment(clean, "tests") || pathHasSegment(clean, "solution"):
		report.Add("compose_build_context", domain.CheckFail, "compose build context must not include tests or solution", path)
	case isEnvironmentPath(clean):
		report.Add("compose_build_context", domain.CheckPass, "compose build context appears scoped to environment", path)
	default:
		report.Add("compose_build_context", domain.CheckWarn, "compose build context not recognized; confirm it excludes tests/solution", path)
	}
}

func checkComposeRepoCommit(report *domain.LintReport, main *yaml.Node, composePath, repoURL, commit string) {
	if strings.TrimSpace(repoURL) == "" && strings.TrimSpace(commit) == "" {
		report.Add("compose_repo_commit", domain.CheckPass, "compose repo/commit provenance not required without submitted repo metadata", composePath)
		return
	}
	build := yamlMapValue(main, "build")
	if !yamlHasValue(build) {
		report.Add("compose_repo_commit", domain.CheckFail, "compose service main uses image only; non 0-1 tasks must prove pinned repo source via Dockerfile", composePath)
		return
	}
	dockerfilePath, dockerfileErr := composeDockerfilePath(composePath, build)
	if dockerfileErr != nil {
		report.Add("compose_repo_commit", domain.CheckFail, dockerfileErr.Error(), composePath)
		return
	}
	if dockerfilePath == "" {
		report.Add("compose_repo_commit", domain.CheckFail, "compose build Dockerfile path cannot be resolved for repo/commit provenance", composePath)
		return
	}
	if _, err := os.Stat(dockerfilePath); err != nil {
		report.Add("compose_repo_commit", domain.CheckFail, "compose build Dockerfile cannot be read for repo/commit provenance", dockerfilePath)
		return
	}
	report.Add("compose_repo_commit", domain.CheckPass, "compose build Dockerfile will be checked for submitted repo and commit", dockerfilePath)
	checkDockerfile(report, dockerfilePath, repoURL, commit)
}

func composeDockerfilePath(composePath string, build *yaml.Node) (string, error) {
	envDir := filepath.Dir(composePath)
	defaultPath := filepath.Join(envDir, "Dockerfile")
	build = resolveYAMLAlias(build)
	if build == nil {
		return defaultPath, nil
	}
	dockerfileName := "Dockerfile"
	contextValue := "."
	switch build.Kind {
	case yaml.MappingNode:
		if value, ok := yamlScalarValue(yamlMapValue(build, "dockerfile")); ok && strings.TrimSpace(value) != "" {
			dockerfileName = value
		}
		if value, ok := yamlScalarValue(yamlMapValue(build, "context")); ok && strings.TrimSpace(value) != "" {
			contextValue = value
		}
	case yaml.ScalarNode:
		if strings.TrimSpace(build.Value) != "" {
			contextValue = build.Value
		}
	}
	if filepath.IsAbs(dockerfileName) {
		return "", fmt.Errorf("compose build dockerfile must be relative and package-local")
	}
	contextValue = strings.TrimSpace(contextValue)
	if isRemoteComposePath(contextValue) {
		return "", fmt.Errorf("compose build context must be package-local for repo/commit provenance")
	}
	if filepath.IsAbs(contextValue) {
		return "", fmt.Errorf("compose build context must be relative and package-local for repo/commit provenance")
	}
	candidates := []string{
		defaultPath,
		filepath.Join(envDir, dockerfileName),
		filepath.Join(envDir, filepath.FromSlash(contextValue), dockerfileName),
		filepath.Join(filepath.Dir(envDir), filepath.FromSlash(contextValue), dockerfileName),
	}
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if !pathWithinDir(candidate, envDir) {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	final := filepath.Clean(candidates[len(candidates)-1])
	if !pathWithinDir(final, envDir) {
		return "", fmt.Errorf("compose build Dockerfile must resolve inside task environment directory")
	}
	return final, nil
}

func checkComposeVolumes(report *domain.LintReport, main *yaml.Node, path string) {
	volumesNode := yamlMapValue(main, "volumes")
	if volumesNode == nil {
		report.Add("compose_no_dangerous_volumes", domain.CheckPass, "compose service main defines no volumes exposing task root, tests, solution, or Docker socket", path)
		return
	}
	if volumesNode.Kind != yaml.SequenceNode {
		report.Add("compose_no_dangerous_volumes", domain.CheckFail, "compose service main volumes must be a list for safety linting", path)
		return
	}
	volumes := parseComposeVolumes(volumesNode)
	dangerous := make([]string, 0)
	for _, volume := range volumes {
		if reason := dangerousComposeVolume(volume); reason != "" {
			dangerous = append(dangerous, reason)
		}
	}
	if len(dangerous) > 0 {
		report.Add("compose_no_dangerous_volumes", domain.CheckFail, "compose volumes must not expose task root, tests, solution, or Docker socket: "+strings.Join(dangerous, "; "), path)
		return
	}
	report.Add("compose_no_dangerous_volumes", domain.CheckPass, "compose volumes do not expose task root, tests, solution, or Docker socket", path)
}

type composeVolume struct {
	source string
	target string
	raw    string
}

func parseComposeVolumes(volumesNode *yaml.Node) []composeVolume {
	volumes := make([]composeVolume, 0, len(volumesNode.Content))
	for _, item := range volumesNode.Content {
		item = resolveYAMLAlias(item)
		switch item.Kind {
		case yaml.ScalarNode:
			volumes = append(volumes, parseComposeShortVolume(item.Value))
		case yaml.MappingNode:
			volumes = append(volumes, parseComposeLongVolume(item))
		default:
			volumes = append(volumes, composeVolume{raw: item.Value})
		}
	}
	return volumes
}

func parseComposeShortVolume(value string) composeVolume {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, ":")
	if len(parts) == 1 {
		return composeVolume{target: strings.TrimSpace(parts[0]), raw: value}
	}
	return composeVolume{
		source: strings.TrimSpace(parts[0]),
		target: strings.TrimSpace(parts[1]),
		raw:    value,
	}
}

func parseComposeLongVolume(node *yaml.Node) composeVolume {
	source, _ := yamlMapStringValue(node, "source", "src")
	target, _ := yamlMapStringValue(node, "target", "destination", "dst")
	raw := strings.TrimSpace(source + ":" + target)
	if raw == ":" {
		raw = "long syntax volume"
	}
	return composeVolume{source: source, target: target, raw: raw}
}

func dangerousComposeVolume(volume composeVolume) string {
	rawLower := strings.ToLower(volume.raw)
	if strings.Contains(rawLower, "docker.sock") {
		return volume.raw
	}
	if composeVolumeTouches(volume, "tests") || composeVolumeTouches(volume, "solution") {
		return volume.raw
	}
	source := strings.TrimSpace(volume.source)
	if source == "" {
		return ""
	}
	if isRemoteComposePath(source) {
		return ""
	}
	clean := cleanComposePath(source)
	if isTaskRootPath(clean) || isParentPath(clean) || clean == "/" {
		return volume.raw
	}
	return ""
}

func composeVolumeTouches(volume composeVolume, name string) bool {
	return pathHasSegment(cleanComposePath(volume.source), name) || pathHasSegment(cleanComposePath(volume.target), name)
}

func yamlDocumentRoot(doc *yaml.Node) *yaml.Node {
	if doc == nil {
		return nil
	}
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		return resolveYAMLAlias(doc.Content[0])
	}
	return resolveYAMLAlias(doc)
}

func yamlMapValue(node *yaml.Node, key string) *yaml.Node {
	node = resolveYAMLAlias(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := resolveYAMLAlias(node.Content[i])
		if keyNode != nil && keyNode.Kind == yaml.ScalarNode && keyNode.Value == key {
			return resolveYAMLAlias(node.Content[i+1])
		}
	}
	return nil
}

func yamlMapStringValue(node *yaml.Node, keys ...string) (string, bool) {
	for _, key := range keys {
		value, ok := yamlScalarValue(yamlMapValue(node, key))
		if ok {
			return value, true
		}
	}
	return "", false
}

func yamlScalarValue(node *yaml.Node) (string, bool) {
	node = resolveYAMLAlias(node)
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag == "!!null" {
		return "", false
	}
	return strings.TrimSpace(node.Value), true
}

func yamlHasValue(node *yaml.Node) bool {
	node = resolveYAMLAlias(node)
	if node == nil || node.Tag == "!!null" {
		return false
	}
	switch node.Kind {
	case yaml.ScalarNode:
		return strings.TrimSpace(node.Value) != ""
	case yaml.MappingNode, yaml.SequenceNode:
		return len(node.Content) > 0
	default:
		return false
	}
}

func resolveYAMLAlias(node *yaml.Node) *yaml.Node {
	for depth := 0; node != nil && node.Kind == yaml.AliasNode && depth < 8; depth++ {
		node = node.Alias
	}
	return node
}

func cleanComposePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(value))
}

func isTaskRootPath(value string) bool {
	return value == "." || value == "$PWD" || value == "${PWD}"
}

func isParentPath(value string) bool {
	return value == ".." || strings.HasPrefix(value, "../")
}

func isEnvironmentPath(value string) bool {
	return value == "environment" || strings.HasPrefix(value, "environment/")
}

func isRemoteComposePath(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.Contains(lower, "://") || strings.HasPrefix(lower, "git@") || strings.HasPrefix(lower, "docker-image://")
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
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func pathHasSegment(value, segment string) bool {
	value = strings.Trim(value, "/")
	if value == "" {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == segment {
			return true
		}
	}
	return false
}

func checkSolution(report *domain.LintReport, taskDir string) {
	path := filepath.Join(taskDir, "solution", "solve.sh")
	content := readLower(report, "solution_read", path)
	if content == "" {
		return
	}
	if suspicious(content, []string{"tests/test.sh", "/tests/test.sh", "reward.txt", "reward.json", "/logs/verifier/reward"}) {
		report.Add("solution_no_bypass", domain.CheckFail, "solve.sh appears to modify/run verifier internals or reward directly", path)
	} else {
		report.Add("solution_no_bypass", domain.CheckPass, "solve.sh has no obvious verifier bypass pattern", path)
	}
}

func checkTest(report *domain.LintReport, taskDir string) {
	path := filepath.Join(taskDir, "tests", "test.sh")
	raw, err := os.ReadFile(path)
	if err != nil {
		report.Add("test_read", domain.CheckFail, "cannot read tests/test.sh", path)
		return
	}
	text := strings.TrimSpace(string(raw))
	lower := strings.ToLower(text)
	if text == "" {
		report.Add("test_nonempty", domain.CheckFail, "tests/test.sh is empty", path)
		return
	}
	report.Add("test_nonempty", domain.CheckPass, "tests/test.sh is non-empty", path)
	if isTrivialTestScript(lower) {
		report.Add("test_not_trivial", domain.CheckFail, "tests/test.sh appears trivial", path)
	} else {
		report.Add("test_not_trivial", domain.CheckPass, "tests/test.sh is not an obvious no-op", path)
	}
	if suspicious(lower, []string{" /solution", "/solution/", "/solution/solve.sh", "solution/solve.sh", "../solution"}) {
		report.Add("test_no_solution_bypass", domain.CheckFail, "tests/test.sh must not read solution artifacts", path)
	} else {
		report.Add("test_no_solution_bypass", domain.CheckPass, "tests/test.sh does not read solution artifacts", path)
	}
	strength := testAssertionStrength(lower)
	if strength.swallowed > 0 {
		report.Add("test_no_swallowed_assertions", domain.CheckFail, "tests/test.sh contains assertions whose failures are swallowed", path)
	} else {
		report.Add("test_no_swallowed_assertions", domain.CheckPass, "tests/test.sh does not swallow assertion failures", path)
	}
	if strength.strong == 0 && strength.weak > 0 {
		report.Add("test_not_file_existence_only", domain.CheckFail, "tests/test.sh only checks file/path existence; add behavioral or content assertions", path)
	} else if strength.strong > 0 {
		report.Add("test_not_file_existence_only", domain.CheckPass, "tests/test.sh includes behavioral or content assertions", path)
	}
	if strength.strong == 0 {
		report.Add("test_has_assertion", domain.CheckFail, "tests/test.sh has no obvious strong functional assertion command", path)
	} else {
		report.Add("test_has_assertion", domain.CheckPass, "tests/test.sh includes obvious assertion or test command", path)
	}
}

type testStrength struct {
	strong    int
	weak      int
	swallowed int
}

func testAssertionStrength(content string) testStrength {
	var strength testStrength
	for _, rawLine := range strings.Split(content, "\n") {
		line := normalizeShellLine(rawLine)
		if line == "" || isTestBoilerplate(line) {
			continue
		}
		if swallowsAssertionFailure(line) {
			if isStrongTestAssertion(line) || isWeakFileAssertion(line) {
				strength.swallowed++
			}
			continue
		}
		switch {
		case isStrongTestAssertion(line):
			strength.strong++
		case isWeakFileAssertion(line):
			strength.weak++
		}
	}
	return strength
}

func normalizeShellLine(line string) string {
	line = strings.TrimSpace(line)
	if idx := strings.Index(line, "#"); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	for _, prefix := range []string{"if ", "then ", "&& ", "|| "} {
		line = strings.TrimPrefix(line, prefix)
	}
	line = strings.TrimSuffix(line, "; then")
	line = strings.TrimSuffix(line, ";")
	return strings.TrimSpace(line)
}

func isTrivialTestScript(content string) bool {
	meaningful := 0
	for _, rawLine := range strings.Split(content, "\n") {
		line := normalizeShellLine(rawLine)
		if line == "" || strings.HasPrefix(line, "#!") || strings.HasPrefix(line, "set ") || strings.HasPrefix(line, "cd ") {
			continue
		}
		meaningful++
		if !isTrivialTestLine(line) {
			return false
		}
	}
	return meaningful > 0
}

func isTrivialTestLine(line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return true
	}
	switch fields[0] {
	case "exit":
		return len(fields) == 1 || fields[1] == "0"
	case "true", ":", "echo", "printf", "touch", "chmod", "chown", "mkdir", "cat", "head", "tail", "wc", "ls", "stat":
		return true
	default:
		return false
	}
}

func isTestBoilerplate(line string) bool {
	if line == "" || strings.HasPrefix(line, "#!") {
		return true
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return true
	}
	switch fields[0] {
	case "set", "cd", "mkdir", "trap", "finish()", "export", "status=$?", "return":
		return true
	default:
		return false
	}
}

func swallowsAssertionFailure(line string) bool {
	swallowPatterns := []string{"|| true", "|| :", "|| echo", "; true", "&& true"}
	for _, pattern := range swallowPatterns {
		if strings.Contains(line, pattern) {
			return true
		}
	}
	return false
}

func isStrongTestAssertion(line string) bool {
	strongTokens := []string{
		"grep ", "grep -", "rg ", "diff ", "cmp ", "pytest", "go test", "cargo test", "npm test", "pnpm test", "yarn test",
		"python -m unittest", "python -m pytest", "mvn test", "gradle test", "curl -f", "curl --fail", "jq ", "sha256sum -c",
	}
	for _, token := range strongTokens {
		if strings.Contains(line, token) {
			return true
		}
	}
	if strings.Contains(line, "==") || strings.Contains(line, "!=") {
		return true
	}
	return false
}

func isWeakFileAssertion(line string) bool {
	weakTokens := []string{
		"test -f ", "test -e ", "test -d ", "test -s ", "test -x ", "test -r ", "test -w ",
		"[ -f ", "[ -e ", "[ -d ", "[ -s ", "[ -x ", "[ -r ", "[ -w ",
		"[[ -f ", "[[ -e ", "[[ -d ", "[[ -s ", "[[ -x ", "[[ -r ", "[[ -w ",
		"stat ", "ls ", "find ", "which ", "command -v ", "chmod ", "chown ", "touch ",
	}
	for _, token := range weakTokens {
		if strings.Contains(line, token) || strings.HasPrefix(line, token) {
			return true
		}
	}
	return false
}

func checkTestsAnalysis(report *domain.LintReport, path string, strict bool) {
	if strings.TrimSpace(path) == "" {
		report.Add("tests_analysis_present", submissionStatus(strict), "tests analysis path not provided", "")
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		report.Add("tests_analysis_present", domain.CheckFail, "tests analysis cannot be read", path)
		return
	}
	text := string(raw)
	required := []string{"instruction 和 environment", "理论通过路径", "具备通过条件"}
	for _, section := range required {
		if !strings.Contains(text, section) {
			report.Add("tests_analysis_sections", domain.CheckFail, "tests analysis is missing required CodeEdge section: "+section, path)
			return
		}
	}
	report.Add("tests_analysis_sections", domain.CheckPass, "tests analysis includes required CodeEdge sections", path)
	sections := testsAnalysisSections(text, required)
	for _, section := range required {
		if !testsAnalysisSectionSubstantive(sections[section]) {
			report.Add("tests_analysis_substance", domain.CheckFail, "tests analysis section has no substantive bullet content: "+section, path)
			return
		}
	}
	report.Add("tests_analysis_substance", domain.CheckPass, "tests analysis sections include substantive bullet content", path)
	if !testsAnalysisAttributionClear(text) {
		report.Add("tests_analysis_attribution", domain.CheckFail, "tests analysis must explain visible instruction/environment facts and verifier/test derivation", path)
		return
	}
	report.Add("tests_analysis_attribution", domain.CheckPass, "tests analysis ties verifier/test checks back to visible task facts", path)
}

func testsAnalysisSections(text string, headings []string) map[string]string {
	sections := make(map[string]string, len(headings))
	for i, heading := range headings {
		start := strings.Index(text, heading)
		if start < 0 {
			continue
		}
		start += len(heading)
		end := len(text)
		for _, next := range headings[i+1:] {
			if idx := strings.Index(text[start:], next); idx >= 0 {
				end = start + idx
				break
			}
		}
		sections[heading] = strings.TrimSpace(text[start:end])
	}
	return sections
}

func testsAnalysisSectionSubstantive(section string) bool {
	lines := strings.Split(section, "\n")
	bullets := 0
	chars := 0
	for _, line := range lines {
		line = strings.TrimSpace(strings.TrimPrefix(line, "---"))
		if line == "" {
			continue
		}
		trimmed := strings.TrimLeft(line, "-*0123456789.、) ")
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(line, "-") || strings.HasPrefix(line, "*") || line[0] >= '0' && line[0] <= '9' {
			bullets++
		}
		chars += len([]rune(trimmed))
	}
	return bullets >= 1 && chars >= 40
}

func testsAnalysisAttributionClear(text string) bool {
	lower := strings.ToLower(text)
	hasVisibleFacts := strings.Contains(lower, "instruction") && strings.Contains(lower, "environment")
	hasVerifier := strings.Contains(lower, "verifier") || strings.Contains(lower, "tests/test.sh") || strings.Contains(lower, "test.sh")
	hasDerivation := strings.Contains(text, "推导") || strings.Contains(text, "依据") || strings.Contains(text, "隐藏") || strings.Contains(lower, "derive")
	return hasVisibleFacts && hasVerifier && hasDerivation
}

func checkHarborResult(report *domain.LintReport, id, path, taskDir string, qwen bool, strict bool) {
	if strings.TrimSpace(path) == "" {
		report.Add(id, submissionStatus(strict), "harbor result path not provided", "")
		return
	}
	result, err := harborrun.ParseFile(path)
	if err != nil {
		report.Add(id, domain.CheckFail, "harbor result cannot be read", path)
		return
	}
	expectedModel := "claude-opus-4-8"
	if qwen {
		expectedModel = "qwen3.7-max"
	}
	failures := harborrun.ValidateForCodeEdgeWithOptions(result, harborrun.ValidationOptions{
		Qwen:              qwen,
		ExpectedModel:     expectedModel,
		TaskDir:           taskDir,
		RequireRuns:       true,
		RequireTaskDigest: true,
		RequireCommandRun: strict,
	})
	if len(failures) > 0 {
		report.Add(id, domain.CheckFail, "harbor result does not satisfy CodeEdge threshold: "+strings.Join(failures, "; "), path)
		return
	}
	report.Add(id, domain.CheckPass, fmt.Sprintf("harbor result valid: trials=%d pass_count=%d average_turns=%.2f", result.Trials, result.PassCount, result.AverageTurns), path)
}

func checkScreenshot(report *domain.LintReport, id, path string, strict bool) {
	if strings.TrimSpace(path) == "" {
		report.Add(id, submissionStatus(strict), "pass@4 screenshot path not provided", "")
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		report.Add(id, domain.CheckFail, "pass@4 screenshot path is not a readable file", path)
		return
	}
	if info.Size() == 0 {
		report.Add(id, domain.CheckFail, "pass@4 screenshot file is empty", path)
		return
	}
	if !isScreenshotExtension(path) {
		report.Add(id, domain.CheckFail, "pass@4 screenshot must be a png, jpg, jpeg, or webp file", path)
		return
	}
	report.Add(id, domain.CheckPass, "pass@4 screenshot file exists and has an image extension", path)
}

func checkDistinctScreenshots(report *domain.LintReport, qwenPath, opusPath string, strict bool) {
	qwenPath = strings.TrimSpace(qwenPath)
	opusPath = strings.TrimSpace(opusPath)
	if qwenPath == "" || opusPath == "" {
		report.Add("screenshots_distinct", submissionStatus(strict), "both pass@4 screenshot paths are required to compare distinct evidence", "")
		return
	}
	qwenAbs, qwenErr := filepath.Abs(qwenPath)
	opusAbs, opusErr := filepath.Abs(opusPath)
	if qwenErr == nil && opusErr == nil && qwenAbs == opusAbs {
		report.Add("screenshots_distinct", domain.CheckFail, "Qwen and Opus pass@4 screenshots must be distinct files", qwenPath)
		return
	}
	report.Add("screenshots_distinct", domain.CheckPass, "Qwen and Opus pass@4 screenshot paths are distinct", "")
}

func readLower(report *domain.LintReport, id, path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		report.Add(id, domain.CheckFail, "cannot read file", path)
		return ""
	}
	return strings.ToLower(string(raw))
}

func suspicious(content string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.Contains(content, pattern) {
			return true
		}
	}
	return false
}

func safeID(value string) string {
	value = strings.ReplaceAll(value, string(filepath.Separator), "_")
	value = strings.ReplaceAll(value, "/", "_")
	value = strings.ReplaceAll(value, ".", "_")
	return value
}

func submissionStatus(strict bool) domain.CheckStatus {
	if strict {
		return domain.CheckFail
	}
	return domain.CheckWarn
}

func dockerfileCopiesWholeContext(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		instruction := strings.ToLower(fields[0])
		if instruction != "copy" && instruction != "add" {
			continue
		}
		for _, field := range fields[1 : len(fields)-1] {
			field = strings.Trim(field, "\"'")
			if field == "." || field == "./" || field == "*" || field == "./*" {
				return true
			}
		}
	}
	return false
}

func stripDockerfileComments(content string) string {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.Index(line, " #"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func firstDockerfileGitCloneURL(content string) string {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(strings.Trim(line, ";&|"))
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] != "git" || fields[i+1] != "clone" {
				continue
			}
			for _, field := range fields[i+2:] {
				field = strings.Trim(field, "\"';&|")
				if isLikelyGitURL(field) {
					return field
				}
			}
		}
	}
	return ""
}

func dockerfileChecksOutCommit(content, commit string) bool {
	commit = strings.ToLower(strings.TrimSpace(commit))
	if commit == "" {
		return false
	}
	for _, line := range strings.Split(content, "\n") {
		if !isGitCheckoutLine(line) {
			continue
		}
		for _, field := range strings.Fields(line) {
			if strings.Trim(strings.ToLower(field), "\"';&|") == commit {
				return true
			}
		}
	}
	return false
}

func dockerfileChecksOutConcreteCommit(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		if isGitCheckoutLine(line) && commitPattern.MatchString(line) {
			return true
		}
	}
	return false
}

func isGitCheckoutLine(line string) bool {
	fields := strings.Fields(line)
	for i := 0; i+2 < len(fields); i++ {
		if fields[i] != "git" {
			continue
		}
		switch fields[i+1] {
		case "checkout":
			return true
		case "reset":
			for _, field := range fields[i+2:] {
				if field == "--hard" {
					return true
				}
			}
		}
	}
	return false
}

func isLikelyGitURL(value string) bool {
	return filepath.IsAbs(value) || strings.Contains(value, "://") || strings.HasPrefix(value, "git@") || strings.Contains(value, "github.com/")
}

func normalizeRepoURL(value string) string {
	value = strings.TrimSpace(strings.Trim(value, "\"'"))
	value = strings.TrimSuffix(value, ".git")
	value = strings.TrimSuffix(value, "/")
	value = strings.ToLower(value)
	value = strings.TrimPrefix(value, "ssh://")
	if strings.HasPrefix(value, "git@github.com:") {
		value = "https://github.com/" + strings.TrimPrefix(value, "git@github.com:")
	}
	if strings.HasPrefix(value, "git@github.com/") {
		value = "https://github.com/" + strings.TrimPrefix(value, "git@github.com/")
	}
	value = strings.Replace(value, "https://www.github.com/", "https://github.com/", 1)
	value = strings.Replace(value, "http://github.com/", "https://github.com/", 1)
	return value
}

func isScreenshotExtension(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".webp":
		return true
	default:
		return false
	}
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func MarshalReport(report domain.LintReport) ([]byte, error) {
	report = sanitize.LintReport(report)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal lint report: %w", err)
	}
	return append(data, '\n'), nil
}
