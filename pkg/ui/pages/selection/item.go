package selection

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/dustin/go-humanize"
)

var _ sort.Interface = Items{}

// Items is a list of Item.
type Items []Item

// Len implements sort.Interface.
func (it Items) Len() int {
	return len(it)
}

// Less implements sort.Interface.
func (it Items) Less(i int, j int) bool {
	if it[i].lastUpdate == nil && it[j].lastUpdate != nil {
		return false
	}
	if it[i].lastUpdate != nil && it[j].lastUpdate == nil {
		return true
	}
	if it[i].lastUpdate == nil && it[j].lastUpdate == nil {
		return it[i].repo.Name() < it[j].repo.Name()
	}
	return it[i].lastUpdate.After(*it[j].lastUpdate)
}

// Swap implements sort.Interface.
func (it Items) Swap(i int, j int) {
	it[i], it[j] = it[j], it[i]
}

// Item represents a single item in the selector.
type Item struct {
	repo       proto.Repository
	lastUpdate *time.Time
	cmd        string
}

// NewItem creates a new Item.
func NewItem(c common.Common, repo proto.Repository) Item {
	var lastUpdate *time.Time
	lu := repo.UpdatedAt()
	if !lu.IsZero() {
		lastUpdate = &lu
	}
	var cmd string
	if cfg := c.Config(); cfg != nil {
		cmd = c.CloneCmd(cfg.SSH.PublicURL, repo.Name())
	}
	return Item{
		repo:       repo,
		lastUpdate: lastUpdate,
		cmd:        cmd,
	}
}

// ID implements selector.IdentifiableItem.
func (i Item) ID() string {
	return i.repo.Name()
}

// Title returns the item title. Implements list.DefaultItem.
func (i Item) Title() string {
	name := singleLine(utils.Sanitize(i.repo.ProjectName()))
	if name == "" {
		name = i.repo.Name()
	}

	return name
}

// Description returns the item description. Implements list.DefaultItem.
func (i Item) Description() string { return singleLine(utils.Sanitize(i.repo.Description())) }

// singleLine collapses every run of whitespace, line breaks included, into a
// single space so that a value fits on the one line the list gives it.
func singleLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// FilterValue implements list.Item. It holds the project name followed by
// the repository name, so filtering matches either.
func (i Item) FilterValue() string {
	title := i.Title()
	if name := i.repo.Name(); name != title {
		return title + " " + name
	}
	return title
}

// Command returns the item Command view.
func (i Item) Command() string {
	return i.cmd
}

// ItemDelegate is the delegate for the item.
type ItemDelegate struct {
	common     *common.Common
	activePane *pane
	copiedID   string
}

// NewItemDelegate creates a new ItemDelegate.
func NewItemDelegate(common *common.Common, activePane *pane) *ItemDelegate {
	return &ItemDelegate{
		common:     common,
		activePane: activePane,
	}
}

// Height returns the item height. Implements list.ItemDelegate.
func (d *ItemDelegate) Height() int {
	height := d.common.Styles.MenuItem.GetVerticalFrameSize() + d.common.Styles.MenuItem.GetHeight()
	return height
}

// Spacing returns the spacing between items. Implements list.ItemDelegate.
func (d *ItemDelegate) Spacing() int { return 1 }

// Update implements list.ItemDelegate.
func (d *ItemDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd {
	item, ok := m.SelectedItem().(Item)
	if !ok {
		return nil
	}
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.common.KeyMap.Copy):
			d.copiedID = item.ID()
			return tea.Batch(
				tea.SetClipboard(item.Command()),
				m.SetItem(m.GlobalIndex(), item),
			)
		}
	}
	return nil
}

// Render implements list.ItemDelegate.
func (d *ItemDelegate) Render(w io.Writer, m list.Model, index int, listItem list.Item) {
	i := listItem.(Item)
	s := strings.Builder{}
	var matchedRunes []int

	// Conditions
	var (
		isSelected = index == m.Index()
		isFiltered = m.FilterState() == list.Filtering || m.FilterState() == list.FilterApplied
	)

	styles := d.common.Styles.RepoSelector.Normal
	if isSelected {
		styles = d.common.Styles.RepoSelector.Active
	}

	// A long title is truncated to leave room for the visibility marker.
	suffix := " " + common.VisibilityMarker(i.repo.IsPrivate())
	if isSelected {
		suffix += " "
	}
	title := i.Title()
	title = common.TruncateString(title, m.Width()-styles.Base.GetHorizontalFrameSize()-lipgloss.Width(suffix))
	titleRunes := utf8.RuneCountInString(title)
	title += suffix
	var updatedStr string
	if i.lastUpdate != nil {
		updatedStr = fmt.Sprintf(" Updated %s", humanize.Time(*i.lastUpdate))
	}
	if m.Width()-styles.Base.GetHorizontalFrameSize()-lipgloss.Width(updatedStr)-lipgloss.Width(title) <= 0 {
		updatedStr = ""
	}
	updatedStyle := styles.Updated.
		Align(lipgloss.Right).
		Width(m.Width() - styles.Base.GetHorizontalFrameSize() - lipgloss.Width(title))
	updated := updatedStyle.Render(updatedStr)

	if isFiltered && index < len(m.VisibleItems()) {
		// Get indices of matched characters. Only those in the title are
		// highlighted: the rest match the repository name, which is not
		// shown here.
		for _, r := range m.MatchesForItem(index) {
			if r < titleRunes {
				matchedRunes = append(matchedRunes, r)
			}
		}
	}

	if isFiltered {
		unmatched := styles.Title.Inline(true)
		matched := unmatched.Underline(true)
		title = lipgloss.StyleRunes(title, matchedRunes, matched, unmatched)
	}
	title = styles.Title.Render(title)
	desc := i.Description()
	desc = common.TruncateString(desc, m.Width()-styles.Base.GetHorizontalFrameSize())
	desc = styles.Desc.Render(desc)

	s.WriteString(lipgloss.JoinHorizontal(lipgloss.Bottom, title, updated))
	s.WriteRune('\n')
	s.WriteString(desc)
	s.WriteRune('\n')

	cmd := i.Command()
	cmdStyler := styles.Command.Render
	if d.copiedID == i.ID() {
		cmd = "(copied to clipboard)"
		cmdStyler = styles.Desc.Render
		d.copiedID = ""
	}
	cmd = common.TruncateString(cmd, m.Width()-styles.Base.GetHorizontalFrameSize())
	s.WriteString(cmdStyler(cmd))
	fmt.Fprint(w, styles.Base.Render(s.String())) //nolint:errcheck
}
