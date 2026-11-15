package web

import (
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// A Basic login whose password is not an access token is refused without a
// bcrypt comparison: many of them take less time than a few comparisons.
// There is no password login, so there is nothing to compare against.
func TestBasicNonTokenLoginRunsNoBcrypt(t *testing.T) {
	f := newOracleFixture(t)

	// The cost of the comparisons a password check used to run.
	const bcrypts = 8
	start := time.Now()
	for range bcrypts {
		backend.VerifyPassword("guess", "")
	}
	budget := time.Since(start)

	const attempts = 64
	start = time.Now()
	for range attempts {
		req := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/", nil)
		// "stranger" is a registered user, "nobody" is not; neither has a
		// way to log in with a password.
		req.SetBasicAuth("stranger", "guess")
		if user, _, err := authenticate(req); user != nil || !badCredentials(err) {
			t.Fatalf("authenticate => %v, %v, want bad credentials", user, err)
		}
		req.SetBasicAuth("nobody", "guess")
		if user, _, err := authenticate(req); user != nil || !badCredentials(err) {
			t.Fatalf("authenticate => %v, %v, want bad credentials", user, err)
		}
	}
	if took := time.Since(start); took > budget {
		t.Errorf("%d failed Basic logins took %s, more than %d bcrypt comparisons (%s)", 2*attempts, took, bcrypts, budget)
	}
}

// A failed Basic login holds its request slot for a small part of one bcrypt
// comparison. Each such login used to run a bcrypt against a dummy hash,
// queued per client address and behind a global semaphore, while holding a
// slot, so sixteen looping clients from one address kept all eight slots of
// max_connections 8 and every anonymous fetch answered 503. Waits that only
// happen under contention, such as a per-client queue or a semaphore around
// cheap work, are deliberately not tested.
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
	ctx, be, _ := newLFSTestContext(t)
	if _, err := be.CreateRepository(ctx, "repo", nil, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	// A limit puts the slot channel in the request path, as in production;
	// a slot a login leaked would make the ninth request answer 503.
	config.FromContext(ctx).HTTP.MaxConnections = 8
	router := testRouter(t, ctx)

	// One bcrypt comparison, as each login used to run. The first call also
	// makes the dummy hash, so take the faster of two.
	bcrypt := time.Duration(math.MaxInt64)
	for range 2 {
		start := time.Now()
		backend.VerifyPassword("guess", "")
		bcrypt = min(bcrypt, time.Since(start))
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
	t.Logf("fastest failed login %s, fastest bcrypt %s", login, bcrypt)
	if login*10 > bcrypt {
		t.Errorf("a failed Basic login held its request slot for %s, more than a tenth of a bcrypt comparison (%s)", login, bcrypt)
	}
}
