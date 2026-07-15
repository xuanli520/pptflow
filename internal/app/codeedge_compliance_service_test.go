package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestCodeEdgeComplianceRecordsTrustedEvidenceAndReconcilesTechnicalRetry(t *testing.T) {
	ctx := context.Background()
	fixture := newCodeEdgeComplianceFixture(t, codeEdgeComplianceFixtureOptions{seedFailedQwenTrial: true})

	request := fixture.complianceRequest(t)
	first, err := fixture.services.CodeEdgeCompliance.RecordFinalCompliance(ctx, request)
	if err != nil {
		t.Fatalf("record trusted CodeEdge final compliance: %v", err)
	}
	if first.Record.Status != store.CodeEdgeComplianceApproved || first.Decision.Status != codeedge.FinalComplianceApproved || first.Authorization == nil {
		t.Fatalf("trusted final compliance = %+v, want approved authorization", first)
	}
	if first.Record.ID != request.IdempotencyKey || first.Record.AuthorizationFingerprint == "" {
		t.Fatalf("stored CodeEdge compliance record = %+v", first.Record)
	}

	qwenTrials, err := fixture.database.ListTrialExecutionsForStageAttempt(ctx, fixture.qwenStage.ID)
	if err != nil {
		t.Fatal(err)
	}
	opusTrials, err := fixture.database.ListTrialExecutionsForStageAttempt(ctx, fixture.opusStage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(qwenTrials) != 4 || len(opusTrials) != 4 {
		t.Fatalf("trusted logical trials: qwen=%+v opus=%+v; want four each", qwenTrials, opusTrials)
	}
	for _, trial := range append(append([]store.TrialExecution(nil), qwenTrials...), opusTrials...) {
		if trial.Status != store.TrialExecutionCompleted {
			t.Fatalf("trusted TrialExecution %s status = %s, want completed", trial.ID, trial.Status)
		}
		attempts, attemptErr := fixture.database.ListTrialAttemptsForTrialExecution(ctx, trial.ID)
		if attemptErr != nil || len(attempts) == 0 || attempts[len(attempts)-1].Status != store.TrialAttemptCompleted {
			t.Fatalf("trusted TrialExecution %s attempts = %+v, %v; want a completed final attempt", trial.ID, attempts, attemptErr)
		}
	}
	if qwenTrials[0].ID != fixture.failedQwenTrialID {
		t.Fatalf("technical retry replaced logical TrialExecution: got %s want %s", qwenTrials[0].ID, fixture.failedQwenTrialID)
	}
	qwenAttempts, err := fixture.database.ListTrialAttemptsForTrialExecution(ctx, qwenTrials[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(qwenAttempts) != 2 || qwenAttempts[0].Status != store.TrialAttemptInfraFailed || qwenAttempts[1].Status != store.TrialAttemptCompleted || qwenAttempts[1].RetryOfTrialAttemptID != qwenAttempts[0].ID {
		t.Fatalf("technical retry chain = %+v, want infra_failed followed by completed retry", qwenAttempts)
	}

	persisted, err := fixture.database.GetCodeEdgeComplianceRecordForRun(ctx, fixture.run.ID)
	if err != nil || persisted == nil || persisted.ID != first.Record.ID || persisted.DecisionFingerprint != first.Record.DecisionFingerprint {
		t.Fatalf("persisted CodeEdge compliance record = %+v, %v", persisted, err)
	}
	if replayed, replayErr := fixture.services.CodeEdgeCompliance.RecordFinalCompliance(ctx, request); replayErr != nil || replayed.Record.ID != first.Record.ID {
		t.Fatalf("compliance replay = %+v, %v; want immutable record %s", replayed, replayErr, first.Record.ID)
	}
}

func TestCodeEdgeEvaluatorEvidenceGateRequiresVerifiedAdoptedHandoff(t *testing.T) {
	ctx := context.Background()
	fixture := newCodeEdgeComplianceFixture(t, codeEdgeComplianceFixtureOptions{stopBeforeHandoff: true})
	opened := openCodeEdgeEvaluatorEvidenceReviewGateForDecision(t, ctx, fixture)

	_, err := fixture.services.Reviews.Decide(ctx, DecideReviewRequest{
		ID:                     newCodeEdgeComplianceUUID(t),
		ReviewRequestID:        opened.Review.ID,
		RevisionID:             fixture.revision.ID,
		Action:                 store.ReviewDecisionApprove,
		ExpectedRevisionDigest: fixture.revision.TaskDigest,
		Actor:                  "codeedge-test",
		Reason:                 "attempt approval before adopting evaluator evidence",
	})
	if err == nil || !strings.Contains(err.Error(), "adopted immutable handoff") {
		t.Fatalf("evaluator evidence gate accepted missing handoff: %v", err)
	}
	decisions, err := fixture.database.ListReviewDecisionsForRequest(ctx, opened.Review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 0 {
		t.Fatalf("missing-handoff gate persisted decisions: %+v", decisions)
	}
}

func TestCodeEdgeEvaluatorEvidenceGateBindsVerifiedHandoffIntoDecisionArtifact(t *testing.T) {
	ctx := context.Background()
	fixture := newCodeEdgeComplianceFixture(t, codeEdgeComplianceFixtureOptions{})
	opened := openCodeEdgeEvaluatorEvidenceReviewGateForDecision(t, ctx, fixture)

	decision, err := fixture.services.Reviews.Decide(ctx, DecideReviewRequest{
		ID:                     newCodeEdgeComplianceUUID(t),
		ReviewRequestID:        opened.Review.ID,
		RevisionID:             fixture.revision.ID,
		Action:                 store.ReviewDecisionApprove,
		ExpectedRevisionDigest: fixture.revision.TaskDigest,
		Actor:                  "codeedge-test",
		Reason:                 "approve verified evaluator evidence handoff",
	})
	if err != nil {
		t.Fatalf("approve verified evaluator evidence gate: %v", err)
	}
	if decision.ID == "" {
		t.Fatal("verified evaluator evidence gate returned no decision")
	}
	// The durable resolution worker repeats this verification before it emits
	// its artifact; exercising it here proves a later replay cannot approve an
	// ambient or missing child Run.
	if _, err := fixture.services.core.verifyCodeEdgeEvaluatorEvidenceHandoffGate(ctx, opened.Binding); err != nil {
		t.Fatalf("verified evaluator evidence gate no longer validates: %v", err)
	}
}

func TestLifecycleMutationAdoptsCodeEdgeEvaluatorEvidenceOnlyAfterPreparedConfirmation(t *testing.T) {
	ctx := context.Background()
	fixture := newCodeEdgeComplianceFixture(t, codeEdgeComplianceFixtureOptions{stopBeforeHandoff: true})
	checkpoint, err := fixture.services.Mutations.CaptureCheckpoint(ctx, fixture.task.ID, fixture.revision.ID, fixture.run.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	key := newCodeEdgeComplianceUUID(t)
	command := CodeEdgeEvaluatorEvidenceHandoffCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: key, Actor: "codeedge-test", Reason: "adopt completed CodeEdge evaluator child evidence", Expected: checkpoint,
		},
		ParentRunID: fixture.run.ID,
		ChildRunID:  fixture.childRun.ID,
	}
	if _, err := fixture.services.Mutations.AdoptCodeEdgeEvaluatorEvidenceHandoff(ctx, command); err == nil || !strings.Contains(err.Error(), "must be prepared") {
		t.Fatalf("direct evidence adoption bypassed first confirmation: %v", err)
	}
	prepared, err := fixture.services.Mutations.PrepareCodeEdgeEvaluatorEvidenceHandoff(ctx, command)
	if err != nil {
		t.Fatalf("prepare evaluator evidence adoption: %v", err)
	}
	if prepared.OperationID == "" || prepared.ParentRunID != fixture.run.ID || prepared.ChildRunID != fixture.childRun.ID || prepared.HandoffFingerprint == "" {
		t.Fatalf("prepared evaluator evidence adoption = %+v", prepared)
	}
	if existing, lookupErr := fixture.database.GetCodeEdgeEvaluatorEvidenceHandoffForParentRun(ctx, fixture.run.ID); lookupErr != nil || existing != nil {
		t.Fatalf("prepare created evaluator evidence handoff = %+v, %v", existing, lookupErr)
	}
	receipt, err := fixture.services.Mutations.AdoptCodeEdgeEvaluatorEvidenceHandoff(ctx, command)
	if err != nil {
		t.Fatalf("confirm evaluator evidence adoption: %v", err)
	}
	if receipt.Action != LifecycleMutationCodeEdgeEvaluatorEvidenceHandoff || receipt.EvaluatorEvidenceHandoffID != key || receipt.ParentRunID != fixture.run.ID || receipt.ChildRunID != fixture.childRun.ID || receipt.EvaluatorEvidenceHandoffFingerprint == "" {
		t.Fatalf("evaluator evidence adoption receipt = %+v", receipt)
	}
	replayed, err := fixture.services.Mutations.AdoptCodeEdgeEvaluatorEvidenceHandoff(ctx, command)
	if err != nil || replayed != receipt {
		t.Fatalf("evaluator evidence adoption replay = %+v, %v; want %+v", replayed, err, receipt)
	}
}

func TestCodeEdgeComplianceFailsClosedOnFrozenEvidenceDrift(t *testing.T) {
	tests := []struct {
		name             string
		omitResultReview bool
		verify           func(t *testing.T, fixture *codeEdgeComplianceFixture) error
		want             error
	}{
		{
			name: "catalog",
			verify: func(t *testing.T, fixture *codeEdgeComplianceFixture) error {
				resolver := catalogLockAttestedResolverForSpec(t, fixture.specification, "codeedge-compliance-catalog", "v2", "lock-v1")
				services := catalogLockLifecycleServices(t, fixture.root, fixture.database, resolver)
				_, err := services.core.loadFrozenCodeEdgeRun(context.Background(), fixture.run.ID)
				return err
			},
			want: stageprovider.ErrDeploymentOperationCatalogDrift,
		},
		{
			name: "lock",
			verify: func(t *testing.T, fixture *codeEdgeComplianceFixture) error {
				resolver := catalogLockAttestedResolverForSpec(t, fixture.specification, "codeedge-compliance-catalog", "v1", "lock-v2")
				services := catalogLockLifecycleServices(t, fixture.root, fixture.database, resolver)
				_, err := services.core.loadFrozenCodeEdgeRun(context.Background(), fixture.run.ID)
				return err
			},
			want: stageprovider.ErrDeploymentOperationCatalogLockDrift,
		},
		{
			name: "managed manifest",
			verify: func(t *testing.T, fixture *codeEdgeComplianceFixture) error {
				path := filepath.Join(fixture.root, managedRunsDirectory, fixture.run.ID, "run-manifest.json")
				if err := os.WriteFile(path, []byte(`{"format":"forged"}`), 0o640); err != nil {
					t.Fatal(err)
				}
				_, err := fixture.services.core.loadFrozenCodeEdgeRun(context.Background(), fixture.run.ID)
				return err
			},
		},
		{
			name: "frozen policy",
			verify: func(t *testing.T, fixture *codeEdgeComplianceFixture) error {
				specification := fixture.specification.Clone()
				specification.CodeEdgeFinalCompliancePolicy.Version = "forged-policy-version"
				canonical, err := specification.CanonicalJSON()
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(fixture.root, managedRunsDirectory, fixture.run.ID, runExecutionSpecFileName)
				if err := os.WriteFile(path, canonical, 0o640); err != nil {
					t.Fatal(err)
				}
				_, err = fixture.services.core.loadFrozenCodeEdgeRun(context.Background(), fixture.run.ID)
				return err
			},
		},
		{
			name: "artifact bytes",
			verify: func(t *testing.T, fixture *codeEdgeComplianceFixture) error {
				reference := fixture.stageArtifact(t, fixture.qwenStage, "qwen_trial_result")
				index, err := loadStageArtifactManifestIndex(context.Background(), fixture.database, reference.ManifestID)
				if err != nil {
					t.Fatal(err)
				}
				object, err := index.objectFor(reference)
				if err != nil {
					t.Fatal(err)
				}
				path, err := fixture.services.core.objects.ObjectPath(object)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("forged evaluator result"), 0o600); err != nil {
					t.Fatal(err)
				}
				checker := &CodeEdgeEvaluatorEvidenceHandoffService{core: fixture.services.core}
				_, _, _, err = checker.rebuildChildEvaluationReceipt(context.Background(), fixture.childFrozen, fixture.qwenStage, workflowadapter.HarborRunQwen, "qwen_trial_result", "qwen_pass4_evidence", fixture.frozen.Policy.QwenPolicy)
				return err
			},
		},
		{
			name:             "required result review",
			omitResultReview: true,
			verify: func(_ *testing.T, fixture *codeEdgeComplianceFixture) error {
				return requireApprovedCodeEdgeReviewGate(context.Background(), fixture.database, fixture.run, fixture.revision, workflowadapter.ResultReview, workflowadapter.ReviewModelResult)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCodeEdgeComplianceFixture(t, codeEdgeComplianceFixtureOptions{omitResultReview: test.omitResultReview})
			err := test.verify(t, fixture)
			if err == nil {
				t.Fatal("frozen CodeEdge evidence accepted drifted or incomplete input")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("frozen evidence error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestCodeEdgePackageAuthorizationFailsClosedWithoutAnApprovedRecord(t *testing.T) {
	ctx := context.Background()
	fixture := newCodeEdgeComplianceFixture(t, codeEdgeComplianceFixtureOptions{packageableRevision: true})

	base := PackageRevisionRequest{
		RevisionID: fixture.revision.ID, ExpectedStateVersion: fixture.revision.StateVersion,
		Actor: "codeedge-test", Reason: "exercise CodeEdge package authorization",
	}
	for _, test := range []struct {
		name    string
		request PackageRevisionRequest
	}{
		{
			name:    "missing selected authorization",
			request: PackageRevisionRequest{ReleaseVersion: "codeedge-missing-auth", IdempotencyKey: newCodeEdgeComplianceUUID(t)},
		},
		{
			name: "fabricated compliance record",
			request: PackageRevisionRequest{
				ReleaseVersion: "codeedge-wrong-record", IdempotencyKey: newCodeEdgeComplianceUUID(t), RunID: fixture.run.ID,
				ExpectedComplianceRecordID: newCodeEdgeComplianceUUID(t), ExpectedAuthorizationFingerprint: string(workflowkit.SHA256Fingerprint([]byte("fabricated authorization"))),
			},
		},
		{
			name: "wrong authorization fingerprint",
			request: PackageRevisionRequest{
				ReleaseVersion: "codeedge-wrong-fingerprint", IdempotencyKey: newCodeEdgeComplianceUUID(t), RunID: fixture.run.ID,
				ExpectedComplianceRecordID: newCodeEdgeComplianceUUID(t), ExpectedAuthorizationFingerprint: string(workflowkit.SHA256Fingerprint([]byte("wrong authorization"))),
			},
		},
		{
			name: "wrong run",
			request: PackageRevisionRequest{
				ReleaseVersion: "codeedge-wrong-run", IdempotencyKey: newCodeEdgeComplianceUUID(t), RunID: newCodeEdgeComplianceUUID(t),
				ExpectedComplianceRecordID: newCodeEdgeComplianceUUID(t), ExpectedAuthorizationFingerprint: string(workflowkit.SHA256Fingerprint([]byte("wrong run authorization"))),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.ReleaseVersion = test.request.ReleaseVersion
			request.IdempotencyKey = test.request.IdempotencyKey
			request.RunID = test.request.RunID
			request.ExpectedComplianceRecordID = test.request.ExpectedComplianceRecordID
			request.ExpectedAuthorizationFingerprint = test.request.ExpectedAuthorizationFingerprint
			if _, packageErr := fixture.services.Releases.PackageRevision(ctx, request); packageErr == nil {
				t.Fatal("CodeEdge package request bypassed immutable authorization")
			}
			if _, statErr := os.Stat(fixture.services.core.layout.releaseDirectory(request.ReleaseVersion)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("rejected package request created output directory: %v", statErr)
			}
		})
	}
}

func TestCodeEdgePackageAuthorizationPinsApprovedReceiptAndReplay(t *testing.T) {
	ctx := context.Background()
	fixture := newCodeEdgeComplianceFixture(t, codeEdgeComplianceFixtureOptions{packageableRevision: true})
	compliance, err := fixture.services.CodeEdgeCompliance.RecordFinalCompliance(ctx, fixture.complianceRequest(t))
	if err != nil || compliance.Authorization == nil {
		t.Fatalf("create approved CodeEdge compliance record = %+v, %v", compliance, err)
	}

	request := PackageRevisionRequest{
		RevisionID: fixture.revision.ID, ExpectedStateVersion: fixture.revision.StateVersion, ReleaseVersion: "codeedge-approved-v1",
		RunID: fixture.run.ID, ExpectedComplianceRecordID: compliance.Record.ID, ExpectedAuthorizationFingerprint: compliance.Record.AuthorizationFingerprint,
		IdempotencyKey: newCodeEdgeComplianceUUID(t), Actor: "codeedge-test", Reason: "package approved CodeEdge revision",
	}
	packaged, err := fixture.services.Releases.PackageRevision(ctx, request)
	if err != nil {
		t.Fatalf("package approved CodeEdge revision: %v", err)
	}
	if packaged.PackagePath == "" || packaged.ReceiptPath == "" {
		t.Fatalf("approved local package result = %+v", packaged)
	}
	var receipt localPackageReceipt
	raw, err := os.ReadFile(packaged.ReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.CodeEdge == nil || receipt.CodeEdge.ComplianceRecordID != compliance.Record.ID || receipt.CodeEdge.RunID != fixture.run.ID || receipt.CodeEdge.AuthorizationFingerprint != workflowkit.Fingerprint(compliance.Record.AuthorizationFingerprint) {
		t.Fatalf("package receipt did not pin approved CodeEdge authorization: %+v", receipt.CodeEdge)
	}
	if err := receipt.CodeEdge.Validate(); err != nil {
		t.Fatalf("package receipt CodeEdge authorization is invalid: %v", err)
	}

	replayed, err := fixture.services.Releases.PackageRevision(ctx, request)
	if err != nil || replayed.Release.ID != packaged.Release.ID || replayed.PackagePath != packaged.PackagePath {
		t.Fatalf("approved package replay = %+v, %v; want immutable package %+v", replayed, err, packaged)
	}
	wrongReplay := request
	wrongReplay.ExpectedAuthorizationFingerprint = string(workflowkit.SHA256Fingerprint([]byte("replay must retain original authorization")))
	if _, replayErr := fixture.services.Releases.PackageRevision(ctx, wrongReplay); replayErr == nil {
		t.Fatal("package replay accepted a different CodeEdge authorization fingerprint")
	}
}

type codeEdgeComplianceFixtureOptions struct {
	omitResultReview    bool
	packageableRevision bool
	seedFailedQwenTrial bool
	stopBeforeHandoff   bool
}

type codeEdgeComplianceFixture struct {
	root          string
	database      *store.Store
	services      *LifecycleServices
	task          store.TaskV2
	revision      store.TaskRevision
	run           store.WorkflowRun
	specification workflowadapter.RunExecutionSpec
	frozen        frozenCodeEdgeRun
	childRun      store.WorkflowRun
	childFrozen   frozenCodeEdgeRun
	runtimeRun    store.WorkflowRun
	qwenStage     store.StageAttempt
	opusStage     store.StageAttempt
	qwen          codeedge.EvaluationReceipt
	opus          codeedge.EvaluationReceipt
	handoff       store.CodeEdgeEvaluatorEvidenceHandoff
	submission    codeedge.SubmissionCheckReceipt

	failedQwenTrialID string
}

func newCodeEdgeComplianceFixture(t *testing.T, options codeEdgeComplianceFixtureOptions) *codeEdgeComplianceFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	bootstrap, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{OperationResolver: testsupport.AcceptAllStageOperationResolver()})
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := bootstrap.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "codeedge-compliance", Title: "CodeEdge Compliance", Actor: "codeedge-test", Reason: "create trusted fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "CodeEdge compliance fixture\n"),
		ChangeSummary:          "create immutable CodeEdge compliance fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	specification := testsupport.CompleteCodeEdgePhase1RunExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	parentResolver := catalogLockAttestedResolverForSpec(t, specification, "codeedge-compliance-catalog", "v1", "lock-v1")
	parentServices := catalogLockLifecycleServices(t, root, database, parentResolver)
	run, err := parentServices.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: codeEdgePhase1RuntimeProfile(t), ExecutionSpec: specification,
		Trigger: "codeedge-final-compliance", Actor: "codeedge-test", Reason: "freeze trusted CodeEdge Run",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "codeedge-test", Reason: "materialize trusted evidence"})
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := parentServices.core.loadFrozenCodeEdgeRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	run = seedApprovedCodeEdgeReviewGate(t, ctx, parentServices, run, revision, workflowadapter.FinalReview, workflowadapter.ReviewFinalQuality)
	run, err = database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "codeedge-test", Reason: "continue after final review"})
	if err != nil {
		t.Fatal(err)
	}

	childSpecification := testsupport.CompleteCodeEdgeEvaluatorChildRunExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	childResolver := catalogLockAttestedResolverForSpec(t, childSpecification, "codeedge-evaluator-child-catalog", "v1", "lock-v1")
	services := catalogLockLifecycleServices(t, root, database, childResolver)
	childRun, childFrozen := startCodeEdgeEvaluatorFixtureRun(t, ctx, services, run, revision, childSpecification, "complete evaluator evidence")
	qwenStage, qwen := seedCodeEdgeEvaluationStage(t, ctx, services, childRun, revision, childFrozen, workflowadapter.HarborRunQwen, frozen.Policy.QwenPolicy)
	opusStage, opus := seedCodeEdgeEvaluationStage(t, ctx, services, childRun, revision, childFrozen, workflowadapter.HarborRunOpus, frozen.Policy.OpusPolicy)
	seedPreallocatedCodeEdgeTrialSet(t, ctx, database, childRun, qwenStage)
	seedPreallocatedCodeEdgeTrialSet(t, ctx, database, childRun, opusStage)

	fixture := &codeEdgeComplianceFixture{
		root: root, database: database, services: services, task: task, revision: revision, run: run, specification: specification,
		frozen: frozen, childRun: childRun, childFrozen: childFrozen, qwenStage: qwenStage, opusStage: opusStage, qwen: qwen, opus: opus,
	}
	if options.seedFailedQwenTrial {
		fixture.failedQwenTrialID = seedFailedCodeEdgeQwenTrial(t, ctx, database, childRun, qwenStage)
	}
	projector := &CodeEdgeComplianceService{core: services.core}
	if err := projector.completeTrustedTrialSet(ctx, childRun, qwenStage, codeEdgeEvaluatorTrialCount, "codeedge-test", "complete trusted Qwen child trials"); err != nil {
		t.Fatal(err)
	}
	if err := projector.completeTrustedTrialSet(ctx, childRun, opusStage, codeEdgeEvaluatorTrialCount, "codeedge-test", "complete trusted Opus child trials"); err != nil {
		t.Fatal(err)
	}
	childRun, err = database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: childRun.ID, ExpectedVersion: childRun.Version, Status: store.WorkflowRunSucceeded, Actor: "codeedge-test", Reason: "complete trusted evaluator child evidence"})
	if err != nil {
		t.Fatal(err)
	}
	fixture.childRun = childRun
	if options.stopBeforeHandoff {
		return fixture
	}
	handoffID := newCodeEdgeComplianceUUID(t)
	handoff, err := (&CodeEdgeEvaluatorEvidenceHandoffService{core: services.core}).Record(ctx, RecordCodeEdgeEvaluatorEvidenceHandoffRequest{
		ID: handoffID, IdempotencyKey: handoffID, ParentRunID: run.ID, ChildRunID: childRun.ID,
		Actor: "codeedge-test", Reason: "adopt trusted evaluator child evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.handoff = handoff
	run = seedApprovedCodeEdgeReviewGate(t, ctx, services, run, revision, workflowadapter.EvaluatorEvidenceHandoff, workflowadapter.ReviewEvaluatorEvidence)
	run, err = database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning,
		Actor: "codeedge-test", Reason: "continue after evaluator evidence handoff review",
	})
	if err != nil {
		t.Fatal(err)
	}
	submission := seedCodeEdgeSubmissionStage(t, ctx, services, run, revision, frozen)
	fixture.submission = submission
	if !options.omitResultReview {
		run = seedApprovedCodeEdgeReviewGate(t, ctx, services, run, revision, workflowadapter.ResultReview, workflowadapter.ReviewModelResult)
	}
	fixture.run = run
	fixture.runtimeRun, _ = startCodeEdgeEvaluatorFixtureRun(t, ctx, services, run, revision, childSpecification, "runtime evaluator exercise")
	if options.packageableRevision {
		fixture.makeRevisionPackageable(t, ctx)
	}
	return fixture
}

func openCodeEdgeEvaluatorEvidenceReviewGateForDecision(t *testing.T, ctx context.Context, fixture *codeEdgeComplianceFixture) store.ReviewGateOpenResult {
	t.Helper()
	if fixture.run.Status != store.WorkflowRunRunning {
		run, err := fixture.database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
			RunID: fixture.run.ID, ExpectedVersion: fixture.run.Version, Status: store.WorkflowRunRunning,
			Actor: "codeedge-test", Reason: "open independent evaluator evidence gate fixture",
		})
		if err != nil {
			t.Fatal(err)
		}
		fixture.run = run
	}
	workflow, err := decodeFrozenRunDefinition(fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, found := workflow.Workflow.Stage(workflowkit.StageKey(workflowadapter.EvaluatorEvidenceHandoff))
	if !found {
		t.Fatal("frozen workflow has no evaluator evidence handoff gate")
	}
	inputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := fixture.database.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: fixture.run.ID, StageKey: workflowadapter.EvaluatorEvidenceHandoff, StageGroup: descriptor.Group, Ordinal: 99,
		InputFingerprint: string(inputs), BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "codeedge-test", Reason: "open evaluator evidence gate through review service",
	})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := fixture.database.OpenReviewGate(ctx, store.OpenReviewGateRequest{
		RunID: fixture.run.ID, ExpectedRunVersion: fixture.run.Version, RevisionID: fixture.revision.ID, RevisionDigest: fixture.revision.TaskDigest,
		DefinitionHash: fixture.run.DefinitionHash, StageAttemptID: stage.ID, ExpectedStageAttemptVersion: stage.Version,
		StageKey: workflowadapter.EvaluatorEvidenceHandoff, ReviewKind: string(workflowadapter.ReviewEvaluatorEvidence),
		NodeGeneration: 9, NodeAttempt: 1, InputBindingsJSON: `[]`, InputFingerprint: string(inputs),
		EvidenceManifestDigest: "sha256:codeedge-evaluator-evidence-review-service-test", Actor: "codeedge-test", Reason: "open evaluator evidence gate",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.run = opened.Run
	return opened
}

// Race instrumentation makes the evaluator's durable preallocation and fence
// setup exceed the generic one-second fixture budget. Keep this scoped to the
// two external evaluator stages: it exercises the same production policy
// shape without converting a scheduler-timing artifact into an interruption.
func codeEdgePhase1RuntimeProfile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	profile := lifecycleCompleteProfileForTemplate(t, workflowadapter.CodeEdgePhase1WorkflowTemplate())
	return profile
}

func codeEdgeEvaluatorRuntimeProfile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	profile := lifecycleCompleteProfileForTemplate(t, workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplate())
	for index := range profile.Stages {
		if profile.Stages[index].StageKey != workflowkit.StageKey(workflowadapter.HarborRunQwen) && profile.Stages[index].StageKey != workflowkit.StageKey(workflowadapter.HarborRunOpus) {
			continue
		}
		profile.Stages[index].Budget.TurnTimeout = 20 * time.Second
		profile.Stages[index].Budget.AttemptTimeout = 20 * time.Second
		profile.Stages[index].Budget.MaxElapsed = 20 * time.Second
	}
	return profile
}

func startCodeEdgeEvaluatorFixtureRun(t *testing.T, ctx context.Context, services *LifecycleServices, parent store.WorkflowRun, revision store.TaskRevision, specification workflowadapter.RunExecutionSpec, reason string) (store.WorkflowRun, frozenCodeEdgeRun) {
	t.Helper()
	child, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: parent.TaskID, RevisionID: revision.ID, ParentRunID: parent.ID,
		Profile: codeEdgeEvaluatorRuntimeProfile(t), ExecutionSpec: specification,
		Trigger: "codeedge-evaluator-fixture", Actor: "codeedge-test", Reason: reason,
	})
	if err != nil {
		t.Fatalf("start evaluator child fixture: %v", err)
	}
	child, err = services.core.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: child.ID, ExpectedVersion: child.Version, Status: store.WorkflowRunRunning, Actor: "codeedge-test", Reason: "run evaluator child fixture",
	})
	if err != nil {
		t.Fatalf("transition evaluator child fixture to running: %v", err)
	}
	manifest, manifestFingerprint, err := services.core.verifyManagedRunManifestForTemplate(child, workflowadapter.CodeEdgeEvaluatorChildTemplateReference(), "CodeEdge evaluator child fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := services.core.verifyPersistedCodeEdgeCatalogProof(child, manifest, workflowadapter.CodeEdgeEvaluatorChildTemplateReference()); err != nil {
		t.Fatal(err)
	}
	catalogRaw, err := canonicalManifestDeploymentCatalogReceipt(manifest)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := stageprovider.ParseDeploymentOperationCatalogReceiptJSON(catalogRaw)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := canonicalManifestDeploymentCatalogLockIdentity(manifest)
	if err != nil || lock == nil {
		t.Fatalf("read evaluator child catalog lock = %+v, %v", lock, err)
	}
	binding := codeedge.FrozenRunBinding{
		TaskSnapshotDigest: workflowkit.SubjectDigest(revision.TaskDigest), CatalogFingerprint: receipt.CatalogFingerprint,
		LockFingerprint: lock.Fingerprint, ManifestFingerprint: manifestFingerprint,
	}
	if err := binding.Validate(); err != nil {
		t.Fatal(err)
	}
	return child, frozenCodeEdgeRun{Run: child, Revision: revision, Binding: binding}
}

func (fixture *codeEdgeComplianceFixture) complianceRequest(t *testing.T) RecordCodeEdgeFinalComplianceRequest {
	t.Helper()
	id := newCodeEdgeComplianceUUID(t)
	return RecordCodeEdgeFinalComplianceRequest{
		ID: id, IdempotencyKey: id, RunID: fixture.run.ID, EvaluatorEvidenceHandoffID: fixture.handoff.ID,
		Submission: fixture.submission.Clone(),
		Actor:      "codeedge-test", Reason: "record trusted final compliance",
	}
}

func (fixture *codeEdgeComplianceFixture) stageArtifact(t *testing.T, stage store.StageAttempt, key string) store.ArtifactRef {
	t.Helper()
	references, err := fixture.database.ListArtifactRefs(context.Background(), stage.ArtifactManifestID)
	if err != nil {
		t.Fatal(err)
	}
	for _, reference := range references {
		if reference.ArtifactKey == key {
			return reference
		}
	}
	t.Fatalf("stage %s has no %q artifact in %+v", stage.ID, key, references)
	return store.ArtifactRef{}
}

func (fixture *codeEdgeComplianceFixture) makeRevisionPackageable(t *testing.T, ctx context.Context) {
	t.Helper()
	validated, err := fixture.services.Revisions.MarkValidated(ctx, fixture.revision.ID, fixture.revision.StateVersion, "sha256:codeedge-validation-evidence", "codeedge-test", "validate CodeEdge package fixture")
	if err != nil {
		t.Fatal(err)
	}
	review, err := fixture.services.Reviews.Request(ctx, validated.ID, "sha256:codeedge-validation-evidence", "codeedge-test", "approve package fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.services.Reviews.Decide(ctx, DecideReviewRequest{
		ReviewRequestID: review.ID, RevisionID: validated.ID, Action: store.ReviewDecisionApprove, ExpectedRevisionDigest: validated.TaskDigest,
		Actor: "codeedge-test", Reason: "approve immutable revision for package fixture",
	}); err != nil {
		t.Fatal(err)
	}
	promoted, err := fixture.services.Reviews.PromoteCurrent(ctx, fixture.task.ID, validated.ID, fixture.task.Version, "codeedge-test", "promote package fixture")
	if err != nil {
		t.Fatal(err)
	}
	fixture.revision = validated
	fixture.task = promoted
}

func seedCodeEdgeEvaluationStage(t *testing.T, ctx context.Context, services *LifecycleServices, run store.WorkflowRun, revision store.TaskRevision, frozen frozenCodeEdgeRun, key string, policy codeedge.EvaluationPolicy) (store.StageAttempt, codeedge.EvaluationReceipt) {
	t.Helper()
	workflow, err := decodeFrozenRunDefinition(run)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, found := workflow.Workflow.Stage(workflowkit.StageKey(key))
	if !found {
		t.Fatalf("frozen workflow has no CodeEdge evaluator stage %q", key)
	}
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := services.core.store.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: key, StageGroup: descriptor.Group, Ordinal: 1, InputFingerprint: string(emptyInputs),
		BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "codeedge-test", Reason: "persist trusted evaluator evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err = services.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "codeedge-test", Reason: "run evaluator evidence fixture"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := services.core.store.CreateNodeAttempt(ctx, store.CreateNodeAttemptRequest{StageAttemptID: stage.ID, NodeID: key, Generation: 0, Attempt: 1, IdempotencyKey: "codeedge-evaluator-node:" + stage.ID, Actor: "codeedge-test", Reason: "persist evaluator evidence"})
	if err != nil {
		t.Fatal(err)
	}
	node, err = services.core.store.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: store.NodeAttemptRunning, Actor: "codeedge-test", Reason: "run evaluator evidence fixture"})
	if err != nil {
		t.Fatal(err)
	}

	taskRoot := services.core.layout.snapshotDirectory(revision.TaskID, revision.ID)
	bundle := codeEdgeHarborRunBundleBytes(t, taskRoot, frozen.Binding.TaskSnapshotDigest, policy, "harbor-job-"+key)
	screenshot := codeEdgePNG(t)
	artifacts := make([]StageArtifact, 0, len(descriptor.Outputs))
	for _, output := range descriptor.Outputs {
		content := screenshot
		if output.Name == evaluatorResultArtifactKey(key) {
			content = bundle
		}
		artifacts = append(artifacts, StageArtifact{Key: output.Name, SchemaVersion: output.SchemaVersion, Content: content})
	}
	manifest, references, err := persistStageArtifacts(ctx, services.core, run, revision, stage, node, descriptor, nil, artifacts, "codeedge-test", "persist trusted evaluator artifacts")
	if err != nil {
		t.Fatal(err)
	}
	stage, err = services.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictPass, ArtifactManifestID: manifest.ID,
		Actor: "codeedge-test", Reason: "complete trusted evaluator evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.core.store.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: store.NodeAttemptCompleted, Actor: "codeedge-test", Reason: "complete trusted evaluator node"}); err != nil {
		t.Fatal(err)
	}

	byKey := make(map[string]store.ArtifactRef, len(references))
	for _, reference := range references {
		byKey[reference.ArtifactKey] = reference
	}
	resultRef, resultOK := byKey[evaluatorResultArtifactKey(key)]
	screenshotRef, screenshotOK := byKey[evaluatorScreenshotArtifactKey(key)]
	if !resultOK || !screenshotOK {
		t.Fatalf("evaluator refs = %+v, want result and screenshot", references)
	}
	receipt, err := codeedge.BuildEvaluationReceipt(codeedge.EvaluationInput{
		Policy: policy,
		Binding: codeedge.EvaluationBinding{
			TaskSnapshotDigest: frozen.Binding.TaskSnapshotDigest,
			CatalogFingerprint: frozen.Binding.CatalogFingerprint, LockFingerprint: frozen.Binding.LockFingerprint, ManifestFingerprint: frozen.Binding.ManifestFingerprint,
		},
		HarborRunBundle:     codeedge.EvaluationEvidence{ArtifactID: workflowkit.ArtifactID(resultRef.ID), ContentDigest: workflowkit.Fingerprint(resultRef.ContentDigest), SchemaVersion: resultRef.SchemaVersion, MediaType: "application/json", Bytes: bundle},
		CanonicalScreenshot: codeedge.EvaluationEvidence{ArtifactID: workflowkit.ArtifactID(screenshotRef.ID), ContentDigest: workflowkit.Fingerprint(screenshotRef.ContentDigest), SchemaVersion: screenshotRef.SchemaVersion, MediaType: "image/png", Bytes: screenshot},
	})
	if err != nil {
		t.Fatal(err)
	}
	return stage, receipt
}

func seedCodeEdgeSubmissionStage(t *testing.T, ctx context.Context, services *LifecycleServices, run store.WorkflowRun, revision store.TaskRevision, frozen frozenCodeEdgeRun) codeedge.SubmissionCheckReceipt {
	t.Helper()
	workflow, err := decodeFrozenRunDefinition(run)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, found := workflow.Workflow.Stage(workflowkit.StageKey(workflowadapter.SubmissionLint))
	if !found {
		t.Fatal("frozen workflow has no submission_lint stage")
	}
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := services.core.store.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: workflowadapter.SubmissionLint, StageGroup: descriptor.Group, Ordinal: 1, InputFingerprint: string(emptyInputs),
		BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "codeedge-test", Reason: "persist trusted submission report",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err = services.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "codeedge-test", Reason: "run submission fixture"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := services.core.store.CreateNodeAttempt(ctx, store.CreateNodeAttemptRequest{StageAttemptID: stage.ID, NodeID: workflowadapter.SubmissionLint, Generation: 0, Attempt: 1, IdempotencyKey: "codeedge-submission-node:" + stage.ID, Actor: "codeedge-test", Reason: "persist submission report"})
	if err != nil {
		t.Fatal(err)
	}
	node, err = services.core.store.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: store.NodeAttemptRunning, Actor: "codeedge-test", Reason: "run submission fixture"})
	if err != nil {
		t.Fatal(err)
	}
	content := []byte(`{"status":"passed","findings":[]}`)
	if len(descriptor.Outputs) != 1 || descriptor.Outputs[0].Name != "submission_lint_report" {
		t.Fatalf("frozen submission_lint descriptor = %+v", descriptor)
	}
	manifest, references, err := persistStageArtifacts(ctx, services.core, run, revision, stage, node, descriptor, nil, []StageArtifact{{Key: descriptor.Outputs[0].Name, SchemaVersion: descriptor.Outputs[0].SchemaVersion, Content: content}}, "codeedge-test", "persist trusted submission report")
	if err != nil {
		t.Fatal(err)
	}
	stage, err = services.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictPass, ArtifactManifestID: manifest.ID,
		Actor: "codeedge-test", Reason: "complete trusted submission report",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.core.store.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: store.NodeAttemptCompleted, Actor: "codeedge-test", Reason: "complete submission node"}); err != nil {
		t.Fatal(err)
	}
	if len(references) != 1 {
		t.Fatalf("submission refs = %+v, want one report", references)
	}
	return codeedge.SubmissionCheckReceipt{
		Format: codeedge.SubmissionCheckReceiptFormat, Version: codeedge.SubmissionCheckReceiptVersion, Status: codeedge.SubmissionCheckPassed,
		CheckerID: frozen.Policy.SubmissionCheckerID, CheckerVersion: frozen.Policy.SubmissionCheckerVersion, Binding: frozen.Binding,
		Report:   workflowkit.ArtifactBinding{Name: "submission_lint_report", ArtifactID: workflowkit.ArtifactID(references[0].ID), ContentDigest: workflowkit.Fingerprint(references[0].ContentDigest), SchemaVersion: references[0].SchemaVersion},
		Findings: []string{},
	}
}

func seedApprovedCodeEdgeReviewGate(t *testing.T, ctx context.Context, services *LifecycleServices, run store.WorkflowRun, revision store.TaskRevision, key string, kind workflowadapter.ReviewKind) store.WorkflowRun {
	t.Helper()
	workflow, err := decodeFrozenRunDefinition(run)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, found := workflow.Workflow.Stage(workflowkit.StageKey(key))
	if !found || len(descriptor.Outputs) != 1 {
		t.Fatalf("frozen review gate %q descriptor = %+v", key, descriptor)
	}
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := services.core.store.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: key, StageGroup: descriptor.Group, Ordinal: 1, InputFingerprint: string(emptyInputs),
		BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "codeedge-test", Reason: "open trusted CodeEdge review gate",
	})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := services.core.store.OpenReviewGate(ctx, store.OpenReviewGateRequest{
		RunID: run.ID, ExpectedRunVersion: run.Version, RevisionID: revision.ID, RevisionDigest: revision.TaskDigest, DefinitionHash: run.DefinitionHash,
		StageAttemptID: stage.ID, ExpectedStageAttemptVersion: stage.Version, StageKey: key, ReviewKind: string(kind), NodeGeneration: 0, NodeAttempt: 1,
		InputBindingsJSON: `[]`, InputFingerprint: string(emptyInputs), EvidenceManifestDigest: "sha256:codeedge-" + key + "-evidence",
		Actor: "codeedge-test", Reason: "open trusted CodeEdge review gate",
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := services.core.store.RecordReviewGateDecision(ctx, store.RecordReviewGateDecisionRequest{
		ReviewRequestID: opened.Review.ID, RunID: run.ID, RevisionID: revision.ID, StageAttemptID: opened.StageAttempt.ID, ExpectedRevisionDigest: revision.TaskDigest,
		ExpectedRunVersion: opened.Run.Version, ExpectedStageAttemptVersion: opened.StageAttempt.Version, Action: store.ReviewDecisionApprove,
		ResolutionPayloadJSON: `{}`, Actor: "codeedge-test", Reason: "approve trusted CodeEdge review gate",
	})
	if err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(reviewGateDecisionArtifact{
		Format: reviewGateDecisionArtifactFormat, ReviewRequestID: opened.Review.ID, ReviewDecisionID: decision.Decision.ID, Action: store.ReviewDecisionApprove,
		RevisionID: revision.ID, RevisionDigest: revision.TaskDigest, ReviewKind: string(kind), EvidenceManifestDigest: opened.Binding.EvidenceManifestDigest,
		InputFingerprint: opened.Binding.InputFingerprint, DecisionActor: decision.Decision.Actor, DecisionReason: decision.Decision.Reason,
	})
	if err != nil {
		t.Fatal(err)
	}
	emptyBindings := []workflowkit.ArtifactBinding{}
	manifest, _, err := persistStageArtifacts(ctx, services.core, run, revision, opened.StageAttempt, opened.NodeAttempt, descriptor, emptyBindings, []StageArtifact{{
		Key: descriptor.Outputs[0].Name, SchemaVersion: descriptor.Outputs[0].SchemaVersion, Content: content,
	}}, "codeedge-test", "persist trusted CodeEdge review decision")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := services.core.store.CompleteReviewGateResolution(ctx, store.CompleteReviewGateResolutionRequest{
		ReviewRequestID: opened.Review.ID, ReviewDecisionID: decision.Decision.ID, RunID: run.ID, StageAttemptID: opened.StageAttempt.ID,
		ExpectedRunVersion: opened.Run.Version, ExpectedStageAttemptVersion: opened.StageAttempt.Version, ExpectedNodeAttemptVersion: opened.NodeAttempt.Version,
		ArtifactManifestID: manifest.ID, Actor: "codeedge-test", Reason: "complete trusted CodeEdge review gate",
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := services.core.store.GetWorkflowRun(ctx, resolved.Run.ID)
	if err != nil || updated == nil {
		t.Fatalf("read resolved CodeEdge review gate run = %+v, %v", updated, err)
	}
	return *updated
}

func seedFailedCodeEdgeQwenTrial(t *testing.T, ctx context.Context, database *store.Store, run store.WorkflowRun, stage store.StageAttempt) string {
	t.Helper()
	execution, err := database.GetTrialExecutionForStageAttempt(ctx, stage.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if execution == nil || execution.RunID != run.ID || execution.StageAttemptID != stage.ID || execution.StageKey != stage.StageKey || execution.Status != store.TrialExecutionRunning {
		t.Fatalf("missing running preallocated Qwen TrialExecution: %+v", execution)
	}
	attempts, err := database.ListTrialAttemptsForTrialExecution(ctx, execution.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Ordinal != 1 || attempts[0].Status != store.TrialAttemptRunning {
		t.Fatalf("preallocated Qwen TrialAttempt = %+v, %v", attempts, err)
	}
	if _, err := database.TransitionTrialAttempt(ctx, store.TransitionTrialAttemptRequest{TrialAttemptID: attempts[0].ID, ExpectedVersion: attempts[0].Version, Status: store.TrialAttemptInfraFailed, FailureClass: "network", ErrorText: "temporary evaluator transport failure", Actor: "codeedge-test", Reason: "record technical failure fixture"}); err != nil {
		t.Fatal(err)
	}
	return execution.ID
}

func seedPreallocatedCodeEdgeTrialSet(t *testing.T, ctx context.Context, database *store.Store, run store.WorkflowRun, stage store.StageAttempt) {
	t.Helper()
	for ordinal := 1; ordinal <= codeEdgeEvaluatorTrialCount; ordinal++ {
		execution, err := database.CreateTrialExecution(ctx, store.CreateTrialExecutionRequest{
			ID: newCodeEdgeComplianceUUID(t), RunID: run.ID, StageAttemptID: stage.ID, StageKey: stage.StageKey, Ordinal: ordinal,
			Actor: "codeedge-test", Reason: "seed runtime-preallocated CodeEdge logical trial",
		})
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := database.CreateTrialAttempt(ctx, store.CreateTrialAttemptRequest{
			ID: newCodeEdgeComplianceUUID(t), TrialExecutionID: execution.ID, Ordinal: 1,
			Actor: "codeedge-test", Reason: "seed runtime-preallocated CodeEdge initial technical trial attempt",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.TransitionTrialAttempt(ctx, store.TransitionTrialAttemptRequest{
			TrialAttemptID: attempt.ID, ExpectedVersion: attempt.Version, Status: store.TrialAttemptRunning,
			Actor: "codeedge-test", Reason: "seed runtime-preallocated CodeEdge technical trial running",
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func evaluatorResultArtifactKey(stageKey string) string {
	if stageKey == workflowadapter.HarborRunQwen {
		return "qwen_trial_result"
	}
	return "opus_trial_result"
}

func evaluatorScreenshotArtifactKey(stageKey string) string {
	if stageKey == workflowadapter.HarborRunQwen {
		return "qwen_pass4_evidence"
	}
	return "opus_pass4_evidence"
}

func codeEdgeHarborRunBundleBytes(t *testing.T, taskRoot string, digest workflowkit.SubjectDigest, policy codeedge.EvaluationPolicy, jobID string) []byte {
	t.Helper()
	jobRoot := filepath.Join(t.TempDir(), "harbor-job")
	codeEdgeWriteHarborBundleJSON(t, filepath.Join(jobRoot, "config.json"), map[string]any{
		"n_attempts":          4,
		"n_concurrent_trials": 1,
		"tasks":               []any{map[string]any{"path": taskRoot}},
		"datasets":            []any{},
		"agents":              []any{map[string]any{"name": policy.Evaluator.AgentName, "model_name": policy.Evaluator.ModelName}},
	})
	codeEdgeWriteHarborBundleJSON(t, filepath.Join(jobRoot, "lock.json"), map[string]any{
		"schema_version":      2,
		"harbor":              map[string]any{"version": "0.18.0"},
		"n_concurrent_trials": 1,
		"retry":               map[string]any{"max_retries": 3},
		"trials":              []any{},
	})
	codeEdgeWriteHarborBundleJSON(t, filepath.Join(jobRoot, "result.json"), map[string]any{
		"id": jobID, "started_at": "2026-07-14T00:00:00Z", "finished_at": "2026-07-14T00:10:00Z", "n_total_trials": 4,
		"stats": map[string]any{
			"n_running_trials": 0,
			"n_pending_trials": 0,
			"n_retries":        0,
			"evals": map[string]any{
				policy.Evaluator.AgentName + "__" + policy.Evaluator.ModelName + "__adhoc": map[string]any{"pass_at_k": map[string]any{"4": 1}},
			},
		},
	})
	lockDigest := "sha256:" + strings.Repeat("d", 64)
	for index := 0; index < 4; index++ {
		directory := "task__trial-" + string(rune('a'+index))
		root := filepath.Join(jobRoot, directory)
		codeEdgeWriteHarborBundleJSON(t, filepath.Join(root, "config.json"), map[string]any{"job_id": jobID})
		codeEdgeWriteHarborBundleJSON(t, filepath.Join(root, "lock.json"), map[string]any{"task": map[string]any{"digest": lockDigest}})
		reward := 0
		if index == 0 {
			reward = 1
		}
		model := map[string]any{"name": policy.Evaluator.ModelName}
		if policy.Evaluator.ModelProvider != "" {
			model["provider"] = policy.Evaluator.ModelProvider
		}
		codeEdgeWriteHarborBundleJSON(t, filepath.Join(root, "result.json"), map[string]any{
			"id": "trial-id-" + string(rune('a'+index)), "trial_name": directory,
			"task_checksum": "harbor-dirhash-" + string(rune('a'+index)), "config": map[string]any{"job_id": jobID},
			"started_at": "2026-07-14T00:00:00Z", "finished_at": "2026-07-14T00:01:00Z", "exception_info": nil,
			"agent_info":      map[string]any{"name": policy.Evaluator.AgentName, "version": policy.Evaluator.AgentVersion, "model_info": model},
			"verifier_result": map[string]any{"rewards": map[string]any{policy.PassRewardKey: reward}},
		})
		codeEdgeWriteHarborBundleJSON(t, filepath.Join(root, "agent", "trajectory.json"), map[string]any{"final_metrics": map[string]any{"total_steps": 20}})
	}
	bundle, err := codeedge.CaptureHarborRunBundleV018(codeedge.HarborRunBundleCaptureRequest{
		JobDirectory: jobRoot, MaterializedTaskRoot: taskRoot, FrozenTaskSnapshotDigest: digest,
		HarborCLI: codeedge.HarborCLIIdentity{CommandID: "harbor-cli", Version: "0.18.0", ContentFingerprint: workflowkit.SHA256Fingerprint([]byte("harbor-cli-" + jobID))},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := bundle.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func codeEdgeWriteHarborBundleJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func codeEdgePNG(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 2, 2))
	picture.Set(0, 0, color.RGBA{R: 4, G: 8, B: 15, A: 255})
	if err := png.Encode(&buffer, picture); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func newCodeEdgeComplianceUUID(t *testing.T) string {
	t.Helper()
	id, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
