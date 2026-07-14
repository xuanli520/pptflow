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

	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

type testChangeProvider struct{}

type misreportingChangeProvider struct{}

type sleepingChangeProvider struct {
	delay time.Duration
}

const (
	candidateHeartbeatLeaseTTL       = 6 * time.Second
	candidateHeartbeatProviderDelay  = 5 * time.Second
	candidateHeartbeatProviderBudget = 10 * time.Second
)

func (provider sleepingChangeProvider) ID() string { return "sleeping_change" }

func (sleepingChangeProvider) ValidatePayload(raw json.RawMessage) (json.RawMessage, error) {
	return raw, nil
}

func (provider sleepingChangeProvider) Apply(_ context.Context, _ ChangeProviderRequest) (ChangeProviderReceipt, error) {
	time.Sleep(provider.delay)
	return ChangeProviderReceipt{Format: "test.sleeping-provider.v1", ProviderID: provider.ID()}, nil
}

func TestChangeProviderServiceDoesNotInstallAmbientAgentRepair(t *testing.T) {
	service := newChangeProviderService(&lifecycleServiceCore{})
	if _, installed := service.providers[AgentRepairProviderID]; installed {
		t.Fatal("agent repair provider must require explicit controlled registration")
	}
	if _, installed := service.providers[LocalPatchProviderID]; !installed {
		t.Fatal("explicit local patch provider must remain installed")
	}
}

func (testChangeProvider) ID() string { return "test_change" }

func (testChangeProvider) ValidatePayload(raw json.RawMessage) (json.RawMessage, error) {
	var payload struct {
		Format string `json:"format"`
	}
	if err := decodeProviderPayload(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Format != "test.change.v1" {
		return nil, os.ErrInvalid
	}
	return raw, nil
}

func (testChangeProvider) Apply(_ context.Context, request ChangeProviderRequest) (ChangeProviderReceipt, error) {
	if err := os.WriteFile(filepath.Join(request.Checkout, "instruction.md"), []byte("repaired instruction\n"), 0o644); err != nil {
		return ChangeProviderReceipt{}, err
	}
	return ChangeProviderReceipt{Format: "test.change-receipt.v1", ProviderID: "test_change", ChangedPaths: []string{"instruction.md"}, Summary: "test repair"}, nil
}

func (misreportingChangeProvider) ID() string { return "misreporting_change" }

func (misreportingChangeProvider) ValidatePayload(raw json.RawMessage) (json.RawMessage, error) {
	return testChangeProvider{}.ValidatePayload(raw)
}

func (provider misreportingChangeProvider) Apply(_ context.Context, request ChangeProviderRequest) (ChangeProviderReceipt, error) {
	if err := os.WriteFile(filepath.Join(request.Checkout, "instruction.md"), []byte("repaired instruction\n"), 0o644); err != nil {
		return ChangeProviderReceipt{}, err
	}
	return ChangeProviderReceipt{Format: "test.change-receipt.v1", ProviderID: provider.ID(), ChangedPaths: []string{"task.toml"}, Summary: "incorrect provider report"}, nil
}

func findingBundleForRun(t *testing.T, ctx context.Context, services *LifecycleServices, dataStore *store.Store, run store.WorkflowRun, revision store.TaskRevision, message string) FindingBundle {
	t.Helper()
	workflow, err := decodeFrozenWorkflow(run)
	if err != nil {
		t.Fatal(err)
	}
	stage, found := workflow.Stage(workflowkit.StageKey(workflowadapter.QualityCheck))
	if !found {
		t.Fatalf("frozen workflow does not contain %q", workflowadapter.QualityCheck)
	}
	bindings := fixtureInputBindings(stage)
	fingerprint, err := workflowkit.FingerprintArtifactBindings(bindings)
	if err != nil {
		t.Fatal(err)
	}
	references := seedReusableContinuationStage(t, ctx, dataStore, services.core.objects, run, revision, stage, bindings, fingerprint)
	if len(references) == 0 {
		t.Fatal("quality stage fixture did not create a report artifact")
	}
	report := references[0]
	return FindingBundle{
		Format: "harbor.findings.v1", RevisionID: revision.ID, RevisionDigest: revision.TaskDigest,
		Findings: []RepairFinding{{
			CheckerID: "quality", StageKey: workflowadapter.QualityCheck, CheckID: "instruction", Severity: "error", Message: message,
			ReportArtifactID: report.ID, ReportContentDigest: report.ContentDigest,
		}},
	}
}

func TestFindingBundleRequiresNonEmptyCanonicalReportEvidence(t *testing.T) {
	reportID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	valid := FindingBundle{
		Format: "harbor.findings.v1", RevisionID: "revision", RevisionDigest: "harbor.task.v2:sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Findings: []RepairFinding{{
			CheckerID: "quality", StageKey: workflowadapter.QualityCheck, CheckID: "instruction", Severity: "error", Message: "invalid instruction",
			ReportArtifactID: reportID, ReportContentDigest: string(workflowkit.SHA256Fingerprint([]byte("report"))),
		}},
	}
	if err := valid.Validate(valid.RevisionID, valid.RevisionDigest); err != nil {
		t.Fatalf("valid structured finding rejected: %v", err)
	}
	cases := []struct {
		name   string
		bundle FindingBundle
	}{
		{name: "empty findings", bundle: FindingBundle{Format: valid.Format, RevisionID: valid.RevisionID, RevisionDigest: valid.RevisionDigest}},
		{name: "missing report ID", bundle: FindingBundle{Format: valid.Format, RevisionID: valid.RevisionID, RevisionDigest: valid.RevisionDigest, Findings: []RepairFinding{{CheckerID: "quality", StageKey: workflowadapter.QualityCheck, CheckID: "instruction", Severity: "error", Message: "invalid instruction", ReportContentDigest: valid.Findings[0].ReportContentDigest}}}},
		{name: "noncanonical report digest", bundle: FindingBundle{Format: valid.Format, RevisionID: valid.RevisionID, RevisionDigest: valid.RevisionDigest, Findings: []RepairFinding{{CheckerID: "quality", StageKey: workflowadapter.QualityCheck, CheckID: "instruction", Severity: "error", Message: "invalid instruction", ReportArtifactID: reportID, ReportContentDigest: "sha256:ABC"}}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := test.bundle.Validate(valid.RevisionID, valid.RevisionDigest); err == nil {
				t.Fatal("invalid finding bundle was accepted")
			}
		})
	}
}

func TestCandidateLeaseHeartbeatReturnsLatestFenceAcrossMultipleRenewals(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	services, err := newLifecycleServicesForTest(root, database)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := database.AcquireLease(ctx, store.AcquireLeaseRequest{ResourceType: "task_revision_candidate", ResourceID: "heartbeat-task", Owner: "heartbeat-owner", TTL: candidateHeartbeatLeaseTTL, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	_, latest, err := services.Changes.applyWithCandidateLeaseHeartbeat(ctx, sleepingChangeProvider{delay: candidateHeartbeatProviderDelay}, ChangeProviderRequest{Actor: "tester", Timeout: candidateHeartbeatProviderBudget}, lease, candidateHeartbeatLeaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version < lease.Version+2 {
		t.Fatalf("latest heartbeat lease version = %d, want at least %d", latest.Version, lease.Version+2)
	}
	if _, err := database.HeartbeatLease(ctx, store.HeartbeatLeaseRequest{LeaseID: latest.ID, Owner: latest.Owner, FencingToken: latest.FencingToken, ExpectedVersion: latest.Version, TTL: candidateHeartbeatLeaseTTL, Actor: "tester", Reason: "prove latest fence"}); err != nil {
		t.Fatalf("latest heartbeat fence cannot finalize a subsequent CAS: %v", err)
	}
}

func TestCandidateLeaseHeartbeatRenewsBeforeProviderStarts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	services, err := newLifecycleServicesForTest(root, database)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := database.AcquireLease(ctx, store.AcquireLeaseRequest{ResourceType: "task_revision_candidate", ResourceID: "start-heartbeat-task", Owner: "start-heartbeat-owner", TTL: candidateHeartbeatLeaseTTL, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	_, latest, err := services.Changes.applyWithCandidateLeaseHeartbeat(ctx, sleepingChangeProvider{delay: 5 * time.Millisecond}, ChangeProviderRequest{Actor: "tester", Timeout: time.Second}, lease, candidateHeartbeatLeaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != lease.Version+1 {
		t.Fatalf("provider start did not synchronously renew candidate lease: got version %d, want %d", latest.Version, lease.Version+1)
	}
}

func TestCandidateLeaseHeartbeatEnforcesProviderBudgetDeadline(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	services, err := newLifecycleServicesForTest(root, database)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := database.AcquireLease(ctx, store.AcquireLeaseRequest{ResourceType: "task_revision_candidate", ResourceID: "budget-task", Owner: "budget-owner", TTL: candidateHeartbeatLeaseTTL, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = services.Changes.applyWithCandidateLeaseHeartbeat(ctx, sleepingChangeProvider{delay: 120 * time.Millisecond}, ChangeProviderRequest{Actor: "tester", Timeout: 30 * time.Millisecond}, lease, candidateHeartbeatLeaseTTL)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("provider that ignored its deadline err=%v, want context deadline exceeded", err)
	}
}

func TestChangeProviderCreatesIsolatedRevisionAndChildRun(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	services, err := newLifecycleServicesForTest(root, database)
	if err != nil {
		t.Fatal(err)
	}
	services.Changes.Register(testChangeProvider{})
	source := writeLifecycleSnapshot(t, "original instruction\n")
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "candidate-task", Actor: "tester", Reason: "import"},
		SourceDirectory:        source,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, StartRunRequest{TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCandidateLeaseProfile(t), ExecutionSpec: lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "verify", Actor: "tester", Reason: "verify base"})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := services.Continuations.CurrentCheckpoint(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	command := ContinueTaskCommand{
		CommandKey: "change-command", TaskID: task.ID, RunID: run.ID, Expected: checkpoint, Actor: "tester", Reason: "apply a verified repair",
		Change: &TaskChangeRequest{ProviderID: "test_change", OperationKey: "change-operation", Payload: json.RawMessage(`{"format":"test.change.v1"}`),
			Findings: findingBundleForRun(t, ctx, services, database, run, revision, "instruction is incomplete")},
	}
	plan, err := services.Continuations.PlanTaskContinuation(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := plan.Snapshot()
	if snapshot.Strategy != "revise_subject" || snapshot.TargetRunRelation != "child_run" || snapshot.CandidateRevisionID == "" {
		t.Fatalf("content continuation plan = %+v", snapshot)
	}
	for _, transition := range snapshot.Nodes {
		if transition.Disposition == workflowkit.DispositionPreserve {
			t.Fatalf("candidate continuation preserved source evidence for %q: %+v", transition.NodeID, transition)
		}
	}
	candidate, err := database.GetRevisionCandidate(ctx, snapshot.CandidateRevisionID)
	if err != nil || candidate == nil || candidate.State != store.RevisionCandidatePrepared || candidate.AfterDigest == revision.TaskDigest {
		t.Fatalf("candidate = %+v, err=%v", candidate, err)
	}
	if _, err := services.Changes.ExecuteTaskChange(ctx, plan.ID(), "different-actor", "different reason"); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("candidate continuation accepted changed frozen provenance: %v", err)
	}
	baseSnapshot, err := services.Revisions.SnapshotDirectory(task.ID, revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	baseInstruction, err := os.ReadFile(filepath.Join(baseSnapshot, "instruction.md"))
	if err != nil || string(baseInstruction) != "original instruction\n" {
		t.Fatalf("sealed base revision changed: %q %v", baseInstruction, err)
	}
	execution, err := services.Continuations.ExecuteTaskContinuation(ctx, plan.ID())
	if err != nil {
		t.Fatal(err)
	}
	job, err := database.GetDurableJobByIdempotency(ctx, "candidate-continuation-job:"+continuationExecutionKey(plan.ID()))
	if err != nil || job == nil || job.EntityID != execution.ID {
		t.Fatalf("candidate continuation durable job = %+v, %v", job, err)
	}
	var payload continuationExecutionPayload
	if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil {
		t.Fatalf("decode candidate continuation durable payload: %v", err)
	}
	frozen, err := decodeFrozenRunDefinition(run)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Format != continuationExecutionFormat || payload.RunID != execution.RunID || payload.SourceRunID != run.ID || payload.PlanID != plan.ID() {
		t.Fatalf("candidate continuation payload binding = %+v", payload)
	}
	assertFrozenQuotaPayload(t, payload.QuotaPolicy, frozen)
	childRun, err := services.Runs.Get(ctx, execution.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if childRun.ParentRunID != run.ID || childRun.RevisionID != candidate.TargetRevisionID || childRun.Status != store.WorkflowRunQueued {
		t.Fatalf("child run = %+v", childRun)
	}
	childRevision, err := services.Revisions.Get(ctx, childRun.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	if childRevision.ParentRevisionID != revision.ID || childRevision.Origin != store.RevisionOriginManual || childRevision.TaskDigest != candidate.AfterDigest {
		t.Fatalf("child revision = %+v", childRevision)
	}
	var childManifest runManifest
	if err := decodeStrictJSON(childRun.RunManifestJSON, &childManifest); err != nil {
		t.Fatal(err)
	}
	if childManifest.Inputs == nil || childManifest.Inputs.Format != runManifestInputsFormat || childManifest.Inputs.BundleID != "" || childManifest.Inputs.ProfileFingerprint == "" || len(childManifest.ExecutionSpec) == 0 {
		t.Fatalf("child run did not create fresh execution inputs: %+v", childManifest.Inputs)
	}
	childSpecification, err := workflowadapter.ParseRunExecutionSpecJSON(childManifest.ExecutionSpec)
	if err != nil {
		t.Fatal(err)
	}
	if childSpecification.Selection.TaskID != task.ID || childSpecification.Selection.RevisionID != childRevision.ID || string(childSpecification.Selection.RevisionDigest) != childRevision.TaskDigest {
		t.Fatalf("child execution selection = %+v, want child revision %s", childSpecification.Selection, childRevision.ID)
	}
	for _, checkout := range childSpecification.References.Checkouts {
		if checkout.RevisionID != childRevision.ID || string(checkout.RevisionDigest) != childRevision.TaskDigest {
			t.Fatalf("child checkout %q did not rebind to candidate revision: %+v", checkout.ID, checkout)
		}
	}
	childSpecificationFingerprint, err := childSpecification.Fingerprint()
	if err != nil || childSpecificationFingerprint != childManifest.Inputs.ExecutionSpecFingerprint {
		t.Fatalf("child execution specification fingerprint = %s, %v; manifest=%s", childSpecificationFingerprint, err, childManifest.Inputs.ExecutionSpecFingerprint)
	}
	var sourceManifest runManifest
	if err := decodeStrictJSON(run.RunManifestJSON, &sourceManifest); err != nil {
		t.Fatal(err)
	}
	sourceSpecification, err := workflowadapter.ParseRunExecutionSpecJSON(sourceManifest.ExecutionSpec)
	if err != nil {
		t.Fatal(err)
	}
	sourceSpecificationFingerprint, err := sourceSpecification.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if childSpecificationFingerprint == sourceSpecificationFingerprint {
		t.Fatal("child execution specification reused the source revision binding")
	}
	childDirectory := filepath.Join(root, managedRunsDirectory, childRun.ID)
	childProfileRaw, err := os.ReadFile(filepath.Join(childDirectory, runExecutionProfileFileName))
	if err != nil {
		t.Fatal(err)
	}
	sourceProfileRaw, err := os.ReadFile(filepath.Join(root, managedRunsDirectory, run.ID, runExecutionProfileFileName))
	if err != nil || !bytes.Equal(childProfileRaw, sourceProfileRaw) {
		t.Fatalf("child managed execution profile = %q, %v; want source frozen profile", childProfileRaw, err)
	}
	childSpecificationRaw, err := os.ReadFile(filepath.Join(childDirectory, runExecutionSpecFileName))
	if err != nil || !bytes.Equal(childSpecificationRaw, childManifest.ExecutionSpec) {
		t.Fatalf("child managed execution specification = %q, %v; want child manifest", childSpecificationRaw, err)
	}
	if _, _, err := services.core.verifyRunManagedExecutionInputs(ctx, childRun); err != nil {
		t.Fatalf("child managed execution inputs do not bind the child manifest/revision: %v", err)
	}
	expectedChildPlan, err := workflowkit.CompileDependencyExecutionPlan(childManifest.Resolved.Descriptor)
	if err != nil || !manifestMatchesInitialExecutionPlan(childManifest, childManifest.Resolved.Descriptor, expectedChildPlan) {
		t.Fatalf("child initial execution plan was not rebuilt: %+v, %v", childManifest.InitialExecutionPlan, err)
	}
	childAttempts, err := database.ListStageAttemptsForRun(ctx, childRun.ID)
	if err != nil || len(childAttempts) != 0 {
		t.Fatalf("child Run inherited stage attempts/evidence = %+v, %v", childAttempts, err)
	}
	childFrozen, err := decodeFrozenRunDefinition(childRun)
	if err != nil {
		t.Fatal(err)
	}
	bridgeRegistry, err := workflowkit.NewControlledPluginRegistry([]workflowkit.PluginRegistration[workflowkit.StageExecutor]{
		{Binding: childFrozen.Workflow.Stages[0].Plugin, Implementation: workflowkit.StageExecutorFunc(completedFixtureStage)},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newFrozenRuntime(t, services, bridgeRegistry)
	bridge := &workflowkitStageBackend{
		runtime: runtime, run: childRun, revision: childRevision, frozen: childFrozen,
		job: store.DurableJob{CreatedBy: "tester"},
	}
	if _, err := bridge.frozenExecution(); err != nil {
		t.Fatalf("public Engine bridge rejected rebound child Run: %v", err)
	}
	updatedCandidate, err := database.GetRevisionCandidate(ctx, candidate.ID)
	if err != nil || updatedCandidate == nil || updatedCandidate.State != store.RevisionCandidateCommitted {
		t.Fatalf("candidate commit = %+v, %v", updatedCandidate, err)
	}
	replayed, err := services.Continuations.ExecuteTaskContinuation(ctx, plan.ID())
	if err != nil || replayed.ID != execution.ID {
		t.Fatalf("idempotent candidate execution = %+v, %v", replayed, err)
	}
	if _, err := services.Changes.ensureCandidateChildRunManifest(ctx, *candidate, run); err != nil {
		t.Fatalf("candidate child recovery rejected matching managed execution inputs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(childDirectory, runExecutionSpecFileName), []byte(`{"tampered":true}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := services.Changes.ensureCandidateChildRunManifest(ctx, *candidate, run); err == nil {
		t.Fatal("candidate child recovery accepted a tampered managed execution specification")
	}
}

func TestCodeEdgeCandidateChildRunMaterializesFreshManagedTaskSnapshotAtomically(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{OperationResolver: testsupport.AcceptAllStageOperationResolver()})
	if err != nil {
		t.Fatal(err)
	}
	services.Changes.Register(testChangeProvider{})

	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "codeedge-candidate-input", Actor: "tester", Reason: "import CodeEdge candidate fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "CodeEdge candidate source instruction\n"),
		ChangeSummary:          "create CodeEdge candidate source revision",
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := codeEdgeEvaluatorRuntimeProfile(t)
	profile.CandidateProviderBudget = workflowadapter.CandidateProviderBudget{
		AttemptTimeout: 15 * time.Second,
		StartupGrace:   2 * time.Second,
		ShutdownGrace:  2 * time.Second,
	}
	run, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: profile,
		ExecutionSpec: testsupport.CompleteCodeEdgePhase1RunExecutionSpec(task.ID, revision.ID, revision.TaskDigest),
		Trigger:       "codeedge-candidate-input", Actor: "tester", Reason: "freeze CodeEdge source Run",
	})
	if err != nil {
		t.Fatal(err)
	}
	var sourceManifest runManifest
	if err := decodeStrictJSON(run.RunManifestJSON, &sourceManifest); err != nil {
		t.Fatal(err)
	}
	if sourceManifest.Inputs == nil || len(sourceManifest.Inputs.ManagedInputs) != 1 {
		t.Fatalf("source managed inputs = %+v, want one task snapshot", sourceManifest.Inputs)
	}
	sourceInput := sourceManifest.Inputs.ManagedInputs[0]

	checkpoint, err := services.Continuations.CurrentCheckpoint(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := services.Continuations.PlanTaskContinuation(ctx, ContinueTaskCommand{
		CommandKey: "codeedge-candidate-input-change", TaskID: task.ID, RunID: run.ID, Expected: checkpoint,
		Actor: "tester", Reason: "repair CodeEdge candidate task snapshot",
		Change: &TaskChangeRequest{
			ProviderID: "test_change", OperationKey: "codeedge-candidate-input-operation", Payload: json.RawMessage(`{"format":"test.change.v1"}`),
			Findings: findingBundleForRun(t, ctx, services, database, run, revision, "candidate task snapshot must be refreshed"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	transitions := make(map[workflowkit.NodeID]workflowkit.NodeTransition, len(plan.Snapshot().Nodes))
	for _, transition := range plan.Snapshot().Nodes {
		transitions[transition.NodeID] = transition
	}
	if transitions[workflowkit.NodeID(workflowadapter.RepoPrepare)].Disposition != workflowkit.DispositionSchedule {
		t.Fatalf("CodeEdge candidate root transition = %+v, want schedule", transitions[workflowkit.NodeID(workflowadapter.RepoPrepare)])
	}
	for _, nodeID := range []workflowkit.NodeID{workflowadapter.HarborRunQwen, workflowadapter.HarborRunOpus, workflowadapter.SubmissionLint, workflowadapter.ResultReview} {
		if transitions[nodeID].Disposition != workflowkit.DispositionInvalidate {
			t.Fatalf("CodeEdge candidate external-effect closure transition %q = %+v, want invalidate", nodeID, transitions[nodeID])
		}
	}
	if transitions[workflowkit.NodeID(workflowadapter.Package)].Disposition != workflowkit.DispositionOperatorOnly {
		t.Fatalf("CodeEdge candidate package transition = %+v, want operator-only", transitions[workflowkit.NodeID(workflowadapter.Package)])
	}
	candidate, err := database.GetRevisionCandidate(ctx, plan.Snapshot().CandidateRevisionID)
	if err != nil || candidate == nil {
		t.Fatalf("load prepared CodeEdge candidate = %+v, %v", candidate, err)
	}
	if preCommitInput, err := database.GetRunInputArtifactForPort(ctx, candidate.TargetRunID, managedTaskSnapshotInputPort); err != nil || preCommitInput != nil {
		t.Fatalf("candidate child input became durable before candidate commit: %+v, %v", preCommitInput, err)
	}

	execution, err := services.Continuations.ExecuteTaskContinuation(ctx, plan.ID())
	if err != nil {
		t.Fatal(err)
	}
	childRun, err := services.Runs.Get(ctx, execution.RunID)
	if err != nil {
		t.Fatal(err)
	}
	childRevision, err := services.Revisions.Get(ctx, childRun.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	if childRun.ID != candidate.TargetRunID || childRevision.ID != candidate.TargetRevisionID || childRevision.TaskDigest != candidate.AfterDigest {
		t.Fatalf("candidate child identity = run %+v revision %+v candidate %+v", childRun, childRevision, candidate)
	}
	var childManifest runManifest
	if err := decodeStrictJSON(childRun.RunManifestJSON, &childManifest); err != nil {
		t.Fatal(err)
	}
	if childManifest.Inputs == nil || len(childManifest.Inputs.ManagedInputs) != 1 {
		t.Fatalf("child managed inputs = %+v, want one task snapshot", childManifest.Inputs)
	}
	childInput := childManifest.Inputs.ManagedInputs[0]
	if childInput.Port != managedTaskSnapshotInputPort || childInput.ID == sourceInput.ID || childInput.ContentDigest == sourceInput.ContentDigest || childInput.RevisionDigest != workflowkit.SubjectDigest(childRevision.TaskDigest) {
		t.Fatalf("child task snapshot did not receive fresh candidate identity/object: child=%+v source=%+v", childInput, sourceInput)
	}
	if bytes.Contains(childManifest.ExecutionSpec, []byte(sourceInput.ID)) || bytes.Contains(childManifest.ExecutionSpec, []byte(sourceInput.ContentDigest)) {
		t.Fatalf("child final execution spec retained a source task_snapshot binding: %s", childManifest.ExecutionSpec)
	}
	persisted, err := database.GetRunInputArtifactForPort(ctx, childRun.ID, managedTaskSnapshotInputPort)
	if err != nil || persisted == nil || persisted.ID != childInput.ID || persisted.RevisionID != childRevision.ID || persisted.RevisionDigest != childRevision.TaskDigest || persisted.ContentDigest != string(childInput.ContentDigest) {
		t.Fatalf("candidate child durable managed input = %+v, %v", persisted, err)
	}
	stage, found := childManifest.Resolved.Descriptor.Stage(workflowkit.StageKey(workflowadapter.RepoPrepare))
	if !found {
		t.Fatal("CodeEdge child workflow omits repo_prepare")
	}
	bindings, err := resolveStageInputs(ctx, database, services.core.objects, childRun, childRevision, stage)
	if err != nil {
		t.Fatalf("resolve CodeEdge child root inputs: %v", err)
	}
	if len(bindings) != 1 || bindings[0].Name != managedTaskSnapshotInputPort || bindings[0].ArtifactID != workflowkit.ArtifactID(childInput.ID) {
		t.Fatalf("CodeEdge child root bindings = %+v", bindings)
	}
	content, err := newStageInputReader(database, services.core.objects, childRun, childRevision, bindings)(ctx, bindings[0])
	if err != nil || len(content) != int(childInput.SizeBytes) {
		t.Fatalf("read CodeEdge child task snapshot = %d bytes, %v", len(content), err)
	}
	attempts, err := database.ListStageAttemptsForRun(ctx, childRun.ID)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("candidate child Run acquired synthetic StageAttempts: %+v, %v", attempts, err)
	}

	replayed, err := services.Continuations.ExecuteTaskContinuation(ctx, plan.ID())
	if err != nil || replayed.ID != execution.ID {
		t.Fatalf("candidate child continuation replay = %+v, %v", replayed, err)
	}
	replayedInput, err := database.GetRunInputArtifactForPort(ctx, childRun.ID, managedTaskSnapshotInputPort)
	if err != nil || replayedInput == nil || replayedInput.ID != childInput.ID || replayedInput.ContentDigest != string(childInput.ContentDigest) {
		t.Fatalf("candidate child managed input replay = %+v, %v", replayedInput, err)
	}
}

func TestLocalPatchProviderRejectsPathTraversalAndDoesNotWriteOutsideCandidate(t *testing.T) {
	provider := LocalPatchProvider{}
	if _, err := provider.ValidatePayload(json.RawMessage(`{"format":"harbor.local-unified-diff.v1","diff":"--- a/../../outside\\n+++ b/../../outside\\n@@ -1 +1 @@\\n-a\\n+b\\n"}`)); err == nil {
		t.Fatal("path-traversing unified diff was accepted")
	}
}

func TestLocalPatchProviderAppliesOnlyCanonicalUnifiedDiff(t *testing.T) {
	checkout := writeLifecycleSnapshot(t, "original instruction\n")
	provider := LocalPatchProvider{}
	payload, err := provider.ValidatePayload(json.RawMessage(`{"format":"harbor.local-unified-diff.v1","diff":"--- a/instruction.md\n+++ b/instruction.md\n@@ -1 +1 @@ instruction\n-original instruction\n+patched instruction\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := provider.Apply(context.Background(), ChangeProviderRequest{Checkout: checkout, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.ChangedPaths) != 1 || receipt.ChangedPaths[0] != "instruction.md" {
		t.Fatalf("patch receipt = %+v", receipt)
	}
	contents, err := os.ReadFile(filepath.Join(checkout, "instruction.md"))
	if err != nil || string(contents) != "patched instruction\n" {
		t.Fatalf("patched instruction = %q, %v", contents, err)
	}
}

func TestLocalPatchProviderAppliesWithoutPATHOrExternalPatchExecutable(t *testing.T) {
	t.Setenv("PATH", "")
	checkout := writeLifecycleSnapshot(t, "original instruction\n")
	provider := LocalPatchProvider{}
	payload, err := provider.ValidatePayload(json.RawMessage(`{"format":"harbor.local-unified-diff.v1","diff":"--- a/instruction.md\n+++ b/instruction.md\n@@ -1 +1 @@\n-original instruction\n+patched without path\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Apply(context.Background(), ChangeProviderRequest{Checkout: checkout, Payload: payload}); err != nil {
		t.Fatalf("in-process local patch with PATH unset: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(checkout, "instruction.md"))
	if err != nil || string(contents) != "patched without path\n" {
		t.Fatalf("PATH-independent patch contents = %q, %v", contents, err)
	}
}

func TestLocalPatchProviderAppliesMultipleHunksInOneManagedFile(t *testing.T) {
	checkout := writeLifecycleSnapshot(t, "first\nsecond\nthird\nfourth\nfifth\n")
	provider := LocalPatchProvider{}
	payload, err := provider.ValidatePayload(json.RawMessage(`{"format":"harbor.local-unified-diff.v1","diff":"--- a/instruction.md\n+++ b/instruction.md\n@@ -1,2 +1,2 @@\n first\n-second\n+updated second\n@@ -4,2 +4,3 @@\n fourth\n-fifth\n+updated fifth\n+tail\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Apply(context.Background(), ChangeProviderRequest{Checkout: checkout, Payload: payload}); err != nil {
		t.Fatalf("apply multi-hunk local patch: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(checkout, "instruction.md"))
	if err != nil || string(contents) != "first\nupdated second\nthird\nfourth\nupdated fifth\ntail\n" {
		t.Fatalf("multi-hunk patched contents = %q, %v", contents, err)
	}
}

func TestLocalPatchProviderStaleLaterFileLeavesAllManagedTargetsUnchanged(t *testing.T) {
	checkout := writeLifecycleSnapshot(t, "original instruction\n")
	beforeInstruction, err := os.ReadFile(filepath.Join(checkout, "instruction.md"))
	if err != nil {
		t.Fatal(err)
	}
	beforeTask, err := os.ReadFile(filepath.Join(checkout, "task.toml"))
	if err != nil {
		t.Fatal(err)
	}
	provider := LocalPatchProvider{}
	payload, err := provider.ValidatePayload(json.RawMessage(`{"format":"harbor.local-unified-diff.v1","diff":"--- a/instruction.md\n+++ b/instruction.md\n@@ -1 +1 @@\n-original instruction\n+would have changed\n--- a/task.toml\n+++ b/task.toml\n@@ -1 +1 @@\n-stale task manifest line\n+would have changed too\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Apply(context.Background(), ChangeProviderRequest{Checkout: checkout, Payload: payload}); err == nil {
		t.Fatal("local patch accepted a stale later-file hunk")
	}
	afterInstruction, err := os.ReadFile(filepath.Join(checkout, "instruction.md"))
	if err != nil {
		t.Fatal(err)
	}
	afterTask, err := os.ReadFile(filepath.Join(checkout, "task.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(afterInstruction) != string(beforeInstruction) || string(afterTask) != string(beforeTask) {
		t.Fatalf("stale multi-file patch partially wrote candidate: instruction=%q task=%q", afterInstruction, afterTask)
	}
}

type testRepairAgent struct{}

var _ agent.Runtime = testRepairAgent{}

func (testRepairAgent) OpenConversation(_ context.Context, request agent.ConversationRequest) (agent.Conversation, error) {
	return testRepairConversation{checkout: request.ProjectPath}, nil
}

type testRepairConversation struct{ checkout string }

func (conversation testRepairConversation) Turn(_ context.Context, _ agent.TurnRequest) (agent.TurnResult, error) {
	if err := os.WriteFile(filepath.Join(conversation.checkout, "instruction.md"), []byte("agent repaired instruction\n"), 0o644); err != nil {
		return agent.TurnResult{}, err
	}
	return agent.TurnResult{Text: "repaired", Model: "fake-agent"}, nil
}

func (testRepairConversation) Close() error { return nil }

func TestAgentRepairProviderCreatesBoundRepairSessionInCandidate(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	services, err := newLifecycleServicesForTest(root, database)
	if err != nil {
		t.Fatal(err)
	}
	services.Changes.Register(AgentRepairProvider{Agent: testRepairAgent{}})
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "agent-candidate", Actor: "tester", Reason: "import"},
		SourceDirectory:        writeLifecycleSnapshot(t, "original instruction\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, StartRunRequest{TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCandidateLeaseProfile(t), ExecutionSpec: lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "verify", Actor: "tester", Reason: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := services.Continuations.CurrentCheckpoint(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := services.Continuations.PlanTaskContinuation(ctx, ContinueTaskCommand{
		CommandKey: "agent-change", TaskID: task.ID, RunID: run.ID, Expected: checkpoint, Actor: "tester", Reason: "repair",
		Change: &TaskChangeRequest{ProviderID: AgentRepairProviderID, OperationKey: "agent-operation", MaxRepairRounds: 2,
			Payload:  json.RawMessage(`{"format":"harbor.agent-repair.v1","guidance":"fix it"}`),
			Findings: findingBundleForRun(t, ctx, services, database, run, revision, "fix instruction")},
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := database.GetRevisionCandidate(ctx, plan.Snapshot().CandidateRevisionID)
	if err != nil || candidate == nil || candidate.RepairSessionID == "" || candidate.RoundOrdinal != 1 {
		t.Fatalf("agent candidate = %+v, %v", candidate, err)
	}
	session, err := database.GetRepairSession(ctx, candidate.RepairSessionID)
	if err != nil || session == nil || session.MaxRounds != 2 || session.BaseRevisionID != revision.ID {
		t.Fatalf("repair session = %+v, %v", session, err)
	}
	if _, err := os.Stat(filepath.Join(services.core.layout.candidateCheckoutDirectory(task.ID, candidate.ID), "agent-repair.log")); !os.IsNotExist(err) {
		t.Fatalf("agent log leaked into strict checkout: %v", err)
	}
}

func TestChangeProviderRejectsUntrustedFindingEvidenceBeforeCandidateCreation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	services, err := newLifecycleServicesForTest(root, database)
	if err != nil {
		t.Fatal(err)
	}
	services.Changes.Register(testChangeProvider{})
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "untrusted-evidence", Actor: "tester", Reason: "import"},
		SourceDirectory:        writeLifecycleSnapshot(t, "original instruction\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, StartRunRequest{TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCompleteProfile(t), ExecutionSpec: lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "verify", Actor: "tester", Reason: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := services.Continuations.CurrentCheckpoint(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	findings := findingBundleForRun(t, ctx, services, database, run, revision, "untrusted report")
	findings.Findings[0].ReportContentDigest = string(workflowkit.SHA256Fingerprint([]byte("different report")))
	_, err = services.Continuations.PlanTaskContinuation(ctx, ContinueTaskCommand{
		CommandKey: "untrusted-evidence-command", TaskID: task.ID, RunID: run.ID, Expected: checkpoint, Actor: "tester", Reason: "reject untrusted evidence",
		Change: &TaskChangeRequest{ProviderID: "test_change", OperationKey: "untrusted-evidence-operation", Payload: json.RawMessage(`{"format":"test.change.v1"}`), Findings: findings},
	})
	if err == nil || !strings.Contains(err.Error(), "report artifact does not match") {
		t.Fatalf("untrusted findings error = %v, want immutable report lineage rejection", err)
	}
	operation, err := database.GetChangeOperationByKey(ctx, "untrusted-evidence-operation")
	if err != nil {
		t.Fatal(err)
	}
	if operation != nil {
		t.Fatalf("untrusted evidence must be rejected before creating an operation: %+v", operation)
	}
}

func TestChangeProviderMarksMismatchedDeclaredPathsForReconciliation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	services, err := newLifecycleServicesForTest(root, database)
	if err != nil {
		t.Fatal(err)
	}
	services.Changes.Register(misreportingChangeProvider{})
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "misreported-change", Actor: "tester", Reason: "import"},
		SourceDirectory:        writeLifecycleSnapshot(t, "original instruction\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, StartRunRequest{TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCandidateLeaseProfile(t), ExecutionSpec: lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "verify", Actor: "tester", Reason: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := services.Continuations.CurrentCheckpoint(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = services.Continuations.PlanTaskContinuation(ctx, ContinueTaskCommand{
		CommandKey: "misreported-change-command", TaskID: task.ID, RunID: run.ID, Expected: checkpoint, Actor: "tester", Reason: "reject inconsistent receipt",
		Change: &TaskChangeRequest{ProviderID: "misreporting_change", OperationKey: "misreported-change-operation", Payload: json.RawMessage(`{"format":"test.change.v1"}`),
			Findings: findingBundleForRun(t, ctx, services, database, run, revision, "receipt mismatch")},
	})
	if !errors.Is(err, ErrChangeReconciliationRequired) {
		t.Fatalf("mismatched provider receipt error = %v, want reconciliation required", err)
	}
	operation, err := database.GetChangeOperationByKey(ctx, "misreported-change-operation")
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.State != store.ChangeOperationUnknown {
		t.Fatalf("mismatched provider operation = %+v, want unknown", operation)
	}
}
