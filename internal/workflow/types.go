package workflow

import (
	"context"
	"io"
	"time"

	"github.com/purplevoid/harbor-factory/internal/runmodel"
)

type WorkflowDefinition struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Nodes  []NodeSpec `json:"nodes"`
	Edges  []EdgeSpec `json:"edges,omitempty"`
	Policy Policy     `json:"policy,omitempty"`
}

type Policy struct {
	MaxNodes     int `json:"max_nodes,omitempty"`
	MaxRevisions int `json:"max_revisions,omitempty"`
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
	TimeoutSeconds    int           `json:"timeout_seconds,omitempty"`
	MaxAttempts       int           `json:"max_attempts,omitempty"`
	RetryBackoffMS    int           `json:"retry_backoff_ms,omitempty"`
	RetryMaxBackoffMS int           `json:"retry_max_backoff_ms,omitempty"`
	Retryable         []FailureKind `json:"retryable_failure_types,omitempty"`
}

type EdgeSpec struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type ArtifactRef = runmodel.ArtifactRef

type ArtifactMeta = ArtifactRef
type Event = runmodel.Event
type ChecklistItem = runmodel.ChecklistItem
type ArtifactPreview = runmodel.ArtifactRef
type GateRequest = runmodel.GateRequest
type NodeStatus = runmodel.NodeStatus

const (
	NodePending   = runmodel.NodePending
	NodeRunning   = runmodel.NodeRunning
	NodeSucceeded = runmodel.NodeSucceeded
	NodeFailed    = runmodel.NodeFailed
	NodeCanceled  = runmodel.NodeCanceled
	NodeSkipped   = runmodel.NodeSkipped
	NodeRequeued  = runmodel.NodeRequeued
)

type RunStatus string

const (
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
	RunRunning   RunStatus = "running"
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
	Revision   int           `json:"revision,omitempty"`
	Attempt    int           `json:"attempt,omitempty"`
	StartedAt  time.Time     `json:"started_at,omitempty"`
	FinishedAt time.Time     `json:"finished_at,omitempty"`
	DurationMS int64         `json:"duration_ms,omitempty"`
	Artifacts  []ArtifactRef `json:"artifacts,omitempty"`
	Metrics    NodeMetrics   `json:"metrics,omitempty"`
	Error      string        `json:"error,omitempty"`
}

type RunRequest struct {
	RunID         string             `json:"run_id,omitempty"`
	Revision      int                `json:"revision,omitempty"`
	Workflow      WorkflowDefinition `json:"workflow"`
	ArtifactRoot  string             `json:"artifact_root"`
	WorkspaceRoot string             `json:"workspace_root"`
	Input         map[string]any     `json:"input,omitempty"`
	// Store and Events allow app.Runner to inject the workspace-scoped durable
	// infrastructure. When nil, Engine creates compatible filesystem defaults.
	Store  ArtifactStore `json:"-"`
	Events EventSink     `json:"-"`
	// Prior contains durable node snapshots from a previous invocation. Only
	// succeeded nodes are reused; all other nodes are scheduled again.
	Prior map[string]NodeRun `json:"-"`
	// Checkpoint runs after Engine atomically persists run_result.json. It is
	// intended for app.Runner state projection and must return an error if the
	// durable checkpoint cannot be accepted.
	Checkpoint func(context.Context, RunResult) error `json:"-"`
}

type RunResult struct {
	RunID         string        `json:"run_id"`
	WorkflowID    string        `json:"workflow_id"`
	Status        RunStatus     `json:"status"`
	Revision      int           `json:"revision,omitempty"`
	ActiveNodeID  string        `json:"active_node_id,omitempty"`
	ActiveAttempt int           `json:"active_attempt,omitempty"`
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
	Inputs        []ArtifactRef
	Attempt       int
	Revision      int
	Store         ArtifactStore
	Events        EventSink
	Runtimes      Runtimes
	Prior         map[string]NodeRun
}

type NodeResult struct {
	Artifacts []ArtifactRef
	Metrics   NodeMetrics
	Directive *NodeDirective
}

type DirectiveAction string

const DirectiveRequeue DirectiveAction = "requeue"

// NodeDirective requests a bounded state-machine transition without adding a
// cycle to the declarative DAG.
type NodeDirective struct {
	Action      DirectiveAction `json:"action"`
	RestartFrom string          `json:"restart_from"`
	Reason      string          `json:"reason,omitempty"`
}

type PutArtifactRequest struct {
	Name     string
	Type     string
	Producer string
	Metadata map[string]string
	Content  io.Reader
}

type RegisterArtifactRequest struct {
	Name     string
	Type     string
	Producer string
	Metadata map[string]string
	Path     string
}

type ArtifactStore interface {
	Put(context.Context, PutArtifactRequest) (ArtifactRef, error)
	PutJSON(context.Context, string, string, string, any) (ArtifactRef, error)
	PutText(context.Context, string, string, string, string) (ArtifactRef, error)
	Register(context.Context, RegisterArtifactRequest) (ArtifactRef, error)
	Get(context.Context, ArtifactRef) (io.ReadCloser, ArtifactMeta, error)
	ReadJSON(context.Context, string, any) (ArtifactRef, error)
	List(context.Context, string) ([]ArtifactMeta, error)
	Path(string) (string, error)
	Root() string
}

type ArtifactInvalidator interface {
	InvalidateProducers(context.Context, []string) error
}

type EventSink interface {
	Emit(context.Context, Event) error
}

type EventSubscriber interface {
	Subscribe(buffer int) (<-chan Event, func())
}

type FailureKind string

const (
	FailureUnknown   FailureKind = "unknown"
	FailureTransient FailureKind = "transient"
	FailureTimeout   FailureKind = "timeout"
	FailureRateLimit FailureKind = "rate_limit"
	FailureNetwork   FailureKind = "network"
	FailureCanceled  FailureKind = "canceled"
	FailurePermanent FailureKind = "permanent"
)

// ClassifiedError lets plugins distinguish retryable infrastructure failures
// from deterministic validation or domain failures.
type ClassifiedError interface {
	error
	FailureKind() FailureKind
	Retryable() bool
}

type Runtimes struct {
	Command    CommandRuntime
	Agent      AgentRuntime
	Evaluation EvaluationRuntime
}

type EvaluationRequest struct {
	NodeID              string
	TaskPath            string
	Model               string
	Agent               string
	AgentEnv            []string
	OutputDir           string
	TimeoutSeconds      int
	SetupTimeoutSeconds int
	AgentCacheDir       string
	Preflight           bool
	Concurrency         int
	Attempts            int
	InfraRetries        int
	Env                 []string
	Progress            func(line, source string)
}

type EvaluationResult struct {
	TrialResult EvaluationTrialResult `json:"trial_result"`
	CommandRun  CommandEvidence       `json:"command_run"`
}

type EvaluationTrialRun struct {
	Trial           int     `json:"trial"`
	Passed          bool    `json:"passed"`
	Turns           int     `json:"turns,omitempty"`
	DurationSeconds int     `json:"duration_seconds,omitempty"`
	Reward          float64 `json:"reward,omitempty"`
	FailureReason   string  `json:"failure_reason,omitempty"`
}

type EvaluationFileEvidence struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type EvaluationTrialResult struct {
	SchemaVersion       string                   `json:"schema_version"`
	Model               string                   `json:"model"`
	Agent               string                   `json:"agent,omitempty"`
	Trials              int                      `json:"trials"`
	PassCount           int                      `json:"pass_count"`
	PassAt4             float64                  `json:"pass_at_4"`
	AverageTurns        float64                  `json:"average_turns"`
	Runs                []EvaluationTrialRun     `json:"runs,omitempty"`
	ResultPath          string                   `json:"result_path,omitempty"`
	RawResultPath       string                   `json:"raw_result_path,omitempty"`
	RawResultSHA256     string                   `json:"raw_result_sha256,omitempty"`
	RawTrialResults     []EvaluationFileEvidence `json:"raw_trial_results,omitempty"`
	TaskDigest          string                   `json:"task_digest,omitempty"`
	HarborTaskChecksum  string                   `json:"harbor_task_checksum,omitempty"`
	TaskPath            string                   `json:"task_path,omitempty"`
	CommandRunPath      string                   `json:"command_run_path,omitempty"`
	SchemaPreflightPath string                   `json:"schema_preflight_command_run_path,omitempty"`
	PreflightRunPath    string                   `json:"preflight_command_run_path,omitempty"`
	PreflightResultPath string                   `json:"preflight_result_path,omitempty"`
	AgentCacheManifest  string                   `json:"agent_cache_manifest_path,omitempty"`
	RetryEvidence       string                   `json:"retry_evidence_manifest_path,omitempty"`
	Screenshot          string                   `json:"screenshot,omitempty"`
	CreatedAt           time.Time                `json:"created_at,omitempty"`
}

type CommandEvidence struct {
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

type EvaluationRuntime interface {
	Evaluate(context.Context, EvaluationRequest) (EvaluationResult, error)
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
	Input             []AgentInputPart
	Model             string
	ReasoningEffort   string
	SandboxMode       string
	SandboxPolicy     string
	NetworkAccess     bool
	WorkspaceRoots    []string
	TimeoutSeconds    int
	MaxOutputBytes    int
	CapabilitySummary string
	LogPath           string
}

type AgentConversationRequest struct {
	ProjectPath       string
	Model             string
	ReasoningEffort   string
	SandboxMode       string
	SandboxPolicy     string
	NetworkAccess     bool
	WorkspaceRoots    []string
	TimeoutSeconds    int
	MaxOutputBytes    int
	CapabilitySummary string
	LogPath           string
}

type AgentInputPart struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	URL    string `json:"url,omitempty"`
	Path   string `json:"path,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type AgentTurnResult struct {
	Text       string     `json:"text"`
	Model      string     `json:"model,omitempty"`
	TokenUsage TokenUsage `json:"token_usage,omitempty"`
	Warnings   []string   `json:"warnings,omitempty"`
}

type AgentConversation interface {
	Turn(context.Context, AgentTurnRequest) (AgentTurnResult, error)
	Close() error
}

type AgentRuntime interface {
	OpenConversation(context.Context, AgentConversationRequest) (AgentConversation, error)
}

func RunAgentTurn(ctx context.Context, runtime AgentRuntime, req AgentTurnRequest) (AgentTurnResult, error) {
	conversation, err := runtime.OpenConversation(ctx, AgentConversationRequest{
		ProjectPath:       req.ProjectPath,
		Model:             req.Model,
		ReasoningEffort:   req.ReasoningEffort,
		SandboxMode:       req.SandboxMode,
		SandboxPolicy:     req.SandboxPolicy,
		NetworkAccess:     req.NetworkAccess,
		WorkspaceRoots:    append([]string(nil), req.WorkspaceRoots...),
		TimeoutSeconds:    req.TimeoutSeconds,
		MaxOutputBytes:    req.MaxOutputBytes,
		CapabilitySummary: req.CapabilitySummary,
		LogPath:           req.LogPath,
	})
	if err != nil {
		return AgentTurnResult{}, err
	}
	result, turnErr := conversation.Turn(ctx, req)
	closeErr := conversation.Close()
	if turnErr != nil {
		return AgentTurnResult{}, turnErr
	}
	if closeErr != nil {
		return AgentTurnResult{}, closeErr
	}
	return result, nil
}
