package backend

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/matryer/is"
)

// lookupCountingStore counts the lookups an access check makes, each of
// which an outage can fail.
type lookupCountingStore struct {
	store.Store
	n atomic.Int32
}

func (s *lookupCountingStore) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	s.n.Add(1)
	return s.Store.GetRepoByName(ctx, h, name)
}

func (s *lookupCountingStore) GetRepoWithCollabByName(ctx context.Context, h db.Handler, name string, userID int64) (models.Repo, access.Level, bool, error) {
	s.n.Add(1)
	return s.Store.GetRepoWithCollabByName(ctx, h, name, userID)
}

func (s *lookupCountingStore) GetCollabByUserIDAndRepoID(ctx context.Context, h db.Handler, userID int64, repoID int64) (models.Collab, error) {
	s.n.Add(1)
	return s.Store.GetCollabByUserIDAndRepoID(ctx, h, userID, repoID)
}

func (s *lookupCountingStore) GetAnonAccess(ctx context.Context, h db.Handler) (access.Level, error) {
	s.n.Add(1)
	return s.Store.GetAnonAccess(ctx, h)
}

// A registered stranger asking for a private repository costs as many
// lookups as asking for a missing name, so an outage cannot tell the two
// apart by which lookup failed. The private repository used to cost one
// more: the collaborator row was read on its own.
func TestReadableLookupsMatchForPrivateAndMissing(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	_, err := be.CreateRepository(ctx, "private", defaultAdmin(t, be), proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	is.NoErr(err)
	st := &lookupCountingStore{Store: be.store}
	be.store = st

	lookups := func(name string) int32 {
		st.n.Store(0)
		r, level, err := be.Readable(ctx, name, stranger)
		is.NoErr(err)
		is.True(r == nil)
		is.True(level < access.ReadOnlyAccess || name == "missing")
		return st.n.Load()
	}
	is.Equal(lookups("private"), lookups("missing"))
}

// With only the collaborators table failing, an existing private repository
// and a missing name answer alike, for an anonymous caller and for a
// stranger: both lookups go through the same failing query. A missing name
// used to skip it and answer as free, while the private repository failed.
func TestReadableCollabOutageAnswersAlike(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	be := newLookupBackend(t, nil)
	ctx := context.Background()
	_, err := be.CreateRepository(ctx, "private", defaultAdmin(t, be), proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	is.NoErr(err)
	breakTable(t, be, "collabs")

	for _, user := range []proto.User{nil, stranger} {
		answer := func(name string) string {
			r, level, err := be.Readable(ctx, name, user)
			return fmt.Sprintf("%v %v %v", r != nil, level, err != nil)
		}
		is.Equal(answer("private"), answer("missing"))
	}
}
