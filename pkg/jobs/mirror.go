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
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/sync"
	"github.com/charmbracelet/soft-serve/pkg/utils"
)

func init() {
	Register("mirror-pull", mirrorPull{})
}

type mirrorPull struct{}

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

// syncMirror fetches each named remote of the mirror at path, pruning refs
// the remote no longer has. Naming every remote keeps git from choosing one
// that was not validated. A remote that fails to fetch does not stop the
// others; their errors are joined.
func syncMirror(ctx context.Context, path string, names []string, env []string) error {
	var errs []error
	for _, name := range names {
		if err := runMirrorGit(ctx, path, env, "fetch", "--prune", "--", name); err != nil {
			errs = append(errs, fmt.Errorf("fetching remote %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// Spec derives the spec used for pull mirrors and implements Runner.
func (m mirrorPull) Spec(ctx context.Context) string {
	cfg := config.FromContext(ctx)
	if cfg.Jobs.MirrorPull != "" {
		return cfg.Jobs.MirrorPull
	}
	return "@every 10m"
}

// Func runs the (pull) mirror job task and implements Runner.
func (m mirrorPull) Func(ctx context.Context) func() {
	cfg := config.FromContext(ctx)
	logger := log.FromContext(ctx).WithPrefix("jobs.mirror")
	b := backend.FromContext(ctx)
	return func() {
		// The same guards that protect imports protect syncs, and the same
		// too-old git ignores them.
		if reason := b.ImportsDisabledReason(); reason != "" {
			logger.Warn("skipping mirror sync", "reason", reason)
			return
		}

		repos, err := b.Repositories(ctx)
		if err != nil {
			logger.Error("error getting repositories", "err", err)
			return
		}

		// Divide the work up among the number of CPUs.
		wq := sync.NewWorkPool(ctx, runtime.GOMAXPROCS(0),
			sync.WithWorkPoolLogger(logger.Errorf),
		)

		logger.Debug("updating mirror repos")
		for _, repo := range repos {
			if repo.IsMirror() {
				r, err := repo.Open()
				if err != nil {
					logger.Error("error opening repository", "repo", repo.Name(), "err", err)
					continue
				}

				name := repo.Name()
				wq.Add(name, func() {
					// Re-validate every configured remote before syncing.
					// The sync fetches all of them, and a remote may
					// predate the import-time guard or have been written out
					// of band. Validation failure skips the repo entirely.
					// No user acts here, so the owner stands in for the
					// importer. An owner that cannot be looked up gets the
					// policy of nobody, which refuses ssh without an
					// allowlist.
					owner, err := b.UserByID(ctx, repo.UserID())
					if err != nil {
						logger.Warn("mirror owner not found, treating as unprivileged", "repo", name, "err", err)
						owner = nil
					}
					policy := backend.RemotePolicy(cfg, owner)

					remotes, err := validateMirrorRemotes(ctx, r, policy)
					if err != nil {
						logger.Warn("skipping mirror sync, remote failed validation", "repo", name, "err", err)
						return
					}

					// When a fetch fails the LFS sync is skipped too; the
					// next run tries both again.
					syncEnv := backend.RemoteGitEnv(cfg, remotes.validated...)
					if err := syncMirror(ctx, r.Path, remotes.names, syncEnv); err != nil {
						logger.Error("error syncing mirror", "repo", name, "err", err)
						return
					}

					if cfg.LFS.Enabled {
						rcfg, err := r.Config()
						if err != nil {
							logger.Error("error getting git config", "repo", name, "err", err)
							return
						}

						lfsEndpoint := rcfg.Section("lfs").Option("url")
						if lfsEndpoint == "" {
							// If there is no LFS url defined, means the repo
							// doesn't use LFS and we can skip it.
							return
						}

						// The endpoint is stored in the repo config and may
						// predate validation, so check it before dialing.
						if _, err := ssrf.ValidateGitRemoteContext(ctx, lfsEndpoint, policy); err != nil {
							logger.Warn("skipping lfs sync, endpoint failed validation", "repo", name, "err", err)
							return
						}

						ep, err := lfs.NewEndpoint(lfsEndpoint)
						if err != nil {
							logger.Error("error creating LFS endpoint", "repo", name, "err", err)
							return
						}

						client := lfs.NewClient(ep)
						if client == nil {
							logger.Errorf("failed to create lfs client: unsupported endpoint %s", utils.RedactURL(lfsEndpoint))
							return
						}

						if err := b.SyncRepoLFSObjects(ctx, repo, client); err != nil {
							if errors.Is(err, backend.ErrLFSObjectsUnavailable) {
								// The next sync tries them again.
								logger.Warn("mirrored repository without some lfs objects", "repo", name, "err", err)
								return
							}
							logger.Error("failed to store missing lfs objects", "err", err, "path", r.Path)
							return
						}
					}
				})
			}
		}

		wq.Run()
	}
}

// mirrorGitTimeout bounds a single git command of a mirror sync.
//
// The job waits for every repository before it finishes, and the scheduler
// skips its runs while one is still going, so a command that never returns
// stops every mirror on the server from syncing. Stalls are caught much
// sooner by the low-speed and keepalive settings in RemoteGitEnv; this is the
// backstop for what those miss, such as git:// remotes, which git dials itself
// with no read timeout, and remotes that trickle data just fast enough.
var mirrorGitTimeout = time.Hour

// mirrorGitWaitDelay is how long, once git is killed, the sync waits for the
// helpers git started (git-remote-http, ssh) to let go of its output.
// They inherit git's stderr and outlive it when killed; past this the output
// is closed and the sync moves on.
const mirrorGitWaitDelay = 5 * time.Second

// mirrorGitStderrLimit caps how much of git's stderr is kept for the error.
// The remote can write to it (remote: lines), so it is not kept in full.
const mirrorGitStderrLimit = 4 << 10

// runMirrorGit runs one git command of a mirror sync in dir with env applied,
// killing it after mirrorGitTimeout or when ctx is done.
func runMirrorGit(ctx context.Context, dir string, env []string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, mirrorGitTimeout)
	defer cancel()

	var stderr limitedBuffer
	stderr.limit = mirrorGitStderrLimit

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = &stderr
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
