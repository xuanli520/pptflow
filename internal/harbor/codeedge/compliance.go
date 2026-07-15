package codeedge

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// SubmissionCheckReceiptFormat identifies the immutable result of the
	// CodeEdge submission-check operation. It is deliberately separate from
	// the evaluator receipt because it is produced after both evaluations.
	SubmissionCheckReceiptFormat     = "codeedge.phase1.submission-check-receipt.v1"
	SubmissionCheckReceiptVersion    = "1"
	FinalComplianceDecisionFormat    = "codeedge.phase1.final-compliance-decision.v1"
	FinalComplianceDecisionVersion   = "2"
	LocalPackageAuthorizationFormat  = "codeedge.phase1.local-package-authorization.v1"
	LocalPackageAuthorizationVersion = "1"

	finalCompliancePolicyFingerprintDomain     = "harbor.codeedge.phase1.final-compliance-policy.v1"
	finalComplianceDecisionFingerprintDomain   = "harbor.codeedge.phase1.final-compliance-decision.v1"
	localPackageAuthorizationFingerprintDomain = "harbor.codeedge.phase1.local-package-authorization.v1"
)

var (
	// ErrInvalidFinalCompliance marks a malformed or incompatible frozen
	// policy, receipt, decision, or package authorization. Such input must not
	// be reinterpreted as an ordinary content failure.
	ErrInvalidFinalCompliance = errors.New("CodeEdge Phase-1 final compliance: invalid evidence")
	// ErrFinalComplianceRejected marks a structurally valid decision that did
	// not pass all required final gates and therefore cannot authorize a local
	// package.
	ErrFinalComplianceRejected = errors.New("CodeEdge Phase-1 final compliance: package is not authorized")
)

// FrozenRunBinding is the minimum immutable Run identity required by final
// compliance. It intentionally contains identities rather than deployment
// paths, credentials, or mutable workspace locations.
type FrozenRunBinding struct {
	TaskSnapshotDigest  workflowkit.SubjectDigest `json:"task_snapshot_digest"`
	CatalogFingerprint  workflowkit.Fingerprint   `json:"catalog_fingerprint"`
	LockFingerprint     workflowkit.Fingerprint   `json:"lock_fingerprint"`
	ManifestFingerprint workflowkit.Fingerprint   `json:"manifest_fingerprint"`
}

// Validate verifies that all final-compliance inputs belong to a concrete
// frozen task and deployment contract.
func (binding FrozenRunBinding) Validate() error {
	if err := binding.TaskSnapshotDigest.Validate(); err != nil {
		return fmt.Errorf("%w: task snapshot digest: %v", ErrInvalidFinalCompliance, err)
	}
	for _, field := range []struct {
		name  string
		value workflowkit.Fingerprint
	}{
		{"catalog fingerprint", binding.CatalogFingerprint},
		{"lock fingerprint", binding.LockFingerprint},
		{"manifest fingerprint", binding.ManifestFingerprint},
	} {
		if err := field.value.Validate(); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInvalidFinalCompliance, field.name, err)
		}
	}
	return nil
}

func sameFrozenRunBinding(left, right FrozenRunBinding) bool {
	return left.TaskSnapshotDigest == right.TaskSnapshotDigest &&
		left.CatalogFingerprint == right.CatalogFingerprint &&
		left.LockFingerprint == right.LockFingerprint &&
		left.ManifestFingerprint == right.ManifestFingerprint
}

// SubmissionCheckStatus records whether the submission-check operation was
// completed successfully, found a content violation, or could not produce a
// trustworthy result. Only Passed can advance to final approval.
type SubmissionCheckStatus string

const (
	SubmissionCheckPassed      SubmissionCheckStatus = "passed"
	SubmissionCheckRejected    SubmissionCheckStatus = "rejected"
	SubmissionCheckInfraFailed SubmissionCheckStatus = "infra_failed"
)

// SubmissionCheckReceipt is the typed immutable result of the submission
// checks that run after Qwen and Opus. Report is an artifact binding rather
// than raw report bytes so the receipt can be stored in durable lineage
// without duplicating or weakening artifact addressing.
type SubmissionCheckReceipt struct {
	Format         string                      `json:"format"`
	Version        string                      `json:"version"`
	Status         SubmissionCheckStatus       `json:"status"`
	CheckerID      string                      `json:"checker_id"`
	CheckerVersion string                      `json:"checker_version"`
	Binding        FrozenRunBinding            `json:"binding"`
	Report         workflowkit.ArtifactBinding `json:"report"`
	Findings       []string                    `json:"findings"`
}

// Clone returns an independently owned receipt value.
func (receipt SubmissionCheckReceipt) Clone() SubmissionCheckReceipt {
	if receipt.Findings != nil {
		receipt.Findings = append([]string{}, receipt.Findings...)
	}
	return receipt
}

// Validate verifies a structurally complete submission receipt. Rejected and
// infra-failed receipts remain valid evidence; final compliance turns their
// status into a non-approval rather than discarding their diagnostics.
func (receipt SubmissionCheckReceipt) Validate() error {
	if receipt.Format != SubmissionCheckReceiptFormat || receipt.Version != SubmissionCheckReceiptVersion {
		return fmt.Errorf("%w: unsupported submission receipt format/version %q/%q", ErrInvalidFinalCompliance, receipt.Format, receipt.Version)
	}
	switch receipt.Status {
	case SubmissionCheckPassed, SubmissionCheckRejected, SubmissionCheckInfraFailed:
	default:
		return fmt.Errorf("%w: unsupported submission receipt status %q", ErrInvalidFinalCompliance, receipt.Status)
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"submission checker id", receipt.CheckerID},
		{"submission checker version", receipt.CheckerVersion},
	} {
		if err := validateFinalComplianceText(field.name, field.value); err != nil {
			return err
		}
	}
	if err := receipt.Binding.Validate(); err != nil {
		return err
	}
	if err := receipt.Report.Validate(); err != nil {
		return fmt.Errorf("%w: submission report: %v", ErrInvalidFinalCompliance, err)
	}
	if receipt.Findings == nil {
		return fmt.Errorf("%w: submission findings must be an explicit array", ErrInvalidFinalCompliance)
	}
	seen := make(map[string]struct{}, len(receipt.Findings))
	for _, finding := range receipt.Findings {
		if err := validateFinalComplianceText("submission finding", finding); err != nil {
			return err
		}
		if _, duplicate := seen[finding]; duplicate {
			return fmt.Errorf("%w: duplicate submission finding %q", ErrInvalidFinalCompliance, finding)
		}
		seen[finding] = struct{}{}
	}
	return nil
}

// CanonicalJSON returns a stable receipt representation suitable for an
// immutable artifact. Findings are set-like diagnostics and therefore sorted.
func (receipt SubmissionCheckReceipt) CanonicalJSON() ([]byte, error) {
	if err := receipt.Validate(); err != nil {
		return nil, err
	}
	canonical := receipt.Clone()
	sort.Strings(canonical.Findings)
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("%w: encode submission receipt: %v", ErrInvalidFinalCompliance, err)
	}
	return encoded, nil
}

// Fingerprint returns the immutable identity of the submission evidence.
func (receipt SubmissionCheckReceipt) Fingerprint() (workflowkit.Fingerprint, error) {
	canonical, err := receipt.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return workflowkit.FingerprintBytes(SubmissionCheckReceiptFormat, canonical)
}

// FinalCompliancePolicy freezes the role-specific rules that turn evaluation
// and submission evidence into the final package decision. It contains no
// external executable, endpoint, model secret, or ambient deployment default.
type FinalCompliancePolicy struct {
	ID                            string           `json:"id"`
	Version                       string           `json:"version"`
	QwenPolicy                    EvaluationPolicy `json:"qwen_policy"`
	OpusPolicy                    EvaluationPolicy `json:"opus_policy"`
	SubmissionCheckerID           string           `json:"submission_checker_id"`
	SubmissionCheckerVersion      string           `json:"submission_checker_version"`
	SubmissionReportSchemaVersion string           `json:"submission_report_schema_version"`
}

// Clone returns an independently owned policy value.
func (policy FinalCompliancePolicy) Clone() FinalCompliancePolicy {
	policy.QwenPolicy = policy.QwenPolicy.Clone()
	policy.OpusPolicy = policy.OpusPolicy.Clone()
	if policy.QwenPolicy.InfraExceptionTypes == nil {
		policy.QwenPolicy.InfraExceptionTypes = []string{}
	}
	if policy.OpusPolicy.InfraExceptionTypes == nil {
		policy.OpusPolicy.InfraExceptionTypes = []string{}
	}
	return policy
}

// Validate enforces the confirmed Phase-1 role split: Qwen is the hard
// pass-at-four gate, while Opus must be fully evidenced but cannot introduce
// a second pass/fail threshold.
func (policy FinalCompliancePolicy) Validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"final compliance policy id", policy.ID},
		{"final compliance policy version", policy.Version},
		{"submission checker id", policy.SubmissionCheckerID},
		{"submission checker version", policy.SubmissionCheckerVersion},
		{"submission report schema version", policy.SubmissionReportSchemaVersion},
	} {
		if err := validateFinalComplianceText(field.name, field.value); err != nil {
			return err
		}
	}
	if err := policy.QwenPolicy.Validate(); err != nil {
		return fmt.Errorf("%w: Qwen policy: %v", ErrInvalidFinalCompliance, err)
	}
	if err := policy.OpusPolicy.Validate(); err != nil {
		return fmt.Errorf("%w: Opus policy: %v", ErrInvalidFinalCompliance, err)
	}
	if policy.QwenPolicy.MaxPassingTrials == nil || *policy.QwenPolicy.MaxPassingTrials != 1 {
		return fmt.Errorf("%w: Qwen policy must freeze max_passing_trials=1", ErrInvalidFinalCompliance)
	}
	if policy.QwenPolicy.LogicalTrialCount != 4 || policy.QwenPolicy.MinimumAverageTurns != 20 {
		return fmt.Errorf("%w: Qwen policy must freeze four trials and minimum_average_turns=20", ErrInvalidFinalCompliance)
	}
	if policy.OpusPolicy.MaxPassingTrials != nil {
		return fmt.Errorf("%w: Opus reference policy must not define a pass-count threshold", ErrInvalidFinalCompliance)
	}
	if policy.OpusPolicy.LogicalTrialCount != 4 {
		return fmt.Errorf("%w: Opus policy must freeze four logical trials", ErrInvalidFinalCompliance)
	}
	if policy.QwenPolicy.ID == policy.OpusPolicy.ID && policy.QwenPolicy.Version == policy.OpusPolicy.Version {
		return fmt.Errorf("%w: Qwen and Opus policies must use distinct identities", ErrInvalidFinalCompliance)
	}
	if policy.QwenPolicy.Evaluator == policy.OpusPolicy.Evaluator {
		return fmt.Errorf("%w: Qwen and Opus policies must use distinct evaluator identities", ErrInvalidFinalCompliance)
	}
	return nil
}

// CanonicalJSON returns a stable policy representation. Exception type order
// is non-semantic in the evaluator policy and is normalized here as well.
func (policy FinalCompliancePolicy) CanonicalJSON() ([]byte, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	canonical := policy.Clone()
	sort.Strings(canonical.QwenPolicy.InfraExceptionTypes)
	sort.Strings(canonical.OpusPolicy.InfraExceptionTypes)
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("%w: encode final compliance policy: %v", ErrInvalidFinalCompliance, err)
	}
	return encoded, nil
}

// Fingerprint returns the immutable policy identity frozen into a decision.
func (policy FinalCompliancePolicy) Fingerprint() (workflowkit.Fingerprint, error) {
	canonical, err := policy.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return workflowkit.FingerprintBytes(finalCompliancePolicyFingerprintDomain, canonical)
}

// FinalComplianceInput combines all evidence required before a CodeEdge local
// package can be created. Evaluator receipts remain bound to their dedicated
// child Run; the versioned handoff is the only authority that adopts them for
// this parent binding.
type FinalComplianceInput struct {
	Policy     FinalCompliancePolicy    `json:"policy"`
	Binding    FrozenRunBinding         `json:"binding"`
	Handoff    EvaluatorEvidenceHandoff `json:"evaluator_evidence_handoff"`
	Submission SubmissionCheckReceipt   `json:"submission"`
}

// FinalComplianceStatus is the durable outcome of final review. Rejected is
// valid, retained evidence; only Approved is eligible for package creation.
type FinalComplianceStatus string

const (
	FinalComplianceApproved FinalComplianceStatus = "approved"
	FinalComplianceRejected FinalComplianceStatus = "rejected"
)

// FinalComplianceDecision is the immutable aggregate evidence that governs
// package eligibility. It contains receipt fingerprints instead of copied
// external result documents, preserving the artifact store as the byte owner.
type FinalComplianceDecision struct {
	Format                              string                  `json:"format"`
	Version                             string                  `json:"version"`
	Status                              FinalComplianceStatus   `json:"status"`
	PolicyID                            string                  `json:"policy_id"`
	PolicyVersion                       string                  `json:"policy_version"`
	PolicyFingerprint                   workflowkit.Fingerprint `json:"policy_fingerprint"`
	Binding                             FrozenRunBinding        `json:"binding"`
	EvaluatorEvidenceHandoffFingerprint workflowkit.Fingerprint `json:"evaluator_evidence_handoff_fingerprint"`
	QwenReceiptFingerprint              workflowkit.Fingerprint `json:"qwen_receipt_fingerprint"`
	OpusReceiptFingerprint              workflowkit.Fingerprint `json:"opus_receipt_fingerprint"`
	SubmissionReceiptFingerprint        workflowkit.Fingerprint `json:"submission_receipt_fingerprint"`
	Reasons                             []string                `json:"reasons"`
}

// Clone returns an independently owned decision.
func (decision FinalComplianceDecision) Clone() FinalComplianceDecision {
	if decision.Reasons != nil {
		decision.Reasons = append([]string{}, decision.Reasons...)
	}
	return decision
}

// Validate verifies a self-contained final decision. A caller that stores or
// replays it can distinguish a rejected decision from malformed evidence.
func (decision FinalComplianceDecision) Validate() error {
	if decision.Format != FinalComplianceDecisionFormat || decision.Version != FinalComplianceDecisionVersion {
		return fmt.Errorf("%w: unsupported final decision format/version %q/%q", ErrInvalidFinalCompliance, decision.Format, decision.Version)
	}
	switch decision.Status {
	case FinalComplianceApproved, FinalComplianceRejected:
	default:
		return fmt.Errorf("%w: unsupported final decision status %q", ErrInvalidFinalCompliance, decision.Status)
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"final decision policy id", decision.PolicyID},
		{"final decision policy version", decision.PolicyVersion},
	} {
		if err := validateFinalComplianceText(field.name, field.value); err != nil {
			return err
		}
	}
	if err := decision.PolicyFingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: final decision policy fingerprint: %v", ErrInvalidFinalCompliance, err)
	}
	if err := decision.Binding.Validate(); err != nil {
		return err
	}
	if err := decision.EvaluatorEvidenceHandoffFingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: final decision evaluator evidence handoff fingerprint: %v", ErrInvalidFinalCompliance, err)
	}
	for _, field := range []struct {
		name  string
		value workflowkit.Fingerprint
	}{
		{"Qwen receipt fingerprint", decision.QwenReceiptFingerprint},
		{"Opus receipt fingerprint", decision.OpusReceiptFingerprint},
		{"submission receipt fingerprint", decision.SubmissionReceiptFingerprint},
	} {
		if err := field.value.Validate(); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInvalidFinalCompliance, field.name, err)
		}
	}
	if decision.Reasons == nil {
		return fmt.Errorf("%w: final decision reasons must be an explicit array", ErrInvalidFinalCompliance)
	}
	if decision.Status == FinalComplianceApproved && len(decision.Reasons) != 0 {
		return fmt.Errorf("%w: approved final decision cannot contain rejection reasons", ErrInvalidFinalCompliance)
	}
	if decision.Status == FinalComplianceRejected && len(decision.Reasons) == 0 {
		return fmt.Errorf("%w: rejected final decision requires a reason", ErrInvalidFinalCompliance)
	}
	seen := make(map[string]struct{}, len(decision.Reasons))
	for _, reason := range decision.Reasons {
		if err := validateFinalComplianceText("final decision reason", reason); err != nil {
			return err
		}
		if _, duplicate := seen[reason]; duplicate {
			return fmt.Errorf("%w: duplicate final decision reason %q", ErrInvalidFinalCompliance, reason)
		}
		seen[reason] = struct{}{}
	}
	return nil
}

// CanonicalJSON returns a stable final decision representation.
func (decision FinalComplianceDecision) CanonicalJSON() ([]byte, error) {
	if err := decision.Validate(); err != nil {
		return nil, err
	}
	canonical := decision.Clone()
	sort.Strings(canonical.Reasons)
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("%w: encode final decision: %v", ErrInvalidFinalCompliance, err)
	}
	return encoded, nil
}

// Fingerprint returns a domain-separated immutable decision identity.
func (decision FinalComplianceDecision) Fingerprint() (workflowkit.Fingerprint, error) {
	canonical, err := decision.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return workflowkit.FingerprintBytes(finalComplianceDecisionFingerprintDomain, canonical)
}

// LocalPackageAuthorization is the only CodeEdge package permission emitted
// by final compliance. A release/package adapter must persist and verify this
// exact authorization before it creates the immutable local ZIP.
type LocalPackageAuthorization struct {
	Format              string                  `json:"format"`
	Version             string                  `json:"version"`
	Decision            FinalComplianceDecision `json:"decision"`
	DecisionFingerprint workflowkit.Fingerprint `json:"decision_fingerprint"`
}

// Validate verifies that the authorization contains a complete approved
// decision and that its visible fingerprint agrees with the canonical body.
func (authorization LocalPackageAuthorization) Validate() error {
	if authorization.Format != LocalPackageAuthorizationFormat || authorization.Version != LocalPackageAuthorizationVersion {
		return fmt.Errorf("%w: unsupported local package authorization format/version %q/%q", ErrInvalidFinalCompliance, authorization.Format, authorization.Version)
	}
	if err := authorization.Decision.Validate(); err != nil {
		return err
	}
	if authorization.Decision.Status != FinalComplianceApproved {
		return fmt.Errorf("%w: local package authorization requires an approved final decision", ErrInvalidFinalCompliance)
	}
	if err := authorization.DecisionFingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: local package authorization decision fingerprint: %v", ErrInvalidFinalCompliance, err)
	}
	fingerprint, err := authorization.Decision.Fingerprint()
	if err != nil {
		return err
	}
	if authorization.DecisionFingerprint != fingerprint {
		return fmt.Errorf("%w: local package authorization decision fingerprint does not match decision", ErrInvalidFinalCompliance)
	}
	return nil
}

// CanonicalJSON returns a stable authorization representation suitable for a
// package receipt or an immutable lineage artifact.
func (authorization LocalPackageAuthorization) CanonicalJSON() ([]byte, error) {
	if err := authorization.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(authorization)
	if err != nil {
		return nil, fmt.Errorf("%w: encode local package authorization: %v", ErrInvalidFinalCompliance, err)
	}
	return encoded, nil
}

// Fingerprint returns the immutable authorization identity that a package
// receipt can pin alongside its package object digest.
func (authorization LocalPackageAuthorization) Fingerprint() (workflowkit.Fingerprint, error) {
	canonical, err := authorization.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return workflowkit.FingerprintBytes(localPackageAuthorizationFingerprintDomain, canonical)
}

// FinalComplianceResult returns the durable decision and, only on approval,
// the typed authorization required by a local package adapter.
type FinalComplianceResult struct {
	Decision      FinalComplianceDecision    `json:"decision"`
	Authorization *LocalPackageAuthorization `json:"authorization,omitempty"`
}

// FinalComplianceService is stateless by design. Deployment composition
// supplies the complete frozen policy and receipts for every evaluation; the
// service has no hidden defaults or access to external provider configuration.
type FinalComplianceService struct{}

// Evaluate verifies all binding and evidence invariants, then produces a
// durable approved or rejected decision. Malformed, cross-run, or policy-drift
// evidence returns an error because it cannot safely be treated as a result.
func (FinalComplianceService) Evaluate(input FinalComplianceInput) (FinalComplianceResult, error) {
	if err := input.Policy.Validate(); err != nil {
		return FinalComplianceResult{}, err
	}
	if err := input.Binding.Validate(); err != nil {
		return FinalComplianceResult{}, err
	}
	if err := input.Handoff.Validate(); err != nil {
		return FinalComplianceResult{}, err
	}
	if !sameFrozenRunBinding(input.Handoff.ParentBinding, input.Binding) {
		return FinalComplianceResult{}, fmt.Errorf("%w: evaluator evidence handoff is bound to another parent Run", ErrInvalidFinalCompliance)
	}

	policyFingerprint, err := input.Policy.Fingerprint()
	if err != nil {
		return FinalComplianceResult{}, err
	}
	handoffFingerprint, err := input.Handoff.Fingerprint()
	if err != nil {
		return FinalComplianceResult{}, err
	}
	qwen := input.Handoff.Qwen.Receipt.Clone()
	opus := input.Handoff.Opus.Receipt.Clone()
	qwenFingerprint, err := validateEvaluationForFinalCompliance("Qwen", qwen, input.Policy.QwenPolicy, input.Handoff.ChildBinding)
	if err != nil {
		return FinalComplianceResult{}, err
	}
	opusFingerprint, err := validateEvaluationForFinalCompliance("Opus", opus, input.Policy.OpusPolicy, input.Handoff.ChildBinding)
	if err != nil {
		return FinalComplianceResult{}, err
	}
	submissionFingerprint, err := validateSubmissionForFinalCompliance(input.Submission, input.Policy, input.Binding)
	if err != nil {
		return FinalComplianceResult{}, err
	}

	reasons := finalComplianceReasons(qwen, opus, input.Submission)
	status := FinalComplianceApproved
	if len(reasons) != 0 {
		status = FinalComplianceRejected
	}
	decision := FinalComplianceDecision{
		Format:                              FinalComplianceDecisionFormat,
		Version:                             FinalComplianceDecisionVersion,
		Status:                              status,
		PolicyID:                            input.Policy.ID,
		PolicyVersion:                       input.Policy.Version,
		PolicyFingerprint:                   policyFingerprint,
		Binding:                             input.Binding,
		EvaluatorEvidenceHandoffFingerprint: handoffFingerprint,
		QwenReceiptFingerprint:              qwenFingerprint,
		OpusReceiptFingerprint:              opusFingerprint,
		SubmissionReceiptFingerprint:        submissionFingerprint,
		Reasons:                             reasons,
	}
	if err := decision.Validate(); err != nil {
		return FinalComplianceResult{}, err
	}
	result := FinalComplianceResult{Decision: decision}
	if decision.Status == FinalComplianceApproved {
		authorization, err := (FinalComplianceService{}).IssueLocalPackageAuthorization(decision)
		if err != nil {
			return FinalComplianceResult{}, err
		}
		result.Authorization = &authorization
	}
	return result, nil
}

// IssueLocalPackageAuthorization reconstructs the same authorization during
// a durable replay. It never authorizes a rejected decision.
func (FinalComplianceService) IssueLocalPackageAuthorization(decision FinalComplianceDecision) (LocalPackageAuthorization, error) {
	if err := decision.Validate(); err != nil {
		return LocalPackageAuthorization{}, err
	}
	if decision.Status != FinalComplianceApproved {
		return LocalPackageAuthorization{}, fmt.Errorf("%w: final decision is %s", ErrFinalComplianceRejected, decision.Status)
	}
	fingerprint, err := decision.Fingerprint()
	if err != nil {
		return LocalPackageAuthorization{}, err
	}
	authorization := LocalPackageAuthorization{
		Format:              LocalPackageAuthorizationFormat,
		Version:             LocalPackageAuthorizationVersion,
		Decision:            decision.Clone(),
		DecisionFingerprint: fingerprint,
	}
	if err := authorization.Validate(); err != nil {
		return LocalPackageAuthorization{}, err
	}
	return authorization, nil
}

func validateEvaluationForFinalCompliance(role string, receipt EvaluationReceipt, policy EvaluationPolicy, binding FrozenRunBinding) (workflowkit.Fingerprint, error) {
	if err := receipt.Validate(); err != nil {
		return "", fmt.Errorf("%w: %s evaluation receipt: %v", ErrInvalidFinalCompliance, role, err)
	}
	policyFingerprint, err := policy.Fingerprint()
	if err != nil {
		return "", fmt.Errorf("%w: %s policy: %v", ErrInvalidFinalCompliance, role, err)
	}
	if receipt.PolicyID != policy.ID || receipt.PolicyVersion != policy.Version || receipt.PolicyFingerprint != policyFingerprint {
		return "", fmt.Errorf("%w: %s evaluation receipt does not match its frozen policy", ErrInvalidFinalCompliance, role)
	}
	if receipt.Evaluator != policy.Evaluator || receipt.HarborEvidenceFormat != policy.HarborEvidenceFormat {
		return "", fmt.Errorf("%w: %s evaluation receipt evaluator or result format drift", ErrInvalidFinalCompliance, role)
	}
	receiptBinding := FrozenRunBinding{
		TaskSnapshotDigest:  receipt.TaskSnapshotDigest,
		CatalogFingerprint:  receipt.CatalogFingerprint,
		LockFingerprint:     receipt.LockFingerprint,
		ManifestFingerprint: receipt.ManifestFingerprint,
	}
	if !sameFrozenRunBinding(receiptBinding, binding) {
		return "", fmt.Errorf("%w: %s evaluation receipt is bound to another task, catalog, lock, or manifest", ErrInvalidFinalCompliance, role)
	}
	fingerprint, err := receipt.Fingerprint()
	if err != nil {
		return "", fmt.Errorf("%w: fingerprint %s evaluation receipt: %v", ErrInvalidFinalCompliance, role, err)
	}
	return fingerprint, nil
}

func validateSubmissionForFinalCompliance(receipt SubmissionCheckReceipt, policy FinalCompliancePolicy, binding FrozenRunBinding) (workflowkit.Fingerprint, error) {
	if err := receipt.Validate(); err != nil {
		return "", err
	}
	if receipt.CheckerID != policy.SubmissionCheckerID || receipt.CheckerVersion != policy.SubmissionCheckerVersion {
		return "", fmt.Errorf("%w: submission receipt does not match the frozen checker identity", ErrInvalidFinalCompliance)
	}
	if receipt.Report.SchemaVersion != policy.SubmissionReportSchemaVersion {
		return "", fmt.Errorf("%w: submission receipt report schema does not match the frozen policy", ErrInvalidFinalCompliance)
	}
	if !sameFrozenRunBinding(receipt.Binding, binding) {
		return "", fmt.Errorf("%w: submission receipt is bound to another task, catalog, lock, or manifest", ErrInvalidFinalCompliance)
	}
	fingerprint, err := receipt.Fingerprint()
	if err != nil {
		return "", fmt.Errorf("%w: fingerprint submission receipt: %v", ErrInvalidFinalCompliance, err)
	}
	return fingerprint, nil
}

func finalComplianceReasons(qwen, opus EvaluationReceipt, submission SubmissionCheckReceipt) []string {
	reasons := make([]string, 0, 5)
	if qwen.Status != EvaluationCompleted {
		reasons = append(reasons, "Qwen evaluation is not a completed trusted four-trial result")
	}
	if !qwen.PolicyCompliant {
		reasons = append(reasons, "Qwen evaluation did not comply with its frozen hard-gate policy")
	}
	if qwen.PassCount > 1 {
		reasons = append(reasons, "Qwen pass count exceeds the maximum of one")
	}
	if qwen.AverageTurns < 20 {
		reasons = append(reasons, "Qwen average turns is below the required minimum of twenty")
	}
	// Opus is reference-only, but it must still be a complete, trusted
	// four-trial receipt with exactly one canonical screenshot. Validate above
	// proves its structural rules; status prevents incomplete infrastructure
	// evidence from being silently accepted as a reference.
	if opus.Status != EvaluationCompleted {
		reasons = append(reasons, "Opus reference evaluation is not a completed trusted four-trial result")
	}
	switch submission.Status {
	case SubmissionCheckPassed:
	case SubmissionCheckRejected:
		reasons = append(reasons, "submission checks rejected the task")
	case SubmissionCheckInfraFailed:
		reasons = append(reasons, "submission checks did not produce a trusted completed result")
	}
	sort.Strings(reasons)
	return reasons
}

func validateFinalComplianceText(label, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidFinalCompliance, label)
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("%w: %s contains a control character", ErrInvalidFinalCompliance, label)
		}
	}
	return nil
}
