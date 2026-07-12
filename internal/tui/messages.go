package tui

import "github.com/purplevoid/harbor-factory/internal/harbor/domain"

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
