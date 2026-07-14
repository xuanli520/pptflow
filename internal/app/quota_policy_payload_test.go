package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestDurableExecutionPayloadsFreezeCompleteQuotaPolicy(t *testing.T) {
	ctx := context.Background()
	fixture := newContinuationFixture(t, store.WorkflowRunFailedRecoverable)
	frozen, err := decodeFrozenRunDefinition(fixture.run)
	if err != nil {
		t.Fatalf("decode frozen run definition: %v", err)
	}

	initialJob, err := fixture.dataStore.GetDurableJobByIdempotency(ctx, "workflow-run-execution:"+fixture.run.ID)
	if err != nil || initialJob == nil {
		t.Fatalf("initial durable job = %+v, %v", initialJob, err)
	}
	var initialPayload workflowRunExecutionPayload
	if err := json.Unmarshal([]byte(initialJob.PayloadJSON), &initialPayload); err != nil {
		t.Fatalf("decode initial durable payload: %v", err)
	}
	if initialPayload.Format != workflowRunExecutionPayloadFormat || initialPayload.RunID != fixture.run.ID || initialPayload.DefinitionHash != fixture.run.DefinitionHash {
		t.Fatalf("initial durable payload binding = %+v", initialPayload)
	}
	if initialPayload.ExecutionSpecFingerprint != frozen.ExecutionSpecFingerprint {
		t.Fatalf("initial durable payload execution specification fingerprint = %s, want %s", initialPayload.ExecutionSpecFingerprint, frozen.ExecutionSpecFingerprint)
	}
	assertFrozenQuotaPayload(t, initialPayload.QuotaPolicy, frozen)

	plan, err := fixture.services.Continuations.PlanTaskContinuation(ctx,
		continuationCommand(t, ctx, fixture, "quota-payload-continuation", []workflowkit.NodeID{workflowadapter.QualityCheck}, false))
	if err != nil {
		t.Fatalf("plan continuation: %v", err)
	}
	execution, err := fixture.services.Continuations.ExecuteTaskContinuation(ctx, plan.ID())
	if err != nil {
		t.Fatalf("execute continuation: %v", err)
	}
	continuationJob, err := fixture.dataStore.GetDurableJobByIdempotency(ctx, continuationExecutionKey(plan.ID())+":job")
	if err != nil || continuationJob == nil || continuationJob.EntityID != execution.ID {
		t.Fatalf("continuation durable job = %+v, %v", continuationJob, err)
	}
	var continuationPayload continuationExecutionPayload
	if err := json.Unmarshal([]byte(continuationJob.PayloadJSON), &continuationPayload); err != nil {
		t.Fatalf("decode continuation durable payload: %v", err)
	}
	if continuationPayload.Format != continuationExecutionFormat || continuationPayload.RunID != fixture.run.ID || continuationPayload.SourceRunID != fixture.run.ID || continuationPayload.PlanID != plan.ID() {
		t.Fatalf("continuation durable payload binding = %+v", continuationPayload)
	}
	assertFrozenQuotaPayload(t, continuationPayload.QuotaPolicy, frozen)
}

func assertFrozenQuotaPayload(t *testing.T, payloadPolicy workflowadapter.ResolvedQuotaPolicy, frozen frozenRunDefinition) {
	t.Helper()
	if payloadPolicy.ID != frozen.QuotaPolicy.ID || payloadPolicy.Version != frozen.QuotaPolicy.Version || payloadPolicy.Fingerprint != frozen.QuotaPolicy.Fingerprint {
		t.Fatalf("payload quota policy = %+v, want frozen %+v", payloadPolicy, frozen.QuotaPolicy)
	}
	if len(payloadPolicy.AccountLimits) != 4 || len(payloadPolicy.Stages) != len(frozen.Workflow.Stages) {
		t.Fatalf("payload quota snapshot is incomplete: accounts=%d stages=%d", len(payloadPolicy.AccountLimits), len(payloadPolicy.Stages))
	}
	if err := payloadPolicy.ValidateForDescriptor(frozen.Workflow); err != nil {
		t.Fatalf("payload quota policy does not match frozen descriptor: %v", err)
	}
}
