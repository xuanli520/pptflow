package domain

import "time"

type RepoPrepared struct {
	SchemaVersion   string       `json:"schema_version"`
	RepoURL         string       `json:"repo_url"`
	RequestedCommit string       `json:"requested_commit"`
	ResolvedCommit  string       `json:"resolved_commit"`
	TreeHash        string       `json:"tree_hash"`
	SourcePath      string       `json:"source_path"`
	CommandLogs     []CommandRun `json:"command_logs,omitempty"`
	PreparedAt      time.Time    `json:"prepared_at"`
}

type CheckStatus string

const (
	CheckPass CheckStatus = "pass"
	CheckWarn CheckStatus = "warn"
	CheckFail CheckStatus = "fail"
)

type CheckResult struct {
	ID      string      `json:"id"`
	Status  CheckStatus `json:"status"`
	Message string      `json:"message"`
	Path    string      `json:"path,omitempty"`
}

type LintReport struct {
	SchemaVersion string        `json:"schema_version"`
	TaskDir       string        `json:"task_dir"`
	RepoURL       string        `json:"repo_url,omitempty"`
	Commit        string        `json:"commit,omitempty"`
	Passed        bool          `json:"passed"`
	Checks        []CheckResult `json:"checks"`
	CreatedAt     time.Time     `json:"created_at"`
}

func (r *LintReport) Add(id string, status CheckStatus, message string, path string) {
	r.Checks = append(r.Checks, CheckResult{ID: id, Status: status, Message: message, Path: path})
	if status == CheckFail {
		r.Passed = false
	}
}

type RunnerEvent struct {
	RunID     string            `json:"run_id,omitempty"`
	Type      string            `json:"type"`
	NodeID    string            `json:"node_id,omitempty"`
	Status    string            `json:"status,omitempty"`
	Message   string            `json:"message,omitempty"`
	Path      string            `json:"path,omitempty"`
	Artifacts []ArtifactPreview `json:"artifacts,omitempty"`
	Logs      []ArtifactPreview `json:"logs,omitempty"`
	Gate      *GateRequest      `json:"gate,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

type RunSummary struct {
	RunID             string            `json:"run_id,omitempty"`
	Workspace         string            `json:"workspace"`
	RepoPrepared      *RepoPrepared     `json:"repo_prepared,omitempty"`
	GenReport         *GenReport        `json:"gen_report,omitempty"`
	LintReport        *LintReport       `json:"lint_report,omitempty"`
	VerifyReport      *VerifyReport     `json:"verify_report,omitempty"`
	QualityReport     *QualityReport    `json:"quality_report,omitempty"`
	SimilarityReport  *SimilarityReport `json:"similarity_report,omitempty"`
	QwenResult        *TrialResult      `json:"qwen_result,omitempty"`
	OpusResult        *TrialResult      `json:"opus_result,omitempty"`
	PackageReport     *PackageReport    `json:"package_report,omitempty"`
	GateDecisions     []GateDecision    `json:"gate_decisions,omitempty"`
	StartedAt         time.Time         `json:"started_at"`
	FinishedAt        time.Time         `json:"finished_at"`
	Status            string            `json:"status,omitempty"`
	Passed            bool              `json:"passed"`
	Events            []RunnerEvent     `json:"events"`
	PersistenceErrors []string          `json:"persistence_errors,omitempty"`
}

type RunnerOptionsSnapshot struct {
	SchemaVersion            string    `json:"schema_version"`
	Workspace                string    `json:"workspace"`
	RepoURL                  string    `json:"repo_url,omitempty"`
	Commit                   string    `json:"commit,omitempty"`
	AllowLocalRepo           bool      `json:"allow_local_repo,omitempty"`
	TaskDir                  string    `json:"task_dir,omitempty"`
	Generate                 bool      `json:"generate"`
	TaskOutputDir            string    `json:"task_output_dir,omitempty"`
	TestsAnalysis            string    `json:"tests_analysis,omitempty"`
	QwenResult               string    `json:"qwen_result,omitempty"`
	OpusResult               string    `json:"opus_result,omitempty"`
	AutoApprove              bool      `json:"auto_approve"`
	VerifyDocker             bool      `json:"verify_docker"`
	QualityCheck             bool      `json:"quality_check"`
	QualityAgent             bool      `json:"quality_agent"`
	SimilarityCheck          bool      `json:"similarity_check"`
	SimilarityGitHub         bool      `json:"similarity_github"`
	SimilarityHistoryDirs    []string  `json:"similarity_history_dirs,omitempty"`
	SimilarityTB3Dirs        []string  `json:"similarity_tb3_dirs,omitempty"`
	SimilarityThreshold      float64   `json:"similarity_threshold,omitempty"`
	GitHubTokenConfigured    bool      `json:"github_credential_configured,omitempty"`
	RunHarbor                bool      `json:"run_harbor"`
	HarborAgent              string    `json:"harbor_agent,omitempty"`
	HarborAgentEnvKeys       []string  `json:"harbor_agent_env_names,omitempty"`
	HarborAgentEnvOmitted    bool      `json:"harbor_agent_env_omitted,omitempty"`
	QwenModel                string    `json:"qwen_model,omitempty"`
	OpusModel                string    `json:"opus_model,omitempty"`
	HarborTimeout            int       `json:"harbor_timeout,omitempty"`
	Package                  bool      `json:"package"`
	OutputDir                string    `json:"output_dir,omitempty"`
	StrictSubmission         bool      `json:"strict_submission"`
	TaskName                 string    `json:"task_name,omitempty"`
	CodeLang                 string    `json:"code_lang,omitempty"`
	TaskType                 string    `json:"task_type,omitempty"`
	Application              string    `json:"application,omitempty"`
	AHT                      string    `json:"aht,omitempty"`
	Description              string    `json:"description,omitempty"`
	IsZeroToOne              bool      `json:"is_0_to_1"`
	QwenScreenshot           string    `json:"qwen_screenshot,omitempty"`
	OpusScreenshot           string    `json:"opus_screenshot,omitempty"`
	Model                    string    `json:"model,omitempty"`
	Reasoning                string    `json:"reasoning,omitempty"`
	CodexPath                string    `json:"codex_path,omitempty"`
	AgentTimeout             int       `json:"agent_timeout,omitempty"`
	SensitiveFieldsOmitted   []string  `json:"sensitive_fields_omitted,omitempty"`
	UnsupportedFieldsOmitted []string  `json:"unsupported_fields_omitted,omitempty"`
	CreatedAt                time.Time `json:"created_at"`
}

type RepoModule struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
}

type TaskArea struct {
	Area               string `json:"area"`
	Module             string `json:"module"`
	Description        string `json:"description"`
	Difficulty         string `json:"difficulty"`
	TypeSuggestion     string `json:"type_suggestion"`
	AffectedFilesCount int    `json:"affected_files_count"`
}

type RepoAnalysis struct {
	SchemaVersion      string       `json:"schema_version"`
	RepoURL            string       `json:"repo_url"`
	CommitSHA          string       `json:"commit_sha"`
	Language           string       `json:"language"`
	LanguageVersion    string       `json:"language_version,omitempty"`
	BuildSystem        string       `json:"build_system"`
	TestFramework      string       `json:"test_framework"`
	KeyModules         []RepoModule `json:"key_modules,omitempty"`
	EntryPoints        []string     `json:"entry_points,omitempty"`
	Dependencies       []string     `json:"dependencies,omitempty"`
	PotentialTaskAreas []TaskArea   `json:"potential_task_areas,omitempty"`
}

type TaskProposal struct {
	SchemaVersion         string   `json:"schema_version"`
	TaskName              string   `json:"task_name"`
	OneLineDescription    string   `json:"one_line_description"`
	CodeLang              string   `json:"code_lang"`
	TaskType              string   `json:"task_type"`
	Application           string   `json:"application"`
	IsZeroToOne           bool     `json:"is_0_to_1"`
	GitHubLink            string   `json:"github_link"`
	CommitSHA             string   `json:"commit_sha"`
	EstimatedAHTMinutes   int      `json:"estimated_aht_minutes"`
	TargetFiles           []string `json:"target_files,omitempty"`
	AffectedModules       []string `json:"affected_modules,omitempty"`
	DifficultyRationale   string   `json:"difficulty_rationale"`
	BoundaryConditions    []string `json:"boundary_conditions,omitempty"`
	SuggestedVerification string   `json:"suggested_verification"`
	SetupCommands         []string `json:"setup_commands,omitempty"`
}

type GeneratedTaskFiles struct {
	SchemaVersion      string `json:"schema_version"`
	RepoURL            string `json:"repo_url,omitempty"`
	CommitSHA          string `json:"commit_sha,omitempty"`
	TaskProposalDigest string `json:"task_proposal_digest,omitempty"`
	InstructionMD      string `json:"instruction_md"`
	SolveSH            string `json:"solve_sh"`
	TestSH             string `json:"test_sh"`
	TestsAnalysis      string `json:"tests_analysis_md"`
	ExtraNotes         string `json:"extra_notes,omitempty"`
}

type TrialRun struct {
	Trial           int     `json:"trial"`
	Passed          bool    `json:"passed"`
	Turns           int     `json:"turns,omitempty"`
	DurationSeconds int     `json:"duration_seconds,omitempty"`
	Reward          float64 `json:"reward,omitempty"`
	FailureReason   string  `json:"failure_reason,omitempty"`
}

type ResultFileEvidence struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type TrialResult struct {
	SchemaVersion      string               `json:"schema_version"`
	Model              string               `json:"model"`
	Agent              string               `json:"agent,omitempty"`
	Trials             int                  `json:"trials"`
	PassCount          int                  `json:"pass_count"`
	PassAt4            float64              `json:"pass_at_4"`
	AverageTurns       float64              `json:"average_turns"`
	Runs               []TrialRun           `json:"runs,omitempty"`
	ResultPath         string               `json:"result_path,omitempty"`
	RawResultPath      string               `json:"raw_result_path,omitempty"`
	RawResultSHA256    string               `json:"raw_result_sha256,omitempty"`
	RawTrialResults    []ResultFileEvidence `json:"raw_trial_results,omitempty"`
	TaskDigest         string               `json:"task_digest,omitempty"`
	HarborTaskChecksum string               `json:"harbor_task_checksum,omitempty"`
	TaskPath           string               `json:"task_path,omitempty"`
	CommandRunPath     string               `json:"command_run_path,omitempty"`
	Screenshot         string               `json:"screenshot,omitempty"`
	CreatedAt          time.Time            `json:"created_at,omitempty"`
}

type QualityCheck struct {
	Passed   bool   `json:"passed"`
	Severity string `json:"severity"`
	Detail   string `json:"detail"`
	Source   string `json:"source,omitempty"`
}

type QualityReport struct {
	SchemaVersion string                  `json:"schema_version"`
	TaskDir       string                  `json:"task_dir"`
	Checks        map[string]QualityCheck `json:"checks"`
	OverallPass   bool                    `json:"overall_pass"`
	Warnings      []string                `json:"warnings,omitempty"`
	Issues        []string                `json:"issues,omitempty"`
	AgentModel    string                  `json:"agent_model,omitempty"`
	AgentOutput   string                  `json:"agent_output,omitempty"`
	CreatedAt     time.Time               `json:"created_at"`
}

type SimilarityCandidate struct {
	Source       string   `json:"source"`
	Title        string   `json:"title,omitempty"`
	Path         string   `json:"path,omitempty"`
	URL          string   `json:"url,omitempty"`
	Score        float64  `json:"score"`
	MatchedTerms []string `json:"matched_terms,omitempty"`
}

type SimilaritySourceEvidence struct {
	Source           string `json:"source"`
	Kind             string `json:"kind"`
	Path             string `json:"path,omitempty"`
	ScannedFileCount int    `json:"scanned_file_count"`
	SourceDigest     string `json:"source_digest,omitempty"`
	QueryCount       int    `json:"query_count,omitempty"`
	ResultCount      int    `json:"result_count,omitempty"`
	HTTPStatuses     []int  `json:"http_statuses,omitempty"`
}

type SimilarityReport struct {
	SchemaVersion     string                     `json:"schema_version"`
	TaskDir           string                     `json:"task_dir"`
	TaskDigest        string                     `json:"task_digest,omitempty"`
	RepoURL           string                     `json:"repo_url,omitempty"`
	Sources           []string                   `json:"sources,omitempty"`
	SuccessfulSources []string                   `json:"successful_sources,omitempty"`
	ScannedFileCount  int                        `json:"scanned_file_count,omitempty"`
	SourceEvidence    []SimilaritySourceEvidence `json:"source_evidence,omitempty"`
	Threshold         float64                    `json:"threshold"`
	MaxScore          float64                    `json:"max_score"`
	Candidates        []SimilarityCandidate      `json:"candidates,omitempty"`
	OverallPass       bool                       `json:"overall_pass"`
	Warnings          []string                   `json:"warnings,omitempty"`
	Issues            []string                   `json:"issues,omitempty"`
	CreatedAt         time.Time                  `json:"created_at"`
}

type GenReport struct {
	SchemaVersion     string       `json:"schema_version"`
	TaskDir           string       `json:"task_dir"`
	TestsAnalysisPath string       `json:"tests_analysis_path"`
	RepoAnalysisPath  string       `json:"repo_analysis_path"`
	TaskProposalPath  string       `json:"task_proposal_path"`
	TaskFilesPath     string       `json:"task_files_path"`
	RepoAnalysis      RepoAnalysis `json:"repo_analysis"`
	TaskProposal      TaskProposal `json:"task_proposal"`
	CreatedAt         time.Time    `json:"created_at"`
	Passed            bool         `json:"passed"`
}

type ChecklistItem struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Critical bool   `json:"critical"`
	Passed   bool   `json:"passed"`
}

type ArtifactPreview struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
}

type GateRequest struct {
	RequestID string            `json:"request_id"`
	GateID    string            `json:"gate_id"`
	GateName  string            `json:"gate_name"`
	NodeID    string            `json:"node_id"`
	Message   string            `json:"message"`
	Checklist []ChecklistItem   `json:"checklist,omitempty"`
	Artifacts []ArtifactPreview `json:"artifacts,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

type GateDecision struct {
	RequestID   string            `json:"request_id"`
	GateID      string            `json:"gate_id"`
	Approved    bool              `json:"approved"`
	Notes       string            `json:"notes,omitempty"`
	EditedFiles map[string]string `json:"edited_files,omitempty"`
	DecidedAt   time.Time         `json:"decided_at"`
}

type CommandRun struct {
	Name         string    `json:"name"`
	Command      string    `json:"command"`
	Argv         []string  `json:"argv,omitempty"`
	Dir          string    `json:"cwd,omitempty"`
	Env          []string  `json:"env,omitempty"`
	Attempt      int       `json:"attempt,omitempty"`
	ExitCode     int       `json:"exit_code"`
	Stdout       string    `json:"stdout,omitempty"`
	Stderr       string    `json:"stderr,omitempty"`
	StdoutPath   string    `json:"stdout_path,omitempty"`
	StderrPath   string    `json:"stderr_path,omitempty"`
	Timeout      bool      `json:"timeout,omitempty"`
	FailureClass string    `json:"failure_class,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	DurationMS   int64     `json:"duration_ms"`
	Passed       bool      `json:"passed"`
}

type VerifyReport struct {
	SchemaVersion       string       `json:"schema_version"`
	TaskDir             string       `json:"task_dir"`
	TaskDigest          string       `json:"task_digest,omitempty"`
	ImageTag            string       `json:"image_tag"`
	DockerBuild         *CommandRun  `json:"docker_build,omitempty"`
	InitialVerify       *CommandRun  `json:"initial_verify,omitempty"`
	InitialExposesIssue bool         `json:"initial_exposes_issue"`
	OracleVerify        *CommandRun  `json:"oracle_verify,omitempty"`
	Cleanup             *CommandRun  `json:"cleanup,omitempty"`
	CommandLogs         []CommandRun `json:"command_logs,omitempty"`
	Passed              bool         `json:"passed"`
	CreatedAt           time.Time    `json:"created_at"`
}

type PackageReport struct {
	SchemaVersion string    `json:"schema_version"`
	TaskDir       string    `json:"task_dir"`
	OutputZip     string    `json:"output_zip"`
	ReportPath    string    `json:"report_path"`
	TaskName      string    `json:"task_name"`
	CreatedAt     time.Time `json:"created_at"`
	Passed        bool      `json:"passed"`
}
