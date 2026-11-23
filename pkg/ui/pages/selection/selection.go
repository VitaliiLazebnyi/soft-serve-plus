package selection

import (
	"context"
	"sort"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/code"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/tabs"
)

const (
	defaultNoContent = "No readme found.\n\nAn admin can create a `.soft-serve` repository with a `README.md` file to show it here."
)

type pane int

const (
	selectorPane pane = iota
	readmePane
	lastPane
)

// refreshKey reloads the repository list, so repositories created, removed
// or shared since the page opened show up without reconnecting.
var refreshKey = key.NewBinding(
	key.WithKeys("r"),
	key.WithHelp("r", "refresh"),
)

func (p pane) String() string {
	return []string{
		"Repositories",
		"About",
	}[p]
}

// Selection is the model for the selection screen/page.
type Selection struct {
	common     *common.Common
	width      int
	height     int
	readme     *code.Code
	selector   *selector.Selector
	activePane pane
	tabs       *tabs.Tabs
	// load is the listing in flight: a refresh cancels it and starts
	// another.
	load common.Loader
}

// New creates a new selection model.
func New(c *common.Common) *Selection {
	ts := make([]string, lastPane)
	for i, b := range []pane{selectorPane, readmePane} {
		ts[i] = b.String()
	}
	t := tabs.New(c, ts)
	t.TabSeparator = lipgloss.NewStyle()
	t.TabInactive = c.Styles.TopLevelNormalTab
	t.TabActive = c.Styles.TopLevelActiveTab
	t.TabDot = c.Styles.TopLevelActiveTabDot
	t.UseDot = true
	sel := &Selection{
		common:     c,
		activePane: selectorPane, // start with the selector focused
		tabs:       t,
	}
	readme := code.New(c, "", "")
	readme.UseGlamour = true
	readme.NoContentStyle = c.Styles.NoContent.
		SetString(defaultNoContent)
	selector := selector.New(c,
		[]selector.IdentifiableItem{},
		NewItemDelegate(c, &sel.activePane))
	selector.SetShowTitle(false)
	selector.SetShowHelp(false)
	selector.SetShowStatusBar(false)
	selector.DisableQuitKeybindings()
	sel.selector = selector
	sel.readme = readme
	return sel
}

func (s *Selection) getMargins() (wm, hm int) {
	wm = 0
	hm = s.common.Styles.Tabs.GetVerticalFrameSize() +
		s.common.Styles.Tabs.GetHeight()
	if s.activePane == selectorPane && s.IsFiltering() {
		// hide tabs when filtering
		hm = 0
	}
	return
}

// FilterState returns the current filter state.
func (s *Selection) FilterState() list.FilterState {
	return s.selector.FilterState()
}

// SetSize implements common.Component.
func (s *Selection) SetSize(width, height int) {
	s.width, s.height = common.ClampSize(width, height)
	wm, hm := s.getMargins()
	s.tabs.SetSize(width, height-hm)
	s.selector.SetSize(width-wm, height-hm)
	s.readme.SetSize(width-wm, height-hm-1) // -1 for readme status line
}

// IsFiltering returns true if the selector is currently filtering.
func (s *Selection) IsFiltering() bool {
	return s.FilterState() == list.Filtering
}

// ShortHelp implements help.KeyMap.
func (s *Selection) ShortHelp() []key.Binding {
	k := s.selector.KeyMap
	kb := make([]key.Binding, 0)
	kb = append(kb,
		s.common.KeyMap.UpDown,
		s.common.KeyMap.Section,
	)
	if s.activePane == selectorPane {
		copyKey := s.common.CopyKey("copy command")
		kb = append(kb,
			s.common.KeyMap.Select,
			k.Filter,
			k.ClearFilter,
			copyKey,
		)
		// While the filter is typed, r goes into the filter text, so the
		// refresh key is only offered outside of filtering.
		if !s.IsFiltering() {
			kb = append(kb, refreshKey)
		}
	}
	return kb
}

// FullHelp implements help.KeyMap.
func (s *Selection) FullHelp() [][]key.Binding {
	b := [][]key.Binding{
		{
			s.common.KeyMap.Section,
		},
	}
	switch s.activePane {
	case readmePane:
		k := s.readme.KeyMap
		b = append(b, []key.Binding{
			k.PageDown,
			k.PageUp,
		})
		b = append(b, []key.Binding{
			k.HalfPageDown,
			k.HalfPageUp,
		})
		b = append(b, []key.Binding{
			k.Down,
			k.Up,
		})
	case selectorPane:
		copyKey := s.common.CopyKey("copy command")
		k := s.selector.KeyMap
		if !s.IsFiltering() {
			b[0] = append(b[0],
				s.common.KeyMap.Select,
				copyKey,
				refreshKey,
			)
		}
		b = append(b, []key.Binding{
			k.CursorUp,
			k.CursorDown,
		})
		b = append(b, []key.Binding{
			k.NextPage,
			k.PrevPage,
			k.GoToStart,
			k.GoToEnd,
		})
		b = append(b, []key.Binding{
			k.Filter,
			k.ClearFilter,
			k.CancelWhileFiltering,
			k.AcceptWhileFiltering,
		})
	}
	return b
}

// setReadme shows the server README. It comes from the about repository;
// the code view sanitises it when it renders it.
func (s *Selection) setReadme(readme, path string) tea.Cmd {
	return s.readme.SetContent(readme, path)
}

// ReposMsg carries the repositories the selection page lists, loaded off
// the UI goroutine by Init.
type ReposMsg struct {
	items      []selector.IdentifiableItem
	readme     string
	readmePath string
	hasReadme  bool
}

// Init implements common.Model.
func (s *Selection) Init() tea.Cmd {
	return loadReposCmd(s.load.Start(s.common.Context()), s.common)
}

// loadReposCmd lists the repositories the caller may read, and the server
// readme, without blocking the UI. bubbletea runs the command on its own
// goroutine while Update keeps resizing the page, so it reads only ctx, the
// load's context, and c, which does not change, and never the model.
func loadReposCmd(ctx context.Context, c *common.Common) tea.Cmd {
	return func() tea.Msg {
		return loadRepos(ctx, c)
	}
}

func loadRepos(ctx context.Context, c *common.Common) tea.Msg {
	if ctx.Err() != nil {
		// A refresh replaced this one before it started.
		return nil
	}
	be := c.Backend
	pk := c.PublicKey()
	if pk == nil {
		allow, err := be.AllowKeylessSetting(ctx)
		if err != nil {
			// Not "nothing to list": the error screen shows a generic
			// message for it.
			return common.LoadError(ctx, err)
		}
		if !allow {
			return nil
		}
	}

	// The session user, not a fresh lookup of the key: a lookup that
	// failed would list what an anonymous caller sees.
	repos, err := be.ReadableRepositoriesForUser(ctx, proto.UserFromContext(ctx))
	if err != nil {
		return common.LoadError(ctx, err)
	}
	var msg ReposMsg
	sortedItems := make(Items, 0, len(repos))
	for _, r := range repos {
		// Every repository here is readable by the caller.
		if r.Name() == backend.AboutRepo && be.AboutRepoOwnedByAdmin(ctx, r) {
			readme, path, err := backend.Readme(ctx, r, nil, config.MaxBlobSize)
			if err != nil {
				c.Logger.Debugf("failed to read the about readme: %v", err)
			} else {
				// setReadme sanitises it when Update shows it.
				msg.readme, msg.readmePath, msg.hasReadme = readme, path, true
			}
		}

		if r.IsHidden() {
			continue
		}
		sortedItems = append(sortedItems, NewItem(c, r))
	}
	sort.Stable(sortedItems)
	msg.items = make([]selector.IdentifiableItem, len(sortedItems))
	for i, it := range sortedItems {
		msg.items[i] = it
	}
	if ctx.Err() != nil {
		// Cancelled while it ran: the list or the readme may be missing
		// because of it.
		return nil
	}
	return msg
}

// setItems replaces the listed repositories and keeps the highlighted one
// highlighted when it is still listed, so a refresh does not move the cursor.
func (s *Selection) setItems(items []selector.IdentifiableItem) tea.Cmd {
	var id string
	if s.selector.FilterState() == list.Unfiltered {
		if it := s.selector.SelectedItem(); it != nil {
			id = it.ID()
		}
	}
	cmd := s.selector.SetItems(items)
	if id != "" {
		for i, it := range items {
			if it.ID() == id {
				s.selector.Select(i)
				break
			}
		}
	}
	return cmd
}

// Update implements common.Model.
func (s *Selection) Update(msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, 0)
	// routed is set when the message has already been delivered to every
	// pane, so that the active pane does not get it a second time.
	routed := false
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		routed = true
		cmds = append(cmds, s.readme.Update(msg))
		cmds = append(cmds, s.selector.Update(msg))
	case code.RenderedMsg:
		// The readme's render, whichever pane is active.
		return s.readme.Update(msg)
	case tea.KeyPressMsg, tea.MouseMsg:
		switch msg := msg.(type) {
		case tea.KeyPressMsg:
			switch {
			case key.Matches(msg, s.common.KeyMap.Back):
				cmds = append(cmds, s.selector.Init())
			case key.Matches(msg, refreshKey) && s.activePane == selectorPane &&
				s.FilterState() != list.Filtering:
				cmds = append(cmds, s.Init())
			}
		}
		cmds = append(cmds, s.tabs.Update(msg))
	case tabs.ActiveTabMsg:
		s.activePane = pane(msg)
	case ReposMsg:
		cmds = append(cmds, s.setItems(msg.items), s.selector.Init())
		if msg.hasReadme {
			cmds = append(cmds, s.setReadme(msg.readme, msg.readmePath))
		}
		return tea.Batch(cmds...)
	}
	if routed {
		return tea.Batch(cmds...)
	}
	switch s.activePane {
	case readmePane:
		cmds = append(cmds, s.readme.Update(msg))
	case selectorPane:
		cmds = append(cmds, s.selector.Update(msg))
	}
	return tea.Batch(cmds...)
}

// View implements common.Model.
func (s *Selection) View() string {
	var view string
	wm, hm := s.getMargins()
	switch s.activePane {
	case selectorPane:
		ss := lipgloss.NewStyle().
			Width(s.width - wm).
			Height(s.height - hm)
		view = ss.Render(s.selector.View())
	case readmePane:
		rs := lipgloss.NewStyle().
			Height(s.height - hm)
		status := common.ScrollPercent(s.readme.ScrollPosition())
		if s.readme.Rendering() {
			// The readme on screen is not the one being rendered.
			status = "loading…"
		}
		readmeStatus := lipgloss.NewStyle().
			Align(lipgloss.Right).
			Width(s.width - wm).
			Foreground(s.common.Styles.InactiveBorderColor).
			Render(status)
		view = rs.Render(lipgloss.JoinVertical(lipgloss.Left,
			s.readme.View(),
			readmeStatus,
		))
	}
	if s.activePane != selectorPane || s.FilterState() != list.Filtering {
		tabs := s.common.Styles.Tabs.Render(s.tabs.View())
		view = lipgloss.JoinVertical(lipgloss.Left,
			tabs,
			view,
		)
	}
	return lipgloss.JoinVertical(
		lipgloss.Left,
		view,
	)
}
