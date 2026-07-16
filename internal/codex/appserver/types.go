package appserver

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/executor"
)

type Session interface {
	Start(ctx context.Context, request Request) error
	Turn(ctx context.Context, request TurnRequest) (Result, error)
	SendGuidance(ctx context.Context, message string) error
	Close() error
}

// DynamicToolHandler handles a function call from the App Server. The input
// and successful output are provider-neutral JSON values. Session code turns a
// successful value into the App Server's textual dynamic-tool result format.
type DynamicToolHandler func(context.Context, json.RawMessage) (json.RawMessage, error)

// DynamicTool is a function registered only for one App Server thread.
// InputSchema is sent as the App Server's inputSchema JSON Schema field.
type DynamicTool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Handler     DynamicToolHandler
}

type Request struct {
	ClientName        string
	ClientVersion     string
	Timeout           time.Duration
	ProjectPath       string
	LogPath           string
	Env               []string
	Prompt            string
	Input             []InputPart
	CommandPath       string
	CapabilitySummary string
	HasAppServer      bool
	Model             string
	ReasoningEffort   string
	SandboxMode       string
	SandboxPolicy     string
	NetworkAccess     bool
	WorkspaceRoots    []string
	MaxOutputBytes    int
	OnDelta           func(update Update)
	DynamicTools      []DynamicTool
}

type TurnRequest struct {
	Timeout        time.Duration
	Prompt         string
	Input          []InputPart
	LogPath        string
	MaxOutputBytes int
	OnDelta        func(update Update)
	// OutputSchema is the JSON Schema that constrains this turn's final
	// assistant message. It is not retained for later turns.
	OutputSchema json.RawMessage
}

type InputPart struct {
	Type   string
	Text   string
	URL    string
	Path   string
	Detail string
}

type Result struct {
	Result   executor.Result
	Warnings []Warning
}

type Update struct {
	TurnID    string
	ItemID    string
	Delta     string
	Text      string
	Done      bool
	Truncated bool
}

type Warning struct {
	Path       string `json:"path"`
	Op         string `json:"op"`
	Error      string `json:"error"`
	Required   bool   `json:"required,omitempty"`
	RecordedAt string `json:"recorded_at,omitempty"`
}

func New(envKeys []string) Session {
	return &appServerSession{envKeys: append([]string{}, envKeys...)}
}

func (w Warning) OK() bool {
	return strings.TrimSpace(w.Error) == ""
}
