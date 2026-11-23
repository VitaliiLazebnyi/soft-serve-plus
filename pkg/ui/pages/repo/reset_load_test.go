package repo

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
)

// Opening a file from the listing shown while another directory loads does
// not cancel that listing: when the file fails to open, the pane still shows
// the directory it is in.
func TestFilesFileLoadKeepsListing(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "sub", "inner.txt"), "inner\n")
	writeFile(t, filepath.Join(dir, "bin.dat"), "\x00\x01\x02binary")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "first")
	gr, err := git.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := gr.HEAD()
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	f := NewFiles(uitest.NewCommon(t, ctx, nil, nil))
	f.SetSize(80, 40)
	f.Update(OpenedMsg(openRepo{path: dir}))
	f.Update(findMsg[FileItemsMsg](t, runCmd(f.Update(RefMsg{Ref: ref}))))
	item := func(name string) selector.SelectMsg {
		for _, it := range f.selector.Items() {
			if it.(selector.IdentifiableItem).ID() == name {
				return selector.SelectMsg{IdentifiableItem: it.(selector.IdentifiableItem)}
			}
		}
		t.Fatalf("no item %q", name)
		return selector.SelectMsg{}
	}

	// Enter sub, then open bin.dat from the root listing still on screen,
	// before sub is listed.
	listing := f.Update(item("sub"))
	file := f.Update(item("bin.dat"))
	for _, m := range runCmd(file) {
		f.Update(m)
	}
	for _, m := range runCmd(listing) {
		f.Update(m)
	}
	var names []string
	for _, it := range f.selector.Items() {
		names = append(names, it.(selector.IdentifiableItem).ID())
	}
	if f.Path() != "sub" || len(names) != 1 || names[0] != "inner.txt" {
		t.Fatalf("path %q lists %v; want sub listing inner.txt", f.Path(), names)
	}
}

// A load is cancelled when its pane resets, so a late result cannot switch
// the pane's view or do work for nothing.
func TestPaneResetCancelsLoads(t *testing.T) {
	t.Parallel()
	stashItem := func(p *pinPage) selector.SelectMsg {
		return selector.SelectMsg{IdentifiableItem: p.stash.list.SelectedItem()}
	}
	cases := map[string]func(p *pinPage) tea.Cmd{
		"stash patch, back": func(p *pinPage) tea.Cmd {
			cmd := p.stash.Update(stashItem(p))
			p.stash.Update(GoBackMsg{})
			return cmd
		},
		"stash patch, new ref": func(p *pinPage) tea.Cmd {
			cmd := p.stash.Update(stashItem(p))
			p.stash.Update(RefMsg{Ref: p.r.ref, gen: p.r.gen})
			return cmd
		},
		"log diff, new ref": func(p *pinPage) tea.Cmd {
			sel := p.log.selector.SelectedItem()
			cmd := p.log.Update(selector.SelectMsg{IdentifiableItem: sel})
			p.log.Update(RefMsg{Ref: p.r.ref, gen: p.r.gen})
			return cmd
		},
		"log page, new ref": func(p *pinPage) tea.Cmd {
			cmd := p.log.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			p.log.Update(RefMsg{Ref: p.r.ref, gen: p.r.gen})
			return cmd
		},
		"log count, empty": func(p *pinPage) tea.Cmd {
			cmd := p.log.Update(RefMsg{Ref: p.r.ref, gen: p.r.gen})
			p.log.Update(EmptyRepoMsg{gen: p.r.gen})
			return cmd
		},
		"log diff, empty": func(p *pinPage) tea.Cmd {
			cmd := p.log.Update(selector.SelectMsg{IdentifiableItem: p.log.selector.SelectedItem()})
			p.log.Update(EmptyRepoMsg{gen: p.r.gen})
			return cmd
		},
		"log page, empty": func(p *pinPage) tea.Cmd {
			cmd := p.log.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			p.log.Update(EmptyRepoMsg{gen: p.r.gen})
			return cmd
		},
		"readme, empty": func(p *pinPage) tea.Cmd {
			cmd := p.readme.Init()
			p.readme.Update(EmptyRepoMsg{gen: p.r.gen})
			return cmd
		},
		"refs, empty": func(p *pinPage) tea.Cmd {
			cmd := p.branches.Init()
			p.branches.Update(EmptyRepoMsg{gen: p.r.gen})
			return cmd
		},
	}
	for name, start := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newPinPage(t)
			p.open()
			noLoadResult(t, runCmd(start(p)))
		})
	}
}
