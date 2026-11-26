package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	charmlog "charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
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
	prev := charmlog.Default()
	charmlog.SetDefault(charmlog.New(e.logs))
	t.Cleanup(func() { charmlog.SetDefault(prev) })
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
