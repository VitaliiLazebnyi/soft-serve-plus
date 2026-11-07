package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matryer/is"
	"golang.org/x/sync/semaphore"
)

// countVerifies replaces verifyPassword with a counting wrapper for the
// test. It swaps a package variable, so callers must not run in parallel.
func countVerifies(t *testing.T) *int {
	t.Helper()
	var n int
	prev := verifyPassword
	verifyPassword = func(password, hash string) bool {
		n++
		return prev(password, hash)
	}
	t.Cleanup(func() { verifyPassword = prev })
	return &n
}

// HTTP Basic login does one bcrypt comparison whether or not the user exists
// or has a password, so the response time does not reveal which accounts
// have passwords. An access token sent as the password needs none.
func TestBasicAuthPasswordCheckIsUniform(t *testing.T) {
	f := newOracleFixture(t)
	is := is.New(t)
	is.NoErr(f.be.SetPassword(f.ctx, "stranger", "hunter2-password"))

	for _, tc := range []struct {
		name, user, pass string
		ok               bool
		verifies         int
	}{
		{"password", "stranger", "hunter2-password", true, 1},
		{"wrong password", "stranger", "wrong", false, 1},
		{"user without password", "owner", "wrong", false, 1},
		{"missing user", "nobody", "wrong", false, 1},
		{"invalid username", "ss_not-a-user", "wrong", false, 1},
		{"token as password", "stranger", f.token, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			is := is.New(t)
			n := countVerifies(t)
			req := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/", nil)
			req.SetBasicAuth(tc.user, tc.pass)
			user, err := authenticate(req, "")
			is.Equal(err == nil && user != nil, tc.ok)
			is.Equal(*n, tc.verifies)
		})
	}
}

// Password checks wait for a free slot, and a request that ends while
// waiting fails without running bcrypt.
func TestBasicAuthPasswordCheckIsBounded(t *testing.T) {
	f := newOracleFixture(t)
	is := is.New(t)
	is.NoErr(f.be.SetPassword(f.ctx, "stranger", "hunter2-password"))
	n := countVerifies(t)

	prev := passwordChecks
	full := semaphore.NewWeighted(1)
	is.True(full.TryAcquire(1))
	passwordChecks = full
	t.Cleanup(func() { passwordChecks = prev })

	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	req.SetBasicAuth("stranger", "hunter2-password")
	_, err := authenticate(req, "")
	is.Equal(err, ErrInvalidPassword)
	is.Equal(*n, 0)
}
