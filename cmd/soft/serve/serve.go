// Package serve is the soft serve command: it opens the server's data and
// runs the SSH, HTTP, git daemon and stats servers and the scheduled jobs
// until it is stopped.
package serve

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/cmd"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/spf13/cobra"
)

// NewCommand returns a new serve command, which loads the server's config
// and logger with load (see cmd.OpenEnv).
//
// Each call builds a new command, so its flags start from their defaults:
// cobra does not reset a flag a previous run set.
func NewCommand(load cmd.Loader) *cobra.Command {
	var syncHooks bool
	command := &cobra.Command{
		Use:   "serve",
		Short: "Start the server",
		Args:  cobra.NoArgs,
		RunE: cmd.WithEnv(load, true, func(c *cobra.Command, _ []string, env *cmd.Env) error {
			return runServe(c.Context(), env, syncHooks)
		}),
	}
	command.Flags().BoolVarP(&syncHooks, "sync-hooks", "", false, "synchronize hooks for all repositories before running the server")

	return command
}

// runServe starts the server env describes and serves until it stops or a
// signal stops it, then shuts it down. With syncHooks it first regenerates
// the hooks of every repository.
func runServe(ctx context.Context, env *cmd.Env, syncHooks bool) error {
	// Use the config the database and backend were set up with,
	// so hooks and directories land under the same data path.
	cfg := env.Config
	if err := prepareDataPath(cfg, env.Logger); err != nil {
		return err
	}

	if err := migrate.Migrate(ctx, env.DB, cfg, env.Logger); err != nil {
		return fmt.Errorf("migration error: %w", err)
	}
	// Reconcile initial_admin_keys on every start, not only on the
	// first migration, so keys added to the config later become
	// admin keys. "soft admin migrate" does not do this.
	if err := migrate.SyncAdminKeys(ctx, env.DB, cfg, env.Logger); err != nil {
		return fmt.Errorf("sync initial admin keys: %w", err)
	}

	s, err := NewServer(ctx, cfg, env.DB, env.Store, env.Backend, env.Logger)
	if err != nil {
		return fmt.Errorf("start server: %w", err)
	}

	if syncHooks {
		if err := cmd.InitializeHooks(ctx, cfg, env.Backend, env.Logger); err != nil {
			_ = s.Close()
			return fmt.Errorf("initialize hooks: %w", err)
		}
	}

	sigs := notifySignals()
	defer sigs.stop()

	// Test builds add an endpoint that stops the server, since
	// Windows doesn't support signals. Release builds don't have it.
	testStop := make(chan struct{})
	s.HTTPServer.Server.Handler = withTestStop(s.HTTPServer.Server.Handler, testStop)

	started := make(chan error, 1)
	go func(ctx context.Context) {
		started <- s.Start(ctx)
	}(ctx)
	// The error the server stopped with is returned only after the
	// shutdown, which runs either way.
	serverErr := waitForStop(s, started, sigs, testStop)

	// Waiting for webhook deliveries can take up to a minute; a
	// second interrupt or terminate signal stops that wait.
	abort, stopWaiting := abortOnSignal(sigs.term, s.logger)
	defer stopWaiting()
	// A context of its own: the goroutine above still holds ctx.
	drainCtx, cancel := context.WithTimeout(ctx, drainTimeout)
	defer cancel()
	return errors.Join(serverErr, s.Shutdown(drainCtx, abort))
}

// prepareDataPath warns about the settings the server ignores, and writes
// what the data directory needs before the server starts: the default config
// file, the custom hooks directory with its sample, the log directory and
// the hook config, and removes stale push refs files.
func prepareDataPath(cfg *config.Config, logger *log.Logger) error {
	warnIgnoredSettings(logger, cfg)
	if !cfg.Exist() {
		if err := cfg.WriteDefaultFile(); err != nil {
			return fmt.Errorf("write config file: %w", err)
		}
	}

	// Create custom hooks directory if it doesn't exist
	customHooksPath := filepath.Join(cfg.DataPath, "hooks")
	if _, err := os.Stat(customHooksPath); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(customHooksPath, 0o700); err != nil {
			return fmt.Errorf("create hooks directory: %w", err)
		}
		// Generate update hook example without executable permissions
		hookPath := filepath.Join(customHooksPath, "update.sample")
		//nolint: gosec
		if err := os.WriteFile(hookPath, []byte(updateHookExample), 0o644); err != nil {
			return fmt.Errorf("failed to generate update hook example: %w", err)
		}
	}

	// Create log directory if it doesn't exist
	logPath := filepath.Join(cfg.DataPath, "log")
	if err := os.MkdirAll(logPath, 0o700); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}

	// A server that stopped mid-push can leave the push's refs file behind.
	if n, err := hooks.RemoveStalePushRefs(cfg); err != nil {
		logger.Warn("failed to remove stale push refs files", "removed", n, "err", err)
	} else if n > 0 {
		logger.Info("removed stale push refs files", "removed", n)
	}

	// `soft hook`, which hooks generated by older versions still run, reads
	// the config the server runs with from here, not config.yaml.
	return hooks.WriteConfig(cfg)
}

// signals receives the signals soft serve handles: an interrupt or
// terminate signal on term stops it, and SIGHUP on hup reloads the TLS
// certificates. signal.Notify drops a signal its channel has no room for,
// so each kind has its own channel: a stop is not lost behind SIGHUPs not
// read yet, and a stop and the second one that aborts the shutdown's wait
// both fit.
type signals struct {
	term chan os.Signal
	hup  chan os.Signal
}

// notifySignals starts delivering the signals soft serve handles.
func notifySignals() *signals {
	s := &signals{term: make(chan os.Signal, 2), hup: make(chan os.Signal, 1)}
	signal.Notify(s.term, os.Interrupt, syscall.SIGTERM)
	signal.Notify(s.hup, syscall.SIGHUP)
	return s
}

// stop stops the delivery, so a command run again in the same process, as
// in tests, does not leave handlers behind.
func (s *signals) stop() {
	signal.Stop(s.term)
	signal.Stop(s.hup)
}

// waitForStop waits until the server stops by itself, and returns its
// error, or until a stop signal or the test endpoint asks it to stop, and
// returns nil. A SIGHUP meanwhile reloads the TLS certificates.
func waitForStop(s *Server, started <-chan error, sigs *signals, testStop <-chan struct{}) error {
	for {
		select {
		case err := <-started:
			if err != nil {
				return fmt.Errorf("server error: %w", err)
			}
			return nil
		case <-sigs.hup:
			s.logger.Info("received SIGHUP signal, reloading TLS certificates if enabled")
			if err := s.ReloadCertificates(); err != nil {
				s.logger.Error("failed to reload TLS certificates", "err", err)
			}
		case <-sigs.term:
			return nil
		case <-testStop:
			return nil
		}
	}
}

// abortOnSignal returns a context that is cancelled when sigs delivers a
// signal: the second interrupt or terminate signal, the first having
// started the shutdown. Call the returned function to stop watching.
func abortOnSignal(sigs <-chan os.Signal, logger *log.Logger) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case sig := <-sigs:
			logger.Warn("received second signal, not waiting for webhook deliveries", "signal", sig)
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// warnIgnoredSettings warns about each setting cfg's file or environment
// sets that the server ignores: a config file key no setting is read from,
// such as a misspelt one, and a removed setting, with what the server does
// instead.
func warnIgnoredSettings(logger *log.Logger, cfg *config.Config) {
	for _, k := range cfg.UnknownKeys() {
		logger.Warn("ignoring unknown config key", "key", k.Key, "line", k.Line, "file", cfg.FilePath())
	}
	for _, r := range cfg.RemovedSettings() {
		msg := "ignoring removed setting; unset the variable"
		if r.InFile {
			msg = "ignoring removed setting; delete it from the config file"
		}
		logger.Warn(msg, "setting", r.Key, "set_by", r.Source, "now", r.Now)
	}
}

const updateHookExample = `#!/bin/sh
#
# An example hook script to echo information about the push
# and send it to the client.
#
# To enable this hook, rename this file to "update" and make it executable.

refname="$1"
oldrev="$2"
newrev="$3"

# Safety check
if [ -z "$GIT_DIR" ]; then
        echo "Don't run this script from the command line." >&2
        echo " (if you want, you could supply GIT_DIR then run" >&2
        echo "  $0 <ref> <oldrev> <newrev>)" >&2
        exit 1
fi

if [ -z "$refname" -o -z "$oldrev" -o -z "$newrev" ]; then
        echo "usage: $0 <ref> <oldrev> <newrev>" >&2
        exit 1
fi

# Check types
# if $newrev is 0000...0000, it's a commit to delete a ref.
zero=$(git hash-object --stdin </dev/null | tr '[0-9a-f]' '0')
if [ "$newrev" = "$zero" ]; then
        newrev_type=delete
else
        newrev_type=$(git cat-file -t $newrev)
fi

echo "Hi from Soft Serve Plus update hook!"
echo
echo "Repository: $SOFT_SERVE_REPO_NAME"
echo "RefName: $refname"
echo "Change Type: $newrev_type"
echo "Old SHA1: $oldrev"
echo "New SHA1: $newrev"

exit 0
`
