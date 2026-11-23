package backend

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// pausingMirrorStore runs paused inside SetMirror's transaction, after its
// checks and before the flag is written.
type pausingMirrorStore struct {
	store.Store
	paused func()
}

func (s *pausingMirrorStore) SetRepoIsMirrorByName(ctx context.Context, h db.Handler, name string, isMirror bool) error {
	s.paused()
	return s.Store.SetRepoIsMirrorByName(ctx, h, name, isMirror)
}

// On Postgres SetMirror holds the row it checked until it commits, so a
// rename of the repository waits for it rather than slip in between the
// check and the write.
func TestSetMirrorLocksCheckedRowPostgres(t *testing.T) {
	t.Parallel()
	e := newPostgresWebhookEnv(t)
	r, err := git.Open(t.Context(), e.be.repoPath("repo"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ConfigSet("remote.origin.url", "https://1.1.1.1/x.git"); err != nil {
		t.Fatal(err)
	}
	e.be.validateGitRemote = func(context.Context, string, ssrf.GitRemotePolicy) (ssrf.ValidatedGitRemote, error) {
		return ssrf.ValidatedGitRemote{}, nil
	}

	checked := make(chan struct{})
	release := make(chan struct{})
	e.be.store = &pausingMirrorStore{Store: e.be.store, paused: func() {
		close(checked)
		<-release
	}}
	mirrored := make(chan error, 1)
	go func() { mirrored <- e.be.SetMirror(e.ctx, "repo", true) }()
	<-checked

	renamed := make(chan error, 1)
	go func() { renamed <- e.be.RenameRepository(e.ctx, "repo", "moved") }()
	waitForLockWaiter(t, e, renamed, "RenameRepository")
	close(release)

	if err := <-mirrored; err != nil {
		t.Fatal(err)
	}
	if err := <-renamed; err != nil {
		t.Fatal(err)
	}
	moved, err := e.be.Repository(e.ctx, "moved")
	if err != nil {
		t.Fatal(err)
	}
	if !moved.IsMirror() {
		t.Fatal("not true: moved.IsMirror()")
	}
}
