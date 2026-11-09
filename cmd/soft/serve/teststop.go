//go:build !webhooktest

package serve

import "net/http"

// withTestStop returns h unchanged: the test-only HEAD /__stop endpoint
// exists only in webhooktest builds. See teststop_webhooktest.go.
func withTestStop(h http.Handler, _ chan<- struct{}) http.Handler {
	return h
}
