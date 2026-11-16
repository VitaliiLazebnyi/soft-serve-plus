package ssrf

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/charmbracelet/soft-serve/internal/testdial"
)

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
