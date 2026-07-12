package runmodel_test

import (
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

func TestWorkflowAndDomainExposeTheSameCanonicalEventTypes(t *testing.T) {
	domainEvent := domain.RunnerEvent{RunID: "run-1", Type: "node_started"}
	var workflowEvent workflow.Event = domainEvent
	if workflowEvent.RunID != domainEvent.RunID {
		t.Fatalf("event aliases diverged: workflow=%+v domain=%+v", workflowEvent, domainEvent)
	}
	domainArtifact := domain.ArtifactPreview{Name: "report.json", Content: "preview"}
	var workflowArtifact workflow.ArtifactRef = domainArtifact
	if workflowArtifact.Content != domainArtifact.Content {
		t.Fatalf("artifact aliases diverged: workflow=%+v domain=%+v", workflowArtifact, domainArtifact)
	}
}
