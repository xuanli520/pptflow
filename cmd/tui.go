package cmd

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/xuanli520/pptflow/internal/app"
	"github.com/xuanli520/pptflow/internal/workflow"
)

func newTUICommand() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Open PPTflow TUI",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := tea.NewProgram(newModel()).Run()
			return err
		},
	}
}

type tuiModel struct {
	scenarios []string
	index     int
	running   bool
	result    *workflow.RunResult
	err       error
}

type runDone struct {
	result workflow.RunResult
	err    error
}

func newModel() tuiModel {
	return tuiModel{scenarios: []string{"performance_review", "business_plan", "roadshow"}}
}

func (m tuiModel) Init() tea.Cmd {
	return nil
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "up", "k":
			if m.index > 0 {
				m.index--
			}
		case "down", "j":
			if m.index < len(m.scenarios)-1 {
				m.index++
			}
		case "enter":
			if !m.running {
				m.running = true
				m.result = nil
				m.err = nil
				scenario := m.scenarios[m.index]
				return m, func() tea.Msg {
					result, err := app.RunPhase0(nil, app.Phase0Options{Scenario: scenario, ArtifactRoot: "artifacts", WorkspaceRoot: "workspace"})
					return runDone{result: result, err: err}
				}
			}
		}
	case runDone:
		m.running = false
		m.result = &msg.result
		m.err = msg.err
	}
	return m, nil
}

func (m tuiModel) View() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39")).Render("PPTflow Phase 0")
	var body string
	for i, scenario := range m.scenarios {
		cursor := "  "
		if i == m.index {
			cursor = "> "
		}
		body += cursor + scenario + "\n"
	}
	status := "Enter 运行，q 退出"
	if m.running {
		status = "正在运行 phase 0..."
	}
	if m.err != nil {
		status = "失败: " + m.err.Error()
	}
	if m.result != nil && m.err == nil {
		status = fmt.Sprintf("完成: %s\n%s", m.result.RunID, m.result.ArtifactRoot)
	}
	return title + "\n\n" + body + "\n" + status + "\n"
}
