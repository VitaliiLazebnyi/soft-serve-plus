package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/gitserve"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/lfs/sshlfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/spf13/cobra"
)

// The git service metrics carry no repository label: anyone who may create
// repositories could otherwise add series without limit, for names that
// are deleted or renamed and never come back.
var (
	uploadPackCounter = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "upload_pack_total",
		Help:      "The total number of git-upload-pack requests",
	})

	receivePackCounter = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "receive_pack_total",
		Help:      "The total number of git-receive-pack requests",
	})

	uploadArchiveCounter = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "upload_archive_total",
		Help:      "The total number of git-upload-archive requests",
	})

	lfsAuthenticateCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "lfs_authenticate_total",
		Help:      "The total number of git-lfs-authenticate requests",
	}, []string{"operation"})

	lfsTransferCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "lfs_transfer_total",
		Help:      "The total number of git-lfs-transfer requests",
	}, []string{"operation"})

	uploadPackSeconds = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "upload_pack_seconds_total",
		Help:      "The total time spent on git-upload-pack requests",
	})

	receivePackSeconds = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "receive_pack_seconds_total",
		Help:      "The total time spent on git-receive-pack requests",
	})

	uploadArchiveSeconds = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "upload_archive_seconds_total",
		Help:      "The total time spent on git-upload-archive requests",
	})

	lfsAuthenticateSeconds = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "lfs_authenticate_seconds_total",
		Help:      "The total time spent on git-lfs-authenticate requests",
	}, []string{"operation"})

	lfsTransferSeconds = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "lfs_transfer_seconds_total",
		Help:      "The total time spent on git-lfs-transfer requests",
	}, []string{"operation"})

	createRepoCounter = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "ssh",
		Name:      "create_repo_total",
		Help:      "The total number of repositories created by a push over SSH",
	})
)

// GitUploadPackCommand returns a cobra command for git-upload-pack.
func (cmds *Commands) GitUploadPackCommand() *cobra.Command {
	return gitCommand("git-upload-pack REPO", "Git upload pack", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return cmds.runUploadPack(cmd, args, gitserve.UploadPackService)
	})
}

// GitUploadArchiveCommand returns a cobra command for git-upload-archive.
func (cmds *Commands) GitUploadArchiveCommand() *cobra.Command {
	return gitCommand("git-upload-archive REPO", "Git upload archive", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return cmds.runUploadPack(cmd, args, gitserve.UploadArchiveService)
	})
}

// GitReceivePackCommand returns a cobra command for git-receive-pack.
func (cmds *Commands) GitReceivePackCommand() *cobra.Command {
	return gitCommand("git-receive-pack REPO", "Git receive pack", cobra.ExactArgs(1), cmds.runReceivePack)
}

// GitLFSAuthenticateCommand returns a cobra command for git-lfs-authenticate.
func (cmds *Commands) GitLFSAuthenticateCommand() *cobra.Command {
	return gitCommand("git-lfs-authenticate REPO OPERATION", "Git LFS authenticate", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		return cmds.runLFS(cmd, args, gitserve.LFSAuthenticateService)
	})
}

// GitLFSTransferCommand returns a cobra command for git-lfs-transfer.
func (cmds *Commands) GitLFSTransferCommand() *cobra.Command {
	return gitCommand("git-lfs-transfer REPO OPERATION", "Git LFS transfer", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		return cmds.runLFS(cmd, args, gitserve.LFSTransferService)
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
	return denyReadAs(anonymous, gitserve.ErrNotAuthed, gitserve.ErrInvalidRepo)
}

// gitAccess authorizes a git service that needs level need on repo, which
// is nil when the caller may not read it (see backend.Readable). anonymous
// is as for denyRead: no account and not a server admin.
func gitAccess(anonymous bool, repo proto.Repository, have, need access.Level) error {
	if repo == nil || have < access.ReadOnlyAccess {
		return gitDenyRead(anonymous)
	}
	if have < need {
		return gitserve.ErrNotAuthed
	}
	return nil
}

// gitLookupFailed logs err, from looking up repository repo, and returns
// gitserve.ErrSystemMalfunction for the client: the lookup failed, so the
// repository is neither known missing nor known unreadable. A lookup the
// client canceled is logged at Debug.
func gitLookupFailed(ctx context.Context, logger *log.Logger, repo string, err error) error {
	if ctx.Err() != nil {
		logger.Debug("repository lookup canceled", "repo", repo, "err", err)
	} else {
		logger.Error("failed to look up repository", "repo", repo, "err", err)
	}
	return gitserve.ErrSystemMalfunction
}

// gitServiceError logs a failed git service and returns the error for the
// client. A client that goes away early, such as one that only reads the ref
// advertisement, closes its session channel, which ends the session context,
// or the whole connection; a write of git's output to it then fails as well.
// git gets the end of its input (and SIGPIPE on its output) and exits; it is
// killed only if it has not exited WaitDelay later. The service reports that
// as context.Canceled; or, when git saw the end of its input and failed
// before the cancellation reached the service, as git's own failure, with
// ctx done or the channel closed. None of these is a server failure, so they
// are logged at Debug and not reported as one.
func gitServiceError(ctx context.Context, logger *log.Logger, service gitserve.Service, repo string, err error) error {
	if !errors.Is(err, context.Canceled) && (ctx.Err() != nil || channelClosed(ctx)) {
		err = fmt.Errorf("client went away: %w (%w)", context.Canceled, err)
	}
	if errors.Is(err, context.Canceled) {
		logger.Debug("git service cancelled", "service", service, "err", err, "repo", repo)
		return err
	}

	logger.Error("failed to handle git service", "service", service, "err", err, "repo", repo)
	return gitserve.ErrSystemMalfunction
}

// channelClosed reports whether the client has closed the session's channel,
// by sending it a keepalive request that wants no reply: on a closed channel
// the send fails at once, and nothing goes out. x/crypto marks the channel
// closed before it ends the client's input there, so a git that failed for
// the end of its input after the client closed the channel always finds it
// marked, even before the session context ends. On an open channel the
// client ignores the request, as OpenSSH does keepalives it gets no reply
// for.
func channelClosed(ctx context.Context) bool {
	s := SessionFromContext(ctx)
	if s == nil {
		return false
	}
	_, err := s.SendRequest("keepalive@openssh.com", false, nil)
	return err != nil
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
	level access.Level
	// anonymous is as for denyRead: no account and not a server admin.
	anonymous bool
	cfg       *config.Config
	// protocol is the session's GIT_PROTOCOL, or "".
	protocol string
	scmd     gitserve.ServiceCommand
}

// hookEnv returns the environment receive-pack and its hooks run with, refs
// being where post-receive writes the refs the push updated.
func (r *gitRequest) hookEnv(refs *hooks.PushRefs) []string {
	return hooks.Env(r.cfg, hooks.Request{
		Transport: hooks.TransportSSH,
		Repo:      r.name,
		User:      r.user,
		PublicKey: proto.PublicKeyFromContext(r.ctx),
		Protocol:  r.protocol,
		Refs:      refs,
	})
}

// newPushRefs returns the file a push's post-receive hook writes its refs
// to, or nil, after logging why, when it cannot be created: the push then
// goes ahead without sending its webhooks.
func (r *gitRequest) newPushRefs() *hooks.PushRefs {
	refs, err := hooks.NewPushRefs(r.cfg)
	if err != nil {
		r.logger.Error("push webhooks will not be sent", "repo", r.name, "err", err)
		return nil
	}
	return refs
}

// afterPush removes refs and hands the refs the push updated to the
// backend, which does the work of the post-receive and post-update hooks.
// The refs are updated already, whether or not the client stayed to the
// end, so this runs either way, and not with the session's context.
func (r *gitRequest) afterPush(refs *hooks.PushRefs) {
	if refs == nil {
		return
	}
	updated, err := refs.Refs()
	if err != nil {
		r.logger.Error("failed to read the pushed refs", "repo", r.name, "err", err)
	}
	if err := refs.Remove(); err != nil {
		r.logger.Warn("failed to remove the push refs file", "err", err)
	}
	r.be.AfterPush(context.WithoutCancel(r.ctx), r.repo, r.user, updated)
}

// newGitRequest looks up the repository args[0] names for the session's
// caller and prepares the service command: its directory, its input and
// output, and GIT_PROTOCOL. runReceivePack adds the hook environment.
func (cmds *Commands) newGitRequest(cmd *cobra.Command, args []string) (*gitRequest, error) {
	ctx := cmd.Context()
	cfg, be, logger := cmds.cfg, cmds.be, cmds.logger
	start := time.Now()

	// repo should be in the form of "repo.git"
	name := proto.SanitizeRepo(args[0])
	user := proto.UserFromContext(ctx)
	if err := gitserve.EnsureWithin(hooks.ReposPath(cfg), name+".git"); err != nil {
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

	// The session's GIT_PROTOCOL, if it sent one, goes to git.
	var protocol string
	if sess := SessionFromContext(ctx); sess != nil {
		for _, env := range sess.Environ() {
			if v, ok := strings.CutPrefix(env, "GIT_PROTOCOL="); ok {
				protocol = v
				break
			}
		}
	}
	// Only receive-pack runs hooks, so only it gets their environment (see
	// runReceivePack).
	envs := hooks.ProtocolEnv(protocol)

	return &gitRequest{
		cfg:      cfg,
		protocol: protocol,
		ctx:      ctx,
		be:       be,
		logger:   logger,
		start:    start,
		name:     name,
		user:     user,
		repo:     repo,
		level:    level,
		// As in denyRead, a server admin is never anonymous: nothing is
		// hidden from them.
		anonymous: user == nil && !cmds.isServerAdmin(ctx),
		scmd: gitserve.ServiceCommand{
			Stdin:  cmd.InOrStdin(),
			Stdout: cmd.OutOrStdout(),
			Stderr: cmd.ErrOrStderr(),
			Env:    envs,
			Dir:    hooks.RepoPath(cfg, name),
			Logger: logger,
		},
	}, nil
}

// setRepo makes repo the request's repository, as after looking the name up
// again.
func (r *gitRequest) setRepo(repo proto.Repository, level access.Level) {
	r.repo, r.level = repo, level
}

// runUploadPack runs git-upload-pack or git-upload-archive, which read the
// repository.
func (cmds *Commands) runUploadPack(cmd *cobra.Command, args []string, service gitserve.Service) error {
	r, err := cmds.newGitRequest(cmd, args)
	if err != nil {
		return err
	}
	if err := gitAccess(r.anonymous, r.repo, r.level, access.ReadOnlyAccess); err != nil {
		return err
	}

	counter, seconds := uploadPackCounter, uploadPackSeconds
	if service == gitserve.UploadArchiveService {
		counter, seconds = uploadArchiveCounter, uploadArchiveSeconds
	}
	counter.Inc()
	defer func() {
		seconds.Add(time.Since(r.start).Seconds())
	}()

	if err := service.Handler(r.ctx, r.scmd); err != nil {
		return gitServiceError(r.ctx, r.logger, service, r.name, err)
	}
	return nil
}

// runReceivePack runs git-receive-pack. A push to a missing name that the
// caller may write creates the repository; if the push then fails, the
// backend removes the repository again unless it was used meanwhile (see
// Backend.RemovePushCreatedRepository).
func (cmds *Commands) runReceivePack(cmd *cobra.Command, args []string) error {
	r, err := cmds.newGitRequest(cmd, args)
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
		done, err := cmds.beginPush(r, r.repo)
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
		done, err := cmds.beginPush(r, created)
		if err != nil {
			// Deleted, and perhaps replaced, already.
			return err
		}
		endPush = done
		r.setRepo(created, r.level)
	}

	receivePackCounter.Inc()
	defer func() {
		receivePackSeconds.Add(time.Since(r.start).Seconds())
	}()

	refs := r.newPushRefs()
	defer r.afterPush(refs)
	r.scmd.Env = r.hookEnv(refs)
	if err := gitserve.ReceivePackService.Handler(r.ctx, r.scmd); err != nil {
		if created != nil {
			// The push may have failed because the session ended, so the
			// removal must not use its context. It must run after the
			// push has ended, or it would find this push running and
			// keep the repository.
			endPush()
			endPush = func() {}
			removePushCreated(context.WithoutCancel(r.ctx), r, created)
		}
		return gitServiceError(r.ctx, r.logger, gitserve.ReceivePackService, r.name, err)
	}

	// The push succeeded, so HEAD is fixed even if the client has gone
	// meanwhile, as the failure path does.
	if err := gitserve.EnsureDefaultBranch(context.WithoutCancel(r.ctx), r.scmd.Dir); err != nil {
		r.logger.Error("failed to ensure default branch", "err", err, "repo", r.name)
		return gitserve.ErrSystemMalfunction
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
	createRepoCounter.Inc()
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
func (cmds *Commands) runLFS(cmd *cobra.Command, args []string, service gitserve.Service) error {
	r, err := cmds.newGitRequest(cmd, args)
	if err != nil {
		return err
	}

	operation := args[1]
	var need access.Level
	switch operation {
	case lfs.OperationDownload:
		need = access.ReadOnlyAccess
	case lfs.OperationUpload:
		need = access.ReadWriteAccess
	default:
		return gitserve.ErrInvalidRequest
	}

	if service == gitserve.LFSAuthenticateService {
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

	counter, seconds := lfsTransferCounter, lfsTransferSeconds
	run := func() error {
		deps := sshlfs.TransferDeps{
			Config: r.cfg,
			DB:     cmds.db,
			Store:  cmds.store,
			Logger: r.logger,
		}
		return sshlfs.Transfer(r.ctx, r.scmd.Stdin, r.scmd.Stdout, deps, r.repo, r.user, operation)
	}
	if service == gitserve.LFSAuthenticateService {
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
		run = func() error {
			return sshlfs.Authenticate(r.scmd.Stdout, r.cfg, cmds.tokens, r.logger, r.repo, r.user, operation)
		}
	} else if operation == lfs.OperationUpload {
		// An upload writes to the repository like a push, so it holds off
		// the same removal.
		done, err := cmds.beginPush(r, r.repo)
		if err != nil {
			return err
		}
		defer done()
	}

	counter.WithLabelValues(operation).Inc()
	defer func() {
		seconds.WithLabelValues(operation).Add(time.Since(r.start).Seconds())
	}()

	if err := run(); err != nil {
		return gitServiceError(r.ctx, r.logger, service, r.name, err)
	}
	return nil
}

// beginPush marks a push or LFS upload of r to repo as running; see
// backend.BeginPush. A repository gone or replaced meanwhile is
// proto.ErrRepoNotFound; a failed lookup of it is logged, and the client
// gets gitserve.ErrSystemMalfunction (see gitLookupFailed).
func (cmds *Commands) beginPush(r *gitRequest, repo proto.Repository) (func(), error) {
	if cmds.beforeBeginPush != nil {
		cmds.beforeBeginPush()
	}
	done, err := r.be.BeginPush(r.ctx, repo)
	if err != nil && !errors.Is(err, proto.ErrRepoNotFound) {
		return nil, gitLookupFailed(r.ctx, r.logger, repo.Name(), err)
	}
	return done, err
}
