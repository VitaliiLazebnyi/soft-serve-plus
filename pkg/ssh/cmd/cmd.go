package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"
)

// SSHCommand returns the ssh invocation that reaches the server, such as
// "ssh -p 23231 localhost", for help and error output. The root command
// shows it as its name, so every command path starts with it.
func SSHCommand(cfg *config.Config) string {
	hostname := "localhost"
	port := "23231"
	if u, err := url.Parse(cfg.SSH.PublicURL); err == nil {
		hostname = u.Hostname()
		port = u.Port()
	}

	sshCmd := "ssh"
	if port != "" && port != "22" {
		sshCmd += " -p " + port
	}

	return sshCmd + " " + hostname
}

// denyReadAs is the one answer for a caller who may not read a repository,
// whether it is missing or private: anon for anonymous callers (HTTP 401),
// registered for everyone else (HTTP 404).
func denyReadAs(anonymous bool, anon, registered error) error {
	if anonymous {
		return anon
	}
	return registered
}

// denyRead is denyReadAs for repo commands. Server admins always get
// ErrRepoNotFound since nothing is hidden from them.
func denyRead(ctx context.Context) error {
	anonymous := proto.UserFromContext(ctx) == nil && !isServerAdmin(ctx)
	return denyReadAs(anonymous, proto.ErrUnauthorized, proto.ErrRepoNotFound)
}

// repoRunE is the body of a repo-scoped command. repo is the repository
// named by the command's first argument, as withRepo resolved it, and level
// the caller's access level on it.
type repoRunE func(cmd *cobra.Command, args []string, repo proto.Repository, level access.AccessLevel) error

// withRepo returns the RunE of a repo-scoped command: the caller must be
// able to read the repository named by the first argument and hold at
// least need on it, and fn then runs with that repository. Readability is
// decided before existence, so a caller who cannot read gets denyRead's
// answer whether or not the repository exists.
func withRepo(need access.AccessLevel, fn repoRunE) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		repo, level, err := gateRepo(cmd.Context(), args, need)
		if err != nil {
			return err
		}
		if repo == nil {
			return denyRead(cmd.Context())
		}
		return fn(cmd, args, repo, level)
	}
}

// withRepoOrName is withRepo for the commands a server admin may also run
// on a name with a row but no repository directory (delete, rename and
// collab), so they can act on such a row: fn then gets a nil repository
// and acts on the name.
func withRepoOrName(need access.AccessLevel, fn repoRunE) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		repo, level, err := gateRepo(cmd.Context(), args, need)
		if err != nil {
			return err
		}
		return fn(cmd, args, repo, level)
	}
}

// gateRepo looks up the repository named by args and checks that the
// caller may read it and holds at least need on it. The repository is nil
// only for a server admin and a name with no repository behind it.
func gateRepo(ctx context.Context, args []string, need access.AccessLevel) (proto.Repository, access.AccessLevel, error) {
	repo, level, err := backend.FromContext(ctx).ReadableForUser(ctx, repoArg(args), proto.UserFromContext(ctx))
	if err != nil {
		return nil, level, lookupFailed(ctx, log.FromContext(ctx), "look up repository", err)
	}
	if repo == nil && !isServerAdmin(ctx) {
		return nil, level, denyRead(ctx)
	}
	if level < need {
		return nil, level, proto.ErrUnauthorized
	}
	return repo, level, nil
}

// isServerAdmin reports whether the session's caller is a server admin,
// by the backend's rule (Backend.IsServerAdmin): the session user has the
// admin flag, or the session's key is a configured admin key.
func isServerAdmin(ctx context.Context) bool {
	be := backend.FromContext(ctx)
	return be != nil && be.IsServerAdmin(ctx, proto.UserFromContext(ctx))
}

// parseBool parses a boolean command argument. It accepts the values
// strconv.ParseBool does (true/false, t/f, 1/0 in any common case) and
// rejects anything else, so a typo such as "yes" or "ture" fails instead of
// silently meaning false.
func parseBool(s string) (bool, error) {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("invalid boolean value %q: use true or false", s)
	}
	return v, nil
}

// repoArg returns the repository name from a repo-scoped command's arguments.
// Repo-scoped commands always take the repository as their first argument.
func repoArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return utils.SanitizeRepo(args[0])
}

// repoAccessLevel returns the caller's access level for the sanitized name.
// The error is lookupFailed's when the lookup failed.
func repoAccessLevel(ctx context.Context, repo string) (access.AccessLevel, error) {
	_, level, err := backend.FromContext(ctx).ReadableForUser(ctx, repo, proto.UserFromContext(ctx))
	if err != nil {
		return access.NoAccess, lookupFailed(ctx, log.FromContext(ctx), "look up repository", err)
	}
	return level, nil
}

// createFailed returns err, from creating, importing or renaming onto a
// repository, for the client: a failed lookup (proto.ErrLookupFailed) is
// logged and answered with errLookupFailed, as by lookupFailed; anything
// else is returned as it is.
func createFailed(ctx context.Context, what string, err error) error {
	if errors.Is(err, proto.ErrLookupFailed) {
		return lookupFailed(ctx, log.FromContext(ctx), what, err)
	}
	return err
}

// errLookupFailed is returned to the client when a lookup the command needs
// failed, as when the database is down. The cause stays in the server log:
// it can name server paths, and the request was not refused.
var errLookupFailed = errors.New("could not look that up on the server, try again later")

// lookupFailed logs err, from a lookup for what, and returns errLookupFailed
// for the client. A lookup the client canceled by going away is logged at
// Debug.
func lookupFailed(ctx context.Context, logger *log.Logger, what string, err error) error {
	if ctx.Err() != nil {
		logger.Debug("lookup canceled", "what", what, "err", err)
	} else {
		logger.Error("lookup failed", "what", what, "err", err)
	}
	return errLookupFailed
}

// checkIfServerAdmin is the authorization gate for global (non-repo-scoped)
// commands such as `user` and `settings`. It allows server admins only.
//
// Unlike checkIfRepoAdmin, it never consults repository access levels, so it
// cannot be bypassed by creating a repository whose name matches the command
// argument. Attach it to the parent command so that every subcommand,
// including ones added later, is gated by default.
func checkIfServerAdmin(cmd *cobra.Command, _ []string) error {
	if !isServerAdmin(cmd.Context()) {
		return errNotServerAdmin(cmd.Context())
	}
	return nil
}

// errNotServerAdmin returns proto.ErrUnauthorized for a command that only
// server admins may run. The server accepts any key, so the session uses the
// first key the client offers, which may be an agent or default key rather
// than the admin key passed with -i. The error names the key's fingerprint
// and how to make ssh offer only the chosen key. It still matches
// proto.ErrUnauthorized.
//
// Use it only for gates that do not depend on repository state, such as
// checkIfServerAdmin: repository gates must keep giving the same answer
// for a private and a missing repository.
func errNotServerAdmin(ctx context.Context) error {
	return sessionKeyError(ctx, proto.ErrUnauthorized,
		"this command needs a server admin, and the key this session authenticated with (%s) is not an admin's")
}

// checkIfRepoCollab gates create and import, which name a repository that
// should not exist yet, on write access to that name. It cannot hide
// whether the name is taken: with one flat namespace, "already exists" is
// the answer either way.
func checkIfRepoCollab(cmd *cobra.Command, args []string) error {
	level, err := repoAccessLevel(cmd.Context(), repoArg(args))
	if err != nil {
		return err
	}
	if level < access.ReadWriteAccess {
		return proto.ErrUnauthorized
	}
	return nil
}

// errUnknownKey returns proto.ErrUserNotFound for a command that needs the
// caller's account when the session's public key belongs to no user. The
// server accepts any key, so the session uses the first key the client
// offers, which may be an agent or default key rather than the one passed
// with -i. The error names the key's fingerprint and how to make ssh offer
// only the chosen key. It still matches proto.ErrUserNotFound.
func errUnknownKey(ctx context.Context) error {
	return sessionKeyError(ctx, proto.ErrUserNotFound,
		"no user has the key this session authenticated with (%s)")
}

// identitiesOnlyHint ends the errors that name the session key: ssh offers
// agent and default keys before the one passed with -i unless told not to.
const identitiesOnlyHint = "if you passed -i to ssh, also pass -o IdentitiesOnly=yes so ssh offers only that key"

// sessionKeyError wraps base with reason, whose %s is replaced by the SHA256
// fingerprint of the session's public key, followed by identitiesOnlyHint.
// The result still matches base. Without a session key it returns base.
func sessionKeyError(ctx context.Context, base error, reason string) error {
	pk := sshutils.PublicKeyFromContext(ctx)
	if pk == nil {
		return base
	}
	return fmt.Errorf("%w: %s; %s", base, fmt.Sprintf(reason, gossh.FingerprintSHA256(pk)), identitiesOnlyHint)
}

// userByPublicKey returns the user that owns the session's public key. When
// no user owns it, the error is errUnknownKey.
//
// The SSH authentication middleware resolves the key once per session and
// stores the user in the context, so that user is used when present. Only
// when there is none (no key owner, a lookup error, or a context built
// without the middleware) is the key looked up, which keeps the errors
// below.
func userByPublicKey(ctx context.Context) (proto.User, error) {
	if user := proto.UserFromContext(ctx); user != nil {
		return user, nil
	}
	be := backend.FromContext(ctx)
	user, err := be.UserByPublicKey(ctx, sshutils.PublicKeyFromContext(ctx))
	// AdminKeyNonAdminOwnerError also matches proto.ErrUserNotFound, but the
	// key does belong to a user, so it keeps its own message. Over SSH this
	// is unreachable, since the auth middleware refuses those sessions
	// before any command runs; the check is a safeguard should that change.
	var nonAdminOwner *backend.AdminKeyNonAdminOwnerError
	if errors.Is(err, proto.ErrUserNotFound) && !errors.As(err, &nonAdminOwner) {
		return nil, errUnknownKey(ctx)
	}
	if err != nil && !errors.Is(err, proto.ErrUserNotFound) {
		return nil, lookupFailed(ctx, log.FromContext(ctx), "look up the session's user", err)
	}
	return user, err
}
