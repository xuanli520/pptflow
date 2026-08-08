package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

func reviewGateSnapshot() app.TaskBoardSnapshot {
	return app.TaskBoardSnapshot{
		AuthoringAvailable: true,
		Tasks: []app.TaskBoardTask{{
			ID: "task-1", Slug: "scoped-route-binding", Title: "Refactor scoped route-binding resolution",
			Column: app.TaskBoardPending, RunID: "run-1", RunStatus: "waiting_review",
			Review: &app.TaskBoardReview{Kind: app.TaskBoardAuthoringReview, RequestID: "review-1"},
			Runs: []app.TaskBoardRun{{
				ID: "run-1", Status: "waiting_review", CurrentStage: workflowadapter.SolutionReview,
			}},
		}},
	}
}

// TestReviewScreenShowsArtifactsAndAgentOpinion is the regression guard for the
// defect where the detail pane knew a gate was open but showed only its kind and
// request ID, so an operator had to approve or reject without seeing either the
// artifacts under review or any agent opinion about them.
func reviewInspectionStub() *taskBoardGatewayStub {
	recorded := time.Date(2026, 8, 7, 9, 12, 0, 0, time.UTC)
	return &taskBoardGatewayStub{
		snapshot: reviewGateSnapshot(),
		reviewInspection: app.TaskBoardReviewInspection{
			Kind: app.TaskBoardAuthoringReview, RequestID: "review-1",
			StageKey: workflowadapter.SolutionReview, ReviewKind: "solution_verifier",
			RunID: "run-1", State: "open", OpenedAt: &recorded, OpenedBy: "worker-1",
			InputFingerprint: "sha256:inputs", EvidenceManifestDigest: "sha256:evidence",
			BindingFingerprint: "sha256:binding",
			Artifacts: []app.TaskBoardReviewArtifact{
				{Name: "instruction", ArtifactID: "artifact-1", ContentDigest: "sha256:aaaa", SchemaVersion: "harbor.instruction.v1"},
				{Name: "test_script", ArtifactID: "artifact-2", ContentDigest: "sha256:bbbb"},
			},
			AgentFindings: []app.TaskBoardReviewFinding{{
				ArtifactKey: "solution_integrity_finding", StageKey: workflowadapter.SolutionIntegrityCritic,
				Code: "solution_integrity_defect", ProducingStage: workflowadapter.SolutionIntegrityCritic,
				TargetWriter: workflowadapter.AuthoringRepair, EvidenceDigest: "sha256:cccc",
				Body:       `{"code":"solution_integrity_defect","producing_stage":"solution_integrity_critic"}`,
				RecordedAt: &recorded,
			}},
			PriorDecisions: []app.TaskBoardReviewDecisionRecord{{
				Action: "request_changes", Actor: "operator", Reason: "tests did not bind the claimed behavior", CreatedAt: recorded,
			}},
		},
	}
}

func TestReviewScreenShowsArtifactsAndAgentOpinion(t *testing.T) {
	stub := reviewInspectionStub()

	model := loadedTaskBoardModel(t, stub)
	// Content is asserted at a window tall enough to hold the whole gate. The
	// body scrolls, so on a short terminal the lower sections are legitimately
	// off-pane; the fit invariant below covers the short sizes separately.
	model.width, model.height = 120, 60
	model.detail = newDetailModel(model.board.SelectedTask())

	updated, command := model.handleKey(keyRune('v'), nil)
	model = updated.(appModel)
	if model.reviewGate == nil {
		t.Fatal("[v] did not open the review gate screen")
	}
	if command == nil {
		t.Fatal("opening the review screen did not request an inspection")
	}
	model = applyCommand(t, model, command)
	if len(stub.inspectReviewRequests) != 1 || stub.inspectReviewRequests[0].Review.RequestID != "review-1" {
		t.Fatalf("inspection requests = %+v", stub.inspectReviewRequests)
	}

	rendered := ansi.Strip(model.View())
	for _, expected := range []string{
		// The artifacts actually under review, with the digests they are bound to.
		"instruction", "sha256:aaaa", "test_script",
		// The agent opinion body, which the board projection alone never carried.
		"solution_integrity_defect", "Agent 评审意见",
		// Prior decisions, localized the same way every other action label is.
		"返修", "tests did not bind the claimed behavior",
		// The checkpoint identity an operator can compare before deciding.
		"sha256:inputs",
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("review screen missing %q", expected)
		}
	}

	// The gate screen must fit every terminal an operator actually uses. This is
	// the invariant the old inline review section broke.
	for _, size := range terminalSizes {
		sized := model
		sized.width, sized.height = size[0], size[1]
		assertFitsWindow(t, "review gate", sized.View(), size[0], size[1])
	}
}

// TestReviewScreenStatesTaskReviewHasNoAgentOpinion pins the plan's explicit
// requirement: task_review runs before every critic, so having no agent opinion
// there is correct and the screen must say so rather than look broken.
func TestReviewScreenStatesTaskReviewHasNoAgentOpinion(t *testing.T) {
	const note = "此门禁无 agent 评审意见（critic 阶段在其后执行）"
	stub := &taskBoardGatewayStub{
		snapshot: reviewGateSnapshot(),
		reviewInspection: app.TaskBoardReviewInspection{
			Kind: app.TaskBoardAuthoringReview, RequestID: "review-1",
			StageKey: workflowadapter.TaskReview, ReviewKind: "task_direction", RunID: "run-1", State: "open",
			Artifacts: []app.TaskBoardReviewArtifact{
				{Name: "task_specification", ArtifactID: "artifact-1", ContentDigest: "sha256:aaaa"},
			},
			AgentFindingsMessage: note,
		},
	}
	model := loadedTaskBoardModel(t, stub)
	model.width, model.height = 120, 60
	model.detail = newDetailModel(model.board.SelectedTask())
	updated, command := model.handleKey(keyRune('v'), nil)
	model = applyCommand(t, updated.(appModel), command)

	rendered := ansi.Strip(model.View())
	if !strings.Contains(rendered, "无 agent 评审意见") {
		t.Fatalf("task_review gate did not explain its missing agent opinion:\n%s", rendered)
	}
	if !strings.Contains(rendered, "task_specification") {
		t.Error("task_review gate did not list the artifact under review")
	}
	for _, size := range terminalSizes {
		sized := model
		sized.width, sized.height = size[0], size[1]
		assertFitsWindow(t, "task_review gate", sized.View(), size[0], size[1])
	}
}

// TestReviewScreenReportsInspectionFailure keeps a failed read visible instead
// of rendering an empty gate that looks like there is nothing to review.
func TestReviewScreenReportsInspectionFailure(t *testing.T) {
	stub := &taskBoardGatewayStub{
		snapshot:            reviewGateSnapshot(),
		reviewInspectionErr: errStubInspection,
	}
	model := loadedTaskBoardModel(t, stub)
	model.width, model.height = 120, 30
	model.detail = newDetailModel(model.board.SelectedTask())
	updated, command := model.handleKey(keyRune('v'), nil)
	model = applyCommand(t, updated.(appModel), command)

	if rendered := ansi.Strip(model.View()); !strings.Contains(rendered, errStubInspection.Error()) {
		t.Fatalf("review screen hid its inspection failure:\n%s", rendered)
	}
}

// TestReviewDecisionSubmitsFromReviewScreen pins that the gate screen is a real
// decision surface: the approve/reject keys reach the durable mutation with the
// operator's reason, and the multi-line reason submits on its stated chord.
func TestReviewDecisionSubmitsFromReviewScreen(t *testing.T) {
	stub := &taskBoardGatewayStub{snapshot: reviewGateSnapshot()}
	model := loadedTaskBoardModel(t, stub)
	model.width, model.height = 120, 30
	model.detail = newDetailModel(model.board.SelectedTask())
	updated, command := model.handleKey(keyRune('v'), nil)
	model = applyCommand(t, updated.(appModel), command)

	updated, _ = model.handleKey(keyRune('a'), nil)
	model = updated.(appModel)
	if model.review == nil || model.review.decision != app.TaskBoardApprove {
		t.Fatalf("approve prompt = %+v", model.review)
	}
	if footer := ansi.Strip(model.View()); !strings.Contains(footer, "[ctrl+s] 提交审核") {
		t.Error("review prompt footer does not state its submit chord")
	}
	// An empty reason must not reach the durable mutation.
	updated, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS}, nil)
	model = updated.(appModel)
	if len(stub.decisionRequests) != 0 {
		t.Fatalf("empty reason submitted a decision: %+v", stub.decisionRequests)
	}

	model.review.reasonInput.SetValue("verified the bound tests\nand the instruction")
	updated, decisionCmd := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS}, nil)
	model = updated.(appModel)
	if decisionCmd == nil {
		t.Fatal("ctrl+s did not submit the review decision")
	}
	if _, ok := decisionCmd().(taskBoardMutationMsg); !ok {
		t.Fatal("review submit did not produce a mutation message")
	}
	if len(stub.decisionRequests) != 1 {
		t.Fatalf("decision requests = %+v", stub.decisionRequests)
	}
	request := stub.decisionRequests[0]
	if request.Decision != app.TaskBoardApprove || request.Review.RequestID != "review-1" || request.IdempotencyKey == "" {
		t.Fatalf("decision request = %+v", request)
	}
	if !strings.Contains(request.Reason, "verified the bound tests") || !strings.Contains(request.Reason, "and the instruction") {
		t.Fatalf("multi-line reason was not preserved: %q", request.Reason)
	}
}

// errStubInspection is a fixed gateway failure used to assert the screen shows
// a failed read instead of an empty gate.
var errStubInspection = errors.New("review inspection unavailable")

// applyCommand runs a command and feeds every resulting message back into the
// model. tea.Batch returns a BatchMsg holding its inner commands rather than
// their messages, so a test that called the command directly would only ever
// see the wrapper and never the message the model actually reacts to.
func applyCommand(t *testing.T, model appModel, command tea.Cmd) appModel {
	t.Helper()
	if command == nil {
		return model
	}
	var apply func(tea.Cmd)
	apply = func(current tea.Cmd) {
		if current == nil {
			return
		}
		switch msg := current().(type) {
		case tea.BatchMsg:
			for _, inner := range msg {
				apply(inner)
			}
		case nil:
		default:
			updated, next := model.Update(msg)
			model = updated.(appModel)
			// A message can produce follow-up work; ignore further commands here so
			// the helper stays a single deterministic step.
			_ = next
		}
	}
	apply(command)
	return model
}

// TestReviewPromptEnterInsertsNewlineAndCtrlSSubmits pins the multi-line reason
// contract. A review reason is prose an operator writes into an immutable
// decision record, so enter must add a line rather than submit; only the
// explicit chord commits, and the footer names it.
func TestReviewPromptEnterInsertsNewlineAndCtrlSSubmits(t *testing.T) {
	stub := reviewInspectionStub()
	model := loadedTaskBoardModel(t, stub)
	model.width, model.height = 120, 30
	model.detail = newDetailModel(model.board.SelectedTask())

	updated, _ := model.handleKey(keyRune('a'), nil)
	model = updated.(appModel)
	if model.review == nil {
		t.Fatal("[a] did not open the review reason prompt")
	}
	model.review.reasonInput.SetValue("first line")
	updated, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	if model.review == nil {
		t.Fatal("enter submitted the review instead of inserting a newline")
	}
	if model.activeMutation != "" || len(stub.decisionRequests) != 0 {
		t.Fatalf("enter dispatched a decision: active=%q requests=%+v", model.activeMutation, stub.decisionRequests)
	}
	model.review.reasonInput.InsertString("second line")
	if value := model.review.reasonInput.Value(); !strings.Contains(value, "\n") {
		t.Fatalf("reason value did not keep its newline: %q", value)
	}

	multiline := model.review.reasonInput.Value()
	updated, command := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS}, nil)
	model = updated.(appModel)
	if command == nil || model.activeMutation != taskBoardReviewMutation {
		t.Fatalf("ctrl+s did not submit: active=%q command=%v", model.activeMutation, command)
	}
	_ = command().(taskBoardMutationMsg)
	if len(stub.decisionRequests) != 1 || stub.decisionRequests[0].Reason != strings.TrimSpace(multiline) {
		t.Fatalf("submitted reason = %+v, want the multi-line value %q", stub.decisionRequests, multiline)
	}
}

// TestReviewScreenRendersCitedDiagnosticsInsteadOfDigestOnly is the regression
// guard for the defect where the gate screen showed a critic's opinion as raw
// envelope JSON plus a bare digest.
//
// A WorkflowFinding is metadata by design: a closed code, two stage keys, and
// three digests, none of which say why the critic objected. The reason lives in
// the validation receipt the finding cites through its diagnostic digest, so the
// screen must resolve that citation and show the failing command. Rendering the
// digest alone left the one actionable sentence unreachable from the terminal.
func TestReviewScreenRendersCitedDiagnosticsInsteadOfDigestOnly(t *testing.T) {
	recorded := time.Date(2026, 8, 8, 0, 29, 22, 0, time.UTC)
	stub := reviewInspectionStub()
	stub.reviewInspection.AgentFindings = []app.TaskBoardReviewFinding{{
		ArtifactKey: "test_quality_finding", StageKey: workflowadapter.TestQualityCritic,
		Code: "test_quality_defect", ProducingStage: workflowadapter.TestQualityCritic,
		TargetWriter:   workflowadapter.AuthoringRepair,
		EvidenceDigest: "sha256:99e41d24", CandidateDigest: "sha256:d634e845",
		RecordedAt:        &recorded,
		DiagnosticSummary: "裁决 pass · 6 项检查 · 1 项失败",
		Diagnostics: []app.TaskBoardReviewDiagnostic{{
			CommandID: "baseline_verify", ExitCode: 1, TestStarted: true,
			StderrTail: "FAIL: ScopedRouteBindingResolver.php is missing\n",
		}},
	}}

	model := loadedTaskBoardModel(t, stub)
	model.width, model.height = 120, 60
	model.detail = newDetailModel(model.board.SelectedTask())
	updated, command := model.handleKey(keyRune('v'), nil)
	model = updated.(appModel)
	model = applyCommand(t, model, command)

	rendered := ansi.Strip(model.View())
	for _, expected := range []string{
		// The failing check and its exit code: the identity of the objection.
		"baseline_verify", "exit 1",
		// The actual reason, which previously existed only behind a digest.
		"FAIL: ScopedRouteBindingResolver.php is missing",
		// The verdict beside the counts, so a passing receipt carrying a failing
		// baseline check cannot be misread as a rejected one.
		"裁决 pass", "1 项失败",
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("review screen missing cited diagnostic %q", expected)
		}
	}

	for _, size := range terminalSizes {
		sized := model
		sized.width, sized.height = size[0], size[1]
		assertFitsWindow(t, "review gate diagnostics", sized.View(), size[0], size[1])
	}
}

// TestReviewScreenStatesWhenCitedReceiptHasNoFailingCheck covers the pass-code
// finding. solution_integrity_pass cites a receipt whose every check passed, and
// an empty diagnostics block there is correct rather than a failed read -- so the
// screen must say so instead of rendering nothing.
func TestReviewScreenStatesWhenCitedReceiptHasNoFailingCheck(t *testing.T) {
	stub := reviewInspectionStub()
	stub.reviewInspection.AgentFindings = []app.TaskBoardReviewFinding{{
		ArtifactKey: "solution_integrity_finding", StageKey: workflowadapter.SolutionIntegrityCritic,
		Code: "solution_integrity_pass", TargetWriter: workflowadapter.AuthoringRepair,
		DiagnosticMessage: "该回执所有检查均通过",
	}}

	model := loadedTaskBoardModel(t, stub)
	model.width, model.height = 120, 60
	model.detail = newDetailModel(model.board.SelectedTask())
	updated, command := model.handleKey(keyRune('v'), nil)
	model = updated.(appModel)
	model = applyCommand(t, model, command)

	if rendered := ansi.Strip(model.View()); !strings.Contains(rendered, "该回执所有检查均通过") {
		t.Error("review screen did not state that the cited receipt had no failing check")
	}
}
