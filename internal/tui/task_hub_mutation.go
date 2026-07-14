package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

const (
	taskHubPackageVersionField       = "release_version"
	taskHubTaskSlugField             = "slug"
	taskHubTaskTitleField            = "title"
	taskHubTaskMetadataJSONField     = "metadata_json"
	taskHubTaskSourceRepoField       = "source_repo"
	taskHubTaskSourceCommitField     = "source_commit"
	taskHubImportSourcePathField     = "source_path"
	taskHubImportProposalDigestField = "proposal_digest"
	taskHubImportChangeSummaryField  = "change_summary"
	taskHubRestoreStateField         = "restore_state"
	taskHubExecutionProfilePathField = "profile_path"
	taskHubExecutionSpecPathField    = "execution_spec_path"
	taskHubRunTriggerField           = "trigger"
	taskHubUnifiedDiffField          = "unified_diff"
)

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
	Action            TaskHubAction
	ControlAction     TaskHubRunControlAction
	Target            TaskHubTarget
	Expected          TaskHubControlCheckpoint
	ExpectedLifecycle TaskHubLifecycleCheckpoint
	Preview           TaskHubPlanPreview
	Actor             string
	IdempotencyKey    string
	FrozenActor       string
	FrozenReason      string
	ReasonInput       textinput.Model
	ValueInputs       map[string]textinput.Model
	TextAreaInputs    map[string]textarea.Model
	FieldOrder        []string
	FocusedField      int
	Phase             taskHubMutationPhase
	Error             string
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
		Action:            action,
		Target:            target,
		Preview:           preview.Clone(),
		ExpectedLifecycle: preview.Expected,
		Actor:             actor,
		IdempotencyKey:    key,
		ValueInputs:       make(map[string]textinput.Model),
		TextAreaInputs:    make(map[string]textarea.Model),
		Phase:             taskHubMutationReady,
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
	for _, field := range taskHubMutationFields(overlay.Action) {
		if field.multiline {
			overlay.addTextAreaInput(field.key, field.placeholder)
			continue
		}
		overlay.addValueInput(field.key, field.placeholder)
	}
	overlay.focusInput(0)
}

type taskHubMutationField struct {
	key         string
	label       string
	placeholder string
	multiline   bool
}

func taskHubMutationFields(action TaskHubAction) []taskHubMutationField {
	switch action {
	case TaskHubActionNewTask:
		return []taskHubMutationField{
			{key: taskHubTaskSlugField, label: "Task 标识", placeholder: "例如 harbor-algorithms"},
			{key: taskHubTaskTitleField, label: "Task 标题", placeholder: "Task 的可读标题"},
			{key: taskHubTaskMetadataJSONField, label: "元数据 JSON（可选）", placeholder: "例如 {\"tags\":[\"go\"]}"},
			{key: taskHubTaskSourceRepoField, label: "来源仓库（可选）", placeholder: "https://..."},
			{key: taskHubTaskSourceCommitField, label: "来源提交（可选）", placeholder: "commit SHA"},
		}
	case TaskHubActionImportTask:
		return []taskHubMutationField{
			{key: taskHubTaskSlugField, label: "Task 标识", placeholder: "例如 harbor-algorithms"},
			{key: taskHubTaskTitleField, label: "Task 标题", placeholder: "Task 的可读标题"},
			{key: taskHubImportSourcePathField, label: "受管快照目录", placeholder: "/absolute/path/to/task-snapshot"},
			{key: taskHubTaskMetadataJSONField, label: "元数据 JSON（可选）", placeholder: "例如 {\"tags\":[\"go\"]}"},
			{key: taskHubTaskSourceRepoField, label: "来源仓库（可选）", placeholder: "https://..."},
			{key: taskHubTaskSourceCommitField, label: "来源提交（可选）", placeholder: "commit SHA"},
			{key: taskHubImportProposalDigestField, label: "提案摘要（可选）", placeholder: "sha256:..."},
			{key: taskHubImportChangeSummaryField, label: "变更说明（可选）", placeholder: "导入说明"},
		}
	case TaskHubActionForkTask:
		return []taskHubMutationField{
			{key: taskHubTaskSlugField, label: "新 Task 标识", placeholder: "例如 harbor-algorithms-fork"},
			{key: taskHubTaskTitleField, label: "新 Task 标题", placeholder: "Fork 的可读标题"},
			{key: taskHubTaskMetadataJSONField, label: "元数据 JSON（可选）", placeholder: "例如 {\"fork\":true}"},
		}
	case TaskHubActionRestoreTask:
		return []taskHubMutationField{{key: taskHubRestoreStateField, label: "恢复后的状态", placeholder: "draft | ready | published | archived"}}
	case TaskHubActionStartRun:
		return []taskHubMutationField{
			{key: taskHubExecutionProfilePathField, label: "Execution profile JSON", placeholder: "/absolute/path/to/profile.json"},
			{key: taskHubExecutionSpecPathField, label: "Execution spec JSON", placeholder: "/absolute/path/to/execution-spec.json"},
			{key: taskHubRunTriggerField, label: "运行触发说明", placeholder: "例如 task_hub"},
		}
	case TaskHubActionEditTask:
		return []taskHubMutationField{{key: taskHubUnifiedDiffField, label: "Unified diff", placeholder: "--- a/path\n+++ b/path\n@@ ...", multiline: true}}
	case TaskHubActionPackageRevision:
		return []taskHubMutationField{{key: taskHubPackageVersionField, label: "本地 package 版本", placeholder: "例如 v1.0.0"}}
	default:
		return nil
	}
}

func taskHubMutationFieldLabel(field string) string {
	if field == "reason" {
		return "操作原因"
	}
	for _, action := range []TaskHubAction{
		TaskHubActionNewTask, TaskHubActionImportTask, TaskHubActionForkTask, TaskHubActionRestoreTask,
		TaskHubActionStartRun, TaskHubActionEditTask, TaskHubActionPackageRevision,
	} {
		for _, candidate := range taskHubMutationFields(action) {
			if candidate.key == field {
				return candidate.label
			}
		}
	}
	return field
}

func (overlay *TaskHubMutationOverlay) addValueInput(field, placeholder string) {
	if overlay == nil {
		return
	}
	input := newTaskHubMutationInput(taskHubMutationFieldLabel(field), placeholder)
	if field == taskHubTaskMetadataJSONField || field == taskHubUnifiedDiffField {
		input.CharLimit = 4096
	}
	overlay.ValueInputs[field] = input
	overlay.FieldOrder = append(overlay.FieldOrder, field)
}

func (overlay *TaskHubMutationOverlay) addTextAreaInput(field, placeholder string) {
	if overlay == nil {
		return
	}
	input := textarea.New()
	input.Prompt = ""
	input.Placeholder = placeholder
	input.CharLimit = 65536
	input.SetWidth(52)
	input.SetHeight(7)
	overlay.TextAreaInputs[field] = input
	overlay.FieldOrder = append(overlay.FieldOrder, field)
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
	clone.TextAreaInputs = make(map[string]textarea.Model, len(overlay.TextAreaInputs))
	for key, input := range overlay.TextAreaInputs {
		clone.TextAreaInputs[key] = input
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
		if input, found := overlay.TextAreaInputs[field]; found {
			if current == overlay.FocusedField {
				input.Focus()
			} else {
				input.Blur()
			}
			overlay.TextAreaInputs[field] = input
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
	if input, found := overlay.TextAreaInputs[field]; found {
		updated, command := input.Update(msg)
		overlay.TextAreaInputs[field] = updated
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
	values := make(map[string]string, len(overlay.ValueInputs)+len(overlay.TextAreaInputs))
	for key, input := range overlay.ValueInputs {
		values[key] = strings.TrimSpace(input.Value())
	}
	for key, input := range overlay.TextAreaInputs {
		values[key] = strings.TrimSpace(input.Value())
	}
	return TaskHubMutationRequest{
		Action:         overlay.Action,
		Target:         overlay.Target,
		PlanID:         strings.TrimSpace(overlay.Preview.PlanID),
		Actor:          actor,
		Reason:         reason,
		IdempotencyKey: strings.TrimSpace(overlay.IdempotencyKey),
		Expected:       overlay.ExpectedLifecycle,
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
	return validateTaskHubMutationValues(request)
}

func validateTaskHubMutationValues(request TaskHubMutationRequest) error {
	for _, field := range taskHubRequiredMutationFields(request.Action) {
		if strings.TrimSpace(request.Values[field]) == "" {
			return fmt.Errorf("必须填写%s", taskHubMutationFieldLabel(field))
		}
	}
	if metadata := strings.TrimSpace(request.Values[taskHubTaskMetadataJSONField]); metadata != "" && !json.Valid([]byte(metadata)) {
		return fmt.Errorf("元数据 JSON 必须是有效 JSON")
	}
	if request.Action == TaskHubActionRestoreTask && !taskHubRestoreStateValid(request.Values[taskHubRestoreStateField]) {
		return fmt.Errorf("恢复后的状态必须是 draft、ready、published 或 archived")
	}
	return nil
}

func taskHubRequiredMutationFields(action TaskHubAction) []string {
	switch action {
	case TaskHubActionNewTask:
		return []string{taskHubTaskSlugField, taskHubTaskTitleField}
	case TaskHubActionImportTask:
		return []string{taskHubTaskSlugField, taskHubTaskTitleField, taskHubImportSourcePathField}
	case TaskHubActionForkTask:
		return []string{taskHubTaskSlugField, taskHubTaskTitleField}
	case TaskHubActionRestoreTask:
		return []string{taskHubRestoreStateField}
	case TaskHubActionStartRun:
		return []string{taskHubExecutionProfilePathField, taskHubExecutionSpecPathField, taskHubRunTriggerField}
	case TaskHubActionEditTask:
		return []string{taskHubUnifiedDiffField}
	case TaskHubActionPackageRevision:
		return []string{taskHubPackageVersionField}
	default:
		return nil
	}
}

func taskHubRestoreStateValid(value string) bool {
	switch strings.TrimSpace(value) {
	case "draft", "ready", "published", "archived":
		return true
	default:
		return false
	}
}

func (overlay *TaskHubMutationOverlay) isRunControl() bool {
	return overlay != nil && overlay.ControlAction != ""
}

func (overlay *TaskHubMutationOverlay) focusedInputIsMultiline() bool {
	if overlay == nil || len(overlay.FieldOrder) == 0 || overlay.FocusedField < 0 || overlay.FocusedField >= len(overlay.FieldOrder) {
		return false
	}
	_, found := overlay.TextAreaInputs[overlay.FieldOrder[overlay.FocusedField]]
	return found
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
	focusedRow := len(rows)
	if !overlay.isFrozen() {
		for index, field := range overlay.FieldOrder {
			if index == overlay.FocusedField {
				focusedRow = len(rows)
			}
			if _, multiline := overlay.TextAreaInputs[field]; multiline {
				rows = append(rows, taskHubMutationFieldLabel(field)+"：")
				rows = append(rows, strings.Split(overlay.fieldView(field, contentWidth), "\n")...)
				continue
			}
			rows = append(rows, taskHubMutationFieldLabel(field)+"："+overlay.fieldView(field, contentWidth))
		}
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
	} else if overlay.hasMultilineInput() {
		rows = append(rows, subtleStyle.Render("[Tab] 切换字段  [Enter] 在 diff 中换行  [Ctrl+S] 确认提交  [Esc] 取消"))
	} else {
		rows = append(rows, subtleStyle.Render("[Tab] 切换字段  [Enter] 确认提交  [Esc] 取消"))
	}
	rows = clipOverlayRows(rows, contentWidth)
	rows = fitOverlayRows(rows, height, focusedRow)
	body := lipgloss.JoinVertical(lipgloss.Left, rows...)
	boxWidth := boundedPanelWidth(width, 42, 82)
	box := panelStyle.Width(boxWidth).Align(lipgloss.Left).Render(body)
	return lipgloss.Place(maxInt(1, width), maxInt(1, height), lipgloss.Center, lipgloss.Center, box)
}

func (overlay *TaskHubMutationOverlay) fieldView(field string, contentWidth int) string {
	if field == "reason" {
		return textInputView(overlay.ReasonInput, maxInt(12, contentWidth-18))
	}
	if input, found := overlay.TextAreaInputs[field]; found {
		input.SetWidth(maxInt(12, contentWidth-2))
		return input.View()
	}
	return textInputView(overlay.ValueInputs[field], maxInt(12, contentWidth-18))
}

func (overlay *TaskHubMutationOverlay) hasMultilineInput() bool {
	return overlay != nil && len(overlay.TextAreaInputs) > 0
}
