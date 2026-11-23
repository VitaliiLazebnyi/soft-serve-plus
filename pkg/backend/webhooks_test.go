package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/log/v2"
	charmssh "charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/google/uuid"
)

// Public addresses only: webhook URLs are validated against private ranges.
const (
	whURL      = "https://93.184.216.34/hook"
	whURLOther = "https://93.184.216.35/hook"
)

// syncBuffer is a log sink that background deliveries write to while a test
// may be reading it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// whEnv is a backend wired like a request: config, db and store in the
// context, an admin user, and one repository.
type whEnv struct {
	ctx   context.Context
	be    *Backend
	cfg   *config.Config
	admin proto.User
	repo  proto.Repository
	logs  *syncBuffer
}

func newWebhookEnv(t *testing.T) whEnv {
	t.Helper()
	logs := &syncBuffer{}
	be, cfg := newTestBackendWithLog(t, logs)
	ctx := context.Background()
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	ctx = proto.WithUserContext(ctx, admin)
	repo, err := be.CreateRepository(ctx, "repo", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return whEnv{ctx: ctx, be: be, cfg: cfg, admin: admin, repo: repo, logs: logs}
}

// otherRepo creates a second repository with one webhook and returns the
// webhook ID, so tests can check that repo-scoped calls cannot reach it.
func (e whEnv) otherRepo(t *testing.T) (proto.Repository, int64) {
	t.Helper()
	other, err := e.be.CreateRepository(e.ctx, "other", e.admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.be.CreateWebhook(e.ctx, other, WebhookInput{URL: whURLOther, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true}); err != nil {
		t.Fatal(err)
	}
	hooks, err := e.be.ListWebhooks(e.ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(hooks); got != 1 {
		t.Fatalf("len(hooks) = %v, want %v", got, 1)
	}
	return other, hooks[0].ID
}

func sortedEvents(evs []webhook.Event) []webhook.Event {
	out := slices.Clone(evs)
	slices.Sort(out)
	return out
}

func (e whEnv) createHook(t *testing.T, url string, events ...webhook.Event) int64 {
	t.Helper()
	if err := e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: url, ContentType: webhook.ContentTypeJSON, Events: events, Active: true}); err != nil {
		t.Fatal(err)
	}
	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hooks {
		if h.URL == url {
			return h.ID
		}
	}
	t.Fatalf("webhook %s not found", url)
	return 0
}

func TestCreateWebhookStoresFields(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)

	if err := e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeForm, Secret: "s3cret", Events: []webhook.Event{webhook.EventPush, webhook.EventBranchTagCreate}, Active: true}); err != nil {
		t.Fatal(err)
	}

	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(hooks); got != 1 {
		t.Fatalf("len(hooks) = %v, want %v", got, 1)
	}
	h := hooks[0]
	if got := h.URL; got != whURL {
		t.Fatalf("h.URL = %v, want %v", got, whURL)
	}
	if got := h.ContentType; got != webhook.ContentTypeForm {
		t.Fatalf("h.ContentType = %v, want %v", got, webhook.ContentTypeForm)
	}
	if got := h.Secret; got != "s3cret" {
		t.Fatalf("h.Secret = %v, want %v", got, "s3cret")
	}
	if !h.Active {
		t.Fatal("not true: h.Active")
	}
	if got, want := h.RepoID, e.repo.ID(); got != want {
		t.Fatalf("h.RepoID = %v, want %v", got, want)
	}
	if got, want := sortedEvents(h.Events), ([]webhook.Event{webhook.EventBranchTagCreate, webhook.EventPush}); !reflect.DeepEqual(got, want) {
		t.Fatalf("sortedEvents(h.Events) = %v, want %v", got, want)
	}

	got, err := e.be.Webhook(e.ctx, e.repo, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := got.URL; got != whURL {
		t.Fatalf("got.URL = %v, want %v", got, whURL)
	}
	if got, want := sortedEvents(got.Events), ([]webhook.Event{webhook.EventBranchTagCreate, webhook.EventPush}); !reflect.DeepEqual(got, want) {
		t.Fatalf("sortedEvents(got.Events) = %v, want %v", got, want)
	}
}

func TestListWebhooksEmpty(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)

	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(hooks); got != 0 {
		t.Fatalf("len(hooks) = %v, want %v", got, 0)
	}
}

func TestCreateWebhookRejectsUnsafeURLs(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)

	for _, u := range []string{
		"",
		"not a url",
		"ftp://93.184.216.34/hook",
		"http://127.0.0.1/hook",
		"http://10.0.0.1/hook",
		"http://169.254.169.254/latest/meta-data",
	} {
		err := e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: u, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true})
		if err == nil {
			t.Fatal("not true: err != nil")
		}
	}

	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(hooks); got != 0 {
		t.Fatalf("len(hooks) = %v, want %v", got, 0)
	}
}

func TestCreateWebhookSanitizesURL(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)

	if err := e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: whURL + "\x1b[31m", ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true}); err != nil {
		t.Fatal(err)
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

// An unknown content type is refused: stored, it would fail every later
// delivery to the webhook.
func TestCreateWebhookRejectsInvalidContentType(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)

	err := e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: whURL, ContentType: webhook.ContentType(99), Events: []webhook.Event{webhook.EventPush}, Active: true})
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

// An event value outside the known set is refused, not stored as is.
func TestCreateWebhookRejectsUnknownEvent(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)

	err := e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.Event(99)}, Active: true})
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

// A repeated event is stored once, rather than violate the unique
// (webhook_id, event) constraint and fail the whole create with a raw
// database error.
func TestCreateWebhookAcceptsDuplicateEvents(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)

	if err := e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush, webhook.EventPush}, Active: true}); err != nil {
		t.Fatal(err)
	}

	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(hooks); got != 1 {
		t.Fatalf("len(hooks) = %v, want %v", got, 1)
	}
	if got, want := hooks[0].Events, ([]webhook.Event{webhook.EventPush}); !reflect.DeepEqual(got, want) {
		t.Fatalf("hooks[0].Events = %v, want %v", got, want)
	}
}

// A repository can have at most MaxWebhooksPerRepo webhooks, however their
// URLs differ. Deleting one makes room again, and other repositories are not
// affected.
func TestCreateWebhookCapsWebhooksPerRepo(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	var ids []int64
	for i := range webhook.MaxWebhooksPerRepo {
		ids = append(ids, e.createHook(t, fmt.Sprintf("%s/%d", whURL, i), webhook.EventPush))
	}

	err := e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: whURL + "/extra", ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true})
	if !errors.Is(err, webhook.ErrTooManyWebhooks) {
		t.Fatal("not true: errors.Is(err, webhook.ErrTooManyWebhooks)")
	}
	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(hooks); got != webhook.MaxWebhooksPerRepo {
		t.Fatalf("len(hooks) = %v, want %v", got, webhook.MaxWebhooksPerRepo)
	}

	_, _ = e.otherRepo(t)

	if err := e.be.DeleteWebhook(e.ctx, e.repo, ids[0]); err != nil {
		t.Fatal(err)
	}
	e.createHook(t, whURL+"/extra", webhook.EventPush)
}

// Concurrent creates on SQLite must not fail with "database is locked". A
// transaction that reads before it writes cannot wait for another writer
// (SQLite returns SQLITE_BUSY at once, without busy_timeout), so the cap is
// checked after the insert. The creates together hold the write lock for
// longer than busy_timeout on a slow disk (Windows), so they also rely on
// the write transactions of one process taking turns (see db.transaction).
func TestCreateWebhookConcurrentSQLite(t *testing.T) {
	t.Parallel()
	testConcurrentCreates(t, newWebhookEnv(t))
}

// testConcurrentCreates starts twice MaxWebhooksPerRepo creates on one
// repository at once. Exactly MaxWebhooksPerRepo must succeed and the rest be
// refused with ErrTooManyWebhooks; any other error fails the test.
func testConcurrentCreates(t *testing.T, e whEnv) {
	t.Helper()
	const n = 2 * webhook.MaxWebhooksPerRepo
	errs := make(chan error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			errs <- e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: fmt.Sprintf("%s/%d", whURL, i), ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true})
		})
	}
	close(start)
	wg.Wait()
	close(errs)

	var ok, capped int
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, webhook.ErrTooManyWebhooks):
			capped++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if got := ok; got != webhook.MaxWebhooksPerRepo {
		t.Fatalf("ok = %v, want %v", got, webhook.MaxWebhooksPerRepo)
	}
	if got, want := capped, n-webhook.MaxWebhooksPerRepo; got != want {
		t.Fatalf("capped = %v, want %v", got, want)
	}
	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(hooks); got != webhook.MaxWebhooksPerRepo {
		t.Fatalf("len(hooks) = %v, want %v", got, webhook.MaxWebhooksPerRepo)
	}
}

func TestUpdateWebhookChangesEverything(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush, webhook.EventCollaborator)

	if err := e.be.UpdateWebhook(e.ctx, e.repo, id, WebhookInput{URL: whURLOther, ContentType: webhook.ContentTypeForm, Secret: "new", Events: []webhook.Event{webhook.EventPush, webhook.EventRepository}, Active: false}); err != nil {
		t.Fatal(err)
	}

	got, err := e.be.Webhook(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := got.URL; got != whURLOther {
		t.Fatalf("got.URL = %v, want %v", got, whURLOther)
	}
	if got := got.ContentType; got != webhook.ContentTypeForm {
		t.Fatalf("got.ContentType = %v, want %v", got, webhook.ContentTypeForm)
	}
	if got := got.Secret; got != "new" {
		t.Fatalf("got.Secret = %v, want %v", got, "new")
	}
	if got.Active {
		t.Fatal("not true: !got.Active")
	}
	if got, want := sortedEvents(got.Events), ([]webhook.Event{webhook.EventPush, webhook.EventRepository}); !reflect.DeepEqual(got, want) {
		t.Fatalf("sortedEvents(got.Events) = %v, want %v", got, want)
	}
}

// An update that keeps the same events succeeds: with no event to remove,
// UpdateWebhook must not pass an empty list to DeleteWebhookEventsByID, as
// sqlx rejects an empty IN clause.
func TestUpdateWebhookWithSameEvents(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)

	if err := e.be.UpdateWebhook(e.ctx, e.repo, id, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true}); err != nil {
		t.Fatal(err)
	}
}

// A plain add (nothing removed) must not hit the same empty IN clause.
func TestUpdateWebhookAddsEventOnly(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)

	if err := e.be.UpdateWebhook(e.ctx, e.repo, id, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush, webhook.EventRepository}, Active: true}); err != nil {
		t.Fatal(err)
	}

	got, err := e.be.Webhook(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedEvents(got.Events), ([]webhook.Event{webhook.EventPush, webhook.EventRepository}); !reflect.DeepEqual(got, want) {
		t.Fatalf("sortedEvents(got.Events) = %v, want %v", got, want)
	}
}

// UpdateWebhook sanitizes the URL as CreateWebhook does, so the stored URL
// does not depend on which call wrote it.
func TestUpdateWebhookSanitizesURL(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)

	if err := e.be.UpdateWebhook(e.ctx, e.repo, id, WebhookInput{URL: whURLOther + "\x1b[31m", ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true}); err != nil {
		t.Fatal(err)
	}

	got, err := e.be.Webhook(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := got.URL; got != whURLOther {
		t.Fatalf("got.URL = %v, want %v", got, whURLOther)
	}
}

func TestUpdateWebhookRejectsUnsafeURL(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)

	err := e.be.UpdateWebhook(e.ctx, e.repo, id, WebhookInput{URL: "http://127.0.0.1/hook", ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true})
	if err == nil {
		t.Fatal("not true: err != nil")
	}

	got, err := e.be.Webhook(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := got.URL; got != whURL {
		t.Fatalf("got.URL = %v, want %v", got, whURL)
	}
}

// Updating a webhook id that does not belong to the repo fails: the store's
// UPDATE matches zero rows, which must not pass silently.
func TestUpdateWebhookUnknownIDFails(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)

	err := e.be.UpdateWebhook(e.ctx, e.repo, 424242, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true})
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

// Updating another repo's webhook through this repo does not rewrite that
// webhook's events, as an event rewrite after a zero-row update would.
func TestUpdateWebhookCannotTouchOtherRepo(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	other, otherID := e.otherRepo(t)

	_ = e.be.UpdateWebhook(e.ctx, e.repo, otherID, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeForm, Secret: "pwned", Events: []webhook.Event{webhook.EventRepository}, Active: false})

	got, err := e.be.Webhook(e.ctx, other, otherID)
	if err != nil {
		t.Fatal(err)
	}
	if got := got.URL; got != whURLOther {
		t.Fatalf("got.URL = %v, want %v", got, whURLOther)
	}
	if got := got.ContentType; got != webhook.ContentTypeJSON {
		t.Fatalf("got.ContentType = %v, want %v", got, webhook.ContentTypeJSON)
	}
	if got := got.Secret; got != "" {
		t.Fatalf("got.Secret = %v, want %v", got, "")
	}
	if !got.Active {
		t.Fatal("not true: got.Active")
	}
	if got, want := got.Events, ([]webhook.Event{webhook.EventPush}); !reflect.DeepEqual(got, want) {
		t.Fatalf("got.Events = %v, want %v", got, want)
	}
}

func TestWebhookGetScopedToRepo(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	other, otherID := e.otherRepo(t)
	_ = other

	_, err := e.be.Webhook(e.ctx, e.repo, otherID)
	if err == nil {
		t.Fatal("not true: err != nil")
	}

	_, err = e.be.Webhook(e.ctx, e.repo, 424242)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

func TestDeleteWebhookScopedToRepo(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	other, otherID := e.otherRepo(t)

	if e.be.DeleteWebhook(e.ctx, e.repo, otherID) == nil {
		t.Fatal("not true: e.be.DeleteWebhook(e.ctx, e.repo, otherID) != nil")
	}
	if e.be.DeleteWebhook(e.ctx, e.repo, 424242) == nil {
		t.Fatal("not true: e.be.DeleteWebhook(e.ctx, e.repo, 424242) != nil")
	}

	hooks, err := e.be.ListWebhooks(e.ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(hooks); got != 1 {
		t.Fatalf("len(hooks) = %v, want %v", got, 1)
	}
}

// Deleting a webhook removes its events and deliveries.
func TestDeleteWebhookRemovesEvents(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush, webhook.EventRepository)
	e.seedDelivery(t, id, whURL, `{}`)

	if err := e.be.DeleteWebhook(e.ctx, e.repo, id); err != nil {
		t.Fatal(err)
	}

	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(hooks); got != 0 {
		t.Fatalf("len(hooks) = %v, want %v", got, 0)
	}

	events, err := e.be.store.GetWebhookEventsByWebhookID(e.ctx, e.be.db, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(events); got != 0 {
		t.Fatalf("len(events) = %v, want %v", got, 0)
	}
	var deliveries int
	if err := e.be.db.GetContext(e.ctx, &deliveries, e.be.db.Rebind("SELECT COUNT(*) FROM webhook_deliveries WHERE webhook_id = ?"), id); err != nil {
		t.Fatal(err)
	}
	if got := deliveries; got != 0 {
		t.Fatalf("deliveries = %v, want %v", got, 0)
	}
}

// seedDelivery stores a delivery row directly, so the read paths can be
// tested without sending anything over the network.
func (e whEnv) seedDelivery(t *testing.T, webhookID int64, url string, body string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.CreateWebhookDelivery(e.ctx, tx, models.WebhookDelivery{
			ID:             id,
			WebhookID:      webhookID,
			Event:          int(webhook.EventPush),
			RequestURL:     url,
			RequestMethod:  "POST",
			RequestHeaders: "X-Test: 1\n",
			RequestBody:    body,
			ResponseStatus: 200,
			ResponseBody:   "ok",
		})
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestListAndReadDeliveries(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	delID := e.seedDelivery(t, id, whURL, `{"ref":"main"}`)

	list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(list); got != 1 {
		t.Fatalf("len(list) = %v, want %v", got, 1)
	}
	if got, want := list[0].ID, delID; !reflect.DeepEqual(got, want) {
		t.Fatalf("list[0].ID = %v, want %v", got, want)
	}
	if got := list[0].Event; got != webhook.EventPush {
		t.Fatalf("list[0].Event = %v, want %v", got, webhook.EventPush)
	}

	d, err := e.be.WebhookDelivery(e.ctx, e.repo, id, delID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := d.ID, delID; !reflect.DeepEqual(got, want) {
		t.Fatalf("d.ID = %v, want %v", got, want)
	}
	if got := d.RequestBody; got != `{"ref":"main"}` {
		t.Fatalf("d.RequestBody = %v, want %v", got, `{"ref":"main"}`)
	}
	if got := d.Event; got != webhook.EventPush {
		t.Fatalf("d.Event = %v, want %v", got, webhook.EventPush)
	}
}

func TestDeliveriesScopedToRepo(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	other, otherID := e.otherRepo(t)
	delID := e.seedDelivery(t, otherID, whURLOther, `{}`)

	_, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, otherID)
	if err == nil {
		t.Fatal("not true: err != nil")
	}

	_, err = e.be.WebhookDelivery(e.ctx, e.repo, otherID, delID)
	if err == nil {
		t.Fatal("not true: err != nil")
	}

	// The owning repo still sees it.
	d, err := e.be.WebhookDelivery(e.ctx, other, otherID, delID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := d.ID, delID; !reflect.DeepEqual(got, want) {
		t.Fatalf("d.ID = %v, want %v", got, want)
	}
}

func TestDeliveriesUnknown(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)

	_, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, 424242)
	if err == nil {
		t.Fatal("not true: err != nil")
	}

	_, err = e.be.WebhookDelivery(e.ctx, e.repo, id, uuid.New())
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

// Redelivery to a private address fails at dial time. The failure is recorded
// as a new delivery, and the call itself returns nil.
func TestRedeliverRecordsNewDelivery(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	// Point the stored hook at a private address without validation, so the
	// redelivery reaches the SSRF-protected client and fails there.
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.UpdateWebhookByID(e.ctx, tx, models.Webhook{ID: id, RepoID: e.repo.ID(), URL: "http://127.0.0.1:1/", ContentType: int(webhook.ContentTypeJSON), Active: true})
	}); err != nil {
		t.Fatal(err)
	}
	delID := e.seedDelivery(t, id, "http://127.0.0.1:1/", `{"ref":"main"}`)

	if err := e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID); err != nil {
		t.Fatal(err)
	}

	list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(list); got != 2 {
		t.Fatalf("len(list) = %v, want %v", got, 2)
	}
}

// Redelivery logs identifiers only, never the stored request body, headers
// or the hook URL.
//
// The old leak went through the global logger, so that is captured too. The
// test swaps the global default logger and must not run in parallel.
func TestRedeliverDoesNotLogPayload(t *testing.T) {
	e := newWebhookEnv(t)
	prev := log.Default()
	log.SetDefault(log.New(e.logs))
	t.Cleanup(func() { log.SetDefault(prev) })
	id := e.createHook(t, whURL, webhook.EventPush)
	const hookURL = "http://127.0.0.1:1/secret-path?token=hook-token"
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.UpdateWebhookByID(e.ctx, tx, models.Webhook{ID: id, RepoID: e.repo.ID(), URL: hookURL, ContentType: int(webhook.ContentTypeJSON), Active: true})
	}); err != nil {
		t.Fatal(err)
	}
	const marker = "payload-marker-do-not-log"
	delID := e.seedDelivery(t, id, hookURL, `{"ref":"main","commit":"`+marker+`"}`)

	if err := e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID); err != nil {
		t.Fatal(err)
	}

	logs := e.logs.String()
	if !strings.Contains(logs, "redelivering webhook delivery") {
		t.Fatal("not true: strings.Contains(logs, \"redelivering webhook delivery\")")
	}
	if !strings.Contains(logs, delID.String()) {
		t.Fatal("not true: strings.Contains(logs, delID.String())")
	}
	for _, leak := range []string{marker, `"ref"`, "X-Test", "secret-path", "hook-token"} {
		if strings.Contains(logs, leak) {
			t.Errorf("redelivery log contains %q:\n%s", leak, logs)
		}
	}
}

// A stored body that fails to parse is reported to the caller, but the parse
// error is not logged, since it quotes part of the body.
func TestRedeliverParseErrorNotLogged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ct   webhook.ContentType
		body string
		msg  string
		// leak is the part of the body the parse error quotes.
		leak string
	}{
		{"json", webhook.ContentTypeJSON, `{"a":x}`, "error unmarshaling webhook payload", "'x'"},
		{"form", webhook.ContentTypeForm, `a=%zz`, "error parsing webhook form body", "%zz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newWebhookEnv(t)
			id := e.createHook(t, whURL, webhook.EventPush)
			if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
				return e.be.store.UpdateWebhookByID(e.ctx, tx, models.Webhook{ID: id, RepoID: e.repo.ID(), URL: whURL, ContentType: int(tc.ct), Active: true})
			}); err != nil {
				t.Fatal(err)
			}
			delID := e.seedDelivery(t, id, whURL, tc.body)

			err := e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID)
			if err == nil {
				t.Fatal("not true: err != nil")
			}
			logs := e.logs.String()
			if !strings.Contains(logs, tc.msg) {
				t.Fatal("not true: strings.Contains(logs, tc.msg)")
			}
			if !strings.Contains(err.Error(), tc.leak) {
				t.Fatal("not true: strings.Contains(err.Error(), tc.leak)")
			}
			if strings.Contains(logs, tc.leak) {
				t.Errorf("log contains %q from the parse error %q:\n%s", tc.leak, err, logs)
			}
		})
	}
}

// A form-encoded webhook can be redelivered: its stored body is sent as
// stored, not form-encoded again from JSON, which fails.
func TestRedeliverFormWebhook(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.UpdateWebhookByID(e.ctx, tx, models.Webhook{ID: id, RepoID: e.repo.ID(), URL: "http://127.0.0.1:1/", ContentType: int(webhook.ContentTypeForm), Active: true})
	}); err != nil {
		t.Fatal(err)
	}
	const body = "n=1&ref=main"
	delID := e.seedDelivery(t, id, "http://127.0.0.1:1/", body)

	if err := e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID); err != nil {
		t.Fatal(err)
	}

	// The redelivery is recorded with the same form-encoded body.
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
		rd, err := e.be.WebhookDelivery(e.ctx, e.repo, id, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got := rd.RequestBody; got != body {
			t.Fatalf("rd.RequestBody = %v, want %v", got, body)
		}
	}
}

// A delivery kept by retention pruning can still be redelivered; a pruned one
// is reported as not found.
func TestRedeliverAfterPrune(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.UpdateWebhookByID(e.ctx, tx, models.Webhook{ID: id, RepoID: e.repo.ID(), URL: "http://127.0.0.1:1/", ContentType: int(webhook.ContentTypeJSON), Active: true})
	}); err != nil {
		t.Fatal(err)
	}
	old := e.seedDelivery(t, id, "http://127.0.0.1:1/", `{"ref":"old"}`)
	kept := e.seedDelivery(t, id, "http://127.0.0.1:1/", `{"ref":"new"}`)
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.PruneWebhookDeliveries(e.ctx, tx, id, 1)
	}); err != nil {
		t.Fatal(err)
	}

	if !errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, old), proto.ErrDeliveryNotFound) {
		t.Fatal("not true: errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, old), proto.ErrDeliveryNotFound)")
	}
	if err := e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, kept); err != nil {
		t.Fatal(err)
	}

	// The redelivery is recorded next to the delivery it repeats.
	list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(list); got != 2 {
		t.Fatalf("len(list) = %v, want %v", got, 2)
	}
}

func TestRedeliverFailures(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	bad := e.seedDelivery(t, id, whURL, `not json`)
	good := e.seedDelivery(t, id, whURL, `{}`)
	other, otherID := e.otherRepo(t)
	otherDel := e.seedDelivery(t, otherID, whURLOther, `{}`)
	_ = other

	// Unknown delivery and unknown webhook are both reported as not found.
	if !errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, uuid.New()), proto.ErrDeliveryNotFound) {
		t.Fatal("not true: errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, uuid.New()), proto.ErrDeliveryNotFound)")
	}
	if !errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, 424242, good), proto.ErrWebhookNotFound) {
		t.Fatal("not true: errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, 424242, good), proto.ErrWebhookNotFound)")
	}
	// Delivery exists, but its body is not valid JSON.
	if e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, bad) == nil {
		t.Fatal("not true: e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, bad) != nil")
	}
	// A webhook owned by another repo cannot be redelivered through this one.
	if e.be.RedeliverWebhookDelivery(e.ctx, e.repo, otherID, otherDel) == nil {
		t.Fatal("not true: e.be.RedeliverWebhookDelivery(e.ctx, e.repo, otherID, otherDel) != nil")
	}
}

// Error paths inside transactions are reached by removing the table the
// operation reads or writes. The database is per test, so this is safe.
func TestErrorPathsWhenStoreFails(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	delID := e.seedDelivery(t, id, whURL, `{}`)

	drop := func(table string) {
		t.Helper()
		_, err := e.be.db.ExecContext(e.ctx, "DROP TABLE "+table)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Events table gone: reads and writes of events fail.
	drop("webhook_events")
	_, err := e.be.ListWebhooks(e.ctx, e.repo)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	_, err = e.be.Webhook(e.ctx, e.repo, id)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if e.be.UpdateWebhook(e.ctx, e.repo, id, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventRepository}, Active: true}) == nil {
		t.Fatal("not true: e.be.UpdateWebhook(e.ctx, e.repo, id, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventRepository}, Active: true}) != nil")
	}
	if e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: whURLOther, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true}) == nil {
		t.Fatal("not true: e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: whURLOther, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true}) != nil")
	}

	// Deliveries table gone.
	drop("webhook_deliveries")
	_, err = e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	_, err = e.be.WebhookDelivery(e.ctx, e.repo, id, delID)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID) == nil {
		t.Fatal("not true: e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID) != nil")
	}

	// Webhooks table gone: every entry point fails.
	drop("webhooks")
	if e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true}) == nil {
		t.Fatal("not true: e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true}) != nil")
	}
	_, err = e.be.ListWebhooks(e.ctx, e.repo)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if e.be.UpdateWebhook(e.ctx, e.repo, id, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true}) == nil {
		t.Fatal("not true: e.be.UpdateWebhook(e.ctx, e.repo, id, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true}) != nil")
	}
	if e.be.DeleteWebhook(e.ctx, e.repo, id) == nil {
		t.Fatal("not true: e.be.DeleteWebhook(e.ctx, e.repo, id) != nil")
	}
}

// A URL already used by another webhook of the same repository violates the
// unique (repo_id, url) constraint, so the update fails and changes nothing.
func TestUpdateWebhookFailsOnDuplicateURL(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	a := e.createHook(t, whURL, webhook.EventPush)
	b := e.createHook(t, whURLOther, webhook.EventPush)

	if e.be.UpdateWebhook(e.ctx, e.repo, b, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventRepository}, Active: true}) == nil {
		t.Fatal("not true: e.be.UpdateWebhook(e.ctx, e.repo, b, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventRepository}, Active: true}) != nil")
	}

	gotB, err := e.be.Webhook(e.ctx, e.repo, b)
	if err != nil {
		t.Fatal(err)
	}
	if got := gotB.URL; got != whURLOther {
		t.Fatalf("gotB.URL = %v, want %v", got, whURLOther)
	}
	if got, want := gotB.Events, ([]webhook.Event{webhook.EventPush}); !reflect.DeepEqual(got, want) {
		t.Fatalf("gotB.Events = %v, want %v", got, want)
	}
	gotA, err := e.be.Webhook(e.ctx, e.repo, a)
	if err != nil {
		t.Fatal(err)
	}
	if got := gotA.URL; got != whURL {
		t.Fatalf("gotA.URL = %v, want %v", got, whURL)
	}
}

// execTrigger makes writes to table fail with a database error (see
// refuseTrigger).
func (e whEnv) execTrigger(t *testing.T, name, timing, op, table string) {
	t.Helper()
	refuseTrigger(t, e.ctx, e.be.db, name, timing, op, table)
}

// dropTrigger removes the trigger execTrigger created as name.
func (e whEnv) dropTrigger(t *testing.T, name string) {
	t.Helper()
	execAll(t, e.ctx, e.be.db, byDriver(e.be.db,
		[]string{"DROP TRIGGER " + name},
		[]string{"DROP FUNCTION " + name + "() CASCADE"}))
}

// refuseTrigger creates the trigger name, which makes each row write of kind
// op (such as "INSERT", "DELETE" or "UPDATE OF name") on table fail with a
// database error, at timing "BEFORE" or "AFTER". On Postgres the trigger
// runs a function of the same name. Triggers live only in the per-test
// database.
func refuseTrigger(t *testing.T, ctx context.Context, dbx *db.DB, name, timing, op, table string) {
	t.Helper()
	execAll(t, ctx, dbx, byDriver(dbx,
		[]string{"CREATE TRIGGER " + name + " " + timing + " " + op + " ON " + table +
			" BEGIN SELECT RAISE(ABORT, 'forced failure'); END;"},
		[]string{
			"CREATE FUNCTION " + name + "() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced failure'; END $$;",
			"CREATE TRIGGER " + name + " " + timing + " " + op + " ON " + table + " FOR EACH ROW EXECUTE FUNCTION " + name + "();",
		}))
}

// byDriver returns sqlite or postgres, whichever is for dbx's driver.
func byDriver[T any](dbx *db.DB, sqlite, postgres T) T {
	if dbx.DriverName() == "postgres" {
		return postgres
	}
	return sqlite
}

// execAll runs each statement on dbx, failing the test at the first error.
func execAll(t *testing.T, ctx context.Context, dbx *db.DB, stmts []string) {
	t.Helper()
	for _, q := range stmts {
		if _, err := dbx.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// A failure while removing an event must roll back the whole update, so the
// URL and the remaining events stay as they were.
func TestUpdateWebhookRollsBackOnEventDeleteFailure(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush, webhook.EventCollaborator)

	e.execTrigger(t, "fail_event_delete", "BEFORE", "DELETE", "webhook_events")
	if e.be.UpdateWebhook(e.ctx, e.repo, id, WebhookInput{URL: whURLOther, ContentType: webhook.ContentTypeForm, Secret: "x", Events: []webhook.Event{webhook.EventPush}, Active: false}) == nil {
		t.Fatal("not true: e.be.UpdateWebhook(e.ctx, e.repo, id, WebhookInput{URL: whURLOther, ContentType: webhook.ContentTypeForm, Secret: \"x\", Events: []webhook.Event{webhook.EventPush}, Active: false}) != nil")
	}
	e.dropTrigger(t, "fail_event_delete")

	got, err := e.be.Webhook(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := got.URL; got != whURL {
		t.Fatalf("got.URL = %v, want %v", got, whURL)
	}
	if got := got.ContentType; got != webhook.ContentTypeJSON {
		t.Fatalf("got.ContentType = %v, want %v", got, webhook.ContentTypeJSON)
	}
	if got, want := sortedEvents(got.Events), ([]webhook.Event{webhook.EventCollaborator, webhook.EventPush}); !reflect.DeepEqual(got, want) {
		t.Fatalf("sortedEvents(got.Events) = %v, want %v", got, want)
	}
}

// A failure while adding an event must roll back the whole update.
func TestUpdateWebhookRollsBackOnEventInsertFailure(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)

	e.execTrigger(t, "fail_event_insert", "BEFORE", "INSERT", "webhook_events")
	if e.be.UpdateWebhook(e.ctx, e.repo, id, WebhookInput{URL: whURLOther, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush, webhook.EventRepository}, Active: true}) == nil {
		t.Fatal("not true: e.be.UpdateWebhook(e.ctx, e.repo, id, WebhookInput{URL: whURLOther, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush, webhook.EventRepository}, Active: true}) != nil")
	}
	e.dropTrigger(t, "fail_event_insert")

	got, err := e.be.Webhook(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := got.URL; got != whURL {
		t.Fatalf("got.URL = %v, want %v", got, whURL)
	}
	if got, want := got.Events, ([]webhook.Event{webhook.EventPush}); !reflect.DeepEqual(got, want) {
		t.Fatalf("got.Events = %v, want %v", got, want)
	}
}

// Each dependent-row delete in DeleteWebhook can fail. A failure must leave
// the webhook, its events and its deliveries in place.
func TestDeleteWebhookRollsBackOnStoreFailure(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"webhook_deliveries", "webhook_events", "webhooks"} {
		t.Run(table, func(t *testing.T) {
			t.Parallel()
			e := newWebhookEnv(t)
			id := e.createHook(t, whURL, webhook.EventPush)
			delID := e.seedDelivery(t, id, whURL, `{}`)

			e.execTrigger(t, "fail_delete", "BEFORE", "DELETE", table)
			if e.be.DeleteWebhook(e.ctx, e.repo, id) == nil {
				t.Fatal("not true: e.be.DeleteWebhook(e.ctx, e.repo, id) != nil")
			}
			e.dropTrigger(t, "fail_delete")

			_, err := e.be.Webhook(e.ctx, e.repo, id)
			if err != nil {
				t.Fatal(err)
			}
			events, err := e.be.store.GetWebhookEventsByWebhookID(e.ctx, e.be.db, id)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(events); got != 1 {
				t.Fatalf("len(events) = %v, want %v", got, 1)
			}
			_, err = e.be.WebhookDelivery(e.ctx, e.repo, id, delID)
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A failure while creating events fails the whole create.
func TestCreateWebhookRollsBackOnEventInsertFailure(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)

	e.execTrigger(t, "fail_create_event", "BEFORE", "INSERT", "webhook_events")
	if e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true}) == nil {
		t.Fatal("not true: e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true}) != nil")
	}
	e.dropTrigger(t, "fail_create_event")

	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(hooks); got != 0 {
		t.Fatalf("len(hooks) = %v, want %v", got, 0)
	}
}

// A stored form body that is not a valid query string cannot be redelivered.
func TestRedeliverMalformedFormBody(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.UpdateWebhookByID(e.ctx, tx, models.Webhook{ID: id, RepoID: e.repo.ID(), URL: "http://127.0.0.1:1/", ContentType: int(webhook.ContentTypeForm), Active: true})
	}); err != nil {
		t.Fatal(err)
	}
	delID := e.seedDelivery(t, id, "http://127.0.0.1:1/", "%zz=1")

	if e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID) == nil {
		t.Fatal("not true: e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID) != nil")
	}
}

// A redelivery sends the stored body in the content type the delivery was
// recorded with, even if the webhook's content type changed afterwards.
func TestRedeliverUsesStoredContentTypeAfterWebhookChanged(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	const body = "n=1&ref=main"

	// Record a form delivery, then switch the webhook to JSON.
	delID := uuid.New()
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.CreateWebhookDelivery(e.ctx, tx, models.WebhookDelivery{
			ID:             delID,
			WebhookID:      id,
			Event:          int(webhook.EventPush),
			RequestURL:     "http://127.0.0.1:1/",
			RequestMethod:  "POST",
			RequestHeaders: "Content-Type: application/x-www-form-urlencoded\nX-Test: 1\n",
			RequestBody:    body,
			ResponseStatus: 200,
			ResponseBody:   "ok",
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.UpdateWebhookByID(e.ctx, tx, models.Webhook{ID: id, RepoID: e.repo.ID(), URL: "http://127.0.0.1:1/", ContentType: int(webhook.ContentTypeJSON), Active: true})
	}); err != nil {
		t.Fatal(err)
	}

	if err := e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID); err != nil {
		t.Fatal(err)
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
		rd, err := e.be.WebhookDelivery(e.ctx, e.repo, id, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got := rd.RequestBody; got != body {
			t.Fatalf("rd.RequestBody = %v, want %v", got, body)
		}
		if !strings.Contains(rd.RequestHeaders, "Content-Type: application/x-www-form-urlencoded") {
			t.Fatal("not true: strings.Contains(rd.RequestHeaders, \"Content-Type: application/x-www-form-urlencoded\")")
		}
	}
}

// A stored Content-Type that is not a known content type does not stop a
// redelivery: the webhook's current content type is used and the fallback is
// logged.
func TestRedeliverFallsBackOnUnknownStoredContentType(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	delID := uuid.New()
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.CreateWebhookDelivery(e.ctx, tx, models.WebhookDelivery{
			ID:             delID,
			WebhookID:      id,
			Event:          int(webhook.EventPush),
			RequestURL:     "http://127.0.0.1:1/",
			RequestMethod:  "POST",
			RequestHeaders: "Content-Type: text/xml\nX-Test: 1\n",
			RequestBody:    `{"ref":"main"}`,
			ResponseStatus: 200,
			ResponseBody:   "ok",
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.UpdateWebhookByID(e.ctx, tx, models.Webhook{ID: id, RepoID: e.repo.ID(), URL: "http://127.0.0.1:1/", ContentType: int(webhook.ContentTypeJSON), Active: true})
	}); err != nil {
		t.Fatal(err)
	}

	if err := e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(e.logs.String(), "unrecognized content type in webhook delivery") {
		t.Fatal("not true: strings.Contains(e.logs.String(), \"unrecognized content type in webhook delivery\")")
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
		rd, err := e.be.WebhookDelivery(e.ctx, e.repo, id, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(rd.RequestHeaders, "Content-Type: application/json") {
			t.Fatal("not true: strings.Contains(rd.RequestHeaders, \"Content-Type: application/json\")")
		}
		if got := strings.TrimSpace(rd.RequestBody); got != `{"ref":"main"}` {
			t.Fatalf("strings.TrimSpace(rd.RequestBody) = %v, want %v", got, `{"ref":"main"}`)
		}
	}
}

// A webhook created for a repository deleted meanwhile fails on its foreign
// key. That must be reported as the repository being gone, not as a
// duplicate key.
func TestCreateWebhookOnDeletedRepository(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	if err := e.be.DeleteRepository(e.ctx, "repo"); err != nil {
		t.Fatal(err)
	}

	err := e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: whURL, ContentType: webhook.ContentTypeJSON, Events: []webhook.Event{webhook.EventPush}, Active: true})
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
}

// redeliveredHeaders redelivers delID and returns the request headers of the
// delivery the redelivery recorded.
func (e whEnv) redeliveredHeaders(t *testing.T, id int64, delID uuid.UUID) string {
	t.Helper()
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.UpdateWebhookByID(e.ctx, tx, models.Webhook{ID: id, RepoID: e.repo.ID(), URL: "http://127.0.0.1:1/", ContentType: int(webhook.ContentTypeJSON), Active: true})
	}); err != nil {
		t.Fatal(err)
	}
	before, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[uuid.UUID]bool{}
	for _, d := range before {
		seen[d.ID] = true
	}

	if err := e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID); err != nil {
		t.Fatal(err)
	}

	list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(list), len(before)+1; got != want {
		t.Fatalf("len(list) = %v, want %v", got, want)
	}
	for _, d := range list {
		if seen[d.ID] {
			continue
		}
		rd, err := e.be.WebhookDelivery(e.ctx, e.repo, id, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		return rd.RequestHeaders
	}
	t.Fatal("no new delivery recorded")
	return ""
}

// A redelivery sends the delivery GUID of the delivery it repeats, and a
// redelivery of that redelivery keeps the same GUID.
func TestRedeliverKeepsDeliveryGUID(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	guid := uuid.New()
	delID := uuid.New()
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.CreateWebhookDelivery(e.ctx, tx, models.WebhookDelivery{
			ID:             delID,
			WebhookID:      id,
			Event:          int(webhook.EventPush),
			RequestURL:     "http://127.0.0.1:1/",
			RequestMethod:  "POST",
			RequestHeaders: "Content-Type: application/json\nX-Softserve-Delivery: " + guid.String() + "\n",
			RequestBody:    `{"ref":"main"}`,
			ResponseStatus: 200,
			ResponseBody:   "ok",
		})
	}); err != nil {
		t.Fatal(err)
	}

	headers := e.redeliveredHeaders(t, id, delID)
	if !strings.Contains(headers, "X-Softserve-Delivery: "+guid.String()+"\n") {
		t.Fatal("not true: strings.Contains(headers, \"X-Softserve-Delivery: \"+guid.String()+\"\\n\")")
	}

	// Redeliver the redelivery: still the first GUID.
	list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	var second uuid.UUID
	for _, d := range list {
		if d.ID != delID {
			second = d.ID
		}
	}
	headers = e.redeliveredHeaders(t, id, second)
	if !strings.Contains(headers, "X-Softserve-Delivery: "+guid.String()+"\n") {
		t.Fatal("not true: strings.Contains(headers, \"X-Softserve-Delivery: \"+guid.String()+\"\\n\")")
	}
}

// A stored delivery without a usable GUID header is redelivered with its own
// id, the GUID it was first sent with.
func TestRedeliverWithoutStoredGUIDUsesDeliveryID(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)

	delID := e.seedDelivery(t, id, "http://127.0.0.1:1/", `{"ref":"main"}`)
	headers := e.redeliveredHeaders(t, id, delID)
	if !strings.Contains(headers, "X-Softserve-Delivery: "+delID.String()+"\n") {
		t.Fatal("not true: strings.Contains(headers, \"X-Softserve-Delivery: \"+delID.String()+\"\\n\")")
	}

	badID := uuid.New()
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.CreateWebhookDelivery(e.ctx, tx, models.WebhookDelivery{
			ID:             badID,
			WebhookID:      id,
			Event:          int(webhook.EventPush),
			RequestURL:     "http://127.0.0.1:1/",
			RequestMethod:  "POST",
			RequestHeaders: "X-Softserve-Delivery: not-a-uuid\n",
			RequestBody:    `{"ref":"main"}`,
			ResponseStatus: 200,
			ResponseBody:   "ok",
		})
	}); err != nil {
		t.Fatal(err)
	}
	headers = e.redeliveredHeaders(t, id, badID)
	if !strings.Contains(headers, "X-Softserve-Delivery: "+badID.String()+"\n") {
		t.Fatal("not true: strings.Contains(headers, \"X-Softserve-Delivery: \"+badID.String()+\"\\n\")")
	}
}

func TestStoredHeader(t *testing.T) {
	t.Parallel()
	headers := "Content-Type: application/json\nx-softserve-delivery:  abc \nX-SoftServe-Delivery: second\n"

	v, ok := storedHeader(headers, "X-SoftServe-Delivery")
	if !ok {
		t.Fatal("not true: ok")
	}
	if got := v; got != "abc" {
		t.Fatalf("v = %v, want %v", got, "abc")
	}

	_, ok = storedHeader(headers, "X-Missing")
	if ok {
		t.Fatal("not true: !ok")
	}
}

const (
	zeroSha = "0000000000000000000000000000000000000000"
	someSha = "1111111111111111111111111111111111111111"
)

// seedHook stores a webhook directly, so its delivery attempts go to a
// loopback address that the SSRF-protected client refuses. The refusal is
// recorded as a delivery, which lets tests see which events were sent.
func (e whEnv) seedHook(t *testing.T, events ...webhook.Event) int64 {
	t.Helper()
	return e.seedHookOn(t, e.repo, events...)
}

// seedHookOn is seedHook for a given repository.
func (e whEnv) seedHookOn(t *testing.T, repo proto.Repository, events ...webhook.Event) int64 {
	t.Helper()
	var id int64
	if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		var err error
		id, err = e.be.store.CreateWebhook(e.ctx, tx, models.Webhook{RepoID: repo.ID(), URL: "http://127.0.0.1:1/", ContentType: int(webhook.ContentTypeJSON), Active: true})
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

// deliveredEvents returns the events of every delivery recorded for a webhook.
func (e whEnv) deliveredEvents(t *testing.T, hookID int64) []webhook.Event {
	t.Helper()
	list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, hookID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]webhook.Event, 0, len(list))
	for _, d := range list {
		out = append(out, d.Event)
	}
	return out
}

// waitFor polls cond until it holds. Changes made through the backend send
// their events in the background, so tests wait for those sends to finish.
// The bound is well above the time one delivery may take to be recorded
// (the webhook package's recordTimeout), so a loaded machine does not fail it.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitDeliveries waits until the webhook has recorded exactly want. A test
// that makes a second change after one that records a delivery in the
// background waits here first: under a DELETE journal_mode data source the
// second change's reads wait out the record's commit, and one longer than
// busy_timeout fails them. A read that fails while a record commits is retried.
func (e whEnv) awaitDeliveries(t *testing.T, hookID int64, want ...webhook.Event) {
	t.Helper()
	waitFor(t, "deliveries", func() bool {
		list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, hookID)
		if err != nil {
			return false
		}
		got := make([]webhook.Event, 0, len(list))
		for _, d := range list {
			got = append(got, d.Event)
		}
		return slices.Equal(got, want)
	})
}

// awaitLog waits until the log contains text.
func (e whEnv) awaitLog(t *testing.T, text string) {
	t.Helper()
	waitFor(t, "log line "+text, func() bool {
		return strings.Contains(e.logs.String(), text)
	})
}

// WaitForDeliveries returns only after the background sends have finished, so
// the failed attempt is already logged when it returns, with no polling.
func TestWaitForDeliveriesCompletesBackgroundSends(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	e.seedHook(t, webhook.EventRepository)

	if err := e.be.DeleteRepository(e.ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	e.be.WaitForDeliveries(e.ctx)

	if !strings.Contains(e.logs.String(), "webhook event not delivered") {
		t.Fatal("not true: strings.Contains(e.logs.String(), \"webhook event not delivered\")")
	}
}

// A delivery that starts just before shutdown must be able to finish within
// the shutdown budget, so the budget must exceed the cap the SSRF-protected
// client puts on one request.
func TestShutdownDeliveryBudgetCoversSendTimeout(t *testing.T) {
	t.Parallel()
	if ShutdownDeliveryTimeout <= ssrf.ClientTimeout {
		t.Fatal("not true: ShutdownDeliveryTimeout > ssrf.ClientTimeout")
	}
}

// Once shutdown has started waiting, a change made by a request that is still
// running is committed, but its event is skipped rather than added to the
// deliveries being waited on.
func TestChangesAfterShutdownStartSkipDelivery(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := e.seedHook(t, webhook.EventCollaborator)

	e.be.WaitForDeliveries(e.ctx)
	if err := e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(e.logs.String(), "webhook event skipped: server is shutting down") {
		t.Fatal("not true: strings.Contains(e.logs.String(), \"webhook event skipped: server is shutting down\")")
	}
	if got := len(e.deliveredEvents(t, id)); got != 0 {
		t.Fatalf("len(e.deliveredEvents(t, id)) = %v, want %v", got, 0)
	}
}

// makeCommit writes one commit into the bare repository at path and points
// refs/heads/main at it, so push events have real commits to describe.
func makeCommit(t *testing.T, path string) string {
	t.Helper()
	git := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"--git-dir", path}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	blob := git("hello\n", "hash-object", "-w", "--stdin")
	tree := git("100644 blob "+blob+"\tREADME.md\n", "mktree")
	commit := git("", "commit-tree", tree, "-m", "first")
	git("", "update-ref", "refs/heads/main", commit)
	return commit
}

// corruptOwner removes the repository owner's row with foreign keys switched
// off, which is the only way to make event building fail. On SQLite the pool
// is pinned to one connection so the pragma applies to the statements that
// follow it; on Postgres the replica role, which skips foreign key checks,
// is set for the deleting transaction only (it needs a superuser).
func corruptOwner(t *testing.T, e whEnv) {
	t.Helper()
	const del = "DELETE FROM users WHERE username = 'admin'"
	if e.be.db.DriverName() == "postgres" {
		if err := e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
			if _, err := tx.ExecContext(e.ctx, "SET LOCAL session_replication_role = replica"); err != nil {
				return err
			}
			_, err := tx.ExecContext(e.ctx, del)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	e.be.db.SetMaxOpenConns(1)
	execAll(t, e.ctx, e.be.db, []string{"PRAGMA foreign_keys = OFF", del, "PRAGMA foreign_keys = ON"})
}

// blockDeliveries replaces the backend's delivery queue with one of one
// worker and maxPending queued deliveries, and occupies the worker until the
// returned function is called.
func blockDeliveries(t *testing.T, e whEnv, maxPending int) func() {
	t.Helper()
	e.be.deliveries = newDeliveryQueue(1, maxPending, e.be.logger)
	release := make(chan struct{})
	started := make(chan struct{})
	e.be.deliveries.enqueue(0, func() {
		close(started)
		<-release
	})
	<-started
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	return unblock
}

// When every sender is busy, the event waits in the queue: the caller returns
// at once, and the event is sent once a sender is free.
func TestDeliveryWithBusySendersIsQueuedNotRunInCaller(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventRepository)
	whs, err := e.be.store.GetWebhooksByRepoID(e.ctx, e.be.db, e.repo.ID())
	if err != nil {
		t.Fatal(err)
	}
	unblock := blockDeliveries(t, e, 10)

	const n = 3
	for i := 0; i < n; i++ {
		ev, err := e.be.webhooks.NewRepositoryEvent(e.ctx, e.admin, e.repo, webhook.RepositoryEventActionRename)
		if err != nil {
			t.Fatal(err)
		}
		e.be.deliverAfterChange(e.ctx, whs, ev, true)
	}
	if got := len(e.deliveredEvents(t, id)); got != 0 {
		t.Fatalf("len(e.deliveredEvents(t, id)) = %v, want %v", got, 0)
	} // queued, not run in the caller

	unblock()
	e.be.WaitForDeliveries(e.ctx)
	if got := len(e.deliveredEvents(t, id)); got != n {
		t.Fatalf("len(e.deliveredEvents(t, id)) = %v, want %v", got, n)
	}
}

// When the queue is full too, the event is dropped without blocking the
// caller. The drop is logged and recorded as a failed delivery.
func TestDeliveryWithFullQueueIsDroppedAndRecorded(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventRepository)
	whs, err := e.be.store.GetWebhooksByRepoID(e.ctx, e.be.db, e.repo.ID())
	if err != nil {
		t.Fatal(err)
	}
	blockDeliveries(t, e, 0)

	ev, err := e.be.webhooks.NewRepositoryEvent(e.ctx, e.admin, e.repo, webhook.RepositoryEventActionRename)
	if err != nil {
		t.Fatal(err)
	}
	e.be.deliverAfterChange(e.ctx, whs, ev, true)

	if !strings.Contains(e.logs.String(), "webhook event dropped: delivery queue is full") {
		t.Fatal("not true: strings.Contains(e.logs.String(), \"webhook event dropped: delivery queue is full\")")
	}
	list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(list); got != 1 {
		t.Fatalf("len(list) = %v, want %v", got, 1)
	}
	d, err := e.be.WebhookDelivery(e.ctx, e.repo, id, list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := d.RequestError.String, errDeliveryQueueFull.Error(); got != want {
		t.Fatalf("d.RequestError.String = %v, want %v", got, want)
	}
	if got := d.Event; got != webhook.EventRepository {
		t.Fatalf("d.Event = %v, want %v", got, webhook.EventRepository)
	}
}

// The queue never runs more than its workers at once, and runs every job.
func TestDeliveryQueueBoundsWorkers(t *testing.T) {
	t.Parallel()
	q := newDeliveryQueue(2, 100, log.New(io.Discard))
	var mu sync.Mutex
	running, peak, ran := 0, 0, 0
	// Every job runs until release, so the first ones are still running
	// when the rest are enqueued.
	release := make(chan struct{})
	for i := 0; i < 50; i++ {
		if got := q.enqueue(int64(i), func() {
			mu.Lock()
			running++
			peak = max(peak, running)
			mu.Unlock()
			<-release
			mu.Lock()
			running--
			ran++
			mu.Unlock()
		}); got != enqueued {
			t.Fatalf("enqueue of job %d = %v, want %v", i, got, enqueued)
		}
	}
	// enqueue starts a job's goroutine or queues it before it returns.
	q.mu.Lock()
	workers, queued := q.workers, q.queued
	q.mu.Unlock()
	if workers != 2 || queued != 48 {
		close(release)
		t.Fatalf("with 50 jobs running until released: %d workers and %d queued, want 2 and 48", workers, queued)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		mu.Lock()
		r := running
		mu.Unlock()
		if r == 2 {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("%d jobs running a minute after they were enqueued, want 2", r)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	_, finished := q.close()
	<-finished
	if got := ran; got != 50 {
		t.Fatalf("ran = %v, want %v", got, 50)
	}
	if got := peak; got != 2 {
		t.Fatalf("peak = %v, want %v", got, 2)
	}
	if got := q.enqueue(1, func() {}); got != enqueueClosed {
		t.Fatalf("q.enqueue(1, func() {}) = %v, want %v", got, enqueueClosed)
	}
}

// blocker returns a job that signals started and then waits for release.
func blocker(started chan<- struct{}, release <-chan struct{}) func() {
	return func() {
		started <- struct{}{}
		<-release
	}
}

// A repository never has more than its share of the senders running, so
// another repository's delivery starts at once even while the first one has
// a backlog of deliveries that do not return.
func TestDeliveryQueueCapsRunningPerRepo(t *testing.T) {
	t.Parallel()
	q := newDeliveryQueue(8, 100, log.New(io.Discard))
	q.maxRepoRunning = 2
	started := make(chan struct{}, 5)
	release := make(chan struct{})
	for i := 0; i < 5; i++ {
		if got := q.enqueue(1, blocker(started, release)); got != enqueued {
			t.Fatalf("q.enqueue(1, blocker(started, release)) = %v, want %v", got, enqueued)
		}
	}
	<-started
	<-started

	ran := make(chan struct{})
	if got := q.enqueue(2, func() { close(ran) }); got != enqueued {
		t.Fatalf("q.enqueue(2, func() { close(ran) }) = %v, want %v", got, enqueued)
	}
	<-ran

	q.mu.Lock()
	if got := q.repos[1].running; got != 2 {
		t.Fatalf("q.repos[1].running = %v, want %v", got, 2)
	}
	if got := len(q.repos[1].queued); got != 3 {
		t.Fatalf("len(q.repos[1].queued) = %v, want %v", got, 3)
	}
	q.mu.Unlock()
	if got := len(started); got != 0 {
		t.Fatalf("len(started) = %v, want %v", got, 0)
	}

	close(release)
	_, finished := q.close()
	<-finished
	if got := len(started); got != 3 {
		t.Fatalf("len(started) = %v, want %v", got, 3)
	} // the queued ones ran once a share was free
}

// One repository's deliveries are sent one at a time, in the order they were
// queued, even while senders are free: a later event must not reach the
// receiver before an earlier one, since each carries the state at its change.
func TestDeliveryQueueSendsRepoEventsInOrder(t *testing.T) {
	t.Parallel()
	q := newDeliveryQueue(8, 100, log.New(io.Discard))
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var mu sync.Mutex
	var order []int
	if got := q.enqueue(1, func() {
		started <- struct{}{}
		<-release
		mu.Lock()
		order = append(order, 1)
		mu.Unlock()
	}); got != enqueued {
		t.Fatalf("enqueue of repository 1 job 1 = %v, want %v", got, enqueued)
	}
	<-started
	for i := 2; i <= 4; i++ {
		if got := q.enqueue(1, func() {
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
		}); got != enqueued {
			t.Fatalf("enqueue of repository 1 job %d = %v, want %v", i, got, enqueued)
		}
	}

	q.mu.Lock()
	if got := q.repos[1].running; got != 1 {
		t.Fatalf("q.repos[1].running = %v, want %v", got, 1)
	}
	if got := len(q.repos[1].queued); got != 3 {
		t.Fatalf("len(q.repos[1].queued) = %v, want %v", got, 3)
	}
	q.mu.Unlock()

	close(release)
	_, finished := q.close()
	<-finished
	if got, want := order, ([]int{1, 2, 3, 4}); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// Once a repository has its cap of deliveries queued or running, its next
// one is refused, while other repositories can still queue theirs.
func TestDeliveryQueueCapsPendingPerRepo(t *testing.T) {
	t.Parallel()
	q := newDeliveryQueue(1, 100, log.New(io.Discard))
	q.maxRepo = 3
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	if got := q.enqueue(1, blocker(started, release)); got != enqueued {
		t.Fatalf("q.enqueue(1, blocker(started, release)) = %v, want %v", got, enqueued)
	}
	<-started

	if got := q.enqueue(1, func() {}); got != enqueued {
		t.Fatalf("q.enqueue(1, func() {}) = %v, want %v", got, enqueued)
	}
	if got := q.enqueue(1, func() {}); got != enqueued {
		t.Fatalf("q.enqueue(1, func() {}) = %v, want %v", got, enqueued)
	}
	if got := q.enqueue(1, func() {}); got != enqueueRepoFull {
		t.Fatalf("q.enqueue(1, func() {}) = %v, want %v", got, enqueueRepoFull)
	}
	ran := make(chan struct{})
	if got := q.enqueue(2, func() { close(ran) }); got != enqueued {
		t.Fatalf("q.enqueue(2, func() { close(ran) }) = %v, want %v", got, enqueued)
	}

	close(release)
	<-ran
	_, finished := q.close()
	<-finished
	if got := len(q.repos); got != 0 {
		t.Fatalf("len(q.repos) = %v, want %v", got, 0)
	}
}

// Queued deliveries are taken from the repositories in turn, so a repository
// with a long backlog does not hold back one that queued later.
func TestDeliveryQueueTakesReposInTurn(t *testing.T) {
	t.Parallel()
	q := newDeliveryQueue(1, 100, log.New(io.Discard))
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	if got := q.enqueue(0, blocker(started, release)); got != enqueued {
		t.Fatalf("q.enqueue(0, blocker(started, release)) = %v, want %v", got, enqueued)
	}
	<-started

	var order []int64 // written by the single worker only
	for _, repo := range []int64{1, 1, 1, 2} {
		if got := q.enqueue(repo, func() { order = append(order, repo) }); got != enqueued {
			t.Fatalf("q.enqueue(repo, func() { order = append(order, repo) }) = %v, want %v", got, enqueued)
		}
	}
	close(release)
	_, finished := q.close()
	<-finished
	if got, want := order, ([]int64{1, 2, 1, 1}); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// An event of a repository that already has its cap of deliveries pending is
// dropped without blocking the caller, logged, and recorded as a failed
// delivery so it can be redelivered. Another repository's event is still
// queued and sent.
func TestDeliveryForBusyRepoIsDroppedAndRecorded(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventRepository)
	whs, err := e.be.store.GetWebhooksByRepoID(e.ctx, e.be.db, e.repo.ID())
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.be.CreateRepository(e.ctx, "other", e.admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	otherID := e.seedHookOn(t, other, webhook.EventRepository)
	otherHooks, err := e.be.store.GetWebhooksByRepoID(e.ctx, e.be.db, other.ID())
	if err != nil {
		t.Fatal(err)
	}

	// The repository's one allowed delivery is running and does not return
	// until released.
	e.be.deliveries = newDeliveryQueue(4, 100, e.be.logger)
	e.be.deliveries.maxRepo = 1
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	if got := e.be.deliveries.enqueue(e.repo.ID(), blocker(started, release)); got != enqueued {
		t.Fatalf("e.be.deliveries.enqueue(e.repo.ID(), blocker(started, release)) = %v, want %v", got, enqueued)
	}
	<-started

	ev, err := e.be.webhooks.NewRepositoryEvent(e.ctx, e.admin, e.repo, webhook.RepositoryEventActionRename)
	if err != nil {
		t.Fatal(err)
	}
	e.be.deliverAfterChange(e.ctx, whs, ev, true)

	if !strings.Contains(e.logs.String(), "webhook event dropped: repository has too many pending deliveries") {
		t.Fatal("not true: strings.Contains(e.logs.String(), \"webhook event dropped: repository has too many pending deliveries\")")
	}
	list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(list); got != 1 {
		t.Fatalf("len(list) = %v, want %v", got, 1)
	}
	d, err := e.be.WebhookDelivery(e.ctx, e.repo, id, list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := d.RequestError.String, errRepoDeliveriesFull.Error(); got != want {
		t.Fatalf("d.RequestError.String = %v, want %v", got, want)
	}
	if got := d.Event; got != webhook.EventRepository {
		t.Fatalf("d.Event = %v, want %v", got, webhook.EventRepository)
	}

	otherEv, err := e.be.webhooks.NewRepositoryEvent(e.ctx, e.admin, other, webhook.RepositoryEventActionRename)
	if err != nil {
		t.Fatal(err)
	}
	e.be.deliverAfterChange(e.ctx, otherHooks, otherEv, true)

	unblock()
	e.be.WaitForDeliveries(e.ctx)
	list, err = e.be.ListWebhookDeliveries(e.ctx, other, otherID)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(list); got != 1 {
		t.Fatalf("len(list) = %v, want %v", got, 1)
	}
	d, err = e.be.WebhookDelivery(e.ctx, other, otherID, list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	// Sent, and refused by the SSRF guard, rather than dropped.
	if d.RequestError.String == errRepoDeliveriesFull.Error() {
		t.Fatal("not true: d.RequestError.String != errRepoDeliveriesFull.Error()")
	}
	if d.RequestError.String == errDeliveryQueueFull.Error() {
		t.Fatal("not true: d.RequestError.String != errDeliveryQueueFull.Error()")
	}
	if got := len(e.deliveredEvents(t, id)); got != 1 {
		t.Fatalf("len(e.deliveredEvents(t, id)) = %v, want %v", got, 1)
	} // the dropped one only
}

// A panicking delivery is logged and does not take the process down; the
// queue keeps running later deliveries.
func TestDeliveryPanicIsRecovered(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	if got := e.be.deliveries.enqueue(1, func() { panic("boom") }); got != enqueued {
		t.Fatalf("e.be.deliveries.enqueue(1, func() { panic(\"boom\") }) = %v, want %v", got, enqueued)
	}
	ran := make(chan struct{})
	if got := e.be.deliveries.enqueue(1, func() { close(ran) }); got != enqueued {
		t.Fatalf("e.be.deliveries.enqueue(1, func() { close(ran) }) = %v, want %v", got, enqueued)
	}
	<-ran
	e.be.WaitForDeliveries(e.ctx)
	if !strings.Contains(e.logs.String(), "webhook delivery panicked") {
		t.Fatal("not true: strings.Contains(e.logs.String(), \"webhook delivery panicked\")")
	}
}

// Shutdown logs how many deliveries it waits for. When its context ends first,
// as on a second signal, it returns at once and warns that some are left.
func TestWaitForDeliveriesLogsPendingAndStopsOnCancel(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	blockDeliveries(t, e, 1)
	if got := e.be.deliveries.enqueue(1, func() {}); got != enqueued {
		t.Fatalf("e.be.deliveries.enqueue(1, func() {}) = %v, want %v", got, enqueued)
	}

	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	// The deliveries stay blocked until the test ends, so WaitForDeliveries
	// returns only if it stops waiting when its context ends. The bound only
	// keeps a regression from hanging the test; a slow machine can't reach it.
	returned := make(chan struct{})
	go func() {
		e.be.WaitForDeliveries(ctx)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Minute):
		t.Fatal("WaitForDeliveries waited for blocked deliveries after its context ended")
	}

	logs := e.logs.String()
	if !strings.Contains(logs, "waiting for webhook deliveries") {
		t.Fatal("not true: strings.Contains(logs, \"waiting for webhook deliveries\")")
	}
	if !strings.Contains(logs, "pending=2") {
		t.Fatal("not true: strings.Contains(logs, \"pending=2\")")
	}
	if !strings.Contains(logs, "shutting down with webhook deliveries still running") {
		t.Fatal("not true: strings.Contains(logs, \"shutting down with webhook deliveries still running\")")
	}
}

// StopDeliveries closes the queue, so no new delivery starts, without
// waiting for the ones queued or running.
func TestStopDeliveriesClosesWithoutWaiting(t *testing.T) {
	e := newWebhookEnv(t)
	blockDeliveries(t, e, 1)
	if got := e.be.deliveries.enqueue(1, func() {}); got != enqueued {
		t.Fatalf("e.be.deliveries.enqueue(1, func() {}) = %v, want %v", got, enqueued)
	}

	// The deliveries stay blocked until the test ends, so StopDeliveries
	// returns only if it does not wait for them.
	returned := make(chan struct{})
	go func() {
		e.be.StopDeliveries()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Minute):
		t.Fatal("StopDeliveries waited for blocked deliveries")
	}
	if !e.be.deliveries.closedForTest() {
		t.Fatal("not true: e.be.deliveries.closedForTest()")
	}
	if got := e.be.deliveries.enqueue(2, func() {}); got != enqueueClosed {
		t.Fatalf("e.be.deliveries.enqueue(2, func() {}) = %v, want %v", got, enqueueClosed)
	}
}

// A change made without a user in the context, as by an initial admin key
// that has no user row, still sends its event, with an empty sender.
func TestEventsWithoutUserHaveEmptySender(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventRepository, webhook.EventRepositoryVisibilityChange)
	pub := newTestPublicKey(t)
	setAdminKeys(t, e.cfg, append(e.cfg.InitialAdminKeys, proto.MarshalAuthorizedKey(pub)))
	ctx := context.WithValue(proto.WithUserContext(e.ctx, nil), charmssh.ContextKeyPublicKey, pub)

	if err := e.be.SetPrivate(ctx, "repo", true); err != nil {
		t.Fatal(err)
	}
	e.awaitDeliveries(t, id, webhook.EventRepositoryVisibilityChange)
	if err := e.be.RenameRepository(ctx, "repo", "renamed"); err != nil {
		t.Fatal(err)
	}
	e.be.WaitForDeliveries(e.ctx)

	if got, want := sortedEvents(e.deliveredEvents(t, id)), sortedEvents([]webhook.Event{webhook.EventRepositoryVisibilityChange, webhook.EventRepository}); !reflect.DeepEqual(got, want) {
		t.Fatalf("sortedEvents(e.deliveredEvents(t, id)) = %v, want %v", got, want)
	}
	var bodies []string
	if err := e.be.db.SelectContext(e.ctx, &bodies, e.be.db.Rebind("SELECT request_body FROM webhook_deliveries WHERE webhook_id = ?"), id); err != nil {
		t.Fatal(err)
	}
	for _, b := range bodies {
		if !strings.Contains(b, `"sender":{"id":0,"username":""}`) {
			t.Fatal("not true: strings.Contains(b, `\"sender\":{\"id\":0,\"username\":\"\"}`)")
		}
	}
	if strings.Contains(e.logs.String(), "panicked") {
		t.Fatal("not true: !strings.Contains(e.logs.String(), \"panicked\")")
	}
}

// A send whose webhook is deleted before it records must not leave a delivery
// row behind for the removed webhook.
func TestDeliveryRecordedAfterWebhookDeletedWritesNoRow(t *testing.T) {
	t.Parallel()
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventRepository)
	whs, err := e.be.store.GetWebhooksByRepoID(e.ctx, e.be.db, e.repo.ID())
	if err != nil {
		t.Fatal(err)
	}
	ev, err := e.be.webhooks.NewRepositoryEvent(e.ctx, e.admin, e.repo, webhook.RepositoryEventActionRename)
	if err != nil {
		t.Fatal(err)
	}

	if err := e.be.DeleteWebhook(e.ctx, e.repo, id); err != nil {
		t.Fatal(err)
	}
	if err := e.be.webhooks.Deliver(e.ctx, whs, ev); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := e.be.db.GetContext(e.ctx, &n, e.be.db.Rebind("SELECT COUNT(*) FROM webhook_deliveries WHERE webhook_id = ?"), id); err != nil {
		t.Fatal(err)
	}
	if got := n; got != 0 {
		t.Fatalf("n = %v, want %v", got, 0)
	}
}

// closedForTest reports whether the queue refuses new deliveries, that is,
// whether shutdown has started.
func (q *deliveryQueue) closedForTest() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}

// A redelivery sends the body of the delivery it repeats byte for byte, in
// the content type it was sent with, for both content types.
func TestRedeliverSendsStoredBytes(t *testing.T) {
	t.Parallel()
	for _, ct := range []webhook.ContentType{webhook.ContentTypeJSON, webhook.ContentTypeForm} {
		t.Run(ct.String(), func(t *testing.T) {
			t.Parallel()
			e := newWebhookEnv(t)
			type request struct{ body, contentType string }
			var (
				mu   sync.Mutex
				reqs []request
			)
			srv := newMemServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read body: %v", err)
				}
				mu.Lock()
				reqs = append(reqs, request{string(b), r.Header.Get("Content-Type")})
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
			}))
			e.setWebhookAllowedHosts(t, "127.0.0.0/8")
			url := srv.URL + "/hook"
			if err := e.be.CreateWebhook(e.ctx, e.repo, WebhookInput{URL: url, ContentType: ct, Secret: "secret", Events: []webhook.Event{webhook.EventRepositoryVisibilityChange}, Active: true}); err != nil {
				t.Fatal(err)
			}
			hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(hooks); got != 1 {
				t.Fatalf("len(hooks) = %v, want %v", got, 1)
			}
			id := hooks[0].ID

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
			stored, err := e.be.WebhookDelivery(e.ctx, e.repo, id, ds[0].ID)
			if err != nil {
				t.Fatal(err)
			}

			if err := e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, ds[0].ID); err != nil {
				t.Fatal(err)
			}

			mu.Lock()
			defer mu.Unlock()
			if got := len(reqs); got != 2 {
				t.Fatalf("len(reqs) = %v, want %v", got, 2)
			}
			if got := reqs[0].body; got != stored.RequestBody {
				t.Fatalf("reqs[0].body = %v, want %v", got, stored.RequestBody)
			}
			if got, want := reqs[1], reqs[0]; !reflect.DeepEqual(got, want) {
				t.Fatalf("reqs[1] = %v, want %v", got, want)
			}
			if got, want := reqs[1].contentType, ct.String(); got != want {
				t.Fatalf("reqs[1].contentType = %v, want %v", got, want)
			}
		})
	}
}

// cancelAfterCommitStore cancels the caller's context on the first read made
// after one of the tracked changes, which is after its transaction has
// committed. It stands for a client that disconnects right after the change.
type cancelAfterCommitStore struct {
	store.Store

	mu     sync.Mutex
	armed  bool
	cancel context.CancelFunc
}

func (s *cancelAfterCommitStore) arm(err error) error {
	if err == nil {
		s.mu.Lock()
		s.armed = true
		s.mu.Unlock()
	}
	return err
}

func (s *cancelAfterCommitStore) fire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.armed && s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

func (s *cancelAfterCommitStore) SetRepoNameByName(ctx context.Context, h db.Handler, name, newName string) error {
	return s.arm(s.Store.SetRepoNameByName(ctx, h, name, newName))
}

func (s *cancelAfterCommitStore) SetRepoIsPrivateByName(ctx context.Context, h db.Handler, name string, private bool) error {
	return s.arm(s.Store.SetRepoIsPrivateByName(ctx, h, name, private))
}

func (s *cancelAfterCommitStore) AddCollabByUsernameAndRepo(ctx context.Context, h db.Handler, username, repo string, level access.Level) error {
	return s.arm(s.Store.AddCollabByUsernameAndRepo(ctx, h, username, repo, level))
}

func (s *cancelAfterCommitStore) RemoveCollabByUsernameAndRepo(ctx context.Context, h db.Handler, username, repo string) error {
	return s.arm(s.Store.RemoveCollabByUsernameAndRepo(ctx, h, username, repo))
}

func (s *cancelAfterCommitStore) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	s.fire()
	return s.Store.GetRepoByName(ctx, h, name)
}

func (s *cancelAfterCommitStore) GetUserByID(ctx context.Context, h db.Handler, id int64) (models.User, error) {
	s.fire()
	return s.Store.GetUserByID(ctx, h, id)
}

func (s *cancelAfterCommitStore) GetWebhooksByRepoIDWhereEvent(ctx context.Context, h db.Handler, repoID int64, events []int) ([]models.Webhook, error) {
	s.fire()
	return s.Store.GetWebhooksByRepoIDWhereEvent(ctx, h, repoID, events)
}

// A change's webhook event is sent even when the caller's context ends right
// after the change commits, such as an SSH client that disconnects as soon
// as the command has run.
func TestWebhookEventSentWhenContextEndsAfterCommit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		event  webhook.Event
		setup  func(t *testing.T, e whEnv)
		change func(ctx context.Context, e whEnv) error
	}{
		{
			name:  "rename",
			event: webhook.EventRepository,
			change: func(ctx context.Context, e whEnv) error {
				return e.be.RenameRepository(ctx, "repo", "renamed")
			},
		},
		{
			name:  "visibility",
			event: webhook.EventRepositoryVisibilityChange,
			change: func(ctx context.Context, e whEnv) error {
				return e.be.SetPrivate(ctx, "repo", true)
			},
		},
		{
			name:  "collaborator added",
			event: webhook.EventCollaborator,
			setup: func(t *testing.T, e whEnv) {
				t.Helper()
				_, err := e.be.CreateUser(e.ctx, "alice", proto.UserOptions{})
				if err != nil {
					t.Fatal(err)
				}
			},
			change: func(ctx context.Context, e whEnv) error {
				return e.be.AddCollaborator(ctx, "repo", "alice", access.ReadOnlyAccess)
			},
		},
		{
			name:  "collaborator removed",
			event: webhook.EventCollaborator,
			setup: func(t *testing.T, e whEnv) {
				t.Helper()
				_, err := e.be.CreateUser(e.ctx, "alice", proto.UserOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if err := e.be.AddCollaborator(e.ctx, "repo", "alice", access.ReadOnlyAccess); err != nil {
					t.Fatal(err)
				}
			},
			change: func(ctx context.Context, e whEnv) error {
				return e.be.RemoveCollaborator(ctx, "repo", "alice")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newWebhookEnv(t)
			if tc.setup != nil {
				tc.setup(t, e)
			}
			id := e.seedHook(t, tc.event)

			ctx, cancel := context.WithCancel(e.ctx)
			defer cancel()
			st := &cancelAfterCommitStore{Store: e.be.store, cancel: cancel}
			e.be.store = st
			e.be.webhooks = webhook.NewSender(e.cfg, e.be.db, st, e.be.logger)

			if err := tc.change(ctx, e); err != nil {
				t.Fatal(err)
			}
			if ctx.Err() == nil {
				t.Fatal("not true: ctx.Err() != nil")
			} // the context ended after the commit
			e.be.WaitForDeliveries(e.ctx)

			if got, want := e.deliveredEvents(t, id), ([]webhook.Event{tc.event}); !reflect.DeepEqual(got, want) {
				t.Fatalf("e.deliveredEvents(t, id) = %v, want %v", got, want)
			}
		})
	}
}

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

// raceDeleteWebhookStore runs race once, inside the repository delete's
// transaction: right after the delete lists the repository's webhooks, or,
// where the delete lists none, just before it deletes the row.
type raceDeleteWebhookStore struct {
	store.Store
	once sync.Once
	race func()
}

func (s *raceDeleteWebhookStore) GetWebhooksByRepoID(ctx context.Context, h db.Handler, repoID int64) ([]models.Webhook, error) {
	whs, err := s.Store.GetWebhooksByRepoID(ctx, h, repoID)
	if _, inTx := h.(*db.Tx); inTx {
		s.once.Do(s.race)
	}
	return whs, err
}

func (s *raceDeleteWebhookStore) DeleteRepoByName(ctx context.Context, h db.Handler, name string) error {
	s.once.Do(s.race)
	return s.Store.DeleteRepoByName(ctx, h, name)
}

// On Postgres, deleting a webhook while its repository is being deleted
// must not make the repository's delete fail, as it would if the webhook
// delete removed a webhook the repository's delete had already listed. The
// two take turns on the repository's row.
func TestDeleteWebhookRacingRepositoryDeletePostgres(t *testing.T) {
	t.Parallel()
	e := newPostgresWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)

	hookDeleted := make(chan error, 1)
	rs := &raceDeleteWebhookStore{Store: e.be.store}
	rs.race = func() {
		go func() { hookDeleted <- e.be.DeleteWebhook(e.ctx, e.repo, id) }()
		// Go on once the webhook delete has finished or waits for a lock.
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) {
			select {
			case err := <-hookDeleted:
				hookDeleted <- err
				return
			default:
			}
			var waiting bool
			if err := e.be.db.GetContext(e.ctx, &waiting, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock');`); err != nil {
				t.Error(err)
				return
			}
			if waiting {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("the webhook delete neither finished nor waited for a lock")
	}
	e.be.store = rs

	if err := e.be.DeleteRepository(e.ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	_, err := e.be.Repository(e.ctx, "repo")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
	// The webhook went with its repository; deleting it found nothing.
	if !errors.Is(<-hookDeleted, proto.ErrWebhookNotFound) {
		t.Fatal("not true: errors.Is(<-hookDeleted, proto.ErrWebhookNotFound)")
	}
}

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
