package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/workflow"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

type testChangeProvider struct{}

type misreportingChangeProvider struct{}

type sleepingChangeProvider struct {
	delay time.Duration
}

func (provider sleepingChangeProvider) ID() string { return "sleeping_change" }

func (sleepingChangeProvider) ValidatePayload(raw json.RawMessage) (json.RawMessage, error) {
	return raw, nil
}

func (provider sleepingChangeProvider) Apply(_ context.Context, _ ChangeProviderRequest) (ChangeProviderReceipt, error) {
	time.Sleep(provider.delay)
	return ChangeProviderReceipt{Format: "test.sleeping-provider.v1", ProviderID: provider.ID()}, nil
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
	stage, found := workflow.Stage(workflowkit.StageKey(nodes.QualityCheck))
	if !found {
		t.Fatalf("frozen workflow does not contain %q", nodes.QualityCheck)
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
			CheckerID: "quality", StageKey: nodes.QualityCheck, CheckID: "instruction", Severity: "error", Message: message,
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
			CheckerID: "quality", StageKey: nodes.QualityCheck, CheckID: "instruction", Severity: "error", Message: "invalid instruction",
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
		{name: "missing report ID", bundle: FindingBundle{Format: valid.Format, RevisionID: valid.RevisionID, RevisionDigest: valid.RevisionDigest, Findings: []RepairFinding{{CheckerID: "quality", StageKey: nodes.QualityCheck, CheckID: "instruction", Severity: "error", Message: "invalid instruction", ReportContentDigest: valid.Findings[0].ReportContentDigest}}}},
		{name: "noncanonical report digest", bundle: FindingBundle{Format: valid.Format, RevisionID: valid.RevisionID, RevisionDigest: valid.RevisionDigest, Findings: []RepairFinding{{CheckerID: "quality", StageKey: nodes.QualityCheck, CheckID: "instruction", Severity: "error", Message: "invalid instruction", ReportArtifactID: reportID, ReportContentDigest: "sha256:ABC"}}}},
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
	services, err := NewLifecycleServices(root, database)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := database.AcquireLease(ctx, store.AcquireLeaseRequest{ResourceType: "task_revision_candidate", ResourceID: "heartbeat-task", Owner: "heartbeat-owner", TTL: time.Second, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	_, latest, err := services.Changes.applyWithCandidateLeaseHeartbeat(ctx, sleepingChangeProvider{delay: 350 * time.Millisecond}, ChangeProviderRequest{Actor: "tester", Timeout: time.Second}, lease, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version < lease.Version+2 {
		t.Fatalf("latest heartbeat lease version = %d, want at least %d", latest.Version, lease.Version+2)
	}
	if _, err := database.HeartbeatLease(ctx, store.HeartbeatLeaseRequest{LeaseID: latest.ID, Owner: latest.Owner, FencingToken: latest.FencingToken, ExpectedVersion: latest.Version, TTL: time.Second, Actor: "tester", Reason: "prove latest fence"}); err != nil {
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
	services, err := NewLifecycleServices(root, database)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := database.AcquireLease(ctx, store.AcquireLeaseRequest{ResourceType: "task_revision_candidate", ResourceID: "start-heartbeat-task", Owner: "start-heartbeat-owner", TTL: time.Second, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	_, latest, err := services.Changes.applyWithCandidateLeaseHeartbeat(ctx, sleepingChangeProvider{delay: 5 * time.Millisecond}, ChangeProviderRequest{Actor: "tester", Timeout: time.Second}, lease, time.Second)
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
	services, err := NewLifecycleServices(root, database)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := database.AcquireLease(ctx, store.AcquireLeaseRequest{ResourceType: "task_revision_candidate", ResourceID: "budget-task", Owner: "budget-owner", TTL: time.Second, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = services.Changes.applyWithCandidateLeaseHeartbeat(ctx, sleepingChangeProvider{delay: 120 * time.Millisecond}, ChangeProviderRequest{Actor: "tester", Timeout: 30 * time.Millisecond}, lease, time.Second)
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
	services, err := NewLifecycleServices(root, database)
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
	run, err := services.Runs.StartRun(ctx, StartRunRequest{TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCompleteProfile(t), Trigger: "verify", Actor: "tester", Reason: "verify base"})
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
	updatedCandidate, err := database.GetRevisionCandidate(ctx, candidate.ID)
	if err != nil || updatedCandidate == nil || updatedCandidate.State != store.RevisionCandidateCommitted {
		t.Fatalf("candidate commit = %+v, %v", updatedCandidate, err)
	}
	replayed, err := services.Continuations.ExecuteTaskContinuation(ctx, plan.ID())
	if err != nil || replayed.ID != execution.ID {
		t.Fatalf("idempotent candidate execution = %+v, %v", replayed, err)
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
	payload, err := provider.ValidatePayload(json.RawMessage(`{"format":"harbor.local-unified-diff.v1","diff":"--- a/instruction.md\n+++ b/instruction.md\n@@ -1 +1 @@\n-original instruction\n+patched instruction\n"}`))
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

type testRepairAgent struct{}

func (testRepairAgent) OpenConversation(_ context.Context, request workflow.AgentConversationRequest) (workflow.AgentConversation, error) {
	return testRepairConversation{checkout: request.ProjectPath}, nil
}

type testRepairConversation struct{ checkout string }

func (conversation testRepairConversation) Turn(_ context.Context, _ workflow.AgentTurnRequest) (workflow.AgentTurnResult, error) {
	if err := os.WriteFile(filepath.Join(conversation.checkout, "instruction.md"), []byte("agent repaired instruction\n"), 0o644); err != nil {
		return workflow.AgentTurnResult{}, err
	}
	return workflow.AgentTurnResult{Text: "repaired", Model: "fake-agent"}, nil
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
	services, err := NewLifecycleServices(root, database)
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
	run, err := services.Runs.StartRun(ctx, StartRunRequest{TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCompleteProfile(t), Trigger: "verify", Actor: "tester", Reason: "verify"})
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
	services, err := NewLifecycleServices(root, database)
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
	run, err := services.Runs.StartRun(ctx, StartRunRequest{TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCompleteProfile(t), Trigger: "verify", Actor: "tester", Reason: "verify"})
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
	services, err := NewLifecycleServices(root, database)
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
	run, err := services.Runs.StartRun(ctx, StartRunRequest{TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCompleteProfile(t), Trigger: "verify", Actor: "tester", Reason: "verify"})
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
