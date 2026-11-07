package web

import (
	"context"
	"net/http"

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
	h := NewLoggingMiddleware(router, logger)
	h = NewContextHandler(ctx)(h)
	h = handlers.RecoveryHandler()(h)

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
