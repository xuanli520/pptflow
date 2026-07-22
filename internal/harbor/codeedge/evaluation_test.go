package codeedge

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestBuildEvaluationReceiptBindsFourTrustedTrialsAndOneScreenshot(t *testing.T) {
	maximumPasses := 1
	input := validEvaluationInput(t, EvaluationPolicy{MaxPassingTrials: &maximumPasses})
	receipt, err := BuildEvaluationReceipt(input)
	if err != nil {
		t.Fatalf("BuildEvaluationReceipt() error = %v", err)
	}
	if receipt.Status != EvaluationCompleted || !receipt.PolicyCompliant {
		t.Fatalf("receipt status/compliance = %q/%t, want completed/true", receipt.Status, receipt.PolicyCompliant)
	}
	if receipt.PassCount != 1 || receipt.AverageTurns != 20 {
		t.Fatalf("receipt aggregates = pass %d, turns %v; want 1, 20", receipt.PassCount, receipt.AverageTurns)
	}
	if receipt.HarborInternalRetryCount != 0 {
		t.Fatalf("receipt Harbor internal retry count = %d, want 0", receipt.HarborInternalRetryCount)
	}
	if len(receipt.Trials) != 4 || receipt.ScreenshotArtifactID != input.CanonicalScreenshot.ArtifactID || receipt.RunBundleArtifactID != input.HarborRunBundle.ArtifactID {
		t.Fatalf("receipt evidence = %#v, want four trials and frozen bundle/screenshot", receipt)
	}
	if receipt.MaterializedTaskRootV2Digest != input.Binding.TaskSnapshotDigest {
		t.Fatalf("receipt materialized task digest = %q, want frozen %q", receipt.MaterializedTaskRootV2Digest, input.Binding.TaskSnapshotDigest)
	}
	if _, err := receipt.CanonicalJSON(); err != nil {
		t.Fatalf("CanonicalJSON() error = %v", err)
	}
	first, err := receipt.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint() first error = %v", err)
	}
	second, err := receipt.Fingerprint()
	if err != nil || first != second {
		t.Fatalf("Fingerprint() = %q, %v then %q, %v; want stable", first, err, second, err)
	}
}

func TestBuildEvaluationReceiptCarriesAggregateHarborRetriesWithoutFabricatingTrialAttempts(t *testing.T) {
	fixture := newEvaluationBundleFixture(t, validEvaluationPolicy())
	fixture.updateJob(t, func(job map[string]any) {
		job["stats"].(map[string]any)["n_retries"] = 3
	})

	receipt, err := BuildEvaluationReceipt(fixture.input(t))
	if err != nil {
		t.Fatalf("BuildEvaluationReceipt() error = %v", err)
	}
	if receipt.HarborInternalRetryCount != 3 {
		t.Fatalf("HarborInternalRetryCount = %d, want 3", receipt.HarborInternalRetryCount)
	}
	if len(receipt.Trials) != 4 {
		t.Fatalf("receipt logical trials = %d, want exactly 4 despite aggregate internal retries", len(receipt.Trials))
	}
	seen := make(map[string]struct{}, len(receipt.Trials))
	for _, trial := range receipt.Trials {
		if _, duplicate := seen[trial.HarborTrialID]; duplicate {
			t.Fatalf("receipt fabricated duplicate logical trial for retry: %#v", receipt.Trials)
		}
		seen[trial.HarborTrialID] = struct{}{}
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("receipt with aggregate retries must remain valid: %v", err)
	}
	fingerprint, err := receipt.Fingerprint()
	if err != nil {
		t.Fatalf("receipt Fingerprint() error = %v", err)
	}
	changed := receipt.Clone()
	changed.HarborInternalRetryCount = 2
	changedFingerprint, err := changed.Fingerprint()
	if err != nil {
		t.Fatalf("changed receipt Fingerprint() error = %v", err)
	}
	if changedFingerprint == fingerprint {
		t.Fatal("aggregate Harbor retry count is absent from immutable receipt fingerprint")
	}
}

func TestEvaluationReceiptRejectsNegativeHarborInternalRetryCount(t *testing.T) {
	receipt, err := BuildEvaluationReceipt(validEvaluationInput(t, EvaluationPolicy{}))
	if err != nil {
		t.Fatalf("BuildEvaluationReceipt() error = %v", err)
	}
	receipt.HarborInternalRetryCount = -1
	if err := receipt.Validate(); !errors.Is(err, ErrInvalidEvaluationEvidence) {
		t.Fatalf("negative Harbor internal retry count error = %v, want ErrInvalidEvaluationEvidence", err)
	}
}

func TestBuildEvaluationReceiptMarksClassifiedInfrastructureWithoutCountingItAsModelFailure(t *testing.T) {
	fixture := newEvaluationBundleFixture(t, validEvaluationPolicy())
	fixture.updateTrial(t, 2, func(trial map[string]any) {
		trial["exception_info"] = map[string]any{"exception_type": "NetworkError"}
		delete(trial, "verifier_result")
	})
	receipt, err := BuildEvaluationReceipt(fixture.input(t))
	if err != nil {
		t.Fatalf("BuildEvaluationReceipt() error = %v", err)
	}
	if receipt.Status != EvaluationInfraFailed || receipt.PassCount != 1 || receipt.PolicyCompliant {
		t.Fatalf("receipt = %#v, want classified infra failure and unchanged pass count", receipt)
	}
	infra := 0
	for _, trial := range receipt.Trials {
		if trial.Status == EvaluationTrialInfraFailed {
			infra++
			if trial.Passed || trial.FailureType != "NetworkError" {
				t.Fatalf("infra trial receipt = %#v", trial)
			}
		}
	}
	if infra != 1 {
		t.Fatalf("receipt trials = %#v, want one infra failure", receipt.Trials)
	}
}

func TestBuildEvaluationReceiptClassifiesHarbor018NaiveTimestampNetworkConnectionFailures(t *testing.T) {
	policy := validEvaluationPolicy()
	policy.InfraExceptionTypes = append(policy.InfraExceptionTypes, "NetworkConnectionError")
	fixture := newEvaluationBundleFixture(t, policy)
	fixture.updateJob(t, func(job map[string]any) {
		// Harbor 0.18 serializes completed job timestamps with Python's
		// timezone-naive ISO-8601 representation.
		job["finished_at"] = "2026-07-14T00:10:00"
	})
	for index := range fixture.harbor.trialDirectories {
		fixture.updateTrial(t, index, func(trial map[string]any) {
			trial["exception_info"] = map[string]any{"exception_type": "NetworkConnectionError"}
			delete(trial, "verifier_result")
		})
	}

	receipt, err := BuildEvaluationReceipt(fixture.input(t))
	if err != nil {
		t.Fatalf("BuildEvaluationReceipt() error = %v", err)
	}
	if receipt.Status != EvaluationInfraFailed || receipt.PassCount != 0 || receipt.PolicyCompliant {
		t.Fatalf("receipt = %#v, want four classified Harbor network infrastructure failures", receipt)
	}
	if len(receipt.Trials) != 4 {
		t.Fatalf("receipt trials = %#v, want four logical trials", receipt.Trials)
	}
	for _, trial := range receipt.Trials {
		if trial.Status != EvaluationTrialInfraFailed || trial.Passed || trial.FailureType != "NetworkConnectionError" {
			t.Fatalf("infra trial receipt = %#v, want NetworkConnectionError", trial)
		}
	}
}

func TestBuildEvaluationReceiptPreservesContentNonCompliance(t *testing.T) {
	maximumPasses := 1
	policy := validEvaluationPolicy()
	policy.MaxPassingTrials = &maximumPasses
	fixture := newEvaluationBundleFixture(t, policy)
	fixture.setTrialReward(t, 1, 1)

	receipt, err := BuildEvaluationReceipt(fixture.input(t))
	if err != nil {
		t.Fatalf("BuildEvaluationReceipt() error = %v", err)
	}
	if receipt.Status != EvaluationCompleted || receipt.PolicyCompliant || receipt.PassCount != 2 {
		t.Fatalf("receipt = %#v, want completed noncompliant pass_count=2", receipt)
	}
	if got := strings.Join(receipt.ComplianceReasons, " "); !strings.Contains(got, "exceeds maximum") {
		t.Fatalf("reasons = %q, want pass-limit explanation", got)
	}
}

func TestBuildEvaluationReceiptDoesNotEquateHarborTaskDigestsWithFlowV2Digest(t *testing.T) {
	fixture := newEvaluationBundleFixture(t, validEvaluationPolicy())
	fixture.updateTrial(t, 0, func(trial map[string]any) {
		trial["task_checksum"] = "different-harbor-dirhash-value"
	})
	if _, err := BuildEvaluationReceipt(fixture.input(t)); err != nil {
		t.Fatalf("different Harbor task_checksum must remain separately evidenced, got %v", err)
	}
}

func TestBuildEvaluationReceiptRejectsInvalidBundleEvidence(t *testing.T) {
	t.Run("wrong evaluator", func(t *testing.T) {
		fixture := newEvaluationBundleFixture(t, validEvaluationPolicy())
		fixture.updateTrial(t, 0, func(trial map[string]any) {
			trial["agent_info"].(map[string]any)["model_info"].(map[string]any)["name"] = "unapproved-model"
		})
		if _, err := BuildEvaluationReceipt(fixture.input(t)); !errors.Is(err, ErrInvalidHarborResult) {
			t.Fatalf("wrong evaluator error = %v, want ErrInvalidHarborResult", err)
		}
	})
	t.Run("missing trajectory", func(t *testing.T) {
		fixture := newEvaluationBundleFixture(t, validEvaluationPolicy())
		if err := os.Remove(filepath.Join(fixture.harbor.jobRoot, fixture.harbor.trialDirectories[0], "agent", "trajectory.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := BuildEvaluationReceipt(fixture.input(t)); !errors.Is(err, ErrInvalidHarborResult) {
			t.Fatalf("missing trajectory error = %v, want ErrInvalidHarborResult", err)
		}
	})
	t.Run("frozen V2 binding drift", func(t *testing.T) {
		fixture := newEvaluationBundleFixture(t, validEvaluationPolicy())
		input := fixture.input(t)
		input.Binding.TaskSnapshotDigest = workflowkit.SubjectDigest("harbor.task.v2:sha256:" + strings.Repeat("b", 64))
		if _, err := BuildEvaluationReceipt(input); !errors.Is(err, ErrInvalidHarborResult) {
			t.Fatalf("binding drift error = %v, want ErrInvalidHarborResult", err)
		}
	})
	t.Run("missing completed trial", func(t *testing.T) {
		fixture := newEvaluationBundleFixture(t, validEvaluationPolicy())
		if err := os.Remove(filepath.Join(fixture.harbor.jobRoot, fixture.harbor.trialDirectories[0], "result.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := CaptureHarborRunBundleV018(fixture.harbor.request()); !errors.Is(err, ErrInvalidHarborRunBundle) {
			t.Fatalf("torn bundle capture error = %v, want ErrInvalidHarborRunBundle", err)
		}
	})
	t.Run("nonterminal job", func(t *testing.T) {
		fixture := newEvaluationBundleFixture(t, validEvaluationPolicy())
		fixture.updateJob(t, func(job map[string]any) { delete(job, "finished_at") })
		if _, err := CaptureHarborRunBundleV018(fixture.harbor.request()); !errors.Is(err, ErrInvalidHarborRunBundle) {
			t.Fatalf("nonterminal bundle capture error = %v, want ErrInvalidHarborRunBundle", err)
		}
	})
}

func TestEvaluationPolicyRequiresExplicitPhaseOneRules(t *testing.T) {
	policy := validEvaluationPolicy()
	policy.LogicalTrialCount = 3
	if err := policy.Validate(); !errors.Is(err, ErrInvalidEvaluationPolicy) {
		t.Fatalf("trial count policy error = %v, want ErrInvalidEvaluationPolicy", err)
	}
	policy = validEvaluationPolicy()
	policy.MinimumAverageTurns = 19
	if err := policy.Validate(); !errors.Is(err, ErrInvalidEvaluationPolicy) {
		t.Fatalf("turn policy error = %v, want ErrInvalidEvaluationPolicy", err)
	}
}

type evaluationBundleFixture struct {
	harbor harborRunBundleFixture
	policy EvaluationPolicy
}

func newEvaluationBundleFixture(t *testing.T, policy EvaluationPolicy) evaluationBundleFixture {
	t.Helper()
	harbor := newHarborRunBundleFixture(t)
	fixture := evaluationBundleFixture{harbor: harbor, policy: policy}
	fixture.write(t)
	return fixture
}

func validEvaluationInput(t *testing.T, overrides EvaluationPolicy) EvaluationInput {
	t.Helper()
	policy := validEvaluationPolicy()
	if overrides.MaxPassingTrials != nil {
		policy.MaxPassingTrials = overrides.MaxPassingTrials
	}
	return newEvaluationBundleFixture(t, policy).input(t)
}

func validEvaluationInputForPolicy(t *testing.T, policy EvaluationPolicy) EvaluationInput {
	t.Helper()
	return newEvaluationBundleFixture(t, policy).input(t)
}

func (fixture evaluationBundleFixture) write(t *testing.T) {
	t.Helper()
	writeHarborRunBundleJSON(t, filepath.Join(fixture.harbor.jobRoot, "config.json"), map[string]any{
		"n_attempts":   4,
		"n_concurrent": 4,
		"tasks":        []any{map[string]any{"path": fixture.harbor.taskRoot}},
		"datasets":     []any{},
		"agents":       []any{map[string]any{"name": fixture.policy.Evaluator.AgentName, "model_name": fixture.policy.Evaluator.ModelName}},
	})
	writeHarborRunBundleJSON(t, filepath.Join(fixture.harbor.jobRoot, "lock.json"), map[string]any{
		"schema_version": 2,
		"harbor":         map[string]any{"version": "0.18.0"},
		"trials":         []any{},
	})
	fixture.updateJob(t, func(job map[string]any) {
		job["id"] = "evaluation-job"
		job["started_at"] = "2026-07-14T00:00:00Z"
		job["finished_at"] = "2026-07-14T00:10:00Z"
		job["n_total_trials"] = 4
		job["stats"] = map[string]any{
			"n_running_trials": 0,
			"n_pending_trials": 0,
			"n_retries":        0,
			"evals": map[string]any{
				fixture.policy.Evaluator.AgentName + "__" + fixture.policy.Evaluator.ModelName + "__adhoc": map[string]any{
					"pass_at_k": map[string]any{"4": 1},
				},
			},
		}
	})
	lockDigest := "sha256:" + strings.Repeat("a", 64)
	for index, directory := range fixture.harbor.trialDirectories {
		root := filepath.Join(fixture.harbor.jobRoot, directory)
		writeHarborRunBundleJSON(t, filepath.Join(root, "config.json"), map[string]any{"job_id": "evaluation-job", "trial_name": directory})
		writeHarborRunBundleJSON(t, filepath.Join(root, "lock.json"), map[string]any{"task": map[string]any{"digest": lockDigest}})
		reward := 0
		if index == 0 {
			reward = 1
		}
		model := map[string]any{"name": fixture.policy.Evaluator.ModelName}
		if fixture.policy.Evaluator.ModelProvider != "" {
			model["provider"] = fixture.policy.Evaluator.ModelProvider
		}
		writeHarborRunBundleJSON(t, filepath.Join(root, "result.json"), map[string]any{
			"id":             "evaluation-trial-" + string(rune('a'+index)),
			"trial_name":     directory,
			"task_checksum":  "dirhash-task-checksum-" + string(rune('a'+index)),
			"config":         map[string]any{"job_id": "evaluation-job"},
			"started_at":     "2026-07-14T00:00:00Z",
			"finished_at":    "2026-07-14T00:01:00Z",
			"exception_info": nil,
			"agent_info": map[string]any{
				"name": fixture.policy.Evaluator.AgentName, "version": fixture.policy.Evaluator.AgentVersion, "model_info": model,
			},
			"verifier_result": map[string]any{"rewards": map[string]any{fixture.policy.PassRewardKey: reward}},
		})
		writeHarborRunBundleJSON(t, filepath.Join(root, "agent", "trajectory.json"), map[string]any{"final_metrics": map[string]any{"total_steps": 20}})
	}
}

func (fixture evaluationBundleFixture) input(t *testing.T) EvaluationInput {
	t.Helper()
	bundle, err := CaptureHarborRunBundleV018(fixture.harbor.request())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := bundle.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return EvaluationInput{
		Policy: fixture.policy,
		Binding: EvaluationBinding{
			TaskSnapshotDigest:  fixture.harbor.digest,
			CatalogFingerprint:  workflowkit.SHA256Fingerprint([]byte("catalog")),
			LockFingerprint:     workflowkit.SHA256Fingerprint([]byte("lock")),
			ManifestFingerprint: workflowkit.SHA256Fingerprint([]byte("manifest")),
		},
		HarborRunBundle:     evidenceForBytes("run-bundle", HarborRunBundleV018Format, "application/json", raw),
		CanonicalScreenshot: evidenceForBytes("screenshot", "harbor.screenshot.v1", "image/png", validPNG(t)),
	}
}

func (fixture evaluationBundleFixture) updateJob(t *testing.T, mutate func(map[string]any)) {
	t.Helper()
	path := filepath.Join(fixture.harbor.jobRoot, "result.json")
	value := readEvaluationJSON(t, path)
	mutate(value)
	writeHarborRunBundleJSON(t, path, value)
}

func (fixture evaluationBundleFixture) updateTrial(t *testing.T, index int, mutate func(map[string]any)) {
	t.Helper()
	path := filepath.Join(fixture.harbor.jobRoot, fixture.harbor.trialDirectories[index], "result.json")
	value := readEvaluationJSON(t, path)
	mutate(value)
	writeHarborRunBundleJSON(t, path, value)
}

func (fixture evaluationBundleFixture) setTrialReward(t *testing.T, index, reward int) {
	t.Helper()
	fixture.updateTrial(t, index, func(trial map[string]any) {
		trial["verifier_result"].(map[string]any)["rewards"].(map[string]any)[fixture.policy.PassRewardKey] = reward
	})
}

func readEvaluationJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func validEvaluationPolicy() EvaluationPolicy {
	return EvaluationPolicy{
		ID:                   "codeedge.qwen-pass-four",
		Version:              "1",
		HarborEvidenceFormat: HarborRunBundleV018Format,
		Evaluator: EvaluatorIdentity{
			ProfileID: "qwen-profile", ProfileVersion: "1", AgentName: "claude-code", AgentVersion: "1.0.0", ModelName: "qwen",
		},
		LogicalTrialCount:   4,
		PassRewardKey:       "reward",
		PassRewardAtLeast:   1,
		MinimumAverageTurns: 20,
		ScreenshotMediaType: "image/png",
		FailureClassifierID: "codeedge-infra", FailureClassifierVersion: "1",
		InfraExceptionTypes: []string{"DockerBuildError", "NetworkError"},
	}
}

func evidenceForBytes(id, schema, mediaType string, raw []byte) EvaluationEvidence {
	return EvaluationEvidence{ArtifactID: workflowkit.ArtifactID(id), SchemaVersion: schema, MediaType: mediaType, ContentDigest: workflowkit.SHA256Fingerprint(raw), Bytes: raw}
}

func validPNG(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 2, 2))
	picture.Set(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	if err := png.Encode(&buffer, picture); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}
	return buffer.Bytes()
}
