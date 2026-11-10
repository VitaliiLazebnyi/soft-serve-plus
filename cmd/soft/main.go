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

	"charm.land/log/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/soft-serve/cmd/soft/admin"
	"github.com/charmbracelet/soft-serve/cmd/soft/browse"
	"github.com/charmbracelet/soft-serve/cmd/soft/hook"
	"github.com/charmbracelet/soft-serve/cmd/soft/serve"
	"github.com/charmbracelet/soft-serve/pkg/config"
	logr "github.com/charmbracelet/soft-serve/pkg/log"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/version"
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

	// CommitDate contains the date of the commit that this application was
	// built against. It's set via ldflags when building.
	CommitDate = ""

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

	manCmd = &cobra.Command{
		Use:    "man",
		Short:  "Generate man pages",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			manPage, err := mcobra.NewManPage(1, rootCmd) //.
			if err != nil {
				return err
			}

			manPage = manPage.WithSection("Copyright", "(C) 2021-2023 Charmbracelet, Inc.\n"+
				"Soft Serve Plus changes (C) their contributors.\n"+
				"Released under MIT license.")
			fmt.Println(manPage.Build(roff.NewDocument()))
			return nil
		},
	}
)

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
	if noColor, _ := strconv.ParseBool(os.Getenv("SOFT_SERVE_NO_COLOR")); noColor {
		common.DefaultColorProfile = colorprofile.NoTTY
	}

	rootCmd.AddCommand(
		manCmd,
		serve.Command,
		hook.Command,
		admin.Command,
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

	version.Version = Version
}

func main() {
	ctx := context.Background()
	cfg := config.DefaultConfig()
	if err := cfg.CheckConfigLocation(); err != nil {
		log.Fatal(err)
	}
	// Parse reads the file and then the environment.
	parse := cfg.ParseEnv
	if cfg.Exist() {
		parse = cfg.Parse
	}
	if err := parse(); err != nil {
		log.Fatal(err)
	}

	ctx = config.WithContext(ctx, cfg)
	logger, f, err := logr.NewLogger(cfg)
	if err != nil {
		if isHookCommand(os.Args[1:]) {
			// A hook's stderr is relayed to the git client as "remote:"
			// lines. Don't send it server paths or server logs; drop the
			// hook logs instead.
			logger.SetOutput(io.Discard)
		} else {
			// logger still writes to stderr.
			logger.Warn("failed to open log file, logging to stderr", "err", err)
		}
	}

	ctx = log.WithContext(ctx, logger)
	if f != nil {
		defer f.Close() //nolint: errcheck
	}

	// Set global logger
	log.SetDefault(logger)

	// GOMAXPROCS is left to the Go runtime: since Go 1.25 it follows the
	// container's cgroup CPU limit and updates it when the limit changes.

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

// isHookCommand reports whether args run the hook command or one of its
// subcommands.
func isHookCommand(args []string) bool {
	c, _, err := rootCmd.Find(args)
	if err != nil {
		return false
	}
	for ; c != nil; c = c.Parent() {
		if c == hook.Command {
			return true
		}
	}
	return false
}
