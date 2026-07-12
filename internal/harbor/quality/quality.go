package quality

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/sanitize"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

const qualityRubricVersion = "codeedge-quality-rubric-v1"

type Options struct {
	TaskDir             string
	Workspace           string
	RepoURL             string
	Commit              string
	TestsAnalysisPath   string
	Proposal            *domain.TaskProposal
	Agent               workflow.AgentRuntime
	Model               string
	ReasoningEffort     string
	AgentTimeoutSeconds int
	WriteReport         string
}

func Run(ctx context.Context, opts Options) (domain.QualityReport, error) {
	taskDir := strings.TrimSpace(opts.TaskDir)
	report := domain.QualityReport{
		SchemaVersion:     "harbor.quality_report.v1",
		TaskDir:           taskDir,
		Checks:            map[string]domain.QualityCheck{},
		OverallPass:       true,
		RubricFingerprint: fingerprint(qualityRubricVersion),
		CreatedAt:         time.Now().UTC(),
	}
	if taskDir == "" {
		add(&report, "task_dir", false, "error", "task directory is required", "deterministic")
		return finish(report, opts.WriteReport)
	}
	taskDigest, err := harborrun.ComputeTaskDigest(taskDir)
	if err != nil {
		add(&report, "task_digest", false, "error", "task digest cannot be computed: "+err.Error(), "deterministic")
		return finish(report, opts.WriteReport)
	}
	report.TaskDigest = taskDigest
	files, err := readTaskFiles(taskDir, opts.TestsAnalysisPath)
	if err != nil {
		add(&report, "task_files_readable", false, "error", err.Error(), "deterministic")
		return finish(report, opts.WriteReport)
	}
	add(&report, "task_files_readable", true, "info", "required task files were read", "deterministic")
	checkInstructionLeak(&report, files)
	checkIssuePRSimilarityRisk(&report, files)
	checkTestLooseness(&report, files)
	checkTestStrictness(&report, files)
	checkSolveBypass(&report, files)
	checkInfraRisk(&report, files)
	checkInstructionTestAlignment(&report, files, opts.Proposal)
	checkTestsAnalysisConsistency(&report, files)
	if opts.Agent != nil {
		if err := runAgentCheck(ctx, opts, files, &report); err != nil {
			add(&report, "agent_quality_check", false, "warning", err.Error(), "agent")
		}
	} else {
		add(&report, "agent_quality_check", true, "info", "agent semantic check not configured; deterministic checks only", "deterministic")
	}
	return finish(report, opts.WriteReport)
}

type taskFiles struct {
	Instruction   string
	TaskTOML      string
	Dockerfile    string
	Compose       string
	Solve         string
	Test          string
	TestsAnalysis string
}

func readTaskFiles(taskDir, testsAnalysisPath string) (taskFiles, error) {
	read := func(rel string) string {
		data, _ := os.ReadFile(filepath.Join(taskDir, filepath.FromSlash(rel)))
		return string(data)
	}
	files := taskFiles{
		Instruction: read("instruction.md"),
		TaskTOML:    read("task.toml"),
		Dockerfile:  read("environment/Dockerfile"),
		Compose:     read("environment/docker-compose.yaml"),
		Solve:       read("solution/solve.sh"),
		Test:        read("tests/test.sh"),
	}
	missing := []string{}
	for rel, content := range map[string]string{
		"instruction.md":    files.Instruction,
		"task.toml":         files.TaskTOML,
		"solution/solve.sh": files.Solve,
		"tests/test.sh":     files.Test,
	} {
		if strings.TrimSpace(content) == "" {
			missing = append(missing, rel)
		}
	}
	if strings.TrimSpace(files.Dockerfile) == "" && strings.TrimSpace(files.Compose) == "" {
		missing = append(missing, "environment/Dockerfile or docker-compose.yaml")
	}
	if len(missing) > 0 {
		return files, fmt.Errorf("missing or empty task files: %s", strings.Join(missing, ", "))
	}
	if strings.TrimSpace(testsAnalysisPath) != "" {
		data, _ := os.ReadFile(testsAnalysisPath)
		files.TestsAnalysis = string(data)
	}
	return files, nil
}

func checkInstructionLeak(report *domain.QualityReport, files taskFiles) {
	lower := strings.ToLower(files.Instruction)
	forbidden := []string{"solution/solve.sh", "solve.sh", "/logs/verifier/reward", "reward.txt", "reward.json", "diff --git", "apply this patch"}
	for _, pattern := range forbidden {
		if strings.Contains(lower, pattern) {
			add(report, "instruction_leak", false, "error", "instruction appears to reveal verifier/oracle detail: "+pattern, "deterministic")
			return
		}
	}
	add(report, "instruction_leak", true, "info", "instruction has no obvious oracle/verifier leakage", "deterministic")
}

var issuePRPattern = regexp.MustCompile(`(?i)github\.com/[^[:space:])]+/(issues|pull)/|github issue|issue\s+#\d+|pull request\s+#\d+|\bpr\s+#\d+`)

func checkIssuePRSimilarityRisk(report *domain.QualityReport, files taskFiles) {
	combined := files.Instruction + "\n" + files.TaskTOML + "\n" + files.TestsAnalysis
	if issuePRPattern.MatchString(combined) {
		add(report, "github_issue_similarity", false, "warning", "task text explicitly references GitHub issue/PR identifiers; likely similarity risk", "deterministic")
		return
	}
	add(report, "github_issue_similarity", true, "info", "no explicit GitHub issue/PR reference found", "deterministic")
}

func checkTestLooseness(report *domain.QualityReport, files taskFiles) {
	lower := strings.ToLower(files.Test)
	meaningfulLines := nonCommentLines(files.Test)
	if len(meaningfulLines) < 5 {
		add(report, "test_looseness", false, "warning", "tests/test.sh is very short and may be too loose", "deterministic")
		return
	}
	if lower == "exit 0" || strings.Contains(lower, "\nexit 0\n") && len(meaningfulLines) <= 3 {
		add(report, "test_looseness", false, "error", "tests/test.sh appears to be a no-op", "deterministic")
		return
	}
	assertions := countAny(lower, []string{"grep", "test ", "[[", "[ ", "go test", "pytest", "cargo test", "npm test", "python -m", "diff", "cmp"})
	if assertions == 0 {
		add(report, "test_looseness", false, "warning", "tests/test.sh has no obvious assertion/test command", "deterministic")
		return
	}
	add(report, "test_looseness", true, "info", "tests/test.sh has non-trivial assertions", "deterministic")
}

func checkTestStrictness(report *domain.QualityReport, files taskFiles) {
	lower := strings.ToLower(files.Test)
	if strings.Contains(lower, "/solution") || strings.Contains(lower, "solution/solve.sh") {
		add(report, "test_strictness", false, "error", "tests/test.sh depends on solution internals", "deterministic")
		return
	}
	if strings.Contains(lower, "sha256sum") || strings.Contains(lower, "md5sum") {
		add(report, "test_strictness", false, "warning", "tests/test.sh may enforce exact implementation artifacts via checksum", "deterministic")
		return
	}
	add(report, "test_strictness", true, "info", "tests/test.sh has no obvious hidden solution-specific checks", "deterministic")
}

func checkSolveBypass(report *domain.QualityReport, files taskFiles) {
	lower := strings.ToLower(files.Solve)
	forbidden := []string{"tests/test.sh", "/tests/test.sh", "/logs/verifier/reward", "reward.txt", "reward.json", "chmod -r", "rm -rf /"}
	for _, pattern := range forbidden {
		if strings.Contains(lower, pattern) {
			add(report, "solve_bypass", false, "error", "solution appears to bypass verifier or modify tests: "+pattern, "deterministic")
			return
		}
	}
	if strings.Contains(lower, "curl ") && strings.Contains(lower, "| bash") {
		add(report, "solve_bypass", false, "warning", "solution downloads and executes remote shell content", "deterministic")
		return
	}
	add(report, "solve_bypass", true, "info", "solution has no obvious verifier bypass pattern", "deterministic")
}

func checkInfraRisk(report *domain.QualityReport, files taskFiles) {
	combined := strings.ToLower(files.Dockerfile + "\n" + files.Compose + "\n" + files.Solve + "\n" + files.Test)
	for _, pattern := range []string{"github_token", "private_token", "ssh://", "id_rsa", "password=", "secret_key"} {
		if strings.Contains(combined, pattern) {
			add(report, "infra_risk", false, "error", "task appears to depend on private credential or SSH resource: "+pattern, "deterministic")
			return
		}
	}
	if strings.Contains(combined, "sleep 60") || strings.Contains(combined, "sleep 120") {
		add(report, "infra_risk", false, "warning", "task scripts include long sleeps that may indicate unstable timing dependency", "deterministic")
		return
	}
	add(report, "infra_risk", true, "info", "no obvious credential or timing infra risk found", "deterministic")
}

func checkInstructionTestAlignment(report *domain.QualityReport, files taskFiles, proposal *domain.TaskProposal) {
	if proposal != nil && len(proposal.TargetFiles) > 0 {
		lowerInstruction := strings.ToLower(files.Instruction)
		lowerTest := strings.ToLower(files.Test)
		for _, target := range proposal.TargetFiles {
			base := strings.ToLower(filepath.Base(target))
			if base != "" && (strings.Contains(lowerInstruction, base) || strings.Contains(lowerTest, base)) {
				add(report, "instruction_test_alignment", true, "info", "target file/module appears in instruction or tests", "deterministic")
				return
			}
		}
		add(report, "instruction_test_alignment", false, "warning", "proposal target files do not appear in instruction or tests", "deterministic")
		return
	}
	if tokenOverlap(files.Instruction, files.Test) < 0.02 {
		add(report, "instruction_test_alignment", false, "warning", "instruction and tests have very low keyword overlap; check for mismatch", "deterministic")
		return
	}
	add(report, "instruction_test_alignment", true, "info", "instruction and tests have plausible keyword overlap", "deterministic")
}

func checkTestsAnalysisConsistency(report *domain.QualityReport, files taskFiles) {
	if strings.TrimSpace(files.TestsAnalysis) == "" {
		add(report, "tests_analysis_consistency", false, "warning", "tests analysis was not provided to quality check", "deterministic")
		return
	}
	required := []string{"instruction 和 environment", "理论通过路径", "具备通过条件"}
	for _, section := range required {
		if !strings.Contains(files.TestsAnalysis, section) {
			add(report, "tests_analysis_consistency", false, "error", "tests analysis missing required section: "+section, "deterministic")
			return
		}
	}
	add(report, "tests_analysis_consistency", true, "info", "tests analysis includes required CodeEdge sections", "deterministic")
}

func runAgentCheck(ctx context.Context, opts Options, files taskFiles, report *domain.QualityReport) error {
	timeout := opts.AgentTimeoutSeconds
	if timeout <= 0 {
		timeout = 300
	}
	prompt := buildAgentPrompt(opts, files)
	report.RequestedModel = strings.TrimSpace(opts.Model)
	report.ReasoningEffort = strings.TrimSpace(opts.ReasoningEffort)
	report.PromptFingerprint = fingerprint(prompt)
	report.ReviewFingerprint = fingerprint(strings.Join([]string{report.RubricFingerprint, report.PromptFingerprint, report.RequestedModel, report.ReasoningEffort}, "\n"))
	result, err := opts.Agent.Turn(ctx, workflow.AgentTurnRequest{
		ProjectPath:     opts.TaskDir,
		Prompt:          prompt,
		Model:           opts.Model,
		ReasoningEffort: opts.ReasoningEffort,
		SandboxMode:     "read-only",
		SandboxPolicy:   "readOnly",
		NetworkAccess:   false,
		WorkspaceRoots:  []string{opts.TaskDir},
		TimeoutSeconds:  timeout,
		MaxOutputBytes:  1 << 20,
		LogPath:         nodes.QualityAgentLogPath(opts.Workspace),
	})
	if err != nil {
		return fmt.Errorf("agent quality check: %w", err)
	}
	report.AgentModel = result.Model
	report.AgentOutput = truncate(result.Text, 12000)
	var parsed struct {
		Checks      map[string]domain.QualityCheck `json:"checks"`
		OverallPass *bool                          `json:"overall_pass"`
		Warnings    []string                       `json:"warnings"`
		Issues      []string                       `json:"issues"`
	}
	raw, err := extractJSONObject(result.Text)
	if err != nil {
		add(report, "agent_quality_check", false, "warning", "agent did not return parseable JSON; raw output retained", "agent")
		return nil
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		add(report, "agent_quality_check", false, "warning", "agent JSON did not match quality report schema: "+err.Error(), "agent")
		return nil
	}
	hasBlockingAgentCheck := false
	for id, check := range parsed.Checks {
		if check.Source == "" {
			check.Source = "agent"
		}
		report.Checks["agent_"+id] = check
		if !check.Passed && strings.EqualFold(check.Severity, "error") {
			hasBlockingAgentCheck = true
			report.OverallPass = false
			report.Issues = append(report.Issues, "agent_"+id+": "+check.Detail)
		} else if !check.Passed {
			report.Warnings = append(report.Warnings, "agent_"+id+": "+check.Detail)
		}
	}
	report.Warnings = append(report.Warnings, parsed.Warnings...)
	if hasBlockingAgentCheck {
		report.Issues = append(report.Issues, parsed.Issues...)
	} else {
		report.Warnings = append(report.Warnings, parsed.Issues...)
	}
	if parsed.OverallPass != nil && !*parsed.OverallPass && !hasBlockingAgentCheck {
		report.Warnings = append(report.Warnings, "agent reported overall_pass=false without a failed error-severity check; treated as advisory")
	}
	add(report, "agent_quality_check", true, "info", "agent semantic check completed", "agent")
	return nil
}

func fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("sha256:%x", sum[:])
}

func buildAgentPrompt(opts Options, files taskFiles) string {
	proposal := ""
	if opts.Proposal != nil {
		data, _ := json.MarshalIndent(opts.Proposal, "", "  ")
		proposal = string(data)
	}
	return fmt.Sprintf(`你是 CodeEdge Harbor 任务质量审查 Agent。只基于下面提供的本地任务文件做语义审查，不要声称你已访问外部 GitHub issue/TB3 数据集。

审查重点:
1. instruction 是否泄露答案、测试内部断言、solve.sh 或 reward 细节。
2. tests 是否过松、过严、只贴合标准答案实现细节，或与 instruction 不一致。
3. solve.sh 是否可信、可复核、没有绕过 tests/reward。
4. 是否存在明显 GitHub issue/PR 直接改写痕迹。
5. 失败是否可能来自 infra/network/token/path/permission，而不是模型能力。

只返回 JSON 对象:
{
  "overall_pass": true,
  "checks": {
    "instruction_leak": {"passed": true, "severity": "info", "detail": "..."},
    "github_issue_similarity": {"passed": true, "severity": "warning", "detail": "..."},
    "test_looseness": {"passed": true, "severity": "info", "detail": "..."},
    "test_strictness": {"passed": true, "severity": "info", "detail": "..."},
    "instruction_test_alignment": {"passed": true, "severity": "info", "detail": "..."},
    "solve_bypass": {"passed": true, "severity": "info", "detail": "..."}
  },
  "warnings": [],
  "issues": []
}

repo_url: %s
commit: %s
task_proposal:
%s

instruction.md:
%s

task.toml:
%s

environment/Dockerfile:
%s

environment/docker-compose.yaml:
%s

solution/solve.sh:
%s

tests/test.sh:
%s

tests_analysis:
%s
`, opts.RepoURL, opts.Commit, proposal, files.Instruction, files.TaskTOML, files.Dockerfile, files.Compose, files.Solve, files.Test, files.TestsAnalysis)
}

func add(report *domain.QualityReport, id string, passed bool, severity, detail, source string) {
	report.Checks[id] = domain.QualityCheck{Passed: passed, Severity: severity, Detail: detail, Source: source}
	if !passed && strings.EqualFold(severity, "error") {
		report.OverallPass = false
		report.Issues = append(report.Issues, id+": "+detail)
		return
	}
	if !passed {
		report.Warnings = append(report.Warnings, id+": "+detail)
	}
}

func finish(report domain.QualityReport, writePath string) (domain.QualityReport, error) {
	report = sanitize.QualityReport(report)
	if strings.TrimSpace(writePath) == "" {
		return report, nil
	}
	if err := os.MkdirAll(filepath.Dir(writePath), 0o700); err != nil {
		return report, err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return report, err
	}
	return report, os.WriteFile(writePath, append(data, '\n'), 0o600)
}

func nonCommentLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func countAny(text string, tokens []string) int {
	count := 0
	for _, token := range tokens {
		if strings.Contains(text, token) {
			count++
		}
	}
	return count
}

var wordPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{2,}`)

func tokenOverlap(a, b string) float64 {
	ma := map[string]bool{}
	for _, word := range wordPattern.FindAllString(strings.ToLower(a), -1) {
		ma[word] = true
	}
	mb := map[string]bool{}
	for _, word := range wordPattern.FindAllString(strings.ToLower(b), -1) {
		mb[word] = true
	}
	if len(ma) == 0 || len(mb) == 0 {
		return 0
	}
	intersection := 0
	for word := range ma {
		if mb[word] {
			intersection++
		}
	}
	return float64(intersection) / float64(len(ma)+len(mb)-intersection)
}

func extractJSONObject(text string) ([]byte, error) {
	data := []byte(text)
	for idx, b := range data {
		if b != '{' {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(string(data[idx:])))
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err == nil && len(raw) > 0 {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("no JSON object found")
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "\n... truncated ..."
}

func defaultString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}
