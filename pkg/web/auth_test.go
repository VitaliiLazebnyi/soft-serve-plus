package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// Credentials that fail are rejected, never served as an anonymous request:
// with anonymous read-write access a push advertisement for a missing
// repository would otherwise create it. Every scheme answers the same 403 as
// a Basic login with a bad password.
func TestBadCredentialsAreNotAnonymous(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	ctx := f.ctx
	is.NoErr(f.be.SetAnonAccess(ctx, access.ReadWriteAccess))

	stranger, err := f.be.User(ctx, "stranger")
	is.NoErr(err)
	expired, err := f.be.CreateAccessToken(ctx, stranger, "old", time.Now().Add(-time.Hour))
	is.NoErr(err)

	// A JWT whose user was deleted, and one whose user was deleted and
	// then replaced by another of the same name.
	gone, err := f.be.CreateUser(ctx, "gone", proto.UserOptions{})
	is.NoErr(err)
	goneJWT := mintJWT(ctx, t, gone, "new", "secret")
	is.NoErr(f.be.DeleteUser(ctx, "gone"))
	reborn, err := f.be.CreateUser(ctx, "reborn", proto.UserOptions{})
	is.NoErr(err)
	rebornJWT := mintJWT(ctx, t, reborn, "new", "secret")
	is.NoErr(f.be.DeleteUser(ctx, "reborn"))
	_, err = f.be.CreateUser(ctx, "reborn", proto.UserOptions{})
	is.NoErr(err)

	basic := func(user, pass string) string {
		r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		r.SetBasicAuth(user, pass)
		return r.Header.Get("Authorization")
	}

	for name, header := range map[string]string{
		"basic bad password":  basic("stranger", "bad-password"),
		"bad token":           "token ss_bogus",
		"expired token":       "token " + expired,
		"bad token as bearer": "Bearer ss_bogus",
		"deleted user jwt":    "Bearer " + goneJWT,
		"replaced user jwt":   "Bearer " + rebornJWT,
	} {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			w := f.do(http.MethodGet, "/new.git/info/refs?service=git-receive-pack", header)
			is.Equal(w.Code, http.StatusForbidden)
			is.True(!f.exists("new"))

			// A private repository answers 403 too, not the 401
			// credentials challenge an anonymous caller gets.
			w = f.do(http.MethodGet, "/secret.git/info/refs?service=git-upload-pack", header)
			is.Equal(w.Code, http.StatusForbidden)
		})
	}
}
