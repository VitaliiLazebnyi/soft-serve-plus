package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// HTTP Basic accepts an access token the ways GitHub clients send one. A
// token sent as the password logs in as its owner whatever the username,
// another registered user's name included. There is no password login: a
// password hash written into the database by hand does not let its user in.
func TestBasicAuthTokenForms(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	f := newOracleFixture(t)
	ctx := f.ctx
	f.env.storePasswordHash(t, "stranger", "stranger-password")
	f.env.storePasswordHash(t, "owner", "owner-password")
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
		{"stored password", "stranger", "stranger-password", "", errInvalidPassword},
		{"stored password, other case", "Stranger", "stranger-password", "", errInvalidPassword},
		{"wrong password", "stranger", "owner-password", "", errInvalidPassword},
		{"missing user", "nobody", "some-password", "", errInvalidPassword},
		{"token as password, empty username", "", f.token, "stranger", nil},
		{"token as password, owner's username", "stranger", f.token, "stranger", nil},
		{"token as password, owner's username in other case", "STRANGER", f.token, "stranger", nil},
		{"token as password, placeholder username", "x-access-token", f.token, "stranger", nil},
		{"token as password, invalid placeholder", "x_token", f.token, "stranger", nil},
		{"token as password, another user's name", "owner", f.token, "stranger", nil},
		{"token as password, another user's name in other case", "Owner", f.token, "stranger", nil},
		{"token as username, no password", f.token, "", "stranger", nil},
		{"token as username, placeholder password", f.token, "x-oauth-basic", "stranger", nil},
		{"token as username, token as password", f.token, f.token, "stranger", nil},
		{"bad token, empty username", "", "ss_bad-token", "", errInvalidPassword},
		{"bad token as username", "ss_bad-token", "", "", errInvalidToken},
		{"bad token as username, placeholder password", "ss_bad-token", "x-oauth-basic", "", errInvalidPassword},
		{"expired token, empty username", "", expired, "", errInvalidPassword},
		{"expired token as username", expired, "x-oauth-basic", "", errInvalidPassword},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
			req.SetBasicAuth(tc.user, tc.pass)

			user, _, err := f.server.authenticate(req, f.server.requestLogger(req), "secret")
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

// "alice:<bob's token>" logs in as bob, the token's owner, as on GitHub. A
// placeholder such as "git" keeps working after someone registers that
// name, and the answer does not depend on whether the name is registered,
// so a token holder cannot use it to find out which usernames exist.
func TestBasicAuthTokenIgnoresUsername(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	f := newOracleFixture(t)
	stranger, err := f.be.User(f.ctx, "stranger")
	is.NoErr(err)
	_, err = f.be.CreateRepository(f.ctx, "mine", stranger, proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	basic := func(user, pass string) string {
		r := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/", nil)
		r.SetBasicAuth(user, pass)
		return r.Header.Get("Authorization")
	}

	// The token's owner may read their private repository whatever name
	// comes with the token.
	const mine = "/mine.git/info/refs?service=git-upload-pack"
	want := answerOf(f.do(http.MethodGet, mine, basic("stranger", f.token)))
	is.Equal(want.code, http.StatusOK)
	for _, name := range []string{"owner", "Owner", "git", "nosuchuser", ""} {
		got := answerOf(f.do(http.MethodGet, mine, basic(name, f.token)))
		if got != want {
			t.Errorf("%q with the token: got %d, want the owner's %d", name, got.code, want.code)
		}
	}

	// A registered name and a free one get the same answer, on a
	// repository the token's owner may read and on one they may not.
	_, err = f.be.CreateUser(f.ctx, "git", proto.UserOptions{})
	is.NoErr(err)
	for _, path := range []string{mine, "/secret.git/info/refs?service=git-upload-pack"} {
		registered := answerOf(f.do(http.MethodGet, path, basic("owner", f.token)))
		placeholder := answerOf(f.do(http.MethodGet, path, basic("git", f.token)))
		free := answerOf(f.do(http.MethodGet, path, basic("nosuchuser", f.token)))
		if registered != free || placeholder != free {
			t.Errorf("%s: registered name %d, registered placeholder %d, free name %d", path, registered.code, placeholder.code, free.code)
		}
	}
}
