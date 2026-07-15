package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestLifecycleMutationsCreateAndSoftDeleteReplayExactlyOnce(t *testing.T) {
	ctx := context.Background()
	_, services := newLifecycleMutationTestServices(t)
	key := mustLifecycleMutationUUID(t)
	command := CreateDraftLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "tester", Reason: "create through Task Hub"},
		Slug:                         "mutation-create", Title: "Mutation Create", MetadataJSON: `{"labels":["tui"]}`,
	}
	first, err := services.Mutations.CreateDraft(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := services.Mutations.CreateDraft(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if first.TaskID == "" || first.TaskID != second.TaskID || first.OperationID != second.OperationID || first.TaskVersion != second.TaskVersion {
		t.Fatalf("create replay first=%+v second=%+v", first, second)
	}
	tasks, err := services.Tasks.List(ctx, true)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("created tasks = %+v, %v", tasks, err)
	}
	conflict := command
	conflict.Title = "different payload"
	if _, err := services.Mutations.CreateDraft(ctx, conflict); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("changed create replay = %v, want ErrIdempotencyConflict", err)
	}

	checkpoint, err := services.Mutations.CaptureCheckpoint(ctx, first.TaskID, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	deleteBase := LifecycleMutationCommandBase{IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "soft delete through Task Hub", Expected: checkpoint}
	deleted, err := services.Mutations.SoftDelete(ctx, deleteBase)
	if err != nil {
		t.Fatal(err)
	}
	deletedReplay, err := services.Mutations.SoftDelete(ctx, deleteBase)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.DeletionRecordID == "" || deleted.DeletionRecordID != deletedReplay.DeletionRecordID || deleted.TaskVersion != checkpoint.TaskVersion+1 {
		t.Fatalf("soft delete replay first=%+v second=%+v", deleted, deletedReplay)
	}
	task, err := services.Tasks.Get(ctx, first.TaskID)
	if err != nil || task.LifecycleState != store.TaskLifecycleDeleted || task.Version != deleted.TaskVersion {
		t.Fatalf("soft-deleted task = %+v, %v", task, err)
	}
	record, err := services.Store().GetDeletionRecord(ctx, deleted.DeletionRecordID)
	if err != nil || record == nil || record.State != store.DeletionCompleted || record.EntityID != task.ID {
		t.Fatalf("soft-delete record = %+v, %v", record, err)
	}
}

func TestLifecycleMutationReplayCompletedFacadeReturnsImmutableReceiptOnly(t *testing.T) {
	ctx := context.Background()
	_, services := newLifecycleMutationTestServices(t)
	command := CreateDraftLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "exercise completed replay facade",
		},
		Slug: "completed-replay-facade", MetadataJSON: `{}`,
	}
	created, err := services.Mutations.CreateDraft(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	replayed, found, err := services.Mutations.ReplayCompleted(ctx, LifecycleMutationCreateDraft, command.IdempotencyKey)
	if err != nil || !found || replayed != created {
		t.Fatalf("completed replay facade = %+v, found=%t, err=%v; want %+v", replayed, found, err, created)
	}
	if _, found, err := services.Mutations.ReplayCompleted(ctx, LifecycleMutationCreateDraft, mustLifecycleMutationUUID(t)); err != nil || found {
		t.Fatalf("missing completed replay = found=%t, err=%v", found, err)
	}
	if _, _, err := services.Mutations.ReplayCompleted(ctx, LifecycleMutationArchive, command.IdempotencyKey); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("mismatched completed replay action = %v, want ErrIdempotencyConflict", err)
	}
}

func TestLifecycleMutationCreateDraftRecoversPreparedReceiptAfterDomainCommit(t *testing.T) {
	ctx := context.Background()
	_, services := newLifecycleMutationTestServices(t)
	command := CreateDraftLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: mustLifecycleMutationUUID(t),
			Actor:          "tester",
			Reason:         "create through Task Hub",
		},
		Slug: "recover-create-draft", Title: "Recover Create Draft", MetadataJSON: `{"labels":["recovery"]}`,
	}

	// Simulate a process stop after CreateTaskV2 commits but before the V12
	// lifecycle operation receives its immutable response receipt.
	op, replay, err := services.Mutations.begin(ctx, LifecycleMutationCreateDraft, command.LifecycleMutationCommandBase, command, lifecycleOperationTargets{TaskID: mustLifecycleMutationUUID(t)})
	if err != nil || replay != nil {
		t.Fatalf("prepare create lifecycle operation: op=%+v replay=%+v err=%v", op, replay, err)
	}
	created, err := services.Tasks.CreateDraft(ctx, CreateDraftTaskRequest{
		ID: op.TaskID, Slug: command.Slug, Title: command.Title, MetadataJSON: command.MetadataJSON,
		SourceRepo: command.SourceRepo, SourceCommit: command.SourceCommit, Actor: command.Actor, Reason: command.Reason,
	})
	if err != nil {
		t.Fatal(err)
	}

	recovered, err := services.Mutations.CreateDraft(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.OperationID != op.ID || recovered.TaskID != created.ID || recovered.TaskVersion != created.Version {
		t.Fatalf("recovered create receipt = %+v, operation=%+v task=%+v", recovered, op, created)
	}
	operation, err := services.Store().GetLifecycleOperation(ctx, op.ID)
	if err != nil || operation == nil || operation.State != store.LifecycleOperationCompleted {
		t.Fatalf("recovered create operation = %+v, %v", operation, err)
	}
	replayed, err := services.Mutations.CreateDraft(ctx, command)
	if err != nil || replayed != recovered {
		t.Fatalf("completed create replay = %+v, %v; want %+v", replayed, err, recovered)
	}
	tasks, err := services.Tasks.List(ctx, true)
	if err != nil || len(tasks) != 1 || tasks[0].ID != created.ID {
		t.Fatalf("recovered create tasks = %+v, %v", tasks, err)
	}
}

func TestLifecycleMutationImportRecoversPreparedReceiptAfterDomainCommit(t *testing.T) {
	ctx := context.Background()
	_, services := newLifecycleMutationTestServices(t)
	source := writeLifecycleSnapshot(t, "import recovery instruction\n")
	command := ImportLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "import through Task Hub"},
		Slug:                         "recover-import", Title: "Recover Import", MetadataJSON: `{}`, SourcePath: source,
	}
	sourcePath, sourceDigest, err := lifecycleImportSource(source)
	if err != nil {
		t.Fatal(err)
	}
	taskID := mustLifecycleMutationUUID(t)
	revisionID := mustLifecycleMutationUUID(t)
	payload := struct {
		Command      ImportLifecycleCommand `json:"command"`
		SourcePath   string                 `json:"source_path"`
		SourceDigest string                 `json:"source_digest"`
	}{Command: command, SourcePath: sourcePath, SourceDigest: sourceDigest}
	op, replay, err := services.Mutations.begin(ctx, LifecycleMutationImport, command.LifecycleMutationCommandBase, payload, lifecycleOperationTargets{TaskID: taskID, RevisionID: revisionID})
	if err != nil || replay != nil {
		t.Fatalf("prepare import lifecycle operation: op=%+v replay=%+v err=%v", op, replay, err)
	}
	// Simulate a process stop after the atomic Task+Revision transaction but
	// before the V12 receipt is completed.
	if _, _, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{ID: op.TaskID, Slug: command.Slug, Title: command.Title, MetadataJSON: command.MetadataJSON, Actor: command.Actor, Reason: command.Reason},
		InitialRevisionID:      op.RevisionID, SourceDirectory: sourcePath,
	}); err != nil {
		t.Fatal(err)
	}
	recovered, err := services.Mutations.Import(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.OperationID != op.ID || recovered.TaskID != taskID || recovered.RevisionID != revisionID {
		t.Fatalf("recovered import receipt = %+v, operation=%+v", recovered, op)
	}
	operation, err := services.Store().GetLifecycleOperation(ctx, op.ID)
	if err != nil || operation == nil || operation.State != store.LifecycleOperationCompleted {
		t.Fatalf("recovered import operation = %+v, %v", operation, err)
	}
	all, err := services.Tasks.List(ctx, true)
	if err != nil || len(all) != 1 {
		t.Fatalf("recovered import task count = %+v, %v", all, err)
	}
}

func TestLifecycleMutationCompletedReplayDoesNotRereadMutableImportOrProfileInput(t *testing.T) {
	ctx := context.Background()
	root, services := newLifecycleMutationTestServices(t)

	importSource := writeLifecycleSnapshot(t, "completed import replay source\n")
	importCommand := ImportLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "replay completed import",
		},
		Slug: "completed-import-replay", Title: "Completed Import Replay", MetadataJSON: `{}`, SourcePath: importSource,
	}
	imported, err := services.Mutations.Import(ctx, importCommand)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(importSource); err != nil {
		t.Fatal(err)
	}
	importReplay, err := services.Mutations.Import(ctx, importCommand)
	if err != nil || importReplay != imported {
		t.Fatalf("completed import replay after source deletion = %+v, %v; want %+v", importReplay, err, imported)
	}

	profileSource := writeLifecycleSnapshot(t, "completed start replay source\n")
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "completed-start-replay", MetadataJSON: `{}`, Actor: "tester", Reason: "start fixture"},
		SourceDirectory:        profileSource,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := services.Mutations.CaptureCheckpoint(ctx, task.ID, revision.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	profilePath := writeLifecycleMutationProfile(t, root)
	executionSpecPath := writeLifecycleMutationExecutionSpec(t, root, task.ID, revision.ID, revision.TaskDigest)
	startCommand := StartRunLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "replay completed run", Expected: checkpoint,
		},
		ProfilePath: profilePath, ExecutionSpecPath: executionSpecPath, Trigger: "task_hub",
	}
	started, err := services.Mutations.StartRun(ctx, startCommand)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profilePath, []byte(`{"invalid":`), 0o600); err != nil {
		t.Fatal(err)
	}
	startReplay, err := services.Mutations.StartRun(ctx, startCommand)
	if err != nil || startReplay != started {
		t.Fatalf("completed start replay after profile change = %+v, %v; want %+v", startReplay, err, started)
	}

	mismatchedAction := StartRunLifecycleCommand{
		LifecycleMutationCommandBase: importCommand.LifecycleMutationCommandBase,
		ProfilePath:                  filepath.Join(root, "does-not-need-to-exist.json"),
		Trigger:                      "mismatch",
	}
	if _, err := services.Mutations.StartRun(ctx, mismatchedAction); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("completed key reused for another action = %v, want ErrIdempotencyConflict", err)
	}
}

func TestLifecycleMutationPreparedImportStillRejectsChangedSourceInput(t *testing.T) {
	ctx := context.Background()
	_, services := newLifecycleMutationTestServices(t)
	source := writeLifecycleSnapshot(t, "prepared import original source\n")
	command := ImportLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "prepared import fingerprint",
		},
		Slug: "prepared-import-conflict", Title: "Prepared Import Conflict", MetadataJSON: `{}`, SourcePath: source,
	}
	sourcePath, sourceDigest, err := lifecycleImportSource(source)
	if err != nil {
		t.Fatal(err)
	}
	payload := struct {
		Command      ImportLifecycleCommand `json:"command"`
		SourcePath   string                 `json:"source_path"`
		SourceDigest string                 `json:"source_digest"`
	}{Command: command, SourcePath: sourcePath, SourceDigest: sourceDigest}
	if _, replay, err := services.Mutations.begin(ctx, LifecycleMutationImport, command.LifecycleMutationCommandBase, payload, lifecycleOperationTargets{
		TaskID: mustLifecycleMutationUUID(t), RevisionID: mustLifecycleMutationUUID(t),
	}); err != nil || replay != nil {
		t.Fatalf("prepare import lifecycle operation: replay=%+v err=%v", replay, err)
	}
	if err := os.WriteFile(filepath.Join(source, "instruction.md"), []byte("prepared import changed source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := services.Mutations.Import(ctx, command); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("prepared import replay after source change = %v, want ErrIdempotencyConflict", err)
	}
}

func TestLifecycleMutationForkRecoversPreparedReceiptAfterDomainCommit(t *testing.T) {
	ctx := context.Background()
	_, services := newLifecycleMutationTestServices(t)
	sourceTask, sourceRevision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "fork-source", MetadataJSON: `{}`, Actor: "tester", Reason: "source fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "fork source instruction\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := services.Mutations.CaptureCheckpoint(ctx, sourceTask.ID, sourceRevision.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	command := ForkLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "fork through Task Hub", Expected: checkpoint},
		Slug:                         "fork-recovery", Title: "Fork Recovery", MetadataJSON: `{}`,
	}
	taskID, revisionID := mustLifecycleMutationUUID(t), mustLifecycleMutationUUID(t)
	op, replay, err := services.Mutations.begin(ctx, LifecycleMutationFork, command.LifecycleMutationCommandBase, command, lifecycleOperationTargets{TaskID: taskID, RevisionID: revisionID})
	if err != nil || replay != nil {
		t.Fatalf("prepare fork lifecycle operation: op=%+v replay=%+v err=%v", op, replay, err)
	}
	if _, _, err := services.Tasks.ForkTask(ctx, ForkTaskRequest{
		SourceTaskID: sourceTask.ID, SourceRevisionID: sourceRevision.ID, ID: op.TaskID, InitialRevisionID: op.RevisionID,
		Slug: command.Slug, Title: command.Title, MetadataJSON: command.MetadataJSON, Actor: command.Actor, Reason: command.Reason,
	}); err != nil {
		t.Fatal(err)
	}
	recovered, err := services.Mutations.Fork(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.OperationID != op.ID || recovered.TaskID != taskID || recovered.RevisionID != revisionID {
		t.Fatalf("recovered fork receipt = %+v, operation=%+v", recovered, op)
	}
	all, err := services.Tasks.List(ctx, true)
	if err != nil || len(all) != 2 {
		t.Fatalf("recovered fork task count = %+v, %v", all, err)
	}
}

func TestLifecycleMutationStartRunFreezesExplicitInputsBeforeExecution(t *testing.T) {
	ctx := context.Background()
	root, services := newLifecycleMutationTestServices(t)
	source := writeLifecycleSnapshot(t, "start recovery instruction\n")
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "start-recovery", MetadataJSON: `{}`, Actor: "tester", Reason: "fixture"},
		SourceDirectory:        source,
	})
	if err != nil {
		t.Fatal(err)
	}
	profilePath := writeLifecycleMutationProfile(t, root)
	executionSpecPath := writeLifecycleMutationExecutionSpec(t, root, task.ID, revision.ID, revision.TaskDigest)
	checkpoint, err := services.Mutations.CaptureCheckpoint(ctx, task.ID, revision.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	command := StartRunLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "start through explicit profile", Expected: checkpoint},
		ProfilePath:                  profilePath, ExecutionSpecPath: executionSpecPath, Trigger: "task_hub",
	}
	prepared, err := services.Mutations.PrepareStartRun(ctx, command)
	if err != nil {
		t.Fatalf("prepare frozen StartRun inputs: %v", err)
	}
	if prepared.InputBundleID != command.IdempotencyKey || prepared.ProfileFingerprint == "" || prepared.ExecutionSpecFingerprint == "" {
		t.Fatalf("prepared StartRun inputs = %+v", prepared)
	}
	bundleDirectory := filepath.Join(root, managedRunInputsDirectory, command.IdempotencyKey)
	for _, name := range []string{runStartInputProfileFileName, runStartInputSpecFileName, runStartInputManifestFileName} {
		if _, err := os.Stat(filepath.Join(bundleDirectory, name)); err != nil {
			t.Fatalf("frozen StartRun input %s: %v", name, err)
		}
	}
	if err := os.Remove(profilePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(executionSpecPath); err != nil {
		t.Fatal(err)
	}
	started, err := services.Mutations.StartRun(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if started.RunID == "" || started.OperationID == "" {
		t.Fatalf("started frozen Run receipt = %+v", started)
	}
	runs, err := services.Runs.ListForTask(ctx, task.ID)
	if err != nil || len(runs) != 1 || runs[0].ResolvedProfileHash != prepared.ProfileFingerprint {
		t.Fatalf("frozen start run = %+v, %v", runs, err)
	}
	manifestRaw, err := os.ReadFile(filepath.Join(root, managedRunsDirectory, started.RunID, "run-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest runManifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Inputs == nil || manifest.Inputs.BundleID != command.IdempotencyKey || manifest.Inputs.ProfileFingerprint != workflowkit.Fingerprint(prepared.ProfileFingerprint) || manifest.Inputs.ExecutionSpecFingerprint != workflowkit.Fingerprint(prepared.ExecutionSpecFingerprint) {
		t.Fatalf("Run manifest frozen input reference = %+v", manifest.Inputs)
	}
	if _, err := workflowadapter.ParseRunExecutionSpecJSON(manifest.ExecutionSpec); err != nil {
		t.Fatalf("manifest execution specification: %v", err)
	}
	replayed, err := services.Mutations.StartRun(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if replayed != started {
		t.Fatalf("StartRun replay = %+v, want %+v", replayed, started)
	}
}

func TestStartRunRejectsUnconfiguredOperationResolverBeforePersistentMutation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	services, err := NewLifecycleServices(root, database)
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "missing-operation-resolver", Actor: "tester", Reason: "fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "unconfigured operation resolver\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := services.Mutations.CaptureCheckpoint(ctx, task.ID, revision.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	key := mustLifecycleMutationUUID(t)
	command := StartRunLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "tester", Reason: "verify preflight", Expected: checkpoint},
		ProfilePath:                  writeLifecycleMutationProfile(t, root),
		ExecutionSpecPath:            writeLifecycleMutationExecutionSpec(t, root, task.ID, revision.ID, revision.TaskDigest),
		Trigger:                      "task_hub",
	}
	if _, err := services.Mutations.PrepareStartRun(ctx, command); err == nil || !strings.Contains(err.Error(), "controlled stage operation resolver is not configured") {
		t.Fatalf("unconfigured resolver StartRun prepare = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, managedRunInputsDirectory, key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unconfigured resolver created frozen input bundle: %v", err)
	}
	if operation, err := database.GetLifecycleOperationByIdempotencyKey(ctx, key); err != nil || operation != nil {
		t.Fatalf("unconfigured resolver created lifecycle operation: %+v, %v", operation, err)
	}
	if _, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCompleteProfile(t), ExecutionSpec: lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest),
		Trigger: "direct", Actor: "tester", Reason: "verify direct preflight",
	}); err == nil || !strings.Contains(err.Error(), "controlled stage operation resolver is not configured") {
		t.Fatalf("unconfigured resolver direct StartRun = %v", err)
	}
	runs, err := services.Runs.ListForTask(ctx, task.ID)
	if err != nil || len(runs) != 0 {
		t.Fatalf("unconfigured resolver created runs: %+v, %v", runs, err)
	}
}

func TestLifecycleMutationPackageAndWithdrawReplayExistingDurableOperations(t *testing.T) {
	ctx := context.Background()
	_, services := newLifecycleMutationTestServices(t)
	task, revision := createPackageableLifecycleMutationTask(t, ctx, services)
	checkpoint, err := services.Mutations.CaptureCheckpoint(ctx, task.ID, revision.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	packageCommand := PackageLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "build local package", Expected: checkpoint},
		ReleaseVersion:               "mutation-v1",
	}
	packaged, err := services.Mutations.Package(ctx, packageCommand)
	if err != nil {
		t.Fatal(err)
	}
	packagedReplay, err := services.Mutations.Package(ctx, packageCommand)
	if err != nil {
		t.Fatal(err)
	}
	if packaged.ReleaseID == "" || packaged.ReleaseID != packagedReplay.ReleaseID || packaged.OperationID != packagedReplay.OperationID {
		t.Fatalf("package replay first=%+v second=%+v", packaged, packagedReplay)
	}
	release, err := services.Store().GetLocalPackageRelease(ctx, packaged.ReleaseID)
	if err != nil || release == nil || release.WithdrawnAt != nil {
		t.Fatalf("packaged release = %+v, %v", release, err)
	}
	withdrawCheckpoint, err := services.Mutations.CaptureCheckpoint(ctx, task.ID, revision.ID, "", release.ID)
	if err != nil {
		t.Fatal(err)
	}
	withdrawBase := LifecycleMutationCommandBase{IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "withdraw local package", Expected: withdrawCheckpoint}
	withdrawn, err := services.Mutations.Withdraw(ctx, withdrawBase)
	if err != nil {
		t.Fatal(err)
	}
	withdrawnReplay, err := services.Mutations.Withdraw(ctx, withdrawBase)
	if err != nil {
		t.Fatal(err)
	}
	if withdrawn.ReleaseID != release.ID || withdrawn.ExecutionID == "" || withdrawn.ExecutionID != withdrawnReplay.ExecutionID {
		t.Fatalf("withdraw replay first=%+v second=%+v", withdrawn, withdrawnReplay)
	}
	updatedRelease, err := services.Store().GetLocalPackageRelease(ctx, release.ID)
	if err != nil || updatedRelease == nil || updatedRelease.WithdrawnAt == nil {
		t.Fatalf("withdrawn release = %+v, %v", updatedRelease, err)
	}
}

func TestLifecycleMutationPackageAndWithdrawRecoverPreparedReceiptsAfterDomainCommit(t *testing.T) {
	ctx := context.Background()
	_, services := newLifecycleMutationTestServices(t)
	task, revision := createPackageableLifecycleMutationTask(t, ctx, services)
	packageCheckpoint, err := services.Mutations.CaptureCheckpoint(ctx, task.ID, revision.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	packageCommand := PackageLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: mustLifecycleMutationUUID(t),
			Actor:          "tester",
			Reason:         "recover local package receipt",
			Expected:       packageCheckpoint,
		},
		ReleaseVersion: "mutation-recovery-v1",
	}
	packageOp, replay, err := services.Mutations.begin(ctx, LifecycleMutationPackage, packageCommand.LifecycleMutationCommandBase, packageCommand, lifecycleOperationTargets{
		TaskID: task.ID, RevisionID: revision.ID, ReleaseID: packageCommand.IdempotencyKey,
	})
	if err != nil || replay != nil {
		t.Fatalf("prepare package lifecycle operation: op=%+v replay=%+v err=%v", packageOp, replay, err)
	}
	// Simulate a stop after the release record and immutable package receipt are
	// committed, but before the outer V12 lifecycle receipt is completed.
	packaged, err := services.Releases.PackageRevision(ctx, PackageRevisionRequest{
		RevisionID: revision.ID, ExpectedStateVersion: packageCheckpoint.RevisionStateVersion,
		ReleaseVersion: packageCommand.ReleaseVersion, IdempotencyKey: packageOp.IdempotencyKey,
		Actor: packageCommand.Actor, Reason: packageCommand.Reason,
	})
	if err != nil {
		t.Fatal(err)
	}
	recoveredPackage, err := services.Mutations.Package(ctx, packageCommand)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredPackage.OperationID != packageOp.ID || recoveredPackage.ReleaseID != packaged.Release.ID || recoveredPackage.ReleaseVersion != packaged.Release.ReleaseVersion {
		t.Fatalf("recovered package receipt = %+v, operation=%+v release=%+v", recoveredPackage, packageOp, packaged.Release)
	}
	packageOperation, err := services.Store().GetLifecycleOperation(ctx, packageOp.ID)
	if err != nil || packageOperation == nil || packageOperation.State != store.LifecycleOperationCompleted {
		t.Fatalf("recovered package operation = %+v, %v", packageOperation, err)
	}

	withdrawCheckpoint, err := services.Mutations.CaptureCheckpoint(ctx, task.ID, revision.ID, "", packaged.Release.ID)
	if err != nil {
		t.Fatal(err)
	}
	withdrawBase := LifecycleMutationCommandBase{
		IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "recover withdraw receipt", Expected: withdrawCheckpoint,
	}
	withdrawOp, replay, err := services.Mutations.begin(ctx, LifecycleMutationWithdraw, withdrawBase, withdrawBase, lifecycleOperationTargets{
		TaskID: task.ID, RevisionID: revision.ID, ReleaseID: packaged.Release.ID,
	})
	if err != nil || replay != nil {
		t.Fatalf("prepare withdraw lifecycle operation: op=%+v replay=%+v err=%v", withdrawOp, replay, err)
	}
	withdrawn, err := services.Releases.Withdraw(ctx, WithdrawReleaseRequest{
		ReleaseID: packaged.Release.ID, ExpectedReleaseVersion: withdrawCheckpoint.ReleaseRecordVersion,
		IdempotencyKey: withdrawOp.IdempotencyKey, Actor: withdrawBase.Actor, Reason: withdrawBase.Reason,
	})
	if err != nil {
		t.Fatal(err)
	}
	recoveredWithdraw, err := services.Mutations.Withdraw(ctx, withdrawBase)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredWithdraw.OperationID != withdrawOp.ID || recoveredWithdraw.ReleaseID != packaged.Release.ID || recoveredWithdraw.ExecutionID != withdrawn.Operation.ID {
		t.Fatalf("recovered withdraw receipt = %+v, operation=%+v withdrawal=%+v", recoveredWithdraw, withdrawOp, withdrawn.Operation)
	}
	withdrawOperation, err := services.Store().GetLifecycleOperation(ctx, withdrawOp.ID)
	if err != nil || withdrawOperation == nil || withdrawOperation.State != store.LifecycleOperationCompleted {
		t.Fatalf("recovered withdraw operation = %+v, %v", withdrawOperation, err)
	}
	releases, err := services.Releases.List(ctx, task.ID)
	if err != nil || len(releases) != 1 || releases[0].ID != packaged.Release.ID || releases[0].WithdrawnAt == nil {
		t.Fatalf("recovered package/withdraw releases = %+v, %v", releases, err)
	}
}

func TestLifecycleMutationReviewUsesFullCheckpointAndRecoversAfterDecisionCommit(t *testing.T) {
	ctx := context.Background()
	_, services := newLifecycleMutationTestServices(t)
	task, revision := createPackageableLifecycleMutationTask(t, ctx, services)
	review, err := services.Reviews.Request(ctx, revision.ID, revision.ValidationEvidenceManifest, "tester", "request mutation review")
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := services.Mutations.CaptureReviewCheckpoint(ctx, task.ID, revision.ID, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.ReviewRequestID != review.ID || checkpoint.ReviewRevisionID != revision.ID || checkpoint.ReviewState != "open" || checkpoint.ReviewEvidenceDigest != revision.ValidationEvidenceManifest {
		t.Fatalf("review checkpoint = %+v", checkpoint)
	}
	command := DecideReviewLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "approve through typed mutation", Expected: checkpoint,
		},
		Decision: store.ReviewDecisionApprove,
	}
	first, err := services.Mutations.DecideReview(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := services.Mutations.DecideReview(ctx, command)
	if err != nil || second != first || first.ReviewRequestID != review.ID || first.ReviewDecisionID == "" || first.ReviewDecision != string(store.ReviewDecisionApprove) {
		t.Fatalf("typed review replay first=%+v second=%+v err=%v", first, second, err)
	}
	decisions, err := services.Store().ListReviewDecisionsForRequest(ctx, review.ID)
	if err != nil || len(decisions) != 1 || decisions[0].ID != first.ReviewDecisionID {
		t.Fatalf("typed review decisions = %+v, %v", decisions, err)
	}

	crashReview, err := services.Reviews.Request(ctx, revision.ID, revision.ValidationEvidenceManifest, "tester", "request crash recovery review")
	if err != nil {
		t.Fatal(err)
	}
	crashCheckpoint, err := services.Mutations.CaptureReviewCheckpoint(ctx, task.ID, revision.ID, crashReview.ID)
	if err != nil {
		t.Fatal(err)
	}
	crashCommand := DecideReviewLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "recover review receipt", Expected: crashCheckpoint,
		},
		Decision: store.ReviewDecisionRequestChanges,
	}
	op, replay, err := services.Mutations.begin(ctx, LifecycleMutationReview, crashCommand.LifecycleMutationCommandBase, crashCommand, lifecycleOperationTargets{
		TaskID: task.ID, RevisionID: revision.ID,
	})
	if err != nil || replay != nil {
		t.Fatalf("prepare review lifecycle operation: op=%+v replay=%+v err=%v", op, replay, err)
	}
	durableDecision, err := services.Reviews.Decide(ctx, DecideReviewRequest{
		ID: op.IdempotencyKey, ReviewRequestID: crashReview.ID, RevisionID: revision.ID, Action: crashCommand.Decision,
		ExpectedRevisionDigest: revision.TaskDigest, Actor: crashCommand.Actor, Reason: crashCommand.Reason,
	})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := services.Mutations.DecideReview(ctx, crashCommand)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.OperationID != op.ID || recovered.ReviewRequestID != crashReview.ID || recovered.ReviewDecisionID != durableDecision.ID || recovered.ReviewDecision != string(crashCommand.Decision) {
		t.Fatalf("recovered review receipt = %+v, operation=%+v decision=%+v", recovered, op, durableDecision)
	}
	operation, err := services.Store().GetLifecycleOperation(ctx, op.ID)
	if err != nil || operation == nil || operation.State != store.LifecycleOperationCompleted {
		t.Fatalf("recovered review operation = %+v, %v", operation, err)
	}

	staleReview, err := services.Reviews.Request(ctx, revision.ID, revision.ValidationEvidenceManifest, "tester", "request stale review")
	if err != nil {
		t.Fatal(err)
	}
	staleCheckpoint, err := services.Mutations.CaptureReviewCheckpoint(ctx, task.ID, revision.ID, staleReview.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Reviews.Decide(ctx, DecideReviewRequest{
		ID: mustLifecycleMutationUUID(t), ReviewRequestID: staleReview.ID, RevisionID: revision.ID,
		Action: store.ReviewDecisionRejectTerminal, ExpectedRevisionDigest: revision.TaskDigest, Actor: "tester", Reason: "close stale review",
	}); err != nil {
		t.Fatal(err)
	}
	staleCommand := DecideReviewLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "stale review decision", Expected: staleCheckpoint,
		},
		Decision: store.ReviewDecisionApprove,
	}
	if _, err := services.Mutations.DecideReview(ctx, staleCommand); !errors.Is(err, store.ErrOptimisticLock) {
		t.Fatalf("stale review checkpoint decision = %v, want ErrOptimisticLock", err)
	}
	staleDecisions, err := services.Store().ListReviewDecisionsForRequest(ctx, staleReview.ID)
	if err != nil || len(staleDecisions) != 1 {
		t.Fatalf("stale review mutation created another decision: %+v, %v", staleDecisions, err)
	}
}

func newLifecycleMutationTestServices(t *testing.T) (string, *LifecycleServices) {
	t.Helper()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	services, err := newLifecycleServicesForTest(root, database)
	if err != nil {
		t.Fatal(err)
	}
	return root, services
}

func mustLifecycleMutationUUID(t *testing.T) string {
	t.Helper()
	id, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func writeLifecycleMutationProfile(t *testing.T, root string) string {
	t.Helper()
	catalog := workflowadapter.StandardStageCatalog()
	template := workflowadapter.StandardTemplateReference()
	document := map[string]any{
		"template":                  map[string]any{"id": template.ID, "version": template.Version},
		"id":                        "task-hub-explicit",
		"version":                   "1",
		"continuation_plan_ttl":     "24h",
		"control_grace_period":      "30s",
		"candidate_provider_budget": map[string]any{"attempt_timeout": "10s", "startup_grace": "0s", "shutdown_grace": "0s"},
		"stages":                    make([]any, 0, len(catalog.Stages)),
	}
	stages := document["stages"].([]any)
	for _, stage := range catalog.Stages {
		stages = append(stages, map[string]any{
			"stage_key": string(stage.Key),
			"budget": map[string]any{
				"turn_timeout": "1s", "max_turns": stage.RequiredTurns, "attempt_timeout": fmt.Sprintf("%ds", stage.RequiredTurns),
				"max_attempts": 1, "max_elapsed": fmt.Sprintf("%ds", stage.RequiredTurns), "idle_timeout": "0s", "startup_grace": "0s", "shutdown_grace": "0s",
				"backoff": map[string]any{"retry_delays": []any{}},
			},
		})
	}
	document["stages"] = stages
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "explicit-profile.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeLifecycleMutationExecutionSpec(t *testing.T, root, taskID, revisionID, revisionDigest string) string {
	t.Helper()
	raw, err := json.Marshal(lifecycleExecutionSpec(taskID, revisionID, revisionDigest))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "explicit-execution-spec.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func createPackageableLifecycleMutationTask(t *testing.T, ctx context.Context, services *LifecycleServices) (store.TaskV2, store.TaskRevision) {
	t.Helper()
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "mutation-package", MetadataJSON: `{}`, Actor: "tester", Reason: "import package fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "package mutation instruction\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	revision, err = services.Revisions.MarkValidated(ctx, revision.ID, revision.StateVersion, "sha256:mutation-package-evidence", "tester", "validate package fixture")
	if err != nil {
		t.Fatal(err)
	}
	review, err := services.Reviews.Request(ctx, revision.ID, revision.ValidationEvidenceManifest, "tester", "request package review")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Reviews.Decide(ctx, DecideReviewRequest{
		ReviewRequestID: review.ID, RevisionID: revision.ID, Action: store.ReviewDecisionApprove, ExpectedRevisionDigest: revision.TaskDigest, Actor: "tester", Reason: "approve package fixture",
	}); err != nil {
		t.Fatal(err)
	}
	task, err = services.Reviews.PromoteCurrent(ctx, task.ID, revision.ID, task.Version, "tester", "promote package fixture")
	if err != nil {
		t.Fatal(err)
	}
	return task, revision
}
