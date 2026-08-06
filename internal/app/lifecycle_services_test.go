package app

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// standardAuthoringParentSourceSnapshot is the canonical source archive
// fixture shared by materialized-task tests. It is validated through the same
// standard-authoring source capture boundary that production uses.
func standardAuthoringParentSourceSnapshot(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	entries := []struct {
		name    string
		mode    int64
		content []byte
		dir     bool
	}{
		{name: "source/", mode: 0o755, dir: true},
		{name: "source/README.md", mode: 0o644, content: []byte("fixture source tree\n")},
	}
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: entry.mode}
		if entry.dir {
			header.Typeflag = tar.TypeDir
		} else {
			header.Typeflag = tar.TypeReg
			header.Size = int64(len(entry.content))
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if !entry.dir {
			if _, err := writer.Write(entry.content); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	source := buffer.Bytes()
	if err := validateStandardAuthoringSourceArchiveBytes(source); err != nil {
		t.Fatalf("source snapshot fixture is invalid: %v", err)
	}
	return source
}

func newLifecycleServicesForTest(root string, dataStore *store.Store) (*LifecycleServices, error) {
	return NewLifecycleServicesWithOptions(root, dataStore, LifecycleServicesOptions{
		OperationResolver: testsupport.AcceptAllStageOperationResolver(),
	})
}

func TestLifecycleServicesInstallsOnlyExplicitExternalChangeProviders(t *testing.T) {
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver: testsupport.AcceptAllStageOperationResolver(),
		ChangeProviders:   []ChangeProvider{testChangeProvider{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, found := services.Changes.providers[testChangeProvider{}.ID()]; !found {
		t.Fatal("explicit change provider was not installed at lifecycle composition")
	}
	if _, found := services.Changes.providers[AgentRepairProviderID]; found {
		t.Fatal("ambient agent repair provider was installed without an explicit composition entry")
	}
}

func TestLifecycleServicesExposeAgentTranscriptRetentionSweep(t *testing.T) {
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	services, err := newLifecycleServicesForTest(root, database)
	if err != nil {
		t.Fatal(err)
	}
	if services.Transcripts == nil {
		t.Fatal("agent transcript retention service is not configured")
	}
	result, err := services.Transcripts.SweepExpired(context.Background(), SweepExpiredAgentTranscriptsRequest{
		Limit: 10, Actor: "tester", Reason: "exercise controlled transcript retention sweep",
	})
	if err != nil || len(result.Expired) != 0 || len(result.Blocked) != 0 {
		t.Fatalf("empty transcript retention sweep = %+v, %v", result, err)
	}
}

func TestLifecycleServicesExposesOnlyCatalogLockAttestedWorkerResolver(t *testing.T) {
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	taskID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	revisionID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	specification := testsupport.CompleteRunExecutionSpec(taskID, revisionID, "harbor.task.v2:sha256:"+strings.Repeat("a", 64))
	resolver := catalogLockAttestedResolverForSpec(t, specification, "worker-composition-catalog", "v1", "lock-v1")
	services := catalogLockLifecycleServices(t, root, database, resolver)
	if got := services.CatalogLockAttestedWorkflowkitProviderResolver(); got != resolver {
		t.Fatal("catalog-lock-attested resolver was not preserved for controlled worker composition")
	}

	nonProductionRoot := t.TempDir()
	nonProductionDB, err := store.OpenForTest(nonProductionRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer nonProductionDB.Close()
	nonProduction, err := NewLifecycleServicesWithOptions(nonProductionRoot, nonProductionDB, LifecycleServicesOptions{
		OperationResolver: testsupport.AcceptAllStageOperationResolver(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := nonProduction.CatalogLockAttestedWorkflowkitProviderResolver(); got != nil {
		t.Fatal("non-production resolver was exposed to the worker as a production provider composition")
	}
}

func TestLifecycleServicesMaterializeImmutableRevisionAndGateCurrentPromotion(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := newLifecycleServicesForTest(root, dataStore)
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
	if got, err := taskpolicy.ComputeManagedTaskDigestV2(snapshot); err != nil || got != revision.TaskDigest {
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
	dataStore, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := newLifecycleServicesForTest(root, dataStore)
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
	if _, err := services.Runs.StartRun(ctx, StartRunRequest{TaskID: task.ID, RevisionID: revision.ID, ExecutionSpec: lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "verify"}); err == nil {
		t.Fatal("run accepted an absent implicit profile")
	}
	profile := lifecycleCompleteProfile(t)
	request := StartRunRequest{
		TaskID:         task.ID,
		RevisionID:     revision.ID,
		Profile:        profile,
		ExecutionSpec:  lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest),
		Trigger:        "verify",
		ExecutionEpoch: 0,
		Actor:          "tester",
		Reason:         "freeze run fixture",
	}
	run, err := services.Runs.StartRun(ctx, request)
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
	if manifest.Resolved.DefinitionFingerprint == "" || manifest.Resolved.ExecutionProfileFingerprint == "" || manifest.Resolved.ContinuationPlanTTL != workflowadapter.RequiredContinuationPlanTTL || manifest.Inputs == nil || manifest.Inputs.ExecutionSpecFingerprint == "" || len(manifest.ExecutionSpec) == 0 {
		t.Fatalf("run manifest is not frozen: %+v", manifest)
	}
	profileCanonical, err := profile.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	profileRaw, err := os.ReadFile(filepath.Join(root, managedRunsDirectory, run.ID, runExecutionProfileFileName))
	if err != nil || !bytes.Equal(profileRaw, profileCanonical) {
		t.Fatalf("managed execution profile = %q, %v; want canonical profile", profileRaw, err)
	}
	specificationRaw, err := os.ReadFile(filepath.Join(root, managedRunsDirectory, run.ID, runExecutionSpecFileName))
	if err != nil {
		t.Fatal(err)
	}
	manifestSpecification, err := workflowadapter.ParseRunExecutionSpecJSON(manifest.ExecutionSpec)
	if err != nil {
		t.Fatal(err)
	}
	manifestSpecificationCanonical, err := manifestSpecification.CanonicalJSON()
	if err != nil || !bytes.Equal(specificationRaw, manifestSpecificationCanonical) {
		t.Fatalf("managed execution specification does not match the manifest canonical bytes: %v", err)
	}
	if _, _, err := services.core.verifyRunManagedExecutionInputs(ctx, run); err != nil {
		t.Fatalf("verify managed execution inputs: %v", err)
	}
	var dispatch workflowRunExecutionPayload
	if err := json.Unmarshal([]byte(jobs[0].PayloadJSON), &dispatch); err != nil {
		t.Fatal(err)
	}
	if dispatch.ExecutionSpecFingerprint != manifest.Inputs.ExecutionSpecFingerprint {
		t.Fatalf("initial durable dispatch execution specification fingerprint = %s, want %s", dispatch.ExecutionSpecFingerprint, manifest.Inputs.ExecutionSpecFingerprint)
	}
	expectedInitialPlan, err := workflowkit.CompileDependencyExecutionPlan(manifest.Resolved.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.InitialExecutionPlan.Validate(manifest.Resolved.Descriptor); err != nil || manifest.InitialExecutionPlan.Fingerprint != expectedInitialPlan.Fingerprint {
		t.Fatalf("run manifest initial execution plan = %+v, err=%v; want frozen dependency plan %s", manifest.InitialExecutionPlan, err, expectedInitialPlan.Fingerprint)
	}
	if err := os.WriteFile(filepath.Join(root, managedRunsDirectory, run.ID, runExecutionSpecFileName), []byte(`{"tampered":true}`), 0o640); err != nil {
		t.Fatal(err)
	}
	replay := request
	replay.ID = run.ID
	if _, err := services.Runs.StartRun(ctx, replay); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("replay accepted a tampered managed execution specification: %v", err)
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

func createStandardMaterializedLifecycleTask(t *testing.T, ctx context.Context, services *LifecycleServices, slug, title, instruction string) (store.TaskV2, store.TaskRevision) {
	t.Helper()
	if services == nil || services.core == nil || services.core.store == nil || services.core.objects == nil {
		t.Fatal("lifecycle services fixture is not configured")
	}
	if title == "" {
		title = slug
	}
	actor := "standard-materialized-fixture"
	sourceArchive := standardAuthoringParentSourceSnapshot(t)
	sourceObject, err := services.core.objects.PutBytes(ctx, sourceArchive)
	if err != nil {
		t.Fatalf("store source snapshot fixture: %v", err)
	}
	repositoryURL := "https://github.com/purplevoid/" + slug + ".git"
	commitSHA := strings.Repeat("1", 40)
	source, err := services.core.store.CreateAuthoringSource(ctx, store.CreateAuthoringSourceRequest{
		RepositoryURL: repositoryURL, CommitSHA: commitSHA,
		SnapshotArtifactRef: string(sourceObject.Digest), SnapshotContentDigest: string(sourceObject.Digest),
		SnapshotSchemaVersion: StandardAuthoringSourceSnapshotSchemaVersion,
		IdempotencyKey:        "standard-source:" + slug, Actor: actor, Reason: "freeze Standard source fixture",
	})
	if err != nil {
		t.Fatalf("create Standard source fixture: %v", err)
	}
	task, err := services.core.store.CreateTaskV2(ctx, store.CreateTaskV2Request{
		Slug: slug, Title: title, SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA,
		Actor: actor, Reason: "reserve Standard draft task fixture",
	})
	if err != nil {
		t.Fatalf("create Standard draft task fixture: %v", err)
	}
	reference := workflowadapter.StandardAuthoringCurrentTemplateReference()
	session, err := services.core.store.CreateAuthoringSession(ctx, store.CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID,
		WorkflowTemplateID: reference.ID, WorkflowTemplateVersion: reference.Version,
		SessionManifestJSON: `{"mode":"standard","fixture":true}`,
		IdempotencyKey:      "standard-session:" + slug, Actor: actor, Reason: "freeze Standard session fixture",
	})
	if err != nil {
		t.Fatalf("create Standard session fixture: %v", err)
	}
	run, err := services.core.store.CreateAuthoringWorkflowRun(ctx, store.CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID,
		WorkflowTemplateID: reference.ID, WorkflowTemplateVersion: reference.Version,
		ResolvedProfileHash: string(workflowkit.SHA256Fingerprint([]byte("standard-profile:" + slug))),
		DefinitionHash:      string(workflowkit.SHA256Fingerprint([]byte("standard-definition:" + slug))),
		RunManifestJSON:     `{}`, Trigger: "standard-materialized-fixture", Actor: actor, Reason: "start Standard fixture Run",
	})
	if err != nil {
		t.Fatalf("create Standard authoring Run fixture: %v", err)
	}
	run, err = services.core.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning,
		Actor: actor, Reason: "run Standard materialization fixture",
	})
	if err != nil {
		t.Fatalf("transition Standard authoring Run fixture: %v", err)
	}
	revisionID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatalf("allocate Standard materialized revision ID: %v", err)
	}
	sourceDirectory := writeLifecycleSnapshot(t, instruction)
	prepared, cleanup, err := (&RevisionService{core: services.core}).prepareSnapshot(ctx, task.ID, revisionID, sourceDirectory)
	if err != nil {
		t.Fatalf("prepare Standard materialized snapshot: %v", err)
	}
	cleanupPrepared := true
	defer func() {
		if cleanupPrepared {
			cleanup()
		}
	}()
	result, err := services.core.store.MaterializeAuthoringTask(ctx, store.MaterializeAuthoringTaskRequest{
		IdempotencyKey: "standard-materialization:" + slug, AuthoringSessionID: session.ID, AuthoringRunID: run.ID,
		ExpectedTaskVersion: task.Version, ExpectedRunVersion: run.Version,
		RevisionID: revisionID, TaskDigest: prepared.TaskDigest, ProposalDigest: string(workflowkit.SHA256Fingerprint([]byte("standard-proposal:" + slug))),
		ManifestID: prepared.ManifestObjectID, ChangeSummary: "materialize Standard fixture", MetadataJSON: `{}`,
		Actor: actor, Reason: "materialize Standard fixture task",
	})
	if err != nil {
		t.Fatalf("materialize Standard fixture task: %v", err)
	}
	cleanupPrepared = false
	return result.Task, result.Revision
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
	return lifecycleCompleteProfileForTemplate(t, workflowadapter.StandardWorkflowTemplate())
}

// lifecycleCandidateLeaseProfile keeps candidate-provider tests focused on
// their protocol instead of the deliberately tiny generic integration budget.
// The candidate policy is template-neutral; it must not infer a lease from a
// task_repair stage that a closed workflow may not contain.
func lifecycleCandidateLeaseProfile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	profile := lifecycleCompleteProfile(t)
	profile.CandidateProviderBudget = workflowadapter.CandidateProviderBudget{AttemptTimeout: 15 * time.Second}
	return profile
}

func lifecycleCompleteProfileForTemplate(t *testing.T, template workflowadapter.WorkflowTemplate) workflowadapter.ExecutionProfile {
	t.Helper()
	catalog := template.Catalog
	profile := workflowadapter.ExecutionProfile{
		Template:            template.Reference(),
		ID:                  "integration",
		Version:             "1",
		ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL,
		ControlGracePeriod:  30 * time.Second,
		CandidateProviderBudget: workflowadapter.CandidateProviderBudget{
			AttemptTimeout: time.Second,
		},
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
				Backoff:        workflowkit.BackoffPolicy{},
			},
		})
	}
	return profile
}

func lifecycleExecutionSpec(taskID, revisionID, revisionDigest string) workflowadapter.RunExecutionSpec {
	specification := testsupport.CompleteRunExecutionSpec(taskID, revisionID, revisionDigest)
	specification.Template = workflowadapter.StandardTemplateReference()
	return specification
}
