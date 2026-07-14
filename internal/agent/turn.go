package agent

import "context"

// RunTurn opens an ephemeral conversation, performs one turn, and closes the
// conversation on every normal error path. A turn error takes precedence over
// a close error so callers retain the primary provider failure.
func RunTurn(ctx context.Context, runtime Runtime, request TurnRequest) (TurnResult, error) {
	conversation, err := runtime.OpenConversation(ctx, ConversationRequest{
		ProjectPath:       request.ProjectPath,
		Model:             request.Model,
		ReasoningEffort:   request.ReasoningEffort,
		SandboxMode:       request.SandboxMode,
		SandboxPolicy:     request.SandboxPolicy,
		NetworkAccess:     request.NetworkAccess,
		WorkspaceRoots:    append([]string(nil), request.WorkspaceRoots...),
		TimeoutSeconds:    request.TimeoutSeconds,
		MaxOutputBytes:    request.MaxOutputBytes,
		CapabilitySummary: request.CapabilitySummary,
		LogPath:           request.LogPath,
	})
	if err != nil {
		return TurnResult{}, err
	}
	result, turnErr := conversation.Turn(ctx, request)
	closeErr := conversation.Close()
	if turnErr != nil {
		return TurnResult{}, turnErr
	}
	if closeErr != nil {
		return TurnResult{}, closeErr
	}
	return result, nil
}
