package ssrf

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/internal/testdial"
)

// TestNewSecureClientBlocksPrivateIPs dials through the client's transport.
// A blocked row must fail with ErrPrivateIP and never reach a connection
// attempt: a dial to such an address fails anyway on most machines, so an
// error alone would pass without the protection. The public row's attempt
// is stubbed, so nothing leaves the machine.
func TestNewSecureClientBlocksPrivateIPs(t *testing.T) {
	fakeResolver(t, nil)
	transport := NewSecureClient().Transport.(*http.Transport)

	tests := []struct {
		name    string
		addr    string
		blocked bool
	}{
		{"block loopback", "127.0.0.1:80", true},
		{"block private 10.x", "10.0.0.1:80", true},
		{"block link-local", "169.254.169.254:80", true},
		{"block CGNAT", "100.64.0.1:80", true},
		{"allow public IP", "140.82.112.3:80", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := stubConnect(t)
			conn, err := transport.DialContext(context.Background(), "tcp", tt.addr)
			if conn != nil {
				_ = conn.Close()
			}

			if tt.blocked {
				if !errors.Is(err, ErrPrivateIP) {
					t.Errorf("dial %s: err = %v, want ErrPrivateIP", tt.addr, err)
				}
				if got := rec.addrs(); len(got) != 0 {
					t.Errorf("dial %s attempted %v", tt.addr, got)
				}
				return
			}
			if !errors.Is(err, errStubRefused) {
				t.Errorf("dial %s: err = %v, want the stubbed attempt's error", tt.addr, err)
			}
			if got := rec.addrs(); len(got) != 1 || got[0] != tt.addr {
				t.Errorf("dial %s attempted %v, want [%s]", tt.addr, got, tt.addr)
			}
		})
	}
}

// TestNewSecureClientBlocksPrivateHostnames exercises the hostname path: the
// name resolves to loopback, which is refused before any attempt.
func TestNewSecureClientBlocksPrivateHostnames(t *testing.T) {
	fakeResolver(t, map[string][]string{"loopback.test": {"127.0.0.1", "::1"}})
	rec := stubConnect(t)
	transport := NewSecureClient().Transport.(*http.Transport)

	conn, err := transport.DialContext(context.Background(), "tcp", "loopback.test:80")
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, ErrPrivateIP) {
		t.Errorf("expected ErrPrivateIP for hostname resolving to loopback, got: %v", err)
	}
	if got := rec.addrs(); len(got) != 0 {
		t.Errorf("attempted %v", got)
	}
}

// A name that does not resolve fails as a lookup, not as ErrPrivateIP.
func TestNewSecureClientUnresolvableNotErrPrivateIP(t *testing.T) {
	fakeResolver(t, nil)
	stubConnect(t)
	transport := NewSecureClient().Transport.(*http.Transport)

	conn, err := transport.DialContext(context.Background(), "tcp", "not-an-ip.test:80")
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("expected error for a name that does not resolve, got none")
	}
	if errors.Is(err, ErrPrivateIP) {
		t.Errorf("lookup failure should not wrap ErrPrivateIP, got: %v", err)
	}
}

// TestNewSecureClientBlocksRedirects checks that a redirect is returned, not
// followed. The listener is allowlisted so the first request reaches it; the
// redirect names a public address, which no connection is attempted to.
func TestNewSecureClientBlocksRedirects(t *testing.T) {
	redirectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://140.82.112.3:8080/safe", http.StatusFound)
	}))
	defer redirectServer.Close()
	fakeResolver(t, nil)
	rec := stubConnect(t, redirectServer.Listener.Addr().String())

	client := NewSecureClientAllowing(mustAllowedHosts(t, "127.0.0.1"))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, redirectServer.URL, nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusFound {
		t.Errorf("Expected redirect response (302), got %d", resp.StatusCode)
	}
	if got := rec.addrs(); len(got) != 1 || got[0] != redirectServer.Listener.Addr().String() {
		t.Errorf("attempted %v, want only the listener", got)
	}
}

// Every secure client bounds the wait for response headers on its transport,
// with or without an allowlist.
func TestNewSecureClientBoundsResponseHeaders(t *testing.T) {
	for name, c := range map[string]*http.Client{
		"plain":    NewSecureClient(),
		"allowing": NewSecureClientAllowing(mustAllowedHosts(t, "10.0.0.0/8")),
	} {
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s: transport is %T, want *http.Transport", name, c.Transport)
		}
		if tr.ResponseHeaderTimeout != ResponseHeaderTimeout {
			t.Errorf("%s: ResponseHeaderTimeout = %v, want %v", name, tr.ResponseHeaderTimeout, ResponseHeaderTimeout)
		}
	}
}

func TestIsPrivateOrInternal(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		// Public
		{"8.8.8.8", false},
		{"2001:4860:4860::8888", false},

		// Loopback
		{"127.0.0.1", true},
		{"::1", true},

		// Private ranges
		{"10.0.0.1", true},
		{"192.168.1.1", true},
		{"172.16.0.1", true},

		// Link-local (cloud metadata)
		{"169.254.169.254", true},

		// CGNAT boundaries
		{"100.64.0.1", true},
		{"100.127.255.255", true},

		// IPv6-mapped IPv4 (bypass vector the old webhook code missed)
		{"::ffff:127.0.0.1", true},
		{"::ffff:169.254.169.254", true},
		{"::ffff:8.8.8.8", false},

		// Reserved
		{"0.0.0.0", true},
		{"240.0.0.1", true},

		// IPv6 transition addresses. Each embeds an IPv4 address inside an
		// IPv6 one, so they look public to the IPv6 checks while naming an
		// internal host once a relay decodes them. The whole prefix is
		// blocked, so the embedded address does not matter: encodings of
		// public addresses are rejected too.
		{"2002:7f00:0001::", true},                        // 6to4 -> 127.0.0.1
		{"2002:a9fe:a9fe::", true},                        // 6to4 -> 169.254.169.254
		{"2002:0a00:0001::", true},                        // 6to4 -> 10.0.0.1
		{"2002:0808:0808::", true},                        // 6to4 -> 8.8.8.8, still blocked
		{"64:ff9b::7f00:1", true},                         // NAT64 well-known -> 127.0.0.1
		{"64:ff9b::a9fe:a9fe", true},                      // NAT64 well-known -> 169.254.169.254
		{"64:ff9b:1::7f00:1", true},                       // NAT64 local-use -> 127.0.0.1
		{"2001:0000:4136:e378:8000:63bf:8001:5601", true}, // Teredo
		{"::7f00:1", true},                                // IPv4-compatible -> 127.0.0.1
		{"192.88.99.1", true},                             // 6to4 relay anycast
		{"::ffff:0:7f00:1", true},                         // IPv4-translated -> 127.0.0.1
		{"::ffff:0:a9fe:a9fe", true},                      // IPv4-translated -> 169.254.169.254
		{"::ffff:0:808:808", true},                        // IPv4-translated -> 8.8.8.8, still blocked

		// Other special-purpose IPv6 ranges no request should reach.
		{"2001:2::1", true},     // benchmarking
		{"2001:10::1", true},    // ORCHID
		{"2001:2f::1", true},    // ORCHIDv2
		{"3fff::1", true},       // documentation
		{"3fff:fff::1", true},   // documentation, end of the range
		{"5f00::1", true},       // SRv6 SIDs
		{"2001:3::1", false},    // next to benchmarking, public
		{"2001:30::1", false},   // past ORCHIDv2, public
		{"3fff:1000::1", false}, // past the documentation range

		// Public IPv6 must stay reachable: the transition prefixes are narrow
		// and must not swallow ordinary global unicast.
		{"2606:4700:4700::1111", false},
		{"2a00:1450:4001::1", false},
		{"2400:cb00::1", false},
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("failed to parse IP: %s", tt.ip)
			}
			if got := isPrivateOrInternal(ip); got != tt.want {
				t.Errorf("isPrivateOrInternal(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

// TestStdlibCoverageParity guards the switch from the standard library's
// Is*() helpers to an explicit prefix table. The helpers covered these ranges
// implicitly, so the table has to cover them too or the refactor silently
// narrows the guard.
func TestStdlibCoverageParity(t *testing.T) {
	for _, ip := range []string{
		// Loopback.
		"127.0.0.1", "127.255.255.254", "::1",
		// Private.
		"10.255.255.255", "172.31.255.255", "192.168.255.255", "fd00::1", "fdff::1",
		// Link-local unicast.
		"169.254.1.1", "fe80::1", "febf::1",
		// Multicast, including link-local and interface-local.
		"224.0.0.1", "239.255.255.255", "ff00::1", "ff01::1", "ff02::1", "ffff::1",
		// Unspecified.
		"0.0.0.0", "::",
	} {
		t.Run(ip, func(t *testing.T) {
			if !isPrivateOrInternal(net.ParseIP(ip)) {
				t.Errorf("isPrivateOrInternal(%s) = false, want true", ip)
			}
		})
	}
}

func TestValidateURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
		errType error
	}{
		// Valid
		{"valid https", "https://1.1.1.1/webhook", false, nil},

		// Scheme validation
		{"ftp scheme", "ftp://example.com/webhook", true, ErrInvalidScheme},
		{"no scheme", "example.com/webhook", true, ErrInvalidScheme},

		// Localhost
		{"localhost", "http://localhost/webhook", true, ErrPrivateIP},
		{"subdomain.localhost", "http://test.localhost/webhook", true, ErrPrivateIP},

		// IP-based blocking (one per category -- range coverage is in TestIsPrivateOrInternal)
		{"loopback IP", "http://127.0.0.1/webhook", true, ErrPrivateIP},
		{"metadata IP", "http://169.254.169.254/latest/meta-data/", true, ErrPrivateIP},

		// Invalid URLs
		{"empty", "", true, ErrInvalidURL},
		{"missing hostname", "http:///webhook", true, ErrInvalidURL},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateURL(t.Context(), tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errType != nil {
				if !errors.Is(err, tt.errType) {
					t.Errorf("ValidateURL(%q) error = %v, want error type %v", tt.url, err, tt.errType)
				}
			}
		})
	}
}

func TestIsLocalhost(t *testing.T) {
	tests := []struct {
		hostname string
		want     bool
	}{
		{"localhost", true},
		{"LOCALHOST", true},
		{"localhost.", true},
		{"localhost.localdomain.", true},
		{"test.localhost", true},
		{"test.localhost.", true},
		{"example.com", false},
		{"localhost.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.hostname, func(t *testing.T) {
			if got := isLocalhost(tt.hostname); got != tt.want {
				t.Errorf("isLocalhost(%s) = %v, want %v", tt.hostname, got, tt.want)
			}
		})
	}
}

// TestValidateURLErrorsHideResolver mirrors the import check for webhook
// URLs, whose validation error is shown to the repository admin. The fake
// resolver fails the way the system one does, naming the DNS server, so the
// test runs offline and still proves that detail is dropped.
func TestValidateURLErrorsHideResolver(t *testing.T) {
	t.Parallel()

	var lookups atomic.Int32
	lookup := func(_ context.Context, host string) ([]net.IPAddr, error) {
		lookups.Add(1)
		return nil, &net.DNSError{Err: "no such host", Name: host, Server: "10.0.0.53:53", IsNotFound: true}
	}
	err := validateURL(context.Background(), "https://does-not-exist.invalid/hook", nil, lookup)
	if n := lookups.Load(); n != 1 {
		t.Fatalf("fake resolver called %d times, want 1", n)
	}
	if !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("ValidateURL() error = %v, want ErrInvalidURL", err)
	}
	for _, leak := range []string{"lookup", " on ", ":53", "no such host"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("ValidateURL() error %q leaks resolver detail %q", err, leak)
		}
	}
}

// TestValidateURLErrorsHideSystemResolver is the test above through the
// exported ValidateURL, which uses the package resolver; the stub DNS server
// stands in for the system's, so the Go resolver's own error text is what
// must not leak.
func TestValidateURLErrorsHideSystemResolver(t *testing.T) {
	fakeResolver(t, nil)

	err := ValidateURL(t.Context(), "https://does-not-exist.invalid/hook")
	if !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("ValidateURL() error = %v, want ErrInvalidURL", err)
	}
	if !strings.Contains(err.Error(), "cannot resolve hostname") {
		t.Errorf("ValidateURL() error = %q, want it to mention %q", err, "cannot resolve hostname")
	}
	for _, leak := range []string{"lookup", " on ", ":53", "no such host"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("ValidateURL() error %q leaks resolver detail %q", err, leak)
		}
	}
}

// TestValidateURLResolvesHostnames covers the DNS path of ValidateURL with a
// fake resolver, so it runs offline.
func TestValidateURLResolvesHostnames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ips     []string
		wantErr error
		// wantMsg, when set, must appear in the error; it tells apart the
		// lookup-failure and empty-answer branches, which share ErrInvalidURL.
		wantMsg string
	}{
		{"public", []string{"140.82.112.3"}, nil, ""},
		{"private", []string{"10.0.0.1"}, ErrPrivateIP, ""},
		{"public and private", []string{"140.82.112.3", "169.254.169.254"}, ErrPrivateIP, ""},
		{"does not resolve", nil, ErrInvalidURL, "cannot resolve hostname"},
		{"empty answer", []string{}, ErrInvalidURL, "no addresses for hostname"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			table := map[string][]string{}
			if tt.ips != nil {
				table["hooks.example"] = tt.ips
			}
			var lookups atomic.Int32
			fake := fakeLookup(table)
			err := validateURL(context.Background(), "https://hooks.example/hook", nil, func(ctx context.Context, host string) ([]net.IPAddr, error) {
				lookups.Add(1)
				return fake(ctx, host)
			})
			if n := lookups.Load(); n != 1 {
				t.Fatalf("fake resolver called %d times, want 1", n)
			}
			if tt.wantErr == nil && err != nil {
				t.Fatalf("validateURL() = %v, want nil", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("validateURL() = %v, want %v", err, tt.wantErr)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("validateURL() error = %q, want it to mention %q", err, tt.wantMsg)
			}
		})
	}
}

// A cancelled context stops the hostname lookup: validation returns at once
// with the context's error instead of waiting for the resolver, and does not
// blame DNS.
func TestValidateURLCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	lookup := func(ctx context.Context, _ string) ([]net.IPAddr, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- validateURL(ctx, "https://hooks.example/hook", nil, lookup) }()
	<-started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("validateURL() = %v, want context.Canceled", err)
		}
		if errors.Is(err, ErrInvalidURL) {
			t.Fatalf("validateURL() = %v, should not report an invalid URL", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("validateURL did not return after its context was cancelled")
	}
}

// ValidateURL and ValidateURLAllowing check a context that is already done
// before resolving anything.
func TestValidateURLAlreadyCancelled(t *testing.T) {
	// The name is unknown to the stub, which would answer "not found"
	// (ErrInvalidURL) if the cancellation were missed.
	fakeResolver(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ValidateURL(ctx, "https://does-not-exist.invalid/"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ValidateURL() = %v, want context.Canceled", err)
	}
	if err := ValidateURLAllowing(ctx, "https://does-not-exist.invalid/", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("ValidateURLAllowing() = %v, want context.Canceled", err)
	}
	// An IP literal needs no lookup, so it is still checked.
	if err := ValidateURL(ctx, "https://93.184.216.34/"); err != nil {
		t.Fatalf("ValidateURL(IP literal) = %v, want nil", err)
	}
}

// The TestSecureClient tests below drive the secure client through its
// transport, as webhooks and the LFS client use it. Names resolve through
// fakeResolver, and the tests up to TestSecureClientDialBudget send
// connection attempts through stubConnect, which reaches only the loopback
// servers a test names; the TestSecureClientTestDial tests use the
// in-memory network instead. Nothing reaches DNS or leaves the machine. A
// refused destination must fail with ErrPrivateIP, which only the SSRF
// checks produce: a dial that merely fails would not pass.

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
		// Bare, so the delivery record does not name the address refused.
		if err != ErrPrivateIP { // the bare sentinel is the point
			t.Errorf("dial %s: err = %v, want exactly ErrPrivateIP", addr, err)
		}
	}
	if got := rec.addrs(); len(got) != 0 {
		t.Errorf("attempted %v, want nothing", got)
	}
}

// TestSecureClientRefusesBlockedLiterals' rows, plus hosts net.Dialer reads
// as the local system: an empty host and the unspecified addresses must be
// refused, not dialed.
func TestSecureClientRefusesLocalSystem(t *testing.T) {
	fakeResolver(t, nil)
	rec := stubConnect(t)
	for _, addr := range []string{":80", "0.0.0.0:80", "[::]:80"} {
		for _, network := range []string{"tcp", "tcp4", "tcp6"} {
			_, err := secureDialFunc(t, NewSecureClient())(context.Background(), network, addr)
			// A family that cannot hold the address fails before any
			// attempt, with net.Dialer's own error.
			if err == nil || (network == "tcp" && !errors.Is(err, ErrPrivateIP)) {
				t.Errorf("dial %s %s: err = %v, want ErrPrivateIP", network, addr, err)
			}
		}
	}
	if got := rec.addrs(); len(got) != 0 {
		t.Errorf("attempted %v, want nothing", got)
	}
}

// TestSecureClientMixedAnswers covers a name that answers with public and
// private addresses. The private ones are never dialed: with the public one
// down the dial fails, and with an allowlisted one up it reaches that.
func TestSecureClientMixedAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	fakeResolver(t, map[string][]string{
		"mixed.test":   {"10.0.0.1", "140.82.112.3", "169.254.169.254", "fc00::1"},
		"private.test": {"10.0.0.1", "127.0.0.1", "169.254.169.254"},
	})

	rec := stubConnect(t)
	_, err := secureDialFunc(t, NewSecureClient())(context.Background(), "tcp", "mixed.test:443")
	if err == nil {
		t.Fatal("dial succeeded with the only public address refused")
	}
	if got := rec.addrs(); !slices.Equal(got, []string{"140.82.112.3:443"}) {
		t.Errorf("attempted %v, want only the public address", got)
	}

	rec = stubConnect(t, srv.Listener.Addr().String())
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://private.test:"+port+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewSecureClientAllowing(mustAllowedHosts(t, "127.0.0.1")).Do(req)
	if err != nil {
		t.Fatalf("GET: %v, want the allowlisted address reached", err)
	}
	_ = res.Body.Close()
	if got := rec.addrs(); !slices.Equal(got, []string{srv.Listener.Addr().String()}) {
		t.Errorf("attempted %v, want only the allowlisted address", got)
	}
}

// TestSecureClientAddressFamily checks that a tcp4 or tcp6 dial tries only
// that family.
func TestSecureClientAddressFamily(t *testing.T) {
	fakeResolver(t, map[string][]string{"dual.test": {"2606:4700::1111", "140.82.112.3"}})
	for network, want := range map[string]string{
		"tcp4": "140.82.112.3:443",
		"tcp6": "[2606:4700::1111]:443",
	} {
		rec := stubConnect(t)
		_, _ = secureDialFunc(t, NewSecureClient())(context.Background(), network, "dual.test:443")
		if got := rec.addrs(); !slices.Equal(got, []string{want}) {
			t.Errorf("%s attempted %v, want [%s]", network, got, want)
		}
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
func TestSecureClientDialBudget(t *testing.T) {
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

// errStubRefused is what a stubbed connection attempt fails with.
var errStubRefused = errors.New("stub: connection refused")

// dialRecorder records every address the secure client gets as far as
// connecting to, that is every address its checks let through, with the
// deadline of the attempt. It refuses every attempt except to the addresses
// in connect, which are dialed for real and must be local listeners, so a
// test never leaves the machine.
type dialRecorder struct {
	mu        sync.Mutex
	connect   map[string]bool
	failFirst int // refuse this many attempts before honouring connect
	dialed    []string
	deadlines []time.Time
}

func (r *dialRecorder) addrs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.dialed...)
}

// attempt records one attempt and reports whether it may connect.
func (r *dialRecorder) attempt(ctx context.Context, addr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dialed = append(r.dialed, addr)
	d, _ := ctx.Deadline()
	r.deadlines = append(r.deadlines, d)
	if r.failFirst > 0 {
		r.failFirst--
		return errStubRefused
	}
	if !r.connect[addr] {
		return errStubRefused
	}
	return nil
}

// stubConnect replaces the secure client's connection attempts for the rest
// of the test; see dialRecorder. It swaps package state, so tests using it
// must not run in parallel.
func stubConnect(t *testing.T, connect ...string) *dialRecorder {
	t.Helper()
	r := &dialRecorder{connect: map[string]bool{}}
	for _, a := range connect {
		r.connect[a] = true
	}
	hook := dialHookFunc(func(ctx context.Context, _, address string) error {
		return r.attempt(ctx, address)
	})
	orig := dialHook.Swap(&hook)
	t.Cleanup(func() { dialHook.Store(orig) })
	return r
}

// fakeResolver points the package's resolver (the secure client's dials and
// every validation lookup not given a resolver of its own) at a DNS server
// that answers from table for the rest of the test, and reports every other
// name as not found. It swaps package state, so tests using it must not run
// in parallel.
func fakeResolver(t *testing.T, table map[string][]string) {
	t.Helper()
	orig := testResolver.Swap(stubResolver(table))
	t.Cleanup(func() { testResolver.Store(orig) })
}

// stubResolver returns a resolver that sends every query to an in-memory
// DNS server answering from table. The server is reached over net.Pipe, so
// nothing touches the host's DNS or the network; the Go resolver still reads
// the hosts file first, which is why tests use names under .test and
// .internal.
func stubResolver(table map[string][]string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			go serveStubDNS(server, table)
			return client, nil
		},
	}
}

// serveStubDNS answers DNS queries on conn, which carries the TCP framing
// (a two-byte length before each message) since a net.Pipe is not a
// PacketConn. Only the question is read; A and AAAA records come from table.
func serveStubDNS(conn net.Conn, table map[string][]string) {
	defer conn.Close() //nolint:errcheck
	for {
		var n uint16
		if err := binary.Read(conn, binary.BigEndian, &n); err != nil {
			return
		}
		q := make([]byte, n)
		if _, err := io.ReadFull(conn, q); err != nil {
			return
		}
		resp := stubDNSAnswer(q, table)
		if resp == nil {
			return
		}
		out := binary.BigEndian.AppendUint16(nil, uint16(len(resp))) // a DNS message is short
		if _, err := conn.Write(append(out, resp...)); err != nil {
			return
		}
	}
}

// stubDNSAnswer builds the response to query q, or nil if q is malformed.
func stubDNSAnswer(q []byte, table map[string][]string) []byte {
	const headerLen = 12
	if len(q) < headerLen {
		return nil
	}
	// The question's name: length-prefixed labels up to a zero byte.
	var labels []string
	i := headerLen
	for {
		if i >= len(q) {
			return nil
		}
		l := int(q[i])
		i++
		if l == 0 {
			break
		}
		if i+l > len(q) {
			return nil
		}
		labels = append(labels, string(q[i:i+l]))
		i += l
	}
	if i+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[i:])
	question := q[headerLen : i+4]

	const typeA, typeAAAA = 1, 28
	answers, known := table[strings.ToLower(strings.Join(labels, "."))]
	var rdata [][]byte
	for _, a := range answers {
		addr := netip.MustParseAddr(a)
		switch {
		case qtype == typeA && addr.Is4():
			rdata = append(rdata, addr.AsSlice())
		case qtype == typeAAAA && addr.Is6():
			rdata = append(rdata, addr.AsSlice())
		}
	}

	// QR, AA, RD and RA; NXDOMAIN for a name not in the table.
	flags := uint16(0x8580)
	if !known {
		flags |= 3
	}
	resp := append([]byte(nil), q[0:2]...) // ID
	resp = binary.BigEndian.AppendUint16(resp, flags)
	resp = binary.BigEndian.AppendUint16(resp, 1)                  // QDCOUNT
	resp = binary.BigEndian.AppendUint16(resp, uint16(len(rdata))) // a short table
	resp = binary.BigEndian.AppendUint16(resp, 0)                  // NSCOUNT
	resp = binary.BigEndian.AppendUint16(resp, 0)                  // ARCOUNT
	resp = append(resp, question...)
	for _, rd := range rdata {
		resp = append(resp, 0xc0, headerLen) // the name, as a pointer to the question's
		resp = binary.BigEndian.AppendUint16(resp, qtype)
		resp = binary.BigEndian.AppendUint16(resp, 1)               // class IN
		resp = binary.BigEndian.AppendUint32(resp, 60)              // TTL
		resp = binary.BigEndian.AppendUint16(resp, uint16(len(rd))) // 4 or 16
		resp = append(resp, rd...)
	}
	return resp
}

// useTestNet routes the secure client's connections through an in-memory
// network for the rest of the test, as other packages' tests do with
// internal/testdial. It swaps package state, so tests using it must not run
// in parallel.
func useTestNet(t *testing.T) *testdial.Network {
	t.Helper()
	n := &testdial.Network{}
	t.Cleanup(testdial.Set(n.Dial))
	return n
}

// With internal/testdial set, a connection the checks let through goes to
// the in-memory network, and one they refuse never reaches it: the
// allowlist and the private ranges still apply.
func TestSecureClientTestDial(t *testing.T) {
	fakeResolver(t, nil)
	n := useTestNet(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "in memory")
	}))
	_ = srv.Listener.Close()
	srv.Listener = n.Listen()
	srv.Start()
	t.Cleanup(srv.Close)

	allowed, err := ParseAllowedHosts([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	get := func(c *http.Client) (string, error) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := c.Do(req)
		if err != nil {
			return "", err
		}
		defer res.Body.Close() //nolint:errcheck
		body, err := io.ReadAll(res.Body)
		return string(body), err
	}
	if body, err := get(NewSecureClientAllowing(allowed)); err != nil || body != "in memory" {
		t.Errorf("allowed: body %q, err %v, want the in-memory server's body", body, err)
	}
	if _, err := get(NewSecureClient()); !errors.Is(err, ErrPrivateIP) {
		t.Errorf("not allowed: err = %v, want ErrPrivateIP", err)
	}

	// A public address passes the checks but has no listener on the
	// in-memory network, so it is refused without leaving the process.
	_, err = secureDialFunc(t, NewSecureClient())(context.Background(), "tcp", "140.82.112.3:443")
	var opErr *net.OpError
	if !errors.As(err, &opErr) || errors.Is(err, ErrPrivateIP) {
		t.Errorf("public: err = %v, want the in-memory network's refusal", err)
	}
}

// Under internal/testdial a name is dialed at its first permitted address,
// as a real dial would connect there, even when an earlier address is
// refused. localhost answers ::1 first, which 127.0.0.0/8 does not cover,
// and then 127.0.0.1.
func TestSecureClientTestDialSkipsRefusedAddresses(t *testing.T) {
	fakeResolver(t, nil) // localhost comes from the hosts file
	n := useTestNet(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "in memory")
	}))
	_ = srv.Listener.Close()
	srv.Listener = n.Listen()
	srv.Start()
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	addrs, err := resolver().LookupIPAddr(context.Background(), "localhost")
	if err != nil || len(addrs) < 2 || addrs[0].IP.To4() != nil {
		t.Skipf("localhost resolves to %v (%v), want ::1 before 127.0.0.1", addrs, err)
	}
	allowed, err := ParseAllowedHosts([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://localhost:"+port+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewSecureClientAllowing(allowed).Do(req)
	if err != nil {
		t.Fatalf("localhost with 127.0.0.0/8 allowed: %v", err)
	}
	defer res.Body.Close() //nolint:errcheck
	if body, _ := io.ReadAll(res.Body); string(body) != "in memory" {
		t.Errorf("body %q, want the in-memory server's", body)
	}
}

// While internal/testdial gets a Func in the middle of a dial, one attempt
// can connect, having been checked before the Func was set, and another be
// redirected after: the hand-off then returns the Func's connection, and
// closes the one the dial made instead of leaking it.
func TestToTestDialerClosesDialedConnection(t *testing.T) {
	// Not parallel: it sets internal/testdial, which is package state.
	redirected, redirectedPeer := net.Pipe()
	dialed, dialedPeer := net.Pipe()
	t.Cleanup(func() {
		for _, c := range []net.Conn{redirected, redirectedPeer, dialed, dialedPeer} {
			_ = c.Close()
		}
	})
	t.Cleanup(testdial.Set(func(context.Context, string, string) (net.Conn, error) {
		return redirected, nil
	}))

	c, handed, err := toTestDialer(t.Context(), &dialTarget{network: "tcp", address: "127.0.0.1:80"}, dialed)
	if !handed || err != nil {
		t.Fatalf("toTestDialer = handed %v, error %v; want the test dialer's connection", handed, err)
	}
	if c != redirected {
		t.Fatal("toTestDialer returned the dialed connection, not the test dialer's")
	}
	// A pipe refuses a deadline once it is closed, and only then.
	if err := dialed.SetDeadline(time.Time{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("the dialed connection is still open: SetDeadline = %v, want %v", err, io.ErrClosedPipe)
	}
}
