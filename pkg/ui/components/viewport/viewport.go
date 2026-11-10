package viewport

import (
	"math"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// Viewport represents a viewport component.
type Viewport struct {
	common common.Common
	*viewport.Model
}

// New returns a new Viewport.
func New(c common.Common) *Viewport {
	vp := viewport.New()
	vp.SetWidth(c.Width)
	vp.SetHeight(c.Height)
	vp.MouseWheelEnabled = true
	return &Viewport{
		common: c,
		Model:  &vp,
	}
}

// SetSize implements common.Component.
func (v *Viewport) SetSize(width, height int) {
	v.common.SetSize(width, height)
	v.Model.SetWidth(width)
	v.Model.SetHeight(height)
}

// Init implements tea.Model.
func (v *Viewport) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model.
func (v *Viewport) Update(msg tea.Msg) (common.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, v.common.KeyMap.GotoTop):
			v.GotoTop()
		case key.Matches(msg, v.common.KeyMap.GotoBottom):
			v.GotoBottom()
		}
	}
	vp, cmd := v.Model.Update(msg)
	v.Model = &vp
	return v, cmd
}

// View implements tea.Model.
func (v *Viewport) View() string {
	return v.Model.View()
}

// SetContent sets the viewport's content.
func (v *Viewport) SetContent(content string) {
	v.Model.SetContent(content)
}

// GotoTop moves the viewport to the top of the content.
func (v *Viewport) GotoTop() {
	v.Model.GotoTop()
}

// GotoBottom moves the viewport to the bottom of the content.
func (v *Viewport) GotoBottom() {
	v.Model.GotoBottom()
}

// HalfViewDown moves the viewport down by half the viewport height.
func (v *Viewport) HalfViewDown() {
	v.Model.HalfPageDown()
}

// HalfViewUp moves the viewport up by half the viewport height.
func (v *Viewport) HalfViewUp() {
	v.Model.HalfPageUp()
}

// ScrollPercent returns the viewport's scroll percentage.
func (v *Viewport) ScrollPercent() float64 {
	return v.Model.ScrollPercent()
}

// ScrollPosition returns the viewport's scroll percentage as a whole number
// between 0 and 100.
func (v *Viewport) ScrollPosition() int {
	scroll := v.ScrollPercent() * 100
	if scroll < 0 || math.IsNaN(scroll) {
		scroll = 0
	}
	return int(scroll)
}
