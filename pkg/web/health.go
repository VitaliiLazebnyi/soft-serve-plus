package web

import (
	"net/http"

	"github.com/gorilla/mux"
)

// healthController registers the health check routes for the web server.
func (s *server) healthController(r *mux.Router) {
	r.HandleFunc("/livez", getLiveness)
	r.HandleFunc("/readyz", s.getReadiness)
}

func getLiveness(w http.ResponseWriter, _ *http.Request) {
	renderStatus(w, http.StatusOK)
}

func (s *server) getReadiness(w http.ResponseWriter, r *http.Request) {
	if err := s.dbx.PingContext(r.Context()); err != nil {
		s.requestLogger(r).Error("error getting db readiness", "err", err)
		renderStatus(w, http.StatusServiceUnavailable)
		return
	}

	renderStatus(w, http.StatusOK)
}
