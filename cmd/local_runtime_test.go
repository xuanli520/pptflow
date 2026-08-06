package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

func TestRunAttachCommandEmitsReadOnlyAttachmentWithoutDurableEffects(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	services := openCommandLifecycle(t, root)
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{Slug: "attach-command", Actor: "tester", Reason: "run attach command fixture"},
		SourceDirectory:        writeCommandTaskSnapshot(t, "attach command fixture\n"),
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: commandCompleteProfile(t), ExecutionSpec: commandExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "attach-command", Actor: "tester", Reason: "start attach command fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	run, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "tester", Reason: "start attach command worker",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	jobs, err := services.Store().ListDurableJobsForRun(ctx, run.ID)
	if err != nil || len(jobs) != 1 {
		services.Store().Close()
		t.Fatalf("list attach command durable job = %+v, %v", jobs, err)
	}
	claim, err := services.Store().ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
		IdempotencyKey: "attach-command-claim", Owner: "attach-command-worker", LeaseTTL: time.Minute, Actor: "tester", Reason: "claim attach command job",
	})
	if err != nil || claim.Job == nil || claim.DispatchLease == nil || claim.Job.ID != jobs[0].ID {
		services.Store().Close()
		t.Fatalf("claim attach command durable job = %+v, %v", claim, err)
	}
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}

	before := snapshotCommandControlPlane(t, root)
	command := newRunCommandV2(&lifecycleCLIConfig{root: root})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"attach", "--run", run.ID})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatalf("run attach command: %v\n%s", err, output.String())
	}
	var attachment app.RunAttachment
	if err := json.Unmarshal(output.Bytes(), &attachment); err != nil {
		t.Fatalf("decode run attach output: %v\n%s", err, output.String())
	}
	if attachment.Run.ID != run.ID || attachment.AttachableJobs != 1 || len(attachment.Jobs) != 1 || !attachment.Jobs[0].Attachable {
		t.Fatalf("run attach output = %+v", attachment)
	}
	if after := snapshotCommandControlPlane(t, root); !reflect.DeepEqual(after, before) {
		t.Fatal("run attach changed the control plane or managed files")
	}
}

func TestRunReconcileCommandRequiresReasonAndScopesRecoveryToNamedRun(t *testing.T) {
	ctx := context.Background()
	actor := defaultLifecycleActor()
	if actor == "" {
		t.Skip("local OS actor is unavailable in this test environment")
	}
	root := t.TempDir()
	services := openCommandLifecycle(t, root)
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{Slug: "reconcile-command", Actor: actor, Reason: "run reconcile command fixture"},
		SourceDirectory:        writeCommandTaskSnapshot(t, "reconcile command fixture\n"),
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	runA := startCommandLocalRuntimeRun(t, ctx, services, task.ID, revision.ID, actor, "reconcile-command-a")
	runB := startCommandLocalRuntimeRun(t, ctx, services, task.ID, revision.ID, actor, "reconcile-command-b")
	claimA := claimCommandLocalRuntimeRunJob(t, ctx, services, runA.ID, "reconcile-command-claim-a")
	claimB := claimCommandLocalRuntimeRunJob(t, ctx, services, runB.ID, "reconcile-command-claim-b")
	time.Sleep(40 * time.Millisecond)
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}

	config := &lifecycleCLIConfig{root: root}
	missingReason := newRunCommandV2(config)
	missingReason.SetArgs([]string{"reconcile", "--run", runA.ID})
	if err := missingReason.ExecuteContext(ctx); err == nil {
		t.Fatal("run reconcile accepted a missing audit reason")
	}

	command := newRunCommandV2(config)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"reconcile", "--run", runA.ID, "--reason", "recover selected local worker"})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatalf("run reconcile command: %v\n%s", err, output.String())
	}
	var result app.RunReconciliationResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode run reconcile output: %v\n%s", err, output.String())
	}
	if len(result.RecoveredJobs) != 1 || result.RecoveredJobs[0].Job.ID != claimA.Job.ID || result.RecoveredJobs[0].Job.State != store.JobInDoubt || result.RecoveredJobs[0].Job.Failure == nil || result.RecoveredJobs[0].Job.Failure.Code != "job.lease_lost" {
		t.Fatalf("run reconcile output = %+v", result)
	}

	check := openCommandLifecycle(t, root)
	defer check.Store().Close()
	selected, err := check.Store().GetDurableJob(ctx, claimA.Job.ID)
	if err != nil || selected == nil || selected.State != store.JobInDoubt || selected.Failure == nil || selected.Failure.Code != "job.lease_lost" {
		t.Fatalf("selected Run job after reconcile = %+v, %v", selected, err)
	}
	unselected, err := check.Store().GetDurableJob(ctx, claimB.Job.ID)
	if err != nil || unselected == nil || unselected.State != store.JobRunning {
		t.Fatalf("unselected Run job changed by reconcile = %+v, %v", unselected, err)
	}
	unselectedLease, err := check.Store().GetLease(ctx, claimB.DispatchLease.ID)
	if err != nil || unselectedLease == nil || unselectedLease.State != store.LeaseActive {
		t.Fatalf("unselected Run lease changed by reconcile = %+v, %v", unselectedLease, err)
	}
}

func startCommandLocalRuntimeRun(t *testing.T, ctx context.Context, services *app.LifecycleServices, taskID, revisionID, actor, trigger string) store.WorkflowRun {
	t.Helper()
	revision, err := services.Store().GetTaskRevision(ctx, revisionID)
	if err != nil || revision == nil || revision.TaskID != taskID {
		t.Fatalf("load command local runtime revision = %+v, %v", revision, err)
	}
	run, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: taskID, RevisionID: revisionID, Profile: commandCompleteProfile(t), ExecutionSpec: commandExecutionSpec(taskID, revisionID, revision.TaskDigest), Trigger: trigger, Actor: actor, Reason: "start command local runtime fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: actor, Reason: "start command local runtime worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func claimCommandLocalRuntimeRunJob(t *testing.T, ctx context.Context, services *app.LifecycleServices, runID, key string) store.DurableJobDispatchClaim {
	t.Helper()
	jobs, err := services.Store().ListDurableJobsForRun(ctx, runID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list command local runtime jobs = %+v, %v", jobs, err)
	}
	claim, err := services.Store().ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
		IdempotencyKey: key, Owner: "command-local-runtime-worker", LeaseTTL: 10 * time.Millisecond,
		Actor: "command-local-runtime-worker", Reason: "claim command local runtime job",
	})
	if err != nil || claim.Job == nil || claim.DispatchLease == nil || claim.Job.ID != jobs[0].ID {
		t.Fatalf("claim command local runtime job = %+v, %v", claim, err)
	}
	return claim
}
