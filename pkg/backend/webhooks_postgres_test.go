package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
)

// newPostgresWebhookEnv is newWebhookEnv, logging to the default logger,
// for a test that needs Postgres. The test is skipped unless
// SOFT_SERVE_DB_DRIVER is "postgres".
func newPostgresWebhookEnv(t *testing.T) whEnv {
	t.Helper()
	if os.Getenv("SOFT_SERVE_DB_DRIVER") != "postgres" {
		t.Skip("set SOFT_SERVE_DB_DRIVER=postgres to run")
	}
	ctx := context.Background()
	be, cfg := openTestBackend(t, log.Default(), "")

	user, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	ctx = proto.WithUserContext(ctx, user)
	repo, err := be.CreateRepository(ctx, "repo", user, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return whEnv{ctx: ctx, be: be, cfg: cfg, admin: user, repo: repo, logs: &syncBuffer{}}
}

// Postgres runs transactions in READ COMMITTED, so without the repository
// lock a create could count the webhooks while another create, holding room
// for the last one, has not yet committed, and both would insert. Here one
// transaction locks the repository at 19 webhooks and holds on; a
// CreateWebhook started meanwhile must block on that transaction (checked
// with pg_blocking_pids, so the test fails if the lock is removed), and once
// it commits its 20th webhook, must fail.
func TestCreateWebhookCapHoldsUnderConcurrencyPostgres(t *testing.T) {
	t.Parallel()
	e := newPostgresWebhookEnv(t)
	for i := range webhook.MaxWebhooksPerRepo - 1 {
		e.createHook(t, fmt.Sprintf("%s/%d", whURL, i), webhook.EventPush)
	}

	result := make(chan error, 1)
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		var holder int
		if err := tx.GetContext(e.ctx, &holder, `SELECT pg_backend_pid();`); err != nil {
			return err
		}
		n, err := e.be.store.LockAndCountWebhooks(e.ctx, tx, e.repo.ID())
		if err != nil {
			return err
		}
		if got, want := n, webhook.MaxWebhooksPerRepo-1; got != want {
			t.Fatalf("n = %v, want %v", got, want)
		}

		go func() {
			result <- e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: whURL + "/racer", ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true})
		}()
		if err := waitBlockedBy(e, holder, result, "CreateWebhook"); err != nil {
			return err
		}

		_, err = e.be.store.CreateWebhook(e.ctx, tx, models.Webhook{RepoID: e.repo.ID(), URL: whURL + "/last", ContentType: int(webhook.ContentTypeJSON), Active: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-result:
		if !errors.Is(err, webhook.ErrTooManyWebhooks) {
			t.Fatal("not true: errors.Is(err, webhook.ErrTooManyWebhooks)")
		}
	case <-time.After(time.Minute):
		t.Fatal("CreateWebhook still waiting after the lock was released")
	}
	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(hooks); got != webhook.MaxWebhooksPerRepo {
		t.Fatalf("len(hooks) = %v, want %v", got, webhook.MaxWebhooksPerRepo)
	}
}

// waitBlockedBy waits until some backend is waiting for a lock held by the
// backend holder. It returns an error naming op, the operation it watches,
// if result delivers first, meaning op finished without waiting, or if
// nothing blocks within a minute. It returns an error rather than failing
// the test so a caller inside the holder's transaction can return it, which
// rolls the transaction back and lets the waiting operation finish before
// the test database is dropped.
func waitBlockedBy(e whEnv, holder int, result <-chan error, op string) error {
	deadline := time.Now().Add(time.Minute)
	for {
		select {
		case err := <-result:
			return fmt.Errorf("%s did not wait for the repository lock: %w", op, err)
		default:
		}
		var blocked bool
		if err := e.be.db.GetContext(e.ctx, &blocked,
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)));`, holder); err != nil {
			return fmt.Errorf("checking for blocked backends: %w", err)
		}
		if blocked {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not block on the repository lock", op)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Many creates at once on Postgres: exactly the cap succeed, the rest get
// ErrTooManyWebhooks, and none fail with a deadlock or other error.
func TestCreateWebhookConcurrentPostgres(t *testing.T) {
	t.Parallel()
	testConcurrentCreates(t, newPostgresWebhookEnv(t))
}

// Postgres refuses invalid UTF-8 and NUL in TEXT columns. A response with a
// Latin-1 header, a NUL and a multi-byte body cut off at the size limit must
// still be recorded, and redelivering it must not report an error.
func TestWebhookDeliveryNonUTF8ResponsePostgres(t *testing.T) {
	t.Parallel()
	e := newPostgresWebhookEnv(t)

	body := "\x00" + strings.Repeat("€", 64<<10)
	srv := newMemServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Name", "caf\xe9")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))

	e.setWebhookAllowedHosts(t, "127.0.0.0/8")
	id := e.createHook(t, srv.URL+"/hook", webhook.EventRepositoryVisibilityChange)

	if err := e.be.SetPrivate(e.ctx, "repo", true); err != nil {
		t.Fatal(err)
	}
	e.be.WaitForDeliveries(e.ctx)

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

	if err := e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, ds[0].ID); err != nil {
		t.Fatal(err)
	}
	ds, err = e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ds); got != 2 {
		t.Fatalf("len(ds) = %v, want %v", got, 2)
	}
	for _, d := range ds {
		full, err := e.be.WebhookDelivery(e.ctx, e.repo, id, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(full.ResponseHeaders, "X-Name: caf�\n") {
			t.Fatal("not true: strings.Contains(full.ResponseHeaders, \"X-Name: caf�\\n\")")
		}
		if !strings.HasPrefix(full.ResponseBody, "€€") {
			t.Fatal("not true: strings.HasPrefix(full.ResponseBody, \"€€\")")
		}
		if !utf8.ValidString(full.ResponseBody) {
			t.Fatal("not true: utf8.ValidString(full.ResponseBody)")
		}
		if strings.Contains(full.ResponseBody, "\x00") {
			t.Fatal("not true: !strings.Contains(full.ResponseBody, \"\\x00\")")
		}
	}
}
