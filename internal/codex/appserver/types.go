package appserver

import (
	"context"
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

type Request struct {
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
}

type TurnRequest struct {
	Timeout        time.Duration
	Prompt         string
	Input          []InputPart
	LogPath        string
	MaxOutputBytes int
	OnDelta        func(update Update)
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
	return &appServerCodexReviewSession{envKeys: append([]string{}, envKeys...)}
}

func (w Warning) OK() bool {
	return strings.TrimSpace(w.Error) == ""
}
