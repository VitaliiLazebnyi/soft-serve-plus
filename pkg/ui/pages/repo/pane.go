package repo

import (
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
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

// paneMsg is the result of a load a tab started. The repository page
// delivers it to that tab, whichever tab is active.
type paneMsg interface {
	pane() paneID
}
