// Package hook is the soft hook command, which the hook scripts of older
// generated repositories run, until soft admin sync-hooks rewrites them.
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

// The hook commands are run by the hook scripts of a repository whose hooks
// an older server generated, until "soft admin sync-hooks" replaces them
// with the scripts of hooks.Generate, which call no soft process. Each
// does what such a script does: post-receive writes the ref lines to
// $SOFT_SERVE_PUSH_REFS, so the server sends the push's webhooks, and every
// hook runs the custom hook. The server does the rest once receive-pack has
// exited, so none opens the database.

// Env is what the hook commands run with: the server's config and the hook
// log. soft's main fills it in before it runs a hook command, from the file
// the server writes for hooks.
type Env struct {
	Config *config.Config
	// Logger is the hook log; nil is log.Default().
	Logger *log.Logger
}

// preReceiveRunE runs the custom pre-receive hook with the ref lines.
func (e *Env) preReceiveRunE(c *cobra.Command, args []string) error {
	refs, err := e.readRefLines(c, hooks.PreReceiveHook)
	if err != nil {
		return err
	}
	return e.runHookCommand(c, hooks.PreReceiveHook, refs, args)
}

// updateRunE runs the custom update hook with its three arguments.
func (e *Env) updateRunE(c *cobra.Command, args []string) error {
	return e.runHookCommand(c, hooks.UpdateHook, nil, args)
}

// postReceiveRunE records the ref lines for the server and runs the custom
// post-receive hook with them.
func (e *Env) postReceiveRunE(c *cobra.Command, args []string) error {
	refs, err := e.readRefLines(c, hooks.PostReceiveHook)
	if err != nil {
		return err
	}
	recordPushRefs(e.hookLogger(), refs)
	return e.runHookCommand(c, hooks.PostReceiveHook, refs, args)
}

// postUpdateRunE runs the custom post-update hook with the ref names.
func (e *Env) postUpdateRunE(c *cobra.Command, args []string) error {
	return e.runHookCommand(c, hooks.PostUpdateHook, nil, args)
}

// logger returns the hook log.
func (e *Env) logger() *log.Logger {
	if e.Logger == nil {
		return log.Default()
	}
	return e.Logger
}

// hookLogger returns the hook log, with the repository the server runs the
// hook for.
func (e *Env) hookLogger() *log.Logger {
	// This is set in the server before invoking git-receive-pack.
	return e.logger().With("repo", os.Getenv("SOFT_SERVE_REPO_NAME"))
}

// readRefLines reads the "<old> <new> <ref>" lines git gives hook on stdin.
// A line longer than the scanner takes, or a failed read, fails the hook
// rather than letting it act, and run the custom hook, on part of the refs.
func (e *Env) readRefLines(c *cobra.Command, hook string) ([]byte, error) {
	var buf bytes.Buffer
	scanner := bufio.NewScanner(c.InOrStdin())
	for scanner.Scan() {
		buf.Write(scanner.Bytes())
		buf.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		e.hookLogger().Error(fmt.Sprintf("failed to read %s hook input", hook), "err", err)
		return nil, fmt.Errorf("read %s hook input: %w", hook, err)
	}
	return buf.Bytes(), nil
}

// runHookCommand runs the custom hook in the data path's hooks directory
// with input on stdin and args, if there is one.
func (e *Env) runHookCommand(c *cobra.Command, hook string, input []byte, args []string) error {
	if e.Config == nil {
		e.hookLogger().Error("refusing to run a hook without the server config")
		return config.ErrNilConfig
	}
	ctx := c.Context()
	path := filepath.Join(e.Config.DataPath, "hooks", hook)
	if err := runCustomHook(ctx, path, bytes.NewReader(input), c.OutOrStdout(), c.ErrOrStderr(), args...); err != nil {
		e.hookLogger().Error("failed to run custom hook", "err", err)
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
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // the server names the file for this hook; whoever sets the hook's environment runs as the server's user already
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

// NewCommand returns a new hook command with its subcommands, which run
// with env as it is when they run.
//
// Each call builds a new command tree, so its flags start from their
// defaults: cobra does not reset a flag a previous run set.
func NewCommand(env *Env) *cobra.Command {
	command := &cobra.Command{
		Use:    "hook",
		Short:  "Run git server hooks",
		Long:   "Handles Soft Serve Plus git server hooks.",
		Hidden: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			// Without SOFT_SERVE_DATA_PATH the data path defaults to "data"
			// in the working directory, which for a hook is the repository,
			// and a custom hook would be looked for there. The server always
			// sets it.
			if os.Getenv("SOFT_SERVE_DATA_PATH") == "" {
				env.logger().Error("refusing to run a hook without SOFT_SERVE_DATA_PATH")
				return ErrNoDataPath
			}
			return nil
		},
	}

	// The --config flag is deprecated and ignored: a hook reads the
	// server's config from the file $SOFT_SERVE_HOOK_CONFIG names (see
	// hooks.WriteConfig).
	var configPath string
	command.PersistentFlags().StringVar(&configPath, "config", "", "path to config file (deprecated)")
	_ = command.PersistentFlags().MarkDeprecated("config", "it is ignored; run \"soft admin sync-hooks\" to regenerate the hooks without it")

	command.AddCommand(
		&cobra.Command{
			Use:   hooks.PreReceiveHook,
			Short: "Run git pre-receive hook",
			RunE:  env.preReceiveRunE,
		},
		&cobra.Command{
			Use:   hooks.UpdateHook,
			Short: "Run git update hook",
			Args:  cobra.ExactArgs(3),
			RunE:  env.updateRunE,
		},
		&cobra.Command{
			Use:   hooks.PostReceiveHook,
			Short: "Run git post-receive hook",
			RunE:  env.postReceiveRunE,
		},
		&cobra.Command{
			Use:   hooks.PostUpdateHook,
			Short: "Run git post-update hook",
			RunE:  env.postUpdateRunE,
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
