package browse

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/internal/termtext"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/footer"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/repo"
	"github.com/spf13/cobra"
)

// Command is the browse command.
var Command = &cobra.Command{
	Use:   "browse PATH",
	Short: "Browse a repository",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		rp := "."
		if len(args) > 0 {
			rp = args[0]
		}

		abs, err := filepath.Abs(rp)
		if err != nil {
			return err
		}

		r, err := git.OpenLocal(cmd.Context(), abs)
		if err != nil {
			return fmt.Errorf("failed to open repository: %w", err)
		}

		m, err := newModel(cmd.Context(), r)
		if err != nil {
			return err
		}
		// Stop the bubblezone worker once the program has exited, and not
		// on quit: the program renders once more after the model quits.
		defer m.common.Zone.Close()
		p := tea.NewProgram(m)

		_, err = p.Run()
		return err
	},
}

// newModel returns the browse model for the repository r.
func newModel(ctx context.Context, r *git.Repository) (*model, error) {
	// soft browse runs without a server: there is no server config to
	// clone from, so the clone command is hidden, and no backend.
	c, err := common.New(ctx, &config.Config{}, nil, nil)
	if err != nil {
		return nil, err
	}
	c.HideCloneCmd = true
	comps := []repo.Pane{
		repo.NewReadme(c),
		repo.NewFiles(c),
		repo.NewLog(c),
	}
	if !r.IsBare {
		comps = append(comps, repo.NewStash(c))
	}
	comps = append(comps, repo.NewRefs(c, git.RefsHeads), repo.NewRefs(c, git.RefsTags))
	rm := repo.New(c, comps...)
	rm.SetNoMenu(true)
	m := &model{
		model:  rm,
		repo:   repository{r: r},
		common: c,
	}
	m.footer = footer.New(c, m)
	return m, nil
}

type state int

const (
	startState state = iota
	errorState
)

type model struct {
	model      *repo.Repo
	footer     *footer.Footer
	repo       repo.Repository
	common     *common.Common
	width      int
	height     int
	state      state
	showFooter bool
	error      error
}

var _ tea.Model = &model{}

func (m *model) SetSize(w, h int) {
	m.width, m.height = common.ClampSize(w, h)
	style := m.common.Styles.App
	wm := style.GetHorizontalFrameSize()
	hm := style.GetVerticalFrameSize()
	if m.showFooter {
		hm += m.footer.Height()
	}

	m.footer.SetSize(w-wm, h-hm)
	m.model.SetSize(w-wm, h-hm)
}

// ShortHelp implements help.KeyMap.
func (m *model) ShortHelp() []key.Binding {
	switch m.state {
	case errorState:
		return []key.Binding{
			m.common.KeyMap.Back,
			m.common.KeyMap.Quit,
			m.common.KeyMap.Help,
		}
	default:
		b := m.model.ShortHelp()
		return append(b, m.common.KeyMap.Quit, m.common.KeyMap.Help)
	}
}

// FullHelp implements help.KeyMap.
func (m *model) FullHelp() [][]key.Binding {
	switch m.state {
	case errorState:
		return [][]key.Binding{
			{
				m.common.KeyMap.Back,
			},
			{
				m.common.KeyMap.Quit,
				m.common.KeyMap.Help,
			},
		}
	default:
		b := m.model.FullHelp()
		return append(b, []key.Binding{
			m.common.KeyMap.Help,
			m.common.KeyMap.Quit,
		})
	}
}

// Init implements tea.Model.
func (m *model) Init() tea.Cmd {
	return tea.Batch(
		m.model.Init(),
		m.footer.Init(),
		// The repository page loads the ref to show itself.
		func() tea.Msg {
			return repo.OpenedMsg(m.repo)
		},
	)
}

// Update implements tea.Model.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.common.Logger.Debugf("msg received: %T", msg)
	cmds := make([]tea.Cmd, 0)
	// While an error is shown the repository view underneath is hidden, so
	// input belongs to the error alone: the key that dismisses it must not
	// also act on the view.
	forwardInput := m.state != errorState
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, m.common.KeyMap.Back) && m.error != nil:
			m.error = nil
			m.state = startState
			// Show the footer only when all help is shown.
			m.showFooter = m.footer.ShowAll()
		case key.Matches(msg, m.common.KeyMap.Help):
			cmds = append(cmds, footer.ToggleFooterCmd)
		case key.Matches(msg, m.common.KeyMap.Quit):
			return m, tea.Quit
		}
	case tea.MouseClickMsg:
		mouse := msg.Mouse()
		switch mouse.Button {
		case tea.MouseLeft:
			switch {
			case m.common.Zone.Get(m.footer.ZoneID()).InBounds(msg):
				cmds = append(cmds, footer.ToggleFooterCmd)
			}
		}
	case footer.ToggleFooterMsg:
		m.footer.SetShowAll(!m.footer.ShowAll())
		m.showFooter = !m.showFooter
		// Size the repository page for the footer now, before it gets the
		// message: the Commits tab loads its page by the size it has then.
		m.SetSize(m.width, m.height)
	case common.ErrorMsg:
		m.error = msg.Err
		if m.error == nil {
			m.error = errors.New("unknown error")
		}
		m.state = errorState
		m.showFooter = true
	}

	cmds = append(cmds, m.footer.Update(msg))

	if forwardInput || !common.IsInputMsg(msg) {
		cmds = append(cmds, m.model.Update(msg))
	}

	// This fixes determining the height margin of the footer.
	m.SetSize(m.width, m.height)

	return m, tea.Batch(cmds...)
}

// View implements tea.Model.
func (m *model) View() tea.View {
	var v tea.View
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion

	style := m.common.Styles.App
	wm, hm := style.GetHorizontalFrameSize(), style.GetVerticalFrameSize()
	if m.showFooter {
		hm += m.footer.Height()
	}

	var view string
	switch m.state {
	case startState:
		view = m.model.View()
	case errorState:
		err := m.common.Styles.ErrorTitle.Render("Bummer")
		// A git error can carry bytes from the repository.
		err += m.common.Styles.ErrorBody.Render(termtext.Text(m.error.Error()))
		view = m.common.Styles.Error.
			Width(m.width -
				wm -
				m.common.Styles.ErrorBody.GetHorizontalFrameSize()).
			Height(m.height -
				hm -
				m.common.Styles.Error.GetVerticalFrameSize()).
			Render(err)
	}

	if m.showFooter {
		view = lipgloss.JoinVertical(lipgloss.Left, view, m.footer.View())
	}

	v.Content = m.common.Zone.Scan(style.Render(view))
	return v
}

// repository is the repository soft browse shows.
type repository struct {
	r *git.Repository
}

// Description implements repo.Repository.
func (r repository) Description() string {
	return ""
}

// IsPrivate implements repo.Repository.
func (repository) IsPrivate() bool {
	return false
}

// Name implements repo.Repository.
func (r repository) Name() string {
	return filepath.Base(r.r.Path)
}

// Open implements repo.Repository. It opens the repository again, bound
// to ctx, the context of the UI load that asks for it.
func (r repository) Open(ctx context.Context) (*git.Repository, error) {
	return git.OpenLocal(ctx, r.r.Path)
}

// ProjectName implements repo.Repository.
func (r repository) ProjectName() string {
	return r.Name()
}
