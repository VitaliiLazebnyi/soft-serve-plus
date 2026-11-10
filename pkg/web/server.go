package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"strings"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
)

// NewRouter returns a new HTTP router.
func NewRouter(ctx context.Context) http.Handler {
	logger := log.FromContext(ctx).WithPrefix("http")
	router := mux.NewRouter()

	// Health routes
	HealthController(ctx, router)

	// Git routes
	GitController(ctx, router)

	router.PathPrefix("/").HandlerFunc(renderNotFound)

	// Context handler
	// Adds context to the request
	h := recoverPanics(router)
	h = NewLoggingMiddleware(h, logger)
	h = NewContextHandler(ctx)(h)

	cfg := config.FromContext(ctx)

	h = handlers.CORS(handlers.AllowedHeaders(cfg.HTTP.CORS.AllowedHeaders),
		handlers.AllowedOrigins(corsOrigins(cfg.HTTP.CORS.AllowedOrigins)),
		handlers.AllowedMethods(cfg.HTTP.CORS.AllowedMethods),
	)(h)
	h = varyOrigin(h)

	return h
}

// varyOrigin marks every response as depending on the Origin request header.
// The CORS handler only says so when more than one origin is allowed, yet
// it answers an allowed origin differently from another one or from none,
// so a shared cache could otherwise serve one origin's answer to another.
func varyOrigin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		h.ServeHTTP(w, r)
	})
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
	if port := u.Port(); port != "" && !(scheme == "http" && port == "80") && !(scheme == "https" && port == "443") {
		host += ":" + port
	}
	return scheme + "://" + host
}

// recoverPanics answers 500 to a request whose handler panicked, and logs
// the panic with its stack on the request logger.
func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			// net/http aborts the response quietly on this one.
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			log.FromContext(r.Context()).Error("panic serving request", "panic", v, "stack", string(debug.Stack()))
			renderInternalServerError(w, r)
		}()
		next.ServeHTTP(w, r)
	})
}
