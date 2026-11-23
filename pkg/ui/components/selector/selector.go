// Package selector is the TUI component for a list of items to select from.
package selector

import (
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// Selector is a list of items that can be selected.
type Selector struct {
	*list.Model
	common     *common.Common
	zonePrefix string
	active     int
}

// IdentifiableItem is an item that can be identified by a string. Implements
// list.DefaultItem.
type IdentifiableItem interface {
	list.DefaultItem
	ID() string
}

// ItemDelegate is a wrapper around list.ItemDelegate.
type ItemDelegate interface {
	list.ItemDelegate
}

// zoneDelegate marks each rendered item with a mouse zone ID that is unique
// to its selector, so that item IDs (file names, repository names, …) cannot
// collide with the zone IDs of other components.
type zoneDelegate struct {
	ItemDelegate
	common *common.Common
	prefix string
}

// Render implements list.ItemDelegate.
func (d zoneDelegate) Render(w io.Writer, m list.Model, index int, listItem list.Item) {
	item, ok := listItem.(IdentifiableItem)
	if !ok {
		d.ItemDelegate.Render(w, m, index, listItem)
		return
	}
	var b strings.Builder
	d.ItemDelegate.Render(&b, m, index, listItem)
	fmt.Fprint(w, d.common.Zone.Mark(d.prefix+item.ID(), b.String())) //nolint:errcheck
}

// SelectMsg is a message that is sent when an item is selected.
type SelectMsg struct{ IdentifiableItem }

// ActiveMsg is a message that is sent when an item is active but not selected.
type ActiveMsg struct{ IdentifiableItem }

// New creates a new selector.
func New(c *common.Common, items []IdentifiableItem, delegate ItemDelegate) *Selector {
	itms := make([]list.Item, len(items))
	for i, item := range items {
		itms[i] = item
	}
	prefix := c.Zone.NewPrefix()
	l := list.New(itms, zoneDelegate{delegate, c, prefix}, 0, 0)
	l.Styles.NoItems = c.Styles.NoContent
	s := &Selector{
		Model:      &l,
		common:     c,
		zonePrefix: prefix,
	}
	return s
}

// PerPage returns the number of items per page.
func (s *Selector) PerPage() int {
	return s.Model.Paginator.PerPage
}

// Page returns the current page.
func (s *Selector) Page() int {
	return s.Model.Paginator.Page
}

// TotalPages returns the total number of pages.
func (s *Selector) TotalPages() int {
	return s.Model.Paginator.TotalPages
}

// SelectedItem returns the currently selected item.
func (s *Selector) SelectedItem() IdentifiableItem {
	item := s.Model.SelectedItem()
	i, ok := item.(IdentifiableItem)
	if !ok {
		return nil
	}
	return i
}

// SetSize implements common.Component.
func (s *Selector) SetSize(width, height int) {
	s.Model.SetSize(width, height)
}

// SetItems sets the items in the selector.
func (s *Selector) SetItems(items []IdentifiableItem) tea.Cmd {
	its := make([]list.Item, len(items))
	for i, item := range items {
		its[i] = item
	}
	return s.Model.SetItems(its)
}

// Init implements common.Model.
func (s *Selector) Init() tea.Cmd {
	return s.activeCmd()
}

// Update implements common.Model.
func (s *Selector) Update(msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, 0)
	// The select command is built after the list has handled msg, so it
	// carries the item the list holds once Update returns.
	var selectItem bool
	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			s.CursorUp()
		case tea.MouseWheelDown:
			s.CursorDown()
		}
	case tea.MouseClickMsg:
		switch msg.Button {
		case tea.MouseLeft:
			// Index() and Select() count from the start of the filtered
			// list, so look up the clicked item among the visible ones.
			curIdx := s.Index()
			for i, item := range s.VisibleItems() {
				item, _ := item.(IdentifiableItem)
				// Check each item to see if it's in bounds.
				if item != nil && s.common.Zone.Get(s.zonePrefix+item.ID()).InBounds(msg) {
					if i == curIdx {
						selectItem = true
					} else {
						s.Select(i)
					}
					break
				}
			}
		}
	case tea.KeyPressMsg:
		// While filtering, keys such as "?" go to the filter input.
		if key.Matches(msg, s.common.KeyMap.Select) && s.FilterState() != list.Filtering {
			selectItem = true
		}
	}
	m, cmd := s.Model.Update(msg)
	s.Model = &m
	if selectItem {
		cmds = append(cmds, s.SelectItemCmd())
	}
	cmds = append(cmds, cmd)
	// Send ActiveMsg when index change.
	if s.active != s.Index() {
		cmds = append(cmds, s.activeCmd())
	}
	s.active = s.Index()
	return tea.Batch(cmds...)
}

// View implements common.Model.
func (s *Selector) View() string {
	return s.Model.View()
}

// SelectItemCmd returns a command that selects the currently active item.
// bubbletea runs commands in their own goroutines, so the item is read here,
// in Update, and not from the command.
func (s *Selector) SelectItemCmd() tea.Cmd {
	item := s.SelectedItem()
	return func() tea.Msg {
		return SelectMsg{item}
	}
}

func (s *Selector) activeCmd() tea.Cmd {
	item := s.SelectedItem()
	return func() tea.Msg {
		return ActiveMsg{item}
	}
}

// NewPane returns a selector for a list inside a repository pane: no title,
// filter, help, pagination or status bar of its own, filtering and the quit
// keys disabled, and the shared page keys so every pane pages the same way.
func NewPane(c *common.Common, delegate ItemDelegate) *Selector {
	s := New(c, []IdentifiableItem{}, delegate)
	s.SetShowFilter(false)
	s.SetShowHelp(false)
	s.SetShowPagination(false)
	s.SetShowStatusBar(false)
	s.SetShowTitle(false)
	s.SetFilteringEnabled(false)
	s.DisableQuitKeybindings()
	s.KeyMap.NextPage = c.KeyMap.NextPage
	s.KeyMap.PrevPage = c.KeyMap.PrevPage
	return s
}
