package repo

import (
	"fmt"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	gitm "github.com/aymanbagabas/git-module"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/viewport"
)

type stashState int

const (
	stashStateLoading stashState = iota
	stashStateList
	stashStatePatch
)

// StashListMsg is a message sent when the stash list is loaded.
type StashListMsg struct {
	stashes []*gitm.Stash
	gen     uint64
}

func (m StashListMsg) generation() uint64 { return m.gen }

// StashPatchMsg is a message sent when the stash patch is loaded.
type StashPatchMsg struct {
	*git.Diff
	gen uint64
}

func (m StashPatchMsg) generation() uint64 { return m.gen }

// Stash is the stash component page.
type Stash struct {
	common       common.Common
	vp           *viewport.Viewport
	ref          *git.Reference
	gen          uint64
	repo         proto.Repository
	spinner      spinner.Model
	list         *selector.Selector
	state        stashState
	currentPatch StashPatchMsg
	currentTitle string
}

// NewStash creates a new stash model.
func NewStash(common common.Common) *Stash {
	vp := viewport.New(common)
	s := spinner.New(spinner.WithSpinner(spinner.Dot),
		spinner.WithStyle(common.Styles.Spinner))
	selector := selector.New(common, []selector.IdentifiableItem{}, StashItemDelegate{&common})
	selector.SetShowFilter(false)
	selector.SetShowHelp(false)
	selector.SetShowPagination(false)
	selector.SetShowStatusBar(false)
	selector.SetShowTitle(false)
	selector.SetFilteringEnabled(false)
	selector.DisableQuitKeybindings()
	selector.KeyMap.NextPage = common.KeyMap.NextPage
	selector.KeyMap.PrevPage = common.KeyMap.PrevPage
	return &Stash{
		vp:      vp,
		common:  common,
		spinner: s,
		list:    selector,
	}
}

// Path implements common.TabComponent.
func (s *Stash) Path() string {
	return ""
}

// TabName returns the name of the tab.
func (s *Stash) TabName() string {
	return "Stash"
}

// SetSize implements common.Component.
func (s *Stash) SetSize(width, height int) {
	s.common.SetSize(width, height)
	s.vp.SetSize(width, height)
	s.list.SetSize(width, height)
}

// ShortHelp implements help.KeyMap.
func (s *Stash) ShortHelp() []key.Binding {
	return []key.Binding{
		s.common.KeyMap.Select,
		s.common.KeyMap.Back,
		s.common.KeyMap.UpDown,
	}
}

// FullHelp implements help.KeyMap.
func (s *Stash) FullHelp() [][]key.Binding {
	b := [][]key.Binding{
		{
			s.common.KeyMap.Select,
			s.common.KeyMap.Back,
			s.common.KeyMap.Copy,
		},
		{
			s.vp.KeyMap.Down,
			s.vp.KeyMap.Up,
			s.common.KeyMap.GotoTop,
			s.common.KeyMap.GotoBottom,
		},
	}
	return b
}

// StatusBarValue implements common.Component.
func (s *Stash) StatusBarValue() string {
	item, ok := s.list.SelectedItem().(StashItem)
	if !ok {
		return " "
	}
	idx := s.list.Index()
	return fmt.Sprintf("stash@{%d}: %s", idx, item.Title())
}

// StatusBarInfo implements common.Component.
func (s *Stash) StatusBarInfo() string {
	switch s.state {
	case stashStateList:
		totalPages := s.list.TotalPages()
		if totalPages <= 1 {
			return "p. 1/1"
		}
		return fmt.Sprintf("p. %d/%d", s.list.Page()+1, totalPages)
	case stashStatePatch:
		return common.ScrollPercent(s.vp.ScrollPosition())
	default:
		return ""
	}
}

// SpinnerID implements common.Component.
func (s *Stash) SpinnerID() int {
	return s.spinner.ID()
}

// Init initializes the model.
func (s *Stash) Init() tea.Cmd {
	s.state = stashStateLoading
	return tea.Batch(s.spinner.Tick, s.fetchStashCmd())
}

// Update updates the model.
func (s *Stash) Update(msg tea.Msg) (common.Model, tea.Cmd) {
	cmds := make([]tea.Cmd, 0)
	switch msg := msg.(type) {
	case RepoMsg:
		s.repo = msg
	case RefMsg:
		s.ref = msg.Ref
		s.gen = msg.gen
		s.list.Select(0)
		cmds = append(cmds, s.Init())
	case tea.WindowSizeMsg:
		s.SetSize(msg.Width, msg.Height)
		if s.currentPatch.Diff != nil {
			s.renderPatch()
		}
	case spinner.TickMsg:
		if s.state == stashStateLoading && s.spinner.ID() == msg.ID {
			sp, cmd := s.spinner.Update(msg)
			s.spinner = sp
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case tea.KeyPressMsg:
		switch s.state {
		case stashStateList:
			// Copying the selected stash message is handled by
			// StashItemDelegate.
			switch {
			case key.Matches(msg, s.common.KeyMap.BackItem):
				cmds = append(cmds, goBackCmd)
			}
		case stashStatePatch:
			switch {
			case key.Matches(msg, s.common.KeyMap.BackItem):
				cmds = append(cmds, goBackCmd)
			case key.Matches(msg, s.common.KeyMap.Copy):
				if s.currentPatch.Diff != nil {
					patch := s.currentPatch.Diff
					cmds = append(cmds, copyCmd(patch.Patch(), "Stash patch copied to clipboard"))
				}
			}
		}
	case StashListMsg:
		s.state = stashStateList
		items := make([]selector.IdentifiableItem, len(msg.stashes))
		for i, stash := range msg.stashes {
			items[i] = StashItem{stash}
		}
		cmds = append(cmds, s.list.SetItems(items))
	case StashPatchMsg:
		s.state = stashStatePatch
		s.currentPatch = msg
		if msg.Diff != nil {
			s.currentTitle = ""
			if item, ok := s.list.SelectedItem().(StashItem); ok {
				s.currentTitle = item.Title()
			}
			s.renderPatch()
			s.vp.GotoTop()
		}
	case selector.SelectMsg:
		switch msg.IdentifiableItem.(type) {
		case StashItem:
			cmds = append(cmds, s.fetchStashPatchCmd())
		}
	case GoBackMsg:
		if s.state == stashStateList {
			s.list.Select(0)
		}
		s.state = stashStateList
	}
	switch s.state {
	case stashStateList:
		l, cmd := s.list.Update(msg)
		s.list = l.(*selector.Selector)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	case stashStatePatch:
		v, cmd := s.vp.Update(msg)
		s.vp = v.(*viewport.Viewport)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return s, tea.Batch(cmds...)
}

// View returns the view.
func (s *Stash) View() string {
	switch s.state {
	case stashStateLoading:
		return renderLoading(s.common, s.spinner)
	case stashStateList:
		return s.list.View()
	case stashStatePatch:
		return s.vp.View()
	}
	return ""
}

// renderPatch shows the current stash patch. The patch is already
// highlighted by renderDiff, so it goes straight into the viewport rather
// than through the code component, which would highlight it again.
func (s *Stash) renderPatch() {
	s.vp.SetContent(lipgloss.JoinVertical(lipgloss.Left,
		s.common.Styles.Stash.Title.Render(s.currentTitle),
		"",
		renderSummary(s.currentPatch.Diff, s.common.Styles, s.common.Width),
		renderDiff(s.currentPatch.Diff, s.common.Width),
	))
}

// fetchStashCmd returns a command that lists the stashes of the repository.
// Commands run in their own goroutines, so it captures the model state it
// needs here instead of reading the model later.
func (s *Stash) fetchStashCmd() tea.Cmd {
	repo, gen := s.repo, s.gen
	return func() tea.Msg {
		if repo == nil {
			return StashListMsg{gen: gen}
		}

		r, err := repo.Open()
		if err != nil {
			return common.ErrorMsg(err)
		}

		stash, err := r.StashList()
		if err != nil {
			return common.ErrorMsg(err)
		}

		return StashListMsg{stashes: stash, gen: gen}
	}
}

// fetchStashPatchCmd returns a command that loads the patch of the selected
// stash.
func (s *Stash) fetchStashPatchCmd() tea.Cmd {
	repo, index, gen := s.repo, s.list.Index(), s.gen
	return func() tea.Msg {
		if repo == nil {
			return StashPatchMsg{gen: gen}
		}

		r, err := repo.Open()
		if err != nil {
			return common.ErrorMsg(err)
		}

		diff, err := r.StashDiff(index)
		if err != nil {
			return common.ErrorMsg(err)
		}

		return StashPatchMsg{Diff: diff, gen: gen}
	}
}
