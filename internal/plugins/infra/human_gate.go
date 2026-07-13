package infra

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/plugins/pluginutil"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

const (
	HumanGatePluginID = "harborfactory.human_gate"
	HumanGateKind     = "harborfactory.human_gate"
)

type GateBroker interface {
	Decide(context.Context, domain.GateRequest) (domain.GateDecision, error)
}

type HumanGatePlugin struct {
	Broker GateBroker
	Now    func() time.Time
}

type GateRejectedError struct {
	Decision domain.GateDecision
}

func (e GateRejectedError) Error() string {
	action := strings.TrimSpace(e.Decision.Action)
	if action == "" {
		action = "reject"
	}
	return "human gate decision: " + action
}

func (GateRejectedError) FailureKind() workflow.FailureKind { return workflow.FailurePermanent }
func (GateRejectedError) Retryable() bool                   { return false }

func (HumanGatePlugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: HumanGatePluginID, Version: "1.0.0", Kinds: []string{HumanGateKind}}
}

func (HumanGatePlugin) Validate(spec workflow.NodeSpec) error {
	if err := pluginutil.RequiredString(spec, "phase"); err != nil {
		return err
	}
	return pluginutil.RequiredString(spec, "gate_id")
}

func (p HumanGatePlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Store == nil {
		return workflow.NodeResult{}, fmt.Errorf("human_gate artifact store is required")
	}
	phase := pluginutil.String(req, "phase")
	gateID := pluginutil.String(req, "gate_id")
	requestID := fmt.Sprintf("%s:r%d:%s", req.RunID, req.Revision, gateID)
	artifactName := pluginutil.ArtifactName(req, phase+"/artifacts/reviews/"+gateID+"/decision.json")

	decision, reused, err := readGateDecision(ctx, req.Store, artifactName, requestID, gateID)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	if !reused {
		if p.Broker == nil {
			return workflow.NodeResult{}, fmt.Errorf("human_gate broker is required when no reusable decision exists")
		}
		now := time.Now
		if p.Now != nil {
			now = p.Now
		}
		checklist, artifacts, evidenceErr := buildGateEvidence(ctx, req)
		if evidenceErr != nil {
			return workflow.NodeResult{}, fmt.Errorf("build %s gate evidence: %w", gateID, evidenceErr)
		}
		request := domain.GateRequest{
			RequestID: requestID,
			GateID:    gateID,
			GateName:  defaultString(pluginutil.String(req, "gate_name"), gateID),
			NodeID:    req.Spec.ID,
			Message:   pluginutil.String(req, "message"),
			Checklist: checklist,
			Artifacts: artifacts,
			CreatedAt: now().UTC(),
		}
		if req.Events != nil {
			_ = req.Events.Emit(ctx, workflow.Event{RunID: req.RunID, NodeID: req.Spec.ID, Type: "gate_requested", Status: workflow.NodeRunning, Attempt: req.Attempt, Gate: &request})
		}
		decision, err = p.Broker.Decide(ctx, request)
		if err != nil {
			return workflow.NodeResult{}, fmt.Errorf("human gate broker: %w", err)
		}
		decision.RequestID = requestID
		decision.GateID = gateID
		if decision.DecidedAt.IsZero() {
			decision.DecidedAt = now().UTC()
		}
		if strings.TrimSpace(decision.Action) == "" {
			if decision.Approved {
				decision.Action = "approve"
			} else {
				decision.Action = "reject"
			}
		}
	}

	ref, err := req.Store.PutJSON(ctx, artifactName, "gate_decision", req.Spec.ID, decision)
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("store gate decision: %w", err)
	}
	historyName := fmt.Sprintf("%s/artifacts/reviews/%s/revisions/revision-%03d.json", phase, gateID, req.Revision)
	if _, err := req.Store.PutJSON(ctx, historyName, "gate_decision_history", "review_history", decision); err != nil {
		return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}, fmt.Errorf("store gate decision history: %w", err)
	}
	action := strings.ToLower(strings.TrimSpace(decision.Action))
	if !decision.Approved || (action != "" && action != "approve") {
		return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}, GateRejectedError{Decision: decision}
	}
	return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}, nil
}

func readGateDecision(ctx context.Context, store workflow.ArtifactStore, name, requestID, gateID string) (domain.GateDecision, bool, error) {
	var decision domain.GateDecision
	_, err := store.ReadJSON(ctx, name, &decision)
	if err != nil {
		if os.IsNotExist(err) {
			return domain.GateDecision{}, false, nil
		}
		return domain.GateDecision{}, false, fmt.Errorf("read gate decision: %w", err)
	}
	if decision.RequestID != requestID || (decision.GateID != "" && decision.GateID != gateID) {
		return domain.GateDecision{}, false, nil
	}
	decision.GateID = gateID
	return decision, true, nil
}

func configuredGateChecklist(req workflow.NodeRequest) []domain.ChecklistItem {
	if value, ok := req.Input["checklist"].([]domain.ChecklistItem); ok {
		return append([]domain.ChecklistItem(nil), value...)
	}
	return decodeConfigSlice[domain.ChecklistItem](req.Spec.Config["checklist"])
}

func configuredGateArtifacts(req workflow.NodeRequest) []domain.ArtifactPreview {
	if value, ok := req.Input["artifacts"].([]domain.ArtifactPreview); ok {
		return append([]domain.ArtifactPreview(nil), value...)
	}
	return decodeConfigSlice[domain.ArtifactPreview](req.Spec.Config["artifacts"])
}

func decodeConfigSlice[T any](value any) []T {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var result []T
	if json.Unmarshal(raw, &result) != nil {
		return nil
	}
	return result
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
