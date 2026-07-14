package app

import (
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

func newLifecycleServicesForTest(root string, dataStore *store.Store) (*LifecycleServices, error) {
	return NewLifecycleServicesWithOptions(root, dataStore, LifecycleServicesOptions{
		OperationResolver: testsupport.AcceptAllStageOperationResolver(),
	})
}

func TestLifecycleServicesInstallsOnlyExplicitExternalChangeProviders(t *testing.T) {
	root := t.TempDir()
	database, err := store.Open(root)
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

func TestLifecycleServicesExposesOnlyCatalogLockAttestedWorkerResolver(t *testing.T) {
	root := t.TempDir()
	database, err := store.Open(root)
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
	nonProductionDB, err := store.Open(nonProductionRoot)
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
	dataStore, err := store.Open(root)
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
	dataStore, err := store.Open(root)
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

func TestRunServiceResolvesOnlyTheTemplateFrozenByProfileAndExecutionSpec(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := newLifecycleServicesForTest(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "codeedge-template", Actor: "tester", Reason: "import fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "CodeEdge template selection\n"),
		ChangeSummary:          "template selection fixture",
	})
	if err != nil {
		t.Fatal(err)
	}

	profile := lifecycleCompleteProfileForTemplate(t, workflowadapter.CodeEdgePhase1WorkflowTemplate())
	specification := testsupport.CompleteCodeEdgePhase1RunExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	run, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: profile, ExecutionSpec: specification,
		Trigger: "codeedge-phase1", Actor: "tester", Reason: "freeze CodeEdge template",
	})
	if err != nil {
		t.Fatalf("start CodeEdge run: %v", err)
	}
	if run.WorkflowTemplateID != workflowadapter.CodeEdgePhase1WorkflowTemplateID || run.WorkflowTemplateVersion != workflowadapter.CodeEdgePhase1WorkflowTemplateVersion {
		t.Fatalf("run template = %s@%s, want CodeEdge Phase-1", run.WorkflowTemplateID, run.WorkflowTemplateVersion)
	}
	var manifest runManifest
	if err := json.Unmarshal([]byte(run.RunManifestJSON), &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.Resolved.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) || manifest.Resolved.Descriptor.Stages[len(manifest.Resolved.Descriptor.Stages)-1].Key != workflowkit.StageKey(workflowadapter.Package) {
		t.Fatalf("CodeEdge run did not freeze its closed descriptor: %+v", manifest.Resolved.Template)
	}

	mismatched := specification.Clone()
	mismatched.Template = workflowadapter.StandardTemplateReference()
	if _, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: profile, ExecutionSpec: mismatched,
		Trigger: "codeedge-template-mismatch", Actor: "tester", Reason: "reject template mismatch",
	}); err == nil {
		t.Fatal("run accepted mismatched profile/specification templates")
	}
	runs, err := dataStore.ListWorkflowRunsForTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("template mismatch created durable work: %+v", runs)
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
	return lifecycleCompleteProfileForTemplate(t, workflowadapter.StandardWorkflowTemplate())
}

// lifecycleCandidateLeaseProfile keeps candidate-provider tests focused on
// their protocol instead of the deliberately tiny generic integration budget.
// Explicit lease-expiry tests configure their own short TTLs.
func lifecycleCandidateLeaseProfile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	profile := lifecycleCompleteProfile(t)
	for index := range profile.Stages {
		if string(profile.Stages[index].StageKey) != workflowadapter.TaskRepair {
			continue
		}
		profile.Stages[index].Budget.TurnTimeout = 15 * time.Second
		profile.Stages[index].Budget.AttemptTimeout = 15 * time.Second
		profile.Stages[index].Budget.MaxElapsed = 15 * time.Second
		return profile
	}
	t.Fatal("standard lifecycle test profile omits task_repair")
	return workflowadapter.ExecutionProfile{}
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
