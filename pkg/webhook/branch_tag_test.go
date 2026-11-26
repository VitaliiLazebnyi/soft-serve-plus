package webhook

import (
	"strings"
	"testing"
)

func TestNewBranchTagEventCreate(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)

	ev, err := e.s.NewBranchTagEvent(e.ctx, e.sender(), e.senderRepo(""), "refs/heads/dev", zeroHash, "1111111111111111111111111111111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if got := ev.EventType; got != EventBranchTagCreate {
		t.Fatalf("ev.EventType = %v, want %v", got, EventBranchTagCreate)
	}
	if !ev.Created {
		t.Fatal("not true: ev.Created")
	}
	if ev.Deleted {
		t.Fatal("not true: !ev.Deleted")
	}
	if got := ev.Ref; got != "refs/heads/dev" {
		t.Fatalf("ev.Ref = %v, want %v", got, "refs/heads/dev")
	}
	if got := ev.Repository.Owner.Username; got != "alice" {
		t.Fatalf("ev.Repository.Owner.Username = %v, want %v", got, "alice")
	}
	if got := ev.Repository.HTTPURL; got != "http://127.0.0.1:23232/repo.git" {
		t.Fatalf("ev.Repository.HTTPURL = %v, want %v", got, "http://127.0.0.1:23232/repo.git")
	}
	if got := ev.Repository.SSHURL; got != "ssh://127.0.0.1:23231/repo.git" {
		t.Fatalf("ev.Repository.SSHURL = %v, want %v", got, "ssh://127.0.0.1:23231/repo.git")
	}
	if got := ev.Repository.GitURL; got != "git://127.0.0.1:9418/repo.git" {
		t.Fatalf("ev.Repository.GitURL = %v, want %v", got, "git://127.0.0.1:9418/repo.git")
	}
	if got := ev.Sender.Username; got != "alice" {
		t.Fatalf("ev.Sender.Username = %v, want %v", got, "alice")
	}
}

func TestNewBranchTagEventDelete(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)

	ev, err := e.s.NewBranchTagEvent(e.ctx, e.sender(), e.senderRepo(""), "refs/tags/v1", "1111111111111111111111111111111111111111", zeroHash)
	if err != nil {
		t.Fatal(err)
	}
	if got := ev.EventType; got != EventBranchTagDelete {
		t.Fatalf("ev.EventType = %v, want %v", got, EventBranchTagDelete)
	}
	if !ev.Deleted {
		t.Fatal("not true: ev.Deleted")
	}
	if ev.Created {
		t.Fatal("not true: !ev.Created")
	}
}

func TestNewBranchTagEventRejectsUpdate(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)

	// Neither side is zero: that is a plain update, not a branch or tag event.
	_, err := e.s.NewBranchTagEvent(e.ctx, e.sender(), e.senderRepo(""), "refs/heads/main",
		"1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if !strings.Contains(err.Error(), "invalid branch or tag event") {
		t.Fatal("not true: strings.Contains(err.Error(), \"invalid branch or tag event\")")
	}
}

func TestNewBranchTagEventOwnerLookupFails(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)

	_, err := e.s.NewBranchTagEvent(e.ctx, e.sender(), e.ownerLess(), "refs/heads/dev", zeroHash, "1111111111111111111111111111111111111111")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}
