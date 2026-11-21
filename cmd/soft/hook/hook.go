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

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/spf13/cobra"
)

// ErrNoDataPath is returned when a hook runs without
// SOFT_SERVE_DATA_PATH, i.e. not from a Soft Serve Plus server.
var ErrNoDataPath = errors.New("SOFT_SERVE_DATA_PATH is not set; hooks must be run by git from a Soft Serve Plus server")

// The hook commands are run for a repository whose hooks were generated
// before the generated scripts stopped calling soft (see
// hooks.GenerateHooks); they call them until "soft admin sync-hooks". Each
// does what the generated script does now: post-receive writes the ref
// lines to $SOFT_SERVE_PUSH_REFS, so the server sends the push's webhooks,
// and every hook runs the custom hook. The server does the rest once
// receive-pack has exited, so none opens the database.

// preReceiveRunE runs the custom pre-receive hook with the ref lines.
func preReceiveRunE(c *cobra.Command, args []string) error {
	refs, err := readRefLines(c, hooks.PreReceiveHook)
	if err != nil {
		return err
	}
	return runHookCommand(c, hooks.PreReceiveHook, refs, args)
}

// updateRunE runs the custom update hook with its three arguments.
func updateRunE(c *cobra.Command, args []string) error {
	return runHookCommand(c, hooks.UpdateHook, nil, args)
}

// postReceiveRunE records the ref lines for the server and runs the custom
// post-receive hook with them.
func postReceiveRunE(c *cobra.Command, args []string) error {
	refs, err := readRefLines(c, hooks.PostReceiveHook)
	if err != nil {
		return err
	}
	recordPushRefs(hookLogger(c), refs)
	return runHookCommand(c, hooks.PostReceiveHook, refs, args)
}

// postUpdateRunE runs the custom post-update hook with the ref names.
func postUpdateRunE(c *cobra.Command, args []string) error {
	return runHookCommand(c, hooks.PostUpdateHook, nil, args)
}

// hookLogger returns the command's logger, with the repository the server
// runs the hook for.
func hookLogger(c *cobra.Command) *log.Logger {
	// This is set in the server before invoking git-receive-pack.
	return log.FromContext(c.Context()).With("repo", os.Getenv("SOFT_SERVE_REPO_NAME"))
}

// readRefLines reads the "<old> <new> <ref>" lines git gives hook on stdin.
// A line longer than the scanner takes, or a failed read, fails the hook
// rather than letting it act, and run the custom hook, on part of the refs.
func readRefLines(c *cobra.Command, hook string) ([]byte, error) {
	var buf bytes.Buffer
	scanner := bufio.NewScanner(c.InOrStdin())
	for scanner.Scan() {
		buf.Write(scanner.Bytes())
		buf.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		hookLogger(c).Error(fmt.Sprintf("failed to read %s hook input", hook), "err", err)
		return nil, fmt.Errorf("read %s hook input: %w", hook, err)
	}
	return buf.Bytes(), nil
}

// runHookCommand runs the custom hook in the data path's hooks directory
// with input on stdin and args, if there is one.
func runHookCommand(c *cobra.Command, hook string, input []byte, args []string) error {
	ctx := c.Context()
	path := filepath.Join(config.FromContext(ctx).DataPath, "hooks", hook)
	if err := runCustomHook(ctx, path, bytes.NewReader(input), c.OutOrStdout(), c.ErrOrStderr(), args...); err != nil {
		hookLogger(c).Error("failed to run custom hook", "err", err)
		return err
	}
	return nil
}

// recordPushRefs appends the post-receive input refs to the file
// $SOFT_SERVE_PUSH_REFS names, as the generated post-receive script does. A
// failure is logged: the push has happened, and only its webhooks are lost.
func recordPushRefs(logger *log.Logger, refs []byte) {
	path := os.Getenv(hooks.PushRefsEnv)
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err == nil {
		_, err = f.Write(refs)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		logger.Error("failed to record the pushed refs for the server", "err", err)
	}
}

// NewCommand returns a new hook command with its subcommands.
//
// Each call builds a new command tree. Cobra keeps a subcommand's context
// from its first run, so a tree run twice would run the second time with
// the first run's config.
func NewCommand() *cobra.Command {
	command := &cobra.Command{
		Use:    "hook",
		Short:  "Run git server hooks",
		Long:   "Handles Soft Serve Plus git server hooks.",
		Hidden: true,
		PersistentPreRunE: func(c *cobra.Command, _ []string) error {
			// Without SOFT_SERVE_DATA_PATH the data path defaults to "data"
			// in the working directory, which for a hook is the repository,
			// and a custom hook would be looked for there. The server always
			// sets it.
			if os.Getenv("SOFT_SERVE_DATA_PATH") == "" {
				log.FromContext(c.Context()).Error("refusing to run a hook without SOFT_SERVE_DATA_PATH")
				return ErrNoDataPath
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
			RunE:  preReceiveRunE,
		},
		&cobra.Command{
			Use:   hooks.UpdateHook,
			Short: "Run git update hook",
			Args:  cobra.ExactArgs(3),
			RunE:  updateRunE,
		},
		&cobra.Command{
			Use:   hooks.PostReceiveHook,
			Short: "Run git post-receive hook",
			RunE:  postReceiveRunE,
		},
		&cobra.Command{
			Use:   hooks.PostUpdateHook,
			Short: "Run git post-update hook",
			RunE:  postUpdateRunE,
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
