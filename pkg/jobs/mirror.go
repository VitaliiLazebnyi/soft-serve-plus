package jobs

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/sync"
	"github.com/charmbracelet/soft-serve/pkg/utils"
)

func init() {
	Register("mirror-pull", mirrorPull{})
}

type mirrorPull struct{}

// validateMirrorRemotes validates every remote configured on a mirror
// repository against address ranges and the operator's ssh host policy, and
// returns them so the caller can render the git environment that must be
// applied to the sync commands.
//
// `git remote update` fetches from all remotes, not just origin, so every one
// of them has to pass. Any failure to read or validate skips the sync: a
// remote that cannot be checked is not one to fetch from.
func validateMirrorRemotes(r *git.Repository, policy ssrf.GitRemotePolicy) ([]ssrf.ValidatedGitRemote, error) {
	cfg, err := r.Config()
	if err != nil {
		return nil, fmt.Errorf("reading git config: %w", err)
	}

	var remotes []ssrf.ValidatedGitRemote
	for _, sub := range cfg.Section("remote").Subsections {
		url := sub.Option("url")
		if url == "" {
			continue
		}

		v, err := ssrf.ValidateGitRemote(url, policy)
		if err != nil {
			return nil, fmt.Errorf("remote %q: %w", sub.Name, err)
		}
		remotes = append(remotes, v)
	}

	if len(remotes) == 0 {
		return nil, fmt.Errorf("no remote url configured")
	}

	return remotes, nil
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
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)
	return func() {
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

		// Divide the work up among the number of CPUs.
		wq := sync.NewWorkPool(ctx, runtime.GOMAXPROCS(0),
			sync.WithWorkPoolLogger(func(format string, args ...any) {
				// The pool only fails to start work once ctx is done, which
				// is an ordinary shutdown, not an error.
				if ctx.Err() == nil {
					logger.Errorf(format, args...)
				}
			}),
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
					repo := repo

					// Re-validate every configured remote before syncing.
					// `remote update` touches all of them, and a remote may
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

					remotes, err := validateMirrorRemotes(r, policy)
					if err != nil {
						logger.Warn("skipping mirror sync, remote failed validation", "repo", name, "err", err)
						return
					}

					syncEnv := backend.RemoteGitEnv(cfg, remotes...)

					if !fetchMirror(ctx, logger, name, r.Path, syncEnv) {
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
						if _, err := ssrf.ValidateGitRemote(lfsEndpoint, policy); err != nil {
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

						if err := backend.SyncRepoLFSObjects(ctx, repo, dbx, datastore, client); err != nil {
							if errors.Is(err, backend.ErrLFSObjectsUnavailable) {
								// The next sync tries them again.
								logger.Warn("mirrored repository without some lfs objects", "repo", name, "err", err)
								return
							}
							if ctx.Err() != nil {
								// The server is stopping.
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

// mirrorSyncCmds are the git commands a mirror sync runs, in order.
var mirrorSyncCmds = [][]string{
	{"fetch", "--prune"},            // fetch prune before updating remote
	{"remote", "update", "--prune"}, // update remote and prune remote refs
}

// runMirrorGit runs one mirror sync git command in dir. Tests replace it.
//
// The command does not get ctx's cancellation. Cancelling would kill git
// (exec.CommandContext sends SIGKILL, or TerminateProcess on Windows), and a
// killed git never removes the ref and packed-refs locks it holds, so every
// later sync of the mirror would fail on "File exists" until someone deletes
// them by hand. A git command that is already running is left to finish:
// Server.Shutdown waits for it within its drain deadline, and if the server
// exits first, git finishes on its own and cleans up after itself, as it
// did before syncs were cancelled at all.
var runMirrorGit = func(ctx context.Context, dir string, env []string, args ...string) error {
	cmd := git.NewCommand(args...).WithContext(context.WithoutCancel(ctx)).WithTimeout(-1)
	cmd.AddEnvs(env...)
	_, err := cmd.RunInDir(dir)
	return err
}

// fetchMirror runs the mirror sync git commands in dir and reports whether
// the sync should go on to the lfs objects. Once ctx is done it starts no
// further command and returns false without logging: the server is
// stopping.
func fetchMirror(ctx context.Context, logger *log.Logger, name, dir string, env []string) bool {
	for _, args := range mirrorSyncCmds {
		if ctx.Err() != nil {
			return false
		}
		if err := runMirrorGit(ctx, dir, env, args...); err != nil {
			if ctx.Err() != nil {
				return false
			}
			logger.Error("error running git remote update", "repo", name, "err", err)
		}
	}
	return ctx.Err() == nil
}
