package webhook

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-querystring/query"
	"github.com/matryer/is"
)

func TestNewRepositoryEventVisibilityChange(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)

	ev, err := e.s.NewRepositoryEvent(e.ctx, e.sender(), e.senderRepo(""), RepositoryEventActionVisibilityChange)
	is.NoErr(err)
	is.Equal(ev.EventType, EventRepositoryVisibilityChange)
	is.Equal(ev.Action, RepositoryEventActionVisibilityChange)
	is.Equal(ev.Repository.Owner.Username, "alice")
	is.Equal(ev.Repository.HTTPURL, "http://127.0.0.1:23232/repo.git")
	is.Equal(ev.Repository.SSHURL, "ssh://127.0.0.1:23231/repo.git")
	is.Equal(ev.Repository.GitURL, "git://127.0.0.1:9418/repo.git")
}

func TestNewRepositoryEventOtherActions(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)

	for _, action := range []RepositoryEventAction{
		RepositoryEventActionDelete,
		RepositoryEventActionRename,
		RepositoryEventActionDefaultBranchChange,
	} {
		ev, err := e.s.NewRepositoryEvent(e.ctx, e.sender(), e.senderRepo(""), action)
		is.NoErr(err)
		is.Equal(ev.EventType, EventRepository)
		is.Equal(ev.Action, action)
	}
}

// Events other than renames and default branch changes leave changes out.
func TestNewRepositoryEventNoChanges(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)

	ev, err := e.s.NewRepositoryEvent(e.ctx, e.sender(), e.senderRepo(""), RepositoryEventActionDelete)
	is.NoErr(err)
	is.True(ev.Changes == nil)
	b, err := json.Marshal(ev)
	is.NoErr(err)
	is.True(!strings.Contains(string(b), `"changes"`))
	v, err := query.Values(ev)
	is.NoErr(err)
	for k := range v {
		is.True(!strings.HasPrefix(k, "changes"))
	}
}

func TestNewRepositoryRenameEvent(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)

	ev, err := e.s.NewRepositoryRenameEvent(e.ctx, e.sender(), e.senderRepo(""), "old-name")
	is.NoErr(err)
	is.Equal(ev.EventType, EventRepository)
	is.Equal(ev.Action, RepositoryEventActionRename)
	b, err := json.Marshal(ev)
	is.NoErr(err)
	is.True(strings.Contains(string(b), `"changes":{"name":{"from":"old-name"}}`))
	v, err := query.Values(ev)
	is.NoErr(err)
	is.Equal(v.Get("changes[name][from]"), "old-name")
	is.Equal(v.Get("changes[default_branch][from]"), "")
}

func TestNewDefaultBranchChangeEvent(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)

	ev, err := e.s.NewDefaultBranchChangeEvent(e.ctx, e.sender(), e.senderRepo(""), "master")
	is.NoErr(err)
	is.Equal(ev.EventType, EventRepository)
	is.Equal(ev.Action, RepositoryEventActionDefaultBranchChange)
	b, err := json.Marshal(ev)
	is.NoErr(err)
	is.True(strings.Contains(string(b), `"changes":{"default_branch":{"from":"master"}}`))
	v, err := query.Values(ev)
	is.NoErr(err)
	is.Equal(v.Get("changes[default_branch][from]"), "master")
}

func TestRepositoryChangeEventsOwnerLookupFails(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)

	_, err := e.s.NewRepositoryRenameEvent(e.ctx, e.sender(), e.ownerLess(), "old")
	is.True(err != nil)
	_, err = e.s.NewDefaultBranchChangeEvent(e.ctx, e.sender(), e.ownerLess(), "old")
	is.True(err != nil)
}

func TestNewRepositoryEventOwnerLookupFails(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)

	_, err := e.s.NewRepositoryEvent(e.ctx, e.sender(), e.ownerLess(), RepositoryEventActionDelete)
	is.True(err != nil)
}
