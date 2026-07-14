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
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
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
	// ModelProvider is optional because Harbor 0.18.0 legitimately omits it
	// for some agents. An empty value means the frozen policy does not claim a
	// provider fact, rather than inventing one from the model name.
	ModelProvider string `json:"model_provider,omitempty"`
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
	HarborEvidenceFormat     string            `json:"harbor_evidence_format"`
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
		{"Harbor evidence format", policy.HarborEvidenceFormat},
		{"evaluator profile id", policy.Evaluator.ProfileID},
		{"evaluator profile version", policy.Evaluator.ProfileVersion},
		{"agent name", policy.Evaluator.AgentName},
		{"agent version", policy.Evaluator.AgentVersion},
		{"model name", policy.Evaluator.ModelName},
		{"pass reward key", policy.PassRewardKey},
		{"screenshot media type", policy.ScreenshotMediaType},
		{"failure classifier id", policy.FailureClassifierID},
		{"failure classifier version", policy.FailureClassifierVersion},
	} {
		if err := validateEvaluationText(field.name, field.value); err != nil {
			return err
		}
	}
	if policy.HarborEvidenceFormat != HarborRunBundleV018Format {
		return fmt.Errorf("%w: unsupported Harbor evidence format %q", ErrInvalidEvaluationPolicy, policy.HarborEvidenceFormat)
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
	if policy.Evaluator.ModelProvider != "" {
		if err := validateEvaluationText("model provider", policy.Evaluator.ModelProvider); err != nil {
			return err
		}
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
// a captured Harbor run bundle can become reusable evidence. PackageDigest is deliberately
// absent: the confirmed Phase-1 flow evaluates a managed task snapshot and
// creates the single local package only after final compliance succeeds.
type EvaluationBinding struct {
	TaskSnapshotDigest  workflowkit.SubjectDigest `json:"task_snapshot_digest"`
	CatalogFingerprint  workflowkit.Fingerprint   `json:"catalog_fingerprint"`
	LockFingerprint     workflowkit.Fingerprint   `json:"lock_fingerprint"`
	ManifestFingerprint workflowkit.Fingerprint   `json:"manifest_fingerprint"`
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
// deployment contract. The separately observed Harbor trial task_checksum and
// lock task.digest stay inside the immutable run bundle; they are never
// accepted as caller-supplied substitutes for this V2 task snapshot digest.
func (binding EvaluationBinding) Validate() error {
	if err := binding.TaskSnapshotDigest.Validate(); err != nil {
		return fmt.Errorf("%w: task snapshot digest: %v", ErrInvalidEvaluationEvidence, err)
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

// EvaluationInput combines immutable policy, frozen Run binding, one
// self-contained Harbor 0.18 run bundle, and the one canonical screenshot. A
// slice is intentionally not used for the screenshot: a receipt structurally
// cannot carry two screenshots for one evaluator, which enforces the Phase-1
// "one image per model" rule.
type EvaluationInput struct {
	Policy              EvaluationPolicy   `json:"policy"`
	Binding             EvaluationBinding  `json:"binding"`
	HarborRunBundle     EvaluationEvidence `json:"harbor_run_bundle"`
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
	Format                       string                    `json:"format"`
	Version                      string                    `json:"version"`
	Status                       EvaluationStatus          `json:"status"`
	PolicyID                     string                    `json:"policy_id"`
	PolicyVersion                string                    `json:"policy_version"`
	PolicyFingerprint            workflowkit.Fingerprint   `json:"policy_fingerprint"`
	Evaluator                    EvaluatorIdentity         `json:"evaluator"`
	HarborEvidenceFormat         string                    `json:"harbor_evidence_format"`
	HarborCLI                    HarborCLIIdentity         `json:"harbor_cli"`
	HarborJobID                  string                    `json:"harbor_job_id"`
	MaterializedTaskRootV2Digest workflowkit.SubjectDigest `json:"materialized_task_root_v2_digest"`
	TaskSnapshotDigest           workflowkit.SubjectDigest `json:"task_snapshot_digest"`
	CatalogFingerprint           workflowkit.Fingerprint   `json:"catalog_fingerprint"`
	LockFingerprint              workflowkit.Fingerprint   `json:"lock_fingerprint"`
	ManifestFingerprint          workflowkit.Fingerprint   `json:"manifest_fingerprint"`
	RunBundleArtifactID          workflowkit.ArtifactID    `json:"run_bundle_artifact_id"`
	RunBundleContentDigest       workflowkit.Fingerprint   `json:"run_bundle_content_digest"`
	ScreenshotArtifactID         workflowkit.ArtifactID    `json:"screenshot_artifact_id"`
	ScreenshotContentDigest      workflowkit.Fingerprint   `json:"screenshot_content_digest"`
	ScreenshotMediaType          string                    `json:"screenshot_media_type"`
	Trials                       []EvaluationTrialReceipt  `json:"trials"`
	PassCount                    int                       `json:"pass_count"`
	AverageTurns                 float64                   `json:"average_turns"`
	PolicyCompliant              bool                      `json:"policy_compliant"`
	ComplianceReasons            []string                  `json:"compliance_reasons"`
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
		{"Harbor evidence format", receipt.HarborEvidenceFormat}, {"Harbor job id", receipt.HarborJobID},
		{"run bundle artifact id", string(receipt.RunBundleArtifactID)},
		{"screenshot artifact id", string(receipt.ScreenshotArtifactID)}, {"screenshot media type", receipt.ScreenshotMediaType},
	} {
		if err := validateEvaluationText(field.name, field.value); err != nil {
			return err
		}
	}
	if receipt.HarborEvidenceFormat != HarborRunBundleV018Format || !supportedScreenshotMediaType(receipt.ScreenshotMediaType) {
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
		{"evaluator model name", receipt.Evaluator.ModelName},
	} {
		if err := validateEvaluationText(field.name, field.value); err != nil {
			return err
		}
	}
	if receipt.Evaluator.ModelProvider != "" {
		if err := validateEvaluationText("evaluator model provider", receipt.Evaluator.ModelProvider); err != nil {
			return err
		}
	}
	if err := receipt.PolicyFingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: policy fingerprint: %v", ErrInvalidEvaluationEvidence, err)
	}
	if err := receipt.TaskSnapshotDigest.Validate(); err != nil {
		return fmt.Errorf("%w: task snapshot digest: %v", ErrInvalidEvaluationEvidence, err)
	}
	if err := receipt.MaterializedTaskRootV2Digest.Validate(); err != nil {
		return fmt.Errorf("%w: materialized task root V2 digest: %v", ErrInvalidEvaluationEvidence, err)
	}
	if receipt.MaterializedTaskRootV2Digest != receipt.TaskSnapshotDigest {
		return fmt.Errorf("%w: materialized task root V2 digest does not match frozen task snapshot", ErrInvalidEvaluationEvidence)
	}
	for _, field := range []struct {
		name  string
		value workflowkit.Fingerprint
	}{
		{"catalog fingerprint", receipt.CatalogFingerprint}, {"lock fingerprint", receipt.LockFingerprint},
		{"manifest fingerprint", receipt.ManifestFingerprint}, {"run bundle content digest", receipt.RunBundleContentDigest},
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

// BuildEvaluationReceipt parses one trusted, self-contained Harbor 0.18 run
// bundle and emits one durable evaluator receipt. It never accepts a
// job-level trial_results projection: each of the four Trial documents is
// independently checked by the bundle inspector. A classified infrastructure
// failure remains evidence instead of becoming a model failure; malformed or
// drifting bundle data remains an error because it cannot be trusted evidence.
func BuildEvaluationReceipt(input EvaluationInput) (EvaluationReceipt, error) {
	if err := input.Policy.Validate(); err != nil {
		return EvaluationReceipt{}, err
	}
	if err := input.Binding.Validate(); err != nil {
		return EvaluationReceipt{}, err
	}
	if err := input.HarborRunBundle.validate("Harbor run bundle"); err != nil {
		return EvaluationReceipt{}, err
	}
	if err := input.CanonicalScreenshot.validate("canonical screenshot"); err != nil {
		return EvaluationReceipt{}, err
	}
	if input.HarborRunBundle.SchemaVersion != HarborRunBundleV018Format || input.HarborRunBundle.MediaType != "application/json" {
		return EvaluationReceipt{}, fmt.Errorf("%w: Harbor run bundle must use %s/application-json", ErrInvalidEvaluationEvidence, HarborRunBundleV018Format)
	}
	if input.CanonicalScreenshot.MediaType != input.Policy.ScreenshotMediaType {
		return EvaluationReceipt{}, fmt.Errorf("%w: screenshot media type %q does not match frozen policy %q", ErrInvalidEvaluationEvidence, input.CanonicalScreenshot.MediaType, input.Policy.ScreenshotMediaType)
	}
	if err := validateScreenshot(input.CanonicalScreenshot.Bytes, input.CanonicalScreenshot.MediaType); err != nil {
		return EvaluationReceipt{}, err
	}
	inspection, err := ParseAndInspectHarborRunBundleV018(input.HarborRunBundle.Bytes)
	if err != nil {
		return EvaluationReceipt{}, err
	}
	bundle := inspection.Bundle()
	if bundle.SourceTaskSnapshotDigest != input.Binding.TaskSnapshotDigest || bundle.MaterializedTaskRootV2Digest != input.Binding.TaskSnapshotDigest {
		return EvaluationReceipt{}, fmt.Errorf("%w: Harbor run bundle task binding does not match frozen Run", ErrInvalidHarborResult)
	}
	job := inspection.Job()
	trials := inspection.Trials()
	if job.TotalTrials != input.Policy.LogicalTrialCount || len(trials) != input.Policy.LogicalTrialCount {
		return EvaluationReceipt{}, fmt.Errorf("%w: Harbor run bundle must contain exactly %d logical trials", ErrInvalidHarborResult, input.Policy.LogicalTrialCount)
	}
	policyFingerprint, err := input.Policy.Fingerprint()
	if err != nil {
		return EvaluationReceipt{}, err
	}

	receipt := EvaluationReceipt{
		Format:                       EvaluationReceiptFormat,
		Version:                      EvaluationReceiptVersion,
		Status:                       EvaluationCompleted,
		PolicyID:                     input.Policy.ID,
		PolicyVersion:                input.Policy.Version,
		PolicyFingerprint:            policyFingerprint,
		Evaluator:                    input.Policy.Evaluator,
		HarborEvidenceFormat:         input.Policy.HarborEvidenceFormat,
		HarborCLI:                    bundle.HarborCLI,
		HarborJobID:                  job.ID,
		MaterializedTaskRootV2Digest: bundle.MaterializedTaskRootV2Digest,
		TaskSnapshotDigest:           input.Binding.TaskSnapshotDigest,
		CatalogFingerprint:           input.Binding.CatalogFingerprint,
		LockFingerprint:              input.Binding.LockFingerprint,
		ManifestFingerprint:          input.Binding.ManifestFingerprint,
		RunBundleArtifactID:          input.HarborRunBundle.ArtifactID,
		RunBundleContentDigest:       input.HarborRunBundle.ContentDigest,
		ScreenshotArtifactID:         input.CanonicalScreenshot.ArtifactID,
		ScreenshotContentDigest:      input.CanonicalScreenshot.ContentDigest,
		ScreenshotMediaType:          input.CanonicalScreenshot.MediaType,
		Trials:                       make([]EvaluationTrialReceipt, 0, len(trials)),
	}
	infraTypes := make(map[string]struct{}, len(input.Policy.InfraExceptionTypes))
	for _, exceptionType := range input.Policy.InfraExceptionTypes {
		infraTypes[exceptionType] = struct{}{}
	}
	turnTotal := 0
	completedTrials := 0
	rawRewards := make([]float64, 0, len(trials))
	for _, trial := range trials {
		if err := validateBundleEvaluatorIdentity(trial.Evaluator, input.Policy.Evaluator); err != nil {
			return EvaluationReceipt{}, err
		}
		trialReceipt := EvaluationTrialReceipt{
			HarborTrialID: trial.ID, HarborTrialName: trial.Name,
			ElapsedMillis: trial.Elapsed.Milliseconds(),
		}
		if trial.ExceptionType != "" {
			if _, allowed := infraTypes[trial.ExceptionType]; !allowed {
				return EvaluationReceipt{}, fmt.Errorf("%w: Harbor trial %q has unclassified exception %q", ErrInvalidHarborResult, trial.Name, trial.ExceptionType)
			}
			trialReceipt.Status = EvaluationTrialInfraFailed
			trialReceipt.FailureType = trial.ExceptionType
			receipt.Status = EvaluationInfraFailed
			receipt.Trials = append(receipt.Trials, trialReceipt)
			continue
		}
		if trial.TrajectoryTotalSteps == nil {
			return EvaluationReceipt{}, fmt.Errorf("%w: Harbor trial %q does not contain a countable trajectory", ErrInvalidHarborResult, trial.Name)
		}
		reward, present := trial.VerifierRewards[input.Policy.PassRewardKey]
		if !present {
			return EvaluationReceipt{}, fmt.Errorf("%w: Harbor trial %q omits pass reward key %q", ErrInvalidHarborResult, trial.Name, input.Policy.PassRewardKey)
		}
		trialReceipt.Status = EvaluationTrialCompleted
		trialReceipt.TurnCount = *trial.TrajectoryTotalSteps
		trialReceipt.Passed = reward >= input.Policy.PassRewardAtLeast
		if trialReceipt.Passed {
			receipt.PassCount++
		}
		rawRewards = append(rawRewards, reward)
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
		if err := validateHarborPassAtFour(job.PassAtK, rawRewards); err != nil {
			return EvaluationReceipt{}, err
		}
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

func validateBundleEvaluatorIdentity(actual HarborRunBundleEvaluatorFactsV018, expected EvaluatorIdentity) error {
	if actual.AgentName != expected.AgentName || actual.AgentVersion != expected.AgentVersion || actual.ModelName == nil || *actual.ModelName != expected.ModelName {
		return fmt.Errorf("%w: Harbor bundle evaluator identity does not match frozen agent/model", ErrInvalidHarborResult)
	}
	if expected.ModelProvider != "" && (actual.ModelProvider == nil || *actual.ModelProvider != expected.ModelProvider) {
		return fmt.Errorf("%w: Harbor bundle model provider does not match frozen provider", ErrInvalidHarborResult)
	}
	return nil
}

func validateHarborPassAtFour(groups map[string]map[string]float64, rewards []float64) error {
	if len(rewards) != 4 || len(groups) != 1 {
		return fmt.Errorf("%w: Harbor pass@4 corroboration does not contain one four-trial evaluator group", ErrInvalidHarborResult)
	}
	var values map[string]float64
	for _, value := range groups {
		values = value
	}
	observed, found := values["4"]
	if !found || math.IsNaN(observed) || math.IsInf(observed, 0) || observed < 0 || observed > 1 {
		return fmt.Errorf("%w: Harbor pass@4 corroboration is absent or invalid", ErrInvalidHarborResult)
	}
	expected := 0.0
	for _, reward := range rewards {
		if reward != 0 && reward != 1 {
			return fmt.Errorf("%w: Harbor pass@4 requires binary raw rewards", ErrInvalidHarborResult)
		}
		if reward == 1 {
			expected = 1
		}
	}
	if math.Abs(observed-expected) > 1e-9 {
		return fmt.Errorf("%w: Harbor pass@4 does not corroborate the four raw trial rewards", ErrInvalidHarborResult)
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
