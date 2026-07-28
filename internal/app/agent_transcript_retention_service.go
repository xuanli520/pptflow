package app

import (
	"context"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

// AgentTranscriptRetentionService is the controlled application boundary for
// expiration of raw Standard Agent turn material. The Store retains immutable
// diagnostic and audit facts after this service removes the expired bodies.
type AgentTranscriptRetentionService struct{ core *lifecycleServiceCore }

type SweepExpiredAgentTranscriptsRequest struct {
	Limit  int
	Actor  string
	Reason string
}

// SweepExpired removes only retention-eligible raw transcript fields. It is
// intentionally a Store-owned operation so active attempts, active workers,
// and legal holds remain fail-closed even when this service is called by a
// periodic worker.
func (service *AgentTranscriptRetentionService) SweepExpired(ctx context.Context, request SweepExpiredAgentTranscriptsRequest) (store.SweepExpiredAgentTurnTranscriptsResult, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return store.SweepExpiredAgentTurnTranscriptsResult{}, fmt.Errorf("agent transcript retention service is not configured")
	}
	if request.Limit < 0 {
		return store.SweepExpiredAgentTurnTranscriptsResult{}, fmt.Errorf("agent transcript retention limit cannot be negative")
	}
	return service.core.store.SweepExpiredAgentTurnTranscripts(ctx, store.SweepExpiredAgentTurnTranscriptsRequest{
		Limit: request.Limit, Actor: request.Actor, Reason: request.Reason,
	})
}
