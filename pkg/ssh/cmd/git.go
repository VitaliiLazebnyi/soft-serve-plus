package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/spf13/cobra"
)

var (
	uploadPackCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "upload_pack_total",
		Help:      "The total number of git-upload-pack requests",
	}, []string{"repo"})

	receivePackCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "receive_pack_total",
		Help:      "The total number of git-receive-pack requests",
	}, []string{"repo"})

	uploadArchiveCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "upload_archive_total",
		Help:      "The total number of git-upload-archive requests",
	}, []string{"repo"})

	lfsAuthenticateCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "lfs_authenticate_total",
		Help:      "The total number of git-lfs-authenticate requests",
	}, []string{"repo", "operation"})

	lfsTransferCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "lfs_transfer_total",
		Help:      "The total number of git-lfs-transfer requests",
	}, []string{"repo", "operation"})

	uploadPackSeconds = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "upload_pack_seconds_total",
		Help:      "The total time spent on git-upload-pack requests",
	}, []string{"repo"})

	receivePackSeconds = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "receive_pack_seconds_total",
		Help:      "The total time spent on git-receive-pack requests",
	}, []string{"repo"})

	uploadArchiveSeconds = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "upload_archive_seconds_total",
		Help:      "The total time spent on git-upload-archive requests",
	}, []string{"repo"})

	lfsAuthenticateSeconds = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "lfs_authenticate_seconds_total",
		Help:      "The total time spent on git-lfs-authenticate requests",
	}, []string{"repo", "operation"})

	lfsTransferSeconds = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "lfs_transfer_seconds_total",
		Help:      "The total time spent on git-lfs-transfer requests",
	}, []string{"repo", "operation"})

	createRepoCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "ssh",
		Name:      "create_repo_total",
		Help:      "The total number of create repo requests",
	}, []string{"repo"})
)

// GitUploadPackCommand returns a cobra command for git-upload-pack.
func GitUploadPackCommand() *cobra.Command {
	return gitCommand("git-upload-pack REPO", "Git upload pack", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return runUploadPack(cmd, args, git.UploadPackService)
	})
}

// GitUploadArchiveCommand returns a cobra command for git-upload-archive.
func GitUploadArchiveCommand() *cobra.Command {
	return gitCommand("git-upload-archive REPO", "Git upload archive", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return runUploadPack(cmd, args, git.UploadArchiveService)
	})
}

// GitReceivePackCommand returns a cobra command for git-receive-pack.
func GitReceivePackCommand() *cobra.Command {
	return gitCommand("git-receive-pack REPO", "Git receive pack", cobra.ExactArgs(1), runReceivePack)
}

// GitLFSAuthenticateCommand returns a cobra command for git-lfs-authenticate.
func GitLFSAuthenticateCommand() *cobra.Command {
	return gitCommand("git-lfs-authenticate REPO OPERATION", "Git LFS authenticate", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		return runLFS(cmd, args, git.LFSAuthenticateService)
	})
}

// GitLFSTransfer returns a cobra command for git-lfs-transfer.
func GitLFSTransfer() *cobra.Command {
	return gitCommand("git-lfs-transfer REPO OPERATION", "Git LFS transfer", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		return runLFS(cmd, args, git.LFSTransferService)
	})
}

// gitCommand returns the hidden command of a git service.
func gitCommand(use, short string, args cobra.PositionalArgs, run func(*cobra.Command, []string) error) *cobra.Command {
	return &cobra.Command{
		Use:    use,
		Short:  short,
		Args:   args,
		Hidden: true,
		RunE:   run,
	}
}

// errLFSAuthNoAccount is returned by git-lfs-authenticate to a caller whose
// key belongs to no account: the token it issues is for an account.
var errLFSAuthNoAccount = errors.New("git-lfs-authenticate needs an account: register this SSH key or use an HTTP(S) remote for LFS")

// gitDenyRead is denyReadAs for git services: ErrNotAuthed when anonymous,
// ErrInvalidRepo otherwise.
func gitDenyRead(anonymous bool) error {
	return denyReadAs(anonymous, git.ErrNotAuthed, git.ErrInvalidRepo)
}

// gitAccess authorizes a git service that needs level need on repo, which
// is nil when the caller may not read it (see backend.Readable). anonymous
// is as for denyRead: no account and not a server admin.
func gitAccess(anonymous bool, repo proto.Repository, have, need access.AccessLevel) error {
	if repo == nil || have < access.ReadOnlyAccess {
		return gitDenyRead(anonymous)
	}
	if have < need {
		return git.ErrNotAuthed
	}
	return nil
}

// gitLookupFailed logs err, from looking up repository repo, and returns
// git.ErrSystemMalfunction for the client: the lookup failed, so the
// repository is neither known missing nor known unreadable. A lookup the
// client canceled is logged at Debug.
func gitLookupFailed(ctx context.Context, logger *log.Logger, repo string, err error) error {
	if ctx.Err() != nil {
		logger.Debug("repository lookup canceled", "repo", repo, "err", err)
	} else {
		logger.Error("failed to look up repository", "repo", repo, "err", err)
	}
	return git.ErrSystemMalfunction
}

// gitServiceError logs a failed git service and returns the error for the
// client. A client that goes away early, such as one that only reads the ref
// advertisement, cancels the session context, or closes its channel so the
// write of git's output fails. git then gets the end of its input (and
// SIGPIPE on its output) and exits; it is killed only if it has not exited
// WaitDelay later. The service reports that as context.Canceled. It is not a
// server failure, so it is logged at Debug and not reported as one.
func gitServiceError(logger *log.Logger, service git.Service, repo string, err error) error {
	if errors.Is(err, context.Canceled) {
		logger.Debug("git service cancelled", "service", service, "err", err, "repo", repo)
		return err
	}

	logger.Error("failed to handle git service", "service", service, "err", err, "repo", repo)
	return git.ErrSystemMalfunction
}

// gitRequest is what every git service over SSH starts from: the
// repository the first argument names, the caller's access to it, and the
// command that runs the service in it.
type gitRequest struct {
	ctx    context.Context
	be     *backend.Backend
	logger *log.Logger
	start  time.Time
	// name is the repository name, sanitized.
	name string
	user proto.User
	// repo is nil when the repository is missing or the caller may not
	// read it; level is the caller's access level either way.
	repo  proto.Repository
	level access.AccessLevel
	// anonymous is as for denyRead: no account and not a server admin.
	anonymous bool
	scmd      git.ServiceCommand
}

// newGitRequest looks up the repository args[0] names for the session's
// caller and prepares the service command: its directory, its input and
// output, and the environment the hooks read.
func newGitRequest(cmd *cobra.Command, args []string) (*gitRequest, error) {
	ctx := cmd.Context()
	cfg := config.FromContext(ctx)
	be := backend.FromContext(ctx)
	logger := log.FromContext(ctx)
	start := time.Now()

	// repo should be in the form of "repo.git"
	name := utils.SanitizeRepo(args[0])
	user := proto.UserFromContext(ctx)
	// git bare repositories should end in ".git"
	// https://git-scm.com/docs/gitrepository-layout
	repoDir := name + ".git"
	reposDir := filepath.Join(cfg.DataPath, "repos")
	if err := git.EnsureWithin(reposDir, repoDir); err != nil {
		return nil, err
	}

	// ReadableForUser applies the same server admin rule as every other SSH
	// command, so a key in initial_admin_keys that no account holds is admin
	// here too. (A listed key held by a non-admin account never gets this
	// far: the SSH middleware refuses its session.)
	repo, level, err := be.ReadableForUser(ctx, name, user)
	if err != nil {
		return nil, gitLookupFailed(ctx, logger, name, err)
	}
	ctx = proto.WithRepositoryContext(ctx, repo)

	repoPath := filepath.Join(reposDir, repoDir)
	// Environment variables to pass down to git hooks.
	envs := []string{
		"SOFT_SERVE_REPO_NAME=" + name,
		"SOFT_SERVE_REPO_PATH=" + repoPath,
		"SOFT_SERVE_PUBLIC_KEY=" + sshutils.MarshalAuthorizedKey(sshutils.PublicKeyFromContext(ctx)),
		"SOFT_SERVE_LOG_PATH=" + filepath.Join(cfg.DataPath, "log", "hooks.log"),
	}
	if user != nil {
		envs = append(envs, "SOFT_SERVE_USERNAME="+user.Username())
	}
	envs = append(envs, cfg.Environ()...)

	// Add GIT_PROTOCOL from session.
	if sess := sshutils.SessionFromContext(ctx); sess != nil {
		for _, env := range sess.Environ() {
			if strings.HasPrefix(env, "GIT_PROTOCOL=") {
				envs = append(envs, env)
				break
			}
		}
	}

	return &gitRequest{
		ctx:    ctx,
		be:     be,
		logger: logger,
		start:  start,
		name:   name,
		user:   user,
		repo:   repo,
		level:  level,
		// As in denyRead, a server admin is never anonymous: nothing is
		// hidden from them.
		anonymous: user == nil && !isServerAdmin(ctx),
		scmd: git.ServiceCommand{
			Stdin:  cmd.InOrStdin(),
			Stdout: cmd.OutOrStdout(),
			Stderr: cmd.ErrOrStderr(),
			Env:    envs,
			Dir:    repoPath,
		},
	}, nil
}

// setRepo makes repo the request's repository, as after looking the name up
// again.
func (r *gitRequest) setRepo(repo proto.Repository, level access.AccessLevel) {
	r.repo, r.level = repo, level
	r.ctx = proto.WithRepositoryContext(r.ctx, repo)
}

// runUploadPack runs git-upload-pack or git-upload-archive, which read the
// repository.
func runUploadPack(cmd *cobra.Command, args []string, service git.Service) error {
	r, err := newGitRequest(cmd, args)
	if err != nil {
		return err
	}
	if err := gitAccess(r.anonymous, r.repo, r.level, access.ReadOnlyAccess); err != nil {
		return err
	}

	counter, seconds := uploadPackCounter, uploadPackSeconds
	if service == git.UploadArchiveService {
		counter, seconds = uploadArchiveCounter, uploadArchiveSeconds
	}
	counter.WithLabelValues(r.name).Inc()
	defer func() {
		seconds.WithLabelValues(r.name).Add(time.Since(r.start).Seconds())
	}()

	if err := service.Handler(r.ctx, r.scmd); err != nil {
		return gitServiceError(r.logger, service, r.name, err)
	}
	return nil
}

// runReceivePack runs git-receive-pack. A push to a missing name that the
// caller may write creates the repository; if the push then fails, the
// backend removes the repository again unless it was used meanwhile (see
// Backend.RemovePushCreatedRepository).
func runReceivePack(cmd *cobra.Command, args []string) error {
	r, err := newGitRequest(cmd, args)
	if err != nil {
		return err
	}
	be := r.be

	// endPush ends the push that BeginPush marked as running, at the
	// latest when the command returns.
	endPush := func() {}
	defer func() { endPush() }()

	if r.repo != nil {
		if err := gitAccess(r.anonymous, r.repo, r.level, access.ReadWriteAccess); err != nil {
			return err
		}
		if r.repo.IsMirror() {
			return proto.ErrMirrorReadOnly
		}
		// Hold off the removal of a repository a push created and left
		// without refs while this push may write some.
		done, err := beginPush(r.ctx, be, r.logger, r.repo)
		switch {
		case err == nil:
			endPush = done
		case errors.Is(err, proto.ErrRepoNotFound):
			// It was removed while this push waited. Look the name up
			// again, so the push creates it as a push to a missing name
			// does.
			repo, level, lerr := be.ReadableForUser(r.ctx, r.name, r.user)
			if lerr != nil {
				return gitLookupFailed(r.ctx, r.logger, r.name, lerr)
			}
			r.setRepo(repo, level)
			if repo != nil {
				// Another repository took the name meanwhile.
				return err
			}
		default:
			return err
		}
	}

	// A missing name is created for a caller who may write; anything else
	// goes through the same check as every other service.
	var created proto.Repository
	if r.repo == nil {
		if r.level < access.ReadWriteAccess {
			return gitAccess(r.anonymous, r.repo, r.level, access.ReadWriteAccess)
		}
		if created, err = createByPush(r); err != nil {
			return err
		}
		// git writes to the repository's directory, found by name. Before
		// it does, BeginPush confirms that the name still belongs to the
		// row just created: an admin may have deleted the repository
		// since, and someone else created another under the same name,
		// which this push was never checked against.
		done, err := beginPush(r.ctx, be, r.logger, created)
		if err != nil {
			// Deleted, and perhaps replaced, already.
			return err
		}
		endPush = done
		r.setRepo(created, r.level)
	}

	// Count only pushes that passed the access check, so a refused name
	// never becomes a metric label.
	receivePackCounter.WithLabelValues(r.name).Inc()
	defer func() {
		receivePackSeconds.WithLabelValues(r.name).Add(time.Since(r.start).Seconds())
	}()

	if err := git.ReceivePackService.Handler(r.ctx, r.scmd); err != nil {
		if created != nil {
			// The push may have failed because the session ended, so the
			// removal must not use its context. It must run after the
			// push has ended, or it would find this push running and
			// keep the repository.
			endPush()
			endPush = func() {}
			removePushCreated(context.WithoutCancel(r.ctx), r, created)
		}
		return gitServiceError(r.logger, git.ReceivePackService, r.name, err)
	}

	if err := git.EnsureDefaultBranch(r.ctx, r.scmd.Dir); err != nil {
		r.logger.Error("failed to ensure default branch", "err", err, "repo", r.name)
		return git.ErrSystemMalfunction
	}
	return nil
}

// createByPush creates the missing repository r names for a push to it.
func createByPush(r *gitRequest) (proto.Repository, error) {
	private, err := r.be.DefaultPrivate(r.ctx, r.user)
	if errors.Is(err, proto.ErrAnonymousPrivate) {
		return nil, fmt.Errorf("%w; authenticate to create %s by pushing, or ask an admin", err, r.name)
	}
	if err != nil {
		return nil, err
	}
	created, err := r.be.CreateRepository(r.ctx, r.name, r.user, proto.RepositoryOptions{Private: private})
	if err != nil {
		if errors.Is(err, proto.ErrLookupFailed) {
			return nil, gitLookupFailed(r.ctx, r.logger, r.name, err)
		}
		// Mostly a bad name or a limit, which the client is told; the
		// backend logs its own failures.
		r.logger.Debug("failed to create repo", "err", err, "repo", r.name)
		return nil, err
	}
	createRepoCounter.WithLabelValues(r.name).Inc()
	return created, nil
}

// removePushCreated removes created, the repository a push created, after
// the push failed, unless it was used meanwhile; see
// Backend.RemovePushCreatedRepository. receive-pack updates the refs and
// reports success to the client before the post-receive hooks run, so a
// push can fail after the client was told it succeeded, for example when
// the client goes away while the hooks run; such a repository is kept, with
// its HEAD on a pushed branch.
func removePushCreated(ctx context.Context, r *gitRequest, created proto.Repository) {
	removed, err := r.be.RemovePushCreatedRepository(ctx, created)
	switch {
	case errors.Is(err, proto.ErrRepoNotFound):
		// Deleted, or renamed, meanwhile.
	case err != nil:
		r.logger.Error("failed to remove repo created by a failed push", "err", err, "repo", r.name)
	case removed:
		r.logger.Debug("removed repo created by a failed push", "repo", r.name)
	default:
		r.logger.Warn("keeping repo created by a failed push, it was used meanwhile", "repo", r.name)
	}
}

// runLFS runs git-lfs-transfer or git-lfs-authenticate for the operation
// args[1] names.
func runLFS(cmd *cobra.Command, args []string, service git.Service) error {
	r, err := newGitRequest(cmd, args)
	if err != nil {
		return err
	}

	operation := args[1]
	var need access.AccessLevel
	switch operation {
	case lfs.OperationDownload:
		need = access.ReadOnlyAccess
	case lfs.OperationUpload:
		need = access.ReadWriteAccess
	default:
		return git.ErrInvalidRequest
	}

	if service == git.LFSAuthenticateService {
		// git-lfs-authenticate issues a token for the account, and the
		// HTTP LFS endpoint checks that token against the account's own
		// access; it knows nothing about admin keys. Authorize with the
		// account's access here too, so a listed admin key with no account
		// gets the answer any caller without an account gets.
		// git-lfs-transfer stays on SSH and keeps the admin key's access.
		repo, level, lerr := r.be.Readable(r.ctx, r.name, r.user)
		if lerr != nil {
			return gitLookupFailed(r.ctx, r.logger, r.name, lerr)
		}
		// Keep the context in step with the repo checked here, so the
		// handler never sees the one ReadableForUser looked up.
		r.setRepo(repo, level)
		r.anonymous = r.user == nil
	}

	if err := gitAccess(r.anonymous, r.repo, r.level, need); err != nil {
		return err
	}

	r.scmd.Args = []string{r.name, operation}
	counter, seconds := lfsTransferCounter, lfsTransferSeconds
	run := git.LFSTransfer
	if service == git.LFSAuthenticateService {
		// A caller without an account that may read the repository, such
		// as an unregistered key on a public repository with anonymous
		// access, has no account to issue a token for. Say so instead of
		// failing in the handler. The repository is readable to it, so
		// this reveals nothing about it.
		if r.user == nil {
			r.logger.Debug("refusing git-lfs-authenticate without an account", "repo", r.name, "operation", operation)
			return errLFSAuthNoAccount
		}
		counter, seconds = lfsAuthenticateCounter, lfsAuthenticateSeconds
		run = git.LFSAuthenticate
	} else if operation == lfs.OperationUpload {
		// An upload writes to the repository like a push, so it holds off
		// the same removal.
		done, err := beginPush(r.ctx, r.be, r.logger, r.repo)
		if err != nil {
			return err
		}
		defer done()
	}

	counter.WithLabelValues(r.name, operation).Inc()
	defer func() {
		seconds.WithLabelValues(r.name, operation).Add(time.Since(r.start).Seconds())
	}()

	if err := run(r.ctx, r.scmd); err != nil {
		return gitServiceError(r.logger, service, r.name, err)
	}
	return nil
}

// testHookBeforeBeginPush, when set, runs in beginPush before the push is
// marked as running: for a push to a missing name, right after the push
// created the repository.
var testHookBeforeBeginPush func()

// beginPush marks a push or LFS upload to repo as running; see
// backend.BeginPush. A repository gone or replaced meanwhile is
// proto.ErrRepoNotFound; a failed lookup of it is logged, and the client
// gets git.ErrSystemMalfunction (see gitLookupFailed).
func beginPush(ctx context.Context, be *backend.Backend, logger *log.Logger, repo proto.Repository) (func(), error) {
	if testHookBeforeBeginPush != nil {
		testHookBeforeBeginPush()
	}
	done, err := be.BeginPush(ctx, repo)
	if err != nil && !errors.Is(err, proto.ErrRepoNotFound) {
		return nil, gitLookupFailed(ctx, logger, repo.Name(), err)
	}
	return done, err
}
