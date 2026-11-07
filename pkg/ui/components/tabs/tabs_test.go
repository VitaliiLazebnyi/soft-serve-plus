package tabs

import (
	"context"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// Tabs switch with the keys bound to KeyMap.Section and KeyMap.PrevSection.
func TestTabsUseSectionKeys(t *testing.T) {
	t.Parallel()
	c := common.NewCommon(context.Background(), 80, 24)
	c.KeyMap.Section = key.NewBinding(key.WithKeys("]"))
	c.KeyMap.PrevSection = key.NewBinding(key.WithKeys("["))
	tb := New(c, []string{"a", "b", "c"})

	press := func(k tea.KeyPressMsg) int {
		_, cmd := tb.Update(k)
		if cmd == nil {
			return -1
		}
		if msg, ok := cmd().(ActiveTabMsg); ok {
			return int(msg)
		}
		return -1
	}
	if got := press(tea.KeyPressMsg{Code: ']', Text: "]"}); got != 1 {
		t.Errorf("next section: active tab = %d, want 1", got)
	}
	if got := press(tea.KeyPressMsg{Code: '[', Text: "["}); got != 0 {
		t.Errorf("previous section: active tab = %d, want 0", got)
	}
	if got := press(tea.KeyPressMsg{Code: '[', Text: "["}); got != 2 {
		t.Errorf("previous section wraps: active tab = %d, want 2", got)
	}
	if got := press(tea.KeyPressMsg{Code: tea.KeyTab}); got != -1 {
		t.Errorf("unbound tab key switched to tab %d", got)
	}
}
