package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// CodeEdgeComplianceService records the one immutable Phase-1 final
// compliance result for a frozen CodeEdge Run. It has no provider or ambient
// configuration dependency: policy, catalog, lock, task identity, results,
// screenshots, submission report, trials, and review decisions are all read
// from the sealed Run and immutable artifact lineage.
type CodeEdgeComplianceService struct{ core *lifecycleServiceCore }

// RecordCodeEdgeFinalComplianceRequest supplies only the evidence identities
// emitted by controlled stages. Policy and frozen Run binding deliberately do
// not appear here: accepting either from a caller would permit a package to be
// authorized under a different deployment contract.
type RecordCodeEdgeFinalComplianceRequest struct {
	ID                 string
	IdempotencyKey     string
	RunID              string
	QwenStageAttemptID string
	OpusStageAttemptID string
	Qwen               codeedge.EvaluationReceipt
	Opus               codeedge.EvaluationReceipt
	Submission         codeedge.SubmissionCheckReceipt
	Actor              string
	Reason             string
}

// CodeEdgeFinalComplianceResult exposes durable facts safe for CLI/TUI
// adapters. The stored receipts remain the immutable source of truth.
type CodeEdgeFinalComplianceResult struct {
	Record        store.CodeEdgeComplianceRecord
	Decision      codeedge.FinalComplianceDecision
	Authorization *codeedge.LocalPackageAuthorization
}

type frozenCodeEdgeRun struct {
	Run      store.WorkflowRun
	Revision store.TaskRevision
	Policy   codeedge.FinalCompliancePolicy
	Binding  codeedge.FrozenRunBinding
}

// RecordFinalCompliance verifies all trusted CodeEdge evidence, projects the
// four logical trial samples for both evaluators, and persists a write-once
// authorization only when final compliance approves the frozen Run.
func (service *CodeEdgeComplianceService) RecordFinalCompliance(ctx context.Context, request RecordCodeEdgeFinalComplianceRequest) (CodeEdgeFinalComplianceResult, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return CodeEdgeFinalComplianceResult{}, fmt.Errorf("CodeEdge compliance service is not configured")
	}
	frozen, err := service.core.loadFrozenCodeEdgeRun(ctx, request.RunID)
	if err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	qwenStage, err := requireCodeEdgeCompletedStage(ctx, service.core.store, frozen.Run, request.QwenStageAttemptID, workflowadapter.HarborRunQwen)
	if err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	opusStage, err := requireCodeEdgeCompletedStage(ctx, service.core.store, frozen.Run, request.OpusStageAttemptID, workflowadapter.HarborRunOpus)
	if err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	qwen, err := service.rebuildCodeEdgeEvaluationReceipt(ctx, frozen, qwenStage, workflowadapter.HarborRunQwen, "qwen_trial_result", "qwen_pass4_evidence", frozen.Policy.QwenPolicy, request.Qwen)
	if err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	opus, err := service.rebuildCodeEdgeEvaluationReceipt(ctx, frozen, opusStage, workflowadapter.HarborRunOpus, "opus_trial_result", "opus_pass4_evidence", frozen.Policy.OpusPolicy, request.Opus)
	if err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	if err := service.verifyCodeEdgeSubmissionReceipt(ctx, frozen, request.Submission); err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	if err := requireApprovedCodeEdgeReviewGate(ctx, service.core.store, frozen.Run, frozen.Revision, workflowadapter.FinalReview, workflowadapter.ReviewFinalQuality); err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	if err := requireApprovedCodeEdgeReviewGate(ctx, service.core.store, frozen.Run, frozen.Revision, workflowadapter.ResultReview, workflowadapter.ReviewModelResult); err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	if err := service.ensureTrustedTrialSet(ctx, frozen.Run, qwenStage, qwen, request.Actor, request.Reason); err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	if err := service.ensureTrustedTrialSet(ctx, frozen.Run, opusStage, opus, request.Actor, request.Reason); err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}

	result, err := (codeedge.FinalComplianceService{}).Evaluate(codeedge.FinalComplianceInput{
		Policy: frozen.Policy, Binding: frozen.Binding, Qwen: qwen, Opus: opus, Submission: request.Submission,
	})
	if err != nil {
		return CodeEdgeFinalComplianceResult{}, fmt.Errorf("evaluate frozen CodeEdge final compliance: %w", err)
	}
	qwenJSON, err := qwen.CanonicalJSON()
	if err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	opusJSON, err := opus.CanonicalJSON()
	if err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	submissionJSON, err := request.Submission.CanonicalJSON()
	if err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	decisionJSON, err := result.Decision.CanonicalJSON()
	if err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	decisionFingerprint, err := result.Decision.Fingerprint()
	if err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}

	status := store.CodeEdgeComplianceRejected
	authorizationJSON := ""
	authorizationFingerprint := workflowkit.Fingerprint("")
	if result.Decision.Status == codeedge.FinalComplianceApproved {
		if result.Authorization == nil {
			return CodeEdgeFinalComplianceResult{}, fmt.Errorf("approved CodeEdge decision has no local package authorization")
		}
		status = store.CodeEdgeComplianceApproved
		authorizationBytes, authorizationErr := result.Authorization.CanonicalJSON()
		if authorizationErr != nil {
			return CodeEdgeFinalComplianceResult{}, authorizationErr
		}
		authorizationJSON = string(authorizationBytes)
		authorizationFingerprint, authorizationErr = result.Authorization.Fingerprint()
		if authorizationErr != nil {
			return CodeEdgeFinalComplianceResult{}, authorizationErr
		}
	}
	record, err := service.core.store.CreateCodeEdgeComplianceRecord(ctx, store.CreateCodeEdgeComplianceRecordRequest{
		ID:                       strings.TrimSpace(request.ID),
		RunID:                    frozen.Run.ID,
		TaskID:                   frozen.Run.TaskID,
		RevisionID:               frozen.Revision.ID,
		TaskDigest:               frozen.Revision.TaskDigest,
		Status:                   status,
		QwenReceiptJSON:          string(qwenJSON),
		OpusReceiptJSON:          string(opusJSON),
		SubmissionReceiptJSON:    string(submissionJSON),
		DecisionJSON:             string(decisionJSON),
		DecisionFingerprint:      string(decisionFingerprint),
		AuthorizationJSON:        authorizationJSON,
		AuthorizationFingerprint: string(authorizationFingerprint),
		IdempotencyKey:           strings.TrimSpace(request.IdempotencyKey),
		Actor:                    request.Actor,
		Reason:                   request.Reason,
	})
	if err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	return CodeEdgeFinalComplianceResult{Record: record, Decision: result.Decision.Clone(), Authorization: cloneCodeEdgeAuthorization(result.Authorization)}, nil
}

func cloneCodeEdgeAuthorization(value *codeedge.LocalPackageAuthorization) *codeedge.LocalPackageAuthorization {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Decision = value.Decision.Clone()
	return &copy
}

// loadFrozenCodeEdgeRun verifies that every binding used by final compliance
// is present in both managed storage and the durable Run record. CodeEdge has
// no non-production fallback because a package authorization without an
// independently verified catalog and lock would be unverifiable on replay.
func (core *lifecycleServiceCore) loadFrozenCodeEdgeRun(ctx context.Context, runID string) (frozenCodeEdgeRun, error) {
	if core == nil || core.store == nil {
		return frozenCodeEdgeRun{}, fmt.Errorf("CodeEdge frozen Run verifier is not configured")
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(runID)); err != nil {
		return frozenCodeEdgeRun{}, err
	}
	if core.deploymentCatalog == nil {
		return frozenCodeEdgeRun{}, fmt.Errorf("CodeEdge final compliance requires a deployment catalog: %w", stageprovider.ErrDeploymentOperationCatalogUnavailable)
	}
	if core.deploymentCatalog.lockResolver == nil || core.deploymentCatalog.lockIdentity == nil {
		return frozenCodeEdgeRun{}, fmt.Errorf("CodeEdge final compliance requires a deployment catalog lock: %w", stageprovider.ErrDeploymentOperationCatalogLockUnavailable)
	}
	run, err := core.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return frozenCodeEdgeRun{}, err
	}
	if run == nil {
		return frozenCodeEdgeRun{}, fmt.Errorf("%w: workflow run %s", ErrLifecycleNotFound, runID)
	}
	if !isCodeEdgePhase1Run(*run) {
		return frozenCodeEdgeRun{}, fmt.Errorf("workflow run %s is not a CodeEdge Phase-1 Run", run.ID)
	}
	revision, err := core.store.GetTaskRevision(ctx, run.RevisionID)
	if err != nil {
		return frozenCodeEdgeRun{}, err
	}
	if revision == nil || revision.TaskID != run.TaskID {
		return frozenCodeEdgeRun{}, fmt.Errorf("%w: CodeEdge Run revision %s", ErrLifecycleNotFound, run.RevisionID)
	}
	profile, specification, err := core.verifyRunManagedExecutionInputs(ctx, *run)
	if err != nil {
		return frozenCodeEdgeRun{}, fmt.Errorf("verify CodeEdge managed execution inputs: %w", err)
	}
	if !profile.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) || !specification.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) || specification.CodeEdgeFinalCompliancePolicy == nil {
		return frozenCodeEdgeRun{}, fmt.Errorf("CodeEdge Run does not contain its required frozen final compliance policy")
	}
	if err := core.verifyRunDeploymentCatalogReceipt(*run); err != nil {
		return frozenCodeEdgeRun{}, fmt.Errorf("verify CodeEdge deployment catalog and lock: %w", err)
	}
	manifest, manifestFingerprint, err := core.verifyManagedCodeEdgeRunManifest(*run)
	if err != nil {
		return frozenCodeEdgeRun{}, err
	}
	catalogRaw, err := canonicalManifestDeploymentCatalogReceipt(manifest)
	if err != nil {
		return frozenCodeEdgeRun{}, fmt.Errorf("decode CodeEdge catalog receipt: %w", err)
	}
	if len(catalogRaw) == 0 {
		return frozenCodeEdgeRun{}, fmt.Errorf("CodeEdge Run has no frozen deployment catalog receipt")
	}
	catalogReceipt, err := stageprovider.ParseDeploymentOperationCatalogReceiptJSON(catalogRaw)
	if err != nil {
		return frozenCodeEdgeRun{}, fmt.Errorf("parse CodeEdge catalog receipt: %w", err)
	}
	if !catalogReceipt.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		return frozenCodeEdgeRun{}, fmt.Errorf("CodeEdge catalog receipt names another workflow template")
	}
	lockIdentity, err := canonicalManifestDeploymentCatalogLockIdentity(manifest)
	if err != nil {
		return frozenCodeEdgeRun{}, fmt.Errorf("decode CodeEdge catalog lock identity: %w", err)
	}
	if lockIdentity == nil {
		return frozenCodeEdgeRun{}, fmt.Errorf("CodeEdge Run has no frozen deployment catalog lock identity")
	}
	policy := specification.CodeEdgeFinalCompliancePolicy.Clone()
	if err := policy.Validate(); err != nil {
		return frozenCodeEdgeRun{}, fmt.Errorf("validate frozen CodeEdge final compliance policy: %w", err)
	}
	binding := codeedge.FrozenRunBinding{
		TaskSnapshotDigest:  workflowkit.SubjectDigest(revision.TaskDigest),
		CatalogFingerprint:  catalogReceipt.CatalogFingerprint,
		LockFingerprint:     lockIdentity.Fingerprint,
		ManifestFingerprint: manifestFingerprint,
	}
	if err := binding.Validate(); err != nil {
		return frozenCodeEdgeRun{}, err
	}
	return frozenCodeEdgeRun{Run: *run, Revision: *revision, Policy: policy, Binding: binding}, nil
}

func (core *lifecycleServiceCore) verifyManagedCodeEdgeRunManifest(run store.WorkflowRun) (runManifest, workflowkit.Fingerprint, error) {
	var stored runManifest
	if err := decodeStrictJSON(run.RunManifestJSON, &stored); err != nil {
		return runManifest{}, "", fmt.Errorf("decode CodeEdge Run manifest: %w", err)
	}
	if stored.Format != "harbor.workflow-run-manifest.v2" || stored.RunID != run.ID || stored.TaskID != run.TaskID || stored.Revision != run.RevisionID || !stored.Resolved.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		return runManifest{}, "", fmt.Errorf("CodeEdge Run manifest does not match durable Run identity")
	}
	raw, err := readManagedRunExecutionInputFile(filepath.Join(core.layout.runDirectory(run.ID), "run-manifest.json"), "run manifest")
	if err != nil {
		return runManifest{}, "", err
	}
	var managed runManifest
	if err := decodeStrictJSON(string(raw), &managed); err != nil {
		return runManifest{}, "", fmt.Errorf("decode managed CodeEdge Run manifest: %w", err)
	}
	canonical, err := json.Marshal(managed)
	if err != nil {
		return runManifest{}, "", fmt.Errorf("canonicalize managed CodeEdge Run manifest: %w", err)
	}
	if !bytes.Equal(canonical, []byte(run.RunManifestJSON)) {
		return runManifest{}, "", fmt.Errorf("managed CodeEdge Run manifest differs from durable Run manifest")
	}
	fingerprint, err := workflowkit.FingerprintBytes("harbor.workflow-run-manifest.v2", []byte(run.RunManifestJSON))
	if err != nil {
		return runManifest{}, "", err
	}
	return stored, fingerprint, nil
}

func isCodeEdgePhase1Run(run store.WorkflowRun) bool {
	return run.WorkflowTemplateID == workflowadapter.CodeEdgePhase1WorkflowTemplateID && run.WorkflowTemplateVersion == workflowadapter.CodeEdgePhase1WorkflowTemplateVersion
}

func requireCodeEdgeCompletedStage(ctx context.Context, dataStore *store.Store, run store.WorkflowRun, stageAttemptID, stageKey string) (store.StageAttempt, error) {
	if err := store.ValidateUUIDv7(strings.TrimSpace(stageAttemptID)); err != nil {
		return store.StageAttempt{}, err
	}
	stage, err := dataStore.GetStageAttempt(ctx, stageAttemptID)
	if err != nil {
		return store.StageAttempt{}, err
	}
	if stage == nil || stage.RunID != run.ID || stage.StageKey != stageKey || stage.ExecutionStatus != store.StageExecutionCompleted {
		return store.StageAttempt{}, fmt.Errorf("CodeEdge stage attempt %s is not a completed %s attempt for Run %s", stageAttemptID, stageKey, run.ID)
	}
	return *stage, nil
}

func requireApprovedCodeEdgeReviewGate(ctx context.Context, dataStore *store.Store, run store.WorkflowRun, revision store.TaskRevision, stageKey string, reviewKind workflowadapter.ReviewKind) error {
	attempts, err := dataStore.ListStageAttemptsForRun(ctx, run.ID)
	if err != nil {
		return err
	}
	matching := make([]store.StageAttempt, 0, 1)
	for _, attempt := range attempts {
		if attempt.StageKey == stageKey {
			matching = append(matching, attempt)
		}
	}
	if len(matching) != 1 {
		return fmt.Errorf("CodeEdge Run %s must have exactly one %s review gate attempt", run.ID, stageKey)
	}
	stage := matching[0]
	if stage.ExecutionStatus != store.StageExecutionCompleted || stage.Verdict != store.VerdictPass {
		return fmt.Errorf("CodeEdge %s review gate is not completed and approved", stageKey)
	}
	binding, err := dataStore.GetReviewGateBindingByStageAttempt(ctx, stage.ID)
	if err != nil {
		return err
	}
	if binding == nil || binding.RunID != run.ID || binding.RevisionID != revision.ID || binding.RevisionDigest != revision.TaskDigest || binding.StageKey != stageKey || binding.ReviewKind != string(reviewKind) {
		return fmt.Errorf("CodeEdge %s review gate binding does not match frozen Run", stageKey)
	}
	decisions, err := dataStore.ListReviewDecisionsForRequest(ctx, binding.ReviewRequestID)
	if err != nil {
		return err
	}
	if len(decisions) != 1 || decisions[0].Action != store.ReviewDecisionApprove || decisions[0].RevisionID != revision.ID || decisions[0].ExpectedRevisionDigest != revision.TaskDigest {
		return fmt.Errorf("CodeEdge %s review gate has no matching approval", stageKey)
	}
	return nil
}

func (service *CodeEdgeComplianceService) rebuildCodeEdgeEvaluationReceipt(ctx context.Context, frozen frozenCodeEdgeRun, stage store.StageAttempt, stageKey, resultKey, screenshotKey string, policy codeedge.EvaluationPolicy, supplied codeedge.EvaluationReceipt) (codeedge.EvaluationReceipt, error) {
	if supplied.Status != codeedge.EvaluationCompleted {
		return codeedge.EvaluationReceipt{}, fmt.Errorf("CodeEdge %s receipt must be a completed trusted four-trial result before final compliance", stageKey)
	}
	if err := supplied.Validate(); err != nil {
		return codeedge.EvaluationReceipt{}, fmt.Errorf("validate supplied CodeEdge %s receipt: %w", stageKey, err)
	}
	resultSchema, resultBytes, err := service.readCodeEdgeStageArtifact(ctx, frozen, stage, resultKey, supplied.ResultArtifactID, supplied.ResultContentDigest)
	if err != nil {
		return codeedge.EvaluationReceipt{}, err
	}
	screenshotSchema, screenshotBytes, err := service.readCodeEdgeStageArtifact(ctx, frozen, stage, screenshotKey, supplied.ScreenshotArtifactID, supplied.ScreenshotContentDigest)
	if err != nil {
		return codeedge.EvaluationReceipt{}, err
	}
	rebuilt, err := codeedge.BuildEvaluationReceipt(codeedge.EvaluationInput{
		Policy: policy,
		Binding: codeedge.EvaluationBinding{
			TaskSnapshotDigest:       frozen.Binding.TaskSnapshotDigest,
			ExpectedHarborTaskDigest: supplied.HarborTaskDigest,
			HarborCLI:                supplied.HarborCLI,
			CatalogFingerprint:       frozen.Binding.CatalogFingerprint,
			LockFingerprint:          frozen.Binding.LockFingerprint,
			ManifestFingerprint:      frozen.Binding.ManifestFingerprint,
		},
		HarborResult: codeedge.EvaluationEvidence{
			ArtifactID: supplied.ResultArtifactID, ContentDigest: supplied.ResultContentDigest,
			SchemaVersion: resultSchema, MediaType: "application/json", Bytes: resultBytes,
		},
		CanonicalScreenshot: codeedge.EvaluationEvidence{
			ArtifactID: supplied.ScreenshotArtifactID, ContentDigest: supplied.ScreenshotContentDigest,
			SchemaVersion: screenshotSchema, MediaType: supplied.ScreenshotMediaType, Bytes: screenshotBytes,
		},
	})
	if err != nil {
		return codeedge.EvaluationReceipt{}, fmt.Errorf("rebuild trusted CodeEdge %s receipt: %w", stageKey, err)
	}
	suppliedCanonical, err := supplied.CanonicalJSON()
	if err != nil {
		return codeedge.EvaluationReceipt{}, err
	}
	rebuiltCanonical, err := rebuilt.CanonicalJSON()
	if err != nil {
		return codeedge.EvaluationReceipt{}, err
	}
	if !bytes.Equal(suppliedCanonical, rebuiltCanonical) {
		return codeedge.EvaluationReceipt{}, fmt.Errorf("CodeEdge %s receipt does not match its trusted result and screenshot evidence", stageKey)
	}
	return rebuilt, nil
}

func (service *CodeEdgeComplianceService) verifyCodeEdgeSubmissionReceipt(ctx context.Context, frozen frozenCodeEdgeRun, receipt codeedge.SubmissionCheckReceipt) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	if receipt.Status != codeedge.SubmissionCheckPassed {
		return fmt.Errorf("CodeEdge submission checks must be passed before final compliance")
	}
	if receipt.CheckerID != frozen.Policy.SubmissionCheckerID || receipt.CheckerVersion != frozen.Policy.SubmissionCheckerVersion || receipt.Report.SchemaVersion != frozen.Policy.SubmissionReportSchemaVersion {
		return fmt.Errorf("CodeEdge submission receipt does not match frozen policy")
	}
	if receipt.Binding != frozen.Binding {
		return fmt.Errorf("CodeEdge submission receipt is bound to another frozen Run")
	}
	if receipt.Report.Name != "submission_lint_report" {
		return fmt.Errorf("CodeEdge submission receipt names unexpected report binding %q", receipt.Report.Name)
	}
	// The report binding names an ArtifactRef, not a stage attempt. Its owner
	// is resolved and verified by readCodeEdgeArtifactBinding below.
	_, _, err := service.readCodeEdgeArtifactBinding(ctx, frozen, receipt.Report, workflowadapter.SubmissionLint, "submission_lint_report")
	return err
}

func (service *CodeEdgeComplianceService) readCodeEdgeStageArtifact(ctx context.Context, frozen frozenCodeEdgeRun, stage store.StageAttempt, artifactKey string, artifactID workflowkit.ArtifactID, contentDigest workflowkit.Fingerprint) (string, []byte, error) {
	if strings.TrimSpace(string(artifactID)) == "" {
		return "", nil, fmt.Errorf("CodeEdge artifact ID is required")
	}
	if err := contentDigest.Validate(); err != nil {
		return "", nil, err
	}
	reference, err := service.core.store.GetArtifactRef(ctx, string(artifactID))
	if err != nil {
		return "", nil, err
	}
	if reference == nil || reference.ID != string(artifactID) || reference.ContentDigest != string(contentDigest) || reference.ArtifactKey != artifactKey || reference.RunID != frozen.Run.ID || reference.StageKey != stage.StageKey || reference.AttemptID != stage.ID || reference.SubjectRevisionID != frozen.Revision.ID || reference.SubjectDigest != frozen.Revision.TaskDigest || reference.WorkflowFingerprint != frozen.Run.DefinitionHash {
		return "", nil, fmt.Errorf("CodeEdge artifact %s does not match frozen stage lineage", artifactID)
	}
	if err := verifyStageArtifactCandidate(ctx, service.core.store, service.core.objects, frozen.Run, frozen.Revision, stageArtifactCandidate{attempt: stage, ref: *reference}); err != nil {
		return "", nil, fmt.Errorf("verify CodeEdge artifact %s: %w", artifactID, err)
	}
	index, err := loadStageArtifactManifestIndex(ctx, service.core.store, reference.ManifestID)
	if err != nil {
		return "", nil, err
	}
	object, err := index.objectFor(*reference)
	if err != nil {
		return "", nil, err
	}
	bytes, err := service.core.objects.ReadAll(ctx, object)
	if err != nil {
		return "", nil, err
	}
	return reference.SchemaVersion, bytes, nil
}

func (service *CodeEdgeComplianceService) readCodeEdgeArtifactBinding(ctx context.Context, frozen frozenCodeEdgeRun, binding workflowkit.ArtifactBinding, stageKey, artifactKey string) (store.StageAttempt, []byte, error) {
	if err := binding.Validate(); err != nil {
		return store.StageAttempt{}, nil, err
	}
	reference, err := service.core.store.GetArtifactRef(ctx, string(binding.ArtifactID))
	if err != nil {
		return store.StageAttempt{}, nil, err
	}
	if reference == nil || reference.ContentDigest != string(binding.ContentDigest) || reference.SchemaVersion != binding.SchemaVersion || reference.ArtifactKey != artifactKey || reference.RunID != frozen.Run.ID || reference.StageKey != stageKey || reference.SubjectRevisionID != frozen.Revision.ID || reference.SubjectDigest != frozen.Revision.TaskDigest || reference.WorkflowFingerprint != frozen.Run.DefinitionHash {
		return store.StageAttempt{}, nil, fmt.Errorf("CodeEdge artifact binding %s does not match frozen submission lineage", binding.ArtifactID)
	}
	stage, err := service.core.store.GetStageAttempt(ctx, reference.AttemptID)
	if err != nil {
		return store.StageAttempt{}, nil, err
	}
	if stage == nil || stage.RunID != frozen.Run.ID || stage.StageKey != stageKey || stage.ExecutionStatus != store.StageExecutionCompleted {
		return store.StageAttempt{}, nil, fmt.Errorf("CodeEdge submission report has no completed submission stage")
	}
	if err := verifyStageArtifactCandidate(ctx, service.core.store, service.core.objects, frozen.Run, frozen.Revision, stageArtifactCandidate{attempt: *stage, ref: *reference}); err != nil {
		return store.StageAttempt{}, nil, err
	}
	index, err := loadStageArtifactManifestIndex(ctx, service.core.store, reference.ManifestID)
	if err != nil {
		return store.StageAttempt{}, nil, err
	}
	object, err := index.objectFor(*reference)
	if err != nil {
		return store.StageAttempt{}, nil, err
	}
	raw, err := service.core.objects.ReadAll(ctx, object)
	if err != nil {
		return store.StageAttempt{}, nil, err
	}
	return *stage, raw, nil
}

func (service *CodeEdgeComplianceService) ensureTrustedTrialSet(ctx context.Context, run store.WorkflowRun, stage store.StageAttempt, receipt codeedge.EvaluationReceipt, actor, reason string) error {
	if receipt.Status != codeedge.EvaluationCompleted || len(receipt.Trials) != 4 {
		return fmt.Errorf("CodeEdge trusted receipt must contain exactly four completed logical trials")
	}
	trials := append([]codeedge.EvaluationTrialReceipt(nil), receipt.Trials...)
	sort.Slice(trials, func(left, right int) bool {
		if trials[left].HarborTrialName != trials[right].HarborTrialName {
			return trials[left].HarborTrialName < trials[right].HarborTrialName
		}
		return trials[left].HarborTrialID < trials[right].HarborTrialID
	})
	existing, err := service.core.store.ListTrialExecutionsForStageAttempt(ctx, stage.ID)
	if err != nil {
		return err
	}
	if len(existing) != len(trials) {
		return fmt.Errorf("CodeEdge evaluator stage %s must have exactly four runtime-preallocated logical trials before trusted receipt projection", stage.ID)
	}
	byOrdinal := make(map[int]store.TrialExecution, len(existing))
	for _, execution := range existing {
		if execution.RunID != run.ID || execution.StageAttemptID != stage.ID || execution.StageKey != stage.StageKey || execution.Ordinal < 1 || execution.Ordinal > len(trials) {
			return fmt.Errorf("CodeEdge evaluator trial execution does not match its frozen stage")
		}
		if _, duplicate := byOrdinal[execution.Ordinal]; duplicate {
			return fmt.Errorf("CodeEdge evaluator stage has duplicate logical trial ordinal %d", execution.Ordinal)
		}
		byOrdinal[execution.Ordinal] = execution
	}
	for ordinal := range trials {
		execution, present := byOrdinal[ordinal+1]
		if !present {
			return fmt.Errorf("CodeEdge evaluator stage %s has no runtime-preallocated logical trial ordinal %d", stage.ID, ordinal+1)
		}
		// TrialExecution ordinal is the stable bridge to the existing
		// canonical receipt ordering above. The provider's logical IDs stay in
		// the immutable receipt schema; no caller can replace the preallocated
		// TrialExecution with a later sample after an external invocation.
		if err := service.completeTrustedTrial(ctx, execution, actor, reason); err != nil {
			return err
		}
	}
	return nil
}

func (service *CodeEdgeComplianceService) completeTrustedTrial(ctx context.Context, execution store.TrialExecution, actor, reason string) error {
	attempts, err := service.core.store.ListTrialAttemptsForTrialExecution(ctx, execution.ID)
	if err != nil {
		return err
	}
	for index, attempt := range attempts {
		if attempt.Ordinal != index+1 {
			return fmt.Errorf("CodeEdge TrialExecution %s has non-contiguous technical attempts", execution.ID)
		}
	}
	if len(attempts) == 0 {
		id, idErr := store.NewUUIDv7()
		if idErr != nil {
			return idErr
		}
		attempt, createErr := service.core.store.CreateTrialAttempt(ctx, store.CreateTrialAttemptRequest{ID: id, TrialExecutionID: execution.ID, Ordinal: 1, Actor: actor, Reason: "project trusted CodeEdge technical attempt"})
		if createErr != nil {
			return createErr
		}
		attempts = []store.TrialAttempt{attempt}
	}
	last := attempts[len(attempts)-1]
	switch last.Status {
	case store.TrialAttemptCompleted:
	case store.TrialAttemptInfraFailed, store.TrialAttemptInterrupted:
		id, idErr := store.NewUUIDv7()
		if idErr != nil {
			return idErr
		}
		retry, createErr := service.core.store.CreateTrialAttempt(ctx, store.CreateTrialAttemptRequest{
			ID: id, TrialExecutionID: execution.ID, RetryOfTrialAttemptID: last.ID, Ordinal: last.Ordinal + 1,
			Actor: actor, Reason: "reconcile trusted CodeEdge technical retry",
		})
		if createErr != nil {
			return createErr
		}
		last = retry
	case store.TrialAttemptCanceled:
		return fmt.Errorf("CodeEdge TrialExecution %s was canceled and cannot be finalized from a trusted result", execution.ID)
	}
	if last.Status == store.TrialAttemptQueued || last.Status == store.TrialAttemptWaiting {
		updated, transitionErr := service.core.store.TransitionTrialAttempt(ctx, store.TransitionTrialAttemptRequest{TrialAttemptID: last.ID, ExpectedVersion: last.Version, Status: store.TrialAttemptRunning, Actor: actor, Reason: "reconcile trusted CodeEdge technical attempt"})
		if transitionErr != nil {
			return transitionErr
		}
		last = updated
	}
	if last.Status == store.TrialAttemptInDoubt {
		updated, transitionErr := service.core.store.TransitionTrialAttempt(ctx, store.TransitionTrialAttemptRequest{TrialAttemptID: last.ID, ExpectedVersion: last.Version, Status: store.TrialAttemptReconciling, Actor: actor, Reason: "reconcile in-doubt CodeEdge technical attempt"})
		if transitionErr != nil {
			return transitionErr
		}
		last = updated
	}
	if last.Status == store.TrialAttemptRunning || last.Status == store.TrialAttemptReconciling {
		if _, transitionErr := service.core.store.TransitionTrialAttempt(ctx, store.TransitionTrialAttemptRequest{TrialAttemptID: last.ID, ExpectedVersion: last.Version, Status: store.TrialAttemptCompleted, Actor: actor, Reason: "record trusted CodeEdge technical result"}); transitionErr != nil {
			return transitionErr
		}
	}
	updatedExecution, err := service.core.store.GetTrialExecution(ctx, execution.ID)
	if err != nil {
		return err
	}
	if updatedExecution == nil {
		return fmt.Errorf("%w: TrialExecution %s", ErrLifecycleNotFound, execution.ID)
	}
	if updatedExecution.Status == store.TrialExecutionCompleted {
		return nil
	}
	if updatedExecution.Status != store.TrialExecutionRunning && updatedExecution.Status != store.TrialExecutionWaiting && updatedExecution.Status != store.TrialExecutionReconciling {
		return fmt.Errorf("CodeEdge TrialExecution %s cannot be finalized from %s", execution.ID, updatedExecution.Status)
	}
	_, err = service.core.store.TransitionTrialExecution(ctx, store.TransitionTrialExecutionRequest{TrialExecutionID: updatedExecution.ID, ExpectedVersion: updatedExecution.Version, Status: store.TrialExecutionCompleted, Actor: actor, Reason: "finalize trusted CodeEdge logical trial"})
	return err
}
