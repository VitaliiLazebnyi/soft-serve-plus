package ssh

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/log/v2"
	"charm.land/ssh"
	"charm.land/wish/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssh/cmd"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"
)

// ErrPermissionDenied is returned when a user is not allowed connect.
var ErrPermissionDenied = fmt.Errorf("permission denied")

// ErrUserLookupFailed is returned when the user that owns the session's
// public key cannot be looked up. It carries no detail on purpose; the cause
// is logged on the server.
var ErrUserLookupFailed = errors.New("could not look up your account, try again later")

// AuthenticationMiddleware handles authentication.
func AuthenticationMiddleware(sh ssh.Handler) ssh.Handler {
	return func(s ssh.Session) {
		// XXX: The authentication key is set in the context but gossh doesn't
		// validate the authentication. We need to verify that the _last_ key
		// that was approved is the one that's being used.

		ctx := s.Context()
		be := backend.FromContext(ctx)

		var pkFp string
		perms := s.Permissions().Permissions
		pk := s.PublicKey()
		if pk != nil {
			// There is no public key stored in the context, public-key auth
			// was never requested, skip
			if perms == nil {
				wish.Fatalln(s, ErrPermissionDenied)
				return
			}

			pkFp = gossh.FingerprintSHA256(pk)
		}

		// Check if the key is the same as the one we have in context
		fp := perms.Extensions["pubkey-fp"]
		if fp != "" && fp != pkFp {
			wish.Fatalln(s, ErrPermissionDenied)
			return
		}

		ac := be.AllowKeyless(ctx)
		publicKeyCounter.WithLabelValues(strconv.FormatBool(ac || pk != nil)).Inc()
		if !ac && pk == nil {
			wish.Fatalln(s, ErrPermissionDenied)
			return
		}

		// Set the auth'd user, or anon, in the context
		var user proto.User
		if pk != nil {
			var err error
			user, err = be.UserByPublicKey(ctx, pk)
			// A listed admin key attached to a non-admin account is
			// refused outright: acting as that account could hand a
			// claimer the key holder's work, and acting as nobody strands
			// a promoted owner. The error says how an admin fixes it.
			// Auth already succeeded with this key, so the session cannot
			// fall back to another key or to keyless access.
			var owned *backend.ErrAdminKeyNonAdminOwner
			if errors.As(err, &owned) {
				wish.Fatalln(s, owned)
				return
			}
			// Any failure other than an unregistered key, such as a busy
			// or unreachable database, ends the session. Running it as
			// anonymous instead would deny a registered user their own
			// repositories, or with anon-access read-write let them create
			// repositories owned by nobody. The backend logs the cause.
			if err != nil && !errors.Is(err, proto.ErrUserNotFound) {
				wish.Fatalln(s, ErrUserLookupFailed)
				return
			}
		}
		ctx.SetValue(proto.ContextKeyUser, user)

		sh(s)
	}
}

// MaxCommandLength is the longest exec command, in bytes, the SSH server
// accepts. Every Soft Serve command fits well within it. Splitting a command
// into arguments costs time quadratic in its length, and any client with any
// key reaches the middleware chain, so longer commands are refused before
// anything parses them.
const MaxCommandLength = 4 << 10

// ErrCommandTooLong is returned when an exec command exceeds MaxCommandLength.
var ErrCommandTooLong = fmt.Errorf("command too long: exceeds %d bytes", MaxCommandLength)

// CommandLengthMiddleware refuses sessions whose exec command is longer than
// MaxCommandLength, before any middleware or handler parses it.
func CommandLengthMiddleware(sh ssh.Handler) ssh.Handler {
	return func(s ssh.Session) {
		if len(s.RawCommand()) > MaxCommandLength {
			wish.Fatalln(s, ErrCommandTooLong)
			return
		}
		sh(s)
	}
}

// ContextMiddleware adds the config, backend, and logger to the session context.
// The session context is shared by every session on the connection, so the
// session itself is added per session by sessionContext instead.
func ContextMiddleware(cfg *config.Config, dbx *db.DB, datastore store.Store, be *backend.Backend, logger *log.Logger) func(ssh.Handler) ssh.Handler {
	return func(sh ssh.Handler) ssh.Handler {
		return func(s ssh.Session) {
			ctx := s.Context()
			ctx.SetValue(config.ContextKey, cfg)
			ctx.SetValue(db.ContextKey, dbx)
			ctx.SetValue(store.ContextKey, datastore)
			ctx.SetValue(backend.ContextKey, be)
			ctx.SetValue(log.ContextKey, logger.WithPrefix("ssh"))
			sh(s)
		}
	}
}

// maxSuggestionArgLength is the longest argument, in bytes, for which the
// command handler still suggests a command name for an unknown command.
const maxSuggestionArgLength = 64

var cliCommandCounter = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "soft_serve",
	Subsystem: "cli",
	Name:      "commands_total",
	Help:      "Total times each command was called",
}, []string{"command"})

// CommandMiddleware handles git commands and CLI commands.
// This middleware must be run after the ContextMiddleware.
func CommandMiddleware(sh ssh.Handler) ssh.Handler {
	return func(s ssh.Session) {
		ctx := sessionContext(s)
		cfg := config.FromContext(ctx)

		args := s.Command()
		rootCmd := newRootCommand(cfg, args)

		// A session with a pty opens the TUI unless its command is one to
		// run (see runWithPty).
		if _, _, ptyReq := s.Pty(); ptyReq && !runWithPty(s, rootCmd, args) {
			sh(s)
			return
		}

		cliCommandCounter.WithLabelValues(commandLabel(rootCmd, args)).Inc()

		rootCmd.SetArgs(args)
		if len(args) == 0 {
			// otherwise it'll default to os.Args, which is not what we want.
			rootCmd.SetArgs([]string{"--help"})
		}
		rootCmd.SetIn(s)
		rootCmd.SetOut(s)
		rootCmd.SetErr(s.Stderr())
		rootCmd.SetContext(ctx)

		if err := rootCmd.ExecuteContext(ctx); err != nil {
			s.Exit(1) //nolint: errcheck
			return
		}
	}
}

// runWithPty reports whether args, sent by a session with a pty, are a CLI
// command to run rather than the TUI to open.
//
// No command opens the TUI, and more than one word cannot be a repository
// name, so it is a command (e.g. `ssh -t host repo list`). A single word is a
// repository to open the TUI on, unless it is a flag or names a top-level
// command (e.g. `ssh -t host info` or `ssh -t host --help`) and the caller
// can read no repository of that name. A repository the caller cannot read
// is treated as missing, so a private repository named like a command and a
// missing one give the same answer.
func runWithPty(s ssh.Session, root *cobra.Command, args []string) bool {
	switch {
	case len(args) == 0:
		return false
	case len(args) > 1:
		return true
	}
	word := args[0]
	if !strings.HasPrefix(word, "-") {
		if l := commandLabel(root, args); l == "" || l == unknownCommand {
			return false
		}
	}
	be := backend.FromContext(s.Context())
	if be == nil {
		return true
	}
	r, _ := be.ReadableByPublicKey(s.Context(), word, s.PublicKey())
	return r == nil
}

// newRootCommand returns the CLI command tree for a session on a server
// with cfg running args.
func newRootCommand(cfg *config.Config, args []string) *cobra.Command {
	rootCmd := &cobra.Command{
		Short:        "Soft Serve Plus is a self-hostable Git server for the command line.",
		SilenceUsage: true,
	}
	rootCmd.CompletionOptions.DisableDefaultCmd = true
	// "Did you mean" suggestions compare an unknown command with every
	// command name by edit distance, which costs milliseconds for a
	// word near MaxCommandLength. No typo of a command name is that
	// long, so leave suggestions out for long arguments.
	rootCmd.DisableSuggestions = slices.ContainsFunc(args, func(a string) bool {
		return len(a) > maxSuggestionArgLength
	})

	rootCmd.SetUsageTemplate(cmd.UsageTemplate)
	rootCmd.SetUsageFunc(cmd.UsageFunc)
	rootCmd.AddCommand(
		cmd.GitUploadPackCommand(),
		cmd.GitUploadArchiveCommand(),
		cmd.GitReceivePackCommand(),
		cmd.RepoCommand(),
		cmd.SettingsCommand(),
		cmd.UserCommand(),
		cmd.InfoCommand(),
		cmd.PubkeyCommand(),
		cmd.SetUsernameCommand(),
		cmd.JWTCommand(),
		cmd.TokenCommand(),
	)

	if cfg.LFS.Enabled {
		rootCmd.AddCommand(
			cmd.GitLFSAuthenticateCommand(),
		)

		if cfg.LFS.SSHEnabled {
			rootCmd.AddCommand(
				cmd.GitLFSTransfer(),
			)
		}
	}

	return rootCmd
}

// sessionContext returns a context derived from the session's context that
// carries the session itself. The session's context belongs to the
// connection, and a client multiplexing sessions over one connection (such as
// OpenSSH with ControlMaster) would otherwise have each session read another
// session's environment, e.g. its GIT_PROTOCOL.
func sessionContext(s ssh.Session) context.Context {
	return context.WithValue(s.Context(), sshutils.ContextKeySession, s)
}

// unknownCommand is the metric label for a command the server does not have.
const unknownCommand = "unknown"

// commandLabel returns the name of the top-level command of root that args
// run, for use as a metric label. Labels come from the command tree, never
// from the client, so the set of series stays fixed: anything that matches
// no command is unknownCommand, and running root itself (no command, or
// only flags) is the empty string.
func commandLabel(root *cobra.Command, args []string) string {
	if len(args) == 0 {
		return ""
	}
	// Execute adds the help command; add it now so it is matched too.
	root.InitDefaultHelpCmd()
	c, _, err := root.Find(args)
	if err != nil || c == nil {
		return unknownCommand
	}
	if c == root {
		return ""
	}
	for c.HasParent() && c.Parent() != root {
		c = c.Parent()
	}
	return c.Name()
}

// LoggingMiddleware logs the ssh connection and command.
func LoggingMiddleware(sh ssh.Handler) ssh.Handler {
	return func(s ssh.Session) {
		ctx := s.Context()
		logger := log.FromContext(ctx).WithPrefix("ssh")
		if logger.GetLevel() > log.DebugLevel {
			// Nothing below is logged, so skip parsing the command.
			sh(s)
			return
		}

		ct := time.Now()
		hpk := sshutils.MarshalAuthorizedKey(s.PublicKey())
		ptyReq, _, isPty := s.Pty()
		addr := s.RemoteAddr().String()
		user := proto.UserFromContext(ctx)
		logArgs := []interface{}{
			"addr",
			addr,
			"cmd",
			s.Command(),
		}

		if user != nil {
			logArgs = append([]interface{}{
				"username",
				user.Username(),
			}, logArgs...)
		}

		if isPty {
			logArgs = append(logArgs,
				"term", ptyReq.Term,
				"width", ptyReq.Window.Width,
				"height", ptyReq.Window.Height,
			)
		}

		if config.IsVerbose() {
			logArgs = append(logArgs,
				"key", hpk,
				"envs", s.Environ(),
			)
		}

		msg := fmt.Sprintf("user %q", s.User())
		logger.Debug(msg+" connected", logArgs...)
		sh(s)
		logger.Debug(msg+" disconnected", append(logArgs, "duration", time.Since(ct))...)
	}
}
