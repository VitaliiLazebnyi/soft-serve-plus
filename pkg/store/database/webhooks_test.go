package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/dbtest"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/google/uuid"
	"github.com/matryer/is"
)

// newWebhookTestDB opens a migrated database for one test: Postgres when
// SOFT_SERVE_DB_DRIVER is "postgres", SQLite otherwise.
func newWebhookTestDB(t *testing.T) (context.Context, *db.DB) {
	t.Helper()
	is := is.New(t)
	ctx := context.Background()
	dbx, err := dbtest.Open(ctx, t)
	is.NoErr(err)
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.DB.Driver = dbx.DriverName()
	ctx = config.WithContext(ctx, cfg)
	is.NoErr(migrate.Migrate(ctx, dbx, cfg, nil))

	// Foreign keys are enforced, so the webhook tables need real parents: one
	// user and the repos 7 and 8 that the tests use.
	_, err = dbx.ExecContext(ctx, `INSERT INTO users (id, username, admin, updated_at) VALUES (100, 'owner', false, CURRENT_TIMESTAMP);`)
	is.NoErr(err)
	_, err = dbx.ExecContext(ctx, `INSERT INTO repos (id, name, project_name, description, private, mirror, hidden, user_id, updated_at)
		VALUES (7, 'repo7', 'repo7', '', false, false, false, 100, CURRENT_TIMESTAMP),
		       (8, 'repo8', 'repo8', '', false, false, false, 100, CURRENT_TIMESTAMP);`)
	is.NoErr(err)
	return ctx, dbx
}

func isSqlite(dbx *db.DB) bool {
	d := dbx.DriverName()
	return d == "sqlite"
}

func TestWebhookStoreRoundTrip(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}

	id, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.34/a", "sec", 1, true)
	is.NoErr(err)

	wh, err := s.GetWebhookByID(ctx, dbx, 7, id)
	is.NoErr(err)
	is.Equal(wh.URL, "https://93.184.216.34/a")
	is.Equal(wh.Secret, "sec")
	is.Equal(wh.ContentType, 1)
	is.True(wh.Active)
	is.Equal(wh.RepoID, int64(7))

	// Scoped by repo: the same ID under another repo is not found.
	_, err = s.GetWebhookByID(ctx, dbx, 8, id)
	is.True(errors.Is(err, db.ErrRecordNotFound))

	is.NoErr(s.UpdateWebhookByID(ctx, dbx, 7, id, "https://93.184.216.35/b", "new", 0, false))
	wh, err = s.GetWebhookByID(ctx, dbx, 7, id)
	is.NoErr(err)
	is.Equal(wh.URL, "https://93.184.216.35/b")
	is.Equal(wh.Secret, "new")
	is.Equal(wh.ContentType, 0)
	is.True(!wh.Active)

	// An update scoped to another repo changes nothing and reports the miss.
	is.True(errors.Is(s.UpdateWebhookByID(ctx, dbx, 8, id, "https://93.184.216.36/c", "x", 0, true), db.ErrRecordNotFound))
	wh, err = s.GetWebhookByID(ctx, dbx, 7, id)
	is.NoErr(err)
	is.Equal(wh.URL, "https://93.184.216.35/b")

	list, err := s.GetWebhooksByRepoID(ctx, dbx, 7)
	is.NoErr(err)
	is.Equal(len(list), 1)
	none, err := s.GetWebhooksByRepoID(ctx, dbx, 99)
	is.NoErr(err)
	is.Equal(len(none), 0)

	is.True(errors.Is(s.DeleteWebhookForRepoByID(ctx, dbx, 8, id), db.ErrRecordNotFound)) // wrong repo: nothing deleted
	_, err = s.GetWebhookByID(ctx, dbx, 7, id)
	is.NoErr(err)
	is.NoErr(s.DeleteWebhookForRepoByID(ctx, dbx, 7, id))
	_, err = s.GetWebhookByID(ctx, dbx, 7, id)
	is.True(errors.Is(err, db.ErrRecordNotFound))
}

func TestWebhookStoreEvents(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}

	hook, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.34/a", "", 0, true)
	is.NoErr(err)
	other, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.35/b", "", 0, true)
	is.NoErr(err)

	is.NoErr(s.CreateWebhookEvents(ctx, dbx, hook, []int{4, 5}))
	is.NoErr(s.CreateWebhookEvents(ctx, dbx, other, []int{6}))
	is.NoErr(s.CreateWebhookEvents(ctx, dbx, hook, nil)) // nothing to add

	evs, err := s.GetWebhookEventsByWebhookID(ctx, dbx, hook)
	is.NoErr(err)
	is.Equal(len(evs), 2)

	is.Equal(evs[0].WebhookID, hook)

	// Filter by event: push (4) matches hook only; an event with no match
	// returns nothing; an empty filter returns nothing and no error.
	whs, err := s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, []int{4})
	is.NoErr(err)
	is.Equal(len(whs), 1)
	is.Equal(whs[0].ID, hook)

	// An inactive webhook subscribed to the same event is left out.
	inactive, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.36/c", "", 0, false)
	is.NoErr(err)
	is.NoErr(s.CreateWebhookEvents(ctx, dbx, inactive, []int{4}))
	whs, err = s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, []int{4})
	is.NoErr(err)
	is.Equal(len(whs), 1)
	is.Equal(whs[0].ID, hook)

	// The single-webhook lookup applies the same filters: the webhook must
	// belong to the repository, be active and subscribe to the event.
	wh, err := s.GetActiveWebhookByIDWhereEvent(ctx, dbx, 7, hook, 5)
	is.NoErr(err)
	is.Equal(wh.ID, hook)
	is.Equal(wh.URL, "https://93.184.216.34/a")
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
	is.NoErr(err)
	is.Equal(len(whs), 0)

	whs, err = s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, nil)
	is.NoErr(err)
	is.Equal(len(whs), 0)

	// Deleting no IDs is a no-op; deleting some removes only those.
	is.NoErr(s.DeleteWebhookEventsByID(ctx, dbx, hook, nil))
	evs, err = s.GetWebhookEventsByWebhookID(ctx, dbx, hook)
	is.NoErr(err)
	is.Equal(len(evs), 2)

	// The deletion is scoped to the webhook: another webhook's ID leaves
	// these events alone.
	is.NoErr(s.DeleteWebhookEventsByID(ctx, dbx, other, []int64{evs[0].ID, evs[1].ID}))
	evs, err = s.GetWebhookEventsByWebhookID(ctx, dbx, hook)
	is.NoErr(err)
	is.Equal(len(evs), 2)
	otherEvs, err := s.GetWebhookEventsByWebhookID(ctx, dbx, other)
	is.NoErr(err)
	is.Equal(len(otherEvs), 1)
	is.NoErr(s.DeleteWebhookEventsByID(ctx, dbx, hook, []int64{otherEvs[0].ID}))
	otherEvs, err = s.GetWebhookEventsByWebhookID(ctx, dbx, other)
	is.NoErr(err)
	is.Equal(len(otherEvs), 1)

	is.NoErr(s.DeleteWebhookEventsByID(ctx, dbx, hook, []int64{evs[0].ID}))
	evs, err = s.GetWebhookEventsByWebhookID(ctx, dbx, hook)
	is.NoErr(err)
	is.Equal(len(evs), 1)

	// Deleting a webhook deletes its events; the other webhook's stay.
	is.NoErr(s.DeleteWebhookForRepoByID(ctx, dbx, 7, hook))
	evs, err = s.GetWebhookEventsByWebhookID(ctx, dbx, hook)
	is.NoErr(err)
	is.Equal(len(evs), 0)
	evs, err = s.GetWebhookEventsByWebhookID(ctx, dbx, other)
	is.NoErr(err)
	is.Equal(len(evs), 1)
}

func TestWebhookStoreDeliveries(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}

	hook, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.34/a", "", 0, true)
	is.NoErr(err)
	other, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.35/b", "", 0, true)
	is.NoErr(err)

	okID := uuid.New()
	failID := uuid.New()
	is.NoErr(s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
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
	}))
	is.NoErr(s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
		ID:            failID,
		WebhookID:     hook,
		Event:         5,
		RequestURL:    "https://93.184.216.34/a",
		RequestMethod: "POST",
		RequestError:  sql.NullString{String: "dial refused", Valid: true},
		RequestBody:   "{}",
	}))
	is.NoErr(s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
		ID:             uuid.New(),
		WebhookID:      other,
		Event:          6,
		RequestURL:     "https://93.184.216.35/b",
		RequestMethod:  "POST",
		RequestBody:    "{}",
		ResponseStatus: 204,
	}))

	d, err := s.GetWebhookDeliveryByID(ctx, dbx, hook, okID)
	is.NoErr(err)
	is.Equal(d.RequestBody, `{"a":1}`)
	is.Equal(d.Event, 4)
	is.True(!d.RequestError.Valid) // no error recorded

	f, err := s.GetWebhookDeliveryByID(ctx, dbx, hook, failID)
	is.NoErr(err)
	is.True(f.RequestError.Valid)
	is.Equal(f.RequestError.String, "dial refused")

	// Scoped by webhook: another webhook's delivery ID is not found.
	_, err = s.GetWebhookDeliveryByID(ctx, dbx, other, okID)
	is.True(errors.Is(err, db.ErrRecordNotFound))

	all, err := allDeliveries(ctx, dbx, hook)
	is.NoErr(err)
	is.Equal(len(all), 2)

	list, err := s.ListWebhookDeliveriesByWebhookID(ctx, dbx, hook)
	is.NoErr(err)
	is.Equal(len(list), 2)

	// Deleting a webhook deletes its deliveries; the other webhook's stay.
	is.NoErr(s.DeleteWebhookForRepoByID(ctx, dbx, 7, hook))
	all, err = allDeliveries(ctx, dbx, hook)
	is.NoErr(err)
	is.Equal(len(all), 0)
	all, err = allDeliveries(ctx, dbx, other)
	is.NoErr(err)
	is.Equal(len(all), 1)
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
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	id, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.34/a", "", 0, true)
	is.NoErr(err)
	did := uuid.New()
	is.NoErr(s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
		ID:            did,
		WebhookID:     id,
		Event:         4,
		RequestURL:    "u",
		RequestMethod: "POST",
	}))
	is.NoErr(dbx.Close())

	checks := map[string]error{}
	_, checks["GetWebhookByID"] = s.GetWebhookByID(ctx, dbx, 7, id)
	_, checks["GetWebhooksByRepoID"] = s.GetWebhooksByRepoID(ctx, dbx, 7)
	_, checks["LockAndCountWebhooks"] = s.LockAndCountWebhooks(ctx, dbx, 7)
	_, checks["GetWebhooksByRepoIDWhereEvent"] = s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, []int{4})
	_, checks["GetActiveWebhookByIDWhereEvent"] = s.GetActiveWebhookByIDWhereEvent(ctx, dbx, 7, id, 4)
	_, checks["CreateWebhook"] = s.CreateWebhook(ctx, dbx, 7, "u", "", 0, true)
	checks["UpdateWebhookByID"] = s.UpdateWebhookByID(ctx, dbx, 7, id, "u", "", 0, true)
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
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	id, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.34/a", "", 1, true)
	is.NoErr(err)
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
	is.True(errors.Is(err, db.ErrForeignKey))

	var n int
	is.NoErr(dbx.GetContext(ctx, &n, "SELECT COUNT(*) FROM webhook_deliveries"))
	is.Equal(n, 0)
}

// Both drivers' forms of a foreign key violation map to db.ErrForeignKey,
// which CreateWebhookDelivery tests for, and nothing else does.
func TestForeignKeyViolationIsMapped(t *testing.T) {
	is := is.New(t)
	is.True(errors.Is(fmt.Errorf("wrapped: %w", db.ErrForeignKey), db.ErrForeignKey))
	is.True(!errors.Is(db.ErrDuplicateKey, db.ErrForeignKey))
	is.True(!errors.Is(errors.New("FOREIGN KEY constraint failed"), db.ErrForeignKey))
	is.True(!errors.Is(nil, db.ErrForeignKey))

	// A real foreign key violation from the driver under test.
	ctx, dbx := newWebhookTestDB(t)
	_, err := dbx.ExecContext(ctx, `INSERT INTO webhook_events (webhook_id, event) VALUES (424242, 1)`)
	is.True(err != nil)
	is.True(errors.Is(err, db.ErrForeignKey))
}

// Webhooks and their events are listed in creation order. Without ORDER BY,
// SQLite returns webhooks in URL order through the unique (repo_id, url)
// index, so "n=10" came before "n=9".
func TestWebhookListsAreOrderedByID(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	var ids []int64
	for _, n := range []int{9, 10, 1} {
		id, err := s.CreateWebhook(ctx, dbx, 7, fmt.Sprintf("https://93.184.216.34/?n=%d", n), "", 1, true)
		is.NoErr(err)
		is.NoErr(s.CreateWebhookEvents(ctx, dbx, id, []int{3, 1, 2}))
		ids = append(ids, id)
	}

	list, err := s.GetWebhooksByRepoID(ctx, dbx, 7)
	is.NoErr(err)
	byEvent, err := s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, []int{1})
	is.NoErr(err)
	for _, got := range [][]int64{idsOf(list), idsOf(byEvent)} {
		is.Equal(got, ids)
	}

	events, err := s.GetWebhookEventsByWebhookID(ctx, dbx, ids[0])
	is.NoErr(err)
	for i := 1; i < len(events); i++ {
		is.True(events[i-1].ID < events[i].ID)
	}
}

// Deliveries recorded within the same second list in the order they were
// inserted, oldest first. created_at has one-second resolution and IDs are
// random UUIDs, so neither can order them.
func TestWebhookStoreDeliveriesOrder(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}

	hook, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.34/a", "", 0, true)
	is.NoErr(err)
	other, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.35/b", "", 0, true)
	is.NoErr(err)

	insert := func(webhookID int64) uuid.UUID {
		id := uuid.New()
		is.NoErr(s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
			ID:             id,
			WebhookID:      webhookID,
			Event:          1,
			RequestURL:     "https://93.184.216.34/a",
			RequestMethod:  "POST",
			RequestBody:    "{}",
			ResponseStatus: 200,
		}))
		return id
	}

	var want []uuid.UUID
	for range 20 {
		want = append(want, insert(hook))
		insert(other) // interleaved deliveries of another webhook
	}
	// Pin every row to the same second.
	_, err = dbx.ExecContext(ctx, `UPDATE webhook_deliveries SET created_at = '2024-01-01 00:00:00';`)
	is.NoErr(err)

	check := func() {
		t.Helper()
		list, err := s.ListWebhookDeliveriesByWebhookID(ctx, dbx, hook)
		is.NoErr(err)
		all, err := allDeliveries(ctx, dbx, hook)
		is.NoErr(err)
		for _, got := range [][]models.WebhookDelivery{list, all} {
			ids := make([]uuid.UUID, len(got))
			for i, d := range got {
				ids[i] = d.ID
			}
			is.Equal(ids, want)
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
	is.NoErr(err)
	want = append([]uuid.UUID{first}, want...)
	check()
}

// The delivery listing is bounded: past maxListedWebhookDeliveries rows,
// as when pruning has fallen behind, it returns the most recent ones,
// oldest first, and leaves the older ones out.
func TestWebhookDeliveryListingsAreLimited(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	hook, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.34/a", "", 1, true)
	is.NoErr(err)

	var ids []uuid.UUID
	for range maxListedWebhookDeliveries + 5 {
		id := uuid.New()
		is.NoErr(s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
			ID:             id,
			WebhookID:      hook,
			Event:          1,
			RequestURL:     "u",
			RequestMethod:  "POST",
			RequestBody:    "{}",
			ResponseStatus: 200,
		}))
		ids = append(ids, id)
	}
	want := ids[5:]

	list, err := s.ListWebhookDeliveriesByWebhookID(ctx, dbx, hook)
	is.NoErr(err)
	gotIDs := make([]uuid.UUID, len(list))
	for i, d := range list {
		gotIDs[i] = d.ID
	}
	is.Equal(gotIDs, want)
}

// Pruning keeps the most recently recorded deliveries of one webhook and
// leaves the deliveries of other webhooks alone. Recording order is what
// counts: created_at and the IDs are set to run the other way.
func TestPruneWebhookDeliveriesKeepsNewest(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	hook, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.34/a", "", 1, true)
	is.NoErr(err)
	other, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.35/b", "", 1, true)
	is.NoErr(err)

	// Eight deliveries, recorded with descending IDs and created_at, and
	// interleaved with deliveries of another webhook.
	ids := make([]uuid.UUID, 8)
	for i := range ids {
		ids[i] = uuid.MustParse(fmt.Sprintf("%08d-0000-1000-8000-000000000000", 99-i))
		is.NoErr(s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
			ID:             ids[i],
			WebhookID:      hook,
			Event:          1,
			RequestURL:     "u",
			RequestMethod:  "POST",
			RequestBody:    "{}",
			ResponseStatus: 200,
		}))
		_, err := dbx.ExecContext(ctx, dbx.Rebind(`UPDATE webhook_deliveries SET created_at = ? WHERE id = ?;`),
			fmt.Sprintf("2024-01-01 00:00:%02d", 59-i), ids[i])
		is.NoErr(err)
		if i < 3 {
			is.NoErr(s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{
				ID:             uuid.New(),
				WebhookID:      other,
				Event:          1,
				RequestURL:     "u",
				RequestMethod:  "POST",
				RequestBody:    "{}",
				ResponseStatus: 200,
			}))
		}
	}

	keptIDs := func() []uuid.UUID {
		t.Helper()
		all, err := allDeliveries(ctx, dbx, hook)
		is.NoErr(err)
		out := []uuid.UUID{}
		for _, d := range all {
			out = append(out, d.ID)
		}
		return out
	}

	is.NoErr(s.PruneWebhookDeliveries(ctx, dbx, hook, 5))
	is.Equal(keptIDs(), ids[3:]) // the three recorded first are gone

	others, err := allDeliveries(ctx, dbx, other)
	is.NoErr(err)
	is.Equal(len(others), 3)

	// Exactly as many deliveries as the limit, or fewer: nothing is deleted.
	is.NoErr(s.PruneWebhookDeliveries(ctx, dbx, hook, 5))
	is.Equal(keptIDs(), ids[3:])
	is.NoErr(s.PruneWebhookDeliveries(ctx, dbx, hook, 100))
	is.Equal(keptIDs(), ids[3:])

	is.NoErr(s.PruneWebhookDeliveries(ctx, dbx, hook, 0))
	is.Equal(keptIDs(), []uuid.UUID{})
	others, err = allDeliveries(ctx, dbx, other)
	is.NoErr(err)
	is.Equal(len(others), 3)
}

// The prune runs on every recorded delivery, so it must not scan the
// deliveries table. On SQLite both its lookups use the (webhook_id, seq)
// index. Postgres picks its plan from table statistics, which a small test
// table does not represent, so its plan is not asserted.
func TestPruneWebhookDeliveriesUsesIndex(t *testing.T) {
	is := is.New(t)
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
	is.NoErr(dbx.SelectContext(ctx, &plan, "EXPLAIN QUERY PLAN "+pruneWebhookDeliveriesQuery, 1, 1, 100))
	is.True(len(plan) > 0)
	for _, p := range plan {
		t.Log(p.Detail)
		is.True(!strings.HasPrefix(p.Detail, "SCAN"))       // no full scan
		is.True(!strings.Contains(p.Detail, "TEMP B-TREE")) // no sort
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
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	n, err := s.LockAndCountWebhooks(ctx, dbx, 7)
	is.NoErr(err)
	is.Equal(n, 0)
	for i := range 3 {
		_, err := s.CreateWebhook(ctx, dbx, 7, fmt.Sprintf("https://93.184.216.34/%d", i), "", 1, true)
		is.NoErr(err)
	}
	_, err = s.CreateWebhook(ctx, dbx, 8, "https://93.184.216.35/x", "", 1, true)
	is.NoErr(err)
	n, err = s.LockAndCountWebhooks(ctx, dbx, 7)
	is.NoErr(err)
	is.Equal(n, 3)
}

// allDeliveries returns every delivery recorded for webhookID, oldest first.
func allDeliveries(ctx context.Context, dbx *db.DB, webhookID int64) ([]models.WebhookDelivery, error) {
	var ds []models.WebhookDelivery
	err := dbx.SelectContext(ctx, &ds, dbx.Rebind(`SELECT * FROM webhook_deliveries WHERE webhook_id = ? ORDER BY seq;`), webhookID)
	return ds, err
}
