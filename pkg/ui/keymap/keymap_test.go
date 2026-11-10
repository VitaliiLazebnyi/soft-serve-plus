package keymap

import (
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// ctrl+c always quits, so it must not also be a copy key that Quit shadows.
func TestCopyIsNotCtrlC(t *testing.T) {
	t.Parallel()
	km := DefaultKeyMap()
	ctrlC := tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	if key.Matches(ctrlC, km.Copy) {
		t.Error("ctrl+c matches Copy")
	}
	if !key.Matches(ctrlC, km.Quit) {
		t.Error("ctrl+c does not match Quit")
	}
}
