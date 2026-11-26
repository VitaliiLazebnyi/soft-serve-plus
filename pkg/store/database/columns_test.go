package database

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/google/uuid"
)

// Each column list is the db tags of its model, in field order, less the
// fields a join fills in, and each qualified list is the same with the
// table in front. A field added to a model is then read once it is added
// to its list.
func TestColumnListsMatchModels(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		model                  any
		table, list, qualified string
		joined                 []string
	}{
		{models.AccessToken{}, "access_tokens", accessTokenColumns, "", nil},
		{models.Collab{}, "collabs", collabColumns, collabColumnsQualified, nil},
		{models.LFSObject{}, "lfs_objects", lfsObjectColumns, "", nil},
		{models.LFSLock{}, "lfs_locks", lfsLockColumns, lfsLockColumnsQualified, []string{"owner_name"}},
		{models.Repo{}, "repos", repoColumns, repoColumnsQualified, nil},
		{models.User{}, "users", userColumns, userColumnsQualified, nil},
		{models.Webhook{}, "webhooks", webhookColumns, webhookColumnsQualified, nil},
		{models.WebhookEvent{}, "webhook_events", webhookEventColumns, "", nil},
		{models.WebhookDelivery{}, "webhook_deliveries", webhookDeliveryColumns, "", nil},
	} {
		rt := reflect.TypeOf(tc.model)
		var want, wantQ []string
		for i := range rt.NumField() {
			tag := rt.Field(i).Tag.Get("db")
			if tag == "" || slices.Contains(tc.joined, tag) {
				continue
			}
			want = append(want, tag)
			wantQ = append(wantQ, tc.table+"."+tag)
		}
		if got := strings.Join(want, ", "); tc.list != got {
			t.Errorf("%s columns = %q, want %q", tc.table, tc.list, got)
		}
		if got := strings.Join(wantQ, ", "); tc.qualified != "" && tc.qualified != got {
			t.Errorf("%s qualified columns = %q, want %q", tc.table, tc.qualified, got)
		}
	}
}

// A column a model does not know, as a later migration adds, does not
// break the reads of its table: they name the columns they scan.
func TestReadsIgnoreColumnsTheModelLacks(t *testing.T) {
	t.Parallel()
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	for _, table := range []string{"access_tokens", "collabs", "lfs_objects", "lfs_locks", "repos", "users", "webhooks", "webhook_events", "webhook_deliveries"} {
		if _, err := dbx.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN zz_unknown INTEGER"); err != nil {
			t.Fatalf("add a column to %s: %v", table, err)
		}
	}
	const owner = 100 // see newWebhookTestDB
	check := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	try := func(what string, fn func(context.Context, db.Handler) error) {
		t.Helper()
		check(what, fn(ctx, dbx))
	}

	try("CreateAccessToken", func(ctx context.Context, h db.Handler) error {
		_, err := s.CreateAccessToken(ctx, h, "t", owner, "hash-1", time.Time{})
		return err
	})
	try("GetAccessTokensByUserID", func(ctx context.Context, h db.Handler) error {
		_, err := s.GetAccessTokensByUserID(ctx, h, owner)
		return err
	})
	try("GetAccessTokenByToken", func(ctx context.Context, h db.Handler) error {
		_, err := s.GetAccessTokenByToken(ctx, h, "hash-1")
		return err
	})
	try("CreateRepo", func(ctx context.Context, h db.Handler) error {
		_, err := s.CreateRepo(ctx, h, models.Repo{Name: "extra", UserID: owner})
		return err
	})
	for name, fn := range map[string]func() error{
		"GetAllRepos":             func() error { _, err := s.GetAllRepos(ctx, dbx); return err },
		"GetUserRepos":            func() error { _, err := s.GetUserRepos(ctx, dbx, owner); return err },
		"GetRepoByName":           func() error { _, err := s.GetRepoByName(ctx, dbx, "repo7"); return err },
		"GetRepoWithCollabByName": func() error { _, _, _, err := s.GetRepoWithCollabByName(ctx, dbx, "repo7", owner); return err },
		"GetRepoByID":             func() error { _, err := s.GetRepoByID(ctx, dbx, 7); return err },
		"GetUserByID":             func() error { _, err := s.GetUserByID(ctx, dbx, owner); return err },
		"FindUserByUsername":      func() error { _, err := s.FindUserByUsername(ctx, dbx, "owner"); return err },
		"GetAllUsers":             func() error { _, err := s.GetAllUsers(ctx, dbx); return err },
	} {
		check(name, fn())
	}
	try("ForUpdate reads", func(ctx context.Context, _ db.Handler) error {
		return dbx.TransactionContext(ctx, func(tx *db.Tx) error {
			if _, err := s.GetRepoByIDForUpdate(ctx, tx, 7); err != nil {
				return err
			}
			if _, err := s.GetRepoByNameForUpdate(ctx, tx, "repo7"); err != nil {
				return err
			}
			if _, err := s.GetUserByIDForUpdate(ctx, tx, owner); err != nil {
				return err
			}
			_, err := s.FindUserByUsernameForUpdate(ctx, tx, "owner")
			return err
		})
	})

	check("AddCollabByUsernameAndRepo", s.AddCollabByUsernameAndRepo(ctx, dbx, "owner", "repo8", access.ReadOnlyAccess))
	for name, fn := range map[string]func() error{
		"GetCollabByUsernameAndRepo": func() error { _, err := s.GetCollabByUsernameAndRepo(ctx, dbx, "owner", "repo8"); return err },
		"ListCollabsByRepo":          func() error { _, err := s.ListCollabsByRepo(ctx, dbx, "repo8"); return err },
		"ListCollabsByRepoAsUsers":   func() error { _, err := s.ListCollabsByRepoAsUsers(ctx, dbx, "repo8"); return err },
		"GetCollabByUserIDAndRepoID": func() error { _, err := s.GetCollabByUserIDAndRepoID(ctx, dbx, owner, 8); return err },
		"ListCollabsByUserID":        func() error { _, err := s.ListCollabsByUserID(ctx, dbx, owner); return err },
	} {
		check(name, fn())
	}

	check("CreateLFSObject", s.CreateLFSObject(ctx, dbx, 7, "oid1", 1))
	try("CreateLFSLockForUser", func(ctx context.Context, h db.Handler) error {
		_, err := s.CreateLFSLockForUser(ctx, h, 7, owner, "a.bin", "")
		return err
	})
	for name, fn := range map[string]func() error{
		"GetLFSObjectByOid":   func() error { _, err := s.GetLFSObjectByOid(ctx, dbx, 7, "oid1"); return err },
		"GetLFSObjectsByOids": func() error { _, err := s.GetLFSObjectsByOids(ctx, dbx, 7, []string{"oid1"}); return err },
		"GetLFSObjects":       func() error { _, err := s.GetLFSObjects(ctx, dbx, 7); return err },
		"GetLFSLocks":         func() error { _, err := s.GetLFSLocks(ctx, dbx, 7, 0, 10); return err },
		"GetLFSLockForPath":   func() error { _, err := s.GetLFSLockForPath(ctx, dbx, 7, "a.bin"); return err },
	} {
		check(name, fn())
	}

	wid, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://example.com", ContentType: 1, Active: true})
	check("CreateWebhook", err)
	check("CreateWebhookEvents", s.CreateWebhookEvents(ctx, dbx, wid, []int{1}))
	did := uuid.New()
	check("CreateWebhookDelivery", s.CreateWebhookDelivery(ctx, dbx, models.WebhookDelivery{ID: did, WebhookID: wid, Event: 1}))
	for name, fn := range map[string]func() error{
		"GetWebhookByID":                   func() error { _, err := s.GetWebhookByID(ctx, dbx, 7, wid); return err },
		"GetWebhooksByRepoID":              func() error { _, err := s.GetWebhooksByRepoID(ctx, dbx, 7); return err },
		"GetWebhooksByRepoIDWhereEvent":    func() error { _, err := s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, []int{1}); return err },
		"GetActiveWebhookByIDWhereEvent":   func() error { _, err := s.GetActiveWebhookByIDWhereEvent(ctx, dbx, 7, wid, 1); return err },
		"GetWebhookEventsByWebhookID":      func() error { _, err := s.GetWebhookEventsByWebhookID(ctx, dbx, wid); return err },
		"GetWebhookDeliveryByID":           func() error { _, err := s.GetWebhookDeliveryByID(ctx, dbx, wid, did); return err },
		"ListWebhookDeliveriesByWebhookID": func() error { _, err := s.ListWebhookDeliveriesByWebhookID(ctx, dbx, wid); return err },
	} {
		check(name, fn())
	}
}
