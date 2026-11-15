package repo

import (
	"context"
	"errors"
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
	// tooLarge is set when the README is larger than the size limit.
	tooLarge bool
	gen      uint64
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
	case EmptyRepoMsg:
		// No readme will be loaded for an empty repository, so stop
		// loading here and show the quick start text instead.
		r.load.Stop()
		r.gen = msg.gen
		r.isLoading = false
		r.readmePath = ""
		r.code.GotoTop()
		cmds = append(cmds,
			r.code.SetContent(defaultEmptyRepoMsg(r.common,
				r.repo.Name()), ".md"),
		)
	case errorMsg:
		// The view still holds the README of the ref shown before.
		r.isLoading = false
		r.readmePath = ""
		cmds = append(cmds, r.code.SetContent("", ""))
	case ReadmeMsg:
		r.isLoading = false
		r.readmePath = msg.Path
		r.code.GotoTop()
		if msg.tooLarge {
			cmds = append(cmds, r.code.SetContent(readmeTooLarge, ".md"))
			break
		}
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
			return errorMsg{err: common.ErrMissingRepo, to: paneReadme, gen: gen}
		}
		rm, rp, err := findReadme(ctx, repo, ref, limit)
		switch {
		case ctx.Err() != nil:
			// Cancelled while it ran: the readme may be missing because
			// of it.
			return nil
		case errors.Is(err, git.ErrFileTooLarge):
			m.tooLarge = true
		case err != nil:
			return loadError(ctx, paneReadme, gen, err)
		}
		m.Content = rm
		m.Path = rp
		return m
	}
}

// readmeTooLarge is shown in place of a README larger than the size limit.
const readmeTooLarge = "The README is larger than the size limit and is not shown."

// findReadme returns the README of ref and its path, searched for in the
// root, .github and docs, in that order, as backend.Readme does. It returns
// "" and no error when there is none, and an error wrapping
// git.ErrFileTooLarge when every README it found is larger than limit.
//
// It differs from backend.Readme in two ways. backend.Readme goes on to
// the next directory after any error and reports only the error of the last
// one it looked in, so it cannot tell a README too large to show from none;
// findReadme keeps the too-large error. And findReadme stops at the first
// error that is neither "too large" nor "not found", such as a git failure,
// and returns it, where backend.Readme would look on and could show a
// README from a later directory.
//
// TODO(tracker 287): make backend.Readme report a README too large to show
// and stop at other errors, then call it here instead.
func findReadme(ctx context.Context, repo proto.Repository, ref *git.Reference, limit int64) (string, string, error) {
	var tooLarge error
	for _, dir := range []string{".", ".github", "docs"} {
		// Tree paths always use forward slashes, whatever the OS.
		rm, rp, err := backend.LatestFile(ctx, repo, ref, path.Join(dir, "[rR][eE][aA][dD][mM][eE]*"), limit)
		switch {
		case err == nil:
			return rm, rp, nil
		case errors.Is(err, git.ErrFileTooLarge):
			if tooLarge == nil {
				tooLarge = err
			}
		case errors.Is(err, git.ErrFileNotFound), errors.Is(err, git.ErrRevisionNotExist):
			// No README here, or no such directory.
		default:
			// A git failure: report it rather than look on.
			return "", "", err
		}
	}
	return "", "", tooLarge
}
