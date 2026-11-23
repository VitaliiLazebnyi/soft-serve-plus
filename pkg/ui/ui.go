package ui

import (
	"context"
	"errors"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/code"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/footer"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/repo"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/selection"
)

// errUnknown stands for an error reported without one.
var errUnknown = errors.New("unknown error")

type page int

const (
	selectionPage page = iota
	repoPage
)

type sessionState int

const (
	loadingState sessionState = iota
	errorState
	readyState
)

// Model is the TUI's root model: the repository list and the repository
// pages of one SSH session.
type Model struct {
	serverName  string
	initialRepo string
	common      *common.Common
	width       int
	height      int
	pages       []common.Component
	activePage  page
	state       sessionState
	footer      *footer.Footer
	showFooter  bool
	error       error
	// repoGen counts repository opens. Each open records the value it
	// started with, and only the newest open is applied.
	repoGen uint64
}

// repoOpenedMsg is the result of opening a repository the user picked (or the
// initial repository). gen is the repoGen the open started with.
type repoOpenedMsg struct {
	gen  uint64
	repo proto.Repository
	err  error
}

// New returns a new Model of width x height cells, limited by
// common.ClampSize.
func New(c *common.Common, width, height int, initialRepo string) *Model {
	width, height = common.ClampSize(width, height)
	ui := &Model{
		serverName:  c.Config.Name,
		width:       width,
		height:      height,
		common:      c,
		pages:       make([]common.Component, 2), // selection & repo
		activePage:  selectionPage,
		state:       loadingState,
		initialRepo: initialRepo,
		showFooter:  true,
	}
	ui.footer = footer.New(c, ui)
	return ui
}

// Common returns the Common the model's components share. Its Zone
// manager must be closed once the program has exited.
func (ui *Model) Common() *common.Common {
	return ui.common
}

// Size returns the model's width and height in cells.
func (ui *Model) Size() (width, height int) {
	return ui.width, ui.height
}

func (ui *Model) getMargins() (wm, hm int) {
	style := ui.common.Styles.App
	switch ui.activePage {
	case selectionPage:
		hm += ui.common.Styles.ServerName.GetHeight() +
			ui.common.Styles.ServerName.GetVerticalFrameSize()
	case repoPage:
	}
	wm += style.GetHorizontalFrameSize()
	hm += style.GetVerticalFrameSize()
	if ui.showFooter {
		// NOTE: we don't use the footer's style to determine the margins
		// because footer.Height() is the height of the footer after applying
		// the styles.
		hm += ui.footer.Height()
	}
	return
}

// ShortHelp implements help.KeyMap.
func (ui *Model) ShortHelp() []key.Binding {
	b := make([]key.Binding, 0)
	switch ui.state {
	case errorState:
		b = append(b, ui.common.KeyMap.Back)
	case readyState:
		b = append(b, ui.pages[ui.activePage].ShortHelp()...)
	}
	if !ui.IsFiltering() {
		b = append(b, ui.common.KeyMap.Quit, ui.common.KeyMap.Help)
	}
	return b
}

// FullHelp implements help.KeyMap.
func (ui *Model) FullHelp() [][]key.Binding {
	b := make([][]key.Binding, 0)
	switch ui.state {
	case errorState:
		b = append(b, []key.Binding{ui.common.KeyMap.Back})
	case readyState:
		b = append(b, ui.pages[ui.activePage].FullHelp()...)
	}
	if !ui.IsFiltering() {
		b = append(b, []key.Binding{
			ui.common.KeyMap.Help,
			ui.common.KeyMap.Quit,
		})
	}
	return b
}

// SetSize implements common.Component.
func (ui *Model) SetSize(width, height int) {
	ui.width, ui.height = common.ClampSize(width, height)
	wm, hm := ui.getMargins()
	ui.footer.SetSize(width-wm, height-hm)
	for _, p := range ui.pages {
		if p != nil {
			p.SetSize(width-wm, height-hm)
		}
	}
}

// Init implements tea.Model.
func (ui *Model) Init() tea.Cmd {
	ui.pages[selectionPage] = selection.New(ui.common)
	ui.pages[repoPage] = repo.New(ui.common,
		repo.NewReadme(ui.common),
		repo.NewFiles(ui.common),
		repo.NewLog(ui.common),
		repo.NewRefs(ui.common, git.RefsHeads),
		repo.NewRefs(ui.common, git.RefsTags),
	)
	ui.SetSize(ui.width, ui.height)
	cmds := make([]tea.Cmd, 0)
	cmds = append(cmds,
		ui.pages[selectionPage].Init(),
		ui.pages[repoPage].Init(),
	)
	if ui.initialRepo != "" {
		// A deep link to a repository that is missing, or that the caller
		// cannot read, shows the same error as picking one from the list.
		cmds = append(cmds, ui.openRepoCmd(ui.initialRepo))
	}
	ui.state = readyState
	ui.SetSize(ui.width, ui.height)
	return tea.Batch(cmds...)
}

// IsFiltering returns true if the selection page is filtering.
func (ui *Model) IsFiltering() bool {
	if ui.activePage == selectionPage {
		if s, ok := ui.pages[selectionPage].(interface{ IsFiltering() bool }); ok {
			return s.IsFiltering()
		}
	}
	return false
}

// Update implements tea.Model.
func (ui *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	ui.common.Logger.Debugf("msg received: %T", msg)
	if m, ok := msg.(repoOpenedMsg); ok {
		// Only the newest open counts. An older one finished late, for
		// example after the user picked another repository.
		if m.gen != ui.repoGen {
			return ui, nil
		}
		// The initial repository and a picked one report a failed open
		// the same way.
		if m.err != nil {
			msg = common.ErrorMsg{Err: m.err}
		} else {
			msg = repo.OpenedMsg(m.repo)
		}
	}
	cmds := make([]tea.Cmd, 0)
	// While an error is shown the page underneath is hidden, so input
	// belongs to the error alone: the key that dismisses it must not also
	// act on the page.
	forwardInput := ui.state != errorState
	switch msg := msg.(type) {
	case tea.WindowSizeMsg, code.RenderedMsg:
		if msg, ok := msg.(tea.WindowSizeMsg); ok {
			ui.SetSize(msg.Width, msg.Height)
		}
		// A render goes to the page whose code view started it, which may
		// not be the active one.
		for _, p := range ui.pages {
			cmds = append(cmds, p.Update(msg))
		}
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, ui.common.KeyMap.Back) && ui.error != nil:
			ui.error = nil
			ui.state = readyState
			// Always show the footer on the selection page; on the repo page
			// only when all help is shown.
			ui.showFooter = ui.activePage == selectionPage || ui.footer.ShowAll()
		case key.Matches(msg, ui.common.KeyMap.Help) && !ui.IsFiltering():
			cmds = append(cmds, footer.ToggleFooterCmd)
		case key.Matches(msg, ui.common.KeyMap.Quit) &&
			(!ui.IsFiltering() || msg.String() == "ctrl+c"):
			// While filtering, "q" is typed into the filter but ctrl+c
			// still quits.
			return ui, tea.Quit
		case ui.activePage == repoPage &&
			ui.pages[ui.activePage].(*repo.Repo).Path() == "" &&
			key.Matches(msg, ui.common.KeyMap.Back):
			ui.activePage = selectionPage
			// Always show the footer on selection page.
			ui.showFooter = true
		}
	case tea.MouseClickMsg:
		switch msg.Button {
		case tea.MouseLeft:
			switch {
			case ui.common.Zone.Get(ui.footer.ZoneID()).InBounds(msg):
				cmds = append(cmds, footer.ToggleFooterCmd)
			}
		}
	case footer.ToggleFooterMsg:
		ui.footer.SetShowAll(!ui.footer.ShowAll())
		// Show the footer when on repo page and shot all help.
		if ui.error == nil && ui.activePage == repoPage {
			ui.showFooter = !ui.showFooter
		}
		// Size the pages for the footer now, before they get the message:
		// the Commits tab loads its page by the size it has then.
		ui.SetSize(ui.width, ui.height)
	case repo.RenderedMsg:
		// A render a tab started, which it waits for to start its next
		// one, even after the user left the repository page.
		cmds = append(cmds, ui.pages[repoPage].Update(msg))
	case repo.OpenedMsg:
		ui.activePage = repoPage
		// Show the footer on repo page if show all is set.
		ui.showFooter = ui.footer.ShowAll()
		// The repository page loads the ref to show itself.
	case common.ErrorMsg:
		err := msg.Err
		if err == nil {
			err = errUnknown
		}
		if !common.IsUserError(err) {
			// The error screen shows a generic message for this error, which
			// may carry git's output or paths on disk, so log it in full.
			ui.common.Logger.Error("ui error", "err", err)
		}
		ui.error = err
		ui.state = errorState
		ui.showFooter = true
	case selection.ReposMsg:
		// The list loads in the background and may arrive after an
		// initial repository opened, so it goes to the selection page
		// whichever page is active.
		if ui.activePage != selectionPage {
			cmds = append(cmds, ui.pages[selectionPage].Update(msg))
		}
	case selector.SelectMsg:
		switch msg.IdentifiableItem.(type) {
		case selection.Item:
			if ui.activePage == selectionPage {
				cmds = append(cmds, ui.openRepoCmd(msg.ID()))
			}
		}
	}
	cmds = append(cmds, ui.footer.Update(msg))
	// A window size message or a code view render has already been
	// delivered to every page above, and a repository render to the
	// repository page.
	_, resized := msg.(tea.WindowSizeMsg)
	_, codeRendered := msg.(code.RenderedMsg)
	_, repoRendered := msg.(repo.RenderedMsg)
	rendered := codeRendered || repoRendered
	if !resized && !rendered && ui.state != loadingState && (forwardInput || !common.IsInputMsg(msg)) {
		cmds = append(cmds, ui.pages[ui.activePage].Update(msg))
	}
	// This fixes determining the height margin of the footer.
	ui.SetSize(ui.width, ui.height)
	return ui, tea.Batch(cmds...)
}

// View implements tea.Model.
func (ui *Model) View() tea.View {
	var v tea.View
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion

	var view string
	wm, hm := ui.getMargins()
	switch ui.state {
	case loadingState:
		view = "Loading..."
	case errorState:
		err := ui.common.Styles.ErrorTitle.Render("Bummer")
		err += ui.common.Styles.ErrorBody.Render(common.ErrorMessage(ui.error))
		view = ui.common.Styles.Error.
			Width(ui.width -
				wm -
				ui.common.Styles.ErrorBody.GetHorizontalFrameSize()).
			Height(ui.height -
				hm -
				ui.common.Styles.Error.GetVerticalFrameSize()).
			Render(err)
	case readyState:
		view = ui.pages[ui.activePage].View()
	default:
		view = "Unknown state :/ this is a bug!"
	}
	if ui.activePage == selectionPage {
		view = lipgloss.JoinVertical(lipgloss.Left,
			ui.common.Styles.ServerName.Render(strings.TrimSpace(ui.serverName)),
			view)
	}
	if ui.showFooter {
		view = lipgloss.JoinVertical(lipgloss.Left, view, ui.footer.View())
	}
	v.Content = ui.common.Zone.Scan(
		ui.common.Styles.App.Render(view),
	)
	return v
}

// openRepoCmd starts opening repository rn. It must be called from Init or
// Update: it takes the next repoGen there, and the command reads only what
// does not change, never the UI.
func (ui *Model) openRepoCmd(rn string) tea.Cmd {
	ui.repoGen++
	gen := ui.repoGen
	ctx := ui.common.Context()
	be := ui.common.Backend
	user := proto.UserFromContext(ctx)
	return func() tea.Msg {
		r, err := openRepo(ctx, be, user, rn)
		return repoOpenedMsg{gen: gen, repo: r, err: err}
	}
}

// openRepo opens repository rn for user, the session user. An admin key on
// ctx still opens every repository.
func openRepo(ctx context.Context, be *backend.Backend, user proto.User, rn string) (proto.Repository, error) {
	// A repository the caller cannot read is reported as missing, so a
	// private repository and a missing one behave the same. The user is
	// the one AuthenticationMiddleware put on the session context, so
	// opening a repository does not look the key up again; an admin key
	// on the context still gets admin access.
	r, _, err := be.ReadableForUser(ctx, rn, user)
	if err != nil {
		// Not a missing repository: the error screen shows a generic
		// message for it and keeps the cause out of the session.
		return nil, err
	}
	if r == nil {
		return nil, common.ErrMissingRepo
	}
	return r, nil
}
