package backend

import (
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/matryer/is"
)

// RemoveCollaborator takes the name in any case, like AddCollaborator, and
// its event carries the stored, lowercase name.
func TestRemoveCollaboratorEventNamesStoredUsername(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	is.NoErr(err)
	is.NoErr(e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess))
	id := e.seedHook(t, webhook.EventCollaborator)

	is.NoErr(e.be.RemoveCollaborator(e.ctx, "repo", "BOB"))
	e.awaitDeliveries(t, id, webhook.EventCollaborator)

	var body string
	is.NoErr(e.be.db.GetContext(e.ctx, &body, "SELECT request_body FROM webhook_deliveries WHERE webhook_id = ?", id))
	is.True(strings.Contains(body, `"username":"bob"`))
	is.True(!strings.Contains(body, "BOB"))
}

// refuseRepoUpdate makes every update of column on repos fail.
func refuseRepoUpdate(t *testing.T, e whEnv, column string) {
	t.Helper()
	is := is.New(t)
	_, err := e.be.db.ExecContext(e.ctx, "CREATE TRIGGER refuse_update BEFORE UPDATE OF "+column+
		" ON repos BEGIN SELECT RAISE(ABORT, 'refused'); END")
	is.NoErr(err)
}

// A description the database refuses leaves the stored one as it was.
func TestSetDescriptionDBFailureKeepsDescription(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newWebhookEnv(t)
	is.NoErr(e.be.SetDescription(e.ctx, "repo", "before"))
	refuseRepoUpdate(t, e, "description")

	is.True(e.be.SetDescription(e.ctx, "repo", "after") != nil)
	r, err := e.be.Repository(e.ctx, "repo")
	is.NoErr(err)
	is.Equal(r.Description(), "before")
}

// A visibility change the database refuses leaves the repository as it was.
func TestSetPrivateDBFailureKeepsVisibility(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newWebhookEnv(t)
	refuseRepoUpdate(t, e, "private")

	is.True(e.be.SetPrivate(e.ctx, "repo", true) != nil)
	r, err := e.be.Repository(e.ctx, "repo")
	is.NoErr(err)
	is.True(!r.IsPrivate())
}
