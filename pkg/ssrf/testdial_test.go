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
