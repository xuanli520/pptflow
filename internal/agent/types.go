// Package agent defines the narrow, replaceable conversation port used by
// Harbor-owned stage executors and change providers.
//
// It deliberately does not depend on workflowkit or a concrete Agent SDK:
// runtime adapters own process, provider, and sandbox implementation details.
package agent

import "context"

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
}

// TurnResult is the durable caller-facing result of one Agent turn.
type TurnResult struct {
	Text       string     `json:"text"`
	Model      string     `json:"model,omitempty"`
	TokenUsage TokenUsage `json:"token_usage,omitempty"`
	Warnings   []string   `json:"warnings,omitempty"`
}

// Conversation owns one ephemeral provider session. Callers must close it
// after their final Turn.
type Conversation interface {
	Turn(context.Context, TurnRequest) (TurnResult, error)
	Close() error
}

// Runtime opens isolated Agent conversations. Implementations must not share
// mutable session state between returned Conversation values.
type Runtime interface {
	OpenConversation(context.Context, ConversationRequest) (Conversation, error)
}
