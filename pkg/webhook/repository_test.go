package webhook

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-querystring/query"
)

func TestNewRepositoryEventVisibilityChange(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)

	ev, err := e.s.NewRepositoryEvent(e.ctx, e.sender(), e.senderRepo(""), RepositoryEventActionVisibilityChange)
	if err != nil {
		t.Fatal(err)
	}
	if got := ev.EventType; got != EventRepositoryVisibilityChange {
		t.Fatalf("ev.EventType = %v, want %v", got, EventRepositoryVisibilityChange)
	}
	if got := ev.Action; got != RepositoryEventActionVisibilityChange {
		t.Fatalf("ev.Action = %v, want %v", got, RepositoryEventActionVisibilityChange)
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
}

func TestNewRepositoryEventOtherActions(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)

	for _, action := range []RepositoryEventAction{
		RepositoryEventActionDelete,
		RepositoryEventActionRename,
		RepositoryEventActionDefaultBranchChange,
	} {
		ev, err := e.s.NewRepositoryEvent(e.ctx, e.sender(), e.senderRepo(""), action)
		if err != nil {
			t.Fatal(err)
		}
		if got := ev.EventType; got != EventRepository {
			t.Fatalf("ev.EventType = %v, want %v", got, EventRepository)
		}
		if got := ev.Action; got != action {
			t.Fatalf("ev.Action = %v, want %v", got, action)
		}
	}
}

// Events other than renames and default branch changes leave changes out.
func TestNewRepositoryEventNoChanges(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)

	ev, err := e.s.NewRepositoryEvent(e.ctx, e.sender(), e.senderRepo(""), RepositoryEventActionDelete)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Changes != nil {
		t.Fatal("not true: ev.Changes == nil")
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"changes"`) {
		t.Fatal("not true: !strings.Contains(string(b), `\"changes\"`)")
	}
	v, err := query.Values(ev)
	if err != nil {
		t.Fatal(err)
	}
	for k := range v {
		if strings.HasPrefix(k, "changes") {
			t.Fatal("not true: !strings.HasPrefix(k, \"changes\")")
		}
	}
}

func TestNewRepositoryRenameEvent(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)

	ev, err := e.s.NewRepositoryRenameEvent(e.ctx, e.sender(), e.senderRepo(""), "old-name")
	if err != nil {
		t.Fatal(err)
	}
	if got := ev.EventType; got != EventRepository {
		t.Fatalf("ev.EventType = %v, want %v", got, EventRepository)
	}
	if got := ev.Action; got != RepositoryEventActionRename {
		t.Fatalf("ev.Action = %v, want %v", got, RepositoryEventActionRename)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"changes":{"name":{"from":"old-name"}}`) {
		t.Fatal("not true: strings.Contains(string(b), `\"changes\":{\"name\":{\"from\":\"old-name\"}}`)")
	}
	v, err := query.Values(ev)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Get("changes[name][from]"); got != "old-name" {
		t.Fatalf("v.Get(\"changes[name][from]\") = %v, want %v", got, "old-name")
	}
	if got := v.Get("changes[default_branch][from]"); got != "" {
		t.Fatalf("v.Get(\"changes[default_branch][from]\") = %v, want %v", got, "")
	}
}

func TestNewDefaultBranchChangeEvent(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)

	ev, err := e.s.NewDefaultBranchChangeEvent(e.ctx, e.sender(), e.senderRepo(""), "master")
	if err != nil {
		t.Fatal(err)
	}
	if got := ev.EventType; got != EventRepository {
		t.Fatalf("ev.EventType = %v, want %v", got, EventRepository)
	}
	if got := ev.Action; got != RepositoryEventActionDefaultBranchChange {
		t.Fatalf("ev.Action = %v, want %v", got, RepositoryEventActionDefaultBranchChange)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"changes":{"default_branch":{"from":"master"}}`) {
		t.Fatal("not true: strings.Contains(string(b), `\"changes\":{\"default_branch\":{\"from\":\"master\"}}`)")
	}
	v, err := query.Values(ev)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Get("changes[default_branch][from]"); got != "master" {
		t.Fatalf("v.Get(\"changes[default_branch][from]\") = %v, want %v", got, "master")
	}
}

func TestRepositoryChangeEventsOwnerLookupFails(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)

	_, err := e.s.NewRepositoryRenameEvent(e.ctx, e.sender(), e.ownerLess(), "old")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	_, err = e.s.NewDefaultBranchChangeEvent(e.ctx, e.sender(), e.ownerLess(), "old")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

func TestNewRepositoryEventOwnerLookupFails(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)

	_, err := e.s.NewRepositoryEvent(e.ctx, e.sender(), e.ownerLess(), RepositoryEventActionDelete)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}
