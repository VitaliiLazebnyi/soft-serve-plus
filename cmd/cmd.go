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

// NewLogger returns the logger cfg's log settings describe. If the log file
// cannot be opened, it returns the error together with a logger that writes
// to stderr, so the caller can keep logging. The caller closes the file.
func NewLogger(cfg *config.Config) (*log.Logger, *os.File, error) {
	if cfg == nil {
		return nil, nil, config.ErrNilConfig
	}
	logger := log.NewWithOptions(os.Stderr, log.Options{ReportTimestamp: true})
	logger.SetTimeFormat(cfg.Log.TimeFormat)

	switch {
	case config.IsVerbose():
		logger.SetReportCaller(true)
		fallthrough
	case config.IsDebug():
		logger.SetLevel(log.DebugLevel)
	}

	switch strings.ToLower(cfg.Log.Format) {
	case "json":
		logger.SetFormatter(log.JSONFormatter)
	case "logfmt":
		logger.SetFormatter(log.LogfmtFormatter)
	case "text":
		logger.SetFormatter(log.TextFormatter)
	}

	var f *os.File
	if cfg.Log.Path != "" {
		// A relative log path resolves into the data directory, which may
		// not exist yet on a first run (the logger is created before
		// anything else touches the data path). Create the parent
		// directory private (0o700), the same mode the data path is
		// created with, so a first run does not leave the data directory
		// readable by other users.
		if err := os.MkdirAll(filepath.Dir(cfg.Log.Path), 0o700); err != nil {
			return logger, nil, err
		}
		var err error
		f, err = os.OpenFile(cfg.Log.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
		if err != nil {
			return logger, nil, err
		}
		logger.SetOutput(f)
	}

	return logger, f, nil
}

// logFileKey is the context key of the log file LoadConfig opened.
type logFileKey struct{}

// LoadConfig loads the server config with config.Load and opens the server
// log it describes, and puts both into c's context. Only the commands that
// act on the server's data run it, through InitBackendContext: other
// commands, such as --version, man and a local browse, neither read the
// config nor create the log file. A context that already holds a config,
// as a test's does, keeps it and its logger.
func LoadConfig(c *cobra.Command) error {
	ctx := c.Context()
	if config.FromContext(ctx) != nil {
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger, f, err := NewLogger(cfg)
	if err != nil {
		// logger still writes to stderr.
		logger.Warn("failed to open log file, logging to stderr", "err", err)
	}
	log.SetDefault(logger)
	ctx = config.WithContext(ctx, cfg)
	ctx = log.WithContext(ctx, logger)
	if f != nil {
		ctx = context.WithValue(ctx, logFileKey{}, f)
	}
	c.SetContext(ctx)
	return nil
}

// closeLogFile closes the log file LoadConfig opened, if it did.
func closeLogFile(ctx context.Context) {
	if f, ok := ctx.Value(logFileKey{}).(*os.File); ok {
		f.Close() //nolint: errcheck
	}
}

// InitBackendContext loads the config (see LoadConfig) and opens the
// database, store and backend into the command's context, creating the
// data directory and the database when they do not exist yet. Only serve
// creates them; the maintenance commands use OpenBackendContext.
func InitBackendContext(cmd *cobra.Command, _ []string) error {
	return initBackend(cmd, true)
}

// OpenBackendContext is InitBackendContext for the maintenance commands
// (admin migrate, rollback and sync-hooks): it refuses to run when the data
// directory or the SQLite database does not exist, rather than create an
// empty one and act on it while the server's real one is elsewhere (R46).
func OpenBackendContext(cmd *cobra.Command, _ []string) error {
	return initBackend(cmd, false)
}

// initBackend is InitBackendContext, or OpenBackendContext when create is
// false.
func initBackend(cmd *cobra.Command, create bool) error {
	if err := LoadConfig(cmd); err != nil {
		return err
	}
	ctx := cmd.Context()
	cfg := config.FromContext(ctx)
	var err error
	if create {
		err = createDataPath(cfg)
	} else {
		err = checkDataPath(cfg)
	}
	if err != nil {
		closeLogFile(ctx)
		return err
	}
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource, db.Options{
		Logger:       log.FromContext(ctx),
		Trace:        config.IsVerbose(),
		MaxOpenConns: cfg.DB.MaxOpenConns,
	})
	if err != nil {
		closeLogFile(ctx)
		return fmt.Errorf("open database: %w", err)
	}

	ctx = db.WithContext(ctx, dbx)
	dbstore := database.New()
	ctx = store.WithContext(ctx, dbstore)
	be := backend.New(ctx, cfg, dbx, dbstore)
	ctx = backend.WithContext(ctx, be)

	cmd.SetContext(ctx)

	return nil
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

// CloseDBContext closes the database, and the log file LoadConfig opened.
func CloseDBContext(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	defer closeLogFile(ctx)
	dbx := db.FromContext(ctx)
	if dbx != nil {
		if err := dbx.Close(); err != nil {
			return fmt.Errorf("close database: %w", err)
		}
	}

	return nil
}

// CloseDBOnError wraps a command's RunE so that the database is closed when
// it fails: cobra does not run PersistentPostRunE, which closes it
// otherwise, when RunE returns an error.
func CloseDBOnError(run func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(c *cobra.Command, args []string) error {
		err := run(c, args)
		if err != nil {
			_ = CloseDBContext(c, args)
		}
		return err
	}
}

// InitializeHooks writes the hooks of every repository. A repository whose
// directory is missing is skipped with a warning: writing its hooks would
// create a directory holding only hooks/, which the server then takes for
// the repository (R47). A repository whose hooks cannot be written does not
// stop the others; the errors are returned together.
func InitializeHooks(ctx context.Context, cfg *config.Config, be *backend.Backend) error {
	repos, err := be.Repositories(ctx)
	if err != nil {
		return err
	}

	logger := log.FromContext(ctx)
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
		if err := hooks.GenerateHooks(ctx, cfg, repo.Name()); err != nil {
			errs = append(errs, fmt.Errorf("hooks of %s: %w", repo.Name(), err))
		}
	}
	return errors.Join(errs...)
}
