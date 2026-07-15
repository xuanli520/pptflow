package stageprovider

import (
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestWorkflowkitRequestExecutionSpecAcceptsAuthoringSessionSubject(t *testing.T) {
	specification := testsupport.CompleteRunExecutionSpec(
		"018f0a73-3b49-7000-8000-000000000021",
		"018f0a73-3b49-7000-8000-000000000022",
		"harbor.task.v2:sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	)
	specification.Template = workflowadapter.StandardAuthoringTemplateReference()
	specification.Selection = workflowadapter.RunSelectionReference{
		Kind:                  workflowadapter.RunSelectionAuthoringSession,
		AuthoringSourceID:     "018f0a73-3b49-7000-8000-000000000031",
		AuthoringSessionID:    "018f0a73-3b49-7000-8000-000000000032",
		AuthoringSourceDigest: workflowkit.SubjectDigest("sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"),
	}
	for index := range specification.References.Checkouts {
		specification.References.Checkouts[index].RevisionID = specification.Selection.AuthoringSessionID
		specification.References.Checkouts[index].RevisionDigest = specification.Selection.AuthoringSourceDigest
	}
	// CompleteRunExecutionSpec is emitted in the closed Standard catalog order;
	// the source-session template is its exact pre-materialization prefix.
	specification.Stages = append([]workflowadapter.StageExecutionBinding(nil), specification.Stages[:len(workflowadapter.StandardAuthoringStageOrder())]...)
	specification.References.Checkouts = specification.References.Checkouts[:1]
	specification.References.Runtimes = specification.References.Runtimes[:1]
	specification.References.Providers = []workflowadapter.ProviderReference{
		specification.References.Providers[0], specification.References.Providers[2],
	}
	specification.References.Secrets = specification.References.Secrets[:1]

	canonical, err := specification.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical authoring execution spec: %v", err)
	}
	binding, err := workflowkit.NewOpaqueExecutionBinding(workflowadapter.RunExecutionSpecFormat, workflowadapter.RunExecutionSpecVersion, canonical)
	if err != nil {
		t.Fatal(err)
	}
	subject, err := specification.Selection.SubjectBinding()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := workflowkitRequestExecutionSpec(workflowkit.StageExecutionRequest{Execution: workflowkit.FrozenExecution{ID: "authoring-execution", Subject: subject, Binding: binding}})
	if err != nil {
		t.Fatalf("authoring subject rejected by generic bridge: %v", err)
	}
	if parsed.Selection != specification.Selection {
		t.Fatalf("selection = %+v, want %+v", parsed.Selection, specification.Selection)
	}

	wrongSubject := subject
	wrongSubject.RevisionID = "018f0a73-3b49-7000-8000-000000000033"
	if _, err := workflowkitRequestExecutionSpec(workflowkit.StageExecutionRequest{Execution: workflowkit.FrozenExecution{ID: "authoring-execution", Subject: wrongSubject, Binding: binding}}); err == nil {
		t.Fatal("accepted authoring execution spec under another session subject")
	}
}
