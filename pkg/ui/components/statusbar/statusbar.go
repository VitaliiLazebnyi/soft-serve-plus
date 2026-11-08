package statusbar

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/x/ansi"
)

// Model is a status bar model.
type Model struct {
	common common.Common
	key    string
	value  string
	info   string
	extra  string
	// msg is a transient message, such as a copy notice, shown in place of
	// the value until the status is set again.
	msg string
}

// New creates a new status bar component.
func New(c common.Common) *Model {
	s := &Model{
		common: c,
	}
	return s
}

// SetSize implements common.Component.
func (s *Model) SetSize(width, height int) {
	s.common.Width = width
	s.common.Height = height
}

// SetStatus sets the status bar status. Empty values clear the matching
// part, and any transient message set with SetMessage is dropped.
func (s *Model) SetStatus(key, value, info, extra string) {
	s.key = key
	s.value = value
	s.info = info
	s.extra = extra
	s.msg = ""
}

// SetMessage shows a transient message in place of the status value until
// the next SetStatus call.
func (s *Model) SetMessage(msg string) {
	s.msg = msg
}

// Init implements tea.Model.
func (s *Model) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model.
func (s *Model) Update(msg tea.Msg) (common.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.SetSize(msg.Width, msg.Height)
	}
	return s, nil
}

// View implements tea.Model.
func (s *Model) View() string {
	st := s.common.Styles
	w := lipgloss.Width
	help := s.common.Zone.Mark(
		"repo-help",
		st.StatusBarHelp.Render("? Help"),
	)
	key := st.StatusBarKey.Render(s.key)
	info := ""
	if s.info != "" {
		info = st.StatusBarInfo.Render(s.info)
	}
	branch := st.StatusBarBranch.Render(s.extra)
	maxWidth := s.common.Width - w(key) - w(info) - w(branch) - w(help)
	v := s.value
	if s.msg != "" {
		v = s.msg
	}
	v = ansi.Truncate(v, maxWidth-st.StatusBarValue.GetHorizontalFrameSize(), "…")
	value := st.StatusBarValue.
		Width(maxWidth).
		Render(v)

	return lipgloss.NewStyle().MaxWidth(s.common.Width).
		Render(
			lipgloss.JoinHorizontal(lipgloss.Top,
				key,
				value,
				info,
				branch,
				help,
			),
		)
}
