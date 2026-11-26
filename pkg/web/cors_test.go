package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

// An empty public URL with no allowed origins allows no origin, not every
// one.
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

// corsCase is a request through the router and the CORS answer it gets.
type corsCase struct {
	name    string
	method  string
	path    string
	origin  string
	reqMeth string // Access-Control-Request-Method; "-" leaves it out
	reqHdrs string // Access-Control-Request-Headers

	code        int
	emptyBody   bool   // the answer has no body
	allowOrigin string // Access-Control-Allow-Origin
	allowMeth   string // Access-Control-Allow-Methods
	allowHdrs   string // Access-Control-Allow-Headers
}

// TestCORSBehaviour pins what the router answers to cross-origin requests
// and preflights: which origins, methods and headers are allowed, the empty
// 200 to an OPTIONS request, no credentials, and Vary: Origin on every
// answer.
func TestCORSBehaviour(t *testing.T) {
	t.Parallel()
	const (
		public = "https://git.example.com"
		app    = "https://app.example.com"
		evil   = "https://evil.example.com"
		refs   = "/repo.git/info/refs"
	)
	preflight := func(name, origin, reqMeth, reqHdrs string, code int, allowOrigin, allowMeth, allowHdrs string) corsCase {
		return corsCase{
			name: name, method: http.MethodOptions, path: refs, origin: origin, reqMeth: reqMeth, reqHdrs: reqHdrs,
			code: code, emptyBody: true, allowOrigin: allowOrigin, allowMeth: allowMeth, allowHdrs: allowHdrs,
		}
	}
	get := func(name, origin, allowOrigin string) corsCase {
		return corsCase{name: name, method: http.MethodGet, path: "/livez", origin: origin, reqMeth: "-", code: http.StatusOK, allowOrigin: allowOrigin}
	}

	for _, group := range []struct {
		name    string
		public  string
		origins []string
		headers []string // nil keeps the default
		methods []string // nil keeps the default
		cases   []corsCase
	}{
		{
			name: "public url and an allowed origin", public: public + "/git", origins: []string{app},
			cases: []corsCase{
				get("get, allowed origin", app, app),
				get("get, public url origin", public, public),
				get("get, other origin", evil, ""),
				get("get, no origin", "", ""),
				get("get, origin in other case", "https://APP.example.com", ""),
				preflight("preflight, simple method", app, http.MethodGet, "", http.StatusOK, app, "", ""),
				preflight("preflight, other allowed method", app, http.MethodPut, "", http.StatusOK, app, http.MethodPut, ""),
				preflight("preflight, lower-case method", app, "put", "", http.StatusMethodNotAllowed, "", "", ""),
				preflight("preflight, method not allowed", app, http.MethodDelete, "", http.StatusMethodNotAllowed, "", "", ""),
				preflight("preflight, no request method", app, "-", "", http.StatusBadRequest, "", "", ""),
				preflight("preflight, empty request method", app, "", "", http.StatusMethodNotAllowed, "", "", ""),
				preflight("preflight, allowed headers", app, http.MethodPost, "content-type, X-Requested-With,authorization", http.StatusOK, app, "", "Content-Type,X-Requested-With,Authorization"),
				preflight("preflight, simple headers only", app, http.MethodPost, "accept, accept-language,Content-Language, origin", http.StatusOK, app, "", ""),
				preflight("preflight, simple and other headers", app, http.MethodPost, "Accept, user-agent", http.StatusOK, app, "", "User-Agent"),
				preflight("preflight, empty header names", app, http.MethodPost, " , ,content-type,", http.StatusOK, app, "", "Content-Type"),
				preflight("preflight, header not allowed", app, http.MethodPost, "content-type, x-custom", http.StatusForbidden, "", "", ""),
				preflight("preflight, other origin", evil, http.MethodPut, "x-custom", http.StatusOK, "", "", ""),
				preflight("options, no origin", "", "-", "", http.StatusOK, "", "", ""),
				preflight("options, other origin", evil, "-", "", http.StatusOK, "", "", ""),
			},
		},
		{
			name: "public url only", public: public,
			cases: []corsCase{
				get("get, public url origin", public, public),
				get("get, other origin", evil, ""),
				preflight("preflight", public, http.MethodHead, "", http.StatusOK, public, "", ""),
			},
		},
		{
			name: "no origins",
			cases: []corsCase{
				get("get, any origin", evil, ""),
				preflight("preflight, any origin", evil, http.MethodGet, "", http.StatusOK, "", "", ""),
				preflight("options, no origin", "", "-", "", http.StatusOK, "", "", ""),
			},
		},
		{
			name: "wildcard", origins: []string{app, "*"},
			cases: []corsCase{
				get("get, any origin", evil, "*"),
				get("get, listed origin", app, "*"),
				get("get, no origin", "", ""),
				preflight("preflight, any origin", evil, http.MethodPut, "", http.StatusOK, "*", http.MethodPut, ""),
			},
		},
		{
			name: "configured headers and methods", origins: []string{app},
			headers: []string{" x-custom ", "", "Content-Type"},
			methods: []string{" delete", "get", ""},
			cases: []corsCase{
				preflight("preflight, configured method", app, http.MethodDelete, "", http.StatusOK, app, http.MethodDelete, ""),
				preflight("preflight, configured simple method", app, http.MethodGet, "", http.StatusOK, app, "", ""),
				preflight("preflight, default method no longer allowed", app, http.MethodPost, "", http.StatusMethodNotAllowed, "", "", ""),
				preflight("preflight, configured header", app, http.MethodGet, "X-CUSTOM, content-type", http.StatusOK, app, "", "X-Custom,Content-Type"),
				preflight("preflight, simple header always allowed", app, http.MethodGet, "accept-language", http.StatusOK, app, "", ""),
				preflight("preflight, default header no longer allowed", app, http.MethodGet, "authorization", http.StatusForbidden, "", "", ""),
			},
		},
		{
			name: "no methods", origins: []string{app}, methods: []string{},
			cases: []corsCase{
				preflight("preflight, simple method", app, http.MethodGet, "", http.StatusMethodNotAllowed, "", "", ""),
				get("get still served", app, app),
			},
		},
	} {
		t.Run(group.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			cfg := env.cfg
			cfg.HTTP.PublicURL = group.public
			cfg.HTTP.CORS.AllowedOrigins = group.origins
			if group.headers != nil {
				cfg.HTTP.CORS.AllowedHeaders = group.headers
			}
			if group.methods != nil {
				cfg.HTTP.CORS.AllowedMethods = group.methods
			}
			h := env.router(t)

			for _, tc := range group.cases {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil)
					if tc.origin != "" {
						req.Header.Set("Origin", tc.origin)
					}
					if tc.reqMeth != "-" {
						req.Header.Set("Access-Control-Request-Method", tc.reqMeth)
					}
					if tc.reqHdrs != "" {
						req.Header.Set("Access-Control-Request-Headers", tc.reqHdrs)
					}
					w := httptest.NewRecorder()
					h.ServeHTTP(w, req)

					if w.Code != tc.code {
						t.Errorf("status = %d, want %d", w.Code, tc.code)
					}
					if tc.emptyBody && w.Body.Len() != 0 {
						t.Errorf("body = %q, want none", w.Body.String())
					}
					if !tc.emptyBody && w.Body.Len() == 0 {
						t.Errorf("no body; the request was not served")
					}
					hdr := w.Header()
					for name, want := range map[string]string{
						"Access-Control-Allow-Origin":      tc.allowOrigin,
						"Access-Control-Allow-Methods":     tc.allowMeth,
						"Access-Control-Allow-Headers":     tc.allowHdrs,
						"Access-Control-Allow-Credentials": "",
						"Access-Control-Expose-Headers":    "",
						"Access-Control-Max-Age":           "",
					} {
						if got := hdr.Get(name); got != want {
							t.Errorf("%s = %q, want %q", name, got, want)
						}
					}
					vary := hdr.Values("Vary")
					origins := slices.DeleteFunc(slices.Clone(vary), func(v string) bool { return v != "Origin" })
					if len(origins) != 1 {
						t.Errorf("Vary = %q, want Origin once", vary)
					}
				})
			}
		})
	}
}

// Every response varies by Origin, whether or not the origin is allowed and
// however many origins are, so a shared cache does not hand one origin's
// answer to another.
func TestCORSVaryOrigin(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	cfg := f.env.cfg
	cfg.HTTP.CORS.AllowedOrigins = []string{"https://git.example.com"}
	h := f.env.router(t)

	for _, origin := range []string{"", "https://git.example.com", "https://evil.example.com"} {
		for _, method := range []string{http.MethodGet, http.MethodOptions} {
			req := httptest.NewRequestWithContext(context.Background(), method, "/nope", nil)
			if origin != "" {
				req.Header.Set("Origin", origin)
			}
			if method == http.MethodOptions {
				req.Header.Set("Access-Control-Request-Method", http.MethodGet)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if got, want := w.Header().Values("Vary"), ([]string{"Origin"}); !reflect.DeepEqual(got, want) {
				t.Fatalf("w.Header().Values(\"Vary\") = %v, want %v", got, want)
			}
		}
	}
}

// A public URL with a path still allows its origin: an Origin header never
// carries a path, so the configured URL is reduced to scheme and host.
func TestCORSPublicURLWithPath(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	cfg := f.env.cfg
	cfg.HTTP.PublicURL = "https://Git.Example.com:443/soft/"
	cfg.HTTP.CORS.AllowedOrigins = []string{cfg.HTTP.PublicURL}
	h := f.env.router(t)

	for origin, want := range map[string]string{
		"https://git.example.com":  "https://git.example.com",
		"https://evil.example.com": "",
	} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/nope", nil)
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != want {
			t.Fatalf("w.Header().Get(\"Access-Control-Allow-Origin\") = %v, want %v", got, want)
		}
	}
}

func TestCORSOrigins(t *testing.T) {
	t.Parallel()
	if got, want := corsOrigins([]string{
		"https://git.example.com/soft",
		"https://git.example.com",
		"HTTP://Example.COM:80/",
		"http://example.com:8080/x?y#z",
		"https://[::1]:443/git",
		"https://[::1]:8443",
		"*",
		"",
		"not a url",
	}), ([]string{
		"https://git.example.com",
		"http://example.com",
		"http://example.com:8080",
		"https://[::1]",
		"https://[::1]:8443",
		"*",
		"",
		"not a url",
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("corsOrigins of the mixed origin list = %v, want %v", got, want)
	}
}
