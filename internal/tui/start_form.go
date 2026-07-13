package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

type startGroup int

const (
	startGroupBasic startGroup = iota
	startGroupHarbor
	startGroupQuality
	startGroupPackage
	startGroupAgent
)

type startStep int

const (
	startStepBasic startStep = iota
	startStepAdvanced
)

func startGroupName(group startGroup) string {
	switch group {
	case startGroupBasic:
		return "基本配置"
	case startGroupHarbor:
		return "Harbor 配置"
	case startGroupQuality:
		return "质量与相似度"
	case startGroupPackage:
		return "结果与打包"
	default:
		return "任务与代理"
	}
}

func advancedGroups() []startGroup {
	return []startGroup{startGroupHarbor, startGroupQuality, startGroupPackage, startGroupAgent}
}

func (m *model) selectAdvancedGroup(delta int) {
	groups := advancedGroups()
	index := 0
	for i, group := range groups {
		if group == m.selectedStartGroup {
			index = i
			break
		}
	}
	m.selectedStartGroup = groups[(index+delta+len(groups))%len(groups)]
	for _, group := range groups {
		m.startCollapsed[group] = group != m.selectedStartGroup
	}
	fields := m.activeStartFields()
	if len(fields) > 0 {
		m.startField = fields[0]
		m.focusStartInput(m.startField)
	}
}

func (m *model) selectAdvancedGroupByNumber(number int) bool {
	groups := advancedGroups()
	if number < 1 || number > len(groups) {
		return false
	}
	m.selectedStartGroup = groups[number-1]
	for _, group := range groups {
		m.startCollapsed[group] = group != m.selectedStartGroup
	}
	fields := m.activeStartFields()
	if len(fields) > 0 {
		m.startField = fields[0]
		m.focusStartInput(m.startField)
	}
	return true
}
func groupForStartField(field startField) startGroup {
	switch field {
	case startFieldHarborAgent, startFieldQwenModel, startFieldOpusModel, startFieldQwenHarborBaseURL, startFieldOpusHarborBaseURL, startFieldHarborTimeout, startFieldHarborSetupTimeout, startFieldHarborPreflight, startFieldHarborConcurrency, startFieldHarborAttempts, startFieldHarborInfraRetries, startFieldRunHarbor:
		return startGroupHarbor
	case startFieldVerifyDocker, startFieldQualityCheck, startFieldQualityAgent, startFieldSimilarityCheck, startFieldSimilarityGitHub, startFieldSimilarityThreshold, startFieldHistoryDirs, startFieldTB3Dirs:
		return startGroupQuality
	case startFieldTestsAnalysis, startFieldQwenResult, startFieldOpusResult, startFieldQwenScreenshot, startFieldOpusScreenshot, startFieldPackage, startFieldOutput:
		return startGroupPackage
	case startFieldTaskName, startFieldCodeLang, startFieldTaskType, startFieldApplication, startFieldAHT, startFieldDescription, startFieldZeroToOne, startFieldCodexModel, startFieldCodexReasoning, startFieldCodexPath, startFieldAgentTimeout:
		return startGroupAgent
	default:
		return startGroupBasic
	}
}

func allTextStartFields() []startField {
	var out []startField
	for field := startFieldMode; field <= startFieldAgentTimeout; field++ {
		if isTextStartField(field) {
			out = append(out, field)
		}
	}
	return out
}
func isTextStartField(field startField) bool {
	return field != startFieldMode && !isBoolStartField(field)
}
func isBoolStartField(field startField) bool {
	switch field {
	case startFieldVerifyDocker, startFieldQualityCheck, startFieldQualityAgent, startFieldSimilarityCheck, startFieldSimilarityGitHub, startFieldRunHarbor, startFieldHarborPreflight, startFieldPackage, startFieldZeroToOne:
		return true
	}
	return false
}
func isPathStartField(field startField) bool {
	switch field {
	case startFieldTaskDir, startFieldWorkspace, startFieldTaskOutput, startFieldTestsAnalysis, startFieldQwenResult, startFieldOpusResult, startFieldQwenScreenshot, startFieldOpusScreenshot, startFieldHistoryDirs, startFieldTB3Dirs, startFieldOutput, startFieldCodexPath:
		return true
	}
	return false
}

func startFieldValue(opts app.RunnerOptions, field startField) string {
	switch field {
	case startFieldTaskDir:
		return opts.TaskDir
	case startFieldRepoURL:
		return opts.RepoURL
	case startFieldCommit:
		return opts.Commit
	case startFieldWorkspace:
		return opts.Workspace
	case startFieldTaskOutput:
		return opts.TaskOutputDir
	case startFieldTestsAnalysis:
		return opts.TestsAnalysis
	case startFieldQwenResult:
		return opts.QwenResult
	case startFieldOpusResult:
		return opts.OpusResult
	case startFieldQwenScreenshot:
		return opts.QwenScreenshot
	case startFieldOpusScreenshot:
		return opts.OpusScreenshot
	case startFieldSimilarityThreshold:
		return formatFloatInput(opts.SimilarityThreshold)
	case startFieldHistoryDirs:
		return joinStartList(opts.SimilarityHistoryDirs)
	case startFieldTB3Dirs:
		return joinStartList(opts.SimilarityTB3Dirs)
	case startFieldOutput:
		return opts.OutputDir
	case startFieldHarborAgent:
		return opts.HarborAgent
	case startFieldQwenModel:
		return opts.QwenModel
	case startFieldOpusModel:
		return opts.OpusModel
	case startFieldQwenHarborBaseURL:
		return opts.QwenHarborBaseURL
	case startFieldOpusHarborBaseURL:
		return opts.OpusHarborBaseURL
	case startFieldHarborTimeout:
		return formatIntInput(opts.HarborTimeout)
	case startFieldHarborSetupTimeout:
		return formatIntInput(opts.HarborSetupTimeout)
	case startFieldHarborConcurrency:
		return formatIntInput(opts.HarborConcurrency)
	case startFieldHarborAttempts:
		return formatIntInput(opts.HarborAttempts)
	case startFieldHarborInfraRetries:
		return formatIntInput(opts.HarborInfraRetries)
	case startFieldTaskName:
		return opts.TaskName
	case startFieldCodeLang:
		return opts.CodeLang
	case startFieldTaskType:
		return opts.TaskType
	case startFieldApplication:
		return opts.Application
	case startFieldAHT:
		return opts.AHT
	case startFieldDescription:
		return opts.Description
	case startFieldCodexModel:
		return opts.Model
	case startFieldCodexReasoning:
		return opts.Reasoning
	case startFieldCodexPath:
		return opts.CodexPath
	case startFieldAgentTimeout:
		return formatIntInput(opts.AgentTimeout)
	default:
		return ""
	}
}

func (m *model) syncStartField(field startField, value string) {
	m.err = nil
	m.dirtyStartInputs[field] = true
	switch field {
	case startFieldTaskDir:
		m.opts.TaskDir = value
	case startFieldRepoURL:
		m.opts.RepoURL = value
	case startFieldCommit:
		m.opts.Commit = value
	case startFieldWorkspace:
		m.opts.Workspace = value
	case startFieldTaskOutput:
		m.opts.TaskOutputDir = value
	case startFieldTestsAnalysis:
		m.opts.TestsAnalysis = value
	case startFieldQwenResult:
		m.opts.QwenResult = value
	case startFieldOpusResult:
		m.opts.OpusResult = value
	case startFieldQwenScreenshot:
		m.opts.QwenScreenshot = value
	case startFieldOpusScreenshot:
		m.opts.OpusScreenshot = value
	case startFieldSimilarityThreshold:
		m.setStartFloat(field, value)
	case startFieldHistoryDirs:
		m.opts.SimilarityHistoryDirs = rawStartList(value)
	case startFieldTB3Dirs:
		m.opts.SimilarityTB3Dirs = rawStartList(value)
	case startFieldOutput:
		m.opts.OutputDir = value
	case startFieldHarborAgent:
		m.opts.HarborAgent = value
	case startFieldQwenModel:
		m.opts.QwenModel = value
	case startFieldOpusModel:
		m.opts.OpusModel = value
	case startFieldQwenHarborBaseURL:
		m.opts.QwenHarborBaseURL = value
	case startFieldOpusHarborBaseURL:
		m.opts.OpusHarborBaseURL = value
	case startFieldHarborTimeout, startFieldHarborSetupTimeout, startFieldHarborConcurrency, startFieldHarborAttempts, startFieldHarborInfraRetries, startFieldAgentTimeout:
		m.setStartInt(field, value)
	case startFieldTaskName:
		m.opts.TaskName = value
	case startFieldCodeLang:
		m.opts.CodeLang = value
	case startFieldTaskType:
		m.opts.TaskType = value
	case startFieldApplication:
		m.opts.Application = value
	case startFieldAHT:
		m.opts.AHT = value
	case startFieldDescription:
		m.opts.Description = value
	case startFieldCodexModel:
		m.opts.Model = value
	case startFieldCodexReasoning:
		m.opts.Reasoning = value
	case startFieldCodexPath:
		m.opts.CodexPath = value
	}
}

func (m *model) focusStartInput(field startField) tea.Cmd {
	var focusCmd tea.Cmd
	for id, ti := range m.startInputs {
		ti.Blur()
		if id == field {
			if !m.dirtyStartInputs[id] {
				ti.SetValue(startFieldValue(m.opts, id))
			}
			focusCmd = ti.Focus()
		}
		m.startInputs[id] = ti
	}
	return focusCmd
}
func (m *model) updateFocusedStartInput(msg tea.KeyMsg) tea.Cmd {
	field := m.startField
	if !isTextStartField(field) {
		return nil
	}
	ti, ok := m.startInputs[field]
	if !ok {
		ti = textinput.New()
		ti.Prompt = ""
		ti.SetValue(startFieldValue(m.opts, field))
		ti.Focus()
	}
	if !m.dirtyStartInputs[field] && ti.Value() != startFieldValue(m.opts, field) {
		ti.SetValue(startFieldValue(m.opts, field))
	}
	var cmd tea.Cmd
	ti, cmd = ti.Update(msg)
	m.startInputs[field] = ti
	m.syncStartField(field, ti.Value())
	return cmd
}

func (m *model) toggleStartGroup(key string) bool {
	if m.startStep != startStepAdvanced {
		return false
	}
	if !strings.HasPrefix(key, "f") {
		return false
	}
	index, err := strconv.Atoi(strings.TrimPrefix(key, "f"))
	if err != nil || index < 1 || index > len(advancedGroups()) {
		return false
	}
	return m.selectAdvancedGroupByNumber(index)
}

func completePath(value string) (string, []string) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "." + string(filepath.Separator)
	}
	expanded := value
	if strings.HasPrefix(expanded, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			expanded = filepath.Join(home, strings.TrimPrefix(expanded, "~/"))
		}
	}
	dir, base := filepath.Split(expanded)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return value, nil
	}
	var matches []string
	for _, entry := range entries {
		if strings.HasPrefix(strings.ToLower(entry.Name()), strings.ToLower(base)) {
			candidate := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				candidate += string(filepath.Separator)
			}
			matches = append(matches, candidate)
		}
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return value, nil
	}
	if len(matches) == 1 {
		return matches[0], matches
	}
	common := matches[0]
	for _, candidate := range matches[1:] {
		common = commonRunePrefix(common, candidate)
	}
	if len(common) > len(expanded) {
		return common, matches
	}
	return value, matches
}

func commonRunePrefix(a, b string) string {
	ar, br := []rune(a), []rune(b)
	limit := minInt(len(ar), len(br))
	i := 0
	for i < limit && ar[i] == br[i] {
		i++
	}
	return string(ar[:i])
}

func (m *model) completeFocusedPath() tea.Cmd {
	if !isPathStartField(m.startField) {
		return m.showToast("当前字段不是路径字段", toastInfo)
	}
	ti := m.startInputs[m.startField]
	value := ti.Value()
	prefix := ""
	if m.startField == startFieldHistoryDirs || m.startField == startFieldTB3Dirs {
		if split := strings.LastIndex(value, ","); split >= 0 {
			prefix = value[:split+1]
			value = strings.TrimSpace(value[split+1:])
		}
	}
	completed, matches := completePath(value)
	if prefix != "" {
		completed = prefix + completed
		for index := range matches {
			matches[index] = prefix + matches[index]
		}
	}
	m.pathSuggestions = matches
	if completed != ti.Value() {
		ti.SetValue(completed)
		ti.CursorEnd()
		m.startInputs[m.startField] = ti
		m.syncStartField(m.startField, completed)
	}
	if len(matches) == 0 {
		return m.showToast("没有匹配的路径", toastWarning)
	}
	if len(matches) > 1 {
		return m.showToast("找到 "+strconv.Itoa(len(matches))+" 个匹配项；已补全公共前缀", toastInfo)
	}
	return m.showToast("路径已补全", toastSuccess)
}

func (m model) validateDirtyStartInputs() error {
	for field, dirty := range m.dirtyStartInputs {
		if !dirty {
			continue
		}
		value := strings.TrimSpace(m.startInputs[field].Value())
		switch field {
		case startFieldSimilarityThreshold:
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil || parsed < 0 || parsed > 1 {
				return fmt.Errorf("%s格式无效", startFieldName(field))
			}
		case startFieldHarborTimeout, startFieldHarborSetupTimeout, startFieldHarborConcurrency, startFieldHarborAttempts, startFieldHarborInfraRetries, startFieldAgentTimeout:
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 0 {
				return fmt.Errorf("%s格式无效", startFieldName(field))
			}
			if err := validateStartHarborPassInt(field, parsed); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateStartHarborPassInt(field startField, value int) error {
	switch field {
	case startFieldHarborConcurrency:
		maximum := minInt(domain.MaxHarborConcurrency, domain.RequiredTrialCount)
		if value > maximum {
			return fmt.Errorf("%s不能超过 %d", startFieldName(field), maximum)
		}
	case startFieldHarborAttempts:
		if value != 0 && value != domain.RequiredTrialCount {
			return fmt.Errorf("%s必须为 %d", startFieldName(field), domain.RequiredTrialCount)
		}
	}
	return nil
}

func (m model) validateStartBasic() error {
	if strings.TrimSpace(m.opts.Workspace) == "" {
		return fmt.Errorf("工作区路径不能为空")
	}
	if m.startMode == startGenerateTask {
		if strings.TrimSpace(m.opts.RepoURL) == "" || strings.TrimSpace(m.opts.Commit) == "" {
			return fmt.Errorf("从仓库生成需要填写仓库地址和提交哈希")
		}
		return nil
	}
	if strings.TrimSpace(m.opts.TaskDir) == "" {
		return fmt.Errorf("运行已有任务需要填写任务路径")
	}
	return nil
}
