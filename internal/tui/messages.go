package tui

type toastExpiredMsg struct{ id uint64 }
type taskHubPollMsg struct{}

type taskHubRunHandoffExecutedMsg struct {
	operationID string
	result      TaskHubRunHandoffResult
	err         error
}
