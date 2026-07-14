package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

type automaticRepairAgent struct {
	mu    sync.Mutex
	calls int
}

var _ agent.Runtime = (*automaticRepairAgent)(nil)

func (agentRuntime *automaticRepairAgent) OpenConversation(_ context.Context, request agent.ConversationRequest) (agent.Conversation, error) {
	return automaticRepairConversation{agent: agentRuntime, checkout: request.ProjectPath}, nil
}

func (agentRuntime *automaticRepairAgent) Calls() int {
	agentRuntime.mu.Lock()
	defer agentRuntime.mu.Unlock()
	return agentRuntime.calls
}

type automaticRepairConversation struct {
	agent    *automaticRepairAgent
	checkout string
}

func (conversation automaticRepairConversation) Turn(_ context.Context, _ agent.TurnRequest) (agent.TurnResult, error) {
	conversation.agent.mu.Lock()
	conversation.agent.calls++
	round := conversation.agent.calls
	conversation.agent.mu.Unlock()
	if err := os.WriteFile(filepath.Join(conversation.checkout, "instruction.md"), []byte(fmt.Sprintf("automatic repair round %d\n", round)), 0o644); err != nil {
		return agent.TurnResult{}, err
	}
	return agent.TurnResult{Text: fmt.Sprintf("repaired round %d", round), Model: "automatic-repair-test"}, nil
}

func (automaticRepairConversation) Close() error { return nil }

type repairLoopFixture struct {
	services  *LifecycleServices
	store     *store.Store
	task      store.TaskV2
	session   store.RepairSession
	candidate store.RevisionCandidate
	childRun  store.WorkflowRun
	agent     *automaticRepairAgent
}

func newRepairLoopFixture(t *testing.T, maxRounds int) repairLoopFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	services, err := newLifecycleServicesForTest(root, database)
	if err != nil {
		t.Fatal(err)
	}
	agentRuntime := &automaticRepairAgent{}
	services.Changes.Register(AgentRepairProvider{Agent: agentRuntime})
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "automatic-repair", Actor: "repair-owner", Reason: "import automatic repair fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "original instruction\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCompleteProfile(t), ExecutionSpec: lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest),
		Trigger: "verify", Actor: "repair-owner", Reason: "verify automatic repair fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := services.Continuations.CurrentCheckpoint(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := services.Continuations.PlanTaskContinuation(ctx, ContinueTaskCommand{
		CommandKey: "automatic-repair-root", TaskID: task.ID, RunID: run.ID, Expected: checkpoint, Actor: "repair-owner", Reason: "start automatic repair",
		Change: &TaskChangeRequest{
			ProviderID: AgentRepairProviderID, OperationKey: "automatic-repair-root-operation", MaxRepairRounds: maxRounds,
			Payload:  json.RawMessage(`{"format":"harbor.agent-repair.v1","guidance":"repair the structured findings"}`),
			Findings: findingBundleForRun(t, ctx, services, database, run, revision, "repair fixture finding"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Continuations.ExecuteTaskContinuation(ctx, plan.ID()); err != nil {
		t.Fatalf("commit first repair candidate: %v", err)
	}
	candidate, err := database.GetRevisionCandidate(ctx, plan.Snapshot().CandidateRevisionID)
	if err != nil || candidate == nil {
		t.Fatalf("load first repair candidate = %+v, %v", candidate, err)
	}
	session, err := database.GetRepairSession(ctx, candidate.RepairSessionID)
	if err != nil || session == nil {
		t.Fatalf("load repair session = %+v, %v", session, err)
	}
	childRun, err := database.GetWorkflowRun(ctx, candidate.TargetRunID)
	if err != nil || childRun == nil {
		t.Fatalf("load first repair child run = %+v, %v", childRun, err)
	}
	return repairLoopFixture{services: services, store: database, task: task, session: *session, candidate: *candidate, childRun: *childRun, agent: agentRuntime}
}

func (fixture repairLoopFixture) makeChildRunNeedsRepair(t *testing.T) store.WorkflowRun {
	t.Helper()
	ctx := context.Background()
	run, err := fixture.store.GetWorkflowRun(ctx, fixture.childRun.ID)
	if err != nil || run == nil {
		t.Fatalf("load repair child run = %+v, %v", run, err)
	}
	current := *run
	current, err = fixture.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: current.ID, ExpectedVersion: current.Version, Status: store.WorkflowRunRunning, Actor: "repair-worker", Reason: "execute repair child run",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := fixture.store.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: current.ID, StageKey: workflowadapter.QualityCheck, StageGroup: "quality", Ordinal: 1,
		InputFingerprint: "automatic-repair-needs-repair", BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "repair-worker", Reason: "record repairable finding",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "repair-worker", Reason: "execute repairable check",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictNeedsRepair, Actor: "repair-worker", Reason: "quality reported needs repair",
	}); err != nil {
		t.Fatal(err)
	}
	current, err = fixture.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: current.ID, ExpectedVersion: current.Version, Status: store.WorkflowRunWaitingContinuation, Actor: "repair-worker", Reason: "quality requires automatic repair",
	})
	if err != nil {
		t.Fatal(err)
	}
	return current
}

func (fixture repairLoopFixture) transitionChildRun(t *testing.T, target store.WorkflowRunStatus) store.WorkflowRun {
	t.Helper()
	ctx := context.Background()
	run, err := fixture.store.GetWorkflowRun(ctx, fixture.childRun.ID)
	if err != nil || run == nil {
		t.Fatalf("load repair child run = %+v, %v", run, err)
	}
	current := *run
	if current.Status == store.WorkflowRunQueued {
		current, err = fixture.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: current.ID, ExpectedVersion: current.Version, Status: store.WorkflowRunRunning, Actor: "repair-worker", Reason: "execute repair child run"})
		if err != nil {
			t.Fatal(err)
		}
	}
	current, err = fixture.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: current.ID, ExpectedVersion: current.Version, Status: target, Actor: "repair-worker", Reason: "project repair child outcome"})
	if err != nil {
		t.Fatal(err)
	}
	return current
}

func TestRepairLoopQueuesBoundSecondRoundAndReplaysIdempotently(t *testing.T) {
	ctx := context.Background()
	fixture := newRepairLoopFixture(t, 2)
	run := fixture.makeChildRunNeedsRepair(t)

	job, queued, err := fixture.services.Repairs.EnqueueRunOutcome(ctx, run.ID, "repair-worker", "queue automatic repair")
	if err != nil || !queued || job.CommandType != repairSessionAdvanceCommandType {
		t.Fatalf("enqueue automatic repair = %+v queued=%t err=%v", job, queued, err)
	}
	result, err := fixture.services.Repairs.HandleDurableJob(ctx, job)
	if err != nil {
		t.Fatalf("advance automatic repair: %v", err)
	}
	if result.Action != "next_round_queued" || result.PlanID == "" || result.Execution.ID == "" || result.Candidate.RepairSessionID != fixture.session.ID || result.Candidate.RoundOrdinal != 2 || result.Candidate.State != store.RevisionCandidateCommitted {
		t.Fatalf("automatic repair result = %+v", result)
	}
	if fixture.agent.Calls() != 2 {
		t.Fatalf("agent calls after two repair rounds = %d, want 2", fixture.agent.Calls())
	}
	command, err := fixture.store.GetContinuationCommand(ctx, result.Candidate.CommandID)
	if err != nil || command == nil || command.CommandKey != automaticRepairCommandKey(fixture.session.ID, 2) {
		t.Fatalf("round two continuation command = %+v, %v", command, err)
	}
	if plan, err := fixture.services.Continuations.GetTaskContinuationPlan(ctx, result.PlanID); err != nil || plan.Snapshot().CandidateRevisionID != result.Candidate.ID {
		t.Fatalf("round two frozen plan = %+v, %v", plan, err)
	}

	replayed, err := fixture.services.Repairs.HandleDurableJob(ctx, job)
	if err != nil || replayed.Candidate.ID != result.Candidate.ID || replayed.Execution.ID != result.Execution.ID {
		t.Fatalf("replayed automatic repair = %+v, %v", replayed, err)
	}
	if fixture.agent.Calls() != 2 {
		t.Fatalf("repair coordinator replay invoked agent again: %d calls", fixture.agent.Calls())
	}
}

func TestRepairLoopTransitionsSessionToNeedsHumanAtFrozenRoundLimit(t *testing.T) {
	ctx := context.Background()
	fixture := newRepairLoopFixture(t, 1)
	run := fixture.makeChildRunNeedsRepair(t)
	job, queued, err := fixture.services.Repairs.EnqueueRunOutcome(ctx, run.ID, "repair-worker", "queue exhausted repair")
	if err != nil || !queued {
		t.Fatalf("enqueue exhausted repair = %+v queued=%t err=%v", job, queued, err)
	}
	result, err := fixture.services.Repairs.HandleDurableJob(ctx, job)
	if err != nil || result.Action != "needs_human" || result.Session.Status != store.RepairSessionNeedsHuman {
		t.Fatalf("exhausted repair result = %+v, %v", result, err)
	}
	if fixture.agent.Calls() != 1 {
		t.Fatalf("exhausted repair invoked another provider turn: %d calls", fixture.agent.Calls())
	}
}

func TestRepairLoopCompletesOrEscalatesTerminalChildOutcomes(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name       string
		status     store.WorkflowRunStatus
		wantStatus store.RepairSessionState
		wantAction string
	}{
		{name: "pass", status: store.WorkflowRunSucceeded, wantStatus: store.RepairSessionCompleted, wantAction: "completed"},
		{name: "reject", status: store.WorkflowRunFailedTerminal, wantStatus: store.RepairSessionNeedsHuman, wantAction: "needs_human"},
		{name: "reconcile", status: store.WorkflowRunInDoubt, wantStatus: store.RepairSessionNeedsHuman, wantAction: "needs_human"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRepairLoopFixture(t, 2)
			run := fixture.transitionChildRun(t, test.status)
			job, queued, err := fixture.services.Repairs.EnqueueRunOutcome(ctx, run.ID, "repair-worker", "queue terminal repair outcome")
			if err != nil || !queued {
				t.Fatalf("enqueue terminal repair outcome = %+v queued=%t err=%v", job, queued, err)
			}
			result, err := fixture.services.Repairs.HandleDurableJob(ctx, job)
			if err != nil || result.Action != test.wantAction || result.Session.Status != test.wantStatus {
				t.Fatalf("terminal repair result = %+v, %v", result, err)
			}
		})
	}
}

func TestRepairLoopExplicitRecoveryUsesSameRoundIdentity(t *testing.T) {
	ctx := context.Background()
	fixture := newRepairLoopFixture(t, 2)
	run := fixture.makeChildRunNeedsRepair(t)

	first, err := fixture.services.Repairs.RecoverRunOutcome(ctx, run.ID)
	if err != nil || first.Action != "next_round_queued" {
		t.Fatalf("recover automatic repair = %+v, %v", first, err)
	}
	second, err := fixture.services.Repairs.RecoverRunOutcome(ctx, run.ID)
	if err != nil || second.Candidate.ID != first.Candidate.ID || second.Execution.ID != first.Execution.ID {
		t.Fatalf("recovered automatic repair replay = %+v, %v", second, err)
	}
	if fixture.agent.Calls() != 2 {
		t.Fatalf("recovery replay invoked agent again: %d calls", fixture.agent.Calls())
	}
}

func TestFrozenRuntimeQueuesAndConsumesAutomaticRepairAdvance(t *testing.T) {
	ctx := context.Background()
	fixture := newRepairLoopFixture(t, 2)
	frozen, err := decodeFrozenRunDefinition(fixture.childRun)
	if err != nil {
		t.Fatal(err)
	}
	stage, found := frozen.Workflow.Stage(workflowkit.StageKey(workflowadapter.QualityCheck))
	if !found {
		t.Fatalf("repair child workflow omits %q", workflowadapter.QualityCheck)
	}
	child, err := fixture.store.GetWorkflowRun(ctx, fixture.childRun.ID)
	if err != nil || child == nil {
		t.Fatalf("load repair child run = %+v, %v", child, err)
	}
	running, err := fixture.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: child.ID, ExpectedVersion: child.Version, Status: store.WorkflowRunRunning, Actor: "repair-runtime", Reason: "project quality repair outcome",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := fixture.store.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: running.ID, StageKey: string(stage.Key), StageGroup: stage.Group, Ordinal: 1,
		InputFingerprint: "automatic-repair-runtime-needs-repair", BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "repair-runtime", Reason: "record runtime repairable quality result",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "repair-runtime", Reason: "start runtime quality check",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictNeedsRepair,
		Actor: "repair-runtime", Reason: "quality check requires repair",
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := workflowkit.NewControlledPluginRegistry([]workflowkit.PluginRegistration[workflowkit.StageExecutor]{
		{Binding: stage.Plugin, Implementation: workflowkit.StageExecutorFunc(completedFixtureStage)},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newFrozenRuntime(t, fixture.services, registry)
	state, err := runtime.afterStageTerminal(ctx, store.DurableJob{CreatedBy: "repair-runtime"}, running, frozen, frozenStageExecutionPayload{}, stage, attempt, nil, "", nil)
	if err != nil || state != store.JobSucceeded {
		t.Fatalf("runtime quality terminal projection = %s, %v", state, err)
	}

	jobs, err := fixture.store.ListDurableJobsForRun(ctx, fixture.childRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	var advance store.DurableJob
	for _, job := range jobs {
		if job.CommandType == repairSessionAdvanceCommandType && job.State == store.JobQueued {
			advance = job
			break
		}
	}
	if advance.ID == "" || advance.EntityID != fixture.session.ID || advance.RunID != fixture.childRun.ID {
		t.Fatalf("runtime did not queue a bound automatic repair advance: %+v", advance)
	}
	child, err = fixture.store.GetWorkflowRun(ctx, fixture.childRun.ID)
	if err != nil || child == nil || child.Status != store.WorkflowRunWaitingContinuation {
		t.Fatalf("quality repair run projection = %+v, %v", child, err)
	}
	if fixture.agent.Calls() != 1 {
		t.Fatalf("quality terminal projection unexpectedly called repair provider: %d calls", fixture.agent.Calls())
	}

	// The fixture's first repair candidate has its own continuation-delivery
	// job. Retire that predecessor delivery so this worker cycle is scoped to
	// the new repair-session coordinator rather than re-running the fixture's
	// already-projected stage setup.
	predecessor, err := fixture.store.ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
		IdempotencyKey: "automatic-repair-runtime-predecessor", Owner: "automatic-repair-runtime-predecessor", RunID: fixture.childRun.ID,
		LeaseTTL: time.Minute, Actor: "repair-runtime", Reason: "retire fixture predecessor continuation delivery",
	})
	if err != nil || predecessor.Job == nil || predecessor.Job.CommandType != "task_continuation.execute" {
		t.Fatalf("claim fixture predecessor continuation = %+v, %v", predecessor, err)
	}
	if _, err := fixture.store.TransitionDurableJob(ctx, store.TransitionDurableJobRequest{
		JobID: predecessor.Job.ID, ExpectedVersion: predecessor.Job.Version, State: store.JobSucceeded,
		Actor: "repair-runtime", Reason: "fixture predecessor delivery retired before repair coordinator dispatch",
	}); err != nil {
		t.Fatal(err)
	}

	worker, err := NewDurableWorker(DurableWorkerConfig{
		Store: fixture.store, Owner: "automatic-repair-runtime-worker", Actor: "repair-runtime", Reason: "automatic repair runtime integration test",
		RunID: fixture.childRun.ID, LeaseTTL: time.Second, HeartbeatEvery: 100 * time.Millisecond, PollInterval: time.Millisecond, Handler: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.RunOnce(ctx)
	if err != nil || result.FinalState != store.JobSucceeded || result.Job == nil || result.Job.ID != advance.ID {
		t.Fatalf("durable repair advance delivery = %+v, %v", result, err)
	}
	if fixture.agent.Calls() != 2 {
		t.Fatalf("durable repair advance did not create exactly one second provider round: %d calls", fixture.agent.Calls())
	}
	candidates, err := fixture.store.ListRevisionCandidatesForTask(ctx, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	roundTwo := 0
	for _, candidate := range candidates {
		if candidate.RepairSessionID != fixture.session.ID || candidate.RoundOrdinal != 2 {
			continue
		}
		roundTwo++
		if candidate.State != store.RevisionCandidateCommitted || candidate.CommandID == "" {
			t.Fatalf("runtime-created repair candidate = %+v", candidate)
		}
	}
	if roundTwo != 1 {
		t.Fatalf("runtime-created round-two candidates = %d, want 1 in %+v", roundTwo, candidates)
	}
}
