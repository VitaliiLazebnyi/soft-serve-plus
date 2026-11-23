package git

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"testing"
)

// runDatedGit runs git in dir with a fixed identity and dates.
func runDatedGit(t *testing.T, dir, date string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{
		"-c", "user.email=test@example.com", "-c", "user.name=Test",
	}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+date, "GIT_AUTHOR_DATE="+date)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// Tags lists the tags the most recently created first, under the
// repository's context, and none for a repository without any.
func TestTags(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	if tags, err := repo.Tags(); err != nil || len(tags) != 0 {
		t.Fatalf("Tags without tags = %q, %v; want none", tags, err)
	}
	runDatedGit(t, repo.Path, "2020-01-02T00:00:00Z", "tag", "-a", "-m", "old", "m-old")
	runDatedGit(t, repo.Path, "2021-01-02T00:00:00Z", "tag", "-a", "-m", "new", "z-new")
	runDatedGit(t, repo.Path, "2019-01-02T00:00:00Z", "tag", "-a", "-m", "oldest", "a-oldest")
	tags, err := repo.Tags()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"z-new", "m-old", "a-oldest"}; !slices.Equal(tags, want) {
		t.Fatalf("Tags = %q; want %q", tags, want)
	}

	ctx, cancel := context.WithCancel(t.Context())
	r, err := Open(ctx, repo.Path)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := r.Tags(); err == nil {
		t.Fatal("Tags under a cancelled context succeeded")
	}
}

// A deletion and a config write finish once started, whatever happens to
// the repository's context: here it is done before they start.
func TestWritesFinishAfterCancel(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	runDatedGit(t, repo.Path, "2020-01-02T00:00:00Z", "branch", "doomed")
	runDatedGit(t, repo.Path, "2020-01-02T00:00:00Z", "tag", "gone")
	ctx, cancel := context.WithCancel(t.Context())
	r, err := Open(ctx, repo.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ConfigSet("test.key", "before"); err != nil {
		t.Fatal(err)
	}
	cancel()

	if _, err := r.Branches(); err == nil {
		t.Fatal("a read under the cancelled context succeeded")
	}
	if err := r.DeleteBranch("doomed"); err != nil {
		t.Errorf("DeleteBranch after cancel: %v", err)
	}
	if err := r.DeleteTag("gone"); err != nil {
		t.Errorf("DeleteTag after cancel: %v", err)
	}
	if err := r.ConfigSet("test.other", "after"); err != nil {
		t.Errorf("ConfigSet after cancel: %v", err)
	}
	if err := r.ConfigUnset("test.key"); err != nil {
		t.Errorf("ConfigUnset after cancel: %v", err)
	}

	live, err := Open(t.Context(), repo.Path)
	if err != nil {
		t.Fatal(err)
	}
	if branches, err := live.Branches(); err != nil || slices.Contains(branches, "doomed") {
		t.Errorf("branches = %q, %v; want doomed deleted", branches, err)
	}
	if tags, err := live.Tags(); err != nil || slices.Contains(tags, "gone") {
		t.Errorf("tags = %q, %v; want gone deleted", tags, err)
	}
	if v, ok, err := live.ConfigGet("test.other"); err != nil || !ok || v != "after" {
		t.Errorf("test.other = %q, %v, %v; want after", v, ok, err)
	}
	if _, ok, err := live.ConfigGet("test.key"); err != nil || ok {
		t.Errorf("test.key still set (%v, %v)", ok, err)
	}
}

// The reads that replace git-module's promoted methods run under the
// repository's context.
func TestRefReadsUnderContext(t *testing.T) {
	t.Parallel()
	repo, head := setupTestRepo(t)
	if id, err := repo.ShowRefVerify(head.Name().String()); err != nil || id != head.ID() {
		t.Errorf("ShowRefVerify = %q, %v; want %s", id, err, head.ID())
	}
	if n, err := repo.RevListCount(0, head.ID()); err != nil || n != 1 {
		t.Errorf("RevListCount = %d, %v; want 1", n, err)
	}
	if _, err := repo.LatestCommitTime(); err != nil {
		t.Errorf("LatestCommitTime: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	r, err := Open(ctx, repo.Path)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	for name, fn := range map[string]func() error{
		"ShowRefVerify":    func() error { _, err := r.ShowRefVerify(head.Name().String()); return err },
		"RevListCount":     func() error { _, err := r.RevListCount(1, head.ID()); return err },
		"LatestCommitTime": func() error { _, err := r.LatestCommitTime(); return err },
		"StashList":        func() error { _, err := r.StashList(); return err },
		"ConfigGet":        func() error { _, _, err := r.ConfigGet("core.bare"); return err },
	} {
		if err := fn(); err == nil {
			t.Errorf("%s under a cancelled context succeeded", name)
		}
	}
}
