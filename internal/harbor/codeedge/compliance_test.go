package codeedge

import (
	"errors"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestFinalComplianceApprovesBoundQwenAndOpusEvidence(t *testing.T) {
	fixture := validFinalComplianceFixture(t)
	result, err := (FinalComplianceService{}).Evaluate(fixture.input)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if result.Decision.Status != FinalComplianceApproved || result.Authorization == nil {
		t.Fatalf("final compliance result = %#v, want approved authorization", result)
	}
	if err := result.Authorization.Validate(); err != nil {
		t.Fatalf("authorization.Validate() error = %v", err)
	}
	if !sameFrozenRunBinding(result.Authorization.Decision.Binding, fixture.input.Binding) {
		t.Fatalf("authorization binding = %#v, want %#v", result.Authorization.Decision.Binding, fixture.input.Binding)
	}
	first, err := result.Authorization.Fingerprint()
	if err != nil {
		t.Fatalf("authorization.Fingerprint() first error = %v", err)
	}
	second, err := result.Authorization.Fingerprint()
	if err != nil || first != second {
		t.Fatalf("authorization.Fingerprint() = %q, %v then %q, %v; want stable fingerprint", first, err, second, err)
	}
}

func TestFinalComplianceRejectsQwenHardGateButRetainsDecision(t *testing.T) {
	fixture := validFinalComplianceFixture(t)
	qwen := fixture.input.Handoff.Qwen.Receipt.Clone()
	qwen.Trials[1].Passed = true
	qwen.PassCount = 2
	qwen.PolicyCompliant = false
	qwen.ComplianceReasons = []string{"pass count 2 exceeds maximum 1"}
	if err := qwen.Validate(); err != nil {
		t.Fatalf("mutated Qwen receipt must remain structurally valid: %v", err)
	}
	setFinalComplianceHandoffQwen(t, &fixture.input, qwen)

	service := FinalComplianceService{}
	result, err := service.Evaluate(fixture.input)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if result.Decision.Status != FinalComplianceRejected || result.Authorization != nil {
		t.Fatalf("final compliance result = %#v, want rejected without authorization", result)
	}
	if got := strings.Join(result.Decision.Reasons, " "); !strings.Contains(got, "Qwen pass count exceeds") {
		t.Fatalf("reasons = %q, want explicit Qwen hard-gate rejection", got)
	}
	if _, err := service.IssueLocalPackageAuthorization(result.Decision); !errors.Is(err, ErrFinalComplianceRejected) {
		t.Fatalf("IssueLocalPackageAuthorization() error = %v, want ErrFinalComplianceRejected", err)
	}
}

func TestFinalComplianceRecomputesQwenNumericHardRules(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*EvaluationReceipt)
		want   string
	}{
		{
			name: "pass count",
			mutate: func(receipt *EvaluationReceipt) {
				receipt.Trials[1].Passed = true
				receipt.PassCount = 2
			},
			want: "Qwen pass count exceeds",
		},
		{
			name: "average turns",
			mutate: func(receipt *EvaluationReceipt) {
				receipt.Trials[0].TurnCount = 19
				receipt.AverageTurns = 19.75
			},
			want: "Qwen average turns is below",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := validFinalComplianceFixture(t)
			qwen := fixture.input.Handoff.Qwen.Receipt.Clone()
			test.mutate(&qwen)
			// A persisted receipt flag alone is never authority for the Qwen
			// hard gate; the final service recomputes both confirmed metrics.
			qwen.PolicyCompliant = true
			qwen.ComplianceReasons = nil
			if err := qwen.Validate(); err != nil {
				t.Fatalf("mutated Qwen receipt must remain structurally valid: %v", err)
			}
			setFinalComplianceHandoffQwen(t, &fixture.input, qwen)

			result, err := (FinalComplianceService{}).Evaluate(fixture.input)
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if result.Decision.Status != FinalComplianceRejected || result.Authorization != nil {
				t.Fatalf("final compliance result = %#v, want rejected without authorization", result)
			}
			if got := strings.Join(result.Decision.Reasons, " "); !strings.Contains(got, test.want) {
				t.Fatalf("reasons = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFinalComplianceTreatsOpusAsReferenceOnly(t *testing.T) {
	fixture := validFinalComplianceFixture(t)
	opus := fixture.input.Handoff.Opus.Receipt.Clone()
	opus.Trials[0].TurnCount = 19
	opus.AverageTurns = 19.75
	opus.PolicyCompliant = false
	opus.ComplianceReasons = []string{"average turns 19.75 is below minimum 20"}
	if err := opus.Validate(); err != nil {
		t.Fatalf("mutated Opus receipt must remain structurally valid: %v", err)
	}
	setFinalComplianceHandoffOpus(t, &fixture.input, opus)

	result, err := (FinalComplianceService{}).Evaluate(fixture.input)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if result.Decision.Status != FinalComplianceApproved || result.Authorization == nil {
		t.Fatalf("Opus reference-only result = %#v, want approved authorization", result)
	}
}

func TestFinalComplianceRefusesASecondOpusHardThreshold(t *testing.T) {
	fixture := validFinalComplianceFixture(t)
	maximumPasses := 1
	fixture.input.Policy.OpusPolicy.MaxPassingTrials = &maximumPasses

	_, err := (FinalComplianceService{}).Evaluate(fixture.input)
	if !errors.Is(err, ErrInvalidFinalCompliance) {
		t.Fatalf("Evaluate() error = %v, want ErrInvalidFinalCompliance", err)
	}
}

func TestFinalComplianceRejectsCrossRunEvidenceBeforePackageDecision(t *testing.T) {
	fixture := validFinalComplianceFixture(t)
	fixture.input.Handoff.Opus.Receipt.CatalogFingerprint = workflowkit.SHA256Fingerprint([]byte("other-catalog"))

	_, err := (FinalComplianceService{}).Evaluate(fixture.input)
	if !errors.Is(err, ErrInvalidFinalCompliance) {
		t.Fatalf("Evaluate() error = %v, want ErrInvalidFinalCompliance", err)
	}
}

func TestFinalComplianceBlocksRejectedSubmissionAndTamperedAuthorization(t *testing.T) {
	fixture := validFinalComplianceFixture(t)
	fixture.input.Submission.Status = SubmissionCheckRejected
	fixture.input.Submission.Findings = []string{"submission contract violation"}
	result, err := (FinalComplianceService{}).Evaluate(fixture.input)
	if err != nil {
		t.Fatalf("Evaluate() rejected submission error = %v", err)
	}
	if result.Decision.Status != FinalComplianceRejected || result.Authorization != nil {
		t.Fatalf("submission rejection result = %#v, want no authorization", result)
	}

	fixture = validFinalComplianceFixture(t)
	approved, err := (FinalComplianceService{}).Evaluate(fixture.input)
	if err != nil || approved.Authorization == nil {
		t.Fatalf("Evaluate() approved result = %#v, %v", approved, err)
	}
	tampered := *approved.Authorization
	tampered.Decision.Binding.LockFingerprint = workflowkit.SHA256Fingerprint([]byte("tampered-lock"))
	if err := tampered.Validate(); !errors.Is(err, ErrInvalidFinalCompliance) {
		t.Fatalf("tampered authorization Validate() error = %v, want ErrInvalidFinalCompliance", err)
	}
}

type finalComplianceFixture struct {
	input FinalComplianceInput
}

func validFinalComplianceFixture(t *testing.T) finalComplianceFixture {
	t.Helper()
	maximumPasses := 1
	qwenInput := validEvaluationInput(t, EvaluationPolicy{MaxPassingTrials: &maximumPasses})
	qwenInput.Policy.ID = "codeedge.qwen.pass-at-four"
	qwenInput.Policy.Version = "1"
	qwenReceipt, err := BuildEvaluationReceipt(qwenInput)
	if err != nil {
		t.Fatalf("build Qwen receipt: %v", err)
	}

	opusPolicy := qwenInput.Policy.Clone()
	opusPolicy.ID = "codeedge.opus.pass-at-four"
	opusPolicy.Evaluator.ProfileID = "opus-profile"
	opusPolicy.Evaluator.ModelName = "opus-model"
	opusPolicy.MaxPassingTrials = nil
	opusInput := validEvaluationInputForPolicy(t, opusPolicy)
	opusInput.HarborRunBundle = evidenceForBytes("opus-bundle", HarborRunBundleV018Format, "application/json", opusInput.HarborRunBundle.Bytes)
	opusInput.CanonicalScreenshot = evidenceForBytes("opus-screenshot", "harbor.screenshot.v1", "image/png", validPNG(t))
	opusReceipt, err := BuildEvaluationReceipt(opusInput)
	if err != nil {
		t.Fatalf("build Opus receipt: %v", err)
	}

	childBinding := FrozenRunBinding{
		TaskSnapshotDigest:  qwenInput.Binding.TaskSnapshotDigest,
		CatalogFingerprint:  qwenInput.Binding.CatalogFingerprint,
		LockFingerprint:     qwenInput.Binding.LockFingerprint,
		ManifestFingerprint: qwenInput.Binding.ManifestFingerprint,
	}
	binding := childBinding
	binding.ManifestFingerprint = workflowkit.SHA256Fingerprint([]byte("parent-run-manifest"))
	policy := FinalCompliancePolicy{
		ID:                            "codeedge.phase1.final-compliance",
		Version:                       "1",
		QwenPolicy:                    qwenInput.Policy,
		OpusPolicy:                    opusInput.Policy,
		SubmissionCheckerID:           "codeedge.submission-check",
		SubmissionCheckerVersion:      "1",
		SubmissionReportSchemaVersion: "codeedge.submission-report.v1",
	}
	submission := SubmissionCheckReceipt{
		Format:         SubmissionCheckReceiptFormat,
		Version:        SubmissionCheckReceiptVersion,
		Status:         SubmissionCheckPassed,
		CheckerID:      policy.SubmissionCheckerID,
		CheckerVersion: policy.SubmissionCheckerVersion,
		Binding:        binding,
		Report: workflowkit.ArtifactBinding{
			Name:          "submission_lint_report",
			ArtifactID:    workflowkit.ArtifactID("submission-report"),
			ContentDigest: workflowkit.SHA256Fingerprint([]byte("submission report")),
			SchemaVersion: policy.SubmissionReportSchemaVersion,
		},
		Findings: []string{},
	}
	return finalComplianceFixture{input: FinalComplianceInput{
		Policy:     policy,
		Binding:    binding,
		Handoff:    finalComplianceHandoff(t, binding, childBinding, qwenReceipt, opusReceipt),
		Submission: submission,
	}}
}

func finalComplianceHandoff(t *testing.T, parent, child FrozenRunBinding, qwen, opus EvaluationReceipt) EvaluatorEvidenceHandoff {
	t.Helper()
	makeSource := func(stageID, bundleName, screenshotName string, receipt EvaluationReceipt) EvaluatorEvidenceSource {
		receiptFingerprint, err := receipt.Fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		return EvaluatorEvidenceSource{
			ChildStageAttemptID: stageID, ArtifactManifestFingerprint: workflowkit.SHA256Fingerprint([]byte(stageID + "-manifest")),
			RunBundle:           workflowkit.ArtifactBinding{Name: bundleName, ArtifactID: receipt.RunBundleArtifactID, ContentDigest: receipt.RunBundleContentDigest, SchemaVersion: HarborRunBundleV018Format},
			CanonicalScreenshot: workflowkit.ArtifactBinding{Name: screenshotName, ArtifactID: receipt.ScreenshotArtifactID, ContentDigest: receipt.ScreenshotContentDigest, SchemaVersion: "image/png"},
			TrialSetFingerprint: workflowkit.SHA256Fingerprint([]byte(stageID + "-trials")),
			Receipt:             receipt, ReceiptFingerprint: receiptFingerprint,
		}
	}
	handoff := EvaluatorEvidenceHandoff{
		Format: EvaluatorEvidenceHandoffFormat, Version: EvaluatorEvidenceHandoffVersion,
		ParentRunID: "parent-run", ParentDefinitionFingerprint: workflowkit.SHA256Fingerprint([]byte("parent-definition")), ParentBinding: parent,
		ChildRunID: "child-run", ChildTemplateID: "harbor.codeedge-evaluator", ChildTemplateVersion: "1.0.0", ChildDefinitionFingerprint: workflowkit.SHA256Fingerprint([]byte("child-definition")), ChildBinding: child,
		Qwen: makeSource("qwen-child-stage", "qwen_trial_result", "qwen_pass4_evidence", qwen),
		Opus: makeSource("opus-child-stage", "opus_trial_result", "opus_pass4_evidence", opus),
	}
	if err := handoff.Validate(); err != nil {
		t.Fatal(err)
	}
	return handoff
}

func setFinalComplianceHandoffQwen(t *testing.T, input *FinalComplianceInput, receipt EvaluationReceipt) {
	t.Helper()
	fingerprint, err := receipt.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	input.Handoff.Qwen.Receipt = receipt
	input.Handoff.Qwen.ReceiptFingerprint = fingerprint
}

func setFinalComplianceHandoffOpus(t *testing.T, input *FinalComplianceInput, receipt EvaluationReceipt) {
	t.Helper()
	fingerprint, err := receipt.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	input.Handoff.Opus.Receipt = receipt
	input.Handoff.Opus.ReceiptFingerprint = fingerprint
}
