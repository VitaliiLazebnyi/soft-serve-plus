package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/git/gittest"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// TestRefListingFailureIsReported breaks a repository's refs so that git
// cannot list them. The commands that list branches or tags report a
// failure instead of taking it for a repository without any: `branch
// list` printed nothing, `branch delete` answered "reference does not
// exist", and `branch default` refused the branch the same way. The
// client gets the generic errGitFailed; git's own message, which names
// server paths, goes to the server log at Error, once.
func TestRefListingFailureIsReported(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	var logs bytes.Buffer
	ctx = log.WithContext(ctx, log.NewWithOptions(&logs, log.Options{Level: log.DebugLevel}))
	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "r", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	is.NoErr(err)
	bare := filepath.Join(config.FromContext(ctx).DataPath, "repos", "r.git")
	root := gittest.Commit(t, bare, gittest.Valid, gittest.Tree(t, bare, "f", "root\n"), "root")
	gittest.Git(t, bare, nil, "update-ref", "refs/heads/main", root)
	gittest.Git(t, bare, nil, "update-ref", "refs/heads/other", root)
	gittest.Git(t, bare, nil, "update-ref", "refs/tags/v1", root)
	gittest.Git(t, bare, nil, "symbolic-ref", "HEAD", "refs/heads/main")
	gittest.Git(t, bare, nil, "pack-refs", "--all")
	is.NoErr(os.WriteFile(filepath.Join(bare, "packed-refs"), []byte("not a packed-refs file\n"), 0o600))

	for _, args := range [][]string{
		{"branch", "list", "r"},
		{"branch", "delete", "r", "other"},
		{"branch", "default", "r", "other"},
		{"tag", "list", "r"},
		{"tag", "delete", "r", "v1"},
		{"info", "r"},
		{"branch", "default", "r"},
		{"tree", "r"},
		{"blob", "r", "f"},
	} {
		logs.Reset()
		out, stderr, err := runRepoOutput(t, ownerCtx, args...)
		if !errors.Is(err, errGitFailed) {
			t.Errorf("%v: err = %v, output %q; want %v", args, err, out, errGitFailed)
		}
		// git reports "unexpected line in ./packed-refs".
		if client := out + stderr + fmt.Sprint(err); strings.Contains(client, "unexpected line in") {
			t.Errorf("%v: git's message reached the client: %q", args, client)
		}
		if n := strings.Count(logs.String(), "unexpected line in"); n != 1 || !strings.Contains(logs.String(), "ERRO") {
			t.Errorf("%v: want git's message logged once at Error, got %d, log:\n%q", args, n, logs.String())
		}
	}
}

// TestBranchesEmpty checks that a repository without branches has none,
// rather than failing as git show-ref does when nothing matches.
func TestBranchesEmpty(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "empty", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	is.NoErr(err)

	out, _, err := runRepoOutput(t, ownerCtx, "branch", "list", "empty")
	is.NoErr(err)
	is.Equal(out, "")
	err = runRepo(t, ownerCtx, "branch", "delete", "empty", "main")
	is.True(errors.Is(err, git.ErrReferenceNotExist))
	_, _, err = runRepoOutput(t, ownerCtx, "branch", "default", "empty")
	is.True(errors.Is(err, errRepoEmpty))
	_, _, err = runRepoOutput(t, ownerCtx, "info", "empty")
	is.NoErr(err)
}

// gitFailed logs a git run that failed because the client went away, its
// context canceled, at Debug and not at Error, and still answers
// errGitFailed.
func TestGitFailedCanceledLogsAtDebug(t *testing.T) {
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(log.WithContext(t.Context(), log.NewWithOptions(&logs, log.Options{Level: log.DebugLevel})))
	cancel()

	err := gitFailed(ctx, "list branches", "r", errors.New("signal: killed"))
	if !errors.Is(err, errGitFailed) {
		t.Fatalf("err = %v, want %v", err, errGitFailed)
	}
	out := logs.String()
	if !strings.Contains(out, "DEBU") || !strings.Contains(out, "signal: killed") {
		t.Errorf("want the failure logged at Debug, log:\n%s", out)
	}
	if strings.Contains(out, "ERRO") {
		t.Errorf("a canceled git run was logged at Error:\n%s", out)
	}
}
