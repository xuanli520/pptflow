package codeedge

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
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
	if len(receipt.Trials) != 4 || receipt.ScreenshotArtifactID != input.CanonicalScreenshot.ArtifactID {
		t.Fatalf("receipt evidence = %#v, want four trials and frozen screenshot", receipt)
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

func TestBuildEvaluationReceiptMarksClassifiedInfrastructureWithoutCountingItAsModelFailure(t *testing.T) {
	input := validEvaluationInput(t, EvaluationPolicy{})
	result := decodeEvaluationResult(t, input.HarborResult.Bytes)
	trials := result["trial_results"].([]any)
	trials[2].(map[string]any)["exception_info"] = map[string]any{
		"exception_type": "NetworkError",
	}
	delete(trials[2].(map[string]any), "verifier_result")
	delete(trials[2].(map[string]any), "agent_result")
	input.HarborResult = evidenceForBytes("result", "harbor.result.v0.18", "application/json", marshalEvaluationResult(t, result))

	receipt, err := BuildEvaluationReceipt(input)
	if err != nil {
		t.Fatalf("BuildEvaluationReceipt() error = %v", err)
	}
	if receipt.Status != EvaluationInfraFailed || receipt.PassCount != 1 || receipt.PolicyCompliant {
		t.Fatalf("receipt = %#v, want classified infra failure and unchanged pass count", receipt)
	}
	if receipt.Trials[2].Status != EvaluationTrialInfraFailed || receipt.Trials[2].Passed {
		t.Fatalf("trial receipt = %#v, want infra_failed non-pass", receipt.Trials[2])
	}
}

func TestBuildEvaluationReceiptPreservesContentNonCompliance(t *testing.T) {
	maximumPasses := 1
	input := validEvaluationInput(t, EvaluationPolicy{MaxPassingTrials: &maximumPasses})
	result := decodeEvaluationResult(t, input.HarborResult.Bytes)
	trials := result["trial_results"].([]any)
	trials[1].(map[string]any)["verifier_result"].(map[string]any)["rewards"].(map[string]any)["reward"] = 1
	input.HarborResult = evidenceForBytes("result", "harbor.result.v0.18", "application/json", marshalEvaluationResult(t, result))

	receipt, err := BuildEvaluationReceipt(input)
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

func TestBuildEvaluationReceiptRejectsUntrustedResultDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   error
	}{
		{
			name: "wrong evaluator",
			mutate: func(result map[string]any) {
				trials := result["trial_results"].([]any)
				trials[0].(map[string]any)["agent_info"].(map[string]any)["model_info"].(map[string]any)["name"] = "unapproved-model"
			},
			want: ErrInvalidHarborResult,
		},
		{
			name: "task digest drift",
			mutate: func(result map[string]any) {
				trials := result["trial_results"].([]any)
				trials[0].(map[string]any)["task_checksum"] = "other-task"
			},
			want: ErrInvalidHarborResult,
		},
		{
			name: "missing rollout detail",
			mutate: func(result map[string]any) {
				trials := result["trial_results"].([]any)
				delete(trials[0].(map[string]any)["agent_result"].(map[string]any), "rollout_details")
			},
			want: ErrInvalidHarborResult,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validEvaluationInput(t, EvaluationPolicy{})
			result := decodeEvaluationResult(t, input.HarborResult.Bytes)
			test.mutate(result)
			input.HarborResult = evidenceForBytes("result", "harbor.result.v0.18", "application/json", marshalEvaluationResult(t, result))
			_, err := BuildEvaluationReceipt(input)
			if !errors.Is(err, test.want) {
				t.Fatalf("BuildEvaluationReceipt() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestParseHarborJobResultV018RejectsDuplicateKeysAndNonterminalJob(t *testing.T) {
	duplicate := []byte(`{"id":"job","id":"other","finished_at":"2026-07-14T00:00:00Z","n_total_trials":4,"trial_results":[]}`)
	if _, err := ParseHarborJobResultV018(duplicate); !errors.Is(err, ErrInvalidHarborResult) {
		t.Fatalf("duplicate JSON error = %v, want ErrInvalidHarborResult", err)
	}
	nonterminal := []byte(`{"id":"job","n_total_trials":4,"trial_results":[]}`)
	if _, err := ParseHarborJobResultV018(nonterminal); !errors.Is(err, ErrInvalidHarborResult) {
		t.Fatalf("nonterminal JSON error = %v, want ErrInvalidHarborResult", err)
	}
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

func validEvaluationInput(t *testing.T, overrides EvaluationPolicy) EvaluationInput {
	t.Helper()
	policy := validEvaluationPolicy()
	if overrides.MaxPassingTrials != nil {
		policy.MaxPassingTrials = overrides.MaxPassingTrials
	}
	resultBytes := marshalEvaluationResult(t, validHarborResult())
	return EvaluationInput{
		Policy: policy,
		Binding: EvaluationBinding{
			TaskSnapshotDigest:       workflowkit.SubjectDigest("harbor.task.v2:sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
			ExpectedHarborTaskDigest: "harbor-dirhash-task-digest",
			HarborCLI:                HarborCLIIdentity{CommandID: "harbor-cli", Version: "0.18.0", ContentFingerprint: workflowkit.SHA256Fingerprint([]byte("harbor-cli"))},
			CatalogFingerprint:       workflowkit.SHA256Fingerprint([]byte("catalog")),
			LockFingerprint:          workflowkit.SHA256Fingerprint([]byte("lock")),
			ManifestFingerprint:      workflowkit.SHA256Fingerprint([]byte("manifest")),
		},
		HarborResult:        evidenceForBytes("result", "harbor.result.v0.18", "application/json", resultBytes),
		CanonicalScreenshot: evidenceForBytes("screenshot", "harbor.screenshot.v1", "image/png", validPNG(t)),
	}
}

func validEvaluationPolicy() EvaluationPolicy {
	return EvaluationPolicy{
		ID:                 "codeedge.qwen-pass-four",
		Version:            "1",
		HarborResultFormat: HarborJobResultV018,
		Evaluator: EvaluatorIdentity{
			ProfileID: "qwen-profile", ProfileVersion: "1", AgentName: "claude-code", AgentVersion: "1.0.0", ModelName: "qwen-model", ModelProvider: "approved-provider",
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

func validHarborResult() map[string]any {
	trials := make([]any, 0, 4)
	for index := 0; index < 4; index++ {
		reward := 0
		if index == 0 {
			reward = 1
		}
		trials = append(trials, validHarborTrial(index, reward))
	}
	return map[string]any{
		"id": "harbor-job-id", "started_at": "2026-07-14T00:00:00Z", "finished_at": "2026-07-14T00:10:00Z", "n_total_trials": 4,
		"trial_results": trials,
	}
}

func validHarborTrial(index, reward int) map[string]any {
	turns := make([]any, 20)
	for turn := range turns {
		turns[turn] = []any{turn + 1}
	}
	return map[string]any{
		"id": "trial-id-" + string(rune('a'+index)), "trial_name": "task__trial-" + string(rune('a'+index)),
		"task_checksum": "harbor-dirhash-task-digest", "started_at": "2026-07-14T00:00:00Z", "finished_at": "2026-07-14T00:01:00Z",
		"agent_info": map[string]any{
			"name": "claude-code", "version": "1.0.0", "model_info": map[string]any{"name": "qwen-model", "provider": "approved-provider"},
		},
		"agent_result":    map[string]any{"rollout_details": []any{map[string]any{"completion_token_ids": turns}}},
		"verifier_result": map[string]any{"rewards": map[string]any{"reward": reward}},
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

func decodeEvaluationResult(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode evaluation result: %v", err)
	}
	return result
}

func marshalEvaluationResult(t *testing.T, result map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal evaluation result: %v", err)
	}
	return raw
}
