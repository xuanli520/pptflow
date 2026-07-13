package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const commandStageArtifactManifestFormat = "harbor.v2.stage-artifact-manifest.v1"

type commandStageArtifactManifest struct {
	Format         string                       `json:"format"`
	RunID          string                       `json:"run_id"`
	StageAttemptID string                       `json:"stage_attempt_id"`
	NodeAttemptID  string                       `json:"node_attempt_id"`
	StageKey       workflowkit.StageKey         `json:"stage_key"`
	Artifacts      []commandStageArtifactObject `json:"artifacts"`
}

type commandStageArtifactObject struct {
	Key           string                  `json:"key"`
	SchemaVersion string                  `json:"schema_version"`
	Digest        workflowkit.Fingerprint `json:"digest"`
	SizeBytes     int64                   `json:"size_bytes"`
	TurnOrdinal   int                     `json:"turn_ordinal"`
}

func TestDefaultLifecycleActorIgnoresEnvironmentOverrides(t *testing.T) {
	t.Setenv("USER", "forged-user")
	t.Setenv("USERNAME", "forged-user")
	actor := defaultLifecycleActor()
	if actor == "forged-user" {
		t.Fatalf("lifecycle actor accepted an environment override: %q", actor)
	}
	if current, err := user.Current(); err == nil && current.Username != "" && actor != current.Username {
		t.Fatalf("lifecycle actor = %q, want current OS user %q", actor, current.Username)
	}
}

func TestTaskImportCommandCreatesManagedImmutableTask(t *testing.T) {
	root := t.TempDir()
	source := writeCommandTaskSnapshot(t, "command fixture\n")
	config := &lifecycleCLIConfig{root: root}
	command := newTaskCommand(config)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{
		"import",
		"--source", source,
		"--slug", "command-task",
		"--repo", "https://example.invalid/repository",
		"--commit", "deadbeef",
		"--reason", "import a controlled fixture",
	})
	if err := command.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("task import command: %v\n%s", err, output.String())
	}
	var result struct {
		Task     store.TaskV2
		Revision store.TaskRevision
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode command output: %v\n%s", err, output.String())
	}
	if result.Task.ID == "" || result.Revision.ID == "" || result.Revision.TaskID != result.Task.ID {
		t.Fatalf("unexpected import result: %+v", result)
	}
	services := openCommandLifecycle(t, root)
	defer services.Store().Close()
	snapshot, err := services.Revisions.SnapshotDirectory(result.Task.ID, result.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(snapshot, "instruction.md")); err != nil {
		t.Fatalf("managed snapshot missing instruction: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "harbor.db")); err != nil {
		t.Fatalf("V2 command did not initialize SQLite control plane: %v", err)
	}
}

func TestTaskMutationCommandRequiresReasonAndOptimisticVersion(t *testing.T) {
	root := t.TempDir()
	config := &lifecycleCLIConfig{root: root}
	services := openCommandLifecycle(t, root)
	task, err := services.Tasks.CreateDraft(context.Background(), app.CreateDraftTaskRequest{
		Slug: "needs-audit", Actor: defaultLifecycleActor(), Reason: "fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}

	missingReason := newTaskCommand(config)
	missingReason.SetArgs([]string{"archive", "--task", task.ID, "--expected-version", "1"})
	if err := missingReason.ExecuteContext(context.Background()); err == nil {
		t.Fatal("archive command accepted a missing audit reason")
	}

	missingVersion := newTaskCommand(config)
	missingVersion.SetArgs([]string{"archive", "--task", task.ID, "--reason", "archive fixture"})
	if err := missingVersion.ExecuteContext(context.Background()); err == nil {
		t.Fatal("archive command accepted a missing optimistic version")
	}
}

func TestTaskPurgeDryRunReportsBlockersWithoutMutation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	actor := defaultLifecycleActor()
	if actor == "" {
		t.Skip("local OS actor is unavailable in this test environment")
	}
	services := openCommandLifecycle(t, root)
	task, err := services.Tasks.CreateDraft(ctx, app.CreateDraftTaskRequest{
		Slug: "purge-cli", Actor: actor, Reason: "create purge command fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	deleted, _, err := services.Deletion.SoftDeleteTask(ctx, task.ID, task.Version, actor, "soft delete purge command fixture")
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	pending, err := services.Store().CreateOutboxEvent(ctx, store.CreateOutboxEventRequest{
		Topic: "task.purge.fixture", EntityType: "task", EntityID: task.ID,
		PayloadJSON: `{"fixture":true}`, IdempotencyKey: "purge-cli-pending", Actor: actor, Reason: "hold purge command fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	beforeEvents, err := services.Store().ListAuditEvents(ctx, store.ListAuditEventsRequest{})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}

	config := &lifecycleCLIConfig{root: root}
	command := newTaskCommand(config)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"purge", "--task", task.ID, "--dry-run"})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatalf("purge dry-run: %v\n%s", err, output.String())
	}
	var preview app.PurgeTaskPreview
	if err := json.Unmarshal(output.Bytes(), &preview); err != nil {
		t.Fatalf("decode purge preview: %v\n%s", err, output.String())
	}
	if preview.WillMutate || preview.Eligible || preview.Task.ID != task.ID || preview.Task.Version != deleted.Version || !hasCommandPurgeBlocker(preview.Blockers, "pending_outbox", pending.ID) {
		t.Fatalf("purge preview = %+v", preview)
	}

	check := openCommandLifecycle(t, root)
	unchanged, err := check.Tasks.Get(ctx, task.ID)
	if err != nil {
		check.Store().Close()
		t.Fatal(err)
	}
	if unchanged.Version != deleted.Version || unchanged.LifecycleState != store.TaskLifecycleDeleted {
		check.Store().Close()
		t.Fatalf("purge dry-run changed task: %+v", unchanged)
	}
	afterEvents, err := check.Store().ListAuditEvents(ctx, store.ListAuditEventsRequest{})
	if err != nil {
		check.Store().Close()
		t.Fatal(err)
	}
	if len(afterEvents) != len(beforeEvents) {
		check.Store().Close()
		t.Fatalf("purge dry-run wrote audit/deletion state: before=%d after=%d", len(beforeEvents), len(afterEvents))
	}
	if err := check.Store().Close(); err != nil {
		t.Fatal(err)
	}

	nonDryRun := newTaskCommand(config)
	nonDryRun.SetArgs([]string{"purge", "--task", task.ID})
	if err := nonDryRun.ExecuteContext(ctx); err == nil {
		t.Fatal("purge command accepted an irreversible execution request without --yes")
	}
}

func TestTaskPurgeCommandExecutesWithCASConfirmationAndIdempotentReplay(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	actor := defaultLifecycleActor()
	if actor == "" {
		t.Skip("local OS actor is unavailable in this test environment")
	}
	services := openCommandLifecycle(t, root)
	task, err := services.Tasks.CreateDraft(ctx, app.CreateDraftTaskRequest{
		Slug: "purge-cli-execute", Actor: actor, Reason: "create purge command fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	deleted, _, err := services.Deletion.SoftDeleteTask(ctx, task.ID, task.Version, actor, "soft delete purge command fixture")
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	directory := filepath.Join(root, "tasks", task.ID)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "ephemeral.txt"), []byte("purge me"), 0o640); err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}

	config := &lifecycleCLIConfig{root: root}
	args := []string{
		"purge", "--task", task.ID, "--expected-version", fmt.Sprint(deleted.Version),
		"--idempotency-key", "purge-cli-execute-key", "--reason", "retention elapsed", "--yes",
	}
	command := newTaskCommand(config)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatalf("execute task purge command: %v\n%s", err, output.String())
	}
	var result app.PurgeTaskResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode task purge result: %v\n%s", err, output.String())
	}
	if !result.Purged || result.InProgress || result.Operation.State != store.TaskPurgeCompleted {
		t.Fatalf("task purge result = %+v", result)
	}
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatalf("task purge did not remove managed directory: %v", err)
	}

	replayCommand := newTaskCommand(config)
	var replayOutput bytes.Buffer
	replayCommand.SetOut(&replayOutput)
	replayCommand.SetErr(&replayOutput)
	replayCommand.SetArgs(args)
	if err := replayCommand.ExecuteContext(ctx); err != nil {
		t.Fatalf("replay task purge command: %v\n%s", err, replayOutput.String())
	}
	var replay app.PurgeTaskResult
	if err := json.Unmarshal(replayOutput.Bytes(), &replay); err != nil {
		t.Fatalf("decode replay task purge result: %v\n%s", err, replayOutput.String())
	}
	if !replay.Purged || replay.Operation.ID != result.Operation.ID || replay.Operation.Version != result.Operation.Version {
		t.Fatalf("task purge replay = %+v initial=%+v", replay, result)
	}

	stale := newTaskCommand(config)
	stale.SetArgs([]string{
		"purge", "--task", task.ID, "--expected-version", fmt.Sprint(deleted.Version - 1),
		"--idempotency-key", "purge-cli-stale-key", "--reason", "stale command", "--yes",
	})
	if err := stale.ExecuteContext(ctx); err == nil {
		t.Fatal("task purge command accepted a stale task version")
	}
}

func hasCommandPurgeBlocker(blockers []app.PurgeTaskBlocker, code, id string) bool {
	for _, blocker := range blockers {
		if blocker.Code != code {
			continue
		}
		for _, candidate := range blocker.IDs {
			if candidate == id {
				return true
			}
		}
	}
	return false
}

func openCommandLifecycle(t *testing.T, root string) *app.LifecycleServices {
	t.Helper()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	services, err := app.NewLifecycleServices(root, database)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	return services
}

func writeCommandTaskSnapshot(t *testing.T, instruction string) string {
	t.Helper()
	root := t.TempDir()
	for _, file := range []struct {
		path string
		data string
		mode os.FileMode
	}{
		{path: "instruction.md", data: instruction, mode: 0o644},
		{path: "task.toml", data: "[task]\nname = \"command\"\n", mode: 0o644},
		{path: "tests_analysis.md", data: "analysis\n", mode: 0o644},
		{path: "environment/Dockerfile", data: "FROM alpine:3.21\n", mode: 0o644},
		{path: "solution/solve.sh", data: "#!/bin/sh\nexit 0\n", mode: 0o755},
		{path: "tests/test.sh", data: "#!/bin/sh\nexit 0\n", mode: 0o755},
	} {
		path := filepath.Join(root, filepath.FromSlash(file.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file.data), file.mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestTaskContinueCommandPlansPreviewsAndExecutesFrozenGroupPlan(t *testing.T) {
	ctx := context.Background()
	actor := defaultLifecycleActor()
	if actor == "" {
		t.Skip("local OS actor is unavailable in this test environment")
	}
	root, run := newCommandContinuationFixture(t, ctx)
	config := &lifecycleCLIConfig{root: root}
	dryRunArgs := []string{
		"continue", "--run", run.ID, "--from-stage", "quality", "--scope", "affected",
		"--idempotency-key", "continue-cli-preview", "--reason", "preview quality continuation", "--dry-run",
	}
	beforePreview := snapshotCommandControlPlane(t, root)
	previewStarted := time.Now().UTC()
	previewOutput, err := executeTaskCommand(t, ctx, config, dryRunArgs)
	previewFinished := time.Now().UTC()
	if err != nil {
		t.Fatalf("task continue dry-run: %v\n%s", err, previewOutput)
	}
	var preview taskContinuationPlanOutput
	if err := json.Unmarshal([]byte(previewOutput), &preview); err != nil {
		t.Fatalf("decode dry-run output: %v\n%s", err, previewOutput)
	}
	if preview.Persisted || preview.Executable || preview.Plan.PlanID == "" {
		t.Fatalf("dry-run output = %+v", preview)
	}
	assertContinuationQualityGroup(t, preview.Plan)
	if preview.Plan.ExpiresAt.Before(previewStarted.Add(workflowadapter.RequiredContinuationPlanTTL)) || preview.Plan.ExpiresAt.After(previewFinished.Add(workflowadapter.RequiredContinuationPlanTTL)) {
		t.Fatalf("dry-run expiry = %s, want frozen 24h range [%s, %s]", preview.Plan.ExpiresAt, previewStarted.Add(workflowadapter.RequiredContinuationPlanTTL), previewFinished.Add(workflowadapter.RequiredContinuationPlanTTL))
	}
	afterPreview := snapshotCommandControlPlane(t, root)
	if !reflect.DeepEqual(afterPreview, beforePreview) {
		t.Fatal("task continue --dry-run changed durable control-plane state")
	}

	invalidOutput, err := executeTaskCommand(t, ctx, config, []string{
		"continue", "--run", run.ID, "--from-stage", "quality_check", "--scope", "affected",
		"--idempotency-key", "continue-cli-invalid-group", "--reason", "reject node selector", "--dry-run",
	})
	if err == nil {
		t.Fatalf("task continue accepted a node key as --from-stage: %s", invalidOutput)
	}
	if afterInvalid := snapshotCommandControlPlane(t, root); !reflect.DeepEqual(afterInvalid, beforePreview) {
		t.Fatal("invalid --from-stage changed durable control-plane state")
	}

	planArgs := []string{
		"continue", "--run", run.ID, "--from-stage", "quality", "--scope", "affected",
		"--idempotency-key", "continue-cli-plan", "--reason", "recompute quality group",
	}
	planStarted := time.Now().UTC()
	planOutput, err := executeTaskCommand(t, ctx, config, planArgs)
	planFinished := time.Now().UTC()
	if err != nil {
		t.Fatalf("task continue plan: %v\n%s", err, planOutput)
	}
	var planned taskContinuationPlanOutput
	if err := json.Unmarshal([]byte(planOutput), &planned); err != nil {
		t.Fatalf("decode plan output: %v\n%s", err, planOutput)
	}
	if !planned.Persisted || !planned.Executable || planned.Plan.PlanID == "" {
		t.Fatalf("plan output = %+v", planned)
	}
	assertContinuationQualityGroup(t, planned.Plan)
	if planned.Plan.ExpiresAt.Before(planStarted.Add(workflowadapter.RequiredContinuationPlanTTL)) || planned.Plan.ExpiresAt.After(planFinished.Add(workflowadapter.RequiredContinuationPlanTTL)) {
		t.Fatalf("planned expiry = %s, want frozen 24h range [%s, %s]", planned.Plan.ExpiresAt, planStarted.Add(workflowadapter.RequiredContinuationPlanTTL), planFinished.Add(workflowadapter.RequiredContinuationPlanTTL))
	}

	replayOutput, err := executeTaskCommand(t, ctx, config, planArgs)
	if err != nil {
		t.Fatalf("task continue idempotent plan replay: %v\n%s", err, replayOutput)
	}
	var replay taskContinuationPlanOutput
	if err := json.Unmarshal([]byte(replayOutput), &replay); err != nil {
		t.Fatalf("decode replay output: %v\n%s", err, replayOutput)
	}
	if replay.Plan.PlanID != planned.Plan.PlanID || replay.Fingerprint != planned.Fingerprint || !replay.Plan.ExpiresAt.Equal(planned.Plan.ExpiresAt) {
		t.Fatalf("idempotent plan replay = %+v, want %+v", replay, planned)
	}
	verifyPlan := openCommandLifecycle(t, root)
	frozen, err := verifyPlan.Store().GetFrozenPlan(ctx, planned.Plan.PlanID)
	if err != nil || frozen == nil {
		_ = verifyPlan.Store().Close()
		t.Fatalf("load persisted continuation plan = %+v, %v", frozen, err)
	}
	commandRecord, err := verifyPlan.Store().GetContinuationCommand(ctx, frozen.CommandID)
	if err != nil || commandRecord == nil {
		_ = verifyPlan.Store().Close()
		t.Fatalf("load persisted continuation command = %+v, %v", commandRecord, err)
	}
	if commandRecord.Actor != actor || commandRecord.Reason != "recompute quality group" || commandRecord.CommandKey != "continue-cli-plan" {
		_ = verifyPlan.Store().Close()
		t.Fatalf("persisted command audit identity = %+v", commandRecord)
	}
	if err := verifyPlan.Store().Close(); err != nil {
		t.Fatal(err)
	}

	executionOutput, err := executeTaskCommand(t, ctx, config, []string{"continue", "--plan", planned.Plan.PlanID, "--yes"})
	if err != nil {
		t.Fatalf("task continue execute: %v\n%s", err, executionOutput)
	}
	var execution store.ContinuationExecution
	if err := json.Unmarshal([]byte(executionOutput), &execution); err != nil {
		t.Fatalf("decode continuation execution: %v\n%s", err, executionOutput)
	}
	if execution.ID == "" || execution.PlanID != planned.Plan.PlanID || execution.State != store.ContinuationExecutionQueued {
		t.Fatalf("continuation execution = %+v", execution)
	}
	replayedExecutionOutput, err := executeTaskCommand(t, ctx, config, []string{"continue", "--plan", planned.Plan.PlanID, "--yes"})
	if err != nil {
		t.Fatalf("task continue execute replay: %v\n%s", err, replayedExecutionOutput)
	}
	var replayedExecution store.ContinuationExecution
	if err := json.Unmarshal([]byte(replayedExecutionOutput), &replayedExecution); err != nil {
		t.Fatalf("decode continuation execution replay: %v\n%s", err, replayedExecutionOutput)
	}
	if replayedExecution.ID != execution.ID || replayedExecution.Version != execution.Version {
		t.Fatalf("continuation execution replay = %+v, want %+v", replayedExecution, execution)
	}
	verifyExecution := openCommandLifecycle(t, root)
	updatedRun, err := verifyExecution.Runs.Get(ctx, run.ID)
	if err != nil {
		_ = verifyExecution.Store().Close()
		t.Fatal(err)
	}
	if updatedRun.ExecutionEpoch != run.ExecutionEpoch+1 {
		_ = verifyExecution.Store().Close()
		t.Fatalf("executed continuation epoch = %d, want %d", updatedRun.ExecutionEpoch, run.ExecutionEpoch+1)
	}
	if err := verifyExecution.Store().Close(); err != nil {
		t.Fatal(err)
	}
}

func executeTaskCommand(t *testing.T, ctx context.Context, config *lifecycleCLIConfig, args []string) (string, error) {
	t.Helper()
	command := newTaskCommand(config)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	err := command.ExecuteContext(ctx)
	return output.String(), err
}

func newCommandContinuationFixture(t *testing.T, ctx context.Context) (string, store.WorkflowRun) {
	t.Helper()
	root := t.TempDir()
	services := openCommandLifecycle(t, root)
	source := writeCommandTaskSnapshot(t, "continuation command fixture\n")
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{Slug: "continue-command", Actor: "tester", Reason: "continuation fixture"},
		SourceDirectory:        source,
	})
	if err != nil {
		_ = services.Store().Close()
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: commandContinuationProfile(), Trigger: "continuation-command-fixture", Actor: "tester", Reason: "freeze continuation fixture",
	})
	if err != nil {
		_ = services.Store().Close()
		t.Fatal(err)
	}
	workflow := commandFrozenWorkflow(t, run)
	objects, err := workflowruntime.NewArtifactObjectStore(filepath.Join(root, "objects"))
	if err != nil {
		_ = services.Store().Close()
		t.Fatal(err)
	}
	seedCommandContinuationLineage(t, ctx, services.Store(), objects, run, revision, workflow, workflowkit.StageKey(nodes.CodeEdgeLint))
	run, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "tester", Reason: "continuation fixture running"})
	if err != nil {
		_ = services.Store().Close()
		t.Fatal(err)
	}
	run, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunFailedRecoverable, Actor: "tester", Reason: "continuation fixture failed"})
	if err != nil {
		_ = services.Store().Close()
		t.Fatal(err)
	}
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}
	return root, run
}

func commandContinuationProfile() workflowadapter.ExecutionProfile {
	catalog := workflowadapter.StandardStageCatalog()
	profile := workflowadapter.ExecutionProfile{ID: "command-continuation", Version: "1", ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL, ControlGracePeriod: 30 * time.Second}
	for _, stage := range catalog.Stages {
		turns := stage.RequiredTurns
		profile.Stages = append(profile.Stages, workflowadapter.StageBudget{
			StageKey: stage.Key,
			Budget: workflowkit.ExecutionBudget{
				TurnTimeout:    time.Second,
				MaxTurns:       turns,
				AttemptTimeout: time.Duration(turns) * time.Second,
				MaxAttempts:    1,
				MaxElapsed:     time.Duration(turns) * time.Second,
			},
		})
	}
	return profile
}

func commandFrozenWorkflow(t *testing.T, run store.WorkflowRun) workflowkit.WorkflowDescriptor {
	t.Helper()
	var manifest struct {
		Resolved struct {
			Descriptor workflowkit.WorkflowDescriptor `json:"descriptor"`
		} `json:"resolved_workflow"`
	}
	if err := json.Unmarshal([]byte(run.RunManifestJSON), &manifest); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Resolved.Descriptor.Validate(); err != nil {
		t.Fatalf("invalid frozen command workflow: %v", err)
	}
	return manifest.Resolved.Descriptor
}

func seedCommandContinuationLineage(t *testing.T, ctx context.Context, dataStore *store.Store, objects *workflowruntime.ArtifactObjectStore, run store.WorkflowRun, revision store.TaskRevision, workflow workflowkit.WorkflowDescriptor, stop workflowkit.StageKey) {
	t.Helper()
	if objects == nil {
		t.Fatal("artifact object store is required")
	}
	for _, stage := range workflow.Stages {
		if stage.Key == stop {
			return
		}
		bindings := commandFixtureBindings(stage)
		fingerprint, err := workflowkit.FingerprintArtifactBindings(bindings)
		if err != nil {
			t.Fatal(err)
		}
		encodedBindings, err := json.Marshal(bindings)
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := dataStore.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
			RunID: run.ID, StageKey: string(stage.Key), StageGroup: stage.Group, Ordinal: 1,
			InputFingerprint: string(fingerprint), BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "tester", Reason: "command continuation lineage",
		})
		if err != nil {
			t.Fatal(err)
		}
		nodeAttemptID, err := store.NewUUIDv7()
		if err != nil {
			t.Fatal(err)
		}
		artifacts := make([]commandStageArtifactObject, 0, len(stage.Outputs))
		for _, output := range stage.Outputs {
			content := []byte("command-continuation:" + string(stage.Key) + ":" + output.Name)
			object, err := objects.PutBytes(ctx, content)
			if err != nil {
				t.Fatal(err)
			}
			artifacts = append(artifacts, commandStageArtifactObject{Key: output.Name, SchemaVersion: output.SchemaVersion, Digest: object.Digest, SizeBytes: object.SizeBytes})
		}
		manifestJSON, err := json.Marshal(commandStageArtifactManifest{
			Format: commandStageArtifactManifestFormat, RunID: run.ID, StageAttemptID: attempt.ID, NodeAttemptID: nodeAttemptID, StageKey: stage.Key, Artifacts: artifacts,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifestFingerprint, err := workflowkit.FingerprintBytes(commandStageArtifactManifestFormat, manifestJSON)
		if err != nil {
			t.Fatal(err)
		}
		artifactManifest, err := dataStore.CreateArtifactManifest(ctx, store.CreateArtifactManifestRequest{
			SubjectRevisionID: revision.ID, SubjectDigest: revision.TaskDigest, WorkflowFingerprint: run.DefinitionHash,
			ManifestJSON: string(manifestJSON), ManifestFingerprint: string(manifestFingerprint), IdempotencyKey: "command-continuation-manifest:" + string(stage.Key), Actor: "tester", Reason: "command continuation lineage",
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, artifact := range artifacts {
			if _, err := dataStore.CreateArtifactRef(ctx, store.CreateArtifactRefRequest{
				ManifestID: artifactManifest.ID, ArtifactKey: artifact.Key, ContentDigest: string(artifact.Digest), SchemaVersion: artifact.SchemaVersion,
				RunID: run.ID, StageKey: string(stage.Key), AttemptID: attempt.ID, TurnOrdinal: artifact.TurnOrdinal, SubjectRevisionID: revision.ID, SubjectDigest: revision.TaskDigest, WorkflowFingerprint: run.DefinitionHash,
				InputBindingsJSON: string(encodedBindings), InputFingerprint: string(fingerprint), ProducerVersion: "command-continuation-fixture.v1", IdempotencyKey: "command-continuation-artifact:" + string(stage.Key) + ":" + artifact.Key, Actor: "tester", Reason: "command continuation lineage",
			}); err != nil {
				t.Fatal(err)
			}
		}
		attempt, err = dataStore.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "tester", Reason: "command continuation lineage"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dataStore.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictPass, ArtifactManifestID: artifactManifest.ID, Actor: "tester", Reason: "command continuation lineage"}); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("frozen workflow is missing stop stage %q", stop)
}

func commandFixtureBindings(stage workflowkit.StageDescriptor) []workflowkit.ArtifactBinding {
	bindings := make([]workflowkit.ArtifactBinding, 0, len(stage.Inputs))
	for _, input := range stage.Inputs {
		bindings = append(bindings, workflowkit.ArtifactBinding{
			Name: input.Name, ArtifactID: workflowkit.ArtifactID("command-input:" + string(stage.Key) + ":" + input.Name), ContentDigest: workflowkit.SHA256Fingerprint([]byte(string(stage.Key) + ":" + input.Name)), SchemaVersion: input.SchemaVersion,
		})
	}
	return bindings
}

func assertContinuationQualityGroup(t *testing.T, snapshot workflowkit.ContinuationPlanSnapshot) {
	t.Helper()
	transitions := make(map[workflowkit.NodeID]workflowkit.NodeTransition, len(snapshot.Nodes))
	for _, transition := range snapshot.Nodes {
		transitions[transition.NodeID] = transition
	}
	for _, nodeID := range []workflowkit.NodeID{nodes.CodeEdgeLint, nodes.QualityCheck} {
		transition, found := transitions[nodeID]
		if !found || transition.Disposition != workflowkit.DispositionSchedule || transition.ToGeneration != transition.FromGeneration+1 {
			t.Fatalf("quality group transition for %q = %+v", nodeID, transition)
		}
	}
}

func snapshotCommandControlPlane(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if path == filepath.Join(root, "harbor.db-wal") || path == filepath.Join(root, "harbor.db-shm") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = content
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
