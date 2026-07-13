package tui

import (
	"fmt"
	"os"
	"os/user"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

const taskHubPackageVersionField = "release_version"

type taskHubMutationPhase string

const (
	taskHubMutationReady     taskHubMutationPhase = "ready"
	taskHubMutationPreparing taskHubMutationPhase = "preparing"
	taskHubMutationExecuting taskHubMutationPhase = "executing"
)

// TaskHubMutationOverlay is the native confirmation form for V2 lifecycle
// mutations. It owns only interaction state: application services remain the
// sole owners of plans, durable commands, and all persistent effects.
type TaskHubMutationOverlay struct {
	Action         TaskHubAction
	ControlAction  TaskHubRunControlAction
	Target         TaskHubTarget
	Expected       TaskHubControlCheckpoint
	Preview        TaskHubPlanPreview
	Actor          string
	IdempotencyKey string
	FrozenActor    string
	FrozenReason   string
	ReasonInput    textinput.Model
	ValueInputs    map[string]textinput.Model
	FieldOrder     []string
	FocusedField   int
	Phase          taskHubMutationPhase
	Error          string
}

// The indirections make actor/key failure paths deterministic in unit tests
// while production always derives both values locally at dialog-open time.
var (
	resolveTaskHubLocalActor = taskHubLocalOSActor
	newTaskHubIdempotencyKey = store.NewUUIDv7
)

func newTaskHubMutationOverlay(action TaskHubAction, target TaskHubTarget, preview TaskHubPlanPreview) (*TaskHubMutationOverlay, error) {
	actor, err := resolveTaskHubLocalActor()
	if err != nil {
		return nil, err
	}
	key, err := newTaskHubIdempotencyKey()
	if err != nil {
		return nil, fmt.Errorf("allocate Task Hub idempotency key: %w", err)
	}
	if err := store.ValidateUUIDv7(key); err != nil {
		return nil, fmt.Errorf("allocate Task Hub idempotency key: %w", err)
	}
	overlay := &TaskHubMutationOverlay{
		Action:         action,
		Target:         target,
		Preview:        preview.Clone(),
		Actor:          actor,
		IdempotencyKey: key,
		ValueInputs:    make(map[string]textinput.Model),
		Phase:          taskHubMutationReady,
	}
	overlay.initializeInputs()
	return overlay, nil
}

func newTaskHubRunControlMutationOverlay(action TaskHubRunControlAction, target TaskHubTarget, expected TaskHubControlCheckpoint, preview TaskHubPlanPreview) (*TaskHubMutationOverlay, error) {
	overlay, err := newTaskHubMutationOverlay(TaskHubActionOpenRunControl, target, preview)
	if err != nil {
		return nil, err
	}
	overlay.ControlAction = action
	overlay.Expected = expected
	return overlay, nil
}

func (overlay *TaskHubMutationOverlay) initializeInputs() {
	if overlay == nil {
		return
	}
	overlay.ReasonInput = newTaskHubMutationInput("操作原因", "说明这次操作的原因")
	overlay.FieldOrder = []string{"reason"}
	if overlay.Action == TaskHubActionPackageRevision {
		input := newTaskHubMutationInput("本地 package 版本", "例如 v1.0.0")
		overlay.ValueInputs[taskHubPackageVersionField] = input
		overlay.FieldOrder = append(overlay.FieldOrder, taskHubPackageVersionField)
	}
	overlay.focusInput(0)
}

func newTaskHubMutationInput(_ string, placeholder string) textinput.Model {
	input := textinput.New()
	input.Prompt = ""
	input.Placeholder = placeholder
	input.CharLimit = 512
	input.Width = 52
	return input
}

func taskHubLocalOSActor() (string, error) {
	if current, err := user.Current(); err == nil {
		if actor := strings.TrimSpace(current.Username); actor != "" {
			return actor, nil
		}
		if actor := strings.TrimSpace(current.Name); actor != "" {
			return actor, nil
		}
	}
	for _, key := range []string{"USER", "LOGNAME", "USERNAME"} {
		if actor := strings.TrimSpace(os.Getenv(key)); actor != "" {
			return actor, nil
		}
	}
	return "", fmt.Errorf("cannot derive local OS actor")
}

func (overlay *TaskHubMutationOverlay) Clone() *TaskHubMutationOverlay {
	if overlay == nil {
		return nil
	}
	clone := *overlay
	clone.Preview = overlay.Preview.Clone()
	clone.FieldOrder = append([]string(nil), overlay.FieldOrder...)
	clone.ValueInputs = make(map[string]textinput.Model, len(overlay.ValueInputs))
	for key, input := range overlay.ValueInputs {
		clone.ValueInputs[key] = input
	}
	return &clone
}

func (overlay *TaskHubMutationOverlay) Init() tea.Cmd                { return nil }
func (overlay *TaskHubMutationOverlay) Focus()                       {}
func (overlay *TaskHubMutationOverlay) Blur()                        {}
func (overlay *TaskHubMutationOverlay) ZIndex() int                  { return 120 }
func (overlay *TaskHubMutationOverlay) InterceptsAllKeys() bool      { return true }
func (overlay *TaskHubMutationOverlay) HandleKey(tea.KeyMsg) tea.Cmd { return nil }
func (overlay *TaskHubMutationOverlay) Update(tea.Msg) (bool, tea.Cmd) {
	return true, nil
}

func (overlay *TaskHubMutationOverlay) focusInput(index int) {
	if overlay == nil || len(overlay.FieldOrder) == 0 {
		return
	}
	overlay.FocusedField = (index + len(overlay.FieldOrder)) % len(overlay.FieldOrder)
	for current, field := range overlay.FieldOrder {
		if field == "reason" {
			if current == overlay.FocusedField {
				overlay.ReasonInput.Focus()
			} else {
				overlay.ReasonInput.Blur()
			}
			continue
		}
		input := overlay.ValueInputs[field]
		if current == overlay.FocusedField {
			input.Focus()
		} else {
			input.Blur()
		}
		overlay.ValueInputs[field] = input
	}
}

func (overlay *TaskHubMutationOverlay) updateFocusedInput(msg tea.KeyMsg) tea.Cmd {
	if overlay == nil || len(overlay.FieldOrder) == 0 {
		return nil
	}
	field := overlay.FieldOrder[overlay.FocusedField]
	if field == "reason" {
		var command tea.Cmd
		overlay.ReasonInput, command = overlay.ReasonInput.Update(msg)
		return command
	}
	input := overlay.ValueInputs[field]
	updated, command := input.Update(msg)
	overlay.ValueInputs[field] = updated
	return command
}

func (overlay *TaskHubMutationOverlay) request() TaskHubMutationRequest {
	if overlay == nil {
		return TaskHubMutationRequest{}
	}
	actor := strings.TrimSpace(overlay.Actor)
	reason := strings.TrimSpace(overlay.ReasonInput.Value())
	if overlay.isFrozen() && strings.TrimSpace(overlay.FrozenActor) != "" && strings.TrimSpace(overlay.FrozenReason) != "" {
		actor = overlay.FrozenActor
		reason = overlay.FrozenReason
	}
	values := make(map[string]string, len(overlay.ValueInputs))
	for key, input := range overlay.ValueInputs {
		values[key] = strings.TrimSpace(input.Value())
	}
	return TaskHubMutationRequest{
		Action:         overlay.Action,
		Target:         overlay.Target,
		PlanID:         strings.TrimSpace(overlay.Preview.PlanID),
		Actor:          actor,
		Reason:         reason,
		IdempotencyKey: strings.TrimSpace(overlay.IdempotencyKey),
		Values:         values,
	}
}

func (overlay *TaskHubMutationOverlay) isFrozen() bool {
	return overlay != nil && strings.TrimSpace(overlay.Preview.PlanID) != ""
}

func (overlay *TaskHubMutationOverlay) lockFrozenProvenance(actor, reason string) error {
	if overlay == nil || !overlay.isFrozen() {
		return fmt.Errorf("cannot lock provenance before a plan is frozen")
	}
	actor = strings.TrimSpace(actor)
	reason = strings.TrimSpace(reason)
	if actor == "" || reason == "" {
		return fmt.Errorf("冻结计划缺少权威操作员或操作原因")
	}
	overlay.FrozenActor = actor
	overlay.FrozenReason = reason
	overlay.Actor = actor
	overlay.ReasonInput.SetValue(reason)
	overlay.ReasonInput.Blur()
	return nil
}

func (overlay *TaskHubMutationOverlay) validate() error {
	request := overlay.request()
	if overlay.isFrozen() && (strings.TrimSpace(overlay.FrozenActor) == "" || strings.TrimSpace(overlay.FrozenReason) == "") {
		return fmt.Errorf("冻结计划缺少不可变操作来源")
	}
	if request.Actor == "" {
		return fmt.Errorf("无法确定本机操作员")
	}
	if request.Reason == "" {
		return fmt.Errorf("必须填写操作原因")
	}
	if err := store.ValidateUUIDv7(request.IdempotencyKey); err != nil {
		return fmt.Errorf("确认幂等键无效: %w", err)
	}
	if overlay.Action == TaskHubActionPackageRevision && strings.TrimSpace(request.Values[taskHubPackageVersionField]) == "" {
		return fmt.Errorf("必须填写本地 package 版本")
	}
	return nil
}

func (overlay *TaskHubMutationOverlay) isRunControl() bool {
	return overlay != nil && overlay.ControlAction != ""
}

func (overlay *TaskHubMutationOverlay) actionLabel() string {
	if overlay != nil && overlay.isRunControl() {
		return taskHubRunControlActionLabel(overlay.ControlAction)
	}
	if overlay == nil {
		return "生命周期操作"
	}
	return taskHubActionLabel(overlay.Action)
}

func (overlay *TaskHubMutationOverlay) View(width, height int) string {
	if overlay == nil {
		return ""
	}
	contentWidth := styleContentWidth(boundedPanelWidth(width, 42, 82), panelStyle)
	rows := []string{
		sectionStyle.Render("确认 " + overlay.actionLabel()),
		"",
		clipDisplay(overlay.Preview.Title, contentWidth),
		clipDisplay(overlay.Preview.Summary, contentWidth),
	}
	rows = append(rows, taskHubPlanExplanationRows(overlay.Preview, contentWidth)...)
	rows = append(rows, "")
	if overlay.isFrozen() {
		rows = append(rows,
			"冻结操作员："+redactSingleLineUI(overlay.FrozenActor),
			"冻结操作原因："+redactSingleLineUI(overlay.FrozenReason),
		)
	} else {
		rows = append(rows, "操作员："+redactSingleLineUI(overlay.Actor))
	}
	if overlay.Action == TaskHubActionPackageRevision {
		rows = append(rows, "本地 package 版本："+overlay.ValueInputs[taskHubPackageVersionField].View())
	}
	if !overlay.isFrozen() {
		rows = append(rows, "操作原因："+overlay.ReasonInput.View())
	}
	if overlay.Error != "" {
		rows = append(rows, failStyle.Render(clipDisplay(redactSingleLineUI(overlay.Error), contentWidth)))
	}
	if overlay.Phase == taskHubMutationPreparing || overlay.Phase == taskHubMutationExecuting {
		rows = append(rows, subtleStyle.Render("正在提交确认..."))
	}
	rows = append(rows, "")
	if overlay.Preview.PlanID != "" {
		rows = append(rows, subtleStyle.Render("已冻结计划："+clipDisplay(overlay.Preview.PlanID, maxInt(12, contentWidth-12))))
	}
	if overlay.isFrozen() {
		rows = append(rows, subtleStyle.Render("[Enter] 提交冻结计划  [Esc] 取消"))
	} else {
		rows = append(rows, subtleStyle.Render("[Tab] 切换字段  [Enter] 确认提交  [Esc] 取消"))
	}
	rows = clipOverlayRows(rows, contentWidth)
	rows = fitOverlayRows(rows, height, len(rows)-3)
	body := lipgloss.JoinVertical(lipgloss.Left, rows...)
	boxWidth := boundedPanelWidth(width, 42, 82)
	box := panelStyle.Width(boxWidth).Align(lipgloss.Left).Render(body)
	return lipgloss.Place(maxInt(1, width), maxInt(1, height), lipgloss.Center, lipgloss.Center, box)
}
