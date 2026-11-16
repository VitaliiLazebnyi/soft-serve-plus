package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/internal/safego"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"golang.org/x/sync/errgroup"
)

// mirrorRemotes are the remotes of a mirror that passed validation: their
// names, which are what the sync fetches, and their validated urls, which
// render the environment the sync must run with.
type mirrorRemotes struct {
	names     []string
	validated []ssrf.ValidatedGitRemote
}

// validateMirrorRemotes validates every remote configured on a mirror
// repository against address ranges and the operator's ssh host policy, and
// returns them so the caller can fetch exactly those remotes, with the git
// environment their validation requires.
//
// Every remote has to pass, and any failure to read or validate skips the
// sync: a remote that cannot be checked is not one to fetch from.
//
// The remotes and their URLs come from git itself rather than from parsing
// the config: only git knows which of several `url` entries it fetches from,
// how `url.<base>.insteadOf` rewrites them, and which spellings of a remote
// section it accepts. Every URL of a remote is checked, not only the one
// fetched from.
//
// Only fetching each remote by name keeps git to the remotes checked here.
// Without a name git picks for itself: `git fetch` takes the url-or-name in
// `branch.<HEAD>.remote`, and `git remote update` follows the
// `remotes.default` group, either of which can lead to a url or a legacy
// `branches/` or `remotes/` file remote that `git remote` does not list. A
// remote group named like a remote also takes the place of that remote when
// it is fetched by name, so a repository with any remote group is refused.
func validateMirrorRemotes(ctx context.Context, r *git.Repository, policy ssrf.GitRemotePolicy) (mirrorRemotes, error) {
	keys, err := git.NewCommand("config", "--list", "--name-only").WithContext(ctx).RunInDir(r.Path)
	if err != nil {
		return mirrorRemotes{}, fmt.Errorf("reading config: %w", err)
	}
	for _, key := range strings.Split(string(keys), "\n") {
		if strings.HasPrefix(key, "remotes.") {
			return mirrorRemotes{}, fmt.Errorf("remote group %q: %w", strings.TrimPrefix(key, "remotes."), errRemoteGroup)
		}
	}

	out, err := git.NewCommand("remote").WithContext(ctx).RunInDir(r.Path)
	if err != nil {
		return mirrorRemotes{}, fmt.Errorf("listing remotes: %w", err)
	}

	var remotes mirrorRemotes
	for _, name := range strings.Fields(string(out)) {
		// A remote without a url is printed as its own name, which git
		// would fetch from as a local path. That fails validation, as it
		// should.
		out, err := git.NewCommand("remote", "get-url", "--all", "--", name).WithContext(ctx).RunInDir(r.Path)
		if err != nil {
			return mirrorRemotes{}, fmt.Errorf("remote %q: reading url: %w", name, err)
		}

		for _, url := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
			v, err := ssrf.ValidateGitRemoteContext(ctx, url, policy)
			if err != nil {
				return mirrorRemotes{}, fmt.Errorf("remote %q: %w", name, err)
			}
			// fetch and remote update reach each remote by name, so a
			// remote.<name>.proxy must be cleared too.
			remotes.validated = append(remotes.validated, v.Named(name))
		}
		remotes.names = append(remotes.names, name)
	}

	if len(remotes.validated) == 0 {
		return mirrorRemotes{}, fmt.Errorf("no remote url configured")
	}

	return remotes, nil
}

// errRemoteGroup refuses a mirror with a remote group: a group named like a
// remote is fetched in its place, and its members need not be remotes that
// `git remote` lists.
var errRemoteGroup = errors.New("remote groups are not supported on mirrors")

// mirrorRefspec is the refspec every mirror fetch uses, the one git clone
// --mirror writes: every ref of the remote, forced, under its own name. The
// job passes it rather than rely on the repository's config, as a repository
// imported without --mirror and turned into a mirror later has no fetch
// refspec, and fetching it would move no branch at all.
const mirrorRefspec = "+refs/*:refs/*"

// syncMirror fetches each named remote of the mirror at path with
// mirrorRefspec, pruning refs the remote no longer has. Naming every remote
// keeps git from choosing one that was not validated. A remote that fails to
// fetch does not stop the others; their errors are joined. Once ctx is done
// no further fetch starts, and one already running is stopped; see
// runMirrorGit.
func syncMirror(ctx context.Context, path string, names []string, env []string) error {
	var errs []error
	for _, name := range names {
		if ctx.Err() != nil {
			break
		}
		if err := runMirrorGit(ctx, path, env, "fetch", "--prune", "--", name, mirrorRefspec); err != nil {
			errs = append(errs, fmt.Errorf("fetching remote %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// SyncMirrors fetches every pull mirror from its remotes, and their LFS
// objects when LFS is enabled, and records the outcome of each sync (see
// Backend.RecordMirrorSync). Up to GOMAXPROCS mirrors sync at once; it
// returns when all of them are done. Once ctx is done no further mirror
// starts; see syncMirror for what happens to one already running. A mirror
// whose sync panics is logged and does not stop the others.
func SyncMirrors(ctx context.Context) {
	cfg := config.FromContext(ctx)
	logger := log.FromContext(ctx).WithPrefix("jobs.mirror")
	b := backend.FromContext(ctx)

	// The same guards that protect imports protect syncs, and the same
	// too-old git ignores them.
	if reason := b.ImportsDisabledReason(); reason != "" {
		logger.Warn("skipping mirror sync", "reason", reason)
		return
	}

	repos, err := b.Repositories(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		logger.Error("error getting repositories", "err", err)
		return
	}

	logger.Debug("updating mirror repos")
	var g errgroup.Group
	g.SetLimit(runtime.GOMAXPROCS(0))
	for _, repo := range repos {
		if !repo.IsMirror() {
			continue
		}
		// Go waits for a free slot, so the server may have begun to stop
		// in the meantime.
		if ctx.Err() != nil {
			break
		}
		name := repo.Name()
		g.Go(func() error {
			// The slot may have come free only after the server began
			// to stop.
			if ctx.Err() != nil {
				return nil
			}
			var err error
			if perr := safego.Run(logger.With("repo", name), func() { err = syncMirrorRepo(ctx, cfg, logger, b, name) }); perr != nil {
				err = perr
			}
			// A sync the server stopped did not fail, and one of a
			// repository that is no longer a mirror did not happen.
			if errors.Is(err, errNotMirror) || ctx.Err() != nil {
				return nil
			}
			if rerr := b.RecordMirrorSync(context.WithoutCancel(ctx), name, err); rerr != nil {
				logger.Warn("failed to record the mirror sync", "repo", name, "err", rerr)
			}
			return nil
		})
	}
	_ = g.Wait()
}

// errNotMirror is returned by syncMirrorRepo for a repository that is no
// longer a mirror.
var errNotMirror = errors.New("not a mirror")

// syncMirrorRepo syncs one pull mirror: its remotes, then its LFS objects.
// It logs what went wrong, and returns it so the job can record it: nil
// once every step succeeded or had nothing to do.
func syncMirrorRepo(jobCtx context.Context, cfg *config.Config, logger *log.Logger, b *backend.Backend, name string) (err error) {
	// One deadline for the whole sync: the job waits for every mirror, and
	// cron skips its runs while one is still going, so a mirror whose
	// remote or LFS endpoint keeps a transfer going for ever would stop
	// every mirror from syncing. The steps below stop quietly once ctx is
	// done, as at shutdown, so running out of time is logged here.
	ctx, cancel := context.WithTimeout(jobCtx, mirrorSyncTimeout)
	defer cancel()
	defer func() {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && jobCtx.Err() == nil {
			logger.Warn("mirror sync stopped, it ran out of time", "repo", name, "timeout", mirrorSyncTimeout)
			err = fmt.Errorf("the sync ran out of time after %v", mirrorSyncTimeout)
		}
	}()

	// The job lists the repositories once and works through them for as
	// long as the slowest takes, so the mirror flag is read again here.
	// Fetching with --prune a repository switched off since would delete
	// every branch pushed to it after the switch.
	repo, err := b.Repository(ctx, name)
	if err != nil {
		logger.Error("error reading repository", "repo", name, "err", err)
		return err
	}
	if !repo.IsMirror() {
		logger.Debug("skipping repository that is no longer a mirror", "repo", name)
		return errNotMirror
	}
	r, err := repo.Open()
	if err != nil {
		logger.Error("error opening repository", "repo", name, "err", err)
		return err
	}

	// Re-validate every configured remote before syncing. The sync fetches
	// all of them, and a remote may predate the import-time guard or have
	// been written out of band. Validation failure skips the repo entirely.
	// No user acts here, so the owner stands in for the importer. An owner
	// that cannot be looked up gets the policy of nobody, which refuses ssh
	// without an allowlist.
	owner, err := b.UserByID(ctx, repo.UserID())
	if err != nil {
		logger.Warn("mirror owner not found, treating as unprivileged", "repo", name, "err", err)
		owner = nil
	}
	policy := backend.RemotePolicy(cfg, owner)

	remotes, err := validateMirrorRemotes(ctx, r, policy)
	if err != nil {
		logger.Warn("skipping mirror sync, remote failed validation", "repo", name, "err", err)
		return err
	}

	// When a fetch fails the LFS sync is skipped too; the next run tries
	// both again.
	syncEnv := backend.RemoteGitEnv(cfg, remotes.validated...)
	if err := fetchMirror(ctx, logger, name, r.Path, remotes.names, syncEnv); err != nil {
		return err
	}

	if !cfg.LFS.Enabled {
		return nil
	}
	lfsEndpoint, _, err := r.WithContext(ctx).ConfigGet("lfs.url")
	if err != nil {
		logger.Error("error getting git config", "repo", name, "err", err)
		return err
	}
	if lfsEndpoint == "" {
		// If there is no LFS url defined, means the repo doesn't use LFS
		// and we can skip it.
		return nil
	}

	// The endpoint is stored in the repo config and may predate
	// validation, so check it before dialing.
	v, err := ssrf.ValidateGitRemoteContext(ctx, lfsEndpoint, policy)
	if err != nil {
		logger.Warn("skipping lfs sync, endpoint failed validation", "repo", name, "err", err)
		return fmt.Errorf("lfs endpoint: %w", err)
	}
	// An ssh or scp-style endpoint, which older imports stored, is
	// refused: objects are fetched over http(s) only.
	if v.Transport == ssrf.GitTransportSSH {
		logger.Warn("skipping lfs sync", "repo", name, "err", lfs.ErrUnsupportedEndpoint)
		return
	}

	ep, err := lfs.NewEndpoint(lfsEndpoint)
	if err != nil {
		logger.Warn("skipping lfs sync", "repo", name, "err", err)
		return err
	}

	if err := syncRepoLFS(ctx, b, repo, lfs.NewClient(ep)); err != nil {
		switch {
		case errors.Is(err, backend.ErrLFSObjectsUnavailable):
			// The next sync tries them again.
			logger.Warn("mirrored repository without some lfs objects", "repo", name, "err", err)
		case ctx.Err() != nil:
			// The server is stopping, or the sync ran out of time.
		default:
			logger.Error("failed to store missing lfs objects", "err", err, "path", r.Path)
		}
		return err
	}
	return nil
}

// syncRepoLFS stores the LFS objects a mirror sync fetched pointers to.
// Tests replace it.
var syncRepoLFS = func(ctx context.Context, b *backend.Backend, repo proto.Repository, client backend.LFSClient) error {
	return b.SyncRepoLFSObjects(ctx, repo, client)
}

// mirrorSyncTimeout bounds the whole sync of one mirror, its fetches and its
// LFS objects. Tests lower it.
//
// Stalls are caught much sooner by the low-speed and keepalive settings in
// RemoteGitEnv and by the LFS client's stall timeout; this is the backstop
// for what those miss, such as git:// remotes, which git dials itself with no
// read timeout, and remotes or LFS endpoints that trickle data just fast
// enough.
var mirrorSyncTimeout = time.Hour

// mirrorGitWaitDelay is how long git has to exit once it was asked to stop,
// and then how long the sync waits for the helpers git started
// (git-remote-http, ssh) to let go of its output. Past the first, git is
// killed; past the second, the output is closed and the sync moves on.
const mirrorGitWaitDelay = 5 * time.Second

// mirrorGitStderrLimit caps how much of git's stderr is kept for the error.
// The remote can write to it (remote: lines), so it is not kept in full.
const mirrorGitStderrLimit = 4 << 10

// runMirrorGit runs one git command of a mirror sync in dir with env applied.
// Tests replace it.
//
// Once ctx is done, at shutdown or when the sync runs out of time, git is
// sent SIGTERM (see terminateOnCancel), which it handles by removing the ref
// and packed-refs locks it holds and exiting. A killed git would leave them,
// and every later sync of the mirror would fail on "File exists" until
// someone deleted them by hand. git is killed only if it is still running
// mirrorGitWaitDelay later.
var runMirrorGit = func(ctx context.Context, dir string, env []string, args ...string) error {
	var stderr limitedBuffer
	stderr.limit = mirrorGitStderrLimit

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = &stderr
	terminateOnCancel(cmd)
	cmd.WaitDelay = mirrorGitWaitDelay

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = fmt.Errorf("%w: %w", ctxErr, err)
		}
		if msg := redactURLs(strings.TrimSpace(stderr.String())); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// urlPattern matches a URL as git and remotes print it in messages: a scheme
// and everything up to the next space or quote.
var urlPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s'"<>]+`)

// redactURLs strips the user info and query from every URL in msg. git leaves
// out the user info of the remote URLs it prints but keeps their query, which
// can hold a token, and the remote's own lines can hold any URL.
func redactURLs(msg string) string {
	return urlPattern.ReplaceAllStringFunc(msg, utils.RedactURL)
}

// limitedBuffer keeps the first limit bytes written to it and discards the
// rest, reporting every write as complete so the writer is not cut off.
//
// The buffer is a named field rather than embedded so that limitedBuffer has
// no ReadFrom: os/exec copies a pipe into a non-file Stderr with io.Copy,
// which would call an embedded bytes.Buffer.ReadFrom and bypass the cap.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

// Write implements io.Writer.
func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// String returns the bytes kept so far.
func (b *limitedBuffer) String() string {
	return b.buf.String()
}

// fetchMirror fetches the named remotes of the mirror in dir. It returns nil
// when the sync should go on to the lfs objects, and otherwise the error,
// after logging a failed fetch, so the lfs sync is skipped until the next
// run. Once ctx is done it starts no further fetch and returns ctx's error
// without logging: the server is stopping, or the sync ran out of time.
func fetchMirror(ctx context.Context, logger *log.Logger, name, dir string, remotes []string, env []string) error {
	err := syncMirror(ctx, dir, remotes, env)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		logger.Error("error syncing mirror", "repo", name, "err", err)
	}
	return err
}
