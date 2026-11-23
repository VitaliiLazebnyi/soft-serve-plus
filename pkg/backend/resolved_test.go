package backend

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
)

// swapBeforeLookupStore runs swap once, right before the first lookup of
// name by GetRepoByName, outside any transaction.
type swapBeforeLookupStore struct {
	store.Store
	name    string
	swapped atomic.Bool
	swap    func()
}

func (s *swapBeforeLookupStore) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	if name == s.name && s.swapped.CompareAndSwap(false, true) {
		s.swap()
	}
	return s.Store.GetRepoByName(ctx, h, name)
}

// swapRepo renames the repository name to moved and creates another under
// name, as another session can between two steps of a change, and returns
// the new one.
func swapRepo(t *testing.T, be *Backend, ctx context.Context, name, moved string) proto.Repository {
	t.Helper()
	if err := be.RenameRepository(ctx, name, moved); err != nil {
		t.Fatal(err)
	}
	r, err := be.CreateRepository(ctx, name, defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// collabNames returns the collaborators of the repository name.
func collabNames(t *testing.T, be *Backend, ctx context.Context, name string) []string {
	t.Helper()
	cs, err := be.Collaborators(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range cs {
		names = append(names, c.Username)
	}
	return names
}

// A collaborator added while another repository takes the name between the
// lookup and the write is not added to that other repository: the change
// is refused as the repository is gone. It used to be added to the other
// repository while the event named the first.
func TestAddCollaboratorActsOnTheRepositoryItLookedUp(t *testing.T) {
	t.Parallel()
	be, ctx, _, replacement := swapFixture(t)
	_, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	err = be.AddCollaborator(ctx, "victim", "alice", access.ReadWriteAccess)
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
	if !(replacement() != 0) {
		t.Fatal("not true: replacement() != 0")
	}
	if got := len(collabNames(t, be, ctx, "victim")); got != 0 {
		t.Fatalf("len(collabNames(t, be, ctx, \"victim\")) = %v, want %v", got, 0)
	}
}

// The same for a removal: the collaborator of the repository now under the
// name stays.
func TestRemoveCollaboratorActsOnTheRepositoryItLookedUp(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	_, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "victim", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.AddCollaborator(ctx, "victim", "alice", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	be.store = &swapAfterLookupStore{Store: be.store, name: "victim", swap: func() {
		swapRepo(t, be, ctx, "victim", "moved")
		if err := be.AddCollaborator(ctx, "victim", "alice", access.ReadOnlyAccess); err != nil {
			t.Fatal(err)
		}
	}}

	err = be.RemoveCollaborator(ctx, "victim", "alice")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
	if got, want := collabNames(t, be, ctx, "victim"), ([]string{"alice"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("collabNames(t, be, ctx, \"victim\") = %v, want %v", got, want)
	}
	if got, want := collabNames(t, be, ctx, "moved"), ([]string{"alice"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("collabNames(t, be, ctx, \"moved\") = %v, want %v", got, want)
	}
}

// The resolved setters act on the repository the caller resolved, and
// refuse when another has taken its name since.
func TestResolvedSetters(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	r, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := be.SetResolvedHidden(ctx, r, true); err != nil {
		t.Fatal(err)
	}
	if err := be.SetResolvedDescription(ctx, r, "d"); err != nil {
		t.Fatal(err)
	}
	if err := be.SetResolvedProjectName(ctx, r, "p"); err != nil {
		t.Fatal(err)
	}
	if err := be.SetResolvedPrivate(ctx, r, true); err != nil {
		t.Fatal(err)
	}
	if err := be.AddResolvedCollaborator(ctx, r, "alice", access.ReadOnlyAccess); err != nil {
		t.Fatal(err)
	}
	if err := be.RemoveResolvedCollaborator(ctx, r, "alice"); err != nil {
		t.Fatal(err)
	}
	cur, err := be.Repository(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if !(cur.IsHidden() && cur.IsPrivate()) {
		t.Fatal("not true: cur.IsHidden() && cur.IsPrivate()")
	}
	if got := cur.Description(); got != "d" {
		t.Fatalf("cur.Description() = %v, want %v", got, "d")
	}
	if got := cur.ProjectName(); got != "p" {
		t.Fatalf("cur.ProjectName() = %v, want %v", got, "p")
	}

	other := swapRepo(t, be, ctx, "repo", "moved")
	for name, err := range map[string]error{
		"SetResolvedHidden":          be.SetResolvedHidden(ctx, r, false),
		"SetResolvedDescription":     be.SetResolvedDescription(ctx, r, "x"),
		"SetResolvedProjectName":     be.SetResolvedProjectName(ctx, r, "x"),
		"SetResolvedPrivate":         be.SetResolvedPrivate(ctx, r, false),
		"AddResolvedCollaborator":    be.AddResolvedCollaborator(ctx, r, "alice", access.ReadOnlyAccess),
		"RemoveResolvedCollaborator": be.RemoveResolvedCollaborator(ctx, r, "alice"),
	} {
		if !errors.Is(err, proto.ErrRepoNotFound) {
			t.Errorf("%s on a repository whose name another has taken => %v, want ErrRepoNotFound", name, err)
		}
	}
	cur, err = be.Repository(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cur.ID(), other.ID(); got != want {
		t.Fatalf("cur.ID() = %v, want %v", got, want)
	}
	if !(!cur.IsHidden() && !cur.IsPrivate()) {
		t.Fatal("not true: !cur.IsHidden() && !cur.IsPrivate()")
	}
	if got := cur.Description(); got != "" {
		t.Fatalf("cur.Description() = %v, want %v", got, "")
	}
	if got := cur.ProjectName(); got != "" {
		t.Fatalf("cur.ProjectName() = %v, want %v", got, "")
	}
	if got := len(collabNames(t, be, ctx, "repo")); got != 0 {
		t.Fatalf("len(collabNames(t, be, ctx, \"repo\")) = %v, want %v", got, 0)
	}
}

// A mirror sync is recorded on the mirror's row, wherever it is now, and a
// mirror deleted meanwhile is not found. It used to be recorded on
// whatever had the name, and a deleted one matched no row and succeeded.
func TestRecordMirrorSyncByID(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	m, err := seedCreate(be, ctx, "m", defaultAdmin(t, be), proto.RepositoryOptions{Mirror: true})
	if err != nil {
		t.Fatal(err)
	}
	swapRepo(t, be, ctx, "m", "moved")

	if err := be.RecordMirrorSync(ctx, m, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	st, err := be.MirrorSyncStatus(ctx, "moved")
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Error; got != "boom" {
		t.Fatalf("st.Error = %v, want %v", got, "boom")
	}
	st, err = be.MirrorSyncStatus(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Error; got != "" {
		t.Fatalf("st.Error = %v, want %v", got, "")
	}

	if err := be.DeleteRepository(ctx, "moved"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(be.RecordMirrorSync(ctx, m, nil), proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(be.RecordMirrorSync(ctx, m, nil), proto.ErrRepoNotFound)")
	}
}

// The events of a visibility change and a rename name the repository the
// change was made to, even if another takes its name before the event is
// built: they go to that repository's webhooks. They used to be built from
// a second lookup of the name.
func TestChangeEventsNameTheChangedRepository(t *testing.T) {
	t.Parallel()
	t.Run("SetPrivate", func(t *testing.T) {
		t.Parallel()
		e := newWebhookEnv(t)
		id := e.seedHook(t, webhook.EventRepositoryVisibilityChange)
		e.be.store = &swapBeforeLookupStore{Store: e.be.store, name: "repo", swap: func() {
			swapRepo(t, e.be, e.ctx, "repo", "moved")
		}}
		if err := e.be.SetPrivate(e.ctx, "repo", true); err != nil {
			t.Fatal(err)
		}
		e.awaitDeliveries(t, id, webhook.EventRepositoryVisibilityChange)
	})
	t.Run("RenameRepository", func(t *testing.T) {
		t.Parallel()
		e := newWebhookEnv(t)
		id := e.seedHook(t, webhook.EventRepository)
		e.be.store = &swapBeforeLookupStore{Store: e.be.store, name: "renamed", swap: func() {
			swapRepo(t, e.be, e.ctx, "renamed", "moved")
		}}
		if err := e.be.RenameRepository(e.ctx, "repo", "renamed"); err != nil {
			t.Fatal(err)
		}
		// The first delivery is this rename's. Built from a second lookup,
		// it went to the new repository, which has no webhook, and the only
		// delivery was the swap's own rename.
		waitFor(t, "the rename's delivery", func() bool {
			list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
			return err == nil && len(list) > 0
		})
		list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
		if err != nil {
			t.Fatal(err)
		}
		d, err := e.be.WebhookDelivery(e.ctx, e.repo, id, list[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(d.RequestBody, `"changes":{"name":{"from":"repo"}}`) {
			t.Fatal("not true: strings.Contains(d.RequestBody, `\"changes\":{\"name\":{\"from\":\"repo\"}}`)")
		}
	})
}
