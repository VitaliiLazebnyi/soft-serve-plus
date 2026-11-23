package backend

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/matryer/is"
)

// TestSetPrivateWebhookOnlyOnChange verifies that the visibility change
// webhook fires when visibility changes and not when it is set to the
// value it already has.
func TestSetPrivateWebhookOnlyOnChange(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	be, _ := newTestBackend(t)

	ctx := context.Background()
	admin, err := be.User(ctx, "admin")
	is.NoErr(err)
	ctx = proto.WithUserContext(ctx, admin)

	repo, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	is.NoErr(err)

	// Deliveries to this address fail fast, but are still recorded.
	var hookID int64
	is.NoErr(be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		hookID, err = be.store.CreateWebhook(ctx, tx, models.Webhook{RepoID: repo.ID(), URL: "http://127.0.0.1:1/", ContentType: int(webhook.ContentTypeJSON), Active: true})
		if err != nil {
			return err
		}
		return be.store.CreateWebhookEvents(ctx, tx, hookID, []int{int(webhook.EventRepositoryVisibilityChange)})
	}))

	deliveries := func() int {
		ds, err := be.store.ListWebhookDeliveriesByWebhookID(ctx, be.db, hookID)
		is.NoErr(err)
		return len(ds)
	}

	// Deliveries run in the background. The negative checks first wait until
	// the queue is idle, so a delivery still on its way cannot be missed.
	idle := func() {
		_, finished := be.deliveries.pending()
		<-finished
	}

	is.NoErr(be.SetPrivate(ctx, "repo", false))
	idle()
	is.Equal(deliveries(), 0)

	is.NoErr(be.SetPrivate(ctx, "repo", true))
	waitFor(t, "first visibility delivery", func() bool { return deliveries() == 1 })

	is.NoErr(be.SetPrivate(ctx, "repo", true))
	idle()
	is.Equal(deliveries(), 1)

	is.NoErr(be.SetPrivate(ctx, "repo", false))
	waitFor(t, "second visibility delivery", func() bool { return deliveries() == 2 })
}
