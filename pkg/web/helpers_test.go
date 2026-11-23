package web

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/gitserve"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/gorilla/mux"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

// testEnv is what a test server is built from: a config with its own data
// directory, a migrated SQLite database, its store and a backend over them.
type testEnv struct {
	// ctx is the context of the test's own calls to the backend.
	ctx       context.Context
	cfg       *config.Config
	dbx       *db.DB
	datastore store.Store
	be        *backend.Backend
	// logger is the logger of the backend and of the servers built from
	// the env; nil is log.Default().
	logger *log.Logger
}

// newTestEnv returns a new env whose backend and servers log on
// log.Default().
func newTestEnv(t testing.TB) *testEnv {
	t.Helper()
	return newTestEnvLogging(t, nil)
}

// newTestEnvLogging returns a new env whose backend and servers log on
// logger.
func newTestEnvLogging(t testing.TB, logger *log.Logger) *testEnv {
	t.Helper()
	ctx := context.Background()

	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	cfg.DB.Driver = "sqlite"
	cfg.DB.DataSource = dp + "/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"

	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck

	if err := migrate.Migrate(ctx, dbx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	datastore := database.New(nil)
	be := backend.New(cfg, dbx, datastore, logger)
	return &testEnv{ctx: ctx, cfg: cfg, dbx: dbx, datastore: datastore, be: be, logger: logger}
}

// useStore makes datastore the store of the env's backend and of the
// servers built from then on, for a test that wraps the store to count or
// fail its calls.
func (e *testEnv) useStore(datastore store.Store) {
	e.datastore = datastore
	e.be = backend.New(e.cfg, e.dbx, datastore, e.logger)
}

// tokens returns the signer of the host key at the env's ssh.key_path, as
// the server loads it at start, or nil when there is no key there.
func (e *testEnv) tokens(t testing.TB) *jwk.Signer {
	t.Helper()
	if _, err := os.Stat(e.cfg.SSH.KeyPath); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	kp, err := config.KeyPair(e.cfg)
	if err != nil {
		t.Fatalf("KeyPair: %v", err)
	}
	s, err := jwk.NewSigner(kp, e.cfg.HTTP.PublicURL)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

// writeHostKey writes an SSH host key made with opts (Ed25519 by default)
// under the env's data directory and points ssh.key_path at it, so the servers built from then
// on verify tokens signed with it.
func (e *testEnv) writeHostKey(t testing.TB, opts ...keygen.Option) {
	t.Helper()
	if len(opts) == 0 {
		opts = []keygen.Option{keygen.WithKeyType(keygen.Ed25519)}
	}
	e.cfg.SSH.KeyPath = filepath.Join(e.cfg.DataPath, "ssh", "host_key")
	if _, err := keygen.New(e.cfg.SSH.KeyPath, append(opts, keygen.WithWrite())...); err != nil {
		t.Fatalf("keygen: %v", err)
	}
}

// mintJWT signs a token for user the way `soft jwt` does, with the env's
// host key.
func (e *testEnv) mintJWT(t testing.TB, user proto.User, audience ...string) string {
	t.Helper()
	signed, err := e.tokens(t).Sign(user, audience, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return signed
}

// server returns a server for the env. The removals of repositories its
// pushes create stop when the test ends.
func (e *testEnv) server(t testing.TB) *server {
	t.Helper()
	s := newServer(e.cfg, e.be, e.dbx, e.datastore, e.tokens(t), e.logger)
	t.Cleanup(func() { _ = s.pending.close(context.Background()) })
	return s
}

// router returns the HTTP router of a new server for the env.
func (e *testEnv) router(t testing.TB) http.Handler {
	t.Helper()
	return e.server(t).router()
}

// gitRouter returns a router with only the git routes of a new server for
// the env, so requests pass through withParams and withAccess rather than
// reaching a handler directly.
func (e *testEnv) gitRouter(t testing.TB) *mux.Router {
	t.Helper()
	return e.server(t).gitRouter()
}

// gitRouter returns a router with only the git routes of s.
func (s *server) gitRouter() *mux.Router {
	router := mux.NewRouter()
	s.gitController(router)
	return router
}

// storePasswordHash writes a bcrypt hash of password into username's row,
// salted as older versions salted it, as a database from such a version may
// hold one. Nothing sets a password any more, and no login reads it.
func (e *testEnv) storePasswordHash(t *testing.T, username, password string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password+"salty-soft-serve"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.dbx.ExecContext(e.ctx, e.dbx.Rebind("UPDATE users SET password = ? WHERE username = ?"), string(hash), username)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("set password of %s: %d rows, %v", username, n, err)
	}
}

// routeCaller is what withAccess finds about a request: the caller, the
// repository and the caller's access level. A test that drives a git route
// handler directly gives it one.
type routeCaller struct {
	user  proto.User
	repo  proto.Repository
	level access.Level
}

// serveRoute calls the git route handler h of s as withAccess does once it
// lets r through for rc. The repository name, directory and service come
// from r's URL variables, or from the repository.
func (s *server) serveRoute(h gitHandler, w http.ResponseWriter, r *http.Request, rc routeCaller) {
	vars := mux.Vars(r)
	gr := &gitRequest{
		logger:  s.requestLogger(r),
		name:    vars["repo"],
		dir:     vars["dir"],
		service: gitserve.Service(vars["service"]),
		user:    rc.user,
		repo:    rc.repo,
		level:   rc.level,
	}
	if gr.name == "" && gr.repo != nil {
		gr.name = gr.repo.Name()
	}
	h(s, w, r, gr)
}

// gitRouteFor returns the git route with path.
func gitRouteFor(t *testing.T, path string) gitRoute {
	t.Helper()
	for _, rt := range gitRoutes {
		if rt.path == path {
			return rt
		}
	}
	t.Fatalf("no git route %q", path)
	return gitRoute{}
}

// fixtureAdmin returns the admin account the first migration creates, the
// owner of fixture repositories: the backend refuses a create by a caller
// who may not create, as an anonymous caller may not by default.
func fixtureAdmin(t testing.TB, be *backend.Backend) proto.User {
	t.Helper()
	u, err := be.User(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	return u
}
