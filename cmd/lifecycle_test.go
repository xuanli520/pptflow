package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const commandStageArtifactManifestFormat = "harbor.v2.stage-artifact-manifest.v1"

func commandExecutionSpec(taskID, revisionID, revisionDigest string) workflowadapter.RunExecutionSpec {
	return testsupport.CompleteRunExecutionSpec(taskID, revisionID, revisionDigest)
}

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
	idempotencyKey := commandLifecycleUUID(t)
	command := newTaskCommand(config)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	args := []string{
		"import",
		"--source", source,
		"--slug", "command-task",
		"--repo", "https://example.invalid/repository",
		"--commit", "deadbeef",
		"--idempotency-key", idempotencyKey,
		"--reason", "import a controlled fixture",
	}
	command.SetArgs(args)
	if err := command.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("task import command: %v\n%s", err, output.String())
	}
	var result app.LifecycleMutationReceipt
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode command output: %v\n%s", err, output.String())
	}
	if result.Action != app.LifecycleMutationImport || result.OperationID == "" || result.TaskID == "" || result.RevisionID == "" {
		t.Fatalf("unexpected import result: %+v", result)
	}
	services := openCommandLifecycle(t, root)
	operation, err := services.Store().GetLifecycleOperationByIdempotencyKey(context.Background(), idempotencyKey)
	if err != nil || operation == nil || operation.ID != result.OperationID || operation.State != store.LifecycleOperationCompleted || operation.Action != string(app.LifecycleMutationImport) {
		t.Fatalf("import lifecycle operation = %+v, %v", operation, err)
	}
	snapshot, err := services.Revisions.SnapshotDirectory(result.TaskID, result.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(snapshot, "instruction.md")); err != nil {
		t.Fatalf("managed snapshot missing instruction: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "harbor.db")); err != nil {
		t.Fatalf("V2 command did not initialize SQLite control plane: %v", err)
	}
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}

	replayCommand := newTaskCommand(config)
	var replayOutput bytes.Buffer
	replayCommand.SetOut(&replayOutput)
	replayCommand.SetErr(&replayOutput)
	replayCommand.SetArgs(args)
	if err := replayCommand.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("replay imported command after source removal: %v\n%s", err, replayOutput.String())
	}
	var replay app.LifecycleMutationReceipt
	if err := json.Unmarshal(replayOutput.Bytes(), &replay); err != nil {
		t.Fatalf("decode replay import output: %v\n%s", err, replayOutput.String())
	}
	if replay != result {
		t.Fatalf("import receipt replay = %+v, want %+v", replay, result)
	}
}

func TestTaskArchiveCommandUsesV12ReceiptAndRejectsStaleCheckpoint(t *testing.T) {
	ctx := context.Background()
	actor := defaultLifecycleActor()
	if actor == "" {
		t.Skip("local OS actor is unavailable in this test environment")
	}
	root := t.TempDir()
	services := openCommandLifecycle(t, root)
	task, err := services.Tasks.CreateDraft(ctx, app.CreateDraftTaskRequest{
		Slug: "archive-receipt", Actor: actor, Reason: "create archive lifecycle fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	task, err = services.Store().UpdateTaskV2(ctx, store.UpdateTaskV2Request{
		TaskID: task.ID, ExpectedVersion: task.Version, LifecycleState: store.TaskLifecycleReady, Actor: actor, Reason: "ready archive lifecycle fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	task, err = services.Store().UpdateTaskV2(ctx, store.UpdateTaskV2Request{
		TaskID: task.ID, ExpectedVersion: task.Version, LifecycleState: store.TaskLifecyclePublished, Actor: actor, Reason: "publish archive lifecycle fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	staleTask, err := services.Tasks.CreateDraft(ctx, app.CreateDraftTaskRequest{
		Slug: "archive-stale", Actor: actor, Reason: "create stale archive lifecycle fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	staleTask, err = services.Store().UpdateTaskV2(ctx, store.UpdateTaskV2Request{
		TaskID: staleTask.ID, ExpectedVersion: staleTask.Version, LifecycleState: store.TaskLifecycleReady, Actor: actor, Reason: "ready stale archive lifecycle fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	staleTask, err = services.Store().UpdateTaskV2(ctx, store.UpdateTaskV2Request{
		TaskID: staleTask.ID, ExpectedVersion: staleTask.Version, LifecycleState: store.TaskLifecyclePublished, Actor: actor, Reason: "publish stale archive lifecycle fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	if _, err := services.Tasks.Update(ctx, app.UpdateTaskRequest{
		TaskID: staleTask.ID, ExpectedVersion: staleTask.Version, Title: "newer fixture state", Actor: actor, Reason: "make archive checkpoint stale",
	}); err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}

	config := &lifecycleCLIConfig{root: root}
	key := commandLifecycleUUID(t)
	args := []string{
		"archive", "--task", task.ID, "--expected-version", fmt.Sprint(task.Version),
		"--idempotency-key", key, "--reason", "archive through lifecycle CLI",
	}
	output, err := executeTaskCommand(t, ctx, config, args)
	if err != nil {
		t.Fatalf("archive command: %v\n%s", err, output)
	}
	var first app.LifecycleMutationReceipt
	if err := json.Unmarshal([]byte(output), &first); err != nil {
		t.Fatalf("decode archive receipt: %v\n%s", err, output)
	}
	if first.Action != app.LifecycleMutationArchive || first.OperationID == "" || first.TaskID != task.ID || first.TaskVersion != task.Version+1 {
		t.Fatalf("archive receipt = %+v", first)
	}
	replayOutput, err := executeTaskCommand(t, ctx, config, args)
	if err != nil {
		t.Fatalf("archive replay: %v\n%s", err, replayOutput)
	}
	var replay app.LifecycleMutationReceipt
	if err := json.Unmarshal([]byte(replayOutput), &replay); err != nil {
		t.Fatalf("decode archive replay: %v\n%s", err, replayOutput)
	}
	if replay != first {
		t.Fatalf("archive receipt replay = %+v, want %+v", replay, first)
	}

	check := openCommandLifecycle(t, root)
	operation, err := check.Store().GetLifecycleOperationByIdempotencyKey(ctx, key)
	if err != nil || operation == nil || operation.ID != first.OperationID || operation.Action != string(app.LifecycleMutationArchive) || operation.State != store.LifecycleOperationCompleted {
		check.Store().Close()
		t.Fatalf("archive lifecycle operation = %+v, %v", operation, err)
	}
	if err := check.Store().Close(); err != nil {
		t.Fatal(err)
	}

	staleKey := commandLifecycleUUID(t)
	staleOutput, err := executeTaskCommand(t, ctx, config, []string{
		"archive", "--task", staleTask.ID, "--expected-version", fmt.Sprint(staleTask.Version),
		"--idempotency-key", staleKey, "--reason", "archive stale lifecycle checkpoint",
	})
	if !errors.Is(err, store.ErrOptimisticLock) {
		t.Fatalf("stale archive command = %v, output=%s; want ErrOptimisticLock", err, staleOutput)
	}
	check = openCommandLifecycle(t, root)
	defer check.Store().Close()
	staleOperation, err := check.Store().GetLifecycleOperationByIdempotencyKey(ctx, staleKey)
	if err != nil || staleOperation != nil {
		t.Fatalf("stale archive unexpectedly created lifecycle receipt = %+v, %v", staleOperation, err)
	}
}

func TestTaskCreateForkDeleteAndRestoreCommandsReplayV12Receipts(t *testing.T) {
	ctx := context.Background()
	actor := defaultLifecycleActor()
	if actor == "" {
		t.Skip("local OS actor is unavailable in this test environment")
	}
	root := t.TempDir()
	config := &lifecycleCLIConfig{root: root}

	createKey := commandLifecycleUUID(t)
	createArgs := []string{
		"create", "--slug", "command-create-replay", "--title", "CLI create replay",
		"--idempotency-key", createKey, "--reason", "create through lifecycle CLI",
	}
	created := executeTaskMutationReceipt(t, ctx, config, createArgs)
	if created.Action != app.LifecycleMutationCreateDraft || created.OperationID == "" || created.TaskID == "" || created.TaskVersion <= 0 {
		t.Fatalf("create receipt = %+v", created)
	}
	if replay := executeTaskMutationReceipt(t, ctx, config, createArgs); replay != created {
		t.Fatalf("create replay = %+v, want %+v", replay, created)
	}

	deleteKey := commandLifecycleUUID(t)
	deleteArgs := []string{
		"delete", "--task", created.TaskID, "--expected-version", fmt.Sprint(created.TaskVersion),
		"--idempotency-key", deleteKey, "--reason", "delete through lifecycle CLI",
	}
	deleted := executeTaskMutationReceipt(t, ctx, config, deleteArgs)
	if deleted.Action != app.LifecycleMutationSoftDelete || deleted.OperationID == "" || deleted.DeletionRecordID == "" || deleted.TaskVersion != created.TaskVersion+1 {
		t.Fatalf("delete receipt = %+v", deleted)
	}
	if replay := executeTaskMutationReceipt(t, ctx, config, deleteArgs); replay != deleted {
		t.Fatalf("delete replay = %+v, want %+v", replay, deleted)
	}

	restoreKey := commandLifecycleUUID(t)
	restoreArgs := []string{
		"restore", "--task", created.TaskID, "--state", string(store.TaskLifecycleDraft), "--expected-version", fmt.Sprint(deleted.TaskVersion),
		"--idempotency-key", restoreKey, "--reason", "restore through lifecycle CLI",
	}
	restored := executeTaskMutationReceipt(t, ctx, config, restoreArgs)
	if restored.Action != app.LifecycleMutationRestore || restored.OperationID == "" || restored.TaskID != created.TaskID || restored.TaskVersion != deleted.TaskVersion+1 {
		t.Fatalf("restore receipt = %+v", restored)
	}
	if replay := executeTaskMutationReceipt(t, ctx, config, restoreArgs); replay != restored {
		t.Fatalf("restore replay = %+v, want %+v", replay, restored)
	}

	services := openCommandLifecycle(t, root)
	sourceTask, sourceRevision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{Slug: "command-fork-source", Actor: actor, Reason: "create fork command fixture"},
		SourceDirectory:        writeCommandTaskSnapshot(t, "fork command fixture\n"),
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}
	forkKey := commandLifecycleUUID(t)
	forkArgs := []string{
		"fork", "--source-task", sourceTask.ID, "--source-revision", sourceRevision.ID, "--slug", "command-fork-replay",
		"--idempotency-key", forkKey, "--reason", "fork through lifecycle CLI",
	}
	forked := executeTaskMutationReceipt(t, ctx, config, forkArgs)
	if forked.Action != app.LifecycleMutationFork || forked.OperationID == "" || forked.TaskID == "" || forked.RevisionID == "" || forked.TaskID == sourceTask.ID {
		t.Fatalf("fork receipt = %+v", forked)
	}
	if replay := executeTaskMutationReceipt(t, ctx, config, forkArgs); replay != forked {
		t.Fatalf("fork replay = %+v, want %+v", replay, forked)
	}

	check := openCommandLifecycle(t, root)
	defer check.Store().Close()
	for key, action := range map[string]app.LifecycleMutationAction{
		createKey:  app.LifecycleMutationCreateDraft,
		deleteKey:  app.LifecycleMutationSoftDelete,
		restoreKey: app.LifecycleMutationRestore,
		forkKey:    app.LifecycleMutationFork,
	} {
		operation, err := check.Store().GetLifecycleOperationByIdempotencyKey(ctx, key)
		if err != nil || operation == nil || operation.Action != string(action) || operation.State != store.LifecycleOperationCompleted {
			t.Fatalf("%s lifecycle operation = %+v, %v", action, operation, err)
		}
	}
}

func executeTaskMutationReceipt(t *testing.T, ctx context.Context, config *lifecycleCLIConfig, args []string) app.LifecycleMutationReceipt {
	t.Helper()
	output, err := executeTaskCommand(t, ctx, config, args)
	if err != nil {
		t.Fatalf("task command %q: %v\n%s", args, err, output)
	}
	var receipt app.LifecycleMutationReceipt
	if err := json.Unmarshal([]byte(output), &receipt); err != nil {
		t.Fatalf("decode task receipt for %q: %v\n%s", args, err, output)
	}
	return receipt
}

func TestReviewDecideCommandReplaysV12ReceiptAndRejectsClosedSelection(t *testing.T) {
	ctx := context.Background()
	actor := defaultLifecycleActor()
	if actor == "" {
		t.Skip("local OS actor is unavailable in this test environment")
	}
	root := t.TempDir()
	services := openCommandLifecycle(t, root)
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{Slug: "command-review-receipt", Actor: actor, Reason: "import review command fixture"},
		SourceDirectory:        writeCommandTaskSnapshot(t, "review command fixture\n"),
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	revision, err = services.Revisions.MarkValidated(ctx, revision.ID, revision.StateVersion, "sha256:command-review-evidence", actor, "validate review command fixture")
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	review, err := services.Reviews.Request(ctx, revision.ID, revision.ValidationEvidenceManifest, actor, "request review command fixture")
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}

	config := &lifecycleCLIConfig{root: root}
	key := commandLifecycleUUID(t)
	args := []string{
		"decide", "--request", review.ID, "--revision", revision.ID, "--action", string(store.ReviewDecisionApprove),
		"--expected-digest", revision.TaskDigest, "--idempotency-key", key, "--reason", "approve through lifecycle CLI",
	}
	first := executeReviewMutationReceipt(t, ctx, config, args)
	if first.Action != app.LifecycleMutationReview || first.OperationID == "" || first.TaskID != task.ID || first.RevisionID != revision.ID || first.ReviewRequestID != review.ID || first.ReviewDecision != string(store.ReviewDecisionApprove) {
		t.Fatalf("review receipt = %+v", first)
	}
	if replay := executeReviewMutationReceipt(t, ctx, config, args); replay != first {
		t.Fatalf("review replay = %+v, want %+v", replay, first)
	}

	services = openCommandLifecycle(t, root)
	closed, err := services.Reviews.Request(ctx, revision.ID, revision.ValidationEvidenceManifest, actor, "request closed review command fixture")
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	if _, err := services.Reviews.Decide(ctx, app.DecideReviewRequest{
		ReviewRequestID: closed.ID, RevisionID: revision.ID, Action: store.ReviewDecisionRequestChanges,
		ExpectedRevisionDigest: revision.TaskDigest, Actor: actor, Reason: "close stale review command fixture",
	}); err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}
	closedKey := commandLifecycleUUID(t)
	closedOutput, err := executeReviewCommand(t, ctx, config, []string{
		"decide", "--request", closed.ID, "--revision", revision.ID, "--action", string(store.ReviewDecisionApprove),
		"--expected-digest", revision.TaskDigest, "--idempotency-key", closedKey, "--reason", "reject closed review selection",
	})
	if err == nil {
		t.Fatalf("review decide accepted closed review selection: %s", closedOutput)
	}
	check := openCommandLifecycle(t, root)
	defer check.Store().Close()
	operation, err := check.Store().GetLifecycleOperationByIdempotencyKey(ctx, closedKey)
	if err != nil || operation != nil {
		t.Fatalf("closed review created lifecycle operation = %+v, %v", operation, err)
	}
}

func executeReviewMutationReceipt(t *testing.T, ctx context.Context, config *lifecycleCLIConfig, args []string) app.LifecycleMutationReceipt {
	t.Helper()
	output, err := executeReviewCommand(t, ctx, config, args)
	if err != nil {
		t.Fatalf("review command %q: %v\n%s", args, err, output)
	}
	var receipt app.LifecycleMutationReceipt
	if err := json.Unmarshal([]byte(output), &receipt); err != nil {
		t.Fatalf("decode review receipt for %q: %v\n%s", args, err, output)
	}
	return receipt
}

func executeReviewCommand(t *testing.T, ctx context.Context, config *lifecycleCLIConfig, args []string) (string, error) {
	t.Helper()
	command := newReviewCommand(config)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	err := command.ExecuteContext(ctx)
	return output.String(), err
}

func TestReleasePackageAndWithdrawCommandsReplayV12Receipts(t *testing.T) {
	ctx := context.Background()
	actor := defaultLifecycleActor()
	if actor == "" {
		t.Skip("local OS actor is unavailable in this test environment")
	}
	root := t.TempDir()
	services := openCommandLifecycle(t, root)
	task, revision := commandPackageableLifecycleFixture(t, ctx, services, actor)
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}

	config := &lifecycleCLIConfig{root: root}
	packageKey := commandLifecycleUUID(t)
	packageArgs := []string{
		"package", "--revision", revision.ID, "--expected-state-version", fmt.Sprint(revision.StateVersion),
		"--version", "cmd-lifecycle-receipt-v1", "--idempotency-key", packageKey, "--reason", "package through lifecycle CLI",
	}
	packageOutput, err := executeReleaseCommand(t, ctx, config, packageArgs)
	if err != nil {
		t.Fatalf("package command: %v\n%s", err, packageOutput)
	}
	var packaged app.LifecycleMutationReceipt
	if err := json.Unmarshal([]byte(packageOutput), &packaged); err != nil {
		t.Fatalf("decode package receipt: %v\n%s", err, packageOutput)
	}
	if packaged.Action != app.LifecycleMutationPackage || packaged.OperationID == "" || packaged.ReleaseID == "" || packaged.TaskID != task.ID || packaged.RevisionID != revision.ID {
		t.Fatalf("package receipt = %+v", packaged)
	}
	packageReplayOutput, err := executeReleaseCommand(t, ctx, config, packageArgs)
	if err != nil {
		t.Fatalf("package replay: %v\n%s", err, packageReplayOutput)
	}
	var packageReplay app.LifecycleMutationReceipt
	if err := json.Unmarshal([]byte(packageReplayOutput), &packageReplay); err != nil {
		t.Fatalf("decode package replay: %v\n%s", err, packageReplayOutput)
	}
	if packageReplay != packaged {
		t.Fatalf("package replay = %+v, want %+v", packageReplay, packaged)
	}

	check := openCommandLifecycle(t, root)
	release, err := check.Store().GetLocalPackageRelease(ctx, packaged.ReleaseID)
	if err != nil || release == nil || release.RecordVersion <= 0 {
		check.Store().Close()
		t.Fatalf("package release = %+v, %v", release, err)
	}
	packageOperation, err := check.Store().GetLifecycleOperationByIdempotencyKey(ctx, packageKey)
	if err != nil || packageOperation == nil || packageOperation.ID != packaged.OperationID || packageOperation.State != store.LifecycleOperationCompleted {
		check.Store().Close()
		t.Fatalf("package operation = %+v, %v", packageOperation, err)
	}
	if err := check.Store().Close(); err != nil {
		t.Fatal(err)
	}

	withdrawKey := commandLifecycleUUID(t)
	withdrawArgs := []string{
		"withdraw", "--release", release.ID, "--expected-version", fmt.Sprint(release.RecordVersion),
		"--idempotency-key", withdrawKey, "--reason", "withdraw through lifecycle CLI",
	}
	withdrawOutput, err := executeReleaseCommand(t, ctx, config, withdrawArgs)
	if err != nil {
		t.Fatalf("withdraw command: %v\n%s", err, withdrawOutput)
	}
	var withdrawn app.LifecycleMutationReceipt
	if err := json.Unmarshal([]byte(withdrawOutput), &withdrawn); err != nil {
		t.Fatalf("decode withdraw receipt: %v\n%s", err, withdrawOutput)
	}
	if withdrawn.Action != app.LifecycleMutationWithdraw || withdrawn.OperationID == "" || withdrawn.ReleaseID != release.ID || withdrawn.ExecutionID == "" {
		t.Fatalf("withdraw receipt = %+v", withdrawn)
	}
	withdrawReplayOutput, err := executeReleaseCommand(t, ctx, config, withdrawArgs)
	if err != nil {
		t.Fatalf("withdraw replay: %v\n%s", err, withdrawReplayOutput)
	}
	var withdrawReplay app.LifecycleMutationReceipt
	if err := json.Unmarshal([]byte(withdrawReplayOutput), &withdrawReplay); err != nil {
		t.Fatalf("decode withdraw replay: %v\n%s", err, withdrawReplayOutput)
	}
	if withdrawReplay != withdrawn {
		t.Fatalf("withdraw replay = %+v, want %+v", withdrawReplay, withdrawn)
	}

	check = openCommandLifecycle(t, root)
	defer check.Store().Close()
	withdrawOperation, err := check.Store().GetLifecycleOperationByIdempotencyKey(ctx, withdrawKey)
	if err != nil || withdrawOperation == nil || withdrawOperation.ID != withdrawn.OperationID || withdrawOperation.State != store.LifecycleOperationCompleted {
		t.Fatalf("withdraw operation = %+v, %v", withdrawOperation, err)
	}
}

func commandPackageableLifecycleFixture(t *testing.T, ctx context.Context, services *app.LifecycleServices, actor string) (store.TaskV2, store.TaskRevision) {
	t.Helper()
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{Slug: "command-package-receipt", MetadataJSON: `{}`, Actor: actor, Reason: "import package command fixture"},
		SourceDirectory:        writeCommandTaskSnapshot(t, "package command fixture\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	revision, err = services.Revisions.MarkValidated(ctx, revision.ID, revision.StateVersion, "sha256:command-package-evidence", actor, "validate package command fixture")
	if err != nil {
		t.Fatal(err)
	}
	review, err := services.Reviews.Request(ctx, revision.ID, revision.ValidationEvidenceManifest, actor, "request package command review")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Reviews.Decide(ctx, app.DecideReviewRequest{
		ReviewRequestID: review.ID, RevisionID: revision.ID, Action: store.ReviewDecisionApprove,
		ExpectedRevisionDigest: revision.TaskDigest, Actor: actor, Reason: "approve package command review",
	}); err != nil {
		t.Fatal(err)
	}
	task, err = services.Reviews.PromoteCurrent(ctx, task.ID, revision.ID, task.Version, actor, "promote package command revision")
	if err != nil {
		t.Fatal(err)
	}
	return task, revision
}

func executeReleaseCommand(t *testing.T, ctx context.Context, config *lifecycleCLIConfig, args []string) (string, error) {
	t.Helper()
	command := newReleaseCommand(config)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	err := command.ExecuteContext(ctx)
	return output.String(), err
}

func commandLifecycleUUID(t *testing.T) string {
	t.Helper()
	id, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	return id
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
	services, err := app.NewLifecycleServicesWithOptions(root, database, app.LifecycleServicesOptions{
		OperationResolver: testsupport.AcceptAllStageOperationResolver(),
	})
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
		TaskID: task.ID, RevisionID: revision.ID, Profile: commandContinuationProfile(), ExecutionSpec: commandExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "continuation-command-fixture", Actor: "tester", Reason: "freeze continuation fixture",
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
	seedCommandContinuationLineage(t, ctx, services.Store(), objects, run, revision, workflow, workflowkit.StageKey(workflowadapter.CodeEdgeLint))
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
	profile := workflowadapter.ExecutionProfile{
		Template:            workflowadapter.StandardTemplateReference(),
		ID:                  "command-continuation",
		Version:             "1",
		ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL,
		ControlGracePeriod:  30 * time.Second,
	}
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
	for _, nodeID := range []workflowkit.NodeID{workflowadapter.CodeEdgeLint, workflowadapter.QualityCheck} {
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
