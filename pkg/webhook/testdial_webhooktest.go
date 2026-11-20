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
// it.
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
func init() {
	if n, err := strconv.Atoi(os.Getenv("SOFT_SERVE_TEST_WEBHOOK_MAX_DELIVERIES")); err == nil && n >= 0 {
		maxDeliveries = n
	}

	addr := os.Getenv("SOFT_SERVE_TEST_WEBHOOK_ADDR")
	if addr == "" {
		return
	}
	network := "tcp"
	if path, ok := strings.CutPrefix(addr, "unix:"); ok {
		network, addr = "unix", path
	}
	useOnlySecureHTTPClient = true
	secureHTTPClient = &http.Client{
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
