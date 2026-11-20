package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
// database, store and backend into the command's context.
func InitBackendContext(cmd *cobra.Command, _ []string) error {
	if err := LoadConfig(cmd); err != nil {
		return err
	}
	ctx := cmd.Context()
	cfg := config.FromContext(ctx)
	if _, err := os.Stat(cfg.DataPath); errors.Is(err, fs.ErrNotExist) {
		// The data directory holds private repositories, the database and
		// secrets, so keep it to the server's user whatever the umask.
		if err := os.MkdirAll(cfg.DataPath, 0o700); err != nil {
			closeLogFile(ctx)
			return fmt.Errorf("create data directory: %w", err)
		}
	}
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	if err != nil {
		closeLogFile(ctx)
		return fmt.Errorf("open database: %w", err)
	}
	dbx.SetPoolLimits(cfg.DB)

	ctx = db.WithContext(ctx, dbx)
	dbstore := database.New()
	ctx = store.WithContext(ctx, dbstore)
	be := backend.New(ctx, cfg, dbx, dbstore)
	ctx = backend.WithContext(ctx, be)

	cmd.SetContext(ctx)

	return nil
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

// InitializeHooks initializes the hooks.
func InitializeHooks(ctx context.Context, cfg *config.Config, be *backend.Backend) error {
	repos, err := be.Repositories(ctx)
	if err != nil {
		return err
	}

	for _, repo := range repos {
		if err := hooks.GenerateHooks(ctx, cfg, repo.Name()); err != nil {
			return err
		}
	}

	return nil
}
