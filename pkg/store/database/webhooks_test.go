package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/internal/dbtest"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/google/uuid"
)

// newWebhookTestDB opens a migrated database for one test: Postgres when
// SOFT_SERVE_DB_DRIVER is "postgres", SQLite otherwise.
func newWebhookTestDB(t *testing.T) (context.Context, *db.DB) {
	t.Helper()
	ctx := context.Background()
	dbx, err := dbtest.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.DB.Driver = dbx.DriverName()
	if err := migrate.Migrate(ctx, dbx, cfg, nil); err != nil {
		t.Fatal(err)
	}

	// Foreign keys are enforced, so the webhook tables need real parents: one
	// user and the repos 7 and 8 that the tests use.
	_, err = dbx.ExecContext(ctx, `INSERT INTO users (id, username, admin, updated_at) VALUES (100, 'owner', false, CURRENT_TIMESTAMP);`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = dbx.ExecContext(ctx, `INSERT INTO repos (id, name, project_name, description, private, mirror, hidden, user_id, updated_at)
		VALUES (7, 'repo7', 'repo7', '', false, false, false, 100, CURRENT_TIMESTAMP),
		       (8, 'repo8', 'repo8', '', false, false, false, 100, CURRENT_TIMESTAMP);`)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, dbx
}

func isSqlite(dbx *db.DB) bool {
	d := dbx.DriverName()
	return d == "sqlite"
}

func TestWebhookStoreRoundTrip(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}

	id, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://93.184.216.34/a", Secret: "sec", ContentType: 1, Active: true})
	if err != nil {
		t.Fatal(err)
	}

	wh, err := s.GetWebhookByID(ctx, dbx, 7, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := wh.URL; got != "https://93.184.216.34/a" {
		t.Fatalf("wh.URL = %v, want %v", got, "https://93.184.216.34/a")
	}
	if got := wh.Secret; got != "sec" {
		t.Fatalf("wh.Secret = %v, want %v", got, "sec")
	}
	if got := wh.ContentType; got != 1 {
		t.Fatalf("wh.ContentType = %v, want %v", got, 1)
	}
	if !wh.Active {
		t.Fatal("not true: wh.Active")
	}
	if got, want := wh.RepoID, int64(7); got != want {
		t.Fatalf("wh.RepoID = %v, want %v", got, want)
	}

	// Scoped by repo: the same ID under another repo is not found.
	_, err = s.GetWebhookByID(ctx, dbx, 8, id)
	if !errors.Is(err, db.ErrRecordNotFound) {
		t.Fatal("not true: errors.Is(err, db.ErrRecordNotFound)")
	}

	if err := s.UpdateWebhookByID(ctx, dbx, models.Webhook{ID: id, RepoID: 7, URL: "https://93.184.216.35/b", Secret: "new", ContentType: 0}); err != nil {
		t.Fatal(err)
	}
	wh, err = s.GetWebhookByID(ctx, dbx, 7, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := wh.URL; got != "https://93.184.216.35/b" {
		t.Fatalf("wh.URL = %v, want %v", got, "https://93.184.216.35/b")
	}
	if got := wh.Secret; got != "new" {
		t.Fatalf("wh.Secret = %v, want %v", got, "new")
	}
	if got := wh.ContentType; got != 0 {
		t.Fatalf("wh.ContentType = %v, want %v", got, 0)
	}
	if wh.Active {
		t.Fatal("not true: !wh.Active")
	}

	// An update scoped to another repo changes nothing and reports the miss.
	if !errors.Is(s.UpdateWebhookByID(ctx, dbx, models.Webhook{ID: id, RepoID: 8, URL: "https://93.184.216.36/c", Secret: "x", ContentType: 0, Active: true}), db.ErrRecordNotFound) {
		t.Fatal("not true: errors.Is(s.UpdateWebhookByID(ctx, dbx, models.Webhook{ID: id, RepoID: 8, URL: \"https://93.184.216.36/c\", Secret: \"x\", ContentType: 0, Active: true}), db.ErrRecordNotFound)")
	}
	wh, err = s.GetWebhookByID(ctx, dbx, 7, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := wh.URL; got != "https://93.184.216.35/b" {
		t.Fatalf("wh.URL = %v, want %v", got, "https://93.184.216.35/b")
	}

	list, err := s.GetWebhooksByRepoID(ctx, dbx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(list); got != 1 {
		t.Fatalf("len(list) = %v, want %v", got, 1)
	}
	none, err := s.GetWebhooksByRepoID(ctx, dbx, 99)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(none); got != 0 {
		t.Fatalf("len(none) = %v, want %v", got, 0)
	}

	if !errors.Is(s.DeleteWebhookForRepoByID(ctx, dbx, 8, id), db.ErrRecordNotFound) {
		t.Fatal("not true: errors.Is(s.DeleteWebhookForRepoByID(ctx, dbx, 8, id), db.ErrRecordNotFound)")
	} // wrong repo: nothing deleted
	_, err = s.GetWebhookByID(ctx, dbx, 7, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWebhookForRepoByID(ctx, dbx, 7, id); err != nil {
		t.Fatal(err)
	}
	_, err = s.GetWebhookByID(ctx, dbx, 7, id)
	if !errors.Is(err, db.ErrRecordNotFound) {
		t.Fatal("not true: errors.Is(err, db.ErrRecordNotFound)")
	}
}

func TestWebhookStoreEvents(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}

	hook, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://93.184.216.34/a", ContentType: 0, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://93.184.216.35/b", ContentType: 0, Active: true})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.CreateWebhookEvents(ctx, dbx, hook, []int{4, 5}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWebhookEvents(ctx, dbx, other, []int{6}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWebhookEvents(ctx, dbx, hook, nil); err != nil {
		t.Fatal(err)
	} // nothing to add

	evs, err := s.GetWebhookEventsByWebhookID(ctx, dbx, hook)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(evs); got != 2 {
		t.Fatalf("len(evs) = %v, want %v", got, 2)
	}

	if got := evs[0].WebhookID; got != hook {
		t.Fatalf("evs[0].WebhookID = %v, want %v", got, hook)
	}

	// Filter by event: push (4) matches hook only; an event with no match
	// returns nothing; an empty filter returns nothing and no error.
	whs, err := s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, []int{4})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(whs); got != 1 {
		t.Fatalf("len(whs) = %v, want %v", got, 1)
	}
	if got := whs[0].ID; got != hook {
		t.Fatalf("whs[0].ID = %v, want %v", got, hook)
	}

	// An inactive webhook subscribed to the same event is left out.
	inactive, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://93.184.216.36/c", ContentType: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWebhookEvents(ctx, dbx, inactive, []int{4}); err != nil {
		t.Fatal(err)
	}
	whs, err = s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, []int{4})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(whs); got != 1 {
		t.Fatalf("len(whs) = %v, want %v", got, 1)
	}
	if got := whs[0].ID; got != hook {
		t.Fatalf("whs[0].ID = %v, want %v", got, hook)
	}

	// The single-webhook lookup applies the same filters: the webhook must
	// belong to the repository, be active and subscribe to the event.
	wh, err := s.GetActiveWebhookByIDWhereEvent(ctx, dbx, 7, hook, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := wh.ID; got != hook {
		t.Fatalf("wh.ID = %v, want %v", got, hook)
	}
	if got := wh.URL; got != "https://93.184.216.34/a" {
		t.Fatalf("wh.URL = %v, want %v", got, "https://93.184.216.34/a")
	}
	for name, args := range map[string]struct {
		repoID, id int64
		event      int
	}{
		"inactive":       {7, inactive, 4},
		"not subscribed": {7, hook, 6},
		"other repo":     {8, hook, 4},
		"missing":        {7, 424242, 4},
	} {
		_, err = s.GetActiveWebhookByIDWhereEvent(ctx, dbx, args.repoID, args.id, args.event)
		if !errors.Is(err, db.ErrRecordNotFound) {
			t.Errorf("%s: GetActiveWebhookByIDWhereEvent error = %v, want db.ErrRecordNotFound", name, err)
		}
	}

	whs, err = s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, []int{99})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(whs); got != 0 {
		t.Fatalf("len(whs) = %v, want %v", got, 0)
	}

	whs, err = s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(whs); got != 0 {
		t.Fatalf("len(whs) = %v, want %v", got, 0)
	}

	// Deleting no IDs is a no-op; deleting some removes only those.
	if err := s.DeleteWebhookEventsByID(ctx, dbx, hook, nil); err != nil {
		t.Fatal(err)
	}
	evs, err = s.GetWebhookEventsByWebhookID(ctx, dbx, hook)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(evs); got != 2 {
		t.Fatalf("len(evs) = %v, want %v", got, 2)
	}

	// The deletion is scoped to the webhook: another webhook's ID leaves
	// these events alone.
	if err := s.DeleteWebhookEventsByID(ctx, dbx, other, []int64{evs[0].ID, evs[1].ID}); err != nil {
		t.Fatal(err)
	}
	evs, err = s.GetWebhookEventsByWebhookID(ctx, dbx, hook)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(evs); got != 2 {
		t.Fatalf("len(evs) = %v, want %v", got, 2)
	}
	otherEvs, err := s.GetWebhookEventsByWebhookID(ctx, dbx, other)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(otherEvs); got != 1 {
		t.Fatalf("len(otherEvs) = %v, want %v", got, 1)
	}
	if err := s.DeleteWebhookEventsByID(ctx, dbx, hook, []int64{otherEvs[0].ID}); err != nil {
		t.Fatal(err)
	}
	otherEvs, err = s.GetWebhookEventsByWebhookID(ctx, dbx, other)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(otherEvs); got != 1 {
		t.Fatalf("len(otherEvs) = %v, want %v", got, 1)
	}

	if err := s.DeleteWebhookEventsByID(ctx, dbx, hook, []int64{evs[0].ID}); err != nil {
		t.Fatal(err)
	}
	evs, err = s.GetWebhookEventsByWebhookID(ctx, dbx, hook)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(evs); got != 1 {
		t.Fatalf("len(evs) = %v, want %v", got, 1)
	}

	// Deleting a webhook deletes its events; the other webhook's stay.
	if err := s.DeleteWebhookForRepoByID(ctx, dbx, 7, hook); err != nil {
		t.Fatal(err)
	}
	evs, err = s.GetWebhookEventsByWebhookID(ctx, dbx, hook)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(evs); got != 0 {
		t.Fatalf("len(evs) = %v, want %v", got, 0)
	}
	evs, err = s.GetWebhookEventsByWebhookID(ctx, dbx, other)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(evs); got != 1 {
		t.Fatalf("len(evs) = %v, want %v", got, 1)
	}
}

func TestWebhookStoreDeliveries(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}

	hook, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://93.184.216.34/a", ContentType: 0, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://93.184.216.35/b", ContentType: 0, Active: true})
	if err != nil {
		t.Fatal(err)
	}

	okID := uuid.New()
	failID := uuid.New()
	if err := s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
		ID:              okID,
		WebhookID:       hook,
		Event:           4,
		RequestURL:      "https://93.184.216.34/a",
		RequestMethod:   "POST",
		RequestHeaders:  "H: 1",
		RequestBody:     `{"a":1}`,
		ResponseStatus:  200,
		ResponseHeaders: "R: 1",
		ResponseBody:    "ok",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
		ID:            failID,
		WebhookID:     hook,
		Event:         5,
		RequestURL:    "https://93.184.216.34/a",
		RequestMethod: "POST",
		RequestError:  sql.NullString{String: "dial refused", Valid: true},
		RequestBody:   "{}",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
		ID:             uuid.New(),
		WebhookID:      other,
		Event:          6,
		RequestURL:     "https://93.184.216.35/b",
		RequestMethod:  "POST",
		RequestBody:    "{}",
		ResponseStatus: 204,
	}); err != nil {
		t.Fatal(err)
	}

	d, err := s.GetWebhookDeliveryByID(ctx, dbx, hook, okID)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.RequestBody; got != `{"a":1}` {
		t.Fatalf("d.RequestBody = %v, want %v", got, `{"a":1}`)
	}
	if got := d.Event; got != 4 {
		t.Fatalf("d.Event = %v, want %v", got, 4)
	}
	if d.RequestError.Valid {
		t.Fatal("not true: !d.RequestError.Valid")
	} // no error recorded

	f, err := s.GetWebhookDeliveryByID(ctx, dbx, hook, failID)
	if err != nil {
		t.Fatal(err)
	}
	if !f.RequestError.Valid {
		t.Fatal("not true: f.RequestError.Valid")
	}
	if got := f.RequestError.String; got != "dial refused" {
		t.Fatalf("f.RequestError.String = %v, want %v", got, "dial refused")
	}

	// Scoped by webhook: another webhook's delivery ID is not found.
	_, err = s.GetWebhookDeliveryByID(ctx, dbx, other, okID)
	if !errors.Is(err, db.ErrRecordNotFound) {
		t.Fatal("not true: errors.Is(err, db.ErrRecordNotFound)")
	}

	all, err := allDeliveries(ctx, dbx, hook)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(all); got != 2 {
		t.Fatalf("len(all) = %v, want %v", got, 2)
	}

	list, err := s.ListWebhookDeliveriesByWebhookID(ctx, dbx, hook)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(list); got != 2 {
		t.Fatalf("len(list) = %v, want %v", got, 2)
	}

	// Deleting a webhook deletes its deliveries; the other webhook's stay.
	if err := s.DeleteWebhookForRepoByID(ctx, dbx, 7, hook); err != nil {
		t.Fatal(err)
	}
	all, err = allDeliveries(ctx, dbx, hook)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(all); got != 0 {
		t.Fatalf("len(all) = %v, want %v", got, 0)
	}
	all, err = allDeliveries(ctx, dbx, other)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(all); got != 1 {
		t.Fatalf("len(all) = %v, want %v", got, 1)
	}
}

// deleteWebhookDelivery deletes one delivery row directly; the store has no
// method for it.
func deleteWebhookDelivery(t *testing.T, ctx context.Context, dbx *db.DB, id uuid.UUID) {
	t.Helper()
	if _, err := dbx.ExecContext(ctx, dbx.Rebind(`DELETE FROM webhook_deliveries WHERE id = ?;`), id); err != nil {
		t.Fatal(err)
	}
}

// Once the database is closed every store call must return an error rather
// than succeed silently. This covers the error return of each method.
func TestWebhookStoreErrorsOnClosedDB(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	id, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://93.184.216.34/a", ContentType: 0, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	did := uuid.New()
	if err := s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
		ID:            did,
		WebhookID:     id,
		Event:         4,
		RequestURL:    "u",
		RequestMethod: "POST",
	}); err != nil {
		t.Fatal(err)
	}
	if err := dbx.Close(); err != nil {
		t.Fatal(err)
	}

	checks := map[string]error{}
	_, checks["GetWebhookByID"] = s.GetWebhookByID(ctx, dbx, 7, id)
	_, checks["GetWebhooksByRepoID"] = s.GetWebhooksByRepoID(ctx, dbx, 7)
	_, checks["LockAndCountWebhooks"] = s.LockAndCountWebhooks(ctx, dbx, 7)
	_, checks["GetWebhooksByRepoIDWhereEvent"] = s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, []int{4})
	_, checks["GetActiveWebhookByIDWhereEvent"] = s.GetActiveWebhookByIDWhereEvent(ctx, dbx, 7, id, 4)
	_, checks["CreateWebhook"] = s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "u", ContentType: 0, Active: true})
	checks["UpdateWebhookByID"] = s.UpdateWebhookByID(ctx, dbx, models.Webhook{ID: id, RepoID: 7, URL: "u", ContentType: 0, Active: true})
	checks["DeleteWebhookForRepoByID"] = s.DeleteWebhookForRepoByID(ctx, dbx, 7, id)
	_, checks["GetWebhookEventsByWebhookID"] = s.GetWebhookEventsByWebhookID(ctx, dbx, id)
	checks["CreateWebhookEvents"] = s.CreateWebhookEvents(ctx, dbx, id, []int{4})
	checks["DeleteWebhookEventsByID"] = s.DeleteWebhookEventsByID(ctx, dbx, id, []int64{1})
	_, checks["GetWebhookDeliveryByID"] = s.GetWebhookDeliveryByID(ctx, dbx, id, did)
	_, checks["ListWebhookDeliveriesByWebhookID"] = s.ListWebhookDeliveriesByWebhookID(ctx, dbx, id)
	checks["CreateWebhookDelivery"] = s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
		ID:            uuid.New(),
		WebhookID:     id,
		Event:         4,
		RequestURL:    "u",
		RequestMethod: "POST",
	})
	checks["PruneWebhookDeliveries"] = s.PruneWebhookDeliveries(ctx, dbx, id, 1)

	for name, err := range checks {
		if err == nil {
			t.Errorf("%s: no error on closed database", name)
		}
	}
}

// A webhook deleted after the existence check but before the insert makes the
// insert violate the foreign key. That is the same outcome as a webhook that
// is already gone: nothing is written, and the error matches
// db.ErrForeignKey. A trigger deletes the webhook in that window.
func TestCreateWebhookDeliveryForeignKeyViolation(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	id, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://93.184.216.34/a", ContentType: 1, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	vanishOnInsert(t, ctx, dbx, id)

	err = s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
		ID:             uuid.New(),
		WebhookID:      id,
		Event:          1,
		RequestURL:     "https://93.184.216.34/a",
		RequestMethod:  "POST",
		RequestBody:    "{}",
		ResponseStatus: 200,
	})
	if !errors.Is(err, db.ErrForeignKey) {
		t.Fatal("not true: errors.Is(err, db.ErrForeignKey)")
	}

	var n int
	if err := dbx.GetContext(ctx, &n, "SELECT COUNT(*) FROM webhook_deliveries"); err != nil {
		t.Fatal(err)
	}
	if got := n; got != 0 {
		t.Fatalf("n = %v, want %v", got, 0)
	}
}

// Both drivers' forms of a foreign key violation map to db.ErrForeignKey,
// which CreateWebhookDelivery tests for, and nothing else does.
func TestForeignKeyViolationIsMapped(t *testing.T) {
	if !errors.Is(fmt.Errorf("wrapped: %w", db.ErrForeignKey), db.ErrForeignKey) {
		t.Fatal("not true: errors.Is(fmt.Errorf(\"wrapped: %w\", db.ErrForeignKey), db.ErrForeignKey)")
	}
	if errors.Is(db.ErrDuplicateKey, db.ErrForeignKey) {
		t.Fatal("not true: !errors.Is(db.ErrDuplicateKey, db.ErrForeignKey)")
	}
	if errors.Is(errors.New("FOREIGN KEY constraint failed"), db.ErrForeignKey) {
		t.Fatal("not true: !errors.Is(errors.New(\"FOREIGN KEY constraint failed\"), db.ErrForeignKey)")
	}
	if errors.Is(nil, db.ErrForeignKey) {
		t.Fatal("not true: !errors.Is(nil, db.ErrForeignKey)")
	}

	// A real foreign key violation from the driver under test.
	ctx, dbx := newWebhookTestDB(t)
	_, err := dbx.ExecContext(ctx, `INSERT INTO webhook_events (webhook_id, event) VALUES (424242, 1)`)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if !errors.Is(err, db.ErrForeignKey) {
		t.Fatal("not true: errors.Is(err, db.ErrForeignKey)")
	}
}

// Webhooks and their events are listed in creation order. Without ORDER BY,
// SQLite returns webhooks in URL order through the unique (repo_id, url)
// index, so "n=10" came before "n=9".
func TestWebhookListsAreOrderedByID(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	var ids []int64
	for _, n := range []int{9, 10, 1} {
		id, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: fmt.Sprintf("https://93.184.216.34/?n=%d", n), ContentType: 1, Active: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CreateWebhookEvents(ctx, dbx, id, []int{3, 1, 2}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	list, err := s.GetWebhooksByRepoID(ctx, dbx, 7)
	if err != nil {
		t.Fatal(err)
	}
	byEvent, err := s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range [][]int64{idsOf(list), idsOf(byEvent)} {
		if got, want := got, ids; !reflect.DeepEqual(got, want) {
			t.Fatalf("got = %v, want %v", got, want)
		}
	}

	events, err := s.GetWebhookEventsByWebhookID(ctx, dbx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(events); i++ {
		if events[i-1].ID >= events[i].ID {
			t.Fatal("not true: events[i-1].ID < events[i].ID")
		}
	}
}

// Deliveries recorded within the same second list in the order they were
// inserted, oldest first. created_at has one-second resolution and IDs are
// random UUIDs, so neither can order them.
func TestWebhookStoreDeliveriesOrder(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}

	hook, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://93.184.216.34/a", ContentType: 0, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://93.184.216.35/b", ContentType: 0, Active: true})
	if err != nil {
		t.Fatal(err)
	}

	insert := func(webhookID int64) uuid.UUID {
		id := uuid.New()
		if err := s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
			ID:             id,
			WebhookID:      webhookID,
			Event:          1,
			RequestURL:     "https://93.184.216.34/a",
			RequestMethod:  "POST",
			RequestBody:    "{}",
			ResponseStatus: 200,
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}

	var want []uuid.UUID
	for range 20 {
		want = append(want, insert(hook))
		insert(other) // interleaved deliveries of another webhook
	}
	// Pin every row to the same second.
	_, err = dbx.ExecContext(ctx, `UPDATE webhook_deliveries SET created_at = '2024-01-01 00:00:00';`)
	if err != nil {
		t.Fatal(err)
	}

	check := func() {
		t.Helper()
		list, err := s.ListWebhookDeliveriesByWebhookID(ctx, dbx, hook)
		if err != nil {
			t.Fatal(err)
		}
		all, err := allDeliveries(ctx, dbx, hook)
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range [][]models.WebhookDelivery{list, all} {
			ids := make([]uuid.UUID, len(got))
			for i, d := range got {
				ids[i] = d.ID
			}
			if got, want := ids, want; !reflect.DeepEqual(got, want) {
				t.Fatalf("ids = %v, want %v", got, want)
			}
		}
	}
	check()

	// Deleting the newest delivery does not let a later one sort before
	// older ones.
	deleteWebhookDelivery(t, ctx, dbx, want[len(want)-1])
	want = want[:len(want)-1]
	want = append(want, insert(hook))
	check()

	// The listing follows seq, not the physical row order: a row stored last
	// with the lowest seq lists first.
	first := uuid.New()
	_, err = dbx.ExecContext(ctx, dbx.Rebind(`INSERT INTO webhook_deliveries (id, webhook_id, event, request_url, request_method, request_headers, request_body, response_status, response_headers, response_body, seq)
		VALUES (?, ?, 1, '', 'POST', '', '{}', 200, '', '', 0);`), first, hook)
	if err != nil {
		t.Fatal(err)
	}
	want = append([]uuid.UUID{first}, want...)
	check()
}

// The delivery listing is bounded: past maxListedWebhookDeliveries rows,
// as when pruning has fallen behind, it returns the most recent ones,
// oldest first, and leaves the older ones out.
func TestWebhookDeliveryListingsAreLimited(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	hook, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://93.184.216.34/a", ContentType: 1, Active: true})
	if err != nil {
		t.Fatal(err)
	}

	var ids []uuid.UUID
	for range maxListedWebhookDeliveries + 5 {
		id := uuid.New()
		if err := s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
			ID:             id,
			WebhookID:      hook,
			Event:          1,
			RequestURL:     "u",
			RequestMethod:  "POST",
			RequestBody:    "{}",
			ResponseStatus: 200,
		}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	want := ids[5:]

	list, err := s.ListWebhookDeliveriesByWebhookID(ctx, dbx, hook)
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := make([]uuid.UUID, len(list))
	for i, d := range list {
		gotIDs[i] = d.ID
	}
	if got, want := gotIDs, want; !reflect.DeepEqual(got, want) {
		t.Fatalf("gotIDs = %v, want %v", got, want)
	}
}

// Pruning keeps the most recently recorded deliveries of one webhook and
// leaves the deliveries of other webhooks alone. Recording order is what
// counts: created_at and the IDs are set to run the other way.
func TestPruneWebhookDeliveriesKeepsNewest(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	hook, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://93.184.216.34/a", ContentType: 1, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://93.184.216.35/b", ContentType: 1, Active: true})
	if err != nil {
		t.Fatal(err)
	}

	// Eight deliveries, recorded with descending IDs and created_at, and
	// interleaved with deliveries of another webhook.
	ids := make([]uuid.UUID, 8)
	for i := range ids {
		ids[i] = uuid.MustParse(fmt.Sprintf("%08d-0000-1000-8000-000000000000", 99-i))
		if err := s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
			ID:             ids[i],
			WebhookID:      hook,
			Event:          1,
			RequestURL:     "u",
			RequestMethod:  "POST",
			RequestBody:    "{}",
			ResponseStatus: 200,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := dbx.ExecContext(ctx, dbx.Rebind(`UPDATE webhook_deliveries SET created_at = ? WHERE id = ?;`),
			fmt.Sprintf("2024-01-01 00:00:%02d", 59-i), ids[i])
		if err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			if err := s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
				ID:             uuid.New(),
				WebhookID:      other,
				Event:          1,
				RequestURL:     "u",
				RequestMethod:  "POST",
				RequestBody:    "{}",
				ResponseStatus: 200,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	keptIDs := func() []uuid.UUID {
		t.Helper()
		all, err := allDeliveries(ctx, dbx, hook)
		if err != nil {
			t.Fatal(err)
		}
		out := []uuid.UUID{}
		for _, d := range all {
			out = append(out, d.ID)
		}
		return out
	}

	if err := s.PruneWebhookDeliveries(ctx, dbx, hook, 5); err != nil {
		t.Fatal(err)
	}
	if got, want := keptIDs(), ids[3:]; !reflect.DeepEqual(got, want) {
		t.Fatalf("keptIDs() = %v, want %v", got, want)
	} // the three recorded first are gone

	others, err := allDeliveries(ctx, dbx, other)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(others); got != 3 {
		t.Fatalf("len(others) = %v, want %v", got, 3)
	}

	// Exactly as many deliveries as the limit, or fewer: nothing is deleted.
	if err := s.PruneWebhookDeliveries(ctx, dbx, hook, 5); err != nil {
		t.Fatal(err)
	}
	if got, want := keptIDs(), ids[3:]; !reflect.DeepEqual(got, want) {
		t.Fatalf("keptIDs() = %v, want %v", got, want)
	}
	if err := s.PruneWebhookDeliveries(ctx, dbx, hook, 100); err != nil {
		t.Fatal(err)
	}
	if got, want := keptIDs(), ids[3:]; !reflect.DeepEqual(got, want) {
		t.Fatalf("keptIDs() = %v, want %v", got, want)
	}

	if err := s.PruneWebhookDeliveries(ctx, dbx, hook, 0); err != nil {
		t.Fatal(err)
	}
	if got, want := keptIDs(), ([]uuid.UUID{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("keptIDs() = %v, want %v", got, want)
	}
	others, err = allDeliveries(ctx, dbx, other)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(others); got != 3 {
		t.Fatalf("len(others) = %v, want %v", got, 3)
	}
}

// The prune runs on every recorded delivery, so it must not scan the
// deliveries table. On SQLite both its lookups use the (webhook_id, seq)
// index. Postgres picks its plan from table statistics, which a small test
// table does not represent, so its plan is not asserted.
func TestPruneWebhookDeliveriesUsesIndex(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	if !isSqlite(dbx) {
		t.Skip("checks an SQLite query plan")
	}
	var plan []struct {
		ID      int    `db:"id"`
		Parent  int    `db:"parent"`
		NotUsed int    `db:"notused"`
		Detail  string `db:"detail"`
	}
	if err := dbx.SelectContext(ctx, &plan, "EXPLAIN QUERY PLAN "+pruneWebhookDeliveriesQuery, 1, 1, 100); err != nil {
		t.Fatal(err)
	}
	if len(plan) <= 0 {
		t.Fatal("not true: len(plan) > 0")
	}
	for _, p := range plan {
		t.Log(p.Detail)
		if strings.HasPrefix(p.Detail, "SCAN") {
			t.Fatal("not true: !strings.HasPrefix(p.Detail, \"SCAN\")")
		} // no full scan
		if strings.Contains(p.Detail, "TEMP B-TREE") {
			t.Fatal("not true: !strings.Contains(p.Detail, \"TEMP B-TREE\")")
		} // no sort
	}
}

func idsOf(whs []models.Webhook) []int64 {
	out := make([]int64, len(whs))
	for i, w := range whs {
		out[i] = w.ID
	}
	return out
}

// LockAndCountWebhooks counts only the given repository's webhooks.
func TestLockAndCountWebhooks(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	n, err := s.LockAndCountWebhooks(ctx, dbx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got := n; got != 0 {
		t.Fatalf("n = %v, want %v", got, 0)
	}
	for i := range 3 {
		_, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: fmt.Sprintf("https://93.184.216.34/%d", i), ContentType: 1, Active: true})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 8, URL: "https://93.184.216.35/x", ContentType: 1, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	n, err = s.LockAndCountWebhooks(ctx, dbx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got := n; got != 3 {
		t.Fatalf("n = %v, want %v", got, 3)
	}
}

// allDeliveries returns every delivery recorded for webhookID, oldest first.
func allDeliveries(ctx context.Context, dbx *db.DB, webhookID int64) ([]models.WebhookDelivery, error) {
	var ds []models.WebhookDelivery
	err := dbx.SelectContext(ctx, &ds, dbx.Rebind(`SELECT * FROM webhook_deliveries WHERE webhook_id = ? ORDER BY seq;`), webhookID)
	return ds, err
}

// The webhook cap holds when the database defaults to REPEATABLE READ. A
// create that waited for LockAndCountWebhooks' row lock must count the
// webhook the holder inserted; under REPEATABLE READ its snapshot predates
// the wait, so it counted one fewer and a second create passed the cap.
func TestLockAndCountWebhooksSeesHolderUnderRepeatableReadPostgres(t *testing.T) {
	if os.Getenv("SOFT_SERVE_DB_DRIVER") != "postgres" {
		t.Skip("set SOFT_SERVE_DB_DRIVER=postgres to run")
	}
	ctx, dbx := newWebhookTestDB(t)
	dbtest.SetDefaultIsolation(ctx, t, dbx, "repeatable read")
	s := New(nil)

	type result struct {
		n   int
		err error
	}
	waiter := make(chan result, 1)
	err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		var holder int
		if err := tx.GetContext(ctx, &holder, `SELECT pg_backend_pid()`); err != nil {
			return err
		}
		if n, err := s.LockAndCountWebhooks(ctx, tx, 7); err != nil || n != 0 {
			t.Errorf("LockAndCountWebhooks() = %d, %v; want 0", n, err)
		}

		go func() {
			var r result
			r.err = dbx.TransactionContext(ctx, func(tx *db.Tx) error {
				var err error
				r.n, err = s.LockAndCountWebhooks(ctx, tx, 7)
				return err
			})
			waiter <- r
		}()

		// Insert only once the other transaction waits for the lock.
		deadline := time.Now().Add(time.Minute)
		for {
			var blocked bool
			if err := dbx.GetContext(ctx, &blocked,
				`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))`, holder); err != nil {
				return err
			}
			if blocked {
				break
			}
			if time.Now().After(deadline) {
				t.Error("the second LockAndCountWebhooks did not wait for the lock")
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		_, err := s.CreateWebhook(ctx, tx, models.Webhook{RepoID: 7, URL: "https://93.184.216.34/a", ContentType: 0, Active: true})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	r := <-waiter
	if r.err != nil || r.n != 1 {
		t.Errorf("LockAndCountWebhooks() after waiting = %d, %v; want 1", r.n, r.err)
	}
}

// Every field CreateWebhookDelivery takes is stored in its own column.
func TestCreateWebhookDeliveryColumns(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	hook, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://93.184.216.34/a", ContentType: 0, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if err := s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
		ID:              id,
		WebhookID:       hook,
		Event:           3,
		RequestURL:      "u",
		RequestMethod:   "PUT",
		RequestError:    sql.NullString{String: "e", Valid: true},
		RequestHeaders:  "rqh",
		RequestBody:     "rqb",
		ResponseStatus:  418,
		ResponseHeaders: "rsh",
		ResponseBody:    "rsb",
	}); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetWebhookDeliveryByID(ctx, dbx, hook, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != id || d.WebhookID != hook || d.Event != 3 || d.RequestURL != "u" || d.RequestMethod != "PUT" ||
		!d.RequestError.Valid || d.RequestError.String != "e" || d.RequestHeaders != "rqh" || d.RequestBody != "rqb" ||
		d.ResponseStatus != 418 || d.ResponseHeaders != "rsh" || d.ResponseBody != "rsb" {
		t.Errorf("delivery = %+v", d)
	}
}

// A webhook ID past what the ID column holds (a 32-bit SERIAL on Postgres)
// names no webhook, on both drivers: lookups find nothing and an update or
// delete changes nothing, rather than failing with "out of range". The
// SSH webhook commands take the ID from the user.
func TestWebhookIDsBeyondColumnRange(t *testing.T) {
	t.Parallel()
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	wid, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://example.com", ContentType: 1, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{math.MaxInt32 + 1, math.MaxInt64} {
		if _, err := s.GetWebhookByID(ctx, dbx, 7, id); !errors.Is(err, db.ErrRecordNotFound) {
			t.Errorf("GetWebhookByID(%d) error = %v, want %v", id, err, db.ErrRecordNotFound)
		}
		if _, err := s.GetActiveWebhookByIDWhereEvent(ctx, dbx, 7, id, 1); !errors.Is(err, db.ErrRecordNotFound) {
			t.Errorf("GetActiveWebhookByIDWhereEvent(%d) error = %v, want %v", id, err, db.ErrRecordNotFound)
		}
		if _, err := s.GetWebhookDeliveryByID(ctx, dbx, id, uuid.New()); !errors.Is(err, db.ErrRecordNotFound) {
			t.Errorf("GetWebhookDeliveryByID(%d) error = %v, want %v", id, err, db.ErrRecordNotFound)
		}
		if ds, err := s.ListWebhookDeliveriesByWebhookID(ctx, dbx, id); err != nil || len(ds) != 0 {
			t.Errorf("ListWebhookDeliveriesByWebhookID(%d) = %d, %v; want 0, nil", id, len(ds), err)
		}
		if es, err := s.GetWebhookEventsByWebhookID(ctx, dbx, id); err != nil || len(es) != 0 {
			t.Errorf("GetWebhookEventsByWebhookID(%d) = %d, %v; want 0, nil", id, len(es), err)
		}
		if err := s.DeleteWebhookEventsByID(ctx, dbx, id, []int64{id}); err != nil {
			t.Errorf("DeleteWebhookEventsByID(%d) = %v, want nil", id, err)
		}
		if err := s.PruneWebhookDeliveries(ctx, dbx, id, 1); err != nil {
			t.Errorf("PruneWebhookDeliveries(%d) = %v, want nil", id, err)
		}
		if err := s.UpdateWebhookByID(ctx, dbx, models.Webhook{ID: id, RepoID: 7, URL: "https://example.com"}); !errors.Is(err, db.ErrRecordNotFound) {
			t.Errorf("UpdateWebhookByID(%d) = %v, want %v", id, err, db.ErrRecordNotFound)
		}
		if err := s.DeleteWebhookForRepoByID(ctx, dbx, 7, id); err != nil && !errors.Is(err, db.ErrRecordNotFound) {
			t.Errorf("DeleteWebhookForRepoByID(%d) = %v", id, err)
		}
	}
	if _, err := s.GetWebhookByID(ctx, dbx, 7, wid); err != nil {
		t.Errorf("GetWebhookByID(%d) = %v, want the webhook", wid, err)
	}
}
