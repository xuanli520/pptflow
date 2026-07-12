package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

type confirmAction int

const (
	confirmNone confirmAction = iota
	confirmApprove
	confirmReject
	confirmCancelRun
	confirmQuit
	confirmEditArtifact
	confirmDeleteWorkspace
)

type ConfirmDialog struct {
	Title, Message string
	Action         confirmAction
	FocusedYes     bool
	Gate           *domain.GateRequest
	Path           string
}

func newConfirmDialog(action confirmAction, title, message string) *ConfirmDialog {
	return &ConfirmDialog{Action: action, Title: title, Message: message, FocusedYes: true}
}

func (d *ConfirmDialog) Init() tea.Cmd                { return nil }
func (d *ConfirmDialog) Focus()                       {}
func (d *ConfirmDialog) Blur()                        {}
func (d *ConfirmDialog) ZIndex() int                  { return 100 }
func (d *ConfirmDialog) InterceptsAllKeys() bool      { return true }
func (d *ConfirmDialog) HandleKey(tea.KeyMsg) tea.Cmd { return nil }
func (d *ConfirmDialog) Update(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return false, nil
	}
	switch key.String() {
	case "left", "right", "tab", "shift+tab":
		d.FocusedYes = !d.FocusedYes
	}
	return true, nil
}
func (d *ConfirmDialog) View(width, height int) string {
	if d == nil {
		return ""
	}
	yes, no := "  是  ", "  否  "
	if d.FocusedYes {
		yes = selectedStyle.Render(yes)
	} else {
		no = selectedStyle.Render(no)
	}
	body := lipgloss.JoinVertical(lipgloss.Center,
		sectionStyle.Render(d.Title), "", redactUI(d.Message), "", yes+"    "+no, "",
		subtleStyle.Render("[←/→] 选择  [Enter] 确认  [Esc] 取消"),
	)
	boxWidth := clampInt(width-8, 32, 68)
	box := panelStyle.Width(boxWidth).Align(lipgloss.Center).Render(body)
	return lipgloss.Place(maxInt(width, boxWidth), maxInt(height, 8), lipgloss.Center, lipgloss.Center, box)
}

type helpOverlay struct{ view viewMode }

func (h *helpOverlay) Init() tea.Cmd                  { return nil }
func (h *helpOverlay) Update(tea.Msg) (bool, tea.Cmd) { return true, nil }
func (h *helpOverlay) Focus()                         {}
func (h *helpOverlay) Blur()                          {}
func (h *helpOverlay) HandleKey(tea.KeyMsg) tea.Cmd   { return nil }
func (h *helpOverlay) ZIndex() int                    { return 90 }
func (h *helpOverlay) InterceptsAllKeys() bool        { return true }
func (h *helpOverlay) View(width, height int) string {
	lines := []string{
		sectionStyle.Render("快捷键帮助"), "",
		"Ctrl+O 总览    Ctrl+G 审查    Ctrl+D 详情",
		"Ctrl+L 日志    Ctrl+E 完成    Ctrl+X 取消运行",
		"Tab/Shift+Tab 切换当前项目/面板  / 搜索或过滤",
		"Esc 返回        q/Ctrl+Q 退出  ? 关闭帮助",
	}
	switch h.view {
	case viewHub:
		lines = append(lines, "", "工作区：↑↓/j k 选择  Enter 打开  Ctrl+N 新建  Ctrl+R 重跑  Del 删除  s/S 排序  / 搜索")
	case viewGate:
		lines = append(lines, "", "审查：↑↓/j k 滚动  PgUp/PgDn 翻页  Home/End 首尾  a 批准  r 拒绝  v 修订/刷新  Ctrl+N 备注  e 编辑工件")
	case viewLogs:
		lines = append(lines, "", "日志：↑↓/j k 滚动  PgUp/PgDn 翻页  Home/End 首尾  t 跟踪")
	case viewStart:
		lines = append(lines, "", "表单：↑↓/Tab 字段  Space 开关  Ctrl+Space 路径补全  Enter 下一步/启动  F1..F4 高级分组  Ctrl+Q 退出")
	}
	lines = append(lines, "", subtleStyle.Render("按 ?、Esc 或 q 关闭"))
	box := panelStyle.Width(clampInt(width-8, 40, 82)).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(maxInt(width, 40), maxInt(height, 10), lipgloss.Center, lipgloss.Center, box)
}
