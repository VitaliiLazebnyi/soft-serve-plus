package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

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
// have passwords. An access token sent as the password needs none, and
// neither does a name that cannot be a username.
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
		{"invalid username", "ss_not-a-user", "wrong", false, 0},
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

// One client address runs one password check at a time: while it has one
// running, its next request waits without running bcrypt, and another
// address is not held up.
func TestBasicAuthPasswordCheckIsPerClient(t *testing.T) {
	f := newOracleFixture(t)
	is := is.New(t)
	is.NoErr(f.be.SetPassword(f.ctx, "stranger", "hunter2-password"))
	n := countVerifies(t)

	prev := clientChecks
	clientChecks = newClientLimiter(maxClientPending)
	t.Cleanup(func() { clientChecks = prev })
	release, err := clientChecks.acquire(f.ctx, "192.0.2.1")
	is.NoErr(err)

	login := func(addr string) error {
		req := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/", nil)
		req.RemoteAddr = addr
		req.SetBasicAuth("stranger", "hunter2-password")
		_, err := authenticate(req, "")
		return err
	}

	done := make(chan error, 1)
	go func() { done <- login("192.0.2.1:1234") }()
	waitPending(t, clientChecks, "192.0.2.1", 2)
	is.Equal(*n, 0) // the waiting check holds no slot, so it cannot verify yet

	is.NoErr(login("198.51.100.7:1234"))
	is.Equal(*n, 1)

	release()
	is.NoErr(<-done)
	is.Equal(*n, 2)
}

// waitPending waits until key has want checks running or waiting in l.
func waitPending(t *testing.T, l *clientLimiter, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		l.mu.Lock()
		var pending int
		if slot := l.keys[key]; slot != nil {
			pending = slot.pending
		}
		l.mu.Unlock()
		if pending == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s has %d checks pending, want %d", key, pending, want)
		}
		runtime.Gosched()
	}
}

// A client with maxPending checks running or waiting is refused at once,
// and its key is dropped once nothing holds it.
func TestClientLimiter(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	l := newClientLimiter(2)
	ctx := t.Context()

	release, err := l.acquire(ctx, "a")
	is.NoErr(err)

	waited := make(chan func())
	go func() {
		r, err := l.acquire(ctx, "a")
		if err != nil {
			close(waited)
			return
		}
		waited <- r
	}()

	waitPending(t, l, "a", 2)

	_, err = l.acquire(ctx, "a")
	is.Equal(err, errClientBusy)

	other, err := l.acquire(ctx, "b")
	is.NoErr(err)
	other()

	release()
	second, ok := <-waited
	is.True(ok)
	second()

	l.mu.Lock()
	is.Equal(len(l.keys), 0)
	l.mu.Unlock()
}
