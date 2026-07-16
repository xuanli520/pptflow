// Package agent defines the narrow, replaceable conversation port used by
// application-owned stage executors and change providers.
//
// It deliberately depends only on the standard library rather than a concrete
// Agent SDK; runtime adapters own process, provider, and sandbox details.
package agent

import (
	"context"
	"encoding/json"
)

// TokenUsage records provider-reported token consumption for one turn.
type TokenUsage struct {
	Input  int `json:"input,omitempty"`
	Output int `json:"output,omitempty"`
	Total  int `json:"total,omitempty"`
}

// InputPart is one optional structured input supplied with an Agent turn.
type InputPart struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	URL    string `json:"url,omitempty"`
	Path   string `json:"path,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// DynamicToolHandler handles one model-requested function invocation. Both
// arguments and the successful result are JSON values so this port stays
// independent of a particular provider protocol or product domain.
type DynamicToolHandler func(context.Context, json.RawMessage) (json.RawMessage, error)

// DynamicTool describes one function available only to a single conversation.
// InputSchema is a JSON Schema value. Implementations must not retain mutable
// caller-owned schema bytes beyond opening the conversation.
type DynamicTool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Handler     DynamicToolHandler
}

// ConversationRequest fixes the settings shared by every turn in one
// ephemeral Agent conversation.
type ConversationRequest struct {
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
	DynamicTools      []DynamicTool
}

// TurnRequest supplies one conversation turn. A runtime may use unset
// per-turn settings from the ConversationRequest that opened the session.
type TurnRequest struct {
	ProjectPath       string
	Prompt            string
	Input             []InputPart
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
	// OutputSchema constrains the final assistant message for this turn. It is
	// a JSON Schema value and is intentionally separate from DynamicTools,
	// which are fixed when the conversation starts.
	OutputSchema json.RawMessage
}

// TurnResult is the durable caller-facing result of one Agent turn.
type TurnResult struct {
	Text       string     `json:"text"`
	Model      string     `json:"model,omitempty"`
	TokenUsage TokenUsage `json:"token_usage,omitempty"`
	Warnings   []string   `json:"warnings,omitempty"`
}

// TurnUpdate is one best-effort live update produced while an Agent turn is
// running. It is intentionally provider-neutral: callers can present partial
// text and item completion without depending on a provider event protocol.
// The final TurnResult remains the durable result of the turn.
type TurnUpdate struct {
	TurnID    string `json:"turn_id,omitempty"`
	ItemID    string `json:"item_id,omitempty"`
	Delta     string `json:"delta,omitempty"`
	Text      string `json:"text,omitempty"`
	Done      bool   `json:"done,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// TurnUpdateHandler receives best-effort live turn updates. Implementations
// invoke it synchronously with their provider event stream; callers should
// keep it bounded and safe for repeated or partial updates.
type TurnUpdateHandler func(TurnUpdate)

// Conversation owns one ephemeral provider session. Callers must close it
// after their final Turn.
type Conversation interface {
	Turn(context.Context, TurnRequest) (TurnResult, error)
	Close() error
}

// StreamingConversation is an optional capability for conversations that can
// deliver live updates while a turn runs. It extends, rather than changes, the
// base Conversation contract so runtimes without streaming remain compatible.
type StreamingConversation interface {
	Conversation
	TurnStream(context.Context, TurnRequest, TurnUpdateHandler) (TurnResult, error)
}

// SteerableConversation is an optional capability for a caller to send live
// guidance to an active turn. The guidance is ephemeral control input; the
// base Conversation contract remains usable by runtimes that do not support
// live steering.
type SteerableConversation interface {
	Conversation
	Steer(context.Context, string) error
}

// Runtime opens isolated Agent conversations. Implementations must not share
// mutable session state between returned Conversation values.
type Runtime interface {
	OpenConversation(context.Context, ConversationRequest) (Conversation, error)
}
