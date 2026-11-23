//go:build !webhooktest

package webhook

import "net/http"

// testClient returns c. The webhooktest build replaces it; see
// testdial_webhooktest.go.
func testClient(c *http.Client) *http.Client { return c }

// testMaxDeliveries returns n. The webhooktest build replaces it; see
// testdial_webhooktest.go.
func testMaxDeliveries(n int) int { return n }
