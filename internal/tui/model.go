package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/purplevoid/harbor-factory/internal/harbor/commandlog"
)

// viewMode intentionally has a single lifecycle page. Detail, confirmation,
// and control surfaces are overlays over the Task Hub rather than legacy
// workspace pages.
type viewMode int

const viewHub viewMode = iota

type model struct {
	ctx       context.Context
	cancel    context.CancelFunc
	lifecycle TaskHubLifecycleService

	width  int
	height int
	view   viewMode
	err    error
	notice string

	taskHub             TaskHubState
	taskHubLoadSequence uint64
	taskHubPrefix       taskHubPrefixState
	taskHubPlan         *TaskHubPlanPreview
	taskHubPlanCommand  *TaskHubCommand
	taskHubDetail       *TaskHubDetailOverlay
	taskHubMutation     *TaskHubMutationOverlay
	runControl          *RunControlOverlay
	exitHandoff         *taskHubExitHandoffOverlay

	hubSearching       bool
	hubSearch          textinput.Model
	taskHubHelpVisible bool
	toast              toastState

	router   *pageRouter
	focusMgr focusManager
}

// initialLifecycleHubModel is the only TUI construction path. It accepts an
// application-service boundary and deliberately has no Runner, Scheduler,
// Store, or arbitrary workspace path state.
func initialLifecycleHubModel(ctx context.Context, cancel context.CancelFunc, lifecycle TaskHubLifecycleService) model {
	return initModelComponents(model{
		ctx:       ctx,
		cancel:    cancel,
		lifecycle: lifecycle,
		taskHub:   newTaskHubState(),
		view:      viewHub,
	})
}

func (m model) header() string {
	title := titleStyle.Render("Harbor 出题工坊")
	context := redactSingleLineUI(fmt.Sprintf(
		"生命周期：Task %d  Run %d  队列 %d",
		len(m.taskHub.Snapshot.Tasks),
		len(m.taskHub.Snapshot.Runs),
		m.taskHub.Snapshot.Queue.Queued,
	))
	if m.width > 0 {
		context = truncateDisplay(context, m.width)
	}
	return lipgloss.JoinVertical(lipgloss.Left, title, subtleStyle.Render(context))
}

func contentWidth(width int) int {
	if width <= 0 {
		return 20
	}
	return maxInt(1, width-4)
}

func emptyDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func redactUI(text string) string {
	text = ansi.Strip(commandlog.RedactText(text))
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= 0x20 && r != 0x7f {
			return r
		}
		return -1
	}, text)
}

func redactSingleLineUI(text string) string {
	return strings.NewReplacer("\n", " ", "\t", " ").Replace(redactUI(text))
}
