package web

import (
	"context"
	"net/http"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/gorilla/mux"
)

// healthController registers the health check routes for the web server.
func healthController(_ context.Context, r *mux.Router) {
	r.HandleFunc("/livez", getLiveness)
	r.HandleFunc("/readyz", getReadiness)
}

func getLiveness(w http.ResponseWriter, _ *http.Request) {
	renderStatus(w, http.StatusOK)
}

func getReadiness(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := log.FromContext(ctx)
	db := db.FromContext(ctx)

	if err := db.PingContext(ctx); err != nil {
		logger.Error("error getting db readiness", "err", err)
		renderStatus(w, http.StatusServiceUnavailable)
		return
	}

	renderStatus(w, http.StatusOK)
}
