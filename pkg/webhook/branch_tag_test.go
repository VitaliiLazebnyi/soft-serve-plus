package webhook

import (
	"strings"
	"testing"

	"github.com/matryer/is"
)

func TestNewBranchTagEventCreate(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)

	ev, err := e.s.NewBranchTagEvent(e.ctx, e.sender(), e.senderRepo(""), "refs/heads/dev", zeroHash, "1111111111111111111111111111111111111111")
	is.NoErr(err)
	is.Equal(ev.EventType, EventBranchTagCreate)
	is.True(ev.Created)
	is.True(!ev.Deleted)
	is.Equal(ev.Ref, "refs/heads/dev")
	is.Equal(ev.Repository.Owner.Username, "alice")
	is.Equal(ev.Repository.HTTPURL, "http://127.0.0.1:23232/repo.git")
	is.Equal(ev.Repository.SSHURL, "ssh://127.0.0.1:23231/repo.git")
	is.Equal(ev.Repository.GitURL, "git://127.0.0.1:9418/repo.git")
	is.Equal(ev.Sender.Username, "alice")
}

func TestNewBranchTagEventDelete(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)

	ev, err := e.s.NewBranchTagEvent(e.ctx, e.sender(), e.senderRepo(""), "refs/tags/v1", "1111111111111111111111111111111111111111", zeroHash)
	is.NoErr(err)
	is.Equal(ev.EventType, EventBranchTagDelete)
	is.True(ev.Deleted)
	is.True(!ev.Created)
}

func TestNewBranchTagEventRejectsUpdate(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)

	// Neither side is zero: that is a plain update, not a branch or tag event.
	_, err := e.s.NewBranchTagEvent(e.ctx, e.sender(), e.senderRepo(""), "refs/heads/main",
		"1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222")
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "invalid branch or tag event"))
}

func TestNewBranchTagEventOwnerLookupFails(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)

	_, err := e.s.NewBranchTagEvent(e.ctx, e.sender(), e.ownerLess(), "refs/heads/dev", zeroHash, "1111111111111111111111111111111111111111")
	is.True(err != nil)
}
