package database

import (
	"errors"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
)

// A write that matches no row reports db.ErrRecordNotFound rather than
// success, so a caller that raced a rename or a delete does not tell the
// user the change was made.
func TestWritesToMissingRowsAreNotFound(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	const repo = 7 // see newWebhookTestDB

	hook, err := s.CreateWebhook(ctx, dbx, repo, "https://93.184.216.34/a", "", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"DeleteRepoByName":         s.DeleteRepoByName(ctx, dbx, "nope"),
		"SetRepoIsPrivateByName":   s.SetRepoIsPrivateByName(ctx, dbx, "nope", true),
		"SetRepoIsHiddenByName":    s.SetRepoIsHiddenByName(ctx, dbx, "nope", true),
		"SetRepoIsMirrorByName":    s.SetRepoIsMirrorByName(ctx, dbx, "nope", true),
		"SetRepoDescriptionByName": s.SetRepoDescriptionByName(ctx, dbx, "nope", "d"),
		"SetRepoProjectNameByName": s.SetRepoProjectNameByName(ctx, dbx, "nope", "p"),
		// Another repository's webhook is not this one's.
		"UpdateWebhookByID":        s.UpdateWebhookByID(ctx, dbx, repo+1, hook, "https://93.184.216.34/b", "", 0, true),
		"DeleteWebhookForRepoByID": s.DeleteWebhookForRepoByID(ctx, dbx, repo+1, hook),
	} {
		if !errors.Is(err, db.ErrRecordNotFound) {
			t.Errorf("%s on a missing row: err = %v, want %v", name, err, db.ErrRecordNotFound)
		}
	}
}

// The settings setters write a setting whose row was deleted, rather than
// failing on the missing row.
func TestSettingsSettersRestoreMissingRows(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	if _, err := dbx.ExecContext(ctx, `DELETE FROM settings`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAnonAccess(ctx, dbx, access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAllowKeylessAccess(ctx, dbx, false); err != nil {
		t.Fatal(err)
	}
	if l, err := s.GetAnonAccess(ctx, dbx); err != nil || l != access.ReadWriteAccess {
		t.Errorf("GetAnonAccess() = %v, %v; want read-write", l, err)
	}
	if a, err := s.GetAllowKeylessAccess(ctx, dbx); err != nil || a {
		t.Errorf("GetAllowKeylessAccess() = %v, %v; want false", a, err)
	}
	// And again, over the row they wrote.
	if err := s.SetAnonAccess(ctx, dbx, access.NoAccess); err != nil {
		t.Fatal(err)
	}
	if l, err := s.GetAnonAccess(ctx, dbx); err != nil || l != access.NoAccess {
		t.Errorf("GetAnonAccess() = %v, %v; want no-access", l, err)
	}
}
