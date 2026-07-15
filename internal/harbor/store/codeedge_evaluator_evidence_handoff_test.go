package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestCodeEdgeEvaluatorEvidenceHandoffIsImmutableRunBoundAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	fixture := newCodeEdgeEvaluatorEvidenceHandoffFixture(t, s)
	request := fixture.request(t)

	handoff, err := s.CreateCodeEdgeEvaluatorEvidenceHandoff(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if handoff.ID != request.IdempotencyKey || handoff.ParentRunID != fixture.parent.ID || handoff.ChildRunID != fixture.child.ID {
		t.Fatalf("created evaluator evidence handoff = %+v", handoff)
	}
	var entityType string
	if err := s.db.QueryRow(`SELECT entity_type FROM entity_id_registry WHERE id = ?`, handoff.ID).Scan(&entityType); err != nil {
		t.Fatal(err)
	}
	if entityType != "codeedge_evaluator_evidence_handoff" {
		t.Fatalf("handoff registry type = %q", entityType)
	}
	loaded, err := s.GetCodeEdgeEvaluatorEvidenceHandoffForParentRun(ctx, fixture.parent.ID)
	if err != nil || loaded == nil || loaded.ID != handoff.ID {
		t.Fatalf("load evaluator evidence handoff = %+v, %v", loaded, err)
	}
	replayed, err := s.CreateCodeEdgeEvaluatorEvidenceHandoff(ctx, request)
	if err != nil || replayed.ID != handoff.ID {
		t.Fatalf("idempotent evidence handoff replay = %+v, %v", replayed, err)
	}
	conflicting := request
	conflicting.Actor = "other-actor"
	if _, err := s.CreateCodeEdgeEvaluatorEvidenceHandoff(ctx, conflicting); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting handoff replay = %v, want idempotency conflict", err)
	}
	if _, err := s.db.Exec(`UPDATE codeedge_evaluator_evidence_handoffs_v2 SET task_digest = ? WHERE id = ?`, fixture.revision.TaskDigest, handoff.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("raw handoff update = %v, want immutable rejection", err)
	}
	if _, err := s.db.Exec(`DELETE FROM codeedge_evaluator_evidence_handoffs_v2 WHERE id = ?`, handoff.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("raw handoff delete = %v, want append-only rejection", err)
	}
}

func TestCodeEdgeEvaluatorEvidenceHandoffRejectsCrossParentChildOrArtifactLineage(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	fixture := newCodeEdgeEvaluatorEvidenceHandoffFixture(t, s)

	tests := []struct {
		name   string
		mutate func(*CreateCodeEdgeEvaluatorEvidenceHandoffRequest)
	}{
		{
			name: "wrong child parent",
			mutate: func(request *CreateCodeEdgeEvaluatorEvidenceHandoffRequest) {
				other := newCodeEdgeEvaluatorEvidenceHandoffFixture(t, s)
				request.ChildRunID = other.child.ID
			},
		},
		{
			name: "wrong child artifact",
			mutate: func(request *CreateCodeEdgeEvaluatorEvidenceHandoffRequest) {
				other := newCodeEdgeEvaluatorEvidenceHandoffFixture(t, s)
				request.QwenBundle = other.qwenBundle
			},
		},
		{
			name: "reused artifact",
			mutate: func(request *CreateCodeEdgeEvaluatorEvidenceHandoffRequest) {
				request.OpusScreenshot = request.QwenScreenshot
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := fixture.request(t)
			test.mutate(&request)
			if _, err := s.CreateCodeEdgeEvaluatorEvidenceHandoff(ctx, request); err == nil {
				t.Fatal("invalid evaluator evidence handoff was accepted")
			}
		})
	}
}

type codeEdgeEvaluatorEvidenceHandoffFixture struct {
	task           TaskV2
	revision       TaskRevision
	parent         WorkflowRun
	child          WorkflowRun
	qwenStage      StageAttempt
	opusStage      StageAttempt
	qwenBundle     CodeEdgeEvaluatorEvidenceArtifact
	qwenScreenshot CodeEdgeEvaluatorEvidenceArtifact
	opusBundle     CodeEdgeEvaluatorEvidenceArtifact
	opusScreenshot CodeEdgeEvaluatorEvidenceArtifact
	qwenManifest   ArtifactManifest
	opusManifest   ArtifactManifest
	qwenTrials     workflowkit.Fingerprint
	opusTrials     workflowkit.Fingerprint
}

func newCodeEdgeEvaluatorEvidenceHandoffFixture(t *testing.T, s *Store) codeEdgeEvaluatorEvidenceHandoffFixture {
	t.Helper()
	ctx := context.Background()
	task, revision := createValidatedTaskAndRevision(t, s)
	parent, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID,
		WorkflowTemplateID: workflowadapter.CodeEdgePhase1WorkflowTemplateID, WorkflowTemplateVersion: workflowadapter.CodeEdgePhase1WorkflowTemplateVersion,
		ResolvedProfileHash: string(evidenceHandoffFingerprint("parent-profile")), DefinitionHash: string(evidenceHandoffFingerprint("parent-definition")), RunManifestJSON: `{}`,
		Trigger: "codeedge-parent", Actor: "tester", Reason: "create CodeEdge parent",
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID,
		WorkflowTemplateID: workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID, WorkflowTemplateVersion: workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion,
		ResolvedProfileHash: string(evidenceHandoffFingerprint("child-profile")), DefinitionHash: string(evidenceHandoffFingerprint("child-definition")), RunManifestJSON: `{}`,
		ParentRunID: parent.ID, Trigger: "codeedge-evaluator", Actor: "tester", Reason: "create evaluator child",
	})
	if err != nil {
		t.Fatal(err)
	}
	qwenStage := evidenceHandoffStage(t, s, child, "harbor_run_qwen", 1)
	opusStage := evidenceHandoffStage(t, s, child, "harbor_run_opus", 2)
	qwenManifest, qwenBundle, qwenScreenshot := evidenceHandoffArtifacts(t, s, child, qwenStage, revision,
		"qwen_trial_result", codeedge.HarborRunBundleV018Format, "qwen_pass4_evidence", "image/png")
	opusManifest, opusBundle, opusScreenshot := evidenceHandoffArtifacts(t, s, child, opusStage, revision,
		"opus_trial_result", codeedge.HarborRunBundleV018Format, "opus_pass4_evidence", "image/png")
	qwenTrials := evidenceHandoffCompletedTrials(t, s, child, qwenStage)
	opusTrials := evidenceHandoffCompletedTrials(t, s, child, opusStage)
	child, err = s.TransitionWorkflowRun(ctx, TransitionWorkflowRunRequest{
		RunID: child.ID, ExpectedVersion: child.Version, Status: WorkflowRunRunning,
		Actor: "tester", Reason: "start completed evaluator child fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err = s.TransitionWorkflowRun(ctx, TransitionWorkflowRunRequest{
		RunID: child.ID, ExpectedVersion: child.Version, Status: WorkflowRunSucceeded,
		Actor: "tester", Reason: "complete evaluator child fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return codeEdgeEvaluatorEvidenceHandoffFixture{
		task: task, revision: revision, parent: parent, child: child, qwenStage: qwenStage, opusStage: opusStage,
		qwenBundle: qwenBundle, qwenScreenshot: qwenScreenshot, opusBundle: opusBundle, opusScreenshot: opusScreenshot,
		qwenManifest: qwenManifest, opusManifest: opusManifest, qwenTrials: qwenTrials, opusTrials: opusTrials,
	}
}

func (fixture codeEdgeEvaluatorEvidenceHandoffFixture) request(t *testing.T) CreateCodeEdgeEvaluatorEvidenceHandoffRequest {
	t.Helper()
	handoff := fixture.document(t)
	handoffJSON, err := handoff.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	handoffFingerprint, err := handoff.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return CreateCodeEdgeEvaluatorEvidenceHandoffRequest{
		ParentRunID: fixture.parent.ID, ChildRunID: fixture.child.ID, TaskID: fixture.task.ID, RevisionID: fixture.revision.ID, TaskDigest: fixture.revision.TaskDigest,
		ParentCatalogFingerprint: string(handoff.ParentBinding.CatalogFingerprint), ParentLockFingerprint: string(handoff.ParentBinding.LockFingerprint), ParentManifestFingerprint: string(handoff.ParentBinding.ManifestFingerprint), ParentDefinitionFingerprint: string(handoff.ParentDefinitionFingerprint),
		ChildCatalogFingerprint: string(handoff.ChildBinding.CatalogFingerprint), ChildLockFingerprint: string(handoff.ChildBinding.LockFingerprint), ChildManifestFingerprint: string(handoff.ChildBinding.ManifestFingerprint), ChildDefinitionFingerprint: string(handoff.ChildDefinitionFingerprint),
		QwenStageAttemptID: fixture.qwenStage.ID, QwenBundle: fixture.qwenBundle, QwenScreenshot: fixture.qwenScreenshot, QwenTrialSetFingerprint: string(fixture.qwenTrials),
		OpusStageAttemptID: fixture.opusStage.ID, OpusBundle: fixture.opusBundle, OpusScreenshot: fixture.opusScreenshot, OpusTrialSetFingerprint: string(fixture.opusTrials),
		HandoffJSON: string(handoffJSON), HandoffFingerprint: string(handoffFingerprint),
		IdempotencyKey: mustUUIDv7(t), Actor: "tester", Reason: "link verified evaluator child evidence",
	}
}

func (fixture codeEdgeEvaluatorEvidenceHandoffFixture) document(t *testing.T) codeedge.EvaluatorEvidenceHandoff {
	t.Helper()
	parentBinding := codeedge.FrozenRunBinding{
		TaskSnapshotDigest: workflowkit.SubjectDigest(fixture.revision.TaskDigest),
		CatalogFingerprint: evidenceHandoffFingerprint("parent-catalog"), LockFingerprint: evidenceHandoffFingerprint("parent-lock"),
		ManifestFingerprint: evidenceHandoffFingerprint("parent-manifest"),
	}
	childBinding := codeedge.FrozenRunBinding{
		TaskSnapshotDigest: workflowkit.SubjectDigest(fixture.revision.TaskDigest),
		CatalogFingerprint: evidenceHandoffFingerprint("child-catalog"), LockFingerprint: evidenceHandoffFingerprint("child-lock"),
		ManifestFingerprint: evidenceHandoffFingerprint("child-manifest"),
	}
	qwen := evidenceHandoffSource(t, "qwen", fixture.qwenStage, fixture.qwenBundle, fixture.qwenScreenshot, fixture.qwenManifest, fixture.qwenTrials, childBinding)
	opus := evidenceHandoffSource(t, "opus", fixture.opusStage, fixture.opusBundle, fixture.opusScreenshot, fixture.opusManifest, fixture.opusTrials, childBinding)
	return codeedge.EvaluatorEvidenceHandoff{
		Format: codeedge.EvaluatorEvidenceHandoffFormat, Version: codeedge.EvaluatorEvidenceHandoffVersion,
		ParentRunID: fixture.parent.ID, ParentDefinitionFingerprint: workflowkit.Fingerprint(fixture.parent.DefinitionHash), ParentBinding: parentBinding,
		ChildRunID: fixture.child.ID, ChildTemplateID: fixture.child.WorkflowTemplateID, ChildTemplateVersion: fixture.child.WorkflowTemplateVersion,
		ChildDefinitionFingerprint: workflowkit.Fingerprint(fixture.child.DefinitionHash), ChildBinding: childBinding,
		Qwen: qwen, Opus: opus,
	}
}

func evidenceHandoffSource(t *testing.T, role string, stage StageAttempt, bundle, screenshot CodeEdgeEvaluatorEvidenceArtifact, manifest ArtifactManifest, trialSetFingerprint workflowkit.Fingerprint, binding codeedge.FrozenRunBinding) codeedge.EvaluatorEvidenceSource {
	t.Helper()
	receipt := evidenceHandoffReceipt(t, role, binding, bundle, screenshot)
	fingerprint, err := receipt.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return codeedge.EvaluatorEvidenceSource{
		ChildStageAttemptID: stage.ID, ArtifactManifestFingerprint: workflowkit.Fingerprint(manifest.ManifestFingerprint),
		RunBundle:           workflowkit.ArtifactBinding{Name: bundleNameForRole(role), ArtifactID: workflowkit.ArtifactID(bundle.ArtifactID), ContentDigest: workflowkit.Fingerprint(bundle.ContentDigest), SchemaVersion: bundle.SchemaVersion},
		CanonicalScreenshot: workflowkit.ArtifactBinding{Name: screenshotNameForRole(role), ArtifactID: workflowkit.ArtifactID(screenshot.ArtifactID), ContentDigest: workflowkit.Fingerprint(screenshot.ContentDigest), SchemaVersion: screenshot.SchemaVersion},
		TrialSetFingerprint: trialSetFingerprint,
		Receipt:             receipt, ReceiptFingerprint: fingerprint,
	}
}

func evidenceHandoffReceipt(t *testing.T, role string, binding codeedge.FrozenRunBinding, bundle, screenshot CodeEdgeEvaluatorEvidenceArtifact) codeedge.EvaluationReceipt {
	t.Helper()
	trials := make([]codeedge.EvaluationTrialReceipt, 0, 4)
	for ordinal := 1; ordinal <= 4; ordinal++ {
		trials = append(trials, codeedge.EvaluationTrialReceipt{
			HarborTrialID: role + "-trial-id-" + string(rune('0'+ordinal)), HarborTrialName: role + "-trial-" + string(rune('0'+ordinal)),
			Status: codeedge.EvaluationTrialCompleted, TurnCount: 20, ElapsedMillis: 1,
		})
	}
	receipt := codeedge.EvaluationReceipt{
		Format: codeedge.EvaluationReceiptFormat, Version: codeedge.EvaluationReceiptVersion, Status: codeedge.EvaluationCompleted,
		PolicyID: "fixture-" + role + "-policy", PolicyVersion: "1", PolicyFingerprint: evidenceHandoffFingerprint(role + "-policy"),
		Evaluator:            codeedge.EvaluatorIdentity{ProfileID: role + "-profile", ProfileVersion: "1", AgentName: "fixture-agent", AgentVersion: "1", ModelName: role + "-model", ModelProvider: "fixture-provider"},
		HarborEvidenceFormat: codeedge.HarborRunBundleV018Format, HarborCLI: codeedge.HarborCLIIdentity{CommandID: "fixture-harbor", Version: "0.18.0", ContentFingerprint: evidenceHandoffFingerprint("harbor-cli")},
		HarborJobID: role + "-job", MaterializedTaskRootV2Digest: binding.TaskSnapshotDigest, TaskSnapshotDigest: binding.TaskSnapshotDigest,
		CatalogFingerprint: binding.CatalogFingerprint, LockFingerprint: binding.LockFingerprint, ManifestFingerprint: binding.ManifestFingerprint,
		RunBundleArtifactID: workflowkit.ArtifactID(bundle.ArtifactID), RunBundleContentDigest: workflowkit.Fingerprint(bundle.ContentDigest),
		ScreenshotArtifactID: workflowkit.ArtifactID(screenshot.ArtifactID), ScreenshotContentDigest: workflowkit.Fingerprint(screenshot.ContentDigest), ScreenshotMediaType: "image/png",
		Trials: trials, PassCount: 0, AverageTurns: 20, PolicyCompliant: true, ComplianceReasons: []string{},
	}
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func bundleNameForRole(role string) string {
	if role == "qwen" {
		return "qwen_trial_result"
	}
	return "opus_trial_result"
}

func screenshotNameForRole(role string) string {
	if role == "qwen" {
		return "qwen_pass4_evidence"
	}
	return "opus_pass4_evidence"
}

func evidenceHandoffStage(t *testing.T, s *Store, run WorkflowRun, key string, ordinal int) StageAttempt {
	t.Helper()
	stage, err := s.CreateStageAttempt(context.Background(), CreateStageAttemptRequest{
		RunID: run.ID, StageKey: key, StageGroup: "evaluation", Ordinal: ordinal,
		InputFingerprint: string(evidenceHandoffFingerprint(key + "-input")), BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "tester", Reason: "create evaluator source stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err = s.TransitionStageAttempt(context.Background(), TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: StageExecutionRunning,
		Actor: "tester", Reason: "start completed evaluator source stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err = s.TransitionStageAttempt(context.Background(), TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: StageExecutionCompleted, Verdict: VerdictPass,
		Actor: "tester", Reason: "complete evaluator source stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	return stage
}

func evidenceHandoffArtifacts(t *testing.T, s *Store, run WorkflowRun, stage StageAttempt, revision TaskRevision, firstKey, firstSchema, secondKey, secondSchema string) (ArtifactManifest, CodeEdgeEvaluatorEvidenceArtifact, CodeEdgeEvaluatorEvidenceArtifact) {
	t.Helper()
	ctx := context.Background()
	manifest, err := s.CreateArtifactManifest(ctx, CreateArtifactManifestRequest{
		SubjectRevisionID: revision.ID, SubjectDigest: revision.TaskDigest, WorkflowFingerprint: run.DefinitionHash,
		ManifestJSON: `{}`, ManifestFingerprint: string(evidenceHandoffFingerprint(stage.StageKey + "-manifest")),
		IdempotencyKey: mustUUIDv7(t), Actor: "tester", Reason: "create evaluator artifact manifest",
	})
	if err != nil {
		t.Fatal(err)
	}
	create := func(key, schema string) CodeEdgeEvaluatorEvidenceArtifact {
		digest := evidenceHandoffFingerprint(key + "-content")
		reference, createErr := s.CreateArtifactRef(ctx, CreateArtifactRefRequest{
			ManifestID: manifest.ID, ArtifactKey: key, ContentDigest: string(digest), SchemaVersion: schema,
			RunID: run.ID, StageKey: stage.StageKey, AttemptID: stage.ID, TurnOrdinal: 0,
			SubjectRevisionID: revision.ID, SubjectDigest: revision.TaskDigest, WorkflowFingerprint: run.DefinitionHash,
			InputBindingsJSON: `[]`, InputFingerprint: string(evidenceHandoffFingerprint(key + "-input")), ProducerVersion: "1",
			IdempotencyKey: mustUUIDv7(t), Actor: "tester", Reason: "create evaluator artifact source",
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return CodeEdgeEvaluatorEvidenceArtifact{ArtifactID: reference.ID, ContentDigest: string(digest), SchemaVersion: schema}
	}
	return manifest, create(firstKey, firstSchema), create(secondKey, secondSchema)
}

func evidenceHandoffCompletedTrials(t *testing.T, s *Store, run WorkflowRun, stage StageAttempt) workflowkit.Fingerprint {
	t.Helper()
	ctx := context.Background()
	parts := make([]workflowkit.FingerprintPart, 0, 8)
	for ordinal := 1; ordinal <= 4; ordinal++ {
		execution, err := s.CreateTrialExecution(ctx, CreateTrialExecutionRequest{
			ID: mustUUIDv7(t), RunID: run.ID, StageAttemptID: stage.ID, StageKey: stage.StageKey, Ordinal: ordinal,
			Actor: "tester", Reason: "create completed evaluator logical trial",
		})
		if err != nil {
			t.Fatal(err)
		}
		execution, err = s.TransitionTrialExecution(ctx, TransitionTrialExecutionRequest{
			TrialExecutionID: execution.ID, ExpectedVersion: execution.Version, Status: TrialExecutionRunning,
			Actor: "tester", Reason: "start completed evaluator logical trial",
		})
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := s.CreateTrialAttempt(ctx, CreateTrialAttemptRequest{
			ID: mustUUIDv7(t), TrialExecutionID: execution.ID, Ordinal: 1,
			Actor: "tester", Reason: "create completed evaluator technical trial",
		})
		if err != nil {
			t.Fatal(err)
		}
		attempt, err = s.TransitionTrialAttempt(ctx, TransitionTrialAttemptRequest{
			TrialAttemptID: attempt.ID, ExpectedVersion: attempt.Version, Status: TrialAttemptRunning,
			Actor: "tester", Reason: "start completed evaluator technical trial",
		})
		if err != nil {
			t.Fatal(err)
		}
		attempt, err = s.TransitionTrialAttempt(ctx, TransitionTrialAttemptRequest{
			TrialAttemptID: attempt.ID, ExpectedVersion: attempt.Version, Status: TrialAttemptCompleted,
			Actor: "tester", Reason: "complete evaluator technical trial",
		})
		if err != nil {
			t.Fatal(err)
		}
		execution, err = s.TransitionTrialExecution(ctx, TransitionTrialExecutionRequest{
			TrialExecutionID: execution.ID, ExpectedVersion: execution.Version, Status: TrialExecutionCompleted,
			Actor: "tester", Reason: "complete evaluator logical trial",
		})
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts,
			workflowkit.FingerprintPart{Name: fmt.Sprintf("trial_%d", execution.Ordinal), Value: []byte(execution.ID)},
			workflowkit.FingerprintPart{Name: fmt.Sprintf("attempt_%d", execution.Ordinal), Value: []byte(attempt.ID)},
		)
	}
	fingerprint, err := workflowkit.FingerprintParts("harbor.codeedge.evaluator-child-trial-set.v1", parts)
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

func evidenceHandoffFingerprint(seed string) workflowkit.Fingerprint {
	return workflowkit.SHA256Fingerprint([]byte("codeedge-evaluator-handoff:" + seed))
}
