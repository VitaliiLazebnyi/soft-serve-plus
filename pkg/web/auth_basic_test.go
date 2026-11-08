package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/matryer/is"
)

// HTTP Basic accepts an access token the ways GitHub clients send one, but a
// token sent as the password with another registered user's name does not
// log in as the token's owner.
func TestBasicAuthTokenForms(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	ctx := f.ctx
	is.NoErr(f.be.SetPassword(ctx, "stranger", "stranger-password"))
	is.NoErr(f.be.SetPassword(ctx, "owner", "owner-password"))
	stranger, err := f.be.User(ctx, "stranger")
	is.NoErr(err)
	expired, err := f.be.CreateAccessToken(ctx, stranger, "old", time.Now().Add(-time.Hour))
	is.NoErr(err)

	for _, tc := range []struct {
		name    string
		user    string
		pass    string
		want    string // the username logged in as, or "" for a refusal
		wantErr error
	}{
		{"password", "stranger", "stranger-password", "stranger", nil},
		{"password, other case", "Stranger", "stranger-password", "stranger", nil},
		{"wrong password", "stranger", "owner-password", "", ErrInvalidPassword},
		{"token as password, empty username", "", f.token, "stranger", nil},
		{"token as password, owner's username", "stranger", f.token, "stranger", nil},
		{"token as password, owner's username in other case", "STRANGER", f.token, "stranger", nil},
		{"token as password, placeholder username", "x-access-token", f.token, "stranger", nil},
		{"token as password, invalid placeholder", "x_token", f.token, "stranger", nil},
		{"token as password, another user's name", "owner", f.token, "", ErrInvalidPassword},
		{"token as password, another user's name in other case", "Owner", f.token, "", ErrInvalidPassword},
		{"token as username, no password", f.token, "", "stranger", nil},
		{"token as username, placeholder password", f.token, "x-oauth-basic", "stranger", nil},
		{"token as username, token as password", f.token, f.token, "stranger", nil},
		{"bad token, empty username", "", "ss_bad-token", "", ErrInvalidPassword},
		{"bad token as username", "ss_bad-token", "", "", ErrInvalidToken},
		{"bad token as username, placeholder password", "ss_bad-token", "x-oauth-basic", "", ErrInvalidPassword},
		{"expired token, empty username", "", expired, "", ErrInvalidPassword},
		{"expired token as username", expired, "x-oauth-basic", "", ErrInvalidPassword},
	} {
		t.Run(tc.name, func(t *testing.T) {
			is := is.New(t)
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
			req.SetBasicAuth(tc.user, tc.pass)

			user, _, err := parseAuthHdr(req, []string{"secret"})
			if tc.want == "" {
				is.True(user == nil)
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			is.NoErr(err)
			is.True(user != nil)
			is.Equal(user.Username(), tc.want)
		})
	}
}

// A refused "alice:<bob's token>" login gets the bad credentials answer of a
// wrong password, not bob's access or an anonymous one.
func TestBasicAuthTokenOfAnotherUserRefused(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	basic := func(user, pass string) string {
		r := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/", nil)
		r.SetBasicAuth(user, pass)
		return r.Header.Get("Authorization")
	}
	const path = "/secret.git/info/refs?service=git-upload-pack"

	w := f.do(http.MethodGet, path, basic("owner", f.token))
	is.Equal(w.Code, http.StatusForbidden)
	is.Equal(answerOf(w), answerOf(f.do(http.MethodGet, path, basic("owner", "wrong-password"))))
}
