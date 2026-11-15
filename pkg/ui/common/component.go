package common

import (
	"charm.land/bubbles/v2/help"
	tea "charm.land/bubbletea/v2"
)

// Model is a UI component. Components are pointers that change in place, so
// Update returns only the command to run; only the two top-level programs
// implement tea.Model itself.
type Model interface {
	Init() tea.Cmd
	Update(tea.Msg) tea.Cmd
	View() string
}

// Component represents a Bubble Tea model that implements a SetSize function.
type Component interface {
	Model
	help.KeyMap
	SetSize(width, height int)
}
