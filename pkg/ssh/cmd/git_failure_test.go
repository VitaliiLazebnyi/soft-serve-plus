package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/soft-serve/git/gittest"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// gitFailureRepo is a repository "r" owned by the session's user, with the
// files "f" and "d/g" on branch main (HEAD), a branch "other" and a tag
// "v1", all loose: every object and ref is a file of its own. It holds the
// session context, the backend, the server log, the repository's directory
// and the IDs of main's commit, its root tree, the blob of "f" and the tree
// of "d".
type gitFailureRepo struct {
	ctx                         context.Context
	be                          *backend.Backend
	logs                        *syncLog
	dir                         string
	commit, tree, blob, subtree string
}

func newGitFailureRepo(t *testing.T, wrap func(store.Store) store.Store) gitFailureRepo {
	t.Helper()
	ctx, be, logs := newLookupTestContextWith(t, wrap)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	if _, err := be.CreateRepository(ownerCtx, "r", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(config.FromContext(ctx).DataPath, "repos", "r.git")
	blob := gittest.Git(t, dir, []byte("root\n"), "hash-object", "-w", "--stdin")
	subtree := gittest.Tree(t, dir, "g", "sub\n")
	tree := gittest.Git(t, dir, fmt.Appendf(nil, "100644 blob %s\tf\n040000 tree %s\td\n", blob, subtree), "mktree")
	commit := gittest.Commit(t, dir, gittest.Valid, tree, "root")
	gittest.Git(t, dir, nil, "update-ref", "refs/heads/main", commit)
	gittest.Git(t, dir, nil, "update-ref", "refs/heads/other", commit)
	gittest.Git(t, dir, nil, "update-ref", "refs/tags/v1", commit)
	gittest.Git(t, dir, nil, "symbolic-ref", "HEAD", "refs/heads/main")
	return gitFailureRepo{ownerCtx, be, logs, dir, commit, tree, blob, subtree}
}

// looseObject returns the path of the loose object id in the repository dir.
func looseObject(dir, id string) string {
	return filepath.Join(dir, "objects", id[:2], id[2:])
}

// wantGitFailed runs the repo command args and checks that the client gets
// only errGitFailed, with none of leaks in what it sees, and that git's
// failure is in the server log once, at Error, holding want.
func (g gitFailureRepo) wantGitFailed(t *testing.T, want string, leaks []string, args ...string) {
	t.Helper()
	out, stderr, err := runRepoOutput(t, g.ctx, args...)
	if !errors.Is(err, errGitFailed) {
		t.Errorf("%v: err = %v, output %q; want %v", args, err, out, errGitFailed)
	}
	client := out + stderr + fmt.Sprint(err)
	for _, leak := range leaks {
		if strings.Contains(client, leak) {
			t.Errorf("%v: %q reached the client: %q", args, leak, client)
		}
	}
	if n := g.logs.count("ERRO"); n != 1 || !strings.Contains(g.logs.String(), want) {
		t.Errorf("%v: want one Error line holding %q, got %d, log:\n%s", args, want, n, g.logs.String())
	}
}

// A ref git cannot lock, as when another git holds it, fails the change on
// the server: the client must not see git's message, which names the
// repository's path on the server.
func TestLockedRefIsServerFailure(t *testing.T) {
	for _, tc := range []struct {
		lock string
		args []string
	}{
		{"HEAD.lock", []string{"branch", "default", "r", "other"}},
		{"refs/heads/other.lock", []string{"branch", "delete", "r", "other"}},
		{"refs/tags/v1.lock", []string{"tag", "delete", "r", "v1"}},
	} {
		t.Run(tc.lock, func(t *testing.T) {
			g := newGitFailureRepo(t, nil)
			if err := os.WriteFile(filepath.Join(g.dir, filepath.FromSlash(tc.lock)), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			g.wantGitFailed(t, ".lock", []string{".lock", g.dir, "fatal", "error:"}, tc.args...)
		})
	}
}

// An object that is missing from the repository, while the refs that lead
// to it still resolve, fails the read on the server.
func TestMissingObjectIsServerFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		missing func(g gitFailureRepo) string
		args    []string
	}{
		{"blob", func(g gitFailureRepo) string { return g.blob }, []string{"blob", "r", "f"}},
		{"blob raw", func(g gitFailureRepo) string { return g.blob }, []string{"blob", "-r", "r", "f"}},
		{"commit blob", func(g gitFailureRepo) string { return g.blob }, []string{"commit", "r", "main"}},
		{"commit patch", func(g gitFailureRepo) string { return g.blob }, []string{"commit", "-p", "r", "main"}},
		{"tree", func(g gitFailureRepo) string { return g.tree }, []string{"tree", "r"}},
		{"tree path", func(g gitFailureRepo) string { return g.tree }, []string{"tree", "r", "f"}},
		{"blob tree", func(g gitFailureRepo) string { return g.tree }, []string{"blob", "r", "f"}},
		{"commit tree", func(g gitFailureRepo) string { return g.tree }, []string{"commit", "r", "main"}},
		{"tree subtree", func(g gitFailureRepo) string { return g.subtree }, []string{"tree", "r", "d"}},
		{"tree in subtree", func(g gitFailureRepo) string { return g.subtree }, []string{"tree", "r", "d/g"}},
		{"blob in subtree", func(g gitFailureRepo) string { return g.subtree }, []string{"blob", "r", "d/g"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGitFailureRepo(t, nil)
			id := tc.missing(g)
			if err := os.Remove(looseObject(g.dir, id)); err != nil {
				t.Fatal(err)
			}
			g.wantGitFailed(t, "git failed", []string{id, "fatal", "bad file", "unable to read", "Needed"}, tc.args...)
			// git's message is in the log: it names the missing object,
			// or, resolving a tree it cannot find, is a fatal error.
			if log := g.logs.String(); !strings.Contains(log, id) && !strings.Contains(log, "fatal") {
				t.Errorf("%v: git's message is not in the log:\n%s", tc.args, log)
			}
		})
	}
}

// A repository directory git does not accept, or one that is gone, fails
// every command that opens it on the server.
func TestOpenFailureIsServerFailure(t *testing.T) {
	t.Run("not a repository", func(t *testing.T) {
		g := newGitFailureRepo(t, nil)
		if err := os.Remove(filepath.Join(g.dir, "HEAD")); err != nil {
			t.Fatal(err)
		}
		// The backend logs why git does not accept the directory; the
		// command logs nothing more.
		g.wantGitFailed(t, "git does not accept the repository", []string{g.dir, "not a git repository"}, "branch", "list", "r")
	})
	t.Run("not a directory", func(t *testing.T) {
		g := newGitFailureRepo(t, nil)
		if err := os.RemoveAll(g.dir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(g.dir, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		g.wantGitFailed(t, "open repository", []string{g.dir, "does not exist"}, "branch", "list", "r")
	})
}

// failRepoByNameStore fails GetRepoByName while armed.
type failRepoByNameStore struct {
	store.Store
	armed *atomic.Bool
}

var errInjectedRepoRead = errors.New("injected repos read failure: /server/path")

func (s failRepoByNameStore) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	if s.armed.Load() {
		return models.Repo{}, errInjectedRepoRead
	}
	return s.Store.GetRepoByName(ctx, h, name)
}

// `repo info` on a mirror reads its sync status from the database; a failed
// read is logged, and the client gets the generic lookup error.
func TestInfoMirrorSyncStatusLookupFailure(t *testing.T) {
	armed := new(atomic.Bool)
	g := newGitFailureRepo(t, func(s store.Store) store.Store { return failRepoByNameStore{Store: s, armed: armed} })
	if err := store.FromContext(g.ctx).SetRepoIsMirrorByName(g.ctx, db.FromContext(g.ctx), "r", true); err != nil {
		t.Fatal(err)
	}
	afterGate(t, func() { armed.Store(true) })

	out, stderr, err := runRepoOutput(t, g.ctx, "info", "r")
	if !errors.Is(err, errLookupFailed) {
		t.Fatalf("err = %v, output %q, want %v", err, out+stderr, errLookupFailed)
	}
	if client := out + stderr + err.Error(); strings.Contains(client, "/server/path") {
		t.Errorf("the database error reached the client: %q", client)
	}
	if n := g.logs.count("ERRO"); n != 1 || !strings.Contains(g.logs.String(), "/server/path") {
		t.Errorf("want the failure logged once at Error, got %d:\n%s", n, g.logs.String())
	}
}
