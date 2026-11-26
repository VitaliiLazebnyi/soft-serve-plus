package webhook

import (
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
)

func TestNewCollaboratorEvent(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	if err := e.ds.CreateUser(e.ctx, e.dbx, "bob", false, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.ds.AddCollabByUsernameAndRepo(e.ctx, e.dbx, "bob", "repo", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}

	ev, err := e.s.NewCollaboratorEvent(e.ctx, e.sender(), e.senderRepo(""), "bob", CollaboratorEventAdded)
	if err != nil {
		t.Fatal(err)
	}
	if got := ev.EventType; got != EventCollaborator {
		t.Fatalf("ev.EventType = %v, want %v", got, EventCollaborator)
	}
	if got := ev.Action; got != CollaboratorEventAdded {
		t.Fatalf("ev.Action = %v, want %v", got, CollaboratorEventAdded)
	}
	if got := ev.Collaborator.Username; got != "bob" {
		t.Fatalf("ev.Collaborator.Username = %v, want %v", got, "bob")
	}
	if ev.Collaborator.ID == 0 {
		t.Fatal("not true: ev.Collaborator.ID != 0")
	}
	if got := ev.AccessLevel; got != access.ReadWriteAccess {
		t.Fatalf("ev.AccessLevel = %v, want %v", got, access.ReadWriteAccess)
	}
	if got := ev.Repository.Owner.Username; got != "alice" {
		t.Fatalf("ev.Repository.Owner.Username = %v, want %v", got, "alice")
	}
	// Collaborator events carry the same clone URLs as every other event.
	if got := ev.Repository.HTTPURL; got != "http://127.0.0.1:23232/repo.git" {
		t.Fatalf("ev.Repository.HTTPURL = %v, want %v", got, "http://127.0.0.1:23232/repo.git")
	}
	if got := ev.Repository.SSHURL; got != "ssh://127.0.0.1:23231/repo.git" {
		t.Fatalf("ev.Repository.SSHURL = %v, want %v", got, "ssh://127.0.0.1:23231/repo.git")
	}
	if got := ev.Repository.GitURL; got != "git://127.0.0.1:9418/repo.git" {
		t.Fatalf("ev.Repository.GitURL = %v, want %v", got, "git://127.0.0.1:9418/repo.git")
	}
}

func TestNewCollaboratorEventUnknownCollaborator(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)

	_, err := e.s.NewCollaboratorEvent(e.ctx, e.sender(), e.senderRepo(""), "nobody", CollaboratorEventRemoved)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

func TestNewCollaboratorEventOwnerLookupFails(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)

	_, err := e.s.NewCollaboratorEvent(e.ctx, e.sender(), e.ownerLess(), "alice", CollaboratorEventAdded)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}
