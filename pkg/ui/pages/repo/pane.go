package repo

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/code"
)

// paneID names a tab of the repository page.
type paneID int

const (
	paneReadme paneID = iota
	paneFiles
	paneLog
	paneStash
	paneBranches
	paneTags
	paneRefs
)

// refsPane returns the tab that lists the references under prefix.
func refsPane(prefix string) paneID {
	switch prefix {
	case git.RefsHeads:
		return paneBranches
	case git.RefsTags:
		return paneTags
	}
	return paneRefs
}

// Pane is a tab of the repository page.
type Pane interface {
	common.Component

	// StatusBarValue returns the status bar value component.
	StatusBarValue() string

	// StatusBarInfo returns the status bar info component.
	StatusBarInfo() string

	// TabName returns the name of the tab.
	TabName() string

	// Path returns the hierarchical path of the tab, or "" when no
	// sub-view is open.
	Path() string

	// Loading reports whether the tab waits for something to load, and
	// the text to show next to the spinner meanwhile. The repository page
	// turns one spinner while any tab waits, and shows it in place of the
	// tab unless the text is empty.
	Loading() (text string, ok bool)

	id() paneID
}

// codeView is a tab with a code view, whose renders are code.RenderedMsg.
type codeView interface {
	ownsRender(msg code.RenderedMsg) bool
}

// paneMsg is the result of a load a tab started. The repository page
// delivers it to that tab, whichever tab is active.
type paneMsg interface {
	pane() paneID
}

// paneRepo stands for the repository page itself, which loads the
// reference to show.
const paneRepo paneID = -1

// errorMsg reports that a load failed. Like the load's result, it carries
// the generation and the pane the load was started for: the repository page
// drops it when its generation is stale, hands it to that pane, which leaves
// its loading view, and then reports the error to the error screen.
type errorMsg struct {
	err error
	to  paneID
	gen uint64
}

func (m errorMsg) generation() uint64 { return m.gen }

func (m errorMsg) pane() paneID { return m.to }

// loadError returns the message for a load of pane to, started in
// generation gen, that failed with err: nothing when the load was
// cancelled, because a newer one replaces it or the session has ended, or
// else an errorMsg.
func loadError(ctx context.Context, to paneID, gen uint64, err error) tea.Msg {
	if ctx.Err() != nil {
		return nil
	}
	return errorMsg{err: err, to: to, gen: gen}
}
