//go:build !webhooktest

package serve

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Release builds don't serve the test stop endpoint, even with
// SOFT_SERVE_TESTRUN set: the request reaches the real handler.
func TestTestStopAbsentInReleaseBuilds(t *testing.T) {
	t.Setenv("SOFT_SERVE_TESTRUN", "1")

	stop := make(chan struct{})
	reached := false
	h := withTestStop(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNotFound)
	}), stop)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/__stop", nil))
	if !reached || rec.Code != http.StatusNotFound {
		t.Fatalf("HEAD /__stop was not passed on: reached=%v code=%d", reached, rec.Code)
	}
	select {
	case <-stop:
		t.Fatal("HEAD /__stop stopped a release build")
	default:
	}
}
