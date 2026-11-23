package cmd

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/gitserve"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/matryer/is"
)

// anonFailsAfterStore answers the first ok reads of the anon-access setting
// and fails every one after, as a database that goes down between the
// command's access gate and the create would.
type anonFailsAfterStore struct {
	store.Store
	ok    int32
	reads *atomic.Int32
}

func (s anonFailsAfterStore) GetAnonAccess(ctx context.Context, h db.Handler) (access.Level, error) {
	if s.reads.Add(1) > s.ok {
		return access.NoAccess, errors.New("SQL logic error: no such table: settings")
	}
	return s.Store.GetAnonAccess(ctx, h)
}

// A create, import, rename onto a new name or push-to-create whose creation
// check cannot read the settings tells the client the lookup failed, keeps
// the database's message from it, and logs the cause once at Error level.
func TestCreateLookupFailureAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		run  func(ctx context.Context) error
		want error
	}{
		{"repo create", func(ctx context.Context) error { return runRepo(t, ctx, "create", "new") }, errLookupFailed},
		{"repo import", func(ctx context.Context) error {
			return runRepo(t, ctx, "import", "new", "https://127.0.0.1/x.git")
		}, errLookupFailed},
		{"repo rename", func(ctx context.Context) error { return runRepo(t, ctx, "rename", "mine", "new") }, errLookupFailed},
		{"push-to-create", func(ctx context.Context) error { return runGit(ctx, (*Commands).GitReceivePackCommand, "new") }, gitserve.ErrSystemMalfunction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			reads := &atomic.Int32{}
			// Fail from the second read on: the first is the command's own
			// access gate, the second the backend's creation check.
			ok := int32(1)
			if tc.name == "repo rename" {
				// rename reads it for the repository renamed too.
				ok = 2
			}
			ctx, be, logs := newLookupTestContextWith(t, func(s store.Store) store.Store {
				return anonFailsAfterStore{Store: s, ok: ok, reads: reads}
			})
			userCtx := withUser(t, ctx, be, "alice", false)
			_, err := be.CreateRepository(userCtx, "mine", proto.UserFromContext(userCtx), proto.RepositoryOptions{})
			is.NoErr(err)
			reads.Store(0)

			err = tc.run(userCtx)
			is.True(errors.Is(err, tc.want))
			is.True(!strings.Contains(err.Error(), "settings"))
			is.Equal(logs.count("ERRO"), 1)
			is.True(strings.Contains(logs.String(), "settings"))
			_, err = be.Repository(ctx, "new")
			is.True(errors.Is(err, proto.ErrRepoNotFound))
		})
	}
}
