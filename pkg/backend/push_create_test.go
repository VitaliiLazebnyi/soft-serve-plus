package backend

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// A repository created by an HTTP push is removed only if nothing was done
// with it since: no ref, no changed setting, no collaborator, webhook, LFS
// object or LFS lock.
func TestRemovePushCreatedRepositoryKeepsUsed(t *testing.T) {
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	inTx := func(fn func(tx *db.Tx) error) func(t *testing.T, r proto.Repository) {
		return func(t *testing.T, _ proto.Repository) {
			if err := be.db.TransactionContext(ctx, fn); err != nil {
				t.Fatal(err)
			}
		}
	}
	tests := map[string]func(t *testing.T, r proto.Repository){
		"untouched": nil,
		"ref": func(t *testing.T, r proto.Repository) {
			dir := be.repoPath(r.Name())
			tree, err := exec.CommandContext(t.Context(), "git", "-C", dir, "mktree").Output()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "commit-tree", string(tree[:len(tree)-1]), "-m", "x")
			cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
			commit, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if err := exec.CommandContext(t.Context(), "git", "-C", dir, "update-ref", "refs/heads/main", string(commit[:len(commit)-1])).Run(); err != nil {
				t.Fatal(err)
			}
		},
		// The settings are changed in the store, as the backend's setters
		// do, without the webhook event those also deliver.
		"description": inTx(func(tx *db.Tx) error {
			return be.store.SetRepoDescriptionByName(ctx, tx, "repo-description", "about")
		}),
		"project-name": inTx(func(tx *db.Tx) error {
			return be.store.SetRepoProjectNameByName(ctx, tx, "repo-project-name", "Project")
		}),
		"private": inTx(func(tx *db.Tx) error {
			return be.store.SetRepoIsPrivateByName(ctx, tx, "repo-private", true)
		}),
		"hidden": inTx(func(tx *db.Tx) error {
			return be.store.SetRepoIsHiddenByName(ctx, tx, "repo-hidden", true)
		}),
		"collaborator": func(t *testing.T, r proto.Repository) {
			if err := be.AddCollaborator(ctx, r.Name(), "alice", access.ReadWriteAccess); err != nil {
				t.Fatal(err)
			}
		},
		"webhook": func(t *testing.T, r proto.Repository) {
			inTx(func(tx *db.Tx) error {
				_, err := be.store.CreateWebhook(ctx, tx, models.Webhook{RepoID: r.ID(), URL: "https://example.com/hook", ContentType: 1, Active: true})
				return err
			})(t, r)
		},
		"lfs-object": func(t *testing.T, r proto.Repository) {
			inTx(func(tx *db.Tx) error {
				return be.store.CreateLFSObject(ctx, tx, r.ID(), "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", 1)
			})(t, r)
		},
		"lfs-lock": func(t *testing.T, r proto.Repository) {
			inTx(func(tx *db.Tx) error {
				_, err := be.store.CreateLFSLockForUser(ctx, tx, r.ID(), owner.ID(), "foo.png", "refs/heads/main")
				return err
			})(t, r)
		},
	}

	for name, use := range tests {
		t.Run(name, func(t *testing.T) {
			repoName := "repo-" + name
			r, err := be.CreateRepository(ctx, repoName, owner, proto.RepositoryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if use != nil {
				use(t, r)
			}

			removed, err := be.RemovePushCreatedRepository(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			_, err = be.Repository(ctx, repoName)
			if use == nil {
				if !removed {
					t.Fatal("not true: removed")
				}
				if !errors.Is(err, proto.ErrRepoNotFound) {
					t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
				}
			} else {
				if removed {
					t.Fatal("not true: !removed")
				}
				if err != nil {
					t.Fatal(err)
				} // the used repository is kept
			}
		})
	}
}

// pushWaitsForGate reports whether a BeginPush waits for its repository's
// push gate, which a removal holds.
func pushWaitsForGate() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		header, frames, _ := strings.Cut(g, "\n")
		if strings.Contains(header, "[sync.RWMutex.RLock") && strings.Contains(frames, "pkg/backend.(*Backend).BeginPush(") {
			return true
		}
	}
	return false
}

// A repository is not removed while a push to it runs, and a push that
// starts while it is being removed waits for that and then finds it gone,
// rather than writing refs into a directory about to be deleted.
func TestRemovePushCreatedRepositoryOrdersPushes(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := be.CreateRepository(ctx, "repo", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// A push is running: the repository stays.
	done, err := be.BeginPush(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := be.RemovePushCreatedRepository(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("not true: !removed")
	}
	done()

	// A push starts during the removal, after the last check for refs.
	pushErr := make(chan error, 1)
	be.beforePushCreatedDelete = func() {
		go func() {
			done, err := be.BeginPush(ctx, r)
			if err == nil {
				done()
			}
			pushErr <- err
		}()
		// The push waits for the removal to end before it starts.
		deadline := time.Now().Add(time.Minute)
		for !pushWaitsForGate() {
			select {
			case err := <-pushErr:
				t.Errorf("push started during the removal: %v", err)
				pushErr <- err
				return
			default:
			}
			if time.Now().After(deadline) {
				t.Error("the push neither started nor waited for the removal")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	removed, err = be.RemovePushCreatedRepository(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("not true: removed")
	}
	if !errors.Is(<-pushErr, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(<-pushErr, proto.ErrRepoNotFound)")
	} // the push finds it gone
}

// A change that lands after the removal's checks for refs, and before the
// repository's row is deleted, still keeps it: the checks run in the
// transaction that deletes the row. A repository renamed away, with
// another created under its name, in that gap is not the one deleted.
func TestRemovePushCreatedRepositoryChecksWhenDeleting(t *testing.T) {
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("collaborator", func(t *testing.T) {
		r, err := be.CreateRepository(ctx, "collab", owner, proto.RepositoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		be.beforePushCreatedDelete = func() {
			if err := be.AddCollaborator(ctx, "collab", "alice", access.ReadWriteAccess); err != nil {
				t.Fatal(err)
			}
		}

		removed, err := be.RemovePushCreatedRepository(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if removed {
			t.Fatal("not true: !removed")
		}
		_, err = be.Repository(ctx, "collab")
		if err != nil {
			t.Fatal(err)
		} // kept with its collaborator
	})

	t.Run("recreated", func(t *testing.T) {
		r, err := be.CreateRepository(ctx, "name", owner, proto.RepositoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var other proto.Repository
		be.beforePushCreatedDelete = func() {
			if err := be.RenameRepository(ctx, "name", "moved"); err != nil {
				t.Fatal(err)
			}
			other, err = be.CreateRepository(ctx, "name", owner, proto.RepositoryOptions{})
			if err != nil {
				t.Fatal(err)
			}
		}

		removed, err := be.RemovePushCreatedRepository(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if removed {
			t.Fatal("not true: !removed")
		}
		cur, err := be.Repository(ctx, "name")
		if err != nil {
			t.Fatal(err)
		} // the repository now under the name is kept
		if got, want := cur.ID(), other.ID(); got != want {
			t.Fatalf("cur.ID() = %v, want %v", got, want)
		}
		_, err = be.Repository(ctx, "moved")
		if err != nil {
			t.Fatal(err)
		}
	})
}

// A repository whose refs cannot be listed counts as having refs, so the
// cleanup never removes one it could not check.
func TestRepositoryHasRefsFailsSafe(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	r, err := be.CreateRepository(ctx, "empty", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if be.RepositoryHasRefs(ctx, r) {
		t.Fatal("not true: !be.RepositoryHasRefs(ctx, r)")
	}

	if err := os.RemoveAll(be.repoPath("empty")); err != nil {
		t.Fatal(err)
	}
	if !be.RepositoryHasRefs(ctx, r) {
		t.Fatal("not true: be.RepositoryHasRefs(ctx, r)")
	}
}

// The check for refs runs under the removal's context: once that is done,
// git is not asked, and the repository counts as having refs. So an empty
// push-created repository is kept, and no delete is tried, rather than
// checked without the context, after which a delete was tried and failed
// with ctx's error.
func TestRemovePushCreatedRepositoryCancelledKeeps(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	r, err := be.CreateRepository(ctx, "cancelled", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deleteTried := false
	be.beforePushCreatedDelete = func() { deleteTried = true }

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	removed, err := be.RemovePushCreatedRepository(cancelled, r)
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("not true: !removed")
	}
	if deleteTried {
		t.Fatal("not true: !deleteTried")
	}
	_, err = be.Repository(ctx, "cancelled")
	if err != nil {
		t.Fatal(err)
	} // the row is kept
	_, err = os.Stat(be.repoPath("cancelled"))
	if err != nil {
		t.Fatal(err)
	} // and so is the directory
}

// commitTo points ref of the repository at dir at a new empty commit.
func commitTo(t *testing.T, dir, ref string) {
	t.Helper()
	tree, err := exec.CommandContext(t.Context(), "git", "-C", dir, "mktree").Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "commit-tree", strings.TrimSpace(string(tree)), "-m", "x")
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
	commit, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.CommandContext(t.Context(), "git", "-C", dir, "update-ref", ref, strings.TrimSpace(string(commit))).Run(); err != nil {
		t.Fatal(err)
	}
}

// A push can fail after its refs landed: receive-pack writes them before the
// hooks run. The repository is then kept, with HEAD on a pushed branch, as
// after a push that succeeded, so it does not name a branch that does not
// exist.
func TestRemovePushCreatedRepositoryKeptWithRefsGetsHead(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	r, err := be.CreateRepository(ctx, "pushed", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dir := be.repoPath("pushed")
	commitTo(t, dir, "refs/heads/feature")

	removed, err := be.RemovePushCreatedRepository(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("not true: !removed")
	}
	head, err := exec.CommandContext(t.Context(), "git", "-C", dir, "symbolic-ref", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(head)); got != "refs/heads/feature" {
		t.Fatalf("strings.TrimSpace(string(head)) = %v, want %v", got, "refs/heads/feature")
	}
}

// A ref a lingering git process writes after the snapshot of the refs and
// before the check for refs keeps the repository: the check sees it. With
// the snapshot taken after the check, the ref was in the snapshot and not
// seen by the check, and the repository was removed with it.
func TestRemovePushCreatedRepositoryRefBetweenSnapshotAndCheck(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	r, err := be.CreateRepository(ctx, "late", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dir := be.repoPath("late")
	wrote := false
	be.afterRefSnapshot = func(rp string) {
		commitTo(t, rp, "refs/heads/main")
		wrote = true
	}

	removed, err := be.RemovePushCreatedRepository(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("not true: wrote")
	}
	if removed {
		t.Fatal("not true: !removed")
	}
	_, err = be.Repository(ctx, "late")
	if err != nil {
		t.Fatal(err)
	} // the row is kept
	_, err = os.Stat(filepath.Join(dir, "refs", "heads", "main"))
	if err != nil {
		t.Fatal(err)
	} // and so is the ref
}

// On Windows the failed push's git process can hold the directory open for
// a moment, so a removal of the directory that fails once the row is gone
// is tried again. Here a directory without write permission stands in for
// the open handle, and is made removable once the first attempt failed.
func TestRemovePushCreatedRepositoryRetriesDirectory(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not prevent removal on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can remove the directory regardless of its mode")
	}
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	// Retry for as long as the test needs, so the test decides when the
	// removal can succeed, not the host's speed.
	be.pushCreatedRetry = retryPolicy{first: 10 * time.Millisecond, total: time.Hour}
	ctx := backendTestContext(be, cfg)
	r, err := be.CreateRepository(ctx, "failed", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dir := be.repoPath("failed")
	objects := filepath.Join(dir, "objects")
	if err := os.Chmod(objects, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(objects, 0o755) }) //nolint:errcheck

	// Make the directory removable once the first removal failed. The
	// goroutine reports a timeout instead of failing the test itself.
	released := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(time.Minute)
		for !strings.Contains(logs.String(), "failed to delete repository directory") {
			if time.Now().After(deadline) {
				released <- errors.New("the first removal never failed")
				os.Chmod(objects, 0o755) //nolint:errcheck
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		released <- os.Chmod(objects, 0o755)
	}()

	removed, err := be.RemovePushCreatedRepository(ctx, r)
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("not true: removed")
	}
	_, err = os.Stat(dir)
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
}

// A directory that stays is given up on once the waits run out, and logged.
func TestRemovePushCreatedRepositoryGivesUpOnDirectory(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not prevent removal on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can remove the directory regardless of its mode")
	}
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	be.pushCreatedRetry = retryPolicy{first: time.Millisecond, total: 3 * time.Millisecond}
	ctx := backendTestContext(be, cfg)
	r, err := be.CreateRepository(ctx, "stuck", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	objects := filepath.Join(be.repoPath("stuck"), "objects")
	if err := os.Chmod(objects, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(objects, 0o755) }) //nolint:errcheck

	removed, err := be.RemovePushCreatedRepository(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("not true: removed")
	} // the row is gone
	if !strings.Contains(logs.String(), "failed to remove the directory of a repository created by a failed push") {
		t.Fatal("not true: strings.Contains(logs.String(), \"failed to remove the directory of a repository created by a failed push\")")
	}
	_, err = os.Stat(be.repoPath("stuck"))
	if err != nil {
		t.Fatal(err)
	}
}

// HEAD is fixed only on the repository the push created. One renamed or
// created under the name since is left as it is.
func TestRemovePushCreatedRepositoryLeavesHeadOfAnother(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	created, err := be.CreateRepository(ctx, "name", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.RenameRepository(ctx, "name", "moved"); err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "name", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dir := be.repoPath("name")
	if err := exec.CommandContext(t.Context(), "git", "-C", dir, "symbolic-ref", "HEAD", "refs/heads/missing").Run(); err != nil {
		t.Fatal(err)
	}
	commitTo(t, dir, "refs/heads/feature")

	removed, err := be.RemovePushCreatedRepository(ctx, created)
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("not true: !removed")
	}
	head, err := exec.CommandContext(t.Context(), "git", "-C", dir, "symbolic-ref", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(head)); got != "refs/heads/missing" {
		t.Fatalf("strings.TrimSpace(string(head)) = %v, want %v", got, "refs/heads/missing")
	}
}

// refOnFailureLog is a log sink that, when the delete logs that it could
// not remove the repository directory, writes a ref into that directory
// before the log call returns: what a lingering receive-pack can do right
// after the row is gone, placed before the first retry without depending on
// timing.
type refOnFailureLog struct {
	syncBuffer
	ref  string
	once sync.Once
	err  error
}

func (w *refOnFailureLog) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "failed to delete repository directory") {
		w.once.Do(func() {
			if w.err = os.MkdirAll(filepath.Dir(w.ref), 0o755); w.err == nil {
				w.err = os.WriteFile(w.ref, []byte(strings.Repeat("a", 40)+"\n"), 0o600)
			}
		})
	}
	return w.syncBuffer.Write(p)
}

// A ref written once the row is gone, as the failed push's lingering
// receive-pack can on Windows, stops the retry: the directory stays, to be
// reported at start-up, rather than being removed with what was pushed.
func TestRemovePushCreatedRepositoryRetryKeepsLateRefs(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not prevent removal on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can remove the directory regardless of its mode")
	}
	logs := &refOnFailureLog{}
	be, cfg := newTestBackendWithLog(t, logs)
	// A few short waits: without the check the retry gives up at once and
	// the test fails on the log below, with no timing involved.
	be.pushCreatedRetry = retryPolicy{first: time.Millisecond, total: 3 * time.Millisecond}
	ctx := backendTestContext(be, cfg)
	r, err := be.CreateRepository(ctx, "late", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dir := be.repoPath("late")
	logs.ref = filepath.Join(dir, "refs", "heads", "pushed")
	objects := filepath.Join(dir, "objects")
	if err := os.Chmod(objects, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(objects, 0o755) }) //nolint:errcheck

	removed, err := be.RemovePushCreatedRepository(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := logs.err; err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("not true: removed")
	} // the row is gone
	_, err = os.Stat(logs.ref)
	if err != nil {
		t.Fatal(err)
	} // the pushed ref is kept
	if !strings.Contains(logs.String(), "wrote refs to the repository after its row was deleted") {
		t.Fatal("not true: strings.Contains(logs.String(), \"wrote refs to the repository after its row was deleted\")")
	}
}

// chmodOnFailureLog is a log sink that makes dir writable again when the
// delete logs that it could not remove the repository directory, before the
// log call returns, so the retry that follows can succeed.
type chmodOnFailureLog struct {
	syncBuffer
	dir  string
	once sync.Once
}

func (w *chmodOnFailureLog) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "failed to delete repository directory") {
		w.once.Do(func() { os.Chmod(w.dir, 0o755) }) //nolint:errcheck
	}
	return w.syncBuffer.Write(p)
}

// A reftable repository always has a file at refs/heads and a table, since
// HEAD lives in the reftable, whether or not anything was pushed. That is
// not a ref written after the delete: the retry goes on and removes the
// directory.
func TestRemovePushCreatedRepositoryRetryReftable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not prevent removal on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can remove the directory regardless of its mode")
	}
	probe := filepath.Join(t.TempDir(), "probe.git")
	if err := exec.CommandContext(t.Context(), "git", "init", "--quiet", "--bare", "--ref-format=reftable", probe).Run(); err != nil {
		t.Skipf("git cannot create reftable repositories: %v", err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "init.defaultRefFormat")
	t.Setenv("GIT_CONFIG_VALUE_0", "reftable")

	logs := &chmodOnFailureLog{}
	be, cfg := newTestBackendWithLog(t, logs)
	be.pushCreatedRetry = retryPolicy{first: time.Millisecond, total: time.Hour}
	ctx := backendTestContext(be, cfg)
	r, err := be.CreateRepository(ctx, "tables", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dir := be.repoPath("tables")
	_, err = os.Stat(filepath.Join(dir, "reftable", "tables.list"))
	if err != nil {
		t.Fatal(err)
	} // the repository is a reftable one
	// The reftable files survive the first removal, as files held open
	// do on Windows.
	logs.dir = filepath.Join(dir, "reftable")
	if err := os.Chmod(logs.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(logs.dir, 0o755) }) //nolint:errcheck

	removed, err := be.RemovePushCreatedRepository(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("not true: removed")
	}
	_, err = os.Stat(dir)
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	} // removed by the retry
	if strings.Contains(logs.String(), "wrote refs") {
		t.Fatal("not true: !strings.Contains(logs.String(), \"wrote refs\")")
	}
}

// refsWritten counts what was added or changed since the snapshot, never
// what a removal took away.
func TestRefsWritten(t *testing.T) {
	t.Parallel()
	loose := func(kv ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	before := refState{loose: loose("refs/heads", "reftable stub"), tables: "0x1.ref\n"}
	for name, tc := range map[string]struct {
		now  refState
		want bool
	}{
		"unchanged":         {refState{loose: loose("refs/heads", "reftable stub"), tables: "0x1.ref\n"}, false},
		"half removed":      {refState{loose: loose()}, false},
		"new table":         {refState{loose: loose("refs/heads", "reftable stub"), tables: "0x1.ref\n0x2.ref\n"}, true},
		"new loose ref":     {refState{loose: loose("refs/heads", "reftable stub", "refs/heads/main", "abc")}, true},
		"changed loose ref": {refState{loose: loose("refs/heads", "other")}, true},
		"packed-refs":       {refState{loose: loose(), packed: "abc refs/heads/main\n"}, true},
		"unreadable":        {refState{loose: loose(), unreadable: true}, true},
	} {
		if got := refsWritten(before, tc.now); got != tc.want {
			t.Errorf("%s: refsWritten = %v, want %v", name, got, tc.want)
		}
	}
}

// A ref file, or a directory of refs, that cannot be read counts as a ref
// written: the retry must not remove what it could not check.
func TestReadRefStateUnreadable(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not prevent reading on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of their mode")
	}
	for name, path := range map[string]string{
		"file":      filepath.Join("refs", "heads", "main"),
		"directory": filepath.Join("refs", "heads", "team"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rp := t.TempDir()
			p := filepath.Join(rp, path)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if name == "file" {
				if err := os.WriteFile(p, []byte("abc\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(filepath.Join(p, "x"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(p, 0o755) }) //nolint:errcheck

			st := readRefState(rp)
			if !st.unreadable {
				t.Errorf("readRefState of a repository with an unreadable %s: unreadable = false", name)
			}
			if !refsWritten(refState{loose: map[string]string{}}, st) {
				t.Errorf("an unreadable %s does not count as a ref written", name)
			}
		})
	}
}

// Postgres runs transactions in READ COMMITTED, so a collaborator added
// after the keep check and before the row is deleted would commit and then
// go with the row through ON DELETE CASCADE, although its caller was told
// it was added. The keep check reads the row with FOR UPDATE, which the
// insert's foreign key check waits for: here the insert must block on the
// deleting transaction (checked with pg_blocking_pids) and then fail with
// proto.ErrRepoNotFound, since the repository it names is gone.
func TestDeleteRepositoryKeepCheckLocksRowPostgres(t *testing.T) {
	t.Parallel()
	e := newPostgresWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	err = e.be.deleteRepository(e.ctx, "repo", 0, func(tx *db.Tx, m models.Repo) (bool, error) {
		var holder int
		if err := tx.GetContext(e.ctx, &holder, `SELECT pg_backend_pid();`); err != nil {
			return true, err
		}
		go func() {
			result <- e.be.AddCollaborator(e.ctx, "repo", "alice", access.ReadWriteAccess)
		}()
		// An error, rather than a failed test, rolls the delete back and
		// lets AddCollaborator finish before the database is dropped.
		if err := waitBlockedBy(e, holder, result, "AddCollaborator"); err != nil {
			return true, err
		}
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-result:
		// The collaborator was not added to the deleted repository, and
		// the error says the repository is gone, not that the
		// collaborator exists.
		if !errors.Is(err, proto.ErrRepoNotFound) {
			t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
		}
	case <-time.After(time.Minute):
		t.Fatal("AddCollaborator still waiting after the delete committed")
	}
	_, err = e.be.Repository(e.ctx, "repo")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
}
