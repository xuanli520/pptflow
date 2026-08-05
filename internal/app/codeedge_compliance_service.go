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
	ID                         string
	IdempotencyKey             string
	RunID                      string
	EvaluatorEvidenceHandoffID string
	Submission                 codeedge.SubmissionCheckReceipt
	Actor                      string
	Reason                     string
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
	return service.recordFinalCompliance(ctx, request, false)
}

// recordFinalComplianceForResultReview is the production ResultReview
// boundary. An approved compliance record and the sealed-to-validated
// revision transition commit together; a rejected compliance result remains
// immutable evidence but cannot validate the revision.
func (service *CodeEdgeComplianceService) recordFinalComplianceForResultReview(ctx context.Context, request RecordCodeEdgeFinalComplianceRequest) (CodeEdgeFinalComplianceResult, error) {
	return service.recordFinalCompliance(ctx, request, true)
}

func (service *CodeEdgeComplianceService) recordFinalCompliance(ctx context.Context, request RecordCodeEdgeFinalComplianceRequest, validateApprovedRevision bool) (CodeEdgeFinalComplianceResult, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return CodeEdgeFinalComplianceResult{}, fmt.Errorf("CodeEdge compliance service is not configured")
	}
	frozen, err := service.core.loadFrozenCodeEdgeRun(ctx, request.RunID)
	if err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	handoff, err := service.loadVerifiedCodeEdgeEvaluatorEvidenceHandoff(ctx, frozen, request.EvaluatorEvidenceHandoffID)
	if err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	handoffFingerprint, err := handoff.Fingerprint()
	if err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	if err := service.verifyCodeEdgeSubmissionReceipt(ctx, frozen, request.Submission); err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	if err := requireApprovedCodeEdgeReviewGate(ctx, service.core.store, frozen.Run, frozen.Revision, workflowadapter.FinalReview, workflowadapter.ReviewFinalQuality); err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	if err := requireApprovedCodeEdgeReviewGate(ctx, service.core.store, frozen.Run, frozen.Revision, workflowadapter.EvaluatorEvidenceHandoff, workflowadapter.ReviewEvaluatorEvidence); err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	if err := requireApprovedCodeEdgeReviewGate(ctx, service.core.store, frozen.Run, frozen.Revision, workflowadapter.ResultReview, workflowadapter.ReviewModelResult); err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	result, err := (codeedge.FinalComplianceService{}).Evaluate(codeedge.FinalComplianceInput{
		Policy: frozen.Policy, Binding: frozen.Binding, Handoff: handoff, Submission: request.Submission,
	})
	if err != nil {
		return CodeEdgeFinalComplianceResult{}, fmt.Errorf("evaluate frozen CodeEdge final compliance: %w", err)
	}
	qwenJSON, err := handoff.Qwen.Receipt.CanonicalJSON()
	if err != nil {
		return CodeEdgeFinalComplianceResult{}, err
	}
	opusJSON, err := handoff.Opus.Receipt.CanonicalJSON()
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
	storeRequest := store.CreateCodeEdgeComplianceRecordRequest{
		ID:                                  strings.TrimSpace(request.ID),
		RunID:                               frozen.Run.ID,
		TaskID:                              frozen.Run.TaskID,
		RevisionID:                          frozen.Revision.ID,
		TaskDigest:                          frozen.Revision.TaskDigest,
		Status:                              status,
		EvaluatorEvidenceHandoffID:          request.EvaluatorEvidenceHandoffID,
		EvaluatorEvidenceHandoffFingerprint: string(handoffFingerprint),
		QwenReceiptJSON:                     string(qwenJSON),
		OpusReceiptJSON:                     string(opusJSON),
		SubmissionReceiptJSON:               string(submissionJSON),
		DecisionJSON:                        string(decisionJSON),
		DecisionFingerprint:                 string(decisionFingerprint),
		AuthorizationJSON:                   authorizationJSON,
		AuthorizationFingerprint:            string(authorizationFingerprint),
		IdempotencyKey:                      strings.TrimSpace(request.IdempotencyKey),
		Actor:                               request.Actor,
		Reason:                              request.Reason,
	}
	var record store.CodeEdgeComplianceRecord
	if validateApprovedRevision && status == store.CodeEdgeComplianceApproved {
		record, _, err = service.core.store.CreateApprovedCodeEdgeComplianceRecordAndValidateRevision(ctx, storeRequest, frozen.Revision.StateVersion)
	} else {
		record, err = service.core.store.CreateCodeEdgeComplianceRecord(ctx, storeRequest)
	}
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
// is present in both managed storage and the durable Run record. The catalog
// receipt stays frozen in the Run while the deployment lock is resolved at
// runtime from the installed template binding; CodeEdge has no non-production
// fallback because a package authorization without an independently verified
// catalog and lock would be unverifiable.
func (core *lifecycleServiceCore) loadFrozenCodeEdgeRun(ctx context.Context, runID string) (frozenCodeEdgeRun, error) {
	if core == nil || core.store == nil {
		return frozenCodeEdgeRun{}, fmt.Errorf("CodeEdge frozen Run verifier is not configured")
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(runID)); err != nil {
		return frozenCodeEdgeRun{}, err
	}
	parentCatalog, configured := core.configuredDeploymentCatalogBindingForTemplate(workflowadapter.CodeEdgePhase1TemplateReference())
	if !configured {
		// Existing single-resolver evaluator compositions inspect a separately
		// persisted parent Run while executing the child. Preserve that
		// cross-Run evidence path, but never allow this fallback in StartRun or
		// worker admission (those use the strict template lookup above).
		parentCatalog, configured = core.deploymentCatalogs.soleBinding()
	}
	if !configured || parentCatalog == nil {
		return frozenCodeEdgeRun{}, fmt.Errorf("CodeEdge final compliance requires a deployment catalog: %w", stageprovider.ErrDeploymentOperationCatalogUnavailable)
	}
	if parentCatalog.lockResolver == nil {
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
	manifest, manifestFingerprint, err := core.verifyManagedCodeEdgeRunManifest(*run)
	if err != nil {
		return frozenCodeEdgeRun{}, err
	}
	// A parent and its evaluator child intentionally use distinct deployment
	// catalogs. Always prove the parent persisted its own catalog receipt;
	// compare against an actively installed catalog only when it names this
	// template, and re-verify the installed lock identity at runtime.
	if err := core.verifyPersistedCodeEdgeCatalogProof(*run, manifest, workflowadapter.CodeEdgePhase1TemplateReference()); err != nil {
		return frozenCodeEdgeRun{}, fmt.Errorf("verify CodeEdge deployment catalog and lock: %w", err)
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
	policy := specification.CodeEdgeFinalCompliancePolicy.Clone()
	if err := policy.Validate(); err != nil {
		return frozenCodeEdgeRun{}, fmt.Errorf("validate frozen CodeEdge final compliance policy: %w", err)
	}
	identity := parentCatalog.lockResolver.LockIdentity()
	if err := parentCatalog.lockResolver.VerifyLockIdentity(identity); err != nil {
		return frozenCodeEdgeRun{}, fmt.Errorf("verify CodeEdge deployment catalog lock identity: %w", err)
	}
	binding := codeedge.FrozenRunBinding{
		TaskSnapshotDigest:  workflowkit.SubjectDigest(revision.TaskDigest),
		CatalogFingerprint:  catalogReceipt.CatalogFingerprint,
		LockFingerprint:     identity.Fingerprint,
		ManifestFingerprint: manifestFingerprint,
	}
	if err := binding.Validate(); err != nil {
		return frozenCodeEdgeRun{}, err
	}
	return frozenCodeEdgeRun{Run: *run, Revision: *revision, Policy: policy, Binding: binding}, nil
}

func (core *lifecycleServiceCore) verifyManagedCodeEdgeRunManifest(run store.WorkflowRun) (runManifest, workflowkit.Fingerprint, error) {
	return core.verifyManagedRunManifestForTemplate(run, workflowadapter.CodeEdgePhase1TemplateReference(), "CodeEdge Run")
}

func (core *lifecycleServiceCore) verifyManagedRunManifestForTemplate(run store.WorkflowRun, template workflowadapter.TemplateReference, label string) (runManifest, workflowkit.Fingerprint, error) {
	var stored runManifest
	if err := decodeStrictJSON(run.RunManifestJSON, &stored); err != nil {
		return runManifest{}, "", fmt.Errorf("decode %s manifest: %w", label, err)
	}
	if stored.Format != "harbor.workflow-run-manifest.v2" || stored.RunID != run.ID || stored.TaskID != run.TaskID || stored.Revision != run.RevisionID || !stored.Resolved.Template.Equal(template) {
		return runManifest{}, "", fmt.Errorf("%s manifest does not match durable Run identity", label)
	}
	raw, err := readManagedRunExecutionInputFile(filepath.Join(core.layout.runDirectory(run.ID), "run-manifest.json"), "run manifest")
	if err != nil {
		return runManifest{}, "", err
	}
	var managed runManifest
	if err := decodeStrictJSON(string(raw), &managed); err != nil {
		return runManifest{}, "", fmt.Errorf("decode managed %s manifest: %w", label, err)
	}
	canonical, err := json.Marshal(managed)
	if err != nil {
		return runManifest{}, "", fmt.Errorf("canonicalize managed %s manifest: %w", label, err)
	}
	if !bytes.Equal(canonical, []byte(run.RunManifestJSON)) {
		return runManifest{}, "", fmt.Errorf("managed %s manifest differs from durable Run manifest", label)
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

func (service *CodeEdgeComplianceService) verifyCodeEdgeSubmissionReceipt(ctx context.Context, frozen frozenCodeEdgeRun, receipt codeedge.SubmissionCheckReceipt) error {
	if err := receipt.Validate(); err != nil {
		return err
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
	stage, raw, err := service.readCodeEdgeArtifactBinding(ctx, frozen, receipt.Report, workflowadapter.SubmissionLint, "submission_lint_report")
	if err != nil {
		return err
	}
	var report codeEdgePhase1StageReport
	if err := decodeStrictJSON(string(raw), &report); err != nil {
		return fmt.Errorf("decode CodeEdge submission report: %w", err)
	}
	if report.Format != codeEdgePhase1ReportFormat || report.Version != codeEdgePhase1ReportVersion ||
		report.Stage != string(workflowadapter.SubmissionLint) || report.RunID != frozen.Run.ID || report.StageAttemptID != stage.ID ||
		report.TaskSnapshotDigest != string(frozen.Binding.TaskSnapshotDigest) || report.Findings == nil || string(stage.Verdict) != string(report.Verdict) {
		return fmt.Errorf("CodeEdge submission report is not bound to the completed frozen stage")
	}
	expectedStatus := codeedge.SubmissionCheckRejected
	if report.Verdict == workflowkit.VerdictPass {
		expectedStatus = codeedge.SubmissionCheckPassed
	}
	if receipt.Status != expectedStatus {
		return fmt.Errorf("CodeEdge submission receipt status %q does not match frozen submission report verdict %q", receipt.Status, report.Verdict)
	}
	wantFindings := append([]string{}, report.Findings...)
	gotFindings := append([]string{}, receipt.Findings...)
	sort.Strings(wantFindings)
	sort.Strings(gotFindings)
	if len(wantFindings) != len(gotFindings) {
		return fmt.Errorf("CodeEdge submission receipt findings do not match frozen submission report")
	}
	for index := range wantFindings {
		if wantFindings[index] != gotFindings[index] {
			return fmt.Errorf("CodeEdge submission receipt findings do not match frozen submission report")
		}
	}
	return nil
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

// completeTrustedTrialSet projects an already independently verified set of
// logical Harbor samples onto the durable UUIDv7 TrialExecution rows created
// before the external call. The caller owns evidence validation; this helper
// owns only the stable four-row identity bridge and technical-attempt state
// transitions. It is used by both controlled evaluator completion and its
// recovery path, so final compliance can remain a pure verifier of completed
// child evidence.
func (service *CodeEdgeComplianceService) completeTrustedTrialSet(ctx context.Context, run store.WorkflowRun, stage store.StageAttempt, expectedCount int, actor, reason string) error {
	if service == nil || service.core == nil || service.core.store == nil {
		return fmt.Errorf("CodeEdge trusted trial projector is not configured")
	}
	if expectedCount != codeEdgeEvaluatorTrialCount {
		return fmt.Errorf("CodeEdge trusted trial projector requires exactly four logical trials")
	}
	existing, err := service.core.store.ListTrialExecutionsForStageAttempt(ctx, stage.ID)
	if err != nil {
		return err
	}
	if len(existing) != expectedCount {
		return fmt.Errorf("CodeEdge evaluator stage %s must have exactly four runtime-preallocated logical trials before trusted receipt projection", stage.ID)
	}
	byOrdinal := make(map[int]store.TrialExecution, expectedCount)
	for _, execution := range existing {
		if execution.RunID != run.ID || execution.StageAttemptID != stage.ID || execution.StageKey != stage.StageKey || execution.Ordinal < 1 || execution.Ordinal > expectedCount {
			return fmt.Errorf("CodeEdge evaluator trial execution does not match its frozen stage")
		}
		if _, duplicate := byOrdinal[execution.Ordinal]; duplicate {
			return fmt.Errorf("CodeEdge evaluator stage has duplicate logical trial ordinal %d", execution.Ordinal)
		}
		byOrdinal[execution.Ordinal] = execution
	}
	for ordinal := 0; ordinal < expectedCount; ordinal++ {
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
	if updatedExecution.Status == store.TrialExecutionInDoubt {
		reconciled, transitionErr := service.core.store.TransitionTrialExecution(ctx, store.TransitionTrialExecutionRequest{
			TrialExecutionID: updatedExecution.ID, ExpectedVersion: updatedExecution.Version, Status: store.TrialExecutionReconciling,
			Actor: actor, Reason: "reconcile observed CodeEdge logical trial",
		})
		if transitionErr != nil {
			return transitionErr
		}
		updatedExecution = &reconciled
	}
	if updatedExecution.Status != store.TrialExecutionRunning && updatedExecution.Status != store.TrialExecutionWaiting && updatedExecution.Status != store.TrialExecutionReconciling {
		return fmt.Errorf("CodeEdge TrialExecution %s cannot be finalized from %s", execution.ID, updatedExecution.Status)
	}
	_, err = service.core.store.TransitionTrialExecution(ctx, store.TransitionTrialExecutionRequest{TrialExecutionID: updatedExecution.ID, ExpectedVersion: updatedExecution.Version, Status: store.TrialExecutionCompleted, Actor: actor, Reason: "finalize trusted CodeEdge logical trial"})
	return err
}
