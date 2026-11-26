package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/git/gittest"
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
	t.Parallel()
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	var logs bytes.Buffer
	ctx = withTestLogger(ctx, log.NewWithOptions(&logs, log.Options{Level: log.DebugLevel}))
	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "r", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	is.NoErr(err)
	bare := filepath.Join(cmdsOf(ctx).cfg.DataPath, "repos", "r.git")
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
		{"commit", "r", "main"},
		{"tree", "r", "main", "f"},
		{"blob", "r", "main", "f"},
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
	t.Parallel()
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
	t.Parallel()
	var logs bytes.Buffer
	cmds := &Commands{logger: log.NewWithOptions(&logs, log.Options{Level: log.DebugLevel})}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := cmds.gitFailed(ctx, "list branches", "r", errors.New("signal: killed"))
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

// TestDeleteRefAtMalformedCommit deletes a branch and a tag that point at a
// commit git-module cannot parse. The delete needs only the ref's ID, so it
// succeeds, without a panic, and still sends its event.
func TestDeleteRefAtMalformedCommit(t *testing.T) {
	t.Parallel()
	for _, kind := range gittest.Malformed {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			ctx, be := newAuthTestContext(t)
			ownerCtx := whRepo(t, ctx, be)
			id := whCreate(t, ownerCtx, be, "-e", "branch_tag_delete")
			whPointAtLoopback(t, ownerCtx, "hooks", id)
			bare := filepath.Join(cmdsOf(ownerCtx).cfg.DataPath, "repos", "hooks.git")

			root := gittest.Commit(t, bare, gittest.Valid, gittest.Tree(t, bare, "f", "root\n"), "root")
			bad := gittest.Commit(t, bare, kind, gittest.Tree(t, bare, "f", "bad\n"), "bad", root)
			gittest.Git(t, bare, nil, "update-ref", "refs/heads/main", root)
			gittest.Git(t, bare, nil, "update-ref", "refs/heads/bad", bad)
			gittest.Git(t, bare, nil, "update-ref", "refs/tags/bad", bad)

			is.NoErr(runRepo(t, ownerCtx, "branch", "delete", "hooks", "bad"))
			is.True(!gitRefExists(t, bare, "refs/heads/bad"))
			// The branch delete's delivery is recorded in the background.
			// Let it finish first: under a DELETE journal_mode data source
			// the tag delete's repository read waits out that commit, and
			// one longer than busy_timeout fails the delete with
			// errLookupFailed.
			// WaitForDeliveries would refuse the tag delete's delivery, so
			// wait for the row instead.
			whAwaitDeliveries(t, ownerCtx, be, "hooks", id, 1)
			is.NoErr(runRepo(t, ownerCtx, "tag", "delete", "hooks", "bad"))
			is.True(!gitRefExists(t, bare, "refs/tags/bad"))

			be.WaitForDeliveries(context.Background())
			r, err := be.Repository(ownerCtx, "hooks")
			is.NoErr(err)
			ds, err := be.ListWebhookDeliveries(ownerCtx, r, id)
			is.NoErr(err)
			is.Equal(len(ds), 2)
			refs := map[string]bool{}
			for _, d := range ds {
				is.Equal(d.Event.String(), "branch_tag_delete")
				full, err := be.WebhookDelivery(ownerCtx, r, id, d.ID)
				is.NoErr(err)
				var payload struct {
					Ref    string `json:"ref"`
					Before string `json:"before"`
				}
				is.NoErr(json.Unmarshal([]byte(full.RequestBody), &payload))
				is.Equal(payload.Before, bad) // the deleted ref's ID
				refs[payload.Ref] = true
			}
			is.Equal(refs, map[string]bool{"refs/heads/bad": true, "refs/tags/bad": true})
		})
	}
}

// TestRepoCommitMalformed checks that "repo commit" on a commit git-module
// cannot parse reports an error instead of panicking.
func TestRepoCommitMalformed(t *testing.T) {
	t.Parallel()
	for _, kind := range gittest.Malformed {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			ctx, be := newAuthTestContext(t)
			ownerCtx := whRepo(t, ctx, be)
			bare := filepath.Join(cmdsOf(ownerCtx).cfg.DataPath, "repos", "hooks.git")
			bad := gittest.Commit(t, bare, kind, gittest.Tree(t, bare, "f", "bad\n"), "bad")

			_, _, err := runRepoOutput(t, ownerCtx, "commit", "hooks", bad)
			is.True(errors.Is(err, git.ErrMalformedCommit))
		})
	}
}
