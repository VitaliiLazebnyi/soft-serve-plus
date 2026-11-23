package backend

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/google/uuid"
)

// TestWebhooksBareContext verifies that the webhook methods use the
// backend's own database and store, so a caller whose context carries
// neither still works. Reading the database from such a context, as they
// once did, gave nil and panicked.
func TestWebhooksBareContext(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	ctx := context.Background()

	if err := e.be.CreateWebhook(ctx, e.repo, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true}); err != nil {
		t.Fatal(err)
	}
	hooks, err := e.be.ListWebhooks(ctx, e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(hooks); got != 1 {
		t.Fatalf("len(hooks) = %v, want %v", got, 1)
	}
	id := hooks[0].ID

	h, err := e.be.Webhook(ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.URL; got != whURL {
		t.Fatalf("h.URL = %v, want %v", got, whURL)
	}

	if err := e.be.UpdateWebhook(ctx, e.repo, id, WebhookInput{URL: whURLOther, ContentType: webhook.ContentTypeForm, Events: []webhook.Event{webhook.EventPush}, Active: true}); err != nil {
		t.Fatal(err)
	}
	h, err = e.be.Webhook(ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.URL; got != whURLOther {
		t.Fatalf("h.URL = %v, want %v", got, whURLOther)
	}

	dels, err := e.be.ListWebhookDeliveries(ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(dels); got != 0 {
		t.Fatalf("len(dels) = %v, want %v", got, 0)
	}
	_, err = e.be.WebhookDelivery(ctx, e.repo, id, uuid.New())
	if !(err != nil) {
		t.Fatal("not true: err != nil")
	} // no such delivery, but no panic either
	if !(e.be.RedeliverWebhookDelivery(ctx, e.repo, id, uuid.New()) != nil) {
		t.Fatal("not true: e.be.RedeliverWebhookDelivery(ctx, e.repo, id, uuid.New()) != nil")
	}

	if err := e.be.DeleteWebhook(ctx, e.repo, id); err != nil {
		t.Fatal(err)
	}
	hooks, err = e.be.ListWebhooks(ctx, e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(hooks); got != 0 {
		t.Fatalf("len(hooks) = %v, want %v", got, 0)
	}
}

// TestRedeliverBareContext redelivers a stored delivery under a context that
// carries no config, database or store. The send must still reach a receiver
// that only webhook.allowed_hosts permits, and the new attempt must be
// recorded; recording from the bare context used to panic.
func TestRedeliverBareContext(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)

	var hits atomic.Int32
	srv := newMemServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	url := srv.URL + "/hook"

	e.setWebhookAllowedHosts(t, "127.0.0.0/8")
	id := e.createHook(t, url, webhook.EventPush)
	delID := e.seedDelivery(t, id, url, `{"ref":"main"}`)

	if err := e.be.RedeliverWebhookDelivery(context.Background(), e.repo, id, delID); err != nil {
		t.Fatal(err)
	}
	if got, want := hits.Load(), int32(1); got != want {
		t.Fatalf("hits.Load() = %v, want %v", got, want)
	}

	list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(list); got != 2 {
		t.Fatalf("len(list) = %v, want %v", got, 2)
	}
	for _, d := range list {
		if d.ID == delID {
			continue
		}
		if got := d.ResponseStatus; got != http.StatusOK {
			t.Fatalf("d.ResponseStatus = %v, want %v", got, http.StatusOK)
		}
		if d.RequestError.Valid {
			t.Fatal("not true: !d.RequestError.Valid")
		}
	}
}

// TestStoreRepoMissingLFSObjectsBackendConfig verifies that LFS objects are
// stored under the backend's data path, whatever config the context carries.
func TestStoreRepoMissingLFSObjectsBackendConfig(t *testing.T) {
	t.Parallel()
	lt := newLFSTest(t, 1)

	lt.ctx = context.Background()
	if err := lt.store(); err != nil {
		t.Fatal(err)
	}
	p := lt.pointers[0]
	got, err := os.ReadFile(filepath.Join(lt.be.cfg.DataPath, "lfs", strconv.FormatInt(lt.repo.ID(), 10), "objects", p.RelativePath()))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := got, lt.client.objects[p.Oid]; !reflect.DeepEqual(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}

	lt.client.requested = nil
	if err := lt.be.SyncRepoLFSObjects(lt.ctx, lt.repo, lt.client); err != nil {
		t.Fatal(err)
	}
	if got := len(lt.client.requested); got != 0 {
		t.Fatalf("len(lt.client.requested) = %v, want %v", got, 0)
	} // already stored
}
