package tui

type toastExpiredMsg struct{ id uint64 }

// toastScrollMsg advances one visual viewport of an overlong transient
// message. It carries the toast identity so an older timer cannot scroll a
// newer notification.
type toastScrollMsg struct{ id uint64 }
type taskHubPollMsg struct{}

type taskHubRunHandoffExecutedMsg struct {
	operationID string
	result      TaskHubRunHandoffResult
	err         error
}
