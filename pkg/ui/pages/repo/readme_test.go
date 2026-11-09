package repo

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/x/ansi"
)

type readmeTestRepo struct{ proto.Repository }

func (readmeTestRepo) Name() string { return "empty-repo" }

// An empty repository has no ref, so no readme is ever loaded. The Readme
// tab must show the quick start text instead of spinning forever.
func TestReadmeEmptyRepoStopsLoading(t *testing.T) {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	r := NewReadme(common.NewCommon(ctx, 80, 40))

	// A readme shown before, for example before the repository was emptied,
	// must not leave its path behind.
	r.Update(RepoMsg(readmeTestRepo{}))
	r.Update(ReadmeMsg{Content: "# Old", Path: "docs/README.md"})
	r.Init()
	r.Update(EmptyRepoMsg{})

	view := ansi.Strip(r.View())
	if strings.Contains(view, "loading") {
		t.Fatalf("readme still shows the spinner:\n%s", view)
	}
	if !strings.Contains(view, "Quick Start") {
		t.Errorf("readme does not show the empty repository text:\n%s", view)
	}
	if got := r.StatusBarValue(); got != " " {
		t.Errorf("status bar value = %q, want no readme directory", got)
	}
}

// "soft browse" hides clone commands, and an empty local repository has no
// server to clone from, so its readme must not point at the server's URL.
func TestReadmeEmptyRepoHidesCloneCmd(t *testing.T) {
	cfg := config.DefaultConfig()
	ctx := config.WithContext(context.Background(), cfg)
	c := common.NewCommon(ctx, 80, 40)
	c.HideCloneCmd = true
	r := NewReadme(c)

	r.Update(RepoMsg(readmeTestRepo{}))
	r.Init()
	r.Update(EmptyRepoMsg{})

	view := ansi.Strip(r.View())
	if strings.Contains(view, "git clone") || strings.Contains(view, cfg.SSH.PublicURL) {
		t.Errorf("readme shows a clone command for a local repository:\n%s", view)
	}
	if !strings.Contains(view, "Empty repository") {
		t.Errorf("readme does not say the repository is empty:\n%s", view)
	}
}
