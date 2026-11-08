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

	h = corsHandler(cfg)(h)

	return h
}

// corsHandler returns the CORS middleware for cfg's allowed headers, origins
// and methods.
func corsHandler(cfg *config.Config) func(http.Handler) http.Handler {
	opts := []handlers.CORSOption{
		handlers.AllowedHeaders(cfg.HTTP.CORS.AllowedHeaders),
		handlers.AllowedMethods(cfg.HTTP.CORS.AllowedMethods),
	}
	if origins := cfg.CORSAllowedOrigins(); len(origins) > 0 {
		opts = append(opts, handlers.AllowedOrigins(origins))
	} else {
		// gorilla/handlers allows every origin when the list is empty, so
		// with no public URL and no allowed origins, reject them all
		// explicitly. Use "*" in allowed_origins to allow every origin.
		opts = append(opts, handlers.AllowedOriginValidator(func(string) bool { return false }))
	}
	return handlers.CORS(opts...)
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
