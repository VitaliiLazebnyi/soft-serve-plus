package repo

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/internal/termtext"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/code"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/syntax"
)

type filesView int

const (
	filesViewLoading filesView = iota
	filesViewFiles
	filesViewContent
)

var (
	errNoFileSelected = common.NewUserError("no file selected")
	errBinaryFile     = common.NewUserError("binary file")
	errInvalidFile    = common.NewUserError("invalid file")
	errFileTooLarge   = common.NewUserError("file too large")
	// errSubmodule is reported for a submodule in the file list: its commit
	// is in another repository, so there is nothing here to show.
	errSubmodule = common.NewUserError("submodule")
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
type FileItemsMsg struct {
	items []selector.IdentifiableItem
	// listing is the Files listing count when the listing was requested.
	listing int
	// dir is the directory listed.
	dir string
	gen uint64
	// truncated is set when the directory is too large to list in full
	// and items holds only the first entries that fit.
	truncated bool
}

func (m FileItemsMsg) generation() uint64 { return m.gen }

func (m FileItemsMsg) pane() paneID { return paneFiles }

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
	// isMarkdown is worked out once, when the file is loaded, because chroma
	// may have to analyse the whole content to tell.
	isMarkdown bool
	gen        uint64
}

func (m FileContentMsg) generation() uint64 { return m.gen }

func (m FileContentMsg) pane() paneID { return paneFiles }

// FileBlameMsg is a message that contains the blame of a file.
type FileBlameMsg struct {
	blame *git.Blame
	// load is the Files load the blame was requested in.
	load int
	gen  uint64
}

func (m FileBlameMsg) generation() uint64 { return m.gen }

func (m FileBlameMsg) pane() paneID { return paneFiles }

// Files is the model for the files view.
type Files struct {
	common         *common.Common
	selector       *selector.Selector
	ref            *git.Reference
	activeView     filesView
	repo           Repository
	code           *code.Code
	path           string
	currentItem    *FileItem
	currentContent FileContentMsg
	currentBlame   *git.Blame
	lastSelected   []int
	lineNumber     bool
	cursor         int
	blameView      bool
	// glamourBeforeBlame is whether a Markdown file was rendered, rather
	// than shown as source, when blame was turned on; turning blame off
	// restores it.
	glamourBeforeBlame bool
	// truncated is set while the listing shows only the first entries
	// of the directory that fit.
	truncated bool
	// listing counts the listings requested, and the files opened: a
	// FileItemsMsg of an earlier listing is dropped. It is not load, as
	// opening a file that then fails to load must not drop the listing of
	// the directory the pane is in.
	listing int
	// listed is the directory the listing on screen shows. It is not
	// path, which names the directory being listed, or the file opened,
	// as soon as the user asks for it.
	listed string
	// load counts the file and blame loads. A FileContentMsg or FileBlameMsg
	// from an earlier load arrived after the user or a ref change moved on,
	// and is dropped.
	load int
	gen  uint64
	// listLoad is the listing in flight, and fileLoad the file or blame
	// load: a new one of each cancels the one before. Listing a directory
	// also cancels a file load, but opening a file does not cancel the
	// listing: the user may open a file from the previous listing, and
	// when it fails to open the pane still needs the listing.
	listLoad, fileLoad common.Loader
}

// NewFiles creates a new files model.
func NewFiles(c *common.Common) *Files {
	f := &Files{
		common:       c,
		code:         code.New(c, "", ""),
		activeView:   filesViewLoading,
		lastSelected: make([]int, 0),
		lineNumber:   true,
	}
	sel := selector.NewPane(c, FileItemDelegate{c})
	f.selector = sel
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
	return f
}

// Path implements Pane.
func (f *Files) Path() string {
	p := f.path
	if p == "." {
		return ""
	}
	return p
}

// TabName returns the tab name.
func (f *Files) TabName() string {
	return "Files"
}

// SetSize implements common.Component.
func (f *Files) SetSize(width, height int) {
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
		copyKey = f.common.CopyKey("copy name")
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
		if f.currentContent.isMarkdown && !f.blameView {
			actionKeys = append(actionKeys, preview)
		}
		copyKey = f.common.CopyKey("copy content")
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

// Init implements common.Model.
func (f *Files) Init() tea.Cmd {
	f.path = ""
	f.currentItem = nil
	f.activeView = filesViewLoading
	f.lastSelected = make([]int, 0)
	f.blameView = false
	f.currentBlame = nil
	f.code.UseGlamour = false
	f.load++
	return f.updateFilesCmd()
}

// Update implements common.Model.
func (f *Files) Update(msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, 0)
	switch msg := msg.(type) {
	case OpenedMsg:
		f.repo = msg
	case RefMsg:
		f.ref = msg.Ref
		f.gen = msg.gen
		f.selector.Select(0)
		cmds = append(cmds, f.Init())
	case FileItemsMsg:
		if msg.listing != f.listing {
			// The user opened a file or another directory since; the
			// listing would replace it while the path names it.
			break
		}
		cmds = append(cmds,
			f.selector.SetItems(msg.items),
		)
		f.truncated = msg.truncated
		f.listed = msg.dir
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
		// A listing still on its way would put the list back while the
		// path names the file: drop it, and stop it, so that it cannot
		// fail and turn off a blame the user opens next either.
		f.listing++
		f.listLoad.Stop()
		f.path = msg.path
		f.pushSelected(path.Dir(msg.path), msg.index)
		f.activeView = filesViewContent
		f.currentContent = msg
		f.code.UseGlamour = msg.isMarkdown
		// The code view sanitises the content, highlights and wraps it in
		// a command; until it is done the tab shows the spinner.
		// currentContent keeps the raw bytes for copying.
		cmds = append(cmds, f.code.SetMarkdownContent(msg.content, msg.ext, msg.isMarkdown))
	case code.RenderedMsg:
		// The code view's render, whichever view is shown now.
		cmds = append(cmds, f.code.Update(msg))
	case FileBlameMsg:
		if msg.load != f.load || !f.blameView {
			break
		}
		f.currentBlame = msg.blame
		f.activeView = filesViewContent
		f.code.UseGlamour = false
		cmds = append(cmds, f.code.SetSideNote(renderBlame(f.common, f.currentItem, msg.blame)))
	case selector.SelectMsg:
		switch sel := msg.IdentifiableItem.(type) {
		case FileItem:
			f.load++
			f.currentItem = &sel
			// The item comes from the listing on screen, which may not be
			// of the directory the user has since asked for.
			p := path.Join(f.listed, sel.entry.Name())
			if sel.entry.IsTree() {
				f.path = p
				f.pushSelected(f.listed, f.selector.Index())
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
				cmds = append(cmds, f.code.Init())
			case key.Matches(msg, blameView):
				f.activeView = filesViewLoading
				f.blameView = !f.blameView
				f.load++
				if f.blameView {
					f.glamourBeforeBlame = f.code.UseGlamour
					cmds = append(cmds, f.fetchBlameCmd())
				} else {
					f.fileLoad.Stop()
					f.activeView = filesViewContent
					f.code.UseGlamour = f.glamourBeforeBlame
					cmds = append(cmds, f.code.SetSideNote(""))
				}
			case key.Matches(msg, preview) &&
				f.currentContent.isMarkdown && !f.blameView:
				f.code.UseGlamour = !f.code.UseGlamour
				cmds = append(cmds, f.code.Init())
			}
		}
	case tea.WindowSizeMsg:
		// A listing does not depend on the size, so a resize loads
		// nothing. The content view re-renders through f.code.Update
		// below. The parent has sized the pane before passing the
		// message on.
	case errorMsg:
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
		f.listLoad.Stop()
		f.fileLoad.Stop()
		f.ref = nil
		f.gen = msg.gen
		f.load++
		f.path = ""
		f.currentItem = nil
		f.activeView = filesViewFiles
		f.lastSelected = make([]int, 0)
		f.selector.Select(0)
		cmds = append(cmds, f.setItems([]selector.IdentifiableItem{}))
	}
	switch f.activeView {
	case filesViewFiles:
		cmds = append(cmds, f.selector.Update(msg))
	case filesViewContent:
		if _, ok := msg.(code.RenderedMsg); !ok {
			cmds = append(cmds, f.code.Update(msg))
		}
	}
	return tea.Batch(cmds...)
}

// View implements common.Model.
func (f *Files) View() string {
	switch f.activeView {
	case filesViewFiles:
		return f.selector.View()
	case filesViewContent:
		return f.code.View()
	default:
		return ""
	}
}

// Loading implements Pane. An opened file loads until its first render is
// done.
func (f *Files) Loading() (string, bool) {
	return "loading…", f.activeView == filesViewLoading ||
		(f.activeView == filesViewContent && f.code.Rendering())
}

func (f *Files) id() paneID { return paneFiles }

func (f *Files) ownsRender(msg code.RenderedMsg) bool { return f.code.Owns(msg) }

// StatusBarValue returns the status bar value.
func (f *Files) StatusBarValue() string {
	p := f.path
	if p == "." || p == "" {
		return " "
	}
	// File names may contain control characters; escape them for display.
	return termtext.Filename(p)
}

// StatusBarInfo returns the status bar info.
func (f *Files) StatusBarInfo() string {
	switch f.activeView {
	case filesViewFiles:
		total := len(f.selector.VisibleItems())
		if total == 0 {
			return "# 0/0"
		}
		if f.truncated {
			return fmt.Sprintf("# %d/%d (truncated)", f.selector.Index()+1, total)
		}
		return fmt.Sprintf("# %d/%d", f.selector.Index()+1, total)
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
	f.listing++
	repo, ref, p, listing, gen := f.repo, f.ref, f.path, f.listing, f.gen
	// The pane is about to list a directory: a file it was opening is no
	// longer wanted.
	f.fileLoad.Stop()
	ctx := f.listLoad.Start(f.common.Context())
	return func() tea.Msg {
		if ctx.Err() != nil {
			// A newer load replaced this one before it started.
			return nil
		}
		if repo == nil || ref == nil {
			return nil
		}
		r, err := openWithContext(ctx, repo)
		if err != nil {
			return loadError(ctx, paneFiles, gen, err)
		}
		t, err := r.TreePath(ref, p)
		if err != nil {
			return loadError(ctx, paneFiles, gen, err)
		}
		ents, err := t.Entries()
		// A directory with too many entries is listed in part, and the
		// status bar says so.
		truncated := errors.Is(err, git.ErrTreeTruncated)
		if err != nil && !truncated {
			return loadError(ctx, paneFiles, gen, err)
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
		return FileItemsMsg{items: append(dirs, files...), listing: listing, dir: p, gen: gen, truncated: truncated}
	}
}

// selectFileCmd returns a command that loads the content of item, which is at
// filePath and at position index of its directory listing.
func (f *Files) selectFileCmd(item FileItem, filePath string, index int) tea.Cmd {
	repo, ref, load, gen := f.repo, f.ref, f.load, f.gen
	ctx := f.fileLoad.Start(f.common.Context())
	limit := int64(config.MaxBlobSize)
	return func() tea.Msg {
		if ctx.Err() != nil {
			// A newer load replaced this one before it started.
			return nil
		}
		if item.entry.IsTree() || item.Mode().IsDir() {
			return loadError(ctx, paneFiles, gen, errInvalidFile)
		}
		if item.entry.IsCommit() {
			return loadError(ctx, paneFiles, gen, errSubmodule)
		}
		if repo == nil || ref == nil {
			return loadError(ctx, paneFiles, gen, errNoFileSelected)
		}
		// The entry was listed under the listing's context, which a later
		// listing cancels; read the file under this load's.
		r, err := openWithContext(ctx, repo)
		if err != nil {
			return loadError(ctx, paneFiles, gen, err)
		}
		fi := r.EntryFile(item.entry)

		// The attributes only mark a file binary; a failure to read them
		// leaves the content sniffing below to decide.
		var bin bool
		if attrs, err := r.CheckAttributes(ref, fi.Path()); err == nil {
			for _, attr := range attrs {
				if (attr.Name == "binary" && attr.Value == "set") ||
					(attr.Name == "text" && attr.Value == "unset") {
					bin = true
					break
				}
			}
		}

		if bin {
			return loadError(ctx, paneFiles, gen, errBinaryFile)
		}

		// Read the file once, capped by the size limit, and sniff the
		// bytes in hand rather than piping the blob a second time.
		c, err := fi.ContentsLimit(limit)
		if errors.Is(err, git.ErrFileTooLarge) {
			// Tell the user, and keep the size details for the log.
			return loadError(ctx, paneFiles, gen, fmt.Errorf("%w: %w", errFileTooLarge, err))
		}
		if err != nil {
			return loadError(ctx, paneFiles, gen, err)
		}

		if bin, _ := git.IsBinary(bytes.NewReader(c)); bin {
			return loadError(ctx, paneFiles, gen, errBinaryFile)
		}

		content, name := string(c), item.entry.Name()
		return FileContentMsg{
			content:    content,
			ext:        name,
			path:       filePath,
			index:      index,
			load:       load,
			isMarkdown: syntax.IsFileMarkdown(content, name),
			gen:        gen,
		}
	}
}

// fetchBlameCmd returns a command that loads the blame of the current file.
func (f *Files) fetchBlameCmd() tea.Cmd {
	repo, ref, item, load, gen := f.repo, f.ref, f.currentItem, f.load, f.gen
	ctx := f.fileLoad.Start(f.common.Context())
	return func() tea.Msg {
		if ctx.Err() != nil {
			// A newer load replaced this one before it started.
			return nil
		}
		if repo == nil || ref == nil || item == nil || item.entry.IsTree() {
			return loadError(ctx, paneFiles, gen, errNoFileSelected)
		}
		r, err := openWithContext(ctx, repo)
		if err != nil {
			return loadError(ctx, paneFiles, gen, err)
		}

		b, err := r.BlameFile(ref.ID(), item.entry.File().Path())
		if err != nil {
			return loadError(ctx, paneFiles, gen, err)
		}

		return FileBlameMsg{blame: b, load: load, gen: gen}
	}
}

func renderBlame(c *common.Common, f *FileItem, b *git.Blame) string {
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
		who := termtext.Text(fmt.Sprintf("%s <%s>", commit.Author.Name, commit.Author.Email))
		line := fmt.Sprintf("%s %s %s",
			c.Styles.Tree.Blame.Hash.Render(commit.ID.String()[:7]),
			c.Styles.Tree.Blame.Message.Render(termtext.Text(commit.Summary())),
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

// pushSelected records index as the position of the item opened in
// directory dir. The back stack holds a position for every directory above
// the item, so it is first cut to dir's depth: an item opened from the
// listing on screen while another directory loaded comes from a directory
// shallower than the one asked for.
func (f *Files) pushSelected(dir string, index int) {
	depth := 0
	if dir != "" && dir != "." {
		depth = strings.Count(dir, "/") + 1
	}
	f.lastSelected = append(f.lastSelected[:min(len(f.lastSelected), depth)], index)
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
	// The list is shown next; the file is rendered again when it is
	// opened, without the blame.
	f.code.ClearSideNote()
	f.blameView = false
	f.currentBlame = nil
	f.code.UseGlamour = false
	return f.updateFilesCmd()
}

func (f *Files) setItems(items []selector.IdentifiableItem) tea.Cmd {
	listing, gen := f.listing, f.gen
	return func() tea.Msg {
		return FileItemsMsg{items: items, listing: listing, gen: gen}
	}
}
