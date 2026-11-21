package webhook

import (
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/git/gittest"
	"github.com/matryer/is"
)

// TestNewPushEventSkipsMalformedCommits pushes, for each kind of commit
// git-module cannot parse, a malformed commit with a valid one on top. The
// push event leaves the malformed commit out and lists the valid one,
// instead of panicking in the hook process and sending no webhook at all.
func TestNewPushEventSkipsMalformedCommits(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	for _, kind := range gittest.Malformed {
		t.Run(string(kind), func(t *testing.T) {
			is := is.New(t)
			dir := filepath.Join(t.TempDir(), "repo.git")
			_, err := git.Init(dir, true)
			is.NoErr(err)
			bad := gittest.Commit(t, dir, kind, gittest.Tree(t, dir, "f", "bad\n"), "bad")
			top := gittest.Commit(t, dir, gittest.Valid, gittest.Tree(t, dir, "f", "top\n"), "top", bad)
			gittest.Git(t, dir, nil, "update-ref", "refs/heads/main", top)

			ev, err := e.s.NewPushEvent(e.ctx, e.sender(), e.senderRepo(dir), "refs/heads/main", zeroHash, top)
			is.NoErr(err)
			is.Equal(len(ev.Commits), 1)
			is.Equal(ev.Commits[0].ID, top)
			is.Equal(ev.Commits[0].Author.Email, "author@example.com")
		})
	}
}
