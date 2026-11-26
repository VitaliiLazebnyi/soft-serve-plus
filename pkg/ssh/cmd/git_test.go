package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/log/v2"
	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/gitserve"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spf13/cobra"
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
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	sessCtx, cancel := context.WithCancel(adminCtx)
	defer cancel()

	// Stdin stays open and empty, so receive-pack waits for the client after
	// the advertisement until it is killed.
	stdin, stdinW := io.Pipe()
	defer stdinW.Close() //nolint: errcheck
	stdout, stdoutW := io.Pipe()

	c := cmdsOf(sessCtx).GitReceivePackCommand()
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
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(gitTestTimeout):
		t.Fatal("timed out reading the advertisement")
	}
	_, err := be.Repository(ctx, "fresh")
	if err != nil {
		t.Fatal(err)
	}

	if beforeCancel != nil {
		beforeCancel(ctx, be, filepath.Join(cmdsOf(ctx).cfg.DataPath, "repos", "fresh.git"))
	}

	cancel()
	go io.Copy(io.Discard, stdout) //nolint: errcheck

	select {
	case err = <-errc:
	case <-time.After(gitTestTimeout):
		t.Fatal("timed out waiting for receive-pack to return")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("not true: errors.Is(err, context.Canceled)")
	}

	return ctx, be
}

// A push that creates a repository and is then cancelled, because the client
// went away, must not leave the new repository behind.
func TestCancelledPushRemovesCreatedRepository(t *testing.T) {
	t.Parallel()
	var dir string
	ctx, be := cancelledPushToCreate(t, func(_ context.Context, _ *backend.Backend, d string) { dir = d })

	_, err := be.Repository(ctx, "fresh")
	if err == nil {
		t.Fatal("not true: err != nil")
	} // the created repository was removed
	if dirExists(dir) {
		t.Fatal("not true: !dirExists(dir)")
	} // and so was its directory
}

// receive-pack updates the refs and reports success to the client before the
// post-receive hooks run. A push cancelled after that, while the hooks run,
// must keep the repository it created: the client was told the push
// succeeded. Its HEAD must point at the pushed branch, as after a push that
// finished, not at a branch that does not exist.
func TestCancelledPushKeepsCreatedRepositoryWithRefs(t *testing.T) {
	t.Parallel()
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
			if err != nil {
				t.Fatal(err)
			}
			return strings.TrimSpace(string(out))
		}
		tree := git("", "mktree")
		commit := git("", "commit-tree", tree, "-m", "pushed")
		// Neither main nor master, so HEAD cannot point at it already.
		git("", "update-ref", "refs/heads/trunk", commit)
	})

	_, err := be.Repository(ctx, "fresh")
	if err != nil {
		t.Fatal(err)
	} // the repository with pushed refs was kept

	out, err := exec.CommandContext(t.Context(), "git", "-C", repoDir, "symbolic-ref", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "refs/heads/trunk" {
		t.Fatalf("strings.TrimSpace(string(out)) = %v, want %v", got, "refs/heads/trunk")
	} // HEAD points at the pushed branch
}

// No git service adds a metric series for the repository it serves. A
// caller who may create repositories can push to any new name, and a push
// that fails removes the repository it created again, so per-name series
// would grow without limit; so would series of repositories deleted or
// renamed since. Here a push creates a repository and fails, and a refused
// push asks for another name.
func TestPushAddsNoRepositorySeries(t *testing.T) {
	t.Parallel()
	cancelledPushToCreate(t, nil)

	ctx, _ := newAuthTestContext(t)
	const refused = "refused-push-metric-label"
	c := cmdsOf(ctx).GitReceivePackCommand()
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.SetIn(strings.NewReader(""))
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	c.SetArgs([]string{refused})
	if c.ExecuteContext(ctx) == nil {
		t.Fatal("not true: c.ExecuteContext(ctx) != nil")
	} // anonymous callers may not push

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
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
	t.Parallel()
	var endPush func()
	ctx, be := cancelledPushToCreate(t, func(ctx context.Context, be *backend.Backend, _ string) {
		repo, err := be.Repository(ctx, "fresh")
		if err != nil {
			t.Fatal(err)
		}
		// Another session's push to the new repository starts.
		endPush, err = be.BeginPush(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
	})
	defer endPush()

	_, err := be.Repository(ctx, "fresh")
	if err != nil {
		t.Fatal(err)
	} // kept: the other push is running
}

func dirExists(dir string) bool {
	_, err := os.Lstat(dir)
	return err == nil
}

// A git service denies a repository it cannot serve the way the repo
// commands do: a key in initial_admin_keys that no account holds is a
// server admin, not an anonymous caller, so a missing repository is
// "invalid repo" for it rather than "not authorized".
func TestGitDenyReadAdminKeyIsNotAnonymous(t *testing.T) {
	pub, ak := slashFreeKey(t)
	ctx, _ := newAuthTestContextWith(t, func(c *config.Config) {
		c.InitialAdminKeys = []string{ak}
	})

	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"anonymous", ctx, gitserve.ErrNotAuthed},
		{"admin key", context.WithValue(ctx, ssh.ContextKeyPublicKey, pub), gitserve.ErrInvalidRepo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if proto.UserFromContext(tc.ctx) != nil {
				t.Fatal("not true: proto.UserFromContext(tc.ctx) == nil")
			}
			c := cmdsOf(tc.ctx).GitUploadPackCommand()
			c.SetIn(&bytes.Buffer{})
			c.SetOut(&bytes.Buffer{})
			c.SetErr(&bytes.Buffer{})
			c.SetArgs([]string{"ghost"})
			err := c.ExecuteContext(tc.ctx)
			if !errors.Is(err, tc.want) {
				t.Fatal("not true: errors.Is(err, tc.want)")
			}
		})
	}
}

// runLFSCommand runs an LFS command with stdin, writing its output to
// stdout, and returns what it logged and its error.
func runLFSCommand(ctx context.Context, newCmd func(*Commands) *cobra.Command, stdin io.Reader, stdout io.Writer, args ...string) (string, error) {
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)
	ctx = withTestLogger(ctx, logger)

	c := newCmd(cmdsOf(ctx))
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.SetIn(stdin)
	c.SetOut(stdout)
	c.SetErr(io.Discard)
	c.SetArgs(args)
	err := c.ExecuteContext(ctx)
	return logs.String(), err
}

// A caller without an account that may read a repository, such as an
// unregistered key on a public repository with anonymous read access, gets
// an answer that says git-lfs-authenticate needs an account, not "something
// went wrong" with an error logged for it. A caller that may not read the
// repository, or that wants to upload, still gets "not authorized".
func TestSSHLFSAuthenticateWithoutAccount(t *testing.T) {
	ctx, be := newAuthTestContext(t)
	owner := proto.UserFromContext(withUser(t, ctx, be, "boss", true))
	_, err := be.CreateRepository(ctx, "pub", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "priv", owner, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		repo, op string
		want     error
	}{
		{"pub", "download", errLFSAuthNoAccount},
		{"pub", "upload", gitserve.ErrNotAuthed},
		{"priv", "download", gitserve.ErrNotAuthed},
		{"ghost", "download", gitserve.ErrNotAuthed},
	} {
		t.Run(tc.repo+" "+tc.op, func(t *testing.T) {
			var out bytes.Buffer
			logs, err := runLFSCommand(ctx, (*Commands).GitLFSAuthenticateCommand, strings.NewReader(""), &out, tc.repo, tc.op)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("issued output %q", out.String())
			}
			if strings.Contains(logs, "ERRO") {
				t.Errorf("logged an error:\n%s", logs)
			}
		})
	}
}

// errWriter is a client that went away: every write fails as a write to a
// closed SSH channel does.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, io.EOF }

// An LFS transfer whose client goes away is not a server failure, so it is
// reported as cancelled and logged at Debug, as a git service is.
func TestSSHLFSTransferClientGone(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	_, err := be.CreateRepository(ctx, "shared", proto.UserFromContext(adminCtx), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	logs, err := runLFSCommand(adminCtx, (*Commands).GitLFSTransferCommand, strings.NewReader(""), errWriter{}, "shared", "download")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if strings.Contains(logs, "ERRO") {
		t.Errorf("logged an error:\n%s", logs)
	}
}

// Each push over SSH is counted once, and a push that is refused is not
// counted at all.
func TestReceivePackCountedOnce(t *testing.T) {
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	readerCtx := withUser(t, ctx, be, "reader", false)
	const name = "receive-pack-counted"
	_, err := be.CreateRepository(ctx, name, proto.UserFromContext(adminCtx), proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}

	// The repository's hooks run the server binary, which is the test
	// binary here; point git at no hooks instead.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", t.TempDir())

	// A commit to push, and its pack.
	work := t.TempDir()
	gitCmd := func(stdin string, args ...string) []byte {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", work}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	gitCmd("", "init", "-q", "--bare")
	tree := strings.TrimSpace(string(gitCmd("", "mktree")))
	commit := strings.TrimSpace(string(gitCmd("", "commit-tree", tree, "-m", "pushed")))
	pack := string(gitCmd(commit+"\n", "pack-objects", "--stdout", "--revs"))

	// push creates branch with the commit.
	push := func(ctx context.Context, branch string) error {
		line := fmt.Sprintf("%s %s refs/heads/%s\x00report-status\n", strings.Repeat("0", 40), commit, branch)
		c := cmdsOf(ctx).GitReceivePackCommand()
		c.SilenceUsage = true
		c.SilenceErrors = true
		c.SetIn(strings.NewReader(fmt.Sprintf("%04x%s0000", len(line)+4, line) + pack))
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		c.SetArgs([]string{name})
		return c.ExecuteContext(ctx)
	}

	before := testutil.ToFloat64(receivePackCounter)
	if err := push(adminCtx, "main"); err != nil {
		t.Fatal(err)
	}
	if err := push(adminCtx, "other"); err != nil {
		t.Fatal(err)
	}
	if push(readerCtx, "denied") == nil {
		t.Fatal("not true: push(readerCtx, \"denied\") != nil")
	} // a user who may not write is refused

	if got := testutil.ToFloat64(receivePackCounter) - before; got != 2.0 {
		t.Fatalf("testutil.ToFloat64(receivePackCounter)-before = %v, want %v", got, 2.0)
	}
}

// A push over SSH counts as running for the removal of a repository an
// HTTP push created and left without refs, so that removal cannot delete
// the repository while the SSH push may write refs to it.
func TestSSHPushHoldsOffPushCreatedRemoval(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	user := proto.UserFromContext(adminCtx)
	repo, err := be.CreateRepository(ctx, "shared", user, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Stdin stays open and empty, so receive-pack waits for the client
	// after the advertisement.
	stdin, stdinW := io.Pipe()
	stdout, stdoutW := io.Pipe()

	c := cmdsOf(adminCtx).GitReceivePackCommand()
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.SetIn(stdin)
	c.SetOut(stdoutW)
	c.SetErr(io.Discard)
	c.SetArgs([]string{"shared"})

	errc := make(chan error, 1)
	go func() {
		errc <- c.ExecuteContext(adminCtx)
		stdoutW.Close() //nolint: errcheck
	}()
	_, err = bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, stdout) //nolint: errcheck

	removed, err := be.RemovePushCreatedRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("not true: !removed")
	} // the SSH push is running

	// The client sends no commands, and the push ends.
	_, err = stdinW.Write([]byte("0000"))
	if err != nil {
		t.Fatal(err)
	}
	if err := stdinW.Close(); err != nil {
		t.Fatal(err)
	}
	<-errc //nolint:errcheck // only that the command returned matters here

	removed, err = be.RemovePushCreatedRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("not true: removed")
	}
}

// An LFS upload over SSH counts as running for that removal too: removing
// the repository under it would fail the upload and leave its temporary
// file behind.
func TestSSHLFSUploadHoldsOffPushCreatedRemoval(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	user := proto.UserFromContext(adminCtx)
	repo, err := be.CreateRepository(ctx, "shared", user, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	stdin, stdinW := io.Pipe()
	stdout, stdoutW := io.Pipe()

	c := cmdsOf(adminCtx).GitLFSTransferCommand()
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.SetIn(stdin)
	c.SetOut(stdoutW)
	c.SetErr(io.Discard)
	c.SetArgs([]string{"shared", "upload"})

	errc := make(chan error, 1)
	go func() {
		errc <- c.ExecuteContext(adminCtx)
		stdoutW.Close() //nolint: errcheck
	}()
	// The transfer has started once it advertises its capabilities.
	_, err = bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, stdout) //nolint: errcheck

	removed, err := be.RemovePushCreatedRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("not true: !removed")
	} // the LFS upload is running

	if err := stdinW.Close(); err != nil {
		t.Fatal(err)
	}
	<-errc //nolint:errcheck // only that the command returned matters here

	removed, err = be.RemovePushCreatedRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("not true: removed")
	}
}

// A push that finds the repository it was going to push to removed, as an
// unused HTTP push-created repository, creates it anew, as a push to a
// missing name does, rather than failing.
func TestSSHPushAfterPushCreatedRemovalCreates(t *testing.T) {
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	user := proto.UserFromContext(adminCtx)
	repo, err := be.CreateRepository(ctx, "shared", user, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// The removal runs after this push looked the repository up and
	// before it starts. The hook runs again for the push that creates
	// the repository anew; only the first run removes.
	cmds := cmdsOf(ctx)
	cmds.beforeBeginPush = func() {
		cmds.beforeBeginPush = nil
		removed, err := be.RemovePushCreatedRepository(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		if !removed {
			t.Fatal("not true: removed")
		}
	}

	// The repository's hooks run the server binary, which is the test
	// binary here; point git at no hooks instead.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", t.TempDir())

	// A commit to push, and its pack.
	work := t.TempDir()
	gitCmd := func(stdin string, args ...string) []byte {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", work}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	gitCmd("", "init", "-q", "--bare")
	tree := strings.TrimSpace(string(gitCmd("", "mktree")))
	commit := strings.TrimSpace(string(gitCmd("", "commit-tree", tree, "-m", "pushed")))
	pack := gitCmd(commit+"\n", "pack-objects", "--stdout", "--revs")
	line := fmt.Sprintf("%s %s refs/heads/main\x00report-status\n", strings.Repeat("0", 40), commit)
	req := fmt.Sprintf("%04x%s0000", len(line)+4, line) + string(pack)

	var out bytes.Buffer
	c := cmdsOf(adminCtx).GitReceivePackCommand()
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.SetIn(strings.NewReader(req))
	c.SetOut(&out)
	c.SetErr(io.Discard)
	c.SetArgs([]string{"shared"})
	if err := c.ExecuteContext(adminCtx); err != nil {
		t.Fatal(err)
	}

	cur, err := be.Repository(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	} // created anew
	if cur.ID() == repo.ID() {
		t.Fatal("not true: cur.ID() != repo.ID()")
	}
	r, err := cur.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	head, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}
	if got := head.ID(); got != commit {
		t.Fatalf("head.ID() = %v, want %v", got, commit)
	} // with the pushed commit
}

// A push to a missing name creates the repository and then pushes to its
// directory, found by name. If an admin deletes the new repository before
// git starts and someone else creates another under the same name, the push
// must not write to that one, which it was never checked against.
func TestSSHPushToCreateDoesNotWriteReplacedRepository(t *testing.T) {
	ctx, be := newAuthTestContext(t)
	pusherCtx := withUser(t, ctx, be, "boss", true)
	otherCtx := withUser(t, ctx, be, "other", true)

	var replacement proto.Repository
	cmds := cmdsOf(ctx)
	cmds.beforeBeginPush = func() {
		cmds.beforeBeginPush = nil
		if err := be.DeleteRepository(ctx, "fresh"); err != nil {
			t.Fatal(err)
		}
		var err error
		replacement, err = be.CreateRepository(ctx, "fresh", proto.UserFromContext(otherCtx), proto.RepositoryOptions{Private: true})
		if err != nil {
			t.Fatal(err)
		}
	}

	// The repository's hooks run the server binary, which is the test
	// binary here; point git at no hooks instead.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", t.TempDir())

	// A commit to push, and its pack.
	work := t.TempDir()
	gitCmd := func(stdin string, args ...string) []byte {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", work}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	gitCmd("", "init", "-q", "--bare")
	tree := strings.TrimSpace(string(gitCmd("", "mktree")))
	commit := strings.TrimSpace(string(gitCmd("", "commit-tree", tree, "-m", "pushed")))
	pack := gitCmd(commit+"\n", "pack-objects", "--stdout", "--revs")
	line := fmt.Sprintf("%s %s refs/heads/main\x00report-status\n", strings.Repeat("0", 40), commit)
	req := fmt.Sprintf("%04x%s0000", len(line)+4, line) + string(pack)

	var out bytes.Buffer
	c := cmdsOf(pusherCtx).GitReceivePackCommand()
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.SetIn(strings.NewReader(req))
	c.SetOut(&out)
	c.SetErr(io.Discard)
	c.SetArgs([]string{"fresh"})
	err := c.ExecuteContext(pusherCtx)
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	} // the repository it created is gone
	if replacement == nil {
		t.Fatal("not true: replacement != nil")
	} // the hook ran

	cur, err := be.Repository(ctx, "fresh")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cur.ID(), replacement.ID(); got != want {
		t.Fatalf("cur.ID() = %v, want %v", got, want)
	} // the other repository is still there
	r, err := cur.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.HEAD(); err == nil {
		t.Fatal("the push wrote to the repository that replaced the one it created")
	}
	if strings.Contains(out.String(), "refs/heads/main") {
		t.Fatal("not true: !strings.Contains(out.String(), \"refs/heads/main\")")
	} // and git never ran on it
}

// beginPushFailStore fails GetRepoByName, which BeginPush reads the
// repository with, once armed.
type beginPushFailStore struct {
	store.Store
	armed *atomic.Bool
}

var errInjectedBeginPush = errors.New("injected repos read failure: /server/path")

func (s beginPushFailStore) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	if s.armed.Load() {
		return models.Repo{}, errInjectedBeginPush
	}
	return s.Store.GetRepoByName(ctx, h, name)
}

// A push or LFS upload whose BeginPush cannot read the repository fails on
// the server: the database error is logged once, and the client gets
// gitserve.ErrSystemMalfunction, not the error.
func TestBeginPushLookupFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		exists bool
		cmd    func(*Commands) *cobra.Command
		args   []string
	}{
		{"push", true, (*Commands).GitReceivePackCommand, []string{"shared"}},
		{"push to create", false, (*Commands).GitReceivePackCommand, []string{"shared"}},
		{"lfs upload", true, (*Commands).GitLFSTransferCommand, []string{"shared", "upload"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			armed := new(atomic.Bool)
			ctx, be, logs := newLookupTestContextWith(t, func(s store.Store) store.Store {
				return beginPushFailStore{Store: s, armed: armed}
			})
			adminCtx := withUser(t, ctx, be, "boss", true)
			if tc.exists {
				_, err := be.CreateRepository(ctx, "shared", proto.UserFromContext(adminCtx), proto.RepositoryOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}
			cmdsOf(adminCtx).beforeBeginPush = func() { armed.Store(true) }

			c := tc.cmd(cmdsOf(adminCtx))
			c.SilenceUsage = true
			c.SilenceErrors = true
			c.SetIn(strings.NewReader(""))
			c.SetOut(io.Discard)
			c.SetErr(io.Discard)
			c.SetArgs(tc.args)
			err := c.ExecuteContext(adminCtx)
			if !errors.Is(err, gitserve.ErrSystemMalfunction) {
				t.Fatal("not true: errors.Is(err, gitserve.ErrSystemMalfunction)")
			}
			if strings.Contains(err.Error(), "/server/path") {
				t.Fatal("not true: !strings.Contains(err.Error(), \"/server/path\")")
			}
			if got := logs.count("ERRO"); got != 1 {
				t.Fatalf("logs.count(\"ERRO\") = %v, want %v", got, 1)
			}
			if !strings.Contains(logs.String(), "/server/path") {
				t.Fatal("not true: strings.Contains(logs.String(), \"/server/path\")")
			}
		})
	}
}

// requestSession is an ssh.Session whose SendRequest fails with err, as a
// send on a channel the client has closed does. Only SendRequest is used.
type requestSession struct {
	ssh.Session
	err error
}

func (s requestSession) SendRequest(string, bool, []byte) (bool, error) {
	return false, s.err
}

// A git service that fails while its session context is still live is a
// server failure, unless the client has closed the channel: git may see
// the end of its input and fail before the channel's context ends.
func TestGitServiceErrorClosedChannel(t *testing.T) {
	failed := errors.New("exit status 128: the remote end hung up unexpectedly")
	for _, tc := range []struct {
		name    string
		sendErr error
		gone    bool
	}{
		{"channel open", nil, false},
		{"channel closed", io.EOF, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := log.NewWithOptions(&logs, log.Options{Level: log.DebugLevel})
			ctx := context.WithValue(t.Context(), ContextKeySession, ssh.Session(requestSession{err: tc.sendErr}))

			err := gitServiceError(ctx, logger, gitserve.UploadPackService, "r", failed)
			if tc.gone {
				if !errors.Is(err, context.Canceled) || !errors.Is(err, failed) {
					t.Fatalf("err = %v, want context.Canceled wrapping the git failure", err)
				}
				if strings.Contains(logs.String(), "ERRO") {
					t.Fatalf("a closed channel was logged as an error:\n%s", logs.String())
				}
				return
			}
			if !errors.Is(err, gitserve.ErrSystemMalfunction) {
				t.Fatalf("err = %v, want %v", err, gitserve.ErrSystemMalfunction)
			}
			if !strings.Contains(logs.String(), "ERRO") {
				t.Fatalf("a server failure was not logged as an error:\n%s", logs.String())
			}
		})
	}
}
