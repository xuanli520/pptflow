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
)

type RunRecord struct {
	RunID          string `json:"run_id"`
	TaskID         string `json:"task_id"`
	StartedAt      string `json:"started_at"`
	FinishedAt     string `json:"finished_at,omitempty"`
	Status         string `json:"status"`
	ManualVerdict  string `json:"manual_verdict"`
	StaticOnly     bool   `json:"static_only"`
	DurationMS     int64  `json:"duration_ms"`
	ArtifactRoot   string `json:"artifact_root"`
	ToolVersions   string `json:"tool_versions,omitempty"`
	PromptVersions string `json:"prompt_versions,omitempty"`
}

type StageRecord struct {
	Stage         string    `json:"stage"`
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	BlockedBy     []string  `json:"blocked_by,omitempty"`
	StartedAt     string    `json:"started_at,omitempty"`
	FinishedAt    string    `json:"finished_at,omitempty"`
	DurationMS    int64     `json:"duration_ms"`
	LogPath       string    `json:"log_path,omitempty"`
	ArtifactPaths []string  `json:"artifact_paths"`
	ErrorSummary  string    `json:"error_summary,omitempty"`
	Findings      []Finding `json:"findings,omitempty"`
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
