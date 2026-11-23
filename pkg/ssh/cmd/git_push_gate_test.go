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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/matryer/is"
	"github.com/spf13/cobra"
)

// A push over SSH counts as running for the removal of a repository an
// HTTP push created and left without refs, so that removal cannot delete
// the repository while the SSH push may write refs to it.
func TestSSHPushHoldsOffPushCreatedRemoval(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	user := proto.UserFromContext(adminCtx)
	repo, err := be.CreateRepository(ctx, "shared", user, proto.RepositoryOptions{})
	is.NoErr(err)

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
	is.NoErr(err)
	go io.Copy(io.Discard, stdout) //nolint: errcheck

	removed, err := be.RemovePushCreatedRepository(ctx, repo)
	is.NoErr(err)
	is.True(!removed) // the SSH push is running

	// The client sends no commands, and the push ends.
	_, err = stdinW.Write([]byte("0000"))
	is.NoErr(err)
	is.NoErr(stdinW.Close())
	<-errc //nolint:errcheck // only that the command returned matters here

	removed, err = be.RemovePushCreatedRepository(ctx, repo)
	is.NoErr(err)
	is.True(removed)
}

// An LFS upload over SSH counts as running for that removal too: removing
// the repository under it would fail the upload and leave its temporary
// file behind.
func TestSSHLFSUploadHoldsOffPushCreatedRemoval(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	user := proto.UserFromContext(adminCtx)
	repo, err := be.CreateRepository(ctx, "shared", user, proto.RepositoryOptions{})
	is.NoErr(err)

	stdin, stdinW := io.Pipe()
	stdout, stdoutW := io.Pipe()

	c := cmdsOf(adminCtx).GitLFSTransfer()
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
	is.NoErr(err)
	go io.Copy(io.Discard, stdout) //nolint: errcheck

	removed, err := be.RemovePushCreatedRepository(ctx, repo)
	is.NoErr(err)
	is.True(!removed) // the LFS upload is running

	is.NoErr(stdinW.Close())
	<-errc //nolint:errcheck // only that the command returned matters here

	removed, err = be.RemovePushCreatedRepository(ctx, repo)
	is.NoErr(err)
	is.True(removed)
}

// A push that finds the repository it was going to push to removed, as an
// unused HTTP push-created repository, creates it anew, as a push to a
// missing name does, rather than failing.
func TestSSHPushAfterPushCreatedRemovalCreates(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	user := proto.UserFromContext(adminCtx)
	repo, err := be.CreateRepository(ctx, "shared", user, proto.RepositoryOptions{})
	is.NoErr(err)

	// The removal runs after this push looked the repository up and
	// before it starts. The hook runs again for the push that creates
	// the repository anew; only the first run removes.
	cmds := cmdsOf(ctx)
	cmds.beforeBeginPush = func() {
		cmds.beforeBeginPush = nil
		removed, err := be.RemovePushCreatedRepository(ctx, repo)
		is.NoErr(err)
		is.True(removed)
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
		is.NoErr(err)
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
	is.NoErr(c.ExecuteContext(adminCtx))

	cur, err := be.Repository(ctx, "shared")
	is.NoErr(err) // created anew
	is.True(cur.ID() != repo.ID())
	r, err := cur.Open(t.Context())
	is.NoErr(err)
	head, err := r.HEAD()
	is.NoErr(err)
	is.Equal(head.ID(), commit) // with the pushed commit
}

// A push to a missing name creates the repository and then pushes to its
// directory, found by name. If an admin deletes the new repository before
// git starts and someone else creates another under the same name, the push
// must not write to that one, which it was never checked against.
func TestSSHPushToCreateDoesNotWriteReplacedRepository(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	pusherCtx := withUser(t, ctx, be, "boss", true)
	otherCtx := withUser(t, ctx, be, "other", true)

	var replacement proto.Repository
	cmds := cmdsOf(ctx)
	cmds.beforeBeginPush = func() {
		cmds.beforeBeginPush = nil
		is.NoErr(be.DeleteRepository(ctx, "fresh"))
		var err error
		replacement, err = be.CreateRepository(ctx, "fresh", proto.UserFromContext(otherCtx), proto.RepositoryOptions{Private: true})
		is.NoErr(err)
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
		is.NoErr(err)
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
	is.True(errors.Is(err, proto.ErrRepoNotFound)) // the repository it created is gone
	is.True(replacement != nil)                    // the hook ran

	cur, err := be.Repository(ctx, "fresh")
	is.NoErr(err)
	is.Equal(cur.ID(), replacement.ID()) // the other repository is still there
	r, err := cur.Open(t.Context())
	is.NoErr(err)
	if _, err := r.HEAD(); err == nil {
		t.Fatal("the push wrote to the repository that replaced the one it created")
	}
	is.True(!strings.Contains(out.String(), "refs/heads/main")) // and git never ran on it
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
// git.ErrSystemMalfunction, not the error.
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
		{"lfs upload", true, (*Commands).GitLFSTransfer, []string{"shared", "upload"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			armed := new(atomic.Bool)
			ctx, be, logs := newLookupTestContextWith(t, func(s store.Store) store.Store {
				return beginPushFailStore{Store: s, armed: armed}
			})
			adminCtx := withUser(t, ctx, be, "boss", true)
			if tc.exists {
				_, err := be.CreateRepository(ctx, "shared", proto.UserFromContext(adminCtx), proto.RepositoryOptions{})
				is.NoErr(err)
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
			is.True(errors.Is(err, git.ErrSystemMalfunction))
			is.True(!strings.Contains(err.Error(), "/server/path"))
			is.Equal(logs.count("ERRO"), 1)
			is.True(strings.Contains(logs.String(), "/server/path"))
		})
	}
}
