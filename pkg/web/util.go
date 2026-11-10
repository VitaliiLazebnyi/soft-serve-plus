package web

import (
	"fmt"
	"io"
	"net/http"
)

// renderStatus returns a handler that answers with code and its status text
// as a plain-text body. The body is labelled as UTF-8 text and must not be
// sniffed, so a browser never renders it as anything else.
func renderStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(code)
		io.WriteString(w, fmt.Sprintf("%d %s", code, http.StatusText(code))) //nolint: errcheck
	}
}
