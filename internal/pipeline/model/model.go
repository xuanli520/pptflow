package model

const (
	StagePending = "pending"
	StageRunning = "running"
	StageDone    = "done"
	StageFailed  = "failed"
	StageBlocked = "blocked"
	StageSkipped = "skipped"

	RunRunning               = "running"
	RunCompletedClean        = "completed_clean"
	RunCompletedWithFindings = "completed_with_findings"
	RunAborted               = "aborted"
	RunCrashed               = "crashed"

	ManualUnset  = "unset"
	ManualPass   = "pass"
	ManualRework = "rework"
	ManualFail   = "fail"

	TaskInspecting    = "inspecting"
	TaskWaitingManual = "waiting_manual"
	TaskCompleted     = "completed"
)

type RunRecord struct {
	RunID           string `json:"run_id"`
	TaskID          string `json:"task_id"`
	StartedAt       string `json:"started_at"`
	FinishedAt      string `json:"finished_at,omitempty"`
	Status          string `json:"status"`
	ManualVerdict   string `json:"manual_verdict"`
	StaticOnly      bool   `json:"static_only"`
	DurationMS      int64  `json:"duration_ms"`
	ArtifactRoot    string `json:"artifact_root"`
	ToolVersions    string `json:"tool_versions,omitempty"`
	PromptVersions  string `json:"prompt_versions,omitempty"`
	CompletionRound int    `json:"completion_round,omitempty"`
}

type Task struct {
	ID               string      `json:"id"`
	BatchID          string      `json:"batch_id"`
	GitURL           string      `json:"git_url"`
	RepoPath         string      `json:"repo_path"`
	State            string      `json:"state"`
	CurrentRunID     string      `json:"current_run_id,omitempty"`
	CompletionCount  int         `json:"completion_count"`
	FrontendURL      string      `json:"frontend_url,omitempty"`
	DockerRunning    bool        `json:"docker_running"`
	ComposeMeta      ComposeMeta `json:"compose_meta,omitempty"`
	EnteredWaitingAt string      `json:"entered_waiting_at,omitempty"`
	LastCompletedAt  string      `json:"last_completed_at,omitempty"`
	ArchivedAt       string      `json:"archived_at,omitempty"`
	SyncError        string      `json:"sync_error,omitempty"`
	CreatedAt        string      `json:"created_at"`
	UpdatedAt        string      `json:"updated_at"`
}

type Batch struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	TaskCount   int    `json:"task_count"`
	MaxTasks    int    `json:"max_tasks"`
	CreatedAt   string `json:"created_at"`
	IsFull      bool   `json:"is_full"`
}

type ComposeMeta struct {
	Project      string        `json:"project"`
	ComposeFiles []string      `json:"compose_files,omitempty"`
	WorkDir      string        `json:"work_dir,omitempty"`
	Ports        []ServicePort `json:"ports,omitempty"`
}

type ServicePort struct {
	Service   string `json:"service,omitempty"`
	URL       string `json:"url,omitempty"`
	Host      int    `json:"host,omitempty"`
	Container int    `json:"container,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
}

type StageRecord struct {
	Stage            string            `json:"stage"`
	Name             string            `json:"name"`
	Status           string            `json:"status"`
	BlockedBy        []string          `json:"blocked_by,omitempty"`
	StartedAt        string            `json:"started_at,omitempty"`
	FinishedAt       string            `json:"finished_at,omitempty"`
	DurationMS       int64             `json:"duration_ms"`
	LogPath          string            `json:"log_path,omitempty"`
	ArtifactPaths    []string          `json:"artifact_paths"`
	ErrorSummary     string            `json:"error_summary,omitempty"`
	Findings         []Finding         `json:"findings,omitempty"`
	ArtifactWarnings []ArtifactWarning `json:"artifact_warnings,omitempty"`
}

type Finding struct {
	ID           string `json:"id"`
	Stage        string `json:"stage"`
	Severity     string `json:"severity"`
	Title        string `json:"title"`
	Rule         string `json:"rule,omitempty"`
	Evidence     string `json:"evidence,omitempty"`
	Impact       string `json:"impact,omitempty"`
	DoneCriteria string `json:"done_criteria,omitempty"`
	MinimumFix   string `json:"minimum_fix,omitempty"`
	SourcePath   string `json:"source_path,omitempty"`
}

type StageStatusFile struct {
	RunID  string        `json:"run_id"`
	Stages []StageRecord `json:"stages"`
}

type ArtifactWarning struct {
	Path       string `json:"path"`
	Op         string `json:"op"`
	Error      string `json:"error"`
	Required   bool   `json:"required,omitempty"`
	RecordedAt string `json:"recorded_at,omitempty"`
}

func (w ArtifactWarning) OK() bool {
	return w.Error == ""
}
