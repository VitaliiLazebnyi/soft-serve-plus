package repo

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/code"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/footer"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/statusbar"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/tabs"
	"github.com/charmbracelet/soft-serve/pkg/utils"
)

type state int

const (
	loadingState state = iota
	readyState
	// failedState is the state of a repository whose reference failed to
	// load. Its tabs still hold what was loaded for the repository shown
	// before, so none of them is shown or gets input.
	failedState
)

// EmptyRepoMsg is a message to indicate that the repository is empty.
type EmptyRepoMsg struct {
	gen uint64
}

func (m EmptyRepoMsg) generation() uint64 { return m.gen }

// genMsg is a message with something loaded for one repository and ref. Its
// generation is the one the repository page was in when the load started.
//
// The repository page starts a new generation whenever it is given another
// repository or ref, and drops a message from an older one: it was loaded
// for a repository or ref the user has left since, and must not be shown in
// place of the current one.
type genMsg interface {
	generation() uint64
}

// RepoMsg is a message that contains a repository.
type RepoMsg proto.Repository //nolint:revive

// GoBackMsg is a message to go back to the previous view.
type GoBackMsg struct{}

// CopyMsg is a message to indicate copied text.
type CopyMsg struct {
	Text    string
	Message string
}

// SwitchTabMsg is a message to switch to a tab.
type SwitchTabMsg paneID

// Repo is a view for a git repository.
type Repo struct {
	common       common.Common
	zonePrefix   string
	selectedRepo proto.Repository
	activeTab    int
	tabs         *tabs.Tabs
	statusbar    *statusbar.Model
	panes        []Pane
	ref          *git.Reference
	state        state
	// spinner turns while the page or one of its tabs loads; ticking is set
	// while its ticks run.
	spinner    spinner.Model
	ticking    bool
	panesReady []bool
	// gen is the generation of the repository and ref shown; see genMsg.
	gen uint64
	// noMenu is set when there is no repository menu to go back to, as in
	// soft browse.
	noMenu bool
	// refLoad is the load of the reference to show: opening another
	// repository cancels it.
	refLoad common.Loader
}

// New returns a new Repo.
func New(c common.Common, comps ...Pane) *Repo {
	sb := statusbar.New(c)
	ts := make([]string, 0)
	for _, c := range comps {
		ts = append(ts, c.TabName())
	}
	c.Logger = c.Logger.WithPrefix("ui.repo")
	tb := tabs.New(c, ts)
	s := c.NewSpinner()
	r := &Repo{
		common:     c,
		zonePrefix: c.Zone.NewPrefix(),
		tabs:       tb,
		statusbar:  sb,
		panes:      comps,
		state:      loadingState,
		spinner:    s,
		panesReady: make([]bool, len(comps)),
	}
	return r
}

// SetNoMenu tells the repository view that there is no repository menu to go
// back to, so esc at the top of a tab does nothing.
func (r *Repo) SetNoMenu(noMenu bool) {
	r.noMenu = noMenu
}

func (r *Repo) getMargins() (int, int) {
	hh := lipgloss.Height(r.headerView())
	hm := r.common.Styles.Repo.Body.GetVerticalFrameSize() +
		hh +
		r.common.Styles.Repo.Header.GetVerticalFrameSize() +
		r.common.Styles.StatusBar.GetHeight()
	return 0, hm
}

// SetSize implements common.Component.
func (r *Repo) SetSize(width, height int) {
	r.common.SetSize(width, height)
	_, hm := r.getMargins()
	r.tabs.SetSize(width, height-hm)
	r.statusbar.SetSize(width, height-hm)
	for _, p := range r.panes {
		p.SetSize(width, height-hm)
	}
}

// Repository returns the repository the page shows, or nil before one is
// selected.
func (r *Repo) Repository() proto.Repository {
	return r.selectedRepo
}

// Path returns the current component path.
func (r *Repo) Path() string {
	if r.state == failedState {
		return ""
	}
	return r.panes[r.activeTab].Path()
}

func (r *Repo) commonHelp() []key.Binding {
	b := make([]key.Binding, 0)
	back := r.common.KeyMap.Back
	switch {
	case r.Path() != "":
		// esc leaves the current sub-view; the pane says what it goes back to.
		back.SetHelp("esc", "back")
		b = append(b, back)
	case !r.noMenu:
		back.SetHelp("esc", "back to menu")
		b = append(b, back)
	}
	tab := r.common.KeyMap.Section
	tab.SetHelp("tab", "switch tab")
	b = append(b, tab)
	return b
}

// ShortHelp implements help.KeyMap.
func (r *Repo) ShortHelp() []key.Binding {
	b := r.commonHelp()
	b = append(b, r.panes[r.activeTab].(help.KeyMap).ShortHelp()...)
	return b
}

// FullHelp implements help.KeyMap.
func (r *Repo) FullHelp() [][]key.Binding {
	b := make([][]key.Binding, 0)
	b = append(b, r.commonHelp())
	b = append(b, r.panes[r.activeTab].(help.KeyMap).FullHelp()...)
	return b
}

// Init implements tea.Model.
func (r *Repo) Init() tea.Cmd {
	r.state = loadingState
	r.activeTab = 0
	// A tick sent while another page was shown went to that page and
	// stopped the spinner, so start it again. Duplicate ticks are dropped
	// by the spinner.
	r.ticking = true
	return tea.Batch(
		r.tabs.Init(),
		r.statusbar.Init(),
		r.spinner.Tick,
	)
}

// busy reports whether the page or one of its tabs is loading.
func (r *Repo) busy() bool {
	switch r.state {
	case loadingState:
		return true
	case failedState:
		return false
	}
	for _, p := range r.panes {
		if _, ok := p.Loading(); ok {
			return true
		}
	}
	return false
}

// Update implements common.Model.
func (r *Repo) Update(msg tea.Msg) tea.Cmd {
	if m, ok := msg.(genMsg); ok {
		if m.generation() != r.gen {
			return nil
		}
		// A new ref, or none, is shown: what was loaded for the previous
		// one is out of date from here on. The panes tag their loads with
		// the generation they get here.
		switch m := msg.(type) {
		case RefMsg:
			r.gen++
			m.gen = r.gen
			msg = m
		case EmptyRepoMsg:
			r.gen++
			m.gen = r.gen
			msg = m
		}
	}
	cmds := make([]tea.Cmd, 0)
	// routed is set when the message has already been delivered to the panes
	// it is meant for, so that the active pane does not get it a second time.
	routed := false
	switch msg := msg.(type) {
	case RepoMsg:
		// Set the state to loading when we get a new repository.
		r.gen++
		r.selectedRepo = msg
		r.ref = nil
		cmds = append(cmds,
			r.Init(),
			// This will set the selected repo in each pane's model.
			r.updateModels(msg),
			updateRefCmd(r.refLoad.Start(r.common.Context()), msg, r.gen),
		)
		routed = true
	case RefMsg:
		r.ref = msg.Ref
		cmds = append(cmds, r.updateModels(msg))
		routed = true
		r.state = readyState
	case tabs.SelectTabMsg:
		r.activeTab = int(msg)
		cmds = append(cmds, r.tabs.Update(msg))
	case tabs.ActiveTabMsg:
		r.activeTab = int(msg)
	case tea.KeyPressMsg, tea.MouseClickMsg:
		cmds = append(cmds, r.tabs.Update(msg))
		if r.selectedRepo != nil {
			cmd := r.common.CloneCmd(r.common.Config().SSH.PublicURL, r.selectedRepo.Name())
			if msg, ok := msg.(tea.MouseMsg); ok && r.common.Zone.Get(r.zonePrefix+"url").InBounds(msg) {
				cmds = append(cmds, copyCmd(cmd, "Command copied to clipboard"))
			}
		}
		switch msg := msg.(type) {
		case tea.MouseClickMsg:
			switch msg.Button {
			case tea.MouseLeft:
				switch {
				case r.common.Zone.Get(r.statusbar.HelpZoneID()).InBounds(msg):
					cmds = append(cmds, footer.ToggleFooterCmd)
				}
			case tea.MouseRight:
				switch {
				case r.common.Zone.Get(r.zonePrefix + "main").InBounds(msg):
					cmds = append(cmds, goBackCmd)
				}
			}
		}
		switch msg := msg.(type) {
		case tea.KeyPressMsg:
			switch {
			case key.Matches(msg, r.common.KeyMap.Back):
				cmds = append(cmds, goBackCmd)
			}
		}
	case CopyMsg:
		txt := msg.Text
		if cfg := r.common.Config(); cfg != nil {
			cmds = append(cmds, tea.SetClipboard(txt))
		}
		r.statusbar.SetMessage(msg.Message)
	case errorMsg:
		routed = true
		if msg.to == paneRepo {
			// No reference of this repository could be loaded.
			r.state = failedState
		} else {
			// The tab that started the load leaves its loading view.
			cmds = append(cmds, r.updatePane(msg.to, msg))
		}
		err := msg.err
		cmds = append(cmds, func() tea.Msg { return common.ErrorMsg{Err: err} })
	case paneMsg:
		// A result goes to the tab that started the load, whichever tab is
		// active now.
		cmds = append(cmds, r.updatePane(msg.pane(), msg))
		routed = true
	case spinner.TickMsg:
		// The spinner is the page's own; no tab gets its ticks.
		routed = true
		if msg.ID == r.spinner.ID() {
			if r.busy() {
				var cmd tea.Cmd
				r.spinner, cmd = r.spinner.Update(msg)
				cmds = append(cmds, cmd)
			} else {
				r.ticking = false
			}
		}
	case tea.WindowSizeMsg:
		r.SetSize(msg.Width, msg.Height)
		cmds = append(cmds, r.updateModels(msg))
		routed = true
	case code.RenderedMsg:
		// A code view's render: the tab whose view it is takes it.
		cmds = append(cmds, r.updateModels(msg))
		routed = true
	case EmptyRepoMsg:
		r.ref = nil
		r.state = readyState
		cmds = append(cmds, r.updateModels(msg))
		routed = true
	case SwitchTabMsg:
		for i, p := range r.panes {
			if p.id() == paneID(msg) {
				cmds = append(cmds, tabs.SelectTabCmd(i))
				break
			}
		}
	}
	if !routed && r.state != failedState {
		active := r.panes[r.activeTab]
		cmds = append(cmds, active.Update(msg))
	}

	// Update the status bar on these events
	// Must come after we've updated the active tab
	switch msg.(type) {
	case RepoMsg, RefMsg, tabs.ActiveTabMsg, tea.KeyPressMsg,
		tea.MouseClickMsg, tea.MouseWheelMsg, FileItemsMsg, FileContentMsg,
		FileBlameMsg, selector.ActiveMsg, LogItemsMsg, GoBackMsg, LogDiffMsg,
		EmptyRepoMsg, StashListMsg, StashPatchMsg, diffRenderedMsg,
		code.RenderedMsg:
		r.setStatusBarInfo()
	}

	cmds = append(cmds, r.statusbar.Update(msg))

	// Something started loading: turn the spinner.
	if !r.ticking && r.busy() {
		r.ticking = true
		cmds = append(cmds, r.spinner.Tick)
	}

	return tea.Batch(cmds...)
}

// View implements tea.Model.
func (r *Repo) View() string {
	wm, hm := r.getMargins()
	hm += r.common.Styles.Tabs.GetHeight() +
		r.common.Styles.Tabs.GetVerticalFrameSize()
	s := r.common.Styles.Repo.Base.
		Width(r.common.Width - wm).
		Height(r.common.Height - hm)
	mainStyle := r.common.Styles.Repo.Body.
		Height(r.common.Height - hm)
	var main string
	var statusbar string
	switch r.state {
	case loadingState:
		main = fmt.Sprintf("%s loading…", r.spinner.View())
	case failedState:
		// Nothing of the repository to show; the error screen said why.
	case readyState:
		p := r.panes[r.activeTab]
		if text, ok := p.Loading(); ok && text != "" {
			// The same height the tab is given in SetSize.
			_, phm := r.getMargins()
			main = r.common.Styles.SpinnerContainer.
				Height(r.common.Height - phm).
				Render(r.spinner.View() + " " + text)
		} else {
			main = p.View()
		}
		statusbar = r.statusbar.View()
	}
	main = r.common.Zone.Mark(
		r.zonePrefix+"main",
		mainStyle.Render(main),
	)
	view := lipgloss.JoinVertical(lipgloss.Left,
		r.headerView(),
		r.tabs.View(),
		main,
		statusbar,
	)
	return s.Render(view)
}

func (r *Repo) headerView() string {
	if r.selectedRepo == nil {
		return ""
	}
	truncate := lipgloss.NewStyle().MaxWidth(r.common.Width)
	header := utils.Sanitize(r.selectedRepo.ProjectName())
	if header == "" {
		header = r.selectedRepo.Name()
	}
	header = r.common.Styles.Repo.HeaderName.Render(header) +
		" " + common.VisibilityMarker(r.selectedRepo.IsPrivate())
	desc := strings.TrimSpace(utils.Sanitize(r.selectedRepo.Description()))
	if desc != "" {
		header = lipgloss.JoinVertical(lipgloss.Left,
			header,
			r.common.Styles.Repo.HeaderDesc.Render(desc),
		)
	}
	urlStyle := r.common.Styles.URLStyle.
		Width(r.common.Width - lipgloss.Width(header) - 1).
		Align(lipgloss.Right)
	var url string
	if cfg := r.common.Config(); cfg != nil {
		url = r.common.CloneCmd(cfg.SSH.PublicURL, r.selectedRepo.Name())
	}
	url = common.TruncateString(url, r.common.Width-lipgloss.Width(header)-1)
	url = r.common.Zone.Mark(
		r.zonePrefix+"url",
		urlStyle.Render(url),
	)

	header = lipgloss.JoinHorizontal(lipgloss.Top, header, url)

	style := r.common.Styles.Repo.Header.Width(r.common.Width)
	return style.Render(
		truncate.Render(header),
	)
}

func (r *Repo) setStatusBarInfo() {
	if r.selectedRepo == nil {
		return
	}

	active := r.panes[r.activeTab]
	key := r.selectedRepo.Name()
	value := active.StatusBarValue()
	info := active.StatusBarInfo()
	extra := "*"
	if r.ref != nil {
		extra += " " + r.ref.Name().Short()
	}

	r.statusbar.SetStatus(key, value, info, extra)
}

// updatePane delivers msg to the tab id.
func (r *Repo) updatePane(id paneID, msg tea.Msg) tea.Cmd {
	for _, p := range r.panes {
		if p.id() == id {
			return p.Update(msg)
		}
	}
	return nil
}

func (r *Repo) updateModels(msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, 0)
	for _, b := range r.panes {
		cmds = append(cmds, b.Update(msg))
	}
	return tea.Batch(cmds...)
}

func copyCmd(text, msg string) tea.Cmd {
	return func() tea.Msg {
		return CopyMsg{
			Text:    text,
			Message: msg,
		}
	}
}

func goBackCmd() tea.Msg {
	return GoBackMsg{}
}

func switchTabCmd(id paneID) tea.Cmd {
	return func() tea.Msg {
		return SwitchTabMsg(id)
	}
}
