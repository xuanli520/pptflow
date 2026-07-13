package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestLifecycleServicesMaterializeImmutableRevisionAndGateCurrentPromotion(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := NewLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	source := writeLifecycleSnapshot(t, "original instruction\n")
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "imported-task", Title: "Imported task", Actor: "tester", Reason: "import fixture"},
		SourceDirectory:        source,
		ChangeSummary:          "import strict snapshot",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.ID == "" || revision.TaskID != task.ID || !strings.HasPrefix(revision.TaskDigest, "harbor.task.v2:sha256:") {
		t.Fatalf("unexpected imported lifecycle entities: task=%+v revision=%+v", task, revision)
	}
	snapshot, err := services.Revisions.SnapshotDirectory(task.ID, revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := harborrun.ComputeManagedTaskDigestV2(snapshot); err != nil || got != revision.TaskDigest {
		t.Fatalf("managed snapshot digest = %q, %v; want %q", got, err, revision.TaskDigest)
	}
	if err := os.WriteFile(filepath.Join(source, "instruction.md"), []byte("source was changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(snapshot, "instruction.md")); err != nil || string(got) != "original instruction\n" {
		t.Fatalf("sealed snapshot changed with source: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(snapshot), "manifest.json")); err != nil {
		t.Fatalf("revision manifest missing: %v", err)
	}

	revision, err = services.Revisions.MarkValidated(ctx, revision.ID, revision.StateVersion, "sha256:blocking-evidence", "tester", "checks pass")
	if err != nil {
		t.Fatal(err)
	}
	review, err := services.Reviews.Request(ctx, revision.ID, "sha256:blocking-evidence", "tester", "request review")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Reviews.Decide(ctx, DecideReviewRequest{
		ReviewRequestID:        review.ID,
		RevisionID:             revision.ID,
		Action:                 store.ReviewDecisionApprove,
		ExpectedRevisionDigest: revision.TaskDigest,
		Actor:                  "tester",
		Reason:                 "approved",
	}); err != nil {
		t.Fatal(err)
	}
	current, err := services.Reviews.PromoteCurrent(ctx, task.ID, revision.ID, task.Version, "tester", "review promotion")
	if err != nil {
		t.Fatal(err)
	}
	if current.CurrentRevisionID != revision.ID || current.LifecycleState != store.TaskLifecycleReady {
		t.Fatalf("current revision did not become ready after approval: %+v", current)
	}
	packageResult, err := services.Releases.PackageRevision(ctx, PackageRevisionRequest{
		RevisionID:             revision.ID,
		ExpectedStateVersion:   revision.StateVersion,
		ReleaseVersion:         "v1",
		Channel:                "stable",
		ExpectedChannelVersion: 0,
		Actor:                  "tester",
		Reason:                 "build local package",
	})
	if err != nil {
		t.Fatal(err)
	}
	if packageResult.Release.PackageRef == "" || packageResult.Release.EvidenceRef != "sha256:blocking-evidence" {
		t.Fatalf("local package release lacks immutable refs: %+v", packageResult.Release)
	}
	if _, err := os.Stat(packageResult.PackagePath); err != nil {
		t.Fatalf("local package path is absent: %v", err)
	}
	if _, err := os.Stat(packageResult.ReceiptPath); err != nil {
		t.Fatalf("local package receipt is absent: %v", err)
	}
	replayedPackage, err := services.Releases.PackageRevision(ctx, PackageRevisionRequest{
		RevisionID:             revision.ID,
		ExpectedStateVersion:   revision.StateVersion,
		ReleaseVersion:         "v1",
		Channel:                "stable",
		ExpectedChannelVersion: 1,
		Actor:                  "tester",
		Reason:                 "reconcile local package after restart",
	})
	if err != nil {
		t.Fatalf("replay local package from immutable receipt: %v", err)
	}
	if replayedPackage.Release.ID != packageResult.Release.ID || replayedPackage.PackagePath != packageResult.PackagePath {
		t.Fatalf("package replay did not reuse immutable release: first=%+v replay=%+v", packageResult, replayedPackage)
	}
	deleted, _, err := services.Deletion.SoftDeleteTask(ctx, task.ID, current.Version+1, "tester", "retire task")
	if err != nil {
		t.Fatal(err)
	}
	purge, err := services.Deletion.PurgeTask(ctx, PurgeTaskRequest{
		TaskID: task.ID, ExpectedTaskVersion: deleted.Version, IdempotencyKey: "lifecycle-release-pin-purge",
		Actor: "tester", Reason: "retention purge",
	})
	if err != nil {
		t.Fatal(err)
	}
	if purge.Purged || len(purge.Dependencies.ReleaseIDs) != 1 || purge.Operation.State != store.TaskPurgeBlocked {
		t.Fatalf("release pin did not block purge: %+v", purge)
	}

	copySource := writeLifecycleSnapshot(t, "changed instruction\n")
	changed, err := services.Revisions.CreateFromSnapshot(ctx, CreateRevisionFromSnapshotRequest{
		TaskID:           task.ID,
		ParentRevisionID: revision.ID,
		Origin:           store.RevisionOriginManual,
		SourceDirectory:  copySource,
		Actor:            "tester",
		Reason:           "manual update",
	})
	if err != nil {
		t.Fatal(err)
	}
	diff, err := services.Revisions.Diff(ctx, revision.ID, changed.ID)
	if err != nil {
		t.Fatal(err)
	}
	changedInstruction := false
	for _, file := range diff.Files {
		if file.Path == "instruction.md" {
			changedInstruction = file.Changed
		}
	}
	if !changedInstruction {
		t.Fatalf("revision diff did not identify changed instruction: %+v", diff)
	}
}

func TestRunServiceRequiresCompleteExplicitProfileAndFreezesManifest(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := NewLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	source := writeLifecycleSnapshot(t, "run instruction\n")
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "run-task", Actor: "tester"},
		SourceDirectory:        source,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Runs.StartRun(ctx, StartRunRequest{TaskID: task.ID, RevisionID: revision.ID, Trigger: "verify"}); err == nil {
		t.Fatal("run accepted an absent implicit profile")
	}
	profile := lifecycleCompleteProfile(t)
	run, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID:         task.ID,
		RevisionID:     revision.ID,
		Profile:        profile,
		Trigger:        "verify",
		ExecutionEpoch: 0,
		Actor:          "tester",
		Reason:         "freeze run fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.DefinitionHash == "" || run.ResolvedProfileHash == "" || run.Status != store.WorkflowRunQueued {
		t.Fatalf("run did not persist frozen definition: %+v", run)
	}
	jobs, err := dataStore.ListDurableJobsForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].CommandType != "workflow_run.execute" || jobs[0].State != store.JobQueued || jobs[0].IdempotencyKey != "workflow-run-execution:"+run.ID {
		t.Fatalf("start run did not atomically queue durable worker job: %+v", jobs)
	}
	pending, err := dataStore.ListPendingOutboxEvents(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundDispatch := false
	for _, event := range pending {
		if event.Topic == "workflow_run.queued" && event.EntityID == run.ID {
			foundDispatch = true
		}
	}
	if !foundDispatch {
		t.Fatalf("start run did not atomically queue workflow outbox event: %+v", pending)
	}
	manifestPath := filepath.Join(root, managedRunsDirectory, run.ID, "run-manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest runManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Resolved.DefinitionFingerprint == "" || manifest.Resolved.ExecutionProfileFingerprint == "" || manifest.Resolved.ContinuationPlanTTL != workflowadapter.RequiredContinuationPlanTTL {
		t.Fatalf("run manifest is not frozen: %+v", manifest)
	}
}

func TestLegacyImportMergesOnlyExactCanonicalIdentityAndQuarantinesIncompleteSource(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := NewLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	source := writeLifecycleSnapshot(t, "legacy exact\n")
	first, err := services.Tasks.ImportLegacy(ctx, LegacyImportRequest{
		SourceDirectory: source,
		Slug:            "legacy-exact",
		SourceRepo:      "https://example.invalid/repo",
		SourceCommit:    "abc123",
		LegacyIdentity:  "repo=https://example.invalid/repo;commit=abc123;proposal=abc",
		ExactIdentity:   true,
		Actor:           "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Merged || first.Orphan || first.Revision == nil {
		t.Fatalf("first exact import was not a canonical revision: %+v", first)
	}
	second, err := services.Tasks.ImportLegacy(ctx, LegacyImportRequest{
		SourceDirectory: source,
		Slug:            "ignored-on-merge",
		SourceRepo:      "https://example.invalid/repo",
		SourceCommit:    "abc123",
		LegacyIdentity:  "repo=https://example.invalid/repo;commit=abc123;proposal=abc",
		ExactIdentity:   true,
		Actor:           "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Merged || second.Orphan || second.Task.ID != first.Task.ID || second.Revision == nil || second.Revision.ID != first.Revision.ID {
		t.Fatalf("exact identity did not merge the existing revision: first=%+v second=%+v", first, second)
	}

	incomplete := t.TempDir()
	writeLifecycleFile(t, filepath.Join(incomplete, "notes.txt"), "unresolved legacy evidence\n", 0o600)
	orphan, err := services.Tasks.ImportLegacy(ctx, LegacyImportRequest{
		SourceDirectory: incomplete,
		Slug:            "legacy-incomplete",
		Actor:           "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !orphan.Orphan || orphan.Merged || orphan.Task.IdentityState != store.TaskIdentityLegacyOrphan || orphan.Revision != nil {
		t.Fatalf("incomplete legacy source did not become an orphan: %+v", orphan)
	}
	if _, err := os.Stat(filepath.Join(root, managedTasksDirectory, orphan.Task.ID, "legacy-snapshot", "notes.txt")); err != nil {
		t.Fatalf("incomplete legacy source was not moved into a managed directory: %v", err)
	}
}

func writeLifecycleSnapshot(t *testing.T, instruction string) string {
	t.Helper()
	root := t.TempDir()
	writeLifecycleFile(t, filepath.Join(root, "instruction.md"), instruction, 0o644)
	writeLifecycleFile(t, filepath.Join(root, "task.toml"), "[task]\nname = \"sample\"\n", 0o644)
	writeLifecycleFile(t, filepath.Join(root, "tests_analysis.md"), "analysis\n", 0o644)
	writeLifecycleFile(t, filepath.Join(root, "environment", "Dockerfile"), "FROM alpine:3.21\n", 0o644)
	writeLifecycleFile(t, filepath.Join(root, "solution", "solve.sh"), "#!/bin/sh\nexit 0\n", 0o755)
	writeLifecycleFile(t, filepath.Join(root, "tests", "test.sh"), "#!/bin/sh\nexit 0\n", 0o755)
	return root
}

func writeLifecycleFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func lifecycleCompleteProfile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	catalog := workflowadapter.StandardStageCatalog()
	profile := workflowadapter.ExecutionProfile{ID: "integration", Version: "1", ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL, ControlGracePeriod: 30 * time.Second}
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
				Backoff:        workflowkit.BackoffPolicy{},
			},
		})
	}
	return profile
}
