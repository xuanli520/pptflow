package stageprovider

import (
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestWorkflowkitRequestExecutionSpecAcceptsAuthoringSessionSubject(t *testing.T) {
	execution := standardAuthoringCodexTestSealedExecution(t, workflowadapter.StandardAuthoringCurrentTemplateReference(), "018f0a73-3b49-7000-8000-000000000021")
	parsed, err := workflowkitRequestExecutionSpec(workflowkit.StageExecutionRequest{Execution: execution})
	if err != nil {
		t.Fatalf("authoring subject rejected by generic bridge: %v", err)
	}
	if parsed.Selection.Kind != workflowadapter.RunSelectionAuthoringSession || parsed.Template != workflowadapter.StandardAuthoringCurrentTemplateReference() {
		t.Fatalf("parsed v2 authoring execution spec = %+v", parsed)
	}

	wrongSubject := execution.Subject
	wrongSubject.RevisionID = "018f0a73-3b49-7000-8000-000000000033"
	execution.Subject = wrongSubject
	if _, err := workflowkitRequestExecutionSpec(workflowkit.StageExecutionRequest{Execution: execution}); err == nil {
		t.Fatal("accepted authoring execution spec under another session subject")
	}
}
