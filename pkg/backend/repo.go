package backend

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
)

// RemotePolicy returns the policy for git remotes supplied on behalf of user:
// the operator's ssh allowlist, and, for server admins, leave to pick any ssh
// host when no allowlist is configured. A nil user gets only the allowlist.
//
// Only the user's stored admin flag counts. It is meant for callers with no
// live session, such as the mirror job acting for a repository's owner. A
// caller acting for a live session must also honour an admin key on the
// session context (see IsServerAdmin); ImportRepository does so through
// importRemotePolicy.
func RemotePolicy(cfg *config.Config, user proto.User) ssrf.GitRemotePolicy {
	return ssrf.GitRemotePolicy{
		AllowedSSHHosts: cfg.SSH.AllowedHosts(),
		AllowAnySSHHost: user != nil && user.IsAdmin(),
	}
}

// errMirrorOwnerNotAdmin explains a refused ssh mirror, at import or when
// mirroring is switched on later. It wraps proto.ErrUnauthorized, so callers
// that test for that still match. The allowlist is offered with its caveat:
// once ssh.client_allowed_hosts is non-empty it binds admins too, so adding
// one host narrows every admin's ssh reach to the list.
var errMirrorOwnerNotAdmin = fmt.Errorf("%w: an ssh mirror is synced with its owner's privileges, and the owner is not an admin; make the owner an admin, or list the host in ssh.client_allowed_hosts (a non-empty list also limits admins to the hosts it names)", proto.ErrUnauthorized)

// errMirrorImportOwnerNotAdmin is errMirrorOwnerNotAdmin at import time,
// where a plain import is a third way out.
var errMirrorImportOwnerNotAdmin = fmt.Errorf("%w, or import without --mirror", errMirrorOwnerNotAdmin)

// errLFSEndpointUnsupported is returned by ImportRepository when LFS objects
// are asked for from an endpoint they can't be fetched from: one reached over
// ssh, including the remote itself when it is an ssh or scp-style one.
var errLFSEndpointUnsupported = errors.New("lfs objects are fetched over http or https only; give an http(s) --lfs-endpoint")

// importRemotePolicy returns the policy for the remote and LFS endpoint that
// user, acting live on ctx, supplies to ImportRepository.
//
// The caller's own privilege counts: the stored admin flag or an admin key
// on the session (see IsServerAdmin). A mirror is also synced later by the
// mirror job, which has no session and re-validates every remote with
// RemotePolicy for the repository's owner. So for a mirror the leave to use
// any ssh host is granted only when that later check will grant it too:
// the owner is the user when there is one, and the default admin otherwise
// (see CreateRepository). Without this, a non-admin account holding an
// admin key could create a mirror that the job then skips on every run.
// mirrorRestricted reports whether the mirror rule withdrew a leave the
// caller would otherwise have had.
func (d *Backend) importRemotePolicy(ctx context.Context, user proto.User, mirror bool) (policy ssrf.GitRemotePolicy, mirrorRestricted bool) {
	policy = RemotePolicy(d.cfg, user)
	live := d.IsServerAdmin(ctx, user)
	policy.AllowAnySSHHost = live
	if mirror && user != nil && !user.IsAdmin() {
		policy.AllowAnySSHHost = false
		mirrorRestricted = live
	}
	return policy, mirrorRestricted
}

// validateImportRemote validates a user-supplied remote and maps the outcome
// to the errors the user sees: refused for want of privilege, or not a
// usable remote. Render the result with RemoteGitEnv and apply it to the
// clone subprocess, or the validation does not hold.
func (d *Backend) validateImportRemote(ctx context.Context, remote string, policy ssrf.GitRemotePolicy) (ssrf.ValidatedGitRemote, error) {
	v, err := d.validateGitRemote(ctx, remote, policy)
	switch {
	case errors.Is(err, ssrf.ErrSSHRequiresAdmin):
		return ssrf.ValidatedGitRemote{}, proto.ErrUnauthorized
	case err != nil:
		return ssrf.ValidatedGitRemote{}, fmt.Errorf("%w: %w", proto.ErrInvalidRemote, err)
	}
	return v, nil
}

// redactRemote strips any user info and query from a remote URL so it can be
// logged: either can hold a token. scp-style remotes pass through unchanged.
func redactRemote(remote string) string {
	return utils.RedactURL(remote)
}

// checkCanCreate refuses the creation of a new repository name by user, the
// caller acting on ctx, unless the level they hold on a name that does not
// exist (BaseAccessLevelForUser) is read-write or above. That level is where
// the creation rule is stated: admin_only_create, the anonymous level, and
// admin keys (see IsServerAdmin). The SSH and web handlers compare the same
// level for the name they were given, which is also how they keep a private
// repository and a missing one alike; this is the backend's own guard, so a
// caller that skips those checks still cannot create. A nil user without an
// admin key is anonymous.
func (d *Backend) checkCanCreate(ctx context.Context, user proto.User) error {
	level, err := d.BaseAccessLevelForUser(ctx, user)
	if err != nil {
		return fmt.Errorf("%w: may %s create: %w", proto.ErrLookupFailed, callerName(user), err)
	}
	if level < access.ReadWriteAccess {
		return proto.ErrUnauthorized
	}
	return nil
}

// callerName names user in an error: the username, or "anonymous".
func callerName(user proto.User) string {
	if user == nil {
		return "anonymous"
	}
	return user.Username()
}

// AboutRepo is the repository whose README the TUI shows on its About tab
// (see pkg/ui/pages/selection). It speaks for the server to every TUI user,
// so only a server admin may create it or rename a repository to it, and the
// TUI shows it only while a server admin owns it.
const AboutRepo = ".soft-serve"

// AboutRepoOwnedByAdmin reports whether r, the AboutRepo, is owned by a
// server admin who created it. The TUI shows its README only then. A
// repository an anonymous caller created is recorded with the default admin
// as owner (see registerRepository), so it is refused here: one created
// before the name was reserved for admins, while anon-access allowed
// creating repositories, would otherwise still be shown. So is one whose
// owner is no longer an admin.
func (d *Backend) AboutRepoOwnedByAdmin(ctx context.Context, r proto.Repository) bool {
	rr, ok := r.(*repo)
	if !ok || rr.repo.Anonymous || r.UserID() == 0 {
		return false
	}
	owner, err := d.UserByID(ctx, r.UserID())
	if err != nil {
		if !errors.Is(err, proto.ErrUserNotFound) {
			d.logLookupError(ctx, "failed to look up the about repository's owner, not showing its readme", err)
		}
		return false
	}
	return owner.IsAdmin()
}

// errAboutRepoReserved refuses a non-admin's claim on AboutRepo.
var errAboutRepoReserved = fmt.Errorf("%w: only an admin can create the %s repository", proto.ErrUnauthorized, AboutRepo)

// checkCanClaim refuses user taking the new repository name: the
// creation rule (see checkCanCreate), and AboutRepo, which only a
// server admin (see IsServerAdmin) may take whatever that setting says.
// AboutRepo is matched ignoring case: on a case-insensitive filesystem
// (default macOS and Windows) a variant such as ".Soft-Serve" claims the
// same directory, which would lock the admin out of the real name.
func (d *Backend) checkCanClaim(ctx context.Context, user proto.User, name string) error {
	if strings.EqualFold(name, AboutRepo) && !d.IsServerAdmin(ctx, user) {
		return errAboutRepoReserved
	}
	return d.checkCanCreate(ctx, user)
}

// repoLimited reports whether max_repos_per_user applies to user creating
// a repository: a limit is set and the caller is not a server admin (see
// IsServerAdmin). Decide it on the caller's own context, which carries the
// session's public key.
func (d *Backend) repoLimited(ctx context.Context, user proto.User) bool {
	return d.cfg.MaxReposPerUser > 0 && !d.IsServerAdmin(ctx, user)
}

// countRepos returns how many repositories user has created. Repositories
// created without an account (a nil user) count together as one user's.
func (d *Backend) countRepos(ctx context.Context, h db.Handler, user proto.User) (int64, error) {
	if user == nil || user.ID() == 0 {
		return d.store.CountAnonymousRepos(ctx, h)
	}
	return d.store.CountUserRepos(ctx, h, user.ID())
}

// checkRepoLimit refuses user, already at max_repos_per_user, before
// anything is created for a new repository: no directory, git init or
// hooks to undo. It is an early answer only: the count is not locked, so
// registerRepository checks again in the transaction that records the
// repository, which is what keeps concurrent creates within the limit.
func (d *Backend) checkRepoLimit(ctx context.Context, user proto.User) error {
	n, err := d.countRepos(ctx, d.db, user)
	if err != nil {
		return err
	}
	if n >= int64(d.cfg.MaxReposPerUser) {
		return d.errRepoLimit(user)
	}
	return nil
}

// errRepoLimit is the error for user being at max_repos_per_user.
func (d *Backend) errRepoLimit(user proto.User) error {
	if user == nil || user.ID() == 0 {
		return fmt.Errorf("%w: anonymous users may create %d repositories in all; delete one or ask an admin", proto.ErrRepoLimitReached, d.cfg.MaxReposPerUser)
	}
	return fmt.Errorf("%w: each user may create %d repositories; delete one or ask an admin", proto.ErrRepoLimitReached, d.cfg.MaxReposPerUser)
}

// DefaultPrivate returns whether a repository that user creates without
// choosing a visibility is private: the default_private setting. Callers
// pass the result as RepositoryOptions.Private for push-to-create, and for
// repo create and repo import without --private.
//
// A repository created without an account is owned by the default admin
// (see registerRepository), and nobody but its owner, admins and
// collaborators can access a private one, so a private default would lock
// its anonymous creator out at once. Over HTTP the push that created it
// would itself be refused. So with default_private set, an anonymous caller
// who is not a server admin (see IsServerAdmin) is refused with
// proto.ErrAnonymousPrivate before anything is created. Such a caller can
// still choose a public repository explicitly where the command allows it.
func (d *Backend) DefaultPrivate(ctx context.Context, user proto.User) (bool, error) {
	if !d.cfg.DefaultPrivate {
		return false, nil
	}
	if (user == nil || user.ID() == 0) && !d.IsServerAdmin(ctx, user) {
		return false, proto.ErrAnonymousPrivate
	}
	return true, nil
}

// CreateRepository creates a new repository. An error that is not a
// refusal (see clientError) is logged and returned as proto.ErrServerFault.
func (d *Backend) CreateRepository(ctx context.Context, name string, user proto.User, opts proto.RepositoryOptions) (proto.Repository, error) {
	r, err := d.createUserRepository(ctx, name, user, opts)
	if err != nil {
		return nil, d.clientError(ctx, "create", name, err)
	}
	return r, nil
}

// createUserRepository is CreateRepository without the mapping of errors.
func (d *Backend) createUserRepository(ctx context.Context, name string, user proto.User, opts proto.RepositoryOptions) (proto.Repository, error) {
	if err := d.checkCanClaim(ctx, user, utils.SanitizeRepo(name)); err != nil {
		return nil, err
	}
	// Report an invalid name before the limit, as ImportRepository does.
	// createRepository sanitizes and validates again; both are idempotent.
	name = utils.SanitizeRepo(name)
	if err := utils.ValidateRepo(name); err != nil {
		return nil, err
	}
	limited := d.repoLimited(ctx, user)
	if limited {
		if err := d.checkRepoLimit(ctx, user); err != nil {
			return nil, err
		}
	}
	return d.createRepository(ctx, name, user, opts, limited)
}

// CreateSystemRepository creates a repository on the server's own behalf,
// attributed to user. It is not subject to AdminOnlyCreate or
// MaxReposPerUser, so it is only for internal bootstrap (such as default_repo
// on boot) and must never be reached from a user request.
func (d *Backend) CreateSystemRepository(ctx context.Context, name string, user proto.User, opts proto.RepositoryOptions) (proto.Repository, error) {
	return d.createRepository(ctx, name, user, opts, false)
}

// createRepository creates an empty repository. limited says whether
// max_repos_per_user applies (see repoLimited).
func (d *Backend) createRepository(ctx context.Context, name string, user proto.User, opts proto.RepositoryOptions, limited bool) (proto.Repository, error) {
	name = utils.SanitizeRepo(name)
	if err := utils.ValidateRepo(name); err != nil {
		return nil, err
	}

	rp := d.repoPath(name)
	if err := claimRepoPath(rp); err != nil {
		return nil, err
	}

	// The callers log a failure: CreateRepository through clientError, and
	// openRepoError the case it maps.
	if _, err := git.Init(rp, true); err != nil {
		d.removeRepoDir(rp) //nolint:errcheck
		return nil, openRepoError(d.logger, err, rp)
	}

	return d.registerRepository(ctx, name, user, opts, limited)
}

// claimRepoPath creates the repository directory, failing if anything already
// occupies it. The filesystem decides what "already" means, which is what
// makes this safe on case- and normalization-insensitive volumes.
func claimRepoPath(rp string) error {
	return withRepoParent(rp, func() error {
		if err := os.Mkdir(rp, os.ModePerm); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return proto.ErrRepoExist
			}
			return err
		}
		return nil
	})
}

// repoParentAttempts bounds how often withRepoParent recreates a parent
// directory that a concurrent delete or rename removed.
const repoParentAttempts = 5

// withRepoParent creates the parent directories of the repository path p and
// then runs place, which puts something at p. A delete or rename of another
// repository in the same group removes parents it leaves empty (see
// removeEmptyRepoParents), so a parent can vanish between its creation and
// place; place then fails with fs.ErrNotExist and is tried again with the
// parents recreated.
func withRepoParent(p string, place func() error) error {
	var err error
	for range repoParentAttempts {
		if err = os.MkdirAll(filepath.Dir(p), os.ModePerm); err != nil {
			return err
		}
		if err = place(); !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return err
}

// removeRepoDir removes the repository directory rp and then the parent
// directories it leaves empty.
func (d *Backend) removeRepoDir(rp string) error {
	if err := os.RemoveAll(rp); err != nil {
		return err
	}
	d.removeEmptyRepoParents(rp)
	return nil
}

// removeEmptyRepoParents removes the directories between the repository path
// rp and the repos directory that are empty, deepest first, so deleting or
// renaming away a nested repository such as "a/b" does not leave an empty
// repos/a behind. It stops at the first directory that is not empty or cannot
// be removed, and never touches the repos directory itself. Only real
// directories are removed: a symlink an administrator put in the tree is
// left alone even when what it points to is empty. Failures are not errors:
// an empty directory left behind is harmless.
func (d *Backend) removeEmptyRepoParents(rp string) {
	root := filepath.Join(d.cfg.DataPath, "repos")
	for dir := filepath.Dir(rp); ; dir = filepath.Dir(dir) {
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return
		}
		fi, err := os.Lstat(dir)
		if err != nil || !fi.IsDir() {
			return
		}
		// Remove fails on a directory that is not empty, which is what
		// keeps the other repositories of the group, and anything else in
		// the directory, in place.
		if err := os.Remove(dir); err != nil {
			return
		}
	}
}

// renameOntoClaim moves the repository at op onto np, the empty directory
// claimRepoPath made for it. *claimed is cleared once that directory is no
// longer the caller's to remove.
func renameOntoClaim(op, np string, claimed *bool) error {
	if runtime.GOOS == "windows" {
		// Windows does not rename onto an existing directory, so the
		// claim is given up first. A create that claims the path in
		// between makes the rename fail rather than be replaced, and
		// withRepoParent does not retry that. With the claim gone, np's
		// group directories may be empty, so a concurrent delete or
		// rename can remove them before the rename; withRepoParent
		// recreates them and retries.
		if err := os.Remove(np); err != nil {
			return err
		}
		*claimed = false
		return withRepoParent(np, func() error { return os.Rename(op, np) })
	}
	// os.Rename refuses an existing directory as the target, but rename(2)
	// replaces an empty one atomically, which is what the claim is.
	if err := syscall.Rename(op, np); err != nil {
		return &os.LinkError{Op: "rename", Old: op, New: np, Err: err}
	}
	*claimed = false
	return nil
}

// registerRepository records a bare repository that already exists at the
// name's path and finishes setting it up. The directory is removed on failure.
// When limited, max_repos_per_user is checked in the same transaction that
// records the repository.
func (d *Backend) registerRepository(ctx context.Context, name string, user proto.User, opts proto.RepositoryOptions, limited bool) (proto.Repository, error) {
	rp := d.repoPath(name)
	opts.ProjectName = utils.Sanitize(opts.ProjectName)
	opts.Description = utils.Sanitize(opts.Description)

	var userID int64
	if user != nil {
		userID = user.ID()
	}
	anonymous := userID == 0

	var m models.Repo
	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		if anonymous {
			// Anonymous callers (e.g. an anon-access/allow-keyless
			// override) have no user of their own, but repos.user_id is
			// NOT NULL. Fall back to the default admin as owner, the same
			// "ownerless repos belong to the first admin" idiom already
			// used when migrating repos from the pre-user schema (see
			// 0001_create_tables.go).
			adminID, err := d.defaultAdminUserID(ctx, tx)
			if err != nil {
				return err
			}
			userID = adminID
		}

		var err error
		m, err = d.store.CreateRepo(ctx, tx, models.Repo{
			Name:        name,
			UserID:      userID,
			ProjectName: opts.ProjectName,
			Description: opts.Description,
			Private:     opts.Private,
			Hidden:      opts.Hidden,
			Mirror:      opts.Mirror,
			Anonymous:   anonymous,
		})
		if err != nil {
			return err
		}

		// The limit is checked after the insert, in the same transaction,
		// which rolls the insert back if the creator is over it. As with the
		// webhook cap (see CreateWebhook), on SQLite the transaction holds
		// the write lock through the count: from BEGIN IMMEDIATE (see
		// db.Open), or, when the data source asks for deferred
		// transactions, from the insert, which is why it comes first. On
		// Postgres the owner's row is locked before counting, so creates
		// racing at the limit take turns.
		if limited {
			if err := d.store.LockUserRepos(ctx, tx, userID); err != nil {
				return err
			}
			n, err := d.countRepos(ctx, tx, user)
			if err != nil {
				return err
			}
			if n > int64(d.cfg.MaxReposPerUser) {
				return d.errRepoLimit(user)
			}
		}

		return hooks.GenerateHooks(ctx, d.cfg, name)
	}); err != nil {
		// The callers log the failure: CreateRepository through
		// clientError, an import where it registers.
		d.removeRepoDir(rp) //nolint:errcheck
		if errors.Is(err, db.ErrDuplicateKey) {
			return nil, proto.ErrRepoExist
		}

		return nil, err
	}

	if d.afterRegister != nil {
		d.afterRegister()
	}

	return &repo{
		name:   name,
		path:   rp,
		repo:   m,
		logger: d.logger,
	}, nil
}

// DefaultAdmin returns the lowest-ID admin user, the account that owns
// repositories the server creates on its own behalf, such as default_repo.
// It returns proto.ErrUserNotFound if there is no admin user.
func (d *Backend) DefaultAdmin(ctx context.Context) (proto.User, error) {
	var adminID int64
	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		adminID, err = d.defaultAdminUserID(ctx, tx)
		return err
	}); err != nil {
		if errors.Is(err, errNoAdminUser) {
			return nil, proto.ErrUserNotFound
		}
		return nil, err
	}

	return d.UserByID(ctx, adminID)
}

// errNoAdminUser is returned by defaultAdminUserID when no user is an admin.
var errNoAdminUser = errors.New("no admin user found")

// defaultAdminUserID returns the ID of the lowest-ID admin user, used to
// own repositories created by callers with no user of their own (anonymous
// access via an anon-access/allow-keyless override). The migration that
// seeds the initial database always creates an admin user, so this should
// never fail to find one in practice.
func (d *Backend) defaultAdminUserID(ctx context.Context, tx *db.Tx) (int64, error) {
	adminID, err := d.store.FindDefaultAdminUserID(ctx, tx)
	if errors.Is(err, db.ErrRecordNotFound) {
		return 0, fmt.Errorf("%w to own anonymous repository", errNoAdminUser)
	}
	if err != nil {
		return 0, err
	}

	return adminID, nil
}

// ImportRepository imports a repository from remote. An error that is not a
// refusal (see clientError) is logged and returned as proto.ErrServerFault.
func (d *Backend) ImportRepository(ctx context.Context, name string, user proto.User, remote string, opts proto.RepositoryOptions) (proto.Repository, error) {
	r, err := d.importUserRepository(ctx, name, user, remote, opts)
	if err != nil {
		return nil, d.clientError(ctx, "import", name, err)
	}
	return r, nil
}

// importUserRepository is ImportRepository without the mapping of errors.
func (d *Backend) importUserRepository(ctx context.Context, name string, user proto.User, remote string, opts proto.RepositoryOptions) (proto.Repository, error) {
	name = utils.SanitizeRepo(name)
	if err := utils.ValidateRepo(name); err != nil {
		return nil, err
	}

	if err := d.checkCanClaim(ctx, user, name); err != nil {
		return nil, err
	}

	// Refuse at the limit before cloning anything. registerRepository
	// checks again when it records the repository.
	admin := d.IsServerAdmin(ctx, user)
	limited := d.cfg.MaxReposPerUser > 0 && !admin // as repoLimited
	slot, err := d.reserveImport(ctx, user, admin, limited)
	if err != nil {
		return nil, err
	}
	defer slot.release()

	if reason := d.ImportsDisabledReason(); reason != "" {
		d.logger.Warn("refusing import", "name", name, "reason", reason)
		return nil, proto.ErrImportsDisabled
	}

	remote = utils.Sanitize(remote)
	policy, mirrorRestricted := d.importRemotePolicy(ctx, user, opts.Mirror)
	// checkRemote validates one user-supplied remote, explaining a refusal
	// that only the mirror rule caused.
	checkRemote := func(r string) (ssrf.ValidatedGitRemote, error) {
		v, err := d.validateImportRemote(ctx, r, policy)
		if mirrorRestricted && errors.Is(err, proto.ErrUnauthorized) {
			d.logger.Warn("refusing ssh mirror import: owner is not an admin, so the mirror job could not sync it",
				"name", name, "remote", redactRemote(r), "user", user.Username())
			return v, errMirrorImportOwnerNotAdmin
		}
		return v, err
	}
	v, err := checkRemote(remote)
	if err != nil {
		return nil, err
	}

	imp := importSpec{
		name:      name,
		path:      d.repoPath(name),
		remote:    remote,
		logRemote: redactRemote(remote),
		env:       RemoteGitEnv(d.cfg, v.ForClone()),
		opts:      opts,
		limited:   limited,
	}

	// LFS objects are fetched only when asked for (--lfs or --lfs-endpoint);
	// a failure there then fails the import, since the user asked for it,
	// except for objects the remote doesn't have (see fetchImportLFSObjects).
	// The endpoint is resolved before anything is created, so one objects
	// can't be fetched from, an ssh or scp-style one, is refused with the
	// reason rather than cloned without its LFS objects.
	lfsEndpoint, lfsRemote := "", v
	switch {
	case opts.LFSEndpoint != "":
		// A user-supplied endpoint is persisted to the repo config, where
		// the mirror job later reads it, so it obeys the same validation
		// and ssh policy as the remote itself.
		lfsRemote, err = checkRemote(opts.LFSEndpoint)
		if err != nil {
			return nil, err
		}
		lfsEndpoint = opts.LFSEndpoint
	case opts.LFS:
		lfsEndpoint = remote
	}
	if lfsEndpoint != "" {
		// pkg/ssrf has told ssh and scp-style remotes from URLs on git's
		// terms; anything else it let through is an http, https or git://
		// URL.
		if lfsRemote.Transport == ssrf.GitTransportSSH {
			return nil, errLFSEndpointUnsupported
		}
		ep, err := lfs.NewEndpoint(lfsEndpoint)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", proto.ErrInvalidRemote, err)
		}
		imp.lfsEndpoint, imp.lfs = lfsEndpoint, lfs.NewClient(ep)
	}

	// Claiming the directory is what serializes concurrent imports of one
	// name: the second one fails here with ErrRepoExist.
	if err := claimRepoPath(imp.path); err != nil {
		return nil, err
	}

	d.logger.Info("importing repository", "name", name, "remote", imp.logRemote, "path", imp.path)

	// The import keeps the caller's values, but stops when the caller goes
	// away, when the server shuts down (StopImports), or after
	// importTimeout, whichever comes first. A stopped import removes what it
	// had created.
	importCtx, cancel := context.WithTimeout(proto.WithUserContext(context.WithoutCancel(ctx), user), d.importTimeout)
	defer cancel()
	defer context.AfterFunc(ctx, cancel)()
	imported := make(chan struct{})
	defer close(imported)
	go func() {
		select {
		case <-d.imports.stop:
			cancel()
		case <-imported:
		}
	}()
	r, err := d.importRepository(importCtx, user, imp)
	if err != nil {
		// A concurrent create can take the last slot while the clone runs.
		if errors.Is(err, proto.ErrRepoLimitReached) {
			return nil, err
		}
		// The real error is logged where it happened. The caller gets a
		// fixed message so a failed import cannot be used to probe hosts
		// and ports.
		return nil, proto.ErrImportFailed
	}

	return r, nil
}

// importSpec is a validated import, ready to run.
type importSpec struct {
	name, path        string
	remote, logRemote string
	// env is the git environment the clone must run with; see RemoteGitEnv.
	env []string
	// lfsEndpoint and lfs, its client, are set only when LFS objects are to
	// be fetched.
	lfsEndpoint string
	lfs         LFSClient
	opts        proto.RepositoryOptions
	// limited says whether max_repos_per_user applies (see repoLimited).
	limited bool
}

// importRepository clones the remote into the claimed directory, registers
// the repository, and fetches LFS objects if asked. On failure nothing is
// left behind.
func (d *Backend) importRepository(ctx context.Context, user proto.User, imp importSpec) (_ proto.Repository, err error) {
	if err := cloneImport(ctx, imp); err != nil {
		d.logger.Error("failed to clone repository", "err", err, "mirror", imp.opts.Mirror, "remote", imp.logRemote, "path", imp.path)
		if rerr := d.removeRepoDir(imp.path); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return nil, err
	}

	// An import stopped while the clone finished is not registered.
	if err := ctx.Err(); err != nil {
		d.logger.Error("import stopped after clone", "err", err, "name", imp.name)
		if rerr := os.RemoveAll(imp.path); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return nil, err
	}

	// The LFS endpoint goes into the clone's config while the directory is
	// still the import's own: once the repository is registered, its owner
	// can delete it and create another under the name, whose config this
	// must not touch.
	if imp.lfs != nil {
		if err := setImportLFSURL(imp.path, imp.lfsEndpoint); err != nil {
			d.logger.Error("failed to set repository config", "err", err, "path", imp.path)
			if rerr := d.removeRepoDir(imp.path); rerr != nil {
				err = errors.Join(err, rerr)
			}
			return nil, err
		}
	}

	// Registering is a short database and filesystem step, run to the end
	// even if ctx is cancelled meanwhile: stopping it once the row is
	// committed would leave a registered repository behind with no cleanup
	// to remove it. A stopped import still ends at the LFS fetch below,
	// whose failure removes the repository.
	r, err := d.registerRepository(context.WithoutCancel(ctx), imp.name, user, imp.opts, imp.limited)
	if err != nil {
		d.logger.Error("failed to create repository", "err", err, "name", imp.name)
		return nil, err
	}

	defer func() {
		if err != nil {
			d.removeFailedImport(ctx, r)
			return
		}
		// git clone runs no hooks, so nothing else records when the
		// imported history was last changed.
		if lerr := d.populateLastModified(context.WithoutCancel(ctx), imp.name); lerr != nil {
			d.logger.Warn("failed to record the last change of an imported repository", "repo", imp.name, "err", lerr)
		}
	}()

	if imp.lfs == nil {
		return r, nil
	}

	if err := d.fetchImportLFSObjects(ctx, r, imp.lfs); err != nil {
		d.logger.Error("failed to store missing lfs objects", "err", err, "path", imp.path)
		return nil, err
	}

	return r, nil
}

// removeFailedImport deletes r, the repository of an import that failed
// after registering it. The import is a visible repository by then, so its
// owner may have deleted it and created another under the name meanwhile;
// only r itself is deleted, by its ID, never what now has its name. ctx may
// be what stopped the import, so the cleanup does not depend on it.
func (d *Backend) removeFailedImport(ctx context.Context, r proto.Repository) {
	err := d.DeleteResolvedRepository(context.WithoutCancel(ctx), r)
	switch {
	case err == nil:
		return
	case errors.Is(err, proto.ErrRepoNotFound):
		d.logger.Info("failed import's repository is gone, leaving its name alone", "name", r.Name())
	default:
		d.logger.Error("failed to delete repository", "err", err, "name", r.Name())
		return
	}
	// The repository was deleted under the import, which may have stored LFS
	// objects for it since that delete removed its LFS storage. IDs are never
	// reused, so nothing else can use the directory.
	if _, err := d.store.GetRepoByID(context.WithoutCancel(ctx), d.db, r.ID()); !errors.Is(err, db.ErrRecordNotFound) {
		return
	}
	lfsPath := filepath.Join(d.cfg.DataPath, "lfs", strconv.FormatInt(r.ID(), 10))
	if err := os.RemoveAll(lfsPath); err != nil {
		d.logger.Error("failed to delete lfs objects of a failed import", "repo", r.Name(), "path", lfsPath, "err", err)
	}
}

// setImportLFSURL sets lfs.url, where the mirror job later reads the LFS
// endpoint, in the git config of the clone at dir.
func setImportLFSURL(dir, endpoint string) error {
	r, err := git.Open(dir)
	if err != nil {
		return err
	}
	return r.ConfigSet("lfs.url", endpoint)
}

// fetchImportLFSObjects fetches the LFS objects of an imported repository.
// Like git lfs fetch, it only warns about objects the remote can't provide,
// so one missing object doesn't throw the whole import away. For a mirror,
// the mirror job tries them again on every sync; a plain import never
// fetches them again, so they stay missing until someone pushes them. Any
// other failure fails the import, including a transfer that fails for a
// reason a retry may get past, such as a dropped connection or a 5xx, so
// such objects are not dropped for good.
func (d *Backend) fetchImportLFSObjects(ctx context.Context, r proto.Repository, client LFSClient) error {
	err := d.StoreRepoMissingLFSObjects(ctx, r, client)
	var unavailable *lfsObjectsUnavailableError
	if errors.As(err, &unavailable) {
		if r.IsMirror() {
			d.logger.Warn("imported mirror without some lfs objects, mirror syncs will retry them",
				"repo", r.Name(), "missing", unavailable.count)
		} else {
			d.logger.Warn("imported repository without some lfs objects, they will not be fetched again",
				"repo", r.Name(), "missing", unavailable.count)
		}
		return nil
	}
	return err
}

// DeleteRepository deletes a repository.
func (d *Backend) DeleteRepository(ctx context.Context, name string) error {
	return d.deleteRepository(ctx, name, 0, nil)
}

// DeleteResolvedRepository deletes r, a repository the caller looked up and
// authorized before: the repository under r's name, provided it is still r.
// If another repository has taken the name since, as a rename and a create
// in between can arrange, it returns proto.ErrRepoNotFound and deletes
// nothing.
func (d *Backend) DeleteResolvedRepository(ctx context.Context, r proto.Repository) error {
	return d.deleteRepository(ctx, r.Name(), r.ID(), nil)
}

// errRepoKept is returned by deleteRepository when its keep check kept the
// repository.
var errRepoKept = errors.New("repository kept")

// deleteRepository is DeleteRepository with optional checks. With a
// non-zero id, only the repository with that ID is deleted. Otherwise the
// repository the delete finds under the name when it starts is the one
// deleted, and its event the one sent: a repository that takes the name
// before its row is locked is not deleted. Either way the name is then
// proto.ErrRepoNotFound.
//
// A non-nil keep runs in the transaction that deletes the repository's row, with that
// row, read and locked (see GetRepoByNameForUpdate) before the check; if it
// returns true the repository is kept and errRepoKept is returned. The lock
// holds off collaborator, webhook and LFS rows for the repository and
// changes to the row itself until the transaction ends, so none of those
// can land between the check and the delete. Anything not in the database,
// such as refs, is the caller's to order.
//
// A name with no row is proto.ErrRepoNotFound, whatever is on disk: a
// directory without a row is left alone, and reported when the server
// starts (see OrphanRepositoryDirs).
func (d *Backend) deleteRepository(ctx context.Context, name string, id int64, keep func(tx *db.Tx, m models.Repo) (bool, error)) error {
	name = utils.SanitizeRepo(name)

	ev, r, err := d.deleteEvent(ctx, name)
	if err != nil {
		return err
	}
	if r != nil {
		// A repository without a directory has no event, and is deleted
		// by its row alone.
		switch {
		case id == 0:
			id = r.ID()
		case id != r.ID():
			return proto.ErrRepoNotFound
		}
	}
	repoID, err := d.deleteRepoRow(ctx, name, id, keep)
	if err != nil {
		return wrapRepoError(err)
	}
	d.removeRepoFiles(name, repoID)

	// The repository is gone, so its event is delivered without recording
	// deliveries: they would refer to webhooks that no longer exist. The
	// receiver's outcome is only logged, not returned, so it cannot undo the
	// delete.
	if ev != nil {
		d.deliverAfterChange(ctx, ev.hooks, ev.event, false)
	}
	return nil
}

// repoDeleteEvent is the webhook event of a repository's deletion and the
// webhooks it goes to.
type repoDeleteEvent struct {
	event webhook.RepositoryEvent
	hooks []models.Webhook
}

// deleteEvent looks up the repository name, and builds the event of
// deleting it and reads its subscribers before anything is deleted: the
// webhooks go with the row. A repository whose directory is gone is not
// found, and has no event, but its row still has to go, so both results
// are nil. An event that cannot be built (its owner lookup failed) is
// logged and skipped, and subscribers that cannot be read are logged,
// rather than stopping the delete. The error is set only when the
// repository could not be looked up.
func (d *Backend) deleteEvent(ctx context.Context, name string) (*repoDeleteEvent, proto.Repository, error) {
	r, err := d.Repository(ctx, name)
	switch {
	case errors.Is(err, proto.ErrRepoNotFound):
		return nil, nil, nil
	case err != nil:
		return nil, nil, err
	}
	ev, err := webhook.NewRepositoryEvent(ctx, proto.UserFromContext(ctx), r, webhook.RepositoryEventActionDelete)
	if err != nil {
		d.logger.Error("deleting repository without webhook event", "repo", name, "err", err)
		return nil, r, nil
	}
	hooks, err := webhook.Subscribers(ctx, ev)
	if err != nil {
		d.logEventNotDelivered(ev, err)
		return nil, r, nil
	}
	return &repoDeleteEvent{event: ev, hooks: hooks}, r, nil
}

// deleteRepoRow deletes the row of the repository name, if its ID is id
// (any, when id is 0) and keep (see deleteRepository) does not keep it, and
// returns the deleted row's ID.
func (d *Backend) deleteRepoRow(ctx context.Context, name string, id int64, keep func(tx *db.Tx, m models.Repo) (bool, error)) (int64, error) {
	var repoID int64
	err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		// The row is locked before anything is deleted. A rename that
		// commits first then leaves no row with the name, rather than the
		// webhooks of the renamed repository deleted and its LFS directory
		// removed. With a keep check, nothing can be added for the
		// repository between the check and the delete, only to go with the
		// row by ON DELETE CASCADE.
		m, err := d.store.GetRepoByNameForUpdate(ctx, tx, name)
		if err != nil {
			return err
		}
		if id != 0 && m.ID != id {
			return proto.ErrRepoNotFound
		}
		if keep != nil {
			kept, err := keep(tx, m)
			if err != nil {
				return err
			}
			if kept {
				return errRepoKept
			}
		}

		// The repository's webhooks, with their events and deliveries, its
		// collaborators and its LFS objects and locks go with the row by
		// ON DELETE CASCADE: db.Open enforces foreign keys.
		if err := d.store.DeleteRepoByName(ctx, tx, name); err != nil {
			return err
		}
		repoID = m.ID
		return nil
	})
	return repoID, err
}

// removeRepoFiles removes the directory and the LFS storage of the
// repository name, with ID repoID, whose row is gone. The delete has
// happened once the row is gone, so a failure here is logged rather than
// returned.
//
// The files are removed only now that the row is gone. Removing the
// directory inside the transaction could leave a repository with a row but a
// partly removed directory when the removal failed, or a row with no
// directory when the commit failed, and a failed delete leaves the LFS
// objects in place. A leftover directory has no row: it cannot be created,
// imported or renamed over, and it is reported when the server starts.
//
// The LFS storage goes with the temporary files of unfinished uploads and
// objects without a row. Repository IDs are never reused, so no other
// repository can have written there. An LFS upload, import or mirror sync
// still running for this repository can recreate the directory after it is
// removed, leaving an orphan that no other repository can reach.
func (d *Backend) removeRepoFiles(name string, repoID int64) {
	rp := d.repoPath(name)
	if err := d.removeRepoDir(rp); err != nil {
		d.logger.Error("failed to delete repository directory", "repo", name, "path", rp, "err", err)
	}
	lfsPath := filepath.Join(d.cfg.DataPath, "lfs", strconv.FormatInt(repoID, 10))
	if err := os.RemoveAll(lfsPath); err != nil {
		d.logger.Error("failed to delete lfs objects", "repo", name, "path", lfsPath, "err", err)
	}
}

// RenameRepository renames a repository. An error that is not a refusal
// (see clientError) is logged and returned as proto.ErrServerFault.
func (d *Backend) RenameRepository(ctx context.Context, oldName string, newName string) error {
	return d.clientError(ctx, "rename", oldName, d.renameRepository(ctx, oldName, newName))
}

// renameRepository is RenameRepository without the mapping of errors.
func (d *Backend) renameRepository(ctx context.Context, oldName string, newName string) error {
	oldName = utils.SanitizeRepo(oldName)
	// The repository may predate the rules for new names.
	if err := utils.ValidateExistingRepo(oldName); err != nil {
		return err
	}

	newName = utils.SanitizeRepo(newName)
	if err := utils.ValidateRepo(newName); err != nil {
		return err
	}

	if oldName == newName {
		return nil
	}

	// Renaming onto a new name creates it, so it needs the same permission.
	// The caller is the user and public key on the context.
	if err := d.checkCanClaim(ctx, proto.UserFromContext(ctx), newName); err != nil {
		return err
	}

	op := d.repoPath(oldName)
	np := d.repoPath(newName)
	if !d.repoPathExists(oldName) {
		return proto.ErrRepoNotFound
	}

	// Claim the new path the way a create or import does, so that whichever
	// comes second fails instead of this rename replacing the other's empty
	// directory (rename(2) replaces an empty one) and that one's cleanup then
	// deleting the moved repository.
	if err := claimRepoPath(np); err != nil {
		if errors.Is(err, proto.ErrRepoExist) {
			return err
		}
		d.logger.Error("failed to claim repository path", "repo", newName, "err", err)
		d.removeEmptyRepoParents(np)
		return errRepoUpdate
	}
	claimed := true
	// renamed is the row of the repository renamed, read and locked in the
	// transaction.
	var renamed models.Repo
	// moved is set once the directory is at np.
	moved := false

	// The directory is moved inside the transaction, and moved back if the
	// transaction fails, as when the commit does. Cancelling ctx after the
	// move, as a client disconnecting does, would roll the new name back
	// under the moved directory, so the rename and its event run to the end
	// once started.
	ctx = context.WithoutCancel(ctx)

	err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		// The directory checks above find a repository whose name differs
		// only in case on a case-insensitive filesystem, so the row decides:
		// with no row named oldName this fails before the directory moves.
		var err error
		renamed, err = d.store.GetRepoByNameForUpdate(ctx, tx, oldName)
		if err != nil {
			return err
		}
		if err := d.store.SetRepoNameByName(ctx, tx, oldName, newName); err != nil {
			return err
		}

		if err := renameOntoClaim(op, np, &claimed); err != nil {
			d.logger.Error("failed to move repository", "repo", oldName, "err", err)
			return errRepoUpdate
		}
		moved = true

		return nil
	})
	if d.renameTxDone != nil {
		err = d.renameTxDone(err)
	}
	if err != nil && moved {
		// A COMMIT whose answer was lost, as when the connection drops
		// during it, can have committed. The row decides where the
		// directory belongs.
		switch name, cerr := d.renamedRowName(ctx, renamed.ID); {
		case cerr != nil:
			d.logger.Error("rename failed and its outcome is unknown; leaving the repository directory where it is, it is reported at start-up if it has no row",
				"repo", oldName, "path", np, "err", err, "lookup_err", cerr)
			return wrapRepoError(err)
		case name == newName:
			d.logger.Warn("rename committed although its transaction reported an error", "repo", oldName, "new", newName, "err", err)
			err = nil
		case name == oldName:
			d.moveRepoDirBack(oldName, op, np)
		default:
			// Renamed again by another session since: the directory
			// belongs with neither name this call knows of.
			d.logger.Error("rename failed and the repository now has another name; leaving its directory where it is, it is reported at start-up",
				"repo", oldName, "row_name", name, "path", np, "err", err)
			return wrapRepoError(err)
		}
	}
	if err != nil {
		if claimed {
			os.Remove(np) //nolint:errcheck
		}
		// claimRepoPath may have created the new name's group
		// directories; do not leave them behind empty. Only empty
		// directories are removed, and a concurrent create or rename into
		// the group recreates them through withRepoParent.
		d.removeEmptyRepoParents(np)
		// A row with the new name and no directory, which claiming the
		// path cannot see, takes the name as much as a repository does.
		if errors.Is(err, db.ErrDuplicateKey) {
			return proto.ErrRepoExist
		}
		return wrapRepoError(err)
	}

	// The old name's group directories may now be empty.
	d.removeEmptyRepoParents(op)

	// The rename is committed, so an event problem is logged, not returned.
	// The event is for the row this call renamed: by the time the new name
	// is looked up again, another repository can have it.
	ctx, cancel := AfterChangeContext(ctx)
	defer cancel()
	user := proto.UserFromContext(ctx)
	renamed.Name = newName
	repo := d.repoFromRow(newName, renamed)

	wh, err := webhook.NewRepositoryRenameEvent(ctx, user, repo, oldName)
	if err != nil {
		d.logger.Error("rename committed but webhook event skipped", "repo", newName, "err", err)
		return nil
	}

	d.sendAfterChange(ctx, wh)
	return nil
}

// renamedRowCheckTimeout bounds how long renamedRowName takes: on Postgres
// the wait for the row's lock and the locked read, on SQLite the plain read.
const renamedRowCheckTimeout = time.Minute

// renamedRowName returns the name in the row of the repository id, after a
// rename whose transaction reported an error, to tell whether it committed
// after all. The row is read locked, in a transaction of its own: on
// Postgres a COMMIT still in flight on the lost connection, waiting on
// synchronous replication say, holds the row's lock and is invisible to a
// plain read until it ends; the locked read waits for it, so it sees the
// row as that COMMIT leaves it.
//
// On SQLite the row is read plainly. A SQLite rename fails at its COMMIT or
// not at all, and has nothing in flight to wait for once it returned; a
// write transaction would only take the database's write lock, and another
// writer holding it past busy_timeout would turn a move back into leaving
// the directory.
func (d *Backend) renamedRowName(ctx context.Context, id int64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, renamedRowCheckTimeout)
	defer cancel()
	if d.db.DriverName() != "postgres" {
		m, err := d.store.GetRepoByID(ctx, d.db, id)
		return m.Name, err
	}
	var name string
	err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		m, err := d.store.GetRepoByIDForUpdate(ctx, tx, id)
		name = m.Name
		return err
	})
	return name, err
}

// moveBackAttempts bounds how often moveRepoDirBack tries to move a
// directory back on Windows, where a rename fails while any file in the
// directory is open, as a git process still running can keep one.
const moveBackAttempts = 5

// moveRepoDirBack moves the directory of the repository oldName from np,
// where a rename whose transaction then failed moved it, back to op, where
// its row says it is. If that fails too, the directory stays at np without
// a row: it is not served, it is reported when the server starts, and
// nothing removes it.
func (d *Backend) moveRepoDirBack(oldName, op, np string) {
	move := func() error { return withRepoParent(op, func() error { return os.Rename(np, op) }) }
	err := move()
	for wait, i := 50*time.Millisecond, 1; err != nil && runtime.GOOS == "windows" && i < moveBackAttempts; wait, i = wait*2, i+1 {
		time.Sleep(wait)
		err = move()
	}
	if err != nil {
		d.logger.Error("rename failed and the repository directory could not be moved back; move it back by hand",
			"repo", oldName, "path", np, "want", op, "err", err)
	}
}

// Repositories returns every repository.
func (d *Backend) Repositories(ctx context.Context) ([]proto.Repository, error) {
	ms, err := d.store.GetAllRepos(ctx, d.db)
	if err != nil {
		return nil, err
	}
	repos := make([]proto.Repository, 0, len(ms))
	for _, m := range ms {
		repos = append(repos, &repo{
			name:   m.Name,
			path:   d.repoPath(m.Name),
			repo:   m,
			logger: d.logger,
		})
	}
	return repos, nil
}

// Repository returns a repository by name.
func (d *Backend) Repository(ctx context.Context, name string) (proto.Repository, error) {
	name = utils.SanitizeRepo(name)
	rp := d.repoPath(name)
	if !d.repoDirFound(rp) {
		return nil, proto.ErrRepoNotFound
	}

	m, err := d.store.GetRepoByName(ctx, d.db, name)
	if err != nil {
		return nil, wrapRepoError(err)
	}

	r := &repo{
		name:   name,
		path:   rp,
		repo:   m,
		logger: d.logger,
	}

	return r, nil
}

// refusals are the errors that refuse a create, import or rename for a
// reason the client may be told, or that already stand for a fault with a
// fixed message (proto.ErrLookupFailed, errRepoUpdate, the
// git.ErrNotAGitRepository of openRepoError), or that say the client went
// away.
var refusals = []error{
	utils.ErrInvalidRepo,
	proto.ErrUnauthorized,
	proto.ErrRepoExist,
	proto.ErrRepoNotFound,
	proto.ErrRepoLimitReached,
	proto.ErrAnonymousPrivate,
	proto.ErrInvalidRemote,
	proto.ErrImportFailed,
	proto.ErrImportsDisabled,
	proto.ErrTooManyImports,
	proto.ErrLookupFailed,
	errLFSEndpointUnsupported,
	errRepoUpdate,
	git.ErrNotAGitRepository,
	context.Canceled,
	context.DeadlineExceeded,
}

// clientError returns err, from a create, import or rename (op) of the
// repository name, for the client: a refusal (see refusals) unchanged, and
// anything else, such as a failed insert, logged with its cause and
// replaced by proto.ErrServerFault, whose text says nothing about the
// server. A failure that came with ctx ending, such as a statement the
// client's disconnect cancelled, is logged at Debug: it is not a fault of
// the server.
func (d *Backend) clientError(ctx context.Context, op, name string, err error) error {
	if err == nil {
		return nil
	}
	for _, r := range refusals {
		if errors.Is(err, r) {
			return err
		}
	}
	msg := "failed to " + op + " repository"
	if ctx.Err() != nil {
		d.logger.Debug(msg, "repo", name, "err", err)
	} else {
		d.logger.Error(msg, "repo", name, "err", err)
	}
	return proto.ErrServerFault
}

// repoDirFound reports whether the repository directory rp can be found. A
// path the filesystem refuses as a name, which comes straight from a
// request, is logged at Debug; any other failure but a missing directory
// at Error.
func (d *Backend) repoDirFound(rp string) bool {
	_, err := os.Stat(rp)
	switch {
	case err == nil:
		return true
	case errors.Is(err, fs.ErrNotExist):
	case isInvalidName(err):
		d.logger.Debug("invalid repository name", "err", err)
	default:
		d.logger.Errorf("failed to stat repository path: %v", err)
	}
	return false
}

// errRepoUpdate replaces filesystem errors, which carry the server data
// path, when a repository setting cannot be written.
var errRepoUpdate = errors.New("failed to update repository")

// wrapRepoError maps a missing database row to proto.ErrRepoNotFound so
// callers never see the raw driver error for a repository that does not
// exist.
func wrapRepoError(err error) error {
	if errors.Is(err, db.ErrRecordNotFound) {
		return proto.ErrRepoNotFound
	}
	return err
}

// SetHidden sets the hidden flag of a repository.
func (d *Backend) SetHidden(ctx context.Context, name string, hidden bool) error {
	return d.setHidden(ctx, name, 0, hidden)
}

// SetResolvedHidden is SetHidden for r, a repository the caller looked up
// and authorized before: see lockResolved.
func (d *Backend) SetResolvedHidden(ctx context.Context, r proto.Repository, hidden bool) error {
	return d.setHidden(ctx, r.Name(), r.ID(), hidden)
}

func (d *Backend) setHidden(ctx context.Context, name string, id int64, hidden bool) error {
	name = utils.SanitizeRepo(name)

	return wrapRepoError(d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		if _, err := d.lockResolved(ctx, tx, name, id); err != nil {
			return err
		}
		return d.store.SetRepoIsHiddenByName(ctx, tx, name, hidden)
	}))
}

// lockResolved checks, in tx, that the repository name is still the one
// with ID id, a caller resolved and authorized before acting on it, and
// locks its row until tx ends, so the name keeps meaning that repository
// for the rest of tx. Another repository renamed or created under the name
// since is proto.ErrRepoNotFound: the caller did not authorize that one.
// With id 0 it does nothing, for callers that act on the name.
func (d *Backend) lockResolved(ctx context.Context, tx *db.Tx, name string, id int64) (models.Repo, error) {
	if id == 0 {
		return models.Repo{}, nil
	}
	m, err := d.store.GetRepoByNameForUpdate(ctx, tx, name)
	if err != nil {
		return m, err
	}
	if m.ID != id {
		return m, proto.ErrRepoNotFound
	}
	return m, nil
}

// mirrorCheckTimeout bounds the hostname lookups SetMirror makes when it
// validates a repository's remotes before switching mirroring on.
const mirrorCheckTimeout = 10 * time.Second

// errRepoChangedDuringMirror is returned when the repository was replaced,
// re-owned or re-pointed while SetMirror was validating its remotes.
var errRepoChangedDuringMirror = errors.New("repository changed while switching mirroring; try again")

// SetMirror sets the mirror flag of a repository.
// Note: enabling mirror mode requires the repository to have been imported
// with a remote URL. Use ImportRepository to create a new mirror.
func (d *Backend) SetMirror(ctx context.Context, name string, mirror bool) error {
	name = utils.SanitizeRepo(name)
	rp := d.repoPath(name)

	// The remotes are validated before the transaction opens. Validation
	// may resolve hostnames, and on SQLite a read inside a transaction holds
	// the database lock until it ends, so a slow resolver would otherwise
	// stall every writer on the server.
	m, err := d.store.GetRepoByName(ctx, d.db, name)
	if err != nil {
		return wrapRepoError(err)
	}
	_, rcfg, err := d.openRepoConfig(rp)
	if err != nil {
		return err
	}
	urls := rcfg.urls()
	lfsURL := rcfg.lfsURL
	if mirror && len(urls) == 0 {
		return errors.New("cannot enable mirror mode: repository has no remote URL configured")
	}
	if mirror && len(rcfg.remotes) > 1 {
		// The mirror job fetches every ref of the remote with --prune, so
		// a second remote would delete and overwrite the first one's refs.
		return errors.New("cannot enable mirror mode: repository has more than one remote")
	}
	if mirror {
		cctx, cancel := context.WithTimeout(ctx, mirrorCheckTimeout)
		err := d.checkMirrorRemotes(cctx, m, rcfg)
		cancel()
		if err != nil {
			return wrapRepoError(err)
		}
	}

	// The git config is written inside the transaction and is not restored
	// if the transaction fails, so cancelling ctx, as a client disconnecting
	// does, must not roll the flag back once the config is written.
	ctx = context.WithoutCancel(ctx)

	return wrapRepoError(d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		// Re-read both, so what is written is what was validated. The row
		// is locked, so the check holds until the commit: on Postgres a
		// rename and a create could otherwise put another repository under
		// the name before the flag is written.
		cur, err := d.store.GetRepoByNameForUpdate(ctx, tx, name)
		if err != nil {
			return err
		}
		r, rcfg, err := d.openRepoConfig(rp)
		if err != nil {
			return err
		}
		if cur.ID != m.ID || cur.UserID != m.UserID ||
			!slices.Equal(rcfg.urls(), urls) || rcfg.lfsURL != lfsURL {
			return errRepoChangedDuringMirror
		}

		// The flag is written first and the git config last, one `git
		// config` per remote, so a failure writing the config rolls the
		// flag back. A failure partway may leave some remotes changed;
		// setting or removing the option is idempotent, so a retry
		// brings the remotes and the flag back in step.
		if err := d.store.SetRepoIsMirrorByName(ctx, tx, name, mirror); err != nil {
			return err
		}
		if err := setRemotesMirror(r, rcfg, mirror); err != nil {
			d.logger.Error("failed to set repository config", "err", err, "path", rp)
			return errRepoUpdate
		}
		return nil
	}))
}

// repoConfig is what SetMirror reads from a repository's git config.
type repoConfig struct {
	// remotes are the remotes that have a URL, in config order.
	remotes []configRemote
	// lfsURL is lfs.url, "" when it is not set.
	lfsURL string
}

// configRemote is a remote of a repository's git config and its URL.
type configRemote struct {
	name, url string
}

// urls returns the URL of every remote in c, in config order.
func (c repoConfig) urls() []string {
	urls := make([]string, len(c.remotes))
	for i, rm := range c.remotes {
		urls[i] = rm.url
	}
	return urls
}

// openRepoConfig opens the repository at rp and reads its remotes and LFS
// endpoint from its git config file. The raw errors carry the server data
// path, so they are logged and a generic one is returned.
func (d *Backend) openRepoConfig(rp string) (*git.Repository, repoConfig, error) {
	r, err := git.Open(rp)
	if err != nil {
		d.logger.Error("failed to open repository", "err", err, "path", rp)
		return nil, repoConfig{}, errRepoUpdate
	}
	rcfg, err := readRepoConfig(r)
	if err != nil {
		d.logger.Error("failed to read repository config", "err", err, "path", rp)
		return nil, repoConfig{}, errRepoUpdate
	}
	return r, rcfg, nil
}

// readRepoConfig reads the remotes and the LFS endpoint from r's config
// file. A remote with several url values is listed with its last one, and a
// remote whose URL is empty is left out. (The mirror job validates every
// url of every remote; see pkg/jobs.)
func readRepoConfig(r *git.Repository) (repoConfig, error) {
	var rcfg repoConfig
	lfsURL, _, err := r.ConfigGet("lfs.url")
	if err != nil {
		return rcfg, err
	}
	rcfg.lfsURL = lfsURL
	entries, err := r.ConfigGetRegexp(`^remote\..*\.url$`)
	if err != nil {
		return rcfg, err
	}
	last := map[string]string{}
	var names []string
	for _, e := range entries {
		name := strings.TrimSuffix(strings.TrimPrefix(e.Key, "remote."), ".url")
		if _, ok := last[name]; !ok {
			names = append(names, name)
		}
		last[name] = e.Value
	}
	for _, name := range names {
		if u := last[name]; u != "" {
			rcfg.remotes = append(rcfg.remotes, configRemote{name: name, url: u})
		}
	}
	return rcfg, nil
}

// setRemotesMirror sets, or with mirror false removes, the mirror option on
// every remote in rcfg, each with one `git config`.
func setRemotesMirror(r *git.Repository, rcfg repoConfig, mirror bool) error {
	for _, rm := range rcfg.remotes {
		key := "remote." + rm.name + ".mirror"
		var err error
		if mirror {
			err = r.ConfigSet(key, "true")
		} else {
			err = r.ConfigUnset(key)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// checkMirrorRemotes refuses to switch on mirroring for repo m, whose git
// config is rcfg, when the mirror job would skip it on every run. The job
// validates every remote with RemotePolicy for the repository's owner (see
// pkg/jobs), so the same check is made here. It matters for a repository
// imported without --mirror by a caller whose admin key let it reach an ssh
// host its non-admin owner account may not.
//
// Like the job, the stored LFS endpoint is checked only when LFS is enabled,
// and a failure there is not a refusal: the job still syncs the git data and
// skips only the LFS objects, so it is logged as a warning.
//
// ctx bounds the hostname lookups validation makes. No transaction may be
// open across this call, for the same reason.
func (d *Backend) checkMirrorRemotes(ctx context.Context, m models.Repo, rcfg repoConfig) error {
	// As in the job, an owner that cannot be looked up is nobody.
	var owner proto.User
	um, err := d.store.GetUserByID(ctx, d.db, m.UserID)
	if err == nil {
		owner = &user{user: um}
	} else if !errors.Is(err, db.ErrRecordNotFound) {
		return err
	}
	policy := RemotePolicy(d.cfg, owner)

	for _, u := range rcfg.urls() {
		_, err := d.validateImportRemote(ctx, u, policy)
		if errors.Is(err, proto.ErrUnauthorized) {
			d.logger.Warn("refusing to enable mirror: owner is not an admin, so the mirror job could not sync it",
				"repo", m.Name, "remote", redactRemote(u))
			return errMirrorOwnerNotAdmin
		}
		if err != nil {
			return err
		}
	}

	if u := rcfg.lfsURL; d.cfg.LFS.Enabled && u != "" {
		if _, err := d.validateImportRemote(ctx, u, policy); err != nil {
			d.logger.Warn("enabling mirror, but the mirror job will skip LFS objects: endpoint failed validation",
				"repo", m.Name, "endpoint", redactRemote(u), "err", err)
		}
	}
	return nil
}

// SetDescription sets the description of a repository.
func (d *Backend) SetDescription(ctx context.Context, name string, desc string) error {
	return d.setDescription(ctx, name, 0, desc)
}

// SetResolvedDescription is SetDescription for r, a repository the caller
// looked up and authorized before: see lockResolved.
func (d *Backend) SetResolvedDescription(ctx context.Context, r proto.Repository, desc string) error {
	return d.setDescription(ctx, r.Name(), r.ID(), desc)
}

func (d *Backend) setDescription(ctx context.Context, name string, id int64, desc string) error {
	name = utils.SanitizeRepo(name)
	desc = utils.Sanitize(desc)

	return wrapRepoError(d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		if _, err := d.lockResolved(ctx, tx, name, id); err != nil {
			return err
		}
		return d.store.SetRepoDescriptionByName(ctx, tx, name, desc)
	}))
}

// SetPrivate sets the private flag of a repository.
func (d *Backend) SetPrivate(ctx context.Context, name string, private bool) error {
	return d.setPrivate(ctx, name, 0, private)
}

// SetResolvedPrivate is SetPrivate for r, a repository the caller looked up
// and authorized before: see lockResolved.
func (d *Backend) SetResolvedPrivate(ctx context.Context, r proto.Repository, private bool) error {
	return d.setPrivate(ctx, r.Name(), r.ID(), private)
}

func (d *Backend) setPrivate(ctx context.Context, name string, id int64, private bool) error {
	name = utils.SanitizeRepo(name)

	var changed bool
	var m models.Repo
	if err := wrapRepoError(
		d.db.TransactionContext(ctx, func(tx *db.Tx) error {
			// Locked, so that changed says what this call did.
			var err error
			m, err = d.store.GetRepoByNameForUpdate(ctx, tx, name)
			if err != nil {
				return err
			}
			if id != 0 && m.ID != id {
				return proto.ErrRepoNotFound
			}
			changed = m.Private != private
			return d.store.SetRepoIsPrivateByName(ctx, tx, name, private)
		}),
	); err != nil {
		return err
	}

	if !changed {
		return nil
	}

	// The change is committed, so its event is sent even if ctx ends
	// meanwhile, and an event problem is logged, not returned. The event is
	// for the row this call changed: by the time the name is looked up
	// again, another repository can have it.
	ctx, cancel := AfterChangeContext(ctx)
	defer cancel()
	m.Private = private
	repo := d.repoFromRow(name, m)

	wh, err := webhook.NewRepositoryEvent(ctx, proto.UserFromContext(ctx), repo, webhook.RepositoryEventActionVisibilityChange)
	if err != nil {
		d.logger.Error("visibility change committed but webhook event skipped", "repo", name, "err", err)
		return nil
	}

	d.sendAfterChange(ctx, wh)
	return nil
}

// SetProjectName sets the project name of a repository.
func (d *Backend) SetProjectName(ctx context.Context, repo string, name string) error {
	return d.setProjectName(ctx, repo, 0, name)
}

// SetResolvedProjectName is SetProjectName for r, a repository the caller
// looked up and authorized before: see lockResolved.
func (d *Backend) SetResolvedProjectName(ctx context.Context, r proto.Repository, name string) error {
	return d.setProjectName(ctx, r.Name(), r.ID(), name)
}

func (d *Backend) setProjectName(ctx context.Context, repo string, id int64, name string) error {
	repo = utils.SanitizeRepo(repo)
	name = utils.Sanitize(name)

	return wrapRepoError(
		d.db.TransactionContext(ctx, func(tx *db.Tx) error {
			if _, err := d.lockResolved(ctx, tx, repo, id); err != nil {
				return err
			}
			return d.store.SetRepoProjectNameByName(ctx, tx, repo, name)
		}),
	)
}

// repoFromRow returns the repository of row m under name.
func (d *Backend) repoFromRow(name string, m models.Repo) *repo {
	return &repo{name: name, path: d.repoPath(name), repo: m, logger: d.logger}
}

// repoPath returns the path to a repository.
func (d *Backend) repoPath(name string) string {
	return hooks.RepoPath(d.cfg, name)
}

// repoPathExists reports whether anything occupies the repository's directory,
// whether or not a database row points at it.
func (d *Backend) repoPathExists(name string) bool {
	_, err := os.Lstat(d.repoPath(name))
	return err == nil
}

var _ proto.Repository = (*repo)(nil)

// repo is a Git repository with metadata stored in the database.
type repo struct {
	name   string
	path   string
	repo   models.Repo
	logger *log.Logger
}

// ID returns the repository's ID.
//
// It implements proto.Repository.
func (r *repo) ID() int64 {
	return r.repo.ID
}

// UserID returns the repository's owner's user ID.
//
// It implements proto.Repository.
func (r *repo) UserID() int64 {
	return r.repo.UserID
}

// Description returns the repository's description.
//
// It implements proto.Repository.
func (r *repo) Description() string {
	return r.repo.Description
}

// IsMirror returns whether the repository is a mirror.
//
// It implements proto.Repository.
func (r *repo) IsMirror() bool {
	return r.repo.Mirror
}

// IsPrivate returns whether the repository is private.
//
// It implements proto.Repository.
func (r *repo) IsPrivate() bool {
	return r.repo.Private
}

// Name returns the repository's name.
//
// It implements proto.Repository.
func (r *repo) Name() string {
	return r.name
}

// Open opens the repository.
//
// It implements proto.Repository.
func (r *repo) Open() (*git.Repository, error) {
	gr, err := git.Open(r.path)
	if err != nil {
		return nil, openRepoError(r.logger, err, r.path)
	}
	return gr, nil
}

// openRepoError returns the error to show to clients when git does not accept
// a repository. git's reason (for example "detected dubious ownership") names
// the server path, so it is logged here and the client gets the plain
// ErrNotAGitRepository. Other errors are returned unchanged.
func openRepoError(logger *log.Logger, err error, path string) error {
	if !errors.Is(err, git.ErrNotAGitRepository) {
		return err
	}
	if logger == nil {
		logger = log.Default()
	}
	logger.Error("git does not accept the repository", "err", err, "path", path)
	return git.ErrNotAGitRepository
}

// ProjectName returns the repository's project name.
//
// It implements proto.Repository.
func (r *repo) ProjectName() string {
	return r.repo.ProjectName
}

// IsHidden returns whether the repository is hidden.
//
// It implements proto.Repository.
func (r *repo) IsHidden() bool {
	return r.repo.Hidden
}

// CreatedAt returns the repository's creation time.
func (r *repo) CreatedAt() time.Time {
	return r.repo.CreatedAt
}

// UpdatedAt returns the repository's last update time: the time of its
// latest commit, as recorded in info/last-modified after each push, import
// and mirror sync (see populateLastModified), or the time its row last
// changed when there is no such file, as for a repository nothing was
// pushed to yet. It runs no git: the TUI asks it for every listed
// repository.
//
// It implements proto.Repository.
func (r *repo) UpdatedAt() time.Time {
	if t, err := readOneline(filepath.Join(r.path, "info", "last-modified")); err == nil {
		if t, err := time.Parse(time.RFC3339, t); err == nil {
			return t
		}
	}
	return r.repo.UpdatedAt
}

// populateLastModified writes the time of the latest commit of the
// repository name to its info/last-modified, where UpdatedAt reads it.
// The post-update hook calls it after a push; an import and a mirror sync
// run no hooks, so they call it themselves.
//
// A repository without refs, such as an empty import or mirror, has no
// commit to date: there is nothing to record, and UpdatedAt keeps falling
// back to the row.
func (d *Backend) populateLastModified(_ context.Context, name string) error {
	rp := d.repoPath(name)
	if !hasRefs(rp) {
		return nil
	}
	r, err := git.Open(rp)
	if err != nil {
		return err
	}
	t, err := r.LatestCommitTime()
	if err != nil {
		return err
	}
	fp := filepath.Join(rp, "info", "last-modified")
	if err := os.MkdirAll(filepath.Dir(fp), os.ModePerm); err != nil {
		return err
	}
	return os.WriteFile(fp, []byte(t.Format(time.RFC3339)), os.ModePerm) //nolint:gosec
}

func readOneline(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}

	defer f.Close() //nolint: errcheck
	s := bufio.NewScanner(f)
	s.Scan()
	return s.Text(), s.Err()
}
