package web

import (
	"bytes"
	"context"
	"crypto/elliptic"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/golang-jwt/jwt/v5"
	"github.com/matryer/is"
	"golang.org/x/crypto/bcrypt"
)

// Credentials that fail are rejected, never served as an anonymous request:
// with anonymous read-write access a push advertisement for a missing
// repository would otherwise create it. Every scheme answers the same 403 as
// a Basic login with a bad password.
func TestBadCredentialsAreNotAnonymous(t *testing.T) {
	t.Parallel()
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
	goneJWT := f.mintJWT(t, gone, "new", "secret")
	is.NoErr(f.be.DeleteUser(ctx, "gone"))
	reborn, err := f.be.CreateUser(ctx, "reborn", proto.UserOptions{})
	is.NoErr(err)
	rebornJWT := f.mintJWT(t, reborn, "new", "secret")
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
			t.Parallel()
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

// A request without credentials is anonymous: authenticate gives it a nil
// user and a nil error, which no caller can mistake for bad credentials or
// for credentials that could not be checked.
func TestAuthenticateWithoutCredentialsIsAnonymous(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	basic := func(user, pass string) string {
		r := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/", nil)
		r.SetBasicAuth(user, pass)
		return r.Header.Get("Authorization")
	}
	for name, header := range map[string]string{
		"no header":      "",
		"one word":       "Token",
		"unknown scheme": "Digest abc",
		"basic not b64":  "Basic !!!",
		"basic empty":    basic("", ""),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			user, audience, err := f.server.authenticate(req, f.server.requestLogger(req), "secret")
			if user != nil || audience != nil || err != nil {
				t.Errorf("authenticate(%q) = %v, %v, %v; want nil, nil, nil", header, user, audience, err)
			}
		})
	}
}

// bcryptCompare runs one bcrypt comparison at the default cost against a
// hash no password matches: what a password check would run for each Basic
// login whose password is not a token.
//
// The tests that time it do not run in parallel, so the machine is as busy
// for the bcrypt comparisons they time as for the logins.
func bcryptCompare(t *testing.T) func() {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("not-guess"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	return func() { _ = bcrypt.CompareHashAndPassword(hash, []byte("guess")) }
}

// A Basic login whose password is not an access token is refused without a
// bcrypt comparison. There is no password login, so there is nothing to
// compare against.
//
// As in TestBasicNonTokenLoginHoldsSlotBriefly, the fastest of many samples
// is compared, not a total over a stretch of time a busy machine can slow
// down in part: noise only makes samples slower, and a pair of logins that
// runs a bcrypt can never be faster than one bcrypt.
func TestBasicNonTokenLoginRunsNoBcrypt(t *testing.T) {
	f := newOracleFixture(t)

	// One bcrypt comparison, as a password check would run; the fastest of
	// a few.
	compare := bcryptCompare(t)
	oneBcrypt := time.Duration(math.MaxInt64)
	for range 4 {
		start := time.Now()
		compare()
		oneBcrypt = min(oneBcrypt, time.Since(start))
	}

	login := time.Duration(math.MaxInt64)
	for range 64 {
		req := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/", nil)
		start := time.Now()
		// "stranger" is a registered user, "nobody" is not; neither has a
		// way to log in with a password.
		req.SetBasicAuth("stranger", "guess")
		if user, _, err := f.server.authenticate(req, f.server.logger); user != nil || !badCredentials(err) {
			t.Fatalf("authenticate => %v, %v, want bad credentials", user, err)
		}
		req.SetBasicAuth("nobody", "guess")
		if user, _, err := f.server.authenticate(req, f.server.logger); user != nil || !badCredentials(err) {
			t.Fatalf("authenticate => %v, %v, want bad credentials", user, err)
		}
		login = min(login, time.Since(start))
	}
	if login >= oneBcrypt {
		t.Errorf("the fastest pair of failed Basic logins took %s, no faster than the fastest bcrypt comparison (%s)", login, oneBcrypt)
	}
}

// A failed Basic login holds its request slot for a small part of one bcrypt
// comparison. A login that ran a bcrypt while holding a slot would let a few
// looping clients from one address keep every slot, and every anonymous
// fetch would answer 503. Waits that only happen under contention, such as a
// per-client queue or a semaphore around cheap work, are deliberately not
// tested.
//
// The test does not race clients against the limit: how many slots looping
// clients hold depends on how fast the machine serves them, not only on what a
// login costs. It measures the cost itself. Requests are served one at a time
// through the production router, so none waits for a slot, and the time each
// spends in the router bounds the time it holds its slot. Every such login
// takes the same path, so the fastest of them is what the path costs; noise
// (a GC pause, a busy runner) only makes samples slower, and the fastest is
// compared with the fastest bcrypt measured on the same machine. A login that
// runs a bcrypt can never be faster than one.
func TestBasicNonTokenLoginHoldsSlotBriefly(t *testing.T) {
	env := newTestEnv(t)
	ctx, be := env.ctx, env.be
	if _, err := be.CreateRepository(ctx, "repo", fixtureAdmin(t, be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	// A limit puts the slot channel in the request path, as in production;
	// a slot a login leaked would make the ninth request answer 503.
	env.cfg.HTTP.MaxConnections = 8
	router := env.router(t)

	// One bcrypt comparison, as a password check would run; the faster of
	// two.
	compare := bcryptCompare(t)
	oneBcrypt := time.Duration(math.MaxInt64)
	for range 2 {
		start := time.Now()
		compare()
		oneBcrypt = min(oneBcrypt, time.Since(start))
	}

	login := time.Duration(math.MaxInt64)
	for range 32 {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/repo.git/info/refs?service=git-upload-pack", nil)
		req.SetBasicAuth("alice", "guess")
		rec := httptest.NewRecorder()
		start := time.Now()
		router.ServeHTTP(rec, req)
		login = min(login, time.Since(start))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("Basic login with a non-token password answered %d, want %d", rec.Code, http.StatusForbidden)
		}
	}

	// An order of magnitude, so a slow machine cannot close the gap: the
	// login path costs a few database lookups and no bcrypt.
	t.Logf("fastest failed login %s, fastest bcrypt %s", login, oneBcrypt)
	if login*10 > oneBcrypt {
		t.Errorf("a failed Basic login held its request slot for %s, more than a tenth of a bcrypt comparison (%s)", login, oneBcrypt)
	}
}

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

// Authentication logs never carry a credential: not the Authorization header,
// not an access token sent as the username or password, and not a password.
//
// The backend and the server log on the test's logger, so backend lookups
// are covered too.
//
// Its subtests run in order on one fixture, so only the test runs in
// parallel with others.
func TestAuthDoesNotLogCredentials(t *testing.T) { //nolint:tparallel // the subtests share one fixture and run in order
	t.Parallel()
	is := is.New(t)
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)

	f := newOracleFixtureLogging(t, logger)
	ctx := f.ctx
	f.env.storePasswordHash(t, "stranger", "hunter2-password")
	stranger, err := f.be.User(ctx, "stranger")
	is.NoErr(err)
	jwt := f.mintJWT(t, stranger, "secret")

	const (
		badToken = "ss_bad-token-do-not-log"
		badPass  = "bad-password-do-not-log"
		badJWT   = "eyJhbGciOiJFZERTQSJ9.eyJzdWIiOiJ4In0.bad-sig-do-not-log"
	)
	basic := func(user, pass string) string {
		r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		r.SetBasicAuth(user, pass)
		return r.Header.Get("Authorization")
	}

	for _, tc := range []struct {
		name   string
		header string
		ok     bool
		scheme string
		// secrets must not appear in the logs.
		secrets []string
		// logged must appear in the logs.
		logged []string
	}{
		{"token", "Token " + f.token, true, "token", []string{f.token}, nil},
		{"bad token", "token " + badToken, false, "token", []string{badToken}, nil},
		{"bearer", "Bearer " + jwt, true, "bearer", []string{jwt}, nil},
		{"bad bearer", "Bearer " + badToken, false, "bearer", []string{badToken}, nil},
		// There is no password login, so a stored password is refused.
		{"basic password", basic("stranger", "hunter2-password"), false, "basic", []string{"hunter2-password"}, []string{"username=stranger"}},
		// A valid username is logged on failure, for per-account monitoring.
		{"basic bad password", basic("stranger", badPass), false, "basic", []string{badPass}, []string{"username=stranger"}},
		{"basic token as password", basic("stranger", f.token), true, "basic", []string{f.token}, nil},
		{"basic token as username", basic(f.token, ""), true, "basic", []string{f.token}, nil},
		{"basic bad token as username", basic(badToken, ""), false, "basic", []string{badToken}, nil},
		// GitHub-style clients send the token as the username with a
		// placeholder password; both lookups fail.
		{"basic token with placeholder", basic(badToken, "x-oauth-basic"), false, "basic", []string{badToken}, nil},
		{"basic jwt with placeholder", basic(badJWT, "x-oauth-basic"), false, "basic", []string{badJWT}, nil},
		{"basic valid token with placeholder", basic(f.token, "x-oauth-basic"), true, "basic", []string{f.token}, nil},
		{"basic token as password, empty username", basic("", f.token), true, "basic", []string{f.token}, nil},
		// Another user's name with the token logs in as the token's owner.
		{"basic token with another user's name", basic("owner", f.token), true, "basic", []string{f.token}, nil},
		// A malformed header whose first word is the credential.
		{"malformed", badToken + " x", false, "other", []string{badToken}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			is := is.New(t)
			logs.Reset()
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
			req.Header.Set("Authorization", tc.header)

			user, _, err := f.server.authenticate(req, f.server.requestLogger(req), "secret")
			is.Equal(err == nil && user != nil, tc.ok)

			out := logs.String()
			for _, s := range tc.secrets {
				if strings.Contains(out, s) {
					t.Errorf("auth log contains credential %q:\n%s", s, out)
				}
			}
			for _, s := range tc.logged {
				if !strings.Contains(out, s) {
					t.Errorf("auth log does not contain %q:\n%s", s, out)
				}
			}
			// Whatever follows a known scheme, such as the base64 Basic
			// payload, is a credential too.
			if _, cred, _ := strings.Cut(tc.header, " "); tc.scheme != "other" && strings.Contains(out, cred) {
				t.Errorf("auth log contains the header credential:\n%s", out)
			}
			is.True(strings.Contains(out, "scheme="+tc.scheme))
		})
	}
}

// A failed login is the client's mistake, not a server error, so none of
// them is logged at error level: anyone could otherwise flood the error log.
//
// Its subtests run in order on one fixture, so only the test runs in
// parallel with others.
func TestFailedAuthNotLoggedAsError(t *testing.T) { //nolint:tparallel // the subtests share one fixture and run in order
	t.Parallel()
	is := is.New(t)
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)

	f := newOracleFixtureLogging(t, logger)
	ctx := f.ctx
	stranger, err := f.be.User(ctx, "stranger")
	is.NoErr(err)
	expired, err := f.be.CreateAccessToken(ctx, stranger, "old", time.Now().Add(-time.Hour))
	is.NoErr(err)
	gone, err := f.be.CreateUser(ctx, "gone", proto.UserOptions{})
	is.NoErr(err)
	goneJWT := f.mintJWT(t, gone, "secret")
	is.NoErr(f.be.DeleteUser(ctx, "gone"))

	basic := func(user, pass string) string {
		r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		r.SetBasicAuth(user, pass)
		return r.Header.Get("Authorization")
	}

	for name, header := range map[string]string{
		"bad token":                   "token ss_bad-token",
		"deleted user bearer":         "Bearer " + goneJWT,
		"expired token":               "token " + expired,
		"bad bearer":                  "Bearer ss_bad-token",
		"basic bad password":          basic("stranger", "bad-password"),
		"basic expired token as pass": basic("stranger", expired),
		"basic bad token as username": basic("ss_bad-token", ""),
		"basic expired as username":   basic(expired, ""),
	} {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			logs.Reset()
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
			req.Header.Set("Authorization", header)

			user, _, err := f.server.authenticate(req, f.server.requestLogger(req), "secret")
			is.True(err != nil || user == nil)

			if out := logs.String(); strings.Contains(out, "ERRO") {
				t.Errorf("failed login logged at error level:\n%s", out)
			}

			// The same login through the router, which also logs the
			// authentication result.
			logs.Reset()
			f.do(http.MethodGet, "/secret.git/info/refs?service=git-upload-pack", header)
			if out := logs.String(); strings.Contains(out, "ERRO") {
				t.Errorf("failed login logged at error level by the router:\n%s", out)
			}
		})
	}
}

// A server that could not read its host key when it was built cannot
// verify any token. A bearer request then fails as bad credentials, not as
// an anonymous request, and the server logs the fault at error level.
func TestBearerWithoutHostKey(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)

	f := newOracleFixtureLogging(t, logger)
	ctx := f.ctx
	stranger, err := f.be.User(ctx, "stranger")
	is.NoErr(err)
	bearer := "Bearer " + f.mintJWT(t, stranger, "secret")

	// The server reads its host key once, when it is built.
	is.NoErr(os.Remove(f.env.cfg.SSH.KeyPath))
	s := f.env.server(t)

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	req.Header.Set("Authorization", bearer)
	_, _, err = s.authenticate(req, s.requestLogger(req), "secret")
	is.True(errors.Is(err, errInvalidToken))
	is.True(errors.Is(err, jwk.ErrHostKey))

	logs.Reset()
	req = httptest.NewRequestWithContext(ctx, http.MethodGet, "/secret.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", bearer)
	w := httptest.NewRecorder()
	s.router().ServeHTTP(w, req)
	is.Equal(w.Code, http.StatusForbidden) // anonymous would get 401
	if out := logs.String(); !strings.Contains(out, "ERRO") || !strings.Contains(out, "host key") {
		t.Errorf("missing host key not logged at error level:\n%s", out)
	}
}

// The log of a Basic login with a token names the token's owner, who is the
// user the request runs as, and not the username sent with the token.
func TestBasicAuthLogsTokenOwner(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)

	f := newOracleFixtureLogging(t, logger)
	r := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/", nil)
	r.SetBasicAuth("owner", f.token)

	f.do(http.MethodGet, "/secret.git/info/refs?service=git-upload-pack", r.Header.Get("Authorization"))
	out := logs.String()
	if !strings.Contains(out, "authenticated") || !strings.Contains(out, "username=stranger") {
		t.Errorf("log does not name the token's owner:\n%s", out)
	}
	if strings.Contains(out, "username=owner") {
		t.Errorf("log names the Basic username, not the token's owner:\n%s", out)
	}
}

// While the database is down, credentials cannot be checked. A request that
// carries them is answered 503 with Retry-After and no credentials
// challenge, not as an anonymous request (401 with a challenge, after which
// git discards the stored credential) nor as a failed login. The fault is
// logged at error level.
//
// Its subtests run in order on one fixture, so only the test runs in
// parallel with others.
func TestAuthDatabaseErrorIsUnavailable(t *testing.T) { //nolint:tparallel // the subtests share one fixture and run in order
	t.Parallel()
	is := is.New(t)
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)

	f := newOracleFixtureLogging(t, logger)
	stranger, err := f.be.User(f.ctx, "stranger")
	is.NoErr(err)
	_, err = f.be.CreateRepository(f.ctx, "mine", stranger, proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	bearer := "Bearer " + f.mintJWT(t, stranger, "mine")
	basic := func(user, pass string) string {
		r := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/", nil)
		r.SetBasicAuth(user, pass)
		return r.Header.Get("Authorization")
	}

	is.NoErr(f.env.dbx.Close())

	creds := map[string]string{
		"token":             "Token " + f.token,
		"bearer":            bearer,
		"basic token":       basic("stranger", f.token),
		"basic token as id": basic(f.token, "x-oauth-basic"),
	}
	for name, auth := range creds {
		t.Run(name, func(t *testing.T) {
			for _, path := range []string{
				"/mine.git/info/refs?service=git-upload-pack",
				"/mine.git/info/refs?service=git-receive-pack",
				"/mine?go-get=1",
			} {
				logs.Reset()
				w := f.do(http.MethodGet, path, auth)
				if w.Code != http.StatusServiceUnavailable {
					t.Fatalf("%s: got %d, want 503", path, w.Code)
				}
				if w.Header().Get("Retry-After") == "" {
					t.Errorf("%s: no Retry-After", path)
				}
				if h := w.Header().Get("WWW-Authenticate"); h != "" {
					t.Errorf("%s: credentials challenge %q", path, h)
				}
				// The backend logs its own error too, so look for the
				// web's line at error level.
				if !hasLogLine(logs.String(), "ERRO", "failed to look up credentials") {
					t.Errorf("%s: database error not logged as an error:\n%s", path, logs.String())
				}
			}

			// LFS gets the same answer as JSON.
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mine.git/info/lfs/objects/batch",
				strings.NewReader(`{"operation":"download","objects":[]}`))
			req.Header.Set("Authorization", auth)
			req.Header.Set("Accept", lfs.MediaType)
			req.Header.Set("Content-Type", lfs.MediaType)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, req)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("lfs: got %d, want 503", w.Code)
			}
			is := is.New(t)
			is.True(w.Header().Get("Retry-After") != "")
			is.Equal(w.Header().Get("LFS-Authenticate"), "")
			is.Equal(w.Header().Get("WWW-Authenticate"), "")
			is.Equal(w.Header().Get("Content-Type"), lfs.MediaType)
			var body lfs.ErrorResponse
			is.NoErr(json.Unmarshal(w.Body.Bytes(), &body))
			is.True(body.Message != "")
		})
	}
}

// Only a failed lookup is a server fault: a missing user or an unknown or
// expired token is a failed login.
func TestLookupFailed(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	is.True(!lookupFailed(nil))
	is.True(!lookupFailed(proto.ErrUserNotFound))
	is.True(!lookupFailed(proto.ErrTokenExpired))
	is.True(lookupFailed(errors.New("sql: database is closed")))
	is.True(lookupFailed(context.DeadlineExceeded))
}

// A lookup the client canceled by going away is not logged as a server
// error, though it is still answered as unavailable.
func TestAuthUnavailableCanceled(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	cancel()
	err := authUnavailable(ctx, logger, context.Canceled)
	is.True(errors.Is(err, errAuthUnavailable))
	is.True(errors.Is(err, context.Canceled))
	is.True(!strings.Contains(logs.String(), "ERRO"))

	logs.Reset()
	err = authUnavailable(context.Background(), logger, errors.New("sql: database is closed"))
	is.True(errors.Is(err, errAuthUnavailable))
	is.True(hasLogLine(logs.String(), "ERRO", "failed to look up credentials"))
}

// hasLogLine reports whether out has a line that holds level and msg.
func hasLogLine(out, level, msg string) bool {
	for line := range strings.Lines(out) {
		if strings.Contains(line, level) && strings.Contains(line, msg) {
			return true
		}
	}
	return false
}

// A bearer token whose subject names no valid username is a failed login,
// as for a user who does not exist, not an anonymous request: the user
// lookup refuses such a name with an error that is not
// proto.ErrUserNotFound, and that is not a server fault either.
func TestBearerInvalidSubjectUsername(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)

	f := newOracleFixtureLogging(t, logger)
	for _, subject := range []string{"not_a_user#1", "1user#1", "#1"} {
		claims := jwtTestClaims(f.env, "secret")
		claims.Subject = subject
		bearer := "Bearer " + signJWT(t, f.env, claims)

		req := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/", nil)
		req.Header.Set("Authorization", bearer)
		user, _, err := f.server.authenticate(req, f.server.requestLogger(req), "secret")
		is.True(user == nil)
		if !errors.Is(err, errInvalidToken) {
			t.Errorf("subject %q: err = %v, want %v", subject, err, errInvalidToken)
		}

		logs.Reset()
		w := f.do(http.MethodGet, "/secret.git/info/refs?service=git-upload-pack", bearer)
		if w.Code != http.StatusForbidden { // anonymous would get 401
			t.Errorf("subject %q: got %d, want 403", subject, w.Code)
		}
		if out := logs.String(); strings.Contains(out, "ERRO") {
			t.Errorf("subject %q: logged at error level:\n%s", subject, out)
		}
	}
}

// A repository lookup that fails, here the collaborator lookup for a
// signed-in caller who does not own the repository, is answered 503 with
// Retry-After and no credentials challenge, not 404 as for a repository the
// caller may not read; LFS requests get the LFS error. A go-get lookup that
// fails is answered the same way. The fault is logged at error level.
func TestRepositoryLookupErrorIsUnavailable(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)

	f := newOracleFixtureLogging(t, logger)
	auth := "Token " + f.token
	is.Equal(f.do(http.MethodGet, "/secret.git/info/refs?service=git-upload-pack", auth).Code, http.StatusNotFound)

	dbx := f.env.dbx
	_, err := dbx.ExecContext(f.ctx, "ALTER TABLE collabs RENAME TO collabs_gone")
	is.NoErr(err)

	w := f.do(http.MethodGet, "/secret.git/info/refs?service=git-upload-pack", auth)
	is.Equal(w.Code, http.StatusServiceUnavailable)
	is.Equal(w.Header().Get("Retry-After"), authRetryAfter)
	is.Equal(w.Header().Get("WWW-Authenticate"), "")

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/secret.git/info/lfs/objects/batch",
		strings.NewReader(`{"operation":"download","objects":[]}`))
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", lfs.MediaType)
	req.Header.Set("Content-Type", lfs.MediaType)
	lw := httptest.NewRecorder()
	f.handler.ServeHTTP(lw, req)
	is.Equal(lw.Code, http.StatusServiceUnavailable)
	var body lfs.ErrorResponse
	is.NoErr(json.Unmarshal(lw.Body.Bytes(), &body))
	is.True(body.Message != "")

	is.Equal(f.do(http.MethodGet, "/secret/pkg?go-get=1", auth).Code, http.StatusServiceUnavailable)
	is.True(hasLogLine(logs.String(), "ERRO", "failed to look up repository"))
}

// During an outage a missing name and an existing private one get the same
// answer, 503, for anonymous and signed-in callers: otherwise anyone could
// tell which private names exist. With the settings unreadable an anonymous
// request is 503 too, not asked for credentials.
func TestOutageAnswersMissingAndExistingAlike(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"repos", "settings"} {
		t.Run(table, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			f := newOracleFixture(t)
			dbx := f.env.dbx
			_, err := dbx.ExecContext(f.ctx, "ALTER TABLE "+table+" RENAME TO "+table+"_gone")
			is.NoErr(err)
			for _, auth := range []string{"", "Token " + f.token} {
				for _, name := range []string{"secret", "ghost"} {
					w := f.do(http.MethodGet, "/"+name+".git/info/refs?service=git-upload-pack", auth)
					is.Equal(w.Code, http.StatusServiceUnavailable)
					is.Equal(w.Header().Get("WWW-Authenticate"), "")
				}
			}
		})
	}
}

// A lookup the client canceled is logged at Debug, not as a server error,
// and still answered as unavailable.
func TestLookupUnavailableCanceled(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/repo.git/info/refs", nil)
	w := httptest.NewRecorder()
	lookupUnavailable(w, r, logger, "repo", context.Canceled, false)
	is.Equal(w.Code, http.StatusServiceUnavailable)
	is.True(hasLogLine(logs.String(), "DEBU", "repository lookup canceled"))
	is.True(!strings.Contains(logs.String(), "ERRO"))
}

// TestParseJWTHostKeyTypes checks that tokens minted with the server's host
// key verify whatever the key type is, so an RSA or ECDSA host key (as
// suggested for older SSH clients) works with `soft jwt` and git-lfs over
// SSH.
func TestParseJWTHostKeyTypes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		alg  string
		opts []keygen.Option
	}{
		{"ed25519", "EdDSA", []keygen.Option{keygen.WithKeyType(keygen.Ed25519)}},
		{"rsa", "RS256", []keygen.Option{keygen.WithKeyType(keygen.RSA), keygen.WithBitSize(2048)}},
		{"ecdsa-p256", "ES256", []keygen.Option{keygen.WithKeyType(keygen.ECDSA), keygen.WithEllipticCurve(elliptic.P256())}},
		{"ecdsa-p384", "ES384", []keygen.Option{keygen.WithKeyType(keygen.ECDSA), keygen.WithEllipticCurve(elliptic.P384())}},
		{"ecdsa-p521", "ES512", []keygen.Option{keygen.WithKeyType(keygen.ECDSA), keygen.WithEllipticCurve(elliptic.P521())}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := jwtTestEnv(t, tc.opts...)
			token := signJWT(t, e, jwtTestClaims(e, "repo"))

			claims, err := e.parseJWT(t, token, []string{"repo"})
			if err != nil {
				t.Fatalf("parseJWT: %v", err)
			}
			if claims.Subject != "user#1" {
				t.Errorf("subject = %q, want %q", claims.Subject, "user#1")
			}

			parsed, _, err := jwt.NewParser().ParseUnverified(token, &jwt.RegisteredClaims{})
			if err != nil {
				t.Fatalf("ParseUnverified: %v", err)
			}
			if got := parsed.Method.Alg(); got != tc.alg {
				t.Errorf("alg = %q, want %q", got, tc.alg)
			}
		})
	}
}

// TestParseJWTRejectsOtherAlgorithms checks that a token is only accepted
// with the algorithm of the host key type, so a token signed with another
// key or algorithm does not verify.
func TestParseJWTRejectsOtherAlgorithms(t *testing.T) {
	t.Parallel()
	rsaEnv := jwtTestEnv(t, keygen.WithKeyType(keygen.RSA), keygen.WithBitSize(2048))
	edEnv := jwtTestEnv(t, keygen.WithKeyType(keygen.Ed25519))

	if _, err := rsaEnv.parseJWT(t, signJWT(t, edEnv, jwtTestClaims(edEnv, "repo")), []string{"repo"}); err == nil {
		t.Error("parseJWT accepted an EdDSA token for an RSA host key")
	}
	if _, err := edEnv.parseJWT(t, signJWT(t, rsaEnv, jwtTestClaims(rsaEnv, "repo")), []string{"repo"}); err == nil {
		t.Error("parseJWT accepted an RS256 token for an Ed25519 host key")
	}

	// An RSA-PSS token from the same RSA key is not the expected RS256.
	kp, err := config.KeyPair(rsaEnv.cfg)
	if err != nil {
		t.Fatalf("KeyPair: %v", err)
	}
	pss, err := jwt.NewWithClaims(jwt.SigningMethodPS256, jwtTestClaims(rsaEnv, "repo")).SignedString(kp.PrivateKey())
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	if _, err := rsaEnv.parseJWT(t, pss, []string{"repo"}); err == nil {
		t.Error("parseJWT accepted a PS256 token for an RSA host key")
	}
}

// jwtTestEnv returns an env with only a config, whose host key is made with
// opts.
func jwtTestEnv(t *testing.T, opts ...keygen.Option) *testEnv {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	e := &testEnv{cfg: cfg}
	e.writeHostKey(t, opts...)
	return e
}

// parseJWT is server.parseJWT on a server with e's host key.
func (e *testEnv) parseJWT(t *testing.T, bearer string, repos []string) (*jwt.RegisteredClaims, error) {
	t.Helper()
	s := newServer(e.cfg, nil, nil, nil, e.tokens(t), nil)
	return s.parseJWT(s.logger, bearer, repos)
}

// jwtTestClaims returns valid claims for user#1, issued by e's server, for
// the repositories in audience.
func jwtTestClaims(e *testEnv, audience ...string) jwt.RegisteredClaims {
	now := time.Now()
	return jwt.RegisteredClaims{
		Subject:   "user#1",
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		NotBefore: jwt.NewNumericDate(now),
		IssuedAt:  jwt.NewNumericDate(now),
		Issuer:    e.cfg.HTTP.PublicURL,
		Audience:  audience,
	}
}

// TestBearerTokenWithKeyID checks a token minted before the "kid" header was
// dropped, which carries one, still authenticates its user until it expires.
func TestBearerTokenWithKeyID(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	kp, err := config.KeyPair(f.env.cfg)
	if err != nil {
		t.Fatalf("KeyPair: %v", err)
	}
	claims := jwtTestClaims(f.env, "secret")
	claims.Subject = fmt.Sprintf("%s#%d", f.owner.Username(), f.owner.ID())
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	// An RFC 7638 thumbprint, as tokens minted with a key ID carry.
	token.Header["kid"] = "pXIvChkPX0Nik0T-8ADSGgxzCbqOXKyjqTUu4M4TYy0"
	signed, err := token.SignedString(kp.PrivateKey())
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}

	const path = "/secret.git/info/refs?service=git-upload-pack"
	if got := f.do(http.MethodGet, path, "Bearer "+signed).Code; got != http.StatusOK {
		t.Errorf("status = %d, want %d", got, http.StatusOK)
	}
	// Without the token the private repository is not readable.
	if got := f.do(http.MethodGet, path, "").Code; got != http.StatusUnauthorized {
		t.Errorf("anonymous status = %d, want %d", got, http.StatusUnauthorized)
	}
}
