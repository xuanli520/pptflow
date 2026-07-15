package stageprovider

import (
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

// TestStandardAuthoringBuiltinOperationFailsClosedUntilItsPublicContractIsApproved
// protects the important negative invariant behind the pending Standard
// deployment README.  A Go-controlled materialization or lifecycle operation
// must not be smuggled through the existing local.command, agent.turn, or
// durable.review forms with a misleading identity.  Until an approved sealed
// payload/lock revision exists, an unknown built-in discriminator must be
// rejected by the strict public parser.
func TestStandardAuthoringBuiltinOperationFailsClosedUntilItsPublicContractIsApproved(t *testing.T) {
	_, err := workflowadapter.ParseStageOperationPayloadJSON([]byte(`{"kind":"harbor.builtin","operation_id":"standard-authoring.materialize-task"}`))
	if err == nil {
		t.Fatal("unapproved built-in Standard operation was accepted")
	}
	if !strings.Contains(err.Error(), "unsupported stage operation payload kind") {
		t.Fatalf("unapproved built-in payload error = %v, want strict unsupported-kind rejection", err)
	}
}
