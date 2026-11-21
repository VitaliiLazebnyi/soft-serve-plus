package repo

import (
	"fmt"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	gitm "github.com/aymanbagabas/git-module"
	"github.com/charmbracelet/soft-serve/git"
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

func (m StashListMsg) pane() paneID { return paneStash }

// StashPatchMsg is a message sent when the stash patch is loaded.
type StashPatchMsg struct {
	*git.Diff
	gen uint64
}

func (m StashPatchMsg) generation() uint64 { return m.gen }

func (m StashPatchMsg) pane() paneID { return paneStash }

// Stash is the stash component page.
type Stash struct {
	common       *common.Common
	width        int
	vp           *viewport.Viewport
	ref          *git.Reference
	gen          uint64
	repo         Repository
	list         *selector.Selector
	state        stashState
	currentPatch StashPatchMsg
	currentTitle string
	// render counts the renders of currentPatch; see diffRenderedMsg.
	render int
	// renders keeps one render in flight.
	renders common.RenderGate
	// winWidth is the window width of the last tea.WindowSizeMsg. The patch
	// is rendered again only when it changes: it changes with the pane's
	// width, which the parent has set by then and which the render uses.
	winWidth int
	// listLoad and patchLoad are the loads in flight of each kind: a new
	// one cancels the one before.
	listLoad, patchLoad common.Loader
}

// NewStash creates a new stash model.
func NewStash(common *common.Common) *Stash {
	vp := viewport.New(common)
	selector := selector.NewPane(common, StashItemDelegate{common})
	return &Stash{
		vp:     vp,
		common: common,
		list:   selector,
	}
}

// Path implements Pane.
func (s *Stash) Path() string {
	return ""
}

// TabName returns the name of the tab.
func (s *Stash) TabName() string {
	return "Stash"
}

// SetSize implements common.Component.
func (s *Stash) SetSize(width, height int) {
	s.width, _ = common.ClampSize(width, height)
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
	copyKey := s.common.CopyKey("copy message")
	if s.state == stashStatePatch {
		copyKey = s.common.CopyKey("copy patch")
	}
	b := [][]key.Binding{
		{
			s.common.KeyMap.Select,
			s.common.KeyMap.Back,
			copyKey,
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

// StatusBarValue implements Pane.
func (s *Stash) StatusBarValue() string {
	item, ok := s.list.SelectedItem().(StashItem)
	if !ok {
		return " "
	}
	idx := s.list.Index()
	return fmt.Sprintf("stash@{%d}: %s", idx, item.Title())
}

// StatusBarInfo implements Pane.
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

// Loading implements Pane.
func (s *Stash) Loading() (string, bool) {
	return "loading…", s.state == stashStateLoading
}

func (s *Stash) id() paneID { return paneStash }

// Init initializes the model.
func (s *Stash) Init() tea.Cmd {
	s.state = stashStateLoading
	s.clearPatch()
	s.patchLoad.Stop()
	return s.fetchStashCmd()
}

// Update implements common.Model.
func (s *Stash) Update(msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, 0)
	switch msg := msg.(type) {
	case RepoMsg:
		s.repo = msg
		s.clearPatch()
	case RefMsg:
		s.ref = msg.Ref
		s.gen = msg.gen
		s.list.Select(0)
		cmds = append(cmds, s.Init())
	case tea.WindowSizeMsg:
		// The patch is wrapped to the width, so only a width change needs
		// it rendered again.
		widthChanged := msg.Width != s.winWidth
		s.winWidth = msg.Width
		if widthChanged && s.currentPatch.Diff != nil {
			cmds = append(cmds, s.renderPatchCmd())
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
					cmds = append(cmds, copyCmd(patch.Patch, "Stash patch copied to clipboard"))
				}
			}
		}
	case errorMsg:
		// The list or a patch failed to load: leave the loading view. A
		// list that failed leaves the previous ref's stashes behind:
		// clear them.
		if s.state == stashStateLoading && s.currentPatch.Diff == nil {
			cmds = append(cmds, s.list.SetItems(nil))
		}
		if s.state == stashStateLoading {
			s.state = stashStateList
		}
	case StashListMsg:
		s.state = stashStateList
		items := make([]selector.IdentifiableItem, len(msg.stashes))
		for i, stash := range msg.stashes {
			items[i] = StashItem{stash}
		}
		cmds = append(cmds, s.list.SetItems(items))
	case StashPatchMsg:
		s.currentPatch = msg
		if msg.Diff == nil {
			s.vp.SetContent("")
			s.state = stashStatePatch
			break
		}
		s.currentTitle = ""
		if item, ok := s.list.SelectedItem().(StashItem); ok {
			s.currentTitle = item.Title()
		}
		// The pane shows the spinner until the patch is rendered.
		s.state = stashStateLoading
		cmds = append(cmds, s.renderPatchCmd())
	case diffRenderedMsg:
		again := s.renders.Done(msg.epoch)
		if msg.render == s.render && s.currentPatch.Diff != nil {
			s.vp.SetContent(msg.content)
			if s.state == stashStateLoading {
				// The first render of this patch; a later one follows a
				// resize and keeps the scroll position.
				s.vp.GotoTop()
				s.state = stashStatePatch
			}
		}
		// Otherwise the patch or the width changed while it was rendered:
		// render what is shown now, once for every change meanwhile.
		if again && s.currentPatch.Diff != nil {
			cmds = append(cmds, s.renderPatchCmd())
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
		s.clearPatch()
		// A patch still loading would open the patch view when it
		// arrives.
		s.patchLoad.Stop()
	}
	switch s.state {
	case stashStateList:
		cmds = append(cmds, s.list.Update(msg))
	case stashStatePatch:
		cmds = append(cmds, s.vp.Update(msg))
	}
	return tea.Batch(cmds...)
}

// View returns the view.
func (s *Stash) View() string {
	switch s.state {
	case stashStateList:
		return s.list.View()
	case stashStatePatch:
		return s.vp.View()
	}
	return ""
}

// renderPatchCmd returns a command that renders the current stash patch.
// The patch is highlighted by renderDiff, so it goes straight into the
// viewport rather than through the code component, which would highlight it
// again.
//
// It returns nil while a render is in flight: the next one starts when it
// returns.
func (s *Stash) renderPatchCmd() tea.Cmd {
	s.render++
	epoch, ok := s.renders.Start()
	if !ok {
		return nil
	}
	title := s.common.Styles.Stash.Title.Render(s.currentTitle) + "\n"
	return renderDiffCmd(paneStash, s.render, epoch, s.common.Styles, s.width, s.currentPatch.Diff, func() string {
		return title
	})
}

// clearPatch forgets the patch shown, and drops a render of it in flight. It
// also forgets that render, whose result may never come back, for example
// when the page was left while it ran.
func (s *Stash) clearPatch() {
	s.currentPatch = StashPatchMsg{}
	s.render++
	s.renders.Reset()
	s.vp.SetContent("")
}

// fetchStashCmd returns a command that lists the stashes of the repository.
// Commands run in their own goroutines, so it captures the model state it
// needs here instead of reading the model later.
func (s *Stash) fetchStashCmd() tea.Cmd {
	repo, gen := s.repo, s.gen
	ctx := s.listLoad.Start(s.common.Context())
	return func() tea.Msg {
		if ctx.Err() != nil {
			// A newer load replaced this one before it started.
			return nil
		}
		if repo == nil {
			return StashListMsg{gen: gen}
		}

		r, err := openWithContext(ctx, repo)
		if err != nil {
			return loadError(ctx, paneStash, gen, err)
		}

		stash, err := r.StashList()
		if err != nil {
			return loadError(ctx, paneStash, gen, err)
		}

		return StashListMsg{stashes: stash, gen: gen}
	}
}

// fetchStashPatchCmd returns a command that loads the patch of the selected
// stash.
func (s *Stash) fetchStashPatchCmd() tea.Cmd {
	repo, index, gen := s.repo, s.list.Index(), s.gen
	ctx := s.patchLoad.Start(s.common.Context())
	return func() tea.Msg {
		if ctx.Err() != nil {
			// A newer load replaced this one before it started.
			return nil
		}
		if repo == nil {
			return StashPatchMsg{gen: gen}
		}

		r, err := openWithContext(ctx, repo)
		if err != nil {
			return loadError(ctx, paneStash, gen, err)
		}

		diff, err := r.StashDiff(index)
		if err != nil {
			return loadError(ctx, paneStash, gen, err)
		}

		return StashPatchMsg{Diff: diff, gen: gen}
	}
}
