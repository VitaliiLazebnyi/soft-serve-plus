package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/git/gittest"
	"github.com/charmbracelet/soft-serve/pkg/proto"
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
	ctx, be := newAuthTestContext(t)
	var logs bytes.Buffer
	ctx = withTestLogger(ctx, log.NewWithOptions(&logs, log.Options{Level: log.DebugLevel}))
	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "r", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(cmdsOf(ctx).cfg.DataPath, "repos", "r.git")
	root := gittest.Commit(t, bare, gittest.Valid, gittest.Tree(t, bare, "f", "root\n"), "root")
	gittest.Git(t, bare, nil, "update-ref", "refs/heads/main", root)
	gittest.Git(t, bare, nil, "update-ref", "refs/heads/other", root)
	gittest.Git(t, bare, nil, "update-ref", "refs/tags/v1", root)
	gittest.Git(t, bare, nil, "symbolic-ref", "HEAD", "refs/heads/main")
	gittest.Git(t, bare, nil, "pack-refs", "--all")
	if err := os.WriteFile(filepath.Join(bare, "packed-refs"), []byte("not a packed-refs file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

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
	ctx, be := newAuthTestContext(t)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "empty", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	out, _, err := runRepoOutput(t, ownerCtx, "branch", "list", "empty")
	if err != nil {
		t.Fatal(err)
	}
	if got := out; got != "" {
		t.Fatalf("out = %v, want %v", got, "")
	}
	err = runRepo(t, ownerCtx, "branch", "delete", "empty", "main")
	if !errors.Is(err, git.ErrReferenceNotExist) {
		t.Fatal("not true: errors.Is(err, git.ErrReferenceNotExist)")
	}
	_, _, err = runRepoOutput(t, ownerCtx, "branch", "default", "empty")
	if !errors.Is(err, errRepoEmpty) {
		t.Fatal("not true: errors.Is(err, errRepoEmpty)")
	}
	_, _, err = runRepoOutput(t, ownerCtx, "info", "empty")
	if err != nil {
		t.Fatal(err)
	}
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

			if err := runRepo(t, ownerCtx, "branch", "delete", "hooks", "bad"); err != nil {
				t.Fatal(err)
			}
			if gitRefExists(t, bare, "refs/heads/bad") {
				t.Fatal("not true: !gitRefExists(t, bare, \"refs/heads/bad\")")
			}
			// The branch delete's delivery is recorded in the background.
			// Let it finish first: under a DELETE journal_mode data source
			// the tag delete's repository read waits out that commit, and
			// one longer than busy_timeout fails the delete with
			// errLookupFailed.
			// WaitForDeliveries would refuse the tag delete's delivery, so
			// wait for the row instead.
			whAwaitDeliveries(t, ownerCtx, be, "hooks", id, 1)
			if err := runRepo(t, ownerCtx, "tag", "delete", "hooks", "bad"); err != nil {
				t.Fatal(err)
			}
			if gitRefExists(t, bare, "refs/tags/bad") {
				t.Fatal("not true: !gitRefExists(t, bare, \"refs/tags/bad\")")
			}

			be.WaitForDeliveries(context.Background())
			r, err := be.Repository(ownerCtx, "hooks")
			if err != nil {
				t.Fatal(err)
			}
			ds, err := be.ListWebhookDeliveries(ownerCtx, r, id)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(ds); got != 2 {
				t.Fatalf("len(ds) = %v, want %v", got, 2)
			}
			refs := map[string]bool{}
			for _, d := range ds {
				if got := d.Event.String(); got != "branch_tag_delete" {
					t.Fatalf("d.Event.String() = %v, want %v", got, "branch_tag_delete")
				}
				full, err := be.WebhookDelivery(ownerCtx, r, id, d.ID)
				if err != nil {
					t.Fatal(err)
				}
				var payload struct {
					Ref    string `json:"ref"`
					Before string `json:"before"`
				}
				if err := json.Unmarshal([]byte(full.RequestBody), &payload); err != nil {
					t.Fatal(err)
				}
				if got := payload.Before; got != bad {
					t.Fatalf("payload.Before = %v, want %v", got, bad)
				} // the deleted ref's ID
				refs[payload.Ref] = true
			}
			if got, want := refs, (map[string]bool{"refs/heads/bad": true, "refs/tags/bad": true}); !reflect.DeepEqual(got, want) {
				t.Fatalf("refs = %v, want %v", got, want)
			}
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
			ctx, be := newAuthTestContext(t)
			ownerCtx := whRepo(t, ctx, be)
			bare := filepath.Join(cmdsOf(ownerCtx).cfg.DataPath, "repos", "hooks.git")
			bad := gittest.Commit(t, bare, kind, gittest.Tree(t, bare, "f", "bad\n"), "bad")

			_, _, err := runRepoOutput(t, ownerCtx, "commit", "hooks", bad)
			if !errors.Is(err, git.ErrMalformedCommit) {
				t.Fatal("not true: errors.Is(err, git.ErrMalformedCommit)")
			}
		})
	}
}
