package evidence

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

func TestRenderPassAt4PNGProducesDistinctReadableEvidence(t *testing.T) {
	base := domain.TrialResult{
		Model: "qwen3.7-max", Agent: "claude-code", Trials: 4, PassCount: 1,
		PassAt4: 0.25, AverageTurns: 23.5, TaskDigest: "0123456789abcdef0123456789abcdef",
		CreatedAt: time.Date(2026, 7, 13, 4, 5, 6, 0, time.UTC),
		Runs: []domain.TrialRun{
			{Trial: 1, Passed: true, Turns: 20}, {Trial: 2, Turns: 23},
			{Trial: 3, Turns: 25}, {Trial: 4, Turns: 26},
		},
	}
	qwen, err := RenderPassAt4PNG("harbor_run_qwen", base)
	if err != nil {
		t.Fatal(err)
	}
	opusResult := base
	opusResult.Model = "claude-opus-4-6"
	opusResult.PassCount = 3
	opusResult.PassAt4 = 0.75
	opus, err := RenderPassAt4PNG("harbor_run_opus", opusResult)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(qwen, opus) {
		t.Fatal("Qwen and Opus evidence images must differ")
	}
	decoded, err := png.Decode(bytes.NewReader(qwen))
	if err != nil {
		t.Fatalf("generated evidence is not a valid PNG: %v", err)
	}
	if decoded.Bounds().Dx() != canvasWidth || decoded.Bounds().Dy() != canvasHeight {
		t.Fatalf("unexpected evidence dimensions: %v", decoded.Bounds())
	}
}

func TestSummaryCoherentRejectsMisleadingResults(t *testing.T) {
	result := domain.TrialResult{
		Trials: 4, PassCount: 1, PassAt4: 0.25, AverageTurns: 21.5,
		Runs: []domain.TrialRun{
			{Trial: 1, Passed: true, Turns: 20}, {Trial: 2, Turns: 21},
			{Trial: 3, Turns: 22}, {Trial: 4, Turns: 23},
		},
	}
	if !summaryCoherent(result) {
		t.Fatal("coherent four-trial result should pass the renderer fallback audit")
	}
	result.Runs[3].Trial = 3
	if summaryCoherent(result) {
		t.Fatal("duplicate trial numbers must not be presented as verified")
	}
	result.Runs[3].Trial = 4
	result.PassCount = 2
	if summaryCoherent(result) {
		t.Fatal("inconsistent pass_count must not be presented as verified")
	}
}

func TestValidateImageFileAndContentIdentity(t *testing.T) {
	result := domain.TrialResult{
		Model: "qwen3.7-max", Trials: 4, PassCount: 1, PassAt4: 0.25, AverageTurns: 21.5,
		Runs: []domain.TrialRun{{Trial: 1, Passed: true, Turns: 20}, {Trial: 2, Turns: 21}, {Trial: 3, Turns: 22}, {Trial: 4, Turns: 23}},
	}
	raw, err := RenderPassAt4PNG("harbor_run_qwen", result)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	first := filepath.Join(dir, "first.png")
	copyPath := filepath.Join(dir, "copy.png")
	invalid := filepath.Join(dir, "invalid.png")
	for _, path := range []string{first, copyPath} {
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(invalid, []byte("not a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateImageFile(first); err != nil {
		t.Fatalf("valid generated PNG rejected: %v", err)
	}
	if err := ValidateImageFile(invalid); err == nil {
		t.Fatal("invalid PNG payload accepted")
	}
	same, err := SameFileContent(first, copyPath)
	if err != nil || !same {
		t.Fatalf("identical evidence content not detected: same=%v err=%v", same, err)
	}
}
