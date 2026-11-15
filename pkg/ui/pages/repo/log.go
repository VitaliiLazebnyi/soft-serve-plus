package repo

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/footer"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/viewport"
	"github.com/charmbracelet/soft-serve/pkg/ui/styles"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/charmbracelet/x/ansi"
)

var waitBeforeLoading = time.Millisecond * 100

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

// LogItemsMsg is a message that contains a slice of LogItem.
type LogItemsMsg struct {
	items []selector.IdentifiableItem
	// limit is the page size the items were loaded for, or 0 when they
	// were not loaded by page.
	limit int
	gen   uint64
}

func (m LogItemsMsg) generation() uint64 { return m.gen }

func (m LogItemsMsg) pane() paneID { return paneLog }

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
	common         common.Common
	selector       *selector.Selector
	vp             *viewport.Viewport
	activeView     logView
	repo           proto.Repository
	ref            *git.Reference
	count          int64
	nextPage       int
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
}

// NewLog creates a new Log model.
func NewLog(common common.Common) *Log {
	l := &Log{
		common:     common,
		vp:         viewport.New(common),
		activeView: logViewCommits,
	}
	selector := selector.NewPane(common, LogItemDelegate{&common})
	l.selector = selector
	return l
}

// Path implements Pane.
func (l *Log) Path() string {
	switch l.activeView {
	case logViewCommits:
		return ""
	default:
		return "diff" // XXX: this is a place holder and doesn't mean anything
	}
}

// TabName returns the name of the tab.
func (l *Log) TabName() string {
	return "Commits"
}

// SetSize implements common.Component.
func (l *Log) SetSize(width, height int) {
	l.common.SetSize(width, height)
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
	k := l.selector.KeyMap
	b := make([][]key.Binding, 0)
	switch l.activeView {
	case logViewCommits:
		copyKey := l.common.CopyKey("copy hash")
		b = append(b, []key.Binding{
			l.common.KeyMap.SelectItem,
		})
		b = append(b, [][]key.Binding{
			{
				copyKey,
				k.CursorUp,
				k.CursorDown,
			},
			{
				k.NextPage,
				k.PrevPage,
				k.GoToStart,
				k.GoToEnd,
			},
		}...)
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

// Init implements tea.Model.
func (l *Log) Init() tea.Cmd {
	l.activeView = logViewCommits
	l.nextPage = 0
	l.count = 0
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
	case RepoMsg:
		l.repo = msg
		l.clearDiff()
	case RefMsg:
		l.ref = msg.Ref
		l.gen = msg.gen
		l.selector.Select(0)
		cmds = append(cmds, l.Init())
	case LogCountMsg:
		l.count = msg.count
		l.selector.SetTotalPages(int(msg.count))
		l.selector.SetItems(make([]selector.IdentifiableItem, l.count))
		cmds = append(cmds, l.updateCommitsCmd())
	case LogItemsMsg:
		// Stop loading after receiving items, unless a commit's diff is
		// loading or shown: a reload on resize or footer toggle refreshes the
		// list behind it without leaving the diff.
		if l.selectedCommit == nil {
			l.activeView = logViewCommits
		}
		cmds = append(cmds, l.selector.SetItems(msg.items))
		l.selector.SetPage(l.nextPage)
		l.SetSize(l.common.Width, l.common.Height)
		if msg.limit > 0 && msg.limit != l.selector.PerPage() {
			// The list was resized after the load was built: on
			// footer.ToggleFooterMsg the page is resized only after the
			// Log pane builds its load. Load the page again by the size
			// the list has now, or it shows rows that were never loaded.
			cmds = append(cmds, l.updateCommitsCmd())
		}
		i := l.selector.SelectedItem()
		if i != nil {
			l.activeCommit = i.(LogItem).Commit
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
			// XXX: This is a hack for loading commits on demand based on
			// list.Pagination.
			curPage := l.selector.Page()
			cmd := l.selector.Update(msg)
			if l.selector.Page() != curPage {
				l.nextPage = l.selector.Page()
				l.selector.SetPage(curPage)
				l.startLoading()
				cmds = append(cmds, l.updateCommitsCmd())
			}
			cmds = append(cmds, cmd)
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
		cmds = append(cmds, l.updateCommitsCmd())
	case tea.WindowSizeMsg:
		// The diff is wrapped to the width, so only a width change needs
		// it rendered again.
		widthChanged := msg.Width != l.winWidth
		l.winWidth = msg.Width
		if widthChanged && l.selectedCommit != nil && l.currentDiff != nil {
			cmds = append(cmds, l.renderDiffCmd())
		}
		if l.repo != nil && l.ref != nil {
			// Reload on resize since the number of commits per page might
			// change and we'd need to load more commits. Only show the
			// spinner in the commits view, so a diff stays on screen.
			cmds = append(cmds, l.updateCommitsCmd())
			if l.activeView == logViewCommits {
				l.startLoading()
			}
		}
	case EmptyRepoMsg:
		l.countLoad.Stop()
		l.pageLoad.Stop()
		l.diffLoad.Stop()
		l.ref = nil
		l.gen = msg.gen
		l.activeView = logViewCommits
		l.nextPage = 0
		l.count = 0
		l.activeCommit = nil
		l.selectedCommit = nil
		l.clearDiff()
		l.selector.Select(0)
		cmds = append(cmds,
			l.setItems([]selector.IdentifiableItem{}),
		)
	}
	switch l.activeView {
	case logViewDiff:
		cmds = append(cmds, l.vp.Update(msg))
	}
	return tea.Batch(cmds...)
}

// View implements tea.Model.
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
	if !l.loadingTime.Add(waitBeforeLoading).Before(time.Now()) {
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
	who := utils.Sanitize(c.Author.Name)
	if email := utils.Sanitize(c.Author.Email); email != "" {
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
		// We're using l.nextPage instead of l.selector.Paginator.Page because
		// of the paginator hack above.
		return fmt.Sprintf("p. %d/%d", l.nextPage+1, l.selector.TotalPages())
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
	commit, st, width := l.selectedCommit, l.common.Styles, l.common.Width
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

// updateCommitsCmd returns a command that loads the commits of the next page.
func (l *Log) updateCommitsCmd() tea.Cmd {
	repo, ref, logger, gen := l.repo, l.ref, l.common.Logger, l.gen
	ctx := l.pageLoad.Start(l.common.Context())
	count, page, limit := l.count, l.nextPage, l.selector.PerPage()
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
			return LogItemsMsg{items: []selector.IdentifiableItem{}, gen: gen}
		}

		skip := page * limit
		items := make([]selector.IdentifiableItem, count)
		// CommitsByPage pages start at 1
		cc, err := r.CommitsByPage(ref, page+1, limit)
		if err != nil {
			logger.Debugf("error loading commits: %v", err)
			return loadError(ctx, paneLog, gen, err)
		}
		for i, c := range cc {
			idx := i + skip
			if int64(idx) >= count {
				break
			}
			items[idx] = LogItem{Commit: c}
		}
		return LogItemsMsg{items: items, limit: limit, gen: gen}
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
	return renderCommit(c, l.common.Styles, l.common.Width)
}

// renderCommit renders c's header and message width columns wide.
func renderCommit(c *git.Commit, st *styles.Styles, width int) string {
	s := strings.Builder{}
	// Strip escape sequences and control characters, CR included since
	// lipgloss prints empty lines when CRLF is used.
	msg := utils.Sanitize(c.Message)
	fmt.Fprintf(&s, "%s\n%s\n%s\n%s\n",
		st.Log.CommitHash.Render("commit "+c.ID.String()),
		st.Log.CommitAuthor.Render(fmt.Sprintf("Author: %s <%s>", utils.Sanitize(c.Author.Name), utils.Sanitize(c.Author.Email))),
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

func (l *Log) setItems(items []selector.IdentifiableItem) tea.Cmd {
	gen := l.gen
	return func() tea.Msg {
		return LogItemsMsg{items: items, gen: gen}
	}
}
