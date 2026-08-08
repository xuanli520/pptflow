package app

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// TestTaskBoardReviewDiagnosticsFromReceiptKeepsOnlyActionableChecks is the
// regression guard for the defect where a review gate showed an agent finding as
// raw envelope JSON and nothing else.
//
// A WorkflowFinding carries no prose by design: it is a closed code plus
// digests. The only human-readable account of why a critic objected lives in the
// validation receipt the finding cites, so the gate must surface the failing
// checks from that receipt. It must also not reproduce the passing ones, because
// a successful container build log is megabytes of noise and reproducing it is
// what made the readout unreadable to begin with.
func TestTaskBoardReviewDiagnosticsFromReceiptKeepsOnlyActionableChecks(t *testing.T) {
	// Shaped after the receipt this defect was diagnosed against: a passing
	// verdict whose baseline check legitimately fails, alongside a build log long
	// enough that carrying it whole is the bug.
	receipt := workflowkit.ValidationReceipt{
		Verdict: workflowkit.ValidationPass,
		Diagnostics: []workflowkit.AgentCommandReport{
			{CommandID: "layout_probe", ExitCode: 0},
			{CommandID: "environment_build", ExitCode: 0, StderrTail: strings.Repeat("#6 exporting layers done\n", 400)},
			{CommandID: "baseline_verify", ExitCode: 1, TestStarted: true, StderrTail: "FAIL: ScopedRouteBindingResolver.php is missing\n"},
			{CommandID: "oracle_verify", ExitCode: 0, TestStarted: true, StdoutTail: "contract checks passed\n"},
		},
	}

	diagnostics, summary, message := taskBoardReviewDiagnosticsFromReceipt(receipt)

	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want only the failing check", diagnostics)
	}
	if diagnostics[0].CommandID != "baseline_verify" || diagnostics[0].ExitCode != 1 {
		t.Errorf("diagnostic = %+v, want the failing baseline_verify", diagnostics[0])
	}
	if !strings.Contains(diagnostics[0].StderrTail, "ScopedRouteBindingResolver.php is missing") {
		t.Errorf("diagnostic stderr = %q, want the failure reason", diagnostics[0].StderrTail)
	}
	// The passing build log is the noise this readout must never carry.
	for _, diagnostic := range diagnostics {
		if strings.Contains(diagnostic.StderrTail, "exporting layers") {
			t.Error("diagnostics carried a passing build log")
		}
	}
	// A passing verdict with a failing check is normal here, and stating only one
	// of the two is exactly what an operator misreads.
	for _, want := range []string{"pass", "4", "1"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary = %q, want it to state %q", summary, want)
		}
	}
	if message != "" {
		t.Errorf("message = %q, want none when every failing check is shown", message)
	}
}

// TestTaskBoardReviewDiagnosticsFromReceiptExplainsAnAllPassingReceipt pins the
// pass-code case. A solution_integrity_pass finding cites a receipt with nothing
// failing; an empty list with no sentence would look like a failed read rather
// than a clean result.
func TestTaskBoardReviewDiagnosticsFromReceiptExplainsAnAllPassingReceipt(t *testing.T) {
	diagnostics, summary, message := taskBoardReviewDiagnosticsFromReceipt(workflowkit.ValidationReceipt{
		Verdict: workflowkit.ValidationPass,
		Diagnostics: []workflowkit.AgentCommandReport{
			{CommandID: "layout_probe", ExitCode: 0},
			{CommandID: "oracle_verify", ExitCode: 0, TestStarted: true},
		},
	})

	if len(diagnostics) != 0 {
		t.Errorf("diagnostics = %+v, want none", diagnostics)
	}
	if message == "" {
		t.Error("an all-passing receipt produced no explanation, so the section would look broken")
	}
	if !strings.Contains(summary, "0") {
		t.Errorf("summary = %q, want it to state zero failures", summary)
	}
}

// TestTaskBoardReviewDiagnosticsFromReceiptBoundsFloodAndTail pins both bounds a
// terminal depends on: a receipt reporting every command as failed cannot flood
// the gate screen, and one enormous tail cannot be carried whole.
func TestTaskBoardReviewDiagnosticsFromReceiptBoundsFloodAndTail(t *testing.T) {
	reports := make([]workflowkit.AgentCommandReport, 0, taskBoardReviewDiagnosticLimit+5)
	for index := 0; index < taskBoardReviewDiagnosticLimit+5; index++ {
		reports = append(reports, workflowkit.AgentCommandReport{
			CommandID: "check", ExitCode: 1, StderrTail: strings.Repeat("x", taskBoardReviewDiagnosticTailLimit*3),
		})
	}

	diagnostics, _, message := taskBoardReviewDiagnosticsFromReceipt(workflowkit.ValidationReceipt{
		Verdict: workflowkit.ValidationReject, Diagnostics: reports,
	})

	if len(diagnostics) != taskBoardReviewDiagnosticLimit {
		t.Fatalf("diagnostics = %d, want the %d bound", len(diagnostics), taskBoardReviewDiagnosticLimit)
	}
	if message == "" {
		t.Error("clipped diagnostics were not disclosed, so a reader could mistake them for the complete set")
	}
	for _, diagnostic := range diagnostics {
		if len(diagnostic.StderrTail) > taskBoardReviewDiagnosticTailLimit+len("…") {
			t.Errorf("stderr tail = %d bytes, want it bounded", len(diagnostic.StderrTail))
		}
	}
}

// TestBoundTaskBoardDiagnosticTailKeepsTheEndAndStaysValidUTF8 pins two
// properties a terminal depends on: a failure reason sits at the end of its
// output, and slicing bytes must never emit a split rune.
func TestBoundTaskBoardDiagnosticTailKeepsTheEndAndStaysValidUTF8(t *testing.T) {
	bounded := boundTaskBoardDiagnosticTail(strings.Repeat("a", taskBoardReviewDiagnosticTailLimit*2) + "FAIL: the reason")
	if !strings.HasSuffix(bounded, "FAIL: the reason") {
		t.Errorf("bounded tail = %q, want it to keep the end where the reason is", bounded[max(0, len(bounded)-40):])
	}

	// A multi-byte rune straddling the byte cut must never reach a terminal.
	multibyte := boundTaskBoardDiagnosticTail(strings.Repeat("检查失败", taskBoardReviewDiagnosticTailLimit))
	if !utf8.ValidString(multibyte) {
		t.Error("bounded tail is not valid UTF-8, so a split rune could reach a terminal")
	}
}
