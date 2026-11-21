package backend

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// A repository whose refs cannot be listed counts as having refs, so the
// cleanup never removes one it could not check.
func TestRepositoryHasRefsFailsSafe(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	r, err := be.CreateRepository(ctx, "empty", defaultAdmin(t, be), proto.RepositoryOptions{})
	is.NoErr(err)
	is.True(!be.RepositoryHasRefs(r))

	is.NoErr(os.RemoveAll(be.repoPath("empty")))
	is.True(be.RepositoryHasRefs(r))
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
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	r, err := be.CreateRepository(ctx, "pushed", defaultAdmin(t, be), proto.RepositoryOptions{})
	is.NoErr(err)
	dir := be.repoPath("pushed")
	commitTo(t, dir, "refs/heads/feature")

	removed, err := be.RemovePushCreatedRepository(ctx, r)
	is.NoErr(err)
	is.True(!removed)
	head, err := exec.CommandContext(t.Context(), "git", "-C", dir, "symbolic-ref", "HEAD").Output()
	is.NoErr(err)
	is.Equal(strings.TrimSpace(string(head)), "refs/heads/feature")
}

// A ref a lingering git process writes after the snapshot of the refs and
// before the check for refs keeps the repository: the check sees it. With
// the snapshot taken after the check, the ref was in the snapshot and not
// seen by the check, and the repository was removed with it.
func TestRemovePushCreatedRepositoryRefBetweenSnapshotAndCheck(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	r, err := be.CreateRepository(ctx, "late", defaultAdmin(t, be), proto.RepositoryOptions{})
	is.NoErr(err)
	dir := be.repoPath("late")
	wrote := false
	be.afterRefSnapshot = func(rp string) {
		commitTo(t, rp, "refs/heads/main")
		wrote = true
	}

	removed, err := be.RemovePushCreatedRepository(ctx, r)
	is.NoErr(err)
	is.True(wrote)
	is.True(!removed)
	_, err = be.Repository(ctx, "late")
	is.NoErr(err) // the row is kept
	_, err = os.Stat(filepath.Join(dir, "refs", "heads", "main"))
	is.NoErr(err) // and so is the ref
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
	is := is.New(t)
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	// Retry for as long as the test needs, so the test decides when the
	// removal can succeed, not the host's speed.
	be.pushCreatedRetry = retryPolicy{first: 10 * time.Millisecond, total: time.Hour}
	ctx := backendTestContext(be, cfg)
	r, err := be.CreateRepository(ctx, "failed", defaultAdmin(t, be), proto.RepositoryOptions{})
	is.NoErr(err)
	dir := be.repoPath("failed")
	objects := filepath.Join(dir, "objects")
	is.NoErr(os.Chmod(objects, 0o555))
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
	is.NoErr(<-released)
	is.NoErr(err)
	is.True(removed)
	_, err = os.Stat(dir)
	is.True(os.IsNotExist(err))
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
	is := is.New(t)
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	be.pushCreatedRetry = retryPolicy{first: time.Millisecond, total: 3 * time.Millisecond}
	ctx := backendTestContext(be, cfg)
	r, err := be.CreateRepository(ctx, "stuck", defaultAdmin(t, be), proto.RepositoryOptions{})
	is.NoErr(err)
	objects := filepath.Join(be.repoPath("stuck"), "objects")
	is.NoErr(os.Chmod(objects, 0o555))
	t.Cleanup(func() { os.Chmod(objects, 0o755) }) //nolint:errcheck

	removed, err := be.RemovePushCreatedRepository(ctx, r)
	is.NoErr(err)
	is.True(removed) // the row is gone
	is.True(strings.Contains(logs.String(), "failed to remove the directory of a repository created by a failed push"))
	_, err = os.Stat(be.repoPath("stuck"))
	is.NoErr(err)
}

// HEAD is fixed only on the repository the push created. One renamed or
// created under the name since is left as it is. HEAD used to be pointed
// at a branch of whatever had the name.
func TestRemovePushCreatedRepositoryLeavesHeadOfAnother(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	created, err := be.CreateRepository(ctx, "name", defaultAdmin(t, be), proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(be.RenameRepository(ctx, "name", "moved"))
	_, err = be.CreateRepository(ctx, "name", defaultAdmin(t, be), proto.RepositoryOptions{})
	is.NoErr(err)
	dir := be.repoPath("name")
	is.NoErr(exec.CommandContext(t.Context(), "git", "-C", dir, "symbolic-ref", "HEAD", "refs/heads/missing").Run())
	commitTo(t, dir, "refs/heads/feature")

	removed, err := be.RemovePushCreatedRepository(ctx, created)
	is.NoErr(err)
	is.True(!removed)
	head, err := exec.CommandContext(t.Context(), "git", "-C", dir, "symbolic-ref", "HEAD").Output()
	is.NoErr(err)
	is.Equal(strings.TrimSpace(string(head)), "refs/heads/missing")
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
	is := is.New(t)
	logs := &refOnFailureLog{}
	be, cfg := newTestBackendWithLog(t, logs)
	// A few short waits: without the check the retry gives up at once and
	// the test fails on the log below, with no timing involved.
	be.pushCreatedRetry = retryPolicy{first: time.Millisecond, total: 3 * time.Millisecond}
	ctx := backendTestContext(be, cfg)
	r, err := be.CreateRepository(ctx, "late", defaultAdmin(t, be), proto.RepositoryOptions{})
	is.NoErr(err)
	dir := be.repoPath("late")
	logs.ref = filepath.Join(dir, "refs", "heads", "pushed")
	objects := filepath.Join(dir, "objects")
	is.NoErr(os.Chmod(objects, 0o555))
	t.Cleanup(func() { os.Chmod(objects, 0o755) }) //nolint:errcheck

	removed, err := be.RemovePushCreatedRepository(ctx, r)
	is.NoErr(err)
	is.NoErr(logs.err)
	is.True(removed) // the row is gone
	_, err = os.Stat(logs.ref)
	is.NoErr(err) // the pushed ref is kept
	is.True(strings.Contains(logs.String(), "wrote refs to the repository after its row was deleted"))
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
// directory. It used to stop at once and log that a push wrote refs.
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

	is := is.New(t)
	logs := &chmodOnFailureLog{}
	be, cfg := newTestBackendWithLog(t, logs)
	be.pushCreatedRetry = retryPolicy{first: time.Millisecond, total: time.Hour}
	ctx := backendTestContext(be, cfg)
	r, err := be.CreateRepository(ctx, "tables", defaultAdmin(t, be), proto.RepositoryOptions{})
	is.NoErr(err)
	dir := be.repoPath("tables")
	_, err = os.Stat(filepath.Join(dir, "reftable", "tables.list"))
	is.NoErr(err) // the repository is a reftable one
	// The reftable files survive the first removal, as files held open
	// do on Windows.
	logs.dir = filepath.Join(dir, "reftable")
	is.NoErr(os.Chmod(logs.dir, 0o555))
	t.Cleanup(func() { os.Chmod(logs.dir, 0o755) }) //nolint:errcheck

	removed, err := be.RemovePushCreatedRepository(ctx, r)
	is.NoErr(err)
	is.True(removed)
	_, err = os.Stat(dir)
	is.True(os.IsNotExist(err)) // removed by the retry
	is.True(!strings.Contains(logs.String(), "wrote refs"))
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
