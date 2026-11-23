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
// apart by which lookup failed.
func TestReadableLookupsMatchForPrivateAndMissing(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	_, err := be.CreateRepository(ctx, "private", defaultAdmin(t, be), proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st := &lookupCountingStore{Store: be.store}
	be.store = st

	lookups := func(name string) int32 {
		st.n.Store(0)
		r, level, err := be.Readable(ctx, name, stranger)
		if err != nil {
			t.Fatal(err)
		}
		if r != nil {
			t.Fatal("not true: r == nil")
		}
		if level >= access.ReadOnlyAccess && name != "missing" {
			t.Fatal("not true: level < access.ReadOnlyAccess || name == \"missing\"")
		}
		return st.n.Load()
	}
	if got, want := lookups("private"), lookups("missing"); got != want {
		t.Fatalf("lookups(\"private\") = %v, want %v", got, want)
	}
}

// With only the collaborators table failing, an existing private repository
// and a missing name answer alike, for an anonymous caller and for a
// stranger: both lookups go through the same failing query, so a missing
// name does not skip it and answer as free.
func TestReadableCollabOutageAnswersAlike(t *testing.T) {
	t.Parallel()
	be := newLookupBackend(t, nil)
	ctx := context.Background()
	_, err := be.CreateRepository(ctx, "private", defaultAdmin(t, be), proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	breakTable(t, be, "collabs")

	for _, user := range []proto.User{nil, stranger} {
		answer := func(name string) string {
			r, level, err := be.Readable(ctx, name, user)
			return fmt.Sprintf("%v %v %v", r != nil, level, err != nil)
		}
		if got, want := answer("private"), answer("missing"); got != want {
			t.Fatalf("answer(\"private\") = %v, want %v", got, want)
		}
	}
}
