package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/spf13/cobra"
)

// Loader loads the server config and the logger a command that acts on the
// server's data runs with. closeLog, which may be nil, closes what the
// logger writes to, such as the log file. soft's main loads them from the
// config file and the environment; a test gives its own with Given.
type Loader func() (cfg *config.Config, logger *log.Logger, closeLog func() error, err error)

// Given returns a Loader that returns cfg and logger, and closes nothing. A
// nil logger is log.Default().
func Given(cfg *config.Config, logger *log.Logger) Loader {
	return func() (*config.Config, *log.Logger, func() error, error) {
		if cfg == nil {
			return nil, nil, nil, config.ErrNilConfig
		}
		if logger == nil {
			logger = log.Default()
		}
		return cfg, logger, nil, nil
	}
}

// Env is what a command that acts on the server's data runs with: the
// config and logger its Loader gave, and the database, store and backend
// OpenEnv opened for them. Close closes it.
type Env struct {
	Config  *config.Config
	Logger  *log.Logger
	DB      *db.DB
	Store   store.Store
	Backend *backend.Backend

	closeLog func() error
}

// OpenEnv loads the config and logger with load and opens the database,
// store and backend for them. A Loader that gives no config is
// config.ErrNilConfig; no logger is log.Default(). With create, as for serve, it creates the
// data directory and the database when they do not exist yet. Without it,
// as for the maintenance commands (admin migrate, rollback and sync-hooks),
// it refuses to run when the data directory or the SQLite database does not
// exist, rather than create an empty one and act on it while the server's
// real one is elsewhere (R46).
func OpenEnv(ctx context.Context, load Loader, create bool) (*Env, error) {
	cfg, logger, closeLog, err := load()
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = log.Default()
	}
	env := &Env{Config: cfg, Logger: logger, closeLog: closeLog}
	if cfg == nil {
		env.Close() //nolint: errcheck
		return nil, config.ErrNilConfig
	}
	if create {
		err = createDataPath(cfg)
	} else {
		err = checkDataPath(cfg)
	}
	if err != nil {
		env.Close() //nolint: errcheck
		return nil, err
	}
	env.DB, err = db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource, db.Options{
		Logger:       logger,
		Trace:        config.IsVerbose(),
		MaxOpenConns: cfg.DB.MaxOpenConns,
	})
	if err != nil {
		env.Close() //nolint: errcheck
		return nil, fmt.Errorf("open database: %w", err)
	}
	env.Store = database.New(logger)
	env.Backend = backend.New(cfg, env.DB, env.Store, logger)
	return env, nil
}

// Close closes the database, and then what the logger writes to.
func (e *Env) Close() error {
	var err error
	if e.DB != nil {
		if cerr := e.DB.Close(); cerr != nil {
			err = fmt.Errorf("close database: %w", cerr)
		}
	}
	if e.closeLog != nil {
		e.closeLog() //nolint: errcheck
	}
	return err
}

// WithEnv returns a cobra RunE that opens an Env (see OpenEnv), runs run
// with it and closes it. When run fails, its error is returned and the
// Env's close error dropped; otherwise the close error is returned.
func WithEnv(load Loader, create bool, run func(c *cobra.Command, args []string, env *Env) error) func(*cobra.Command, []string) error {
	return func(c *cobra.Command, args []string) error {
		env, err := OpenEnv(c.Context(), load, create)
		if err != nil {
			return err
		}
		if err := run(c, args, env); err != nil {
			env.Close() //nolint: errcheck
			return err
		}
		return env.Close()
	}
}

// createDataPath creates the data directory if it does not exist.
func createDataPath(cfg *config.Config) error {
	if _, err := os.Stat(cfg.DataPath); errors.Is(err, fs.ErrNotExist) {
		// The data directory holds private repositories, the database and
		// secrets, so keep it to the server's user whatever the umask.
		if err := os.MkdirAll(cfg.DataPath, 0o700); err != nil {
			return fmt.Errorf("create data directory: %w", err)
		}
	}
	return nil
}

// checkDataPath returns an error when the data directory, or the SQLite
// database file, does not exist. The error names SOFT_SERVE_DATA_PATH,
// since a shell without it, unlike the service, uses ./data.
func checkDataPath(cfg *config.Config) error {
	if _, err := os.Stat(cfg.DataPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("data directory %s does not exist; set SOFT_SERVE_DATA_PATH to the server's data directory (only soft serve creates it)", cfg.DataPath)
		}
		return fmt.Errorf("data directory: %w", err)
	}
	if cfg.DB.Driver != "sqlite" {
		return nil
	}
	file := sqliteFile(cfg.DB.DataSource)
	if file == "" {
		return nil
	}
	if _, err := os.Stat(file); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("database %s does not exist; check SOFT_SERVE_DATA_PATH and db.data_source (only soft serve creates it)", file)
		}
		return fmt.Errorf("database: %w", err)
	}
	return nil
}

// sqliteFile returns the database file a SQLite data source names, as
// Validate resolved it, or "" for an in-memory database.
func sqliteFile(dsn string) string {
	name, query, _ := strings.Cut(dsn, "?")
	rest, isURI := strings.CutPrefix(name, "file:")
	if !isURI {
		if name == ":memory:" {
			return ""
		}
		return name
	}
	if q, err := url.ParseQuery(query); err == nil && q.Get("mode") == "memory" {
		return ""
	}
	path := rest
	if u, err := url.Parse(name); err == nil {
		path = u.Path
		if u.Opaque != "" {
			if p, err := url.PathUnescape(u.Opaque); err == nil {
				path = p
			}
		}
	}
	if path == "" || path == ":memory:" {
		return ""
	}
	// file:///C:/data/soft-serve.db has the path /C:/data/soft-serve.db.
	if len(path) >= 3 && path[0] == '/' && path[2] == ':' && filepath.VolumeName(path[1:]) != "" {
		path = path[1:]
	}
	return filepath.FromSlash(path)
}

// InitializeHooks writes the hooks of every repository. A repository whose
// directory is missing is skipped with a warning: writing its hooks would
// create a directory holding only hooks/, which the server then takes for
// the repository (R47). A repository whose hooks cannot be written does not
// stop the others; the errors are returned together.
func InitializeHooks(ctx context.Context, cfg *config.Config, be *backend.Backend, logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}
	repos, err := be.Repositories(ctx)
	if err != nil {
		return err
	}

	var errs []error
	for _, repo := range repos {
		dir := hooks.RepoPath(cfg, repo.Name())
		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			logger.Warn("skipping the hooks of a repository whose directory is missing", "repo", repo.Name(), "path", dir)
			continue
		} else if err != nil {
			errs = append(errs, fmt.Errorf("hooks of %s: %w", repo.Name(), err))
			continue
		}
		if err := hooks.Generate(ctx, cfg, repo.Name()); err != nil {
			errs = append(errs, fmt.Errorf("hooks of %s: %w", repo.Name(), err))
		}
	}
	return errors.Join(errs...)
}
