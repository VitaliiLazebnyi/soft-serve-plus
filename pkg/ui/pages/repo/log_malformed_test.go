package repo

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/git/gittest"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// The Log tab of a repository whose history holds a commit git-module
// cannot parse lists the other commits instead of ending the session with a
// panic.
func TestLogSkipsMalformedCommit(t *testing.T) {
	for _, kind := range gittest.Malformed {
		t.Run(string(kind), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "r.git")
			gittest.Git(t, "", nil, "init", "-q", "--bare", dir)
			root := gittest.Commit(t, dir, gittest.Valid, gittest.Tree(t, dir, "f", "root\n"), "root")
			bad := gittest.Commit(t, dir, kind, gittest.Tree(t, dir, "f", "bad\n"), "bad", root)
			top := gittest.Commit(t, dir, gittest.Valid, gittest.Tree(t, dir, "f", "top\n"), "top", bad)
			gittest.Git(t, dir, nil, "update-ref", "refs/heads/main", top)
			gittest.Git(t, dir, nil, "symbolic-ref", "HEAD", "refs/heads/main")

			r, err := git.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := r.HEAD()
			if err != nil {
				t.Fatal(err)
			}
			ctx := config.WithContext(context.Background(), config.DefaultConfig())
			l := NewLog(common.NewCommon(ctx, 80, 40))
			l.SetSize(80, 40)
			l.Update(RepoMsg(openRepo{path: dir}))
			l.Update(RefMsg{Ref: ref})
			l.Update(l.countCommitsCmd()())
			msg := l.updateCommitsCmd()()
			items, ok := msg.(LogItemsMsg)
			if !ok {
				t.Fatalf("loading commits = %T %v; want LogItemsMsg", msg, msg)
			}
			l.Update(items)
			l.View()

			var got []string
			for _, it := range items.items {
				if li, ok := it.(LogItem); ok && li.Commit != nil {
					got = append(got, li.Commit.ID.String())
				}
			}
			if len(got) != 2 || got[0] != top || got[1] != root {
				t.Fatalf("listed %v, want the valid commits %v", got, []string{top, root})
			}
		})
	}
}
