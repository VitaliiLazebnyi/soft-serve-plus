package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
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
	cmd := &cobra.Command{
		Use:    "git-upload-pack REPO",
		Short:  "Git upload pack",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE:   gitRunE,
	}

	return cmd
}

// GitUploadArchiveCommand returns a cobra command for git-upload-archive.
func GitUploadArchiveCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "git-upload-archive REPO",
		Short:  "Git upload archive",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE:   gitRunE,
	}

	return cmd
}

// GitReceivePackCommand returns a cobra command for git-receive-pack.
func GitReceivePackCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "git-receive-pack REPO",
		Short:  "Git receive pack",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE:   gitRunE,
	}

	return cmd
}

// GitLFSAuthenticateCommand returns a cobra command for git-lfs-authenticate.
func GitLFSAuthenticateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "git-lfs-authenticate REPO OPERATION",
		Short:  "Git LFS authenticate",
		Args:   cobra.ExactArgs(2),
		Hidden: true,
		RunE:   gitRunE,
	}

	return cmd
}

// GitLFSTransfer returns a cobra command for git-lfs-transfer.
func GitLFSTransfer() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "git-lfs-transfer REPO OPERATION",
		Short:  "Git LFS transfer",
		Args:   cobra.ExactArgs(2),
		Hidden: true,
		RunE:   gitRunE,
	}

	return cmd
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

// gitServiceError logs a failed git service and returns the error for the
// client. A client that goes away early, such as one that only reads the ref
// advertisement, cancels the session context and git is killed for it. That
// is not a server failure, so it is logged at Debug and not reported as one.
func gitServiceError(logger *log.Logger, service git.Service, repo string, err error) error {
	if errors.Is(err, context.Canceled) {
		logger.Debug("git service cancelled", "service", service, "err", err, "repo", repo)
		return err
	}

	logger.Error("failed to handle git service", "service", service, "err", err, "repo", repo)
	return git.ErrSystemMalfunction
}

// deleteFailedPushRepo deletes a repository created by a push that failed,
// unless the push already wrote refs to it. receive-pack updates the refs and
// reports success to the client before the post-receive hooks run, so a push
// can fail after the client was told it succeeded, for example when the client
// goes away while the hooks run. Such a repository is kept, and its HEAD is
// pointed at a pushed branch as after a push that finished, so it does not
// name a branch that does not exist.
//
// The push may have failed because the session context was cancelled, so ctx
// must not be the cancelled one.
func deleteFailedPushRepo(ctx context.Context, be *backend.Backend, logger *log.Logger, name, dir string) {
	removeFailedPushRepo(ctx, logger, name, dir, failedPushRepoOps{
		del:       be.DeleteRepository,
		removeDir: be.RemoveRepositoryDir,
		rowExists: func(ctx context.Context, name string) (bool, error) {
			_, err := be.Repository(ctx, name)
			if errors.Is(err, proto.ErrRepoNotFound) {
				return false, nil
			}
			return err == nil, err
		},
	}, failedPushRetry)
}

// failedPushRepoOps are the backend operations removeFailedPushRepo uses.
type failedPushRepoOps struct {
	// del deletes the repository, its row first and then its directory.
	del func(ctx context.Context, name string) error
	// rowExists reports whether a repository with the name is listed.
	rowExists func(ctx context.Context, name string) (bool, error)
	// removeDir removes the leftover directory of a deleted repository and
	// the group directories it leaves empty.
	removeDir func(dir string) error
}

// failedPushRetry is how removeFailedPushRepo retries: a first wait of 100ms,
// doubled after each attempt, within a 5s budget. The waits that fit add up
// to 3.1s (100ms to 1.6s), since the next one would go past the budget.
var failedPushRetry = retryPolicy{first: 100 * time.Millisecond, total: 5 * time.Second}

// retryPolicy waits first, doubles the wait after each attempt, and gives up
// once the sum of the waits would go past total. Only the waits count
// toward total, not the time the attempts take or a sleep that runs over, so
// the number of attempts is the same however slow the host is: on Windows
// starting git for the refs check alone can take a large part of a short
// budget, and a sleep can run past its wait by the timer resolution. The
// time spent waiting is the sum of the waits that fit, which can be well
// short of total.
type retryPolicy struct {
	first, total time.Duration
}

// removeFailedPushRepo is deleteFailedPushRepo with the backend operations
// and the retry policy passed in.
//
// The delete is retried until the repository's directory is gone. On
// Windows the git wrapper runs receive-pack in a child process that outlives
// the killed wrapper for a moment and holds the directory, so the first
// removal can fail. The delete removes the repository's row before its
// directory, and a failure to remove the directory leaves the directory
// behind.
//
// The refs are checked again before each attempt as long as the repository's
// row exists, since the surviving receive-pack could still write them, and
// only while the directory is still the one this push created. Once
// the row is gone the repository is deleted, and later attempts only remove
// the leftover directory, and only while no repository with the name is
// listed and the directory is still the one this push created: another
// session may have removed the leftover and created a new repository with
// the name, which must be left alone.
func removeFailedPushRepo(ctx context.Context, logger *log.Logger, name, dir string, ops failedPushRepoOps, policy retryPolicy) {
	var waited time.Duration
	wait := policy.first
	rowGone := false
	// orig identifies the directory this push created, so a directory
	// re-created at the same path is not removed as its leftover.
	orig, _ := dirIdentity(dir)
	for attempt := 0; ; attempt++ {
		var err error
		if !rowGone {
			// A delete that failed may have failed with no row left, as
			// when DeleteRepository could not remove the directory as an
			// orphan, so the directory is checked to be the one this push
			// created before its refs are checked and it is deleted again:
			// another session may have removed it and created a new
			// repository with the name.
			if attempt > 0 {
				switch same, serr := sameDir(orig, dir); {
				case errors.Is(serr, fs.ErrNotExist):
					return
				case serr != nil:
					logger.Error("failed to identify directory of repo created by a failed push, keeping it", "err", serr, "repo", name)
					return
				case !same:
					logger.Warn("directory of repo created by a failed push was re-created, leaving it", "repo", name)
					return
				}
			}
			// --git-dir keeps git from looking for a repository in the
			// parent directories if this one is not one.
			out, cerr := exec.CommandContext(ctx, "git", "--git-dir="+dir, "for-each-ref", "--count=1").Output()
			if cerr != nil {
				logger.Error("failed to check refs of repo created by a failed push, keeping it", "err", cerr, "repo", name)
				return
			}
			if len(bytes.TrimSpace(out)) > 0 {
				logger.Warn("keeping repo created by a failed push, it already has refs", "repo", name)
				if err := git.EnsureDefaultBranch(ctx, dir); err != nil {
					logger.Warn("failed to ensure default branch of kept repo", "err", err, "repo", name)
				}
				return
			}

			err = ops.del(ctx, name)
			if err == nil || errors.Is(err, proto.ErrRepoNotFound) {
				rowGone = true
			}
		} else {
			exists, rerr := ops.rowExists(ctx, name)
			if rerr != nil {
				logger.Error("failed to look up repo created by a failed push, leaving its directory", "err", rerr, "repo", name)
				return
			}
			if exists {
				logger.Warn("repo created by a failed push was re-created, leaving it", "repo", name)
				return
			}
			switch same, serr := sameDir(orig, dir); {
			case errors.Is(serr, fs.ErrNotExist):
				return
			case serr != nil:
				// The directory cannot be identified, so it is not removed
				// on this attempt.
				err = serr
			case !same:
				logger.Warn("directory of repo created by a failed push was re-created, leaving it", "repo", name)
				return
			default:
				err = ops.removeDir(dir)
			}
		}

		if rowGone {
			if _, serr := os.Lstat(dir); errors.Is(serr, fs.ErrNotExist) {
				return
			}
		}

		if waited+wait > policy.total {
			if err == nil {
				err = errors.New("repository directory still exists")
			}
			logger.Error("failed to delete repo created by a failed push", "err", err, "repo", name)
			return
		}
		time.Sleep(wait)
		waited += wait
		wait *= 2
	}
}

// sameDir reports whether the directory at dir is the one orig identifies.
// A nil orig, a directory that could not be identified at the start, matches
// nothing.
func sameDir(orig os.FileInfo, dir string) (bool, error) {
	fi, err := dirIdentity(dir)
	if err != nil {
		return false, err
	}
	return orig != nil && os.SameFile(orig, fi), nil
}

// dirIdentity returns the FileInfo of the directory at dir with its identity
// loaded, for os.SameFile. It stats an open handle rather than the path: on
// Windows os.Lstat records only the path, and os.SameFile loads the volume
// and file index from that path when it is called, so two Lstat results taken
// at different times would both describe whatever is at the path at
// comparison time. File.Stat reads the identity from the handle right away.
// The handle is opened sharing read and write, so a process that has the
// directory as its working directory does not make it fail.
func dirIdentity(dir string) (os.FileInfo, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint: errcheck
	return f.Stat()
}

func gitRunE(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	cfg := config.FromContext(ctx)
	be := backend.FromContext(ctx)
	logger := log.FromContext(ctx)
	start := time.Now()

	// repo should be in the form of "repo.git"
	name := utils.SanitizeRepo(args[0])
	pk := sshutils.PublicKeyFromContext(ctx)
	ak := sshutils.MarshalAuthorizedKey(pk)
	user := proto.UserFromContext(ctx)
	// git bare repositories should end in ".git"
	// https://git-scm.com/docs/gitrepository-layout
	repoDir := name + ".git"
	reposDir := filepath.Join(cfg.DataPath, "repos")
	if err := git.EnsureWithin(reposDir, repoDir); err != nil {
		return err
	}

	// Set repo in context. readableRepo applies the same server admin rule
	// as every other SSH command, so a key in initial_admin_keys that no
	// account holds is admin here too. (A listed key held by a non-admin
	// account never gets this far: the SSH middleware refuses its session.)
	repo, accessLevel := readableRepo(ctx, name)
	ctx = proto.WithRepositoryContext(ctx, repo)
	// As in denyRead, a server admin is never anonymous: nothing is hidden
	// from them.
	anonymous := user == nil && !isServerAdmin(ctx)

	// Environment variables to pass down to git hooks.
	envs := []string{
		"SOFT_SERVE_REPO_NAME=" + name,
		"SOFT_SERVE_REPO_PATH=" + filepath.Join(reposDir, repoDir),
		"SOFT_SERVE_PUBLIC_KEY=" + ak,
		"SOFT_SERVE_LOG_PATH=" + filepath.Join(cfg.DataPath, "log", "hooks.log"),
	}

	if user != nil {
		envs = append(envs,
			"SOFT_SERVE_USERNAME="+user.Username(),
		)
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

	repoPath := filepath.Join(reposDir, repoDir)
	service := git.Service(cmd.Name())
	stdin := cmd.InOrStdin()
	stdout := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()
	scmd := git.ServiceCommand{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Env:    envs,
		Dir:    repoPath,
	}

	switch service {
	case git.ReceivePackService:
		if repo != nil {
			if err := gitAccess(anonymous, repo, accessLevel, access.ReadWriteAccess); err != nil {
				return err
			}
			if repo.IsMirror() {
				return proto.ErrMirrorReadOnly
			}
			// Hold off the removal of a repository an HTTP push created
			// and left without refs while this push may write some.
			done, err := beginPush(ctx, be, repo)
			switch {
			case err == nil:
				defer done()
			case errors.Is(err, proto.ErrRepoNotFound):
				// It was removed while this push waited. Look the name
				// up again, so the push creates it as a push to a
				// missing name does.
				repo, accessLevel = readableRepo(ctx, name)
				ctx = proto.WithRepositoryContext(ctx, repo)
				if repo != nil {
					// Another repository took the name meanwhile.
					return err
				}
			default:
				return err
			}
		}

		// A missing name is created for a caller who may write; anything
		// else goes through the same check as every other service.
		created := repo == nil && accessLevel >= access.ReadWriteAccess
		if created {
			private, err := be.DefaultPrivate(ctx, user)
			if errors.Is(err, proto.ErrAnonymousPrivate) {
				return fmt.Errorf("%w; authenticate to create %s by pushing, or ask an admin", err, name)
			}
			if err != nil {
				return err
			}
			if _, err := be.CreateRepository(ctx, name, user, proto.RepositoryOptions{Private: private}); err != nil {
				// Mostly a bad name or a limit, which the client is told;
				// the backend logs its own failures.
				logger.Debug("failed to create repo", "err", err, "repo", name)
				return err
			}
			createRepoCounter.WithLabelValues(name).Inc()
		} else if repo == nil {
			return gitAccess(anonymous, repo, accessLevel, access.ReadWriteAccess)
		}

		// Count only pushes that passed the access check, so a refused
		// name never becomes a metric label.
		receivePackCounter.WithLabelValues(name).Inc()
		defer func() {
			receivePackSeconds.WithLabelValues(name).Add(time.Since(start).Seconds())
		}()

		if err := service.Handler(ctx, scmd); err != nil {
			if created {
				deleteFailedPushRepo(context.WithoutCancel(ctx), be, logger, name, repoPath)
			}

			return gitServiceError(logger, service, name, err)
		}

		if err := git.EnsureDefaultBranch(ctx, scmd.Dir); err != nil {
			logger.Error("failed to ensure default branch", "err", err, "repo", name)
			return git.ErrSystemMalfunction
		}

		receivePackCounter.WithLabelValues(name).Inc()

		return nil
	case git.UploadPackService, git.UploadArchiveService:
		if err := gitAccess(anonymous, repo, accessLevel, access.ReadOnlyAccess); err != nil {
			return err
		}

		switch service {
		case git.UploadArchiveService:
			uploadArchiveCounter.WithLabelValues(name).Inc()
			defer func() {
				uploadArchiveSeconds.WithLabelValues(name).Add(time.Since(start).Seconds())
			}()
		default:
			uploadPackCounter.WithLabelValues(name).Inc()
			defer func() {
				uploadPackSeconds.WithLabelValues(name).Add(time.Since(start).Seconds())
			}()
		}

		if err := service.Handler(ctx, scmd); err != nil {
			return gitServiceError(logger, service, name, err)
		}

		return nil
	case git.LFSTransferService, git.LFSAuthenticateService:
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
			// account's access here too, so a listed admin key with no
			// account gets the answer any caller without an account gets.
			// git-lfs-transfer stays on SSH and keeps the admin key's
			// access.
			repo, accessLevel = be.Readable(ctx, name, user)
			anonymous = user == nil
			// Keep the context in step with the repo checked here, so the
			// handler never sees the one readableRepo looked up.
			ctx = proto.WithRepositoryContext(ctx, repo)
		}

		if err := gitAccess(anonymous, repo, accessLevel, need); err != nil {
			return err
		}

		// A caller without an account that may read the repository, such
		// as an unregistered key on a public repository with anonymous
		// access, has no account to issue a token for. Say so instead of
		// failing in the handler. The repository is readable to it, so
		// this reveals nothing about it.
		if service == git.LFSAuthenticateService && user == nil {
			logger.Debug("refusing git-lfs-authenticate without an account", "repo", name, "operation", operation)
			return errLFSAuthNoAccount
		}

		// An upload writes to the repository like a push, so it holds off
		// the same removal.
		if service == git.LFSTransferService && operation == lfs.OperationUpload {
			done, err := beginPush(ctx, be, repo)
			if err != nil {
				return err
			}
			defer done()
		}

		scmd.Args = []string{
			name,
			args[1],
		}

		switch service {
		case git.LFSTransferService:
			lfsTransferCounter.WithLabelValues(name, operation).Inc()
			defer func() {
				lfsTransferSeconds.WithLabelValues(name, operation).Add(time.Since(start).Seconds())
			}()
		default:
			lfsAuthenticateCounter.WithLabelValues(name, operation).Inc()
			defer func() {
				lfsAuthenticateSeconds.WithLabelValues(name, operation).Add(time.Since(start).Seconds())
			}()
		}

		if err := service.Handler(ctx, scmd); err != nil {
			return gitServiceError(logger, service, name, err)
		}

		return nil
	}

	return errors.New("unsupported git service")
}

// testHookBeforeBeginPush, when set, runs in beginPush before the push is
// marked as running.
var testHookBeforeBeginPush func()

// beginPush marks a push or LFS upload to repo as running; see
// backend.BeginPush.
func beginPush(ctx context.Context, be *backend.Backend, repo proto.Repository) (func(), error) {
	if testHookBeforeBeginPush != nil {
		testHookBeforeBeginPush()
	}
	return be.BeginPush(ctx, repo)
}
