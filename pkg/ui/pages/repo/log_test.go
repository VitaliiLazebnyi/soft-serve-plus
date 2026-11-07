package repo

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// Toggling the footer resizes the commit list only after the Log pane has
// built its load, so the load must page by the size the list has when it
// runs, or the page shown after the resize has rows that were never loaded.
func TestLogLoadUsesPageSizeWhenItRuns(t *testing.T) {
	repo := newDiskRepo(t, "r", "main", "# r\n")
	for range 40 {
		gitRun(t, repo.path, "commit", "-q", "--allow-empty", "-m", "c")
	}

	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	c := common.NewCommon(ctx, 80, 40)
	l := NewLog(c)
	l.SetSize(80, 40)
	l.Update(RepoMsg(repo))
	ref := updateRefCmd(repo, 1)()
	refMsg, ok := ref.(RefMsg)
	if !ok {
		t.Fatalf("updateRefCmd = %T %v; want a RefMsg", ref, ref)
	}
	l.Update(refMsg)
	l.Update(l.countCommitsCmd()())

	// Go to the second page.
	l.nextPage = 1
	l.Update(l.updateCommitsCmd()())

	// The footer is toggled: the load is built, then the list shrinks.
	load := l.updateCommitsCmd()
	before := l.selector.PerPage()
	l.SetSize(80, 20)
	after := l.selector.PerPage()
	if after >= before {
		t.Fatalf("per page %d after shrinking, %d before; want fewer", after, before)
	}
	l.Update(load())

	items := l.selector.Items()
	for i := l.nextPage * after; i < (l.nextPage+1)*after && i < len(items); i++ {
		if it, ok := items[i].(LogItem); !ok || it.Commit == nil {
			t.Fatalf("row %d on page %d is not loaded after the resize", i, l.nextPage+1)
		}
	}
}
