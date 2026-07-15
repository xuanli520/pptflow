package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestToastCyclesLongMessageWithinTerminalWidth(t *testing.T) {
	m := initialLifecycleHubModel(context.Background(), func() {}, nil)
	m.width = 14
	m.height = 24
	m.showToast("abcdefghijklmnopqrstuv", toastError)

	first := ansi.Strip(m.renderToast())
	if ansi.StringWidth(first) > m.width || !strings.Contains(first, "abcdefgh") {
		t.Fatalf("initial toast viewport = %q width=%d", first, ansi.StringWidth(first))
	}
	seenSuffix := false
	for index := 0; index < toastCycleLength(m.toast.Message); index++ {
		updated, command := m.Update(toastScrollMsg{id: m.toast.ID})
		m = updated.(model)
		if command == nil {
			t.Fatal("long toast did not schedule its next viewport")
		}
		visible := ansi.Strip(m.renderToast())
		if ansi.StringWidth(visible) > m.width {
			t.Fatalf("toast exceeded terminal width: %q width=%d terminal=%d", visible, ansi.StringWidth(visible), m.width)
		}
		if strings.Contains(visible, "qrstuv") {
			seenSuffix = true
		}
	}
	if !seenSuffix {
		t.Fatal("toast circular viewport never exposed the hidden suffix")
	}
}

func TestToastDoesNotScrollWhenMessageFitsViewport(t *testing.T) {
	m := initialLifecycleHubModel(context.Background(), func() {}, nil)
	m.width = 80
	m.showToast("短消息", toastSuccess)
	updated, command := m.Update(toastScrollMsg{id: m.toast.ID})
	m = updated.(model)
	if command != nil || m.toast.Offset != 0 || ansi.StringWidth(m.renderToast()) > m.width {
		t.Fatalf("short toast unexpectedly scrolled: offset=%d command=%v rendered=%q", m.toast.Offset, command, m.renderToast())
	}
}
