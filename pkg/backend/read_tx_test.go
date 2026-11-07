package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/google/uuid"
	"github.com/matryer/is"
	"golang.org/x/crypto/ssh"
)

// TestLookupsDoNotWaitForWriter checks that the lookups behind every access
// check read without taking the SQLite write lock: they must succeed while
// another connection holds a write transaction open, instead of waiting for
// it (busy_timeout) and then failing with "database is locked".
func TestLookupsDoNotWaitForWriter(t *testing.T) {
	is := is.New(t)
	be, _ := newTestBackend(t)
	// The webhook methods take the database and store from the context.
	ctx := db.WithContext(context.Background(), be.db)
	ctx = store.WithContext(ctx, be.store)

	// Both differ from what the lookups fall back to on an error.
	is.NoErr(be.SetAnonAccess(ctx, access.ReadWriteAccess))
	is.NoErr(be.SetAllowKeyless(ctx, true))
	admin, err := be.User(ctx, "admin")
	is.NoErr(err)
	repo, err := be.CreateRepository(ctx, "repo", admin, proto.RepositoryOptions{Description: "desc"})
	is.NoErr(err)
	pk := newTestPublicKey(t)
	collab, err := be.CreateUser(ctx, "collab", proto.UserOptions{PublicKeys: []ssh.PublicKey{pk}})
	is.NoErr(err)
	token, err := be.CreateAccessToken(ctx, collab, "t", time.Time{})
	is.NoErr(err)
	// Inactive, so RedeliverWebhookDelivery stops with ErrWebhookInactive
	// after its lookups instead of sending anything.
	var hookID int64
	delID := uuid.New()
	is.NoErr(be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		if err := be.store.AddCollabByUsernameAndRepo(ctx, tx, "collab", "repo", access.ReadOnlyAccess); err != nil {
			return err
		}
		var err error
		hookID, err = be.store.CreateWebhook(ctx, tx, repo.ID(), "http://127.0.0.1:1/", "", int(webhook.ContentTypeJSON), false)
		if err != nil {
			return err
		}
		return be.store.CreateWebhookDelivery(ctx, tx, delID, hookID, int(webhook.EventPush), "http://127.0.0.1:1/", "POST", nil, "", "{}", 200, "", "")
	}))

	written := make(chan struct{})
	release := make(chan struct{})
	writerErr := make(chan error, 1)
	go func() {
		writerErr <- be.db.TransactionContext(ctx, func(tx *db.Tx) error {
			if err := be.store.CreateUser(ctx, tx, "writer", false, nil); err != nil {
				return err
			}
			close(written)
			<-release
			return nil
		})
	}()
	select {
	case <-written:
	case err := <-writerErr:
		t.Fatalf("writer: %v", err)
	}

	func() {
		defer close(release)
		// Each lookup gets its own deadline, well under busy_timeout, so a
		// lookup that waits for the writer fails instead of slowing the test
		// down, and does not use up the time of the lookups after it.
		var cancels []context.CancelFunc
		defer func() {
			for _, cancel := range cancels {
				cancel()
			}
		}()
		rctx := func() context.Context {
			c, cancel := context.WithTimeout(ctx, 2*time.Second)
			cancels = append(cancels, cancel)
			return c
		}

		u, err := be.User(rctx(), "admin")
		if err != nil {
			t.Errorf("User: %v", err)
		} else if _, err := be.UserByID(rctx(), u.ID()); err != nil {
			t.Errorf("UserByID: %v", err)
		}
		if _, err := be.Users(rctx()); err != nil {
			t.Errorf("Users: %v", err)
		}
		if u, err := be.UserByPublicKey(rctx(), pk); err != nil || u.Username() != "collab" {
			t.Errorf("UserByPublicKey = %v, %v; want collab", u, err)
		}
		if u, err := be.UserByAccessToken(rctx(), token); err != nil || u.Username() != "collab" {
			t.Errorf("UserByAccessToken = %v, %v; want collab", u, err)
		}
		if _, err := be.ListPublicKeys(rctx(), "admin"); err != nil {
			t.Errorf("ListPublicKeys: %v", err)
		}
		if got := be.AnonAccess(rctx()); got != access.ReadWriteAccess {
			t.Errorf("AnonAccess = %v, want %v", got, access.ReadWriteAccess)
		}
		if !be.AllowKeyless(rctx()) {
			t.Errorf("AllowKeyless = false, want true")
		}
		if level, ok, err := be.IsCollaborator(rctx(), "repo", "collab"); err != nil || !ok || level != access.ReadOnlyAccess {
			t.Errorf("IsCollaborator = %v, %v, %v; want %v, true, nil", level, ok, err, access.ReadOnlyAccess)
		}
		if _, err := be.Collaborators(rctx(), "repo"); err != nil {
			t.Errorf("Collaborators: %v", err)
		}
		if _, err := be.Repository(rctx(), "repo"); err != nil {
			t.Errorf("Repository: %v", err)
		}
		if _, err := be.Repositories(rctx()); err != nil {
			t.Errorf("Repositories: %v", err)
		}
		if _, err := be.Description(rctx(), "repo"); err != nil {
			t.Errorf("Description: %v", err)
		}
		if _, err := be.Webhook(rctx(), repo, hookID); err != nil {
			t.Errorf("Webhook: %v", err)
		}
		if _, err := be.ListWebhooks(rctx(), repo); err != nil {
			t.Errorf("ListWebhooks: %v", err)
		}
		if ds, err := be.ListWebhookDeliveries(rctx(), repo, hookID); err != nil || len(ds) != 1 {
			t.Errorf("ListWebhookDeliveries = %d deliveries, %v; want 1, nil", len(ds), err)
		}
		if _, err := be.WebhookDelivery(rctx(), repo, hookID, delID); err != nil {
			t.Errorf("WebhookDelivery: %v", err)
		}
		if err := be.RedeliverWebhookDelivery(rctx(), repo, hookID, delID); !errors.Is(err, webhook.ErrWebhookInactive) {
			t.Errorf("RedeliverWebhookDelivery = %v, want %v", err, webhook.ErrWebhookInactive)
		}
	}()

	is.NoErr(<-writerErr)
}
