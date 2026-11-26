package webhook

import (
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/matryer/is"
)

func TestNewCollaboratorEvent(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)
	is.NoErr(e.ds.CreateUser(e.ctx, e.dbx, "bob", false, nil))
	is.NoErr(e.ds.AddCollabByUsernameAndRepo(e.ctx, e.dbx, "bob", "repo", access.ReadWriteAccess))

	ev, err := e.s.NewCollaboratorEvent(e.ctx, e.sender(), e.senderRepo(""), "bob", CollaboratorEventAdded)
	is.NoErr(err)
	is.Equal(ev.EventType, EventCollaborator)
	is.Equal(ev.Action, CollaboratorEventAdded)
	is.Equal(ev.Collaborator.Username, "bob")
	is.True(ev.Collaborator.ID != 0)
	is.Equal(ev.AccessLevel, access.ReadWriteAccess)
	is.Equal(ev.Repository.Owner.Username, "alice")
	// Collaborator events carry the same clone URLs as every other event.
	is.Equal(ev.Repository.HTTPURL, "http://127.0.0.1:23232/repo.git")
	is.Equal(ev.Repository.SSHURL, "ssh://127.0.0.1:23231/repo.git")
	is.Equal(ev.Repository.GitURL, "git://127.0.0.1:9418/repo.git")
}

func TestNewCollaboratorEventUnknownCollaborator(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)

	_, err := e.s.NewCollaboratorEvent(e.ctx, e.sender(), e.senderRepo(""), "nobody", CollaboratorEventRemoved)
	is.True(err != nil)
}

func TestNewCollaboratorEventOwnerLookupFails(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newTestEnv(t)

	_, err := e.s.NewCollaboratorEvent(e.ctx, e.sender(), e.ownerLess(), "alice", CollaboratorEventAdded)
	is.True(err != nil)
}
