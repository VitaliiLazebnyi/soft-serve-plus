package backend

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
)

// setWebhookAllowedHosts sets webhook.allowed_hosts and parses it the way the
// server does at startup, and rebuilds the backend's webhook sender with it.
func (e whEnv) setWebhookAllowedHosts(t *testing.T, entries ...string) {
	t.Helper()
	e.cfg.Webhook.AllowedHosts = entries
	if err := e.cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// The server reads the list once, when the backend builds its sender.
	e.be.webhooks = webhook.NewSender(e.cfg, e.be.db, e.be.store, e.be.logger)
}

// With webhook.allowed_hosts covering a loopback receiver, a webhook to it can
// be created and is delivered to through the SSRF-protected client.
func TestWebhookAllowedHostsCreateAndDeliver(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)

	var hits atomic.Int32
	srv := newMemServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-SoftServe-Event") == webhook.EventRepositoryVisibilityChange.String() {
			hits.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	url := srv.URL + "/hook"

	// Default: refused at create.
	err := e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: url, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventRepositoryVisibilityChange}, Active: true})
	if !errors.Is(err, ssrf.ErrPrivateIP) {
		t.Fatal("not true: errors.Is(err, ssrf.ErrPrivateIP)")
	}

	e.setWebhookAllowedHosts(t, "127.0.0.0/8")
	id := e.createHook(t, url, webhook.EventRepositoryVisibilityChange)

	if err := e.be.SetPrivate(e.ctx, "repo", true); err != nil {
		t.Fatal(err)
	}
	e.be.WaitForDeliveries(e.ctx)
	if got, want := hits.Load(), int32(1); got != want {
		t.Fatalf("hits.Load() = %v, want %v", got, want)
	}

	ds, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ds); got != 1 {
		t.Fatalf("len(ds) = %v, want %v", got, 1)
	}
	if got := ds[0].ResponseStatus; got != http.StatusOK {
		t.Fatalf("ds[0].ResponseStatus = %v, want %v", got, http.StatusOK)
	}
	if ds[0].RequestError.Valid {
		t.Fatal("not true: !ds[0].RequestError.Valid")
	}
}

// Update validates the new URL against the list too, and the list does not
// open metadata addresses.
func TestWebhookAllowedHostsUpdate(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	events := []webhook.Event{webhook.EventPush}

	err := e.be.UpdateWebhook(e.ctx, e.repo, id, WebhookInput{URL: "http://10.1.2.3/hook", ContentType: webhook.ContentTypeJSON, Events: events, Active: true})
	if !errors.Is(err, ssrf.ErrPrivateIP) {
		t.Fatal("not true: errors.Is(err, ssrf.ErrPrivateIP)")
	}

	e.setWebhookAllowedHosts(t, "10.0.0.0/8", "0.0.0.0/0:8080")
	if err := e.be.UpdateWebhook(e.ctx, e.repo, id, WebhookInput{URL: "http://10.1.2.3/hook", ContentType: webhook.ContentTypeJSON, Events: events, Active: true}); err != nil {
		t.Fatal(err)
	}

	err = e.be.UpdateWebhook(e.ctx, e.repo, id, WebhookInput{URL: "http://169.254.169.254:8080/latest/meta-data", ContentType: webhook.ContentTypeJSON, Events: events, Active: true})
	if !errors.Is(err, ssrf.ErrPrivateIP) {
		t.Fatal("not true: errors.Is(err, ssrf.ErrPrivateIP)")
	}
	err = e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: "http://169.254.169.254:8080/", ContentType: webhook.ContentTypeJSON, Events: events, Active: true})
	if !errors.Is(err, ssrf.ErrPrivateIP) {
		t.Fatal("not true: errors.Is(err, ssrf.ErrPrivateIP)")
	}

	h, err := e.be.Webhook(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.URL; got != "http://10.1.2.3/hook" {
		t.Fatalf("h.URL = %v, want %v", got, "http://10.1.2.3/hook")
	}
}

// Create and update resolve the webhook's hostname with the caller's context,
// so a caller that has gone away is not held by the resolver, and nothing is
// stored or changed.
func TestWebhookCreateAndUpdateStopWhenCancelled(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	events := []webhook.Event{webhook.EventPush}

	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	err := e.be.CreateWebhook(ctx, e.repo, WebhookInput{URL: "https://hooks.example/new", ContentType: webhook.ContentTypeJSON, Events: events, Active: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("not true: errors.Is(err, context.Canceled)")
	}
	err = e.be.UpdateWebhook(ctx, e.repo, id, WebhookInput{URL: "https://hooks.example/changed", ContentType: webhook.ContentTypeJSON, Events: events, Active: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("not true: errors.Is(err, context.Canceled)")
	}

	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(hooks); got != 1 {
		t.Fatalf("len(hooks) = %v, want %v", got, 1)
	}
	if got := hooks[0].URL; got != whURL {
		t.Fatalf("hooks[0].URL = %v, want %v", got, whURL)
	}
}
