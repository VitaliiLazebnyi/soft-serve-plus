package database

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/matryer/is"
)

// TestFindDefaultAdminUserID verifies that the default admin is the
// lowest-ID admin, skipping lower-ID non-admins, and that sql.ErrNoRows is
// returned when no user is an admin.
func TestFindDefaultAdminUserID(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &userStore{}

	// The migration seeds the initial "admin" user; demote it so the test
	// controls every admin row.
	_, err := dbx.ExecContext(ctx, `UPDATE users SET admin = false;`)
	is.NoErr(err)

	_, err = s.FindDefaultAdminUserID(ctx, dbx)
	is.True(errors.Is(err, sql.ErrNoRows))

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
