package stageprovider

import (
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

// TestHarborBuiltinOperationPayloadUsesOnlyItsSealedHandlerIdentity protects
// the post-approval replacement for the original fail-closed gap test. A
// built-in operation is now an explicit typed payload, but still has no
// operation/config/path escape hatch.
func TestHarborBuiltinOperationPayloadUsesOnlyItsSealedHandlerIdentity(t *testing.T) {
	payload, err := workflowadapter.ParseStageOperationPayloadJSON([]byte(`{"kind":"harbor.builtin","handler_id":"standard-authoring.materialize-task"}`))
	if err != nil {
		t.Fatal(err)
	}
	builtin, ok := payload.(workflowadapter.HarborBuiltinOperationPayload)
	if !ok || builtin.HandlerID != "standard-authoring.materialize-task" {
		t.Fatalf("payload = %#v, want typed materialize handler", payload)
	}
	if _, err := workflowadapter.ParseStageOperationPayloadJSON([]byte(`{"kind":"harbor.builtin","handler_id":"standard-authoring.materialize-task","operation_id":"smuggled"}`)); err == nil {
		t.Fatal("harbor.builtin accepted an unknown operation field")
	}
}
