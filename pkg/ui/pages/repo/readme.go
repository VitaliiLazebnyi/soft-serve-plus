package repo

import (
	"path"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/code"
)

// ReadmeMsg is a message sent when the readme is loaded.
type ReadmeMsg struct {
	Content string
	Path    string
	gen     uint64
}

func (m ReadmeMsg) generation() uint64 { return m.gen }

func (m ReadmeMsg) pane() paneID { return paneReadme }

// Readme is the readme component page.
type Readme struct {
	common     common.Common
	code       *code.Code
	ref        *git.Reference
	repo       proto.Repository
	readmePath string
	isLoading  bool
	gen        uint64
	// load is the readme load in flight: a new one cancels it.
	load common.Loader
}

// NewReadme creates a new readme model.
func NewReadme(common common.Common) *Readme {
	readme := code.New(common, "", "")
	readme.NoContentStyle = readme.NoContentStyle.SetString("No readme found.")
	readme.UseGlamour = true
	return &Readme{
		code:      readme,
		common:    common,
		isLoading: true,
	}
}

// Path implements Pane.
func (r *Readme) Path() string {
	return ""
}

// TabName returns the name of the tab.
func (r *Readme) TabName() string {
	return "Readme"
}

// SetSize implements common.Component.
func (r *Readme) SetSize(width, height int) {
	r.common.SetSize(width, height)
	r.code.SetSize(width, height)
}

// ShortHelp implements help.KeyMap.
func (r *Readme) ShortHelp() []key.Binding {
	b := []key.Binding{
		r.common.KeyMap.UpDown,
	}
	return b
}

// FullHelp implements help.KeyMap.
func (r *Readme) FullHelp() [][]key.Binding {
	k := r.code.KeyMap
	b := [][]key.Binding{
		{
			k.PageDown,
			k.PageUp,
			k.HalfPageDown,
			k.HalfPageUp,
		},
		{
			k.Down,
			k.Up,
			r.common.KeyMap.GotoTop,
			r.common.KeyMap.GotoBottom,
		},
	}
	return b
}

// Init implements tea.Model.
func (r *Readme) Init() tea.Cmd {
	r.isLoading = true
	return r.updateReadmeCmd()
}

// Update implements common.Model.
func (r *Readme) Update(msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, 0)
	switch msg := msg.(type) {
	case RepoMsg:
		r.repo = msg
	case RefMsg:
		r.ref = msg.Ref
		r.gen = msg.gen
		cmds = append(cmds, r.Init())
	case tea.WindowSizeMsg:
		r.SetSize(msg.Width, msg.Height)
	case EmptyRepoMsg:
		// No readme will be loaded for an empty repository, so stop
		// loading here and show the quick start text instead.
		r.gen = msg.gen
		r.isLoading = false
		r.readmePath = ""
		r.code.GotoTop()
		cmds = append(cmds,
			r.code.SetContent(defaultEmptyRepoMsg(r.common,
				r.repo.Name()), ".md"),
		)
	case ReadmeMsg:
		r.isLoading = false
		r.readmePath = msg.Path
		r.code.GotoTop()
		// The code view sanitises the README, which comes from the
		// repository, when it renders it.
		cmds = append(cmds, r.code.SetContent(msg.Content, msg.Path))
	}
	cmds = append(cmds, r.code.Update(msg))
	return tea.Batch(cmds...)
}

// View implements tea.Model.
func (r *Readme) View() string {
	return r.code.View()
}

// Loading implements Pane.
func (r *Readme) Loading() (string, bool) {
	return "loading…", r.isLoading || r.code.Rendering()
}

func (r *Readme) id() paneID { return paneReadme }

// StatusBarValue implements statusbar.StatusBar.
func (r *Readme) StatusBarValue() string {
	dir := path.Dir(r.readmePath)
	if dir == "." || dir == "" {
		return " "
	}
	return dir
}

// StatusBarInfo implements statusbar.StatusBar.
func (r *Readme) StatusBarInfo() string {
	return common.ScrollPercent(r.code.ScrollPosition())
}

// updateReadmeCmd returns a command that loads the readme of the current ref.
// Commands run in their own goroutines, so it captures the model state it
// needs here instead of reading the model later.
func (r *Readme) updateReadmeCmd() tea.Cmd {
	repo, ref, gen := r.repo, r.ref, r.gen
	ctx := r.load.Start(r.common.Context())
	limit := r.common.Config().BlobSizeLimit()
	return func() tea.Msg {
		if ctx.Err() != nil {
			// A newer load replaced this one before it started.
			return nil
		}
		m := ReadmeMsg{gen: gen}
		if repo == nil {
			return common.ErrorMsg(common.ErrMissingRepo)
		}
		rm, rp, _ := backend.Readme(ctx, repo, ref, limit)
		if ctx.Err() != nil {
			// Cancelled while it ran: the readme may be missing because
			// of it.
			return nil
		}
		m.Content = rm
		m.Path = rp
		return m
	}
}
