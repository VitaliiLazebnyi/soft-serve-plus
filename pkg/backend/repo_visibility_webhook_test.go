package backend

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
)

// TestSetPrivateWebhookOnlyOnChange verifies that the visibility change
// webhook fires when visibility changes and not when it is set to the
// value it already has.
func TestSetPrivateWebhookOnlyOnChange(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)

	ctx := context.Background()
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	ctx = proto.WithUserContext(ctx, admin)

	repo, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Deliveries to this address fail fast, but are still recorded.
	var hookID int64
	if err := be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		hookID, err = be.store.CreateWebhook(ctx, tx, models.Webhook{RepoID: repo.ID(), URL: "http://127.0.0.1:1/", ContentType: int(webhook.ContentTypeJSON), Active: true})
		if err != nil {
			return err
		}
		return be.store.CreateWebhookEvents(ctx, tx, hookID, []int{int(webhook.EventRepositoryVisibilityChange)})
	}); err != nil {
		t.Fatal(err)
	}

	deliveries := func() int {
		ds, err := be.store.ListWebhookDeliveriesByWebhookID(ctx, be.db, hookID)
		if err != nil {
			t.Fatal(err)
		}
		return len(ds)
	}

	// Deliveries run in the background. The negative checks first wait until
	// the queue is idle, so a delivery still on its way cannot be missed.
	idle := func() {
		_, finished := be.deliveries.pending()
		<-finished
	}

	if err := be.SetPrivate(ctx, "repo", false); err != nil {
		t.Fatal(err)
	}
	idle()
	if got := deliveries(); got != 0 {
		t.Fatalf("deliveries() = %v, want %v", got, 0)
	}

	if err := be.SetPrivate(ctx, "repo", true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first visibility delivery", func() bool { return deliveries() == 1 })

	if err := be.SetPrivate(ctx, "repo", true); err != nil {
		t.Fatal(err)
	}
	idle()
	if got := deliveries(); got != 1 {
		t.Fatalf("deliveries() = %v, want %v", got, 1)
	}

	if err := be.SetPrivate(ctx, "repo", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second visibility delivery", func() bool { return deliveries() == 2 })
}
