package backend

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/charmbracelet/soft-serve/internal/testdial"
)

// testNet carries every connection the SSRF-protected clients open in this
// package's tests (see TestMain). Webhook receivers listen on it with
// newMemServer. A send to any other address, such as a webhook's public
// test address, is refused at once, so no test reaches the network.
var testNet testdial.Network

// TestMain keeps pkg/ssrf off the network: its clients connect through
// testNet, and a hostname it looks up, such as a webhook's or an import
// remote's, resolves from the hosts file only and otherwise fails at once.
func TestMain(m *testing.M) {
	restoreDial := testdial.Set(testNet.Dial)
	restoreResolver := testdial.SetResolver(testdial.NoDNS())
	code := m.Run()
	restoreResolver()
	restoreDial()
	os.Exit(code)
}

// newMemServer starts an httptest server for h on testNet and closes it when
// the test ends. Its URL is an http URL on 127.0.0.1, which the SSRF checks
// treat as loopback.
func newMemServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	// NewUnstartedServer has already opened a loopback listener; it is
	// replaced before anything can connect to it.
	_ = srv.Listener.Close()
	srv.Listener = testNet.Listen()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}
