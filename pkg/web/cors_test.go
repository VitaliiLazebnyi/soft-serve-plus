package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

// An empty public URL with no allowed origins allows no origin. gorilla's
// CORS handler would allow every origin for an empty list.
func TestCORSHandlerOrigins(t *testing.T) {
	t.Parallel()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	for _, tc := range []struct {
		name      string
		publicURL string
		origins   []string
		origin    string
		want      string
	}{
		{name: "no origins", origin: "https://evil.example.com", want: ""},
		{name: "public url", publicURL: "https://git.example.com", origin: "https://git.example.com", want: "https://git.example.com"},
		{name: "other origin", publicURL: "https://git.example.com", origin: "https://evil.example.com", want: ""},
		{name: "allowed origin", origins: []string{"https://app.example.com"}, origin: "https://app.example.com", want: "https://app.example.com"},
		{name: "wildcard", origins: []string{"*"}, origin: "https://any.example.com", want: "*"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.DefaultConfig()
			cfg.HTTP.PublicURL = tc.publicURL
			cfg.HTTP.CORS.AllowedOrigins = tc.origins
			h := corsHandler(cfg)(ok)
			for _, method := range []string{http.MethodGet, http.MethodOptions} {
				req := httptest.NewRequestWithContext(t.Context(), method, "/", nil)
				req.Header.Set("Origin", tc.origin)
				if method == http.MethodOptions {
					req.Header.Set("Access-Control-Request-Method", http.MethodGet)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.want {
					t.Errorf("%s: Access-Control-Allow-Origin = %q, want %q", method, got, tc.want)
				}
			}
		})
	}
}
