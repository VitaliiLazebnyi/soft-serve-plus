package hook

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/cmd"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/spf13/cobra"
)

var (
	// errInternalServerError indicates that an internal server error occurred.
	errInternalServerError = errors.New("internal server error")

	// ErrNoDataPath is returned when a hook runs without
	// SOFT_SERVE_DATA_PATH, i.e. not from a Soft Serve Plus server.
	ErrNoDataPath = errors.New("SOFT_SERVE_DATA_PATH is not set; hooks must be run by git from a Soft Serve Plus server")

	// hooksRunE runs a git hook. Git hooks read the config from the
	// environment, based on $SOFT_SERVE_DATA_PATH, which the binary parses
	// when it starts.
	hooksRunE = cmd.CloseDBOnError(func(c *cobra.Command, args []string) error {
		ctx := c.Context()
		hks := backend.FromContext(ctx)
		cfg := config.FromContext(ctx)

		// This is set in the server before invoking git-receive-pack/git-upload-pack
		repoName := os.Getenv("SOFT_SERVE_REPO_NAME")

		logger := log.FromContext(ctx).With("repo", repoName)

		stdin := c.InOrStdin()
		stdout := c.OutOrStdout()
		stderr := c.ErrOrStderr()

		cmdName := c.Name()
		customHookPath := filepath.Join(cfg.DataPath, "hooks", cmdName)

		var buf bytes.Buffer
		opts := make([]hooks.HookArg, 0)

		switch cmdName {
		case hooks.PreReceiveHook, hooks.PostReceiveHook:
			scanner := bufio.NewScanner(stdin)
			for scanner.Scan() {
				buf.Write(scanner.Bytes())
				buf.WriteByte('\n')
				fields := strings.Fields(scanner.Text())
				if len(fields) != 3 {
					logger.Error(fmt.Sprintf("invalid %s hook input", cmdName), "input", scanner.Text())
					continue
				}
				opts = append(opts, hooks.HookArg{
					OldSha:  fields[0],
					NewSha:  fields[1],
					RefName: fields[2],
				})
			}
			// A line longer than the scanner takes, or a failed read, ends
			// the loop early. The hook then fails rather than acting, and
			// running the custom hook, on part of the refs.
			if err := scanner.Err(); err != nil {
				logger.Error(fmt.Sprintf("failed to read %s hook input", cmdName), "err", err)
				return fmt.Errorf("read %s hook input: %w", cmdName, err)
			}

			switch cmdName {
			case hooks.PreReceiveHook:
				hks.PreReceive(ctx, stdout, stderr, repoName, opts)
			case hooks.PostReceiveHook:
				hks.PostReceive(ctx, stdout, stderr, repoName, opts)
			}
		case hooks.UpdateHook:
			// updateCmd takes exactly three arguments.
			hks.Update(ctx, stdout, stderr, repoName, hooks.HookArg{
				RefName: args[0],
				OldSha:  args[1],
				NewSha:  args[2],
			})
		case hooks.PostUpdateHook:
			hks.PostUpdate(ctx, stdout, stderr, repoName, args...)
		}

		// Custom hooks
		if err := runCustomHook(ctx, customHookPath, &buf, stdout, stderr, args...); err != nil {
			logger.Error("failed to run custom hook", "err", err)
			return err
		}

		return nil
	})
)

// NewCommand returns a new hook command with its subcommands.
//
// Each call builds a new command tree. Cobra keeps a subcommand's context
// from its first run, and InitBackendContext stores the database and
// backend in it, so a tree run twice would run the second time with the
// first run's config and closed database.
func NewCommand() *cobra.Command {
	command := &cobra.Command{
		Use:    "hook",
		Short:  "Run git server hooks",
		Long:   "Handles Soft Serve Plus git server hooks.",
		Hidden: true,
		PersistentPreRunE: func(c *cobra.Command, args []string) error {
			logger := log.FromContext(c.Context())
			// Without SOFT_SERVE_DATA_PATH the data path defaults to "data"
			// in the working directory, which for a hook is the repository:
			// opening the backend would create a data directory and an
			// empty database inside it. The server always sets it.
			if os.Getenv("SOFT_SERVE_DATA_PATH") == "" {
				logger.Error("refusing to run a hook without SOFT_SERVE_DATA_PATH")
				return ErrNoDataPath
			}
			if err := cmd.InitBackendContext(c, args); err != nil {
				logger.Error("failed to initialize backend context", "err", err)
				return errInternalServerError
			}

			return nil
		},
		PersistentPostRunE: func(c *cobra.Command, args []string) error {
			// The hook has done its work by now, so a failure to close the
			// database is only logged: failing here would make git reject a
			// push that pre-receive or update accepted.
			if err := cmd.CloseDBContext(c, args); err != nil {
				log.FromContext(c.Context()).Error("failed to close backend", "err", err)
			}

			return nil
		},
	}

	// The --config flag is deprecated and ignored: git hooks read the
	// config from the environment, based on $SOFT_SERVE_DATA_PATH.
	var configPath string
	command.PersistentFlags().StringVar(&configPath, "config", "", "path to config file (deprecated)")
	_ = command.PersistentFlags().MarkDeprecated("config", "it is ignored; run \"soft admin sync-hooks\" to regenerate the hooks without it")

	command.AddCommand(
		&cobra.Command{
			Use:   hooks.PreReceiveHook,
			Short: "Run git pre-receive hook",
			RunE:  hooksRunE,
		},
		&cobra.Command{
			Use:   hooks.UpdateHook,
			Short: "Run git update hook",
			Args:  cobra.ExactArgs(3),
			RunE:  hooksRunE,
		},
		&cobra.Command{
			Use:   hooks.PostReceiveHook,
			Short: "Run git post-receive hook",
			RunE:  hooksRunE,
		},
		&cobra.Command{
			Use:   hooks.PostUpdateHook,
			Short: "Run git post-update hook",
			RunE:  hooksRunE,
		},
	)

	return command
}

func runCommand(ctx context.Context, in io.Reader, out, errOut io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = errOut
	return cmd.Run()
}

func runCustomHook(ctx context.Context, path string, in io.Reader, out, errOut io.Writer, args ...string) error {
	name, hookArgs, ok, err := customHookCommand(runtime.GOOS, path)
	if err != nil || !ok {
		return err
	}

	return runCommand(ctx, in, out, errOut, name, append(hookArgs, args...)...)
}

// customHookCommand returns the program and leading arguments that run the
// custom hook at path on goos, or ok false when there is no hook to run.
//
// Elsewhere a hook runs only when it is executable, as git does. Windows
// has no execute bit and cannot start an extensionless script, so there a
// hook file is run with sh, as git for Windows runs its hooks. Its sh is on
// PATH while git runs hooks.
func customHookCommand(goos, path string) (name string, args []string, ok bool, err error) {
	stat, err := os.Stat(path)
	if err != nil || stat.IsDir() {
		return "", nil, false, nil
	}
	if goos != "windows" {
		if stat.Mode()&0o111 == 0 {
			return "", nil, false, nil
		}
		return path, nil, true, nil
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		return "", nil, false, fmt.Errorf("custom hook %s: no sh to run it with: %w", path, err)
	}
	return sh, []string{path}, true, nil
}
