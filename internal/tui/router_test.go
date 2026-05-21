package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestRouterOverlayOnlyInterceptsKeys(t *testing.T) {
	router := newPageRouter()
	page := &routerTestPage{}
	overlay := &routerTestOverlay{interceptsAllKeys: true}
	router.RegisterPage(pageTaskBoard, page)
	_ = router.PushOverlay(overlay)

	handled, _ := router.Dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if !handled || page.updates != 0 || overlay.updates != 1 {
		t.Fatalf("key should be intercepted by overlay, handled=%v page=%d overlay=%d", handled, page.updates, overlay.updates)
	}

	handled, _ = router.Dispatch(routerTestMsg{})
	if !handled || page.updates != 1 || overlay.updates != 2 {
		t.Fatalf("non-key message should reach active page, handled=%v page=%d overlay=%d", handled, page.updates, overlay.updates)
	}
}

func TestSettingsOverlayUpdateHandlesCloseKeys(t *testing.T) {
	overlay := SettingsOverlay{}
	for _, msg := range []tea.KeyMsg{
		{Type: tea.KeyEsc},
		{Type: tea.KeyRunes, Runes: []rune("q")},
		{Type: tea.KeyCtrlUnderscore},
	} {
		handled, cmd := overlay.Update(msg)
		if !handled || cmd != nil {
			t.Fatalf("close key should be handled without cmd: %#v handled=%v cmd=%v", msg, handled, cmd)
		}
	}
	handled, _ := overlay.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if handled {
		t.Fatal("ordinary settings key should be delegated to the settings panel")
	}
}

type routerTestMsg struct{}

type routerTestPage struct {
	updates int
}

func (p *routerTestPage) Init() tea.Cmd { return nil }

func (p *routerTestPage) Update(tea.Msg) (bool, tea.Cmd) {
	p.updates++
	return true, nil
}

func (p *routerTestPage) View(int, int) string { return "" }
func (p *routerTestPage) Focus()               {}
func (p *routerTestPage) Blur()                {}
func (p *routerTestPage) HandleKey(tea.KeyMsg) tea.Cmd {
	return nil
}
func (p *routerTestPage) Destroy() tea.Cmd { return nil }

type routerTestOverlay struct {
	updates           int
	interceptsAllKeys bool
}

func (o *routerTestOverlay) Init() tea.Cmd { return nil }

func (o *routerTestOverlay) Update(tea.Msg) (bool, tea.Cmd) {
	o.updates++
	return false, nil
}

func (o *routerTestOverlay) View(int, int) string { return "" }
func (o *routerTestOverlay) ZIndex() int          { return 1 }
func (o *routerTestOverlay) InterceptsAllKeys() bool {
	return o.interceptsAllKeys
}
func (o *routerTestOverlay) Destroy() tea.Cmd { return nil }
