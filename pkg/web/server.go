package web

import (
	"context"
	"errors"
	"net/http"
	"runtime/debug"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/gitserve"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/gorilla/mux"
)

// server holds what the handlers of the HTTP server share: the
// dependencies NewHTTPServer is given, and the state of the git routes.
type server struct {
	cfg       *config.Config
	be        *backend.Backend
	dbx       *db.DB
	datastore lfsStore
	// tokens verifies bearer tokens. It is nil when the server has no
	// usable host key, and every bearer token is then refused.
	tokens *jwk.Signer
	// logger is the server's logger. Each request logs on a logger derived
	// from it; see requestLogger.
	logger *log.Logger
	// pending tracks repositories created by push-to-create; see
	// pushCreates.
	pending *pushCreates
	// limits bounds how many requests the git routes serve at once; see
	// requestLimits. The router bounds the time of every request; see
	// requestDeadlines.
	limits *requestLimits

	// runService runs a service RPC's git. A test stands in for git with
	// it, as for a push whose client went away after its refs were
	// updated.
	runService func(ctx context.Context, service gitserve.Service, cmd gitserve.ServiceCommand) error
	// beforePushCreate, when set, runs in withAccess after a push to a
	// missing name was authorized and before the repository is created.
	beforePushCreate func()
}

// lfsStore is what the LFS routes use of the store.
type lfsStore interface {
	store.LFSStore
	store.RepositoryStore
}

// newServer returns the handlers' server. A nil logger is log.Default().
func newServer(cfg *config.Config, be *backend.Backend, dbx *db.DB, datastore lfsStore, tokens *jwk.Signer, logger *log.Logger) *server {
	if logger == nil {
		logger = log.Default()
	}
	logger = logger.WithPrefix("http")
	return &server{
		cfg:       cfg,
		be:        be,
		dbx:       dbx,
		datastore: datastore,
		tokens:    tokens,
		logger:    logger,
		pending:   newPushCreates(be, logger),
		limits:    newRequestLimits(cfg.HTTP),
		runService: func(ctx context.Context, service gitserve.Service, cmd gitserve.ServiceCommand) error {
			return service.Handler(ctx, cmd)
		},
	}
}

// requestLogger returns the logger of request r: the server's, with the
// request's method, path and client address.
func (s *server) requestLogger(r *http.Request) *log.Logger {
	return s.logger.With(
		"method", r.Method,
		"path", r.URL,
		"addr", r.RemoteAddr,
	)
}

// router returns the HTTP handler of the server.
func (s *server) router() http.Handler {
	router := mux.NewRouter()

	// Health routes
	s.healthController(router)

	// Git routes
	s.gitController(router)

	router.PathPrefix("/").HandlerFunc(renderNotFound)

	h := s.recoverPanics(router)
	h = newLoggingMiddleware(h, s.logger)
	h = corsHandler(s.cfg)(h)

	// Outermost, so every answer is bound by the deadlines, including the
	// ones the middleware gives, such as a CORS preflight.
	h = withDeadlines(s.cfg.HTTP, h)

	return h
}

// recoverPanics answers 500 to a request whose handler panicked, and logs
// the panic with its stack on the request logger.
func (s *server) recoverPanics(next http.Handler) http.Handler {
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
			s.requestLogger(r).Error("panic serving request", "panic", v, "stack", string(debug.Stack()))
			renderInternalServerError(w, r)
		}()
		next.ServeHTTP(w, r)
	})
}
