package backend

import (
	"os"
	"path/filepath"
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

// A description the database refuses is not written to the repository's
// description file either.
func TestSetDescriptionDBFailureKeepsFile(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	is.NoErr(e.be.SetDescription(e.ctx, "repo", "before"))
	refuseRepoUpdate(t, e, "description")

	is.True(e.be.SetDescription(e.ctx, "repo", "after") != nil)
	got, err := os.ReadFile(filepath.Join(e.be.repoPath("repo"), "description"))
	is.NoErr(err)
	is.Equal(string(got), "before")
}

// A visibility change the database refuses leaves git-daemon-export-ok as it
// was.
func TestSetPrivateDBFailureKeepsExportFile(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	fp := filepath.Join(e.be.repoPath("repo"), "git-daemon-export-ok")
	_, err := os.Stat(fp)
	is.NoErr(err)
	refuseRepoUpdate(t, e, "private")

	is.True(e.be.SetPrivate(e.ctx, "repo", true) != nil)
	_, err = os.Stat(fp)
	is.NoErr(err)
}
