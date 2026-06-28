package workflow

import (
	"context"
	"io"
	"time"
)

type WorkflowDefinition struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Nodes  []NodeSpec `json:"nodes"`
	Edges  []EdgeSpec `json:"edges,omitempty"`
	Policy Policy     `json:"policy,omitempty"`
}

type Policy struct {
	MaxNodes int `json:"max_nodes,omitempty"`
}

type NodeSpec struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	PluginID  string         `json:"plugin_id"`
	Name      string         `json:"name"`
	DependsOn []string       `json:"depends_on,omitempty"`
	Inputs    []ArtifactRef  `json:"inputs,omitempty"`
	Config    map[string]any `json:"config,omitempty"`
	Policy    NodePolicy     `json:"policy,omitempty"`
}

type NodePolicy struct {
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	MaxAttempts    int `json:"max_attempts,omitempty"`
}

type EdgeSpec struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type ArtifactRef struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Path      string            `json:"path"`
	Type      string            `json:"type"`
	Producer  string            `json:"producer"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

type ArtifactMeta = ArtifactRef

type Event struct {
	RunID     string         `json:"run_id"`
	NodeID    string         `json:"node_id,omitempty"`
	Type      string         `json:"type"`
	Message   string         `json:"message,omitempty"`
	Fields    map[string]any `json:"fields,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

type NodeStatus string

const (
	NodePending   NodeStatus = "pending"
	NodeRunning   NodeStatus = "running"
	NodeSucceeded NodeStatus = "succeeded"
	NodeFailed    NodeStatus = "failed"
	NodeSkipped   NodeStatus = "skipped"
)

type RunStatus string

const (
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

type TokenUsage struct {
	Input  int `json:"input,omitempty"`
	Output int `json:"output,omitempty"`
	Total  int `json:"total,omitempty"`
}

type NodeMetrics struct {
	Model       string     `json:"model,omitempty"`
	TokenUsage  TokenUsage `json:"token_usage,omitempty"`
	RetryCount  int        `json:"retry_count,omitempty"`
	FailureType string     `json:"failure_type,omitempty"`
}

type NodeRun struct {
	NodeID     string        `json:"node_id"`
	Kind       string        `json:"kind"`
	Name       string        `json:"name"`
	Status     NodeStatus    `json:"status"`
	StartedAt  time.Time     `json:"started_at,omitempty"`
	FinishedAt time.Time     `json:"finished_at,omitempty"`
	DurationMS int64         `json:"duration_ms,omitempty"`
	Artifacts  []ArtifactRef `json:"artifacts,omitempty"`
	Metrics    NodeMetrics   `json:"metrics,omitempty"`
	Error      string        `json:"error,omitempty"`
}

type RunRequest struct {
	Workflow      WorkflowDefinition `json:"workflow"`
	ArtifactRoot  string             `json:"artifact_root"`
	WorkspaceRoot string             `json:"workspace_root"`
	Input         map[string]any     `json:"input,omitempty"`
}

type RunResult struct {
	RunID         string        `json:"run_id"`
	WorkflowID    string        `json:"workflow_id"`
	Status        RunStatus     `json:"status"`
	ArtifactRoot  string        `json:"artifact_root"`
	WorkspaceRoot string        `json:"workspace_root"`
	Nodes         []NodeRun     `json:"nodes"`
	Artifacts     []ArtifactRef `json:"artifacts"`
	Events        []Event       `json:"events"`
	StartedAt     time.Time     `json:"started_at"`
	FinishedAt    time.Time     `json:"finished_at"`
	DurationMS    int64         `json:"duration_ms"`
}

type PluginManifest struct {
	ID      string   `json:"id"`
	Version string   `json:"version"`
	Kinds   []string `json:"kinds"`
}

type Plugin interface {
	Manifest() PluginManifest
	Validate(NodeSpec) error
	Execute(context.Context, NodeRequest) (NodeResult, error)
}

type NodeRequest struct {
	RunID         string
	Spec          NodeSpec
	ArtifactRoot  string
	WorkspaceRoot string
	Input         map[string]any
	Store         ArtifactStore
	Events        EventSink
	Runtimes      Runtimes
	Prior         map[string]NodeRun
}

type NodeResult struct {
	Artifacts []ArtifactRef
	Metrics   NodeMetrics
}

type PutArtifactRequest struct {
	Name     string
	Type     string
	Producer string
	Metadata map[string]string
	Content  io.Reader
}

type ArtifactStore interface {
	Put(context.Context, PutArtifactRequest) (ArtifactRef, error)
	PutJSON(context.Context, string, string, string, any) (ArtifactRef, error)
	PutText(context.Context, string, string, string, string) (ArtifactRef, error)
	Get(context.Context, ArtifactRef) (io.ReadCloser, ArtifactMeta, error)
	ReadJSON(context.Context, string, any) (ArtifactRef, error)
	List(context.Context, string) ([]ArtifactMeta, error)
	Path(string) (string, error)
	Root() string
}

type EventSink interface {
	Emit(context.Context, Event) error
}

type Runtimes struct {
	Command CommandRuntime
	Agent   AgentRuntime
	Image   ImageRuntime
}

type CommandRequest struct {
	Dir            string
	Env            []string
	Command        string
	Args           []string
	TimeoutSeconds int
}

type CommandResult struct {
	Command  string `json:"command"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Timeout  bool   `json:"timeout,omitempty"`
}

type CommandRuntime interface {
	Run(context.Context, CommandRequest) (CommandResult, error)
}

type AgentTurnRequest struct {
	ProjectPath       string
	Prompt            string
	Model             string
	SandboxMode       string
	SandboxPolicy     string
	NetworkAccess     bool
	TimeoutSeconds    int
	MaxOutputBytes    int
	CapabilitySummary string
	LogPath           string
}

type AgentTurnResult struct {
	Text       string     `json:"text"`
	Model      string     `json:"model,omitempty"`
	TokenUsage TokenUsage `json:"token_usage,omitempty"`
	Warnings   []string   `json:"warnings,omitempty"`
}

type AgentRuntime interface {
	Turn(context.Context, AgentTurnRequest) (AgentTurnResult, error)
}

type ImageRequest struct {
	Prompt         string
	Size           string
	Quality        string
	OutputPath     string
	TimeoutSeconds int
}

type ImageResult struct {
	Path    string `json:"path"`
	Model   string `json:"model"`
	Size    string `json:"size"`
	Quality string `json:"quality"`
	MIME    string `json:"mime"`
}

type ImageRuntime interface {
	Generate(context.Context, ImageRequest) (ImageResult, error)
	Configured() bool
}
