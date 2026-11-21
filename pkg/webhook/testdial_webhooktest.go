//go:build webhooktest

package webhook

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/ssrf"
)

// This file is compiled only with the webhooktest build tag, which the
// testscript suite uses to build its soft binary. Release builds never contain
// it; they use testdial_default.go. The suite drives a separate soft process,
// so it cannot hand a Sender a client the way the unit tests do: the process
// reads these variables when the backend builds its Sender.
//
// When SOFT_SERVE_TEST_WEBHOOK_ADDR is set, every webhook request is dialed to
// that address instead of the webhook's host, whatever
// webhook.allowed_hosts says. Webhook URLs are still validated
// as usual, so the tests use public addresses, but nothing leaves the machine:
// the requests reach a receiver the test runs locally. An address of the form
// unix:<path> is a Unix socket, which the testscript receiver uses: a loopback
// TCP connect can stall for seconds on a busy macOS machine and fail with
// ETIMEDOUT, and nothing sends a failed delivery again, so the event would be
// lost. Any other address is dialed over TCP.
//
// SOFT_SERVE_TEST_WEBHOOK_MAX_DELIVERIES lowers how many deliveries each
// webhook keeps, so a test can reach the limit with a few events.

// testClient returns c, or, when SOFT_SERVE_TEST_WEBHOOK_ADDR is set, a
// client that dials that address for every request.
func testClient(c *http.Client) *http.Client {
	addr := os.Getenv("SOFT_SERVE_TEST_WEBHOOK_ADDR")
	if addr == "" {
		return c
	}
	network := "tcp"
	if path, ok := strings.CutPrefix(addr, "unix:"); ok {
		network, addr = "unix", path
	}
	return &http.Client{
		Timeout: ssrf.ClientTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
			MaxResponseHeaderBytes: ssrf.MaxResponseHeaderBytes,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// testMaxDeliveries returns n, or SOFT_SERVE_TEST_WEBHOOK_MAX_DELIVERIES when
// it is set to a number of at least 0.
func testMaxDeliveries(n int) int {
	if v, err := strconv.Atoi(os.Getenv("SOFT_SERVE_TEST_WEBHOOK_MAX_DELIVERIES")); err == nil && v >= 0 {
		return v
	}
	return n
}
