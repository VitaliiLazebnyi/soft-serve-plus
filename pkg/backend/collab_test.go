package backend

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
)

func TestCollaboratorEventsAddAndRemove(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := e.seedHook(t, webhook.EventCollaborator)

	if err := e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	e.awaitDeliveries(t, id, webhook.EventCollaborator)
	if err := e.be.RemoveCollaborator(e.ctx, "repo", "bob"); err != nil {
		t.Fatal(err)
	}

	e.awaitDeliveries(t, id, webhook.EventCollaborator, webhook.EventCollaborator)
}

// A failed event does not undo committed collaborator changes.
func TestCollaboratorChangesKeepWhenEventFails(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	bob, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	e.seedHook(t, webhook.EventCollaborator)
	_, err = e.be.db.ExecContext(e.ctx, "DROP TABLE webhook_deliveries")
	if err != nil {
		t.Fatal(err)
	}

	if err := e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	// Its event fails to be recorded; wait for that to finish before the
	// next change, which would otherwise read while the record writes.
	e.awaitLog(t, "webhook event not delivered")
	if got := levelOf(t, e.be, e.ctx, "repo", bob); got != access.ReadWriteAccess {
		t.Fatalf("levelOf(t, e.be, e.ctx, \"repo\", bob) = %v, want %v", got, access.ReadWriteAccess)
	}

	if err := e.be.RemoveCollaborator(e.ctx, "repo", "bob"); err != nil {
		t.Fatal(err)
	}
	// The repository is public, so a non-collaborator gets read-only.
	if got := levelOf(t, e.be, e.ctx, "repo", bob); got != access.ReadOnlyAccess {
		t.Fatalf("levelOf(t, e.be, e.ctx, \"repo\", bob) = %v, want %v", got, access.ReadOnlyAccess)
	}
	// Wait for both events, so neither is still writing under the test's
	// temporary directory when it is removed.
	e.be.WaitForDeliveries(e.ctx)
	if got := strings.Count(e.logs.String(), "webhook event not delivered"); got != 2 {
		t.Fatalf("strings.Count(e.logs.String(), \"webhook event not delivered\") = %v, want %v", got, 2)
	}
}

func TestRemoveCollaboratorNotFound(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := e.seedHook(t, webhook.EventCollaborator)

	if !errors.Is(e.be.RemoveCollaborator(e.ctx, "repo", "bob"), proto.ErrCollaboratorNotFound) {
		t.Fatal("not true: errors.Is(e.be.RemoveCollaborator(e.ctx, \"repo\", \"bob\"), proto.ErrCollaboratorNotFound)")
	}
	e.be.WaitForDeliveries(e.ctx)
	if got := len(e.deliveredEvents(t, id)); got != 0 {
		t.Fatalf("len(e.deliveredEvents(t, id)) = %v, want %v", got, 0)
	}
}

// Collaborator changes validate the username before anything is written.
func TestAddCollaboratorRejectsInvalidUsername(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	if e.be.AddCollaborator(e.ctx, "repo", "bad name!", access.ReadWriteAccess) == nil {
		t.Fatal("not true: e.be.AddCollaborator(e.ctx, \"repo\", \"bad name!\", access.ReadWriteAccess) != nil")
	}
}

// Adding a collaborator twice is refused, and no second event is sent.
func TestAddCollaboratorTwice(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := e.seedHook(t, webhook.EventCollaborator)
	if err := e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	e.awaitDeliveries(t, id, webhook.EventCollaborator)

	if !errors.Is(e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess), proto.ErrCollaboratorExist) {
		t.Fatal("not true: errors.Is(e.be.AddCollaborator(e.ctx, \"repo\", \"bob\", access.ReadWriteAccess), proto.ErrCollaboratorExist)")
	}
	e.awaitDeliveries(t, id, webhook.EventCollaborator)
}

// Adding or removing a collaborator whose event cannot be built still commits
// and returns nil; the skipped event is logged.
func TestCollaboratorEventBuildFailure(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	bob, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	corruptOwner(t, e)

	if err := e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	if got := levelOf(t, e.be, e.ctx, "repo", bob); got != access.ReadWriteAccess {
		t.Fatalf("levelOf(t, e.be, e.ctx, \"repo\", bob) = %v, want %v", got, access.ReadWriteAccess)
	}

	if err := e.be.RemoveCollaborator(e.ctx, "repo", "bob"); err != nil {
		t.Fatal(err)
	}
	// The repository is public, so a non-collaborator gets read-only.
	if got := levelOf(t, e.be, e.ctx, "repo", bob); got != access.ReadOnlyAccess {
		t.Fatalf("levelOf(t, e.be, e.ctx, \"repo\", bob) = %v, want %v", got, access.ReadOnlyAccess)
	}
	if !strings.Contains(e.logs.String(), "collaborator added but webhook event skipped") {
		t.Fatal("not true: strings.Contains(e.logs.String(), \"collaborator added but webhook event skipped\")")
	}
	if !strings.Contains(e.logs.String(), "collaborator removed but webhook event skipped") {
		t.Fatal("not true: strings.Contains(e.logs.String(), \"collaborator removed but webhook event skipped\")")
	}

	// A missing collaborator is still reported as such.
	if !errors.Is(e.be.RemoveCollaborator(e.ctx, "repo", "bob"), proto.ErrCollaboratorNotFound) {
		t.Fatal("not true: errors.Is(e.be.RemoveCollaborator(e.ctx, \"repo\", \"bob\"), proto.ErrCollaboratorNotFound)")
	}
}

// A failing collaborator table makes both operations return the error.
func TestCollaboratorStoreFailure(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.be.db.ExecContext(e.ctx, "DROP TABLE collabs")
	if err != nil {
		t.Fatal(err)
	}

	if e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess) == nil {
		t.Fatal("not true: e.be.AddCollaborator(e.ctx, \"repo\", \"bob\", access.ReadWriteAccess) != nil")
	}
	if e.be.RemoveCollaborator(e.ctx, "repo", "bob") == nil {
		t.Fatal("not true: e.be.RemoveCollaborator(e.ctx, \"repo\", \"bob\") != nil")
	}
}

// Removing a row that a trigger refuses to delete returns the database error.
func TestRemoveCollaboratorDeleteFailure(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	e.execTrigger(t, "fail_collab_delete", "BEFORE", "DELETE", "collabs")

	if e.be.RemoveCollaborator(e.ctx, "repo", "bob") == nil {
		t.Fatal("not true: e.be.RemoveCollaborator(e.ctx, \"repo\", \"bob\") != nil")
	}
}

// If the collaborator row vanishes between the lookup and the delete, the
// removal reports not found. A trigger deletes the row first, so the outer
// delete matches nothing. On Postgres the trigger skips the outer delete of
// the row it removed, which would fail otherwise, and does not fire for its
// own delete.
func TestRemoveCollaboratorRowVanishes(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	execAll(t, e.ctx, e.be.db, byDriver(e.be.db,
		[]string{"CREATE TRIGGER vanish BEFORE DELETE ON collabs BEGIN DELETE FROM collabs WHERE rowid = OLD.rowid; END;"},
		[]string{
			"CREATE FUNCTION vanish() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN DELETE FROM collabs WHERE id = OLD.id; RETURN NULL; END $$;",
			"CREATE TRIGGER vanish BEFORE DELETE ON collabs FOR EACH ROW WHEN (pg_trigger_depth() < 1) EXECUTE FUNCTION vanish();",
		}))

	if !errors.Is(e.be.RemoveCollaborator(e.ctx, "repo", "bob"), proto.ErrCollaboratorNotFound) {
		t.Fatal("not true: errors.Is(e.be.RemoveCollaborator(e.ctx, \"repo\", \"bob\"), proto.ErrCollaboratorNotFound)")
	}
}

// afterLookupStore calls after once, inside the first GetRepoByName after it
// is set, once the lookup has returned its row.
type afterLookupStore struct {
	store.Store
	after func()
}

func (s *afterLookupStore) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	m, err := s.Store.GetRepoByName(ctx, h, name)
	if s.after != nil {
		after := s.after
		s.after = nil
		after()
	}
	return m, err
}

// On Postgres a read transaction sees what other transactions commit
// between its statements. A rename of the checked repository and a create
// under its old name, committed right after the ID check, must not make
// ResolvedCollaborators list the new repository's collaborators: they are
// read by the checked ID. SQLite reads from one snapshot, so only Postgres
// can show it.
func TestResolvedCollaboratorsReadByCheckedID(t *testing.T) {
	t.Parallel()
	e := newPostgresWebhookEnv(t)
	for _, name := range []string{"alice", "bob"} {
		if _, err := e.be.CreateUser(e.ctx, name, proto.UserOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.be.AddCollaborator(e.ctx, "repo", "alice", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}

	st := &afterLookupStore{Store: e.be.store}
	e.be.store = st
	st.after = func() {
		if err := st.Store.SetRepoNameByName(e.ctx, e.be.db, "repo", "moved"); err != nil {
			t.Error(err)
			return
		}
		if _, err := st.Store.CreateRepo(e.ctx, e.be.db, models.Repo{Name: "repo", UserID: e.admin.ID()}); err != nil {
			t.Error(err)
			return
		}
		if err := st.Store.AddCollabByUsernameAndRepo(e.ctx, e.be.db, "bob", "repo", access.ReadOnlyAccess); err != nil {
			t.Error(err)
		}
	}

	got, err := e.be.ResolvedCollaborators(e.ctx, e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Collaborator{{Username: "alice", AccessLevel: access.ReadWriteAccess}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ResolvedCollaborators() = %+v, want %+v", got, want)
	}
}
