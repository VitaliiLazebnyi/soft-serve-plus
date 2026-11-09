//go:build webhooktest

package serve

import (
	"net/http"
	"os"
	"strconv"
	"sync"
)

// withTestStop adds the HEAD /__stop endpoint the testscript suite uses to
// stop the server, since Windows has no signals to send it. It is compiled
// only with the webhooktest build tag, which the testscript suite builds its
// soft binary with, and is served only when SOFT_SERVE_TESTRUN is set.
// Release builds never contain it. The stop channel is closed once, however
// many requests arrive.
func withTestStop(h http.Handler, stop chan<- struct{}) http.Handler {
	if testRun, _ := strconv.ParseBool(os.Getenv("SOFT_SERVE_TESTRUN")); !testRun {
		return h
	}
	var once sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__stop" && r.Method == http.MethodHead {
			once.Do(func() { close(stop) })
			return
		}
		h.ServeHTTP(w, r)
	})
}
