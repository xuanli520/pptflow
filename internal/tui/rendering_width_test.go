package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/purplevoid/harbor-factory/internal/app"
)

func TestTextInputViewUsesExactCellBudget(t *testing.T) {
	values := []string{
		"https://github.com/tower-rs/tower-http/tree/main/tower-http",
		"工作区/中文目录/任务文件",
		"Cafe\u0301/combining/value",
		"family-👨‍👩‍👧‍👦-workspace",
	}
	for _, value := range values {
		for _, width := range []int{8, 16, 31} {
			for _, cursor := range []int{0, len([]rune(value)) / 2, len([]rune(value))} {
				input := textinput.New()
				input.Prompt = ""
				input.SetValue(value)
				input.SetCursor(cursor)
				input.Focus()
				got := textInputView(input, width)
				if gotWidth := ansi.StringWidth(got); gotWidth != width {
					t.Fatalf("value=%q width=%d cursor=%d rendered width=%d: %q", value, width, cursor, gotWidth, got)
				}
				if strings.Contains(got, "\n") {
					t.Fatalf("text input rendered a newline: value=%q width=%d cursor=%d", value, width, cursor)
				}
			}
		}
	}
}

func TestFocusedStartURLKeepsClosingBracketOnInputLine(t *testing.T) {
	const longURL = "https://github.com/tower-rs/tower-http/tree/main/tower-http/src/decompression/service.rs"
	for _, width := range []int{40, 60, 80, 96, 120} {
		m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{RepoURL: longURL})
		m.width, m.height = width, 30
		m.startMode = startGenerateTask
		m.startField = startFieldRepoURL
		m.focusStartInput(startFieldRepoURL)
		input := m.startInputs[startFieldRepoURL]
		for _, cursor := range []int{0, len([]rune(longURL)) / 2, len([]rune(longURL))} {
			input.SetCursor(cursor)
			m.startInputs[startFieldRepoURL] = input
			line := m.renderStartField(startFieldRepoURL)
			plain := ansi.Strip(line)
			if strings.Contains(plain, "\n") || !strings.HasSuffix(plain, " ]") {
				t.Fatalf("%d columns cursor=%d lost closing bracket on focused line: %q", width, cursor, plain)
			}
			lineBudget := styleContentWidth(contentWidth(width), panelStyle)
			if got := ansi.StringWidth(line); got > lineBudget {
				t.Fatalf("%d columns cursor=%d line width=%d budget=%d: %q", width, cursor, got, lineBudget, line)
			}
		}

		view := ansi.Strip(m.View())
		matched := false
		for _, line := range strings.Split(view, "\n") {
			if strings.Contains(line, localizeField(startFieldRepoURL)) {
				matched = true
				if !strings.Contains(line, " ]") {
					t.Fatalf("%d-column full view wrapped the closing bracket:\n%s", width, view)
				}
			}
		}
		if !matched {
			t.Fatalf("%d-column full view did not show focused repository field:\n%s", width, view)
		}
	}
}

func TestOverlaysFitExtremeTerminalSizes(t *testing.T) {
	for _, size := range []struct{ width, height int }{{20, 10}, {40, 12}} {
		views := map[string]string{
			"confirm": newConfirmDialog(confirmQuit, "确认", "包含\r\n换行的确认消息").View(size.width, size.height),
			"control": newRunControlOverlay("run\nidentifier", "target/工作区/👨‍👩‍👧‍👦", false, false).View(size.width, size.height),
			"help":    (&helpOverlay{view: viewStart}).View(size.width, size.height),
		}
		for name, view := range views {
			assertRenderedWidth(t, name, view, size.width)
			if got := len(strings.Split(strings.TrimSuffix(view, "\n"), "\n")); got > size.height {
				t.Fatalf("%s exceeded %dx%d terminal height with %d lines:\n%s", name, size.width, size.height, got, ansi.Strip(view))
			}
		}
	}
}

func TestDisplayPaddingAndMouseTargetsUseGraphemeWidth(t *testing.T) {
	styled := lipgloss.NewStyle().Reverse(true).Render("中文👨‍👩‍👧‍👦e\u0301")
	if got := ansi.StringWidth(padRightDisplay(styled, 14)); got != 14 {
		t.Fatalf("ANSI/grapheme padding width=%d", got)
	}

	prefix := "前缀中文👨‍👩‍👧‍👦e\u0301 "
	marker := "目标"
	frame := newRenderedFrame(prefix + selectedStyle.Render(marker))
	targets := frame.targets(marker, func() tea.Cmd { return nil })
	if len(targets) != 1 {
		t.Fatalf("marker targets=%d", len(targets))
	}
	if want := ansi.StringWidth(prefix); targets[0].x != want {
		t.Fatalf("mouse x=%d want=%d", targets[0].x, want)
	}
	if want := ansi.StringWidth(marker); targets[0].width != want {
		t.Fatalf("mouse width=%d want=%d", targets[0].width, want)
	}
}

func TestSingleLineUIStripsTerminalControls(t *testing.T) {
	got := redactSingleLineUI("safe\x1b[2J\r\nnext\tvalue\a")
	if got != "safe next value" {
		t.Fatalf("single-line sanitization=%q", got)
	}
}

func assertRenderedWidth(t *testing.T, name, rendered string, width int) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSuffix(rendered, "\n"), "\n") {
		if got := ansi.StringWidth(line); got > width {
			t.Fatalf("%s exceeded %d columns with width %d: %q", name, width, got, line)
		}
	}
}
