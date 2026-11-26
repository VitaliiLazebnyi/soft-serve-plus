package repo

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/internal/termtext"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/footer"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/viewport"
	"github.com/charmbracelet/soft-serve/pkg/ui/styles"
	"github.com/charmbracelet/x/ansi"
)

// waitBeforeLoading is how long a load in the Commits tab leaves the commit
// list on screen before the spinner shows.
const waitBeforeLoading = 100 * time.Millisecond

type logView int

const (
	logViewLoading logView = iota
	logViewCommits
	logViewDiff
)

// LogCountMsg is a message that contains the number of commits in a repo.
type LogCountMsg struct {
	count int64
	gen   uint64
}

func (m LogCountMsg) generation() uint64 { return m.gen }

func (m LogCountMsg) pane() paneID { return paneLog }

// LogItemsMsg is a message that contains one page of commits.
type LogItemsMsg struct {
	items []selector.IdentifiableItem
	// page is the page the items were loaded for, counted from 0, and
	// limit the page size.
	page, limit int
	gen         uint64
}

func (m LogItemsMsg) generation() uint64 { return m.gen }

func (m LogItemsMsg) pane() paneID { return paneLog }

// logSizeMsg follows a footer toggle, which resizes the Commits tab only
// after the tab has handled it: by the time this message comes, the tab has
// its new size.
type logSizeMsg struct{}

func (logSizeMsg) pane() paneID { return paneLog }

// LogDiffMsg is a message that contains the diff of a git commit.
type LogDiffMsg struct {
	commit *git.Commit
	diff   *git.Diff
	gen    uint64
}

func (m LogDiffMsg) generation() uint64 { return m.gen }

func (m LogDiffMsg) pane() paneID { return paneLog }

// LogDiffErrorMsg reports that a selected commit's diff failed to load.
type LogDiffErrorMsg struct {
	commit *git.Commit
	err    error
	gen    uint64
}

func (m LogDiffErrorMsg) generation() uint64 { return m.gen }

func (m LogDiffErrorMsg) pane() paneID { return paneLog }

// Log is a model that displays a list of commits and their diffs.
type Log struct {
	common     *common.Common
	width      int
	selector   *selector.Selector
	vp         *viewport.Viewport
	activeView logView
	repo       Repository
	ref        *git.Reference
	// count is the number of commits of the ref. The list holds one page of
	// them: page, counted from 0, of perPage commits, with the cursor on
	// row. While a page loads, they are the page, size and row it is
	// loaded for.
	count          int64
	page           int
	perPage        int
	row            int
	activeCommit   *git.Commit
	selectedCommit *git.Commit
	currentDiff    *git.Diff
	loadingTime    time.Time
	gen            uint64
	// render counts the renders of currentDiff; see diffRenderedMsg.
	render int
	// renders keeps one render in flight.
	renders common.RenderGate
	// winWidth is the window width of the last tea.WindowSizeMsg. The diff
	// is rendered again only when it changes. The window is wider than the
	// pane by the parent's margins, which do not change, so its width
	// changes with the pane's, which the parent has set by the time the
	// message comes and which the render uses.
	winWidth int
	// countLoad, pageLoad and diffLoad are the loads in flight of each
	// kind: a new one cancels the one before.
	countLoad, pageLoad, diffLoad common.Loader
	// waitBeforeLoading is waitBeforeLoading; tests change it.
	waitBeforeLoading time.Duration
}

// NewLog creates a new Log model.
func NewLog(c *common.Common) *Log {
	l := &Log{
		common:     c,
		vp:         viewport.New(c),
		activeView: logViewCommits,

		waitBeforeLoading: waitBeforeLoading,
	}
	sel := selector.NewPane(c, LogItemDelegate{c})
	l.selector = sel
	return l
}

// Path implements Pane.
func (l *Log) Path() string {
	switch l.activeView {
	case logViewCommits:
		return ""
	default:
		return "diff" // any non-empty path: a commit's diff is open
	}
}

// TabName returns the name of the tab.
func (l *Log) TabName() string {
	return "Commits"
}

// SetSize implements common.Component.
func (l *Log) SetSize(width, height int) {
	l.width, _ = common.ClampSize(width, height)
	l.selector.SetSize(width, height)
	l.vp.SetSize(width, height)
}

// ShortHelp implements help.KeyMap.
func (l *Log) ShortHelp() []key.Binding {
	switch l.activeView {
	case logViewCommits:
		copyKey := l.common.CopyKey("copy hash")
		return []key.Binding{
			l.common.KeyMap.UpDown,
			l.common.KeyMap.SelectItem,
			copyKey,
		}
	case logViewDiff:
		copyKey := l.common.CopyKey("copy diff")
		return []key.Binding{
			l.common.KeyMap.UpDown,
			l.common.KeyMap.BackItem,
			copyKey,
			l.common.KeyMap.GotoTop,
			l.common.KeyMap.GotoBottom,
		}
	default:
		return []key.Binding{}
	}
}

// FullHelp implements help.KeyMap.
func (l *Log) FullHelp() [][]key.Binding {
	k := l.selector.KeyMap()
	b := make([][]key.Binding, 0)
	switch l.activeView {
	case logViewCommits:
		copyKey := l.common.CopyKey("copy hash")
		b = append(b, []key.Binding{
			l.common.KeyMap.SelectItem,
		})
		move := []key.Binding{k.GoToStart, k.GoToEnd}
		if l.lastPage() > 0 {
			// The page keys do something only with more than one page.
			move = append([]key.Binding{l.common.KeyMap.NextPage, l.common.KeyMap.PrevPage}, move...)
		}
		b = append(b, []key.Binding{copyKey, k.CursorUp, k.CursorDown}, move)
	case logViewDiff:
		copyKey := l.common.CopyKey("copy diff")
		k := l.vp.KeyMap
		b = append(b, []key.Binding{
			l.common.KeyMap.BackItem,
			copyKey,
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
				l.common.KeyMap.GotoTop,
				l.common.KeyMap.GotoBottom,
			},
		}...)
	}
	return b
}

func (l *Log) startLoading() {
	l.loadingTime = time.Now()
	l.activeView = logViewLoading
}

// Init implements common.Model.
func (l *Log) Init() tea.Cmd {
	l.activeView = logViewCommits
	l.count = 0
	l.page, l.row, l.perPage = 0, 0, l.selector.PerPage()
	l.activeCommit = nil
	l.selectedCommit = nil
	l.clearDiff()
	// A page or a diff of the previous ref is no longer wanted; the count
	// starts again below.
	l.pageLoad.Stop()
	l.diffLoad.Stop()
	// start loading on init
	l.startLoading()
	return l.countCommitsCmd()
}

// Update implements common.Model.
func (l *Log) Update(msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, 0)
	switch msg := msg.(type) {
	case OpenedMsg:
		l.repo = msg
		l.clearDiff()
	case RefMsg:
		l.ref = msg.Ref
		l.gen = msg.gen
		l.selector.Select(0)
		cmds = append(cmds, l.Init())
	case LogCountMsg:
		l.count = msg.count
		cmds = append(cmds, l.reloadPage())
	case LogItemsMsg:
		if msg.page != l.page || msg.limit != l.perPage {
			// A load for a page the user has left, or by a size the list
			// no longer has, that finished after a newer load started:
			// the newer one shows its page.
			break
		}
		// Stop loading after receiving items, unless a commit's diff is
		// loading or shown: a reload on resize or footer toggle refreshes the
		// list behind it without leaving the diff.
		if l.selectedCommit == nil {
			l.activeView = logViewCommits
		}
		cmds = append(cmds, l.selector.SetItems(msg.items))
		l.selector.Select(max(0, min(l.row, len(msg.items)-1)))
		l.row = l.selector.Index()
		if msg.limit != l.selector.PerPage() {
			// The list was resized after the load was built, as on a
			// footer toggle while the page loaded: load it again by the
			// size the list has now, or it shows too few or too many rows.
			cmds = append(cmds, l.reloadPage())
		}
		if i, ok := l.selector.SelectedItem().(LogItem); ok {
			l.activeCommit = i.Commit
		}
	case tea.KeyPressMsg, tea.MouseClickMsg, tea.MouseWheelMsg:
		switch l.activeView {
		case logViewCommits:
			switch kmsg := msg.(type) {
			case tea.KeyPressMsg:
				switch {
				case key.Matches(kmsg, l.common.KeyMap.SelectItem):
					cmds = append(cmds, l.selector.SelectItemCmd())
				}
			}
			// The list holds the page shown; a move off it loads the
			// page it goes to.
			if page, row, ok := l.pageMove(msg); ok {
				l.page, l.row = page, row
				l.startLoading()
				cmds = append(cmds, l.updateCommitsCmd())
				break
			}
			cmds = append(cmds, l.selector.Update(msg))
			l.row = l.selector.Index()
		case logViewDiff:
			switch kmsg := msg.(type) {
			case tea.KeyPressMsg:
				switch {
				case key.Matches(kmsg, l.common.KeyMap.BackItem):
					l.goBack()
				case key.Matches(kmsg, l.common.KeyMap.Copy):
					if l.currentDiff != nil {
						cmds = append(cmds, copyCmd(l.currentDiff.Patch, "Commit diff copied to clipboard"))
					}
				}
			}
		}
	case GoBackMsg:
		l.goBack()
	case selector.ActiveMsg:
		switch sel := msg.IdentifiableItem.(type) {
		case LogItem:
			l.activeCommit = sel.Commit
		}
	case selector.SelectMsg:
		switch sel := msg.IdentifiableItem.(type) {
		case LogItem:
			l.selectedCommit = sel.Commit
			l.clearDiff()
			l.startLoading()
			cmds = append(cmds, l.loadDiffCmd())
		}
	case LogDiffMsg:
		if l.selectedCommit == nil || l.selectedCommit != msg.commit || msg.diff == nil {
			// The commit was deselected, another commit was opened, or the
			// ref changed, while the diff was loading.
			break
		}
		// The pane stays in its loading view until the diff is rendered.
		l.currentDiff = msg.diff
		cmds = append(cmds, l.renderDiffCmd())
	case diffRenderedMsg:
		again := l.renders.Done(msg.epoch)
		if msg.render == l.render && l.selectedCommit != nil && l.currentDiff != nil {
			l.vp.SetContent(msg.content)
			if l.activeView == logViewLoading {
				// The first render of this diff; a later one follows a
				// resize and keeps the scroll position.
				l.vp.GotoTop()
				l.activeView = logViewDiff
			}
		}
		// Otherwise the diff or the width changed while it was rendered:
		// render what is shown now, once for every change meanwhile.
		if again && l.selectedCommit != nil && l.currentDiff != nil {
			cmds = append(cmds, l.renderDiffCmd())
		}
	case errorMsg:
		// The count or a page failed to load: leave the loading view for
		// the commit list, unless a commit is opening.
		// The list still holds the previous ref's commits: clear it.
		if l.activeView == logViewLoading && l.selectedCommit == nil {
			l.activeView = logViewCommits
			l.count = 0
			l.page, l.row = 0, 0
			cmds = append(cmds, l.selector.SetItems(nil))
		}
	case LogDiffErrorMsg:
		// Leave the loading view for the commit list, so the spinner does not
		// stay on screen once the error is dismissed, then show the error.
		// Repo can deliver this message twice; only the first one acts.
		// An error for a commit that is no longer selected is dropped.
		if l.activeView == logViewLoading && l.selectedCommit != nil && l.selectedCommit == msg.commit {
			l.selectedCommit = nil
			l.activeView = logViewCommits
			err := msg.err
			cmds = append(cmds, func() tea.Msg { return common.ErrorMsg{Err: err} })
		}
	case footer.ToggleFooterMsg:
		// The tab is resized only after it handles this message: look at
		// its size once that is done.
		cmds = append(cmds, func() tea.Msg { return logSizeMsg{} })
	case logSizeMsg:
		cmds = append(cmds, l.resized())
	case tea.WindowSizeMsg:
		// The diff is wrapped to the width, so only a width change needs
		// it rendered again.
		widthChanged := msg.Width != l.winWidth
		l.winWidth = msg.Width
		if widthChanged && l.selectedCommit != nil && l.currentDiff != nil {
			cmds = append(cmds, l.renderDiffCmd())
		}
		cmds = append(cmds, l.resized())
	case EmptyRepoMsg:
		l.countLoad.Stop()
		l.pageLoad.Stop()
		l.diffLoad.Stop()
		l.ref = nil
		l.gen = msg.gen
		l.activeView = logViewCommits
		l.count = 0
		l.page, l.row = 0, 0
		l.activeCommit = nil
		l.selectedCommit = nil
		l.clearDiff()
		cmds = append(cmds, l.selector.SetItems(nil))
		l.selector.Select(0)
	}
	switch l.activeView {
	case logViewDiff:
		cmds = append(cmds, l.vp.Update(msg))
	}
	return tea.Batch(cmds...)
}

// View implements common.Model.
func (l *Log) View() string {
	switch l.activeView {
	case logViewLoading, logViewCommits:
		// The repository page shows its spinner in place of the commit
		// list once a load has taken waitBeforeLoading; see Loading.
		return l.selector.View()
	case logViewDiff:
		return l.vp.View()
	default:
		return ""
	}
}

// Loading implements Pane. For the first waitBeforeLoading of a load it
// returns no text, so the commit list stays on screen and a fast page load
// does not flash the spinner.
func (l *Log) Loading() (string, bool) {
	if l.activeView != logViewLoading {
		return "", false
	}
	if !l.loadingTime.Add(l.waitBeforeLoading).Before(time.Now()) {
		return "", true
	}
	if l.selectedCommit == nil {
		return "loading commits…", true
	}
	return "loading commit…", true
}

func (l *Log) id() paneID { return paneLog }

// StatusBarValue returns the status bar value.
func (l *Log) StatusBarValue() string {
	if l.activeView == logViewLoading {
		return ""
	}
	c := l.activeCommit
	if c == nil {
		return ""
	}
	who := termtext.Text(c.Author.Name)
	if email := termtext.Text(c.Author.Email); email != "" {
		who += " <" + email + ">"
	}
	value := c.ID.String()[:7]
	if who != "" {
		value += " by " + who
	}
	return value
}

// StatusBarInfo returns the status bar info.
func (l *Log) StatusBarInfo() string {
	switch l.activeView {
	case logViewLoading:
		if l.count == 0 {
			return ""
		}
		fallthrough
	case logViewCommits:
		// The list holds one page; the Log knows which.
		return fmt.Sprintf("p. %d/%d", l.page+1, l.lastPage()+1)
	case logViewDiff:
		return common.ScrollPercent(l.vp.ScrollPosition())
	default:
		return ""
	}
}

func (l *Log) goBack() {
	if l.activeView == logViewDiff {
		l.activeView = logViewCommits
		l.selectedCommit = nil
		l.clearDiff()
	}
}

// clearDiff forgets the diff shown, and drops a render of it in flight. It
// also forgets that render, whose result may never come back, for example
// when the page was left while it ran.
func (l *Log) clearDiff() {
	l.currentDiff = nil
	l.render++
	l.renders.Reset()
	l.vp.SetContent("")
}

// renderDiffCmd returns a command that renders the selected commit and its
// diff for the viewport, or nil while a render is in flight: the next one
// starts when it returns.
func (l *Log) renderDiffCmd() tea.Cmd {
	l.render++
	epoch, ok := l.renders.Start()
	if !ok {
		return nil
	}
	commit, st, width := l.selectedCommit, l.common.Styles, l.width
	return renderDiffCmd(paneLog, l.render, epoch, st, width, l.currentDiff, func() string {
		return renderCommit(commit, st, width)
	})
}

// countCommitsCmd returns a command that counts the commits of the current
// ref. Commands run in their own goroutines, so it captures the model state it
// needs here instead of reading the model later.
func (l *Log) countCommitsCmd() tea.Cmd {
	repo, ref, logger, gen := l.repo, l.ref, l.common.Logger, l.gen
	ctx := l.countLoad.Start(l.common.Context())
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
			return loadError(ctx, paneLog, gen, err)
		}
		count, err := r.CountCommits(ref)
		if err != nil {
			logger.Debugf("error counting commits: %v", err)
			return loadError(ctx, paneLog, gen, err)
		}
		return LogCountMsg{count: count, gen: gen}
	}
}

// lastPage returns the number of the last page of commits, counted from 0.
func (l *Log) lastPage() int {
	if l.count <= 0 || l.perPage <= 0 {
		return 0
	}
	return int((l.count - 1) / int64(l.perPage))
}

// pageMove returns the page and row msg moves the cursor to when it moves
// off the page shown: a page key, home or end, or a cursor key or the
// wheel at the edge of the page. ok is false when the cursor stays on the
// page, where the list moves it.
func (l *Log) pageMove(msg tea.Msg) (page, row int, ok bool) {
	k := l.selector.KeyMap()
	last, idx := l.lastPage(), l.selector.Index()
	var up, down bool
	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		up, down = msg.Button == tea.MouseWheelUp, msg.Button == tea.MouseWheelDown
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, k.CursorUp):
			up = true
		case key.Matches(msg, k.CursorDown):
			down = true
		case key.Matches(msg, l.common.KeyMap.PrevPage):
			return l.page - 1, idx, l.page > 0
		case key.Matches(msg, l.common.KeyMap.NextPage):
			return l.page + 1, idx, l.page < last
		case key.Matches(msg, k.GoToStart):
			return 0, 0, l.page > 0
		case key.Matches(msg, k.GoToEnd):
			return last, l.lastRow(), l.page < last
		}
	}
	switch {
	case up && idx == 0 && l.page > 0:
		return l.page - 1, l.perPage - 1, true
	case down && idx >= len(l.selector.Items())-1 && l.page < last:
		return l.page + 1, 0, true
	}
	return 0, 0, false
}

// lastRow returns the row of the last commit on the last page.
func (l *Log) lastRow() int {
	if l.count <= 0 || l.perPage <= 0 {
		return 0
	}
	return int((l.count - 1) % int64(l.perPage))
}

// resized returns a command that loads the page again when the list holds
// another number of commits a page since the page was loaded, or nil.
func (l *Log) resized() tea.Cmd {
	if l.repo == nil || l.ref == nil || l.selector.PerPage() == l.perPage {
		return nil
	}
	// Only show the spinner in the commits view, so a diff stays on screen.
	if l.activeView == logViewCommits {
		l.startLoading()
	}
	return l.reloadPage()
}

// reloadPage returns a command that loads, by the size the list has now, the
// page that holds the commit under the cursor.
func (l *Log) reloadPage() tea.Cmd {
	if per := l.selector.PerPage(); per != l.perPage {
		if l.perPage > 0 {
			i := l.page*l.perPage + l.row
			if l.count > 0 {
				// A row past the last commit, which a page that has
				// not arrived yet may hold, is on the last commit.
				i = min(i, int(l.count)-1)
			}
			l.page, l.row = i/per, i%per
		}
		l.perPage = per
	}
	return l.updateCommitsCmd()
}

// updateCommitsCmd returns a command that loads the page of commits the list
// is to show.
func (l *Log) updateCommitsCmd() tea.Cmd {
	repo, ref, logger, gen := l.repo, l.ref, l.common.Logger, l.gen
	ctx := l.pageLoad.Start(l.common.Context())
	count, page, limit := l.count, l.page, l.perPage
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
			return loadError(ctx, paneLog, gen, err)
		}

		if count == 0 {
			return LogItemsMsg{page: page, limit: limit, gen: gen}
		}

		// CommitsByPage pages start at 1
		cc, err := r.CommitsByPage(ref, page+1, limit)
		if err != nil {
			logger.Debugf("error loading commits: %v", err)
			return loadError(ctx, paneLog, gen, err)
		}
		items := make([]selector.IdentifiableItem, 0, min(len(cc), limit))
		for _, c := range cc[:min(len(cc), limit)] {
			items = append(items, LogItem{Commit: c})
		}
		return LogItemsMsg{items: items, page: page, limit: limit, gen: gen}
	}
}

// loadDiffCmd returns a command that loads the diff of the selected commit.
func (l *Log) loadDiffCmd() tea.Cmd {
	repo, commit, logger, gen := l.repo, l.selectedCommit, l.common.Logger, l.gen
	ctx := l.diffLoad.Start(l.common.Context())
	return func() tea.Msg {
		if ctx.Err() != nil {
			// A newer load replaced this one before it started.
			return nil
		}
		if repo == nil || commit == nil {
			return nil
		}
		r, err := openWithContext(ctx, repo)
		if err != nil {
			logger.Debugf("error loading diff repository: %v", err)
			if ctx.Err() != nil {
				return nil
			}
			return LogDiffErrorMsg{commit: commit, err: err, gen: gen}
		}
		diff, err := r.Diff(commit)
		if err != nil {
			logger.Debugf("error loading diff: %v", err)
			if ctx.Err() != nil {
				return nil
			}
			return LogDiffErrorMsg{commit: commit, err: err, gen: gen}
		}
		return LogDiffMsg{commit: commit, diff: diff, gen: gen}
	}
}

func (l *Log) renderCommit(c *git.Commit) string {
	return renderCommit(c, l.common.Styles, l.width)
}

// renderCommit renders c's header and message width columns wide.
func renderCommit(c *git.Commit, st *styles.Styles, width int) string {
	s := strings.Builder{}
	// Strip escape sequences and control characters, CR included since
	// lipgloss prints empty lines when CRLF is used.
	msg := termtext.Text(c.Message)
	fmt.Fprintf(&s, "%s\n%s\n%s\n%s\n",
		st.Log.CommitHash.Render("commit "+c.ID.String()),
		st.Log.CommitAuthor.Render(fmt.Sprintf("Author: %s <%s>", termtext.Text(c.Author.Name), termtext.Text(c.Author.Email))),
		st.Log.CommitDate.Render("Date:   "+c.Author.When.Format(time.UnixDate)),
		st.Log.CommitBody.Render(msg),
	)
	return wrapText(s.String(), width-2)
}

// wrapText hard-wraps s at width columns. It measures with x/ansi grapheme
// widths, the same way lipgloss lays out the pane, so emoji and other wide
// characters wrap where the layout expects them to. Tabs are expanded to four
// spaces first, as reflow/wrap did, because x/ansi gives a tab no width.
func wrapText(s string, width int) string {
	return ansi.Hardwrap(strings.ReplaceAll(s, "\t", "    "), width, false)
}
