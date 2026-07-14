package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestCodeEdgeComplianceRecordIsImmutableRunBoundAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	fixture := newTrialStoreFixture(t, s)
	request := codeEdgeComplianceRecordFixture(t, s, fixture)

	record, err := s.CreateCodeEdgeComplianceRecord(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != request.IdempotencyKey || record.Status != CodeEdgeComplianceApproved || record.AuthorizationJSON == "" {
		t.Fatalf("created CodeEdge compliance record = %+v", record)
	}
	var entityType string
	if err := s.db.QueryRow(`SELECT entity_type FROM entity_id_registry WHERE id = ?`, record.ID).Scan(&entityType); err != nil {
		t.Fatal(err)
	}
	if entityType != "codeedge_compliance_record" {
		t.Fatalf("CodeEdge compliance registry type = %q, want codeedge_compliance_record", entityType)
	}
	loaded, err := s.GetCodeEdgeComplianceRecordForRun(ctx, fixture.run.ID)
	if err != nil || loaded == nil || loaded.ID != record.ID {
		t.Fatalf("load CodeEdge compliance record = %+v, %v", loaded, err)
	}
	replayed, err := s.CreateCodeEdgeComplianceRecord(ctx, request)
	if err != nil || replayed.ID != record.ID {
		t.Fatalf("idempotent CodeEdge compliance replay = %+v, %v", replayed, err)
	}
	conflicting := request
	conflicting.Actor = "another-tester"
	if _, err := s.CreateCodeEdgeComplianceRecord(ctx, conflicting); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting CodeEdge compliance replay = %v, want ErrIdempotencyConflict", err)
	}
	if _, err := s.db.Exec(`UPDATE codeedge_compliance_records_v20 SET status = 'rejected' WHERE id = ?`, record.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("raw CodeEdge compliance update = %v, want immutable rejection", err)
	}
	if _, err := s.db.Exec(`DELETE FROM codeedge_compliance_records_v20 WHERE id = ?`, record.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("raw CodeEdge compliance delete = %v, want append-only rejection", err)
	}
}

func TestCodeEdgeComplianceRecordRejectsCrossRunAndAuthorizationShape(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	fixture := newTrialStoreFixture(t, s)
	request := codeEdgeComplianceRecordFixture(t, s, fixture)

	other := newTrialStoreFixture(t, s)
	request.RunID = other.run.ID
	if _, err := s.CreateCodeEdgeComplianceRecord(ctx, request); err == nil || !strings.Contains(err.Error(), "Run does not match") {
		t.Fatalf("cross-run CodeEdge compliance record = %v, want Run binding rejection", err)
	}

	request = codeEdgeComplianceRecordFixture(t, s, fixture)
	request.Status = CodeEdgeComplianceRejected
	if _, err := s.CreateCodeEdgeComplianceRecord(ctx, request); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("rejected record with authorization = %v, want authorization rejection", err)
	}
	request = codeEdgeComplianceRecordFixtureWithStatus(t, s, fixture, CodeEdgeComplianceRejected)
	rejected, err := s.CreateCodeEdgeComplianceRecord(ctx, request)
	if err != nil || rejected.Status != CodeEdgeComplianceRejected || rejected.AuthorizationJSON != "" {
		t.Fatalf("rejected CodeEdge compliance record = %+v, %v", rejected, err)
	}
}

func TestCodeEdgeComplianceRecordRejectsNonCanonicalOrInconsistentTypedEvidence(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	fixture := newTrialStoreFixture(t, s)

	request := codeEdgeComplianceRecordFixture(t, s, fixture)
	request.DecisionFingerprint = string(workflowkit.SHA256Fingerprint([]byte("forged-decision")))
	if _, err := s.CreateCodeEdgeComplianceRecord(ctx, request); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("forged decision fingerprint = %v, want typed fingerprint rejection", err)
	}

	request = codeEdgeComplianceRecordFixture(t, s, fixture)
	request.QwenReceiptJSON = strings.Replace(request.QwenReceiptJSON, "{", `{"unknown":true,`, 1)
	if _, err := s.CreateCodeEdgeComplianceRecord(ctx, request); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("non-canonical Qwen receipt = %v, want canonical rejection", err)
	}

	request = codeEdgeComplianceRecordFixture(t, s, fixture)
	request.AuthorizationFingerprint = string(workflowkit.SHA256Fingerprint([]byte("forged-authorization")))
	if _, err := s.CreateCodeEdgeComplianceRecord(ctx, request); err == nil || !strings.Contains(err.Error(), "authorization fingerprint") {
		t.Fatalf("forged authorization fingerprint = %v, want typed authorization rejection", err)
	}
}

func codeEdgeComplianceRecordFixture(t *testing.T, s *Store, fixture trialStoreFixture) CreateCodeEdgeComplianceRecordRequest {
	return codeEdgeComplianceRecordFixtureWithStatus(t, s, fixture, CodeEdgeComplianceApproved)
}

func codeEdgeComplianceRecordFixtureWithStatus(t *testing.T, s *Store, fixture trialStoreFixture, status CodeEdgeComplianceStatus) CreateCodeEdgeComplianceRecordRequest {
	t.Helper()
	revision, err := s.GetTaskRevision(context.Background(), fixture.run.RevisionID)
	if err != nil || revision == nil {
		t.Fatalf("load CodeEdge compliance fixture revision = %+v, %v", revision, err)
	}
	binding := codeedge.FrozenRunBinding{
		TaskSnapshotDigest:  workflowkit.SubjectDigest(revision.TaskDigest),
		CatalogFingerprint:  codeEdgeFixtureFingerprint("catalog"),
		LockFingerprint:     codeEdgeFixtureFingerprint("lock"),
		ManifestFingerprint: codeEdgeFixtureFingerprint("manifest"),
	}
	qwen := codeEdgeFixtureEvaluationReceipt(t, "qwen", binding)
	opus := codeEdgeFixtureEvaluationReceipt(t, "opus", binding)
	submission := codeedge.SubmissionCheckReceipt{
		Format: codeedge.SubmissionCheckReceiptFormat, Version: codeedge.SubmissionCheckReceiptVersion,
		Status: codeedge.SubmissionCheckPassed, CheckerID: "fixture-submission-check", CheckerVersion: "1",
		Binding: binding,
		Report: workflowkit.ArtifactBinding{
			Name: "submission_lint_report", ArtifactID: "fixture-submission-report", ContentDigest: codeEdgeFixtureFingerprint("submission-report"), SchemaVersion: "fixture.submission-report.v1",
		},
		Findings: []string{},
	}
	qwenFingerprint, err := qwen.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	opusFingerprint, err := opus.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	submissionFingerprint, err := submission.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	decisionStatus := codeedge.FinalComplianceApproved
	reasons := []string{}
	if status == CodeEdgeComplianceRejected {
		decisionStatus = codeedge.FinalComplianceRejected
		reasons = []string{"fixture final compliance rejection"}
	}
	decision := codeedge.FinalComplianceDecision{
		Format: codeedge.FinalComplianceDecisionFormat, Version: codeedge.FinalComplianceDecisionVersion, Status: decisionStatus,
		PolicyID: "fixture-final-compliance", PolicyVersion: "1", PolicyFingerprint: codeEdgeFixtureFingerprint("final-policy"),
		Binding: binding, QwenReceiptFingerprint: qwenFingerprint, OpusReceiptFingerprint: opusFingerprint, SubmissionReceiptFingerprint: submissionFingerprint, Reasons: reasons,
	}
	qwenJSON, err := qwen.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	opusJSON, err := opus.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	submissionJSON, err := submission.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	decisionJSON, err := decision.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	decisionFingerprint, err := decision.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	authorizationJSON, authorizationFingerprint := "", ""
	if status == CodeEdgeComplianceApproved {
		authorization, authorizationErr := (codeedge.FinalComplianceService{}).IssueLocalPackageAuthorization(decision)
		if authorizationErr != nil {
			t.Fatal(authorizationErr)
		}
		authorizationRaw, authorizationErr := authorization.CanonicalJSON()
		if authorizationErr != nil {
			t.Fatal(authorizationErr)
		}
		authorizationJSON = string(authorizationRaw)
		authorizationFingerprintValue, authorizationErr := authorization.Fingerprint()
		if authorizationErr != nil {
			t.Fatal(authorizationErr)
		}
		authorizationFingerprint = string(authorizationFingerprintValue)
	}
	return CreateCodeEdgeComplianceRecordRequest{
		RunID: fixture.run.ID, TaskID: fixture.run.TaskID, RevisionID: fixture.run.RevisionID,
		TaskDigest: revision.TaskDigest, Status: status,
		QwenReceiptJSON: string(qwenJSON), OpusReceiptJSON: string(opusJSON),
		SubmissionReceiptJSON: string(submissionJSON), DecisionJSON: string(decisionJSON),
		DecisionFingerprint: string(decisionFingerprint), AuthorizationJSON: authorizationJSON,
		AuthorizationFingerprint: authorizationFingerprint, IdempotencyKey: mustUUIDv7(t),
		Actor: "tester", Reason: "record CodeEdge final compliance",
	}
}

func codeEdgeFixtureEvaluationReceipt(t *testing.T, role string, binding codeedge.FrozenRunBinding) codeedge.EvaluationReceipt {
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
		PolicyID: "fixture-" + role + "-policy", PolicyVersion: "1", PolicyFingerprint: codeEdgeFixtureFingerprint(role + "-policy"),
		Evaluator:          codeedge.EvaluatorIdentity{ProfileID: role + "-profile", ProfileVersion: "1", AgentName: "fixture-agent", AgentVersion: "1", ModelName: role + "-model", ModelProvider: "fixture-provider"},
		HarborResultFormat: codeedge.HarborJobResultV018, HarborCLI: codeedge.HarborCLIIdentity{CommandID: "fixture-harbor", Version: "0.18.0", ContentFingerprint: codeEdgeFixtureFingerprint("harbor-cli")},
		HarborJobID: role + "-job", HarborTaskDigest: "fixture-harbor-task-digest", TaskSnapshotDigest: binding.TaskSnapshotDigest,
		CatalogFingerprint: binding.CatalogFingerprint, LockFingerprint: binding.LockFingerprint, ManifestFingerprint: binding.ManifestFingerprint,
		ResultArtifactID: workflowkit.ArtifactID(role + "-result"), ResultContentDigest: codeEdgeFixtureFingerprint(role + "-result"),
		ScreenshotArtifactID: workflowkit.ArtifactID(role + "-screenshot"), ScreenshotContentDigest: codeEdgeFixtureFingerprint(role + "-screenshot"), ScreenshotMediaType: "image/png",
		Trials: trials, PassCount: 0, AverageTurns: 20, PolicyCompliant: true, ComplianceReasons: []string{},
	}
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func codeEdgeFixtureFingerprint(seed string) workflowkit.Fingerprint {
	return workflowkit.SHA256Fingerprint([]byte(seed))
}
