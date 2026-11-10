package repo

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

func newTestCommon() common.Common {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	return common.NewCommon(ctx, 80, 20)
}

func helpKeys(bindings []key.Binding) []string {
	keys := make([]string, 0, len(bindings))
	for _, b := range bindings {
		h := b.Help()
		keys = append(keys, h.Key+" "+h.Desc)
	}
	return keys
}

// Turning the blame view off with "b" must not also page the file up.
func TestFilesBlameKeyDoesNotScroll(t *testing.T) {
	t.Parallel()
	f := NewFiles(newTestCommon())
	f.Update(FileContentMsg{content: strings.Repeat("line\n", 200), ext: ".txt"})
	f.code.GotoBottom()
	f.blameView = true
	before := f.code.YOffset()
	f.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	if f.blameView {
		t.Fatal("b did not toggle the blame view off")
	}
	if got := f.code.YOffset(); got != before {
		t.Errorf("b scrolled the file from line %d to %d", before, got)
	}
	for _, k := range []tea.KeyPressMsg{
		{Code: 'b', Text: "b"},
		{Code: 'l', Text: "l"},
	} {
		if key.Matches(k, f.code.KeyMap.PageUp, f.code.KeyMap.Right) {
			t.Errorf("%q is still a viewport key in the file view", k.String())
		}
	}
}

// The commit list has nothing to go back to, so its help does not offer it.
func TestLogCommitListHelpHasNoBack(t *testing.T) {
	t.Parallel()
	l := NewLog(newTestCommon())
	for _, group := range l.FullHelp() {
		for _, k := range helpKeys(group) {
			if strings.Contains(k, "back") {
				t.Errorf("commit list help shows %q", k)
			}
		}
	}
}

// esc goes back to the repository menu only at the top of a tab, and there is
// no menu in soft browse.
func TestRepoBackHelp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		noMenu bool
		inDiff bool
		want   string
	}{
		{name: "menu", want: "esc back to menu"},
		{name: "sub-view", inDiff: true, want: "esc back"},
		{name: "browse sub-view", noMenu: true, inDiff: true, want: "esc back"},
		{name: "browse top", noMenu: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestCommon()
			l := NewLog(c)
			if tc.inDiff {
				l.activeView = logViewDiff
			}
			r := New(c, l)
			r.SetNoMenu(tc.noMenu)
			var got string
			for _, k := range helpKeys(r.ShortHelp()) {
				if strings.HasPrefix(k, "esc ") {
					got = k
				}
			}
			if got != tc.want {
				t.Errorf("esc help = %q, want %q", got, tc.want)
			}
		})
	}
}
