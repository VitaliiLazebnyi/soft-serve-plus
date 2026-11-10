package repo

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/code"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/utils"
)

type filesView int

const (
	filesViewLoading filesView = iota
	filesViewFiles
	filesViewContent
)

var (
	errNoFileSelected = errors.New("no file selected")
	errBinaryFile     = errors.New("binary file")
	errInvalidFile    = errors.New("invalid file")
)

var (
	lineNo = key.NewBinding(
		key.WithKeys("l"),
		key.WithHelp("l", "toggle line numbers"),
	)
	blameView = key.NewBinding(
		key.WithKeys("b"),
		key.WithHelp("b", "toggle blame view"),
	)
	preview = key.NewBinding(
		key.WithKeys("p"),
		key.WithHelp("p", "toggle preview"),
	)
)

// FileItemsMsg is a message that contains a list of files.
type FileItemsMsg []selector.IdentifiableItem

// FileContentMsg is a message that contains the content of a file.
type FileContentMsg struct {
	content string
	ext     string
	// path is the path of the file and index is the position of the file in
	// its directory listing.
	path  string
	index int
	// load is the Files load the content was requested in.
	load int
}

// FileBlameMsg is a message that contains the blame of a file.
type FileBlameMsg struct {
	blame *git.Blame
	// load is the Files load the blame was requested in.
	load int
}

// Files is the model for the files view.
type Files struct {
	common         common.Common
	selector       *selector.Selector
	ref            *git.Reference
	activeView     filesView
	repo           proto.Repository
	code           *code.Code
	path           string
	currentItem    *FileItem
	currentContent FileContentMsg
	currentBlame   *git.Blame
	lastSelected   []int
	lineNumber     bool
	spinner        spinner.Model
	cursor         int
	blameView      bool
	// load counts the file and blame loads. A FileContentMsg or FileBlameMsg
	// from an earlier load arrived after the user or a ref change moved on,
	// and is dropped.
	load int
}

// NewFiles creates a new files model.
func NewFiles(common common.Common) *Files {
	f := &Files{
		common:       common,
		code:         code.New(common, "", ""),
		activeView:   filesViewLoading,
		lastSelected: make([]int, 0),
		lineNumber:   true,
	}
	selector := selector.New(common, []selector.IdentifiableItem{}, FileItemDelegate{&common})
	selector.SetShowFilter(false)
	selector.SetShowHelp(false)
	selector.SetShowPagination(false)
	selector.SetShowStatusBar(false)
	selector.SetShowTitle(false)
	selector.SetFilteringEnabled(false)
	selector.DisableQuitKeybindings()
	selector.KeyMap.NextPage = common.KeyMap.NextPage
	selector.KeyMap.PrevPage = common.KeyMap.PrevPage
	f.selector = selector
	f.code.ShowLineNumber = f.lineNumber
	// "b" toggles blame and "l" toggles line numbers in the content view, so
	// they must not also page up or scroll right in the viewport.
	f.code.KeyMap.PageUp = key.NewBinding(
		key.WithKeys("pgup"),
		key.WithHelp("pgup", "page up"),
	)
	f.code.KeyMap.Right = key.NewBinding(
		key.WithKeys("right"),
		key.WithHelp("→", "move right"),
	)
	s := spinner.New(spinner.WithSpinner(spinner.Dot),
		spinner.WithStyle(common.Styles.Spinner))
	f.spinner = s
	return f
}

// Path implements common.TabComponent.
func (f *Files) Path() string {
	path := f.path
	if path == "." {
		return ""
	}
	return path
}

// TabName returns the tab name.
func (f *Files) TabName() string {
	return "Files"
}

// SetSize implements common.Component.
func (f *Files) SetSize(width, height int) {
	f.common.SetSize(width, height)
	f.selector.SetSize(width, height)
	f.code.SetSize(width, height)
}

// ShortHelp implements help.KeyMap.
func (f *Files) ShortHelp() []key.Binding {
	k := f.selector.KeyMap
	switch f.activeView {
	case filesViewFiles:
		return []key.Binding{
			f.common.KeyMap.SelectItem,
			f.common.KeyMap.BackItem,
			k.CursorUp,
			k.CursorDown,
		}
	case filesViewContent:
		b := []key.Binding{
			f.common.KeyMap.UpDown,
			f.common.KeyMap.BackItem,
		}
		return b
	default:
		return []key.Binding{}
	}
}

// FullHelp implements help.KeyMap.
func (f *Files) FullHelp() [][]key.Binding {
	b := make([][]key.Binding, 0)
	copyKey := f.common.KeyMap.Copy
	actionKeys := []key.Binding{}
	switch f.activeView {
	case filesViewFiles:
		copyKey.SetHelp("c", "copy name")
		k := f.selector.KeyMap
		b = append(b, [][]key.Binding{
			{
				f.common.KeyMap.SelectItem,
				f.common.KeyMap.BackItem,
			},
			{
				k.CursorUp,
				k.CursorDown,
				k.NextPage,
				k.PrevPage,
			},
			{
				k.GoToStart,
				k.GoToEnd,
			},
		}...)
	case filesViewContent:
		if !f.code.UseGlamour {
			actionKeys = append(actionKeys, lineNo)
		}
		actionKeys = append(actionKeys, blameView)
		if common.IsFileMarkdown(f.currentContent.content, f.currentContent.ext) &&
			!f.blameView {
			actionKeys = append(actionKeys, preview)
		}
		copyKey.SetHelp("c", "copy content")
		k := f.code.KeyMap
		b = append(b, []key.Binding{
			f.common.KeyMap.BackItem,
		})
		b = append(b, [][]key.Binding{
			{
				k.PageDown,
				k.PageUp,
				k.HalfPageDown,
				k.HalfPageUp,
			},
			{
				k.Down,
				k.Up,
				f.common.KeyMap.GotoTop,
				f.common.KeyMap.GotoBottom,
			},
		}...)
	}
	actionKeys = append([]key.Binding{
		copyKey,
	}, actionKeys...)
	return append(b, actionKeys)
}

// Init implements tea.Model.
func (f *Files) Init() tea.Cmd {
	f.path = ""
	f.currentItem = nil
	f.activeView = filesViewLoading
	f.lastSelected = make([]int, 0)
	f.blameView = false
	f.currentBlame = nil
	f.code.UseGlamour = false
	f.load++
	return tea.Batch(f.spinner.Tick, f.updateFilesCmd())
}

// Update implements tea.Model.
func (f *Files) Update(msg tea.Msg) (common.Model, tea.Cmd) {
	cmds := make([]tea.Cmd, 0)
	switch msg := msg.(type) {
	case RepoMsg:
		f.repo = msg
	case RefMsg:
		f.ref = msg
		f.selector.Select(0)
		cmds = append(cmds, f.Init())
	case FileItemsMsg:
		cmds = append(cmds,
			f.selector.SetItems(msg),
		)
		f.activeView = filesViewFiles
		if f.cursor >= 0 {
			f.selector.Select(f.cursor)
			f.cursor = -1
		}
	case FileContentMsg:
		if msg.load != f.load {
			break
		}
		// Start a new load so that a repeated delivery of this message is
		// dropped and the file's position is pushed only once.
		f.load++
		f.path = msg.path
		f.lastSelected = append(f.lastSelected, msg.index)
		f.activeView = filesViewContent
		f.currentContent = msg
		f.code.UseGlamour = common.IsFileMarkdown(f.currentContent.content, f.currentContent.ext)
		// File contents come from the repository and must not reach the
		// terminal as escape sequences; chroma and glamour pass them
		// through. currentContent keeps the raw bytes for copying.
		cmds = append(cmds, f.code.SetContent(utils.Sanitize(msg.content), msg.ext))
		f.code.GotoTop()
	case FileBlameMsg:
		if msg.load != f.load || !f.blameView {
			break
		}
		f.currentBlame = msg.blame
		f.activeView = filesViewContent
		f.code.UseGlamour = false
		f.code.SetSideNote(renderBlame(f.common, f.currentItem, msg.blame))
	case selector.SelectMsg:
		switch sel := msg.IdentifiableItem.(type) {
		case FileItem:
			f.load++
			f.currentItem = &sel
			p := path.Join(f.path, sel.entry.Name())
			if sel.entry.IsTree() {
				f.path = p
				f.lastSelected = append(f.lastSelected, f.selector.Index())
				f.cursor = 0
				cmds = append(cmds, f.updateFilesCmd())
			} else {
				cmds = append(cmds, f.selectFileCmd(sel, p, f.selector.Index()))
			}
		}
	case GoBackMsg:
		switch f.activeView {
		case filesViewFiles, filesViewContent:
			cmds = append(cmds, f.deselectItemCmd())
		}
	case tea.KeyPressMsg:
		switch f.activeView {
		case filesViewFiles:
			switch {
			case key.Matches(msg, f.common.KeyMap.SelectItem):
				cmds = append(cmds, f.selector.SelectItemCmd())
			case key.Matches(msg, f.common.KeyMap.BackItem):
				cmds = append(cmds, f.deselectItemCmd())
			}
		case filesViewContent:
			switch {
			case key.Matches(msg, f.common.KeyMap.BackItem):
				cmds = append(cmds, f.deselectItemCmd())
			case key.Matches(msg, f.common.KeyMap.Copy):
				cmds = append(cmds, copyCmd(f.currentContent.content, "File contents copied to clipboard"))
			case key.Matches(msg, lineNo) && !f.code.UseGlamour:
				f.lineNumber = !f.lineNumber
				f.code.ShowLineNumber = f.lineNumber
				cmds = append(cmds, f.code.SetContent(utils.Sanitize(f.currentContent.content), f.currentContent.ext))
			case key.Matches(msg, blameView):
				f.activeView = filesViewLoading
				f.blameView = !f.blameView
				f.load++
				if f.blameView {
					cmds = append(cmds, f.fetchBlameCmd())
				} else {
					f.activeView = filesViewContent
					cmds = append(cmds, f.code.SetSideNote(""))
				}
				cmds = append(cmds, f.spinner.Tick)
			case key.Matches(msg, preview) &&
				common.IsFileMarkdown(f.currentContent.content, f.currentContent.ext) && !f.blameView:
				f.code.UseGlamour = !f.code.UseGlamour
				cmds = append(cmds, f.code.SetContent(utils.Sanitize(f.currentContent.content), f.currentContent.ext))
			}
		}
	case tea.WindowSizeMsg:
		f.SetSize(msg.Width, msg.Height)
		// The content view re-renders through f.code.Update below.
		if f.activeView == filesViewFiles && f.repo != nil {
			cmds = append(cmds, f.updateFilesCmd())
		}
	case common.ErrorMsg:
		// A listing or blame that failed to load never sends its result, so
		// leave the loading view rather than spin until the next reload.
		if f.activeView == filesViewLoading {
			if f.blameView {
				f.blameView = false
				f.activeView = filesViewContent
			} else {
				// The selector still holds the previous listing, maybe of
				// another ref or repository. Clear it now, not through a
				// FileItemsMsg that could land after a later good listing.
				f.activeView = filesViewFiles
				cmds = append(cmds, f.selector.SetItems(nil))
			}
		}
	case EmptyRepoMsg:
		f.ref = nil
		f.load++
		f.path = ""
		f.currentItem = nil
		f.activeView = filesViewFiles
		f.lastSelected = make([]int, 0)
		f.selector.Select(0)
		cmds = append(cmds, f.setItems([]selector.IdentifiableItem{}))
	case spinner.TickMsg:
		if f.activeView == filesViewLoading && f.spinner.ID() == msg.ID {
			s, cmd := f.spinner.Update(msg)
			f.spinner = s
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}
	switch f.activeView {
	case filesViewFiles:
		m, cmd := f.selector.Update(msg)
		f.selector = m.(*selector.Selector)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	case filesViewContent:
		m, cmd := f.code.Update(msg)
		f.code = m.(*code.Code)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return f, tea.Batch(cmds...)
}

// View implements tea.Model.
func (f *Files) View() string {
	switch f.activeView {
	case filesViewLoading:
		return renderLoading(f.common, f.spinner)
	case filesViewFiles:
		return f.selector.View()
	case filesViewContent:
		return f.code.View()
	default:
		return ""
	}
}

// SpinnerID implements common.TabComponent.
func (f *Files) SpinnerID() int {
	return f.spinner.ID()
}

// StatusBarValue returns the status bar value.
func (f *Files) StatusBarValue() string {
	p := f.path
	if p == "." || p == "" {
		return " "
	}
	// Quote the path the way the file list quotes names, as file names may
	// contain control characters.
	return common.UnquoteFilename(p)
}

// StatusBarInfo returns the status bar info.
func (f *Files) StatusBarInfo() string {
	switch f.activeView {
	case filesViewFiles:
		return fmt.Sprintf("# %d/%d", f.selector.Index()+1, len(f.selector.VisibleItems()))
	case filesViewContent:
		return common.ScrollPercent(f.code.ScrollPosition())
	default:
		return ""
	}
}

// updateFilesCmd returns a command that lists the files of the current
// directory. Commands run in their own goroutines, so it captures the model
// state it needs here instead of reading the model later.
func (f *Files) updateFilesCmd() tea.Cmd {
	repo, ref, path := f.repo, f.ref, f.path
	ctx := f.common.Context()
	return func() tea.Msg {
		if repo == nil || ref == nil {
			return nil
		}
		r, err := openWithContext(ctx, repo)
		if err != nil {
			return common.ErrorMsg(err)
		}
		t, err := r.TreePath(ref, path)
		if err != nil {
			return common.ErrorMsg(err)
		}
		ents, err := t.EntriesWithSizes()
		if err != nil {
			return common.ErrorMsg(err)
		}
		ents.Sort()
		files := make([]selector.IdentifiableItem, 0)
		dirs := make([]selector.IdentifiableItem, 0)
		for _, e := range ents {
			if e.IsTree() {
				dirs = append(dirs, FileItem{entry: e})
			} else {
				files = append(files, FileItem{entry: e})
			}
		}
		return FileItemsMsg(append(dirs, files...))
	}
}

// selectFileCmd returns a command that loads the content of item, which is at
// filePath and at position index of its directory listing.
func (f *Files) selectFileCmd(item FileItem, filePath string, index int) tea.Cmd {
	repo, ref, load := f.repo, f.ref, f.load
	ctx := f.common.Context()
	limit := f.common.Config().BlobSizeLimit()
	return func() tea.Msg {
		if item.entry.IsTree() || item.Mode().IsDir() {
			return common.ErrorMsg(errInvalidFile)
		}
		if repo == nil || ref == nil {
			return common.ErrorMsg(errNoFileSelected)
		}
		fi := item.entry.File()

		var bin bool
		r, err := openWithContext(ctx, repo)
		if err == nil {
			attrs, err := r.CheckAttributes(ref, fi.Path())
			if err == nil {
				for _, attr := range attrs {
					if (attr.Name == "binary" && attr.Value == "set") ||
						(attr.Name == "text" && attr.Value == "unset") {
						bin = true
						break
					}
				}
			}
		}

		if bin {
			return common.ErrorMsg(errBinaryFile)
		}

		// Read the file once, capped by the size limit, and sniff the
		// bytes in hand rather than piping the blob a second time.
		c, err := fi.ContentsLimit(limit)
		if err != nil {
			return common.ErrorMsg(err)
		}

		if bin, _ := git.IsBinary(bytes.NewReader(c)); bin {
			return common.ErrorMsg(errBinaryFile)
		}

		return FileContentMsg{
			content: string(c),
			ext:     item.entry.Name(),
			path:    filePath,
			index:   index,
			load:    load,
		}
	}
}

// fetchBlameCmd returns a command that loads the blame of the current file.
func (f *Files) fetchBlameCmd() tea.Cmd {
	repo, ref, item, load := f.repo, f.ref, f.currentItem, f.load
	ctx := f.common.Context()
	return func() tea.Msg {
		if repo == nil || ref == nil || item == nil || item.entry.IsTree() {
			return common.ErrorMsg(errNoFileSelected)
		}
		r, err := openWithContext(ctx, repo)
		if err != nil {
			return common.ErrorMsg(err)
		}

		b, err := r.BlameFile(ref.ID, item.entry.File().Path())
		if err != nil {
			return common.ErrorMsg(err)
		}

		return FileBlameMsg{blame: b, load: load}
	}
}

func renderBlame(c common.Common, f *FileItem, b *git.Blame) string {
	if f == nil || f.entry.IsTree() || b == nil {
		return ""
	}

	lines := make([]string, 0)
	i := 1
	var prev string
	for {
		commit := b.Line(i)
		if commit == nil {
			break
		}
		who := utils.Sanitize(fmt.Sprintf("%s <%s>", commit.Author.Name, commit.Author.Email))
		line := fmt.Sprintf("%s %s %s",
			c.Styles.Tree.Blame.Hash.Render(commit.ID.String()[:7]),
			c.Styles.Tree.Blame.Message.Render(utils.Sanitize(commit.Summary())),
			c.Styles.Tree.Blame.Who.Render(who),
		)
		if line != prev {
			lines = append(lines, line)
		} else {
			lines = append(lines, "")
		}
		prev = line
		i++
	}

	return strings.Join(lines, "\n")
}

func (f *Files) deselectItemCmd() tea.Cmd {
	f.load++
	f.path = path.Dir(f.path)
	index := 0
	if len(f.lastSelected) > 0 {
		index = f.lastSelected[len(f.lastSelected)-1]
		f.lastSelected = f.lastSelected[:len(f.lastSelected)-1]
	}
	f.cursor = index
	f.activeView = filesViewFiles
	f.code.SetSideNote("")
	f.blameView = false
	f.currentBlame = nil
	f.code.UseGlamour = false
	return f.updateFilesCmd()
}

func (f *Files) setItems(items []selector.IdentifiableItem) tea.Cmd {
	return func() tea.Msg {
		return FileItemsMsg(items)
	}
}
