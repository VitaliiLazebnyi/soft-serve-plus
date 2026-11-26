// Package backendtest builds the fixture that tests of a server share: a
// config with a data directory of the test's own, a migrated database from
// dbtest (Postgres when SOFT_SERVE_DB_DRIVER is "postgres", SQLite
// otherwise), its store and a backend over them.
package backendtest

import (
	"context"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/internal/dbtest"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
)

// Env is a test server's state.
type Env struct {
	// Ctx is the context of the test's own calls; it carries nothing.
	Ctx context.Context
	// Cfg is validated. Its DB section names the test database, and its
	// data directory is empty but for what the migration and backend wrote.
	Cfg     *config.Config
	DB      *db.DB
	Store   store.Store
	Backend *backend.Backend
	// Logger is the backend's logger as given; nil means log.Default().
	Logger *log.Logger
}

// Option changes how New builds an Env.
type Option func(*options)

type options struct {
	base   *config.Config
	config func(*config.Config)
	store  func(store.Store) store.Store
	logger *log.Logger
}

// UseConfig makes cfg the Env's config, for a test that builds its own: New
// keeps cfg's data directory, sets its DB section to the test database and
// validates it.
func UseConfig(cfg *config.Config) Option {
	return func(o *options) { o.base = cfg }
}

// WithConfig calls f on the config before it is validated and the database
// migrated, so that f can set, for example, the initial admin keys.
func WithConfig(f func(*config.Config)) Option {
	return func(o *options) { o.config = f }
}

// WithStore makes the store wrap(the database store), for a test that counts
// or fails store calls.
func WithStore(wrap func(store.Store) store.Store) Option {
	return func(o *options) { o.store = wrap }
}

// WithLogger makes logger the backend's logger.
func WithLogger(logger *log.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// New returns a new Env. The database is closed, and dropped on Postgres,
// when the test is done: after the cleanups the test registers later, such
// as a server's.
func New(tb testing.TB, opts ...Option) *Env {
	tb.Helper()
	ctx := context.Background()
	dbx, dataSource, err := dbtest.OpenSource(ctx, tb)
	if err != nil {
		tb.Fatal(err)
	}
	e, err := build(ctx, dbx, dataSource, tb.TempDir(), opts)
	if err != nil {
		tb.Fatal(err)
	}
	return e
}

// NewUnmanaged is New for a caller with no testing.TB, such as a TestMain:
// the data directory is dataPath, and the caller calls remove when it is
// done, which closes the database and drops it on Postgres.
func NewUnmanaged(dataPath string, opts ...Option) (e *Env, remove func() error, err error) {
	ctx := context.Background()
	dbx, dataSource, remove, err := dbtest.OpenUnmanaged(ctx, dataPath)
	if err != nil {
		return nil, nil, err
	}
	if e, err = build(ctx, dbx, dataSource, dataPath, opts); err != nil {
		_ = remove()
		return nil, nil, err
	}
	return e, remove, nil
}

func build(ctx context.Context, dbx *db.DB, dataSource, dataPath string, opts []Option) (*Env, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	cfg := o.base
	if cfg == nil {
		cfg = config.DefaultConfig()
		cfg.DataPath = dataPath
	}
	cfg.DB.Driver = dbx.DriverName()
	cfg.DB.DataSource = dataSource
	if o.config != nil {
		o.config(cfg)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := migrate.Migrate(ctx, dbx, cfg, nil); err != nil {
		return nil, err
	}
	var st store.Store = database.New(nil)
	if o.store != nil {
		st = o.store(st)
	}
	return &Env{
		Ctx:     ctx,
		Cfg:     cfg,
		DB:      dbx,
		Store:   st,
		Backend: backend.New(cfg, dbx, st, o.logger),
		Logger:  o.logger,
	}, nil
}
