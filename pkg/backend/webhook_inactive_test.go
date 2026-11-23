package backend

import (
	"errors"
	"reflect"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/google/uuid"
)

// setHookActive switches a webhook on or off in the store, keeping its
// loopback URL, which the backend's validating update would refuse.
func (e whEnv) setHookActive(t *testing.T, id int64, active bool) {
	t.Helper()
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		h, err := e.be.store.GetWebhookByID(e.ctx, tx, e.repo.ID(), id)
		if err != nil {
			return err
		}
		return e.be.store.UpdateWebhookByID(e.ctx, tx, models.Webhook{ID: id, RepoID: e.repo.ID(), URL: h.URL, Secret: h.Secret, ContentType: h.ContentType, Active: active})
	}); err != nil {
		t.Fatal(err)
	}
}

// seedInactiveHook stores a second, inactive webhook on another loopback
// address, since a repository's webhook URLs are unique.
func (e whEnv) seedInactiveHook(t *testing.T, events ...webhook.Event) int64 {
	t.Helper()
	var id int64
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		var err error
		id, err = e.be.store.CreateWebhook(e.ctx, tx, models.Webhook{RepoID: e.repo.ID(), URL: "http://127.0.0.1:2/", ContentType: int(webhook.ContentTypeJSON)})
		if err != nil {
			return err
		}
		evs := make([]int, len(events))
		for i, ev := range events {
			evs[i] = int(ev)
		}
		return e.be.store.CreateWebhookEvents(e.ctx, tx, id, evs)
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// An inactive webhook is sent nothing, on the push path that sends in the
// caller and on the background path, while an active one next to it is.
func TestInactiveWebhookGetsNoEvents(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	active := e.seedHook(t, webhook.EventBranchTagCreate, webhook.EventRepository)
	inactive := e.seedInactiveHook(t, webhook.EventBranchTagCreate, webhook.EventRepository)

	e.afterPush(t, e.admin, []hooks.HookArg{{
		OldSha: zeroSha, NewSha: someSha, RefName: "refs/heads/feature",
	}})
	if err := e.be.RenameRepository(e.ctx, "repo", "renamed"); err != nil {
		t.Fatal(err)
	}
	e.be.WaitForDeliveries(e.ctx)

	if got, want := sortedEvents(e.deliveredEvents(t, active)), sortedEvents([]webhook.Event{webhook.EventBranchTagCreate, webhook.EventRepository}); !reflect.DeepEqual(got, want) {
		t.Fatalf("sortedEvents(e.deliveredEvents(t, active)) = %v, want %v", got, want)
	}
	if got := len(e.deliveredEvents(t, inactive)); got != 0 {
		t.Fatalf("len(e.deliveredEvents(t, inactive)) = %v, want %v", got, 0)
	}

	// Inactive webhooks are still listed, so they can be managed.
	list, err := e.be.ListWebhooks(e.ctx, e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(list); got != 2 {
		t.Fatalf("len(list) = %v, want %v", got, 2)
	}
}

// Reactivating a webhook resumes its deliveries.
func TestReactivatedWebhookResumesDeliveries(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventRepositoryVisibilityChange)

	e.setHookActive(t, id, false)
	if err := e.be.SetPrivate(e.ctx, "repo", true); err != nil {
		t.Fatal(err)
	}
	// Wait for the queue to drain without closing it, so the next change is
	// still delivered.
	_, idle := e.be.deliveries.pending()
	<-idle
	if got := len(e.deliveredEvents(t, id)); got != 0 {
		t.Fatalf("len(e.deliveredEvents(t, id)) = %v, want %v", got, 0)
	}

	e.setHookActive(t, id, true)
	if err := e.be.SetPrivate(e.ctx, "repo", false); err != nil {
		t.Fatal(err)
	}
	e.be.WaitForDeliveries(e.ctx)
	if got, want := e.deliveredEvents(t, id), ([]webhook.Event{webhook.EventRepositoryVisibilityChange}); !reflect.DeepEqual(got, want) {
		t.Fatalf("e.deliveredEvents(t, id) = %v, want %v", got, want)
	}
}

// A webhook deactivated while its event waits in the queue is skipped when
// the event is sent.
func TestWebhookDeactivatedWhileQueuedIsSkipped(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventRepository)
	unblock := blockDeliveries(t, e, 10)

	ev, err := e.be.webhooks.NewRepositoryEvent(e.ctx, e.admin, e.repo, webhook.RepositoryEventActionRename)
	if err != nil {
		t.Fatal(err)
	}
	subs, err := e.be.webhooks.Subscribers(e.ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(subs); got != 1 {
		t.Fatalf("len(subs) = %v, want %v", got, 1)
	}
	e.be.deliverAfterChange(e.ctx, subs, ev, true)

	e.setHookActive(t, id, false)
	unblock()
	e.be.WaitForDeliveries(e.ctx)

	if got := len(e.deliveredEvents(t, id)); got != 0 {
		t.Fatalf("len(e.deliveredEvents(t, id)) = %v, want %v", got, 0)
	}
}

// Redelivering to an inactive webhook is refused and sends nothing. Once the
// webhook is active again it can be redelivered.
func TestRedeliverInactiveWebhookRefused(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventPush)
	delID := e.seedDelivery(t, id, "http://127.0.0.1:1/", `{"ref":"main"}`)
	e.setHookActive(t, id, false)

	err := e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID)
	if !errors.Is(err, webhook.ErrWebhookInactive) {
		t.Fatal("not true: errors.Is(err, webhook.ErrWebhookInactive)")
	}
	if got := len(e.deliveredEvents(t, id)); got != 1 {
		t.Fatalf("len(e.deliveredEvents(t, id)) = %v, want %v", got, 1)
	}

	// An unknown delivery is reported as not found, not as inactive.
	err = e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, uuid.New())
	if !errors.Is(err, proto.ErrDeliveryNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrDeliveryNotFound)")
	}

	e.setHookActive(t, id, true)
	if err := e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID); err != nil {
		t.Fatal(err)
	}
	if got := len(e.deliveredEvents(t, id)); got != 2 {
		t.Fatalf("len(e.deliveredEvents(t, id)) = %v, want %v", got, 2)
	}
}
