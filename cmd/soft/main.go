// Command soft is the Soft Serve Plus binary: it runs the server (soft
// serve), its maintenance commands (soft admin) and a local repository
// browser (soft browse). Its hook commands (soft hook) are run only by the
// hook scripts of a repository whose hooks an older server generated, until
// soft admin sync-hooks or soft serve --sync-hooks replaces them with scripts
// that call no soft process.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/cmd/soft/admin"
	"github.com/charmbracelet/soft-serve/cmd/soft/browse"
	"github.com/charmbracelet/soft-serve/cmd/soft/hook"
	"github.com/charmbracelet/soft-serve/cmd/soft/serve"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	mcobra "github.com/muesli/mango-cobra"
	"github.com/muesli/roff"
	"github.com/spf13/cobra"
)

var (
	// Version contains the application version number. It's set via ldflags
	// when building.
	Version = ""

	// CommitSHA contains the SHA of the commit that this application was built
	// against. It's set via ldflags when building.
	CommitSHA = ""

	rootCmd = &cobra.Command{
		Use:          "soft [PATH]",
		Short:        "A self-hostable Git server for the command line",
		Long:         "Soft Serve Plus is a self-hostable Git server for the command line.",
		SilenceUsage: true,
		Args:         rootArgs,
		// Same as cobra's default, which only applies to its own
		// unknown command error.
		SuggestionsMinimumDistance: 2,
		RunE: func(cmd *cobra.Command, args []string) error {
			return browse.Command.RunE(cmd, args)
		},
	}

	// hookEnv is what hookCmd runs with; run fills it in for a hook.
	hookEnv = &hook.Env{}

	// hookCmd is the hook command in rootCmd's tree.
	hookCmd = hook.NewCommand(hookEnv)

	manCmd = &cobra.Command{
		Use:    "man",
		Short:  "Generate man pages",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manPage, err := mcobra.NewManPage(1, rootCmd) //.
			if err != nil {
				return err
			}
			date, err := sourceDate(os.Getenv("SOURCE_DATE_EPOCH"))
			if err != nil {
				return err
			}

			manPage = manPage.WithSection("Copyright", "(C) 2021-2023 Charmbracelet, Inc.\n"+
				"Soft Serve Plus changes (C) their contributors.\n"+
				"Released under MIT license.")
			_, err = fmt.Fprintln(cmd.OutOrStdout(), manPage.Build(datedDocument{roff.NewDocument(), date}))
			return err
		},
	}
)

// sourceDate returns the time SOURCE_DATE_EPOCH names (seconds since the Unix
// epoch, https://reproducible-builds.org/specs/source-date-epoch/), or the
// current time when it is empty. The release build sets it to the commit time,
// so the man page does not depend on the day it is built.
func sourceDate(epoch string) (time.Time, error) {
	if epoch == "" {
		return time.Now(), nil
	}
	sec, err := strconv.ParseInt(epoch, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid SOURCE_DATE_EPOCH %q: %w", epoch, err)
	}
	return time.Unix(sec, 0).UTC(), nil
}

// datedDocument is a roff document whose title heading carries date instead
// of the time the page is built.
type datedDocument struct {
	*roff.Document
	date time.Time
}

// Heading writes the title heading with d.date.
func (d datedDocument) Heading(section uint, title, description string, _ time.Time) {
	d.Document.Heading(section, title, description, d.date)
}

// rootArgs lets "soft PATH" browse the repository at PATH like "soft browse
// PATH". An argument that is not an existing path is reported as an unknown
// command, since it is more likely a mistyped one.
func rootArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.MaximumNArgs(1)(cmd, args); err != nil {
		return err
	}
	if len(args) == 1 {
		if _, err := os.Stat(args[0]); err != nil {
			var sb strings.Builder
			fmt.Fprintf(&sb, "unknown command %q for %q", args[0], cmd.CommandPath())
			if s := cmd.SuggestionsFor(args[0]); !cmd.DisableSuggestions && len(s) > 0 {
				sb.WriteString("\n\nDid you mean this?\n")
				for _, c := range s {
					fmt.Fprintf(&sb, "\t%s\n", c)
				}
			}
			return errors.New(sb.String())
		}
	}
	return nil
}

func init() {
	rootCmd.AddCommand(
		manCmd,
		serve.NewCommand(loadServerConfig),
		hookCmd,
		admin.NewCommand(loadServerConfig),
		browse.Command,
	)
	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	if len(CommitSHA) >= 7 {
		vt := rootCmd.VersionTemplate()
		rootCmd.SetVersionTemplate(vt[:len(vt)-1] + " (" + CommitSHA[0:7] + ")\n")
	}
	if Version == "" {
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Sum != "" {
			Version = info.Main.Version
		} else {
			Version = "unknown (built from source)"
		}
	}
	rootCmd.Version = Version

	webhook.Version = Version
}

func main() {
	superviseIfInit()
	os.Exit(run(context.Background(), os.Args[1:]))
}

// run runs the soft command args and returns the exit status. Only the
// commands that act on the server's data load the server config and open
// its log: serve and admin's subcommands do it themselves, with
// loadServerConfig, and a hook here, from the file the server writes for
// hooks. Any other command, such as --version, man or a local browse,
// reads neither.
func run(ctx context.Context, args []string) int {
	rootCmd.SetArgs(args)
	if isHookCommand(args) {
		f, ok := loadHookEnv(hookEnv)
		if !ok {
			return 1
		}
		if f != nil {
			defer f.Close() //nolint: errcheck
		}
	}

	// GOMAXPROCS is left to the runtime, which follows the cgroup CPU limit.

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		return 1
	}
	return 0
}

// loadServerConfig is the cmd.Loader of serve and admin's subcommands: it
// loads the server config with config.Load, opens the log it describes and
// makes that the default logger. A log file that cannot be opened is
// warned about, and the log goes to stderr.
func loadServerConfig() (*config.Config, *log.Logger, func() error, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, nil, err
	}
	logger, f, err := newLogger(cfg)
	if err != nil {
		// logger still writes to stderr.
		logger.Warn("failed to open log file, logging to stderr", "err", err)
	}
	log.SetDefault(logger)
	var closeLog func() error
	if f != nil {
		closeLog = f.Close
	}
	return cfg, logger, closeLog, nil
}

// loadHookEnv fills env with the config and logger of a `soft hook`
// command, and returns the hook log file to close. It reports false when
// the config cannot be loaded; the error is then in the hook log and the
// client is told only that the server failed.
func loadHookEnv(env *hook.Env) (*os.File, bool) {
	cfg, cfgErr := loadHookConfig()
	logger, f, err := newLogger(cfg)
	if err != nil {
		// A hook's stderr is relayed to the git client as "remote:"
		// lines. Don't send it server paths or server logs; drop the
		// hook logs instead.
		logger.SetOutput(io.Discard)
	}
	log.SetDefault(logger)

	if cfgErr != nil {
		// A hook's stderr goes to whoever pushes, and a config error can
		// quote config values. The error goes to the hook log; the client
		// is told only that the server failed.
		logger.Error("hook: failed to load the server config", "err", cfgErr)
		fmt.Fprintln(os.Stderr, errHookInternal)
		if f != nil {
			f.Close() //nolint: errcheck
		}
		return nil, false
	}

	env.Config, env.Logger = cfg, logger
	return f, true
}

// isHookCommand reports whether args run the hook command or one of its
// subcommands.
func isHookCommand(args []string) bool {
	c, _, err := rootCmd.Find(args)
	if err != nil {
		return false
	}
	for ; c != nil; c = c.Parent() {
		if c == hookCmd {
			return true
		}
	}
	return false
}
