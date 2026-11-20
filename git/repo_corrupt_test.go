package git

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/git/gittest"
)

// TestLsTreeCorruptRefsIsNotMissing checks that git failing to read a
// repository, here one with corrupt packed-refs, is not taken for a
// missing revision: both exit with 128, and only a revision git itself
// reports missing is an ErrRevisionNotExist.
func TestLsTreeCorruptRefsIsNotMissing(t *testing.T) {
	t.Parallel()
	bare := filepath.Join(t.TempDir(), "r.git")
	gittest.Git(t, filepath.Dir(bare), nil, "init", "-q", "--bare", bare)
	root := gittest.Commit(t, bare, gittest.Valid, gittest.Tree(t, bare, "f", "root\n"), "root")
	gittest.Git(t, bare, nil, "update-ref", "refs/heads/main", root)
	gittest.Git(t, bare, nil, "symbolic-ref", "HEAD", "refs/heads/main")

	repo, err := Open(bare)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LsTree("nope"); !errors.Is(err, ErrRevisionNotExist) {
		t.Fatalf("LsTree(nope) error = %v, want ErrRevisionNotExist", err)
	}

	gittest.Git(t, bare, nil, "pack-refs", "--all")
	if err := os.WriteFile(filepath.Join(bare, "packed-refs"), []byte("not a packed-refs file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"main", "nope"} {
		_, err := repo.LsTree(ref)
		if err == nil || errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("LsTree(%q) with corrupt refs: err = %v, want git's failure", ref, err)
		}
	}
}

// TestLsTreeMissingTreeIsNotMissingRevision checks that a commit whose tree
// object is gone from the repository is git failing to read it, not a
// revision the caller got wrong, while a revision that names no tree-ish,
// such as a blob, still is an ErrRevisionNotExist.
func TestLsTreeMissingTreeIsNotMissingRevision(t *testing.T) {
	t.Parallel()
	bare := filepath.Join(t.TempDir(), "r.git")
	gittest.Git(t, filepath.Dir(bare), nil, "init", "-q", "--bare", bare)
	tree := gittest.Tree(t, bare, "f", "root\n")
	root := gittest.Commit(t, bare, gittest.Valid, tree, "root")
	blob := gittest.Git(t, bare, nil, "rev-parse", tree+":f")
	gittest.Git(t, bare, nil, "update-ref", "refs/heads/main", root)

	repo, err := Open(bare)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LsTree(blob); !errors.Is(err, ErrRevisionNotExist) {
		t.Fatalf("LsTree(blob) error = %v, want ErrRevisionNotExist", err)
	}

	if err := os.Remove(filepath.Join(bare, "objects", tree[:2], tree[2:])); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"main", root} {
		_, err := repo.LsTree(ref)
		if err == nil || errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("LsTree(%q) without its tree object: err = %v, want git's failure", ref, err)
		}
	}
	if _, err := repo.LsTree(blob); !errors.Is(err, ErrRevisionNotExist) {
		t.Fatalf("LsTree(blob) error = %v, want ErrRevisionNotExist", err)
	}
}

// TestMissingCommitObjectOfRef checks that a ref whose commit object is
// gone (a branch, an annotated tag, a detached HEAD) is the repository
// failing, for both the tree and the commit lookup, while a raw object ID
// that names nothing, an unknown name and a tag on a blob, lightweight or
// annotated, still are missing revisions.
func TestMissingCommitObjectOfRef(t *testing.T) {
	t.Parallel()
	bare := filepath.Join(t.TempDir(), "r.git")
	gittest.Git(t, filepath.Dir(bare), nil, "init", "-q", "--bare", bare)
	tree := gittest.Tree(t, bare, "f", "root\n")
	blob := gittest.Git(t, bare, nil, "rev-parse", tree+":f")
	root := gittest.Commit(t, bare, gittest.Valid, tree, "root")
	gittest.Git(t, bare, nil, "update-ref", "refs/heads/main", root)
	gittest.Git(t, bare, nil, "update-ref", "refs/tags/blobtag", blob)
	gittest.Git(t, bare, nil, "tag", "-a", "-m", "on a blob", "annblob", blob)
	gittest.Git(t, bare, nil, "tag", "-a", "-m", "on main", "anngone", root)
	gittest.Git(t, bare, nil, "symbolic-ref", "HEAD", "refs/heads/main")
	if err := os.Remove(filepath.Join(bare, "objects", root[:2], root[2:])); err != nil {
		t.Fatal(err)
	}

	detached := filepath.Join(t.TempDir(), "d.git")
	gittest.Git(t, filepath.Dir(detached), nil, "init", "-q", "--bare", detached)
	dcommit := gittest.Commit(t, detached, gittest.Valid, gittest.Tree(t, detached, "f", "root\n"), "root")
	gittest.Git(t, detached, nil, "update-ref", "--no-deref", "HEAD", dcommit)
	if err := os.Remove(filepath.Join(detached, "objects", dcommit[:2], dcommit[2:])); err != nil {
		t.Fatal(err)
	}
	drepo, err := Open(detached)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drepo.LsTree("HEAD"); err == nil || errors.Is(err, ErrRevisionNotExist) {
		t.Errorf("LsTree(HEAD) of a detached HEAD without its commit: err = %v, want git's failure", err)
	}
	if _, err := drepo.CommitByRevision("HEAD"); err == nil || errors.Is(err, ErrRevisionNotExist) {
		t.Errorf("CommitByRevision(HEAD) of a detached HEAD without its commit: err = %v, want git's failure", err)
	}

	repo, err := Open(bare)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"main", "refs/heads/main", "HEAD", "anngone"} {
		if _, err := repo.LsTree(ref); err == nil || errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("LsTree(%q): err = %v, want git's failure", ref, err)
		}
		if _, err := repo.CommitByRevision(ref); err == nil || errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("CommitByRevision(%q): err = %v, want git's failure", ref, err)
		}
	}
	for _, rev := range []string{root, "nope", "main~1", "blobtag", "annblob"} {
		if _, err := repo.LsTree(rev); !errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("LsTree(%q): err = %v, want ErrRevisionNotExist", rev, err)
		}
		if _, err := repo.CommitByRevision(rev); !errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("CommitByRevision(%q): err = %v, want ErrRevisionNotExist", rev, err)
		}
	}
}
