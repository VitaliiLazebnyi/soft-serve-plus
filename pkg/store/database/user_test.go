package database

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"charm.land/log/v2"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/matryer/is"
)

// TestFindDefaultAdminUserID verifies that the default admin is the
// lowest-ID admin, skipping lower-ID non-admins, and that db.ErrRecordNotFound is
// returned when no user is an admin.
func TestFindDefaultAdminUserID(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}

	// The migration seeds the initial "admin" user; demote it so the test
	// controls every admin row.
	_, err := dbx.ExecContext(ctx, `UPDATE users SET admin = false;`)
	is.NoErr(err)

	_, err = s.FindDefaultAdminUserID(ctx, dbx)
	is.True(errors.Is(err, db.ErrRecordNotFound))

	_, err = dbx.ExecContext(ctx, `INSERT INTO users (id, username, admin, updated_at) VALUES
		(200, 'plain1', false, CURRENT_TIMESTAMP),
		(300, 'admin300', true, CURRENT_TIMESTAMP),
		(250, 'admin250', true, CURRENT_TIMESTAMP),
		(400, 'plain2', false, CURRENT_TIMESTAMP);`)
	is.NoErr(err)

	id, err := s.FindDefaultAdminUserID(ctx, dbx)
	is.NoErr(err)
	is.Equal(id, int64(250))
}

// A stored key that does not parse is skipped and logged on the store's
// logger, not on one taken from the context.
func TestListPublicKeysLogsOnStoreLogger(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	var logs, ctxLogs bytes.Buffer
	s := New(log.New(&logs))
	ctx = log.WithContext(ctx, log.New(&ctxLogs))
	_, err := dbx.ExecContext(ctx, `INSERT INTO public_keys (user_id, public_key, updated_at) VALUES (1, 'not a key', CURRENT_TIMESTAMP);`)
	is.NoErr(err)
	pks, err := s.ListPublicKeysByUserID(ctx, dbx, 1)
	is.NoErr(err)
	is.Equal(len(pks), 0)
	is.True(strings.Contains(logs.String(), "skipping unparsable stored public key"))
	is.True(strings.Contains(logs.String(), "store"))
	is.Equal(ctxLogs.String(), "")
}
