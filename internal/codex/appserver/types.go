package appserver

import (
	"context"
	"strings"
	"time"

	"github.com/xuanli520/pptflow/internal/executor"
)

type Session interface {
	Start(ctx context.Context, request Request) error
	SendGuidance(ctx context.Context, message string) error
	Wait(ctx context.Context) (Result, error)
}

type Request struct {
	Timeout           time.Duration
	ProjectPath       string
	LogPath           string
	Env               []string
	Prompt            string
	CommandPath       string
	CapabilitySummary string
	HasAppServer      bool
	Model             string
	SandboxMode       string
	SandboxPolicy     string
	NetworkAccess     bool
	MaxOutputBytes    int
	OnDelta           func(update Update)
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
