package tui

import "github.com/purplevoid/harbor-factory/internal/harbor/domain"

import (
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

type runnerEventMsg domain.RunnerEvent
type runnerDoneMsg struct {
	summary domain.RunSummary
	err     error
}
type workspaceRefreshMsg struct {
	summary domain.RunSummary
	events  []domain.RunnerEvent
}
type editorDoneMsg struct {
	path   string
	before fileSnapshot
	after  fileSnapshot
	err    error
}
type gateDecisionWrittenMsg struct {
	path     string
	gate     *domain.GateRequest
	decision domain.GateDecision
	err      error
}
type toastExpiredMsg struct{ id uint64 }
type confirmOpenedMsg struct{}
type hubLoadedMsg struct {
	items []store.RunWithTask
	err   error
}
type hubPollMsg struct{}
type hubSearchMsg struct{ query string }
type workspaceDeletedMsg struct {
	path string
	err  error
}
type clonePreparedMsg struct {
	opts     app.RunnerOptions
	manifest app.CloneWorkspaceManifest
	err      error
}
