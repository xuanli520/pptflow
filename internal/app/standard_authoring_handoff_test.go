package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

type codeEdgePhase1DefinitionProviderFunc func(context.Context, CodeEdgePhase1RunDefinitionRequest) (CodeEdgePhase1RunDefinition, error)

func (function codeEdgePhase1DefinitionProviderFunc) DefinitionForCodeEdgePhase1Run(ctx context.Context, request CodeEdgePhase1RunDefinitionRequest) (CodeEdgePhase1RunDefinition, error) {
	return function(ctx, request)
}

func TestStandardAuthoringHandoffCreatesOneTaskBoundPhase1ChildFromPersistedReceipt(t *testing.T) {
	ctx := context.Background()
	fixture := newStandardAuthoringHandoffFixture(t)
	provider := codeEdgePhase1DefinitionProviderFunc(func(_ context.Context, request CodeEdgePhase1RunDefinitionRequest) (CodeEdgePhase1RunDefinition, error) {
		if request.TaskID != fixture.handoff.TaskID || request.RevisionID != fixture.handoff.RevisionID || request.RevisionDigest != fixture.handoff.RevisionDigest ||
			request.AuthoringRunID != fixture.run.ID || request.AuthoringSourceID != fixture.source.ID || request.AuthoringSessionID != fixture.session.ID ||
			request.TaskSnapshot != fixture.handoff.TaskSnapshot {
			t.Fatalf("definition request = %+v; does not match persisted handoff", request)
		}
		return CodeEdgePhase1RunDefinition{
			Profile:       lifecycleCompleteProfileForTemplate(t, workflowadapter.CodeEdgePhase1WorkflowTemplate()),
			ExecutionSpec: testsupport.CompleteCodeEdgePhase1RunExecutionSpec(request.TaskID, request.RevisionID, string(request.RevisionDigest)),
		}, nil
	})
	services, err := NewLifecycleServicesWithOptions(fixture.root, fixture.database, LifecycleServicesOptions{
		OperationResolver:                   testsupport.AcceptAllStageOperationResolver(),
		CodeEdgePhase1RunDefinitionProvider: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	childID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	request := StandardAuthoringHandoffRequest{
		AuthoringRunID: fixture.run.ID, StageAttemptID: fixture.stageAttempt.ID,
		HandoffArtifactID: fixture.handoffArtifactID, ChildRunID: childID,
		Actor: "worker", Reason: "consume durable authoring handoff",
	}
	child, err := services.StandardAuthoringHandoffs.Consume(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if child.ID != childID || child.ParentRunID != fixture.run.ID || child.TaskID != fixture.handoff.TaskID || child.RevisionID != fixture.handoff.RevisionID ||
		child.WorkflowTemplateID != workflowadapter.CodeEdgePhase1WorkflowTemplateID || child.WorkflowTemplateVersion != workflowadapter.CodeEdgePhase1WorkflowTemplateVersion {
		t.Fatalf("child Run = %+v", child)
	}
	managedSnapshot, err := fixture.database.GetRunInputArtifactForPort(ctx, child.ID, managedTaskSnapshotInputPort)
	if err != nil {
		t.Fatal(err)
	}
	if managedSnapshot == nil || managedSnapshot.ContentDigest != string(fixture.handoff.TaskSnapshot.ContentDigest) || managedSnapshot.SchemaVersion != fixture.handoff.TaskSnapshot.SchemaVersion || managedSnapshot.ID == fixture.handoffArtifactID {
		t.Fatalf("child managed snapshot = %+v; want fresh input identity over handoff bytes", managedSnapshot)
	}
	replayed, err := services.StandardAuthoringHandoffs.Consume(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != child.ID || replayed.ParentRunID != fixture.run.ID {
		t.Fatalf("replayed child Run = %+v; want %+v", replayed, child)
	}
	// Once the child row committed, replay must return it before consulting a
	// potentially changed/missing deployment definition. The durable record,
	// rather than the mutable provider process, remains the recovery authority.
	withoutDefinition, err := NewLifecycleServicesWithOptions(fixture.root, fixture.database, LifecycleServicesOptions{OperationResolver: testsupport.AcceptAllStageOperationResolver()})
	if err != nil {
		t.Fatal(err)
	}
	differentChildID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := withoutDefinition.StandardAuthoringHandoffs.Consume(ctx, StandardAuthoringHandoffRequest{
		AuthoringRunID: fixture.run.ID, StageAttemptID: fixture.stageAttempt.ID, HandoffArtifactID: fixture.handoffArtifactID,
		ChildRunID: differentChildID, Actor: "worker", Reason: "recover existing child without provider",
	})
	if err != nil || recovered.ID != child.ID {
		t.Fatalf("provider-free existing-child recovery = %+v, %v; want %s", recovered, err, child.ID)
	}
	// Generic StartRun never receives the private durable handoff capability.
	// Even a matching template/parent/task request must be rejected rather than
	// creating a second Phase-1 child around the authoring artifact boundary.
	unauthorizedID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	_, err = services.Runs.StartRun(ctx, StartRunRequest{
		ID: unauthorizedID, TaskID: fixture.handoff.TaskID, RevisionID: fixture.handoff.RevisionID,
		Profile:       lifecycleCompleteProfileForTemplate(t, workflowadapter.CodeEdgePhase1WorkflowTemplate()),
		ExecutionSpec: testsupport.CompleteCodeEdgePhase1RunExecutionSpec(fixture.handoff.TaskID, fixture.handoff.RevisionID, string(fixture.handoff.RevisionDigest)),
		ParentRunID:   fixture.run.ID, Trigger: standardAuthoringHandoffRunTrigger, Actor: "worker", Reason: "attempt bypass",
	})
	if !strings.Contains(errString(err), "authoring parent requires a persisted Phase-1 handoff") {
		t.Fatalf("generic authoring-parent StartRun error = %v", err)
	}
	runs, err := fixture.database.ListWorkflowRunsForTask(ctx, fixture.handoff.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	children := 0
	for _, candidate := range runs {
		if candidate.ParentRunID == fixture.run.ID && candidate.WorkflowTemplateID == workflowadapter.CodeEdgePhase1WorkflowTemplateID {
			children++
		}
	}
	if children != 1 {
		t.Fatalf("Phase-1 children = %d; want exactly one", children)
	}
}

func TestStandardAuthoringHandoffFailsClosedWithoutPhase1Definition(t *testing.T) {
	ctx := context.Background()
	fixture := newStandardAuthoringHandoffFixture(t)
	services, err := NewLifecycleServicesWithOptions(fixture.root, fixture.database, LifecycleServicesOptions{OperationResolver: testsupport.AcceptAllStageOperationResolver()})
	if err != nil {
		t.Fatal(err)
	}
	childID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	_, err = services.StandardAuthoringHandoffs.Consume(ctx, StandardAuthoringHandoffRequest{
		AuthoringRunID: fixture.run.ID, StageAttemptID: fixture.stageAttempt.ID,
		HandoffArtifactID: fixture.handoffArtifactID, ChildRunID: childID, Actor: "worker", Reason: "verify fail closed",
	})
	if !strings.Contains(errString(err), ErrCodeEdgePhase1DefinitionUnavailable.Error()) {
		t.Fatalf("Consume without definition error = %v; want definition unavailable", err)
	}
	if child, lookupErr := fixture.database.GetWorkflowRun(ctx, childID); lookupErr != nil || child != nil {
		t.Fatalf("fail-closed handoff created child = %+v, %v", child, lookupErr)
	}
	if ref, lookupErr := fixture.database.GetArtifactRef(ctx, fixture.handoffArtifactID); lookupErr != nil || ref == nil {
		t.Fatalf("fail-closed handoff lost durable artifact = %+v, %v", ref, lookupErr)
	}
	prepared, err := fixture.database.GetAuthoringPhase1HandoffForAuthoringRun(ctx, fixture.run.ID)
	if err != nil || prepared == nil || prepared.ChildRunID != childID {
		t.Fatalf("fail-closed handoff did not preserve its redrive identity = %+v, %v", prepared, err)
	}
	// A later approved provider consumes that exact durable record even when a
	// retry supplies a fresh UUID. This proves the first request cannot be used
	// to manufacture a second child after a configuration change.
	provider := codeEdgePhase1DefinitionProviderFunc(func(_ context.Context, definition CodeEdgePhase1RunDefinitionRequest) (CodeEdgePhase1RunDefinition, error) {
		return CodeEdgePhase1RunDefinition{
			Profile:       lifecycleCompleteProfileForTemplate(t, workflowadapter.CodeEdgePhase1WorkflowTemplate()),
			ExecutionSpec: testsupport.CompleteCodeEdgePhase1RunExecutionSpec(definition.TaskID, definition.RevisionID, string(definition.RevisionDigest)),
		}, nil
	})
	redriveServices, err := NewLifecycleServicesWithOptions(fixture.root, fixture.database, LifecycleServicesOptions{OperationResolver: testsupport.AcceptAllStageOperationResolver(), CodeEdgePhase1RunDefinitionProvider: provider})
	if err != nil {
		t.Fatal(err)
	}
	newRequestID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	child, err := redriveServices.StandardAuthoringHandoffs.Consume(ctx, StandardAuthoringHandoffRequest{
		AuthoringRunID: fixture.run.ID, StageAttemptID: fixture.stageAttempt.ID, HandoffArtifactID: fixture.handoffArtifactID,
		ChildRunID: newRequestID, Actor: "worker", Reason: "redrive after provider installation",
	})
	if err != nil || child.ID != prepared.ChildRunID || child.ID != childID {
		t.Fatalf("redriven child = %+v, %v; want original %s", child, err, childID)
	}
}

func TestFrozenRuntimeEnqueuesAndConsumesOneStandardAuthoringHandoffJob(t *testing.T) {
	ctx := context.Background()
	fixture := newStandardAuthoringHandoffFixture(t)
	initial := queueStandardAuthoringInitialCoordinator(t, ctx, fixture)
	provider := codeEdgePhase1DefinitionProviderFunc(func(_ context.Context, definition CodeEdgePhase1RunDefinitionRequest) (CodeEdgePhase1RunDefinition, error) {
		return CodeEdgePhase1RunDefinition{
			Profile:       lifecycleCompleteProfileForTemplate(t, workflowadapter.CodeEdgePhase1WorkflowTemplate()),
			ExecutionSpec: testsupport.CompleteCodeEdgePhase1RunExecutionSpec(definition.TaskID, definition.RevisionID, string(definition.RevisionDigest)),
		}, nil
	})
	services, err := NewLifecycleServicesWithOptions(fixture.root, fixture.database, LifecycleServicesOptions{
		OperationResolver:                   testsupport.AcceptAllStageOperationResolver(),
		CodeEdgePhase1RunDefinitionProvider: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &FrozenExecutionRuntime{core: services.core, services: services}
	stageJob := store.DurableJob{CreatedBy: "worker", Priority: 7}
	if err := runtime.enqueueStandardAuthoringHandoff(ctx, stageJob, fixture.run, fixture.stageAttempt); err != nil {
		t.Fatal(err)
	}
	job, err := fixture.database.GetDurableJobByIdempotency(ctx, "standard-authoring-handoff:"+fixture.stageAttempt.ID)
	if err != nil || job == nil {
		t.Fatalf("durable handoff job = %+v, %v", job, err)
	}
	payload, err := standardAuthoringHandoffJobPayload(*job)
	if err != nil || payload.AuthoringRunID != fixture.run.ID || payload.HandoffArtifactID != fixture.handoffArtifactID {
		t.Fatalf("durable handoff payload = %+v, %v", payload, err)
	}
	state, err := runtime.handleStandardAuthoringHandoff(ctx, *job)
	if err != nil || state != store.JobSucceeded {
		t.Fatalf("consume durable handoff = %s, %v", state, err)
	}
	completion, err := fixture.database.GetDurableJobByIdempotency(ctx, "standard-authoring-completion:"+job.ID)
	if err != nil || completion == nil || completion.CommandType != "workflow_run.execute" || completion.RunID != fixture.run.ID || completion.PayloadJSON != initial.PayloadJSON {
		t.Fatalf("handoff completion coordinator = %+v, %v", completion, err)
	}
	child, err := fixture.database.GetWorkflowRun(ctx, payload.ChildRunID)
	if err != nil || child == nil || child.ParentRunID != fixture.run.ID {
		t.Fatalf("durable handoff child = %+v, %v", child, err)
	}
	if err := runtime.enqueueStandardAuthoringHandoff(ctx, stageJob, fixture.run, fixture.stageAttempt); err != nil {
		t.Fatal(err)
	}
	jobs, err := fixture.database.ListDurableJobsForRun(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, candidate := range jobs {
		if candidate.CommandType == standardAuthoringHandoffCommandType {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("handoff jobs = %d; want exactly one", count)
	}
}

func TestStandardAuthoringHandoffBlocksReverseCoordinatorCompletionUntilChildIsBound(t *testing.T) {
	ctx := context.Background()
	fixture := newStandardAuthoringHandoffFixture(t)
	initial := queueStandardAuthoringInitialCoordinator(t, ctx, fixture)
	services, err := NewLifecycleServicesWithOptions(fixture.root, fixture.database, LifecycleServicesOptions{
		OperationResolver:                   testsupport.AcceptAllStageOperationResolver(),
		CodeEdgePhase1RunDefinitionProvider: standardAuthoringHandoffDefinitionProvider(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &FrozenExecutionRuntime{core: services.core, services: services}
	stageJob := store.DurableJob{CreatedBy: "worker", Priority: 7}
	if err := runtime.enqueueStandardAuthoringHandoff(ctx, stageJob, fixture.run, fixture.stageAttempt); err != nil {
		t.Fatal(err)
	}
	handoffJob, err := fixture.database.GetDurableJobByIdempotency(ctx, "standard-authoring-handoff:"+fixture.stageAttempt.ID)
	if err != nil || handoffJob == nil {
		t.Fatalf("load queued Standard handoff = %+v, %v", handoffJob, err)
	}
	handoffDelivery := *handoffJob

	// This call models the old reverse scheduling order: a coordinator that
	// was already queued reaches its completion branch before the handoff job
	// has created the Phase-1 child. It must complete harmlessly, not terminally.
	profile := lifecycleCompleteProfileForTemplate(t, workflowadapter.StandardAuthoringWorkflowTemplate())
	resolved, err := workflowadapter.StandardAuthoringWorkflowTemplate().Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	frozen := frozenRunDefinition{Workflow: resolved.Descriptor}
	plan := runtimeExecutionPlan{
		Workflow: resolved.Descriptor,
		Transitions: map[workflowkit.StageKey]workflowkit.NodeTransition{
			workflowkit.StageKey(workflowadapter.MaterializeTask): {
				NodeID: workflowkit.StageKey(workflowadapter.MaterializeTask), FromGeneration: 0, ToGeneration: 0,
				Disposition: workflowkit.DispositionSchedule,
			},
		},
	}
	if err := runtime.completeExecutionIfSatisfied(ctx, initial, fixture.run, frozen, plan); err != nil {
		t.Fatalf("reverse coordinator completion = %v", err)
	}
	parent, err := fixture.database.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || parent == nil || parent.Status != store.WorkflowRunRunning {
		t.Fatalf("parent after reverse coordinator = %+v, %v; want running", parent, err)
	}

	handoffDelivery, err = fixture.database.TransitionDurableJob(ctx, store.TransitionDurableJobRequest{
		JobID: handoffDelivery.ID, ExpectedVersion: handoffDelivery.Version, State: store.JobRunning, Actor: "worker", Reason: "consume queued Standard handoff",
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := runtime.handleStandardAuthoringHandoff(ctx, handoffDelivery)
	if err != nil || state != store.JobSucceeded {
		t.Fatalf("consume Standard handoff = %s, %v", state, err)
	}
	handoffDelivery, err = fixture.database.TransitionDurableJob(ctx, store.TransitionDurableJobRequest{
		JobID: handoffDelivery.ID, ExpectedVersion: handoffDelivery.Version, State: state, Actor: "worker", Reason: "record Standard handoff delivery",
	})
	if err != nil {
		t.Fatal(err)
	}
	completion, err := fixture.database.GetDurableJobByIdempotency(ctx, "standard-authoring-completion:"+handoffDelivery.ID)
	if err != nil || completion == nil || completion.State != store.JobQueued {
		t.Fatalf("handoff completion coordinator = %+v, %v", completion, err)
	}
	if err := runtime.completeExecutionIfSatisfied(ctx, *completion, fixture.run, frozen, plan); err != nil {
		t.Fatalf("post-handoff completion = %v", err)
	}
	parent, err = fixture.database.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || parent == nil || parent.Status != store.WorkflowRunSucceeded {
		t.Fatalf("parent after bound child = %+v, %v; want succeeded", parent, err)
	}
}

func TestStandardAuthoringHandoffDefinitionHoldRequiresExplicitRedriveAndReusesChild(t *testing.T) {
	ctx := context.Background()
	fixture := newStandardAuthoringHandoffFixture(t)
	queueStandardAuthoringInitialCoordinator(t, ctx, fixture)
	withoutDefinition, err := NewLifecycleServicesWithOptions(fixture.root, fixture.database, LifecycleServicesOptions{OperationResolver: testsupport.AcceptAllStageOperationResolver()})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &FrozenExecutionRuntime{core: withoutDefinition.core, services: withoutDefinition}
	stageJob := store.DurableJob{CreatedBy: "worker", Priority: 7}
	if err := runtime.enqueueStandardAuthoringHandoff(ctx, stageJob, fixture.run, fixture.stageAttempt); err != nil {
		t.Fatal(err)
	}
	original, err := fixture.database.GetDurableJobByIdempotency(ctx, "standard-authoring-handoff:"+fixture.stageAttempt.ID)
	if err != nil || original == nil {
		t.Fatalf("original handoff job = %+v, %v", original, err)
	}
	originalDelivery := *original
	originalDelivery, err = fixture.database.TransitionDurableJob(ctx, store.TransitionDurableJobRequest{
		JobID: originalDelivery.ID, ExpectedVersion: originalDelivery.Version, State: store.JobRunning, Actor: "worker", Reason: "attempt unavailable Standard handoff",
	})
	if err != nil {
		t.Fatal(err)
	}
	state, holdErr := runtime.handleStandardAuthoringHandoff(ctx, originalDelivery)
	if state != store.JobInDoubt || !errors.Is(holdErr, ErrCodeEdgePhase1DefinitionUnavailable) {
		t.Fatalf("definition hold = state %s err %v; want in_doubt/unavailable", state, holdErr)
	}
	originalDelivery, err = fixture.database.TransitionDurableJob(ctx, store.TransitionDurableJobRequest{
		JobID: originalDelivery.ID, ExpectedVersion: originalDelivery.Version, State: state, Actor: "worker", Reason: "record unavailable Standard handoff hold",
	})
	if err != nil {
		t.Fatal(err)
	}
	if originalDelivery.FinishedAt == nil {
		t.Fatalf("in_doubt handoff must finish its current delivery: %+v", originalDelivery)
	}
	payload, err := standardAuthoringHandoffJobPayload(originalDelivery)
	if err != nil {
		t.Fatal(err)
	}

	withDefinition, err := NewLifecycleServicesWithOptions(fixture.root, fixture.database, LifecycleServicesOptions{
		OperationResolver:                   testsupport.AcceptAllStageOperationResolver(),
		CodeEdgePhase1RunDefinitionProvider: standardAuthoringHandoffDefinitionProvider(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	command := RedriveStandardAuthoringHandoffCommand{AuthoringRunID: fixture.run.ID, IdempotencyKey: key, Actor: "operator", Reason: "install approved Phase-1 definition"}
	redrive, err := withDefinition.StandardAuthoringHandoffs.Redrive(ctx, command)
	if err != nil || redrive.CommandType != standardAuthoringHandoffRedriveCommandType || redrive.State != store.JobQueued || redrive.PayloadJSON != originalDelivery.PayloadJSON {
		t.Fatalf("explicit handoff redrive = %+v, %v", redrive, err)
	}
	replayed, err := withDefinition.StandardAuthoringHandoffs.Redrive(ctx, command)
	if err != nil || replayed.ID != redrive.ID {
		t.Fatalf("same redrive command did not replay its durable job: %+v, %v", replayed, err)
	}
	originalAfter, err := fixture.database.GetDurableJob(ctx, original.ID)
	if err != nil || originalAfter == nil || originalAfter.State != store.JobInDoubt {
		t.Fatalf("original handoff redrive fact = %+v, %v; want in_doubt", originalAfter, err)
	}

	runtime = &FrozenExecutionRuntime{core: withDefinition.core, services: withDefinition}
	redrive, err = fixture.database.TransitionDurableJob(ctx, store.TransitionDurableJobRequest{
		JobID: redrive.ID, ExpectedVersion: redrive.Version, State: store.JobRunning, Actor: "worker", Reason: "consume explicit Standard handoff redrive",
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err = runtime.handleStandardAuthoringHandoff(ctx, redrive)
	if err != nil || state != store.JobSucceeded {
		t.Fatalf("redrive handoff delivery = %s, %v", state, err)
	}
	if _, err := fixture.database.TransitionDurableJob(ctx, store.TransitionDurableJobRequest{
		JobID: redrive.ID, ExpectedVersion: redrive.Version, State: state, Actor: "worker", Reason: "record explicit Standard handoff redrive",
	}); err != nil {
		t.Fatal(err)
	}
	child, err := fixture.database.GetWorkflowRun(ctx, payload.ChildRunID)
	if err != nil || child == nil || child.ID != payload.ChildRunID || child.ParentRunID != fixture.run.ID {
		t.Fatalf("redriven child = %+v, %v; want original child identity %s", child, err, payload.ChildRunID)
	}
	completion, err := fixture.database.GetDurableJobByIdempotency(ctx, "standard-authoring-completion:"+original.ID)
	if err != nil || completion == nil || completion.CommandType != "workflow_run.execute" {
		t.Fatalf("redrive completion coordinator = %+v, %v", completion, err)
	}
	jobs, err := fixture.database.ListDurableJobsForRun(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	completionCount := 0
	for _, job := range jobs {
		if job.IdempotencyKey == "standard-authoring-completion:"+original.ID {
			completionCount++
		}
	}
	if completionCount != 1 {
		t.Fatalf("redrive completion coordinators = %d; want exactly one", completionCount)
	}
}

func TestRecoveredStandardAuthoringHandoffCreatesCompletionAfterChildWasPersisted(t *testing.T) {
	ctx := context.Background()
	fixture := newStandardAuthoringHandoffFixture(t)
	initial := queueStandardAuthoringInitialCoordinator(t, ctx, fixture)
	withDefinition, err := NewLifecycleServicesWithOptions(fixture.root, fixture.database, LifecycleServicesOptions{
		OperationResolver:                   testsupport.AcceptAllStageOperationResolver(),
		CodeEdgePhase1RunDefinitionProvider: standardAuthoringHandoffDefinitionProvider(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &FrozenExecutionRuntime{core: withDefinition.core, services: withDefinition}
	stageJob := store.DurableJob{CreatedBy: "worker", Priority: 7}
	if err := runtime.enqueueStandardAuthoringHandoff(ctx, stageJob, fixture.run, fixture.stageAttempt); err != nil {
		t.Fatal(err)
	}
	original, err := fixture.database.GetDurableJobByIdempotency(ctx, "standard-authoring-handoff:"+fixture.stageAttempt.ID)
	if err != nil || original == nil {
		t.Fatalf("original recoverable handoff job = %+v, %v", original, err)
	}
	payload, err := standardAuthoringHandoffJobPayload(*original)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the crash window after Consume commits the child but before the
	// handler publishes its completion coordinator or records JobSucceeded.
	child, err := withDefinition.StandardAuthoringHandoffs.Consume(ctx, StandardAuthoringHandoffRequest{
		AuthoringRunID: payload.AuthoringRunID, StageAttemptID: payload.StageAttemptID, HandoffArtifactID: payload.HandoffArtifactID,
		ChildRunID: payload.ChildRunID, Actor: "worker", Reason: "persist child before simulated worker loss",
	})
	if err != nil || child.ID != payload.ChildRunID {
		t.Fatalf("persist child before recovery = %+v, %v", child, err)
	}
	delivery, err := fixture.database.TransitionDurableJob(ctx, store.TransitionDurableJobRequest{
		JobID: original.ID, ExpectedVersion: original.Version, State: store.JobRunning, Actor: "worker", Reason: "simulate claimed handoff delivery",
	})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err = fixture.database.TransitionDurableJob(ctx, store.TransitionDurableJobRequest{
		JobID: delivery.ID, ExpectedVersion: delivery.Version, State: store.JobInterrupted, Actor: "recovery", Reason: "simulate expired worker lease",
	})
	if err != nil {
		t.Fatal(err)
	}
	withoutDefinition, err := NewLifecycleServicesWithOptions(fixture.root, fixture.database, LifecycleServicesOptions{OperationResolver: testsupport.AcceptAllStageOperationResolver()})
	if err != nil {
		t.Fatal(err)
	}
	runtime = &FrozenExecutionRuntime{core: withoutDefinition.core, services: withoutDefinition}
	if err := runtime.reconcileRecoveredStandardAuthoringHandoff(ctx, delivery); err != nil {
		t.Fatalf("recover child-persisted Standard handoff without provider = %v", err)
	}
	completion, err := fixture.database.GetDurableJobByIdempotency(ctx, "standard-authoring-completion:"+original.ID)
	if err != nil || completion == nil || completion.State != store.JobQueued {
		t.Fatalf("recovered completion coordinator = %+v, %v", completion, err)
	}
	profile := lifecycleCompleteProfileForTemplate(t, workflowadapter.StandardAuthoringWorkflowTemplate())
	resolved, err := workflowadapter.StandardAuthoringWorkflowTemplate().Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	frozen := frozenRunDefinition{Workflow: resolved.Descriptor}
	plan := runtimeExecutionPlan{
		Workflow: resolved.Descriptor,
		Transitions: map[workflowkit.StageKey]workflowkit.NodeTransition{
			workflowkit.StageKey(workflowadapter.MaterializeTask): {
				NodeID: workflowkit.StageKey(workflowadapter.MaterializeTask), FromGeneration: 0, ToGeneration: 0,
				Disposition: workflowkit.DispositionSchedule,
			},
		},
	}
	if err := runtime.completeExecutionIfSatisfied(ctx, initial, fixture.run, frozen, plan); err != nil {
		t.Fatalf("recovered handoff completion barrier = %v", err)
	}
	parent, err := fixture.database.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || parent == nil || parent.Status != store.WorkflowRunSucceeded {
		t.Fatalf("parent after recovered child handoff = %+v, %v; want succeeded", parent, err)
	}
}

func TestStandardAuthoringHandoffRedriveRejectsUnavailableDefinitionAndNonInDoubtDelivery(t *testing.T) {
	ctx := context.Background()
	t.Run("definition unavailable", func(t *testing.T) {
		fixture := newStandardAuthoringHandoffFixture(t)
		services, err := NewLifecycleServicesWithOptions(fixture.root, fixture.database, LifecycleServicesOptions{OperationResolver: testsupport.AcceptAllStageOperationResolver()})
		if err != nil {
			t.Fatal(err)
		}
		key, err := store.NewUUIDv7()
		if err != nil {
			t.Fatal(err)
		}
		_, err = services.StandardAuthoringHandoffs.Redrive(ctx, RedriveStandardAuthoringHandoffCommand{AuthoringRunID: fixture.run.ID, IdempotencyKey: key, Actor: "operator", Reason: "attempt unavailable redrive"})
		if !errors.Is(err, ErrCodeEdgePhase1DefinitionUnavailable) {
			t.Fatalf("redrive without definition = %v, want unavailable", err)
		}
	})
	t.Run("original delivery is not in doubt", func(t *testing.T) {
		fixture := newStandardAuthoringHandoffFixture(t)
		services, err := NewLifecycleServicesWithOptions(fixture.root, fixture.database, LifecycleServicesOptions{
			OperationResolver:                   testsupport.AcceptAllStageOperationResolver(),
			CodeEdgePhase1RunDefinitionProvider: standardAuthoringHandoffDefinitionProvider(t),
		})
		if err != nil {
			t.Fatal(err)
		}
		runtime := &FrozenExecutionRuntime{core: services.core, services: services}
		if err := runtime.enqueueStandardAuthoringHandoff(ctx, store.DurableJob{CreatedBy: "worker"}, fixture.run, fixture.stageAttempt); err != nil {
			t.Fatal(err)
		}
		key, err := store.NewUUIDv7()
		if err != nil {
			t.Fatal(err)
		}
		_, err = services.StandardAuthoringHandoffs.Redrive(ctx, RedriveStandardAuthoringHandoffCommand{AuthoringRunID: fixture.run.ID, IdempotencyKey: key, Actor: "operator", Reason: "reject queued original redrive"})
		if err == nil || !strings.Contains(err.Error(), "not eligible") {
			t.Fatalf("redrive queued original = %v; want not eligible", err)
		}
	})
}

func standardAuthoringHandoffDefinitionProvider(t *testing.T) CodeEdgePhase1RunDefinitionProvider {
	t.Helper()
	return codeEdgePhase1DefinitionProviderFunc(func(_ context.Context, definition CodeEdgePhase1RunDefinitionRequest) (CodeEdgePhase1RunDefinition, error) {
		return CodeEdgePhase1RunDefinition{
			Profile:       lifecycleCompleteProfileForTemplate(t, workflowadapter.CodeEdgePhase1WorkflowTemplate()),
			ExecutionSpec: testsupport.CompleteCodeEdgePhase1RunExecutionSpec(definition.TaskID, definition.RevisionID, string(definition.RevisionDigest)),
		}, nil
	})
}

// queueStandardAuthoringInitialCoordinator mirrors the immutable initial
// dispatch written by StartAuthoringRun. The materializer fixture persists
// only the terminal stage facts, so it needs this durable parent handoff to
// exercise the same completion path as a production authoring Run.
func queueStandardAuthoringInitialCoordinator(t *testing.T, ctx context.Context, fixture standardAuthoringHandoffFixture) store.DurableJob {
	t.Helper()
	payload, err := json.Marshal(workflowRunExecutionPayload{
		Format:                   workflowRunExecutionPayloadFormat,
		RunID:                    fixture.run.ID,
		DefinitionHash:           fixture.run.DefinitionHash,
		ExecutionSpecFingerprint: workflowkit.SHA256Fingerprint([]byte("standard authoring handoff fixture execution spec")),
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := fixture.database.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "workflow_run.execute", EntityType: "workflow_run", EntityID: fixture.run.ID, RunID: fixture.run.ID,
		PayloadJSON: string(payload), IdempotencyKey: "workflow-run-execution:" + fixture.run.ID,
		Actor: "worker", Reason: "queue frozen authoring initial coordinator fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

type standardAuthoringHandoffFixture struct {
	root              string
	database          *store.Store
	source            store.AuthoringSource
	session           store.AuthoringSession
	run               store.WorkflowRun
	stageAttempt      store.StageAttempt
	handoff           workflowadapter.StandardAuthoringTaskHandoff
	handoffArtifactID string
}

func newStandardAuthoringHandoffFixture(t *testing.T) standardAuthoringHandoffFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	executor, err := NewStandardAuthoringMaterializeExecutor(StandardAuthoringMaterializeExecutorConfig{ManagedRoot: root, Store: database})
	if err != nil {
		t.Fatal(err)
	}
	sourceDigest := "sha256:" + strings.Repeat("a", 64)
	source, err := database.CreateAuthoringSource(ctx, store.CreateAuthoringSourceRequest{
		RepositoryURL: "https://github.com/tower-rs/tower-http.git", CommitSHA: "f066e10ebc07ea9050a2ce4576315abfa568edf4",
		SnapshotArtifactRef: sourceDigest, SnapshotContentDigest: sourceDigest, SnapshotSchemaVersion: "harbor.source-snapshot.v1",
		IdempotencyKey: "handoff-source", Actor: "author", Reason: "freeze source",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := database.CreateTaskV2(ctx, store.CreateTaskV2Request{
		Slug: "handoff-task", Title: "Handoff task", MetadataJSON: `{}`,
		SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA, Actor: "author", Reason: "reserve draft task",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := database.CreateAuthoringSession(ctx, store.CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID, WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringWorkflowTemplateVersion, SessionManifestJSON: `{"format":"test"}`,
		IdempotencyKey: "handoff-session", Actor: "author", Reason: "freeze session",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.CreateAuthoringWorkflowRun(ctx, store.CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID, WorkflowTemplateID: session.WorkflowTemplateID, WorkflowTemplateVersion: session.WorkflowTemplateVersion,
		ResolvedProfileHash: "sha256:handoff-profile", DefinitionHash: "sha256:handoff-definition", RunManifestJSON: `{}`,
		Trigger: "task.generate", Actor: "author", Reason: "start handoff fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, managedRunsDirectory, run.ID), 0o750); err != nil {
		t.Fatal(err)
	}
	run, err = database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "author", Reason: "run materializer"})
	if err != nil {
		t.Fatal(err)
	}
	stageAttempt, err := database.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: workflowadapter.MaterializeTask, StageGroup: string(workflowadapter.StageTaskGeneration), Ordinal: 1,
		InputFingerprint: "sha256:handoff-input", BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "worker", Reason: "create materialization stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	stageAttempt, err = database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{StageAttemptID: stageAttempt.ID, ExpectedVersion: stageAttempt.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "worker", Reason: "execute materialization stage"})
	if err != nil {
		t.Fatal(err)
	}
	profile := lifecycleCompleteProfileForTemplate(t, workflowadapter.StandardAuthoringWorkflowTemplate())
	resolved, err := workflowadapter.StandardAuthoringWorkflowTemplate().Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	stage, found := resolved.Descriptor.Stage(workflowkit.StageKey(workflowadapter.MaterializeTask))
	if !found {
		t.Fatal("compiled authoring workflow omitted materialize_task")
	}
	contents := map[string][]byte{
		"instruction":              []byte("# Task\n\nImplement the requested behavior.\n"),
		"task_toml":                []byte("[task]\nid = \"handoff-task\"\n"),
		"dockerfile":               []byte("FROM alpine:3.20\n"),
		"solve_script":             []byte("#!/bin/sh\nexit 0\n"),
		"test_script":              []byte("#!/bin/sh\nexit 0\n"),
		"tests_analysis":           []byte("The tests validate the requested behavior.\n"),
		"solution_review_decision": approvedAuthoringSolutionDecision(t, source, session, run),
	}
	inputs := standardAuthoringMaterializerBindings(t, stage, contents)
	subject := workflowkit.SubjectBinding{SubjectID: source.ID, RevisionID: session.ID, Digest: workflowkit.SubjectDigest(source.SnapshotContentDigest)}
	result, err := executor.ExecuteHarborBuiltin(ctx, stageprovider.StageOperationInvocation{
		Request: workflowkit.StageExecutionRequest{
			Execution: workflowkit.FrozenExecution{ID: run.ID, Subject: subject, Actor: "worker"},
			Claim:     workflowkit.JobClaim{Stage: &workflowkit.StageClaim{StageAttempt: workflowkit.AttemptIdentity{ID: workflowkit.AttemptID(stageAttempt.ID)}, Stage: stage}},
			Stage:     stage, Inputs: inputs,
			ReadInput: func(_ context.Context, binding workflowkit.ArtifactBinding) ([]byte, error) {
				return append([]byte(nil), contents[binding.Name]...), nil
			},
		},
		Resolution: workflowadapter.StageOperationResolution{StageKey: workflowkit.StageKey(workflowadapter.MaterializeTask)},
	}, workflowadapter.HarborBuiltinOperationPayload{HandlerID: standardAuthoringMaterializeHandlerID})
	if err != nil {
		t.Fatal(err)
	}
	handoff := parseMaterializerHandoff(t, result)
	node, err := database.CreateNodeAttempt(ctx, store.CreateNodeAttemptRequest{StageAttemptID: stageAttempt.ID, NodeID: workflowadapter.MaterializeTask, Generation: 0, Attempt: 1, IdempotencyKey: "handoff-node", Actor: "worker", Reason: "persist materialized outputs"})
	if err != nil {
		t.Fatal(err)
	}
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{OperationResolver: testsupport.AcceptAllStageOperationResolver()})
	if err != nil {
		t.Fatal(err)
	}
	resolvedSubject, err := services.core.resolveWorkflowRunSubject(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := make([]StageArtifact, 0, len(result.Artifacts))
	for _, artifact := range result.Artifacts {
		artifacts = append(artifacts, StageArtifact{ID: string(artifact.ID), Key: artifact.Name, SchemaVersion: artifact.SchemaVersion, Content: artifact.Content, TurnOrdinal: artifact.TurnOrdinal})
	}
	manifest, refs, err := persistStageArtifactsForSubject(ctx, services.core, run, resolvedSubject, stageAttempt, node, stage, inputs, artifacts, "worker", "persist materialized handoff outputs")
	if err != nil {
		t.Fatal(err)
	}
	stageAttempt, err = database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{StageAttemptID: stageAttempt.ID, ExpectedVersion: stageAttempt.Version, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictPass, ArtifactManifestID: manifest.ID, Actor: "worker", Reason: "persist materialization result"})
	if err != nil {
		t.Fatal(err)
	}
	handoffArtifactID := ""
	for _, reference := range refs {
		if reference.ArtifactKey == workflowadapter.StandardAuthoringTaskHandoffArtifact {
			handoffArtifactID = reference.ID
		}
	}
	if handoffArtifactID == "" {
		t.Fatal("persisted materializer output omitted handoff artifact")
	}
	materializedTask, err := database.GetTaskV2(ctx, task.ID)
	if err != nil || materializedTask == nil || materializedTask.CurrentRevisionID != handoff.RevisionID {
		t.Fatalf("materialized Task = %+v, %v; want current revision %s", materializedTask, err, handoff.RevisionID)
	}
	return standardAuthoringHandoffFixture{root: root, database: database, source: source, session: session, run: run, stageAttempt: stageAttempt, handoff: handoff, handoffArtifactID: handoffArtifactID}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
