package ssh

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"charm.land/log/v2"
	"charm.land/ssh"
	"charm.land/wish/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssh/cmd"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/spf13/cobra"
)

// ErrPermissionDenied is returned when a user is not allowed connect.
var ErrPermissionDenied = fmt.Errorf("permission denied")

// ErrUserLookupFailed is returned when the user that owns the session's
// public key cannot be looked up. It carries no detail on purpose; the cause
// is logged on the server.
var ErrUserLookupFailed = errors.New("could not look up your account, try again later")

// authenticationMiddleware handles authentication.
func (srv *Server) authenticationMiddleware(sh ssh.Handler) ssh.Handler {
	return func(s ssh.Session) {
		// charm.land/ssh gives the session the key that signed, parsed
		// from the permissions x/crypto returned for that key, not the
		// last key the client offered. TestAuthenticationAfterOfferedKey
		// guards that.
		ctx := s.Context()
		be := srv.be
		pk := s.PublicKey()

		// The keyless setting only matters without a key, so a keyed
		// session does not look it up.
		if pk == nil {
			allow, err := be.AllowKeylessSetting(ctx)
			if err != nil {
				logAccessCheckFailure(ctx, srv.logger, err)
				wish.Fatalln(s, ErrAccessCheckFailed)
				return
			}
			if !allow {
				wish.Fatalln(s, ErrPermissionDenied)
				return
			}
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
			var owned *backend.AdminKeyNonAdminOwnerError
			if errors.As(err, &owned) {
				wish.Fatalln(s, owned)
				return
			}
			// Any failure other than an unregistered key, such as a busy
			// or unreachable database, ends the session. Running it as
			// anonymous instead would deny a registered user their own
			// repositories, or with anon-access read-write let them create
			// repositories owned by nobody. The cause is logged here, at
			// Debug when the client went away and canceled the lookup.
			if err != nil && !errors.Is(err, proto.ErrUserNotFound) {
				logUserLookupFailure(ctx, srv.logger, err)
				wish.Fatalln(s, ErrUserLookupFailed)
				return
			}
		}
		ctx.SetValue(proto.ContextKeyUser, user)
		// A connection with neither an account nor an admin key is
		// closed after noAccountLifetime.
		if user != nil || be.IsAdminKey(pk) {
			hasAccount(ctx)
		}

		sh(s)
	}
}

// ErrAccessCheckFailed is sent to a session whose access could not be
// checked, as when the database is down. The cause stays in the server log.
var ErrAccessCheckFailed = errors.New("could not check your access, try again later")

// logAccessCheckFailure logs a failed access check for a session, at Debug
// when the client went away and canceled it.
func logAccessCheckFailure(ctx context.Context, logger *log.Logger, err error) {
	if ctx.Err() != nil {
		logger.Debug("access check canceled", "err", err)
		return
	}
	logger.Error("failed to check the session's access", "err", err)
}

// logUserLookupFailure logs a failed lookup of the session's user, at Debug
// when the client went away and canceled it.
func logUserLookupFailure(ctx context.Context, logger *log.Logger, err error) {
	if ctx.Err() != nil {
		logger.Debug("user lookup canceled", "err", err)
		return
	}
	logger.Error("failed to look up the session's user", "err", err)
}

// MaxCommandLength is the longest exec command, in bytes, the SSH server
// accepts. Every Soft Serve command fits well within it. Splitting a command
// into arguments costs time quadratic in its length, and any client with any
// key reaches the middleware chain, so longer commands are refused before
// anything parses them.
const MaxCommandLength = 4 << 10

// ErrCommandTooLong is returned when an exec command exceeds MaxCommandLength.
var ErrCommandTooLong = fmt.Errorf("command too long: exceeds %d bytes", MaxCommandLength)

// commandLengthMiddleware refuses sessions whose exec command is longer than
// MaxCommandLength, before any middleware or handler parses it.
func commandLengthMiddleware(sh ssh.Handler) ssh.Handler {
	return func(s ssh.Session) {
		if len(s.RawCommand()) > MaxCommandLength {
			wish.Fatalln(s, ErrCommandTooLong)
			return
		}
		sh(s)
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

// commandMiddleware handles git commands and CLI commands.
func (srv *Server) commandMiddleware(sh ssh.Handler) ssh.Handler {
	return func(s ssh.Session) {
		ctx := sessionContext(s)

		args := s.Command()
		rootCmd := newRootCommand(srv.cfg, srv.cmds, args)

		// A session with a pty opens the TUI unless its command is one to
		// run (see runWithPty). A command has no TUI to read the window
		// changes forwardWindowChanges forwards, so they are dropped
		// there; nothing to do here.
		if _, _, ptyReq := s.Pty(); ptyReq && !runWithPty(ctx, srv.be, srv.logger, rootCmd, args) {
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
func runWithPty(ctx context.Context, be *backend.Backend, logger *log.Logger, root *cobra.Command, args []string) bool {
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
	// The auth middleware has already resolved the session user, so reuse
	// it rather than looking the key up again (as the TUI does in openRepo).
	// A lookup that fails runs the word as a command, which then reports
	// its own error: this only chooses between the two.
	r, _, err := be.ReadableForUser(ctx, word, proto.UserFromContext(ctx))
	if err != nil {
		logger.Debug("repository lookup failed, running the word as a command", "word", word, "err", err)
	}
	return r == nil
}

// newRootCommand returns the CLI command tree, built by cmds, for a session
// on a server with cfg running args.
func newRootCommand(cfg *config.Config, cmds *cmd.Commands, args []string) *cobra.Command {
	rootCmd := &cobra.Command{
		Short:        "Soft Serve Plus is a self-hostable Git server for the command line.",
		SilenceUsage: true,
		// The root command has no name of its own: show the ssh
		// invocation instead, so usage lines and errors such as
		// "unknown command" read as what the user typed.
		Annotations: map[string]string{
			cobra.CommandDisplayNameAnnotation: cmd.SSHCommand(cfg),
		},
	}
	rootCmd.CompletionOptions.DisableDefaultCmd = true
	// Cobra would describe the flag with the display name.
	rootCmd.Flags().BoolP("help", "h", false, "help for this command")
	// "Did you mean" suggestions compare an unknown command with every
	// command name by edit distance, which costs milliseconds for a
	// word near MaxCommandLength. No typo of a command name is that
	// long, so leave suggestions out for long arguments.
	rootCmd.DisableSuggestions = slices.ContainsFunc(args, func(a string) bool {
		return len(a) > maxSuggestionArgLength
	})

	rootCmd.AddCommand(
		cmds.GitUploadPackCommand(),
		cmds.GitUploadArchiveCommand(),
		cmds.GitReceivePackCommand(),
		cmds.RepoCommand(),
		cmds.SettingsCommand(),
		cmds.UserCommand(),
		cmds.InfoCommand(),
		cmds.PubkeyCommand(),
		cmds.SetUsernameCommand(),
		cmds.JWTCommand(),
		cmds.TokenCommand(),
	)

	if cfg.LFS.Enabled {
		rootCmd.AddCommand(
			cmds.GitLFSAuthenticateCommand(),
		)

		if cfg.LFS.SSHEnabled {
			rootCmd.AddCommand(
				cmds.GitLFSTransfer(),
			)
		}
	}

	return rootCmd
}

// redacted replaces a secret in a logged command line.
const redacted = "[redacted]"

// redactCommand returns a copy of args that is safe to log: the value of a
// --secret (-s) flag is replaced, and any URL is reduced to its scheme and
// host. Webhook secrets are passed with --secret, and import and webhook URLs
// can carry a token as user info, in the path (Slack, Discord and the like)
// or in the query.
func redactCommand(args []string) []string {
	if len(args) == 0 {
		return args
	}
	out := make([]string, len(args))
	secretNext := false
	for i, arg := range args {
		switch {
		case secretNext:
			out[i] = redacted
			secretNext = false
		case arg == "--secret":
			out[i] = arg
			secretNext = true
		case strings.HasPrefix(arg, "--secret="):
			out[i] = "--secret=" + redacted
		default:
			if n := shorthandSecret(arg); n > 0 {
				// -s, -as and the like take the next argument as the
				// value; -sVALUE, -s=VALUE, -asVALUE and -as=VALUE carry it.
				out[i] = arg[:n]
				if n < len(arg) {
					out[i] += redacted
				} else {
					secretNext = true
				}
				continue
			}
			out[i] = redactURLArg(arg)
		}
	}
	return out
}

// shorthandSecret reports where the value of a -s flag starts in a group of
// short flags such as -s, -sVALUE, -as or -as=VALUE: it returns the index just
// past the "s", or 0 when arg holds no -s. Short flags can be grouped, so an
// "s" after any run of letters counts. This may also match a letter inside
// the value of another short flag (-uhttps://...), which only redacts more
// than needed.
func shorthandSecret(arg string) int {
	if len(arg) < 2 || arg[0] != '-' || arg[1] == '-' {
		return 0
	}
	for j := 1; j < len(arg); j++ {
		c := arg[j]
		if c == 's' {
			return j + 1
		}
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return 0
		}
	}
	return 0
}

// redactURLArg redacts a URL given as a positional argument or as a flag
// value (--url=URL, -uURL, -u=URL). Any other argument is returned unchanged.
func redactURLArg(arg string) string {
	prefix, value := "", arg
	if strings.HasPrefix(arg, "--") {
		if k, v, ok := strings.Cut(arg, "="); ok {
			prefix, value = k+"=", v
		}
	} else if strings.HasPrefix(arg, "-") && len(arg) > 2 {
		prefix, value = arg[:2], arg[2:]
		if strings.HasPrefix(value, "=") {
			prefix, value = prefix+"=", value[1:]
		}
	}
	if !strings.Contains(value, "://") {
		return arg
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return prefix + redacted
	}
	r := u.Scheme + "://" + u.Host
	if u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		r += "/" + redacted
	}
	return prefix + r
}

// sessionContext returns a context derived from the session's context that
// carries the session itself. The values of the session's context belong to
// the connection (see channelContext), and a client multiplexing sessions over one connection (such as
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

// loggingMiddleware logs the ssh connection and command.
func (srv *Server) loggingMiddleware(sh ssh.Handler) ssh.Handler {
	return func(s ssh.Session) {
		ctx := s.Context()
		logger := srv.logger
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
			redactCommand(s.Command()),
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
