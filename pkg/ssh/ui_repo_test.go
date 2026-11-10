package ssh

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/repo"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/selection"
	_ "modernc.org/sqlite" // sqlite driver
)

// newRepoTestUI returns a UI backed by a real backend that holds the
// repositories "a" and "b". The pages are built without Init, which would load
// the repository list.
func newRepoTestUI(t *testing.T) *UI {
	t.Helper()
	t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
	ctx := context.Background()
	cfg := config.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx = config.WithContext(ctx, cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbx.Close() })
	if err := migrate.Migrate(ctx, dbx); err != nil {
		t.Fatal(err)
	}
	dbstore := database.New(ctx, dbx)
	ctx = store.WithContext(ctx, dbstore)
	be := backend.New(ctx, cfg, dbx, dbstore)
	ctx = backend.WithContext(ctx, be)
	for _, name := range []string{"a", "b"} {
		if _, err := be.CreateRepository(ctx, name, nil, proto.RepositoryOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	c := common.NewCommon(ctx, 80, 40)
	ui := NewUI(c, "")
	ui.pages[selectionPage] = selection.New(ui.common)
	ui.pages[repoPage] = repo.New(ui.common,
		repo.NewReadme(ui.common),
		repo.NewFiles(ui.common),
		repo.NewLog(ui.common),
		repo.NewRefs(ui.common, git.RefsHeads),
		repo.NewRefs(ui.common, git.RefsTags),
	)
	ui.state = readyState
	t.Cleanup(ui.common.Zone.Close)
	return ui
}

// shownRepo returns the name of the repository the UI shows, or "" on the
// selection page.
func shownRepo(t *testing.T, ui *UI) string {
	t.Helper()
	if ui.activePage != repoPage {
		return ""
	}
	rp, ok := ui.pages[repoPage].(*repo.Repo)
	if !ok {
		t.Fatalf("repository page is %T", ui.pages[repoPage])
	}
	r := rp.Repository()
	if r == nil {
		t.Fatal("repository page shown without a repository")
	}
	return r.Name()
}

// TestUIDropsLateRepoOpen opens "a" and then "b", and delivers the result for
// "a" last. The UI must keep showing "b".
func TestUIDropsLateRepoOpen(t *testing.T) {
	ui := newRepoTestUI(t)

	openA := ui.openRepoCmd("a", false)
	openB := ui.openRepoCmd("b", false)
	msgB := openB()
	msgA := openA()

	ui.Update(msgB)
	if got := shownRepo(t, ui); got != "b" {
		t.Fatalf("after opening b the UI shows %q", got)
	}
	ui.Update(msgA)
	if got := shownRepo(t, ui); got != "b" {
		t.Fatalf("a late open of a replaced b: the UI shows %q", got)
	}
}

// TestUIDropsLateRepoOpenError checks that a failed open the user has moved on
// from does not show the error screen over the current repository.
func TestUIDropsLateRepoOpenError(t *testing.T) {
	ui := newRepoTestUI(t)

	openMissing := ui.openRepoCmd("missing", false)
	openB := ui.openRepoCmd("b", false)
	ui.Update(openB())
	ui.Update(openMissing())
	if ui.state == errorState {
		t.Fatalf("a late error was shown: %v", ui.error)
	}
	if got := shownRepo(t, ui); got != "b" {
		t.Fatalf("the UI shows %q, want b", got)
	}

	// The newest open still reports its error.
	ui.Update(ui.openRepoCmd("missing", false)())
	if ui.state != errorState || !errors.Is(ui.error, common.ErrMissingRepo) {
		t.Fatalf("state %v, error %v; want the missing repository error", ui.state, ui.error)
	}
}

// TestUIInitialRepoOpen checks the initial repository: it is shown when it
// opens, a missing one leaves the selection page without an error, and one
// that finishes after the user picked another repository is dropped.
func TestUIInitialRepoOpen(t *testing.T) {
	ui := newRepoTestUI(t)
	ui.Update(ui.openRepoCmd("a", true)())
	if got := shownRepo(t, ui); got != "a" {
		t.Fatalf("initial repository: the UI shows %q, want a", got)
	}

	ui = newRepoTestUI(t)
	ui.Update(ui.openRepoCmd("missing", true)())
	if ui.state == errorState || ui.activePage != selectionPage {
		t.Fatalf("a missing initial repository changed the UI: state %v, page %v", ui.state, ui.activePage)
	}

	ui = newRepoTestUI(t)
	initial := ui.openRepoCmd("a", true)
	ui.Update(ui.openRepoCmd("b", false)())
	ui.Update(initial())
	if got := shownRepo(t, ui); got != "b" {
		t.Fatalf("a late initial repository replaced b: the UI shows %q", got)
	}
}

// TestUIRepoOpenDoesNotReadModel runs an open command while Update shows
// another repository, which replaces the UI's context. The command must work
// on the values copied when it was built; under -race, reading the UI from
// the command goroutine is reported as a data race.
func TestUIRepoOpenDoesNotReadModel(t *testing.T) {
	ui := newRepoTestUI(t)
	rb, err := ui.common.Backend().Repository(ui.common.Context(), "b")
	if err != nil {
		t.Fatal(err)
	}

	for range 20 {
		open := ui.openRepoCmd("a", false)
		var wg sync.WaitGroup
		var msg any
		wg.Add(1)
		go func() {
			defer wg.Done()
			msg = open()
		}()
		ui.Update(repo.RepoMsg(rb))
		wg.Wait()
		m, ok := msg.(repoOpenedMsg)
		if !ok || m.err != nil || m.repo == nil || m.repo.Name() != "a" {
			t.Fatalf("open returned %#v, want repository a", msg)
		}
	}
}
