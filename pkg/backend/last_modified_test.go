package backend

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// longAgo is the date of the commits these tests make, far from the time
// any row is written.
var longAgo = time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

// commitLongAgo points refs/heads/main of the repository at dir at a new
// empty commit dated longAgo, without running any hook.
func commitLongAgo(t *testing.T, dir string) {
	t.Helper()
	tree, err := exec.CommandContext(t.Context(), "git", "-C", dir, "mktree").Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "commit-tree", strings.TrimSpace(string(tree)), "-m", "old")
	date := longAgo.Format(time.RFC3339)
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b", "GIT_AUTHOR_DATE="+date,
		"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b", "GIT_COMMITTER_DATE="+date)
	commit, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.CommandContext(t.Context(), "git", "-C", dir, "update-ref", "refs/heads/main", strings.TrimSpace(string(commit))).Run(); err != nil {
		t.Fatal(err)
	}
}

// UpdatedAt runs no git: without info/last-modified it is the time the row
// last changed, not the latest commit's, so a TUI listing runs no git for
// a repository nothing has been pushed to.
func TestUpdatedAtFallsBackToRow(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	_, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	commitLongAgo(t, be.repoPath("repo"))

	r, err := be.Repository(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if r.UpdatedAt().IsZero() {
		t.Fatal("not true: !r.UpdatedAt().IsZero()")
	}
	if !r.UpdatedAt().After(longAgo.Add(24 * time.Hour)) {
		t.Fatal("not true: r.UpdatedAt().After(longAgo.Add(24 * time.Hour))")
	} // the row's time, not the commit's
}

// An import and a mirror sync run no hooks, so they record the time of the
// latest commit themselves.
func TestImportAndMirrorSyncRecordLastModified(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))

	src := t.TempDir()
	if out, err := exec.CommandContext(context.Background(), "git", "init", "--bare", "--quiet", src).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	commitLongAgo(t, src)

	imp := importSpec{name: "imported", path: be.repoPath("imported"), remote: src, logRemote: src}
	if err := claimRepoPath(imp.path); err != nil {
		t.Fatal(err)
	}
	r, err := be.importRepository(ctx, defaultAdmin(t, be), imp)
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(imp.path + "/info/last-modified")
	if err != nil {
		t.Fatal(err)
	}
	if !r.UpdatedAt().Equal(longAgo) {
		t.Fatal("not true: r.UpdatedAt().Equal(longAgo)")
	}

	// A sync that fetched a newer history records it.
	if err := os.Remove(imp.path + "/info/last-modified"); err != nil {
		t.Fatal(err)
	}
	if err := be.RecordMirrorSync(ctx, r, nil); err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(imp.path + "/info/last-modified")
	if err != nil {
		t.Fatal(err)
	}
	r, err = be.Repository(ctx, "imported")
	if err != nil {
		t.Fatal(err)
	}
	if !r.UpdatedAt().Equal(longAgo) {
		t.Fatal("not true: r.UpdatedAt().Equal(longAgo)")
	}
}

// An empty import, or a sync of an empty mirror, has no commit to record:
// it is not a failure to warn about.
func TestEmptyImportRecordsNoLastModified(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))

	src := t.TempDir()
	if out, err := exec.CommandContext(context.Background(), "git", "init", "--bare", "--quiet", src).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	imp := importSpec{name: "empty", path: be.repoPath("empty"), remote: src, logRemote: src}
	if err := claimRepoPath(imp.path); err != nil {
		t.Fatal(err)
	}
	r, err := be.importRepository(ctx, defaultAdmin(t, be), imp)
	if err != nil {
		t.Fatal(err)
	}
	if err := be.RecordMirrorSync(ctx, r, nil); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(logs.String(), "failed to record the last change") {
		t.Fatal("not true: !strings.Contains(logs.String(), \"failed to record the last change\")")
	}
	_, err = os.Stat(imp.path + "/info/last-modified")
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
}

// info/last-modified is a data file: it is written readable and writable as
// the umask allows, like the files git writes, and never executable.
func TestLastModifiedIsNotExecutable(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no execute bits")
	}
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	_, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	commitLongAgo(t, be.repoPath("repo"))

	if err := be.populateLastModified(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(be.repoPath("repo"), "info", "last-modified"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode&^0o666 != 0 {
		t.Fatalf("info/last-modified has mode %v, want no bits beyond 0666", mode)
	}
}
