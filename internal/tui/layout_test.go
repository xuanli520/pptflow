package tui

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/purplevoid/harbor-factory/internal/app"
)

// terminalSizes covers the window shapes an operator actually uses. Every
// screen must fit all of them: a screen taller than its window scrolls its
// own header off the top of an alt-screen terminal, which is exactly the
// defect this file guards against.
var terminalSizes = [][2]int{{100, 24}, {120, 30}, {140, 41}}

// evidenceLoadedSnapshot is the worst realistic case for vertical budget: a
// failed Run carrying full authoring evidence, an operator summary, agent
// transcripts, and an open review gate.
func evidenceLoadedSnapshot() app.TaskBoardSnapshot {
	recorded := time.Date(2026, 8, 7, 7, 43, 25, 0, time.UTC)
	return app.TaskBoardSnapshot{
		AuthoringAvailable: true,
		Tasks: []app.TaskBoardTask{{
			ID:            "task-1",
			Slug:          "scoped-route-binding",
			Title:         "Refactor scoped route-binding resolution",
			RepositoryURL: "https://example.invalid/some/rather/long/repository/path.git",
			CommitSHA:     "abcdef0123456789abcdef0123456789abcdef01",
			Column:        app.TaskBoardPending,
			Review:        &app.TaskBoardReview{Kind: app.TaskBoardAuthoringReview, RequestID: "review-1"},
			RunID:         "run-1",
			RunStatus:     "failed_recoverable",
			OperatorSummary: &app.TaskBoardOperatorSummary{
				Status: "validation_rejected", Cause: "oracle verify failed", NextAction: "repair",
				LatestValidation: &app.TaskBoardLatestValidation{
					Status: "recorded", Verdict: "rejected", Stage: "host_candidate_verify",
					StageExecutionStatus: "succeeded", StageVerdict: "rejected",
					FailureCode: "verify.oracle_failed", RecordedAt: &recorded,
				},
			},
			Runs: []app.TaskBoardRun{{
				ID: "run-1", Status: "failed_recoverable", CurrentStage: "task_synthesis",
				LogPath:               "/home/operator/.harbor-factory/runs/019fdb2d-3bcc-7f77-9529-faf91173ac61/worker.log",
				CanRetry:              true,
				RetryStrategy:         app.TaskBoardRetryStrategyTaskContinuation,
				FailureStage:          "task_synthesis",
				FailureCode:           "agent.protocol_rejected",
				FailureSummary:        strings.Repeat("The agent submission was rejected because declared artifact claims did not match the root contract. ", 3),
				FailureRecordedAt:     &recorded,
				FailureRecoveryAction: app.TaskBoardFailureRecoveryRepairOrNewRun,
				CreatedAt:             recorded,
				OperatorSummary: &app.TaskBoardOperatorSummary{
					Status: "validation_rejected", Cause: "oracle verify failed", NextAction: "repair",
				},
				AuthoringEvidence: &app.TaskBoardAuthoringEvidence{
					Contract: app.TaskBoardAuthoringContract{
						Digest: "sha256:104e9f3ab18c99ab840eb56d7c4c997284892083e6567aa64e0ff8d9bc63aef5",
						Slug:   "scoped-route-binding", Title: "Refactor scoped route-binding resolution",
						CodeLang: "php", TaskType: "refactor", Application: "backend",
						RepositoryURL:  "https://example.invalid/some/rather/long/repository/path.git",
						CommitSHA:      "abcdef0123456789abcdef0123456789abcdef01",
						SnapshotDigest: "sha256:aaaabbbbccccddddeeeeffff0000111122223333444455556666777788889999",
						CheckoutRoot:   "/work/src", BaseImage: "registry.example.invalid/base:tag@sha256:bbbb",
						Objective: "Add the requested bounded behavior", ProfileFingerprint: "sha256:cccc",
						PackageFormat: "codeedge.v1",
					},
					Claims: []app.TaskBoardAuthoringClaim{
						{ArtifactKey: "instruction", State: "matched"},
						{ArtifactKey: "task_toml", State: "matched"},
						{ArtifactKey: "dockerfile", State: "matched"},
					},
					Lineage: []app.TaskBoardAuthoringArtifact{
						{ArtifactKey: "final_package", ArtifactID: "artifact-1", Digest: "sha256:dddd"},
						{ArtifactKey: "docker_image", ArtifactID: "artifact-2", Digest: "sha256:eeee"},
					},
				},
				AgentTurnTranscripts: []app.TaskBoardAgentTranscript{
					{ID: "t1", StageKey: "task_synthesis", Turn: 3, SubmissionStatus: "rejected", ModelID: "model-a", ResponseText: strings.Repeat("rejected because the claim set drifted. ", 12), CreatedAt: recorded, ExpiresAt: recorded},
					{ID: "t2", StageKey: "task_synthesis", Turn: 2, SubmissionStatus: "accepted", ModelID: "model-a", CreatedAt: recorded, ExpiresAt: recorded},
				},
			}},
		}},
	}
}

// assertFitsWindow is the core layout invariant: a rendered screen must never
// exceed the window it was given in either dimension. A screen taller than its
// window scrolls its own header off the top of an alt-screen terminal, and a
// screen wider than its window wraps every line and doubles the overflow.
func assertFitsWindow(t *testing.T, label string, rendered string, width, height int) {
	t.Helper()
	plain := ansi.Strip(rendered)
	if actual := lipgloss.Height(plain); actual > height {
		t.Errorf("%s height %d exceeds window height %d (%dx%d)", label, actual, height, width, height)
	}
	for _, line := range strings.Split(plain, "\n") {
		if actual := lipgloss.Width(line); actual > width {
			t.Errorf("%s line width %d exceeds window width %d (%dx%d): %q", label, actual, width, width, height, line)
			return
		}
	}
}

// TestEveryScreenFitsItsWindow is the regression guard for the defect where a
// detail body rendered a fixed ~71 lines regardless of terminal height, which
// scrolled the header and task cards off an alt-screen terminal.
func TestEveryScreenFitsItsWindow(t *testing.T) {
	for _, size := range terminalSizes {
		width, height := size[0], size[1]
		newModel := func(t *testing.T) appModel {
			t.Helper()
			stub := &taskBoardGatewayStub{snapshot: evidenceLoadedSnapshot()}
			model := loadedTaskBoardModel(t, stub)
			model.width, model.height = width, height
			return model
		}

		t.Run("board", func(t *testing.T) {
			model := newModel(t)
			assertFitsWindow(t, "board", model.View(), width, height)
		})

		t.Run("new task config input", func(t *testing.T) {
			model := newModel(t)
			model.input.BeginConfigLoad()
			assertFitsWindow(t, "new task config input", model.View(), width, height)
		})

		t.Run("new task edit input", func(t *testing.T) {
			model := newModel(t)
			model.input.Show()
			model.input.validationErr = "URL, SHA, base image, slug, title, task type, application, code language, objective, and reason are required"
			assertFitsWindow(t, "new task edit input", model.View(), width, height)
		})

		t.Run("detail", func(t *testing.T) {
			model := newModel(t)
			model.detail = newDetailModel(model.board.SelectedTask())
			assertFitsWindow(t, "detail", model.View(), width, height)
		})

		t.Run("detail with review prompt", func(t *testing.T) {
			model := newModel(t)
			model.detail = newDetailModel(model.board.SelectedTask())
			model.review = newReviewPrompt(app.TaskBoardApprove)
			assertFitsWindow(t, "review prompt", model.View(), width, height)
		})

		t.Run("detail with run action prompt", func(t *testing.T) {
			model := newModel(t)
			model.detail = newDetailModel(model.board.SelectedTask())
			model.action = newRunActionPrompt(taskBoardRetryAction, app.TaskBoardRetryStrategyTaskContinuation)
			assertFitsWindow(t, "action prompt", model.View(), width, height)
		})

		t.Run("log", func(t *testing.T) {
			model := newModel(t)
			model.detail = newDetailModel(model.board.SelectedTask())
			model.logs = newLogModel(model.detail.task, app.TaskBoardLog{
				RunID:   "run-1",
				Path:    "/home/operator/.harbor-factory/runs/019fdb2d/worker.log",
				Content: strings.Repeat("a fairly long worker log line that keeps going\n", 400),
			})
			assertFitsWindow(t, "log", model.View(), width, height)
		})

		t.Run("transcript", func(t *testing.T) {
			model := newModel(t)
			model.detail = newDetailModel(model.board.SelectedTask())
			model.transcript = newAgentTranscriptModel(model.detail.task)
			assertFitsWindow(t, "transcript", model.View(), width, height)
		})
	}
}

// TestDetailKeepsHeaderAndBreadcrumbVisible pins the actual operator symptom:
// the top-of-screen chrome must survive an evidence-loaded task.
func TestDetailKeepsHeaderAndBreadcrumbVisible(t *testing.T) {
	stub := &taskBoardGatewayStub{snapshot: evidenceLoadedSnapshot()}
	model := loadedTaskBoardModel(t, stub)
	model.width, model.height = 120, 30
	model.detail = newDetailModel(model.board.SelectedTask())

	lines := strings.Split(ansi.Strip(model.View()), "\n")
	if len(lines) > model.height {
		t.Fatalf("detail rendered %d lines into a %d-row window", len(lines), model.height)
	}
	if !strings.Contains(lines[0], "Harbor Task Factory") {
		t.Errorf("first rendered line is not the header: %q", lines[0])
	}
}

// TestTruncationPreservesUTF8AndWidth guards the byte-slicing truncation bug:
// cutting CJK text by byte index produces invalid UTF-8 and misreports width.
func TestTruncationPreservesUTF8AndWidth(t *testing.T) {
	cases := []struct {
		name  string
		value string
		limit int
	}{
		{name: "cjk status", value: "验证拒绝: 参考解答验证未通过，已记录了失败码", limit: 20},
		{name: "cjk status wide", value: "验证拒绝: 参考解答验证未通过，已记录了失败码", limit: 31},
		{name: "mixed", value: "stage 题目设计 failed with code agent.protocol_rejected", limit: 24},
		{name: "ascii", value: strings.Repeat("abcdef", 20), limit: 17},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := truncateDisplay(testCase.value, testCase.limit)
			if !utf8.ValidString(got) {
				t.Errorf("truncateDisplay(%q, %d) produced invalid UTF-8: %q", testCase.value, testCase.limit, got)
			}
			if width := lipgloss.Width(got); width > testCase.limit {
				t.Errorf("truncateDisplay(%q, %d) width %d exceeds limit", testCase.value, testCase.limit, width)
			}
		})
	}
}
