package web

import (
	"context"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
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

// Failed Basic logins do not hold request slots: with max_connections 8,
// sixteen clients looping on "alice:guess" from one address do not make
// anonymous fetches answer 503. Each such login used to run a bcrypt, one at
// a time per address and behind a global semaphore, while holding a slot.
func TestBasicNonTokenLoginsDoNotStarveFetches(t *testing.T) {
	addr := newLimitsServer(t, func(c *config.HTTPConfig) {
		c.MaxConnections = 8
	})
	url := "http://" + addr + "/repo.git/info/refs?service=git-upload-pack"
	// Keep connections alive, so the loop measures request slots and not
	// the loopback listen queue.
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{MaxIdleConnsPerHost: 32},
	}
	t.Cleanup(client.CloseIdleConnections)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for range 16 {
		wg.Go(func() {
			for ctx.Err() == nil {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
				if err != nil {
					return
				}
				req.SetBasicAuth("alice", "guess")
				resp, err := client.Do(req)
				if err != nil {
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				mu.Lock()
				codes[resp.StatusCode]++
				mu.Unlock()
				// A refusal that costs nothing leaves its slot at once.
				// The pause keeps the clients from holding all eight
				// slots by request rate alone, which any request could.
				// It is random, so the clients do not move in step.
				time.Sleep(time.Duration(10+rand.IntN(50)) * time.Millisecond)
			}
		})
	}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	// Let the attackers fill whatever they can hold.
	time.Sleep(200 * time.Millisecond)
	const probes = 9
	ok := 0
	for range probes {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			ok++
		} else {
			t.Logf("anonymous fetch answered %d", resp.StatusCode)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()

	if ok != probes {
		t.Errorf("%d of %d anonymous fetches succeeded", ok, probes)
	}
	mu.Lock()
	defer mu.Unlock()
	if codes[http.StatusForbidden] == 0 {
		t.Errorf("failed logins got %v, want 403 for bad credentials", codes)
	}
}
