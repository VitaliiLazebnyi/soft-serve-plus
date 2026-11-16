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
	// The settings rows migration 1 seeds, gone.
	if _, err := dbx.ExecContext(ctx, `DELETE FROM settings`); err != nil {
		t.Fatal(err)
	}

	for name, err := range map[string]error{
		"DeleteRepoByName":         s.DeleteRepoByName(ctx, dbx, "nope"),
		"SetRepoIsPrivateByName":   s.SetRepoIsPrivateByName(ctx, dbx, "nope", true),
		"SetRepoIsHiddenByName":    s.SetRepoIsHiddenByName(ctx, dbx, "nope", true),
		"SetRepoIsMirrorByName":    s.SetRepoIsMirrorByName(ctx, dbx, "nope", true),
		"SetRepoDescriptionByName": s.SetRepoDescriptionByName(ctx, dbx, "nope", "d"),
		"SetRepoProjectNameByName": s.SetRepoProjectNameByName(ctx, dbx, "nope", "p"),
		"SetAnonAccess":            s.SetAnonAccess(ctx, dbx, access.ReadOnlyAccess),
		"SetAllowKeylessAccess":    s.SetAllowKeylessAccess(ctx, dbx, false),
		// Another repository's webhook is not this one's.
		"UpdateWebhookByID":        s.UpdateWebhookByID(ctx, dbx, repo+1, hook, "https://93.184.216.34/b", "", 0, true),
		"DeleteWebhookForRepoByID": s.DeleteWebhookForRepoByID(ctx, dbx, repo+1, hook),
	} {
		if !errors.Is(err, db.ErrRecordNotFound) {
			t.Errorf("%s on a missing row: err = %v, want %v", name, err, db.ErrRecordNotFound)
		}
	}
}
