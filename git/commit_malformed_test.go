package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git/gittest"
)

// malformedRepo returns a bare repository with, for each malformed kind, a
// branch and a tag of that name pointing at a commit of that kind, and a
// branch "main" whose history runs valid, then one commit of each malformed
// kind, then valid again. Each commit changes the file "f". It also returns
// main's commits, newest first, and the IDs of the valid ones.
func malformedRepo(t *testing.T) (r *Repository, history []string, valid map[string]bool) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "r.git")
	gittest.Git(t, "", nil, "init", "-q", "--bare", dir)

	valid = map[string]bool{}
	commit := func(kind gittest.Kind, content string, parents ...string) string {
		id := gittest.Commit(t, dir, kind, gittest.Tree(t, dir, "f", content+"\n"), content, parents...)
		if kind == gittest.Valid {
			valid[id] = true
		}
		return id
	}
	tip := commit(gittest.Valid, "root")
	history = []string{tip}
	for _, kind := range gittest.Malformed {
		id := commit(kind, string(kind), tip)
		gittest.Git(t, dir, nil, "update-ref", RefsHeads+string(kind), id)
		gittest.Git(t, dir, nil, "update-ref", RefsTags+string(kind), id)
		tip = id
		history = append([]string{tip}, history...)
	}
	tip = commit(gittest.Valid, "top", tip)
	history = append([]string{tip}, history...)
	gittest.Git(t, dir, nil, "update-ref", RefsHeads+"main", tip)
	gittest.Git(t, dir, nil, "symbolic-ref", "HEAD", RefsHeads+"main")

	r, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return r, history, valid
}

// TestMalformedCommitIsAnError checks that every way of reading one commit
// turns git-module's parser panic into ErrMalformedCommit.
func TestMalformedCommitIsAnError(t *testing.T) {
	t.Parallel()
	r, _, _ := malformedRepo(t)

	for _, kind := range gittest.Malformed {
		name := string(kind)
		reads := map[string]func(r *Repository) (*Commit, error){
			"CatFileCommit":    func(r *Repository) (*Commit, error) { return r.CatFileCommit(RefsHeads + name) },
			"BranchCommit":     func(r *Repository) (*Commit, error) { return r.BranchCommit(name) },
			"TagCommit":        func(r *Repository) (*Commit, error) { return r.TagCommit(name) },
			"CommitByRevision": func(r *Repository) (*Commit, error) { return r.CommitByRevision(name) },
		}
		for read, fn := range reads {
			t.Run(name+"/"+read, func(t *testing.T) {
				t.Parallel()
				// A fresh repository each time: git-module caches
				// the commits it parsed.
				fresh, err := Open(t.Context(), r.Path)
				if err != nil {
					t.Fatal(err)
				}
				c, err := fn(fresh)
				if !errors.Is(err, ErrMalformedCommit) {
					t.Fatalf("%s(%s) = %v, %v; want ErrMalformedCommit", read, name, c, err)
				}
			})
		}
	}
}

// TestLogSkipsMalformedCommits checks that the commit lists leave out the
// commits they cannot parse and keep the others, in order.
func TestLogSkipsMalformedCommits(t *testing.T) {
	t.Parallel()
	r, history, valid := malformedRepo(t)

	var want []string
	for _, id := range history {
		if valid[id] {
			want = append(want, id)
		}
	}
	check := func(name string, commits []*Commit, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got []string
		for _, c := range commits {
			got = append(got, c.ID.String())
		}
		if len(got) != len(want) {
			t.Fatalf("%s = %v, want the valid commits %v", name, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s = %v, want the valid commits %v", name, got, want)
			}
		}
	}

	commits, err := r.Log("main", LogOptions{})
	check("Log", commits, err)

	head, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}
	page, err := r.CommitsByPage(head, 1, len(history))
	check("CommitsByPage", page, err)

	// The options Log takes apply.
	commits, err = r.Log("main", LogOptions{MaxCount: 1})
	if err != nil || len(commits) != 1 || commits[0].ID.String() != history[0] {
		t.Fatalf("Log(MaxCount 1) = %v, %v; want %s", commits, err, history[0])
	}
}

// TestBlameMalformedCommitIsAnError checks that a blame whose lines come
// from a malformed commit fails rather than panics.
func TestBlameMalformedCommitIsAnError(t *testing.T) {
	t.Parallel()
	r, _, _ := malformedRepo(t)

	for _, kind := range gittest.Malformed {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			if _, err := r.BlameFile(RefsHeads+string(kind), "f"); !errors.Is(err, ErrMalformedCommit) {
				t.Fatalf("BlameFile = %v, want ErrMalformedCommit", err)
			}
		})
	}
}

// TestCommitByRevisionMissing checks that a revision git cannot resolve is
// still an ErrRevisionNotExist, matched by git's exit status.
func TestCommitByRevisionMissing(t *testing.T) {
	t.Parallel()
	r, _, _ := malformedRepo(t)

	for _, rev := range []string{"nope", "main~99"} {
		if _, err := r.CommitByRevision(rev); !errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("CommitByRevision(%q) = %v, want ErrRevisionNotExist", rev, err)
		}
	}
	c, err := r.CommitByRevision("main")
	if err != nil || c.Author == nil || c.Author.Email != "author@example.com" {
		t.Fatalf("CommitByRevision(main) = %+v, %v", c, err)
	}
}

// TestCommitByRevisionUnreadable checks that git log failing with exit
// status 128 on a revision that does resolve, here because the commit's
// parent is missing from the repository, is reported as git's error and
// not as a missing revision.
func TestCommitByRevisionUnreadable(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "r.git")
	gittest.Git(t, "", nil, "init", "-q", "--bare", dir)
	missing := strings.Repeat("1", 40)
	id := gittest.Commit(t, dir, gittest.Valid, gittest.Tree(t, dir, "f", "x\n"), "orphan", missing)
	gittest.Git(t, dir, nil, "update-ref", RefsHeads+"main", id)
	r, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}

	_, err = r.CommitByRevision("main")
	if err == nil || errors.Is(err, ErrRevisionNotExist) {
		t.Fatalf("CommitByRevision on a commit with a missing parent = %v; want git's error", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 128 {
		t.Fatalf("CommitByRevision error = %v; want git log's exit status 128", err)
	}
}

// TestMalformedCommitErrorHidesParser checks that the error, which reaches
// SSH users, names the revision and not the parser's panic.
func TestMalformedCommitErrorHidesParser(t *testing.T) {
	t.Parallel()
	r, _, _ := malformedRepo(t)

	for _, kind := range gittest.Malformed {
		rev := RefsHeads + string(kind)
		_, err := r.CatFileCommit(rev)
		if want := ErrMalformedCommit.Error() + ": " + rev; err == nil || err.Error() != want {
			t.Errorf("CatFileCommit(%s) error = %v; want %q", rev, err, want)
		}
	}
}

// TestCommitByRevisionBrokenRepository checks that a repository git cannot
// read at all, here because its objects directory is gone, is reported as
// git's error: "rev-parse --verify --quiet" exits with 128 for it, and only
// its exit status 1 means a missing revision.
func TestCommitByRevisionBrokenRepository(t *testing.T) {
	t.Parallel()
	r, _, _ := malformedRepo(t)
	if err := os.RemoveAll(filepath.Join(r.Path, "objects")); err != nil {
		t.Fatal(err)
	}

	for _, rev := range []string{"main", "nope"} {
		if _, err := r.CommitByRevision(rev); err == nil || errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("CommitByRevision(%q) without objects/ = %v; want git's error", rev, err)
		}
	}
}
