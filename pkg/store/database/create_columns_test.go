package database

import (
	"database/sql"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/google/uuid"
)

// Each flag of CreateRepo lands in its own column: one repository per flag,
// with only that flag set.
func TestCreateRepoColumns(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	const owner = 100 // see newWebhookTestDB

	type flags struct{ private, hidden, mirror, anonymous bool }
	for name, want := range map[string]flags{
		"p": {private: true},
		"h": {hidden: true},
		"m": {mirror: true},
		"a": {anonymous: true},
	} {
		if _, err := s.CreateRepo(ctx, dbx, models.Repo{
			Name:        name,
			UserID:      owner,
			ProjectName: "P-" + name,
			Description: "D-" + name,
			Private:     want.private,
			Hidden:      want.hidden,
			Mirror:      want.mirror,
			Anonymous:   want.anonymous,
		}); err != nil {
			t.Fatal(err)
		}
		r, err := s.GetRepoByName(ctx, dbx, name)
		if err != nil {
			t.Fatal(err)
		}
		got := flags{private: r.Private, hidden: r.Hidden, mirror: r.Mirror, anonymous: r.Anonymous}
		if got != want || r.ProjectName != "P-"+name || r.Description != "D-"+name || r.UserID != owner {
			t.Errorf("repo %q = %+v, want flags %+v", name, r, want)
		}
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
