package backend

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// orphansOf returns the repository directories of be with no row, relative
// to its repos directory and with forward slashes.
func orphansOf(t *testing.T, be *Backend) []string {
	t.Helper()
	dirs, err := be.OrphanRepositoryDirs(context.Background())
	if err != nil {
		t.Fatalf("OrphanRepositoryDirs => %v", err)
	}
	root := filepath.Join(be.cfg.DataPath, "repos")
	rel := make([]string, 0, len(dirs))
	for _, d := range dirs {
		r, err := filepath.Rel(root, d)
		if err != nil {
			t.Fatal(err)
		}
		rel = append(rel, filepath.ToSlash(r))
	}
	return rel
}

// mkdirs creates each directory under be's repos directory.
func mkdirs(t *testing.T, be *Backend, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(be.cfg.DataPath, "repos", filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// Directories that look like repositories and have no row are reported,
// nested ones included; the repositories with rows, their groups, and
// directories that do not look like repositories are not.
func TestOrphanRepositoryDirs(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	for _, name := range []string{"kept", "org/team/kept"} {
		if _, err := be.CreateRepository(ctx, name, defaultAdmin(t, be), proto.RepositoryOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	mkdirs(t, be,
		"orphan.git",
		"org/orphan.git",
		"org/team/orphan.git/inner.git", // inside an orphan: not looked at
		"empty-group/deeper/orphan.git",
		"not-a-repo",
	)
	if err := os.WriteFile(filepath.Join(cfg.DataPath, "repos", "file.git"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	got := orphansOf(t, be)
	want := []string{"empty-group/deeper/orphan.git", "org/orphan.git", "org/team/orphan.git", "orphan.git"}
	if !slices.Equal(got, want) {
		t.Errorf("OrphanRepositoryDirs => %q, want %q", got, want)
	}
}

// With no repos directory there is nothing to report.
func TestOrphanRepositoryDirsNoReposDir(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	if err := os.RemoveAll(filepath.Join(be.cfg.DataPath, "repos")); err != nil {
		t.Fatal(err)
	}
	if got := orphansOf(t, be); len(got) != 0 {
		t.Errorf("OrphanRepositoryDirs => %q, want none", got)
	}
}

// A link to a repository's directory is that repository, not an orphan; a
// link to a directory with no row is one.
func TestOrphanRepositoryDirsSymlinks(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	if _, err := be.CreateRepository(ctx, "real", defaultAdmin(t, be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.git")
	if err := os.Mkdir(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(cfg.DataPath, "repos")
	if err := os.Symlink(be.repoPath("real"), filepath.Join(root, "alias.git")); err != nil {
		t.Skipf("cannot create symbolic links: %v", err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(root, "linked.git")); err != nil {
		t.Fatal(err)
	}

	got := orphansOf(t, be)
	if want := []string{"linked.git"}; !slices.Equal(got, want) {
		t.Errorf("OrphanRepositoryDirs => %q, want %q", got, want)
	}
}

// On a case-insensitive filesystem a repository can be named "Secret" while
// its directory is spelled secret.git. That directory is the repository's,
// not an orphan.
func TestOrphanRepositoryDirsCaseVariant(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	if !createCaseVariantFixture(t, be) {
		t.Skip("filesystem is case-sensitive: Secret has its own directory")
	}
	ctx := backendTestContext(be, cfg)
	if err := be.store.SetRepoNameByName(ctx, be.db, "secret", "Secret"); err != nil {
		t.Fatal(err)
	}
	if got := orphansOf(t, be); len(got) != 0 {
		t.Errorf("OrphanRepositoryDirs => %q, want none", got)
	}
}

// reportOrphans names each orphan, up to maxReportedOrphans, and
// counts the rest.
func TestReportOrphanRepositories(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	be, _ := newTestBackendWithLog(t, &logs)
	for i := range maxReportedOrphans + 2 {
		mkdirs(t, be, "orphan"+strings.Repeat("x", i)+".git")
	}

	orphans, err := be.OrphanRepositoryDirs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	be.reportOrphans(orphans)

	out := logs.String()
	if n := strings.Count(out, "repository directory has no database row"); n != maxReportedOrphans {
		t.Errorf("%d orphans named, want %d:\n%s", n, maxReportedOrphans, out)
	}
	if !strings.Contains(out, "more repository directories have no database row") || !strings.Contains(out, "more=2") {
		t.Errorf("the rest are not counted:\n%s", out)
	}
	if !strings.Contains(out, filepath.Join(be.cfg.DataPath, "repos", "orphan.git")) {
		t.Errorf("orphan path not logged:\n%s", out)
	}
}

// APFS ignores Unicode normalization, so a repository whose row names the
// decomposed form of its name while its directory is spelled precomposed
// is not an orphan.
func TestOrphanRepositoryDirsNormalizationVariant(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	const (
		precomposed = "\uD55C"             // 한
		decomposed  = "\u1112\u1161\u11AB" // 한 as jamo
	)
	if _, err := be.CreateRepository(ctx, precomposed, defaultAdmin(t, be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	if !be.repoPathExists(decomposed) {
		t.Skip("filesystem keeps Unicode normalization forms apart")
	}
	if err := be.store.SetRepoNameByName(ctx, be.db, precomposed, decomposed); err != nil {
		t.Fatal(err)
	}
	if got := orphansOf(t, be); len(got) != 0 {
		t.Errorf("OrphanRepositoryDirs => %q, want none", got)
	}
}

// A data directory whose repositories and groups are all spelled as their
// rows name them is matched by path alone: the scan makes no identity
// comparison, so its cost does not grow with groups times repositories.
func TestOrphanRepositoryDirsMatchesByPathFirst(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	for _, name := range []string{"a/one", "a/two", "b/c/three", "four"} {
		if _, err := be.CreateRepository(ctx, name, defaultAdmin(t, be), proto.RepositoryOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	scan, err := be.scanRepoDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.orphans) != 0 || scan.compares != 0 {
		t.Errorf("scanRepoDirs => %q with %d identity comparisons, want none and 0", scan.orphans, scan.compares)
	}
}
