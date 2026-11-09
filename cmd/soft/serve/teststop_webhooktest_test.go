//go:build webhooktest

package serve

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Repeated and concurrent HEAD /__stop requests stop the server once and
// never panic on closing the stop channel twice.
func TestTestStopRepeatedRequests(t *testing.T) {
	t.Setenv("SOFT_SERVE_TESTRUN", "1")

	stop := make(chan struct{})
	h := withTestStop(http.NotFoundHandler(), stop)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/__stop", nil))
		}()
	}
	wg.Wait()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/__stop", nil))

	select {
	case <-stop:
	default:
		t.Fatal("HEAD /__stop did not stop the server")
	}
}

// Without SOFT_SERVE_TESTRUN the endpoint is not served, even in test builds.
func TestTestStopNeedsTestRun(t *testing.T) {
	t.Setenv("SOFT_SERVE_TESTRUN", "")

	stop := make(chan struct{})
	h := withTestStop(http.NotFoundHandler(), stop)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/__stop", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("HEAD /__stop: got %d, want %d", rec.Code, http.StatusNotFound)
	}
	select {
	case <-stop:
		t.Fatal("HEAD /__stop stopped the server without SOFT_SERVE_TESTRUN")
	default:
	}
}
