package tui

import (
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

// reduceNodeEvents builds the TUI read model from the canonical, intentionally
// sparse event stream. Events are patches to a node's presentation state, not
// complete snapshots.
func reduceNodeEvents(events []domain.RunnerEvent) map[string]domain.RunnerEvent {
	nodes := make(map[string]domain.RunnerEvent)
	for _, event := range events {
		if strings.TrimSpace(event.NodeID) == "" {
			continue
		}
		nodes[event.NodeID] = mergeNodeEvent(nodes[event.NodeID], event)
	}
	return nodes
}

func mergeNodeEvent(previous, event domain.RunnerEvent) domain.RunnerEvent {
	if event.RunID == "" {
		event.RunID = previous.RunID
	}
	if event.Status == "" {
		event.Status = previous.Status
	}
	if event.Type == "node_attempt_failed" || event.Type == "node_retry_scheduled" {
		event.Status = "running"
	}
	if strings.TrimSpace(event.Message) == "" {
		event.Message = nodeEventFallbackMessage(event)
		if event.Message == "" {
			event.Message = previous.Message
		}
	}
	if event.Path == "" {
		event.Path = previous.Path
	}
	if event.Attempt == 0 {
		event.Attempt = previous.Attempt
	}
	if len(event.Artifacts) == 0 {
		event.Artifacts = previous.Artifacts
	}
	if len(event.Logs) == 0 {
		event.Logs = previous.Logs
	}
	return event
}

func nodeEventFallbackMessage(event domain.RunnerEvent) string {
	switch event.Type {
	case "node_attempt_started":
		if event.Attempt > 0 {
			return fmt.Sprintf("正在执行第 %d 次尝试", event.Attempt)
		}
		return "节点尝试已开始"
	case "node_retry_scheduled":
		if event.Attempt > 0 {
			return fmt.Sprintf("已安排第 %d 次尝试", event.Attempt)
		}
		return "节点重试已安排"
	case "node_succeeded", "node_failed", "node_canceled", "node_skipped", "node_requeued", "node_reused", "node_preserved":
		return localizeEventType(event.Type)
	case "gate_requested":
		return localizeEventType(event.Type)
	default:
		return ""
	}
}
