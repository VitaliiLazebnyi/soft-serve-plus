package jobs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
)

// fakeSSH is an ssh client that runs the remote command locally, so a test
// can mirror over ssh:// without a server. git calls it as
// "ssh [options] host command" and the command comes last.
const fakeSSH = `#!/bin/sh
for last; do :; done
exec sh -c "$last"
`

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// refs lists a repository's refs as "name oid" lines.
func refs(t *testing.T, dir string) string {
	t.Helper()
	return runGit(t, dir, "for-each-ref", "--format=%(refname) %(objectname)")
}

// newMirrorJobContext returns a context carrying everything the mirror job
// reads: config, database, store and backend.
func newMirrorJobContext(t *testing.T) (context.Context, *backend.Backend) {
	t.Helper()
	ctx := context.Background()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	ctx = config.WithContext(ctx, cfg)

	// The server's own SQLite settings: the job's workers look up mirror
	// owners concurrently and rely on busy_timeout to take turns.
	cfg.DB.Driver = "sqlite"
	cfg.DB.DataSource = filepath.Join(cfg.DataPath, "soft-serve.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbx.Close() })
	if err := migrate.Migrate(ctx, dbx); err != nil {
		t.Fatal(err)
	}
	st := database.New(ctx, dbx)
	be := backend.New(ctx, cfg, dbx, st)
	ctx = db.WithContext(ctx, dbx)
	ctx = store.WithContext(ctx, st)
	ctx = backend.WithContext(ctx, be)
	return ctx, be
}

// The mirror job fetches every mirror from its remote, prunes what the remote
// deleted, and leaves alone mirrors whose remote fails validation and
// repositories that are not mirrors.
func TestMirrorPullFunc(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake ssh client is a shell script")
	}

	// git resolves the ssh client the job configures through PATH.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(fakeSSH), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, be := newMirrorJobContext(t)
	if reason := be.ImportsDisabledReason(); reason != "" {
		t.Skipf("mirror syncs are disabled here: %s", reason)
	}
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}

	// The upstream: two branches.
	work := t.TempDir()
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	runGit(t, work, "init", "-q", "-b", "main", ".")
	runGit(t, work, "commit", "-q", "--allow-empty", "-m", "first")
	runGit(t, work, "branch", "feature")
	runGit(t, work, "clone", "-q", "--bare", work, upstream)
	runGit(t, work, "remote", "add", "origin", upstream)

	// A mirror of it over ssh (allowed: its owner is an admin), a mirror
	// of it over file:// (refused by validation), and a plain repository
	// with the ssh remote (not a mirror).
	sshRemote := "ssh://upstream.example" + filepath.ToSlash(upstream)
	addRepo := func(name string, mirror bool, remote string) string {
		t.Helper()
		if _, err := be.CreateRepository(ctx, name, admin, proto.RepositoryOptions{Mirror: mirror}); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(config.FromContext(ctx).DataPath, "repos", name+".git")
		runGit(t, dir, "config", "remote.origin.url", remote)
		runGit(t, dir, "config", "remote.origin.fetch", "+refs/*:refs/*")
		runGit(t, dir, "config", "remote.origin.mirror", "true")
		return dir
	}
	mirrorDir := addRepo("mirror", true, sshRemote)
	refusedDir := addRepo("refused", true, "file://"+filepath.ToSlash(upstream))
	plainDir := addRepo("plain", false, sshRemote)

	job := mirrorPull{}.Func(ctx)
	job()

	if got, want := refs(t, mirrorDir), refs(t, upstream); got != want {
		t.Errorf("mirror refs after sync:\n%s\nwant:\n%s", got, want)
	}
	if got := refs(t, refusedDir); got != "" {
		t.Errorf("mirror with a refused remote was fetched; refs:\n%s", got)
	}
	if got := refs(t, plainDir); got != "" {
		t.Errorf("repository that is not a mirror was fetched; refs:\n%s", got)
	}

	// New commits arrive, deleted branches go.
	runGit(t, work, "commit", "-q", "--allow-empty", "-m", "second")
	runGit(t, work, "push", "-q", "origin", "main", ":feature")
	job()

	got, want := refs(t, mirrorDir), refs(t, upstream)
	if got != want {
		t.Errorf("mirror refs after the second sync:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "refs/heads/feature") {
		t.Errorf("branch deleted upstream is still in the mirror:\n%s", got)
	}
}
