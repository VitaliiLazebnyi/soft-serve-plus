package web

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

// CORS answers. The handler allows the configured origins, methods and
// headers. It sends no credentials header, no exposed headers and no max
// age, and answers a preflight with an empty 200.
const (
	corsAllowOrigin    = "Access-Control-Allow-Origin"
	corsAllowMethods   = "Access-Control-Allow-Methods"
	corsAllowHeaders   = "Access-Control-Allow-Headers"
	corsRequestMethod  = "Access-Control-Request-Method"
	corsRequestHeaders = "Access-Control-Request-Headers"
	corsAnyOrigin      = "*"
)

var (
	// corsSimpleMethods need not be listed in Access-Control-Allow-Methods.
	corsSimpleMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost}
	// corsSimpleHeaders are always allowed, and never listed in
	// Access-Control-Allow-Headers.
	corsSimpleHeaders = []string{"Accept", "Accept-Language", "Content-Language", "Origin"}
)

// cors is the CORS middleware.
type cors struct {
	next http.Handler
	// origins are the allowed origins; none is allowed when it is empty.
	origins []string
	// anyOrigin is set when origins holds "*": every origin is allowed and
	// answered with "*" rather than echoed.
	anyOrigin bool
	// methods are the methods a preflight may ask for, upper-cased.
	methods []string
	// headers are the request headers a preflight may ask for, in
	// canonical form, the simple ones included.
	headers []string
}

// corsHandler returns the CORS middleware for cfg's allowed headers, origins
// and methods. With no public URL and no allowed origins, no origin is
// allowed; "*" in allowed_origins allows every one.
func corsHandler(cfg *config.Config) func(http.Handler) http.Handler {
	origins := corsOrigins(cfg.CORSAllowedOrigins())
	methods := []string{}
	for _, m := range cfg.HTTP.CORS.AllowedMethods {
		if m = strings.ToUpper(strings.TrimSpace(m)); m != "" && !slices.Contains(methods, m) {
			methods = append(methods, m)
		}
	}
	headers := slices.Clone(corsSimpleHeaders)
	for _, h := range cfg.HTTP.CORS.AllowedHeaders {
		if h = http.CanonicalHeaderKey(strings.TrimSpace(h)); h != "" && !slices.Contains(headers, h) {
			headers = append(headers, h)
		}
	}
	return func(next http.Handler) http.Handler {
		return &cors{
			next:      next,
			origins:   origins,
			anyOrigin: slices.Contains(origins, corsAnyOrigin),
			methods:   methods,
			headers:   headers,
		}
	}
}

// ServeHTTP implements http.Handler. Every answer is marked as depending on
// the Origin request header: an allowed origin is answered differently from
// another one or from none, so a shared cache could otherwise serve one
// origin's answer to another.
func (c *cors) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Add("Vary", "Origin")

	origin := r.Header.Get("Origin")
	allowed := origin != "" && (c.anyOrigin || slices.Contains(c.origins, origin))
	preflight := r.Method == http.MethodOptions
	if !allowed {
		// An OPTIONS request from an origin that is not allowed, or from
		// none, gets an empty 200.
		if !preflight {
			c.next.ServeHTTP(w, r)
		}
		return
	}

	if preflight {
		if _, ok := r.Header[corsRequestMethod]; !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		method := r.Header.Get(corsRequestMethod)
		if !slices.Contains(c.methods, method) {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var allowHeaders []string
		for _, v := range strings.Split(r.Header.Get(corsRequestHeaders), ",") {
			name := http.CanonicalHeaderKey(strings.TrimSpace(v))
			if name == "" || slices.Contains(corsSimpleHeaders, name) {
				continue
			}
			if !slices.Contains(c.headers, name) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			allowHeaders = append(allowHeaders, name)
		}
		if len(allowHeaders) > 0 {
			h.Set(corsAllowHeaders, strings.Join(allowHeaders, ","))
		}
		if !slices.Contains(corsSimpleMethods, method) {
			h.Set(corsAllowMethods, method)
		}
	}

	if c.anyOrigin {
		h.Set(corsAllowOrigin, corsAnyOrigin)
	} else {
		h.Set(corsAllowOrigin, origin)
	}

	if preflight {
		w.WriteHeader(http.StatusOK)
		return
	}
	c.next.ServeHTTP(w, r)
}

// corsOrigins returns the allowed origins with each URL reduced to the
// origin a browser sends: scheme and host, lower-cased, without a default
// port, path, query or fragment. The HTTP public URL is among the allowed
// origins and may carry a path, as in "https://example.com/git", which no
// Origin header ever matches. Entries that are not absolute URLs, such as
// "*", are kept as they are.
func corsOrigins(origins []string) []string {
	out := make([]string, 0, len(origins))
	for _, o := range origins {
		o = corsOrigin(o)
		if !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	return out
}

// corsOrigin returns the serialized origin of s, or s when it is not an
// absolute URL with a host.
func corsOrigin(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" {
		return s
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && (scheme != "http" || port != "80") && (scheme != "https" || port != "443") {
		host += ":" + port
	}
	return scheme + "://" + host
}
