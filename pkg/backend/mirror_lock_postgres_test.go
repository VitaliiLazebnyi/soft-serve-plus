package backend

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/matryer/is"
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
// check and the write. The check used to read the row without a lock.
func TestSetMirrorLocksCheckedRowPostgres(t *testing.T) {
	is := is.New(t)
	e := newPostgresWebhookEnv(t)
	r, err := git.Open(e.be.repoPath("repo"))
	is.NoErr(err)
	is.NoErr(r.ConfigSet("remote.origin.url", "https://1.1.1.1/x.git"))
	prev := validateGitRemote
	t.Cleanup(func() { validateGitRemote = prev })
	validateGitRemote = func(context.Context, string, ssrf.GitRemotePolicy) (ssrf.ValidatedGitRemote, error) {
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

	is.NoErr(<-mirrored)
	is.NoErr(<-renamed)
	moved, err := e.be.Repository(e.ctx, "moved")
	is.NoErr(err)
	is.True(moved.IsMirror())
}
