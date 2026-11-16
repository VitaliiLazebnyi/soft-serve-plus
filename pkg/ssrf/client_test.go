package ssrf

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests drive the secure client through its transport, as webhooks and
// the LFS client use it. Names resolve through fakeResolver and connection
// attempts go through stubConnect, so nothing reaches DNS or leaves the
// machine. A refused destination must fail with ErrPrivateIP, which only the
// SSRF checks produce: a dial that merely fails would not pass.

// blockedLiterals are addresses of every blocked kind, as dial targets.
var blockedLiterals = []string{
	"127.0.0.1:80",                // loopback
	"127.1.2.3:80",                // loopback, not .1
	"[::1]:80",                    // IPv6 loopback
	"10.0.0.1:80",                 // RFC1918
	"172.16.0.1:80",               // RFC1918
	"192.168.1.1:80",              // RFC1918
	"169.254.169.254:80",          // link-local, cloud metadata
	"[fe80::1]:80",                // IPv6 link-local
	"100.64.0.1:80",               // CGNAT
	"100.100.100.200:80",          // Alibaba metadata, inside CGNAT
	"224.0.0.1:80",                // multicast
	"[ff02::1]:80",                // IPv6 multicast
	"0.0.0.0:80",                  // unspecified
	"[::]:80",                     // IPv6 unspecified
	"[::ffff:127.0.0.1]:80",       // IPv4-mapped loopback
	"[::ffff:169.254.169.254]:80", // IPv4-mapped metadata
	"[fc00::1]:80",                // unique local
	"[fd00:ec2::254]:80",          // AWS metadata over IPv6
	"[64:ff9b::7f00:1]:80",        // NAT64 of loopback
	"[2002:7f00:1::1]:80",         // 6to4 of loopback
	"240.0.0.1:80",                // reserved
}

func secureDialFunc(t *testing.T, c *http.Client) func(ctx context.Context, network, addr string) (net.Conn, error) {
	t.Helper()
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		t.Fatalf("transport is %T, want *http.Transport with DialContext", c.Transport)
	}
	return tr.DialContext
}

func TestSecureClientRefusesBlockedLiterals(t *testing.T) {
	fakeResolver(t, nil)
	rec := stubConnect(t)
	dial := secureDialFunc(t, NewSecureClient())
	for _, addr := range blockedLiterals {
		conn, err := dial(context.Background(), "tcp", addr)
		if conn != nil {
			_ = conn.Close()
		}
		if !errors.Is(err, ErrPrivateIP) {
			t.Errorf("dial %s: err = %v, want ErrPrivateIP", addr, err)
		}
	}
	if got := rec.addrs(); len(got) != 0 {
		t.Errorf("attempted %v, want nothing", got)
	}
}

// TestSecureClientRefusesPrivateNames covers names whose every answer is
// blocked, through a real request to a loopback listener the client must
// not reach.
func TestSecureClientRefusesPrivateNames(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	table := map[string][]string{}
	var names []string
	for _, addr := range blockedLiterals {
		host, _, _ := net.SplitHostPort(addr)
		name := "n" + strings.NewReplacer(".", "-", ":", "x").Replace(host) + ".test"
		table[name] = []string{host}
		names = append(names, name)
	}
	table["two-private.test"] = []string{"10.0.0.1", "127.0.0.1"}
	names = append(names, "two-private.test")
	fakeResolver(t, table)
	rec := stubConnect(t, srv.Listener.Addr().String())

	client := NewSecureClient()
	for _, name := range names {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+name+":"+port+"/hook", nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.Do(req)
		if res != nil {
			_ = res.Body.Close()
		}
		if !errors.Is(err, ErrPrivateIP) {
			t.Errorf("POST to %s (%v): err = %v, want ErrPrivateIP", name, table[name], err)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("listener reached %d times", hits.Load())
	}
	if got := rec.addrs(); len(got) != 0 {
		t.Errorf("attempted %v, want nothing", got)
	}
}

// TestSecureClientDialsPublicAddresses checks that public destinations get
// as far as a connection attempt, to the address the name resolved to.
func TestSecureClientDialsPublicAddresses(t *testing.T) {
	fakeResolver(t, map[string][]string{
		"public.test":  {"140.82.112.3"},
		"public6.test": {"2606:4700::1111"},
	})
	for addr, want := range map[string]string{
		"140.82.112.3:443":      "140.82.112.3:443",
		"[2606:4700::1111]:443": "[2606:4700::1111]:443",
		"public.test:443":       "140.82.112.3:443",
		"public6.test:443":      "[2606:4700::1111]:443",
	} {
		rec := stubConnect(t)
		_, err := secureDialFunc(t, NewSecureClient())(context.Background(), "tcp", addr)
		if err == nil || errors.Is(err, ErrPrivateIP) {
			t.Errorf("dial %s: err = %v, want the stubbed refusal", addr, err)
		}
		if got := rec.addrs(); !slices.Equal(got, []string{want}) {
			t.Errorf("dial %s attempted %v, want [%s]", addr, got, want)
		}
	}
}

// TestSecureClientFailsOver checks that a failed attempt moves on to the
// name's next address. Both answers are the allowlisted listener, so the
// order the resolver's answers are tried in does not matter; the first
// attempt is refused.
func TestSecureClientFailsOver(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	fakeResolver(t, map[string][]string{"twice.test": {"127.0.0.1", "127.0.0.1"}})
	rec := stubConnect(t, srv.Listener.Addr().String())
	rec.failFirst = 1

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://twice.test:"+port+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewSecureClientAllowing(mustAllowedHosts(t, "127.0.0.1")).Do(req)
	if err != nil {
		t.Fatalf("GET: %v, want the second address reached", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d", res.StatusCode)
	}
	if got := rec.addrs(); len(got) != 2 {
		t.Errorf("attempted %v, want two attempts", got)
	}
}

// TestSecureClientAllowlistAtDial checks the allowlist where the connection
// is made: an entry opens what it names, on its port, and never the
// metadata addresses unless it targets them.
func TestSecureClientAllowlistAtDial(t *testing.T) {
	fakeResolver(t, map[string][]string{
		"hooks.internal": {"10.1.2.3"},
		"meta.internal":  {"169.254.169.254"},
	})
	tests := []struct {
		allowed []string
		addr    string
		permit  bool
	}{
		{[]string{"10.0.0.0/8"}, "10.1.2.3:80", true},
		{[]string{"10.1.2.3:8080"}, "10.1.2.3:8080", true},
		{[]string{"10.1.2.3:8080"}, "10.1.2.3:80", false},
		{[]string{"hooks.internal"}, "hooks.internal:80", true},
		{[]string{"hooks.internal:443"}, "hooks.internal:80", false},
		{[]string{"other.internal"}, "hooks.internal:80", false},
		{[]string{"meta.internal"}, "meta.internal:80", false},
		{[]string{"0.0.0.0/0"}, "169.254.169.254:80", false},
		{[]string{"169.254.169.254"}, "169.254.169.254:80", true},
		{[]string{"100.64.0.0/10"}, "100.100.100.200:80", false},
		{[]string{"fc00::/7"}, "[fd00:ec2::254]:80", false},
		{[]string{"fd00:ec2::254"}, "[fd00:ec2::254]:80", true},
		{[]string{"10.0.0.0/8"}, "127.0.0.1:80", false},
	}
	for _, tt := range tests {
		rec := stubConnect(t)
		_, err := secureDialFunc(t, NewSecureClientAllowing(mustAllowedHosts(t, tt.allowed...)))(context.Background(), "tcp", tt.addr)
		attempted := len(rec.addrs()) > 0
		if tt.permit {
			if !attempted || errors.Is(err, ErrPrivateIP) {
				t.Errorf("allowed %v, dial %s: attempted %v, err %v; want an attempt", tt.allowed, tt.addr, rec.addrs(), err)
			}
			continue
		}
		if attempted || !errors.Is(err, ErrPrivateIP) {
			t.Errorf("allowed %v, dial %s: attempted %v, err %v; want ErrPrivateIP", tt.allowed, tt.addr, rec.addrs(), err)
		}
	}
}

// TestSecureClientHidesResolverErrors checks that a failed lookup reports the
// host only: the error is stored with the webhook delivery, and the resolver
// error names the server's DNS resolver.
func TestSecureClientHidesResolverErrors(t *testing.T) {
	fakeResolver(t, nil)
	rec := stubConnect(t)
	_, err := secureDialFunc(t, NewSecureClient())(context.Background(), "tcp", "missing.test:443")
	if err == nil {
		t.Fatal("dial to an unresolvable name succeeded")
	}
	if want := "DNS resolution failed for host missing.test"; err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
	if errors.Is(err, ErrPrivateIP) {
		t.Errorf("err = %v, want it not to be ErrPrivateIP", err)
	}
	if got := rec.addrs(); len(got) != 0 {
		t.Errorf("attempted %v", got)
	}
}

// TestSecureClientCancelled checks that a cancelled dial says so and
// attempts nothing.
func TestSecureClientCancelled(t *testing.T) {
	fakeResolver(t, map[string][]string{"public.test": {"140.82.112.3"}})
	rec := stubConnect(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, addr := range []string{"public.test:443", "140.82.112.3:443"} {
		_, err := secureDialFunc(t, NewSecureClient())(ctx, "tcp", addr)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("dial %s: err = %v, want context.Canceled", addr, err)
		}
	}
	if got := rec.addrs(); len(got) != 0 {
		t.Errorf("attempted %v after cancellation", got)
	}
}

// TestSecureClientDialBudget checks that every attempt runs under the dial
// budget, not the request's longer deadline: net/http dials with a context
// that does not carry the request deadline.
func TestSecureClientDialUnderBudget(t *testing.T) {
	fakeResolver(t, map[string][]string{"public.test": {"140.82.112.3", "140.82.112.4"}})
	rec := stubConnect(t)
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://public.test/hook", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewSecureClient().Do(req)
	if res != nil {
		_ = res.Body.Close()
	}
	if err == nil {
		t.Fatal("request succeeded with every attempt refused")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.deadlines) == 0 {
		t.Fatal("no attempt made")
	}
	// Generous slack: this checks which budget applies (10s against an
	// hour), not how fast the machine is.
	for i, d := range rec.deadlines {
		if d.IsZero() || d.Sub(start) > dialTimeout+time.Minute {
			t.Errorf("attempt %d deadline %v after start, want within the %v dial budget", i, d.Sub(start), dialTimeout)
		}
	}
}
