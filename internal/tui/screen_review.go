package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/purplevoid/harbor-factory/internal/app"
)

// reviewScreen is the dedicated page an operator decides a review gate on.
//
// It exists because the board projection alone carries only the gate's kind and
// request ID. Deciding a gate from that is deciding blind: the operator could
// see that something was waiting but not what was waiting, what evidence it was
// bound to, or what an agent had already said about it. This screen renders the
// gate identity, the artifacts frozen into the gate, the agent finding bodies
// read from the Run's own critic stages, and every prior decision.
//
// It is also a full page rather than a section inside the detail body, which is
// what previously pushed the decision context off a short terminal.
type reviewScreen struct {
	taskName   string
	review     app.TaskBoardReview
	inspection *app.TaskBoardReviewInspection
	message    string
	pane       *scrollPane
}

func newReviewScreen(task *TaskItem, review app.TaskBoardReview) *reviewScreen {
	name := "任务审核"
	if task != nil && task.Name != "" {
		name = task.Name
	}
	return &reviewScreen{
		taskName: name,
		review:   review,
		message:  "正在读取门禁详情...",
		pane:     newScrollPane(),
	}
}

// SetInspection installs the loaded gate detail. A nil inspection with a
// message keeps the screen open and states why nothing could be read, rather
// than closing and leaving the operator without an explanation.
func (s *reviewScreen) SetInspection(inspection app.TaskBoardReviewInspection) {
	if s == nil {
		return
	}
	s.inspection = &inspection
	s.message = ""
	s.pane.GoToStart()
}

func (s *reviewScreen) SetMessage(message string) {
	if s != nil {
		s.message = message
		s.inspection = nil
	}
}

func (s *reviewScreen) MoveUp() {
	if s != nil {
		s.pane.MoveUp()
	}
}

func (s *reviewScreen) MoveDown() {
	if s != nil {
		s.pane.MoveDown()
	}
}

func (s *reviewScreen) PageUp() {
	if s != nil {
		s.pane.PageUp()
	}
}

func (s *reviewScreen) PageDown() {
	if s != nil {
		s.pane.PageDown()
	}
}

func (s *reviewScreen) GoToStart() {
	if s != nil {
		s.pane.GoToStart()
	}
}

func (s *reviewScreen) GoToEnd() {
	if s != nil {
		s.pane.GoToEnd()
	}
}

func (s *reviewScreen) bodyContent(width int) string {
	if s.inspection == nil {
		return mutedStyle.Render(s.message)
	}
	inspection := *s.inspection
	sections := []string{
		s.identitySection(inspection, width),
		s.artifactSection(inspection, width),
		s.findingSection(inspection, width),
		s.decisionSection(inspection, width),
	}
	return strings.Join(sections, "\n")
}

// identitySection states exactly which durable gate is being decided.
func (s *reviewScreen) identitySection(inspection app.TaskBoardReviewInspection, width int) string {
	fields := []string{
		detailField("门禁阶段", displayStageName(inspection.StageKey), width),
		detailField("门禁类型", displayReviewGateKind(inspection), width),
		detailField("审核请求", inspection.RequestID, width),
		detailField("Run ID", inspection.RunID, width),
		detailField("打开时间", formatDetailTime(inspection.OpenedAt), width),
		detailField("门禁状态", inspection.State, width),
	}
	if inspection.RevisionID != "" {
		fields = append(fields, detailField("Revision", inspection.RevisionID, width))
	}
	if inspection.InputFingerprint != "" {
		fields = append(fields, detailField("输入指纹", inspection.InputFingerprint, width))
	}
	if inspection.EvidenceManifestDigest != "" {
		fields = append(fields, detailField("证据摘要", inspection.EvidenceManifestDigest, width))
	}
	if inspection.DefinitionHash != "" {
		fields = append(fields, detailField("工作流指纹", inspection.DefinitionHash, width))
	}
	return renderDetailSection("门禁身份", detailFields(fields...), width)
}

// artifactSection lists what the gate froze as its inputs. These are the
// artifacts the decision actually applies to.
func (s *reviewScreen) artifactSection(inspection app.TaskBoardReviewInspection, width int) string {
	if len(inspection.Artifacts) == 0 {
		body := mutedStyle.Render("此门禁没有记录待审产物")
		if inspection.ArtifactsMessage != "" {
			body = styleFail.Render(inspection.ArtifactsMessage)
		}
		return renderDetailSection("待审产物", body, width)
	}
	fields := make([]string, 0, len(inspection.Artifacts)*3)
	for _, artifact := range inspection.Artifacts {
		// An artifact key is an identifier the operator must be able to read
		// exactly, and keys like task_specification are wider than the label
		// gutter. Give the name its own full-width row so it is never split
		// mid-word, and carry its digest and schema as labelled rows beneath it.
		fields = append(fields,
			detailValueStyle.Render(truncateDisplay(artifact.Name, max(1, width-detailFieldGutter))),
			detailField("  摘要", artifact.ContentDigest, width),
		)
		if artifact.SchemaVersion != "" {
			fields = append(fields, detailField("  schema", artifact.SchemaVersion, width))
		}
	}
	if inspection.ArtifactsMessage != "" {
		fields = append(fields, styleFail.Render(inspection.ArtifactsMessage))
	}
	return renderDetailSection(fmt.Sprintf("待审产物 (%d)", len(inspection.Artifacts)), detailFields(fields...), width)
}

// findingSection renders the agent review opinions in full.
//
// A gate with no findings is not a broken screen. task_review runs before every
// critic in the authoring graph, so it structurally cannot carry an agent
// opinion; the application layer says so in AgentFindingsMessage and this
// section shows that sentence verbatim instead of an empty box.
func (s *reviewScreen) findingSection(inspection app.TaskBoardReviewInspection, width int) string {
	if len(inspection.AgentFindings) == 0 {
		body := mutedStyle.Render("此门禁无 agent 评审意见")
		if inspection.AgentFindingsMessage != "" {
			body = mutedStyle.Render(wrapDisplay(inspection.AgentFindingsMessage, max(1, width-detailFieldGutter)))
		}
		return renderDetailSection("Agent 评审意见", body, width)
	}
	blocks := make([]string, 0, len(inspection.AgentFindings))
	for _, finding := range inspection.AgentFindings {
		fields := []string{
			detailField("产出阶段", displayStageName(finding.StageKey), width),
			detailField("结论代码", finding.Code, width),
			detailField("修复目标", displayStageName(finding.TargetWriter), width),
		}
		if finding.EvidenceDigest != "" {
			fields = append(fields, detailField("证据摘要", finding.EvidenceDigest, width))
		}
		if finding.CandidateDigest != "" {
			fields = append(fields, detailField("候选摘要", finding.CandidateDigest, width))
		}
		if finding.RecordedAt != nil {
			fields = append(fields, detailField("记录时间", formatDetailTime(finding.RecordedAt), width))
		}
		block := detailFields(fields...)
		// The cited diagnostic comes before the verbatim body: it is the only
		// human-readable account of why the critic objected, and a reader should
		// reach it without scrolling past the finding envelope.
		block += renderFindingDiagnostics(finding, width)
		if body := strings.TrimSpace(finding.Body); body != "" {
			block += "\n" + wrapDisplay(body, max(1, width-detailFieldGutter))
		}
		if finding.BodyTruncated {
			block += "\n" + styleFail.Render(fmt.Sprintf("正文超过 %d KiB 上限，已截断显示", app.TaskBoardReviewFindingBodyLimit/1024))
		}
		if finding.Message != "" {
			block += "\n" + styleFail.Render(finding.Message)
		}
		blocks = append(blocks, block)
	}
	body := strings.Join(blocks, "\n\n")
	if inspection.AgentFindingsMessage != "" {
		body += "\n" + mutedStyle.Render(inspection.AgentFindingsMessage)
	}
	return renderDetailSection(fmt.Sprintf("Agent 评审意见 (%d)", len(inspection.AgentFindings)), body, width)
}

// renderFindingDiagnostics renders the failing checks a finding cites.
//
// A WorkflowFinding carries no prose: its fields are a closed code, two stage
// keys, and digests. Rendering only those told an operator that a critic
// objected but never why. The cited validation receipt holds the actual
// account -- which command failed, its exit code, and its output tail -- so
// this block is the readable part of an otherwise machine-facing record.
//
// It returns a leading newline with its content, or the empty string, so a
// caller can append it unconditionally.
func renderFindingDiagnostics(finding app.TaskBoardReviewFinding, width int) string {
	if len(finding.Diagnostics) == 0 {
		if finding.DiagnosticMessage == "" {
			return ""
		}
		return "\n" + mutedStyle.Render(wrapDisplay(finding.DiagnosticMessage, max(1, width-detailFieldGutter)))
	}
	rows := make([]string, 0, len(finding.Diagnostics)*3+1)
	if finding.DiagnosticSummary != "" {
		rows = append(rows, detailField("校验结论", finding.DiagnosticSummary, width))
	}
	for _, diagnostic := range finding.Diagnostics {
		// The command id and its exit code belong on one line: the pair is the
		// identity of the failure, and splitting them across rows made a reader
		// re-associate them by eye.
		rows = append(rows, styleFail.Render(truncateDisplay(
			fmt.Sprintf("✗ %s (exit %d)", diagnostic.CommandID, diagnostic.ExitCode),
			max(1, width-detailFieldGutter),
		)))
		// Tails are pre-bounded by the application layer. They are indented as
		// verbatim output rather than wrapped into prose, because a stack trace
		// or assertion line loses its meaning when reflowed.
		for _, tail := range []string{diagnostic.StderrTail, diagnostic.StdoutTail} {
			for _, line := range strings.Split(strings.TrimRight(tail, "\n"), "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				rows = append(rows, mutedStyle.Render(truncateDisplay("    "+line, max(1, width-detailFieldGutter))))
			}
		}
	}
	if finding.DiagnosticMessage != "" {
		rows = append(rows, mutedStyle.Render(wrapDisplay(finding.DiagnosticMessage, max(1, width-detailFieldGutter))))
	}
	return "\n" + strings.Join(rows, "\n")
}

// decisionSection shows the immutable decision history for this gate.
func (s *reviewScreen) decisionSection(inspection app.TaskBoardReviewInspection, width int) string {
	if len(inspection.PriorDecisions) == 0 {
		return renderDetailSection("历史决策", mutedStyle.Render("此门禁尚无决策记录"), width)
	}
	rows := make([]string, 0, len(inspection.PriorDecisions)*2)
	for _, decision := range inspection.PriorDecisions {
		rows = append(rows, detailField(displayReviewAction(decision.Action), decision.Actor+" · "+formatDetailTime(&decision.CreatedAt), width))
		if reason := strings.TrimSpace(decision.Reason); reason != "" {
			rows = append(rows, wrapDisplay(reason, max(1, width-detailFieldGutter)))
		}
	}
	return renderDetailSection(fmt.Sprintf("历史决策 (%d)", len(inspection.PriorDecisions)), strings.Join(rows, "\n"), width)
}

func (s *reviewScreen) View(width, bodyRows int) string {
	contentWidth := max(24, width)
	header := lipgloss.JoinVertical(lipgloss.Left,
		detailBreadcrumbStyle.Render("题目管理 / 任务详情 / 审核门禁"),
		detailTitleStyle.Width(max(1, contentWidth-2)).Render(
			truncateDisplay(s.taskName+" · "+displayReviewKind(s.review), max(1, contentWidth-4)),
		),
		// The operator is told the consequence before deciding, not after.
		detailSubtitleStyle.Render("审核意见将写入不可变决策记录"),
	)
	paneRows := max(1, bodyRows-lipgloss.Height(header))
	m := s.pane
	m.Resize(contentWidth, paneRows)
	m.SetContent(s.bodyContent(contentWidth), contentWidth)
	return lipgloss.JoinVertical(lipgloss.Left, header, m.View())
}

func displayReviewGateKind(inspection app.TaskBoardReviewInspection) string {
	if inspection.ReviewKind != "" {
		return inspection.ReviewKind
	}
	return string(inspection.Kind)
}

func displayReviewAction(action string) string {
	switch action {
	case "approve":
		return "通过"
	case "request_changes":
		return "返修"
	case "reject_terminal":
		return "终止拒绝"
	default:
		return action
	}
}
