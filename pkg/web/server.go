package web

import (
	"context"
	"errors"
	"net/http"
	"runtime/debug"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
)

// NewRouter returns a new HTTP router.
func NewRouter(ctx context.Context) http.Handler {
	return newRouter(ctx, newPushCreates())
}

// newRouter returns a new HTTP router that tracks the repositories HTTP
// pushes create in pending.
func newRouter(ctx context.Context, pending *pushCreates) http.Handler {
	logger := log.FromContext(ctx).WithPrefix("http")
	router := mux.NewRouter()

	// Health routes
	HealthController(ctx, router)

	// Git routes
	gitController(ctx, router, pending)

	router.PathPrefix("/").HandlerFunc(renderNotFound)

	// Context handler
	// Adds context to the request
	h := recoverPanics(router)
	h = NewLoggingMiddleware(h, logger)
	h = NewContextHandler(ctx)(h)

	cfg := config.FromContext(ctx)

	h = handlers.CORS(handlers.AllowedHeaders(cfg.HTTP.CORS.AllowedHeaders),
		handlers.AllowedOrigins(cfg.HTTP.CORS.AllowedOrigins),
		handlers.AllowedMethods(cfg.HTTP.CORS.AllowedMethods),
	)(h)

	return h
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
