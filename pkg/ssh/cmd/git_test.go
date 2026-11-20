package cmd

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/matryer/is"
	"github.com/prometheus/client_golang/prometheus"
)

// gitTestTimeout bounds the waits in these tests, so a handler that never
// returns fails the test instead of hanging it.
const gitTestTimeout = time.Minute

// cancelledPushToCreate runs a push to the missing repository "fresh" as an
// admin, which creates it, and cancels the session once receive-pack has sent
// its advertisement, as a client going away does. beforeCancel runs with the
// backend and the repository's path just before the cancellation. It returns the backend and
// a context to look the repository up with.
func cancelledPushToCreate(t *testing.T, beforeCancel func(ctx context.Context, be *backend.Backend, dir string)) (context.Context, *backend.Backend) {
	t.Helper()
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	sessCtx, cancel := context.WithCancel(adminCtx)
	defer cancel()

	// Stdin stays open and empty, so receive-pack waits for the client after
	// the advertisement until it is killed.
	stdin, stdinW := io.Pipe()
	defer stdinW.Close() //nolint: errcheck
	stdout, stdoutW := io.Pipe()

	c := GitReceivePackCommand()
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.SetIn(stdin)
	c.SetOut(stdoutW)
	c.SetErr(io.Discard)
	c.SetArgs([]string{"fresh"})

	errc := make(chan error, 1)
	go func() {
		errc <- c.ExecuteContext(sessCtx)
		stdoutW.Close() //nolint: errcheck
	}()

	// The repository exists once the advertisement starts.
	lc := make(chan error, 1)
	go func() {
		_, err := bufio.NewReader(stdout).ReadString('\n')
		lc <- err
	}()
	select {
	case err := <-lc:
		is.NoErr(err)
	case <-time.After(gitTestTimeout):
		t.Fatal("timed out reading the advertisement")
	}
	_, err := be.Repository(ctx, "fresh")
	is.NoErr(err)

	if beforeCancel != nil {
		beforeCancel(ctx, be, filepath.Join(config.FromContext(ctx).DataPath, "repos", "fresh.git"))
	}

	cancel()
	go io.Copy(io.Discard, stdout) //nolint: errcheck

	select {
	case err = <-errc:
	case <-time.After(gitTestTimeout):
		t.Fatal("timed out waiting for receive-pack to return")
	}
	is.True(errors.Is(err, context.Canceled))

	return ctx, be
}

// A push that creates a repository and is then cancelled, because the client
// went away, must not leave the new repository behind.
func TestCancelledPushRemovesCreatedRepository(t *testing.T) {
	is := is.New(t)
	var dir string
	ctx, be := cancelledPushToCreate(t, func(_ context.Context, _ *backend.Backend, d string) { dir = d })

	_, err := be.Repository(ctx, "fresh")
	is.True(err != nil)      // the created repository was removed
	is.True(!dirExists(dir)) // and so was its directory
}

// receive-pack updates the refs and reports success to the client before the
// post-receive hooks run. A push cancelled after that, while the hooks run,
// must keep the repository it created: the client was told the push
// succeeded. Its HEAD must point at the pushed branch, as after a push that
// finished, not at a branch that does not exist.
func TestCancelledPushKeepsCreatedRepositoryWithRefs(t *testing.T) {
	is := is.New(t)
	var repoDir string
	ctx, be := cancelledPushToCreate(t, func(_ context.Context, _ *backend.Backend, dir string) {
		repoDir = dir
		git := func(stdin string, args ...string) string {
			cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
			cmd.Stdin = strings.NewReader(stdin)
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
				"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			)
			out, err := cmd.Output()
			is.NoErr(err)
			return strings.TrimSpace(string(out))
		}
		tree := git("", "mktree")
		commit := git("", "commit-tree", tree, "-m", "pushed")
		// Neither main nor master, so HEAD cannot point at it already.
		git("", "update-ref", "refs/heads/trunk", commit)
	})

	_, err := be.Repository(ctx, "fresh")
	is.NoErr(err) // the repository with pushed refs was kept

	out, err := exec.CommandContext(t.Context(), "git", "-C", repoDir, "symbolic-ref", "HEAD").Output()
	is.NoErr(err)
	is.Equal(strings.TrimSpace(string(out)), "refs/heads/trunk") // HEAD points at the pushed branch
}

// No git service adds a metric series for the repository it serves. A
// caller who may create repositories can push to any new name, and a push
// that fails removes the repository it created again, so per-name series
// would grow without limit; so would series of repositories deleted or
// renamed since. Here a push creates a repository and fails, and a refused
// push asks for another name.
func TestPushAddsNoRepositorySeries(t *testing.T) {
	is := is.New(t)
	cancelledPushToCreate(t, nil)

	ctx, _ := newAuthTestContext(t)
	const refused = "refused-push-metric-label"
	c := GitReceivePackCommand()
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.SetIn(strings.NewReader(""))
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	c.SetArgs([]string{refused})
	is.True(c.ExecuteContext(ctx) != nil) // anonymous callers may not push

	families, err := prometheus.DefaultGatherer.Gather()
	is.NoErr(err)
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if v := l.GetValue(); v == "fresh" || v == refused {
					t.Errorf("%s has a series labelled %s=%q", f.GetName(), l.GetName(), v)
				}
			}
		}
	}
}

// A push that created a repository and failed must not remove it while
// another push to it runs: that push may write refs after the removal
// checked for them, and they would be lost with the directory.
func TestFailedPushKeepsCreatedRepositoryWhilePushRuns(t *testing.T) {
	is := is.New(t)
	var endPush func()
	ctx, be := cancelledPushToCreate(t, func(ctx context.Context, be *backend.Backend, _ string) {
		repo, err := be.Repository(ctx, "fresh")
		is.NoErr(err)
		// Another session's push to the new repository starts.
		endPush, err = be.BeginPush(ctx, repo)
		is.NoErr(err)
	})
	defer endPush()

	_, err := be.Repository(ctx, "fresh")
	is.NoErr(err) // kept: the other push is running
}

func dirExists(dir string) bool {
	_, err := os.Lstat(dir)
	return err == nil
}
