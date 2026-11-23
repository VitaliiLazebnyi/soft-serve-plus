package backend

import (
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
)

// RemoveCollaborator takes the name in any case, like AddCollaborator, and
// its event carries the stored, lowercase name.
func TestRemoveCollaboratorEventNamesStoredUsername(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	id := e.seedHook(t, webhook.EventCollaborator)

	if err := e.be.RemoveCollaborator(e.ctx, "repo", "BOB"); err != nil {
		t.Fatal(err)
	}
	e.awaitDeliveries(t, id, webhook.EventCollaborator)

	var body string
	if err := e.be.db.GetContext(e.ctx, &body, e.be.db.Rebind("SELECT request_body FROM webhook_deliveries WHERE webhook_id = ?"), id); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"username":"bob"`) {
		t.Fatal("not true: strings.Contains(body, `\"username\":\"bob\"`)")
	}
	if strings.Contains(body, "BOB") {
		t.Fatal("not true: !strings.Contains(body, \"BOB\")")
	}
}

// refuseRepoUpdate makes every update of column on repos fail.
func refuseRepoUpdate(t *testing.T, e whEnv, column string) {
	t.Helper()
	refuseTrigger(t, e.ctx, e.be.db, "refuse_update", "BEFORE", "UPDATE OF "+column, "repos")
}

// A description the database refuses leaves the stored one as it was.
func TestSetDescriptionDBFailureKeepsDescription(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	if err := e.be.SetDescription(e.ctx, "repo", "before"); err != nil {
		t.Fatal(err)
	}
	refuseRepoUpdate(t, e, "description")

	if !(e.be.SetDescription(e.ctx, "repo", "after") != nil) {
		t.Fatal("not true: e.be.SetDescription(e.ctx, \"repo\", \"after\") != nil")
	}
	r, err := e.be.Repository(e.ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Description(); got != "before" {
		t.Fatalf("r.Description() = %v, want %v", got, "before")
	}
}

// A visibility change the database refuses leaves the repository as it was.
func TestSetPrivateDBFailureKeepsVisibility(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	refuseRepoUpdate(t, e, "private")

	if !(e.be.SetPrivate(e.ctx, "repo", true) != nil) {
		t.Fatal("not true: e.be.SetPrivate(e.ctx, \"repo\", true) != nil")
	}
	r, err := e.be.Repository(e.ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if r.IsPrivate() {
		t.Fatal("not true: !r.IsPrivate()")
	}
}
