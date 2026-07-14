package codeedge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// HarborJobResultV018 is the result.json contract inspected from the
	// deployment-approved Harbor 0.18.0 installation.  The parser accepts only
	// the fields that are evidence-bearing for a CodeEdge evaluation and rejects
	// duplicate JSON keys, so a future Harbor result layout requires an explicit
	// parser-version/catalog update rather than silent reinterpretation.
	HarborJobResultV018 = "harbor.job-result.v0.18"

	// EvaluationReceiptFormat identifies the immutable, domain-owned receipt
	// produced after one evaluator has completed its four logical trials.
	EvaluationReceiptFormat     = "codeedge.phase1.evaluation-receipt.v1"
	EvaluationReceiptVersion    = "1"
	evaluationFingerprintDomain = "harbor.codeedge.phase1.evaluation-receipt.v1"
)

var (
	// ErrInvalidEvaluationPolicy marks a policy that would leave a CodeEdge
	// evaluator's model, result parser, pass rule, screenshot, or failure
	// classifier ambiguous.
	ErrInvalidEvaluationPolicy = errors.New("CodeEdge Phase-1 evaluation: invalid policy")
	// ErrInvalidEvaluationEvidence marks missing, altered, or structurally
	// incomplete evidence.  It is intentionally distinct from a model result:
	// invalid evidence cannot be counted as a model failure.
	ErrInvalidEvaluationEvidence = errors.New("CodeEdge Phase-1 evaluation: invalid evidence")
	// ErrInvalidHarborResult marks a malformed or unsupported trusted
	// result.json document.  Runtime/caller code should classify this as an
	// infrastructure/reconcile condition, never as a failed model trial.
	ErrInvalidHarborResult = errors.New("CodeEdge Phase-1 evaluation: invalid Harbor result")
)

// EvaluatorIdentity is the exact agent/model identity expected in result.json.
// The catalog lock independently attests the executable/model configuration;
// this identity makes that expectation visible in the durable receipt as well.
type EvaluatorIdentity struct {
	ProfileID      string `json:"profile_id"`
	ProfileVersion string `json:"profile_version"`
	AgentName      string `json:"agent_name"`
	AgentVersion   string `json:"agent_version"`
	ModelName      string `json:"model_name"`
	ModelProvider  string `json:"model_provider"`
}

// EvaluationPolicy is a versioned CodeEdge rule set.  It deliberately has no
// default values: deployment composition must freeze the actual Harbor CLI,
// evaluator, reward semantics, screenshot representation, and classifier.
//
// MaxPassingTrials is nil for informational evaluators such as the confirmed
// Opus reference.  A non-nil value is a content-compliance rule (for example
// Qwen's pass_count <= 1); it does not turn an infrastructure failure into a
// failed model trial.
type EvaluationPolicy struct {
	ID                       string            `json:"id"`
	Version                  string            `json:"version"`
	HarborResultFormat       string            `json:"harbor_result_format"`
	Evaluator                EvaluatorIdentity `json:"evaluator"`
	LogicalTrialCount        int               `json:"logical_trial_count"`
	PassRewardKey            string            `json:"pass_reward_key"`
	PassRewardAtLeast        float64           `json:"pass_reward_at_least"`
	MaxPassingTrials         *int              `json:"max_passing_trials,omitempty"`
	MinimumAverageTurns      int               `json:"minimum_average_turns"`
	ScreenshotMediaType      string            `json:"screenshot_media_type"`
	FailureClassifierID      string            `json:"failure_classifier_id"`
	FailureClassifierVersion string            `json:"failure_classifier_version"`
	InfraExceptionTypes      []string          `json:"infra_exception_types"`
}

// Clone returns an independently owned policy value.
func (policy EvaluationPolicy) Clone() EvaluationPolicy {
	policy.InfraExceptionTypes = append([]string(nil), policy.InfraExceptionTypes...)
	if policy.MaxPassingTrials != nil {
		value := *policy.MaxPassingTrials
		policy.MaxPassingTrials = &value
	}
	return policy
}

// Validate verifies all decision-bearing evaluator inputs.  CodeEdge
// Phase-1's confirmed contract intentionally permits only four logical trials
// and a minimum average turn threshold of twenty; a policy version bump is
// required if that program rule changes.
func (policy EvaluationPolicy) Validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"policy id", policy.ID},
		{"policy version", policy.Version},
		{"Harbor result format", policy.HarborResultFormat},
		{"evaluator profile id", policy.Evaluator.ProfileID},
		{"evaluator profile version", policy.Evaluator.ProfileVersion},
		{"agent name", policy.Evaluator.AgentName},
		{"agent version", policy.Evaluator.AgentVersion},
		{"model name", policy.Evaluator.ModelName},
		{"model provider", policy.Evaluator.ModelProvider},
		{"pass reward key", policy.PassRewardKey},
		{"screenshot media type", policy.ScreenshotMediaType},
		{"failure classifier id", policy.FailureClassifierID},
		{"failure classifier version", policy.FailureClassifierVersion},
	} {
		if err := validateEvaluationText(field.name, field.value); err != nil {
			return err
		}
	}
	if policy.HarborResultFormat != HarborJobResultV018 {
		return fmt.Errorf("%w: unsupported Harbor result format %q", ErrInvalidEvaluationPolicy, policy.HarborResultFormat)
	}
	if policy.LogicalTrialCount != 4 {
		return fmt.Errorf("%w: CodeEdge Phase-1 requires exactly four logical trials", ErrInvalidEvaluationPolicy)
	}
	if math.IsNaN(policy.PassRewardAtLeast) || math.IsInf(policy.PassRewardAtLeast, 0) {
		return fmt.Errorf("%w: pass reward threshold must be finite", ErrInvalidEvaluationPolicy)
	}
	if policy.MaxPassingTrials != nil && (*policy.MaxPassingTrials < 0 || *policy.MaxPassingTrials > policy.LogicalTrialCount) {
		return fmt.Errorf("%w: max passing trials must be between zero and %d", ErrInvalidEvaluationPolicy, policy.LogicalTrialCount)
	}
	if policy.MinimumAverageTurns < 20 {
		return fmt.Errorf("%w: CodeEdge Phase-1 minimum average turns cannot be below 20", ErrInvalidEvaluationPolicy)
	}
	if !supportedScreenshotMediaType(policy.ScreenshotMediaType) {
		return fmt.Errorf("%w: unsupported screenshot media type %q", ErrInvalidEvaluationPolicy, policy.ScreenshotMediaType)
	}
	if policy.InfraExceptionTypes == nil {
		return fmt.Errorf("%w: infra exception types must be an explicit array", ErrInvalidEvaluationPolicy)
	}
	seen := make(map[string]struct{}, len(policy.InfraExceptionTypes))
	for _, exceptionType := range policy.InfraExceptionTypes {
		if err := validateEvaluationText("infra exception type", exceptionType); err != nil {
			return err
		}
		if _, duplicate := seen[exceptionType]; duplicate {
			return fmt.Errorf("%w: duplicate infra exception type %q", ErrInvalidEvaluationPolicy, exceptionType)
		}
		seen[exceptionType] = struct{}{}
	}
	return nil
}

// Fingerprint returns the canonical policy identity frozen into an evaluation
// receipt.  Array ordering for exception classes is non-semantic and is
// normalized before hashing.
func (policy EvaluationPolicy) Fingerprint() (workflowkit.Fingerprint, error) {
	if err := policy.Validate(); err != nil {
		return "", err
	}
	canonical := policy.Clone()
	sort.Strings(canonical.InfraExceptionTypes)
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: encode policy: %v", ErrInvalidEvaluationPolicy, err)
	}
	return workflowkit.FingerprintBytes("harbor.codeedge.phase1.evaluation-policy.v1", encoded)
}

// EvaluationBinding captures facts that must agree with the frozen Run before
// result.json can become reusable evidence.  PackageDigest is deliberately
// absent: the confirmed Phase-1 flow evaluates a managed task snapshot and
// creates the single local package only after final compliance succeeds.
type EvaluationBinding struct {
	TaskSnapshotDigest       workflowkit.SubjectDigest `json:"task_snapshot_digest"`
	ExpectedHarborTaskDigest string                    `json:"expected_harbor_task_digest"`
	HarborCLI                HarborCLIIdentity         `json:"harbor_cli"`
	CatalogFingerprint       workflowkit.Fingerprint   `json:"catalog_fingerprint"`
	LockFingerprint          workflowkit.Fingerprint   `json:"lock_fingerprint"`
	ManifestFingerprint      workflowkit.Fingerprint   `json:"manifest_fingerprint"`
}

// HarborCLIIdentity is the executable identity that produced trusted
// result.json. It records no path or credentials: deployment lock evidence
// owns the resolved path while this receipt preserves the stable command,
// release, and content identity visible to a reviewer.
type HarborCLIIdentity struct {
	CommandID          string                  `json:"command_id"`
	Version            string                  `json:"version"`
	ContentFingerprint workflowkit.Fingerprint `json:"content_fingerprint"`
}

// Validate proves that the receipt names a concrete frozen Harbor CLI.
func (identity HarborCLIIdentity) Validate() error {
	if err := validateEvaluationText("Harbor CLI command id", identity.CommandID); err != nil {
		return err
	}
	if err := validateEvaluationText("Harbor CLI version", identity.Version); err != nil {
		return err
	}
	if err := identity.ContentFingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: Harbor CLI content fingerprint: %v", ErrInvalidEvaluationEvidence, err)
	}
	return nil
}

// Validate proves an evaluator receipt can be tied back to a frozen task and
// deployment contract.  ExpectedHarborTaskDigest is the Harbor-controlled
// task directory digest reported by result.json; it is separate from, and is
// additionally bound to, Harbor Flow's V2 snapshot digest.
func (binding EvaluationBinding) Validate() error {
	if err := binding.TaskSnapshotDigest.Validate(); err != nil {
		return fmt.Errorf("%w: task snapshot digest: %v", ErrInvalidEvaluationEvidence, err)
	}
	if err := validateEvaluationText("expected Harbor task digest", binding.ExpectedHarborTaskDigest); err != nil {
		return err
	}
	if err := binding.HarborCLI.Validate(); err != nil {
		return err
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
			return fmt.Errorf("%w: %s: %v", ErrInvalidEvaluationEvidence, field.name, err)
		}
	}
	return nil
}

// EvaluationEvidence identifies one immutable artifact and supplies the
// bytes which were read to build a receipt.  The builder recomputes the
// content digest, preventing a caller from binding a visible screenshot or
// result to a different stored artifact.
type EvaluationEvidence struct {
	ArtifactID    workflowkit.ArtifactID  `json:"artifact_id"`
	ContentDigest workflowkit.Fingerprint `json:"content_digest"`
	SchemaVersion string                  `json:"schema_version"`
	MediaType     string                  `json:"media_type"`
	Bytes         []byte                  `json:"-"`
}

func (evidence EvaluationEvidence) validate(name string) error {
	if err := validateEvaluationText(name+" artifact id", string(evidence.ArtifactID)); err != nil {
		return err
	}
	if err := evidence.ContentDigest.Validate(); err != nil {
		return fmt.Errorf("%w: %s artifact digest: %v", ErrInvalidEvaluationEvidence, name, err)
	}
	if err := validateEvaluationText(name+" artifact schema version", evidence.SchemaVersion); err != nil {
		return err
	}
	if err := validateEvaluationText(name+" artifact media type", evidence.MediaType); err != nil {
		return err
	}
	if len(evidence.Bytes) == 0 {
		return fmt.Errorf("%w: %s artifact bytes are required", ErrInvalidEvaluationEvidence, name)
	}
	if workflowkit.SHA256Fingerprint(evidence.Bytes) != evidence.ContentDigest {
		return fmt.Errorf("%w: %s artifact digest does not match bytes", ErrInvalidEvaluationEvidence, name)
	}
	return nil
}

// EvaluationInput combines immutable policy, run binding, result.json, and
// the one canonical screenshot.  A slice is intentionally not used for the
// screenshot: a receipt structurally cannot carry two screenshots for one
// evaluator, which enforces the Phase-1 "one image per model" rule.
type EvaluationInput struct {
	Policy              EvaluationPolicy   `json:"policy"`
	Binding             EvaluationBinding  `json:"binding"`
	HarborResult        EvaluationEvidence `json:"harbor_result"`
	CanonicalScreenshot EvaluationEvidence `json:"canonical_screenshot"`
}

// EvaluationStatus distinguishes a trustworthy completed aggregation from an
// infrastructure interruption.  A completed-but-noncompliant result remains
// evidence and is surfaced as a content decision; an infra failure is never
// counted as a model failure.
type EvaluationStatus string

const (
	EvaluationCompleted   EvaluationStatus = "completed"
	EvaluationInfraFailed EvaluationStatus = "infra_failed"
)

// EvaluationTrialStatus is the per-logical-trial classification persisted in
// the receipt.  Harbor's internal retries replace a trial result under the
// same trial name; result.json therefore contributes exactly one final record
// for each logical sample.
type EvaluationTrialStatus string

const (
	EvaluationTrialCompleted   EvaluationTrialStatus = "completed"
	EvaluationTrialInfraFailed EvaluationTrialStatus = "infra_failed"
)

// EvaluationTrialReceipt is one final logical trial projected from trusted
// Harbor result.json.  HarborTrialID/name remain external evidence identities;
// the lifecycle adapter can additionally persist its own UUIDv7 TrialExecution
// entity without changing this immutable receipt schema.
type EvaluationTrialReceipt struct {
	HarborTrialID   string                `json:"harbor_trial_id"`
	HarborTrialName string                `json:"harbor_trial_name"`
	Status          EvaluationTrialStatus `json:"status"`
	Passed          bool                  `json:"passed"`
	TurnCount       int                   `json:"turn_count"`
	ElapsedMillis   int64                 `json:"elapsed_millis"`
	FailureType     string                `json:"failure_type,omitempty"`
}

// EvaluationReceipt is the immutable evidence summary for one evaluator.  It
// records both actual aggregate facts and policy compliance so a TUI/review
// can explain a noncompliant task without conflating it with an infra failure.
type EvaluationReceipt struct {
	Format                  string                    `json:"format"`
	Version                 string                    `json:"version"`
	Status                  EvaluationStatus          `json:"status"`
	PolicyID                string                    `json:"policy_id"`
	PolicyVersion           string                    `json:"policy_version"`
	PolicyFingerprint       workflowkit.Fingerprint   `json:"policy_fingerprint"`
	Evaluator               EvaluatorIdentity         `json:"evaluator"`
	HarborResultFormat      string                    `json:"harbor_result_format"`
	HarborCLI               HarborCLIIdentity         `json:"harbor_cli"`
	HarborJobID             string                    `json:"harbor_job_id"`
	HarborTaskDigest        string                    `json:"harbor_task_digest"`
	TaskSnapshotDigest      workflowkit.SubjectDigest `json:"task_snapshot_digest"`
	CatalogFingerprint      workflowkit.Fingerprint   `json:"catalog_fingerprint"`
	LockFingerprint         workflowkit.Fingerprint   `json:"lock_fingerprint"`
	ManifestFingerprint     workflowkit.Fingerprint   `json:"manifest_fingerprint"`
	ResultArtifactID        workflowkit.ArtifactID    `json:"result_artifact_id"`
	ResultContentDigest     workflowkit.Fingerprint   `json:"result_content_digest"`
	ScreenshotArtifactID    workflowkit.ArtifactID    `json:"screenshot_artifact_id"`
	ScreenshotContentDigest workflowkit.Fingerprint   `json:"screenshot_content_digest"`
	ScreenshotMediaType     string                    `json:"screenshot_media_type"`
	Trials                  []EvaluationTrialReceipt  `json:"trials"`
	PassCount               int                       `json:"pass_count"`
	AverageTurns            float64                   `json:"average_turns"`
	PolicyCompliant         bool                      `json:"policy_compliant"`
	ComplianceReasons       []string                  `json:"compliance_reasons"`
}

// Clone returns an independently owned receipt.
func (receipt EvaluationReceipt) Clone() EvaluationReceipt {
	receipt.Trials = append([]EvaluationTrialReceipt(nil), receipt.Trials...)
	receipt.ComplianceReasons = append([]string(nil), receipt.ComplianceReasons...)
	return receipt
}

// CanonicalJSON returns a stable receipt representation suitable for an
// immutable artifact.  Trial/reason ordering is normalized rather than being
// inherited from an external CLI's completion timing.
func (receipt EvaluationReceipt) CanonicalJSON() ([]byte, error) {
	if err := receipt.Validate(); err != nil {
		return nil, err
	}
	canonical := receipt.Clone()
	sort.Slice(canonical.Trials, func(left, right int) bool {
		if canonical.Trials[left].HarborTrialName != canonical.Trials[right].HarborTrialName {
			return canonical.Trials[left].HarborTrialName < canonical.Trials[right].HarborTrialName
		}
		return canonical.Trials[left].HarborTrialID < canonical.Trials[right].HarborTrialID
	})
	sort.Strings(canonical.ComplianceReasons)
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("%w: encode receipt: %v", ErrInvalidEvaluationEvidence, err)
	}
	return encoded, nil
}

// Fingerprint derives a domain-separated immutable receipt identity.
func (receipt EvaluationReceipt) Fingerprint() (workflowkit.Fingerprint, error) {
	canonical, err := receipt.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return workflowkit.FingerprintBytes(evaluationFingerprintDomain, canonical)
}

// Validate proves that a receipt remains self-consistent after persistence.
func (receipt EvaluationReceipt) Validate() error {
	if receipt.Format != EvaluationReceiptFormat || receipt.Version != EvaluationReceiptVersion {
		return fmt.Errorf("%w: unsupported receipt format/version %q/%q", ErrInvalidEvaluationEvidence, receipt.Format, receipt.Version)
	}
	if receipt.Status != EvaluationCompleted && receipt.Status != EvaluationInfraFailed {
		return fmt.Errorf("%w: unsupported receipt status %q", ErrInvalidEvaluationEvidence, receipt.Status)
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"policy id", receipt.PolicyID}, {"policy version", receipt.PolicyVersion},
		{"Harbor result format", receipt.HarborResultFormat}, {"Harbor job id", receipt.HarborJobID},
		{"Harbor task digest", receipt.HarborTaskDigest}, {"result artifact id", string(receipt.ResultArtifactID)},
		{"screenshot artifact id", string(receipt.ScreenshotArtifactID)}, {"screenshot media type", receipt.ScreenshotMediaType},
	} {
		if err := validateEvaluationText(field.name, field.value); err != nil {
			return err
		}
	}
	if receipt.HarborResultFormat != HarborJobResultV018 || !supportedScreenshotMediaType(receipt.ScreenshotMediaType) {
		return fmt.Errorf("%w: unsupported frozen evidence format", ErrInvalidEvaluationEvidence)
	}
	if err := receipt.HarborCLI.Validate(); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"evaluator profile id", receipt.Evaluator.ProfileID}, {"evaluator profile version", receipt.Evaluator.ProfileVersion},
		{"evaluator agent name", receipt.Evaluator.AgentName}, {"evaluator agent version", receipt.Evaluator.AgentVersion},
		{"evaluator model name", receipt.Evaluator.ModelName}, {"evaluator model provider", receipt.Evaluator.ModelProvider},
	} {
		if err := validateEvaluationText(field.name, field.value); err != nil {
			return err
		}
	}
	if err := receipt.PolicyFingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: policy fingerprint: %v", ErrInvalidEvaluationEvidence, err)
	}
	if err := receipt.TaskSnapshotDigest.Validate(); err != nil {
		return fmt.Errorf("%w: task snapshot digest: %v", ErrInvalidEvaluationEvidence, err)
	}
	for _, field := range []struct {
		name  string
		value workflowkit.Fingerprint
	}{
		{"catalog fingerprint", receipt.CatalogFingerprint}, {"lock fingerprint", receipt.LockFingerprint},
		{"manifest fingerprint", receipt.ManifestFingerprint}, {"result content digest", receipt.ResultContentDigest},
		{"screenshot content digest", receipt.ScreenshotContentDigest},
	} {
		if err := field.value.Validate(); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInvalidEvaluationEvidence, field.name, err)
		}
	}
	if len(receipt.Trials) != 4 {
		return fmt.Errorf("%w: receipt must contain exactly four trials", ErrInvalidEvaluationEvidence)
	}
	seen := make(map[string]struct{}, len(receipt.Trials))
	completed := 0
	passCount := 0
	turnTotal := 0
	for _, trial := range receipt.Trials {
		if err := validateEvaluationText("Harbor trial id", trial.HarborTrialID); err != nil {
			return err
		}
		if err := validateEvaluationText("Harbor trial name", trial.HarborTrialName); err != nil {
			return err
		}
		if _, duplicate := seen[trial.HarborTrialName]; duplicate {
			return fmt.Errorf("%w: duplicate Harbor trial name %q", ErrInvalidEvaluationEvidence, trial.HarborTrialName)
		}
		seen[trial.HarborTrialName] = struct{}{}
		if trial.ElapsedMillis < 0 || trial.TurnCount < 0 {
			return fmt.Errorf("%w: invalid trial timing or turns", ErrInvalidEvaluationEvidence)
		}
		switch trial.Status {
		case EvaluationTrialCompleted:
			completed++
			turnTotal += trial.TurnCount
			if trial.Passed {
				passCount++
			}
		case EvaluationTrialInfraFailed:
			if trial.Passed || strings.TrimSpace(trial.FailureType) == "" {
				return fmt.Errorf("%w: infra trial must have a failure type and cannot pass", ErrInvalidEvaluationEvidence)
			}
		default:
			return fmt.Errorf("%w: unsupported trial status %q", ErrInvalidEvaluationEvidence, trial.Status)
		}
	}
	if receipt.Status == EvaluationCompleted && completed != len(receipt.Trials) {
		return fmt.Errorf("%w: completed receipt contains infrastructure-failed trial", ErrInvalidEvaluationEvidence)
	}
	if receipt.Status == EvaluationInfraFailed && completed == len(receipt.Trials) {
		return fmt.Errorf("%w: infrastructure-failed receipt has no infrastructure trial", ErrInvalidEvaluationEvidence)
	}
	if receipt.PassCount != passCount {
		return fmt.Errorf("%w: pass count does not match trials", ErrInvalidEvaluationEvidence)
	}
	if completed > 0 {
		expectedAverage := float64(turnTotal) / float64(completed)
		if receipt.AverageTurns != expectedAverage {
			return fmt.Errorf("%w: average turns does not match trials", ErrInvalidEvaluationEvidence)
		}
	}
	return nil
}

// BuildEvaluationReceipt parses a trusted Harbor 0.18 result.json and emits
// one durable evaluator receipt.  It preserves a classified infra failure as
// evidence instead of returning it as a model failure; malformed/drifting
// inputs remain errors because they cannot be trusted evidence at all.
func BuildEvaluationReceipt(input EvaluationInput) (EvaluationReceipt, error) {
	if err := input.Policy.Validate(); err != nil {
		return EvaluationReceipt{}, err
	}
	if err := input.Binding.Validate(); err != nil {
		return EvaluationReceipt{}, err
	}
	if err := input.HarborResult.validate("Harbor result"); err != nil {
		return EvaluationReceipt{}, err
	}
	if err := input.CanonicalScreenshot.validate("canonical screenshot"); err != nil {
		return EvaluationReceipt{}, err
	}
	if input.HarborResult.MediaType != "application/json" {
		return EvaluationReceipt{}, fmt.Errorf("%w: Harbor result must use application/json", ErrInvalidEvaluationEvidence)
	}
	if input.CanonicalScreenshot.MediaType != input.Policy.ScreenshotMediaType {
		return EvaluationReceipt{}, fmt.Errorf("%w: screenshot media type %q does not match frozen policy %q", ErrInvalidEvaluationEvidence, input.CanonicalScreenshot.MediaType, input.Policy.ScreenshotMediaType)
	}
	if err := validateScreenshot(input.CanonicalScreenshot.Bytes, input.CanonicalScreenshot.MediaType); err != nil {
		return EvaluationReceipt{}, err
	}
	result, err := ParseHarborJobResultV018(input.HarborResult.Bytes)
	if err != nil {
		return EvaluationReceipt{}, err
	}
	if result.TotalTrials != input.Policy.LogicalTrialCount || len(result.Trials) != input.Policy.LogicalTrialCount {
		return EvaluationReceipt{}, fmt.Errorf("%w: Harbor result must contain exactly %d logical trials", ErrInvalidHarborResult, input.Policy.LogicalTrialCount)
	}
	policyFingerprint, err := input.Policy.Fingerprint()
	if err != nil {
		return EvaluationReceipt{}, err
	}

	trials := append([]HarborTrialResult(nil), result.Trials...)
	sort.Slice(trials, func(left, right int) bool {
		if trials[left].TrialName != trials[right].TrialName {
			return trials[left].TrialName < trials[right].TrialName
		}
		return trials[left].ID < trials[right].ID
	})
	receipt := EvaluationReceipt{
		Format:                  EvaluationReceiptFormat,
		Version:                 EvaluationReceiptVersion,
		Status:                  EvaluationCompleted,
		PolicyID:                input.Policy.ID,
		PolicyVersion:           input.Policy.Version,
		PolicyFingerprint:       policyFingerprint,
		Evaluator:               input.Policy.Evaluator,
		HarborResultFormat:      input.Policy.HarborResultFormat,
		HarborCLI:               input.Binding.HarborCLI,
		HarborJobID:             result.ID,
		HarborTaskDigest:        input.Binding.ExpectedHarborTaskDigest,
		TaskSnapshotDigest:      input.Binding.TaskSnapshotDigest,
		CatalogFingerprint:      input.Binding.CatalogFingerprint,
		LockFingerprint:         input.Binding.LockFingerprint,
		ManifestFingerprint:     input.Binding.ManifestFingerprint,
		ResultArtifactID:        input.HarborResult.ArtifactID,
		ResultContentDigest:     input.HarborResult.ContentDigest,
		ScreenshotArtifactID:    input.CanonicalScreenshot.ArtifactID,
		ScreenshotContentDigest: input.CanonicalScreenshot.ContentDigest,
		ScreenshotMediaType:     input.CanonicalScreenshot.MediaType,
		Trials:                  make([]EvaluationTrialReceipt, 0, len(trials)),
	}
	infraTypes := make(map[string]struct{}, len(input.Policy.InfraExceptionTypes))
	for _, exceptionType := range input.Policy.InfraExceptionTypes {
		infraTypes[exceptionType] = struct{}{}
	}
	turnTotal := 0
	completedTrials := 0
	for _, trial := range trials {
		if trial.TaskDigest != input.Binding.ExpectedHarborTaskDigest {
			return EvaluationReceipt{}, fmt.Errorf("%w: Harbor trial %q task digest does not match frozen checkout", ErrInvalidHarborResult, trial.TrialName)
		}
		if err := validateResultEvaluatorIdentity(trial.Evaluator, input.Policy.Evaluator); err != nil {
			return EvaluationReceipt{}, err
		}
		trialReceipt := EvaluationTrialReceipt{
			HarborTrialID: trial.ID, HarborTrialName: trial.TrialName,
			ElapsedMillis: trial.Elapsed.Milliseconds(),
		}
		if trial.ExceptionType != "" {
			if _, allowed := infraTypes[trial.ExceptionType]; !allowed {
				return EvaluationReceipt{}, fmt.Errorf("%w: Harbor trial %q has unclassified exception %q", ErrInvalidHarborResult, trial.TrialName, trial.ExceptionType)
			}
			trialReceipt.Status = EvaluationTrialInfraFailed
			trialReceipt.FailureType = trial.ExceptionType
			receipt.Status = EvaluationInfraFailed
			receipt.Trials = append(receipt.Trials, trialReceipt)
			continue
		}
		if !trial.TurnCountKnown {
			return EvaluationReceipt{}, fmt.Errorf("%w: Harbor trial %q does not contain a countable trajectory", ErrInvalidHarborResult, trial.TrialName)
		}
		if !trial.HasVerifierReward {
			return EvaluationReceipt{}, fmt.Errorf("%w: Harbor trial %q has no verifier reward", ErrInvalidHarborResult, trial.TrialName)
		}
		reward, present := trial.Rewards[input.Policy.PassRewardKey]
		if !present {
			return EvaluationReceipt{}, fmt.Errorf("%w: Harbor trial %q omits pass reward key %q", ErrInvalidHarborResult, trial.TrialName, input.Policy.PassRewardKey)
		}
		trialReceipt.Status = EvaluationTrialCompleted
		trialReceipt.TurnCount = trial.TurnCount
		trialReceipt.Passed = reward >= input.Policy.PassRewardAtLeast
		if trialReceipt.Passed {
			receipt.PassCount++
		}
		turnTotal += trialReceipt.TurnCount
		completedTrials++
		receipt.Trials = append(receipt.Trials, trialReceipt)
	}
	if completedTrials > 0 {
		receipt.AverageTurns = float64(turnTotal) / float64(completedTrials)
	}
	if receipt.Status == EvaluationInfraFailed {
		receipt.PolicyCompliant = false
		receipt.ComplianceReasons = []string{"infrastructure failure: rerun the same logical trial set after reconciliation"}
	} else {
		receipt.PolicyCompliant = true
		if input.Policy.MaxPassingTrials != nil && receipt.PassCount > *input.Policy.MaxPassingTrials {
			receipt.PolicyCompliant = false
			receipt.ComplianceReasons = append(receipt.ComplianceReasons, fmt.Sprintf("pass count %d exceeds maximum %d", receipt.PassCount, *input.Policy.MaxPassingTrials))
		}
		if receipt.AverageTurns < float64(input.Policy.MinimumAverageTurns) {
			receipt.PolicyCompliant = false
			receipt.ComplianceReasons = append(receipt.ComplianceReasons, fmt.Sprintf("average turns %.6g is below minimum %d", receipt.AverageTurns, input.Policy.MinimumAverageTurns))
		}
	}
	if err := receipt.Validate(); err != nil {
		return EvaluationReceipt{}, err
	}
	return receipt, nil
}

// HarborEvaluatorIdentity is the result.json identity emitted by Harbor.
type HarborEvaluatorIdentity struct {
	AgentName     string
	AgentVersion  string
	ModelName     string
	ModelProvider string
}

// HarborTrialResult is the evidence-bearing subset of a Harbor 0.18 trial
// result.  It is deliberately not a mirror of Harbor's full schema.
type HarborTrialResult struct {
	ID                string
	TrialName         string
	TaskDigest        string
	Evaluator         HarborEvaluatorIdentity
	Rewards           map[string]float64
	HasVerifierReward bool
	ExceptionType     string
	TurnCount         int
	TurnCountKnown    bool
	Elapsed           time.Duration
}

// HarborJobResult is the strict subset of a completed Harbor 0.18 job result
// needed to build an evaluator receipt.
type HarborJobResult struct {
	ID          string
	TotalTrials int
	Trials      []HarborTrialResult
}

// ParseHarborJobResultV018 parses the completed job-level result.json written
// by Harbor 0.18.0.  Unknown fields are intentionally ignored because they
// are not evidence authority here; duplicate keys, missing evidence fields,
// malformed values, and nonterminal jobs are rejected.
func ParseHarborJobResultV018(raw []byte) (HarborJobResult, error) {
	if err := rejectDuplicateEvaluationJSONKeys(raw); err != nil {
		return HarborJobResult{}, fmt.Errorf("%w: duplicate or malformed JSON: %v", ErrInvalidHarborResult, err)
	}
	root, err := evaluationJSONObject(raw, "job result")
	if err != nil {
		return HarborJobResult{}, err
	}
	id, err := evaluationRequiredString(root, "id", "job result")
	if err != nil {
		return HarborJobResult{}, err
	}
	if _, err := evaluationRequiredTime(root, "finished_at", "job result"); err != nil {
		return HarborJobResult{}, err
	}
	totalTrials, err := evaluationRequiredInt(root, "n_total_trials", "job result")
	if err != nil || totalTrials < 1 {
		if err == nil {
			err = errors.New("must be positive")
		}
		return HarborJobResult{}, fmt.Errorf("%w: job result n_total_trials: %v", ErrInvalidHarborResult, err)
	}
	trialRaw, err := evaluationRequiredArray(root, "trial_results", "job result")
	if err != nil {
		return HarborJobResult{}, err
	}
	trials := make([]HarborTrialResult, 0, len(trialRaw))
	seenIDs := make(map[string]struct{}, len(trialRaw))
	seenNames := make(map[string]struct{}, len(trialRaw))
	for index, rawTrial := range trialRaw {
		trial, err := parseHarborTrialResultV018(rawTrial)
		if err != nil {
			return HarborJobResult{}, fmt.Errorf("%w: trial result %d: %v", ErrInvalidHarborResult, index, err)
		}
		if _, duplicate := seenIDs[trial.ID]; duplicate {
			return HarborJobResult{}, fmt.Errorf("%w: duplicate trial id %q", ErrInvalidHarborResult, trial.ID)
		}
		if _, duplicate := seenNames[trial.TrialName]; duplicate {
			return HarborJobResult{}, fmt.Errorf("%w: duplicate trial name %q", ErrInvalidHarborResult, trial.TrialName)
		}
		seenIDs[trial.ID] = struct{}{}
		seenNames[trial.TrialName] = struct{}{}
		trials = append(trials, trial)
	}
	return HarborJobResult{ID: id, TotalTrials: totalTrials, Trials: trials}, nil
}

func parseHarborTrialResultV018(raw json.RawMessage) (HarborTrialResult, error) {
	object, err := evaluationJSONObject(raw, "trial result")
	if err != nil {
		return HarborTrialResult{}, err
	}
	id, err := evaluationRequiredString(object, "id", "trial result")
	if err != nil {
		return HarborTrialResult{}, err
	}
	name, err := evaluationRequiredString(object, "trial_name", "trial result")
	if err != nil {
		return HarborTrialResult{}, err
	}
	taskDigest, err := evaluationRequiredString(object, "task_checksum", "trial result")
	if err != nil {
		return HarborTrialResult{}, err
	}
	startedAt, err := evaluationRequiredTime(object, "started_at", "trial result")
	if err != nil {
		return HarborTrialResult{}, err
	}
	finishedAt, err := evaluationRequiredTime(object, "finished_at", "trial result")
	if err != nil {
		return HarborTrialResult{}, err
	}
	if finishedAt.Before(startedAt) {
		return HarborTrialResult{}, fmt.Errorf("%w: trial finished before it started", ErrInvalidHarborResult)
	}
	evaluator, err := parseHarborEvaluatorIdentity(object)
	if err != nil {
		return HarborTrialResult{}, err
	}
	result := HarborTrialResult{
		ID: id, TrialName: name, TaskDigest: taskDigest, Evaluator: evaluator,
		Elapsed: finishedAt.Sub(startedAt),
	}
	if exception, present, err := evaluationOptionalObject(object, "exception_info", "trial result"); err != nil {
		return HarborTrialResult{}, err
	} else if present {
		exceptionType, err := evaluationRequiredString(exception, "exception_type", "trial exception")
		if err != nil {
			return HarborTrialResult{}, err
		}
		result.ExceptionType = exceptionType
		return result, nil
	}
	verifier, present, err := evaluationOptionalObject(object, "verifier_result", "trial result")
	if err != nil {
		return HarborTrialResult{}, err
	}
	if present {
		rewards, hasRewards, err := parseHarborRewards(verifier)
		if err != nil {
			return HarborTrialResult{}, err
		}
		result.Rewards, result.HasVerifierReward = rewards, hasRewards
	}
	turns, known, err := parseHarborTrialTurns(object)
	if err != nil {
		return HarborTrialResult{}, err
	}
	result.TurnCount, result.TurnCountKnown = turns, known
	return result, nil
}

func parseHarborEvaluatorIdentity(trial map[string]json.RawMessage) (HarborEvaluatorIdentity, error) {
	agent, err := evaluationRequiredObject(trial, "agent_info", "trial result")
	if err != nil {
		return HarborEvaluatorIdentity{}, err
	}
	name, err := evaluationRequiredString(agent, "name", "agent info")
	if err != nil {
		return HarborEvaluatorIdentity{}, err
	}
	version, err := evaluationRequiredString(agent, "version", "agent info")
	if err != nil {
		return HarborEvaluatorIdentity{}, err
	}
	model, err := evaluationRequiredObject(agent, "model_info", "agent info")
	if err != nil {
		return HarborEvaluatorIdentity{}, err
	}
	modelName, err := evaluationRequiredString(model, "name", "model info")
	if err != nil {
		return HarborEvaluatorIdentity{}, err
	}
	provider, err := evaluationRequiredString(model, "provider", "model info")
	if err != nil {
		return HarborEvaluatorIdentity{}, err
	}
	return HarborEvaluatorIdentity{AgentName: name, AgentVersion: version, ModelName: modelName, ModelProvider: provider}, nil
}

func parseHarborRewards(verifier map[string]json.RawMessage) (map[string]float64, bool, error) {
	raw, present := verifier["rewards"]
	if !present || evaluationJSONNull(raw) {
		return nil, false, nil
	}
	rewardsObject, err := evaluationJSONObject(raw, "verifier rewards")
	if err != nil {
		return nil, false, err
	}
	rewards := make(map[string]float64, len(rewardsObject))
	for key, rawValue := range rewardsObject {
		if err := validateEvaluationText("verifier reward key", key); err != nil {
			return nil, false, err
		}
		decoder := json.NewDecoder(bytes.NewReader(rawValue))
		decoder.UseNumber()
		var number json.Number
		if err := decoder.Decode(&number); err != nil {
			return nil, false, fmt.Errorf("%w: verifier reward %q must be a JSON number", ErrInvalidHarborResult, key)
		}
		value, err := number.Float64()
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, false, fmt.Errorf("%w: verifier reward %q is invalid", ErrInvalidHarborResult, key)
		}
		rewards[key] = value
	}
	return rewards, true, nil
}

func parseHarborTrialTurns(trial map[string]json.RawMessage) (int, bool, error) {
	if agentResult, present, err := evaluationOptionalObject(trial, "agent_result", "trial result"); err != nil {
		return 0, false, err
	} else if present {
		return parseHarborAgentContextTurns(agentResult)
	}
	steps, present, err := evaluationOptionalArray(trial, "step_results", "trial result")
	if err != nil {
		return 0, false, err
	}
	if !present || len(steps) == 0 {
		return 0, false, nil
	}
	total := 0
	for index, rawStep := range steps {
		step, err := evaluationJSONObject(rawStep, "step result")
		if err != nil {
			return 0, false, fmt.Errorf("%w: step result %d: %v", ErrInvalidHarborResult, index, err)
		}
		agentResult, present, err := evaluationOptionalObject(step, "agent_result", "step result")
		if err != nil || !present {
			if err != nil {
				return 0, false, err
			}
			return 0, false, nil
		}
		turns, known, err := parseHarborAgentContextTurns(agentResult)
		if err != nil || !known {
			return 0, false, err
		}
		total += turns
	}
	return total, true, nil
}

func parseHarborAgentContextTurns(context map[string]json.RawMessage) (int, bool, error) {
	rollouts, present, err := evaluationOptionalArray(context, "rollout_details", "agent context")
	if err != nil {
		return 0, false, err
	}
	if !present || len(rollouts) == 0 {
		return 0, false, nil
	}
	total := 0
	for index, rawRollout := range rollouts {
		rollout, err := evaluationJSONObject(rawRollout, "rollout detail")
		if err != nil {
			return 0, false, fmt.Errorf("%w: rollout detail %d: %v", ErrInvalidHarborResult, index, err)
		}
		completions, err := evaluationRequiredArray(rollout, "completion_token_ids", "rollout detail")
		if err != nil {
			return 0, false, err
		}
		total += len(completions)
	}
	return total, true, nil
}

func validateResultEvaluatorIdentity(actual HarborEvaluatorIdentity, expected EvaluatorIdentity) error {
	if actual.AgentName != expected.AgentName || actual.AgentVersion != expected.AgentVersion || actual.ModelName != expected.ModelName || actual.ModelProvider != expected.ModelProvider {
		return fmt.Errorf("%w: Harbor result evaluator %s@%s / %s via %s does not match frozen evaluator %s@%s / %s via %s", ErrInvalidHarborResult, actual.AgentName, actual.AgentVersion, actual.ModelName, actual.ModelProvider, expected.AgentName, expected.AgentVersion, expected.ModelName, expected.ModelProvider)
	}
	return nil
}

func validateScreenshot(raw []byte, mediaType string) error {
	configuration, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || configuration.Width < 1 || configuration.Height < 1 {
		return fmt.Errorf("%w: canonical screenshot is not a decodable image", ErrInvalidEvaluationEvidence)
	}
	actualMediaType := screenshotMediaTypeForFormat(format)
	if actualMediaType == "" || actualMediaType != mediaType {
		return fmt.Errorf("%w: canonical screenshot image format %q does not match declared media type %q", ErrInvalidEvaluationEvidence, format, mediaType)
	}
	return nil
}

func supportedScreenshotMediaType(mediaType string) bool {
	return screenshotMediaTypeForFormat(strings.TrimPrefix(mediaType, "image/")) == mediaType
}

func screenshotMediaTypeForFormat(format string) string {
	switch format {
	case "png":
		return "image/png"
	case "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	default:
		return ""
	}
}

func validateEvaluationText(label, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidEvaluationPolicy, label)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%w: %s contains a control character", ErrInvalidEvaluationPolicy, label)
		}
	}
	return nil
}

func evaluationJSONObject(raw []byte, label string) (map[string]json.RawMessage, error) {
	if evaluationJSONNull(raw) {
		return nil, fmt.Errorf("%w: %s must be an object", ErrInvalidHarborResult, label)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		if err == nil {
			err = errors.New("object is required")
		}
		return nil, fmt.Errorf("%w: %s: %v", ErrInvalidHarborResult, label, err)
	}
	return object, nil
}

func evaluationRequiredObject(object map[string]json.RawMessage, key, label string) (map[string]json.RawMessage, error) {
	raw, present := object[key]
	if !present {
		return nil, fmt.Errorf("%w: %s.%s is required", ErrInvalidHarborResult, label, key)
	}
	return evaluationJSONObject(raw, label+"."+key)
}

func evaluationOptionalObject(object map[string]json.RawMessage, key, label string) (map[string]json.RawMessage, bool, error) {
	raw, present := object[key]
	if !present || evaluationJSONNull(raw) {
		return nil, false, nil
	}
	parsed, err := evaluationJSONObject(raw, label+"."+key)
	return parsed, err == nil, err
}

func evaluationRequiredArray(object map[string]json.RawMessage, key, label string) ([]json.RawMessage, error) {
	raw, present := object[key]
	if !present || evaluationJSONNull(raw) {
		return nil, fmt.Errorf("%w: %s.%s is required", ErrInvalidHarborResult, label, key)
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		if err == nil {
			err = errors.New("array is required")
		}
		return nil, fmt.Errorf("%w: %s.%s: %v", ErrInvalidHarborResult, label, key, err)
	}
	return values, nil
}

func evaluationOptionalArray(object map[string]json.RawMessage, key, label string) ([]json.RawMessage, bool, error) {
	raw, present := object[key]
	if !present || evaluationJSONNull(raw) {
		return nil, false, nil
	}
	values, err := evaluationRequiredArray(object, key, label)
	return values, err == nil, err
}

func evaluationRequiredString(object map[string]json.RawMessage, key, label string) (string, error) {
	raw, present := object[key]
	if !present || evaluationJSONNull(raw) {
		return "", fmt.Errorf("%w: %s.%s is required", ErrInvalidHarborResult, label, key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%w: %s.%s must be a string", ErrInvalidHarborResult, label, key)
	}
	if err := validateEvaluationText(label+"."+key, value); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidHarborResult, err)
	}
	return value, nil
}

func evaluationRequiredInt(object map[string]json.RawMessage, key, label string) (int, error) {
	raw, present := object[key]
	if !present || evaluationJSONNull(raw) {
		return 0, fmt.Errorf("%w: %s.%s is required", ErrInvalidHarborResult, label, key)
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, fmt.Errorf("%w: %s.%s must be an integer", ErrInvalidHarborResult, label, key)
	}
	return value, nil
}

func evaluationRequiredTime(object map[string]json.RawMessage, key, label string) (time.Time, error) {
	value, err := evaluationRequiredString(object, key, label)
	if err != nil {
		return time.Time{}, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s.%s must be RFC3339: %v", ErrInvalidHarborResult, label, key, err)
	}
	return parsed, nil
}

func evaluationJSONNull(raw []byte) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

func rejectDuplicateEvaluationJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := scanEvaluationJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func scanEvaluationJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, valid := keyToken.(string)
			if !valid {
				return errors.New("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate key %q", key)
			}
			seen[key] = struct{}{}
			if err := scanEvaluationJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			if err != nil {
				return err
			}
			return errors.New("unterminated object")
		}
	case '[':
		for decoder.More() {
			if err := scanEvaluationJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			if err != nil {
				return err
			}
			return errors.New("unterminated array")
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delimiter)
	}
	return nil
}
