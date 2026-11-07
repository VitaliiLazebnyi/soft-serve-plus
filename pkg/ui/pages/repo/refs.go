package repo

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
)

// RefMsg is a message that contains the git.Reference to show.
type RefMsg struct {
	Ref *git.Reference
	gen uint64
}

func (m RefMsg) generation() uint64 { return m.gen }

// RefItemsMsg is a message that contains a list of RefItem.
type RefItemsMsg struct {
	prefix string
	items  []selector.IdentifiableItem
	gen    uint64
}

func (m RefItemsMsg) generation() uint64 { return m.gen }

// Refs is a component that displays a list of references.
type Refs struct {
	common    common.Common
	selector  *selector.Selector
	repo      proto.Repository
	ref       *git.Reference
	activeRef *git.Reference
	refPrefix string
	spinner   spinner.Model
	isLoading bool
	gen       uint64
}

// NewRefs creates a new Refs component.
func NewRefs(common common.Common, refPrefix string) *Refs {
	r := &Refs{
		common:    common,
		refPrefix: refPrefix,
		isLoading: true,
	}
	s := selector.New(common, []selector.IdentifiableItem{}, RefItemDelegate{&common})
	s.SetShowFilter(false)
	s.SetShowHelp(false)
	s.SetShowPagination(false)
	s.SetShowStatusBar(false)
	s.SetShowTitle(false)
	s.SetFilteringEnabled(false)
	s.DisableQuitKeybindings()
	r.selector = s
	sp := spinner.New(spinner.WithSpinner(spinner.Dot),
		spinner.WithStyle(common.Styles.Spinner))
	r.spinner = sp
	return r
}

// Path implements common.TabComponent.
func (r *Refs) Path() string {
	return ""
}

// TabName returns the name of the tab.
func (r *Refs) TabName() string {
	switch r.refPrefix {
	case git.RefsHeads:
		return "Branches"
	case git.RefsTags:
		return "Tags"
	}
	return "Refs"
}

// SetSize implements common.Component.
func (r *Refs) SetSize(width, height int) {
	r.common.SetSize(width, height)
	r.selector.SetSize(width, height)
}

// ShortHelp implements help.KeyMap.
func (r *Refs) ShortHelp() []key.Binding {
	copyKey := r.common.KeyMap.Copy
	copyKey.SetHelp("c", "copy ref")
	k := r.selector.KeyMap
	return []key.Binding{
		r.common.KeyMap.SelectItem,
		k.CursorUp,
		k.CursorDown,
		copyKey,
	}
}

// FullHelp implements help.KeyMap.
func (r *Refs) FullHelp() [][]key.Binding {
	copyKey := r.common.KeyMap.Copy
	copyKey.SetHelp("c", "copy ref")
	k := r.selector.KeyMap
	return [][]key.Binding{
		{r.common.KeyMap.SelectItem},
		{
			k.CursorUp,
			k.CursorDown,
			k.NextPage,
			k.PrevPage,
		},
		{
			k.GoToStart,
			k.GoToEnd,
			copyKey,
		},
	}
}

// Init implements tea.Model.
func (r *Refs) Init() tea.Cmd {
	r.isLoading = true
	return tea.Batch(r.spinner.Tick, r.updateItemsCmd())
}

// Update implements tea.Model.
func (r *Refs) Update(msg tea.Msg) (common.Model, tea.Cmd) {
	cmds := make([]tea.Cmd, 0)
	switch msg := msg.(type) {
	case RepoMsg:
		r.selector.Select(0)
		r.repo = msg
	case RefMsg:
		r.ref = msg.Ref
		r.gen = msg.gen
		cmds = append(cmds, r.Init())
	case tea.WindowSizeMsg:
		r.SetSize(msg.Width, msg.Height)
	case RefItemsMsg:
		if r.refPrefix == msg.prefix {
			cmds = append(cmds, r.selector.SetItems(msg.items))
			i := r.selector.SelectedItem()
			if i != nil {
				r.activeRef = i.(RefItem).Reference
			}
			r.isLoading = false
		}
	case selector.ActiveMsg:
		switch sel := msg.IdentifiableItem.(type) {
		case RefItem:
			r.activeRef = sel.Reference
		}
	case selector.SelectMsg:
		switch i := msg.IdentifiableItem.(type) {
		case RefItem:
			cmds = append(cmds,
				switchRefCmd(i.Reference, r.gen),
				switchTabCmd(&Files{}),
			)
		}
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, r.common.KeyMap.SelectItem):
			cmds = append(cmds, r.selector.SelectItemCmd)
		}
	case EmptyRepoMsg:
		r.ref = nil
		r.gen = msg.gen
		cmds = append(cmds, r.setItems([]selector.IdentifiableItem{}))
	case spinner.TickMsg:
		if r.isLoading && r.spinner.ID() == msg.ID {
			s, cmd := r.spinner.Update(msg)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
			r.spinner = s
		}
	}
	m, cmd := r.selector.Update(msg)
	r.selector = m.(*selector.Selector)
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	return r, tea.Batch(cmds...)
}

// View implements tea.Model.
func (r *Refs) View() string {
	if r.isLoading {
		return renderLoading(r.common, r.spinner)
	}
	return r.selector.View()
}

// SpinnerID implements common.TabComponent.
func (r *Refs) SpinnerID() int {
	return r.spinner.ID()
}

// StatusBarValue implements statusbar.StatusBar.
func (r *Refs) StatusBarValue() string {
	if r.activeRef == nil {
		return ""
	}
	return r.activeRef.Name().String()
}

// StatusBarInfo implements statusbar.StatusBar.
func (r *Refs) StatusBarInfo() string {
	totalPages := r.selector.TotalPages()
	if totalPages <= 1 {
		return "p. 1/1"
	}
	return fmt.Sprintf("p. %d/%d", r.selector.Page()+1, totalPages)
}

func (r *Refs) updateItemsCmd() tea.Cmd {
	repo, prefix, gen := r.repo, r.refPrefix, r.gen
	return func() tea.Msg {
		return loadRefItems(r.common, repo, prefix, gen)
	}
}

func loadRefItems(c common.Common, repo proto.Repository, refPrefix string, gen uint64) tea.Msg {
	its := make(RefItems, 0)
	rr, err := repo.Open()
	if err != nil {
		return common.ErrorMsg(err)
	}
	refs, err := rr.References()
	if err != nil {
		c.Logger.Debugf("ui: error getting references: %v", err)
		return common.ErrorMsg(err)
	}
	for _, ref := range refs {
		if strings.HasPrefix(ref.Name().String(), refPrefix) {
			refItem := RefItem{
				Reference: ref,
			}

			if ref.IsTag() {
				refItem.Tag, _ = rr.Tag(ref.Name().Short())
				if refItem.Tag != nil {
					refItem.Commit, _ = refItem.Tag.Commit()
				}
			} else {
				refItem.Commit, _ = rr.CatFileCommit(ref.ID)
			}
			its = append(its, refItem)
		}
	}
	sort.Sort(its)
	items := make([]selector.IdentifiableItem, len(its))
	for i, it := range its {
		items[i] = it
	}
	return RefItemsMsg{
		items:  items,
		prefix: refPrefix,
		gen:    gen,
	}
}

func (r *Refs) setItems(items []selector.IdentifiableItem) tea.Cmd {
	prefix, gen := r.refPrefix, r.gen
	return func() tea.Msg {
		return RefItemsMsg{
			items:  items,
			prefix: prefix,
			gen:    gen,
		}
	}
}

func switchRefCmd(ref *git.Reference, gen uint64) tea.Cmd {
	return func() tea.Msg {
		return RefMsg{Ref: ref, gen: gen}
	}
}

// updateRefCmd sends a RefMsg with the reference to show first: HEAD, or a
// fallback branch when HEAD names a branch that does not exist (see
// defaultRef). It sends EmptyRepoMsg when the repository has no branches.
// gen is the generation the repository page started for repo.
func updateRefCmd(repo proto.Repository, gen uint64) tea.Cmd {
	return func() tea.Msg {
		r, err := repo.Open()
		if err != nil {
			return common.ErrorMsg(err)
		}
		bs, _ := r.Branches()
		if len(bs) == 0 {
			return EmptyRepoMsg{gen: gen}
		}
		ref, err := defaultRef(r)
		if err != nil {
			return common.ErrorMsg(err)
		}
		if ref == nil {
			return EmptyRepoMsg{gen: gen}
		}
		return RefMsg{Ref: ref, gen: gen}
	}
}

// defaultRef returns the reference to show first: HEAD, or a branch when
// HEAD names a branch that does not exist, such as in a mirror whose remote
// renamed its default branch. It prefers "main" or "master", like
// EnsureDefaultBranch does, and then the first branch. It returns nil when
// there is no branch to show.
func defaultRef(r *git.Repository) (*git.Reference, error) {
	ref, err := r.HEAD()
	if !errors.Is(err, git.ErrReferenceNotExist) {
		return ref, err
	}
	refs, _ := r.References()
	var first *git.Reference
	for _, ref := range refs {
		if !ref.IsBranch() {
			continue
		}
		if b := ref.Name().Short(); b == "main" || b == "master" {
			return ref, nil
		}
		if first == nil {
			first = ref
		}
	}
	return first, nil
}
