package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/git/gittest"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// TestRefListingFailureIsReported breaks a repository's refs so that git
// cannot list them. The commands that list branches or tags report git's
// failure instead of taking it for a repository without any: `branch
// list` printed nothing, `branch delete` answered "reference does not
// exist", and `branch default` refused the branch the same way.
func TestRefListingFailureIsReported(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
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
	} {
		out, _, err := runRepoOutput(t, ownerCtx, args...)
		if err == nil || errors.Is(err, git.ErrReferenceNotExist) {
			t.Errorf("%v: err = %v, output %q; want git's failure", args, err, out)
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
